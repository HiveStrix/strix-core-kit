// Package idempotency makes a write with the same Idempotency-Key happen once
// (proposal §5.5). The key travels as gRPC metadata `idempotency-key`; the
// interceptor records it in the Core's own tenant database, in the table
// Migration creates, and answers a retry with the response of the first
// attempt instead of performing the write again.
//
// Why it is the platform's and not each Core's: a double submit without a key
// recurred in billing, inventory, maintenance, costing and expenses, and the
// mobile offline queue retries by design. Machines retry most of all, so for
// a service or an agent a write without a key is refused outright.
//
// What is a write comes from the operation catalog (capabilities), the same
// classification the gate uses for the assistant: anything that is not
// `effect: read` is a write.
package idempotency

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/hs-javierviquez/strix-core-kit/auth"
	"github.com/hs-javierviquez/strix-core-kit/capabilities"
)

// Migration creates the key table, goose-compatible. Each Core copies it
// into its next migration file (the table lives in the Core's schema of each
// tenant database, like the outbox).
const Migration = `-- +goose Up
CREATE TABLE IF NOT EXISTS idempotency_keys (
    tenant_id    text        NOT NULL,
    key          text        NOT NULL,
    method       text        NOT NULL,
    request_hash bytea       NOT NULL,
    status       text        NOT NULL,
    response     bytea,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, key)
);
CREATE INDEX IF NOT EXISTS idempotency_keys_created_at_idx ON idempotency_keys (created_at);

-- +goose Down
DROP TABLE IF EXISTS idempotency_keys;
`

// MetadataKey is the gRPC metadata the key travels in (the Shell's BFF maps
// the HTTP Idempotency-Key header onto it).
const MetadataKey = "idempotency-key"

const (
	// DefaultTTL is how long a key is remembered (proposal §5.5). A retry
	// after that is a new request.
	DefaultTTL = 72 * time.Hour
	// DefaultStaleAfter is how long a key may stay in progress before a
	// retry takes it over. It covers the process that died between claiming
	// the key and recording the response; a handler that legitimately runs
	// longer than this would be executed twice, so it is far above any RPC
	// deadline the platform uses.
	DefaultStaleAfter = 10 * time.Minute
	// MaxKeyLength bounds the key. A UUID is 36.
	MaxKeyLength = 255
)

const (
	statusInProgress = "in_progress"
	statusCompleted  = "completed"
)

// PoolResolver returns a tenant's pool. tenancy.Base satisfies it; SinglePool
// adapts a Core with one database.
type PoolResolver interface {
	PoolFor(ctx context.Context, tenantID string) (*pgxpool.Pool, error)
}

type singlePool struct{ p *pgxpool.Pool }

func (s singlePool) PoolFor(context.Context, string) (*pgxpool.Pool, error) { return s.p, nil }

// SinglePool is a PoolResolver that answers the same pool for every tenant.
func SinglePool(p *pgxpool.Pool) PoolResolver { return singlePool{p} }

// Interceptor enforces and applies idempotency keys.
type Interceptor struct {
	pools      PoolResolver
	methods    map[string]capabilities.Effect
	ttl        time.Duration
	staleAfter time.Duration
}

// Option configures an Interceptor.
type Option func(*Interceptor)

// TTL overrides DefaultTTL.
func TTL(d time.Duration) Option { return func(i *Interceptor) { i.ttl = d } }

// StaleAfter overrides DefaultStaleAfter.
func StaleAfter(d time.Duration) Option { return func(i *Interceptor) { i.staleAfter = d } }

// New builds the interceptor over the Core's tenant pools and its catalog's
// method -> effect map (capabilities.Catalog.MethodEffects()).
//
// With a map, a method it does not list is a write: an uncatalogued RPC is a
// gap, and the safe reading of a gap is "may write" (capcheck keeps the gap
// from existing). With a nil map, a method whose name starts with Get, List
// or Read is a read and everything else a write, the method-name form of the
// gate's read|list|get rule. Install it with the catalog: without one, a
// machine calling a read named otherwise (LookupSuppliers) is refused for
// lack of a key.
func New(pools PoolResolver, methods map[string]capabilities.Effect, opts ...Option) *Interceptor {
	i := &Interceptor{pools: pools, ttl: DefaultTTL, staleAfter: DefaultStaleAfter}
	if methods != nil {
		i.methods = make(map[string]capabilities.Effect, len(methods))
		for k, v := range methods {
			i.methods[k] = v
		}
	}
	for _, o := range opts {
		o(i)
	}
	return i
}

// IsWrite reports how the interceptor classifies a gRPC full method.
func (i *Interceptor) IsWrite(fullMethod string) bool {
	if i.methods != nil {
		e, ok := i.methods[fullMethod]
		return !ok || e != capabilities.EffectRead
	}
	name := fullMethod[strings.LastIndex(fullMethod, "/")+1:]
	for _, p := range []string{"Get", "List", "Read"} {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// Unary is the interceptor. It goes AFTER auth's (it reads the verified
// tenant and principal) and before the handler. For a write:
//
//   - no key: a machine (service or agent, or any principal type that is not
//     a person) gets InvalidArgument; a person goes through without
//     idempotency (the UI sends a key, but an old client must keep working);
//   - a key never seen, or seen more than TTL ago: the handler runs once and
//     its response is recorded. If the handler fails, the key is released so
//     the retry runs again: errors are not replayed;
//   - the same key with the same request: the recorded response, the
//     handler does not run;
//   - the same key with another request (other body, other method, or
//     another principal): AlreadyExists;
//   - the same key still running: Aborted, retry later. Aborted is gRPC's
//     code for a concurrency conflict the client resolves by retrying, and
//     the retry gets the recorded response once the first attempt finishes.
//     A key in progress for more than StaleAfter is taken over;
//   - the key store unreachable: Unavailable, and the handler does NOT run:
//     a write that cannot be made idempotent is not made.
//
// The key is recorded in its own statements, not in the handler's
// transaction: if the process dies after the handler committed and before
// the response is recorded, a retry after StaleAfter runs the handler again.
// That window is the price of an interceptor that needs no change to any
// handler; a Core that cannot afford it writes the key inside its own
// transaction.
func (i *Interceptor) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !i.IsWrite(info.FullMethod) {
			return handler(ctx, req)
		}
		claims, ok := auth.ClaimsFrom(ctx)
		if !ok {
			// No verified identity: the health check, or an RPC outside the
			// auth chain, which the gate refuses anyway.
			return handler(ctx, req)
		}
		key, err := keyFrom(ctx)
		if err != nil {
			return nil, err
		}
		if key == "" {
			if claims.PrincipalType() != auth.PrincipalUser {
				return nil, status.Errorf(codes.InvalidArgument,
					"idempotency: a %s must send %q metadata on every write", claims.PrincipalType(), MetadataKey)
			}
			return handler(ctx, req)
		}
		msg, ok := req.(proto.Message)
		if !ok {
			return nil, status.Error(codes.Internal, "idempotency: request is not a proto message")
		}
		hash, err := requestHash(info.FullMethod, claims, msg)
		if err != nil {
			return nil, status.Error(codes.Internal, "idempotency: hash request")
		}
		return i.run(ctx, claims.TenantID, key, info.FullMethod, hash, req, handler)
	}
}

func keyFrom(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(MetadataKey)
	if len(vals) == 0 {
		return "", nil
	}
	if len(vals) > 1 {
		return "", status.Errorf(codes.InvalidArgument, "idempotency: %q sent more than once", MetadataKey)
	}
	k := vals[0]
	if k == "" || len(k) > MaxKeyLength {
		return "", status.Errorf(codes.InvalidArgument, "idempotency: %q must be 1 to %d characters", MetadataKey, MaxKeyLength)
	}
	for _, r := range k {
		if r < 0x21 || r > 0x7e {
			return "", status.Errorf(codes.InvalidArgument, "idempotency: %q must be printable ASCII without spaces", MetadataKey)
		}
	}
	return k, nil
}

// requestHash binds a key to what it was first used for: the method, who
// called (a key another principal of the tenant reuses must not replay the
// first one's response to it) and the request, marshalled deterministically.
func requestHash(method string, c *auth.Claims, req proto.Message) ([]byte, error) {
	body, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	for _, part := range []string{method, c.PrincipalType(), c.Subject} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write(body)
	return h.Sum(nil), nil
}

func (i *Interceptor) run(ctx context.Context, tenantID, key, method string, hash []byte, req any, handler grpc.UnaryHandler) (any, error) {
	pool, err := i.pools.PoolFor(ctx, tenantID)
	if err != nil {
		slog.ErrorContext(ctx, "idempotency: no pool for tenant", "tenant", tenantID, "error", err)
		return nil, status.Error(codes.Unavailable, "idempotency: key store unavailable")
	}
	owned, prev, err := i.claim(ctx, pool, tenantID, key, method, hash)
	if err != nil {
		slog.ErrorContext(ctx, "idempotency: claim failed", "tenant", tenantID, "method", method, "error", err)
		return nil, status.Error(codes.Unavailable, "idempotency: key store unavailable")
	}
	if !owned {
		return replay(prev, hash)
	}

	resp, herr := handler(ctx, req)
	// Recording runs on a context that outlives a cancelled request: the
	// write already happened, and losing its record is what makes a retry
	// run it twice.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if herr != nil {
		if _, err := pool.Exec(rctx, `DELETE FROM idempotency_keys WHERE tenant_id = $1 AND key = $2 AND status = $3`,
			tenantID, key, statusInProgress); err != nil {
			slog.ErrorContext(ctx, "idempotency: release failed; the key stays in progress until it goes stale",
				"tenant", tenantID, "method", method, "error", err)
		}
		return nil, herr
	}
	stored, err := marshalResponse(resp)
	if err != nil {
		slog.ErrorContext(ctx, "idempotency: response not recordable", "method", method, "error", err)
		return resp, nil
	}
	if _, err := pool.Exec(rctx, `UPDATE idempotency_keys SET status = $3, response = $4 WHERE tenant_id = $1 AND key = $2`,
		tenantID, key, statusCompleted, stored); err != nil {
		slog.ErrorContext(ctx, "idempotency: record failed; a retry after the stale window would run again",
			"tenant", tenantID, "method", method, "error", err)
	}
	return resp, nil
}

type record struct {
	method string
	hash   []byte
	status string
	resp   []byte
}

// claim takes the key for this request, or returns what holds it. A key
// past its TTL, or in progress past StaleAfter, is taken over in the same
// statement that checks it, so two retries cannot both take it.
func (i *Interceptor) claim(ctx context.Context, pool *pgxpool.Pool, tenantID, key, method string, hash []byte) (bool, record, error) {
	const insert = `
		INSERT INTO idempotency_keys (tenant_id, key, method, request_hash, status)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, key) DO NOTHING`
	tag, err := pool.Exec(ctx, insert, tenantID, key, method, hash, statusInProgress)
	if err != nil {
		return false, record{}, err
	}
	if tag.RowsAffected() == 1 {
		return true, record{}, nil
	}

	const takeover = `
		UPDATE idempotency_keys
		SET method = $3, request_hash = $4, status = $5, response = NULL, created_at = now()
		WHERE tenant_id = $1 AND key = $2
		  AND (created_at < now() - make_interval(secs => $6)
		       OR (status = $5 AND created_at < now() - make_interval(secs => $7)))`
	tag, err = pool.Exec(ctx, takeover, tenantID, key, method, hash, statusInProgress, i.ttl.Seconds(), i.staleAfter.Seconds())
	if err != nil {
		return false, record{}, err
	}
	if tag.RowsAffected() == 1 {
		return true, record{}, nil
	}

	var r record
	err = pool.QueryRow(ctx, `SELECT method, request_hash, status, response FROM idempotency_keys WHERE tenant_id = $1 AND key = $2`,
		tenantID, key).Scan(&r.method, &r.hash, &r.status, &r.resp)
	if errors.Is(err, pgx.ErrNoRows) {
		// Released between our insert and this read (the first attempt
		// failed). Retrying is the client's call: report it as in flight.
		return false, record{status: statusInProgress, hash: hash}, nil
	}
	return false, r, err
}

func replay(r record, hash []byte) (any, error) {
	if string(r.hash) != string(hash) {
		return nil, status.Error(codes.AlreadyExists, "idempotency: key already used for a different request")
	}
	if r.status != statusCompleted {
		return nil, status.Error(codes.Aborted, "idempotency: a request with this key is still in progress; retry later")
	}
	resp, err := unmarshalResponse(r.resp)
	if err != nil {
		return nil, status.Error(codes.Internal, "idempotency: recorded response unreadable")
	}
	return resp, nil
}

// marshalResponse stores the response as an Any, so the replay can rebuild
// the concrete type from the registry without knowing the handler.
func marshalResponse(resp any) ([]byte, error) {
	msg, ok := resp.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("response %T is not a proto message", resp)
	}
	a, err := anypb.New(msg)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(a)
}

func unmarshalResponse(b []byte) (proto.Message, error) {
	a := &anypb.Any{}
	if err := proto.Unmarshal(b, a); err != nil {
		return nil, err
	}
	return a.UnmarshalNew()
}

// Cleanup deletes one tenant's keys older than the TTL and returns how many.
// Run it periodically for every tenant (the relay's sweep is a natural
// place); a key past the TTL is already ignored, so this only reclaims
// space.
func (i *Interceptor) Cleanup(ctx context.Context, tenantID string) (int64, error) {
	pool, err := i.pools.PoolFor(ctx, tenantID)
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE tenant_id = $1 AND created_at < now() - make_interval(secs => $2)`,
		tenantID, i.ttl.Seconds())
	if err != nil {
		return 0, fmt.Errorf("idempotency: cleanup: %w", err)
	}
	return tag.RowsAffected(), nil
}

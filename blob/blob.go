// Package blob is where the BYTES of an attachment live, keyed by the
// storage key its metadata carries (proposal §4.4). It replaces the copies
// of `internal/blob` in strix-maintenance and strix-expenses with the same
// Store interface, plus the production implementation those copies never
// had: S3 (MinIO in `data`), with URLs signed for the browser.
//
// TENANT FIRST. A key is "<tenant>/<entity>/<id>/<name>", and every store in
// this package refuses a key whose first segment is not the tenant in the
// context (tenantctx, which the PEP interceptor seeds from the verified
// token). The key usually comes from a metadata row the Core already scoped
// to the tenant, but "usually" is the word every cross-tenant leak starts
// with; the check costs a string compare. Code with no request context (an
// event consumer) seeds it with tenantctx.WithTenant from the envelope.
//
// CORE SECOND. In S3 the object is "<core>/<key>": the bucket is shared, the
// credential is per Core (Vault), and its policy only reaches "<core>/*".
package blob

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// Store is the seam between the Core and wherever bytes live. Same
// signatures as the Cores' copies, so moving to the kit is an import change.
type Store interface {
	Put(ctx context.Context, key, contentType string, data []byte) error
	Get(ctx context.Context, key string) (data []byte, contentType string, err error)
	// Delete is idempotent: deleting what is not there is not an error.
	Delete(ctx context.Context, key string) error
}

// Presigner hands out time-limited URLs so a browser moves the bytes
// directly to and from the object store, without the Core proxying them.
// S3Store and MemStore implement it; DiskStore cannot.
type Presigner interface {
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, error)
}

// MaxPresignTTL bounds a signed URL. A link to a tenant's file is a bearer
// credential for that file: whoever has it reads it, logged in or not. An
// hour covers an upload from a slow phone; anything longer is a link that
// outlives the session that asked for it.
const MaxPresignTTL = time.Hour

// ErrNotFound is what Get returns for a key that holds nothing. It is
// fs.ErrNotExist, so errors.Is(err, fs.ErrNotExist) — what code written
// against DiskStore checks — keeps working.
var ErrNotFound = fs.ErrNotExist

// ErrInvalidKey is returned for a key that is malformed or belongs to another
// tenant.
var ErrInvalidKey = errors.New("blob: invalid key")

// CheckKey validates a key against the tenant in ctx: relative, no empty,
// "." or ".." segment, no backslash or control character, and the first
// segment equal to tenantctx.Tenant(ctx).
func CheckKey(ctx context.Context, key string) error {
	tenant := tenantctx.Tenant(ctx)
	if tenant == "" {
		return fmt.Errorf("%w: no tenant in context", ErrInvalidKey)
	}
	if key == "" || len(key) > 1024 {
		return fmt.Errorf("%w: empty or longer than 1024", ErrInvalidKey)
	}
	if strings.ContainsAny(key, "\\") {
		return fmt.Errorf("%w: backslash", ErrInvalidKey)
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control character", ErrInvalidKey)
		}
	}
	segs := strings.Split(key, "/")
	if len(segs) < 2 {
		return fmt.Errorf("%w: want \"<tenant>/<...>\"", ErrInvalidKey)
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return fmt.Errorf("%w: empty, \".\" or \"..\" segment", ErrInvalidKey)
		}
	}
	if segs[0] != tenant {
		return fmt.Errorf("%w: key belongs to another tenant", ErrInvalidKey)
	}
	return nil
}

func checkTTL(ttl time.Duration) error {
	if ttl <= 0 || ttl > MaxPresignTTL {
		return fmt.Errorf("blob: presign ttl %v outside (0, %v]", ttl, MaxPresignTTL)
	}
	return nil
}

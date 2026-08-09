// Package auth is the Core's PEP — Policy Enforcement Point (SCC Appendix D
// §A). It validates the access token the Shell brokers on every request,
// LOCALLY, with no round trip to the auth service.
//
// A Core never issues tokens and never handles passwords. It only validates
// (SEC-plat §5, SEC-auth §12).
//
// This lives in the kit rather than in each Core because it is the one place
// where a divergence between Cores is a security hole that nothing reports:
// both copies keep compiling, both keep passing their tests, and the one left
// behind keeps accepting tokens the other already rejects (gitops#38).
package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// Claims are the verified access-token claims handlers read from the context.
//
// NEVER read tenant or subject from a request body. Some proto messages carry a
// tenant_id field; it is legacy and informative, and this struct is the source
// of truth (SCC Appendix D §A.7).
type Claims struct {
	Subject      string
	TenantID     string
	Scope        string
	Entitlements []string
	AMR          []string
	ACR          string
	Email        string
}

// Verifier validates access tokens against the authorization server's JWKS.
//
// The key set is cached in-process with a short TTL and served stale on a
// transient fetch error: a JWKS endpoint blipping for a few seconds must not
// take the whole core down, and a stale key is still a key the issuer
// published. Rotation is handled by kid, so a rotated-out key simply stops
// matching.
type Verifier struct {
	jwksURL   string
	issuer    string
	audiences []string

	mu        sync.Mutex
	set       jwk.Set
	fetchedAt time.Time
	ttl       time.Duration
}

// NewVerifier builds the verifier for a JWKS URL, an expected issuer and the
// expected audience(s).
//
// audiences is a list so a phased rollout can accept more than one value; a
// token is accepted when it carries ANY of them.
func NewVerifier(jwksURL, issuer string, audiences []string) *Verifier {
	return &Verifier{
		jwksURL:   jwksURL,
		issuer:    issuer,
		audiences: audiences,
		ttl:       5 * time.Minute,
	}
}

func (v *Verifier) keySet(ctx context.Context) (jwk.Set, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.set != nil && time.Since(v.fetchedAt) < v.ttl {
		return v.set, nil
	}
	set, err := jwk.Fetch(ctx, v.jwksURL)
	if err != nil {
		if v.set != nil {
			return v.set, nil // stale beats failing on a transient fetch error
		}
		return nil, fmt.Errorf("auth: fetch jwks: %w", err)
	}
	v.set = set
	v.fetchedAt = time.Now()
	return set, nil
}

// Verify validates the token end to end, in this order (SCC Appendix D §A,
// SEC-auth §4.3–§4.4):
//
//  1. parse the JWS and read the protected header directly — never trust a
//     caller-controlled decode of "alg";
//  2. PIN THE ALGORITHM TO EdDSA IN CODE. Reject alg:none, HS256 and anything
//     else, whatever the header claims. This is the classic JWT forgery, and
//     the only defence is refusing to negotiate;
//  3. require typ == "at+jwt" (RFC 9068). An ID token also carries tenant_id
//     and must never pass as an access token;
//  4. verify the signature by kid against the cached JWKS (kid required);
//  5. verify iss (exact), aud, and exp/nbf/iat;
//  6. require tenant_id to be present — it is the isolation boundary, and a
//     token without it cannot be scoped to anything.
//
// Any failure at any step is an error, which the interceptor turns into
// codes.Unauthenticated.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Claims, error) {
	msg, err := jws.Parse([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("auth: parse jws: %w", err)
	}
	sigs := msg.Signatures()
	if len(sigs) == 0 {
		return nil, fmt.Errorf("auth: token has no signature")
	}
	hdr := sigs[0].ProtectedHeaders()
	if alg, _ := hdr.Algorithm(); alg != jwa.EdDSA() {
		return nil, fmt.Errorf("auth: unexpected alg %v, want EdDSA", alg)
	}
	if typ, _ := hdr.Type(); typ != "at+jwt" {
		return nil, fmt.Errorf("auth: unexpected typ %q, want at+jwt", typ)
	}

	set, err := v.keySet(ctx)
	if err != nil {
		return nil, err
	}
	tok, err := jwt.Parse([]byte(raw),
		jwt.WithKeySet(set, jws.WithRequireKid(true)),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.issuer),
	)
	if err != nil {
		return nil, fmt.Errorf("auth: verify token: %w", err)
	}

	// Audience any-of: jwt.WithAudience only takes a single value, so the check
	// is done here against the configured allowlist.
	tokAud, _ := tok.Audience()
	if !audienceMatch(tokAud, v.audiences) {
		return nil, fmt.Errorf("auth: audience mismatch")
	}

	c := &Claims{}
	c.Subject, _ = tok.Subject()
	_ = tok.Get("tenant_id", &c.TenantID)
	_ = tok.Get("scope", &c.Scope)
	_ = tok.Get("acr", &c.ACR)
	_ = tok.Get("email", &c.Email)
	c.Entitlements = getStringSlice(tok, "entitlements")
	c.AMR = getStringSlice(tok, "amr")
	if c.TenantID == "" {
		return nil, fmt.Errorf("auth: token missing tenant_id")
	}
	return c, nil
}

// audienceMatch reports whether any of the token's audiences is in the
// allowlist.
func audienceMatch(tokenAud, expected []string) bool {
	for _, want := range expected {
		for _, got := range tokenAud {
			if got == want {
				return true
			}
		}
	}
	return false
}

// getStringSlice reads a custom array claim, coercing element by element (jwx
// stores custom array claims as []any).
func getStringSlice(tok jwt.Token, name string) []string {
	var raw []any
	if err := tok.Get(name, &raw); err != nil {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

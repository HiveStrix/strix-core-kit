package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

const (
	testIssuer = "https://authn.strixapps.com"
	testAud    = "core-test"
	testKid    = "test-key-1"
)

// signer holds the Ed25519 key pair the tests sign with, plus the JWKS the
// verifier fetches.
type signer struct {
	priv    ed25519.PrivateKey
	jwksRaw []byte
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pubKey, err := jwk.Import(pub)
	if err != nil {
		t.Fatalf("import public key: %v", err)
	}
	if err := pubKey.Set(jwk.KeyIDKey, testKid); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	if err := pubKey.Set(jwk.AlgorithmKey, jwa.EdDSA()); err != nil {
		t.Fatalf("set alg: %v", err)
	}
	set := jwk.NewSet()
	if err := set.AddKey(pubKey); err != nil {
		t.Fatalf("add key: %v", err)
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return &signer{priv: priv, jwksRaw: raw}
}

// claimSet is what a token carries; the zero value plus the defaults applied in
// token() is a valid access token, so each test only states its deviation.
type claimSet struct {
	issuer   string
	audience []string
	subject  string
	tenantID any // any, so a test can omit it entirely
	expires  time.Time
	typ      string
	alg      jwa.KeyAlgorithm
	key      any
	entitles []string
}

func (s *signer) token(t *testing.T, c claimSet) string {
	t.Helper()
	if c.issuer == "" {
		c.issuer = testIssuer
	}
	if c.audience == nil {
		c.audience = []string{testAud}
	}
	if c.subject == "" {
		c.subject = "user-1"
	}
	if c.expires.IsZero() {
		c.expires = time.Now().Add(10 * time.Minute)
	}
	if c.typ == "" {
		c.typ = "at+jwt"
	}
	if c.alg == nil {
		c.alg = jwa.EdDSA()
	}
	if c.key == nil {
		c.key = s.priv
	}

	b := jwt.NewBuilder().
		Issuer(c.issuer).
		Audience(c.audience).
		Subject(c.subject).
		IssuedAt(time.Now()).
		Expiration(c.expires)
	if c.tenantID != nil {
		b = b.Claim("tenant_id", c.tenantID)
	}
	if c.entitles != nil {
		b = b.Claim("entitlements", c.entitles)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}

	hdrs := jws.NewHeaders()
	if err := hdrs.Set(jws.TypeKey, c.typ); err != nil {
		t.Fatalf("set typ: %v", err)
	}
	if err := hdrs.Set(jws.KeyIDKey, testKid); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	raw, err := jwt.Sign(tok, jwt.WithKey(c.alg, c.key, jws.WithProtectedHeaders(hdrs)))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(raw)
}

// jwksServer serves the signer's key set and counts how many times it was hit.
func jwksServer(t *testing.T, s *signer) (*httptest.Server, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	var hits atomic.Int64
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.jwksRaw)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &down
}

func TestVerifyAcceptsAValidAccessToken(t *testing.T) {
	s := newSigner(t)
	srv, _, _ := jwksServer(t, s)
	v := NewVerifier(srv.URL, testIssuer, []string{testAud})

	raw := s.token(t, claimSet{tenantID: "acme", entitles: []string{"expenses", "costing"}})
	claims, err := v.Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("Verify returned error: %v", err)
	}
	if claims.TenantID != "acme" {
		t.Errorf("TenantID = %q, want %q", claims.TenantID, "acme")
	}
	if claims.Subject != "user-1" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "user-1")
	}
	if len(claims.Entitlements) != 2 || claims.Entitlements[0] != "expenses" {
		t.Errorf("Entitlements = %v, want [expenses costing]", claims.Entitlements)
	}
}

// The forgeries and malformed tokens the PEP exists to stop. Each case must
// fail; what matters is that none of them ever returns claims.
func TestVerifyRejects(t *testing.T) {
	s := newSigner(t)
	srv, _, _ := jwksServer(t, s)
	v := NewVerifier(srv.URL, testIssuer, []string{testAud})

	otherKeyPub, otherKeyPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	_ = otherKeyPub

	// wantReason pins the check that must do the rejecting, for the cases where
	// a different check catching it first would mean the defence under test is
	// not actually wired. Empty means any rejection will do.
	cases := []struct {
		name       string
		token      func() string
		wantReason string
	}{
		{
			// The classic JWT forgery: swap the alg for a symmetric one and sign
			// with a guessable secret. Pinning EdDSA in code is the defence, and
			// it must be what fires — jwx would also refuse the key, but relying
			// on that would make this package's own check dead code nobody
			// notices removing.
			"HS256 instead of EdDSA",
			func() string {
				return s.token(t, claimSet{tenantID: "acme", alg: jwa.HS256(), key: []byte("not-a-real-secret")})
			},
			"unexpected alg",
		},
		{
			// An ID token also carries tenant_id, so without the typ check it
			// would pass as an access token (RFC 9068).
			"typ JWT instead of at+jwt",
			func() string { return s.token(t, claimSet{tenantID: "acme", typ: "JWT"}) },
			"unexpected typ",
		},
		{
			"issuer mismatch",
			func() string { return s.token(t, claimSet{tenantID: "acme", issuer: "https://evil.example"}) },
			"",
		},
		{
			"audience not in the allowlist",
			func() string { return s.token(t, claimSet{tenantID: "acme", audience: []string{"core-other"}}) },
			"audience mismatch",
		},
		{
			// tenant_id is the isolation boundary: a token without it cannot be
			// scoped to anything, so it must never yield claims.
			"missing tenant_id",
			func() string { return s.token(t, claimSet{}) },
			"missing tenant_id",
		},
		{
			"expired",
			func() string {
				return s.token(t, claimSet{tenantID: "acme", expires: time.Now().Add(-time.Minute)})
			},
			"",
		},
		{
			// Correctly shaped and correctly typed, but signed by a key the JWKS
			// does not publish.
			"signed by an unknown key",
			func() string { return s.token(t, claimSet{tenantID: "acme", key: otherKeyPriv}) },
			"",
		},
		{
			"not a JWS at all",
			func() string { return "not.a.token" },
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := v.Verify(context.Background(), tc.token())
			if err == nil {
				t.Fatalf("Verify accepted the token, claims = %+v", claims)
			}
			if claims != nil {
				t.Errorf("Verify returned claims alongside the error: %+v", claims)
			}
			if tc.wantReason != "" && !strings.Contains(err.Error(), tc.wantReason) {
				t.Errorf("rejected for the wrong reason:\n  got:  %v\n  want: an error mentioning %q", err, tc.wantReason)
			}
		})
	}
}

// The audience list exists so a phased rollout (shared audience -> per-core)
// can run without a flag day: a token carrying ANY configured value passes.
func TestVerifyAcceptsAnyConfiguredAudience(t *testing.T) {
	s := newSigner(t)
	srv, _, _ := jwksServer(t, s)
	v := NewVerifier(srv.URL, testIssuer, []string{"core-clients", "core-expenses"})

	for _, aud := range []string{"core-clients", "core-expenses"} {
		raw := s.token(t, claimSet{tenantID: "acme", audience: []string{aud}})
		if _, err := v.Verify(context.Background(), raw); err != nil {
			t.Errorf("audience %q rejected: %v", aud, err)
		}
	}
}

// A JWKS endpoint blipping for a few seconds must not take the Core down: the
// cached key set is served stale rather than failing closed on the fetch.
func TestVerifyServesStaleKeysOnATransientJWKSFailure(t *testing.T) {
	s := newSigner(t)
	srv, hits, down := jwksServer(t, s)
	v := NewVerifier(srv.URL, testIssuer, []string{testAud})

	raw := s.token(t, claimSet{tenantID: "acme"})
	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("first Verify failed: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("JWKS hits = %d, want 1", got)
	}

	// Expire the cache and take the endpoint down.
	v.mu.Lock()
	v.fetchedAt = time.Now().Add(-time.Hour)
	v.mu.Unlock()
	down.Store(true)

	if _, err := v.Verify(context.Background(), raw); err != nil {
		t.Fatalf("Verify failed while the JWKS endpoint was down, want stale keys: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("JWKS hits = %d, want 2 (the failed refetch)", got)
	}
}

// With no key set cached yet, an unreachable JWKS has nothing to fall back to
// and must fail closed.
func TestVerifyFailsWhenJWKSIsUnreachableAndNothingIsCached(t *testing.T) {
	s := newSigner(t)
	srv, _, down := jwksServer(t, s)
	down.Store(true)
	v := NewVerifier(srv.URL, testIssuer, []string{testAud})

	if _, err := v.Verify(context.Background(), s.token(t, claimSet{tenantID: "acme"})); err == nil {
		t.Fatal("Verify succeeded with no keys available, want an error")
	}
}

// The key set is cached: a second verification inside the TTL must not refetch.
func TestVerifyCachesTheKeySet(t *testing.T) {
	s := newSigner(t)
	srv, hits, _ := jwksServer(t, s)
	v := NewVerifier(srv.URL, testIssuer, []string{testAud})

	raw := s.token(t, claimSet{tenantID: "acme"})
	for range 3 {
		if _, err := v.Verify(context.Background(), raw); err != nil {
			t.Fatalf("Verify failed: %v", err)
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("JWKS hits = %d, want 1", got)
	}
}

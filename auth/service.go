package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// ServiceCredentials identify a Core as a confidential OAuth client of
// strix-auth, for the calls it makes with NO user behind them: an event
// consumer that has to read a neighbour's catalogue, a sweeper, a job.
//
// This is the machine-to-machine grant of the security contract (§5, item
// 6): `client_credentials`, confidential client only, the token's `sub` is
// the client id and its `tenant_id` comes from the client's binding or from
// an authorized override — which is what a shared Core that serves many
// tenants uses, one token per tenant. The Shell's provisioning client is the
// same pattern; this is the version every Core shares.
//
// The client itself is registered by a platform-admin in strix-auth
// (`POST /admin/v1/oauth-clients`, grant_types [client_credentials], scopes
// [core.read], allowed_audiences = the Cores it calls, confidential, and
// allow_tenant_override for a shared Core). The secret travels as a Secret
// declared by NAME in the Core's strix.yaml, never in the spec.
type ServiceCredentials struct {
	// TokenURL is strix-auth's token endpoint: issuer + "/oauth/token".
	TokenURL     string
	ClientID     string
	ClientSecret string
	// Scope requested on every token, e.g. "core.read". Empty asks for every
	// scope the client is registered with.
	Scope string
	// HTTPClient, when nil, is a client with a 10 s timeout.
	HTTPClient *http.Client
}

// Configured reports whether the three mandatory values are set.
func (c ServiceCredentials) Configured() bool {
	return c.TokenURL != "" && c.ClientID != "" && c.ClientSecret != ""
}

// ServiceTokens mints and caches client_credentials tokens, one per
// (tenant, audience). A token is reused until 30 s before it expires.
type ServiceTokens struct {
	cfg  ServiceCredentials
	http *http.Client
	now  func() time.Time

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	token string
	exp   time.Time
}

// NewServiceTokens builds the token source. Nil when the credentials are not
// configured, which Outgoing treats as "forward only": a Core without a
// registered client keeps working exactly as before for user calls, and its
// consumers get the callee's 401 instead of a token this Core cannot mint.
func NewServiceTokens(cfg ServiceCredentials) *ServiceTokens {
	if !cfg.Configured() {
		return nil
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &ServiceTokens{cfg: cfg, http: hc, now: time.Now, cache: map[string]cachedToken{}}
}

// Token returns a bearer token for tenantID with aud = audience, minting one
// when the cached one is missing or about to expire.
func (s *ServiceTokens) Token(ctx context.Context, tenantID, audience string) (string, error) {
	if s == nil {
		return "", errors.New("auth: service credentials not configured")
	}
	if tenantID == "" {
		return "", errors.New("auth: a service token needs a tenant")
	}
	key := tenantID + "\x00" + audience
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[key]; ok && s.now().Before(c.exp) {
		return c.token, nil
	}
	token, ttl, err := s.mint(ctx, tenantID, audience)
	if err != nil {
		return "", err
	}
	if ttl < time.Minute {
		ttl = time.Minute
	}
	s.cache[key] = cachedToken{token: token, exp: s.now().Add(ttl - 30*time.Second)}
	return token, nil
}

func (s *ServiceTokens) mint(ctx context.Context, tenantID, audience string) (string, time.Duration, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("tenant_id", tenantID)
	if audience != "" {
		form.Set("audience", audience)
	}
	if s.cfg.Scope != "" {
		form.Set("scope", s.cfg.Scope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// client_secret_basic (RFC 6749 §2.3.1), the method strix-auth advertises.
	req.SetBasicAuth(s.cfg.ClientID, s.cfg.ClientSecret)

	resp, err := s.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("auth: service token: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", 0, fmt.Errorf("auth: service token: unreadable response (http %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		// The OAuth error is safe to surface; the secret never is.
		return "", 0, fmt.Errorf("auth: service token refused (http %d): %s %s", resp.StatusCode, out.Error, out.ErrorDesc)
	}
	return out.AccessToken, time.Duration(out.ExpiresIn) * time.Second, nil
}

// Outgoing prepares the context of a call to another Core.
//
// The caller's own bearer, when the context carries one, is forwarded as is:
// a request a person made keeps that person's identity all the way down, and
// the callee authorizes HER (entitlements, roles, policies). Only when there
// is nobody — an event consumer, a job — is a service token minted, for the
// tenant in context and the callee's audience; the callee then sees this
// Core as the subject and authorizes it by scope. A service identity never
// replaces a user's: that would make the callee take this Core's word for
// who is asking.
//
// With no bearer to forward and no credentials (tokens == nil) the context
// goes out unchanged, and the callee answers Unauthenticated: the honest
// outcome, never a silent skip.
func Outgoing(ctx context.Context, tokens *ServiceTokens, audience string) (context.Context, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if bearer := md.Get("authorization"); len(bearer) > 0 {
			return metadata.AppendToOutgoingContext(ctx, "authorization", bearer[0]), nil
		}
	}
	if tokens == nil {
		return ctx, nil
	}
	tok, err := tokens.Token(ctx, tenantctx.Tenant(ctx), audience)
	if err != nil {
		return ctx, err
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok), nil
}

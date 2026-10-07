// Package natsconn dials NATS the way every Core must (proposal §2.4,
// condition 1D of the roadmap): with the service's own credential, from the
// environment, and its own reply inbox.
//
// NATS is moving from a node anyone can publish to into one user per
// service, with publish and subscribe permissions generated from each Core's
// events/catalog.yaml. During the migration a permissive `legacy` user is the
// broker's no_auth_user, so a Core without credentials keeps working; once
// the last service carries its own, `legacy` is deleted.
//
// The credential comes from the environment (Vault via ESO, like any other
// secret), in exactly one of three forms:
//
//	NATS_CREDS_FILE       a .creds file (JWT + seed)
//	NATS_NKEY_SEED_FILE   an nkey seed file
//	NATS_USER + NATS_PASSWORD
//
// None of them is anonymous (the legacy user). More than one is an error:
// guessing which one the operator meant is how a service ends up connected
// with a credential nobody audited.
//
// The inbox: request/reply and the JetStream API answer on _INBOX.>, and a
// user allowed to subscribe there reads every service's replies. Each
// service therefore gets its own prefix, _INBOX.<service>, and its NATS user
// may only subscribe to that one.
package natsconn

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// Environment variables Connect reads.
const (
	EnvUser        = "NATS_USER"
	EnvPassword    = "NATS_PASSWORD"
	EnvCredsFile   = "NATS_CREDS_FILE"
	EnvNkeySeed    = "NATS_NKEY_SEED_FILE"
	EnvService     = "NATS_SERVICE_NAME"
	InboxPrefixFmt = "_INBOX.%s"
)

type config struct {
	service string
	getenv  func(string) string
	extra   []nats.Option
}

// Option configures Connect.
type Option func(*config)

// WithService names the connection and gives it the reply inbox
// _INBOX.<service>. Without it, NATS_SERVICE_NAME is used; with neither the
// connection keeps the shared _INBOX prefix (only acceptable while the broker
// still has the legacy user).
func WithService(name string) Option { return func(c *config) { c.service = name } }

// WithNATSOptions appends raw nats.go options (applied last).
func WithNATSOptions(opts ...nats.Option) Option {
	return func(c *config) { c.extra = append(c.extra, opts...) }
}

// withEnv replaces os.Getenv, for tests.
func withEnv(f func(string) string) Option { return func(c *config) { c.getenv = f } }

// Options builds the nats.go options Connect uses: reconnect behaviour,
// credential and inbox. Exposed for a Core that dials NATS itself.
func Options(opts ...Option) ([]nats.Option, error) {
	c := &config{getenv: os.Getenv}
	for _, o := range opts {
		o(c)
	}
	out := []nats.Option{
		// The first connection retries in the background too: a DNS or NATS
		// hiccup while the pod starts must not leave it without a broker
		// until the next restart (outbox.Dial, v0.15.0).
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.ReconnectJitter(500*time.Millisecond, time.Second),
	}

	creds, seed := c.getenv(EnvCredsFile), c.getenv(EnvNkeySeed)
	user, pass := c.getenv(EnvUser), c.getenv(EnvPassword)
	set := 0
	for _, v := range []string{creds, seed, user + pass} {
		if v != "" {
			set++
		}
	}
	if set > 1 {
		return nil, fmt.Errorf("natsconn: more than one credential set (%s, %s, %s/%s); set exactly one", EnvCredsFile, EnvNkeySeed, EnvUser, EnvPassword)
	}
	switch {
	case creds != "":
		if _, err := os.Stat(creds); err != nil {
			return nil, fmt.Errorf("natsconn: %s: %w", EnvCredsFile, err)
		}
		out = append(out, nats.UserCredentials(creds))
	case seed != "":
		o, err := nats.NkeyOptionFromSeed(seed)
		if err != nil {
			return nil, fmt.Errorf("natsconn: %s: %w", EnvNkeySeed, err)
		}
		out = append(out, o)
	case user != "" || pass != "":
		if user == "" || pass == "" {
			return nil, fmt.Errorf("natsconn: %s and %s go together", EnvUser, EnvPassword)
		}
		out = append(out, nats.UserInfo(user, pass))
	}

	service := c.service
	if service == "" {
		service = c.getenv(EnvService)
	}
	if service != "" {
		if err := checkToken(service); err != nil {
			return nil, err
		}
		out = append(out, nats.Name(service), nats.CustomInboxPrefix(fmt.Sprintf(InboxPrefixFmt, service)))
	}
	return append(out, c.extra...), nil
}

// checkToken: the service name becomes one subject token, so it cannot carry
// a dot, a wildcard or whitespace.
func checkToken(s string) error {
	if strings.ContainsAny(s, ".*> \t\r\n") {
		return errors.New("natsconn: service name must be a single subject token (no dots, wildcards or spaces)")
	}
	return nil
}

// Connect dials url with Options. A broker not reachable yet is not an error
// (the first dial retries in the background); a malformed URL, an ambiguous
// or unreadable credential, or a bad service name is.
func Connect(url string, opts ...Option) (*nats.Conn, error) {
	o, err := Options(opts...)
	if err != nil {
		return nil, err
	}
	return nats.Connect(url, o...)
}

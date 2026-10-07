package natsconn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

func env(m map[string]string) Option {
	return withEnv(func(k string) string { return m[k] })
}

func apply(t *testing.T, opts ...Option) nats.Options {
	t.Helper()
	o, err := Options(opts...)
	if err != nil {
		t.Fatalf("Options: %v", err)
	}
	n := nats.GetDefaultOptions()
	for _, f := range o {
		if err := f(&n); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

func TestNoCredentialIsAnonymousWithReconnects(t *testing.T) {
	n := apply(t, env(nil))
	if n.User != "" || n.Password != "" || n.UserJWT != nil || n.Nkey != "" {
		t.Fatalf("no env must be anonymous: %+v", n)
	}
	if !n.RetryOnFailedConnect || n.MaxReconnect != -1 || n.ReconnectWait != 2*time.Second {
		t.Fatalf("reconnect settings = retry %v, max %d, wait %v", n.RetryOnFailedConnect, n.MaxReconnect, n.ReconnectWait)
	}
	if n.InboxPrefix != "" {
		t.Fatalf("no service, no custom inbox: %q", n.InboxPrefix)
	}
}

func TestUserAndPassword(t *testing.T) {
	n := apply(t, env(map[string]string{EnvUser: "core-billing", EnvPassword: "s3cret"}))
	if n.User != "core-billing" || n.Password != "s3cret" {
		t.Fatalf("user/password = %q/%q", n.User, n.Password)
	}
}

func TestCredsAndSeedFiles(t *testing.T) {
	dir := t.TempDir()
	kp, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	seed, _ := kp.Seed()
	pub, _ := kp.PublicKey()
	seedFile := filepath.Join(dir, "user.nk")
	if err := os.WriteFile(seedFile, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	n := apply(t, env(map[string]string{EnvNkeySeed: seedFile}))
	if n.Nkey != pub || n.SignatureCB == nil {
		t.Fatalf("nkey = %q, want %q", n.Nkey, pub)
	}

	credsFile := filepath.Join(dir, "user.creds")
	if err := os.WriteFile(credsFile, []byte("placeholder"), 0o600); err != nil {
		t.Fatal(err)
	}
	n = apply(t, env(map[string]string{EnvCredsFile: credsFile}))
	if n.UserJWT == nil || n.SignatureCB == nil {
		t.Fatal("a creds file must install the JWT and signature callbacks")
	}
}

func TestCredentialErrors(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "x")
	_ = os.WriteFile(f, []byte("x"), 0o600)
	cases := map[string]map[string]string{
		"two methods":      {EnvCredsFile: f, EnvUser: "u", EnvPassword: "p"},
		"seed and creds":   {EnvCredsFile: f, EnvNkeySeed: f},
		"user only":        {EnvUser: "u"},
		"password only":    {EnvPassword: "p"},
		"missing creds":    {EnvCredsFile: filepath.Join(dir, "nope")},
		"missing seed":     {EnvNkeySeed: filepath.Join(dir, "nope")},
		"bad service":      {EnvService: "core.billing"},
		"wildcard service": {EnvService: "core-*"},
	}
	for name, m := range cases {
		if _, err := Options(env(m)); err == nil {
			t.Errorf("%s: want an error", name)
		}
		if _, err := Connect("nats://127.0.0.1:1", env(m)); err == nil {
			t.Errorf("%s: Connect must fail too", name)
		}
	}
	if _, err := Options(env(nil), WithService("has space")); err == nil {
		t.Error("a service name with a space must be refused")
	}
}

func TestServiceInbox(t *testing.T) {
	n := apply(t, env(nil), WithService("core-billing"))
	if n.InboxPrefix != "_INBOX.core-billing" || n.Name != "core-billing" {
		t.Fatalf("inbox = %q, name = %q", n.InboxPrefix, n.Name)
	}
	n = apply(t, env(map[string]string{EnvService: "core-costing"}))
	if n.InboxPrefix != "_INBOX.core-costing" {
		t.Fatalf("inbox from env = %q", n.InboxPrefix)
	}
	// The option wins over the environment.
	n = apply(t, env(map[string]string{EnvService: "core-costing"}), WithService("core-billing"))
	if n.InboxPrefix != "_INBOX.core-billing" {
		t.Fatalf("inbox = %q", n.InboxPrefix)
	}
}

// Against a broker with users:
//
//	docker run --rm -d --name kit-nats-auth -p 54231:4222 public.ecr.aws/docker/library/nats:2.12-alpine --user core-test --pass s3cret
//	NATSCONN_TEST_URL=nats://127.0.0.1:54231 go test ./natsconn -run Broker -v
func TestBrokerWithUsers(t *testing.T) {
	url := os.Getenv("NATSCONN_TEST_URL")
	if url == "" {
		t.Skip("NATSCONN_TEST_URL not set")
	}
	wait := func(nc *nats.Conn) bool {
		for i := 0; i < 30 && !nc.IsConnected(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		return nc.IsConnected()
	}
	good, err := Connect(url, env(map[string]string{EnvUser: "core-test", EnvPassword: "s3cret"}), WithService("core-test"))
	if err != nil {
		t.Fatal(err)
	}
	defer good.Close()
	if !wait(good) {
		t.Fatal("the right credential did not connect")
	}

	bad, err := Connect(url, env(map[string]string{EnvUser: "core-test", EnvPassword: "wrong"}),
		WithNATSOptions(nats.RetryOnFailedConnect(false)))
	if err == nil {
		defer bad.Close()
		if wait(bad) {
			t.Fatal("a wrong password connected")
		}
	} else if !strings.Contains(strings.ToLower(err.Error()), "authorization") {
		t.Fatalf("wrong password: %v", err)
	}

	// Replies come back on the service's own inbox.
	sub, err := good.Subscribe("echo", func(m *nats.Msg) { _ = m.Respond([]byte(m.Reply)) })
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Unsubscribe()
	resp, err := good.Request("echo", nil, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(resp.Data), "_INBOX.core-test.") {
		t.Fatalf("reply subject = %q, want under _INBOX.core-test.", resp.Data)
	}
}

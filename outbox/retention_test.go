package outbox

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Every Core's stream is born with an age limit. Without one it grows forever,
// and the broker's storage is shared: when it fills, no Core can publish.
func TestStreamConfigBoundsTheAge(t *testing.T) {
	day := 24 * time.Hour
	cases := []struct {
		name       string
		configured time.Duration
		env        string
		want       time.Duration
	}{
		{"nothing said: the default", 0, "", DefaultStreamMaxAge},
		{"the environment", 0, "336h", 14 * day},
		{"Config wins over the environment", 7 * day, "336h", 7 * day},
		{"unreadable environment: the default, never forever", 0, "30d", DefaultStreamMaxAge},
		{"zero in the environment: the default, never forever", 0, "0", DefaultStreamMaxAge},
		{"negative in Config: forever, as before v0.24.0", -1, "336h", 0},
		{"negative in the environment: forever", 0, "-1s", 0},
		{"shorter than the redelivery ladders: raised", 30 * time.Minute, "", MinStreamMaxAge},
		{"shorter in the environment: raised", 0, "30m", MinStreamMaxAge},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("STRIX_STREAM_MAX_AGE", c.env)
			sc := streamConfig(Config{StreamName: "T", SubjectPrefix: "t.>", MaxAge: c.configured})
			if sc.MaxAge != c.want {
				t.Fatalf("MaxAge = %v, want %v", sc.MaxAge, c.want)
			}
			if sc.Name != "T" || len(sc.Subjects) != 1 || sc.Subjects[0] != "t.>" || sc.Retention != jetstream.LimitsPolicy {
				t.Fatalf("stream identity changed: %+v", sc)
			}
			// A size limit is reserved up front by the broker, and the sum of
			// every stream's must fit in its storage: not something a Core's
			// stream may claim on its own.
			if sc.MaxBytes != 0 || sc.MaxMsgs != 0 || sc.Discard != jetstream.DiscardOld {
				t.Fatalf("only the age is bounded, got MaxBytes=%d MaxMsgs=%d Discard=%v", sc.MaxBytes, sc.MaxMsgs, sc.Discard)
			}
		})
	}
}

// The deploy that brings the retention finds a stream that already exists,
// created without one and holding events: the limit reaches it on the first
// start, and a Core that asks for no limit takes it off again.
//
// Needs a NATS with JetStream already running (the CI starts one):
//
//	OUTBOX_TEST_NATS=nats://127.0.0.1:14222 go test ./outbox -run Retention -v
func TestConnectPutsTheRetentionOnAnExistingStream(t *testing.T) {
	url := os.Getenv("OUTBOX_TEST_NATS")
	if url == "" {
		t.Skip("OUTBOX_TEST_NATS not set")
	}
	t.Setenv("STRIX_STREAM_MAX_AGE", "")
	ctx := context.Background()
	nc, js, err := Dial(url)
	if err != nil {
		t.Fatalf("Dial = %v", err)
	}
	defer nc.Close()
	const stream = "RETENTION_TEST"
	_ = js.DeleteStream(ctx, stream)
	defer func() { _ = js.DeleteStream(ctx, stream) }()

	// The stream as every kit before v0.24.0 declared it.
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{
		Name: stream, Subjects: []string{"retentiontest.>"}, Retention: jetstream.LimitsPolicy,
	}); err != nil {
		t.Fatalf("CreateStream = %v", err)
	}
	if _, err := js.Publish(ctx, "retentiontest.x", []byte(`{}`)); err != nil {
		t.Fatalf("Publish = %v", err)
	}

	maxAge := func(cfg Config) time.Duration {
		t.Helper()
		cfg.StreamName, cfg.SubjectPrefix = stream, "retentiontest.>"
		r, err := Connect(ctx, url, nil, cfg)
		if err != nil {
			t.Fatalf("Connect = %v", err)
		}
		defer r.Close()
		deadline := time.Now().Add(15 * time.Second)
		for !r.ensureStream(ctx) {
			if time.Now().After(deadline) {
				t.Fatal("the stream was never ready")
			}
			time.Sleep(200 * time.Millisecond)
		}
		s, err := js.Stream(ctx, stream)
		if err != nil {
			t.Fatalf("Stream = %v", err)
		}
		info, err := s.Info(ctx)
		if err != nil {
			t.Fatalf("Info = %v", err)
		}
		if info.State.Msgs != 1 {
			t.Fatalf("the stream holds %d events, want the 1 it had: a fresh event must survive the update", info.State.Msgs)
		}
		return info.Config.MaxAge
	}

	if got := maxAge(Config{}); got != DefaultStreamMaxAge {
		t.Fatalf("existing stream MaxAge = %v after Connect, want %v", got, DefaultStreamMaxAge)
	}
	if got := maxAge(Config{MaxAge: -1}); got != 0 {
		t.Fatalf("MaxAge = %v after a Core asked for no limit, want 0", got)
	}
}

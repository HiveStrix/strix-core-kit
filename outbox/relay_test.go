package outbox

import (
	"context"
	"os"
	"testing"
	"time"
)

// A broker that does not answer when the Core starts must not cost the pod
// its relay: Connect returns a working Relay, without an error and without
// blocking, and the stream is simply not ready yet.
func TestConnectToleratesUnreachableBroker(t *testing.T) {
	start := time.Now()
	// Port 1 on loopback: nothing listens, the dial is refused at once.
	r, err := Connect(context.Background(), "nats://127.0.0.1:1", nil, Config{
		StreamName: "TEST", SubjectPrefix: "test.>",
	})
	if err != nil {
		t.Fatalf("Connect with an unreachable broker = %v, want nil error", err)
	}
	if r == nil {
		t.Fatal("Connect returned a nil Relay: the pod would run without one until restarted")
	}
	defer r.Close()
	if r.streamReady {
		t.Fatal("streamReady = true with no broker")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Connect blocked %v on an unreachable broker", elapsed)
	}
	if r.ensureStream(context.Background()) {
		t.Fatal("ensureStream reported ready with no broker")
	}
}

// The case that motivated the retry, against a real broker: the Core starts
// while NATS is still down, NATS comes up afterwards, and the stream gets
// created with no restart.
//
// Needs a NATS with JetStream that the test does NOT start itself: point
// OUTBOX_TEST_LATE_NATS at an address where a broker will appear AFTER the
// test begins, e.g.
//
//	OUTBOX_TEST_LATE_NATS=nats://127.0.0.1:54222 go test ./outbox -run Late &
//	sleep 3 && docker run --rm -d --name late-nats -p 54222:4222 nats:2 -js
func TestStreamCreatedWhenBrokerAppearsLate(t *testing.T) {
	url := os.Getenv("OUTBOX_TEST_LATE_NATS")
	if url == "" {
		t.Skip("OUTBOX_TEST_LATE_NATS not set")
	}
	ctx := context.Background()
	r, err := Connect(ctx, url, nil, Config{StreamName: "LATE_TEST", SubjectPrefix: "latetest.>"})
	if err != nil {
		t.Fatalf("Connect = %v", err)
	}
	defer r.Close()
	if r.streamReady {
		t.Fatal("the broker was already up: this test needs it to appear after Connect")
	}

	deadline := time.Now().Add(90 * time.Second)
	for !r.ensureStream(ctx) {
		if time.Now().After(deadline) {
			t.Fatal("the stream was never created after the broker came up")
		}
		time.Sleep(time.Second)
	}
	if _, err := r.js.Stream(ctx, "LATE_TEST"); err != nil {
		t.Fatalf("stream LATE_TEST not found on the broker: %v", err)
	}
}

// The relay and the consumers dial through natsconn: the credential comes
// from the environment and Config.Service gives the connection its own
// reply inbox.
func TestDialUsesTheServiceIdentity(t *testing.T) {
	for _, k := range []string{"NATS_USER", "NATS_PASSWORD", "NATS_CREDS_FILE", "NATS_NKEY_SEED_FILE", "NATS_SERVICE_NAME"} {
		t.Setenv(k, "")
	}
	r, err := Connect(context.Background(), "nats://127.0.0.1:1", nil, Config{StreamName: "T", SubjectPrefix: "t.>", Service: "core-x"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.nc.Opts.InboxPrefix != "_INBOX.core-x" {
		t.Fatalf("inbox = %q", r.nc.Opts.InboxPrefix)
	}

	t.Setenv("NATS_USER", "core-x")
	t.Setenv("NATS_PASSWORD", "pw")
	nc, _, err := Dial("nats://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if nc.Opts.User != "core-x" || nc.Opts.Password != "pw" {
		t.Fatalf("Dial ignored NATS_USER/NATS_PASSWORD: %q", nc.Opts.User)
	}
	t.Setenv("NATS_CREDS_FILE", "/nonexistent.creds")
	if _, _, err := Dial("nats://127.0.0.1:1"); err == nil {
		t.Fatal("an ambiguous credential must fail the dial")
	}
}

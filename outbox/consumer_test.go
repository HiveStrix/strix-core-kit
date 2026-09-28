package outbox

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestDialWithoutBrokerConfigured(t *testing.T) {
	nc, js, err := Dial("")
	if nc != nil || js != nil || err != nil {
		t.Fatalf("Dial(\"\") = %v, %v, %v; want nil, nil, nil", nc, js, err)
	}
}

// The consumer side of the bug the relay had: a broker that does not answer
// when the Core starts must not cost the pod its consumers.
func TestDialToleratesUnreachableBroker(t *testing.T) {
	start := time.Now()
	nc, js, err := Dial("nats://127.0.0.1:1")
	if err != nil {
		t.Fatalf("Dial with an unreachable broker = %v, want nil error", err)
	}
	if nc == nil || js == nil {
		t.Fatal("Dial returned no connection: the Core would stay deaf until restarted")
	}
	defer nc.Close()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Dial blocked %v on an unreachable broker", elapsed)
	}
}

// Waiting for the broker must not outlive the Core: SIGTERM ends the loop.
func TestSubscribeStopsWhenCancelledWithoutBroker(t *testing.T) {
	nc, js, err := Dial("nats://127.0.0.1:1")
	if err != nil {
		t.Fatalf("Dial = %v", err)
	}
	defer nc.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Subscribe(ctx, js, Subscription{
			Stream:   "ABSENT",
			Consumer: jetstream.ConsumerConfig{Durable: "test"},
			Handle:   func(jetstream.Msg) {},
		})
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Subscribe kept running after its context was cancelled")
	}
}

// The producer's stream does not exist when the consumer starts (the producer
// Core deploys later, or its relay has not swept yet) and appears afterwards:
// the durable binds and the event arrives, with no restart.
//
// Needs a NATS with JetStream already running:
//
//	docker run --rm -d --name kit-nats -p 54222:4222 nats:2 -js
//	OUTBOX_TEST_NATS=nats://127.0.0.1:54222 go test ./outbox -run StreamAppears -v
func TestSubscribeBindsWhenStreamAppearsLate(t *testing.T) {
	url := os.Getenv("OUTBOX_TEST_NATS")
	if url == "" {
		t.Skip("OUTBOX_TEST_NATS not set")
	}
	nc, js, err := Dial(url)
	if err != nil {
		t.Fatalf("Dial = %v", err)
	}
	defer nc.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_ = js.DeleteStream(ctx, "LATE_STREAM_TEST")

	got := subscribeForTest(ctx, js, "LATE_STREAM_TEST", "latestream.x")
	time.Sleep(time.Second) // the first bind attempt fails: no stream yet

	publishForTest(t, ctx, js, "LATE_STREAM_TEST", "latestream.x")
	waitForTest(t, got, 30*time.Second)
	_ = js.DeleteStream(ctx, "LATE_STREAM_TEST")
}

// The case that motivated the fix, against a real broker: the Core starts
// while NATS is still down, NATS comes up afterwards, and the consumer binds
// with no restart.
//
// Needs a NATS with JetStream that the test does NOT start itself: point
// OUTBOX_TEST_LATE_NATS at an address where a broker will appear AFTER the
// test begins, e.g.
//
//	OUTBOX_TEST_LATE_NATS=nats://127.0.0.1:54223 go test ./outbox -run BrokerAppears -v &
//	sleep 3 && docker run --rm -d --name late-nats -p 54223:4222 nats:2 -js
func TestSubscribeBindsWhenBrokerAppearsLate(t *testing.T) {
	url := os.Getenv("OUTBOX_TEST_LATE_NATS")
	if url == "" {
		t.Skip("OUTBOX_TEST_LATE_NATS not set")
	}
	nc, js, err := Dial(url)
	if err != nil {
		t.Fatalf("Dial = %v", err)
	}
	defer nc.Close()
	if nc.IsConnected() {
		t.Fatal("the broker was already up: this test needs it to appear after Dial")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := subscribeForTest(ctx, js, "LATE_BROKER_TEST", "latebroker.x")

	deadline := time.Now().Add(90 * time.Second)
	for !nc.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("the connection never came up after the broker did")
		}
		time.Sleep(time.Second)
	}
	publishForTest(t, ctx, js, "LATE_BROKER_TEST", "latebroker.x")
	waitForTest(t, got, 30*time.Second)
}

func subscribeForTest(ctx context.Context, js jetstream.JetStream, stream, subject string) <-chan struct{} {
	got := make(chan struct{}, 1)
	go Subscribe(ctx, js, Subscription{
		Stream: stream,
		Consumer: jetstream.ConsumerConfig{
			Durable:       "kit-test",
			FilterSubject: subject,
			AckPolicy:     jetstream.AckExplicitPolicy,
		},
		Handle: func(msg jetstream.Msg) {
			_ = msg.Ack()
			select {
			case got <- struct{}{}:
			default:
			}
		},
	})
	return got
}

func publishForTest(t *testing.T, ctx context.Context, js jetstream.JetStream, stream, subject string) {
	t.Helper()
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{Name: stream, Subjects: []string{subject}}); err != nil {
		t.Fatalf("creating stream %s: %v", stream, err)
	}
	if _, err := js.Publish(ctx, subject, []byte(`{}`)); err != nil {
		t.Fatalf("publishing to %s: %v", subject, err)
	}
}

func waitForTest(t *testing.T, got <-chan struct{}, within time.Duration) {
	t.Helper()
	select {
	case <-got:
	case <-time.After(within):
		t.Fatalf("no message handled within %v: the consumer never bound", within)
	}
}

package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/hs-javierviquez/strix-core-kit/natsconn"
)

// Dial connects to NATS the way every Core must: the FIRST connection retries
// in the background too.
//
// MaxReconnects only governs reconnecting after a first successful connection.
// Without RetryOnFailedConnect, a DNS or NATS hiccup in the seconds a pod takes
// to start fails the dial once and for all, and the Core runs without its
// relay or its consumers until the next restart. With it, Dial returns at once
// and the connection comes up whenever the broker answers.
//
// A nil connection with a nil error is returned when natsURL is empty: no
// broker is configured. The error return is kept for what retrying cannot fix:
// a malformed URL or invalid options.
//
// Since v0.20.0 it dials through natsconn: the service's NATS credential
// comes from the environment (NATS_CREDS_FILE, NATS_NKEY_SEED_FILE or
// NATS_USER/NATS_PASSWORD) and NATS_SERVICE_NAME gives it its own reply
// inbox, so a Core moves to its own NATS user by configuration alone. With
// none of them set it connects as before.
func Dial(natsURL string) (*nats.Conn, jetstream.JetStream, error) {
	return DialWith(natsURL)
}

// DialWith is Dial with natsconn options, e.g. natsconn.WithService.
func DialWith(natsURL string, opts ...natsconn.Option) (*nats.Conn, jetstream.JetStream, error) {
	if natsURL == "" {
		return nil, nil, nil
	}
	nc, err := natsconn.Connect(natsURL, opts...)
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	return nc, js, nil
}

// Subscription is one durable consumer of another Core's stream.
type Subscription struct {
	// Stream is the producer's stream, e.g. "CORE_EXPENSES".
	Stream string
	// Consumer is created or updated on every bind, so a change to it (a new
	// BackOff, a longer AckWait) reaches the broker on the next deploy.
	Consumer jetstream.ConsumerConfig
	// Handle receives every message. Acking, nak-with-delay and idempotency
	// (MarkProcessed) stay with the Core: they are domain decisions.
	Handle jetstream.MessageHandler
	// MaxRetry caps the wait between attempts while the stream is absent or
	// the durable cannot be created. The wait starts short and doubles up to
	// this cap. Zero means 5 minutes.
	MaxRetry time.Duration
}

const (
	// firstRetry is the first wait once the broker answers but the bind
	// fails. Short on purpose: when the whole cluster restarts, the producer's
	// relay creates its stream a couple of seconds after the broker is up.
	firstRetry = 5 * time.Second
	// defaultMaxRetry bounds the wait for a stream that may never appear: the
	// producer Core may simply not be deployed for this installation.
	defaultMaxRetry = 5 * time.Minute
	// brokerPoll is how often the connection is checked while the broker is
	// unreachable. It costs nothing: no request leaves the process.
	brokerPoll = 2 * time.Second
	// bindTimeout bounds one attempt, so a broker that accepts the TCP
	// connection but does not answer cannot stall the loop.
	bindTimeout = 10 * time.Second
)

// Subscribe binds s and consumes until ctx is cancelled. It never gives up.
//
// A broker that is not reachable yet, a stream that does not exist yet and a
// durable that cannot be created right now are all the same thing to a
// consumer: not subscribed YET. Giving up on any of them left the Core deaf to
// that stream until its next restart, while the producer's events piled up
// unread. Nothing is lost meanwhile: JetStream keeps them, and the durable
// picks them up from where it left off once it binds.
//
// It blocks, so each subscription runs in its own goroutine. When ctx ends it
// stops the consumption before returning; closing the connection is the
// caller's (Drain, so in-flight handlers finish).
func Subscribe(ctx context.Context, js jetstream.JetStream, s Subscription) {
	maxRetry := s.MaxRetry
	if maxRetry <= 0 {
		maxRetry = defaultMaxRetry
	}
	durable := s.Consumer.Durable
	logged := false
	notYet := func(reason string, retryIn time.Duration) {
		level := slog.LevelDebug
		if !logged {
			level, logged = slog.LevelInfo, true
		}
		slog.Log(ctx, level, "consumer: not subscribed yet, will retry",
			"stream", s.Stream, "durable", durable, "reason", reason, "retry_in", retryIn.String())
	}

	wait := firstRetry
	for {
		var retryIn time.Duration
		if js.Conn().IsConnected() {
			cc, err := bind(ctx, js, s)
			if err == nil {
				slog.InfoContext(ctx, "consumer: subscribed", "stream", s.Stream, "durable", durable)
				<-ctx.Done()
				cc.Stop()
				return
			}
			retryIn = min(wait, maxRetry)
			wait = retryIn * 2
			notYet(err.Error(), retryIn)
		} else {
			retryIn = brokerPoll
			notYet("broker not reachable yet", retryIn)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryIn):
		}
	}
}

func bind(ctx context.Context, js jetstream.JetStream, s Subscription) (jetstream.ConsumeContext, error) {
	attempt, cancel := context.WithTimeout(ctx, bindTimeout)
	defer cancel()
	if _, err := js.Stream(attempt, s.Stream); err != nil {
		return nil, err
	}
	consumer, err := js.CreateOrUpdateConsumer(attempt, s.Stream, s.Consumer)
	if err != nil {
		return nil, err
	}
	return consumer.Consume(s.Handle)
}

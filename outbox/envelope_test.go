package outbox

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// occurred_at is when the change happened (the outbox row was written), not
// when the relay got around to publishing it. A relay that was down for an
// hour must not stamp an hour-old change with the time of the retry.
func TestEnvelopeCarriesCreationTime(t *testing.T) {
	created := time.Date(2026, 10, 5, 9, 30, 0, 0, time.FixedZone("CST", -6*3600))
	row := Row{ID: 7, EventID: "0b6f…", TenantID: "acme", Subject: "expenses.purchase_document.registered.v1",
		Payload: []byte(`{"id":"p1"}`), CreatedAt: created}

	raw, err := envelopeFor(row)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OccurredAt != "2026-10-05T15:30:00Z" {
		t.Fatalf("occurred_at = %q, want the row's creation time in UTC", env.OccurredAt)
	}
	if env.EventID != row.EventID || env.TenantID != "acme" || env.Subject != row.Subject || string(env.Data) != `{"id":"p1"}` {
		t.Fatalf("envelope = %+v", env)
	}
}

// Against a real broker: publishing the same row twice (the relay republishes
// when MarkPublished fails, and two replicas can race) leaves ONE message,
// because the event id travels as Nats-Msg-Id.
//
//	docker run --rm -d --name kit-nats -p 54223:4222 nats:2 -js
//	OUTBOX_TEST_NATS=nats://127.0.0.1:54223 go test ./outbox -run Dedup
func TestPublishDeduplicatesByEventID(t *testing.T) {
	url := os.Getenv("OUTBOX_TEST_NATS")
	if url == "" {
		t.Skip("OUTBOX_TEST_NATS not set")
	}
	ctx := context.Background()
	nc, js, err := Dial(url)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	for i := 0; i < 50 && !nc.IsConnected(); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	name := "DEDUP_TEST"
	_ = js.DeleteStream(ctx, name)
	if _, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: name, Subjects: []string{"deduptest.>"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), name) })

	row := Row{ID: 1, EventID: "5d1c2a3e-0000-4000-8000-000000000001", TenantID: "acme",
		Subject: "deduptest.thing.happened.v1", Payload: []byte(`{}`), CreatedAt: time.Now()}
	for i := 0; i < 2; i++ {
		if err := publish(ctx, js, row); err != nil {
			t.Fatalf("publish #%d: %v", i+1, err)
		}
	}
	info, err := js.Stream(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	st, err := info.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.State.Msgs != 1 {
		t.Fatalf("messages in stream = %d, want 1 (deduplicated by event id)", st.State.Msgs)
	}
}

package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// A row without traceparent or causation — every row of a Core without the
// columns — produces exactly the envelope it produced before: no new keys.
func TestEnvelopeWithoutMetaIsUnchanged(t *testing.T) {
	row := Row{EventID: "e1", TenantID: "acme", Subject: "x.y.v1", Payload: []byte(`{}`),
		CreatedAt: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	raw, err := envelopeFor(row)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"event_id":"e1","tenant_id":"acme","subject":"x.y.v1","occurred_at":"2026-10-06T12:00:00Z","data":{}}`
	if string(raw) != want {
		t.Fatalf("envelope = %s\nwant       %s", raw, want)
	}
}

func TestEnvelopeCarriesTraceparentAndCausation(t *testing.T) {
	row := Row{EventID: "e2", TenantID: "acme", Subject: "x.y.v1", Payload: []byte(`{}`), CreatedAt: time.Now(),
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		Causation:   []byte(`{"event_id":"e1","principal_type":"agent","principal_id":"inst-7","chain":["u-1","inst-7"]}`)}
	raw, err := envelopeFor(row)
	if err != nil {
		t.Fatal(err)
	}
	env, err := ParseEnvelope(raw)
	if err != nil {
		t.Fatal(err)
	}
	if env.Traceparent != row.Traceparent {
		t.Fatalf("traceparent = %q", env.Traceparent)
	}
	c := env.Causation
	if c == nil || c.EventID != "e1" || c.PrincipalType != "agent" || c.PrincipalID != "inst-7" || strings.Join(c.Chain, ",") != "u-1,inst-7" {
		t.Fatalf("causation = %+v", c)
	}
	// Consumers that predate the fields still parse the envelope.
	var old struct {
		EventID string          `json:"event_id"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &old); err != nil || old.EventID != "e2" {
		t.Fatalf("an old consumer cannot read the envelope: %v", err)
	}
}

func TestEnvelopeRejectsMalformedCausation(t *testing.T) {
	row := Row{EventID: "e3", Payload: []byte(`{}`), CreatedAt: time.Now(), Causation: []byte(`[1,2]`)}
	if _, err := envelopeFor(row); err == nil {
		t.Fatal("a causation that is not an object must not be published as if it were")
	}
}

func TestNextCausation(t *testing.T) {
	root, err := NextCausation("e1", nil, "user", "u-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if root.EventID != "e1" || root.PrincipalID != "u-1" || strings.Join(root.Chain, ",") != "u-1" {
		t.Fatalf("root = %+v", root)
	}
	next, err := NextCausation("e2", root, "agent", "inst-7", 0)
	if err != nil {
		t.Fatal(err)
	}
	if next.EventID != "e2" || next.PrincipalType != "agent" || strings.Join(next.Chain, ",") != "u-1,inst-7" {
		t.Fatalf("next = %+v", next)
	}
	// The parent's chain is not aliased by the child.
	if len(root.Chain) != 1 {
		t.Fatalf("NextCausation modified its input: %v", root.Chain)
	}

	// The agent reacting to an event its own action caused, through another
	// principal: a cycle.
	third, err := NextCausation("e3", next, "service", "core-billing", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NextCausation("e4", third, "agent", "inst-7", 0); !errors.Is(err, ErrCausationCycle) {
		t.Fatalf("agent loop: err = %v, want ErrCausationCycle", err)
	}
	// Directly on its own event too.
	if _, err := NextCausation("e3", next, "agent", "inst-7", 0); !errors.Is(err, ErrCausationCycle) {
		t.Fatalf("self trigger: err = %v, want ErrCausationCycle", err)
	}

	// Depth: a chain of maxDepth may not grow.
	c := (*Causation)(nil)
	for i := 0; i < 3; i++ {
		c, err = NextCausation("e", c, "agent", string(rune('a'+i)), 3)
		if err != nil {
			t.Fatalf("hop %d: %v", i+1, err)
		}
	}
	if _, err := NextCausation("e", c, "agent", "d", 3); !errors.Is(err, ErrCausationTooDeep) {
		t.Fatalf("hop 4 of 3: err = %v, want ErrCausationTooDeep", err)
	}
	// The default bound applies when none is given.
	c = nil
	for i := 0; i < DefaultMaxCausationDepth; i++ {
		if c, err = NextCausation("e", c, "agent", string(rune('a'+i)), 0); err != nil {
			t.Fatalf("hop %d: %v", i+1, err)
		}
	}
	if _, err := NextCausation("e", c, "agent", "z", 0); !errors.Is(err, ErrCausationTooDeep) {
		t.Fatalf("past the default: err = %v", err)
	}

	// A received chain that already repeats someone is refused as is.
	bad := &Causation{Chain: []string{"a", "b", "a"}}
	if _, err := NextCausation("e", bad, "agent", "c", 0); !errors.Is(err, ErrCausationCycle) {
		t.Fatalf("incoming cycle: err = %v", err)
	}
	if err := bad.Validate(0); !errors.Is(err, ErrCausationCycle) {
		t.Fatalf("Validate: %v", err)
	}
	if _, err := NextCausation("e", nil, "agent", "", 0); err == nil {
		t.Fatal("a causation needs a principal")
	}
}

func TestContextCarriesTheEnvelope(t *testing.T) {
	env := Envelope{EventID: "e9", Traceparent: "00-aa-bb-01", Causation: &Causation{Chain: []string{"u-1"}}}
	ctx := ContextWithEnvelope(context.Background(), env)
	if TraceparentFrom(ctx) != "00-aa-bb-01" {
		t.Fatal("traceparent not in context")
	}
	id, c, ok := CausationFrom(ctx)
	if !ok || id != "e9" || c == nil || c.Chain[0] != "u-1" {
		t.Fatalf("CausationFrom = %q %+v %v", id, c, ok)
	}
	if _, _, ok := CausationFrom(context.Background()); ok {
		t.Fatal("an empty context has no causation")
	}
	if TraceparentFrom(context.Background()) != "" {
		t.Fatal("an empty context has no traceparent")
	}
}

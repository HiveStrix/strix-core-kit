package capabilities

import "testing"

func TestIsRead(t *testing.T) {
	without := map[string]bool{
		"billing.invoice.read": true, "billing.invoice.list": true, "billing.invoice.get": true,
		"billing.client.lookup": false, "billing.invoice.search": false, "billing.report.view": false,
		"billing.invoice.write": false, "billing.taxrate.admin": false, "billing.read.write": false,
		"billing.invoice.readall": false,
	}
	for action, want := range without {
		if got := IsRead(nil, action); got != want {
			t.Errorf("IsRead(nil, %q) = %v, want %v", action, got, want)
		}
	}
	cat := map[string]Effect{"a.x.export": EffectRead, "a.x.list": EffectWrite, "a.x.del": EffectDelete}
	with := map[string]bool{"a.x.export": true, "a.x.list": false, "a.x.del": false, "a.x.get": false}
	for action, want := range with {
		if got := IsRead(cat, action); got != want {
			t.Errorf("IsRead(catalog, %q) = %v, want %v", action, got, want)
		}
	}
	// An empty but non-nil catalog lists nothing, so nothing is a read.
	if IsRead(map[string]Effect{}, "a.x.read") {
		t.Error("an empty catalog must not fall back to verbs")
	}
}

func TestEffectValid(t *testing.T) {
	for _, e := range []Effect{EffectRead, EffectWrite, EffectExternalSideEffect, EffectDelete} {
		if !e.Valid() {
			t.Errorf("%q must be valid", e)
		}
	}
	for _, e := range []Effect{"", "Read", "reed", "side_effect"} {
		if e.Valid() {
			t.Errorf("%q must not be valid", e)
		}
	}
}

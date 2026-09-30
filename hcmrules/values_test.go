package hcmrules

import "testing"

func TestDecodeValues(t *testing.T) {
	got, err := DecodeValues(`{"entitlement_days": 12, "weeks_base": "50", "rate": 19.5}`)
	if err != nil {
		t.Fatalf("DecodeValues: %v", err)
	}
	want := map[string]string{"entitlement_days": "12", "weeks_base": "50", "rate": "19.5"}
	if len(got) != len(want) {
		t.Fatalf("got %d values, want %d", len(got), len(want))
	}
	for key, w := range want {
		if !got[key].Equal(d(w)) {
			t.Errorf("%s: got %s, want %s", key, got[key], w)
		}
	}

	empty, err := DecodeValues("")
	if err != nil || len(empty) != 0 {
		t.Errorf("an empty document must decode to no values, got %v (%v)", empty, err)
	}

	for _, bad := range []string{`{"a": true}`, `{"a": {"b": 1}}`, `{"a": "doce"}`, `[1, 2]`, `{`} {
		if _, err := DecodeValues(bad); err == nil {
			t.Errorf("expected an error for %s", bad)
		}
	}
}

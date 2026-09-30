package hcmrules

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// internalScale is the fractional precision kept for intermediate division
// inside a formula. Callers round to whatever scale their own storage needs
// (money_amount numeric(18,4), rule_value numeric(24,10)) when they persist
// the result — this package has no schema of its own to round for, unlike
// core-costing's moneyScale, so it stays maximally precise instead of
// guessing a scale on their behalf.
const internalScale = 16

// requireValue fetches key from values (a formula's coefficients/inputs or a
// predicate's params/facts) and errors explicitly when it is missing.
//
// A missing legal parameter is a data-entry bug, not a scenario the engine
// should paper over with a silent zero: a forgotten "cap_days" must fail
// loudly, not quietly cap severance at zero.
func requireValue(values map[string]decimal.Decimal, key, kind string) (decimal.Decimal, error) {
	v, ok := values[key]
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: missing %q", kind, key)
	}
	return v, nil
}

// DecodeValues parses a rule's JSON object of coefficients/params (the
// `coefficients` of a Formula, the `params` of an EligibilityRule as
// GetRuleset returns them) into exact decimals.
//
// Numbers are read as their literal text (json.Number), never through
// float64: 19.5 must stay 19.5, not 19.499999999999998. A value may also be a
// decimal string ("19.5"). Anything else — a nested object, a bool, a
// non-numeric string — is a data error and fails loudly, like a missing
// coefficient does at evaluation.
func DecodeValues(raw string) (map[string]decimal.Decimal, error) {
	out := map[string]decimal.Decimal{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return nil, fmt.Errorf("hcmrules: decode values: %w", err)
	}
	for key, v := range fields {
		var text string
		switch x := v.(type) {
		case json.Number:
			text = x.String()
		case string:
			text = x
		default:
			return nil, fmt.Errorf("hcmrules: decode values: %q is not a number", key)
		}
		n, err := decimal.NewFromString(text)
		if err != nil {
			return nil, fmt.Errorf("hcmrules: decode values: %q: %w", key, err)
		}
		out[key] = n
	}
	return out, nil
}

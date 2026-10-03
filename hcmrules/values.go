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

// maxCount bounds every count coefficient. The longest legal table in the
// seeds is cesantía's 13 rows; a count far above that is not a law but a typo
// or a hostile preview (core-hcmrules' EvaluateFormula evaluates coefficients
// the caller sends), and it must not size a loop or an allocation.
const maxCount = 100

// requireCount fetches a count coefficient (how many bands or rows a rule
// has) and errors unless it is a whole number in [atLeast, maxCount]: a "2.5"
// or a negative count is a seed typo, and reading it as 2 or 0 would silently
// drop part of the law. The bound is checked on the decimal, before IntPart,
// so a 2^64 cannot wrap around into a small, plausible count.
func requireCount(values map[string]decimal.Decimal, key, kind string, atLeast int) (int, error) {
	v, err := requireValue(values, key, kind)
	if err != nil {
		return 0, err
	}
	if !v.IsInteger() || v.LessThan(decimal.NewFromInt(int64(atLeast))) || v.GreaterThan(decimal.NewFromInt(maxCount)) {
		return 0, fmt.Errorf("hcmrules: %s: %q = %s must be a whole number in [%d, %d]", kind, key, v, atLeast, maxCount)
	}
	return int(v.IntPart()), nil
}

// requireFlag fetches a 0/1 coefficient. Anything else is a data error rather
// than "truthy": a 0.5 boundary flag means the row was typed wrong.
func requireFlag(values map[string]decimal.Decimal, key, kind string) (bool, error) {
	v, err := requireValue(values, key, kind)
	if err != nil {
		return false, err
	}
	return flagValue(v, key, kind)
}

// optionalFlag is requireFlag for a flag added after rows were already
// seeded: absent reads as 0, so those rows keep their meaning.
func optionalFlag(values map[string]decimal.Decimal, key, kind string) (bool, error) {
	v, ok := values[key]
	if !ok {
		return false, nil
	}
	return flagValue(v, key, kind)
}

func flagValue(v decimal.Decimal, key, kind string) (bool, error) {
	switch {
	case v.Equal(decimal.Zero):
		return false, nil
	case v.Equal(decimal.NewFromInt(1)):
		return true, nil
	}
	return false, fmt.Errorf("hcmrules: %s: %q = %s must be 0 or 1", kind, key, v)
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

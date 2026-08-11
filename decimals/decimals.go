// Package decimals bounds the numbers a Core accepts from its users.
//
// WHY THIS EXISTS: a decimal.Decimal is a coefficient and an exponent, so
// holding "1e900000000" is free — and rendering it costs one byte per digit.
// Cores render every number they persist (totals, unit costs, event payloads,
// explanation JSON), so a single absurd magnitude anywhere in a request turns
// into gigabytes of allocation while it is being written back.
//
// That is not hypothetical. On 2026-08-11 core-expenses was OOMKilled twice
// (exit 137) while creating a purchase document. Memory went from 3 MiB to past
// its 512 MiB limit in under two seconds, the metrics-server never sampled the
// spike, and SIGKILL left no log line behind. Measured growth is linear in the
// exponent: 1e1000 renders 2 KB, 1e100000 renders 200 KB, 1e9 renders about a
// gigabyte.
//
// The check is cheap precisely because it never renders: NumDigits and Exponent
// read the representation. A guard that had to call String() first would BE the
// bomb it guards against.
//
// This lives in the kit rather than in one Core because every Core parses user
// numbers, and the failure is silent until it is fatal: the process dies with no
// error to attribute it to. One implementation is also one place to change the
// day a Core legitimately needs wider bounds.
package decimals

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// Limits describes the accepted magnitude and precision of a number.
type Limits struct {
	// MaxDigits is the largest magnitude, counted as integer digits.
	MaxDigits int
	// MinExponent is the finest fraction. -6 admits six decimal places.
	MinExponent int
}

var (
	// Money bounds amounts and quantities: 15 integer digits covers 999 trillion
	// in any currency, and six decimals is finer than any money or quantity
	// column stores. Anything beyond is a typo or an attack.
	Money = Limits{MaxDigits: 15, MinExponent: -6}

	// Factor bounds conversion factors, rates and percentages, which are
	// legitimately more precise than money — a unit factor is stored with ten
	// decimals — but never large.
	Factor = Limits{MaxDigits: 15, MinExponent: -12}
)

// Parse reads a user-supplied number with Money limits. field names the input
// so the error can be shown next to it.
func Parse(s, field string) (decimal.Decimal, error) {
	return ParseWith(s, field, Money)
}

// ParseWith reads a user-supplied number with explicit limits.
func ParseWith(s, field string, l Limits) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("%s: %q no es un número válido", field, s)
	}
	if err := CheckWith(d, field, l); err != nil {
		return decimal.Zero, err
	}
	return d, nil
}

// Check validates an already-parsed number against Money limits.
func Check(d decimal.Decimal, field string) error {
	return CheckWith(d, field, Money)
}

// CheckWith validates an already-parsed number against explicit limits.
func CheckWith(d decimal.Decimal, field string, l Limits) error {
	if d.Exponent() < int32(l.MinExponent) {
		return fmt.Errorf("%s: demasiados decimales (máximo %d)", field, -l.MinExponent)
	}
	// NumDigits counts the coefficient; adding the exponent gives the magnitude
	// without materializing anything.
	if int(d.NumDigits())+int(d.Exponent()) > l.MaxDigits {
		return fmt.Errorf("%s: el valor es demasiado grande (máximo %d dígitos)", field, l.MaxDigits)
	}
	return nil
}

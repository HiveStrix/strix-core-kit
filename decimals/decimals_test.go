package decimals

import (
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

func TestParse_AcceptsRealisticValues(t *testing.T) {
	for _, s := range []string{"0", "1", "100.50", "999999999999999", "0.000001", "1e5", "-42.75"} {
		if _, err := Parse(s, "monto"); err != nil {
			t.Errorf("Parse(%q) rejected a realistic value: %v", s, err)
		}
	}
}

// The regression this package exists for. These are cheap to hold and fatal to
// render, which is why they must be refused before any arithmetic touches them.
func TestParse_RejectsRenderBombs(t *testing.T) {
	for _, s := range []string{"1e16", "1e100", "1e100000", "1e900000000"} {
		if _, err := Parse(s, "monto"); err == nil {
			t.Errorf("Parse(%q) accepted a value that would render as gigabytes", s)
		}
	}
}

func TestParse_RejectsExcessivePrecision(t *testing.T) {
	if _, err := Parse("0.00000001", "cantidad"); err == nil {
		t.Error("Parse accepted more precision than a money column can hold")
	}
}

// Factor admits what Money does not: a unit conversion factor is stored with ten
// decimals, so applying Money limits to one would reject valid catalog data.
func TestFactor_AdmitsMorePrecisionThanMoney(t *testing.T) {
	const tenDecimals = "1.0000000001"
	if _, err := ParseWith(tenDecimals, "factor", Factor); err != nil {
		t.Errorf("Factor rejected a legitimate conversion factor: %v", err)
	}
	if _, err := Parse(tenDecimals, "factor"); err == nil {
		t.Error("Money should not admit ten decimals; the two limit sets would be redundant")
	}
}

func TestError_NamesTheField(t *testing.T) {
	_, err := Parse("1e100000", "cantidad")
	if err == nil || !strings.Contains(err.Error(), "cantidad") {
		t.Errorf("the error must name the offending field so the UI can point at it: %v", err)
	}
}

// Completing at all is the assertion: materializing 1e900000000 would exhaust
// memory long before any comparison ran.
func TestCheck_DoesNotRender(t *testing.T) {
	huge, err := decimal.NewFromString("1e900000000")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := Check(huge, "monto"); err == nil {
		t.Error("Check accepted 1e900000000")
	}
}

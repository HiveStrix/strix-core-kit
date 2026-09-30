package hcmrules

import (
	"testing"

	"github.com/shopspring/decimal"
)

// d parses a literal into an exact decimal for test fixtures — never
// decimal.NewFromFloat, so a test constant never carries a binary-float
// rounding artifact that isn't there in the real figure.
func d(s string) decimal.Decimal {
	return decimal.RequireFromString(s)
}

func dptr(s string) *decimal.Decimal {
	v := d(s)
	return &v
}

// isrCRTiers is an ILLUSTRATIVE Costa Rica ISR-shaped table: the source
// theory only gives the 2026 exempt band (₡918,000, Decreto 45333-H) and the
// 0–25% overall range, not the full gazette bracket cutoffs, so tiers 2–4's
// exact cutoffs here are invented-but-self-consistent for the purpose of
// exercising EvalBracket, anchored to the one real figure available. The
// quick_deduction column is derived by hand so the two algorithms agree.
func isrCRTiers() []Tier {
	return []Tier{
		{Seq: 1, LowerBound: d("0"), UpperBound: dptr("918000"), Rate: d("0"), QuickDeduction: d("0")},
		{Seq: 2, LowerBound: d("918000"), UpperBound: dptr("1400000"), Rate: d("0.10"), QuickDeduction: d("91800")},
		{Seq: 3, LowerBound: d("1400000"), UpperBound: dptr("2500000"), Rate: d("0.15"), QuickDeduction: d("161800")},
		{Seq: 4, LowerBound: d("2500000"), UpperBound: nil, Rate: d("0.25"), QuickDeduction: d("411800")},
	}
}

// irrfBRTiers is an ILLUSTRATIVE Brazil IRRF-shaped table (the standard
// 0/7.5/15/22.5/27.5% progressive shape the theory names in Domain 6), with
// round invented cutoffs — the theory does not enumerate the exact current
// gazette cutoffs (and explicitly separates out the Lei 15.270/2025 "zero
// below R$5,000" reduction as its own mechanism, which is intentionally NOT
// modeled here to avoid inventing unverified legal detail). quick_deduction
// is derived by hand so the two algorithms agree.
func irrfBRTiers() []Tier {
	return []Tier{
		{Seq: 1, LowerBound: d("0"), UpperBound: dptr("2000"), Rate: d("0"), QuickDeduction: d("0")},
		{Seq: 2, LowerBound: d("2000"), UpperBound: dptr("3000"), Rate: d("0.075"), QuickDeduction: d("150")},
		{Seq: 3, LowerBound: d("3000"), UpperBound: dptr("4000"), Rate: d("0.15"), QuickDeduction: d("375")},
		{Seq: 4, LowerBound: d("4000"), UpperBound: dptr("5000"), Rate: d("0.225"), QuickDeduction: d("675")},
		{Seq: 5, LowerBound: d("5000"), UpperBound: nil, Rate: d("0.275"), QuickDeduction: d("925")},
	}
}

// inssPatronalBRTiers models the Brazil employer INSS contribution: a FLAT
// 20% with explicitly NO ceiling (Lei 8.212/1991 art. 22, cited verbatim in
// the theory as "NO ceiling"). Modeled as a single uncapped tier so EvalBracket
// exercises the UpperBound == nil path even in the degenerate one-tier case.
func inssPatronalBRTiers() []Tier {
	return []Tier{
		{Seq: 1, LowerBound: d("0"), UpperBound: nil, Rate: d("0.20"), QuickDeduction: d("0")},
	}
}

// inssEmpregadoBR2026Tiers is the REAL 2026 Brazil employee INSS table cited
// in the theory (7.5/9/12/14%, ceiling R$8,475.55). Unlike the two tables
// above, these cutoffs are the actual cited figures, not illustrative ones.
// It is CAPPED — the point of this fixture is the divergence past the cap:
// evalBracketMarginal keeps working (contribution simply stops accruing
// above the ceiling), evalBracketQuickDeduction has no tier to report from
// and errors. quick_deduction is derived by hand for the in-range cases.
func inssEmpregadoBR2026Tiers() []Tier {
	return []Tier{
		{Seq: 1, LowerBound: d("0"), UpperBound: dptr("1621.00"), Rate: d("0.075"), QuickDeduction: d("0")},
		{Seq: 2, LowerBound: d("1621.00"), UpperBound: dptr("2902.84"), Rate: d("0.09"), QuickDeduction: d("24.315")},
		{Seq: 3, LowerBound: d("2902.84"), UpperBound: dptr("4354.27"), Rate: d("0.12"), QuickDeduction: d("111.4002")},
		{Seq: 4, LowerBound: d("4354.27"), UpperBound: dptr("8475.55"), Rate: d("0.14"), QuickDeduction: d("198.4856")},
	}
}

func TestEvalBracket_ISR_CR(t *testing.T) {
	tiers := isrCRTiers()
	cases := []struct {
		name string
		base string
		want string
	}{
		{"below exempt band", "500000", "0"},
		{"exactly at exempt boundary", "918000", "0"},
		{"inside second tier", "1000000", "8200"},
		{"inside third tier", "2000000", "138200"},
		{"inside uncapped top tier", "5000000", "838200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalBracket(tiers, d(tc.base))
			if err != nil {
				t.Fatalf("EvalBracket: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("marginal = %s, want %s", got, tc.want)
			}
			// A well-formed table must agree with the published shortcut.
			qd, err := evalBracketQuickDeduction(sortedByLowerBound(tiers), d(tc.base))
			if err != nil {
				t.Fatalf("evalBracketQuickDeduction: %v", err)
			}
			if !qd.Equal(d(tc.want)) {
				t.Errorf("quick-deduction = %s, want %s", qd, tc.want)
			}
		})
	}
}

func TestEvalBracket_IRRF_BR(t *testing.T) {
	tiers := irrfBRTiers()
	cases := []struct {
		name string
		base string
		want string
	}{
		{"exempt", "1500", "0"},
		{"second tier", "2500", "37.5"},
		{"fourth tier", "4500", "337.5"},
		{"uncapped top tier", "10000", "1825"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalBracket(tiers, d(tc.base))
			if err != nil {
				t.Fatalf("EvalBracket: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("marginal = %s, want %s", got, tc.want)
			}
			qd, err := evalBracketQuickDeduction(sortedByLowerBound(tiers), d(tc.base))
			if err != nil {
				t.Fatalf("evalBracketQuickDeduction: %v", err)
			}
			if !qd.Equal(d(tc.want)) {
				t.Errorf("quick-deduction = %s, want %s", qd, tc.want)
			}
		})
	}
}

func TestEvalBracket_UncappedTopTier_INSSPatronalBR(t *testing.T) {
	tiers := inssPatronalBRTiers()
	cases := []struct {
		name string
		base string
		want string
	}{
		{"small base", "50000", "10000"},
		{"large base, still uncapped", "1000000", "200000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalBracket(tiers, d(tc.base))
			if err != nil {
				t.Fatalf("EvalBracket: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("marginal = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestEvalBracket_CappedTable_MarginalVsQuickDeductionDivergence is the
// documented reason evalBracketMarginal is primary: on a CAPPED table (the
// real 2026 BR employee INSS bands), both methods agree everywhere inside
// the table, but only marginal accumulation has an answer once base exceeds
// the ceiling — contribution simply stops accruing, which is correct
// behaviour, not an error. The quick-deduction shortcut has no tier left to
// report from and must error rather than guess.
func TestEvalBracket_CappedTable_MarginalVsQuickDeductionDivergence(t *testing.T) {
	tiers := inssEmpregadoBR2026Tiers()

	inRange := []struct {
		name string
		base string
		want string
	}{
		{"inside first tier", "1000", "75"},
		{"inside third tier", "3500", "308.5998"},
	}
	for _, tc := range inRange {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalBracket(tiers, d(tc.base))
			if err != nil {
				t.Fatalf("EvalBracket: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("marginal = %s, want %s", got, tc.want)
			}
			qd, err := evalBracketQuickDeduction(sortedByLowerBound(tiers), d(tc.base))
			if err != nil {
				t.Fatalf("evalBracketQuickDeduction: %v", err)
			}
			if !qd.Equal(d(tc.want)) {
				t.Errorf("quick-deduction = %s, want %s", qd, tc.want)
			}
		})
	}

	t.Run("beyond the ceiling", func(t *testing.T) {
		base := d("10000")
		got, err := EvalBracket(tiers, base)
		if err != nil {
			t.Fatalf("EvalBracket: %v", err)
		}
		if !got.Equal(d("988.0914")) {
			t.Errorf("marginal = %s, want the capped total 988.0914", got)
		}
		if _, err := evalBracketQuickDeduction(sortedByLowerBound(tiers), base); err == nil {
			t.Error("evalBracketQuickDeduction: expected an error once base exceeds the capped table, got nil")
		}
	})
}

func TestEvalBracket_Errors(t *testing.T) {
	if _, err := EvalBracket(nil, d("100")); err == nil {
		t.Error("expected an error for an empty tier table")
	}
	if _, err := EvalBracket(isrCRTiers(), d("-1")); err == nil {
		t.Error("expected an error for a negative base")
	}
}

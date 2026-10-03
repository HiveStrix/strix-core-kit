package hcmrules

import (
	"fmt"
	"sort"

	"github.com/shopspring/decimal"
)

// Tier is one ordered band of a progressive bracket table (mirrors
// rule_bracket_tier). Seq is the storage/display order; the evaluator does
// not trust it for correctness and instead sorts a copy by LowerBound, since
// LowerBound is the field the math actually depends on.
type Tier struct {
	Seq            int
	LowerBound     decimal.Decimal
	UpperBound     *decimal.Decimal // nil = uncapped (the top tier of a table like an uncapped payroll tax)
	Rate           decimal.Decimal
	QuickDeduction decimal.Decimal
}

// EvalBracket applies a progressive bracket table to base and returns the
// amount owed.
//
// Two algorithms compute this number and MUST agree on a well-formed table:
//
//   - Marginal accumulation (the PRIMARY method, used for the returned value):
//     for every tier, tax the portion of base that falls inside
//     [LowerBound, UpperBound) at that tier's own Rate, and sum the portions.
//     This is the literal legal definition of a progressive scale, it needs
//     no precomputed column, and it stays correct even when QuickDeduction is
//     left at zero. It is also the only one of the two that is well-defined
//     for a CAPPED table once base exceeds the top tier (an uncapped
//     employer contribution keeps accruing; a capped one — e.g. an employee
//     contribution ceiling — correctly stops accruing above the ceiling,
//     which is not an error, just an empty tier from there on).
//   - "Parcela a deduzir" / quick-deduction (evalBracketQuickDeduction, kept
//     internal): amount = base × rate(tier containing base) −
//     quick_deduction(that tier). This is the shortcut CR Hacienda and BR
//     Receita Federal publish so a human can compute tax with one lookup
//     instead of walking every lower tier. It is only a correct shortcut
//     when QuickDeduction was derived correctly for every tier, and it has NO
//     answer once base falls beyond a capped table's last tier — which is
//     why it is not the primary method here, only a cross-check kept for
//     tables that publish it (see bracket_test.go for CR ISR / BR IRRF style
//     tables where both methods are asserted to agree).
func EvalBracket(tiers []Tier, base decimal.Decimal) (decimal.Decimal, error) {
	if len(tiers) == 0 {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: bracket table has no tiers")
	}
	if base.IsNegative() {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: bracket base %s cannot be negative", base.String())
	}
	return evalBracketMarginal(sortedByLowerBound(tiers), base), nil
}

// EvalBracketIncremental is what the slice of base between before and after
// adds to a progressive table: EvalBracket(after) − EvalBracket(before).
//
// It is how a monthly tax is withheld when the month has several payments.
// Costa Rica's ISR is levied on the income of the whole month (Ley 7092 arts.
// 32-33), so on each payment before is the month's taxable base already paid
// and after the base including this payment; the increments of every payment
// of the month add up to the tax of the month's total. Credits and what was
// already withheld are the caller's, not the table's.
//
// after below before is an error: a reversal is a decision about what to give
// back, not a negative slice of a table.
func EvalBracketIncremental(tiers []Tier, before, after decimal.Decimal) (decimal.Decimal, error) {
	if after.LessThan(before) {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: incremental bracket: after %s is below before %s", after, before)
	}
	hi, err := EvalBracket(tiers, after)
	if err != nil {
		return decimal.Decimal{}, err
	}
	lo, err := EvalBracket(tiers, before)
	if err != nil {
		return decimal.Decimal{}, err
	}
	return hi.Sub(lo), nil
}

// evalBracketMarginal sums, tier by tier, the portion of base that falls
// inside that tier's own range times that tier's Rate. tiers must already be
// sorted ascending by LowerBound.
func evalBracketMarginal(tiers []Tier, base decimal.Decimal) decimal.Decimal {
	total := decimal.Zero
	for _, t := range tiers {
		if base.LessThanOrEqual(t.LowerBound) {
			continue
		}
		upper := base
		if t.UpperBound != nil && t.UpperBound.LessThan(base) {
			upper = *t.UpperBound
		}
		portion := upper.Sub(t.LowerBound)
		total = total.Add(portion.Mul(t.Rate))
	}
	return total
}

// evalBracketQuickDeduction finds the single tier that contains base and
// applies the published shortcut. tiers must already be sorted ascending by
// LowerBound. It errors when no tier contains base, which happens whenever
// base sits beyond a capped table's last tier — a case evalBracketMarginal
// handles without error, which is exactly why that one is primary.
func evalBracketQuickDeduction(tiers []Tier, base decimal.Decimal) (decimal.Decimal, error) {
	for _, t := range tiers {
		inLower := base.GreaterThanOrEqual(t.LowerBound)
		inUpper := t.UpperBound == nil || base.LessThanOrEqual(*t.UpperBound)
		if inLower && inUpper {
			return base.Mul(t.Rate).Sub(t.QuickDeduction), nil
		}
	}
	return decimal.Decimal{}, fmt.Errorf("hcmrules: no tier of the bracket table contains base %s", base.String())
}

// sortedByLowerBound returns a copy of tiers ordered ascending by LowerBound,
// leaving the caller's slice untouched.
func sortedByLowerBound(tiers []Tier) []Tier {
	sorted := make([]Tier, len(tiers))
	copy(sorted, tiers)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].LowerBound.LessThan(sorted[j].LowerBound)
	})
	return sorted
}

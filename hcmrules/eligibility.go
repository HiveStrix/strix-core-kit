package hcmrules

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// eligibilityRegistry maps a predicate_kind to the pure Go function that
// evaluates it. Adding a jurisdiction's eligibility rule is one entry here
// plus its function.
var eligibilityRegistry = map[string]func(params, facts map[string]decimal.Decimal) (bool, error){
	"fmla_us":             checkFMLAUS,
	"vacation_vesting_br": checkVacationVestingBR,
}

// CheckEligibility resolves kind to a registered pure predicate and evaluates
// it against params (the jurisdiction's legal thresholds) and facts (this
// employee's situation). An unregistered kind fails explicitly rather than
// defaulting to ineligible, since that default would silently deny a right.
func CheckEligibility(kind string, params, facts map[string]decimal.Decimal) (bool, error) {
	fn, ok := eligibilityRegistry[kind]
	if !ok {
		return false, fmt.Errorf("hcmrules: unknown predicate_kind %q", kind)
	}
	return fn(params, facts)
}

// checkFMLAUS is the US FMLA eligibility test: employer headcount, employee
// tenure, and hours worked must ALL clear their threshold — three
// independent conditions ANDed together, every threshold a coefficient.
//
// The "within 75 miles" worksite-radius part of the real test is a
// geographic aggregation, not a decimal comparison; it is resolved upstream
// into the headcount fact this predicate receives, the same way it resolves
// any other pre-aggregated count.
//
//	params: min_headcount, min_tenure_months, min_hours
//	facts:  headcount, tenure_months, hours_worked
func checkFMLAUS(params, facts map[string]decimal.Decimal) (bool, error) {
	minHeadcount, err := requireValue(params, "min_headcount", "fmla_us")
	if err != nil {
		return false, err
	}
	minTenureMonths, err := requireValue(params, "min_tenure_months", "fmla_us")
	if err != nil {
		return false, err
	}
	minHours, err := requireValue(params, "min_hours", "fmla_us")
	if err != nil {
		return false, err
	}
	headcount, err := requireValue(facts, "headcount", "fmla_us")
	if err != nil {
		return false, err
	}
	tenureMonths, err := requireValue(facts, "tenure_months", "fmla_us")
	if err != nil {
		return false, err
	}
	hoursWorked, err := requireValue(facts, "hours_worked", "fmla_us")
	if err != nil {
		return false, err
	}
	return headcount.GreaterThanOrEqual(minHeadcount) &&
		tenureMonths.GreaterThanOrEqual(minTenureMonths) &&
		hoursWorked.GreaterThanOrEqual(minHours), nil
}

// checkVacationVestingBR is Brazil's período aquisitivo test: a single
// threshold on continuous months of service — deliberately a simpler shape
// than fmla_us's three-way AND, since the two kinds are meant to show that a
// predicate's arity is not fixed by the engine.
//
//	params: min_continuous_months
//	facts:  continuous_months
func checkVacationVestingBR(params, facts map[string]decimal.Decimal) (bool, error) {
	minContinuousMonths, err := requireValue(params, "min_continuous_months", "vacation_vesting_br")
	if err != nil {
		return false, err
	}
	continuousMonths, err := requireValue(facts, "continuous_months", "vacation_vesting_br")
	if err != nil {
		return false, err
	}
	return continuousMonths.GreaterThanOrEqual(minContinuousMonths), nil
}

package hcmrules

import (
	"fmt"

	"github.com/shopspring/decimal"
)

// formulaRegistry maps a formula_kind to the pure Go function that evaluates
// it. Adding a jurisdiction's formula is one entry here plus its function —
// never a new branch inside an existing one.
var formulaRegistry = map[string]func(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error){
	"aguinaldo_cr":            evalAguinaldoCR,
	"thirteenth_br":           evalThirteenthBR,
	"cesantia_cr":             evalCesantiaCR,
	"preaviso_cr":             evalPreavisoCR,
	"aviso_previo_br":         evalAvisoPrevioBR,
	"fgts_br":                 evalFGTSBR,
	"overtime":                evalOvertime,
	"min_wage_higher_of_us":   evalMinWageHigherOfUS,
	"vacation_accrual_cr":     vacationProportional("vacation_accrual_cr"),
	"vacation_proportional":   vacationProportional("vacation_proportional"),
	"vacation_period_vesting": evalVacationPeriodVesting,
}

// EvalFormula resolves kind to a registered pure function and evaluates it
// against coefficients (the jurisdiction's legal parameters) and inputs (the
// facts of this specific employee/period). Every number a lawyer would look
// up in a gazette lives in coefficients; the Go code only knows the shape of
// the calculation.
//
// An unregistered kind is a configuration error, not a zero result: it fails
// explicitly instead of silently returning nothing owed.
func EvalFormula(kind string, coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	fn, ok := formulaRegistry[kind]
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: unknown formula_kind %q", kind)
	}
	return fn(coefficients, inputs)
}

// evalAguinaldoCR is Costa Rica's aguinaldo (Código de Trabajo Art. 196):
// one-twelfth of the salaries ACTUALLY PAID over the accrual period. The
// summation itself happens upstream (payroll already knows every salary it
// paid); this formula only receives the already-summed total.
//
//	inputs:       total_annual_salary — Σ of gross salaries paid Dec–Nov
//	coefficients: divisor             — months in the accrual period (12)
func evalAguinaldoCR(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	total, err := requireValue(inputs, "total_annual_salary", "aguinaldo_cr")
	if err != nil {
		return decimal.Decimal{}, err
	}
	divisor, err := requireValue(coefficients, "divisor", "aguinaldo_cr")
	if err != nil {
		return decimal.Decimal{}, err
	}
	return total.DivRound(divisor, internalScale), nil
}

// evalThirteenthBR is Brazil's 13º salário: proportional to the number of
// months worked in the year, off the CURRENT monthly salary — a different
// shape from aguinaldo_cr, which sums the salaries actually paid instead of
// projecting the current one. This is the real legal distinction between the
// two (salary can vary month to month in both countries; CR's formula tracks
// that variation, BR's does not), not an arbitrary implementation choice.
//
//	inputs:       monthly_salary, months_worked
//	coefficients: divisor — months in a full year (12)
func evalThirteenthBR(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	monthlySalary, err := requireValue(inputs, "monthly_salary", "thirteenth_br")
	if err != nil {
		return decimal.Decimal{}, err
	}
	monthsWorked, err := requireValue(inputs, "months_worked", "thirteenth_br")
	if err != nil {
		return decimal.Decimal{}, err
	}
	divisor, err := requireValue(coefficients, "divisor", "thirteenth_br")
	if err != nil {
		return decimal.Decimal{}, err
	}
	return monthlySalary.DivRound(divisor, internalScale).Mul(monthsWorked), nil
}

// evalCesantiaCR is Costa Rica's cesantía (Código de Trabajo Art. 29): days
// owed per year of service, GRADUATED by tenure band, capped at a maximum
// number of years. The band table itself is data — any number of bands, each
// with its own days-per-year rate — so a change to the legal scale is a
// coefficients edit, never a code change.
//
//	inputs:       years_of_service
//	coefficients: cap_years, band_count,
//	              band<i>_upto_years, band<i>_days_per_year for i in 1..band_count
//	              (band<i>_upto_years is the cumulative years-of-service
//	              ceiling of that band; the last band's upto_years should
//	              equal cap_years by convention)
//
// Fractional years are prorated linearly within the band they fall in.
func evalCesantiaCR(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	years, err := requireValue(inputs, "years_of_service", "cesantia_cr")
	if err != nil {
		return decimal.Decimal{}, err
	}
	capYears, err := requireValue(coefficients, "cap_years", "cesantia_cr")
	if err != nil {
		return decimal.Decimal{}, err
	}
	bandCountValue, err := requireValue(coefficients, "band_count", "cesantia_cr")
	if err != nil {
		return decimal.Decimal{}, err
	}
	bandCount := int(bandCountValue.IntPart())

	effectiveYears := years
	if effectiveYears.GreaterThan(capYears) {
		effectiveYears = capYears
	}

	totalDays := decimal.Zero
	previousBound := decimal.Zero
	for i := 1; i <= bandCount; i++ {
		if !effectiveYears.GreaterThan(previousBound) {
			break
		}
		upto, err := requireValue(coefficients, fmt.Sprintf("band%d_upto_years", i), "cesantia_cr")
		if err != nil {
			return decimal.Decimal{}, err
		}
		daysPerYear, err := requireValue(coefficients, fmt.Sprintf("band%d_days_per_year", i), "cesantia_cr")
		if err != nil {
			return decimal.Decimal{}, err
		}
		bandTop := upto
		if bandTop.GreaterThan(effectiveYears) {
			bandTop = effectiveYears
		}
		span := bandTop.Sub(previousBound)
		if span.IsPositive() {
			totalDays = totalDays.Add(span.Mul(daysPerYear))
		}
		previousBound = upto
	}
	return totalDays, nil
}

// evalPreavisoCR is Costa Rica's preaviso (Código de Trabajo Art. 28): a STEP
// lookup by tenure, not a cumulative sum — the employee owes the notice of
// the single band their tenure has reached, not the sum of every band passed
// through. Below the first band's threshold nothing is owed. Bands may be
// given in any order; the one with the highest min_years that years_of_service
// still meets wins.
//
//	inputs:       years_of_service
//	coefficients: band_count, band<i>_min_years, band<i>_notice_days
func evalPreavisoCR(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	years, err := requireValue(inputs, "years_of_service", "preaviso_cr")
	if err != nil {
		return decimal.Decimal{}, err
	}
	bandCountValue, err := requireValue(coefficients, "band_count", "preaviso_cr")
	if err != nil {
		return decimal.Decimal{}, err
	}
	bandCount := int(bandCountValue.IntPart())

	noticeDays := decimal.Zero
	haveMatch := false
	bestMinYears := decimal.Zero
	for i := 1; i <= bandCount; i++ {
		minYears, err := requireValue(coefficients, fmt.Sprintf("band%d_min_years", i), "preaviso_cr")
		if err != nil {
			return decimal.Decimal{}, err
		}
		days, err := requireValue(coefficients, fmt.Sprintf("band%d_notice_days", i), "preaviso_cr")
		if err != nil {
			return decimal.Decimal{}, err
		}
		if years.GreaterThanOrEqual(minYears) && (!haveMatch || minYears.GreaterThan(bestMinYears)) {
			bestMinYears = minYears
			noticeDays = days
			haveMatch = true
		}
	}
	return noticeDays, nil
}

// evalAvisoPrevioBR is Brazil's aviso prévio (Lei 12.506/2011): a base notice
// plus a fixed increment per completed year of service, capped. Unlike
// preaviso_cr's step lookup, this is linear-with-a-cap — a second, distinct
// shape for what is conceptually the same legal concept (statutory notice),
// which is the point: the formula_kind carries the shape, coefficients carry
// the numbers.
//
//	inputs:       years_of_service
//	coefficients: base_days, per_year_days, cap_days
func evalAvisoPrevioBR(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	years, err := requireValue(inputs, "years_of_service", "aviso_previo_br")
	if err != nil {
		return decimal.Decimal{}, err
	}
	baseDays, err := requireValue(coefficients, "base_days", "aviso_previo_br")
	if err != nil {
		return decimal.Decimal{}, err
	}
	perYearDays, err := requireValue(coefficients, "per_year_days", "aviso_previo_br")
	if err != nil {
		return decimal.Decimal{}, err
	}
	capDays, err := requireValue(coefficients, "cap_days", "aviso_previo_br")
	if err != nil {
		return decimal.Decimal{}, err
	}
	total := baseDays.Add(perYearDays.Mul(years))
	if total.GreaterThan(capDays) {
		total = capDays
	}
	return total, nil
}

// evalFGTSBR is a plain rate-on-a-base formula shared by two distinct FGTS
// legal concepts, which is exactly why it stays one kind: the ordinary
// monthly deposit (base = salary, rate = 0.08) and the 40% termination
// penalty on a dismissal without cause (base = the accumulated FGTS balance,
// rate = 0.40) are the SAME shape, "amount = base × rate" — the multa IS a
// coefficient (a different rate), not a different algorithm. The caller picks
// which base and which rate to pass for each of the two calls.
//
//	inputs:       base — salary for the deposit, or accrued balance for the multa
//	coefficients: rate — 0.08 for the deposit, 0.40 for the without-cause penalty
func evalFGTSBR(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	base, err := requireValue(inputs, "base", "fgts_br")
	if err != nil {
		return decimal.Decimal{}, err
	}
	rate, err := requireValue(coefficients, "rate", "fgts_br")
	if err != nil {
		return decimal.Decimal{}, err
	}
	return base.Mul(rate), nil
}

// evalOvertime is the common overtime-pay shape across every jurisdiction in
// the source material (CR/BR/US 1.5×, ES ≥1.25×, CZ 1.25×): ordinary hourly
// rate times a premium multiplier times the hours worked at that premium.
//
//	inputs:       ordinary_hourly_rate, overtime_hours
//	coefficients: multiplier
func evalOvertime(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	hourlyRate, err := requireValue(inputs, "ordinary_hourly_rate", "overtime")
	if err != nil {
		return decimal.Decimal{}, err
	}
	hours, err := requireValue(inputs, "overtime_hours", "overtime")
	if err != nil {
		return decimal.Decimal{}, err
	}
	multiplier, err := requireValue(coefficients, "multiplier", "overtime")
	if err != nil {
		return decimal.Decimal{}, err
	}
	return hourlyRate.Mul(multiplier).Mul(hours), nil
}

// evalMinWageHigherOfUS is the US "higher of" rule: an employer owes the
// highest wage floor that applies to a worksite — federal, state, and in
// many cities a local ordinance on top of that (the source material notes 63
// localities exceeding their own state floor). Rather than hardcode exactly
// two named inputs (federal/state), this takes the maximum of however many
// wage-floor candidates the caller passes, so a city ordinance is just
// another entry in the same map, not a new formula_kind.
//
//	inputs: any number of wage-floor candidates (e.g. "federal", "state", "city")
//	coefficients: none — there is no jurisdiction number here, only a comparison
func evalMinWageHigherOfUS(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	if len(inputs) == 0 {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: min_wage_higher_of_us: no wage-floor inputs given")
	}
	highest := decimal.Decimal{}
	first := true
	for _, v := range inputs {
		if first || v.GreaterThan(highest) {
			highest = v
			first = false
		}
	}
	return highest, nil
}

// vacationProportional is proportional vacation accrual: a fixed entitlement
// prorated by weeks actually worked out of the accrual base — a third
// distinct shape alongside the sum (aguinaldo_cr) and the
// proportion-of-current-salary (thirteenth_br) forms above. Costa Rica's
// "two weeks per fifty weeks of work" (Código de Trabajo Art. 153) is this
// shape; so is a company policy written as "N days per year worked".
//
// "vacation_accrual_cr" is the kind the first CR seed used; it stays
// registered as the same function so rows seeded under that name keep
// evaluating.
//
//	inputs:       weeks_worked
//	coefficients: entitlement_days — full entitlement in days
//	              weeks_base       — weeks that earn the full entitlement
func vacationProportional(kind string) func(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	return func(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
		weeksWorked, err := requireValue(inputs, "weeks_worked", kind)
		if err != nil {
			return decimal.Decimal{}, err
		}
		entitlementDays, err := requireValue(coefficients, "entitlement_days", kind)
		if err != nil {
			return decimal.Decimal{}, err
		}
		weeksBase, err := requireValue(coefficients, "weeks_base", kind)
		if err != nil {
			return decimal.Decimal{}, err
		}
		if !weeksBase.IsPositive() {
			return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: weeks_base must be positive", kind)
		}
		return entitlementDays.Mul(weeksWorked).DivRound(weeksBase, internalScale), nil
	}
}

// evalVacationPeriodVesting is vacation granted in whole blocks: the full
// entitlement vests at the end of each completed accrual period and nothing
// accrues in between — Brazil's "30 dias corridos por período aquisitivo de
// 12 meses" (CLT Art. 130). The proportional share owed on termination is a
// payroll computation, not this balance.
//
//	inputs:       months_of_service — completed months of the contract
//	coefficients: days_per_period   — entitlement per completed period (30)
//	              period_months     — length of the accrual period (12)
func evalVacationPeriodVesting(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	months, err := requireValue(inputs, "months_of_service", "vacation_period_vesting")
	if err != nil {
		return decimal.Decimal{}, err
	}
	daysPerPeriod, err := requireValue(coefficients, "days_per_period", "vacation_period_vesting")
	if err != nil {
		return decimal.Decimal{}, err
	}
	periodMonths, err := requireValue(coefficients, "period_months", "vacation_period_vesting")
	if err != nil {
		return decimal.Decimal{}, err
	}
	if !periodMonths.IsPositive() {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: vacation_period_vesting: period_months must be positive")
	}
	if months.IsNegative() {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: vacation_period_vesting: months_of_service must not be negative")
	}
	periods := months.Div(periodMonths).Floor()
	return daysPerPeriod.Mul(periods), nil
}

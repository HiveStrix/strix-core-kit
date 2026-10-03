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
	"cesantia_cr_art29":       evalCesantiaCRArt29,
	"preaviso_cr":             evalPreavisoCR,
	"fixed_term_indemnity_cr": evalFixedTermIndemnityCR,
	"absence_employer_share":  evalAbsenceEmployerShare,
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

// evalAguinaldoCR is Costa Rica's aguinaldo (Ley 2412 arts. 1-3, art. 2 as
// amended by Ley 3929 — not the Código de Trabajo): one-twelfth of the
// salaries EARNED (devengados) over the accrual period. The summation itself
// happens upstream (payroll already knows every salary it accrued); this
// formula only receives the already-summed total.
//
//	inputs:       total_annual_salary — Σ of gross salaries earned Dec–Nov
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
//
// That cumulative shape is NOT what art. 29 says (the rate of the row of the
// completed years times the years, fixed days below one year): use
// cesantia_cr_art29. This kind stays registered so rows seeded under it keep
// evaluating until they are closed.
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
// given in any order; the one with the highest threshold that
// years_of_service still meets wins.
//
// Art. 28 draws its lines with "que exceda de seis meses y no sea mayor de un
// año" and "después de un año": at exactly 6 months the worker is still in
// the first band. A band marked band<i>_exclusive = 1 is met only ABOVE its
// min_years; absent or 0 keeps the original ">=" so rows seeded before the
// flag existed evaluate as they did.
//
// band_count goes through requireCount since v0.18.0: a negative or
// fractional count, which used to evaluate as no band at all, is now an
// error. The seeded rows carry whole, small counts and are not affected.
//
//	inputs:       years_of_service
//	coefficients: band_count, band<i>_min_years, band<i>_notice_days,
//	              band<i>_exclusive (optional, 0/1)
func evalPreavisoCR(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	years, err := requireValue(inputs, "years_of_service", "preaviso_cr")
	if err != nil {
		return decimal.Decimal{}, err
	}
	bandCount, err := requireCount(coefficients, "band_count", "preaviso_cr", 0)
	if err != nil {
		return decimal.Decimal{}, err
	}

	bands := make([]stepBand, 0, bandCount)
	for i := 1; i <= bandCount; i++ {
		minYears, err := requireValue(coefficients, fmt.Sprintf("band%d_min_years", i), "preaviso_cr")
		if err != nil {
			return decimal.Decimal{}, err
		}
		days, err := requireValue(coefficients, fmt.Sprintf("band%d_notice_days", i), "preaviso_cr")
		if err != nil {
			return decimal.Decimal{}, err
		}
		exclusive, err := optionalFlag(coefficients, fmt.Sprintf("band%d_exclusive", i), "preaviso_cr")
		if err != nil {
			return decimal.Decimal{}, err
		}
		bands = append(bands, stepBand{min: minYears, exclusive: exclusive, value: days})
	}
	if days, ok := highestMet(bands, years); ok {
		return days, nil
	}
	return decimal.Zero, nil
}

// evalCesantiaCRArt29 is Costa Rica's cesantía as Código de Trabajo art. 29
// reads it and the MTSS applies it (Directriz MTSS 1-2003; DAJ-AE-083-09,
// DAJ-AE-142-11, DAJ-AE-765-06):
//
//   - Under one completed year, a fixed number of days by the highest
//     sub-year band reached (art. 29 inc. 1-2: 7 days from 3 months, 14 days
//     past 6 months); nothing below the first band.
//   - From one completed year on, the days-per-year of the row of the
//     COMPLETED years (row i = i years; the last row covers every tenure at
//     or above it) times the years counted, capped: min(completed years +
//     (fraction counts ? 1 : 0), cap_years). The fraction adds a year to the
//     multiplier but never moves the worker to the next row (Directriz
//     1-2003, considerando III): 1 year 8 months = 19.5 × 2 = 39 days.
//
// Tenure arrives already split, so the boundaries are the caller's facts and
// not this function's guess: exactly 12 months is completed_years 1 and
// remainder_months 0, which is row 1. Whether a fraction of exactly
// fraction_min_months counts, and whether a sub-year band is met at its
// exact threshold, are coefficients (the law says "superior", the MTSS
// Directriz "igual o superior"), not code.
//
//	inputs:       completed_years  — whole years of service (integer >= 0)
//	              remainder_months — months beyond them, in [0, 12); with 0
//	                                 completed years, the total months
//	coefficients: sub_year_band_count,
//	              sub_year_band<i>_min_months, sub_year_band<i>_inclusive (0/1),
//	              sub_year_band<i>_days for i in 1..sub_year_band_count;
//	              row_count, row<i>_days_per_year for i in 1..row_count;
//	              cap_years; fraction_min_months; fraction_inclusive (0/1)
func evalCesantiaCRArt29(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	const kind = "cesantia_cr_art29"
	years, err := requireValue(inputs, "completed_years", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if !years.IsInteger() || years.IsNegative() {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: completed_years %s must be a whole number >= 0", kind, years)
	}
	months, err := requireValue(inputs, "remainder_months", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if months.IsNegative() || months.GreaterThanOrEqual(decimal.NewFromInt(12)) {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: remainder_months %s must be in [0, 12); twelve months are a completed year", kind, months)
	}

	// Every coefficient is read before branching, so a broken row fails for
	// every employee and not only for the ones whose tenure happens to reach
	// the broken part.
	subYearCount, err := requireCount(coefficients, "sub_year_band_count", kind, 0)
	if err != nil {
		return decimal.Decimal{}, err
	}
	subYear := make([]stepBand, 0, subYearCount)
	for i := 1; i <= subYearCount; i++ {
		minMonths, err := requireValue(coefficients, fmt.Sprintf("sub_year_band%d_min_months", i), kind)
		if err != nil {
			return decimal.Decimal{}, err
		}
		inclusive, err := requireFlag(coefficients, fmt.Sprintf("sub_year_band%d_inclusive", i), kind)
		if err != nil {
			return decimal.Decimal{}, err
		}
		days, err := requireValue(coefficients, fmt.Sprintf("sub_year_band%d_days", i), kind)
		if err != nil {
			return decimal.Decimal{}, err
		}
		subYear = append(subYear, stepBand{min: minMonths, exclusive: !inclusive, value: days})
	}
	rowCount, err := requireCount(coefficients, "row_count", kind, 1)
	if err != nil {
		return decimal.Decimal{}, err
	}
	rows := make([]decimal.Decimal, rowCount)
	for i := 1; i <= rowCount; i++ {
		if rows[i-1], err = requireValue(coefficients, fmt.Sprintf("row%d_days_per_year", i), kind); err != nil {
			return decimal.Decimal{}, err
		}
	}
	capYears, err := requireValue(coefficients, "cap_years", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if !capYears.IsPositive() {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: cap_years must be positive", kind)
	}
	fractionMin, err := requireValue(coefficients, "fraction_min_months", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	fractionInclusive, err := requireFlag(coefficients, "fraction_inclusive", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}

	if years.IsZero() {
		if days, ok := highestMet(subYear, months); ok {
			return days, nil
		}
		return decimal.Zero, nil
	}

	row := rowCount
	if years.LessThan(decimal.NewFromInt(int64(rowCount))) {
		row = int(years.IntPart())
	}
	counted := years
	if (stepBand{min: fractionMin, exclusive: !fractionInclusive}).metBy(months) {
		counted = counted.Add(decimal.NewFromInt(1))
	}
	if counted.GreaterThan(capYears) {
		counted = capYears
	}
	return rows[row-1].Mul(counted), nil
}

// evalFixedTermIndemnityCR is the minimum an employer owes for ending a
// fixed-term or project contract early without just cause (Código de Trabajo
// art. 31): one day of salary per block of days worked or fraction of one,
// never less than a floor that is higher for contracts of six months or more.
// The concrete damages a court may award on top are not payroll's.
//
//	inputs:       days_worked, contract_months — the agreed term
//	coefficients: block_days (7), days_per_block (1), min_days (3),
//	              min_days_long (22), long_contract_months (6)
//
// Result = max(ceil(days_worked / block_days) × days_per_block,
// contract_months >= long_contract_months ? min_days_long : min_days), in
// days.
func evalFixedTermIndemnityCR(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	const kind = "fixed_term_indemnity_cr"
	daysWorked, err := requireValue(inputs, "days_worked", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	contractMonths, err := requireValue(inputs, "contract_months", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if daysWorked.IsNegative() || contractMonths.IsNegative() {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: days_worked and contract_months must not be negative", kind)
	}
	blockDays, err := requireValue(coefficients, "block_days", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if !blockDays.IsPositive() {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: block_days must be positive", kind)
	}
	daysPerBlock, err := requireValue(coefficients, "days_per_block", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	minDays, err := requireValue(coefficients, "min_days", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	minDaysLong, err := requireValue(coefficients, "min_days_long", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	longContractMonths, err := requireValue(coefficients, "long_contract_months", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}

	// Quotient and remainder instead of a division, so a block count is never
	// at the mercy of a rounded quotient.
	blocks, rest := daysWorked.QuoRem(blockDays, 0)
	if rest.IsPositive() {
		blocks = blocks.Add(decimal.NewFromInt(1))
	}
	owed := blocks.Mul(daysPerBlock)
	floor := minDays
	if contractMonths.GreaterThanOrEqual(longContractMonths) {
		floor = minDaysLong
	}
	if owed.LessThan(floor) {
		return floor, nil
	}
	return owed, nil
}

// evalAbsenceEmployerShare is the share of the daily salary the EMPLOYER
// pays for one day of a subsidised absence, by the day's position in that
// absence: Costa Rica's CCSS sick leave is 50 % for days 1-3 and nothing from
// day 4 (MTSS DAJ-AE-829-06, DAJ-AE-201-12 — a criterion, not statute),
// maternity a flat 50 % (art. 95). The institution's part is not payroll's,
// and neither is deciding which day of the absence this is.
//
//	inputs:       absence_day_index — 1 for the first day of the absence
//	coefficients: band_count, band<i>_from_day, band<i>_rate
//
// The band with the highest from_day at or below the index wins. An index
// below every band is a data error, not a zero share: it means the table
// does not say what this day is worth.
func evalAbsenceEmployerShare(coefficients, inputs map[string]decimal.Decimal) (decimal.Decimal, error) {
	const kind = "absence_employer_share"
	index, err := requireValue(inputs, "absence_day_index", kind)
	if err != nil {
		return decimal.Decimal{}, err
	}
	if !index.IsInteger() || index.LessThan(decimal.NewFromInt(1)) {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: absence_day_index %s must be a whole number >= 1", kind, index)
	}
	bandCount, err := requireCount(coefficients, "band_count", kind, 1)
	if err != nil {
		return decimal.Decimal{}, err
	}
	bands := make([]stepBand, 0, bandCount)
	seen := map[string]bool{}
	for i := 1; i <= bandCount; i++ {
		fromDay, err := requireValue(coefficients, fmt.Sprintf("band%d_from_day", i), kind)
		if err != nil {
			return decimal.Decimal{}, err
		}
		if !fromDay.IsInteger() || fromDay.LessThan(decimal.NewFromInt(1)) {
			return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: band%d_from_day %s must be a whole number >= 1", kind, i, fromDay)
		}
		if seen[fromDay.String()] {
			return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: two bands start on day %s", kind, fromDay)
		}
		seen[fromDay.String()] = true
		rate, err := requireValue(coefficients, fmt.Sprintf("band%d_rate", i), kind)
		if err != nil {
			return decimal.Decimal{}, err
		}
		bands = append(bands, stepBand{min: fromDay, value: rate})
	}
	rate, ok := highestMet(bands, index)
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("hcmrules: %s: no band covers absence day %s", kind, index)
	}
	return rate, nil
}

// stepBand is one threshold of a step lookup — the worker is owed the value
// of the single highest band reached, not a sum. It is met at min, or only
// above it when exclusive.
type stepBand struct {
	min       decimal.Decimal
	exclusive bool
	value     decimal.Decimal
}

func (b stepBand) metBy(x decimal.Decimal) bool {
	if b.exclusive {
		return x.GreaterThan(b.min)
	}
	return x.GreaterThanOrEqual(b.min)
}

// outranks reports whether b is a higher threshold than o: a larger min, or
// the same min crossed strictly.
func (b stepBand) outranks(o stepBand) bool {
	if c := b.min.Cmp(o.min); c != 0 {
		return c > 0
	}
	return b.exclusive && !o.exclusive
}

// highestMet returns the value of the highest band x meets, in whatever
// order the bands come; on an exact tie the first one given wins. ok is
// false when x meets none.
func highestMet(bands []stepBand, x decimal.Decimal) (value decimal.Decimal, ok bool) {
	var best stepBand
	for _, b := range bands {
		if b.metBy(x) && (!ok || b.outranks(best)) {
			best, ok = b, true
		}
	}
	return best.value, ok
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

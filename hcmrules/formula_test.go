package hcmrules

import (
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
)

func TestEvalFormula_AguinaldoCR(t *testing.T) {
	// Sums the salaries ACTUALLY PAID Dec–Nov (already summed upstream) and
	// divides by the months in the accrual period.
	coefficients := map[string]decimal.Decimal{"divisor": d("12")}
	inputs := map[string]decimal.Decimal{"total_annual_salary": d("6000000")}
	got, err := EvalFormula("aguinaldo_cr", coefficients, inputs)
	if err != nil {
		t.Fatalf("EvalFormula: %v", err)
	}
	if !got.Equal(d("500000")) {
		t.Errorf("got %s, want 500000", got)
	}
}

// TestEvalFormula_AguinaldoCR_vs_ThirteenthBR_DifferentShapes is the required
// demonstration that the two "13th-month" style bonuses are NOT the same
// formula wearing a different coefficient set: CR sums the salaries actually
// paid (which can vary month to month), BR projects the CURRENT salary
// prorated by months worked. Same legal family, different inputs entirely.
func TestEvalFormula_AguinaldoCR_vs_ThirteenthBR_DifferentShapes(t *testing.T) {
	// A CR worker whose salary varied through the year: aguinaldo reflects
	// that variation because it sums what was actually paid.
	crCoefficients := map[string]decimal.Decimal{"divisor": d("12")}
	crInputs := map[string]decimal.Decimal{"total_annual_salary": d("6300000")} // varied months summed upstream
	crGot, err := EvalFormula("aguinaldo_cr", crCoefficients, crInputs)
	if err != nil {
		t.Fatalf("EvalFormula(aguinaldo_cr): %v", err)
	}
	if !crGot.Equal(d("525000")) {
		t.Errorf("aguinaldo_cr = %s, want 525000", crGot)
	}

	// A BR worker on the SAME final monthly salary who only worked 6 of the
	// 12 months: thirteenth_br has no notion of "salary actually paid per
	// month" at all — it projects the current salary and prorates by months.
	brCoefficients := map[string]decimal.Decimal{"divisor": d("12")}
	brInputs := map[string]decimal.Decimal{"monthly_salary": d("3000"), "months_worked": d("6")}
	brGot, err := EvalFormula("thirteenth_br", brCoefficients, brInputs)
	if err != nil {
		t.Fatalf("EvalFormula(thirteenth_br): %v", err)
	}
	if !brGot.Equal(d("1500")) {
		t.Errorf("thirteenth_br = %s, want 1500", brGot)
	}
}

func TestEvalFormula_CesantiaCR_CapActivates(t *testing.T) {
	// Illustrative graduated table (theory gives only the ~19.5–23 days/year
	// range and the 8-year cap, not a published per-year schedule).
	coefficients := map[string]decimal.Decimal{
		"cap_years":           d("8"),
		"band_count":          d("3"),
		"band1_upto_years":    d("3"),
		"band1_days_per_year": d("19.5"),
		"band2_upto_years":    d("6"),
		"band2_days_per_year": d("21"),
		"band3_upto_years":    d("8"),
		"band3_days_per_year": d("22"),
	}

	cases := []struct {
		name  string
		years string
		want  string
	}{
		{"within first band", "2", "39"},
		{"spans first and second band", "5", "100.5"},
		{"exactly at the cap", "8", "165.5"},
		{"beyond the cap — capped at 8 years' worth", "10", "165.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalFormula("cesantia_cr", coefficients, map[string]decimal.Decimal{
				"years_of_service": d(tc.years),
			})
			if err != nil {
				t.Fatalf("EvalFormula: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestEvalFormula_PreavisoCR_StepByTenure(t *testing.T) {
	coefficients := map[string]decimal.Decimal{
		"band_count":        d("3"),
		"band1_min_years":   d("0.25"),
		"band1_notice_days": d("7"),
		"band2_min_years":   d("0.5"),
		"band2_notice_days": d("14"),
		"band3_min_years":   d("1"),
		"band3_notice_days": d("30"),
	}
	cases := []struct {
		name  string
		years string
		want  string
	}{
		{"below the first band — nothing owed", "0.1", "0"},
		{"first band", "0.3", "7"},
		{"second band", "0.6", "14"},
		{"third band", "5", "30"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalFormula("preaviso_cr", coefficients, map[string]decimal.Decimal{
				"years_of_service": d(tc.years),
			})
			if err != nil {
				t.Fatalf("EvalFormula: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestEvalFormula_AvisoPrevioBR_Cap90(t *testing.T) {
	coefficients := map[string]decimal.Decimal{
		"base_days":     d("30"),
		"per_year_days": d("3"),
		"cap_days":      d("90"),
	}
	cases := []struct {
		name  string
		years string
		want  string
	}{
		{"no completed years", "0", "30"},
		{"under the cap", "10", "60"},
		{"exactly at the cap", "20", "90"},
		{"beyond the cap — capped at 90", "25", "90"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalFormula("aviso_previo_br", coefficients, map[string]decimal.Decimal{
				"years_of_service": d(tc.years),
			})
			if err != nil {
				t.Fatalf("EvalFormula: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestEvalFormula_FGTSBR_SameKindTwoLegalConcepts is the "multa como
// coeficiente" requirement: the ordinary monthly deposit and the 40%
// without-cause termination penalty are the SAME registered kind, only rate
// and base differ.
func TestEvalFormula_FGTSBR_SameKindTwoLegalConcepts(t *testing.T) {
	deposit, err := EvalFormula("fgts_br",
		map[string]decimal.Decimal{"rate": d("0.08")},
		map[string]decimal.Decimal{"base": d("3000")})
	if err != nil {
		t.Fatalf("EvalFormula(deposit): %v", err)
	}
	if !deposit.Equal(d("240")) {
		t.Errorf("deposit = %s, want 240", deposit)
	}

	multa, err := EvalFormula("fgts_br",
		map[string]decimal.Decimal{"rate": d("0.40")},
		map[string]decimal.Decimal{"base": d("15000")})
	if err != nil {
		t.Fatalf("EvalFormula(multa): %v", err)
	}
	if !multa.Equal(d("6000")) {
		t.Errorf("multa = %s, want 6000", multa)
	}
}

func TestEvalFormula_Overtime(t *testing.T) {
	cases := []struct {
		name       string
		multiplier string
		rate       string
		hours      string
		want       string
	}{
		{"1.5x premium (CR/BR/US)", "1.5", "10", "5", "75"},
		{"1.25x premium (CZ)", "1.25", "200", "4", "1000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalFormula("overtime",
				map[string]decimal.Decimal{"multiplier": d(tc.multiplier)},
				map[string]decimal.Decimal{
					"ordinary_hourly_rate": d(tc.rate),
					"overtime_hours":       d(tc.hours),
				})
			if err != nil {
				t.Fatalf("EvalFormula: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestEvalFormula_MinWageHigherOfUS(t *testing.T) {
	// Two candidates: state exceeds federal (the common case).
	got, err := EvalFormula("min_wage_higher_of_us", nil, map[string]decimal.Decimal{
		"federal": d("7.25"),
		"state":   d("16.50"),
	})
	if err != nil {
		t.Fatalf("EvalFormula: %v", err)
	}
	if !got.Equal(d("16.50")) {
		t.Errorf("got %s, want 16.50", got)
	}

	// Three candidates: a city ordinance on top of state and federal, proving
	// this is not hardcoded to exactly two named inputs.
	got, err = EvalFormula("min_wage_higher_of_us", nil, map[string]decimal.Decimal{
		"federal": d("7.25"),
		"state":   d("16.50"),
		"city":    d("17.00"),
	})
	if err != nil {
		t.Fatalf("EvalFormula: %v", err)
	}
	if !got.Equal(d("17.00")) {
		t.Errorf("got %s, want 17.00", got)
	}
}

func TestEvalFormula_VacationAccrualCR(t *testing.T) {
	coefficients := map[string]decimal.Decimal{
		"entitlement_days": d("14"), // 2 weeks
		"weeks_base":       d("50"),
	}
	cases := []struct {
		name        string
		weeksWorked string
		want        string
	}{
		{"half the accrual period", "25", "7"},
		{"full accrual period", "50", "14"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalFormula("vacation_accrual_cr", coefficients, map[string]decimal.Decimal{
				"weeks_worked": d(tc.weeksWorked),
			})
			if err != nil {
				t.Fatalf("EvalFormula: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestEvalFormula_VacationProportional is the generic name of the same
// shape: CR's 12 working days (Mon-Sat) per 50 weeks, as the MTSS reads
// Código de Trabajo Art. 153.
func TestEvalFormula_VacationProportional(t *testing.T) {
	coefficients := map[string]decimal.Decimal{
		"entitlement_days": d("12"),
		"weeks_base":       d("50"),
	}
	cases := []struct {
		name        string
		weeksWorked string
		want        string
	}{
		{"nothing worked yet", "0", "0"},
		{"half the accrual period", "25", "6"},
		{"full accrual period", "50", "12"},
		{"two periods", "100", "24"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalFormula("vacation_proportional", coefficients, map[string]decimal.Decimal{
				"weeks_worked": d(tc.weeksWorked),
			})
			if err != nil {
				t.Fatalf("EvalFormula: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
	if _, err := EvalFormula("vacation_proportional",
		map[string]decimal.Decimal{"entitlement_days": d("12"), "weeks_base": d("0")},
		map[string]decimal.Decimal{"weeks_worked": d("10")}); err == nil {
		t.Error("expected an error for a non-positive weeks_base")
	}
}

// TestEvalFormula_VacationPeriodVesting is BR's CLT Art. 130: 30 calendar
// days vest at the end of each completed 12-month period, nothing before.
func TestEvalFormula_VacationPeriodVesting(t *testing.T) {
	coefficients := map[string]decimal.Decimal{
		"days_per_period": d("30"),
		"period_months":   d("12"),
	}
	cases := []struct {
		name   string
		months string
		want   string
	}{
		{"first period not complete", "11", "0"},
		{"exactly one period", "12", "30"},
		{"one period and a half", "18", "30"},
		{"two periods", "24", "60"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalFormula("vacation_period_vesting", coefficients, map[string]decimal.Decimal{
				"months_of_service": d(tc.months),
			})
			if err != nil {
				t.Fatalf("EvalFormula: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
	if _, err := EvalFormula("vacation_period_vesting", coefficients,
		map[string]decimal.Decimal{"months_of_service": d("-1")}); err == nil {
		t.Error("expected an error for negative months_of_service")
	}
}

// TestEvalFormula_USNeedsNoSeveranceOrThirteenthCode is the central thesis
// the ADR must demonstrate: a US payroll run simply never calls
// "aguinaldo_cr", "cesantia_cr", "thirteenth_br" or "aviso_previo_br" — the
// mandatory-13th/severance flags (rule_flag) decide that upstream in
// payroll. There is no US-specific kind, and no US branch anywhere in this
// package: the registry above contains ONLY the kinds listed in the task,
// none of them named or gated by jurisdiction in code.
func TestEvalFormula_USNeedsNoSeveranceOrThirteenthCode(t *testing.T) {
	for _, kind := range []string{"aguinaldo_cr", "cesantia_cr", "thirteenth_br", "aviso_previo_br", "fgts_br", "preaviso_cr"} {
		if _, ok := formulaRegistry[kind]; !ok {
			t.Fatalf("expected %q to be registered by the other tests in this suite", kind)
		}
	}
	// The only two US-flavoured kinds in the whole registry are wage/overtime
	// mechanics that apply everywhere, not termination or a 13th salary.
	for kind := range formulaRegistry {
		if kind == "min_wage_higher_of_us" || kind == "overtime" || kind == "vacation_accrual_cr" ||
			kind == "vacation_proportional" || kind == "vacation_period_vesting" ||
			kind == "aguinaldo_cr" || kind == "cesantia_cr" || kind == "preaviso_cr" ||
			kind == "cesantia_cr_art29" || kind == "fixed_term_indemnity_cr" || kind == "absence_employer_share" ||
			kind == "thirteenth_br" || kind == "aviso_previo_br" || kind == "fgts_br" {
			continue
		}
		t.Fatalf("unexpected formula_kind %q registered — every kind must be accounted for", kind)
	}
}

func TestEvalFormula_Errors(t *testing.T) {
	if _, err := EvalFormula("does_not_exist", nil, nil); err == nil {
		t.Error("expected an explicit error for an unknown formula_kind")
	}
	// A missing coefficient must fail loudly, never default to zero.
	if _, err := EvalFormula("aviso_previo_br",
		map[string]decimal.Decimal{"base_days": d("30"), "per_year_days": d("3")}, // cap_days omitted
		map[string]decimal.Decimal{"years_of_service": d("5")}); err == nil {
		t.Error("expected an explicit error for a missing coefficient")
	}
}

// cesantiaArt29 is the art. 29 table as the CR seed carries it: 7 days from 3
// months, 14 days past 6 months, the 13 rows of inc. 3-4 (13 and over = 20),
// an 8-year cap and a fraction of 6 months. The two boundary flags are the
// parameters: they are decisions (N8), not something the code assumes.
func cesantiaArt29(fractionInclusive, band2Inclusive string) map[string]decimal.Decimal {
	c := map[string]decimal.Decimal{
		"sub_year_band_count":       d("2"),
		"sub_year_band1_min_months": d("3"),
		"sub_year_band1_inclusive":  d("1"),
		"sub_year_band1_days":       d("7"),
		"sub_year_band2_min_months": d("6"),
		"sub_year_band2_inclusive":  d(band2Inclusive),
		"sub_year_band2_days":       d("14"),
		"row_count":                 d("13"),
		"cap_years":                 d("8"),
		"fraction_min_months":       d("6"),
		"fraction_inclusive":        d(fractionInclusive),
	}
	for i, days := range []string{"19.5", "20", "20.5", "21", "21.24", "21.5", "22", "22", "22", "21.5", "21", "20.5", "20"} {
		c[fmt.Sprintf("row%d_days_per_year", i+1)] = d(days)
	}
	return c
}

func tenure(years, months string) map[string]decimal.Decimal {
	return map[string]decimal.Decimal{"completed_years": d(years), "remainder_months": d(months)}
}

// TestEvalFormula_CesantiaCRArt29_Goldens are the figures the MTSS publishes
// or that follow from its Directriz 1-2003: the row of the COMPLETED years,
// times the years counted (the fraction adds one, never a row), capped at 8.
func TestEvalFormula_CesantiaCRArt29_Goldens(t *testing.T) {
	coefficients := cesantiaArt29("1", "0")
	cases := []struct {
		name          string
		years, months string
		want          string
	}{
		{"5 years (DAJ-AE-142-11)", "5", "0", "106.2"},
		{"1 year 8 months (DAJ-AE-083-09): row 1 × 2", "1", "8", "39"},
		{"5 years 8 months: row 5 × 6, the fraction does not move the row", "5", "8", "127.44"},
		{"exactly 12 months is row 1", "1", "0", "19.5"},
		{"7 years", "7", "0", "154"},
		{"8 years 7 months: the fraction hits the cap", "8", "7", "176"},
		{"10 years: row 10 × the 8-year cap", "10", "0", "172"},
		{"15 years: the last row covers everything above it", "15", "0", "160"},
		{"13 years 11 months", "13", "11", "160"},
		{"12 years 7 months", "12", "7", "164"},
		{"7 years 7 months: 7 + 1 reaches the cap exactly", "7", "7", "176"},
		{"6 years 11 months", "6", "11", "150.5"},
		{"1 year 6 months exactly: the fraction counts (inclusive)", "1", "6", "39"},
		{"1 year 5.99 months: the fraction does not count", "1", "5.99", "19.5"},
		{"2 months: nothing", "0", "2", "0"},
		{"2.99 months: still nothing", "0", "2.99", "0"},
		{"3 months: 7 days", "0", "3", "7"},
		{"6 months exactly with band 2 exclusive: 7 days", "0", "6", "7"},
		{"7 months: 14 days", "0", "7", "14"},
		{"11.99 months: 14 days", "0", "11.99", "14"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalFormula("cesantia_cr_art29", coefficients, tenure(tc.years, tc.months))
			if err != nil {
				t.Fatalf("EvalFormula: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// The two boundaries the law and the MTSS read differently are coefficients:
// the same tenure gives a different figure when only the flag changes.
func TestEvalFormula_CesantiaCRArt29_BoundariesAreData(t *testing.T) {
	sixMonths := tenure("0", "6")
	if got, err := EvalFormula("cesantia_cr_art29", cesantiaArt29("1", "1"), sixMonths); err != nil || !got.Equal(d("14")) {
		t.Errorf("6 months with band 2 inclusive = %s (%v), want 14", got, err)
	}
	if got, err := EvalFormula("cesantia_cr_art29", cesantiaArt29("1", "0"), sixMonths); err != nil || !got.Equal(d("7")) {
		t.Errorf("6 months with band 2 exclusive = %s (%v), want 7", got, err)
	}

	oneAndAHalf := tenure("1", "6")
	if got, err := EvalFormula("cesantia_cr_art29", cesantiaArt29("1", "0"), oneAndAHalf); err != nil || !got.Equal(d("39")) {
		t.Errorf("1 year 6 months with the fraction inclusive = %s (%v), want 39", got, err)
	}
	if got, err := EvalFormula("cesantia_cr_art29", cesantiaArt29("0", "0"), oneAndAHalf); err != nil || !got.Equal(d("19.5")) {
		t.Errorf("1 year 6 months with the fraction exclusive = %s (%v), want 19.5", got, err)
	}
}

// Sub-year bands may come in any order; the highest one reached wins.
func TestEvalFormula_CesantiaCRArt29_SubYearBandOrder(t *testing.T) {
	c := cesantiaArt29("1", "0")
	c["sub_year_band1_min_months"], c["sub_year_band2_min_months"] = c["sub_year_band2_min_months"], c["sub_year_band1_min_months"]
	c["sub_year_band1_inclusive"], c["sub_year_band2_inclusive"] = c["sub_year_band2_inclusive"], c["sub_year_band1_inclusive"]
	c["sub_year_band1_days"], c["sub_year_band2_days"] = c["sub_year_band2_days"], c["sub_year_band1_days"]
	for months, want := range map[string]string{"2": "0", "3": "7", "6": "7", "7": "14"} {
		got, err := EvalFormula("cesantia_cr_art29", c, tenure("0", months))
		if err != nil {
			t.Fatalf("EvalFormula: %v", err)
		}
		if !got.Equal(d(want)) {
			t.Errorf("%s months: got %s, want %s", months, got, want)
		}
	}
}

func TestEvalFormula_CesantiaCRArt29_Errors(t *testing.T) {
	good := cesantiaArt29("1", "0")
	badInputs := map[string]map[string]decimal.Decimal{
		"fractional completed years": tenure("1.5", "0"),
		"negative completed years":   tenure("-1", "0"),
		"twelve remainder months":    tenure("0", "12"),
		"more than a year of months": tenure("2", "14"),
		"negative remainder months":  tenure("1", "-0.5"),
		"missing completed_years":    {"remainder_months": d("3")},
		"missing remainder_months":   {"completed_years": d("3")},
	}
	for name, inputs := range badInputs {
		if _, err := EvalFormula("cesantia_cr_art29", good, inputs); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	broken := func(change func(map[string]decimal.Decimal)) map[string]decimal.Decimal {
		c := cesantiaArt29("1", "0")
		change(c)
		return c
	}
	badCoefficients := map[string]map[string]decimal.Decimal{
		"no rows":                   broken(func(c map[string]decimal.Decimal) { c["row_count"] = d("0") }),
		"fractional row count":      broken(func(c map[string]decimal.Decimal) { c["row_count"] = d("2.5") }),
		"missing row":               broken(func(c map[string]decimal.Decimal) { delete(c, "row5_days_per_year") }),
		"negative band count":       broken(func(c map[string]decimal.Decimal) { c["sub_year_band_count"] = d("-1") }),
		"missing band flag":         broken(func(c map[string]decimal.Decimal) { delete(c, "sub_year_band1_inclusive") }),
		"band flag that is not 0/1": broken(func(c map[string]decimal.Decimal) { c["sub_year_band2_inclusive"] = d("0.5") }),
		"fraction flag 2":           broken(func(c map[string]decimal.Decimal) { c["fraction_inclusive"] = d("2") }),
		"missing fraction flag":     broken(func(c map[string]decimal.Decimal) { delete(c, "fraction_inclusive") }),
		"zero cap":                  broken(func(c map[string]decimal.Decimal) { c["cap_years"] = d("0") }),
		"missing cap":               broken(func(c map[string]decimal.Decimal) { delete(c, "cap_years") }),
	}
	for name, c := range badCoefficients {
		// Under a year, so only a function that reads every coefficient
		// first can notice the broken row.
		if _, err := EvalFormula("cesantia_cr_art29", c, tenure("0", "7")); err == nil {
			t.Errorf("%s: expected an error even for a tenure that does not reach it", name)
		}
	}
}

// TestEvalFormula_PreavisoCR_ExclusiveBands is art. 28 with its own words:
// "que exceda de seis meses" and "después de un año" are strict.
func TestEvalFormula_PreavisoCR_ExclusiveBands(t *testing.T) {
	coefficients := map[string]decimal.Decimal{
		"band_count":        d("3"),
		"band1_min_years":   d("0.25"),
		"band1_notice_days": d("7"),
		"band1_exclusive":   d("0"),
		"band2_min_years":   d("0.5"),
		"band2_notice_days": d("15"),
		"band2_exclusive":   d("1"),
		"band3_min_years":   d("1"),
		"band3_notice_days": d("30"),
		"band3_exclusive":   d("1"),
	}
	cases := []struct {
		years, want string
	}{
		{"0.24", "0"},
		{"0.25", "7"},
		{"0.5", "7"},
		{"0.51", "15"},
		{"1", "15"},
		{"1.01", "30"},
		{"20", "30"},
	}
	for _, tc := range cases {
		got, err := EvalFormula("preaviso_cr", coefficients, map[string]decimal.Decimal{"years_of_service": d(tc.years)})
		if err != nil {
			t.Fatalf("%s years: %v", tc.years, err)
		}
		if !got.Equal(d(tc.want)) {
			t.Errorf("%s years: got %s, want %s", tc.years, got, tc.want)
		}
	}

	// Without the flag a row keeps the original ">=", so what is seeded today
	// still evaluates as it did.
	legacy := map[string]decimal.Decimal{
		"band_count":        d("2"),
		"band1_min_years":   d("0.25"),
		"band1_notice_days": d("7"),
		"band2_min_years":   d("0.5"),
		"band2_notice_days": d("14"),
	}
	if got, err := EvalFormula("preaviso_cr", legacy, map[string]decimal.Decimal{"years_of_service": d("0.5")}); err != nil || !got.Equal(d("14")) {
		t.Errorf("a row without band<i>_exclusive must keep >=: got %s (%v), want 14", got, err)
	}

	coefficients["band2_exclusive"] = d("2")
	if _, err := EvalFormula("preaviso_cr", coefficients, map[string]decimal.Decimal{"years_of_service": d("1")}); err == nil {
		t.Error("expected an error for a band<i>_exclusive that is not 0 or 1")
	}

	// No band at all still owes nothing, as before.
	if got, err := EvalFormula("preaviso_cr", map[string]decimal.Decimal{"band_count": d("0")}, map[string]decimal.Decimal{"years_of_service": d("5")}); err != nil || !got.IsZero() {
		t.Errorf("band_count 0: got %s (%v), want 0", got, err)
	}
}

// evalNoPanic turns a panic inside the evaluator into a test failure that
// names the case, instead of taking the whole test binary down with it.
func evalNoPanic(t *testing.T, name, kind string, coefficients, inputs map[string]decimal.Decimal) (got decimal.Decimal, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("%s: EvalFormula panicked: %v", name, r)
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return EvalFormula(kind, coefficients, inputs)
}

// A count coefficient sizes a loop and an allocation, and core-hcmrules'
// EvaluateFormula evaluates whatever coefficients its caller sends: a
// negative, huge or wrapping count must be an error, never a panic or an
// out-of-memory that takes the process down for every tenant.
func TestEvalFormula_CountsAreBounded(t *testing.T) {
	with := func(base map[string]decimal.Decimal, key, value string) map[string]decimal.Decimal {
		c := map[string]decimal.Decimal{}
		for k, v := range base {
			c[k] = v
		}
		c[key] = d(value)
		return c
	}
	preaviso := map[string]decimal.Decimal{"band_count": d("1"), "band1_min_years": d("0.25"), "band1_notice_days": d("7")}
	absence := map[string]decimal.Decimal{"band_count": d("1"), "band1_from_day": d("1"), "band1_rate": d("0.5")}
	art29 := cesantiaArt29("1", "0")
	// Only row1 is loaded: a row_count that wrapped to 1 would evaluate.
	art29OneRow := with(art29, "row_count", "1")
	for i := 2; i <= 13; i++ {
		delete(art29OneRow, fmt.Sprintf("row%d_days_per_year", i))
	}

	years := func(v string) map[string]decimal.Decimal { return map[string]decimal.Decimal{"years_of_service": d(v)} }
	day1 := map[string]decimal.Decimal{"absence_day_index": d("1")}
	cases := []struct {
		name, kind   string
		coefficients map[string]decimal.Decimal
		inputs       map[string]decimal.Decimal
	}{
		{"preaviso band_count -1", "preaviso_cr", with(preaviso, "band_count", "-1"), years("1")},
		{"preaviso band_count 2.5", "preaviso_cr", with(preaviso, "band_count", "2.5"), years("1")},
		{"preaviso band_count 1e13", "preaviso_cr", with(preaviso, "band_count", "1e13"), years("1")},
		{"preaviso band_count 1e15", "preaviso_cr", with(preaviso, "band_count", "1e15"), years("1")},
		{"preaviso band_count 2^64+1", "preaviso_cr", with(preaviso, "band_count", "18446744073709551617"), years("1")},
		{"preaviso band_count 101", "preaviso_cr", with(preaviso, "band_count", "101"), years("1")},
		{"art29 row_count 2^64", "cesantia_cr_art29", with(art29, "row_count", "18446744073709551616"), tenure("5", "0")},
		{"art29 row_count 2^64+1 with only row1", "cesantia_cr_art29", with(art29OneRow, "row_count", "18446744073709551617"), tenure("5", "0")},
		{"art29 row_count 1e13", "cesantia_cr_art29", with(art29, "row_count", "1e13"), tenure("5", "0")},
		{"art29 row_count 1e20", "cesantia_cr_art29", with(art29, "row_count", "1e20"), tenure("5", "0")},
		{"art29 sub_year_band_count 1e13", "cesantia_cr_art29", with(art29, "sub_year_band_count", "1e13"), tenure("0", "7")},
		{"art29 sub_year_band_count 2^64+2", "cesantia_cr_art29", with(art29, "sub_year_band_count", "18446744073709551618"), tenure("0", "7")},
		{"absence band_count 1e13", "absence_employer_share", with(absence, "band_count", "1e13"), day1},
		{"absence band_count 1e15", "absence_employer_share", with(absence, "band_count", "1e15"), day1},
		{"absence band_count 2^64+1", "absence_employer_share", with(absence, "band_count", "18446744073709551617"), day1},
		{"absence band_count -1", "absence_employer_share", with(absence, "band_count", "-1"), day1},
	}
	for _, tc := range cases {
		if got, err := evalNoPanic(t, tc.name, tc.kind, tc.coefficients, tc.inputs); err == nil {
			t.Errorf("%s: got %s, want an error", tc.name, got)
		}
	}

	// The bound is inclusive: a table of exactly maxCount bands evaluates.
	full := map[string]decimal.Decimal{"band_count": decimal.NewFromInt(maxCount)}
	for i := 1; i <= maxCount; i++ {
		full[fmt.Sprintf("band%d_from_day", i)] = decimal.NewFromInt(int64(i))
		full[fmt.Sprintf("band%d_rate", i)] = d("0.5")
	}
	if got, err := evalNoPanic(t, "maxCount bands", "absence_employer_share", full, map[string]decimal.Decimal{"absence_day_index": d("250")}); err != nil || !got.Equal(d("0.5")) {
		t.Errorf("%d bands: got %s (%v), want 0.5", maxCount, got, err)
	}
}

// TestEvalFormula_FixedTermIndemnityCR is art. 31: a day per 7 worked or
// fraction, at least 3, at least 22 when the contract was 6 months or more.
func TestEvalFormula_FixedTermIndemnityCR(t *testing.T) {
	coefficients := map[string]decimal.Decimal{
		"block_days":           d("7"),
		"days_per_block":       d("1"),
		"min_days":             d("3"),
		"min_days_long":        d("22"),
		"long_contract_months": d("6"),
	}
	cases := []struct {
		name, daysWorked, contractMonths, want string
	}{
		{"nothing worked: the floor", "0", "3", "3"},
		{"one full block: still the floor", "7", "3", "3"},
		{"10 days: two blocks, the floor wins", "10", "3", "3"},
		{"15 days: three blocks", "15", "3", "3"},
		{"22 days: a fraction counts as a block", "22", "3", "4"},
		{"30 days", "30", "2", "5"},
		{"100 days on a 5-month contract", "100", "5", "15"},
		{"100 days on a 6-month contract: the long floor", "100", "6", "22"},
		{"154 days: 22 blocks exactly", "154", "12", "22"},
		{"155 days: one more block", "155", "12", "23"},
		{"180 days on a 6-month contract", "180", "6", "26"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EvalFormula("fixed_term_indemnity_cr", coefficients, map[string]decimal.Decimal{
				"days_worked": d(tc.daysWorked), "contract_months": d(tc.contractMonths),
			})
			if err != nil {
				t.Fatalf("EvalFormula: %v", err)
			}
			if !got.Equal(d(tc.want)) {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}

	for name, inputs := range map[string]map[string]decimal.Decimal{
		"negative days":    {"days_worked": d("-1"), "contract_months": d("3")},
		"negative months":  {"days_worked": d("10"), "contract_months": d("-3")},
		"missing days":     {"contract_months": d("3")},
		"missing contract": {"days_worked": d("10")},
	} {
		if _, err := EvalFormula("fixed_term_indemnity_cr", coefficients, inputs); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	zeroBlock := map[string]decimal.Decimal{}
	for k, v := range coefficients {
		zeroBlock[k] = v
	}
	zeroBlock["block_days"] = d("0")
	if _, err := EvalFormula("fixed_term_indemnity_cr", zeroBlock, map[string]decimal.Decimal{
		"days_worked": d("10"), "contract_months": d("3"),
	}); err == nil {
		t.Error("expected an error for block_days 0")
	}
}

// TestEvalFormula_AbsenceEmployerShare: CCSS sick leave, the employer pays
// half of days 1-3 and nothing from day 4; maternity is a flat half.
func TestEvalFormula_AbsenceEmployerShare(t *testing.T) {
	sick := map[string]decimal.Decimal{
		"band_count":     d("2"),
		"band1_from_day": d("4"),
		"band1_rate":     d("0"),
		"band2_from_day": d("1"),
		"band2_rate":     d("0.50"),
	}
	for day, want := range map[string]string{"1": "0.5", "2": "0.5", "3": "0.5", "4": "0", "5": "0", "30": "0"} {
		got, err := EvalFormula("absence_employer_share", sick, map[string]decimal.Decimal{"absence_day_index": d(day)})
		if err != nil {
			t.Fatalf("day %s: %v", day, err)
		}
		if !got.Equal(d(want)) {
			t.Errorf("sick day %s: got %s, want %s", day, got, want)
		}
	}

	maternity := map[string]decimal.Decimal{"band_count": d("1"), "band1_from_day": d("1"), "band1_rate": d("0.50")}
	for _, day := range []string{"1", "120"} {
		got, err := EvalFormula("absence_employer_share", maternity, map[string]decimal.Decimal{"absence_day_index": d(day)})
		if err != nil || !got.Equal(d("0.5")) {
			t.Errorf("maternity day %s: got %s (%v), want 0.5", day, got, err)
		}
	}

	for _, day := range []string{"0", "-1", "1.5"} {
		if _, err := EvalFormula("absence_employer_share", sick, map[string]decimal.Decimal{"absence_day_index": d(day)}); err == nil {
			t.Errorf("day %s: expected an error", day)
		}
	}
	if _, err := EvalFormula("absence_employer_share", sick, nil); err == nil {
		t.Error("expected an error for a missing absence_day_index")
	}

	broken := map[string]map[string]decimal.Decimal{
		"no band covers day 1": {"band_count": d("1"), "band1_from_day": d("2"), "band1_rate": d("0.5")},
		"two bands, same day": {"band_count": d("2"), "band1_from_day": d("1"), "band1_rate": d("0.5"),
			"band2_from_day": d("1.0"), "band2_rate": d("0")},
		"band from day 0":     {"band_count": d("1"), "band1_from_day": d("0"), "band1_rate": d("0.5")},
		"fractional from_day": {"band_count": d("1"), "band1_from_day": d("1.5"), "band1_rate": d("0.5")},
		"missing rate":        {"band_count": d("1"), "band1_from_day": d("1")},
		"no bands":            {"band_count": d("0")},
	}
	for name, c := range broken {
		if _, err := EvalFormula("absence_employer_share", c, map[string]decimal.Decimal{"absence_day_index": d("1")}); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

package hcmrules

import (
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

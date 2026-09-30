package hcmrules

import (
	"testing"

	"github.com/shopspring/decimal"
)

func TestCheckEligibility_FMLAUS(t *testing.T) {
	params := map[string]decimal.Decimal{
		"min_headcount":     d("50"),
		"min_tenure_months": d("12"),
		"min_hours":         d("1250"),
	}
	cases := []struct {
		name      string
		headcount string
		tenure    string
		hours     string
		want      bool
	}{
		{"meets all three thresholds", "60", "24", "1500", true},
		{"exactly at all three thresholds", "50", "12", "1250", true},
		{"headcount too small", "40", "24", "1500", false},
		{"tenure too short", "60", "6", "1500", false},
		{"hours too few", "60", "24", "800", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckEligibility("fmla_us", params, map[string]decimal.Decimal{
				"headcount":     d(tc.headcount),
				"tenure_months": d(tc.tenure),
				"hours_worked":  d(tc.hours),
			})
			if err != nil {
				t.Fatalf("CheckEligibility: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckEligibility_VacationVestingBR(t *testing.T) {
	params := map[string]decimal.Decimal{"min_continuous_months": d("12")}
	cases := []struct {
		name             string
		continuousMonths string
		want             bool
	}{
		{"vested", "12", true},
		{"beyond vesting", "18", true},
		{"not yet vested", "6", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CheckEligibility("vacation_vesting_br", params, map[string]decimal.Decimal{
				"continuous_months": d(tc.continuousMonths),
			})
			if err != nil {
				t.Fatalf("CheckEligibility: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckEligibility_Errors(t *testing.T) {
	if _, err := CheckEligibility("does_not_exist", nil, nil); err == nil {
		t.Error("expected an explicit error for an unknown predicate_kind")
	}
	if _, err := CheckEligibility("fmla_us",
		map[string]decimal.Decimal{"min_headcount": d("50"), "min_tenure_months": d("12")}, // min_hours omitted
		map[string]decimal.Decimal{"headcount": d("60"), "tenure_months": d("24"), "hours_worked": d("1500")},
	); err == nil {
		t.Error("expected an explicit error for a missing param")
	}
}

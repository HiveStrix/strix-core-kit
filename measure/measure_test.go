package measure

import (
	"errors"
	"testing"

	"github.com/shopspring/decimal"
)

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

var (
	unitU    = Unit{Code: "u", Dimension: DimensionCount, FactorToBase: one}
	unitG    = Unit{Code: "g", Dimension: DimensionMass, FactorToBase: one}
	unitKg   = Unit{Code: "kg", Dimension: DimensionMass, FactorToBase: d("1000")}
	unitMl   = Unit{Code: "ml", Dimension: DimensionVolume, FactorToBase: one}
	unitL    = Unit{Code: "l", Dimension: DimensionVolume, FactorToBase: d("1000")}
	unitSack = Unit{Code: "saco", Dimension: DimensionContainer, FactorToBase: one}
)

func TestValidateUnit(t *testing.T) {
	// A container's factor is meaningless: it is forced to 1, not rejected.
	got, err := ValidateUnit(Unit{Code: "caja", Dimension: DimensionContainer, FactorToBase: d("12")})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got.FactorToBase.Equal(one) {
		t.Errorf("el factor de un envase debe ser 1, fue %s", got.FactorToBase)
	}

	good := Unit{Code: "lb", Dimension: DimensionMass, FactorToBase: d("453.59237")}
	if _, err := ValidateUnit(good); err != nil {
		t.Fatalf("unidad válida: %v", err)
	}

	bad := []Unit{
		{Code: "kg", Dimension: "weight", FactorToBase: one},
		{Code: "kg", Dimension: DimensionMass, FactorToBase: decimal.Zero},
		{Code: "kg", Dimension: DimensionMass, FactorToBase: d("-1")},
		{Code: "kg", Dimension: DimensionMass, FactorToBase: d("0.0000000000001")}, // más de 12 decimales
	}
	for _, u := range bad {
		if _, err := ValidateUnit(u); !errors.Is(err, ErrInvalidUnit) {
			t.Errorf("ValidateUnit(%+v) = %v, esperaba ErrInvalidUnit", u, err)
		}
	}
}

func TestConvert(t *testing.T) {
	cases := []struct {
		name     string
		qty      decimal.Decimal
		from, to Unit
		want     decimal.Decimal
	}{
		{"kg a g", d("2.5"), unitKg, unitG, d("2500")},
		{"g a kg", d("250"), unitG, unitKg, d("0.25")},
		{"l a ml", d("0.75"), unitL, unitMl, d("750")},
		{"misma unidad", d("3"), unitKg, unitKg, d("3")},
		{"conteo a conteo", d("5"), unitU, unitU, d("5")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Convert(c.qty, c.from, c.to)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if !got.Equal(c.want) {
				t.Fatalf("got %s, esperaba %s", got, c.want)
			}
		})
	}

	// Un tercio exacto: redondeado a seis decimales es 0.333333.
	third := Unit{Code: "x3", Dimension: DimensionMass, FactorToBase: d("3")}
	got, err := Convert(one, unitG, third)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got.Round(6).Equal(d("0.333333")) {
		t.Errorf("1 g a x3 = %s, esperaba ~0.333333", got)
	}

	if _, err := Convert(one, unitKg, unitL); !errors.Is(err, ErrIncompatibleDimensions) {
		t.Errorf("kg -> l: %v, esperaba ErrIncompatibleDimensions", err)
	}
	if _, err := Convert(one, unitSack, unitG); !errors.Is(err, ErrContainerDoesNotConvert) {
		t.Errorf("saco -> g: %v, esperaba ErrContainerDoesNotConvert", err)
	}
}

func TestValidatePresentation(t *testing.T) {
	// A physical unit derives its content from the factors: a kilo is 1000 g.
	p, err := ValidatePresentation(Presentation{Name: " Kilo ", PurchaseUnit: unitKg, ContentUnit: unitG}, unitG)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if p.Name != "Kilo" || !p.ContentQuantity.Equal(d("1000")) {
		t.Errorf("got %+v", p)
	}

	// A container declares the placeholder 1: the invoice decides.
	p, err = ValidatePresentation(Presentation{Name: "Saco", PurchaseUnit: unitSack, ContentUnit: unitG}, unitG)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !p.ContentQuantity.Equal(one) {
		t.Errorf("content = %s, esperaba el marcador 1", p.ContentQuantity)
	}

	// A container with a real declared content keeps it.
	p, err = ValidatePresentation(Presentation{Name: "Saco de 50 kg", PurchaseUnit: unitSack, ContentUnit: unitG, ContentQuantity: d("50000")}, unitG)
	if err != nil || !p.ContentQuantity.Equal(d("50000")) {
		t.Errorf("got %+v err %v", p, err)
	}

	bad := []struct {
		name        string
		p           Presentation
		consumption Unit
		want        error
	}{
		{"sin nombre", Presentation{PurchaseUnit: unitKg, ContentUnit: unitG}, unitG, ErrInvalidPresentation},
		{"contenido en envase", Presentation{Name: "x", PurchaseUnit: unitSack, ContentUnit: unitSack}, unitG, ErrIncompatibleDimensions},
		{"contenido en otra dimensión", Presentation{Name: "x", PurchaseUnit: unitSack, ContentUnit: unitMl}, unitG, ErrIncompatibleDimensions},
		{"compra física en otra dimensión", Presentation{Name: "x", PurchaseUnit: unitL, ContentUnit: unitG}, unitG, ErrIncompatibleDimensions},
		{"contenido negativo", Presentation{Name: "x", PurchaseUnit: unitKg, ContentUnit: unitG, ContentQuantity: d("-5")}, unitG, ErrInvalidPresentation},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ValidatePresentation(c.p, c.consumption); !errors.Is(err, c.want) {
				t.Fatalf("err = %v, esperaba %v", err, c.want)
			}
		})
	}
}

func TestToConsumptionUnit(t *testing.T) {
	sack := Presentation{Name: "Saco", PurchaseUnit: unitSack, ContentUnit: unitKg, ContentQuantity: one}
	kilo := Presentation{Name: "Kilo", PurchaseUnit: unitKg, ContentUnit: unitG, ContentQuantity: d("1000")}

	// 3 sacks of 50 kg each, item consumed in grams.
	got, err := ToConsumptionUnit(d("3"), sack, d("50"), unitG)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got.Equal(d("150000")) {
		t.Errorf("got %s, esperaba 150000", got)
	}

	// 2 kilos, item consumed in grams.
	got, err = ToConsumptionUnit(d("2"), kilo, decimal.Zero, unitG)
	if err != nil || !got.Equal(d("2000")) {
		t.Errorf("got %s err %v", got, err)
	}

	// The invoice content wins over a declared one.
	declared := Presentation{Name: "Saco de 50 kg", PurchaseUnit: unitSack, ContentUnit: unitG, ContentQuantity: d("50000")}
	got, err = ToConsumptionUnit(one, declared, d("45000"), unitKg)
	if err != nil || !got.Equal(d("45")) {
		t.Errorf("got %s err %v", got, err)
	}
	// ...and the declared one serves when the invoice brings none.
	got, err = ToConsumptionUnit(one, declared, decimal.Zero, unitKg)
	if err != nil || !got.Equal(d("50")) {
		t.Errorf("got %s err %v", got, err)
	}

	// A container with only the placeholder and no invoice content is a hole.
	if _, err := ToConsumptionUnit(d("3"), sack, decimal.Zero, unitG); !errors.Is(err, ErrContainerNeedsContent) {
		t.Errorf("err = %v, esperaba ErrContainerNeedsContent", err)
	}
}

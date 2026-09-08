// Package measure is the shared unit-of-measure conversion used by the cores
// that value quantities (inventory, expenses).
//
// EVERY FUNCTION HERE IS PURE: no database, no transport, no clock. It works on
// shopspring/decimal, the kit's decimal type, and unifies two copies that had
// drifted (core-inventory and core-expenses each had their own). The rules come
// from a production incident in expenses: a presentation that claimed to be
// bought in KILOS with a content of 10 GRAMS — a value that means nothing and
// that nobody could catch by eye.
//
//   - A unit belongs to one dimension and converts only inside it, through its
//     factor to the dimension's base unit.
//   - The CONTAINER dimension is abstract and does not convert. Box, sack and
//     bottle group a quantity that depends on the item (a box of carrots holds
//     kilograms, a box of cups holds units), so its factor is always 1 and the
//     real content is captured on each purchase line.
//
// Convention for "not provided": a zero decimal means "no value given" (derive
// or reject as the case demands). A core whose input type distinguishes empty
// from an explicit zero enforces that distinction at its own boundary, before
// calling in.
package measure

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"

	"github.com/hs-javierviquez/strix-core-kit/decimals"
)

// The four dimensions. Closed platform vocabulary: a tenant combines units, it
// does not invent dimensions.
const (
	DimensionMass      = "mass"
	DimensionVolume    = "volume"
	DimensionCount     = "count"
	DimensionContainer = "container"
)

// Dimensions is the closed vocabulary.
var Dimensions = []string{DimensionMass, DimensionVolume, DimensionCount, DimensionContainer}

var (
	// ErrInvalidUnit marks a unit whose dimension or factor is not usable.
	ErrInvalidUnit = errors.New("UNIDAD_DE_MEDIDA_INVALIDA")
	// ErrIncompatibleDimensions: conversion is only defined inside one dimension.
	ErrIncompatibleDimensions = errors.New("DIMENSIONES_INCOMPATIBLES")
	// ErrContainerDoesNotConvert: a sack has no fixed equivalence; what it holds
	// is a property of the purchase, not of the unit.
	ErrContainerDoesNotConvert = errors.New("ENVASE_NO_CONVIERTE")
	// ErrContainerNeedsContent: a container line without its content is a hole in
	// the purchase, never a default.
	ErrContainerNeedsContent = errors.New("CONTENIDO_POR_ENVASE_REQUERIDO")
	// ErrInvalidPresentation marks a presentation that does not hang together.
	ErrInvalidPresentation = errors.New("PRESENTACION_INVALIDA")
)

// divisionScale is the working precision for intermediate divisions. Wide enough
// that a repeating decimal does not lose cents when multiplied back up by a
// purchase quantity, and finite so results are deterministic.
const divisionScale = 12

var one = decimal.NewFromInt(1)

// Unit is a unit of measure as the engine needs it.
type Unit struct {
	Code      string
	Dimension string
	// FactorToBase converts to the dimension's base unit. Always 1 for a
	// container.
	FactorToBase decimal.Decimal
}

// IsContainer reports whether the unit is an abstract container.
func (u Unit) IsContainer() bool { return u.Dimension == DimensionContainer }

// Presentation is an admitted purchase unit for an item, with how much of
// ContentUnit one PurchaseUnit holds: a 50 kg sack, a box of 500 cups.
type Presentation struct {
	Name         string
	PurchaseUnit Unit
	ContentUnit  Unit
	// ContentQuantity is how much of ContentUnit one purchase unit holds. Zero
	// means "not given": derived for a physical unit, placeholder 1 for a
	// container.
	ContentQuantity decimal.Decimal
}

// ValidateUnit checks the intrinsic part of a unit: a known dimension and a
// positive, bounded factor. A container's factor is meaningless, so it is
// forced to 1 whatever the caller sent. Code and name length are input rules of
// each core, not of conversion, and are left to the caller.
func ValidateUnit(u Unit) (Unit, error) {
	if !oneOf(u.Dimension, Dimensions) {
		return Unit{}, fmt.Errorf("%w: dimensión %q (válidas: %s)", ErrInvalidUnit, u.Dimension, strings.Join(Dimensions, ", "))
	}
	if u.IsContainer() {
		u.FactorToBase = one
	}
	if !u.FactorToBase.IsPositive() {
		return Unit{}, fmt.Errorf("%w: el factor a la unidad base debe ser positivo", ErrInvalidUnit)
	}
	if err := decimals.CheckWith(u.FactorToBase, "factor a la unidad base", decimals.Factor); err != nil {
		return Unit{}, fmt.Errorf("%w: %v", ErrInvalidUnit, err)
	}
	return u, nil
}

// Convert expresses a quantity given in `from` into `to`.
//
// It fails across dimensions, and it fails for containers: a sack cannot be
// converted to grams without knowing what that particular sack held.
func Convert(qty decimal.Decimal, from, to Unit) (decimal.Decimal, error) {
	if from.Code != "" && from.Code == to.Code {
		return qty, nil
	}
	if from.IsContainer() || to.IsContainer() {
		return decimal.Zero, fmt.Errorf("%w: %q no se convierte a %q; el contenido de un envase se declara en la presentación y en la compra", ErrContainerDoesNotConvert, from.Code, to.Code)
	}
	if from.Dimension != to.Dimension {
		return decimal.Zero, fmt.Errorf("%w: %q (%s) no se convierte a %q (%s)", ErrIncompatibleDimensions, from.Code, from.Dimension, to.Code, to.Dimension)
	}
	if to.FactorToBase.IsZero() {
		return decimal.Zero, fmt.Errorf("%w: la unidad %q tiene factor cero", ErrInvalidUnit, to.Code)
	}
	// quantity -> base -> target.
	inBase := qty.Mul(from.FactorToBase)
	return inBase.DivRound(to.FactorToBase, divisionScale), nil
}

// ValidatePresentation checks a presentation against the units it names and the
// item's consumption unit, and fills the content a physical unit implies.
//
//   - The content unit must share the consumption unit's dimension.
//   - A physical purchase unit must share it too, and then its content is
//     derived from the factors (a kilo is a thousand grams, always) rather than
//     asked.
//   - A container declares the placeholder 1 unless a real content is given;
//     either way the purchase line decides.
//
// A zero ContentQuantity means "not given". A caller that must reject an
// explicit zero enforces that at its own boundary before calling in.
func ValidatePresentation(p Presentation, consumption Unit) (Presentation, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		return Presentation{}, fmt.Errorf("%w: el nombre no puede estar vacío", ErrInvalidPresentation)
	}
	if p.ContentUnit.IsContainer() {
		return Presentation{}, fmt.Errorf("%w: el contenido no se mide en un envase (%q)", ErrIncompatibleDimensions, p.ContentUnit.Code)
	}
	if p.ContentUnit.Dimension != consumption.Dimension {
		return Presentation{}, fmt.Errorf("%w: el contenido se mide en %q (%s) y el artículo se consume en %q (%s)", ErrIncompatibleDimensions, p.ContentUnit.Code, p.ContentUnit.Dimension, consumption.Code, consumption.Dimension)
	}
	if !p.PurchaseUnit.IsContainer() && p.PurchaseUnit.Dimension != consumption.Dimension {
		return Presentation{}, fmt.Errorf("%w: se compra en %q (%s) y se consume en %q (%s)", ErrIncompatibleDimensions, p.PurchaseUnit.Code, p.PurchaseUnit.Dimension, consumption.Code, consumption.Dimension)
	}
	if p.ContentQuantity.IsZero() {
		if p.PurchaseUnit.IsContainer() {
			p.ContentQuantity = one
		} else {
			derived, err := Convert(one, p.PurchaseUnit, p.ContentUnit)
			if err != nil {
				return Presentation{}, err
			}
			p.ContentQuantity = derived
		}
	}
	if !p.ContentQuantity.IsPositive() {
		return Presentation{}, fmt.Errorf("%w: el contenido por unidad de compra debe ser positivo", ErrInvalidPresentation)
	}
	return p, nil
}

// ToConsumptionUnit turns what a purchase line says ("3 sacks") into the
// quantity the kardex uses: the amount in the item's consumption unit.
//
// contentPerContainer is what each container held IN THIS purchase, in the
// presentation's content unit. It only matters for a container, where it wins
// over the declared content; zero means "use the declared content", and a
// declared content that is only the placeholder 1 is not a content. Falling
// back to it would register "3 sacks" as 3 grams with no error anywhere.
func ToConsumptionUnit(amount decimal.Decimal, p Presentation, contentPerContainer decimal.Decimal, consumption Unit) (decimal.Decimal, error) {
	perUnit := p.ContentQuantity
	if p.PurchaseUnit.IsContainer() {
		perUnit = contentPerContainer
		if !perUnit.IsPositive() && p.ContentQuantity.GreaterThan(one) {
			perUnit = p.ContentQuantity
		}
		if !perUnit.IsPositive() {
			return decimal.Zero, fmt.Errorf("%w: la línea comprada por %s necesita el contenido por envase; cuánto trae cada uno varía entre compras y se escribe en la factura", ErrContainerNeedsContent, p.Name)
		}
	}
	if !perUnit.IsPositive() {
		return decimal.Zero, fmt.Errorf("%w: la presentación %q no declara contenido, así que %q no se convierte", ErrInvalidPresentation, p.Name, p.PurchaseUnit.Code)
	}
	// One purchase unit holds perUnit of the content unit.
	inContent := amount.Mul(perUnit)
	return Convert(inContent, p.ContentUnit, consumption)
}

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

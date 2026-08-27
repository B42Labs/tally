// Package money holds the only rounding and division entry points in the system.
// Money, prices, and usage quantities are decimals from end to end: floats never
// touch them, which the forbidigo rules in .golangci.yml enforce.
//
// The normative specification is roadmap/00-conventions.md section 6.
package money

import (
	"github.com/shopspring/decimal"
)

const (
	// AmountPlaces is the scale every monetary value is rounded and rendered at.
	// It is exported because a renderer outside this package writes the same
	// numbers as text rather than through Amount, and a second copy of the scale
	// would drift from this one without a test noticing.
	AmountPlaces = 2
	// QuantityPlaces is the scale usage quantities, minutes and counters alike,
	// are rounded and rendered at. It is exported for the reason AmountPlaces
	// is.
	QuantityPlaces = 4
	// RatePlaces is the scale the rate of a pricing adjustment is rendered at.
	// Six fractional digits is what the adjustments schema admits, and the
	// constant is exported for the reason AmountPlaces is.
	RatePlaces = 6
	// divisionScale is the working precision every division runs at, set
	// explicitly so no result depends on decimal.DivisionPrecision.
	divisionScale = 28
)

// Round2 rounds to two decimal places, half away from zero. It is the single
// rounding entry point: per-dimension costs are rounded here, and every
// aggregate is a sum of already-rounded values, so a total always equals the sum
// of its visible line items.
func Round2(d decimal.Decimal) decimal.Decimal {
	return d.Round(AmountPlaces)
}

// Minutes converts a duration in whole seconds to minutes, rounded to four
// decimal places. Interval arithmetic is done in integer seconds; this is where
// it becomes a billable quantity.
func Minutes(seconds int64) decimal.Decimal {
	return Div(decimal.NewFromInt(seconds), decimal.NewFromInt(60)).Round(QuantityPlaces)
}

// RoundQuantity rounds a usage quantity to four decimal places, half away from
// zero, the way Round2 rounds money. It is the rounding a counter value read
// from a metrics store goes through before it is carried as a Quantity.
func RoundQuantity(d decimal.Decimal) decimal.Decimal {
	return d.Round(QuantityPlaces)
}

// Div divides at an explicit working precision. Dividing by zero panics, as it
// does in the underlying decimal package: no billing input can legitimately be a
// zero divisor, so it is a caller bug rather than a condition to handle.
func Div(a, b decimal.Decimal) decimal.Decimal {
	return a.DivRound(b, divisionScale)
}

// Amount is a monetary value that serializes at full two-place precision. A bare
// decimal renders 19.20 as 19.2, which is not acceptable in an export a customer
// reconciles against an invoice.
type Amount struct {
	decimal.Decimal
}

// NewAmount wraps a decimal for serialization. It does not round: round with
// Round2 first where the value is computed.
func NewAmount(d decimal.Decimal) Amount {
	return Amount{Decimal: d}
}

// MarshalJSON renders the amount as a JSON number with exactly two decimal places.
func (a Amount) MarshalJSON() ([]byte, error) {
	return []byte(a.StringFixed(AmountPlaces)), nil
}

// Quantity is a usage quantity that serializes at the four-place scale Minutes
// and RoundQuantity round to. A bare decimal renders 21600.0000 as 21600, which
// hides the scale the quantity carries into JSONB.
type Quantity struct {
	decimal.Decimal
}

// NewQuantity wraps a decimal for serialization. It does not round: derive the
// value with Minutes, or round it with RoundQuantity, which both round to four
// places.
func NewQuantity(d decimal.Decimal) Quantity {
	return Quantity{Decimal: d}
}

// MarshalJSON renders the quantity as a JSON number with exactly four decimal places.
func (q Quantity) MarshalJSON() ([]byte, error) {
	return []byte(q.StringFixed(QuantityPlaces)), nil
}

// Rate is the rate of a pricing adjustment that serializes at the six-place
// scale the adjustments schema admits. A bare decimal renders 0.150000 as 0.15,
// which drops the scale the rate was written with.
type Rate struct {
	decimal.Decimal
}

// NewRate wraps a decimal for serialization. It does not round: a rate is
// parsed from an adjustments document, which admits at most six fractional
// digits.
func NewRate(d decimal.Decimal) Rate {
	return Rate{Decimal: d}
}

// MarshalJSON renders the rate as a JSON number with exactly six decimal places.
func (r Rate) MarshalJSON() ([]byte, error) {
	return []byte(r.StringFixed(RatePlaces)), nil
}

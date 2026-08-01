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
	// moneyPlaces is the scale every monetary value is rounded and rendered at.
	moneyPlaces = 2
	// minutePlaces is the scale derived minute quantities are rounded at.
	minutePlaces = 4
	// divisionScale is the working precision every division runs at, set
	// explicitly so no result depends on decimal.DivisionPrecision.
	divisionScale = 28
)

// Round2 rounds to two decimal places, half away from zero. It is the single
// rounding entry point: per-dimension costs are rounded here, and every
// aggregate is a sum of already-rounded values, so a total always equals the sum
// of its visible line items.
func Round2(d decimal.Decimal) decimal.Decimal {
	return d.Round(moneyPlaces)
}

// Minutes converts a duration in whole seconds to minutes, rounded to four
// decimal places. Interval arithmetic is done in integer seconds; this is where
// it becomes a billable quantity.
func Minutes(seconds int64) decimal.Decimal {
	return Div(decimal.NewFromInt(seconds), decimal.NewFromInt(60)).Round(minutePlaces)
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
	return []byte(a.StringFixed(moneyPlaces)), nil
}

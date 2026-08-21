// Package period turns the YYYY-MM billing month the engine CLI reads and
// prints into the interval a run meters over. A billing period is a UTC
// calendar month, half-open [first day 00:00:00Z, first day of the next month
// 00:00:00Z), so an instant exactly on a boundary belongs to the next period.
//
// The normative specification is roadmap/00-conventions.md section 5.
package period

import (
	"fmt"
	"time"
)

// monthLayout is the reference time in the YYYY-MM form, for both directions.
const monthLayout = "2006-01"

// Parse reads a billing month and returns its half-open interval: "2026-03"
// maps to [2026-03-01T00:00:00Z, 2026-04-01T00:00:00Z). The month is four
// digits, a hyphen, and 01 to 12; anything else is an error quoting the input,
// which is what the CLI shows the operator.
func Parse(s string) (from, to time.Time, err error) {
	// The accepted shape is stated here rather than left to how tolerant
	// time.Parse is about padding and stray characters around a layout: seven
	// characters, four digits, a hyphen, two digits.
	if len(s) != 7 {
		return time.Time{}, time.Time{}, fmt.Errorf("%q is not a YYYY-MM month", s)
	}
	// The layout carries no zone, so the parsed instant is already UTC.
	from, err = time.Parse(monthLayout, s)
	if err != nil {
		// The parser's error names the layout and the fragment it choked on,
		// which tells an operator who mistyped --period nothing. The message
		// above is the whole diagnosis, so it replaces that error rather than
		// wrapping it.
		return time.Time{}, time.Time{}, fmt.Errorf("%q is not a YYYY-MM month", s)
	}
	// Adding one month rolls December into January of the next year.
	return from, from.AddDate(0, 1, 0), nil
}

// Format renders the start of a billing period as its YYYY-MM month, the form
// periods list prints. It is Parse's inverse over a period's start. A period is
// a UTC month, so an instant carrying another zone is converted before its
// month is named.
func Format(from time.Time) string {
	return from.UTC().Format(monthLayout)
}

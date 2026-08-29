package simulator

import (
	"math/rand/v2"
	"time"
)

// The working-week profile weighs the hours of the simulated period. The office
// hours of a Monday to Friday carry ten times the weight of a night or a
// weekend, and the two hours before and the four after them sit in between,
// which is what a cloud that people work on looks like. Which days are working
// days follows the real calendar of the period: July 2026 begins on a
// Wednesday, and its first weekend is the fourth and the fifth.
//
// Every draw the profile makes is taken from the shape generator, so a profile
// decides what happens and when and never which identifiers a month is built
// from. Every instant it returns is a whole number of seconds, the way span
// produces one, because the projection orders events by their timestamp and two
// in the same second are two it cannot order.
const (
	officeWeight = 10
	fringeWeight = 3
	quietWeight  = 1

	// The UTC hours the weights are cut at. Office hours run from officeFrom to
	// officeTo, the fringe around them from fringeFrom to fringeTo, and what is
	// left of the day is quiet.
	officeFrom = 7
	officeTo   = 19
	fringeFrom = 5
	fringeTo   = 23
)

// workingDay reports whether t falls on a Monday to Friday.
func workingDay(t time.Time) bool {
	weekday := t.UTC().Weekday()
	return weekday != time.Saturday && weekday != time.Sunday
}

// hourWeight is how likely an instant in the hour t lies in is against one in
// any other hour of the period. The weights are relative to each other, and the
// quiet ones are never zero: a window that holds nothing but a weekend still
// yields an instant.
func hourWeight(t time.Time) int {
	hour := t.UTC().Hour()
	switch {
	case !workingDay(t):
		return quietWeight
	case hour >= officeFrom && hour < officeTo:
		return officeWeight
	case hour >= fringeFrom && hour < fringeTo:
		return fringeWeight
	default:
		return quietWeight
	}
}

// slot is a piece of a window that lies inside one UTC clock hour, together
// with the whole seconds it holds. hourWeight is constant over a slot, which is
// what lets a window be weighed piece by piece.
type slot struct {
	start   time.Time
	seconds int64
}

// slotsOf cuts [lo, hi) at every full UTC hour. The first piece runs from lo to
// the next full hour and the last one ends at hi, so a window that starts or
// ends mid-hour keeps the weight of the hours both of its ends lie in. A hi
// that is not after lo is no range and is cut into nothing.
func slotsOf(lo, hi time.Time) []slot {
	var pieces []slot
	for cursor := lo; cursor.Before(hi); {
		end := cursor.Truncate(time.Hour).Add(time.Hour)
		if end.After(hi) {
			end = hi
		}
		pieces = append(pieces, slot{start: cursor, seconds: int64(end.Sub(cursor) / time.Second)})
		cursor = end
	}
	return pieces
}

// pick draws one of the pieces, each as likely as the weight it carries against
// the others, and returns it together with what is left of the draw inside it.
// It reports false when the weights add up to nothing, which is a window the
// caller has to answer for itself, and it costs no draw at all in that case.
func pick(shape *rand.Rand, pieces []slot, weight func(slot) int64) (slot, int64, bool) {
	var total int64
	for _, piece := range pieces {
		total += weight(piece)
	}
	if total == 0 {
		return slot{}, 0, false
	}

	drawn := shape.Int64N(total)
	for _, piece := range pieces {
		share := weight(piece)
		if drawn < share {
			return piece, drawn, true
		}
		drawn -= share
	}
	// The draw is below the total the loop subtracts from, so one of the pieces
	// above holds it and the loop returns before it runs out.
	return slot{}, 0, false
}

// drawInstant draws one instant from [lo, hi) on the working-week profile: an
// office hour of a working day is ten times as likely as a night hour, and the
// seconds of one hour are equally likely among themselves. It is what a step
// nobody waits for is placed by, such as a CI job a schedule starts or a
// machine-driven step of a shoot.
//
// A window that holds no whole second is answered with lo and costs no draw at
// all. A caller that hands one over therefore leaves the generators of the
// month exactly where they were, and the transitions after it keep their
// instants.
func drawInstant(shape *rand.Rand, lo, hi time.Time) time.Time {
	piece, _, ok := pick(shape, slotsOf(lo, hi), func(piece slot) int64 {
		return int64(hourWeight(piece.start)) * piece.seconds
	})
	if !ok {
		return lo
	}
	// The hours weigh against each other, so the second inside the drawn hour is
	// a draw of its own.
	return piece.start.Add(time.Duration(shape.Int64N(piece.seconds)) * time.Second)
}

// drawWorkingInstant draws one instant from the office hours of [lo, hi), each
// of their seconds equally likely. It places what somebody triggers: a shoot is
// created and a pipeline is started while its team is at work, whereas what
// follows on its own runs at any hour.
//
// A window without a single office second falls back to drawInstant, because a
// caller that has to place a step inside such a window needs an instant rather
// than a refusal.
func drawWorkingInstant(shape *rand.Rand, lo, hi time.Time) time.Time {
	piece, second, ok := pick(shape, slotsOf(lo, hi), func(piece slot) int64 {
		if hourWeight(piece.start) != officeWeight {
			return 0
		}
		return piece.seconds
	})
	if !ok {
		return drawInstant(shape, lo, hi)
	}
	// Every office second weighs the same, so the one draw picks the piece and
	// the second inside it at once.
	return piece.start.Add(time.Duration(second) * time.Second)
}

// workingDays returns the midnight of every Monday to Friday in [from, to), in
// order. A workload that does something once a day walks them, and it skips the
// weekends of the real calendar rather than every seventh day counted from the
// first: July 2026 has 23 of them.
//
// A range that holds none is answered with an empty slice and never with nil,
// so a caller ranges over the result without a check.
func workingDays(from, to time.Time) []time.Time {
	from = from.UTC()
	cursor := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)

	days := make([]time.Time, 0)
	for ; cursor.Before(to); cursor = cursor.AddDate(0, 0, 1) {
		if workingDay(cursor) {
			days = append(days, cursor)
		}
	}
	return days
}

// at is the instant a clock time falls on a day. The workloads state the steps
// they anchor on an hour and a minute of a working day, and this puts them on
// the UTC calendar the rest of the month is drawn on.
func at(day time.Time, hour, minute int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, time.UTC)
}

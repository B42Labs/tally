package simulator

import (
	"math/rand/v2"
	"testing"
	"time"
)

// The two days of July 2026 the profile is held against. The eighth is a
// Wednesday and the eleventh a Saturday, which the tests below check before
// they read a weight off them: a profile that follows the real calendar is only
// worth testing against days the calendar really has.
var (
	wednesday = time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	saturday  = time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC)
	// mondayOfWeek28 opens the one full Monday to Monday week the draws are
	// counted over, so a distribution covers five working days and one weekend.
	mondayOfWeek28 = time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
)

// profileShape is a shape generator for the profile tests. Pinning the stream
// keeps a distribution that fails reproducible.
func profileShape(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, shapeStream))
}

// requireWeekday fails the test when the day is not the one the case is written
// for.
func requireWeekday(t *testing.T, day time.Time, want time.Weekday) {
	t.Helper()

	if day.Weekday() != want {
		t.Fatalf("%s is a %s, want a %s: the test days come from the real calendar",
			day.Format(time.DateOnly), day.Weekday(), want)
	}
}

func TestHourWeightFollowsTheWorkingWeek(t *testing.T) {
	requireWeekday(t, wednesday, time.Wednesday)
	requireWeekday(t, saturday, time.Saturday)

	cases := []struct {
		name    string
		instant time.Time
		want    int
	}{
		{"a working day at midday", at(wednesday, 10, 0), officeWeight},
		{"the hour before a working day's office hours", at(wednesday, 6, 0), fringeWeight},
		{"the evening of a working day", at(wednesday, 20, 0), fringeWeight},
		{"the night of a working day", at(wednesday, 2, 0), quietWeight},
		{"a Saturday at the same hour as the working week's peak", at(saturday, 10, 0), quietWeight},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hourWeight(c.instant); got != c.want {
				t.Errorf("hourWeight(%s, a %s) = %d, want %d",
					c.instant.Format(time.RFC3339), c.instant.Weekday(), got, c.want)
			}
		})
	}
}

func TestDrawInstantStaysInsideTheWindowOnWholeSeconds(t *testing.T) {
	shape := profileShape(3)
	lo := at(wednesday, 16, 0)
	hi := at(wednesday, 18, 30)

	for i := range 1000 {
		got := drawInstant(shape, lo, hi)

		if got.Before(lo) || !got.Before(hi) {
			t.Fatalf("draw %d = %s, want it inside [%s, %s): an instant outside the window is a "+
				"notification outside the period it was drawn for", i, got, lo, hi)
		}
		if !got.Truncate(time.Second).Equal(got) {
			t.Fatalf("draw %d = %s, want a whole second: the projection orders events by their "+
				"timestamp, and the rest of the month is drawn on whole seconds", i, got)
		}
	}
}

func TestDrawInstantFavoursWorkingHours(t *testing.T) {
	requireWeekday(t, mondayOfWeek28, time.Monday)

	shape := profileShape(5)
	lo := mondayOfWeek28
	hi := lo.AddDate(0, 0, 7)

	const draws = 10000
	var office, quiet int
	for range draws {
		switch hourWeight(drawInstant(shape, lo, hi)) {
		case officeWeight:
			office++
		case quietWeight:
			quiet++
		}
	}

	// Over a full week the office hours hold about 78 % of the weight and the
	// nights and weekends about 10 %. A profile that had gone uniform would put
	// 36 % on the office hours and 46 % on the quiet ones, so the bounds are wide
	// enough for the draw to wander and still tell the two apart.
	if office < 7000 {
		t.Errorf("%d of %d draws landed on an office hour, want at least 7000: the workloads are "+
			"placed on the working week, not spread over the month", office, draws)
	}
	if quiet > 1500 {
		t.Errorf("%d of %d draws landed on a night or a weekend, want at most 1500: the working "+
			"week is what makes a simulated month look like a worked one", quiet, draws)
	}
}

func TestDrawInstantReturnsLoOnADegenerateWindow(t *testing.T) {
	lo := at(wednesday, 9, 0)

	cases := []struct {
		name string
		hi   time.Time
	}{
		{"a window of no length", lo},
		{"a window that ends before it starts", lo.Add(-time.Hour)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := drawInstant(profileShape(7), lo, c.hi); !got.Equal(lo) {
				t.Errorf("drawInstant(%s, %s) = %s, want %s: a window with nothing to draw from "+
					"answers with its own start instead of panicking",
					lo.Format(time.RFC3339), c.hi.Format(time.RFC3339), got, lo)
			}
		})
	}

	t.Run("it costs no draw", func(t *testing.T) {
		used, untouched := profileShape(7), profileShape(7)
		drawInstant(used, lo, lo)

		if got, want := used.Uint64(), untouched.Uint64(); got != want {
			t.Errorf("the generator that went through a degenerate window yields %d next, want %d: "+
				"a draw spent here would move every transition after it", got, want)
		}
	})
}

func TestDrawWorkingInstantDrawsWorkingHoursOnly(t *testing.T) {
	shape := profileShape(11)
	lo := mondayOfWeek28
	hi := lo.AddDate(0, 0, 7)

	for i := range 1000 {
		got := drawWorkingInstant(shape, lo, hi)

		if got.Before(lo) || !got.Before(hi) {
			t.Fatalf("draw %d = %s, want it inside [%s, %s)", i, got, lo, hi)
		}
		if hourWeight(got) != officeWeight {
			t.Fatalf("draw %d = %s (a %s) has weight %d, want %d: what somebody triggers happens "+
				"while that somebody is at work", i, got, got.Weekday(), hourWeight(got), officeWeight)
		}
	}

	t.Run("a window without a working second falls back", func(t *testing.T) {
		sunday := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)
		requireWeekday(t, sunday, time.Sunday)
		night, morning := at(sunday, 0, 0), at(sunday, 6, 0)

		got := drawWorkingInstant(profileShape(13), night, morning)
		if got.Before(night) || !got.Before(morning) {
			t.Errorf("drawWorkingInstant over a Sunday night = %s, want an instant inside [%s, %s): "+
				"a window holding no office hour is answered rather than refused", got, night, morning)
		}
	})
}

func TestWorkingDaysOfJuly2026(t *testing.T) {
	days := workingDays(july2026, july2026.AddDate(0, 1, 0))

	if len(days) != 23 {
		t.Fatalf("workingDays(July 2026) returned %d days, want the 23 the calendar has: a workload "+
			"that runs once a working day runs that often", len(days))
	}
	if want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC); !days[0].Equal(want) {
		t.Errorf("the first working day is %s, want %s, the Wednesday July 2026 opens on",
			days[0], want)
	}
	if want := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC); !days[len(days)-1].Equal(want) {
		t.Errorf("the last working day is %s, want %s, the Friday July 2026 ends on",
			days[len(days)-1], want)
	}
	for _, midnight := range days {
		if !workingDay(midnight) {
			t.Errorf("%s is a %s, want no weekend among the working days",
				midnight.Format(time.DateOnly), midnight.Weekday())
		}
		if !midnight.Equal(at(midnight, 0, 0)) {
			t.Errorf("the working day %s is not at midnight UTC, want the day itself so that a "+
				"caller places its own hours on it", midnight)
		}
	}

	empty := workingDays(july2026, july2026)
	if empty == nil {
		t.Fatal("workingDays over an empty range returned nil, want an empty slice a caller ranges " +
			"over without a check")
	}
	if len(empty) != 0 {
		t.Errorf("workingDays over an empty range returned %d days, want none", len(empty))
	}
}

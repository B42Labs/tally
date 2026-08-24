package scheduler

import (
	"testing"
	"time"

	"github.com/b42labs/tally/internal/engine/period"
)

// at is a UTC instant, the form a stored period and the tick's clock reach the
// walk in.
func at(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

// earliestAt is the first stored period as monthsDue takes it: a pointer,
// because an engine that has never billed a month has none.
func earliestAt(t time.Time) *time.Time {
	return &t
}

// monthsBack is the count months ending at last, oldest first: what the walk of
// a tick that is capped covers.
func monthsBack(last time.Time, count int) []string {
	months := make([]string, 0, count)
	for i := count - 1; i >= 0; i-- {
		months = append(months, period.Format(last.AddDate(0, -i, 0)))
	}
	return months
}

// TestMonthsDue pins which months one tick walks. The walk decides what the
// tick touches at all, and it is the one part of the state machine that needs
// no database: both of its inputs are arguments.
func TestMonthsDue(t *testing.T) {
	// +02:00, the zone a European connection reads a timestamp back in.
	cest := time.FixedZone("CEST", 2*3600)

	for name, tc := range map[string]struct {
		earliest    *time.Time
		now         time.Time
		want        []string
		wantSkipped int
	}{
		"the walk runs from the earliest period to the last month that has ended": {
			earliest: earliestAt(at(2026, time.January, 1, 0, 0)),
			now:      at(2026, time.April, 15, 10, 30),
			want:     []string{"2026-01", "2026-02", "2026-03"},
		},
		"no stored period walks the month before now": {
			earliest: nil,
			now:      at(2026, time.April, 15, 10, 30),
			want:     []string{"2026-03"},
		},
		"an earliest past the last month that has ended walks nothing": {
			earliest: earliestAt(at(2026, time.April, 1, 0, 0)),
			now:      at(2026, time.April, 15, 10, 30),
			want:     nil,
		},
		"december rolls into january": {
			earliest: earliestAt(at(2025, time.November, 1, 0, 0)),
			now:      at(2026, time.January, 5, 6, 0),
			want:     []string{"2025-11", "2025-12"},
		},
		"an earliest inside a month anchors the walk on that month": {
			earliest: earliestAt(at(2026, time.January, 17, 13, 45)),
			now:      at(2026, time.April, 15, 10, 30),
			want:     []string{"2026-01", "2026-02", "2026-03"},
		},
		// A period ends exactly at the first instant of the next month, so the
		// month that just ended is due at that instant rather than an hour later.
		"a clock on a month boundary walks the month that ended there": {
			earliest: earliestAt(at(2026, time.March, 1, 0, 0)),
			now:      at(2026, time.April, 1, 0, 0),
			want:     []string{"2026-03"},
		},
		// 2026-01-31T22:30Z read back as 2026-02-01T00:30+02:00. The zone would
		// anchor the walk on February and drop the month the period belongs to.
		"an earliest in another zone anchors the walk on its utc month": {
			earliest: earliestAt(time.Date(2026, time.February, 1, 0, 30, 0, 0, cest)),
			now:      at(2026, time.April, 15, 10, 30),
			want:     []string{"2026-01", "2026-02", "2026-03"},
		},
		// A mistyped tally-engine run --period is what puts a period row that far
		// back, and no subcommand takes it out again. Unbounded, the walk of every
		// following tick would be some twenty-four thousand months long: the tick
		// would never end, and concurrencyPolicy: Forbid would keep the next one
		// from ever starting.
		"an ancient earliest walks the most recent months rather than all of them": {
			earliest: earliestAt(at(1, time.January, 1, 0, 0)),
			now:      at(2026, time.April, 15, 10, 30),
			want:     monthsBack(at(2026, time.March, 1, 0, 0), maxTickMonths),
			// 0001-01 through 2023-03, the months the cap leaves out. The tick
			// never walks them again, so the count is what it reports rather than
			// dropping them: a database restored from an old backup carries a
			// month in grace that would otherwise never be metered, never be
			// finalized and never be named anywhere.
			wantSkipped: 24267,
		},
		"an earliest exactly maxTickMonths back is walked whole": {
			earliest: earliestAt(at(2023, time.April, 1, 0, 0)),
			now:      at(2026, time.April, 15, 10, 30),
			want:     monthsBack(at(2026, time.March, 1, 0, 0), maxTickMonths),
		},
	} {
		t.Run(name, func(t *testing.T) {
			due, skipped := monthsDue(tc.earliest, tc.now)

			if skipped != tc.wantSkipped {
				t.Errorf("monthsDue() skipped %d months, want %d", skipped, tc.wantSkipped)
			}
			months := make([]string, 0, len(due))
			for _, month := range due {
				months = append(months, period.Format(month.From))
			}
			if len(months) != len(tc.want) {
				t.Fatalf("monthsDue() = %v, want %v", months, tc.want)
			}
			for i, want := range tc.want {
				if months[i] != want {
					t.Fatalf("monthsDue() = %v, want %v", months, tc.want)
				}
				// Every month is a whole UTC calendar month, which is what the
				// period rows the tick writes are keyed on.
				from, to, err := period.Parse(want)
				if err != nil {
					t.Fatalf("Parse(%q) error = %v, want nil", want, err)
				}
				if !due[i].From.Equal(from) || !due[i].To.Equal(to) {
					t.Errorf("month %s = [%s, %s), want [%s, %s)", want,
						due[i].From.Format(time.RFC3339), due[i].To.Format(time.RFC3339),
						from.Format(time.RFC3339), to.Format(time.RFC3339))
				}
			}
		})
	}
}

package period_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/engine/period"
)

func TestParse(t *testing.T) {
	for name, tc := range map[string]struct {
		in       string
		wantFrom time.Time
		wantTo   time.Time
	}{
		"a month becomes its half-open interval": {
			in:       "2026-03",
			wantFrom: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			wantTo:   time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		},
		"december ends in january of the next year": {
			in:       "2026-12",
			wantFrom: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC),
			wantTo:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	} {
		t.Run(name, func(t *testing.T) {
			from, to, err := period.Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v, want nil", tc.in, err)
			}
			if !from.Equal(tc.wantFrom) {
				t.Errorf("Parse(%q) from = %s, want %s", tc.in, from.Format(time.RFC3339), tc.wantFrom.Format(time.RFC3339))
			}
			if !to.Equal(tc.wantTo) {
				t.Errorf("Parse(%q) to = %s, want %s", tc.in, to.Format(time.RFC3339), tc.wantTo.Format(time.RFC3339))
			}
		})
	}
}

func TestParseRejects(t *testing.T) {
	// The message reaches an operator who mistyped --period, so the test pins
	// its wording, not only that some error came back.
	for _, in := range []string{"", "2026-3", "2026-00", "2026-13", "March 2026"} {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			from, to, err := period.Parse(in)
			if err == nil {
				t.Fatalf("Parse(%q) error = nil, want an error", in)
			}
			if want := fmt.Sprintf("%q is not a YYYY-MM month", in); err.Error() != want {
				t.Errorf("Parse(%q) error = %q, want %q", in, err.Error(), want)
			}
			if !from.IsZero() || !to.IsZero() {
				t.Errorf("Parse(%q) = %s, %s, want the zero times", in, from, to)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	// +02:00, the zone a European operator's clock is in over summer.
	cest := time.FixedZone("CEST", 2*3600)

	for name, tc := range map[string]struct {
		in   time.Time
		want string
	}{
		"a period start renders as its month": {
			in:   time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			want: "2026-03",
		},
		"the same instant in another zone renders as the same month": {
			in:   time.Date(2026, 3, 1, 2, 0, 0, 0, cest),
			want: "2026-03",
		},
		// 2026-03-31T23:00:00Z. The zone would name April; the UTC month it
		// belongs to is March.
		"an instant whose local month differs renders as its utc month": {
			in:   time.Date(2026, 4, 1, 1, 0, 0, 0, cest),
			want: "2026-03",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := period.Format(tc.in); got != tc.want {
				t.Errorf("Format(%s) = %q, want %q", tc.in.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

package reconciliation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestChangedRecently pins where the settle window ends. A change on its edge
// has settled, one a nanosecond later has not, and a change dated after the run
// cannot be placed before it.
func TestChangedRecently(t *testing.T) {
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	edge := at.Add(-time.Minute)
	justIn := edge.Add(time.Nanosecond)
	ahead := at.Add(time.Second)

	for name, tc := range map[string]struct {
		windowStart *time.Time
		instants    []*time.Time
		want        bool
	}{
		"an instant on the window's edge is not recent": {
			windowStart: &edge, instants: []*time.Time{&edge}, want: false,
		},
		"an instant just inside the window is recent": {
			windowStart: &edge, instants: []*time.Time{&justIn}, want: true,
		},
		"an instant after the run is recent": {
			windowStart: &edge, instants: []*time.Time{&ahead}, want: true,
		},
		"no instant is no evidence": {
			windowStart: &edge, instants: []*time.Time{nil, nil}, want: false,
		},
		"a later instant holds when an earlier one is absent": {
			windowStart: &edge, instants: []*time.Time{nil, &justIn}, want: true,
		},
		"the window off holds nothing": {
			windowStart: nil, instants: []*time.Time{&justIn}, want: false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var d Deferred
			got := d.changedRecently(tc.windowStart, tc.instants...)

			want := Deferred{}
			if tc.want {
				want.Recent = 1
			}
			if got != tc.want || d != want {
				t.Errorf("changedRecently() = %t, counts %+v, want %t, counts %+v", got, d, tc.want, want)
			}
		})
	}
}

// TestUnannouncedOnly pins when an update is a refresh: every member the two
// sizes differ in is one no platform event carries. A member only one side
// holds differs, and a stored document that is not an object makes no refresh.
func TestUnannouncedOnly(t *testing.T) {
	counts := []string{"listeners", "pools"}

	for name, tc := range map[string]struct {
		observed    map[string]any
		stored      string
		unannounced []string
		want        bool
	}{
		"no unannounced member makes no refresh": {
			observed: map[string]any{"listeners": 3}, stored: `{"listeners": 2}`,
			unannounced: nil, want: false,
		},
		"an empty list of unannounced members makes no refresh": {
			observed: map[string]any{"listeners": 3}, stored: `{"listeners": 2}`,
			unannounced: []string{}, want: false,
		},
		"a difference in unannounced members alone is a refresh": {
			observed:    map[string]any{"listeners": 3, "pools": 2, "flavor": "small"},
			stored:      `{"listeners": 2, "pools": 1, "flavor": "small"}`,
			unannounced: counts, want: true,
		},
		"a difference in one member that is not unannounced is no refresh": {
			observed:    map[string]any{"listeners": 3, "flavor": "large"},
			stored:      `{"listeners": 2, "flavor": "small"}`,
			unannounced: counts, want: false,
		},
		"a member only the observation holds differs": {
			observed:    map[string]any{"listeners": 2, "flavor": "small"},
			stored:      `{"listeners": 2}`,
			unannounced: counts, want: false,
		},
		"a member only the row holds differs": {
			observed:    map[string]any{"listeners": 2},
			stored:      `{"listeners": 2, "flavor": "small"}`,
			unannounced: counts, want: false,
		},
		"an unannounced member only one side holds is a refresh": {
			observed:    map[string]any{"listeners": 2, "pools": 1},
			stored:      `{"listeners": 2}`,
			unannounced: counts, want: true,
		},
		"a stored null reads as a size without members": {
			observed: map[string]any{"listeners": 2}, stored: `null`,
			unannounced: counts, want: true,
		},
		"a stored null against a member that is not unannounced is no refresh": {
			observed: map[string]any{"flavor": "small"}, stored: `null`,
			unannounced: counts, want: false,
		},
		"a stored document that is not an object makes no refresh": {
			observed: map[string]any{"listeners": 2}, stored: `[1]`,
			unannounced: counts, want: false,
		},
		"an int and the JSON number it is stored as are one value": {
			observed:    map[string]any{"listeners": 3, "size_gb": 10},
			stored:      `{"listeners": 2, "size_gb": 10}`,
			unannounced: counts, want: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := unannouncedOnly(tc.observed, []byte(tc.stored), tc.unannounced)
			if err != nil {
				t.Fatalf("unannouncedOnly() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("unannouncedOnly() = %t, want %t", got, tc.want)
			}
		})
	}

	t.Run("an empty list decodes nothing", func(t *testing.T) {
		// The stored document does not parse, so a nil error says the function
		// answered before it decoded anything.
		got, err := unannouncedOnly(map[string]any{"listeners": 3}, []byte(`{`), nil)
		if err != nil || got {
			t.Errorf("unannouncedOnly() = %t, %v, want false, nil", got, err)
		}
	})

	t.Run("a stored document that does not parse is an error", func(t *testing.T) {
		_, err := unannouncedOnly(map[string]any{"listeners": 3}, []byte(`{`), counts)

		const prefix = "decoding the stored size:"
		if err == nil || !strings.HasPrefix(err.Error(), prefix) {
			t.Fatalf("unannouncedOnly() error = %v, want one starting %q", err, prefix)
		}
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) {
			t.Errorf("unannouncedOnly() error = %v, want a *json.SyntaxError in its chain", err)
		}
	})

	t.Run("an observed member JSON cannot hold is an error", func(t *testing.T) {
		_, err := unannouncedOnly(map[string]any{"listeners": make(chan int)}, []byte(`{}`), counts)

		const prefix = "marshaling the observed size:"
		if err == nil || !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("unannouncedOnly() error = %v, want one starting %q", err, prefix)
		}
	})
}

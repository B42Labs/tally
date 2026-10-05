package reconciliation

import (
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

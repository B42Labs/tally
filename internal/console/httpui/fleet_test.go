package httpui

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/reporting/httpapi"
)

func TestReadFleet(t *testing.T) {
	t.Parallel()

	t.Run("defaults to the active fleet now", func(t *testing.T) {
		t.Parallel()

		state, err := readFleet(httptest.NewRequest("GET", "/resources", nil), testNow)
		if err != nil {
			t.Fatalf("readFleet() error = %v", err)
		}
		if state.Status != "active" || !state.At.Equal(testNow) || state.Pinned {
			t.Errorf("state = %+v, want active at now, not pinned", state)
		}
	})

	t.Run("reads both forms of an instant as UTC", func(t *testing.T) {
		t.Parallel()

		want := time.Date(2026, 3, 3, 0, 30, 0, 0, time.UTC)
		for _, at := range []string{"2026-03-03T00:30:00Z", "2026-03-03T01:30:00%2B01:00", "2026-03-03T00:30"} {
			state, err := readFleet(httptest.NewRequest("GET", "/resources?at="+at, nil), testNow)
			if err != nil {
				t.Fatalf("readFleet(%s) error = %v", at, err)
			}
			if !state.At.Equal(want) || !state.Pinned || state.At.Location() != time.UTC {
				t.Errorf("readFleet(%s) = %v in %v, want %v pinned in UTC", at, state.At, state.At.Location(), want)
			}
		}
	})

	t.Run("refuses what it cannot read", func(t *testing.T) {
		t.Parallel()

		for _, query := range []string{"status=zombie", "at=yesterday", "at=2026-13-01T00:00", "mode=whenever"} {
			if _, err := readFleet(httptest.NewRequest("GET", "/resources?"+query, nil), testNow); err == nil {
				t.Errorf("readFleet(%s) accepted it", query)
			}
		}
	})

	t.Run("the mode says which way the fleet is read", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			query  string
			window bool
		}{
			{"", false},
			{"at=2026-03-03T00:00", false},
			{"mode=window", true},
			{"from=2026-03-01T00:00", true},
			{"to=2026-03-08T00:00", true},
			{"mode=instant&from=2026-03-01T00:00", false},
		}
		for _, tc := range cases {
			state, err := readFleet(httptest.NewRequest("GET", "/resources?"+tc.query, nil), testNow)
			if err != nil {
				t.Fatalf("readFleet(%s) error = %v", tc.query, err)
			}
			if state.Window != tc.window {
				t.Errorf("readFleet(%s) window = %v, want %v", tc.query, state.Window, tc.window)
			}
		}
	})

	t.Run("each way ignores what the other one is asked with", func(t *testing.T) {
		t.Parallel()

		state, err := readFleet(httptest.NewRequest(
			"GET", "/resources?mode=window&at=yesterday&from=2026-03-01T00:00", nil), testNow)
		if err != nil {
			t.Fatalf("readFleet() error = %v", err)
		}
		if state.Pinned || state.From == nil {
			t.Errorf("state = %+v, want the window alone", state)
		}

		if state, err = readFleet(httptest.NewRequest(
			"GET", "/resources?mode=instant&from=nonsense&at=2026-03-03T00:00", nil), testNow); err != nil {
			t.Fatalf("readFleet() error = %v", err)
		}
		if state.From != nil || !state.Pinned {
			t.Errorf("state = %+v, want the instant alone", state)
		}
	})
}

func TestExistedAt(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	deleted := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	between := time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)
	before := created.Add(-time.Hour)

	cases := []struct {
		name     string
		resource httpapi.Resource
		at       time.Time
		want     bool
	}{
		{"alive and created before", httpapi.Resource{CreatedAt: &created}, between, true},
		{"alive and created at the instant", httpapi.Resource{CreatedAt: &created}, created, true},
		{"not yet created", httpapi.Resource{CreatedAt: &created}, before, false},
		{"deleted after the instant", httpapi.Resource{CreatedAt: &created, DeletedAt: &deleted}, between, true},
		{"deleted at the instant", httpapi.Resource{CreatedAt: &created, DeletedAt: &deleted}, deleted, false},
		{"deleted before the instant", httpapi.Resource{CreatedAt: &created, DeletedAt: &deleted}, deleted.Add(time.Hour), false},
		{"a history without a create before its first event", httpapi.Resource{FirstEventAt: created}, before, false},
		{"a history without a create at its first event", httpapi.Resource{FirstEventAt: created}, created, true},
		{"a history without a create after its first event", httpapi.Resource{FirstEventAt: created}, between, true},
		{"a history without a create that ended", httpapi.Resource{FirstEventAt: created, DeletedAt: &deleted}, deleted.Add(time.Hour), false},
		{"a row with neither a creation nor a first event", httpapi.Resource{}, before, true},
		{"a row with neither a creation nor a first event that ended", httpapi.Resource{DeletedAt: &deleted}, deleted.Add(time.Hour), false},
	}
	for _, tc := range cases {
		if got := existedAt(tc.resource, tc.at); got != tc.want {
			t.Errorf("%s: existedAt() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLifetimeStart(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	created := first.Add(24 * time.Hour)

	cases := []struct {
		name     string
		resource httpapi.Resource
		want     time.Time
		ok       bool
	}{
		{"a creation", httpapi.Resource{CreatedAt: &first}, first, true},
		{"a first event without a creation", httpapi.Resource{FirstEventAt: first}, first, true},
		{"a creation after the first event", httpapi.Resource{CreatedAt: &created, FirstEventAt: first}, created, true},
		{"neither", httpapi.Resource{}, time.Time{}, false},
	}
	for _, tc := range cases {
		got, ok := lifetimeStart(tc.resource)
		if !got.Equal(tc.want) || ok != tc.ok {
			t.Errorf("%s: lifetimeStart() = %v, %v, want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestScale(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	deleted := time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC)

	sc := newScale(created, deleted, 100)
	if got := sc.x(created.Add(-time.Hour)); got != 0 {
		t.Errorf("before the start = %d, want 0", got)
	}
	if got := sc.x(deleted.Add(time.Hour)); got != 100 {
		t.Errorf("after the end = %d, want 100", got)
	}
	if got := sc.x(created.Add(7 * 12 * time.Hour)); got != 50 {
		t.Errorf("halfway = %d, want 50", got)
	}
	if got := newScale(created, created, 100).x(created); got != 0 {
		t.Errorf("an empty span = %d, want 0", got)
	}
}

func TestLifetimeHours(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	deleted := created.Add(90 * time.Minute)

	hours, lived := lifetimeHours(httpapi.Resource{CreatedAt: &created, DeletedAt: &deleted}, testNow)
	if !lived || hours.String() != "1.5" {
		t.Errorf("a deleted resource = %s, %v, want 1.5 hours", hours, lived)
	}

	hours, lived = lifetimeHours(httpapi.Resource{CreatedAt: &created}, testNow)
	if !lived || hours.String() != "348" {
		t.Errorf("a resource still there = %s, %v, want 348 hours to now", hours, lived)
	}

	hours, lived = lifetimeHours(httpapi.Resource{FirstEventAt: created}, testNow)
	if !lived || hours.String() != "348" {
		t.Errorf("a history without a create = %s, %v, want 348 hours from its first event", hours, lived)
	}

	hours, lived = lifetimeHours(httpapi.Resource{FirstEventAt: created, DeletedAt: &deleted}, testNow)
	if !lived || hours.String() != "1.5" {
		t.Errorf("a deleted history without a create = %s, %v, want 1.5 hours", hours, lived)
	}

	hours, lived = lifetimeHours(httpapi.Resource{CreatedAt: &created, FirstEventAt: created.Add(-24 * time.Hour)}, testNow)
	if !lived || hours.String() != "348" {
		t.Errorf("a creation after the first event = %s, %v, want 348 hours from the creation", hours, lived)
	}

	if _, lived = lifetimeHours(httpapi.Resource{}, testNow); lived {
		t.Error("a row with neither a creation nor a first event has a lifetime")
	}

	later := testNow.Add(time.Hour)
	if hours, _ = lifetimeHours(httpapi.Resource{CreatedAt: &later}, testNow); !hours.IsZero() {
		t.Errorf("a creation after now = %s, want 0", hours)
	}
}

func TestPayloadSummary(t *testing.T) {
	t.Parallel()

	if got := payloadSummary(nil); got != "" {
		t.Errorf("no payload = %q", got)
	}
	if got := payloadSummary(&map[string]interface{}{}); got != "" {
		t.Errorf("an empty payload = %q", got)
	}
	if got := payloadSummary(&map[string]interface{}{"a": 1}); got != "1 key" {
		t.Errorf("one key = %q", got)
	}
	if got := payloadSummary(&map[string]interface{}{"a": 1, "b": 2}); got != "2 keys" {
		t.Errorf("two keys = %q", got)
	}
}

func TestKeep(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC)
	deleted := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	gone := httpapi.Resource{CreatedAt: &created, DeletedAt: &deleted}
	alive := httpapi.Resource{CreatedAt: &created}
	// orphan is gone's history without its create, and unplaced is one the API
	// served no first event for either.
	orphan := httpapi.Resource{FirstEventAt: created, DeletedAt: &deleted}
	unplaced := httpapi.Resource{DeletedAt: &deleted}
	at := func(day int) time.Time { return time.Date(2026, 3, day, 0, 0, 0, 0, time.UTC) }
	window := func(from, to int) fleetState {
		f, t := at(from), at(to)
		return fleetState{Window: true, From: &f, To: &t}
	}

	cases := []struct {
		name     string
		resource httpapi.Resource
		state    fleetState
		want     bool
	}{
		{"nothing set keeps what exists now", alive, fleetState{At: testNow}, true},
		{"nothing set drops what is gone now", gone, fleetState{At: testNow}, false},
		{"a window keeps what lived in it", gone, window(5, 8), true},
		{"a window keeps what began in it", gone, window(1, 5), true},
		{"a window keeps what ended in it", gone, window(8, 12), true},
		{"a window keeps what outlives it", alive, window(5, 8), true},
		{"a window drops what ended at its start", gone, window(10, 12), false},
		{"a window drops what began at its end", gone, window(1, 3), false},
		{"a window open at the end keeps what began after the start", gone, fleetState{Window: true, From: pointerTo(at(5))}, true},
		{"a window open at the start drops what began at the end", gone, fleetState{Window: true, To: pointerTo(at(3))}, false},
		{"a window without a bound keeps what is long gone", gone, fleetState{Window: true}, true},
		{"an instant keeps what existed at it", gone, fleetState{At: at(5), Pinned: true}, true},
		{"an instant drops what was gone by then", gone, fleetState{At: at(11), Pinned: true}, false},
		{"a window drops a history without a create that began at its end", orphan, window(1, 3), false},
		{"a window keeps a history without a create that began in it", orphan, window(1, 5), true},
		{"a window open at the start drops a history without a create that began at the end", orphan, fleetState{Window: true, To: pointerTo(at(3))}, false},
		{"a window keeps a row with neither a creation nor a first event", unplaced, window(1, 3), true},
	}
	for _, tc := range cases {
		if got := keep(tc.resource, tc.state); got != tc.want {
			t.Errorf("%s: keep() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

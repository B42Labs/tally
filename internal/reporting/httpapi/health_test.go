package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

// errDatabaseDown is what the fake database returns while it is pretending to
// be unreachable.
var errDatabaseDown = errors.New("database is down")

// errSchemaTooOld is what a reachable database whose schema predates this build
// answers the readiness check with.
var errSchemaTooOld = errors.New("the database is on schema version 0, this build needs 1")

// pingerFunc adapts one function to the DB the probes take, answering both
// checks with it. Tests that need the two to differ use splitDB.
type pingerFunc func(ctx context.Context) error

// Ping runs the adapted function.
func (f pingerFunc) Ping(ctx context.Context) error { return f(ctx) }

// Ready runs the adapted function.
func (f pingerFunc) Ready(ctx context.Context) error { return f(ctx) }

// splitDB answers the two probe checks separately, which is what a database
// that is reachable but not migrated to this build's schema looks like.
type splitDB struct {
	ping  error
	ready error
}

// Ping reports the configured liveness result.
func (d splitDB) Ping(context.Context) error { return d.ping }

// Ready reports the configured readiness result.
func (d splitDB) Ready(context.Context) error { return d.ready }

// fakeClock is the injected clock the threshold tests advance by hand, so that
// a ten-minute outage costs no wall-clock time.
type fakeClock struct {
	now time.Time
}

// Now reports the fake clock's current instant.
func (c *fakeClock) Now() time.Time { return c.now }

// Advance moves the fake clock forward.
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func TestHealthz(t *testing.T) {
	const threshold = 10 * time.Minute

	t.Run("stays alive while the outage is within the threshold", func(t *testing.T) {
		clock, srv, down := newTestServer(t, threshold)
		*down = errDatabaseDown

		clock.Advance(threshold)

		rec := probe(t, srv.Healthz)
		assertOK(t, rec)
	})

	t.Run("fails once the outage outlasts the threshold", func(t *testing.T) {
		clock, srv, down := newTestServer(t, threshold)
		*down = errDatabaseDown

		clock.Advance(threshold + time.Second)

		rec := probe(t, srv.Healthz)
		assertUnavailable(t, rec)
	})

	t.Run("counts an outage from startup while no ping has ever succeeded", func(t *testing.T) {
		clock, srv, down := newTestServer(t, threshold)
		*down = errDatabaseDown

		// Nothing has succeeded yet, so the span is measured against the moment
		// the tracker was built rather than against the zero time.
		clock.Advance(threshold)
		assertOK(t, probe(t, srv.Healthz))

		clock.Advance(time.Second)
		assertUnavailable(t, probe(t, srv.Healthz))
	})

	t.Run("a successful ping resets the outage span", func(t *testing.T) {
		clock, srv, down := newTestServer(t, threshold)
		*down = errDatabaseDown

		clock.Advance(threshold)
		assertOK(t, probe(t, srv.Healthz))

		// The database comes back, which restarts the clock on the next outage.
		*down = nil
		assertOK(t, probe(t, srv.Healthz))

		*down = errDatabaseDown
		clock.Advance(threshold)
		assertOK(t, probe(t, srv.Healthz))

		clock.Advance(time.Second)
		assertUnavailable(t, probe(t, srv.Healthz))
	})

	t.Run("treats a ping that runs into the deadline as a failure", func(t *testing.T) {
		clock, srv, down := newTestServer(t, threshold)
		*down = context.DeadlineExceeded

		clock.Advance(threshold + time.Second)

		assertUnavailable(t, probe(t, srv.Healthz))
	})
}

func TestReadyz(t *testing.T) {
	t.Run("is ready while the database answers", func(t *testing.T) {
		_, srv, _ := newTestServer(t, time.Minute)

		assertOK(t, probe(t, srv.Readyz))
	})

	t.Run("is unready as soon as the database fails, whatever the threshold", func(t *testing.T) {
		clock, srv, down := newTestServer(t, time.Hour)
		*down = errDatabaseDown

		// Well inside the liveness threshold: readiness does not wait it out.
		clock.Advance(time.Second)

		assertUnavailable(t, probe(t, srv.Readyz))
	})

	t.Run("treats a ping that runs into the deadline as a failure", func(t *testing.T) {
		_, srv, down := newTestServer(t, time.Hour)
		*down = context.DeadlineExceeded

		assertUnavailable(t, probe(t, srv.Readyz))
	})

	t.Run("is unready while the database is reachable but unusable", func(t *testing.T) {
		// A database the pool reaches but whose schema this build cannot serve:
		// readiness has to fail so the pod stays out of rotation, while liveness
		// keeps passing because restarting fixes nothing.
		srv := &server{
			db:     splitDB{ready: errSchemaTooOld},
			health: newHealthTracker(time.Now, time.Minute),
		}

		assertUnavailable(t, probe(t, srv.Readyz))
		assertOK(t, probe(t, srv.Healthz))
	})

	t.Run("passes the probe deadline to the database", func(t *testing.T) {
		var deadline time.Time
		srv := &server{
			db: pingerFunc(func(ctx context.Context) error {
				deadline, _ = ctx.Deadline()
				return nil
			}),
			health: newHealthTracker(time.Now, time.Minute),
		}

		before := time.Now()
		assertOK(t, probe(t, srv.Readyz))
		after := time.Now()

		if deadline.IsZero() {
			t.Fatal("the ping ran without a deadline, want one")
		}
		// The ping started somewhere between before and after, so its deadline is
		// one probe timeout past that window and nowhere else.
		if deadline.Before(before.Add(pingTimeout)) || deadline.After(after.Add(pingTimeout)) {
			t.Errorf("ping deadline = %v, want %v after the ping, which started between %v and %v",
				deadline, pingTimeout, before, after)
		}
	})
}

// newTestServer builds a probe server on a fake clock. The returned error
// pointer is what the fake database reports: setting it takes the database down,
// clearing it brings the database back.
func newTestServer(t *testing.T, threshold time.Duration) (*fakeClock, *server, *error) {
	t.Helper()

	clock := &fakeClock{now: time.Date(2026, time.August, 3, 12, 0, 0, 0, time.UTC)}
	var down error
	srv := &server{
		db:     pingerFunc(func(context.Context) error { return down }),
		health: newHealthTracker(clock.Now, threshold),
	}
	return clock, srv, &down
}

// probe runs one probe handler and returns what it wrote.
func probe(t *testing.T, handler http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return rec
}

// assertOK checks the plain-text body a passing probe answers with.
func assertOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if got := rec.Code; got != http.StatusOK {
		t.Errorf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
	}
	if got := rec.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
	if got, want := rec.Header().Get("Content-Type"), "text/plain; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
}

// assertUnavailable checks the problem a failing probe answers with.
func assertUnavailable(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()

	if got := rec.Code; got != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", got, http.StatusServiceUnavailable)
	}
	if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", got, problem.ContentType)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if got := body["type"]; got != problem.TypeUnavailable {
		t.Errorf("body type = %v, want %v", got, problem.TypeUnavailable)
	}
}

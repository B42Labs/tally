package httpapi

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

// DB is the part of the database the probes need. The store satisfies it.
//
// The two checks differ on purpose: liveness asks whether the process can reach
// the database at all, readiness whether this build can serve queries against
// it. A schema older than the build is not something restarting the pod fixes.
type DB interface {
	// Ping reports whether the database answers.
	Ping(ctx context.Context) error
	// Ready reports whether the database can serve this build's queries.
	Ready(ctx context.Context) error
}

// pingTimeout bounds a probe's database check. A probe that hangs is a probe
// that reports nothing, so an unreachable database has to look like a failure
// well before Kubernetes gives up on the request.
const pingTimeout = 2 * time.Second

// healthTracker remembers when the service last reached the database, which is
// what turns single failures into the continuous outage liveness reports on.
type healthTracker struct {
	now       func() time.Time
	threshold time.Duration
	// start is the instant every timestamp below is measured against. Keeping
	// one time.Time and storing offsets from it preserves Go's monotonic clock
	// reading, so a wall-clock step, an NTP correction or a resumed VM, does not
	// register as elapsed time and restart the pod.
	start time.Time
	// lastSuccess is nanoseconds since start rather than a time.Time so that
	// probes running concurrently can read and write it without a lock.
	lastSuccess atomic.Int64
}

// newHealthTracker starts a tracker whose baseline is now. A service that never
// reached the database counts as unhealthy since it started, which is what lets
// liveness fail a pod that came up against a database that was never there.
func newHealthTracker(now func() time.Time, threshold time.Duration) *healthTracker {
	return &healthTracker{now: now, threshold: threshold, start: now()}
}

// recordSuccess marks the database as reached right now, which ends the current
// unhealthy span.
func (t *healthTracker) recordSuccess() {
	t.lastSuccess.Store(int64(t.now().Sub(t.start)))
}

// unhealthyFor reports how long the service has been unable to reach the
// database, counting from the last success or from startup.
func (t *healthTracker) unhealthyFor() time.Duration {
	return t.now().Sub(t.start) - time.Duration(t.lastSuccess.Load())
}

// server answers the health probes. It is the generated ServerInterface, which
// grows an implementation per operation as the contract does.
type server struct {
	db     DB
	health *healthTracker
}

// Healthz answers the liveness probe. It fails only once the database has been
// unreachable for longer than the configured threshold, because restarting the
// pod does not bring a database back: a shorter fuse would turn a database
// outage into a restart loop across every replica.
func (s *server) Healthz(w http.ResponseWriter, r *http.Request) {
	err := s.check(r.Context(), s.db.Ping)
	if err == nil {
		s.health.recordSuccess()
		writeOK(w)
		return
	}

	unhealthy := s.health.unhealthyFor()
	Logger(r.Context()).Warn("liveness probe could not reach the database",
		"error", err, "unhealthy_for", unhealthy.Round(time.Second).String())

	if unhealthy <= s.health.threshold {
		writeOK(w)
		return
	}

	// How long the outage has lasted goes to the log rather than into the body:
	// the probe answers whoever asks, and that is a detail about this service's
	// state rather than about the caller's request.
	problem.Write(w, http.StatusServiceUnavailable, problem.TypeUnavailable,
		"Service unavailable", "the database has been unreachable for too long")
}

// Readyz answers the readiness probe. It fails as soon as the database cannot
// serve this build's queries, which takes this pod out of the load balancer
// while leaving it running.
func (s *server) Readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.check(r.Context(), s.db.Ready); err != nil {
		Logger(r.Context()).Warn("readiness probe could not use the database", "error", err)
		problem.Write(w, http.StatusServiceUnavailable, problem.TypeUnavailable,
			"Service unavailable", "the database is unusable")
		return
	}
	writeOK(w)
}

// check runs one probe's database check under the probe deadline.
func (s *server) check(ctx context.Context, probe func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return probe(ctx)
}

// writeOK sends the plain-text body both probes answer with when they pass.
func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok")
}

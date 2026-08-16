// Package health tracks how long a service's checks have been failing, which is
// what a liveness probe weighs against its threshold before it fails a pod.
package health

import (
	"sync/atomic"
	"time"
)

// Tracker remembers when a service's checks last passed, which is what turns
// single failures into the continuous outage liveness reports on.
type Tracker struct {
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

// New starts a tracker whose baseline is now. A service whose checks never
// passed counts as unhealthy since it started, which is what lets liveness fail
// a pod that came up against a dependency that was never there.
func New(now func() time.Time, threshold time.Duration) *Tracker {
	return &Tracker{now: now, threshold: threshold, start: now()}
}

// Exhausted reports whether the checks have been failing for longer than the
// threshold, which is the rule a liveness probe fails a pod by. It lives here
// rather than at the call sites so that the boundary between an outage that is
// still tolerated and one that is not is written once for every service.
func (t *Tracker) Exhausted() bool { return t.UnhealthyFor() > t.threshold }

// RecordSuccess marks the checks as passing right now, which ends the current
// unhealthy span.
func (t *Tracker) RecordSuccess() {
	t.lastSuccess.Store(int64(t.now().Sub(t.start)))
}

// UnhealthyFor reports how long the checks have been failing, counting from the
// last success or from startup.
func (t *Tracker) UnhealthyFor() time.Duration {
	return t.now().Sub(t.start) - time.Duration(t.lastSuccess.Load())
}

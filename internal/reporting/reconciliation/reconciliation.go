// Package reconciliation is the loop that keeps the projection honest. It asks
// a platform what it currently holds, diffs that observed reality against the
// projection, and feeds the difference back as ordinary events through the
// ingest pipeline. The difference gets no special path: the synthetic events
// take the same deduplication, storage, and projection route a collector's
// events take, so a resource the sync corrected ends up with the same kind of
// history as one that was never missed.
//
// A platform API that stops answering must read as "no information", never as
// "everything was deleted". An adapter that cannot enumerate a resource type
// says so with an EnumerationError and carries on with its remaining types, and
// the missed-delete pass then runs only over the types that finished without
// one. A type that legitimately holds nothing is still a type that finished, so
// the pass needs the positive set of attempted types rather than the set of
// failures, which is what ResourceTypes reports.
//
// A run abandoned by a process crash keeps status='running' in sync_runs
// forever, because nothing is left to rewrite the row. The advisory lock that
// guards the cloud is session-scoped, so the database drops it when the
// connection dies and the next run of that cloud proceeds; the stale row is a
// signal for an operator, not a gate. A sync writes no audit row by design:
// sync_runs is the operational record, and the same run kept in two places
// would only drift apart. That is a deliberate deviation from the rebuild
// handler in internal/reporting/httpapi/rebuild.go, which does audit, because a
// rebuild is an operator's manual act on the projection while a sync is the
// service's own schedule.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.10.
package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"
)

// ErrUnknownCloud is what a sync of a cloud the configuration does not name
// returns. An operator who misspells a cloud learns that instead of getting a
// successful run that reconciled nothing.
var ErrUnknownCloud = errors.New("the configuration names no such cloud")

// ErrAlreadyRunning is what a sync of a cloud whose lock another session holds
// returns. Two syncs of one cloud would diff the same projection rows against
// two overlapping observations, so the second one steps aside rather than
// queuing behind the first: the cron job that asked for it comes back anyway.
var ErrAlreadyRunning = errors.New("a sync of this cloud is already running")

// ObservedResource is one resource as the platform reported it. The adapter has
// already normalized it, so State and Size speak Tally's vocabulary and the
// diff can compare the resource against a projection row without knowing which
// platform it came from.
type ObservedResource struct {
	ResourceType string
	ResourceID   string
	ProjectID    string
	State        string // normalized tally state
	Size         map[string]any
	CreatedAt    *time.Time // real creation time if the API exposes it
	DeletedAt    *time.Time // set only for resources reported as deleted
}

// Adapter is the platform-specific half of a sync, and the only half. What the
// framework does with an adapter's answer (the diff, the synthetic events, the
// run bookkeeping) is identical for every platform, which is what keeps a new
// provider down to one implementation of this interface.
type Adapter interface {
	// Platform is the platform the adapter speaks for. LoadConfig checks it
	// against each configured cloud, so a cloud cannot be wired to an adapter
	// that observes a different platform.
	Platform() string
	// ResourceTypes is the set of resource types this adapter enumerates under
	// cfg. The missed-delete pass runs only over types in this set that
	// finished without an EnumerationError.
	ResourceTypes(cfg map[string]any) ([]string, error)
	// ListResources streams the full live inventory; it MAY also yield
	// recently deleted resources (DeletedAt set) when the platform exposes
	// them. since bounds only that optional deleted listing, never the live
	// one: the diff treats the live stream as complete.
	//
	// at is the instant the run is at. The framework passes the run-local now
	// it dates everything else by, so at is never zero, and how far back the
	// deleted listing reaches is measured from it rather than from a clock the
	// adapter reads for itself.
	ListResources(ctx context.Context, cfg map[string]any, since *time.Time, at time.Time,
	) iter.Seq2[ObservedResource, error]
}

// EnumerationError reports that one resource type could not be fully
// enumerated. An adapter yields it through the error side of the stream and
// continues with its remaining types; any other error aborts the run.
type EnumerationError struct {
	ResourceType string
	Err          error
}

// Error names the resource type that stayed incomplete, because that is what
// decides which rows the run leaves alone.
func (e *EnumerationError) Error() string {
	return fmt.Sprintf("enumerating %s: %v", e.ResourceType, e.Err)
}

// Unwrap exposes the platform's own error, so a caller can still match it with
// errors.Is or errors.As.
func (e *EnumerationError) Unwrap() error {
	return e.Err
}

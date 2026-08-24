package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// LateReport is what DetectLate found: the run the late events are late
// against, and the resources they belong to.
type LateReport struct {
	// RunID is the latest finalized run of the period, the one a correction
	// would diff against.
	RunID uuid.UUID
	// Kind is that run's kind, KindRegular or KindCorrection: the regular run
	// that closed the month, or the last correction finalized over it.
	Kind string
	// SnapshotAt is the instant that run read the reporting database at, in
	// UTC. It is what the events listed here arrived after.
	SnapshotAt time.Time
	// Resources are the resources whose events arrived late, with how many did
	// and when the last of them did. It is empty when nothing arrived late, and
	// holds at most source.LateResourceLimit entries.
	Resources []source.LateResource
	// Truncated is how many further resources have late events beyond the ones
	// Resources names. It is zero where Resources names every one of them.
	Truncated int
}

// DetectLate lists the resources of a finalized period whose events reached the
// reporting database after the latest finalized run of that period read it, and
// is the whole of what tally-engine detect-late does. The engine pool is where
// the finalized run is read from, the reporting database is where the events
// are counted, and neither is closed here.
//
// The run the events are held against is the latest finalized run of the period
// by started_at, which is the one a correction diffs against: the regular run
// that closed the month, and the last finalized correction after that. The
// instant is the snapshot_at that run recorded in its stats.
//
// The report names at most source.LateResourceLimit resources and counts the
// rest in Truncated, because a period finalized before its ingest had settled
// has one of them per resource of the fleet.
//
// No period lock is taken and nothing is written. A report read while a
// correction or a finalization of the period is in flight can therefore name a
// run that is superseded a moment later, and what gives the current answer is
// reading it again once that pass is done.
//
// The comparison against the snapshot time is strict. An ingest transaction
// that began before the snapshot and committed after it carries a received_at
// below it, so its events are not listed here, although a correction of the
// period does re-meter them. The window is the length of an ingest
// transaction; see ListLateEvents in internal/engine/source/queries.sql.
//
// A period without a finalized run yields an error wrapping
// ErrPeriodNotFinalized, and a finalized run whose stats carry no snapshot_at
// one that names that run. A period whose chunks the reporting database has
// compressed is read whole, which its statement_timeout is the only bound on;
// see lateEventsError for what running into it reports.
func DetectLate(
	ctx context.Context,
	engine *pgxpool.Pool,
	reporting *source.DB,
	periodFrom, periodTo time.Time,
) (LateReport, error) {
	var report LateReport
	month := period.Format(periodFrom)

	run, err := sqlcgen.New(engine).LatestFinalizedRun(ctx, timestamptz(periodFrom))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return report, fmt.Errorf(
				"%w: %s has no finalized run, and late events are late against one: "+
					"tally-engine run --period %s and tally-engine finalize produce it",
				ErrPeriodNotFinalized, month, month)
		}
		return report, fmt.Errorf("reading the finalized run of %s: %w", month, err)
	}
	runID := uuid.UUID(run.ID.Bytes)

	var stats Stats
	if err := json.Unmarshal(run.Stats, &stats); err != nil {
		return report, fmt.Errorf("reading the stats of run %s: %w", runID, err)
	}
	if stats.SnapshotAt == nil {
		return report, fmt.Errorf("run %s carries no snapshot_at in its stats", runID)
	}

	snap, err := reporting.Snapshot(ctx)
	if err != nil {
		return report, err
	}
	// The snapshot is closed explicitly once the late events are read; this
	// covers the paths that do not get there. Closing an already closed snapshot
	// reports success, and the context is one no cancellation reaches but a
	// timeout bounds, because a canceled call still gives the reporting
	// connection back and a reporting host that stopped answering must not keep
	// it from doing so.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
		defer cancel()
		_ = snap.Close(closeCtx)
	}()

	resources, total, err := snap.LateEvents(ctx, periodFrom, periodTo, *stats.SnapshotAt)
	if err != nil {
		return report, lateEventsError(month, err)
	}
	if err := snap.Close(ctx); err != nil {
		return report, err
	}

	return LateReport{
		RunID:      runID,
		Kind:       run.Kind,
		SnapshotAt: stats.SnapshotAt.UTC(),
		Resources:  resources,
		Truncated:  total - len(resources),
	}, nil
}

// The SQLSTATE Postgres reports for a canceled statement, and the message it
// carries when the statement_timeout is what canceled it. The code does not say
// that on its own: pg_cancel_backend reports the same one, and a five-minute
// read that decompresses whole chunks is exactly what a DBA or a long-query
// killer cancels. Matching the message beside the code is what
// internal/engine/pricing/store.go does with its constraint name, and it is
// what keeps a deliberately killed query from being reported as this feature's
// timeout. A server whose lc_messages is not English falls through to its own
// error unchanged, which says less rather than something untrue.
const (
	queryCanceled           = "57014"
	statementTimeoutMessage = "canceling statement due to statement timeout"
)

// lateEventsError reports a failed read of the late events of month. Every
// other read the engine issues is served by an index; this one is bounded by
// nothing but the statement_timeout source.New sets, because the chunks of a
// period past the reporting database's 90-day compression policy are segmented
// by (cloud, resource_type) and carry no bounds on received_at, so they are
// decompressed whole to be filtered on it. The LIMIT the query carries bounds
// the rows it returns rather than the events it reads, and does not help.
//
// The timeout is therefore a property of the feature on an old period rather
// than a broken database, and reporting it as the SQLSTATE alone leaves the
// operator with no reading of it. What it says instead is what ran out and what
// books the late events without the report.
//
// That is a correction, and it is named as the path that needs no report rather
// than as a cheaper one. It re-meters the period through one ListHistory per
// candidate resource (internal/engine/metering/metering.go), and resource_id is
// not a segment key, so against the same compressed chunks each of those reads
// decompresses the resource's whole (cloud, resource_type) segment under this
// same statement_timeout. Telling the operator it is bounded would send them
// into a correction run that fails after taking the period lock; the message
// says what it costs instead, and leaves the call to them.
func lateEventsError(month string, err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != queryCanceled ||
		!strings.Contains(pgErr.Message, statementTimeoutMessage) {
		return err
	}
	return fmt.Errorf(
		"reading the late events of %s did not finish inside the reporting database's "+
			"statement timeout, because a period past its compression policy is decompressed "+
			"whole to be filtered on received_at; tally-engine correct --period %s books those "+
			"events without the report, but it is not the cheaper read: it re-meters the period "+
			"one resource at a time, and each of those reads decompresses that resource's whole "+
			"(cloud, resource_type) segment under the same timeout: %w",
		month, month, err)
}

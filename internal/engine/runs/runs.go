// Package runs executes one billing period: it takes the period lock, opens a
// run row, chains metering, counters, rating, attribution and statement
// rendering over one reporting snapshot, and writes what came out under that
// run. What a period is billed as is decided by the packages this one calls;
// what belongs here is the order they run in, the run's bookkeeping, and the
// stats an operator reads a finished run from.
//
// A run either writes its whole output or none of it. The records, the
// supersede of the run it replaces, and the completion are one transaction, so
// a reader of the period never sees two completed runs or a completed run with
// half its records. A failure after the run row exists ends that row as
// 'failed' with the reason in its stats, and the previously completed run keeps
// standing.
//
// A correction (Correct) is a run of kind 'correction' over a finalized period,
// through the same lock, the same run row, the same snapshot and the same write
// transaction. What it adds is the diff: its rated amounts are held against
// those of the latest finalized run of the period, and the differences are
// stored as correction deltas and as one credit note per affected project. The
// report ahead of it is DetectLate, which lists the resources whose events
// arrived after that run read the reporting database.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.8,
// with one deviation from its literal SQL: the roadmap takes the period lock as
// pg_advisory_xact_lock, which lives for one transaction, while a run opens
// several and holds the lock across all of them. The lock is a session lock
// here, taken and released on the one connection Execute holds for its whole
// length (the author's decision of 2026-08-24). It has to be released on that
// same connection: pgxpool does not reset session state when a connection is
// returned, and tally-engine tick executes several periods in one process, so a
// lock left behind would block the next run of that period until the process
// ends. A release that fails takes its connection out of the pool with it,
// because a connection that may still hold the lock is worse than one less.
package runs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b42labs/tally/internal/engine/attribution"
	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// WarningPeriodNotEnded marks a run over a period that has not ended yet. Every
// resource that is still alive is billed to the period's end, so the numbers
// such a run produces are a forecast of the month rather than its invoice.
const WarningPeriodNotEnded = "period_not_ended"

// KindRegular and KindCorrection are the two values runs.kind carries: a
// regular run meters a period, a correction re-meters a finalized one and
// stores the differences. statusFinalized is the period status this package
// refuses to meter.
const (
	KindRegular     = "regular"
	KindCorrection  = "correction"
	statusFinalized = "finalized"
)

// bookkeepingTimeout bounds the work that outlives a run's own context:
// recording a failed run, releasing the period lock, and rolling back the
// reporting snapshot and the write transaction. All of it runs on a context the
// caller's cancellation does not reach, so a canceled run is still written down
// and still gives back what it holds, and the bound is what keeps a database
// that stopped answering from holding the process there. Without it a rollback
// waits on a blackholed host for as long as the TCP stack takes, which outlasts
// the SIGTERM budget of the CronJob's pod and leaves the bookkeeping behind it
// unrun: the run stays 'running' until a reclaim takes it hours later.
const bookkeepingTimeout = 10 * time.Second

// ErrRunInProgress is what Execute returns for a period another process is
// metering. Nothing is written for such a call: the period lock is taken before
// the first row.
var ErrRunInProgress = errors.New("another run of this period is in progress")

// ErrPeriodFinalized is what Execute returns for a period that is closed. A
// regular run leaves it alone, because its records are immutable; what changes
// a finalized period is a correction run (Correct).
var ErrPeriodFinalized = errors.New("the billing period is finalized")

// ErrLockReleaseFailed marks the one failure of a run that leaves its output
// standing: the run itself came through and only the release of the period lock
// did not. The Result beside such an error is the committed run, so a caller
// reports the month as billed and this as a warning rather than losing the run
// id and metering the month again. It marks nothing else: a run that broke
// comes back as the error that broke it, whether its lock was released or not.
//
// A connection pooler in transaction pooling mode is what produces it in
// practice: the session lock lands on one server connection and its release on
// another, so every run of the deployment ends here.
var ErrLockReleaseFailed = errors.New("the period lock could not be released")

// Options is what one run meters.
type Options struct {
	// PeriodFrom and PeriodTo are the half-open interval of the billing month,
	// as internal/engine/period derives it.
	PeriodFrom, PeriodTo time.Time
	// Clouds are the clouds to meter. An empty list meters every cloud the
	// projection knows, which is the only meaning the engine can give it: it
	// keeps no cloud list of its own.
	Clouds []string
	// AttributingRelationTypes are the relation types attribution walks. An
	// empty list is attribution turned off.
	AttributingRelationTypes []string
	// Counters is the counter sources file of the deployment, empty where it
	// measures no counter metric.
	Counters counters.Config
	// VM answers the metricsql counter sources. It is nil unless
	// Counters.HasMetricsQL() reports that any source needs it.
	VM counters.Querier
}

// Result is what one call to Execute did.
type Result struct {
	// RunID is the run that was opened. It is the zero id when the call was
	// refused before any row was written.
	RunID uuid.UUID
	// PricingVersion is the model version the period was rated with, which the
	// CLI prints beside the run.
	PricingVersion string
	// Stats is what the run counted, the same object it stored in runs.stats.
	Stats Stats
	// Superseded are the completed regular runs of the period this one replaced.
	Superseded []uuid.UUID
	// Reclaimed are the runs of the period that were still 'running' with no
	// process behind them and were failed before this run opened.
	Reclaimed []uuid.UUID
}

// Stats is the object a run stores in runs.stats. Every list is left out when
// it is empty, so a clean run reads as the four counts and nothing else.
type Stats struct {
	SnapshotAt           *time.Time                       `json:"snapshot_at,omitempty"`
	Candidates           int                              `json:"candidates"`
	UsageRecords         int                              `json:"usage_records"`
	RatedRecords         int                              `json:"rated_records"`
	Statements           int                              `json:"statements"`
	Warnings             []Warning                        `json:"warnings,omitempty"`
	MeteringWarnings     []metering.Warning               `json:"metering_warnings,omitempty"`
	CounterWarnings      []counters.Warning               `json:"counter_warnings,omitempty"`
	AttributionWarnings  []attribution.Warning            `json:"attribution_warnings,omitempty"`
	Unpriced             []rating.UnpricedResourceType    `json:"unpriced,omitempty"`
	Unreadable           []rating.UnreadableQuantity      `json:"unreadable,omitempty"`
	UnregisteredProjects []statements.UnregisteredProject `json:"unregistered_projects,omitempty"`
	Violations           []metering.ResourceViolations    `json:"violations,omitempty"`
	Error                string                           `json:"error,omitempty"`
}

// Warning is a finding about the run itself, beside the warnings the passes it
// chains report under their own names. Code is WarningPeriodNotEnded.
type Warning struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// Execute meters, rates and records one billing period, and is the whole of
// what tally-engine run does. The engine pool is where the run is written, the
// reporting database is where it is read from, and neither is closed here.
//
// The period is locked for the length of the call. A period another process is
// already metering yields an error wrapping ErrRunInProgress, and a finalized
// one an error wrapping ErrPeriodFinalized; neither writes a row. Any failure
// once the run row exists ends that row as 'failed' with the reason in its
// stats and comes back as the error that caused it, so the caller reports what
// went wrong rather than that something did. The one exception is the release
// of the period lock: it happens after the run is committed, so a failure there
// comes back wrapping ErrLockReleaseFailed beside the Result of a run that is
// done.
func Execute(ctx context.Context, engine *pgxpool.Pool, reporting *source.DB, opts Options) (Result, error) {
	var result Result
	retry := "tally-engine run --period " + period.Format(opts.PeriodFrom)
	err := withPeriodLock(ctx, engine, opts.PeriodFrom, retry, func() (err error) {
		result, err = execute(ctx, engine, reporting, opts)
		return err
	})
	return result, err
}

// Reclaim fails the runs of one period whose process is gone and returns the
// ids it took. It is what a caller that does not meter the period itself
// reclaims through: the scheduler counts a month's failed runs before it
// decides whether to meter it again, and a killed run that nothing has failed
// yet counts as no failure at all.
//
// The period is locked the way Execute locks it, and for the same reason. What
// stands for the missing process is the age of the run row, and that age bounds
// no run of tally-engine run --period: a month large enough to meter for longer
// than it would be failed underneath the process that is still metering it, and
// the write that process is heading for would then be discarded whole. Under
// the lock a run that still reads as 'running' has no process behind it.
//
// A period another process is metering yields an error wrapping
// ErrRunInProgress and nothing is written, which is the answer the reclaim was
// asking for: that run is the process the age was standing in for.
func Reclaim(ctx context.Context, engine *pgxpool.Pool, periodFrom time.Time) ([]uuid.UUID, error) {
	var reclaimed []uuid.UUID
	month := period.Format(periodFrom)
	err := withPeriodLock(ctx, engine, periodFrom, "tally-engine tick", func() error {
		ids, err := sqlcgen.New(engine).ReclaimStaleRuns(ctx, timestamptz(periodFrom))
		if err != nil {
			return fmt.Errorf("reclaiming the stale runs of %s: %w", month, err)
		}
		reclaimed = uuidsOf(ids)
		return nil
	})
	return reclaimed, err
}

// withPeriodLock runs fn under the period lock of periodFrom. retryHint is the
// command the caller is retried with, which the refusal of a period another
// process holds names.
func withPeriodLock(
	ctx context.Context,
	engine *pgxpool.Pool,
	periodFrom time.Time,
	retryHint string,
	fn func() error,
) (err error) {
	month := period.Format(periodFrom)

	// One connection for the whole call: the lock below is session scoped, so
	// the release has to reach the session that took it.
	conn, err := engine.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring the connection of the %s period lock: %w", month, err)
	}
	defer conn.Release()

	// The key is the period start as text, rendered here rather than by the
	// server: see TryLockPeriod in internal/engine/store/queries.sql.
	key := periodFrom.UTC().Format(time.RFC3339)
	locked, err := sqlcgen.New(conn).TryLockPeriod(ctx, key)
	if err != nil {
		return fmt.Errorf("locking the period %s: %w", month, err)
	}
	if !locked {
		return fmt.Errorf(
			"%w: %s is being metered by another process, and %s is retried once that run has ended",
			ErrRunInProgress, month, retryHint)
	}
	defer func() {
		// On the connection the lock was taken on, and on a context the caller's
		// cancellation does not reach: a lock this call keeps is one the next run
		// of the period waits behind.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
		defer cancel()

		var lockErr error
		released, unlockErr := sqlcgen.New(conn).UnlockPeriod(unlockCtx, key)
		switch {
		case unlockErr != nil:
			// Whether the session still holds the lock is exactly what this error
			// leaves open, and pgxpool resets no session state: a connection put
			// back holding it hands the lock to whoever draws it next, and every
			// further run of this period in this process then fails as if another
			// one were metering it. Hijacking takes the connection out of the pool
			// and closing it ends the session, which releases the lock for
			// certain. The conn.Release() above becomes a no-op once hijacked.
			// Whether the close itself succeeds changes nothing: the connection is
			// out of the pool either way, and its socket is gone with it.
			_ = conn.Hijack().Close(unlockCtx)
			lockErr = fmt.Errorf("unlocking the period %s: %w", month, unlockErr)
		case !released:
			// The session did not hold the lock, so the connection is clean. That
			// it got here at all means the lock was lost underneath the run, or
			// that a pooler put its release on another server connection than its
			// acquisition.
			lockErr = fmt.Errorf("unlocking the period %s: the session no longer held the lock", month)
		default:
			return
		}
		// The lock's bookkeeping is not the run: fn either wrote its whole output
		// or none of it, and neither branch above touches that. Where fn came
		// through, the sentinel is what tells the caller it still has a billed
		// month and a run id to report; where it did not, its own error stays
		// what the call failed with and this is joined beside it.
		if err == nil {
			err = fmt.Errorf("%w: %w", ErrLockReleaseFailed, lockErr)
			return
		}
		err = errors.Join(err, lockErr)
	}()

	return fn()
}

// execute is Execute under the period lock: everything from the billing period
// row to the completed run.
func execute(ctx context.Context, engine *pgxpool.Pool, reporting *source.DB, opts Options) (Result, error) {
	var result Result
	q := sqlcgen.New(engine)
	month := period.Format(opts.PeriodFrom)

	// The period is recorded before it is read, so that the first run of a month
	// finds the row it is about to check the status of. A month that is already
	// known keeps the status it has: metering a period moves no period status.
	if err := q.UpsertBillingPeriod(ctx, sqlcgen.UpsertBillingPeriodParams{
		PeriodFrom: timestamptz(opts.PeriodFrom),
		PeriodTo:   timestamptz(opts.PeriodTo),
	}); err != nil {
		return result, fmt.Errorf("recording the billing period %s: %w", month, err)
	}
	billingPeriod, err := q.GetBillingPeriod(ctx, timestamptz(opts.PeriodFrom))
	if err != nil {
		return result, fmt.Errorf("reading the billing period %s: %w", month, err)
	}
	if billingPeriod.Status == statusFinalized {
		return result, fmt.Errorf(
			"%w: %s was closed by run %s, and a finalized period is changed with tally-engine correct --period %s",
			ErrPeriodFinalized, month, uuid.UUID(billingPeriod.FinalizedRunID.Bytes), month)
	}

	// Under the period lock a run of this period that still reads as running has
	// no process behind it: whatever opened it is gone.
	reclaimed, err := q.ReclaimStaleRuns(ctx, timestamptz(opts.PeriodFrom))
	if err != nil {
		return result, fmt.Errorf("reclaiming the stale runs of %s: %w", month, err)
	}
	result.Reclaimed = uuidsOf(reclaimed)

	// The prices are resolved before the run row exists, so a period nothing
	// prices leaves no run behind to explain.
	model, err := pricing.ForPeriod(ctx, q, opts.PeriodFrom)
	if err != nil {
		return result, err
	}
	result.PricingVersion = model.Version

	// A nil cloud list is bound as the empty array rather than passed on: pgx
	// encodes nil as SQL NULL and runs.clouds is NOT NULL. Both spellings mean
	// every cloud, which is what the column's default says.
	clouds := opts.Clouds
	if clouds == nil {
		clouds = []string{}
	}
	// This insert commits on its own, so a process killed from here on leaves a
	// 'running' row for the next run of the period to reclaim above. The zero
	// CorrectsRunID binds NULL, which is what a run that corrects nothing stores.
	run, err := q.InsertRun(ctx, sqlcgen.InsertRunParams{
		PeriodFrom:     timestamptz(opts.PeriodFrom),
		PeriodTo:       timestamptz(opts.PeriodTo),
		Kind:           KindRegular,
		PricingVersion: pgtype.Text{String: model.Version, Valid: true},
		Clouds:         clouds,
	})
	if err != nil {
		return result, fmt.Errorf("opening the run of %s: %w", month, err)
	}
	result.RunID = uuid.UUID(run.ID.Bytes)

	var stats Stats
	superseded, err := produce(ctx, engine, reporting, opts, model, result.RunID, &stats)
	if err != nil {
		stats.Error = err.Error()
		result.Stats = stats
		return result, recordFailure(ctx, engine, result.RunID, stats, err)
	}
	result.Stats = stats
	result.Superseded = superseded
	return result, nil
}

// produce meters, rates, renders and writes one run, and returns the completed
// runs it superseded. stats is filled as the passes report, so a failure
// carries what the run got to rather than an empty object.
func produce(
	ctx context.Context,
	engine *pgxpool.Pool,
	reporting *source.DB,
	opts Options,
	model pricing.Model,
	runID uuid.UUID,
	stats *Stats,
) ([]uuid.UUID, error) {
	metered, projects, relations, err := meter(ctx, reporting, opts, stats)
	if err != nil {
		return nil, err
	}
	warnPeriodNotEnded(stats, opts.PeriodTo)

	// The rest of the pass is arithmetic over what the snapshot handed out: it
	// reads nothing and runs with the reporting connection already given back.
	rated := rating.Rate(model, metered.Resources)
	stats.Unpriced = rated.Unpriced
	stats.Unreadable = rated.Unreadable

	resolution := attribution.Resolve(projects, relations)
	stats.AttributionWarnings = resolution.Warnings

	built, err := statements.Build(opts.PeriodFrom, opts.PeriodTo, metered.Resources, rated, projects, resolution, nil)
	if err != nil {
		return nil, err
	}
	stats.UnregisteredProjects = built.Unregistered

	usage, ids, err := usageRows(runID, metered.Resources)
	if err != nil {
		return nil, err
	}
	amounts, err := ratedRows(runID, rated, ids)
	if err != nil {
		return nil, err
	}
	stats.UsageRecords = len(usage)
	stats.RatedRecords = len(amounts)
	stats.Statements = len(built.Statements)

	payload, err := json.Marshal(*stats)
	if err != nil {
		return nil, fmt.Errorf("marshalling the stats of run %s: %w", runID, err)
	}
	return write(ctx, engine, opts.PeriodFrom, runID, KindRegular, usage, amounts, nil, built.Statements, payload)
}

// warnPeriodNotEnded warns about a month that has not ended, which is metered
// as if it had: metering clips every interval that is still open to the
// period's end, so the amounts are the whole month's rather than the part of it
// that has passed.
func warnPeriodNotEnded(stats *Stats, periodTo time.Time) {
	if !periodTo.After(time.Now()) {
		return
	}
	stats.Warnings = append(stats.Warnings, Warning{
		Code: WarningPeriodNotEnded,
		Detail: fmt.Sprintf(
			"period_to %s has not passed yet, so every resource that is still alive is billed for the whole month",
			periodTo.UTC().Format(time.RFC3339)),
	})
}

// meter reads everything one run takes from the reporting database: the drafts
// of the period, the counter metrics measured into them, and the project graph
// they are attributed over.
//
// It holds the snapshot open for exactly those reads. A counter measures its
// events through the same snapshot the history was folded from, so a counter
// and the drafts it slices into see the same data, and the graph is read before
// the connection is given back rather than while the period is being rated.
func meter(ctx context.Context, reporting *source.DB, opts Options, stats *Stats) (
	*metering.Result, []source.Project, []source.Relation, error,
) {
	snap, err := reporting.Snapshot(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	// The snapshot is closed explicitly once the graph is read; this covers the
	// paths that do not get there. Closing an already closed snapshot reports
	// success, and the context is one no cancellation reaches but a timeout
	// bounds, because a canceled run still has to give the reporting connection
	// back and a reporting host that stopped answering must not keep it from
	// writing its failed run down.
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
		defer cancel()
		_ = snap.Close(closeCtx)
	}()

	at := snap.At
	stats.SnapshotAt = &at

	metered, err := metering.Meter(ctx, snap, opts.PeriodFrom, opts.PeriodTo, opts.Clouds)
	if err != nil {
		// The violating resources are what an operator fixes the period from, so
		// they go into the stats of the failed run rather than into a log line.
		var violation *metering.ViolationError
		if errors.As(err, &violation) {
			stats.Violations = violation.Resources
		}
		return nil, nil, nil, err
	}
	stats.Candidates = metered.Candidates
	stats.MeteringWarnings = metered.Warnings

	measurer, err := counters.New(opts.Counters, snap, opts.VM)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("preparing the counter sources: %w", err)
	}
	counterWarnings, err := measurer.Apply(ctx, metered.Resources)
	if err != nil {
		return nil, nil, nil, err
	}
	stats.CounterWarnings = counterWarnings

	projects, err := snap.Projects(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	relations, err := snap.Relations(ctx, opts.AttributingRelationTypes, opts.PeriodFrom, opts.PeriodTo)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := snap.Close(ctx); err != nil {
		return nil, nil, nil, err
	}
	return metered, projects, relations, nil
}

// write stores one run's output and ends the run, in one transaction: the
// records, the supersede of the runs this one replaces, and the completion
// become visible together, so no reader of the period ever sees two completed
// runs or a completed run whose records are still arriving. It is the one write
// transaction of a regular run and of a correction. What tells the two apart is
// kind, which decides whether the period's completed regular run or its
// completed correction is superseded, and the deltas, which only a correction
// carries. The stats arrive marshalled, because the two kinds store different
// objects under them.
//
// The run row is taken FOR NO KEY UPDATE first. Every record insert fires the
// trigger that locks it FOR SHARE, and escalating that lock afterwards
// deadlocks two writers of one run, which is the ordering the comment on
// forbid_finalized_mutation in migration 0001 asks for.
func write(
	ctx context.Context,
	engine *pgxpool.Pool,
	periodFrom time.Time,
	runID uuid.UUID,
	kind string,
	usage []sqlcgen.CreateUsageRecordsParams,
	amounts []sqlcgen.CreateRatedRecordsParams,
	deltas []sqlcgen.CreateCorrectionDeltasParams,
	sts []statements.Statement,
	payload []byte,
) ([]uuid.UUID, error) {
	tx, err := engine.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening the write transaction of run %s: %w", runID, err)
	}
	// The rollback of the failure paths, on a context no cancellation reaches but
	// a timeout bounds: a canceled run gives its row locks back rather than
	// leaving them to the connection's teardown, and an engine database that
	// stopped answering must not hold the process here past the bookkeeping that
	// follows. Rolling a committed transaction back is a no-op that reports
	// pgx.ErrTxClosed, which is why the error is dropped.
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()

	q := sqlcgen.New(tx)
	if _, err := q.LockRun(ctx, uuidValue(runID)); err != nil {
		return nil, fmt.Errorf("locking the run %s: %w", runID, err)
	}
	if _, err := q.CreateUsageRecords(ctx, usage); err != nil {
		return nil, fmt.Errorf("writing the usage records of run %s: %w", runID, err)
	}
	if _, err := q.CreateRatedRecords(ctx, amounts); err != nil {
		return nil, fmt.Errorf("writing the rated records of run %s: %w", runID, err)
	}
	if len(deltas) > 0 {
		if _, err := q.CreateCorrectionDeltas(ctx, deltas); err != nil {
			return nil, fmt.Errorf("writing the correction deltas of run %s: %w", runID, err)
		}
	}
	if err := statements.Persist(ctx, q, runID, sts); err != nil {
		return nil, err
	}
	superseded, err := q.SupersedeCompletedRuns(ctx, sqlcgen.SupersedeCompletedRunsParams{
		PeriodFrom: timestamptz(periodFrom),
		Kind:       kind,
	})
	if err != nil {
		return nil, fmt.Errorf("superseding the completed runs of %s: %w", period.Format(periodFrom), err)
	}
	// A run this process still believes it owns can have been reclaimed while it
	// was metering, and the reclaim is the period's decision: writing over it
	// would leave the period with two completed runs of this kind, one of them
	// declared dead. The rollback of this transaction takes the records with it.
	completed, err := q.CompleteRun(ctx, sqlcgen.CompleteRunParams{ID: uuidValue(runID), Stats: payload})
	if err != nil {
		return nil, fmt.Errorf("completing the run %s: %w", runID, err)
	}
	if completed == 0 {
		return nil, fmt.Errorf("the run %s was reclaimed while it was still metering, so its records are discarded", runID)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing the run %s: %w", runID, err)
	}
	return uuidsOf(superseded), nil
}

// recordFailure ends a run that broke and returns what Execute or Correct fails
// with, which is the cause and nothing else while the bookkeeping itself holds.
// The stats it writes are the ones the run got to, error included, so a failed
// run is read for how far it came. They are taken as they are given, a Stats or
// a CorrectionStats, because what a run counts is what its own kind counts.
//
// The write runs on a context the caller's cancellation does not reach: a run
// canceled mid-pass is a run that has to be written down as failed, and one
// left at 'running' would block its period until the next run reclaims it.
// Nothing here touches the run this one was going to supersede, because the
// supersede only ever runs inside the write transaction that completes a run.
func recordFailure(ctx context.Context, engine *pgxpool.Pool, runID uuid.UUID, stats any, cause error) error {
	failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer cancel()

	payload, err := json.Marshal(stats)
	if err != nil {
		return errors.Join(cause, fmt.Errorf("marshalling the stats of the failed run %s: %w", runID, err))
	}
	failed, err := sqlcgen.New(engine).FailRun(failCtx, sqlcgen.FailRunParams{
		ID:    uuidValue(runID),
		Stats: payload,
	})
	if err != nil {
		return errors.Join(cause, fmt.Errorf("recording the failed run %s: %w", runID, err))
	}
	// The run had already been moved off 'running', so its stats carry something
	// other than this failure and the two do not match. Which of the two ways
	// that happened is what the stored status says, and only it: a reclaim left
	// the row 'failed' with the reclaim's reason, while a commit whose
	// acknowledgement was lost -- a connection that dropped after the server had
	// taken it -- left it 'completed' with the period's numbers. Naming either
	// here would state one of them as fact in a billing audit trail.
	if failed == 0 {
		return errors.Join(cause, fmt.Errorf(
			"the run %s is no longer 'running', so this failure was not recorded on it: what became of it is the status its row carries",
			runID))
	}
	return cause
}

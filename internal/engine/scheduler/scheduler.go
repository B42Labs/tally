// Package scheduler is the hourly tick of the engine: it walks the billing
// months that have ended, moves each of them from open into grace, and has the
// month whose grace window has passed metered and, where the deployment asks
// for it, closed. It is the whole of what tally-engine tick does.
//
// The metering half is injected as an Executor rather than called here. A run
// needs the reporting database, the pricing model and the counter sources, all
// of which the CLI already assembles for tally-engine run. Finalization touches
// the engine database alone, so runs.Finalize is called directly.
//
// The horizon of the walk comes from the stored state (the author's decision of
// 2026-08-24): it starts at the earliest billing_periods row and ends at the
// last month that has ended. It is neither a configured number of months back
// nor a question put to the reporting database. A CronJob that was down for
// months therefore catches up on its own, as long as one period row exists, and
// a month older than every stored period enters the walk by being run once with
// tally-engine run --period, which writes its row. Its length is capped at
// maxTickMonths all the same: nothing bounds how old the earliest row is, and a
// walk of thousands of months is a tick that never ends.
//
// The two steps of the state machine are made against the status a month is
// stored with, so a month that has just ended moves into grace on one tick and
// is metered on a later one. At an hourly tick that costs an hour.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.8,
// its scheduler block.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// The billing period statuses the tick reads and writes: a month is 'open'
// while it is being billed for the first time, 'grace' while late events are
// still waited for, and 'finalized' once a run has closed it.
const (
	statusOpen      = "open"
	statusGrace     = "grace"
	statusFinalized = "finalized"
)

// transitionToGrace is what a month's report says when this tick ended its
// open phase. It is the only status change the tick writes itself.
const transitionToGrace = "open -> grace"

// maxTickMonths bounds the walk of one tick, counted back from the last month
// that has ended. The walk starts at the earliest stored period and nothing
// bounds how far back that is: period.Parse takes any four-digit year, so a
// mistyped tally-engine run --period writes a period row centuries back that no
// subcommand removes. Every following tick would then walk tens of thousands of
// months, and with concurrencyPolicy: Forbid a tick that does not end is billing
// that never runs again. Three years is longer than any outage a catch-up is
// meant to cover; months older than that are metered with tally-engine run.
const maxTickMonths = 36

// The retries of a month whose runs keep failing: nothing waits after the first
// failure, and from the second on the wait doubles with every further one up to
// maxRetryDelay. See retryDelay.
const (
	baseRetryDelay = time.Hour
	maxRetryDelay  = 24 * time.Hour
)

// Executor meters one billing period and returns the run it recorded. It is
// what the CLI wires runs.Execute into: everything a run needs beside the
// engine database is the caller's, and the tick decides only which months it is
// called for.
type Executor func(ctx context.Context, periodFrom, periodTo time.Time) (uuid.UUID, error)

// Options is how one tick treats the months it walks.
type Options struct {
	// GraceHours is how long a month that has ended waits before it is metered,
	// so that events which arrive late are still billed into it.
	GraceHours int
	// AutoFinalize closes a month over its completed run without an operator.
	// It is off where the numbers are reviewed before the month becomes
	// immutable.
	AutoFinalize bool
	// Execute meters one month. It is called at most once per month per tick.
	Execute Executor
}

// Report is what one tick did: one entry per month it walked, in the order it
// walked them. A month where nothing was due carries an entry too, so the
// report says which months were looked at rather than only which ones moved.
type Report []MonthReport

// MonthReport is what one tick did with one month.
type MonthReport struct {
	// Month is the billing month, as YYYY-MM.
	Month string
	// Transition is the period status change this tick wrote, empty where it
	// wrote none. The only one it writes is "open -> grace".
	Transition string
	// RunID is the run this tick metered or closed the month over, the zero id
	// where it did neither.
	RunID uuid.UUID
	// Finalized says the tick closed the month over a completed run.
	Finalized bool
	// RetryAfter is when a month whose last runs failed is metered again, and
	// Failures is how many of them failed in a row. Both are zero where this
	// tick did not hold the month back.
	RetryAfter time.Time
	Failures   int
	// SkippedBefore is how many billing months older than this one the walk left
	// out, and is set on the oldest month of the walk alone. The walk is capped
	// at maxTickMonths, so a period row further back than that -- a database
	// restored from an old backup, an installation re-enabled after a long
	// shutdown -- names months no tick ever reaches. Reporting the count is what
	// keeps them from being dropped without a trace; they are billed with
	// tally-engine run --period.
	SkippedBefore int
	// Warning is what went wrong beside the month's step rather than in it: the
	// month got where it was going and this names what did not. The one the tick
	// reports is a run that could not give its period lock back, which leaves the
	// run and the records it committed standing.
	Warning error
	// Err is what went wrong with this month. The fields above still carry what
	// the month got to before it failed.
	Err error
}

// Month is one billing period of the walk: the half-open interval [From, To) of
// a UTC calendar month, the way internal/engine/period derives it.
type Month struct {
	From, To time.Time
}

// Tick walks the billing months that have ended and moves each of them one
// step, and is the whole of what tally-engine tick does. now is the clock, an
// argument rather than a reading, which is what makes the state machine
// testable; the CLI passes time.Now().UTC().
//
// A month whose step fails is reported in its own MonthReport.Err and the walk
// goes on with the months after it: one month nothing prices must not keep the
// rest from being billed. The error Tick returns joins those failures, so the
// exit status of the CronJob still reports them. A canceled context ends the
// walk where it is and comes back as context.Canceled.
func Tick(ctx context.Context, engine *pgxpool.Pool, now time.Time, opts Options) (Report, error) {
	first, err := sqlcgen.New(engine).EarliestBillingPeriod(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the earliest billing period: %w", err)
	}
	// The aggregate answers an empty table with one NULL row, which is a
	// deployment that has not billed a month yet.
	var earliest *time.Time
	if first.Valid {
		earliest = &first.Time
	}

	var (
		report   Report
		failures []error
	)
	walk, skipped := monthsDue(earliest, now)
	for i, due := range walk {
		// A tick that was told to stop stops between months rather than in the
		// middle of one: the month it is on has already written what it wrote.
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}

		month, err := tickMonth(ctx, engine, due, now, opts)
		if err != nil {
			month.Err = err
			failures = append(failures, err)
		}
		// The months the cap left out are named on the oldest month that was
		// walked, which is where they end. They are not an error: the cap is what
		// keeps a mistyped --period from making every later tick unbounded, and a
		// tick that failed on it would stay failed for as long as that row stands.
		if i == 0 {
			month.SkippedBefore = skipped
		}
		report = append(report, month)
	}
	return report, errors.Join(failures...)
}

// tickMonth moves one month one step: it records the period, and then either
// ends its open phase or, once the grace window has passed, has it metered and
// closed. The report it returns carries what the month got to, on the failure
// path as well.
func tickMonth(ctx context.Context, engine *pgxpool.Pool, due Month, now time.Time, opts Options) (MonthReport, error) {
	q := sqlcgen.New(engine)
	month := period.Format(due.From)
	report := MonthReport{Month: month}

	// The period is recorded before it is read, so that a month the engine has
	// never seen has the row whose status the steps below are made against. A
	// month that is already known keeps the status it carries.
	if err := q.UpsertBillingPeriod(ctx, sqlcgen.UpsertBillingPeriodParams{
		PeriodFrom: timestamptz(due.From),
		PeriodTo:   timestamptz(due.To),
	}); err != nil {
		return report, fmt.Errorf("recording the billing period %s: %w", month, err)
	}
	billingPeriod, err := q.GetBillingPeriod(ctx, timestamptz(due.From))
	if err != nil {
		return report, fmt.Errorf("reading the billing period %s: %w", month, err)
	}

	switch billingPeriod.Status {
	case statusOpen:
		// The 'now >= period_to' of the state machine is what put the month into
		// the walk: monthsDue stops at the last month that has ended, so every
		// month reaching here has ended.
		if err := q.SetBillingPeriodStatus(ctx, sqlcgen.SetBillingPeriodStatusParams{
			PeriodFrom: timestamptz(due.From),
			Status:     statusGrace,
		}); err != nil {
			return report, fmt.Errorf("moving the billing period %s into grace: %w", month, err)
		}
		report.Transition = transitionToGrace

	case statusGrace:
		// Inside the grace window nothing happens, which is also what keeps a
		// month that was already metered open for a re-run until it closes.
		if now.Before(due.To.Add(time.Duration(opts.GraceHours) * time.Hour)) {
			return report, nil
		}
		if err := bill(ctx, engine, due, now, opts, &report); err != nil {
			return report, err
		}

	case statusFinalized:
		// The month is closed and its records are immutable. What changes a
		// finalized period is a correction run (WP 3.9), never the tick.
	}
	return report, nil
}

// bill is the grace step of one month: the run it does not have yet and, where
// the deployment closes months on its own, the finalization over the run that
// meters it. The order is what lets one tick do both for a month that was
// metered by this very tick. A month whose runs keep failing is left for later
// instead, which is deferMetering's decision.
func bill(ctx context.Context, engine *pgxpool.Pool, due Month, now time.Time, opts Options, report *MonthReport) error {
	q := sqlcgen.New(engine)
	month := report.Month

	metered, err := q.HasCompleteRegularRun(ctx, timestamptz(due.From))
	if err != nil {
		return fmt.Errorf("reading the runs of %s: %w", month, err)
	}
	// A month that already carries a completed or finalized regular run is not
	// metered again. The tick runs hourly, and re-metering a month is an
	// operator's decision (tally-engine run --period).
	if !metered {
		// The runs whose process is gone are reclaimed before the failures are
		// counted rather than by the run below, which is what turns them into
		// failures at all: a tick that was OOM killed mid-run leaves a 'running'
		// row, and a row nothing has failed yet counts as nothing. Counting after
		// the reclaim is what has the backoff see the failure the tick before
		// caused instead of metering the month again on every pass.
		//
		// It goes through runs.Reclaim rather than the query, so that it is taken
		// under the period lock: what the query stands the missing process on is
		// the age of the run row, and nothing bounds how long a tally-engine run
		// --period of a large month takes.
		_, err := runs.Reclaim(ctx, engine, due.From)
		switch {
		// The month is being metered right now, which is what the reclaim was
		// asking about: nothing of it is stale, and every step below is refused by
		// the same lock. The run that process leaves is the next tick's.
		case errors.Is(err, runs.ErrRunInProgress):
			return nil
		// The reclaim came through and only the release of the lock did not. That
		// is the deployment's connection pooler rather than this month, and the
		// run below reports it on its own account, so the month goes on being
		// billed instead of never being billed at all.
		case err != nil && !errors.Is(err, runs.ErrLockReleaseFailed):
			return fmt.Errorf("reclaiming the stale runs of %s: %w", month, err)
		}
		deferred, err := deferMetering(ctx, q, due.From, now, report)
		if err != nil {
			return fmt.Errorf("reading the failed runs of %s: %w", month, err)
		}
		// The month has nothing to close over either: it is held back exactly
		// because it carries no run that got to the end.
		if deferred {
			return nil
		}
		runID, err := opts.Execute(ctx, due.From, due.To)
		// A run that committed and then failed to give its period lock back is a
		// billed month: its records stand, and failing the month here would drop
		// the run id, meter the month again on the next pass, and leave the hourly
		// CronJob red for a month that is done.
		if err != nil && !errors.Is(err, runs.ErrLockReleaseFailed) {
			return fmt.Errorf("metering %s: %w", month, err)
		}
		report.RunID = runID
		if err != nil {
			report.Warning = fmt.Errorf("metering %s: %w", month, err)
			// The lock this run did not give back is the one the finalization
			// below takes, and taking it again in the same breath is how a month
			// that was just billed comes back as ErrRunInProgress and reddens the
			// hourly CronJob anyway. Closing it is the next tick's, which finds
			// the completed run and a lock nobody kept.
			return nil
		}
	}
	if !opts.AutoFinalize {
		return nil
	}

	runID, err := q.LatestCompletedRegularRun(ctx, timestamptz(due.From))
	if err != nil {
		// A month with no completed run has nothing to close over: its run
		// failed, or another process superseded it between these two queries.
		// The next tick meters it again.
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("reading the completed run of %s: %w", month, err)
	}
	finalized := uuid.UUID(runID.Bytes)
	if _, err := runs.Finalize(ctx, engine, due.From, finalized); err != nil {
		return fmt.Errorf("closing %s: %w", month, err)
	}
	report.Finalized = true
	// The tick's output names the run it closed. A month an earlier tick metered
	// leaves the execution above unrun, so the run being closed is the only id
	// this month has.
	if report.RunID == uuid.Nil {
		report.RunID = finalized
	}
	return nil
}

// deferMetering reports whether this tick leaves a month whose runs keep failing
// alone, and writes the wait into the month's report where it does. A month
// nothing prices, or one whose history carries an ordering no later event
// repairs, fails every run it is given, and metering it hourly for as long as
// the deployment stands costs a full pass over the reporting database and
// another stats blob every hour. The report is what turns such a month from a
// silently churning one into one an operator sees.
func deferMetering(
	ctx context.Context,
	q *sqlcgen.Queries,
	periodFrom time.Time,
	now time.Time,
	report *MonthReport,
) (bool, error) {
	failed, err := q.ConsecutiveFailedRuns(ctx, timestamptz(periodFrom))
	if err != nil {
		return false, err
	}
	// A month that never ran, or whose last run got to the end, is due now. The
	// instant is null in exactly that first case.
	delay := retryDelay(int(failed.Failures))
	if delay == 0 || !failed.LastFailure.Valid {
		return false, nil
	}
	retryAfter := failed.LastFailure.Time.Add(delay)
	if !now.Before(retryAfter) {
		return false, nil
	}
	report.RetryAfter = retryAfter
	report.Failures = int(failed.Failures)
	return true, nil
}

// retryDelay is how long a month waits after its last failed run before the tick
// meters it again: nothing after the first failure, and from the second on the
// wait doubles with every further one up to a day. The tick is hourly, so a
// month whose failure is a database that was down is retried at the next pass
// and the one after it, while a month that will never succeed settles at one
// pass a day instead of twenty-four.
func retryDelay(failures int) time.Duration {
	switch {
	case failures < 2:
		return 0
	// Past five the doubling is above the cap anyway, and the count it would be
	// shifted by is whatever the table holds.
	case failures > 5:
		return maxRetryDelay
	}
	return baseRetryDelay << (failures - 1)
}

// monthsDue is the months one tick walks, in order: from the month of earliest,
// the first billing period the engine knows, through the last month that has
// ended at now. earliest is nil where no period is stored, and the walk is then
// the single month before now, which is what a first tick of a fresh deployment
// is there for.
//
// earliest is anchored on the month it falls in, so a period_from that is not a
// month start still walks whole months. A start later than the last month that
// has ended walks nothing: a deployment whose only period is the running one
// has nothing due yet. A start further back than maxTickMonths is moved up to
// it, so the walk of one tick is bounded whatever the table holds, and the
// second return is how many months that moving up dropped: no tick ever reaches
// them again, so a walk that says nothing about them is one an operator can
// only find by reading tally-engine periods against what has been billed.
func monthsDue(earliest *time.Time, now time.Time) (due []Month, skipped int) {
	// period_to is the first instant of the next month, so an instant exactly on
	// a month boundary belongs to the month starting there, and the month before
	// it is the last one that has ended.
	end := monthStart(now).AddDate(0, -1, 0)
	start := end
	if earliest != nil {
		start = monthStart(*earliest)
	}
	// The walk is the most recent maxTickMonths months of that range and nothing
	// older: a period row from a mistyped --period is centuries back, and the
	// months that are actually being billed have to stay inside every tick.
	if horizon := end.AddDate(0, -(maxTickMonths - 1), 0); start.Before(horizon) {
		skipped = monthsBetween(start, horizon)
		start = horizon
	}

	for from := start; !from.After(end); from = from.AddDate(0, 1, 0) {
		due = append(due, Month{From: from, To: from.AddDate(0, 1, 0)})
	}
	return due, skipped
}

// monthsBetween is how many whole months lie between two month starts, from
// included and to left out.
func monthsBetween(from, to time.Time) int {
	return (to.Year()-from.Year())*12 + int(to.Month()-from.Month())
}

// monthStart is the first instant of the UTC month an instant falls in. The
// instant is converted first, because a timestamp read back from the database
// carries the zone of the connection rather than UTC.
func monthStart(t time.Time) time.Time {
	utc := t.UTC()
	return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// timestamptz maps an instant to the query parameter.
func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

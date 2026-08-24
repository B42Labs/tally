package runs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// statusCompleted is the run status a period is closed over: a run that got to
// the end of its pass and has all its records.
const statusCompleted = "completed"

// ErrRunNotFound is what Finalize returns for a run id no row carries. Nothing
// is written for such a call.
var ErrRunNotFound = errors.New("the run does not exist")

// ErrRunNotCompleted is what Finalize returns for a run that did not get to the
// end of its pass. A period is billed from the records of a completed run, so a
// running, failed or superseded one closes nothing.
var ErrRunNotCompleted = errors.New("the run is not completed")

// ErrPeriodNotFinalized is what Finalize returns for a correction run of a
// period that is not closed, and what Correct returns for a period it is asked
// to correct that is not closed. A correction exists only over a finalized
// month: what it books is what arrived after the run that billed that month,
// and an open month is billed by metering it again.
var ErrPeriodNotFinalized = errors.New("the billing period is not finalized")

// ErrPeriodMismatch is what Finalize returns for a run of another month than
// the one being closed. Both months are named: the pair reaches the engine from
// two flags of one command, and either of them can be the mistyped one.
var ErrPeriodMismatch = errors.New("the run bills another billing period")

// Finalize closes a completed run, and is the whole of what tally-engine
// finalize does. Once its transaction commits, the D8 triggers of migration
// 0001 hold that run's usage records, rated records, correction deltas and
// statements against every write.
//
// A regular run closes the billing period it meters. The run's status and the
// period's move in one transaction, so no reader ever sees a period pointing at
// a run that is not finalized itself. A correction run, which exists only over
// a period that is already closed, moves to 'finalized' alone: billing_periods
// keeps naming the regular run that closed the month, because those three
// columns say which run closed the period, and the latest finalized truth of a
// month is read through LatestFinalizedRun rather than off the period row.
//
// The period is locked the way Execute locks it, and for the same length: a
// finalize must not land between the record writes of a run and the completion
// that ends it. A period another process is metering yields an error wrapping
// ErrRunInProgress.
//
// The five refusals are a run no row carries (ErrRunNotFound), a run of another
// month (ErrPeriodMismatch), a run that is not completed (ErrRunNotCompleted),
// a regular run over a period that is already closed (ErrPeriodFinalized), and
// a correction run over a period that is not closed (ErrPeriodNotFinalized).
// None of them writes a row.
//
// What comes back is the kind of the run that was closed, which decides what
// the caller reports: a regular run closed its period, a correction closed only
// itself. It is the empty string on every refusal.
func Finalize(
	ctx context.Context,
	engine *pgxpool.Pool,
	periodFrom time.Time,
	runID uuid.UUID,
) (string, error) {
	retry := fmt.Sprintf("tally-engine finalize --period %s --run %s", period.Format(periodFrom), runID)
	var kind string
	err := withPeriodLock(ctx, engine, periodFrom, retry, func() error {
		var err error
		kind, err = finalize(ctx, engine, periodFrom, runID)
		return err
	})
	return kind, err
}

// finalize is Finalize under the period lock: the checks and the writes, in one
// transaction. The run row is read FOR NO KEY UPDATE, so the status the checks
// are made against is the one the finalization writes over.
func finalize(
	ctx context.Context,
	engine *pgxpool.Pool,
	periodFrom time.Time,
	runID uuid.UUID,
) (string, error) {
	month := period.Format(periodFrom)

	tx, err := engine.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("opening the finalize transaction of run %s: %w", runID, err)
	}
	// The rollback of the refusal paths, on a context no cancellation reaches: a
	// canceled call gives its row locks back rather than leaving them to the
	// connection's teardown. Rolling a committed transaction back is a no-op that
	// reports pgx.ErrTxClosed, which is why the error is dropped.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	q := sqlcgen.New(tx)
	run, err := q.GetRunForUpdate(ctx, uuidValue(runID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: there is no run %s to close %s over", ErrRunNotFound, runID, month)
		}
		return "", fmt.Errorf("reading the run %s: %w", runID, err)
	}
	// Over the instants rather than the months they render as: what the two
	// writes below address is the period start, and a run of a different one
	// would leave the period they are meant to move together untouched.
	if !run.PeriodFrom.Time.Equal(periodFrom) {
		runMonth := period.Format(run.PeriodFrom.Time)
		return "", fmt.Errorf(
			"%w: run %s bills %s, not %s, and tally-engine finalize --period %s --run %s closes the month it bills",
			ErrPeriodMismatch, runID, runMonth, month, runMonth, runID)
	}
	if run.Status != statusCompleted {
		return "", fmt.Errorf(
			"%w: run %s is %s, and a period is closed over a completed run, which tally-engine run --period %s produces",
			ErrRunNotCompleted, runID, run.Status, month)
	}

	billingPeriod, err := q.GetBillingPeriod(ctx, timestamptz(periodFrom))
	if err != nil {
		return "", fmt.Errorf("reading the billing period %s: %w", month, err)
	}

	if run.Kind == KindCorrection {
		if billingPeriod.Status != statusFinalized {
			return "", fmt.Errorf(
				"%w: a correction run exists only over a finalized period, and %s is %s",
				ErrPeriodNotFinalized, month, billingPeriod.Status)
		}
		// The run row alone. The month was closed by the regular run the period
		// names, and finalizing what corrects it does not take that name away.
		if err := q.FinalizeRun(ctx, uuidValue(runID)); err != nil {
			return "", fmt.Errorf("finalizing the correction run %s: %w", runID, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return "", fmt.Errorf(
				"committing the finalization of the correction run %s of %s: %w", runID, month, err)
		}
		return KindCorrection, nil
	}

	if billingPeriod.Status == statusFinalized {
		return "", fmt.Errorf(
			"%w: %s was closed by run %s, and a finalized period is changed with tally-engine correct --period %s",
			ErrPeriodFinalized, month, uuid.UUID(billingPeriod.FinalizedRunID.Bytes), month)
	}

	// The run's own trigger, trg_runs_immutable, lets this update through
	// because the row still reads 'completed': that is the one transition into
	// 'finalized' migration 0001 leaves open.
	if err := q.FinalizeRun(ctx, uuidValue(runID)); err != nil {
		return "", fmt.Errorf("finalizing the run %s: %w", runID, err)
	}
	// Status, finalized_run_id and finalized_at in one statement, which is what
	// the check constraint on billing_periods requires of a finalized period.
	if err := q.FinalizeBillingPeriod(ctx, sqlcgen.FinalizeBillingPeriodParams{
		PeriodFrom:     timestamptz(periodFrom),
		FinalizedRunID: uuidValue(runID),
	}); err != nil {
		return "", fmt.Errorf("closing the billing period %s: %w", month, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("committing the finalization of %s over run %s: %w", month, runID, err)
	}
	return run.Kind, nil
}

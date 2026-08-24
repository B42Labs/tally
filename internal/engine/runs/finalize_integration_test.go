package runs_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/runs"
)

// finalize closes one period over a run, on the test's own context.
func (f fixture) finalize(t *testing.T, from time.Time, runID uuid.UUID) error {
	t.Helper()

	return runs.Finalize(t.Context(), f.engine.Store.Pool(), from, runID)
}

// periodRow is a billing_periods row as the cases read it back. The two
// finalization columns are nullable, and an open period reads as the zero id
// and the zero instant.
type periodRow struct {
	status       string
	finalizedRun uuid.UUID
	finalizedAt  time.Time
}

// readPeriod reads one billing period with what it says about its closing.
func (f fixture) readPeriod(t *testing.T, from time.Time) periodRow {
	t.Helper()

	var row periodRow
	var finalizedRun *uuid.UUID
	var finalizedAt *time.Time
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT status, finalized_run_id, finalized_at FROM billing_periods WHERE period_from = $1`, from,
	).Scan(&row.status, &finalizedRun, &finalizedAt); err != nil {
		t.Fatalf("reading the period %s: %v", from.Format(time.RFC3339), err)
	}
	if finalizedRun != nil {
		row.finalizedRun = *finalizedRun
	}
	if finalizedAt != nil {
		row.finalizedAt = *finalizedAt
	}
	return row
}

// seedRun writes a run row of a period directly, in the status the case needs.
// The lifecycle produces neither a failed run nor a second completed one of a
// period on demand: it supersedes what it replaces.
func (f fixture) seedRun(t *testing.T, from, to time.Time, status string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, kind, pricing_version, status, completed_at)
		 VALUES ($1, $2, 'regular', 'v1', $3, now()) RETURNING id`, from, to, status).Scan(&id); err != nil {
		t.Fatalf("seeding the %s run of %s: %v", status, from.Format(time.RFC3339), err)
	}
	return id
}

// refused runs one statement the triggers of migration 0001 have to raise on,
// and returns what they raised. A statement that goes through fails the case:
// it changed a closed month.
func (f fixture) refused(t *testing.T, statement string, args ...any) string {
	t.Helper()

	if _, err := f.engine.Store.Pool().Exec(t.Context(), statement, args...); err != nil {
		return err.Error()
	}
	t.Fatalf("%q went through, want the trigger to refuse it", statement)
	return ""
}

// assertUnchanged fails when a refused finalize moved anything: the run keeps
// the status it had and the period stays open with neither of its finalization
// columns filled.
func (f fixture) assertUnchanged(t *testing.T, from time.Time, runID uuid.UUID, status string) {
	t.Helper()

	if got := f.readRun(t, runID).status; got != status {
		t.Errorf("run %s is %q, want it left at %q: a refused finalize writes no row", runID, got, status)
	}
	open := f.readPeriod(t, from)
	if open.status != "open" {
		t.Errorf("the period %s is %q, want it left open", period.Format(from), open.status)
	}
	if open.finalizedRun != uuid.Nil {
		t.Errorf("the period %s names run %s, want no closing run", period.Format(from), open.finalizedRun)
	}
	if !open.finalizedAt.IsZero() {
		t.Errorf("the period %s was closed at %s, want no closing time", period.Format(from), open.finalizedAt)
	}
}

func TestFinalize(t *testing.T) {
	f := newFixture(t)

	t.Run("closes the period over the run that meters it", func(t *testing.T) {
		from, to := month(-20)
		const cloud = "os-finalize-close"
		f.seedProject(t, cloud, "proj-close")
		f.seedResource(t, instance(cloud, "i-close", "proj-close", from, standardSize))

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if err := f.finalize(t, from, result.RunID); err != nil {
			t.Fatalf("Finalize() error = %v, want nil", err)
		}

		run := f.readRun(t, result.RunID)
		if run.status != "finalized" {
			t.Errorf("status = %q, want finalized", run.status)
		}
		if run.completedAt == nil {
			t.Fatal("completed_at = NULL, want the instant the run ended kept")
		}

		closed := f.readPeriod(t, from)
		if closed.status != "finalized" {
			t.Errorf("period status = %q, want finalized", closed.status)
		}
		if closed.finalizedRun != result.RunID {
			t.Errorf("finalized_run_id = %s, want the run %s the period is billed from",
				closed.finalizedRun, result.RunID)
		}
		if closed.finalizedAt.IsZero() {
			t.Fatal("finalized_at = NULL, want the instant the period was closed")
		}
		if closed.finalizedAt.Before(*run.completedAt) {
			t.Errorf("finalized_at = %s, want it at or after the run ended at %s",
				closed.finalizedAt, *run.completedAt)
		}
	})

	t.Run("refuses a run that does not exist", func(t *testing.T) {
		from, to := month(-19)
		const cloud = "os-finalize-unknown"
		f.seedProject(t, cloud, "proj-unknown")
		f.seedResource(t, instance(cloud, "i-unknown", "proj-unknown", from, standardSize))

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		unknown := uuid.New()
		err = f.finalize(t, from, unknown)
		if !errors.Is(err, runs.ErrRunNotFound) {
			t.Fatalf("Finalize() error = %v, want one matching ErrRunNotFound", err)
		}
		if !strings.Contains(err.Error(), unknown.String()) {
			t.Errorf("Finalize() error = %q, want it to name the run %s it did not find", err, unknown)
		}
		f.assertUnchanged(t, from, result.RunID, "completed")
	})

	t.Run("refuses a run of another period", func(t *testing.T) {
		closingFrom, closingTo := month(-18)
		otherFrom, otherTo := month(-17)
		const closingCloud, otherCloud = "os-finalize-closing", "os-finalize-other"
		f.seedProject(t, closingCloud, "proj-closing")
		f.seedProject(t, otherCloud, "proj-other")
		f.seedResource(t, instance(closingCloud, "i-closing", "proj-closing", closingFrom, standardSize))
		f.seedResource(t, instance(otherCloud, "i-other", "proj-other", otherFrom, standardSize))

		closing, err := f.execute(t, runs.Options{
			PeriodFrom: closingFrom, PeriodTo: closingTo, Clouds: []string{closingCloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		other, err := f.execute(t, runs.Options{
			PeriodFrom: otherFrom, PeriodTo: otherTo, Clouds: []string{otherCloud},
		})
		if err != nil {
			t.Fatalf("Execute() over the other period error = %v, want nil", err)
		}

		err = f.finalize(t, closingFrom, other.RunID)
		if !errors.Is(err, runs.ErrPeriodMismatch) {
			t.Fatalf("Finalize() error = %v, want one matching ErrPeriodMismatch", err)
		}
		for _, want := range []string{period.Format(closingFrom), period.Format(otherFrom)} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Finalize() error = %q, want it to name the month %s", err, want)
			}
		}
		f.assertUnchanged(t, closingFrom, closing.RunID, "completed")
		f.assertUnchanged(t, otherFrom, other.RunID, "completed")
	})

	t.Run("refuses a run that is not completed", func(t *testing.T) {
		from, to := month(-16)
		const cloud = "os-finalize-not-completed"
		f.seedProject(t, cloud, "proj-not-completed")
		f.seedResource(t, instance(cloud, "i-not-completed", "proj-not-completed", from, standardSize))

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		// The period holds a completed run beside the broken one, so what the
		// case shows is the run that was named being refused rather than the
		// period having nothing to close over.
		failed := f.seedRun(t, from, to, "failed")

		err = f.finalize(t, from, failed)
		if !errors.Is(err, runs.ErrRunNotCompleted) {
			t.Fatalf("Finalize() error = %v, want one matching ErrRunNotCompleted", err)
		}
		if want := "is failed"; !strings.Contains(err.Error(), want) {
			t.Errorf("Finalize() error = %q, want it to name the status with %q", err, want)
		}
		f.assertUnchanged(t, from, failed, "failed")
		f.assertUnchanged(t, from, result.RunID, "completed")
	})

	t.Run("refuses a period that is already closed", func(t *testing.T) {
		from, to := month(-15)
		const cloud = "os-finalize-closed"
		f.seedProject(t, cloud, "proj-closed")
		f.seedResource(t, instance(cloud, "i-closed", "proj-closed", from, standardSize))

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		second := f.seedRun(t, from, to, "completed")
		if err := f.finalize(t, from, result.RunID); err != nil {
			t.Fatalf("Finalize() error = %v, want nil", err)
		}

		err = f.finalize(t, from, second)
		if !errors.Is(err, runs.ErrPeriodFinalized) {
			t.Fatalf("Finalize() error = %v, want one matching ErrPeriodFinalized", err)
		}
		if !strings.Contains(err.Error(), result.RunID.String()) {
			t.Errorf("Finalize() error = %q, want it to name the run %s that closed the period", err, result.RunID)
		}
		if got := f.readRun(t, second).status; got != "completed" {
			t.Errorf("the second run is %q, want it left completed: a refused finalize writes no row", got)
		}
		if closed := f.readPeriod(t, from); closed.finalizedRun != result.RunID {
			t.Errorf("finalized_run_id = %s, want the run %s that closed the period kept",
				closed.finalizedRun, result.RunID)
		}
	})

	t.Run("holds the records of the closed period immutable", func(t *testing.T) {
		from, to := month(-14)
		const cloud = "os-finalize-immutable"
		f.seedProject(t, cloud, "proj-immutable")
		f.seedResource(t, instance(cloud, "i-immutable", "proj-immutable", from, standardSize))

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if err := f.finalize(t, from, result.RunID); err != nil {
			t.Fatalf("Finalize() error = %v, want nil", err)
		}

		// Every record table of the run, changed and removed: what holds a
		// closed month is the database (D8), not the code that writes to it.
		for _, statement := range []string{
			`UPDATE usage_records SET seconds = seconds + 1 WHERE run_id = $1`,
			`DELETE FROM usage_records WHERE run_id = $1`,
			`UPDATE rated_records SET amount = amount + 1 WHERE run_id = $1`,
			`DELETE FROM rated_records WHERE run_id = $1`,
			`UPDATE project_statements SET total = total + 1 WHERE run_id = $1`,
			`DELETE FROM project_statements WHERE run_id = $1`,
		} {
			raised := f.refused(t, statement, result.RunID)
			for _, want := range []string{"records of finalized run", "are immutable"} {
				if !strings.Contains(raised, want) {
					t.Errorf("%q raised %q, want it to say %q", statement, raised, want)
				}
			}
		}

		// The run row carries a trigger of its own, over its own message.
		raised := f.refused(t, `UPDATE runs SET status = 'completed' WHERE id = $1`, result.RunID)
		if want := "is finalized and immutable"; !strings.Contains(raised, want) {
			t.Errorf("the update of the run raised %q, want it to say %q", raised, want)
		}

		if got := len(f.readUsage(t, result.RunID)); got != 1 {
			t.Errorf("the finalized run holds %d usage records, want the one it wrote", got)
		}
		if got := len(f.readRated(t, result.RunID)); got != 3 {
			t.Errorf("the finalized run holds %d rated records, want the three it wrote", got)
		}
		if got := len(f.readStatements(t, result.RunID)); got != 1 {
			t.Errorf("the finalized run holds %d statements, want the one it wrote", got)
		}
	})

	t.Run("refuses a run over the period it closed", func(t *testing.T) {
		from, to := month(-13)
		const cloud = "os-finalize-rerun"
		f.seedProject(t, cloud, "proj-rerun")
		f.seedResource(t, instance(cloud, "i-rerun", "proj-rerun", from, standardSize))
		opts := runs.Options{PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud}}

		result, err := f.execute(t, opts)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if err := f.finalize(t, from, result.RunID); err != nil {
			t.Fatalf("Finalize() error = %v, want nil", err)
		}

		_, err = f.execute(t, opts)
		if !errors.Is(err, runs.ErrPeriodFinalized) {
			t.Fatalf("Execute() error = %v, want one matching ErrPeriodFinalized", err)
		}
		if !strings.Contains(err.Error(), result.RunID.String()) {
			t.Errorf("Execute() error = %q, want it to name the run %s that closed the period", err, result.RunID)
		}
		if want := "tally-engine correct --period"; !strings.Contains(err.Error(), want) {
			t.Errorf("Execute() error = %q, want it to point at %q", err, want)
		}
		if got := f.countRuns(t, from); got != 1 {
			t.Errorf("the period holds %d runs, want only the finalized one: a refused run writes no row", got)
		}
	})

	t.Run("refuses a finalize while another run holds the period", func(t *testing.T) {
		from, to := month(-12)
		const cloud = "os-finalize-locked"
		f.seedProject(t, cloud, "proj-locked")
		f.seedResource(t, instance(cloud, "i-locked", "proj-locked", from, standardSize))

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		// The lock is taken the way a run takes it, on a connection of its own,
		// after the run above has given it back: it is a session lock, so it is
		// held until this connection closes.
		holder, err := pgx.Connect(t.Context(), f.engine.URL)
		if err != nil {
			t.Fatalf("opening the connection that holds the lock: %v", err)
		}
		defer func() {
			if err := holder.Close(context.Background()); err != nil {
				t.Errorf("closing the connection that holds the lock: %v", err)
			}
		}()
		var locked bool
		if err := holder.QueryRow(t.Context(),
			`SELECT pg_try_advisory_lock(hashtextextended('period:' || $1::text, 0))`,
			from.UTC().Format(time.RFC3339)).Scan(&locked); err != nil {
			t.Fatalf("taking the period lock: %v", err)
		}
		if !locked {
			t.Fatal("pg_try_advisory_lock = false, want the test to hold the lock")
		}

		// A finalize between the record writes of a run and its completion would
		// close the period over half a run, which is why it waits for the lock
		// rather than reading the status of the run it was pointed at.
		err = f.finalize(t, from, result.RunID)
		if !errors.Is(err, runs.ErrRunInProgress) {
			t.Fatalf("Finalize() error = %v, want one matching ErrRunInProgress", err)
		}
		f.assertUnchanged(t, from, result.RunID, "completed")
	})
}

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

// finalize closes one run, on the test's own context, and reports the kind
// Finalize said it closed.
func (f fixture) finalize(t *testing.T, from time.Time, runID uuid.UUID) (string, error) {
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

// seedRunOfKind writes a run row of a period directly, in the kind and status
// the case needs. The lifecycle produces neither a failed run nor a second
// completed one of a period on demand: it supersedes what it replaces. A
// correction run names the run it corrects; uuid.Nil binds NULL, which is what
// a regular run carries there.
func (f fixture) seedRunOfKind(
	t *testing.T,
	from, to time.Time,
	kind, status string,
	correctsRunID uuid.UUID,
) uuid.UUID {
	t.Helper()

	var corrects *uuid.UUID
	if correctsRunID != uuid.Nil {
		corrects = &correctsRunID
	}
	var id uuid.UUID
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, kind, corrects_run_id, pricing_version, status, completed_at)
		 VALUES ($1, $2, $3, $4, 'v1', $5, now()) RETURNING id`,
		from, to, kind, corrects, status).Scan(&id); err != nil {
		t.Fatalf("seeding the %s %s run of %s: %v", status, kind, from.Format(time.RFC3339), err)
	}
	return id
}

// seedRun writes a regular run row of a period in the status the case needs.
func (f fixture) seedRun(t *testing.T, from, to time.Time, status string) uuid.UUID {
	t.Helper()

	return f.seedRunOfKind(t, from, to, runs.KindRegular, status, uuid.Nil)
}

// seedDelta writes one correction_deltas row of a run, the way a correction
// writes what it found. The three amounts are passed as the text they are
// stored with, which keeps them off floats (roadmap/00-conventions.md
// section 6).
func (f fixture) seedDelta(
	t *testing.T,
	runID, correctsRunID uuid.UUID,
	cloud, resourceID, project, dimension, oldAmount, newAmount, delta string,
) {
	t.Helper()

	if _, err := f.engine.Store.Pool().Exec(t.Context(),
		`INSERT INTO correction_deltas (run_id, corrects_run_id, cloud, platform, resource_type,
		                                resource_id, project_id, dimension,
		                                old_amount, new_amount, delta, currency)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::numeric, $10::numeric, $11::numeric, 'EUR')`,
		runID, correctsRunID, cloud, platform, resourceType, resourceID, project, dimension,
		oldAmount, newAmount, delta); err != nil {
		t.Fatalf("seeding the %s delta of run %s: %v", dimension, runID, err)
	}
}

// seedStatement writes one project_statements row of a run. The document is
// left empty: what the cases around it are about is the row being there and
// staying as it is.
func (f fixture) seedStatement(t *testing.T, runID uuid.UUID, key, total string) {
	t.Helper()

	if _, err := f.engine.Store.Pool().Exec(t.Context(),
		`INSERT INTO project_statements (run_id, project_id, document, total, currency)
		 VALUES ($1, $2, '{}'::jsonb, $3::numeric, 'EUR')`, runID, key, total); err != nil {
		t.Fatalf("seeding the statement of run %s for %s: %v", runID, key, err)
	}
}

// readDeltaCount is how many correction deltas a run holds, which is what the
// statements a trigger refused have to have left standing.
func (f fixture) readDeltaCount(t *testing.T, runID uuid.UUID) int {
	t.Helper()

	var count int
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM correction_deltas WHERE run_id = $1`, runID).Scan(&count); err != nil {
		t.Fatalf("counting the deltas of run %s: %v", runID, err)
	}
	return count
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
		finalized, err := f.finalize(t, from, result.RunID)
		if err != nil {
			t.Fatalf("Finalize() error = %v, want nil", err)
		}
		// The value says a month was closed: a regular run is what carries a
		// period into 'finalized'.
		if finalized != runs.KindRegular {
			t.Errorf("Finalize() = %q, want %q", finalized, runs.KindRegular)
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
		_, err = f.finalize(t, from, unknown)
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

		_, err = f.finalize(t, closingFrom, other.RunID)
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

		_, err = f.finalize(t, from, failed)
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
		if _, err := f.finalize(t, from, result.RunID); err != nil {
			t.Fatalf("Finalize() error = %v, want nil", err)
		}

		_, err = f.finalize(t, from, second)
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
		if _, err := f.finalize(t, from, result.RunID); err != nil {
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
		if _, err := f.finalize(t, from, result.RunID); err != nil {
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
		_, err = f.finalize(t, from, result.RunID)
		if !errors.Is(err, runs.ErrRunInProgress) {
			t.Fatalf("Finalize() error = %v, want one matching ErrRunInProgress", err)
		}
		f.assertUnchanged(t, from, result.RunID, "completed")
	})

	// A correction is finalized over the same command a regular run is, and it
	// closes itself alone: the month stays closed by the run the period names.
	t.Run("finalizes a correction and leaves the period naming the regular run", func(t *testing.T) {
		from, to := month(-22)
		const cloud = "os-finalize-correction"
		f.seedProject(t, cloud, "proj-correction")
		f.seedResource(t, instance(cloud, "i-correction", "proj-correction", from, standardSize))

		regular, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if _, err := f.finalize(t, from, regular.RunID); err != nil {
			t.Fatalf("Finalize() of the regular run error = %v, want nil", err)
		}
		closed := f.readPeriod(t, from)

		// The correction and the two records it carries: the delta against what
		// the finalized run billed, and the credit note over it.
		correction := f.seedRunOfKind(t, from, to, runs.KindCorrection, "completed", regular.RunID)
		f.seedDelta(t, correction, regular.RunID, cloud, "i-correction", "proj-correction",
			"vcpus", "0.96", "1.92", "0.96")
		f.seedStatement(t, correction, "proj-correction", "0.96")

		finalized, err := f.finalize(t, from, correction)
		if err != nil {
			t.Fatalf("Finalize() of the correction error = %v, want nil", err)
		}
		if finalized != runs.KindCorrection {
			t.Errorf("Finalize() = %q, want %q", finalized, runs.KindCorrection)
		}
		if got := f.readRun(t, correction).status; got != "finalized" {
			t.Errorf("the correction is %q, want finalized", got)
		}

		after := f.readPeriod(t, from)
		if after.status != closed.status {
			t.Errorf("period status = %q, want the %q the regular run left", after.status, closed.status)
		}
		if after.finalizedRun != regular.RunID {
			t.Errorf("finalized_run_id = %s, want the regular run %s that closed the period",
				after.finalizedRun, regular.RunID)
		}
		if !after.finalizedAt.Equal(closed.finalizedAt) {
			t.Errorf("finalized_at = %s, want the %s the regular run left", after.finalizedAt, closed.finalizedAt)
		}
	})

	// A correction meets the same status check a regular run does, which is
	// what a second finalize of it runs into.
	t.Run("refuses to finalize a correction twice", func(t *testing.T) {
		from, to := month(-23)
		const cloud = "os-finalize-correction-again"
		f.seedProject(t, cloud, "proj-correction-again")
		f.seedResource(t, instance(cloud, "i-correction-again", "proj-correction-again", from, standardSize))

		regular, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if _, err := f.finalize(t, from, regular.RunID); err != nil {
			t.Fatalf("Finalize() of the regular run error = %v, want nil", err)
		}

		correction := f.seedRunOfKind(t, from, to, runs.KindCorrection, "completed", regular.RunID)
		if _, err := f.finalize(t, from, correction); err != nil {
			t.Fatalf("Finalize() of the correction error = %v, want nil", err)
		}
		// Beside it, the correction a later one replaced, which closes nothing
		// either.
		superseded := f.seedRunOfKind(t, from, to, runs.KindCorrection, "superseded", regular.RunID)

		for _, tc := range []struct {
			runID  uuid.UUID
			status string
		}{
			{correction, "finalized"},
			{superseded, "superseded"},
		} {
			_, err := f.finalize(t, from, tc.runID)
			if !errors.Is(err, runs.ErrRunNotCompleted) {
				t.Fatalf("Finalize() of the %s correction error = %v, want one matching ErrRunNotCompleted",
					tc.status, err)
			}
			if want := "is " + tc.status; !strings.Contains(err.Error(), want) {
				t.Errorf("Finalize() error = %q, want it to name the status with %q", err, want)
			}
			if got := f.readRun(t, tc.runID).status; got != tc.status {
				t.Errorf("the correction is %q, want it left at %q: a refused finalize writes no row",
					got, tc.status)
			}
		}
	})

	// A correction exists only over a closed month. No code path produces this
	// state, because correcting an open period is refused before a run is
	// opened, and the branch is what holds the invariant.
	t.Run("refuses a correction over a period that is not finalized", func(t *testing.T) {
		from, to := month(-24)
		const cloud = "os-finalize-correction-open"
		f.seedProject(t, cloud, "proj-correction-open")
		f.seedResource(t, instance(cloud, "i-correction-open", "proj-correction-open", from, standardSize))

		// Metered and left open: this is the period the correction below names.
		regular, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		correction := f.seedRunOfKind(t, from, to, runs.KindCorrection, "completed", regular.RunID)

		_, err = f.finalize(t, from, correction)
		if !errors.Is(err, runs.ErrPeriodNotFinalized) {
			t.Fatalf("Finalize() error = %v, want one matching ErrPeriodNotFinalized", err)
		}
		for _, want := range []string{period.Format(from), "is open"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Finalize() error = %q, want it to say %q", err, want)
			}
		}
		f.assertUnchanged(t, from, correction, "completed")
	})

	// The deltas and the credit notes of a finalized correction are held by the
	// same rule that holds the records of a finalized regular run (D8).
	t.Run("holds the records of a finalized correction immutable", func(t *testing.T) {
		from, to := month(-25)
		const cloud = "os-finalize-correction-immutable"
		f.seedProject(t, cloud, "proj-correction-immutable")
		f.seedResource(t, instance(cloud, "i-correction-immutable", "proj-correction-immutable",
			from, standardSize))

		regular, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if _, err := f.finalize(t, from, regular.RunID); err != nil {
			t.Fatalf("Finalize() of the regular run error = %v, want nil", err)
		}

		correction := f.seedRunOfKind(t, from, to, runs.KindCorrection, "completed", regular.RunID)
		f.seedDelta(t, correction, regular.RunID, cloud, "i-correction-immutable", "proj-correction-immutable",
			"vcpus", "0.96", "0.48", "-0.48")
		f.seedStatement(t, correction, "proj-correction-immutable", "-0.48")
		if _, err := f.finalize(t, from, correction); err != nil {
			t.Fatalf("Finalize() of the correction error = %v, want nil", err)
		}

		// Both record tables of a correction, changed, removed and written to.
		for _, statement := range []string{
			`UPDATE correction_deltas SET delta = delta + 1 WHERE run_id = $1`,
			`DELETE FROM correction_deltas WHERE run_id = $1`,
			`INSERT INTO correction_deltas (run_id, corrects_run_id, cloud, platform, resource_type,
			                                resource_id, project_id, dimension,
			                                old_amount, new_amount, delta, currency)
			 VALUES ($1, $1, 'os-late', 'openstack', 'instance', 'i-late', 'proj-late',
			         'vcpus', 0, 1, 1, 'EUR')`,
			`UPDATE project_statements SET total = total + 1 WHERE run_id = $1`,
			`DELETE FROM project_statements WHERE run_id = $1`,
			`INSERT INTO project_statements (run_id, project_id, document, total, currency)
			 VALUES ($1, 'x', '{}', 0, 'EUR')`,
		} {
			raised := f.refused(t, statement, correction)
			for _, want := range []string{"records of finalized run", "are immutable"} {
				if !strings.Contains(raised, want) {
					t.Errorf("%q raised %q, want it to say %q", statement, raised, want)
				}
			}
		}

		// What the rule reads is run_id, on both sides of the write. A further
		// correction names this one in corrects_run_id and writes its rows under
		// its own run, which is the chaining migration 0001 indexes that column
		// for, so those rows go through.
		chained := f.seedRunOfKind(t, from, to, runs.KindCorrection, "running", correction)
		f.seedDelta(t, chained, correction, cloud, "i-correction-immutable", "proj-correction-immutable",
			"vcpus", "0.48", "0.72", "0.24")
		if got := f.readDeltaCount(t, chained); got != 1 {
			t.Errorf("the chained correction holds %d deltas, want the one it wrote", got)
		}

		if got := f.readDeltaCount(t, correction); got != 1 {
			t.Errorf("the finalized correction holds %d deltas, want the one it was given", got)
		}
		if got := len(f.readStatements(t, correction)); got != 1 {
			t.Errorf("the finalized correction holds %d statements, want the one it was given", got)
		}
	})
}

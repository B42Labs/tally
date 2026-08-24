// This file pins the guards the run statements carry, against a real database
// rather than against the generated signatures: they are one predicate each,
// and dropping one is a change no compiler and no caller notices. What the
// guards keep out is a run that another process has already settled being
// written back over that decision -- trg_runs_immutable fires on
// OLD.status = 'finalized' alone, so every other transition passes the database
// untouched, and a period would end up with two runs that both read as its
// current numbers.
package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
	"github.com/b42labs/tally/internal/engine/store/storetest"
)

// TestRunEndsRequireARunningRun holds CompleteRun and FailRun to the status they
// move from. A run whose process is still metering can be failed underneath it
// by the reclaim of another process, and the write that follows must not undo
// that.
func TestRunEndsRequireARunningRun(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	t.Run("completes a run that is still running", func(t *testing.T) {
		id := openRun(t, db, "running")

		rows, err := q.CompleteRun(t.Context(), sqlcgen.CompleteRunParams{ID: id, Stats: []byte(`{}`)})
		if err != nil {
			t.Fatalf("CompleteRun() error = %v, want nil", err)
		}
		if rows != 1 {
			t.Fatalf("CompleteRun() moved %d rows, want 1", rows)
		}
		if got := readRunStatus(t, db, id); got != "completed" {
			t.Errorf("the run is %q, want completed", got)
		}
	})

	t.Run("leaves a run another process reclaimed", func(t *testing.T) {
		// The state ReclaimStaleRuns leaves behind. Completing it would put a run
		// its period has already replaced back among the period's completed runs,
		// with the reclaim's reason written over by this run's stats.
		id := openRun(t, db, "failed")

		rows, err := q.CompleteRun(t.Context(), sqlcgen.CompleteRunParams{ID: id, Stats: []byte(`{}`)})
		if err != nil {
			t.Fatalf("CompleteRun() error = %v, want nil", err)
		}
		if rows != 0 {
			t.Fatalf("CompleteRun() moved %d rows, want 0: the run is not running any more", rows)
		}
		if got := readRunStatus(t, db, id); got != "failed" {
			t.Errorf("the run is %q, want the failed it was reclaimed as", got)
		}
	})

	t.Run("does not fail a run that was already settled", func(t *testing.T) {
		// The other order of the same race: the run was completed while the
		// process that opened it was recording its own failure.
		id := openRun(t, db, "completed")

		rows, err := q.FailRun(t.Context(), sqlcgen.FailRunParams{ID: id, Stats: []byte(`{}`)})
		if err != nil {
			t.Fatalf("FailRun() error = %v, want nil", err)
		}
		if rows != 0 {
			t.Fatalf("FailRun() moved %d rows, want 0: the run is not running any more", rows)
		}
		if got := readRunStatus(t, db, id); got != "completed" {
			t.Errorf("the run is %q, want it left completed", got)
		}
	})
}

// TestReclaimStaleRunsWaitsOutTheRunsThatMayLive pins the age ReclaimStaleRuns
// reads a missing process off. The period lock cannot stand in for it: it is a
// session lock on one pooled connection which stays protocol-idle for the whole
// run, so anything that closes that connection alone releases it while the
// process keeps metering.
func TestReclaimStaleRunsWaitsOutTheRunsThatMayLive(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	young := openRun(t, db, "running")
	old := openRun(t, db, "running")
	if _, err := db.Store.Pool().Exec(t.Context(),
		`UPDATE runs SET started_at = now() - interval '3 hours' WHERE id = $1`, uuid.UUID(old.Bytes),
	); err != nil {
		t.Fatalf("ageing the stale run: %v", err)
	}

	reclaimed, err := q.ReclaimStaleRuns(t.Context(), pgtype.Timestamptz{Time: periodFrom, Valid: true})
	if err != nil {
		t.Fatalf("ReclaimStaleRuns() error = %v, want nil", err)
	}

	if len(reclaimed) != 1 || reclaimed[0] != old {
		t.Fatalf("ReclaimStaleRuns() = %v, want the aged run %s alone", reclaimed, uuid.UUID(old.Bytes))
	}
	if got := readRunStatus(t, db, young); got != "running" {
		t.Errorf("the run that started moments ago is %q, want it left running", got)
	}
}

// openRun writes a run of the period in the status a case starts from. The
// insert is plain SQL: the statements under test are what a case asserts over,
// so they are not also what sets it up.
func openRun(t *testing.T, db storetest.DB, status string) pgtype.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, kind, pricing_version, status)
		 VALUES ($1, $2, 'regular', 'v1', $3) RETURNING id`,
		periodFrom, periodTo, status).Scan(&id); err != nil {
		t.Fatalf("seeding the %s run: %v", status, err)
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// readRunStatus reads back what a run is stored as.
func readRunStatus(t *testing.T, db storetest.DB, id pgtype.UUID) string {
	t.Helper()

	var status string
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT status FROM runs WHERE id = $1`, uuid.UUID(id.Bytes)).Scan(&status); err != nil {
		t.Fatalf("reading the run %s: %v", uuid.UUID(id.Bytes), err)
	}
	return status
}

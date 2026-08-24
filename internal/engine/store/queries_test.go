// This file pins the guards the run statements carry, against a real database
// rather than against the generated signatures: they are one predicate each,
// and dropping one is a change no compiler and no caller notices. What the
// guards keep out is a run that another process has already settled being
// written back over that decision -- trg_runs_immutable fires on
// OLD.status = 'finalized' alone, so every other transition passes the database
// untouched, and a period would end up with two runs that both read as its
// current numbers.
//
// The queries a correction is built from are held here too: which run it takes
// as its baseline, the key it diffs by, and the delta rows it writes.
package store_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

// TestSupersedeCompletedRunsByKind holds the supersede to the kind it is called
// with. A period carries one completed run per kind, the regular run that
// metered it and the correction that re-metered it, and retiring the runs of
// one kind must leave the other kind standing.
func TestSupersedeCompletedRunsByKind(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	regular := openRun(t, db, "completed")
	correction := openSeededRun(t, db, runSeed{
		kind: "correction", status: "completed", correctsRunID: regular,
	})
	inFlight := openSeededRun(t, db, runSeed{
		kind: "correction", status: "running", correctsRunID: regular,
	})

	// The two cases run in order: the second one reads the period the first left
	// behind, where the correction is already retired.
	t.Run("retires the completed correction alone", func(t *testing.T) {
		superseded, err := q.SupersedeCompletedRuns(t.Context(), sqlcgen.SupersedeCompletedRunsParams{
			PeriodFrom: pgtype.Timestamptz{Time: periodFrom, Valid: true},
			Kind:       "correction",
		})
		if err != nil {
			t.Fatalf("SupersedeCompletedRuns() error = %v, want nil", err)
		}
		if len(superseded) != 1 || superseded[0] != correction {
			t.Fatalf("SupersedeCompletedRuns() = %v, want the completed correction %s alone",
				superseded, uuid.UUID(correction.Bytes))
		}
		if got := readRunStatus(t, db, regular); got != "completed" {
			t.Errorf("the regular run is %q, want it left completed", got)
		}
		if got := readRunStatus(t, db, inFlight); got != "running" {
			t.Errorf("the correction that is still metering is %q, want it left running", got)
		}
	})

	t.Run("retires the completed regular run alone", func(t *testing.T) {
		superseded, err := q.SupersedeCompletedRuns(t.Context(), sqlcgen.SupersedeCompletedRunsParams{
			PeriodFrom: pgtype.Timestamptz{Time: periodFrom, Valid: true},
			Kind:       "regular",
		})
		if err != nil {
			t.Fatalf("SupersedeCompletedRuns() error = %v, want nil", err)
		}
		if len(superseded) != 1 || superseded[0] != regular {
			t.Fatalf("SupersedeCompletedRuns() = %v, want the completed regular run %s alone",
				superseded, uuid.UUID(regular.Bytes))
		}
		if got := readRunStatus(t, db, correction); got != "superseded" {
			t.Errorf("the correction is %q, want the superseded the case above left it as", got)
		}
	})
}

// TestLatestFinalizedRun pins the baseline a correction diffs against: the
// newest finalized run of the period, which is the regular run that closed it
// until a correction of that period is finalized in turn.
func TestLatestFinalizedRun(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	// Written in an order that is not the order they started in, so what comes
	// back is decided by started_at and by nothing else.
	now := time.Now()
	regular := openSeededRun(t, db, runSeed{
		kind: "regular", status: "finalized", startedAt: now.Add(-3 * time.Hour),
	})
	latest := openSeededRun(t, db, runSeed{
		kind: "correction", status: "finalized", correctsRunID: regular, startedAt: now.Add(-time.Hour),
	})
	earlier := openSeededRun(t, db, runSeed{
		kind: "correction", status: "finalized", correctsRunID: regular, startedAt: now.Add(-2 * time.Hour),
	})

	t.Run("takes the newest finalized run of the period", func(t *testing.T) {
		row, err := q.LatestFinalizedRun(t.Context(), pgtype.Timestamptz{Time: periodFrom, Valid: true})
		if err != nil {
			t.Fatalf("LatestFinalizedRun() error = %v, want nil", err)
		}
		if row.ID != latest {
			t.Errorf("LatestFinalizedRun() = %s, want the newest finalized run %s (the earlier correction is %s)",
				uuid.UUID(row.ID.Bytes), uuid.UUID(latest.Bytes), uuid.UUID(earlier.Bytes))
		}
		if row.Kind != "correction" {
			t.Errorf("the run is a %q run, want correction", row.Kind)
		}
		if row.CorrectsRunID != regular {
			t.Errorf("the run corrects %s, want the regular run %s",
				uuid.UUID(row.CorrectsRunID.Bytes), uuid.UUID(regular.Bytes))
		}
	})

	t.Run("reports no row for a period nothing has closed", func(t *testing.T) {
		open := periodFrom.AddDate(0, -1, 0)
		_, err := q.LatestFinalizedRun(t.Context(), pgtype.Timestamptz{Time: open, Valid: true})
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("LatestFinalizedRun() error = %v, want %v", err, pgx.ErrNoRows)
		}
	})
}

// TestSumRatedByRun pins the key a correction diffs by. One resource is metered
// into as many usage drafts as its history has intervals, and the diff is over
// the resource rather than over those drafts: the amounts of one dimension have
// to arrive added up.
func TestSumRatedByRun(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	runID := openRun(t, db, "completed")
	middle := periodFrom.AddDate(0, 0, 10)
	first := seedUsageRecord(t, db, runID, "abc-123", "tenant-a", periodFrom, middle)
	second := seedUsageRecord(t, db, runID, "abc-123", "tenant-a", middle, periodTo)
	other := seedUsageRecord(t, db, runID, "def-456", "tenant-b", periodFrom, periodTo)
	seedRatedRecord(t, db, runID, first, "vcpus", "10.00")
	seedRatedRecord(t, db, runID, second, "vcpus", "5.50")
	seedRatedRecord(t, db, runID, first, "ram_gb", "2.25")
	seedRatedRecord(t, db, runID, other, "vcpus", "1.00")

	type summed struct {
		cloud, platform, resourceType, resourceID, projectID, dimension, amount, currency string
	}

	t.Run("sums the amounts of a run per resource and dimension", func(t *testing.T) {
		rows, err := q.SumRatedByRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("SumRatedByRun() error = %v, want nil", err)
		}
		got := make([]summed, 0, len(rows))
		for _, row := range rows {
			got = append(got, summed{
				row.Cloud, row.Platform, row.ResourceType, row.ResourceID,
				row.ProjectID, row.Dimension, amountText(t, row.Amount), row.Currency,
			})
		}
		want := []summed{
			{"openstack", "compute", "vm", "abc-123", "tenant-a", "ram_gb", "2.25", "EUR"},
			{"openstack", "compute", "vm", "abc-123", "tenant-a", "vcpus", "15.50", "EUR"},
			{"openstack", "compute", "vm", "def-456", "tenant-b", "vcpus", "1.00", "EUR"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("SumRatedByRun() = %v, want %v", got, want)
		}
	})

	t.Run("reports nothing for a run that rated nothing", func(t *testing.T) {
		empty := openRun(t, db, "completed")

		rows, err := q.SumRatedByRun(t.Context(), empty)
		if err != nil {
			t.Fatalf("SumRatedByRun() error = %v, want nil", err)
		}
		if len(rows) != 0 {
			t.Errorf("SumRatedByRun() = %v, want no rows", rows)
		}
	})
}

// TestCreateCorrectionDeltas holds the copy of a correction's delta rows. What
// they carry is what a credit note is written from, so every amount has to come
// back exactly as it was handed over.
func TestCreateCorrectionDeltas(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	corrected := openRun(t, db, "finalized")
	correction := openSeededRun(t, db, runSeed{
		kind: "correction", status: "running", correctsRunID: corrected,
	})

	type stored struct {
		dimension, oldAmount, newAmount, delta, currency string
		correctsRunID                                    pgtype.UUID
	}

	t.Run("copies the deltas of a correction", func(t *testing.T) {
		deltas := []sqlcgen.CreateCorrectionDeltasParams{
			correctionDelta(t, correction, corrected, "vcpus", "59.52", "49.92", "-9.60"),
			correctionDelta(t, correction, corrected, "ram_gb", "29.76", "24.96", "-4.80"),
		}

		copied, err := q.CreateCorrectionDeltas(t.Context(), deltas)
		if err != nil {
			t.Fatalf("CreateCorrectionDeltas() error = %v, want nil", err)
		}
		if copied != 2 {
			t.Fatalf("CreateCorrectionDeltas() copied %d rows, want 2", copied)
		}

		rows, err := db.Store.Pool().Query(t.Context(),
			`SELECT dimension, old_amount::text, new_amount::text, delta::text, currency, corrects_run_id
			 FROM correction_deltas WHERE run_id = $1 ORDER BY dimension`, correction)
		if err != nil {
			t.Fatalf("reading the deltas back: %v", err)
		}
		defer rows.Close()

		var got []stored
		for rows.Next() {
			var row stored
			if err := rows.Scan(&row.dimension, &row.oldAmount, &row.newAmount,
				&row.delta, &row.currency, &row.correctsRunID); err != nil {
				t.Fatalf("reading a delta back: %v", err)
			}
			got = append(got, row)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("reading the deltas back: %v", err)
		}
		want := []stored{
			{"ram_gb", "29.76", "24.96", "-4.80", "EUR", corrected},
			{"vcpus", "59.52", "49.92", "-9.60", "EUR", corrected},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the stored deltas are %v, want %v", got, want)
		}
	})

	t.Run("copies nothing for an empty batch", func(t *testing.T) {
		copied, err := q.CreateCorrectionDeltas(t.Context(), nil)
		if err != nil {
			t.Fatalf("CreateCorrectionDeltas() error = %v, want nil", err)
		}
		if copied != 0 {
			t.Errorf("CreateCorrectionDeltas() copied %d rows, want 0", copied)
		}
	})
}

// runSeed is the run a case starts from: its kind, the status it carries, the
// run it corrects where it is a correction, and the time it started. An
// invalid correctsRunID is the NULL a run that corrects nothing stores, and a
// zero startedAt is now.
type runSeed struct {
	kind          string
	status        string
	correctsRunID pgtype.UUID
	startedAt     time.Time
}

// openSeededRun writes one run of the period. The insert is plain SQL: the
// statements under test are what a case asserts over, so they are not also what
// sets it up. started_at is written here rather than moved afterwards, because
// the trigger on runs holds a finalized row immutable.
func openSeededRun(t *testing.T, db storetest.DB, seed runSeed) pgtype.UUID {
	t.Helper()

	startedAt := seed.startedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	var id uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, kind, corrects_run_id, pricing_version, status, started_at)
		 VALUES ($1, $2, $3, $4, 'v1', $5, $6) RETURNING id`,
		periodFrom, periodTo, seed.kind, seed.correctsRunID, seed.status, startedAt).Scan(&id); err != nil {
		t.Fatalf("seeding the %s %s run: %v", seed.status, seed.kind, err)
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// openRun writes a regular run of the period in the status a case starts from,
// which is where most of them start.
func openRun(t *testing.T, db storetest.DB, status string) pgtype.UUID {
	t.Helper()

	return openSeededRun(t, db, runSeed{kind: "regular", status: status})
}

// seedUsageRecord writes one usage draft under a run and returns its id. What a
// sum groups by is what a case passes; the rest is one shape of draft.
func seedUsageRecord(
	t *testing.T,
	db storetest.DB,
	runID pgtype.UUID,
	resourceID, projectID string,
	from, to time.Time,
) pgtype.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO usage_records (run_id, cloud, platform, resource_type, resource_id, project_id,
		                            state, from_ts, to_ts, seconds, usage)
		 VALUES ($1, 'openstack', 'compute', 'vm', $2, $3, 'active', $4, $5, $6, '{"vcpus": 4}')
		 RETURNING id`,
		runID, resourceID, projectID, from, to, int64(to.Sub(from)/time.Second)).Scan(&id); err != nil {
		t.Fatalf("seeding the usage record of %s: %v", resourceID, err)
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// seedRatedRecord writes one rated amount over a usage draft.
func seedRatedRecord(
	t *testing.T,
	db storetest.DB,
	runID, usageRecordID pgtype.UUID,
	dimension, amount string,
) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO rated_records (run_id, usage_record_id, dimension, amount, currency)
		 VALUES ($1, $2, $3, $4, 'EUR')`,
		runID, usageRecordID, dimension, numeric(t, amount)); err != nil {
		t.Fatalf("seeding the %s amount %s: %v", dimension, amount, err)
	}
}

// correctionDelta is one delta row as a case hands it to the copy.
func correctionDelta(
	t *testing.T,
	runID, correctsRunID pgtype.UUID,
	dimension, oldAmount, newAmount, difference string,
) sqlcgen.CreateCorrectionDeltasParams {
	t.Helper()

	return sqlcgen.CreateCorrectionDeltasParams{
		ID:            pgtype.UUID{Bytes: uuid.New(), Valid: true},
		RunID:         runID,
		CorrectsRunID: correctsRunID,
		Cloud:         "openstack",
		Platform:      "compute",
		ResourceType:  "vm",
		ResourceID:    "abc-123",
		ProjectID:     "tenant-a",
		Dimension:     dimension,
		OldAmount:     numeric(t, oldAmount),
		NewAmount:     numeric(t, newAmount),
		Delta:         numeric(t, difference),
		Currency:      "EUR",
	}
}

// numeric is an amount on its way into a NUMERIC(14,2) column. It reaches the
// column as text rather than through a float (roadmap/00-conventions.md
// section 6).
func numeric(t *testing.T, amount string) pgtype.Numeric {
	t.Helper()

	var value pgtype.Numeric
	if err := value.Scan(amount); err != nil {
		t.Fatalf("reading the amount %q: %v", amount, err)
	}
	return value
}

// amountText is a stored amount read back as text, which is what keeps an
// assertion over money off floats.
func amountText(t *testing.T, amount pgtype.Numeric) string {
	t.Helper()

	value, err := amount.Value()
	if err != nil {
		t.Fatalf("reading the stored amount: %v", err)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("the stored amount is a %T, want it as text", value)
	}
	return text
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

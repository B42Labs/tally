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
//
// The reads an export loads a run through are held here as well: the run row
// itself, taken without a lock, and the two listings whose ordering is what
// makes two exports of one run identical.
package store_test

import (
	"context"
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

// TestGetRun pins the read an export loads a run through. Every column of the
// row has to arrive, a run that does not exist has to read as no row rather
// than as an empty one, and the read must take no lock: an export runs beside
// the finalization of another run, and a read that queued behind that row would
// hold the finalization up in turn.
func TestGetRun(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	corrected := openRun(t, db, "finalized")
	// A fixed instant rather than now(): timestamptz keeps microseconds, and an
	// assertion over the column is only exact if what went in was.
	startedAt := periodTo.Add(time.Hour)
	runID := openSeededRun(t, db, runSeed{
		kind: "correction", status: "completed", correctsRunID: corrected, startedAt: startedAt,
	})

	t.Run("reads every column of a run", func(t *testing.T) {
		row, err := q.GetRun(t.Context(), runID)
		if err != nil {
			t.Fatalf("GetRun() error = %v, want nil", err)
		}
		if row.ID != runID {
			t.Errorf("GetRun() = %s, want %s", uuid.UUID(row.ID.Bytes), uuid.UUID(runID.Bytes))
		}
		if stamp(row.PeriodFrom.Time) != stamp(periodFrom) || stamp(row.PeriodTo.Time) != stamp(periodTo) {
			t.Errorf("the run covers %s to %s, want %s to %s",
				stamp(row.PeriodFrom.Time), stamp(row.PeriodTo.Time), stamp(periodFrom), stamp(periodTo))
		}
		if row.Kind != "correction" {
			t.Errorf("the run is a %q run, want correction", row.Kind)
		}
		if row.CorrectsRunID != corrected {
			t.Errorf("the run corrects %s, want %s",
				uuid.UUID(row.CorrectsRunID.Bytes), uuid.UUID(corrected.Bytes))
		}
		if row.PricingVersion.String != "v1" {
			t.Errorf("the run priced against %q, want v1", row.PricingVersion.String)
		}
		if row.Status != "completed" {
			t.Errorf("the run is %q, want completed", row.Status)
		}
		if len(row.Clouds) != 0 {
			t.Errorf("the run names the clouds %v, want the empty default", row.Clouds)
		}
		if string(row.Stats) != "{}" {
			t.Errorf("the run carries the stats %s, want the empty default", row.Stats)
		}
		if stamp(row.StartedAt.Time) != stamp(startedAt) {
			t.Errorf("the run started %s, want %s", stamp(row.StartedAt.Time), stamp(startedAt))
		}
		if row.CompletedAt.Valid {
			t.Errorf("the run completed %s, want the null of a run no end was written for",
				stamp(row.CompletedAt.Time))
		}
	})

	t.Run("reports no row for a run that does not exist", func(t *testing.T) {
		unknown := pgtype.UUID{Bytes: uuid.New(), Valid: true}

		_, err := q.GetRun(t.Context(), unknown)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("GetRun() error = %v, want %v", err, pgx.ErrNoRows)
		}
	})

	t.Run("reads a run another transaction holds locked", func(t *testing.T) {
		tx, err := db.Store.Pool().Begin(t.Context())
		if err != nil {
			t.Fatalf("beginning the locking transaction: %v", err)
		}
		defer func() {
			if err := tx.Rollback(t.Context()); err != nil {
				t.Errorf("rolling the locking transaction back: %v", err)
			}
		}()
		if _, err := sqlcgen.New(tx).GetRunForUpdate(t.Context(), runID); err != nil {
			t.Fatalf("GetRunForUpdate() error = %v, want nil", err)
		}

		// The transaction above holds FOR NO KEY UPDATE on the row until the
		// rollback. A GetRun carrying a FOR clause would wait it out, which is
		// what the deadline turns from a hung test into a failing one.
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()

		if _, err := q.GetRun(ctx, runID); err != nil {
			t.Fatalf("GetRun() error = %v, want nil: the read must not queue behind the row lock", err)
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

// TestListRatedRecords pins the order an export walks the rated records of a
// run in. Two exports of one run have to come out byte-identical, and that
// holds only where the ordering is a total one: the drafts of one resource
// never overlap, so from_ts settles the records of a resource and the dimension
// settles the amounts of a draft.
func TestListRatedRecords(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	runID := openRun(t, db, "completed")
	middle := periodFrom.AddDate(0, 0, 10)
	// The resource ids sort against their clouds, so an ordering that read the
	// resource before the cloud would hand the two resources back the other way
	// round.
	early := seedUsageRecordIn(t, db, runID, "os-a", "z-9", "tenant-z", periodFrom, middle)
	late := seedUsageRecordIn(t, db, runID, "os-a", "z-9", "tenant-z", middle, periodTo)
	other := seedUsageRecordIn(t, db, runID, "os-b", "a-1", "tenant-a", periodFrom, periodTo)
	// Written in none of the orders the case asserts.
	seedRatedRecord(t, db, runID, late, "vcpus", "5.50")
	seedRatedRecord(t, db, runID, other, "vcpus", "1.00")
	seedRatedRecord(t, db, runID, early, "vcpus", "10.00")
	seedRatedRecord(t, db, runID, late, "ram_gb", "1.25")
	seedRatedRecord(t, db, runID, early, "ram_gb", "2.25")

	type record struct {
		cloud, platform, resourceType, resourceID, projectID, state string
		from, to, usage, dimension, amount, currency                string
	}

	t.Run("reads the rated records of a run in cloud, resource, time and dimension order", func(t *testing.T) {
		rows, err := q.ListRatedRecords(t.Context(), runID)
		if err != nil {
			t.Fatalf("ListRatedRecords() error = %v, want nil", err)
		}
		got := make([]record, 0, len(rows))
		for _, row := range rows {
			got = append(got, record{
				row.Cloud, row.Platform, row.ResourceType, row.ResourceID, row.ProjectID, row.State,
				stamp(row.FromTs.Time), stamp(row.ToTs.Time), string(row.Usage),
				row.Dimension, amountText(t, row.Amount), row.Currency,
			})
		}
		usage := `{"vcpus": 4}`
		want := []record{
			{
				"os-a", "compute", "vm", "z-9", "tenant-z", "active",
				stamp(periodFrom), stamp(middle), usage, "ram_gb", "2.25", "EUR",
			},
			{
				"os-a", "compute", "vm", "z-9", "tenant-z", "active",
				stamp(periodFrom), stamp(middle), usage, "vcpus", "10.00", "EUR",
			},
			{
				"os-a", "compute", "vm", "z-9", "tenant-z", "active",
				stamp(middle), stamp(periodTo), usage, "ram_gb", "1.25", "EUR",
			},
			{
				"os-a", "compute", "vm", "z-9", "tenant-z", "active",
				stamp(middle), stamp(periodTo), usage, "vcpus", "5.50", "EUR",
			},
			{
				"os-b", "compute", "vm", "a-1", "tenant-a", "active",
				stamp(periodFrom), stamp(periodTo), usage, "vcpus", "1.00", "EUR",
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ListRatedRecords() = %v, want %v", got, want)
		}
	})

	t.Run("reports nothing for a run that rated nothing", func(t *testing.T) {
		empty := openRun(t, db, "completed")

		rows, err := q.ListRatedRecords(t.Context(), empty)
		if err != nil {
			t.Fatalf("ListRatedRecords() error = %v, want nil", err)
		}
		if len(rows) != 0 {
			t.Errorf("ListRatedRecords() = %v, want no rows", rows)
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

// TestListCorrectionDeltas pins the order an export walks the deltas of a
// correction in, which is the order corrections.Diff sorted them in before they
// were written. The rows land through a copy, and a copy keeps no order of its
// own, so the ordering the reader gets has to come from the statement.
func TestListCorrectionDeltas(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	corrected := openRun(t, db, "finalized")
	correction := openSeededRun(t, db, runSeed{
		kind: "correction", status: "running", correctsRunID: corrected,
	})
	// Written in none of the orders the case asserts, and with the resource ids
	// sorting against their clouds and one resource split over two projects, so
	// that every key column of the ordering is read.
	seedCorrectionDelta(t, db, correction, corrected, "os-b", "a-1", "tenant-a", "vcpus", "8.00", "6.00", "-2.00")
	seedCorrectionDelta(t, db, correction, corrected, "os-a", "z-9", "tenant-a", "vcpus", "59.52", "49.92", "-9.60")
	seedCorrectionDelta(t, db, correction, corrected, "os-a", "z-9", "tenant-b", "vcpus", "4.00", "5.00", "1.00")
	seedCorrectionDelta(t, db, correction, corrected, "os-a", "z-9", "tenant-a", "ram_gb", "29.76", "24.96", "-4.80")

	type delta struct {
		cloud, platform, resourceType, resourceID, projectID, dimension string
		oldAmount, newAmount, difference, currency                      string
	}

	t.Run("reads the deltas of a correction in the order they were diffed in", func(t *testing.T) {
		rows, err := q.ListCorrectionDeltas(t.Context(), correction)
		if err != nil {
			t.Fatalf("ListCorrectionDeltas() error = %v, want nil", err)
		}
		got := make([]delta, 0, len(rows))
		for _, row := range rows {
			got = append(got, delta{
				row.Cloud, row.Platform, row.ResourceType, row.ResourceID, row.ProjectID, row.Dimension,
				amountText(t, row.OldAmount), amountText(t, row.NewAmount),
				amountText(t, row.Delta), row.Currency,
			})
		}
		want := []delta{
			{"os-a", "compute", "vm", "z-9", "tenant-a", "ram_gb", "29.76", "24.96", "-4.80", "EUR"},
			{"os-a", "compute", "vm", "z-9", "tenant-a", "vcpus", "59.52", "49.92", "-9.60", "EUR"},
			{"os-a", "compute", "vm", "z-9", "tenant-b", "vcpus", "4.00", "5.00", "1.00", "EUR"},
			{"os-b", "compute", "vm", "a-1", "tenant-a", "vcpus", "8.00", "6.00", "-2.00", "EUR"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ListCorrectionDeltas() = %v, want %v", got, want)
		}
	})

	t.Run("reports nothing for a run that corrected nothing", func(t *testing.T) {
		empty := openSeededRun(t, db, runSeed{
			kind: "correction", status: "running", correctsRunID: corrected,
		})

		rows, err := q.ListCorrectionDeltas(t.Context(), empty)
		if err != nil {
			t.Fatalf("ListCorrectionDeltas() error = %v, want nil", err)
		}
		if len(rows) != 0 {
			t.Errorf("ListCorrectionDeltas() = %v, want no rows", rows)
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

	return seedUsageRecordIn(t, db, runID, "openstack", resourceID, projectID, from, to)
}

// seedUsageRecordIn writes that draft into the cloud a case names. The cloud
// leads the order the listings hand their rows back in, so a case pinning that
// order needs drafts in more than the one cloud above.
func seedUsageRecordIn(
	t *testing.T,
	db storetest.DB,
	runID pgtype.UUID,
	cloud, resourceID, projectID string,
	from, to time.Time,
) pgtype.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO usage_records (run_id, cloud, platform, resource_type, resource_id, project_id,
		                            state, from_ts, to_ts, seconds, usage)
		 VALUES ($1, $2, 'compute', 'vm', $3, $4, 'active', $5, $6, $7, '{"vcpus": 4}')
		 RETURNING id`,
		runID, cloud, resourceID, projectID, from, to,
		int64(to.Sub(from)/time.Second)).Scan(&id); err != nil {
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

// seedCorrectionDelta writes one delta row of a correction. The insert is plain
// SQL for the reason openSeededRun's is: the statement under test is the read,
// so it is not also what sets the rows up.
func seedCorrectionDelta(
	t *testing.T,
	db storetest.DB,
	runID, correctsRunID pgtype.UUID,
	cloud, resourceID, projectID, dimension string,
	oldAmount, newAmount, difference string,
) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO correction_deltas (run_id, corrects_run_id, cloud, platform, resource_type,
		                                resource_id, project_id, dimension,
		                                old_amount, new_amount, delta, currency)
		 VALUES ($1, $2, $3, 'compute', 'vm', $4, $5, $6, $7, $8, $9, 'EUR')`,
		runID, correctsRunID, cloud, resourceID, projectID, dimension,
		numeric(t, oldAmount), numeric(t, newAmount), numeric(t, difference)); err != nil {
		t.Fatalf("seeding the %s delta of %s: %v", dimension, resourceID, err)
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

// stamp renders a timestamp the way a case compares it. Two instants that are
// the same moment in different locations are not the same time.Time, and what
// pgx hands back carries the session's location rather than the one a case
// wrote.
func stamp(ts time.Time) string {
	return ts.UTC().Format(time.RFC3339)
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

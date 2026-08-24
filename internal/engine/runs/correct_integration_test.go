package runs_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/runs"
)

// correct corrects one period against the fixture, on the test's own context.
// The one case that needs a context it can cancel calls runs.Correct itself.
func (f fixture) correct(t *testing.T, opts runs.CorrectOptions) (runs.CorrectionResult, error) {
	t.Helper()

	return runs.Correct(t.Context(), f.engine.Store.Pool(), f.source, opts)
}

// seedPeriod writes a billing period row in the status the case needs. What the
// cases around it are about is the status a correction is refused over, and the
// lifecycle carries a period into one status at a time.
func (f fixture) seedPeriod(t *testing.T, from, to time.Time, status string) {
	t.Helper()

	if _, err := f.engine.Store.Pool().Exec(t.Context(),
		`INSERT INTO billing_periods (period_from, period_to, status) VALUES ($1, $2, $3)`,
		from, to, status); err != nil {
		t.Fatalf("seeding the %s period %s: %v", status, from.Format(time.RFC3339), err)
	}
}

// seedFinalizedPeriod writes a finalized regular run and the finalized period
// that names it. Both rows are inserted as they are meant to read, because the
// runs trigger holds a finalized row against every update. version is what the
// run recorded as its pricing version, and the empty string is stored as NULL,
// which is a run nothing says the prices of.
func (f fixture) seedFinalizedPeriod(t *testing.T, from, to time.Time, version string) uuid.UUID {
	t.Helper()

	var pricingVersion *string
	if version != "" {
		pricingVersion = &version
	}
	var id uuid.UUID
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, kind, pricing_version, status, completed_at)
		 VALUES ($1, $2, 'regular', $3, 'finalized', now()) RETURNING id`,
		from, to, pricingVersion).Scan(&id); err != nil {
		t.Fatalf("seeding the finalized run of %s: %v", from.Format(time.RFC3339), err)
	}
	if _, err := f.engine.Store.Pool().Exec(t.Context(),
		`INSERT INTO billing_periods (period_from, period_to, status, finalized_run_id, finalized_at)
		 VALUES ($1, $2, 'finalized', $3, now())`, from, to, id); err != nil {
		t.Fatalf("seeding the finalized period %s: %v", from.Format(time.RFC3339), err)
	}
	return id
}

// closedMonth meters one period over one cloud and closes it, which is the
// state every case that corrects something starts from.
func (f fixture) closedMonth(t *testing.T, from, to time.Time, cloud string) uuid.UUID {
	t.Helper()

	result, err := f.execute(t, runs.Options{PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud}})
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if _, err := f.finalize(t, from, result.RunID); err != nil {
		t.Fatalf("Finalize() error = %v, want nil", err)
	}
	return result.RunID
}

// assertWholeMonth fails when the run did not bill its one resource for the 744
// hours of a 31-day month. It is what the deltas of the cases below are
// differences against, so it is stated before they are read.
func (f fixture) assertWholeMonth(t *testing.T, runID uuid.UUID) {
	t.Helper()

	usage := f.readUsage(t, runID)
	if len(usage) != 1 {
		t.Fatalf("usage records = %d, want the one draft of a resource that lived the whole month", len(usage))
	}
	want := []ratedRow{
		{dimension: "disk_gb", amount: "59.52", currency: "EUR", usageRecordID: usage[0].id},
		{dimension: "ram_gb", amount: "29.76", currency: "EUR", usageRecordID: usage[0].id},
		{dimension: "vcpus", amount: "59.52", currency: "EUR", usageRecordID: usage[0].id},
	}
	if got := f.readRated(t, runID); !slices.Equal(got, want) {
		t.Fatalf("rated records = %+v, want %+v, which is 744 hours at the concept's prices", got, want)
	}
}

// assertEmptyRun fails when a run holds any record at all, which is what a
// correction that broke has to leave behind: its write transaction is the one
// that would have written them.
func (f fixture) assertEmptyRun(t *testing.T, runID uuid.UUID) {
	t.Helper()

	for _, tc := range []struct {
		records string
		count   int
	}{
		{records: "usage records", count: len(f.readUsage(t, runID))},
		{records: "rated records", count: len(f.readRated(t, runID))},
		{records: "correction deltas", count: len(f.readDeltas(t, runID))},
		{records: "statements", count: len(f.readStatements(t, runID))},
	} {
		if tc.count != 0 {
			t.Errorf("the failed correction holds %d %s, want the transaction rolled back", tc.count, tc.records)
		}
	}
}

// TestCorrect runs against the pricing model of the concept's example. Every
// case keeps to its own month and its own cloud: a correction of a month
// supersedes the other corrections of that month whatever cloud they metered,
// and a month is finalized once. The three cases that bill a whole month take
// the 31-day months thirtyOneDayMonth walks back to, and the others take
// offsets past the deepest of those.
func TestCorrect(t *testing.T) {
	f := newFixtureWith(t, correctionPricingDocument)

	t.Run("credits the concept's late power cycle", func(t *testing.T) {
		from, to := thirtyOneDayMonth(1)
		const cloud = "os-correct-chain"
		const key = cloud + "/proj-456"
		f.seedProject(t, cloud, "proj-456")
		vm := wholeMonth(cloud, "abc-123", "proj-456", from, to, standardSize)
		f.seedResource(t, vm)

		finalized := f.closedMonth(t, from, to, cloud)
		f.assertWholeMonth(t, finalized)
		closed := f.readPeriod(t, from)

		// The concept's power cycle, arriving after the month was billed: the
		// events are dated inside the period and received at an instant past
		// the one the finalized run read the reporting database at.
		received := f.snapshotTime(t)
		f.seedEventReceived(t, vm, "ev-off-abc-123", "compute.instance.power_off.end",
			from.Add(10*24*time.Hour), received, `{"state":"shutoff"}`)
		f.seedEventReceived(t, vm, "ev-on-abc-123", "compute.instance.power_on.end",
			from.Add(20*24*time.Hour), received, `{"state":"active"}`)

		opts := runs.CorrectOptions{PeriodFrom: from, PeriodTo: to}
		wantDeltas := []deltaRow{
			{
				correctsRunID: finalized, projectID: "proj-456", dimension: "disk_gb",
				oldAmount: "59.52", newAmount: "49.92", delta: "-9.60", currency: "EUR",
			},
			{
				correctsRunID: finalized, projectID: "proj-456", dimension: "ram_gb",
				oldAmount: "29.76", newAmount: "24.96", delta: "-4.80", currency: "EUR",
			},
			{
				correctsRunID: finalized, projectID: "proj-456", dimension: "vcpus",
				oldAmount: "59.52", newAmount: "49.92", delta: "-9.60", currency: "EUR",
			},
		}

		first, err := f.correct(t, opts)
		if err != nil {
			t.Fatalf("Correct() error = %v, want nil", err)
		}
		if first.CorrectsRunID != finalized {
			t.Errorf("CorrectsRunID = %s, want the finalized run %s", first.CorrectsRunID, finalized)
		}
		if first.PricingVersion != "v1" {
			t.Errorf("PricingVersion = %q, want the v1 the finalized run recorded", first.PricingVersion)
		}
		if first.Stats.Deltas != 3 {
			t.Errorf("Stats.Deltas = %d, want the three dimensions the power cycle changed", first.Stats.Deltas)
		}
		if got := f.readDeltas(t, first.RunID); !slices.Equal(got, wantDeltas) {
			t.Errorf("deltas = %+v, want %+v", got, wantDeltas)
		}
		if got := f.readStatementTotals(t, first.RunID); !maps.Equal(got, map[string]string{key: "-24.00"}) {
			t.Errorf("statement totals = %v, want the credit note of %s at -24.00", got, key)
		}
		if got := text(t, f.readStatementDocument(t, first.RunID, key), "corrects_run_id"); got != finalized.String() {
			t.Errorf("the credit note corrects run %s, want the finalized run %s", got, finalized)
		}

		run := f.readRun(t, first.RunID)
		if run.kind != runs.KindCorrection {
			t.Errorf("kind = %q, want %q", run.kind, runs.KindCorrection)
		}
		if run.correctsRunID != finalized {
			t.Errorf("corrects_run_id = %s, want the finalized run %s", run.correctsRunID, finalized)
		}
		if run.pricingVersion != "v1" {
			t.Errorf("pricing_version = %q, want the version the corrected run was rated with", run.pricingVersion)
		}
		if !slices.Equal(run.clouds, []string{cloud}) {
			t.Errorf("clouds = %v, want the %v the corrected run metered", run.clouds, []string{cloud})
		}
		if run.status != "completed" {
			t.Errorf("status = %q, want completed", run.status)
		}
		if run.completedAt == nil {
			t.Error("completed_at = NULL, want the instant the correction ended")
		}

		stats := f.readStats(t, first.RunID)
		if _, err := time.Parse(time.RFC3339Nano, text(t, stats, "snapshot_at")); err != nil {
			t.Errorf("parsing snapshot_at: %v", err)
		}
		assertCounts(t, stats, "1", "3", "9", "1")
		if got := number(t, stats, "deltas"); got != "3" {
			t.Errorf("stats deltas = %s, want 3", got)
		}
		if got := len(f.readUsage(t, first.RunID)); got != 3 {
			t.Errorf("the correction holds %d usage records, want the three intervals it metered", got)
		}
		if got := len(f.readRated(t, first.RunID)); got != 9 {
			t.Errorf("the correction holds %d rated records, want three dimensions per interval", got)
		}

		// The month it corrects is untouched: a correction stores its own
		// records and leaves the finalized ones where they are.
		if got := len(f.readUsage(t, finalized)); got != 1 {
			t.Errorf("the finalized run holds %d usage records, want its own kept", got)
		}
		if got := len(f.readRated(t, finalized)); got != 3 {
			t.Errorf("the finalized run holds %d rated records, want its own kept", got)
		}
		if got := len(f.readStatements(t, finalized)); got != 1 {
			t.Errorf("the finalized run holds %d statements, want its own kept", got)
		}
		if got := f.readRun(t, finalized).status; got != "finalized" {
			t.Errorf("the corrected run is %q, want it left finalized", got)
		}

		// A completed correction is not the period's truth yet, so the next one
		// diffs against the same finalized run and supersedes it.
		second, err := f.correct(t, opts)
		if err != nil {
			t.Fatalf("Correct() error = %v, want nil", err)
		}
		if second.CorrectsRunID != finalized {
			t.Errorf("CorrectsRunID = %s, want the finalized run %s again", second.CorrectsRunID, finalized)
		}
		if got := f.readDeltas(t, second.RunID); !slices.Equal(got, wantDeltas) {
			t.Errorf("deltas = %+v, want the same %+v the first correction found", got, wantDeltas)
		}
		if got := f.readStatementTotals(t, second.RunID); !maps.Equal(got, map[string]string{key: "-24.00"}) {
			t.Errorf("statement totals = %v, want the credit note of %s at -24.00 again", got, key)
		}
		if !slices.Equal(second.Superseded, []uuid.UUID{first.RunID}) {
			t.Errorf("Superseded = %v, want the completed correction %s", second.Superseded, first.RunID)
		}
		if got := f.readRun(t, first.RunID).status; got != "superseded" {
			t.Errorf("the first correction is %q, want superseded", got)
		}
		// Its records stay for the audit, the way a superseded regular run's do.
		for _, tc := range []struct {
			records string
			count   int
			want    int
		}{
			{records: "usage records", count: len(f.readUsage(t, first.RunID)), want: 3},
			{records: "rated records", count: len(f.readRated(t, first.RunID)), want: 9},
			{records: "correction deltas", count: len(f.readDeltas(t, first.RunID)), want: 3},
			{records: "statements", count: len(f.readStatements(t, first.RunID)), want: 1},
		} {
			if tc.count != tc.want {
				t.Errorf("the superseded correction holds %d %s, want its own %d kept",
					tc.count, tc.records, tc.want)
			}
		}

		if got, err := f.finalize(t, from, second.RunID); err != nil {
			t.Fatalf("Finalize() error = %v, want nil", err)
		} else if got != runs.KindCorrection {
			t.Errorf("Finalize() closed a %q, want a %q", got, runs.KindCorrection)
		}

		// The finalized correction is the period's truth now, and nothing has
		// arrived since, so there is nothing left to correct.
		third, err := f.correct(t, opts)
		if err != nil {
			t.Fatalf("Correct() error = %v, want nil", err)
		}
		if third.CorrectsRunID != second.RunID {
			t.Errorf("CorrectsRunID = %s, want the finalized correction %s", third.CorrectsRunID, second.RunID)
		}
		if third.Stats.Deltas != 0 {
			t.Errorf("Stats.Deltas = %d, want none: nothing arrived since the correction that closed", third.Stats.Deltas)
		}
		if got := f.readDeltas(t, third.RunID); len(got) != 0 {
			t.Errorf("deltas = %+v, want none", got)
		}
		if got := f.readStatementTotals(t, third.RunID); len(got) != 0 {
			t.Errorf("statement totals = %v, want none: there is nothing to credit", got)
		}
		if got := f.readRun(t, third.RunID).status; got != "completed" {
			t.Errorf("status = %q, want completed: a correction that found nothing is a correction", got)
		}
		if len(third.Superseded) != 0 {
			t.Errorf("Superseded = %v, want none: the correction ahead of it is finalized, not completed",
				third.Superseded)
		}

		// One more late event, against the finalized correction rather than
		// against the run that closed the month.
		f.seedEvent(t, vm, "ev-off-2-abc-123", "compute.instance.power_off.end",
			from.Add(25*24*time.Hour), `{"state":"shutoff"}`)

		fourth, err := f.correct(t, opts)
		if err != nil {
			t.Fatalf("Correct() error = %v, want nil", err)
		}
		if fourth.CorrectsRunID != second.RunID {
			t.Errorf("CorrectsRunID = %s, want the finalized correction %s", fourth.CorrectsRunID, second.RunID)
		}
		if !slices.Equal(fourth.Superseded, []uuid.UUID{third.RunID}) {
			t.Errorf("Superseded = %v, want the completed correction %s", fourth.Superseded, third.RunID)
		}
		wantChained := []deltaRow{
			{
				correctsRunID: second.RunID, projectID: "proj-456", dimension: "disk_gb",
				oldAmount: "49.92", newAmount: "44.16", delta: "-5.76", currency: "EUR",
			},
			{
				correctsRunID: second.RunID, projectID: "proj-456", dimension: "ram_gb",
				oldAmount: "24.96", newAmount: "22.08", delta: "-2.88", currency: "EUR",
			},
			{
				correctsRunID: second.RunID, projectID: "proj-456", dimension: "vcpus",
				oldAmount: "49.92", newAmount: "44.16", delta: "-5.76", currency: "EUR",
			},
		}
		if got := f.readDeltas(t, fourth.RunID); !slices.Equal(got, wantChained) {
			t.Errorf("deltas = %+v, want %+v, the difference against the finalized correction", got, wantChained)
		}
		// What a diff against the run that closed the month would have credited
		// is -38.40, and the customer has been credited -24.00 of it already.
		if got := f.readStatementTotals(t, fourth.RunID); !maps.Equal(got, map[string]string{key: "-14.40"}) {
			t.Errorf("statement totals = %v, want the credit note of %s at -14.40", got, key)
		}

		if got := f.readRun(t, finalized).status; got != "finalized" {
			t.Errorf("the corrected run is %q, want it left finalized through the whole chain", got)
		}
		if now := f.readPeriod(t, from); now.status != closed.status ||
			now.finalizedRun != closed.finalizedRun || !now.finalizedAt.Equal(closed.finalizedAt) {
			t.Errorf("the period is %+v, want it left at %+v: a correction closes no month", now, closed)
		}
	})

	t.Run("debits a resource that appeared late", func(t *testing.T) {
		from, to := thirtyOneDayMonth(2)
		const cloud = "os-correct-appeared"
		f.seedProject(t, cloud, "proj-456")
		f.seedProject(t, cloud, "proj-789")
		f.seedResource(t, wholeMonth(cloud, "abc-123", "proj-456", from, to, standardSize))

		finalized := f.closedMonth(t, from, to, cloud)
		f.assertWholeMonth(t, finalized)

		// A resource the finalized run never saw, alive for the whole month it
		// appeared in.
		f.seedResource(t, resource{
			cloud: cloud, id: "def-456", project: "proj-789",
			created: from, deleted: to.Add(24 * time.Hour), size: standardSize,
		})

		result, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
		if err != nil {
			t.Fatalf("Correct() error = %v, want nil", err)
		}
		want := []deltaRow{
			{
				correctsRunID: finalized, projectID: "proj-789", dimension: "disk_gb",
				oldAmount: "0.00", newAmount: "59.52", delta: "59.52", currency: "EUR",
			},
			{
				correctsRunID: finalized, projectID: "proj-789", dimension: "ram_gb",
				oldAmount: "0.00", newAmount: "29.76", delta: "29.76", currency: "EUR",
			},
			{
				correctsRunID: finalized, projectID: "proj-789", dimension: "vcpus",
				oldAmount: "0.00", newAmount: "59.52", delta: "59.52", currency: "EUR",
			},
		}
		if got := f.readDeltas(t, result.RunID); !slices.Equal(got, want) {
			t.Errorf("deltas = %+v, want %+v, the whole of what the resource costs", got, want)
		}
		// The project whose resource did not change is credited nothing, so it
		// gets no note at all.
		if got := f.readStatementTotals(t, result.RunID); !maps.Equal(got,
			map[string]string{cloud + "/proj-789": "148.80"}) {
			t.Errorf("statement totals = %v, want the debit of %s/proj-789 at 148.80", got, cloud)
		}
	})

	t.Run("credits a resource deleted late", func(t *testing.T) {
		from, to := thirtyOneDayMonth(3)
		const cloud = "os-correct-deleted"
		f.seedProject(t, cloud, "proj-456")
		vm := wholeMonth(cloud, "abc-123", "proj-456", from, to, standardSize)
		f.seedResource(t, vm)

		finalized := f.closedMonth(t, from, to, cloud)
		f.assertWholeMonth(t, finalized)

		// The delete the month was billed without: 25 of its 31 days.
		f.seedEvent(t, vm, "ev-delete-late-abc-123", "compute.instance.delete.end",
			from.Add(25*24*time.Hour), "")

		result, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
		if err != nil {
			t.Fatalf("Correct() error = %v, want nil", err)
		}
		want := []deltaRow{
			{
				correctsRunID: finalized, projectID: "proj-456", dimension: "disk_gb",
				oldAmount: "59.52", newAmount: "48.00", delta: "-11.52", currency: "EUR",
			},
			{
				correctsRunID: finalized, projectID: "proj-456", dimension: "ram_gb",
				oldAmount: "29.76", newAmount: "24.00", delta: "-5.76", currency: "EUR",
			},
			{
				correctsRunID: finalized, projectID: "proj-456", dimension: "vcpus",
				oldAmount: "59.52", newAmount: "48.00", delta: "-11.52", currency: "EUR",
			},
		}
		if got := f.readDeltas(t, result.RunID); !slices.Equal(got, want) {
			t.Errorf("deltas = %+v, want %+v, the six days the resource did not live", got, want)
		}
		if got := f.readStatementTotals(t, result.RunID); !maps.Equal(got,
			map[string]string{cloud + "/proj-456": "-28.80"}) {
			t.Errorf("statement totals = %v, want the credit of %s/proj-456 at -28.80", got, cloud)
		}
		// A resource that is deleted inside the period breaches nothing: the
		// late event closes its timeline rather than reopening it.
		assertAbsent(t, f.readStats(t, result.RunID), "violations", "error")
	})

	t.Run("refuses a period that is not finalized", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			offset   int
			status   string
			runCount int
			seed     func(t *testing.T, from, to time.Time)
		}{
			{
				name: "open with a completed run", offset: -8, status: "open", runCount: 1,
				seed: func(t *testing.T, from, to time.Time) {
					const cloud = "os-correct-open-run"
					f.seedProject(t, cloud, "proj-456")
					f.seedResource(t, instance(cloud, "i-open-run", "proj-456", from, standardSize))
					if _, err := f.execute(t, runs.Options{
						PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
					}); err != nil {
						t.Fatalf("Execute() error = %v, want nil", err)
					}
				},
			},
			{
				name: "grace without a run", offset: -9, status: "grace", runCount: 0,
				seed: func(t *testing.T, from, to time.Time) { f.seedPeriod(t, from, to, "grace") },
			},
			{
				name: "open without a run", offset: -10, status: "open", runCount: 0,
				seed: func(t *testing.T, from, to time.Time) { f.seedPeriod(t, from, to, "open") },
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				from, to := month(tc.offset)
				tc.seed(t, from, to)

				_, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
				if !errors.Is(err, runs.ErrPeriodNotFinalized) {
					t.Fatalf("Correct() error = %v, want one matching ErrPeriodNotFinalized", err)
				}
				for _, want := range []string{tc.status, "tally-engine run --period", "tally-engine finalize"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Correct() error = %q, want it to name %q", err, want)
					}
				}
				if got := f.countRuns(t, from); got != tc.runCount {
					t.Errorf("the period holds %d runs, want %d: a refused correction writes no row",
						got, tc.runCount)
				}
			})
		}
	})

	t.Run("refuses a month it does not know", func(t *testing.T) {
		from, to := month(-11)

		_, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
		if !errors.Is(err, runs.ErrPeriodNotFinalized) {
			t.Fatalf("Correct() error = %v, want one matching ErrPeriodNotFinalized", err)
		}
		if want := "tally-engine run --period"; !strings.Contains(err.Error(), want) {
			t.Errorf("Correct() error = %q, want it to point at %q", err, want)
		}

		// The period row is not created here: a correction bills what a
		// finalized month was billed without, and a month nothing metered has
		// nothing to correct.
		var periods int
		if err := f.engine.Store.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM billing_periods WHERE period_from = $1`, from).Scan(&periods); err != nil {
			t.Fatalf("counting the billing periods of %s: %v", from.Format(time.RFC3339), err)
		}
		if periods != 0 {
			t.Errorf("the month holds %d billing periods, want none recorded", periods)
		}
	})

	t.Run("refuses a correction while another run holds the period", func(t *testing.T) {
		from, to := month(-12)
		const cloud = "os-correct-locked"
		f.seedProject(t, cloud, "proj-456")
		f.seedResource(t, instance(cloud, "i-locked", "proj-456", from, standardSize))
		f.closedMonth(t, from, to, cloud)

		// The lock is taken the way the run takes it, on a connection of its
		// own: it is a session lock, so it is held until this connection closes.
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

		if _, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to}); !errors.Is(
			err, runs.ErrRunInProgress) {
			t.Fatalf("Correct() error = %v, want one matching ErrRunInProgress", err)
		}
		if got := f.countRuns(t, from); got != 1 {
			t.Errorf("the period holds %d runs, want only the finalized one: the lock is taken before the first row",
				got)
		}
	})

	t.Run("reclaims a stale correction", func(t *testing.T) {
		from, to := month(-13)
		const cloud = "os-correct-reclaim"
		f.seedProject(t, cloud, "proj-456")
		f.seedResource(t, instance(cloud, "i-reclaim", "proj-456", from, standardSize))
		finalized := f.closedMonth(t, from, to, cloud)

		// Old enough that no process can be behind it, which is what a
		// correction whose process was killed leaves.
		stale := f.seedRunOfKind(t, from, to, runs.KindCorrection, "running", finalized)
		if _, err := f.engine.Store.Pool().Exec(t.Context(),
			`UPDATE runs SET started_at = now() - interval '3 hours' WHERE id = $1`, stale); err != nil {
			t.Fatalf("ageing the stale correction: %v", err)
		}

		result, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
		if err != nil {
			t.Fatalf("Correct() error = %v, want nil", err)
		}
		if !slices.Equal(result.Reclaimed, []uuid.UUID{stale}) {
			t.Errorf("Reclaimed = %v, want the stale correction %s", result.Reclaimed, stale)
		}
		if got := f.readRun(t, stale).status; got != "failed" {
			t.Errorf("the stale correction is %q, want failed", got)
		}
	})

	t.Run("inherits the clouds of the run it corrects", func(t *testing.T) {
		from, to := month(-14)
		const metered, other = "os-correct-clouds-a", "os-correct-clouds-b"
		f.seedProject(t, metered, "proj-456")
		f.seedProject(t, other, "proj-456")
		f.seedResource(t, instance(metered, "i-clouds-a", "proj-456", from, standardSize))
		f.closedMonth(t, from, to, metered)

		// A resource of another cloud, priced and alive in the same month. The
		// correction does not meter it, because the run it corrects did not.
		f.seedResource(t, instance(other, "i-clouds-b", "proj-456", from, standardSize))

		result, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
		if err != nil {
			t.Fatalf("Correct() error = %v, want nil", err)
		}
		if got := f.readRun(t, result.RunID).clouds; !slices.Equal(got, []string{metered}) {
			t.Errorf("clouds = %v, want the %v the corrected run metered", got, []string{metered})
		}
		if result.Stats.Deltas != 0 {
			t.Errorf("Stats.Deltas = %d, want none: the only late resource is of another cloud",
				result.Stats.Deltas)
		}
		if got := f.readDeltas(t, result.RunID); len(got) != 0 {
			t.Errorf("deltas = %+v, want none", got)
		}
	})

	// The way the scheduler closes a month: no cloud list at all, which meters
	// every cloud. The correction inherits that empty list and re-meters
	// everything, which is what a correction of a scheduled month runs as.
	t.Run("corrects a baseline that metered every cloud", func(t *testing.T) {
		from, to := month(-20)
		const cloud = "os-correct-every-cloud"
		f.seedProject(t, cloud, "proj-456")
		vm := instance(cloud, "i-every-cloud", "proj-456", from, standardSize)
		f.seedResource(t, vm)

		baseline, err := f.execute(t, runs.Options{PeriodFrom: from, PeriodTo: to})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if _, err := f.finalize(t, from, baseline.RunID); err != nil {
			t.Fatalf("Finalize() error = %v, want nil", err)
		}

		// A power cycle the finalized run never saw: the instance was shut off
		// halfway through the 48 hours it lived, which halves the second half.
		f.seedEventReceived(t, vm, "ev-off-i-every-cloud", "compute.instance.power_off.end",
			from.Add(48*time.Hour), f.snapshotTime(t), `{"state":"shutoff"}`)

		result, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
		if err != nil {
			t.Fatalf("Correct() error = %v, want nil", err)
		}
		if run := f.readRun(t, result.RunID); len(run.clouds) != 0 || run.clouds == nil {
			t.Errorf("clouds = %v, want the empty array that meters every cloud", run.clouds)
		}
		want := []deltaRow{
			{
				correctsRunID: baseline.RunID, projectID: "proj-456", dimension: "disk_gb",
				oldAmount: "3.84", newAmount: "2.88", delta: "-0.96", currency: "EUR",
			},
			{
				correctsRunID: baseline.RunID, projectID: "proj-456", dimension: "ram_gb",
				oldAmount: "1.92", newAmount: "1.44", delta: "-0.48", currency: "EUR",
			},
			{
				correctsRunID: baseline.RunID, projectID: "proj-456", dimension: "vcpus",
				oldAmount: "3.84", newAmount: "2.88", delta: "-0.96", currency: "EUR",
			},
		}
		if got := f.readDeltas(t, result.RunID); !slices.Equal(got, want) {
			t.Errorf("deltas = %+v, want %+v, the half of the second day the instance was off", got, want)
		}
	})

	t.Run("refuses a baseline without a pricing version", func(t *testing.T) {
		from, to := month(-15)
		baseline := f.seedFinalizedPeriod(t, from, to, "")

		_, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
		if err == nil {
			t.Fatal("Correct() error = nil, want the run without a pricing version refused")
		}
		for _, want := range []string{baseline.String(), "carries no pricing version"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Correct() error = %q, want it to name %q", err, want)
			}
		}
		if got := f.countRuns(t, from); got != 1 {
			t.Errorf("the period holds %d runs, want only the finalized one", got)
		}
	})

	t.Run("refuses a pricing version no model is stored under", func(t *testing.T) {
		from, to := month(-16)
		f.seedFinalizedPeriod(t, from, to, "v-missing")

		_, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
		if !errors.Is(err, pricing.ErrVersionNotFound) {
			t.Fatalf("Correct() error = %v, want one matching pricing.ErrVersionNotFound", err)
		}
		if got := f.countRuns(t, from); got != 1 {
			t.Errorf("the period holds %d runs, want only the finalized one: "+
				"the prices are resolved before the run row", got)
		}
	})

	t.Run("fails the correction on an invariant violation", func(t *testing.T) {
		from, to := month(-17)
		const cloud = "os-correct-violation"
		f.seedProject(t, cloud, "proj-456")
		sound := instance(cloud, "i-violation", "proj-456", from, standardSize)
		f.seedResource(t, sound)
		finalized := f.closedMonth(t, from, to, cloud)

		// An update after the delete reopens the timeline, which bills time the
		// resource's events do not describe a life for.
		f.seedEvent(t, sound, "ev-late-i-violation", "compute.instance.update",
			sound.deleted.Add(24*time.Hour), `{"state":"active"}`)

		result, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
		if err == nil {
			t.Fatal("Correct() error = nil, want the violating resource reported")
		}
		var violation *metering.ViolationError
		if !errors.As(err, &violation) {
			t.Fatalf("Correct() error = %v, want a *metering.ViolationError", err)
		}

		if got := f.readRun(t, result.RunID).status; got != "failed" {
			t.Errorf("status = %q, want failed", got)
		}
		stats := f.readStats(t, result.RunID)
		violations := list(t, stats, "violations")
		if len(violations) != 1 || text(t, violations[0], "resource_id") != "i-violation" {
			t.Errorf("violations = %v, want the one violating resource named", violations)
		}
		if got := text(t, stats, "error"); got != err.Error() {
			t.Errorf("stats error = %q, want the failure %q", got, err.Error())
		}
		f.assertEmptyRun(t, result.RunID)
		if got := f.readRun(t, finalized).status; got != "finalized" {
			t.Errorf("the corrected run is %q, want it left finalized", got)
		}
	})

	t.Run("records a canceled correction as failed", func(t *testing.T) {
		from, to := month(-18)
		const cloud = "os-correct-canceled"
		f.seedProject(t, cloud, "proj-456")
		f.seedResource(t, instance(cloud, "i-canceled", "proj-456", from, standardSize))
		f.closedMonth(t, from, to, cloud)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		result, err := runs.Correct(ctx, f.engine.Store.Pool(), f.source, runs.CorrectOptions{
			PeriodFrom: from, PeriodTo: to,
			Counters: counters.Config{Sources: []counters.Source{egressSource}},
			VM:       cancelingQuerier{cancel: cancel},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Correct() error = %v, want one matching context.Canceled", err)
		}
		// The bookkeeping runs on a context the cancellation does not reach, so
		// the correction is written down rather than left reading as in flight.
		if got := f.readRun(t, result.RunID).status; got != "failed" {
			t.Errorf("status = %q, want failed", got)
		}
		if got := text(t, f.readStats(t, result.RunID), "error"); !strings.Contains(got, "context canceled") {
			t.Errorf("stats error = %q, want the cancellation recorded", got)
		}
	})

	t.Run("writes nothing when a credit note is out of range", func(t *testing.T) {
		from, to := month(-19)
		const cloud = "os-correct-overflow"
		f.seedProject(t, cloud, "proj-456")
		f.seedResource(t, instance(cloud, "i-overflow-sound", "proj-456", from, standardSize))
		f.closedMonth(t, from, to, cloud)

		// Two dimensions of six hundred billion each: every single delta fits
		// the column, and the credit note they add up to does not, so the
		// correction fails inside the transaction that had already written its
		// records.
		f.seedResource(t, instance(cloud, "i-overflow", "proj-456", from, largeSize))

		result, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
		if err == nil {
			t.Fatal("Correct() error = nil, want the oversized credit note refused")
		}
		for _, want := range []string{"the statement of " + cloud + "/proj-456", "999999999999.99", "out of range"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Correct() error = %q, want it to name %q", err, want)
			}
		}
		if got := f.readRun(t, result.RunID).status; got != "failed" {
			t.Errorf("status = %q, want failed", got)
		}
		f.assertEmptyRun(t, result.RunID)
	})
}

// TestCorrectReportsAnUpstreamFailure breaks the reporting database under a
// correction. It runs on databases of its own, because what it does to the
// reporting one leaves nothing else able to meter.
func TestCorrectReportsAnUpstreamFailure(t *testing.T) {
	f := newFixtureWith(t, correctionPricingDocument)
	from, to := month(-8)
	const cloud = "os-correct-upstream"
	f.seedProject(t, cloud, "proj-456")
	f.seedResource(t, instance(cloud, "i-upstream", "proj-456", from, standardSize))
	finalized := f.closedMonth(t, from, to, cloud)

	if _, err := f.reporting.Store.Pool().Exec(t.Context(), `DROP TABLE current_resources`); err != nil {
		t.Fatalf("dropping the projection table: %v", err)
	}

	result, err := f.correct(t, runs.CorrectOptions{PeriodFrom: from, PeriodTo: to})
	if err == nil {
		t.Fatal("Correct() error = nil, want the failed read reported")
	}
	if want := "listing the candidate resources"; !strings.Contains(err.Error(), want) {
		t.Errorf("Correct() error = %q, want the read that failed named with %q", err, want)
	}

	if got := f.readRun(t, result.RunID).status; got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
	if got := text(t, f.readStats(t, result.RunID), "error"); got != err.Error() {
		t.Errorf("stats error = %q, want the failure %q, as the reporting side reported it", got, err.Error())
	}
	if got := f.readRun(t, finalized).status; got != "finalized" {
		t.Errorf("the corrected run is %q, want it left finalized", got)
	}
}

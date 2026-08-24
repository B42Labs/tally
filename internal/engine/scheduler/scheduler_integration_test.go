package scheduler_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/scheduler"
	"github.com/b42labs/tally/internal/engine/store/storetest"
)

// The months the cases tick, written down rather than derived from the wall
// clock: what the cases vary is the clock the tick is given, so the months it
// is held against have to stand still.
var (
	marchFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	marchTo   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	// February, for the cases that walk more than one month.
	februaryFrom = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	februaryTo   = marchFrom
)

// graceHours is the window every case leaves a month before it is metered.
const graceHours = 24

// The two clocks around March: one inside its grace window, one exactly at the
// end of that window, which is the first instant the month is metered at.
var (
	inGrace    = marchTo.Add(time.Hour)
	afterGrace = marchTo.Add(graceHours * time.Hour)
)

// errUnpriced stands for whatever makes the metering of one month fail while
// the months beside it are fine.
var errUnpriced = errors.New("the month carries an unpriced resource type")

// fixture is the engine database the cases tick against.
type fixture struct {
	db storetest.DB
}

// newFixture starts the database. It is called once per test function, and
// every case empties the tables first: the walk starts at the earliest period
// row of the table, so a case that left rows behind would put its months into
// the walk of the next one.
func newFixture(t *testing.T) fixture {
	t.Helper()

	return fixture{db: storetest.NewDB(t)}
}

// reset empties the tables the tick reads and writes.
func (f fixture) reset(t *testing.T) {
	t.Helper()

	if _, err := f.db.Store.Pool().Exec(t.Context(), `TRUNCATE billing_periods, runs CASCADE`); err != nil {
		t.Fatalf("emptying the billing periods and runs: %v", err)
	}
}

// tick runs one tick against the fixture, on the test's own context. The case
// that cancels its context calls scheduler.Tick itself.
func (f fixture) tick(t *testing.T, now time.Time, opts scheduler.Options) (scheduler.Report, error) {
	t.Helper()

	return scheduler.Tick(t.Context(), f.db.Store.Pool(), now, opts)
}

// recorder is an Executor that records the months it was called for and leaves
// the completed run a real run would leave behind, which is what the
// finalization half of the tick then has something to close over.
func (f fixture) recorder(t *testing.T, calls *[]string) scheduler.Executor {
	t.Helper()

	return func(_ context.Context, from, to time.Time) (uuid.UUID, error) {
		*calls = append(*calls, period.Format(from))
		return f.seedRun(t, from, to, "completed"), nil
	}
}

// seedPeriod writes a billing period in the status a case starts from.
func (f fixture) seedPeriod(t *testing.T, from, to time.Time, status string) {
	t.Helper()

	if _, err := f.db.Store.Pool().Exec(t.Context(),
		`INSERT INTO billing_periods (period_from, period_to, status) VALUES ($1, $2, $3)`,
		from, to, status,
	); err != nil {
		t.Fatalf("seeding the %s period %s: %v", status, period.Format(from), err)
	}
}

// seedRun writes a regular run of a period in the status a case starts from.
func (f fixture) seedRun(t *testing.T, from, to time.Time, status string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := f.db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, kind, pricing_version, status, completed_at)
		 VALUES ($1, $2, 'regular', 'v1', $3, now()) RETURNING id`, from, to, status,
	).Scan(&id); err != nil {
		t.Fatalf("seeding the %s run of %s: %v", status, period.Format(from), err)
	}
	return id
}

// seedFailedRun writes a regular run of a period that failed at completedAt. The
// instant is the case's rather than the database's clock, because what the
// backoff of a month whose runs keep failing is counted from is the last
// failure, held against the clock the tick is given.
func (f fixture) seedFailedRun(t *testing.T, from, to, completedAt time.Time) {
	t.Helper()

	if _, err := f.db.Store.Pool().Exec(t.Context(),
		`INSERT INTO runs (period_from, period_to, kind, pricing_version, status, started_at, completed_at)
		 VALUES ($1, $2, 'regular', 'v1', 'failed', $3, $3)`, from, to, completedAt,
	); err != nil {
		t.Fatalf("seeding the failed run of %s: %v", period.Format(from), err)
	}
}

// seedStaleRun writes a regular run of a period that reads as one whose process
// ended without completing it: 'running', old enough for the reclaim to take
// it. started_at is the database's clock rather than the case's, because the
// reclaim holds it against now(). Whether a process still stands behind such a
// row is exactly what the age does not say, which is what the case that holds
// the period lock beside it is about.
func (f fixture) seedStaleRun(t *testing.T, from, to time.Time) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := f.db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, kind, pricing_version, status, started_at)
		 VALUES ($1, $2, 'regular', 'v1', 'running', now() - interval '3 hours') RETURNING id`, from, to,
	).Scan(&id); err != nil {
		t.Fatalf("seeding the stale run of %s: %v", period.Format(from), err)
	}
	return id
}

// holdPeriodLock takes the period lock of a month on a connection of its own and
// keeps it until the case ends, which is what a run of another process does: the
// lock is session scoped, so it lives exactly as long as its connection.
func (f fixture) holdPeriodLock(t *testing.T, from time.Time) {
	t.Helper()

	holder, err := pgx.Connect(t.Context(), f.db.URL)
	if err != nil {
		t.Fatalf("opening the connection that holds the period lock: %v", err)
	}
	t.Cleanup(func() {
		if err := holder.Close(context.Background()); err != nil {
			t.Errorf("closing the connection that holds the period lock: %v", err)
		}
	})

	var locked bool
	if err := holder.QueryRow(t.Context(),
		`SELECT pg_try_advisory_lock(hashtextextended('period:' || $1::text, 0))`,
		from.UTC().Format(time.RFC3339)).Scan(&locked); err != nil {
		t.Fatalf("taking the period lock of %s: %v", period.Format(from), err)
	}
	if !locked {
		t.Fatalf("pg_try_advisory_lock = false, want the case to hold the lock of %s", period.Format(from))
	}
}

// periodStatus reads back what a month is stored as.
func (f fixture) periodStatus(t *testing.T, from time.Time) string {
	t.Helper()

	var status string
	if err := f.db.Store.Pool().QueryRow(t.Context(),
		`SELECT status FROM billing_periods WHERE period_from = $1`, from,
	).Scan(&status); err != nil {
		t.Fatalf("reading the period %s: %v", period.Format(from), err)
	}
	return status
}

// runStatus reads back what a run is stored as.
func (f fixture) runStatus(t *testing.T, id uuid.UUID) string {
	t.Helper()

	var status string
	if err := f.db.Store.Pool().QueryRow(t.Context(), `SELECT status FROM runs WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("reading the run %s: %v", id, err)
	}
	return status
}

// countRuns is how many runs a month carries, which is what a case that expects
// no metering at all asserts on.
func (f fixture) countRuns(t *testing.T, from time.Time) int {
	t.Helper()

	var count int
	if err := f.db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM runs WHERE period_from = $1`, from,
	).Scan(&count); err != nil {
		t.Fatalf("counting the runs of %s: %v", period.Format(from), err)
	}
	return count
}

// months is what a report walked, for comparison against the months a case
// expects.
func months(report scheduler.Report) []string {
	walked := make([]string, 0, len(report))
	for _, month := range report {
		walked = append(walked, month.Month)
	}
	return walked
}

// equal compares two lists of months.
func equal(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestTick(t *testing.T) {
	f := newFixture(t)

	t.Run("moves a month that has ended into grace", func(t *testing.T) {
		f.reset(t)
		var calls []string

		// The table is empty, so the tick writes the period row itself: this is
		// the first month a fresh deployment bills.
		report, err := f.tick(t, inGrace, scheduler.Options{
			GraceHours: graceHours,
			Execute:    f.recorder(t, &calls),
		})
		if err != nil {
			t.Fatalf("Tick() error = %v, want nil", err)
		}

		if got := months(report); !equal(got, []string{"2026-03"}) {
			t.Fatalf("Tick() walked %v, want [2026-03]", got)
		}
		if got := report[0].Transition; got != "open -> grace" {
			t.Errorf("Transition = %q, want %q", got, "open -> grace")
		}
		if report[0].RunID != uuid.Nil {
			t.Errorf("RunID = %s, want none: the grace window has not passed", report[0].RunID)
		}
		if got := f.periodStatus(t, marchFrom); got != "grace" {
			t.Errorf("the period 2026-03 is %q, want grace", got)
		}
		if len(calls) != 0 {
			t.Errorf("the executor was called for %v, want not at all inside the grace window", calls)
		}
	})

	t.Run("meters a month whose grace window has passed", func(t *testing.T) {
		f.reset(t)
		f.seedPeriod(t, marchFrom, marchTo, "grace")
		var calls []string

		report, err := f.tick(t, afterGrace, scheduler.Options{
			GraceHours: graceHours,
			Execute:    f.recorder(t, &calls),
		})
		if err != nil {
			t.Fatalf("Tick() error = %v, want nil", err)
		}

		if !equal(calls, []string{"2026-03"}) {
			t.Fatalf("the executor was called for %v, want [2026-03] exactly once", calls)
		}
		if report[0].RunID == uuid.Nil {
			t.Fatal("RunID = none, want the run the tick had metered")
		}
		if got := f.runStatus(t, report[0].RunID); got != "completed" {
			t.Errorf("the reported run is %q, want the completed run of the month", got)
		}
		if got := report[0].Transition; got != "" {
			t.Errorf("Transition = %q, want none: the month was already in grace", got)
		}
		if got := f.periodStatus(t, marchFrom); got != "grace" {
			t.Errorf("the period 2026-03 is %q, want it left in grace: the tick does not finalize on its own", got)
		}
	})

	t.Run("leaves a month that already carries a complete run alone", func(t *testing.T) {
		f.reset(t)
		// February's run is seeded finalized beside a period still in grace,
		// which the lifecycle itself does not produce: what the case is about is
		// which run statuses count as metered.
		f.seedPeriod(t, februaryFrom, februaryTo, "grace")
		f.seedRun(t, februaryFrom, februaryTo, "finalized")
		f.seedPeriod(t, marchFrom, marchTo, "grace")
		f.seedRun(t, marchFrom, marchTo, "completed")
		var calls []string

		report, err := f.tick(t, afterGrace, scheduler.Options{
			GraceHours: graceHours,
			Execute:    f.recorder(t, &calls),
		})
		if err != nil {
			t.Fatalf("Tick() error = %v, want nil", err)
		}

		if len(calls) != 0 {
			t.Errorf("the executor was called for %v, want not at all: both months are metered", calls)
		}
		if got := months(report); !equal(got, []string{"2026-02", "2026-03"}) {
			t.Fatalf("Tick() walked %v, want [2026-02 2026-03]", got)
		}
		for _, month := range report {
			if month.RunID != uuid.Nil {
				t.Errorf("%s reports run %s, want none", month.Month, month.RunID)
			}
		}
	})

	t.Run("closes a month only where the deployment asks for it", func(t *testing.T) {
		f.reset(t)
		f.seedPeriod(t, marchFrom, marchTo, "grace")
		var calls []string
		execute := f.recorder(t, &calls)

		report, err := f.tick(t, afterGrace, scheduler.Options{GraceHours: graceHours, Execute: execute})
		if err != nil {
			t.Fatalf("Tick() error = %v, want nil", err)
		}
		if report[0].Finalized {
			t.Error("Finalized = true, want false: the deployment does not close months on its own")
		}
		if report[0].RunID == uuid.Nil {
			t.Fatal("RunID = none, want the run the tick had metered")
		}
		runID := report[0].RunID
		if got := f.runStatus(t, runID); got != "completed" {
			t.Errorf("the run is %q, want it left completed", got)
		}
		if got := f.periodStatus(t, marchFrom); got != "grace" {
			t.Errorf("the period 2026-03 is %q, want it left in grace", got)
		}

		report, err = f.tick(t, afterGrace, scheduler.Options{
			GraceHours:   graceHours,
			AutoFinalize: true,
			Execute:      execute,
		})
		if err != nil {
			t.Fatalf("Tick() with AutoFinalize error = %v, want nil", err)
		}
		if !equal(calls, []string{"2026-03"}) {
			t.Errorf("the executor was called for %v, want the one call of the first tick", calls)
		}
		if report[0].RunID != uuid.Nil {
			t.Errorf("RunID = %s, want none: the month was metered by the tick before", report[0].RunID)
		}
		if !report[0].Finalized {
			t.Error("Finalized = false, want the tick to report the month it closed")
		}
		if got := f.runStatus(t, runID); got != "finalized" {
			t.Errorf("the run is %q, want finalized", got)
		}
		if got := f.periodStatus(t, marchFrom); got != "finalized" {
			t.Errorf("the period 2026-03 is %q, want finalized", got)
		}
	})

	t.Run("reports a month that failed and walks the rest", func(t *testing.T) {
		f.reset(t)
		f.seedPeriod(t, februaryFrom, februaryTo, "grace")
		f.seedPeriod(t, marchFrom, marchTo, "grace")

		var calls []string
		execute := func(_ context.Context, from, to time.Time) (uuid.UUID, error) {
			calls = append(calls, period.Format(from))
			if from.Equal(februaryFrom) {
				return uuid.Nil, errUnpriced
			}
			return f.seedRun(t, from, to, "completed"), nil
		}

		report, err := f.tick(t, afterGrace, scheduler.Options{GraceHours: graceHours, Execute: execute})
		if !errors.Is(err, errUnpriced) {
			t.Fatalf("Tick() error = %v, want one matching the failure of 2026-02", err)
		}

		if !equal(calls, []string{"2026-02", "2026-03"}) {
			t.Fatalf("the executor was called for %v, want both months", calls)
		}
		if got := months(report); !equal(got, []string{"2026-02", "2026-03"}) {
			t.Fatalf("Tick() walked %v, want [2026-02 2026-03]", got)
		}
		if !errors.Is(report[0].Err, errUnpriced) {
			t.Errorf("the 2026-02 entry reports %v, want the failure of that month", report[0].Err)
		}
		if report[0].RunID != uuid.Nil {
			t.Errorf("the 2026-02 entry reports run %s, want none: its metering failed", report[0].RunID)
		}
		if got := f.periodStatus(t, februaryFrom); got != "grace" {
			t.Errorf("the period 2026-02 is %q, want it left in grace for the next tick", got)
		}
		if report[1].Err != nil {
			t.Errorf("the 2026-03 entry reports %v, want nil", report[1].Err)
		}
		if report[1].RunID == uuid.Nil {
			t.Error("the 2026-03 entry reports no run, want the month after the failed one metered")
		}
	})

	t.Run("a canceled context ends the walk", func(t *testing.T) {
		f.reset(t)
		f.seedPeriod(t, februaryFrom, februaryTo, "grace")
		f.seedPeriod(t, marchFrom, marchTo, "grace")

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var calls []string
		execute := func(_ context.Context, from, to time.Time) (uuid.UUID, error) {
			calls = append(calls, period.Format(from))
			// The tick is stopped the way a drained node stops it: in the middle
			// of the walk, with the month it is on already metered.
			cancel()
			return f.seedRun(t, from, to, "completed"), nil
		}

		report, err := scheduler.Tick(ctx, f.db.Store.Pool(), afterGrace, scheduler.Options{
			GraceHours: graceHours,
			Execute:    execute,
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Tick() error = %v, want one matching context.Canceled", err)
		}

		if !equal(calls, []string{"2026-02"}) {
			t.Fatalf("the executor was called for %v, want only the month the walk got to", calls)
		}
		if got := months(report); !equal(got, []string{"2026-02"}) {
			t.Fatalf("Tick() walked %v, want [2026-02]", got)
		}
		if got := f.countRuns(t, marchFrom); got != 0 {
			t.Errorf("2026-03 carries %d runs, want none: the walk ended before it", got)
		}
	})

	t.Run("holds a month back whose runs keep failing", func(t *testing.T) {
		f.reset(t)
		f.seedPeriod(t, marchFrom, marchTo, "grace")
		// Three failures in a row. Without a wait between them the tick meters
		// the month again every hour for as long as the deployment stands, and
		// each pass reads the whole month out of the reporting database and
		// leaves another run row with a full stats blob behind.
		for i := range 3 {
			f.seedFailedRun(t, marchFrom, marchTo, afterGrace.Add(-time.Duration(i)*time.Hour))
		}
		var calls []string

		report, err := f.tick(t, afterGrace, scheduler.Options{
			GraceHours: graceHours,
			Execute:    f.recorder(t, &calls),
		})
		if err != nil {
			t.Fatalf("Tick() error = %v, want nil", err)
		}

		if len(calls) != 0 {
			t.Errorf("the executor was called for %v, want not at all: the month is waiting out its failures", calls)
		}
		if got := f.countRuns(t, marchFrom); got != 3 {
			t.Errorf("the month carries %d runs, want the 3 seeded ones and no new one", got)
		}
		if want := afterGrace.Add(4 * time.Hour); !report[0].RetryAfter.Equal(want) {
			t.Errorf("RetryAfter = %s, want %s, four hours after the last failure",
				report[0].RetryAfter.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		if got := report[0].Failures; got != 3 {
			t.Errorf("Failures = %d, want the 3 the month carries: an operator reads a stuck month off this", got)
		}
	})

	t.Run("meters a held back month once its wait has passed", func(t *testing.T) {
		f.reset(t)
		f.seedPeriod(t, marchFrom, marchTo, "grace")
		for i := range 3 {
			f.seedFailedRun(t, marchFrom, marchTo, afterGrace.Add(-time.Duration(i)*time.Hour))
		}
		var calls []string

		// A wait is a wait and not a verdict: what fails a month is often a
		// database that was down, and the month is metered again on the first
		// tick past the wait.
		report, err := f.tick(t, afterGrace.Add(5*time.Hour), scheduler.Options{
			GraceHours: graceHours,
			Execute:    f.recorder(t, &calls),
		})
		if err != nil {
			t.Fatalf("Tick() error = %v, want nil", err)
		}

		if !equal(calls, []string{"2026-03"}) {
			t.Fatalf("the executor was called for %v, want [2026-03]: the wait after the last failure has passed", calls)
		}
		if !report[0].RetryAfter.IsZero() {
			t.Errorf("RetryAfter = %s, want none: the month was metered", report[0].RetryAfter.Format(time.RFC3339))
		}
	})

	t.Run("counts the runs it reclaims before it decides to meter again", func(t *testing.T) {
		f.reset(t)
		f.seedPeriod(t, marchFrom, marchTo, "grace")
		// Two runs whose process was killed rather than ended: an OOM kill of
		// this very CronJob leaves exactly this row, and nothing writes the
		// failure down. Reclaimed only by the run the tick is about to start,
		// they would count as no failures at all and the month would be metered
		// -- and killed -- on this pass and the next.
		f.seedStaleRun(t, marchFrom, marchTo)
		f.seedStaleRun(t, marchFrom, marchTo)
		var calls []string

		report, err := f.tick(t, afterGrace, scheduler.Options{
			GraceHours: graceHours,
			Execute:    f.recorder(t, &calls),
		})
		if err != nil {
			t.Fatalf("Tick() error = %v, want nil", err)
		}

		if len(calls) != 0 {
			t.Errorf("the executor was called for %v, want not at all: two runs of the month were just reclaimed", calls)
		}
		if got := report[0].Failures; got != 2 {
			t.Errorf("Failures = %d, want the 2 reclaimed runs counted on the tick that reclaimed them", got)
		}
		if report[0].RetryAfter.IsZero() {
			t.Error("RetryAfter = none, want the month held back")
		}
		if got := f.countRuns(t, marchFrom); got != 2 {
			t.Errorf("the month carries %d runs, want the 2 reclaimed ones and no new one", got)
		}
	})

	t.Run("leaves the runs of a month another process is metering alone", func(t *testing.T) {
		f.reset(t)
		f.seedPeriod(t, marchFrom, marchTo, "grace")
		// A run past the reclaim's age with its process still behind it: what
		// bounds a tally-engine run --period is the size of the month, and a month
		// that meters for longer than the age reads exactly like a killed one.
		live := f.seedStaleRun(t, marchFrom, marchTo)
		f.holdPeriodLock(t, marchFrom)
		var calls []string

		report, err := f.tick(t, afterGrace, scheduler.Options{
			GraceHours: graceHours,
			Execute:    f.recorder(t, &calls),
		})
		// A month somebody else is billing right now is not a failure of this
		// tick: the next one finds the run that process left.
		if err != nil {
			t.Fatalf("Tick() error = %v, want nil", err)
		}

		if got := f.runStatus(t, live); got != "running" {
			t.Errorf("the run of the process holding the lock is %q, want it left running: "+
				"failing it under the run discards the whole pass it is in the middle of", got)
		}
		if len(calls) != 0 {
			t.Errorf("the executor was called for %v, want not at all: the period is locked", calls)
		}
		if report[0].Err != nil {
			t.Errorf("Err = %v, want nil", report[0].Err)
		}
	})

	t.Run("reports a billed month whose run could not give the period lock back", func(t *testing.T) {
		f.reset(t)
		f.seedPeriod(t, marchFrom, marchTo, "grace")

		var billed uuid.UUID
		execute := func(_ context.Context, from, to time.Time) (uuid.UUID, error) {
			// What a connection pooler in transaction pooling mode does to every
			// run: the records are committed and the run is 'completed', and only
			// the release of the session lock did not land.
			billed = f.seedRun(t, from, to, "completed")
			return billed, fmt.Errorf("metering %s: %w", period.Format(from), runs.ErrLockReleaseFailed)
		}

		report, err := f.tick(t, afterGrace, scheduler.Options{GraceHours: graceHours, Execute: execute})
		// The month is billed, so the hourly CronJob has nothing to go red over:
		// a failure here would repeat every hour for a month that is done.
		if err != nil {
			t.Fatalf("Tick() error = %v, want nil: only the lock bookkeeping failed", err)
		}

		if report[0].Err != nil {
			t.Errorf("Err = %v, want nil: the month was metered", report[0].Err)
		}
		if report[0].RunID != billed {
			t.Errorf("RunID = %s, want the committed run %s: an operator has no other way to find it",
				report[0].RunID, billed)
		}
		if !errors.Is(report[0].Warning, runs.ErrLockReleaseFailed) {
			t.Errorf("Warning = %v, want the lock release reported beside the run", report[0].Warning)
		}
	})

	t.Run("leaves a month whose run kept the period lock for the next tick to close", func(t *testing.T) {
		f.reset(t)
		f.seedPeriod(t, marchFrom, marchTo, "grace")

		var billed uuid.UUID
		execute := func(_ context.Context, from, to time.Time) (uuid.UUID, error) {
			billed = f.seedRun(t, from, to, "completed")
			return billed, fmt.Errorf("metering %s: %w", period.Format(from), runs.ErrLockReleaseFailed)
		}

		report, err := f.tick(t, afterGrace, scheduler.Options{
			GraceHours:   graceHours,
			AutoFinalize: true,
			Execute:      execute,
		})
		// The lock the run did not give back is the one a finalization takes, so
		// closing the month in the same breath is how a month that was just billed
		// comes back refused and reddens the CronJob anyway.
		if err != nil {
			t.Fatalf("Tick() error = %v, want nil: only the lock bookkeeping failed", err)
		}
		if report[0].Finalized {
			t.Error("Finalized = true, want the month left for a tick that finds the lock free")
		}
		if !errors.Is(report[0].Warning, runs.ErrLockReleaseFailed) {
			t.Errorf("Warning = %v, want the lock release reported beside the run", report[0].Warning)
		}
		if got := f.periodStatus(t, marchFrom); got != "grace" {
			t.Errorf("the period 2026-03 is %q, want it left in grace", got)
		}

		var calls []string
		report, err = f.tick(t, afterGrace.Add(time.Hour), scheduler.Options{
			GraceHours:   graceHours,
			AutoFinalize: true,
			Execute:      f.recorder(t, &calls),
		})
		if err != nil {
			t.Fatalf("the second Tick() error = %v, want nil", err)
		}
		if len(calls) != 0 {
			t.Errorf("the executor was called for %v, want not at all: the month is billed", calls)
		}
		if !report[0].Finalized {
			t.Error("Finalized = false, want the month closed over the run of the tick before")
		}
		if report[0].RunID != billed {
			t.Errorf("RunID = %s, want the committed run %s", report[0].RunID, billed)
		}
		if got := f.periodStatus(t, marchFrom); got != "finalized" {
			t.Errorf("the period 2026-03 is %q, want finalized", got)
		}
	})

	t.Run("a second tick inside the grace window changes nothing", func(t *testing.T) {
		f.reset(t)
		var calls []string
		execute := f.recorder(t, &calls)

		if _, err := f.tick(t, inGrace, scheduler.Options{GraceHours: graceHours, Execute: execute}); err != nil {
			t.Fatalf("the first Tick() error = %v, want nil", err)
		}

		report, err := f.tick(t, inGrace, scheduler.Options{GraceHours: graceHours, Execute: execute})
		if err != nil {
			t.Fatalf("the second Tick() error = %v, want nil", err)
		}

		if got := months(report); !equal(got, []string{"2026-03"}) {
			t.Fatalf("Tick() walked %v, want [2026-03]: a month with nothing due is reported too", got)
		}
		if got := report[0].Transition; got != "" {
			t.Errorf("Transition = %q, want none: the month was moved by the tick before", got)
		}
		if report[0].RunID != uuid.Nil {
			t.Errorf("RunID = %s, want none", report[0].RunID)
		}
		if report[0].Finalized {
			t.Error("Finalized = true, want false")
		}
		if report[0].Err != nil {
			t.Errorf("Err = %v, want nil", report[0].Err)
		}
		if got := f.periodStatus(t, marchFrom); got != "grace" {
			t.Errorf("the period 2026-03 is %q, want it left in grace", got)
		}
		if got := f.countRuns(t, marchFrom); got != 0 {
			t.Errorf("2026-03 carries %d runs, want none inside its grace window", got)
		}
		if len(calls) != 0 {
			t.Errorf("the executor was called for %v, want not at all", calls)
		}
	})
}

package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/engine/store"
	"github.com/b42labs/tally/internal/engine/store/storetest"
	enginemigrations "github.com/b42labs/tally/migrations/engine"
)

// chainVersions is every version of the embedded engine chain, oldest first.
// The chain is numbered sequentially from 1, so its highest version names the
// whole of it; a gap in the numbering fails the tests below.
var chainVersions = func() []int64 {
	versions := make([]int64, 0, enginemigrations.Version)
	for version := int64(1); version <= enginemigrations.Version; version++ {
		versions = append(versions, version)
	}
	return versions
}()

// wantTables is every table the embedded chain creates, sorted. Goose's own
// bookkeeping table is not part of the schema and the queries below exclude it.
var wantTables = []string{
	"adjustment_records",
	"billing_periods",
	"correction_deltas",
	"pricing_models",
	"project_statements",
	"rated_records",
	"runs",
	"usage_records",
}

// The period every seeded run meters. The month is the concept's worked
// example, which nothing here depends on beyond being a valid half-open month.
var (
	periodFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodTo   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
)

func TestMigrate(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("an empty database receives the whole chain", func(t *testing.T) {
		fresh := db.NewSiblingDB(t, "migrate_fresh")

		applied, err := store.Migrate(t.Context(), fresh)
		if err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if !slices.Equal(applied, chainVersions) {
			t.Errorf("Migrate() = %v, want %v", applied, chainVersions)
		}

		s, err := store.New(t.Context(), fresh)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()

		if tables := publicTables(t, s); !slices.Equal(tables, wantTables) {
			t.Errorf("tables = %v, want the eight of the schema %v", tables, wantTables)
		}
	})

	t.Run("a second run applies nothing", func(t *testing.T) {
		applied, err := store.Migrate(t.Context(), db.URL)
		if err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if len(applied) != 0 {
			t.Errorf("Migrate() = %v, want nothing applied to a database already on the chain", applied)
		}
	})

	t.Run("the chain rolls down to zero and up again", func(t *testing.T) {
		rolledBack, err := store.MigrateDownTo(t.Context(), db.URL, 0)
		if err != nil {
			t.Fatalf("MigrateDownTo(0) error = %v, want nil", err)
		}
		// Newest first, the order a rollback has to run in.
		want := slices.Clone(chainVersions)
		slices.Reverse(want)
		if !slices.Equal(rolledBack, want) {
			t.Errorf("MigrateDownTo(0) = %v, want %v", rolledBack, want)
		}
		if tables := publicTables(t, db.Store); len(tables) != 0 {
			t.Errorf("tables after the rollback = %v, want an empty schema", tables)
		}
		// The tables took their triggers with them. The two functions behind those
		// triggers are the part a rollback has to drop by name, and one left
		// behind fails the next up migration on a duplicate.
		if left := triggerFunctions(t, db.Store); left != 0 {
			t.Errorf("immutability trigger functions after the rollback = %d, want none left", left)
		}

		applied, err := store.Migrate(t.Context(), db.URL)
		if err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if !slices.Equal(applied, chainVersions) {
			t.Errorf("Migrate() = %v, want %v", applied, chainVersions)
		}
		if tables := publicTables(t, db.Store); !slices.Equal(tables, wantTables) {
			t.Errorf("tables after migrating up again = %v, want %v", tables, wantTables)
		}
	})
}

func TestMigrationStatus(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("reports the chain of an untouched database as pending", func(t *testing.T) {
		status, err := store.MigrationStatus(t.Context(), db.NewSiblingDB(t, "status_pending"))
		if err != nil {
			t.Fatalf("MigrationStatus() error = %v, want nil", err)
		}
		want := chainStatus(false)
		if !slices.Equal(status, want) {
			t.Errorf("MigrationStatus() = %v, want %v", status, want)
		}
	})

	t.Run("reports the chain of a migrated database as applied", func(t *testing.T) {
		status, err := store.MigrationStatus(t.Context(), db.URL)
		if err != nil {
			t.Fatalf("MigrationStatus() error = %v, want nil", err)
		}
		want := chainStatus(true)
		if !slices.Equal(status, want) {
			t.Errorf("MigrationStatus() = %v, want %v", status, want)
		}
	})
}

// TestImmutabilityTriggers is D8 at the level it is decided on: a finalized run
// and its records are held immutable by the database, so a bug in the engine
// cannot rewrite what has already left for an ERP.
func TestImmutabilityTriggers(t *testing.T) {
	db := storetest.NewDB(t)
	runID := seedRun(t, db.Store)
	live := seedRecords(t, db.Store, runID, "live")

	t.Run("the records of a completed run can still be changed", func(t *testing.T) {
		for _, row := range live.guarded() {
			if _, err := db.Store.Pool().Exec(t.Context(), row.update, row.id); err != nil {
				t.Errorf("updating %s while the run is completed: %v, want it to go through", row.table, err)
			}
		}

		// A set of its own, because a deleted row cannot serve the finalized half
		// below and the point here is that the delete reaches the row at all.
		doomed := seedRecords(t, db.Store, runID, "doomed")
		for _, row := range doomed.guarded() {
			if _, err := db.Store.Pool().Exec(t.Context(), row.deleteRow(), row.id); err != nil {
				t.Errorf("deleting from %s while the run is completed: %v, want it to go through", row.table, err)
			}
		}
	})

	t.Run("a completed run can be finalized", func(t *testing.T) {
		// The one update the runs trigger leaves open: it reads a row whose status
		// is still 'completed', which is what the WHEN guard tests.
		if _, err := db.Store.Pool().Exec(t.Context(),
			`UPDATE runs SET status = 'finalized', completed_at = now() WHERE id = $1`,
			runID); err != nil {
			t.Fatalf("finalizing the run: %v, want the transition into 'finalized' to go through", err)
		}
	})

	t.Run("the records of a finalized run are immutable", func(t *testing.T) {
		for _, row := range live.guarded() {
			assertRefused(t, db.Store, "updating "+row.table, row.update, row.id,
				"records of finalized run", "are immutable")
			assertRefused(t, db.Store, "deleting from "+row.table, row.deleteRow(), row.id,
				"records of finalized run", "are immutable")
		}
	})

	t.Run("no record can be added to a finalized run", func(t *testing.T) {
		// What a run bills is the sum over its records, so a row appended after
		// finalization moves that sum exactly as a changed row would.
		for _, row := range live.guarded() {
			assertRefused(t, db.Store, "inserting into "+row.table, row.insert, runID,
				"records of finalized run", "are immutable")
		}
	})

	t.Run("no record can be moved into a finalized run", func(t *testing.T) {
		// A run of the next month, so that the record starts out under a run the
		// trigger lets it be written to. One regular run per period is in flight
		// at a time, which is why this one does not meter March again.
		var nextMonth string
		if err := db.Store.Pool().QueryRow(t.Context(),
			`INSERT INTO runs (period_from, period_to, status)
			 VALUES ($1, $2, 'completed')
			 RETURNING id`, periodTo, periodTo.AddDate(0, 1, 0)).Scan(&nextMonth); err != nil {
			t.Fatalf("seeding the run of the next month: %v", err)
		}
		moved := seedRecords(t, db.Store, nextMonth, "next-month")

		assertRefused(t, db.Store, "moving a usage record into the finalized run",
			`UPDATE usage_records SET run_id = (SELECT id FROM runs WHERE status = 'finalized')
			 WHERE id = $1`, moved.usage,
			"records of finalized run", "are immutable")
	})

	t.Run("a finalized run is immutable itself", func(t *testing.T) {
		assertRefused(t, db.Store, "updating the run",
			`UPDATE runs SET status = 'superseded' WHERE id = $1`, runID,
			"is finalized and immutable")
		assertRefused(t, db.Store, "deleting the run",
			`DELETE FROM runs WHERE id = $1`, runID,
			"is finalized and immutable")
	})
}

// raceTimeout bounds both halves of the race below: how long the concurrent
// write is given to reach the lock, and how long it is given to come back once
// the finalize has committed. It is generous because both are decided by a
// container's scheduler, and neither takes anywhere near it when the trigger
// works.
const raceTimeout = 30 * time.Second

// insertUsageRecord writes one usage record for the run it is passed. It is a
// constant because waitForBlockedWrite finds the statement in pg_stat_activity
// by the words it opens with.
const insertUsageRecord = `INSERT INTO usage_records (run_id, cloud, platform, resource_type,
	                                   resource_id, project_id, state, from_ts, to_ts, seconds, usage)
	 VALUES ($1, 'os-prod-eu1', 'openstack', 'instance', 'instance-racing', 'project-racing',
	         'active', $2, $3, 86400, '{}')`

// TestFinalizeDuringARecordWrite is the race the record trigger's lock exists
// for: a finalize that commits between the trigger's read of the run and the
// write it guards. The trigger locks the run without filtering on its status,
// because a status filter is applied below the lock and would drop the row that
// matters here — one this transaction still sees as 'completed' — before
// anything is locked, leaving the finalize free to commit underneath it.
func TestFinalizeDuringARecordWrite(t *testing.T) {
	db := storetest.NewDB(t)
	runID := seedRun(t, db.Store)
	pool := db.Store.Pool()

	// The finalize, held open. Every other transaction reads the run as
	// 'completed' until this one commits.
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("beginning the finalizing transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(),
		`UPDATE runs SET status = 'finalized', completed_at = now() WHERE id = $1`, runID); err != nil {
		t.Fatalf("finalizing the run: %v", err)
	}

	// The record written into that run meanwhile, on a connection of its own.
	written := make(chan error, 1)
	go func() {
		_, err := pool.Exec(t.Context(), insertUsageRecord, runID, periodFrom, periodFrom.Add(24*time.Hour))
		written <- err
	}()
	waitForBlockedWrite(t, db.Store, written)

	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("committing the finalize: %v", err)
	}

	select {
	case err := <-written:
		if err == nil {
			t.Fatal("the record was written into the run the finalize closed, want the trigger to refuse it")
		}
		for _, fragment := range []string{"records of finalized run", "are immutable"} {
			if !strings.Contains(err.Error(), fragment) {
				t.Errorf("the write was refused with %q, want the trigger's %q", err, fragment)
			}
		}
	case <-time.After(raceTimeout):
		t.Fatal("the write never came back after the finalize committed")
	}
}

// waitForBlockedWrite returns once the concurrent write is waiting on a lock,
// which is where the trigger's FOR SHARE puts it while the finalize is still
// open. A write that came back instead was let through against a snapshot the
// finalize had already moved past, which is the bug this guards.
func waitForBlockedWrite(t *testing.T, s *store.Store, written <-chan error) {
	t.Helper()

	deadline := time.Now().Add(raceTimeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-written:
			t.Fatalf("the write returned %v while the finalize was still open, want it held by the trigger's lock", err)
		default:
		}

		var blocked int
		if err := s.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM pg_stat_activity
			 WHERE wait_event_type = 'Lock' AND query LIKE 'INSERT INTO usage_records%'`).Scan(&blocked); err != nil {
			t.Fatalf("querying pg_stat_activity: %v", err)
		}
		if blocked > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("the write never reached the lock while the finalize was open")
}

// chainStatus is the status of the whole chain with every migration in the same
// applied state.
func chainStatus(applied bool) []store.MigrationState {
	states := make([]store.MigrationState, 0, len(chainVersions))
	for _, version := range chainVersions {
		states = append(states, store.MigrationState{Version: version, Applied: applied})
	}
	return states
}

// publicTables is every table of the database s is opened on, sorted. Goose's
// bookkeeping table is left out: it belongs to the migrator rather than to the
// schema, and it outlives a rollback to zero.
func publicTables(t *testing.T, s *store.Store) []string {
	t.Helper()

	rows, err := s.Pool().Query(t.Context(),
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		   AND table_name <> 'goose_db_version_engine'
		 ORDER BY table_name`)
	if err != nil {
		t.Fatalf("querying information_schema.tables: %v", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scanning a table name: %v", err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the table names: %v", err)
	}
	return tables
}

// triggerFunctions counts the two functions the immutability triggers run.
func triggerFunctions(t *testing.T, s *store.Store) int {
	t.Helper()

	var found int
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM pg_proc
		 WHERE proname IN ('forbid_finalized_mutation', 'forbid_finalized_run_mutation')`).
		Scan(&found); err != nil {
		t.Fatalf("querying pg_proc: %v", err)
	}
	return found
}

// seedRun writes the billing period and the completed run that every record
// below hangs off, and returns the run's id.
func seedRun(t *testing.T, s *store.Store) string {
	t.Helper()

	if _, err := s.Pool().Exec(t.Context(),
		`INSERT INTO billing_periods (period_from, period_to) VALUES ($1, $2)`,
		periodFrom, periodTo); err != nil {
		t.Fatalf("seeding the billing period: %v", err)
	}

	var runID string
	if err := s.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, status)
		 VALUES ($1, $2, 'completed')
		 RETURNING id`, periodFrom, periodTo).Scan(&runID); err != nil {
		t.Fatalf("seeding the completed run: %v", err)
	}
	return runID
}

// records holds one seeded row per table the record trigger guards.
type records struct {
	usage      string
	rated      string
	statement  string
	delta      string
	adjustment string
}

// seedRecords writes one row into each of the five tables the record trigger
// guards, all of them belonging to runID. The tag keeps two sets of the same run
// apart: project_statements holds at most one row per (run_id, project_id), so a
// second set needs a project of its own.
func seedRecords(t *testing.T, s *store.Store, runID, tag string) records {
	t.Helper()

	ctx := t.Context()
	pool := s.Pool()
	project := "project-" + tag
	resource := "instance-" + tag

	var seeded records
	if err := pool.QueryRow(ctx,
		`INSERT INTO usage_records (run_id, cloud, platform, resource_type,
		                            resource_id, project_id, state, from_ts,
		                            to_ts, seconds, usage)
		 VALUES ($1, 'os-prod-eu1', 'openstack', 'instance', $2, $3, 'active',
		         $4, $5, 86400, '{"minutes": 1440, "count": 1}')
		 RETURNING id`,
		runID, resource, project, periodFrom, periodFrom.Add(24*time.Hour),
	).Scan(&seeded.usage); err != nil {
		t.Fatalf("seeding the %s usage record: %v", tag, err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO rated_records (run_id, usage_record_id, dimension, amount, currency)
		 VALUES ($1, $2, 'minutes', 12.00, 'EUR')
		 RETURNING id`, runID, seeded.usage).Scan(&seeded.rated); err != nil {
		t.Fatalf("seeding the %s rated record: %v", tag, err)
	}

	if err := pool.QueryRow(ctx,
		`INSERT INTO project_statements (run_id, project_id, document, total, currency)
		 VALUES ($1, $2, '{}', 12.00, 'EUR')
		 RETURNING id`, runID, project).Scan(&seeded.statement); err != nil {
		t.Fatalf("seeding the %s project statement: %v", tag, err)
	}

	// A delta belongs to the correction run that produced it and names the run it
	// corrects. Both are this run here: a run correcting itself is nothing the
	// engine writes, but the column pair the trigger reads is the same either way.
	if err := pool.QueryRow(ctx,
		`INSERT INTO correction_deltas (run_id, corrects_run_id, cloud, platform,
		                                resource_type, resource_id, project_id,
		                                dimension, old_amount, new_amount, delta,
		                                currency)
		 VALUES ($1, $1, 'os-prod-eu1', 'openstack', 'instance', $2, $3,
		         'minutes', 10.00, 12.00, 2.00, 'EUR')
		 RETURNING id`, runID, resource, project).Scan(&seeded.delta); err != nil {
		t.Fatalf("seeding the %s correction delta: %v", tag, err)
	}

	// The relation an adjustment came from lives in the reporting database, so
	// relation_id is provenance rather than a foreign key and any id serves here.
	if err := pool.QueryRow(ctx,
		`INSERT INTO adjustment_records (run_id, project_id, relation_id, relation_type,
		                                 relation_target, type, scope, rate, base,
		                                 amount, currency)
		 VALUES ($1, $2, gen_random_uuid(), 'managed_by', $3, 'discount', 'all',
		         0.150000, 12.00, -1.80, 'EUR')
		 RETURNING id`, runID, project, "partner-"+tag).Scan(&seeded.adjustment); err != nil {
		t.Fatalf("seeding the %s adjustment record: %v", tag, err)
	}
	return seeded
}

// guardedRow is one seeded row of a table the record trigger guards, with the
// statements that change it. update and deleteRow take the row's id, insert
// takes the id of the run the new row would belong to.
type guardedRow struct {
	table  string
	id     string
	update string
	insert string
}

// deleteRow removes the row again, which the trigger guards just like the
// update.
func (g guardedRow) deleteRow() string {
	return "DELETE FROM " + g.table + " WHERE id = $1"
}

// guarded is the seeded set as the five rows the tests work through.
// rated_records comes first because its foreign key points at usage_records:
// deleting the set in this order is the only order that works.
func (r records) guarded() []guardedRow {
	return []guardedRow{
		{
			table: "rated_records", id: r.rated,
			update: `UPDATE rated_records SET amount = amount + 1 WHERE id = $1`,
			insert: `INSERT INTO rated_records (run_id, usage_record_id, dimension, amount, currency)
			         VALUES ($1, (SELECT id FROM usage_records WHERE run_id = $1 LIMIT 1),
			                 'minutes', 4200.00, 'EUR')`,
		},
		{
			table: "usage_records", id: r.usage,
			update: `UPDATE usage_records SET seconds = seconds + 1 WHERE id = $1`,
			insert: `INSERT INTO usage_records (run_id, cloud, platform, resource_type, resource_id,
			                                   project_id, state, from_ts, to_ts, seconds, usage)
			         VALUES ($1, 'os-prod-eu1', 'openstack', 'instance', 'instance-added',
			                 'project-added', 'active', '2026-03-02Z', '2026-03-03Z', 86400, '{}')`,
		},
		{
			table: "project_statements", id: r.statement,
			update: `UPDATE project_statements SET total = total + 1 WHERE id = $1`,
			insert: `INSERT INTO project_statements (run_id, project_id, document, total, currency)
			         VALUES ($1, 'project-added', '{}', 4200.00, 'EUR')`,
		},
		{
			table: "correction_deltas", id: r.delta,
			update: `UPDATE correction_deltas SET delta = delta + 1 WHERE id = $1`,
			insert: `INSERT INTO correction_deltas (run_id, corrects_run_id, cloud, platform,
			                                       resource_type, resource_id, project_id,
			                                       dimension, old_amount, new_amount, delta, currency)
			         VALUES ($1, $1, 'os-prod-eu1', 'openstack', 'instance', 'instance-added',
			                 'project-added', 'minutes', 10.00, 4210.00, 4200.00, 'EUR')`,
		},
		{
			table: "adjustment_records", id: r.adjustment,
			update: `UPDATE adjustment_records SET amount = amount + 1 WHERE id = $1`,
			insert: `INSERT INTO adjustment_records (run_id, project_id, relation_id, relation_type,
			                                        relation_target, type, scope, rate, base,
			                                        amount, currency)
			         VALUES ($1, 'project-added', gen_random_uuid(), 'managed_by', 'partner-added',
			                 'discount', 'all', 0.150000, 4200.00, -630.00, 'EUR')`,
		},
	}
}

// assertRefused runs a statement a trigger has to refuse and checks that the
// refusal is that trigger's own. The fragments matter: a statement that fails
// for some other reason, a foreign key or a typo, would pass a bare "an error
// came back" and prove nothing about immutability.
func assertRefused(t *testing.T, s *store.Store, what, statement, id string, fragments ...string) {
	t.Helper()

	if _, err := s.Pool().Exec(t.Context(), statement, id); err != nil {
		for _, fragment := range fragments {
			if !strings.Contains(err.Error(), fragment) {
				t.Errorf("%s was refused with %q, want the trigger's %q", what, err, fragment)
			}
		}
		return
	}
	t.Errorf("%s went through, want the trigger to refuse it", what)
}

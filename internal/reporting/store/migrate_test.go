package store_test

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
	reportingmigrations "github.com/b42labs/tally/migrations/reporting"
)

// wantTables is every table migration 0001 creates, sorted.
var wantTables = []string{
	"api_tokens",
	"audit_log",
	"current_resources",
	"events",
	"ingest_credentials",
	"project_relations",
	"projects",
	"rejected_events",
	"resource_types",
	"sync_runs",
}

func TestMigrate(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("an empty database receives the whole chain", func(t *testing.T) {
		fresh := db.NewSiblingDB(t, "migrate_fresh")

		applied, err := store.Migrate(t.Context(), fresh)
		if err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if want := []int64{1}; !slices.Equal(applied, want) {
			t.Errorf("Migrate() = %v, want %v", applied, want)
		}

		s, err := store.New(t.Context(), fresh, 2)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()

		rows, err := s.Pool().Query(t.Context(),
			`SELECT table_name FROM information_schema.tables
			 WHERE table_schema = 'public' AND table_name = ANY($1)
			 ORDER BY table_name`, wantTables)
		if err != nil {
			t.Fatalf("querying information_schema.tables: %v", err)
		}
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
		if !slices.Equal(tables, wantTables) {
			t.Errorf("tables = %v, want %v", tables, wantTables)
		}

		var hypertables int
		if err := s.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM timescaledb_information.hypertables
			 WHERE hypertable_name = 'events'`).Scan(&hypertables); err != nil {
			t.Fatalf("querying timescaledb_information.hypertables: %v", err)
		}
		if hypertables != 1 {
			t.Errorf("hypertables named events = %d, want 1", hypertables)
		}

		var compressionJobs int
		if err := s.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM timescaledb_information.jobs
			 WHERE hypertable_name = 'events' AND proc_name = 'policy_compression'`).Scan(&compressionJobs); err != nil {
			t.Fatalf("querying timescaledb_information.jobs: %v", err)
		}
		if compressionJobs != 1 {
			t.Errorf("compression policy jobs for events = %d, want 1", compressionJobs)
		}
	})

	t.Run("a second run applies nothing", func(t *testing.T) {
		applied, err := store.Migrate(t.Context(), db.URL)
		if err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if len(applied) != 0 {
			t.Errorf("Migrate() = %v, want no applied versions", applied)
		}
	})

	t.Run("the chain rolls down to zero and up again", func(t *testing.T) {
		rolledBack, err := store.MigrateDownTo(t.Context(), db.URL, 0)
		if err != nil {
			t.Fatalf("MigrateDownTo(0) error = %v, want nil", err)
		}
		if want := []int64{1}; !slices.Equal(rolledBack, want) {
			t.Errorf("MigrateDownTo(0) = %v, want %v", rolledBack, want)
		}
		if eventsTableExists(t, db) {
			t.Error("the events table survived the down migration")
		}

		if _, err := store.Migrate(t.Context(), db.URL); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if !eventsTableExists(t, db) {
			t.Error("the events table is missing after migrating up again")
		}
	})

	t.Run("concurrent runs against one database do not race", func(t *testing.T) {
		// Both callers see version 0 and both start applying 0001. Without the
		// session lock the loser fails on a relation the winner just created.
		fresh := db.NewSiblingDB(t, "migrate_concurrent")

		const runs = 6
		var wg sync.WaitGroup
		errs := make([]error, runs)
		applied := make([][]int64, runs)
		for i := range runs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				applied[i], errs[i] = store.Migrate(t.Context(), fresh)
			}()
		}
		wg.Wait()

		total := 0
		for i, err := range errs {
			if err != nil {
				t.Errorf("Migrate() error = %v in run %d, want nil", err, i)
			}
			total += len(applied[i])
		}
		if total != 1 {
			t.Errorf("migrations applied across %d concurrent runs = %d, want 1", runs, total)
		}
	})
}

func TestMigrationStatus(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("reports the chain of a migrated database as applied", func(t *testing.T) {
		status, err := store.MigrationStatus(t.Context(), db.URL)
		if err != nil {
			t.Fatalf("MigrationStatus() error = %v, want nil", err)
		}
		want := []store.MigrationState{{Version: reportingmigrations.Version, Applied: true}}
		if !slices.Equal(status, want) {
			t.Errorf("MigrationStatus() = %v, want %v", status, want)
		}
	})

	t.Run("reports the chain of an untouched database as pending", func(t *testing.T) {
		status, err := store.MigrationStatus(t.Context(), db.NewSiblingDB(t, "status_pending"))
		if err != nil {
			t.Fatalf("MigrationStatus() error = %v, want nil", err)
		}
		want := []store.MigrationState{{Version: reportingmigrations.Version, Applied: false}}
		if !slices.Equal(status, want) {
			t.Errorf("MigrationStatus() = %v, want %v", status, want)
		}
	})
}

func TestMigrateUnreachableDatabase(t *testing.T) {
	applied, err := store.Migrate(t.Context(), "postgres://nobody@127.0.0.1:1/none")
	if err == nil {
		t.Fatalf("Migrate() = %v, want an error", applied)
	}
	if prefix := "applying the migrations:"; !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("Migrate() error = %q, want it to start with %q", err, prefix)
	}
}

// eventsTableExists reports whether the hypertable the chain creates first and
// drops last is present, which stands in for the whole schema.
func eventsTableExists(t *testing.T, db storetest.DB) bool {
	t.Helper()

	var exists bool
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT EXISTS (
		     SELECT 1 FROM information_schema.tables
		     WHERE table_schema = 'public' AND table_name = 'events'
		 )`).Scan(&exists); err != nil {
		t.Fatalf("querying information_schema.tables: %v", err)
	}
	return exists
}

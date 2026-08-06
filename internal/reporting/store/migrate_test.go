package store_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
	reportingmigrations "github.com/b42labs/tally/migrations/reporting"
)

// chainVersions is every version of the embedded migration chain, oldest first.
// The chain is numbered sequentially from 1, so its highest version names the
// whole of it; a gap in the numbering fails the tests below.
var chainVersions = func() []int64 {
	versions := make([]int64, 0, reportingmigrations.Version)
	for version := int64(1); version <= reportingmigrations.Version; version++ {
		versions = append(versions, version)
	}
	return versions
}()

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

// wantResourceTypes is the registry migration 0002 seeds: the size schema of
// every OpenStack type of Phase 1, keyed by "platform/resource_type".
var wantResourceTypes = map[string]string{
	"openstack/instance": `{
		"type": "object",
		"required": ["vcpus", "ram_gb", "disk_gb", "flavor"],
		"properties": {"vcpus": {"type": "integer", "minimum": 1},
		               "ram_gb": {"type": "number", "exclusiveMinimum": 0},
		               "disk_gb": {"type": "number", "minimum": 0},
		               "flavor": {"type": "string"}},
		"additionalProperties": true
	}`,
	"openstack/volume": `{
		"type": "object",
		"required": ["size_gb", "type"],
		"properties": {"size_gb": {"type": "number", "exclusiveMinimum": 0},
		               "type": {"type": "string"}},
		"additionalProperties": true
	}`,
	"openstack/floating_ip": `{
		"type": "object",
		"required": ["ip_version"],
		"properties": {"ip_version": {"enum": [4, 6]}},
		"additionalProperties": true
	}`,
	"openstack/image": `{
		"type": "object",
		"required": ["size_gb"],
		"properties": {"size_gb": {"type": "number", "minimum": 0}},
		"additionalProperties": true
	}`,
}

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

		assertSeededResourceTypes(t, s)
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
		// Newest first, the order a rollback has to run in.
		want := slices.Clone(chainVersions)
		slices.Reverse(want)
		if !slices.Equal(rolledBack, want) {
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
		assertSeededResourceTypes(t, db.Store)
	})

	t.Run("rolling back the seed empties the registry", func(t *testing.T) {
		rolledBack, err := store.MigrateDownTo(t.Context(), db.URL, 1)
		if err != nil {
			t.Fatalf("MigrateDownTo(1) error = %v, want nil", err)
		}
		if want := []int64{2}; !slices.Equal(rolledBack, want) {
			t.Errorf("MigrateDownTo(1) = %v, want %v", rolledBack, want)
		}
		var remaining int
		if err := db.Store.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM resource_types`).Scan(&remaining); err != nil {
			t.Fatalf("counting the resource types: %v", err)
		}
		if remaining != 0 {
			t.Errorf("resource types after the rollback = %d, want 0", remaining)
		}

		if _, err := store.Migrate(t.Context(), db.URL); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		assertSeededResourceTypes(t, db.Store)
	})

	t.Run("the seed reaches a database that already registered one of its types", func(t *testing.T) {
		// The operator ran the previous release, whose chain ended at 1, and
		// registered a type by hand to unblock collection. The seed has to leave
		// their row alone rather than fail the whole chain on the duplicate key,
		// which would keep every pod of the new release unready.
		occupied := db.NewSiblingDB(t, "migrate_occupied")
		if _, err := store.Migrate(t.Context(), occupied); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if _, err := store.MigrateDownTo(t.Context(), occupied, 1); err != nil {
			t.Fatalf("MigrateDownTo(1) error = %v, want nil", err)
		}
		s, err := store.New(t.Context(), occupied, 1)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()
		const registered = `{"type": "object", "required": ["vcpus"]}`
		if _, err := s.Pool().Exec(t.Context(),
			`INSERT INTO resource_types (platform, resource_type, size_schema)
			 VALUES ('openstack', 'instance', $1)`, registered); err != nil {
			t.Fatalf("registering the type by hand: %v", err)
		}

		applied, err := store.Migrate(t.Context(), occupied)
		if err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if want := []int64{2}; !slices.Equal(applied, want) {
			t.Errorf("Migrate() = %v, want %v", applied, want)
		}

		var stored []byte
		if err := s.Pool().QueryRow(t.Context(),
			`SELECT size_schema FROM resource_types
			 WHERE platform = 'openstack' AND resource_type = 'instance'`).Scan(&stored); err != nil {
			t.Fatalf("reading the registered type: %v", err)
		}
		if !jsonEqual(t, stored, []byte(registered)) {
			t.Errorf("size schema = %s, want the operator's %s", stored, registered)
		}
		for _, resourceType := range []string{"volume", "floating_ip", "image"} {
			var found int
			if err := s.Pool().QueryRow(t.Context(),
				`SELECT count(*) FROM resource_types
				 WHERE platform = 'openstack' AND resource_type = $1`, resourceType).Scan(&found); err != nil {
				t.Fatalf("counting the seeded %s: %v", resourceType, err)
			}
			if found != 1 {
				t.Errorf("rows for (openstack, %s) = %d, want the seed", resourceType, found)
			}
		}
	})

	t.Run("concurrent runs against one database do not race", func(t *testing.T) {
		// Both callers see version 0 and both start applying the chain. Without
		// the session lock the loser fails on a relation the winner just created.
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
		if total != len(chainVersions) {
			t.Errorf("migrations applied across %d concurrent runs = %d, want %d",
				runs, total, len(chainVersions))
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
		want := chainStatus(true)
		if !slices.Equal(status, want) {
			t.Errorf("MigrationStatus() = %v, want %v", status, want)
		}
	})

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

// chainStatus is the status of the whole chain with every migration in the
// same applied state.
func chainStatus(applied bool) []store.MigrationState {
	states := make([]store.MigrationState, 0, len(chainVersions))
	for _, version := range chainVersions {
		states = append(states, store.MigrationState{Version: version, Applied: applied})
	}
	return states
}

// assertSeededResourceTypes checks that the registry holds the seeds of
// migration 0002 and nothing else.
func assertSeededResourceTypes(t *testing.T, s *store.Store) {
	t.Helper()

	rows, err := s.Pool().Query(t.Context(),
		`SELECT platform || '/' || resource_type, size_schema
		 FROM resource_types ORDER BY 1`)
	if err != nil {
		t.Fatalf("querying resource_types: %v", err)
	}
	var names []string
	schemas := make(map[string][]byte)
	for rows.Next() {
		var name string
		var schema []byte
		if err := rows.Scan(&name, &schema); err != nil {
			t.Fatalf("scanning a resource type: %v", err)
		}
		names = append(names, name)
		schemas[name] = schema
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the resource types: %v", err)
	}

	wantNames := slices.Sorted(maps.Keys(wantResourceTypes))
	if !slices.Equal(names, wantNames) {
		t.Fatalf("resource types = %v, want %v", names, wantNames)
	}
	for _, name := range wantNames {
		want := wantResourceTypes[name]
		if !jsonEqual(t, schemas[name], []byte(want)) {
			t.Errorf("size schema of %s = %s, want %s", name, schemas[name], want)
		}
	}
}

// jsonEqual reports whether two documents describe the same JSON value. The
// database returns jsonb, which keeps neither the key order nor the whitespace
// the migration wrote.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()

	var parsedA, parsedB any
	if err := json.Unmarshal(a, &parsedA); err != nil {
		t.Fatalf("parsing %s: %v", a, err)
	}
	if err := json.Unmarshal(b, &parsedB); err != nil {
		t.Fatalf("parsing %s: %v", b, err)
	}
	return reflect.DeepEqual(parsedA, parsedB)
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

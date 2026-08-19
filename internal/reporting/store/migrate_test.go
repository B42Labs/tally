package store_test

import (
	"encoding/json"
	"maps"
	"math/rand/v2"
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

// afterInit is every version the chain adds on top of the schema 0001 creates,
// oldest first. It is derived rather than spelled out so that adding a migration
// does not need the rollback tests below rewritten.
var afterInit = chainVersions[1:]

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

// wantResourceTypes is the registry the chain seeds: the size schema of every
// OpenStack type of Phase 1, keyed by "platform/resource_type". Migration 0002
// carries the first four, 0006 the load balancer.
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
	// The seed marker is what the rollback of 0006 names its own row by, so it is
	// part of the document the migration has to write.
	"openstack/loadbalancer": `{
		"type": "object",
		"required": ["listeners", "pools"],
		"properties": {"listeners": {"type": "integer", "minimum": 0},
		               "pools": {"type": "integer", "minimum": 0}},
		"additionalProperties": true,
		"x-tally-seed": 6
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

	t.Run("the fleet index reaches a row written before the state bound", func(t *testing.T) {
		// The operator ran the chain up to 0004, whose only index over state was
		// idx_current_resources_type, and a collector reported a state long enough
		// to fit next to a short resource_type there. 0005 puts platform and cloud
		// in front of the same value, so the tuple no longer fits and the build
		// aborts on that one row, leaving an INVALID index behind and the upgrade
		// stuck. The lengths below are picked so the row is over the limit of the
		// new index and well under the limit of the old one, and the values are
		// incompressible because a btree compresses an attribute before it
		// measures the tuple: a repeated character fits where a real state of the
		// same length does not.
		legacy := db.NewSiblingDB(t, "migrate_long_state")
		if _, err := store.Migrate(t.Context(), legacy); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if _, err := store.MigrateDownTo(t.Context(), legacy, 4); err != nil {
			t.Fatalf("MigrateDownTo(4) error = %v, want nil", err)
		}
		s, err := store.New(t.Context(), legacy, 1)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()
		if _, err := s.Pool().Exec(t.Context(),
			`INSERT INTO current_resources
			   (cloud, platform, resource_type, resource_id, project_id, state,
			    last_event_type, last_event_at)
			 VALUES ($1, $2, 'instance', 'r-1', 'p-1', $3,
			         'compute.instance.create.end', now())`,
			noise(250), noise(250), noise(2300)); err != nil {
			t.Fatalf("writing the row the old schema accepted: %v", err)
		}

		applied, err := store.Migrate(t.Context(), legacy)
		if err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		// Everything the chain carries above the 4 it was rolled down to, derived
		// so that a later migration does not need this scenario rewritten.
		if want := chainVersions[4:]; !slices.Equal(applied, want) {
			t.Errorf("Migrate() = %v, want %v", applied, want)
		}

		var length int
		if err := s.Pool().QueryRow(t.Context(),
			`SELECT octet_length(state) FROM current_resources WHERE resource_id = 'r-1'`).Scan(&length); err != nil {
			t.Fatalf("reading the repaired state: %v", err)
		}
		if length > 512 {
			t.Errorf("state = %d bytes, want the 512 the code bounds it to at most", length)
		}
	})

	t.Run("a rollback repeats after one that dropped without recording", func(t *testing.T) {
		// The index migrations run outside a transaction in both directions, so a
		// rollback that dies after dropping the index and before goose deletes the
		// version row leaves the index gone with the migration still recorded as
		// applied: migrating up is then a no-op and the index never comes back.
		// Running the rollback again is the operator's repair, and it only works
		// if the drops tolerate the index they already dropped.
		interrupted := db.NewSiblingDB(t, "migrate_interrupted")
		if _, err := store.Migrate(t.Context(), interrupted); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		s, err := store.New(t.Context(), interrupted, 1)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()
		for _, index := range []string{"idx_events_cloud_project", "idx_rejected_events_received"} {
			if _, err := s.Pool().Exec(t.Context(), "DROP INDEX "+index); err != nil {
				t.Fatalf("dropping %s the way an interrupted rollback leaves it: %v", index, err)
			}
		}

		rolledBack, err := store.MigrateDownTo(t.Context(), interrupted, 0)
		if err != nil {
			t.Fatalf("MigrateDownTo(0) error = %v, want nil", err)
		}
		// Newest first, the order a rollback has to run in.
		want := slices.Clone(chainVersions)
		slices.Reverse(want)
		if !slices.Equal(rolledBack, want) {
			t.Errorf("MigrateDownTo(0) = %v, want %v", rolledBack, want)
		}
	})

	t.Run("the stats index migration repeats after one that built without recording", func(t *testing.T) {
		// 0007 swaps one index for another and runs outside a transaction, so
		// neither direction is atomic: goose records the migration only once both
		// statements have gone through. A concurrent drop waits for every
		// transaction that can still see the index, and current_resources is
		// written from inside the ingest transaction, so a lock_timeout or a lost
		// connection between the build and the drop is ordinary. What that leaves
		// is the index built and the version unrecorded, in either direction. The
		// rerun is the operator's only repair, and it only works if the build
		// tolerates the index a previous run already left behind.
		half := db.NewSiblingDB(t, "migrate_half_built")
		if _, err := store.Migrate(t.Context(), half); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		s, err := store.New(t.Context(), half, 1)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()

		// A rollback that built the fleet index back and died before recording it.
		if _, err := s.Pool().Exec(t.Context(),
			`CREATE INDEX idx_current_resources_fleet
			     ON current_resources (platform, cloud, resource_type, state)`); err != nil {
			t.Fatalf("building the index an interrupted rollback leaves: %v", err)
		}
		if _, err := store.MigrateDownTo(t.Context(), half, 6); err != nil {
			t.Fatalf("MigrateDownTo(6) error = %v, want nil", err)
		}
		if !indexExists(t, s, "idx_current_resources_fleet") {
			t.Error("the fleet index is missing after the rollback")
		}
		if indexExists(t, s, "idx_current_resources_stats") {
			t.Error("the stats index survived the rollback")
		}

		// An upgrade that built the stats index and died before dropping the one
		// it makes redundant.
		if _, err := s.Pool().Exec(t.Context(),
			`CREATE INDEX idx_current_resources_stats
			     ON current_resources (platform, cloud, resource_type, state, project_id)`); err != nil {
			t.Fatalf("building the index an interrupted upgrade leaves: %v", err)
		}
		if _, err := store.Migrate(t.Context(), half); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if !indexExists(t, s, "idx_current_resources_stats") {
			t.Error("the stats index is missing after the repeated upgrade")
		}
		if indexExists(t, s, "idx_current_resources_fleet") {
			t.Error("the fleet index survived the repeated upgrade")
		}
	})

	t.Run("rolling back the seed empties the registry", func(t *testing.T) {
		rolledBack, err := store.MigrateDownTo(t.Context(), db.URL, 1)
		if err != nil {
			t.Fatalf("MigrateDownTo(1) error = %v, want nil", err)
		}
		// Newest first, which is the order a rollback undoes the chain in.
		want := slices.Clone(afterInit)
		slices.Reverse(want)
		if !slices.Equal(rolledBack, want) {
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
		// registered two types by hand to unblock collection. The seed has to
		// leave their rows alone rather than fail the whole chain on the duplicate
		// key, which would keep every pod of the new release unready.
		//
		// openstack/loadbalancer is the pair a production database actually
		// carries: an operator running Octavia before this chain reached 6 had no
		// other way to register it. Its conflict is 0006's, one migration removed
		// from 0002's, and a wrong conflict target in either fails every such
		// upgrade.
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
		byHand := map[string]string{
			// A document of their own, which is the case a rollback that deletes by
			// content already protects.
			"instance": `{"type": "object", "required": ["vcpus"]}`,
			// The document 0006 seeds, without the marker that migration added to
			// it. An operator registering the pair before the chain carried it had
			// nothing else to copy, so byte-identical is the likely case rather
			// than the edge one, and content alone cannot tell their row from the
			// chain's.
			"loadbalancer": `{
				"type": "object",
				"required": ["listeners", "pools"],
				"properties": {"listeners": {"type": "integer", "minimum": 0},
				               "pools": {"type": "integer", "minimum": 0}},
				"additionalProperties": true
			}`,
		}
		for resourceType, registered := range byHand {
			if _, err := s.Pool().Exec(t.Context(),
				`INSERT INTO resource_types (platform, resource_type, size_schema)
				 VALUES ('openstack', $1, $2)`, resourceType, registered); err != nil {
				t.Fatalf("registering %s by hand: %v", resourceType, err)
			}
		}

		applied, err := store.Migrate(t.Context(), occupied)
		if err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if !slices.Equal(applied, afterInit) {
			t.Errorf("Migrate() = %v, want %v", applied, afterInit)
		}

		assertRegisteredByHand := func(t *testing.T) {
			t.Helper()

			for resourceType, registered := range byHand {
				var stored []byte
				if err := s.Pool().QueryRow(t.Context(),
					`SELECT size_schema FROM resource_types
					 WHERE platform = 'openstack' AND resource_type = $1`,
					resourceType).Scan(&stored); err != nil {
					t.Fatalf("reading the registered %s: %v", resourceType, err)
				}
				if !jsonEqual(t, stored, []byte(registered)) {
					t.Errorf("size schema of %s = %s, want the operator's %s",
						resourceType, stored, registered)
				}
			}
		}
		assertRegisteredByHand(t)

		// Every pair the chain seeds is there, the two the operator owns
		// included, derived so a later migration does not need this scenario
		// rewritten.
		for _, name := range slices.Sorted(maps.Keys(wantResourceTypes)) {
			platform, resourceType, _ := strings.Cut(name, "/")
			var found int
			if err := s.Pool().QueryRow(t.Context(),
				`SELECT count(*) FROM resource_types
				 WHERE platform = $1 AND resource_type = $2`,
				platform, resourceType).Scan(&found); err != nil {
				t.Fatalf("counting the seeded %s: %v", name, err)
			}
			if found != 1 {
				t.Errorf("rows for (%s, %s) = %d, want one", platform, resourceType, found)
			}
		}

		// Rolling the release back is where the seed can still take what it never
		// wrote. The operator's document is not the chain's to delete, so the
		// rollback has to leave both rows exactly where the Up found them.
		if _, err := store.MigrateDownTo(t.Context(), occupied, 5); err != nil {
			t.Fatalf("MigrateDownTo(5) error = %v, want nil", err)
		}
		assertRegisteredByHand(t)
	})

	t.Run("rolling the seed back leaves a customized document alone", func(t *testing.T) {
		// size_schema is public and round-trippable: GET answers the stored
		// document verbatim and PUT stores what arrives verbatim. An operator who
		// fetches the seeded load balancer schema, tightens it, and registers it
		// back therefore keeps the seed marker inside a document that is now
		// theirs. Rolling the release back must leave it where it is — deleting it
		// would take the customization with it and leave the cloud's load-balancer
		// ingest with no registered schema at all.
		customized := db.NewSiblingDB(t, "migrate_customized")
		if _, err := store.Migrate(t.Context(), customized); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		s, err := store.New(t.Context(), customized, 1)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()

		const tightened = `{
			"type": "object",
			"required": ["listeners", "pools"],
			"properties": {"listeners": {"type": "integer", "minimum": 0},
			               "pools": {"type": "integer", "minimum": 0}},
			"additionalProperties": false,
			"x-tally-seed": 6
		}`
		if _, err := s.Pool().Exec(t.Context(),
			`UPDATE resource_types SET size_schema = $1
			 WHERE platform = 'openstack' AND resource_type = 'loadbalancer'`,
			tightened); err != nil {
			t.Fatalf("registering the operator's own load balancer schema: %v", err)
		}

		if _, err := store.MigrateDownTo(t.Context(), customized, 5); err != nil {
			t.Fatalf("MigrateDownTo(5) error = %v, want nil", err)
		}

		var stored []byte
		if err := s.Pool().QueryRow(t.Context(),
			`SELECT size_schema FROM resource_types
			 WHERE platform = 'openstack' AND resource_type = 'loadbalancer'`).Scan(&stored); err != nil {
			t.Fatalf("reading the customized load balancer schema after the rollback: %v", err)
		}
		if !jsonEqual(t, stored, []byte(tightened)) {
			t.Errorf("size schema of openstack/loadbalancer = %s, want the operator's %s",
				stored, tightened)
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

// noise is n characters of text pglz gives up on, which is what a column value
// has to be for its stored length to be the length it was written with. The
// generator is seeded so that a failure repeats.
func noise(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

	r := rand.New(rand.NewPCG(1, 2))
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(out)
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

// assertSeededResourceTypes checks that the registry holds the seeds of the
// chain and nothing else.
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

// indexExists says whether the database s is opened on carries an index of that
// name, which is what the migration tests read the index swap of 0007 off.
func indexExists(t *testing.T, s *store.Store, name string) bool {
	t.Helper()

	var exists bool
	if err := s.Pool().QueryRow(t.Context(),
		`SELECT EXISTS (
		     SELECT 1 FROM pg_indexes
		     WHERE schemaname = 'public' AND indexname = $1
		 )`, name).Scan(&exists); err != nil {
		t.Fatalf("querying pg_indexes: %v", err)
	}
	return exists
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

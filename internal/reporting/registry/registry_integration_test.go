package registry_test

import (
	"strings"
	"testing"

	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// TestValidatorAgainstARealDatabase drives the registry against the
// resource_types rows themselves. What the cache has to notice is a row the
// database rewrote, so the timestamp it keys on has to be the one Postgres
// wrote rather than one a stub made up.
func TestValidatorAgainstARealDatabase(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	t.Run("reports a pair no row registers as unregistered", func(t *testing.T) {
		schema, ok, err := registry.New().Validator(t.Context(), q, "openstack", "black_hole")
		if err != nil {
			t.Fatalf("Validator() error = %v, want nil", err)
		}
		if ok {
			t.Error("Validator() found a resource type, want none registered")
		}
		if schema != nil {
			t.Errorf("Validator() schema = %v, want nil", schema)
		}
	})

	t.Run("serves the seeded openstack instance schema", func(t *testing.T) {
		schema, ok, err := registry.New().Validator(t.Context(), q, "openstack", "instance")
		if err != nil {
			t.Fatalf("Validator() error = %v, want nil", err)
		}
		if !ok {
			t.Fatal("Validator() found no resource type, want the seeded one")
		}

		if err := schema.Validate(instance(t, `{"vcpus": 4, "ram_gb": 8, "disk_gb": 40, "flavor": "m1.large"}`)); err != nil {
			t.Errorf("Validate() error = %v, want nil for a complete size", err)
		}
		if err := schema.Validate(instance(t, `{"vcpus": 4, "ram_gb": 8}`)); err == nil {
			t.Error("Validate() error = nil, want a size without disk_gb and flavor rejected")
		}
	})

	t.Run("recompiles when the row was rewritten", func(t *testing.T) {
		const (
			platform     = "openstack"
			resourceType = "share"
			// The size both schemas see. The first one accepts it, the second one
			// does not, so the assertion cannot pass on a stale validator.
			size = `{"size_gb": 10}`
		)
		upsert(t, q, platform, resourceType, `{"type": "object", "required": ["size_gb"]}`)

		reg := registry.New()
		before, ok, err := reg.Validator(t.Context(), q, platform, resourceType)
		if err != nil || !ok {
			t.Fatalf("Validator() = _, %t, %v; want a schema and nil", ok, err)
		}
		if err := before.Validate(instance(t, size)); err != nil {
			t.Fatalf("Validate() error = %v, want the first schema to accept %s", err, size)
		}

		upsert(t, q, platform, resourceType,
			`{"type": "object", "required": ["size_gb", "share_type"]}`)

		after, ok, err := reg.Validator(t.Context(), q, platform, resourceType)
		if err != nil || !ok {
			t.Fatalf("Validator() = _, %t, %v; want a schema and nil", ok, err)
		}
		if err := after.Validate(instance(t, size)); err == nil {
			t.Errorf("Validate() error = nil, want the rewritten schema to reject %s", size)
		}
	})

	t.Run("names a stored schema that does not compile", func(t *testing.T) {
		const resourceType = "corrupt"
		// The registering handler compiles before it writes, so this is a row
		// somebody changed past the API. Every batch naming the pair then fails
		// wholesale, which is why the error says which row is at fault.
		upsert(t, q, "openstack", resourceType, `{"type": 42}`)

		_, ok, err := registry.New().Validator(t.Context(), q, "openstack", resourceType)
		if err == nil {
			t.Fatal("Validator() error = nil, want the compile failure")
		}
		if ok {
			t.Error("Validator() reported the type as registered, want it unresolved")
		}
		if want := "the stored size schema for (openstack, corrupt) does not compile: "; !strings.HasPrefix(err.Error(), want) {
			t.Errorf("Validator() error = %q, want it prefixed %q", err, want)
		}
	})

	t.Run("names itself when the query fails", func(t *testing.T) {
		s, err := store.New(t.Context(), db.URL, 1)
		if err != nil {
			t.Fatalf("store.New() error = %v, want nil", err)
		}
		// A closed pool fails every query with something other than pgx.ErrNoRows,
		// which is the outage the registry has to tell from an unregistered pair.
		s.Close()

		_, ok, err := registry.New().Validator(t.Context(), sqlcgen.New(s.Pool()), "openstack", "instance")
		if err == nil {
			t.Fatal("Validator() error = nil, want the query failure")
		}
		if ok {
			t.Error("Validator() reported the type as registered, want it unresolved")
		}
		if want := "loading the size schema for (openstack, instance): "; !strings.HasPrefix(err.Error(), want) {
			t.Errorf("Validator() error = %q, want it prefixed %q", err, want)
		}
	})
}

// upsert registers raw as the size schema of (platform, resourceType), which is
// what the handler of WP 1.5 does once it exists.
func upsert(t *testing.T, q *sqlcgen.Queries, platform, resourceType, raw string) {
	t.Helper()

	if _, err := q.UpsertResourceType(t.Context(), sqlcgen.UpsertResourceTypeParams{
		Platform:     platform,
		ResourceType: resourceType,
		SizeSchema:   []byte(raw),
	}); err != nil {
		t.Fatalf("UpsertResourceType() error = %v, want nil", err)
	}
}

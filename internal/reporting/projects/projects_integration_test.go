package projects_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/projects"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// Every fixture seeds one platform and two relation types: the type the default
// configuration attributes cost along, and one it does not.
const (
	platform      = "openstack"
	attributing   = "infrastructure_tenant"
	informational = "parent"
)

// attributingTypes is the list the cycle guard is called with, as the default
// configuration spells it.
var attributingTypes = []string{attributing}

// The instants the fixtures work with. A relation starts at relationStart and
// every walk asks about walkAt, so a relation is invisible only when the
// fixture closed it or dated it into the future on purpose.
var (
	relationStart = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	beforeClose   = time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
	closedAt      = time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	walkAt        = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	futureStart   = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
)

// TestTraverse walks real project_relations rows. The subtests share one
// database, so each of them seeds its projects in a cloud of its own.
func TestTraverse(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	t.Run("answers one more level per depth along a chain", func(t *testing.T) {
		ids := seed(t, q, "os-projects-chain", "a", "b", "c")
		ab := relate(t, q, ids["a"], ids["b"], attributing)
		bc := relate(t, q, ids["b"], ids["c"], attributing)

		assertRelated(t, traverse(t, q, ids["a"], 1, nil, walkAt), []projects.Related{
			{ProjectID: ids["b"], RelationType: attributing, Depth: 1, Path: []uuid.UUID{ab}},
		})
		assertRelated(t, traverse(t, q, ids["a"], 2, nil, walkAt), []projects.Related{
			{ProjectID: ids["b"], RelationType: attributing, Depth: 1, Path: []uuid.UUID{ab}},
			{ProjectID: ids["c"], RelationType: attributing, Depth: 2, Path: []uuid.UUID{ab, bc}},
		})
	})

	t.Run("answers nothing below a depth of one", func(t *testing.T) {
		ids := seed(t, q, "os-projects-depth", "a", "b")
		relate(t, q, ids["a"], ids["b"], attributing)

		for _, depth := range []int{0, -1} {
			if got := traverse(t, q, ids["a"], depth, nil, walkAt); len(got) != 0 {
				t.Errorf("Traverse() at depth %d = %v, want nothing", depth, got)
			}
		}
	})

	t.Run("terminates on a cycle without answering the start", func(t *testing.T) {
		ids := seed(t, q, "os-projects-cycle", "a", "b")
		ab := relate(t, q, ids["a"], ids["b"], attributing)
		relate(t, q, ids["b"], ids["a"], attributing)

		// The depth is deeper than the cycle is long, so the walk stops on the
		// visited set rather than on the depth it was given.
		assertRelated(t, traverse(t, q, ids["a"], 5, nil, walkAt), []projects.Related{
			{ProjectID: ids["b"], RelationType: attributing, Depth: 1, Path: []uuid.UUID{ab}},
		})
	})

	t.Run("answers only the relations of the type the walk names", func(t *testing.T) {
		ids := seed(t, q, "os-projects-type", "a", "b", "c")
		ab := relate(t, q, ids["a"], ids["b"], attributing)
		ac := relate(t, q, ids["a"], ids["c"], informational)

		assertRelated(t, traverse(t, q, ids["a"], 2, ref(attributing), walkAt), []projects.Related{
			{ProjectID: ids["b"], RelationType: attributing, Depth: 1, Path: []uuid.UUID{ab}},
		})
		assertRelated(t, traverse(t, q, ids["a"], 2, ref(informational), walkAt), []projects.Related{
			{ProjectID: ids["c"], RelationType: informational, Depth: 1, Path: []uuid.UUID{ac}},
		})
	})

	t.Run("walks neither a closed relation nor one that starts later", func(t *testing.T) {
		ids := seed(t, q, "os-projects-temporal", "a", "b", "c", "d")
		ab := relate(t, q, ids["a"], ids["b"], attributing)
		closeRelation(t, q, ab, ids["a"], closedAt)
		relateFrom(t, q, ids["a"], ids["c"], attributing, futureStart)
		ad := relate(t, q, ids["a"], ids["d"], attributing)

		assertRelated(t, traverse(t, q, ids["a"], 2, nil, walkAt), []projects.Related{
			{ProjectID: ids["d"], RelationType: attributing, Depth: 1, Path: []uuid.UUID{ad}},
		})
		// The same walk at an instant the closed relation was still valid at
		// answers it, which is what closing rather than deleting is for.
		assertRelated(t, traverse(t, q, ids["a"], 2, nil, beforeClose), []projects.Related{
			{ProjectID: ids["b"], RelationType: attributing, Depth: 1, Path: []uuid.UUID{ab}},
			{ProjectID: ids["d"], RelationType: attributing, Depth: 1, Path: []uuid.UUID{ad}},
		})
	})

	t.Run("answers a project two paths reach through the one visited first", func(t *testing.T) {
		ids := seed(t, q, "os-projects-diamond", "a", "b", "c", "d")
		ab := relate(t, q, ids["a"], ids["b"], attributing)
		ac := relate(t, q, ids["a"], ids["c"], attributing)
		bd := relate(t, q, ids["b"], ids["d"], attributing)
		relate(t, q, ids["c"], ids["d"], attributing)

		// b was related before c, so the first level answers b before c and the
		// second one reaches d through b.
		assertRelated(t, traverse(t, q, ids["a"], 2, nil, walkAt), []projects.Related{
			{ProjectID: ids["b"], RelationType: attributing, Depth: 1, Path: []uuid.UUID{ab}},
			{ProjectID: ids["c"], RelationType: attributing, Depth: 1, Path: []uuid.UUID{ac}},
			{ProjectID: ids["d"], RelationType: attributing, Depth: 2, Path: []uuid.UUID{ab, bd}},
		})
	})

	t.Run("answers nothing for a project no relation leaves", func(t *testing.T) {
		ids := seed(t, q, "os-projects-sink", "a", "b")
		// The only relation reaches a rather than leaving it, and the walk is
		// over outgoing relations.
		relate(t, q, ids["b"], ids["a"], attributing)

		if got := traverse(t, q, ids["a"], 2, nil, walkAt); len(got) != 0 {
			t.Errorf("Traverse() = %v, want nothing for a project no relation leaves", got)
		}
	})
}

// TestGuardCycle drives the cycle guard over the shapes a relation creation
// meets: the walk starts at the target of the proposed relation and asks
// whether its source is reachable.
func TestGuardCycle(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	t.Run("refuses a relation the target relates straight back through", func(t *testing.T) {
		ids := seed(t, q, "os-guard-direct", "a", "b")
		relate(t, q, ids["b"], ids["a"], attributing)

		err := projects.GuardCycle(t.Context(), q, ids["a"], ids["b"], attributingTypes)

		if !errors.Is(err, projects.ErrCycle) {
			t.Errorf("GuardCycle() error = %v, want %v", err, projects.ErrCycle)
		}
	})

	t.Run("refuses a relation a chain out of the target reaches back through", func(t *testing.T) {
		ids := seed(t, q, "os-guard-chain", "a", "b", "x")
		relate(t, q, ids["b"], ids["x"], attributing)
		relate(t, q, ids["x"], ids["a"], attributing)

		err := projects.GuardCycle(t.Context(), q, ids["a"], ids["b"], attributingTypes)

		if !errors.Is(err, projects.ErrCycle) {
			t.Errorf("GuardCycle() error = %v, want %v", err, projects.ErrCycle)
		}
	})

	t.Run("takes a relation that closes no cycle", func(t *testing.T) {
		ids := seed(t, q, "os-guard-acyclic", "a", "b", "c", "x")
		relate(t, q, ids["b"], ids["x"], attributing)
		relate(t, q, ids["x"], ids["c"], attributing)

		if err := projects.GuardCycle(t.Context(), q, ids["a"], ids["b"], attributingTypes); err != nil {
			t.Errorf("GuardCycle() error = %v, want nil", err)
		}
	})

	t.Run("walks no relation of a type that attributes no cost", func(t *testing.T) {
		ids := seed(t, q, "os-guard-type", "a", "b")
		relate(t, q, ids["b"], ids["a"], informational)

		if err := projects.GuardCycle(t.Context(), q, ids["a"], ids["b"], attributingTypes); err != nil {
			t.Errorf("GuardCycle() error = %v, want nil for a relation carrying no cost", err)
		}
	})

	t.Run("walks no relation that was closed", func(t *testing.T) {
		ids := seed(t, q, "os-guard-closed", "a", "b")
		ba := relate(t, q, ids["b"], ids["a"], attributing)
		closeRelation(t, q, ba, ids["b"], closedAt)

		if err := projects.GuardCycle(t.Context(), q, ids["a"], ids["b"], attributingTypes); err != nil {
			t.Errorf("GuardCycle() error = %v, want nil for a closed relation", err)
		}
	})

	t.Run("finds no cycle when no type attributes cost", func(t *testing.T) {
		ids := seed(t, q, "os-guard-empty", "a", "b")
		relate(t, q, ids["b"], ids["a"], attributing)

		for name, types := range map[string][]string{"nil": nil, "empty": {}} {
			if err := projects.GuardCycle(t.Context(), q, ids["a"], ids["b"], types); err != nil {
				t.Errorf("GuardCycle() with %s types error = %v, want nil", name, err)
			}
		}
	})
}

// seed inserts one project per name into cloud and answers their ids by name.
func seed(t *testing.T, q *sqlcgen.Queries, cloud string, names ...string) map[string]uuid.UUID {
	t.Helper()

	ids := make(map[string]uuid.UUID, len(names))
	for _, name := range names {
		project, err := q.InsertProject(t.Context(), sqlcgen.InsertProjectParams{
			Platform:   platform,
			Cloud:      cloud,
			ExternalID: name,
			Metadata:   []byte(`{}`),
		})
		if err != nil {
			t.Fatalf("InsertProject(%s, %s) error = %v, want nil", cloud, name, err)
		}
		ids[name] = project.ID
	}
	return ids
}

// relate inserts an active relation valid from relationStart and answers its
// id. The rows of one source are walked in the order they were written in, so
// the order of these calls is what a subtest's assertions rest on.
func relate(t *testing.T, q *sqlcgen.Queries, source, target uuid.UUID, relationType string) uuid.UUID {
	t.Helper()

	return relateFrom(t, q, source, target, relationType, relationStart)
}

// relateFrom inserts an active relation valid from validFrom.
func relateFrom(t *testing.T, q *sqlcgen.Queries, source, target uuid.UUID,
	relationType string, validFrom time.Time,
) uuid.UUID {
	t.Helper()

	relation, err := q.InsertProjectRelation(t.Context(), sqlcgen.InsertProjectRelationParams{
		SourceID:     source,
		TargetID:     target,
		RelationType: relationType,
		Metadata:     []byte(`{}`),
		ValidFrom:    pgtype.Timestamptz{Time: validFrom, Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertProjectRelation() error = %v, want nil", err)
	}
	return relation.ID
}

// closeRelation closes a relation at validTo, which is what a DELETE on it does
// at an instant the test picks rather than at now.
func closeRelation(t *testing.T, q *sqlcgen.Queries, id, source uuid.UUID, validTo time.Time) {
	t.Helper()

	if _, err := q.UpdateProjectRelation(t.Context(), sqlcgen.UpdateProjectRelationParams{
		ID:       id,
		SourceID: source,
		ValidTo:  pgtype.Timestamptz{Time: validTo, Valid: true},
	}); err != nil {
		t.Fatalf("UpdateProjectRelation() error = %v, want nil", err)
	}
}

// traverse runs one walk and fails the test when it errs.
func traverse(t *testing.T, q *sqlcgen.Queries, start uuid.UUID, depth int,
	relationType *string, at time.Time,
) []projects.Related {
	t.Helper()

	got, err := projects.Traverse(t.Context(), q, start, depth, relationType, at)
	if err != nil {
		t.Fatalf("Traverse() error = %v, want nil", err)
	}
	return got
}

// assertRelated fails the test unless the walk answered want in that order.
func assertRelated(t *testing.T, got, want []projects.Related) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Traverse() = %+v, want %+v", got, want)
	}
}

// ref is the type filter of a walk that names one type.
func ref(relationType string) *string {
	return &relationType
}

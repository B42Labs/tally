package projects_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// The audit trail a registration leaves, spelled out here rather than read off
// the package so that a change to either constant fails this test.
const (
	auditObject  = "projects"
	actionCreate = "projects.create"
)

// emptyDocument is the metadata a registration that carries none stores.
const emptyDocument = "{}"

// testActor is who the registrations of this test are attributed to. A refusal
// is checked by counting the audit rows under it, so nothing else writing to
// this database may use it.
const testActor = "test"

// uniqueViolation is the SQLSTATE the registry's unique key raises, which the
// duplicate registration carries out to its caller.
const uniqueViolation = "23505"

// The check the schema carries decision D1 in, and the SQLSTATE it raises. They
// are what refuses the hand-written INSERT the admin CLI's premise -- direct
// database access -- puts within an operator's reach.
const (
	checkViolation       = "23514"
	virtualKeyConstraint = "projects_virtual_key"
)

// TestRegister drives the one registration both the HTTP handler and the admin
// CLI go through. The subtests share one database and register in a cloud of
// their own.
func TestRegister(t *testing.T) {
	db := storetest.NewDB(t)

	if projects.AuditObject != auditObject || projects.ActionCreate != actionCreate {
		t.Fatalf("the audit trail = (%s, %s), want (%s, %s)",
			projects.AuditObject, projects.ActionCreate, auditObject, actionCreate)
	}

	t.Run("registers a meta-project", func(t *testing.T) {
		registration := projects.Registration{
			Platform:   "meta",
			Cloud:      "meta",
			ExternalID: "customer-alpha",
			Name:       pgtype.Text{String: "Customer Alpha", Valid: true},
		}

		stored, err := registerIn(t, db, testActor, registration)
		if err != nil {
			t.Fatalf("Register() error = %v, want nil", err)
		}
		assertRegistered(t, db, stored, registration)
	})

	t.Run("registers a partner", func(t *testing.T) {
		registration := projects.Registration{
			Platform:   "partner",
			Cloud:      "partner",
			ExternalID: "partner-corp",
			Name:       pgtype.Text{String: "Partner Corp", Valid: true},
		}

		stored, err := registerIn(t, db, testActor, registration)
		if err != nil {
			t.Fatalf("Register() error = %v, want nil", err)
		}
		assertRegistered(t, db, stored, registration)
	})

	t.Run("refuses a virtual platform paired with another cloud", func(t *testing.T) {
		before := auditRowsOf(t, db, testActor)

		pairs := []struct {
			platform, cloud, externalID string
		}{
			{"meta", "os-virtual-key", "virtual-key-1"},
			{platform, "meta", "virtual-key-2"},
			{"meta", "partner", "virtual-key-3"},
			{"partner", "meta", "virtual-key-4"},
		}
		for _, pair := range pairs {
			_, err := registerIn(t, db, testActor, projects.Registration{
				Platform:   pair.platform,
				Cloud:      pair.cloud,
				ExternalID: pair.externalID,
			})

			if !errors.Is(err, projects.ErrVirtualKey) {
				t.Errorf("Register(%s, %s) error = %v, want %v",
					pair.platform, pair.cloud, err, projects.ErrVirtualKey)
			}
			if got := projectRows(t, db, pair.cloud, pair.externalID); got != 0 {
				t.Errorf("Register(%s, %s) left %d projects rows, want 0",
					pair.platform, pair.cloud, got)
			}
		}

		if got := auditRowsOf(t, db, testActor); got != before {
			t.Errorf("the audit rows of %s = %d, want %d, the count before the refusals",
				testActor, got, before)
		}
	})

	t.Run("refuses a virtual pair the database is handed directly", func(t *testing.T) {
		// Register is not the only writer an operator has: the admin CLI exists
		// because operators hold direct database access, so the rule is the
		// schema's as well as the package's.
		pairs := []struct {
			platform, cloud, externalID string
		}{
			{"meta", "os-virtual-check", "virtual-check-1"},
			{platform, "meta", "virtual-check-2"},
			{"meta", "partner", "virtual-check-3"},
			{"partner", "meta", "virtual-check-4"},
		}
		for _, pair := range pairs {
			_, err := db.Store.Pool().Exec(t.Context(),
				"INSERT INTO projects (platform, cloud, external_id) VALUES ($1, $2, $3)",
				pair.platform, pair.cloud, pair.externalID)

			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != checkViolation ||
				pgErr.ConstraintName != virtualKeyConstraint {
				t.Errorf("INSERT (%s, %s) error = %v, want %s on %s",
					pair.platform, pair.cloud, err, checkViolation, virtualKeyConstraint)
			}
			if got := projectRows(t, db, pair.cloud, pair.externalID); got != 0 {
				t.Errorf("INSERT (%s, %s) left %d projects rows, want 0",
					pair.platform, pair.cloud, got)
			}
		}
	})

	t.Run("refuses a pair the registry holds", func(t *testing.T) {
		held := projects.Registration{
			Platform:   platform,
			Cloud:      "os-register-dup",
			ExternalID: "dup",
			Name:       pgtype.Text{String: "first", Valid: true},
		}
		first, err := registerIn(t, db, testActor, held)
		if err != nil {
			t.Fatalf("Register() error = %v, want nil", err)
		}
		held.Name = pgtype.Text{String: "second", Valid: true}

		_, err = registerIn(t, db, testActor, held)

		if !errors.Is(err, projects.ErrAlreadyRegistered) {
			t.Fatalf("Register() error = %v, want %v", err, projects.ErrAlreadyRegistered)
		}
		// The violation travels out with the sentinel, so a caller that wants the
		// constraint the write collided with still has it.
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != uniqueViolation {
			t.Errorf("Register() error = %v, want one carrying SQLSTATE %s", err, uniqueViolation)
		}
		stored, err := sqlcgen.New(db.Store.Pool()).GetProject(t.Context(), first.ID)
		if err != nil {
			t.Fatalf("GetProject() error = %v, want nil", err)
		}
		if stored.Name.String != "first" {
			t.Errorf("the held row's name = %q, want %q", stored.Name.String, "first")
		}
		if got := projectRows(t, db, "os-register-dup", "dup"); got != 1 {
			t.Errorf("the projects rows of (os-register-dup, dup) = %d, want 1", got)
		}
	})

	t.Run("stores the empty object for absent metadata", func(t *testing.T) {
		absent := map[string]json.RawMessage{
			"metadata-nil":   nil,
			"metadata-empty": {},
		}
		for externalID, metadata := range absent {
			stored, err := registerIn(t, db, testActor, projects.Registration{
				Platform:   platform,
				Cloud:      "os-register-metadata",
				ExternalID: externalID,
				Metadata:   metadata,
			})
			if err != nil {
				t.Fatalf("Register(%s) error = %v, want nil", externalID, err)
			}
			if got := string(stored.Metadata); got != emptyDocument {
				t.Errorf("Register(%s) stored metadata %s, want %s", externalID, got, emptyDocument)
			}
		}

		stored, err := registerIn(t, db, testActor, projects.Registration{
			Platform:   platform,
			Cloud:      "os-register-metadata",
			ExternalID: "metadata-document",
			Metadata:   json.RawMessage(`{"tier":"gold"}`),
		})
		if err != nil {
			t.Fatalf("Register() error = %v, want nil", err)
		}
		// JSONB stores the document rather than the bytes it arrived as, so the
		// comparison is over the decoded one.
		var document map[string]any
		if err := json.Unmarshal(stored.Metadata, &document); err != nil {
			t.Fatalf("decoding the stored metadata %s: %v", stored.Metadata, err)
		}
		if want := map[string]any{"tier": "gold"}; !reflect.DeepEqual(document, want) {
			t.Errorf("Register() stored metadata %+v, want %+v", document, want)
		}
	})

	t.Run("rolls the row back when the audit insert fails", func(t *testing.T) {
		ctx := t.Context()
		tx, err := db.Store.Pool().Begin(ctx)
		if err != nil {
			t.Fatalf("Begin() error = %v, want nil", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		// The DDL is transactional, so the rollback below hands the table back to
		// the subtests that come after this one.
		if _, err := tx.Exec(ctx, "DROP TABLE audit_log"); err != nil {
			t.Fatalf("dropping audit_log: %v", err)
		}

		_, err = projects.Register(ctx, sqlcgen.New(tx), testActor, projects.Registration{
			Platform:   platform,
			Cloud:      "os-register-audit",
			ExternalID: "audit",
		})

		if err == nil || !strings.Contains(err.Error(), "audit log row") {
			t.Fatalf("Register() error = %v, want one naming the audit log row", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("Rollback() error = %v, want nil", err)
		}
		if got := projectRows(t, db, "os-register-audit", "audit"); got != 0 {
			t.Errorf("the projects rows of (os-register-audit, audit) = %d, want 0", got)
		}
	})

	t.Run("wraps an insert that fails for another reason", func(t *testing.T) {
		ctx := t.Context()
		before := auditRowsOf(t, db, testActor)
		tx, err := db.Store.Pool().Begin(ctx)
		if err != nil {
			t.Fatalf("Begin() error = %v, want nil", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("Rollback() error = %v, want nil", err)
		}

		_, err = projects.Register(ctx, sqlcgen.New(tx), testActor, projects.Registration{
			Platform:   platform,
			Cloud:      "os-register-closed",
			ExternalID: "closed",
		})

		if !errors.Is(err, pgx.ErrTxClosed) {
			t.Fatalf("Register() error = %v, want %v", err, pgx.ErrTxClosed)
		}
		const prefix = "registering (os-register-closed, closed):"
		if !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("Register() error = %v, want one starting with %q", err, prefix)
		}
		if got := projectRows(t, db, "os-register-closed", "closed"); got != 0 {
			t.Errorf("the projects rows of (os-register-closed, closed) = %d, want 0", got)
		}
		if got := auditRowsOf(t, db, testActor); got != before {
			t.Errorf("the audit rows of %s = %d, want %d, the count before the call",
				testActor, got, before)
		}
	})
}

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

// registerIn runs one registration in a transaction of its own and answers what
// it stored together with its error, which is what most of the subtests assert
// on.
func registerIn(t *testing.T, db storetest.DB, actor string,
	r projects.Registration,
) (sqlcgen.Project, error) {
	t.Helper()

	var stored sqlcgen.Project
	err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		stored, err = projects.Register(t.Context(), sqlcgen.New(tx), actor, r)
		return err
	})
	return stored, err
}

// assertRegistered fails the test unless the stored row carries what was
// registered, the empty metadata document, and exactly one audit entry.
func assertRegistered(t *testing.T, db storetest.DB, stored sqlcgen.Project,
	r projects.Registration,
) {
	t.Helper()

	if stored.Platform != r.Platform || stored.Cloud != r.Cloud ||
		stored.ExternalID != r.ExternalID {
		t.Errorf("Register() stored (%s, %s, %s), want (%s, %s, %s)",
			stored.Platform, stored.Cloud, stored.ExternalID,
			r.Platform, r.Cloud, r.ExternalID)
	}
	if stored.Name != r.Name {
		t.Errorf("Register() stored the name %+v, want %+v", stored.Name, r.Name)
	}
	if got := string(stored.Metadata); got != emptyDocument {
		t.Errorf("Register() stored metadata %s, want %s", got, emptyDocument)
	}

	want := []auditEntry{{actor: testActor, action: actionCreate, objectType: auditObject}}
	if got := auditEntries(t, db, stored.ID.String()); !reflect.DeepEqual(got, want) {
		t.Errorf("the audit entries of the registration = %+v, want %+v", got, want)
	}
}

// projectRows counts the registry rows holding one key pair, which is how a
// refusal is checked to have written nothing.
func projectRows(t *testing.T, db storetest.DB, cloud, externalID string) int {
	t.Helper()

	var found int
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM projects WHERE cloud = $1 AND external_id = $2`,
		cloud, externalID,
	).Scan(&found); err != nil {
		t.Fatalf("counting the projects rows of (%s, %s): %v", cloud, externalID, err)
	}
	return found
}

// auditEntry is an audit_log row as the assertions read it back.
type auditEntry struct {
	actor      string
	action     string
	objectType string
}

// auditEntries returns every audit_log row written about objectID, oldest
// first.
func auditEntries(t *testing.T, db storetest.DB, objectID string) []auditEntry {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT actor, action, object_type FROM audit_log WHERE object_id = $1 ORDER BY id`,
		objectID)
	if err != nil {
		t.Fatalf("reading the audit rows of %s: %v", objectID, err)
	}
	defer rows.Close()

	var found []auditEntry
	for rows.Next() {
		var entry auditEntry
		if err := rows.Scan(&entry.actor, &entry.action, &entry.objectType); err != nil {
			t.Fatalf("scanning an audit row: %v", err)
		}
		found = append(found, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the audit rows of %s: %v", objectID, err)
	}
	return found
}

// auditRowsOf counts what one actor left in the log. A refusal has no object id
// to look under, so the count is what says it wrote nothing.
func auditRowsOf(t *testing.T, db storetest.DB, actor string) int {
	t.Helper()

	var found int
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM audit_log WHERE actor = $1`, actor,
	).Scan(&found); err != nil {
		t.Fatalf("counting the audit rows of %s: %v", actor, err)
	}
	return found
}

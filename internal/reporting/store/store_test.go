package store_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// errCallback is what a failing WithTx callback returns, so a test can prove the
// error reaches the caller unchanged rather than wrapped.
var errCallback = errors.New("callback failed")

// uniqueViolation is the SQLSTATE Postgres reports for a broken unique index.
const uniqueViolation = "23505"

func TestNewBoundsThePool(t *testing.T) {
	db := storetest.NewDB(t)

	// Left to itself pgxpool sizes the pool after the node's CPU count, which
	// says nothing about the database's connection budget.
	const maxConns = 3
	s, err := store.New(t.Context(), db.URL, maxConns)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	defer s.Close()

	if got := s.Pool().Config().MaxConns; got != maxConns {
		t.Errorf("pool MaxConns = %d, want %d", got, maxConns)
	}
}

func TestReady(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("a migrated database is ready", func(t *testing.T) {
		if err := db.Store.Ready(t.Context()); err != nil {
			t.Errorf("Ready() error = %v, want nil", err)
		}
	})

	t.Run("a reachable database without the schema is not ready", func(t *testing.T) {
		// This is what a fresh deployment reaches before anyone migrated: the
		// pool connects, so Ping passes and readiness must not.
		unmigrated, err := store.New(t.Context(), db.NewSiblingDB(t, "ready_unmigrated"), 1)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer unmigrated.Close()

		if err := unmigrated.Ping(t.Context()); err != nil {
			t.Fatalf("Ping() error = %v, want nil for a database that is up", err)
		}
		if err := unmigrated.Ready(t.Context()); err == nil {
			t.Error("Ready() error = nil, want the missing schema reported")
		}
	})

	t.Run("a database behind the build's schema is not ready", func(t *testing.T) {
		behind := db.NewSiblingDB(t, "ready_behind")
		if _, err := store.Migrate(t.Context(), behind); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}
		if _, err := store.MigrateDownTo(t.Context(), behind, 0); err != nil {
			t.Fatalf("MigrateDownTo(0) error = %v, want nil", err)
		}

		s, err := store.New(t.Context(), behind, 1)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()

		err = s.Ready(t.Context())
		if err == nil {
			t.Fatal("Ready() error = nil, want the schema version reported")
		}
		if want := "schema version"; !strings.Contains(err.Error(), want) {
			t.Errorf("Ready() error = %q, want it to mention %q", err, want)
		}
	})

	t.Run("a failed read is forgiven once the schema check has passed", func(t *testing.T) {
		// A read that fails says nothing about the schema. Dropping the table
		// stands in for that — a revoked grant reads the same way — and a pod
		// the database has already answered for has to stay ready through it.
		forgiving := db.NewSiblingDB(t, "ready_read_failure")
		if _, err := store.Migrate(t.Context(), forgiving); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}

		s, err := store.New(t.Context(), forgiving, 1)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()

		if err := s.Ready(t.Context()); err != nil {
			t.Fatalf("Ready() error = %v, want nil for a migrated database", err)
		}

		if _, err := s.Pool().Exec(t.Context(), "DROP TABLE goose_db_version"); err != nil {
			t.Fatalf("dropping the goose bookkeeping table: %v", err)
		}
		if err := s.Ready(t.Context()); err != nil {
			t.Errorf("Ready() error = %v, want nil once the schema check has passed", err)
		}
	})

	t.Run("a rollback takes an already ready store out of rotation", func(t *testing.T) {
		// The other direction of the same probe: a read that succeeds and
		// reports a schema this build cannot serve is what an operator running
		// migrate-down-to — or a restore to a pre-migration snapshot — leaves
		// behind, and every handler that touches a table fails on it.
		rolledBack := db.NewSiblingDB(t, "ready_rolled_back")
		if _, err := store.Migrate(t.Context(), rolledBack); err != nil {
			t.Fatalf("Migrate() error = %v, want nil", err)
		}

		s, err := store.New(t.Context(), rolledBack, 1)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()

		if err := s.Ready(t.Context()); err != nil {
			t.Fatalf("Ready() error = %v, want nil for a migrated database", err)
		}

		if _, err := store.MigrateDownTo(t.Context(), rolledBack, 0); err != nil {
			t.Fatalf("MigrateDownTo(0) error = %v, want nil", err)
		}

		err = s.Ready(t.Context())
		if err == nil {
			t.Fatal("Ready() error = nil, want the rolled back schema reported")
		}
		if want := "schema version"; !strings.Contains(err.Error(), want) {
			t.Errorf("Ready() error = %q, want it to mention %q", err, want)
		}
	})
}

func TestStore(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("WithTx commits when the callback succeeds", func(t *testing.T) {
		const cloud = "os-commit"

		if err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(),
				`INSERT INTO projects (platform, cloud, external_id) VALUES ('openstack', $1, 'p1')`, cloud)
			return err
		}); err != nil {
			t.Fatalf("WithTx() error = %v, want nil", err)
		}

		if got := countProjects(t, db, cloud); got != 1 {
			t.Errorf("projects in cloud %q = %d, want 1", cloud, got)
		}
	})

	t.Run("WithTx rolls back and returns the callback error unchanged", func(t *testing.T) {
		const cloud = "os-rollback"

		err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(),
				`INSERT INTO projects (platform, cloud, external_id) VALUES ('openstack', $1, 'p1')`, cloud); err != nil {
				return err
			}
			return errCallback
		})
		if !errors.Is(err, errCallback) {
			t.Fatalf("WithTx() error = %v, want %v", err, errCallback)
		}
		if err != errCallback {
			t.Errorf("WithTx() error = %#v, want the callback's own error value", err)
		}

		if got := countProjects(t, db, cloud); got != 0 {
			t.Errorf("projects in cloud %q = %d, want 0 after the rollback", cloud, got)
		}
	})

	t.Run("WithTx rolls back and re-raises a panicking callback", func(t *testing.T) {
		const (
			cloud     = "os-panic"
			wantPanic = "callback panicked"
		)

		func() {
			defer func() {
				if got := recover(); got != wantPanic {
					t.Errorf("recover() = %v, want %q", got, wantPanic)
				}
			}()

			_ = db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
				if _, err := tx.Exec(t.Context(),
					`INSERT INTO projects (platform, cloud, external_id) VALUES ('openstack', $1, 'p1')`, cloud); err != nil {
					t.Errorf("inserting inside the transaction: %v", err)
				}
				panic(wantPanic)
			})
			t.Error("WithTx() returned, want the panic to escape")
		}()

		if got := countProjects(t, db, cloud); got != 0 {
			t.Errorf("projects in cloud %q = %d, want 0 after the rollback", cloud, got)
		}
	})

	t.Run("WithTx reports a closed pool at begin", func(t *testing.T) {
		closed, err := store.New(t.Context(), db.URL, 1)
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		closed.Close()

		err = closed.WithTx(t.Context(), func(pgx.Tx) error {
			t.Error("the callback ran on a closed pool")
			return nil
		})
		if err == nil {
			t.Fatal("WithTx() error = nil, want an error")
		}
		if prefix := "begin transaction:"; !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("WithTx() error = %q, want it to start with %q", err, prefix)
		}
	})

	t.Run("uq_relations_active allows one open relation per pair and type", func(t *testing.T) {
		const cloud = "os-relations"
		source := insertProject(t, db, cloud, "source")
		target := insertProject(t, db, cloud, "target")

		openRelation := func() error {
			_, err := db.Store.Pool().Exec(t.Context(),
				`INSERT INTO project_relations (source_id, target_id, relation_type)
				 VALUES ($1, $2, 'reseller')`, source, target)
			return err
		}

		if err := openRelation(); err != nil {
			t.Fatalf("opening the first relation: %v", err)
		}

		err := openRelation()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("opening a second relation: error = %v, want a Postgres error", err)
		}
		if pgErr.Code != uniqueViolation || pgErr.ConstraintName != "uq_relations_active" {
			t.Errorf("opening a second relation: code = %s on %q, want %s on %q",
				pgErr.Code, pgErr.ConstraintName, uniqueViolation, "uq_relations_active")
		}

		if _, err := db.Store.Pool().Exec(t.Context(),
			`UPDATE project_relations SET valid_to = now()
			 WHERE source_id = $1 AND target_id = $2 AND valid_to IS NULL`, source, target); err != nil {
			t.Fatalf("closing the first relation: %v", err)
		}
		if err := openRelation(); err != nil {
			t.Errorf("opening a relation after closing the previous one: %v", err)
		}
	})

	t.Run("the generated queries carry a token's project ids through Postgres", func(t *testing.T) {
		const cloud = "os-tokens"
		projectIDs := []uuid.UUID{
			insertProject(t, db, cloud, "first"),
			insertProject(t, db, cloud, "second"),
		}

		queries := sqlcgen.New(db.Store.Pool())
		id, err := queries.CreateAPIToken(t.Context(), sqlcgen.CreateAPITokenParams{
			TokenHash:   "hash-of-a-project-token",
			Role:        "project",
			ProjectIds:  projectIDs,
			Description: pgtype.Text{String: "integration test", Valid: true},
		})
		if err != nil {
			t.Fatalf("CreateAPIToken() error = %v, want nil", err)
		}

		token, err := queries.GetAPITokenByTokenHash(t.Context(), "hash-of-a-project-token")
		if err != nil {
			t.Fatalf("GetAPITokenByTokenHash() error = %v, want nil", err)
		}
		if token.ID != id {
			t.Errorf("token id = %s, want %s", token.ID, id)
		}
		if !slices.Equal(token.ProjectIds, projectIDs) {
			t.Errorf("token project ids = %v, want %v", token.ProjectIds, projectIDs)
		}

		refs, err := queries.GetProjectRefsByIDs(t.Context(), token.ProjectIds)
		if err != nil {
			t.Fatalf("GetProjectRefsByIDs() error = %v, want nil", err)
		}
		if len(refs) != len(projectIDs) {
			t.Errorf("project refs = %d, want %d", len(refs), len(projectIDs))
		}

		if err := queries.RevokeAPIToken(t.Context(), id); err != nil {
			t.Fatalf("RevokeAPIToken() error = %v, want nil", err)
		}
		if _, err := queries.GetAPITokenByTokenHash(t.Context(), "hash-of-a-project-token"); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("GetAPITokenByTokenHash() after revoking: error = %v, want %v", err, pgx.ErrNoRows)
		}
	})
}

// insertProject adds a project and returns the id the database generated.
func insertProject(t *testing.T, db storetest.DB, cloud, externalID string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO projects (platform, cloud, external_id)
		 VALUES ('openstack', $1, $2) RETURNING id`, cloud, externalID).Scan(&id); err != nil {
		t.Fatalf("inserting project %s/%s: %v", cloud, externalID, err)
	}
	return id
}

// countProjects reports how many projects a cloud has, which is how the
// transaction tests tell a commit from a rollback.
func countProjects(t *testing.T, db storetest.DB, cloud string) int {
	t.Helper()

	var count int
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM projects WHERE cloud = $1`, cloud).Scan(&count); err != nil {
		t.Fatalf("counting projects in cloud %s: %v", cloud, err)
	}
	return count
}

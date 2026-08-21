package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/b42labs/tally/internal/engine/store"
	"github.com/b42labs/tally/internal/engine/store/storetest"
	enginemigrations "github.com/b42labs/tally/migrations/engine"
)

// TestCheckSchema pins the gate every database-backed subcommand runs before
// its first query. What it has to refuse is not only a database on an older
// migration than the build, but every answer that is not one: a gate that
// passes a read it could not make is a gate that disables itself on exactly the
// deployment it exists for.
func TestCheckSchema(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("passes a database on the chain", func(t *testing.T) {
		if err := db.Store.CheckSchema(t.Context()); err != nil {
			t.Errorf("CheckSchema() error = %v, want nil", err)
		}
	})

	t.Run("refuses a database the migrator never ran against", func(t *testing.T) {
		// Not even the migrator's bookkeeping, which is the state an image
		// deployed ahead of its migrate step finds. Reading the version fails
		// with the table missing, and that is version 0 rather than a pass.
		s, err := store.New(t.Context(), db.NewSiblingDB(t, "check_schema_empty"))
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()

		err = s.CheckSchema(t.Context())
		if err == nil {
			t.Fatal("CheckSchema() error = nil, want the empty database refused")
		}
		want := fmt.Sprintf("the database is on schema version 0, this build needs %d",
			enginemigrations.Version)
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CheckSchema() error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("refuses a database that carries another chain", func(t *testing.T) {
		// The reporting database, which one copy-pasted connection string
		// reaches in a deployment that carries both. It books its migrations in
		// goose's default table, and the number there is a position in its
		// chain: read as this build's it clears the gate, and every subcommand
		// then dies on a relation the engine's own chain was going to create.
		s, err := store.New(t.Context(), db.NewSiblingDB(t, "check_schema_other_chain"))
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()

		if _, err := s.Pool().Exec(t.Context(),
			`CREATE TABLE goose_db_version (
			   id SERIAL PRIMARY KEY, version_id BIGINT NOT NULL, is_applied BOOLEAN NOT NULL,
			   tstamp TIMESTAMP NOT NULL DEFAULT now())`); err != nil {
			t.Fatalf("creating the other chain's bookkeeping table: %v", err)
		}
		if _, err := s.Pool().Exec(t.Context(),
			`INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, true)`,
			enginemigrations.Version+1); err != nil {
			t.Fatalf("recording the other chain's version: %v", err)
		}

		err = s.CheckSchema(t.Context())
		if err == nil {
			t.Fatal("CheckSchema() error = nil, want the other service's database refused")
		}
		want := fmt.Sprintf("the database is on schema version 0, this build needs %d",
			enginemigrations.Version)
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CheckSchema() error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("reports a read that never answered", func(t *testing.T) {
		// A database that cannot be reached at all. The read fails for a reason
		// that says nothing about the schema, and passing it would skip the
		// check for that invocation instead of reporting why it could not run.
		s, err := store.New(t.Context(), "postgres://nobody@127.0.0.1:1/none")
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		defer s.Close()

		err = s.CheckSchema(t.Context())
		if err == nil {
			t.Fatal("CheckSchema() error = nil, want the failed read reported")
		}
		if want := "reading the schema version:"; !strings.Contains(err.Error(), want) {
			t.Errorf("CheckSchema() error = %q, want it to contain %q", err, want)
		}
	})
}

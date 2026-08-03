package store

import (
	"context"
	"database/sql"
	"fmt"

	// The pgx stdlib driver registers itself as "pgx" for database/sql, which is
	// the interface goose migrates through.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	reportingmigrations "github.com/b42labs/tally/migrations/reporting"
)

// Migrate applies every pending migration of the embedded reporting chain and
// returns the versions it applied, oldest first. An up-to-date database yields
// an empty slice and no error.
//
// Only the admin CLI and the integration tests call this. The API server runs
// no DDL: a schema change is an operator's decision, not a side effect of a
// pod restart.
func Migrate(ctx context.Context, dbURL string) ([]int64, error) {
	return runMigrations(ctx, dbURL, func(provider *goose.Provider) ([]*goose.MigrationResult, error) {
		return provider.Up(ctx)
	})
}

// MigrateDownTo rolls the chain back to version, applying the down migration of
// everything above it, and returns the versions it rolled back. Passing 0 leaves
// an empty schema.
//
// It is the counterpart of Migrate: the chain ships down migrations, and a
// rollback nobody can run is a rollback that does not exist.
func MigrateDownTo(ctx context.Context, dbURL string, version int64) ([]int64, error) {
	return runMigrations(ctx, dbURL, func(provider *goose.Provider) ([]*goose.MigrationResult, error) {
		return provider.DownTo(ctx, version)
	})
}

// MigrationState is one migration of the chain and whether the database carries
// it. It is what answers "which schema is this database on" before code that
// assumes a newer one is deployed against it.
type MigrationState struct {
	// Version is the migration's version, the number its file name starts with.
	Version int64
	// Applied says whether the database has this migration.
	Applied bool
}

// MigrationStatus reports every migration of the embedded chain against the
// database, oldest first.
func MigrationStatus(ctx context.Context, dbURL string) ([]MigrationState, error) {
	db, err := openForMigration(dbURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	provider, err := newMigrationProvider(db)
	if err != nil {
		return nil, err
	}

	status, err := provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the migration status: %w", err)
	}

	states := make([]MigrationState, 0, len(status))
	for _, entry := range status {
		states = append(states, MigrationState{
			Version: entry.Source.Version,
			Applied: entry.State == goose.StateApplied,
		})
	}
	return states, nil
}

// runMigrations opens the database, hands the provider to run, and reports the
// versions it touched.
func runMigrations(ctx context.Context, dbURL string, run func(*goose.Provider) ([]*goose.MigrationResult, error)) ([]int64, error) {
	db, err := openForMigration(dbURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	provider, err := newMigrationProvider(db)
	if err != nil {
		return nil, err
	}

	results, err := run(provider)
	if err != nil {
		return nil, fmt.Errorf("applying the migrations: %w", err)
	}

	versions := make([]int64, 0, len(results))
	for _, result := range results {
		versions = append(versions, result.Source.Version)
	}
	return versions, nil
}

// openForMigration opens the migration connection. The pool is capped at one
// connection because the advisory lock below is session-level: it is held on
// the connection that took it, so every statement of the run has to arrive on
// that same connection.
func openForMigration(dbURL string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// newMigrationProvider builds the provider over the embedded chain. The session
// lock serializes concurrent runs: without it two callers both read version 0,
// both start applying the same migration, and the loser fails with a duplicate
// relation that reads like a broken chain rather than a lost race.
func newMigrationProvider(db *sql.DB) (*goose.Provider, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("preparing the migration lock: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, reportingmigrations.FS,
		goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("preparing the migration provider: %w", err)
	}
	return provider, nil
}

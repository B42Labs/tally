// Package dbmigrate runs an embedded goose migration chain against a PostgreSQL
// database. Every service ships its own chain and its own connection string;
// what they share is the machinery around goose — the single-connection pool
// the session lock needs, the advisory lock that serializes concurrent runs,
// and the error wrapping an operator reads. That machinery lives here so a fix
// to it reaches every service.
//
// The chain is a plain fs.FS, which is what the embedding package exposes, so a
// service's store package is a name for its chain and the table that chain books
// its applied versions in, and nothing more.
package dbmigrate

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"

	// The pgx stdlib driver registers itself as "pgx" for database/sql, which is
	// the interface goose migrates through.
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// MigrationState is one migration of a chain and whether the database carries
// it. It is what answers "which schema is this database on" before code that
// assumes a newer one is run against it.
type MigrationState struct {
	// Version is the migration's version, the number its file name starts with.
	Version int64
	// Applied says whether the database has this migration.
	Applied bool
}

// Up applies every pending migration of chain and returns the versions it
// applied, oldest first. An up-to-date database yields an empty slice and no
// error.
func Up(ctx context.Context, dbURL string, chain fs.FS, versionTable string) ([]int64, error) {
	return run(ctx, dbURL, chain, versionTable, func(provider *goose.Provider) ([]*goose.MigrationResult, error) {
		return provider.Up(ctx)
	})
}

// DownTo rolls chain back to version, applying the down migration of everything
// above it, and returns the versions it rolled back. Passing 0 leaves an empty
// schema.
//
// It is the counterpart of Up: a chain ships down migrations, and a rollback
// nobody can run is a rollback that does not exist.
func DownTo(ctx context.Context, dbURL string, chain fs.FS, versionTable string, version int64) ([]int64, error) {
	return run(ctx, dbURL, chain, versionTable, func(provider *goose.Provider) ([]*goose.MigrationResult, error) {
		return provider.DownTo(ctx, version)
	})
}

// Status reports every migration of chain against the database, oldest first.
func Status(ctx context.Context, dbURL string, chain fs.FS, versionTable string) ([]MigrationState, error) {
	db, err := open(dbURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	provider, err := newProvider(db, chain, versionTable)
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

// run opens the database, hands the provider to apply, and reports the versions
// it touched.
func run(ctx context.Context, dbURL string, chain fs.FS, versionTable string, apply func(*goose.Provider) ([]*goose.MigrationResult, error)) ([]int64, error) {
	db, err := open(dbURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	provider, err := newProvider(db, chain, versionTable)
	if err != nil {
		return nil, err
	}

	results, err := apply(provider)
	if err != nil {
		return nil, fmt.Errorf("applying the migrations: %w", err)
	}

	versions := make([]int64, 0, len(results))
	for _, result := range results {
		versions = append(versions, result.Source.Version)
	}
	return versions, nil
}

// open opens the migration connection. The pool is capped at one connection
// because the advisory lock below is session-level: it is held on the
// connection that took it, so every statement of the run has to arrive on that
// same connection.
func open(dbURL string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return nil, fmt.Errorf("opening the database: %w", err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// newProvider builds the provider over chain. The session lock serializes
// concurrent runs: without it two callers both read version 0, both start
// applying the same migration, and the loser fails with a duplicate relation
// that reads like a broken chain rather than a lost race.
//
// versionTable is the chain's own, because the version recorded in one is a
// position in that chain and nothing else. Sharing goose's default name across
// services makes a connection string that reaches the wrong database read a
// number that belongs to another chain, which is a number that answers no
// question anyone asked.
func newProvider(db *sql.DB, chain fs.FS, versionTable string) (*goose.Provider, error) {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("preparing the migration lock: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, chain,
		goose.WithSessionLocker(locker), goose.WithTableName(versionTable))
	if err != nil {
		return nil, fmt.Errorf("preparing the migration provider: %w", err)
	}
	return provider, nil
}

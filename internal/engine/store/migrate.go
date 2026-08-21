package store

import (
	"context"

	"github.com/b42labs/tally/internal/core/dbmigrate"
	enginemigrations "github.com/b42labs/tally/migrations/engine"
)

// versionTable is where goose records which migrations of the engine chain a
// database carries. The name is the engine's rather than goose's default,
// because the version in that table is a position in one chain: sharing the
// default with the reporting chain lets an engine binary pointed at the
// reporting database read that chain's version, find it higher than its own,
// and pass a schema gate on a database that holds none of its tables.
const versionTable = "goose_db_version_engine"

// MigrationState is one migration of the chain and whether the database carries
// it. It is what answers "which schema is this database on" before code that
// assumes a newer one is run against it.
type MigrationState = dbmigrate.MigrationState

// Migrate applies every pending migration of the embedded engine chain and
// returns the versions it applied, oldest first. An up-to-date database yields
// an empty slice and no error.
//
// Only the engine CLI and the integration tests call this. No other subcommand
// runs DDL as a side effect: a schema change is an operator's decision, not
// something a scheduled run brings along.
func Migrate(ctx context.Context, dbURL string) ([]int64, error) {
	return dbmigrate.Up(ctx, dbURL, enginemigrations.FS, versionTable)
}

// MigrateDownTo rolls the chain back to version, applying the down migration of
// everything above it, and returns the versions it rolled back. Passing 0 leaves
// an empty schema.
//
// It is the counterpart of Migrate: the chain ships down migrations, and a
// rollback nobody can run is a rollback that does not exist.
func MigrateDownTo(ctx context.Context, dbURL string, version int64) ([]int64, error) {
	return dbmigrate.DownTo(ctx, dbURL, enginemigrations.FS, versionTable, version)
}

// MigrationStatus reports every migration of the embedded chain against the
// database, oldest first.
func MigrationStatus(ctx context.Context, dbURL string) ([]MigrationState, error) {
	return dbmigrate.Status(ctx, dbURL, enginemigrations.FS, versionTable)
}

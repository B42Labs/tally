package store

import (
	"context"

	"github.com/b42labs/tally/internal/core/dbmigrate"
	reportingmigrations "github.com/b42labs/tally/migrations/reporting"
)

// versionTable is where goose records which migrations of the reporting chain a
// database carries. It stays on goose's default name: deployed databases book
// there already, and a rename would read as a database the migrator never ran
// against and re-apply a chain it carries.
const versionTable = "goose_db_version"

// MigrationState is one migration of the chain and whether the database carries
// it. It is what answers "which schema is this database on" before code that
// assumes a newer one is deployed against it.
type MigrationState = dbmigrate.MigrationState

// Migrate applies every pending migration of the embedded reporting chain and
// returns the versions it applied, oldest first. An up-to-date database yields
// an empty slice and no error.
//
// Only the admin CLI and the integration tests call this. The API server runs
// no DDL: a schema change is an operator's decision, not a side effect of a
// pod restart.
func Migrate(ctx context.Context, dbURL string) ([]int64, error) {
	return dbmigrate.Up(ctx, dbURL, reportingmigrations.FS, versionTable)
}

// MigrateDownTo rolls the chain back to version, applying the down migration of
// everything above it, and returns the versions it rolled back. Passing 0 leaves
// an empty schema.
//
// It is the counterpart of Migrate: the chain ships down migrations, and a
// rollback nobody can run is a rollback that does not exist.
func MigrateDownTo(ctx context.Context, dbURL string, version int64) ([]int64, error) {
	return dbmigrate.DownTo(ctx, dbURL, reportingmigrations.FS, versionTable, version)
}

// MigrationStatus reports every migration of the embedded chain against the
// database, oldest first.
func MigrationStatus(ctx context.Context, dbURL string) ([]MigrationState, error) {
	return dbmigrate.Status(ctx, dbURL, reportingmigrations.FS, versionTable)
}

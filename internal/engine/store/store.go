// Package store is the metering engine's database seam: the pgx connection pool
// and the migrator that applies the embedded engine chain.
//
// Opening a Store does not dial the database. The connection is established on
// the first query, so a subcommand that only parses its flags fails on the flags
// rather than on the database. The engine never runs DDL outside the migrate
// subcommands of its CLI: a schema change is an operator's decision.
//
// The static queries in queries.sql are compiled by sqlc into the sqlcgen
// subpackage, which takes the pool as its DBTX. `make generate` rewrites that
// package, and it is committed so that a plain `go build` needs no generator.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.1.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	enginemigrations "github.com/b42labs/tally/migrations/engine"
)

// undefinedTable is the SQLSTATE Postgres reports for a statement naming a
// relation that does not exist.
const undefinedTable = "42P01"

// poolMaxConns bounds the pool. The bound is fixed rather than configured: the
// engine is a short-lived CLI process rather than a serving fleet whose replica
// count the database budgets connections for.
const poolMaxConns = 10

// Store owns the connection pool the engine reads and writes through.
type Store struct {
	pool *pgxpool.Pool
}

// New parses dbURL and prepares the pool for it. Connections are established
// lazily, so a database that is down does not keep the process from starting.
func New(ctx context.Context, dbURL string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parsing the database url: %w", err)
	}
	cfg.MaxConns = poolMaxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening the database pool: %w", err)
	}
	return &Store{pool: pool}, nil
}

// CheckSchema refuses a database that carries less of the migration chain than
// this build was compiled against. It is what every subcommand runs before its
// first query, so that a deployment whose migrate step lagged behind the image
// fails on the cause rather than on whichever column the missing migration was
// going to add, halfway through a run.
//
// A database without this chain's bookkeeping is a database this chain never
// ran against, which is version 0 and reported as such. The table carries the
// engine's own name, so a connection string that reaches another service's
// database — the reporting one, a copy-paste away in a deployment that carries
// both — reads 0 here rather than that chain's position, which is a number the
// comparison below has no meaning against. Every other failed read is reported
// as it came back: a read that did not answer is not a pass, and a gate that
// lets one through is a gate that disables itself on the deployment it exists
// for. The relation is schema-qualified for the reason migration 0001 pins the
// trigger function's search_path.
func (s *Store) CheckSchema(ctx context.Context) error {
	var version int64
	err := s.pool.QueryRow(ctx,
		`SELECT coalesce(max(version_id), 0) FROM public.`+versionTable+` WHERE is_applied`,
	).Scan(&version)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == undefinedTable {
		version, err = 0, nil
	}
	if err != nil {
		return fmt.Errorf("reading the schema version: %w", err)
	}
	if version < enginemigrations.Version {
		return fmt.Errorf("the database is on schema version %d, this build needs %d: run tally-engine migrate",
			version, enginemigrations.Version)
	}
	return nil
}

// Close releases every connection the pool holds and waits for in-flight ones
// to be returned.
func (s *Store) Close() {
	s.pool.Close()
}

// Pool returns the underlying pool, which is what the generated sqlc queries
// take as their DBTX.
func (s *Store) Pool() *pgxpool.Pool {
	return s.pool
}

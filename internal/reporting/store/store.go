// Package store is the Reporting API's database seam: the pgx connection pool,
// the transaction helper every multi-statement write goes through, and the
// migrator that applies the embedded goose chain.
//
// Opening a Store does not dial the database. The API server starts while
// TimescaleDB is still unavailable and reports that state through readiness,
// so a connection failure is a per-request error rather than a startup error.
// The server also never runs DDL: Migrate belongs to the admin CLI and to the
// integration tests.
//
// The static queries in queries.sql are compiled by sqlc into the sqlcgen
// subpackage, which takes either the pool or a transaction as its DBTX.
// Statements that are built per request stay hand-written against pgx.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.3.
package store

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	reportingmigrations "github.com/b42labs/tally/migrations/reporting"
)

// Store owns the connection pool the Reporting API shares across requests.
type Store struct {
	pool *pgxpool.Pool
	// schemaChecked records whether the database has already answered with a
	// schema this build can serve. It decides nothing but what a later failed
	// read means: once there has been an answer, a read that fails says nothing
	// about the schema, and before it there is nothing to fall back on.
	schemaChecked atomic.Bool
}

// New parses dbURL and prepares a pool of at most maxConns connections for it.
// Connections are established lazily, so a database that is down does not keep
// the process from starting.
//
// The bound is passed in rather than left to pgxpool, which derives it from the
// node's CPU count: that number says nothing about how many connections the
// database has budgeted for this service's replicas.
func New(ctx context.Context, dbURL string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parsing the database url: %w", err)
	}
	cfg.MaxConns = maxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening the database pool: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Ping acquires a connection and checks that the database answers. It is what
// the liveness probe reports on: a process whose database is unreachable is
// still a process worth keeping.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging the database: %w", err)
	}
	return nil
}

// Ready reports whether the database answers and carries the schema this binary
// was built against. It is what the readiness probe reports on.
//
// The schema half matters because a pool connects to an empty database just as
// happily as to a migrated one: without this check a fresh deployment reaches
// Ready before anyone ran the migrations, and every handler that touches a
// table then fails on a pod Kubernetes has declared healthy.
//
// A rollback puts a serving pod in exactly that state, so the comparison stays
// on every probe. What the first answer buys is narrower: after it, a read that
// fails is forgiven, because a hiccup on goose's bookkeeping table — a lock, a
// revoked grant — says nothing about the schema and must not take a serving pod
// out of rotation. A read that succeeds always decides.
func (s *Store) Ready(ctx context.Context) error {
	if err := s.Ping(ctx); err != nil {
		return err
	}

	var version int64
	if err := s.pool.QueryRow(ctx,
		`SELECT coalesce(max(version_id), 0) FROM goose_db_version WHERE is_applied`,
	).Scan(&version); err != nil {
		if s.schemaChecked.Load() {
			return nil
		}
		return fmt.Errorf("reading the schema version: %w", err)
	}
	if version < reportingmigrations.Version {
		s.schemaChecked.Store(false)
		return fmt.Errorf("the database is on schema version %d, this build needs %d",
			version, reportingmigrations.Version)
	}
	s.schemaChecked.Store(true)
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

// WithTx runs fn in a transaction: it commits when fn returns nil and rolls
// back otherwise. The callback's error is returned unchanged so that callers
// can match their own sentinels with errors.Is, and a panic is re-raised after
// the rollback so a bug never leaves a transaction open.
func (s *Store) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// Rolling back a committed transaction is a no-op, which lets one deferred
	// call cover the error return and the panic alike.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

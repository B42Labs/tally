// Package source is the metering engine's read seam over the Reporting
// database: a connection pool and, per run, one read-only REPEATABLE READ
// transaction that every read of that run goes through (D1, D3). The engine
// reaches the reporting data over its own connection rather than over the API,
// through the tally_engine_reader role, and this package writes nothing, here
// or anywhere else.
//
// Every reporting read of the engine is a method on Snapshot. The transaction
// itself stays unexported, so a later package that needs another query, such as
// the event count a counter measures, adds a method here rather than taking a
// handle on the transaction and running its own statements outside the
// snapshot.
//
// A snapshot holds one connection for its lifetime and pins the reporting
// database's vacuum horizon while it is open, which keeps dead tuples from
// being reclaimed. A caller closes it as soon as metering has read what it
// needs rather than keeping it open across the rating and writing that follow,
// and the timeouts New sets keep a read that hangs from holding the horizon
// past them.
//
// The static queries in queries.sql are compiled by sqlc into the sqlcgen
// subpackage. `make generate` rewrites that package, and it is committed so
// that a plain `go build` needs no generator.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.2.
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/engine/source/sqlcgen"
)

// poolMaxConns bounds the pool. The bound is fixed rather than configured: the
// engine is a short-lived CLI process rather than a serving fleet whose replica
// count the database budgets connections for.
const poolMaxConns = 10

// What a connection of this pool may hold open. A run reads every candidate
// through one transaction, so a read that has stopped making progress keeps the
// reporting database's vacuum horizon at the run's xmin and its AccessShare
// locks on the chunks it read, which is what a compression policy waits behind.
// Without a bound the run does not fail, it hangs, and holds both for as long
// as it does. A read that has not returned in five minutes is one that will not
// return: every query here is indexed and reads one resource or one list.
const (
	statementTimeout         = 5 * time.Minute
	idleInTransactionTimeout = 10 * time.Minute
)

// DB owns the connection pool the engine reads the reporting database through.
type DB struct {
	pool *pgxpool.Pool
}

// New parses dbURL and prepares the pool for it. Connections are established
// lazily, so a database that is down does not keep the process from starting.
func New(ctx context.Context, dbURL string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parsing the reporting database url: %w", err)
	}
	cfg.MaxConns = poolMaxConns
	// The server ends what runs past the bounds, whatever the client is doing:
	// a canceled query releases the snapshot's grip on the vacuum horizon, a
	// context the engine never cancels does not.
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = milliseconds(statementTimeout)
	cfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = milliseconds(idleInTransactionTimeout)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening the reporting database pool: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close releases every connection the pool holds and waits for in-flight ones
// to be returned.
func (db *DB) Close() {
	db.pool.Close()
}

// Snapshot opens the transaction a run reads everything through. It is
// REPEATABLE READ, so every query in it sees the same version of the data
// however long the run takes, and read-only as defense in depth under the
// grants the reader role holds.
//
// Reading the snapshot time is the transaction's first statement, which is
// where REPEATABLE READ takes the MVCC snapshot the later queries see. The
// value it returns is that snapshot's time, which the run records as
// runs.stats.snapshot_at.
func (db *DB) Snapshot(ctx context.Context) (*Snapshot, error) {
	tx, err := db.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("opening the reporting snapshot: %w", err)
	}

	queries := sqlcgen.New(tx)
	at, err := queries.SnapshotTime(ctx)
	if err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("reading the snapshot time: %w", err)
	}
	return &Snapshot{At: at.Time.UTC(), tx: tx, queries: queries}, nil
}

// Snapshot is one run's consistent view of the reporting database.
type Snapshot struct {
	// At is when the snapshot was taken, in UTC. It is what the run records as
	// runs.stats.snapshot_at: every row this snapshot returns is the data as of
	// this instant.
	At time.Time

	tx      pgx.Tx
	queries *sqlcgen.Queries
}

// Close ends the transaction and returns its connection to the pool. A
// snapshot that is already closed is not an error, so a deferred Close next to
// an explicit one both report success.
func (s *Snapshot) Close(ctx context.Context) error {
	if err := s.tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return fmt.Errorf("closing the reporting snapshot: %w", err)
	}
	return nil
}

// Resource identifies one candidate of a billing period.
type Resource struct {
	Cloud        string
	Platform     string
	ResourceType string
	ResourceID   string
}

// Candidates lists the resources the period can bill, ordered by cloud,
// resource type, and resource id. The projection is the index: its rows are
// never deleted, so a resource that lived during the period is still there to
// be found.
//
// An empty clouds list meters every cloud the projection knows. The engine
// keeps no cloud list of its own, so "no filter" is the only meaning the empty
// list can carry.
func (s *Snapshot) Candidates(ctx context.Context, clouds []string, periodFrom, periodTo time.Time) ([]Resource, error) {
	// The nil is what carries that meaning into the query: it encodes as SQL
	// NULL, which the predicate short-circuits on, while an empty non-nil slice
	// encodes as '{}' and matches no cloud at all.
	if len(clouds) == 0 {
		clouds = nil
	}
	rows, err := s.queries.ListCandidates(ctx, sqlcgen.ListCandidatesParams{
		Clouds:     clouds,
		PeriodFrom: timestamptz(periodFrom),
		PeriodTo:   timestamptz(periodTo),
	})
	if err != nil {
		return nil, fmt.Errorf("listing the candidate resources: %w", err)
	}

	resources := make([]Resource, 0, len(rows))
	for _, row := range rows {
		resources = append(resources, Resource{
			Cloud:        row.Cloud,
			Platform:     row.Platform,
			ResourceType: row.ResourceType,
			ResourceID:   row.ResourceID,
		})
	}
	return resources, nil
}

// History loads one resource's events up to the period end, ordered by
// timestamp, received_at, and event id. The history starts at the first event
// there is, because the state the resource holds at the period start is
// whatever the events before it left behind.
func (s *Snapshot) History(ctx context.Context, r Resource, periodTo time.Time) ([]event.Stored, error) {
	rows, err := s.queries.ListHistory(ctx, sqlcgen.ListHistoryParams{
		Cloud:        r.Cloud,
		ResourceType: r.ResourceType,
		ResourceID:   r.ResourceID,
		PeriodTo:     timestamptz(periodTo),
	})
	if err != nil {
		return nil, fmt.Errorf("loading the history of %s/%s/%s: %w",
			r.Cloud, r.ResourceType, r.ResourceID, err)
	}

	events := make([]event.Stored, 0, len(rows))
	for _, row := range rows {
		stored, err := decode(row)
		if err != nil {
			return nil, err
		}
		events = append(events, stored)
	}
	return events, nil
}

// Project is one entry of the project registry.
type Project struct {
	ID         uuid.UUID
	Platform   string
	Cloud      string
	ExternalID string
}

// Projects reads the project registry whole, ordered by cloud and external id.
// It is one of the two project graph loaders of WP 3.2; their consumer is
// attribution, which walks the graph over these UUIDs while the resources it
// walks from carry the external id rather than the registry's.
func (s *Snapshot) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.queries.ListProjects(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing the projects: %w", err)
	}

	projects := make([]Project, 0, len(rows))
	for _, row := range rows {
		projects = append(projects, Project{
			ID:         row.ID,
			Platform:   row.Platform,
			Cloud:      row.Cloud,
			ExternalID: row.ExternalID,
		})
	}
	return projects, nil
}

// Relation is one edge of the project graph. ValidTo is nil while the relation
// is open.
type Relation struct {
	ID           uuid.UUID
	SourceID     uuid.UUID
	TargetID     uuid.UUID
	RelationType string
	ValidFrom    time.Time
	ValidTo      *time.Time
}

// Relations lists the relations of the given types whose validity overlaps the
// period (D4), ordered by id. It is the second project graph loader of WP 3.2,
// read by attribution.
//
// An empty type list is attribution turned off, which is what an explicitly
// empty TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES configures. No query runs then,
// because every relation type is one attribution would not walk.
func (s *Snapshot) Relations(ctx context.Context, relationTypes []string, periodFrom, periodTo time.Time) ([]Relation, error) {
	if len(relationTypes) == 0 {
		return []Relation{}, nil
	}
	rows, err := s.queries.ListRelationsOverlapping(ctx, sqlcgen.ListRelationsOverlappingParams{
		RelationTypes: relationTypes,
		PeriodFrom:    timestamptz(periodFrom),
		PeriodTo:      timestamptz(periodTo),
	})
	if err != nil {
		return nil, fmt.Errorf("listing the project relations: %w", err)
	}

	relations := make([]Relation, 0, len(rows))
	for _, row := range rows {
		relation := Relation{
			ID:           row.ID,
			SourceID:     row.SourceID,
			TargetID:     row.TargetID,
			RelationType: row.RelationType,
			ValidFrom:    row.ValidFrom.Time.UTC(),
		}
		if row.ValidTo.Valid {
			validTo := row.ValidTo.Time.UTC()
			relation.ValidTo = &validTo
		}
		relations = append(relations, relation)
	}
	return relations, nil
}

// decode turns one events row into the stored event the metering fold works on,
// the way the projection reads the same row. A row whose payload column is NULL
// carries the empty envelope, which reports neither a state nor a size.
// Timestamps come out in UTC, the zone every instant the engine works with is
// stated in.
func decode(row sqlcgen.Event) (event.Stored, error) {
	stored := event.Stored{
		Event: event.Event{
			EventID:      row.EventID,
			Timestamp:    row.Timestamp.Time.UTC(),
			EventType:    row.EventType,
			Platform:     row.Platform,
			Cloud:        row.Cloud,
			ResourceType: row.ResourceType,
			ResourceID:   row.ResourceID,
			ProjectID:    row.ProjectID,
			Source:       event.Source(row.Source),
		},
		ReceivedAt: row.ReceivedAt.Time.UTC(),
	}
	if len(row.Payload) > 0 {
		if err := json.Unmarshal(row.Payload, &stored.Payload); err != nil {
			return event.Stored{}, fmt.Errorf("decoding the payload of event %s: %w", row.EventID, err)
		}
	}
	return stored, nil
}

// milliseconds is a timeout in the unit the server settings take.
func milliseconds(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

// timestamptz maps an instant to the query parameter.
func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

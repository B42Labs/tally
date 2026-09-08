// Package store is the demo console's read-only seam over the engine database.
// The console renders billing periods, runs, statements, pricing versions and
// correction deltas; the engine exposes no read API for them, so the console
// opens that database directly. Nothing in this package writes: queries.sql
// holds SELECTs alone, and every method here reads.
//
// It is also the seam a read API of the engine would be swapped in at. The
// pages call these methods and see plain Go values rather than pgx types, so
// replacing the queries with HTTP calls is a change to this package alone.
//
// Every method wraps a failed query as "<QueryName>: %w" with the name the
// query carries in queries.sql, so an error page can name the read that failed
// while errors.Is(err, pgx.ErrNoRows) still holds for a lookup that found
// nothing.
//
// A stored amount reaches a page as a decimal. amountOf refuses a NULL and a
// NaN numeric, which is not an amount anything can be shown for: a list drops
// such a row and logs it, a single read reports it.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/store/sqlcgen"
)

// poolMaxConns bounds the pool. A demo console serves one viewer clicking
// through pages, so a handful of connections covers every page it renders at
// once, and the engine database keeps the rest for the runs that write it.
const poolMaxConns = 4

// Store owns the connection pool the console reads the engine database
// through.
type Store struct {
	pool   *pgxpool.Pool
	q      *sqlcgen.Queries
	logger *slog.Logger
}

// New parses dbURL and prepares the pool for it. Connections are established
// lazily, the way the engine's own store opens: a database that is down does
// not keep the console from starting, it fails the pages that read it.
func New(ctx context.Context, dbURL string, logger *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parsing the database url: %w", err)
	}
	cfg.MaxConns = poolMaxConns

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening the database pool: %w", err)
	}
	return &Store{pool: pool, q: sqlcgen.New(pool), logger: logger}, nil
}

// Close releases every connection the pool holds and waits for in-flight ones
// to be returned.
func (s *Store) Close() {
	s.pool.Close()
}

// Period is one billing month. FinalizedRunID and FinalizedAt are zero for a
// period that is not finalized, which is the pairing the schema admits.
type Period struct {
	From           time.Time
	To             time.Time
	Status         string
	FinalizedRunID uuid.UUID
	FinalizedAt    time.Time
}

// Run is one metering run. CorrectsRunID is zero for a regular run,
// PricingVersion empty for a run of a period no model priced, and CompletedAt
// zero for a run no end was written for.
type Run struct {
	ID             uuid.UUID
	PeriodFrom     time.Time
	PeriodTo       time.Time
	Kind           string
	CorrectsRunID  uuid.UUID
	PricingVersion string
	Status         string
	Clouds         []string
	Stats          []byte
	StartedAt      time.Time
	CompletedAt    time.Time
}

// StatementRow is one line of a run's statement table: the project key and what
// the run billed it.
type StatementRow struct {
	Key      string
	Total    decimal.Decimal
	Currency string
}

// Statement is the stored document of one project statement with the total it
// sums to.
type Statement struct {
	Document []byte
	Total    decimal.Decimal
	Currency string
}

// ProjectStatementRow is one month of a project's history: the run that billed
// it and what that run charged.
type ProjectStatementRow struct {
	RunID      uuid.UUID
	PeriodFrom time.Time
	Kind       string
	Status     string
	Total      decimal.Decimal
	Currency   string
}

// PricingModel is one imported catalog version without its document.
type PricingModel struct {
	Version    string
	ValidFrom  time.Time
	Currency   string
	ImportedAt time.Time
}

// PricingDocument is a catalog version with the document it was imported from.
type PricingDocument struct {
	PricingModel
	Document []byte
}

// Segment is one metered interval of a resource at one rated dimension. A
// resource metered over three intervals and rated on two dimensions each has
// six of them.
type Segment struct {
	State     string
	From      time.Time
	To        time.Time
	Seconds   int64
	Usage     []byte
	Dimension string
	Amount    decimal.Decimal
	Currency  string
}

// Delta is one row a correction run moved: the key it belongs to and the two
// amounts it moved between.
type Delta struct {
	Cloud        string
	Platform     string
	ResourceType string
	ResourceID   string
	ProjectID    string
	Dimension    string
	OldAmount    decimal.Decimal
	NewAmount    decimal.Decimal
	Delta        decimal.Decimal
	Currency     string
}

// ListPeriods returns every billing period, newest month first.
func (s *Store) ListPeriods(ctx context.Context) ([]Period, error) {
	rows, err := s.q.ListPeriods(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListPeriods: %w", err)
	}

	periods := make([]Period, 0, len(rows))
	for _, row := range rows {
		periods = append(periods, Period{
			From:           timeOf(row.PeriodFrom),
			To:             timeOf(row.PeriodTo),
			Status:         row.Status,
			FinalizedRunID: uuidOf(row.FinalizedRunID),
			FinalizedAt:    timeOf(row.FinalizedAt),
		})
	}
	return periods, nil
}

// ListRuns returns the newest runs by start, at most limit of them.
func (s *Store) ListRuns(ctx context.Context, limit int32) ([]Run, error) {
	rows, err := s.q.ListRuns(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("ListRuns: %w", err)
	}

	list := make([]Run, 0, len(rows))
	for _, row := range rows {
		list = append(list, runOf(row))
	}
	return list, nil
}

// GetRun returns one run. A run that does not exist comes back as
// pgx.ErrNoRows, which is what the page turns into its not-found answer.
func (s *Store) GetRun(ctx context.Context, id uuid.UUID) (Run, error) {
	row, err := s.q.GetRun(ctx, pgUUID(id))
	if err != nil {
		return Run{}, fmt.Errorf("GetRun: %w", err)
	}
	return runOf(row), nil
}

// ListStatements returns the statements of a run, largest total first.
func (s *Store) ListStatements(ctx context.Context, runID uuid.UUID) ([]StatementRow, error) {
	rows, err := s.q.ListStatements(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("ListStatements: %w", err)
	}

	list := make([]StatementRow, 0, len(rows))
	for _, row := range rows {
		total, ok := amountOf(row.Total)
		if !ok {
			s.skipped("ListStatements", "key", row.ProjectID)
			continue
		}
		list = append(list, StatementRow{Key: row.ProjectID, Total: total, Currency: row.Currency})
	}
	return list, nil
}

// GetStatement returns the stored document of one project statement. A pair
// that was never billed comes back as pgx.ErrNoRows. A total that is not a
// number is an error rather than a skipped row: the page exists to show that
// one statement, and there is nothing left of it to render.
func (s *Store) GetStatement(ctx context.Context, runID uuid.UUID, key string) (Statement, error) {
	row, err := s.q.GetStatement(ctx, sqlcgen.GetStatementParams{RunID: pgUUID(runID), ProjectID: key})
	if err != nil {
		return Statement{}, fmt.Errorf("GetStatement: %w", err)
	}
	total, ok := amountOf(row.Total)
	if !ok {
		return Statement{}, fmt.Errorf("GetStatement: the total of %s in run %s is not a number", key, runID)
	}
	return Statement{Document: row.Document, Total: total, Currency: row.Currency}, nil
}

// ListStatementsForProject returns what every run billed one project, oldest
// month first.
func (s *Store) ListStatementsForProject(ctx context.Context, key string) ([]ProjectStatementRow, error) {
	rows, err := s.q.ListStatementsForProject(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("ListStatementsForProject: %w", err)
	}

	list := make([]ProjectStatementRow, 0, len(rows))
	for _, row := range rows {
		total, ok := amountOf(row.Total)
		if !ok {
			s.skipped("ListStatementsForProject", "key", key, "run", uuidOf(row.RunID))
			continue
		}
		list = append(list, ProjectStatementRow{
			RunID:      uuidOf(row.RunID),
			PeriodFrom: timeOf(row.PeriodFrom),
			Kind:       row.Kind,
			Status:     row.Status,
			Total:      total,
			Currency:   row.Currency,
		})
	}
	return list, nil
}

// ListPricingModels returns the imported catalog versions, the one in force
// last first.
func (s *Store) ListPricingModels(ctx context.Context) ([]PricingModel, error) {
	rows, err := s.q.ListPricingModels(ctx)
	if err != nil {
		return nil, fmt.Errorf("ListPricingModels: %w", err)
	}

	list := make([]PricingModel, 0, len(rows))
	for _, row := range rows {
		list = append(list, PricingModel{
			Version:    row.Version,
			ValidFrom:  timeOf(row.ValidFrom),
			Currency:   row.Currency,
			ImportedAt: timeOf(row.ImportedAt),
		})
	}
	return list, nil
}

// GetPricingModel returns one catalog version with its document. A version that
// was never imported comes back as pgx.ErrNoRows.
func (s *Store) GetPricingModel(ctx context.Context, version string) (PricingDocument, error) {
	row, err := s.q.GetPricingModel(ctx, version)
	if err != nil {
		return PricingDocument{}, fmt.Errorf("GetPricingModel: %w", err)
	}
	return PricingDocument{
		PricingModel: PricingModel{
			Version:    row.Version,
			ValidFrom:  timeOf(row.ValidFrom),
			Currency:   row.Currency,
			ImportedAt: timeOf(row.ImportedAt),
		},
		Document: row.Document,
	}, nil
}

// LatestRunWithResource returns the newest run that metered a resource, and
// reports whether any run did. A resource no run ever metered is a page that
// has nothing to show rather than a failed read, so it comes back as false and
// no error.
func (s *Store) LatestRunWithResource(ctx context.Context, cloud, resourceType, resourceID string) (uuid.UUID, bool, error) {
	id, err := s.q.LatestRunWithResource(ctx, sqlcgen.LatestRunWithResourceParams{
		Cloud:        cloud,
		ResourceType: resourceType,
		ResourceID:   resourceID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("LatestRunWithResource: %w", err)
	}
	return uuidOf(id), true, nil
}

// ListResourceSegments returns what one run metered and rated for one resource,
// ordered by interval and then dimension.
func (s *Store) ListResourceSegments(ctx context.Context, runID uuid.UUID, cloud, resourceType, resourceID string) ([]Segment, error) {
	rows, err := s.q.ListResourceSegments(ctx, sqlcgen.ListResourceSegmentsParams{
		RunID:        pgUUID(runID),
		Cloud:        cloud,
		ResourceType: resourceType,
		ResourceID:   resourceID,
	})
	if err != nil {
		return nil, fmt.Errorf("ListResourceSegments: %w", err)
	}

	list := make([]Segment, 0, len(rows))
	for _, row := range rows {
		amount, ok := amountOf(row.Amount)
		if !ok {
			s.skipped("ListResourceSegments", "dimension", row.Dimension, "from", timeOf(row.FromTs))
			continue
		}
		list = append(list, Segment{
			State:     row.State,
			From:      timeOf(row.FromTs),
			To:        timeOf(row.ToTs),
			Seconds:   row.Seconds,
			Usage:     row.Usage,
			Dimension: row.Dimension,
			Amount:    amount,
			Currency:  row.Currency,
		})
	}
	return list, nil
}

// ListCorrectionDeltas returns the rows a correction run moved. A run that
// corrected nothing, and every regular run, has none.
func (s *Store) ListCorrectionDeltas(ctx context.Context, runID uuid.UUID) ([]Delta, error) {
	rows, err := s.q.ListCorrectionDeltas(ctx, pgUUID(runID))
	if err != nil {
		return nil, fmt.Errorf("ListCorrectionDeltas: %w", err)
	}

	list := make([]Delta, 0, len(rows))
	for _, row := range rows {
		old, oldOK := amountOf(row.OldAmount)
		current, currentOK := amountOf(row.NewAmount)
		difference, differenceOK := amountOf(row.Delta)
		if !oldOK || !currentOK || !differenceOK {
			s.skipped("ListCorrectionDeltas", "resource_id", row.ResourceID, "dimension", row.Dimension)
			continue
		}
		list = append(list, Delta{
			Cloud:        row.Cloud,
			Platform:     row.Platform,
			ResourceType: row.ResourceType,
			ResourceID:   row.ResourceID,
			ProjectID:    row.ProjectID,
			Dimension:    row.Dimension,
			OldAmount:    old,
			NewAmount:    current,
			Delta:        difference,
			Currency:     row.Currency,
		})
	}
	return list, nil
}

// skipped reports a row a listing dropped. The query and the row's key columns
// are what an operator needs to find the row behind a page that is one line
// short.
func (s *Store) skipped(query string, keys ...any) {
	s.logger.Warn("skipping a row whose amount is not a number",
		append([]any{"query", query}, keys...)...)
}

// runOf maps a run row to the value a page renders. The three nullable columns
// each read as the absence they stand for: a regular run corrects nothing, a
// run of a period no model priced carries no version, and a run that did not
// get to the end of its pass has no completion.
func runOf(row sqlcgen.Run) Run {
	return Run{
		ID:             uuidOf(row.ID),
		PeriodFrom:     timeOf(row.PeriodFrom),
		PeriodTo:       timeOf(row.PeriodTo),
		Kind:           row.Kind,
		CorrectsRunID:  uuidOf(row.CorrectsRunID),
		PricingVersion: textOf(row.PricingVersion),
		Status:         row.Status,
		Clouds:         row.Clouds,
		Stats:          row.Stats,
		StartedAt:      timeOf(row.StartedAt),
		CompletedAt:    timeOf(row.CompletedAt),
	}
}

// amountOf maps a stored numeric to a decimal, and reports whether it is one. A
// NULL and a NaN are refused where they are read, the way amountOf in
// internal/engine/export/export.go refuses them: neither is an amount anything
// can be invoiced for.
func amountOf(n pgtype.Numeric) (decimal.Decimal, bool) {
	if !n.Valid || n.NaN {
		return decimal.Decimal{}, false
	}
	return decimal.NewFromBigInt(n.Int, n.Exp), true
}

// uuidOf maps a stored id to the value the pages pass around. A NULL column
// reads as the nil id, which is how a regular run's empty corrects_run_id
// reaches a template.
func uuidOf(id pgtype.UUID) uuid.UUID {
	if !id.Valid {
		return uuid.Nil
	}
	return uuid.UUID(id.Bytes)
}

// pgUUID maps an id to the parameter the generated queries take.
func pgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// timeOf maps a stored timestamp to UTC. A NULL column reads as the zero time:
// pgx hands back the session's location, so a value a page compares or formats
// is normalized here rather than at every call.
func timeOf(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time.UTC()
}

// textOf maps a nullable text column to a string, reading a NULL as the empty
// one.
func textOf(text pgtype.Text) string {
	if !text.Valid {
		return ""
	}
	return text.String
}

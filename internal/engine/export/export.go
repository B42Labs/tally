// Package export reads one run back out of the engine database and hands it to
// a BillingExporter, which is the seam every consumer of billing artifacts sits
// behind. Load is the read: one run row, its statements, its rated records, the
// kickbacks it settles for its partners and, for a correction, its deltas and
// the kickback differences to the run it corrects, all out of one snapshot. The
// JSON and the CSV file writers are the first implementations of the interface,
// and an ERP adapter is another one rather than a change to the loader.
//
// The package writes nothing to the database, and only a completed or a
// finalized run is loaded at all: the superseded, failed and running rows a
// period accumulates stay in the database for audit and are excluded from every
// export (roadmap WP 3.8). A finalized run's records are immutable and a
// completed one's are not, because a run of the same period supersedes it, so
// the status Load read under its snapshot is read again by the export
// subcommand before anything is written.
//
// An export is a function of the run it reads. Every ordering is fixed by the
// queries, every timestamp is rendered in UTC, no artifact records when it was
// written, and each file is written to a temporary name and renamed into place,
// so exporting the same finalized run twice yields byte-identical files and a
// second export replaces the first one file by file.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP
// 3.10.
package export

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/corrections"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// The two run statuses an export is produced from. A superseded run's records
// are still in the database, and a failed or running run's records are there
// too, and none of them bills anything: WP 3.8 excludes all three from every
// export and every query.
const (
	statusCompleted = "completed"
	statusFinalized = "finalized"
)

// Run is one run as an exporter receives it: its row, its statements, its rated
// records, the kickbacks it settles, and, for a correction, its deltas. It is
// everything the file writers below need, so an exporter runs with no database
// handle of its own.
type Run struct {
	ID uuid.UUID
	// Kind is runs.KindRegular or runs.KindCorrection.
	Kind string
	// CorrectsRunID is the run a correction corrects, and uuid.Nil for a regular
	// run.
	CorrectsRunID uuid.UUID
	PeriodFrom    time.Time
	PeriodTo      time.Time
	// Status is "completed" or "finalized": Load refuses every other one.
	Status string
	// PricingVersion is the version the run rated with, and the empty string
	// where the row carries NULL.
	PricingVersion string
	// Clouds is what the run was restricted to, empty for a run over every
	// cloud. It is never nil, so an exporter renders a list rather than a null.
	Clouds    []string
	StartedAt time.Time
	// CompletedAt is the zero time where the row carries NULL, which is what a
	// run that never got to the end of its pass reads as.
	CompletedAt time.Time
	// Stats is the run's stats object as it is stored.
	Stats json.RawMessage
	// Statements holds one entry per project the run billed, ordered by Key.
	Statements []statements.Statement
	// Rated holds one entry per rated record, in the order ListRatedRecords
	// returns them: by resource and then by the start of the usage record and
	// its dimension.
	Rated []RatedRecord
	// Deltas holds one entry per correction delta, in the order corrections.Diff
	// sorted them. It is empty for a regular run, which has none.
	Deltas []Delta
	// Kickbacks holds what the run settles for its partners, in the order
	// sortKickbacks defines: beneficiary, currency, statement key, relation,
	// scope, rate, amount, base. A correction's are the differences to the run
	// it corrects, and a run that owes nobody carries none.
	Kickbacks []Kickback
}

// RatedRecord is one rated_records row with the usage record it rates: what was
// billed, for whom, over which interval, and the quantity the amount was
// computed from.
type RatedRecord struct {
	Resource         source.Resource
	ProjectID, State string
	FromTS, ToTS     time.Time
	Dimension        string
	// Quantity is the dimension's quantity as it was stored in the usage
	// object, at the four places every quantity carries.
	Quantity decimal.Decimal
	// Amount is what the dimension was billed at, at two places.
	Amount   decimal.Decimal
	Currency string
}

// Delta is one correction_deltas row: the difference between what the corrected
// run billed and what the correction rated, for one key.
type Delta struct {
	corrections.Delta
	Currency string
}

// ErrRunNotExportable is what Load returns for a run whose status is not
// completed or finalized. Nothing is read past the run row for such a call.
var ErrRunNotExportable = errors.New("the run is not exportable")

// BillingExporter delivers one loaded run to whatever consumes billing
// artifacts. The file writers below are the first implementations; an ERP
// adapter (an SFTP drop, a REST push) implements the same interface and is
// chosen by the caller, so the engine does not change when one is added.
type BillingExporter interface {
	Export(ctx context.Context, run Run) error
}

// Load reads one run and everything an exporter renders from it. Four queries
// run for a regular run (the run row, its statements, its rated records and its
// adjustment records) and six for a correction (its deltas and the adjustment
// records of the run it corrects on top), all in one REPEATABLE READ
// transaction, so what an exporter receives is what belonged to the run at one
// instant rather than at four or six. A correction's kickbacks are diffed
// against the corrected run's inside that same snapshot, so the two sides of a
// difference come from one state of the database. The transaction is read-only
// as defense in depth, and it is ended by the rollback below: an export writes
// nothing there is anything to commit.
//
// A run id no row carries is refused with runs.ErrRunNotFound, and a run that
// is neither completed nor finalized with ErrRunNotExportable. A stored amount
// that is not a number and a usage object that does not decode are refused by
// naming the row that carries them: an export short of a value, or holding a
// zero where a number was meant, is one nobody can tell from a correct one.
func Load(ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID) (Run, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Run{}, fmt.Errorf("reading the run %s: %w", runID, err)
	}
	// The rollback that ends the snapshot, on a context no cancellation reaches:
	// a canceled call gives its connection back rather than leaving it to the
	// connection's teardown. The error is dropped because there is nothing a
	// failed rollback of a read-only transaction changes.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	id := pgtype.UUID{Bytes: runID, Valid: true}
	q := sqlcgen.New(tx)
	row, err := q.GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Run{}, fmt.Errorf("%w: there is no run %s to export", runs.ErrRunNotFound, runID)
		}
		return Run{}, fmt.Errorf("reading the run %s: %w", runID, err)
	}
	if row.Status != statusCompleted && row.Status != statusFinalized {
		return Run{}, fmt.Errorf("%w: run %s is %s, and only a completed or finalized run is exported",
			ErrRunNotExportable, runID, row.Status)
	}

	run := Run{
		ID:             uuid.UUID(row.ID.Bytes),
		Kind:           row.Kind,
		PeriodFrom:     row.PeriodFrom.Time.UTC(),
		PeriodTo:       row.PeriodTo.Time.UTC(),
		Status:         row.Status,
		PricingVersion: row.PricingVersion.String,
		Clouds:         row.Clouds,
		StartedAt:      row.StartedAt.Time.UTC(),
		Stats:          json.RawMessage(row.Stats),
	}
	// The three nullable columns, each read as the absence it stands for: a
	// regular run corrects nothing, a run of a period no model priced carries no
	// version, and a run that did not get to the end of its pass has no
	// completion.
	if row.CorrectsRunID.Valid {
		run.CorrectsRunID = uuid.UUID(row.CorrectsRunID.Bytes)
	}
	if row.CompletedAt.Valid {
		run.CompletedAt = row.CompletedAt.Time.UTC()
	}
	// An empty cloud list means every cloud, and the empty slice is what says so
	// to an exporter: nil would render as a null, which reads as a missing value
	// rather than as an unrestricted run.
	if run.Clouds == nil {
		run.Clouds = []string{}
	}

	if run.Statements, err = loadStatements(ctx, q, id, runID); err != nil {
		return Run{}, err
	}
	if run.Rated, err = loadRated(ctx, q, id, runID); err != nil {
		return Run{}, err
	}
	if run.Kind == runs.KindCorrection {
		if run.Deltas, err = loadDeltas(ctx, q, id, runID); err != nil {
			return Run{}, err
		}
	}
	if run.Kickbacks, err = loadKickbacks(ctx, q, run); err != nil {
		return Run{}, err
	}
	return run, nil
}

// loadStatements reads the documents the run billed. The project_id column
// stores the statements.Key rendering of the cloud and the project, which is
// what the exporters name their files and their index entries from.
func loadStatements(ctx context.Context, q *sqlcgen.Queries, id pgtype.UUID, runID uuid.UUID) (
	[]statements.Statement, error,
) {
	rows, err := q.ListProjectStatements(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading the statements of run %s: %w", runID, err)
	}

	result := make([]statements.Statement, 0, len(rows))
	for _, row := range rows {
		total, number := amountOf(row.Total)
		if !number {
			return nil, fmt.Errorf("the total of statement %s of run %s is not a number", row.ProjectID, runID)
		}
		result = append(result, statements.Statement{
			Key:      row.ProjectID,
			Document: row.Document,
			Total:    total,
			Currency: row.Currency,
		})
	}
	return result, nil
}

// loadRated reads the run's rated records and the usage records they rate. The
// quantity a record was billed on is read back out of the stored usage object
// rather than stored a second time beside the amount, so an export shows the
// number the amount was computed from.
func loadRated(ctx context.Context, q *sqlcgen.Queries, id pgtype.UUID, runID uuid.UUID) ([]RatedRecord, error) {
	rows, err := q.ListRatedRecords(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading the rated records of run %s: %w", runID, err)
	}

	result := make([]RatedRecord, 0, len(rows))
	for _, row := range rows {
		amount, number := amountOf(row.Amount)
		if !number {
			return nil, fmt.Errorf("the rated amount of %s/%s/%s/%s under %s for %s of run %s is not a number",
				row.Cloud, row.Platform, row.ResourceType, row.ResourceID,
				row.ProjectID, row.Dimension, runID)
		}
		usage, err := usageOf(row)
		if err != nil {
			return nil, err
		}
		// The readable flag is ignored on purpose. A value nothing reads a number
		// from was rated at 0.00 by the run that stored it, and the export shows
		// the 0.0000 it was billed at rather than refusing a record the invoice
		// already carries. Which values those are is reported by the run itself,
		// as runs.stats.unreadable.
		quantity, _ := rating.QuantityOf(usage, row.Dimension)
		result = append(result, RatedRecord{
			Resource: source.Resource{
				Cloud:        row.Cloud,
				Platform:     row.Platform,
				ResourceType: row.ResourceType,
				ResourceID:   row.ResourceID,
			},
			ProjectID: row.ProjectID,
			State:     row.State,
			FromTS:    row.FromTs.Time.UTC(),
			ToTS:      row.ToTs.Time.UTC(),
			Dimension: row.Dimension,
			Quantity:  money.RoundQuantity(quantity),
			Amount:    amount,
			Currency:  row.Currency,
		})
	}
	return result, nil
}

// loadDeltas reads what a correction run credited or debited, in the order
// corrections.Diff sorted the deltas in.
func loadDeltas(ctx context.Context, q *sqlcgen.Queries, id pgtype.UUID, runID uuid.UUID) ([]Delta, error) {
	rows, err := q.ListCorrectionDeltas(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("reading the deltas of run %s: %w", runID, err)
	}

	result := make([]Delta, 0, len(rows))
	for _, row := range rows {
		old, oldNumber := amountOf(row.OldAmount)
		current, currentNumber := amountOf(row.NewAmount)
		difference, differenceNumber := amountOf(row.Delta)
		if !oldNumber || !currentNumber || !differenceNumber {
			return nil, fmt.Errorf("the delta of %s/%s/%s/%s under %s for %s of run %s is not a number",
				row.Cloud, row.Platform, row.ResourceType, row.ResourceID,
				row.ProjectID, row.Dimension, runID)
		}
		result = append(result, Delta{
			Delta: corrections.Delta{
				Key: corrections.Key{
					Cloud:        row.Cloud,
					Platform:     row.Platform,
					ResourceType: row.ResourceType,
					ResourceID:   row.ResourceID,
					ProjectID:    row.ProjectID,
					Dimension:    row.Dimension,
				},
				Old:   old,
				New:   current,
				Delta: difference,
			},
			Currency: row.Currency,
		})
	}
	return result, nil
}

// usageOf decodes the usage object a rated record was computed from. The
// decoder reads JSON numbers as json.Number, so a size that outgrew the range a
// float64 holds exactly reaches rating.QuantityOf as the text it was stored as
// rather than as the nearest float.
func usageOf(row sqlcgen.ListRatedRecordsRow) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(row.Usage))
	decoder.UseNumber()

	var usage map[string]any
	if err := decoder.Decode(&usage); err != nil {
		return nil, fmt.Errorf("decoding the usage of %s/%s/%s/%s over [%s, %s): %w",
			row.Cloud, row.Platform, row.ResourceType, row.ResourceID,
			row.FromTs.Time.UTC().Format(time.RFC3339Nano),
			row.ToTs.Time.UTC().Format(time.RFC3339Nano), err)
	}
	return usage, nil
}

// amountOf maps a stored numeric to a decimal, and reports whether it is one. A
// NULL and a NaN are refused where they are read, the way baselineAmounts in
// internal/engine/runs/correct.go refuses them: neither is an amount anything
// can be invoiced for.
func amountOf(n pgtype.Numeric) (decimal.Decimal, bool) {
	if !n.Valid || n.NaN {
		return decimal.Decimal{}, false
	}
	return decimal.NewFromBigInt(n.Int, n.Exp), true
}

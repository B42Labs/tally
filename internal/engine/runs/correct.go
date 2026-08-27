package runs

import (
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
	"github.com/b42labs/tally/internal/engine/adjustments"
	"github.com/b42labs/tally/internal/engine/attribution"
	"github.com/b42labs/tally/internal/engine/corrections"
	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// CorrectOptions is what one correction meters. There is no cloud list: a
// correction inherits the clouds of the run it corrects, because it re-meters
// what that run billed rather than a set of clouds a caller chooses.
type CorrectOptions struct {
	// PeriodFrom and PeriodTo are the half-open interval of the billing month,
	// as internal/engine/period derives it.
	PeriodFrom, PeriodTo time.Time
	// AttributingRelationTypes are the relation types attribution walks. An
	// empty list is attribution turned off.
	AttributingRelationTypes []string
	// AdjustmentRelationTypes are the relation types adjustment resolution
	// walks from a statement's project. An empty list is adjustments turned
	// off.
	AdjustmentRelationTypes []string
	// AdjustmentDepth bounds that walk: how many relation levels it follows
	// from the statement's project, at least 1. The configuration checks it,
	// and so does adjustments.New.
	AdjustmentDepth int
	// Counters is the counter sources file of the deployment, empty where it
	// measures no counter metric.
	Counters counters.Config
	// VM answers the metricsql counter sources. It is nil unless
	// Counters.HasMetricsQL() reports that any source needs it.
	VM counters.Querier
}

// CorrectionResult is what one call to Correct did.
type CorrectionResult struct {
	// RunID is the correction run that was opened. It is the zero id when the
	// call was refused before any row was written.
	RunID uuid.UUID
	// CorrectsRunID is the finalized run the correction diffed against, which
	// its run row and every delta it wrote name.
	CorrectsRunID uuid.UUID
	// PricingVersion is the model version the period was rated with, which is
	// the one the corrected run recorded.
	PricingVersion string
	// Stats is what the correction counted, the same object it stored in
	// runs.stats.
	Stats CorrectionStats
	// Superseded are the completed correction runs of the period this one
	// replaced.
	Superseded []uuid.UUID
	// Reclaimed are the runs of the period that were still 'running' with no
	// process behind them and were failed before this run opened.
	Reclaimed []uuid.UUID
}

// CorrectionStats is what a correction stores in runs.stats: everything a
// regular run counts, under the same keys, and the deltas it wrote beside them.
// Statements counts the credit notes, which are the documents a correction
// renders. AdjustmentDeltas counts the adjustments the correction applies
// differently than the run it corrects.
type CorrectionStats struct {
	Stats
	Deltas           int `json:"deltas"`
	AdjustmentDeltas int `json:"adjustment_deltas"`
}

// Correct meters a finalized period again with the pricing version the latest
// finalized run of it recorded, diffs what came out against that run's amounts
// per the key of decision D6, and writes every non-zero difference as a
// correction_deltas row and one credit note per affected project, under a run
// of kind 'correction'. It is the whole of what tally-engine correct does. The
// engine pool is where the correction is written, the reporting database is
// where it is read from, and neither is closed here.
//
// The baseline is the latest finalized run of the period by started_at: the
// regular run that closed the month for the first correction, and the last
// finalized correction after that. That run is what corrects_run_id names, on
// the run row and on every delta. A correction that is completed but not
// finalized is not the period's truth yet, so the next call diffs against the
// same baseline, produces the same deltas again, and supersedes it the way a
// regular run supersedes the completed run it replaces. The baseline cannot
// move while the pass runs, because Finalize takes the period lock this call
// holds for its whole length.
//
// The clouds are the ones the corrected run metered, and the counter metrics
// are measured again rather than read off that run: a correction re-meters the
// period whole (D6).
//
// The period is locked for the length of the call. A period another process is
// metering yields an error wrapping ErrRunInProgress, a period that is not
// closed one wrapping ErrPeriodNotFinalized, and a pricing version the database
// no longer holds one wrapping pricing.ErrVersionNotFound; none of them writes
// a row. Any failure once the run row exists ends that row as 'failed' with the
// reason in its stats and comes back as the error that caused it. The one
// exception is the release of the period lock: it happens after the correction
// is committed, so a failure there comes back wrapping ErrLockReleaseFailed
// beside the CorrectionResult of a correction that is done.
func Correct(
	ctx context.Context,
	engine *pgxpool.Pool,
	reporting *source.DB,
	opts CorrectOptions,
) (CorrectionResult, error) {
	var result CorrectionResult
	retry := "tally-engine correct --period " + period.Format(opts.PeriodFrom)
	err := withPeriodLock(ctx, engine, opts.PeriodFrom, retry, func() (err error) {
		result, err = correct(ctx, engine, reporting, opts)
		return err
	})
	return result, err
}

// correct is Correct under the period lock: everything from the finalized run
// the correction is diffed against to the completed correction run.
func correct(
	ctx context.Context,
	engine *pgxpool.Pool,
	reporting *source.DB,
	opts CorrectOptions,
) (CorrectionResult, error) {
	var result CorrectionResult
	q := sqlcgen.New(engine)
	month := period.Format(opts.PeriodFrom)

	// The period is read rather than recorded: what a correction books is what
	// arrived after the run that billed the month, so a month the engine does
	// not know is refused here rather than opened.
	billingPeriod, err := q.GetBillingPeriod(ctx, timestamptz(opts.PeriodFrom))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, fmt.Errorf(
				"%w: %s has no billing period, and a correction runs over a finalized month: "+
					"tally-engine run --period %s and tally-engine finalize produce one",
				ErrPeriodNotFinalized, month, month)
		}
		return result, fmt.Errorf("reading the billing period %s: %w", month, err)
	}
	if billingPeriod.Status != statusFinalized {
		return result, fmt.Errorf(
			"%w: %s is %s, and a correction runs over a finalized month: "+
				"tally-engine run --period %s and tally-engine finalize close it",
			ErrPeriodNotFinalized, month, billingPeriod.Status, month)
	}

	// Under the period lock a run of this period that still reads as running has
	// no process behind it: whatever opened it is gone.
	reclaimed, err := q.ReclaimStaleRuns(ctx, timestamptz(opts.PeriodFrom))
	if err != nil {
		return result, fmt.Errorf("reclaiming the stale runs of %s: %w", month, err)
	}
	result.Reclaimed = uuidsOf(reclaimed)

	baseline, err := q.LatestFinalizedRun(ctx, timestamptz(opts.PeriodFrom))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return result, fmt.Errorf("%s is finalized but carries no finalized run", month)
		}
		return result, fmt.Errorf("reading the finalized run of %s: %w", month, err)
	}
	baselineID := uuid.UUID(baseline.ID.Bytes)
	if !baseline.PricingVersion.Valid {
		return result, fmt.Errorf("the finalized run %s of %s carries no pricing version", baselineID, month)
	}

	// The prices are the ones the corrected run was rated with, whatever has
	// been imported since: a correction fixes the usage of a month and leaves
	// its prices where they were (D6). They are resolved before the run row
	// exists, so a version the database no longer holds leaves no run behind to
	// explain.
	model, err := pricing.ByVersion(ctx, q, baseline.PricingVersion.String)
	if err != nil {
		return result, err
	}

	old, err := baselineAmounts(ctx, q, baselineID, model)
	if err != nil {
		return result, err
	}
	oldAdjustments, err := baselineAdjustments(ctx, q, baselineID, model)
	if err != nil {
		return result, err
	}

	// A nil cloud list is bound as the empty array rather than passed on: pgx
	// encodes nil as SQL NULL and runs.clouds is NOT NULL. Both spellings mean
	// every cloud, which is what the column's default says.
	clouds := baseline.Clouds
	if clouds == nil {
		clouds = []string{}
	}
	// This insert commits on its own, so a process killed from here on leaves a
	// 'running' row for the next run of the period to reclaim above.
	run, err := q.InsertRun(ctx, sqlcgen.InsertRunParams{
		PeriodFrom:     timestamptz(opts.PeriodFrom),
		PeriodTo:       timestamptz(opts.PeriodTo),
		Kind:           KindCorrection,
		CorrectsRunID:  uuidValue(baselineID),
		PricingVersion: pgtype.Text{String: model.Version, Valid: true},
		Clouds:         clouds,
	})
	if err != nil {
		return result, fmt.Errorf("opening the correction run of %s: %w", month, err)
	}
	result.RunID = uuid.UUID(run.ID.Bytes)
	result.CorrectsRunID = baselineID
	result.PricingVersion = model.Version

	pass := Options{
		PeriodFrom:               opts.PeriodFrom,
		PeriodTo:                 opts.PeriodTo,
		Clouds:                   clouds,
		AttributingRelationTypes: opts.AttributingRelationTypes,
		AdjustmentRelationTypes:  opts.AdjustmentRelationTypes,
		AdjustmentDepth:          opts.AdjustmentDepth,
		Counters:                 opts.Counters,
		VM:                       opts.VM,
	}

	var stats CorrectionStats
	superseded, err := produceCorrection(
		ctx, engine, reporting, pass, model, result.RunID, baselineID, old, oldAdjustments, &stats)
	if err != nil {
		stats.Error = err.Error()
		result.Stats = stats
		return result, recordFailure(ctx, engine, result.RunID, stats, err)
	}
	result.Stats = stats
	result.Superseded = superseded
	return result, nil
}

// baselineAmounts sums what the corrected run billed, per the key the two
// passes are diffed by. An amount that is not a number, and a row rated in
// another currency than the model prices in, are refused by naming what carries
// them: a difference built from either would be arithmetic over two different
// things. A run with no rated records yields an empty map, against which every
// amount the correction rates is a delta of its own size.
func baselineAmounts(ctx context.Context, q *sqlcgen.Queries, baselineID uuid.UUID, model pricing.Model) (
	map[corrections.Key]decimal.Decimal, error,
) {
	rows, err := q.SumRatedByRun(ctx, uuidValue(baselineID))
	if err != nil {
		return nil, fmt.Errorf("reading the rated amounts of run %s: %w", baselineID, err)
	}

	amounts := make(map[corrections.Key]decimal.Decimal, len(rows))
	for _, row := range rows {
		if !row.Amount.Valid || row.Amount.NaN {
			return nil, fmt.Errorf("the rated amount of %s/%s/%s/%s under %s for %s of run %s is not a number",
				row.Cloud, row.Platform, row.ResourceType, row.ResourceID,
				row.ProjectID, row.Dimension, baselineID)
		}
		if row.Currency != model.Currency {
			return nil, fmt.Errorf("the finalized run %s was rated in %s and pricing model %s is in %s",
				baselineID, row.Currency, model.Version, model.Currency)
		}
		amounts[corrections.Key{
			Cloud:        row.Cloud,
			Platform:     row.Platform,
			ResourceType: row.ResourceType,
			ResourceID:   row.ResourceID,
			ProjectID:    row.ProjectID,
			Dimension:    row.Dimension,
		}] = decimal.NewFromBigInt(row.Amount.Int, row.Amount.Exp)
	}
	return amounts, nil
}

// baselineAdjustments sums what the corrected run applied, per the key the
// adjustments of the two passes are diffed by. A rate or an amount that is not
// a number, and a row applied in another currency than the model prices in, are
// refused by naming what carries them: a difference built from either would be
// arithmetic over two different things, and a numeric with no coefficient is
// not a decimal at all. A run with no adjustment records, one finalized before
// the records existed included, yields an empty map, against which every
// adjustment the correction applies is a delta of its own size.
func baselineAdjustments(ctx context.Context, q *sqlcgen.Queries, baselineID uuid.UUID, model pricing.Model) (
	map[corrections.AdjustmentKey]corrections.AdjustmentAmount, error,
) {
	rows, err := q.ListAdjustmentRecords(ctx, uuidValue(baselineID))
	if err != nil {
		return nil, fmt.Errorf("reading the adjustment records of run %s: %w", baselineID, err)
	}

	amounts := make(map[corrections.AdjustmentKey]corrections.AdjustmentAmount, len(rows))
	for _, row := range rows {
		relationID := uuid.UUID(row.RelationID.Bytes)
		if !row.Amount.Valid || row.Amount.NaN || !row.Rate.Valid || row.Rate.NaN {
			return nil, fmt.Errorf("the %s adjustment of relation %s on %s of run %s is not a number",
				row.Type, relationID, row.ProjectID, baselineID)
		}
		if row.Currency != model.Currency {
			return nil, fmt.Errorf("the finalized run %s carries an adjustment in %s and pricing model %s is in %s",
				baselineID, row.Currency, model.Version, model.Currency)
		}
		rate := decimal.NewFromBigInt(row.Rate.Int, row.Rate.Exp)
		key := corrections.AdjustmentKey{
			StatementKey: row.ProjectID,
			RelationID:   relationID.String(),
			Type:         row.Type,
			Scope:        row.Scope,
			Rate:         rate.StringFixed(money.RatePlaces),
		}
		amounts[key] = corrections.AdjustmentAmount{
			RelationType:   row.RelationType,
			RelationTarget: row.RelationTarget,
			RateValue:      rate,
			Amount:         amounts[key].Amount.Add(decimal.NewFromBigInt(row.Amount.Int, row.Amount.Exp)),
		}
	}
	return amounts, nil
}

// produceCorrection meters, rates, diffs, renders and writes one correction,
// and returns the completed corrections it superseded. old and oldAdjustments
// are what the run being corrected billed and applied, and stats is filled as
// the passes report, so a failure carries what the correction got to rather
// than an empty object.
func produceCorrection(
	ctx context.Context,
	engine *pgxpool.Pool,
	reporting *source.DB,
	opts Options,
	model pricing.Model,
	runID, baselineID uuid.UUID,
	old map[corrections.Key]decimal.Decimal,
	oldAdjustments map[corrections.AdjustmentKey]corrections.AdjustmentAmount,
	stats *CorrectionStats,
) ([]uuid.UUID, error) {
	metered, g, err := meter(ctx, reporting, opts, &stats.Stats)
	if err != nil {
		return nil, err
	}
	warnPeriodNotEnded(&stats.Stats, opts.PeriodTo)

	// The rest of the pass is arithmetic over what the snapshot handed out: it
	// reads nothing and runs with the reporting connection already given back.
	rated := rating.Rate(model, metered.Resources)
	stats.Unpriced = rated.Unpriced
	stats.Unreadable = rated.Unreadable

	resolution := attribution.Resolve(g.projects, g.attributing)
	stats.AttributionWarnings = resolution.Warnings

	// No adjustment relation type is adjustments turned off, and a nil adjuster
	// is what the statement build renders a period without them from. The error
	// passes through as it is: it names the depth or the relation it could not
	// be built from. A relation whose stored adjustments cannot be read fails
	// the statement build below instead, and only where a walk reaches it.
	var adjuster *adjustments.Adjuster
	if len(opts.AdjustmentRelationTypes) > 0 {
		var warnings []adjustments.Warning
		adjuster, warnings, err = adjustments.New(g.adjusting, g.projects, opts.AdjustmentDepth)
		if err != nil {
			return nil, err
		}
		stats.AdjustmentWarnings = warnings
	}

	current, err := corrections.Amounts(metered.Resources, rated)
	if err != nil {
		return nil, err
	}
	deltas := corrections.Diff(old, current)

	// The full statements of the re-metered period. Their documents are
	// discarded, because what a correction stores are its credit notes; what
	// the build is run for are the adjustment lines, which become the
	// correction's own adjustment records and the current side of the
	// adjustment diff.
	built, err := statements.Build(
		opts.PeriodFrom, opts.PeriodTo, metered.Resources, rated, g.projects, resolution, adjuster)
	if err != nil {
		return nil, err
	}
	records, err := adjustmentRows(runID, built.Statements)
	if err != nil {
		return nil, err
	}
	adjustmentDeltas := corrections.DiffAdjustments(oldAdjustments, corrections.AdjustmentAmounts(built.Statements))

	notes, err := corrections.BuildCreditNotes(
		opts.PeriodFrom, opts.PeriodTo, baselineID, model.Currency,
		deltas, adjustmentDeltas, g.projects, resolution)
	if err != nil {
		return nil, err
	}
	stats.UnregisteredProjects = notes.Unregistered

	// A correction stores what it metered under itself, the way a regular run
	// does, so what its deltas were computed from can be read back off its own
	// records rather than metered again.
	usage, ids, err := usageRows(runID, metered.Resources)
	if err != nil {
		return nil, err
	}
	amounts, err := ratedRows(runID, rated, ids)
	if err != nil {
		return nil, err
	}
	differences, err := deltaRows(runID, baselineID, deltas, model.Currency)
	if err != nil {
		return nil, err
	}
	stats.UsageRecords = len(usage)
	stats.RatedRecords = len(amounts)
	stats.Statements = len(notes.Statements)
	stats.AdjustmentRecords = len(records)
	stats.Deltas = len(deltas)
	stats.AdjustmentDeltas = len(adjustmentDeltas)

	payload, err := json.Marshal(*stats)
	if err != nil {
		return nil, fmt.Errorf("marshalling the stats of run %s: %w", runID, err)
	}
	return write(ctx, engine, opts.PeriodFrom, runID, KindCorrection,
		usage, amounts, differences, records, notes.Statements, payload)
}

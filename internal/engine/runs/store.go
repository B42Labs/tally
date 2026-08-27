package runs

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/adjustment"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/corrections"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// maxRatedAmount is the largest amount rated_records.amount holds:
// NUMERIC(14,2) leaves twelve digits ahead of the point. It is the bound
// statements.Persist holds a document's total to, one level down, and it is
// checked for the same reason: nothing upstream bounds a usage quantity, the
// database reports the overflow by naming the column alone, and the event the
// amount was rated from is immutable, so every re-run of the period fails the
// same way until someone is told which resource and which dimension to look at.
var maxRatedAmount = decimal.RequireFromString("999999999999.99")

// usageRows builds the usage records of one run, one per draft, in the order
// metering produced them. Beside the rows it returns the ids of each resource's
// records, indexed the way its drafts are, which is what lets a rated record
// name the usage record it was rated from before either row is written.
//
// The ids are generated here rather than left to the column default, because
// COPY evaluates no defaults and both tables go in over COPY.
//
// Its only error is a usage object that does not marshal, named by the resource
// it belongs to.
func usageRows(runID uuid.UUID, resources []metering.ResourceUsage) (
	[]sqlcgen.CreateUsageRecordsParams, map[source.Resource][]uuid.UUID, error,
) {
	rows := make([]sqlcgen.CreateUsageRecordsParams, 0, len(resources))
	ids := make(map[source.Resource][]uuid.UUID, len(resources))

	for _, resource := range resources {
		drafts := make([]uuid.UUID, 0, len(resource.Drafts))
		for _, draft := range resource.Drafts {
			usage, err := json.Marshal(draft.Usage)
			if err != nil {
				return nil, nil, fmt.Errorf("marshalling the usage of %s over [%s, %s): %w",
					name(resource.Resource), instant(draft.FromTS), instant(draft.ToTS), err)
			}

			id := uuid.New()
			drafts = append(drafts, id)
			rows = append(rows, sqlcgen.CreateUsageRecordsParams{
				ID:           uuidValue(id),
				RunID:        uuidValue(runID),
				Cloud:        resource.Resource.Cloud,
				Platform:     resource.Resource.Platform,
				ResourceType: resource.Resource.ResourceType,
				ResourceID:   resource.Resource.ResourceID,
				ProjectID:    draft.ProjectID,
				State:        draft.State,
				FromTs:       timestamptz(draft.FromTS),
				ToTs:         timestamptz(draft.ToTS),
				Seconds:      draft.Seconds,
				Usage:        usage,
			})
		}
		ids[resource.Resource] = drafts
	}
	return rows, ids, nil
}

// ratedRows builds the rated records of one run: one per dimension of every
// rated record, each naming the usage record of the draft it rates. ids is what
// usageRows returned, so record j of a resource is stored against draft j of
// the same resource.
//
// A rated resource the metering pass did not produce, and one whose record
// count differs from its draft count, are errors naming that resource rather
// than rows written against whichever usage record lines up. So is an amount
// past what the column holds, named by its resource and its dimension: it is
// refused here, before the first insert, because the transaction that would
// report it has already written the usage records the retry would collide with.
func ratedRows(runID uuid.UUID, rated rating.Result, ids map[source.Resource][]uuid.UUID) (
	[]sqlcgen.CreateRatedRecordsParams, error,
) {
	rows := make([]sqlcgen.CreateRatedRecordsParams, 0, len(rated.Resources))

	for _, resource := range rated.Resources {
		drafts, metered := ids[resource.Resource]
		if !metered {
			return nil, fmt.Errorf("the rated resource %s carries no metered usage", name(resource.Resource))
		}
		if len(resource.Records) != len(drafts) {
			return nil, fmt.Errorf("the rated resource %s carries %d records for %d usage drafts",
				name(resource.Resource), len(resource.Records), len(drafts))
		}

		for i, record := range resource.Records {
			for _, dimension := range record.Amounts {
				if dimension.Amount.Abs().GreaterThan(maxRatedAmount) {
					return nil, fmt.Errorf(
						"the %s amount of %s is %s, past the %s the column holds: "+
							"a usage value it was rated from is out of range",
						dimension.Metric, name(resource.Resource), dimension.Amount.StringFixed(2), maxRatedAmount)
				}

				// The column is NUMERIC(14,2), and the decimal reaches it as the
				// text of the amount rather than through a float.
				var amount pgtype.Numeric
				if err := amount.Scan(dimension.Amount.StringFixed(2)); err != nil {
					return nil, fmt.Errorf("reading the %s amount of %s: %w",
						dimension.Metric, name(resource.Resource), err)
				}

				rows = append(rows, sqlcgen.CreateRatedRecordsParams{
					ID:            uuidValue(uuid.New()),
					RunID:         uuidValue(runID),
					UsageRecordID: uuidValue(drafts[i]),
					Dimension:     dimension.Metric,
					Amount:        amount,
					Currency:      rated.Currency,
				})
			}
		}
	}
	return rows, nil
}

// deltaRows builds the correction deltas of one run: one row per non-zero
// difference the diff found, in the order it produced them, each naming the
// finalized run whose amounts the old side comes from.
//
// An amount past what the column holds is refused before the first insert,
// named by its resource and its dimension, for the reason ratedRows refuses
// one: the transaction that would report it has already written the usage
// records the retry would collide with. All three amounts of a delta are
// checked, because the difference of two amounts the column holds is one it
// need not hold.
func deltaRows(runID, correctsRunID uuid.UUID, deltas []corrections.Delta, currency string) (
	[]sqlcgen.CreateCorrectionDeltasParams, error,
) {
	rows := make([]sqlcgen.CreateCorrectionDeltasParams, 0, len(deltas))
	for _, delta := range deltas {
		// The three columns are NUMERIC(14,2), and the decimals reach them as
		// the text of the amounts rather than through a float.
		var amounts [3]pgtype.Numeric
		for i, amount := range [3]decimal.Decimal{delta.Old, delta.New, delta.Delta} {
			if amount.Abs().GreaterThan(maxRatedAmount) {
				return nil, fmt.Errorf(
					"the %s delta of %s is %s, past the %s the column holds: "+
						"a usage value it was rated from is out of range",
					delta.Dimension, name(resourceOf(delta.Key)), amount.StringFixed(2), maxRatedAmount)
			}
			if err := amounts[i].Scan(amount.StringFixed(2)); err != nil {
				return nil, fmt.Errorf("reading the %s delta of %s: %w",
					delta.Dimension, name(resourceOf(delta.Key)), err)
			}
		}

		rows = append(rows, sqlcgen.CreateCorrectionDeltasParams{
			ID:            uuidValue(uuid.New()),
			RunID:         uuidValue(runID),
			CorrectsRunID: uuidValue(correctsRunID),
			Cloud:         delta.Cloud,
			Platform:      delta.Platform,
			ResourceType:  delta.ResourceType,
			ResourceID:    delta.ResourceID,
			ProjectID:     delta.ProjectID,
			Dimension:     delta.Dimension,
			OldAmount:     amounts[0],
			NewAmount:     amounts[1],
			Delta:         amounts[2],
			Currency:      currency,
		})
	}
	return rows, nil
}

// adjustmentRows builds the adjustment records of one run: one row per
// adjustment line of every statement, in statement and then application order,
// each naming the relation the adjustment came from. Beneficiary carries the
// relation's target on a kickback and nothing on the other types, because a
// kickback is the one adjustment somebody is owed the amount of.
//
// The ids are generated here rather than left to the column default, because
// COPY evaluates no defaults.
//
// A base or an amount past what the column holds is refused before the first
// insert, named by its type, its relation and its statement, for the reason
// ratedRows refuses one. So is a relation id that does not parse, which the
// line carrying it as the text of a uuid.UUID leaves unreachable.
func adjustmentRows(runID uuid.UUID, sts []statements.Statement) (
	[]sqlcgen.CreateAdjustmentRecordsParams, error,
) {
	rows := make([]sqlcgen.CreateAdjustmentRecordsParams, 0, len(sts))
	for _, st := range sts {
		for _, line := range st.Adjustments {
			relationID, err := uuid.Parse(line.RelationID)
			if err != nil {
				return nil, fmt.Errorf("reading the relation id %q of the %s adjustment on %s: %w",
					line.RelationID, line.Type, st.Key, err)
			}

			// The three columns are NUMERIC, the rate at six places and the two
			// amounts at two, and the decimals reach them as their own text
			// rather than through a float.
			var rate pgtype.Numeric
			if err := rate.Scan(line.Rate.StringFixed(money.RatePlaces)); err != nil {
				return nil, fmt.Errorf("reading the %s adjustment of relation %s on %s: %w",
					line.Type, line.RelationID, st.Key, err)
			}
			var amounts [2]pgtype.Numeric
			for i, value := range [2]decimal.Decimal{line.Base.Decimal, line.Amount.Decimal} {
				if value.Abs().GreaterThan(maxRatedAmount) {
					return nil, fmt.Errorf(
						"the %s adjustment of relation %s on %s is %s, past the %s the column holds: "+
							"a usage value it was rated from is out of range",
						line.Type, line.RelationID, st.Key, value.StringFixed(2), maxRatedAmount)
				}
				if err := amounts[i].Scan(value.StringFixed(2)); err != nil {
					return nil, fmt.Errorf("reading the %s adjustment of relation %s on %s: %w",
						line.Type, line.RelationID, st.Key, err)
				}
			}

			rows = append(rows, sqlcgen.CreateAdjustmentRecordsParams{
				ID:             uuidValue(uuid.New()),
				RunID:          uuidValue(runID),
				ProjectID:      st.Key,
				RelationID:     uuidValue(relationID),
				RelationType:   line.RelationType,
				RelationTarget: line.RelationTarget,
				Beneficiary: pgtype.Text{
					String: line.RelationTarget,
					Valid:  line.Type == adjustment.TypeKickback,
				},
				Type:     line.Type,
				Scope:    line.Scope,
				Rate:     rate,
				Base:     amounts[0],
				Amount:   amounts[1],
				Currency: st.Currency,
			})
		}
	}
	return rows, nil
}

// resourceOf is the resource a delta's key names, which is what an error about
// that delta identifies it by.
func resourceOf(key corrections.Key) source.Resource {
	return source.Resource{
		Cloud:        key.Cloud,
		Platform:     key.Platform,
		ResourceType: key.ResourceType,
		ResourceID:   key.ResourceID,
	}
}

// uuidValue maps an id to the parameter the engine queries take.
func uuidValue(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

// uuidsOf maps the ids a query returned back. An empty result is nil, which is
// what a run reports when it superseded or reclaimed nothing.
func uuidsOf(values []pgtype.UUID) []uuid.UUID {
	if len(values) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(values))
	for _, value := range values {
		ids = append(ids, uuid.UUID(value.Bytes))
	}
	return ids
}

// timestamptz maps an instant to the query parameter.
func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// name identifies a resource in an error.
func name(resource source.Resource) string {
	return fmt.Sprintf("%s/%s/%s/%s",
		resource.Cloud, resource.Platform, resource.ResourceType, resource.ResourceID)
}

// instant formats a draft boundary in an error, at the scale the events carry.
func instant(ts time.Time) string {
	return ts.Format(time.RFC3339Nano)
}

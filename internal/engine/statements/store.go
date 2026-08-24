package statements

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// maxStatementTotal is the largest total project_statements.total holds:
// NUMERIC(14,2) leaves twelve digits ahead of the point.
var maxStatementTotal = decimal.RequireFromString("999999999999.99")

// Persist writes the statements of one run: one insert per statement, in the
// order they are passed, each under its own key as the project_statements row's
// project_id. An empty slice writes nothing and runs no query, because a period
// that bills nobody has nothing to store.
//
// A failure names the statement it happened on and wraps what the database
// said, and that wrap is the whole of the error handling here. The unique key
// over (run_id, project_id) reports a project written twice for one run, the
// record trigger reports a run that is already finalized, and a canceled
// context comes back as context.Canceled. Persist only ever inserts, and it
// opens no transaction of its own: a failure leaves the statements written
// before it behind, and discarding them is decided by the run transaction these
// inserts run in (roadmap/03-phase-3-metering-rating.md, WP 3.8).
//
// Every insert fires the record trigger, which locks the run row FOR SHARE for
// the rest of the caller's transaction. A transaction that also updates that
// run's row, which the lifecycle of WP 3.8 does every time it carries the run's
// stats along, must take the row first, with
//
//	SELECT id FROM runs WHERE id = $1 FOR NO KEY UPDATE
//
// before its first call here. Escalating the share lock afterwards deadlocks
// two writers of the same run, and PostgreSQL resolves that by aborting one of
// them with everything it had metered (migrations/engine/0001_init.sql,
// forbid_finalized_mutation).
func Persist(ctx context.Context, q *sqlcgen.Queries, runID uuid.UUID, sts []Statement) error {
	if len(sts) == 0 {
		return nil
	}

	// Nothing upstream bounds a usage quantity, so a size reported in the wrong
	// unit is rated into an amount the column cannot hold. Postgres reports that
	// as a numeric field overflow naming the column, which says nothing about
	// where the number came from, and the event it was rated from is immutable:
	// every re-run of the period would fail the same way. The bound is checked
	// here so the failure names the statement and what to look for.
	//
	// Every total is checked before the first insert rather than as its own
	// statement is reached. The check reads nothing but sts, and a caller that
	// does not wrap Persist in the run transaction would otherwise have the
	// statements ahead of the oversized one committed: the retry would then fail
	// on the unique key over (run_id, project_id) and report a duplicate key
	// instead of the range the operator has to go and look at.
	for _, st := range sts {
		if st.Total.Abs().GreaterThan(maxStatementTotal) {
			return fmt.Errorf("the statement of %s totals %s, past the %s the column holds: "+
				"a usage value it was rated from is out of range",
				st.Key, st.Total.StringFixed(2), maxStatementTotal)
		}
	}

	for _, st := range sts {
		// The column is NUMERIC(14,2), and the decimal reaches it as the text of
		// the amount the document shows rather than through a float.
		var total pgtype.Numeric
		if err := total.Scan(st.Total.StringFixed(2)); err != nil {
			return fmt.Errorf("inserting the statement of %s: %w", st.Key, err)
		}

		if err := q.InsertProjectStatement(ctx, sqlcgen.InsertProjectStatementParams{
			RunID:     pgtype.UUID{Bytes: runID, Valid: true},
			ProjectID: st.Key,
			Document:  st.Document,
			Total:     total,
			Currency:  st.Currency,
		}); err != nil {
			return fmt.Errorf("inserting the statement of %s: %w", st.Key, err)
		}
	}
	return nil
}

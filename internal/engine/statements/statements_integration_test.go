package statements_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/engine/statements"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
	"github.com/b42labs/tally/internal/engine/store/storetest"
)

// uniqueViolation is the SQLSTATE the unique key over (run_id, project_id)
// reports a project written twice for one run with.
const uniqueViolation = "23505"

// seedRun writes the run the statements are keyed to and returns its id. The
// status is a parameter because the record trigger reads it: a run seeded as
// 'finalized' is one nothing can be written to.
func seedRun(t *testing.T, db storetest.DB, status string) uuid.UUID {
	t.Helper()

	var runID uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, status)
		 VALUES ($1, $2, $3)
		 RETURNING id`, periodFrom, periodTo, status).Scan(&runID); err != nil {
		t.Fatalf("seeding the %s run: %v", status, err)
	}
	return runID
}

// statement is one statement as the cases below hand it to Persist, built by
// hand: what Persist stores is the key, the bytes, the total and the currency,
// and none of them depend on what a rendering pass put in the document.
func statement(t *testing.T, key, total string) statements.Statement {
	t.Helper()

	amount, err := decimal.NewFromString(total)
	if err != nil {
		t.Fatalf("parsing the total %q: %v", total, err)
	}
	document, err := json.Marshal(map[string]string{"project_id": key, "total": total})
	if err != nil {
		t.Fatalf("marshalling the document of %s: %v", key, err)
	}
	return statements.Statement{Key: key, Document: document, Total: amount, Currency: "EUR"}
}

// listStatements reads the statements of a run back.
func listStatements(t *testing.T, q *sqlcgen.Queries, runID uuid.UUID) []sqlcgen.ProjectStatement {
	t.Helper()

	rows, err := q.ListProjectStatements(t.Context(), pgtype.UUID{Bytes: runID, Valid: true})
	if err != nil {
		t.Fatalf("ListProjectStatements() error = %v, want nil", err)
	}
	return rows
}

// sameDocument reports whether two documents hold the same JSON. The column is
// jsonb, which Postgres stores as a parsed value and writes back in its own key
// order and spacing, so the comparison goes through a decode rather than over
// the bytes.
func sameDocument(t *testing.T, got, want []byte) bool {
	t.Helper()

	return reflect.DeepEqual(decode(t, got), decode(t, want))
}

// decode decodes one document into the generic values the comparison walks.
func decode(t *testing.T, document []byte) any {
	t.Helper()

	var value any
	if err := json.Unmarshal(document, &value); err != nil {
		t.Fatalf("decoding the document %s: %v", document, err)
	}
	return value
}

// totalOf is the stored total as text. NUMERIC(14,2) comes back as a
// pgtype.Numeric, and reading it as text is what keeps the assertion off floats
// (roadmap/00-conventions.md section 6).
func totalOf(t *testing.T, total pgtype.Numeric) string {
	t.Helper()

	value, err := total.Value()
	if err != nil {
		t.Fatalf("reading the stored total: %v", err)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("the stored total is a %T, want it as text", value)
	}
	return text
}

func TestPersistAndList(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())
	runID := seedRun(t, db, "running")

	gardener := statement(t, gardenerCloud+"/tenant-b", "100.00")
	first := statement(t, openstackCloud+"/tenant-a", "0.50")
	second := statement(t, openstackCloud+"/tenant-c", "12.34")
	// The largest total NUMERIC(14,2) holds, so the bound Persist refuses a
	// statement at is held against what the column actually takes.
	largest := statement(t, openstackCloud+"/tenant-d", "999999999999.99")

	// Passed out of key order on purpose: the listing orders by project_id, not
	// by the order the statements were written in.
	if err := statements.Persist(t.Context(), q, runID,
		[]statements.Statement{second, largest, gardener, first}); err != nil {
		t.Fatalf("Persist() error = %v, want nil", err)
	}

	rows := listStatements(t, q, runID)
	want := []statements.Statement{gardener, first, second, largest}
	if len(rows) != len(want) {
		t.Fatalf("ListProjectStatements() = %d rows, want %d", len(rows), len(want))
	}

	for i, row := range rows {
		st := want[i]
		if row.ProjectID != st.Key {
			t.Errorf("row %d project_id = %q, want %q", i, row.ProjectID, st.Key)
		}
		if !sameDocument(t, row.Document, st.Document) {
			t.Errorf("row %d document = %s, want %s", i, row.Document, st.Document)
		}
		if got, wantTotal := totalOf(t, row.Total), st.Total.StringFixed(2); got != wantTotal {
			t.Errorf("row %d total = %s, want %s", i, got, wantTotal)
		}
		if row.Currency != st.Currency {
			t.Errorf("row %d currency = %q, want %q", i, row.Currency, st.Currency)
		}
	}
}

func TestPersistEmpty(t *testing.T) {
	// No database: the nil *sqlcgen.Queries is the assertion. A query on it
	// panics, so returning nil is what proves no query was run.
	runID := uuid.New()

	if err := statements.Persist(t.Context(), nil, runID, nil); err != nil {
		t.Errorf("Persist(nil) error = %v, want nil", err)
	}
	if err := statements.Persist(t.Context(), nil, runID, []statements.Statement{}); err != nil {
		t.Errorf("Persist(empty slice) error = %v, want nil", err)
	}
}

func TestPersistDuplicateRun(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())
	runID := seedRun(t, db, "running")

	sts := []statements.Statement{
		statement(t, openstackCloud+"/tenant-a", "0.50"),
		statement(t, openstackCloud+"/tenant-c", "12.34"),
	}
	if err := statements.Persist(t.Context(), q, runID, sts); err != nil {
		t.Fatalf("Persist() error = %v, want nil", err)
	}

	err := statements.Persist(t.Context(), q, runID, sts)
	if err == nil {
		t.Fatal("Persist() error = nil, want the second statement of a project refused")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("Persist() error = %v, want a *pgconn.PgError", err)
	}
	if pgErr.Code != uniqueViolation {
		t.Errorf("Persist() SQLSTATE = %s, want %s for the unique key over (run_id, project_id)",
			pgErr.Code, uniqueViolation)
	}
	// The first statement is where the second pass collides, and the error has
	// to say which project that was rather than only that something collided.
	if want := "inserting the statement of " + sts[0].Key; !strings.Contains(err.Error(), want) {
		t.Errorf("Persist() error = %q, want it to name the colliding statement with %q", err, want)
	}
	if rows := listStatements(t, q, runID); len(rows) != len(sts) {
		t.Errorf("the run holds %d statements, want the %d of the first pass", len(rows), len(sts))
	}
}

// TestPersistPartialWrite persists a statement that collides after one that
// does not. Persist opens no transaction of its own, so the statement written
// before the failure is committed and stays behind: what to do about it is the
// run transaction's decision, and a caller that does nothing retries into a
// collision on a row it never meant to keep.
func TestPersistPartialWrite(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())
	runID := seedRun(t, db, "running")

	held := statement(t, openstackCloud+"/tenant-a", "0.50")
	fresh := statement(t, openstackCloud+"/tenant-c", "12.34")
	if err := statements.Persist(t.Context(), q, runID, []statements.Statement{held}); err != nil {
		t.Fatalf("Persist() error = %v, want nil", err)
	}

	err := statements.Persist(t.Context(), q, runID, []statements.Statement{fresh, held})
	if err == nil {
		t.Fatal("Persist() error = nil, want the second statement of a project refused")
	}
	if want := "inserting the statement of " + held.Key; !strings.Contains(err.Error(), want) {
		t.Errorf("Persist() error = %q, want it to name the colliding statement with %q", err, want)
	}

	rows := listStatements(t, q, runID)
	stored := make([]string, 0, len(rows))
	for _, row := range rows {
		stored = append(stored, row.ProjectID)
	}
	if want := []string{held.Key, fresh.Key}; !reflect.DeepEqual(stored, want) {
		t.Errorf("the run holds %v, want %v: the statement written before the collision is committed",
			stored, want)
	}
}

// TestPersistOversizedTotal refuses a statement whose total is past what
// NUMERIC(14,2) holds. Nothing upstream bounds a usage quantity, and the
// database would report the insert as a numeric field overflow naming the
// column rather than the statement or what produced the number.
//
// The second case puts the oversized statement behind one that fits. Persist
// opens no transaction of its own, so a caller that does not wrap it in the run
// transaction would have the statement ahead committed and see the retry fail
// on the unique key over (run_id, project_id) instead: the range that is the
// actual cause would be reported once and then never again.
func TestPersistOversizedTotal(t *testing.T) {
	// No database: the nil *sqlcgen.Queries is the assertion, the way
	// TestPersistEmpty uses it. A query on it panics, so returning the error is
	// what proves the total was refused before anything was written.
	runID := uuid.New()
	oversized := statement(t, openstackCloud+"/tenant-a", "1000000000000.00")

	for _, tc := range []struct {
		name string
		sts  []statements.Statement
	}{
		{name: "alone", sts: []statements.Statement{oversized}},
		{name: "behind a statement that fits", sts: []statements.Statement{
			statement(t, openstackCloud+"/tenant-b", "48.00"), oversized,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := statements.Persist(t.Context(), nil, runID, tc.sts)
			if err == nil {
				t.Fatal("Persist() error = nil, want the oversized total refused")
			}
			if !strings.Contains(err.Error(), oversized.Key) {
				t.Errorf("Persist() error = %q, want it to name the statement %q", err, oversized.Key)
			}
			if !strings.Contains(err.Error(), "out of range") {
				t.Errorf("Persist() error = %q, want it to say a usage value is out of range", err)
			}
		})
	}
}

func TestPersistFinalizedRun(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())
	// Seeded finalized rather than finalized afterwards: the runs trigger holds
	// a finalized row against every update, and what this case exercises is the
	// statement trigger on INSERT.
	runID := seedRun(t, db, "finalized")

	st := statement(t, openstackCloud+"/tenant-a", "0.50")

	err := statements.Persist(t.Context(), q, runID, []statements.Statement{st})
	if err == nil {
		t.Fatal("Persist() error = nil, want the write into a finalized run refused")
	}
	if !strings.Contains(err.Error(), "are immutable") {
		t.Errorf("Persist() error = %q, want the immutability trigger reported", err)
	}
	if want := "inserting the statement of " + st.Key; !strings.Contains(err.Error(), want) {
		t.Errorf("Persist() error = %q, want it to name the refused statement with %q", err, want)
	}
	if rows := listStatements(t, q, runID); len(rows) != 0 {
		t.Errorf("the finalized run holds %d statements, want none written", len(rows))
	}
}

func TestPersistCanceledContext(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())
	runID := seedRun(t, db, "running")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	st := statement(t, openstackCloud+"/tenant-a", "0.50")

	err := statements.Persist(ctx, q, runID, []statements.Statement{st})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Persist() error = %v, want one matching context.Canceled", err)
	}
	if want := "inserting the statement of " + st.Key; !strings.Contains(err.Error(), want) {
		t.Errorf("Persist() error = %q, want it to name the statement with %q", err, want)
	}
}

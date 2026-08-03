package audit_test

import (
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/audit"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// undefinedTable is the SQLSTATE Postgres reports for a statement naming a
// relation that does not exist.
const undefinedTable = "42P01"

// auditRow is an audit_log row as the assertions read it back through the pool,
// which is the way an operator would read it: outside the transaction that
// wrote it.
type auditRow struct {
	actor      string
	objectType pgtype.Text
	objectID   pgtype.Text
	details    map[string]any
}

func TestLog(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("commits the audit row together with the change it describes", func(t *testing.T) {
		const action = "ingest_credentials.create"
		details := map[string]any{"platform": "openstack", "cloud": "os-audit"}

		var credentialID uuid.UUID
		if err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
			q := sqlcgen.New(tx)

			id, err := q.CreateIngestCredential(t.Context(), sqlcgen.CreateIngestCredentialParams{
				Platform:    "openstack",
				Cloud:       "os-audit",
				TokenHash:   "hash-of-an-audited-credential",
				Description: pgtype.Text{String: "audit integration test", Valid: true},
			})
			if err != nil {
				return err
			}
			credentialID = id

			return audit.Log(t.Context(), q, audit.Entry{
				Actor:      "admin-cli",
				Action:     action,
				ObjectType: "ingest_credentials",
				ObjectID:   id.String(),
				Details:    details,
			})
		}); err != nil {
			t.Fatalf("WithTx() error = %v, want nil", err)
		}

		rows := auditRows(t, db, action)
		if len(rows) != 1 {
			t.Fatalf("audit rows for action %q = %d, want 1", action, len(rows))
		}
		row := rows[0]

		if row.actor != "admin-cli" {
			t.Errorf("actor = %q, want %q", row.actor, "admin-cli")
		}
		if row.objectType.String != "ingest_credentials" || !row.objectType.Valid {
			t.Errorf("object type = %#v, want %q", row.objectType, "ingest_credentials")
		}
		if row.objectID.String != credentialID.String() || !row.objectID.Valid {
			t.Errorf("object id = %#v, want %q", row.objectID, credentialID)
		}
		if !maps.Equal(row.details, details) {
			t.Errorf("details = %v, want %v", row.details, details)
		}
	})

	t.Run("stores no object and no details when the entry names none", func(t *testing.T) {
		const action = "sync_runs.start"

		if err := audit.Log(t.Context(), sqlcgen.New(db.Store.Pool()), audit.Entry{
			Actor:  "internal",
			Action: action,
		}); err != nil {
			t.Fatalf("Log() error = %v, want nil", err)
		}

		rows := auditRows(t, db, action)
		if len(rows) != 1 {
			t.Fatalf("audit rows for action %q = %d, want 1", action, len(rows))
		}
		row := rows[0]

		if row.objectType.Valid || row.objectID.Valid {
			t.Errorf("object type, id = %#v, %#v; want both NULL", row.objectType, row.objectID)
		}
		if row.details != nil {
			t.Errorf("details = %v, want NULL", row.details)
		}
	})

	t.Run("writes nothing when the details do not marshal to JSON", func(t *testing.T) {
		const action = "api_tokens.revoke"

		err := audit.Log(t.Context(), sqlcgen.New(db.Store.Pool()), audit.Entry{
			Actor:   "admin-cli",
			Action:  action,
			Details: map[string]any{"channel": make(chan int)},
		})
		if err == nil {
			t.Fatal("Log() error = nil, want the failed marshaling")
		}
		if prefix := "marshaling the audit details:"; !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("Log() error = %q, want it to start with %q", err, prefix)
		}

		if rows := auditRows(t, db, action); len(rows) != 0 {
			t.Errorf("audit rows for action %q = %d, want 0", action, len(rows))
		}
	})
}

// TestLogRollsBackTheChangeWhenTheInsertFails runs against a database of its
// own, because the only way to make the insert fail for reasons the caller did
// not cause is to take audit_log away.
func TestLogRollsBackTheChangeWhenTheInsertFails(t *testing.T) {
	db := storetest.NewDB(t)
	if _, err := db.Store.Pool().Exec(t.Context(), `DROP TABLE audit_log`); err != nil {
		t.Fatalf("dropping audit_log: %v", err)
	}

	const tokenHash = "hash-of-an-unaudited-credential"

	err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
		q := sqlcgen.New(tx)

		id, err := q.CreateIngestCredential(t.Context(), sqlcgen.CreateIngestCredentialParams{
			Platform:    "openstack",
			Cloud:       "os-audit-rollback",
			TokenHash:   tokenHash,
			Description: pgtype.Text{String: "audit integration test", Valid: true},
		})
		if err != nil {
			return err
		}

		return audit.Log(t.Context(), q, audit.Entry{
			Actor:      "admin-cli",
			Action:     "ingest_credentials.create",
			ObjectType: "ingest_credentials",
			ObjectID:   id.String(),
		})
	})
	if err == nil {
		t.Fatal("WithTx() error = nil, want the failed audit insert")
	}
	if prefix := "inserting the audit log row:"; !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("WithTx() error = %q, want it to start with %q", err, prefix)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("WithTx() error = %v, want a Postgres error", err)
	}
	if pgErr.Code != undefinedTable {
		t.Errorf("WithTx() error code = %s, want %s", pgErr.Code, undefinedTable)
	}

	var count int
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM ingest_credentials WHERE token_hash = $1`, tokenHash).Scan(&count); err != nil {
		t.Fatalf("counting the credentials: %v", err)
	}
	if count != 0 {
		t.Errorf("ingest credentials with the rolled back hash = %d, want 0", count)
	}
}

// auditRows reads every audit_log row an action left behind.
func auditRows(t *testing.T, db storetest.DB, action string) []auditRow {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT actor, object_type, object_id, details FROM audit_log WHERE action = $1 ORDER BY id`, action)
	if err != nil {
		t.Fatalf("querying the audit log for action %q: %v", action, err)
	}
	defer rows.Close()

	var found []auditRow
	for rows.Next() {
		var row auditRow
		if err := rows.Scan(&row.actor, &row.objectType, &row.objectID, &row.details); err != nil {
			t.Fatalf("scanning an audit log row: %v", err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the audit log for action %q: %v", action, err)
	}
	return found
}

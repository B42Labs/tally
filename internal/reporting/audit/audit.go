// Package audit records who changed what in the Reporting API. Every write
// leaves one audit_log row behind, and that row is written in the transaction
// that carries the change itself: Log takes a queries handle rather than a
// pool, so a caller inside store.WithTx passes sqlcgen.New(tx). An audit
// insert that fails then returns an error out of the transaction callback and
// takes the mutation down with it. A change the log does not name never
// reaches the database.
//
// An action names its object and the verb applied to it, <object>.<verb>:
// ingest_credentials.create, api_tokens.revoke, projects.create. The object
// part is the table the change lands in, which is what ObjectType holds as
// well, so the log answers both what happened to one row and who ran a whole
// kind of change.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.4.
package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// Entry is one audit record: who acted, what they did, and which object they
// did it to.
type Entry struct {
	// Actor is who caused the change: a credential or token id, the name of an
	// operator tool such as admin-cli, or "internal" for what the system does
	// on its own.
	Actor string
	// Action is what happened, written <object>.<verb>, for example
	// ingest_credentials.create.
	Action string
	// ObjectType is the kind of object the change touched, which is normally
	// the table it lives in. An empty ObjectType is stored as NULL.
	ObjectType string
	// ObjectID identifies the object within its type. An empty ObjectID is
	// stored as NULL.
	ObjectID string
	// Details holds whatever else is worth keeping about the change and is
	// stored as JSONB. A nil map is stored as NULL. Tokens and other secrets
	// never go in here: the log is readable by everyone who may read the
	// database.
	Details map[string]any
}

// Log writes e as one audit_log row through q. Callers pass a handle bound to
// the transaction that carries the change they are recording, which is
// sqlcgen.New(tx) inside store.WithTx, so that a failure here rolls that
// change back instead of leaving it unattributed.
func Log(ctx context.Context, q *sqlcgen.Queries, e Entry) error {
	details, err := marshalDetails(e.Details)
	if err != nil {
		return err
	}
	if err := q.InsertAuditLog(ctx, sqlcgen.InsertAuditLogParams{
		Actor:      e.Actor,
		Action:     e.Action,
		ObjectType: text(e.ObjectType),
		ObjectID:   text(e.ObjectID),
		Details:    details,
	}); err != nil {
		return fmt.Errorf("inserting the audit log row: %w", err)
	}
	return nil
}

// marshalDetails renders details for the JSONB column. A nil map becomes a nil
// slice, which pgx sends as NULL.
func marshalDetails(details map[string]any) ([]byte, error) {
	if details == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return nil, fmt.Errorf("marshaling the audit details: %w", err)
	}
	return encoded, nil
}

// text maps an entry's optional string to the column value: an empty string is
// the absence of one and becomes NULL.
func text(s string) pgtype.Text {
	return pgtype.Text{String: s, Valid: s != ""}
}

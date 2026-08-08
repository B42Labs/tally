package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// deadLetterCursorKeys is how many parts the sort key of this list has: the
// instant the item was refused and the row's id, in the order ORDER BY names
// them.
const deadLetterCursorKeys = 2

// deadLetterDetail answers every failure of this route a caller can do nothing
// about.
const deadLetterDetail = "the dead-lettered events could not be read"

// ListRejectedEvents answers one page of the items ingestion refused, so that a
// server-side rejection is something an operator can review after the collector
// has dropped the batch from its buffer. The page is read one row longer than it
// is served, which is what trimPage turns into the cursor decision.
//
// Nothing here reads a principal or resolves a scope. The dispatch table puts
// the route behind the admin role, so every request that arrives may read the
// whole table, and a refused item has no project to be scoped by: its raw JSON
// is whatever a collector sent, which is what made the item refusable in the
// first place.
func (s *server) ListRejectedEvents(w http.ResponseWriter, r *http.Request, params ListRejectedEventsParams) {
	ctx := r.Context()

	var cursorAt pgtype.Timestamptz
	var cursorID pgtype.UUID
	if params.Cursor != nil {
		var err error
		if cursorAt, cursorID, err = deadLetterCursor(*params.Cursor); err != nil {
			refuseCursor(ctx, w, err)
			return
		}
	}

	limit := pageLimit(params.Limit)

	// A from later than to yields nothing, which is what the half-open window
	// means and needs no case of its own: each bound is a valid instant on its
	// own, and the contract cannot express a rule spanning two parameters.
	rows, err := s.queries.ListRejectedEvents(ctx, sqlcgen.ListRejectedEventsParams{
		FromTs:   filterInstant(params.From),
		ToTs:     filterInstant(params.To),
		CursorTs: cursorAt,
		CursorID: cursorID,
		PageSize: int32(limit) + 1,
	})
	if err != nil {
		writeInternal(ctx, w, "listing rejected events", err, deadLetterDetail)
		return
	}

	rows, more := trimPage(rows, limit)
	items := make([]DeadLetteredEvent, len(rows))
	for i, row := range rows {
		if items[i], err = deadLetteredEventOf(row); err != nil {
			writeInternal(ctx, w, "decoding a dead-lettered item", err, deadLetterDetail)
			return
		}
	}

	list := DeadLetterList{Items: items}
	if more {
		// The cursor names the last item served, so the next page starts at the
		// row after it. RFC3339Nano carries the microseconds a TIMESTAMPTZ holds,
		// which is what makes the round trip exact.
		last := items[len(items)-1]
		cursor := encodeCursor([]string{last.ReceivedAt.Format(time.RFC3339Nano), last.Id.String()})
		list.NextCursor = &cursor
	}
	writeJSON(w, list)
}

// deadLetterCursor reads the position a cursor resumes from into the two bounds
// the query compares its sort key against.
//
// Both keys are parsed rather than passed through, so a cursor whose instant is
// not an instant and one whose id is not a UUID are refused here instead of
// reaching the database as casts that fail there.
func deadLetterCursor(cursor string) (pgtype.Timestamptz, pgtype.UUID, error) {
	keys, err := decodeCursor(cursor, deadLetterCursorKeys)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, keys[0])
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{},
			fmt.Errorf("the timestamp of the cursor is not RFC 3339: %w", err)
	}
	id, err := uuid.Parse(keys[1])
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{},
			fmt.Errorf("the id of the cursor is not a UUID: %w", err)
	}
	return pgtype.Timestamptz{Time: at, Valid: true}, pgtype.UUID{Bytes: id, Valid: true}, nil
}

// deadLetteredEventOf renders one rejected_events row as the answer the contract
// promises. The instant comes out in UTC, the zone this API states them in, and
// the raw item is decoded rather than passed through, because the contract
// constrains it to nothing and a collector may have submitted anything.
func deadLetteredEventOf(row sqlcgen.RejectedEvent) (DeadLetteredEvent, error) {
	item := DeadLetteredEvent{
		Id:         row.ID,
		ReceivedAt: row.ReceivedAt.Time.UTC(),
		Reason:     row.Reason,
	}
	if err := json.Unmarshal(row.Raw, &item.Raw); err != nil {
		return DeadLetteredEvent{}, fmt.Errorf("decoding the raw item of %s: %w", row.ID, err)
	}
	return item, nil
}

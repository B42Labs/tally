package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// eventCursorKeys is how many parts the sort key of this list has: the timestamp
// and the event id, in the order ORDER BY names them.
const eventCursorKeys = 2

// eventsDetail answers every failure of this route a caller can do nothing
// about.
const eventsDetail = "the events could not be read"

// ListEvents answers one page of the stored events, narrowed by the filters the
// request carries. The page is read one row longer than it is served, which is
// what trimPage turns into the cursor decision.
//
// The list is event-scoped. A project token reads every event whose project_id
// names one of its projects, which includes the events a resource carried before
// it was transferred away, and the pair filter is what keeps a project id
// another cloud uses for something else out of the answer.
func (s *server) ListEvents(w http.ResponseWriter, r *http.Request, params ListEventsParams) {
	ctx := r.Context()

	scope, ok := s.queryScope(w, r)
	if !ok {
		return
	}
	// A filtered scope holding no project reaches no event at all, so the empty
	// page is answered here rather than asked of the database.
	if !scope.Unfiltered && len(scope.Refs) == 0 {
		writeJSON(w, EventList{Items: []StoredEvent{}})
		return
	}
	// A project the token does not hold is refused rather than answered with an
	// empty page, so that asking outside the scope is distinguishable from a
	// project that has no events yet.
	if params.ProjectId != nil && !scope.Unfiltered && !reachesProject(scope, *params.ProjectId) {
		problem.Write(w, http.StatusForbidden, problem.TypeForbidden,
			"Forbidden", "this token does not reach the project this query names")
		return
	}

	var cursorAt pgtype.Timestamptz
	var cursorEventID pgtype.Text
	if params.Cursor != nil {
		var err error
		if cursorAt, cursorEventID, err = eventCursor(*params.Cursor); err != nil {
			refuseCursor(ctx, w, err)
			return
		}
	}

	limit := pageLimit(params.Limit)
	clouds, projects := scopeFilter(scope)

	// A from later than to yields nothing, which is what the half-open window
	// means and needs no case of its own: each bound is a valid instant on its
	// own, and the contract cannot express a rule spanning two parameters.
	rows, err := s.queries.ListEvents(ctx, sqlcgen.ListEventsParams{
		Cloud:         filterText(params.Cloud),
		Platform:      filterText(params.Platform),
		ProjectID:     filterText(params.ProjectId),
		ResourceType:  filterText(params.ResourceType),
		EventType:     filterText(params.EventType),
		Source:        filterText((*string)(params.Source)),
		FromTs:        filterInstant(params.From),
		ToTs:          filterInstant(params.To),
		ScopeClouds:   clouds,
		ScopeProjects: projects,
		CursorTs:      cursorAt,
		CursorEventID: cursorEventID,
		PageSize:      int32(limit) + 1,
	})
	if err != nil {
		writeInternal(ctx, w, "listing events", err, eventsDetail)
		return
	}

	rows, more := trimPage(rows, limit)
	items := make([]StoredEvent, len(rows))
	for i, row := range rows {
		if items[i], err = storedEventOf(row); err != nil {
			writeInternal(ctx, w, "decoding a stored payload", err, eventsDetail)
			return
		}
	}

	list := EventList{Items: items}
	if more {
		// The cursor names the last item served, so the next page starts at the
		// row after it. RFC3339Nano carries the microseconds a TIMESTAMPTZ holds,
		// which is what makes the round trip exact.
		last := items[len(items)-1]
		cursor := encodeCursor([]string{last.Timestamp.Format(time.RFC3339Nano), last.EventId})
		list.NextCursor = &cursor
	}
	writeJSON(w, list)
}

// eventCursor reads the position a cursor resumes from into the two bounds the
// query compares its sort key against.
//
// The timestamp is parsed rather than passed through, so a cursor whose first
// key is not an instant is refused here instead of reaching the database as a
// cast that fails there.
func eventCursor(cursor string) (pgtype.Timestamptz, pgtype.Text, error) {
	keys, err := decodeCursor(cursor, eventCursorKeys)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.Text{}, err
	}
	at, err := time.Parse(time.RFC3339Nano, keys[0])
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.Text{},
			fmt.Errorf("the timestamp of the cursor is not RFC 3339: %w", err)
	}
	return pgtype.Timestamptz{Time: at, Valid: true}, pgtype.Text{String: keys[1], Valid: true}, nil
}

// storedEventOf renders one events row as the answer the contract promises. The
// timestamps come out in UTC, which is the zone the API states them in, and the
// payload is decoded rather than passed through, because the contract types it
// as an object and a row written past this API could hold anything.
//
// A row whose payload column is NULL renders as a null member: no envelope was
// stored, and an empty object would claim one was.
func storedEventOf(row sqlcgen.Event) (StoredEvent, error) {
	item := StoredEvent{
		EventId:      row.EventID,
		Timestamp:    row.Timestamp.Time.UTC(),
		EventType:    row.EventType,
		Platform:     row.Platform,
		Cloud:        row.Cloud,
		ResourceType: row.ResourceType,
		ResourceId:   row.ResourceID,
		ProjectId:    row.ProjectID,
		Source:       row.Source,
		ReceivedAt:   row.ReceivedAt.Time.UTC(),
	}
	if len(row.Payload) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(row.Payload, &payload); err != nil {
			return StoredEvent{}, fmt.Errorf("decoding the payload of event %s: %w", row.EventID, err)
		}
		item.Payload = &payload
	}
	return item, nil
}

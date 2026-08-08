package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// defaultPageSize is how long a page is when the request names no limit. The
// contract declares the same number, and bounds the parameter, so a limit that
// reaches this handler is either absent or between 1 and 1000.
const defaultPageSize = 100

// eventCursorKeys is how many parts the sort key of this list has: the timestamp
// and the event id, in the order ORDER BY names them.
const eventCursorKeys = 2

// badCursorDetail answers every cursor this API cannot use. Which part of it was
// wrong goes to the log rather than to the client: a cursor is opaque, so a
// caller has nothing to fix inside one and can only walk the list again.
const badCursorDetail = "the cursor is not one this API issued"

// errNoPrincipal is what a query route reports when the request context carries
// no principal. Every route the dispatch table puts behind the query guard has
// one, in enforced mode from the token and in disabled mode synthetically, so
// finding none is this service disagreeing with itself.
var errNoPrincipal = errors.New("the request context carries no query principal")

// ListEvents answers one page of the stored events, narrowed by the filters the
// request carries.
//
// The page is read one row longer than it is served, and that extra row is what
// decides whether a cursor is sent: it tells "the page is full" from "there is
// more" without a second query counting the rest.
//
// The list is event-scoped. A project token reads every event whose project_id
// names one of its projects, which includes the events a resource carried before
// it was transferred away, and the pair filter is what keeps a project id
// another cloud uses for something else out of the answer.
func (s *server) ListEvents(w http.ResponseWriter, r *http.Request, params ListEventsParams) {
	ctx := r.Context()

	principal, ok := auth.QueryFromContext(ctx)
	if !ok {
		writeInternal(ctx, w, "serving a query route without a principal", errNoPrincipal,
			"the request could not be served")
		return
	}

	scope, err := auth.ResolveProjectScope(ctx, s.queries, principal)
	if err != nil {
		writeInternal(ctx, w, "resolving the project scope", err,
			"the request could not be served")
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
		if cursorAt, cursorEventID, err = eventCursor(*params.Cursor); err != nil {
			Logger(ctx).Warn("refusing a cursor", "error", err)
			problem.Write(w, http.StatusBadRequest, problem.TypeValidation,
				"Validation failed", badCursorDetail)
			return
		}
	}

	limit := defaultPageSize
	if params.Limit != nil {
		limit = *params.Limit
	}
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
		writeInternal(ctx, w, "listing events", err, "the events could not be read")
		return
	}

	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	items := make([]StoredEvent, len(rows))
	for i, row := range rows {
		if items[i], err = storedEventOf(row); err != nil {
			writeInternal(ctx, w, "decoding a stored payload", err,
				"the events could not be read")
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

// scopeFilter zips the projects a filtered principal reads into the two parallel
// arrays the query pairs positionally, so that clouds[i] belongs to
// projects[i]. Pairing them is what keeps a project id one cloud uses from
// matching the rows another cloud stores under the same id.
//
// An unfiltered principal yields two nil slices, which pgx sends as NULL and the
// query reads as no scope filter at all.
func scopeFilter(scope auth.Scope) (clouds, projects []string) {
	if scope.Unfiltered {
		return nil, nil
	}
	clouds = make([]string, len(scope.Refs))
	projects = make([]string, len(scope.Refs))
	for i, ref := range scope.Refs {
		clouds[i] = ref.Cloud
		projects[i] = ref.ExternalID
	}
	return clouds, projects
}

// reachesProject reports whether a filtered scope holds the project a request
// asked for by name. The cloud is left out of the comparison: the pair filter of
// the query narrows the answer to the cloud the token holds the project in
// anyway, so a matching external id is enough to let the request through.
func reachesProject(scope auth.Scope, externalID string) bool {
	return slices.ContainsFunc(scope.Refs, func(ref auth.ProjectRef) bool {
		return ref.ExternalID == externalID
	})
}

// filterText maps one optional string filter onto its query parameter. A filter
// the request left out becomes NULL, which the query reads as every value.
func filterText(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *value, Valid: true}
}

// filterInstant maps one bound of the time window onto its query parameter.
func filterInstant(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *value, Valid: true}
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

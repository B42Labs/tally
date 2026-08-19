package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/timeline"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/projection"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// resourceCursorKeys is how many parts the sort key of the resource list has:
// the three columns of the projection's primary key, in the order ORDER BY
// names them.
const resourceCursorKeys = 3

// unknownResourceDetail answers every read of a resource this token has no
// answer for. A resource that does not exist and one the scope does not reach
// are told apart nowhere in the response, so a caller cannot map the fleet of
// another project by asking for its resources one by one.
const unknownResourceDetail = "this resource is not known"

// The details that answer a failure of one of these routes a caller can do
// nothing about. Which route failed is what they tell apart, so each is spelled
// once rather than at every writeInternal of that route.
const (
	resourcesDetail = "the resources could not be read"
	historyDetail   = "the resource history could not be read"
	lifecycleDetail = "the lifecycle could not be read"
)

// maxResourceHistory is how many events the two per-resource reads answer at
// most. Neither is paginated, so without a bound the memory one request costs is
// set by how many events a collector has written for that resource, which is a
// number this service does not control: a caller could hold the whole history of
// a long-lived resource resident, and concurrent requests for it multiply that.
//
// The bound counts rows, not bytes. What one event's payload may weigh is the
// ingest path's to say, and it does not say it today, so this bounds how many
// payloads a request holds rather than how much they add up to.
//
// A resource above the bound is refused rather than answered short, so a client
// never mistakes a truncated history for a whole one. It is far above any real
// history: a resource changing every five minutes reaches it in about a month.
const maxResourceHistory = 10000

// longHistoryDetail answers a resource whose stored history is longer than the
// unpaginated reads serve at once. It names the bound, because a caller that
// walks the event list instead needs to know what it ran into.
const longHistoryDetail = "this resource has more stored events than the unpaginated per-resource reads serve; " +
	"read them through GET /api/v1/events instead"

// ListResources answers one page of the projection rows, narrowed by the filters
// the request carries. The page is read one row longer than it is served, which
// is what trimPage turns into the cursor decision.
//
// A project token reads the rows whose (cloud, project_id) pair one of its
// projects names, which is the pair the projection row carries today rather than
// the one an older event of the resource did.
func (s *server) ListResources(w http.ResponseWriter, r *http.Request, params ListResourcesParams) {
	ctx := r.Context()

	scope, ok := s.queryScope(w, r)
	if !ok {
		return
	}
	// A filtered scope holding no project reaches no resource at all, so the
	// empty page is answered here rather than asked of the database.
	if !scope.Unfiltered && len(scope.Refs) == 0 {
		writeJSON(w, ResourceList{Items: []Resource{}})
		return
	}
	// A project the token does not hold is refused rather than answered with an
	// empty page, so that asking outside the scope is distinguishable from a
	// project that has no resources yet.
	if params.ProjectId != nil && !scope.Unfiltered && !reachesProject(scope, *params.ProjectId) {
		problem.Write(w, http.StatusForbidden, problem.TypeForbidden,
			"Forbidden", "this token does not reach the project this query names")
		return
	}

	var cursorCloud, cursorType, cursorID pgtype.Text
	if params.Cursor != nil {
		keys, err := decodeCursor(*params.Cursor, resourceCursorKeys)
		if err != nil {
			refuseCursor(ctx, w, err)
			return
		}
		cursorCloud = pgtype.Text{String: keys[0], Valid: true}
		cursorType = pgtype.Text{String: keys[1], Valid: true}
		cursorID = pgtype.Text{String: keys[2], Valid: true}
	}

	limit := pageLimit(params.Limit)
	clouds, projects := scopeFilter(scope)

	// state and status filter independently, so a request naming a live state
	// together with status=deleted is served the empty page rather than refused:
	// each parameter is valid on its own, and the contract cannot express a rule
	// spanning two of them.
	rows, err := s.queries.ListCurrentResources(ctx, sqlcgen.ListCurrentResourcesParams{
		Cloud:              filterText(params.Cloud),
		Platform:           filterText(params.Platform),
		ProjectID:          filterText(params.ProjectId),
		ResourceType:       filterText(params.ResourceType),
		State:              filterText(params.State),
		Deleted:            filterStatus(params.Status),
		ScopeClouds:        clouds,
		ScopeProjects:      projects,
		CursorCloud:        cursorCloud,
		CursorResourceType: cursorType,
		CursorResourceID:   cursorID,
		PageSize:           int32(limit) + 1,
	})
	if err != nil {
		writeInternal(ctx, w, "listing resources", err, resourcesDetail)
		return
	}

	rows, more := trimPage(rows, limit)
	items := make([]Resource, len(rows))
	for i, row := range rows {
		if items[i], err = resourceOf(row); err != nil {
			writeInternal(ctx, w, "decoding a stored resource", err, resourcesDetail)
			return
		}
	}

	list := ResourceList{Items: items}
	if more {
		// The cursor names the last item served, so the next page starts at the
		// row after it. The three keys are the columns themselves, so nothing
		// has to be formatted for the round trip.
		last := items[len(items)-1]
		cursor := encodeCursor([]string{last.Cloud, last.ResourceType, last.ResourceId})
		list.NextCursor = &cursor
	}
	writeJSON(w, list)
}

// ListResourceEvents answers with the history of one resource the request may
// see, in the order the fold reads a history in.
//
// The answer is never paginated: the events of one resource are what a client
// asks for here, and cutting them into pages would make the caller reassemble
// the history before it can do anything with it. A history above
// maxResourceHistory is refused instead, which is what keeps the answer bounded.
func (s *server) ListResourceEvents(w http.ResponseWriter, r *http.Request, cloud, resourceType, resourceID string) {
	ctx := r.Context()

	scope, ok := s.queryScope(w, r)
	if !ok {
		return
	}
	if _, ok = s.loadScopedResource(w, r, scope, cloud, resourceType, resourceID); !ok {
		return
	}

	rows, ok := s.resourceHistory(w, r, scope, cloud, resourceType, resourceID, historyDetail)
	if !ok {
		return
	}

	var err error
	items := make([]StoredEvent, len(rows))
	for i, row := range rows {
		if items[i], err = storedEventOf(row); err != nil {
			writeInternal(ctx, w, "decoding a stored payload", err, historyDetail)
			return
		}
	}
	// The cursor stays null: one answer carries the whole history, so there is
	// never a page after this one.
	writeJSON(w, EventList{Items: items})
}

// GetResourceLifecycle answers with one resource, the history the request may
// see, and the billable intervals that history folds into.
//
// The fold is internal/core/timeline, the one the projection replay runs, so
// what this endpoint reports about a resource and what the projection row holds
// are derived from the same reading of the same events. The replay folds every
// event; a project token folds its own, so the intervals it reads are the spans
// it is billed for and not the ones another project was.
func (s *server) GetResourceLifecycle(w http.ResponseWriter, r *http.Request, cloud, resourceType, resourceID string) {
	ctx := r.Context()

	scope, ok := s.queryScope(w, r)
	if !ok {
		return
	}
	row, ok := s.loadScopedResource(w, r, scope, cloud, resourceType, resourceID)
	if !ok {
		return
	}
	resource, err := resourceOf(row)
	if err != nil {
		writeInternal(ctx, w, "decoding a stored resource", err, lifecycleDetail)
		return
	}

	rows, ok := s.resourceHistory(w, r, scope, cloud, resourceType, resourceID, lifecycleDetail)
	if !ok {
		return
	}

	// Each row is read twice: once as the event the answer carries, and once as
	// the value the fold works on, which decodes the payload into the envelope
	// instead of into a free document.
	events := make([]StoredEvent, len(rows))
	history := make([]event.Stored, len(rows))
	for i, row := range rows {
		if events[i], err = storedEventOf(row); err != nil {
			writeInternal(ctx, w, "decoding a stored payload", err, lifecycleDetail)
			return
		}
		if history[i], err = projection.Decode(row); err != nil {
			writeInternal(ctx, w, "decoding a stored payload", err, lifecycleDetail)
			return
		}
	}

	folded := timeline.Build(history)
	// The three arrays are answered as arrays even when they are empty, which a
	// history without a single billable interval leaves the middle one: a client
	// iterates them without a nil check, and null would break that.
	intervals := make([]LifecycleInterval, len(folded.Intervals))
	for i, interval := range folded.Intervals {
		intervals[i] = lifecycleIntervalOf(interval)
	}
	warnings := folded.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, Lifecycle{
		Resource:  resource,
		Events:    events,
		Intervals: intervals,
		Warnings:  warnings,
	})
}

// resourceHistory reads the events of one resource the request may see, and
// answers the request itself when it has no history to serve.
//
// The scope runs per event rather than per resource: the gate in front of this
// says whether the resource is the token's today, and this says which of its
// events are. The two differ exactly once a resource has been transferred, and
// then the events of the project the resource left are the old project's alone.
// A principal reading unfiltered passes no scope and reads every event there is.
//
// The read is bounded, and a resource above the bound is refused rather than
// answered short, because neither route paginates.
//
// The bound is decided by counting first. The row set of the read below is
// materialized whole before the caller sees any of it, so a refusal taken off it
// would have cost every payload the refusal exists to avoid holding. The count
// runs under the same scope, so what decides the refusal is the history this
// request would be served rather than the one the resource has, and it stops one
// row past the bound, so a history far above it costs the same as one just above
// it rather than growing with how far above it is.
//
// The read is still asked for one row past the bound, and the answer is refused
// if it comes back: an event stored between the two queries would otherwise be
// served as a history one event short of the whole one.
func (s *server) resourceHistory(w http.ResponseWriter, r *http.Request, scope auth.Scope,
	cloud, resourceType, resourceID, detail string,
) ([]sqlcgen.Event, bool) {
	ctx := r.Context()

	clouds, projects := scopeFilter(scope)
	// The count saturates at the row that decides the refusal, so what comes back
	// answers whether the history is too long and not how long it is.
	count, err := s.queries.CountEventsForResource(ctx, sqlcgen.CountEventsForResourceParams{
		Cloud:         cloud,
		ResourceType:  resourceType,
		ResourceID:    resourceID,
		ScopeClouds:   clouds,
		ScopeProjects: projects,
		ProbeLimit:    maxResourceHistory + 1,
	})
	if err != nil {
		writeInternal(ctx, w, "counting the resource history", err, detail)
		return nil, false
	}
	if count > maxResourceHistory {
		refuseLongHistory(ctx, w, cloud, resourceType, resourceID)
		return nil, false
	}

	rows, err := s.queries.ListEventsForResource(ctx, sqlcgen.ListEventsForResourceParams{
		Cloud:         cloud,
		ResourceType:  resourceType,
		ResourceID:    resourceID,
		ScopeClouds:   clouds,
		ScopeProjects: projects,
		PageSize:      pgtype.Int4{Int32: maxResourceHistory + 1, Valid: true},
	})
	if err != nil {
		writeInternal(ctx, w, "listing the resource history", err, detail)
		return nil, false
	}
	if len(rows) > maxResourceHistory {
		refuseLongHistory(ctx, w, cloud, resourceType, resourceID)
		return nil, false
	}
	return rows, true
}

// refuseLongHistory answers a request for a history the unpaginated reads do not
// serve, and logs the resource it was about: the bound being hit at all is an
// operational signal, and the response says nothing about which resource hit it.
func refuseLongHistory(ctx context.Context, w http.ResponseWriter, cloud, resourceType, resourceID string) {
	Logger(ctx).Warn("refusing a resource history above the bound",
		"cloud", cloud, "resource_type", resourceType, "resource_id", resourceID,
		"bound", maxResourceHistory)
	problem.Write(w, http.StatusUnprocessableEntity, problem.TypeHistoryTooLong,
		"History too long", longHistoryDetail)
}

// loadScopedResource reads the projection row a per-resource read is about and
// reports whether the request may see it. A request it has no answer for is
// answered here, and the caller returns.
//
// The row's own (cloud, project_id) pair is what the scope is checked against,
// which is the pair the resource carries today: a transferred resource follows
// its new project, and the old project stops reading it here. What the new
// project then reads is not the whole history but its own part of it, which
// resourceHistory is what decides.
//
// A resource without a row and a resource outside the scope leave through the
// same problem.Write, so the two responses are identical byte for byte and a
// caller learns nothing about the resources of a project it does not hold.
func (s *server) loadScopedResource(w http.ResponseWriter, r *http.Request, scope auth.Scope,
	cloud, resourceType, resourceID string,
) (sqlcgen.CurrentResource, bool) {
	ctx := r.Context()

	row, err := s.queries.GetCurrentResource(ctx, sqlcgen.GetCurrentResourceParams{
		Cloud:        cloud,
		ResourceType: resourceType,
		ResourceID:   resourceID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		writeInternal(ctx, w, "loading the resource", err, "the resource could not be read")
		return sqlcgen.CurrentResource{}, false
	case scope.Unfiltered || reachesPair(scope, row.Cloud, row.ProjectID):
		return row, true
	}

	problem.Write(w, http.StatusNotFound, problem.TypeNotFound, "Not found", unknownResourceDetail)
	return sqlcgen.CurrentResource{}, false
}

// filterStatus maps the status parameter onto the one boolean the query splits
// the fleet with: false serves the rows that live, true the deleted ones, and
// NULL both.
//
// A request naming no status is served the active rows. The contract's default
// says so, and the generated binding does not apply it, so it is applied here.
func filterStatus(status *ListResourcesParamsStatus) pgtype.Bool {
	value := ListResourcesParamsStatusActive
	if status != nil {
		value = *status
	}
	if value == ListResourcesParamsStatusAll {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: value == ListResourcesParamsStatusDeleted, Valid: true}
}

// resourceOf renders one projection row as the answer the contract promises. The
// instants come out in UTC, the zone this API states them in, and the two JSON
// columns are decoded rather than passed through, because the contract types
// them as objects and a row written past this API could hold anything.
//
// created_at stays null for a history that never showed a create and deleted_at
// while the resource lives, so both members are set only when their column holds
// an instant. A row whose last_payload column is NULL renders as a null member:
// no envelope was stored, and an empty object would claim one was.
func resourceOf(row sqlcgen.CurrentResource) (Resource, error) {
	var size map[string]any
	if err := json.Unmarshal(row.Size, &size); err != nil {
		return Resource{}, fmt.Errorf("decoding the size of %s/%s/%s: %w",
			row.Cloud, row.ResourceType, row.ResourceID, err)
	}

	item := Resource{
		Cloud:         row.Cloud,
		Platform:      row.Platform,
		ResourceType:  row.ResourceType,
		ResourceId:    row.ResourceID,
		ProjectId:     row.ProjectID,
		State:         row.State,
		Size:          size,
		LastEventType: row.LastEventType,
		LastEventAt:   row.LastEventAt.Time.UTC(),
	}
	if row.CreatedAt.Valid {
		at := row.CreatedAt.Time.UTC()
		item.CreatedAt = &at
	}
	if row.DeletedAt.Valid {
		at := row.DeletedAt.Time.UTC()
		item.DeletedAt = &at
	}
	if len(row.LastPayload) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(row.LastPayload, &payload); err != nil {
			return Resource{}, fmt.Errorf("decoding the last payload of %s/%s/%s: %w",
				row.Cloud, row.ResourceType, row.ResourceID, err)
		}
		item.LastPayload = &payload
	}
	return item, nil
}

// lifecycleIntervalOf renders one folded interval as the answer the contract
// promises: the instants in UTC, and the open end as a null `to`.
//
// A size the history never reported leaves the interval's size nil, which the
// contract types as an object, so the empty one stands for it the way the
// projection's size column does.
func lifecycleIntervalOf(interval timeline.Interval) LifecycleInterval {
	item := LifecycleInterval{
		From:      interval.Start.UTC(),
		State:     interval.State,
		Size:      interval.Size,
		ProjectId: interval.ProjectID,
	}
	if item.Size == nil {
		item.Size = map[string]any{}
	}
	if interval.End != nil {
		end := interval.End.UTC()
		item.To = &end
	}
	return item
}

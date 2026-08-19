package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/projection"
	"github.com/b42labs/tally/internal/reporting/stats"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// summaryDetail answers every failure of this route a caller can do nothing
// about.
const summaryDetail = "the project summary could not be read"

// maxSummaryEvents is how many events one summary folds. The answer is not
// paginated, and folding a history means holding it: the memory one request
// costs is set by how many events the collectors wrote for the resources of the
// project, which is a number this service does not control.
//
// A project above the bound is refused rather than summarized from part of its
// history, so a client never mistakes a partial fold for a whole one. The Phase
// 3 usage records are what answer a history that long, because they carry the
// folded numbers instead of the events they come from.
const maxSummaryEvents = 100000

// longProjectHistoryDetail answers a project whose stored history is longer than
// one summary folds. It names where the answer comes from instead, because there
// is nothing about the request itself the caller can change.
const longProjectHistoryDetail = "this project has more stored events than one summary folds at once; " +
	"the Phase 3 usage records answer a history this long"

// GetProjectSummary answers with one project and what each of its resource types
// did inside the window.
//
// The window numbers come from folding the histories of the project's resources
// with internal/core/timeline, the fold every other read of a history runs, so
// what the summary reports and what a lifecycle reports are derived from one
// reading of the same events. A resource that changed hands is folded whole and
// credited per interval, so its minutes stop here at the transfer rather than
// running on beside the new owner's. `active_now` is counted off the projection
// instead: it says what the project runs today, which no window can answer.
//
// A from at or past to is an empty window and needs no case of its own: the fold
// clips every interval to it, so the created, deleted, and minute counts come out
// zero while active_now stays what it is.
//
// The answer names the project and does not carry the registry row: the guard on
// this route is the one the queries take rather than the one the registry takes,
// so what a project scope is built out of is not served through it.
func (s *server) GetProjectSummary(w http.ResponseWriter, r *http.Request, id Uuid, params GetProjectSummaryParams) {
	ctx := r.Context()

	scope, ok := s.queryScope(w, r)
	if !ok {
		return
	}

	project, err := s.queries.GetProject(ctx, id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeInternal(ctx, w, "reading a project", err, summaryDetail)
		return
	}
	// A project this registry does not hold and a project outside the token's
	// scope leave through the same write, so the two responses are identical byte
	// for byte and a token holder cannot tell a project it may not read from an
	// id that names nothing.
	if err != nil || (!scope.Unfiltered && !reachesPair(scope, project.Cloud, project.ExternalID)) {
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound, "Not found", unknownProjectDetail)
		return
	}

	rows, ok := s.projectHistory(w, r, project, params.To)
	if !ok {
		return
	}

	// One resource is one history, so the events are split per resource before
	// anything folds them. The query's order is kept inside each group, which is
	// the order the fold reads a history in.
	histories := make(map[projection.Key][]event.Stored)
	for _, row := range rows {
		key := projection.Key{Cloud: row.Cloud, ResourceType: row.ResourceType, ResourceID: row.ResourceID}
		histories[key] = append(histories[key], foldEvent(row))
	}

	folded := make([]stats.History, 0, len(histories))
	for key, events := range histories {
		folded = append(folded, stats.History{ResourceType: key.ResourceType, Events: events})
	}
	activities := stats.Summarize(folded, project.ExternalID, params.From, params.To, s.now())

	active, err := s.queries.CountProjectResourcesByType(ctx, sqlcgen.CountProjectResourcesByTypeParams{
		Cloud:     project.Cloud,
		ProjectID: project.ExternalID,
	})
	if err != nil {
		writeInternal(ctx, w, "counting the resources of a project", err, summaryDetail)
		return
	}

	// The project is named rather than served: this route is reachable with a
	// project token and the registry row it hangs off is not, so what a project
	// scope is built out of — the name and the operator-set metadata — stays
	// behind the read_all the registry reads take.
	writeJSON(w, ProjectSummary{
		Project: ProjectRef{
			Id:         project.ID,
			Cloud:      project.Cloud,
			ExternalId: project.ExternalID,
		},
		ResourceTypes: mergeActivity(activities, active),
	})
}

// projectHistory reads the events one summary folds, and answers the request
// itself when it has no summary to serve.
//
// The read is bounded, and a project above the bound is refused rather than
// summarized short, because the route does not paginate. The bound is decided by
// counting first, for the reason the per-resource reads count first: the row set
// of the read is materialized and sorted whole before the caller sees any of it,
// so deciding the refusal off the rows themselves costs exactly the read the
// refusal exists to avoid. The count probes the joined set the read returns
// rather than the project's own events, which bound nothing about it: a resource
// the project holds a single event of pulls in the events of every project it
// was transferred between. The count stops one row past the bound, so a set far
// above it costs the same as one just above it.
//
// The read is still asked for one row past the bound, and the answer is refused
// if it comes back: an event stored between the two queries would otherwise be
// folded as a history one event short of the whole one.
//
// Only the upper bound of the window filters the read. The events before it are
// what say which resources the project was already running when the window
// opened, and the fold clips its intervals to the window itself.
func (s *server) projectHistory(w http.ResponseWriter, r *http.Request,
	project sqlcgen.Project, to time.Time,
) ([]sqlcgen.ListProjectFoldEventsRow, bool) {
	ctx := r.Context()

	count, err := s.queries.CountProjectFoldEvents(ctx, sqlcgen.CountProjectFoldEventsParams{
		Cloud:      project.Cloud,
		ProjectID:  project.ExternalID,
		ToTs:       pgtype.Timestamptz{Time: to, Valid: true},
		ProbeLimit: maxSummaryEvents + 1,
	})
	if err != nil {
		writeInternal(ctx, w, "counting the history of a project", err, summaryDetail)
		return nil, false
	}
	if count > maxSummaryEvents {
		refuseLongProjectHistory(ctx, w, project, to)
		return nil, false
	}

	rows, err := s.queries.ListProjectFoldEvents(ctx, sqlcgen.ListProjectFoldEventsParams{
		Cloud:     project.Cloud,
		ProjectID: project.ExternalID,
		ToTs:      pgtype.Timestamptz{Time: to, Valid: true},
		PageSize:  maxSummaryEvents + 1,
	})
	if err != nil {
		writeInternal(ctx, w, "listing the history of a project", err, summaryDetail)
		return nil, false
	}
	if len(rows) > maxSummaryEvents {
		refuseLongProjectHistory(ctx, w, project, to)
		return nil, false
	}
	return rows, true
}

// foldEvent renders one read row as the stored event the fold reads. The read
// carries no payload, because nothing the summary reports is decided by one, so
// this cannot fail the way decoding a stored event can.
func foldEvent(row sqlcgen.ListProjectFoldEventsRow) event.Stored {
	return event.Stored{
		Event: event.Event{
			EventID:      row.EventID,
			Timestamp:    row.Timestamp.Time,
			EventType:    row.EventType,
			Platform:     row.Platform,
			Cloud:        row.Cloud,
			ResourceType: row.ResourceType,
			ResourceID:   row.ResourceID,
			ProjectID:    row.ProjectID,
			Source:       event.Source(row.Source),
		},
		ReceivedAt: row.ReceivedAt.Time,
	}
}

// refuseLongProjectHistory answers a project the summary does not fold, and logs
// which project it was about: the bound being hit at all is an operational
// signal, and the response says nothing about the project that hit it.
func refuseLongProjectHistory(ctx context.Context, w http.ResponseWriter,
	project sqlcgen.Project, to time.Time,
) {
	Logger(ctx).Warn("refusing a project history above the bound",
		"cloud", project.Cloud, "project_id", project.ExternalID, "to", to,
		"bound", maxSummaryEvents)
	problem.Write(w, http.StatusUnprocessableEntity, problem.TypeHistoryTooLong,
		"History too long", longProjectHistoryDetail)
}

// mergeActivity joins what the window folded with what the project runs today.
// Neither side contains the other: a resource type the window saw nothing of is
// still running, and one the project has since given up still spent minutes
// inside the window. The union of both is what the summary reports, zero-filled
// on whichever side has no row for a type.
//
// The rows come out ordered by resource type, and the slice is never nil: a
// project with neither events nor resources is answered as the empty array.
func mergeActivity(activities []stats.Activity,
	active []sqlcgen.CountProjectResourcesByTypeRow,
) []ProjectActivity {
	byType := make(map[string]ProjectActivity, len(activities)+len(active))
	for _, activity := range activities {
		byType[activity.ResourceType] = ProjectActivity{
			ResourceType: activity.ResourceType,
			Created:      activity.Created,
			Deleted:      activity.Deleted,
			TotalMinutes: activity.TotalMinutes,
		}
	}
	for _, row := range active {
		item := byType[row.ResourceType]
		item.ResourceType = row.ResourceType
		item.ActiveNow = int(row.Resources)
		byType[row.ResourceType] = item
	}

	items := make([]ProjectActivity, 0, len(byType))
	for _, item := range byType {
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b ProjectActivity) int {
		return strings.Compare(a.ResourceType, b.ResourceType)
	})
	return items
}

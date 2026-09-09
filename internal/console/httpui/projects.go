package httpui

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/reporting"
	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/engine/statements"
	"github.com/b42labs/tally/internal/reporting/httpapi"
)

// projectsData is one page of the registry with the link to the next one.
type projectsData struct {
	Items    listing[projectRow]
	NextLink string
}

// projectColumns is the registry listing.
var projectColumns = []column[projectRow]{
	textCol("cloud", func(r projectRow) string { return r.Project.Cloud }),
	textCol("external id", func(r projectRow) string { return r.Project.ExternalId }),
	textCol("name", func(r projectRow) string { return optString(r.Project.Name, r.Project.ExternalId) }),
	textCol("platform", func(r projectRow) string { return r.Project.Platform }),
	textCol("registered", func(r projectRow) string { return stamp(r.Project.CreatedAt) }),
}

// projectRow is one registered project and its own page.
type projectRow struct {
	Project httpapi.Project
	Link    string
}

// projectData is one project from both sides: what the registry holds, what it
// ran over the window the viewer reads, and what every run billed it.
type projectData struct {
	Project    httpapi.Project
	Relations  listing[relationRow]
	Related    listing[relatedRow]
	Activity   listing[httpapi.ProjectActivity]
	Window     windowView
	From       time.Time
	To         time.Time
	Key        string
	Statements listing[projectStatementRow]
}

// windowView is the control above a table that reads one window: the two
// inputs with the hidden fields their form carries the page through, and the
// spans one click away. It is the window half of the fleet controls, drawn the
// same way, for a page whose numbers are a window and nothing else.
type windowView struct {
	Path    string
	Hidden  []field
	From    string
	To      string
	Presets []choice
}

// The columns of a project page's tables.
var (
	relationColumns = []column[relationRow]{
		textCol("relation type", func(r relationRow) string { return r.Relation.RelationType }),
		textCol("source", func(r relationRow) string { return r.Relation.SourceId.String() }),
		textCol("target", func(r relationRow) string { return r.Relation.TargetId.String() }),
		textCol("valid from", func(r relationRow) string { return stamp(r.Relation.ValidFrom) }),
		textCol("valid to", func(r relationRow) string { return optStamp(r.Relation.ValidTo) }),
	}
	relatedColumns = []column[relatedRow]{
		textCol("project", func(r relatedRow) string {
			return optString(r.Related.Project.Name, r.Related.Project.ExternalId)
		}),
		textCol("cloud", func(r relatedRow) string { return r.Related.Project.Cloud }),
		textCol("relation type", func(r relatedRow) string { return r.Related.RelationType }),
		countCol("depth", func(r relatedRow) int64 { return int64(r.Related.Depth) }),
	}
	activityColumns = []column[httpapi.ProjectActivity]{
		textCol("resource type", func(r httpapi.ProjectActivity) string { return r.ResourceType }),
		countCol("created", func(r httpapi.ProjectActivity) int64 { return int64(r.Created) }),
		countCol("deleted", func(r httpapi.ProjectActivity) int64 { return int64(r.Deleted) }),
		countCol("active now", func(r httpapi.ProjectActivity) int64 { return int64(r.ActiveNow) }),
		countCol("minutes", func(r httpapi.ProjectActivity) int64 { return r.TotalMinutes }),
	}
	projectStatementColumns = []column[projectStatementRow]{
		textCol("period", func(r projectStatementRow) string { return stamp(r.Row.PeriodFrom) }),
		textCol("run kind", func(r projectStatementRow) string { return r.Row.Kind }),
		textCol("run status", func(r projectStatementRow) string { return r.Row.Status }),
		numberCol("total", func(r projectStatementRow) decimal.Decimal { return r.Row.Total }),
	}
)

// relationRow is one relation of a project and the pages of the projects at
// its ends. The listing holds the relations of both directions, so either end
// can be another project, and that end is what the link leads to. The end that
// is the project of the page gets no link: it would lead back to the page it is
// printed on.
type relationRow struct {
	Relation   httpapi.Relation
	SourceLink string
	TargetLink string
}

// relatedRow is one project a traversal reached and its own page.
type relatedRow struct {
	Related httpapi.RelatedProject
	Link    string
}

// projectStatementRow is one month of a project's history and the statement it
// opens.
type projectStatementRow struct {
	Row  store.ProjectStatementRow
	Link string
}

// projects lists one page of the registry. The cursor is followed only when the
// viewer asks for the next page: one request reads one page.
func (h *handlers) projects(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var src sources

	parameters := r.URL.Query()
	query := reporting.ProjectsQuery{
		Platform: parameters.Get("platform"),
		Cloud:    parameters.Get("cloud"),
		Cursor:   parameters.Get("cursor"),
	}

	list, request, err := h.api.ListProjects(ctx, query)
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	rows := make([]projectRow, 0, len(list.Items))
	for _, project := range list.Items {
		rows = append(rows, projectRow{Project: project, Link: link("/project", "id", project.Id.String())})
	}

	data := projectsData{Items: tabulate(r, "projects", projectColumns, rows)}
	if list.NextCursor != nil || query.Cursor != "" {
		data.Items = data.Items.paged()
	}
	if list.NextCursor != nil {
		data.NextLink = nextLink(r, *list.NextCursor)
	}
	h.render(w, r, "projects", page{Title: "Projects", Sources: src, Data: data})
}

// project shows one project. It is addressed by the id the API assigns, or by
// the pair a resource names its project with, cloud and external id, which the
// registry resolves to that id first. The statements are read under the key
// the engine stores them by, which is that same pair.
func (h *handlers) project(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var src sources

	now := h.now()
	from, to, err := readWindow(r, now)
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}

	id, err := h.projectID(ctx, r, &src)
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}

	project, request, err := h.api.GetProject(ctx, id)
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	relations, request, err := h.api.ListProjectRelations(ctx, id)
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	related, request, err := h.api.ListRelatedProjects(ctx, id)
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	summary, request, err := h.api.GetProjectSummary(ctx, id, from, to)
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	key := statements.Key(project.Cloud, project.ExternalId)
	billed, err := h.store.ListStatementsForProject(ctx, key)
	src.query("ListStatementsForProject")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	listed := make([]relationRow, 0, len(relations.Items))
	for _, relation := range relations.Items {
		listed = append(listed, relationRow{
			Relation:   relation,
			SourceLink: otherProjectLink(relation.SourceId, id),
			TargetLink: otherProjectLink(relation.TargetId, id),
		})
	}

	reached := make([]relatedRow, 0, len(related.Items))
	for _, project := range related.Items {
		reached = append(reached, relatedRow{
			Related: project,
			Link:    link("/project", "id", project.Project.Id.String()),
		})
	}

	rows := make([]projectStatementRow, 0, len(billed))
	for _, row := range billed {
		rows = append(rows, projectStatementRow{
			Row:  row,
			Link: link("/statement", "run", row.RunID.String(), "key", key),
		})
	}

	data := projectData{
		Project:    project,
		Relations:  tabulate(r, "relations", relationColumns, listed),
		Related:    tabulate(r, "related", relatedColumns, reached),
		Activity:   tabulate(r, "activity", activityColumns, summary.ResourceTypes),
		Window:     buildWindowView(r, from, to, now),
		From:       from,
		To:         to,
		Key:        key,
		Statements: tabulate(r, "statements", projectStatementColumns, rows),
	}
	h.render(w, r, "project", page{
		Title:   "Project " + optString(project.Name, project.ExternalId),
		Sources: src,
		Data:    data,
	})
}

// readWindow reads the window a project's activity is summarized over: the
// bounds from and to, each in either form an instant is written in, and this
// month when the request names neither. The summary route takes both bounds,
// so a request that carries one and not the other is refused rather than
// answered over half a window it never asked for, and a window that ends no
// later than it starts is refused the way the fleet's is.
func readWindow(r *http.Request, now time.Time) (from, to time.Time, err error) {
	query := r.URL.Query()
	first, second := query.Get(fromParameter), query.Get(toParameter)
	if first == "" && second == "" {
		start := firstOfThisMonthUTC(now)
		return start, start.AddDate(0, 1, 0), nil
	}
	if first == "" {
		return time.Time{}, time.Time{}, missingParameter(fromParameter)
	}
	if second == "" {
		return time.Time{}, time.Time{}, missingParameter(toParameter)
	}

	if from, err = parseInstant(first); err != nil {
		return time.Time{}, time.Time{}, unreadableInstant(fromParameter, err)
	}
	if to, err = parseInstant(second); err != nil {
		return time.Time{}, time.Time{}, unreadableInstant(toParameter, err)
	}
	if !to.After(from) {
		return time.Time{}, time.Time{}, &paramError{name: toParameter, reason: "is not after from"}
	}
	return from, to, nil
}

// buildWindowView lays the window control out for one request. The presets are
// the fleet's spans over the same bounds, so the two pages are read the same
// way, and the form carries the rest of the page through, the project the page
// is about included.
func buildWindowView(r *http.Request, from, to, now time.Time) windowView {
	values := r.URL.Query()
	return windowView{
		Path:    r.URL.Path,
		Hidden:  hiddenFields(values, fromParameter, toParameter),
		From:    from.UTC().Format(atLayout),
		To:      to.UTC().Format(atLayout),
		Presets: spanPresets(r.URL.Path, values, &from, &to, now),
	}
}

// projectID reads which project the page is about: the id parameter when it
// is there, and otherwise the project registered under the cloud and
// external_id parameters, which is how a resource names its project. The pair
// is looked up through the project list filtered by both, and the answer is
// checked for an exact match on each, so a project of another pair never stands
// in. A pair nothing is registered under fails the way an unknown id does.
func (h *handlers) projectID(ctx context.Context, r *http.Request, src *sources) (uuid.UUID, error) {
	query := r.URL.Query()
	if query.Get("id") != "" || query.Get("external_id") == "" {
		return uuidParameter(r, "id")
	}

	cloud, err := stringParameter(r, "cloud")
	if err != nil {
		return uuid.Nil, err
	}
	externalID := query.Get("external_id")

	list, request, err := h.api.ListProjects(ctx, reporting.ProjectsQuery{Cloud: cloud, ExternalID: externalID})
	src.api(request)
	if err != nil {
		return uuid.Nil, apiFailed(err)
	}
	for _, project := range list.Items {
		if project.Cloud == cloud && project.ExternalId == externalID {
			return project.Id, nil
		}
	}
	return uuid.Nil, nothingRegistered(
		fmt.Errorf("no project is registered under the cloud %s and the external id %s", cloud, externalID))
}

// otherProjectLink is the page of one end of a relation, and nothing for the
// end that is the project the page is about.
func otherProjectLink(end, self uuid.UUID) string {
	if end == self {
		return ""
	}
	return link("/project", "id", end.String())
}

// projectLink is the page of the project a resource names, addressed by the
// pair the resource carries rather than by the id the API assigns, which the
// project page resolves. A resource that names no project gets no link.
func projectLink(cloud, externalID string) string {
	if externalID == "" {
		return ""
	}
	return link("/project", "cloud", cloud, "external_id", externalID)
}

// firstOfThisMonthUTC is the start of the month now falls in. The summary
// window is a whole month in UTC, the way a billing period is, so the page does
// not report a month the engine never billed.
func firstOfThisMonthUTC(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
}

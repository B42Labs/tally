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
// ran this month, and what every run billed it.
type projectData struct {
	Project    httpapi.Project
	Relations  listing[relationRow]
	Related    listing[relatedRow]
	Activity   listing[httpapi.ProjectActivity]
	From       time.Time
	To         time.Time
	Key        string
	Statements listing[projectStatementRow]
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

	from := firstOfThisMonthUTC(h.now())
	to := from.AddDate(0, 1, 0)
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

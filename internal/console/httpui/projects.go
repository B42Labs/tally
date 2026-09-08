package httpui

import (
	"net/http"
	"time"

	"github.com/b42labs/tally/internal/console/reporting"
	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/engine/statements"
	"github.com/b42labs/tally/internal/reporting/httpapi"
)

// projectsData is one page of the registry with the link to the next one.
type projectsData struct {
	Items    []projectRow
	NextLink string
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
	Relations  []httpapi.Relation
	Related    []relatedRow
	Activity   []httpapi.ProjectActivity
	From       time.Time
	To         time.Time
	Key        string
	Statements []projectStatementRow
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

	data := projectsData{Items: rows}
	if list.NextCursor != nil {
		data.NextLink = link("/projects",
			"platform", query.Platform, "cloud", query.Cloud, "cursor", *list.NextCursor)
	}
	h.render(w, r, "projects", page{Title: "Projects", Sources: src, Data: data})
}

// project shows one project. The statements are read under the key the engine
// stores them by, which is the pair of cloud and external id rather than the
// id the API addresses the project with.
func (h *handlers) project(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var src sources

	id, err := uuidParameter(r, "id")
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
		Relations:  relations.Items,
		Related:    reached,
		Activity:   summary.ResourceTypes,
		From:       from,
		To:         to,
		Key:        key,
		Statements: rows,
	}
	h.render(w, r, "project", page{
		Title:   "Project " + optString(project.Name, project.ExternalId),
		Sources: src,
		Data:    data,
	})
}

// firstOfThisMonthUTC is the start of the month now falls in. The summary
// window is a whole month in UTC, the way a billing period is, so the page does
// not report a month the engine never billed.
func firstOfThisMonthUTC(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
}

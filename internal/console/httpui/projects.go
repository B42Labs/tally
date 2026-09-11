package httpui

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/reporting"
	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/core/adjustment"
	projectcore "github.com/b42labs/tally/internal/core/project"
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
	Project   httpapi.Project
	Relations listing[relationRow]
	Related   listing[relatedRow]
	Activity  listing[activityRow]
	// Partial is set when the API served one page of the project's resources
	// and held more, so the folds say they are not the whole list.
	Partial bool
	// Kickbacks is what a partner is owed, per period, and is empty for every
	// project that is not one.
	Kickbacks  listing[settlementRow]
	Window     windowView
	From       time.Time
	To         time.Time
	Key        string
	Statements listing[periodRow]
	// Rollup is what the members of a meta-project are billed, per period,
	// and nil for every project that is not one.
	Rollup *rollupView
}

// activityRow is one resource type of a project over the window: what the
// summary counted of it, and the resources of that type the projection holds
// for that window, which the row folds open. Searched is what the filter
// matches the row on, the type and the resources under it.
type activityRow struct {
	Activity  httpapi.ProjectActivity
	Resources []projectResourceRow
	Searched  string
}

// projectResourceRow is one resource under a type, its own page, what its
// created cell prints, and how long it has lived. It is the resources page's
// row without the columns that only make sense beside resources of other
// projects.
type projectResourceRow struct {
	Resource httpapi.Resource
	Link     string
	Created  string
	Lifetime decimal.Decimal
	Lived    bool
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
	// The type column is searched as the type and the adjustments the relation
	// carries, so a filter on kickback finds the relation that grants one. The
	// type leads the text, so the column still sorts by type.
	relationColumns = []column[relationRow]{
		textCol("relation type", func(r relationRow) string { return r.Searched }),
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
	// The type column is searched as the type and the resources folded under
	// it, so a filter finds the type one resource sits in. The type leads the
	// text, so the column still sorts by type.
	activityColumns = []column[activityRow]{
		textCol("resource type", func(r activityRow) string { return r.Searched }),
		countCol("created", func(r activityRow) int64 { return int64(r.Activity.Created) }),
		countCol("deleted", func(r activityRow) int64 { return int64(r.Activity.Deleted) }),
		countCol("active now", func(r activityRow) int64 { return int64(r.Activity.ActiveNow) }),
		countCol("minutes", func(r activityRow) int64 { return r.Activity.TotalMinutes }),
	}
	// A period is searched as the period and every statement under it, so a
	// filter on a kind or a status finds the month that holds one. The period
	// leads the text, so the column still sorts by period.
	projectPeriodColumns = []column[periodRow]{
		textCol("period", func(r periodRow) string { return r.Searched }),
		textCol("status", func(r periodRow) string { return r.Status }),
		countCol("statements", func(r periodRow) int64 { return int64(len(r.Statements)) }),
		numberCol("total", func(r periodRow) decimal.Decimal { return r.Sort }),
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
	// Adjustments is the commercial pricing the relation carries, which the
	// row folds open: a discount the target grants, the kickback it is owed,
	// a surcharge, a group discount. Unreadable holds what the metadata could
	// not be read as, and is empty for the metadata this API answers with.
	Adjustments []adjustment.Adjustment
	Unreadable  string
	Searched    string
}

// relatedRow is one project a traversal reached and its own page.
type relatedRow struct {
	Related httpapi.RelatedProject
	Link    string
}

// settlementRow is one billing period of a partner: what every run of it
// settles for the partner, which the row folds open, and what the partner is
// owed for the period once the runs are added up.
type settlementRow struct {
	From     time.Time
	Records  []store.Kickback
	Totals   []currencyTotal
	Sort     decimal.Decimal
	Status   string
	Searched string
}

// settlementColumns is a partner's settlement, one row per period.
var settlementColumns = []column[settlementRow]{
	textCol("period", func(r settlementRow) string { return r.Searched }),
	textCol("status", func(r settlementRow) string { return r.Status }),
	countCol("records", func(r settlementRow) int64 { return int64(len(r.Records)) }),
	numberCol("kickback", func(r settlementRow) decimal.Decimal { return r.Sort }),
}

// periodRow is one billing period of a project: every statement a run wrote
// for it, which the row folds open, and what the project is charged for the
// period once the statements that stand are added up.
//
// A period accumulates statements: the regular run that billed it, the run
// that replaced that one, and every correction booked against the run that
// closed it. What the project owes for the period is the sum of the ones that
// stand, and reading it off the rows meant knowing which statuses count.
type periodRow struct {
	From       time.Time
	Statements []projectStatementRow
	// Totals is what stands, summed per currency, and Sort is the amount the
	// column is ordered by. A period is billed in one currency, so the sum is
	// usually one amount; a period whose statements disagree prints each one
	// rather than adding them up.
	Totals   []currencyTotal
	Sort     decimal.Decimal
	Status   string
	Searched string
}

// currencyTotal is an amount in the currency it was billed in.
type currencyTotal struct {
	Total    decimal.Decimal
	Currency string
}

// projectStatementRow is one statement of a project, the page it opens, and
// whether it is one of those the period is charged by. A superseded run's
// statement is on the page for the audit it leaves and counts for nothing.
type projectStatementRow struct {
	Row      store.ProjectStatementRow
	Link     string
	Standing bool
}

// standingStatuses are the run statuses whose statements the project is
// charged by, which are the ones an export reads: a run that stands has either
// closed its period or is the completed one of its kind. A superseded run was
// replaced by another of its kind, a failed one billed nothing, and neither is
// counted.
var standingStatuses = []string{"completed", "finalized"}

// regularKind is the run kind that bills a period, as opposed to the
// corrections booked against it afterwards.
const regularKind = "regular"

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

	relations, request, err := h.api.ListProjectRelations(ctx, id, reporting.RelationsQuery{})
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

	// The resources of the project, which the activity rows fold open. A
	// resource names its project by the pair the registry row carries, and the
	// listing holds every status, because a type of the window is mostly
	// resources that are gone by now.
	fleet, request, err := h.api.ListResources(ctx, reporting.ResourcesQuery{
		Cloud:     project.Cloud,
		ProjectID: project.ExternalId,
		Status:    statuses[len(statuses)-1],
		Limit:     resourcePageLimit,
	})
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
		listed = append(listed, relationOf(relation, id))
	}

	reached := make([]relatedRow, 0, len(related.Items))
	for _, project := range related.Items {
		reached = append(reached, relatedRow{
			Related: project,
			Link:    link("/project", "id", project.Project.Id.String()),
		})
	}

	activity := activityRows(summary.ResourceTypes, fleet.Items, from, to, now)

	periods := periodRows(billed, key)

	// What the members of a meta-project are billed. A meta-project owns no
	// resources and has no statement of its own, so this is the money its page
	// shows, and it is read for a meta-project and for no other project.
	var rollup *rollupView
	if project.Platform == projectcore.PlatformMeta {
		if rollup, err = h.buildRollup(r, project, &src); err != nil {
			h.failFrom(w, r, err, src)
			return
		}
	}

	// What a partner is owed. Only a partner is ever a beneficiary, and the
	// column holds the external id alone, so the read is made for a partner
	// and for no other project: another platform's project of the same
	// external id would otherwise answer with a settlement that is not its.
	var settled []store.Kickback
	if project.Platform == projectcore.PlatformPartner {
		if settled, err = h.store.ListKickbacksForBeneficiary(ctx, project.ExternalId); err != nil {
			h.failFrom(w, r, storeFailed(err), src)
			return
		}
		src.query("ListKickbacksForBeneficiary")
	}

	data := projectData{
		Project:    project,
		Relations:  tabulate(r, "relations", relationColumns, listed),
		Related:    tabulate(r, "related", relatedColumns, reached),
		Activity:   tabulate(r, "activity", activityColumns, activity),
		Partial:    fleet.NextCursor != nil,
		Window:     buildWindowView(r, from, to, now),
		From:       from,
		To:         to,
		Key:        key,
		Statements: tabulate(r, "statements", projectPeriodColumns, periods),
		Kickbacks:  tabulate(r, "kickbacks", settlementColumns, settlementRows(settled)),
		Rollup:     rollup,
	}
	h.render(w, r, "project", page{
		Title:   "Project " + optString(project.Name, project.ExternalId),
		Sources: src,
		Data:    data,
	})
}

// activityRows puts the resources of a project under the type they are of, so
// that a type the summary counted folds open into the resources it counted.
// Only the resources that lived inside the window are listed, the rule the
// fleet reads a window by, and they are listed in the order their ids sort in,
// which is the order a reader looks one up in.
//
// A type the summary reports and the projection holds nothing of is drawn
// without a fold rather than with an empty one: the summary folds the events
// of the window, and the projection holds what the resources are today, so a
// resource the project has since handed on is counted here and listed nowhere.
func activityRows(
	types []httpapi.ProjectActivity, resources []httpapi.Resource, from, to, now time.Time,
) []activityRow {
	byType := make(map[string][]projectResourceRow, len(types))
	for _, resource := range resources {
		if !livedBetween(resource, &from, &to) {
			continue
		}
		lifetime, lived := lifetimeHours(resource, now)
		byType[resource.ResourceType] = append(byType[resource.ResourceType], projectResourceRow{
			Resource: resource,
			Link: link("/resource",
				"cloud", resource.Cloud, "type", resource.ResourceType, "id", resource.ResourceId),
			Created:  createdText(resource),
			Lifetime: lifetime,
			Lived:    lived,
		})
	}

	rows := make([]activityRow, 0, len(types))
	for _, activity := range types {
		under := byType[activity.ResourceType]
		slices.SortFunc(under, func(a, b projectResourceRow) int {
			return cmp.Compare(a.Resource.ResourceId, b.Resource.ResourceId)
		})

		searched := make([]string, 0, len(under)+1)
		searched = append(searched, activity.ResourceType)
		for _, resource := range under {
			searched = append(searched, resource.Resource.ResourceId)
		}
		rows = append(rows, activityRow{
			Activity:  activity,
			Resources: under,
			Searched:  strings.Join(searched, " "),
		})
	}
	return rows
}

// settlementRows groups what the runs settle for one partner into the periods
// they settle, oldest first, and adds each period up. Every row a run of a
// period holds counts: a regular run's rows are what it owes, and a
// correction's are the difference to the run it corrects, so the two add up to
// what the partner is owed for the period the way the statements of a period
// add up to what a project is charged.
func settlementRows(settled []store.Kickback) []settlementRow {
	var rows []settlementRow
	index := make(map[time.Time]int, len(settled))
	for _, record := range settled {
		at, held := index[record.PeriodFrom]
		if !held {
			at = len(rows)
			index[record.PeriodFrom] = at
			rows = append(rows, settlementRow{From: record.PeriodFrom})
		}
		rows[at].Records = append(rows[at].Records, record)
	}

	for i := range rows {
		sums := make(map[string]decimal.Decimal)
		searched := []string{stamp(rows[i].From)}
		for _, record := range rows[i].Records {
			sums[record.Currency] = sums[record.Currency].Add(record.Amount)
			searched = append(searched, record.Kind, record.ProjectID)
			if record.Kind == regularKind {
				rows[i].Status = record.Status
			}
		}
		for _, currency := range slices.Sorted(maps.Keys(sums)) {
			rows[i].Totals = append(rows[i].Totals, currencyTotal{Total: sums[currency], Currency: currency})
		}
		if len(rows[i].Totals) > 0 {
			rows[i].Sort = rows[i].Totals[0].Total
		}
		if rows[i].Status == "" {
			rows[i].Status = absent
		}
		rows[i].Searched = strings.Join(searched, " ")
	}
	return rows
}

// periodRows groups what every run billed one project into the periods the
// runs billed, oldest period first, which is the order the statements arrive
// in. Inside a period the statements keep that order too, which is the order
// the runs started in, so a correction stands under the run it corrects.
//
// The period's total is the sum of the statements that stand. A period whose
// statements were all replaced adds up to nothing, which is what it is: every
// run of it has been superseded by another the page also holds.
func periodRows(billed []store.ProjectStatementRow, key string) []periodRow {
	var rows []periodRow
	index := make(map[time.Time]int, len(billed))
	for _, row := range billed {
		at, held := index[row.PeriodFrom]
		if !held {
			at = len(rows)
			index[row.PeriodFrom] = at
			rows = append(rows, periodRow{From: row.PeriodFrom})
		}
		rows[at].Statements = append(rows[at].Statements, projectStatementRow{
			Row:      row,
			Link:     link("/statement", "run", row.RunID.String(), "key", key),
			Standing: slices.Contains(standingStatuses, row.Status),
		})
	}

	for i := range rows {
		rows[i].total()
		rows[i].describe()
	}
	return rows
}

// total adds the statements that stand up, per the currency each was billed
// in, and reports the amount the column is ordered by, which is the first
// currency of a period billed in more than one.
func (r *periodRow) total() {
	sums := make(map[string]decimal.Decimal)
	for _, statement := range r.Statements {
		if statement.Standing {
			sums[statement.Row.Currency] = sums[statement.Row.Currency].Add(statement.Row.Total)
		}
	}

	for _, currency := range slices.Sorted(maps.Keys(sums)) {
		r.Totals = append(r.Totals, currencyTotal{Total: sums[currency], Currency: currency})
	}
	if len(r.Totals) > 0 {
		r.Sort = r.Totals[0].Total
	}
}

// describe says what the period stands at and what a filter matches it on. The
// status is the standing regular run's, which is what says whether the month is
// closed; a period whose regular run was replaced and not re-run stands at
// nothing.
func (r *periodRow) describe() {
	r.Status = absent
	searched := []string{stamp(r.From)}
	for _, statement := range r.Statements {
		if statement.Standing && statement.Row.Kind == regularKind {
			r.Status = statement.Row.Status
		}
		searched = append(searched, statement.Row.Kind, statement.Row.Status)
	}
	r.Searched = strings.Join(searched, " ")
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

// relationOf is one relation as the table draws it: the two ends as links, and
// the pricing adjustments the relation carries read out of its metadata.
//
// The registry holds every adjustments document to the schema at the write, so
// one that does not read here is a document written past this API or a schema
// this console does not know. It is reported in the row rather than as a failed
// page: the other relations of the project are readable, and what the document
// says is what the reader has to see.
func relationOf(relation httpapi.Relation, self uuid.UUID) relationRow {
	row := relationRow{
		Relation:   relation,
		SourceLink: otherProjectLink(relation.SourceId, self),
		TargetLink: otherProjectLink(relation.TargetId, self),
	}

	metadata, err := json.Marshal(relation.Metadata)
	if err == nil {
		row.Adjustments, _, err = adjustment.FromMetadata(metadata)
	}
	if err != nil {
		row.Adjustments, row.Unreadable = nil, err.Error()
	}

	searched := []string{relation.RelationType}
	for _, line := range row.Adjustments {
		searched = append(searched, line.Type, line.Scope, line.Description)
	}
	row.Searched = strings.Join(searched, " ")
	return row
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

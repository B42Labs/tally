package httpui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/reporting"
	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/reporting/httpapi"
)

// testNow is the instant every page under test is rendered at, so a windowed
// read asks for the same window on every run.
var testNow = time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC)

// fakeAPI answers every Reporting API call from a canned value and records what
// it was asked, so a test can assert on the arguments a page passed down.
type fakeAPI struct {
	projects      httpapi.ProjectList
	projectsErr   error
	projectsQuery reporting.ProjectsQuery

	project    httpapi.Project
	projectErr error

	relations    httpapi.RelationList
	relationsErr error

	related    httpapi.RelatedProjectList
	relatedErr error

	summary     httpapi.ProjectSummary
	summaryErr  error
	summaryFrom time.Time
	summaryTo   time.Time

	resources      httpapi.ResourceList
	resourcesErr   error
	resourcesQuery reporting.ResourcesQuery

	lifecycle      httpapi.Lifecycle
	lifecycleErr   error
	lifecycleParts []string

	resourceStats    httpapi.ResourceStatsList
	resourceStatsErr error

	eventStats    httpapi.EventStatsList
	eventStatsErr error

	rejected    httpapi.DeadLetterList
	rejectedErr error
}

func (f *fakeAPI) ListProjects(
	_ context.Context, q reporting.ProjectsQuery,
) (httpapi.ProjectList, reporting.Request, error) {
	f.projectsQuery = q
	return f.projects, apiRequest("/api/v1/projects", url.Values{
		"platform": nonEmpty(q.Platform),
		"cloud":    nonEmpty(q.Cloud),
		"cursor":   nonEmpty(q.Cursor),
	}), f.projectsErr
}

func (f *fakeAPI) GetProject(_ context.Context, id uuid.UUID) (httpapi.Project, reporting.Request, error) {
	return f.project, apiRequest("/api/v1/projects/"+id.String(), nil), f.projectErr
}

func (f *fakeAPI) ListProjectRelations(_ context.Context, id uuid.UUID) (httpapi.RelationList, reporting.Request, error) {
	return f.relations, apiRequest("/api/v1/projects/"+id.String()+"/relations", nil), f.relationsErr
}

func (f *fakeAPI) ListRelatedProjects(
	_ context.Context, id uuid.UUID,
) (httpapi.RelatedProjectList, reporting.Request, error) {
	return f.related, apiRequest("/api/v1/projects/"+id.String()+"/related", nil), f.relatedErr
}

func (f *fakeAPI) GetProjectSummary(
	_ context.Context, id uuid.UUID, from, to time.Time,
) (httpapi.ProjectSummary, reporting.Request, error) {
	f.summaryFrom, f.summaryTo = from, to
	request := apiRequest("/api/v1/projects/"+id.String()+"/summary", url.Values{
		"from": {from.UTC().Format(time.RFC3339)},
		"to":   {to.UTC().Format(time.RFC3339)},
	})
	return f.summary, request, f.summaryErr
}

func (f *fakeAPI) ListResources(
	_ context.Context, q reporting.ResourcesQuery,
) (httpapi.ResourceList, reporting.Request, error) {
	f.resourcesQuery = q
	return f.resources, apiRequest("/api/v1/resources", url.Values{
		"cloud":         nonEmpty(q.Cloud),
		"project_id":    nonEmpty(q.ProjectID),
		"resource_type": nonEmpty(q.ResourceType),
		"state":         nonEmpty(q.State),
		"status":        nonEmpty(q.Status),
		"cursor":        nonEmpty(q.Cursor),
	}), f.resourcesErr
}

func (f *fakeAPI) GetLifecycle(
	_ context.Context, cloud, resourceType, resourceID string,
) (httpapi.Lifecycle, reporting.Request, error) {
	f.lifecycleParts = []string{cloud, resourceType, resourceID}
	route := "/api/v1/resources/" + url.PathEscape(cloud) +
		"/" + url.PathEscape(resourceType) + "/" + url.PathEscape(resourceID) + "/lifecycle"
	return f.lifecycle, apiRequest(route, nil), f.lifecycleErr
}

func (f *fakeAPI) ResourceStats(
	_ context.Context, groupBy []string, status string,
) (httpapi.ResourceStatsList, reporting.Request, error) {
	request := apiRequest("/api/v1/stats/resources", url.Values{
		"group_by": {strings.Join(groupBy, ",")},
		"status":   nonEmpty(status),
	})
	return f.resourceStats, request, f.resourceStatsErr
}

func (f *fakeAPI) EventStats(
	_ context.Context, groupBy []string, interval string, from, to time.Time,
) (httpapi.EventStatsList, reporting.Request, error) {
	request := apiRequest("/api/v1/stats/events", url.Values{
		"group_by": {strings.Join(groupBy, ",")},
		"interval": {interval},
		"from":     {from.UTC().Format(time.RFC3339)},
		"to":       {to.UTC().Format(time.RFC3339)},
	})
	return f.eventStats, request, f.eventStatsErr
}

func (f *fakeAPI) ListRejectedEvents(_ context.Context, limit int) (httpapi.DeadLetterList, reporting.Request, error) {
	request := apiRequest("/api/v1/rejected-events", url.Values{"limit": {strconv.Itoa(limit)}})
	return f.rejected, request, f.rejectedErr
}

// apiRequest builds the request line a fake call reports, the way the client
// builds the one it made.
func apiRequest(route string, query url.Values) reporting.Request {
	if len(query) > 0 {
		route += "?" + query.Encode()
	}
	return reporting.Request{Method: http.MethodGet, Path: route}
}

// nonEmpty carries a filter into a query only when it was set, so a fake
// request line reads like the one the client would have sent.
func nonEmpty(value string) []string {
	if value == "" {
		return nil
	}
	return []string{value}
}

// fakeStore answers every engine read from a canned value and records the ids
// it was asked for.
type fakeStore struct {
	periods    []store.Period
	periodsErr error

	runs      []store.Run
	runsErr   error
	runsLimit int32

	run    store.Run
	runErr error
	runID  uuid.UUID

	statements    []store.StatementRow
	statementsErr error

	statement    store.Statement
	statementErr error
	statementRun uuid.UUID
	statementKey string

	projectStatements    []store.ProjectStatementRow
	projectStatementsErr error
	projectStatementsKey string

	pricingModels    []store.PricingModel
	pricingModelsErr error

	pricingDocument    store.PricingDocument
	pricingDocumentErr error
	pricingVersion     string

	latestRun      uuid.UUID
	latestRunFound bool
	latestRunErr   error

	segments    []store.Segment
	segmentsErr error
	segmentsRun uuid.UUID

	deltas    []store.Delta
	deltasErr error
}

func (f *fakeStore) ListPeriods(context.Context) ([]store.Period, error) {
	return f.periods, f.periodsErr
}

func (f *fakeStore) ListRuns(_ context.Context, limit int32) ([]store.Run, error) {
	f.runsLimit = limit
	return f.runs, f.runsErr
}

func (f *fakeStore) GetRun(_ context.Context, id uuid.UUID) (store.Run, error) {
	f.runID = id
	return f.run, f.runErr
}

func (f *fakeStore) ListStatements(context.Context, uuid.UUID) ([]store.StatementRow, error) {
	return f.statements, f.statementsErr
}

func (f *fakeStore) GetStatement(_ context.Context, runID uuid.UUID, key string) (store.Statement, error) {
	f.statementRun, f.statementKey = runID, key
	return f.statement, f.statementErr
}

func (f *fakeStore) ListStatementsForProject(_ context.Context, key string) ([]store.ProjectStatementRow, error) {
	f.projectStatementsKey = key
	return f.projectStatements, f.projectStatementsErr
}

func (f *fakeStore) ListPricingModels(context.Context) ([]store.PricingModel, error) {
	return f.pricingModels, f.pricingModelsErr
}

func (f *fakeStore) GetPricingModel(_ context.Context, version string) (store.PricingDocument, error) {
	f.pricingVersion = version
	return f.pricingDocument, f.pricingDocumentErr
}

func (f *fakeStore) LatestRunWithResource(context.Context, string, string, string) (uuid.UUID, bool, error) {
	return f.latestRun, f.latestRunFound, f.latestRunErr
}

func (f *fakeStore) ListResourceSegments(
	_ context.Context, runID uuid.UUID, _, _, _ string,
) ([]store.Segment, error) {
	f.segmentsRun = runID
	return f.segments, f.segmentsErr
}

func (f *fakeStore) ListCorrectionDeltas(context.Context, uuid.UUID) ([]store.Delta, error) {
	return f.deltas, f.deltasErr
}

// serve builds the console over two fakes and hands back the log it writes, so
// a test can assert that a failed page reached the operator as well as the
// viewer.
func serve(t *testing.T, api API, engine Store, now time.Time) (http.Handler, *bytes.Buffer) {
	t.Helper()

	logged := &bytes.Buffer{}
	handler, err := NewRouter(Options{
		Logger: slog.New(slog.NewTextHandler(logged, &slog.HandlerOptions{Level: slog.LevelDebug})),
		API:    api,
		Store:  engine,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("building the console: %v", err)
	}
	return handler, logged
}

// get asks the console for one page and reads the whole answer.
func get(t *testing.T, handler http.Handler, path string) (status int, body, contentType string) {
	t.Helper()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

	result := recorder.Result()
	defer func() { _ = result.Body.Close() }()

	read, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("reading the answer of %s: %v", path, err)
	}
	return result.StatusCode, string(read), result.Header.Get("Content-Type")
}

func TestTemplatesParse(t *testing.T) {
	t.Parallel()

	handler, err := NewRouter(Options{API: &fakeAPI{}, Store: &fakeStore{}})
	if err != nil {
		t.Fatalf("parsing the console templates: %v", err)
	}
	if handler == nil {
		t.Fatal("NewRouter returned no handler")
	}
}

func TestUnknownPathIs404(t *testing.T) {
	t.Parallel()

	handler, logged := serve(t, &fakeAPI{}, &fakeStore{}, testNow)

	status, body, contentType := get(t, handler, "/nowhere")
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
	}
	if contentType != htmlContentType {
		t.Errorf("content type = %q, want %q", contentType, htmlContentType)
	}
	for _, want := range []string{"no such page", "/nowhere", "this page read nothing"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not carry %q:\n%s", want, body)
		}
	}
	if !strings.Contains(logged.String(), "/nowhere") {
		t.Errorf("the log does not name the path:\n%s", logged.String())
	}
}

func TestBarWidth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		value, max string
		want       int64
	}{
		{name: "a table of zeros draws nothing", value: "0", max: "0", want: 0},
		{name: "the largest value fills the chart", value: "12.50", max: "12.50", want: chartWidth},
		{name: "half of it fills half", value: "6.25", max: "12.50", want: chartWidth / 2},
		{name: "a negative value draws nothing", value: "-1.00", max: "12.50", want: 0},
		{name: "a zero value draws nothing", value: "0", max: "12.50", want: 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := barWidth(mustDecimal(t, c.value), mustDecimal(t, c.max)); got != c.want {
				t.Errorf("barWidth(%s, %s) = %d, want %d", c.value, c.max, got, c.want)
			}
		})
	}
}

func TestBuildTimeline(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	t.Run("an open interval reaches the right edge", func(t *testing.T) {
		t.Parallel()

		line := buildTimeline([]httpapi.LifecycleInterval{
			{From: start, To: pointerTo(start.Add(2 * time.Hour)), State: "active"},
			{From: start.Add(2 * time.Hour), To: nil, State: "shutoff"},
		}, nil, start.Add(4*time.Hour))

		if len(line.Upper) != 2 {
			t.Fatalf("the lifecycle lane holds %d rectangles, want 2", len(line.Upper))
		}
		open := line.Upper[1]
		if !open.Open {
			t.Error("the open interval is not flagged open")
		}
		if open.Label != "still open" {
			t.Errorf("the open interval is labelled %q, want %q", open.Label, "still open")
		}
		if open.X+open.W != chartWidth {
			t.Errorf("the open interval ends at %d, want %d", open.X+open.W, chartWidth)
		}
	})

	t.Run("a span of one second divides", func(t *testing.T) {
		t.Parallel()

		line := buildTimeline([]httpapi.LifecycleInterval{
			{From: start, To: pointerTo(start.Add(time.Second)), State: "active"},
		}, nil, start)

		if len(line.Upper) != 1 {
			t.Fatalf("the lifecycle lane holds %d rectangles, want 1", len(line.Upper))
		}
		if line.Upper[0].W != chartWidth {
			t.Errorf("the only interval is %d wide, want %d", line.Upper[0].W, chartWidth)
		}
	})

	t.Run("a short segment group stays visible", func(t *testing.T) {
		t.Parallel()

		line := buildTimeline([]httpapi.LifecycleInterval{
			{From: start, To: pointerTo(start.Add(24 * time.Hour)), State: "active"},
		}, []segmentGroup{
			{State: "active", From: start, To: start.Add(time.Second), Amount: mustDecimal(t, "0.01")},
		}, start)

		if len(line.Lower) != 1 {
			t.Fatalf("the metered lane holds %d rectangles, want 1", len(line.Lower))
		}
		if line.Lower[0].W < 1 {
			t.Errorf("the segment group is %d wide, want at least 1", line.Lower[0].W)
		}
		if line.Lower[0].Label != "0.01" {
			t.Errorf("the segment group is labelled %q, want %q", line.Lower[0].Label, "0.01")
		}
	})

	t.Run("nothing to draw is an empty picture", func(t *testing.T) {
		t.Parallel()

		line := buildTimeline(nil, nil, start)
		if line.Width != chartWidth {
			t.Errorf("width = %d, want %d", line.Width, chartWidth)
		}
		if len(line.Upper) != 0 || len(line.Lower) != 0 {
			t.Errorf("an empty picture holds %d and %d rectangles", len(line.Upper), len(line.Lower))
		}
	})
}

// mustDecimal parses a decimal a test wrote as text. Money never comes from a
// float, in a test as much as in the code under test.
func mustDecimal(t *testing.T, text string) decimal.Decimal {
	t.Helper()

	d, err := decimal.NewFromString(text)
	if err != nil {
		t.Fatalf("parsing the decimal %q: %v", text, err)
	}
	return d
}

// pointerTo addresses a value the API models carry as a pointer.
func pointerTo[T any](value T) *T {
	return &value
}

package httpui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/reporting"
	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/statements"
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

// The world every full page is rendered from. One project, one resource, one
// run, one statement: enough for every section of every page to hold a row.
var (
	testProjectID = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	testRunID     = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	testKey       = statements.Key("os-sim", "p-1")
	testPeriod    = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
)

// fullAPI answers every Reporting API call of every page.
func fullAPI(t *testing.T) *fakeAPI {
	t.Helper()

	name := "the first project"
	created := testPeriod
	ended := testPeriod.Add(24 * time.Hour)
	state := "active"

	project := httpapi.Project{
		Id:         testProjectID,
		Cloud:      "os-sim",
		ExternalId: "p-1",
		Platform:   "openstack",
		Name:       &name,
		Metadata:   map[string]interface{}{"owner": "demo"},
		CreatedAt:  created,
	}
	resource := httpapi.Resource{
		Cloud:         "os-sim",
		Platform:      "openstack",
		ProjectId:     "p-1",
		ResourceId:    "vm-1",
		ResourceType:  "instance",
		State:         "active",
		Size:          map[string]interface{}{"vcpus": 4},
		CreatedAt:     &created,
		LastEventAt:   created,
		LastEventType: "instance.create.end",
		LastPayload:   &map[string]interface{}{"state": "active"},
	}

	return &fakeAPI{
		projects: httpapi.ProjectList{Items: []httpapi.Project{project}},
		project:  project,
		relations: httpapi.RelationList{Items: []httpapi.Relation{{
			Id:           uuid.MustParse("33333333-3333-4333-8333-333333333333"),
			RelationType: "infrastructure_tenant",
			SourceId:     testProjectID,
			TargetId:     testProjectID,
			ValidFrom:    created,
			Metadata:     map[string]interface{}{},
			CreatedAt:    created,
		}}},
		related: httpapi.RelatedProjectList{Items: []httpapi.RelatedProject{{
			Project:      project,
			RelationType: "infrastructure_tenant",
			Depth:        1,
		}}},
		summary: httpapi.ProjectSummary{
			Project: httpapi.ProjectRef{Id: testProjectID, Cloud: "os-sim", ExternalId: "p-1"},
			ResourceTypes: []httpapi.ProjectActivity{{
				ResourceType: "instance", Created: 1, Deleted: 0, ActiveNow: 1, TotalMinutes: 1440,
			}},
		},
		resources: httpapi.ResourceList{Items: []httpapi.Resource{resource}},
		lifecycle: httpapi.Lifecycle{
			Resource: resource,
			Events: []httpapi.StoredEvent{{
				Cloud: "os-sim", EventId: "e-1", EventType: "instance.create.end", Platform: "openstack",
				ProjectId: "p-1", ReceivedAt: created, ResourceId: "vm-1", ResourceType: "instance",
				Source: "collector", Timestamp: created,
			}},
			Intervals: []httpapi.LifecycleInterval{{
				From: created, To: &ended, ProjectId: "p-1", State: "active",
				Size: map[string]interface{}{"vcpus": 4},
			}},
			Warnings: []string{},
		},
		resourceStats: httpapi.ResourceStatsList{Items: []httpapi.ResourceStatsItem{
			{Cloud: "os-sim", ResourceType: "instance", State: &state, Count: 3},
			{Cloud: "os-sim", ResourceType: "volume", State: &state, Count: 1},
		}},
		eventStats: httpapi.EventStatsList{Items: []httpapi.EventStatsItem{
			{Bucket: created, Cloud: "os-sim", EventType: "instance.create.end", Count: 2},
		}},
		rejected: httpapi.DeadLetterList{Items: []httpapi.DeadLetteredEvent{{
			Id:         uuid.MustParse("44444444-4444-4444-8444-444444444444"),
			Reason:     "schema: 'vcpus' is a required property",
			ReceivedAt: created,
			Raw:        map[string]interface{}{"event_id": "e-2"},
		}}},
	}
}

// fullStore answers every engine read of every page.
func fullStore(t *testing.T) *fakeStore {
	t.Helper()

	run := store.Run{
		ID:             testRunID,
		PeriodFrom:     testPeriod,
		PeriodTo:       testPeriod.AddDate(0, 1, 0),
		Kind:           "regular",
		PricingVersion: "2026-03",
		Status:         "completed",
		Clouds:         []string{"os-sim"},
		StartedAt:      testPeriod.AddDate(0, 1, 0),
		CompletedAt:    testPeriod.AddDate(0, 1, 0).Add(time.Minute),
	}

	return &fakeStore{
		periods: []store.Period{{
			From: testPeriod, To: testPeriod.AddDate(0, 1, 0), Status: "open",
		}},
		runs:   []store.Run{run},
		run:    run,
		runErr: nil,
		statements: []store.StatementRow{
			{Key: testKey, Total: mustDecimal(t, "128.45"), Currency: "EUR"},
			{Key: "os-sim/p-2", Total: mustDecimal(t, "64.00"), Currency: "EUR"},
		},
		statement: store.Statement{
			Document: goldenStatement(t),
			Total:    mustDecimal(t, "128.45"),
			Currency: "EUR",
		},
		projectStatements: []store.ProjectStatementRow{{
			RunID: testRunID, PeriodFrom: testPeriod, Kind: "regular", Status: "completed",
			Total: mustDecimal(t, "128.45"), Currency: "EUR",
		}},
		pricingModels: []store.PricingModel{{
			Version: "2026-03", ValidFrom: testPeriod, Currency: "EUR", ImportedAt: testPeriod,
		}},
		pricingDocument: store.PricingDocument{
			PricingModel: store.PricingModel{
				Version: "2026-03", ValidFrom: testPeriod, Currency: "EUR", ImportedAt: testPeriod,
			},
			Document: shippedCatalog(t),
		},
		latestRun:      testRunID,
		latestRunFound: true,
		segments: []store.Segment{
			{
				State: "active", From: testPeriod, To: testPeriod.Add(24 * time.Hour), Seconds: 86400,
				Dimension: "vcpus", Amount: mustDecimal(t, "1.92"), Currency: "EUR",
			},
			{
				State: "active", From: testPeriod, To: testPeriod.Add(24 * time.Hour), Seconds: 86400,
				Dimension: "ram_gb", Amount: mustDecimal(t, "0.96"), Currency: "EUR",
			},
		},
		deltas: []store.Delta{{
			Cloud: "os-sim", Platform: "openstack", ResourceType: "instance", ResourceID: "vm-1",
			ProjectID: "p-1", Dimension: "vcpus",
			OldAmount: mustDecimal(t, "1.92"), NewAmount: mustDecimal(t, "2.88"),
			Delta: mustDecimal(t, "0.96"), Currency: "EUR",
		}},
	}
}

// goldenStatement is the document the engine's export golden holds, so the page
// is tested against a statement a run really wrote.
func goldenStatement(t *testing.T) []byte {
	t.Helper()

	path := filepath.Join("..", "..", "engine", "export", "testdata", "golden", "regular",
		"statement-os-prod%2Fproj-456.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the golden statement: %v", err)
	}
	return raw
}

// shippedCatalog is the catalog the repository ships, parsed into the document
// form a version is stored in.
func shippedCatalog(t *testing.T) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "pricing", "2026-03.yaml"))
	if err != nil {
		t.Fatalf("reading the shipped catalog: %v", err)
	}
	return catalogDocument(t, raw)
}

// catalogDocument turns a catalog file into the JSON document the store holds.
func catalogDocument(t *testing.T, file []byte) []byte {
	t.Helper()

	_, document, err := pricing.Parse(file)
	if err != nil {
		t.Fatalf("parsing the catalog: %v", err)
	}
	return document
}

// pagePaths is every page route with the parameters it needs.
func pagePaths() []string {
	return []string{
		"/",
		"/projects",
		"/project?id=" + testProjectID.String(),
		"/resources",
		"/resource?cloud=os-sim&type=instance&id=vm-1",
		"/pricing",
		"/catalog?version=2026-03",
		"/run?id=" + testRunID.String(),
		"/statement?run=" + testRunID.String() + "&key=" + url.QueryEscape(testKey),
	}
}

// assetPattern finds every URL a page tells the browser to load or follow.
var assetPattern = regexp.MustCompile(`(?:href|src)="([^"]*)"`)

func TestEveryRouteRendersWithoutScripts(t *testing.T) {
	t.Parallel()

	handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

	for _, path := range pagePaths() {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			status, body, contentType := get(t, handler, path)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want %d:\n%s", status, http.StatusOK, body)
			}
			if contentType != htmlContentType {
				t.Errorf("content type = %q, want %q", contentType, htmlContentType)
			}
			if strings.Contains(body, "<script") {
				t.Error("the page carries a script element")
			}
			for _, match := range assetPattern.FindAllStringSubmatch(body, -1) {
				reference := match[1]
				if strings.HasPrefix(reference, "http://") || strings.HasPrefix(reference, "https://") {
					t.Errorf("the page loads the external URL %q", reference)
				}
				if strings.HasPrefix(reference, "/static/") && reference != "/static/console.css" {
					t.Errorf("the page loads the asset %q beside the stylesheet", reference)
				}
			}
			if !strings.Contains(body, `href="/static/console.css"`) {
				t.Error("the page does not load the stylesheet")
			}
		})
	}

	t.Run("the stylesheet", func(t *testing.T) {
		t.Parallel()

		status, body, contentType := get(t, handler, "/static/console.css")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d", status, http.StatusOK)
		}
		if contentType != "text/css; charset=utf-8" {
			t.Errorf("content type = %q, want text/css; charset=utf-8", contentType)
		}
		if !strings.Contains(body, ".bar") {
			t.Error("the stylesheet defines no bar colour")
		}
	})
}

func TestOverviewPage(t *testing.T) {
	t.Parallel()

	t.Run("nothing was refused", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.rejected = httpapi.DeadLetterList{Items: nil}
		handler, _ := serve(t, api, fullStore(t), testNow)

		status, body, _ := get(t, handler, "/")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d", status, http.StatusOK)
		}
		if !strings.Contains(body, "nothing was refused") {
			t.Error("the page does not say that nothing was refused")
		}
		if strings.Contains(body, "The ingest path refused these events") {
			t.Error("the page holds the refused-items heading with nothing under it")
		}
	})

	t.Run("an engine that has run nothing", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.periods = nil
		engine.runs = nil
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, "/")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d", status, http.StatusOK)
		}
		for _, want := range []string{"no period is opened", "no run has been recorded", "instance", "volume"} {
			if !strings.Contains(body, want) {
				t.Errorf("the page does not carry %q", want)
			}
		}
	})

	t.Run("five reads in the footer", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/")
		if got := strings.Count(body, "<li><span>"); got != 5 {
			t.Errorf("the footer lists %d reads, want 5:\n%s", got, body)
		}
		wanted := []string{
			"GET /api/v1/stats/resources?group_by=cloud%2Cresource_type%2Cstate",
			"GET /api/v1/stats/events?",
			"GET /api/v1/rejected-events?limit=5",
			"ListPeriods",
			"ListRuns",
		}
		for _, want := range wanted {
			if !strings.Contains(body, want) {
				t.Errorf("the footer does not name %q", want)
			}
		}
	})
}

func TestProjectsPage(t *testing.T) {
	t.Parallel()

	t.Run("no project matched", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.projects = httpapi.ProjectList{Items: nil}
		handler, _ := serve(t, api, fullStore(t), testNow)

		status, body, _ := get(t, handler, "/projects")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d", status, http.StatusOK)
		}
		if !strings.Contains(body, "<th>") {
			t.Error("the empty page drops the table header")
		}
		if !strings.Contains(body, "no project matched") {
			t.Error("the empty page does not say that nothing matched")
		}
	})

	t.Run("the last page carries no next link", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/projects")
		if strings.Contains(body, "next page") {
			t.Error("the last page offers a next page")
		}
	})

	t.Run("a cursor is carried into the next link", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.projects.NextCursor = pointerTo("abc")
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/projects?platform=openstack")
		if !strings.Contains(body, "cursor=abc") {
			t.Errorf("the next link carries no cursor:\n%s", body)
		}
		if !strings.Contains(body, "platform=openstack") {
			t.Error("the next link drops the filter the page was read with")
		}
		if api.projectsQuery.Platform != "openstack" {
			t.Errorf("the API was asked for platform %q, want openstack", api.projectsQuery.Platform)
		}
	})

	t.Run("a project without a name", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.projects.Items[0].Name = nil
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/projects")
		if strings.Contains(body, "the first project") {
			t.Error("the page still shows the name of a project that has none")
		}
		if got := strings.Count(body, "p-1"); got < 2 {
			t.Errorf("the external id stands in the name column %d times, want it twice", got)
		}
	})
}

func TestProjectPage(t *testing.T) {
	t.Parallel()

	t.Run("the API holds no such project", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.projectErr = &reporting.ProblemError{
			Status: http.StatusNotFound,
			Type:   "urn:tally:error:not_found",
			Title:  "Not found",
			Detail: "no project with that id",
		}
		handler, _ := serve(t, api, fullStore(t), testNow)

		status, body, _ := get(t, handler, "/project?id="+testProjectID.String())
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
		}
		for _, want := range []string{"urn:tally:error:not_found", "Not found", "no project with that id"} {
			if !strings.Contains(body, want) {
				t.Errorf("the page does not carry %q from the problem document", want)
			}
		}
	})

	t.Run("the API refuses the token", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.projectErr = &reporting.ProblemError{
			Status: http.StatusUnauthorized,
			Type:   "urn:tally:error:unauthorized",
			Title:  "Unauthorized",
		}
		handler, _ := serve(t, api, fullStore(t), testNow)

		status, body, _ := get(t, handler, "/project?id="+testProjectID.String())
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
		}
		if !strings.Contains(body, "refused the configured token") {
			t.Errorf("the page does not name the credential:\n%s", body)
		}
	})

	t.Run("the API cannot be reached", func(t *testing.T) {
		t.Parallel()

		endpoint := "http://127.0.0.1:1/api/v1/projects/" + testProjectID.String()
		api := fullAPI(t)
		api.projectErr = fmt.Errorf("calling %s: %w", endpoint, errors.New("connection refused"))
		handler, logged := serve(t, api, fullStore(t), testNow)

		status, body, _ := get(t, handler, "/project?id="+testProjectID.String())
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
		}
		if !strings.Contains(body, endpoint) {
			t.Errorf("the page does not name the endpoint:\n%s", body)
		}
		if !strings.Contains(logged.String(), "connection refused") {
			t.Errorf("the log does not carry the wrapped error:\n%s", logged.String())
		}
	})

	t.Run("a full project page", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, "/project?id="+testProjectID.String())
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d:\n%s", status, http.StatusOK, body)
		}
		wanted := []string{
			"GET /api/v1/projects/" + testProjectID.String(),
			"/relations",
			"/related",
			"from=2026-03-01T00%3A00%3A00Z",
			"to=2026-04-01T00%3A00%3A00Z",
			"ListStatementsForProject",
		}
		for _, want := range wanted {
			if !strings.Contains(body, want) {
				t.Errorf("the footer does not name %q:\n%s", want, body)
			}
		}
		if engine.projectStatementsKey != testKey {
			t.Errorf("the statements were read under %q, want %q", engine.projectStatementsKey, testKey)
		}
		if !strings.Contains(body, "key=os-sim%2Fp-1") {
			t.Error("the statement row does not link to the stored key")
		}
		if !strings.Contains(body, "run="+testRunID.String()) {
			t.Error("the statement row does not link to its run")
		}
	})
}

func TestPricingPage(t *testing.T) {
	t.Parallel()

	handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

	status, body, _ := get(t, handler, "/pricing")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if !strings.Contains(body, "ListPricingModels") {
		t.Error("the footer does not name the query the page read")
	}
	if strings.Contains(body, kindAPI) {
		t.Error("the page claims to have read the Reporting API")
	}
}

func TestRunPage(t *testing.T) {
	t.Parallel()

	t.Run("no such run", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.runErr = fmt.Errorf("GetRun: %w", pgx.ErrNoRows)
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, "/run?id="+testRunID.String())
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want %d:\n%s", status, http.StatusNotFound, body)
		}
	})

	t.Run("a run that billed nothing", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.statements = nil
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, "/run?id="+testRunID.String())
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d", status, http.StatusOK)
		}
		if !strings.Contains(body, "this run produced no statement") {
			t.Error("the page does not say that the run billed nothing")
		}
	})

	t.Run("a run that moved nothing", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.deltas = nil
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		_, body, _ := get(t, handler, "/run?id="+testRunID.String())
		if strings.Contains(body, "What this run moved") {
			t.Error("the page holds the corrections heading with nothing under it")
		}
	})

	t.Run("a correction run", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/run?id="+testRunID.String())
		if !strings.Contains(body, "What this run moved") {
			t.Error("the page drops the corrections heading")
		}
		for _, want := range []string{"vcpus", "1.92", "2.88", "0.96"} {
			if !strings.Contains(body, want) {
				t.Errorf("the corrections table does not carry %q", want)
			}
		}
	})

	t.Run("the statements cannot be read", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.statementsErr = fmt.Errorf("ListStatements: %w", errors.New("connection refused"))
		handler, logged := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, "/run?id="+testRunID.String())
		if status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", status, http.StatusServiceUnavailable)
		}
		if !strings.Contains(body, "ListStatements") {
			t.Errorf("the page does not name the query that failed:\n%s", body)
		}
		if !strings.Contains(logged.String(), "ListStatements: connection refused") {
			t.Errorf("the log does not carry the wrapped error:\n%s", logged.String())
		}
	})

	t.Run("a run whose totals are all zero", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.statements = []store.StatementRow{
			{Key: testKey, Total: decimal.Zero, Currency: "EUR"},
			{Key: "os-sim/p-2", Total: decimal.Zero, Currency: "EUR"},
		}
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, "/run?id="+testRunID.String())
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d", status, http.StatusOK)
		}
		widths := regexp.MustCompile(`<rect[^>]*width="(\d+)"`).FindAllStringSubmatch(body, -1)
		if len(widths) != 2 {
			t.Fatalf("the page draws %d bars, want 2", len(widths))
		}
		for _, width := range widths {
			if width[1] != "0" {
				t.Errorf("a bar of a zero total is %s wide, want 0", width[1])
			}
		}
	})
}

func TestStatementPage(t *testing.T) {
	t.Parallel()

	statementPath := "/statement?run=" + testRunID.String() + "&key=" + url.QueryEscape(testKey)

	t.Run("no such statement", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.statementErr = fmt.Errorf("GetStatement: %w", pgx.ErrNoRows)
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, _, _ := get(t, handler, statementPath)
		if status != http.StatusNotFound {
			t.Fatalf("status = %d, want %d", status, http.StatusNotFound)
		}
	})

	t.Run("a document that is no statement", func(t *testing.T) {
		t.Parallel()

		for _, document := range []string{`{}`, `{"currency":"EUR"}`, `not json at all`} {
			engine := fullStore(t)
			engine.statement = store.Statement{Document: []byte(document), Currency: "EUR"}
			handler, logged := serve(t, fullAPI(t), engine, testNow)

			status, body, _ := get(t, handler, statementPath)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d for %s, want %d", status, document, http.StatusServiceUnavailable)
			}
			want := fmt.Sprintf("reading the statement of %s in run %s", testKey, testRunID)
			if !strings.Contains(body, want) {
				t.Errorf("the page for %s does not carry %q:\n%s", document, want, body)
			}
			if !strings.Contains(logged.String(), "reading the statement of") {
				t.Errorf("the log for %s does not carry the failure:\n%s", document, logged.String())
			}
		}
	})

	t.Run("the golden statement", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		status, body, _ := get(t, handler, statementPath)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d:\n%s", status, http.StatusOK, body)
		}
		for _, want := range []string{"128.45", "240.00", "80.0000", "1.0000", "0.5000", "EUR"} {
			if !strings.Contains(body, want) {
				t.Errorf("the bill does not carry %q", want)
			}
		}
		for _, unwanted := range []string{"base cost", "net cost", "kickback total", "Adjustments"} {
			if strings.Contains(body, unwanted) {
				t.Errorf("the bill carries %q, which this statement holds none of", unwanted)
			}
		}
		if !strings.Contains(body, "os-sim") || !strings.Contains(body, "p-1") {
			t.Error("the bill does not name the pair its key was built from")
		}
	})

	t.Run("a statement carrying a related cost", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.statement = store.Statement{
			Document: relatedCostDocument(t),
			Total:    mustDecimal(t, "12.00"),
			Currency: "EUR",
		}
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, statementPath)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d:\n%s", status, http.StatusOK, body)
		}
		if !strings.Contains(body, "Related cost of p-2") {
			t.Errorf("the bill does not name the attributed project:\n%s", body)
		}
		for _, want := range []string{"12.00", "vm-2", "24.00"} {
			if !strings.Contains(body, want) {
				t.Errorf("the related block does not carry %q", want)
			}
		}
	})
}

// relatedCostDocument is a statement whose costs are one attributed project's,
// which the golden statement holds none of.
func relatedCostDocument(t *testing.T) []byte {
	t.Helper()

	item := statements.LineItem{
		ResourceType: "instance",
		ResourceID:   "vm-2",
		Platform:     "openstack",
		Description:  "m1.small instance",
		Periods: []statements.Period{{
			State:         "active",
			Hours:         money.NewAmount(mustDecimal(t, "24")),
			Usage:         map[string]money.Quantity{"vcpus": money.NewQuantity(mustDecimal(t, "2"))},
			Cost:          map[string]money.Amount{"vcpus": money.NewAmount(mustDecimal(t, "12"))},
			StateModifier: money.NewQuantity(mustDecimal(t, "1")),
		}},
		Total: money.NewAmount(mustDecimal(t, "12")),
	}
	document := statements.Document{
		BillingPeriod: statements.BillingPeriod{From: "2026-03-01T00:00:00Z", To: "2026-04-01T00:00:00Z"},
		ProjectID:     "p-1",
		Platform:      "openstack",
		RelatedCosts: []statements.RelatedCost{{
			RelationType: "infrastructure_tenant",
			ProjectID:    "p-2",
			Platform:     "openstack",
			LineItems:    []statements.LineItem{item},
			Total:        money.NewAmount(mustDecimal(t, "12")),
		}},
		Total:    money.NewAmount(mustDecimal(t, "12")),
		Currency: "EUR",
	}

	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("building the statement document: %v", err)
	}
	return raw
}

func TestCatalogPage(t *testing.T) {
	t.Parallel()

	t.Run("a document that is no catalog", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.pricingDocument.Document = []byte(`{"nope":true}`)
		handler, logged := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, "/catalog?version=2026-03")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", status, http.StatusServiceUnavailable)
		}
		if !strings.Contains(body, "reading the pricing catalog 2026-03") {
			t.Errorf("the page does not name the version it failed on:\n%s", body)
		}
		if !strings.Contains(logged.String(), "reading the pricing catalog") {
			t.Errorf("the log does not carry the failure:\n%s", logged.String())
		}
	})

	t.Run("the shipped catalog", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, "/catalog?version=2026-03")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d:\n%s", status, http.StatusOK, body)
		}
		if engine.pricingVersion != "2026-03" {
			t.Errorf("the store was asked for %q, want 2026-03", engine.pricingVersion)
		}
		// The harbor repository's storage is priced at five hundredths of a
		// millicent per gigabyte hour, which only the catalog's own scale shows.
		if !strings.Contains(body, "0.00005") {
			t.Errorf("the smallest price is not rendered as it was imported:\n%s", body)
		}
		if !strings.Contains(body, "0.5") {
			t.Error("the shutoff modifier of an instance is missing")
		}
	})

	t.Run("an entry without modifiers", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.pricingDocument.Document = catalogDocument(t, []byte(`
version: "test"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    floating_ip:
      dimensions:
        - metric: "count"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
`))
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, "/catalog?version=test")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d:\n%s", status, http.StatusOK, body)
		}
		if got := strings.Count(body, "<td>none</td>"); got != 2 {
			t.Errorf("the row holds %d empty modifier columns, want 2:\n%s", got, body)
		}
	})
}

func TestRequiredParameters(t *testing.T) {
	t.Parallel()

	handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

	cases := []struct {
		path      string
		parameter string
	}{
		{path: "/project", parameter: "id"},
		{path: "/project?id=not-a-uuid", parameter: "id"},
		{path: "/resource?type=instance&id=vm-1", parameter: "cloud"},
		{path: "/resource?cloud=os-sim&id=vm-1", parameter: "type"},
		{path: "/resource?cloud=os-sim&type=instance", parameter: "id"},
		{path: "/resource?cloud=os-sim&type=instance&id=vm-1&run=not-a-uuid", parameter: "run"},
		{path: "/catalog", parameter: "version"},
		{path: "/run", parameter: "id"},
		{path: "/run?id=not-a-uuid", parameter: "id"},
		{path: "/statement?key=os-sim%2Fp-1", parameter: "run"},
		{path: "/statement?run=" + testRunID.String(), parameter: "key"},
		{path: "/statement?run=not-a-uuid&key=os-sim%2Fp-1", parameter: "run"},
	}

	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			t.Parallel()

			status, body, _ := get(t, handler, c.path)
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d:\n%s", status, http.StatusBadRequest, body)
			}
			if !strings.Contains(body, "the parameter "+c.parameter) {
				t.Errorf("the page does not name the parameter %q:\n%s", c.parameter, body)
			}
		})
	}
}

func TestResourcePage(t *testing.T) {
	t.Parallel()

	resourcePath := "/resource?cloud=os-sim&type=instance&id=vm-1"

	t.Run("no run holds usage", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.latestRun, engine.latestRunFound = uuid.Nil, false
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, body, _ := get(t, handler, resourcePath)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d:\n%s", status, http.StatusOK, body)
		}
		if !strings.Contains(body, "no run holds usage for this resource") {
			t.Error("the page does not say that no run metered the resource")
		}
		if !strings.Contains(body, `class="lane state-active"`) {
			t.Errorf("the lifecycle lane is missing:\n%s", body)
		}
		if !strings.Contains(body, "instance.create.end") {
			t.Error("the events are missing")
		}
		if strings.Contains(body, "/catalog?version=") {
			t.Error("the page links a catalog although no run rated the resource")
		}
	})

	t.Run("a metered resource", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		status, body, _ := get(t, handler, resourcePath)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d", status, http.StatusOK)
		}
		if !strings.Contains(body, `href="/catalog?version=2026-03"`) {
			t.Errorf("the page does not link the catalog the run rated with:\n%s", body)
		}
		for _, want := range []string{"vcpus", "1.92", "ram_gb", "0.96", "86400"} {
			if !strings.Contains(body, want) {
				t.Errorf("the segment table does not carry %q", want)
			}
		}
		// The two dimensions of one interval are drawn as one rectangle
		// labelled with what they add up to.
		if !strings.Contains(body, ">2.88<") {
			t.Errorf("the metered lane is not labelled with the sum of its dimensions:\n%s", body)
		}
	})

	t.Run("a run the viewer named", func(t *testing.T) {
		t.Parallel()

		asked := uuid.MustParse("55555555-5555-4555-8555-555555555555")
		engine := fullStore(t)
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		status, _, _ := get(t, handler, resourcePath+"&run="+asked.String())
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d", status, http.StatusOK)
		}
		if engine.runID != asked {
			t.Errorf("the run %s was read, want the one the viewer named, %s", engine.runID, asked)
		}
		if engine.segmentsRun != asked {
			t.Errorf("the segments of run %s were read, want those of %s", engine.segmentsRun, asked)
		}
	})

	t.Run("an interval that has not ended", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.lifecycle.Intervals = []httpapi.LifecycleInterval{{
			From: testPeriod, To: nil, ProjectId: "p-1", State: "active",
		}}
		engine := fullStore(t)
		engine.latestRun, engine.latestRunFound = uuid.Nil, false
		handler, _ := serve(t, api, engine, testNow)

		_, body, _ := get(t, handler, resourcePath)
		if !strings.Contains(body, `x="0" y="16" width="640"`) {
			t.Errorf("the open interval does not reach the right edge:\n%s", body)
		}
		if !strings.Contains(body, "still open") {
			t.Error("the open interval is not labelled")
		}
	})

	t.Run("a resource the projection holds little of", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.lifecycle.Resource.CreatedAt = nil
		api.lifecycle.Resource.DeletedAt = nil
		api.lifecycle.Resource.LastPayload = nil
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, resourcePath)
		if got := strings.Count(body, "<dd>none</dd>"); got != 2 {
			t.Errorf("the page renders %d absent timestamps, want 2:\n%s", got, body)
		}
		if !strings.Contains(body, "<pre>none</pre>") {
			t.Error("the page does not render the absent payload")
		}
	})
}

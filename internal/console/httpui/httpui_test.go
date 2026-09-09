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
		"platform":    nonEmpty(q.Platform),
		"cloud":       nonEmpty(q.Cloud),
		"external_id": nonEmpty(q.ExternalID),
		"cursor":      nonEmpty(q.Cursor),
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
		"limit":         nonEmpty(strconv.Itoa(q.Limit)),
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
	// testSourceID is the project a relation reaching testProjectID leaves and
	// testTargetID the one a relation leaving it reaches, which are the two ends
	// the relations table links to.
	testSourceID = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	testTargetID = uuid.MustParse("66666666-6666-4666-8666-666666666666")
	testKey      = statements.Key("os-sim", "p-1")
	testPeriod   = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
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
			TargetId:     testTargetID,
			ValidFrom:    created,
			Metadata:     map[string]interface{}{},
			CreatedAt:    created,
		}, {
			Id:           uuid.MustParse("55555555-5555-4555-8555-555555555555"),
			RelationType: "infrastructure_tenant",
			SourceId:     testSourceID,
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
		relations := section(t, body, "Relations", "Related projects")
		for _, end := range []uuid.UUID{testSourceID, testTargetID} {
			if !strings.Contains(relations, `<a href="/project?id=`+end.String()+`">`+end.String()+"</a>") {
				t.Errorf("the relations do not link the project %s:\n%s", end, relations)
			}
		}
		if strings.Contains(relations, `<a href="/project?id=`+testProjectID.String()+`">`) {
			t.Errorf("a relation links this project back to the page it is printed on:\n%s", relations)
		}
	})

	t.Run("the activity opens on this month", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/project?id="+testProjectID.String())
		if !api.summaryFrom.Equal(testPeriod) || !api.summaryTo.Equal(testPeriod.AddDate(0, 1, 0)) {
			t.Errorf("the summary was read over %s to %s, want this month", api.summaryFrom, api.summaryTo)
		}
		for _, want := range []string{
			`name="from" value="2026-03-01T00:00"`,
			`name="to" value="2026-04-01T00:00"`,
			`aria-current="true">this month</a>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
	})

	t.Run("a window is read over what it names", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler,
			"/project?id="+testProjectID.String()+"&from=2026-02-01T00:00&to=2026-02-15T00:00:00Z")
		if !api.summaryFrom.Equal(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)) ||
			!api.summaryTo.Equal(time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)) {
			t.Errorf("the summary was read over %s to %s, want the window of the request",
				api.summaryFrom, api.summaryTo)
		}
		for _, want := range []string{
			`name="from" value="2026-02-01T00:00"`,
			`name="to" value="2026-02-15T00:00"`,
			"2026-02-01T00:00:00Z to 2026-02-15T00:00:00Z",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, `aria-current="true">this month</a>`) {
			t.Error("a window of its own is marked as this month")
		}
	})

	t.Run("the window presets carry the project and mark the one in effect", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		id := testProjectID.String()
		_, body, _ := get(t, handler, "/project?id="+id+"&activity.sort=minutes")
		for _, want := range []string{
			`<a href="/project?activity.sort=minutes&amp;from=2026-02-01T00%3A00%3A00Z&amp;id=` + id +
				`&amp;to=2026-03-01T00%3A00%3A00Z">last month</a>`,
			`<input type="hidden" name="id" value="` + id + `">`,
			`<input type="hidden" name="activity.sort" value="minutes">`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
	})

	t.Run("a resource type folds open into its resources", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/project?id="+testProjectID.String())
		if api.resourcesQuery.Cloud != "os-sim" || api.resourcesQuery.ProjectID != "p-1" ||
			api.resourcesQuery.Status != "all" {
			t.Errorf("the resources were read with %+v, want every status of this project", api.resourcesQuery)
		}
		for _, want := range []string{
			`<details class="cell"><summary>instance</summary>`,
			`<a href="/resource?cloud=os-sim&amp;id=vm-1&amp;type=instance">vm-1</a>`,
			`<td class="number">348.00</td>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
	})

	t.Run("a page of resources says the folds are not the whole list", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.resources.NextCursor = pointerTo("abc")
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/project?id="+testProjectID.String())
		if !strings.Contains(body, "a type may have run resources this list does not name") {
			t.Errorf("a partial list is not said to be one:\n%s", body)
		}

		_, body, _ = get(t, handler, "/resources")
		if strings.Contains(body, "a type may have run resources this list does not name") {
			t.Error("the note is on a page that folds nothing")
		}
	})

	t.Run("a type folds only what lived in the window", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		// vm-1 was created in March, so a February window holds none of it and
		// the row the summary still counts is drawn without a fold.
		_, body, _ := get(t, handler,
			"/project?id="+testProjectID.String()+"&from=2026-02-01T00:00&to=2026-02-15T00:00")
		activity := section(t, body, "What this project ran", "Statements")
		if strings.Contains(activity, ">vm-1</a>") || !strings.Contains(activity, "<td>instance</td>") {
			t.Errorf("a window before the resource existed folds it open:\n%s", activity)
		}
	})

	t.Run("the activity filter finds the type a resource sits in", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/project?id="+testProjectID.String()+"&activity.q=vm-1")
		activity := section(t, body, "What this project ran", "Statements")
		if !strings.Contains(activity, ">instance</summary>") || !strings.Contains(activity, "1 of 1 row match") {
			t.Errorf("the filter does not match a row on the resources under it:\n%s", activity)
		}
	})

	t.Run("half a window and a window that ends before it starts are refused", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		cases := []struct {
			query  string
			reason string
		}{
			{"&from=2026-03-01T00:00", "the parameter to is missing"},
			{"&to=2026-03-01T00:00", "the parameter from is missing"},
			{"&from=2026-03-08T00:00&to=2026-03-02T00:00", "the parameter to is not after from"},
			{"&from=whenever&to=2026-03-02T00:00", "the parameter from is not an instant"},
		}
		for _, tc := range cases {
			status, body, _ := get(t, handler, "/project?id="+testProjectID.String()+tc.query)
			if status != http.StatusBadRequest || !strings.Contains(body, tc.reason) {
				t.Errorf("%s: status = %d, body:\n%s", tc.query, status, body)
			}
		}
	})
}

// section is what one page prints between two of its headings, so an assertion
// about one table reads that table alone and not a link another table drew.
func section(t *testing.T, body, from, to string) string {
	t.Helper()

	_, after, found := strings.Cut(body, "<h2>"+from+"</h2>")
	if !found {
		t.Fatalf("the page has no %q heading:\n%s", from, body)
	}
	before, _, found := strings.Cut(after, "<h2>"+to+"</h2>")
	if !found {
		t.Fatalf("the page has no %q heading:\n%s", to, body)
	}
	return before
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
		if !strings.Contains(body, "Related cost of <code>p-2</code>") {
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
		if got := strings.Count(body, "<dd>none</dd>"); got != 1 {
			t.Errorf("the page renders %d absent timestamps, want the deletion alone:\n%s", got, body)
		}
		if !strings.Contains(body, "<dt>created</dt><dd>unknown, the history starts with") {
			t.Error("the page does not say the creation is unknown")
		}
		if !strings.Contains(body, "<pre>none</pre>") {
			t.Error("the page does not render the absent payload")
		}
	})
}

// post sends one form to the console and reads the whole answer, cookies
// included.
func post(t *testing.T, handler http.Handler, path string, form url.Values) *http.Response {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	result := recorder.Result()
	t.Cleanup(func() { _ = result.Body.Close() })
	return result
}

// getWithCookie asks for one page as a viewer who chose a theme.
func getWithCookie(t *testing.T, handler http.Handler, path, theme string) string {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.AddCookie(&http.Cookie{Name: themeCookie, Value: theme})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	result := recorder.Result()
	defer func() { _ = result.Body.Close() }()
	read, err := io.ReadAll(result.Body)
	if err != nil {
		t.Fatalf("reading the answer of %s: %v", path, err)
	}
	return string(read)
}

// themeCookieOf finds the theme cookie an answer set.
func themeCookieOf(t *testing.T, result *http.Response) *http.Cookie {
	t.Helper()

	for _, cookie := range result.Cookies() {
		if cookie.Name == themeCookie {
			return cookie
		}
	}
	t.Fatal("the answer set no theme cookie")
	return nil
}

func TestTheme(t *testing.T) {
	t.Parallel()

	t.Run("a page without a cookie follows the system", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/projects?platform=openstack")
		if strings.Contains(body, "data-theme") {
			t.Error("the page pins a theme nobody chose")
		}
		if !strings.Contains(body, `value="auto" aria-pressed="true"`) {
			t.Error("the auto button is not pressed")
		}
		if !strings.Contains(body, `name="back" value="/projects?platform=openstack"`) {
			t.Errorf("the form does not carry the page it stands on:\n%s", body)
		}
	})

	t.Run("a choice is kept in a cookie and the viewer goes back", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		result := post(t, handler, themeRoute, url.Values{"theme": {"dark"}, "back": {"/pricing?models.sort=version"}})
		if result.StatusCode != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusSeeOther)
		}
		if location := result.Header.Get("Location"); location != "/pricing?models.sort=version" {
			t.Errorf("location = %q, want the page the form was on", location)
		}
		cookie := themeCookieOf(t, result)
		if cookie.Value != "dark" || cookie.MaxAge != themeCookieAge || cookie.Path != "/" {
			t.Errorf("cookie = %s, want dark for a year on /", cookie)
		}
	})

	t.Run("the page renders the choice", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		body := getWithCookie(t, handler, "/", "dark")
		if !strings.Contains(body, `<html lang="en" data-theme="dark">`) {
			t.Error("the page does not carry the chosen theme")
		}
		if !strings.Contains(body, `value="dark" aria-pressed="true"`) {
			t.Error("the dark button is not pressed")
		}
		if strings.Contains(body, `value="auto" aria-pressed="true"`) {
			t.Error("the auto button is still pressed")
		}

		body = getWithCookie(t, handler, "/nowhere", "light")
		if !strings.Contains(body, `data-theme="light"`) {
			t.Error("the error page ignores the chosen theme")
		}
	})

	t.Run("a cookie holding nonsense is no choice", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		if body := getWithCookie(t, handler, "/", "purple"); strings.Contains(body, "data-theme") {
			t.Error("the page pins a theme the stylesheet does not know")
		}
	})

	t.Run("auto deletes the cookie", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		result := post(t, handler, themeRoute, url.Values{"theme": {"auto"}, "back": {"/"}})
		if result.StatusCode != http.StatusSeeOther {
			t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusSeeOther)
		}
		if cookie := themeCookieOf(t, result); cookie.MaxAge >= 0 || cookie.Value != "" {
			t.Errorf("cookie = %s, want it deleted", cookie)
		}
	})

	t.Run("a back path off the console lands on the front page", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		for _, back := range []string{"https://example.com/", "//example.com/", "", "pricing"} {
			result := post(t, handler, themeRoute, url.Values{"theme": {"light"}, "back": {back}})
			if location := result.Header.Get("Location"); location != "/" {
				t.Errorf("back=%q sent the viewer to %q", back, location)
			}
		}
	})

	t.Run("a choice that is none of the three is refused", func(t *testing.T) {
		t.Parallel()

		handler, logged := serve(t, fullAPI(t), fullStore(t), testNow)

		result := post(t, handler, themeRoute, url.Values{"theme": {"purple"}, "back": {"/"}})
		if result.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want %d", result.StatusCode, http.StatusBadRequest)
		}
		if len(result.Cookies()) != 0 {
			t.Error("a refused choice set a cookie")
		}
		if !strings.Contains(logged.String(), "is not auto, light or dark") {
			t.Errorf("the log does not name the choice:\n%s", logged.String())
		}
	})

	t.Run("fetching the route lands on the error page", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		status, body, contentType := get(t, handler, themeRoute)
		if status != http.StatusMethodNotAllowed {
			t.Errorf("status = %d, want %d", status, http.StatusMethodNotAllowed)
		}
		if contentType != htmlContentType || !strings.Contains(body, "does not answer GET") {
			t.Errorf("the answer is not the error page:\n%s", body)
		}
	})
}

func TestTablesOnPages(t *testing.T) {
	t.Parallel()

	t.Run("the overview sorts a table by a number", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/?stats.sort=count")
		volume, instance := strings.Index(body, "<td>volume</td>"), strings.Index(body, "<td>instance</td>")
		if volume < 0 || instance < 0 || volume > instance {
			t.Errorf("the count of one comes after the count of three:\n%s", body)
		}
		if !strings.Contains(body, `<th class="number" aria-sort="ascending"><a href="/?stats.sort=-count">count</a></th>`) {
			t.Errorf("the sorted heading does not say so, does not flip, or is not aligned over its numbers:\n%s", body)
		}
		if !strings.Contains(body, `<th class="number"><a href="/?events.sort=-count&amp;stats.sort=count">count</a></th>`) {
			t.Errorf("the heading of another table drops the state of this one:\n%s", body)
		}
		if !strings.Contains(body, `<th><a href="/?stats.sort=cloud">cloud</a></th>`) {
			t.Error("a text heading is aligned as a number")
		}
	})

	t.Run("the overview filters one table and leaves the others", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/?stats.q=VOLUME")
		if strings.Contains(body, `<td class="number">3</td>`) {
			t.Error("the row of the instances survived the filter")
		}
		if !strings.Contains(body, "1 of 2 rows match") {
			t.Error("the count does not say what matched")
		}
		if !strings.Contains(body, `href="/"`) {
			t.Error("the filter cannot be cleared")
		}
		if !strings.Contains(body, "instance.create.end") {
			t.Error("the filter of the counts emptied the events")
		}
		if !strings.Contains(body, `<input type="hidden" name="stats.q" value="VOLUME">`) {
			t.Error("the form of another table drops the filter of this one")
		}
	})

	t.Run("a filter that matches nothing says so", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/?runs.q=nomatch")
		if !strings.Contains(body, "0 of 1 row match") || !strings.Contains(body, "no row matches the filter") {
			t.Errorf("the emptied table does not say so:\n%s", body)
		}
		if strings.Contains(body, "no run has been recorded") {
			t.Error("the emptied table claims nothing was recorded")
		}
	})

	t.Run("the next link carries the table state", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.projects.NextCursor = pointerTo("abc")
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/projects?cloud=os-sim&projects.sort=-name&projects.q=p")
		if !strings.Contains(body, `href="/projects?cloud=os-sim&amp;cursor=abc&amp;projects.q=p&amp;projects.sort=-name"`) {
			t.Errorf("the next link drops the filter or the order:\n%s", body)
		}
		if !strings.Contains(body, "1 row on this page") {
			t.Error("the count does not say it counts one page")
		}
	})

	t.Run("the form carries the page's own identifier", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/catalog?version=2026-03&dimensions.q=vcpus")
		if !strings.Contains(body, `<input type="hidden" name="version" value="2026-03">`) {
			t.Error("the filter form drops the catalog version")
		}
		if !strings.Contains(body, `action="/catalog"`) || !strings.Contains(body, `name="dimensions.q" value="vcpus"`) {
			t.Errorf("the filter form is not the catalog's:\n%s", body)
		}
		if strings.Contains(body, "ram_gb") {
			t.Error("a metric the filter does not name survived")
		}
	})

	t.Run("the timeline draws every segment whatever the table shows", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resource?cloud=os-sim&type=instance&id=vm-1&segments.q=nomatch")
		if !strings.Contains(body, "metered and rated") {
			t.Error("the filter of the segment table emptied the timeline")
		}
		if !strings.Contains(body, "no row matches the filter") {
			t.Error("the emptied segment table does not say so")
		}
	})

	t.Run("every table of every page carries its form", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		for _, path := range pagePaths() {
			_, body, _ := get(t, handler, path)
			tables := strings.Count(body, "<table>")
			forms := strings.Count(body, `<form class="tools"`)
			// A statement's forms are its summary tables, one per section: the
			// metric tables inside the items sort and carry no form.
			if strings.HasPrefix(path, "/statement") {
				tables = strings.Count(body, "<h2>Adjustments</h2>") +
					strings.Count(body, "<h2>Line items</h2>") + strings.Count(body, "<h2>Related cost of")
			}
			if forms != tables {
				t.Errorf("%s: %d tables and %d filter forms", path, tables, forms)
			}
		}
	})
}

func TestProjectByPair(t *testing.T) {
	t.Parallel()

	t.Run("the resource pages link the project by its pair", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		want := `<a href="/project?cloud=os-sim&amp;external_id=p-1">p-1</a>`
		for _, path := range []string{"/resources", "/resource?cloud=os-sim&type=instance&id=vm-1"} {
			if _, body, _ := get(t, handler, path); !strings.Contains(body, want) {
				t.Errorf("%s does not link the project:\n%s", path, body)
			}
		}
	})

	t.Run("a resource without a project gets no link", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.resources.Items[0].ProjectId = ""
		handler, _ := serve(t, api, fullStore(t), testNow)

		if _, body, _ := get(t, handler, "/resources"); strings.Contains(body, "/project?cloud=os-sim") {
			t.Error("the page links a project the resource does not name")
		}
	})

	t.Run("the pair resolves to the project", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		handler, _ := serve(t, api, fullStore(t), testNow)

		status, body, _ := get(t, handler, "/project?cloud=os-sim&external_id=p-1")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want %d:\n%s", status, http.StatusOK, body)
		}
		if !strings.Contains(body, "Project the first project") {
			t.Error("the page is not the project's")
		}
		if got := api.projectsQuery; got.Cloud != "os-sim" || got.ExternalID != "p-1" || got.Platform != "" {
			t.Errorf("the list was asked with %+v, want the cloud and the external id alone", got)
		}
		if !strings.Contains(body, "GET /api/v1/projects?cloud=os-sim&amp;external_id=p-1") {
			t.Error("the provenance panel does not list the lookup")
		}
	})

	t.Run("the id wins over the pair", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		handler, _ := serve(t, api, fullStore(t), testNow)

		status, _, _ := get(t, handler, "/project?id="+testProjectID.String()+"&cloud=other&external_id=p-9")
		if status != http.StatusOK {
			t.Errorf("status = %d, want %d", status, http.StatusOK)
		}
		if api.projectsQuery.ExternalID != "" {
			t.Error("the list was asked although the id was there")
		}
	})

	t.Run("a pair nothing is registered under is 404", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.projects.Items = nil
		handler, _ := serve(t, api, fullStore(t), testNow)

		status, body, _ := get(t, handler, "/project?cloud=os-sim&external_id=p-9")
		if status != http.StatusNotFound {
			t.Errorf("status = %d, want %d", status, http.StatusNotFound)
		}
		if !strings.Contains(body, "the cloud os-sim and the external id p-9") {
			t.Errorf("the error page does not name the pair:\n%s", body)
		}
	})

	t.Run("a project of another pair does not stand in", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.projects.Items[0].ExternalId = "p-10"
		handler, _ := serve(t, api, fullStore(t), testNow)

		if status, _, _ := get(t, handler, "/project?cloud=os-sim&external_id=p-1"); status != http.StatusNotFound {
			t.Errorf("status = %d, want %d", status, http.StatusNotFound)
		}
	})

	t.Run("the pair needs its cloud", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		status, body, _ := get(t, handler, "/project?external_id=p-1")
		if status != http.StatusBadRequest || !strings.Contains(body, "the parameter cloud is missing") {
			t.Errorf("status = %d, body:\n%s", status, body)
		}
	})
}

func TestFleet(t *testing.T) {
	t.Parallel()

	t.Run("the page opens on the active fleet now", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources")
		if api.resourcesQuery.Status != "active" || api.resourcesQuery.Limit != resourcePageLimit {
			t.Errorf("the API was asked with %+v, want the active fleet in the widest page", api.resourcesQuery)
		}
		for _, want := range []string{
			`aria-current="true">active</a>`,
			`aria-current="true">at an instant</a>`,
			`aria-current="true">now</a>`,
			`name="at" value=""`,
			"1 of 1 row exists now",
			`<th class="number"><a href="/resources?resources.sort=-lifetime_hours">lifetime hours</a></th>`,
			`<td class="number">348.00</td>`,
			`<details class="cell"><summary>1 key</summary><pre>{`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "on this page") {
			t.Error("a fleet that fits one page is called a page")
		}
		if form := fleetForm(t, body); strings.Contains(form, `name="from"`) || strings.Contains(form, `name="to"`) {
			t.Errorf("the instant asks for a window as well:\n%s", form)
		}
	})

	t.Run("a status is passed to the API and the switch drops the cursor", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?status=deleted&cursor=abc")
		if api.resourcesQuery.Status != "deleted" || api.resourcesQuery.Cursor != "abc" {
			t.Errorf("the API was asked with %+v", api.resourcesQuery)
		}
		for _, want := range []string{
			`<a href="/resources">active</a>`,
			`<a href="/resources?status=deleted" aria-current="true">deleted</a>`,
			`<a href="/resources?status=all">all</a>`,
			`<input type="hidden" name="cursor" value="abc">`,
			`<input type="hidden" name="status" value="deleted">`,
			"1 row on this page",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
	})

	t.Run("an unknown status is refused", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		status, body, _ := get(t, handler, "/resources?status=zombie")
		if status != http.StatusBadRequest || !strings.Contains(body, "is not active, deleted or all") {
			t.Errorf("status = %d, body:\n%s", status, body)
		}
	})

	t.Run("the instant hides what was not yet there", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?at=2026-02-01T00:00:00Z")
		if strings.Contains(body, ">vm-1</a>") {
			t.Error("a resource created after the instant is on the page")
		}
		for _, want := range []string{
			"0 of 1 row existed at 2026-02-01T00:00:00Z",
			"no resource of this page existed at 2026-02-01T00:00:00Z",
			`name="at" value="2026-02-01T00:00"`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
		if api.resourcesQuery.Status != "active" {
			t.Error("the instant changed what the API was asked for")
		}
	})

	t.Run("a deleted resource is shown while it lived", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		deleted := testPeriod.Add(4 * 24 * time.Hour)
		api.resources.Items[0].DeletedAt = &deleted
		api.resources.Items[0].State = "deleted"
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?status=all&at=2026-03-03T00:00")
		for _, want := range []string{
			">vm-1</a>",
			"1 of 1 row existed at 2026-03-03T00:00:00Z",
			`<td class="number">96.00</td>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}

		_, body, _ = get(t, handler, "/resources?status=all")
		if strings.Contains(body, ">vm-1</a>") || !strings.Contains(body, "0 of 1 row exist now") {
			t.Errorf("a deleted resource is on the page now:\n%s", body)
		}
	})

	t.Run("an unreadable instant is refused", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		status, body, _ := get(t, handler, "/resources?at=yesterday")
		if status != http.StatusBadRequest || !strings.Contains(body, "the parameter at is not an instant") {
			t.Errorf("status = %d, body:\n%s", status, body)
		}
	})

	t.Run("the presets and the form carry the rest of the page", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?status=all&resources.sort=state&cursor=abc")
		for _, want := range []string{
			`<a href="/resources?at=2026-03-08T12%3A00%3A00Z&amp;cursor=abc&amp;resources.sort=state&amp;status=all">7 days ago</a>`,
			`<a href="/resources?at=2026-03-01T00%3A00%3A00Z&amp;cursor=abc&amp;resources.sort=state&amp;status=all">start of this month</a>`,
			`<a href="/resources?at=2026-02-01T00%3A00%3A00Z&amp;cursor=abc&amp;resources.sort=state&amp;status=all">start of last month</a>`,
			`<a href="/resources?resources.sort=state&amp;status=deleted">deleted</a>`,
			`<input type="hidden" name="resources.sort" value="state">`,
			`<input type="hidden" name="status" value="all">`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
		if strings.Contains(fleetForm(t, body), `type="hidden" name="at"`) {
			t.Error("the fleet form carries its own input hidden")
		}
	})

	t.Run("a preset in effect is marked", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?at=2026-03-08T12:00:00Z")
		if !strings.Contains(body, `aria-current="true">7 days ago</a>`) {
			t.Error("the preset in effect is not marked")
		}
		if strings.Contains(body, `aria-current="true">now</a>`) {
			t.Error("now is marked although the instant is pinned")
		}
	})

	t.Run("the next link keeps the status and the instant", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.resources.NextCursor = pointerTo("abc")
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?status=all&at=2026-03-03T00:00:00Z")
		if !strings.Contains(body, `href="/resources?at=2026-03-03T00%3A00%3A00Z&amp;cursor=abc&amp;status=all"`) {
			t.Errorf("the next link drops the status or the instant:\n%s", body)
		}
		if !strings.Contains(body, "on this page") {
			t.Error("a page the API followed is not called a page")
		}
	})

	t.Run("a project list that fits one page is not called a page", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		if _, body, _ := get(t, handler, "/projects"); strings.Contains(body, "on this page") {
			t.Error("the whole registry is called a page")
		}
	})
}

// fleetForm cuts the fleet form out of a page, so a test can tell its inputs
// and hidden fields from the table filter's, which carries the window and the
// instant on purpose.
func fleetForm(t *testing.T, body string) string {
	t.Helper()

	start := strings.Index(body, `<form class="fleet-row"`)
	if start < 0 {
		t.Fatalf("the page carries no fleet form:\n%s", body)
	}
	end := strings.Index(body[start:], "</form>")
	if end < 0 {
		t.Fatalf("the fleet form does not end:\n%s", body)
	}
	return body[start : start+end]
}

// twoResources is the fleet the window tests read: the fixture's vm-1, alive
// since the first of March, beside vm-2, which lived from the third to the
// tenth.
func twoResources(t *testing.T) *fakeAPI {
	t.Helper()

	api := fullAPI(t)
	second := api.resources.Items[0]
	created := testPeriod.Add(2 * 24 * time.Hour)
	deleted := testPeriod.Add(9 * 24 * time.Hour)
	second.ResourceId, second.CreatedAt, second.DeletedAt, second.State = "vm-2", &created, &deleted, "deleted"
	api.resources.Items = append(api.resources.Items, second)
	return api
}

func TestFleetWindow(t *testing.T) {
	t.Parallel()

	t.Run("a window keeps what lived in it", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, twoResources(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?status=all&from=2026-03-02T00:00&to=2026-03-08T00:00")
		for _, want := range []string{
			">vm-1</a>",
			">vm-2</a>",
			"2 of 2 rows existed between 2026-03-02T00:00:00Z and 2026-03-08T00:00:00Z",
			`name="from" value="2026-03-02T00:00"`,
			`name="to" value="2026-03-08T00:00"`,
			`aria-current="true">over a window</a>`,
			// vm-2 lived a week whatever the window is.
			`<td class="number">168.00</td>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
		if form := fleetForm(t, body); strings.Contains(form, `name="at"`) {
			t.Errorf("the window asks for an instant as well:\n%s", form)
		}
		if strings.Contains(body, `>now</a>`) {
			t.Error("a window offers the instant now")
		}
	})

	t.Run("a window drops what ended before it or began after it", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, twoResources(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?status=all&from=2026-03-11T00:00:00Z&to=2026-03-12T00:00:00Z")
		if strings.Contains(body, ">vm-2</a>") || !strings.Contains(body, ">vm-1</a>") {
			t.Errorf("the window after vm-2 ended keeps the wrong rows:\n%s", body)
		}

		_, body, _ = get(t, handler, "/resources?status=all&from=2026-02-01T00:00:00Z&to=2026-02-15T00:00:00Z")
		if strings.Contains(body, ">vm-1</a>") ||
			!strings.Contains(body, "no resource of this page existed between 2026-02-01T00:00:00Z and 2026-02-15T00:00:00Z") {
			t.Errorf("the window before anything began keeps a row or does not say so:\n%s", body)
		}
	})

	t.Run("a window without a bound holds whatever lived", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, twoResources(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?status=all&mode=window")
		for _, want := range []string{
			">vm-1</a>", ">vm-2</a>",
			"2 of 2 rows existed at some time",
			`aria-current="true">any time</a>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
	})

	t.Run("a window ignores an instant and the switch drops it", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, twoResources(t), fullStore(t), testNow)

		// The instant would have dropped vm-2, which ended before it; the
		// window the request also names is what the page answers.
		_, body, _ := get(t, handler,
			"/resources?status=all&mode=window&from=2026-03-02T00:00:00Z&to=2026-03-08T00:00:00Z&at=2026-03-11T00:00:00Z")
		if !strings.Contains(body, ">vm-2</a>") ||
			!strings.Contains(body, "2 of 2 rows existed between 2026-03-02T00:00:00Z and 2026-03-08T00:00:00Z") {
			t.Errorf("the window did not answer alone:\n%s", body)
		}
		// The switch drops the window and keeps the instant of the request,
		// which is what the other way is read at.
		if !strings.Contains(body, `<a href="/resources?at=2026-03-11T00%3A00%3A00Z&amp;status=all">at an instant</a>`) {
			t.Errorf("the switch to the instant keeps the window:\n%s", body)
		}
	})

	t.Run("the switch to the window drops the instant", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, twoResources(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?status=all&at=2026-03-05T00:00:00Z")
		if !strings.Contains(body, `<a href="/resources?mode=window&amp;status=all">over a window</a>`) {
			t.Errorf("the switch to the window keeps the instant:\n%s", body)
		}
	})

	t.Run("a window open on one side filters on that side", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, twoResources(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?status=all&from=2026-03-05T00:00:00Z")
		for _, want := range []string{
			"2 of 2 rows existed since 2026-03-05T00:00:00Z",
			`name="to" value=""`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}

		_, body, _ = get(t, handler, "/resources?status=all&to=2026-03-02T00:00:00Z")
		if !strings.Contains(body, "1 of 2 rows existed before 2026-03-02T00:00:00Z") {
			t.Errorf("an open start keeps the wrong rows:\n%s", body)
		}
	})

	t.Run("a window that ends before it starts is refused", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		status, body, _ := get(t, handler, "/resources?from=2026-03-08T00:00&to=2026-03-02T00:00")
		if status != http.StatusBadRequest || !strings.Contains(body, "the parameter to is not after from") {
			t.Errorf("status = %d, body:\n%s", status, body)
		}
		status, body, _ = get(t, handler, "/resources?from=soon")
		if status != http.StatusBadRequest || !strings.Contains(body, "the parameter from is not an instant") {
			t.Errorf("status = %d, body:\n%s", status, body)
		}
	})

	t.Run("the window presets carry the page and mark the one in effect", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?mode=window&status=all&resources.q=vm")
		for _, want := range []string{
			`<a href="/resources?from=2026-03-01T00%3A00%3A00Z&amp;mode=window&amp;resources.q=vm&amp;status=all&amp;to=2026-04-01T00%3A00%3A00Z">this month</a>`,
			`<a href="/resources?from=2026-02-01T00%3A00%3A00Z&amp;mode=window&amp;resources.q=vm&amp;status=all&amp;to=2026-03-01T00%3A00%3A00Z">last month</a>`,
			`<a href="/resources?from=2026-03-08T12%3A00%3A00Z&amp;mode=window&amp;resources.q=vm&amp;status=all&amp;to=2026-03-15T12%3A00%3A00Z">last 7 days</a>`,
			`<a href="/resources?mode=window&amp;resources.q=vm&amp;status=all" aria-current="true">any time</a>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}

		_, body, _ = get(t, handler, "/resources?from=2026-03-01T00:00:00Z&to=2026-04-01T00:00:00Z")
		if !strings.Contains(body, `aria-current="true">this month</a>`) {
			t.Error("the window in effect is not marked")
		}
		if !strings.Contains(body, `<a href="/resources?mode=window">any time</a>`) {
			t.Error("any time does not clear the window")
		}
		form := fleetForm(t, body)
		if strings.Contains(form, `type="hidden" name="from"`) || strings.Contains(form, `type="hidden" name="to"`) {
			t.Error("the fleet form carries its own inputs hidden")
		}
	})

	t.Run("the next link keeps the window", func(t *testing.T) {
		t.Parallel()

		api := fullAPI(t)
		api.resources.NextCursor = pointerTo("abc")
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources?from=2026-03-01T00:00:00Z&to=2026-04-01T00:00:00Z")
		if !strings.Contains(body, `href="/resources?cursor=abc&amp;from=2026-03-01T00%3A00%3A00Z&amp;to=2026-04-01T00%3A00%3A00Z"`) {
			t.Errorf("the next link drops the window:\n%s", body)
		}
	})
}

// billDocument is a statement of two items, one that cost something and one
// that cost nothing, whose description only repeats its type and id.
func billDocument(t *testing.T) []byte {
	t.Helper()

	period := func(state, hours, vcpus, cost, modifier string) statements.Period {
		return statements.Period{
			State: state,
			Hours: money.NewAmount(mustDecimal(t, hours)),
			Usage: map[string]money.Quantity{"vcpus": money.NewQuantity(mustDecimal(t, vcpus))},
			Cost: map[string]money.Amount{
				"vcpus": money.NewAmount(mustDecimal(t, cost)),
				"total": money.NewAmount(mustDecimal(t, cost)),
			},
			StateModifier: money.NewQuantity(mustDecimal(t, modifier)),
		}
	}
	document := statements.Document{
		BillingPeriod: statements.BillingPeriod{From: "2026-03-01T00:00:00Z", To: "2026-04-01T00:00:00Z"},
		ProjectID:     "p-1",
		Platform:      "openstack",
		LineItems: []statements.LineItem{
			{
				ResourceType: "instance", ResourceID: "vm-3", Platform: "openstack",
				Description: "instance vm-3",
				Periods:     []statements.Period{period("shelved", "10", "2", "0", "0")},
				Total:       money.NewAmount(mustDecimal(t, "0")),
			},
			{
				ResourceType: "instance", ResourceID: "vm-4", Platform: "openstack",
				Description: "m1.small instance",
				Periods: []statements.Period{
					period("active", "24", "2", "12", "1"),
					period("shutoff", "6", "2", "1.5", "0.5"),
				},
				Total: money.NewAmount(mustDecimal(t, "13.5")),
			},
		},
		Total:    money.NewAmount(mustDecimal(t, "13.5")),
		Currency: "EUR",
	}

	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("building the statement document: %v", err)
	}
	return raw
}

func TestBill(t *testing.T) {
	t.Parallel()

	statementPath := "/statement?run=" + testRunID.String() + "&key=" + url.QueryEscape(testKey)

	billStore := func(t *testing.T) *fakeStore {
		t.Helper()

		engine := fullStore(t)
		engine.statement = store.Statement{Document: billDocument(t), Total: mustDecimal(t, "13.5"), Currency: "EUR"}
		return engine
	}

	t.Run("the summary opens sorted by total and leads to the items", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), billStore(t), testNow)

		_, body, _ := get(t, handler, statementPath)
		for _, want := range []string{
			`<th class="number" aria-sort="descending"><a href="/statement?items.sort=total&amp;key=os-sim%2Fp-1&amp;run=` +
				testRunID.String() + `">total</a></th>`,
			`<td><a href="/statement?key=os-sim%2Fp-1&amp;open=li-1&amp;run=` + testRunID.String() +
				`#li-1"><code>vm-4</code></a></td>`,
			`<td>m1.small instance</td>`,
			`<td class="number">30.00</td>`,
			`<td class="number">13.50 EUR</td>`,
			`<td class="number zero">0.00 EUR</td>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the summary lacks %q:\n%s", want, body)
			}
		}
		if strings.Index(body, `<code>vm-4</code>`) > strings.Index(body, `<code>vm-3</code>`) {
			t.Error("the item that cost something is not first")
		}
	})

	t.Run("every block starts folded and the link opens one", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), billStore(t), testNow)

		_, body, _ := get(t, handler, statementPath)
		if strings.Contains(body, " open>") {
			t.Errorf("a block starts unfolded:\n%s", body)
		}

		_, body, _ = get(t, handler, statementPath+"&open=li-1")
		for _, want := range []string{
			`<details class="item" id="li-1" open>`,
			`<details class="item" id="li-0">`,
			`<input type="hidden" name="open" value="li-1">`,
			`<summary><span>instance <code>vm-4</code></span> <span class="muted">m1.small instance</span><span class="total">13.50 EUR</span></summary>`,
			`<summary><span>instance <code>vm-3</code></span><span class="total zero">0.00 EUR</span></summary>`,
			`<a href="/resource?cloud=os-sim&amp;id=vm-4&amp;type=instance">the resource's page</a>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the blocks lack %q:\n%s", want, body)
			}
		}
		if strings.Contains(body, "<dt>description</dt>") || strings.Contains(body, "instance vm-3</span>") {
			t.Error("a description that repeats the heading is printed")
		}
	})

	t.Run("a period is one row per metric and a total", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), billStore(t), testNow)

		_, body, _ := get(t, handler, statementPath)
		for _, want := range []string{
			"<tr>\n<td>active</td>\n<td class=\"number\">24.00</td>\n<td>vcpus</td>\n" +
				"<td class=\"number\">2.0000</td>\n<td class=\"number\">12.00</td>\n<td class=\"number\">1.0000</td>\n</tr>",
			"<tr class=\"subtotal\">\n<td>active</td>\n<td class=\"number\">24.00</td>\n<td>total</td>\n" +
				"<td class=\"number zero\"></td>\n<td class=\"number\">12.00</td>\n<td class=\"number\">1.0000</td>\n</tr>",
			`<td class="number zero">0.00</td>`,
			`<th class="number"><a href="/statement?key=os-sim%2Fp-1&amp;li-1.sort=-cost&amp;run=` +
				testRunID.String() + `">cost</a></th>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the metric rows lack %q:\n%s", want, body)
			}
		}
	})

	t.Run("a metric table sorts on its own", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), billStore(t), testNow)

		_, body, _ := get(t, handler, statementPath+"&li-1.sort=cost")
		block := body[strings.Index(body, `id="li-1"`):]
		if strings.Index(block, "1.50") > strings.Index(block, "12.00") {
			t.Errorf("the cheapest row is not first:\n%s", block)
		}
	})

	t.Run("filtering the summary filters the blocks", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), billStore(t), testNow)

		_, body, _ := get(t, handler, statementPath+"&items.q=vm-4")
		if strings.Count(body, "<details") != 1 || !strings.Contains(body, `id="li-1"`) {
			t.Errorf("the blocks do not follow the filter:\n%s", body)
		}
		if !strings.Contains(body, "1 of 2 rows match") {
			t.Error("the summary does not count the filter")
		}
	})

	t.Run("the golden statement flattens its periods", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, statementPath)
		for _, want := range []string{
			`<td class="number">744.00</td>`,
			"<td>disk_gb</td>\n<td class=\"number\">80.0000</td>\n<td class=\"number\">19.20</td>",
			"<td>total</td>\n<td class=\"number zero\"></td>\n<td class=\"number\">49.62</td>",
			`<details class="item" id="li-0">`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the golden bill lacks %q:\n%s", want, body)
			}
		}
		if strings.Count(body, `<tr class="subtotal">`) != 3 {
			t.Errorf("the golden bill carries %d period totals, want 3", strings.Count(body, `<tr class="subtotal">`))
		}
	})

	t.Run("a related cost is a section of its own", func(t *testing.T) {
		t.Parallel()

		engine := fullStore(t)
		engine.statement = store.Statement{Document: relatedCostDocument(t), Total: mustDecimal(t, "12.00"), Currency: "EUR"}
		handler, _ := serve(t, fullAPI(t), engine, testNow)

		_, body, _ := get(t, handler, statementPath)
		for _, want := range []string{
			"<p>this section holds no line item</p>",
			`open=rc-0-li-0&amp;run=` + testRunID.String() + `#rc-0-li-0"><code>vm-2</code></a></td>`,
			`<details class="item" id="rc-0-li-0">`,
			`name="rc-0.q"`,
			`rc-0.sort=total`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the related section lacks %q:\n%s", want, body)
			}
		}
	})

	t.Run("a key the page cannot read links no resource", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), billStore(t), testNow)

		_, body, _ := get(t, handler, "/statement?run="+testRunID.String()+"&key=nokey")
		if strings.Contains(body, "the resource's page") {
			t.Error("a statement without a cloud links a resource")
		}
	})
}

func TestHistoryWithoutCreate(t *testing.T) {
	t.Parallel()

	// orphan is the fixture's resource without a creation, the way the API
	// serves a history that starts without a create.
	orphan := func(t *testing.T) *fakeAPI {
		t.Helper()

		api := fullAPI(t)
		api.resources.Items[0].CreatedAt = nil
		api.lifecycle.Resource.CreatedAt = nil
		api.lifecycle.Warnings = []string{"history_starts_without_create"}
		return api
	}

	t.Run("the listing says the creation and the lifetime are unknown", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, orphan(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resources")
		for _, want := range []string{
			"<td>unknown</td>\n<td>none</td>\n<td class=\"number\">unknown</td>",
			"1 row has no creation and no lifetime, because its history starts without a create</p>",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the listing lacks %q:\n%s", want, body)
			}
		}

		handler, _ = serve(t, fullAPI(t), fullStore(t), testNow)
		if _, body, _ = get(t, handler, "/resources"); strings.Contains(body, "no creation") {
			t.Error("a fleet with every creation known is told about unknown ones")
		}
	})

	t.Run("the resource page names the first event and counts from it", func(t *testing.T) {
		t.Parallel()

		api := orphan(t)
		later := api.lifecycle.Events[0]
		later.EventType, later.Timestamp = "instance.power_on", testPeriod.Add(48*time.Hour)
		// The later event is served first, and the page still finds the earliest.
		api.lifecycle.Events = []httpapi.StoredEvent{later, api.lifecycle.Events[0]}
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resource?cloud=os-sim&type=instance&id=vm-1")
		for _, want := range []string{
			"<dt>created</dt><dd>unknown, the history starts with instance.create.end at 2026-03-01T00:00:00Z</dd>",
			"<dt>lifetime</dt><dd>348.00 hours since the first event</dd>",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
	})

	t.Run("a resource with a creation counts from it", func(t *testing.T) {
		t.Parallel()

		handler, _ := serve(t, fullAPI(t), fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resource?cloud=os-sim&type=instance&id=vm-1")
		for _, want := range []string{
			"<dt>created</dt><dd>2026-03-01T00:00:00Z</dd>",
			"<dt>lifetime</dt><dd>348.00 hours</dd>",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the page lacks %q:\n%s", want, body)
			}
		}
	})

	t.Run("a history without any event is unknown throughout", func(t *testing.T) {
		t.Parallel()

		api := orphan(t)
		api.lifecycle.Events = nil
		handler, _ := serve(t, api, fullStore(t), testNow)

		_, body, _ := get(t, handler, "/resource?cloud=os-sim&type=instance&id=vm-1")
		if !strings.Contains(body, "<dt>created</dt><dd>unknown</dd>") ||
			!strings.Contains(body, "<dt>lifetime</dt><dd>unknown</dd>") {
			t.Errorf("the page guesses at a history it does not have:\n%s", body)
		}
	})
}

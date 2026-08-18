package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// The fixture the golden numbers are rated for: one project of one cloud, whose
// instances carry the history of the concept's section 3.4 example.
const (
	slicePlatform    = "openstack"
	sliceCloud       = "os-prod-eu1"
	sliceProject     = "proj-456"
	sliceMonth       = "2026-03"
	slicePricingPath = "../../pricing/prototype.yaml"
)

// sliceInternalToken is the shared secret the /internal routes of the test
// router are guarded with. The slice never calls one; the router requires the
// secret either way.
const sliceInternalToken = "internal-token-of-the-slice-test"

// sliceAPI is the Reporting API one test runs the slice against: where the
// server listens, and the query token the slice reads it with.
type sliceAPI struct {
	url   string
	token string
}

// newSliceAPI assembles the real API over a migrated database, seeds the two
// credentials it takes, and ingests the golden events through it. What the
// slice then reads has passed the contract validator, the ingest guard, the
// size schema, and the projection, which is what makes the numbers below a
// statement about the chain rather than about a fixture.
func newSliceAPI(t *testing.T) sliceAPI {
	t.Helper()

	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	handler, err := httpapi.NewRouter(httpapi.Options{
		Logger:                   slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:                       db.Store,
		UnhealthyThreshold:       time.Minute,
		Queries:                  q,
		Store:                    db.Store,
		AuthMode:                 auth.ModeEnforced,
		InternalToken:            sliceInternalToken,
		Authenticator:            auth.NewStaticTokenAuthenticator(q),
		Pipeline:                 ingest.New(registry.New(), false, nil, nil),
		AttributingRelationTypes: []string{"infrastructure_tenant"},
		Syncer: reconciliation.New(db.Store, ingest.New(registry.New(), false, nil, nil),
			reconciliation.Config{}, map[string]reconciliation.Adapter{}, time.Now, nil),
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v, want nil", err)
	}

	// Plain HTTP: the slice's TLS handling is the client's own test, and a
	// server whose certificate the run has to trust would only add a CA file to
	// this one.
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	ingestGoldenEvents(t, server.URL, seedIngestToken(t, q))

	return sliceAPI{url: server.URL, token: seedReadAllToken(t, q)}
}

// seedIngestToken issues the credential the golden batch is submitted with and
// stores its hash. It is scoped to the fixture's platform and cloud, which is
// what the ingest guard holds the batch against.
func seedIngestToken(t *testing.T, q *sqlcgen.Queries) string {
	t.Helper()

	token, err := auth.GenerateIngestToken()
	if err != nil {
		t.Fatalf("generating the ingest token: %v", err)
	}
	if _, err := q.CreateIngestCredential(t.Context(), sqlcgen.CreateIngestCredentialParams{
		Platform:    slicePlatform,
		Cloud:       sliceCloud,
		TokenHash:   auth.HashToken(token),
		Description: pgtype.Text{String: "vertical slice integration test", Valid: true},
	}); err != nil {
		t.Fatalf("CreateIngestCredential() error = %v, want nil", err)
	}
	return token
}

// seedReadAllToken issues the query token the slice reads with and stores its
// hash.
func seedReadAllToken(t *testing.T, q *sqlcgen.Queries) string {
	t.Helper()

	token, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("generating the api token: %v", err)
	}
	// project_ids is NOT NULL, so a token that names no project carries an empty
	// array rather than a nil slice, which pgx would send as NULL.
	if _, err := q.CreateAPIToken(t.Context(), sqlcgen.CreateAPITokenParams{
		TokenHash:   auth.HashToken(token),
		Role:        string(auth.RoleReadAll),
		ProjectIds:  []uuid.UUID{},
		Description: pgtype.Text{String: "vertical slice integration test", Valid: true},
	}); err != nil {
		t.Fatalf("CreateAPIToken() error = %v, want nil", err)
	}
	return token
}

// goldenEvent builds one event of the golden history. A nil size reports that
// the size did not change, which is what a power transition carries.
func goldenEvent(eventID, eventType, resourceID string, ts time.Time, state string, size map[string]any) event.Event {
	return event.Event{
		EventID:      eventID,
		Timestamp:    ts,
		EventType:    eventType,
		Platform:     slicePlatform,
		Cloud:        sliceCloud,
		ResourceType: "instance",
		ResourceID:   resourceID,
		ProjectID:    sliceProject,
		Source:       event.SourceCollector,
		Payload:      event.PayloadEnvelope{State: &state, Size: size},
	}
}

// goldenEvents is the history the concept's example rates: an instance created
// in February, powered off on 03-11 and on again on 03-21, next to one created
// on 03-01 and resized on 03-16. Two more carry the ends of a life: one deleted
// inside the month, which is how an instance's billing usually stops, and one
// that lived and died before it. Every size meets the (openstack, instance)
// schema the migration chain seeds.
func goldenEvents() []event.Event {
	large := map[string]any{"vcpus": 4, "ram_gb": 8, "disk_gb": 80, "flavor": "m1.large"}
	small := map[string]any{"vcpus": 2, "ram_gb": 4, "disk_gb": 40, "flavor": "m1.small"}

	return []event.Event{
		goldenEvent("vs-abc-123-create", "compute.instance.create.end", "abc-123",
			time.Date(2026, 2, 10, 8, 0, 0, 0, time.UTC), "active", large),
		goldenEvent("vs-abc-123-power-off", "compute.instance.power_off", "abc-123",
			time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC), "shutoff", nil),
		goldenEvent("vs-abc-123-power-on", "compute.instance.power_on", "abc-123",
			time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC), "active", nil),
		goldenEvent("vs-def-456-create", "compute.instance.create.end", "def-456",
			time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), "active", small),
		goldenEvent("vs-def-456-resize", "compute.instance.resize.end", "def-456",
			time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC), "active", large),
		goldenEvent("vs-ghi-789-create", "compute.instance.create.end", "ghi-789",
			time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC), "active", small),
		goldenEvent("vs-ghi-789-delete", "compute.instance.delete.end", "ghi-789",
			time.Date(2026, 3, 20, 0, 0, 0, 0, time.UTC), "deleted", nil),
		goldenEvent("vs-jkl-012-create", "compute.instance.create.end", "jkl-012",
			time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC), "active", small),
		goldenEvent("vs-jkl-012-delete", "compute.instance.delete.end", "jkl-012",
			time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC), "deleted", nil),
	}
}

// ingestGoldenEvents submits the whole history as one batch. A refused item is
// reported with the answer in full: the numbers the tests below assert only
// exist because the ingest took every event, so an ingest that took fewer has
// to be diagnosable from this failure alone.
func ingestGoldenEvents(t *testing.T, baseURL, token string) {
	t.Helper()

	events := goldenEvents()
	body, err := json.Marshal(events)
	if err != nil {
		t.Fatalf("marshaling the golden batch: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/api/v1/events", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building the ingest request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("submitting the golden batch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the ingest answer: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest status = %d, want %d (body %s)", resp.StatusCode, http.StatusOK, answer)
	}

	var result httpapi.IngestResult
	if err := json.Unmarshal(answer, &result); err != nil {
		t.Fatalf("decoding the ingest answer %s: %v", answer, err)
	}
	if result.Accepted != len(events) || len(result.Rejected) != 0 {
		t.Fatalf("ingest result = %+v, want %d accepted and nothing rejected (body %s)",
			result, len(events), answer)
	}
}

// The document as a test reads it back. Every number is a json.Number, so an
// assertion compares the digits the slice wrote rather than a value they were
// parsed into: what a customer reconciles against an invoice is the rendering,
// and 19.2 is not 19.20 there.
type sliceDocument struct {
	Cloud     string `json:"cloud"`
	ProjectID string `json:"project_id"`
	Period    struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"period"`
	Currency  string          `json:"currency"`
	Resources []sliceResource `json:"resources"`
	Total     json.Number     `json:"total"`
}

// sliceResource is one rated resource of the document.
type sliceResource struct {
	ResourceID string        `json:"resource_id"`
	Warnings   []string      `json:"warnings"`
	Violations []string      `json:"violations"`
	Records    []sliceRecord `json:"records"`
	Total      json.Number   `json:"total"`
}

// sliceRecord is one billed interval with its line items.
type sliceRecord struct {
	From       string                    `json:"from"`
	To         string                    `json:"to"`
	State      string                    `json:"state"`
	Seconds    int64                     `json:"seconds"`
	Minutes    json.Number               `json:"minutes"`
	Dimensions map[string]sliceDimension `json:"dimensions"`
	Subtotal   json.Number               `json:"subtotal"`
}

// sliceDimension is what one priced metric contributed to a record.
type sliceDimension struct {
	Quantity json.Number `json:"quantity"`
	Cost     json.Number `json:"cost"`
}

// wantResource is the golden rating of one resource. Every figure is pinned:
// the quantities come from the concept's example and the prices from the
// prototype file, which TestLoadPricingReadsThePrototype holds to its own
// numbers, so each cost below is determined by the two together.
type wantResource struct {
	resourceID string
	records    []wantRecord
	total      string
}

// wantRecord is one golden billed interval.
type wantRecord struct {
	from       string
	to         string
	state      string
	seconds    int64
	minutes    string
	dimensions map[string]wantDimension
	subtotal   string
}

// wantDimension is the golden quantity and cost of one priced metric.
type wantDimension struct {
	quantity string
	cost     string
}

// goldenResources is what the concept's section 3.4 example rates the March of
// the fixture at: abc-123 with its three intervals in EUR, def-456 with the two
// the resize splits its month into, and ghi-789 with the one its deletion ends.
// jkl-012 is not among them: it was gone before the month began, so the
// document leaves it out rather than rating it at zero.
func goldenResources() []wantResource {
	return []wantResource{
		{
			resourceID: "abc-123",
			records: []wantRecord{
				{
					from: "2026-03-01T00:00:00Z", to: "2026-03-11T00:00:00Z", state: "active",
					seconds: 864000, minutes: "14400.0000",
					dimensions: map[string]wantDimension{
						"vcpus":   {quantity: "4", cost: "19.20"},
						"ram_gb":  {quantity: "8", cost: "9.60"},
						"disk_gb": {quantity: "80", cost: "19.20"},
					},
					subtotal: "48.00",
				},
				{
					from: "2026-03-11T00:00:00Z", to: "2026-03-21T00:00:00Z", state: "shutoff",
					seconds: 864000, minutes: "14400.0000",
					dimensions: map[string]wantDimension{
						"vcpus":   {quantity: "4", cost: "9.60"},
						"ram_gb":  {quantity: "8", cost: "4.80"},
						"disk_gb": {quantity: "80", cost: "9.60"},
					},
					subtotal: "24.00",
				},
				{
					from: "2026-03-21T00:00:00Z", to: "2026-04-01T00:00:00Z", state: "active",
					seconds: 950400, minutes: "15840.0000",
					dimensions: map[string]wantDimension{
						"vcpus":   {quantity: "4", cost: "21.12"},
						"ram_gb":  {quantity: "8", cost: "10.56"},
						"disk_gb": {quantity: "80", cost: "21.12"},
					},
					subtotal: "52.80",
				},
			},
			total: "124.80",
		},
		{
			resourceID: "def-456",
			records: []wantRecord{
				{
					from: "2026-03-01T00:00:00Z", to: "2026-03-16T00:00:00Z", state: "active",
					seconds: 1296000, minutes: "21600.0000",
					dimensions: map[string]wantDimension{
						"vcpus":   {quantity: "2", cost: "14.40"},
						"ram_gb":  {quantity: "4", cost: "7.20"},
						"disk_gb": {quantity: "40", cost: "14.40"},
					},
					subtotal: "36.00",
				},
				{
					from: "2026-03-16T00:00:00Z", to: "2026-04-01T00:00:00Z", state: "active",
					seconds: 1382400, minutes: "23040.0000",
					dimensions: map[string]wantDimension{
						"vcpus":   {quantity: "4", cost: "30.72"},
						"ram_gb":  {quantity: "8", cost: "15.36"},
						"disk_gb": {quantity: "80", cost: "30.72"},
					},
					subtotal: "76.80",
				},
			},
			total: "112.80",
		},
		{
			resourceID: "ghi-789",
			records: []wantRecord{
				{
					from: "2026-03-05T00:00:00Z", to: "2026-03-20T00:00:00Z", state: "active",
					seconds: 1296000, minutes: "21600.0000",
					dimensions: map[string]wantDimension{
						"vcpus":   {quantity: "2", cost: "14.40"},
						"ram_gb":  {quantity: "4", cost: "7.20"},
						"disk_gb": {quantity: "40", cost: "14.40"},
					},
					subtotal: "36.00",
				},
			},
			total: "36.00",
		},
	}
}

// TestSliceReproducesTheGoldenNumbers is the CI-run half of the WP 1.15
// verification: the golden events go through the real ingest, auth, and query
// stack, and the document the slice prints is held against the concept's
// numbers value by value, the encoded precision included.
func TestSliceReproducesTheGoldenNumbers(t *testing.T) {
	api := newSliceAPI(t)

	var buf bytes.Buffer
	if err := run(t.Context(), config{
		cloud:        sliceCloud,
		project:      sliceProject,
		month:        sliceMonth,
		reportingURL: api.url,
		pricingPath:  slicePricingPath,
		token:        api.token,
	}, &buf); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}

	output := buf.String()
	doc := decodeSliceDocument(t, output)

	for _, field := range []struct{ name, got, want string }{
		{"cloud", doc.Cloud, sliceCloud},
		{"project_id", doc.ProjectID, sliceProject},
		{"currency", doc.Currency, "EUR"},
		{"period.from", doc.Period.From, "2026-03-01T00:00:00Z"},
		{"period.to", doc.Period.To, "2026-04-01T00:00:00Z"},
	} {
		if field.got != field.want {
			t.Errorf("%s = %q, want %q", field.name, field.got, field.want)
		}
	}

	want := goldenResources()
	if len(doc.Resources) != len(want) {
		t.Fatalf("the document rates %d resources, want %d:\n%s", len(doc.Resources), len(want), output)
	}
	for i, resource := range want {
		t.Run(resource.resourceID, func(t *testing.T) {
			assertResource(t, doc.Resources[i], resource)
		})
	}

	// The document's total is pinned rather than compared against the sum of the
	// resource totals it was built from: that comparison would hold the
	// implementation against itself and pass on any rating.
	assertNumber(t, "total", doc.Total, "273.60")
}

// TestSliceServesTheEmptyDocument rates a project the cloud has nothing under.
// The run is a success rather than a failure, and the document it prints is
// readable without a special case: a client iterates the resources of an empty
// month the way it iterates a busy one.
func TestSliceServesTheEmptyDocument(t *testing.T) {
	api := newSliceAPI(t)

	var buf bytes.Buffer
	if err := run(t.Context(), config{
		cloud:        sliceCloud,
		project:      "proj-none",
		month:        sliceMonth,
		reportingURL: api.url,
		pricingPath:  slicePricingPath,
		token:        api.token,
	}, &buf); err != nil {
		t.Fatalf("run() error = %v, want nil", err)
	}

	output := buf.String()
	// The rendering is asserted on the bytes: a nil slice marshals to null, and
	// a client that iterates the member without checking would fail on it.
	if !strings.Contains(output, `"resources": []`) {
		t.Errorf("the document does not carry resources as an empty array:\n%s", output)
	}

	doc := decodeSliceDocument(t, output)
	if len(doc.Resources) != 0 {
		t.Errorf("the document rates %d resources, want none:\n%s", len(doc.Resources), output)
	}
	assertNumber(t, "total", doc.Total, "0.00")
}

// decodeSliceDocument reads the printed document back, keeping every number as
// the digits the slice wrote.
func decodeSliceDocument(t *testing.T, output string) sliceDocument {
	t.Helper()

	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.UseNumber()

	var doc sliceDocument
	if err := decoder.Decode(&doc); err != nil {
		t.Fatalf("decoding the document:\n%s\nerror = %v", output, err)
	}
	return doc
}

// assertResource holds one rated resource against its golden form.
func assertResource(t *testing.T, got sliceResource, want wantResource) {
	t.Helper()

	if got.ResourceID != want.resourceID {
		t.Errorf("resource_id = %q, want %q", got.ResourceID, want.resourceID)
	}
	// A warning or a violation qualifies the numbers next to it, so a clean
	// resource has neither: the golden history folds without a gap and bills
	// exactly the part of the month it lived through.
	if len(got.Warnings) != 0 {
		t.Errorf("%s warnings = %v, want none", want.resourceID, got.Warnings)
	}
	if len(got.Violations) != 0 {
		t.Errorf("%s violations = %v, want none", want.resourceID, got.Violations)
	}

	if len(got.Records) != len(want.records) {
		t.Fatalf("%s carries %d records, want %d", want.resourceID, len(got.Records), len(want.records))
	}
	for i, record := range want.records {
		assertRecord(t, fmt.Sprintf("%s.records[%d]", want.resourceID, i), got.Records[i], record)
	}
	assertNumber(t, want.resourceID+".total", got.Total, want.total)
}

// assertRecord holds one billed interval against its golden form, naming the
// resource, the record, and the field of every mismatch.
func assertRecord(t *testing.T, where string, got sliceRecord, want wantRecord) {
	t.Helper()

	for _, field := range []struct{ name, got, want string }{
		{"from", got.From, want.from},
		{"to", got.To, want.to},
		{"state", got.State, want.state},
	} {
		if field.got != field.want {
			t.Errorf("%s.%s = %q, want %q", where, field.name, field.got, field.want)
		}
	}
	if got.Seconds != want.seconds {
		t.Errorf("%s.seconds = %d, want %d", where, got.Seconds, want.seconds)
	}
	assertNumber(t, where+".minutes", got.Minutes, want.minutes)
	assertNumber(t, where+".subtotal", got.Subtotal, want.subtotal)

	if len(got.Dimensions) != len(want.dimensions) {
		t.Fatalf("%s rates %d dimensions, want %d", where, len(got.Dimensions), len(want.dimensions))
	}
	for metric, dimension := range want.dimensions {
		rated, ok := got.Dimensions[metric]
		if !ok {
			t.Errorf("%s rates no %s", where, metric)
			continue
		}
		assertNumber(t, fmt.Sprintf("%s.dimensions[%s].quantity", where, metric), rated.Quantity, dimension.quantity)
		assertNumber(t, fmt.Sprintf("%s.dimensions[%s].cost", where, metric), rated.Cost, dimension.cost)
	}
}

// assertNumber compares one rendered number against its golden form.
func assertNumber(t *testing.T, where string, got json.Number, want string) {
	t.Helper()

	if string(got) != want {
		t.Errorf("%s = %s, want %s", where, got, want)
	}
}

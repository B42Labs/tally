// The golden suite's harness: the containers a run of the suite starts, the
// pair of databases each case gets, the fixtures a case is seeded from, and the
// assertions its expectations are checked with.
//
// Every case bills March 2026, [2026-03-01T00:00:00Z, 2026-04-01T00:00:00Z).
// The values in testdata/golden/<case>/expected.json are derived by hand from
// README section 3.4 ("Metering Output Examples") and pricing/2026-03.yaml.
// They are never regenerated from what the engine produced: a number the engine
// wrote says nothing about whether the engine is right.
//
// Events are inserted into the events table and folded into current_resources
// by projection.Replay, the writer the ingest path folds through, so a case is
// metered from the candidate index production derives. The metricsql querier is
// the only seam the suite substitutes, because VictoriaMetrics is another
// process; everything else a case runs through is the engine's own code.
package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/export"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
	enginestore "github.com/b42labs/tally/internal/engine/store"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
	enginetest "github.com/b42labs/tally/internal/engine/store/storetest"
	"github.com/b42labs/tally/internal/reporting/projection"
	"github.com/b42labs/tally/internal/reporting/registry"
	reportingstore "github.com/b42labs/tally/internal/reporting/store"
	reportingsqlcgen "github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	reportingtest "github.com/b42labs/tally/internal/reporting/store/storetest"
)

const (
	// goldenDir holds one directory per case, named after the case.
	goldenDir = "testdata/golden"
	// pricingFile is the model every case is rated with: the example model of
	// the concept, which is what the concept's amounts were computed from.
	pricingFile = "../../pricing/2026-03.yaml"
	// pricingVersion is the version that file declares, and what every run of
	// the suite has to report having rated with.
	pricingVersion = "2026-03"
	// currency is the currency that model quotes, carried by every record and
	// every statement it prices.
	currency = "EUR"
	// reportingPoolMaxConns bounds the pool a case opens on its own reporting
	// database. One case runs at a time and reads through one snapshot, so a
	// handful leaves the container's connection budget to the pools beside it.
	reportingPoolMaxConns = 4
	// maxBulkCount bounds the count of one bulk generator. The largest the
	// suite carries stands for 812 events, and seedEvents pays a round trip per
	// event, so anything past this is an extra digit rather than a case, and
	// expandBulk names it instead of spending minutes on it.
	maxBulkCount = 5_000
)

// The billing period every case meters: March 2026, 744 hours. It lies in the
// past, so no case runs into runs.WarningPeriodNotEnded, and it is written down
// rather than derived from the wall clock, because the amounts the cases assert
// are the concept's amounts over exactly this month.
var (
	periodFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodTo   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
)

// attributingRelationTypes is what attribution walks for every case, the one
// relation type the concept's related-costs example uses.
var attributingRelationTypes = []string{"infrastructure_tenant"}

// adjustmentRelationTypes and adjustmentDepth are what adjustment resolution
// walks for every case, the defaults of the deployment. The managed_by and
// member_of edges of virtual_relations carry no adjustments, so a run of the
// suite proves that walking them leaves every statement at the Phase 3 bytes.
var adjustmentRelationTypes = []string{"managed_by", "member_of"}

const adjustmentDepth = 3

// goldenFixture is the pair of containers the suite runs its cases in. The
// cases share them and take a database of their own out of each.
type goldenFixture struct {
	engine    enginetest.DB
	reporting reportingtest.DB
}

// newGoldenFixture starts both containers. The suite calls it once: a container
// per case, or one per test function, would pay for the image and the migration
// chain again for every one of them.
func newGoldenFixture(t *testing.T) goldenFixture {
	t.Helper()

	return goldenFixture{engine: enginetest.NewDB(t), reporting: reportingtest.NewDB(t)}
}

// caseDBs is the pair of databases one case runs against, and the source handle
// the engine reads the reporting side through.
type caseDBs struct {
	engine    *pgxpool.Pool
	reporting *pgxpool.Pool
	source    *source.DB
}

// caseDatabases creates, migrates and opens the two databases of one case, and
// imports the pricing model into the engine side.
//
// Each case gets its own pair rather than a cloud of its own in a shared one: a
// case that finalizes its period would refuse every regular run after it,
// whatever order the cases are run in.
func (f goldenFixture) caseDatabases(t *testing.T, name string) caseDBs {
	t.Helper()

	ctx := t.Context()

	engineURL := f.engine.NewSiblingDB(t, "golden_"+name)
	if _, err := enginestore.Migrate(ctx, engineURL); err != nil {
		t.Fatalf("migrating the engine database of %s: %v", name, err)
	}
	engineDB, err := enginestore.New(ctx, engineURL)
	if err != nil {
		t.Fatalf("opening the engine pool of %s: %v", name, err)
	}
	t.Cleanup(engineDB.Close)

	document, err := os.ReadFile(pricingFile)
	if err != nil {
		t.Fatalf("reading %s: %v", pricingFile, err)
	}
	model, doc, err := pricing.Parse(document)
	if err != nil {
		t.Fatalf("parsing %s: %v", pricingFile, err)
	}
	if _, err := pricing.Import(ctx, sqlcgen.New(engineDB.Pool()), model, doc); err != nil {
		t.Fatalf("importing %s: %v", pricingFile, err)
	}

	reportingURL := f.reporting.NewSiblingDB(t, "golden_"+name)
	if _, err := reportingstore.Migrate(ctx, reportingURL); err != nil {
		t.Fatalf("migrating the reporting database of %s: %v", name, err)
	}
	reportingDB, err := reportingstore.New(ctx, reportingURL, reportingPoolMaxConns)
	if err != nil {
		t.Fatalf("opening the reporting pool of %s: %v", name, err)
	}
	t.Cleanup(reportingDB.Close)

	src, err := source.New(ctx, reportingURL)
	if err != nil {
		t.Fatalf("opening the reporting source of %s: %v", name, err)
	}
	t.Cleanup(src.Close)

	return caseDBs{engine: engineDB.Pool(), reporting: reportingDB.Pool(), source: src}
}

// eventsFile is events.json, and late.json, which has the same shape: the
// events a case is seeded with, written out one by one, plus the generators
// that stand for the ones there are too many of to write out.
type eventsFile struct {
	Events []event.Event   `json:"events"`
	Bulk   []bulkGenerator `json:"bulk"`
}

// bulkGenerator stands for Count events of one type on one resource, spread
// over an interval. It is how a case seeds the hundreds of events an events
// counter counts without listing them.
type bulkGenerator struct {
	EventType    string                `json:"event_type"`
	Count        int                   `json:"count"`
	From         time.Time             `json:"from"`
	To           time.Time             `json:"to"`
	Platform     string                `json:"platform"`
	Cloud        string                `json:"cloud"`
	ResourceType string                `json:"resource_type"`
	ResourceID   string                `json:"resource_id"`
	ProjectID    string                `json:"project_id"`
	Payload      event.PayloadEnvelope `json:"payload"`
}

// registryFile is registry.json: the projects a case's resources are billed
// under, and the relations attribution walks between them.
type registryFile struct {
	Projects  []registryProject  `json:"projects"`
	Relations []registryRelation `json:"relations"`
}

// registryProject is one row of the project registry.
type registryProject struct {
	Platform   string `json:"platform"`
	Cloud      string `json:"cloud"`
	ExternalID string `json:"external_id"`
}

// registryRelation is one edge between two registered projects. ValidTo is nil
// where the file says null, which is an edge that is still open.
type registryRelation struct {
	Source       projectRef `json:"source"`
	Target       projectRef `json:"target"`
	RelationType string     `json:"relation_type"`
	ValidFrom    time.Time  `json:"valid_from"`
	ValidTo      *time.Time `json:"valid_to"`
}

// projectRef names a registered project the way a resource does, by cloud and
// external id, because the registry's own UUIDs are assigned at seeding time.
type projectRef struct {
	Cloud      string `json:"cloud"`
	ExternalID string `json:"external_id"`
}

// metricEntry is one stubbed answer of metrics.json: the rendered query, the
// instant it is asked at, and the value VictoriaMetrics would return.
type metricEntry struct {
	Query string    `json:"query"`
	At    time.Time `json:"at"`
	Value string    `json:"value"`
}

// goldenCase is one case's input, everything but its expectations.
type goldenCase struct {
	Name string
	Dir  string
	// Events is what the case is metered from.
	Events eventsFile
	// Registry is the projects and relations of the case.
	Registry registryFile
	// Counters is the counter sources file of the case, zero where it has none.
	Counters counters.Config
	// Metrics are the stubbed metricsql answers, empty where the case has no
	// metricsql source.
	Metrics []metricEntry
	// Late is late.json, the events a correction of the case re-meters. It is
	// nil for a case that has none.
	Late *eventsFile
}

// loadCase reads one case's fixtures. events.json and registry.json are
// required; counters.yaml, metrics.json and late.json are read where the case
// carries them.
func loadCase(t *testing.T, name string) goldenCase {
	t.Helper()

	dir := filepath.Join(goldenDir, name)
	c := goldenCase{Name: name, Dir: dir}

	eventsPath := filepath.Join(dir, "events.json")
	decodeJSON(t, eventsPath, readRequired(t, eventsPath), &c.Events)

	registryPath := filepath.Join(dir, "registry.json")
	decodeJSON(t, registryPath, readRequired(t, registryPath), &c.Registry)

	countersPath := filepath.Join(dir, "counters.yaml")
	if data, found := readOptional(t, countersPath); found {
		cfg, err := counters.Parse(data)
		if err != nil {
			t.Fatalf("decoding %s: %v", countersPath, err)
		}
		c.Counters = cfg
	}

	metricsPath := filepath.Join(dir, "metrics.json")
	if data, found := readOptional(t, metricsPath); found {
		decodeJSON(t, metricsPath, data, &c.Metrics)
	}

	latePath := filepath.Join(dir, "late.json")
	if data, found := readOptional(t, latePath); found {
		var late eventsFile
		decodeJSON(t, latePath, data, &late)
		c.Late = &late
	}

	return c
}

// expectedFile is expected.json: the clouds the run is restricted to and every
// number the case asserts.
type expectedFile struct {
	Clouds           []string            `json:"clouds"`
	Stats            expectedStats       `json:"stats"`
	Usage            []expectedUsage     `json:"usage"`
	Rated            []expectedRated     `json:"rated"`
	Statements       []expectedStatement `json:"statements"`
	AbsentStatements []string            `json:"absent_statements"`
}

// expectedStats is the four counts a run reports.
type expectedStats struct {
	Candidates   int `json:"candidates"`
	UsageRecords int `json:"usage_records"`
	RatedRecords int `json:"rated_records"`
	Statements   int `json:"statements"`
}

// expectedUsage is one usage_records row. Usage maps every key of the stored
// object to its value written as a string, numbers included, so that no
// expectation of the suite passes through a float.
type expectedUsage struct {
	Cloud        string            `json:"cloud"`
	Platform     string            `json:"platform"`
	ResourceType string            `json:"resource_type"`
	ResourceID   string            `json:"resource_id"`
	ProjectID    string            `json:"project_id"`
	State        string            `json:"state"`
	From         time.Time         `json:"from"`
	To           time.Time         `json:"to"`
	Seconds      int64             `json:"seconds"`
	Usage        map[string]string `json:"usage"`
}

// expectedRated is one rated record, named by the resource, the interval and
// the dimension it rates.
type expectedRated struct {
	Cloud      string          `json:"cloud"`
	ResourceID string          `json:"resource_id"`
	From       time.Time       `json:"from"`
	Dimension  string          `json:"dimension"`
	Quantity   decimal.Decimal `json:"quantity"`
	Amount     decimal.Decimal `json:"amount"`
}

// expectedStatement is one project statement and the document it carries.
type expectedStatement struct {
	Key           string                `json:"key"`
	Total         decimal.Decimal       `json:"total"`
	Currency      string                `json:"currency"`
	BillingPeriod expectedBillingPeriod `json:"billing_period"`
	ProjectID     string                `json:"project_id"`
	Platform      string                `json:"platform"`
	LineItems     []expectedLineItem    `json:"line_items"`
	RelatedCosts  []expectedRelatedCost `json:"related_costs"`
}

// expectedBillingPeriod is the interval a document says it bills, written the
// way the document renders it. It is what says the invoice covers the month the
// run was asked for rather than one beside it: no amount below it changes with
// the interval a document is stamped with.
type expectedBillingPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// expectedLineItem is one resource on a statement or on a related cost.
type expectedLineItem struct {
	ResourceID string           `json:"resource_id"`
	Total      decimal.Decimal  `json:"total"`
	Periods    []expectedPeriod `json:"periods"`
}

// expectedPeriod is one interval of a line item. Usage holds the quantity every
// dimension was rated from, and Cost one key per dimension plus "total", the
// way the document renders them.
type expectedPeriod struct {
	State         string                     `json:"state"`
	Hours         decimal.Decimal            `json:"hours"`
	StateModifier decimal.Decimal            `json:"state_modifier"`
	Usage         map[string]decimal.Decimal `json:"usage"`
	Cost          map[string]decimal.Decimal `json:"cost"`
}

// expectedRelatedCost is one attributed project on the statement of the project
// it is billed under.
type expectedRelatedCost struct {
	RelationType string             `json:"relation_type"`
	ProjectID    string             `json:"project_id"`
	Platform     string             `json:"platform"`
	Total        decimal.Decimal    `json:"total"`
	LineItems    []expectedLineItem `json:"line_items"`
}

// loadExpected reads one case's expectations, which every case carries.
func loadExpected(t *testing.T, name string) expectedFile {
	t.Helper()

	path := filepath.Join(goldenDir, name, "expected.json")
	var want expectedFile
	decodeJSON(t, path, readRequired(t, path), &want)
	return want
}

// readRequired reads a fixture file every case must carry. The error names the
// path and says what was wrong with it, an absent file included.
func readRequired(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

// readOptional reads a fixture file a case may leave out. found is false only
// for a file that is not there: a file that exists and cannot be read fails the
// case rather than passing for a case that configures nothing.
func readOptional(t *testing.T, path string) ([]byte, bool) {
	t.Helper()

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, false
	case err != nil:
		t.Fatalf("reading %s: %v", path, err)
	}
	return data, true
}

// decodeJSON decodes one fixture file into v, naming the file a failure is in.
func decodeJSON(t *testing.T, path string, data []byte, v any) {
	t.Helper()

	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
}

// seedEvents writes the events of a fixture and folds them into the projection.
// The generators are expanded first, so a bulk event is seeded and replayed the
// way a listed one is.
func seedEvents(t *testing.T, dbs caseDBs, file eventsFile) {
	t.Helper()

	events := append(slices.Clone(file.Events), expandBulk(t, file.Bulk)...)
	if len(events) == 0 {
		return
	}

	ctx := t.Context()
	sizes := registry.New()
	for _, ev := range events {
		// A fixture names a source only where the case is about one; every
		// other event arrived the way a collector's does.
		if ev.Source == "" {
			ev.Source = event.SourceCollector
		}
		if err := ev.Validate(); err != nil {
			t.Fatalf("event %s: %v", ev.EventID, err)
		}
		checkSize(t, dbs, sizes, ev)
		payload, err := json.Marshal(ev.Payload)
		if err != nil {
			t.Fatalf("event %s: encoding the payload: %v", ev.EventID, err)
		}
		// A delete event carries no payload at all. Stored as an empty object it
		// would be an event that reports a resource without a state rather than
		// one that reports nothing.
		var body any
		if !bytes.Equal(payload, []byte("{}")) {
			body = payload
		}
		if _, err := dbs.reporting.Exec(ctx,
			`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
			                     resource_type, resource_id, project_id, source, payload)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)`,
			ev.EventID, ev.Timestamp, ev.EventType, ev.Platform, ev.Cloud,
			ev.ResourceType, ev.ResourceID, ev.ProjectID, string(ev.Source), body); err != nil {
			t.Fatalf("seeding the event %s: %v", ev.EventID, err)
		}
	}

	replay(t, dbs, events)
}

// checkSize refuses a fixture event the ingest path would have dead-lettered.
// event.Validate checks the shape of an event and nothing about the size it
// reports; what decides whether an event enters the events table at all is the
// size schema registered for its (platform, resource_type), which ingest
// validates against before it stores anything (internal/reporting/ingest,
// checkSize). Those schemas are seeded by the reporting migrations the case
// databases run, so a fixture that would be refused in production must not be
// metered here either.
//
// A pair no row registers is taken unvalidated, which is what the lax pipeline
// does with it, and what the platforms of the suite the chain seeds no schema
// for rest on.
func checkSize(t *testing.T, dbs caseDBs, sizes *registry.Registry, ev event.Event) {
	t.Helper()

	if ev.Payload.Size == nil {
		return
	}

	schema, registered, err := sizes.Validator(t.Context(),
		reportingsqlcgen.New(dbs.reporting), ev.Platform, ev.ResourceType)
	if err != nil {
		t.Fatalf("event %s: reading the size schema: %v", ev.EventID, err)
	}
	if !registered {
		return
	}

	// The size goes through JSON on its way to the validator the way it does in
	// ingest: a keyword like "integer" is checked against the type the decoder
	// produced, not against the one a Go literal happens to carry.
	raw, err := json.Marshal(ev.Payload.Size)
	if err != nil {
		t.Fatalf("event %s: encoding the size: %v", ev.EventID, err)
	}
	size, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("event %s: decoding the size: %v", ev.EventID, err)
	}
	if err := schema.Validate(size); err != nil {
		t.Fatalf("event %s: size_schema: %v", ev.EventID, err)
	}
}

// expandBulk turns the generators of a fixture into the events they stand for.
// The timestamps divide each generator's interval into count+1 equal steps and
// sit on the inner boundaries, so every one of them lies strictly inside the
// interval and none falls on an instant the timeline splits at. A generator of
// count 0 expands to nothing.
//
// The id of an event names the generator it comes from whole: the cloud, the
// resource type and the resource id, the event type, both ends of the interval,
// and the count. A resource is keyed by the triple rather than by cloud and id,
// and the step the timestamps sit on is derived from the interval together with
// the count, so a generator that differs in any one of them stands for other
// events. Leaving one out lets two of them stand for the same ids, which the
// events table refuses on (event_id, timestamp) where the timestamps agree and
// silently files two histories under one id where they do not.
//
// A negative count, an interval too short for the count, and a count past
// maxBulkCount are typos in the fixture rather than cases: the first divides by
// zero, the second truncates the step to nothing and stamps every event the
// generator stands for on From, which is an instant the timeline splits at in
// every case the suite carries, and the third spends a round trip per event
// until a case that runs in seconds reads as a hung one. All three are named
// here rather than left to divide, to shift a counter's window, or to run out
// the clock without a fixture line to point at.
func expandBulk(t *testing.T, generators []bulkGenerator) []event.Event {
	t.Helper()

	var events []event.Event
	for _, g := range generators {
		if g.Count < 0 {
			t.Fatalf("the %s generator from %s: count = %d, want a count that is not negative",
				g.EventType, instant(g.From), g.Count)
		}
		if g.Count > maxBulkCount {
			t.Fatalf("the %s generator from %s: count = %d, want at most %d: "+
				"a larger one is a typo rather than a case",
				g.EventType, instant(g.From), g.Count, maxBulkCount)
		}
		step := g.To.Sub(g.From) / time.Duration(g.Count+1)
		if g.Count > 0 && step <= 0 {
			t.Fatalf("the %s generator: [%s, %s) is too short for %d events, "+
				"which would all be stamped on %s",
				g.EventType, instant(g.From), instant(g.To), g.Count, instant(g.From))
		}
		for i := range g.Count {
			events = append(events, event.Event{
				EventID: fmt.Sprintf("bulk-%s-%s-%s-%s-%s-%s-%d-%d",
					g.Cloud, g.ResourceType, g.ResourceID, g.EventType,
					g.From.UTC().Format(time.RFC3339), g.To.UTC().Format(time.RFC3339),
					g.Count, i),
				Timestamp:    g.From.Add(time.Duration(i+1) * step),
				EventType:    g.EventType,
				Platform:     g.Platform,
				Cloud:        g.Cloud,
				ResourceType: g.ResourceType,
				ResourceID:   g.ResourceID,
				ProjectID:    g.ProjectID,
				Payload:      g.Payload,
			})
		}
	}
	return events
}

// replay folds the seeded events into current_resources, once per resource they
// name, in the order the resources first appear. The suite never writes that
// table itself: the candidate index a case is metered from is the one the
// ingest path derives from the same history.
func replay(t *testing.T, dbs caseDBs, events []event.Event) {
	t.Helper()

	ctx := t.Context()
	tx, err := dbs.reporting.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the projection transaction: %v", err)
	}
	// Replay takes a transaction-scoped lock, so it needs one, and a rollback
	// after the commit below is the no-op the pool expects.
	defer func() { _ = tx.Rollback(ctx) }()

	seen := make(map[projection.Key]bool, len(events))
	for _, ev := range events {
		key := projection.Key{Cloud: ev.Cloud, ResourceType: ev.ResourceType, ResourceID: ev.ResourceID}
		if seen[key] {
			continue
		}
		seen[key] = true
		if err := projection.Replay(ctx, tx, key, nil); err != nil {
			t.Fatalf("replaying %s/%s/%s: %v", key.Cloud, key.ResourceType, key.ResourceID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("committing the projection transaction: %v", err)
	}
}

// seedRegistry registers the projects of a case and the relations between them.
// A project the registry does not hold gets no statement of its own, so every
// project a case's resources name belongs here.
func seedRegistry(t *testing.T, dbs caseDBs, reg registryFile) {
	t.Helper()

	ctx := t.Context()
	ids := make(map[projectRef]uuid.UUID, len(reg.Projects))
	for _, p := range reg.Projects {
		var id uuid.UUID
		if err := dbs.reporting.QueryRow(ctx,
			`INSERT INTO projects (platform, cloud, external_id) VALUES ($1, $2, $3) RETURNING id`,
			p.Platform, p.Cloud, p.ExternalID).Scan(&id); err != nil {
			t.Fatalf("registering the project %s/%s: %v", p.Cloud, p.ExternalID, err)
		}
		ids[projectRef{Cloud: p.Cloud, ExternalID: p.ExternalID}] = id
	}

	for _, r := range reg.Relations {
		edge := fmt.Sprintf("%s/%s -> %s/%s",
			r.Source.Cloud, r.Source.ExternalID, r.Target.Cloud, r.Target.ExternalID)
		// An end the registry does not hold is a typo in the fixture, and
		// naming the pair that is missing is what tells which of the two it is.
		resolve := func(ref projectRef) uuid.UUID {
			id, ok := ids[ref]
			if !ok {
				t.Fatalf("relation %s: no project %s/%s in registry.json", edge, ref.Cloud, ref.ExternalID)
			}
			return id
		}
		if _, err := dbs.reporting.Exec(ctx,
			`INSERT INTO project_relations (source_id, target_id, relation_type, valid_from, valid_to)
			 VALUES ($1, $2, $3, $4, $5)`,
			resolve(r.Source), resolve(r.Target), r.RelationType, r.ValidFrom, r.ValidTo); err != nil {
			t.Fatalf("relating %s: %v", edge, err)
		}
	}
}

// seedLate writes the events of late.json, which arrive after a case's first
// run has been finalized. A case without that file is a case whose test asked
// for something it does not carry.
func seedLate(t *testing.T, dbs caseDBs, c goldenCase) {
	t.Helper()

	if c.Late == nil {
		t.Fatalf("the case %s carries no late.json", c.Name)
	}
	seedEvents(t, dbs, *c.Late)
}

// queryKey is one stubbed answer's coordinates: the rendered expression and the
// instant it is asked at, which is the pair the engine calls Query with.
type queryKey struct{ expr, at string }

// stubQuerier answers the metricsql sources of a case from metrics.json.
// VictoriaMetrics is another process, so it is the one seam the suite
// substitutes, and the answers are written down beside the amounts they produce
// rather than measured.
type stubQuerier struct {
	values map[queryKey]decimal.Decimal
}

var _ counters.Querier = (*stubQuerier)(nil)

// newStubQuerier indexes the stubbed answers of a case by query and instant.
func newStubQuerier(t *testing.T, entries []metricEntry) *stubQuerier {
	t.Helper()

	values := make(map[queryKey]decimal.Decimal, len(entries))
	for _, entry := range entries {
		value, err := decimal.NewFromString(entry.Value)
		if err != nil {
			t.Fatalf("the stubbed value of %q: %v", entry.Query, err)
		}
		values[queryKey{expr: entry.Query, at: instant(entry.At)}] = value
	}
	return &stubQuerier{values: values}
}

// instant renders the instant a stubbed answer is keyed on and a failure names
// an interval by, in UTC so that two spellings of one instant meet.
func instant(at time.Time) string {
	return at.UTC().Format(time.RFC3339Nano)
}

// Query answers one instant query. A pair metrics.json does not hold is an
// error rather than a zero: a case whose window or whose end instant moved
// would otherwise be billed for a counter nobody wrote down.
func (s *stubQuerier) Query(ctx context.Context, expr string, at time.Time) (decimal.Decimal, error) {
	// A canceled run must be answered as canceled, whatever the map holds.
	if err := ctx.Err(); err != nil {
		return decimal.Zero, err
	}

	value, ok := s.values[queryKey{expr: expr, at: instant(at)}]
	if !ok {
		return decimal.Zero, fmt.Errorf("no stubbed metric for %q at %s", expr, instant(at))
	}
	return value, nil
}

// querier is what a case's metricsql sources are answered by, and an untyped
// nil for a case with none: a typed nil pointer would leave counters.New
// reporting a querier where the case has none.
func (c goldenCase) querier(t *testing.T) counters.Querier {
	t.Helper()

	if len(c.Metrics) == 0 {
		return nil
	}
	return newStubQuerier(t, c.Metrics)
}

// options is what a case is metered with. The clouds come from expected.json,
// beside the candidate count they produce.
func (c goldenCase) options(t *testing.T, clouds []string) runs.Options {
	t.Helper()

	return runs.Options{
		PeriodFrom:               periodFrom,
		PeriodTo:                 periodTo,
		Clouds:                   clouds,
		AttributingRelationTypes: attributingRelationTypes,
		AdjustmentRelationTypes:  adjustmentRelationTypes,
		AdjustmentDepth:          adjustmentDepth,
		Counters:                 c.Counters,
		VM:                       c.querier(t),
	}
}

// correctOptions is what a case is corrected with. A correction re-meters the
// period the finalized run recorded, whatever clouds that run was restricted
// to, so it takes no cloud list.
func (c goldenCase) correctOptions(t *testing.T) runs.CorrectOptions {
	t.Helper()

	return runs.CorrectOptions{
		PeriodFrom:               periodFrom,
		PeriodTo:                 periodTo,
		AttributingRelationTypes: attributingRelationTypes,
		AdjustmentRelationTypes:  adjustmentRelationTypes,
		AdjustmentDepth:          adjustmentDepth,
		Counters:                 c.Counters,
		VM:                       c.querier(t),
	}
}

// assertClean checks that the run rated with the suite's model and reported
// nothing to an operator.
func assertClean(t *testing.T, result runs.Result) {
	t.Helper()

	if result.PricingVersion != pricingVersion {
		t.Errorf("PricingVersion = %q, want %q", result.PricingVersion, pricingVersion)
	}
	assertNoWarnings(t, result.Stats)
}

// assertNoWarnings checks that one pass reported nothing to an operator. Every
// case of the suite is a case the engine has all the data for, so any warning is
// a finding. A correction re-meters the period whole, so every list a regular
// run fills is live on it too and is held to the same bar here.
func assertNoWarnings(t *testing.T, stats runs.Stats) {
	t.Helper()

	for _, list := range []struct {
		name    string
		entries any
		length  int
	}{
		{"warnings", stats.Warnings, len(stats.Warnings)},
		{"metering_warnings", stats.MeteringWarnings, len(stats.MeteringWarnings)},
		{"counter_warnings", stats.CounterWarnings, len(stats.CounterWarnings)},
		{"attribution_warnings", stats.AttributionWarnings, len(stats.AttributionWarnings)},
		{"adjustment_warnings", stats.AdjustmentWarnings, len(stats.AdjustmentWarnings)},
		{"unpriced", stats.Unpriced, len(stats.Unpriced)},
		{"unreadable", stats.Unreadable, len(stats.Unreadable)},
		{"unregistered_projects", stats.UnregisteredProjects, len(stats.UnregisteredProjects)},
		{"violations", stats.Violations, len(stats.Violations)},
	} {
		if list.length != 0 {
			t.Errorf("stats %s = %v, want none", list.name, list.entries)
		}
	}

	if stats.Error != "" {
		t.Errorf("stats error = %q, want none", stats.Error)
	}
}

// assertStats checks the four counts the run reported.
func assertStats(t *testing.T, got runs.Stats, want expectedStats) {
	t.Helper()

	for _, count := range []struct {
		name      string
		got, want int
	}{
		{"candidates", got.Candidates, want.Candidates},
		{"usage_records", got.UsageRecords, want.UsageRecords},
		{"rated_records", got.RatedRecords, want.RatedRecords},
		{"statements", got.Statements, want.Statements},
	} {
		if count.got != count.want {
			t.Errorf("stats %s = %d, want %d", count.name, count.got, count.want)
		}
	}
}

// storedUsage is one usage_records row as the suite reads it back, past the
// packages that wrote it.
type storedUsage struct {
	cloud, platform, resourceType, resourceID string
	projectID, state                          string
	fromTS, toTS                              time.Time
	seconds                                   int64
	usage                                     []byte
}

// assertUsage checks the usage records of a run against the expectation, in the
// order the query fixes. The intervals a case is split into are what every
// amount below is computed over, so this is the assertion the rest rests on.
func assertUsage(t *testing.T, dbs caseDBs, runID uuid.UUID, want []expectedUsage) {
	t.Helper()

	rows, err := dbs.engine.Query(t.Context(),
		`SELECT cloud, platform, resource_type, resource_id, project_id, state,
		        from_ts, to_ts, seconds, usage
		 FROM usage_records WHERE run_id = $1
		 ORDER BY cloud, resource_type, resource_id, from_ts`, runID)
	if err != nil {
		t.Fatalf("reading the usage records of run %s: %v", runID, err)
	}
	defer rows.Close()

	var got []storedUsage
	for rows.Next() {
		var row storedUsage
		if err := rows.Scan(&row.cloud, &row.platform, &row.resourceType, &row.resourceID,
			&row.projectID, &row.state, &row.fromTS, &row.toTS, &row.seconds, &row.usage); err != nil {
			t.Fatalf("scanning a usage record of run %s: %v", runID, err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the usage records of run %s: %v", runID, err)
	}

	if len(got) != len(want) {
		t.Fatalf("usage records = %d, want %d", len(got), len(want))
	}

	for i, w := range want {
		g := got[i]
		record := fmt.Sprintf("%s/%s from %s", g.cloud, g.resourceID, instant(g.fromTS))

		for _, field := range []struct{ name, got, want string }{
			{"cloud", g.cloud, w.Cloud},
			{"platform", g.platform, w.Platform},
			{"resource_type", g.resourceType, w.ResourceType},
			{"resource_id", g.resourceID, w.ResourceID},
			{"project_id", g.projectID, w.ProjectID},
			{"state", g.state, w.State},
		} {
			if field.got != field.want {
				t.Errorf("usage record %d: %s = %q, want %q", i, field.name, field.got, field.want)
			}
		}
		if !g.fromTS.Equal(w.From) {
			t.Errorf("usage record %d: from = %s, want %s", i, instant(g.fromTS), instant(w.From))
		}
		if !g.toTS.Equal(w.To) {
			t.Errorf("usage record %d: to = %s, want %s", i, instant(g.toTS), instant(w.To))
		}
		if g.seconds != w.Seconds {
			t.Errorf("usage record %d: seconds = %d, want %d", i, g.seconds, w.Seconds)
		}
		assertUsageObject(t, record, g.usage, w.Usage)
	}
}

// assertUsageObject compares one stored usage object against the expectation.
// The object carries the size the provider reported beside the quantities the
// engine derived, so a number is compared as a decimal and a string verbatim.
// Neither side may hold a key the other does not: a counter that stopped being
// measured would otherwise pass for a record that never carried it.
func assertUsageObject(t *testing.T, record string, raw []byte, want map[string]string) {
	t.Helper()

	// UseNumber keeps a stored quantity out of a float on the way to the
	// comparison, which is the one place the suite could lose digits.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var got map[string]any
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decoding the usage of %s: %v", record, err)
	}

	for key, value := range got {
		expected, listed := want[key]
		if !listed {
			t.Errorf("the usage of %s carries %s = %v, which expected.json does not list", record, key, value)
			continue
		}
		switch v := value.(type) {
		case json.Number:
			wantValue, err := decimal.NewFromString(expected)
			if err != nil {
				t.Errorf("the expected %s of %s: %v", key, record, err)
				continue
			}
			gotValue, err := decimal.NewFromString(v.String())
			if err != nil {
				t.Errorf("the stored %s of %s: %v", key, record, err)
				continue
			}
			if !gotValue.Equal(wantValue) {
				t.Errorf("the usage %s of %s = %s, want %s", key, record, v.String(), expected)
			}
		case string:
			if v != expected {
				t.Errorf("the usage %s of %s = %q, want %q", key, record, v, expected)
			}
		default:
			t.Errorf("the usage %s of %s is a %T, which is neither a number nor a string", key, record, value)
		}
	}

	for key := range want {
		if _, stored := got[key]; !stored {
			t.Errorf("the usage of %s carries no %s, which expected.json lists", record, key)
		}
	}
}

// ratedKey identifies one rated record among a run's: which resource, which
// interval, which dimension.
type ratedKey struct{ cloud, resourceID, from, dimension string }

// assertRated checks the amounts a run billed. The records are matched on what
// they rate rather than compared in order, so the listing's order is the
// export's concern and not every case's.
func assertRated(t *testing.T, got []export.RatedRecord, want []expectedRated) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("rated records = %d, want %d", len(got), len(want))
	}

	byKey := make(map[ratedKey]export.RatedRecord, len(got))
	for _, record := range got {
		byKey[ratedKey{
			cloud:      record.Resource.Cloud,
			resourceID: record.Resource.ResourceID,
			from:       instant(record.FromTS),
			dimension:  record.Dimension,
		}] = record
		if record.Currency != currency {
			t.Errorf("the currency of the %s of %s/%s = %q, want %q",
				record.Dimension, record.Resource.Cloud, record.Resource.ResourceID,
				record.Currency, currency)
		}
	}

	for _, w := range want {
		key := ratedKey{cloud: w.Cloud, resourceID: w.ResourceID, from: instant(w.From), dimension: w.Dimension}
		record, ok := byKey[key]
		if !ok {
			t.Errorf("no rated record for the %s of %s/%s from %s",
				w.Dimension, w.Cloud, w.ResourceID, key.from)
			continue
		}
		if !record.Quantity.Equal(w.Quantity) {
			t.Errorf("the %s quantity of %s/%s from %s = %s, want %s",
				w.Dimension, w.Cloud, w.ResourceID, key.from, record.Quantity, w.Quantity)
		}
		if !record.Amount.Equal(w.Amount) {
			t.Errorf("the %s amount of %s/%s from %s = %s, want %s",
				w.Dimension, w.Cloud, w.ResourceID, key.from, record.Amount, w.Amount)
		}
	}
}

// assertStatements checks the statements a run rendered, their totals and their
// documents, and that no key in absent got one. A project that is billed under
// another one has no statement of its own, and a suite that only checked the
// statements it expects would not see a second one beside them.
func assertStatements(
	t *testing.T,
	got []statements.Statement,
	want []expectedStatement,
	absent []string,
) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("statements = %d, want %d", len(got), len(want))
	}

	byKey := make(map[string]statements.Statement, len(got))
	for _, s := range got {
		byKey[s.Key] = s
	}

	for _, key := range absent {
		if _, billed := byKey[key]; billed {
			t.Errorf("the run billed a statement %s, which no project of the case may get", key)
		}
	}

	for _, w := range want {
		s, ok := byKey[w.Key]
		if !ok {
			t.Errorf("no statement %s", w.Key)
			continue
		}
		if !s.Total.Equal(w.Total) {
			t.Errorf("the total of %s = %s, want %s", w.Key, s.Total, w.Total)
		}
		if s.Currency != w.Currency {
			t.Errorf("the currency of %s = %q, want %q", w.Key, s.Currency, w.Currency)
		}

		var doc statements.Document
		if err := json.Unmarshal(s.Document, &doc); err != nil {
			t.Fatalf("decoding the document of %s: %v", w.Key, err)
		}
		assertBillingPeriod(t, w.Key, doc.BillingPeriod, w.BillingPeriod)
		for _, field := range []struct{ name, got, want string }{
			{"project", doc.ProjectID, w.ProjectID},
			{"platform", doc.Platform, w.Platform},
		} {
			if field.got != field.want {
				t.Errorf("the %s of %s = %q, want %q", field.name, w.Key, field.got, field.want)
			}
		}
		assertLineItems(t, w.Key, doc.LineItems, w.LineItems)
		assertRelatedCosts(t, w.Key, doc.RelatedCosts, w.RelatedCosts)
	}
}

// assertBillingPeriod checks the interval one document says it bills. A
// statement and a credit note carry the same pair, and both are wired from a
// period the caller passed rather than derived from what they bill, so neither
// is checked by any amount on it.
func assertBillingPeriod(t *testing.T, document string, got statements.BillingPeriod, want expectedBillingPeriod) {
	t.Helper()

	if got.From != want.From || got.To != want.To {
		t.Errorf("the billing period of %s = [%s, %s), want [%s, %s)",
			document, got.From, got.To, want.From, want.To)
	}
}

// assertLineItems compares the line items of one document or of one related
// cost. They are matched by resource id, which is what a line item is about.
func assertLineItems(t *testing.T, statement string, got []statements.LineItem, want []expectedLineItem) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("the line items of %s = %d, want %d", statement, len(got), len(want))
	}

	byID := make(map[string]statements.LineItem, len(got))
	for _, item := range got {
		byID[item.ResourceID] = item
	}

	for _, w := range want {
		item, ok := byID[w.ResourceID]
		if !ok {
			t.Fatalf("%s carries no line item for %s", statement, w.ResourceID)
		}
		if !item.Total.Equal(w.Total) {
			t.Errorf("the total of %s in %s = %s, want %s", w.ResourceID, statement, item.Total, w.Total)
		}
		assertPeriods(t, w.ResourceID+" in "+statement, item.Periods, w.Periods)
	}
}

// assertPeriods compares the periods of one line item in order: their place in
// that list is the order the resource changed in.
func assertPeriods(t *testing.T, item string, got []statements.Period, want []expectedPeriod) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("the periods of %s = %d, want %d", item, len(got), len(want))
	}

	for i, w := range want {
		g := got[i]
		if g.State != w.State {
			t.Errorf("period %d of %s: state = %q, want %q", i, item, g.State, w.State)
		}
		if !g.Hours.Equal(w.Hours) {
			t.Errorf("period %d of %s: hours = %s, want %s", i, item, g.Hours, w.Hours)
		}
		if !g.StateModifier.Equal(w.StateModifier) {
			t.Errorf("period %d of %s: state_modifier = %s, want %s", i, item, g.StateModifier, w.StateModifier)
		}

		for dimension, quantity := range g.Usage {
			expected, listed := w.Usage[dimension]
			if !listed {
				t.Errorf("period %d of %s was rated from %s = %s, which expected.json does not list",
					i, item, dimension, quantity)
				continue
			}
			if !quantity.Equal(expected) {
				t.Errorf("period %d of %s: the %s it was rated from = %s, want %s",
					i, item, dimension, quantity, expected)
			}
		}
		for dimension := range w.Usage {
			if _, rated := g.Usage[dimension]; !rated {
				t.Errorf("period %d of %s was rated from no %s, which expected.json lists",
					i, item, dimension)
			}
		}

		for dimension, amount := range g.Cost {
			expected, listed := w.Cost[dimension]
			if !listed {
				t.Errorf("period %d of %s costs %s = %s, which expected.json does not list",
					i, item, dimension, amount)
				continue
			}
			if !amount.Equal(expected) {
				t.Errorf("period %d of %s: the cost of %s = %s, want %s", i, item, dimension, amount, expected)
			}
		}
		for dimension := range w.Cost {
			if _, billed := g.Cost[dimension]; !billed {
				t.Errorf("period %d of %s costs no %s, which expected.json lists", i, item, dimension)
			}
		}
	}
}

// assertRelatedCosts compares the related costs of one document in order, which
// is the order attribution claimed the projects in.
func assertRelatedCosts(
	t *testing.T,
	statement string,
	got []statements.RelatedCost,
	want []expectedRelatedCost,
) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("the related costs of %s = %d, want %d", statement, len(got), len(want))
	}

	for i, w := range want {
		g := got[i]
		for _, field := range []struct{ name, got, want string }{
			{"relation_type", g.RelationType, w.RelationType},
			{"project_id", g.ProjectID, w.ProjectID},
			{"platform", g.Platform, w.Platform},
		} {
			if field.got != field.want {
				t.Errorf("related cost %d of %s: %s = %q, want %q",
					i, statement, field.name, field.got, field.want)
			}
		}
		if !g.Total.Equal(w.Total) {
			t.Errorf("related cost %d of %s: total = %s, want %s", i, statement, g.Total, w.Total)
		}
		assertLineItems(t, fmt.Sprintf("the related cost %s of %s", w.ProjectID, statement),
			g.LineItems, w.LineItems)
	}
}

// runStatus is the status one run row carries.
func runStatus(t *testing.T, dbs caseDBs, id uuid.UUID) string {
	t.Helper()

	var status string
	if err := dbs.engine.QueryRow(t.Context(),
		`SELECT status FROM runs WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("reading the status of run %s: %v", id, err)
	}
	return status
}

// periodRow is the status of the case's billing period and the run that closed
// it, which is the zero id while the period is still open.
func periodRow(t *testing.T, dbs caseDBs) (status string, finalizedRun uuid.UUID) {
	t.Helper()

	var finalized *uuid.UUID
	if err := dbs.engine.QueryRow(t.Context(),
		`SELECT status, finalized_run_id FROM billing_periods WHERE period_from = $1`,
		periodFrom).Scan(&status, &finalized); err != nil {
		t.Fatalf("reading the billing period %s: %v", instant(periodFrom), err)
	}
	if finalized != nil {
		finalizedRun = *finalized
	}
	return status, finalizedRun
}

// countRuns is how many runs the case's period holds, which is what a refused
// call has to leave at what it was.
func countRuns(t *testing.T, dbs caseDBs) int {
	t.Helper()

	var count int
	if err := dbs.engine.QueryRow(t.Context(),
		`SELECT count(*) FROM runs WHERE period_from = $1`, periodFrom).Scan(&count); err != nil {
		t.Fatalf("counting the runs of %s: %v", instant(periodFrom), err)
	}
	return count
}

// countRows is how many rows of one run a table holds. The table name is
// interpolated because it is a constant of the calling case, one of
// usage_records, rated_records and project_statements, and no placeholder
// stands for an identifier.
func countRows(t *testing.T, dbs caseDBs, table string, runID uuid.UUID) int {
	t.Helper()

	var count int
	if err := dbs.engine.QueryRow(t.Context(),
		fmt.Sprintf(`SELECT count(*) FROM %s WHERE run_id = $1`, table), runID).Scan(&count); err != nil {
		t.Fatalf("counting the %s of run %s: %v", table, runID, err)
	}
	return count
}

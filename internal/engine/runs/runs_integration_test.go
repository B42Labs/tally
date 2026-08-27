package runs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
	enginetest "github.com/b42labs/tally/internal/engine/store/storetest"
	reportingtest "github.com/b42labs/tally/internal/reporting/store/storetest"
)

// The platform and resource type every case meters. One priced resource type is
// enough: what these cases are about is the lifecycle around the passes, not
// what those passes compute.
const (
	platform     = "openstack"
	resourceType = "instance"
)

// standardSize is the size of a resource the cases meter, and largeSize is one
// whose quantities rate into amounts no column holds.
const (
	standardSize = `{"vcpus":4,"ram_gb":8,"disk_gb":80,"flavor":"m1.large"}`
	largeSize    = `{"vcpus":625000000000,"ram_gb":2500000000000}`
)

// resellerAdjustments is what a relation to a reseller carries: 15 percent off
// what the customer is billed, and 10 percent of what is left owed to the
// partner. discountAdjustments is the one discount the cases that need nothing
// else take.
const (
	resellerAdjustments = `{"pricing_adjustments":[` +
		`{"type":"discount","rate":"0.15","scope":"all"},` +
		`{"type":"kickback","rate":"0.10","scope":"all"}]}`
	discountAdjustments = `{"pricing_adjustments":[{"type":"discount","rate":"0.10","scope":"all"}]}`
)

// adjustmentDepth is how deep the cases that resolve adjustments walk the
// project graph, and adjustmentRelationTypes are the relation types they walk.
// Three levels is past every graph the cases seed, and an empty type list is
// adjustments turned off, which one of the cases runs with.
const adjustmentDepth = 3

var adjustmentRelationTypes = []string{"managed_by", "member_of"}

// pricingDocument is the model every case is rated with. valid_from is filled
// in with an instant before every period the cases meter.
const pricingDocument = `version: "v1"
valid_from: %q
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.5"
`

// correctionPricingDocument is the model the correction cases are rated with:
// the three time_gauge dimensions of the concept's example, and the state
// modifier that halves a powered-off instance, which is what a late power cycle
// changes the amounts by.
const correctionPricingDocument = `version: "v1"
valid_from: %q
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.001"
      state_modifiers:
        shutoff: "0.5"
`

// month is the UTC billing month offset months from the one the test runs in:
// -3 is three months back, 0 the current one. The periods are derived from the
// wall clock rather than written down, because whether a period has ended is
// what one of the cases below is about and what the others must not run into.
func month(offset int) (from, to time.Time) {
	now := time.Now().UTC()
	from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, offset, 0)
	return from, from.AddDate(0, 1, 0)
}

// thirtyOneDayMonth is the n-th most recent month of 31 days, counted back from
// two months ago: n = 1 is the first such month, n = 2 the one before it. The
// cases that bill a resource for a whole month need one, because 744 hours is
// what the concept's amounts were computed over, and which offset carries 31
// days moves with the wall clock the periods are derived from.
//
// The walk starts at offset -2 so that no month it returns is the one a test
// runs in or the one before it. Seven months of twelve have 31 days, so the
// fifth one is at most eight offsets further back, which is what the offsets
// the other cases pick stay clear of.
func thirtyOneDayMonth(n int) (from, to time.Time) {
	for offset := -2; ; offset-- {
		from, to = month(offset)
		if to.Sub(from) != 31*24*time.Hour {
			continue
		}
		n--
		if n == 0 {
			return from, to
		}
	}
}

// fixture is the pair of databases the cases run against: the engine database a
// run is written to, and the reporting database it is read from, with one
// pricing model imported.
type fixture struct {
	engine    enginetest.DB
	reporting reportingtest.DB
	source    *source.DB
}

// newFixture starts both databases and imports the model every case of a run is
// rated with. It is called once per test function: the cases inside share the
// containers and keep to their own period and their own cloud.
func newFixture(t *testing.T) fixture {
	t.Helper()

	return newFixtureWith(t, pricingDocument)
}

// newFixtureWith starts both databases and imports the pricing model the test
// passes, whose valid_from is filled in with an instant before every period the
// cases meter.
func newFixtureWith(t *testing.T, document string) fixture {
	t.Helper()

	engineDB := enginetest.NewDB(t)
	reportingDB := reportingtest.NewDB(t)

	validFrom, _ := month(-36)
	model, doc, err := pricing.Parse([]byte(fmt.Sprintf(document, validFrom.Format(time.RFC3339))))
	if err != nil {
		t.Fatalf("parsing the pricing model: %v", err)
	}
	if _, err := pricing.Import(t.Context(), sqlcgen.New(engineDB.Store.Pool()), model, doc); err != nil {
		t.Fatalf("importing the pricing model: %v", err)
	}

	src, err := source.New(t.Context(), reportingDB.URL)
	if err != nil {
		t.Fatalf("opening the reporting source: %v", err)
	}
	t.Cleanup(src.Close)

	return fixture{engine: engineDB, reporting: reportingDB, source: src}
}

// execute runs one period against the fixture, on the test's own context. The
// one case that needs a context it can cancel calls runs.Execute itself.
func (f fixture) execute(t *testing.T, opts runs.Options) (runs.Result, error) {
	t.Helper()

	return runs.Execute(t.Context(), f.engine.Store.Pool(), f.source, opts)
}

// snapshotTime is the instant a snapshot taken now carries. Two of them bracket
// the snapshot_at a run stored.
func (f fixture) snapshotTime(t *testing.T) time.Time {
	t.Helper()

	snap, err := f.source.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("opening a snapshot: %v", err)
	}
	defer func() {
		if err := snap.Close(t.Context()); err != nil {
			t.Errorf("closing the snapshot: %v", err)
		}
	}()
	return snap.At
}

// resource is one instance a case meters. It is created and deleted inside its
// own period, which is what confines it to that period: the candidate query
// selects on exactly those two instants, so the cases share one reporting
// database without seeing each other's resources.
type resource struct {
	cloud, id, project string
	created, deleted   time.Time
	size               string
}

// instance describes the resource of one case over the period it is metered in:
// alive from the period's second day to its fourth, which is 48 hours.
func instance(cloud, id, project string, from time.Time, size string) resource {
	return resource{
		cloud: cloud, id: id, project: project,
		created: from.Add(24 * time.Hour), deleted: from.Add(72 * time.Hour),
		size: size,
	}
}

// wholeMonth describes a resource that bills the whole of the period it is
// metered in: created a day before it and deleted a day after it. The delete
// event lies at or past the period end, which is where History stops reading,
// so the fold leaves the resource alive and metering clips its one interval to
// the period. It is what the concept's 744-hour amounts are metered from.
func wholeMonth(cloud, id, project string, from, to time.Time, size string) resource {
	return resource{
		cloud: cloud, id: id, project: project,
		created: from.Add(-24 * time.Hour), deleted: to.Add(24 * time.Hour),
		size: size,
	}
}

// seedResource writes the projection row and the create and delete events of
// one resource.
func (f fixture) seedResource(t *testing.T, r resource) {
	t.Helper()

	f.seedCandidate(t, r)
	f.seedEvent(t, r, "ev-create-"+r.id, "compute.instance.create.end", r.created,
		`{"state":"active","size":`+r.size+`}`)
	f.seedEvent(t, r, "ev-delete-"+r.id, "compute.instance.delete.end", r.deleted, "")
}

// seedCandidate writes the projection row alone, which is a candidate the
// history of is missing.
func (f fixture) seedCandidate(t *testing.T, r resource) {
	t.Helper()

	if _, err := f.reporting.Store.Pool().Exec(t.Context(),
		`INSERT INTO current_resources (cloud, platform, resource_type, resource_id, project_id,
		                                state, size, created_at, deleted_at, last_event_type, last_event_at)
		 VALUES ($1, $2, $3, $4, $5, 'deleted', $6::jsonb, $7, $8, 'compute.instance.delete.end', $8)`,
		r.cloud, platform, resourceType, r.id, r.project, r.size, r.created, r.deleted); err != nil {
		t.Fatalf("seeding the projection row of %s: %v", r.id, err)
	}
}

// seedEvent writes one event of a resource. An empty payload is stored as NULL,
// which reports neither a state nor a size.
func (f fixture) seedEvent(t *testing.T, r resource, eventID, eventType string, ts time.Time, payload string) {
	t.Helper()

	var body any
	if payload != "" {
		body = payload
	}
	if _, err := f.reporting.Store.Pool().Exec(t.Context(),
		`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
		                     resource_type, resource_id, project_id, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)`,
		eventID, ts, eventType, platform, r.cloud, resourceType, r.id, r.project, body); err != nil {
		t.Fatalf("seeding the event %s: %v", eventID, err)
	}
}

// seedEventReceived writes one event with the instant it reached the reporting
// database, which the column otherwise defaults to now(). An event received
// after a run took its snapshot is what that run never saw and what a
// correction of the period re-meters.
func (f fixture) seedEventReceived(
	t *testing.T,
	r resource,
	eventID, eventType string,
	ts, receivedAt time.Time,
	payload string,
) {
	t.Helper()

	var body any
	if payload != "" {
		body = payload
	}
	if _, err := f.reporting.Store.Pool().Exec(t.Context(),
		`INSERT INTO events (event_id, timestamp, received_at, event_type, platform, cloud,
		                     resource_type, resource_id, project_id, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)`,
		eventID, ts, receivedAt, eventType, platform, r.cloud, resourceType,
		r.id, r.project, body); err != nil {
		t.Fatalf("seeding the event %s: %v", eventID, err)
	}
}

// seedProject registers a project, which is what gets its statement a document
// keyed to the registry rather than an entry in unregistered_projects.
func (f fixture) seedProject(t *testing.T, cloud, externalID string) {
	t.Helper()

	if _, err := f.reporting.Store.Pool().Exec(t.Context(),
		`INSERT INTO projects (platform, cloud, external_id) VALUES ($1, $2, $3)`,
		platform, cloud, externalID); err != nil {
		t.Fatalf("seeding the project %s: %v", externalID, err)
	}
}

// seedVirtualProject registers a project that owns no resources: a partner or
// a meta-project. Its cloud is its platform, which is what a virtual project
// carries there, and it returns the registry id a relation names it by.
func (f fixture) seedVirtualProject(t *testing.T, platform, externalID string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := f.reporting.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO projects (platform, cloud, external_id) VALUES ($1, $1, $2) RETURNING id`,
		platform, externalID).Scan(&id); err != nil {
		t.Fatalf("seeding the %s project %s: %v", platform, externalID, err)
	}
	return id
}

// projectIDOf is the registry id of one project, which a relation names and
// seedProject does not report.
func (f fixture) projectIDOf(t *testing.T, cloud, externalID string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := f.reporting.Store.Pool().QueryRow(t.Context(),
		`SELECT id FROM projects WHERE cloud = $1 AND external_id = $2`,
		cloud, externalID).Scan(&id); err != nil {
		t.Fatalf("reading the id of the project %s/%s: %v", cloud, externalID, err)
	}
	return id
}

// seedRelation writes one edge of the project graph and returns its id. An
// empty metadata is the empty document a relation created without one carries,
// and a nil validTo leaves the relation open. The two validity instants are
// what fixes a relation to the periods it adjusts (D4).
func (f fixture) seedRelation(
	t *testing.T,
	sourceID, targetID uuid.UUID,
	relationType, metadata string,
	validFrom time.Time,
	validTo *time.Time,
) uuid.UUID {
	t.Helper()

	if metadata == "" {
		metadata = "{}"
	}
	var id uuid.UUID
	if err := f.reporting.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO project_relations (source_id, target_id, relation_type, metadata, valid_from, valid_to)
		 VALUES ($1, $2, $3, $4::jsonb, $5, $6) RETURNING id`,
		sourceID, targetID, relationType, metadata, validFrom, validTo).Scan(&id); err != nil {
		t.Fatalf("seeding the %s relation of %s: %v", relationType, sourceID, err)
	}
	return id
}

// runRow is a runs row as the cases read it back, past the package under test.
// corrects_run_id is nullable, and a regular run reads as the zero id.
type runRow struct {
	kind           string
	correctsRunID  uuid.UUID
	status         string
	pricingVersion string
	clouds         []string
	startedAt      time.Time
	completedAt    *time.Time
}

// readRun reads one run row.
func (f fixture) readRun(t *testing.T, runID uuid.UUID) runRow {
	t.Helper()

	var row runRow
	var corrects *uuid.UUID
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT kind, corrects_run_id, status, pricing_version, clouds, started_at, completed_at
		 FROM runs WHERE id = $1`, runID,
	).Scan(&row.kind, &corrects, &row.status, &row.pricingVersion, &row.clouds,
		&row.startedAt, &row.completedAt); err != nil {
		t.Fatalf("reading run %s: %v", runID, err)
	}
	if corrects != nil {
		row.correctsRunID = *corrects
	}
	return row
}

// countRuns is how many runs the period holds, which is what a refused call has
// to leave at what it was.
func (f fixture) countRuns(t *testing.T, from time.Time) int {
	t.Helper()

	var count int
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM runs WHERE period_from = $1`, from).Scan(&count); err != nil {
		t.Fatalf("counting the runs of %s: %v", from.Format(time.RFC3339), err)
	}
	return count
}

// usageRow is a usage_records row as the cases read it back.
type usageRow struct {
	id        uuid.UUID
	projectID string
	state     string
	fromTS    time.Time
	toTS      time.Time
	seconds   int64
	usage     []byte
}

// readUsage reads a run's usage records, ordered the way they were written.
func (f fixture) readUsage(t *testing.T, runID uuid.UUID) []usageRow {
	t.Helper()

	rows, err := f.engine.Store.Pool().Query(t.Context(),
		`SELECT id, project_id, state, from_ts, to_ts, seconds, usage
		 FROM usage_records WHERE run_id = $1 ORDER BY from_ts, resource_id`, runID)
	if err != nil {
		t.Fatalf("reading the usage records of run %s: %v", runID, err)
	}
	defer rows.Close()

	var records []usageRow
	for rows.Next() {
		var row usageRow
		if err := rows.Scan(&row.id, &row.projectID, &row.state, &row.fromTS,
			&row.toTS, &row.seconds, &row.usage); err != nil {
			t.Fatalf("scanning a usage record of run %s: %v", runID, err)
		}
		records = append(records, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the usage records of run %s: %v", runID, err)
	}
	return records
}

// ratedRow is a rated_records row as the cases read it back. The amount is read
// as text, which is what keeps the assertion off floats
// (roadmap/00-conventions.md section 6).
type ratedRow struct {
	dimension     string
	amount        string
	currency      string
	usageRecordID uuid.UUID
}

// readRated reads a run's rated records, ordered by dimension.
func (f fixture) readRated(t *testing.T, runID uuid.UUID) []ratedRow {
	t.Helper()

	rows, err := f.engine.Store.Pool().Query(t.Context(),
		`SELECT dimension, amount::text, currency, usage_record_id
		 FROM rated_records WHERE run_id = $1 ORDER BY dimension`, runID)
	if err != nil {
		t.Fatalf("reading the rated records of run %s: %v", runID, err)
	}
	defer rows.Close()

	var records []ratedRow
	for rows.Next() {
		var row ratedRow
		if err := rows.Scan(&row.dimension, &row.amount, &row.currency, &row.usageRecordID); err != nil {
			t.Fatalf("scanning a rated record of run %s: %v", runID, err)
		}
		records = append(records, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the rated records of run %s: %v", runID, err)
	}
	return records
}

// readStatements reads the keys of a run's statements, ordered by key.
func (f fixture) readStatements(t *testing.T, runID uuid.UUID) []string {
	t.Helper()

	rows, err := f.engine.Store.Pool().Query(t.Context(),
		`SELECT project_id FROM project_statements WHERE run_id = $1 ORDER BY project_id`, runID)
	if err != nil {
		t.Fatalf("reading the statements of run %s: %v", runID, err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("scanning a statement of run %s: %v", runID, err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the statements of run %s: %v", runID, err)
	}
	return keys
}

// adjustmentRow is an adjustment_records row as the cases read it back. The
// rate and the two amounts are read as text, which is what keeps the assertion
// off floats (roadmap/00-conventions.md section 6), and so is the relation id,
// because that is what a case compares it against. beneficiary is nil on every
// row but a kickback's.
type adjustmentRow struct {
	projectID      string
	relationID     string
	relationType   string
	relationTarget string
	beneficiary    *string
	typ            string
	scope          string
	rate           string
	base           string
	amount         string
	currency       string
}

// readAdjustments reads a run's adjustment records, in the order
// ListAdjustmentRecords returns them, which is the order a correction diffs
// them in.
func (f fixture) readAdjustments(t *testing.T, runID uuid.UUID) []adjustmentRow {
	t.Helper()

	rows, err := f.engine.Store.Pool().Query(t.Context(),
		`SELECT project_id, relation_id::text, relation_type, relation_target, beneficiary,
		        type, scope, rate::text, base::text, amount::text, currency
		 FROM adjustment_records WHERE run_id = $1
		 ORDER BY project_id, relation_id, type, scope, rate, amount`, runID)
	if err != nil {
		t.Fatalf("reading the adjustment records of run %s: %v", runID, err)
	}
	defer rows.Close()

	var records []adjustmentRow
	for rows.Next() {
		var row adjustmentRow
		if err := rows.Scan(&row.projectID, &row.relationID, &row.relationType, &row.relationTarget,
			&row.beneficiary, &row.typ, &row.scope, &row.rate, &row.base, &row.amount,
			&row.currency); err != nil {
			t.Fatalf("scanning an adjustment record of run %s: %v", runID, err)
		}
		records = append(records, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the adjustment records of run %s: %v", runID, err)
	}
	return records
}

// deltaRow is a correction_deltas row as the cases read it back. The three
// amounts are read as text, which is what keeps the assertion off floats
// (roadmap/00-conventions.md section 6).
type deltaRow struct {
	correctsRunID uuid.UUID
	projectID     string
	dimension     string
	oldAmount     string
	newAmount     string
	delta         string
	currency      string
}

// readDeltas reads a correction's deltas, ordered the way the diff produces
// them.
func (f fixture) readDeltas(t *testing.T, runID uuid.UUID) []deltaRow {
	t.Helper()

	rows, err := f.engine.Store.Pool().Query(t.Context(),
		`SELECT corrects_run_id, project_id, dimension, old_amount::text, new_amount::text,
		        delta::text, currency
		 FROM correction_deltas WHERE run_id = $1
		 ORDER BY cloud, resource_id, project_id, dimension`, runID)
	if err != nil {
		t.Fatalf("reading the deltas of run %s: %v", runID, err)
	}
	defer rows.Close()

	var deltas []deltaRow
	for rows.Next() {
		var row deltaRow
		if err := rows.Scan(&row.correctsRunID, &row.projectID, &row.dimension,
			&row.oldAmount, &row.newAmount, &row.delta, &row.currency); err != nil {
			t.Fatalf("scanning a delta of run %s: %v", runID, err)
		}
		deltas = append(deltas, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the deltas of run %s: %v", runID, err)
	}
	return deltas
}

// readStatementTotals is what a run's statements total, per key, as the text
// the column holds.
func (f fixture) readStatementTotals(t *testing.T, runID uuid.UUID) map[string]string {
	t.Helper()

	rows, err := f.engine.Store.Pool().Query(t.Context(),
		`SELECT project_id, total::text FROM project_statements WHERE run_id = $1`, runID)
	if err != nil {
		t.Fatalf("reading the statement totals of run %s: %v", runID, err)
	}
	defer rows.Close()

	totals := make(map[string]string)
	for rows.Next() {
		var key, total string
		if err := rows.Scan(&key, &total); err != nil {
			t.Fatalf("scanning a statement total of run %s: %v", runID, err)
		}
		totals[key] = total
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the statement totals of run %s: %v", runID, err)
	}
	return totals
}

// readStatementDocument reads one stored statement as the values a case asserts
// over.
func (f fixture) readStatementDocument(t *testing.T, runID uuid.UUID, key string) map[string]any {
	t.Helper()

	return decodeJSON(t, f.readStatementBytes(t, runID, key))
}

// readStatementBytes reads one stored statement as the bytes the column holds,
// which is what the case that renders one document twice compares.
func (f fixture) readStatementBytes(t *testing.T, runID uuid.UUID, key string) []byte {
	t.Helper()

	var raw []byte
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT document FROM project_statements WHERE run_id = $1 AND project_id = $2`,
		runID, key).Scan(&raw); err != nil {
		t.Fatalf("reading the statement of run %s for %s: %v", runID, key, err)
	}
	return raw
}

// readStats reads a run's stats as the generic values a case asserts over.
// Numbers keep the text they are stored with, so a quantity is never compared
// as a float.
func (f fixture) readStats(t *testing.T, runID uuid.UUID) map[string]any {
	t.Helper()

	var raw []byte
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT stats FROM runs WHERE id = $1`, runID).Scan(&raw); err != nil {
		t.Fatalf("reading the stats of run %s: %v", runID, err)
	}
	return decodeJSON(t, raw)
}

// periodStatus is the status of one billing period.
func (f fixture) periodStatus(t *testing.T, from time.Time) string {
	t.Helper()

	var status string
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT status FROM billing_periods WHERE period_from = $1`, from).Scan(&status); err != nil {
		t.Fatalf("reading the status of the period %s: %v", from.Format(time.RFC3339), err)
	}
	return status
}

// decodeJSON decodes one stored object into the values a case asserts over.
func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var value map[string]any
	if err := dec.Decode(&value); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	return value
}

// number is what an object holds under key, as the text it was stored with.
func number(t *testing.T, object map[string]any, key string) string {
	t.Helper()

	value, held := object[key]
	if !held {
		t.Fatalf("the object holds no %s: %v", key, object)
	}
	text, isNumber := value.(json.Number)
	if !isNumber {
		t.Fatalf("%s is a %T, want a number", key, value)
	}
	return text.String()
}

// text is what an object holds under key, as a string.
func text(t *testing.T, object map[string]any, key string) string {
	t.Helper()

	value, held := object[key]
	if !held {
		t.Fatalf("the object holds no %s: %v", key, object)
	}
	str, isText := value.(string)
	if !isText {
		t.Fatalf("%s is a %T, want text", key, value)
	}
	return str
}

// list is what an object holds under key, as the objects of a list.
func list(t *testing.T, object map[string]any, key string) []map[string]any {
	t.Helper()

	value, held := object[key]
	if !held {
		t.Fatalf("the object holds no %s: %v", key, object)
	}
	entries, isList := value.([]any)
	if !isList {
		t.Fatalf("%s is a %T, want a list", key, value)
	}
	objects := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		object, isObject := entry.(map[string]any)
		if !isObject {
			t.Fatalf("an entry of %s is a %T, want an object", key, entry)
		}
		objects = append(objects, object)
	}
	return objects
}

// assertAbsent fails when the stats hold any of the keys, which is how a case
// states that a clean run reports nothing under them.
func assertAbsent(t *testing.T, stats map[string]any, keys ...string) {
	t.Helper()

	for _, key := range keys {
		if value, held := stats[key]; held {
			t.Errorf("stats hold %s = %v, want the key left out", key, value)
		}
	}
}

// assertCounts fails when the run's four counts are not these.
func assertCounts(t *testing.T, stats map[string]any, candidates, usage, rated, documents string) {
	t.Helper()

	for _, tc := range []struct{ key, want string }{
		{"candidates", candidates},
		{"usage_records", usage},
		{"rated_records", rated},
		{"statements", documents},
	} {
		if got := number(t, stats, tc.key); got != tc.want {
			t.Errorf("stats %s = %s, want %s", tc.key, got, tc.want)
		}
	}
}

// assertNoRecords fails when a run holds any record at all, which is what a run
// that failed before its write transaction has to leave behind.
func (f fixture) assertNoRecords(t *testing.T, runID uuid.UUID) {
	t.Helper()

	for _, tc := range []struct {
		records string
		count   int
	}{
		{records: "usage records", count: len(f.readUsage(t, runID))},
		{records: "rated records", count: len(f.readRated(t, runID))},
		{records: "statements", count: len(f.readStatements(t, runID))},
		{records: "adjustment records", count: len(f.readAdjustments(t, runID))},
	} {
		if tc.count != 0 {
			t.Errorf("the failed run holds %d %s, want none", tc.count, tc.records)
		}
	}
}

// failingQuerier fails every query, the way a VictoriaMetrics that is down
// fails one.
type failingQuerier struct{ err error }

func (q failingQuerier) Query(context.Context, string, time.Time) (decimal.Decimal, error) {
	return decimal.Zero, q.err
}

// cancelingQuerier cancels the run and then waits for that cancellation to
// arrive. It is how a case cancels a run at a fixed point of the pass rather
// than wherever the scheduler happens to be.
type cancelingQuerier struct{ cancel context.CancelFunc }

func (q cancelingQuerier) Query(ctx context.Context, _ string, _ time.Time) (decimal.Decimal, error) {
	q.cancel()
	<-ctx.Done()
	return decimal.Zero, ctx.Err()
}

// egressSource is a metricsql counter source over the resource type the cases
// meter. Its query is only ever answered by the stubs above.
var egressSource = counters.Source{
	Platform: platform, ResourceType: resourceType, Metric: "egress_gb", Kind: counters.KindMetricsQL,
	Query: `sum(egress_bytes{cloud="{cloud}", resource_id="{resource_id}"})`, Required: true,
}

func TestExecute(t *testing.T) {
	f := newFixture(t)

	t.Run("completes a run over the period it metered", func(t *testing.T) {
		from, to := month(-20)
		const cloud = "os-run-complete"
		f.seedProject(t, cloud, "proj-complete")
		f.seedResource(t, instance(cloud, "i-complete", "proj-complete", from, standardSize))

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if result.PricingVersion != "v1" {
			t.Errorf("PricingVersion = %q, want v1", result.PricingVersion)
		}

		run := f.readRun(t, result.RunID)
		if run.status != "completed" {
			t.Errorf("status = %q, want completed", run.status)
		}
		if run.completedAt == nil {
			t.Error("completed_at = NULL, want the instant the run ended")
		}
		if run.pricingVersion != "v1" {
			t.Errorf("pricing_version = %q, want the model the period was rated with", run.pricingVersion)
		}
		if !slices.Equal(run.clouds, []string{cloud}) {
			t.Errorf("clouds = %v, want %v", run.clouds, []string{cloud})
		}

		usage := f.readUsage(t, result.RunID)
		if len(usage) != 1 {
			t.Fatalf("usage records = %d, want one per draft", len(usage))
		}
		if usage[0].seconds != 172800 {
			t.Errorf("seconds = %d, want the 48 hours the resource lived", usage[0].seconds)
		}
		if usage[0].projectID != "proj-complete" || usage[0].state != "active" {
			t.Errorf("usage record = (%s, %s), want (proj-complete, active)", usage[0].projectID, usage[0].state)
		}

		rated := f.readRated(t, result.RunID)
		want := []ratedRow{
			{dimension: "egress_gb", amount: "0.00", currency: "EUR", usageRecordID: usage[0].id},
			{dimension: "ram_gb", amount: "1.92", currency: "EUR", usageRecordID: usage[0].id},
			{dimension: "vcpus", amount: "3.84", currency: "EUR", usageRecordID: usage[0].id},
		}
		if !slices.Equal(rated, want) {
			t.Errorf("rated records = %+v, want %+v, one per dimension against the usage record of its draft",
				rated, want)
		}

		if keys := f.readStatements(t, result.RunID); !slices.Equal(keys, []string{cloud + "/proj-complete"}) {
			t.Errorf("statements = %v, want one per rendered document", keys)
		}
	})

	t.Run("stores what the run counted in its stats", func(t *testing.T) {
		from, to := month(-19)
		const cloud = "os-run-stats"
		f.seedProject(t, cloud, "proj-stats")
		f.seedResource(t, instance(cloud, "i-stats", "proj-stats", from, standardSize))
		// A project no registry row holds is billed under its raw id and named
		// in the stats, and a candidate without events is a metering warning.
		f.seedResource(t, instance(cloud, "i-unregistered", "proj-unregistered", from, standardSize))
		f.seedCandidate(t, instance(cloud, "i-without-history", "proj-stats", from, standardSize))

		before := f.snapshotTime(t)
		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		after := f.snapshotTime(t)

		stats := f.readStats(t, result.RunID)
		snapshotAt, err := time.Parse(time.RFC3339Nano, text(t, stats, "snapshot_at"))
		if err != nil {
			t.Fatalf("parsing snapshot_at: %v", err)
		}
		if snapshotAt.Before(before) || snapshotAt.After(after) {
			t.Errorf("snapshot_at = %s, want the snapshot's own instant, between %s and %s",
				snapshotAt, before, after)
		}
		if result.Stats.SnapshotAt == nil || !result.Stats.SnapshotAt.Equal(snapshotAt) {
			t.Errorf("Stats.SnapshotAt = %v, want the instant %s the run stored", result.Stats.SnapshotAt, snapshotAt)
		}

		assertCounts(t, stats, "3", "2", "6", "2")

		warnings := list(t, stats, "metering_warnings")
		if len(warnings) != 1 || text(t, warnings[0], "resource_id") != "i-without-history" {
			t.Errorf("metering_warnings = %v, want the candidate without history named", warnings)
		}
		if got := text(t, warnings[0], "code"); got != "candidate_without_history" {
			t.Errorf("metering warning code = %q, want candidate_without_history", got)
		}
		unregistered := list(t, stats, "unregistered_projects")
		if len(unregistered) != 1 || text(t, unregistered[0], "project_id") != "proj-unregistered" {
			t.Errorf("unregistered_projects = %v, want the project no registry row holds", unregistered)
		}

		assertAbsent(t, stats, "warnings", "counter_warnings", "attribution_warnings",
			"unpriced", "unreadable", "violations", "error",
			"adjustment_records", "adjustment_warnings")
	})

	t.Run("leaves the period status alone", func(t *testing.T) {
		for _, tc := range []struct {
			offset int
			status string
		}{
			{offset: -18, status: "open"},
			{offset: -17, status: "grace"},
		} {
			t.Run(tc.status, func(t *testing.T) {
				from, to := month(tc.offset)
				status := tc.status
				cloud := "os-run-status-" + status
				f.seedProject(t, cloud, "proj-status")
				f.seedResource(t, instance(cloud, "i-status-"+status, "proj-status", from, standardSize))
				if status != "open" {
					if _, err := f.engine.Store.Pool().Exec(t.Context(),
						`INSERT INTO billing_periods (period_from, period_to, status) VALUES ($1, $2, $3)`,
						from, to, status); err != nil {
						t.Fatalf("seeding the %s period: %v", status, err)
					}
				}

				result, err := f.execute(t, runs.Options{
					PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
				})
				if err != nil {
					t.Fatalf("Execute() error = %v, want nil", err)
				}
				if run := f.readRun(t, result.RunID); run.status != "completed" {
					t.Errorf("status = %q, want completed", run.status)
				}
				if got := f.periodStatus(t, from); got != status {
					t.Errorf("period status = %q, want it left at %q: a run moves no period status", got, status)
				}
			})
		}
	})

	t.Run("supersedes the completed run it replaces", func(t *testing.T) {
		from, to := month(-16)
		const cloud = "os-run-supersede"
		f.seedProject(t, cloud, "proj-supersede")
		f.seedResource(t, instance(cloud, "i-supersede", "proj-supersede", from, standardSize))
		opts := runs.Options{PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud}}

		first, err := f.execute(t, opts)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		second, err := f.execute(t, opts)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		if !slices.Equal(second.Superseded, []uuid.UUID{first.RunID}) {
			t.Errorf("Superseded = %v, want the completed run %s", second.Superseded, first.RunID)
		}
		if run := f.readRun(t, first.RunID); run.status != "superseded" {
			t.Errorf("the first run is %q, want superseded", run.status)
		}
		if run := f.readRun(t, second.RunID); run.status != "completed" {
			t.Errorf("the second run is %q, want completed", run.status)
		}
		// The records of a superseded run stay for the audit; what changes is
		// the status every later query filters on.
		if got := len(f.readUsage(t, first.RunID)); got != 1 {
			t.Errorf("the superseded run holds %d usage records, want its own kept", got)
		}
		if got := len(f.readRated(t, first.RunID)); got != 3 {
			t.Errorf("the superseded run holds %d rated records, want its own kept", got)
		}
		if got := len(f.readStatements(t, first.RunID)); got != 1 {
			t.Errorf("the superseded run holds %d statements, want its own kept", got)
		}
	})

	t.Run("refuses a finalized period", func(t *testing.T) {
		from, to := month(-15)
		const cloud = "os-run-finalized"
		f.seedProject(t, cloud, "proj-finalized")
		f.seedResource(t, instance(cloud, "i-finalized", "proj-finalized", from, standardSize))

		// Inserted as finalized rather than finalized afterwards: the runs
		// trigger holds a finalized row against every update.
		var finalizedRun uuid.UUID
		if err := f.engine.Store.Pool().QueryRow(t.Context(),
			`INSERT INTO runs (period_from, period_to, kind, pricing_version, status)
			 VALUES ($1, $2, 'regular', 'v1', 'finalized') RETURNING id`, from, to).Scan(&finalizedRun); err != nil {
			t.Fatalf("seeding the finalized run: %v", err)
		}
		if _, err := f.engine.Store.Pool().Exec(t.Context(),
			`INSERT INTO billing_periods (period_from, period_to, status, finalized_run_id, finalized_at)
			 VALUES ($1, $2, 'finalized', $3, now())`, from, to, finalizedRun); err != nil {
			t.Fatalf("seeding the finalized period: %v", err)
		}

		_, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if !errors.Is(err, runs.ErrPeriodFinalized) {
			t.Fatalf("Execute() error = %v, want one matching ErrPeriodFinalized", err)
		}
		if !strings.Contains(err.Error(), finalizedRun.String()) {
			t.Errorf("Execute() error = %q, want it to name the run %s that closed the period", err, finalizedRun)
		}
		if want := "tally-engine correct --period"; !strings.Contains(err.Error(), want) {
			t.Errorf("Execute() error = %q, want it to point at %q", err, want)
		}
		if got := f.countRuns(t, from); got != 1 {
			t.Errorf("the period holds %d runs, want only the finalized one: a refused run writes no row", got)
		}
	})

	t.Run("refuses a run while another holds the period", func(t *testing.T) {
		lockedFrom, lockedTo := month(-14)
		freeFrom, freeTo := month(-13)
		const lockedCloud, freeCloud = "os-run-locked", "os-run-free"
		f.seedProject(t, freeCloud, "proj-free")
		f.seedResource(t, instance(freeCloud, "i-free", "proj-free", freeFrom, standardSize))

		// The lock is taken the way the run takes it, on a connection of its
		// own: it is a session lock, so it is held until this connection closes.
		holder, err := pgx.Connect(t.Context(), f.engine.URL)
		if err != nil {
			t.Fatalf("opening the connection that holds the lock: %v", err)
		}
		defer func() {
			if err := holder.Close(context.Background()); err != nil {
				t.Errorf("closing the connection that holds the lock: %v", err)
			}
		}()
		var locked bool
		if err := holder.QueryRow(t.Context(),
			`SELECT pg_try_advisory_lock(hashtextextended('period:' || $1::text, 0))`,
			lockedFrom.UTC().Format(time.RFC3339)).Scan(&locked); err != nil {
			t.Fatalf("taking the period lock: %v", err)
		}
		if !locked {
			t.Fatal("pg_try_advisory_lock = false, want the test to hold the lock")
		}

		_, err = f.execute(t, runs.Options{
			PeriodFrom: lockedFrom, PeriodTo: lockedTo, Clouds: []string{lockedCloud},
		})
		if !errors.Is(err, runs.ErrRunInProgress) {
			t.Fatalf("Execute() error = %v, want one matching ErrRunInProgress", err)
		}
		if got := f.countRuns(t, lockedFrom); got != 0 {
			t.Errorf("the locked period holds %d runs, want none: the lock is taken before the first row", got)
		}

		// Another period is metered while that lock is held: the lock is per
		// period, not per engine.
		result, err := f.execute(t, runs.Options{
			PeriodFrom: freeFrom, PeriodTo: freeTo, Clouds: []string{freeCloud},
		})
		if err != nil {
			t.Fatalf("Execute() over another period error = %v, want nil", err)
		}
		if run := f.readRun(t, result.RunID); run.status != "completed" {
			t.Errorf("status = %q, want completed", run.status)
		}
	})

	t.Run("reclaims the run of a killed process", func(t *testing.T) {
		from, to := month(-12)
		const cloud = "os-run-reclaim"
		f.seedProject(t, cloud, "proj-reclaim")
		f.seedResource(t, instance(cloud, "i-reclaim", "proj-reclaim", from, standardSize))

		// Old enough that no process can be behind it: the age is what the
		// reclaim reads a killed process off, because the period lock is a
		// session lock that can be released while its process meters on.
		var stale uuid.UUID
		if err := f.engine.Store.Pool().QueryRow(t.Context(),
			`INSERT INTO runs (period_from, period_to, kind, pricing_version, status, started_at, stats)
			 VALUES ($1, $2, 'regular', 'v1', 'running', now() - interval '3 hours', '{"candidates": 7}'::jsonb)
			 RETURNING id`,
			from, to).Scan(&stale); err != nil {
			t.Fatalf("seeding the stale run: %v", err)
		}

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if !slices.Equal(result.Reclaimed, []uuid.UUID{stale}) {
			t.Errorf("Reclaimed = %v, want the stale run %s", result.Reclaimed, stale)
		}

		reclaimed := f.readRun(t, stale)
		if reclaimed.status != "failed" {
			t.Errorf("the stale run is %q, want failed", reclaimed.status)
		}
		if reclaimed.completedAt == nil {
			t.Fatal("the stale run has no completed_at, want the instant it was reclaimed")
		}
		// Reclaimed before the new run row exists, which is what keeps the
		// period from holding two runs that both read as in flight.
		if reclaimed.completedAt.After(f.readRun(t, result.RunID).startedAt) {
			t.Error("the stale run was failed after the new run started, want it reclaimed first")
		}
		stats := f.readStats(t, stale)
		if got := text(t, stats, "error"); !strings.Contains(got, "process ended without completing") {
			t.Errorf("the stale run's error = %q, want it to name the process that ended", got)
		}
		if got := number(t, stats, "candidates"); got != "7" {
			t.Errorf("the stale run's candidates = %s, want the 7 it had counted kept", got)
		}
	})

	t.Run("leaves a run that may still have a process behind it", func(t *testing.T) {
		from, to := month(-21)
		const cloud = "os-run-live"
		f.seedProject(t, cloud, "proj-live")
		f.seedResource(t, instance(cloud, "i-live", "proj-live", from, standardSize))

		// A run that started moments ago. The period lock says nothing about it:
		// it is a session lock on one pooled connection that stays protocol-idle
		// for the whole run, so an idle timeout or a reaper releases it while the
		// process behind it keeps metering. Failing that run here would take the
		// period away from a run that is still writing it.
		var live uuid.UUID
		if err := f.engine.Store.Pool().QueryRow(t.Context(),
			`INSERT INTO runs (period_from, period_to, kind, pricing_version, status)
			 VALUES ($1, $2, 'regular', 'v1', 'running') RETURNING id`,
			from, to).Scan(&live); err != nil {
			t.Fatalf("seeding the live run: %v", err)
		}

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if len(result.Reclaimed) != 0 {
			t.Errorf("Reclaimed = %v, want none: the run is too young to have lost its process", result.Reclaimed)
		}
		if got := f.readRun(t, live).status; got != "running" {
			t.Errorf("the live run is %q, want it left running", got)
		}
	})

	t.Run("refuses a period no model prices", func(t *testing.T) {
		from, to := month(-40)

		_, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{"os-run-unpriced"},
		})
		if !errors.Is(err, pricing.ErrNoModel) {
			t.Fatalf("Execute() error = %v, want one matching pricing.ErrNoModel", err)
		}
		if got := f.countRuns(t, from); got != 0 {
			t.Errorf("the period holds %d runs, want none: the prices are resolved before the run row", got)
		}
	})

	t.Run("fails the run on an invariant violation", func(t *testing.T) {
		from, to := month(-11)
		const cloud = "os-run-violation"
		f.seedProject(t, cloud, "proj-violation")
		f.seedResource(t, instance(cloud, "i-sound", "proj-violation", from, standardSize))
		opts := runs.Options{PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud}}

		sound, err := f.execute(t, opts)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		// An update after the delete reopens the timeline, which bills time the
		// resource's events do not describe a life for.
		broken := instance(cloud, "i-broken", "proj-violation", from, standardSize)
		f.seedResource(t, broken)
		f.seedEvent(t, broken, "ev-late-i-broken", "compute.instance.update",
			broken.deleted.Add(24*time.Hour), `{"state":"active"}`)

		result, err := f.execute(t, opts)
		if err == nil {
			t.Fatal("Execute() error = nil, want the violating resource reported")
		}
		var violation *metering.ViolationError
		if !errors.As(err, &violation) {
			t.Fatalf("Execute() error = %v, want a *metering.ViolationError", err)
		}

		if run := f.readRun(t, result.RunID); run.status != "failed" {
			t.Errorf("status = %q, want failed", run.status)
		}
		stats := f.readStats(t, result.RunID)
		violations := list(t, stats, "violations")
		if len(violations) != 1 || text(t, violations[0], "resource_id") != "i-broken" {
			t.Errorf("violations = %v, want the one violating resource named", violations)
		}
		if got := text(t, stats, "error"); got != err.Error() {
			t.Errorf("stats error = %q, want the failure %q", got, err.Error())
		}
		if got := len(f.readUsage(t, result.RunID)); got != 0 {
			t.Errorf("the failed run holds %d usage records, want none", got)
		}
		if got := len(f.readRated(t, result.RunID)); got != 0 {
			t.Errorf("the failed run holds %d rated records, want none", got)
		}
		if got := len(f.readStatements(t, result.RunID)); got != 0 {
			t.Errorf("the failed run holds %d statements, want none", got)
		}
		if run := f.readRun(t, sound.RunID); run.status != "completed" {
			t.Errorf("the earlier run is %q, want it left completed", run.status)
		}
	})

	t.Run("fails the run when a required counter source fails", func(t *testing.T) {
		from, to := month(-10)
		const cloud = "os-run-counter-failure"
		f.seedProject(t, cloud, "proj-counter-failure")
		f.seedResource(t, instance(cloud, "i-counter-failure", "proj-counter-failure", from, standardSize))
		down := errors.New("the metrics store did not answer")

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
			Counters: counters.Config{Sources: []counters.Source{egressSource}},
			VM:       failingQuerier{err: down},
		})
		if !errors.Is(err, down) {
			t.Fatalf("Execute() error = %v, want the querier's failure", err)
		}

		if run := f.readRun(t, result.RunID); run.status != "failed" {
			t.Errorf("status = %q, want failed", run.status)
		}
		if got := text(t, f.readStats(t, result.RunID), "error"); got != err.Error() {
			t.Errorf("stats error = %q, want the failure %q", got, err.Error())
		}
		if got := len(f.readUsage(t, result.RunID)); got != 0 {
			t.Errorf("the failed run holds %d usage records, want none", got)
		}
		if got := len(f.readRated(t, result.RunID)); got != 0 {
			t.Errorf("the failed run holds %d rated records, want none", got)
		}
		if got := len(f.readStatements(t, result.RunID)); got != 0 {
			t.Errorf("the failed run holds %d statements, want none", got)
		}
	})

	t.Run("records a canceled run as failed", func(t *testing.T) {
		from, to := month(-9)
		const cloud = "os-run-canceled"
		f.seedProject(t, cloud, "proj-canceled")
		f.seedResource(t, instance(cloud, "i-canceled", "proj-canceled", from, standardSize))

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		result, err := runs.Execute(ctx, f.engine.Store.Pool(), f.source, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
			Counters: counters.Config{Sources: []counters.Source{egressSource}},
			VM:       cancelingQuerier{cancel: cancel},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Execute() error = %v, want one matching context.Canceled", err)
		}
		// The bookkeeping runs on a context the cancellation does not reach, so
		// the run is written down rather than left reading as in flight.
		if run := f.readRun(t, result.RunID); run.status != "failed" {
			t.Errorf("status = %q, want failed", run.status)
		}
		if got := text(t, f.readStats(t, result.RunID), "error"); !strings.Contains(got, "context canceled") {
			t.Errorf("stats error = %q, want the cancellation recorded", got)
		}
	})

	t.Run("completes a period without candidates", func(t *testing.T) {
		from, to := month(-8)

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{"os-run-empty"},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if run := f.readRun(t, result.RunID); run.status != "completed" {
			t.Errorf("status = %q, want completed: an empty period is metered, not failed", run.status)
		}
		assertCounts(t, f.readStats(t, result.RunID), "0", "0", "0", "0")
		if got := len(f.readUsage(t, result.RunID)); got != 0 {
			t.Errorf("the run holds %d usage records, want none", got)
		}
		if got := len(f.readRated(t, result.RunID)); got != 0 {
			t.Errorf("the run holds %d rated records, want none", got)
		}
		if got := len(f.readStatements(t, result.RunID)); got != 0 {
			t.Errorf("the run holds %d statements, want none", got)
		}
	})

	t.Run("meters every cloud when none is named", func(t *testing.T) {
		from, to := month(-7)
		const cloud = "os-run-all-clouds"
		f.seedProject(t, cloud, "proj-all-clouds")
		f.seedResource(t, instance(cloud, "i-all-clouds", "proj-all-clouds", from, standardSize))

		// A nil cloud list and no counter sources at all, which is what a
		// deployment that configures neither runs with.
		result, err := f.execute(t, runs.Options{PeriodFrom: from, PeriodTo: to})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		run := f.readRun(t, result.RunID)
		if run.status != "completed" {
			t.Errorf("status = %q, want completed", run.status)
		}
		if len(run.clouds) != 0 || run.clouds == nil {
			t.Errorf("clouds = %v, want the empty array the column's default holds", run.clouds)
		}
		if got := len(f.readUsage(t, result.RunID)); got != 1 {
			t.Errorf("usage records = %d, want the resource of the period metered", got)
		}
	})

	t.Run("warns about a period that has not ended", func(t *testing.T) {
		from, to := month(0)
		const cloud = "os-run-open-period"
		f.seedProject(t, cloud, "proj-open-period")
		f.seedResource(t, instance(cloud, "i-open-period", "proj-open-period", from, standardSize))

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if run := f.readRun(t, result.RunID); run.status != "completed" {
			t.Errorf("status = %q, want completed", run.status)
		}

		warnings := list(t, f.readStats(t, result.RunID), "warnings")
		if len(warnings) != 1 {
			t.Fatalf("warnings = %v, want the one about the period", warnings)
		}
		if got := text(t, warnings[0], "code"); got != runs.WarningPeriodNotEnded {
			t.Errorf("warning code = %q, want %q", got, runs.WarningPeriodNotEnded)
		}
		if want := to.Format(time.RFC3339); !strings.Contains(text(t, warnings[0], "detail"), want) {
			t.Errorf("warning detail = %q, want it to name period_to %s", text(t, warnings[0], "detail"), want)
		}
	})

	t.Run("writes nothing when a statement is out of range", func(t *testing.T) {
		from, to := month(-6)
		const cloud = "os-run-overflow"
		f.seedProject(t, cloud, "proj-overflow")
		f.seedResource(t, instance(cloud, "i-overflow-sound", "proj-overflow", from, standardSize))
		opts := runs.Options{PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud}}

		sound, err := f.execute(t, opts)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		// Two dimensions of six hundred billion each: every single amount fits
		// the column, and the statement they add up to does not, so the run
		// fails inside the transaction that had already written its records.
		f.seedResource(t, instance(cloud, "i-overflow", "proj-overflow", from, largeSize))

		result, err := f.execute(t, opts)
		if err == nil {
			t.Fatal("Execute() error = nil, want the oversized statement refused")
		}
		if !strings.Contains(err.Error(), "out of range") {
			t.Errorf("Execute() error = %q, want it to say a usage value is out of range", err)
		}
		// Named by the statement rather than by a resource and a dimension,
		// which is what tells the refusal inside the write transaction from the
		// per-amount bound that would have refused the rows before it.
		if want := "the statement of " + cloud + "/proj-overflow"; !strings.Contains(err.Error(), want) {
			t.Errorf("Execute() error = %q, want the statement named with %q", err, want)
		}

		if run := f.readRun(t, result.RunID); run.status != "failed" {
			t.Errorf("status = %q, want failed", run.status)
		}
		if got := len(f.readUsage(t, result.RunID)); got != 0 {
			t.Errorf("the failed run holds %d usage records, want the transaction rolled back", got)
		}
		if got := len(f.readRated(t, result.RunID)); got != 0 {
			t.Errorf("the failed run holds %d rated records, want the transaction rolled back", got)
		}
		if run := f.readRun(t, sound.RunID); run.status != "completed" {
			t.Errorf("the earlier run is %q, want it left completed", run.status)
		}
	})

	t.Run("stores a counter metric beside the derived usage", func(t *testing.T) {
		from, to := month(-5)
		const cloud = "os-run-counters"
		f.seedProject(t, cloud, "proj-counters")
		measured := instance(cloud, "i-counters", "proj-counters", from, standardSize)
		f.seedResource(t, measured)
		// Inside the resource's one draft, and carrying no payload, so the
		// interval is not split by it.
		f.seedEvent(t, measured, "ev-reboot-i-counters", "compute.instance.reboot.end",
			measured.created.Add(12*time.Hour), "")

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
			Counters: counters.Config{Sources: []counters.Source{{
				Platform: platform, ResourceType: resourceType, Metric: "reboots",
				Kind: counters.KindEvents, EventType: "compute.instance.reboot.end",
			}}},
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		usage := f.readUsage(t, result.RunID)
		if len(usage) != 1 {
			t.Fatalf("usage records = %d, want one per draft", len(usage))
		}
		stored := decodeJSON(t, usage[0].usage)
		for _, tc := range []struct{ metric, want string }{
			{"reboots", "1.0000"},
			{"minutes", "2880.0000"},
			{"count", "1"},
			{"vcpus", "4"},
		} {
			if got := number(t, stored, tc.metric); got != tc.want {
				t.Errorf("usage %s = %s, want %s", tc.metric, got, tc.want)
			}
		}
	})

	t.Run("applies the adjustments of a reseller relation", func(t *testing.T) {
		from, to := month(-22)
		const cloud = "os-run-reseller"
		const key = cloud + "/proj-reseller"
		f.seedProject(t, cloud, "proj-reseller")
		f.seedResource(t, instance(cloud, "i-reseller", "proj-reseller", from, standardSize))
		// The partner the project is managed by, and the relation that carries
		// what its customers are billed at. It opens before the period, so it
		// adjusts the whole of it.
		relation := f.seedRelation(t, f.projectIDOf(t, cloud, "proj-reseller"),
			f.seedVirtualProject(t, "partner", "partner-corp"),
			"managed_by", resellerAdjustments, from.AddDate(0, -1, 0), nil)

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
			AdjustmentRelationTypes: adjustmentRelationTypes, AdjustmentDepth: adjustmentDepth,
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		// 5.76 rated, 15 percent off it, and 10 percent of what is left owed to
		// the partner beside the net rather than in it.
		document := f.readStatementDocument(t, result.RunID, key)
		for _, tc := range []struct{ field, want string }{
			{"base_cost", "5.76"},
			{"net_cost", "4.90"},
			{"kickback_total", "0.49"},
			{"total", "4.90"},
		} {
			if got := number(t, document, tc.field); got != tc.want {
				t.Errorf("the statement's %s = %s, want %s", tc.field, got, tc.want)
			}
		}
		lines := list(t, document, "adjustments")
		if len(lines) != 2 {
			t.Fatalf("the statement holds %d adjustments, want the discount and the kickback", len(lines))
		}
		for i, tc := range []struct{ typ, base, amount string }{
			{typ: "discount", base: "5.76", amount: "-0.86"},
			{typ: "kickback", base: "4.90", amount: "0.49"},
		} {
			if got := text(t, lines[i], "type"); got != tc.typ {
				t.Errorf("adjustment %d is a %s, want a %s", i, got, tc.typ)
			}
			if got := number(t, lines[i], "base"); got != tc.base {
				t.Errorf("the %s is rated on %s, want %s", tc.typ, got, tc.base)
			}
			if got := number(t, lines[i], "amount"); got != tc.amount {
				t.Errorf("the %s is %s, want %s", tc.typ, got, tc.amount)
			}
		}
		if got := f.readStatementTotals(t, result.RunID)[key]; got != "4.90" {
			t.Errorf("the stored total of %s is %s, want the 4.90 the customer pays", key, got)
		}

		// One record per applied adjustment, each naming the relation it came
		// from, and the partner named as owed the kickback alone.
		records := f.readAdjustments(t, result.RunID)
		if len(records) != 2 {
			t.Fatalf("the run holds %d adjustment records, want one per applied adjustment", len(records))
		}
		for i, tc := range []struct{ typ, rate, base, amount, beneficiary string }{
			{typ: "discount", rate: "0.150000", base: "5.76", amount: "-0.86"},
			{typ: "kickback", rate: "0.100000", base: "4.90", amount: "0.49", beneficiary: "partner-corp"},
		} {
			want := adjustmentRow{
				projectID: key, relationID: relation.String(), relationType: "managed_by",
				relationTarget: "partner-corp", typ: tc.typ, scope: "all",
				rate: tc.rate, base: tc.base, amount: tc.amount, currency: "EUR",
			}
			got := records[i]
			beneficiary := ""
			if got.beneficiary != nil {
				beneficiary = *got.beneficiary
			}
			got.beneficiary = nil
			if got != want || beneficiary != tc.beneficiary {
				t.Errorf("the %s record is %+v owed to %q, want %+v owed to %q",
					tc.typ, got, beneficiary, want, tc.beneficiary)
			}
		}

		stats := f.readStats(t, result.RunID)
		if got := number(t, stats, "adjustment_records"); got != "2" {
			t.Errorf("stats adjustment_records = %s, want 2", got)
		}
		assertAbsent(t, stats, "adjustment_warnings")
	})

	t.Run("applies a relation closed inside the period and not one closed before it", func(t *testing.T) {
		from, to := month(-23)
		const cloud = "os-run-relation-validity"
		f.seedProject(t, cloud, "proj-validity")
		f.seedResource(t, instance(cloud, "i-validity", "proj-validity", from, standardSize))
		project := f.projectIDOf(t, cloud, "proj-validity")

		// A relation is read when its validity overlaps the period, and it then
		// adjusts the whole of it (D4). One of these two was closed halfway
		// through the month and one an hour before it began.
		inside := from.Add(15 * 24 * time.Hour)
		before := from.Add(-time.Hour)
		overlapping := f.seedRelation(t, project, f.seedVirtualProject(t, "partner", "partner-inside"),
			"managed_by", discountAdjustments, from.AddDate(0, -1, 0), &inside)
		f.seedRelation(t, project, f.seedVirtualProject(t, "partner", "partner-before"),
			"managed_by", discountAdjustments, from.AddDate(0, -2, 0), &before)

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
			AdjustmentRelationTypes: adjustmentRelationTypes, AdjustmentDepth: adjustmentDepth,
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}

		records := f.readAdjustments(t, result.RunID)
		if len(records) != 1 {
			t.Fatalf("the run holds %d adjustment records, want the one of the relation the period overlaps",
				len(records))
		}
		if records[0].relationID != overlapping.String() {
			t.Errorf("the record names relation %s, want the %s the period overlaps",
				records[0].relationID, overlapping)
		}
		if records[0].amount != "-0.58" {
			t.Errorf("the discount is %s, want -0.58, a tenth of the 5.76 the month was rated at",
				records[0].amount)
		}
	})

	t.Run("leaves a statement alone when adjustments are turned off", func(t *testing.T) {
		from, to := month(-24)
		const cloud = "os-run-adjustments-off"
		const key = cloud + "/proj-off"
		f.seedProject(t, cloud, "proj-off")
		f.seedResource(t, instance(cloud, "i-off", "proj-off", from, standardSize))
		opts := runs.Options{PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud}}

		unadjusted, err := f.execute(t, opts)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		document := f.readStatementBytes(t, unadjusted.RunID, key)

		// The relation exists now, and the run still walks no relation type at
		// all, which is what turns the resolution off.
		f.seedRelation(t, f.projectIDOf(t, cloud, "proj-off"),
			f.seedVirtualProject(t, "partner", "partner-off"),
			"managed_by", resellerAdjustments, from.AddDate(0, -1, 0), nil)

		result, err := f.execute(t, opts)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if got := f.readStatementBytes(t, result.RunID, key); !bytes.Equal(got, document) {
			t.Errorf("the statement is %s, want the %s it was rendered as before the relation existed",
				got, document)
		}
		if got := len(f.readAdjustments(t, result.RunID)); got != 0 {
			t.Errorf("the run holds %d adjustment records, want none", got)
		}
	})

	t.Run("warns about a kickback to a meta-project", func(t *testing.T) {
		from, to := month(-25)
		const cloud = "os-run-kickback-warning"
		const key = cloud + "/proj-warning"
		f.seedProject(t, cloud, "proj-warning")
		f.seedResource(t, instance(cloud, "i-warning", "proj-warning", from, standardSize))
		// A meta-project is not a partner, so the kickback of the relation is
		// dropped and the project discount beside it is applied.
		relation := f.seedRelation(t, f.projectIDOf(t, cloud, "proj-warning"),
			f.seedVirtualProject(t, "meta", "customer-alpha"), "member_of",
			`{"pricing_adjustments":[{"type":"project_discount","rate":"0.05","scope":"all"},`+
				`{"type":"kickback","rate":"0.10","scope":"all"}]}`,
			from.AddDate(0, -1, 0), nil)

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
			AdjustmentRelationTypes: adjustmentRelationTypes, AdjustmentDepth: adjustmentDepth,
		})
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if run := f.readRun(t, result.RunID); run.status != "completed" {
			t.Errorf("status = %q, want completed: a kickback nobody is owed is a warning, not a failure",
				run.status)
		}

		records := f.readAdjustments(t, result.RunID)
		if len(records) != 1 {
			t.Fatalf("the run holds %d adjustment records, want the project discount alone", len(records))
		}
		if records[0].typ != "project_discount" || records[0].base != "5.76" || records[0].amount != "-0.29" {
			t.Errorf("the record is %+v, want the project discount of -0.29 on 5.76", records[0])
		}

		document := f.readStatementDocument(t, result.RunID, key)
		if got := len(list(t, document, "adjustments")); got != 1 {
			t.Errorf("the statement holds %d adjustments, want the project discount alone", got)
		}
		if got := number(t, document, "kickback_total"); got != "0.00" {
			t.Errorf("kickback_total = %s, want 0.00: nobody is owed the commission", got)
		}

		warnings := list(t, f.readStats(t, result.RunID), "adjustment_warnings")
		if len(warnings) != 1 {
			t.Fatalf("adjustment_warnings = %v, want the one dropped kickback", warnings)
		}
		for _, tc := range []struct{ field, want string }{
			{"code", "adjustment_kickback_target_not_partner"},
			{"relation_id", relation.String()},
			{"target_id", "customer-alpha"},
		} {
			if got := text(t, warnings[0], tc.field); got != tc.want {
				t.Errorf("the warning's %s = %q, want %q", tc.field, got, tc.want)
			}
		}
	})

	t.Run("fails the run on a relation whose adjustments the schema refuses", func(t *testing.T) {
		from, to := month(-26)
		const cloud = "os-run-adjustment-invalid"
		f.seedProject(t, cloud, "proj-invalid")
		f.seedResource(t, instance(cloud, "i-invalid", "proj-invalid", from, standardSize))
		// A type the schema does not admit, which the registry's own API refuses
		// on the way in. A relation whose stored array cannot be read must not
		// bill as though it carried nothing.
		//
		// It is valid for this month alone, because a relation the resolution
		// cannot parse fails every run whose period it overlaps, whatever
		// project it names, and the months of the other cases are not this
		// one's to fail.
		relation := f.seedRelation(t, f.projectIDOf(t, cloud, "proj-invalid"),
			f.seedVirtualProject(t, "partner", "partner-invalid"), "managed_by",
			`{"pricing_adjustments":[{"type":"rebate","rate":"0.15","scope":"all"}]}`,
			from, &to)

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
			AdjustmentRelationTypes: adjustmentRelationTypes, AdjustmentDepth: adjustmentDepth,
		})
		if err == nil {
			t.Fatal("Execute() error = nil, want the relation the schema refuses reported")
		}
		for _, want := range []string{relation.String(), "do not match the schema"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Execute() error = %q, want it to name %q", err, want)
			}
		}

		if run := f.readRun(t, result.RunID); run.status != "failed" {
			t.Errorf("status = %q, want failed", run.status)
		}
		if got := text(t, f.readStats(t, result.RunID), "error"); got != err.Error() {
			t.Errorf("stats error = %q, want the failure %q", got, err.Error())
		}
		f.assertNoRecords(t, result.RunID)
	})

	t.Run("writes nothing when an adjustment is out of range", func(t *testing.T) {
		from, to := month(-27)
		const cloud = "os-run-adjustment-overflow"
		f.seedProject(t, cloud, "proj-adjustment-overflow")
		// Two dimensions of six hundred billion each: every rated amount fits
		// the column, and the surcharge on what they add up to does not, so the
		// run is refused before its first insert.
		f.seedResource(t, instance(cloud, "i-adjustment-overflow", "proj-adjustment-overflow", from, largeSize))
		f.seedRelation(t, f.projectIDOf(t, cloud, "proj-adjustment-overflow"),
			f.seedVirtualProject(t, "partner", "partner-overflow"), "managed_by",
			`{"pricing_adjustments":[{"type":"surcharge","rate":"1","scope":"all"}]}`,
			from.AddDate(0, -1, 0), nil)

		result, err := f.execute(t, runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
			AdjustmentRelationTypes: adjustmentRelationTypes, AdjustmentDepth: adjustmentDepth,
		})
		if err == nil {
			t.Fatal("Execute() error = nil, want the oversized adjustment refused")
		}
		if want := "past the 999999999999.99 the column holds"; !strings.Contains(err.Error(), want) {
			t.Errorf("Execute() error = %q, want it to say %q", err, want)
		}

		if run := f.readRun(t, result.RunID); run.status != "failed" {
			t.Errorf("status = %q, want failed", run.status)
		}
		f.assertNoRecords(t, result.RunID)
	})

	t.Run("stores the adjustment records with the run", func(t *testing.T) {
		from, to := month(-28)
		const cloud = "os-run-adjustment-records"
		f.seedProject(t, cloud, "proj-records")
		f.seedResource(t, instance(cloud, "i-records", "proj-records", from, standardSize))
		f.seedRelation(t, f.projectIDOf(t, cloud, "proj-records"),
			f.seedVirtualProject(t, "partner", "partner-records"), "managed_by",
			resellerAdjustments, from.AddDate(0, -1, 0), nil)
		opts := runs.Options{
			PeriodFrom: from, PeriodTo: to, Clouds: []string{cloud},
			AdjustmentRelationTypes: adjustmentRelationTypes, AdjustmentDepth: adjustmentDepth,
		}

		first, err := f.execute(t, opts)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		records := f.readAdjustments(t, first.RunID)
		if len(records) != 2 {
			t.Fatalf("the run holds %d adjustment records, want the discount and the kickback", len(records))
		}
		for _, record := range records {
			if record.currency != "EUR" {
				t.Errorf("the %s record is in %s, want the EUR the period was rated in",
					record.typ, record.currency)
			}
		}

		second, err := f.execute(t, opts)
		if err != nil {
			t.Fatalf("Execute() error = %v, want nil", err)
		}
		if !slices.Equal(second.Superseded, []uuid.UUID{first.RunID}) {
			t.Errorf("Superseded = %v, want the completed run %s", second.Superseded, first.RunID)
		}
		// The records of a superseded run stay for the audit, the way its usage
		// records do, and the run that replaced it holds its own.
		if got := len(f.readAdjustments(t, first.RunID)); got != 2 {
			t.Errorf("the superseded run holds %d adjustment records, want its own kept", got)
		}
		if got := len(f.readAdjustments(t, second.RunID)); got != 2 {
			t.Errorf("the second run holds %d adjustment records, want the two it applied", got)
		}
	})
}

// TestExecuteReportsAnUpstreamFailure breaks the reporting database under a run.
// It runs on databases of its own, because what it does to the reporting one
// leaves nothing else able to meter.
func TestExecuteReportsAnUpstreamFailure(t *testing.T) {
	f := newFixture(t)
	from, to := month(-4)

	if _, err := f.reporting.Store.Pool().Exec(t.Context(), `DROP TABLE current_resources`); err != nil {
		t.Fatalf("dropping the projection table: %v", err)
	}

	result, err := f.execute(t, runs.Options{
		PeriodFrom: from, PeriodTo: to, Clouds: []string{"os-run-upstream"},
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want the failed read reported")
	}
	if want := "listing the candidate resources"; !strings.Contains(err.Error(), want) {
		t.Errorf("Execute() error = %q, want the read that failed named with %q", err, want)
	}

	if run := f.readRun(t, result.RunID); run.status != "failed" {
		t.Errorf("status = %q, want failed", run.status)
	}
	if got := text(t, f.readStats(t, result.RunID), "error"); got != err.Error() {
		t.Errorf("stats error = %q, want the failure %q, as the reporting side reported it", got, err.Error())
	}
}

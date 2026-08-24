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

// month is the UTC billing month offset months from the one the test runs in:
// -3 is three months back, 0 the current one. The periods are derived from the
// wall clock rather than written down, because whether a period has ended is
// what one of the cases below is about and what the others must not run into.
func month(offset int) (from, to time.Time) {
	now := time.Now().UTC()
	from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, offset, 0)
	return from, from.AddDate(0, 1, 0)
}

// fixture is the pair of databases the cases run against: the engine database a
// run is written to, and the reporting database it is read from, with one
// pricing model imported.
type fixture struct {
	engine    enginetest.DB
	reporting reportingtest.DB
	source    *source.DB
}

// newFixture starts both databases and imports the pricing model. It is called
// once per test function: the cases inside share the containers and keep to
// their own period and their own cloud.
func newFixture(t *testing.T) fixture {
	t.Helper()

	engineDB := enginetest.NewDB(t)
	reportingDB := reportingtest.NewDB(t)

	validFrom, _ := month(-36)
	model, document, err := pricing.Parse([]byte(fmt.Sprintf(pricingDocument, validFrom.Format(time.RFC3339))))
	if err != nil {
		t.Fatalf("parsing the pricing model: %v", err)
	}
	if _, err := pricing.Import(t.Context(), sqlcgen.New(engineDB.Store.Pool()), model, document); err != nil {
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

// runRow is a runs row as the cases read it back, past the package under test.
type runRow struct {
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
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT status, pricing_version, clouds, started_at, completed_at FROM runs WHERE id = $1`, runID,
	).Scan(&row.status, &row.pricingVersion, &row.clouds, &row.startedAt, &row.completedAt); err != nil {
		t.Fatalf("reading run %s: %v", runID, err)
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
			"unpriced", "unreadable", "violations", "error")
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

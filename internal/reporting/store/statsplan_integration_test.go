package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// The fixture the plan is taken over. Two clouds, two projects, two event types
// and two sources, so a bucket of the count carries more than one group.
const (
	planPlatform     = "openstack"
	planCloud        = "os-plan"
	planOtherCloud   = "os-plan-second"
	planProject      = "project-a"
	planOtherProject = "project-b"
	planResourceType = "volume"
)

// The window the count is planned for. The fixture spans it, so every seeded
// event is inside the bounds the read carries.
var (
	planWindowStart = time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)
	planWindowEnd   = planWindowStart.Add(72 * time.Hour)
)

// planTransferEvents is how long the history of the transferred resource is. The
// last of them moves it to planProject, so that project stores one event of a
// resource whose fold reads all of them.
const planTransferEvents = 6

// planRowCap is what the count is capped at. It is far above the number of
// groups the fixture can produce, so the cap never decides what comes back.
const planRowCap = 1000

// TestCountEventBucketsBoundedWindowNeedsNoSeqScan takes the plan of the
// bucketed event count over a bounded window and fails when any node of it reads
// the events hypertable sequentially.
//
// The plan is taken with enable_seqscan off, which only makes a sequential scan
// expensive and cannot forbid one. A Seq Scan that survives the setting is
// therefore one the planner had no index path to put in its place, which is the
// regression this pins: the count runs over the table that grows without bound,
// and a window is worth asking for only while the answer costs what the window
// holds rather than what the table does. The index that serves it is
// events_timestamp_idx, the one create_hypertable puts on the time dimension;
// idx_events_type leads with the event type, which this read does not filter on.
func TestCountEventBucketsBoundedWindowNeedsNoSeqScan(t *testing.T) {
	db := storetest.NewDB(t)
	seedPlanEvents(t, db)

	pool, recorder := tracedPool(t, db)

	rows, err := sqlcgen.New(pool).CountEventBuckets(t.Context(), sqlcgen.CountEventBucketsParams{
		BucketWidth: "1 hour",
		FromTs:      pgtype.Timestamptz{Time: planWindowStart, Valid: true},
		ToTs:        pgtype.Timestamptz{Time: planWindowEnd, Valid: true},
		RowCap:      planRowCap,
	})
	if err != nil {
		t.Fatalf("CountEventBuckets() error = %v, want nil", err)
	}
	// A window the fixture misses would leave the planner nothing to prove: an
	// empty range is cheap to answer from anywhere, chunk included.
	if len(rows) == 0 {
		t.Fatal("CountEventBuckets() rows = 0, want the seeded window counted")
	}

	// Every later call through this pool replaces what the recorder holds, so
	// the statement is read before the EXPLAIN is issued.
	sql, args := recorder.sql, recorder.args

	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquiring a connection: %v", err)
	}
	defer conn.Release()

	// The setting lives for the session, so it and the EXPLAIN run on the one
	// connection.
	setPlannerFlag(t, conn, "enable_seqscan", "off")

	plan, explained := explainPlan(t, conn, sql, args)
	if scanned := seqScanned(plan, eventChunk); len(scanned) > 0 {
		t.Errorf("relations of the events hypertable scanned sequentially = %v, want none\nplan: %s",
			scanned, explained)
	}

	// The control, with the settings the other way round. With the index paths
	// priced out and the sequential scan no longer penalized the planner has
	// nothing else to take, so the walk has to find one. Were it blind to them,
	// because the shape of the plan or the keys of its JSON stopped matching what
	// planNode reads, the assertion above would pass just as quietly on the
	// regression it exists to catch.
	//
	// Every one of these settings is a price rather than a prohibition, which is
	// why the first plan is taken with only enable_seqscan off: penalizing the
	// index paths as well would leave both in the running and decide nothing.
	setPlannerFlag(t, conn, "enable_seqscan", "on")
	for _, flag := range []string{"enable_indexscan", "enable_indexonlyscan", "enable_bitmapscan"} {
		setPlannerFlag(t, conn, flag, "off")
	}
	plan, explained = explainPlan(t, conn, sql, args)
	if scanned := seqScanned(plan, eventChunk); len(scanned) == 0 {
		t.Errorf("sequential scans seen with every index path off = 0, want the walk to find them\nplan: %s",
			explained)
	}
}

// TestCountCurrentResourcesGroupedNeedsNoSeqScan takes the plan of the grouped
// resource count and fails when it reads the projection sequentially.
//
// It is read the way the bucketed count above is, and for the same reason: the
// count runs once per request over a table that only grows, because a deleted
// resource keeps its row, and that heap is wide — size and last_payload are
// JSONB on every one of them. The index that serves it is
// idx_current_resources_stats from 0007, which leads with the five columns the
// query groups by and so answers it index-only; without it the planner has the
// heap and a hash aggregate and nothing else.
func TestCountCurrentResourcesGroupedNeedsNoSeqScan(t *testing.T) {
	db := storetest.NewDB(t)
	seedPlanResources(t, db)

	pool, recorder := tracedPool(t, db)

	rows, err := sqlcgen.New(pool).CountCurrentResourcesGrouped(t.Context(),
		sqlcgen.CountCurrentResourcesGroupedParams{RowCap: planRowCap})
	if err != nil {
		t.Fatalf("CountCurrentResourcesGrouped() error = %v, want nil", err)
	}
	// A projection the query counts nothing of would leave the planner nothing to
	// prove: an empty table is cheap to answer from anywhere.
	if len(rows) == 0 {
		t.Fatal("CountCurrentResourcesGrouped() rows = 0, want the seeded fleet counted")
	}

	sql, args := recorder.sql, recorder.args

	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquiring a connection: %v", err)
	}
	defer conn.Release()

	setPlannerFlag(t, conn, "enable_seqscan", "off")

	plan, explained := explainPlan(t, conn, sql, args)
	if scanned := seqScanned(plan, currentResources); len(scanned) > 0 {
		t.Errorf("relations of the projection scanned sequentially = %v, want none\nplan: %s",
			scanned, explained)
	}

	// The control the bucketed count takes as well: with every index path priced
	// out the walk has to find the scan, or the assertion above would pass just as
	// quietly on a plan its reader no longer understands.
	setPlannerFlag(t, conn, "enable_seqscan", "on")
	for _, flag := range []string{"enable_indexscan", "enable_indexonlyscan", "enable_bitmapscan"} {
		setPlannerFlag(t, conn, flag, "off")
	}
	plan, explained = explainPlan(t, conn, sql, args)
	if scanned := seqScanned(plan, currentResources); len(scanned) == 0 {
		t.Errorf("sequential scans seen with every index path off = 0, want the walk to find them\nplan: %s",
			explained)
	}
}

// TestCountProjectFoldEventsProbesTheJoinedSet counts the events one project
// summary folds and fails when the answer is the project's own events instead of
// the set the fold reads.
//
// The summary refuses a project whose fold is longer than one answer holds, and
// it decides that off this count because the read it guards materializes and
// sorts its rows whole before the first one reaches the caller. The two have to
// measure one set: a transfer writes the new project onto the event that moves
// the resource, so a project holding a single event of a resource pulls in the
// whole history it was handed with, and a count of the project's own events
// would leave the read it guards bounded by nothing.
func TestCountProjectFoldEventsProbesTheJoinedSet(t *testing.T) {
	db := storetest.NewDB(t)
	seedPlanTransfer(t, db)

	queries := sqlcgen.New(db.Store.Pool())
	params := sqlcgen.CountProjectFoldEventsParams{
		Cloud:      planCloud,
		ProjectID:  planProject,
		ToTs:       pgtype.Timestamptz{Time: planWindowEnd, Valid: true},
		ProbeLimit: planRowCap,
	}

	count, err := queries.CountProjectFoldEvents(t.Context(), params)
	if err != nil {
		t.Fatalf("CountProjectFoldEvents() error = %v, want nil", err)
	}
	if want := int64(planTransferEvents); count != want {
		t.Errorf("CountProjectFoldEvents() = %d, want the %d events the fold reads", count, want)
	}

	// What the project stored under its own name, which is the number the count
	// must not answer with: it is the whole difference the join makes, and a fold
	// bounded by it would read every event of the resource under a bound of one.
	var own int64
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM events WHERE cloud = $1 AND project_id = $2`,
		planCloud, planProject).Scan(&own); err != nil {
		t.Fatalf("counting the events of the project: %v", err)
	}
	if own != 1 {
		t.Fatalf("events stored under the project = %d, want the 1 the transfer leaves it", own)
	}

	// The probe limit is what keeps the answer cheap on a set far past the bound:
	// it saturates there rather than reporting how long the set is.
	params.ProbeLimit = planTransferEvents - 1
	saturated, err := queries.CountProjectFoldEvents(t.Context(), params)
	if err != nil {
		t.Fatalf("CountProjectFoldEvents() error = %v, want nil", err)
	}
	if want := int64(planTransferEvents - 1); saturated != want {
		t.Errorf("CountProjectFoldEvents() = %d, want the %d the probe limit stops at", saturated, want)
	}
}

// tracedPool opens a pool of its own that records the statement it last sent, so
// that the EXPLAIN a test issues runs on the statement sqlc generated rather
// than on a copy of it kept in the test, which nothing would keep in step with
// queries.sql.
func tracedPool(t *testing.T, db storetest.DB) (*pgxpool.Pool, *queryRecorder) {
	t.Helper()

	config, err := pgxpool.ParseConfig(db.URL)
	if err != nil {
		t.Fatalf("parsing the database url: %v", err)
	}
	recorder := &queryRecorder{}
	config.ConnConfig.Tracer = recorder
	// One connection: the recorder holds one statement, and a second connection
	// establishing itself alongside the query could be what wrote it.
	config.MaxConns = 1

	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("opening the traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, recorder
}

// setPlannerFlag turns one planner setting on or off for the session conn holds.
func setPlannerFlag(t *testing.T, conn *pgxpool.Conn, flag, value string) {
	t.Helper()

	if _, err := conn.Exec(t.Context(), "SET "+flag+" = "+value); err != nil {
		t.Fatalf("setting %s to %s: %v", flag, value, err)
	}
}

// explainPlan takes the plan of sql with args and returns its root node together
// with the JSON it was read from, which is what a failure prints.
func explainPlan(t *testing.T, conn *pgxpool.Conn, sql string, args []any) (planNode, []byte) {
	t.Helper()

	var explained []byte
	if err := conn.QueryRow(t.Context(), "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&explained); err != nil {
		t.Fatalf("explaining the bucket count: %v", err)
	}

	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(explained, &plans); err != nil {
		t.Fatalf("decoding the plan: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("plans in the EXPLAIN output = %d, want 1", len(plans))
	}
	return plans[0].Plan, explained
}

// queryRecorder keeps the statement a pgx connection sent last, together with
// the arguments it sent with it. It is what turns a generated query into
// something EXPLAIN can be run on.
type queryRecorder struct {
	sql  string
	args []any
}

func (r *queryRecorder) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.sql, r.args = data.SQL, data.Args
	return ctx
}

func (r *queryRecorder) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// planNode is the part of an EXPLAIN (FORMAT JSON) node the assertion reads.
// The output is a tree: every node carries the nodes it draws its rows from.
type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	Plans        []planNode `json:"Plans"`
}

// seqScanned reports the relations a plan reads sequentially, out of the ones
// the predicate names, walking the whole tree because the scans sit at its
// leaves.
func seqScanned(node planNode, relation func(string) bool) []string {
	var scanned []string
	if node.NodeType == "Seq Scan" && relation(node.RelationName) {
		scanned = append(scanned, node.RelationName)
	}
	for _, child := range node.Plans {
		scanned = append(scanned, seqScanned(child, relation)...)
	}
	return scanned
}

// eventChunk names a relation of the events hypertable. A plan names the chunks
// it touches rather than the hypertable, and TimescaleDB names a chunk
// _hyper_<hypertable>_<chunk>_chunk.
func eventChunk(relation string) bool {
	return relation == "events" || strings.HasPrefix(relation, "_hyper")
}

// currentResources names the projection, which is a plain table and so appears
// in a plan under the name it was created with.
func currentResources(relation string) bool {
	return relation == "current_resources"
}

// seedPlanEvents writes the events the plan is taken over. Only the columns the
// count groups and filters by vary; the payload it never reads stays NULL.
func seedPlanEvents(t *testing.T, db storetest.DB) {
	t.Helper()

	queries := sqlcgen.New(db.Store.Pool())
	for _, e := range []struct {
		id        string
		at        time.Time
		eventType string
		cloud     string
		projectID string
		source    event.Source
	}{
		{"plan-1", planWindowStart, "volume.create", planCloud, planProject, event.SourceCollector},
		{"plan-2", planWindowStart.Add(90 * time.Minute), "volume.delete", planCloud, planProject, event.SourceCollector},
		{"plan-3", planWindowStart.Add(26 * time.Hour), "volume.create", planOtherCloud, planOtherProject, event.SourceCollector},
		// Inside the bucket plan-3 opened, under another cloud and another
		// source: one bucket, two groups.
		{"plan-4", planWindowStart.Add(26*time.Hour + 20*time.Minute), "volume.create", planCloud, planOtherProject, event.SourceReconciliation},
		{"plan-5", planWindowStart.Add(50 * time.Hour), "volume.delete", planOtherCloud, planProject, event.SourceReconciliation},
		{"plan-6", planWindowStart.Add(71 * time.Hour), "volume.create", planCloud, planProject, event.SourceCollector},
	} {
		if _, err := queries.InsertEvent(t.Context(), sqlcgen.InsertEventParams{
			EventID:      e.id,
			Timestamp:    pgtype.Timestamptz{Time: e.at, Valid: true},
			EventType:    e.eventType,
			Platform:     planPlatform,
			Cloud:        e.cloud,
			ResourceType: planResourceType,
			ResourceID:   "vol-" + e.id,
			ProjectID:    e.projectID,
			Source:       string(e.source),
		}); err != nil {
			t.Fatalf("seeding event %s: %v", e.id, err)
		}
	}
}

// seedPlanTransfer writes the history of one resource that changed hands inside
// the plan window. Every event but the last names the project that held it
// first; the last one is the transfer, which is the only event the new project
// stores under its own name and the only one a count narrowed to it would see.
func seedPlanTransfer(t *testing.T, db storetest.DB) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
		                     resource_type, resource_id, project_id, source)
		 SELECT 'plan-transfer-' || n, $1::timestamptz + (n * INTERVAL '1 minute'),
		        'volume.update', $2, $3, $4, 'vol-plan-transfer',
		        CASE WHEN n = $5::int THEN $7 ELSE $6 END, 'collector'
		 FROM generate_series(1, $5::int) AS n`,
		planWindowStart, planPlatform, planCloud, planResourceType,
		planTransferEvents, planOtherProject, planProject); err != nil {
		t.Fatalf("seeding the history of a transferred resource: %v", err)
	}
}

// seedPlanResources writes the projection rows the resource plan is taken over.
// Every one of the five dimensions varies across them, so the grouping the query
// runs has more than one row to fold.
func seedPlanResources(t *testing.T, db storetest.DB) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO current_resources (cloud, platform, resource_type, resource_id,
		                                project_id, state, last_event_type, last_event_at)
		 VALUES ($1, $2, 'volume',   'vol-plan-1', $3, 'available', 'volume.create',   $5),
		        ($1, $2, 'volume',   'vol-plan-2', $4, 'in-use',    'volume.update',   $5),
		        ($1, $2, 'instance', 'i-plan-1',   $3, 'active',    'instance.create', $5),
		        ($6, $2, 'instance', 'i-plan-2',   $4, 'deleted',   'instance.delete', $5)`,
		planCloud, planPlatform, planProject, planOtherProject, planWindowStart,
		planOtherCloud); err != nil {
		t.Fatalf("seeding the projection: %v", err)
	}
}

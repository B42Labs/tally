package adapters_test

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/reconciliation/adapters"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// The coordinates every configured cloud is reconciled under. The platform is
// the one the adapter speaks for and the one the migration chain seeds the size
// schemas of, so a correction is measured against the schema an operator's own
// database holds.
const (
	platform    = "openstack"
	adapterName = "openstack"
)

// The two projects the recorded listings hand their resources to.
const (
	projectA = "4c9d2f6b81e34a7f9b3c5d8e0a1f2b34"
	projectB = "8a1b7c6d5e4f40398271a6b5c4d3e2f1"
)

// The statuses a finished run leaves in sync_runs.
const (
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// pollTime is what the injected clock answers, and therefore what a correction
// the platform gave no instant for is dated at. It falls past every instant the
// recorded listings carry, so such a correction is the newest event of its row
// rather than one the diff has to date past what the row already holds.
var pollTime = time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)

// The sizes the flavors of the recorded servers amount to, as the projection
// stores them. The adapter builds them from exact decimals and the database
// hands them back as JSON numbers, which decode as float64: 512 MiB is half a
// gibibyte, and the disk is the root disk plus the ephemeral one.
var (
	sizeOfSmall = map[string]any{"vcpus": 1.0, "ram_gb": 0.5, "disk_gb": 30.0, "flavor": "m1.small"}
	sizeOfTiny  = map[string]any{"vcpus": 1.0, "ram_gb": 1.0, "disk_gb": 5.0, "flavor": "m1.tiny"}
	sizeOfLarge = map[string]any{"vcpus": 4.0, "ram_gb": 8.0, "disk_gb": 80.0, "flavor": "m1.large"}
)

// reload is what one cloud holds on the next run: it replaces the recorded
// pages the live listing at path answers from and rewinds it to the first of
// them. Registering a second listing would not do, because a request is
// answered by the first registered one that matches it.
//
// A listing left without a page answers with a 500, which is how a run meets a
// service that stopped answering between two syncs.
func (c *cloud) reload(t *testing.T, path string, fixtures ...string) {
	t.Helper()

	pages := make([][]byte, 0, len(fixtures))
	for _, name := range fixtures {
		pages = append(pages, c.fixture(t, name))
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, registered := range c.listings[path] {
		// A request without deleted=true reaches the live listing, the half
		// every path has. The deleted half of the servers listing keeps the
		// pages it was registered with: the first run of a cloud never asks for
		// it, so they are still there for the second.
		if !registered.answers(url.Values{}) {
			continue
		}
		registered.pages, registered.served = pages, 0
		return
	}
	t.Fatalf("no listing is registered for %s", path)
}

// TestOpenStackSync drives the OpenStack adapter through the reconciliation
// framework, over the path a deployment runs: a run reads the clouds.yaml entry
// its adapter_config names, authenticates against the recorded Keystone, walks
// the catalog that Keystone publishes, and hands what it observed to the diff.
// The corrections take the strict ingest pipeline into a migrated database, so
// the size schemas the migration chain seeds measure every one of them.
//
// The subtests share one database and one process-wide clouds.yaml, so each of
// them works on a cloud of its own and none of them runs in parallel.
func TestOpenStackSync(t *testing.T) {
	db := storetest.NewDB(t)
	// Strict, which is what a production deployment ingests with: a size no
	// registered schema accepts is dead-lettered rather than stored, and the run
	// that emitted it fails.
	pipeline := ingest.New(registry.New(), true, nil, nil)

	t.Run("creates the instances the projection does not hold", func(t *testing.T) {
		const cloud = "os-openstack-create"
		openstack := newCloud(t)
		openstack.serve(t, serversPath, "servers_page1.json", "servers_page2.json")
		writeCloudsYAML(t, openstack.URL)
		syncer := newSyncer(t, db, pipeline, cloud, map[string]any{"os_cloud": testCloud})

		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(3, 0, 0))
		assertRun(t, db, res, statusCompleted)
		// Every create carries nova's own creation instant: a resource the
		// projection missed existed from the moment the platform says it did,
		// not from the poll that discovered it.
		assertCorrections(t, db, cloud, "sync.create", []correction{
			{
				resourceType: "instance", resourceID: "2d8b6e10-4f3c-49a5-b7d2-6c1a8e5f0937",
				projectID: projectB, at: "2026-07-20T16:45:02Z", state: "shutoff", size: sizeOfTiny,
			},
			{
				resourceType: "instance", resourceID: "7f3a1c58-9d2b-4e17-8c6a-2b5d0e9f4a31",
				projectID: projectA, at: "2026-07-14T09:12:33Z", state: "active", size: sizeOfSmall,
			},
			{
				resourceType: "instance", resourceID: "9c4f7a23-1e6d-4b80-95af-3d2c7b1e6048",
				projectID: projectB, at: "2026-07-21T08:03:44Z", state: "rescued", size: sizeOfTiny,
			},
		})
		assertRows(t, db, cloud, []projectionRow{
			{
				resourceType: "instance", resourceID: "2d8b6e10-4f3c-49a5-b7d2-6c1a8e5f0937",
				projectID: projectB, state: "shutoff", size: sizeOfTiny,
				createdAt: "2026-07-20T16:45:02Z",
			},
			{
				resourceType: "instance", resourceID: "7f3a1c58-9d2b-4e17-8c6a-2b5d0e9f4a31",
				projectID: projectA, state: "active", size: sizeOfSmall,
				createdAt: "2026-07-14T09:12:33Z",
			},
			{
				resourceType: "instance", resourceID: "9c4f7a23-1e6d-4b80-95af-3d2c7b1e6048",
				projectID: projectB, state: "rescued", size: sizeOfTiny,
				createdAt: "2026-07-21T08:03:44Z",
			},
		})
	})

	t.Run("updates the instances that drifted and leaves the one that did not", func(t *testing.T) {
		const cloud = "os-openstack-update"
		openstack := newCloud(t)
		openstack.serve(t, serversPath, "servers_drift_before.json")
		openstack.serveDeleted(t, "servers_empty.json")
		writeCloudsYAML(t, openstack.URL)
		syncer := newSyncer(t, db, pipeline, cloud, map[string]any{"os_cloud": testCloud})
		assertStats(t, mustSync(t, syncer, cloud).Stats, tally(4, 0, 0))

		// The same cloud, one resize, one shutdown and one transfer later. The
		// fourth server is reported exactly as before, and a size the adapter
		// built twice out of one flavor must not read as drift.
		openstack.reload(t, serversPath, "servers_drift_after.json")
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 3, 0))
		assertRun(t, db, res, statusCompleted)
		// A drift the platform puts no instant on is booked at the poll: nova
		// says what an instance is now, never when it became that.
		assertCorrections(t, db, cloud, "sync.update", []correction{
			{
				resourceType: "instance", resourceID: "2f6c9d41-8a35-4b72-9e0d-1c4a7b3e5d80",
				projectID: projectA, at: rfc(pollTime), state: "active", size: sizeOfLarge,
			},
			{
				resourceType: "instance", resourceID: "3b7e0a52-9c46-4d83-8f1e-2d5b8c4f6a91",
				projectID: projectA, at: rfc(pollTime), state: "shutoff", size: sizeOfTiny,
			},
			{
				// The correction names the owner nova reports rather than the one
				// the row still holds.
				resourceType: "instance", resourceID: "4c8f1b63-0d57-4e94-9a2f-3e6c9d5a7b02",
				projectID: projectB, at: rfc(pollTime), state: "active", size: sizeOfTiny,
			},
		})
		assertRows(t, db, cloud, []projectionRow{
			{
				resourceType: "instance", resourceID: "2f6c9d41-8a35-4b72-9e0d-1c4a7b3e5d80",
				projectID: projectA, state: "active", size: sizeOfLarge,
				createdAt: "2026-05-04T07:20:11Z",
			},
			{
				resourceType: "instance", resourceID: "3b7e0a52-9c46-4d83-8f1e-2d5b8c4f6a91",
				projectID: projectA, state: "shutoff", size: sizeOfTiny,
				createdAt: "2026-05-06T11:02:44Z",
			},
			{
				resourceType: "instance", resourceID: "4c8f1b63-0d57-4e94-9a2f-3e6c9d5a7b02",
				projectID: projectB, state: "active", size: sizeOfTiny,
				createdAt: "2026-05-09T15:41:07Z",
			},
			{
				resourceType: "instance", resourceID: "5d9a2c74-1e68-4f05-8b3a-4f7d0e6b8c13",
				projectID: projectB, state: "active", size: sizeOfTiny,
				createdAt: "2026-05-12T06:15:29Z",
			},
		})
	})

	t.Run("deletes what nova destroyed, at the instant nova destroyed it", func(t *testing.T) {
		const cloud = "os-openstack-delete-dated"
		openstack := newCloud(t)
		openstack.serve(t, serversPath, "servers_doomed.json")
		openstack.serveDeleted(t, "servers_deleted.json")
		writeCloudsYAML(t, openstack.URL)
		syncer := newSyncer(t, db, pipeline, cloud, map[string]any{"os_cloud": testCloud})
		assertStats(t, mustSync(t, syncer, cloud).Stats, tally(2, 0, 0))

		// A cloud that never completed a run has no window behind it to catch up
		// on, so this first run asked for the live listing alone.
		if got := len(openstack.requestsTo(serversPath)); got != 1 {
			t.Fatalf("the first run made %d requests to %s, want the live listing alone",
				got, serversPath)
		}

		// Both instances are gone by the next run, and nova names them in the
		// listing of what it destroyed since the run before.
		openstack.reload(t, serversPath, "servers_empty.json")
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 2))
		assertRun(t, db, res, statusCompleted)
		// The instant is nova's own, to the precision nova reported it in. It is
		// the whole point of the deleted listing: an invoice is written off the
		// hours a resource ran, and a delete booked at the poll bills every hour
		// between the two.
		assertCorrections(t, db, cloud, "sync.delete", []correction{
			{
				resourceType: "instance", resourceID: "3a9e5c07-2b81-4d6f-9a34-5c7e1b0d8f26",
				// The owner is the row's: who held the instance is settled history
				// by the time nova reports it gone.
				projectID: projectA, at: "2026-08-14T10:31:07Z",
			},
			{
				resourceType: "instance", resourceID: "b6d2f483-7e15-4a90-8c73-0d5b9a1e6c42",
				projectID: projectB, at: "2026-08-15T22:03:41.5Z",
			},
		})
		assertRows(t, db, cloud, []projectionRow{
			{
				resourceType: "instance", resourceID: "3a9e5c07-2b81-4d6f-9a34-5c7e1b0d8f26",
				projectID: projectA, state: "deleted", size: sizeOfSmall,
				createdAt: "2026-07-02T13:44:10Z", deletedAt: "2026-08-14T10:31:07Z",
			},
			{
				resourceType: "instance", resourceID: "b6d2f483-7e15-4a90-8c73-0d5b9a1e6c42",
				projectID: projectB, state: "deleted", size: sizeOfTiny,
				createdAt: "2026-06-19T07:05:55Z", deletedAt: "2026-08-15T22:03:41.5Z",
			},
		})
	})

	t.Run("deletes a live row no listing named, at the poll", func(t *testing.T) {
		const cloud = "os-openstack-delete-absent"
		openstack := newCloud(t)
		openstack.serve(t, serversPath, "servers_page2.json")
		openstack.serveDeleted(t, "servers_empty.json")
		writeCloudsYAML(t, openstack.URL)
		syncer := newSyncer(t, db, pipeline, cloud, map[string]any{"os_cloud": testCloud})
		assertStats(t, mustSync(t, syncer, cloud).Stats, tally(2, 0, 0))

		// Nova holds neither instance any more and names neither of them as
		// destroyed within the window: a delete that fell outside it, or one the
		// cloud has already purged. The absence is all this run has to go on.
		openstack.reload(t, serversPath, "servers_empty.json")
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 2))
		assertRun(t, db, res, statusCompleted)
		assertCorrections(t, db, cloud, "sync.delete", []correction{
			{
				resourceType: "instance", resourceID: "2d8b6e10-4f3c-49a5-b7d2-6c1a8e5f0937",
				projectID: projectB, at: rfc(pollTime),
			},
			{
				resourceType: "instance", resourceID: "9c4f7a23-1e6d-4b80-95af-3d2c7b1e6048",
				projectID: projectB, at: rfc(pollTime),
			},
		})
		assertRows(t, db, cloud, []projectionRow{
			{
				resourceType: "instance", resourceID: "2d8b6e10-4f3c-49a5-b7d2-6c1a8e5f0937",
				projectID: projectB, state: "deleted", size: sizeOfTiny,
				createdAt: "2026-07-20T16:45:02Z", deletedAt: rfc(pollTime),
			},
			{
				resourceType: "instance", resourceID: "9c4f7a23-1e6d-4b80-95af-3d2c7b1e6048",
				projectID: projectB, state: "deleted", size: sizeOfTiny,
				createdAt: "2026-07-21T08:03:44Z", deletedAt: rfc(pollTime),
			},
		})
	})

	t.Run("keeps every volume when cinder stops answering", func(t *testing.T) {
		const cloud = "os-openstack-cinder-down"
		openstack := newCloud(t)
		openstack.serve(t, serversPath, "servers_page2.json")
		openstack.serveDeleted(t, "servers_empty.json")
		openstack.serve(t, volumesPath, "volumes.json")
		writeCloudsYAML(t, openstack.URL)
		syncer := newSyncer(t, db, pipeline, cloud, map[string]any{"os_cloud": testCloud})
		assertStats(t, mustSync(t, syncer, cloud).Stats, tally(4, 0, 0))

		// Cinder answers the next run with a 500 while nova enumerates cleanly
		// and holds nothing. A partial outage is missing information about the
		// one service it hit, never a fleet that was deleted.
		openstack.reload(t, serversPath, "servers_empty.json")
		openstack.reload(t, volumesPath)
		res, err := syncer.Sync(t.Context(), cloud)

		if err == nil {
			t.Fatal("Sync() error = nil, want the run reporting the listing it could not read")
		}
		if res.Stats.Created != 0 || res.Stats.Updated != 0 || res.Stats.Deleted != 2 {
			t.Errorf("stats = %+v, want the two instances nova enumerated to its end deleted",
				res.Stats)
		}
		if len(res.Stats.Errors) != 1 || !strings.Contains(res.Stats.Errors[0], "enumerating volume") {
			t.Errorf("stats errors = %v, want one naming the volume listing that failed",
				res.Stats.Errors)
		}
		// The run row carries the same tally and the same reason the caller was
		// handed, which is what an operator reads the outage off.
		assertRun(t, db, res, statusFailed)
		assertRows(t, db, cloud, []projectionRow{
			{
				resourceType: "instance", resourceID: "2d8b6e10-4f3c-49a5-b7d2-6c1a8e5f0937",
				projectID: projectB, state: "deleted", size: sizeOfTiny,
				createdAt: "2026-07-20T16:45:02Z", deletedAt: rfc(pollTime),
			},
			{
				resourceType: "instance", resourceID: "9c4f7a23-1e6d-4b80-95af-3d2c7b1e6048",
				projectID: projectB, state: "deleted", size: sizeOfTiny,
				createdAt: "2026-07-21T08:03:44Z", deletedAt: rfc(pollTime),
			},
			{
				resourceType: "volume", resourceID: "5b8c2d17-6e94-4a30-b7f1-2c8d5e0a9b64",
				projectID: projectA, state: "in-use",
				size:      map[string]any{"size_gb": 100.0, "type": "ssd"},
				createdAt: "2026-06-02T08:15:00Z",
			},
			{
				resourceType: "volume", resourceID: "c3f9a041-8b25-4d67-9e13-7a6c2b4d8e50",
				projectID: projectB, state: "error_deleting",
				size:      map[string]any{"size_gb": 25.0, "type": "hdd"},
				createdAt: "2026-06-11T21:03:44Z",
			},
		})
	})

	t.Run("creates a load balancer the seeded schema accepts", func(t *testing.T) {
		const cloud = "os-openstack-octavia"
		openstack := newCloud(t)
		openstack.serve(t, loadBalancersPath, "loadbalancers.json")
		writeCloudsYAML(t, openstack.URL)
		syncer := newSyncer(t, db, pipeline, cloud,
			map[string]any{"os_cloud": testCloud, "include_octavia": true})

		res := mustSync(t, syncer, cloud)

		// The strict pipeline refuses a size for a pair no schema registers, so a
		// clean run over a load balancer is what says migration 0006 reached this
		// database: without it the correction would be dead-lettered and the run
		// would end failed.
		assertStats(t, res.Stats, tally(1, 0, 0))
		assertRun(t, db, res, statusCompleted)
		assertRows(t, db, cloud, []projectionRow{{
			resourceType: "loadbalancer", resourceID: "4a5b6c7d-8e90-4123-a456-7b8c9d0e1f23",
			projectID: projectA, state: "active",
			size:      map[string]any{"listeners": 2.0, "pools": 1.0},
			createdAt: "2026-04-20T10:00:00Z",
		}})
	})
}

// newSyncer builds a Syncer that reconciles one cloud through the production
// OpenStack adapter, on a clock that always answers pollTime so that a
// correction dated at the poll is one a subtest can name.
func newSyncer(t *testing.T, db storetest.DB, pipeline *ingest.Pipeline, cloud string,
	adapterConfig map[string]any,
) *reconciliation.Syncer {
	t.Helper()

	cfg := reconciliation.Config{Clouds: []reconciliation.CloudConfig{{
		Cloud: cloud, Platform: platform, Adapter: adapterName, AdapterConfig: adapterConfig,
	}}}
	return reconciliation.New(db.Store, pipeline, cfg,
		map[string]reconciliation.Adapter{adapterName: adapters.NewOpenStack(time.Now, discardLogs)},
		func() time.Time { return pollTime }, nil)
}

// mustSync runs one sync and fails the test unless it finished clean.
func mustSync(t *testing.T, syncer *reconciliation.Syncer, cloud string) reconciliation.Result {
	t.Helper()

	res, err := syncer.Sync(t.Context(), cloud)
	if err != nil {
		t.Fatalf("Sync() error = %v, want nil", err)
	}
	return res
}

// tally is the stats of a run that recorded no error.
func tally(created, updated, deleted int) reconciliation.Stats {
	return reconciliation.Stats{
		Created: created, Updated: updated, Deleted: deleted, Errors: []string{},
	}
}

// assertStats fails the test unless the run did exactly what want describes.
func assertStats(t *testing.T, got, want reconciliation.Stats) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Errorf("stats = %+v, want %+v", got, want)
	}
}

// assertRun fails the test unless the stored run row ended at status carrying
// the stats the caller was handed. The two are one record: what an operator
// reads in sync_runs is what the caller of the sync was told.
func assertRun(t *testing.T, db storetest.DB, res reconciliation.Result, status string) {
	t.Helper()

	var (
		stored      string
		completedAt pgtype.Timestamptz
		raw         []byte
	)
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT status, completed_at, stats FROM sync_runs WHERE id = $1::uuid`,
		res.RunID).Scan(&stored, &completedAt, &raw); err != nil {
		t.Fatalf("reading sync run %s: %v", res.RunID, err)
	}
	if stored != status {
		t.Errorf("run status = %q, want %q", stored, status)
	}
	if !completedAt.Valid {
		t.Error("completed at = NULL, want the instant the run ended")
	}

	var stats reconciliation.Stats
	if err := json.Unmarshal(raw, &stats); err != nil {
		t.Fatalf("decoding the stored stats: %v", err)
	}
	if !reflect.DeepEqual(stats, res.Stats) {
		t.Errorf("stored stats = %+v, want the ones the caller got, %+v", stats, res.Stats)
	}
}

// correction is a stored synthetic event as the assertions compare it. The
// instant is kept as text so that a whole batch compares in one DeepEqual.
type correction struct {
	resourceType string
	resourceID   string
	projectID    string
	at           string
	state        string
	size         map[string]any
}

// assertCorrections fails the test unless the corrections of one event type
// stored for cloud are exactly want, in the order the resources sort in.
func assertCorrections(t *testing.T, db storetest.DB, cloud, eventType string, want []correction) {
	t.Helper()

	found, err := db.Store.Pool().Query(t.Context(),
		`SELECT resource_type, resource_id, project_id, timestamp, payload
		 FROM events
		 WHERE cloud = $1 AND source = 'reconciliation' AND event_type = $2
		 ORDER BY resource_type, resource_id, timestamp`, cloud, eventType)
	if err != nil {
		t.Fatalf("reading the %s corrections of %s: %v", eventType, cloud, err)
	}
	defer found.Close()

	var got []correction
	for found.Next() {
		var (
			stored  correction
			ts      time.Time
			payload []byte
		)
		if err := found.Scan(&stored.resourceType, &stored.resourceID, &stored.projectID,
			&ts, &payload); err != nil {
			t.Fatalf("scanning a correction: %v", err)
		}

		var envelope event.PayloadEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			t.Fatalf("decoding the payload of a correction: %v", err)
		}
		if envelope.State != nil {
			stored.state = *envelope.State
		}
		stored.at, stored.size = rfc(ts), envelope.Size
		got = append(got, stored)
	}
	if err := found.Err(); err != nil {
		t.Fatalf("reading the %s corrections of %s: %v", eventType, cloud, err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored %s corrections = %+v, want %+v", eventType, got, want)
	}
}

// projectionRow is a current_resources row as the assertions compare it. The
// instants are kept as text, empty for a column holding NULL, so that a whole
// snapshot compares in one DeepEqual.
type projectionRow struct {
	resourceType string
	resourceID   string
	projectID    string
	state        string
	size         map[string]any
	createdAt    string
	deletedAt    string
}

// assertRows fails the test unless the projection of cloud is exactly want,
// ordered by resource.
func assertRows(t *testing.T, db storetest.DB, cloud string, want []projectionRow) {
	t.Helper()

	found, err := db.Store.Pool().Query(t.Context(),
		`SELECT resource_type, resource_id, project_id, state, size, created_at, deleted_at
		 FROM current_resources
		 WHERE cloud = $1
		 ORDER BY resource_type, resource_id`, cloud)
	if err != nil {
		t.Fatalf("reading the projection rows of %s: %v", cloud, err)
	}
	defer found.Close()

	var got []projectionRow
	for found.Next() {
		var (
			row                  projectionRow
			size                 []byte
			createdAt, deletedAt pgtype.Timestamptz
		)
		if err := found.Scan(&row.resourceType, &row.resourceID, &row.projectID, &row.state,
			&size, &createdAt, &deletedAt); err != nil {
			t.Fatalf("scanning a projection row: %v", err)
		}
		if err := json.Unmarshal(size, &row.size); err != nil {
			t.Fatalf("decoding the size of %s: %v", row.resourceID, err)
		}
		if createdAt.Valid {
			row.createdAt = rfc(createdAt.Time)
		}
		if deletedAt.Valid {
			row.deletedAt = rfc(deletedAt.Time)
		}
		got = append(got, row)
	}
	if err := found.Err(); err != nil {
		t.Fatalf("reading the projection rows of %s: %v", cloud, err)
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("projection rows = %+v, want %+v", got, want)
	}
}

// rfc renders an instant the way the assertions compare instants, which is the
// way the recorded listings spell the ones they carry.
func rfc(ts time.Time) string {
	return ts.UTC().Format(time.RFC3339Nano)
}

package projection_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/metrics"
	"github.com/b42labs/tally/internal/reporting/projection"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// The fixtures all report one platform and two clouds, which is what lets a
// filtered rebuild be told apart from a full one.
const (
	platform   = "openstack"
	cloud      = "os-projection"
	otherCloud = "os-projection-second"
	projectA   = "project-a"
	projectB   = "project-b"
)

// The instants the fixtures use. They are whole seconds, so what Postgres stores
// compares equal to what the test passed in.
var (
	createTime = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	resizeTime = createTime.Add(time.Hour)
	deleteTime = createTime.Add(2 * time.Hour)
)

// projectionRow is a current_resources row as the assertions read it back
// through the pool, which is the way an operator would read it.
type projectionRow struct {
	platform      string
	projectID     string
	state         string
	size          map[string]any
	createdAt     pgtype.Timestamptz
	deletedAt     pgtype.Timestamptz
	lastEventType string
	lastEventAt   pgtype.Timestamptz
	lastPayload   map[string]any
}

// want is the row a step is expected to leave behind. A nil createdAt or
// deletedAt is the NULL column.
type want struct {
	state         string
	size          map[string]any
	projectID     string
	createdAt     *time.Time
	deletedAt     *time.Time
	lastEventType string
	lastEventAt   time.Time
}

func TestApply(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("folds an in-order create, resize, and delete", func(t *testing.T) {
		key := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-in-order"}

		ingest(t, db, key, resourceEvent(key, "vol-in-order-create", "volume.create",
			createTime, payloadOf("available", sizeGB(10))))
		assertRow(t, db, key, want{
			state:         "available",
			size:          sizeGB(10),
			projectID:     projectA,
			createdAt:     &createTime,
			lastEventType: "volume.create",
			lastEventAt:   createTime,
		})

		// The row also keeps the payload the event carried, which is what the
		// lifecycle endpoint reports as the last thing the platform said.
		row, _ := readRow(t, db, key)
		if state, _ := row.lastPayload["state"].(string); state != "available" {
			t.Errorf("last payload = %v, want it to carry the state of the create", row.lastPayload)
		}

		// The resize hands the volume to another project, which the row follows:
		// the top-level project id of an event is always authoritative.
		resize := resourceEvent(key, "vol-in-order-resize", "volume.resize",
			resizeTime, payloadOf("available", sizeGB(20)))
		resize.ProjectID = projectB
		ingest(t, db, key, resize)
		assertRow(t, db, key, want{
			state:         "available",
			size:          sizeGB(20),
			projectID:     projectB,
			createdAt:     &createTime,
			lastEventType: "volume.resize",
			lastEventAt:   resizeTime,
		})

		// A delete carries no payload at all: the core sets the state itself.
		remove := resourceEvent(key, "vol-in-order-delete", "volume.delete",
			deleteTime, event.PayloadEnvelope{})
		remove.ProjectID = projectB
		ingest(t, db, key, remove)
		assertRow(t, db, key, want{
			state:         "deleted",
			size:          sizeGB(20),
			projectID:     projectB,
			createdAt:     &createTime,
			deletedAt:     &deleteTime,
			lastEventType: "volume.delete",
			lastEventAt:   deleteTime,
		})
	})

	t.Run("keeps the row of a deleted resource", func(t *testing.T) {
		key := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-deleted"}

		ingest(t, db, key, resourceEvent(key, "vol-deleted-create", "volume.create",
			createTime, payloadOf("available", sizeGB(10))))
		ingest(t, db, key, resourceEvent(key, "vol-deleted-delete", "volume.delete",
			deleteTime, event.PayloadEnvelope{}))

		row, ok := readRow(t, db, key)
		if !ok {
			t.Fatal("the deleted resource lost its projection row, want it kept")
		}
		if row.state != "deleted" {
			t.Errorf("state = %q, want %q", row.state, "deleted")
		}
		if !row.deletedAt.Valid || !row.deletedAt.Time.Equal(deleteTime) {
			t.Errorf("deleted at = %#v, want %s", row.deletedAt, deleteTime)
		}
		// The size the resource had when it went away stays readable, because
		// metering bills the interval that ends at the delete.
		if !reflect.DeepEqual(row.size, sizeGB(10)) {
			t.Errorf("size = %v, want the size the resource was deleted with, %v", row.size, sizeGB(10))
		}
	})

	t.Run("replays when an event arrives after the delete it precedes", func(t *testing.T) {
		late := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-late"}
		reference := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-reference"}

		ingest(t, db, late, resourceEvent(late, "vol-late-create", "volume.create",
			createTime, payloadOf("available", sizeGB(10))))
		ingest(t, db, late, resourceEvent(late, "vol-late-delete", "volume.delete",
			deleteTime, event.PayloadEnvelope{}))
		// The resize belongs between the two events already folded, so Apply cannot
		// fold it on top and has to rebuild the row from the history.
		ingest(t, db, late, resourceEvent(late, "vol-late-resize", "volume.resize",
			resizeTime, payloadOf("available", sizeGB(20))))

		assertRow(t, db, late, want{
			state:         "deleted",
			size:          sizeGB(20),
			projectID:     projectA,
			createdAt:     &createTime,
			deletedAt:     &deleteTime,
			lastEventType: "volume.delete",
			lastEventAt:   deleteTime,
		})

		ingest(t, db, reference, resourceEvent(reference, "vol-reference-create", "volume.create",
			createTime, payloadOf("available", sizeGB(10))))
		ingest(t, db, reference, resourceEvent(reference, "vol-reference-resize", "volume.resize",
			resizeTime, payloadOf("available", sizeGB(20))))
		ingest(t, db, reference, resourceEvent(reference, "vol-reference-delete", "volume.delete",
			deleteTime, event.PayloadEnvelope{}))

		rows := snapshots(t, db)
		if rows[late] != rows[reference] {
			t.Errorf("row of the late arrival = %s, want the row of in-order ingestion, %s",
				rows[late], rows[reference])
		}
	})

	t.Run("counts the replay a late event causes and nothing else", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		m := metrics.New(reg)
		key := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-late-counted"}

		meteredIngest(t, db, key, resourceEvent(key, "vol-late-counted-create", "volume.create",
			createTime, payloadOf("available", sizeGB(10))), m)
		meteredIngest(t, db, key, resourceEvent(key, "vol-late-counted-delete", "volume.delete",
			deleteTime, event.PayloadEnvelope{}), m)
		// Both events arrived in order, so both folds took the incremental path
		// and the counter has no sample at all.
		assertReplays(t, reg, map[string]float64{})

		meteredIngest(t, db, key, resourceEvent(key, "vol-late-counted-resize", "volume.resize",
			resizeTime, payloadOf("available", sizeGB(20))), m)
		assertReplays(t, reg, map[string]float64{cloud: 1})
	})

	t.Run("lets the batch applied last win a timestamp tie", func(t *testing.T) {
		for _, order := range [][2]string{{"paused", "active"}, {"active", "paused"}} {
			first, second := order[0], order[1]

			t.Run(first+" then "+second, func(t *testing.T) {
				key := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-tie-" + first}

				ingest(t, db, key, resourceEvent(key, key.ResourceID+"-create", "volume.create",
					createTime, payloadOf("available", sizeGB(10))))
				// Both updates report the same instant, so only the received_at the
				// database assigns tells them apart.
				ingest(t, db, key, resourceEvent(key, key.ResourceID+"-first", "volume.update",
					resizeTime, payloadOf(first, nil)))
				ingest(t, db, key, resourceEvent(key, key.ResourceID+"-second", "volume.update",
					resizeTime, payloadOf(second, nil)))

				assertRow(t, db, key, want{
					state:         second,
					size:          sizeGB(10),
					projectID:     projectA,
					createdAt:     &createTime,
					lastEventType: "volume.update",
					lastEventAt:   resizeTime,
				})
			})
		}
	})
}

func TestReplay(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("projects a history that never saw the create", func(t *testing.T) {
		key := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-without-create"}

		record(t, db, resourceEvent(key, "vol-without-create-first", "volume.update",
			createTime, payloadOf("available", sizeGB(10))))
		record(t, db, resourceEvent(key, "vol-without-create-second", "volume.update",
			resizeTime, payloadOf("paused", nil)))

		replay(t, db, key)

		row, ok := readRow(t, db, key)
		if !ok {
			t.Fatal("no projection row, want one for the history without a create")
		}
		// When the create was missed, the resource's start is unknown rather than
		// the first event's timestamp.
		if row.createdAt.Valid {
			t.Errorf("created at = %s, want NULL", row.createdAt.Time)
		}
		if row.state != "paused" {
			t.Errorf("state = %q, want %q", row.state, "paused")
		}
		if !reflect.DeepEqual(row.size, sizeGB(10)) {
			t.Errorf("size = %v, want the size carried forward, %v", row.size, sizeGB(10))
		}
	})

	t.Run("writes no row for a key without history", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		key := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-without-history"}

		meteredReplay(t, db, key, metrics.New(reg))

		if _, ok := readRow(t, db, key); ok {
			t.Error("Replay() wrote a projection row, want none for a key without events")
		}
		// The counter reports how often the replay path was taken, so this run
		// counts although it wrote nothing.
		assertReplays(t, reg, map[string]float64{cloud: 1})
	})

	t.Run("keeps the size of a resource created and deleted at one instant", func(t *testing.T) {
		key := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-instantaneous"}

		// A platform reporting whole seconds gives a short-lived resource a
		// create and a delete at the same instant. The interval between them
		// bills nothing and is dropped, but the size the history reported is
		// still what the row has to carry: it is what the resource was billed
		// for, and the events cannot un-lose it once the fold has dropped it.
		ingest(t, db, key, resourceEvent(key, "vol-instantaneous-create", "volume.create",
			createTime, payloadOf("available", sizeGB(10))))
		ingest(t, db, key, resourceEvent(key, "vol-instantaneous-delete", "volume.delete",
			createTime, event.PayloadEnvelope{}))
		folded, ok := readRow(t, db, key)
		if !ok {
			t.Fatal("no projection row after ingestion, want one")
		}

		replay(t, db, key)

		replayed, ok := readRow(t, db, key)
		if !ok {
			t.Fatal("no projection row after the replay, want one")
		}
		if !reflect.DeepEqual(replayed.size, sizeGB(10)) {
			t.Errorf("size after the replay = %v, want the %v the history reported",
				replayed.size, sizeGB(10))
		}
		if !reflect.DeepEqual(replayed, folded) {
			t.Errorf("row after the replay = %+v, want the %+v the incremental fold wrote",
				replayed, folded)
		}
	})
}

func TestRebuild(t *testing.T) {
	db := storetest.NewDB(t)

	liveVolume := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-live"}
	goneVolume := projection.Key{Cloud: cloud, ResourceType: "volume", ResourceID: "vol-gone"}
	instance := projection.Key{Cloud: otherCloud, ResourceType: "instance", ResourceID: "inst-live"}

	ingest(t, db, liveVolume, resourceEvent(liveVolume, "vol-live-create", "volume.create",
		createTime, payloadOf("available", sizeGB(10))))

	ingest(t, db, goneVolume, resourceEvent(goneVolume, "vol-gone-create", "volume.create",
		createTime, payloadOf("available", sizeGB(10))))
	ingest(t, db, goneVolume, resourceEvent(goneVolume, "vol-gone-resize", "volume.resize",
		resizeTime, payloadOf("available", sizeGB(20))))
	ingest(t, db, goneVolume, resourceEvent(goneVolume, "vol-gone-delete", "volume.delete",
		deleteTime, event.PayloadEnvelope{}))

	ingest(t, db, instance, resourceEvent(instance, "inst-live-create", "instance.create",
		createTime, payloadOf("active", map[string]any{"vcpus": float64(2)})))
	ingest(t, db, instance, resourceEvent(instance, "inst-live-pause", "instance.pause",
		resizeTime, payloadOf("paused", nil)))

	// What ingestion folded is the result every rebuild has to reproduce.
	ingested := snapshots(t, db)
	if len(ingested) != 3 {
		t.Fatalf("projection rows after the seed = %d, want 3", len(ingested))
	}

	t.Run("rebuilds every key when the filter is empty", func(t *testing.T) {
		corrupt(t, db)

		rebuilt, err := projection.Rebuild(t.Context(), db.Store, projection.Filter{}, nil)
		if err != nil {
			t.Fatalf("Rebuild() error = %v, want nil", err)
		}
		if rebuilt != len(ingested) {
			t.Errorf("Rebuild() = %d, want %d", rebuilt, len(ingested))
		}
		assertSnapshots(t, snapshots(t, db), ingested)
	})

	t.Run("rebuilds only the cloud the filter names", func(t *testing.T) {
		broken := corrupt(t, db)

		rebuilt, err := projection.Rebuild(t.Context(), db.Store, projection.Filter{Cloud: cloud}, nil)
		if err != nil {
			t.Fatalf("Rebuild() error = %v, want nil", err)
		}
		if rebuilt != 2 {
			t.Errorf("Rebuild() = %d, want the 2 keys of cloud %s", rebuilt, cloud)
		}

		// The cloud outside the filter keeps the state the corruption gave it.
		assertSnapshots(t, snapshots(t, db), map[projection.Key]string{
			liveVolume: ingested[liveVolume],
			goneVolume: ingested[goneVolume],
			instance:   broken[instance],
		})
	})

	t.Run("rebuilds only the resource type the filter names", func(t *testing.T) {
		broken := corrupt(t, db)

		rebuilt, err := projection.Rebuild(t.Context(), db.Store,
			projection.Filter{ResourceType: "volume"}, nil)
		if err != nil {
			t.Fatalf("Rebuild() error = %v, want nil", err)
		}
		if rebuilt != 2 {
			t.Errorf("Rebuild() = %d, want the 2 volume keys", rebuilt)
		}

		// The instance is of another type and keeps the state the corruption
		// gave it, whatever cloud it lives in.
		assertSnapshots(t, snapshots(t, db), map[projection.Key]string{
			liveVolume: ingested[liveVolume],
			goneVolume: ingested[goneVolume],
			instance:   broken[instance],
		})
	})

	t.Run("writes nothing for a filter no key matches", func(t *testing.T) {
		broken := corrupt(t, db)

		rebuilt, err := projection.Rebuild(t.Context(), db.Store,
			projection.Filter{Cloud: cloud, ResourceType: "black_hole"}, nil)
		if err != nil {
			t.Fatalf("Rebuild() error = %v, want nil", err)
		}
		if rebuilt != 0 {
			t.Errorf("Rebuild() = %d, want 0", rebuilt)
		}
		assertSnapshots(t, snapshots(t, db), broken)
	})

	t.Run("counts one replay per key it walks", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		// Every key the run reaches is one replay, under the cloud it belongs to,
		// so the rows the fleet holds say what the counter has to hold. The
		// fixtures span two clouds, which is what makes the labels tell them apart.
		fleet := snapshots(t, db)
		want := map[string]float64{}
		for key := range fleet {
			want[key.Cloud]++
		}

		rebuilt, err := projection.Rebuild(t.Context(), db.Store, projection.Filter{}, metrics.New(reg))
		if err != nil {
			t.Fatalf("Rebuild() error = %v, want nil", err)
		}
		if rebuilt != len(fleet) {
			t.Errorf("Rebuild() = %d, want %d", rebuilt, len(fleet))
		}
		assertReplays(t, reg, want)
	})

	t.Run("replays a fleet that spans more than one transaction", func(t *testing.T) {
		// Rebuild replays the keys in transactions of 100. Everything above
		// takes the multi-transaction path, which is the only one a real fleet
		// ever takes and the one the accumulated count comes from.
		const (
			batched = "os-projection-batched"
			keys    = 101
		)
		want := map[projection.Key]string{}
		for i := range keys {
			key := projection.Key{
				Cloud:        batched,
				ResourceType: "volume",
				ResourceID:   fmt.Sprintf("vol-batched-%03d", i),
			}
			ingest(t, db, key, resourceEvent(key, key.ResourceID+"-create", "volume.create",
				createTime, payloadOf("available", sizeGB(10))))
			want[key] = ""
		}
		for key, snapshot := range snapshots(t, db) {
			if _, seeded := want[key]; seeded {
				want[key] = snapshot
			}
		}
		corrupt(t, db)

		rebuilt, err := projection.Rebuild(t.Context(), db.Store, projection.Filter{Cloud: batched}, nil)
		if err != nil {
			t.Fatalf("Rebuild() error = %v, want nil", err)
		}

		if rebuilt != keys {
			t.Errorf("Rebuild() = %d, want the %d keys of cloud %s", rebuilt, keys, batched)
		}
		got := snapshots(t, db)
		for key, snapshot := range want {
			if got[key] != snapshot {
				t.Errorf("row of %s = %s, want %s", key.ResourceID, got[key], snapshot)
			}
		}
	})
}

// resourceEvent builds one event of key. It reports the project the fixtures
// start from; a test that hands the resource on overwrites the project id.
func resourceEvent(key projection.Key, id, eventType string, ts time.Time, payload event.PayloadEnvelope) event.Event {
	return event.Event{
		EventID:      id,
		Timestamp:    ts,
		EventType:    eventType,
		Platform:     platform,
		Cloud:        key.Cloud,
		ResourceType: key.ResourceType,
		ResourceID:   key.ResourceID,
		ProjectID:    projectA,
		Source:       event.SourceCollector,
		Payload:      payload,
	}
}

// payloadOf is the payload of an event reporting a state and, when size is not
// nil, a new size. A nil size means the size did not change.
func payloadOf(state string, size map[string]any) event.PayloadEnvelope {
	return event.PayloadEnvelope{State: &state, Size: size}
}

// sizeGB is the size of a volume of gb gigabytes. The number is a float64
// because that is what it comes back as once it has been through JSON.
func sizeGB(gb float64) map[string]any {
	return map[string]any{"size_gb": gb}
}

// record writes e to the events table and pairs it with the received_at the
// database assigned, which is the tie-breaker the fold orders equal timestamps
// by.
func record(t *testing.T, db storetest.DB, e event.Event) event.Stored {
	t.Helper()

	payload, err := json.Marshal(e.Payload)
	if err != nil {
		t.Fatalf("marshaling the payload of event %s: %v", e.EventID, err)
	}

	var receivedAt pgtype.Timestamptz
	if err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
		var err error
		receivedAt, err = sqlcgen.New(tx).InsertEvent(t.Context(), sqlcgen.InsertEventParams{
			EventID:      e.EventID,
			Timestamp:    pgtype.Timestamptz{Time: e.Timestamp, Valid: true},
			EventType:    e.EventType,
			Platform:     e.Platform,
			Cloud:        e.Cloud,
			ResourceType: e.ResourceType,
			ResourceID:   e.ResourceID,
			ProjectID:    e.ProjectID,
			Source:       string(e.Source),
			Payload:      payload,
		})
		return err
	}); err != nil {
		t.Fatalf("inserting event %s: %v", e.EventID, err)
	}
	return event.Stored{Event: e, ReceivedAt: receivedAt.Time}
}

// ingest records e and folds it into the projection, which is the way one event
// travels through the pipeline. It records no metrics, which is what a test
// asserting on rows alone needs.
func ingest(t *testing.T, db storetest.DB, key projection.Key, e event.Event) {
	t.Helper()

	meteredIngest(t, db, key, e, nil)
}

// meteredIngest is ingest with m recording what the fold counts.
func meteredIngest(t *testing.T, db storetest.DB, key projection.Key, e event.Event, m *metrics.Metrics) {
	t.Helper()

	stored := record(t, db, e)
	if err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
		return projection.Apply(t.Context(), tx, key, []event.Stored{stored}, m)
	}); err != nil {
		t.Fatalf("Apply() error = %v, want nil", err)
	}
}

// replay folds the whole history of key in a transaction of its own.
func replay(t *testing.T, db storetest.DB, key projection.Key) {
	t.Helper()

	meteredReplay(t, db, key, nil)
}

// meteredReplay is replay with m recording what the fold counts.
func meteredReplay(t *testing.T, db storetest.DB, key projection.Key, m *metrics.Metrics) {
	t.Helper()

	if err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
		return projection.Replay(t.Context(), tx, key, m)
	}); err != nil {
		t.Fatalf("Replay() error = %v, want nil", err)
	}
}

// assertReplays fails the test unless the registry exports exactly want for
// tally_projection_replays_total. Comparing the whole map also states which
// clouds counted nothing.
func assertReplays(t *testing.T, reg *prometheus.Registry, want map[string]float64) {
	t.Helper()

	if got := replays(t, reg); !reflect.DeepEqual(got, want) {
		t.Errorf("tally_projection_replays_total = %v, want %v", got, want)
	}
}

// replays is every sample the registry exports for
// tally_projection_replays_total, keyed by the cloud it counts.
func replays(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering the registry: %v", err)
	}

	found := map[string]float64{}
	for _, family := range families {
		if family.GetName() != "tally_projection_replays_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "cloud" {
					found[label.GetValue()] = metric.GetCounter().GetValue()
				}
			}
		}
	}
	return found
}

// corrupt overwrites the state of every projection row, which is the drift a
// rebuild has to remove. It returns the rows as they stand afterwards, so a test
// can assert that a filtered run left the rows outside its filter alone.
func corrupt(t *testing.T, db storetest.DB) map[projection.Key]string {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`UPDATE current_resources SET state = 'corrupt'`); err != nil {
		t.Fatalf("corrupting the projection rows: %v", err)
	}
	return snapshots(t, db)
}

// snapshots reads every projection row as the JSON document Postgres renders it
// as, without the three key columns. What is left is everything the fold
// derived, so two rows folded from the same history compare as one string.
func snapshots(t *testing.T, db storetest.DB) map[projection.Key]string {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT cloud, resource_type, resource_id,
		        (to_jsonb(c) - 'cloud'::text - 'resource_type'::text - 'resource_id'::text)::text
		 FROM current_resources c`)
	if err != nil {
		t.Fatalf("reading the projection rows: %v", err)
	}
	defer rows.Close()

	found := map[projection.Key]string{}
	for rows.Next() {
		var (
			key      projection.Key
			snapshot string
		)
		if err := rows.Scan(&key.Cloud, &key.ResourceType, &key.ResourceID, &snapshot); err != nil {
			t.Fatalf("scanning a projection row: %v", err)
		}
		found[key] = snapshot
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the projection rows: %v", err)
	}
	return found
}

// assertSnapshots fails the test for every projection row that differs from the
// one want holds for it.
func assertSnapshots(t *testing.T, got, want map[projection.Key]string) {
	t.Helper()

	if len(got) != len(want) {
		t.Errorf("projection rows = %d, want %d", len(got), len(want))
	}
	for key, snapshot := range want {
		if got[key] != snapshot {
			t.Errorf("row of %s = %s, want %s", key.ResourceID, got[key], snapshot)
		}
	}
}

// readRow returns the projection row of key and whether there is one.
func readRow(t *testing.T, db storetest.DB, key projection.Key) (projectionRow, bool) {
	t.Helper()

	var row projectionRow
	err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT platform, project_id, state, size, created_at, deleted_at,
		        last_event_type, last_event_at, last_payload
		 FROM current_resources
		 WHERE cloud = $1 AND resource_type = $2 AND resource_id = $3`,
		key.Cloud, key.ResourceType, key.ResourceID,
	).Scan(&row.platform, &row.projectID, &row.state, &row.size, &row.createdAt, &row.deletedAt,
		&row.lastEventType, &row.lastEventAt, &row.lastPayload)
	if errors.Is(err, pgx.ErrNoRows) {
		return projectionRow{}, false
	}
	if err != nil {
		t.Fatalf("reading the projection row of %s: %v", key.ResourceID, err)
	}
	return row, true
}

// assertRow fails the test for every column of key's row that w does not
// describe.
func assertRow(t *testing.T, db storetest.DB, key projection.Key, w want) {
	t.Helper()

	got, ok := readRow(t, db, key)
	if !ok {
		t.Fatalf("no projection row for %s, want one", key.ResourceID)
	}
	if got.state != w.state {
		t.Errorf("state = %q, want %q", got.state, w.state)
	}
	if !reflect.DeepEqual(got.size, w.size) {
		t.Errorf("size = %v, want %v", got.size, w.size)
	}
	if got.projectID != w.projectID {
		t.Errorf("project id = %q, want %q", got.projectID, w.projectID)
	}
	// Every fixture reports the same platform, so a row carrying another one did
	// not take it from its events.
	if got.platform != platform {
		t.Errorf("platform = %q, want %q", got.platform, platform)
	}
	assertTime(t, "created at", got.createdAt, w.createdAt)
	assertTime(t, "deleted at", got.deletedAt, w.deletedAt)
	if got.lastEventType != w.lastEventType {
		t.Errorf("last event type = %q, want %q", got.lastEventType, w.lastEventType)
	}
	assertTime(t, "last event at", got.lastEventAt, &w.lastEventAt)
}

// assertTime fails the test unless got holds want, where a nil want is the NULL
// column.
func assertTime(t *testing.T, name string, got pgtype.Timestamptz, want *time.Time) {
	t.Helper()

	switch {
	case want == nil && got.Valid:
		t.Errorf("%s = %s, want NULL", name, got.Time)
	case want != nil && !got.Valid:
		t.Errorf("%s = NULL, want %s", name, *want)
	case want != nil && !got.Time.Equal(*want):
		t.Errorf("%s = %s, want %s", name, got.Time, *want)
	}
}

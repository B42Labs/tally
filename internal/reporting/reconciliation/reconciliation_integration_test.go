package reconciliation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/ids"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/metrics"
	"github.com/b42labs/tally/internal/reporting/projection"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// The fixtures every subtest builds its cloud from. The resource types are
// registered in no size schema, so a size a subtest invents is measured against
// nothing; the subtest about a refused correction registers a type of its own
// for exactly that.
const (
	platform    = "openstack"
	adapterName = "fake"
	projectA    = "project-a"
	projectB    = "project-b"
	typeShare   = "share"
	typeRouter  = "router"
	typeQuota   = "quota"
)

// The statuses a finished run leaves in sync_runs.
const (
	statusCompleted = "completed"
	statusFailed    = "failed"
)

// The instants the fixtures work with. They are whole seconds, so what
// PostgreSQL stores reads back as what the test passed in.
var (
	// createTime and deleteTime are the platform's own instants, which a
	// correction carries whenever the adapter exposes them.
	createTime = time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	deleteTime = time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)
	// pollTime is what the injected clock answers. It is what a correction the
	// platform gave no instant for is dated at.
	pollTime = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
)

// errManilaDown stands in for whatever a platform client returns when one of
// its services is unreachable.
var errManilaDown = errors.New("connection refused")

// TestSync drives the reconciliation framework against a real database. The
// subtests share one, so each of them works on a cloud of its own: a sync reads
// the projection, the run history, and the advisory lock all by cloud.
func TestSync(t *testing.T) {
	db := storetest.NewDB(t)
	pipeline := ingest.New(registry.New(), false, nil, nil)

	t.Run("creates what the projection does not hold yet", func(t *testing.T) {
		const cloud = "os-sync-create"
		dated := seen(typeShare, "share-dated", projectA, "available", map[string]any{"size_gb": 50})
		dated.CreatedAt = &createTime
		// The platform exposes no creation time for this one, so the correction
		// is dated at the poll instead: the concept's accepted approximation.
		fresh := seen(typeShare, "share-fresh", projectA, "available", nil)
		fake := &fakeAdapter{types: []string{typeShare}, steps: stream(dated, fresh)}
		syncer := newSyncer(t, db, pipeline, cloud, fake)

		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(2, 0, 0))
		assertRun(t, db, res, statusCompleted)
		want := []correction{
			{
				eventType: "sync.create", resourceType: typeShare, resourceID: "share-dated",
				projectID: projectA, at: rfc(createTime), state: "available",
				size: map[string]any{"size_gb": 50.0},
			},
			{
				eventType: "sync.create", resourceType: typeShare, resourceID: "share-fresh",
				projectID: projectA, at: rfc(pollTime), state: "available",
				// A create reports the size the resource starts with, and the
				// adapter reported none: the event carries the empty object rather
				// than being invalid.
				size: map[string]any{},
			},
		}
		assertCorrections(t, db, cloud, want)
		assertRows(t, db, cloud, []projectionRow{
			{
				resourceType: typeShare, resourceID: "share-dated", projectID: projectA,
				state: "available", size: map[string]any{"size_gb": 50.0}, createdAt: rfc(createTime),
			},
			{
				resourceType: typeShare, resourceID: "share-fresh", projectID: projectA,
				state: "available", size: map[string]any{}, createdAt: rfc(pollTime),
			},
		})
	})

	t.Run("creates a resource the projection holds as deleted again", func(t *testing.T) {
		const cloud = "os-sync-resurrection"
		alive := seen(typeShare, "share-back", projectA, "available", map[string]any{"size_gb": 10})
		fake := &fakeAdapter{types: []string{typeShare}, steps: stream(alive)}
		syncer := newSyncer(t, db, pipeline, cloud, fake)
		mustSync(t, syncer, cloud)

		// The platform reports the resource as gone, which puts the row into the
		// state a resurrection starts from.
		fake.steps = stream(vanished(typeShare, "share-back", deleteTime))
		mustSync(t, syncer, cloud)
		if row := rowOf(t, db, cloud, typeShare, "share-back"); row.state != "deleted" {
			t.Fatalf("state after the deletion = %q, want %q", row.state, "deleted")
		}

		fake.steps = stream(alive)
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(1, 0, 0))
		row := rowOf(t, db, cloud, typeShare, "share-back")
		if row.state != "available" || row.deletedAt != "" {
			t.Errorf("row after the resurrection = %+v, want it live again", row)
		}
		// Three runs, three corrections: the second create is a new life rather
		// than a repeat of the first.
		if got := len(corrections(t, db, cloud)); got != 3 {
			t.Errorf("stored corrections = %d, want the create, the delete and the create, 3", got)
		}
	})

	t.Run("dates a resurrection at the poll rather than at the platform's create", func(t *testing.T) {
		const cloud = "os-sync-resurrection-dated"
		alive := seen(typeShare, "share-back", projectA, "available", map[string]any{"size_gb": 10})
		// This platform exposes the resource's own creation time, and it predates
		// the deletion the projection comes to hold.
		alive.CreatedAt = &createTime
		fake := &fakeAdapter{types: []string{typeShare}, steps: stream(alive)}
		syncer := newSyncer(t, db, pipeline, cloud, fake)
		mustSync(t, syncer, cloud)

		fake.steps = stream(vanished(typeShare, "share-back", deleteTime))
		mustSync(t, syncer, cloud)

		fake.steps = stream(alive)
		res := mustSync(t, syncer, cloud)

		// A create ordered before the delete would revive nothing: the fold takes
		// the state from the last delete whatever precedes it, so the correction
		// would count, change nothing, and be written again by every later run.
		assertStats(t, res.Stats, tally(1, 0, 0))
		row := rowOf(t, db, cloud, typeShare, "share-back")
		if row.state != "available" || row.deletedAt != "" {
			t.Errorf("row after the resurrection = %+v, want it live again", row)
		}
	})

	t.Run("dates a resurrection past a delete the platform dated ahead of the poll", func(t *testing.T) {
		const cloud = "os-sync-resurrection-ahead"
		// A platform whose clock runs ahead of this service's dates its deletion
		// in Tally's future, and the projection takes the instant the platform
		// gave. A create dated at the poll is ordered before that delete, and the
		// fold takes the state from the last delete whatever precedes it: the row
		// would stay deleted and every later run would mint one more event id for
		// the same drift.
		ahead := pollTime.Add(20 * time.Minute)
		alive := seen(typeShare, "share-back", projectA, "available", map[string]any{"size_gb": 10})
		fake, syncer := seedFleet(t, db, pipeline, cloud, alive)

		fake.steps = stream(vanished(typeShare, "share-back", ahead))
		mustSync(t, syncer, cloud)

		fake.steps = stream(alive)
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(1, 0, 0))
		row := rowOf(t, db, cloud, typeShare, "share-back")
		if row.state != "available" || row.deletedAt != "" {
			t.Errorf("row after the resurrection = %+v, want it live again", row)
		}
		assertCorrections(t, db, cloud, []correction{
			{
				eventType: "sync.create", resourceType: typeShare, resourceID: "share-back",
				projectID: projectA, at: rfc(pollTime), state: "available",
				size: map[string]any{"size_gb": 10.0},
			},
			{
				eventType: "sync.delete", resourceType: typeShare, resourceID: "share-back",
				projectID: projectA, at: rfc(ahead),
			},
			{
				// One instant past the delete rather than at the poll, which is what
				// makes the correction the row's newest event.
				eventType: "sync.create", resourceType: typeShare, resourceID: "share-back",
				projectID: projectA, at: rfc(ahead.Add(time.Microsecond)), state: "available",
				size: map[string]any{"size_gb": 10.0},
			},
		})

		// The run that follows finds the projection corrected and emits nothing:
		// the resurrection converged rather than repeating for the life of the
		// resource.
		res = mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 0))
		if got := len(corrections(t, db, cloud)); got != 3 {
			t.Errorf("stored corrections = %d, want the create, the delete and the create, 3", got)
		}
	})

	t.Run("keeps a resurrection a rebuild folds again", func(t *testing.T) {
		const cloud = "os-sync-resurrection-rebuilt"
		alive := seen(typeShare, "share-back", projectA, "available", map[string]any{"size_gb": 10})
		alive.CreatedAt = &createTime
		fake, syncer := seedFleet(t, db, pipeline, cloud, alive)

		fake.steps = stream(vanished(typeShare, "share-back", deleteTime))
		mustSync(t, syncer, cloud)
		fake.steps = stream(alive)
		mustSync(t, syncer, cloud)

		// The row is derived state: a rebuild folds the same history through the
		// replay path, and what that path writes has to be what the incremental
		// one wrote. A replay that read the resurrection as a deletion would send
		// the next run around the same drift, under one more event id every time.
		if _, err := projection.Rebuild(t.Context(), db.Store,
			projection.Filter{Cloud: cloud}, nil); err != nil {
			t.Fatalf("Rebuild() error = %v, want nil", err)
		}
		row := rowOf(t, db, cloud, typeShare, "share-back")
		if row.state != "available" || row.deletedAt != "" {
			t.Errorf("row after the rebuild = %+v, want the resurrection kept", row)
		}

		fake.steps = stream(alive)
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 0))
		if got := len(corrections(t, db, cloud)); got != 3 {
			t.Errorf("stored corrections = %d, want the create, the delete and the create, 3", got)
		}
	})

	t.Run("updates a resource whose state drifted", func(t *testing.T) {
		const cloud = "os-sync-update-state"
		fake, syncer := seedFleet(t, db, pipeline, cloud,
			seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10}))

		fake.steps = stream(seen(typeShare, "share-1", projectA, "in-use", map[string]any{"size_gb": 10}))
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 1, 0))
		assertCorrection(t, db, cloud, "sync.update", correction{
			eventType: "sync.update", resourceType: typeShare, resourceID: "share-1",
			projectID: projectA, at: rfc(pollTime), state: "in-use",
			size: map[string]any{"size_gb": 10.0},
		})
		if row := rowOf(t, db, cloud, typeShare, "share-1"); row.state != "in-use" {
			t.Errorf("state = %q, want the observed %q", row.state, "in-use")
		}
	})

	t.Run("updates a resource whose size drifted", func(t *testing.T) {
		const cloud = "os-sync-update-size"
		fake, syncer := seedFleet(t, db, pipeline, cloud,
			seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10}))

		fake.steps = stream(seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 20}))
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 1, 0))
		assertCorrection(t, db, cloud, "sync.update", correction{
			eventType: "sync.update", resourceType: typeShare, resourceID: "share-1",
			projectID: projectA, at: rfc(pollTime), state: "available",
			size: map[string]any{"size_gb": 20.0},
		})
		if row := rowOf(t, db, cloud, typeShare, "share-1"); !reflect.DeepEqual(row.size, map[string]any{"size_gb": 20.0}) {
			t.Errorf("size = %v, want the observed one", row.size)
		}
	})

	t.Run("updates a resource that changed hands", func(t *testing.T) {
		const cloud = "os-sync-update-owner"
		fake, syncer := seedFleet(t, db, pipeline, cloud,
			seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10}))

		// A transfer is drift like any other, and the correction names the owner
		// the platform reports rather than the one the row still holds.
		fake.steps = stream(seen(typeShare, "share-1", projectB, "available", map[string]any{"size_gb": 10}))
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 1, 0))
		assertCorrection(t, db, cloud, "sync.update", correction{
			eventType: "sync.update", resourceType: typeShare, resourceID: "share-1",
			projectID: projectB, at: rfc(pollTime), state: "available",
			size: map[string]any{"size_gb": 10.0},
		})
		if row := rowOf(t, db, cloud, typeShare, "share-1"); row.projectID != projectB {
			t.Errorf("project id = %q, want the observed %q", row.projectID, projectB)
		}
	})

	t.Run("takes the last word of a resource the adapter listed twice", func(t *testing.T) {
		const cloud = "os-sync-duplicate-observation"
		fake, syncer := seedFleet(t, db, pipeline, cloud,
			seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10}))

		// A resource an adapter lists twice states its final word rather than two
		// facts, so the run corrects the projection to the last report.
		fake.steps = stream(
			seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10}),
			seen(typeShare, "share-1", projectA, "in-use", map[string]any{"size_gb": 10}))
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 1, 0))
		if row := rowOf(t, db, cloud, typeShare, "share-1"); row.state != "in-use" {
			t.Errorf("state = %q, want the last report %q", row.state, "in-use")
		}
	})

	t.Run("reads two spellings of one size as one size", func(t *testing.T) {
		const cloud = "os-sync-size-equality"
		fake, syncer := seedFleet(t, db, pipeline, cloud,
			seen(typeShare, "share-1", projectA, "available", map[string]any{"gb": 100}))

		// The adapter builds the size from an int and the database returns it as
		// a JSON number. Comparing the two documents rather than their bytes is
		// what keeps that from reading as drift.
		fake.steps = stream(seen(typeShare, "share-1", projectA, "available", map[string]any{"gb": 100}))
		res := mustSync(t, syncer, cloud)
		assertStats(t, res.Stats, tally(0, 0, 0))

		// An adapter that reports no size reports no evidence about the size, so
		// the stored one is not drift either.
		fake.steps = stream(seen(typeShare, "share-1", projectA, "available", nil))
		res = mustSync(t, syncer, cloud)
		assertStats(t, res.Stats, tally(0, 0, 0))

		if got := len(corrections(t, db, cloud)); got != 1 {
			t.Errorf("stored corrections = %d, want only the create the fleet was seeded with", got)
		}
	})

	t.Run("deletes what the platform reports as gone", func(t *testing.T) {
		const cloud = "os-sync-observed-delete"
		live := seen(typeShare, "share-live", projectA, "available", map[string]any{"size_gb": 10})
		doomed := seen(typeShare, "share-doomed", projectB, "available", map[string]any{"size_gb": 20})
		// This platform exposes the resource's own creation time, so the deletion
		// it reports falls past the row's create and is carried verbatim.
		doomed.CreatedAt = &createTime
		fake, syncer := seedFleet(t, db, pipeline, cloud, live, doomed)

		fake.steps = stream(live, vanished(typeShare, "share-doomed", deleteTime))
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 1))
		assertCorrection(t, db, cloud, "sync.delete", correction{
			eventType: "sync.delete", resourceType: typeShare, resourceID: "share-doomed",
			// The owner is the row's: who held the resource is settled history by
			// the time the platform reports it gone.
			projectID: projectB, at: rfc(deleteTime),
		})
		row := rowOf(t, db, cloud, typeShare, "share-doomed")
		if row.state != "deleted" || row.deletedAt != rfc(deleteTime) {
			t.Errorf("row = %+v, want it deleted at the platform's instant %s", row, rfc(deleteTime))
		}

		// A deletion of a row that already holds one, and one of a resource the
		// projection never held, both correct nothing.
		before := len(corrections(t, db, cloud))
		fake.steps = stream(live,
			vanished(typeShare, "share-doomed", deleteTime),
			vanished(typeShare, "share-unknown", deleteTime))
		res = mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 0))
		if got := len(corrections(t, db, cloud)); got != before {
			t.Errorf("stored corrections = %d, want them unchanged at %d", got, before)
		}
	})

	t.Run("dates a deletion the platform put before the row's newest event past it", func(t *testing.T) {
		const cloud = "os-sync-observed-delete-behind"
		// This platform exposes no creation time, so the row's create is dated at
		// the poll. The deletion it reports predates that: a history saying the
		// resource was gone before it existed, which the fold would order the
		// correction underneath, leaving the row live for every later run to
		// correct again under a new event id.
		doomed := seen(typeShare, "share-doomed", projectB, "available", map[string]any{"size_gb": 20})
		fake, syncer := seedFleet(t, db, pipeline, cloud, doomed)

		fake.steps = stream(vanished(typeShare, "share-doomed", deleteTime))
		res := mustSync(t, syncer, cloud)

		corrected := rfc(pollTime.Add(time.Microsecond))
		assertStats(t, res.Stats, tally(0, 0, 1))
		assertCorrection(t, db, cloud, "sync.delete", correction{
			eventType: "sync.delete", resourceType: typeShare, resourceID: "share-doomed",
			projectID: projectB, at: corrected,
		})
		if row := rowOf(t, db, cloud, typeShare, "share-doomed"); row.state != "deleted" ||
			row.deletedAt != corrected {
			t.Errorf("row = %+v, want it deleted at the corrected instant %s", row, corrected)
		}

		// The run that follows finds the projection corrected and emits nothing:
		// the deletion converged rather than repeating for the life of the row.
		fake.steps = stream(vanished(typeShare, "share-doomed", deleteTime))
		res = mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 0))
		if got := len(corrections(t, db, cloud)); got != 2 {
			t.Errorf("stored corrections = %d, want the create and the delete, 2", got)
		}
	})

	t.Run("deletes what an enumerated type no longer holds", func(t *testing.T) {
		const cloud = "os-sync-missed-delete"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		missed := seen(typeShare, "share-missed", projectB, "available", map[string]any{"size_gb": 20})
		router := seen(typeRouter, "router-1", projectA, "active", nil)
		fake, syncer := seedFleet(t, db, pipeline, cloud, share, missed, router)

		// Only the shares are enumerated this time, so the router's absence says
		// nothing about the router.
		fake.types, fake.steps = []string{typeShare}, stream(share)
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 1))
		assertCorrection(t, db, cloud, "sync.delete", correction{
			eventType: "sync.delete", resourceType: typeShare, resourceID: "share-missed",
			projectID: projectB, at: rfc(pollTime),
		})
		if row := rowOf(t, db, cloud, typeRouter, "router-1"); row.state != "active" {
			t.Errorf("router state = %q, want the unenumerated type left alone", row.state)
		}

		// A type enumerated to its end that holds nothing deletes every live row
		// it has: an empty answer is an answer.
		fake.types, fake.steps = []string{typeShare, typeRouter}, nil
		res = mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 2))
		for _, key := range [][2]string{{typeShare, "share-1"}, {typeRouter, "router-1"}} {
			if row := rowOf(t, db, cloud, key[0], key[1]); row.state != "deleted" {
				t.Errorf("%s %s state = %q, want %q", key[0], key[1], row.state, "deleted")
			}
		}
	})

	t.Run("emits nothing when the platform matches the projection", func(t *testing.T) {
		const cloud = "os-sync-clean"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		fake, syncer := seedFleet(t, db, pipeline, cloud, share)
		before := len(corrections(t, db, cloud))

		fake.steps = stream(share)
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 0))
		assertRun(t, db, res, statusCompleted)
		if got := len(corrections(t, db, cloud)); got != before {
			t.Errorf("stored corrections = %d, want them unchanged at %d", got, before)
		}
	})

	t.Run("emits nothing for a cloud that holds nothing", func(t *testing.T) {
		const cloud = "os-sync-empty"
		fake := &fakeAdapter{types: []string{typeShare}}
		syncer := newSyncer(t, db, pipeline, cloud, fake)

		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 0))
		assertRun(t, db, res, statusCompleted)
		if got := corrections(t, db, cloud); len(got) != 0 {
			t.Errorf("stored corrections = %+v, want none", got)
		}
	})

	t.Run("derives every event id from the run it belongs to", func(t *testing.T) {
		const cloud = "os-sync-event-ids"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		fake := &fakeAdapter{types: []string{typeShare}, steps: stream(share)}
		syncer := newSyncer(t, db, pipeline, cloud, fake)
		created := mustSync(t, syncer, cloud)

		fake.steps = stream(seen(typeShare, "share-1", projectA, "in-use", map[string]any{"size_gb": 10}))
		updated := mustSync(t, syncer, cloud)

		fake.steps = nil
		deleted := mustSync(t, syncer, cloud)

		want := []string{
			ids.SyntheticEventID(created.RunID, cloud, typeShare, "share-1", "create"),
			ids.SyntheticEventID(updated.RunID, cloud, typeShare, "share-1", "update"),
			ids.SyntheticEventID(deleted.RunID, cloud, typeShare, "share-1", "delete"),
		}
		slices.Sort(want)
		if got := storedEventIDs(t, db, cloud); !reflect.DeepEqual(got, want) {
			t.Errorf("stored event ids = %v, want the ones the runs derive, %v", got, want)
		}
	})

	t.Run("keeps the rows of a type it could not enumerate", func(t *testing.T) {
		const cloud = "os-sync-enumeration-error"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		router := seen(typeRouter, "router-1", projectA, "active", nil)
		fake, syncer := seedFleet(t, db, pipeline, cloud, share, router)

		// The shares stay unknown, the routers are enumerated to their end: the
		// fleet loses the router that is gone and gains the one that is new,
		// while the share nobody could list is left exactly as it was.
		fresh := seen(typeRouter, "router-2", projectB, "active", nil)
		fake.steps = []step{
			{err: &reconciliation.EnumerationError{ResourceType: typeShare, Err: errManilaDown}},
			{resource: fresh},
		}
		res, err := syncer.Sync(t.Context(), cloud)

		if err == nil {
			t.Fatal("Sync() error = nil, want the run reporting that it did not finish clean")
		}
		if res.Stats.Created != 1 || res.Stats.Deleted != 1 || res.Stats.Updated != 0 {
			t.Errorf("stats = %+v, want the create of router-2 and the delete of router-1", res.Stats)
		}
		if len(res.Stats.Errors) != 1 || !strings.Contains(res.Stats.Errors[0], typeShare) {
			t.Errorf("stats errors = %v, want the enumeration failure of %s", res.Stats.Errors, typeShare)
		}
		if !strings.Contains(res.Stats.Errors[0], errManilaDown.Error()) {
			t.Errorf("stats errors = %v, want the platform's own message", res.Stats.Errors)
		}
		assertRun(t, db, res, statusFailed)
		if row := rowOf(t, db, cloud, typeShare, "share-1"); row.state != "available" {
			t.Errorf("share state = %q, want the incomplete type left alone", row.state)
		}
		if row := rowOf(t, db, cloud, typeRouter, "router-1"); row.state != "deleted" {
			t.Errorf("router state = %q, want the enumerated type reconciled", row.state)
		}
	})

	t.Run("writes nothing when the stream fails outright", func(t *testing.T) {
		const cloud = "os-sync-stream-error"
		fake := &fakeAdapter{types: []string{typeShare}, steps: []step{
			{resource: seen(typeShare, "share-1", projectA, "available", nil)},
			{err: errManilaDown},
		}}
		syncer := newSyncer(t, db, pipeline, cloud, fake)

		res, err := syncer.Sync(t.Context(), cloud)

		if !errors.Is(err, errManilaDown) {
			t.Fatalf("Sync() error = %v, want it wrapping %v", err, errManilaDown)
		}
		assertRun(t, db, res, statusFailed)
		if len(res.Stats.Errors) != 1 || !strings.Contains(res.Stats.Errors[0], errManilaDown.Error()) {
			t.Errorf("stats errors = %v, want the error that ended the run", res.Stats.Errors)
		}
		// The resource the stream did report is not a correction: a run that
		// stopped mid-inventory knows nothing about the rest of the fleet.
		if got := corrections(t, db, cloud); len(got) != 0 {
			t.Errorf("stored corrections = %+v, want none", got)
		}
	})

	t.Run("keeps the fleet when the stream stops without saying why", func(t *testing.T) {
		const cloud = "os-sync-truncated-stream"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		unreached := seen(typeShare, "share-2", projectB, "available", map[string]any{"size_gb": 20})
		fake, syncer := seedFleet(t, db, pipeline, cloud, share, unreached)
		before := len(corrections(t, db, cloud))

		// The adapter returns on the cancelled context without yielding an error,
		// which is what a range-over-func selecting on ctx.Done() does. What it
		// listed is half the fleet, and reading that as the inventory would delete
		// the other half.
		fake.steps, fake.truncates = stream(share), true
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		res, err := syncer.Sync(ctx, cloud)

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Sync() error = %v, want it wrapping %v", err, context.DeadlineExceeded)
		}
		if len(res.Stats.Errors) != 1 || !strings.Contains(res.Stats.Errors[0], "stream ended early") {
			t.Errorf("stats errors = %v, want one naming the listing that stopped", res.Stats.Errors)
		}
		assertRun(t, db, res, statusFailed)
		if row := rowOf(t, db, cloud, typeShare, "share-2"); row.state != "available" {
			t.Errorf("state = %q, want the resource the run never reached left alone", row.state)
		}
		if got := len(corrections(t, db, cloud)); got != before {
			t.Errorf("stored corrections = %d, want them unchanged at %d", got, before)
		}
	})

	t.Run("writes nothing when the resource types cannot be read", func(t *testing.T) {
		const cloud = "os-sync-types-error"
		fake := &fakeAdapter{
			typesErr: errManilaDown,
			steps:    stream(seen(typeShare, "share-1", projectA, "available", nil)),
		}
		syncer := newSyncer(t, db, pipeline, cloud, fake)

		res, err := syncer.Sync(t.Context(), cloud)

		if !errors.Is(err, errManilaDown) {
			t.Fatalf("Sync() error = %v, want it wrapping %v", err, errManilaDown)
		}
		assertRun(t, db, res, statusFailed)
		if got := corrections(t, db, cloud); len(got) != 0 {
			t.Errorf("stored corrections = %+v, want none", got)
		}
	})

	t.Run("skips an observation the adapter reported incompletely", func(t *testing.T) {
		// Each case drops the one field it names, which leaves an item that
		// cannot be diffed. The type it belongs to is treated as incomplete for
		// it, so the row the run failed to see keeps its life.
		cases := []struct {
			name    string
			cloud   string
			broken  reconciliation.ObservedResource
			missing string
		}{
			{
				name: "without a resource id", cloud: "os-sync-invalid-id",
				broken:  seen(typeShare, "", projectA, "available", nil),
				missing: "resource id",
			},
			{
				name: "without a state", cloud: "os-sync-invalid-state",
				broken:  seen(typeShare, "share-broken", projectA, "", nil),
				missing: "state",
			},
			{
				name: "without a project id", cloud: "os-sync-invalid-project",
				broken:  seen(typeShare, "share-broken", "", "available", nil),
				missing: "project id",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
				fake, syncer := seedFleet(t, db, pipeline, tc.cloud, share)
				before := len(corrections(t, db, tc.cloud))

				fake.steps = stream(tc.broken)
				res, err := syncer.Sync(t.Context(), tc.cloud)

				if err == nil {
					t.Fatal("Sync() error = nil, want the run reporting the broken observation")
				}
				assertStats(t, res.Stats, reconciliation.Stats{
					Errors: res.Stats.Errors, ErrorCount: len(res.Stats.Errors),
				})
				if len(res.Stats.Errors) != 1 || !strings.Contains(res.Stats.Errors[0], tc.missing) {
					t.Errorf("stats errors = %v, want one naming the missing %s", res.Stats.Errors, tc.missing)
				}
				assertRun(t, db, res, statusFailed)
				if row := rowOf(t, db, tc.cloud, typeShare, "share-1"); row.state != "available" {
					t.Errorf("state = %q, want the row left alone", row.state)
				}
				if got := len(corrections(t, db, tc.cloud)); got != before {
					t.Errorf("stored corrections = %d, want them unchanged at %d", got, before)
				}
			})
		}
	})

	t.Run("deletes nothing for a run that met an observation without a type", func(t *testing.T) {
		const cloud = "os-sync-invalid-type"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		router := seen(typeRouter, "router-1", projectA, "active", nil)
		fake, syncer := seedFleet(t, db, pipeline, cloud, share, router)

		// An item that names no type is missing information about the inventory
		// as a whole rather than about one type of it: there is no type to hold
		// the missed-delete pass back for, so no type may conclude a deletion.
		fake.steps = stream(share, seen("", "orphan", projectA, "available", nil))
		res, err := syncer.Sync(t.Context(), cloud)

		if err == nil {
			t.Fatal("Sync() error = nil, want the run reporting the broken observation")
		}
		if res.Stats.Deleted != 0 {
			t.Errorf("stats = %+v, want no deletion from a run that lost track of a type", res.Stats)
		}
		assertRun(t, db, res, statusFailed)
		if row := rowOf(t, db, cloud, typeRouter, "router-1"); row.state != "active" {
			t.Errorf("router state = %q, want the unobserved type left alone", row.state)
		}
	})

	t.Run("records a correction the pipeline refused", func(t *testing.T) {
		const cloud = "os-sync-rejected"
		registerSizeSchema(t, db, typeQuota, `{
			"type": "object",
			"required": ["slots"],
			"properties": {"slots": {"type": "integer"}}
		}`)
		strict := ingest.New(registry.New(), true, nil, nil)
		// The second quota is what tells a dead-lettered correction from a batch
		// that was thrown away whole: its siblings have to land.
		fake := &fakeAdapter{
			types: []string{typeQuota},
			steps: stream(
				seen(typeQuota, "quota-1", projectA, "active", map[string]any{"slots": "many"}),
				seen(typeQuota, "quota-2", projectA, "active", map[string]any{"slots": 4})),
		}
		syncer := newSyncer(t, db, strict, cloud, fake)

		res, err := syncer.Sync(t.Context(), cloud)

		if err == nil {
			t.Fatal("Sync() error = nil, want the run reporting the refused correction")
		}
		// The tally counts what the committed batch carried, and the errors say
		// what became of it: the correction was dead-lettered rather than stored.
		if res.Stats.Created != 2 {
			t.Errorf("stats = %+v, want both emitted creates counted", res.Stats)
		}
		wantPrefix := "event " + ids.SyntheticEventID(res.RunID, cloud, typeQuota, "quota-1", "create") + " rejected: "
		if len(res.Stats.Errors) != 1 || !strings.HasPrefix(res.Stats.Errors[0], wantPrefix) {
			t.Errorf("stats errors = %v, want one prefixed %q", res.Stats.Errors, wantPrefix)
		}
		if !strings.Contains(res.Stats.Errors[0], "size_schema") {
			t.Errorf("stats errors = %v, want the reason the schema gave", res.Stats.Errors)
		}
		assertRun(t, db, res, statusFailed)
		got := rows(t, db, cloud)
		if len(got) != 1 || got[0].resourceID != "quota-2" {
			t.Errorf("projection rows = %+v, want only the accepted correction's", got)
		}
	})

	t.Run("records a platform error text a jsonb column cannot hold", func(t *testing.T) {
		const cloud = "os-sync-error-text"
		// An HTTP client hands the response body back verbatim, and a misbehaving
		// proxy puts whatever it likes in one. PostgreSQL holds no NUL in a jsonb
		// document, so an unscreened one would fail the write that records why the
		// run ended and leave the row at 'running' for a run that is over.
		fake := &fakeAdapter{types: []string{typeShare}, steps: []step{{
			err: &reconciliation.EnumerationError{
				ResourceType: typeShare,
				Err:          errors.New("bad gateway: <html>\x00</html>"),
			},
		}}}
		syncer := newSyncer(t, db, pipeline, cloud, fake)

		res, err := syncer.Sync(t.Context(), cloud)

		if err == nil {
			t.Fatal("Sync() error = nil, want the run reporting the enumeration failure")
		}
		assertRun(t, db, res, statusFailed)
		if len(res.Stats.Errors) != 1 || strings.ContainsRune(res.Stats.Errors[0], 0) {
			t.Fatalf("stats errors = %q, want one carrying no NUL", res.Stats.Errors)
		}
		if !strings.Contains(res.Stats.Errors[0], `\x00`) {
			t.Errorf("stats errors = %q, want what the platform sent to stay legible", res.Stats.Errors)
		}
	})

	t.Run("keeps the head of a platform error text nothing bounds", func(t *testing.T) {
		const cloud = "os-sync-error-size"
		// An HTTP client hands the response body back verbatim, and a load
		// balancer answering a paginated listing with an error page makes that
		// body megabytes long. The reason is held for the run, written into a
		// jsonb column, answered to the caller, and logged, so only its head is
		// kept.
		const (
			body = 5 << 20 // what the platform sent
			kept = 4 << 10 // the package's maxErrorBytes
		)
		fake := &fakeAdapter{types: []string{typeShare}, steps: []step{{
			err: &reconciliation.EnumerationError{
				ResourceType: typeShare,
				Err:          errors.New(strings.Repeat("x", body)),
			},
		}}}
		syncer := newSyncer(t, db, pipeline, cloud, fake)

		res, err := syncer.Sync(t.Context(), cloud)

		if err == nil {
			t.Fatal("Sync() error = nil, want the run reporting the enumeration failure")
		}
		if len(res.Stats.Errors) != 1 {
			t.Fatalf("stats errors = %d entries, want one", len(res.Stats.Errors))
		}
		reason := res.Stats.Errors[0]
		if want := kept + len("… (truncated)"); len(reason) != want {
			t.Errorf("recorded reason = %d bytes, want its head and the marker, %d", len(reason), want)
		}
		// What the platform sent stays legible as far as it is kept: the type that
		// failed is named at the front, and the end says the rest was dropped.
		if !strings.HasPrefix(reason, "enumerating "+typeShare) {
			t.Errorf("recorded reason = %q…, want it naming the type that failed", reason[:32])
		}
		if !strings.HasSuffix(reason, "(truncated)") {
			t.Errorf("recorded reason = …%q, want it marked as truncated", reason[len(reason)-32:])
		}
		assertRun(t, db, res, statusFailed)
	})

	t.Run("keeps the first of a flood of reasons and counts them all", func(t *testing.T) {
		const cloud = "os-sync-error-flood"
		// Nothing bounds how many reasons an adapter produces, and they are stored
		// in a jsonb column, answered to the caller, and joined into one string.
		const (
			failures = 250
			kept     = 100 // the package's maxRecordedErrors
		)
		steps := make([]step, failures)
		for i := range steps {
			steps[i] = step{err: &reconciliation.EnumerationError{
				ResourceType: typeShare, Err: fmt.Errorf("listing page %d", i),
			}}
		}
		fake := &fakeAdapter{types: []string{typeShare}, steps: steps}
		syncer := newSyncer(t, db, pipeline, cloud, fake)

		res, err := syncer.Sync(t.Context(), cloud)

		if err == nil {
			t.Fatal("Sync() error = nil, want the run reporting the enumeration failures")
		}
		if len(res.Stats.Errors) != kept {
			t.Errorf("stats errors = %d entries, want them capped at %d", len(res.Stats.Errors), kept)
		}
		if res.Stats.ErrorCount != failures {
			t.Errorf("error count = %d, want every reason counted, %d", res.Stats.ErrorCount, failures)
		}
		assertRun(t, db, res, statusFailed)
	})

	t.Run("commits a fleet that does not fit in one transaction", func(t *testing.T) {
		const cloud = "os-sync-batched"
		// The first sync of a cloud emits one correction per resource it holds,
		// and the ingest pipeline holds an advisory lock per resource until its
		// transaction commits. More resources than one batch carries is what
		// exercises the chunking that keeps the run off the shared lock table.
		const fleet = 250
		observed := make([]reconciliation.ObservedResource, fleet)
		for i := range observed {
			observed[i] = seen(typeShare, fmt.Sprintf("share-%03d", i), projectA,
				"available", map[string]any{"size_gb": 10})
		}
		fake := &fakeAdapter{types: []string{typeShare}, steps: stream(observed...)}
		syncer := newSyncer(t, db, pipeline, cloud, fake)

		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(fleet, 0, 0))
		if got := len(corrections(t, db, cloud)); got != fleet {
			t.Errorf("stored corrections = %d, want one per resource, %d", got, fleet)
		}
		if got := len(rows(t, db, cloud)); got != fleet {
			t.Errorf("projection rows = %d, want one per resource, %d", got, fleet)
		}
	})

	t.Run("refuses a cloud whose adapter is not registered", func(t *testing.T) {
		const cloud = "os-sync-no-adapter"
		// New takes the configuration and the adapters independently, so a cloud
		// naming an adapter nobody registered reaches the run rather than being
		// caught when the configuration was loaded.
		cfg := reconciliation.Config{Clouds: []reconciliation.CloudConfig{{
			Cloud: cloud, Platform: platform, Adapter: "absent",
		}}}
		syncer := reconciliation.New(db.Store, pipeline, cfg,
			map[string]reconciliation.Adapter{}, func() time.Time { return pollTime }, nil)

		res, err := syncer.Sync(t.Context(), cloud)

		if err == nil || !strings.Contains(err.Error(), "absent") {
			t.Errorf("Sync() error = %v, want it naming the adapter that is missing", err)
		}
		if res.RunID != "" {
			t.Errorf("Result = %+v, want the zero one for a cloud that cannot be synced", res)
		}
		if got := runsOf(t, db, cloud); got != 0 {
			t.Errorf("sync_runs rows = %d, want none for a run that never started", got)
		}
	})

	t.Run("takes its own corrections a second time as duplicates", func(t *testing.T) {
		const cloud = "os-sync-replay"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		fake, syncer := seedFleet(t, db, pipeline, cloud, share)
		items := storedItems(t, db, cloud)
		before := rows(t, db, cloud)

		var out ingest.Outcome
		if err := db.Store.WithTx(t.Context(), func(tx pgx.Tx) error {
			var err error
			out, err = pipeline.Ingest(t.Context(), tx, items, event.SourceReconciliation, nil)
			return err
		}); err != nil {
			t.Fatalf("Ingest() error = %v, want nil", err)
		}

		if out.Accepted != 0 || out.Duplicates != len(items) || len(out.Rejected) != 0 {
			t.Errorf("Ingest() = %+v, want the %d corrections as duplicates", out, len(items))
		}
		if after := rows(t, db, cloud); !reflect.DeepEqual(after, before) {
			t.Errorf("projection rows = %+v, want them unchanged at %+v", after, before)
		}

		// The other half of the same guarantee: a run over an unchanged platform
		// has nothing to correct.
		fake.steps = stream(share)
		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(0, 0, 0))
		if after := rows(t, db, cloud); !reflect.DeepEqual(after, before) {
			t.Errorf("projection rows = %+v, want them unchanged at %+v", after, before)
		}
	})

	t.Run("bounds the next run by the last completed one", func(t *testing.T) {
		const cloud = "os-sync-window"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		fake := &fakeAdapter{types: []string{typeShare}, steps: stream(share)}
		syncer := newSyncer(t, db, pipeline, cloud, fake)

		first := mustSync(t, syncer, cloud)
		if fake.since != nil {
			// A cloud that never completed a run has no window to start from, so
			// its first run is a full one.
			t.Errorf("since = %s, want none for the first run of a cloud", fake.since)
		}

		fake.steps = stream(share)
		second := mustSync(t, syncer, cloud)
		started := runRow(t, db, first.RunID).StartedAt.Time
		if fake.since == nil || !fake.since.Equal(started) {
			t.Fatalf("since = %v, want the started_at of the completed run, %s", fake.since, started)
		}

		// A failed run does not move the window: what it could not enumerate is
		// exactly what it may have missed, so the next run walks that window
		// again from where the last completed run started.
		fake.steps = []step{{err: errManilaDown}}
		if _, err := syncer.Sync(t.Context(), cloud); err == nil {
			t.Fatal("Sync() error = nil, want the failing run to fail")
		}
		fake.steps = stream(share)
		mustSync(t, syncer, cloud)

		completed := runRow(t, db, second.RunID).StartedAt.Time
		if fake.since == nil || !fake.since.Equal(completed) {
			t.Errorf("since = %v, want the last completed run's start, %s, rather than the failed run's",
				fake.since, completed)
		}
	})

	t.Run("refuses a second sync of a cloud a run is holding", func(t *testing.T) {
		const cloud = "os-sync-lock"
		held := blockingAdapter()
		holder := newSyncer(t, db, pipeline, cloud, held)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := holder.Sync(context.Background(), cloud); err != nil {
				t.Errorf("the holding Sync() error = %v, want nil", err)
			}
		}()
		<-held.entered

		refused := newSyncer(t, db, pipeline, cloud, &fakeAdapter{types: []string{typeShare}})
		res, err := refused.Sync(t.Context(), cloud)

		if !errors.Is(err, reconciliation.ErrAlreadyRunning) {
			t.Errorf("Sync() error = %v, want %v", err, reconciliation.ErrAlreadyRunning)
		}
		if res.RunID != "" {
			t.Errorf("Result = %+v, want the zero one for a refused sync", res)
		}
		// The refused call writes no row at all: the run it stepped aside for is
		// the only one there is.
		if got := runsOf(t, db, cloud); got != 1 {
			t.Errorf("sync_runs rows = %d, want only the holding run's, 1", got)
		}

		close(held.released)
		wg.Wait()
	})

	t.Run("frees the lock after a run that finished, failed, or crashed", func(t *testing.T) {
		const cloud = "os-sync-lock-release"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		fake := &fakeAdapter{types: []string{typeShare}, steps: stream(share)}
		syncer := newSyncer(t, db, pipeline, cloud, fake)
		mustSync(t, syncer, cloud)

		fake.steps = []step{{err: errManilaDown}}
		if _, err := syncer.Sync(t.Context(), cloud); err == nil {
			t.Fatal("Sync() error = nil, want the failing run to fail")
		}

		fake.steps = []step{{panics: true}}
		func() {
			defer func() {
				if recover() == nil {
					t.Error("Sync() returned, want the adapter's panic to reach the caller")
				}
			}()
			_, _ = syncer.Sync(t.Context(), cloud)
		}()

		// The lock is session-scoped, so a run that ended any of those three ways
		// has to have given it back before the next one asks.
		fake.steps = stream(share)
		if _, err := syncer.Sync(t.Context(), cloud); err != nil {
			t.Fatalf("Sync() error = %v, want the lock free again", err)
		}
	})

	t.Run("records the run and frees the lock when the caller's deadline passes", func(t *testing.T) {
		const cloud = "os-sync-deadline"
		blocked := blockingAdapter()
		syncer := newSyncer(t, db, pipeline, cloud, blocked)

		ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
		defer cancel()
		res, err := syncer.Sync(ctx, cloud)

		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Sync() error = %v, want it wrapping %v", err, context.DeadlineExceeded)
		}
		// The completion runs on a context of its own, so the row says the run is
		// over rather than staying at 'running' forever.
		assertRun(t, db, res, statusFailed)

		free := newSyncer(t, db, pipeline, cloud, &fakeAdapter{types: []string{typeShare}})
		if _, err := free.Sync(t.Context(), cloud); err != nil {
			t.Errorf("Sync() error = %v, want the lock free again", err)
		}
	})

	t.Run("refuses a cloud the configuration does not name", func(t *testing.T) {
		syncer := newSyncer(t, db, pipeline, "os-sync-configured", &fakeAdapter{})

		res, err := syncer.Sync(t.Context(), "os-sync-unconfigured")

		if !errors.Is(err, reconciliation.ErrUnknownCloud) {
			t.Errorf("Sync() error = %v, want %v", err, reconciliation.ErrUnknownCloud)
		}
		if res.RunID != "" {
			t.Errorf("Result = %+v, want the zero one for an unknown cloud", res)
		}
	})
}

// The series a finished run lands in.
const (
	runsSeries       = "tally_sync_runs_total"
	reconciledSeries = "tally_sync_resources_reconciled_total"
	errorsSeries     = "tally_sync_errors_total"
)

// syncSeries is all three of them, which the assertions over a sync that
// counted nothing read at once.
var syncSeries = []string{runsSeries, reconciledSeries, errorsSeries}

// TestSyncCounts holds the framework to what it reports about a run: one count
// per finished run under the status its row carries, the corrections it
// committed under their action, the reasons it recorded, and nothing at all for
// a sync that never wrote a run row.
func TestSyncCounts(t *testing.T) {
	db := storetest.NewDB(t)
	pipeline := ingest.New(registry.New(), false, nil, nil)

	t.Run("counts a finished run and what it reconciled", func(t *testing.T) {
		const cloud = "os-sync-count-clean"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		doomed := seen(typeShare, "share-doomed", projectB, "available", map[string]any{"size_gb": 20})
		// The fleet is seeded through a syncer that counts nothing, so the
		// registry below sees the one run that follows.
		seedFleet(t, db, pipeline, cloud, share, doomed)

		// One resource the projection does not hold, one whose state drifted, and
		// one the platform no longer lists: a run that moves all three actions.
		fresh := seen(typeShare, "share-2", projectA, "available", map[string]any{"size_gb": 30})
		drifted := seen(typeShare, "share-1", projectA, "in-use", map[string]any{"size_gb": 10})
		fake := &fakeAdapter{types: []string{typeShare}, steps: stream(fresh, drifted)}
		syncer, reg := meteredSyncer(t, db, pipeline, cloud, fake)

		res := mustSync(t, syncer, cloud)

		assertStats(t, res.Stats, tally(1, 1, 1))
		assertRun(t, db, res, statusCompleted)
		assertSamples(t, reg, runsSeries, map[string]float64{
			fmt.Sprintf("cloud=%q,status=%q", cloud, statusCompleted): 1,
		})
		assertSamples(t, reg, reconciledSeries, map[string]float64{
			reconciledLabels(cloud, "created"): float64(res.Stats.Created),
			reconciledLabels(cloud, "updated"): float64(res.Stats.Updated),
			reconciledLabels(cloud, "deleted"): float64(res.Stats.Deleted),
		})
		// A clean run reports the zero it recorded rather than leaving the series
		// out, so a rate over it reads no errors instead of no data.
		assertSamples(t, reg, errorsSeries, map[string]float64{
			fmt.Sprintf("cloud=%q", cloud): 0,
		})
	})

	t.Run("counts a run that did not finish clean as failed", func(t *testing.T) {
		const cloud = "os-sync-count-failed"
		share := seen(typeShare, "share-1", projectA, "available", map[string]any{"size_gb": 10})
		router := seen(typeRouter, "router-1", projectA, "active", nil)
		seedFleet(t, db, pipeline, cloud, share, router)

		// The shares stay unknown and the routers are enumerated to their end, so
		// the run reconciles what it saw and reports the type it could not list.
		fake := &fakeAdapter{types: []string{typeShare, typeRouter}, steps: []step{
			{err: &reconciliation.EnumerationError{ResourceType: typeShare, Err: errManilaDown}},
			{resource: seen(typeRouter, "router-2", projectB, "active", nil)},
		}}
		syncer, reg := meteredSyncer(t, db, pipeline, cloud, fake)

		res, err := syncer.Sync(t.Context(), cloud)

		if err == nil {
			t.Fatal("Sync() error = nil, want the run reporting the enumeration failure")
		}
		if res.Stats.Created != 1 || res.Stats.Updated != 0 || res.Stats.Deleted != 1 {
			t.Fatalf("stats = %+v, want the create of router-2 and the delete of router-1", res.Stats)
		}
		if res.Stats.ErrorCount == 0 {
			t.Fatalf("stats = %+v, want the enumeration failure recorded", res.Stats)
		}
		// The status the row ends at and the one the counter carries are derived
		// from the same tally, so a failed run is failed in both records.
		assertRun(t, db, res, statusFailed)
		assertSamples(t, reg, runsSeries, map[string]float64{
			fmt.Sprintf("cloud=%q,status=%q", cloud, statusFailed): 1,
		})
		// A run that spoiled part of its work keeps what it did reconcile, and the
		// action it never took is exported as the zero it is.
		assertSamples(t, reg, reconciledSeries, map[string]float64{
			reconciledLabels(cloud, "created"): float64(res.Stats.Created),
			reconciledLabels(cloud, "updated"): 0,
			reconciledLabels(cloud, "deleted"): float64(res.Stats.Deleted),
		})
		assertSamples(t, reg, errorsSeries, map[string]float64{
			fmt.Sprintf("cloud=%q", cloud): float64(res.Stats.ErrorCount),
		})
	})

	t.Run("counts nothing for a cloud the configuration does not name", func(t *testing.T) {
		syncer, reg := meteredSyncer(t, db, pipeline, "os-sync-count-configured", &fakeAdapter{})

		_, err := syncer.Sync(t.Context(), "os-sync-count-unconfigured")

		if !errors.Is(err, reconciliation.ErrUnknownCloud) {
			t.Fatalf("Sync() error = %v, want %v", err, reconciliation.ErrUnknownCloud)
		}
		assertNoSyncSamples(t, reg, "a cloud that cannot be synced")
	})

	t.Run("counts nothing for a sync a run is already holding", func(t *testing.T) {
		const cloud = "os-sync-count-lock"
		held := blockingAdapter()
		holder := newSyncer(t, db, pipeline, cloud, held)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := holder.Sync(context.Background(), cloud); err != nil {
				t.Errorf("the holding Sync() error = %v, want nil", err)
			}
		}()
		<-held.entered

		refused, reg := meteredSyncer(t, db, pipeline, cloud, &fakeAdapter{types: []string{typeShare}})
		_, err := refused.Sync(t.Context(), cloud)

		if !errors.Is(err, reconciliation.ErrAlreadyRunning) {
			t.Errorf("Sync() error = %v, want %v", err, reconciliation.ErrAlreadyRunning)
		}
		// The refused sync wrote no run row, and a run counts once, when its row
		// is written: the run it stepped aside for is the only one there is.
		assertNoSyncSamples(t, reg, "a sync that stepped aside")

		close(held.released)
		wg.Wait()
	})
}

// step is one entry of a scripted stream: a resource the platform holds, an
// error it answers with, or a crash in the middle of the listing.
type step struct {
	resource reconciliation.ObservedResource
	err      error
	panics   bool
}

// fakeAdapter is the platform half of these tests. What it yields, in which
// order, and what it says about the types it enumerates is scripted per
// subtest, which is what lets one adapter cover a clean inventory, a partial
// enumeration failure, and a run that never gets going.
type fakeAdapter struct {
	steps    []step
	types    []string
	typesErr error

	// truncates makes the stream stop after its steps once the context ends,
	// without yielding an error, which is what a range-over-func selecting on
	// ctx.Done() does. What it listed is then a partial inventory that says
	// nothing about the rest of the fleet.
	truncates bool

	// since is the window the last listing was given, which is what the
	// incremental-window assertions read.
	since *time.Time

	// at is the instant the last listing was told the run is at.
	at time.Time

	// entered is closed when a blocking stream starts and released is what holds
	// it there, so a test can compete with a run it knows is in flight. Both are
	// nil for a fake that does not block.
	entered  chan struct{}
	released chan struct{}
}

// blockingAdapter is a fake whose stream stops before its first step until the
// test releases it or the context ends.
func blockingAdapter() *fakeAdapter {
	return &fakeAdapter{
		types:    []string{typeShare},
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

func (a *fakeAdapter) Platform() string { return platform }

func (a *fakeAdapter) ResourceTypes(map[string]any) ([]string, error) {
	if a.typesErr != nil {
		return nil, a.typesErr
	}
	return a.types, nil
}

func (a *fakeAdapter) ListResources(ctx context.Context, _ map[string]any, since *time.Time,
	at time.Time,
) iter.Seq2[reconciliation.ObservedResource, error] {
	a.since = since
	a.at = at
	return func(yield func(reconciliation.ObservedResource, error) bool) {
		if a.released != nil {
			close(a.entered)
			select {
			case <-a.released:
			case <-ctx.Done():
				yield(reconciliation.ObservedResource{}, ctx.Err())
				return
			}
		}
		for _, s := range a.steps {
			if s.panics {
				panic("the adapter crashed while listing")
			}
			if !yield(s.resource, s.err) {
				return
			}
		}
		if a.truncates {
			<-ctx.Done()
		}
	}
}

// seen is a live resource as the platform reports it.
func seen(resourceType, id, project, state string, size map[string]any) reconciliation.ObservedResource {
	return reconciliation.ObservedResource{
		ResourceType: resourceType,
		ResourceID:   id,
		ProjectID:    project,
		State:        state,
		Size:         size,
	}
}

// vanished is a resource the platform reports as deleted at ts. It carries the
// key and the instant only, which is all an adapter knows about a resource that
// is gone.
func vanished(resourceType, id string, ts time.Time) reconciliation.ObservedResource {
	return reconciliation.ObservedResource{ResourceType: resourceType, ResourceID: id, DeletedAt: &ts}
}

// stream turns resources into the steps a listing that goes well yields.
func stream(resources ...reconciliation.ObservedResource) []step {
	steps := make([]step, len(resources))
	for i, resource := range resources {
		steps[i] = step{resource: resource}
	}
	return steps
}

// newSyncer builds a Syncer serving one cloud through fake, on a clock that
// always answers pollTime so that a correction dated at the poll is one a test
// can name.
func newSyncer(t *testing.T, db storetest.DB, pipeline *ingest.Pipeline, cloud string,
	fake *fakeAdapter,
) *reconciliation.Syncer {
	t.Helper()

	cfg := reconciliation.Config{Clouds: []reconciliation.CloudConfig{{
		Cloud: cloud, Platform: platform, Adapter: adapterName,
	}}}
	return reconciliation.New(db.Store, pipeline, cfg,
		map[string]reconciliation.Adapter{adapterName: fake},
		func() time.Time { return pollTime }, nil)
}

// meteredSyncer builds a Syncer like newSyncer does, recording into a registry
// of its own so that what one subtest counts stays out of every other.
func meteredSyncer(t *testing.T, db storetest.DB, pipeline *ingest.Pipeline, cloud string,
	fake *fakeAdapter,
) (*reconciliation.Syncer, *prometheus.Registry) {
	t.Helper()

	cfg := reconciliation.Config{Clouds: []reconciliation.CloudConfig{{
		Cloud: cloud, Platform: platform, Adapter: adapterName,
	}}}
	reg := prometheus.NewRegistry()
	return reconciliation.New(db.Store, pipeline, cfg,
		map[string]reconciliation.Adapter{adapterName: fake},
		func() time.Time { return pollTime }, metrics.New(reg)), reg
}

// seedFleet puts resources into the projection by running one sync over them,
// which is the shortest route to a projection the next run can drift from. It
// hands back the fake and the syncer so that the subtest scripts what the
// platform holds next.
func seedFleet(t *testing.T, db storetest.DB, pipeline *ingest.Pipeline, cloud string,
	resources ...reconciliation.ObservedResource,
) (*fakeAdapter, *reconciliation.Syncer) {
	t.Helper()

	types := map[string]bool{}
	for _, resource := range resources {
		types[resource.ResourceType] = true
	}
	fake := &fakeAdapter{types: slices.Sorted(maps.Keys(types)), steps: stream(resources...)}
	syncer := newSyncer(t, db, pipeline, cloud, fake)
	if res := mustSync(t, syncer, cloud); res.Stats.Created != len(resources) {
		t.Fatalf("seeding %s created %d resources, want %d", cloud, res.Stats.Created, len(resources))
	}
	return fake, syncer
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

	row := runRow(t, db, res.RunID)
	if row.Status != status {
		t.Errorf("run status = %q, want %q", row.Status, status)
	}
	if !row.CompletedAt.Valid {
		t.Error("completed at = NULL, want the instant the run ended")
	}

	var stored reconciliation.Stats
	if err := json.Unmarshal(row.Stats, &stored); err != nil {
		t.Fatalf("decoding the stored stats: %v", err)
	}
	if !reflect.DeepEqual(stored, res.Stats) {
		t.Errorf("stored stats = %+v, want the ones the caller got, %+v", stored, res.Stats)
	}

	// The stored document says "errors": [] rather than null, so that every run
	// reads the same way whatever it did.
	var raw map[string]any
	if err := json.Unmarshal(row.Stats, &raw); err != nil {
		t.Fatalf("decoding the stored stats: %v", err)
	}
	if errs, ok := raw["errors"].([]any); !ok || len(errs) != len(res.Stats.Errors) {
		t.Errorf("stored errors = %v, want the %d the run recorded, as a list",
			raw["errors"], len(res.Stats.Errors))
	}
}

// reconciledLabels is the label set tally_sync_resources_reconciled_total
// carries for one action of cloud, as the exposition format spells it.
func reconciledLabels(cloud, action string) string {
	return fmt.Sprintf("action=%q,cloud=%q", action, cloud)
}

// assertSamples fails the test unless the registry exports exactly want for the
// named metric. Comparing the whole map also states which series the run left
// alone.
func assertSamples(t *testing.T, reg *prometheus.Registry, name string, want map[string]float64) {
	t.Helper()

	if got := samples(t, reg, name); !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// samples is every sample the registry exports for the named metric, keyed by
// the label set it carries, as the exposition format spells it.
func samples(t *testing.T, reg *prometheus.Registry, name string) map[string]float64 {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering the registry: %v", err)
	}

	found := map[string]float64{}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := make([]string, 0, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				labels = append(labels, fmt.Sprintf("%s=%q", label.GetName(), label.GetValue()))
			}
			found[strings.Join(labels, ",")] = metric.GetCounter().GetValue()
		}
	}
	return found
}

// assertNoSyncSamples fails the test unless the registry exports no sync series
// at all. subject names what left them empty.
func assertNoSyncSamples(t *testing.T, reg *prometheus.Registry, subject string) {
	t.Helper()

	count, err := testutil.GatherAndCount(reg, syncSeries...)
	if err != nil {
		t.Fatalf("gathering the registry: %v", err)
	}
	if count != 0 {
		t.Errorf("the registry exports %d sync series, want none for %s", count, subject)
	}
}

// runRow reads the sync_runs row of a run back.
func runRow(t *testing.T, db storetest.DB, runID string) sqlcgen.SyncRun {
	t.Helper()

	id, err := uuid.Parse(runID)
	if err != nil {
		t.Fatalf("parsing the run id %q: %v", runID, err)
	}
	row, err := sqlcgen.New(db.Store.Pool()).GetSyncRun(t.Context(), id)
	if err != nil {
		t.Fatalf("reading sync run %s: %v", runID, err)
	}
	return row
}

// runsOf is how many sync_runs rows cloud has, which is what a refused sync is
// measured against.
func runsOf(t *testing.T, db storetest.DB, cloud string) int {
	t.Helper()

	var found int
	if err := db.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM sync_runs WHERE cloud = $1`, cloud).Scan(&found); err != nil {
		t.Fatalf("counting the sync runs of %s: %v", cloud, err)
	}
	return found
}

// correction is a stored synthetic event as the assertions compare it. The
// instant is kept as text so that a whole batch compares in one DeepEqual.
type correction struct {
	eventType    string
	resourceType string
	resourceID   string
	projectID    string
	at           string
	state        string
	size         map[string]any
}

// corrections is every synthetic event stored for cloud, ordered by the
// resource it belongs to and the instant it carries.
func corrections(t *testing.T, db storetest.DB, cloud string) []correction {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT event_type, resource_type, resource_id, project_id, timestamp, payload
		 FROM events
		 WHERE cloud = $1 AND source = 'reconciliation'
		 ORDER BY resource_type, resource_id, timestamp, event_type`, cloud)
	if err != nil {
		t.Fatalf("reading the corrections of %s: %v", cloud, err)
	}
	defer rows.Close()

	var found []correction
	for rows.Next() {
		var (
			got     correction
			ts      time.Time
			payload []byte
		)
		if err := rows.Scan(&got.eventType, &got.resourceType, &got.resourceID,
			&got.projectID, &ts, &payload); err != nil {
			t.Fatalf("scanning a correction: %v", err)
		}

		var envelope event.PayloadEnvelope
		if err := json.Unmarshal(payload, &envelope); err != nil {
			t.Fatalf("decoding the payload of a correction: %v", err)
		}
		if envelope.State != nil {
			got.state = *envelope.State
		}
		got.at, got.size = rfc(ts), envelope.Size
		found = append(found, got)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the corrections of %s: %v", cloud, err)
	}
	return found
}

// assertCorrections fails the test unless cloud holds exactly want.
func assertCorrections(t *testing.T, db storetest.DB, cloud string, want []correction) {
	t.Helper()

	if got := corrections(t, db, cloud); !reflect.DeepEqual(got, want) {
		t.Errorf("stored corrections = %+v, want %+v", got, want)
	}
}

// assertCorrection fails the test unless exactly one correction of eventType is
// stored for cloud, and it is want. It is what a subtest that seeded its fleet
// through an earlier run asserts with: the creates of the seeding run are
// stored as well, and only the event type under test is the subject.
func assertCorrection(t *testing.T, db storetest.DB, cloud, eventType string, want correction) {
	t.Helper()

	var found []correction
	for _, got := range corrections(t, db, cloud) {
		if got.eventType == eventType {
			found = append(found, got)
		}
	}
	if len(found) != 1 {
		t.Fatalf("stored %s corrections = %+v, want exactly one", eventType, found)
	}
	if !reflect.DeepEqual(found[0], want) {
		t.Errorf("stored correction = %+v, want %+v", found[0], want)
	}
}

// storedEventIDs is the id of every synthetic event stored for cloud, sorted.
func storedEventIDs(t *testing.T, db storetest.DB, cloud string) []string {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT event_id FROM events WHERE cloud = $1 AND source = 'reconciliation'
		 ORDER BY event_id`, cloud)
	if err != nil {
		t.Fatalf("reading the event ids of %s: %v", cloud, err)
	}
	defer rows.Close()

	var found []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning an event id: %v", err)
		}
		found = append(found, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the event ids of %s: %v", cloud, err)
	}
	return found
}

// storedItems is every synthetic event of cloud rendered as the batch items a
// caller re-submitting the run would send.
func storedItems(t *testing.T, db storetest.DB, cloud string) []json.RawMessage {
	t.Helper()

	rows, err := db.Store.Pool().Query(t.Context(),
		`SELECT event_id, timestamp, event_type, platform, cloud, resource_type, resource_id,
		        project_id, payload
		 FROM events
		 WHERE cloud = $1 AND source = 'reconciliation'
		 ORDER BY event_id`, cloud)
	if err != nil {
		t.Fatalf("reading the corrections of %s: %v", cloud, err)
	}
	defer rows.Close()

	var found []json.RawMessage
	for rows.Next() {
		var (
			e       event.Event
			payload []byte
		)
		if err := rows.Scan(&e.EventID, &e.Timestamp, &e.EventType, &e.Platform, &e.Cloud,
			&e.ResourceType, &e.ResourceID, &e.ProjectID, &payload); err != nil {
			t.Fatalf("scanning a correction: %v", err)
		}
		if err := json.Unmarshal(payload, &e.Payload); err != nil {
			t.Fatalf("decoding the payload of correction %s: %v", e.EventID, err)
		}
		e.Source = event.SourceReconciliation

		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshaling correction %s: %v", e.EventID, err)
		}
		found = append(found, raw)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the corrections of %s: %v", cloud, err)
	}
	return found
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

// rows is the projection of cloud, ordered by resource.
func rows(t *testing.T, db storetest.DB, cloud string) []projectionRow {
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

	var snapshot []projectionRow
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
		snapshot = append(snapshot, row)
	}
	if err := found.Err(); err != nil {
		t.Fatalf("reading the projection rows of %s: %v", cloud, err)
	}
	return snapshot
}

// assertRows fails the test unless the projection of cloud is exactly want.
func assertRows(t *testing.T, db storetest.DB, cloud string, want []projectionRow) {
	t.Helper()

	if got := rows(t, db, cloud); !reflect.DeepEqual(got, want) {
		t.Errorf("projection rows = %+v, want %+v", got, want)
	}
}

// rowOf is the projection row of one resource. A subtest asking for a row it
// expects to exist gets a failure rather than a zero value when it does not.
func rowOf(t *testing.T, db storetest.DB, cloud, resourceType, resourceID string) projectionRow {
	t.Helper()

	for _, row := range rows(t, db, cloud) {
		if row.resourceType == resourceType && row.resourceID == resourceID {
			return row
		}
	}
	t.Fatalf("no projection row for %s %s in %s, want one", resourceType, resourceID, cloud)
	return projectionRow{}
}

// registerSizeSchema registers raw as the size schema of (platform,
// resourceType), which is what makes the strict pipeline measure a reported
// size against it.
func registerSizeSchema(t *testing.T, db storetest.DB, resourceType, raw string) {
	t.Helper()

	if _, err := sqlcgen.New(db.Store.Pool()).UpsertResourceType(t.Context(),
		sqlcgen.UpsertResourceTypeParams{
			Platform: platform, ResourceType: resourceType, SizeSchema: []byte(raw),
		}); err != nil {
		t.Fatalf("registering the size schema of %s: %v", resourceType, err)
	}
}

// rfc renders an instant the way the assertions compare instants.
func rfc(ts time.Time) string {
	return ts.UTC().Format(time.RFC3339Nano)
}

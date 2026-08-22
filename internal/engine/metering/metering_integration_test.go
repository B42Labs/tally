package metering_test

import (
	"context"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// TestMeterAgainstTheReportingDatabase runs the concept's first worked example
// through the whole read path: the projection row makes the resource a
// candidate, its two events are loaded and decoded by source.Snapshot, and the
// drafts that come out are the ones the unit tests derive from the same history
// in memory. It is what proves the seam between the two packages, which the
// in-memory fake cannot.
func TestMeterAgainstTheReportingDatabase(t *testing.T) {
	db := storetest.NewDB(t)

	metered := source.Resource{
		Cloud: "os-prod-eu1", Platform: "openstack", ResourceType: "instance", ResourceID: "def-456",
	}
	created := utc(time.February, 10)
	resized := utc(time.March, 16)
	small := `{"vcpus":2,"ram_gb":4,"disk_gb":40,"flavor":"m1.small"}`
	large := `{"vcpus":4,"ram_gb":8,"disk_gb":80,"flavor":"m1.large"}`

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO current_resources (cloud, platform, resource_type, resource_id, project_id,
		                                state, created_at, last_event_type, last_event_at)
		 VALUES ($1, $2, $3, $4, 'proj-456', 'active', $5, 'compute.instance.resize.end', $6)`,
		metered.Cloud, metered.Platform, metered.ResourceType, metered.ResourceID,
		created, resized); err != nil {
		t.Fatalf("seeding the projection row: %v", err)
	}
	for _, seed := range []struct {
		eventID, eventType string
		timestamp          time.Time
		payload            string
	}{
		{"ev-create", "compute.instance.create.end", created, `{"state":"active","size":` + small + `}`},
		{"ev-resize", "compute.instance.resize.end", resized, `{"state":"active","size":` + large + `}`},
	} {
		if _, err := db.Store.Pool().Exec(t.Context(),
			`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
			                     resource_type, resource_id, project_id, payload)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, 'proj-456', $8)`,
			seed.eventID, seed.timestamp, seed.eventType, metered.Platform, metered.Cloud,
			metered.ResourceType, metered.ResourceID, seed.payload); err != nil {
			t.Fatalf("seeding the event %s: %v", seed.eventID, err)
		}
	}

	src, err := source.New(t.Context(), db.URL)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	t.Cleanup(src.Close)
	snap, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		// The test's own context is canceled before this runs.
		if err := snap.Close(context.Background()); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})

	t.Run("meters the seeded resource", func(t *testing.T) {
		result, err := metering.Meter(t.Context(), snap, periodFrom, periodTo, []string{metered.Cloud})
		if err != nil {
			t.Fatalf("Meter() error = %v, want nil", err)
		}

		if result.Candidates != 1 {
			t.Errorf("Candidates = %d, want 1", result.Candidates)
		}
		if len(result.Warnings) != 0 {
			t.Errorf("Warnings = %+v, want none", result.Warnings)
		}
		if len(result.Resources) != 1 {
			t.Fatalf("got %d resources, want 1: %+v", len(result.Resources), result.Resources)
		}
		if result.Resources[0].Resource != metered {
			t.Errorf("Resource = %+v, want %+v", result.Resources[0].Resource, metered)
		}

		drafts := result.Resources[0].Drafts
		wantDrafts(t, drafts, []want{
			{
				state: "active", project: "proj-456",
				from: periodFrom, to: resized,
				minutes: "21600", size: size(t, small),
			},
			{
				state: "active", project: "proj-456",
				from: resized, to: periodTo,
				minutes: "23040", size: size(t, large),
			},
		})
		for i, expected := range []int64{1296000, 1382400} {
			if drafts[i].Seconds != expected {
				t.Errorf("draft %d Seconds = %d, want %d", i, drafts[i].Seconds, expected)
			}
		}
	})

	t.Run("meters no resource of another cloud", func(t *testing.T) {
		result, err := metering.Meter(t.Context(), snap, periodFrom, periodTo, []string{"os-prod-us1"})
		if err != nil {
			t.Fatalf("Meter() error = %v, want nil", err)
		}

		if result.Candidates != 0 {
			t.Errorf("Candidates = %d, want 0, the cloud filter reaching the query", result.Candidates)
		}
		if len(result.Resources) != 0 {
			t.Errorf("Resources = %+v, want none", result.Resources)
		}
	})
}

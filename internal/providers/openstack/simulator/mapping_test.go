package simulator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/b42labs/tally/internal/core/testkit"
	"github.com/b42labs/tally/internal/providers/openstack"
)

func TestEveryBillableTransitionMapsToAnEvent(t *testing.T) {
	for _, transition := range generateMonth(t, 1, july2026, testCloud) {
		notification := parse(t, render(t, transition))

		mapped, ok := openstack.MapNotification(notification, testCloud)
		if ok != transition.Billable {
			t.Errorf("MapNotification(%s) mapped = %v, want %v",
				transition.EventType, ok, transition.Billable)
			continue
		}
		if !ok {
			continue
		}

		raw, err := json.Marshal(mapped)
		if err != nil {
			t.Fatalf("encoding the event of %s: %v", transition.EventType, err)
		}
		testkit.AssertValidEvent(t, raw)

		if mapped.ResourceID != transition.ResourceID {
			t.Errorf("%s booked resource %q, want %q",
				transition.EventType, mapped.ResourceID, transition.ResourceID)
		}
		if mapped.ProjectID != transition.ProjectID {
			t.Errorf("%s booked project %q, want %q",
				transition.EventType, mapped.ProjectID, transition.ProjectID)
		}
		if !mapped.Timestamp.Equal(transition.At) {
			t.Errorf("%s booked %s, want %s", transition.EventType, mapped.Timestamp, transition.At)
		}
		if mapped.Cloud != testCloud {
			t.Errorf("%s booked cloud %q, want %q", transition.EventType, mapped.Cloud, testCloud)
		}
	}
}

// TestEveryRecordedNotificationTypeIsRendered is the drift guard between the
// collector's fixtures and the simulator. A type that was recorded from a real
// deployment but never rendered is one a run leaves untested, whatever the seed
// an operator picks, so the forced steps of the workload are held against the
// whole fixture set rather than against one month.
func TestEveryRecordedNotificationTypeIsRendered(t *testing.T) {
	entries, err := os.ReadDir(sampleDir)
	if err != nil {
		t.Fatalf("reading %s: %v", sampleDir, err)
	}
	if len(entries) == 0 {
		t.Fatalf("%s holds no recorded notifications, want the collector's fixtures", sampleDir)
	}

	var recorded []string
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(sampleDir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		if eventType := parse(t, body).EventType; !slices.Contains(recorded, eventType) {
			recorded = append(recorded, eventType)
		}
	}
	slices.Sort(recorded)
	if len(recorded) == 0 {
		t.Fatalf("%s holds no readable notification, want the collector's fixtures", sampleDir)
	}

	for seed := uint64(1); seed <= 5; seed++ {
		var rendered []string
		for _, transition := range generateMonth(t, seed, july2026, testCloud) {
			if !slices.Contains(rendered, transition.EventType) {
				rendered = append(rendered, transition.EventType)
			}
		}
		for _, eventType := range recorded {
			if !slices.Contains(rendered, eventType) {
				t.Errorf("seed %d renders no %s, want every recorded type in every month", seed, eventType)
			}
		}
	}
}

package timeline_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/timeline"
)

var (
	t0 = time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	t1 = time.Date(2026, time.March, 2, 0, 0, 0, 0, time.UTC)
	t2 = time.Date(2026, time.March, 3, 0, 0, 0, 0, time.UTC)
)

type option func(*event.Stored)

func withState(state string) option {
	return func(s *event.Stored) { s.Payload.State = &state }
}

func withSize(size map[string]any) option {
	return func(s *event.Stored) { s.Payload.Size = size }
}

func withProject(id string) option {
	return func(s *event.Stored) { s.ProjectID = id }
}

func withReceivedAt(ts time.Time) option {
	return func(s *event.Stored) { s.ReceivedAt = ts }
}

// ev builds a stored event. Defaults keep each test to the one dimension it
// exercises: same project, receipt at the event time.
func ev(id, eventType string, ts time.Time, opts ...option) event.Stored {
	s := event.Stored{
		Event: event.Event{
			EventID:      id,
			Timestamp:    ts,
			EventType:    eventType,
			Platform:     "openstack",
			Cloud:        "os-prod-eu1",
			ResourceType: "instance",
			ResourceID:   "i-1",
			ProjectID:    "p-1",
			Source:       event.SourceCollector,
		},
		ReceivedAt: ts,
	}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

func TestBuildEmptyHistory(t *testing.T) {
	tests := []struct {
		name   string
		events []event.Stored
	}{
		{name: "nil", events: nil},
		{name: "empty slice", events: []event.Stored{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := timeline.Build(tc.events)

			if len(got.Intervals) != 0 {
				t.Errorf("Intervals = %v, want none", got.Intervals)
			}
			if got.CreatedAt != nil {
				t.Errorf("CreatedAt = %v, want nil", got.CreatedAt)
			}
			if got.DeletedAt != nil {
				t.Errorf("DeletedAt = %v, want nil", got.DeletedAt)
			}
			if len(got.Warnings) != 0 {
				t.Errorf("Warnings = %v, want none", got.Warnings)
			}
		})
	}
}

func TestBuildSingleCreateStaysOpen(t *testing.T) {
	got := timeline.Build([]event.Stored{
		ev("e1", "compute.instance.create.end", t0, withState("active"), withSize(map[string]any{"vcpus": 4})),
	})

	if len(got.Intervals) != 1 {
		t.Fatalf("got %d intervals, want 1: %+v", len(got.Intervals), got.Intervals)
	}
	if got.Intervals[0].End != nil {
		t.Errorf("End = %v, want nil for a live resource", got.Intervals[0].End)
	}
	if !got.Intervals[0].Start.Equal(t0) {
		t.Errorf("Start = %v, want %v", got.Intervals[0].Start, t0)
	}
	if got.CreatedAt == nil || !got.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, t0)
	}
}

func TestBuildCreateThenDelete(t *testing.T) {
	got := timeline.Build([]event.Stored{
		ev("e1", "compute.instance.create.end", t0, withState("active"), withSize(map[string]any{"vcpus": 4})),
		ev("e2", "compute.instance.delete.end", t1),
	})

	if len(got.Intervals) != 1 {
		t.Fatalf("got %d intervals, want 1: %+v", len(got.Intervals), got.Intervals)
	}
	if got.Intervals[0].End == nil || !got.Intervals[0].End.Equal(t1) {
		t.Errorf("End = %v, want %v", got.Intervals[0].End, t1)
	}
	if got.DeletedAt == nil || !got.DeletedAt.Equal(t1) {
		t.Errorf("DeletedAt = %v, want %v", got.DeletedAt, t1)
	}
	// A deleted resource accrues nothing, so the delete opens no interval.
	for _, interval := range got.Intervals {
		if interval.State == "deleted" {
			t.Errorf("found an interval in state deleted: %+v", interval)
		}
	}
}

func TestBuildCreateResizeDelete(t *testing.T) {
	small := map[string]any{"vcpus": 2}
	large := map[string]any{"vcpus": 4}

	got := timeline.Build([]event.Stored{
		ev("e1", "compute.instance.create.end", t0, withState("active"), withSize(small)),
		ev("e2", "compute.instance.resize.end", t1, withState("active"), withSize(large)),
		ev("e3", "compute.instance.delete.end", t2),
	})

	if len(got.Intervals) != 2 {
		t.Fatalf("got %d intervals, want 2: %+v", len(got.Intervals), got.Intervals)
	}
	if got.Intervals[0].End == nil || !got.Intervals[0].End.Equal(t1) {
		t.Errorf("first interval End = %v, want the resize timestamp %v", got.Intervals[0].End, t1)
	}
	if !got.Intervals[1].Start.Equal(t1) {
		t.Errorf("second interval Start = %v, want the resize timestamp %v", got.Intervals[1].Start, t1)
	}
	if !reflect.DeepEqual(got.Intervals[0].Size, small) {
		t.Errorf("first interval Size = %v, want %v", got.Intervals[0].Size, small)
	}
	if !reflect.DeepEqual(got.Intervals[1].Size, large) {
		t.Errorf("second interval Size = %v, want %v", got.Intervals[1].Size, large)
	}
}

func TestBuildResizeToAnEqualSizeIsNotBillable(t *testing.T) {
	// Deep equality, not identity: a second map with the same contents is the
	// same size and must not split the interval.
	got := timeline.Build([]event.Stored{
		ev("e1", "compute.instance.create.end", t0, withState("active"), withSize(map[string]any{"vcpus": 2})),
		ev("e2", "compute.instance.resize.end", t1, withState("active"), withSize(map[string]any{"vcpus": 2})),
	})

	if len(got.Intervals) != 1 {
		t.Fatalf("got %d intervals, want 1: %+v", len(got.Intervals), got.Intervals)
	}
}

func TestBuildOrdersEqualTimestampsByReceivedAt(t *testing.T) {
	early := t1.Add(-time.Hour)
	late := t1.Add(time.Hour)

	// Both changes carry the same event timestamp; the receipt order decides
	// which state the resource ends in.
	got := timeline.Build([]event.Stored{
		ev("e3", "compute.instance.power_on.end", t1, withState("second"), withReceivedAt(late)),
		ev("e2", "compute.instance.power_off.end", t1, withState("first"), withReceivedAt(early)),
		ev("e1", "compute.instance.create.end", t0, withState("active"), withSize(map[string]any{"vcpus": 2})),
	})

	last := got.Intervals[len(got.Intervals)-1]
	if last.State != "second" {
		t.Errorf("final state = %q, want %q (received_at breaks the tie)", last.State, "second")
	}
}

func TestBuildOrdersEqualTimestampsAndReceiptsByEventID(t *testing.T) {
	// Same timestamp and same receipt: only the event id is left to order by.
	got := timeline.Build([]event.Stored{
		ev("e-b", "compute.instance.power_on.end", t1, withState("second")),
		ev("e-a", "compute.instance.power_off.end", t1, withState("first")),
		ev("e-0", "compute.instance.create.end", t0, withState("active"), withSize(map[string]any{"vcpus": 2})),
	})

	last := got.Intervals[len(got.Intervals)-1]
	if last.State != "second" {
		t.Errorf("final state = %q, want %q (event_id breaks the tie)", last.State, "second")
	}
}

func TestBuildIgnoresNonBillableEvent(t *testing.T) {
	size := map[string]any{"vcpus": 2}

	got := timeline.Build([]event.Stored{
		ev("e1", "compute.instance.create.end", t0, withState("active"), withSize(size)),
		// Same state, same size, same project: nothing billable changed.
		ev("e2", "compute.instance.metadata.update.end", t1, withState("active"), withSize(size)),
	})

	if len(got.Intervals) != 1 {
		t.Fatalf("got %d intervals, want 1: %+v", len(got.Intervals), got.Intervals)
	}
	if got.Intervals[0].End != nil {
		t.Errorf("End = %v, want nil: a no-op event must not close the interval", got.Intervals[0].End)
	}
}

func TestBuildDropsZeroLengthIntervals(t *testing.T) {
	// Two billable changes at one instant leave an interval of no duration
	// between them, which bills nothing and must not appear.
	got := timeline.Build([]event.Stored{
		ev("e1", "compute.instance.create.end", t0, withState("active"), withSize(map[string]any{"vcpus": 2})),
		ev("e2", "compute.instance.power_off.end", t1, withState("shutoff"), withReceivedAt(t1)),
		ev("e3", "compute.instance.power_on.end", t1, withState("active"), withReceivedAt(t1.Add(time.Second))),
	})

	for _, interval := range got.Intervals {
		if interval.End != nil && !interval.End.After(interval.Start) {
			t.Errorf("zero-length interval survived: %+v", interval)
		}
	}
	if len(got.Intervals) != 2 {
		t.Fatalf("got %d intervals, want 2: %+v", len(got.Intervals), got.Intervals)
	}
}

func TestBuildProjectTransferSplitsInterval(t *testing.T) {
	size := map[string]any{"vcpus": 2}

	got := timeline.Build([]event.Stored{
		ev("e1", "compute.instance.create.end", t0, withState("active"), withSize(size)),
		ev("e2", "volume.transfer.accept.end", t1, withState("active"), withSize(size), withProject("p-2")),
	})

	if len(got.Intervals) != 2 {
		t.Fatalf("got %d intervals, want 2: %+v", len(got.Intervals), got.Intervals)
	}
	if got.Intervals[0].ProjectID != "p-1" || got.Intervals[1].ProjectID != "p-2" {
		t.Errorf("project ids = %q then %q, want p-1 then p-2",
			got.Intervals[0].ProjectID, got.Intervals[1].ProjectID)
	}
}

func TestBuildHistoryStartingWithoutCreate(t *testing.T) {
	got := timeline.Build([]event.Stored{
		ev("e1", "compute.instance.power_off.end", t0, withState("shutoff"), withSize(map[string]any{"vcpus": 2})),
	})

	if len(got.Intervals) != 1 {
		t.Fatalf("got %d intervals, want 1: %+v", len(got.Intervals), got.Intervals)
	}
	if !got.Intervals[0].Start.Equal(t0) {
		t.Errorf("Start = %v, want the first event %v", got.Intervals[0].Start, t0)
	}
	if got.CreatedAt != nil {
		t.Errorf("CreatedAt = %v, want nil: the create was never seen", got.CreatedAt)
	}
	if !slices.Contains(got.Warnings, timeline.WarningHistoryStartsWithoutCreate) {
		t.Errorf("Warnings = %v, want %q", got.Warnings, timeline.WarningHistoryStartsWithoutCreate)
	}
}

func TestBuildLoneDelete(t *testing.T) {
	got := timeline.Build([]event.Stored{
		ev("e1", "compute.instance.delete.end", t1),
	})

	if len(got.Intervals) != 0 {
		t.Errorf("Intervals = %+v, want none", got.Intervals)
	}
	if got.DeletedAt == nil || !got.DeletedAt.Equal(t1) {
		t.Errorf("DeletedAt = %v, want %v", got.DeletedAt, t1)
	}
	if !slices.Contains(got.Warnings, timeline.WarningHistoryStartsWithoutCreate) {
		t.Errorf("Warnings = %v, want %q", got.Warnings, timeline.WarningHistoryStartsWithoutCreate)
	}
}

func TestBuildCreateAfterDeleteStartsASecondLife(t *testing.T) {
	size := map[string]any{"vcpus": 2}

	got := timeline.Build([]event.Stored{
		ev("e1", "compute.instance.create.end", t0, withState("active"), withSize(size)),
		ev("e2", "compute.instance.delete.end", t1),
		ev("e3", "compute.instance.create.end", t2, withState("active"), withSize(size)),
	})

	// The resource is back, so the delete it superseded is history rather than
	// what the timeline reports. A fold that kept it would tell the projection's
	// replay path that a resource its incremental path holds live is deleted.
	if got.DeletedAt != nil {
		t.Errorf("DeletedAt = %v, want nil: the resource came back at %v", got.DeletedAt, t2)
	}
	if got.CreatedAt == nil || !got.CreatedAt.Equal(t0) {
		t.Errorf("CreatedAt = %v, want the first create %v", got.CreatedAt, t0)
	}
	if len(got.Intervals) != 2 {
		t.Fatalf("got %d intervals, want 2: %+v", len(got.Intervals), got.Intervals)
	}
	if !got.Intervals[1].Start.Equal(t2) || got.Intervals[1].End != nil {
		t.Errorf("second interval = %+v, want one open from the second create %v",
			got.Intervals[1], t2)
	}
}

func TestBuildDoesNotMutateInput(t *testing.T) {
	events := []event.Stored{
		ev("e2", "compute.instance.delete.end", t1),
		ev("e1", "compute.instance.create.end", t0, withState("active"), withSize(map[string]any{"vcpus": 2})),
	}

	timeline.Build(events)

	if events[0].EventID != "e2" {
		t.Errorf("Build sorted the caller's slice in place: %q is first", events[0].EventID)
	}
}

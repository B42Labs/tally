// Package timeline folds a resource's event history into the billable intervals
// it implies. It is the one implementation shared by projection replay, the
// lifecycle endpoint, and the metering engine, so that every consumer folds the
// same history into the same intervals.
//
// The normative specification is roadmap/00-conventions.md section 5.
package timeline

import (
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/b42labs/tally/internal/core/event"
)

// WarningHistoryStartsWithoutCreate marks a timeline whose first event is not a
// create, which means the create was missed and the resource's real start is
// unknown.
const WarningHistoryStartsWithoutCreate = "history_starts_without_create"

// Interval is a half-open span [Start, End) over which a resource's billable
// configuration did not change. An open interval (End nil) means the resource is
// still in this configuration.
type Interval struct {
	Start     time.Time
	End       *time.Time
	State     string
	Size      map[string]any
	ProjectID string
}

// Timeline is a resource's folded history.
type Timeline struct {
	Intervals []Interval
	// CreatedAt is the timestamp of the first create event, nil when the history
	// starts mid-life.
	CreatedAt *time.Time
	// DeletedAt is the timestamp of the delete event, nil while the resource lives.
	DeletedAt *time.Time
	Warnings  []string
}

// Build folds an event history into intervals. The input may arrive in any
// order; Build sorts it by (timestamp, received_at, event_id), which is a total
// order even when several events share an instant.
//
// An empty or nil history yields the zero Timeline.
func Build(events []event.Stored) Timeline {
	if len(events) == 0 {
		return Timeline{}
	}

	ordered := slices.Clone(events)
	slices.SortStableFunc(ordered, Compare)

	var (
		tl     Timeline
		open   Interval
		isOpen bool
	)

	for i, e := range ordered {
		category := event.Categorize(e.EventType)

		if i == 0 && category != event.CategoryCreate {
			tl.Warnings = append(tl.Warnings, WarningHistoryStartsWithoutCreate)
		}
		if category == event.CategoryCreate && tl.CreatedAt == nil {
			ts := e.Timestamp
			tl.CreatedAt = &ts
		}

		// A delete closes the running interval and opens nothing: a deleted
		// resource accrues no usage, so it produces no interval of its own.
		if category == event.CategoryDelete {
			if isOpen {
				tl.Intervals = appendClosed(tl.Intervals, open, e.Timestamp)
				isOpen = false
			}
			ts := e.Timestamp
			tl.DeletedAt = &ts
			continue
		}

		// An absent state or size means "unchanged", so the running values carry
		// forward. The top-level project id is always authoritative.
		next := Interval{
			Start:     e.Timestamp,
			State:     open.State,
			Size:      open.Size,
			ProjectID: e.ProjectID,
		}
		if e.Payload.State != nil {
			next.State = *e.Payload.State
		}
		if e.Payload.Size != nil {
			next.Size = e.Payload.Size
		}

		if !isOpen {
			open, isOpen = next, true
			continue
		}
		if next.State == open.State && next.ProjectID == open.ProjectID && reflect.DeepEqual(next.Size, open.Size) {
			continue
		}

		tl.Intervals = appendClosed(tl.Intervals, open, e.Timestamp)
		open = next
	}

	if isOpen {
		tl.Intervals = append(tl.Intervals, open)
	}
	return tl
}

// appendClosed closes an interval at end and keeps it, unless it would carry no
// duration. Two billable changes at the same instant produce such an interval,
// and it bills nothing, so it never reaches the result.
func appendClosed(intervals []Interval, open Interval, end time.Time) []Interval {
	if !end.After(open.Start) {
		return intervals
	}
	open.End = &end
	return append(intervals, open)
}

// Compare orders two events by (timestamp, received_at, event_id), the total
// order Build folds a history in. It is exported so that every consumer folding
// a history sorts it the same way this package does.
func Compare(a, b event.Stored) int {
	if c := a.Timestamp.Compare(b.Timestamp); c != 0 {
		return c
	}
	if c := a.ReceivedAt.Compare(b.ReceivedAt); c != 0 {
		return c
	}
	return strings.Compare(a.EventID, b.EventID)
}

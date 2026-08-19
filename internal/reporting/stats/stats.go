// Package stats aggregates folded resource histories into the per-window
// activity the project summary reports: per resource type, how many resources
// began or ended their life inside the window, and how many minutes they ran
// within it. It folds through internal/core/timeline, so the summary counts the
// same intervals every other consumer of a history counts.
//
// The package does no I/O. Loading the histories is the caller's job, which
// keeps the arithmetic answerable without a database.
//
// The normative specification is roadmap/02-phase-2-reporting-dashboards.md,
// WP 2.1.
package stats

import (
	"slices"
	"strings"
	"time"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/timeline"
)

// secondsPerMinute converts the second sums the intervals add up to into the
// minutes the summary reports.
const secondsPerMinute = 60

// History is one resource's stored events together with the type of resource
// they describe. Each resource folds on its own, so a caller passes one History
// per resource rather than one merged event slice.
type History struct {
	ResourceType string
	Events       []event.Stored
}

// Activity is what one resource type did inside a window.
type Activity struct {
	ResourceType string
	// Created and Deleted count the resources of the type whose life began or
	// ended inside the window.
	Created int
	Deleted int
	// TotalMinutes is the time every resource of the type ran within the window,
	// truncated to whole minutes.
	TotalMinutes int64
}

// Summarize folds each history and aggregates the results per resource type
// over the half-open window [from, to), for the project projectID names. A
// create or a delete counts when the folded timestamp falls inside the window.
// An interval contributes the seconds it overlaps the window, whatever state the
// resource was in: what a state is worth is a rating question, and rating is
// Phase 3. An interval still open ends at now, so a window reaching into the
// future bills no time that has not been served yet.
//
// The histories are whole ones, and projectID is what says which part of each
// belongs to this summary. A resource that changed hands accrues its seconds
// here only over the intervals it carried this project through: the transfer
// closes the interval the resource ran under the old owner and opens one under
// the new, so a resource is billed to one project at a time rather than to both
// for the rest of its life. What a project created and deleted is read off its
// own events instead, because a transfer moves a resource rather than its birth.
//
// A resource type gets a row once one of the project's histories of it folds
// into a life or an interval, even when the window holds nothing of it. A
// history the project has no event in folds into nothing and gets no row.
//
// The result is sorted by resource type and is never nil.
func Summarize(histories []History, projectID string, from, to, now time.Time) []Activity {
	type totals struct {
		created int
		deleted int
		seconds int64
	}

	byType := make(map[string]totals, len(histories))
	for _, h := range histories {
		tl := timeline.Build(h.Events)
		own := timeline.Build(eventsOf(h.Events, projectID))
		if own.CreatedAt == nil && own.DeletedAt == nil && len(own.Intervals) == 0 {
			continue
		}

		t := byType[h.ResourceType]
		if inWindow(own.CreatedAt, from, to) {
			t.created++
		}
		if inWindow(own.DeletedAt, from, to) {
			t.deleted++
		}
		for _, interval := range tl.Intervals {
			if interval.ProjectID != projectID {
				continue
			}
			t.seconds += overlapSeconds(interval, from, to, now)
		}
		byType[h.ResourceType] = t
	}

	activities := make([]Activity, 0, len(byType))
	for resourceType, t := range byType {
		activities = append(activities, Activity{
			ResourceType: resourceType,
			Created:      t.created,
			Deleted:      t.deleted,
			TotalMinutes: t.seconds / secondsPerMinute,
		})
	}
	slices.SortFunc(activities, func(a, b Activity) int {
		return strings.Compare(a.ResourceType, b.ResourceType)
	})
	return activities
}

// eventsOf is the part of a history one project wrote, which is what its own
// lifecycle is folded from: a transfer carries the new project on the event that
// moves the resource, so an event names the project that held it when it
// happened.
func eventsOf(events []event.Stored, projectID string) []event.Stored {
	own := make([]event.Stored, 0, len(events))
	for _, e := range events {
		if e.ProjectID == projectID {
			own = append(own, e)
		}
	}
	return own
}

// inWindow reports whether a lifecycle timestamp falls inside the half-open
// window [from, to). A nil timestamp is a life that never began or has not
// ended, and neither happened in any window.
func inWindow(ts *time.Time, from, to time.Time) bool {
	return ts != nil && !ts.Before(from) && ts.Before(to)
}

// overlapSeconds is the whole seconds an interval ran inside [from, to). An open
// interval ends at now, which is as far as it has accrued; clipping it to a
// later to would bill time the resource has not spent yet.
func overlapSeconds(interval timeline.Interval, from, to, now time.Time) int64 {
	start, end := interval.Start, now
	if interval.End != nil {
		end = *interval.End
	}
	if start.Before(from) {
		start = from
	}
	if to.Before(end) {
		end = to
	}
	if !end.After(start) {
		return 0
	}
	return int64(end.Sub(start) / time.Second)
}

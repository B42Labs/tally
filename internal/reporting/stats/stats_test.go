package stats_test

import (
	"slices"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/reporting/stats"
)

// The window every case is measured against, unless the case names its own. It
// is a day long, so a resource that runs through all of it bills 1440 minutes.
var (
	windowFrom = time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	windowTo   = windowFrom.Add(24 * time.Hour)
)

// summaryProject is the project every fixture belongs to unless the case moves a
// resource out of it, and otherProject is where a transferred resource ends up.
const (
	summaryProject = "p-1"
	otherProject   = "p-2"
)

// ev builds a stored event with the identity every fixture here shares. These
// tests vary only the event type and the timestamps.
func ev(id, eventType string, ts time.Time) event.Stored {
	return event.Stored{
		Event: event.Event{
			EventID:      id,
			Timestamp:    ts,
			EventType:    eventType,
			Platform:     "openstack",
			Cloud:        "os-prod-eu1",
			ResourceType: "instance",
			ResourceID:   "i-1",
			ProjectID:    summaryProject,
			Source:       event.SourceCollector,
		},
		ReceivedAt: ts,
	}
}

// createEvent builds a create, which carries the state and size a create is
// required to.
func createEvent(id string, ts time.Time) event.Stored {
	e := ev(id, "compute.instance.create.end", ts)
	state := "active"
	e.Payload.State = &state
	e.Payload.Size = map[string]any{"vcpus": 2}
	return e
}

// deleteEvent builds a delete, which carries no payload: the fold reads a
// resource's fate from the event type.
func deleteEvent(id string, ts time.Time) event.Stored {
	return ev(id, "compute.instance.delete.end", ts)
}

// transferEvent builds the event that moves a resource to another project. It
// is an ordinary update, so what says the resource changed hands is the project
// it names rather than its type.
func transferEvent(id, projectID string, ts time.Time) event.Stored {
	e := ev(id, "compute.instance.update.end", ts)
	e.ProjectID = projectID
	return e
}

func TestSummarize(t *testing.T) {
	tests := []struct {
		name      string
		histories []stats.History
		// projectID is whose summary this is. It is summaryProject unless the
		// case names another one, which is what the two transfer cases do.
		projectID string
		from      time.Time
		to        time.Time
		now       time.Time
		want      []stats.Activity
	}{
		{
			name:      "nil histories",
			histories: nil,
			from:      windowFrom,
			to:        windowTo,
			now:       windowTo,
			want:      []stats.Activity{},
		},
		{
			name:      "no histories",
			histories: []stats.History{},
			from:      windowFrom,
			to:        windowTo,
			now:       windowTo,
			want:      []stats.Activity{},
		},
		{
			// The history names a resource type but says nothing about it, so
			// there is no row to open for it.
			name:      "a history without events opens no row",
			histories: []stats.History{{ResourceType: "instance"}},
			from:      windowFrom,
			to:        windowTo,
			now:       windowTo,
			want:      []stats.Activity{},
		},
		{
			name: "an interval covering the window bills every minute of it",
			histories: []stats.History{{
				ResourceType: "instance",
				Events: []event.Stored{
					createEvent("e1", windowFrom.Add(-time.Hour)),
				},
			}},
			from: windowFrom,
			to:   windowTo,
			now:  windowTo,
			// The create is older than the window, so only the minutes count.
			want: []stats.Activity{{ResourceType: "instance", TotalMinutes: 1440}},
		},
		{
			// Both ends of the half-open window at once: the create sits on from
			// and counts, the delete sits on to and does not, and the interval
			// stops at to rather than reaching past it.
			name: "a create at from is inside the window and a delete at to is not",
			histories: []stats.History{{
				ResourceType: "instance",
				Events: []event.Stored{
					createEvent("e1", windowFrom),
					deleteEvent("e2", windowTo),
				},
			}},
			from: windowFrom,
			to:   windowTo,
			now:  windowTo,
			want: []stats.Activity{{ResourceType: "instance", Created: 1, TotalMinutes: 1440}},
		},
		{
			// The fold takes a resource's fate from its newest lifecycle event,
			// so the comeback leaves the resource created and not deleted. Both
			// lives bill: one hour before the delete, 21 hours after the second
			// create.
			name: "a create after a delete counts one life and bills both intervals",
			histories: []stats.History{{
				ResourceType: "instance",
				Events: []event.Stored{
					createEvent("e1", windowFrom.Add(time.Hour)),
					deleteEvent("e2", windowFrom.Add(2*time.Hour)),
					createEvent("e3", windowFrom.Add(3*time.Hour)),
				},
			}},
			from: windowFrom,
			to:   windowTo,
			now:  windowTo,
			want: []stats.Activity{{ResourceType: "instance", Created: 1, TotalMinutes: 1320}},
		},
		{
			name: "an open interval bills up to now, not to the end of a future window",
			histories: []stats.History{{
				ResourceType: "instance",
				Events:       []event.Stored{createEvent("e1", windowFrom)},
			}},
			from: windowFrom,
			to:   windowFrom.Add(48 * time.Hour),
			now:  windowFrom.Add(24 * time.Hour),
			want: []stats.Activity{{ResourceType: "instance", Created: 1, TotalMinutes: 1440}},
		},
		{
			name: "a window that ends where it starts holds nothing",
			histories: []stats.History{{
				ResourceType: "instance",
				Events:       []event.Stored{createEvent("e1", windowFrom)},
			}},
			from: windowFrom,
			to:   windowFrom,
			now:  windowTo,
			want: []stats.Activity{{ResourceType: "instance"}},
		},
		{
			name: "a window that ends before it starts holds nothing",
			histories: []stats.History{{
				ResourceType: "instance",
				Events:       []event.Stored{createEvent("e1", windowFrom)},
			}},
			from: windowTo,
			to:   windowFrom,
			now:  windowTo,
			want: []stats.Activity{{ResourceType: "instance"}},
		},
		{
			// 119 seconds are one minute and 59 seconds the summary drops: a
			// minute is reported once it has been served in full.
			name: "seconds short of a minute are dropped",
			histories: []stats.History{{
				ResourceType: "instance",
				Events: []event.Stored{
					createEvent("e1", windowFrom),
					deleteEvent("e2", windowFrom.Add(119*time.Second)),
				},
			}},
			from: windowFrom,
			to:   windowTo,
			now:  windowTo,
			want: []stats.Activity{{ResourceType: "instance", Created: 1, Deleted: 1, TotalMinutes: 1}},
		},
		{
			// The volume comes first in the input and second in the result.
			name: "histories of one type add up and the rows come out sorted",
			histories: []stats.History{
				{
					ResourceType: "volume",
					Events:       []event.Stored{createEvent("v1", windowFrom.Add(time.Hour))},
				},
				{
					ResourceType: "instance",
					Events: []event.Stored{
						createEvent("a1", windowFrom),
						deleteEvent("a2", windowFrom.Add(time.Hour)),
					},
				},
				{
					ResourceType: "instance",
					Events: []event.Stored{
						createEvent("b1", windowFrom.Add(2*time.Hour)),
						deleteEvent("b2", windowFrom.Add(3*time.Hour)),
					},
				},
			},
			from: windowFrom,
			to:   windowTo,
			now:  windowTo,
			want: []stats.Activity{
				{ResourceType: "instance", Created: 2, Deleted: 2, TotalMinutes: 120},
				{ResourceType: "volume", Created: 1, TotalMinutes: 1380},
			},
		},
		{
			// The transfer moves the resource six hours into the window. The old
			// owner accrues those six hours and stops there: its own events end at
			// the transfer, and reading them alone would leave an interval that
			// never closes and bills up to now beside the new owner's.
			name: "a resource transferred away stops accruing at the transfer",
			histories: []stats.History{{
				ResourceType: "instance",
				Events: []event.Stored{
					createEvent("e1", windowFrom),
					transferEvent("e2", otherProject, windowFrom.Add(6*time.Hour)),
				},
			}},
			from: windowFrom,
			to:   windowTo,
			now:  windowTo,
			want: []stats.Activity{{ResourceType: "instance", Created: 1, TotalMinutes: 360}},
		},
		{
			// The same history read as the new owner: the eighteen hours after the
			// transfer and not one minute before it. The create belongs to the old
			// owner, so it is not counted here even though it falls in the window,
			// and the two summaries together bill the window exactly once.
			name: "a resource transferred in accrues from the transfer",
			histories: []stats.History{{
				ResourceType: "instance",
				Events: []event.Stored{
					createEvent("e1", windowFrom),
					transferEvent("e2", otherProject, windowFrom.Add(6*time.Hour)),
				},
			}},
			projectID: otherProject,
			from:      windowFrom,
			to:        windowTo,
			now:       windowTo,
			want:      []stats.Activity{{ResourceType: "instance", TotalMinutes: 1080}},
		},
		{
			// A history the project never held opens no row, whatever the fold
			// makes of it: the intervals belong to someone else and the lifecycle
			// is not this project's either.
			name: "a history of another project alone opens no row",
			histories: []stats.History{{
				ResourceType: "instance",
				Events:       []event.Stored{createEvent("e1", windowFrom)},
			}},
			projectID: otherProject,
			from:      windowFrom,
			to:        windowTo,
			now:       windowTo,
			want:      []stats.Activity{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			projectID := tc.projectID
			if projectID == "" {
				projectID = summaryProject
			}

			got := stats.Summarize(tc.histories, projectID, tc.from, tc.to, tc.now)

			// A caller encodes the result as a JSON array, which a nil slice
			// would turn into null rather than [].
			if got == nil {
				t.Fatal("Summarize returned nil, want an empty slice")
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("Summarize() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

package simulator

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/providers/openstack"
)

// TestEveryBillableTransitionIsBookedOnce holds the fact ledger against the
// schedule it is built beside. The effect of a transition is stated at the emit
// that produces it, so an emit handed the unbooked effect by mistake leaves the
// month with a billable transition the oracle says nothing about. The
// comparison would then report the engine at fault for a resource nobody stated
// a size for, which is the one failure a comparison must never invent.
//
// The oslo type of every fact is held against billableTypes as well, so an emit
// that booked a type the collector records nothing for is caught here too.
func TestEveryBillableTransitionIsBookedOnce(t *testing.T) {
	// booking is one transition as the ledger and the schedule both name it: the
	// instant it happened at and the resource it is about. The instant alone
	// would not do, since two resources of a month change in the same second.
	type booking struct {
		at time.Time
		id string
	}

	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			g := buildMonth(t, seed)
			billable := g.schedule.Billable()

			if len(g.facts) != len(billable) {
				t.Errorf("the month books %d facts over %d billable transitions, want one fact per "+
					"transition", len(g.facts), len(billable))
			}

			booked := make(map[booking]int, len(g.facts))
			for _, f := range g.facts {
				booked[booking{at: f.at, id: f.resourceID}]++
				if f.eventType == imageCreateType {
					t.Errorf("%s at %s is booked, want the one type the mapping skips to stay out of "+
						"the ledger: the image has no size yet", f.eventType, f.at.Format(time.RFC3339))
					continue
				}
				if _, ok := billableTypes[f.eventType]; !ok {
					t.Errorf("%s at %s is booked under a type the ledger does not name, want every "+
						"fact to carry one of the %d types billableTypes holds", f.eventType,
						f.at.Format(time.RFC3339), len(billableTypes))
				}
			}
			emitted := make(map[booking]int, len(billable))
			for _, transition := range billable {
				emitted[booking{at: transition.At, id: transition.ResourceID}]++
			}

			for b, count := range emitted {
				if booked[b] != count {
					t.Errorf("resource %s at %s is emitted %d times and booked %d times, want the "+
						"ledger to state the effect of every billable transition", b.id,
						b.at.Format(time.RFC3339), count, booked[b])
				}
			}
			for b, count := range booked {
				if _, ok := emitted[b]; !ok {
					t.Errorf("resource %s at %s is booked %d times and emitted never, want the ledger "+
						"to state nothing the month did not do", b.id, b.at.Format(time.RFC3339), count)
				}
			}
		})
	}
}

// factKey is one fact as the schedule names it as well: the instant it happened
// at and the resource it is about. No two facts of a month share the pair.
type factKey struct {
	at         time.Time
	resourceID string
}

// TestOracleAgreesWithTheMapping holds the ledger's own vocabulary against the
// collector's. The ledger states what a transition meant without reading the
// mapping, so the two only agree as long as nobody renames an event type, moves
// a resource under another type, or changes what a size object holds on one
// side alone. A drift here would have the comparison report the engine for
// every resource of the month.
func TestOracleAgreesWithTheMapping(t *testing.T) {
	g := buildMonth(t, 1)

	booked := make(map[factKey]fact, len(g.facts))
	for _, f := range g.facts {
		booked[factKey{at: f.at, resourceID: f.resourceID}] = f
	}

	// What the collector would record, counted the way the oracle counts it.
	recorded := make(map[countKey]int, len(g.facts))

	for _, transition := range g.schedule.Billable() {
		mapped, ok := openstack.MapNotification(parse(t, render(t, transition)), testCloud)
		if !ok {
			t.Fatalf("the mapping records nothing for %s at %s, want an event per billable transition",
				transition.EventType, transition.At.Format(time.RFC3339))
		}
		recorded[countKey{projectID: mapped.ProjectID, eventType: mapped.EventType}]++

		f, ok := booked[factKey{at: transition.At, resourceID: transition.ResourceID}]
		if !ok {
			t.Fatalf("%s at %s is billable and unbooked, want a fact per billable transition",
				transition.EventType, transition.At.Format(time.RFC3339))
		}
		holdFactAgainstEvent(t, f, mapped)
	}

	oracle, err := buildOracle(g.facts, 1, testCloud, july2026, july2026.AddDate(0, 1, 0))
	if err != nil {
		t.Fatalf("buildOracle() error = %v, want nil", err)
	}

	stated := make(map[countKey]int, len(oracle.Counts))
	for _, count := range oracle.Counts {
		stated[countKey{projectID: count.ProjectID, eventType: count.EventType}] = count.Count
	}
	for key, count := range recorded {
		if stated[key] != count {
			t.Errorf("the oracle expects %d %s events in project %s, want %d", stated[key],
				key.eventType, key.projectID, count)
		}
	}
	for key, count := range stated {
		if _, ok := recorded[key]; !ok {
			t.Errorf("the oracle expects %d %s events in project %s, want none the collector records",
				count, key.eventType, key.projectID)
		}
	}
	if !slices.IsSortedFunc(oracle.Counts, func(a, b OracleCount) int {
		if c := strings.Compare(a.ProjectID, b.ProjectID); c != 0 {
			return c
		}
		return strings.Compare(a.EventType, b.EventType)
	}) {
		t.Errorf("the oracle's counts run %v, want them sorted by project and then event type", oracle.Counts)
	}
}

// holdFactAgainstEvent holds one fact against the event the collector's mapping
// makes of the same transition.
func holdFactAgainstEvent(t *testing.T, f fact, mapped event.Event) {
	t.Helper()

	booked := billableTypes[f.eventType]
	if booked.resourceType != mapped.ResourceType {
		t.Errorf("%s books resource type %q, want the mapping's %q", f.eventType,
			booked.resourceType, mapped.ResourceType)
	}
	if booked.eventType != mapped.EventType {
		t.Errorf("%s books event type %q, want the mapping's %q", f.eventType,
			booked.eventType, mapped.EventType)
	}
	if f.projectID != mapped.ProjectID {
		t.Errorf("%s books project %q, want the mapping's %q", f.eventType, f.projectID, mapped.ProjectID)
	}
	if ended := event.Categorize(mapped.EventType) == event.CategoryDelete; f.effect.ended != ended {
		t.Errorf("%s books ended = %t, want the mapping's %t", f.eventType, f.effect.ended, ended)
	}
	if !f.effect.ended {
		switch {
		case mapped.Payload.State == nil:
			t.Errorf("%s books state %q and the mapping books none", f.eventType, f.effect.state)
		case f.effect.state != *mapped.Payload.State:
			t.Errorf("%s books state %q, want the mapping's %q", f.eventType, f.effect.state,
				*mapped.Payload.State)
		}
	}
	if mapped.Payload.Size == nil {
		return
	}
	if len(f.effect.size) != len(mapped.Payload.Size) {
		t.Errorf("%s books the size %v, want the members of the mapping's %v", f.eventType,
			f.effect.size, mapped.Payload.Size)
		return
	}
	for member, value := range f.effect.size {
		want, ok := mapped.Payload.Size[member]
		if !ok {
			t.Errorf("%s books the size member %q, which the mapping's size does not hold", f.eventType, member)
			continue
		}
		holdMemberAgainst(t, f.eventType, member, value, want, mapped.Payload.Size)
	}
}

// holdMemberAgainst holds one size member of a fact against the same member of
// an object the engine reads. A member the ledger states as text is compared as
// text, and a number as a decimal, because the same quantity reaches the engine
// as a json.Number here and as an int where the mapping counted something.
func holdMemberAgainst(t *testing.T, eventType, member string, booked, want any, object map[string]any) {
	t.Helper()

	switch booked := booked.(type) {
	case string:
		text, ok := want.(string)
		if !ok || text != booked {
			t.Errorf("%s books %s = %q, want %v", eventType, member, booked, want)
		}
	case json.Number:
		quantity, ok := rating.QuantityOf(object, member)
		if !ok {
			t.Errorf("%s carries %s = %v, which no quantity is read from", eventType, member, want)
			return
		}
		number, err := decimal.NewFromString(booked.String())
		if err != nil {
			t.Errorf("%s books %s = %q, want a number: %v", eventType, member, booked, err)
			return
		}
		if !number.Equal(quantity) {
			t.Errorf("%s books %s = %s, want %s", eventType, member, number, quantity)
		}
	default:
		t.Errorf("%s books %s as a %T, want a string or a json.Number", eventType, member, booked)
	}
}

// TestOracleAgreesWithTheEngineFold folds every month of the first five seeds
// twice: once here, out of the fact ledger, and once through the engine, out of
// the events the collector's mapping makes of the very same transitions. The
// two folds are written from the same rules and share no code, so a month they
// disagree on is a bug in one of them rather than a comparison that agrees with
// itself.
func TestOracleAgreesWithTheEngineFold(t *testing.T) {
	to := july2026.AddDate(0, 1, 0)

	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			month, err := GenerateMonth(seed, july2026, to, testCloud, Faults{})
			if err != nil {
				t.Fatalf("GenerateMonth(%d, %s, %q) error = %v, want nil", seed,
					july2026.Format(time.RFC3339), testCloud, err)
			}

			history := make(map[resourceKey][]event.Stored)
			for _, transition := range month.Schedule.Billable() {
				mapped, ok := openstack.MapNotification(parse(t, render(t, transition)), testCloud)
				if !ok {
					t.Fatalf("the mapping records nothing for %s at %s, want an event per billable transition",
						transition.EventType, transition.At.Format(time.RFC3339))
				}
				key := resourceKey{resourceType: mapped.ResourceType, resourceID: mapped.ResourceID}
				history[key] = append(history[key], event.Stored{Event: mapped})
			}

			metered := make(map[resourceKey][]metering.UsageDraft, len(history))
			for key, events := range history {
				drafts, err := metering.MeterResource(events, july2026, to)
				if err != nil {
					t.Fatalf("MeterResource(%s %s) error = %v, want nil", key.resourceType,
						key.resourceID, err)
				}
				// A resource the period bills nothing for is one the oracle leaves
				// out as well, so it is not held against an interval it has none of.
				if len(drafts) == 0 {
					continue
				}
				metered[key] = drafts
			}

			stated := make(map[resourceKey]OracleResource, len(month.Oracle.Resources))
			for _, resource := range month.Oracle.Resources {
				stated[resourceKey{resourceType: resource.ResourceType, resourceID: resource.ResourceID}] = resource
			}

			for key, drafts := range metered {
				resource, ok := stated[key]
				if !ok {
					t.Errorf("the engine bills %s %s over %d intervals, and the oracle states none",
						key.resourceType, key.resourceID, len(drafts))
					continue
				}
				holdIntervalsAgainstDrafts(t, resource, drafts)
			}
			for key := range stated {
				if _, ok := metered[key]; !ok {
					t.Errorf("the oracle states %s %s, and the engine bills nothing for it",
						key.resourceType, key.resourceID)
				}
			}
		})
	}
}

// holdIntervalsAgainstDrafts holds one resource's oracle intervals against the
// drafts the engine folded the same history into.
func holdIntervalsAgainstDrafts(t *testing.T, resource OracleResource, drafts []metering.UsageDraft) {
	t.Helper()

	if len(drafts) != len(resource.Intervals) {
		t.Errorf("%s %s is billed over %d intervals, want the %d the oracle states",
			resource.ResourceType, resource.ResourceID, len(drafts), len(resource.Intervals))
		return
	}
	for i, interval := range resource.Intervals {
		draft := drafts[i]
		if !draft.FromTS.Equal(interval.From) || !draft.ToTS.Equal(interval.To) {
			t.Errorf("%s %s interval %d is billed [%s, %s), want [%s, %s)", resource.ResourceType,
				resource.ResourceID, i, draft.FromTS.Format(time.RFC3339), draft.ToTS.Format(time.RFC3339),
				interval.From.Format(time.RFC3339), interval.To.Format(time.RFC3339))
		}
		if draft.State != interval.State {
			t.Errorf("%s %s interval %d is billed in state %q, want %q", resource.ResourceType,
				resource.ResourceID, i, draft.State, interval.State)
		}
		if draft.ProjectID != interval.ProjectID {
			t.Errorf("%s %s interval %d is billed to project %q, want %q", resource.ResourceType,
				resource.ResourceID, i, draft.ProjectID, interval.ProjectID)
		}
		for member, value := range interval.Size {
			holdMemberAgainst(t, resource.ResourceType, member, value, draft.Usage[member], draft.Usage)
		}
	}
}

// TestOracleUsesNoEngineFold holds the one property that makes the comparison
// worth running: the oracle is folded here, by this package's own code. An
// oracle that called the timeline or the metering engine would report every
// month as correct, including the ones those packages fold wrongly.
func TestOracleUsesNoEngineFold(t *testing.T) {
	forbidden := []string{
		"github.com/b42labs/tally/internal/core/timeline",
		"github.com/b42labs/tally/internal/engine/metering",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir(.) error = %v, want nil", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%s) error = %v, want nil", name, err)
		}
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("the import %s of %s is unreadable: %v", imported.Path.Value, name, err)
			}
			if slices.Contains(forbidden, path) {
				t.Errorf("%s imports %s, want the oracle folded by this package alone", name, path)
			}
		}
	}
}

// TestOracleIntervalsFollowTheLives holds the folded month against the lives the
// generator gave its resources: the transfer that moves a volume between two
// projects, the resize that puts a server into a state of its own for a minute,
// the delete that ends a life, and the shoot that is gone before the month is.
func TestOracleIntervalsFollowTheLives(t *testing.T) {
	g := buildMonth(t, 1)

	oracle, err := buildOracle(g.facts, 1, testCloud, july2026, july2026.AddDate(0, 1, 0))
	if err != nil {
		t.Fatalf("buildOracle() error = %v, want nil", err)
	}

	t.Run("every interval lies in the month and follows the one before it", func(t *testing.T) {
		for _, resource := range oracle.Resources {
			var previous time.Time
			for i, interval := range resource.Intervals {
				if interval.From.Before(july2026) || interval.To.After(g.to) {
					t.Errorf("%s %s interval %d is [%s, %s), want it inside the month", resource.ResourceType,
						resource.ResourceID, i, interval.From.Format(time.RFC3339),
						interval.To.Format(time.RFC3339))
				}
				if !interval.To.After(interval.From) {
					t.Errorf("%s %s interval %d is [%s, %s), want it to carry a length",
						resource.ResourceType, resource.ResourceID, i,
						interval.From.Format(time.RFC3339), interval.To.Format(time.RFC3339))
				}
				if i > 0 && !interval.From.Equal(previous) {
					t.Errorf("%s %s interval %d starts at %s, want the %s the interval before it ended at",
						resource.ResourceType, resource.ResourceID, i,
						interval.From.Format(time.RFC3339), previous.Format(time.RFC3339))
				}
				previous = interval.To
			}
		}
	})

	t.Run("the transferred volume changes hands where it was accepted", func(t *testing.T) {
		spare := resourceNamed(t, oracle, g.projects[0].spare.id)
		if len(spare.Intervals) != 2 {
			t.Fatalf("the spare volume is billed over %d intervals, want the 2 the transfer splits it into",
				len(spare.Intervals))
		}
		accepted := transitionAt(t, g.schedule, "volume.transfer.accept.end", spare.ResourceID)
		if spare.Intervals[0].ProjectID != g.projects[0].id {
			t.Errorf("the spare volume is billed to %q until it is accepted, want %q",
				spare.Intervals[0].ProjectID, g.projects[0].id)
		}
		if spare.Intervals[1].ProjectID != g.projects[1].id {
			t.Errorf("the spare volume is billed to %q after it is accepted, want %q",
				spare.Intervals[1].ProjectID, g.projects[1].id)
		}
		if !spare.Intervals[0].To.Equal(accepted) || !spare.Intervals[1].From.Equal(accepted) {
			t.Errorf("the spare volume changes hands between %s and %s, want both at %s",
				spare.Intervals[0].To.Format(time.RFC3339), spare.Intervals[1].From.Format(time.RFC3339),
				accepted.Format(time.RFC3339))
		}
	})

	t.Run("the resized server is billed as resized for the resize alone", func(t *testing.T) {
		first := resourceNamed(t, oracle, g.projects[0].instances[0].id)
		index := slices.IndexFunc(first.Intervals, func(iv OracleInterval) bool {
			return iv.State == stateResized
		})
		if index < 0 || index == len(first.Intervals)-1 {
			t.Fatalf("the first instance runs through the states %v, want a resized one that is followed",
				statesOf(first))
		}
		resized, active := first.Intervals[index], first.Intervals[index+1]
		if got := resized.To.Sub(resized.From); got != resizeDuration {
			t.Errorf("the first instance is resized for %s, want %s", got, resizeDuration)
		}
		if active.State != stateActive {
			t.Errorf("the first instance is billed as %q after the resize, want %q", active.State, stateActive)
		}
		if resized.Size["vcpus"] != active.Size["vcpus"] {
			t.Errorf("the first instance carries %v vcpus while it resizes and %v after it, want the "+
				"flavor it is moving to on both", resized.Size["vcpus"], active.Size["vcpus"])
		}
		last := first.Intervals[len(first.Intervals)-1]
		deleted := transitionAt(t, g.schedule, "compute.instance.delete.end", first.ResourceID)
		if !last.To.Equal(deleted) {
			t.Errorf("the first instance is billed to %s, want the %s it was deleted at",
				last.To.Format(time.RFC3339), deleted.Format(time.RFC3339))
		}
	})

	t.Run("the shoot that is torn down bills nothing to the end of the month", func(t *testing.T) {
		var ids []string
		for _, transition := range transitionsOf(g.schedule, shootNamed(t, g, "batch")) {
			if !slices.Contains(ids, transition.ResourceID) {
				ids = append(ids, transition.ResourceID)
			}
		}
		for _, id := range ids {
			resource := resourceNamed(t, oracle, id)
			last := resource.Intervals[len(resource.Intervals)-1]
			if !last.To.Before(g.to) {
				t.Errorf("%s %s is billed to %s, want an end before the month's %s", resource.ResourceType,
					id, last.To.Format(time.RFC3339), g.to.Format(time.RFC3339))
			}
		}
	})
}

// resourceNamed returns the oracle's statement about one resource or fails the
// test.
func resourceNamed(t *testing.T, oracle Oracle, id string) OracleResource {
	t.Helper()

	for _, resource := range oracle.Resources {
		if resource.ResourceID == id {
			return resource
		}
	}
	t.Fatalf("the oracle states nothing about %s, want the resource the month billed", id)
	return OracleResource{}
}

// statesOf names the states a resource ran through, for the message of a test
// that expected one of them.
func statesOf(resource OracleResource) []string {
	states := make([]string, 0, len(resource.Intervals))
	for _, interval := range resource.Intervals {
		states = append(states, interval.State)
	}
	return states
}

// transitionAt returns the instant one resource reported one event type at, or
// fails the test when the month holds no such transition.
func transitionAt(t *testing.T, schedule Schedule, eventType, resourceID string) time.Time {
	t.Helper()

	for _, transition := range schedule {
		if transition.EventType == eventType && transition.ResourceID == resourceID {
			return transition.At
		}
	}
	t.Fatalf("the month holds no %s of %s, want the one the resource's life turns on", eventType, resourceID)
	return time.Time{}
}

// TestBuildOracleClipsToTheMonth folds a ledger written by hand rather than by
// the generator, because a generated month emits nothing outside the month it
// was generated for. What the clip is written for are the resources that
// already existed when the month began, and the only way to state one today is
// to write the fact down.
func TestBuildOracleClipsToTheMonth(t *testing.T) {
	from := july2026
	to := from.AddDate(0, 1, 0)
	const (
		owner    = "p1"
		neighbor = "p2"
	)

	small := volumeSizeOf(&volume{sizeGB: 50, volumeType: "ssd"})
	grown := volumeSizeOf(&volume{sizeGB: 100, volumeType: "ssd"})
	facts := []fact{
		{
			at: from.Add(-time.Hour), eventType: "volume.create.end", resourceID: "vol-1",
			projectID: owner, workload: workloadClassic, effect: alive(stateAvailable, small),
		},
		{
			at: from.Add(2 * time.Hour), eventType: "volume.resize.end", resourceID: "vol-1",
			projectID: owner, workload: workloadClassic, effect: alive(stateAvailable, grown),
		},
		{
			at: from.Add(-48 * time.Hour), eventType: "compute.instance.create.end", resourceID: "srv-1",
			projectID: owner, workload: workloadClassic,
			effect: alive(stateActive, instanceSizeOf(flavors[0])),
		},
		{
			at: from.Add(-24 * time.Hour), eventType: "compute.instance.delete.end", resourceID: "srv-1",
			projectID: owner, workload: workloadClassic, effect: deleted,
		},
		{
			at: from.Add(3 * time.Hour), eventType: "floatingip.delete.end", resourceID: "fip-1",
			projectID: neighbor, workload: workloadClassic, effect: deleted,
		},
	}

	oracle, err := buildOracle(facts, 7, testCloud, from, to)
	if err != nil {
		t.Fatalf("buildOracle() error = %v, want nil", err)
	}

	t.Run("the oracle names the month it was folded for", func(t *testing.T) {
		if oracle.Format != oracleFormat {
			t.Errorf("the oracle states format %d, want the %d this build writes", oracle.Format, oracleFormat)
		}
		if oracle.Seed != 7 || oracle.Cloud != testCloud {
			t.Errorf("the oracle is seed %d of %q, want seed 7 of %q", oracle.Seed, oracle.Cloud, testCloud)
		}
		if !oracle.PeriodFrom.Equal(from) || !oracle.PeriodTo.Equal(to) {
			t.Errorf("the oracle covers [%s, %s), want [%s, %s)", oracle.PeriodFrom.Format(time.RFC3339),
				oracle.PeriodTo.Format(time.RFC3339), from.Format(time.RFC3339), to.Format(time.RFC3339))
		}
	})

	t.Run("a resource older than the month is billed from its start", func(t *testing.T) {
		vol := resourceNamed(t, oracle, "vol-1")
		want := []OracleInterval{
			{From: from, To: from.Add(2 * time.Hour), State: stateAvailable, ProjectID: owner, Size: small},
			{From: from.Add(2 * time.Hour), To: to, State: stateAvailable, ProjectID: owner, Size: grown},
		}
		if len(vol.Intervals) != len(want) {
			t.Fatalf("the volume is billed over %d intervals, want %d", len(vol.Intervals), len(want))
		}
		for i, interval := range want {
			if !reflect.DeepEqual(vol.Intervals[i], interval) {
				t.Errorf("the volume's interval %d = %+v, want %+v", i, vol.Intervals[i], interval)
			}
		}
	})

	t.Run("a resource that died before the month is left out", func(t *testing.T) {
		if index := slices.IndexFunc(oracle.Resources, func(r OracleResource) bool {
			return r.ResourceID == "srv-1"
		}); index >= 0 {
			t.Errorf("the oracle states %+v, want nothing about a server the month never held",
				oracle.Resources[index])
		}
	})

	t.Run("a delete with no life behind it opens nothing", func(t *testing.T) {
		if index := slices.IndexFunc(oracle.Resources, func(r OracleResource) bool {
			return r.ResourceID == "fip-1"
		}); index >= 0 {
			t.Errorf("the oracle states %+v, want nothing about an address whose only fact is its delete",
				oracle.Resources[index])
		}
	})

	t.Run("every booked fact is counted, whichever month it happened in", func(t *testing.T) {
		want := []OracleCount{
			{ProjectID: owner, EventType: "compute.instance.create.end", Count: 1},
			{ProjectID: owner, EventType: "compute.instance.delete.end", Count: 1},
			{ProjectID: owner, EventType: "volume.create.end", Count: 1},
			{ProjectID: owner, EventType: "volume.resize.end", Count: 1},
			{ProjectID: neighbor, EventType: "floatingip.delete.end", Count: 1},
		}
		if !slices.Equal(oracle.Counts, want) {
			t.Errorf("the oracle counts %+v, want %+v", oracle.Counts, want)
		}
	})

	t.Run("an empty ledger states an empty month", func(t *testing.T) {
		empty, err := buildOracle(nil, 7, testCloud, from, to)
		if err != nil {
			t.Fatalf("buildOracle(nil) error = %v, want nil", err)
		}
		// The two are rendered as arrays rather than as null, so a comparison
		// reads an empty month as one that states nothing rather than as one it
		// cannot read.
		if empty.Resources == nil || len(empty.Resources) != 0 {
			t.Errorf("buildOracle(nil) states the resources %v, want an empty list", empty.Resources)
		}
		if empty.Counts == nil || len(empty.Counts) != 0 {
			t.Errorf("buildOracle(nil) states the counts %v, want an empty list", empty.Counts)
		}
	})
}

// TestBuildOracleKeepsAFactThatRestatesTheOpenInterval folds two facts that
// say the same thing about one resource at two instants. No generated month
// holds such a pair today — a retype always picks another type and a resize
// always doubles the size — so the ledger is written by hand. The fold has to
// pass the second fact over rather than close the interval at it: an event that
// changed nothing the month is billed by opens no interval of its own, and an
// oracle that split one there would report the engine for a fold that is right.
func TestBuildOracleKeepsAFactThatRestatesTheOpenInterval(t *testing.T) {
	from := july2026
	to := from.AddDate(0, 1, 0)
	const owner = "p1"

	// Two size objects of equal content rather than one shared map, because
	// what the fold reads is what the two objects hold and not that they are
	// the same object.
	size := volumeSizeOf(&volume{sizeGB: 50, volumeType: "ssd"})
	same := volumeSizeOf(&volume{sizeGB: 50, volumeType: "ssd"})
	facts := []fact{
		{
			at: from.Add(time.Hour), eventType: "volume.retype", resourceID: "vol-1",
			projectID: owner, workload: workloadClassic, effect: alive(stateAvailable, size),
		},
		{
			at: from.Add(5 * time.Hour), eventType: "volume.retype", resourceID: "vol-1",
			projectID: owner, workload: workloadClassic, effect: alive(stateAvailable, same),
		},
	}

	oracle, err := buildOracle(facts, 1, testCloud, from, to)
	if err != nil {
		t.Fatalf("buildOracle() error = %v, want nil", err)
	}

	vol := resourceNamed(t, oracle, "vol-1")
	want := []OracleInterval{
		{From: from.Add(time.Hour), To: to, State: stateAvailable, ProjectID: owner, Size: size},
	}
	if len(vol.Intervals) != len(want) {
		t.Fatalf("the volume is billed over %d intervals, want %d: %+v", len(vol.Intervals), len(want), vol.Intervals)
	}
	if !reflect.DeepEqual(vol.Intervals[0], want[0]) {
		t.Errorf("the volume's interval = %+v, want %+v", vol.Intervals[0], want[0])
	}
}

// TestBuildOracleRefusesTwoFactsAtOneInstant holds the two ledgers the fold
// refuses rather than guesses at. Both are the generator's own mistakes, and a
// fold that papered over them would state a month nobody generated.
func TestBuildOracleRefusesTwoFactsAtOneInstant(t *testing.T) {
	from := july2026
	to := from.AddDate(0, 1, 0)

	t.Run("two transitions of one resource at one instant", func(t *testing.T) {
		at := from.Add(6 * time.Hour)
		size := volumeSizeOf(&volume{sizeGB: 10, volumeType: "ssd"})
		facts := []fact{
			{
				at: at, eventType: "volume.create.end", resourceID: "vol-1", projectID: "p1",
				workload: workloadClassic, effect: alive(stateAvailable, size),
			},
			{
				at: at, eventType: "volume.retype", resourceID: "vol-1", projectID: "p1",
				workload: workloadClassic, effect: alive(stateAvailable, size),
			},
		}

		_, err := buildOracle(facts, 1, testCloud, from, to)
		want := fmt.Sprintf("volume vol-1 reports two billable transitions at %s, which the projection "+
			"cannot order", at.UTC().Format(time.RFC3339))
		if err == nil || err.Error() != want {
			t.Fatalf("buildOracle() error = %v, want %q", err, want)
		}
	})

	t.Run("a type the ledger does not name", func(t *testing.T) {
		facts := []fact{{
			at: from.Add(time.Hour), eventType: "compute.instance.reboot.end", resourceID: "srv-1",
			projectID: "p1", workload: workloadClassic,
			effect: alive(stateActive, instanceSizeOf(flavors[0])),
		}}

		_, err := buildOracle(facts, 1, testCloud, from, to)
		want := "the oracle knows no resource type for compute.instance.reboot.end"
		if err == nil || err.Error() != want {
			t.Fatalf("buildOracle() error = %v, want %q", err, want)
		}
	})
}

// TestOracleFormatCoversTheGeneratorsBookedSurface holds oracleFormat against
// what the generator books. The number is what refuses an oracle an earlier
// build wrote, and nothing else in the build notices a row added to
// billableTypes, a member added to a size object or a member added to the
// document while the number stays where it is: the file written before the
// change keeps every member it had, so neither DisallowUnknownFields nor
// validate has anything to say about it, and an oracle folded before the change
// is compared against an export of the month after it. Every resource the
// change touched then reads as a difference the engine did not cause.
//
// The surface below is what the current format covers, and it is raised
// together with oracleFormat. It states what the generator books, what a size
// holds, what the document states and the states a resource is booked under,
// not everything the document turns on: which transitions a month schedules is
// beyond a list of names, and what the collector makes of them is what
// TestOracleAgreesWithTheMapping holds these rows against.
func TestOracleFormatCoversTheGeneratorsBookedSurface(t *testing.T) {
	// surface is the format the lists below are stated for. A change to any of
	// them raises this number and oracleFormat with it.
	const surface = 1

	if oracleFormat != surface {
		t.Fatalf("oracleFormat = %d and the surface below is stated for format %d, want the two "+
			"raised together", oracleFormat, surface)
	}

	t.Run("what the generator books", func(t *testing.T) {
		want := []string{
			"compute.instance.create.end -> instance/compute.instance.create.end",
			"compute.instance.delete.end -> instance/compute.instance.delete.end",
			"compute.instance.finish_resize.end -> instance/compute.instance.resize.end",
			"compute.instance.power_off.end -> instance/compute.instance.power_off",
			"compute.instance.power_on.end -> instance/compute.instance.power_on",
			"compute.instance.resize.end -> instance/compute.instance.resize.end",
			"compute.instance.shelve_offload.end -> instance/compute.instance.shelve",
			"compute.instance.unshelve.end -> instance/compute.instance.unshelve",
			"floatingip.create.end -> floating_ip/floatingip.create.end",
			"floatingip.delete.end -> floating_ip/floatingip.delete.end",
			"image.delete -> image/image.delete",
			"image.upload -> image/image.create",
			"octavia.loadbalancer.create.end -> loadbalancer/octavia.loadbalancer.create.end",
			"octavia.loadbalancer.delete.end -> loadbalancer/octavia.loadbalancer.delete.end",
			"octavia.loadbalancer.update.end -> loadbalancer/octavia.loadbalancer.update.end",
			"volume.create.end -> volume/volume.create.end",
			"volume.delete.end -> volume/volume.delete.end",
			"volume.resize.end -> volume/volume.resize.end",
			"volume.retype -> volume/volume.retype",
			"volume.transfer.accept.end -> volume/volume.transfer.accept.end",
		}

		booked := make([]string, 0, len(billableTypes))
		for oslo, billable := range billableTypes {
			booked = append(booked, fmt.Sprintf("%s -> %s/%s", oslo, billable.resourceType, billable.eventType))
		}
		slices.Sort(booked)

		if !slices.Equal(booked, want) {
			t.Errorf("the generator books\n\t%s\nwant\n\t%s", strings.Join(booked, "\n\t"),
				strings.Join(want, "\n\t"))
		}
	})

	t.Run("what a size holds", func(t *testing.T) {
		for _, tc := range []struct {
			resourceType string
			size         map[string]any
			want         []string
		}{
			{
				resourceType: "instance", size: instanceSizeOf(flavors[0]),
				want: []string{"disk_gb", "flavor", "ram_gb", "vcpus"},
			},
			{resourceType: "volume", size: volumeSizeOf(&volume{}), want: []string{"size_gb", "type"}},
			{resourceType: "image", size: imageSizeOf(&image{}), want: []string{"size_gb"}},
			{resourceType: "floating_ip", size: floatingIPSizeOf(), want: []string{"ip_version"}},
			{
				resourceType: "loadbalancer", size: loadBalancerSizeOf(1, 1),
				want: []string{"listeners", "pools"},
			},
		} {
			t.Run(tc.resourceType, func(t *testing.T) {
				if held := slices.Sorted(maps.Keys(tc.size)); !slices.Equal(held, tc.want) {
					t.Errorf("a %s size holds %v, want %v", tc.resourceType, held, tc.want)
				}
			})
		}
	})

	// The members of the document are stated here because nothing else in the
	// build notices one added. DisallowUnknownFields only refuses a document
	// that holds a member this build does not read, never one that lacks a
	// member this build gained, so a new member decodes to its zero value out of
	// an oracle an earlier build wrote and every resource of the month reads as
	// a difference the engine did not cause.
	t.Run("what the document states", func(t *testing.T) {
		for _, tc := range []struct {
			named string
			typ   reflect.Type
			want  []string
		}{
			{
				named: "Oracle", typ: reflect.TypeFor[Oracle](),
				want: []string{"format", "cloud", "seed", "period_from", "period_to", "resources", "counts"},
			},
			{
				named: "OracleResource", typ: reflect.TypeFor[OracleResource](),
				want: []string{"resource_type", "resource_id", "workload", "intervals"},
			},
			{
				named: "OracleInterval", typ: reflect.TypeFor[OracleInterval](),
				want: []string{"from", "to", "state", "project_id", "size"},
			},
			{
				named: "OracleCount", typ: reflect.TypeFor[OracleCount](),
				want: []string{"project_id", "event_type", "count"},
			},
		} {
			t.Run(tc.named, func(t *testing.T) {
				stated := make([]string, 0, tc.typ.NumField())
				for i := range tc.typ.NumField() {
					stated = append(stated, strings.Split(tc.typ.Field(i).Tag.Get("json"), ",")[0])
				}
				if !slices.Equal(stated, tc.want) {
					t.Errorf("%s states %v, want %v", tc.named, stated, tc.want)
				}
			})
		}
	})

	// The states are what the document's state member holds, and a comparison
	// reads them verbatim. A renamed one leaves a document of this format saying
	// something else than it did, and every interval of an oracle written before
	// the rename reads as a difference.
	t.Run("the states a resource is booked under", func(t *testing.T) {
		want := []string{"active", "available", "in-use", "resized", "shelved", "shutoff"}

		booked := []string{stateActive, stateAvailable, stateInUse, stateResized, stateShelved, stateShutoff}
		slices.Sort(booked)

		if !slices.Equal(booked, want) {
			t.Errorf("a resource is booked under %v, want %v", booked, want)
		}
	})
}

// emptyOracleDocument is an oracle of a month that states no resource, written
// out by hand because buildOracle only folds one from a ledger the generator
// never produces.
const emptyOracleDocument = `{"format":1,"cloud":"os-test","seed":1,` +
	`"period_from":"2026-07-01T00:00:00Z",` +
	`"period_to":"2026-08-01T00:00:00Z","resources":[],"counts":[]}`

// completeOracleDocument is the smallest document ReadOracle accepts, and
// oracleIntervalDocument the one interval it holds. The cases that hold the
// read against a document another build wrote cut one member out of it, so each
// of them differs from an accepted file in the one member it is about.
const oracleIntervalDocument = `{"from":"2026-07-01T00:00:00Z","to":"2026-07-02T00:00:00Z",` +
	`"state":"available","project_id":"p1","size":{"size_gb":50}}`

const completeOracleDocument = `{"format":1,"cloud":"os-test","seed":1,` +
	`"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z",` +
	`"resources":[{"resource_type":"volume","resource_id":"v","workload":"classic",` +
	`"intervals":[` + oracleIntervalDocument + `]}],"counts":[]}`

// generatedOracle is the oracle of one generated month, or a failed test.
func generatedOracle(t *testing.T, seed uint64) Oracle {
	t.Helper()

	month, err := GenerateMonth(seed, july2026, july2026.AddDate(0, 1, 0), testCloud, Faults{})
	if err != nil {
		t.Fatalf("GenerateMonth(%d, %s, %q) error = %v, want nil", seed,
			july2026.Format(time.RFC3339), testCloud, err)
	}
	return month.Oracle
}

// writeOracleFile writes a document to an oracle file of its own and returns
// its path. It is how the tests build the files ReadOracle has to refuse, which
// WriteOracle cannot produce.
func writeOracleFile(t *testing.T, document string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "oracle.json")
	if err := os.WriteFile(path, []byte(document), streamFileMode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// TestOracleRoundTrips writes the oracle of a generated month out and reads it
// back. The file is what a comparison works from, and the numbers are what it
// turns on: a size that came back as a float64 would be held against the digits
// the engine read from the same notification and lose them.
func TestOracleRoundTrips(t *testing.T) {
	oracle := generatedOracle(t, 1)
	path := filepath.Join(t.TempDir(), "oracle.json")

	if err := WriteOracle(path, oracle); err != nil {
		t.Fatalf("WriteOracle() error = %v, want nil", err)
	}
	read, err := ReadOracle(path)
	if err != nil {
		t.Fatalf("ReadOracle() error = %v, want nil", err)
	}

	if !reflect.DeepEqual(read, oracle) {
		t.Errorf("ReadOracle() = %+v, want %+v", read, oracle)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	text := string(content)
	if !strings.HasPrefix(text, "{") {
		t.Errorf("%s starts with %q, want the object the oracle renders to", path,
			text[:min(len(text), 20)])
	}
	if !strings.HasSuffix(text, "\n") {
		t.Errorf("%s ends with %q, want a trailing newline", path, text[max(len(text)-20, 0):])
	}
}

func TestWriteOracleReportsAnUnwritablePath(t *testing.T) {
	// A --out whose directory was never created is what an operator hits, and
	// the error names the file rather than the syscall that refused it.
	path := filepath.Join(t.TempDir(), "absent", "oracle.json")

	err := WriteOracle(path, generatedOracle(t, 1))

	prefix := "writing " + path + ": "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("WriteOracle() error = %v, want it to start with %q", err, prefix)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s error = %v, want the failed write to have left no file behind", path, err)
	}
}

func TestReadOracleReportsAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oracle.json")

	_, err := ReadOracle(path)

	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadOracle() error = %v, want it to report a missing file", err)
	}
	prefix := "reading " + path + ": "
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("ReadOracle() error = %q, want it to start with %q", err, prefix)
	}
}

// TestReadOracleRefusesAnEmptyOracle holds the read against a document that
// parses and states nothing. A comparison handed one would report every
// resource of the month as one the engine invented.
func TestReadOracleRefusesAnEmptyOracle(t *testing.T) {
	path := writeOracleFile(t, emptyOracleDocument)

	_, err := ReadOracle(path)

	want := path + " holds no resources"
	if err == nil || err.Error() != want {
		t.Fatalf("ReadOracle() error = %v, want %q", err, want)
	}
}

// TestReadOracleRefusesWhatIsNotAnOracle holds the files the read refuses
// before it hands anything to a comparison: bytes that are no document at all,
// an oracle another build wrote that states more than this one reads or was
// written to another format, and one that leaves a member this build reads
// unstated. The last of them is the one a decoder says nothing about, because
// JSON leaves an absent member at its zero value: a document without a size
// would be compared, and every time gauge dimension of every priced resource
// would come out as a difference the engine did not cause.
func TestReadOracleRefusesWhatIsNotAnOracle(t *testing.T) {
	t.Run("the document the cases below cut a member out of", func(t *testing.T) {
		path := writeOracleFile(t, completeOracleDocument)

		if _, err := ReadOracle(path); err != nil {
			t.Fatalf("ReadOracle() error = %v, want nil for a complete document", err)
		}
	})

	t.Run("an oracle written to another format", func(t *testing.T) {
		path := writeOracleFile(t, strings.Replace(completeOracleDocument, `"format":1`, `"format":2`, 1))

		_, err := ReadOracle(path)

		want := fmt.Sprintf("%s states format 2 and this build writes format %d", path, oracleFormat)
		if err == nil || err.Error() != want {
			t.Fatalf("ReadOracle() error = %v, want %q", err, want)
		}
	})

	for _, tc := range []struct {
		name string
		cut  string
		want string
	}{
		{name: "a document without a cloud", cut: `"cloud":"os-test",`, want: "names no cloud"},
		{
			name: "a document without a period",
			cut:  `"period_from":"2026-07-01T00:00:00Z",`,
			want: "names no period",
		},
		{
			name: "a resource without an id",
			cut:  `"resource_id":"v",`,
			want: "holds a resource without a type or an id",
		},
		{
			name: "a resource without an interval",
			cut:  oracleIntervalDocument,
			want: "states no interval for volume v",
		},
		{
			name: "an interval without bounds",
			cut:  `"from":"2026-07-01T00:00:00Z",`,
			want: "states an interval of volume v without bounds",
		},
		{
			name: "an interval without a state",
			cut:  `"state":"available",`,
			want: "states no state for volume v from 2026-07-01T00:00:00Z",
		},
		{
			name: "an interval without a project",
			cut:  `"project_id":"p1",`,
			want: "states no project for volume v from 2026-07-01T00:00:00Z",
		},
		{
			name: "an interval without a size",
			cut:  `,"size":{"size_gb":50}`,
			want: "states no size for volume v from 2026-07-01T00:00:00Z",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document := strings.Replace(completeOracleDocument, tc.cut, "", 1)
			if document == completeOracleDocument {
				t.Fatalf("the document holds no %q to cut, want the case to name a member of it", tc.cut)
			}
			path := writeOracleFile(t, document)

			_, err := ReadOracle(path)

			want := path + " " + tc.want
			if err == nil || err.Error() != want {
				t.Fatalf("ReadOracle() error = %v, want %q", err, want)
			}
		})
	}

	t.Run("a file that is not a document", func(t *testing.T) {
		path := writeOracleFile(t, "not json")

		_, err := ReadOracle(path)

		prefix := "reading " + path + ": "
		if err == nil || !strings.HasPrefix(err.Error(), prefix) {
			t.Fatalf("ReadOracle() error = %v, want it to start with %q", err, prefix)
		}
	})

	t.Run("a resource that states a member this build does not read", func(t *testing.T) {
		path := writeOracleFile(t, `{"cloud":"os-test","seed":1,`+
			`"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z",`+
			`"resources":[{"resource_type":"volume","resource_id":"v","workload":"classic",`+
			`"faults":[],"intervals":[]}],"counts":[]}`)

		_, err := ReadOracle(path)

		if err == nil || !strings.Contains(err.Error(), "faults") {
			t.Fatalf("ReadOracle() error = %v, want it to name the unknown member faults", err)
		}
	})
}

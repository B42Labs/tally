package simulator

import (
	"bytes"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/providers/openstack"
)

// unknownSwitch is the whole error a name outside FaultNames is refused with.
// It lists the six, because a mistyped switch is answered with the ones that
// exist rather than with the name that does not.
const unknownSwitch = `unknown fault switch "bogus"; the switches are ` +
	`pre-existing, missing-create, duplicates, reordering, refused-shapes, held-back`

func TestParseFaults(t *testing.T) {
	cases := []struct {
		name  string
		names []string
		// want is the parsed set and wantNames what it names itself by. Both are
		// read only where wantErr is empty.
		want      Faults
		wantNames []string
		wantErr   string
	}{
		{
			name:      "no switch at all",
			names:     nil,
			want:      Faults{},
			wantNames: []string{},
		},
		{
			name:      "an empty list of switches",
			names:     []string{},
			want:      Faults{},
			wantNames: []string{},
		},
		{
			name:      "one switch on its own",
			names:     []string{FaultDuplicates},
			want:      Faults{Duplicates: true},
			wantNames: []string{FaultDuplicates},
		},
		{
			// Every switch but missing-create, which excludes pre-existing. They
			// come in reverse and one of them twice, which is what the order of
			// Names and the set behind it are read from.
			name: "every switch that stands beside the others, in reverse and one twice",
			names: []string{
				FaultHeldBack, FaultRefusedShapes, FaultReordering,
				FaultDuplicates, FaultPreExisting, FaultHeldBack,
			},
			want: Faults{
				PreExisting: true, Duplicates: true,
				Reordering: true, RefusedShapes: true, HeldBack: true,
			},
			wantNames: []string{
				FaultPreExisting, FaultDuplicates, FaultReordering,
				FaultRefusedShapes, FaultHeldBack,
			},
		},
		{
			name:    "a switch nobody named that way",
			names:   []string{"bogus"},
			wantErr: unknownSwitch,
		},
		{
			name:    "an unknown switch beside a known one",
			names:   []string{FaultDuplicates, "bogus"},
			wantErr: unknownSwitch,
		},
		{
			name:    "the two switches that exclude each other",
			names:   []string{FaultPreExisting, FaultMissingCreate},
			wantErr: "pre-existing and missing-create exclude each other",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ParseFaults(c.names)

			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseFaults(%q) error = nil, want %q", c.names, c.wantErr)
				}
				if err.Error() != c.wantErr {
					t.Errorf("ParseFaults(%q) error = %q, want %q", c.names, err, c.wantErr)
				}
				if got != (Faults{}) {
					t.Errorf("ParseFaults(%q) = %+v, want the zero Faults beside the error", c.names, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseFaults(%q) error = %v, want nil", c.names, err)
			}
			if got != c.want {
				t.Errorf("ParseFaults(%q) = %+v, want %+v", c.names, got, c.want)
			}
			if names := got.Names(); !slices.Equal(names, c.wantNames) {
				t.Errorf("ParseFaults(%q).Names() = %v, want %v", c.names, names, c.wantNames)
			}
		})
	}
}

// TestFaultsNamesRunInSwitchOrder holds the set with every switch on to
// FaultNames. It is built here rather than parsed, because ParseFaults never
// returns it: pre-existing and missing-create exclude each other.
func TestFaultsNamesRunInSwitchOrder(t *testing.T) {
	all := Faults{
		PreExisting: true, MissingCreate: true, Duplicates: true,
		Reordering: true, RefusedShapes: true, HeldBack: true,
	}
	if got := all.Names(); !slices.Equal(got, FaultNames) {
		t.Errorf("Names() of every switch = %v, want %v", got, FaultNames)
	}
}

// TestFaultsNamesIsNeverNil holds both name lists to the empty slice. They are
// rendered into the oracle, where a nil slice becomes null and an empty one the
// [] whoever reads a month expects.
func TestFaultsNamesIsNeverNil(t *testing.T) {
	if names := (Faults{}).Names(); names == nil || len(names) != 0 {
		t.Errorf("Faults{}.Names() = %#v, want an empty non-nil slice", names)
	}
	if names := (touchedResources{}).names(resourceKey{}); names == nil || len(names) != 0 {
		t.Errorf("touchedResources{}.names(resourceKey{}) = %#v, want an empty non-nil slice", names)
	}
}

func TestTouchedResourcesNameTheSwitchesInOrder(t *testing.T) {
	instanceKey := resourceKey{resourceType: "instance", resourceID: "i-1"}
	volumeKey := resourceKey{resourceType: "volume", resourceID: "v-1"}

	touched := make(touchedResources)
	// Out of FaultNames order and with one switch added twice, which is what the
	// order of the answer and the set behind it are read from.
	touched.add(instanceKey, FaultHeldBack)
	touched.add(instanceKey, FaultPreExisting)
	touched.add(instanceKey, FaultHeldBack)

	want := []string{FaultPreExisting, FaultHeldBack}
	if got := touched.names(instanceKey); !slices.Equal(got, want) {
		t.Errorf("names(%+v) = %v, want %v", instanceKey, got, want)
	}
	if got := touched.names(volumeKey); got == nil || len(got) != 0 {
		t.Errorf("names(%+v) = %#v, want an empty non-nil slice for a resource nothing touched", volumeKey, got)
	}

	g := &generator{touched: make(touchedResources)}
	g.touch(volumeKey.resourceType, volumeKey.resourceID, FaultDuplicates)
	if got := g.touched.names(volumeKey); !slices.Equal(got, []string{FaultDuplicates}) {
		t.Errorf("touch(%q, %q, %q) recorded %v under %+v, want %v",
			volumeKey.resourceType, volumeKey.resourceID, FaultDuplicates, got, volumeKey, []string{FaultDuplicates})
	}
	if got := g.touched.names(instanceKey); len(got) != 0 {
		t.Errorf("touch recorded %v under %+v, want it under %+v alone", got, instanceKey, volumeKey)
	}
}

// TestSortedFaultNames holds the order the switches are printed in. An oracle
// read from disk states them in the order the document spells them, and one an
// older build wrote may name a switch this build knows nothing about.
func TestSortedFaultNames(t *testing.T) {
	reversed := slices.Clone(FaultNames)
	slices.Reverse(reversed)

	for _, tc := range []struct {
		name  string
		names []string
		want  []string
	}{
		{name: "no switch at all", names: []string{}, want: []string{}},
		{
			name:  "the switches in the order they are named in",
			names: []string{FaultPreExisting, FaultDuplicates, FaultHeldBack},
			want:  []string{FaultPreExisting, FaultDuplicates, FaultHeldBack},
		},
		{name: "the switches in reverse", names: reversed, want: FaultNames},
		{
			name:  "a name this build does not know",
			names: []string{"bogus", FaultHeldBack, FaultPreExisting},
			want:  []string{FaultPreExisting, FaultHeldBack, "bogus"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			given := slices.Clone(tc.names)

			if got := sortedFaultNames(given); !slices.Equal(got, tc.want) {
				t.Errorf("sortedFaultNames(%v) = %v, want %v", tc.names, got, tc.want)
			}
			if !slices.Equal(given, tc.names) {
				t.Errorf("sortedFaultNames(%v) left it as %v, want the list it was given untouched",
					tc.names, given)
			}
		})
	}
}

// TestFaultStreamsAreSeededByTheirName holds a switch's stream to the seed and
// the name together. Two switches drawing one sequence would mean which
// resources one of them touches follows from the other being on beside it.
func TestFaultStreamsAreSeededByTheirName(t *testing.T) {
	first := draws(faultStream(1, FaultPreExisting))

	if again := draws(faultStream(1, FaultPreExisting)); again != first {
		t.Errorf("faultStream(1, %q) drew %v and then %v, want the same draws twice",
			FaultPreExisting, first, again)
	}
	if other := draws(faultStream(1, FaultDuplicates)); other == first {
		t.Errorf("faultStream(1, %q) drew %v, want another sequence than %q's",
			FaultDuplicates, other, FaultPreExisting)
	}
	if other := draws(faultStream(2, FaultPreExisting)); other == first {
		t.Errorf("faultStream(2, %q) drew %v, want another sequence than seed 1's",
			FaultPreExisting, other)
	}
}

// draws takes the first four values of a stream, which is enough to tell two
// generators apart.
func draws(stream *rand.Rand) [4]uint64 {
	var drawn [4]uint64
	for i := range drawn {
		drawn[i] = stream.Uint64()
	}
	return drawn
}

// messageIDs returns the message ids of a schedule in the order they stand in
// it. The stream passes move, repeat and add notifications, and the id is what
// tells one of them from another.
func messageIDs(schedule Schedule) []string {
	ids := make([]string, 0, len(schedule))
	for _, transition := range schedule {
		ids = append(ids, transition.MessageID)
	}
	return ids
}

// renderedBodies renders every transition of a schedule or fails the test. Two
// schedules that render the same bytes are the same month on the wire, down to
// their message ids.
func renderedBodies(t *testing.T, schedule Schedule) []string {
	t.Helper()

	bodies := make([]string, 0, len(schedule))
	for _, transition := range schedule {
		bodies = append(bodies, string(render(t, transition)))
	}
	return bodies
}

// writeEvents writes the events a schedule expects and hands the file back, or
// fails the test. It is what the switches that only change the stream are held
// to: the file states what the collector has to record, and a switch may not
// move it.
func writeEvents(t *testing.T, schedule Schedule) []byte {
	t.Helper()

	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := WriteEvents(path, testCloud, schedule); err != nil {
		t.Fatalf("WriteEvents(%q, %q) error = %v, want nil", path, testCloud, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

// transitionKey lines the transitions of two months up: the type of a
// notification together with the resource it is about.
type transitionKey struct {
	eventType  string
	resourceID string
}

// transitionCounts counts a schedule's transitions by type and resource, which
// is what two months hold in common when one of them moved a share of its
// instants.
func transitionCounts(schedule Schedule) map[transitionKey]int {
	counts := make(map[transitionKey]int, len(schedule))
	for _, transition := range schedule {
		counts[transitionKey{eventType: transition.EventType, resourceID: transition.ResourceID}]++
	}
	return counts
}

// billableInstants returns, per type and resource, the instants of the billable
// transitions in schedule order.
func billableInstants(schedule Schedule) map[transitionKey][]time.Time {
	instants := make(map[transitionKey][]time.Time)
	for _, transition := range schedule {
		if !transition.Billable {
			continue
		}
		key := transitionKey{eventType: transition.EventType, resourceID: transition.ResourceID}
		instants[key] = append(instants[key], transition.At)
	}
	return instants
}

// billableIDsByResource groups the message ids of a schedule's billable
// transitions by their resource, in schedule order.
func billableIDsByResource(schedule Schedule) map[string][]string {
	byResource := make(map[string][]string)
	for _, transition := range schedule {
		if transition.Billable {
			byResource[transition.ResourceID] = append(byResource[transition.ResourceID], transition.MessageID)
		}
	}
	return byResource
}

// firstPositions returns where the first transition of each message id stands,
// which is where a stream pass applies its pick.
func firstPositions(schedule Schedule) map[string]int {
	positions := make(map[string]int, len(schedule))
	for i, transition := range schedule {
		if _, ok := positions[transition.MessageID]; !ok {
			positions[transition.MessageID] = i
		}
	}
	return positions
}

// duplicatedIDs returns the message ids a stream carries more than once.
func duplicatedIDs(stream Schedule) map[string]bool {
	seen := make(map[string]bool, len(stream))
	duplicated := make(map[string]bool)
	for _, transition := range stream {
		if seen[transition.MessageID] {
			duplicated[transition.MessageID] = true
			continue
		}
		seen[transition.MessageID] = true
	}
	return duplicated
}

// twinnedIDs returns the message ids of the transitions a refused twin follows.
// A twin is told by its own message id, which is drawn when it is placed and is
// therefore in no schedule.
func twinnedIDs(month Month) map[string]bool {
	scheduled := make(map[string]bool, len(month.Schedule))
	for _, transition := range month.Schedule {
		scheduled[transition.MessageID] = true
	}

	twinned := make(map[string]bool)
	for i, transition := range month.Stream {
		if i > 0 && !scheduled[transition.MessageID] {
			twinned[month.Stream[i-1].MessageID] = true
		}
	}
	return twinned
}

// reorderedResources returns the resources the oracle names the reordering
// switch on, sorted, so a failure reports them in one order.
func reorderedResources(oracle Oracle) []string {
	var ids []string
	for key, resource := range touchedResourcesOf(oracle) {
		if slices.Contains(resource.Faults, FaultReordering) {
			ids = append(ids, key.resourceID)
		}
	}
	slices.Sort(ids)
	return ids
}

// touchedInstances returns the servers the oracle names a switch on, sorted.
func touchedInstances(oracle Oracle) []string {
	var ids []string
	for key := range touchedResourcesOf(oracle) {
		if key.resourceType == "instance" {
			ids = append(ids, key.resourceID)
		}
	}
	slices.Sort(ids)
	return ids
}

// sortedIDs returns a schedule's message ids as a multiset, which is what two
// sequences of one month hold in common when one of them was reordered.
func sortedIDs(schedule Schedule) []string {
	ids := messageIDs(schedule)
	slices.Sort(ids)
	return ids
}

// TestEverySwitchOffLeavesTheMonthAsItWas covers the property the four stream
// switches are held to: a switch changes what the bus carries and never what
// the simulated cloud did. The schedule of a month with one of them on renders
// the very bytes the schedule of the month without it renders, so the oracle
// and the expected events of the two months are one.
//
// pre-existing is the switch that does move the schedule, because what it
// changes is when a server was created. It is held to its transitions instead:
// the month holds the same notifications about the same resources, and every
// resource the oracle names no switch on keeps its instants.
func TestEverySwitchOffLeavesTheMonthAsItWas(t *testing.T) {
	plain := faultyMonth(t, 1, Faults{})
	plainBodies := renderedBodies(t, plain.Schedule)

	if got, want := renderedBodies(t, plain.Stream), plainBodies; !slices.Equal(got, want) {
		t.Errorf("a month with no switch on publishes %d notifications and its schedule holds %d, "+
			"want the stream to be the schedule", len(got), len(want))
	}
	if len(plain.Held) != 0 {
		t.Errorf("a month with no switch on holds %d transitions back, want none", len(plain.Held))
	}
	if plain.Oracle.Faults == nil || len(plain.Oracle.Faults) != 0 {
		t.Errorf("Oracle.Faults = %#v, want an empty non-nil list", plain.Oracle.Faults)
	}
	for _, resource := range plain.Oracle.Resources {
		if resource.Faults == nil || len(resource.Faults) != 0 {
			t.Errorf("%s %s reports Faults = %#v, want an empty non-nil list",
				resource.ResourceType, resource.ResourceID, resource.Faults)
		}
	}

	for _, faults := range []Faults{
		{Duplicates: true}, {Reordering: true}, {RefusedShapes: true}, {HeldBack: true},
	} {
		t.Run(strings.Join(faults.Names(), " "), func(t *testing.T) {
			month := faultyMonth(t, 1, faults)

			if got := renderedBodies(t, month.Schedule); !slices.Equal(got, plainBodies) {
				t.Errorf("the schedule of a month with %v on renders other bytes than the plain month's, "+
					"want the switch to change the stream alone", faults.Names())
			}
		})
	}

	t.Run(FaultPreExisting, func(t *testing.T) {
		month := faultyMonth(t, 1, Faults{PreExisting: true})

		if got, want := transitionCounts(month.Schedule), transitionCounts(plain.Schedule); !maps.Equal(got, want) {
			t.Errorf("the month holds %d kinds of transition and the plain month %d, want the switch to "+
				"move what it moves and to publish the same notifications about the same resources",
				len(got), len(want))
		}

		untouched := make(map[string]bool)
		for _, resource := range month.Oracle.Resources {
			if len(resource.Faults) == 0 {
				untouched[resource.ResourceID] = true
			}
		}
		want := billableInstants(plain.Schedule)
		for key, instants := range billableInstants(month.Schedule) {
			if !untouched[key.resourceID] {
				continue
			}
			if !slices.EqualFunc(instants, want[key], time.Time.Equal) {
				t.Errorf("%s of %s is at %v and was at %v, want a resource the oracle names no switch on "+
					"to keep its instants", key.eventType, key.resourceID, instants, want[key])
			}
		}
	})
}

// TestHeldBackHoldsAShareOfTheBillableMonth covers the switch that keeps a
// share of the month off the bus. What it holds is the month's own
// notifications rather than copies of them, so the stream and the held
// transitions together are the schedule, and the schedule is what the run still
// expects the collector to record: the run releases them before it ends.
func TestHeldBackHoldsAShareOfTheBillableMonth(t *testing.T) {
	month := faultyMonth(t, 1, Faults{HeldBack: true})
	billable := month.Schedule.Billable()

	if len(month.Held) == 0 {
		t.Fatalf("the switch held nothing back of %d billable transitions, want one in %d of them",
			len(billable), heldBackShare)
	}
	for i, transition := range month.Held {
		if !transition.Billable {
			t.Errorf("the switch holds %s of %s back, want the billable transitions alone",
				transition.EventType, transition.ResourceID)
		}
		if i > 0 && transition.At.Before(month.Held[i-1].At) {
			t.Errorf("held transition %d is at %s behind one at %s, want them in instant order",
				i, transition.At.Format(time.RFC3339), month.Held[i-1].At.Format(time.RFC3339))
		}
	}

	published := make(map[string]bool, len(month.Stream))
	for _, transition := range month.Stream {
		published[transition.MessageID] = true
	}
	for _, transition := range month.Held {
		if published[transition.MessageID] {
			t.Errorf("the bus carries %s, which the switch holds back, want the two disjoint",
				transition.MessageID)
		}
	}

	got := sortedIDs(append(slices.Clone(month.Stream), month.Held...))
	if want := sortedIDs(month.Schedule); !slices.Equal(got, want) {
		t.Errorf("the stream and the held transitions hold %d notifications and the schedule %d, "+
			"want the two together to be the schedule", len(got), len(want))
	}

	touched := touchedResourcesOf(month.Oracle)
	for _, transition := range month.Held {
		key := billableKey(transition)
		if resource, ok := touched[key]; !ok || !slices.Contains(resource.Faults, FaultHeldBack) {
			t.Errorf("%s of %s %s is held back and the oracle names %v on the resource, want it to name %q",
				transition.EventType, key.resourceType, key.resourceID, resource.Faults, FaultHeldBack)
		}
	}

	// The events the run expects are the whole billable month, held transitions
	// and all: what is held back is published before the run ends.
	if got, want := bytes.Count(writeEvents(t, month.Schedule), []byte("\n")), len(billable); got != want {
		t.Errorf("the events of the month hold %d lines, want the %d billable transitions of the schedule",
			got, want)
	}
}

// TestReorderingSwapsTheFirstTwoTransitionsOfAResource covers the switch that
// publishes the first billable transition of a resource behind its second. The
// instants do not move, so what the collector receives disagrees with the
// timestamps it receives it under, which is the arrival order the projection
// and the engine have to undo.
func TestReorderingSwapsTheFirstTwoTransitionsOfAResource(t *testing.T) {
	plain := faultyMonth(t, 1, Faults{})
	month := faultyMonth(t, 1, Faults{Reordering: true})

	reordered := reorderedResources(month.Oracle)
	if len(reordered) == 0 {
		t.Fatalf("the switch moved nothing, want it to move one in %d of the resources with two "+
			"billable transitions", reorderShare)
	}

	byResource := billableIDsByResource(month.Schedule)
	inStream := firstPositions(month.Stream)
	inSchedule := firstPositions(month.Schedule)
	moved := make(map[string]bool, len(reordered))
	for _, resourceID := range reordered {
		ids := byResource[resourceID]
		if len(ids) < 2 {
			t.Errorf("the switch moved %s, which has %d billable transitions, want a resource with two",
				resourceID, len(ids))
			continue
		}
		first, second := ids[0], ids[1]
		moved[first] = true

		if inSchedule[first] > inSchedule[second] {
			t.Errorf("%s stands at %d in the schedule of %s and its second transition at %d, "+
				"want the schedule to say what the cloud did", first, inSchedule[first], resourceID,
				inSchedule[second])
		}
		if inStream[first] != inStream[second]+1 {
			t.Errorf("%s stands at %d in the stream of %s and its second transition at %d, "+
				"want the first published directly behind the second", first, inStream[first], resourceID,
				inStream[second])
		}
	}

	// What is left once the moved transitions are taken out is the order the
	// cloud produced: the switch moves them and nothing else.
	if got, want := withoutIDs(month.Stream, moved), withoutIDs(month.Schedule, moved); !slices.Equal(got, want) {
		t.Error("the stream without the moved transitions is not the schedule without them, " +
			"want the switch to move the picked firsts alone")
	}
	if got, want := sortedIDs(month.Stream), sortedIDs(month.Schedule); !slices.Equal(got, want) {
		t.Errorf("the stream carries %d notifications and the schedule holds %d, "+
			"want the switch to move them rather than to add or drop one", len(got), len(want))
	}

	if len(month.Schedule) != len(plain.Schedule) {
		t.Fatalf("the month holds %d transitions and the plain month %d, want the switch to leave "+
			"the schedule as it is", len(month.Schedule), len(plain.Schedule))
	}
	for i := range month.Schedule {
		if !month.Schedule[i].At.Equal(plain.Schedule[i].At) {
			t.Errorf("transition %d is at %s and was at %s, want the switch to move no instant",
				i, month.Schedule[i].At.Format(time.RFC3339), plain.Schedule[i].At.Format(time.RFC3339))
		}
	}
}

// withoutIDs returns the message ids of a schedule with the named ones left
// out, in the order they stand in.
func withoutIDs(schedule Schedule, without map[string]bool) []string {
	var ids []string
	for _, transition := range schedule {
		if !without[transition.MessageID] {
			ids = append(ids, transition.MessageID)
		}
	}
	return ids
}

// TestDuplicatesRepeatATransitionTenLinesLater covers the switch that publishes
// a notification twice. The copy carries the message id of the original, which
// is what the Reporting API deduplicates on, and the schedule knows nothing
// about it: what the collector has to record is the month without the
// redelivery.
func TestDuplicatesRepeatATransitionTenLinesLater(t *testing.T) {
	plain := faultyMonth(t, 1, Faults{})
	month := faultyMonth(t, 1, Faults{Duplicates: true})
	bodies := renderedBodies(t, month.Stream)

	// The input of the pass is the schedule, so the transitions of the stream
	// that are not repetitions are counted to hold a copy to its distance.
	original := make(map[string]int, len(month.Stream))
	firstBody := make(map[string]string, len(month.Stream))
	inputs, duplicates := 0, 0
	for i, transition := range month.Stream {
		at, repeated := original[transition.MessageID]
		if !repeated {
			original[transition.MessageID] = inputs
			firstBody[transition.MessageID] = bodies[i]
			inputs++
			continue
		}

		duplicates++
		if bodies[i] != firstBody[transition.MessageID] {
			t.Errorf("the copy of %s renders other bytes than the notification it repeats, "+
				"want the two byte for byte the same", transition.MessageID)
		}
		if behind := inputs - at - 1; behind != duplicateDistance && inputs != len(month.Schedule) {
			t.Errorf("the copy of %s stands %d notifications behind it, want %d or the end of the month",
				transition.MessageID, behind, duplicateDistance)
		}
	}

	if duplicates == 0 {
		t.Fatalf("the switch repeated nothing of %d billable transitions, want one in %d of them",
			len(month.Schedule.Billable()), duplicateShare)
	}
	if got, want := len(month.Stream), len(month.Schedule)+duplicates; got != want {
		t.Errorf("the stream carries %d notifications and the schedule holds %d beside %d copies, want %d",
			got, len(month.Schedule), duplicates, want)
	}

	if got := renderedBodies(t, month.Schedule); !slices.Equal(got, renderedBodies(t, plain.Schedule)) {
		t.Error("the schedule of the month renders other bytes than the plain month's, " +
			"want the switch to change the stream alone")
	}
	if !slices.Equal(month.Oracle.Counts, plain.Oracle.Counts) {
		t.Error("the oracle counts other events than the plain month's, " +
			"want a redelivery to be no second event")
	}
	if !bytes.Equal(writeEvents(t, month.Schedule), writeEvents(t, plain.Schedule)) {
		t.Error("the events of the month are not the plain month's, want a redelivery to be no second event")
	}
}

// TestRefusedShapesTwinTheirOriginals covers the switch that puts a
// notification the collector refuses behind a notification it books. The three
// shapes are refused for three reasons, and none of them adds an event: the
// oversized and the truncated twin are unparseable, one past the body bound and
// one half a document, and the versioned twin parses and is skipped, because
// the mapping table claims nothing for a versioned type.
func TestRefusedShapesTwinTheirOriginals(t *testing.T) {
	plain := faultyMonth(t, 1, Faults{})
	month := faultyMonth(t, 1, Faults{RefusedShapes: true})

	// A twin is told by its message id and not by being unbillable: the plain
	// month carries unbillable notifications of its own, the image.create that
	// precedes an upload among them.
	scheduled := make(map[string]bool, len(month.Schedule))
	for _, transition := range month.Schedule {
		scheduled[transition.MessageID] = true
	}

	shapes := make(map[string]int)
	for i, twin := range month.Stream {
		if scheduled[twin.MessageID] {
			continue
		}
		if i == 0 {
			t.Fatalf("the stream opens with %s, which is in no schedule, want a twin behind the "+
				"notification it doubles", twin.EventType)
		}

		of := month.Stream[i-1]
		if !twin.At.Equal(of.At) || twin.ResourceID != of.ResourceID || twin.Exchange != of.Exchange {
			t.Errorf("the twin %s of %s at %s on %s follows %s of %s at %s on %s, want it to follow "+
				"the notification it doubles", twin.EventType, twin.ResourceID,
				twin.At.Format(time.RFC3339), twin.Exchange, of.EventType, of.ResourceID,
				of.At.Format(time.RFC3339), of.Exchange)
			continue
		}
		if twin.Billable {
			t.Errorf("the twin %s of %s is marked billable, want the collector to record nothing for it",
				twin.EventType, twin.ResourceID)
		}

		body := render(t, twin)
		switch {
		case strings.HasPrefix(twin.EventType, "instance."):
			shapes["versioned"]++
			if _, ok := openstack.MapNotification(parse(t, body), testCloud); ok {
				t.Errorf("the mapping claims an event for the versioned %s, want it skipped", twin.EventType)
			}
		case twin.truncated:
			shapes["truncated"]++
			if _, err := openstack.ParseEnvelope(body); err == nil {
				t.Errorf("the collector parses the truncated twin of %s, want it refused", of.EventType)
			}
		default:
			// oversizedPadding is the collector's own body bound, which is what
			// the twin has to stand past.
			shapes["oversized"]++
			if len(body) <= oversizedPadding {
				t.Errorf("the oversized twin of %s renders %d bytes, want more than the %d the collector reads",
					of.EventType, len(body), oversizedPadding)
			}
		}
	}

	for _, shape := range []string{"versioned", "truncated", "oversized"} {
		if shapes[shape] == 0 {
			t.Errorf("the month carries no %s twin, want one in %d billable transitions of every shape",
				shape, refusedShapeDraw)
		}
	}

	if got := renderedBodies(t, month.Schedule); !slices.Equal(got, renderedBodies(t, plain.Schedule)) {
		t.Error("the schedule of the month renders other bytes than the plain month's, " +
			"want the switch to change the stream alone")
	}

	// The versioned names stand beside the ones the plain month carries, and the
	// collector admits openstack.LabelValueLimit distinct event_type values
	// before it folds the rest into event_type="other".
	types := make(map[string]struct{}, len(month.Stream))
	for _, transition := range month.Stream {
		types[transition.EventType] = struct{}{}
	}
	if len(types) >= openstack.LabelValueLimit {
		t.Errorf("the stream renders %d distinct event types and the collector admits %d, "+
			"want fewer than the bound", len(types), openstack.LabelValueLimit)
	}
}

// TestVersionedTwinOfAFinishResize covers the twin nova would have published
// had it been left on the versioned notification format
// docs/openstack-collector.md refuses. The type is renamed, the payload moves
// under nova_object.data, and the server is named uuid there rather than
// instance_id.
func TestVersionedTwinOfAFinishResize(t *testing.T) {
	const (
		serverID = "8f7e6d5c-4b3a-4291-8071-6f5e4d3c2b1a"
		twinID   = "1b2c3d4e-5f60-4718-8293-a4b5c6d7e8f9"
	)
	original := Transition{
		At:          july2026.Add(time.Hour),
		EventType:   "compute.instance.finish_resize.end",
		Exchange:    novaExchange,
		Billable:    true,
		PublisherID: "compute.compute-01",
		ResourceID:  serverID,
		Payload:     map[string]any{"instance_id": serverID, "state": "active"},
	}

	twin := versionedTwin(original, twinID)

	if want := "instance.resize_finish.end"; twin.EventType != want {
		t.Errorf("versionedTwin(%q).EventType = %q, want %q", original.EventType, twin.EventType, want)
	}
	if want := "nova-compute:"; !strings.HasPrefix(twin.PublisherID, want) {
		t.Errorf("versionedTwin(%q).PublisherID = %q, want it to start with %q",
			original.EventType, twin.PublisherID, want)
	}
	if twin.MessageID != twinID || twin.Billable {
		t.Errorf("versionedTwin() = message id %q, billable %t, want %q and false",
			twin.MessageID, twin.Billable, twinID)
	}

	payload := parse(t, render(t, twin)).Payload
	data, ok := payload["nova_object.data"].(map[string]any)
	if !ok {
		t.Fatalf("the rendered payload = %v, want a nova_object.data object", payload)
	}
	if data["uuid"] != serverID {
		t.Errorf("nova_object.data holds uuid = %v, want %q", data["uuid"], serverID)
	}
	if _, ok := data["instance_id"]; ok {
		t.Errorf("nova_object.data holds instance_id = %v, want the versioned format's uuid alone",
			data["instance_id"])
	}
	if original.Payload["instance_id"] != serverID {
		t.Errorf("the payload of the original holds instance_id = %v, want the twin to leave it at %q",
			original.Payload["instance_id"], serverID)
	}

	cases := []struct {
		oslo      string
		versioned string
	}{
		{"compute.instance.create.end", "instance.create.end"},
		{"compute.instance.delete.end", "instance.delete.end"},
		{"compute.instance.resize.end", "instance.resize.end"},
		{"compute.instance.finish_resize.end", "instance.resize_finish.end"},
		{"compute.instance.shelve_offload.end", "instance.shelve_offload.end"},
		{"compute.instance.unshelve.end", "instance.unshelve.end"},
		{"compute.instance.power_off.end", "instance.power_off.end"},
		{"compute.instance.power_on.end", "instance.power_on.end"},
	}
	covered := make(map[string]bool, len(cases))
	for _, c := range cases {
		covered[c.oslo] = true
		t.Run(c.oslo, func(t *testing.T) {
			if got := versionedType(c.oslo); got != c.versioned {
				t.Errorf("versionedType(%q) = %q, want %q", c.oslo, got, c.versioned)
			}
		})
	}
	// The table covers every nova type the oracle books, so a type added there
	// is named here as well.
	for eventType := range billableTypes {
		if exchangeFor(eventType) == novaExchange && !covered[eventType] {
			t.Errorf("the table names no versioned type for %s, want every nova type the oracle books",
				eventType)
		}
	}
}

// TestStreamFaultPicksAreIndependentOfEachOther covers the rule every switch
// draws by: each of them walks the schedule with the stream of its own name, so
// what it picks follows from the seed and the switch alone. Held-back is the
// switch the others are held against, because it is the one that takes a
// transition away: a pick whose transition is held simply never applies, and
// every other pick stands where it stood.
func TestStreamFaultPicksAreIndependentOfEachOther(t *testing.T) {
	held := faultyMonth(t, 1, Faults{HeldBack: true})
	holding := make(map[string]bool, len(held.Held))
	for _, transition := range held.Held {
		holding[transition.MessageID] = true
	}

	t.Run(FaultDuplicates, func(t *testing.T) {
		requireSamePicks(t, "the copy of",
			duplicatedIDs(faultyMonth(t, 1, Faults{Duplicates: true}).Stream),
			duplicatedIDs(faultyMonth(t, 1, Faults{Duplicates: true, HeldBack: true}).Stream),
			holding)
	})

	t.Run(FaultRefusedShapes, func(t *testing.T) {
		requireSamePicks(t, "the twin of",
			twinnedIDs(faultyMonth(t, 1, Faults{RefusedShapes: true})),
			twinnedIDs(faultyMonth(t, 1, Faults{RefusedShapes: true, HeldBack: true})),
			holding)
	})

	t.Run(FaultReordering, func(t *testing.T) {
		alone := faultyMonth(t, 1, Faults{Reordering: true})
		beside := faultyMonth(t, 1, Faults{Reordering: true, HeldBack: true})
		byResource := billableIDsByResource(alone.Schedule)

		moved := make(map[string]bool)
		for _, resourceID := range reorderedResources(beside.Oracle) {
			moved[resourceID] = true
		}
		for _, resourceID := range reorderedResources(alone.Oracle) {
			ids := byResource[resourceID]
			if len(ids) < 2 {
				t.Errorf("the switch moved %s, which has %d billable transitions, want two", resourceID, len(ids))
				continue
			}
			// A pair one of whose members is held is no reordering: the switch
			// leaves it where it stands rather than moving a transition behind one
			// that is not on the bus.
			want := !holding[ids[0]] && !holding[ids[1]]
			if moved[resourceID] != want {
				t.Errorf("reordering moved %s beside held-back = %t, want %t: held-back holds its "+
					"first transition = %t and its second = %t", resourceID, moved[resourceID], want,
					holding[ids[0]], holding[ids[1]])
			}
			delete(moved, resourceID)
		}
		for resourceID := range moved {
			t.Errorf("reordering moved %s beside held-back and not on its own, want the picks of one "+
				"switch to follow from the seed and the switch alone", resourceID)
		}
	})

	t.Run("pre-existing and missing-create pick the same servers", func(t *testing.T) {
		pre := touchedInstances(faultyMonth(t, 1, Faults{PreExisting: true}).Oracle)
		missing := touchedInstances(faultyMonth(t, 1, Faults{MissingCreate: true}).Oracle)

		if len(pre) == 0 {
			t.Fatalf("pre-existing touched no server, want one in %d of the classic tenants'", preExistingShare)
		}
		if !slices.Equal(pre, missing) {
			t.Errorf("pre-existing touched %v and missing-create %v, want the two switches to pick the "+
				"same servers from the one stream they share", pre, missing)
		}
	})
}

// requireSamePicks holds the picks a switch made beside held-back against the
// ones it made on its own: a pick whose transition is held never applies, and
// every other one stands.
func requireSamePicks(t *testing.T, what string, alone, beside, holding map[string]bool) {
	t.Helper()

	for id := range alone {
		if want := !holding[id]; beside[id] != want {
			t.Errorf("%s %s beside held-back = %t, want %t: held-back holds it = %t",
				what, id, beside[id], want, holding[id])
		}
	}
	for id := range beside {
		if !alone[id] {
			t.Errorf("%s %s stands beside held-back and not on its own, want the picks of one switch "+
				"to follow from the seed and the switch alone", what, id)
		}
	}
}

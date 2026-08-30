package simulator

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/providers/openstack"
)

// The months the workload tests generate. July and May are both 31 days long,
// so a comparison between them holds every instant at the same offset, while
// June is a day shorter and moves the ones anchored on the month's end.
var (
	july2026 = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	june2026 = time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	may2026  = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
)

// testCloud is the cloud the generated months are salted with.
const testCloud = "os-test"

// collectorTopic is the topic the collector binds its queue with out of the
// box: the default of TALLY_OSC_TOPICS in
// internal/providers/openstack/config.go. A simulator that published under
// another one would produce a month no collector receives. The exchanges it
// publishes on are ServiceExchanges.
const collectorTopic = "notifications.info"

// generateMonth generates one month or fails the test. Every test takes its
// schedule through it, so a generation error is reported once and in one form.
func generateMonth(t *testing.T, seed uint64, from time.Time, cloud string) Schedule {
	t.Helper()

	month, err := GenerateMonth(seed, from, from.AddDate(0, 1, 0), cloud, Faults{})
	if err != nil {
		t.Fatalf("GenerateMonth(%d, %s, %q) error = %v, want nil", seed, from.Format(time.RFC3339), cloud, err)
	}
	return month.Schedule
}

// faultyMonth generates July 2026 over testCloud with the switches on, or fails
// the test. It hands back the whole month, because a switch is read off the
// oracle as much as off the schedule.
func faultyMonth(t *testing.T, seed uint64, faults Faults) Month {
	t.Helper()

	month, err := GenerateMonth(seed, july2026, july2026.AddDate(0, 1, 0), testCloud, faults)
	if err != nil {
		t.Fatalf("GenerateMonth(%d, %s, %q, %v) error = %v, want nil", seed,
			july2026.Format(time.RFC3339), testCloud, faults.Names(), err)
	}
	return month
}

// touchedResourcesOf returns the resources the oracle names a switch on, keyed
// the way the oracle keys one.
func touchedResourcesOf(oracle Oracle) map[resourceKey]OracleResource {
	touched := make(map[resourceKey]OracleResource)
	for _, resource := range oracle.Resources {
		if len(resource.Faults) == 0 {
			continue
		}
		touched[resourceKey{resourceType: resource.ResourceType, resourceID: resource.ResourceID}] = resource
	}
	return touched
}

// requireDisjoint fails the test when the two schedules share a value of the
// field, which is what salting the identifiers is supposed to rule out.
func requireDisjoint(t *testing.T, field string, first, second Schedule, of func(Transition) string) {
	t.Helper()

	seen := make(map[string]struct{}, len(first))
	for _, transition := range first {
		seen[of(transition)] = struct{}{}
	}
	for _, transition := range second {
		if _, ok := seen[of(transition)]; ok {
			t.Errorf("both schedules carry the %s %q, want them disjoint", field, of(transition))
			return
		}
	}
}

// ofWorkload returns the transitions one workload emitted, in schedule order.
// The workloads of a month are told apart by nothing else: their tenants are
// ids, and their notifications are the ones every other tenant sends.
func ofWorkload(schedule Schedule, workload string) Schedule {
	of := schedule[:0:0]
	for _, transition := range schedule {
		if transition.Workload == workload {
			of = append(of, transition)
		}
	}
	return of
}

// buildMonth generates July 2026 over testCloud the way Generate does and
// returns the generator that built it, so a test reads the world behind the
// schedule as well as the schedule. The message ids are not drawn, which is the
// one thing Generate does that these tests do not read.
func buildMonth(t *testing.T, seed uint64) *generator {
	t.Helper()

	shape := rand.New(rand.NewPCG(seed, shapeStream))
	identifiers := idReader{src: rand.New(rand.NewPCG(seed, identifierSalt(testCloud, july2026)))}

	g := newGenerator(shape, identifiers, noiseIdentifiers(seed, testCloud, july2026),
		july2026, july2026.AddDate(0, 1, 0), testCloud)
	g.run()
	slices.SortStableFunc(g.schedule, func(a, b Transition) int { return a.At.Compare(b.At) })
	return g
}

// shootNamed returns the shoot of that name, whichever project holds it.
func shootNamed(t *testing.T, g *generator, name string) *shoot {
	t.Helper()

	for _, gp := range g.gardenerProjects {
		for _, s := range gp.shoots {
			if s.name == name {
				return s
			}
		}
	}
	t.Fatalf("the month holds no shoot named %q, want the three the workload creates", name)
	return nil
}

// transitionsOf returns the billable transitions of one shoot, in schedule
// order. A transition names no shoot, so every type is matched by what carries
// the shoot's name: the display_name of a server or a volume, the name of a
// load balancer, and, for an address, the balancer that holds it.
func transitionsOf(schedule Schedule, s *shoot) []Transition {
	var of []Transition
	for _, transition := range ofWorkload(schedule, workloadGardener) {
		// The life of a shoot is read off its billable transitions alone. The
		// noise around its steps, the audits, the .start halves, the ports and
		// the attach and detach, is held by noise_test.go, and a daily
		// compute.instance.exists of a worker that is gone would otherwise be
		// the last transition of batch.
		if !transition.Billable {
			continue
		}
		switch {
		case strings.HasPrefix(transition.EventType, "compute."),
			strings.HasPrefix(transition.EventType, "volume."):
			if name, _ := transition.Payload["display_name"].(string); !strings.HasPrefix(name, s.technicalID+"-") {
				continue
			}
		case strings.HasPrefix(transition.EventType, "octavia."):
			if name, _ := transition.Payload["name"].(string); !strings.HasPrefix(name, "kube_service_"+s.technicalID+"_") {
				continue
			}
		case strings.HasPrefix(transition.EventType, "floatingip."):
			if !slices.ContainsFunc(s.loadBalancers, func(lb *loadBalancer) bool {
				return lb.fip.id == transition.ResourceID
			}) {
				continue
			}
		default:
			continue
		}
		of = append(of, transition)
	}
	return of
}

// idsIn lists the resource ids of the transitions of one type inside [lo, hi),
// in schedule order.
func idsIn(transitions []Transition, eventType string, lo, hi time.Time) []string {
	var ids []string
	for _, transition := range transitions {
		if transition.EventType != eventType || transition.At.Before(lo) || !transition.At.Before(hi) {
			continue
		}
		ids = append(ids, transition.ResourceID)
	}
	return ids
}

// instantOf returns when one resource reported an event type, and the zero
// instant when it never did.
func instantOf(transitions []Transition, eventType, resourceID string) time.Time {
	for _, transition := range transitions {
		if transition.EventType == eventType && transition.ResourceID == resourceID {
			return transition.At
		}
	}
	return time.Time{}
}

// window returns the first and the last instant of the transitions match picks,
// and false when it picks none.
func window(transitions []Transition, match func(Transition) bool) (first, last time.Time, ok bool) {
	for _, transition := range transitions {
		if !match(transition) {
			continue
		}
		if !ok {
			first, ok = transition.At, true
		}
		last = transition.At
	}
	return first, last, ok
}

// isClaimDelete reports whether the transition releases a persistent volume
// claim rather than the root volume of a worker, which carries its worker's
// name.
func isClaimDelete(transition Transition) bool {
	name, _ := transition.Payload["display_name"].(string)
	return transition.EventType == "volume.delete.end" && strings.Contains(name, "-dynamic-pvc-")
}

func TestGenerateIsDeterministic(t *testing.T) {
	first := generateMonth(t, 7, july2026, testCloud)
	second := generateMonth(t, 7, july2026, testCloud)

	if len(first) != len(second) {
		t.Fatalf("the same seed produced %d and %d transitions, want the same month twice",
			len(first), len(second))
	}
	for i := range first {
		before, after := render(t, first[i]), render(t, second[i])
		if !bytes.Equal(before, after) {
			t.Fatalf("transition %d differs between two runs of the same seed:\n%s\n%s", i, before, after)
		}
	}
}

func TestGenerateSaltsIdentifiersWithPeriodAndCloud(t *testing.T) {
	t.Run("another cloud keeps the shape and renames everything", func(t *testing.T) {
		first := generateMonth(t, 7, july2026, "os-a")
		second := generateMonth(t, 7, july2026, "os-b")

		if len(first) != len(second) {
			t.Fatalf("two clouds produced %d and %d transitions, want the shape to be the seed's alone",
				len(first), len(second))
		}
		for i := range first {
			if first[i].EventType != second[i].EventType {
				t.Errorf("transition %d is %s on one cloud and %s on the other, want the same shape",
					i, first[i].EventType, second[i].EventType)
			}
			if !first[i].At.Equal(second[i].At) {
				t.Errorf("transition %d is at %s on one cloud and %s on the other, want the same shape",
					i, first[i].At, second[i].At)
			}
			if len(first[i].Payload) != len(second[i].Payload) {
				t.Errorf("transition %d carries %d payload members on one cloud and %d on the other",
					i, len(first[i].Payload), len(second[i].Payload))
			}
		}
		requireDisjoint(t, "message id", first, second, func(tr Transition) string { return tr.MessageID })
		requireDisjoint(t, "resource id", first, second, func(tr Transition) string { return tr.ResourceID })
		requireDisjoint(t, "project id", first, second, func(tr Transition) string { return tr.ProjectID })
	})

	t.Run("another month of the same length keeps every offset", func(t *testing.T) {
		// The offsets are compared over the classic tenants alone. The profile the
		// machine-driven workloads are drawn on follows the real calendar of the
		// period, so a shoot and a pipeline move their activity onto the working
		// days of the month they run in, while a classic tenant, which is anchored
		// on the month start alone, keeps every offset it has (author decision of
		// 2026-08-29).
		july := generateMonth(t, 7, july2026, testCloud)
		may := generateMonth(t, 7, may2026, testCloud)

		classicJuly, classicMay := ofWorkload(july, workloadClassic), ofWorkload(may, workloadClassic)
		if len(classicJuly) != len(classicMay) {
			t.Fatalf("two 31 day months produced %d and %d classic transitions, want the same shape",
				len(classicJuly), len(classicMay))
		}
		for i := range classicJuly {
			if classicJuly[i].EventType != classicMay[i].EventType {
				t.Errorf("transition %d is %s in July and %s in May, want the same shape",
					i, classicJuly[i].EventType, classicMay[i].EventType)
			}
			inJuly, inMay := classicJuly[i].At.Sub(july2026), classicMay[i].At.Sub(may2026)
			if inJuly != inMay {
				t.Errorf("transition %d sits %s into July and %s into May, want the same offset",
					i, inJuly, inMay)
			}
		}

		// A month that moved its machine-driven workloads still holds them.
		for _, workload := range []string{workloadGardener, workloadCI} {
			for name, schedule := range map[string]Schedule{"July": july, "May": may} {
				if len(ofWorkload(schedule, workload)) == 0 {
					t.Errorf("%s holds no %s transition, want every workload in every month",
						name, workload)
				}
			}
		}
		requireDisjoint(t, "message id", july, may, func(tr Transition) string { return tr.MessageID })
	})

	t.Run("a shorter month renames everything as well", func(t *testing.T) {
		// June is a day shorter than July, so the transitions anchored on the end
		// of the month sit at other offsets and the offsets are not compared.
		july := generateMonth(t, 7, july2026, testCloud)
		june := generateMonth(t, 7, june2026, testCloud)

		requireDisjoint(t, "message id", july, june, func(tr Transition) string { return tr.MessageID })
		requireDisjoint(t, "resource id", july, june, func(tr Transition) string { return tr.ResourceID })
	})
}

func TestGenerateStaysInsideThePeriod(t *testing.T) {
	from := july2026
	to := from.AddDate(0, 1, 0)
	schedule := generateMonth(t, 1, from, testCloud)

	if routingKey != collectorTopic {
		t.Errorf("routingKey = %q, want %q, the topic the collector binds by default",
			routingKey, collectorTopic)
	}

	last := make(map[string]time.Time, len(schedule))
	for i, transition := range schedule {
		if transition.At.Before(from) || !transition.At.Before(to) {
			t.Errorf("transition %d (%s) is at %s, want it inside [%s, %s)",
				i, transition.EventType, transition.At, from, to)
		}
		if i > 0 && transition.At.Before(schedule[i-1].At) {
			t.Errorf("transition %d is at %s after %s, want the schedule sorted",
				i, transition.At, schedule[i-1].At)
		}
		if !slices.Contains(ServiceExchanges, transition.Exchange) {
			t.Errorf("%s is published on %q, want one of the exchanges the simulator declares: %v",
				transition.EventType, transition.Exchange, ServiceExchanges)
		}
		if previous, ok := last[transition.ResourceID]; ok && transition.At.Sub(previous) < time.Second {
			t.Errorf("resource %s reports %s at %s, %s after its previous transition, want at least a second",
				transition.ResourceID, transition.EventType, transition.At, transition.At.Sub(previous))
		}
		last[transition.ResourceID] = transition.At
	}

	// What is billable is read off the collector's mapping and not off a list
	// of type names: a transition is billable when the mapping records an event
	// for it, and the noise of the catalogue is what it records nothing for.
	var want []Transition
	for _, transition := range schedule {
		if _, ok := openstack.MapNotification(parse(t, render(t, transition)), testCloud); ok {
			want = append(want, transition)
		}
	}
	got := schedule.Billable()
	if len(got) != len(want) {
		t.Fatalf("Billable() returned %d of %d transitions, want the %d the collector's mapping records an event for",
			len(got), len(schedule), len(want))
	}
	for i := range got {
		if got[i].MessageID != want[i].MessageID {
			t.Errorf("Billable()[%d] is %s, want %s", i, got[i].MessageID, want[i].MessageID)
		}
	}
}

// TestPreExistingInstancesStartBeforeTheMonth holds what the pre-existing
// switch does to a month: a share of the classic tenants' servers, with the
// volumes and the address that belong to them, is created before the month
// begins and reports every transition of that history. The month still bills
// them from its first instant, because the oracle clips an interval that starts
// before the month to the month.
func TestPreExistingInstancesStartBeforeTheMonth(t *testing.T) {
	month := faultyMonth(t, 1, Faults{PreExisting: true})
	touched := touchedResourcesOf(month.Oracle)

	before := 0
	for _, transition := range month.Schedule {
		if !transition.At.Before(july2026) {
			continue
		}
		before++
		if transition.Workload != workloadClassic {
			t.Errorf("%s of a %s resource is at %s, want the switch to work on the classic tenants alone",
				transition.EventType, transition.Workload, transition.At.Format(time.RFC3339))
		}
		if !transition.Billable {
			continue
		}
		billable, ok := billableTypes[transition.EventType]
		if !ok {
			t.Fatalf("the oracle knows no resource type for the billable %s", transition.EventType)
		}
		key := resourceKey{resourceType: billable.resourceType, resourceID: transition.ResourceID}
		if resource, ok := touched[key]; !ok || !slices.Contains(resource.Faults, FaultPreExisting) {
			t.Errorf("%s %s reports %s at %s, and the oracle names no switch on it",
				key.resourceType, key.resourceID, transition.EventType,
				transition.At.Format(time.RFC3339))
		}
	}
	if before == 0 {
		t.Fatalf("no transition of the month lies before %s, want the switch to move a share of "+
			"the classic servers behind it", july2026.Format(time.RFC3339))
	}

	created := make(map[string]time.Time)
	for _, transition := range month.Schedule {
		if transition.EventType == "compute.instance.create.end" {
			created[transition.ResourceID] = transition.At
		}
	}

	for key, resource := range touched {
		switch key.resourceType {
		case "instance", "volume", "floating_ip":
		default:
			t.Errorf("the switch touched the %s %s, want the servers of the classic tenants with "+
				"their volumes and their addresses", key.resourceType, key.resourceID)
		}
		if resource.Workload != workloadClassic {
			t.Errorf("the switch touched %s %s of the %s workload, want the classic one",
				key.resourceType, key.resourceID, resource.Workload)
		}
		for i, interval := range resource.Intervals {
			if interval.From.Before(july2026) {
				t.Errorf("%s %s interval %d starts at %s, want the clip to hold it at %s",
					key.resourceType, key.resourceID, i, interval.From.Format(time.RFC3339),
					july2026.Format(time.RFC3339))
			}
		}
		if from := resource.Intervals[0].From; !from.Equal(july2026) {
			t.Errorf("%s %s is billed from %s, want it billed from the month's first instant %s",
				key.resourceType, key.resourceID, from.Format(time.RFC3339),
				july2026.Format(time.RFC3339))
		}
		if key.resourceType != "instance" {
			continue
		}
		at, ok := created[key.resourceID]
		if !ok {
			t.Errorf("instance %s reports no create, want the whole history of a server the month "+
				"inherited", key.resourceID)
			continue
		}
		if at.Before(july2026.Add(-preExistingLeadMax)) || at.After(july2026.Add(-preExistingLeadMin)) {
			t.Errorf("instance %s was created at %s, want it inside [%s, %s]", key.resourceID,
				at.Format(time.RFC3339), july2026.Add(-preExistingLeadMax).Format(time.RFC3339),
				july2026.Add(-preExistingLeadMin).Format(time.RFC3339))
		}
	}
}

// TestMissingCreateDropsWhatLiesBeforeTheMonth holds the other half of the
// pair: missing-create picks the very same servers and then keeps every
// transition of theirs that happened before the month off the bus. The oracle
// still states them, because what the bus carried is not what the cloud did,
// and the daily audits still report them, because the audits are a pass over
// the schedule that ran before the drop.
func TestMissingCreateDropsWhatLiesBeforeTheMonth(t *testing.T) {
	to := july2026.AddDate(0, 1, 0)
	month := faultyMonth(t, 1, Faults{MissingCreate: true})
	touched := touchedResourcesOf(month.Oracle)

	for _, transition := range month.Schedule {
		if transition.At.Before(july2026) {
			t.Errorf("%s of %s is at %s, want every transition the bus carries inside the month",
				transition.EventType, transition.ResourceID, transition.At.Format(time.RFC3339))
		}
	}

	t.Run("the two switches pick the same resources", func(t *testing.T) {
		want := touchedResourcesOf(faultyMonth(t, 1, Faults{PreExisting: true}).Oracle)

		if len(touched) != len(want) {
			t.Fatalf("missing-create touched %d resources and pre-existing %d, want the same ones "+
				"for one seed", len(touched), len(want))
		}
		for key, resource := range want {
			if !slices.Equal(resource.Faults, []string{FaultPreExisting}) {
				t.Errorf("pre-existing names %v on %s %s, want %v alone", resource.Faults,
					key.resourceType, key.resourceID, []string{FaultPreExisting})
			}
			got, ok := touched[key]
			if !ok {
				t.Errorf("pre-existing touched %s %s and missing-create touched it not",
					key.resourceType, key.resourceID)
				continue
			}
			if !slices.Equal(got.Faults, []string{FaultMissingCreate}) {
				t.Errorf("missing-create names %v on %s %s, want %v alone", got.Faults,
					key.resourceType, key.resourceID, []string{FaultMissingCreate})
			}
		}
	})

	t.Run("no touched resource reports its create", func(t *testing.T) {
		for _, transition := range month.Schedule {
			if !strings.HasSuffix(transition.EventType, ".create.end") {
				continue
			}
			for key := range touched {
				if key.resourceID == transition.ResourceID {
					t.Errorf("%s %s reports %s at %s, want its create off the bus", key.resourceType,
						key.resourceID, transition.EventType, transition.At.Format(time.RFC3339))
				}
			}
		}
	})

	t.Run("the oracle states every touched resource from the month's first instant", func(t *testing.T) {
		for key, resource := range touched {
			if from := resource.Intervals[0].From; !from.Equal(july2026) {
				t.Errorf("%s %s is billed from %s, want it billed from %s", key.resourceType,
					key.resourceID, from.Format(time.RFC3339), july2026.Format(time.RFC3339))
			}
		}
	})

	t.Run("the counts state the creates the collector receives", func(t *testing.T) {
		// A create that never reached the bus is a create the collector records
		// no event for, so it counts under nothing. The project of a touched
		// server is read off the oracle, since its create is gone.
		missing := make(map[string]int)
		classic := make(map[string]bool)
		for _, resource := range month.Oracle.Resources {
			if resource.ResourceType != "instance" || resource.Workload != workloadClassic {
				continue
			}
			project := resource.Intervals[0].ProjectID
			classic[project] = true
			if _, ok := touched[resourceKey{resourceType: "instance", resourceID: resource.ResourceID}]; ok {
				missing[project]++
			}
		}
		if len(classic) != projectCount {
			t.Fatalf("the oracle states classic servers in %d projects, want %d", len(classic), projectCount)
		}

		counts := make(map[countKey]int, len(month.Oracle.Counts))
		for _, count := range month.Oracle.Counts {
			counts[countKey{projectID: count.ProjectID, eventType: count.EventType}] = count.Count
		}
		for project := range classic {
			key := countKey{projectID: project, eventType: "compute.instance.create.end"}
			want := instancesPerProject - missing[project]
			if got := counts[key]; got != want {
				t.Errorf("project %s is expected to record %d server creates, want the %d of its %d "+
					"servers whose create the bus carried", project, got, want, instancesPerProject)
			}
		}
	})

	t.Run("the audits report a touched server on every day of the month", func(t *testing.T) {
		deleted := make(map[string]time.Time)
		for _, transition := range month.Schedule {
			if transition.EventType == "compute.instance.delete.end" {
				deleted[transition.ResourceID] = transition.At
			}
		}

		// An audit sits at a midnight or, when the server already reports a
		// transition in that second, whole seconds after it, so the audits are
		// counted per day and not per instant. The existence notification a
		// delete sequence carries (noise.go) is none of the daily audits and is
		// left out of the count.
		audited := make(map[string]map[time.Time]int)
		for _, transition := range month.Schedule {
			if transition.EventType != "compute.instance.exists" {
				continue
			}
			if at, ok := deleted[transition.ResourceID]; ok &&
				transition.At.Equal(at.Add(-instanceDeleteLead+time.Second)) {
				continue
			}
			if audited[transition.ResourceID] == nil {
				audited[transition.ResourceID] = make(map[time.Time]int)
			}
			audited[transition.ResourceID][transition.At.UTC().Truncate(day)]++
		}
		// The audit pass reports at every midnight of the month a transition
		// stands at or after, and a deleted server is reported one last time at
		// the midnight that follows its delete.
		last := month.Schedule[len(month.Schedule)-1].At

		for key := range touched {
			if key.resourceType != "instance" {
				continue
			}
			var want []time.Time
			for midnight := july2026.Add(day); midnight.Before(to) && !midnight.After(last); midnight = midnight.Add(day) {
				if at, ok := deleted[key.resourceID]; ok && !midnight.Before(at.Add(day)) {
					break
				}
				want = append(want, midnight)
			}

			if len(audited[key.resourceID]) != len(want) {
				t.Errorf("instance %s is audited on %d days, want %d", key.resourceID,
					len(audited[key.resourceID]), len(want))
			}
			for _, midnight := range want {
				if got := audited[key.resourceID][midnight]; got != 1 {
					t.Errorf("instance %s reports %d audits on %s, want one", key.resourceID, got,
						midnight.Format(time.RFC3339))
				}
			}
		}
	})
}

func TestGenerateRefusesANonMonth(t *testing.T) {
	cases := []struct {
		name string
		from time.Time
		to   time.Time
	}{
		{
			name: "a period that starts mid-month",
			from: time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC),
			to:   time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "a period that is not the whole month",
			from: july2026,
			to:   july2026.AddDate(0, 0, 20),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := GenerateMonth(1, c.from, c.to, testCloud, Faults{})
			if err == nil {
				t.Fatalf("GenerateMonth(1, %s, %s, %q) error = nil, want a refusal",
					c.from.Format(time.RFC3339), c.to.Format(time.RFC3339), testCloud)
			}
			if !strings.HasSuffix(err.Error(), " is not a UTC month") {
				t.Errorf("GenerateMonth() error = %q, want it to end with %q", err, " is not a UTC month")
			}
		})
	}
}

// sizeOf maps one transition the way the collector does and returns the size
// the booked event carries, together with the notification it was read from.
func sizeOf(t *testing.T, transition Transition) (map[string]any, openstack.Notification) {
	t.Helper()

	notification := parse(t, render(t, transition))
	mapped, ok := openstack.MapNotification(notification, testCloud)
	if !ok {
		t.Fatalf("MapNotification(%s) mapped = false, want the collector to book it", transition.EventType)
	}
	return mapped.Payload.Size, notification
}

// TestShootsFollowTheirLives holds each of the three shoots against the life it
// stands for: one that runs the whole month and rolls its workers once, one
// that hibernates every night, and one that is created and torn down inside the
// month. A shoot that scaled up without scaling back, woke up without
// hibernating, or reported a resource after its tear-down would put usage into
// the month that no cluster ever had.
func TestShootsFollowTheirLives(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			g := buildMonth(t, seed)

			t.Run("api-prod keeps its pool and replaces every worker of it", func(t *testing.T) {
				s := shootNamed(t, g, "api-prod")
				transitions := transitionsOf(g.schedule, s)

				if len(s.workers) != s.baseWorkers {
					t.Errorf("api-prod ends the month with %d workers, want the %d of its pool: "+
						"what the autoscaler adds in the morning it gives back the same evening",
						len(s.workers), s.baseWorkers)
				}

				created := make(map[string]int)
				creates, deletes := 0, 0
				for _, transition := range transitions {
					switch transition.EventType {
					case "compute.instance.create.end":
						creates++
						created[transition.ResourceID]++
					case "compute.instance.delete.end":
						deletes++
					}
				}
				if creates-deletes != s.baseWorkers {
					t.Errorf("api-prod creates %d workers and deletes %d, want %d more created than "+
						"deleted: the pool the month ends with is the one it started with",
						creates, deletes, s.baseWorkers)
				}
				if deletes < s.baseWorkers {
					t.Errorf("api-prod deletes %d workers, want at least the %d of its pool: the "+
						"rolling update replaces every one of them", deletes, s.baseWorkers)
				}
				for id, count := range created {
					if count > 1 {
						t.Errorf("the instance %s is created %d times, want every worker to carry an id "+
							"no earlier worker had: a resource that comes back reopens a closed record",
							id, count)
					}
				}

				// A worker that boots from an image names the one its tenant
				// uploaded, which is the mirror of the empty image_ref_url api-dev's
				// workers carry.
				want := fmt.Sprintf("http://glance.%s.example:9292/images/%s",
					testCloud, s.owner.tenant.images[0].id)
				for _, transition := range transitions {
					if transition.EventType != "compute.instance.create.end" {
						continue
					}
					if url, _ := transition.Payload["image_ref_url"].(string); url != want {
						t.Errorf("the worker %s boots from %q, want %q: a shoot's workers run the image "+
							"its tenant uploaded", transition.ResourceID, url, want)
						break
					}
				}
			})

			t.Run("api-dev boots its pool every working morning", func(t *testing.T) {
				s := shootNamed(t, g, "api-dev")
				transitions := transitionsOf(g.schedule, s)
				// The working days after the first, which is the day the shoot is
				// created on rather than woken up on.
				wakeUps := workingDays(july2026.AddDate(0, 0, 1), july2026.AddDate(0, 1, 0))

				var previous []string
				for _, d := range wakeUps {
					ids := idsIn(transitions, "compute.instance.create.end", at(d, 7, 0), at(d, 7, 1))
					if len(ids) != s.baseWorkers {
						t.Errorf("api-dev boots %d workers at 07:00 on %s, want the %d of its pool: a "+
							"woken shoot comes back with the pool it hibernated with",
							len(ids), d.Format(time.DateOnly), s.baseWorkers)
					}
					for _, id := range ids {
						if slices.Contains(previous, id) {
							t.Errorf("the instance %s comes back on %s, want a woken shoot to boot new "+
								"machines: a destroyed server is billed to its end and never resumed",
								id, d.Format(time.DateOnly))
						}
					}
					previous = ids
				}

				for _, d := range append([]time.Time{july2026}, wakeUps...) {
					ids := idsIn(transitions, "compute.instance.delete.end", at(d, 19, 0), at(d, 19, 5))
					if len(ids) != s.baseWorkers {
						t.Errorf("api-dev destroys %d workers at 19:00 on %s, want the %d of its pool: "+
							"hibernation takes every worker and keeps everything else",
							len(ids), d.Format(time.DateOnly), s.baseWorkers)
					}
				}

				for _, transition := range transitions {
					if transition.EventType == "compute.instance.create.end" && !workingDay(transition.At) {
						t.Errorf("api-dev boots a worker on %s, a %s, want it hibernated over the weekend",
							transition.At, transition.At.Weekday())
					}
				}

				// A worker that boots from a volume names no image: the image was
				// written to the volume before the server existed.
				for _, transition := range transitions {
					if transition.EventType != "compute.instance.create.end" {
						continue
					}
					if url, _ := transition.Payload["image_ref_url"].(string); url != "" {
						t.Errorf("the worker %s names the image %q, want none: it boots off the volume "+
							"the image was written to", transition.ResourceID, url)
						break
					}
				}

				// A root volume carries the name of the worker it boots, which is
				// what pairs the two here.
				workerCreated, workerDeleted := map[string]time.Time{}, map[string]time.Time{}
				volumeCreated, volumeDeleted := map[string]time.Time{}, map[string]time.Time{}
				for _, transition := range transitions {
					name, _ := transition.Payload["display_name"].(string)
					switch transition.EventType {
					case "compute.instance.create.end":
						workerCreated[name] = transition.At
					case "compute.instance.delete.end":
						workerDeleted[name] = transition.At
					case "volume.create.end":
						volumeCreated[name] = transition.At
					case "volume.delete.end":
						volumeDeleted[name] = transition.At
					}
				}
				for name, booted := range workerCreated {
					provisioned, ok := volumeCreated[name]
					if !ok {
						t.Errorf("the worker %s has no root volume, want one: it runs on a flavor "+
							"without a root disk and boots from a volume", name)
						continue
					}
					if !provisioned.Before(booted) {
						t.Errorf("the root volume of %s is created at %s and the worker at %s, want the "+
							"volume first: the image is written to it before the server boots off it",
							name, provisioned, booted)
					}
					released, ok := volumeDeleted[name]
					if !ok {
						t.Errorf("the root volume of %s is never deleted, want it to go with its worker: "+
							"a volume nothing releases is billed past the server that carried it", name)
						continue
					}
					if destroyed := workerDeleted[name]; !released.After(destroyed) {
						t.Errorf("the root volume of %s is deleted at %s and the worker at %s, want the "+
							"volume after it", name, released, destroyed)
					}
				}

				for _, transition := range transitions {
					if transition.EventType == "volume.delete.end" && transition.ResourceID == s.claims[0].id {
						t.Errorf("the first claim of api-dev is released at %s, want it to outlive the "+
							"month: it carries the state that survives every workload", transition.At)
					}
				}
			})

			t.Run("batch is torn down in the order it was built", func(t *testing.T) {
				s := shootNamed(t, g, "batch")
				transitions := transitionsOf(g.schedule, s)

				from, to := july2026.Add(2*day), july2026.Add(8*day)
				if s.createdAt.Before(from) || !s.createdAt.Before(to) {
					t.Errorf("batch is created at %s, want it inside [%s, %s): a transient shoot lives "+
						"a stretch of the month and not all of it", s.createdAt, from, to)
				}
				last := transitions[len(transitions)-1]
				from, to = july2026.Add(18*day), july2026.Add(27*day)
				if last.At.Before(from) || !last.At.Before(to) {
					t.Errorf("batch reports its last transition at %s, want it inside [%s, %s)",
						last.At, from, to)
				}
				if !isClaimDelete(last) {
					name, _ := last.Payload["display_name"].(string)
					t.Errorf("batch ends on %s of %q, want the release of a claim: the tear-down is the "+
						"last thing the shoot does and its claims go last of all",
						last.EventType, name)
				}

				// The tear-down day, which is the only day of the shoot the deletes
				// below can come from: a shoot is torn down inside working hours, so
				// its last day brings nothing else.
				torn := transitions[:0:0]
				for _, transition := range transitions {
					if !transition.At.Before(at(s.deletedAt, 0, 0)) {
						torn = append(torn, transition)
					}
				}

				for _, lb := range s.loadBalancers {
					address := instantOf(torn, "floatingip.delete.end", lb.fip.id)
					balancer := instantOf(torn, "octavia.loadbalancer.delete.end", lb.id)
					if address.IsZero() || balancer.IsZero() {
						t.Fatalf("the tear-down releases the address of %s at %v and deletes the "+
							"balancer at %v, want both", lb.name, address, balancer)
					}
					if !address.Before(balancer) {
						t.Errorf("the address of %s is released at %s and its balancer deleted at %s, "+
							"want the address first: an address of a gone balancer is one nothing "+
							"releases", lb.name, address, balancer)
					}
				}

				_, balancers, hasBalancers := window(torn, func(transition Transition) bool {
					return transition.EventType == "octavia.loadbalancer.delete.end"
				})
				workersFrom, workersTo, hasWorkers := window(torn, func(transition Transition) bool {
					return transition.EventType == "compute.instance.delete.end"
				})
				claims, _, hasClaims := window(torn, isClaimDelete)
				if !hasBalancers || !hasWorkers || !hasClaims {
					t.Fatalf("the tear-down of batch deletes balancers = %v, workers = %v, claims = %v, "+
						"want all three", hasBalancers, hasWorkers, hasClaims)
				}
				if !balancers.Before(workersFrom) {
					t.Errorf("the last balancer of batch goes at %s and its first worker at %s, want the "+
						"balancers first: they route to the workers", balancers, workersFrom)
				}
				if !workersTo.Before(claims) {
					t.Errorf("the last worker of batch goes at %s and its first claim at %s, want the "+
						"workers first: a claim is released once nothing mounts it", workersTo, claims)
				}
			})

			t.Run("a claim that runs out of room doubles and grows at most twice", func(t *testing.T) {
				// A resize is the one step that changes what a claim is billed at
				// after it exists, and the cap is what keeps a month from doubling
				// one volume into a size no cluster carries.
				sizes := make(map[string]int)
				grown := make(map[string]int)
				for _, transition := range ofWorkload(g.schedule, workloadGardener) {
					size, _ := transition.Payload["size"].(int)
					switch transition.EventType {
					case "volume.create.end":
						sizes[transition.ResourceID] = size
					case "volume.resize.end":
						grown[transition.ResourceID]++
						if want := 2 * sizes[transition.ResourceID]; size != want {
							t.Errorf("the claim %s grows to %d GB from %d GB, want %d: a claim that runs "+
								"out of room doubles", transition.ResourceID, size,
								sizes[transition.ResourceID], want)
						}
						sizes[transition.ResourceID] = size
					}
				}

				if len(grown) == 0 {
					t.Fatalf("no claim of the month grows, want the working days to expand one: a month " +
						"without a resize never prices a size change")
				}
				for id, count := range grown {
					if count > 2 {
						t.Errorf("the claim %s grows %d times, want at most twice: a claim doubled once "+
							"more than that is a volume of a size no cluster carries", id, count)
					}
				}
			})
		})
	}
}

// TestLoadBalancersAreBookedFromTheirUpdate covers the notification order a
// balancer's size depends on. Octavia sends the create before the service's
// ports are attached, so it carries no listener and no pool, and the update
// that follows carries both: a month booked from the create alone would bill
// every balancer of it as empty.
func TestLoadBalancersAreBookedFromTheirUpdate(t *testing.T) {
	g := buildMonth(t, 1)

	want := []string{
		"octavia.loadbalancer.create.end",
		"floatingip.create.end",
		"octavia.loadbalancer.update.end",
	}
	balancers := 0
	for _, gp := range g.gardenerProjects {
		for _, s := range gp.shoots {
			transitions := transitionsOf(g.schedule, s)
			for _, lb := range s.loadBalancers {
				balancers++

				var of []Transition
				for _, transition := range transitions {
					if transition.ResourceID == lb.id || transition.ResourceID == lb.fip.id {
						of = append(of, transition)
					}
				}
				if len(of) < len(want) {
					t.Fatalf("the balancer %s reports %d transitions, want at least its create, its "+
						"address, and its update", lb.name, len(of))
				}
				for i, eventType := range want {
					if of[i].EventType != eventType {
						t.Errorf("the balancer %s reports %s as its transition %d, want %s: octavia "+
							"creates the balancer, neutron gives it its address, and the update carries "+
							"what the service attached", lb.name, of[i].EventType, i, eventType)
					}
				}

				size, _ := sizeOf(t, of[0])
				for _, member := range []string{"listeners", "pools"} {
					if got := fmt.Sprint(size[member]); got != "0" {
						t.Errorf("the create of %s books %s = %s, want 0: it is sent before the "+
							"service's ports are attached", lb.name, member, got)
					}
				}

				size, notification := sizeOf(t, of[2])
				for _, member := range []string{"listeners", "pools"} {
					elements, _ := notification.Payload[member].([]any)
					if len(elements) == 0 {
						t.Errorf("the update of %s carries no %s, want the ones the service attached",
							lb.name, member)
					}
					if got, count := fmt.Sprint(size[member]), strconv.Itoa(len(elements)); got != count {
						t.Errorf("the update of %s books %s = %s, want %s, the number its payload "+
							"carries", lb.name, member, got, count)
					}
				}
			}
		}
	}
	if balancers == 0 {
		t.Fatalf("the month holds no load balancer, want one per service of type LoadBalancer")
	}

	deletes := 0
	for _, transition := range g.schedule {
		if transition.EventType == "octavia.loadbalancer.delete.end" {
			deletes++
		}
	}
	if deletes == 0 {
		t.Errorf("the month renders no octavia.loadbalancer.delete.end, want the torn-down shoot to " +
			"close its balancers: one nothing deletes is billed past the cluster it served")
	}

	// The listener day publishes another port on the ingress balancer of
	// api-prod. Octavia notifies on the balancer alone, so the new count arrives
	// on a second update rather than on a notification of the listener itself,
	// and that update is the one place a booked balancer grows inside the month.
	t.Run("a listener added later is booked on a second update", func(t *testing.T) {
		s := shootNamed(t, g, "api-prod")
		lb := s.loadBalancers[0]

		var updates []Transition
		for _, transition := range transitionsOf(g.schedule, s) {
			if transition.EventType == "octavia.loadbalancer.update.end" && transition.ResourceID == lb.id {
				updates = append(updates, transition)
			}
		}
		if len(updates) != 2 {
			t.Fatalf("the balancer %s reports %d updates, want 2: the one its service attaches its "+
				"ports on and the one the listener day adds a port on", lb.name, len(updates))
		}

		attached, _ := sizeOf(t, updates[0])
		grown, _ := sizeOf(t, updates[1])
		before, _ := attached["listeners"].(int)
		after, _ := grown["listeners"].(int)
		if after != before+1 {
			t.Errorf("the balancer %s books %d listeners on its second update and %d on its first, "+
				"want one more: a service that publishes another port adds a listener to the balancer "+
				"it already has", lb.name, after, before)
		}
		if grown["pools"] != attached["pools"] {
			t.Errorf("the balancer %s books %v pools on its second update and %v on its first, want "+
				"them unchanged: the new listener shares the pool behind it",
				lb.name, grown["pools"], attached["pools"])
		}
	})
}

// TestCIRunnersBurstInWorkingHours covers when the CI tenant's runners exist
// and how long each of them lives. A runner holds one job, so a month of them
// is churn the other two workloads do not produce: hundreds of servers that are
// created and closed inside the period rather than carried over it.
func TestCIRunnersBurstInWorkingHours(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			schedule := ofWorkload(generateMonth(t, seed, july2026, testCloud), workloadCI)

			created := make(map[string]time.Time)
			deleted := make(map[string]time.Time)
			perDay := make(map[time.Time]int)
			for _, transition := range schedule {
				switch transition.EventType {
				case "compute.instance.create.end":
					created[transition.ResourceID] = transition.At
					perDay[at(transition.At, 0, 0)]++
					if !workingDay(transition.At) || transition.At.Hour() < 7 || transition.At.Hour() >= 19 {
						t.Errorf("the runner %s comes up on a %s at %s, want a Monday to Friday between "+
							"07:00 and 19:00 UTC: a CI runner runs while the pipelines that ask for it run",
							transition.ResourceID, transition.At.Weekday(), transition.At.Format(time.RFC3339))
					}
				case "compute.instance.delete.end":
					deleted[transition.ResourceID] = transition.At
				}
			}
			if len(created) == 0 {
				t.Fatalf("the month holds no CI runner, want the bursts of every working day of July")
			}

			for id, createdAt := range created {
				deletedAt, ok := deleted[id]
				if !ok {
					t.Errorf("the runner %s comes up at %s and reports no delete, want one: a runner "+
						"nothing deletes is billed as a server that outlives its job",
						id, createdAt.Format(time.RFC3339))
					continue
				}
				if life := deletedAt.Sub(createdAt); life < 3*time.Minute || life >= 40*time.Minute {
					t.Errorf("the runner %s lives %s, want [3m, 40m): a runner is destroyed when its job "+
						"ends, and a longer life is a fleet the month bills by the hour", id, life)
				}
			}

			// Four to eight bursts of two to five runners each.
			for _, d := range workingDays(july2026, july2026.AddDate(0, 1, 0)) {
				if runners := perDay[d]; runners < 8 || runners > 40 {
					t.Errorf("%s brings %d runners, want 8 to 40: a day outside the band is a month whose "+
						"bursts drifted off the working days they belong on",
						d.Format("2006-01-02"), runners)
				}
			}
		})
	}
}

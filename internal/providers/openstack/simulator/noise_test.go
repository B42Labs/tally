package simulator

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/cardinality"
	"github.com/b42labs/tally/internal/providers/openstack"
)

// noiseTypes is the catalogue of notifications a month carries and the
// collector bills nothing for, in the order noise.go renders them, grouped by
// the exchange they are published on. None of them has a recorded sample, so
// this list and the member sets of noiseMembers (render_test.go) are what holds
// the catalogue in place: a type that leaves one of them is a type no test
// covers any more.
var noiseTypes = []string{
	// nova
	"scheduler.select_destinations.start",
	"scheduler.select_destinations.end",
	"compute.instance.create.start",
	"compute.instance.update",
	"compute.instance.exists",
	"compute.instance.delete.start",
	"compute.instance.shutdown.start",
	"compute.instance.shutdown.end",
	"compute.instance.power_off.start",
	"compute.instance.power_on.start",
	"compute.instance.resize.start",
	"compute.instance.finish_resize.start",
	"compute.instance.shelve_offload.start",
	"compute.instance.unshelve.start",
	"keypair.import.start",
	"keypair.import.end",
	"keypair.delete.start",
	"keypair.delete.end",
	// cinder
	"volume.create.start",
	"volume.delete.start",
	"volume.resize.start",
	"volume.transfer.accept.start",
	"volume.attach.start",
	"volume.attach.end",
	"volume.detach.start",
	"volume.detach.end",
	// neutron
	"network.create.start",
	"network.create.end",
	"network.delete.start",
	"network.delete.end",
	"subnet.create.start",
	"subnet.create.end",
	"subnet.delete.start",
	"subnet.delete.end",
	"router.create.start",
	"router.create.end",
	"router.interface.create",
	"router.interface.delete",
	"router.delete.start",
	"router.delete.end",
	"security_group.create.start",
	"security_group.create.end",
	"security_group.delete.start",
	"security_group.delete.end",
	"security_group_rule.create.start",
	"security_group_rule.create.end",
	"port.create.start",
	"port.create.end",
	"port.update.start",
	"port.update.end",
	"port.delete.start",
	"port.delete.end",
	// glance
	"image.prepare",
	"image.activate",
	// keystone
	"identity.project.created",
	"identity.user.created",
	"identity.authenticate",
	// designate
	"dns.zone.create",
	"dns.recordset.create",
	"dns.recordset.delete",
	// barbican
	"audit.http.request",
	"audit.http.response",
}

// has returns the transition of that type at that instant, and false when the
// slice holds none. Every noise instant is a fixed offset from the billable
// instant it belongs to, so a sequence is asserted by asking for each of its
// members at the second it is due at.
func has(transitions []Transition, eventType string, at time.Time) (Transition, bool) {
	for _, transition := range transitions {
		if transition.EventType == eventType && transition.At.Equal(at) {
			return transition, true
		}
	}
	return Transition{}, false
}

// byResource indexes a schedule by the resource each transition is about, in
// schedule order. A month holds thousands of transitions and the sequences
// below are read per resource, so the index is built once and walked instead of
// the schedule being scanned per instance.
func byResource(transitions []Transition) map[string][]Transition {
	index := make(map[string][]Transition)
	for _, transition := range transitions {
		index[transition.ResourceID] = append(index[transition.ResourceID], transition)
	}
	return index
}

// byAuditTarget indexes the month's CADF audit records by the resource each of
// them names. A record is reported under an id of its own, so the resource
// index does not reach one: what ties a record to the call it stands for is its
// target.
func byAuditTarget(transitions []Transition) map[string][]Transition {
	index := make(map[string][]Transition)
	for _, transition := range transitions {
		target, ok := transition.Payload["target"].(map[string]any)
		if !ok {
			continue
		}
		id, _ := target["id"].(string)
		index[id] = append(index[id], transition)
	}
	return index
}

// buildWorld returns the generator of a month before any workload has run: the
// world newGenerator draws and an empty schedule. It is what the helpers of
// noise.go are called on directly, so a path the month never produces is
// covered without a seed that produces it.
func buildWorld(t *testing.T, seed uint64) *generator {
	t.Helper()

	shape := rand.New(rand.NewPCG(seed, shapeStream))
	identifiers := idReader{src: rand.New(rand.NewPCG(seed, identifierSalt(testCloud, july2026)))}

	return newGenerator(shape, identifiers, noiseIdentifiers(seed, testCloud, july2026),
		july2026, july2026.AddDate(0, 1, 0), testCloud)
}

// TestEverySeedRendersTheWholeCatalogue is the drift guard of the catalogue. A
// month that renders 61 of the 62 types is a month whose skip counters are
// short of one type, whichever seed an operator picks, so every seed is held
// against the whole list rather than one month against most of it.
func TestEverySeedRendersTheWholeCatalogue(t *testing.T) {
	const want = 62
	if len(noiseTypes) != want {
		t.Fatalf("the catalogue names %d types, want %d: the list is what the member sets and the "+
			"sequences below are held against", len(noiseTypes), want)
	}

	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			schedule := generateMonth(t, seed, july2026, testCloud)

			rendered := make(map[string]int, len(noiseTypes))
			for _, transition := range schedule {
				rendered[transition.EventType]++

				if slices.Contains(noiseTypes, transition.EventType) {
					if transition.Billable {
						t.Errorf("%s is billable, want the collector to record nothing for it: "+
							"WriteEvents refuses a transition that is billable and mapped to nothing",
							transition.EventType)
					}
					if transition.PublisherID == "" {
						t.Errorf("%s names no publisher, want the service instance that emitted it: "+
							"octavia is the one service that publishes without one and it renders no "+
							"noise", transition.EventType)
					}
					continue
				}
				if !transition.Billable && transition.EventType != imageCreateType {
					t.Errorf("%s is neither billable nor part of the catalogue, want every type of a "+
						"month to be one or the other: %s is the one skipped billable type",
						transition.EventType, imageCreateType)
				}
			}

			for _, eventType := range noiseTypes {
				if rendered[eventType] == 0 {
					t.Errorf("seed %d renders no %s, want the whole catalogue in every month: a type "+
						"no seed renders is one nothing on the bus exercises", seed, eventType)
				}
			}
		})
	}
}

// TestNoiseReachesForNeitherOfTheOtherTwoStreams is the guard on the rule the
// third generator exists for: the catalogue takes no draw from the shape stream
// and no identifier from the identifier stream, so the billable transitions of
// a seed, a period, and a cloud are the ones the month would render without it.
//
// A helper that reached for the wrong reader would renumber every billable
// resource of every seed downstream of it and leave the whole suite green: the
// months two runs of one build produce still match, two clouds still draw
// disjoint sets, and every other case here is structural. The rule is about the
// file rather than about the month, so it is checked on the file: nowhere in
// noise.go may a selector name the generator's shape or its identifier stream.
func TestNoiseReachesForNeitherOfTheOtherTwoStreams(t *testing.T) {
	const file = "noise.go"

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}

	forbidden := map[string]string{
		"shape": "the shape stream: a draw from it moves every instant and every size the " +
			"billable month draws after it",
		"identifiers": "the identifier stream: a draw from it renumbers every billable resource " +
			"the month names after it",
	}
	// The draws the catalogue is meant to make. Counting them is what keeps this
	// case from passing over a file that no longer draws anything at all.
	draws := 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if selector.Sel.Name == "noiseIDs" {
			draws++
		}
		if why, ok := forbidden[selector.Sel.Name]; ok {
			t.Errorf("%s reaches for %s, want it to take nothing from %s",
				fset.Position(selector.Pos()), selector.Sel.Name, why)
		}
		return true
	})

	if draws == 0 {
		t.Errorf("%s takes no identifier from the noise stream, want the stream this case is here "+
			"to keep the catalogue on: it is looking at the wrong file or at the wrong name", file)
	}
}

// TestTheNoiseDrawsItsMessageIdsFromItsOwnStream covers the last identifier a
// noise transition needs. Every other id of the catalogue is drawn while the
// month is generated, and the message ids are drawn over the sorted schedule
// once it stands: a noise message id taken from the identifier stream would put
// the ids the collector books under the length of the catalogue, and one
// transition more or less would renumber every event of every seed.
//
// The noise stream hands out nothing but uuids, so its draws are replayed here
// and the month's noise message ids have to be the run of them that follows the
// ids of the announced resources.
func TestTheNoiseDrawsItsMessageIdsFromItsOwnStream(t *testing.T) {
	const seed = 1
	schedule := generateMonth(t, seed, july2026, testCloud)

	var want []string
	for _, transition := range schedule {
		if transition.noise {
			want = append(want, transition.MessageID)
		}
	}
	if len(want) == 0 {
		t.Fatalf("the month carries no noise, want the catalogue")
	}

	// The draws that named the announced resources come first. How many there
	// are follows from the month, so they are skipped rather than counted, and
	// the bound is far above what a month of any seed takes.
	const bound = 1 << 20
	stream := noiseIdentifiers(seed, testCloud, july2026)
	found := false
	for range bound {
		if stream.nextUUID() == want[0] {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the first noise message id %s is no draw of the noise stream, want every message "+
			"id of the catalogue to come from it: one drawn from the identifier stream renumbers "+
			"the billable month", want[0])
	}
	for i, id := range want[1:] {
		if got := stream.nextUUID(); got != id {
			t.Fatalf("the noise message ids leave the stream at number %d: the month carries %s "+
				"where the noise stream hands out %s, want the two to run together to the end of "+
				"the month", i+1, id, got)
		}
	}
}

// TestAMonthStaysInsideTheCollectorsLabelBudget covers the one bound a month
// can outgrow without failing anything else. The collector admits
// openstack.LabelValueLimit distinct event_type values, first seen first
// served, and the consumed and the skipped counters share that budget. A month
// is published in chronological order and opens with the noise, so the types
// that would be folded into event_type="other" are the late ones: the delete of
// a transient shoot's balancer and the image delete near the month's end, which
// are the series an operator is told to read.
func TestAMonthStaysInsideTheCollectorsLabelBudget(t *testing.T) {
	types := make(map[string]struct{}, len(noiseTypes))
	for _, transition := range generateMonth(t, 1, july2026, testCloud) {
		types[transition.EventType] = struct{}{}
	}

	if len(types) >= openstack.LabelValueLimit {
		t.Errorf("a month renders %d distinct event types and the collector admits %d, want fewer "+
			"than the bound: the types past it are counted under event_type=%q",
			len(types), openstack.LabelValueLimit, cardinality.Overflow)
	}
}

// TestExchangeForNamesEveryService covers where the catalogue is published. A
// notification on the wrong exchange is one no bound queue receives, and an
// unknown type is reported as the empty exchange rather than guessed at.
func TestExchangeForNamesEveryService(t *testing.T) {
	tests := []struct {
		eventType string
		exchange  string
	}{
		{"scheduler.select_destinations.start", "nova"},
		{"keypair.import.end", "nova"},
		{"compute.instance.exists", "nova"},
		{"network.create.end", "neutron"},
		{"subnet.delete.start", "neutron"},
		{"router.interface.create", "neutron"},
		{"security_group.create.end", "neutron"},
		{"security_group_rule.create.end", "neutron"},
		{"port.update.end", "neutron"},
		{"identity.authenticate", "keystone"},
		{"dns.zone.create", "designate"},
		{"audit.http.request", "barbican"},
		{"octavia.loadbalancer.create.end", "octavia"},
		{"foo.bar", ""},
	}

	for _, test := range tests {
		t.Run(test.eventType, func(t *testing.T) {
			if got := exchangeFor(test.eventType); got != test.exchange {
				t.Errorf("exchangeFor(%q) = %q, want %q", test.eventType, got, test.exchange)
			}
		})
	}
}

// TestInstancesBootAndDieInSequence holds every server of every month against
// the notifications a deployment sends around its create and its delete. The
// distances are fixed, so a boot whose port is bound to another host or a
// delete that leaves its port behind is a sequence an operator would not
// recognise.
func TestInstancesBootAndDieInSequence(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			schedule := generateMonth(t, seed, july2026, testCloud)
			byID := byResource(schedule)

			// A server's transitions carry no port id, so the port of one is found
			// through the device its create names.
			ports := make(map[string][]Transition)
			for _, transition := range schedule {
				if transition.EventType != "port.create.end" {
					continue
				}
				payload, _ := transition.Payload["port"].(map[string]any)
				deviceID, _ := payload["device_id"].(string)
				ports[deviceID] = append(ports[deviceID], transition)
			}

			for _, transition := range schedule {
				switch transition.EventType {
				case "compute.instance.create.end":
					requireBoot(t, byID, ports, transition)
				case "compute.instance.delete.end":
					requireDelete(t, byID, ports, transition)
				}
			}

			// Every step of an instance is a pair, and the .start half of one is
			// five seconds before the .end the collector books.
			for _, step := range []string{
				"power_off", "power_on", "resize", "finish_resize", "shelve_offload", "unshelve",
			} {
				start, end := "compute.instance."+step+".start", "compute.instance."+step+".end"
				for _, transition := range schedule {
					if transition.EventType != start {
						continue
					}
					if _, ok := has(byID[transition.ResourceID], end,
						transition.At.Add(stepLead)); !ok {
						t.Errorf("the instance %s reports %s at %s and no %s at %s, want both halves "+
							"of a step: a .start without its .end is a step nothing books",
							transition.ResourceID, start, transition.At, end,
							transition.At.Add(stepLead))
					}
				}
			}
		})
	}
}

// requireBoot holds one create against the boot nova and neutron send before
// it: the scheduler's decision, the server nova announces, the three updates of
// a build, and the port the server holds its address on.
func requireBoot(t *testing.T, byID, ports map[string][]Transition, create Transition) {
	t.Helper()

	id, at := create.ResourceID, create.At
	of := byID[id]
	for _, step := range []struct {
		eventType string
		lead      time.Duration
	}{
		{"scheduler.select_destinations.start", 25 * time.Second},
		{"scheduler.select_destinations.end", 24 * time.Second},
		{"compute.instance.create.start", 20 * time.Second},
	} {
		if _, ok := has(of, step.eventType, at.Add(-step.lead)); !ok {
			t.Errorf("the instance %s is created at %s and reports no %s at %s, want the boot a "+
				"deployment sends before a create", id, at, step.eventType, at.Add(-step.lead))
		}
	}

	for i, task := range []string{"networking", "block_device_mapping", "spawning"} {
		when := at.Add(-time.Duration(15-5*i) * time.Second)
		update, ok := has(of, "compute.instance.update", when)
		if !ok {
			t.Errorf("the instance %s reports no compute.instance.update at %s, want the three a "+
				"build passes through", id, when)
			continue
		}
		if got, _ := update.Payload["new_task_state"].(string); got != task {
			t.Errorf("the instance %s takes up the task %q at %s, want %q: a build runs from "+
				"scheduling to spawning", id, got, when, task)
		}
	}

	if len(ports[id]) != 1 {
		t.Errorf("the instance %s is created at %s and holds %d ports, want the one neutron gives "+
			"every server", id, at, len(ports[id]))
		return
	}
	created, ok := has(ports[id], "port.create.end", at.Add(-16*time.Second))
	if !ok {
		t.Errorf("the port %s of the instance %s is created at %s, want it at %s: neutron creates it "+
			"while nova builds the server", ports[id][0].ResourceID, id, ports[id][0].At,
			at.Add(-16*time.Second))
		return
	}
	bound, ok := has(byID[created.ResourceID], "port.update.end", at.Add(-11*time.Second))
	if !ok {
		t.Errorf("the port %s of the instance %s is never bound at %s, want the compute to bind it "+
			"before the server is launched", created.ResourceID, id, at.Add(-11*time.Second))
		return
	}
	payload, _ := bound.Payload["port"].(map[string]any)
	if got, _ := payload["status"].(string); got != "ACTIVE" {
		t.Errorf("the bound port %s reports status %q, want \"ACTIVE\": a port a compute has taken "+
			"is up", created.ResourceID, got)
	}
	host, _ := create.Payload["host"].(string)
	if got, _ := payload["binding:host_id"].(string); got != host {
		t.Errorf("the port %s of the instance %s is bound to %q, want %q, the compute the server "+
			"runs on", created.ResourceID, id, got, host)
	}
}

// requireDelete holds one delete against the sequence nova sends before it and
// the port neutron releases after it.
func requireDelete(t *testing.T, byID, ports map[string][]Transition, del Transition) {
	t.Helper()

	id, at := del.ResourceID, del.At
	of := byID[id]
	update, ok := has(of, "compute.instance.update", at.Add(-5*time.Second))
	if !ok {
		t.Errorf("the instance %s is deleted at %s and reports no compute.instance.update at %s, "+
			"want the task the delete begins with", id, at, at.Add(-5*time.Second))
	} else if got, _ := update.Payload["new_task_state"].(string); got != "deleting" {
		t.Errorf("the instance %s takes up the task %q before its delete, want \"deleting\"", id, got)
	}

	audit, ok := has(of, "compute.instance.exists", at.Add(-4*time.Second))
	if !ok {
		t.Errorf("the instance %s reports no compute.instance.exists at %s, want the audit a server "+
			"sends before it is gone", id, at.Add(-4*time.Second))
	} else if got, _ := audit.Payload["audit_period_ending"].(string); got != stamp(at) {
		t.Errorf("the pre-delete audit of %s ends at %q, want %q, the instant the server goes",
			id, got, stamp(at))
	}

	for _, step := range []struct {
		eventType string
		lead      time.Duration
	}{
		{"compute.instance.delete.start", 3 * time.Second},
		{"compute.instance.shutdown.start", 2 * time.Second},
		{"compute.instance.shutdown.end", 1 * time.Second},
	} {
		if _, ok := has(of, step.eventType, at.Add(-step.lead)); !ok {
			t.Errorf("the instance %s is deleted at %s and reports no %s at %s, want the tear-down "+
				"nova sends before a delete", id, at, step.eventType, at.Add(-step.lead))
		}
	}

	if len(ports[id]) == 0 {
		t.Errorf("the instance %s is deleted at %s and never held a port, want the one neutron "+
			"gives every server", id, at)
		return
	}
	portID := ports[id][0].ResourceID
	if _, ok := has(byID[portID], "port.delete.end", at.Add(2*time.Second)); !ok {
		t.Errorf("the port %s of the instance %s is not released at %s, want neutron to take it "+
			"back once the server is gone", portID, id, at.Add(2*time.Second))
	}
}

// TestNoTwoLivePortsShareAFixedAddress covers the one allocator every subnet
// has. Neutron hands a fixed address out once and takes it back when the port
// that held it is released, so two ports of one subnet reporting the same
// address while both are up is a state no real neutron can reach. The workers'
// ports and the balancers' VIP ports lie on the same shoot subnet, which is
// where a second allocator would collide with the first.
func TestNoTwoLivePortsShareAFixedAddress(t *testing.T) {
	for seed := uint64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			schedule := generateMonth(t, seed, july2026, testCloud)
			// A port that is never released holds its address to the end of the
			// month, which is where a life without a delete ends.
			to := july2026.AddDate(0, 1, 0)
			released := make(map[string]time.Time, len(schedule))
			for _, transition := range schedule {
				if transition.EventType == "port.delete.end" {
					released[transition.ResourceID] = transition.At
				}
			}

			// The ports that held each address of each subnet, by the second they
			// took it and the second they gave it back.
			type life struct {
				id        string
				from, til time.Time
			}
			held := make(map[string][]life)
			for _, transition := range schedule {
				if transition.EventType != "port.create.end" {
					continue
				}
				payload, _ := transition.Payload["port"].(map[string]any)
				fixed, _ := payload["fixed_ips"].([]any)
				if len(fixed) != 1 {
					t.Fatalf("the port %s reports %d fixed addresses, want the one every port of a "+
						"month holds", transition.ResourceID, len(fixed))
				}
				first, _ := fixed[0].(map[string]any)
				subnet, _ := first["subnet_id"].(string)
				address, _ := first["ip_address"].(string)

				til, ok := released[transition.ResourceID]
				if !ok {
					til = to
				}
				key := subnet + " " + address
				for _, other := range held[key] {
					if transition.At.Before(other.til) && other.from.Before(til) {
						t.Errorf("the ports %s and %s both hold %s on the subnet %s, from %s to %s "+
							"and from %s to %s, want one port per address: neutron allocates an "+
							"address of a subnet once", other.id, transition.ResourceID, address,
							subnet, other.from, other.til, transition.At, til)
					}
				}
				held[key] = append(held[key], life{id: transition.ResourceID, from: transition.At, til: til})
			}
			if len(held) == 0 {
				t.Fatalf("the month creates no port, want one per server and one per balancer")
			}
		})
	}
}

// TestExistsAuditsCoverEveryDayAnInstanceExisted holds nova's periodic audit
// against the days every server of a month existed. An instance that is audited
// twice for one day, or not at all for a day it ran, is a bus whose skipped
// counters no longer stand for the fleet.
func TestExistsAuditsCoverEveryDayAnInstanceExisted(t *testing.T) {
	schedule := generateMonth(t, 1, july2026, testCloud)
	to := july2026.AddDate(0, 1, 0)
	byID := byResource(schedule)

	shortLived, resized, stopped := 0, 0, 0
	for _, create := range schedule {
		if create.EventType != "compute.instance.create.end" {
			continue
		}
		id, createdAt := create.ResourceID, create.At
		of := byID[id]
		deletedAt := instantOf(of, "compute.instance.delete.end", id)

		// The daily audits of the instance, by the midnight each of them ends at.
		// The audit a delete sends ends at the delete instant instead, which is
		// what tells the two apart.
		daily := make(map[time.Time]Transition)
		occupied := make(map[time.Time]struct{}, len(of))
		count := 0
		for _, transition := range of {
			occupied[transition.At] = struct{}{}
			if transition.EventType != "compute.instance.exists" {
				continue
			}
			ending := parseStamp(t, transition.Payload["audit_period_ending"])
			if !ending.Equal(at(ending, 0, 0)) {
				continue
			}
			daily[ending] = transition
			count++
		}

		var want []time.Time
		for midnight := at(createdAt, 0, 0).Add(day); midnight.Before(to); midnight = midnight.Add(day) {
			if deletedAt.IsZero() || deletedAt.After(midnight.Add(-day)) {
				want = append(want, midnight)
			}
		}
		if count != len(want) {
			t.Errorf("the instance %s is created at %s, deleted at %v and audited %d times, want %d: "+
				"one audit per midnight it saw", id, createdAt, deletedAt, count, len(want))
		}

		for _, midnight := range want {
			audit, ok := daily[midnight]
			if !ok {
				t.Errorf("the instance %s reports no audit over the day that ends at %s, want one: "+
					"a day a server ran and nothing reports is a day the bus is silent about",
					id, midnight)
				continue
			}
			if got, _ := audit.Payload["audit_period_beginning"].(string); got != stamp(midnight.Add(-day)) {
				t.Errorf("the audit of %s at %s covers from %q, want %q: the period is the day it ends",
					id, audit.At, got, stamp(midnight.Add(-day)))
			}
			if audit.At.Before(midnight) {
				t.Errorf("the audit of %s over the day that ends at %s lies at %s, want it at the "+
					"midnight or after it", id, midnight, audit.At)
				continue
			}
			if !audit.At.Before(to) {
				t.Errorf("the audit of %s lies at %s, want it inside the month: an audit at the end "+
					"of the period falls into the month that follows", id, audit.At)
			}
			for second := midnight; second.Before(audit.At); second = second.Add(time.Second) {
				if _, taken := occupied[second]; !taken {
					t.Errorf("the audit of %s is pushed to %s and the instance reports nothing at %s, "+
						"want an audit to step aside from an occupied second alone", id, audit.At, second)
				}
			}
		}

		if !deletedAt.IsZero() && at(createdAt, 0, 0).Equal(at(deletedAt, 0, 0)) && count == 1 {
			shortLived++
		}

		// The state an audit repeats is the one the instance's last .end left it
		// in. A server that is still powered off at a midnight is where that
		// carry-forward shows, and a deleted one is audited once more as deleted.
		for midnight, audit := range daily {
			var last Transition
			for _, transition := range of {
				if strings.HasPrefix(transition.EventType, "compute.instance.") &&
					strings.HasSuffix(transition.EventType, ".end") &&
					transition.At.Before(audit.At) {
					last = transition
				}
			}
			var want string
			switch last.EventType {
			case "compute.instance.power_off.end":
				want = "stopped"
				stopped++
			case "compute.instance.delete.end":
				want = "deleted"
			default:
				continue
			}
			if got, _ := audit.Payload["state"].(string); got != want {
				t.Errorf("the audit of %s over the day that ends at %s reports the state %q, want "+
					"%q: its last notification before the audit was the %s at %s",
					id, midnight, got, want, last.EventType, last.At)
			}
		}

		if create.Workload != workloadClassic {
			continue
		}
		resize, ok := firstOf(of, "compute.instance.resize.end")
		if !ok {
			continue
		}
		resized++
		booted, _ := create.Payload["instance_type"].(string)
		bootedID, _ := create.Payload["instance_flavor_id"].(string)
		bootedDisk, _ := create.Payload["disk_gb"].(int)
		moved, _ := resize.Payload["instance_type"].(string)
		movedRoot, _ := resize.Payload["root_gb"].(int)
		movedEphemeral, _ := resize.Payload["ephemeral_gb"].(int)
		for midnight, audit := range daily {
			want, wantID, wantDisk := booted, bootedID, bootedDisk
			if audit.At.After(resize.At) {
				// The resize payload names the flavor by its type id and reports the
				// two disks apart, so the uuid and the sum an audit carries come
				// from the catalog and from the payload's two members.
				want, wantID, wantDisk = moved, flavorIDNamed(t, moved), movedRoot+movedEphemeral
			}
			if got, _ := audit.Payload["instance_type"].(string); got != want {
				t.Errorf("the audit of %s over the day that ends at %s reports the flavor %q, want "+
					"%q: an audit repeats the server as it stands, and it resized at %s",
					id, midnight, got, want, resize.At)
			}
			if got, _ := audit.Payload["instance_flavor_id"].(string); got != wantID {
				t.Errorf("the audit of %s over the day that ends at %s reports the flavor uuid %q, "+
					"want %q: the three names of a flavor move together, and it resized at %s",
					id, midnight, got, wantID, resize.At)
			}
			if got, _ := audit.Payload["disk_gb"].(int); got != wantDisk {
				t.Errorf("the audit of %s over the day that ends at %s reports %d disk gibibytes, "+
					"want %d: the disk of an audit is the root and the ephemeral of its flavor "+
					"summed, and it resized at %s", id, midnight, got, wantDisk, resize.At)
			}
		}
	}

	if shortLived == 0 {
		t.Errorf("no instance of the month is created and deleted between two midnights, want the CI " +
			"runners: a server whose whole life is one day is audited once and dropped")
	}
	if resized == 0 {
		t.Errorf("no classic instance of the month resizes, want the first of every project: without " +
			"one nothing holds the flavor an audit carries forward")
	}
	if stopped == 0 {
		t.Errorf("no instance of the month is audited while it is powered off, want the power " +
			"cycles of the classic tenants: without one nothing holds the state an audit carries " +
			"forward")
	}
}

// flavorIDNamed is the uuid the catalog gives the flavor of that name. A resize
// payload reports no uuid, so what an audit after one carries is looked up
// rather than read off the notification that moved the server.
func flavorIDNamed(t *testing.T, name string) string {
	t.Helper()

	for _, f := range flavors {
		if f.name == name {
			return f.flavorID
		}
	}
	if bootVolumeFlavor.name == name {
		return bootVolumeFlavor.flavorID
	}
	t.Fatalf("the catalog holds no flavor named %q", name)
	return ""
}

// parseStamp reads back a rendered timestamp. The layout carries no zone, and
// every reader of those digits takes them for UTC.
func parseStamp(t *testing.T, value any) time.Time {
	t.Helper()

	text, ok := value.(string)
	if !ok {
		t.Fatalf("the payload carries %v where a timestamp is due, want the form oslo writes", value)
	}
	parsed, err := time.ParseInLocation(timestampLayout, text, time.UTC)
	if err != nil {
		t.Fatalf("parsing the timestamp %q: %v", text, err)
	}
	return parsed
}

// firstOf returns the first transition of that type, and false when the slice
// holds none.
func firstOf(transitions []Transition, eventType string) (Transition, bool) {
	for _, transition := range transitions {
		if transition.EventType == eventType {
			return transition, true
		}
	}
	return Transition{}, false
}

// TestVolumesAreAttachedAndDetachedAroundTheirLives covers what cinder sends
// around a volume the collector bills. A volume that is never attached reports
// the available status on every billable notification, and one whose detach is
// missing is billed as in use past the server that held it.
func TestVolumesAreAttachedAndDetachedAroundTheirLives(t *testing.T) {
	g := buildMonth(t, 1)
	byID := byResource(g.schedule)

	// The volumes that change hands are the ones no server ever holds.
	spares := make(map[string]struct{}, len(g.projects))
	for _, p := range g.projects {
		spares[p.spare.id] = struct{}{}
	}

	for _, create := range g.schedule {
		if create.EventType != "volume.create.end" {
			continue
		}
		id, at := create.ResourceID, create.At
		if _, ok := has(byID[id], "volume.create.start", at.Add(-volumeProvision)); !ok {
			t.Errorf("the volume %s is created at %s and reports no volume.create.start at %s, want "+
				"the request cinder accepted", id, at, at.Add(-volumeProvision))
		}
		if _, spare := spares[id]; spare {
			for _, transition := range byID[id] {
				if strings.HasPrefix(transition.EventType, "volume.attach.") {
					t.Errorf("the spare volume %s reports %s at %s, want no attach at all: it is the "+
						"volume that changes hands and no server ever holds",
						id, transition.EventType, transition.At)
				}
			}
			continue
		}
		for _, step := range []struct {
			eventType string
			lag       time.Duration
		}{
			{"volume.attach.start", 1 * time.Second},
			{"volume.attach.end", 2 * time.Second},
		} {
			if _, ok := has(byID[id], step.eventType, at.Add(step.lag)); !ok {
				t.Errorf("the volume %s is created at %s and reports no %s at %s, want the attach "+
					"that puts it in use", id, at, step.eventType, at.Add(step.lag))
			}
		}
	}

	for _, del := range g.schedule {
		if del.EventType != "volume.delete.end" {
			continue
		}
		if _, ok := has(byID[del.ResourceID], "volume.delete.start", del.At.Add(-time.Second)); !ok {
			t.Errorf("the volume %s is deleted at %s and reports no volume.delete.start at %s, want "+
				"the delete cinder announces", del.ResourceID, del.At, del.At.Add(-time.Second))
		}
	}

	t.Run("the claim of a hibernating shoot is given back every night", func(t *testing.T) {
		s := shootNamed(t, g, "api-dev")
		claim := s.claims[0]
		created := at(claim.createdAt, 0, 0)

		for _, d := range workingDays(july2026, july2026.AddDate(0, 1, 0)) {
			if d.Before(created) {
				continue
			}
			if ids := idsIn(byID[claim.id], "volume.detach.start", at(d, 19, 0), at(d, 19, 5)); len(ids) != 1 {
				t.Errorf("the first claim of api-dev reports %d detaches in the evening of %s, want "+
					"one: hibernation takes every worker and gives the claims back",
					len(ids), d.Format(time.DateOnly))
			}
			if d.Equal(created) {
				continue
			}
			if ids := idsIn(byID[claim.id], "volume.attach.start", at(d, 7, 0), at(d, 7, 5)); len(ids) != 1 {
				t.Errorf("the first claim of api-dev reports %d attaches in the morning of %s, want "+
					"one: the workloads of a woken shoot mount it again",
					len(ids), d.Format(time.DateOnly))
			}
		}
	})

	t.Run("the root volume of a worker is detached with it", func(t *testing.T) {
		s := shootNamed(t, g, "api-dev")
		gardener := ofWorkload(g.schedule, workloadGardener)

		// A root volume carries the name of the worker it boots, which is what
		// pairs the two here.
		roots := make(map[string]string)
		for _, transition := range gardener {
			if transition.EventType != "volume.create.end" {
				continue
			}
			name, _ := transition.Payload["display_name"].(string)
			roots[name] = transition.ResourceID
		}

		workers := 0
		for _, del := range gardener {
			name, _ := del.Payload["display_name"].(string)
			if del.EventType != "compute.instance.delete.end" ||
				!strings.HasPrefix(name, s.technicalID+"-worker-") {
				continue
			}
			workers++

			root, ok := roots[name]
			if !ok {
				t.Errorf("the worker %s has no root volume, want the one it boots from", name)
				continue
			}
			if _, ok := has(byID[root], "volume.detach.start", del.At.Add(detachLag)); !ok {
				t.Errorf("the root volume of %s is not detached at %s, want cinder to give it back "+
					"once the server that held it is gone", name, del.At.Add(detachLag))
			}
			released := instantOf(byID[root], "volume.delete.end", root)
			if released.IsZero() || !released.After(del.At.Add(detachLag)) {
				t.Errorf("the root volume of %s is deleted at %v and detached at %s, want the delete "+
					"after the detach", name, released, del.At.Add(detachLag))
			}
			// A root volume is the server's first disk, which is what tells the
			// attachment of one from the attachment of a claim.
			attached, ok := firstOf(byID[root], "volume.attach.end")
			if !ok {
				t.Errorf("the root volume of %s is never attached, want the server to boot off it", name)
				continue
			}
			if got := mountpointOf(t, attached); got != "/dev/vda" {
				t.Errorf("the root volume of %s is mounted at %q, want \"/dev/vda\": a server boots "+
					"off its first disk", name, got)
			}
		}
		if workers == 0 {
			t.Fatalf("api-dev deletes no worker, want the pool it hibernates every evening")
		}

		// The other half of that branch: everything a shoot mounts beside the root
		// volume is the second disk of the server that holds it.
		if len(s.claims) == 0 {
			t.Fatalf("api-dev holds no claim, want the ones its workloads keep over the night")
		}
		for _, claim := range s.claims {
			attached, ok := firstOf(byID[claim.id], "volume.attach.end")
			if !ok {
				t.Errorf("the claim %s is never attached, want the worker that mounts it", claim.id)
				continue
			}
			if got := mountpointOf(t, attached); got != "/dev/vdb" {
				t.Errorf("the claim %s is mounted at %q, want \"/dev/vdb\": a claim is the second "+
					"disk of the worker that holds it", claim.id, got)
			}
		}
	})
}

// mountpointOf is the device name the attachment a volume notification carries
// reports. A notification with no attachment or with more than one is a payload
// cinder does not send about a volume a single server holds.
func mountpointOf(t *testing.T, transition Transition) string {
	t.Helper()

	list, _ := transition.Payload["volume_attachment"].([]any)
	if len(list) != 1 {
		t.Fatalf("the %s of %s carries %d attachments, want the one of the server that holds it",
			transition.EventType, transition.ResourceID, len(list))
	}
	record, _ := list[0].(map[string]any)
	mountpoint, _ := record["mountpoint"].(string)
	return mountpoint
}

// TestShootInfrastructureBracketsTheShoot covers what Gardener creates before
// the first worker of a cluster boots and gives back after the last resource of
// it is gone. The infrastructure is billed for by nothing, and it is what the
// billable resources of a shoot sit inside.
func TestShootInfrastructureBracketsTheShoot(t *testing.T) {
	g := buildMonth(t, 1)
	gardener := ofWorkload(g.schedule, workloadGardener)
	byID := byResource(gardener)

	for _, gp := range g.gardenerProjects {
		for _, s := range gp.shoots {
			t.Run(s.name, func(t *testing.T) {
				p := s.owner.tenant
				if len(s.securityGroupRuleIDs) != 2 {
					t.Fatalf("%s has %d security group rules, want the two Gardener opens a worker "+
						"pool with", s.name, len(s.securityGroupRuleIDs))
				}

				authenticated := false
				for _, transition := range gardener {
					if transition.EventType == "identity.authenticate" &&
						transition.At.Equal(s.createdAt.Add(-2*time.Second)) &&
						transition.ProjectID == p.id {
						authenticated = true
						break
					}
				}
				if !authenticated {
					t.Errorf("%s is created at %s and its tenant takes no token at %s, want the one "+
						"reconciliation that builds it", s.name, s.createdAt,
						s.createdAt.Add(-2*time.Second))
				}

				keypair := p.userID + ":" + s.keypairName
				steps := []struct {
					second     int
					eventType  string
					resourceID string
				}{
					{1, "network.create.start", s.networkID},
					{2, "network.create.end", s.networkID},
					{3, "subnet.create.start", s.subnetID},
					{4, "subnet.create.end", s.subnetID},
					{5, "router.create.start", s.routerID},
					{6, "router.create.end", s.routerID},
					{7, "router.interface.create", s.routerID},
					{8, "security_group.create.start", s.securityGroupID},
					{9, "security_group.create.end", s.securityGroupID},
					{10, "security_group_rule.create.start", s.securityGroupRuleIDs[0]},
					{11, "security_group_rule.create.end", s.securityGroupRuleIDs[0]},
					{12, "security_group_rule.create.start", s.securityGroupRuleIDs[1]},
					{13, "security_group_rule.create.end", s.securityGroupRuleIDs[1]},
					{14, "keypair.import.start", keypair},
					{15, "keypair.import.end", keypair},
					{16, "dns.recordset.create", s.apiRecord.id},
				}
				for _, step := range steps {
					when := s.createdAt.Add(time.Duration(step.second) * time.Second)
					if _, ok := has(byID[step.resourceID], step.eventType, when); !ok {
						t.Errorf("%s reports no %s on %s at %s, %d seconds after its creation, want "+
							"the infrastructure a cluster needs before its first worker",
							s.name, step.eventType, step.resourceID, when, step.second)
					}
				}

				// The first worker of a shoot boots between 30 and 90 seconds after
				// its creation instant, so the sixteen seconds of the infrastructure
				// lie before the create the collector books. That create is the
				// bound, and not the scheduler transition the boot begins with: the
				// scheduler decides 25 seconds before the create, which falls inside
				// the infrastructure window itself.
				worker, ok := firstOf(transitionsOf(g.schedule, s), "compute.instance.create.end")
				if !ok {
					t.Fatalf("%s boots no worker, want the pool every cluster comes up with", s.name)
				}
				if !s.createdAt.Add(16 * time.Second).Before(worker.At) {
					t.Errorf("%s finishes its infrastructure at %s and boots its first worker at %s, "+
						"want the infrastructure first: a server on a network that does not exist yet "+
						"is a sequence no deployment sends", s.name,
						s.createdAt.Add(16*time.Second), worker.At)
				}
			})
		}
	}

	t.Run("batch gives its infrastructure back after everything on it", func(t *testing.T) {
		s := shootNamed(t, g, "batch")
		p := s.owner.tenant
		keypair := p.userID + ":" + s.keypairName

		resources := map[string]struct{}{
			s.networkID: {}, s.subnetID: {}, s.routerID: {}, s.securityGroupID: {},
			s.securityGroupRuleIDs[0]: {}, s.securityGroupRuleIDs[1]: {},
			keypair: {}, s.apiRecord.id: {}, s.ingressRecord.id: {},
		}
		var lastClaim time.Time
		for _, transition := range transitionsOf(g.schedule, s) {
			if isClaimDelete(transition) {
				resources[transition.ResourceID] = struct{}{}
				lastClaim = transition.At
			}
		}
		if lastClaim.IsZero() {
			t.Fatalf("batch releases no claim, want the ones its workloads held")
		}

		var of, after []Transition
		for _, transition := range gardener {
			if _, ok := resources[transition.ResourceID]; !ok {
				continue
			}
			of = append(of, transition)
			if transition.At.After(lastClaim) {
				after = append(after, transition)
			}
		}
		if len(of) == 0 {
			t.Fatalf("batch reports nothing on its infrastructure, want it built and given back")
		}
		if last := of[len(of)-1]; last.EventType != "network.delete.end" {
			t.Errorf("batch ends its infrastructure on %s at %s, want network.delete.end: nothing of "+
				"a shoot is emitted after its network is gone", last.EventType, last.At)
		}

		want := []struct {
			eventType  string
			resourceID string
		}{
			{"dns.recordset.delete", s.apiRecord.id},
			{"dns.recordset.delete", s.ingressRecord.id},
			{"keypair.delete.start", keypair},
			{"keypair.delete.end", keypair},
			{"security_group.delete.start", s.securityGroupID},
			{"security_group.delete.end", s.securityGroupID},
			{"router.interface.delete", s.routerID},
			{"router.delete.start", s.routerID},
			{"router.delete.end", s.routerID},
			{"subnet.delete.start", s.subnetID},
			{"subnet.delete.end", s.subnetID},
			{"network.delete.start", s.networkID},
			{"network.delete.end", s.networkID},
		}
		if len(after) != len(want) {
			t.Fatalf("batch reports %d transitions on its infrastructure after its last claim went "+
				"at %s, want the %d of the tear-down", len(after), lastClaim, len(want))
		}
		for i, step := range want {
			if after[i].EventType != step.eventType || after[i].ResourceID != step.resourceID {
				t.Errorf("the tear-down of batch reports %s on %s as its step %d, want %s on %s: the "+
					"resources are given back in the order they depend on each other",
					after[i].EventType, after[i].ResourceID, i, step.eventType, step.resourceID)
			}
			if i > 0 && !after[i].At.Equal(after[i-1].At.Add(time.Second)) {
				t.Errorf("the tear-down of batch reports %s at %s and %s at %s, want them a second "+
					"apart: two notifications about one resource in one second are two the projection "+
					"cannot order", after[i-1].EventType, after[i-1].At, after[i].EventType, after[i].At)
			}
		}
	})

	t.Run("a shoot that outlives the month keeps its infrastructure", func(t *testing.T) {
		for _, name := range []string{"api-prod", "api-dev"} {
			s := shootNamed(t, g, name)
			resources := []string{
				s.networkID, s.subnetID, s.routerID, s.securityGroupID,
				s.owner.tenant.userID + ":" + s.keypairName,
			}
			prefixes := []string{
				"network.delete.", "subnet.delete.", "router.delete.",
				"security_group.delete.", "keypair.delete.",
			}
			for _, id := range resources {
				for _, transition := range byID[id] {
					for _, prefix := range prefixes {
						if strings.HasPrefix(transition.EventType, prefix) {
							t.Errorf("%s reports %s on %s at %s, want its infrastructure to outlive "+
								"the month: the cluster is still running when the period ends",
								name, transition.EventType, id, transition.At)
						}
					}
				}
			}
		}
	})
}

// TestTenantsAreAnnouncedBeforeTheirFirstResource covers what keystone sends
// for a tenant that is created inside the month. The classic tenants pre-exist
// it and are announced by nothing, which is what keeps an announcement from
// standing for a project that was there all along.
func TestTenantsAreAnnouncedBeforeTheirFirstResource(t *testing.T) {
	g := buildMonth(t, 1)
	byID := byResource(g.schedule)

	announced := make([]*project, 0, len(g.gardenerProjects)+1)
	for _, gp := range g.gardenerProjects {
		announced = append(announced, gp.tenant)

		if _, ok := has(byID[gp.zoneID], "dns.zone.create", july2026.Add(2*time.Second)); !ok {
			t.Errorf("the project %s publishes no zone at %s, want the one its shoots create their "+
				"records in", gp.name, july2026.Add(2*time.Second))
		}
	}
	announced = append(announced, g.ciTenant)

	if _, ok := has(byID[g.ciTenant.network.id], "network.create.start",
		july2026.Add(2*time.Second)); !ok {
		t.Errorf("the CI tenant builds no network at %s, want the one its runners hold their "+
			"addresses on", july2026.Add(2*time.Second))
	}

	// The first transition of every project, which the announcement of an
	// announced tenant is.
	first := make(map[string]int, len(announced))
	for i, transition := range g.schedule {
		if _, ok := first[transition.ProjectID]; !ok {
			first[transition.ProjectID] = i
		}
	}

	for _, p := range announced {
		if _, ok := has(byID[p.id], "identity.project.created", july2026); !ok {
			t.Errorf("the tenant %s is never announced at %s, want keystone to create it before "+
				"anything runs in it", p.id, july2026)
			continue
		}
		if _, ok := has(byID[p.userID], "identity.user.created", july2026.Add(time.Second)); !ok {
			t.Errorf("the user %s of the tenant %s is never announced at %s, want it a second after "+
				"the project", p.userID, p.id, july2026.Add(time.Second))
		}

		index := -1
		for i, transition := range g.schedule {
			if transition.EventType == "identity.project.created" && transition.ResourceID == p.id {
				index = i
				break
			}
		}
		if earliest := first[p.id]; earliest < index {
			t.Errorf("the tenant %s reports %s at %s before its identity.project.created, want the "+
				"announcement first: nothing runs in a project keystone has not created",
				p.id, g.schedule[earliest].EventType, g.schedule[earliest].At)
		}
	}

	classic := make(map[string]struct{}, len(g.projects))
	for _, p := range g.projects {
		classic[p.id] = struct{}{}
	}
	for _, transition := range g.schedule {
		if !strings.HasPrefix(transition.EventType, "identity.") {
			continue
		}
		if _, ok := classic[transition.ProjectID]; ok {
			t.Errorf("%s at %s runs in the classic tenant %s, want keystone to announce the "+
				"machine-driven tenants alone: a classic project pre-exists the month",
				transition.EventType, transition.At, transition.ProjectID)
		}
	}
}

// TestLoadBalancersGetTheirPortsAndCertificates covers what neutron, designate
// and barbican send around a load balancer. The address of a cluster is
// published once, on the first balancer of the shoot, and the certificate the
// ingress terminates on is four audit records before the update the balancer is
// booked from.
func TestLoadBalancersGetTheirPortsAndCertificates(t *testing.T) {
	g := buildMonth(t, 1)
	byID := byResource(g.schedule)
	byTarget := byAuditTarget(g.schedule)

	secondBalancers := 0
	for _, gp := range g.gardenerProjects {
		for _, s := range gp.shoots {
			tenant := s.owner.tenant
			for index, lb := range s.loadBalancers {
				created := instantOf(g.schedule, "octavia.loadbalancer.create.end", lb.id)
				if created.IsZero() {
					t.Fatalf("the balancer %s is never created, want the service it stands for", lb.name)
				}

				port, ok := has(byID[lb.vipPortID], "port.create.end", created.Add(-time.Second))
				if !ok {
					t.Errorf("the balancer %s holds no port created at %s, want the one it holds its "+
						"VIP address on", lb.name, created.Add(-time.Second))
				} else {
					payload, _ := port.Payload["port"].(map[string]any)
					if got, _ := payload["device_owner"].(string); got != "Octavia" {
						t.Errorf("the VIP port of %s is owned by %q, want \"Octavia\": the device "+
							"behind it is the balancer and not a server", lb.name, got)
					}
				}

				if index > 0 {
					secondBalancers++
					// The lookups are scoped to what this balancer would have
					// published. Two shoots of a project share a tenant, so a search
					// over the month would answer with the other shoot's record and
					// pass while this one's is missing.
					if _, ok := has(byID[s.ingressRecord.id], "dns.recordset.create",
						created.Add(ingressRecordLag)); ok {
						t.Errorf("the balancer %s publishes a record at %s, want the ingress record "+
							"to be published with the first balancer of a shoot alone",
							lb.name, created.Add(ingressRecordLag))
					}
					if lb.secretID != "" || lb.containerID != "" {
						t.Errorf("the balancer %s holds the barbican secret %q and the container "+
							"%q, want neither: it terminates one listener and no TLS",
							lb.name, lb.secretID, lb.containerID)
					}
					continue
				}

				record, ok := has(byID[s.ingressRecord.id], "dns.recordset.create",
					created.Add(ingressRecordLag))
				if !ok {
					t.Errorf("%s publishes no ingress record at %s, want the wildcard its services "+
						"are reached under", s.name, created.Add(ingressRecordLag))
				} else {
					records, _ := record.Payload["records"].([]string)
					if !slices.Equal(records, []string{lb.fip.address}) {
						t.Errorf("the ingress record of %s points at %v, want %v, the address of its "+
							"first balancer", s.name, records, []string{lb.fip.address})
					}
				}

				requireCertificate(t, byTarget, tenant, lb, created.Add(certificateLag), "create",
					[]string{"/v1/secrets", "/v1/secrets", "/v1/containers", "/v1/containers"})
				updated := instantOf(g.schedule, "octavia.loadbalancer.update.end", lb.id)
				if updated.IsZero() || !created.Add(certificateLag+3*time.Second).Before(updated) {
					t.Errorf("the balancer %s is updated at %v and its certificate stored until %s, "+
						"want the certificate first: the service is published once it terminates TLS",
						lb.name, updated, created.Add(certificateLag+3*time.Second))
				}
			}
		}
	}
	if secondBalancers == 0 {
		t.Errorf("no shoot of the month publishes a second service, want api-prod to publish one: " +
			"without it nothing holds the record and the certificate to the first balancer of a shoot")
	}

	t.Run("batch takes its balancers back with their ports and certificates", func(t *testing.T) {
		s := shootNamed(t, g, "batch")
		for index, lb := range s.loadBalancers {
			deleted := instantOf(g.schedule, "octavia.loadbalancer.delete.end", lb.id)
			if deleted.IsZero() {
				t.Fatalf("the balancer %s of batch is never deleted, want the tear-down to close it",
					lb.name)
			}
			if _, ok := has(byID[lb.vipPortID], "port.delete.end", deleted.Add(2*time.Second)); !ok {
				t.Errorf("the VIP port of %s is not released at %s, want it to go with the balancer "+
					"that held it", lb.name, deleted.Add(2*time.Second))
			}
			if index > 0 {
				continue
			}
			requireCertificate(t, byTarget, s.owner.tenant, lb, deleted.Add(3*time.Second), "delete",
				[]string{
					"/v1/containers/" + lb.containerID, "/v1/containers/" + lb.containerID,
					"/v1/secrets/" + lb.secretID, "/v1/secrets/" + lb.secretID,
				})
		}
	})
}

// requireCertificate holds the four records barbican's audit middleware sends
// for the two calls of a certificate against the balancer they belong to: a
// request and a response per call, a second apart, both naming the same action
// and path.
//
// A record is looked up by the resource it names rather than searched for in
// the month. Two shoots of one project run on one tenant, so a record of the
// other shoot at the same second carries the same project, the same action and
// the same path, and a search over the month would take it for this balancer's.
func requireCertificate(t *testing.T, byTarget map[string][]Transition, p *project,
	lb *loadBalancer, from time.Time, action string, paths []string,
) {
	t.Helper()

	// The secret is given the first two records and the container the other two.
	// A certificate is given back the other way round, because the container
	// refers to the secret and goes first.
	targets := []string{lb.secretID, lb.secretID, lb.containerID, lb.containerID}
	if action == "delete" {
		targets = []string{lb.containerID, lb.containerID, lb.secretID, lb.secretID}
	}

	for i, path := range paths {
		eventType := "audit.http.request"
		if i%2 == 1 {
			eventType = "audit.http.response"
		}
		when := from.Add(time.Duration(i) * time.Second)

		record, ok := has(byTarget[targets[i]], eventType, when)
		if !ok {
			t.Errorf("the certificate of %s reports no %s at %s naming %s, want the four records "+
				"of its two calls against barbican", lb.name, eventType, when, targets[i])
			continue
		}
		if record.ProjectID != p.id {
			t.Errorf("the record of %s at %s runs in the project %s, want %s, the tenant the shoot "+
				"runs on", lb.name, when, record.ProjectID, p.id)
		}
		if got, _ := record.Payload["requestPath"].(string); got != path {
			t.Errorf("the record of %s at %s names the path %q, want %q", lb.name, when, got, path)
		}
		if got, _ := record.Payload["action"].(string); got != action {
			t.Errorf("the record of %s at %s reports the action %q, want %q: the method the call was "+
				"made with is what the action is read off", lb.name, when, got, action)
		}
	}
}

// TestGardenerTakesOneTokenPerReconciliation covers every keystone record the
// machine-driven tenants send. A shoot's creation, each of its wake-ups, its
// rolling update, and its tear-down are one reconciliation each, and nothing
// else Gardener does costs a token: the autoscaler and the claim activity take
// none, and a wake-up that took one per worker would put five records on the
// bus where one belongs.
func TestGardenerTakesOneTokenPerReconciliation(t *testing.T) {
	g := buildMonth(t, 1)
	gardener := ofWorkload(g.schedule, workloadGardener)
	to := july2026.AddDate(0, 1, 0)

	// The instants a token is due at, by the tenant that takes it and what the
	// record stands for. A rolling update is drawn inside its day, so it is not
	// among them and is matched afterwards.
	type token struct {
		project string
		at      time.Time
	}
	want := make(map[token]string)
	// The rolling update days, by the shoot that rolls on one.
	rolling := make(map[time.Time]*shoot)

	for _, gp := range g.gardenerProjects {
		for _, s := range gp.shoots {
			want[token{gp.tenant.id, s.createdAt.Add(-authenticateLead)}] =
				"the reconciliation that creates " + s.name
			if !s.deletedAt.IsZero() {
				want[token{gp.tenant.id, s.deletedAt.Add(-authenticateLead)}] =
					"the tear-down of " + s.name
			}
			if !s.rollingUpdateDay.IsZero() {
				rolling[s.rollingUpdateDay] = s
			}
			if !s.hibernates {
				continue
			}
			// A wake-up boots the shoot's first worker of the day at the stroke of
			// the office hour, which is what marks the morning it woke up on.
			for _, d := range workingDays(july2026, to) {
				morning := at(d, officeFrom, 0)
				if _, ok := has(transitionsOf(g.schedule, s), "compute.instance.create.end", morning); ok {
					want[token{gp.tenant.id, morning.Add(-authenticateLead)}] = "the wake-up of " + s.name
				}
			}
		}
	}
	if len(want) < 4 {
		t.Fatalf("the month holds %d reconciliations to hold the tokens against, want the creation "+
			"of every shoot, the wake-ups of api-dev and the tear-down of batch", len(want))
	}

	seen := make(map[token]int)
	for _, transition := range gardener {
		if transition.EventType != "identity.authenticate" {
			continue
		}
		key := token{transition.ProjectID, transition.At}
		if _, ok := want[key]; ok {
			seen[key]++
			continue
		}

		// What is left is a rolling update: one token on the day the shoot rolls
		// on, two seconds before the first machine of the new generation boots.
		s, ok := rolling[at(transition.At, 0, 0)]
		if !ok || s.owner.tenant.id != transition.ProjectID {
			t.Errorf("the tenant %s takes a token at %s, want one per action a controller starts: "+
				"a shoot's creation, a wake-up, a rolling update, and a tear-down",
				transition.ProjectID, transition.At)
			continue
		}
		if _, booted := has(transitionsOf(g.schedule, s), "compute.instance.create.end",
			transition.At.Add(authenticateLead)); !booted {
			t.Errorf("%s takes a token at %s on its rolling update day and boots no machine at %s, "+
				"want the update the token was taken for", s.name, transition.At,
				transition.At.Add(authenticateLead))
		}
		seen[token{transition.ProjectID, s.rollingUpdateDay}]++
	}

	for key, what := range want {
		if seen[key] != 1 {
			t.Errorf("%s reports %d tokens at %s, want exactly one: a reconciliation authenticates "+
				"once, whatever it goes on to create", what, seen[key], key.at)
		}
	}
	for d, s := range rolling {
		if got := seen[token{s.owner.tenant.id, d}]; got != 1 {
			t.Errorf("the rolling update of %s on %s reports %d tokens, want exactly one: the "+
				"machines it replaces are one reconciliation", s.name, d.Format(time.DateOnly), got)
		}
	}
}

// TestImagesArePreparedAndActivatedAroundTheirUpload covers the two
// notifications glance sends around the one the collector books an image from.
// The prepare carries no size yet and the activate repeats the upload, so a
// month that dropped either would leave the image billed from the same instant
// and the bus short of what a real glance sends.
func TestImagesArePreparedAndActivatedAroundTheirUpload(t *testing.T) {
	schedule := generateMonth(t, 1, july2026, testCloud)
	byID := byResource(schedule)

	const want = 9
	uploads := 0
	for _, upload := range schedule {
		if upload.EventType != "image.upload" {
			continue
		}
		uploads++

		id, at := upload.ResourceID, upload.At
		prepared, ok := has(byID[id], "image.prepare", at.Add(-time.Second))
		if !ok {
			t.Errorf("the image %s is uploaded at %s and reports no image.prepare at %s, want glance "+
				"taking the bits in", id, at, at.Add(-time.Second))
		}
		if _, ok := has(byID[id], "image.activate", at.Add(time.Second)); !ok {
			t.Errorf("the image %s is uploaded at %s and reports no image.activate at %s, want "+
				"glance flipping it to active", id, at, at.Add(time.Second))
		}
		announced := instantOf(byID[id], imageCreateType, id)
		switch {
		case announced.IsZero():
			t.Errorf("the image %s is uploaded at %s and never announced, want the %s glance sends "+
				"before it has content", id, at, imageCreateType)
		case ok && !announced.Before(prepared.At):
			t.Errorf("the image %s is announced at %s and prepared at %s, want the announcement "+
				"first: glance accepts an image before it receives its content",
				id, announced, prepared.At)
		}
	}
	if uploads != want {
		t.Errorf("the month uploads %d images, want %d: two per classic tenant and one per "+
			"machine-driven one", uploads, want)
	}
}

// TestCIBurstsAuthenticate covers the token a push takes. A pipeline is started
// by somebody, so a burst costs one authentication and a runner none: a month
// with one per runner would put hundreds of keystone records on the bus that no
// build system sends.
func TestCIBurstsAuthenticate(t *testing.T) {
	for seed := uint64(1); seed <= 5; seed++ {
		t.Run(fmt.Sprintf("seed %d", seed), func(t *testing.T) {
			schedule := ofWorkload(generateMonth(t, seed, july2026, testCloud), workloadCI)

			creates := make(map[time.Time]struct{}, len(schedule))
			for _, transition := range schedule {
				if transition.EventType == "compute.instance.create.end" {
					creates[transition.At] = struct{}{}
				}
			}

			perDay := make(map[time.Time]int)
			bursts := 0
			for _, transition := range schedule {
				if transition.EventType != "identity.authenticate" {
					continue
				}
				bursts++
				perDay[at(transition.At, 0, 0)]++
				if _, ok := creates[transition.At.Add(2*time.Second)]; !ok {
					t.Errorf("the CI tenant takes a token at %s and boots no runner at %s, want a "+
						"burst behind every authentication", transition.At,
						transition.At.Add(2*time.Second))
				}
			}
			if bursts == 0 {
				t.Fatalf("the CI tenant authenticates never, want one token per push")
			}

			for _, d := range workingDays(july2026, july2026.AddDate(0, 1, 0)) {
				if got := perDay[d]; got < 4 || got > 8 {
					t.Errorf("%s brings %d authentications, want 4 to 8: one per burst of the day",
						d.Format(time.DateOnly), got)
				}
			}
		})
	}
}

// TestAuditsStepAsideFromAnOccupiedSecond covers the one instant an audit
// cannot take: a midnight the instance already reports a transition at. A
// generated month reaches it only when a delete happens to fall on a midnight,
// so the two transitions around it are written out here rather than drawn.
func TestAuditsStepAsideFromAnOccupiedSecond(t *testing.T) {
	g := buildWorld(t, 1)
	p := g.projects[0]
	inst := p.instances[0]
	inst.flavor = largeFlavor
	inst.host = computeHosts[0]
	inst.createdAt = time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	deletedAt := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)

	g.schedule = Schedule{
		{
			At: inst.createdAt, EventType: "compute.instance.create.end", Workload: workloadClassic,
			PublisherID: computePublisher(inst), ProjectID: p.id, UserID: p.userID,
			ResourceID: inst.id, Payload: instanceCreatePayload(p, inst, testCloud),
		},
		{
			At: deletedAt, EventType: "compute.instance.delete.end", Workload: workloadClassic,
			PublisherID: computePublisher(inst), ProjectID: p.id, UserID: p.userID,
			ResourceID: inst.id, Payload: instanceDeletePayload(p, inst, deletedAt),
		},
	}
	g.audits()

	var audits []Transition
	for _, transition := range g.schedule {
		if transition.EventType == "compute.instance.exists" && transition.ResourceID == inst.id {
			audits = append(audits, transition)
		}
	}

	want := []time.Time{
		time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 5, 0, 0, 1, 0, time.UTC),
	}
	if len(audits) != len(want) {
		t.Fatalf("the instance is audited %d times, want %d: one for the day it ran through and one "+
			"for the part of the day it was deleted on", len(audits), len(want))
	}
	for i, when := range want {
		if !audits[i].At.Equal(when) {
			t.Errorf("audit %d lies at %s, want %s: a midnight the instance already reports a "+
				"transition at is one the audit is pushed past", i, audits[i].At, when)
		}
		if audits[i].Workload != workloadClassic {
			t.Errorf("audit %d carries the workload %q, want %q, the one of the create it repeats",
				i, audits[i].Workload, workloadClassic)
		}
	}
}

// TestAuditsOverAnEmptyScheduleEmitNothing covers the pass over a month that
// holds no server. The audits walk every midnight of the period whatever the
// schedule carries, so a month without an instance is where the walk would
// otherwise reach into an empty fleet.
func TestAuditsOverAnEmptyScheduleEmitNothing(t *testing.T) {
	t.Run("a schedule that holds nothing", func(t *testing.T) {
		g := buildWorld(t, 1)
		g.audits()

		if len(g.schedule) != 0 {
			t.Errorf("the audits added %d transitions to an empty month, want none: nothing existed "+
				"on any day of it", len(g.schedule))
		}
	})

	t.Run("a schedule that holds no server", func(t *testing.T) {
		g := buildWorld(t, 1)
		p := g.projects[0]
		vol := &volume{
			id: g.noiseIDs.nextUUID(), name: "audited-by-nobody", sizeGB: 10, volumeType: "ssd",
			createdAt: time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC),
		}
		g.schedule = Schedule{{
			At: vol.createdAt, EventType: "volume.create.end", Billable: true,
			PublisherID: volumePublisher, ProjectID: p.id, UserID: p.userID,
			ResourceID: vol.id, Payload: volumeCreatePayload(p, vol),
		}}
		g.audits()

		if len(g.schedule) != 1 {
			t.Errorf("the audits made a month of one volume %d transitions long, want it unchanged: "+
				"a resource without a create.end behind it is nothing this pass audits", len(g.schedule))
		}
	})
}

// TestNoiseHelpersOnAbsentResources covers the branches of noise.go the month
// never reaches: a volume that is attached to no server, one that is deleted
// while nothing holds it, and a server that is destroyed without a port. Each
// of them is a nil the helpers answer for rather than dereference.
func TestNoiseHelpersOnAbsentResources(t *testing.T) {
	t0 := time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC)

	newVolume := func(g *generator) *volume {
		return &volume{
			id: g.noiseIDs.nextUUID(), name: "orphan", sizeGB: 10, volumeType: "ssd",
			createdAt: t0.Add(-time.Hour),
		}
	}

	t.Run("attach with no instance", func(t *testing.T) {
		g := buildWorld(t, 1)
		g.workload = workloadClassic
		p := g.projects[0]
		vol := newVolume(g)

		g.attach(p, vol, nil, t0)

		attached, ok := has(g.schedule, "volume.attach.end", t0.Add(time.Second))
		if !ok {
			t.Fatalf("the attach of %s renders no volume.attach.end at %s", vol.name, t0.Add(time.Second))
		}
		attachment, _ := attached.Payload["volume_attachment"].([]any)
		if len(attachment) != 1 {
			t.Fatalf("the attach reports %d attachments, want the one cinder handed out", len(attachment))
		}
		record, _ := attachment[0].(map[string]any)
		for _, member := range []string{"instance_uuid", "attached_host"} {
			if got := record[member]; got != nil {
				t.Errorf("the attachment reports %s = %v, want null: cinder names neither the server "+
					"nor its compute on an attachment whose server it does not know", member, got)
			}
		}
		if !vol.attached {
			t.Errorf("the volume is attached = false, want it in use: a billable payload rendered " +
				"afterwards reports it the way it reports one a server holds")
		}
		if got := volumeStatePayload(p, vol)["status"]; got != "in-use" {
			t.Errorf("the volume reports status = %v, want \"in-use\"", got)
		}
	})

	t.Run("deleteVolume on a detached volume", func(t *testing.T) {
		g := buildWorld(t, 1)
		g.workload = workloadClassic
		p := g.projects[0]

		g.deleteVolume(p, newVolume(g), t0)

		requireSequence(t, g.schedule, []sequenceStep{
			{"volume.delete.start", t0.Add(-time.Second)},
			{"volume.delete.end", t0},
		})
		for _, transition := range g.schedule {
			if strings.HasPrefix(transition.EventType, "volume.detach.") {
				t.Errorf("the delete renders %s at %s, want none: a volume whose server is already "+
					"gone was detached with it", transition.EventType, transition.At)
			}
		}
	})

	t.Run("deleteVolume on an attached volume", func(t *testing.T) {
		g := buildWorld(t, 1)
		g.workload = workloadClassic
		p := g.projects[0]
		vol := newVolume(g)

		g.attach(p, vol, nil, t0.Add(-time.Hour))
		g.schedule = nil
		g.deleteVolume(p, vol, t0)

		requireSequence(t, g.schedule, []sequenceStep{
			{"volume.detach.start", t0.Add(-3 * time.Second)},
			{"volume.detach.end", t0.Add(-2 * time.Second)},
			{"volume.delete.start", t0.Add(-time.Second)},
			{"volume.delete.end", t0},
		})
	})

	t.Run("destroyInstance without a port", func(t *testing.T) {
		g := buildWorld(t, 1)
		g.workload = workloadClassic
		p := g.projects[0]
		inst := &instance{
			id: g.noiseIDs.nextUUID(), name: "lonely", host: computeHosts[0], flavor: largeFlavor,
			createdAt: t0.Add(-time.Hour),
		}

		g.destroyInstance(p, inst, t0)

		requireSequence(t, g.schedule, []sequenceStep{
			{"compute.instance.update", t0.Add(-5 * time.Second)},
			{"compute.instance.exists", t0.Add(-4 * time.Second)},
			{"compute.instance.delete.start", t0.Add(-3 * time.Second)},
			{"compute.instance.shutdown.start", t0.Add(-2 * time.Second)},
			{"compute.instance.shutdown.end", t0.Add(-time.Second)},
			{"compute.instance.delete.end", t0},
		})
		for _, transition := range g.schedule {
			if strings.HasPrefix(transition.EventType, "port.") {
				t.Errorf("the delete renders %s at %s, want none: a server that never held a port "+
					"has none to release", transition.EventType, transition.At)
			}
		}
		if deleted, ok := has(g.schedule, "compute.instance.delete.end", t0); ok && !deleted.Billable {
			t.Errorf("the delete is billable = false, want the collector to book it: the noise around " +
				"it is what it bills nothing for")
		}
		if !inst.deletedAt.Equal(t0) {
			t.Errorf("the instance reports deletedAt = %s, want %s", inst.deletedAt, t0)
		}
	})
}

// sequenceStep is one transition a rendered sequence is to hold: which type,
// and the instant it is due at.
type sequenceStep struct {
	eventType string
	at        time.Time
}

// requireSequence holds a schedule against the transitions it is to hold, in
// order and at the instants they are due at.
func requireSequence(t *testing.T, schedule Schedule, want []sequenceStep) {
	t.Helper()

	if len(schedule) != len(want) {
		t.Fatalf("the schedule holds %d transitions, want %d", len(schedule), len(want))
	}
	for i, step := range want {
		if schedule[i].EventType != step.eventType || !schedule[i].At.Equal(step.at) {
			t.Errorf("transition %d is %s at %s, want %s at %s",
				i, schedule[i].EventType, schedule[i].At, step.eventType, step.at)
		}
	}
}

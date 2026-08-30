package simulator

import (
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
)

// The six fault switches, under the names --faults takes them by. A switch
// changes what the bus carries and never what the simulated cloud did, so the
// oracle of a month states the same usage whichever switches are on.
const (
	FaultPreExisting   = "pre-existing"
	FaultMissingCreate = "missing-create"
	FaultDuplicates    = "duplicates"
	FaultReordering    = "reordering"
	FaultRefusedShapes = "refused-shapes"
	FaultHeldBack      = "held-back"
)

// FaultNames holds the six switches in the order everything that lists them
// uses: the help of the flag, the switches a run logs, and the switches the
// oracle names per resource.
var FaultNames = []string{
	FaultPreExisting,
	FaultMissingCreate,
	FaultDuplicates,
	FaultReordering,
	FaultRefusedShapes,
	FaultHeldBack,
}

// Faults is the set of switches one run turns on, one field per name in
// FaultNames. The zero value is every switch off, which is the run nobody
// passed --faults to.
type Faults struct {
	PreExisting, MissingCreate, Duplicates, Reordering, RefusedShapes, HeldBack bool
}

// ParseFaults reads the switch names --faults was given. An empty list is every
// switch off, and a name given twice is the run a name given once is.
//
// pre-existing and missing-create are refused together. Both of them pick the
// instances they work on from the same set, so a run with both on would have
// the two switches compete for the same instances.
func ParseFaults(names []string) (Faults, error) {
	var faults Faults
	for _, name := range names {
		switch name {
		case FaultPreExisting:
			faults.PreExisting = true
		case FaultMissingCreate:
			faults.MissingCreate = true
		case FaultDuplicates:
			faults.Duplicates = true
		case FaultReordering:
			faults.Reordering = true
		case FaultRefusedShapes:
			faults.RefusedShapes = true
		case FaultHeldBack:
			faults.HeldBack = true
		default:
			return Faults{}, fmt.Errorf("unknown fault switch %q; the switches are %s",
				name, strings.Join(FaultNames, ", "))
		}
	}
	if faults.PreExisting && faults.MissingCreate {
		return Faults{}, errors.New("pre-existing and missing-create exclude each other")
	}
	return faults, nil
}

// Names returns the switches that are on, in FaultNames order. It is never nil:
// the names go into the oracle of the month, and a run with no switch on has to
// render there as [] rather than as null.
func (f Faults) Names() []string {
	on := []bool{f.PreExisting, f.MissingCreate, f.Duplicates, f.Reordering, f.RefusedShapes, f.HeldBack}
	names := make([]string, 0, len(FaultNames))
	for i, name := range FaultNames {
		if on[i] {
			names = append(names, name)
		}
	}
	return names
}

// sortedFaultNames returns the names in FaultNames order, with a name outside
// FaultNames behind every switch and in the order it arrived in. It leaves the
// slice it was given as it stands.
//
// It exists because an oracle read from disk states the switches in whatever
// order the document spells them, and the lines a comparison prints name them
// in the one order everything else does.
func sortedFaultNames(names []string) []string {
	order := func(name string) int {
		if i := slices.Index(FaultNames, name); i >= 0 {
			return i
		}
		return len(FaultNames)
	}
	sorted := slices.Clone(names)
	slices.SortStableFunc(sorted, func(a, b string) int { return order(a) - order(b) })
	return sorted
}

// faultSalt mixes a switch's name into the state of its stream, the way
// identifierSalt (workload.go) mixes the cloud and the billing month into the
// identifier stream's.
func faultSalt(name string) uint64 {
	digest := fnv.New64a()
	// A hash.Hash never fails a write, which is why the error is not examined.
	_, _ = digest.Write([]byte("fault\x00" + name))
	return digest.Sum64()
}

// faultStream is the generator one switch draws from: the seed of the run
// together with the switch's own name.
//
// Every switch draws from the stream of its own name and never from the shape
// stream. A month with every switch off therefore consumes the three streams of
// GenerateMonth exactly as it does without the switches and renders
// byte-identical files, and which resources or notifications one switch touches
// does not move when another switch is on beside it.
//
// missing-create draws from the pre-existing stream on purpose. The two exclude
// each other, and one stream between them means both pick the same instances
// with the same leads for one seed.
//
// A fault stream carries neither the cloud nor the billing month. The
// identifier stream carries both because a collector stores what it draws, and
// a fault stream draws no such identifier: the one it hands out is the message
// id of a refused twin, which no collector stores.
func faultStream(seed uint64, name string) *rand.Rand {
	return rand.New(rand.NewPCG(seed, faultSalt(name)))
}

// touchedResources records which switches touched which resource of a month. It
// is keyed the way the oracle keys a resource, so the switches of one are read
// out by its type and its id together.
type touchedResources map[resourceKey]map[string]bool

// add records that the switch touched the resource. One switch added twice is
// the same as one added once: what a reader asks is which switches touched a
// resource, not how often each of them did.
func (t touchedResources) add(key resourceKey, fault string) {
	if t[key] == nil {
		t[key] = make(map[string]bool)
	}
	t[key][fault] = true
}

// names returns the switches that touched the resource, in FaultNames order. It
// is never nil, for the reason Faults.Names is not: a resource no switch
// touched renders as [] rather than as null.
func (t touchedResources) names(key resourceKey) []string {
	touched := t[key]
	names := make([]string, 0, len(touched))
	for _, name := range FaultNames {
		if touched[name] {
			names = append(names, name)
		}
	}
	return names
}

// touch records that the switch touched the resource the type and the id name.
// It is the one call the generator records a touched resource through, so no
// caller builds the key itself.
func (g *generator) touch(resourceType, resourceID, fault string) {
	g.touched.add(resourceKey{resourceType: resourceType, resourceID: resourceID}, fault)
}

// What the two pre-existing switches draw: one in preExistingShare of the
// classic tenants' instances starts before the month, between
// preExistingLeadMin and preExistingLeadMax before it begins.
//
// The share is a package constant rather than a flag, because a drill wants a
// known effect it can reproduce and not one it dials in.
const (
	preExistingShare   = 3
	preExistingLeadMin = day
	preExistingLeadMax = 30 * day
)

// What the four switches that change the publication stream draw. The shares
// are package constants rather than flags, for the reason the pre-existing
// share is one: a drill wants a known effect it can reproduce.
const (
	// heldBackShare is what is held back: one in 20 billable transitions.
	heldBackShare = 20
	// reorderShare is what is reordered: one in 10 of the resources with two
	// billable transitions has its first two published the wrong way round.
	reorderShare = 10
	// duplicateShare is what is repeated: one in 20 billable transitions is
	// published twice.
	duplicateShare = 20
	// duplicateDistance is how many notifications of the stream stand between a
	// duplicate and the transition it repeats.
	duplicateDistance = 10
	// refusedShapeDraw is the denominator of the three refused shapes: one draw
	// in it picks an oversized twin, two a truncated one, and twenty a
	// versioned one.
	refusedShapeDraw = 400
	// oversizedPadding is how much padding the oversized twin carries, which
	// puts its body past what the collector reads. It repeats bodyMax in
	// internal/providers/openstack/osloamqp.go, which is unexported and stays
	// so, the way collectorQueue in publish.go repeats the collector's queue
	// name.
	oversizedPadding = 1 << 20
)

// novaExchange is the exchange the versioned twin is published on. The
// versioned format is nova's, so no other service has a twin of that shape.
const novaExchange = "nova"

// applyStreamFaults builds the stream a run publishes out of the schedule the
// simulated cloud produced, and returns the transitions held-back keeps off the
// bus beside it.
//
// The schedule is left as it stands: every transition once, sorted by its
// instant. That is what WriteEvents and the oracle read, so neither of them
// knows about the duplicates and the refused twins, which are in the stream
// alone. A held transition stays in the schedule, because the collector records
// it once the run releases it.
//
// The four passes run in the order held-back, reordering, duplicates,
// refused-shapes, and each of them draws its picks by walking the schedule's
// billable transitions with the stream of its own name. What one switch picks
// therefore follows from the seed and the switch alone, and turning another
// switch on beside it moves nothing. A pick is keyed by the message id, and a
// pass applies it to the first transition of that id it meets in the stream it
// is transforming: a transition an earlier pass copied is twinned once, and a
// pick whose transition is held never applies.
func applyStreamFaults(schedule Schedule, seed uint64, faults Faults, touched touchedResources) (stream, held Schedule) {
	stream = slices.Clone(schedule)
	held = Schedule{}
	if faults.HeldBack {
		stream, held = holdBack(stream, schedule, seed, touched)
	}
	if faults.Reordering {
		stream = reorder(stream, schedule, seed, held, touched)
	}
	if faults.Duplicates {
		stream = duplicate(stream, schedule, seed, touched)
	}
	if faults.RefusedShapes {
		stream = refuseShapes(stream, schedule, seed, touched)
	}
	return stream, held
}

// billableKey names the resource the oracle records a billable transition
// under. Every transition a stream pass picks comes from Schedule.Billable, so
// its type is in the table.
func billableKey(t Transition) resourceKey {
	return resourceKey{resourceType: billableTypes[t.EventType].resourceType, resourceID: t.ResourceID}
}

// holdBack keeps one in heldBackShare of the billable transitions off the bus
// and hands them back beside the rest. They come back in the order of the
// stream they were taken out of, which is instant order, so a run releases them
// in the order the cloud produced them.
func holdBack(stream, schedule Schedule, seed uint64, touched touchedResources) (kept, held Schedule) {
	draw := faultStream(seed, FaultHeldBack)
	picked := make(map[string]bool)
	for _, transition := range schedule.Billable() {
		if draw.IntN(heldBackShare) == 0 {
			picked[transition.MessageID] = true
		}
	}

	kept = make(Schedule, 0, len(stream))
	held = make(Schedule, 0, len(picked))
	for _, transition := range stream {
		if !picked[transition.MessageID] {
			kept = append(kept, transition)
			continue
		}
		delete(picked, transition.MessageID)
		held = append(held, transition)
		touched.add(billableKey(transition), FaultHeldBack)
	}
	return kept, held
}

// reorder publishes the first billable transition of one in reorderShare of the
// resources directly behind its second. The instants do not change, so the
// notification about what happened first arrives second, which is the order the
// projection and the engine have to undo.
//
// A resource with one billable transition is never drawn, because it has no
// pair to swap. A pair one of whose members held-back is holding is left where
// it stands: moving a transition behind one that is not on the bus is no
// reordering.
func reorder(stream, schedule Schedule, seed uint64, held Schedule, touched touchedResources) Schedule {
	holding := make(map[string]bool, len(held))
	for _, transition := range held {
		holding[transition.MessageID] = true
	}

	// moves maps the message id of a first transition to the id of the second
	// its notification is published behind.
	draw := faultStream(seed, FaultReordering)
	counts := make(map[string]int)
	firsts := make(map[string]Transition)
	moves := make(map[string]string)
	for _, transition := range schedule.Billable() {
		counts[transition.ResourceID]++
		switch counts[transition.ResourceID] {
		case 1:
			firsts[transition.ResourceID] = transition
		case 2:
			if draw.IntN(reorderShare) != 0 {
				continue
			}
			first := firsts[transition.ResourceID]
			if holding[first.MessageID] || holding[transition.MessageID] {
				continue
			}
			moves[first.MessageID] = transition.MessageID
			touched.add(billableKey(first), FaultReordering)
		}
	}

	out := make(Schedule, 0, len(stream))
	waiting := make(map[string]Transition, len(moves))
	for _, transition := range stream {
		if second, ok := moves[transition.MessageID]; ok {
			delete(moves, transition.MessageID)
			waiting[second] = transition
			continue
		}
		out = append(out, transition)
		if first, ok := waiting[transition.MessageID]; ok {
			delete(waiting, transition.MessageID)
			out = append(out, first)
		}
	}
	return out
}

// duplicate publishes one in duplicateShare of the billable transitions a
// second time, byte for byte and duplicateDistance notifications later. The
// copy carries the message id of the original, which is what the Reporting API
// deduplicates on, so a collector that consumes both records one event.
func duplicate(stream, schedule Schedule, seed uint64, touched touchedResources) Schedule {
	draw := faultStream(seed, FaultDuplicates)
	picked := make(map[string]bool)
	for _, transition := range schedule.Billable() {
		if draw.IntN(duplicateShare) == 0 {
			picked[transition.MessageID] = true
		}
	}

	out := make(Schedule, 0, len(stream)+len(picked))
	for i, transition := range stream {
		out = append(out, transition)
		if picked[transition.MessageID] {
			touched.add(billableKey(transition), FaultDuplicates)
		}
		// The distance is counted over the transitions of the input rather than
		// over what goes out, so one copy does not push the next one further away.
		if i >= duplicateDistance && picked[stream[i-duplicateDistance].MessageID] {
			out = append(out, stream[i-duplicateDistance])
		}
	}
	// A transition picked in the last duplicateDistance of the month has no room
	// left for its distance, and its copy goes out behind the last notification.
	for _, transition := range stream[max(0, len(stream)-duplicateDistance):] {
		if picked[transition.MessageID] {
			out = append(out, transition)
		}
	}
	return out
}

// twinBuilder turns a transition into the refused twin that follows it on the
// bus. The twin is handed the message id it publishes under, because the id is
// drawn when the twin is placed rather than when it is picked.
type twinBuilder func(t Transition, messageID string) Transition

// refuseShapes puts a twin of a billable transition on the bus directly behind
// it: one draw in refusedShapeDraw picks an oversized twin, two a truncated
// one, and twenty a versioned one. The versioned twin is drawn for nova alone,
// since the versioned format is nova's.
//
// The collector refuses all three. It counts the oversized and the truncated
// one as unparseable, because the first is past its body bound and the second
// carries half a document, and the versioned one as skipped, because the
// mapping table claims nothing for the type. A run that publishes them
// therefore expects the very events it expects without them.
func refuseShapes(stream, schedule Schedule, seed uint64, touched touchedResources) Schedule {
	draw := faultStream(seed, FaultRefusedShapes)
	picked := make(map[string]twinBuilder)
	for _, transition := range schedule.Billable() {
		switch d := draw.IntN(refusedShapeDraw); {
		case d == 0:
			picked[transition.MessageID] = oversizedTwin
		case d <= 2:
			picked[transition.MessageID] = truncatedTwin
		case d <= 22 && transition.Exchange == novaExchange:
			picked[transition.MessageID] = versionedTwin
		}
	}

	// The twins take their message ids from the switch's own stream, behind the
	// draws that picked them. No collector stores such an id: it names a
	// notification nothing is recorded for.
	ids := idReader{src: draw}
	out := make(Schedule, 0, len(stream)+len(picked))
	for _, transition := range stream {
		out = append(out, transition)
		build, ok := picked[transition.MessageID]
		if !ok {
			continue
		}
		delete(picked, transition.MessageID)
		out = append(out, build(transition, ids.nextUUID()))
		touched.add(billableKey(transition), FaultRefusedShapes)
	}
	return out
}

// refusedTwin is the copy the three shapes are built from: the original with a
// message id of its own, billable to nobody, and outside the noise catalogue.
// It keeps the instant, the exchange, and the addressing of the original, so
// the twin travels the route the notification it follows travels.
func refusedTwin(t Transition, messageID string) Transition {
	t.MessageID = messageID
	t.Billable = false
	t.noise = false
	return t
}

// versionedTwin is the notification a nova configured for versioned
// notifications would have sent instead. It carries another type name and wraps
// the payload in nova_object.data, which is the format
// docs/openstack-collector.md refuses under "Required OpenStack service
// settings": ParseEnvelope reads it, and the mapping table claims nothing for
// the type, so the collector counts it as skipped.
func versionedTwin(t Transition, messageID string) Transition {
	twin := refusedTwin(t, messageID)
	twin.EventType = versionedType(t.EventType)
	twin.PublisherID = "nova-compute:" + strings.TrimPrefix(t.PublisherID, "compute.")

	// The versioned payload names the server uuid where the unversioned one
	// names it instance_id.
	data := maps.Clone(t.Payload)
	if id, ok := data["instance_id"]; ok {
		delete(data, "instance_id")
		data["uuid"] = id
	}
	twin.Payload = map[string]any{
		"nova_object.name":      "InstanceActionPayload",
		"nova_object.namespace": "nova",
		"nova_object.version":   "1.8",
		"nova_object.data":      data,
	}
	return twin
}

// versionedType is the name nova publishes an unversioned compute type under
// when it is configured for versioned notifications. The verb of a finished
// resize is the other way round there, so compute.instance.finish_resize.end
// becomes instance.resize_finish.end.
func versionedType(eventType string) string {
	name := strings.TrimPrefix(eventType, "compute.instance.")
	return "instance." + strings.Replace(name, "finish_resize", "resize_finish", 1)
}

// truncatedTwin is a notification that arrives cut in half. Render cuts the
// inner message of a transition marked this way, and the collector's second
// decode fails on what is left, which it counts as unparseable.
func truncatedTwin(t Transition, messageID string) Transition {
	twin := refusedTwin(t, messageID)
	twin.truncated = true
	return twin
}

// oversizedTwin is a notification padded past the body bound the collector
// reads with. The collector measures the delivery before it parses it, so this
// twin is counted as unparseable without ever being decoded.
func oversizedTwin(t Transition, messageID string) Transition {
	twin := refusedTwin(t, messageID)
	twin.Payload = maps.Clone(t.Payload)
	twin.Payload["fault_padding"] = strings.Repeat("x", oversizedPadding)
	return twin
}

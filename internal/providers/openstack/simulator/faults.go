package simulator

import (
	"errors"
	"fmt"
	"hash/fnv"
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

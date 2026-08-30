package simulator

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
)

// The fact ledger is what the oracle of a month is folded from. The generator
// states the effect of every billable transition at the moment it emits it,
// while it still holds the resource the transition changed: which state the
// resource is in from that instant, the whole size object that describes it, or
// that the resource is gone.
//
// The effect is stated here rather than read back from the rendered
// notification because the rendering is one of the things the comparison is
// about. A payload that lost a member renders a notification the collector maps
// to a size nobody meant, and a ledger built from that notification would agree
// with it.
//
// The vocabulary is the one the collector's mapping records rather than the one
// the emitting service uses: nova reports a stopped server, the mapping books it
// as shutoff, and shutoff is what the ledger says.

// effect is what one billable transition leaves behind on its resource.
type effect struct {
	// booked reports whether the collector's mapping records an event for the
	// transition. It is false on the unsized image.create alone, which the
	// mapping skips because the image has no content yet.
	booked bool
	// ended marks a delete, which carries neither a state nor a size: the core
	// sets the state of a delete itself, and a resource that is gone has no size.
	ended bool
	// state is the state the resource is in from this instant on.
	state string
	// size is the resource's whole size object, not the part of it the
	// transition changed.
	size map[string]any
}

// alive is the effect of a transition that leaves its resource in place.
func alive(state string, size map[string]any) effect {
	return effect{booked: true, state: state, size: size}
}

// deleted is the effect of a delete.
var deleted = effect{booked: true, ended: true}

// unbooked is the effect of a transition the mapping records nothing for.
var unbooked effect

// The states a resource is booked under. They are the normalized ones of
// osmap.VMState and the fixed ones the mapping sets per event type, which is
// why a shelved server is shelved here and not shelved_offloaded.
const (
	stateActive    = "active"
	stateShutoff   = "shutoff"
	stateShelved   = "shelved"
	stateResized   = "resized"
	stateAvailable = "available"
	stateInUse     = "in-use"
)

// volumeStateOf returns the state cinder reports a volume under: in use while a
// server holds it, available once nothing does.
func volumeStateOf(vol *volume) string {
	if vol.attached {
		return stateInUse
	}
	return stateAvailable
}

// mebibytesPerGibibyte and bytesPerGibibyte are the divisors a reported memory
// and a reported image size are converted with. They are the ones osmap holds
// for the collector, repeated here so that the ledger converts without the code
// it is the oracle for.
var (
	mebibytesPerGibibyte = decimal.NewFromInt(1024)
	bytesPerGibibyte     = decimal.NewFromInt(1 << 30)
)

// Every number a size builder below puts into its object is a json.Number. A
// size travels through encoding/json on its way into an event and is read back
// under Decoder.UseNumber, where a Go int would have arrived as a float64. The
// literal is built from an integer or from a decimal's own rendering, so a
// quotient such as 0.5 keeps its digits.

// instanceSizeOf describes a server by the flavor it runs on: the vcpus, the
// memory in gibibytes, the sum of the two disks nova reports separately, and
// the flavor's name.
func instanceSizeOf(f flavor) map[string]any {
	ramGB := money.Div(decimal.NewFromInt(int64(f.memoryMB)), mebibytesPerGibibyte)
	return map[string]any{
		"vcpus":   json.Number(strconv.Itoa(f.vcpus)),
		"ram_gb":  json.Number(ramGB.String()),
		"disk_gb": json.Number(strconv.Itoa(f.rootGB + f.ephemeralGB)),
		"flavor":  f.name,
	}
}

// volumeSizeOf describes a volume by its size in gibibytes and its cinder type.
func volumeSizeOf(vol *volume) map[string]any {
	return map[string]any{
		"size_gb": json.Number(strconv.Itoa(vol.sizeGB)),
		"type":    vol.volumeType,
	}
}

// imageSizeOf describes an image. Glance reports bytes, and every image of a
// month is a whole number of quarter gibibytes, so the quotient is exact.
func imageSizeOf(img *image) map[string]any {
	return map[string]any{
		"size_gb": json.Number(money.Div(decimal.NewFromInt(img.size), bytesPerGibibyte).String()),
	}
}

// floatingIPSizeOf describes an address, whose one billable property is which
// protocol it is an address of. Every address of a month comes from
// floatingPrefix, which is an IPv4 range.
func floatingIPSizeOf() map[string]any {
	return map[string]any{"ip_version": json.Number("4")}
}

// loadBalancerSizeOf describes a load balancer by the two counts its registered
// size schema requires.
func loadBalancerSizeOf(listeners, pools int) map[string]any {
	return map[string]any{
		"listeners": json.Number(strconv.Itoa(listeners)),
		"pools":     json.Number(strconv.Itoa(pools)),
	}
}

// fact is one booked transition and the effect it had. The generator appends
// one per emit whose effect is booked, in the order the month is generated in.
type fact struct {
	at         time.Time
	eventType  string
	resourceID string
	projectID  string
	workload   string
	effect     effect
}

// billableType is what the collector's mapping makes of one oslo type: the
// resource type the event is booked under and the Tally event type the
// notification becomes.
type billableType struct {
	resourceType string
	eventType    string
}

// billableTypes names every oslo type the simulator emits and the mapping
// records an event for. Each row is the resource type first and the Tally event
// type second.
//
// The table repeats what mappings in internal/providers/openstack/mapping.go
// says, and it does so on purpose: an oracle that read the mapping would state
// the month the collector believes in rather than the month the generator
// built, and a renamed event type would still agree with itself. What keeps the
// two from drifting apart is TestOracleAgreesWithTheMapping, which holds every
// row here against the mapping's own.
var billableTypes = map[string]billableType{
	"compute.instance.create.end":         {"instance", "compute.instance.create.end"},
	"compute.instance.delete.end":         {"instance", "compute.instance.delete.end"},
	"compute.instance.resize.end":         {"instance", "compute.instance.resize.end"},
	"compute.instance.finish_resize.end":  {"instance", "compute.instance.resize.end"},
	"compute.instance.shelve_offload.end": {"instance", "compute.instance.shelve"},
	"compute.instance.unshelve.end":       {"instance", "compute.instance.unshelve"},
	"compute.instance.power_off.end":      {"instance", "compute.instance.power_off"},
	"compute.instance.power_on.end":       {"instance", "compute.instance.power_on"},

	"volume.create.end":          {"volume", "volume.create.end"},
	"volume.delete.end":          {"volume", "volume.delete.end"},
	"volume.resize.end":          {"volume", "volume.resize.end"},
	"volume.retype":              {"volume", "volume.retype"},
	"volume.transfer.accept.end": {"volume", "volume.transfer.accept.end"},

	"floatingip.create.end": {"floating_ip", "floatingip.create.end"},
	"floatingip.delete.end": {"floating_ip", "floatingip.delete.end"},

	"image.upload": {"image", "image.create"},
	"image.delete": {"image", "image.delete"},

	"octavia.loadbalancer.create.end": {"loadbalancer", "octavia.loadbalancer.create.end"},
	"octavia.loadbalancer.update.end": {"loadbalancer", "octavia.loadbalancer.update.end"},
	"octavia.loadbalancer.delete.end": {"loadbalancer", "octavia.loadbalancer.delete.end"},
}

// oracleFormat is the format of the document this build writes and reads. It
// is what tells an oracle of this build from one another build wrote: the two
// things a comparison holds an oracle and an export together by, the cloud and
// the period, both pass for an oracle of the same month folded by a generator
// that has since gained a billable transition or a size member, and every
// resource that changed would then be reported as a difference the engine did
// not cause.
//
// Whoever changes what the generator books, what a size holds, or what this
// document states raises the number, and an oracle written before that change
// is refused rather than compared. What holds them to it is
// TestOracleFormatCoversTheGeneratorsBookedSurface, which fails on a booked
// transition, a size member, a member of this document or a state the number
// was not raised for.
//
// The guard runs both ways: ReadOracle refuses a document of another format,
// and DisallowUnknownFields refuses one that states a member this build does
// not read.
const oracleFormat = 1

// Oracle is the generator's statement of what a month contained: for every
// billable resource the intervals of constant state, size and project it
// intended, clipped to the month, and the count of events it expects the
// collector to record per project and Tally event type.
type Oracle struct {
	Format     int              `json:"format"`
	Cloud      string           `json:"cloud"`
	Seed       uint64           `json:"seed"`
	PeriodFrom time.Time        `json:"period_from"`
	PeriodTo   time.Time        `json:"period_to"`
	Resources  []OracleResource `json:"resources"`
	Counts     []OracleCount    `json:"counts"`
}

// OracleResource is one billable resource of the month and the intervals it
// was meant to be billed over, ordered by their start.
type OracleResource struct {
	ResourceType string           `json:"resource_type"`
	ResourceID   string           `json:"resource_id"`
	Workload     string           `json:"workload"`
	Intervals    []OracleInterval `json:"intervals"`
}

// OracleInterval is a half-open span [From, To) over which a resource's state,
// size and project did not change. Both ends lie inside the month.
type OracleInterval struct {
	From      time.Time      `json:"from"`
	To        time.Time      `json:"to"`
	State     string         `json:"state"`
	ProjectID string         `json:"project_id"`
	Size      map[string]any `json:"size"`
}

// OracleCount is how many events of one Tally event type the month expects a
// project to have recorded.
type OracleCount struct {
	ProjectID string `json:"project_id"`
	EventType string `json:"event_type"`
	Count     int    `json:"count"`
}

// resourceKey names one resource of the month. The id alone would not do: two
// services hand out identifiers of their own, and the projection keys a
// resource by its type and its id together.
type resourceKey struct {
	resourceType string
	resourceID   string
}

// countKey is what an expected event count is keyed by: the project the event
// was recorded in and the Tally event type it carries.
type countKey struct {
	projectID string
	eventType string
}

// buildOracle folds the fact ledger into the oracle of the month [from, to).
//
// The facts of one resource are read in the order they happened in. A live fact
// opens an interval; a live fact that repeats the state, project and size of
// the open one changes nothing; any other live fact closes the open interval
// and opens the next; a delete closes the open interval and opens nothing. A
// closed interval that carries no length is dropped, the way the engine's fold
// drops one. Two facts of one resource at the same instant are the only thing
// that would produce such an interval, and they are refused instead: that is a
// pair the projection cannot order, so no fold may claim to know what it
// means.
//
// Every interval is then clipped to the month: a start before from becomes
// from, and an end after to, or an interval still open at the last fact,
// becomes to. Every instant a month emits lies inside the month it was
// generated for, so the clip changes nothing today. It is written out because
// the resources that already exist when a month begins start before it.
//
// The counts are the events the collector is expected to record: one per booked
// fact, under the project the request ran in and the Tally event type the
// mapping gives its oslo type. The transfer of the spare volume therefore
// counts under the accepting project.
func buildOracle(facts []fact, seed uint64, cloud string, from, to time.Time) (Oracle, error) {
	grouped := make(map[resourceKey][]fact)
	counts := make(map[countKey]int)

	for _, f := range facts {
		billable, ok := billableTypes[f.eventType]
		if !ok {
			return Oracle{}, fmt.Errorf("the oracle knows no resource type for %s", f.eventType)
		}
		key := resourceKey{resourceType: billable.resourceType, resourceID: f.resourceID}
		grouped[key] = append(grouped[key], f)
		counts[countKey{projectID: f.projectID, eventType: billable.eventType}]++
	}

	resources := make([]OracleResource, 0, len(grouped))
	for key, group := range grouped {
		slices.SortStableFunc(group, func(a, b fact) int { return a.at.Compare(b.at) })
		for i := 1; i < len(group); i++ {
			if group[i].at.Equal(group[i-1].at) {
				return Oracle{}, fmt.Errorf(
					"%s %s reports two billable transitions at %s, which the projection cannot order",
					key.resourceType, key.resourceID, group[i].at.UTC().Format(time.RFC3339))
			}
		}

		intervals := clipIntervals(foldFacts(group), from, to)
		// A resource that lived entirely outside the month is nothing the month
		// bills, so it is left out rather than stated with no interval.
		if len(intervals) == 0 {
			continue
		}
		resources = append(resources, OracleResource{
			ResourceType: key.resourceType,
			ResourceID:   key.resourceID,
			Workload:     group[0].workload,
			Intervals:    intervals,
		})
	}
	slices.SortFunc(resources, func(a, b OracleResource) int {
		if c := strings.Compare(a.ResourceType, b.ResourceType); c != 0 {
			return c
		}
		return strings.Compare(a.ResourceID, b.ResourceID)
	})

	return Oracle{
		Format:     oracleFormat,
		Cloud:      cloud,
		Seed:       seed,
		PeriodFrom: from,
		PeriodTo:   to,
		Resources:  resources,
		Counts:     sortedCounts(counts),
	}, nil
}

// foldFacts turns one resource's facts, ordered by their instant, into the
// intervals they imply. An interval still open after the last fact comes back
// with a zero To, which is what the clip reads as "to the end of the month".
func foldFacts(group []fact) []OracleInterval {
	var (
		intervals []OracleInterval
		open      OracleInterval
		isOpen    bool
	)

	for _, f := range group {
		// A deleted resource accrues nothing, so a delete produces no interval of
		// its own. A delete with nothing open is a resource that was already gone.
		if f.effect.ended {
			if isOpen {
				intervals = appendClosed(intervals, open, f.at)
				isOpen = false
			}
			continue
		}

		next := OracleInterval{
			From:      f.at,
			State:     f.effect.state,
			ProjectID: f.projectID,
			Size:      f.effect.size,
		}
		if !isOpen {
			open, isOpen = next, true
			continue
		}
		// A fact that restates the open interval is passed over: an event that
		// changed nothing the month is billed by opens no interval of its own,
		// which is how the engine's fold reads one too.
		if next.State == open.State && next.ProjectID == open.ProjectID && maps.Equal(next.Size, open.Size) {
			continue
		}
		intervals = appendClosed(intervals, open, f.at)
		open = next
	}

	if isOpen {
		intervals = append(intervals, open)
	}
	return intervals
}

// appendClosed closes an interval at end and keeps it, unless it would carry no
// length.
func appendClosed(intervals []OracleInterval, open OracleInterval, end time.Time) []OracleInterval {
	if !end.After(open.From) {
		return intervals
	}
	open.To = end
	return append(intervals, open)
}

// clipIntervals cuts the intervals of one resource down to the part of them
// that lies in [from, to). An interval left with no length is dropped. The
// intervals arrive ordered by their start, and cutting a start forward to from
// keeps them that way, so the result is ordered too.
func clipIntervals(intervals []OracleInterval, from, to time.Time) []OracleInterval {
	clipped := make([]OracleInterval, 0, len(intervals))
	for _, iv := range intervals {
		if iv.From.Before(from) {
			iv.From = from
		}
		if iv.To.IsZero() || iv.To.After(to) {
			iv.To = to
		}
		if !iv.To.After(iv.From) {
			continue
		}
		clipped = append(clipped, iv)
	}
	return clipped
}

// sortedCounts turns the counted facts into the oracle's counts, ordered by
// project and then by event type so that two oracles of one month read the
// same however their maps were walked.
func sortedCounts(counts map[countKey]int) []OracleCount {
	stated := make([]OracleCount, 0, len(counts))
	for key, count := range counts {
		stated = append(stated, OracleCount{ProjectID: key.projectID, EventType: key.eventType, Count: count})
	}
	slices.SortFunc(stated, func(a, b OracleCount) int {
		if c := strings.Compare(a.ProjectID, b.ProjectID); c != 0 {
			return c
		}
		return strings.Compare(a.EventType, b.EventType)
	})
	return stated
}

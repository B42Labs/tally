package simulator

import (
	"encoding/json"
	"strconv"
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

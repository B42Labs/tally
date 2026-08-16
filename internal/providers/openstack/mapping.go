package openstack

import (
	"encoding/json"
	"log/slog"
	"net/netip"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/ids"
	"github.com/b42labs/tally/internal/core/money"
)

// platform is the platform name every event this collector produces carries. It
// is also half of the key the resource type registry validates a size against.
const platform = "openstack"

// mebibytesPerGibibyte and bytesPerGibibyte are the divisors the reported
// quantities are converted with. Nova reports memory in MiB and glance reports
// image sizes in bytes, while the canonical size objects are in GiB.
var (
	mebibytesPerGibibyte = decimal.NewFromInt(1024)
	bytesPerGibibyte     = decimal.NewFromInt(1 << 30)
)

// vmStates maps a nova vm_state to the state Tally records. A state outside the
// map passes through unchanged, because a state is an opaque provider string
// and substituting something known for an unknown one would hide what the cloud
// actually reported.
var vmStates = map[string]string{
	"active":            "active",
	"stopped":           "shutoff",
	"shelved_offloaded": "shelved",
	"paused":            "paused",
	"suspended":         "suspended",
	"error":             "error",
}

// stateRule derives an event's payload state from the notification payload.
type stateRule func(payload map[string]any) string

// sizeBuilder derives an event's payload size object from the notification
// payload. It returns a map even when it can read nothing from the payload, so
// that a create event says "this is the size" rather than "the size did not
// change".
type sizeBuilder func(payload map[string]any) map[string]any

// mappingEntry is one row of the mapping table: everything that turns one oslo
// notification type into a Tally event.
type mappingEntry struct {
	// eventType is the Tally event type the notification becomes. It is spelled
	// out per entry rather than inherited from the oslo type, because several
	// oslo types map onto one Tally type.
	eventType string
	// resourceType is the type the size object is validated against server-side.
	resourceType string
	// state derives payload.state. It is nil on a delete entry, where the core
	// sets the state itself, and set on every other entry, because validation
	// requires a state on everything but a delete.
	state stateRule
	// size derives payload.size. It is nil when the event does not describe a
	// size, which is how an event says the size is unchanged.
	size sizeBuilder
	// resourceIDPath is where in the payload the resource id sits, as a path
	// because neutron nests its resource inside the payload.
	resourceIDPath []string
	// projectIDPath is where in the payload the owning project sits. It is empty
	// when the notification carries no project of its own and the request context
	// is the only source.
	projectIDPath []string
	// skip reports whether a notification of this type carries nothing worth
	// recording yet. It is nil where every notification is mapped.
	skip func(payload map[string]any) bool
}

// mappings is one entry per oslo event type this collector records, keyed by the
// type as the emitting service spells it. Keeping it data rather than code is
// deliberate: oslo names and payload members differ per OpenStack release, so
// adapting the collector to a deployment is an edit to this table and to nothing
// around it. A type absent from the table is not mapped, and the caller counts
// and skips it.
var mappings = map[string]mappingEntry{
	"compute.instance.create.end": {
		eventType:      "compute.instance.create.end",
		resourceType:   "instance",
		state:          vmState,
		size:           instanceSize,
		resourceIDPath: []string{"instance_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"compute.instance.delete.end": {
		eventType:      "compute.instance.delete.end",
		resourceType:   "instance",
		resourceIDPath: []string{"instance_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"compute.instance.resize.end": {
		eventType:      "compute.instance.resize.end",
		resourceType:   "instance",
		state:          vmState,
		size:           instanceSize,
		resourceIDPath: []string{"instance_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	// A resize is finished under a second name, and both carry the new size, so
	// both are booked as the same Tally event.
	"compute.instance.finish_resize.end": {
		eventType:      "compute.instance.resize.end",
		resourceType:   "instance",
		state:          vmState,
		size:           instanceSize,
		resourceIDPath: []string{"instance_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"compute.instance.shelve_offload.end": {
		eventType:      "compute.instance.shelve",
		resourceType:   "instance",
		state:          fixedState("shelved"),
		resourceIDPath: []string{"instance_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"compute.instance.unshelve.end": {
		eventType:      "compute.instance.unshelve",
		resourceType:   "instance",
		state:          fixedState("active"),
		resourceIDPath: []string{"instance_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"compute.instance.power_off.end": {
		eventType:      "compute.instance.power_off",
		resourceType:   "instance",
		state:          fixedState("shutoff"),
		resourceIDPath: []string{"instance_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"compute.instance.power_on.end": {
		eventType:      "compute.instance.power_on",
		resourceType:   "instance",
		state:          fixedState("active"),
		resourceIDPath: []string{"instance_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"volume.create.end": {
		eventType:      "volume.create.end",
		resourceType:   "volume",
		state:          fixedState("available"),
		size:           volumeSize,
		resourceIDPath: []string{"volume_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"volume.delete.end": {
		eventType:      "volume.delete.end",
		resourceType:   "volume",
		resourceIDPath: []string{"volume_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"volume.resize.end": {
		eventType:      "volume.resize.end",
		resourceType:   "volume",
		state:          volumeStatus,
		size:           volumeSize,
		resourceIDPath: []string{"volume_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"volume.retype": {
		eventType:      "volume.retype",
		resourceType:   "volume",
		state:          volumeStatus,
		size:           volumeSize,
		resourceIDPath: []string{"volume_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	// A transfer changes nothing about the volume itself, only who owns it, and
	// the payload's tenant is already the new owner.
	"volume.transfer.accept.end": {
		eventType:      "volume.transfer.accept.end",
		resourceType:   "volume",
		state:          volumeStatus,
		size:           volumeSize,
		resourceIDPath: []string{"volume_id"},
		projectIDPath:  []string{"tenant_id"},
	},
	"floatingip.create.end": {
		eventType:      "floatingip.create.end",
		resourceType:   "floating_ip",
		state:          fixedState("active"),
		size:           floatingIPSize,
		resourceIDPath: []string{"floatingip", "id"},
		projectIDPath:  []string{"floatingip", "tenant_id"},
	},
	// The delete names the address that is gone at the top level and repeats
	// nothing else about it, so the project comes from the request context.
	"floatingip.delete.end": {
		eventType:      "floatingip.delete.end",
		resourceType:   "floating_ip",
		resourceIDPath: []string{"floatingip_id"},
	},
	"image.upload": {
		eventType:      "image.create",
		resourceType:   "image",
		state:          fixedState("active"),
		size:           imageSize,
		resourceIDPath: []string{"id"},
		projectIDPath:  []string{"owner"},
	},
	"image.create": {
		eventType:      "image.create",
		resourceType:   "image",
		state:          fixedState("active"),
		size:           imageSize,
		resourceIDPath: []string{"id"},
		projectIDPath:  []string{"owner"},
		skip:           unsizedImage,
	},
	"image.delete": {
		eventType:      "image.delete",
		resourceType:   "image",
		resourceIDPath: []string{"id"},
		projectIDPath:  []string{"owner"},
	},
}

// MapNotification turns one oslo notification into the Tally event it stands
// for. The second return is false when the notification is not mapped, either
// because no table entry claims its type or because the entry's own rule says
// this particular notification carries nothing to record yet.
//
// Mapping never fails. A payload missing the resource id or the project still
// produces an event, and the Reporting API dead-letters it with the validation
// reason it broke. That leaves an operator a record of a notification the
// mapping did not understand, where dropping it here would have been silent.
func MapNotification(n Notification, cloud string) (event.Event, bool) {
	entry, ok := mappings[n.EventType]
	if !ok {
		return event.Event{}, false
	}
	if entry.skip != nil && entry.skip(n.Payload) {
		return event.Event{}, false
	}

	resourceID := stringAt(n.Payload, entry.resourceIDPath...)
	mapped := event.Event{
		// The oslo message id is unique per notification, which is what makes a
		// redelivery a duplicate at ingestion rather than a second event.
		EventID:      n.MessageID,
		Timestamp:    n.Timestamp,
		EventType:    entry.eventType,
		Platform:     platform,
		Cloud:        cloud,
		ResourceType: entry.resourceType,
		ResourceID:   resourceID,
		ProjectID:    projectID(n, entry.projectIDPath),
		Source:       event.SourceCollector,
		Payload: event.PayloadEnvelope{
			// The oslo type is kept as it arrived, so an event that was renamed on
			// the way in can still be traced back to the notification it came from.
			Provider: map[string]any{"oslo_event_type": n.EventType},
		},
	}
	if entry.state != nil {
		state := entry.state(n.Payload)
		mapped.Payload.State = &state
	}
	if entry.size != nil {
		mapped.Payload.Size = entry.size(n.Payload)
	}
	if mapped.EventID == "" {
		mapped.EventID = ids.DeterministicEventID(platform, cloud, resourceID, entry.eventType, n.Timestamp)
	}
	return mapped, true
}

// projectID resolves the project an event is booked to. The payload wins because
// it describes the resource, while the context describes whoever made the
// request, and the two differ when an administrator acts on another project's
// resource. Services set one of the two context members and not the other, so
// both are tried.
func projectID(n Notification, path []string) string {
	if id := stringAt(n.Payload, path...); id != "" {
		return id
	}
	if n.ContextProjectID != "" {
		return n.ContextProjectID
	}
	return n.ContextTenantID
}

// vmState reads nova's vm_state and normalizes it through vmStates.
func vmState(payload map[string]any) string {
	state := stringAt(payload, "state")
	if normalized, ok := vmStates[state]; ok {
		return normalized
	}
	return state
}

// fixedState is the rule for a notification whose type already says what the
// state became, so the payload is not consulted at all.
func fixedState(state string) stateRule {
	return func(map[string]any) string { return state }
}

// volumeStatus reads the status cinder reports. The events it is used for change
// a volume's size or owner rather than its status, and cinder does not always
// repeat the status in them, so an absent one falls back to the state a usable
// volume is in.
func volumeStatus(payload map[string]any) string {
	if status := stringAt(payload, "status"); status != "" {
		return status
	}
	return "available"
}

// unsizedImage reports whether a glance image is still without content. An image
// is created before its bits are uploaded, so its size is absent or zero at that
// point; the upload that follows carries the real size and is the notification
// the image is booked from.
func unsizedImage(payload map[string]any) bool {
	size, ok := decimalAt(payload, "size")
	return !ok || !size.IsPositive()
}

// instanceSize describes an instance the way the rating engine meters it.
func instanceSize(payload map[string]any) map[string]any {
	size := make(map[string]any, 4)
	setNumber(size, "vcpus", payload, "vcpus")
	setQuotient(size, "ram_gb", payload, "memory_mb", mebibytesPerGibibyte)
	if disk, ok := diskGB(payload); ok {
		size["disk_gb"] = json.Number(disk.String())
	}
	setString(size, "flavor", payload, "instance_type")
	return size
}

// diskGB sums the two disks nova reports separately. Either of them may be
// absent, an instance without ephemeral storage reports no ephemeral_gb, and
// counts as zero then. A payload naming neither says nothing about the disk at
// all, which is why that case is reported as absent instead of as zero.
//
// The sum goes through the same exact decimals every other quantity in this
// file does, and for the same reason: a deployment whose notification path
// re-serializes payload numbers sends 20.0 where nova sent 20, and demanding a
// bare integer would drop that disk and book the instance with the ephemeral
// zero behind it.
func diskGB(payload map[string]any) (decimal.Decimal, bool) {
	var total decimal.Decimal
	var found bool
	for _, key := range []string{"root_gb", "ephemeral_gb"} {
		value, ok := decimalAt(payload, key)
		if !ok {
			continue
		}
		total = total.Add(value)
		found = true
	}
	return total, found
}

// volumeSize describes a volume. On a retype the payload already names the type
// the volume was moved to, so the same builder serves every volume event.
func volumeSize(payload map[string]any) map[string]any {
	size := make(map[string]any, 2)
	setNumber(size, "size_gb", payload, "size")
	setString(size, "type", payload, "volume_type")
	return size
}

// imageSize describes an image. Glance reports the size in bytes.
func imageSize(payload map[string]any) map[string]any {
	size := make(map[string]any, 1)
	setQuotient(size, "size_gb", payload, "size", bytesPerGibibyte)
	return size
}

// floatingIPSize describes a floating IP, whose only billable property is which
// protocol it is an address of. An address that is absent or unreadable counts
// as IPv4, since that is what a deployment allocates unless it says otherwise,
// and a skipped event would cost the address its whole billing record.
func floatingIPSize(payload map[string]any) map[string]any {
	address := stringAt(payload, "floatingip", "floating_ip_address")
	version := 4
	switch parsed, err := netip.ParseAddr(address); {
	case err != nil:
		slog.Default().Debug("floating ip address is unreadable, assuming IPv4",
			"address", address)
	case !parsed.Is4():
		version = 6
	}
	return map[string]any{"ip_version": version}
}

// lookup walks the payload along a path of keys. A step that is missing, or that
// is not an object where the path continues, yields nil: reading a payload never
// fails, it only comes back empty.
func lookup(payload map[string]any, path ...string) any {
	var current any = payload
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

// stringAt returns the string at a payload path, or the empty string when the
// path leads nowhere or to a value of another type.
func stringAt(payload map[string]any, path ...string) string {
	value, _ := lookup(payload, path...).(string)
	return value
}

// numberAt returns the JSON number at a payload path. The literal is kept as it
// arrived, so a quantity still carries every digit the service sent.
func numberAt(payload map[string]any, path ...string) (json.Number, bool) {
	number, ok := lookup(payload, path...).(json.Number)
	return number, ok
}

// maxQuantityDigits and maxQuantityExp bound what a payload number may cost.
// encoding/json validates a number's syntax and nothing about its magnitude, so
// the literal a publisher chose reaches here as it was written. A decimal's
// exponent is what big.Int expands on the first arithmetic the value takes part
// in: 1e2000000000 is 25 bytes on the wire, a valid JSON number, and a
// two-billion-digit integer inside the Add in diskGB or the Div in setQuotient.
// A quantity nova, cinder, or glance reports is far inside both bounds.
const (
	maxQuantityDigits = 40
	maxQuantityExp    = 30
)

// decimalAt reads a payload number as an exact decimal, which is the only form
// a quantity is calculated in. A number past the bounds above is reported as
// absent, the way every other unusable value in this file is: the event still
// goes out, without the member this one would have carried.
func decimalAt(payload map[string]any, path ...string) (decimal.Decimal, bool) {
	number, ok := numberAt(payload, path...)
	if !ok || len(number.String()) > maxQuantityDigits {
		return decimal.Decimal{}, false
	}
	value, err := decimal.NewFromString(number.String())
	if err != nil || value.Exponent() > maxQuantityExp || value.Exponent() < -maxQuantityExp {
		return decimal.Decimal{}, false
	}
	return value, true
}

// setNumber copies a payload number into the size object unchanged. A member
// whose source is absent or of another type is left out rather than defaulted,
// so a size says only what the notification actually reported.
func setNumber(size map[string]any, member string, payload map[string]any, key string) {
	if number, ok := numberAt(payload, key); ok {
		size[member] = number
	}
}

// setString copies a payload string into the size object, under the same rule as
// setNumber.
func setString(size map[string]any, member string, payload map[string]any, key string) {
	if value, ok := lookup(payload, key).(string); ok {
		size[member] = value
	}
}

// setQuotient converts a payload number into another unit and stores the result
// as a JSON number literal. Rendering the decimal itself is what keeps the value
// exact through encoding: a quotient of 8192 by 1024 is written as 8 and one of
// 512 by 1024 as 0.5, neither of them rounded on the way.
func setQuotient(size map[string]any, member string, payload map[string]any, key string,
	divisor decimal.Decimal,
) {
	value, ok := decimalAt(payload, key)
	if !ok {
		return
	}
	size[member] = json.Number(money.Div(value, divisor).String())
}

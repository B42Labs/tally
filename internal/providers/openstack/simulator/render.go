package simulator

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// timestampLayout is how oslo writes a notification timestamp: no zone, six
// fractional digits. It is the first layout the collector tries, so a rendered
// notification carries the same timestamp form a recorded one does.
const timestampLayout = "2006-01-02 15:04:05.000000"

// The distances a payload reports between a resource's request and the instant
// it was ready. Nova and cinder both write a created_at that precedes the
// notification, and repeating that shape keeps a simulated payload from being
// the one payload where the two timestamps are equal.
const (
	instanceBoot    = 30 * time.Second
	volumeProvision = 8 * time.Second
)

// The constants glance and cinder repeat in every notification of the simulated
// cloud. They are the ones the recorded samples carry.
const (
	imageDiskFormat        = "qcow2"
	imageContainerFormat   = "bare"
	imageVisibility        = "private"
	imageMinDiskGB         = 5
	imageMinRAMMB          = 512
	imageVirtualSizeFactor = 5
	volumeReplication      = "disabled"
)

// stamp renders an instant the way a service writes it. It goes through UTC
// first, because the layout carries no zone and every reader of those digits,
// the collector included, takes them for UTC.
func stamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}

// Render turns a transition into the message body an OpenStack service would
// publish for it.
//
// The body has two layers, and the inner one travels as a string rather than as
// a nested object. That is what oslo does, and reproducing it is the whole
// point: the collector decodes the body twice, so a simulator that nested the
// notification would produce something only the simulator could read.
func Render(t Transition) ([]byte, error) {
	// octavia publishes a null publisher, the way the recorded samples carry it.
	// Every other service names the instance the notification came from.
	var publisher any
	if t.PublisherID != "" {
		publisher = t.PublisherID
	}

	inner, err := json.Marshal(map[string]any{
		"message_id":   t.MessageID,
		"event_type":   t.EventType,
		"publisher_id": publisher,
		"priority":     "INFO",
		"timestamp":    stamp(t.At),
		// Services differ in which of the two they set. Setting both is what a
		// deployment running several releases side by side looks like on the bus.
		"_context_project_id": t.ProjectID,
		"_context_tenant_id":  t.ProjectID,
		"_context_user_id":    t.UserID,
		"payload":             t.Payload,
	})
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", t.EventType, err)
	}

	body, err := json.Marshal(map[string]string{
		"oslo.version": "2.0",
		"oslo.message": string(inner),
	})
	if err != nil {
		return nil, fmt.Errorf("rendering %s: %w", t.EventType, err)
	}
	return body, nil
}

// instanceCreatePayload describes a server nova has just booted. It is the
// widest of the compute payloads: it names the flavor three times over and the
// image the server was booted from, none of which the later notifications
// repeat in full.
func instanceCreatePayload(p *project, inst *instance, cloud string) map[string]any {
	// A server booted from a volume names no image, because the image was
	// written to the volume before the server existed.
	imageURL := ""
	if inst.bootVolume == nil {
		imageURL = fmt.Sprintf("http://glance.%s.example:9292/images/%s", cloud, inst.imageID)
	}

	return map[string]any{
		"instance_id":        inst.id,
		"tenant_id":          p.id,
		"user_id":            p.userID,
		"display_name":       inst.name,
		"state":              "active",
		"state_description":  "",
		"vcpus":              inst.flavor.vcpus,
		"memory_mb":          inst.flavor.memoryMB,
		"root_gb":            inst.flavor.rootGB,
		"ephemeral_gb":       inst.flavor.ephemeralGB,
		"disk_gb":            inst.flavor.rootGB + inst.flavor.ephemeralGB,
		"instance_type":      inst.flavor.name,
		"instance_type_id":   inst.flavor.typeID,
		"instance_flavor_id": inst.flavor.flavorID,
		"host":               inst.host,
		"availability_zone":  availabilityZone,
		"image_ref_url":      imageURL,
		"created_at":         stamp(inst.createdAt.Add(-instanceBoot)),
		"launched_at":        stamp(inst.createdAt),
	}
}

// instanceDeletePayload describes a destroyed server. It reports no
// state_description, which is the one member the other compute payloads carry
// and this one does not.
func instanceDeletePayload(p *project, inst *instance, at time.Time) map[string]any {
	return map[string]any{
		"instance_id":   inst.id,
		"tenant_id":     p.id,
		"user_id":       p.userID,
		"display_name":  inst.name,
		"state":         "deleted",
		"vcpus":         inst.flavor.vcpus,
		"memory_mb":     inst.flavor.memoryMB,
		"root_gb":       inst.flavor.rootGB,
		"ephemeral_gb":  inst.flavor.ephemeralGB,
		"instance_type": inst.flavor.name,
		"host":          inst.host,
		"created_at":    stamp(inst.createdAt.Add(-instanceBoot)),
		"deleted_at":    stamp(at),
		"terminated_at": stamp(at),
	}
}

// instanceResizePayload describes a server that changed flavor. Both halves of
// a resize carry the same members and differ in the state alone, which is why
// one builder serves them: nova announces the new size twice, and the mapping
// books both under one event type.
func instanceResizePayload(p *project, inst *instance, state string) map[string]any {
	return map[string]any{
		"instance_id":       inst.id,
		"tenant_id":         p.id,
		"user_id":           p.userID,
		"display_name":      inst.name,
		"state":             state,
		"state_description": "",
		"vcpus":             inst.flavor.vcpus,
		"memory_mb":         inst.flavor.memoryMB,
		"root_gb":           inst.flavor.rootGB,
		"ephemeral_gb":      inst.flavor.ephemeralGB,
		"instance_type":     inst.flavor.name,
		"instance_type_id":  inst.flavor.typeID,
		"host":              inst.host,
		"availability_zone": availabilityZone,
		"created_at":        stamp(inst.createdAt.Add(-instanceBoot)),
		"launched_at":       stamp(inst.createdAt),
	}
}

// instancePowerPayload describes a server that was powered off or on. A power
// state change costs nova the shortest payload of them all: no disk, no zone,
// and no timestamps, because none of them changed.
func instancePowerPayload(p *project, inst *instance, state string) map[string]any {
	return map[string]any{
		"instance_id":       inst.id,
		"tenant_id":         p.id,
		"user_id":           p.userID,
		"display_name":      inst.name,
		"state":             state,
		"state_description": "",
		"vcpus":             inst.flavor.vcpus,
		"memory_mb":         inst.flavor.memoryMB,
		"instance_type":     inst.flavor.name,
		"host":              inst.host,
	}
}

// instanceShelvePayload describes a server that was shelved or brought back. It
// reports the disk a power change leaves out, since a shelved server keeps its
// image on disk while it holds no memory.
func instanceShelvePayload(p *project, inst *instance, state string) map[string]any {
	return map[string]any{
		"instance_id":       inst.id,
		"tenant_id":         p.id,
		"user_id":           p.userID,
		"display_name":      inst.name,
		"state":             state,
		"state_description": "",
		"vcpus":             inst.flavor.vcpus,
		"memory_mb":         inst.flavor.memoryMB,
		"root_gb":           inst.flavor.rootGB,
		"ephemeral_gb":      inst.flavor.ephemeralGB,
		"instance_type":     inst.flavor.name,
		"host":              inst.host,
	}
}

// floatingIPCreatePayload describes an allocated address. Neutron nests the
// resource one level down, and it reports both the older tenant_id and the
// newer project_id, which is why the mapping reads the address through a path
// instead of a member name.
func floatingIPCreatePayload(p *project, fip *floatingIP, networkID string) map[string]any {
	// An address is allocated before it is associated with a port, so it points
	// at nothing yet and is down. The VIP port of a load balancer is the
	// exception: that address is associated the moment it is created.
	var fixedAddress, portID, routerID any
	status := "DOWN"
	if fip.portID != "" {
		fixedAddress, portID, routerID = fip.fixedAddress, fip.portID, fip.routerID
		status = "ACTIVE"
	}

	return map[string]any{
		"floatingip": map[string]any{
			"id":                  fip.id,
			"tenant_id":           p.id,
			"project_id":          p.id,
			"floating_ip_address": fip.address,
			"floating_network_id": networkID,
			"fixed_ip_address":    fixedAddress,
			"port_id":             portID,
			"router_id":           routerID,
			"status":              status,
			"description":         "",
		},
	}
}

// floatingIPDeletePayload describes a released address. Neutron names the id
// and nothing else, so the project the address was billed to comes from the
// request context.
func floatingIPDeletePayload(fip *floatingIP) map[string]any {
	return map[string]any{"floatingip_id": fip.id}
}

// imageCreatePayload describes an image glance has accepted but has no bits
// for. Both size members are null, which is what makes this the one
// notification the simulator emits that the mapping skips.
func imageCreatePayload(p *project, img *image) map[string]any {
	return map[string]any{
		"id":               img.id,
		"owner":            p.id,
		"name":             img.name,
		"status":           "queued",
		"size":             nil,
		"virtual_size":     nil,
		"disk_format":      imageDiskFormat,
		"container_format": imageContainerFormat,
		"visibility":       imageVisibility,
		"protected":        false,
		"created_at":       stamp(img.createdAt),
		"updated_at":       stamp(img.createdAt),
	}
}

// imageUploadPayload describes an image that now has content. It is the
// notification the image is billed from, because it is the first one to report
// a size.
func imageUploadPayload(p *project, img *image, at time.Time) map[string]any {
	return map[string]any{
		"id":               img.id,
		"owner":            p.id,
		"name":             img.name,
		"status":           "active",
		"size":             img.size,
		"virtual_size":     img.size * imageVirtualSizeFactor,
		"disk_format":      imageDiskFormat,
		"container_format": imageContainerFormat,
		"checksum":         strings.ReplaceAll(img.id, "-", ""),
		"min_disk":         imageMinDiskGB,
		"min_ram":          imageMinRAMMB,
		"visibility":       imageVisibility,
		"protected":        false,
		"created_at":       stamp(img.createdAt),
		"updated_at":       stamp(at),
	}
}

// imageDeletePayload describes a removed image. Glance still reports the size
// it had, which is what lets the projection close the record with the size it
// was billed at.
func imageDeletePayload(p *project, img *image, at time.Time) map[string]any {
	return map[string]any{
		"id":               img.id,
		"owner":            p.id,
		"name":             img.name,
		"status":           "deleted",
		"size":             img.size,
		"disk_format":      imageDiskFormat,
		"container_format": imageContainerFormat,
		"deleted":          true,
		"deleted_at":       stamp(at),
	}
}

// volumeCreatePayload describes a volume cinder has provisioned. It is
// available at this point: an attach follows for most of them, and cinder
// reports that one under a type the collector does not map.
func volumeCreatePayload(p *project, vol *volume) map[string]any {
	return map[string]any{
		"volume_id":          vol.id,
		"tenant_id":          p.id,
		"user_id":            p.userID,
		"display_name":       vol.name,
		"status":             "available",
		"size":               vol.sizeGB,
		"volume_type":        vol.volumeType,
		"availability_zone":  availabilityZone,
		"host":               volumeHost,
		"replication_status": volumeReplication,
		"created_at":         stamp(vol.createdAt.Add(-volumeProvision)),
		"launched_at":        stamp(vol.createdAt),
	}
}

// volumeDeletePayload describes a removed volume, with the size and type it
// carried when it went.
func volumeDeletePayload(p *project, vol *volume, at time.Time) map[string]any {
	return map[string]any{
		"volume_id":    vol.id,
		"tenant_id":    p.id,
		"user_id":      p.userID,
		"display_name": vol.name,
		"status":       "deleted",
		"size":         vol.sizeGB,
		"volume_type":  vol.volumeType,
		"host":         volumeHost,
		"created_at":   stamp(vol.createdAt.Add(-volumeProvision)),
		"deleted_at":   stamp(at),
	}
}

// volumeStatePayload describes a volume whose size, type, or owner changed.
// Cinder reports the same members for all three, each already carrying the new
// value, which is why one builder serves a resize, a retype, and an accepted
// transfer alike.
func volumeStatePayload(p *project, vol *volume) map[string]any {
	status := "available"
	if vol.attached {
		status = "in-use"
	}
	return map[string]any{
		"volume_id":    vol.id,
		"tenant_id":    p.id,
		"user_id":      p.userID,
		"display_name": vol.name,
		"status":       status,
		"size":         vol.sizeGB,
		"volume_type":  vol.volumeType,
		"host":         volumeHost,
		"created_at":   stamp(vol.createdAt.Add(-volumeProvision)),
	}
}

// listenerSpecs are the listeners a load balancer gets, in the order a shoot
// adds them. A service publishes one port after another, so a balancer that
// carries two listeners carries the first two of these.
var listenerSpecs = []struct {
	name     string
	protocol string
	port     int
}{
	{name: "http", protocol: "HTTP", port: 80},
	{name: "https", protocol: "TERMINATED_HTTPS", port: 443},
	{name: "metrics", protocol: "TCP", port: 9100},
}

// loadBalancerPayload describes one octavia load balancer. Octavia reports the
// listeners and the pools on an update alone, which is why the collector books
// a balancer's size from the update: the create carries a size of zero
// listeners and zero pools, and the delete carries neither member at all.
func loadBalancerPayload(p *project, s *shoot, lb *loadBalancer, withMembers bool) map[string]any {
	payload := map[string]any{
		"admin_state_up":    true,
		"description":       "",
		"loadbalancer_id":   lb.id,
		"name":              lb.name,
		"project_id":        p.id,
		"vip_address":       lb.vipAddress,
		"vip_network_id":    s.networkID,
		"vip_port_id":       lb.vipPortID,
		"vip_qos_policy_id": nil,
		"vip_subnet_id":     s.subnetID,
		"vip_sg_ids":        []any{},
		"additional_vips":   []any{},
	}
	if !withMembers {
		return payload
	}

	// Both members are slices even when they hold nothing, because the mapping
	// counts the array and a null is not one it can count.
	listeners := make([]any, 0, len(lb.listenerIDs))
	for index, id := range lb.listenerIDs {
		// A balancer that carries more listeners than the catalog names takes the
		// catalog from the front again. The mapping books the count and nothing
		// else of a listener, so a repeated name and port cost the month nothing,
		// where reaching past the catalog would end the run in this renderer with
		// the balancer that outgrew it named nowhere.
		spec := listenerSpecs[index%len(listenerSpecs)]
		// A listener points at the pool behind it, and a balancer with fewer
		// pools than listeners has them share one. One with no pool at all points
		// its listeners at nothing, the way octavia reports a listener whose
		// default pool was never created, rather than ending the run here.
		var poolID any
		if len(lb.poolIDs) > 0 {
			poolID = lb.poolIDs[index%len(lb.poolIDs)]
		}
		listeners = append(listeners, map[string]any{
			"admin_state_up":  true,
			"default_pool_id": poolID,
			"listener_id":     id,
			"loadbalancer_id": lb.id,
			"name":            spec.name,
			"project_id":      p.id,
			"protocol":        spec.protocol,
			"protocol_port":   spec.port,
		})
	}

	pools := make([]any, 0, len(lb.poolIDs))
	for index, id := range lb.poolIDs {
		pools = append(pools, map[string]any{
			"admin_state_up":  true,
			"lb_algorithm":    "ROUND_ROBIN",
			"loadbalancer_id": lb.id,
			"name":            fmt.Sprintf("pool-%d", index),
			"pool_id":         id,
			"project_id":      p.id,
			"protocol":        "HTTP",
		})
	}

	payload["listeners"] = listeners
	payload["pools"] = pools
	return payload
}

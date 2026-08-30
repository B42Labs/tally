package simulator

import (
	"encoding/json"
	"fmt"
	"maps"
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

// schedulerPayload describes the placement decision a boot begins with. The
// scheduler reports the request it answered rather than a server, so the
// flavor arrives as the type that was asked for and the server-to-be as the
// properties it is going to have.
func schedulerPayload(p *project, inst *instance) map[string]any {
	// A server booted from a volume is scheduled without an image, because the
	// image was written to the volume before the request was made.
	imageID := inst.imageID
	if inst.bootVolume != nil {
		imageID = ""
	}

	return map[string]any{
		"request_spec": map[string]any{
			"image": map[string]any{"id": imageID},
			"instance_properties": map[string]any{
				"availability_zone": availabilityZone,
				"display_name":      inst.name,
				"ephemeral_gb":      inst.flavor.ephemeralGB,
				"memory_mb":         inst.flavor.memoryMB,
				"project_id":        p.id,
				"root_gb":           inst.flavor.rootGB,
				"user_id":           p.userID,
				"uuid":              inst.id,
				"vcpus":             inst.flavor.vcpus,
			},
			"instance_type": map[string]any{
				"ephemeral_gb": inst.flavor.ephemeralGB,
				"flavorid":     inst.flavor.flavorID,
				"id":           inst.flavor.typeID,
				"memory_mb":    inst.flavor.memoryMB,
				"name":         inst.flavor.name,
				"root_gb":      inst.flavor.rootGB,
				"vcpus":        inst.flavor.vcpus,
			},
			"num_instances": 1,
		},
	}
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

// instanceCreateStartPayload describes the server nova is about to boot. It
// carries the members of the create it precedes: the server is still building
// and has not been launched, and everything else about it is already decided.
func instanceCreateStartPayload(p *project, inst *instance, cloud string) map[string]any {
	payload := instanceCreatePayload(p, inst, cloud)
	payload["state"] = "building"
	payload["launched_at"] = nil
	return payload
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

// instanceUpdatePayload describes a server whose state or task changed. Nova
// sends one on every step of a boot and once more when a delete begins, and it
// is the payload that carries the audit window: what the server did between
// from and to, which for an update is the day so far.
//
// The task states are typed as any because nova reports a null for a server
// that is not working on anything, and the old state is the state itself,
// because an update reports the task a server took up and not a state it left.
func instanceUpdatePayload(p *project, inst *instance, state string, oldTask, newTask any,
	from, to time.Time,
) map[string]any {
	payload := instancePowerPayload(p, inst, state)
	payload["audit_period_beginning"] = stamp(from)
	payload["audit_period_ending"] = stamp(to)
	payload["bandwidth"] = map[string]any{}
	payload["new_task_state"] = newTask
	payload["old_state"] = state
	payload["old_task_state"] = oldTask
	return payload
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

// instanceExistsPayload describes a server nova audits. base is the members of
// the create, which is what nova repeats in an existence audit, and it is
// cloned so that the caller's map stays what it was. The audit reports the
// window it covers, the traffic over it, and the image the server runs, and
// the simulated cloud meters none of the three.
func instanceExistsPayload(base map[string]any, from, to time.Time) map[string]any {
	payload := maps.Clone(base)
	payload["audit_period_beginning"] = stamp(from)
	payload["audit_period_ending"] = stamp(to)
	payload["bandwidth"] = map[string]any{}
	payload["image_meta"] = map[string]any{}
	return payload
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

// portRequestPayload describes the port neutron was asked for. The request
// names the network and the device the port is for and nothing neutron decides
// itself, so it carries neither an id nor an address yet.
func portRequestPayload(p *project, port *port) map[string]any {
	return map[string]any{
		"port": map[string]any{
			"admin_state_up": true,
			"device_id":      port.deviceID,
			"device_owner":   port.deviceOwner,
			"name":           "",
			"network_id":     port.networkID,
			"project_id":     p.id,
			"tenant_id":      p.id,
		},
	}
}

// portPayload describes a port neutron has created. A port is unbound while it
// belongs to no host: it names no host, its interface type is unknown, and it
// is down until the compute the server runs on binds it, which is what bound
// tells the two halves of a port's life apart.
func portPayload(p *project, port *port, bound bool) map[string]any {
	host, vifType, status := "", "unbound", "DOWN"
	if bound {
		host, vifType, status = port.host, "ovs", "ACTIVE"
	}
	// The group is a slice even when the network has none, because a null is
	// not a list of groups.
	securityGroups := []any{}
	if port.securityGroupID != "" {
		securityGroups = append(securityGroups, port.securityGroupID)
	}

	return map[string]any{
		"port": map[string]any{
			"admin_state_up":   true,
			"binding:host_id":  host,
			"binding:vif_type": vifType,
			"device_id":        port.deviceID,
			"device_owner":     port.deviceOwner,
			"fixed_ips": []any{map[string]any{
				"ip_address": port.address,
				"subnet_id":  port.subnetID,
			}},
			"id":              port.id,
			"mac_address":     port.macAddress,
			"name":            "",
			"network_id":      port.networkID,
			"project_id":      p.id,
			"security_groups": securityGroups,
			"status":          status,
			"tenant_id":       p.id,
		},
	}
}

// portBindingPayload describes the binding a compute asks for. It names the
// host the port is to be bound to and the device behind it, since those are
// the members the request changes.
func portBindingPayload(port *port) map[string]any {
	return map[string]any{
		"port": map[string]any{
			"binding:host_id": port.host,
			"device_id":       port.deviceID,
			"device_owner":    port.deviceOwner,
		},
	}
}

// neutronDeletePayload describes a removed neutron resource. Neutron names the
// kind and the id and nothing else, the way it announces a released address,
// so the project the resource belonged to comes from the request context.
func neutronDeletePayload(kind, id string) map[string]any {
	return map[string]any{kind + "_id": id}
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

// imagePreparePayload describes an image glance is receiving the bits of. It
// carries the members of the finished image with the three that only exist once
// the content is there left empty: the size on disk, the size the disk grows
// to, and the checksum over what arrived.
func imagePreparePayload(p *project, img *image) map[string]any {
	payload := imageUploadPayload(p, img, img.createdAt)
	payload["status"] = "saving"
	payload["size"] = nil
	payload["virtual_size"] = nil
	payload["checksum"] = nil
	return payload
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

// volumeCreateStartPayload describes a volume cinder has accepted and is
// building. It carries the members of the finished volume with two of them
// changed: the status is the one a volume has while it is provisioned, and
// there is no launched_at, because the volume has not been handed out yet.
func volumeCreateStartPayload(p *project, vol *volume) map[string]any {
	payload := volumeCreatePayload(p, vol)
	payload["status"] = "creating"
	payload["launched_at"] = nil
	return payload
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
// transfer alike. The status is the one the world holds: a volume a server
// holds is in use, and a volume attached to nothing is available. The members
// themselves come from volumeStatusPayload, which the steps around a volume
// render their own statuses through.
func volumeStatePayload(p *project, vol *volume) map[string]any {
	status := "available"
	if vol.attached {
		status = "in-use"
	}
	return volumeStatusPayload(p, vol, status)
}

// volumeStatusPayload describes a volume in the status the caller names. Cinder
// reports the same members whatever the volume is doing, and the status is what
// tells a creating volume from an attaching, a deleting, or an available one.
func volumeStatusPayload(p *project, vol *volume, status string) map[string]any {
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

// attachmentOf is the record cinder writes when a volume is connected to a
// server: which server holds it, on which compute, since when, and under which
// device name. A server's root volume is its first disk and every other volume
// its second, which is what the two mountpoints stand for.
//
// The server may be nil, and the attachment then names neither an instance nor
// a host: both members are null, the way cinder writes an attachment whose
// server it does not know.
func attachmentOf(id string, vol *volume, inst *instance, t time.Time) map[string]any {
	var instanceID, host any
	mountpoint := "/dev/vdb"
	if inst != nil {
		instanceID, host = inst.id, inst.host
		if inst.bootVolume == vol {
			mountpoint = "/dev/vda"
		}
	}
	return map[string]any{
		"attach_mode":   "rw",
		"attach_status": "attached",
		"attach_time":   stamp(t),
		"attached_host": host,
		"id":            id,
		"instance_uuid": instanceID,
		"mountpoint":    mountpoint,
		"volume_id":     vol.id,
	}
}

// volumeAttachmentPayload describes a volume in the middle of an attach or a
// detach. Cinder repeats the members of the volume and adds what it is
// connected to, always as an array: it holds the one attachment of the server
// behind the volume, and it is empty while the volume is connected to nothing.
func volumeAttachmentPayload(p *project, vol *volume, status string,
	attachment map[string]any,
) map[string]any {
	payload := volumeStatusPayload(p, vol, status)
	payload["volume_attachment"] = []any{}
	if attachment != nil {
		payload["volume_attachment"] = []any{attachment}
	}
	return payload
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

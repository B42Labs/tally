package simulator

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/providers/openstack"
)

// sampleDir holds the notifications recorded from a real deployment. They are
// the collector's fixtures, and the simulator is held against them rather than
// against fixtures of its own: a simulator that drifts from them produces a
// month the collector was never written for.
var sampleDir = filepath.Join("..", "testdata", "golden", "notifications")

// render renders a transition or fails the test.
func render(t *testing.T, transition Transition) []byte {
	t.Helper()

	body, err := Render(transition)
	if err != nil {
		t.Fatalf("Render(%s) error = %v, want nil", transition.EventType, err)
	}
	return body
}

// parse runs a body through the collector's own decoder or fails the test.
func parse(t *testing.T, body []byte) openstack.Notification {
	t.Helper()

	notification, err := openstack.ParseEnvelope(body)
	if err != nil {
		t.Fatalf("ParseEnvelope(%s) error = %v, want nil", body, err)
	}
	return notification
}

// sampleName turns an event type into the file it was recorded in: both the
// dots and the underscores of the type are hyphens there.
var sampleName = strings.NewReplacer(".", "-", "_", "-")

// sampleFile names the recorded notification of an event type. image.create is
// recorded twice, with and without a size, and the simulator emits only the
// form without one.
func sampleFile(eventType string) string {
	if eventType == "image.create" {
		return "image-create-without-size.json"
	}
	return sampleName.Replace(eventType) + ".json"
}

// memberShape is a payload's structure as its sorted member names, with the
// members of a nested object, and of the objects an array holds, named under
// their parent. Neutron nests its resource one level down, so comparing the top
// level alone would let a floating IP payload pass with nothing in it, and
// cinder, neutron and octavia carry records inside arrays that nothing else
// would name.
func memberShape(payload map[string]any) []string {
	names := make([]string, 0, len(payload))
	for name, value := range payload {
		for _, suffix := range memberSuffixes(value) {
			names = append(names, name+suffix)
		}
	}
	slices.Sort(names)
	// Two records of one array have the same members, and each of them is worth
	// naming once.
	return slices.Compact(names)
}

// memberSuffixes are what a value adds to the name of the member holding it:
// the empty string for a scalar, and one ".<inner>" per member underneath an
// object or underneath the objects of an array.
//
// An empty object, an empty array, and an array of scalars have nothing to name
// underneath them, so they add the empty string and the member stays in the
// shape under its own name. Leaving them out would let a builder drop one
// unnoticed: the payloads carry an empty bandwidth, an empty image_meta, an
// empty attachment list, and arrays of plain ids.
func memberSuffixes(value any) []string {
	var inner []string
	switch typed := value.(type) {
	case map[string]any:
		inner = memberShape(typed)
	case []any:
		for _, element := range typed {
			nested, ok := element.(map[string]any)
			if !ok {
				continue
			}
			inner = append(inner, memberShape(nested)...)
		}
	}
	if len(inner) == 0 {
		return []string{""}
	}

	suffixes := make([]string, 0, len(inner))
	for _, name := range inner {
		suffixes = append(suffixes, "."+name)
	}
	return suffixes
}

// missingFrom returns the names of want that names does not carry.
func missingFrom(names, want []string) []string {
	var missing []string
	for _, name := range want {
		if !slices.Contains(names, name) {
			missing = append(missing, name)
		}
	}
	return missing
}

func TestRenderParsesWithTheCollector(t *testing.T) {
	for _, transition := range generateMonth(t, 1, july2026, testCloud) {
		notification := parse(t, render(t, transition))

		if notification.MessageID != transition.MessageID {
			t.Errorf("%s message id = %q, want %q",
				transition.EventType, notification.MessageID, transition.MessageID)
		}
		if notification.EventType != transition.EventType {
			t.Errorf("event type = %q, want %q", notification.EventType, transition.EventType)
		}
		if !notification.Timestamp.Equal(transition.At) {
			t.Errorf("%s timestamp = %s, want %s",
				transition.EventType, notification.Timestamp, transition.At)
		}
		if notification.ContextProjectID != transition.ProjectID {
			t.Errorf("%s context project = %q, want %q",
				transition.EventType, notification.ContextProjectID, transition.ProjectID)
		}
		if notification.ContextTenantID != transition.ProjectID {
			t.Errorf("%s context tenant = %q, want %q",
				transition.EventType, notification.ContextTenantID, transition.ProjectID)
		}
	}
}

func TestRenderedPayloadsCarryTheRecordedMembers(t *testing.T) {
	seen := make(map[string]bool)
	for _, transition := range generateMonth(t, 1, july2026, testCloud) {
		if seen[transition.EventType] {
			continue
		}
		seen[transition.EventType] = true

		body, err := os.ReadFile(filepath.Join(sampleDir, sampleFile(transition.EventType)))
		if err != nil {
			// The fixtures are the collector's, and the noise types of the
			// catalogue have none: the collector maps none of them, so a real
			// deployment never had one recorded here.
			// TestNoisePayloadsCarryTheirMembers pins those instead.
			if errors.Is(err, fs.ErrNotExist) && !transition.Billable {
				t.Logf("no recorded sample for %s, the mapping skips it", transition.EventType)
				continue
			}
			t.Fatalf("reading the recorded %s: %v", transition.EventType, err)
		}
		got := memberShape(parse(t, render(t, transition)).Payload)
		want := memberShape(parse(t, body).Payload)

		if !slices.Equal(got, want) {
			t.Errorf("%s payload members = %v, want %v (rendered and not recorded: %v; recorded and not rendered: %v)",
				transition.EventType, got, want, missingFrom(want, got), missingFrom(got, want))
		}
	}
}

// The members the payload builders of the billable month render. The catalogue
// repeats them: nova sends the same server description before a create that it
// sends with one, and cinder the same volume on an attach that it sends on a
// resize, so the noise is pinned against these sets rather than against
// sixty-two lists written out one by one.
var (
	createMembers = []string{
		"availability_zone", "created_at", "disk_gb", "display_name", "ephemeral_gb", "host",
		"image_ref_url", "instance_flavor_id", "instance_id", "instance_type", "instance_type_id",
		"launched_at", "memory_mb", "root_gb", "state", "state_description", "tenant_id", "user_id",
		"vcpus",
	}
	powerMembers = []string{
		"display_name", "host", "instance_id", "instance_type", "memory_mb", "state",
		"state_description", "tenant_id", "user_id", "vcpus",
	}
	shelveMembers = append(slices.Clone(powerMembers), "ephemeral_gb", "root_gb")
	resizeMembers = []string{
		"availability_zone", "created_at", "display_name", "ephemeral_gb", "host", "instance_id",
		"instance_type", "instance_type_id", "launched_at", "memory_mb", "root_gb", "state",
		"state_description", "tenant_id", "user_id", "vcpus",
	}
	volumeCreateMembers = []string{
		"availability_zone", "created_at", "display_name", "host", "launched_at",
		"replication_status", "size", "status", "tenant_id", "user_id", "volume_id", "volume_type",
	}
	volumeStateMembers = []string{
		"created_at", "display_name", "host", "size", "status", "tenant_id", "user_id", "volume_id",
		"volume_type",
	}
	imageMembers = []string{
		"checksum", "container_format", "created_at", "disk_format", "id", "min_disk", "min_ram",
		"name", "owner", "protected", "size", "status", "updated_at", "virtual_size", "visibility",
	}
)

// The members the catalogue adds to those sets, and the ones of the resources
// no billable notification is sent about. A CADF record is one skeleton: an
// authentication is the shortest form of it, a barbican request names the call
// on top of that, and the response the status of the call on top of that again.
var (
	updateMembers = append(slices.Clone(powerMembers),
		"audit_period_beginning", "audit_period_ending", "bandwidth", "new_task_state", "old_state",
		"old_task_state")
	existsMembers = append(slices.Clone(createMembers),
		"audit_period_beginning", "audit_period_ending", "bandwidth", "image_meta")
	// The attachment list of a volume connected to nothing is empty, which is
	// the member with nothing under it, and the one of a volume a server holds
	// carries the record cinder wrote. The two halves of an attach and of a
	// detach are one of each.
	attachmentMembers = append(slices.Clone(volumeStateMembers), "volume_attachment")
	attachedMembers   = append(slices.Clone(volumeStateMembers),
		"volume_attachment.attach_mode", "volume_attachment.attach_status",
		"volume_attachment.attach_time", "volume_attachment.attached_host",
		"volume_attachment.id", "volume_attachment.instance_uuid",
		"volume_attachment.mountpoint", "volume_attachment.volume_id")
	schedulerMembers = []string{
		"request_spec.image.id",
		"request_spec.instance_properties.availability_zone",
		"request_spec.instance_properties.display_name",
		"request_spec.instance_properties.ephemeral_gb",
		"request_spec.instance_properties.memory_mb",
		"request_spec.instance_properties.project_id",
		"request_spec.instance_properties.root_gb",
		"request_spec.instance_properties.user_id",
		"request_spec.instance_properties.uuid",
		"request_spec.instance_properties.vcpus",
		"request_spec.instance_type.ephemeral_gb",
		"request_spec.instance_type.flavorid",
		"request_spec.instance_type.id",
		"request_spec.instance_type.memory_mb",
		"request_spec.instance_type.name",
		"request_spec.instance_type.root_gb",
		"request_spec.instance_type.vcpus",
		"request_spec.num_instances",
	}
	keypairMembers = []string{"key_name", "tenant_id", "user_id"}
	portMembers    = []string{
		"port.admin_state_up", "port.binding:host_id", "port.binding:vif_type", "port.device_id",
		"port.device_owner", "port.fixed_ips.ip_address", "port.fixed_ips.subnet_id", "port.id",
		"port.mac_address", "port.name", "port.network_id", "port.project_id",
		"port.security_groups", "port.status", "port.tenant_id",
	}
	securityGroupRuleMembers = []string{
		"security_group_rule.description", "security_group_rule.direction",
		"security_group_rule.ethertype", "security_group_rule.port_range_max",
		"security_group_rule.port_range_min", "security_group_rule.project_id",
		"security_group_rule.protocol", "security_group_rule.remote_group_id",
		"security_group_rule.remote_ip_prefix", "security_group_rule.security_group_id",
		"security_group_rule.tenant_id",
	}
	authenticateMembers = []string{
		"action", "eventTime", "eventType", "id", "initiator.host.address", "initiator.id",
		"initiator.project_id", "initiator.typeURI", "observer.id", "observer.typeURI", "outcome",
		"target.id", "target.typeURI", "typeURI",
	}
	auditRequestMembers  = append(slices.Clone(authenticateMembers), "requestPath", "target.addresses.url")
	auditResponseMembers = append(slices.Clone(auditRequestMembers), "reason.reasonCode", "reason.reasonType")
	recordSetMembers     = []string{
		"action", "created_at", "id", "name", "project_id", "records", "status", "ttl", "type",
		"version", "zone_id", "zone_name",
	}
)

// noiseMembers is the payload of every type of the catalogue, by the members it
// carries. The collector maps none of these types, so none of them was ever
// recorded from a real deployment and
// TestRenderedPayloadsCarryTheRecordedMembers has no fixture to hold them
// against. This table is that fixture: a builder that drops a member or renames
// one changes a payload nothing else in the package reads.
var noiseMembers = map[string][]string{
	"scheduler.select_destinations.start":   schedulerMembers,
	"scheduler.select_destinations.end":     schedulerMembers,
	"compute.instance.create.start":         createMembers,
	"compute.instance.update":               updateMembers,
	"compute.instance.exists":               existsMembers,
	"compute.instance.delete.start":         shelveMembers,
	"compute.instance.shutdown.start":       shelveMembers,
	"compute.instance.shutdown.end":         shelveMembers,
	"compute.instance.shelve_offload.start": shelveMembers,
	"compute.instance.unshelve.start":       shelveMembers,
	"compute.instance.power_off.start":      powerMembers,
	"compute.instance.power_on.start":       powerMembers,
	"compute.instance.resize.start":         resizeMembers,
	"compute.instance.finish_resize.start":  resizeMembers,
	"keypair.import.start":                  keypairMembers,
	"keypair.import.end":                    keypairMembers,
	"keypair.delete.start":                  keypairMembers,
	"keypair.delete.end":                    keypairMembers,

	"volume.create.start":          volumeCreateMembers,
	"volume.delete.start":          volumeStateMembers,
	"volume.resize.start":          volumeStateMembers,
	"volume.transfer.accept.start": volumeStateMembers,
	"volume.attach.start":          attachmentMembers,
	"volume.attach.end":            attachedMembers,
	"volume.detach.start":          attachedMembers,
	"volume.detach.end":            attachmentMembers,

	"network.create.start": {"network.admin_state_up", "network.name"},
	"network.create.end": {
		"network.admin_state_up", "network.description", "network.id", "network.mtu",
		"network.name", "network.project_id", "network.router:external", "network.shared",
		"network.status", "network.subnets", "network.tenant_id",
	},
	"network.delete.start": {"network_id"},
	"network.delete.end":   {"network_id"},
	"subnet.create.start": {
		"subnet.cidr", "subnet.ip_version", "subnet.name", "subnet.network_id",
	},
	"subnet.create.end": {
		"subnet.allocation_pools.end", "subnet.allocation_pools.start", "subnet.cidr",
		"subnet.dns_nameservers", "subnet.enable_dhcp", "subnet.gateway_ip", "subnet.id",
		"subnet.ip_version", "subnet.name", "subnet.network_id", "subnet.project_id",
		"subnet.tenant_id",
	},
	"subnet.delete.start": {"subnet_id"},
	"subnet.delete.end":   {"subnet_id"},
	"router.create.start": {
		"router.admin_state_up", "router.external_gateway_info.network_id", "router.name",
	},
	"router.create.end": {
		"router.admin_state_up", "router.external_gateway_info.enable_snat",
		"router.external_gateway_info.network_id", "router.id", "router.name", "router.project_id",
		"router.status", "router.tenant_id",
	},
	"router.interface.create": {
		"router_interface.id", "router_interface.network_id", "router_interface.port_id",
		"router_interface.subnet_id", "router_interface.subnet_ids", "router_interface.tenant_id",
	},
	"router.interface.delete": {
		"router_interface.id", "router_interface.network_id", "router_interface.port_id",
		"router_interface.subnet_id", "router_interface.subnet_ids", "router_interface.tenant_id",
	},
	"router.delete.start":         {"router_id"},
	"router.delete.end":           {"router_id"},
	"security_group.create.start": {"security_group.description", "security_group.name"},
	"security_group.create.end": {
		"security_group.description", "security_group.id", "security_group.name",
		"security_group.project_id", "security_group.security_group_rules",
		"security_group.tenant_id",
	},
	"security_group.delete.start":      {"security_group_id"},
	"security_group.delete.end":        {"security_group_id"},
	"security_group_rule.create.start": securityGroupRuleMembers,
	"security_group_rule.create.end": append(slices.Clone(securityGroupRuleMembers),
		"security_group_rule.id"),
	"port.create.start": {
		"port.admin_state_up", "port.device_id", "port.device_owner", "port.name",
		"port.network_id", "port.project_id", "port.tenant_id",
	},
	"port.create.end":   portMembers,
	"port.update.start": {"port.binding:host_id", "port.device_id", "port.device_owner"},
	"port.update.end":   portMembers,
	"port.delete.start": {"port_id"},
	"port.delete.end":   {"port_id"},

	"image.prepare":  imageMembers,
	"image.activate": imageMembers,

	"identity.project.created": {"resource_info"},
	"identity.user.created":    {"resource_info"},
	"identity.authenticate":    authenticateMembers,

	"dns.zone.create": {
		"action", "created_at", "email", "id", "name", "project_id", "serial", "status", "ttl",
		"type", "version",
	},
	"dns.recordset.create": recordSetMembers,
	"dns.recordset.delete": recordSetMembers,

	"audit.http.request":  auditRequestMembers,
	"audit.http.response": auditResponseMembers,
}

// TestNoisePayloadsCarryTheirMembers is what
// TestRenderedPayloadsCarryTheRecordedMembers is for the billable month: the
// payload of every type of the catalogue, held against the members it is to
// carry. The two together cover every type a month renders.
func TestNoisePayloadsCarryTheirMembers(t *testing.T) {
	if len(noiseMembers) != len(noiseTypes) {
		t.Errorf("the table pins %d payloads and the catalogue names %d types, want one per type",
			len(noiseMembers), len(noiseTypes))
	}
	for _, eventType := range noiseTypes {
		if _, ok := noiseMembers[eventType]; !ok {
			t.Fatalf("the catalogue type %s has no pinned members, want every type of it in the "+
				"table: a payload nothing pins is one a builder may quietly drop a member from",
				eventType)
		}
	}

	seen := make(map[string]bool, len(noiseTypes))
	for _, transition := range generateMonth(t, 1, july2026, testCloud) {
		pinned, ok := noiseMembers[transition.EventType]
		if !ok || seen[transition.EventType] {
			continue
		}
		seen[transition.EventType] = true

		got := memberShape(parse(t, render(t, transition)).Payload)
		want := slices.Sorted(slices.Values(pinned))
		if !slices.Equal(got, want) {
			t.Errorf("%s payload members = %v, want %v (rendered and not pinned: %v; pinned and not rendered: %v)",
				transition.EventType, got, want, missingFrom(want, got), missingFrom(got, want))
		}
	}
	for _, eventType := range noiseTypes {
		if !seen[eventType] {
			t.Errorf("the month renders no %s, want every type of the catalogue: a pinned payload "+
				"nothing renders is one the table alone agrees with", eventType)
		}
	}
}

func TestRenderWritesANullPublisherForAnEmptyPublisherID(t *testing.T) {
	// The publisher is read as text and not through a decoder: a JSON null and
	// an empty JSON string both arrive as the empty Go string, so the bytes on
	// the wire are the only place the two are told apart.
	message := func(t *testing.T, transition Transition) string {
		t.Helper()

		var envelope map[string]string
		if err := json.Unmarshal(render(t, transition), &envelope); err != nil {
			t.Fatalf("unmarshalling the rendered envelope: %v", err)
		}
		return envelope["oslo.message"]
	}

	t.Run("octavia publishes without one", func(t *testing.T) {
		got := message(t, Transition{
			EventType: "octavia.loadbalancer.create.end",
			Payload:   map[string]any{"loadbalancer_id": "5e6f7081-92a3-4b4c-8d5e-6f708192a3b4"},
		})

		const want = `"publisher_id":null`
		if !strings.Contains(got, want) {
			t.Errorf("rendered notification = %s, want it to carry %s", got, want)
		}
	})

	t.Run("every other service names its instance", func(t *testing.T) {
		got := message(t, Transition{
			EventType:   "compute.instance.create.end",
			PublisherID: "compute.compute-01",
			Payload:     map[string]any{"instance_id": "8f7e6d5c-4b3a-4291-8071-6f5e4d3c2b1a"},
		})

		const want = `"publisher_id":"compute.compute-01"`
		if !strings.Contains(got, want) {
			t.Errorf("rendered notification = %s, want it to carry %s", got, want)
		}
	})
}

// TestFloatingIPCreateReportsWhatTheAddressPointsAt covers the one branch of a
// floating IP payload: an address a tenant allocates points at nothing and is
// down, while the address of a load balancer's VIP port is associated the
// moment it exists. Both sides carry the same members, so their values are the
// only place the two are told apart.
func TestFloatingIPCreateReportsWhatTheAddressPointsAt(t *testing.T) {
	p := &project{id: "4f6b8d0a2c1e3f5a7b9c0d2e4f6a8b0c"}
	tests := []struct {
		name string
		fip  *floatingIP
		want map[string]any
	}{
		{
			name: "an address a tenant allocates points at nothing",
			fip:  &floatingIP{id: "1a2b3c4d-5e6f-4071-8293-a4b5c6d7e8f9", address: "203.0.113.7"},
			want: map[string]any{
				"status":           "DOWN",
				"port_id":          nil,
				"router_id":        nil,
				"fixed_ip_address": nil,
			},
		},
		{
			name: "the address of a VIP port is associated at once",
			fip: &floatingIP{
				id:           "2b3c4d5e-6f70-4182-93a4-b5c6d7e8f9a0",
				address:      "203.0.113.8",
				portID:       "3c4d5e6f-7081-4293-a4b5-c6d7e8f9a0b1",
				fixedAddress: "10.250.1.10",
				routerID:     "4d5e6f70-8192-43a4-b5c6-d7e8f9a0b1c2",
			},
			want: map[string]any{
				"status":           "ACTIVE",
				"port_id":          "3c4d5e6f-7081-4293-a4b5-c6d7e8f9a0b1",
				"router_id":        "4d5e6f70-8192-43a4-b5c6-d7e8f9a0b1c2",
				"fixed_ip_address": "10.250.1.10",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			notification := parse(t, render(t, Transition{
				EventType: "floatingip.create.end",
				Payload:   floatingIPCreatePayload(p, test.fip, "5e6f7081-92a3-44b5-c6d7-e8f9a0b1c2d3"),
			}))

			address, ok := notification.Payload["floatingip"].(map[string]any)
			if !ok {
				t.Fatalf("the payload carries %v under floatingip, want the address neutron nests there",
					notification.Payload["floatingip"])
			}
			for member, want := range test.want {
				if got := address[member]; got != want {
					t.Errorf("floatingip.%s = %v, want %v: an address that is associated reports its "+
						"port, the address behind it, and its router, and one that is not reports none "+
						"of the three", member, got, want)
				}
			}
		})
	}
}

// TestLoadBalancerPayloadDescribesEveryListener covers a balancer that carries
// more listeners than the catalog names. The catalog is read from the front
// again rather than past its end, because a shoot that publishes another port
// is a change in gardener.go and the renderer is where it would otherwise end
// the run.
func TestLoadBalancerPayloadDescribesEveryListener(t *testing.T) {
	lb := &loadBalancer{
		id:        "6f708192-a3b4-45c6-d7e8-f9a0b1c2d3e4",
		name:      "kube_service_shoot--alpha--api-prod_ingress_nginx-ingress-controller",
		vipPortID: "708192a3-b4c5-46d7-e8f9-a0b1c2d3e4f5",
		poolIDs:   []string{"8192a3b4-c5d6-47e8-f9a0-b1c2d3e4f506"},
	}
	for index := range len(listenerSpecs) + 1 {
		lb.listenerIDs = append(lb.listenerIDs, fmt.Sprintf("92a3b4c5-d6e7-48f9-a0b1-c2d3e4f5061%d", index))
	}

	payload := loadBalancerPayload(&project{id: "5a7c9e1b3d5f7091a2b4c6d8e0f2a4b6"}, &shoot{}, lb, true)

	listeners, _ := payload["listeners"].([]any)
	if len(listeners) != len(lb.listenerIDs) {
		t.Fatalf("the update of a balancer with %d listeners describes %d of them, want all of them: "+
			"the mapping books the count the array carries", len(lb.listenerIDs), len(listeners))
	}
}

// TestLoadBalancerPayloadWithoutAPool covers a balancer that carries a listener
// and no pool, the way a headless or UDP-only service publishes one. The
// listener points at nothing rather than at a pool the balancer never had, and
// the renderer does not end the run over it.
func TestLoadBalancerPayloadWithoutAPool(t *testing.T) {
	lb := &loadBalancer{
		id:          "6f708192-a3b4-45c6-d7e8-f9a0b1c2d3e4",
		name:        "kube_service_shoot--alpha--api-prod_default_api",
		vipPortID:   "708192a3-b4c5-46d7-e8f9-a0b1c2d3e4f5",
		listenerIDs: []string{"92a3b4c5-d6e7-48f9-a0b1-c2d3e4f50610"},
	}

	payload := loadBalancerPayload(&project{id: "5a7c9e1b3d5f7091a2b4c6d8e0f2a4b6"}, &shoot{}, lb, true)

	listeners, _ := payload["listeners"].([]any)
	if len(listeners) != 1 {
		t.Fatalf("the update of a balancer with one listener describes %d of them, want one: "+
			"the mapping books the count the array carries", len(listeners))
	}
	listener, _ := listeners[0].(map[string]any)
	if got := listener["default_pool_id"]; got != nil {
		t.Errorf("listener.default_pool_id = %v, want nil: a balancer with no pool has no pool "+
			"to point its listener at", got)
	}
}

func TestRenderReportsAMarshalError(t *testing.T) {
	// A channel is the one thing encoding/json refuses outright, which is how a
	// payload builder that put an unencodable value in a member would surface.
	_, err := Render(Transition{
		EventType: "compute.instance.create.end",
		Payload:   map[string]any{"bad": make(chan int)},
	})
	if err == nil {
		t.Fatal("Render() error = nil, want the marshal failure reported")
	}

	const prefix = "rendering compute.instance.create.end: "
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("Render() error = %q, want it to start with %q", err, prefix)
	}
}

// TestSecurityGroupRulePayloadNamesTheCreatedRuleAlone covers the two forms a
// rule of a shoot's group is reported in. The request neutron was handed
// carries no id, because neutron hands that one out with the rule it created,
// and the two forms are otherwise the same rule.
func TestSecurityGroupRulePayloadNamesTheCreatedRuleAlone(t *testing.T) {
	const ruleID = "0a1b2c3d-4e5f-4061-8273-94a5b6c7d8e9"
	p := &project{id: "5a7c9e1b3d5f7091a2b4c6d8e0f2a4b6"}
	s := &shoot{
		technicalID:     "shoot--beta--batch",
		securityGroupID: "1b2c3d4e-5f60-4172-8394-a5b6c7d8e9f0",
	}

	rule := func(index int, id string) map[string]any {
		payload, ok := securityGroupRulePayload(p, s, index, id)["security_group_rule"].(map[string]any)
		if !ok {
			t.Fatalf("rule %d carries no security_group_rule, want the rule neutron nests there", index)
		}
		return payload
	}

	requested, created := rule(0, ""), rule(0, ruleID)
	if _, ok := requested["id"]; ok {
		t.Errorf("the requested rule reports id = %v, want no id at all: neutron hands the id out "+
			"with the rule it created", requested["id"])
	}
	if got := created["id"]; got != ruleID {
		t.Errorf("the created rule reports id = %v, want %s", got, ruleID)
	}
	if len(created) != len(requested)+1 {
		t.Errorf("the requested rule carries %d members and the created one %d, want the id to be "+
			"the one member between them", len(requested), len(created))
	}

	// The second rule narrows nothing but the group it comes from, which is
	// where the nulls of a rule are.
	group := rule(1, ruleID)
	if got := group["remote_group_id"]; got != s.securityGroupID {
		t.Errorf("the second rule reports remote_group_id = %v, want %s, the group itself",
			got, s.securityGroupID)
	}
	for _, member := range []string{"port_range_max", "port_range_min", "protocol", "remote_ip_prefix"} {
		if got := group[member]; got != nil {
			t.Errorf("the second rule reports %s = %v, want null: a rule that names neither a port "+
				"range nor a protocol narrows neither", member, got)
		}
	}
}

// TestAuditPayloadReportsTheStatusOnTheResponse covers the one branch of a CADF
// record: the request has no HTTP status yet and the response carries it. Both
// records of a call carry the same target, so the status is where the two are
// told apart.
func TestAuditPayloadReportsTheStatusOnTheResponse(t *testing.T) {
	const (
		path     = "/v1/secrets"
		targetID = "2c3d4e5f-6071-4283-94a5-b6c7d8e9f0a1"
	)
	p := &project{id: "5a7c9e1b3d5f7091a2b4c6d8e0f2a4b6", userID: "6b8d0a2c4e6f8091b3d5f7a9c1e3f507"}
	at := time.Date(2026, 7, 7, 19, 2, 41, 0, time.UTC)

	request := auditPayload(p, "os-sim", "3d4e5f60-7182-4394-a5b6-c7d8e9f0a1b2", "create", "pending",
		path, secretsType, targetID, 0, at)
	if _, ok := request["reason"]; ok {
		t.Errorf("the request reports reason = %v, want none: a call that is not answered yet has no "+
			"HTTP status", request["reason"])
	}

	response := auditPayload(p, "os-sim", "4e5f6071-8293-44a5-b6c7-d8e9f0a1b2c3", "create", "success",
		path, secretsType, targetID, 201, at.Add(time.Second))
	reason, ok := response["reason"].(map[string]any)
	if !ok {
		t.Fatalf("the response carries %v under reason, want the HTTP status of the call",
			response["reason"])
	}
	if got := reason["reasonCode"]; got != "201" {
		t.Errorf("the response reports reasonCode = %v, want \"201\", the status as a string", got)
	}

	target, _ := response["target"].(map[string]any)
	addresses, _ := target["addresses"].([]any)
	address, _ := addresses[0].(map[string]any)
	want := "https://barbican.os-sim.example:9311" + path
	if got := address["url"]; got != want {
		t.Errorf("the record names the endpoint %v, want %s: two clouds' records name two endpoints",
			got, want)
	}
}

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

// memberShape is a payload's structure as its sorted member names, with a
// nested object's members named under their parent. Neutron nests its resource
// one level down, so comparing the top level alone would let a floating IP
// payload pass with nothing in it.
func memberShape(payload map[string]any) []string {
	names := make([]string, 0, len(payload))
	for name, value := range payload {
		// An empty object is a member with nothing under it, so flattening it
		// would leave it out of the shape. Naming it is what lets a member test
		// catch a builder that drops one: the noise payloads carry an empty
		// bandwidth and an empty image_meta.
		nested, ok := value.(map[string]any)
		if !ok || len(nested) == 0 {
			names = append(names, name)
			continue
		}
		for _, inner := range memberShape(nested) {
			names = append(names, name+"."+inner)
		}
	}
	slices.Sort(names)
	return names
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

package simulator

import (
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
		nested, ok := value.(map[string]any)
		if !ok {
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

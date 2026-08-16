package openstack

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/ids"
	"github.com/b42labs/tally/internal/core/testkit"
)

// goldenCloud is the cloud the golden events were written for.
const goldenCloud = "os-dev"

// notify builds a parsed notification carrying the members the mapping reads, so
// a test states only what it varies. The context ids are empty, which leaves the
// payload as the only project a case does not set them in on purpose.
func notify(eventType string, payload map[string]any) Notification {
	return Notification{
		MessageID: "0d1c2b3a-4958-4677-8695-a4b3c2d1e0f9",
		EventType: eventType,
		Timestamp: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		Payload:   payload,
	}
}

// canonicalJSON re-encodes a document so that two of them compare equal unless
// they differ in content: Go sorts the keys of a map on the way out, and
// decoding the numbers as literals keeps 0.5 the value it was written as instead
// of turning it into a float that renders differently.
func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("re-encoding %s: %v", raw, err)
	}
	return string(encoded)
}

// TestMapNotificationMatchesGoldenEvents runs every captured notification
// through the mapping and holds the result against the event it is expected to
// become, byte for byte. A fixture with no expected event is one the table is
// meant to skip.
func TestMapNotificationMatchesGoldenEvents(t *testing.T) {
	notificationDir := filepath.Join("testdata", "golden", "notifications")
	eventDir := filepath.Join("testdata", "golden", "events")

	fixtures, err := filepath.Glob(filepath.Join(notificationDir, "*.json"))
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	if len(fixtures) == 0 {
		t.Fatalf("no notification fixtures in %s", notificationDir)
	}

	for _, fixture := range fixtures {
		name := filepath.Base(fixture)
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatalf("reading the notification: %v", err)
			}
			parsed, err := ParseEnvelope(body)
			if err != nil {
				t.Fatalf("ParseEnvelope() error = %v, want nil", err)
			}
			got, ok := MapNotification(parsed, goldenCloud)

			want, err := os.ReadFile(filepath.Join(eventDir, name))
			if errors.Is(err, fs.ErrNotExist) {
				if ok {
					t.Fatalf("MapNotification() ok = true, want false: %s has no expected event", name)
				}
				return
			}
			if err != nil {
				t.Fatalf("reading the expected event: %v", err)
			}
			if !ok {
				t.Fatal("MapNotification() ok = false, want true")
			}

			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("encoding the event: %v", err)
			}
			if gotJSON, wantJSON := canonicalJSON(t, raw), canonicalJSON(t, want); gotJSON != wantJSON {
				t.Errorf("event = %s, want %s", gotJSON, wantJSON)
			}
			testkit.AssertValidEvent(t, raw)
		})
	}

	t.Run("every expected event has a notification producing it", func(t *testing.T) {
		expected, err := filepath.Glob(filepath.Join(eventDir, "*.json"))
		if err != nil {
			t.Fatalf("Glob: %v", err)
		}
		for _, path := range expected {
			name := filepath.Base(path)
			if _, err := os.Stat(filepath.Join(notificationDir, name)); err != nil {
				t.Errorf("%s expects an event no fixture produces: %v", name, err)
			}
		}
	})
}

// TestVMStateNormalizesNovaStates covers the map the reconciliation adapter
// shares, including the states it has no entry for: those are the cloud's own
// vocabulary and are recorded as they arrived.
func TestVMStateNormalizesNovaStates(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{name: "active stays active", payload: map[string]any{"state": "active"}, want: "active"},
		{name: "stopped becomes shutoff", payload: map[string]any{"state": "stopped"}, want: "shutoff"},
		{
			name:    "shelved_offloaded becomes shelved",
			payload: map[string]any{"state": "shelved_offloaded"},
			want:    "shelved",
		},
		{name: "paused stays paused", payload: map[string]any{"state": "paused"}, want: "paused"},
		{name: "suspended stays suspended", payload: map[string]any{"state": "suspended"}, want: "suspended"},
		{name: "error stays error", payload: map[string]any{"state": "error"}, want: "error"},
		{
			name:    "a state the map has no entry for passes through unchanged",
			payload: map[string]any{"state": "rescued"},
			want:    "rescued",
		},
		{name: "a payload without a state yields an empty one", payload: map[string]any{}, want: ""},
		{
			name:    "a state that is not a string yields an empty one",
			payload: map[string]any{"state": json.Number("1")},
			want:    "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := vmState(tc.payload); got != tc.want {
				t.Errorf("vmState() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestVolumeStatusFallsBackToAvailable covers the rule the volume events that
// change a size or an owner rest on: cinder does not always repeat the status in
// them, and an event without a state is dead-lettered by the API, so an absent
// status is read as the state a usable volume is in.
func TestVolumeStatusFallsBackToAvailable(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name:    "a reported status is kept",
			payload: map[string]any{"status": "in-use"},
			want:    "in-use",
		},
		{
			name:    "a payload without a status falls back to available",
			payload: map[string]any{},
			want:    "available",
		},
		{
			name:    "a status that is not a string falls back too",
			payload: map[string]any{"status": json.Number("1")},
			want:    "available",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := volumeStatus(tc.payload); got != tc.want {
				t.Errorf("volumeStatus() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMapNotificationRenamesFinishResize pins the alias: nova finishes a resize
// under a second name, both notifications carry the new size, and both are
// booked as one Tally event. The oslo name survives in the provider data, so the
// renamed event can still be traced back.
func TestMapNotificationRenamesFinishResize(t *testing.T) {
	got, ok := MapNotification(notify("compute.instance.finish_resize.end", map[string]any{
		"instance_id":   "instance-1",
		"tenant_id":     "project-1",
		"state":         "active",
		"vcpus":         json.Number("8"),
		"memory_mb":     json.Number("16384"),
		"root_gb":       json.Number("160"),
		"instance_type": "m1.xlarge",
	}), goldenCloud)
	if !ok {
		t.Fatal("MapNotification() ok = false, want true")
	}

	if want := "compute.instance.resize.end"; got.EventType != want {
		t.Errorf("EventType = %q, want %q", got.EventType, want)
	}
	if want := "compute.instance.finish_resize.end"; got.Payload.Provider["oslo_event_type"] != want {
		t.Errorf("provider.oslo_event_type = %v, want %q", got.Payload.Provider["oslo_event_type"], want)
	}
}

// TestMapNotificationSkipsAnUnmappedType covers the types the table has no entry
// for. Most of what a cloud emits is not a lifecycle fact Tally bills on.
func TestMapNotificationSkipsAnUnmappedType(t *testing.T) {
	got, ok := MapNotification(notify("compute.instance.snapshot.end", map[string]any{
		"instance_id": "instance-1",
		"tenant_id":   "project-1",
	}), goldenCloud)
	if ok {
		t.Errorf("MapNotification() ok = true, want false")
	}
	if got.EventID != "" {
		t.Errorf("event = %+v, want the zero Event", got)
	}
}

// TestMapNotificationSkipsImageCreateWithoutSize covers the gate on glance's
// create: an image is registered before its content exists, and booking it then
// would record an image of no size.
func TestMapNotificationSkipsImageCreateWithoutSize(t *testing.T) {
	tests := []struct {
		name string
		size any
		want bool
	}{
		{name: "a create naming no size is skipped", size: nil, want: false},
		{name: "a create with a zero size is skipped", size: json.Number("0"), want: false},
		{name: "a create with a negative size is skipped", size: json.Number("-1"), want: false},
		{name: "a create whose size is not a number is skipped", size: "1073741824", want: false},
		{name: "a create with a positive size is mapped", size: json.Number("1073741824"), want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"id": "image-1", "owner": "project-1"}
			if tc.size != nil {
				payload["size"] = tc.size
			}

			if _, ok := MapNotification(notify("image.create", payload), goldenCloud); ok != tc.want {
				t.Errorf("MapNotification() ok = %t, want %t", ok, tc.want)
			}
		})
	}
}

// TestMapNotificationProducesAnEventFromAnEmptyPayload covers a notification the
// mapping understands nothing in. It still becomes an event, because the
// Reporting API dead-letters it with the rule it broke, and that record is what
// an operator needs to fix the deployment. Dropping it here would be silent.
func TestMapNotificationProducesAnEventFromAnEmptyPayload(t *testing.T) {
	got, ok := MapNotification(notify("compute.instance.create.end", map[string]any{}), goldenCloud)
	if !ok {
		t.Fatal("MapNotification() ok = false, want true")
	}

	if got.ResourceID != "" {
		t.Errorf("ResourceID = %q, want the empty string", got.ResourceID)
	}
	if got.ProjectID != "" {
		t.Errorf("ProjectID = %q, want the empty string", got.ProjectID)
	}
	if got.Payload.State == nil || *got.Payload.State != "" {
		t.Errorf("payload.state = %v, want a pointer to the empty string", got.Payload.State)
	}
	if got.Payload.Size == nil {
		t.Error("payload.size = nil, want an empty size object")
	}
}

// TestMapNotificationResolvesTheProject covers the order the owning project is
// taken in. The payload wins because it describes the resource, and the context
// members are the fallback because services differ in which one they set.
func TestMapNotificationResolvesTheProject(t *testing.T) {
	tests := []struct {
		name           string
		payloadTenant  string
		contextProject string
		contextTenant  string
		want           string
	}{
		{
			name:           "the payload names the project",
			payloadTenant:  "payload-project",
			contextProject: "context-project",
			contextTenant:  "context-tenant",
			want:           "payload-project",
		},
		{
			name:           "a payload without a project falls back to the request context",
			contextProject: "context-project",
			contextTenant:  "context-tenant",
			want:           "context-project",
		},
		{
			name:          "a context without a project falls back to its tenant",
			contextTenant: "context-tenant",
			want:          "context-tenant",
		},
		{
			name: "a notification naming no project at all produces an empty one",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"instance_id": "instance-1"}
			if tc.payloadTenant != "" {
				payload["tenant_id"] = tc.payloadTenant
			}
			notification := notify("compute.instance.delete.end", payload)
			notification.ContextProjectID = tc.contextProject
			notification.ContextTenantID = tc.contextTenant

			got, ok := MapNotification(notification, goldenCloud)
			if !ok {
				t.Fatal("MapNotification() ok = false, want true")
			}
			if got.ProjectID != tc.want {
				t.Errorf("ProjectID = %q, want %q", got.ProjectID, tc.want)
			}
		})
	}
}

// TestMapNotificationRendersQuantitiesExactly holds the converted quantities to
// their exact literals. They travel as JSON numbers rather than as floats, so a
// half gibibyte stays a half gibibyte all the way into the event.
func TestMapNotificationRendersQuantitiesExactly(t *testing.T) {
	tests := []struct {
		name         string
		notification Notification
		member       string
		want         json.Number
	}{
		{
			name: "8192 MB of memory are 8 GiB",
			notification: notify("compute.instance.create.end", map[string]any{
				"memory_mb": json.Number("8192"),
			}),
			member: "ram_gb",
			want:   json.Number("8"),
		},
		{
			name: "512 MB of memory are half a GiB",
			notification: notify("compute.instance.create.end", map[string]any{
				"memory_mb": json.Number("512"),
			}),
			member: "ram_gb",
			want:   json.Number("0.5"),
		},
		{
			name: "1610612736 bytes of image are one and a half GiB",
			notification: notify("image.upload", map[string]any{
				"size": json.Number("1610612736"),
			}),
			member: "size_gb",
			want:   json.Number("1.5"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MapNotification(tc.notification, goldenCloud)
			if !ok {
				t.Fatal("MapNotification() ok = false, want true")
			}
			value, isNumber := got.Payload.Size[tc.member].(json.Number)
			if !isNumber {
				t.Fatalf("size[%s] is %T, want json.Number", tc.member, got.Payload.Size[tc.member])
			}
			if value != tc.want {
				t.Errorf("size[%s] = %q, want %q", tc.member, value, tc.want)
			}
		})
	}
}

// TestMapNotificationSkipsUnreadableSizeMembers covers a payload that names a
// size member in a shape the mapping cannot read. The member is left out rather
// than defaulted, so the event says only what the service reported and the
// server refuses it against the registered size schema.
func TestMapNotificationSkipsUnreadableSizeMembers(t *testing.T) {
	got, ok := MapNotification(notify("compute.instance.create.end", map[string]any{
		"instance_id":   "instance-1",
		"tenant_id":     "project-1",
		"state":         "active",
		"vcpus":         "four",
		"memory_mb":     json.Number("2048"),
		"instance_type": "m1.small",
	}), goldenCloud)
	if !ok {
		t.Fatal("MapNotification() ok = false, want true")
	}

	if _, present := got.Payload.Size["vcpus"]; present {
		t.Errorf("size[vcpus] = %v, want it left out", got.Payload.Size["vcpus"])
	}
	if _, present := got.Payload.Size["disk_gb"]; present {
		t.Errorf("size[disk_gb] = %v, want it left out", got.Payload.Size["disk_gb"])
	}
	if want := json.Number("2"); got.Payload.Size["ram_gb"] != want {
		t.Errorf("size[ram_gb] = %v, want %q", got.Payload.Size["ram_gb"], want)
	}
}

// TestMapNotificationSumsTheInstanceDisks covers nova reporting the two disks of
// an instance separately. An instance without ephemeral storage reports none,
// and that half counts as zero rather than voiding the disk size. A disk that
// arrived as a fractional literal counts too: a deployment whose notification
// path re-serializes the payload numbers sends 20.0 where nova sent 20, and
// dropping that half would book the instance with the ephemeral zero alone.
func TestMapNotificationSumsTheInstanceDisks(t *testing.T) {
	tests := []struct {
		name   string
		disks  map[string]any
		want   any
		absent bool
	}{
		{
			name:  "a root disk alone is the whole disk",
			disks: map[string]any{"root_gb": json.Number("20")},
			want:  json.Number("20"),
		},
		{
			name:  "the two disks are summed",
			disks: map[string]any{"root_gb": json.Number("20"), "ephemeral_gb": json.Number("40")},
			want:  json.Number("60"),
		},
		{
			name:  "a root disk written as a decimal literal still counts",
			disks: map[string]any{"root_gb": json.Number("20.0"), "ephemeral_gb": json.Number("0")},
			want:  json.Number("20"),
		},
		{
			name:   "a payload naming neither disk reports no disk at all",
			disks:  map[string]any{},
			absent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{
				"instance_id": "instance-1",
				"tenant_id":   "project-1",
				"state":       "active",
			}
			for key, value := range tc.disks {
				payload[key] = value
			}

			got, ok := MapNotification(notify("compute.instance.create.end", payload), goldenCloud)
			if !ok {
				t.Fatal("MapNotification() ok = false, want true")
			}

			disk, present := got.Payload.Size["disk_gb"]
			if tc.absent {
				if present {
					t.Errorf("size[disk_gb] = %v, want the member left out", disk)
				}
				return
			}
			if disk != tc.want {
				t.Errorf("size[disk_gb] = %v (%T), want %v", disk, disk, tc.want)
			}
		})
	}
}

// TestMapNotificationBoundsWhatAPayloadNumberMayCost covers the magnitude
// encoding/json does not check. A JSON number's syntax is all it validates, so
// 1e2000000000 survives the decode as a json.Number and parses into a decimal
// whose exponent is two billion. The first arithmetic that value takes part in
// expands it into a two-billion-digit integer: roughly 830 MB of heap and
// minutes of CPU inside one Add, from a 25-byte literal anyone who may publish
// on a notification exchange can write. The delivery is never acknowledged, so
// the broker hands the same message to the next process.
//
// A number past the bounds is treated as unreadable, which is what every other
// value this mapping cannot use is treated as: the member is left out and the
// event still goes out.
func TestMapNotificationBoundsWhatAPayloadNumberMayCost(t *testing.T) {
	tests := []struct {
		name   string
		number string
	}{
		{name: "an exponent that would expand into gigabytes", number: "1e2000000000"},
		{name: "a negative exponent of the same size", number: "1e-2000000000"},
		{name: "a mantissa longer than any reported quantity", number: strings.Repeat("9", 41)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Both quantities that reach decimal arithmetic: diskGB sums root_gb,
			// and memory_mb is divided into ram_gb.
			got, ok := MapNotification(notify("compute.instance.create.end", map[string]any{
				"instance_id": "instance-1",
				"tenant_id":   "project-1",
				"state":       "active",
				"root_gb":     json.Number(tc.number),
				"memory_mb":   json.Number(tc.number),
			}), goldenCloud)
			if !ok {
				t.Fatal("MapNotification() ok = false, want true")
			}

			for _, member := range []string{"disk_gb", "ram_gb"} {
				if value, present := got.Payload.Size[member]; present {
					t.Errorf("size[%s] = %v, want the unusable number left out", member, value)
				}
			}
		})
	}
}

// TestMapNotificationDerivesAnEventIDWithoutMessageID covers a deployment whose
// notifications carry no message id. The derived id has to be the same on every
// redelivery, or ingestion would store the same fact twice.
func TestMapNotificationDerivesAnEventIDWithoutMessageID(t *testing.T) {
	notification := notify("compute.instance.power_on.end", map[string]any{
		"instance_id": "instance-1",
		"tenant_id":   "project-1",
	})
	notification.MessageID = ""

	got, ok := MapNotification(notification, goldenCloud)
	if !ok {
		t.Fatal("MapNotification() ok = false, want true")
	}
	want := ids.DeterministicEventID("openstack", goldenCloud, "instance-1",
		"compute.instance.power_on", notification.Timestamp)
	if got.EventID != want {
		t.Errorf("EventID = %q, want %q", got.EventID, want)
	}

	testkit.AssertDeterministicIDs(t, func() string {
		mapped, _ := MapNotification(notification, goldenCloud)
		return mapped.EventID
	})
}

// TestMapNotificationReadsTheIPVersion covers the one billable property of a
// floating IP. An address the mapping cannot read counts as IPv4, since that is
// what a deployment allocates unless it says otherwise, and skipping the event
// would cost the address its whole billing record.
func TestMapNotificationReadsTheIPVersion(t *testing.T) {
	tests := []struct {
		name       string
		floatingIP map[string]any
		want       int
	}{
		{
			name:       "an IPv4 address is version 4",
			floatingIP: map[string]any{"floating_ip_address": "203.0.113.42"},
			want:       4,
		},
		{
			name:       "an IPv6 address is version 6",
			floatingIP: map[string]any{"floating_ip_address": "2001:db8::1"},
			want:       6,
		},
		{
			name:       "an absent address falls back to version 4",
			floatingIP: map[string]any{},
			want:       4,
		},
		{
			name:       "an unreadable address falls back to version 4",
			floatingIP: map[string]any{"floating_ip_address": "no-address-at-all"},
			want:       4,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MapNotification(notify("floatingip.create.end", map[string]any{
				"floatingip": tc.floatingIP,
			}), goldenCloud)
			if !ok {
				t.Fatal("MapNotification() ok = false, want true")
			}
			if got.Payload.Size["ip_version"] != tc.want {
				t.Errorf("size[ip_version] = %v, want %d", got.Payload.Size["ip_version"], tc.want)
			}
		})
	}
}

package event_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/event"
)

// validEvent is a fully populated create event. Tests mutate one field of a copy
// to isolate the rule they exercise.
func validEvent() event.Event {
	state := "active"
	return event.Event{
		EventID:      "openstack-abc123",
		Timestamp:    time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC),
		EventType:    "compute.instance.create.end",
		Platform:     "openstack",
		Cloud:        "os-prod-eu1",
		ResourceType: "instance",
		ResourceID:   "i-1",
		ProjectID:    "p-1",
		Source:       event.SourceCollector,
		Payload: event.PayloadEnvelope{
			State: &state,
			Size:  map[string]any{"vcpus": 4},
		},
	}
}

func TestValidateAcceptsCompleteCreateEvent(t *testing.T) {
	e := validEvent()
	if err := e.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateZeroValueNamesEveryMissingField(t *testing.T) {
	var e event.Event

	err := e.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error")
	}

	// The zero value breaks every rule at once, and a caller fixing it should see
	// the whole list rather than one field per attempt.
	for _, field := range []string{
		"event_id", "timestamp", "event_type", "platform", "cloud",
		"resource_type", "resource_id", "project_id", "payload.state",
	} {
		if !strings.Contains(err.Error(), field) {
			t.Errorf("Validate() error does not mention %q: %v", field, err)
		}
	}
}

func TestValidateEventIDLength(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		wantErr bool
	}{
		{name: "empty", id: "", wantErr: true},
		{name: "one character", id: "x"},
		{name: "256 characters", id: strings.Repeat("x", 256)},
		{name: "257 characters", id: strings.Repeat("x", 257), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			e.EventID = tc.id

			err := e.Validate()
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Validate() = nil, want an error for a %d character id", len(tc.id))
			case tc.wantErr && !strings.Contains(err.Error(), "event_id"):
				t.Fatalf("Validate() error does not mention event_id: %v", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestValidateStateLength(t *testing.T) {
	tests := []struct {
		name    string
		state   string
		wantErr bool
	}{
		{name: "one character", state: "x"},
		{name: "512 characters", state: strings.Repeat("x", 512)},
		{name: "513 characters", state: strings.Repeat("x", 513), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			e.Payload.State = &tc.state

			err := e.Validate()
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Validate() = nil, want an error for a %d character state", len(tc.state))
			case tc.wantErr && !strings.Contains(err.Error(), "payload.state"):
				t.Fatalf("Validate() error does not mention payload.state: %v", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestValidateEventTypeLength(t *testing.T) {
	// The pattern holds in every case, so only the length rule can refuse them.
	tests := []struct {
		name      string
		eventType string
		wantErr   bool
	}{
		{name: "512 characters", eventType: strings.Repeat("a", 505) + ".create"},
		{name: "513 characters", eventType: strings.Repeat("a", 506) + ".create", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			e.EventType = tc.eventType
			// Keep the payload complete so only the type rule can fail.
			state := "active"
			e.Payload.State = &state
			e.Payload.Size = map[string]any{"vcpus": 4}

			err := e.Validate()
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Validate() = nil, want an error for a %d character event type", len(tc.eventType))
			case tc.wantErr && !strings.Contains(err.Error(), "event_type"):
				t.Fatalf("Validate() error does not mention event_type: %v", err)
			// The reason a refused item is dead-lettered under is this text, and
			// that column is capped by nothing but the value it quotes.
			case tc.wantErr && strings.Contains(err.Error(), tc.eventType):
				t.Fatalf("Validate() error carries the whole event type: %v", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestValidateEventTypePattern(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		wantErr   bool
	}{
		{name: "dotted", eventType: "compute.instance.create.end"},
		{name: "two segments", eventType: "sync.create"},
		{name: "underscores", eventType: "load_balancer.create.end"},
		{name: "single segment", eventType: "create", wantErr: true},
		{name: "empty", eventType: "", wantErr: true},
		{name: "uppercase", eventType: "Compute.Instance.Create.End", wantErr: true},
		{name: "trailing dot", eventType: "compute.instance.", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			e.EventType = tc.eventType
			// Keep the payload complete so only the type rule can fail.
			state := "active"
			e.Payload.State = &state
			e.Payload.Size = map[string]any{"vcpus": 4}

			err := e.Validate()
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Validate() = nil, want an error for %q", tc.eventType)
			case tc.wantErr && !strings.Contains(err.Error(), "event_type"):
				t.Fatalf("Validate() error does not mention event_type: %v", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestValidatePayloadRulesByCategory(t *testing.T) {
	state := "active"
	size := map[string]any{"vcpus": 4}

	tests := []struct {
		name      string
		eventType string
		state     *string
		size      map[string]any
		wantErr   string
	}{
		{name: "create with state and size", eventType: "compute.instance.create.end", state: &state, size: size},
		{name: "create without size", eventType: "compute.instance.create.end", state: &state, wantErr: "payload.size"},
		{name: "create without state", eventType: "compute.instance.create.end", size: size, wantErr: "payload.state"},
		{name: "update with state and size", eventType: "compute.instance.resize.end", state: &state, size: size},
		{name: "update without size is fine", eventType: "compute.instance.power_off.end", state: &state},
		{name: "update without state", eventType: "compute.instance.power_off.end", wantErr: "payload.state"},
		{name: "delete without state", eventType: "compute.instance.delete.end"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			e.EventType = tc.eventType
			e.Payload.State = tc.state
			e.Payload.Size = tc.size

			err := e.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Validate() error does not mention %q: %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateSource(t *testing.T) {
	tests := []struct {
		name    string
		source  event.Source
		wantErr bool
	}{
		{name: "empty means collector", source: ""},
		{name: "collector", source: event.SourceCollector},
		{name: "reconciliation", source: event.SourceReconciliation},
		{name: "anything else", source: event.Source("operator"), wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			e.Source = tc.source

			err := e.Validate()
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Validate() = nil, want an error for source %q", tc.source)
			case tc.wantErr && !strings.Contains(err.Error(), "source"):
				t.Fatalf("Validate() error does not mention source: %v", err)
			case !tc.wantErr && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestValidateVirtualPlatform(t *testing.T) {
	// Both fields are named by every case, because the rule reads both and a
	// case that left one to the fixture would not say which one it exercises.
	tests := []struct {
		name            string
		platform, cloud string
		wantErr         string
		wantNotErr      string
	}{
		{
			name:     "meta project",
			platform: "meta", cloud: "os-prod-eu1",
			wantErr: `platform: "meta" is a virtual platform, which never carries resources`,
		},
		{
			name:     "partner",
			platform: "partner", cloud: "os-prod-eu1",
			wantErr: `platform: "partner" is a virtual platform, which never carries resources`,
		},
		{
			// The owner of a resource is resolved by (cloud, project_id), so a real
			// platform under the cloud "meta" would reach a meta-project.
			name:     "meta cloud under a real platform",
			platform: "openstack", cloud: "meta",
			wantErr: `cloud: "meta" is a virtual platform, which never carries resources`,
		},
		{
			name:     "partner cloud under a real platform",
			platform: "openstack", cloud: "partner",
			wantErr: `cloud: "partner" is a virtual platform, which never carries resources`,
		},
		{
			name:     "empty is empty, not virtual",
			platform: "", cloud: "",
			wantErr:    "platform: must not be empty",
			wantNotErr: "virtual platform",
		},
		{name: "real platform", platform: "openstack", cloud: "os-prod-eu1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			e.Platform, e.Cloud = tc.platform, tc.cloud

			err := e.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate() = nil, want an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Validate() error does not mention %q: %v", tc.wantErr, err)
			}
			if tc.wantNotErr != "" && err != nil && strings.Contains(err.Error(), tc.wantNotErr) {
				t.Fatalf("Validate() error mentions %q for (%q, %q): %v",
					tc.wantNotErr, tc.platform, tc.cloud, err)
			}
		})
	}
}

func TestPayloadEnvelopePreservesUnknownFields(t *testing.T) {
	const raw = `{"state":"active","note":"x","size":{"vcpus":4}}`

	var p event.PayloadEnvelope
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("Unmarshal() = %v, want nil", err)
	}
	if p.State == nil || *p.State != "active" {
		t.Fatalf("State = %v, want %q", p.State, "active")
	}

	encoded, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil", err)
	}

	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatalf("decoding round-tripped payload: %v", err)
	}
	if out["note"] != "x" {
		t.Errorf("round-tripped payload lost the unknown field: %s", encoded)
	}
	if out["state"] != "active" {
		t.Errorf("round-tripped payload lost state: %s", encoded)
	}
}

func TestPayloadEnvelopeEmptyObject(t *testing.T) {
	var p event.PayloadEnvelope
	if err := json.Unmarshal([]byte(`{}`), &p); err != nil {
		t.Fatalf("Unmarshal() = %v, want nil", err)
	}

	if p.State != nil {
		t.Errorf("State = %v, want nil", *p.State)
	}
	if p.Size != nil {
		t.Errorf("Size = %v, want nil", p.Size)
	}
	if p.Provider != nil {
		t.Errorf("Provider = %v, want nil", p.Provider)
	}
}

func TestPayloadEnvelopeMalformedJSON(t *testing.T) {
	// The decoder's own syntax error travels to the caller unchanged, so a
	// malformed payload is reported as malformed rather than as a field problem.
	var p event.PayloadEnvelope
	err := p.UnmarshalJSON([]byte(`{`))

	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) {
		t.Fatalf("UnmarshalJSON() = %v, want a *json.SyntaxError", err)
	}
}

func TestCategorize(t *testing.T) {
	tests := []struct {
		eventType string
		want      event.Category
	}{
		{eventType: "compute.instance.create.end", want: event.CategoryCreate},
		{eventType: "sync.create", want: event.CategoryCreate},
		{eventType: "compute.instance.delete.end", want: event.CategoryDelete},
		{eventType: "sync.delete", want: event.CategoryDelete},
		{eventType: "volume.transfer.accept.end", want: event.CategoryUpdate},
		{eventType: "sync.update", want: event.CategoryUpdate},
		{eventType: "compute.instance.power_off.end", want: event.CategoryUpdate},
		{eventType: "", want: event.CategoryUpdate},
	}

	for _, tc := range tests {
		t.Run(tc.eventType, func(t *testing.T) {
			if got := event.Categorize(tc.eventType); got != tc.want {
				t.Errorf("Categorize(%q) = %q, want %q", tc.eventType, got, tc.want)
			}
		})
	}
}

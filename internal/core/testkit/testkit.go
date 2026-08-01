// Package testkit holds the assertions every provider collector is tested
// against, so that a new provider proves it produces events the core accepts
// without reimplementing the rules.
//
// The normative specification is roadmap/00-conventions.md section 9.
package testkit

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/event"
)

// reporter is the part of *testing.T the assertions use. It exists so the
// assertions can themselves be tested for the failures they are meant to report,
// which a *testing.T cannot be made to do.
type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// AssertValidEvent fails the test unless raw decodes into an Event that passes
// every validation rule.
func AssertValidEvent(t *testing.T, raw []byte) {
	t.Helper()
	assertValidEvent(t, raw)
}

func assertValidEvent(t reporter, raw []byte) {
	t.Helper()

	var e event.Event
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Errorf("decoding event: %v", err)
		return
	}
	if err := e.Validate(); err != nil {
		t.Errorf("validating event: %v", err)
	}
}

// AssertDeterministicIDs fails the test unless fn returns the same id twice.
func AssertDeterministicIDs(t *testing.T, fn func() string) {
	t.Helper()
	assertDeterministicIDs(t, fn)
}

func assertDeterministicIDs(t reporter, fn func() string) {
	t.Helper()

	first, second := fn(), fn()
	if first != second {
		t.Errorf("id function is not deterministic: got %q then %q", first, second)
	}
}

// EventBuilder produces valid events for tests. Every method returns a copy, so
// a builder can be shared as a base and specialized per case.
type EventBuilder struct {
	e event.Event
}

// NewEventBuilder returns a builder for a valid OpenStack instance create event.
func NewEventBuilder() EventBuilder {
	state := "active"
	return EventBuilder{e: event.Event{
		EventID:      "test-event-1",
		Timestamp:    time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC),
		EventType:    "compute.instance.create.end",
		Platform:     "openstack",
		Cloud:        "os-dev",
		ResourceType: "instance",
		ResourceID:   "resource-1",
		ProjectID:    "project-1",
		Source:       event.SourceCollector,
		Payload: event.PayloadEnvelope{
			State: &state,
			Size:  map[string]any{"vcpus": 2, "ram_mb": 4096},
		},
	}}
}

// WithEventID sets the event id.
func (b EventBuilder) WithEventID(id string) EventBuilder {
	b.e.EventID = id
	return b
}

// WithTimestamp sets the event timestamp.
func (b EventBuilder) WithTimestamp(ts time.Time) EventBuilder {
	b.e.Timestamp = ts
	return b
}

// WithEventType sets the event type, which also decides the event's category.
func (b EventBuilder) WithEventType(eventType string) EventBuilder {
	b.e.EventType = eventType
	return b
}

// WithResourceID sets the resource id.
func (b EventBuilder) WithResourceID(id string) EventBuilder {
	b.e.ResourceID = id
	return b
}

// WithProjectID sets the owning project at or after this event.
func (b EventBuilder) WithProjectID(id string) EventBuilder {
	b.e.ProjectID = id
	return b
}

// WithState sets the payload state.
func (b EventBuilder) WithState(state string) EventBuilder {
	b.e.Payload.State = &state
	return b
}

// WithoutState clears the payload state, which is how an event says the state
// did not change.
func (b EventBuilder) WithoutState() EventBuilder {
	b.e.Payload.State = nil
	return b
}

// WithSize sets the payload size object.
func (b EventBuilder) WithSize(size map[string]any) EventBuilder {
	b.e.Payload.Size = size
	return b
}

// WithoutSize clears the payload size, which is how an event says the size did
// not change.
func (b EventBuilder) WithoutSize() EventBuilder {
	b.e.Payload.Size = nil
	return b
}

// Build returns the event.
func (b EventBuilder) Build() event.Event {
	return b.e
}

// BuildStored returns the event paired with a server-side receipt timestamp.
func (b EventBuilder) BuildStored(receivedAt time.Time) event.Stored {
	return event.Stored{Event: b.e, ReceivedAt: receivedAt}
}

// BuildJSON returns the event encoded as it travels the wire.
func (b EventBuilder) BuildJSON(t *testing.T) []byte {
	t.Helper()

	raw, err := json.Marshal(b.e)
	if err != nil {
		t.Fatalf("encoding fixture event: %v", err)
	}
	return raw
}

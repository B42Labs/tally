// Package event carries the canonical event schema every platform is normalized
// into: the wire types, their validation rules, and the categorization that
// derives an event's effect from its type. It is the contract collectors write
// against and the ingestion pipeline reads, so it holds no provider-specific
// knowledge and does no I/O.
//
// The normative specification is roadmap/00-conventions.md section 4.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Source names the pipeline an event came from.
type Source string

const (
	// SourceCollector marks an event pushed by a provider-side collector. It is
	// the default: an event that names no source is treated as a collector event.
	SourceCollector Source = "collector"
	// SourceReconciliation marks a synthetic event emitted by a server-side sync
	// run to correct drift between a platform and the projection.
	SourceReconciliation Source = "reconciliation"
)

// Category is the effect an event has on a resource.
type Category string

const (
	// CategoryCreate starts a resource's life: it sets created_at and requires a
	// full payload (state and size).
	CategoryCreate Category = "create"
	// CategoryUpdate changes a resource's state, size, or owner.
	CategoryUpdate Category = "update"
	// CategoryDelete ends a resource's life: it sets deleted_at and forces the
	// state to "deleted".
	CategoryDelete Category = "delete"
)

// eventIDMaxLen bounds event_id so it fits the database column and stays a
// usable idempotency key.
const eventIDMaxLen = 256

// eventTypeMaxLen bounds event_type for the same reason as identifierMaxLen
// below: idx_events_type indexes it next to timestamp. The pattern alone lets a
// value of any length through, and one past the btree limit fails the insert
// rather than the event, which would take down whatever batch it travelled in.
const eventTypeMaxLen = 512

// identifierMaxLen bounds the fields that identify a resource. They are indexed
// columns, and a value past the btree limit fails the insert rather than the
// event, which would take down whatever batch the event travelled in.
const identifierMaxLen = 512

// stateMaxLen bounds payload.state for the same reason. The projection writes it
// to current_resources.state, which idx_current_resources_type indexes next to
// resource_type and idx_current_resources_stats next to platform, cloud,
// resource_type and project_id. Those four are bounded above, so a state within
// this bound keeps the widest of the tuples under the btree limit.
const stateMaxLen = 512

var eventTypePattern = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)+$`)

// PayloadEnvelope is the normalized payload every event carries. Collectors map
// provider data into it so that projection, timeline, and metering never need
// provider-specific code.
//
// Unknown fields survive a decode/encode round-trip untouched, which keeps an
// event byte-faithful to what the provider sent even as this struct grows.
type PayloadEnvelope struct {
	// State is the resource state at or after the event. It is required on every
	// event except a delete, where the core sets "deleted" itself.
	State *string `json:"state,omitempty"`
	// Size is the full replacement size object, required on create and on any
	// size-changing event. Absent means the size did not change.
	Size map[string]any `json:"size,omitempty"`
	// Provider is free-form raw provider data, kept for debugging and audit. Core
	// logic never reads it.
	Provider map[string]any `json:"provider,omitempty"`

	extra map[string]json.RawMessage
}

// UnmarshalJSON decodes the envelope, routing every field this struct does not
// name into an unexported map so MarshalJSON can put it back.
func (p *PayloadEnvelope) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	*p = PayloadEnvelope{}
	for key, value := range raw {
		var err error
		switch key {
		case "state":
			err = json.Unmarshal(value, &p.State)
		case "size":
			err = json.Unmarshal(value, &p.Size)
		case "provider":
			err = json.Unmarshal(value, &p.Provider)
		default:
			if p.extra == nil {
				p.extra = make(map[string]json.RawMessage, len(raw))
			}
			p.extra[key] = value
			continue
		}
		if err != nil {
			return fmt.Errorf("payload field %q: %w", key, err)
		}
	}
	return nil
}

// MarshalJSON encodes the named fields alongside every unknown field a previous
// decode preserved.
func (p PayloadEnvelope) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(p.extra)+3)
	for key, value := range p.extra {
		out[key] = value
	}

	set := func(key string, value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("payload field %q: %w", key, err)
		}
		out[key] = encoded
		return nil
	}
	if p.State != nil {
		if err := set("state", p.State); err != nil {
			return nil, err
		}
	}
	if p.Size != nil {
		if err := set("size", p.Size); err != nil {
			return nil, err
		}
	}
	if p.Provider != nil {
		if err := set("provider", p.Provider); err != nil {
			return nil, err
		}
	}
	return json.Marshal(out)
}

// Event is an immutable lifecycle fact. The events table is the system's single
// source of truth; everything else is derived from it and rebuildable.
type Event struct {
	EventID      string          `json:"event_id"`
	Timestamp    time.Time       `json:"timestamp"`
	EventType    string          `json:"event_type"`
	Platform     string          `json:"platform"`
	Cloud        string          `json:"cloud"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	ProjectID    string          `json:"project_id"`
	Source       Source          `json:"source"`
	Payload      PayloadEnvelope `json:"payload"`
}

// Stored pairs an Event with the server-side timestamp that breaks ordering ties
// between events sharing an instant.
type Stored struct {
	Event
	ReceivedAt time.Time `json:"received_at"`
}

// Validate reports every rule the event breaks, joined into one error, so a
// caller sees the whole picture rather than the first problem.
func (e *Event) Validate() error {
	var errs []error

	if n := len(e.EventID); n < 1 || n > eventIDMaxLen {
		errs = append(errs, fmt.Errorf("event_id: must be 1..%d characters, got %d", eventIDMaxLen, n))
	}
	if e.Timestamp.IsZero() {
		errs = append(errs, errors.New("timestamp: must be set"))
	}
	// The length is reported before the pattern rather than alongside it, so that
	// the quoted value below stays as bounded as the field itself: the reason a
	// refusal is dead-lettered under is this error's text, and that column has no
	// cap of its own.
	switch n := len(e.EventType); {
	case n > eventTypeMaxLen:
		errs = append(errs, fmt.Errorf("event_type: must be at most %d characters, got %d",
			eventTypeMaxLen, n))
	case !eventTypePattern.MatchString(e.EventType):
		errs = append(errs, fmt.Errorf("event_type: %q must match %s", e.EventType, eventTypePattern))
	}

	for _, field := range []struct{ name, value string }{
		{"platform", e.Platform},
		{"cloud", e.Cloud},
		{"resource_type", e.ResourceType},
		{"resource_id", e.ResourceID},
		{"project_id", e.ProjectID},
	} {
		switch n := len(field.value); {
		case n == 0:
			errs = append(errs, fmt.Errorf("%s: must not be empty", field.name))
		case n > identifierMaxLen:
			errs = append(errs, fmt.Errorf("%s: must be at most %d characters, got %d",
				field.name, identifierMaxLen, n))
		}
	}

	if e.Payload.State != nil {
		if n := len(*e.Payload.State); n > stateMaxLen {
			errs = append(errs, fmt.Errorf("payload.state: must be at most %d characters, got %d",
				stateMaxLen, n))
		}
	}

	switch e.Source {
	case "", SourceCollector, SourceReconciliation:
	default:
		errs = append(errs, fmt.Errorf("source: %q must be %q or %q", e.Source, SourceCollector, SourceReconciliation))
	}

	switch Categorize(e.EventType) {
	case CategoryCreate:
		if e.Payload.State == nil {
			errs = append(errs, errors.New("payload.state: required on create events"))
		}
		if e.Payload.Size == nil {
			errs = append(errs, errors.New("payload.size: required on create events"))
		}
	case CategoryUpdate:
		if e.Payload.State == nil {
			errs = append(errs, errors.New("payload.state: required on every event except delete"))
		}
	case CategoryDelete:
	}

	return errors.Join(errs...)
}

// Categorize derives an event's effect from its type alone, which is what keeps
// the core free of per-platform code. See roadmap/00-conventions.md section 4.2.
func Categorize(eventType string) Category {
	parts := strings.Split(eventType, ".")
	if slices.Contains(parts, "create") {
		return CategoryCreate
	}
	if slices.Contains(parts, "delete") {
		return CategoryDelete
	}
	return CategoryUpdate
}

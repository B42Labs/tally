package openstack

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// osloMessageKey names the envelope member the notification travels in. Its
// value is a string of JSON rather than a nested object, which is why a body
// has to be decoded twice.
const osloMessageKey = "oslo.message"

// timestampLayouts are the layouts a notification timestamp is tried against,
// in order. The services write the first two, with and without the microsecond
// fraction and neither carrying a zone; the third covers a deployment that
// emits RFC 3339 instead.
var timestampLayouts = []string{
	"2006-01-02 15:04:05.000000",
	"2006-01-02 15:04:05",
	time.RFC3339,
}

// Notification is one oslo.messaging notification as an OpenStack service
// emitted it: what happened, when, in which project, and the service's own
// description of the resource. It is the raw provider fact, before any mapping
// into the canonical event schema.
type Notification struct {
	// MessageID is the id the emitting service assigned the notification. It is
	// unique per notification, which makes it the key a redelivery is recognized
	// by.
	MessageID string
	// EventType names what happened, such as "compute.instance.create.end". The
	// namespace is the emitting service's own, not Tally's.
	EventType string
	// Timestamp is when the service recorded the event, always in UTC.
	Timestamp time.Time
	// Payload is the service's description of the resource, decoded as it
	// arrived. Its numbers are json.Number and not float64, so a quantity keeps
	// every digit it was sent with and can still be turned into an exact decimal.
	Payload map[string]any
	// ContextProjectID is the project the request ran in, taken from the request
	// context the service attaches to its notifications.
	ContextProjectID string
	// ContextTenantID is the same project under its older name. Services differ
	// in which of the two they set, so both are read and the caller takes
	// whichever is populated.
	ContextTenantID string
}

// notification is the inner document's shape: the members this package reads,
// with everything else ignored. The timestamp stays a string because the layout
// it arrives in is decided by parseTimestamp, not by encoding/json.
type notification struct {
	MessageID        string         `json:"message_id"`
	EventType        string         `json:"event_type"`
	Timestamp        string         `json:"timestamp"`
	Payload          map[string]any `json:"payload"`
	ContextProjectID string         `json:"_context_project_id"`
	ContextTenantID  string         `json:"_context_tenant_id"`
}

// ParseEnvelope decodes a message body into the notification it carries.
//
// The body has two layers. The outer object is the oslo envelope, which holds
// the notification under "oslo.message" as a string, and that string is the
// JSON document the emitting service wrote. Both layers are decoded here.
//
// Only the timestamp is required. A notification without a message id, event
// type, payload, or project comes back as it arrived, because deciding what to
// do about a gap belongs to the mapping rather than to the decoder. An error
// means the body is unusable: the consumer acknowledges such a delivery instead
// of requeueing it, since a body that fails to parse fails again on every
// redelivery, so this text is all the operator gets to see.
func ParseEnvelope(body []byte) (Notification, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Notification{}, fmt.Errorf("decoding the oslo envelope: %w", err)
	}

	// Presence is checked before the type so that an envelope from some other
	// producer is reported as such, rather than as a message of the wrong shape.
	raw, ok := envelope[osloMessageKey]
	if !ok {
		return Notification{}, fmt.Errorf("the envelope has no %s member", osloMessageKey)
	}
	var message string
	if err := json.Unmarshal(raw, &message); err != nil {
		return Notification{}, fmt.Errorf("%s is not a string: %w", osloMessageKey, err)
	}

	decoder := json.NewDecoder(strings.NewReader(message))
	// The payload's numbers stay json.Number: a later stage divides them with
	// exact decimal arithmetic, and a detour through float64 would have rounded
	// them before that stage ever saw them.
	decoder.UseNumber()
	var inner notification
	if err := decoder.Decode(&inner); err != nil {
		return Notification{}, fmt.Errorf("decoding the oslo notification: %w", err)
	}

	if inner.Timestamp == "" {
		return Notification{}, errors.New("the oslo notification has no timestamp")
	}
	timestamp, err := parseTimestamp(inner.Timestamp)
	if err != nil {
		return Notification{}, err
	}

	return Notification{
		MessageID:        inner.MessageID,
		EventType:        inner.EventType,
		Timestamp:        timestamp,
		Payload:          inner.Payload,
		ContextProjectID: inner.ContextProjectID,
		ContextTenantID:  inner.ContextTenantID,
	}, nil
}

// parseTimestamp reads a notification timestamp as UTC. The zoneless layouts are
// parsed in UTC and not with time.Parse's local fallback, because the collector
// runs wherever it is deployed and a local zone would shift every event by that
// offset. A value that carries its own offset keeps it through the parse and is
// converted afterwards, so the instant survives either way.
func parseTimestamp(value string) (time.Time, error) {
	for _, layout := range timestampLayouts {
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("timestamp %q matches none of the known layouts", value)
}

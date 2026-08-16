package openstack

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// wrap builds a message body the way oslo.messaging does: the notification
// document travels as a JSON string inside the outer envelope. It takes a
// testing.TB because the fuzz target seeds itself through it.
func wrap(t testing.TB, inner string) []byte {
	t.Helper()

	body, err := json.Marshal(map[string]string{
		"oslo.version": "2.0",
		osloMessageKey: inner,
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return body
}

// innerDocument is a complete notification, as a nova instance creation writes
// it, so a test that varies one member states only that member.
const innerDocument = `{
	"message_id": "0a3b5f1e-6d2c-4f8a-9b1d-2c4e6a8b0d2f",
	"event_type": "compute.instance.create.end",
	"timestamp": "2026-03-01 12:34:56.789012",
	"_context_project_id": "5f0c1d2e3a4b5c6d7e8f9a0b1c2d3e4f",
	"_context_tenant_id": "5f0c1d2e3a4b5c6d7e8f9a0b1c2d3e4f",
	"payload": {
		"instance_id": "7c1e9a4b-3d5f-4a2c-8e6b-1f3a5c7e9b0d",
		"memory_mb": 2048,
		"vcpus": 2
	}
}`

func TestParseEnvelopeReadsEveryField(t *testing.T) {
	got, err := ParseEnvelope(wrap(t, innerDocument))
	if err != nil {
		t.Fatalf("ParseEnvelope() error = %v, want nil", err)
	}

	if want := "0a3b5f1e-6d2c-4f8a-9b1d-2c4e6a8b0d2f"; got.MessageID != want {
		t.Errorf("MessageID = %q, want %q", got.MessageID, want)
	}
	if want := "compute.instance.create.end"; got.EventType != want {
		t.Errorf("EventType = %q, want %q", got.EventType, want)
	}
	if want := time.Date(2026, 3, 1, 12, 34, 56, 789012000, time.UTC); !got.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, want)
	}
	if want := "5f0c1d2e3a4b5c6d7e8f9a0b1c2d3e4f"; got.ContextProjectID != want {
		t.Errorf("ContextProjectID = %q, want %q", got.ContextProjectID, want)
	}
	if want := "5f0c1d2e3a4b5c6d7e8f9a0b1c2d3e4f"; got.ContextTenantID != want {
		t.Errorf("ContextTenantID = %q, want %q", got.ContextTenantID, want)
	}
	if want := "7c1e9a4b-3d5f-4a2c-8e6b-1f3a5c7e9b0d"; got.Payload["instance_id"] != want {
		t.Errorf("Payload[instance_id] = %v, want %q", got.Payload["instance_id"], want)
	}
}

// TestParseEnvelopeKeepsPayloadNumbersExact guards the decoder setting the
// mapping depends on: a payload number reaching it as a float64 would already
// have been rounded, and no later stage can undo that.
func TestParseEnvelopeKeepsPayloadNumbersExact(t *testing.T) {
	got, err := ParseEnvelope(wrap(t, innerDocument))
	if err != nil {
		t.Fatalf("ParseEnvelope() error = %v, want nil", err)
	}

	for _, member := range []struct {
		name string
		want string
	}{
		{name: "memory_mb", want: "2048"},
		{name: "vcpus", want: "2"},
	} {
		t.Run(member.name, func(t *testing.T) {
			number, ok := got.Payload[member.name].(json.Number)
			if !ok {
				t.Fatalf("Payload[%s] is %T, want json.Number", member.name, got.Payload[member.name])
			}
			if number.String() != member.want {
				t.Errorf("Payload[%s] = %q, want %q", member.name, number, member.want)
			}
		})
	}
}

func TestParseEnvelopeReadsEveryTimestampLayout(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  time.Time
	}{
		{
			name:  "a microsecond timestamp carries no zone and is read as UTC",
			value: "2026-03-01 12:00:00.000123",
			want:  time.Date(2026, 3, 1, 12, 0, 0, 123000, time.UTC),
		},
		{
			name:  "a whole-second timestamp is read as UTC too",
			value: "2026-03-01 12:00:00",
			want:  time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		},
		{
			name:  "an RFC 3339 offset is normalized to the same instant in UTC",
			value: "2026-03-01T12:00:00+02:00",
			want:  time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		},
		{
			name:  "an RFC 3339 value already in UTC keeps its instant",
			value: "2026-03-01T12:00:00Z",
			want:  time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEnvelope(wrap(t, `{"timestamp": "`+tc.value+`"}`))
			if err != nil {
				t.Fatalf("ParseEnvelope() error = %v, want nil", err)
			}
			if !got.Timestamp.Equal(tc.want) {
				t.Errorf("Timestamp = %v, want %v", got.Timestamp, tc.want)
			}
			if got.Timestamp.Location() != time.UTC {
				t.Errorf("Timestamp location = %v, want UTC", got.Timestamp.Location())
			}
		})
	}
}

// TestParseEnvelopeRejectsUnusableBodies covers every way a delivery can fail to
// yield a notification. Each of them is acknowledged rather than requeued, so
// the error names what was wrong with the body.
func TestParseEnvelopeRejectsUnusableBodies(t *testing.T) {
	tests := []struct {
		name  string
		body  []byte
		wants string
	}{
		{
			name:  "the body is not JSON at all",
			body:  []byte("not json"),
			wants: "envelope",
		},
		{
			name:  "the envelope carries no oslo.message member",
			body:  []byte(`{"oslo.version": "2.0"}`),
			wants: osloMessageKey,
		},
		{
			name:  "oslo.message is a nested object instead of a string",
			body:  []byte(`{"oslo.version": "2.0", "oslo.message": {"event_type": "compute.instance.create.end"}}`),
			wants: osloMessageKey,
		},
		{
			name:  "the string in oslo.message is not JSON",
			body:  wrap(t, "not json either"),
			wants: "notification",
		},
		{
			name:  "the notification carries no timestamp",
			body:  wrap(t, `{"event_type": "compute.instance.create.end"}`),
			wants: "timestamp",
		},
		{
			name:  "the timestamp is empty",
			body:  wrap(t, `{"timestamp": ""}`),
			wants: "timestamp",
		},
		{
			name:  "the timestamp matches none of the known layouts",
			body:  wrap(t, `{"timestamp": "01.03.2026 12:00"}`),
			wants: "timestamp",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEnvelope(tc.body)
			if err == nil {
				t.Fatal("ParseEnvelope() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("ParseEnvelope() error = %q, want it to mention %q", err, tc.wants)
			}
			if !reflect.DeepEqual(got, Notification{}) {
				t.Errorf("ParseEnvelope() = %+v, want the zero Notification", got)
			}
		})
	}
}

// FuzzParseEnvelope drives the collector's trust boundary. The bytes reaching
// it are whatever an external broker delivered, they are decoded through two
// nested JSON layers, and the consumer's whole answer to a bad body rests on
// this call returning an error rather than taking the process down: a delivery
// the parser panicked on is never acknowledged, so the broker hands it back on
// every reconnect.
//
// The invariant is that shape. A body either parses or is refused, and a parsed
// notification carries the one member the consumer relies on afterwards: a
// timestamp that is set and in UTC, because the event's instant is derived from
// it and a zoneless layout read as local time would shift every event by the
// collector's own offset.
func FuzzParseEnvelope(f *testing.F) {
	f.Add(wrap(f, innerDocument))
	f.Add(wrap(f, `{"timestamp": "2026-03-01T12:00:00+02:00"}`))
	f.Add(wrap(f, `{"timestamp": ""}`))
	f.Add(wrap(f, "not json either"))
	f.Add([]byte("not json"))
	f.Add([]byte(`{"oslo.version": "2.0"}`))
	f.Add([]byte(`{"oslo.version": "2.0", "oslo.message": {"event_type": "x"}}`))

	f.Fuzz(func(t *testing.T, body []byte) {
		got, err := ParseEnvelope(body)
		if err != nil {
			return
		}
		if got.Timestamp.IsZero() {
			t.Errorf("ParseEnvelope(%q) returned a zero timestamp without an error", body)
		}
		if got.Timestamp.Location() != time.UTC {
			t.Errorf("ParseEnvelope(%q) timestamp location = %v, want UTC", body, got.Timestamp.Location())
		}
	})
}

// TestParseEnvelopeAcceptsAbsentMembers pins what the decoder tolerates: only
// the timestamp is required, and every gap besides it is left for the mapping
// to decide about.
func TestParseEnvelopeAcceptsAbsentMembers(t *testing.T) {
	t.Run("a notification without a payload parses with a nil payload", func(t *testing.T) {
		got, err := ParseEnvelope(wrap(t, `{
			"message_id": "0a3b5f1e-6d2c-4f8a-9b1d-2c4e6a8b0d2f",
			"event_type": "compute.instance.create.end",
			"timestamp": "2026-03-01 12:00:00"
		}`))
		if err != nil {
			t.Fatalf("ParseEnvelope() error = %v, want nil", err)
		}
		if got.Payload != nil {
			t.Errorf("Payload = %v, want nil", got.Payload)
		}
	})

	t.Run("a notification without a message id parses with an empty one", func(t *testing.T) {
		got, err := ParseEnvelope(wrap(t, `{
			"event_type": "compute.instance.create.end",
			"timestamp": "2026-03-01 12:00:00",
			"payload": {"instance_id": "7c1e9a4b-3d5f-4a2c-8e6b-1f3a5c7e9b0d"}
		}`))
		if err != nil {
			t.Fatalf("ParseEnvelope() error = %v, want nil", err)
		}
		if got.MessageID != "" {
			t.Errorf("MessageID = %q, want the empty string", got.MessageID)
		}
		if got.ContextProjectID != "" {
			t.Errorf("ContextProjectID = %q, want the empty string", got.ContextProjectID)
		}
		if got.EventType != "compute.instance.create.end" {
			t.Errorf("EventType = %q, want it kept", got.EventType)
		}
	})
}

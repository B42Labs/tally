package openstack

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
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

// keystoneToken is the credential the fixtures carry and no dump line may print.
const keystoneToken = "gAAAAABlive-keystone-token"

// schedulerMessageID is the message id of schedulerDocument.
const schedulerMessageID = "5b1c7d2e-8f3a-4c6b-9d0e-1a2b3c4d5e6f"

// schedulerDocument is a nova scheduler notification as a deployment publishes
// it: the request context sits inside the payload, Keystone token included. No
// fixture and no simulator payload has that shape.
const schedulerDocument = `{"message_id": "` + schedulerMessageID + `", "event_type": "scheduler.select_destinations.start", "timestamp": "2026-03-01 12:34:56.789012", "payload": {"request_spec": {"instance_properties": {"pci_requests": {"_context": {"auth_token": "` + keystoneToken + `", "user": "u1"}}}}}}`

// schedulerRequestContext is the path to the request context inside a dumped
// schedulerDocument line.
var schedulerRequestContext = []any{"payload", "request_spec", "instance_properties", "pci_requests", "_context"}

// volumeAttachDocument is a cinder attachment notification with the
// connection_info of a Ceph backend: the monitors, the user and the secret UUID.
const volumeAttachDocument = `{"message_id": "6c2d8e3f-9a4b-4d7c-8e1f-2b3c4d5e6f7a", "event_type": "volume.attach.end", "timestamp": "2026-03-01 12:34:56.789012", "payload": {"volume_id": "v1", "volume_attachment": [{"connection_info": {"hosts": ["10.0.0.1"], "auth_username": "cinder", "secret_uuid": "457eb676-33da-42ec-9a8c-9293d545c337"}, "instance_uuid": "i1"}]}}`

// closedWriter refuses every write, the way the dump's standard output does
// once the reader of the pipe is gone.
type closedWriter struct{}

func (closedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// memberAt follows path into a decoded JSON tree: a string step names a member
// of an object and an int step an element of an array. It returns nil where
// the path leaves the tree.
func memberAt(value any, path ...any) any {
	for _, step := range path {
		switch step := step.(type) {
		case string:
			object, _ := value.(map[string]any)
			value = object[step]
		case int:
			array, _ := value.([]any)
			if step < 0 || step >= len(array) {
				return nil
			}
			value = array[step]
		}
	}
	return value
}

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
// TestPreviewRedactsTheRequestContextCredentials covers the encoding the
// redaction has to match. oslo carries the notification as a JSON string and
// not as a nested object, so in the raw bytes the inner document's quotes are
// backslash-escaped and the token reaches the dump as
// \"_context_auth_token\": \"gAAAAAB…\". A pattern written against bare quotes
// matches none of that and the body is printed through unchanged.
//
// Redaction runs whenever ParseEnvelope refuses a body, and the most common
// refusal of a well-formed envelope is a timestamp layout the collector does not
// know — which is exactly when an operator reaches for the dump and attaches its
// output to a ticket. A Keystone token is valid for hours.
func TestPreviewRedactsTheRequestContextCredentials(t *testing.T) {
	const token = "gAAAAABlive-keystone-token"

	tests := []struct {
		name string
		body []byte
	}{
		{
			// The envelope decodes, only the timestamp does not, so the credentials
			// travel escaped inside the oslo.message string.
			name: "an envelope the parser refused for its timestamp",
			body: wrap(t, `{
				"event_type": "identity.authenticate",
				"timestamp": "01.03.2026 12:00",
				"_context_auth_token": "`+token+`",
				"_context_password": "hunter2"
			}`),
		},
		{
			// A body that is no envelope at all is previewed as it arrived, which is
			// the case the pattern already covered.
			name: "a body that is no envelope at all",
			body: []byte(`{"_context_auth_token": "` + token + `", "_context_password": "hunter2"}`),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseEnvelope(tc.body); err == nil {
				t.Fatal("ParseEnvelope() error = nil, want the body to reach the preview")
			}

			got := preview(tc.body)

			if strings.Contains(got, token) {
				t.Errorf("preview() printed the Keystone token: %s", got)
			}
			if strings.Contains(got, "hunter2") {
				t.Errorf("preview() printed the request context password: %s", got)
			}
			if want := strings.Count(got, `"[redacted]"`); want != 2 {
				t.Errorf("preview() replaced %d credentials, want the token and the password: %s", want, got)
			}
		})
	}
}

// TestPreviewDescribesABodyItCannotRedact covers the bodies the redaction
// cannot reach. The pattern is written against JSON quoting, so a
// msgpack-serialized notification — oslo.messaging offers that serializer — and
// anything else a publisher on a bound topic sends carry their
// _context_auth_token past it untouched. Printing those bytes raw would put a
// live Keystone token in the file an operator attaches to a ticket, which is
// what the redaction exists to prevent.
func TestPreviewDescribesABodyItCannotRedact(t *testing.T) {
	const token = "gAAAAABlive-keystone-token"
	// A msgpack map: the member names arrive length-prefixed rather than quoted,
	// so the pattern matches nothing at all.
	body := []byte("\x82\xb3_context_auth_token\xd9\x1a" + token + "\xaaevent_type")

	if _, err := ParseEnvelope(body); err == nil {
		t.Fatal("ParseEnvelope() error = nil, want the body to reach the preview")
	}

	got := preview(body)

	if strings.Contains(got, token) {
		t.Errorf("preview() printed the Keystone token: %s", got)
	}
	// The delivery is still reported: that a body arrived which this collector can
	// neither read nor redact is what the operator running the dump is looking for.
	if !strings.Contains(got, "not JSON") {
		t.Errorf("preview() = %q, want it to report the body it did not print", got)
	}
}

// TestPreviewCutsTheBodyAtTheBound keeps the dump's line to the beginning of a
// message, which is where its shape shows.
func TestPreviewCutsTheBodyAtTheBound(t *testing.T) {
	body := wrap(t, `{"timestamp": "01.03.2026 12:00", "filler": "`+
		strings.Repeat("z", 2*previewMax)+`"}`)

	if got := preview(body); len(got) != previewMax {
		t.Errorf("preview() returned %d bytes, want it cut at %d", len(got), previewMax)
	}
}

// TestSecretMember pins the rule both dump lines go by. The names are the ones
// services use for a credential and the ones the mapping reads, which the rule
// has to leave alone.
func TestSecretMember(t *testing.T) {
	for _, name := range []string{
		"auth_token", "_context_auth_token", "password", "_context_password", "auth_password",
		"admin_password", "PASSWORD", "X-Auth-Token", "service_token", "secret_uuid",
		"connection_info", "old_connection_info",
	} {
		if !secretMember(name) {
			t.Errorf("secretMember(%q) = false, want the member withheld", name)
		}
	}

	for _, name := range []string{
		"", "instance_id", "tenant_id", "volume_id", "display_name", "key_name", "routing_key",
		"size", "connection",
	} {
		if secretMember(name) {
			t.Errorf("secretMember(%q) = true, want the member printed", name)
		}
	}
}

// TestRedactionLeavesWhatTheMappingReads holds the rule against the mapping
// table: a payload the dump has redacted maps to the event the delivered one
// maps to, for every golden notification.
func TestRedactionLeavesWhatTheMappingReads(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "golden", "notifications", "*.json"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("Glob() = %v, %v, want the notification fixtures", fixtures, err)
	}

	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			body, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatalf("reading the notification: %v", err)
			}
			parsed, err := ParseEnvelope(body)
			if err != nil {
				t.Fatalf("ParseEnvelope() error = %v, want nil", err)
			}
			want, wantOK := MapNotification(parsed, goldenCloud)

			redactValue(parsed.Payload)

			got, gotOK := MapNotification(parsed, goldenCloud)
			if gotOK != wantOK || !reflect.DeepEqual(got, want) {
				t.Errorf("MapNotification() of the redacted payload = %+v, %v, want %+v, %v", got, gotOK, want, wantOK)
			}
		})
	}
}

// TestRedactValue covers the walk over a decoded payload. The rule reads the
// name alone, so a matching member is replaced whatever it holds, and a tree
// without one comes back as it went in.
func TestRedactValue(t *testing.T) {
	tests := []struct {
		name string
		tree any
		want any
	}{
		{
			name: "a string under a matching name is replaced",
			tree: map[string]any{"auth_token": keystoneToken, "user": "u1"},
			want: map[string]any{"auth_token": "[redacted]", "user": "u1"},
		},
		{
			name: "a number under a matching name is replaced",
			tree: map[string]any{"token": json.Number("42")},
			want: map[string]any{"token": "[redacted]"},
		},
		{
			name: "a boolean under a matching name is replaced",
			tree: map[string]any{"has_secret": true},
			want: map[string]any{"has_secret": "[redacted]"},
		},
		{
			name: "null under a matching name is replaced",
			tree: map[string]any{"password": nil},
			want: map[string]any{"password": "[redacted]"},
		},
		{
			name: "an object under a matching name is replaced whole",
			tree: map[string]any{"connection_info": map[string]any{"hosts": []any{"10.0.0.1"}}},
			want: map[string]any{"connection_info": "[redacted]"},
		},
		{
			name: "an array under a matching name is replaced whole",
			tree: map[string]any{"tokens": []any{"a", "b"}},
			want: map[string]any{"tokens": "[redacted]"},
		},
		{
			name: "a matching member inside an array inside an array is replaced",
			tree: []any{[]any{map[string]any{"Admin_Password": nil}}},
			want: []any{[]any{map[string]any{"Admin_Password": "[redacted]"}}},
		},
		{
			name: "an empty map comes back empty",
			tree: map[string]any{},
			want: map[string]any{},
		},
		{
			name: "an empty slice comes back empty",
			tree: []any{},
			want: []any{},
		},
		{
			name: "a tree with no matching member comes back as it went in",
			tree: map[string]any{"instance_id": "i1", "vcpus": json.Number("2"), "nics": []any{map[string]any{"connection": "c1"}}},
			want: map[string]any{"instance_id": "i1", "vcpus": json.Number("2"), "nics": []any{map[string]any{"connection": "c1"}}},
		},
		{
			name: "nil is left alone",
			tree: nil,
			want: nil,
		},
		{
			name: "a nil map is left alone",
			tree: map[string]any(nil),
			want: map[string]any(nil),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			redactValue(tc.tree)

			if !reflect.DeepEqual(tc.tree, tc.want) {
				t.Errorf("redactValue() left %#v, want %#v", tc.tree, tc.want)
			}
		})
	}
}

// printedLine runs one notification through printNotification the way the dump
// delivers it, and returns what was written and that line decoded.
func printedLine(t *testing.T, inner string) (string, map[string]any) {
	t.Helper()

	var out bytes.Buffer
	delivery := amqp091.Delivery{Exchange: "nova", RoutingKey: "notifications.info", Body: wrap(t, inner)}
	if err := printNotification(json.NewEncoder(&out), delivery); err != nil {
		t.Fatalf("printNotification() error = %v, want nil", err)
	}
	if strings.Count(out.String(), "\n") != 1 || !strings.HasSuffix(out.String(), "\n") {
		t.Fatalf("printNotification() wrote %q, want one line", out.String())
	}
	var line map[string]any
	if err := json.Unmarshal(out.Bytes(), &line); err != nil {
		t.Fatalf("printNotification() wrote %q, want a JSON object: %v", out.String(), err)
	}
	return out.String(), line
}

// TestPrintNotificationRedactsThePayload covers the line the dump prints for
// nearly every delivery, the one whose body parses. Both shapes are ones a
// deployment publishes with a credential in the payload, and neither is among
// the fixtures.
func TestPrintNotificationRedactsThePayload(t *testing.T) {
	t.Run("the Keystone token of a scheduler notification", func(t *testing.T) {
		out, line := printedLine(t, schedulerDocument)

		if got := memberAt(line, append(schedulerRequestContext, "auth_token")...); got != "[redacted]" {
			t.Errorf("the printed auth_token = %v, want [redacted]", got)
		}
		if got := memberAt(line, append(schedulerRequestContext, "user")...); got != "u1" {
			t.Errorf("the printed user = %v, want it kept as u1", got)
		}
		if strings.Contains(out, keystoneToken) {
			t.Errorf("printNotification() printed the Keystone token: %s", out)
		}
		// What is not payload is printed as before.
		for _, member := range []struct{ name, want string }{
			{name: "exchange", want: "nova"},
			{name: "routing_key", want: "notifications.info"},
			{name: "message_id", want: schedulerMessageID},
			{name: "event_type", want: "scheduler.select_destinations.start"},
			{name: "timestamp", want: "2026-03-01T12:34:56.789012Z"},
		} {
			if got := line[member.name]; got != member.want {
				t.Errorf("the printed %s = %v, want %q", member.name, got, member.want)
			}
		}
	})

	t.Run("the connection_info of a volume attachment", func(t *testing.T) {
		out, line := printedLine(t, volumeAttachDocument)

		if got := memberAt(line, "payload", "volume_attachment", 0, "connection_info"); got != "[redacted]" {
			t.Errorf("the printed connection_info = %v, want the string [redacted]", got)
		}
		if got := memberAt(line, "payload", "volume_id"); got != "v1" {
			t.Errorf("the printed volume_id = %v, want it kept as v1", got)
		}
		if got := memberAt(line, "payload", "volume_attachment", 0, "instance_uuid"); got != "i1" {
			t.Errorf("the printed instance_uuid = %v, want it kept as i1", got)
		}
		for _, secret := range []string{"10.0.0.1", "457eb676-33da-42ec-9a8c-9293d545c337"} {
			if strings.Contains(out, secret) {
				t.Errorf("printNotification() printed %s from the connection_info: %s", secret, out)
			}
		}
	})

	t.Run("a notification without a payload prints none", func(t *testing.T) {
		_, line := printedLine(t, `{"message_id": "0a3b5f1e-6d2c-4f8a-9b1d-2c4e6a8b0d2f", `+
			`"event_type": "compute.instance.create.end", "timestamp": "2026-03-01 12:00:00"}`)

		if payload, ok := line["payload"]; ok {
			t.Errorf("the printed line carries the payload %v, want the member left out", payload)
		}
	})
}

// TestPrintNotificationReportsAWriteThatFailed keeps a dump whose output is
// gone from running on: the error ends the session.
func TestPrintNotificationReportsAWriteThatFailed(t *testing.T) {
	delivery := amqp091.Delivery{Exchange: "nova", RoutingKey: "notifications.info", Body: wrap(t, schedulerDocument)}

	err := printNotification(json.NewEncoder(closedWriter{}), delivery)

	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("printNotification() error = %v, want it to wrap io.ErrClosedPipe", err)
	}
	if !strings.HasPrefix(err.Error(), "printing a notification: ") {
		t.Errorf("printNotification() error = %q, want it to start with %q", err, "printing a notification: ")
	}
}

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

// TestQueueDeclareArgs pins the two declares the collector issues. Everything
// but quorum declares with no arguments at all, which is the declare of classic
// and of every collector up to v0.2.0, so the nil is asserted and an empty
// table does not pass for it.
func TestQueueDeclareArgs(t *testing.T) {
	tests := []struct {
		name      string
		queueType string
		want      amqp091.Table
	}{
		{
			name:      "a Config that never went through Load declares no arguments",
			queueType: "",
			want:      nil,
		},
		{
			name:      "classic declares no arguments",
			queueType: queueTypeClassic,
			want:      nil,
		},
		{
			name:      "a type the collector does not declare falls back to no arguments",
			queueType: "stream",
			want:      nil,
		},
		{
			// The int32 is part of what is pinned: reflect.DeepEqual tells it from
			// an int of the same value.
			name:      "quorum declares the type and disables the delivery limit",
			queueType: queueTypeQuorum,
			want:      amqp091.Table{"x-queue-type": "quorum", "x-delivery-limit": int32(-1)},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := queueDeclareArgs(tc.queueType); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("queueDeclareArgs(%q) = %#v, want %#v", tc.queueType, got, tc.want)
			}
		})
	}
}

// TestQueueDeclareError pins the two texts a failed declare is reported with.
// Only a 406 is a queue that exists with other arguments, so only a 406 names
// the variable: a 403 and a closed channel are not fixed by changing it.
func TestQueueDeclareError(t *testing.T) {
	const plain = "declaring the queue tally-notifications: "
	mismatch := func(queueType string) string {
		return plain + "the queue exists with other arguments than TALLY_OSC_QUEUE_TYPE=" + queueType +
			" declares, and a queue keeps the type it was declared with: "
	}
	inequivalent := &amqp091.Error{
		Code: amqp091.PreconditionFailed,
		Reason: "PRECONDITION_FAILED - inequivalent arg 'x-queue-type' for queue 'tally-notifications' " +
			"in vhost '/': received 'quorum' but current is 'classic'",
	}
	forbidden := &amqp091.Error{
		Code:   amqp091.AccessRefused,
		Reason: "ACCESS_REFUSED - configure access to queue 'tally-notifications' in vhost '/' refused for user 'tally'",
	}
	closed := errors.New("closed")

	tests := []struct {
		name      string
		cause     error
		queueType string
		want      string
	}{
		{
			name:      "a 406 under classic names classic",
			cause:     inequivalent,
			queueType: queueTypeClassic,
			want:      mismatch("classic") + inequivalent.Error(),
		},
		{
			name:      "a 406 under quorum names quorum",
			cause:     inequivalent,
			queueType: queueTypeQuorum,
			want:      mismatch("quorum") + inequivalent.Error(),
		},
		{
			name:      "a 403 keeps the plain text",
			cause:     forbidden,
			queueType: queueTypeQuorum,
			want:      plain + forbidden.Error(),
		},
		{
			name:      "an error that is no broker error keeps the plain text",
			cause:     closed,
			queueType: queueTypeQuorum,
			want:      plain + closed.Error(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := queueDeclareError(tc.cause, tc.queueType)

			if got.Error() != tc.want {
				t.Errorf("queueDeclareError() = %q, want %q", got, tc.want)
			}
			if !errors.Is(got, tc.cause) {
				t.Errorf("queueDeclareError() = %v, want it to wrap its cause", got)
			}
		})
	}

	// The broker's own error stays reachable behind the added text, code and all.
	var refused *amqp091.Error
	if got := queueDeclareError(inequivalent, queueTypeClassic); !errors.As(got, &refused) || refused != inequivalent {
		t.Errorf("errors.As(queueDeclareError()) found %v, want the broker's 406", refused)
	}
}

// TestRequireQuorumBroker covers the gate in front of the quorum declare. The
// version is a server property, which is whatever the broker chose to send, so
// the cases it cannot read are refused and not waved through.
func TestRequireQuorumBroker(t *testing.T) {
	const (
		remedy = "; set TALLY_OSC_QUEUE_TYPE=classic for this broker"
		older  = "TALLY_OSC_QUEUE_TYPE=quorum needs RabbitMQ 4.0 or newer and the broker reports 3.13.7: " +
			"an older broker reads the delivery limit of -1 as a limit and drops a notification on its first requeue" +
			remedy
		unusable = "TALLY_OSC_QUEUE_TYPE=quorum needs RabbitMQ 4.0 or newer and the broker reports no usable version: "
	)

	tests := []struct {
		name       string
		properties amqp091.Table
		// want is the error text, and empty for a broker the gate lets through.
		want string
	}{
		{name: "the oldest release the gate lets through", properties: amqp091.Table{"version": "4.0.0"}},
		{name: "a patch release of the 4.x line", properties: amqp091.Table{"version": "4.3.6"}},
		{name: "a later major release", properties: amqp091.Table{"version": "5.0.0"}},
		{name: "a version without a dot", properties: amqp091.Table{"version": "4"}},
		{
			name:       "a broker older than 4.0",
			properties: amqp091.Table{"version": "3.13.7"},
			want:       older,
		},
		{
			name:       "no server properties at all",
			properties: nil,
			want:       unusable + "<nil>" + remedy,
		},
		{
			name:       "server properties without a version",
			properties: amqp091.Table{"product": "RabbitMQ"},
			want:       unusable + "<nil>" + remedy,
		},
		{
			name:       "an empty version",
			properties: amqp091.Table{"version": ""},
			want:       unusable + remedy,
		},
		{
			name:       "a version that is not a string",
			properties: amqp091.Table{"version": int32(4)},
			want:       unusable + "4" + remedy,
		},
		{
			name:       "a version that starts with no number",
			properties: amqp091.Table{"version": "unknown"},
			want:       unusable + "unknown" + remedy,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := requireQuorumBroker(tc.properties)

			if tc.want == "" {
				if err != nil {
					t.Fatalf("requireQuorumBroker() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("requireQuorumBroker() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("requireQuorumBroker() error = %q, want %q", err, tc.want)
			}
		})
	}
}

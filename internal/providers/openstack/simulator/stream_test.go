package simulator

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/event"
)

// streamFileMode is what the tests write a stream file with by hand. It is the
// mode os.Create gives the files this package writes, minus the umask.
const streamFileMode = 0o600

// encodeLine marshals one stream line or fails the test.
func encodeLine(t *testing.T, line Line) []byte {
	t.Helper()

	encoded, err := json.Marshal(line)
	if err != nil {
		t.Fatalf("encoding the line: %v", err)
	}
	return encoded
}

// writeLines writes the lines to a stream file of their own and returns its
// path. It is how the tests build a file ReadStream has to refuse, which
// WriteStream cannot produce.
func writeLines(t *testing.T, lines ...[]byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "notifications.jsonl")
	var content bytes.Buffer
	for _, line := range lines {
		content.Write(line)
		content.WriteByte('\n')
	}
	if err := os.WriteFile(path, content.Bytes(), streamFileMode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// readLines returns the lines of a file. The event stream is read this way
// rather than through ReadStream, which decodes the notification stream.
func readLines(t *testing.T, path string) [][]byte {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	trimmed := bytes.TrimSuffix(content, []byte("\n"))
	if len(trimmed) == 0 {
		return nil
	}
	return bytes.Split(trimmed, []byte("\n"))
}

func TestStreamRoundTrips(t *testing.T) {
	schedule := generateMonth(t, 1, july2026, testCloud)
	path := filepath.Join(t.TempDir(), "notifications.jsonl")

	if err := WriteStream(path, schedule); err != nil {
		t.Fatalf("WriteStream() error = %v, want nil", err)
	}
	lines, err := ReadStream(path)
	if err != nil {
		t.Fatalf("ReadStream() error = %v, want nil", err)
	}

	if len(lines) != len(schedule) {
		t.Fatalf("ReadStream() returned %d lines, want the schedule's %d", len(lines), len(schedule))
	}
	for i, line := range lines {
		transition := schedule[i]
		if line.Exchange != transition.Exchange {
			t.Errorf("line %d exchange = %q, want %q", i+1, line.Exchange, transition.Exchange)
		}
		if line.RoutingKey != collectorTopic {
			t.Errorf("line %d routing key = %q, want %q", i+1, line.RoutingKey, collectorTopic)
		}
		if want := render(t, transition); !bytes.Equal(line.Body, want) {
			t.Errorf("line %d body = %s, want %s", i+1, line.Body, want)
		}
	}
}

func TestWriteEventsWritesOneLinePerBillableTransition(t *testing.T) {
	schedule := generateMonth(t, 1, july2026, testCloud)
	path := filepath.Join(t.TempDir(), "events.jsonl")

	if err := WriteEvents(path, testCloud, schedule); err != nil {
		t.Fatalf("WriteEvents() error = %v, want nil", err)
	}

	billable := schedule.Billable()
	documents := readLines(t, path)
	if len(documents) != len(billable) {
		t.Fatalf("events.jsonl holds %d lines, want one per billable transition, which is %d",
			len(documents), len(billable))
	}

	got := make(map[string]struct{}, len(documents))
	for i, document := range documents {
		var recorded event.Event
		if err := json.Unmarshal(document, &recorded); err != nil {
			t.Fatalf("decoding line %d: %v", i+1, err)
		}
		if err := recorded.Validate(); err != nil {
			t.Errorf("line %d: Validate() error = %v, want nil", i+1, err)
		}
		if recorded.Cloud != testCloud {
			t.Errorf("line %d cloud = %q, want %q", i+1, recorded.Cloud, testCloud)
		}
		if recorded.Source != event.SourceCollector {
			t.Errorf("line %d source = %q, want %q", i+1, recorded.Source, event.SourceCollector)
		}
		got[recorded.EventID] = struct{}{}
	}

	// The event id is the notification's message id, which is what a redelivery
	// is deduplicated by, so the two sets have to name the same notifications.
	want := make(map[string]struct{}, len(billable))
	for _, transition := range billable {
		want[transition.MessageID] = struct{}{}
	}
	if !maps.Equal(got, want) {
		t.Errorf("events.jsonl holds %d event ids, want the %d billable message ids",
			len(got), len(want))
	}
}

func TestWriteEventsReportsAnUnmappedBillableTransition(t *testing.T) {
	// A reboot is a notification nova publishes and the collector's table holds
	// no entry for, which is what a workload catalog that drifted from the
	// mapping looks like from here.
	const instanceID = "3f1a9c62-8b45-4d07-9e13-6c2a5f8b41d0"
	transition := Transition{
		At:          time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
		EventType:   "compute.instance.reboot.end",
		Exchange:    "nova",
		Billable:    true,
		MessageID:   "b7d4e1a0-52c9-4f68-8a3b-1e05d9c7f264",
		PublisherID: "compute.compute-01",
		ProjectID:   "9c4d2f81a5b3476e8d17f0a2c6b93e54",
		UserID:      "2e8b7a0c34d54f19b6c8a1e50f37d942",
		ResourceID:  instanceID,
		Payload:     map[string]any{"instance_id": instanceID},
	}
	path := filepath.Join(t.TempDir(), "events.jsonl")

	err := WriteEvents(path, testCloud, Schedule{transition})

	const want = "transition compute.instance.reboot.end at 2026-07-03T10:00:00Z " +
		"is marked billable but the mapping claims nothing for it"
	if err == nil || err.Error() != want {
		t.Fatalf("WriteEvents() error = %v, want %q", err, want)
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("stat %s: %v", path, statErr)
	}
	if info.Size() != 0 {
		t.Errorf("%s holds %d bytes, want the failed write to have put nothing in it", path, info.Size())
	}
}

func TestReadStreamReportsAnEmptyFile(t *testing.T) {
	path := writeLines(t)

	_, err := ReadStream(path)

	want := path + " holds no notifications"
	if err == nil || err.Error() != want {
		t.Fatalf("ReadStream() error = %v, want %q", err, want)
	}
}

func TestReadStreamNamesTheBadLine(t *testing.T) {
	schedule := generateMonth(t, 1, july2026, testCloud)
	valid := make([][]byte, 2)
	for i := range valid {
		valid[i] = encodeLine(t, Line{
			Exchange:   schedule[i].Exchange,
			RoutingKey: collectorTopic,
			Body:       render(t, schedule[i]),
		})
	}

	t.Run("a line that is not a stream line", func(t *testing.T) {
		path := writeLines(t, valid[0], []byte("not json"))

		_, err := ReadStream(path)

		prefix := path + ": line 2: "
		if err == nil || !strings.HasPrefix(err.Error(), prefix) {
			t.Fatalf("ReadStream() error = %v, want it to start with %q", err, prefix)
		}
	})

	t.Run("a notification without a timestamp", func(t *testing.T) {
		// The envelope is built by hand because Render always writes a timestamp,
		// while a recording taken off a real bus may hold a notification that
		// carries none.
		const untimed = `{"oslo.version":"2.0",` +
			`"oslo.message":"{\"message_id\":\"x\",\"event_type\":\"image.upload\",\"payload\":{}}"}`
		path := writeLines(t, valid[0], valid[1], encodeLine(t, Line{
			Exchange:   "glance",
			RoutingKey: collectorTopic,
			Body:       json.RawMessage(untimed),
		}))

		_, err := ReadStream(path)

		want := path + ": line 3: the oslo notification has no timestamp"
		if err == nil || err.Error() != want {
			t.Fatalf("ReadStream() error = %v, want %q", err, want)
		}
	})
}

func TestReadStreamReportsAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notifications.jsonl")

	_, err := ReadStream(path)

	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadStream() error = %v, want it to report a missing file", err)
	}
	prefix := "reading " + path + ": "
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Errorf("ReadStream() error = %q, want it to start with %q", err, prefix)
	}
}

func TestWriteStreamReportsAnUnwritablePath(t *testing.T) {
	// A --out whose directory was never created is what an operator hits, and
	// the error names the file rather than the syscall that refused it.
	path := filepath.Join(t.TempDir(), "absent", "notifications.jsonl")

	err := WriteStream(path, generateMonth(t, 1, july2026, testCloud))

	prefix := "writing " + path + ": "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("WriteStream() error = %v, want it to start with %q", err, prefix)
	}
}

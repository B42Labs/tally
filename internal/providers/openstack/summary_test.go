package openstack

import (
	"context"
	"log/slog"
	"math"
	"reflect"
	"testing"
	"time"
)

// testSummaryInterval is the interval every summary under test is built with,
// which is the collector's default.
const testSummaryInterval = 60 * time.Second

// runSummary runs a Summary until its waits are used up and returns what it
// logged. Each of waits stands for one elapsed interval: it does the test's
// recording and the wait then returns nil, so a line follows it. The wait after
// the last of them returns context.Canceled, which is what ends Run.
//
// connected, depth and oldest are what the three closures report on every line.
func runSummary(t *testing.T, m *Metrics, connected bool, depth int64, oldest float64,
	waits ...func(),
) *logCapture {
	t.Helper()

	return runSummaryReading(t, m,
		func() bool { return connected },
		func() int64 { return depth },
		func() float64 { return oldest },
		waits...)
}

// runSummaryReading is runSummary for a session and an outbox that change
// between two lines: the Summary reads them through the three closures, and a
// wait changes what they report.
func runSummaryReading(t *testing.T, m *Metrics, connected func() bool, depth func() int64,
	oldest func() float64, waits ...func(),
) *logCapture {
	t.Helper()

	logs := &logCapture{}
	summary := NewSummary(testSummaryInterval, m, connected, depth, oldest, slog.New(logs))
	taken := 0
	summary.sleep = func(_ context.Context, d time.Duration) error {
		if d != testSummaryInterval {
			t.Errorf("the summary waited %v, want the interval of %v", d, testSummaryInterval)
		}
		if taken == len(waits) {
			return context.Canceled
		}
		waits[taken]()
		taken++
		return nil
	}

	if err := summary.Run(t.Context()); err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}
	return logs
}

// summaryLines returns the attributes of every record logs holds, and fails the
// test unless there are want records and each is the summary line at Info.
func summaryLines(t *testing.T, logs *logCapture, want int) []map[string]any {
	t.Helper()

	logs.mu.Lock()
	defer logs.mu.Unlock()

	if len(logs.records) != want {
		t.Fatalf("%d records logged, want %d: %v", len(logs.records), want, logs.records)
	}
	lines := make([]map[string]any, 0, want)
	for _, record := range logs.records {
		if record.level != slog.LevelInfo || record.message != "summary" {
			t.Fatalf("logged %q at %s, want %q at %s", record.message, record.level, "summary", slog.LevelInfo)
		}
		lines = append(lines, record.attrs)
	}
	return lines
}

func TestSummaryLogsWhatHappenedSinceThePreviousLine(t *testing.T) {
	m := freshMetrics(t)

	logs := runSummary(t, m, true, 7, 42.5,
		func() {
			m.Consumed("compute.instance.create.end")
			m.Consumed("volume.create.end")
			m.Skipped("compute.instance.reboot.start")
			m.Unparseable()
			m.Delivered(5)
			m.DeliveryError()
		},
		func() { m.Consumed("image.delete") },
	)

	lines := summaryLines(t, logs, 2)
	if want := map[string]any{
		"interval_seconds":        int64(60),
		"connected":               true,
		"consumed":                int64(2),
		"skipped":                 int64(1),
		"unparseable":             int64(1),
		"delivered":               int64(5),
		"delivery_errors":         int64(1),
		"buffered":                int64(7),
		"oldest_buffered_seconds": int64(42),
	}; !reflect.DeepEqual(lines[0], want) {
		t.Errorf("the first line carries %v, want %v", lines[0], want)
	}
	// The second line holds what the second wait recorded and nothing of the
	// first: the counts are differences, not totals.
	if want := map[string]any{
		"interval_seconds":        int64(60),
		"connected":               true,
		"consumed":                int64(1),
		"skipped":                 int64(0),
		"unparseable":             int64(0),
		"delivered":               int64(0),
		"delivery_errors":         int64(0),
		"buffered":                int64(7),
		"oldest_buffered_seconds": int64(42),
	}; !reflect.DeepEqual(lines[1], want) {
		t.Errorf("the second line carries %v, want %v", lines[1], want)
	}
}

// TestSummaryReadsTheSessionAndTheOutboxForEveryLine covers the state that
// changes between two lines: the consumer loses its session and the outbox
// fills, and the second line reports what holds then.
func TestSummaryReadsTheSessionAndTheOutboxForEveryLine(t *testing.T) {
	connected, depth, oldest := true, int64(0), 0.0

	logs := runSummaryReading(t, freshMetrics(t),
		func() bool { return connected },
		func() int64 { return depth },
		func() float64 { return oldest },
		func() {},
		func() { connected, depth, oldest = false, 7, 42.5 },
	)

	lines := summaryLines(t, logs, 2)
	for i, want := range []map[string]any{
		{"connected": true, "buffered": int64(0), "oldest_buffered_seconds": int64(0)},
		{"connected": false, "buffered": int64(7), "oldest_buffered_seconds": int64(42)},
	} {
		for name, value := range want {
			if got := lines[i][name]; got != value {
				t.Errorf("line %d: %s = %v, want %v", i+1, name, got, value)
			}
		}
	}
}

// TestSummaryLogsAnIdleInterval covers the collector at rest: an interval in
// which nothing was recorded is logged with zeros and not skipped, because the
// line is how a collector that receives nothing still reports itself.
func TestSummaryLogsAnIdleInterval(t *testing.T) {
	logs := runSummary(t, freshMetrics(t), false, 0, 0, func() {})

	lines := summaryLines(t, logs, 1)
	if want := map[string]any{
		"interval_seconds":        int64(60),
		"connected":               false,
		"consumed":                int64(0),
		"skipped":                 int64(0),
		"unparseable":             int64(0),
		"delivered":               int64(0),
		"delivery_errors":         int64(0),
		"buffered":                int64(0),
		"oldest_buffered_seconds": int64(0),
	}; !reflect.DeepEqual(lines[0], want) {
		t.Errorf("the line carries %v, want %v", lines[0], want)
	}
}

// TestSummaryLogsZerosWithoutMetrics covers the collector built with a nil
// *Metrics, which is the supported way to turn the instrumentation off.
func TestSummaryLogsZerosWithoutMetrics(t *testing.T) {
	logs := runSummary(t, nil, true, 7, 42.5, func() {})

	line := summaryLines(t, logs, 1)[0]
	for _, count := range []string{"consumed", "skipped", "unparseable", "delivered", "delivery_errors"} {
		if got, ok := line[count]; !ok || got != int64(0) {
			t.Errorf("%s = %v, want int64(0) (the line carries %v)", count, got, line)
		}
	}
}

// TestSummaryLeavesOutTheAgeOfAnUnreadableBuffer covers the outbox that cannot
// be read, which reports its oldest event's age as NaN. The JSON handler prints
// a NaN as an error text, so the line carries no age then, and everything else.
func TestSummaryLeavesOutTheAgeOfAnUnreadableBuffer(t *testing.T) {
	logs := runSummary(t, freshMetrics(t), true, 7, math.NaN(), func() {})

	line := summaryLines(t, logs, 1)[0]
	if got, ok := line["oldest_buffered_seconds"]; ok {
		t.Errorf("oldest_buffered_seconds = %v, want the attribute left out", got)
	}
	if want := map[string]any{
		"interval_seconds": int64(60),
		"connected":        true,
		"consumed":         int64(0),
		"skipped":          int64(0),
		"unparseable":      int64(0),
		"delivered":        int64(0),
		"delivery_errors":  int64(0),
		"buffered":         int64(7),
	}; !reflect.DeepEqual(line, want) {
		t.Errorf("the line carries %v, want the other eight attributes %v", line, want)
	}
}

// TestSummaryStopsWithoutALine covers the shutdown: a wait that ends on a done
// context ends Run, and the interval it cut short gets no line.
func TestSummaryStopsWithoutALine(t *testing.T) {
	logs := runSummary(t, freshMetrics(t), true, 7, 42.5)

	summaryLines(t, logs, 0)
}

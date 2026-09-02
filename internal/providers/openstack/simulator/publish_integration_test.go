package simulator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/b42labs/tally/internal/core/cardinality"
	"github.com/b42labs/tally/internal/providers/openstack"
)

// The broker these tests run against. Every test starts one of its own, because
// the collector's queue name is fixed and two tests on one broker would consume
// each other's month.
const (
	brokerImage = "rabbitmq:4-alpine"
	brokerPort  = "5672/tcp"
	// The port listens well before the broker accepts a connection on it, so the
	// wait is on RabbitMQ's own boot marker instead.
	brokerReadyLog = "Server startup complete"
)

// How a test waits for something it cannot be notified of: how often it looks
// and how long it keeps looking. They are the collector's own
// (internal/providers/openstack/amqp_integration_test.go), so a simulated month
// is given as long to arrive as a hand-published notification is.
const (
	pollInterval = 25 * time.Millisecond
	pollDeadline = 30 * time.Second
)

// testOutboxMax is the outbox bound the collector runs under here. Only the
// billable notifications reach the outbox at all, about 1800 for a month of
// seed 1, since the noise is skipped before it. Nothing drains the outbox
// during a test, so the bound stays far above that: a consumer that paused on
// backpressure would stall the run rather than fail it.
const testOutboxMax = 100_000

// startBroker runs a RabbitMQ container for the test and returns the URL it is
// reachable under, together with the stop that terminates it. The stop also runs
// at the end of the test, so a test that never calls it leaves nothing behind.
func startBroker(t *testing.T) (string, func()) {
	t.Helper()

	ctx := context.Background()
	container, err := testcontainers.Run(ctx, brokerImage,
		testcontainers.WithExposedPorts(brokerPort),
		testcontainers.WithWaitStrategy(wait.ForLog(brokerReadyLog)),
	)
	stop := sync.OnceFunc(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminating the broker container: %v", err)
		}
	})
	t.Cleanup(stop)
	if err != nil {
		t.Fatalf("starting the broker container: %v", err)
	}

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("reading the container host: %v", err)
	}
	port, err := container.MappedPort(ctx, brokerPort)
	if err != nil {
		t.Fatalf("reading the mapped broker port: %v", err)
	}
	// The guest account is the image's own, and the container is reachable only
	// through its ephemeral host port for the length of one test.
	return fmt.Sprintf("amqp://guest:guest@%s/", net.JoinHostPort(host, port.Port())), stop
}

// connect dials the broker the way a run does and closes the connection when the
// test ends.
func connect(t *testing.T, url string) *Publisher {
	t.Helper()

	publisher, err := Connect(url)
	if err != nil {
		t.Fatalf("Connect() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if err := publisher.Close(); err != nil {
			t.Errorf("Close() error = %v, want nil", err)
		}
	})
	return publisher
}

// defaultCollectorExchanges are the exchanges a collector binds when nothing
// sets TALLY_OSC_EXCHANGES: the default of the variable in
// internal/providers/openstack/config.go.
var defaultCollectorExchanges = []string{"nova", "neutron", "cinder", "glance"}

// startCollector runs the collector's own consumer against the broker, bound to
// the exchanges it is given, into an outbox and a registry of its own, until
// the test ends. It is the collector as the pipeline runs it: the same
// consumer, the same buffer, and the same counters, so what a test asserts is
// what a deployment would see.
//
// The returned stop waits for the consumer's loop to return before it closes the
// outbox, so nothing the collector logs outlives the test and nothing touches a
// closed handle.
func startCollector(t *testing.T, url string, exchanges []string) (*openstack.Outbox, *prometheus.Registry, func()) {
	t.Helper()

	outbox, err := openstack.OpenOutbox(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatalf("OpenOutbox() error = %v, want nil", err)
	}

	reg := prometheus.NewRegistry()
	m := openstack.NewMetrics(reg,
		func() float64 { return float64(outbox.Depth()) },
		outbox.OldestBufferedSeconds)
	consumer := openstack.NewConsumer(openstack.Config{
		AMQPURL:         url,
		Exchanges:       exchanges,
		Topics:          []string{collectorTopic},
		Cloud:           testCloud,
		Prefetch:        10,
		BufferMaxEvents: testOutboxMax,
	}, outbox, m, testLogger(t))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := consumer.Run(ctx); err != nil {
			t.Errorf("Run() error = %v, want nil", err)
		}
	}()

	stop := sync.OnceFunc(func() {
		cancel()
		<-done
		if err := outbox.Close(); err != nil {
			t.Errorf("closing the outbox: %v", err)
		}
	})
	t.Cleanup(stop)
	return outbox, reg, stop
}

// testLogger writes the simulator's and the collector's log through the test, so
// that a failing test carries the publishes and reconnects that led to it.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(testWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// waitFor polls until condition holds, and fails the test naming what it waited
// for when it never does.
func waitFor(t *testing.T, why string, condition func() bool) {
	t.Helper()

	for deadline := time.Now().Add(pollDeadline); time.Now().Before(deadline); {
		if condition() {
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("timed out after %v waiting until %s", pollDeadline, why)
}

// capturedLogger is the run's logger over a writer that keeps a copy of what it
// wrote, for the cases that assert on a line the run logged. It logs at info,
// so the copy holds the run's own lines rather than the debug line of every
// published notification.
func capturedLogger(t *testing.T) (*slog.Logger, *captureWriter) {
	t.Helper()

	writer := &captureWriter{t: t}
	return slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelInfo})), writer
}

// captureWriter passes the log through to the test and keeps it, so a case that
// reads one line back still has the whole log under a failure.
type captureWriter struct {
	t  *testing.T
	mu sync.Mutex
	sb strings.Builder
}

func (w *captureWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.sb.Write(p)
	w.mu.Unlock()

	w.t.Log(strings.TrimSuffix(string(p), "\n"))
	return len(p), nil
}

// String is everything the run has logged so far.
func (w *captureWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.sb.String()
}

// reservePort takes a free port off the operating system and hands it straight
// back, so a case knows the port a run's control endpoint will bind. A run
// picks its own with a configured port of 0, and that one it never reports
// anywhere a test reads.
func reservePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("the reserved address is %v, want a TCP one", listener.Addr())
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("giving the reserved port back: %v", err)
	}
	return addr.Port
}

// controlRequest sends one request to the control endpoint of a run on port and
// returns the status and the body. The bool is false when nothing answered: a
// run dials the broker and waits for the collector before it binds its port, so
// a poll can arrive before the endpoint is there.
func controlRequest(t *testing.T, port int, method, path string) (int, string, bool) {
	t.Helper()

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequestWithContext(t.Context(), method, url, nil)
	if err != nil {
		t.Fatalf("building %s %s: %v", method, url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", false
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the answer to %s %s: %v", method, url, err)
	}
	return resp.StatusCode, string(body), true
}

// waitForHold polls the control endpoint until the run reports that it holds
// the last share of the month back, and returns the document it saw there. It
// is how a case reaches the hold: the last notifications of a month may be
// noise nothing else waits for.
//
// The wait is on holding rather than on the counts, the way a script driving a
// drill waits: the published count reaches total minus held while the run is
// still on its way into the hold, and a release sent on it is refused.
func waitForHold(t *testing.T, port, held int) clockDocument {
	t.Helper()

	var doc clockDocument
	waitFor(t, "the run holds the last share of the month back", func() bool {
		status, body, answered := controlRequest(t, port, http.MethodGet, "/clock")
		if !answered {
			return false
		}
		if status != http.StatusOK {
			t.Fatalf("GET /clock = %d, want %d (body %q)", status, http.StatusOK, body)
		}
		doc = decodeDocument(t, body)
		return doc.Holding && doc.Held == held
	})
	return doc
}

// depthStaysAt fails the test when the outbox moves off depth within a second.
// It is what tells a run that holds notifications back from one that is merely
// slow: a held share never arrives on its own.
func depthStaysAt(t *testing.T, outbox *openstack.Outbox, depth int64) {
	t.Helper()

	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if got := outbox.Depth(); got != depth {
			t.Fatalf("Depth() = %d, want it to stay at %d: the held notifications went out unasked",
				got, depth)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// counterValue reads one counter out of the collector's registry, through the
// exposition rather than through the instrument, since the instruments belong to
// the collector package. A family or a child that was never touched reads 0,
// which is what a test asserting that nothing was skipped needs. An empty
// labelName addresses the unlabeled counter.
func counterValue(t *testing.T, reg *prometheus.Registry, name, labelName, labelValue string) float64 {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v, want nil", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if labelName == "" && len(metric.GetLabel()) == 0 {
				return metric.GetCounter().GetValue()
			}
			for _, label := range metric.GetLabel() {
				if label.GetName() == labelName && label.GetValue() == labelValue {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// skippedSum adds up every child of tally_collector_skipped_total, whatever
// event type it carries, and reads 0 while the family is absent. A test waits
// on the sum rather than on one type, because the sum is what says the whole
// month has been handled: a per-type count is only worth comparing once it has.
func skippedSum(t *testing.T, reg *prometheus.Registry) float64 {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v, want nil", err)
	}
	sum := 0.0
	for _, family := range families {
		if family.GetName() != "tally_collector_skipped_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			sum += metric.GetCounter().GetValue()
		}
	}
	return sum
}

// storedEventIDs lists the event ids the outbox holds, sorted so that a test
// compares sets rather than the order the consumer happened to buffer in.
func storedEventIDs(t *testing.T, outbox *openstack.Outbox, n int) []string {
	t.Helper()

	rows, err := outbox.Batch(t.Context(), n)
	if err != nil {
		t.Fatalf("Batch(%d) error = %v, want nil", n, err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, decodeEventID(t, row.EventJSON))
	}
	slices.Sort(ids)
	return ids
}

// billableMessageIDs lists the message ids of the notifications the collector
// records an event for, sorted. They are the event ids the outbox has to end up
// with, because ingestion books an event under the message id it arrived as.
func billableMessageIDs(t *testing.T, schedule Schedule) []string {
	t.Helper()

	ids := make([]string, 0, len(schedule))
	for _, transition := range schedule.Billable() {
		ids = append(ids, transition.MessageID)
	}
	slices.Sort(ids)
	return ids
}

// eventIDsOf lists the event ids an events.jsonl holds, sorted. It is what the
// run wrote down as its expectation, held against what the collector produced.
func eventIDsOf(t *testing.T, path string) []string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	ids := []string{}
	for _, line := range strings.Split(string(content), "\n") {
		if line == "" {
			continue
		}
		ids = append(ids, decodeEventID(t, []byte(line)))
	}
	slices.Sort(ids)
	return ids
}

// decodeEventID reads the event id of one recorded event.
func decodeEventID(t *testing.T, eventJSON []byte) string {
	t.Helper()

	var stored struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(eventJSON, &stored); err != nil {
		t.Fatalf("decoding an event: %v", err)
	}
	return stored.EventID
}

// TestRunPublishesWhatTheCollectorConsumes drives a whole simulated month
// through a real broker into the collector's own consumer. It is what the
// simulator exists for: the month it generates has to be a month the unmodified
// collector maps, buffers, and counts, and the events.jsonl it writes has to say
// the same thing the outbox ends up holding.
func TestRunPublishesWhatTheCollectorConsumes(t *testing.T) {
	url, _ := startBroker(t)
	// The publisher connects first, because its declares are what the collector's
	// passive ones find: a consumer started against a fresh broker reconnects
	// until the exchanges exist.
	publisher := connect(t, url)
	outbox, reg, _ := startCollector(t, url, ServiceExchanges)

	out := t.TempDir()
	err := Run(t.Context(), Config{Cloud: testCloud, HTTPPort: 0}, RunOptions{
		Period:           "2026-07",
		Seed:             1,
		Factor:           0,
		Out:              out,
		WaitForCollector: pollDeadline,
		MetricsInterval:  testMetricsInterval,
	}, publisher, testLogger(t))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	month := generateMonth(t, 1, july2026, testCloud)
	want := billableMessageIDs(t, month)
	waitFor(t, "every billable notification is buffered", func() bool {
		return outbox.Depth() == int64(len(want))
	})
	if got := storedEventIDs(t, outbox, len(want)); !slices.Equal(got, want) {
		t.Errorf("buffered event ids = %v, want %v", got, want)
	}
	// The file the run wrote is its statement of what the collector would produce,
	// so it is held against what the collector did produce rather than trusted.
	if got := eventIDsOf(t, filepath.Join(out, "events.jsonl")); !slices.Equal(got, want) {
		t.Errorf("the event ids in events.jsonl = %v, want %v", got, want)
	}
	// The oracle beside them is one this build reads back, and it carries the
	// traffic of the month although this run pushed none of it: what a drill
	// reads the intended figure off is the file, not the endpoint.
	oracle, err := ReadOracle(filepath.Join(out, "oracle.json"))
	if err != nil {
		t.Fatalf("ReadOracle() error = %v, want nil", err)
	}
	if len(oracle.Traffic) == 0 {
		t.Error("oracle.json states no traffic row, want the rows the run placed")
	}

	// What the rest of the month becomes in the collector: every notification the
	// mapping records nothing for is counted as skipped under the type it arrived
	// as. That counter is how a deployment sees the types it bills nothing for,
	// so the month is held to it type by type.
	expected := map[string]int{}
	skippedTotal := 0
	for _, transition := range month {
		if transition.Billable {
			continue
		}
		expected[transition.EventType]++
		skippedTotal++
	}
	// The consumer counts a skip when it handles the message, and the last
	// messages of a month may be noise that arrives after the last billable one,
	// so a full outbox does not yet mean the counters are complete.
	waitFor(t, "every non-billable notification is counted as skipped", func() bool {
		return skippedSum(t, reg) == float64(skippedTotal)
	})

	// The month carries one image.create per image, nine in all: two for each of
	// the three classic projects, one for each of the two Gardener tenants, and
	// one for the CI tenant. Beside them stands the noise catalogue, and the
	// mapping records nothing for any of it: an announced image has no size yet,
	// and no noise type is billable. The counts below hold each of those series
	// to the month.
	if got := counterValue(t, reg, "tally_collector_skipped_total", "event_type", "image.create"); got != 9 {
		t.Errorf("tally_collector_skipped_total{event_type=\"image.create\"} = %v, want 9", got)
	}
	for _, eventType := range slices.Sorted(maps.Keys(expected)) {
		got := counterValue(t, reg, "tally_collector_skipped_total", "event_type", eventType)
		if got != float64(expected[eventType]) {
			t.Errorf("tally_collector_skipped_total{event_type=%q} = %v, want %d",
				eventType, got, expected[eventType])
		}
	}
	// The 83 types of the month lie inside the bound the collector holds the
	// label's values to, so none of them is folded into the overflow value.
	if got := counterValue(t, reg, "tally_collector_skipped_total",
		"event_type", cardinality.Overflow); got != 0 {
		t.Errorf("tally_collector_skipped_total{event_type=%q} = %v, want 0", cardinality.Overflow, got)
	}
	// And nothing in the month went unrecorded for another reason: a notification
	// the collector cannot parse is one the simulator rendered wrong.
	if got := counterValue(t, reg, "tally_collector_unparseable_total", "", ""); got != 0 {
		t.Errorf("tally_collector_unparseable_total = %v, want 0", got)
	}
}

// TestACollectorAtItsDefaultExchangesMissesTheOtherFourExchanges is what a
// deployment that left TALLY_OSC_EXCHANGES alone gets. A topic exchange copies
// a message only to the queues bound to it, so what the month publishes on
// octavia, keystone, designate and barbican reaches a collector bound to the
// other four neither as an event nor as a skip: it is dropped at the broker,
// and nothing in the collector counts it. The noise on the four bound
// exchanges is what such a collector does see, as skips.
func TestACollectorAtItsDefaultExchangesMissesTheOtherFourExchanges(t *testing.T) {
	url, _ := startBroker(t)
	// The publisher declares all eight, so the four unbound exchanges exist and
	// take their messages whether a queue is bound to them or not.
	publisher := connect(t, url)
	outbox, reg, _ := startCollector(t, url, defaultCollectorExchanges)

	err := Run(t.Context(), Config{Cloud: testCloud, HTTPPort: 0}, RunOptions{
		Period:           "2026-07",
		Seed:             1,
		Factor:           0,
		Out:              t.TempDir(),
		WaitForCollector: pollDeadline,
		MetricsInterval:  testMetricsInterval,
	}, publisher, testLogger(t))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	billable := generateMonth(t, 1, july2026, testCloud).Billable()
	want := make([]string, 0, len(billable))
	for _, transition := range billable {
		if transition.Exchange != "octavia" {
			want = append(want, transition.MessageID)
		}
	}
	slices.Sort(want)
	if len(want) == len(billable) {
		t.Fatalf("the month carries no billable notification on the octavia exchange, want the load " +
			"balancers of the shoots: a month without one holds nothing back from this collector")
	}

	waitFor(t, "every notification off the four default exchanges is buffered", func() bool {
		return outbox.Depth() == int64(len(want))
	})
	if got := storedEventIDs(t, outbox, len(want)); !slices.Equal(got, want) {
		t.Errorf("buffered event ids = %v, want %v", got, want)
	}

	// What it does see instead of the four exchanges it is not bound to: nova's
	// noise, of which the audits are the largest part. The daily audits run to
	// the end of the month and may be among its last messages, so the counter is
	// waited for before the skips are read.
	const audits = "compute.instance.exists"
	waitFor(t, "the daily audits are counted as skipped", func() bool {
		return counterValue(t, reg, "tally_collector_skipped_total", "event_type", audits) > 0
	})

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v, want nil", err)
	}
	for _, family := range families {
		if family.GetName() != "tally_collector_skipped_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() != "event_type" ||
					slices.Contains(defaultCollectorExchanges, exchangeFor(label.GetValue())) {
					continue
				}
				t.Errorf("tally_collector_skipped_total{event_type=%q} = %v, want the type in no counter "+
					"at all: a message the broker drops is one the collector never sees",
					label.GetValue(), metric.GetCounter().GetValue())
			}
		}
	}
}

// TestReplayPublishesACapturedMonth runs the two halves the drill uses: a run
// that only writes files, and a replay that puts that file on a broker later.
// What comes out of the collector has to be the same month either way, because
// a recorded month is what a drill replays when it has no generator at hand.
func TestReplayPublishesACapturedMonth(t *testing.T) {
	out := t.TempDir()
	err := Run(t.Context(), Config{Cloud: testCloud}, RunOptions{
		Period:          "2026-07",
		Seed:            1,
		Factor:          0,
		Out:             out,
		MetricsInterval: testMetricsInterval,
	}, nil, testLogger(t))
	if err != nil {
		t.Fatalf("Run() in file mode error = %v, want nil", err)
	}

	url, _ := startBroker(t)
	publisher := connect(t, url)
	outbox, _, _ := startCollector(t, url, ServiceExchanges)

	replayErr := Replay(t.Context(), Config{HTTPPort: 0}, ReplayOptions{
		In:               filepath.Join(out, "notifications.jsonl"),
		Factor:           0,
		WaitForCollector: pollDeadline,
	}, publisher, testLogger(t))
	if replayErr != nil {
		t.Fatalf("Replay() error = %v, want nil", replayErr)
	}

	want := billableMessageIDs(t, generateMonth(t, 1, july2026, testCloud))
	waitFor(t, "every replayed notification is buffered", func() bool {
		return outbox.Depth() == int64(len(want))
	})
	if got := storedEventIDs(t, outbox, len(want)); !slices.Equal(got, want) {
		t.Errorf("buffered event ids = %v, want %v", got, want)
	}

	t.Run("reports a missing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "notifications.jsonl")
		err := Replay(t.Context(), Config{HTTPPort: 0}, ReplayOptions{In: path}, publisher, testLogger(t))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Replay() error = %v, want it to wrap fs.ErrNotExist", err)
		}
		if prefix := "reading " + path + ": "; !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("Replay() error = %q, want it to start with %q", err, prefix)
		}
	})
}

// TestRunWaitsForTheCollector covers the wait on both settings, against a broker
// nothing consumes from. The wait is what keeps a demo from publishing a month
// into a topic exchange no queue is bound to, where every notification is
// dropped without a trace.
func TestRunWaitsForTheCollector(t *testing.T) {
	url, _ := startBroker(t)
	publisher := connect(t, url)

	t.Run("publishes at once when the wait is disabled", func(t *testing.T) {
		// The bound is a context rather than a bare timeout, so a run that does
		// wait is stopped and waited for instead of left behind logging into a
		// finished test.
		ctx, cancel := context.WithTimeout(t.Context(), pollDeadline)
		defer cancel()

		done := make(chan error, 1)
		go func() {
			done <- Run(ctx, Config{Cloud: testCloud}, RunOptions{
				Period:           "2026-07",
				Seed:             2,
				WaitForCollector: 0,
				MetricsInterval:  testMetricsInterval,
			}, publisher, testLogger(t))
		}()

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run() error = %v, want nil", err)
			}
		case <-ctx.Done():
			<-done
			t.Fatalf("Run() did not finish within %v with the wait disabled", pollDeadline)
		}
	})

	t.Run("refuses to publish into nothing", func(t *testing.T) {
		const wait = 2 * time.Second
		err := Run(t.Context(), Config{Cloud: testCloud}, RunOptions{
			Period:           "2026-07",
			Seed:             2,
			WaitForCollector: wait,
			MetricsInterval:  testMetricsInterval,
		}, publisher, testLogger(t))

		want := "no consumer on the queue tally-notifications appeared within 2s; " +
			"start the collector first, or pass --wait-for-collector 0 to publish anyway"
		if err == nil || err.Error() != want {
			t.Fatalf("Run() error = %v, want %q", err, want)
		}

		// And it published nothing while it waited: a collector started now binds
		// its queue and finds it empty, however long it looks.
		outbox, _, _ := startCollector(t, url, ServiceExchanges)
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			if depth := outbox.Depth(); depth != 0 {
				t.Fatalf("Depth() = %d, want 0: the refused run published anyway", depth)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
}

// TestConnectFailsWhenTheBrokerIsGone is the error an operator meets most: the
// broker in the compose stack is not up yet, or not up any more. Connect has to
// say which of its steps failed, because a run that reported an unadorned
// network error would leave them guessing between the broker and the URL.
func TestConnectFailsWhenTheBrokerIsGone(t *testing.T) {
	url, stop := startBroker(t)
	stop()

	publisher, err := Connect(url)
	if err == nil {
		_ = publisher.Close()
		t.Fatal("Connect() error = nil, want a failed dial")
	}
	if !strings.Contains(err.Error(), "dialing the broker") {
		t.Errorf("Connect() error = %q, want it to name the dial", err)
	}
}

// TestRunHoldsBackUntilReleased is the held-back switch end to end: the month
// goes out but for the share the switch keeps, the run stays there until
// somebody asks for it, and what the collector ends up holding is the whole
// month either way. It is the drill the switch exists for, where a backlog is
// let go long after the notifications it holds were timestamped.
func TestRunHoldsBackUntilReleased(t *testing.T) {
	url, _ := startBroker(t)
	publisher := connect(t, url)
	outbox, _, _ := startCollector(t, url, ServiceExchanges)

	month := faultyMonth(t, 1, Faults{HeldBack: true})
	want := billableMessageIDs(t, month.Schedule)
	held := len(month.Held)
	if held == 0 {
		t.Fatal("the month holds nothing back, want the switch to have picked notifications")
	}

	port := reservePort(t)
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Cloud: testCloud, HTTPPort: port}, RunOptions{
			Period:           "2026-07",
			Seed:             1,
			Factor:           0,
			Faults:           []string{FaultHeldBack},
			WaitForCollector: pollDeadline,
			MetricsInterval:  testMetricsInterval,
		}, publisher, testLogger(t))
	}()

	onTheBus := int64(len(want) - held)
	waitFor(t, "every billable notification but the held ones is buffered", func() bool {
		return outbox.Depth() == onTheBus
	})
	doc := waitForHold(t, port, held)
	if doc.Published != doc.Total-held {
		t.Errorf("GET /clock reports %d of %d published, want %d: the month is holding",
			doc.Published, doc.Total, doc.Total-held)
	}
	depthStaysAt(t, outbox, onTheBus)

	status, body, answered := controlRequest(t, port, http.MethodPost, "/release")
	if !answered {
		t.Fatal("POST /release reached nothing, want the endpoint to serve the hold")
	}
	if status != http.StatusOK {
		t.Fatalf("POST /release = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if got := decodeDocument(t, body).Held; got != 0 {
		t.Errorf("POST /release document held = %d, want 0", got)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(pollDeadline):
		t.Fatalf("Run() did not finish within %v after the release", pollDeadline)
	}

	waitFor(t, "the released notifications are buffered too", func() bool {
		return outbox.Depth() == int64(len(want))
	})
	if got := storedEventIDs(t, outbox, len(want)); !slices.Equal(got, want) {
		t.Errorf("buffered event ids = %v, want %v", got, want)
	}
}

// cloudRequest sends one request to the fake OpenStack API a run serves on port,
// under the token a caller holds. controlRequest beside it sends neither a body
// nor a credential, which is every request the control routes take.
func cloudRequest(t *testing.T, port int, method, path, token, body string,
) (int, http.Header, string) {
	t.Helper()

	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building %s %s: %v", method, url, err)
	}
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the answer to %s %s: %v", method, url, err)
	}
	return resp.StatusCode, resp.Header, string(answer)
}

// instancesAtPeriodStart are the instances the oracle holds at the first instant
// of its month, by the intervals alone: an interval that begins there contains
// it, and a resource whose first one begins later does not exist yet. The
// pre-existing switch is what puts instances there, because the oracle clips a
// life that began before the period to its first instant.
func instancesAtPeriodStart(oracle Oracle) []string {
	var ids []string
	for _, resource := range oracle.Resources {
		if resource.ResourceType != typeInstance {
			continue
		}
		if resource.Intervals[0].From.Equal(oracle.PeriodFrom) {
			ids = append(ids, resource.ResourceID)
		}
	}
	slices.Sort(ids)
	return ids
}

// TestRunServesTheFakeAPIOnTheControlListener is the mount a run composes: the
// OpenStack API of the month it publishes is answered on the very port the
// control routes are on, so a sync reaching the simulator authenticates against
// the run and reads the cloud the notifications came from. What the fake API
// answers with is held in api_test.go against a handler built there; what only a
// run puts together is that the handler is on the listener at all.
func TestRunServesTheFakeAPIOnTheControlListener(t *testing.T) {
	url, _ := startBroker(t)
	publisher := connect(t, url)
	startCollector(t, url, ServiceExchanges)

	// A factor of 0 stops virtual time at the first instant of the month, so the
	// cloud holds the pre-existing instances for as long as the run is up, and
	// held-back is what keeps it up: the run waits in the hold until it is asked
	// to release, and the fake API goes down with the run.
	month := faultyMonth(t, 1, Faults{PreExisting: true, HeldBack: true})
	held := len(month.Held)
	if held == 0 {
		t.Fatal("the month holds nothing back, want the switch to have picked notifications")
	}
	want := instancesAtPeriodStart(month.Oracle)
	if len(want) == 0 {
		t.Fatal("the month holds no instance at its first instant, want the pre-existing ones")
	}

	port := reservePort(t)
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{Cloud: testCloud, HTTPPort: port}, RunOptions{
			Period:           "2026-07",
			Seed:             1,
			Factor:           0,
			Faults:           []string{FaultPreExisting, FaultHeldBack},
			WaitForCollector: pollDeadline,
			MetricsInterval:  testMetricsInterval,
		}, publisher, testLogger(t))
	}()
	waitForHold(t, port, held)

	credentials := fmt.Sprintf(`{"auth": {"identity": {"methods": ["password"], "password": `+
		`{"user": {"name": %q, "password": %q, "domain": {"name": "Default"}}}}}}`,
		cloudUsername, cloudPassword)
	status, header, answer := cloudRequest(t, port, http.MethodPost, "/v3/auth/tokens", "", credentials)
	if status != http.StatusCreated {
		t.Fatalf("POST /v3/auth/tokens on the run's listener = %d, want %d (body %q)",
			status, http.StatusCreated, answer)
	}
	token := header.Get("X-Subject-Token")
	if token == "" {
		t.Fatal("the answer carries no X-Subject-Token, want the token this run issued")
	}

	status, _, answer = cloudRequest(t, port, http.MethodGet, serversPath, token, "")
	if status != http.StatusOK {
		t.Fatalf("GET %s on the run's listener = %d, want %d (body %q)",
			serversPath, status, http.StatusOK, answer)
	}
	served := servedIDs(t, answer, "servers")
	slices.Sort(served)
	if !slices.Equal(served, want) {
		t.Errorf("the live listing answered with %v, want the instances the month holds at its "+
			"first instant %v", served, want)
	}

	// The hold is let go the way the case above lets it go, so the run ends before
	// the broker it publishes to does.
	status, body, answered := controlRequest(t, port, http.MethodPost, "/release")
	if !answered {
		t.Fatal("POST /release reached nothing, want the endpoint to serve the hold")
	}
	if status != http.StatusOK {
		t.Fatalf("POST /release = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(pollDeadline):
		t.Fatalf("Run() did not finish within %v after the release", pollDeadline)
	}
}

// TestRunStoppedWhileHoldingKeepsTheHeldNotificationsBack is what SIGINT does
// to a run that is holding: the process ends with exit status 0, and the share
// it held never reaches the bus. That is the other half of the drill, where the
// backlog is thrown away rather than let go.
func TestRunStoppedWhileHoldingKeepsTheHeldNotificationsBack(t *testing.T) {
	url, _ := startBroker(t)
	publisher := connect(t, url)
	outbox, _, _ := startCollector(t, url, ServiceExchanges)

	month := faultyMonth(t, 1, Faults{HeldBack: true})
	want := billableMessageIDs(t, month.Schedule)
	held := len(month.Held)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logger, log := capturedLogger(t)
	port := reservePort(t)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Cloud: testCloud, HTTPPort: port}, RunOptions{
			Period:           "2026-07",
			Seed:             1,
			Factor:           0,
			Faults:           []string{FaultHeldBack},
			WaitForCollector: pollDeadline,
			MetricsInterval:  testMetricsInterval,
		}, publisher, logger)
	}()

	waitForHold(t, port, held)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil: a stop while holding is a clean stop", err)
		}
	case <-time.After(pollDeadline):
		t.Fatalf("Run() did not finish within %v after the stop", pollDeadline)
	}
	if !strings.Contains(log.String(), "stopped") {
		t.Errorf("the run logged %q, want it to report the stop", log.String())
	}

	onTheBus := int64(len(want) - held)
	waitFor(t, "everything the run published is buffered", func() bool {
		return outbox.Depth() == onTheBus
	})
	depthStaysAt(t, outbox, onTheBus)
}

// TestRunStoppedWhileRegisteringWritesNothing is what SIGINT does to a run that
// is registering: the process ends with exit status 0, the log says how far the
// registration came, and neither a file nor a notification is left behind that
// the run never got to. The registration is the one step of a run that turns a
// failure into a clean stop, so the two halves of it are held here.
func TestRunStoppedWhileRegisteringWritesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	logger, log := capturedLogger(t)
	out := t.TempDir()

	// https and a closed port: the registration is checked before it is sent, and
	// the cancelled context ends it before anything is dialled.
	err := Run(ctx, Config{
		Cloud:        testCloud,
		ReportingURL: "https://127.0.0.1:1",
		APIToken:     "tly_a_secret-of-the-test",
		GardenCloud:  gardenCloud,
	}, RunOptions{
		Period: "2026-07", Seed: 1, Out: out, RegisterProjects: true,
		MetricsInterval: testMetricsInterval,
	}, nil, logger)
	if err != nil {
		t.Fatalf("Run() error = %v, want nil: a stop while registering is a clean stop", err)
	}
	for _, want := range []string{"registration incomplete", "stopped"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the run logged %q, want a %q line: what reached the registry is what an "+
				"operator has to know after a stop", log.String(), want)
		}
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 0 {
		t.Errorf("the output directory holds %v (error %v), want nothing: the month is written after "+
			"the registration", entries, err)
	}
}

// TestReleaseFailsWhenTheBrokerIsGone covers the release that cannot publish
// what it let out. The endpoint answers it, because all a release does is close
// a channel, and the run reports the failed publish afterwards: a broker that
// went away during the hold is no clean stop.
func TestReleaseFailsWhenTheBrokerIsGone(t *testing.T) {
	url, stopBroker := startBroker(t)
	// Dialled here rather than through connect: the broker is terminated while
	// the run holds, and a close of a connection that is already gone is not what
	// this case is about.
	publisher, err := Connect(url)
	if err != nil {
		t.Fatalf("Connect() error = %v, want nil", err)
	}
	defer func() { _ = publisher.Close() }()

	held := len(faultyMonth(t, 1, Faults{HeldBack: true}).Held)
	port := reservePort(t)
	done := make(chan error, 1)
	go func() {
		// No collector, so the wait for one is disabled: what this case needs of
		// the broker is that it takes the month and then stops existing.
		done <- Run(t.Context(), Config{Cloud: testCloud, HTTPPort: port}, RunOptions{
			Period:           "2026-07",
			Seed:             1,
			Factor:           0,
			Faults:           []string{FaultHeldBack},
			WaitForCollector: 0,
			MetricsInterval:  testMetricsInterval,
		}, publisher, testLogger(t))
	}()

	waitForHold(t, port, held)
	stopBroker()

	status, body, answered := controlRequest(t, port, http.MethodPost, "/release")
	if !answered {
		t.Fatal("POST /release reached nothing, want the endpoint to serve the hold")
	}
	if status != http.StatusOK {
		t.Fatalf("POST /release = %d, want %d (body %q)", status, http.StatusOK, body)
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "publishing to") {
			t.Fatalf("Run() error = %v, want it to name the publish that failed", err)
		}
	case <-time.After(pollDeadline):
		t.Fatalf("Run() did not finish within %v after the release", pollDeadline)
	}
}

// TestRunPublishesDuplicatesTheCollectorHandsOn is the duplicates switch
// against the collector: a copy carries the message id of the notification it
// repeats, so the collector buffers both and the Reporting API books one. What
// this case holds is the collector's half of that, the depth the outbox reaches
// with both copies in it.
func TestRunPublishesDuplicatesTheCollectorHandsOn(t *testing.T) {
	url, _ := startBroker(t)
	publisher := connect(t, url)
	outbox, reg, _ := startCollector(t, url, ServiceExchanges)

	month := faultyMonth(t, 1, Faults{Duplicates: true})
	want := billableMessageIDs(t, month.Schedule)
	duplicates := len(month.Stream) - len(month.Schedule)
	if duplicates == 0 {
		t.Fatalf("the stream repeats nothing, want one in %d billable transitions twice", duplicateShare)
	}

	err := Run(t.Context(), Config{Cloud: testCloud, HTTPPort: 0}, RunOptions{
		Period:           "2026-07",
		Seed:             1,
		Factor:           0,
		Faults:           []string{FaultDuplicates},
		WaitForCollector: pollDeadline,
		MetricsInterval:  testMetricsInterval,
	}, publisher, testLogger(t))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	waitFor(t, "both copies of every repeated notification are buffered", func() bool {
		return outbox.Depth() == int64(len(want)+duplicates)
	})
	// A copy is the bytes of its original, so nothing about it is unparseable:
	// the collector hands it on and ingestion is what drops it.
	if got := counterValue(t, reg, "tally_collector_unparseable_total", "", ""); got != 0 {
		t.Errorf("tally_collector_unparseable_total = %v, want 0", got)
	}
}

// TestRunPublishesRefusedShapesTheCollectorRefuses is the refused-shapes switch
// against the collector: the oversized and the truncated twin are counted as
// unparseable, the versioned one as skipped under the type it arrived as, and
// the outbox ends up holding the very events a month without the switch
// produces.
func TestRunPublishesRefusedShapesTheCollectorRefuses(t *testing.T) {
	url, _ := startBroker(t)
	publisher := connect(t, url)
	outbox, reg, _ := startCollector(t, url, ServiceExchanges)

	month := faultyMonth(t, 1, Faults{RefusedShapes: true})
	want := billableMessageIDs(t, month.Schedule)

	// A twin is told by its message id: it stands in the stream and in no
	// schedule. The three shapes are told apart the way faults.go builds them.
	scheduled := make(map[string]bool, len(month.Schedule))
	for _, transition := range month.Schedule {
		scheduled[transition.MessageID] = true
	}
	versioned := make(map[string]int)
	unparseable := 0
	for _, twin := range month.Stream {
		switch {
		case scheduled[twin.MessageID]:
		case twin.truncated, twin.Payload["fault_padding"] != nil:
			unparseable++
		case strings.HasPrefix(twin.EventType, "instance."):
			versioned[twin.EventType]++
		default:
			t.Fatalf("the stream carries a twin of no known shape: %s", twin.EventType)
		}
	}
	if unparseable == 0 || len(versioned) == 0 {
		t.Fatalf("the stream carries %d unparseable twins and %d versioned types, want both shapes",
			unparseable, len(versioned))
	}

	err := Run(t.Context(), Config{Cloud: testCloud, HTTPPort: 0}, RunOptions{
		Period:           "2026-07",
		Seed:             1,
		Factor:           0,
		Faults:           []string{FaultRefusedShapes},
		WaitForCollector: pollDeadline,
		MetricsInterval:  testMetricsInterval,
	}, publisher, testLogger(t))
	if err != nil {
		t.Fatalf("Run() error = %v, want nil", err)
	}

	waitFor(t, "every twin the collector cannot parse is counted", func() bool {
		return counterValue(t, reg, "tally_collector_unparseable_total", "", "") == float64(unparseable)
	})

	// The skips of the month and the versioned twins together, waited for as a
	// sum before any one type is read: a type is only worth comparing once the
	// whole month has been handled.
	skippedTotal := 0
	for _, transition := range month.Schedule {
		if !transition.Billable {
			skippedTotal++
		}
	}
	for _, count := range versioned {
		skippedTotal += count
	}
	waitFor(t, "every skipped notification is counted", func() bool {
		return skippedSum(t, reg) == float64(skippedTotal)
	})
	for _, eventType := range slices.Sorted(maps.Keys(versioned)) {
		got := counterValue(t, reg, "tally_collector_skipped_total", "event_type", eventType)
		if got != float64(versioned[eventType]) {
			t.Errorf("tally_collector_skipped_total{event_type=%q} = %v, want %d",
				eventType, got, versioned[eventType])
		}
	}
	// The versioned names stand beside the month's own and still leave the label
	// inside the bound the collector admits.
	if got := counterValue(t, reg, "tally_collector_skipped_total",
		"event_type", cardinality.Overflow); got != 0 {
		t.Errorf("tally_collector_skipped_total{event_type=%q} = %v, want 0", cardinality.Overflow, got)
	}

	// And the events are the ones a month without the switch produces: a refused
	// twin is refused, not booked.
	waitFor(t, "every billable notification is buffered", func() bool {
		return outbox.Depth() == int64(len(want))
	})
	if got := storedEventIDs(t, outbox, len(want)); !slices.Equal(got, want) {
		t.Errorf("buffered event ids = %v, want %v", got, want)
	}
}

// metricsStub stands in for the OTLP/HTTP endpoint a run pushes to. It records
// what a request carried rather than the request itself: a month at factor 0
// arrives as hundreds of batches of five thousand points, and a stub keeping
// the bodies would hold the whole month a second time.
//
// The statuses are answered one per request, the last of them repeated, the way
// the stand-in of otlp_test.go answers them. No status at all is 200 throughout.
type metricsStub struct {
	*httptest.Server
	mu       sync.Mutex
	statuses []int
	requests []pushedBatch
}

// pushedBatch is what the stub saw of one request: how many points it carried,
// whether it authenticated, and the series in it.
type pushedBatch struct {
	points int
	basic  bool
	series map[string]pushedSeries
}

// pushedSeries is one metric name inside one request: the points it carried and
// the shape it arrived under. A counter has to reach the collector as a
// monotonic cumulative sum, because that is what the remote write exporter
// stores under the name the sample was pushed with.
type pushedSeries struct {
	points      int
	sum         bool
	monotonic   bool
	temporality int
}

// startMetricsStub runs the stand-in endpoint for the test.
func startMetricsStub(t *testing.T, statuses ...int) *metricsStub {
	t.Helper()

	stub := &metricsStub{statuses: statuses}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var export otlpExport
		// A body that ends early is passed over rather than failed on: a stopped
		// run drops the request it was sending, and half a document arriving is
		// that stop working. What a whole document holds is held in otlp_test.go,
		// and a build that wrote none at all would leave the counts below at zero.
		if err := json.NewDecoder(r.Body).Decode(&export); err != nil {
			return
		}
		_, _, ok := r.BasicAuth()
		batch := pushedBatch{basic: ok, series: make(map[string]pushedSeries)}
		for _, resource := range export.ResourceMetrics {
			for _, scope := range resource.ScopeMetrics {
				for _, metric := range scope.Metrics {
					series := batch.series[metric.Name]
					if metric.Sum != nil {
						series.sum = true
						series.monotonic = metric.Sum.IsMonotonic
						series.temporality = metric.Sum.AggregationTemporality
						series.points += len(metric.Sum.DataPoints)
						batch.points += len(metric.Sum.DataPoints)
					}
					if metric.Gauge != nil {
						series.points += len(metric.Gauge.DataPoints)
						batch.points += len(metric.Gauge.DataPoints)
					}
					batch.series[metric.Name] = series
				}
			}
		}

		stub.mu.Lock()
		stub.requests = append(stub.requests, batch)
		status := http.StatusOK
		if len(stub.statuses) > 0 {
			status = stub.statuses[min(len(stub.requests)-1, len(stub.statuses)-1)]
		}
		stub.mu.Unlock()

		if status/100 != 2 {
			http.Error(w, "the collector refused the batch", status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(stub.Close)
	return stub
}

// taken is the batches the endpoint took. The pusher reaches the handler from
// the run's goroutine and a case reads the slice from its own, so it is guarded
// and handed out as a copy.
func (s *metricsStub) taken() []pushedBatch {
	s.mu.Lock()
	defer s.mu.Unlock()

	return slices.Clone(s.requests)
}

// pushSettleWait is the window a case watches the stand-in endpoint for once a
// run has returned, and it waits two of them. The first lets the batch the
// endpoint was still handling when the run gave up on it arrive, and a count
// that moves during the second is a pusher that outlived the run: one at factor
// 0 walks the month batch after batch.
const pushSettleWait = 500 * time.Millisecond

// TestPushSamplesFlushesEveryStepOfAPacedRun is the branch a drill runs under.
// At a factor above zero the clock has not reached the next grid instant by the
// time the step has been folded, so the batch of a step leaves with that step
// instead of filling to the cap; the cases around it run at factor 0, where
// virtual time stands still and the cap is what flushes. A month whose metrics
// all arrived in one lump at the end would leave the panels of a paced drill
// empty for as long as it ran.
func TestPushSamplesFlushesEveryStepOfAPacedRun(t *testing.T) {
	stub := startMetricsStub(t)

	// Twelve grid steps of a period of its own rather than a whole month: the
	// factor puts each of them a twentieth of a second of wall time behind the
	// one before, and a month at that pace would outlast the test by hours.
	const steps = 12
	from := july2026
	month := Month{
		Tenants: []Tenant{{ID: cloudTenant, Name: "tenant-a", Workload: workloadClassic}},
		Oracle: Oracle{
			Cloud:      testCloud,
			PeriodFrom: from,
			PeriodTo:   from.Add(steps * testMetricsInterval),
		},
	}
	factor := 20 * testMetricsInterval.Seconds()

	var pushed atomic.Int64
	err := pushSamples(t.Context(), NewClock(from, factor, time.Now), &metricsRun{
		pusher:   NewPusher(stub.URL, pushUser, pushPassword, testCloud, nil),
		month:    month,
		interval: testMetricsInterval,
	}, testLogger(t), &pushed)
	if err != nil {
		t.Fatalf("pushSamples() error = %v, want nil", err)
	}

	if n := len(stub.taken()); n != steps {
		t.Errorf("the endpoint took %d requests over %d grid steps, want one per step: "+
			"the batch of a step leaves with that step on a paced run", n, steps)
	}
	if pushed.Load() == 0 {
		t.Error("the run counted no pushed point, want the inventory of every step")
	}
}

// TestRunPushesTheMetricsAndServesTheInventory is the second face of a month
// end to end: while the notifications go out, the traffic counters and the
// inventory are pushed to the endpoint the configuration names, and the
// inventory of the instant the run stands at is served on the control
// listener. What each of the two states is held in metrics_test.go and
// exporter_test.go; what only a run puts together is that both go out beside
// the month.
func TestRunPushesTheMetricsAndServesTheInventory(t *testing.T) {
	url, _ := startBroker(t)
	publisher := connect(t, url)
	startCollector(t, url, ServiceExchanges)
	stub := startMetricsStub(t)

	// A factor of 0 stops virtual time at the first instant of the month, and
	// held-back is what keeps the run up long enough to scrape it: it waits in
	// the hold, and the endpoint goes down with the run.
	month := faultyMonth(t, 1, Faults{PreExisting: true, HeldBack: true})
	held := len(month.Held)
	if held == 0 {
		t.Fatal("the month holds nothing back, want the switch to have picked notifications")
	}
	samples, _, err := TrafficOf(month.Oracle, 1, testMetricsInterval)
	if err != nil {
		t.Fatalf("TrafficOf() error = %v, want nil", err)
	}

	logger, log := capturedLogger(t)
	port := reservePort(t)
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Config{
			Cloud:          testCloud,
			HTTPPort:       port,
			MetricsEnabled: true,
			OTLPURL:        stub.URL,
			OTLPUser:       pushUser,
			OTLPPassword:   pushPassword,
			OTLPInsecure:   true,
		}, RunOptions{
			Period:           "2026-07",
			Seed:             1,
			Factor:           0,
			Faults:           []string{FaultPreExisting, FaultHeldBack},
			WaitForCollector: pollDeadline,
			MetricsInterval:  testMetricsInterval,
		}, publisher, logger)
	}()
	waitForHold(t, port, held)

	status, body, answered := controlRequest(t, port, http.MethodGet, "/metrics")
	if !answered {
		t.Fatal("GET /metrics reached nothing, want the inventory on the run's listener")
	}
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	for _, want := range []string{
		seriesNovaTotalVMs, seriesCinderVolumes, seriesNeutronFloatingIPs,
		seriesGlanceImages, seriesLoadBalancerTotal,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scrape carries no %s, want the aggregate the dashboards read", want)
		}
	}
	// A scrape carries no static label of its own: platform and cloud come from
	// the scrape job, and an endpoint stating them would push the job's own under
	// exported_cloud.
	if strings.Contains(body, "platform=") {
		t.Error("the scrape carries a platform label, want the scrape job to state it")
	}

	status, body, answered = controlRequest(t, port, http.MethodPost, "/release")
	if !answered {
		t.Fatal("POST /release reached nothing, want the endpoint to serve the hold")
	}
	if status != http.StatusOK {
		t.Fatalf("POST /release = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(pollDeadline):
		t.Fatalf("Run() did not finish within %v after the release", pollDeadline)
	}

	batches := stub.taken()
	if len(batches) < 2 {
		t.Fatalf("the endpoint took %d requests, want a month to arrive in several", len(batches))
	}
	traffic, inventory := 0, 0
	pushedNames := make(map[string]bool)
	for number, batch := range batches {
		if batch.points > maxDataPoints {
			t.Errorf("request %d carried %d points, want at most %d", number, batch.points, maxDataPoints)
		}
		if !batch.basic {
			t.Errorf("request %d carried no Basic credential, want every push to authenticate", number)
		}
		for _, name := range []string{egressSeries, ingressSeries} {
			series, ok := batch.series[name]
			if !ok {
				continue
			}
			traffic += series.points
			if !series.sum || !series.monotonic || series.temporality != otlpCumulative {
				t.Fatalf("request %d states %s as sum=%v monotonic=%v temporality=%d, "+
					"want a monotonic cumulative sum", number, name, series.sum, series.monotonic,
					series.temporality)
			}
		}
		// Everything else in the batch is the inventory of the grid step, which
		// the push carries beside the counters: the scrape above reads the same
		// world through the exporter, and nothing but this states that it reaches
		// the endpoint at all.
		for name, series := range batch.series {
			if name == egressSeries || name == ingressSeries {
				continue
			}
			pushedNames[name] = true
			inventory += series.points
			if series.sum {
				t.Errorf("request %d states the inventory series %s as a sum, "+
					"want the gauge a world read at an instant is", number, name)
			}
		}
	}
	if inventory == 0 {
		t.Error("the endpoint took no inventory point, want the gauges beside the counters")
	}
	for _, name := range []string{seriesNovaTotalVMs, seriesIdentityProjects, seriesNeutronRouter} {
		if !pushedNames[name] {
			t.Errorf("the endpoint took no %s point, want the pushed month to hold what the scraped one holds",
				name)
		}
	}
	// Every traffic sample the month placed reached the endpoint exactly once:
	// the cursor hands each grid step to the step it belongs to, and no batch
	// repeats one.
	if traffic != len(samples) {
		t.Errorf("the endpoint took %d traffic points, want the %d the month placed", traffic, len(samples))
	}
	if !strings.Contains(log.String(), "msg=completed") || strings.Contains(log.String(), "pushed=0") {
		t.Errorf("the run logged %q, want a completed line with the points it pushed", log.String())
	}
}

// TestRunReportsAFailedPushAfterTheMonth is the endpoint that refuses every
// batch: the month goes out whole regardless, because that is what the operator
// asked for, and the run ends with the failed push so the exit status says the
// metric half of it never arrived.
func TestRunReportsAFailedPushAfterTheMonth(t *testing.T) {
	url, _ := startBroker(t)
	publisher := connect(t, url)
	outbox, _, _ := startCollector(t, url, ServiceExchanges)
	stub := startMetricsStub(t, http.StatusServiceUnavailable)

	want := billableMessageIDs(t, generateMonth(t, 1, july2026, testCloud))
	logger, log := capturedLogger(t)
	err := Run(t.Context(), Config{
		Cloud:        testCloud,
		HTTPPort:     0,
		OTLPURL:      stub.URL,
		OTLPUser:     pushUser,
		OTLPPassword: pushPassword,
		OTLPInsecure: true,
	}, RunOptions{
		Period:           "2026-07",
		Seed:             1,
		Factor:           0,
		WaitForCollector: pollDeadline,
		MetricsInterval:  testMetricsInterval,
	}, publisher, logger)

	if err == nil {
		t.Fatal("Run() error = nil, want the refused push")
	}
	for _, fragment := range []string{"pushing metrics to " + stub.URL, "503"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("Run() error = %q, want it to contain %q", err, fragment)
		}
	}
	// The message reaches whatever an operator pastes it into, and the credential
	// is never in it.
	if strings.Contains(err.Error(), pushPassword) {
		t.Errorf("Run() error = %q, want it to keep the password out", err)
	}
	// The exit status waits for the month, and the log does not: a drill paced
	// over an hour would otherwise publish for the rest of it with nothing but
	// healthy progress in the log.
	if !strings.Contains(log.String(), "msg=\"pushing stopped\"") {
		t.Errorf("the run logged %q, want the push to be reported where it stopped", log.String())
	}
	// And the month is on the bus whole: the push is reported after the last
	// notification, not instead of it.
	waitFor(t, "every billable notification is buffered", func() bool {
		return outbox.Depth() == int64(len(want))
	})
}

// TestRunStoppedWhilePushingIsACleanStop is what SIGINT does to the pusher: the
// cancellation reaches it as the context's own error, which is no failed push,
// so the process ends with exit status 0 the way a stop during the publishing
// does. The run waits for the pusher on its way out, so nothing pushes a month
// the run has already left behind.
func TestRunStoppedWhilePushingIsACleanStop(t *testing.T) {
	url, _ := startBroker(t)
	publisher := connect(t, url)
	startCollector(t, url, ServiceExchanges)
	stub := startMetricsStub(t)

	month := faultyMonth(t, 1, Faults{HeldBack: true})
	held := len(month.Held)
	if held == 0 {
		t.Fatal("the month holds nothing back, want the switch to have picked notifications")
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	port := reservePort(t)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Cloud:        testCloud,
			HTTPPort:     port,
			OTLPURL:      stub.URL,
			OTLPUser:     pushUser,
			OTLPPassword: pushPassword,
			OTLPInsecure: true,
		}, RunOptions{
			Period:           "2026-07",
			Seed:             1,
			Factor:           0,
			Faults:           []string{FaultHeldBack},
			WaitForCollector: pollDeadline,
			MetricsInterval:  testMetricsInterval,
		}, publisher, testLogger(t))
	}()

	waitForHold(t, port, held)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil: a stop during the push is a clean stop", err)
		}
	case <-time.After(pollDeadline):
		t.Fatalf("Run() did not finish within %v after the stop", pollDeadline)
	}

	time.Sleep(pushSettleWait)
	taken := len(stub.taken())
	if taken == 0 {
		t.Fatal("the endpoint took no request, want the run to have pushed while it published")
	}
	time.Sleep(pushSettleWait)
	if again := len(stub.taken()); again != taken {
		t.Errorf("the endpoint took %d requests after the run returned, want it to stay at %d: "+
			"the pusher outlived the run", again, taken)
	}
}

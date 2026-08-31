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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
		Period: "2026-07",
		Seed:   1,
		Factor: 0,
		Out:    out,
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
	}, RunOptions{Period: "2026-07", Seed: 1, Out: out, RegisterProjects: true}, nil, logger)
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

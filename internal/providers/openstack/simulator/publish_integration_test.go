package simulator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
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

// testOutboxMax is the outbox bound the collector runs under here. Nothing
// drains the outbox during a test, so the bound is far above the few hundred
// notifications one month produces: a consumer that paused on backpressure
// would stall the run rather than fail it.
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

// startCollector runs the collector's own consumer against the broker, into an
// outbox and a registry of its own, until the test ends. It is the collector as
// the pipeline runs it: the same consumer, the same buffer, and the same
// counters, so what a test asserts is what a deployment would see.
//
// The returned stop waits for the consumer's loop to return before it closes the
// outbox, so nothing the collector logs outlives the test and nothing touches a
// closed handle.
func startCollector(t *testing.T, url string) (*openstack.Outbox, *prometheus.Registry, func()) {
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
		Exchanges:       collectorExchanges,
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
	outbox, reg, _ := startCollector(t, url)

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

	want := billableMessageIDs(t, generateMonth(t, 1, july2026, testCloud))
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

	// The month carries one image.create per image, six in all, and the mapping
	// skips every one of them because an announced image has no size yet. Nothing
	// else in the month may go unrecorded: a notification the collector cannot
	// parse is one the simulator rendered wrong.
	if got := counterValue(t, reg, "tally_collector_skipped_total", "event_type", "image.create"); got != 6 {
		t.Errorf("tally_collector_skipped_total{event_type=\"image.create\"} = %v, want 6", got)
	}
	if got := counterValue(t, reg, "tally_collector_unparseable_total", "", ""); got != 0 {
		t.Errorf("tally_collector_unparseable_total = %v, want 0", got)
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
	outbox, _, _ := startCollector(t, url)

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
		outbox, _, _ := startCollector(t, url)
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

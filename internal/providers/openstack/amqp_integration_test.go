package openstack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// The broker the tests run against. Every test starts one of its own: the
// collector's queue name is fixed, so two tests on one broker would consume each
// other's notifications.
const (
	brokerImage = "rabbitmq:4-alpine"
	brokerPort  = "5672/tcp"
	// The port is listening well before the broker accepts a connection on it, so
	// the wait is on RabbitMQ's own boot marker instead.
	brokerReadyLog = "Server startup complete"
)

// testTopic is the notification topic the tests bind and publish under, which is
// the collector's default.
const testTopic = "notifications.info"

// The consumer's waits, shortened so that a test spends milliseconds where the
// collector spends seconds.
const (
	testMinBackoff  = 50 * time.Millisecond
	testMaxBackoff  = 200 * time.Millisecond
	testPausePoll   = 50 * time.Millisecond
	testInsertRetry = 100 * time.Millisecond
)

// How a test waits for something it cannot be notified of: how often it looks,
// how long it keeps looking, and how often it republishes for a subscriber it
// cannot ask about.
const (
	pollInterval     = 25 * time.Millisecond
	pollDeadline     = 30 * time.Second
	republishInteval = 250 * time.Millisecond
)

// testBufferMax is the outbox bound of a test that is not about backpressure:
// far more than it publishes, so the consumer never pauses.
const testBufferMax = 1000

// startBroker runs a RabbitMQ container for the test and returns the URL it is
// reachable under.
func startBroker(t *testing.T) string {
	t.Helper()

	ctx := context.Background()
	container, err := testcontainers.Run(ctx, brokerImage,
		testcontainers.WithExposedPorts(brokerPort),
		testcontainers.WithWaitStrategy(wait.ForLog(brokerReadyLog)),
	)
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("terminating the broker container: %v", err)
		}
	})
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
	return fmt.Sprintf("amqp://guest:guest@%s/", net.JoinHostPort(host, port.Port()))
}

// openChannel connects to the broker the way an OpenStack service would and
// hands back a channel to publish and inspect through.
func openChannel(t *testing.T, url string) *amqp091.Channel {
	t.Helper()

	conn, err := amqp091.Dial(url)
	if err != nil {
		t.Fatalf("dialing the broker at %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	channel, err := conn.Channel()
	if err != nil {
		t.Fatalf("opening a channel: %v", err)
	}
	return channel
}

// declareExchanges declares the service exchanges the way the OpenStack services
// declare them, which is what the collector's passive declares then find.
func declareExchanges(t *testing.T, channel *amqp091.Channel, exchanges ...string) {
	t.Helper()

	for _, exchange := range exchanges {
		if err := channel.ExchangeDeclare(exchange, exchangeKind,
			true, false, false, false, nil); err != nil {
			t.Fatalf("declaring the exchange %s: %v", exchange, err)
		}
	}
}

// publish sends one message body to an exchange under the notification topic.
func publish(t *testing.T, channel *amqp091.Channel, exchange string, body []byte) {
	t.Helper()

	if err := channel.PublishWithContext(t.Context(), exchange, testTopic, false, false,
		amqp091.Publishing{ContentType: "application/json", Body: body}); err != nil {
		t.Fatalf("publishing to %s: %v", exchange, err)
	}
}

// publishUntil publishes body every so often until condition holds. It is how a
// test waits for a subscriber it cannot look up on the broker: the dump's queue
// is server-named and exclusive to the dump's own connection, so nothing outside
// it can tell whether the queue is bound yet.
func publishUntil(t *testing.T, channel *amqp091.Channel, exchange string,
	body []byte, why string, condition func() bool,
) {
	t.Helper()

	deadline := time.Now().Add(pollDeadline)
	for time.Now().Before(deadline) {
		publish(t, channel, exchange, body)
		for republished := time.Now().Add(republishInteval); time.Now().Before(republished); {
			if condition() {
				return
			}
			time.Sleep(pollInterval)
		}
	}
	t.Fatalf("timed out after %v waiting until %s", pollDeadline, why)
}

// readyMessages is how many messages the collector's queue holds ready for
// delivery. A message a consumer holds unacknowledged is not one of them, which
// is why a test that reads this as an acknowledgement stops the consumer first:
// an unacknowledged message becomes ready again when its connection closes.
func readyMessages(t *testing.T, channel *amqp091.Channel) int {
	t.Helper()

	queue, err := channel.QueueDeclarePassive(queueName, true, false, false, false, nil)
	if err != nil {
		t.Fatalf("inspecting the queue %s: %v", queueName, err)
	}
	return queue.Messages
}

// fixture reads a captured oslo envelope and the message id it carries, which is
// the event id the collector books it under.
func fixture(t *testing.T, name string) ([]byte, string) {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("testdata", "golden", "notifications", name+".json"))
	if err != nil {
		t.Fatalf("reading the notification fixture %s: %v", name, err)
	}
	notification, err := ParseEnvelope(body)
	if err != nil {
		t.Fatalf("parsing the notification fixture %s: %v", name, err)
	}
	return body, notification.MessageID
}

// instanceNotification builds a complete nova instance creation under the given
// message id, so that a test can publish several notifications that map to
// distinct events.
func instanceNotification(t *testing.T, messageID string) []byte {
	t.Helper()

	return wrap(t, fmt.Sprintf(`{
		"message_id": %q,
		"event_type": "compute.instance.create.end",
		"timestamp": "2026-03-01 12:00:00.000000",
		"_context_project_id": "9c4a1b2d3e4f5061728394a5b6c7d8e9",
		"payload": {
			"instance_id": %q,
			"tenant_id": "9c4a1b2d3e4f5061728394a5b6c7d8e9",
			"state": "active",
			"vcpus": 2,
			"memory_mb": 2048,
			"root_gb": 20,
			"instance_type": "m1.small"
		}
	}`, messageID, "instance-"+messageID))
}

// paddedNotification is a complete nova instance creation whose flavor name is
// padded to filler bytes, which is how a test grows a delivery body, or the
// event it maps to, past the bound it may not cross.
func paddedNotification(t *testing.T, messageID string, filler int) []byte {
	t.Helper()

	return wrap(t, fmt.Sprintf(`{
		"message_id": %q,
		"event_type": "compute.instance.create.end",
		"timestamp": "2026-03-01 12:00:00.000000",
		"payload": {
			"instance_id": %q,
			"tenant_id": "9c4a1b2d3e4f5061728394a5b6c7d8e9",
			"state": "active",
			"instance_type": %q
		}
	}`, messageID, "instance-"+messageID, strings.Repeat("a", filler)))
}

// testConfig is what a test runs the collector under: its own broker, the
// exchanges it declared, and everything else at the collector's defaults.
func testConfig(url string, exchanges []string, bufferMax int64) Config {
	return Config{
		AMQPURL:         url,
		Exchanges:       exchanges,
		Topics:          []string{testTopic},
		Cloud:           "os-test",
		Prefetch:        10,
		BufferMaxEvents: bufferMax,
	}
}

// startConsumer runs a consumer over buffer until the test ends. The returned
// stop waits for the consumer's loop to return, so a test can end the session
// early and nothing the consumer logs outlives the test.
//
// Every consumer under test records into a Metrics of its own, which is
// returned alongside it: the consume path is the only place that decides which
// counter a delivery lands in, so a suite that ran it with nil metrics would
// pin what a counter does once called but not which one gets called.
func startConsumer(t *testing.T, cfg Config, buffer eventBuffer) (*Consumer, *Metrics, func()) {
	t.Helper()

	m := freshMetrics(t)
	consumer := NewConsumer(cfg, buffer, m, testLogger(t))
	consumer.minBackoff = testMinBackoff
	consumer.maxBackoff = testMaxBackoff
	consumer.pausePoll = testPausePoll
	consumer.insertRetry = testInsertRetry

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
	})
	t.Cleanup(stop)
	return consumer, m, stop
}

// consumedTotal is what the consume path counted for one event type under one
// of its two labelled counters.
func consumedTotal(counter *prometheus.CounterVec, eventType string) float64 {
	return testutil.ToFloat64(counter.WithLabelValues(eventType))
}

// startDump runs a dump into a buffer until the test ends, and waits for it to
// return so that nothing it writes outlives the test.
func startDump(t *testing.T, cfg Config) *syncBuffer {
	t.Helper()

	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := Dump(ctx, cfg, out, testLogger(t)); err != nil {
			t.Errorf("Dump() error = %v, want nil", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return out
}

// syncBuffer collects the dump's output. The dump writes from a goroutine of its
// own while the test reads, so both go through one mutex.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// failingBuffer is a real outbox behind a stage that refuses every insert until
// it is repaired, which is what a full disk looks like to the consumer.
type failingBuffer struct {
	inner    *Outbox
	failing  atomic.Bool
	failures atomic.Int64
}

func (b *failingBuffer) Insert(ctx context.Context, eventJSON []byte) error {
	if b.failing.Load() {
		b.failures.Add(1)
		return errors.New("the buffer has no space left")
	}
	return b.inner.Insert(ctx, eventJSON)
}

func (b *failingBuffer) Depth() int64 { return b.inner.Depth() }

// testLogger writes the collector's log through the test, so that a failing test
// carries the reconnects and retries that led to it. Every goroutine that logs
// is waited for before the test returns.
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

// eventID reads the event id of a buffered event.
func eventID(t *testing.T, eventJSON []byte) string {
	t.Helper()

	var stored struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(eventJSON, &stored); err != nil {
		t.Fatalf("decoding a buffered event: %v", err)
	}
	return stored.EventID
}

// storedEventIDs lists the event ids the outbox holds, sorted so that a test
// compares sets rather than the order the consumer happened to buffer in.
func storedEventIDs(t *testing.T, box *Outbox) []string {
	t.Helper()

	ids := []string{}
	for _, row := range readBatch(t, box, 100) {
		ids = append(ids, eventID(t, row.EventJSON))
	}
	slices.Sort(ids)
	return ids
}

// drain reads the outbox the way the sender does and deletes what it read,
// counting every event id it took. It is what lets a paused consumer resume.
func drain(t *testing.T, box *Outbox, taken map[string]int) {
	t.Helper()

	batch := readBatch(t, box, 100)
	for _, row := range batch {
		taken[eventID(t, row.EventJSON)]++
	}
	if err := box.DeleteBatch(context.Background(), batchIDs(batch)); err != nil {
		t.Fatalf("DeleteBatch() error = %v, want nil", err)
	}
}

// dumpLines decodes the dump's output, failing the test on anything that is not
// the one JSON object per line the dump promises.
func dumpLines(t *testing.T, out string) []map[string]any {
	t.Helper()

	lines := []map[string]any{}
	for _, raw := range strings.Split(out, "\n") {
		if raw == "" {
			continue
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("the dump printed %q, want one JSON object per line: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// dumpLineWith is the first dumped line whose member holds want, or nil when no
// line does yet.
func dumpLineWith(t *testing.T, out, member, want string) map[string]any {
	t.Helper()

	for _, line := range dumpLines(t, out) {
		if line[member] == want {
			return line
		}
	}
	return nil
}

// TestConsumerBuffersNotificationsFromEveryDefaultExchange drives the whole
// consume path against a real broker: the four exchanges the collector binds by
// default, one captured notification per service, and the outbox all four have
// to reach before their deliveries are acknowledged.
func TestConsumerBuffersNotificationsFromEveryDefaultExchange(t *testing.T) {
	url := startBroker(t)
	publisher := openChannel(t, url)

	exchanges := []string{"nova", "neutron", "cinder", "glance"}
	fixtures := map[string]string{
		"nova":    "compute-instance-create-end",
		"neutron": "floatingip-create-end",
		"cinder":  "volume-create-end",
		"glance":  "image-upload",
	}
	declareExchanges(t, publisher, exchanges...)

	box := newOutbox(t)
	consumer, m, stop := startConsumer(t, testConfig(url, exchanges, testBufferMax), box)
	waitFor(t, "the consumer is connected", consumer.Connected)

	want := make([]string, 0, len(exchanges))
	for _, exchange := range exchanges {
		body, messageID := fixture(t, fixtures[exchange])
		publish(t, publisher, exchange, body)
		want = append(want, messageID)
	}
	slices.Sort(want)

	waitFor(t, "every published notification is buffered", func() bool {
		return box.Depth() == int64(len(want))
	})
	if got := storedEventIDs(t, box); !slices.Equal(got, want) {
		t.Errorf("buffered event ids = %v, want %v", got, want)
	}
	// Which counter a delivery lands in is decided on this path and nowhere else,
	// so the oslo type of every one of them is asserted where it was recorded.
	for _, eventType := range []string{
		"compute.instance.create.end", "floatingip.create.end", "volume.create.end", "image.upload",
	} {
		if got := consumedTotal(m.consumed, eventType); got != 1 {
			t.Errorf("tally_collector_consumed_total{event_type=%q} = %v, want 1", eventType, got)
		}
	}

	// The acknowledgements only show once the consumer is gone: a message it
	// still holds is not a ready one either way, but one that was never
	// acknowledged becomes ready again when the connection closes.
	stop()
	waitFor(t, "the queue is drained", func() bool { return readyMessages(t, publisher) == 0 })
}

// TestConsumerAcknowledgesWhatItCannotRecord covers the deliveries that produce
// no event, each under the counter it lands in. The two size bounds are the
// reason this matters beyond bookkeeping: without them, a body or an event that
// nothing between the bus and the ingest endpoint bounds is a message the
// collector either dies on before it can acknowledge, or buffers and then
// cannot deliver at any batch size. Either way the broker hands it back forever.
func TestConsumerAcknowledgesWhatItCannotRecord(t *testing.T) {
	url := startBroker(t)
	publisher := openChannel(t, url)
	declareExchanges(t, publisher, "nova")

	box := newOutbox(t)
	consumer, m, stop := startConsumer(t, testConfig(url, []string{"nova"}, testBufferMax), box)
	waitFor(t, "the consumer is connected", consumer.Connected)

	// One channel, so the broker delivers these in the order they are published
	// and the usable one at the end says the four before it were handled.
	publish(t, publisher, "nova", []byte("this is not an oslo envelope"))
	publish(t, publisher, "nova", paddedNotification(t, "11111111-2222-4333-8444-555555555551", bodyMax))
	publish(t, publisher, "nova", paddedNotification(t, "11111111-2222-4333-8444-555555555552", eventMax))
	publish(t, publisher, "nova", wrap(t, `{
		"message_id": "11111111-2222-4333-8444-555555555553",
		"event_type": "compute.instance.reboot.start",
		"timestamp": "2026-03-01 12:00:00.000000",
		"payload": {"instance_id": "instance-1"}
	}`))
	usable := "11111111-2222-4333-8444-555555555554"
	publish(t, publisher, "nova", instanceNotification(t, usable))

	waitFor(t, "the usable notification is buffered", func() bool { return box.Depth() == 1 })
	if got := storedEventIDs(t, box); !slices.Equal(got, []string{usable}) {
		t.Errorf("buffered event ids = %v, want only [%s]", got, usable)
	}

	// The unusable body and the oversized one both fail before there is an event
	// type to label a count with; the oversized event and the type the table does
	// not claim are counted as skipped under the type they arrived as.
	if got := testutil.ToFloat64(m.unparseable); got != 2 {
		t.Errorf("tally_collector_unparseable_total = %v, want the garbage and the oversized body", got)
	}
	if got := consumedTotal(m.skipped, "compute.instance.reboot.start"); got != 1 {
		t.Errorf("tally_collector_skipped_total{event_type=%q} = %v, want 1",
			"compute.instance.reboot.start", got)
	}
	if got := consumedTotal(m.skipped, "compute.instance.create.end"); got != 1 {
		t.Errorf("tally_collector_skipped_total{event_type=%q} = %v, want the oversized event",
			"compute.instance.create.end", got)
	}
	if got := consumedTotal(m.consumed, "compute.instance.create.end"); got != 1 {
		t.Errorf("tally_collector_consumed_total{event_type=%q} = %v, want only the usable one",
			"compute.instance.create.end", got)
	}

	// None of them is requeued: a delivery that fails the way these do fails the
	// same way on every redelivery, so it would sit in front of every message
	// behind it for as long as the collector runs.
	stop()
	waitFor(t, "the queue is drained", func() bool { return readyMessages(t, publisher) == 0 })
}

// TestConsumerRecoversWhenTheExchangeAppearsLater covers the deployment where
// the collector starts before the service it collects has published anything: a
// passive declare against an exchange that is not there fails and closes the
// channel with it, so the consumer has nothing to do but retry.
func TestConsumerRecoversWhenTheExchangeAppearsLater(t *testing.T) {
	url := startBroker(t)
	box := newOutbox(t)
	consumer, _, _ := startConsumer(t, testConfig(url, []string{"nova"}, testBufferMax), box)

	// Several reconnects worth of looking: the consumer must not report itself
	// connected while the exchange it is configured for does not exist.
	for range 20 {
		if consumer.Connected() {
			t.Fatal("Connected() = true, want false while the exchange is missing")
		}
		time.Sleep(pollInterval)
	}
	if depth := box.Depth(); depth != 0 {
		t.Fatalf("Depth() = %d, want 0 while the exchange is missing", depth)
	}

	publisher := openChannel(t, url)
	declareExchanges(t, publisher, "nova")
	waitFor(t, "the consumer connects once the exchange exists", consumer.Connected)

	body, messageID := fixture(t, "compute-instance-create-end")
	publish(t, publisher, "nova", body)

	waitFor(t, "the recovered consumer buffers the notification", func() bool {
		return box.Depth() == 1
	})
	if got := storedEventIDs(t, box); !slices.Equal(got, []string{messageID}) {
		t.Errorf("buffered event ids = %v, want [%s]", got, messageID)
	}
}

// TestConsumerRequeuesADeliveryTheOutboxRefused is the reason the acknowledgement
// comes after the commit: an event that was not buffered leaves its notification
// on the broker, and the redelivery is what stores it once the buffer works.
func TestConsumerRequeuesADeliveryTheOutboxRefused(t *testing.T) {
	url := startBroker(t)
	publisher := openChannel(t, url)
	declareExchanges(t, publisher, "nova")

	box := newOutbox(t)
	buffer := &failingBuffer{inner: box}
	buffer.failing.Store(true)

	consumer, _, _ := startConsumer(t, testConfig(url, []string{"nova"}, testBufferMax), buffer)
	waitFor(t, "the consumer is connected", consumer.Connected)

	body, messageID := fixture(t, "compute-instance-create-end")
	publish(t, publisher, "nova", body)

	waitFor(t, "the refused insert was attempted", func() bool {
		return buffer.failures.Load() > 0
	})

	// The error path waits between attempts, so a buffer that refuses everything
	// is retried a handful of times a second rather than as fast as the broker
	// can redeliver. The bound is loose; a hot loop overruns it by orders of
	// magnitude.
	const window = 10 * testInsertRetry
	const maxAttempts = 20
	before := buffer.failures.Load()
	time.Sleep(window)
	if attempts := buffer.failures.Load() - before; attempts > maxAttempts {
		t.Errorf("%d refused inserts in %v, want at most %d: the retry does not wait",
			attempts, window, maxAttempts)
	}

	buffer.failing.Store(false)
	waitFor(t, "the redelivered notification is buffered", func() bool {
		return box.Depth() == 1
	})
	// Once, not once per attempt: the refused inserts stored nothing, and the
	// requeued delivery came back rather than being dropped.
	if got := storedEventIDs(t, box); !slices.Equal(got, []string{messageID}) {
		t.Errorf("buffered event ids = %v, want [%s]", got, messageID)
	}
}

// TestConsumerPausesAtTheBufferBoundAndLosesNothing publishes more than the
// outbox may hold. What the bound buys is that the surplus waits on the bus:
// consumption stops at the bound and resumes once the buffer has drained, and
// every notification is stored exactly once across the two.
func TestConsumerPausesAtTheBufferBoundAndLosesNothing(t *testing.T) {
	url := startBroker(t)
	publisher := openChannel(t, url)
	declareExchanges(t, publisher, "nova")

	const bound = 2
	box := newOutbox(t)
	consumer, _, _ := startConsumer(t, testConfig(url, []string{"nova"}, bound), box)
	waitFor(t, "the consumer is connected", consumer.Connected)

	want := make([]string, 0, 5)
	for i := range 5 {
		messageID := fmt.Sprintf("11111111-2222-4333-8444-00000000000%d", i)
		publish(t, publisher, "nova", instanceNotification(t, messageID))
		want = append(want, messageID)
	}
	slices.Sort(want)

	waitFor(t, "the outbox reaches its bound", func() bool { return box.Depth() >= bound })
	// Consumption stops there: the depth holds at the bound for as long as the
	// buffer is full, however many notifications are still waiting on the bus.
	time.Sleep(10 * testPausePoll)
	if depth := box.Depth(); depth > bound {
		t.Fatalf("Depth() = %d, want it to stop at the bound %d", depth, bound)
	}

	// Draining is the sender's job; here it is what lets the consumer resume.
	taken := map[string]int{}
	waitFor(t, "every published notification is buffered", func() bool {
		drain(t, box, taken)
		return len(taken) == len(want)
	})

	got := make([]string, 0, len(taken))
	for id, count := range taken {
		got = append(got, id)
		if count != 1 {
			t.Errorf("event id %s was buffered %d times, want once", id, count)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("buffered event ids = %v, want %v", got, want)
	}
}

// TestDumpPrintsNotificationsWithoutTakingThemFromTheConsumer runs the dump
// alongside a collector on the same broker. A topic exchange copies each message
// to every bound queue, so the dump reads a copy and the collector's durable
// queue keeps its own.
func TestDumpPrintsNotificationsWithoutTakingThemFromTheConsumer(t *testing.T) {
	url := startBroker(t)
	publisher := openChannel(t, url)
	declareExchanges(t, publisher, "nova")

	box := newOutbox(t)
	cfg := testConfig(url, []string{"nova"}, testBufferMax)
	consumer, _, _ := startConsumer(t, cfg, box)
	out := startDump(t, cfg)
	waitFor(t, "the consumer is connected", consumer.Connected)

	// The notification is published until the dump prints it, because nothing
	// outside the dump can tell when its queue is bound. The collector buffers a
	// copy of each of them, which is why the assertion below is that it stored
	// this event and not how often.
	body, messageID := fixture(t, "compute-instance-create-end")
	publishUntil(t, publisher, "nova", body, "the dump prints the notification", func() bool {
		return dumpLineWith(t, out.String(), "message_id", messageID) != nil
	})

	line := dumpLineWith(t, out.String(), "message_id", messageID)
	for _, member := range []struct{ name, want string }{
		{name: "exchange", want: "nova"},
		{name: "routing_key", want: testTopic},
		{name: "event_type", want: "compute.instance.create.end"},
	} {
		if got := line[member.name]; got != member.want {
			t.Errorf("the dumped line's %s = %v, want %q", member.name, got, member.want)
		}
	}
	if _, ok := line["payload"].(map[string]any); !ok {
		t.Errorf("the dumped line's payload = %v, want the notification's payload", line["payload"])
	}

	waitFor(t, "the collector buffered its own copy", func() bool {
		return slices.Contains(storedEventIDs(t, box), messageID)
	})

	// A body the parser refuses is printed raw. It is what an operator verifying
	// an unknown deployment most needs to see, and the collector acknowledges it
	// rather than requeueing it, so it costs the queue nothing.
	//
	// Raw stops at the credentials the oslo request context carries. The dump's
	// output is a file an operator redirects and attaches to a ticket, and
	// _context_auth_token is the Keystone token of the request that produced the
	// message, valid for hours. Neither the event type nor the payload shape,
	// which is what the dump exists to show, needs it.
	const token = "gAAAAABlive-keystone-token"
	garbage := `{"event_type": "compute.instance.create.end", "_context_auth_token": "` + token +
		`", "_context_password": "hunter2", "filler": "` + strings.Repeat("z", previewMax) + `"}`
	redacted := strings.NewReplacer(
		`"`+token+`"`, `"[redacted]"`, `"hunter2"`, `"[redacted]"`).Replace(garbage)
	// And what is left of it is cut off after previewMax, because the shape of a
	// message shows in its beginning.
	printed := redacted[:previewMax]
	publishUntil(t, publisher, "nova", []byte(garbage), "the dump prints the unusable body", func() bool {
		return dumpLineWith(t, out.String(), "unparseable", printed) != nil
	})
	if strings.Contains(out.String(), token) {
		t.Error("the dump printed the Keystone token the request context carried")
	}
}

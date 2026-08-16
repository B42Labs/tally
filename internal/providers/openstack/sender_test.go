package openstack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"gopkg.in/yaml.v3"
)

// What every sender under test is built with. The flush interval is a duration
// no backoff can produce, so a recorded wait says by itself which of the two the
// loop took.
const (
	testToken         = "s3cr3t-token"
	testFlushInterval = 7 * time.Second
)

// testEvents are the documents the tests buffer. They are compact, so the array
// the sender posts is their bytes joined by commas.
var testEvents = []string{
	`{"event_id":"a","event_type":"compute.instance.create.end"}`,
	`{"event_id":"b","event_type":"volume.create.end"}`,
	`{"event_id":"c","event_type":"image.delete"}`,
}

// oversizedEvent builds a buffered document past eventMax, which is the bound
// the consumer writes every event under. An event past it is the only one the
// sender drops when the Reporting API refuses it alone.
func oversizedEvent(eventID string) string {
	return `{"event_id":"` + eventID + `","payload":{"filler":"` +
		strings.Repeat("z", eventMax) + `"}}`
}

// logRecord is one logged record, flattened to what the assertions read.
type logRecord struct {
	level   slog.Level
	message string
	attrs   map[string]any
}

// errorAttr is the error the record carries.
func (r logRecord) errorAttr(t *testing.T) error {
	t.Helper()

	err, ok := r.attrs["error"].(error)
	if !ok {
		t.Fatalf("the record %q carries no error attribute: %v", r.message, r.attrs)
	}
	return err
}

// logCapture is a slog handler that keeps its records instead of writing them,
// so a test asserts over a record's attributes rather than over formatted text.
// Run logs from a goroutine of its own, which is what the lock is for.
type logCapture struct {
	mu      sync.Mutex
	records []logRecord
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (c *logCapture) Handle(_ context.Context, record slog.Record) error {
	kept := logRecord{level: record.Level, message: record.Message, attrs: map[string]any{}}
	record.Attrs(func(attr slog.Attr) bool {
		kept.attrs[attr.Key] = attr.Value.Any()
		return true
	})

	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, kept)
	return nil
}

// The sender logs no preset and no grouped attributes, so neither of these has
// anything to keep.
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

// at returns every record logged at level.
func (c *logCapture) at(level slog.Level) []logRecord {
	c.mu.Lock()
	defer c.mu.Unlock()

	var found []logRecord
	for _, record := range c.records {
		if record.level == level {
			found = append(found, record)
		}
	}
	return found
}

// only returns the single record logged at level and fails the test when there
// is not exactly one.
func (c *logCapture) only(t *testing.T, level slog.Level) logRecord {
	t.Helper()

	found := c.at(level)
	if len(found) != 1 {
		t.Fatalf("%d records logged at %s, want 1: %v", len(found), level, found)
	}
	return found[0]
}

// fakeSleeper stands in for the sender's wait. It records what was asked for and
// returns at once, so a test runs through the backoffs in no time, and it is
// what ends Run: the wait after stopAfter of them cancels the loop's context.
type fakeSleeper struct {
	mu        sync.Mutex
	waits     []time.Duration
	stopAfter int
	cancel    context.CancelFunc
}

func (f *fakeSleeper) sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	f.waits = append(f.waits, d)
	taken := len(f.waits)
	f.mu.Unlock()

	if taken >= f.stopAfter {
		f.cancel()
		return ctx.Err()
	}
	return nil
}

// recorded is the waits the sender asked for, in order.
func (f *fakeSleeper) recorded() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]time.Duration(nil), f.waits...)
}

// ingestRequest is one POST the stand-in Reporting API was sent.
type ingestRequest struct {
	method string
	path   string
	header http.Header
	body   []byte
}

// ingestServer stands in for the Reporting API's ingest endpoint: it records
// every request and answers with what answer produces.
type ingestServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []ingestRequest
}

func newIngestServer(t *testing.T, answer http.HandlerFunc) *ingestServer {
	t.Helper()

	server := &ingestServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the posted body: %v", err)
		}
		server.mu.Lock()
		server.requests = append(server.requests, ingestRequest{
			method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body,
		})
		server.mu.Unlock()
		// Put the body back, so an answer that depends on what was posted reads it
		// rather than the drained reader this recording left behind.
		r.Body = io.NopCloser(bytes.NewReader(body))
		answer(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// received is what the server was sent, in order.
func (s *ingestServer) received() []ingestRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]ingestRequest(nil), s.requests...)
}

// answers replies to every request with status and body.
func answers(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// accepted is the answer of a Reporting API that stored the whole batch.
func accepted(events int) http.HandlerFunc {
	return answers(http.StatusOK,
		fmt.Sprintf(`{"accepted":%d,"duplicates":0,"rejected":[]}`, events))
}

// answersInTurn replies with one answer per request and repeats the last one
// once the list has run out.
func answersInTurn(answers ...http.HandlerFunc) http.HandlerFunc {
	var (
		mu     sync.Mutex
		served int
	)
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		answer := answers[min(served, len(answers)-1)]
		served++
		mu.Unlock()
		answer(w, r)
	}
}

// failingBatches puts a stage that fails in front of a real outbox, which is
// what a full volume, a read-only remount, or a lock the busy timeout outlasted
// does to the buffer under a sender that is otherwise working.
type failingBatches struct {
	inner      *Outbox
	failBatch  bool
	failDelete bool
}

func (b *failingBatches) Batch(ctx context.Context, limit int) ([]Row, error) {
	if b.failBatch {
		return nil, errors.New("the outbox is unreadable")
	}
	return b.inner.Batch(ctx, limit)
}

func (b *failingBatches) DeleteBatch(ctx context.Context, ids []int64) error {
	if b.failDelete {
		return errors.New("the outbox is not writable")
	}
	return b.inner.DeleteBatch(ctx, ids)
}

// senderTest is one sender under test, with the doubles its run is observed
// through.
type senderTest struct {
	sender  *Sender
	metrics *Metrics
	logs    *logCapture
	sleeper *fakeSleeper
	ctx     context.Context
	cancel  context.CancelFunc
}

// newSenderTest builds a sender over box that posts to reportingURL and ends
// after stopAfterWaits waits.
func newSenderTest(t *testing.T, box eventBatches, reportingURL string, batchMax, stopAfterWaits int) *senderTest {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sleeper := &fakeSleeper{stopAfter: stopAfterWaits, cancel: cancel}
	logs := &logCapture{}
	m := freshMetrics(t)
	sender := NewSender(box, reportingURL, testToken, batchMax, testFlushInterval, m, slog.New(logs))
	sender.sleep = sleeper.sleep

	return &senderTest{sender: sender, metrics: m, logs: logs, sleeper: sleeper, ctx: ctx, cancel: cancel}
}

// run runs the sender until its sleeper ends it, and joins the goroutine before
// returning so that every assertion reads a finished loop.
func (st *senderTest) run(t *testing.T) {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- st.sender.Run(st.ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run() did not return, want it to end once the context is done")
	}
}

// bufferedEvents is how many events are still waiting to be delivered.
func bufferedEvents(t *testing.T, box *Outbox) int {
	t.Helper()

	return len(readBatch(t, box, 500))
}

// TestSenderPostsTheStoredEventsAsOneJSONArray pins what goes on the wire: the
// bytes the mapping produced, in id order, in one array, with the credential the
// ingest endpoint authorizes on.
func TestSenderPostsTheStoredEventsAsOneJSONArray(t *testing.T) {
	box := newOutbox(t)
	for _, event := range testEvents {
		insertEvent(t, box, event)
	}
	server := newIngestServer(t, accepted(len(testEvents)))
	st := newSenderTest(t, box, server.URL, 500, 1)

	st.run(t)

	requests := server.received()
	if len(requests) != 1 {
		t.Fatalf("the server was sent %d requests, want 1", len(requests))
	}
	posted := requests[0]
	if posted.method != http.MethodPost || posted.path != "/api/v1/events" {
		t.Errorf("request = %s %s, want POST /api/v1/events", posted.method, posted.path)
	}
	if want := "[" + strings.Join(testEvents, ",") + "]"; string(posted.body) != want {
		t.Errorf("body = %s, want the stored bytes in id order as %s", posted.body, want)
	}
	if got, want := posted.header.Get("Authorization"), "Bearer "+testToken; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got := posted.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q", got, "application/json")
	}
}

func TestSenderDeletesTheBatchAfterA200(t *testing.T) {
	box := newOutbox(t)
	for _, event := range testEvents {
		insertEvent(t, box, event)
	}
	server := newIngestServer(t, accepted(len(testEvents)))
	st := newSenderTest(t, box, server.URL, 500, 1)

	st.run(t)

	if got := bufferedEvents(t, box); got != 0 {
		t.Errorf("%d events are still buffered, want the delivered batch deleted", got)
	}
	if got, want := testutil.ToFloat64(st.metrics.delivered), float64(len(testEvents)); got != want {
		t.Errorf("tally_collector_delivered_total = %v, want %v", got, want)
	}
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 0 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want 0", got)
	}
	// A delivered batch that did not fill up is followed by the flush interval,
	// which is also what says the loop is at its shortest wait and took no backoff.
	if waits := st.sleeper.recorded(); len(waits) != 1 || waits[0] != testFlushInterval {
		t.Errorf("waits = %v, want the one flush interval of %v", waits, testFlushInterval)
	}
}

// TestSenderLogsARefusedEventAndDeletesTheBatchAnyway covers the per-item
// rejects inside a 200: the Reporting API keeps them dead-lettered, so the
// collector logs them and offers none of them again.
func TestSenderLogsARefusedEventAndDeletesTheBatchAnyway(t *testing.T) {
	box := newOutbox(t)
	for _, event := range testEvents {
		insertEvent(t, box, event)
	}
	server := newIngestServer(t, answers(http.StatusOK, `{"accepted":2,"duplicates":0,`+
		`"rejected":[{"index":1,"event_id":"b","reason":"size_schema: 'size_gb' is a required property"}]}`))
	// Two waits, so a loop that offered the refused event again would have the
	// round to do it in.
	st := newSenderTest(t, box, server.URL, 500, 2)

	st.run(t)

	warning := st.logs.only(t, slog.LevelWarn)
	for attr, want := range map[string]any{
		"index":    int64(1),
		"event_id": "b",
		"reason":   "size_schema: 'size_gb' is a required property",
	} {
		if got := warning.attrs[attr]; got != want {
			t.Errorf("the warning's %s = %v, want %v", attr, got, want)
		}
	}
	if got := bufferedEvents(t, box); got != 0 {
		t.Errorf("%d events are still buffered, want the whole batch deleted", got)
	}
	if requests := server.received(); len(requests) != 1 {
		t.Errorf("the server was sent %d requests, want the one batch and no retry", len(requests))
	}
	// The counter follows the answer and not the batch: a dead-lettered item was
	// posted but not accepted, and a counter that said otherwise would report full
	// throughput while every event of every batch is being refused.
	if got := testutil.ToFloat64(st.metrics.delivered); got != 2 {
		t.Errorf("tally_collector_delivered_total = %v, want the 2 events the answer accepted", got)
	}
}

// TestSenderBoundsWhatA200MayCost covers the answer of a destination that is
// not the Reporting API: what is decoded and what is logged are bounded by the
// batch that was sent rather than by what the answer claims.
func TestSenderBoundsWhatA200MayCost(t *testing.T) {
	// A 200 whose body outgrows the bound is cut off mid-document and decodes to
	// nothing, which leaves the batch buffered the way every unreadable 200 does.
	t.Run("a result past the body bound is not decoded", func(t *testing.T) {
		box := newOutbox(t)
		insertEvent(t, box, testEvents[0])
		flood := `{"accepted":1,"duplicates":0,"rejected":[` +
			strings.Repeat(`{"index":0,"event_id":"a","reason":"x"},`, resultBodyMax/40+1) + `]}`
		server := newIngestServer(t, answers(http.StatusOK, flood))
		st := newSenderTest(t, box, server.URL, 500, 1)

		st.run(t)

		logged := st.logs.only(t, slog.LevelError).errorAttr(t)
		if !strings.Contains(logged.Error(), "decoding the ingest result") {
			t.Errorf("the logged error = %v, want the truncated result to fail the decode", logged)
		}
		if got := bufferedEvents(t, box); got != 1 {
			t.Errorf("%d events are buffered, want the batch kept", got)
		}
	})

	// An answer cannot refuse more items than the batch carried, so a destination
	// that claims otherwise gets no more log lines than there were events.
	t.Run("more rejections than events are logged once per event", func(t *testing.T) {
		box := newOutbox(t)
		insertEvent(t, box, testEvents[0])
		server := newIngestServer(t, answers(http.StatusOK, `{"accepted":0,"duplicates":0,"rejected":[`+
			`{"index":0,"event_id":"a","reason":"one"},`+
			`{"index":1,"event_id":"b","reason":"two"},`+
			`{"index":2,"event_id":"c","reason":"three"}]}`))
		st := newSenderTest(t, box, server.URL, 500, 1)

		st.run(t)

		if warnings := st.logs.at(slog.LevelWarn); len(warnings) != 1 {
			t.Errorf("%d warnings logged, want one per posted event: %v", len(warnings), warnings)
		}
	})
}

// TestSenderShrinksABatchTheAPIRefusedAsTooLarge covers the one status a plain
// retry cannot survive: a body past what the API accepts is past it on every
// attempt, so the next round offers half of it instead of the same one.
func TestSenderShrinksABatchTheAPIRefusedAsTooLarge(t *testing.T) {
	box := newOutbox(t)
	for _, event := range []string{
		`{"event_id":"a"}`, `{"event_id":"b"}`, `{"event_id":"c"}`, `{"event_id":"d"}`,
	} {
		insertEvent(t, box, event)
	}
	server := newIngestServer(t, answersInTurn(
		answers(http.StatusRequestEntityTooLarge, `{"title":"the body is too large"}`),
		accepted(2),
	))
	st := newSenderTest(t, box, server.URL, 4, 2)

	st.run(t)

	requests := server.received()
	if len(requests) != 3 {
		t.Fatalf("the server was sent %d requests, want the refused batch of 4 and the two halves",
			len(requests))
	}
	for i, want := range []int{4, 2, 2} {
		if got := strings.Count(string(requests[i].body), "event_id"); got != want {
			t.Errorf("request %d carried %d events, want %d", i, got, want)
		}
	}
	if got := bufferedEvents(t, box); got != 0 {
		t.Errorf("%d events are still buffered, want the halves delivered", got)
	}
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 1 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want the one refusal", got)
	}
}

// TestSenderDropsASingleEventTheAPIRefusesAsTooLarge is the end of the halving:
// one event past the bound the consumer buffered it under is refused at every
// batch size, and keeping it would hold up every event behind it for as long as
// the collector runs.
func TestSenderDropsASingleEventTheAPIRefusesAsTooLarge(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, oversizedEvent("oversized"))
	insertEvent(t, box, testEvents[1])
	server := newIngestServer(t, answersInTurn(
		answers(http.StatusRequestEntityTooLarge, `{"title":"the body is too large"}`),
		accepted(1),
	))
	// One event per batch, so the refused batch is already the smallest one.
	st := newSenderTest(t, box, server.URL, 1, 1)

	st.run(t)

	dropped := st.logs.only(t, slog.LevelError)
	if got := dropped.attrs["event_id"]; got != "oversized" {
		t.Errorf("the error names event_id %v, want the dropped event", got)
	}
	// The event behind it is delivered in the same run, which is the point of
	// dropping the one in front.
	if got := bufferedEvents(t, box); got != 0 {
		t.Errorf("%d events are buffered, want the dropped one gone and the next one delivered", got)
	}
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 1 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want the refusal counted", got)
	}
	if got := testutil.ToFloat64(st.metrics.delivered); got != 1 {
		t.Errorf("tally_collector_delivered_total = %v, want only the event that was accepted", got)
	}
}

// TestSenderKeepsASingleEventUnderTheBoundThatIsRefusedAsTooLarge is the other
// half of the drop above, and the reason it is decided on the event's size and
// not on the status alone.
//
// The consumer caps every event it buffers at eventMax, so an event under that
// bound is one a correctly configured Reporting API accepts. A 413 for it comes
// from something in front of the API — an ingress with a small
// client_max_body_size, a WAF rule — and answering that by deleting the event
// would walk the halving ladder down to one and destroy the entire outbox one
// event at a time, permanently: the rows are gone from SQLite and the broker
// has long acknowledged the notifications.
func TestSenderKeepsASingleEventUnderTheBoundThatIsRefusedAsTooLarge(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, testEvents[0])
	server := newIngestServer(t, answers(http.StatusRequestEntityTooLarge,
		`{"title":"the body is too large"}`))
	// One event per batch, so the refused batch is already the smallest one, and
	// two rounds so a loop that dropped it would have emptied the buffer by the
	// second.
	st := newSenderTest(t, box, server.URL, 1, 2)

	st.run(t)

	if got := bufferedEvents(t, box); got != 1 {
		t.Errorf("%d events are buffered, want the event kept for a retry", got)
	}
	// Every refusal is retried visibly, the way a misconfigured token is, rather
	// than draining the buffer into nowhere.
	logged := st.logs.at(slog.LevelError)
	if len(logged) != 2 {
		t.Fatalf("%d errors logged, want one per refused round: %v", len(logged), logged)
	}
	for i, record := range logged {
		if record.message != "delivering a batch failed, keeping it buffered" {
			t.Errorf("error %d = %q, want the batch kept and not dropped", i, record.message)
		}
	}
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 2 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want one per round", got)
	}
}

// TestSenderSurvivesAnAnswerThatCountsMoreOrFewerThanTheBatch covers the one
// number in the answer the sender arithmetically depends on. It is decoded
// straight out of the response body, so it is decided by whoever answers at
// TALLY_OSC_REPORTING_URL, and prometheus.Counter.Add panics on a negative
// value. Nothing in the collector binary recovers, and the panic fires before
// the batch is deleted, so the same batch would be read, posted, and panicked
// on again after every restart.
func TestSenderSurvivesAnAnswerThatCountsMoreOrFewerThanTheBatch(t *testing.T) {
	tests := []struct {
		name     string
		accepted string
		want     float64
	}{
		{name: "a negative count", accepted: "-1", want: 0},
		{name: "a count past the batch", accepted: "5000", want: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			box := newOutbox(t)
			insertEvent(t, box, testEvents[0])
			server := newIngestServer(t, answers(http.StatusOK,
				`{"accepted":`+tc.accepted+`,"duplicates":0,"rejected":[]}`))
			st := newSenderTest(t, box, server.URL, 500, 1)

			st.run(t)

			if got := testutil.ToFloat64(st.metrics.delivered); got != tc.want {
				t.Errorf("tally_collector_delivered_total = %v, want %v bounded by the batch", got, tc.want)
			}
			// The answer was a 200, so the batch is gone either way: what the count
			// decides is the counter and not the buffer.
			if got := bufferedEvents(t, box); got != 0 {
				t.Errorf("%d events are buffered, want the delivered batch deleted", got)
			}
		})
	}
}

// TestSenderSettlesOnABatchSizeTheDestinationAccepts covers the steady state of
// a destination whose body limit the configured batch never fits. A limit that
// kept doubling past a size the destination has already refused would be refused
// there again on every second round, forever: each of those rounds costs an
// uploaded oversized body, a counted delivery error, and a backoff wait, so the
// collector would deliver a fraction of what the cloud emits, the outbox would
// fill from a body-size limit a stable batch size absorbs, and the permanent
// error rate would drown out the signal of a real outage.
func TestSenderSettlesOnABatchSizeTheDestinationAccepts(t *testing.T) {
	box := newOutbox(t)
	for i := range 12 {
		insertEvent(t, box, fmt.Sprintf(`{"event_id":"e%d"}`, i))
	}
	// Anything past two events is refused, which is the ingress the fix is about.
	server := newIngestServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		events := strings.Count(string(body), "event_id")
		if events > 2 {
			answers(http.StatusRequestEntityTooLarge, `{"title":"the body is too large"}`)(w, r)
			return
		}
		accepted(events)(w, r)
	})
	st := newSenderTest(t, box, server.URL, 8, 5)

	st.run(t)

	var sizes []int
	for _, request := range server.received() {
		sizes = append(sizes, strings.Count(string(request.body), "event_id"))
	}
	// 8 is refused and halved to 4, which is refused and halved to 2, which gets
	// through. The doubling then probes 3, one below the smallest refused size,
	// and that refusal drops the ceiling to 2 for good: every round after it posts
	// the size the destination takes, and none of them is refused. The last
	// request is short because the buffer ran out, not because the limit did.
	if want := []int{8, 4, 2, 3, 1, 2, 2, 2, 2, 1}; !slices.Equal(sizes, want) {
		t.Errorf("posted batch sizes = %v, want %v", sizes, want)
	}
	if got := bufferedEvents(t, box); got != 0 {
		t.Errorf("%d events are still buffered, want them all delivered", got)
	}
	// The refusals are the three the search costs and not one per batch: what the
	// count says about a settled sender is that the destination is fine.
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 3 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want the three refusals of the search", got)
	}
}

// TestSenderWaitsTheFlushIntervalWhenTheOutboxCannotBeRead covers the buffer
// side of a failure: a batch that could not be read is not a destination that
// refused one, so it costs the flush interval and no delivery error.
func TestSenderWaitsTheFlushIntervalWhenTheOutboxCannotBeRead(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, testEvents[0])
	server := newIngestServer(t, accepted(1))
	st := newSenderTest(t, &failingBatches{inner: box, failBatch: true}, server.URL, 500, 1)

	st.run(t)

	if requests := server.received(); len(requests) != 0 {
		t.Errorf("the server was sent %d requests, want none while the buffer cannot be read", len(requests))
	}
	logged := st.logs.only(t, slog.LevelError).errorAttr(t)
	if !strings.Contains(logged.Error(), "unreadable") {
		t.Errorf("the logged error = %v, want the failed read", logged)
	}
	if waits := st.sleeper.recorded(); len(waits) != 1 || waits[0] != testFlushInterval {
		t.Errorf("waits = %v, want the one flush interval of %v", waits, testFlushInterval)
	}
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 0 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want 0 for a buffer failure", got)
	}
}

// TestSenderBacksOffWhenTheDeliveredBatchCannotBeDeleted is what keeps a full
// volume from turning the sender into a POST loop: the rows stay, so the next
// round reads the same ones, and without the backoff it would repost them as
// fast as the API answers.
func TestSenderBacksOffWhenTheDeliveredBatchCannotBeDeleted(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, testEvents[0])
	server := newIngestServer(t, accepted(1))
	st := newSenderTest(t, &failingBatches{inner: box, failDelete: true}, server.URL, 500, 2)

	st.run(t)

	logged := st.logs.at(slog.LevelError)
	if len(logged) != 2 {
		t.Fatalf("%d errors logged, want one per round: %v", len(logged), logged)
	}
	if err := logged[0].errorAttr(t); !strings.Contains(err.Error(), "not writable") {
		t.Errorf("the logged error = %v, want the failed delete", err)
	}
	// Both waits are backoffs and neither is the flush interval, which is what the
	// full-batch shortcut would have skipped altogether.
	for i, wait := range st.sleeper.recorded() {
		if wait < time.Second || wait >= 4*time.Second {
			t.Errorf("wait %d = %v, want a delivery backoff", i, wait)
		}
	}
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 2 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want one per round", got)
	}
	if got := bufferedEvents(t, box); got != 1 {
		t.Errorf("%d events are buffered, want the undeleted event kept", got)
	}
}

// TestSenderDrainsAFullBatchWithoutWaiting is what keeps a backlog from draining
// at one batch per flush interval.
func TestSenderDrainsAFullBatchWithoutWaiting(t *testing.T) {
	box := newOutbox(t)
	for _, event := range []string{
		`{"event_id":"a"}`, `{"event_id":"b"}`, `{"event_id":"c"}`, `{"event_id":"d"}`,
	} {
		insertEvent(t, box, event)
	}
	server := newIngestServer(t, accepted(2))
	st := newSenderTest(t, box, server.URL, 2, 1)

	st.run(t)

	if requests := server.received(); len(requests) != 2 {
		t.Fatalf("the server was sent %d requests, want the 2 full batches", len(requests))
	}
	// The only wait is the one the emptied buffer earned: the two full batches
	// went out back to back.
	if waits := st.sleeper.recorded(); len(waits) != 1 || waits[0] != testFlushInterval {
		t.Errorf("waits = %v, want no wait between the two full batches", waits)
	}
	if got := bufferedEvents(t, box); got != 0 {
		t.Errorf("%d events are still buffered, want an empty outbox", got)
	}
}

// TestSenderKeepsTheBatchWhenTheResultDoesNotDecode covers a 200 the sender
// cannot read: what it covered is unknown, so the batch stays and the retry is
// deduplicated by event_id.
func TestSenderKeepsTheBatchWhenTheResultDoesNotDecode(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, testEvents[0])
	server := newIngestServer(t, answers(http.StatusOK, "not json at all"))
	st := newSenderTest(t, box, server.URL, 500, 1)

	st.run(t)

	logged := st.logs.only(t, slog.LevelError).errorAttr(t)
	if !strings.Contains(logged.Error(), "decoding the ingest result") {
		t.Errorf("the logged error = %v, want it to name the unreadable result", logged)
	}
	if got := bufferedEvents(t, box); got != 1 {
		t.Errorf("%d events are buffered, want the batch kept", got)
	}
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 1 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(st.metrics.delivered); got != 0 {
		t.Errorf("tally_collector_delivered_total = %v, want 0", got)
	}
	if waits := st.sleeper.recorded(); len(waits) != 1 || waits[0] < time.Second || waits[0] >= 2*time.Second {
		t.Errorf("waits = %v, want the one backoff of at least a second", waits)
	}
}

func TestSenderKeepsTheBatchOnEveryRefusedStatus(t *testing.T) {
	tests := []struct {
		status int
		body   string
	}{
		{status: http.StatusBadRequest, body: `{"title":"the batch is not an array"}`},
		{status: http.StatusUnauthorized, body: `{"title":"the token is unknown"}`},
		{status: http.StatusInternalServerError, body: "the database is unreachable"},
	}

	for _, tc := range tests {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			box := newOutbox(t)
			insertEvent(t, box, testEvents[0])
			server := newIngestServer(t, answers(tc.status, tc.body))
			st := newSenderTest(t, box, server.URL, 500, 1)

			st.run(t)

			logged := st.logs.only(t, slog.LevelError).errorAttr(t).Error()
			if !strings.Contains(logged, strconv.Itoa(tc.status)) || !strings.Contains(logged, tc.body) {
				t.Errorf("the logged error = %q, want the status %d and the answer %q", logged, tc.status, tc.body)
			}
			if got := bufferedEvents(t, box); got != 1 {
				t.Errorf("%d events are buffered, want the refused batch kept", got)
			}
			if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 1 {
				t.Errorf("tally_collector_delivery_errors_total = %v, want 1", got)
			}
			if got := testutil.ToFloat64(st.metrics.delivered); got != 0 {
				t.Errorf("tally_collector_delivered_total = %v, want 0", got)
			}
		})
	}
}

// TestSenderLogsAtMostTheFirstBytesOfARefusal bounds what a destination can put
// into the collector's log: the answer may be a proxy's error page rather than
// the API's own few hundred bytes of JSON.
func TestSenderLogsAtMostTheFirstBytesOfARefusal(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, testEvents[0])
	server := newIngestServer(t, answers(http.StatusBadGateway, strings.Repeat("a", errorBodyMax+88)))
	st := newSenderTest(t, box, server.URL, 500, 1)

	st.run(t)

	logged := st.logs.only(t, slog.LevelError).errorAttr(t).Error()
	if !strings.Contains(logged, strings.Repeat("a", errorBodyMax)) {
		t.Errorf("the logged error = %q, want the first %d bytes of the answer", logged, errorBodyMax)
	}
	if strings.Contains(logged, strings.Repeat("a", errorBodyMax+1)) {
		t.Errorf("the logged error carries more than %d bytes of the answer: %q", errorBodyMax, logged)
	}
}

func TestSenderKeepsTheBatchWhenTheConnectionIsRefused(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, testEvents[0])
	// A server that was started and closed again leaves an address nothing
	// listens on, which is what an unreachable Reporting API looks like.
	unreachable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachable.Close()
	st := newSenderTest(t, box, unreachable.URL, 500, 1)

	st.run(t)

	if got := bufferedEvents(t, box); got != 1 {
		t.Errorf("%d events are buffered, want the undelivered batch kept", got)
	}
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 1 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want 1", got)
	}
	if logged := st.logs.only(t, slog.LevelError).errorAttr(t); logged == nil {
		t.Error("the failure was logged without the transport error")
	}
}

// TestSenderKeepsTheBatchWhenTheRequestTimesOut covers the destination that
// takes the connection and then stops answering: the client's timeout is what
// gets the batch back, and it comes back buffered.
func TestSenderKeepsTheBatchWhenTheRequestTimesOut(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, testEvents[0])
	stalled := make(chan struct{})
	server := newIngestServer(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-stalled:
		case <-r.Context().Done():
		}
	})
	// Registered after the server's own cleanup, so the handler is released
	// before the server is closed.
	t.Cleanup(func() { close(stalled) })
	st := newSenderTest(t, box, server.URL, 500, 1)
	st.sender.client.Timeout = 50 * time.Millisecond

	st.run(t)

	logged := st.logs.only(t, slog.LevelError).errorAttr(t)
	if !errors.Is(logged, context.DeadlineExceeded) {
		t.Errorf("the logged error = %v, want the client's timeout to surface as a deadline", logged)
	}
	if got := bufferedEvents(t, box); got != 1 {
		t.Errorf("%d events are buffered, want the undelivered batch kept", got)
	}
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 1 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want 1", got)
	}
}

// TestSenderBackoffGrowsWithEveryFailureAndResetsAfterA200 walks the wait a
// failing destination earns: a second, doubling, plus up to a second of jitter,
// and back to a second once a batch got through.
func TestSenderBackoffGrowsWithEveryFailureAndResetsAfterA200(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, testEvents[0])
	insertEvent(t, box, testEvents[1])
	server := newIngestServer(t, answersInTurn(
		answers(http.StatusInternalServerError, "down"),
		answers(http.StatusInternalServerError, "down"),
		answers(http.StatusInternalServerError, "down"),
		accepted(1),
		answers(http.StatusInternalServerError, "down"),
	))
	// One event per batch, so every batch is a full one: the 200 is followed by
	// the next POST without a flush interval in between, and the wait that POST
	// earns is the backoff the 200 reset.
	st := newSenderTest(t, box, server.URL, 1, 4)

	st.run(t)

	waits := st.sleeper.recorded()
	if len(waits) != 4 {
		t.Fatalf("waits = %v, want 4", waits)
	}
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, time.Second} {
		if waits[i] < want || waits[i] >= want+deliveryJitter {
			t.Errorf("wait %d = %v, want it in [%v, %v)", i, waits[i], want, want+deliveryJitter)
		}
	}
}

// TestSenderCapsTheBackoff keeps a destination that stays down from being asked
// ever more rarely: the doubling stops at five minutes.
func TestSenderCapsTheBackoff(t *testing.T) {
	box := newOutbox(t)
	insertEvent(t, box, testEvents[0])
	server := newIngestServer(t, answers(http.StatusServiceUnavailable, "down"))
	// The doubling reaches the cap on the tenth wait, so twelve of them show the
	// growth stopping there and staying there.
	st := newSenderTest(t, box, server.URL, 500, 12)

	st.run(t)

	waits := st.sleeper.recorded()
	if len(waits) != 12 {
		t.Fatalf("waits = %v, want 12", waits)
	}
	for i, wait := range waits {
		if wait >= maxDeliveryBackoff+deliveryJitter {
			t.Errorf("wait %d = %v, want no wait past %v", i, wait, maxDeliveryBackoff+deliveryJitter)
		}
	}
	if last := waits[len(waits)-1]; last < maxDeliveryBackoff {
		t.Errorf("the last wait = %v, want the capped %v", last, maxDeliveryBackoff)
	}
}

// TestSenderPollsAnEmptyOutboxWithoutPosting covers the state a quiet collector
// spends its time in: nothing buffered is nothing to post.
func TestSenderPollsAnEmptyOutboxWithoutPosting(t *testing.T) {
	box := newOutbox(t)
	server := newIngestServer(t, accepted(0))
	st := newSenderTest(t, box, server.URL, 500, 3)

	st.run(t)

	if requests := server.received(); len(requests) != 0 {
		t.Errorf("the server was sent %d requests, want none for an empty buffer", len(requests))
	}
	waits := st.sleeper.recorded()
	if len(waits) != 3 {
		t.Fatalf("waits = %v, want 3", waits)
	}
	for i, wait := range waits {
		if wait != testFlushInterval {
			t.Errorf("wait %d = %v, want the flush interval of %v", i, wait, testFlushInterval)
		}
	}
	if got := testutil.ToFloat64(st.metrics.deliveryErrors); got != 0 {
		t.Errorf("tally_collector_delivery_errors_total = %v, want 0", got)
	}
}

// TestIngestResultMirrorsTheSpec ties the hand-written client type to the
// contract it mirrors. Mirroring rather than importing the server's type is
// deliberate, because the collector runs against a Tally this repository knows
// nothing about, but the decode is lenient: a member renamed on the server side
// would still compile, still delete the batch, and log a refusal with an empty
// event id and an empty reason, which is the pointer into the dead-letter view
// an operator has and nothing else would report its loss.
func TestIngestResultMirrorsTheSpec(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Required []string `yaml:"required"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "reporting", "openapi.yaml"))
	if err != nil {
		t.Fatalf("reading the Reporting API specification: %v", err)
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("decoding the Reporting API specification: %v", err)
	}

	tests := []struct {
		schema string
		mirror reflect.Type
	}{
		{schema: "IngestResult", mirror: reflect.TypeFor[ingestResult]()},
		{schema: "RejectedEvent", mirror: reflect.TypeFor[rejectedEvent]()},
	}

	for _, tc := range tests {
		t.Run(tc.schema, func(t *testing.T) {
			want := slices.Clone(spec.Components.Schemas[tc.schema].Required)
			if len(want) == 0 {
				t.Fatalf("the specification defines no required members for %s", tc.schema)
			}
			got := make([]string, 0, tc.mirror.NumField())
			for i := range tc.mirror.NumField() {
				tag, _, _ := strings.Cut(tc.mirror.Field(i).Tag.Get("json"), ",")
				got = append(got, tag)
			}

			slices.Sort(want)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("%s reads %v, want the specification's %v", tc.mirror, got, want)
			}
		})
	}
}

func TestSenderStopsWhenTheContextIsDone(t *testing.T) {
	// A shutdown that arrives while the loop is waiting must not have to wait the
	// rest of that wait out.
	t.Run("cancelled during a wait", func(t *testing.T) {
		box := newOutbox(t)
		server := newIngestServer(t, accepted(0))
		st := newSenderTest(t, box, server.URL, 500, 1)

		st.run(t)

		if waits := st.sleeper.recorded(); len(waits) != 1 {
			t.Errorf("waits = %v, want the loop to end on the first one", waits)
		}
	})

	// A context that is already done is checked before the buffer is read, so a
	// collector shutting down posts nothing more.
	t.Run("cancelled before the first round", func(t *testing.T) {
		box := newOutbox(t)
		insertEvent(t, box, testEvents[0])
		server := newIngestServer(t, accepted(1))
		st := newSenderTest(t, box, server.URL, 500, 1)
		st.cancel()

		st.run(t)

		if requests := server.received(); len(requests) != 0 {
			t.Errorf("the server was sent %d requests, want none once the context is done", len(requests))
		}
		if waits := st.sleeper.recorded(); len(waits) != 0 {
			t.Errorf("waits = %v, want none", waits)
		}
		if got := bufferedEvents(t, box); got != 1 {
			t.Errorf("%d events are buffered, want the undelivered event kept", got)
		}
	})
}

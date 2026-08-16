package openstack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The shape of the outage the durability test drives: how many events the
// collector buffers before its restart and how many after it, how many of them
// one POST carries, and how many refusals it waits for before it looks at the
// buffer. The batch is far smaller than the backlog, so the recovery empties the
// outbox as a series of batches and a half-delivered one would leave a gap in
// the received set.
const (
	eventsBeforeRestart = 20
	eventsAfterRestart  = 5
	durabilityBatchMax  = 5
	refusalsWhileDown   = 3
)

// senderRetryWait is what the sender's wait is cut to. The loop asks for a
// second and more after a refusal, and every phase here is over in milliseconds.
const senderRetryWait = 20 * time.Millisecond

// dedupingAPI is the Reporting API of the exit criterion: it can be taken down
// and brought back up under a running collector, and while it is up it
// deduplicates on event_id the way the server's ingest does. What it received
// outlives the outage, so a batch that is posted again after a failure is
// answered as a duplicate rather than counted as a second receipt.
type dedupingAPI struct {
	*httptest.Server
	// up is what a test flips. While it is false every POST is refused, which is
	// the state the collector has to come through without losing an event.
	up atomic.Bool
	// attempts counts the POSTs, refused ones included, so that a phase waits for
	// the sender to have tried instead of sleeping for a while.
	attempts atomic.Int64

	mu sync.Mutex
	// received maps an event id to how often it arrived. The handler writes it
	// from the server's goroutines while the test reads it, hence the lock.
	received map[string]int
}

// newDedupingAPI starts a Reporting API that is down, and closes it when the
// test ends.
func newDedupingAPI(t *testing.T) *dedupingAPI {
	t.Helper()

	api := &dedupingAPI{received: map[string]int{}}
	api.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api.attempts.Add(1)
		if !api.up.Load() {
			http.Error(w, "the Reporting API is down", http.StatusServiceUnavailable)
			return
		}

		var batch []struct {
			EventID string `json:"event_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("decoding a posted batch: %v", err)
			http.Error(w, "the batch is not an array of events", http.StatusBadRequest)
			return
		}

		firstSeen, repeats := 0, 0
		api.mu.Lock()
		for _, item := range batch {
			api.received[item.EventID]++
			if api.received[item.EventID] == 1 {
				firstSeen++
			} else {
				repeats++
			}
		}
		api.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"accepted":%d,"duplicates":%d,"rejected":[]}`, firstSeen, repeats)
	}))
	t.Cleanup(api.Close)
	return api
}

// acceptedIDs is the distinct set of event ids the API has taken, sorted so that
// it compares against the produced set rather than against a delivery order.
func (a *dedupingAPI) acceptedIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	distinct := make([]string, 0, len(a.received))
	for id := range a.received {
		distinct = append(distinct, id)
	}
	slices.Sort(distinct)
	return distinct
}

// shortSleep is the sender's wait, cut to a length a test can spend on it. It
// returns on a cancelled context the way the collector's own wait does, which is
// what ends the loop.
func shortSleep(ctx context.Context, _ time.Duration) error {
	return sleep(ctx, senderRetryWait)
}

// startSender runs a sender over box until the returned stop is called, and
// joins its goroutine there, so that no phase reads a buffer a running loop is
// still changing. The wait is replaced rather than shortened through the backoff
// bounds: what a phase needs is the round after a refusal, not the growth of the
// pause before it.
func startSender(t *testing.T, box *Outbox, reportingURL string, batchMax int) func() {
	t.Helper()

	sender := NewSender(box, reportingURL, testToken, batchMax, testFlushInterval, nil, testLogger(t))
	sender.sleep = shortSleep

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := sender.Run(ctx); err != nil {
			t.Errorf("Run() error = %v, want nil", err)
		}
	}()

	stop := sync.OnceFunc(func() {
		cancel()
		<-done
	})
	t.Cleanup(stop)
	return stop
}

// produceEvent maps one nova instance creation and buffers the event it becomes,
// the way the consumer does, and returns the event id it carries. The index
// varies the oslo message id, which is what the mapping books as the event id
// and what the Reporting API deduplicates on.
func produceEvent(t *testing.T, box *Outbox, index int) string {
	t.Helper()

	notification, err := ParseEnvelope(instanceNotification(t,
		fmt.Sprintf("7a1e0b2c-3d4f-4a5b-8c6d-%012d", index)))
	if err != nil {
		t.Fatalf("ParseEnvelope() error = %v, want nil", err)
	}
	mapped, ok := MapNotification(notification, goldenCloud)
	if !ok {
		t.Fatal("MapNotification() ok = false, want the instance creation mapped")
	}
	eventJSON, err := json.Marshal(mapped)
	if err != nil {
		t.Fatalf("encoding the mapped event: %v", err)
	}

	insertEvent(t, box, string(eventJSON))
	return mapped.EventID
}

// TestDurabilityAcrossAnAPIOutageAndARestart is the exit criterion of the
// collector in one scenario: with the Reporting API down for a stretch and the
// collector restarted while its buffer is full, nothing produced is lost and
// nothing is booked twice.
//
// Each phase pins one link of the chain that carries it. The outage pins that a
// refused batch stays buffered, because only a 200 deletes one. The restart pins
// that the buffer is the file and not the process. The recovery pins that the
// backlog leaves once the destination answers again. The set the API ends up
// with is the property the three add up to: it is the produced set exactly,
// because a retried event is deduplicated by its event_id instead of being
// stored a second time.
func TestDurabilityAcrossAnAPIOutageAndARestart(t *testing.T) {
	api := newDedupingAPI(t)
	path := filepath.Join(t.TempDir(), "outbox.db")
	box := openOutboxAt(t, path)

	// Under load with the destination down: the consumer's promise ends at the
	// buffer, so the outage is the sender's problem alone and production goes on.
	want := make([]string, 0, eventsBeforeRestart+eventsAfterRestart)
	for i := range eventsBeforeRestart {
		want = append(want, produceEvent(t, box, i))
	}

	stop := startSender(t, box, api.URL, durabilityBatchMax)
	waitFor(t, "the sender has been refused a few times", func() bool {
		return api.attempts.Load() >= refusalsWhileDown
	})

	// A non-200 deletes nothing. Everything produced is still buffered after
	// several refused rounds, and the API has taken none of it.
	if got := box.Depth(); got != eventsBeforeRestart {
		t.Fatalf("Depth() = %d while the API is down, want the %d produced events",
			got, eventsBeforeRestart)
	}
	if got := len(readBatch(t, box, 500)); got != eventsBeforeRestart {
		t.Fatalf("%d events are buffered, want the %d refused ones kept", got, eventsBeforeRestart)
	}
	if got := api.acceptedIDs(); len(got) != 0 {
		t.Fatalf("the API took %v, want nothing while it is down", got)
	}

	// The restart, taken mid-buffer: the process ends and the file it leaves
	// behind is what the collector replacing it picks up.
	stop()
	if err := box.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	restarted := openOutboxAt(t, path)
	if got := restarted.Depth(); got != eventsBeforeRestart {
		t.Fatalf("Depth() = %d after the restart, want the %d events the file holds",
			got, eventsBeforeRestart)
	}

	// The restarted collector consumes again while the API is still down, so the
	// buffer keeps growing on top of what it inherited.
	for i := range eventsAfterRestart {
		want = append(want, produceEvent(t, restarted, eventsBeforeRestart+i))
	}

	// Recovery: the destination answers again and the whole backlog drains,
	// batch by batch, without anything else happening in between.
	startSender(t, restarted, api.URL, durabilityBatchMax)
	api.up.Store(true)
	waitFor(t, "the backlog is delivered and the outbox is empty", func() bool {
		return restarted.Depth() == 0 && len(readBatch(t, restarted, 500)) == 0
	})

	// Zero loss and zero duplicates, as one comparison: the distinct set the API
	// received is the set the collector produced, with nothing missing from it and
	// nothing in it that was never produced. A batch that was posted twice is
	// absorbed by the event_id deduplication and adds no member.
	slices.Sort(want)
	if got := api.acceptedIDs(); !slices.Equal(got, want) {
		t.Errorf("the API received %d distinct event ids, want the %d produced\ngot:  %v\nwant: %v",
			len(got), len(want), got, want)
	}
}

package openstack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"time"
)

// eventsPath is the ingest route, appended to the configured base URL of the
// Reporting API.
const eventsPath = "/api/v1/events"

// The bounds one delivery attempt and the wait after a failed one are taken
// under.
const (
	// The bounds of the wait between two delivery attempts. The first is short
	// enough that a Reporting API restart costs a second, the cap keeps an API
	// that stays down from being posted to in a loop.
	minDeliveryBackoff = time.Second
	maxDeliveryBackoff = 300 * time.Second
	// deliveryJitter is the span added on top of every wait, uniformly. It
	// spreads the retries of a fleet of collectors that one Reporting API outage
	// put into backoff in the same instant.
	deliveryJitter = time.Second
	// deliveryTimeout bounds one POST. A destination that accepts the connection
	// and then stops answering would otherwise hold the batch for as long as it
	// stays that way, while the buffer behind it keeps growing.
	deliveryTimeout = 30 * time.Second
	// errorBodyMax is how much of a refused answer's body is logged. What arrives
	// there is whatever the destination produced, which may be a proxy's HTML
	// error page rather than the API's own JSON.
	errorBodyMax = 512
	// resultBodyMax bounds the 200 answer that is decoded. A rejection entry is a
	// few hundred bytes and a batch is at most a thousand items, so a megabyte
	// covers every answer the Reporting API can give. What it rules out is a
	// destination that answers 200 and then streams for as long as the delivery
	// timeout allows, which the decoder would otherwise hold in memory whole.
	resultBodyMax = 1 << 20
)

// errTooLarge marks the answer of a destination that refused the batch for its
// size. Run offers a smaller one next, because the same batch would be refused
// again for as long as it is offered.
var errTooLarge = errors.New("the batch is too large for the Reporting API")

// Sender empties the outbox into the Reporting API. It runs independently of the
// consumer: the consumer's promise ends at the buffer, and this is what carries
// the buffered events the rest of the way.
//
// One rule decides a batch's fate, with the single exception named below. A 200
// deletes it and nothing else does: every other status, and every error that
// keeps a status from arriving at all, leaves the batch buffered and offers it
// again after a wait. Resending costs nothing, because the event carries the
// oslo message id as its event_id and the API deduplicates on it, so a needless
// retry is answered as a duplicate. Deleting on anything but a 200 would cost
// usage nobody can reconstruct, which is why a misconfigured token retries
// visibly until an operator fixes it rather than draining the buffer into
// nowhere.
//
// The items a 200 refused are not a failure of the batch. The Reporting API
// keeps a refused item dead-lettered on its side, so offering it again would
// only have it refused again: the sender logs each one and deletes the batch
// with the rest of it.
//
// A 413 is the one status the rule above cannot answer with a plain retry: a
// body past what the API accepts is past it on every attempt. The batch is
// halved until it fits, and the size that worked is grown back gradually but
// never past the smallest size that was refused, so a destination with a smaller
// body bound than the configured batch settles on one that fits instead of
// walking the ladder again after every batch.
//
// A single event that is still refused is dropped only when it is past eventMax,
// which is the bound the consumer buffered it under. Below that bound the 413
// describes the destination and not the event, so the event is kept and retried
// like every other refusal: an ingress with a small body limit would otherwise
// destroy the whole outbox one event at a time, and those events are gone from
// SQLite and already acknowledged on the bus.
//
// The retry lives in this loop rather than in the HTTP client. What is retried
// is the batch and not the request: the wait belongs to the outbox, which holds
// the events meanwhile, and the attempt that follows reads the buffer again
// instead of resending a body kept in memory.
type Sender struct {
	outbox   eventBatches
	endpoint string
	token    string
	batchMax int
	flush    time.Duration
	metrics  *Metrics
	logger   *slog.Logger

	// The client and the wait, as fields rather than constants at their call
	// sites, so that the tests can shorten them. Nothing outside this package
	// sets them.
	client *http.Client
	sleep  func(context.Context, time.Duration) error
}

// eventBatches is what the sender needs of the outbox: read the oldest buffered
// events, and remove the ones the Reporting API took. *Outbox satisfies it. The
// sender takes the interface rather than the type so that a test can fail a
// read or a delete the way a full volume does.
type eventBatches interface {
	Batch(ctx context.Context, limit int) ([]Row, error)
	DeleteBatch(ctx context.Context, ids []int64) error
}

// NewSender builds the loop that posts outbox's events to reportingURL, at most
// batchMax of them per request, waiting flushInterval between the rounds that
// found nothing to send or did not fill a batch.
//
// m may be nil, which records nothing, and logger may be nil, which logs through
// the default logger.
func NewSender(outbox eventBatches, reportingURL, token string, batchMax int,
	flushInterval time.Duration, m *Metrics, logger *slog.Logger,
) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	return &Sender{
		outbox:   outbox,
		endpoint: reportingURL + eventsPath,
		token:    token,
		batchMax: batchMax,
		flush:    flushInterval,
		metrics:  m,
		logger:   logger,
		client:   &http.Client{Timeout: deliveryTimeout},
		sleep:    sleep,
	}
}

// Run delivers until ctx is done, and then returns nil, which is the only way it
// returns. A failed attempt is waited out with a growing backoff, so a Reporting
// API that is restarting costs the collector a pause rather than the process,
// and one that stays down is asked once every five minutes while the events wait
// in the outbox.
//
// A batch that filled up is followed by the next one without a wait, which is
// what lets a backlog drain at the speed the API takes it rather than at one
// batch per flush interval.
func (s *Sender) Run(ctx context.Context) error {
	backoff := minDeliveryBackoff
	// How many events the next round offers. It is the configured batch size
	// until a 413 says that many do not fit, and doubles back toward it with
	// every batch that got through.
	limit := s.batchMax
	// The smallest batch the destination refused for its size, which is the
	// ceiling the doubling grows back under. Without it the limit would double
	// past a bound the destination has already stated and be refused there again,
	// forever: every second round would cost an uploaded oversized body, a counted
	// delivery error, and a backoff wait, and the sender would deliver a fraction
	// of what the cloud emits. A destination whose bound changes is picked up on
	// the next start, which is when a batch past this ceiling is offered again.
	tooLarge := s.batchMax + 1
	for ctx.Err() == nil {
		batch, err := s.outbox.Batch(ctx, limit)
		if err != nil {
			// A buffer that cannot be read has nothing to do with the destination,
			// so this waits out the flush interval rather than the delivery backoff
			// and leaves that backoff where the last attempt left it.
			s.logger.Error("reading a batch from the outbox failed", "error", err)
			if err := s.sleep(ctx, s.flush); err != nil {
				break
			}
			continue
		}
		if len(batch) == 0 {
			if err := s.sleep(ctx, s.flush); err != nil {
				break
			}
			continue
		}

		if err := s.deliver(ctx, batch); err != nil {
			s.metrics.DeliveryError()
			// A batch refused for its size is the one failure a retry of the same
			// batch cannot survive, so the next round offers half of it. Everything
			// else is offered again unchanged.
			if errors.Is(err, errTooLarge) {
				tooLarge = len(batch)
				limit = max(1, len(batch)/2)
			}
			wait := backoff + rand.N(deliveryJitter)
			s.logger.Error("delivering a batch failed, keeping it buffered",
				"events", len(batch), "in", wait, "error", err)
			if err := s.sleep(ctx, wait); err != nil {
				break
			}
			backoff = min(2*backoff, maxDeliveryBackoff)
			continue
		}

		backoff = minDeliveryBackoff
		full := len(batch) == limit
		// The limit grows back toward the configured one instead of jumping to it,
		// and stops below the smallest size the destination refused. A destination
		// whose body bound the configured batch never fits would otherwise walk the
		// halving ladder again after every batch it took, paying a doubling backoff
		// and an uploaded oversized body per step, and the outbox fills at a
		// body-size limit a stable batch size absorbs.
		limit = min(s.batchMax, max(1, tooLarge-1), 2*limit)
		if full {
			continue
		}
		if err := s.sleep(ctx, s.flush); err != nil {
			break
		}
	}
	return nil
}

// deliver posts one batch and, once the Reporting API has answered 200, removes
// it from the buffer. The error it returns is what the caller counts and backs
// off on, and a batch it returns an error for is still buffered.
func (s *Sender) deliver(ctx context.Context, batch []Row) error {
	events := make([]json.RawMessage, len(batch))
	ids := make([]int64, len(batch))
	for i, row := range batch {
		events[i] = row.EventJSON
		ids[i] = row.ID
	}
	// The stored documents go on the wire as they were buffered: they are what the
	// mapping produced, and re-encoding them here would put this file in the way
	// of what the API is sent.
	body, err := json.Marshal(events)
	if err != nil {
		return fmt.Errorf("encoding a batch of %d events: %w", len(batch), err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building the ingest request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+s.token)
	request.Header.Set("Content-Type", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("posting %d events: %w", len(batch), err)
	}
	// Reading the rest of the answer before closing it is what lets the connection
	// be used again, on the paths that read part of the body as well as on the one
	// that reads none of it.
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	if response.StatusCode != http.StatusOK {
		refusal, _ := io.ReadAll(io.LimitReader(response.Body, errorBodyMax))
		answer := fmt.Errorf("the Reporting API answered %d: %s", response.StatusCode, refusal)
		if response.StatusCode != http.StatusRequestEntityTooLarge {
			return answer
		}
		if len(batch) > 1 {
			return fmt.Errorf("%w: %w", errTooLarge, answer)
		}
		// A 413 for a body this small is the destination's bound and not the
		// event's: the consumer caps every event it buffers at eventMax, so one
		// under that bound is one a correctly configured API accepts, and the
		// refusal comes from an ingress, a proxy, or a WAF in front of it. It stays
		// buffered and the retry stays visible until an operator fixes that, which
		// is the rule every other status follows. Deciding on the status alone
		// would drain the whole outbox into nowhere one event at a time.
		if len(batch[0].EventJSON) <= eventMax {
			return answer
		}
		// One event the API refuses for its size is refused at every batch size, so
		// there is no smaller offer left to make. Keeping it would hold up every
		// event behind it forever, which costs the buffer rather than saving this
		// one event, so it goes and says so. The attempt is counted here rather
		// than by the caller, which counts what it backs off on: dropping an event
		// is not something the loop waits out, and a drop nothing counts is a data
		// loss that shows up in no dashboard.
		s.metrics.DeliveryError()
		s.logger.Error("the Reporting API refused a single event as too large, dropping it",
			"event_id", eventIDOf(batch[0]), "bytes", len(batch[0].EventJSON), "answer", answer)
		return s.outbox.DeleteBatch(ctx, ids)
	}

	var result ingestResult
	if err := json.NewDecoder(io.LimitReader(response.Body, resultBodyMax)).Decode(&result); err != nil {
		// The batch stays buffered: a 200 nobody could read is not one the sender
		// can act on, and the retry it forces is deduplicated by event_id.
		return fmt.Errorf("decoding the ingest result: %w", err)
	}
	// An answer cannot refuse more items than the batch carried, so a destination
	// that claims otherwise gets no more log lines than there were events.
	for _, refused := range result.Rejected[:min(len(result.Rejected), len(batch))] {
		s.logger.Warn("the Reporting API refused an event and dead-lettered it",
			"index", refused.Index, "event_id", refused.EventID, "reason", refused.Reason)
	}
	// What the answer says it stored is what is counted, so the counter keeps
	// meaning accepted events rather than posted ones: the duplicates of a resent
	// batch and the items the API dead-lettered are neither. The count is a number
	// this process does not control, and a Prometheus counter panics on a negative
	// one, which would take the collector down on every retry of the batch that
	// carried it. It is bounded by the batch above as well as below, for the same
	// reason the rejection log lines are.
	if result.Accepted > 0 {
		s.metrics.Delivered(min(result.Accepted, len(batch)))
	}

	if err := s.outbox.DeleteBatch(ctx, ids); err != nil {
		// The events are delivered, but a buffer that keeps them is a buffer the
		// next round reads again: returning the error is what puts the loop on its
		// backoff instead of letting it repost the same batch as fast as the API
		// answers. The repost is deduplicated by event_id.
		return fmt.Errorf("removing %d delivered events from the outbox: %w", len(batch), err)
	}
	return nil
}

// eventIDOf reads the event id out of a buffered document, for the one log line
// that has to name an event the sender is giving up on. An id it cannot read is
// empty, the way every other reader of a buffered document treats a gap.
func eventIDOf(row Row) string {
	var event struct {
		EventID string `json:"event_id"`
	}
	_ = json.Unmarshal(row.EventJSON, &event)
	return event.EventID
}

// ingestResult is the answer POST /api/v1/events gives a batch, as much of it as
// the sender reads. It mirrors the documented response instead of importing the
// server's type: the collector is a client of that API and shares no code with
// it, and it is built to run against a Tally the repository it lives in knows
// nothing about.
type ingestResult struct {
	Accepted   int             `json:"accepted"`
	Duplicates int             `json:"duplicates"`
	Rejected   []rejectedEvent `json:"rejected"`
}

// rejectedEvent is one refused item and the reason it was refused. The index is
// what identifies it, because an item may have been refused for carrying no
// event id at all.
type rejectedEvent struct {
	Index   int    `json:"index"`
	EventID string `json:"event_id"`
	Reason  string `json:"reason"`
}

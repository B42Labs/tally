package openstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	amqp091 "github.com/rabbitmq/amqp091-go"
)

// osloMessageKey names the envelope member the notification travels in. Its
// value is a string of JSON rather than a nested object, which is why a body
// has to be decoded twice.
const osloMessageKey = "oslo.message"

// timestampLayouts are the layouts a notification timestamp is tried against,
// in order. The services write the first two, with and without the microsecond
// fraction and neither carrying a zone; the third covers a deployment that
// emits RFC 3339 instead.
var timestampLayouts = []string{
	"2006-01-02 15:04:05.000000",
	"2006-01-02 15:04:05",
	time.RFC3339,
}

// Notification is one oslo.messaging notification as an OpenStack service
// emitted it: what happened, when, in which project, and the service's own
// description of the resource. It is the raw provider fact, before any mapping
// into the canonical event schema.
type Notification struct {
	// MessageID is the id the emitting service assigned the notification. It is
	// unique per notification, which makes it the key a redelivery is recognized
	// by.
	MessageID string
	// EventType names what happened, such as "compute.instance.create.end". The
	// namespace is the emitting service's own, not Tally's.
	EventType string
	// Timestamp is when the service recorded the event, always in UTC.
	Timestamp time.Time
	// Payload is the service's description of the resource, decoded as it
	// arrived. Its numbers are json.Number and not float64, so a quantity keeps
	// every digit it was sent with and can still be turned into an exact decimal.
	Payload map[string]any
	// ContextProjectID is the project the request ran in, taken from the request
	// context the service attaches to its notifications.
	ContextProjectID string
	// ContextTenantID is the same project under its older name. Services differ
	// in which of the two they set, so both are read and the caller takes
	// whichever is populated.
	ContextTenantID string
}

// notification is the inner document's shape: the members this package reads,
// with everything else ignored. The timestamp stays a string because the layout
// it arrives in is decided by parseTimestamp, not by encoding/json.
type notification struct {
	MessageID        string         `json:"message_id"`
	EventType        string         `json:"event_type"`
	Timestamp        string         `json:"timestamp"`
	Payload          map[string]any `json:"payload"`
	ContextProjectID string         `json:"_context_project_id"`
	ContextTenantID  string         `json:"_context_tenant_id"`
}

// ParseEnvelope decodes a message body into the notification it carries.
//
// The body has two layers. The outer object is the oslo envelope, which holds
// the notification under "oslo.message" as a string, and that string is the
// JSON document the emitting service wrote. Both layers are decoded here.
//
// Only the timestamp is required. A notification without a message id, event
// type, payload, or project comes back as it arrived, because deciding what to
// do about a gap belongs to the mapping rather than to the decoder. An error
// means the body is unusable: the consumer acknowledges such a delivery instead
// of requeueing it, since a body that fails to parse fails again on every
// redelivery, so this text is all the operator gets to see.
func ParseEnvelope(body []byte) (Notification, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return Notification{}, fmt.Errorf("decoding the oslo envelope: %w", err)
	}

	// Presence is checked before the type so that an envelope from some other
	// producer is reported as such, rather than as a message of the wrong shape.
	raw, ok := envelope[osloMessageKey]
	if !ok {
		return Notification{}, fmt.Errorf("the envelope has no %s member", osloMessageKey)
	}
	var message string
	if err := json.Unmarshal(raw, &message); err != nil {
		return Notification{}, fmt.Errorf("%s is not a string: %w", osloMessageKey, err)
	}

	decoder := json.NewDecoder(strings.NewReader(message))
	// The payload's numbers stay json.Number: a later stage divides them with
	// exact decimal arithmetic, and a detour through float64 would have rounded
	// them before that stage ever saw them.
	decoder.UseNumber()
	var inner notification
	if err := decoder.Decode(&inner); err != nil {
		return Notification{}, fmt.Errorf("decoding the oslo notification: %w", err)
	}

	if inner.Timestamp == "" {
		return Notification{}, errors.New("the oslo notification has no timestamp")
	}
	timestamp, err := parseTimestamp(inner.Timestamp)
	if err != nil {
		return Notification{}, err
	}

	return Notification{
		MessageID:        inner.MessageID,
		EventType:        inner.EventType,
		Timestamp:        timestamp,
		Payload:          inner.Payload,
		ContextProjectID: inner.ContextProjectID,
		ContextTenantID:  inner.ContextTenantID,
	}, nil
}

// parseTimestamp reads a notification timestamp as UTC. The zoneless layouts are
// parsed in UTC and not with time.Parse's local fallback, because the collector
// runs wherever it is deployed and a local zone would shift every event by that
// offset. A value that carries its own offset keeps it through the parse and is
// converted afterwards, so the instant survives either way.
func parseTimestamp(value string) (time.Time, error) {
	for _, layout := range timestampLayouts {
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("timestamp %q matches none of the known layouts", value)
}

// The names the collector claims on the broker. The queue is the collector's
// own; the tag names its consumer on the channel, which is what backpressure
// cancels and what resuming registers again.
const (
	queueName   = "tally-notifications"
	consumerTag = "tally-openstack-collector"
)

// exchangeKind is what every service exchange is: oslo publishes notifications
// under the topic the deployment configured, and a topic exchange is what routes
// a message to every queue bound to that topic.
const exchangeKind = "topic"

// The waits the collector takes. They are the defaults NewConsumer puts on the
// Consumer's fields, which is what lets a test shorten them.
const (
	// The bounds of the wait between two connect attempts. The first is short
	// enough that a broker restart costs a second, the cap keeps a broker that
	// stays down from being dialed in a loop.
	minReconnectBackoff = time.Second
	maxReconnectBackoff = time.Minute
	// reconnectJitter is the span added on top of every wait, uniformly. It
	// spreads the reconnects of a fleet of collectors that one broker restart
	// disconnected in the same instant.
	reconnectJitter = time.Second
	// pausePollInterval is how often a paused consumer looks at the outbox depth.
	pausePollInterval = 5 * time.Second
	// insertRetryDelay is how long the consumer waits after the outbox refused an
	// event. A buffer that fails, fails as fast as it is asked, so without this
	// wait a full disk would spin the queue through nack and redelivery as quickly
	// as the broker can serve it.
	insertRetryDelay = time.Second
)

// resumeShare is how far the outbox must have drained before a paused consumer
// resumes. Resuming at the bound itself would pause again on the very next
// message, so the pause ends only once a tenth of the buffer is free.
const resumeShare = 0.9

// The sizes a delivery may not exceed on its way into the buffer. Anyone who
// may publish on a notification exchange decides both of them, which on a stock
// OpenStack is every service account.
const (
	// bodyMax bounds the raw message body. An oslo notification is kilobytes; a
	// megabyte is already generous. What it closes is the decode: ParseEnvelope
	// decodes the body twice and expands the payload into a map[string]any that
	// is several times the wire size, and dying there means dying before the
	// acknowledgement and therefore again on every redelivery.
	//
	// What it does not close is how much is resident before that. The check runs
	// in handle, which the AMQP client reaches only after it has assembled the
	// whole frame sequence into delivery.Body, and RabbitMQ has never implemented
	// the prefetch_size half of Qos, so there is no client-side knob for it: what
	// bounds resident memory is Prefetch times the broker's max_message_size, and
	// docs/openstack-collector.md states both as deployment requirements.
	bodyMax = 1 << 20
	// eventMax bounds the mapped event. The payload strings it copies are the
	// publisher's, and an event past what one ingest request may carry is one no
	// batch holding it can ever be delivered.
	eventMax = 64 << 10
)

// eventBuffer is what the consumer needs of the outbox: commit one mapped event,
// and say how many are waiting. *Outbox satisfies it. The consumer takes the
// interface rather than the type so that a test can put a stage that fails in
// front of a real buffer.
type eventBuffer interface {
	Insert(ctx context.Context, eventJSON []byte) error
	Depth() int64
}

// Consumer reads oslo notifications off AMQP and buffers the events they map to.
//
// The chain is at-least-once, and the acknowledgement is what makes it so: a
// delivery is acknowledged only after the mapped event is committed to the
// outbox, so a crash between the two leaves the notification on the broker and
// the next connection is handed it again. That redelivery costs nothing, because
// the event carries the oslo message id as its event_id and the Reporting API
// deduplicates on it. Every retry along the way rests on that one property: a
// redelivered notification, a resent batch, and an outbox replayed after a
// restart all arrive as the same event.
type Consumer struct {
	cfg     Config
	buffer  eventBuffer
	metrics *Metrics
	logger  *slog.Logger

	// connected is what Connected reports, written as the session that owns the
	// connection begins and ends.
	connected atomic.Bool

	// The waits, as fields rather than constants at their call sites, so that the
	// integration tests can shorten them. Nothing outside this package sets them.
	minBackoff  time.Duration
	maxBackoff  time.Duration
	pausePoll   time.Duration
	insertRetry time.Duration
}

// NewConsumer builds the consumer over cfg's broker, buffering into buffer.
//
// m may be nil, which records nothing, and logger may be nil, which logs through
// the default logger.
func NewConsumer(cfg Config, buffer eventBuffer, m *Metrics, logger *slog.Logger) *Consumer {
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{
		cfg:         cfg,
		buffer:      buffer,
		metrics:     m,
		logger:      logger,
		minBackoff:  minReconnectBackoff,
		maxBackoff:  maxReconnectBackoff,
		pausePoll:   pausePollInterval,
		insertRetry: insertRetryDelay,
	}
}

// Run consumes until ctx is done, and then returns nil, which is the only way it
// returns. A connection that fails to come up, or one the broker closes, is
// retried with a growing wait: a broker that is restarting, or a service that
// has not declared its exchange yet, costs the collector a pause rather than the
// process, and the notifications wait on the bus meanwhile.
func (c *Consumer) Run(ctx context.Context) error {
	return connectLoop(ctx, c.logger, c.minBackoff, c.maxBackoff, c.session)
}

// Connected reports whether a connection and a consumer are established. It is
// what the readiness probe reads. A consumer paused by backpressure counts as
// connected: the connection is up and not consuming is the deliberate part.
func (c *Consumer) Connected() bool {
	return c.connected.Load()
}

// session is one connection's life: declare, bind, consume, and process
// deliveries until the broker closes the connection or ctx is done. The bool
// reports whether it got as far as consuming, which is what resets the caller's
// wait.
func (c *Consumer) session(ctx context.Context) (bool, error) {
	conn, channel, err := connect(c.cfg)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()

	// The collector's own queue, durable so that a restart finds what arrived
	// while it was down. It is a queue of its own rather than a share of somebody
	// else's, which is oslo's listener-pool semantics: a topic exchange copies
	// each notification to every bound queue, so Ceilometer and every other
	// listener keep receiving their copies untouched.
	if _, err := channel.QueueDeclare(queueName, true, false, false, false, nil); err != nil {
		return false, fmt.Errorf("declaring the queue %s: %w", queueName, err)
	}
	if err := bindQueue(channel, queueName, c.cfg); err != nil {
		return false, err
	}
	// The prefetch bounds what the broker hands out unacknowledged. Since the acks
	// follow the outbox insert, it also bounds what a crash has redelivered.
	if err := channel.Qos(c.cfg.Prefetch, 0, false); err != nil {
		return false, fmt.Errorf("bounding the prefetch: %w", err)
	}
	deliveries, err := c.consume(channel)
	if err != nil {
		return false, err
	}

	closed := conn.NotifyClose(make(chan *amqp091.Error, 1))
	c.connected.Store(true)
	defer c.connected.Store(false)

	return true, c.deliver(ctx, channel, deliveries, closed)
}

// consume registers this collector's consumer on the queue. The acks are manual,
// which is what lets the outbox commit come first.
func (c *Consumer) consume(channel *amqp091.Channel) (<-chan amqp091.Delivery, error) {
	deliveries, err := channel.Consume(queueName, consumerTag, false, false, false, false, nil)
	if err != nil {
		return nil, fmt.Errorf("consuming from %s: %w", queueName, err)
	}
	return deliveries, nil
}

// deliver processes deliveries until the connection closes or ctx is done. The
// context is checked around the select as well as in it, so a shutdown does not
// have to wait out the deliveries the broker has already handed over.
func (c *Consumer) deliver(ctx context.Context, channel *amqp091.Channel,
	deliveries <-chan amqp091.Delivery, closed <-chan *amqp091.Error,
) error {
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return nil
		case err := <-closed:
			return closedError(err)
		case delivery, ok := <-deliveries:
			if !ok {
				// The broker cancelled the consumer, which is what a deleted queue
				// does. The next session declares it again.
				return errors.New("the broker stopped delivering")
			}
			// The bound is weighed before the delivery is processed, so the message
			// that reached it is not the one that puts the outbox past it.
			if c.buffer.Depth() >= c.cfg.BufferMaxEvents {
				resumed, err := c.pause(ctx, channel, delivery, deliveries)
				if err != nil {
					return err
				}
				deliveries = resumed
				continue
			}
			if err := c.handle(ctx, delivery); err != nil {
				return err
			}
		}
	}
	return nil
}

// handle decides one delivery's fate. Exactly one of four things happens to it:
//
//   - the body is past bodyMax or does not parse: it is acknowledged and counted
//     as unparseable, because a body that failed to parse fails again on every
//     redelivery, and requeueing it would keep the same message in front of
//     every other one for as long as the collector runs;
//   - the mapping table claims nothing for its type, or the event it maps to is
//     past eventMax: acknowledged and counted as skipped;
//   - it maps and the outbox takes the event: acknowledged after the commit and
//     counted as consumed;
//   - it maps and the outbox refuses it: requeued, so the notification stays on
//     the broker until a buffer that works takes it.
//
// The error it returns is an AMQP one, which ends the session and reconnects. A
// refused insert is not one of them: that is the broker's message to keep.
func (c *Consumer) handle(ctx context.Context, delivery amqp091.Delivery) error {
	// The bound comes before the parse rather than after it: the parse is what an
	// oversized body would take the process down in, and a process that dies
	// there never acknowledges the delivery, so the broker hands the same message
	// to the next connection and the collector is in a crash loop no restart
	// clears.
	if len(delivery.Body) > bodyMax {
		c.logger.Warn("a delivery exceeded the body bound, acknowledging it",
			"exchange", delivery.Exchange, "routing_key", delivery.RoutingKey,
			"bytes", len(delivery.Body), "limit", bodyMax)
		c.metrics.Unparseable()
		return ack(delivery)
	}

	notification, err := ParseEnvelope(delivery.Body)
	if err != nil {
		c.logger.Warn("a delivery carried an unusable body, acknowledging it",
			"exchange", delivery.Exchange, "routing_key", delivery.RoutingKey, "error", err)
		c.metrics.Unparseable()
		return ack(delivery)
	}

	mapped, ok := MapNotification(notification, c.cfg.Cloud)
	if !ok {
		c.metrics.Skipped(notification.EventType)
		return ack(delivery)
	}

	eventJSON, err := json.Marshal(mapped)
	if err != nil {
		// The event was built from a body encoding/json decoded, so there is
		// nothing in it that cannot be encoded again. It is acknowledged for the
		// same reason an unusable body is: a redelivery would fail here again.
		c.logger.Error("a mapped event could not be encoded, acknowledging the delivery",
			"event_type", notification.EventType, "error", err)
		return ack(delivery)
	}
	// An event this large is one the ingest endpoint refuses for its size, and a
	// refused batch stays buffered: buffering it would put it in front of every
	// event behind it. It is acknowledged for the same reason an unusable body
	// is, and the log line names what to look for on the bus.
	if len(eventJSON) > eventMax {
		c.logger.Error("a mapped event is too large to deliver, acknowledging the delivery",
			"event_id", mapped.EventID, "event_type", notification.EventType,
			"bytes", len(eventJSON), "limit", eventMax)
		c.metrics.Skipped(notification.EventType)
		return ack(delivery)
	}

	if err := c.buffer.Insert(ctx, eventJSON); err != nil {
		c.logger.Error("buffering an event failed, requeueing the notification",
			"event_id", mapped.EventID, "error", fmt.Errorf("inserting into the outbox: %w", err))
		if err := delivery.Nack(false, true); err != nil {
			return fmt.Errorf("requeueing a delivery after a failed insert: %w", err)
		}
		return sleep(ctx, c.insertRetry)
	}
	if err := ack(delivery); err != nil {
		return err
	}
	c.metrics.Consumed(notification.EventType)
	return nil
}

// pause stops consuming while the outbox stands at its bound, and returns the
// deliveries of the consumer registered again once it has drained.
//
// The delivery that hit the bound and everything the client has already buffered
// go back to the broker: the events then wait on the bus, where they survive a
// collector that does not, rather than in a process that is already unable to
// write them down. Nothing is dropped, and the pause lasts as long as it has to.
func (c *Consumer) pause(ctx context.Context, channel *amqp091.Channel,
	hit amqp091.Delivery, deliveries <-chan amqp091.Delivery,
) (<-chan amqp091.Delivery, error) {
	c.logger.Warn("the outbox is at its bound, pausing the consumer",
		"depth", c.buffer.Depth(), "limit", c.cfg.BufferMaxEvents)

	if err := hit.Nack(false, true); err != nil {
		return nil, fmt.Errorf("requeueing a delivery at the buffer bound: %w", err)
	}
	if err := channel.Cancel(consumerTag, false); err != nil {
		return nil, fmt.Errorf("cancelling the consumer at the buffer bound: %w", err)
	}
	// The cancel closes the delivery channel, but the client hands out what it had
	// buffered first. Those messages were never processed, so they go back too.
	for delivery := range deliveries {
		if err := delivery.Nack(false, true); err != nil {
			return nil, fmt.Errorf("requeueing a buffered delivery: %w", err)
		}
	}

	resume := resumeShare * float64(c.cfg.BufferMaxEvents)
	for float64(c.buffer.Depth()) >= resume {
		if err := sleep(ctx, c.pausePoll); err != nil {
			return nil, err
		}
	}

	c.logger.Info("the outbox has drained, resuming the consumer", "depth", c.buffer.Depth())
	return c.consume(channel)
}

// Dump prints the notifications a deployment publishes, one JSON line per
// delivery, until ctx is done.
//
// It is the check that comes before a rollout. The oslo event types and their
// payload members differ per OpenStack release, and the exchange and topic names
// are a deployment's own configuration, so what the mapping table expects is
// verified against what the broker actually carries before the collector is
// pointed at it.
//
// The dump consumes through an exclusive, server-named, auto-deleting queue and
// acknowledges nothing it reads. A topic exchange copies every message to every
// bound queue, so what the dump prints is a copy: the collector's durable queue,
// Ceilometer, and a collector running at the same time all keep receiving
// theirs, and the broker is left as the dump found it.
func Dump(ctx context.Context, cfg Config, out io.Writer, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	return connectLoop(ctx, logger, minReconnectBackoff, maxReconnectBackoff,
		func(ctx context.Context) (bool, error) { return dumpSession(ctx, cfg, out) })
}

// dumpSession is one connection's worth of printing. It reports whether it got
// as far as consuming, the way the consumer's session does.
func dumpSession(ctx context.Context, cfg Config, out io.Writer) (bool, error) {
	conn, channel, err := connect(cfg)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close() }()

	// Server-named, exclusive, and auto-deleting: the queue belongs to this
	// connection and is gone with it, so a dump that is interrupted leaves no
	// queue behind that fills up unread.
	queue, err := channel.QueueDeclare("", false, true, true, false, nil)
	if err != nil {
		return false, fmt.Errorf("declaring the dump queue: %w", err)
	}
	if err := bindQueue(channel, queue.Name, cfg); err != nil {
		return false, err
	}
	// Automatic acks: the dump reads a copy of every message and owes the broker
	// nothing for it.
	deliveries, err := channel.Consume(queue.Name, "", true, true, false, false, nil)
	if err != nil {
		return false, fmt.Errorf("consuming from the dump queue: %w", err)
	}

	closed := conn.NotifyClose(make(chan *amqp091.Error, 1))
	encoder := json.NewEncoder(out)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return true, nil
		case err := <-closed:
			return true, closedError(err)
		case delivery, ok := <-deliveries:
			if !ok {
				return true, errors.New("the broker stopped delivering")
			}
			if err := printNotification(encoder, delivery); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

// dumpLine is one printed notification: what the broker delivered it under, and
// what the envelope carried.
type dumpLine struct {
	Exchange   string         `json:"exchange"`
	RoutingKey string         `json:"routing_key"`
	MessageID  string         `json:"message_id,omitempty"`
	EventType  string         `json:"event_type,omitempty"`
	Timestamp  time.Time      `json:"timestamp,omitzero"`
	Payload    map[string]any `json:"payload,omitempty"`
	// Unparseable holds the raw body of a delivery the parser refused, and is
	// absent on every other line.
	Unparseable string `json:"unparseable,omitempty"`
}

// previewMax is how much of an unparseable body the dump prints. What the dump
// is asked for is the shape of a message, and the beginning of it shows that.
const previewMax = 512

// contextSecrets matches the members of an oslo request context that hold a
// credential. An unversioned notification serializes that context next to the
// payload, and _context_auth_token is the Keystone token of the request that
// produced the message: valid for hours, and the dump's output is a file an
// operator attaches to a ticket.
var contextSecrets = regexp.MustCompile(`("_context_(?:auth_token|password)"\s*:\s*)"(?:[^"\\]|\\.)*"`)

// printNotification writes one delivery as a JSON line. A body that does not
// parse is printed raw rather than left out: showing an operator what the
// deployment sends is what the dump is for, and a body this collector cannot
// read is the most interesting thing it can find. Raw is not verbatim, though:
// the credentials the request context carries are replaced and the rest is cut
// off after previewMax bytes, because neither the event type nor the payload
// shape needs any of that.
func printNotification(encoder *json.Encoder, delivery amqp091.Delivery) error {
	line := dumpLine{Exchange: delivery.Exchange, RoutingKey: delivery.RoutingKey}
	if notification, err := ParseEnvelope(delivery.Body); err != nil {
		line.Unparseable = preview(delivery.Body)
	} else {
		line.MessageID = notification.MessageID
		line.EventType = notification.EventType
		line.Timestamp = notification.Timestamp
		line.Payload = notification.Payload
	}
	if err := encoder.Encode(line); err != nil {
		return fmt.Errorf("printing a notification: %w", err)
	}
	return nil
}

// preview redacts and cuts a body the parser refused.
//
// The inner document is unwrapped first, because it travels as a JSON string
// and not as a nested object: in the raw bytes its quotes are backslash-escaped,
// so a token arrives as \"_context_auth_token\": \"gAAAAAB…\" and the pattern,
// which needs a bare quote on both sides of the member name, matches nothing at
// all. Only after the unwrap are the quotes the ones it is written against.
//
// A JSON body whose envelope does not decode is previewed as it arrived, which
// is the case the pattern already handled: its quotes are the bare ones.
//
// A body that is not JSON at all is described instead of printed. The pattern
// needs JSON quoting around the member name, so on a msgpack-serialized
// notification — oslo.messaging offers that serializer — or on anything else a
// publisher on a bound topic sends, it matches nothing and the request context's
// credentials would reach the output as they arrived. That such a body arrived,
// and how long it is, is what the dump can say about those bytes without
// printing a live Keystone token into a file that goes on a ticket.
func preview(body []byte) string {
	if !json.Valid(body) {
		return fmt.Sprintf("%d bytes that are not JSON", len(body))
	}
	raw := body
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err == nil {
		var message string
		if err := json.Unmarshal(envelope[osloMessageKey], &message); err == nil {
			raw = []byte(message)
		}
	}
	redacted := contextSecrets.ReplaceAll(raw, []byte(`${1}"[redacted]"`))
	return string(redacted[:min(len(redacted), previewMax)])
}

// connectLoop runs one AMQP session after another until ctx is done, waiting
// between them. session reports whether it got as far as consuming, and that is
// what resets the wait: a connection that worked for a day and then dropped
// starts over at the shortest wait, while one that fails while coming up keeps
// doubling up to the cap.
func connectLoop(ctx context.Context, logger *slog.Logger, minBackoff, maxBackoff time.Duration,
	session func(context.Context) (bool, error),
) error {
	backoff := minBackoff
	for ctx.Err() == nil {
		established, err := session(ctx)
		if ctx.Err() != nil {
			break
		}
		if established {
			backoff = minBackoff
		}
		if err != nil {
			logger.Warn("the AMQP session ended, reconnecting", "error", err, "in", backoff)
		}
		if err := sleep(ctx, backoff+rand.N(reconnectJitter)); err != nil {
			break
		}
		backoff = min(2*backoff, maxBackoff)
	}
	return nil
}

// connect dials the broker, opens a channel, and declares the configured
// exchanges passively.
//
// Passively is the point: the exchanges belong to the OpenStack services, and
// which options they were declared with differs per deployment, so a collector
// that declared them itself would have to guess those options and would fail
// against every deployment that chose others. A missing exchange fails the
// declare and closes the channel with it, which is why the caller reconnects
// instead of binding what is left: a service that has not published yet costs a
// retry, and the error names the exchange it is waiting for.
func connect(cfg Config) (*amqp091.Connection, *amqp091.Channel, error) {
	// The URL is parsed here and the parse error is thrown away, because that one
	// error is the only one on this path that quotes the URL back: net/url formats
	// the whole input into it, password and all, and the reconnect loop writes
	// every session's error to the log. A password with a stray % is enough. What
	// Dial reports afterwards carries the host and the port alone.
	if _, err := amqp091.ParseURI(cfg.AMQPURL); err != nil {
		return nil, nil, fmt.Errorf("%s is not a usable AMQP URL", envAMQPURL)
	}
	conn, err := amqp091.Dial(cfg.AMQPURL)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing the broker: %w", err)
	}
	channel, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("opening a channel: %w", err)
	}
	for _, exchange := range cfg.Exchanges {
		if err := channel.ExchangeDeclarePassive(exchange, exchangeKind,
			true, false, false, false, nil); err != nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("declaring the exchange %s: %w", exchange, err)
		}
	}
	return conn, channel, nil
}

// bindQueue binds queue to every configured topic on every configured exchange.
// The topic is the routing key, because that is how oslo publishes: the
// notification topic is the key itself and not a prefix of one.
func bindQueue(channel *amqp091.Channel, queue string, cfg Config) error {
	for _, exchange := range cfg.Exchanges {
		for _, topic := range cfg.Topics {
			if err := channel.QueueBind(queue, topic, exchange, false, nil); err != nil {
				return fmt.Errorf("binding %s to %s on %s: %w", queue, topic, exchange, err)
			}
		}
	}
	return nil
}

// ack acknowledges a delivery. A failed acknowledgement means the channel is
// gone, so it ends the session and the message is delivered again on the next
// one, which is what the outbox insert coming first makes safe.
func ack(delivery amqp091.Delivery) error {
	if err := delivery.Ack(false); err != nil {
		return fmt.Errorf("acknowledging a delivery: %w", err)
	}
	return nil
}

// closedError names why a connection ended. NotifyClose reports nil when the
// close was this collector's own.
func closedError(err *amqp091.Error) error {
	if err == nil {
		return errors.New("the connection to the broker closed")
	}
	return fmt.Errorf("the broker closed the connection: %w", err)
}

// sleep waits for d, or until ctx is done and then reports ctx's error. Every
// wait the collector takes goes through it, so a shutdown never has to outlast a
// backoff.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

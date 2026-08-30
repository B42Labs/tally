package openstack

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/b42labs/tally/internal/core/cardinality"
)

// Metrics holds the collector's instruments and the registry they are
// registered on: the five counters over the consume and the deliver path that
// WP 1.12 defines, plus the two gauges that report the outbox. It registers
// them together with the Go runtime and process collectors, which Handler then
// serves.
//
// The zero value is not usable; call NewMetrics. A nil *Metrics is usable and
// records nothing, so a collector built without metrics does not panic on every
// notification. Passing a nil *Metrics is the supported way to turn the
// instrumentation off.
//
// The delivered counter counts what an answer says the Reporting API stored,
// not how many events the batch carried: a resent batch is answered as
// duplicates, and an item the API dead-lettered was refused. Counting the batch
// instead would report full throughput while every event in it is being
// refused, which is the one failure the counter has to make visible.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.12.
type Metrics struct {
	reg     *prometheus.Registry
	handler http.Handler

	// limiter bounds the event_type label. Its values arrive off the AMQP wire,
	// and a vector keeps every child it is ever asked for for the life of the
	// process, so a deployment emitting notification types Tally has never heard
	// of turns them into process memory and scrape response otherwise.
	limiter *cardinality.Limiter

	consumed       *prometheus.CounterVec
	skipped        *prometheus.CounterVec
	unparseable    prometheus.Counter
	delivered      prometheus.Counter
	deliveryErrors prometheus.Counter
}

// NewMetrics builds the instruments and registers them on reg, along with the Go
// runtime and process collectors. reg is also what Handler gathers from. It
// panics if reg already carries one of these collectors, which makes a second
// NewMetrics over the same registry a programming error rather than a silent
// half-registration.
//
// depth and oldestSeconds back the two gauges and are read at scrape time, not
// here. The binary wires them to the outbox's Depth and OldestBufferedSeconds,
// which is why the buffer needs no recording method of its own: what a gauge
// reports is a state the outbox already knows, not a sum of events.
func NewMetrics(reg *prometheus.Registry, depth, oldestSeconds func() float64) *Metrics {
	m := &Metrics{
		reg:     reg,
		limiter: cardinality.New(LabelValueLimit),
		consumed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_collector_consumed_total",
			Help: "Notifications mapped to an event and buffered.",
		}, []string{labelEventType}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_collector_skipped_total",
			Help: "Notifications the mapping table produced no event for.",
		}, []string{labelEventType}),
		// This one is unlabeled where the two above carry an event type: a body
		// that did not parse holds no event type worth labeling a count with.
		unparseable: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tally_collector_unparseable_total",
			Help: "AMQP deliveries whose body could not be parsed.",
		}),
		delivered: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tally_collector_delivered_total",
			Help: "Events the Reporting API accepted.",
		}),
		deliveryErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tally_collector_delivery_errors_total",
			Help: "Delivery attempts the Reporting API did not accept.",
		}),
	}

	reg.MustRegister(
		m.consumed,
		m.skipped,
		m.unparseable,
		m.delivered,
		m.deliveryErrors,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "tally_collector_buffer_depth",
			Help: "Events waiting in the outbox.",
		}, depth),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "tally_collector_oldest_buffered_seconds",
			Help: "Age of the oldest event waiting in the outbox, 0 when it is empty, " +
				"and NaN when the buffer cannot be read.",
		}, oldestSeconds),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		MaxRequestsInFlight: maxScrapesInFlight,
		Timeout:             scrapeBudget,
	})
	return m
}

// The bounds one scrape is served under. A gather walks every child of every
// collector while holding its lock and serializes the lot, so an unbounded
// number of concurrent scrapes turns a 200-byte GET into as much work as the
// registry is large. The route carries no credential, which is what makes that
// worth bounding rather than trusting: a scrape past the limit is answered 503,
// and one that outlives the budget is cut off.
const (
	maxScrapesInFlight = 3
	scrapeBudget       = 10 * time.Second
)

// Handler serves the registry in the Prometheus exposition format. It is what
// /metrics is mounted on. Every scrape is answered by the one handler built in
// NewMetrics, because the in-flight bound above is a bound only while the
// scrapes share it.
func (m *Metrics) Handler() http.Handler {
	return m.handler
}

// Consumed counts one notification that was mapped and buffered. The event type
// is bounded before it becomes a label value, because it is whatever the broker
// delivered.
func (m *Metrics) Consumed(eventType string) {
	if m == nil {
		return
	}
	m.consumed.WithLabelValues(m.limiter.Bound(labelEventType, eventType)).Inc()
}

// Skipped counts one notification that produced no event, because no mapping
// table entry covers its type or the entry gated it off. An unmapped type is
// what mints a series here, so the event type this counter is handed is
// arbitrary by construction and bounded like the consumed one.
func (m *Metrics) Skipped(eventType string) {
	if m == nil {
		return
	}
	m.skipped.WithLabelValues(m.limiter.Bound(labelEventType, eventType)).Inc()
}

// Unparseable counts one delivery whose body did not parse.
func (m *Metrics) Unparseable() {
	if m == nil {
		return
	}
	m.unparseable.Inc()
}

// Delivered counts the n events one 200 answer covered.
func (m *Metrics) Delivered(n int) {
	if m == nil {
		return
	}
	m.delivered.Add(float64(n))
}

// DeliveryError counts one delivery attempt that failed. A batch the Reporting
// API refused stays in the outbox and is offered again, so one batch may count
// here several times before it counts as delivered once.
func (m *Metrics) DeliveryError() {
	if m == nil {
		return
	}
	m.deliveryErrors.Inc()
}

// labelEventType is the one label whose values this package does not decide.
// The notification carries it, and the envelope parser checks it against no set
// of known values.
const labelEventType = "event_type"

// LabelValueLimit is how many distinct event types the series may carry, on the
// terms internal/core/cardinality bounds them with.
//
// It is 100 here and 128 in internal/reporting/metrics on purpose. Each service
// bounds what its own traffic can mint, and 100 is several times the mapping
// table, which is the vocabulary a healthy deployment stays inside. The two
// numbers are not meant to track each other.
//
// It is exported because the simulator is held to it: a generated month has to
// stay inside the bound, or the series an operator is told to read fold into
// event_type="other".
const LabelValueLimit = 100

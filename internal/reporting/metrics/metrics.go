// Package metrics instruments the Reporting API. One Metrics value owns the
// nine series WP 1.11 defines: eight counters over the ingest, projection, and
// reconciliation paths, and the tally_current_resources gauge that reports the
// fleet the projection holds. It registers them together with the Go runtime and
// process collectors on a registry of its own, which Handler then serves.
//
// Every recording method tolerates a nil receiver, so a component built without
// metrics records nothing instead of panicking. Passing a nil *Metrics is the
// supported way to turn the instrumentation off.
//
// One imprecision is accepted. A Prometheus counter cannot be rolled back, so
// every code path counts on its success return: a database transaction that
// fails to commit after the instrumented call returned leaves the counters high
// by that one batch.
//
// What an event contributes to a label is bounded here rather than trusted. A
// vector keeps every child it is ever asked for for the life of the process,
// and event_type, resource_type, and state reach this package from the ingest
// request body, so without a bound of their own a credential holder turns them
// into unbounded process memory and an unbounded scrape response.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.11.
package metrics

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the instruments of the Reporting API and the registry they are
// registered on. The zero value is not usable; call New. A nil *Metrics is, and
// records nothing.
type Metrics struct {
	reg     *prometheus.Registry
	handler http.Handler

	// limiter bounds the label values that come off an ingested event. The
	// refresher shares it, so a resource type the counters admitted is the one
	// the gauge reports too.
	limiter *limiter

	eventsIngested      *prometheus.CounterVec
	eventsDeduplicated  *prometheus.CounterVec
	eventsRejected      *prometheus.CounterVec
	sizeUnvalidated     *prometheus.CounterVec
	projectionReplays   *prometheus.CounterVec
	syncRuns            *prometheus.CounterVec
	resourcesReconciled *prometheus.CounterVec
	syncErrors          *prometheus.CounterVec

	// currentResources is written by the refresher, which reads the counts off the
	// projection, not by any of the recording methods.
	currentResources *prometheus.GaugeVec
}

// New builds the instruments and registers them on reg, along with the Go
// runtime and process collectors. reg is also what Handler gathers from. It
// panics if reg already carries one of these collectors, which makes a second
// New over the same registry a programming error rather than a silent
// half-registration.
func New(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		reg:     reg,
		limiter: newLimiter(),
		eventsIngested: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_events_ingested_total",
			Help: "Events stored by the ingest pipeline.",
		}, []string{"platform", "cloud", "resource_type", "event_type", "source"}),
		eventsDeduplicated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_events_deduplicated_total",
			Help: "Events dropped because the same event was already stored.",
		}, []string{"cloud"}),
		eventsRejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_events_rejected_total",
			Help: "Events refused by the ingest pipeline, by the kind of check that refused them.",
		}, []string{"cloud", "reason"}),
		sizeUnvalidated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_ingest_unvalidated_size_total",
			Help: "Events stored with a size no registered schema validated.",
		}, []string{"platform", "resource_type"}),
		projectionReplays: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_projection_replays_total",
			Help: "Projection rows folded again from a resource's whole event history.",
		}, []string{"cloud"}),
		syncRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_sync_runs_total",
			Help: "Reconciliation runs that finished, by status.",
		}, []string{"cloud", "status"}),
		resourcesReconciled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_sync_resources_reconciled_total",
			Help: "Resources a reconciliation run created, updated, or deleted.",
		}, []string{"cloud", "action"}),
		syncErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_sync_errors_total",
			Help: "Errors reconciliation runs reported.",
		}, []string{"cloud"}),
		currentResources: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tally_current_resources",
			Help: "Resources the projection holds, by state.",
		}, []string{"platform", "cloud", "resource_type", "state"}),
	}

	reg.MustRegister(
		m.eventsIngested,
		m.eventsDeduplicated,
		m.eventsRejected,
		m.sizeUnvalidated,
		m.projectionReplays,
		m.syncRuns,
		m.resourcesReconciled,
		m.syncErrors,
		m.currentResources,
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
// New, because the in-flight bound above is a bound only while the scrapes
// share it.
func (m *Metrics) Handler() http.Handler {
	return m.handler
}

// EventIngested counts one stored event. The resource type and the event type
// are bounded before they become label values; the platform, the cloud, and the
// source are not, because the scope check pins the first two to the credential
// and the pipeline states the third itself.
func (m *Metrics) EventIngested(platform, cloud, resourceType, eventType, source string) {
	if m == nil {
		return
	}
	m.eventsIngested.WithLabelValues(
		platform,
		cloud,
		m.limiter.bound(labelResourceType, resourceType),
		m.limiter.bound(labelEventType, eventType),
		source,
	).Inc()
}

// EventDeduplicated counts one event that was already stored.
func (m *Metrics) EventDeduplicated(cloud string) {
	if m == nil {
		return
	}
	m.eventsDeduplicated.WithLabelValues(cloud).Inc()
}

// EventRejected counts one refused event under the kind of check that refused
// it, which normalize reduces the pipeline's reason to.
func (m *Metrics) EventRejected(cloud, reason string) {
	if m == nil {
		return
	}
	m.eventsRejected.WithLabelValues(cloud, normalizeReason(reason)).Inc()
}

// SizeUnvalidated counts one event stored with a size no schema validated. An
// unregistered pair is what mints a series here, so the resource type this
// counter is handed is arbitrary by construction and bounded like every other.
func (m *Metrics) SizeUnvalidated(platform, resourceType string) {
	if m == nil {
		return
	}
	m.sizeUnvalidated.WithLabelValues(platform, m.limiter.bound(labelResourceType, resourceType)).Inc()
}

// ProjectionReplayed counts one projection row folded again from its history.
func (m *Metrics) ProjectionReplayed(cloud string) {
	if m == nil {
		return
	}
	m.projectionReplays.WithLabelValues(cloud).Inc()
}

// SyncRunFinished counts one finished reconciliation run under its status.
func (m *Metrics) SyncRunFinished(cloud, status string) {
	if m == nil {
		return
	}
	m.syncRuns.WithLabelValues(cloud, status).Inc()
}

// ResourcesReconciled counts n resources a reconciliation run acted on. action
// is created, updated, or deleted.
func (m *Metrics) ResourcesReconciled(cloud, action string, n int) {
	if m == nil {
		return
	}
	m.resourcesReconciled.WithLabelValues(cloud, action).Add(float64(n))
}

// SyncErrorsRecorded counts the n errors a reconciliation run reported.
func (m *Metrics) SyncErrorsRecorded(cloud string, n int) {
	if m == nil {
		return
	}
	m.syncErrors.WithLabelValues(cloud).Add(float64(n))
}

// normalizeReason reduces a rejection reason to the text before its first colon.
// The pipeline spells its reasons "<check>: <detail>", and the detail carries
// decoder and validator output, which would put an unbounded number of label
// values into the series. What is left is the check that refused the event:
// schema, size_schema, or scope.
func normalizeReason(reason string) string {
	check, _, _ := strings.Cut(reason, ":")
	return check
}

// The labels whose values an event decides. event.Validate bounds resource_type
// to 512 characters and event_type to a pattern with no length limit at all,
// and checks neither against a set of known values; state is whatever
// payload.state spelled, which nothing checks beyond its presence.
const (
	labelResourceType = "resource_type"
	labelEventType    = "event_type"
	labelState        = "state"
)

// What one such label may put into the series: values longer than
// labelValueMax, and values past the labelValueLimit-th distinct one, are
// counted under labelOverflow instead.
//
// The limit is per label and admits on a first-seen basis, so a deployment
// reporting fewer kinds than that sees its real values throughout and a
// credential filling the limit with noise costs the remaining series their
// detail rather than the process its memory. labelOverflow is a value an event
// may legitimately carry as well; the two then share a series, which is the
// price of not keeping a second, unbounded vocabulary to tell them apart.
const (
	labelValueMax   = 128
	labelValueLimit = 128
	labelOverflow   = "other"
)

// limiter is the vocabulary the bounded labels have been recorded under. It is
// shared by every instrument and safe for concurrent use: the ingest path calls
// it from whatever goroutine serves the batch.
type limiter struct {
	mu       sync.Mutex
	admitted map[labelValue]struct{}
	distinct map[string]int
}

// labelValue is one value of one label, which is what a limiter admits.
type labelValue struct{ label, value string }

func newLimiter() *limiter {
	return &limiter{admitted: map[labelValue]struct{}{}, distinct: map[string]int{}}
}

// bound returns what value may be recorded as under label: value itself while
// the label has room for it, and labelOverflow once it has not.
func (l *limiter) bound(label, value string) string {
	if len(value) > labelValueMax {
		return labelOverflow
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	admitted := labelValue{label: label, value: value}
	if _, ok := l.admitted[admitted]; ok {
		return value
	}
	if l.distinct[label] >= labelValueLimit {
		return labelOverflow
	}
	l.admitted[admitted] = struct{}{}
	l.distinct[label]++
	return value
}

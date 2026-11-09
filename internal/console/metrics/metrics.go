// Package metrics instruments the console. One Metrics value owns two counters,
// the requests that passed the sign-in and the requests the sign-in refused, so
// every request counts once. It registers them with the Go runtime and process
// collectors on a registry of its own, which Handler then serves.
//
// Both recording methods tolerate a nil receiver, so a console built without
// metrics records nothing instead of panicking.
//
// No label needs a cardinality limiter. A route is a pattern the router
// registered and carries no identifier, because every identifier travels in the
// query. A status is a code a handler wrote, and a reason is one of the
// constants in the signin package.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/b42labs/tally/internal/console/signin"
)

// routeUnmatched is the route of a path or a method the console does not serve.
const routeUnmatched = "unmatched"

// The bounds one scrape is served under, the same as the two other services
// use. The route carries no credential, so a scrape past the limit is answered
// 503 and one that outlives the budget is cut off.
const (
	maxScrapesInFlight = 3
	scrapeBudget       = 10 * time.Second
)

// Metrics holds the console's instruments. The zero value is not usable; call
// New. A nil *Metrics is, and records nothing.
type Metrics struct {
	handler http.Handler

	requests *prometheus.CounterVec
	refusals *prometheus.CounterVec
}

// New builds the instruments and registers them on reg, along with the Go
// runtime and process collectors. reg is also what Handler gathers from. It
// panics if reg already carries one of these collectors, which makes a second
// New over the same registry a programming error.
//
// Every reason is created at zero, so that increase() sees the first refusal.
func New(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_console_requests_total",
			Help: "Requests that passed the sign-in, by route and status code.",
		}, []string{"route", "status"}),
		refusals: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_console_sign_in_refusals_total",
			Help: "Requests the sign-in refused, by reason.",
		}, []string{"reason"}),
	}
	for _, reason := range signin.Reasons {
		m.refusals.WithLabelValues(reason)
	}

	reg.MustRegister(
		m.requests,
		m.refusals,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	m.handler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		MaxRequestsInFlight: maxScrapesInFlight,
		Timeout:             scrapeBudget,
	})
	return m
}

// Handler serves the registry in the Prometheus exposition format. Every scrape
// is answered by the one handler built in New, because the in-flight bound is a
// bound only while the scrapes share it.
func (m *Metrics) Handler() http.Handler {
	return m.handler
}

// Request counts one request that passed the sign-in. An empty route is a path
// or a method the console does not serve.
func (m *Metrics) Request(route string, status int) {
	if m == nil {
		return
	}
	if route == "" {
		route = routeUnmatched
	}
	m.requests.WithLabelValues(route, strconv.Itoa(status)).Inc()
}

// SignInRefused counts one request the sign-in refused.
func (m *Metrics) SignInRefused(reason string) {
	if m == nil {
		return
	}
	m.refusals.WithLabelValues(reason).Inc()
}

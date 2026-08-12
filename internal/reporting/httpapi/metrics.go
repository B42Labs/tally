package httpapi

import (
	"net/http"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

// Metrics serves this service's instruments in the Prometheus exposition
// format. The route carries no credential, which is what the conventions ask of
// every service (roadmap/00-conventions.md section 7). The Gateway publishes
// /api/v1 alone, so nothing off the cluster reaches it; inside the cluster
// nothing stops any pod from reading the fleet the exposition names, because
// deploy/ carries no NetworkPolicy yet. Bounding what one scrape may cost is
// therefore the metrics package's job rather than the router's.
//
// A build whose instrumentation is off answers 404 rather than an empty
// exposition: a scrape that returns nothing looks like a service with no
// activity, while a 404 tells the operator that this deployment serves no
// metrics at all.
func (s *server) Metrics(w http.ResponseWriter, r *http.Request) {
	if !s.metricsEnabled || s.metrics == nil {
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound,
			"Not found", "this service does not serve metrics")
		return
	}
	s.metrics.Handler().ServeHTTP(w, r)
}

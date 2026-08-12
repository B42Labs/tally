package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/metrics"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// metricsRoute is where a Prometheus scrape reaches this service.
const metricsRoute = "/metrics"

// TestMetricsOverHTTP drives GET /metrics through the whole router: the
// contract has to describe the route, the dispatch table has to serve it to a
// request carrying no credential, and a deployment that serves no metrics has
// to answer a scrape the way it answers any unknown path.
func TestMetricsOverHTTP(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("serves the exposition to a request carrying no credential", func(t *testing.T) {
		// The router enforces authentication, so a scrape reaching the handler is
		// what says the route is credential-free rather than unguarded by accident.
		a := newMetricsAPI(t, db.Store, metrics.New(prometheus.NewRegistry()), true)

		rec := a.call(t, http.MethodGet, metricsRoute, "", nil)

		if got := rec.Code; got != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
			t.Errorf("Content-Type = %q, want the text/plain of the exposition format", got)
		}
		if got := rec.Body.String(); !strings.Contains(got, "go_goroutines") {
			t.Errorf("the body carries no go_goroutines series, want the Go collector in it:\n%s", got)
		}
	})

	t.Run("carries no series of an instrument nothing has recorded yet", func(t *testing.T) {
		// A counter or gauge with no label combination collects nothing, so a
		// service that has ingested and reconciled nothing exposes neither series:
		// a rate over a counter that only appears once it is first incremented is
		// well defined, one over a counter that starts at zero at every restart is
		// not.
		a := newMetricsAPI(t, db.Store, metrics.New(prometheus.NewRegistry()), true)

		body := a.call(t, http.MethodGet, metricsRoute, "", nil).Body.String()

		for _, name := range []string{"tally_events_ingested_total", "tally_current_resources"} {
			if strings.Contains(body, name) {
				t.Errorf("the body carries %s, want no series before anything recorded one:\n%s", name, body)
			}
		}
	})

	t.Run("answers a scrape 404 while the instrumentation is off", func(t *testing.T) {
		a := newMetricsAPI(t, db.Store, metrics.New(prometheus.NewRegistry()), false)

		rec := a.call(t, http.MethodGet, metricsRoute, "", nil)

		assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
	})

	t.Run("answers a scrape 404 when the service was built without instruments", func(t *testing.T) {
		// Configuration that turns the route on cannot conjure the instruments, so
		// the handler answers this the way it answers the disabled route.
		a := newMetricsAPI(t, db.Store, nil, true)

		rec := a.call(t, http.MethodGet, metricsRoute, "", nil)

		assertProblem(t, rec, http.StatusNotFound, problem.TypeNotFound)
	})
}

// newMetricsAPI builds the full router over s with authentication enforced and
// the scrape route configured as m and enabled say. The shared harness wires no
// instrumentation at all, so a test that scrapes builds its router here.
func newMetricsAPI(t *testing.T, s *store.Store, m *metrics.Metrics, enabled bool) api {
	t.Helper()

	q := sqlcgen.New(s.Pool())
	handler, err := NewRouter(Options{
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:                 s,
		UnhealthyThreshold: time.Minute,
		Queries:            q,
		Store:              s,
		AuthMode:           auth.ModeEnforced,
		InternalToken:      internalToken,
		Authenticator:      auth.NewStaticTokenAuthenticator(q),
		Pipeline:           ingest.New(registry.New(), false, nil),
		Syncer: reconciliation.New(s, ingest.New(registry.New(), false, nil),
			reconciliation.Config{}, map[string]reconciliation.Adapter{}, time.Now),
		Metrics:        m,
		MetricsEnabled: enabled,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v, want nil", err)
	}
	return api{store: s, queries: q, handler: handler}
}

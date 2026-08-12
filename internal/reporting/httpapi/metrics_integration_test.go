package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/b42labs/tally/internal/core/event"
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

// The series a scrape of a service that has worked carries.
const (
	ingestedSeries     = "tally_events_ingested_total"
	deduplicatedSeries = "tally_events_deduplicated_total"
	rejectedSeries     = "tally_events_rejected_total"
	runsSeries         = "tally_sync_runs_total"
	reconciledSeries   = "tally_sync_resources_reconciled_total"
	errorsSeries       = "tally_sync_errors_total"
)

// TestMetricsCountWhatTheServiceDid drives the wiring cmd/tally-reporting
// builds: one Metrics value behind the ingest pipeline, the syncer, and the
// scrape route. What a request did over HTTP is then readable in the body the
// next scrape answers with, which is what an operator has to be able to rely
// on. The subtests share one database, and each of them works on a cloud and a
// registry of its own, so what one counts stays out of every other.
func TestMetricsCountWhatTheServiceDid(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("counts a stored event under the source the server stamped it with", func(t *testing.T) {
		const cloud = "os-metrics-ingested"
		a := newMeteredAPI(t, db.Store, cloud, &syncFake{})
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-metrics-ingested"}

		got := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, token,
			item(t, res.event("vol-metrics-ingested-create", "volume.create", createTime,
				payloadOf("available", volumeSize(10))))))

		if got.Accepted != 1 {
			t.Fatalf("result = %+v, want the event stored", got)
		}
		assertScrape(t, a, ingestedSeries, map[string]float64{
			ingestedLabels(cloud, res.resourceType, "volume.create", event.SourceCollector): 1,
		})
	})

	t.Run("counts a resubmitted batch as duplicates and stores nothing more", func(t *testing.T) {
		const cloud = "os-metrics-duplicated"
		a := newMeteredAPI(t, db.Store, cloud, &syncFake{})
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		res := fixture{cloud: cloud, resourceType: "volume", id: "vol-metrics-duplicated"}
		body := item(t, res.event("vol-metrics-duplicated-create", "volume.create", createTime,
			payloadOf("available", volumeSize(10))))

		if got := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, token, body)); got.Accepted != 1 {
			t.Fatalf("result of the first call = %+v, want the event stored", got)
		}
		if got := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, token, body)); got.Duplicates != 1 {
			t.Fatalf("result of the replay = %+v, want the event counted as a duplicate", got)
		}

		assertScrape(t, a, deduplicatedSeries, map[string]float64{cloudLabel(cloud): 1})
		// The replay stored nothing, so the ingest counter stands where the first
		// call left it rather than at two.
		assertScrape(t, a, ingestedSeries, map[string]float64{
			ingestedLabels(cloud, res.resourceType, "volume.create", event.SourceCollector): 1,
		})
	})

	t.Run("counts a refused item under the check that refused it", func(t *testing.T) {
		const cloud = "os-metrics-rejected"
		a := newMeteredAPI(t, db.Store, cloud, &syncFake{})
		_, token := seedIngestCredential(t, a.queries, fixturePlatform, cloud)
		// The timestamp is a number rather than an instant, so the item does not
		// decode at all. What it names is therefore whatever the caller cared to
		// write, and the counter labels the refusal with the cloud the token
		// authenticated for instead.
		undecodable := json.RawMessage(fmt.Sprintf(`{"event_id": "vol-metrics-undecodable",
			"timestamp": 42, "event_type": "volume.create", "platform": %q, "cloud": "os-metrics-claimed",
			"resource_type": "volume", "resource_id": "vol-metrics-rejected", "project_id": %q,
			"payload": {"state": "available"}}`, fixturePlatform, fixtureProject))

		got := ingestResultOf(t, a.call(t, http.MethodPost, eventsRoute, token, batch(t, undecodable)))

		if len(got.Rejected) != 1 || got.Accepted != 0 {
			t.Fatalf("result = %+v, want the undecodable item refused and nothing stored", got)
		}
		assertScrape(t, a, rejectedSeries, map[string]float64{cloudLabel(cloud) + `,reason="schema"`: 1})
		// A batch that stored nothing leaves the ingest counter without a series,
		// rather than with one standing at zero.
		assertScrape(t, a, ingestedSeries, map[string]float64{})
	})

	t.Run("counts a finished run, its corrections, and the events it stored", func(t *testing.T) {
		const cloud = "os-metrics-sync"
		// The projection holds nothing for this cloud, so the observed resource is
		// drift the run corrects with one synthetic create.
		fake := &syncFake{observed: []reconciliation.ObservedResource{{
			ResourceType: syncResourceType,
			ResourceID:   "vol-metrics-synced",
			ProjectID:    fixtureProject,
			State:        "available",
			Size:         map[string]any{"size_gb": 10, "type": "ssd"},
		}}}
		a := newMeteredAPI(t, db.Store, cloud, fake)

		rec := a.call(t, http.MethodPost, syncRoute(cloud), internalToken, nil)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body)
		}
		var result SyncResult
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
		}
		if result.Stats.Created != 1 {
			t.Fatalf("stats = %+v, want the observed resource reported as new", result.Stats)
		}

		assertScrape(t, a, runsSeries, map[string]float64{runLabels(cloud, "completed"): 1})
		assertScrape(t, a, reconciledSeries, map[string]float64{
			reconciledLabels(cloud, "created"): float64(result.Stats.Created),
			reconciledLabels(cloud, "updated"): float64(result.Stats.Updated),
			reconciledLabels(cloud, "deleted"): float64(result.Stats.Deleted),
		})
		// The corrections go through the ordinary pipeline, so they count as
		// ingested events under the source the run stamped them with.
		assertScrape(t, a, ingestedSeries, map[string]float64{
			ingestedLabels(cloud, syncResourceType, "sync.create", event.SourceReconciliation): 1,
		})
	})

	t.Run("counts a run that failed and the errors it recorded", func(t *testing.T) {
		const cloud = "os-metrics-sync-failed"
		a := newMeteredAPI(t, db.Store, cloud, &syncFake{err: errPlatformDown})

		rec := a.call(t, http.MethodPost, syncRoute(cloud), internalToken, nil)

		assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
		assertScrape(t, a, runsSeries, map[string]float64{runLabels(cloud, "failed"): 1})
		// How many reasons a failure leaves behind is the run's business; that it
		// left one is what the counter has to say.
		recorded := scrape(t, a, errorsSeries)
		if got := recorded[cloudLabel(cloud)]; got < 1 {
			t.Errorf("%s = %v, want the failure the run recorded counted", errorsSeries, recorded)
		}
	})
}

// newMeteredAPI builds the full router over s the way cmd/tally-reporting
// builds the process: one Metrics value behind everything that records, and the
// scrape route serving the registry it was built on. cloud is the one the
// syncer is configured with, reached through fake.
//
// The ingest route and the syncer share one pipeline, so a collector's batch
// and a run's corrections count into the same instruments, each under its own
// source.
func newMeteredAPI(t *testing.T, s *store.Store, cloud string, fake *syncFake) api {
	t.Helper()

	m := metrics.New(prometheus.NewRegistry())
	pipeline := ingest.New(registry.New(), false, nil, m)
	cfg := reconciliation.Config{Clouds: []reconciliation.CloudConfig{{
		Cloud: cloud, Platform: fixturePlatform, Adapter: syncAdapterName,
	}}}

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
		Pipeline:           pipeline,
		Syncer: reconciliation.New(s, pipeline, cfg,
			map[string]reconciliation.Adapter{syncAdapterName: fake}, time.Now, m),
		Metrics:        m,
		MetricsEnabled: true,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v, want nil", err)
	}
	return api{store: s, queries: q, handler: handler}
}

// The label sets the asserted series carry, as the exposition format spells
// them: one pair per label, in alphabetical order.
func ingestedLabels(cloud, resourceType, eventType string, source event.Source) string {
	return fmt.Sprintf("cloud=%q,event_type=%q,platform=%q,resource_type=%q,source=%q",
		cloud, eventType, fixturePlatform, resourceType, source)
}

func reconciledLabels(cloud, action string) string {
	return fmt.Sprintf("action=%q,cloud=%q", action, cloud)
}

func runLabels(cloud, status string) string {
	return fmt.Sprintf("cloud=%q,status=%q", cloud, status)
}

func cloudLabel(cloud string) string {
	return fmt.Sprintf("cloud=%q", cloud)
}

// assertScrape fails the test unless a scrape carries exactly want for the
// named metric. Comparing the whole map also states which series the requests
// before it left alone.
func assertScrape(t *testing.T, a api, name string, want map[string]float64) {
	t.Helper()

	if got := scrape(t, a, name); !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// scrape reads GET /metrics through the router and returns every sample the
// body carries for the named metric, keyed by the label set its line spells. It
// parses the exposition a Prometheus server would read rather than gathering
// from the registry behind it, so what it asserts is what a scrape gets.
func scrape(t *testing.T, a api, name string) map[string]float64 {
	t.Helper()

	rec := a.call(t, http.MethodGet, metricsRoute, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body)
	}

	found := map[string]float64{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		// A sample line is "name{labels} value"; the HELP and TYPE lines in front
		// of it start with a hash and match no series.
		series, raw, ok := strings.Cut(line, " ")
		if !ok || !strings.HasPrefix(series, name+"{") {
			continue
		}
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			t.Fatalf("parsing the value of the sample %q: %v", line, err)
		}
		found[strings.TrimSuffix(strings.TrimPrefix(series, name+"{"), "}")] = value
	}
	return found
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
		Pipeline:           ingest.New(registry.New(), false, nil, nil),
		Syncer: reconciliation.New(s, ingest.New(registry.New(), false, nil, nil),
			reconciliation.Config{}, map[string]reconciliation.Adapter{}, time.Now, nil),
		Metrics:        m,
		MetricsEnabled: enabled,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v, want nil", err)
	}
	return api{store: s, queries: q, handler: handler}
}

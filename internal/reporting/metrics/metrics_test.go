package metrics

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// tallySeries names every series this package registers. The tests assert over
// the whole set, so an instrument that is added without a test shows up here.
var tallySeries = []string{
	"tally_events_ingested_total",
	"tally_events_deduplicated_total",
	"tally_events_rejected_total",
	"tally_ingest_unvalidated_size_total",
	"tally_projection_replays_total",
	"tally_sync_runs_total",
	"tally_sync_resources_reconciled_total",
	"tally_sync_errors_total",
	"tally_current_resources",
}

func TestNilMetrics(t *testing.T) {
	t.Run("records nothing instead of panicking", func(t *testing.T) {
		var m *Metrics

		m.EventIngested("openstack", "prod", "instance", "compute.instance.create.end", "collector")
		m.EventDeduplicated("prod")
		m.EventRejected("prod", "schema: invalid character 'x'")
		m.SizeUnvalidated("openstack", "instance")
		m.ProjectionReplayed("prod")
		m.SyncRunFinished("prod", "completed")
		m.ResourcesReconciled("prod", "created", 3)
		m.SyncErrorsRecorded("prod", 1)
	})
}

func TestNew(t *testing.T) {
	t.Run("registers the Go runtime collector", func(t *testing.T) {
		// Only the Go collector is asserted: what the process collector exports
		// depends on the platform, and on macOS it exports nothing.
		m := fresh(t)

		if got := gatherAndCount(t, m, "go_goroutines"); got != 1 {
			t.Errorf("the registry exports %d go_goroutines series, want 1", got)
		}
	})

	t.Run("exports no tally series before the first recording", func(t *testing.T) {
		// Every tally instrument is labeled, and a labeled instrument has no child
		// until a recording method names one.
		m := fresh(t)

		if got := gatherAndCount(t, m, tallySeries...); got != 0 {
			t.Errorf("the fresh registry exports %d tally series, want none", got)
		}
	})
}

func TestEventRejected(t *testing.T) {
	t.Run("labels the rejection with the check that refused the event", func(t *testing.T) {
		// The pipeline's reason carries decoder and validator output, which would
		// put an unbounded number of values into the reason label.
		for name, tc := range map[string]struct {
			reason string
			want   string
		}{
			"decoder detail": {
				reason: "schema: invalid character 'x'",
				want:   "schema",
			},
			"size schema detail": {
				reason: "size_schema: no schema registered for (openstack, instance)",
				want:   "size_schema",
			},
			"no detail at all": {
				reason: "scope",
				want:   "scope",
			},
		} {
			t.Run(name, func(t *testing.T) {
				m := fresh(t)

				m.EventRejected("prod", tc.reason)

				if got := testutil.CollectAndCount(m.eventsRejected); got != 1 {
					t.Fatalf("tally_events_rejected_total has %d series, want the one the rejection recorded", got)
				}
				// A reason that was not normalized leaves this child at zero, because
				// asking for it is what creates it.
				if got := testutil.ToFloat64(m.eventsRejected.WithLabelValues("prod", tc.want)); got != 1 {
					t.Errorf(`tally_events_rejected_total{cloud="prod",reason=%q} = %v, want 1`, tc.want, got)
				}
			})
		}
	})
}

func TestResourcesReconciled(t *testing.T) {
	t.Run("adds up what several runs reconciled", func(t *testing.T) {
		m := fresh(t)

		m.ResourcesReconciled("prod", "created", 3)
		m.ResourcesReconciled("prod", "created", 2)

		if got := testutil.ToFloat64(m.resourcesReconciled); got != 5 {
			t.Errorf("tally_sync_resources_reconciled_total = %v, want 5", got)
		}
	})

	t.Run("counts every action on its own series", func(t *testing.T) {
		m := fresh(t)

		m.ResourcesReconciled("prod", "created", 3)
		m.ResourcesReconciled("prod", "deleted", 1)

		if got := testutil.CollectAndCount(m.resourcesReconciled); got != 2 {
			t.Fatalf("tally_sync_resources_reconciled_total has %d series, want one per action", got)
		}
		for action, want := range map[string]float64{"created": 3, "deleted": 1} {
			if got := testutil.ToFloat64(m.resourcesReconciled.WithLabelValues("prod", action)); got != want {
				t.Errorf(`tally_sync_resources_reconciled_total{action=%q} = %v, want %v`, action, got, want)
			}
		}
	})
}

func TestSyncErrorsRecorded(t *testing.T) {
	t.Run("adds up the errors of several runs", func(t *testing.T) {
		m := fresh(t)

		m.SyncErrorsRecorded("prod", 3)
		m.SyncErrorsRecorded("prod", 2)

		if got := testutil.ToFloat64(m.syncErrors); got != 5 {
			t.Errorf("tally_sync_errors_total = %v, want 5", got)
		}
	})
}

func TestBoundedLabels(t *testing.T) {
	t.Run("keeps a long label value out of the series", func(t *testing.T) {
		// event.Validate bounds resource_type to 512 characters and event_type
		// to a pattern with no length limit at all, so both reach this package
		// longer than a series name should ever be.
		m := fresh(t)
		long := strings.Repeat("a", labelValueMax+1)

		m.EventIngested("openstack", "prod", long, "compute."+long, "collector")

		if got := testutil.ToFloat64(m.eventsIngested.WithLabelValues(
			"openstack", "prod", labelOverflow, labelOverflow, "collector",
		)); got != 1 {
			t.Errorf("the overflow series counted %v of the long labels, want 1", got)
		}
		if got := testutil.CollectAndCount(m.eventsIngested); got != 1 {
			t.Errorf("tally_events_ingested_total has %d series, want the one overflow series", got)
		}
	})

	t.Run("folds every value past the limit into one series", func(t *testing.T) {
		// A credential holder can send a distinct resource type per item, and a
		// counter child is never evicted, so the number of them the vector may
		// hold is what has to be bounded rather than their length alone.
		m := fresh(t)

		for i := range labelValueLimit + 100 {
			m.SizeUnvalidated("openstack", "type-"+strconv.Itoa(i))
		}

		// The admitted values plus the one bucket the rest share.
		if got := testutil.CollectAndCount(m.sizeUnvalidated); got != labelValueLimit+1 {
			t.Errorf("tally_ingest_unvalidated_size_total has %d series, want %d",
				got, labelValueLimit+1)
		}
		if got := testutil.ToFloat64(m.sizeUnvalidated.WithLabelValues("openstack", labelOverflow)); got != 100 {
			t.Errorf("the overflow series counted %v events, want the 100 past the limit", got)
		}
	})

	t.Run("keeps reporting a value it already admitted", func(t *testing.T) {
		m := fresh(t)

		for i := range labelValueLimit + 100 {
			m.SizeUnvalidated("openstack", "type-"+strconv.Itoa(i))
		}
		m.SizeUnvalidated("openstack", "type-0")

		if got := testutil.ToFloat64(m.sizeUnvalidated.WithLabelValues("openstack", "type-0")); got != 2 {
			t.Errorf(`tally_ingest_unvalidated_size_total{resource_type="type-0"} = %v, want 2`, got)
		}
	})

	t.Run("bounds each label on its own", func(t *testing.T) {
		// The limit is per label: filling resource_type must not cost event_type
		// the values it would otherwise report.
		m := fresh(t)

		for i := range labelValueLimit {
			m.EventIngested("openstack", "prod", "type-"+strconv.Itoa(i), "compute.instance.create.end", "collector")
		}
		m.EventIngested("openstack", "prod", "type-past-the-limit", "compute.instance.delete.end", "collector")

		if got := testutil.ToFloat64(m.eventsIngested.WithLabelValues(
			"openstack", "prod", labelOverflow, "compute.instance.delete.end", "collector",
		)); got != 1 {
			t.Error("the event type was folded into the overflow bucket along with the resource type")
		}
	})
}

func TestHandler(t *testing.T) {
	t.Run("answers every scrape from the same handler", func(t *testing.T) {
		// The in-flight bound the handler carries is a bound only while the
		// scrapes share it, so a handler built per request would silently drop it.
		m := fresh(t)

		first := reflect.ValueOf(m.Handler()).Pointer()
		if second := reflect.ValueOf(m.Handler()).Pointer(); first != second {
			t.Error("Handler() returns a new handler per call, so its in-flight bound is per call too")
		}
	})

	t.Run("answers the registry in the Prometheus exposition format", func(t *testing.T) {
		m := fresh(t)
		m.EventDeduplicated("prod")

		rec := httptest.NewRecorder()
		m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
			t.Errorf("Content-Type = %q, want a text/plain family type", got)
		}

		body := rec.Body.String()
		for _, want := range []string{
			"# HELP tally_events_deduplicated_total",
			"# TYPE tally_events_deduplicated_total counter",
			`tally_events_deduplicated_total{cloud="prod"} 1`,
			"go_goroutines",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("the scraped body carries no %q:\n%s", want, body)
			}
		}
	})
}

// fresh builds a Metrics over a registry of its own, so the series one test
// records stay out of every other test.
func fresh(t *testing.T) *Metrics {
	t.Helper()

	return New(prometheus.NewRegistry())
}

// gatherAndCount reports how many series m's registry exports for the named
// metrics.
func gatherAndCount(t *testing.T, m *Metrics, names ...string) int {
	t.Helper()

	count, err := testutil.GatherAndCount(m.reg, names...)
	if err != nil {
		t.Fatalf("gathering the registry: %v", err)
	}
	return count
}

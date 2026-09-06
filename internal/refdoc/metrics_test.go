package refdoc

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/b42labs/tally/internal/providers/openstack"
	reportingmetrics "github.com/b42labs/tally/internal/reporting/metrics"
)

// The number of series each of the two services of this repository owns. A
// series added without a row on the page is noticed here.
const (
	reportingSeries = 9
	collectorSeries = 7
)

// newFixtureRegistry builds a registry carrying every shape a row is rendered
// from: a vector without a child, whose type only its name states; a vector
// whose collector bounds the values of a label, which the descriptor writes
// differently; an unlabelled counter the registry holds a value for; a gauge
// read at scrape time; a help text naming a placeholder; and the Go collector,
// which reports the process rather than the product.
func newFixtureRegistry(t *testing.T) *prometheus.Registry {
	t.Helper()

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tally_fixture_events_total",
			Help: "Events the fixture stored.",
		}, []string{"cloud", "source"}),
		prometheus.V2.NewCounterVec(prometheus.CounterVecOpts{
			CounterOpts: prometheus.CounterOpts{
				Name: "tally_fixture_bounded_total",
				Help: "Deliveries the fixture bounded the clouds of.",
			},
			VariableLabels: prometheus.ConstrainedLabels{
				prometheus.ConstrainedLabel{
					Name:       "cloud",
					Constraint: func(value string) string { return value },
				},
			},
		}),
		prometheus.NewCounter(prometheus.CounterOpts{
			Name: "tally_fixture_deliveries_total",
			Help: "Batches the fixture delivered.",
		}),
		prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "tally_fixture_resources",
			Help: "Resources the fixture holds, by <state> as the projection reports it.",
		}, []string{"state"}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "tally_fixture_buffer_depth",
			Help: "Events waiting in the fixture outbox.",
		}, func() float64 { return 0 }),
		collectors.NewGoCollector(),
	)
	return reg
}

func TestMetrics(t *testing.T) {
	got, err := Metrics(newFixtureRegistry(t))
	if err != nil {
		t.Fatalf("Metrics() error = %v, want nil", err)
	}

	assertWant(t, "metrics.want.md", got)
}

func TestMetricsRendersEachInstrumentShape(t *testing.T) {
	got, err := Metrics(newFixtureRegistry(t))
	if err != nil {
		t.Fatalf("Metrics() error = %v, want nil", err)
	}

	for _, want := range []string{
		// A vector without a child is gathered as nothing, so its name is what
		// says it counts.
		"| `tally_fixture_events_total` | counter | `cloud`, `source` |",
		// A gauge read at scrape time carries one value and no label.
		"| `tally_fixture_buffer_depth` | gauge | none |",
		// A label whose values its collector bounds is written c(cloud) by the
		// descriptor, and the bound is the collector's business, not a page's.
		"| `tally_fixture_bounded_total` | counter | `cloud` |",
		// A placeholder in a help text is a code span rather than markup.
		"Resources the fixture holds, by `<state>` as the projection reports it.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
	// The rows are sorted, whatever order the registry describes them in.
	assertOrder(t, got, []string{
		"`tally_fixture_bounded_total`",
		"`tally_fixture_buffer_depth`",
		"`tally_fixture_deliveries_total`",
		"`tally_fixture_events_total`",
		"`tally_fixture_resources`",
	})
	// The Go collector reports the process, not the product.
	if strings.Contains(got, "go_") {
		t.Errorf("the rendering carries a runtime series:\n%s", got)
	}
}

func TestMetricsRendersTheReportingInstruments(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := reportingmetrics.New(reg)
	m.EventIngested("openstack", "os-prod-eu1", "instance", "compute.instance.create.end", "collector")
	m.EventDeduplicated("os-prod-eu1")
	m.EventRejected("os-prod-eu1", "schema: bad size")
	m.SizeUnvalidated("openstack", "instance")
	m.ProjectionReplayed("os-prod-eu1")
	m.SyncRunFinished("os-prod-eu1", "completed")
	m.ResourcesReconciled("os-prod-eu1", "created", 1)
	m.SyncErrorsRecorded("os-prod-eu1", 1)

	got, err := Metrics(reg)
	if err != nil {
		t.Fatalf("Metrics() error = %v, want nil", err)
	}
	if n := countRows(got); n != reportingSeries {
		t.Errorf("the Reporting API rendered %d series, want %d:\n%s", n, reportingSeries, got)
	}
	for _, want := range []string{
		"| `tally_events_ingested_total` | counter | " +
			"`platform`, `cloud`, `resource_type`, `event_type`, `source` |",
		// The gauge the refresher writes carries no value until a run wrote
		// one, so its name is what types it.
		"| `tally_current_resources` | gauge | `platform`, `cloud`, `resource_type`, `state` |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

func TestMetricsRendersTheCollectorInstruments(t *testing.T) {
	reg := prometheus.NewRegistry()
	zero := func() float64 { return 0 }
	m := openstack.NewMetrics(reg, zero, zero)
	m.Consumed("compute.instance.create.end")
	m.Skipped("compute.instance.unknown")
	m.Unparseable()
	m.Delivered(1)
	m.DeliveryError()

	got, err := Metrics(reg)
	if err != nil {
		t.Fatalf("Metrics() error = %v, want nil", err)
	}
	if n := countRows(got); n != collectorSeries {
		t.Errorf("the collector rendered %d series, want %d:\n%s", n, collectorSeries, got)
	}
	for _, want := range []string{
		"| `tally_collector_consumed_total` | counter | `event_type` |",
		"| `tally_collector_oldest_buffered_seconds` | gauge | none |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

func TestMetricsRejectsWhatItCannotRender(t *testing.T) {
	cases := map[string]struct {
		register func(*prometheus.Registry)
		want     string
	}{
		"a registry of nothing this project owns": {
			register: func(reg *prometheus.Registry) {
				reg.MustRegister(collectors.NewGoCollector())
			},
			want: "refdoc: the registry carries no tally_ metric",
		},
		"a gauge named as a counter": {
			register: func(reg *prometheus.Registry) {
				reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{
					Name: "tally_y_total",
					Help: "A gauge the exposition format reads as a counter.",
				}))
			},
			want: "refdoc: tally_y_total is a gauge but its name says counter",
		},
		"a counter named as a gauge": {
			register: func(reg *prometheus.Registry) {
				reg.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{
					Name: "tally_sync_errors",
					Help: "A counter whose name drops the suffix.",
				}))
			},
			want: "refdoc: tally_sync_errors is a counter but its name says gauge",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			tc.register(reg)

			_, err := Metrics(reg)
			if err == nil {
				t.Fatalf("Metrics() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("Metrics() error = %q, want %q", err, tc.want)
			}
		})
	}
}

// countRows is how many rows a table carries, the header and its separator
// left out.
func countRows(rendered string) int {
	return max(countHeadings(rendered, "| ")-2, 0)
}

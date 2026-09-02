package simulator

import (
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// The endpoint the inventory of a month is scraped off. It stands in for the
// OpenStack database exporter a real cloud runs beside its services: the same
// series under the same names, read off the month instead of off the databases
// of nova, cinder, neutron, glance and octavia. A dashboard written against a
// real cloud therefore fills against a simulated one, and the drill compares
// what the panels show with what the oracle states.
//
// What a scrape reports is decided by InventoryAt alone. This file turns those
// samples into the exposition format and serves them; which series a month
// holds, and what a value is, is stated there.

// exporterHelp is the help every family of this endpoint carries. The exporter
// it stands in for writes one line per family out of its own table, and one
// line for all of them says the thing somebody reading a scrape by hand needs
// to know: where the numbers come from and which instant they belong to.
const exporterHelp = "The simulated cloud's inventory at the run's virtual instant, " +
	"under the OpenStack database exporter's names."

// The bounds one scrape is served under. They are the collector's, stated in
// internal/providers/openstack/metrics.go together with the reasons: a gather
// walks and serializes the whole registry, and this route carries no credential
// either. The two constants there are unexported, so the values are repeated
// here rather than shared.
const (
	maxScrapesInFlight = 3
	scrapeBudget       = 10 * time.Second
)

// inventoryCollector renders the inventory of one month on every scrape. It
// holds the month and the clock rather than a rendered set of samples, because
// what a scrape reports is the world at the instant it arrives: a drill that
// stops the clock reads one document twice, and one that lets it run watches
// the month go by.
type inventoryCollector struct {
	month Month
	clock *Clock
}

// Describe sends no descriptor, which registers this as an unchecked collector.
// The families a scrape carries follow the world at that instant: a month holds
// no load balancer before the first one is built, and states none until it is.
// A descriptor set fixed at registration would either promise families that are
// absent or forbid the ones that appear.
func (c inventoryCollector) Describe(chan<- *prometheus.Desc) {}

// Collect states every sample the inventory places at the clock's instant, each
// as a gauge under the labels the sample carries. An instant past the end of
// the month is answered at that end, which is where a listing of the fake API
// is held too, so a clock that outran the month keeps reporting what it held.
//
// A scrape carries no timestamp of its own: Prometheus stamps a sample with the
// instant it read it, and the virtual instant the value belongs to is the one
// the whole run is paced by.
//
// The exposition format is float by definition. Every value here is a count, a
// gibibyte figure or the byte size of an image, all far below 2^53, so the
// conversion states them exactly.
func (c inventoryCollector) Collect(out chan<- prometheus.Metric) {
	for _, sample := range InventoryAt(c.month, c.clock.Now()) {
		keys := slices.Sorted(maps.Keys(sample.Labels))
		values := make([]string, 0, len(keys))
		for _, key := range keys {
			values = append(values, sample.Labels[key])
		}
		out <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(sample.Name, exporterHelp, keys, nil),
			prometheus.GaugeValue, float64(sample.Value), values...)
	}
}

// NewExporter serves the inventory of one month in the Prometheus exposition
// format, over the clock the run is paced by. It is what GET /metrics is
// mounted on for as long as the run publishes.
//
// The registry is private and holds this one collector: no Go collector and no
// process collector. The endpoint stands in for an exporter of the simulated
// cloud, and a scrape that also carried the simulator's heap and its file
// descriptors would state series about the process running the drill under an
// endpoint that is supposed to speak for a cloud.
func NewExporter(month Month, clock *Clock) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(inventoryCollector{month: month, clock: clock})
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		MaxRequestsInFlight: maxScrapesInFlight,
		Timeout:             scrapeBudget,
	})
}

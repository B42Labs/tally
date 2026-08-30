package openstack

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/b42labs/tally/internal/core/cardinality"
)

// collectorSeries names every series this file registers. The tests assert over
// the whole set, so an instrument that is added without a test shows up here.
var collectorSeries = []string{
	"tally_collector_consumed_total",
	"tally_collector_skipped_total",
	"tally_collector_unparseable_total",
	"tally_collector_delivered_total",
	"tally_collector_delivery_errors_total",
	"tally_collector_buffer_depth",
	"tally_collector_oldest_buffered_seconds",
}

// freshMetrics builds a Metrics over a registry of its own, so the series one
// test records stay out of every other test. The gauges report fixed values,
// which is what lets a scrape assert the closures are the ones being read.
func freshMetrics(t *testing.T) *Metrics {
	t.Helper()

	return NewMetrics(
		prometheus.NewRegistry(),
		func() float64 { return 7 },
		func() float64 { return 42.5 },
	)
}

// scrapeMetrics serves one request against m's handler and returns the body.
func scrapeMetrics(t *testing.T, m *Metrics) string {
	t.Helper()

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	return rec.Body.String()
}

func TestNilMetricsRecordsNothingInsteadOfPanicking(t *testing.T) {
	var m *Metrics

	m.Consumed("compute.instance.create.end")
	m.Skipped("compute.instance.reboot.start")
	m.Unparseable()
	m.Delivered(5)
	m.DeliveryError()
}

func TestNewMetricsExportsEveryInstrument(t *testing.T) {
	m := freshMetrics(t)

	m.Consumed("compute.instance.create.end")
	m.Skipped("compute.instance.reboot.start")
	m.Unparseable()
	m.Delivered(5)
	m.DeliveryError()

	body := scrapeMetrics(t, m)
	for _, name := range collectorSeries {
		if !strings.Contains(body, "# TYPE "+name+" ") {
			t.Errorf("the scraped body carries no %s:\n%s", name, body)
		}
	}
	// Only the Go collector is asserted: what the process collector exports
	// depends on the platform, and on macOS it exports nothing.
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("the scraped body carries no go_goroutines, so the Go collector is unregistered:\n%s", body)
	}
}

func TestNewMetricsExportsNoLabeledSeriesBeforeTheFirstRecording(t *testing.T) {
	// The two event_type vectors have no child until a recording method names
	// one, which is what keeps a quiet collector out of the vocabulary.
	m := freshMetrics(t)

	count, err := testutil.GatherAndCount(m.reg,
		"tally_collector_consumed_total", "tally_collector_skipped_total")
	if err != nil {
		t.Fatalf("gathering the registry: %v", err)
	}
	if count != 0 {
		t.Errorf("the fresh registry exports %d event_type series, want none", count)
	}
}

func TestNewMetricsReadsTheGaugesFromTheInjectedClosures(t *testing.T) {
	m := freshMetrics(t)

	body := scrapeMetrics(t, m)
	for _, want := range []string{
		"tally_collector_buffer_depth 7",
		"tally_collector_oldest_buffered_seconds 42.5",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the scraped body carries no %q:\n%s", want, body)
		}
	}
}

func TestConsumedCountsEveryNotificationOfATypeOnOneSeries(t *testing.T) {
	m := freshMetrics(t)

	m.Consumed("compute.instance.create.end")
	m.Consumed("compute.instance.create.end")

	if got := testutil.CollectAndCount(m.consumed); got != 1 {
		t.Fatalf("tally_collector_consumed_total has %d series, want the one the event type mints", got)
	}
	if got := testutil.ToFloat64(m.consumed.WithLabelValues("compute.instance.create.end")); got != 2 {
		t.Errorf(`tally_collector_consumed_total{event_type="compute.instance.create.end"} = %v, want 2`, got)
	}
}

func TestDeliveredAddsUpWhatTheBatchesCarried(t *testing.T) {
	m := freshMetrics(t)

	m.Delivered(5)
	m.Delivered(3)

	if got := testutil.ToFloat64(m.delivered); got != 8 {
		t.Errorf("tally_collector_delivered_total = %v, want 8", got)
	}
}

func TestConsumedFoldsEveryEventTypePastTheLimitIntoOneSeries(t *testing.T) {
	// The event types come off the wire and a counter child is never evicted, so
	// the number of them the vector may hold is what has to be bounded.
	m := freshMetrics(t)

	for i := range LabelValueLimit {
		m.Consumed("compute.instance.type-" + strconv.Itoa(i))
	}
	// The first type past the limit, recorded twice so the overflow series can be
	// told apart from a single stray recording.
	m.Consumed("compute.instance.past-the-limit")
	m.Consumed("compute.instance.past-the-limit")

	// The admitted values plus the one bucket the rest share.
	if got := testutil.CollectAndCount(m.consumed); got != LabelValueLimit+1 {
		t.Errorf("tally_collector_consumed_total has %d series, want %d", got, LabelValueLimit+1)
	}
	if got := testutil.ToFloat64(m.consumed.WithLabelValues(cardinality.Overflow)); got != 2 {
		t.Errorf(`tally_collector_consumed_total{event_type=%q} = %v, want the 2 past the limit`,
			cardinality.Overflow, got)
	}
}

func TestConsumedKeepsALongEventTypeOutOfTheSeriesAndOfTheBudget(t *testing.T) {
	// A long value is refused on its length alone, before the vocabulary is even
	// consulted, so it cannot spend one of the slots a real event type needs.
	m := freshMetrics(t)

	m.Consumed(strings.Repeat("a", cardinality.ValueMax+1))
	for i := range LabelValueLimit {
		m.Consumed("compute.instance.type-" + strconv.Itoa(i))
	}

	if got := testutil.ToFloat64(m.consumed.WithLabelValues(cardinality.Overflow)); got != 1 {
		t.Errorf(`tally_collector_consumed_total{event_type=%q} = %v, want the 1 long value`,
			cardinality.Overflow, got)
	}
	if got := testutil.ToFloat64(m.consumed.WithLabelValues(
		"compute.instance.type-" + strconv.Itoa(LabelValueLimit-1),
	)); got != 1 {
		t.Error("the long event type consumed a slot, so the last real event type was folded into the overflow bucket")
	}
}

func TestSkippedSharesTheBoundWithConsumed(t *testing.T) {
	// Both labels are the same event_type vocabulary, so a type the consumed
	// counter admitted is the one the skipped counter reports too, and the limit
	// is spent once rather than per instrument.
	m := freshMetrics(t)

	for i := range LabelValueLimit {
		m.Consumed("compute.instance.type-" + strconv.Itoa(i))
	}
	m.Skipped("compute.instance.type-0")
	m.Skipped("compute.instance.past-the-limit")

	if got := testutil.ToFloat64(m.skipped.WithLabelValues("compute.instance.type-0")); got != 1 {
		t.Errorf(`tally_collector_skipped_total{event_type="compute.instance.type-0"} = %v, want 1`, got)
	}
	if got := testutil.ToFloat64(m.skipped.WithLabelValues(cardinality.Overflow)); got != 1 {
		t.Errorf(`tally_collector_skipped_total{event_type=%q} = %v, want the 1 past the limit`,
			cardinality.Overflow, got)
	}
}

func TestLabelValueLimitIsTheCollectorsOwnBound(t *testing.T) {
	// The reporting registry bounds its labels at 128. This one is fixed at 100
	// by WP 1.12, and the two are not meant to drift together.
	if LabelValueLimit != 100 {
		t.Errorf("LabelValueLimit = %d, want the 100 the collector is specified with", LabelValueLimit)
	}
}

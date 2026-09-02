package simulator

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// aggregateSeries is the five the alert TallyExporterServiceSilent reads off a
// database exporter, in deploy/kubernetes/base/vmalert/rules.yaml. A scrape of
// the stand-in that states one of them under another name is an exporter the
// rule calls silent.
var aggregateSeries = []string{
	seriesNovaTotalVMs, seriesCinderVolumes, seriesNeutronFloatingIPs,
	seriesGlanceImages, seriesLoadBalancerTotal,
}

// scrapeOf is the body of one scrape of a month at a virtual instant. The clock
// runs at factor 0, so the instant the endpoint answers at is the one the test
// picked however long the test takes.
func scrapeOf(t *testing.T, month Month, at time.Time) string {
	t.Helper()

	server := httptest.NewServer(NewExporter(month, NewClock(at, 0, time.Now)))
	t.Cleanup(server.Close)

	status, contentType, body := request(t, server, http.MethodGet, "/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics at %s = %d, want %d (body %q)",
			at.Format(time.RFC3339), status, http.StatusOK, body)
	}
	if !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("GET /metrics Content-Type = %q, want it to start with text/plain", contentType)
	}
	return body
}

// scrapedGauge is the one sample a family of the exposition holds: its value
// and its labels. A scrape of this endpoint is a short document of gauges
// without a timestamp, so it is read line by line rather than through a parser
// this module would have to depend on for the tests alone.
//
// A family stated twice, stated not at all, or typed as anything but a gauge
// fails here: those are the three ways a collector states an inventory a
// dashboard cannot read.
func scrapedGauge(t *testing.T, body, name string) (float64, map[string]string) {
	t.Helper()

	if !strings.Contains(body, "# TYPE "+name+" gauge\n") {
		t.Errorf("%s is not typed as a gauge, which is what an inventory is read as", name)
	}

	// The name is matched whole, against the line's own name and not against a
	// prefix of it, so openstack_cinder_volumes is no line of
	// openstack_cinder_volume_gb.
	held := make([]string, 0, 1)
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
			held = append(held, line)
		}
	}
	if len(held) != 1 {
		t.Fatalf("%s is stated %d times, want once", name, len(held))
	}

	labels := make(map[string]string)
	rest, labeled := strings.CutPrefix(held[0], name+"{")
	if labeled {
		block, tail, closed := strings.Cut(rest, "}")
		if !closed {
			t.Fatalf("the line %q closes no label block", held[0])
		}
		for _, pair := range strings.Split(block, ",") {
			key, value, ok := strings.Cut(pair, "=")
			if !ok {
				t.Fatalf("the label %q of %s is no key=\"value\" pair", pair, name)
			}
			// No label of this endpoint carries a quote or a comma of its own, so
			// the quotes around a value are the only ones on the line.
			labels[key] = strings.Trim(value, `"`)
		}
		rest = tail
	} else {
		rest = strings.TrimPrefix(held[0], name)
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
	if err != nil {
		t.Fatalf("the value of %s in %q: %v", name, held[0], err)
	}
	return value, labels
}

func TestExporterServesTheInventoryOfTheInstant(t *testing.T) {
	at := cloudDay(10)
	month := faultyMonth(t, 1, Faults{})
	body := scrapeOf(t, month, at)

	samples := InventoryAt(month, at)
	for _, name := range aggregateSeries {
		held := namedSamples(samples, name)
		if len(held) != 1 {
			t.Fatalf("the inventory states %s %d times, want once", name, len(held))
		}
		value, labels := scrapedGauge(t, body, name)
		if value != float64(held[0].Value) {
			t.Errorf("the scrape states %s = %g, want the %d the inventory holds at %s",
				name, value, held[0].Value, at.Format(time.RFC3339))
		}
		if len(labels) != 0 {
			t.Errorf("%s carries the labels %v, want an aggregate of the cloud to carry none", name, labels)
		}
	}

	served := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, seriesNovaServerStatus+"{") && strings.Contains(line, `tenant_id="`) {
			served = true
			break
		}
	}
	if !served {
		t.Errorf("no %s line names a tenant_id, want one per server the month runs at %s",
			seriesNovaServerStatus, at.Format(time.RFC3339))
	}

	// The two labels a scrape job puts on every series it collects. An endpoint
	// that stated them itself would push the job's own under exported_cloud.
	for _, forbidden := range []string{"platform=", "cloud="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the scrape carries a %s label, want it from the scrape job alone",
				strings.TrimSuffix(forbidden, "="))
		}
	}
}

func TestExporterGathersConsistently(t *testing.T) {
	month := Month{
		Tenants: []Tenant{{ID: cloudTenant, Name: "tenant-a", Workload: workloadClassic}},
		Oracle: testOracle(
			oneInstance("srv-1", cloudDay(2), cloudDay(4)),
			oneResource(typeVolume, "vol-1", cloudDay(2), cloudDay(4), stateInUse,
				volumeSizeOf(&volume{sizeGB: 100, volumeType: "ssd"})),
		),
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(inventoryCollector{month: month, clock: NewClock(cloudDay(3), 0, time.Now)})

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v, want nil: a registry refuses a family whose members "+
			"disagree about their labels", err)
	}

	names := make([]string, 0, len(families))
	metrics := 0
	for _, family := range families {
		names = append(names, family.GetName())
		metrics += len(family.GetMetric())
	}
	if !slices.IsSorted(names) {
		t.Errorf("the families come back as %v, want them by name, which is the order a scrape reads",
			names)
	}
	// The instance states its status, the volume its status and its size, the
	// tenant its six limits and its project info, and the cloud its count of
	// projects together with the five aggregates.
	const want = 1 + 2 + 6 + 1 + 1 + 5
	if metrics != want {
		t.Errorf("the gather holds %d samples, want %d", metrics, want)
	}
	if len(families) != want {
		t.Errorf("the gather holds %d families, want %d, because this month states every series once",
			len(families), want)
	}
}

func TestExporterClampsPastTheMonth(t *testing.T) {
	month := faultyMonth(t, 1, Faults{})
	end := scrapeOf(t, month, month.Oracle.PeriodTo)
	past := scrapeOf(t, month, month.Oracle.PeriodTo.AddDate(0, 0, 1))

	if end == "" {
		t.Fatal("the last instant of the month scrapes empty, want the resources that outlived it")
	}
	if past != end {
		t.Error("a day past the month scrapes another document than its last instant, want the end held")
	}
}

func TestExporterAnswersBeforeTheMonth(t *testing.T) {
	month := faultyMonth(t, 1, Faults{})
	body := scrapeOf(t, month, month.Oracle.PeriodFrom.Add(-time.Hour))

	for _, name := range aggregateSeries {
		value, _ := scrapedGauge(t, body, name)
		if value != 0 {
			t.Errorf("%s = %g an hour before the month began, want 0", name, value)
		}
	}
	if strings.Contains(body, seriesNovaServerStatus) {
		t.Error("the scrape states a server before the month began, want none")
	}
}

func TestExporterAnswersOverAMalformedSize(t *testing.T) {
	month := Month{
		Tenants: []Tenant{{ID: cloudTenant, Name: "tenant-a", Workload: workloadClassic}},
		Oracle: testOracle(
			oneResource(typeInstance, "srv-1", cloudDay(2), cloudDay(4), stateActive,
				map[string]any{}),
			oneResource(typeVolume, "vol-1", cloudDay(2), cloudDay(4), stateInUse,
				map[string]any{"type": "ssd"}),
		),
	}
	body := scrapeOf(t, month, cloudDay(3))

	// A size member nobody stated costs its own series a value and nothing else:
	// the volume is still stated, at zero gibibytes.
	size, labels := scrapedGauge(t, body, seriesCinderVolumeGB)
	if size != 0 {
		t.Errorf("%s = %g for a volume whose size states no size_gb, want 0", seriesCinderVolumeGB, size)
	}
	if labels["id"] != "vol-1" {
		t.Errorf("%s names the volume %q, want %q", seriesCinderVolumeGB, labels["id"], "vol-1")
	}

	status, _ := scrapedGauge(t, body, seriesNovaServerStatus)
	if status != 1 {
		t.Errorf("%s = %g for a server whose size names no flavor, want the server stated all the same",
			seriesNovaServerStatus, status)
	}
}

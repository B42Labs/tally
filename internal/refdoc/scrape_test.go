package refdoc

import (
	"strings"
	"testing"
)

// realScrape is the scrape configuration this repository deploys, and the
// number of jobs a page states. A job added without a row is noticed here.
const (
	realScrape     = "../../deploy/kubernetes/base/victoriametrics/scrape.yaml"
	realScrapeJobs = 4
)

func TestScrapeJobs(t *testing.T) {
	got, err := ScrapeJobs(readFixture(t, "scrape.yaml"))
	if err != nil {
		t.Fatalf("ScrapeJobs() error = %v, want nil", err)
	}

	assertWant(t, "scrape.want.md", got)
}

func TestScrapeJobsRendersEachJobShape(t *testing.T) {
	got, err := ScrapeJobs(readFixture(t, "scrape.yaml"))
	if err != nil {
		t.Fatalf("ScrapeJobs() error = %v, want nil", err)
	}

	for _, want := range []string{
		// A job naming its targets carries the labels the file stamps on them.
		"| `fixture-exporter` | `300s` | `60s` | `fixture-exporter:9180` | " +
			"`cloud=os-prod-eu1`, `platform=openstack` |",
		// A job that discovers its targets names none, so what it keeps is the
		// cell instead.
		"| `fixture-api` | `30s` | none | discovered, role `endpointslice`, kept by `fixture-api;http` | none |",
		// A job that states no timeout and stamps no label.
		"| `fixture-gateway` | `60s` | none | `fixture-gateway:9101` | none |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

func TestScrapeJobsRendersTheJobsOfThisRepository(t *testing.T) {
	got, err := ScrapeJobs(readRepositoryFile(t, realScrape))
	if err != nil {
		t.Fatalf("ScrapeJobs() error = %v, want nil", err)
	}

	// The header and its separator are the two lines that are not a job.
	if n := countHeadings(got, "| ") - 2; n != realScrapeJobs {
		t.Errorf("the configuration rendered %d jobs, want %d:\n%s", n, realScrapeJobs, got)
	}
	for _, want := range []string{
		"| `reporting-api` | `30s` | none | discovered, role `endpointslice`, " +
			"kept by `reporting-api;http` | none |",
		"| `openstack-db-exporter` | `300s` | `60s` | `os-db-exporter:9180` | " +
			"`cloud=os-prod-eu1`, `platform=openstack` |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

func TestScrapeJobsRejectsWhatItCannotRender(t *testing.T) {
	cases := map[string]struct {
		cfg  string
		want string
	}{
		"no jobs at all":  {cfg: "global:\n  scrape_interval: 30s\n", want: "refdoc: no scrape jobs"},
		"an empty block":  {cfg: "scrape_configs: []\n", want: "refdoc: no scrape jobs"},
		"a null document": {cfg: "", want: "refdoc: no scrape jobs"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ScrapeJobs([]byte(tc.cfg))
			if err == nil {
				t.Fatalf("ScrapeJobs() error = nil, want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("ScrapeJobs() error = %q, want %q", err, tc.want)
			}
		})
	}
}

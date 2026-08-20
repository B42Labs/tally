// This file pins what turns a metric into a page. Every one of these fails
// quietly: a rule dropped in an edit leaves the condition it watched unwatched
// and nothing reports the gap, a runbook annotation naming a page that was
// renamed hands whoever is woken at 03:00 a dead link, and a scrape job added
// without a matching rule is a target nobody hears about once it stops
// answering. Whether the expressions parse is what `make check-alerting`
// answers, by loading them into the vmalert binary the cluster runs; this test
// reads the YAML from disk and needs no cluster.
package vmalert_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	rulesFile  = "rules.yaml"
	scrapeFile = "../victoriametrics/scrape.yaml"
	// The runbook annotations are written from the repository root, which is
	// four levels up from this directory.
	repoRoot = "../../../.."

	// The job TallyExporterServiceSilent selects, the pair the anomaly rule is
	// split into, and the rule that watches that pair.
	exporterJob       = "openstack-db-exporter"
	anomalyAlert      = "TallyResourceCountAnomaly"
	coverageAlert     = "TallyRecordedSeriesMissing"
	recordedSeries    = "tally:current_resources:sum"
	rawResourceSeries = "tally_current_resources"
)

// alerts is the rule set in the order rules.yaml carries it. The order is
// asserted rather than the set alone, so a rule that is moved out of the block
// its comment explains is noticed.
var alerts = []string{
	"TallyCloudEventsSilent",
	"TallyEventsRejected",
	"TallySyncErrors",
	"TallySyncStale",
	"TallyReconciliationDriftHigh",
	"TallyCollectorBufferAging",
	"TallyResourceCountAnomaly",
	"TallyRecordedSeriesMissing",
	"TallyScrapeTargetDown",
	"TallyScrapeJobMissing",
	"TallyExporterServiceSilent",
}

// criticalAlerts are the ones a deployment's Alertmanager repeats on the short
// interval and that carry a runbook. Severity decides what wakes a person, so
// which rules hold it is a decision of this repository rather than a detail.
var criticalAlerts = []string{
	"TallyCloudEventsSilent",
	"TallyExporterServiceSilent",
	"TallyScrapeJobMissing",
	"TallyScrapeTargetDown",
	"TallySyncStale",
}

// The headings a runbook answers with. A page that is linked from a critical
// alert and stops at a description leaves the reader where the alert did.
var runbookHeadings = []string{"## Symptom", "## Impact on billing", "## First checks"}

type ruleFile struct {
	Groups []group `yaml:"groups"`
}

type group struct {
	Name     string `yaml:"name"`
	Interval string `yaml:"interval"`
	Rules    []rule `yaml:"rules"`
}

// rule is either an alerting rule, which names an Alert, or a recording rule,
// which names a Record. Only one of the two is ever set.
type rule struct {
	Alert       string            `yaml:"alert"`
	Record      string            `yaml:"record"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

func TestRuleGroup(t *testing.T) {
	groups := rules(t)

	// vmalert evaluates every group on its own timer, and a second group would
	// evaluate on a timer this test says nothing about.
	if len(groups) != 1 {
		t.Fatalf("%s declares %d groups, want 1", rulesFile, len(groups))
	}
	if got := groups[0].Name; got != "tally" {
		t.Errorf("group name = %q, want %q", got, "tally")
	}
	if got := groups[0].Interval; got != "1m" {
		t.Errorf("group interval = %q, want %q; the interval is how long a condition may hold before the first evaluation sees it", got, "1m")
	}
}

func TestRulesAreTheOnesThisRepositoryDecidedOn(t *testing.T) {
	got := alertNames(t)

	if !slices.Equal(got, alerts) {
		t.Fatalf("%s carries\n  %v\nwant\n  %v\na rule removed here stops watching what it watched, and nothing else reports that it is gone",
			rulesFile, got, alerts)
	}
}

func TestEveryRuleTellsAReaderWhatHappened(t *testing.T) {
	severities := []string{"critical", "warning", "info"}

	for _, r := range alertRules(t) {
		// An unknown severity routes as if it had none: Alertmanager's tree
		// matches severity="critical" and falls through to the root for the
		// rest, so a typo turns a page into a four-hour repeat interval.
		if !slices.Contains(severities, r.Labels["severity"]) {
			t.Errorf("%s has severity %q, want one of %v; an unmatched severity takes the root route's repeat interval", r.Alert, r.Labels["severity"], severities)
		}
		// The summary is what a receiver renders. Without one, a notification
		// carries the alert name and the labels alone.
		if strings.TrimSpace(r.Annotations["summary"]) == "" {
			t.Errorf("%s carries no summary, so a notification of it says no more than its name", r.Alert)
		}
	}
}

func TestCriticalRules(t *testing.T) {
	all := alertRules(t)

	t.Run("are the ones that wake someone", func(t *testing.T) {
		var got []string
		for _, r := range all {
			if r.Labels["severity"] == "critical" {
				got = append(got, r.Alert)
			}
		}
		slices.Sort(got)

		if !slices.Equal(got, criticalAlerts) {
			t.Fatalf("critical rules are\n  %v\nwant\n  %v\nseverity is what Alertmanager's short repeat interval matches on",
				got, criticalAlerts)
		}
	})

	t.Run("link a runbook that answers", func(t *testing.T) {
		for _, r := range all {
			if r.Labels["severity"] != "critical" {
				continue
			}

			want := "docs/runbooks/" + r.Alert + ".md"
			got := r.Annotations["runbook"]
			if got != want {
				t.Errorf("%s links runbook %q, want %q", r.Alert, got, want)
				continue
			}

			path := filepath.Join(repoRoot, filepath.FromSlash(got))
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s links %s, which does not read, so the alert hands its reader a dead link: %v", r.Alert, path, err)
				continue
			}
			for _, heading := range runbookHeadings {
				if !strings.Contains(string(raw), heading) {
					t.Errorf("%s has no %q section, so the alert it answers for leaves the reader where it found them", got, heading)
				}
			}
		}
	})
}

func TestScrapeJobsAreCovered(t *testing.T) {
	// The three rules that name jobs rather than metrics. A job added to the
	// scrape config or renamed there is invisible to all of them until this file
	// names it too, and neither the scrape nor the rules fail on their own.
	var cfg struct {
		ScrapeConfigs []struct {
			JobName      string           `yaml:"job_name"`
			KubernetesSD []map[string]any `yaml:"kubernetes_sd_configs"`
		} `yaml:"scrape_configs"`
	}
	raw, err := os.ReadFile(scrapeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", scrapeFile, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parsing %s: %v", scrapeFile, err)
	}
	if len(cfg.ScrapeConfigs) == 0 {
		t.Fatalf("%s declares no scrape jobs, so this test would assert over nothing", scrapeFile)
	}

	byName := map[string]rule{}
	for _, r := range alertRules(t) {
		byName[r.Alert] = r
	}
	targetDown := byName["TallyScrapeTargetDown"].Expr
	jobMissing := byName["TallyScrapeJobMissing"].Expr
	exporterSilent := byName["TallyExporterServiceSilent"].Expr

	var declaresExporterJob bool
	for _, job := range cfg.ScrapeConfigs {
		if job.JobName == exporterJob {
			declaresExporterJob = true
		}
		if !strings.Contains(targetDown, job.JobName) {
			t.Errorf("TallyScrapeTargetDown does not name job %q of %s, so a target of it that stops answering is reported by nothing",
				job.JobName, scrapeFile)
		}
		if len(job.KubernetesSD) == 0 {
			continue
		}
		// A discovered job that resolves to no targets emits no up series at
		// all rather than up == 0, so TallyScrapeTargetDown stays silent and
		// absent() is what sees it.
		want := fmt.Sprintf("absent(up{job=%q})", job.JobName)
		if !strings.Contains(jobMissing, want) {
			t.Errorf("TallyScrapeJobMissing has no %s, so job %q resolving to zero targets is reported by nothing",
				want, job.JobName)
		}
	}

	// TallyExporterServiceSilent pins one job on both sides of every `unless`,
	// and it is the only rule watching for an exporter that answers a scrape
	// with a whole service missing. Renamed in the scrape config alone, that
	// selector matches no series: every branch has an empty left side, the `or`
	// chain returns nothing, and the rule can never fire again. It stays a legal
	// expression, so `make check-alerting` reports nothing either.
	selector := fmt.Sprintf("job=%q", exporterJob)
	if !strings.Contains(exporterSilent, selector) {
		t.Errorf("TallyExporterServiceSilent no longer selects %s, so this test asserts nothing about the job %s declares",
			selector, scrapeFile)
	}
	if !declaresExporterJob {
		t.Errorf("%s declares no job %q while TallyExporterServiceSilent selects it, so the rule matches no series and the short invoice it exists to prevent goes unreported",
			scrapeFile, exporterJob)
	}
}

// TestTheAnomalyRuleReadsARecordedSeries pins the pair that keeps the baseline
// off the raw series, and the third rule that watches the pair. Aggregated
// inline, the baseline made the store repeat that aggregation once per step of
// the seven-day window on every minute evaluation, against the single replica
// that also serves Grafana and the Reporting API. Reading the record instead put
// both sides of the comparison behind vmalert's -remoteWrite.url: a queue that
// overflows leaves a hole in the recorded series, the anomaly rule returns
// nothing on every evaluation, and legal, silent and never firing is what that
// looks like from outside.
func TestTheAnomalyRuleReadsARecordedSeries(t *testing.T) {
	var recorded, anomaly, coverage *rule
	all := allRules(t)
	for i := range all {
		switch {
		case all[i].Record == recordedSeries:
			recorded = &all[i]
		case all[i].Alert == anomalyAlert:
			anomaly = &all[i]
		case all[i].Alert == coverageAlert:
			coverage = &all[i]
		}
	}

	if recorded == nil {
		t.Fatalf("%s records no %s, so %s reads a series nothing writes and can never fire", rulesFile, recordedSeries, anomalyAlert)
	}
	if anomaly == nil {
		t.Fatalf("%s carries no %s", rulesFile, anomalyAlert)
	}
	if !strings.Contains(anomaly.Expr, recordedSeries) {
		t.Errorf("%s does not read %s:\n%s", anomalyAlert, recordedSeries, anomaly.Expr)
	}
	if strings.Contains(anomaly.Expr, rawResourceSeries) {
		t.Errorf("%s selects %s directly, so its baseline re-aggregates the raw series on every evaluation instead of reading the recorded one:\n%s",
			anomalyAlert, rawResourceSeries, anomaly.Expr)
	}

	// Nothing else in this group reads the write path vmalert records through,
	// so without this rule a stalled remote-write queue takes the anomaly rule
	// off the air and no alert, no `for` timer and no log line says so.
	if coverage == nil {
		t.Fatalf("%s carries no %s, so a hole in %s silently stops %s from evaluating", rulesFile, coverageAlert, recordedSeries, anomalyAlert)
	}
	if want := "absent(" + recordedSeries + ")"; !strings.Contains(coverage.Expr, want) {
		t.Errorf("%s does not carry %s, so it no longer reports the recorded series going missing:\n%s", coverageAlert, want, coverage.Expr)
	}
	// The second clause. absent() alone fires wherever nothing writes the raw
	// series either, which is every dev cluster and every production one before
	// its first reconciliation run.
	if !strings.Contains(coverage.Expr, rawResourceSeries) {
		t.Errorf("%s does not pair its absent() with %s, so it fires on any cluster that has not reconciled yet rather than on a stalled write path:\n%s",
			coverageAlert, rawResourceSeries, coverage.Expr)
	}
}

// rules parses the groups vmalert evaluates.
func rules(t *testing.T) []group {
	t.Helper()

	raw, err := os.ReadFile(rulesFile)
	if err != nil {
		t.Fatalf("reading %s: %v", rulesFile, err)
	}

	var file ruleFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parsing %s, which vmalert refuses to start on: %v", rulesFile, err)
	}
	return file.Groups
}

// allRules returns the rules of the one group, in file order.
func allRules(t *testing.T) []rule {
	t.Helper()

	groups := rules(t)
	if len(groups) != 1 {
		t.Fatalf("%s declares %d groups, want 1", rulesFile, len(groups))
	}
	return groups[0].Rules
}

// alertRules returns the alerting rules of the one group, in file order. A
// recording rule carries no severity, no summary and no runbook, so the
// assertions over those read this rather than allRules.
func alertRules(t *testing.T) []rule {
	t.Helper()

	var alerting []rule
	for _, r := range allRules(t) {
		if r.Alert != "" {
			alerting = append(alerting, r)
		}
	}
	return alerting
}

// alertNames returns the alert of every alerting rule, in file order.
func alertNames(t *testing.T) []string {
	t.Helper()

	var names []string
	for _, r := range alertRules(t) {
		names = append(names, r.Alert)
	}
	return names
}

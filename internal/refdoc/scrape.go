package refdoc

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"gopkg.in/yaml.v3"
)

// keepAction is the relabel action that decides which discovered target a job
// keeps.
const keepAction = "keep"

// scrapeFile is the part of a scrape configuration a page reads.
type scrapeFile struct {
	ScrapeConfigs []scrapeJob `yaml:"scrape_configs"`
}

// scrapeJob is one job: how often it is scraped, how long one scrape may cost,
// and how it arrives at its targets.
type scrapeJob struct {
	JobName        string             `yaml:"job_name"`
	ScrapeInterval string             `yaml:"scrape_interval"`
	ScrapeTimeout  string             `yaml:"scrape_timeout"`
	StaticConfigs  []staticConfig     `yaml:"static_configs"`
	KubernetesSD   []serviceDiscovery `yaml:"kubernetes_sd_configs"`
	RelabelConfigs []relabelRule      `yaml:"relabel_configs"`
}

// staticConfig is a set of targets named in the file, with the labels every
// sample of them carries.
type staticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels"`
}

// serviceDiscovery is what a job discovers its targets from.
type serviceDiscovery struct {
	Role string `yaml:"role"`
}

// relabelRule is one rule over a discovered target.
type relabelRule struct {
	Action string `yaml:"action"`
	Regex  string `yaml:"regex"`
}

// ScrapeJobs renders the jobs a store scrapes: one row per job in the order the
// file writes them, with what it scrapes and how often.
//
// A job that discovers its targets names none, so the cell states what it
// discovers and which of the discovered targets it keeps. That is the whole
// difference between a job that goes silent as one target and one that goes
// silent as no target at all.
func ScrapeJobs(cfg []byte) (string, error) {
	var file scrapeFile
	if err := yaml.Unmarshal(cfg, &file); err != nil {
		return "", fmt.Errorf("refdoc: scrape: %w", err)
	}
	if len(file.ScrapeConfigs) == 0 {
		return "", errors.New("refdoc: no scrape jobs")
	}

	rows := make([][]string, 0, len(file.ScrapeConfigs))
	for _, job := range file.ScrapeConfigs {
		rows = append(rows, []string{
			code(job.JobName),
			codeOrNone(job.ScrapeInterval),
			codeOrNone(job.ScrapeTimeout),
			targetCell(job),
			staticLabelCell(job),
		})
	}
	return table([]string{"Job", "Interval", "Timeout", "Targets", "Static labels"}, rows), nil
}

// targetCell names what a job scrapes.
func targetCell(job scrapeJob) string {
	var targets []string
	for _, static := range job.StaticConfigs {
		targets = append(targets, static.Targets...)
	}
	if len(targets) > 0 {
		return codeSpans(targets, ", ")
	}
	if len(job.KubernetesSD) == 0 {
		return "none"
	}

	discovered := "discovered, role " + code(job.KubernetesSD[0].Role)
	for _, rule := range job.RelabelConfigs {
		if rule.Action == keepAction {
			return discovered + ", kept by " + code(rule.Regex)
		}
	}
	return discovered
}

// staticLabelCell lists the labels the file stamps on every sample of the job,
// in key order.
func staticLabelCell(job scrapeJob) string {
	labels := map[string]string{}
	for _, static := range job.StaticConfigs {
		maps.Copy(labels, static.Labels)
	}

	pairs := make([]string, 0, len(labels))
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		pairs = append(pairs, key+"="+labels[key])
	}
	return codeSpans(pairs, ", ")
}

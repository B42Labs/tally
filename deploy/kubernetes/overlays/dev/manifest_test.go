// This file pins the two files this overlay adds to the metrics pipeline, and
// the wiring that carries them into the cluster. Every mismatch it looks for
// fails quietly. A scrape config that dropped or renamed a job of the base
// leaves TallyScrapeTargetDown, TallyScrapeJobMissing and
// TallyExporterServiceSilent selecting jobs this cluster no longer scrapes, and
// neither the scrape nor the rules fail on their own. An exporter job on
// another address or under another cloud label scrapes nothing while the
// inventory of the simulated month sits unread. A counter sources file the
// engine refuses to load fails every hourly tick before it opens a database,
// and a patch whose variable and mount path disagree does the same on a file
// the pod never carried. The tests read the YAML from disk and need neither a
// cluster nor kustomize.
package dev_test

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/b42labs/tally/internal/engine/counters"
)

const (
	scrapeFile        = "victoriametrics/scrape.yaml"
	baseScrapeFile    = "../../base/victoriametrics/scrape.yaml"
	sourcesFile       = "counter-sources.yaml"
	kustomizationFile = "kustomization.yaml"

	// The two OpenStack jobs: the one this overlay repoints and the one it
	// copies over untouched.
	exporterJob   = "openstack-db-exporter"
	ceilometerJob = "ceilometer"

	// Where the compose stack publishes the simulator, as a pod in the kind
	// node reaches it, and the cloud the simulated month is booked under.
	simulatorTarget = "host.docker.internal:8091"
	simulatedCloud  = "os-sim"

	// The generated ConfigMaps, by their unsuffixed names, and the CronJob the
	// second one is mounted into, which carries a container of the same name.
	scrapeConfigMap  = "victoriametrics-scrape"
	sourcesConfigMap = "tally-counter-sources"
	cronJob          = "tally-engine"

	// What points the engine at the mounted sources file.
	sourcesVariable = "TALLY_ENGINE_COUNTER_SOURCES"
)

// scrapeConfig is the part of a scrape file these tests assert over. yaml.v3
// ignores every field not named here, so the discovered jobs decode to their
// names alone.
type scrapeConfig struct {
	ScrapeConfigs []scrapeJob `yaml:"scrape_configs"`
}

type scrapeJob struct {
	JobName        string         `yaml:"job_name"`
	ScrapeInterval string         `yaml:"scrape_interval"`
	ScrapeTimeout  string         `yaml:"scrape_timeout"`
	StaticConfigs  []staticConfig `yaml:"static_configs"`
}

type staticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels"`
}

// kustomization is the part of the overlay these tests assert over: what it
// generates and what it patches.
type kustomization struct {
	ConfigMapGenerator []generator `yaml:"configMapGenerator"`
	Patches            []struct {
		Patch string `yaml:"patch"`
	} `yaml:"patches"`
}

type generator struct {
	Name     string   `yaml:"name"`
	Behavior string   `yaml:"behavior"`
	Files    []string `yaml:"files"`
}

// cronJobPatch is one strategic merge patch against the scheduler CronJob.
type cronJobPatch struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		JobTemplate struct {
			Spec struct {
				Template struct {
					Spec struct {
						Containers []container `yaml:"containers"`
						Volumes    []volume    `yaml:"volumes"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"jobTemplate"`
	} `yaml:"spec"`
}

type container struct {
	Name         string        `yaml:"name"`
	Env          []envVar      `yaml:"env"`
	VolumeMounts []volumeMount `yaml:"volumeMounts"`
}

type envVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type volumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	ReadOnly  bool   `yaml:"readOnly"`
}

type volume struct {
	Name      string `yaml:"name"`
	ConfigMap struct {
		Name string `yaml:"name"`
	} `yaml:"configMap"`
}

func TestScrapeConfigKeepsTheBaseJobs(t *testing.T) {
	// Three alerting rules name the scrape jobs rather than a metric, and
	// deploy/kubernetes/base/vmalert/rules_test.go holds them to the base file.
	// This overlay replaces that file wholesale, so a job it drops or renames
	// takes itself out of all three selectors while both files stay legal YAML
	// and the cluster keeps scraping the rest.
	overlay := jobNames(scrapeConfigOf(t, scrapeFile))
	base := jobNames(scrapeConfigOf(t, baseScrapeFile))

	if !slices.Equal(overlay, base) {
		t.Fatalf("%s declares jobs %v, want %v, the jobs of %s that TallyScrapeTargetDown, TallyScrapeJobMissing and TallyExporterServiceSilent select",
			scrapeFile, overlay, base, baseScrapeFile)
	}
}

func TestExporterJobScrapesTheSimulator(t *testing.T) {
	// The one change this file makes against the base. The target is where a
	// pod in the kind node reaches the simulator the compose stack publishes on
	// the host, and the cloud label is what the simulated month's events are
	// booked under: another value leaves the inventory series under a cloud no
	// metering row meets. A scrape of a port nothing listens on is a job that
	// TallyScrapeTargetDown reports, which is also what it does between two
	// drill runs, so neither mistake shows up as anything else.
	overlay := scrapeConfigOf(t, scrapeFile)
	base := scrapeConfigOf(t, baseScrapeFile)

	exporter := jobNamed(t, overlay, scrapeFile, exporterJob)
	want := []staticConfig{{
		Targets: []string{simulatorTarget},
		Labels:  map[string]string{"platform": "openstack", "cloud": simulatedCloud},
	}}
	if !sameStaticConfigs(exporter.StaticConfigs, want) {
		t.Errorf("job %s scrapes %+v, want %+v, which is the simulator the compose stack publishes on the host",
			exporterJob, exporter.StaticConfigs, want)
	}

	// One scrape of this exporter runs a whole query set against the service
	// databases of a real cloud, which is what the base's interval and timeout
	// are cut for. Shortening them here would leave the dev cluster testing a
	// pacing no deployment runs.
	baseExporter := jobNamed(t, base, baseScrapeFile, exporterJob)
	if exporter.ScrapeInterval != baseExporter.ScrapeInterval || exporter.ScrapeTimeout != baseExporter.ScrapeTimeout {
		t.Errorf("job %s is scraped every %q with a %q timeout, want the base's %q and %q",
			exporterJob, exporter.ScrapeInterval, exporter.ScrapeTimeout,
			baseExporter.ScrapeInterval, baseExporter.ScrapeTimeout)
	}

	// The traffic of a simulated month reaches the store over OTLP, so this job
	// has nothing to point at here and keeps the base's placeholder. Pointed at
	// the simulator too, it would scrape the inventory a second time under a
	// second job name.
	ceilometer := jobNamed(t, overlay, scrapeFile, ceilometerJob)
	baseCeilometer := jobNamed(t, base, baseScrapeFile, ceilometerJob)
	if !sameStaticConfigs(ceilometer.StaticConfigs, baseCeilometer.StaticConfigs) {
		t.Errorf("job %s scrapes %+v, want the base's %+v: the simulator's traffic counters are pushed over OTLP, not scraped",
			ceilometerJob, ceilometer.StaticConfigs, baseCeilometer.StaticConfigs)
	}
}

func TestCounterSourcesMeasureTheSimulatedEgress(t *testing.T) {
	// The engine reads this file the way Load reads a deployment's, so a source
	// it refuses is an hourly tick that fails before it opens a database. What
	// the query has to carry beyond that is the series the simulator pushes,
	// the three placeholders that narrow it to one resource and one interval,
	// and the divisor that leaves the billed quantity in the unit the oracle
	// states its bytes in.
	cfg, err := counters.Load(sourcesFile)
	if err != nil {
		t.Fatalf("loading %s, which every tick of this cluster does: %v", sourcesFile, err)
	}
	if len(cfg.Sources) != 1 {
		t.Fatalf("%s holds %d sources, want the one that measures the simulated egress", sourcesFile, len(cfg.Sources))
	}

	s := cfg.Sources[0]
	if s.Platform != "openstack" || s.ResourceType != "instance" || s.Metric != "egress_gb" {
		t.Errorf("the source measures %s of %s/%s, want egress_gb of openstack/instance, which is the usage key the export prices",
			s.Metric, s.Platform, s.ResourceType)
	}
	if s.Kind != counters.KindMetricsQL {
		t.Errorf("the source is a %q source, want %q: the egress is a series in the store, not an event in the reporting database",
			s.Kind, counters.KindMetricsQL)
	}
	// A dev cluster where no month was ever simulated holds none of this
	// series. Required, the source would fail every scheduled tick with it;
	// optional, it yields a counter_source_failed warning on a run that
	// finished.
	if s.Required {
		t.Error("the source is required, so a cluster that has never run a drill fails every hourly tick on a series nothing wrote")
	}

	for _, want := range []string{
		"ceilometer_network_outgoing_bytes_total",
		"{cloud}", "{resource_id}", "{window}",
		"1024 * 1024 * 1024",
	} {
		if !strings.Contains(s.Query, want) {
			t.Errorf("the query does not carry %q:\n%s", want, s.Query)
		}
	}
	// An identity read as a pattern selects the series of the resources beside
	// the one it names, and an aggregate over them bills this resource for
	// theirs. Load refuses it, which the case below pins; the query itself may
	// not get there in the first place.
	if strings.Contains(s.Query, "=~") {
		t.Errorf("the query matches with =~, which reads a substituted identity as a pattern:\n%s", s.Query)
	}
}

func TestARegexMatchedIdentityIsRefused(t *testing.T) {
	// What keeps the assertion above worth making: the loader, not this file,
	// is what stops a query from reading a resource id as a pattern. A copy of
	// the file with the one matcher loosened has to fail to load, or an edit
	// that loosens the real one would reach a tick.
	raw, err := os.ReadFile(sourcesFile)
	if err != nil {
		t.Fatalf("reading %s: %v", sourcesFile, err)
	}
	const matcher = `resource_id="{resource_id}"`
	if n := strings.Count(string(raw), matcher); n != 1 {
		t.Fatalf("%s carries %d occurrences of %s, so this case would loosen something else", sourcesFile, n, matcher)
	}
	loosened := strings.Replace(string(raw), matcher, `resource_id=~"{resource_id}"`, 1)

	path := filepath.Join(t.TempDir(), sourcesFile)
	if err := os.WriteFile(path, []byte(loosened), 0o600); err != nil {
		t.Fatalf("writing the loosened copy: %v", err)
	}
	if _, err := counters.Load(path); err == nil {
		t.Fatal("counters.Load accepted a query matching {resource_id} with =~, so nothing but review stops an aggregate over the series of the resources beside the one it names")
	}
}

func TestKustomizationWiresTheTwoConfigMaps(t *testing.T) {
	// Neither file reaches the cluster on its own. Without the replacing
	// generator VictoriaMetrics keeps serving the base's placeholders, and
	// without the patch the engine keeps the empty path the base sets, which is
	// a tick that measures no counter and reports nothing about it.
	var k kustomization
	raw, err := os.ReadFile(kustomizationFile)
	if err != nil {
		t.Fatalf("reading %s: %v", kustomizationFile, err)
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parsing %s, which kustomize refuses to build: %v", kustomizationFile, err)
	}

	scrape := generatorNamed(t, k, scrapeConfigMap)
	if scrape.Behavior != "replace" {
		t.Errorf("the %s generator has behavior %q, want \"replace\": that is what overrides the base's generated ConfigMap rather than adding a second one under the same name",
			scrapeConfigMap, scrape.Behavior)
	}
	if !slices.Contains(scrape.Files, scrapeFile) {
		t.Errorf("the %s generator carries %v rather than %s, so the base's placeholder targets stay in the store's scrape config",
			scrapeConfigMap, scrape.Files, scrapeFile)
	}
	if sources := generatorNamed(t, k, sourcesConfigMap); !slices.Contains(sources.Files, sourcesFile) {
		t.Errorf("the %s generator carries %v rather than %s, so the mount below projects a ConfigMap without the sources file",
			sourcesConfigMap, sources.Files, sourcesFile)
	}

	// Three things have to agree: the volume names the generated ConfigMap, the
	// mount puts it at a path, and the variable names the file inside that
	// path. A mismatch in any of them leaves the tick failing on a file it
	// cannot read, once an hour, in a Job history nobody reads.
	patch := cronJobPatchOf(t, k)
	c := containerNamed(t, patch, cronJob)

	var name string
	for _, v := range patch.Spec.JobTemplate.Spec.Template.Spec.Volumes {
		if v.ConfigMap.Name == sourcesConfigMap {
			name = v.Name
			break
		}
	}
	if name == "" {
		t.Fatalf("the patch declares no volume backed by ConfigMap %s, so the sources file reaches no container", sourcesConfigMap)
	}

	i := slices.IndexFunc(c.VolumeMounts, func(m volumeMount) bool { return m.Name == name })
	if i < 0 {
		t.Fatalf("container %s carries %v rather than a mount of volume %q, so %s points at nothing",
			cronJob, c.VolumeMounts, name, sourcesVariable)
	}
	mount := c.VolumeMounts[i]
	if !mount.ReadOnly {
		t.Errorf("the %s mount is writable, although the tick only reads it", name)
	}

	value, set := envValue(c, sourcesVariable)
	if !set {
		t.Fatalf("the patch sets no %s, so the engine keeps the empty path the base carries and measures no counter", sourcesVariable)
	}
	if want := mount.MountPath + "/" + sourcesFile; value != want {
		t.Errorf("%s = %q, want %q, which is where volume %q mounts the generated ConfigMap", sourcesVariable, value, want, name)
	}
}

// scrapeConfigOf decodes one scrape file.
func scrapeConfigOf(t *testing.T, path string) scrapeConfig {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var cfg scrapeConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	if len(cfg.ScrapeConfigs) == 0 {
		t.Fatalf("%s declares no scrape jobs, so this test would assert over nothing", path)
	}
	return cfg
}

// jobNames is the jobs of a scrape file in the order it declares them. The
// order is asserted rather than the set alone, so a job moved out of the block
// its comment explains is noticed.
func jobNames(cfg scrapeConfig) []string {
	names := make([]string, 0, len(cfg.ScrapeConfigs))
	for _, job := range cfg.ScrapeConfigs {
		names = append(names, job.JobName)
	}
	return names
}

// jobNamed returns one job, failing if it is missing rather than asserting over
// a zero value.
func jobNamed(t *testing.T, cfg scrapeConfig, path, name string) scrapeJob {
	t.Helper()

	for _, job := range cfg.ScrapeConfigs {
		if job.JobName == name {
			return job
		}
	}
	t.Fatalf("%s declares no job %q", path, name)
	return scrapeJob{}
}

// sameStaticConfigs reports whether two job's static targets and labels match.
func sameStaticConfigs(a, b []staticConfig) bool {
	return slices.EqualFunc(a, b, func(x, y staticConfig) bool {
		return slices.Equal(x.Targets, y.Targets) && maps.Equal(x.Labels, y.Labels)
	})
}

// generatorNamed returns one ConfigMap generator of the overlay.
func generatorNamed(t *testing.T, k kustomization, name string) generator {
	t.Helper()

	for _, g := range k.ConfigMapGenerator {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("%s generates no ConfigMap %q", kustomizationFile, name)
	return generator{}
}

// cronJobPatchOf returns the one strategic merge patch against the scheduler.
// The other entries of patches are JSON patches, which are sequences rather
// than documents and are skipped by their shape.
func cronJobPatchOf(t *testing.T, k kustomization) cronJobPatch {
	t.Helper()

	var found []cronJobPatch
	for i, p := range k.Patches {
		var shape any
		if err := yaml.Unmarshal([]byte(p.Patch), &shape); err != nil {
			t.Fatalf("parsing patches[%d]: %v", i, err)
		}
		if _, isDocument := shape.(map[string]any); !isDocument {
			continue
		}
		var doc cronJobPatch
		if err := yaml.Unmarshal([]byte(p.Patch), &doc); err != nil {
			t.Fatalf("parsing patches[%d]: %v", i, err)
		}
		if doc.Kind == "CronJob" && doc.Metadata.Name == cronJob {
			found = append(found, doc)
		}
	}

	if len(found) != 1 {
		t.Fatalf("%s holds %d strategic merge patches against CronJob %s, want one: two of them would leave which env list is merged to the order of the file",
			kustomizationFile, len(found), cronJob)
	}
	return found[0]
}

// containerNamed returns one container of a patched pod. The name is what
// strategic merge matches on, so a patch naming a container the base does not
// have adds a second one instead of setting anything.
func containerNamed(t *testing.T, patch cronJobPatch, name string) container {
	t.Helper()

	for _, c := range patch.Spec.JobTemplate.Spec.Template.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the patch against CronJob %s carries no container %q", patch.Metadata.Name, name)
	return container{}
}

// envValue reports what one variable of a container is set to, and whether the
// patch names it at all.
func envValue(c container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

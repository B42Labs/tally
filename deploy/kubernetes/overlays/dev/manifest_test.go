// This file pins the three files this overlay adds to the metrics pipeline,
// and the wiring that carries them into the cluster. Every mismatch it looks
// for fails quietly. A scrape config that dropped or renamed a job of the base
// leaves TallyExporterServiceSilent selecting a job this cluster no longer
// scrapes, and a rules file whose absent list is not the scrape config's
// discovered jobs leaves TallyScrapeJobMissing firing for a job nobody
// configured or silent on one that resolves to nothing; neither the scrape nor
// the rules fail on their own. An exporter job on
// another address or under another cloud label scrapes nothing while the
// inventory of the simulated month sits unread, and an address that is not the
// simulator's alias in the compose stack is one no pod resolves. A counter
// sources file the engine refuses to load fails every hourly tick before it
// opens a database, and a patch whose variable and mount path disagree does the
// same on a file the pod never carried. A Reporting API without the settle
// window set to 0 keeps the default of 60 seconds, measured against the instant
// a sync of the simulated month is told, and defers the month's last minute on
// every run. The tests read the YAML from disk and need neither a cluster nor
// kustomize.
package dev_test

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/providers/openstack/simulator"
)

const (
	scrapeFile        = "victoriametrics/scrape.yaml"
	scrapeRulesFile   = "victoriametrics/scrape-rules.yaml"
	baseScrapeFile    = "../../base/victoriametrics/scrape.yaml"
	sourcesFile       = "counter-sources.yaml"
	kustomizationFile = "kustomization.yaml"

	// The base's rule over its discovered jobs, which this overlay's differs
	// from in the absent list alone.
	baseScrapeRulesFile = "../../base/vmalert/scrape-rules.yaml"

	// The two OpenStack jobs: the one this overlay repoints and the one it
	// copies over untouched.
	exporterJob   = "openstack-db-exporter"
	ceilometerJob = "ceilometer"

	// The simulator as a pod in the kind node reaches it, by its alias on the
	// kind network and its container port, and the cloud the simulated month is
	// booked under.
	simulatorTarget = "tally-openstack-simulator:8080"
	simulatedCloud  = "os-sim"

	// The compose stack that gives the simulator its alias, the network and the
	// service that carry it, and the clouds.yaml the Reporting API authenticates
	// against the simulated cloud with.
	composeFile      = "../../../compose/compose.yaml"
	kindNetwork      = "kind"
	simulatorService = "simulator"
	cloudsFile       = "reconciliation/clouds.yaml"

	// The generated ConfigMaps, by their unsuffixed names, and the CronJob the
	// last one is mounted into, which carries a container of the same name.
	scrapeConfigMap      = "victoriametrics-scrape"
	scrapeRulesConfigMap = "vmalert-scrape-rules"
	sourcesConfigMap     = "tally-counter-sources"
	cronJob              = "tally-engine"

	// The one rule the overlay's rules file carries.
	jobMissingAlert = "TallyScrapeJobMissing"

	// What points the engine at the mounted sources file.
	sourcesVariable = "TALLY_ENGINE_COUNTER_SOURCES"

	// The Reporting API Deployment, which carries a container of the same name,
	// and the variable that sets its settle window.
	reportingDeployment = "reporting-api"
	settleVariable      = "TALLY_REPORTING_SYNC_SETTLE_S"

	// The kustomize component that declares what the stack needs Envoy Gateway
	// for, as the overlay lists it.
	envoyGatewayComponent = "../../components/envoy-gateway"
)

// scrapeConfig is the part of a scrape file these tests assert over. yaml.v3
// ignores every field not named here, so the discovered jobs decode to their
// names alone.
type scrapeConfig struct {
	ScrapeConfigs []scrapeJob `yaml:"scrape_configs"`
}

type scrapeJob struct {
	JobName        string           `yaml:"job_name"`
	ScrapeInterval string           `yaml:"scrape_interval"`
	ScrapeTimeout  string           `yaml:"scrape_timeout"`
	StaticConfigs  []staticConfig   `yaml:"static_configs"`
	KubernetesSD   []map[string]any `yaml:"kubernetes_sd_configs"`
}

type staticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels"`
}

// ruleFile is the part of a vmalert rules file these tests assert over.
type ruleFile struct {
	Groups []struct {
		Rules []struct {
			Alert string `yaml:"alert"`
			Expr  string `yaml:"expr"`
		} `yaml:"rules"`
	} `yaml:"groups"`
}

// absentJobRe finds every job an absent() clause over up names.
var absentJobRe = regexp.MustCompile(`absent\(up\{job="([^"]+)"\}\)`)

// kustomization is the part of the overlay these tests assert over: the
// components it lists, what it generates and what it patches.
type kustomization struct {
	Components         []string    `yaml:"components"`
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

// deploymentPatch is one strategic merge patch against a Deployment.
type deploymentPatch struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []container `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
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
	// TallyExporterServiceSilent names a scrape job rather than a metric, and
	// deploy/kubernetes/base/vmalert/rules_test.go holds it to the base file.
	// This overlay replaces that file wholesale, so a job it drops or renames
	// takes itself out of the selector while both files stay legal YAML and the
	// cluster keeps scraping the rest. The absent list of TallyScrapeJobMissing
	// is the overlay's own, which the test below holds to this file.
	overlay := jobNames(scrapeConfigOf(t, scrapeFile))
	base := jobNames(scrapeConfigOf(t, baseScrapeFile))

	if !slices.Equal(overlay, base) {
		t.Fatalf("%s declares jobs %v, want %v, the jobs of %s the base's rules are written against",
			scrapeFile, overlay, base, baseScrapeFile)
	}
}

func TestScrapeRulesNameTheDiscoveredJobsOfThisOverlay(t *testing.T) {
	// The absent list of TallyScrapeJobMissing is this cluster's list of
	// discovered jobs, and the overlay replaces the base's with it. A clause
	// for a job the scrape config does not discover fires for as long as the
	// cluster runs, and a discovered job without one resolves to no targets
	// unheard.
	var rules ruleFile
	decodeFile(t, scrapeRulesFile, &rules)
	if len(rules.Groups) != 1 || len(rules.Groups[0].Rules) != 1 || rules.Groups[0].Rules[0].Alert != jobMissingAlert {
		t.Fatalf("%s carries %+v, want one group with the one alerting rule %s", scrapeRulesFile, rules.Groups, jobMissingAlert)
	}

	var named []string
	for _, match := range absentJobRe.FindAllStringSubmatch(rules.Groups[0].Rules[0].Expr, -1) {
		named = append(named, match[1])
	}
	var discovered []string
	for _, job := range scrapeConfigOf(t, scrapeFile).ScrapeConfigs {
		if len(job.KubernetesSD) > 0 {
			discovered = append(discovered, job.JobName)
		}
	}
	for _, job := range named {
		if !slices.Contains(discovered, job) {
			t.Errorf("%s carries absent(up{job=%q}), a job %s does not discover, so %s fires for as long as the cluster runs",
				scrapeRulesFile, job, scrapeFile, jobMissingAlert)
		}
	}
	for _, job := range discovered {
		if !slices.Contains(named, job) {
			t.Errorf("%s carries no absent(up{job=%q}), so the discovered job resolving to no targets is reported by nothing",
				scrapeRulesFile, job)
		}
	}
	if !slices.Equal(named, discovered) {
		t.Errorf("%s names the jobs %v, want %v, the discovered jobs of %s in its order", scrapeRulesFile, named, discovered, scrapeFile)
	}

	// Without the replacing generator vmalert keeps the base's file, whose
	// absent list is the base's scrape config's.
	g := generatorNamed(t, kustomizationOf(t), scrapeRulesConfigMap)
	if g.Behavior != "replace" {
		t.Errorf("the %s generator has behavior %q, want \"replace\": that is what overrides the base's generated ConfigMap",
			scrapeRulesConfigMap, g.Behavior)
	}
	if !slices.Equal(g.Files, []string{scrapeRulesFile}) {
		t.Errorf("the %s generator carries %v, want exactly [%s]", scrapeRulesConfigMap, g.Files, scrapeRulesFile)
	}
}

func TestScrapeRulesKeepTheBaseRuleBesideTheAbsentList(t *testing.T) {
	// The absent list is the one part of the rule that is this cluster's. The
	// group, the for, the severity and the annotations are the base's, which
	// deploy/kubernetes/base/vmalert/rules_test.go pins, and a copy that drifts
	// from them pages on the dev cluster in a way the base does not: a dropped
	// for fires on the first evaluation, a warning severity takes the root
	// route's repeat interval, and a renamed runbook is a dead link. The test
	// above reads the absent clauses alone, and `make check-alerting` loads
	// this file into vmalert, which is what says the expression parses.
	overlay := linesBesideTheAbsentList(t, scrapeRulesFile)
	base := linesBesideTheAbsentList(t, baseScrapeRulesFile)

	if !slices.Equal(overlay, base) {
		t.Errorf("%s differs from %s beside its expr line:\n%s\nwant\n%s",
			scrapeRulesFile, baseScrapeRulesFile, strings.Join(overlay, "\n"), strings.Join(base, "\n"))
	}
}

func TestExporterJobScrapesTheSimulator(t *testing.T) {
	// The one change this file makes against the base. The target is where a
	// pod in the kind node reaches the simulator of the compose stack, and the
	// cloud label is what the simulated month's events are booked under:
	// another value leaves the inventory series under a cloud no metering row
	// meets. A scrape of an address nothing answers on is a job that
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
		t.Errorf("job %s scrapes %+v, want %+v, which is the simulator by its alias on the kind network",
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

func TestSimulatorAddressIsTheComposeAlias(t *testing.T) {
	// The scrape job and the clouds.yaml address the simulator by the alias the
	// compose stack gives it on the kind network, on the port it listens on in
	// its container. Any other name is one no pod resolves, and any other port
	// is one nothing listens on: the job stays down and every sync fails to
	// authenticate, which is what a dev cluster shows between two drills too.
	var stack struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
			Networks    map[string]struct {
				Aliases []string `yaml:"aliases"`
			} `yaml:"networks"`
		} `yaml:"services"`
	}
	decodeFile(t, composeFile, &stack)

	sim, ok := stack.Services[simulatorService]
	if !ok {
		t.Fatalf("%s declares no service %q", composeFile, simulatorService)
	}
	aliases := sim.Networks[kindNetwork].Aliases
	if len(aliases) != 1 {
		t.Fatalf("%s gives %s the aliases %v on %s, want exactly one", composeFile, simulatorService, aliases, kindNetwork)
	}
	if got := aliases[0] + ":" + strconv.Itoa(simulatorListener(t, sim.Environment).HTTPPort); got != simulatorTarget {
		t.Errorf("%s gives %s the address %s on %s, want %s, which the %s job scrapes",
			composeFile, simulatorService, got, kindNetwork, simulatorTarget, exporterJob)
	}

	var clouds struct {
		Clouds map[string]struct {
			Auth struct {
				AuthURL string `yaml:"auth_url"`
			} `yaml:"auth"`
		} `yaml:"clouds"`
	}
	decodeFile(t, cloudsFile, &clouds)
	if got, want := clouds.Clouds[simulatedCloud].Auth.AuthURL, "http://"+simulatorTarget+"/v3"; got != want {
		t.Errorf("%s authenticates %s at %q, want %q, the simulator's identity endpoint at the address the job scrapes",
			cloudsFile, simulatedCloud, got, want)
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
	k := kustomizationOf(t)

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

func TestTheSettleWindowIsOffOnTheReportingAPI(t *testing.T) {
	patch := deploymentPatchOf(t, kustomizationOf(t), reportingDeployment)
	containers := patch.Spec.Template.Spec.Containers
	i := slices.IndexFunc(containers, func(c container) bool { return c.Name == reportingDeployment })
	if i < 0 {
		t.Fatalf("the patch against Deployment %s carries no container %q", reportingDeployment, reportingDeployment)
	}

	value, set := envValue(containers[i], settleVariable)
	if !set || value != "0" {
		t.Errorf("%s = %q (set: %t), want \"0\": the default of 60 seconds is measured against the instant a sync of the simulated month is told, and a loop holding that instant at period_to defers the month's last minute on every run",
			settleVariable, value, set)
	}
}

func TestTheEnvoyGatewayComponentIsListed(t *testing.T) {
	// The base is plain Gateway API and names no implementation. The
	// GatewayClass bound to Envoy Gateway's controller, the rate limit on the
	// OTLP routes and the two 403 answers are the component's, and the dev
	// stack runs Envoy Gateway. An entry lost in an edit is not an error to
	// kustomize: the overlay renders, and the Gateway names a class nobody
	// declares.
	k := kustomizationOf(t)

	if want := []string{envoyGatewayComponent}; !slices.Equal(k.Components, want) {
		t.Errorf("%s lists the components %v, want %v; without the entry the cluster has no GatewayClass and no rate limit, and the overlay still renders",
			kustomizationFile, k.Components, want)
	}
}

// kustomizationOf decodes the overlay's kustomization.yaml.
func kustomizationOf(t *testing.T) kustomization {
	t.Helper()

	raw, err := os.ReadFile(kustomizationFile)
	if err != nil {
		t.Fatalf("reading %s: %v", kustomizationFile, err)
	}
	var k kustomization
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parsing %s, which kustomize refuses to build: %v", kustomizationFile, err)
	}
	return k
}

// decodeFile decodes one YAML file into v.
func decodeFile(t *testing.T, path string, v any) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := yaml.Unmarshal(raw, v); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
}

// linesBesideTheAbsentList returns the lines of a rules file that are neither
// blank, a comment nor the expr line, in file order. The expr of both copies is
// one line, so what is left is everything of the rule but its absent list.
func linesBesideTheAbsentList(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var kept []string
	for line := range strings.Lines(string(raw)) {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "expr:") {
			continue
		}
		kept = append(kept, strings.TrimRight(line, "\n"))
	}
	return kept
}

// simulatorListener loads the simulator's configuration from the two variables
// of env that place its listener, with every other variable it reads blanked to
// its default, so a value in the developer's shell never reaches it.
func simulatorListener(t *testing.T, env map[string]string) simulator.Config {
	t.Helper()

	for _, name := range simulator.EnvNames {
		t.Setenv(name, "")
	}
	for _, name := range []string{"TALLY_SIM_HTTP_ADDR", "TALLY_SIM_HTTP_PORT"} {
		if value, ok := env[name]; ok {
			t.Setenv(name, value)
		}
	}
	cfg, err := simulator.Load()
	if err != nil {
		t.Fatalf("loading the simulator's configuration from the %s environment: %v", composeFile, err)
	}
	return cfg
}

// scrapeConfigOf decodes one scrape file.
func scrapeConfigOf(t *testing.T, path string) scrapeConfig {
	t.Helper()

	var cfg scrapeConfig
	decodeFile(t, path, &cfg)
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

// deploymentPatchOf returns the one strategic merge patch against the named
// Deployment, skipping the JSON patches by their shape the way cronJobPatchOf
// does.
func deploymentPatchOf(t *testing.T, k kustomization, name string) deploymentPatch {
	t.Helper()

	var found []deploymentPatch
	for i, p := range k.Patches {
		var shape any
		if err := yaml.Unmarshal([]byte(p.Patch), &shape); err != nil {
			t.Fatalf("parsing patches[%d]: %v", i, err)
		}
		if _, isDocument := shape.(map[string]any); !isDocument {
			continue
		}
		var doc deploymentPatch
		if err := yaml.Unmarshal([]byte(p.Patch), &doc); err != nil {
			t.Fatalf("parsing patches[%d]: %v", i, err)
		}
		if doc.Kind == "Deployment" && doc.Metadata.Name == name {
			found = append(found, doc)
		}
	}

	if len(found) != 1 {
		t.Fatalf("%s holds %d strategic merge patches against Deployment %s, want one: two of them would leave which env list is merged to the order of the file",
			kustomizationFile, len(found), name)
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

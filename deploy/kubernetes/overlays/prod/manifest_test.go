// This file pins where the prod overlay's names come from and what it keeps
// off the internet. Every mismatch it looks for fails quietly. A replacement
// aimed at the wrong field leaves a route on the base's placeholder hostname,
// and the overlay still renders. A delete patch lost in an edit, or a listener
// index that no longer names postgres after the base reorders its listeners,
// renders just as well, and the first sign is an unauthenticated service on a
// public address. An example secret file without a key the base mounts hands
// the operator a Secret that fails a pod on its first start rather than the
// apply. The tests read the YAML from disk and need neither a cluster nor
// kustomize.
package prod_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	kustomizationFile = "kustomization.yaml"
	hostsFile         = "hosts.yaml"
	issuersFile       = "issuers.yaml"
	scrapeFile        = "victoriametrics/scrape.yaml"
	baseScrapeFile    = "../../base/victoriametrics/scrape.yaml"
	baseGatewayFile   = "../../base/gateway/gateway.yaml"
	baseDir           = "../../base"
	gitignoreFile     = "../../../../.gitignore"

	// The ConfigMap every hostname is read from, and the objects the overlay
	// points at the real ones.
	hostsConfigMap = "tally-hosts"
	certificate    = "tally-wildcard"
	gateway        = "tally"
	namespace      = "tally"
	acmeIssuer     = "letsencrypt"

	// The generated ConfigMap the overlay's scrape file replaces.
	scrapeConfigMap = "victoriametrics-scrape"

	// The Reporting API image by the name the base gives it, and the
	// repository the release workflow publishes it to.
	reportingImage    = "tally-reporting"
	reportingRegistry = "ghcr.io/b42labs/tally-reporting"

	// The line that keeps the filled-in secret files out of the repository.
	secretsIgnoreLine = "deploy/kubernetes/overlays/prod/secrets/*.env"
)

// releaseTag is the shape packaging/release-version.sh accepts, with the
// leading v it strips. The release workflow publishes images only under such
// a tag.
var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.]+)?$`)

// hostLineRe is a line of the data block of hosts.yaml as the Makefile reads it
// with sed: one unquoted key at two spaces, and the hostname alone.
var hostLineRe = regexp.MustCompile(`^  [a-z-]+: [a-z0-9.-]+$`)

// kustomization is the part of the overlay these tests assert over.
type kustomization struct {
	Images []struct {
		Name    string `yaml:"name"`
		NewName string `yaml:"newName"`
		NewTag  string `yaml:"newTag"`
	} `yaml:"images"`
	SecretGenerator []struct {
		Name string   `yaml:"name"`
		Envs []string `yaml:"envs"`
	} `yaml:"secretGenerator"`
	ConfigMapGenerator []generator `yaml:"configMapGenerator"`
	Patches            []struct {
		Patch  string `yaml:"patch"`
		Target struct {
			Group string `yaml:"group"`
			Kind  string `yaml:"kind"`
			Name  string `yaml:"name"`
		} `yaml:"target"`
	} `yaml:"patches"`
	Replacements []replacement `yaml:"replacements"`
}

type generator struct {
	Name     string   `yaml:"name"`
	Behavior string   `yaml:"behavior"`
	Files    []string `yaml:"files"`
}

type replacement struct {
	Source struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		FieldPath string `yaml:"fieldPath"`
	} `yaml:"source"`
	Targets []replacementTarget `yaml:"targets"`
}

type replacementTarget struct {
	Select struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"select"`
	FieldPaths []string `yaml:"fieldPaths"`
	Options    struct {
		Delimiter string `yaml:"delimiter"`
		Index     int    `yaml:"index"`
	} `yaml:"options"`
}

// jsonPatchOp is one operation of a patch that carries a target.
type jsonPatchOp struct {
	Op    string `yaml:"op"`
	Path  string `yaml:"path"`
	Value any    `yaml:"value"`
}

// deletePatch is a strategic merge patch that removes the object it names.
type deletePatch struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Patch string `yaml:"$patch"`
}

// scrapeConfig is the part of a scrape file these tests compare. yaml.v3
// ignores every field not named here.
type scrapeConfig struct {
	ScrapeConfigs []scrapeJob `yaml:"scrape_configs"`
}

type scrapeJob struct {
	JobName        string          `yaml:"job_name"`
	ScrapeInterval string          `yaml:"scrape_interval"`
	RelabelConfigs []relabelConfig `yaml:"relabel_configs"`
}

type relabelConfig struct {
	SourceLabels []string `yaml:"source_labels"`
	Regex        string   `yaml:"regex"`
	Action       string   `yaml:"action"`
	TargetLabel  string   `yaml:"target_label"`
}

func TestHostsAreSixNamesUnderOneDomain(t *testing.T) {
	// hosts.yaml is the one place the domain is written, so serving another
	// domain means editing six values and nothing else. Without the
	// local-config annotation kustomize emits the ConfigMap into the cluster as
	// an object nothing reads. A key renamed or dropped leaves a replacement
	// sourcing a field that is not there. A domain swap that misses one value
	// leaves that service on a name the new DNS does not answer for, and for a
	// published name the HTTP-01 challenge fails with it, which fails the one
	// certificate all four names share. The domain is taken from the file
	// rather than written here, so the test holds for whichever domain is
	// served.
	raw, err := os.ReadFile(hostsFile)
	if err != nil {
		t.Fatalf("reading %s: %v", hostsFile, err)
	}
	var hosts struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name        string            `yaml:"name"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &hosts); err != nil {
		t.Fatalf("parsing %s: %v", hostsFile, err)
	}

	if hosts.Kind != "ConfigMap" || hosts.Metadata.Name != hostsConfigMap {
		t.Fatalf("%s declares %s %s, want ConfigMap %s, which every replacement in %s sources",
			hostsFile, hosts.Kind, hosts.Metadata.Name, hostsConfigMap, kustomizationFile)
	}
	if got := hosts.Metadata.Annotations["config.kubernetes.io/local-config"]; got != "true" {
		t.Errorf("%s carries config.kubernetes.io/local-config %q, want \"true\", so kustomize would emit it into the cluster",
			hostsFile, got)
	}

	keys := []string{"alertmanager", "api", "grafana", "otlp", "otlp-grpc", "vmalert"}
	if got := slices.Sorted(maps.Keys(hosts.Data)); !slices.Equal(got, keys) {
		t.Fatalf("%s holds keys %v, want exactly %v", hostsFile, got, keys)
	}

	// make prod-up reads the values as text, with sed, to print the hostnames
	// and the DNS record to create. A quoted value, a comment after one or a
	// data block indented otherwise is still YAML kustomize renders, and the
	// operator is told to point the record at a name with a quote or a comment
	// in it, or at *. alone.
	_, block, _ := strings.Cut(string(raw), "\ndata:\n")
	for _, line := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
		if !hostLineRe.MatchString(line) {
			t.Errorf("%s carries %q under data, want an unquoted key at two spaces and the hostname alone, as make prod-up reads it",
				hostsFile, line)
		}
	}

	api := hosts.Data["api"]
	_, domain, found := strings.Cut(api, ".")
	if !found || domain == "" {
		t.Fatalf("api = %q, want a full hostname whose labels after the first name the domain", api)
	}
	for _, key := range keys {
		host := hosts.Data[key]
		if label, found := strings.CutSuffix(host, "."+domain); !found || label == "" {
			t.Errorf("%s = %q, want a name under %s, the domain api is served under", key, host, domain)
		}
	}
}

func TestEveryPublishedRouteTakesItsHostFromHosts(t *testing.T) {
	// The base's routes match placeholder hostnames nobody resolves. A
	// replacement that targets another field, or another object, leaves a
	// route on its placeholder: the Gateway accepts it, the overlay renders,
	// and the service answers nothing on its real name.
	k := kustomizationOf(t)
	base := baseObjects(t)

	routes := []struct{ key, kind, name string }{
		{"api", "HTTPRoute", "reporting-api"},
		{"otlp", "HTTPRoute", "otel-collector-http"},
		{"otlp-grpc", "GRPCRoute", "otel-collector-grpc"},
		{"grafana", "HTTPRoute", "grafana"},
	}
	for _, r := range routes {
		t.Run(r.kind+"/"+r.name, func(t *testing.T) {
			if _, ok := targetOf(hostTargets(t, k, r.key), r.kind, r.name, "spec.hostnames.0"); !ok {
				t.Errorf("no replacement copies data.%s into spec.hostnames.0 of %s %s, so the route keeps the base's placeholder",
					r.key, r.kind, r.name)
			}
			// A target the base does not declare selects nothing, and the
			// overlay renders without the replacement. The replacement writes
			// the first hostname, so a second one would keep its placeholder.
			if got := objectNamed(t, base, r.kind, r.name).Spec.Hostnames; len(got) != 1 {
				t.Errorf("%s %s carries the hostnames %v in the base, want exactly one", r.kind, r.name, got)
			}
		})
	}

	// Grafana, vmalert and Alertmanager build the links they render from these
	// values, so a URL left on the placeholder sends a reader to a host that
	// answers nothing. The value carries a scheme and the source does not, so
	// the replacement swaps the part after "://". Without the delimiter it
	// drops the scheme, and with index 0 it replaces the scheme instead.
	urls := []struct{ key, kind, name, container, variable string }{
		{"grafana", "Deployment", "grafana", "grafana", "GF_SERVER_ROOT_URL"},
		{"vmalert", "Deployment", "vmalert", "vmalert", "VMALERT_EXTERNAL_URL"},
		{"alertmanager", "StatefulSet", "alertmanager", "alertmanager", "ALERTMANAGER_EXTERNAL_URL"},
	}
	for _, u := range urls {
		t.Run(u.variable, func(t *testing.T) {
			fieldPath := "spec.template.spec.containers.[name=" + u.container + "].env.[name=" + u.variable + "].value"
			target, ok := targetOf(hostTargets(t, k, u.key), u.kind, u.name, fieldPath)
			if !ok {
				t.Fatalf("no replacement copies data.%s into %s of %s %s, so the variable keeps the base's placeholder",
					u.key, fieldPath, u.kind, u.name)
			}
			if target.Options.Delimiter != "://" || target.Options.Index != 1 {
				t.Errorf("the replacement into %s splits at %q and replaces index %d, want \"://\" and 1, which keeps the https scheme and swaps the host",
					u.variable, target.Options.Delimiter, target.Options.Index)
			}
			// A container or a variable the base renamed is a field path that
			// names nothing on the object the replacement selects.
			if value, set := envValue(objectNamed(t, base, u.kind, u.name), u.container, u.variable); !set || !strings.HasPrefix(value, "https://") {
				t.Errorf("container %s of %s %s in the base sets %s to %q (set: %t), want an https:// URL whose host the replacement swaps",
					u.container, u.kind, u.name, u.variable, value, set)
			}
		})
	}
}

func TestTheCertificateNamesThePublishedHostsAndTheAcmeIssuer(t *testing.T) {
	// Let's Encrypt signs a wildcard only over DNS-01, so the Certificate names
	// each published hostname. A name missing from the list is a hostname the
	// https listener serves with a certificate that does not cover it, while
	// the Certificate itself is issued and reports Ready.
	k := kustomizationOf(t)
	ops := jsonPatchOf(t, k, "cert-manager.io", "Certificate", certificate)

	// A patch whose target the base does not declare selects nothing, and the
	// overlay renders the base's wildcard on its placeholder issuer.
	if cert := objectNamed(t, baseObjects(t), "Certificate", certificate); !strings.HasPrefix(cert.APIVersion, "cert-manager.io/") {
		t.Errorf("Certificate %s in the base has apiVersion %q, want the group cert-manager.io the patch targets", certificate, cert.APIVersion)
	}

	keys := []string{"api", "otlp", "otlp-grpc", "grafana"}
	var dnsNames, issuer bool
	for _, op := range ops {
		switch {
		case op.Op == "replace" && op.Path == "/spec/dnsNames":
			dnsNames = true
			if got := stringsOf(op.Value); !slices.Equal(got, keys) {
				t.Errorf("the patch sets dnsNames to %v, want the placeholders %v, one entry per published hostname", op.Value, keys)
			}
		case op.Op == "replace" && op.Path == "/spec/issuerRef/name":
			issuer = true
			if op.Value != acmeIssuer {
				t.Errorf("the patch points the Certificate at issuer %v, want %s, the ClusterIssuer %s declares",
					op.Value, acmeIssuer, issuersFile)
			}
		}
	}
	if !dnsNames {
		t.Errorf("the patch on Certificate %s does not replace /spec/dnsNames, so it keeps the base's wildcard, which HTTP-01 cannot validate", certificate)
	}
	if !issuer {
		t.Errorf("the patch on Certificate %s does not replace /spec/issuerRef/name, so it waits on the base's placeholder issuer", certificate)
	}

	// The patch lists the keys as placeholders and the replacements overwrite
	// each entry. A replacement aimed at the wrong index leaves a placeholder
	// such as api in the list, which Let's Encrypt refuses to issue for, and no
	// name gets a certificate.
	for i, key := range keys {
		fieldPath := "spec.dnsNames." + strconv.Itoa(i)
		if _, ok := targetOf(hostTargets(t, k, key), "Certificate", certificate, fieldPath); !ok {
			t.Errorf("no replacement copies data.%s into %s of Certificate %s", key, fieldPath, certificate)
		}
	}

	// The challenge route cert-manager creates attaches to the parentRef the
	// solver names. The Gateway admits routes from its own namespace only, so a
	// parentRef in another namespace, or naming another Gateway, leaves every
	// challenge unanswered and the certificate unissued.
	raw, err := os.ReadFile(issuersFile)
	if err != nil {
		t.Fatalf("reading %s: %v", issuersFile, err)
	}
	var clusterIssuer struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			ACME struct {
				Solvers []struct {
					HTTP01 struct {
						GatewayHTTPRoute struct {
							ParentRefs []struct {
								Kind      string `yaml:"kind"`
								Name      string `yaml:"name"`
								Namespace string `yaml:"namespace"`
							} `yaml:"parentRefs"`
						} `yaml:"gatewayHTTPRoute"`
					} `yaml:"http01"`
				} `yaml:"solvers"`
			} `yaml:"acme"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &clusterIssuer); err != nil {
		t.Fatalf("parsing %s: %v", issuersFile, err)
	}
	if clusterIssuer.Kind != "ClusterIssuer" || clusterIssuer.Metadata.Name != acmeIssuer {
		t.Fatalf("%s declares %s %s, want ClusterIssuer %s", issuersFile, clusterIssuer.Kind, clusterIssuer.Metadata.Name, acmeIssuer)
	}
	var solved bool
	for _, s := range clusterIssuer.Spec.ACME.Solvers {
		for _, ref := range s.HTTP01.GatewayHTTPRoute.ParentRefs {
			if ref.Kind == "Gateway" && ref.Name == gateway && ref.Namespace == namespace {
				solved = true
			}
		}
	}
	if !solved {
		t.Errorf("%s has no gatewayHTTPRoute solver whose parentRef names Gateway %s in namespace %s", issuersFile, gateway, namespace)
	}
}

func TestNothingUnauthenticatedIsPublished(t *testing.T) {
	// The UIs of the store, vmalert and Alertmanager answer without a
	// credential, and a TCPRoute on a LoadBalancer is the database on the
	// internet. The CronJob is in the list because this cluster runs no
	// scheduler. A delete patch dropped in an edit renders a valid overlay that
	// publishes the object again.
	k := kustomizationOf(t)

	want := []string{
		"CronJob/tally-engine",
		"HTTPRoute/alertmanager",
		"HTTPRoute/victoriametrics",
		"HTTPRoute/vmalert",
		"HTTPRouteFilter/alertmanager-deny-writes",
		"TCPRoute/timescaledb",
	}
	if got := deletedObjects(t, k); !slices.Equal(got, want) {
		t.Errorf("%s deletes %v, want exactly %v", kustomizationFile, got, want)
	}

	// A JSON patch removes a listener by its index, not its name. If the base
	// reorders its listeners, the same index removes http or https while the
	// postgres listener stays, so the index is held to the name it has in the
	// base.
	var removed []int
	for _, op := range jsonPatchOf(t, k, "gateway.networking.k8s.io", "Gateway", gateway) {
		index, found := strings.CutPrefix(op.Path, "/spec/listeners/")
		if op.Op != "remove" || !found {
			continue
		}
		n, err := strconv.Atoi(index)
		if err != nil {
			t.Fatalf("the Gateway patch removes %s, which names no listener index: %v", op.Path, err)
		}
		removed = append(removed, n)
	}
	if len(removed) != 1 {
		t.Fatalf("the Gateway patch removes listeners %v, want exactly one, the postgres listener", removed)
	}

	listeners := baseListeners(t)
	n := removed[0]
	if n < 0 || n >= len(listeners) {
		t.Fatalf("the Gateway patch removes listener %d, but Gateway %s in %s has only the listeners %v",
			n, gateway, baseGatewayFile, listeners)
	}
	if listeners[n] != "postgres" {
		t.Errorf("the Gateway patch removes listener %d, which is %q in %s, want the one named postgres: the listeners are %v",
			n, listeners[n], baseGatewayFile, listeners)
	}
}

func TestGrafanaServesNoMetrics(t *testing.T) {
	// Grafana answers /metrics on the port its route publishes, and without a
	// credential unless basic auth is set for it. Nothing on this cluster
	// scrapes it, and published it names the exact version and counts the
	// failed logins for anyone who asks.
	k := kustomizationOf(t)

	var values []string
	for i, p := range k.Patches {
		var shape any
		if err := yaml.Unmarshal([]byte(p.Patch), &shape); err != nil {
			t.Fatalf("parsing patches[%d]: %v", i, err)
		}
		if _, isDocument := shape.(map[string]any); !isDocument {
			continue
		}
		var patch object
		if err := yaml.Unmarshal([]byte(p.Patch), &patch); err != nil {
			t.Fatalf("parsing patches[%d]: %v", i, err)
		}
		if patch.Kind != "Deployment" || patch.Metadata.Name != "grafana" {
			continue
		}
		if value, set := envValue(patch, "grafana", "GF_METRICS_ENABLED"); set {
			values = append(values, value)
		}
	}
	if !slices.Equal(values, []string{"false"}) {
		t.Errorf("the patches of %s set GF_METRICS_ENABLED on Grafana to %v, want [false]", kustomizationFile, values)
	}
}

func TestImagesComeFromTheRegistry(t *testing.T) {
	// The base names a locally built image, which a real cluster cannot pull.
	// The tag has to be one the release workflow publishes under; any other
	// tag names an image that is not in the registry, and the pod sits in
	// ImagePullBackOff.
	k := kustomizationOf(t)

	var found int
	for _, image := range k.Images {
		if image.Name != reportingImage {
			continue
		}
		found++
		if image.NewName != reportingRegistry {
			t.Errorf("image %s is pulled from %q, want %s", reportingImage, image.NewName, reportingRegistry)
		}
		if !releaseTag.MatchString(image.NewTag) {
			t.Errorf("image %s is pulled at tag %q, want a release tag of the shape packaging/release-version.sh accepts",
				reportingImage, image.NewTag)
		}
	}
	if found != 1 {
		t.Errorf("%s holds %d images entries for %s, want exactly one", kustomizationFile, found, reportingImage)
	}
}

func TestEverySecretHasAnExampleWithTheKeysTheBaseMounts(t *testing.T) {
	// The operator fills the untracked files from the examples. A key missing
	// from an example is a key missing from the Secret, which the pod that
	// mounts it fails on at its first start, after the apply succeeded. The
	// keys are read from the base, less the objects this overlay deletes, so a
	// key the base starts to mount is one the example has to carry.
	// engine-password is among them although no engine runs here: TimescaleDB
	// reads it for the initdb script that creates the engine's reader role.
	k := kustomizationOf(t)

	deleted := deletedObjects(t, k)
	want := map[string][]string{}
	for _, o := range baseObjects(t) {
		if !slices.Contains(deleted, o.Kind+"/"+o.Metadata.Name) {
			mountedSecretKeys(t, o.raw, want)
		}
	}
	if len(want) == 0 {
		t.Fatalf("%s mounts no secret key, so this test would assert over nothing", baseDir)
	}
	for name, keys := range want {
		slices.Sort(keys)
		want[name] = slices.Compact(keys)
	}

	generated := make(map[string]bool, len(k.SecretGenerator))
	for _, g := range k.SecretGenerator {
		generated[g.Name] = true
		keys, known := want[g.Name]
		if !known {
			t.Errorf("%s generates secret %s, which the base does not mount", kustomizationFile, g.Name)
			continue
		}
		file := "secrets/" + g.Name + ".env"
		if !slices.Equal(g.Envs, []string{file}) {
			t.Errorf("secret %s reads %v, want exactly [%s]", g.Name, g.Envs, file)
			continue
		}
		example := file + ".example"
		got, err := envKeys(example)
		if err != nil {
			t.Errorf("secret %s has no usable example to copy %s from: %v", g.Name, file, err)
			continue
		}
		slices.Sort(got)
		if !slices.Equal(got, keys) {
			t.Errorf("%s carries keys %v, want %v, the keys the base mounts from secret %s", example, got, keys, g.Name)
		}
	}
	for name, keys := range want {
		if !generated[name] {
			t.Errorf("the base mounts %v from secret %s, which %s does not generate", keys, name, kustomizationFile)
		}
	}

	// Without the ignore line a filled-in file is one `git add` away from the
	// repository.
	raw, err := os.ReadFile(gitignoreFile)
	if err != nil {
		t.Fatalf("reading %s: %v", gitignoreFile, err)
	}
	if !slices.Contains(strings.Split(string(raw), "\n"), secretsIgnoreLine) {
		t.Errorf("%s has no line %s, so the filled-in secret files are not ignored", gitignoreFile, secretsIgnoreLine)
	}
}

func TestScrapeConfigKeepsOnlyTheInClusterJobs(t *testing.T) {
	// The OpenStack jobs of the base scrape placeholder addresses, and on a
	// cluster with no such exporter they keep TallyScrapeTargetDown firing.
	// TallyScrapeJobMissing selects exactly reporting-api and otel-collector,
	// so those two stay and nothing else is added.
	overlay := scrapeConfigOf(t, scrapeFile)
	base := scrapeConfigOf(t, baseScrapeFile)

	var names []string
	for _, job := range overlay.ScrapeConfigs {
		names = append(names, job.JobName)
	}
	if want := []string{"reporting-api", "otel-collector"}; !slices.Equal(names, want) {
		t.Fatalf("%s declares jobs %v, want %v", scrapeFile, names, want)
	}

	// The two jobs are copies of the base's, which the rules and the comments
	// of the base file are written against. A copy that drifted, such as a
	// relabel rule keeping another port, scrapes the wrong endpoint or none,
	// and the file stays legal YAML.
	for _, job := range overlay.ScrapeConfigs {
		i := slices.IndexFunc(base.ScrapeConfigs, func(b scrapeJob) bool { return b.JobName == job.JobName })
		if i < 0 {
			t.Errorf("%s declares no job %s to copy", baseScrapeFile, job.JobName)
			continue
		}
		if !reflect.DeepEqual(job, base.ScrapeConfigs[i]) {
			t.Errorf("job %s is %+v, want the base's %+v", job.JobName, job, base.ScrapeConfigs[i])
		}
	}

	// Without the replacing generator VictoriaMetrics keeps the base's file
	// with both placeholder jobs.
	k := kustomizationOf(t)
	i := slices.IndexFunc(k.ConfigMapGenerator, func(g generator) bool { return g.Name == scrapeConfigMap })
	if i < 0 {
		t.Fatalf("%s generates no ConfigMap %s", kustomizationFile, scrapeConfigMap)
	}
	g := k.ConfigMapGenerator[i]
	if g.Behavior != "replace" {
		t.Errorf("the %s generator has behavior %q, want \"replace\": that is what overrides the base's generated ConfigMap",
			scrapeConfigMap, g.Behavior)
	}
	if !slices.Equal(g.Files, []string{scrapeFile}) {
		t.Errorf("the %s generator carries %v, want exactly [%s]", scrapeConfigMap, g.Files, scrapeFile)
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

// hostTargets returns the targets of every replacement that sources one key of
// the hosts ConfigMap, failing if none does.
func hostTargets(t *testing.T, k kustomization, key string) []replacementTarget {
	t.Helper()

	var targets []replacementTarget
	for _, r := range k.Replacements {
		if r.Source.Kind == "ConfigMap" && r.Source.Name == hostsConfigMap && r.Source.FieldPath == "data."+key {
			targets = append(targets, r.Targets...)
		}
	}
	if len(targets) == 0 {
		t.Fatalf("%s has no replacement sourcing data.%s from ConfigMap %s", kustomizationFile, key, hostsConfigMap)
	}
	return targets
}

// targetOf returns the target that selects one object and carries one field
// path, and whether there is one.
func targetOf(targets []replacementTarget, kind, name, fieldPath string) (replacementTarget, bool) {
	for _, target := range targets {
		if target.Select.Kind == kind && target.Select.Name == name && slices.Contains(target.FieldPaths, fieldPath) {
			return target, true
		}
	}
	return replacementTarget{}, false
}

// jsonPatchOf returns the operations of the one patch that targets an object.
// The group is matched as well: a target in another group selects nothing,
// and kustomize applies such a patch to no object without an error.
func jsonPatchOf(t *testing.T, k kustomization, group, kind, name string) []jsonPatchOp {
	t.Helper()

	var found [][]jsonPatchOp
	for i, p := range k.Patches {
		if p.Target.Group != group || p.Target.Kind != kind || p.Target.Name != name {
			continue
		}
		var ops []jsonPatchOp
		if err := yaml.Unmarshal([]byte(p.Patch), &ops); err != nil {
			t.Fatalf("parsing patches[%d] against %s %s as a JSON patch: %v", i, kind, name, err)
		}
		found = append(found, ops)
	}
	if len(found) != 1 {
		t.Fatalf("%s holds %d patches targeting %s %s in group %s, want one", kustomizationFile, len(found), kind, name, group)
	}
	return found[0]
}

// deletedObjects returns kind/name of every object a strategic merge patch of
// the overlay deletes, sorted. The JSON patches are sequences rather than
// documents and are skipped by their shape.
func deletedObjects(t *testing.T, k kustomization) []string {
	t.Helper()

	var deleted []string
	for i, p := range k.Patches {
		var shape any
		if err := yaml.Unmarshal([]byte(p.Patch), &shape); err != nil {
			t.Fatalf("parsing patches[%d]: %v", i, err)
		}
		if _, isDocument := shape.(map[string]any); !isDocument {
			continue
		}
		var doc deletePatch
		if err := yaml.Unmarshal([]byte(p.Patch), &doc); err != nil {
			t.Fatalf("parsing patches[%d]: %v", i, err)
		}
		if doc.Patch == "delete" {
			deleted = append(deleted, doc.Kind+"/"+doc.Metadata.Name)
		}
	}
	slices.Sort(deleted)
	return deleted
}

// baseListeners returns the listener names of the base's Gateway, in order.
// The file carries the GatewayClass first, so every document is read.
func baseListeners(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(baseGatewayFile)
	if err != nil {
		t.Fatalf("reading %s: %v", baseGatewayFile, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Listeners []struct {
					Name string `yaml:"name"`
				} `yaml:"listeners"`
			} `yaml:"spec"`
		}
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parsing %s: %v", baseGatewayFile, err)
		}
		if doc.Kind != "Gateway" || doc.Metadata.Name != gateway {
			continue
		}
		names := make([]string, 0, len(doc.Spec.Listeners))
		for _, l := range doc.Spec.Listeners {
			names = append(names, l.Name)
		}
		return names
	}
	t.Fatalf("%s declares no Gateway %s", baseGatewayFile, gateway)
	return nil
}

// object is the part of a Kubernetes object these tests look up, in the base or
// in a patch. raw is the whole object, which mountedSecretKeys walks.
type object struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Hostnames []string `yaml:"hostnames"`
		Template  struct {
			Spec struct {
				Containers []struct {
					Name string `yaml:"name"`
					Env  []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"env"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
	raw any
}

// baseObjects decodes every object the YAML files of the base declare. A
// document without a kind and a name, such as a scrape or a provisioning file,
// is not an object and is skipped.
func baseObjects(t *testing.T) []object {
	t.Helper()

	var objects []object
	err := filepath.WalkDir(baseDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Ext(path) != ".yaml" {
			return walkErr
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		for {
			var node yaml.Node
			err := dec.Decode(&node)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}
			var o object
			if err := node.Decode(&o.raw); err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}
			doc, _ := o.raw.(map[string]any)
			metadata, _ := doc["metadata"].(map[string]any)
			_, kind := doc["kind"].(string)
			if _, name := metadata["name"].(string); !kind || !name {
				continue
			}
			if err := node.Decode(&o); err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}
			objects = append(objects, o)
		}
	})
	if err != nil {
		t.Fatalf("reading the objects of %s: %v", baseDir, err)
	}
	return objects
}

// objectNamed returns the one object of a kind and a name, failing when there
// is none or more than one.
func objectNamed(t *testing.T, objects []object, kind, name string) object {
	t.Helper()

	var found []object
	for _, o := range objects {
		if o.Kind == kind && o.Metadata.Name == name {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s declares %d objects %s %s, want one", baseDir, len(found), kind, name)
	}
	return found[0]
}

// envValue returns the value one container of an object sets a variable to, and
// whether it sets it.
func envValue(o object, container, variable string) (string, bool) {
	for _, c := range o.Spec.Template.Spec.Containers {
		if c.Name != container {
			continue
		}
		for _, e := range c.Env {
			if e.Name == variable {
				return e.Value, true
			}
		}
	}
	return "", false
}

// mountedSecretKeys adds to keys every Secret key value reads, wherever in it a
// pod spec reads one: an env secretKeyRef or an item of a secret volume. A
// secret volume without items mounts keys no example can be checked against,
// and fails.
func mountedSecretKeys(t *testing.T, value any, keys map[string][]string) {
	t.Helper()

	switch v := value.(type) {
	case map[string]any:
		if ref, ok := v["secretKeyRef"].(map[string]any); ok {
			name, _ := ref["name"].(string)
			key, _ := ref["key"].(string)
			keys[name] = append(keys[name], key)
		}
		if secret, ok := v["secret"].(map[string]any); ok {
			name, _ := secret["secretName"].(string)
			items, _ := secret["items"].([]any)
			if len(items) == 0 {
				t.Errorf("the base mounts every key of secret %s, which no example can be checked against", name)
			}
			for _, item := range items {
				entry, _ := item.(map[string]any)
				key, _ := entry["key"].(string)
				keys[name] = append(keys[name], key)
			}
		}
		for _, child := range v {
			mountedSecretKeys(t, child, keys)
		}
	case []any:
		for _, child := range v {
			mountedSecretKeys(t, child, keys)
		}
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

// envKeys returns the keys of an env file the way kustomize reads it: blank
// lines and lines starting with # are skipped, and a key is what precedes the
// first =.
func envKeys(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys []string
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, _, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("%s line %d carries no =: %s", path, i+1, line)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// stringsOf returns a decoded YAML sequence as strings. An entry that is not a
// string becomes the empty string, which no comparison in this file accepts.
func stringsOf(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, _ := item.(string)
		out = append(out, s)
	}
	return out
}

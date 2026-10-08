// This file pins where the prod overlay's names come from and what it keeps
// off the internet. Every mismatch it looks for fails quietly. A replacement
// aimed at the wrong field leaves a route on the base's placeholder hostname,
// and the overlay still renders. A delete patch lost in an edit, or a listener
// index that no longer names postgres after the base reorders its listeners,
// renders just as well, and the first sign is an unauthenticated service on a
// public address. An example secret file without a key the base mounts hands
// the operator a Secret that fails a pod on its first start rather than the
// apply. The collector fails the same way: two images at two tags render, a
// settings file without TALLY_OSC_CLOUD renders, and so does one that sets a
// variable the component fixes, which the pod never sees. A clouds config the
// Makefile cannot read renders as well, and make prod-up then refuses a cloud
// that is set. The tests read the YAML from disk and need neither a cluster nor
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
	scrapeRulesFile   = "victoriametrics/scrape-rules.yaml"
	baseScrapeRules   = "../../base/vmalert/scrape-rules.yaml"
	baseGatewayFile   = "../../base/gateway/gateway.yaml"
	baseDir           = "../../base"
	componentDir      = "../../components/envoy-gateway"
	gitignoreFile     = "../../../../.gitignore"

	// The component that declares the collector, and the file the overlay
	// generates the collector's non-secret settings from.
	collectorComponentDir = "../../components/openstack-collector"
	collectorSettingsFile = "collector.env"

	// The components that declare the migration Job and the sync.
	migrationsComponentDir     = "../../components/migrations"
	reconciliationComponentDir = "../../components/reconciliation"

	// The ConfigMap every hostname is read from, and the objects the overlay
	// points at the real ones.
	hostsConfigMap = "tally-hosts"
	certificate    = "tally-wildcard"
	gateway        = "tally"
	namespace      = "tally"
	acmeIssuer     = "letsencrypt"

	// The generated ConfigMaps the overlay's scrape file and its rules file
	// replace, and the one rule the rules file carries.
	scrapeConfigMap      = "victoriametrics-scrape"
	scrapeRulesConfigMap = "vmalert-scrape-rules"
	jobMissingAlert      = "TallyScrapeJobMissing"

	// The Reporting API image by the name the base gives it, and the
	// repository the release workflow publishes it to.
	reportingImage    = "tally-reporting"
	reportingRegistry = "ghcr.io/b42labs/tally-reporting"

	// The collector image the same way, the ConfigMap its settings are
	// generated into, and the Secret of its ingest token, which no generator
	// carries: the operator creates it from the output of
	// create-ingest-credential.
	collectorImage       = "tally-openstack-collector"
	collectorRegistry    = "ghcr.io/b42labs/tally-openstack-collector"
	collectorConfigMap   = "tally-openstack-collector"
	collectorTokenSecret = "tally-collector-token"

	// The images of the scheduler and the migration Job the same way.
	engineImage    = "tally-engine"
	engineRegistry = "ghcr.io/b42labs/tally-engine"
	adminImage     = "tally-reporting-admin"
	adminRegistry  = "ghcr.io/b42labs/tally-reporting-admin"

	// The Secret the Reporting API authenticates against the cloud with and
	// the one file it is generated from, and the ConfigMap of the cloud it
	// reconciles with the file that one is generated from.
	cloudsSecret     = "tally-reconciliation-auth"
	cloudsSecretFile = "secrets/clouds.yaml"
	cloudsConfigMap  = "tally-reconciliation"
	cloudsConfigFile = "reconciliation/clouds-config.yaml"

	// The lines that keep the filled-in secret files out of the repository.
	secretsIgnoreLine = "deploy/kubernetes/overlays/prod/secrets/*.env"
	cloudsIgnoreLine  = "deploy/kubernetes/overlays/prod/secrets/clouds.yaml"
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
	Components []string `yaml:"components"`
	Images     []struct {
		Name    string `yaml:"name"`
		NewName string `yaml:"newName"`
		NewTag  string `yaml:"newTag"`
	} `yaml:"images"`
	SecretGenerator []struct {
		Name  string   `yaml:"name"`
		Envs  []string `yaml:"envs"`
		Files []string `yaml:"files"`
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
	Envs     []string `yaml:"envs"`
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
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
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
	JobName        string           `yaml:"job_name"`
	ScrapeInterval string           `yaml:"scrape_interval"`
	RelabelConfigs []relabelConfig  `yaml:"relabel_configs"`
	KubernetesSD   []map[string]any `yaml:"kubernetes_sd_configs"`
}

type relabelConfig struct {
	SourceLabels []string `yaml:"source_labels"`
	Regex        string   `yaml:"regex"`
	Action       string   `yaml:"action"`
	TargetLabel  string   `yaml:"target_label"`
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
	// internet. A delete patch dropped in an edit renders a valid overlay that
	// publishes the object again. One added in an edit takes away what the
	// cluster runs, such as the scheduler, and renders just as well.
	k := kustomizationOf(t)

	want := []string{
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

func TestEveryComponentIsListed(t *testing.T) {
	// The base is plain Gateway API and names no implementation. The
	// GatewayClass bound to Envoy Gateway's controller, the rate limit on the
	// OTLP routes and the 403 on Grafana's datasource proxy are the first
	// component's, and this cluster runs Envoy Gateway. The second declares the
	// collector, without which the stack stores what nothing sends. The third
	// migrates both databases, without which the Reporting API stays unready
	// and every tick fails on the schema. The fourth syncs the cloud, without
	// which a delete the collector lost is billed until somebody notices. An
	// entry lost in an edit of this file alone is not an error to kustomize.
	k := kustomizationOf(t)

	want := []string{componentDir, collectorComponentDir, migrationsComponentDir, reconciliationComponentDir}
	if !slices.Equal(k.Components, want) {
		t.Errorf("%s lists the components %v, want %v; without one of them the overlay still renders",
			kustomizationFile, k.Components, want)
	}
}

func TestEveryDeletePatchHasATarget(t *testing.T) {
	// The overlay deletes objects of the base and a filter the component
	// declares. A delete patch whose target no object matches is the one
	// mismatch here that kustomize refuses, and no test renders the overlay,
	// so the first build to fail would be the one of a deploy. kustomize
	// matches the target by its API version as well as by its kind and name.
	k := kustomizationOf(t)

	declared := append(baseObjects(t), objectsIn(t, componentDir)...)
	for _, d := range deletePatches(t, k) {
		found := slices.ContainsFunc(declared, func(o object) bool {
			return o.APIVersion == d.APIVersion && o.Kind == d.Kind && o.Metadata.Name == d.Metadata.Name
		})
		if !found {
			t.Errorf("%s deletes %s %s in %s, which neither %s nor %s declares; a delete patch without a target fails the build, and it does so at deploy time alone",
				kustomizationFile, d.Kind, d.Metadata.Name, d.APIVersion, baseDir, componentDir)
		}
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
	// The base and the components name locally built images, which a real
	// cluster cannot pull. The tag has to be one the release workflow publishes
	// under; any other tag names an image that is not in the registry, and the
	// pod sits in ImagePullBackOff. The overlay deploys one release, so the
	// four tags are one: the migration Job applies the chains of its images,
	// and a Reporting API or a scheduler of another release refuses or misreads
	// that schema.
	k := kustomizationOf(t)

	var tags []string
	for _, want := range []struct{ name, registry string }{
		{reportingImage, reportingRegistry},
		{collectorImage, collectorRegistry},
		{engineImage, engineRegistry},
		{adminImage, adminRegistry},
	} {
		var found int
		for _, image := range k.Images {
			if image.Name != want.name {
				continue
			}
			found++
			tags = append(tags, image.NewTag)
			if image.NewName != want.registry {
				t.Errorf("image %s is pulled from %q, want %s", want.name, image.NewName, want.registry)
			}
			if !releaseTag.MatchString(image.NewTag) {
				t.Errorf("image %s is pulled at tag %q, want a release tag of the shape packaging/release-version.sh accepts",
					want.name, image.NewTag)
			}
		}
		if found != 1 {
			t.Errorf("%s holds %d images entries for %s, want exactly one", kustomizationFile, found, want.name)
		}
	}
	slices.Sort(tags)
	if distinct := slices.Compact(tags); len(distinct) > 1 {
		t.Errorf("%s pulls its images at the tags %v, want one tag, the release the overlay deploys", kustomizationFile, distinct)
	}

	// An images entry rewrites a container by the name before its tag. A name
	// no container carries matches nothing, the overlay renders, and the pod
	// pulls a :dev tag no registry holds. A locally built image without an
	// entry fails the same way.
	var local []string
	for _, o := range keptObjects(t, k) {
		local = devImages(o.raw, local)
	}
	slices.Sort(local)
	local = slices.Compact(local)
	if want := []string{engineImage, collectorImage, reportingImage, adminImage}; !slices.Equal(local, want) {
		t.Errorf("the objects this overlay keeps run the locally built images %v, want exactly %v, the names %s maps to the registry",
			local, want, kustomizationFile)
	}
}

func TestEverySecretHasAnExampleWithTheKeysTheBaseMounts(t *testing.T) {
	// The operator fills the untracked files from the examples. A key missing
	// from an example is a key missing from the Secret, which the pod that
	// mounts it fails on at its first start, after the apply succeeded. The
	// keys are read from the base and from the three components that declare
	// objects, less the objects this overlay deletes, so a key one of them
	// starts to mount is one the example has to carry.
	k := kustomizationOf(t)

	want := map[string][]string{}
	for _, o := range keptObjects(t, k) {
		mountedSecretKeys(t, o.raw, want)
	}
	if len(want) == 0 {
		t.Fatalf("%s and the components mount no secret key, so this test would assert over nothing", baseDir)
	}
	// The ingest token is the one mounted Secret without a file. It can be
	// issued only once make prod-up has migrated the database, and make prod-up
	// refuses an unfilled file, so a generator for it would ask for a value the
	// operator cannot have yet.
	if _, mounted := want[collectorTokenSecret]; !mounted {
		t.Errorf("%s mounts no secret %s, which this test exempts from the generators", collectorComponentDir, collectorTokenSecret)
	}
	for name, keys := range want {
		slices.Sort(keys)
		want[name] = slices.Compact(keys)
	}

	generated := make(map[string]bool, len(k.SecretGenerator))
	for _, g := range k.SecretGenerator {
		generated[g.Name] = true
		if g.Name == collectorTokenSecret {
			t.Errorf("%s generates secret %s, which has no file: the operator creates it from the output of create-ingest-credential",
				kustomizationFile, g.Name)
			continue
		}
		keys, known := want[g.Name]
		if !known {
			t.Errorf("%s generates secret %s, which neither the base nor a component mounts", kustomizationFile, g.Name)
			continue
		}
		// The clouds.yaml of the cloud is a YAML file, not an env file. The
		// generator makes its one key the file's base name, which is the key
		// the reconciliation component mounts.
		if g.Name == cloudsSecret {
			if len(g.Envs) != 0 || !slices.Equal(g.Files, []string{cloudsSecretFile}) {
				t.Errorf("secret %s reads the env files %v and the files %v, want exactly the file [%s]", g.Name, g.Envs, g.Files, cloudsSecretFile)
				continue
			}
			if base := filepath.Base(cloudsSecretFile); !slices.Equal(keys, []string{base}) {
				t.Errorf("secret %s is mounted with the keys %v, want [%s], the key its file is generated under", g.Name, keys, base)
			}
			checkCloudsExample(t, cloudsSecretFile+".example")
			continue
		}
		if len(g.Files) != 0 {
			t.Errorf("secret %s reads the files %v, which make prod-up checks no key of; only %s is generated from a file", g.Name, g.Files, cloudsSecret)
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
			t.Errorf("%s carries keys %v, want %v, the keys mounted from secret %s", example, got, keys, g.Name)
		}
	}
	for name, keys := range want {
		if name != collectorTokenSecret && !generated[name] {
			t.Errorf("%v is mounted from secret %s, which %s does not generate", keys, name, kustomizationFile)
		}
	}

	// Without the ignore lines a filled-in file is one `git add` away from the
	// repository.
	raw, err := os.ReadFile(gitignoreFile)
	if err != nil {
		t.Fatalf("reading %s: %v", gitignoreFile, err)
	}
	for _, line := range []string{secretsIgnoreLine, cloudsIgnoreLine} {
		if !slices.Contains(strings.Split(string(raw), "\n"), line) {
			t.Errorf("%s has no line %s, so the filled-in secret files are not ignored", gitignoreFile, line)
		}
	}
}

// checkCloudsExample holds the example of the clouds.yaml to what the operator
// copies: one entry under clouds, whose values are placeholders make prod-up
// refuses until they are filled.
func checkCloudsExample(t *testing.T, example string) {
	t.Helper()

	raw, err := os.ReadFile(example)
	if err != nil {
		t.Errorf("secret %s has no usable example to copy %s from: %v", cloudsSecret, cloudsSecretFile, err)
		return
	}
	var clouds struct {
		Clouds map[string]any `yaml:"clouds"`
	}
	if err := yaml.Unmarshal(raw, &clouds); err != nil {
		t.Errorf("parsing %s: %v", example, err)
		return
	}
	if len(clouds.Clouds) != 1 {
		t.Errorf("%s carries %d entries under clouds, want one, the entry os_cloud in %s names", example, len(clouds.Clouds), cloudsConfigFile)
	}
	if !regexp.MustCompile(`<[a-z-]+>`).Match(raw) {
		t.Errorf("%s carries no <...> placeholder, so make prod-up cannot tell a copy that was never filled in", example)
	}
}

func TestTheCloudsConfigShipsEmptyAndAsTheMakefileReadsIt(t *testing.T) {
	// The Reporting API reconciles the clouds this file names, and the sync
	// asks for the cloud of collector.env. A guessed name syncs a cloud nothing
	// reports under, so the file ships with both names empty, which the
	// Reporting API refuses at startup and make prod-up before that. make
	// prod-up reads the two lines with grep, so a quoted value, a comment after
	// one or another indentation is still YAML kustomize renders, and is
	// refused as naming no cloud once the operator has filled it.
	k := kustomizationOf(t)

	i := slices.IndexFunc(k.ConfigMapGenerator, func(g generator) bool { return g.Name == cloudsConfigMap })
	if i < 0 {
		t.Fatalf("%s generates no ConfigMap %s, which the Reporting API reads its clouds from", kustomizationFile, cloudsConfigMap)
	}
	if g := k.ConfigMapGenerator[i]; !slices.Equal(g.Files, []string{cloudsConfigFile}) || len(g.Envs) != 0 {
		t.Errorf("the %s generator reads the files %v and the env files %v, want exactly the file [%s]", cloudsConfigMap, g.Files, g.Envs, cloudsConfigFile)
	}

	raw, err := os.ReadFile(cloudsConfigFile)
	if err != nil {
		t.Fatalf("reading %s: %v", cloudsConfigFile, err)
	}
	var config struct {
		Clouds []struct {
			Cloud         string `yaml:"cloud"`
			Platform      string `yaml:"platform"`
			Adapter       string `yaml:"adapter"`
			AdapterConfig struct {
				OSCloud string `yaml:"os_cloud"`
			} `yaml:"adapter_config"`
		} `yaml:"clouds"`
	}
	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatalf("parsing %s, which the Reporting API refuses at startup: %v", cloudsConfigFile, err)
	}
	if len(config.Clouds) != 1 {
		t.Fatalf("%s names %d clouds, want one, the cloud of the one collector", cloudsConfigFile, len(config.Clouds))
	}
	entry := config.Clouds[0]
	if entry.Platform != "openstack" || entry.Adapter != "openstack" {
		t.Errorf("the entry has platform %q and adapter %q, want openstack for both", entry.Platform, entry.Adapter)
	}
	if entry.Cloud != "" {
		t.Errorf("the entry ships with cloud %q, want it empty, which make prod-up refuses", entry.Cloud)
	}
	if entry.AdapterConfig.OSCloud != "" {
		t.Errorf("the entry ships with os_cloud %q, want it empty, which make prod-up refuses", entry.AdapterConfig.OSCloud)
	}

	// The two lines carry the keys the operator fills: an empty value and a
	// missing key read alike above.
	lines := strings.Split(string(raw), "\n")
	for _, want := range []string{"  - cloud:", "      os_cloud:"} {
		if !slices.Contains(lines, want) {
			t.Errorf("%s carries no line %q, which is the line make prod-up reads once the operator has filled it", cloudsConfigFile, want)
		}
	}
}

func TestTheCollectorSettingsNameTheCloud(t *testing.T) {
	// The collector takes its non-secret settings from a ConfigMap generated
	// from a tracked file, and the file ships with an empty TALLY_OSC_CLOUD the
	// operator fills. make prod-up reads that line with grep, so a file without
	// it is refused as leaving the cloud empty, and one with two is judged by
	// either. The pod's env wins over the ConfigMap, so a variable the
	// component sets is a line here that changes nothing, and a secret set here
	// beside its *_FILE companion stops the collector.
	k := kustomizationOf(t)

	i := slices.IndexFunc(k.ConfigMapGenerator, func(g generator) bool { return g.Name == collectorConfigMap })
	if i < 0 {
		t.Fatalf("%s generates no ConfigMap %s, which the collector takes its settings from", kustomizationFile, collectorConfigMap)
	}
	if g := k.ConfigMapGenerator[i]; !slices.Equal(g.Envs, []string{collectorSettingsFile}) {
		t.Errorf("the %s generator reads the env files %v, want exactly [%s]", collectorConfigMap, g.Envs, collectorSettingsFile)
	}

	// The names the component fixes are read from its manifest, so a variable
	// it starts to set is one this file may no longer carry.
	var fixed []string
	for _, o := range objectsIn(t, collectorComponentDir) {
		for _, c := range o.Spec.Template.Spec.Containers {
			for _, e := range c.Env {
				fixed = append(fixed, e.Name)
			}
		}
	}
	if len(fixed) == 0 {
		t.Fatalf("%s sets no variable on any container, so this test would hold %s to nothing", collectorComponentDir, collectorSettingsFile)
	}
	refused := append([]string{"TALLY_OSC_AMQP_URL", "TALLY_OSC_TOKEN"}, fixed...)

	keys, err := envKeys(collectorSettingsFile)
	if err != nil {
		t.Fatalf("reading the settings of the collector: %v", err)
	}
	var clouds int
	for _, key := range keys {
		if key == "TALLY_OSC_CLOUD" {
			clouds++
		}
		if slices.Contains(refused, key) {
			t.Errorf("%s sets %s, which is a secret or a variable the collector component fixes", collectorSettingsFile, key)
		}
	}
	if clouds != 1 {
		t.Errorf("%s carries %d lines starting TALLY_OSC_CLOUD=, want exactly one, which make prod-up reads", collectorSettingsFile, clouds)
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

func TestScrapeRulesNameTheDiscoveredJobsOfThisOverlay(t *testing.T) {
	// The absent list of TallyScrapeJobMissing is this cluster's list of
	// discovered jobs, and the overlay replaces the base's with it. A clause
	// for a job the scrape config does not discover fires for as long as the
	// cluster runs, and a discovered job without one resolves to no targets
	// unheard.
	raw, err := os.ReadFile(scrapeRulesFile)
	if err != nil {
		t.Fatalf("reading %s: %v", scrapeRulesFile, err)
	}
	var rules ruleFile
	if err := yaml.Unmarshal(raw, &rules); err != nil {
		t.Fatalf("parsing %s: %v", scrapeRulesFile, err)
	}
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

	// The scrape config copies the base's discovered jobs, so the rule over
	// them is the base's too, byte for byte, the way the jobs are.
	base, err := os.ReadFile(baseScrapeRules)
	if err != nil {
		t.Fatalf("reading %s: %v", baseScrapeRules, err)
	}
	if !bytes.Equal(raw, base) {
		t.Errorf("%s differs from %s, which it copies unchanged", scrapeRulesFile, baseScrapeRules)
	}

	// Without the replacing generator vmalert keeps the base's file, which is
	// the same today and drifts apart unnoticed the day either changes.
	k := kustomizationOf(t)
	i := slices.IndexFunc(k.ConfigMapGenerator, func(g generator) bool { return g.Name == scrapeRulesConfigMap })
	if i < 0 {
		t.Fatalf("%s generates no ConfigMap %s", kustomizationFile, scrapeRulesConfigMap)
	}
	g := k.ConfigMapGenerator[i]
	if g.Behavior != "replace" {
		t.Errorf("the %s generator has behavior %q, want \"replace\": that is what overrides the base's generated ConfigMap",
			scrapeRulesConfigMap, g.Behavior)
	}
	if !slices.Equal(g.Files, []string{scrapeRulesFile}) {
		t.Errorf("the %s generator carries %v, want exactly [%s]", scrapeRulesConfigMap, g.Files, scrapeRulesFile)
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
// the overlay deletes, sorted.
func deletedObjects(t *testing.T, k kustomization) []string {
	t.Helper()

	var deleted []string
	for _, d := range deletePatches(t, k) {
		deleted = append(deleted, d.Kind+"/"+d.Metadata.Name)
	}
	slices.Sort(deleted)
	return deleted
}

// deletePatches returns every strategic merge patch of the overlay that deletes
// an object. The JSON patches are sequences rather than documents and are
// skipped by their shape.
func deletePatches(t *testing.T, k kustomization) []deletePatch {
	t.Helper()

	var deleted []deletePatch
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
			deleted = append(deleted, doc)
		}
	}
	return deleted
}

// baseListeners returns the listener names of the base's Gateway, in order.
// Every document of the file is read, so the Gateway need not be the first.
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
// in a patch. raw is the whole object, which mountedSecretKeys and devImages
// walk.
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

// baseObjects decodes every object the YAML files of the base declare.
func baseObjects(t *testing.T) []object {
	t.Helper()
	return objectsIn(t, baseDir)
}

// keptObjects decodes every object the base and the three components that
// declare objects carry, less the objects the overlay deletes, which is what
// it renders before its other patches. The patch the reconciliation component
// carries is among them, so what it mounts counts as well.
func keptObjects(t *testing.T, k kustomization) []object {
	t.Helper()

	objects := baseObjects(t)
	for _, dir := range []string{collectorComponentDir, migrationsComponentDir, reconciliationComponentDir} {
		objects = append(objects, objectsIn(t, dir)...)
	}
	deleted := deletedObjects(t, k)
	return slices.DeleteFunc(objects, func(o object) bool { return slices.Contains(deleted, o.Kind+"/"+o.Metadata.Name) })
}

// objectsIn decodes every object the YAML files under one directory declare. A
// document without a kind and a name, such as a scrape or a provisioning file,
// is not an object and is skipped.
func objectsIn(t *testing.T, dir string) []object {
	t.Helper()

	var objects []object
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
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
		t.Fatalf("reading the objects of %s: %v", dir, err)
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
				t.Errorf("a pod mounts every key of secret %s, which no example can be checked against", name)
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

// devImages appends to names the name of every image value runs at the tag
// dev, which is what `make images` builds, wherever in it a pod spec names one.
func devImages(value any, names []string) []string {
	switch v := value.(type) {
	case map[string]any:
		if image, ok := v["image"].(string); ok {
			if name, found := strings.CutSuffix(image, ":dev"); found {
				names = append(names, name)
			}
		}
		for _, child := range v {
			names = devImages(child, names)
		}
	case []any:
		for _, child := range v {
			names = devImages(child, names)
		}
	}
	return names
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

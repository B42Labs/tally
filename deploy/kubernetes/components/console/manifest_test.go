// This file pins the console Deployment's contract with the binary, with the
// three Secrets it reads, with the Service of the base it reads, with the
// Gateway of the base its route attaches to and with the scrape job of the base
// that reads it. It fails quietly: a *_FILE path that stopped matching its
// mount leaves the console restarting on a file it cannot read, a listen
// address of 127.0.0.1 leaves the probes failing and the route answering 503, a
// URL naming a Service the base no longer declares leaves every page on an
// error, a route that names the ops port publishes the metrics without a
// credential, and a Service the scrape job does not keep leaves the console
// unscraped. None of it stops kustomize or an apply. The tests read the YAML
// from disk and need neither a cluster nor kustomize.
package console_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	manifestFile      = "console.yaml"
	kustomizationFile = "kustomization.yaml"

	// The name of every object of the component, and of the container.
	console = "console"

	// The files of the base that declare the Service the console reads, the
	// Gateway its route attaches to, and the scrape config that discovers it.
	reportingAPIFile = "../../base/reporting-api/reporting-api.yaml"
	baseGatewayFile  = "../../base/gateway/gateway.yaml"
	baseScrapeFile   = "../../base/victoriametrics/scrape.yaml"

	// The Gateway of the base and the listener the route attaches to.
	gateway  = "tally"
	listener = "https"

	// The group of Envoy Gateway's own kinds, which the base and this
	// component leave to the envoy-gateway component.
	envoyGroup = "gateway.envoyproxy.io"

	// The user the image runs as, distroless nonroot.
	nonroot = 65532
)

// fileSecrets is every secret the console takes from a mounted file, by the
// variable that names the file. plain is the variable that carries the same
// secret as an environment value, which this manifest must not set.
var fileSecrets = []struct {
	variable, plain, secret, key string
}{
	{"TALLY_CONSOLE_API_TOKEN_FILE", "TALLY_CONSOLE_API_TOKEN", "tally-console-token", "api-token"},
	{"TALLY_CONSOLE_ENGINE_DB_URL_FILE", "TALLY_CONSOLE_ENGINE_DB_URL", "tally-console-db", "db-url"},
	{"TALLY_CONSOLE_HTPASSWD_FILE", "TALLY_CONSOLE_HTPASSWD", "tally-console-auth", "htpasswd"},
}

// object is the part of a manifest document these tests assert over. yaml.v3
// ignores every field not named here, so one shape covers the Deployment, its
// Service, its route and the Service and the Gateway of the base.
type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// Deployment
		Template struct {
			Metadata struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"metadata"`
			Spec podSpec `yaml:"spec"`
		} `yaml:"template"`
		// Service. The selector is untyped because the Deployment's nests
		// matchLabels under the same key; a Service's is a flat map.
		Selector map[string]any `yaml:"selector"`
		Ports    []servicePort  `yaml:"ports"`
		// HTTPRoute
		ParentRefs []struct {
			Name        string `yaml:"name"`
			SectionName string `yaml:"sectionName"`
		} `yaml:"parentRefs"`
		Hostnames []string `yaml:"hostnames"`
		Rules     []struct {
			BackendRefs []struct {
				Name string `yaml:"name"`
				Port int    `yaml:"port"`
			} `yaml:"backendRefs"`
		} `yaml:"rules"`
		// Gateway
		Listeners []gatewayListener `yaml:"listeners"`
	} `yaml:"spec"`
}

type gatewayListener struct {
	Name string `yaml:"name"`
}

// scrapeJob is the part of a job of the scrape config these tests read.
type scrapeJob struct {
	JobName        string `yaml:"job_name"`
	RelabelConfigs []struct {
		Regex  string `yaml:"regex"`
		Action string `yaml:"action"`
	} `yaml:"relabel_configs"`
}

type servicePort struct {
	Name       string `yaml:"name"`
	Port       int    `yaml:"port"`
	TargetPort string `yaml:"targetPort"`
}

type podSpec struct {
	AutomountServiceAccountToken *bool          `yaml:"automountServiceAccountToken"`
	Containers                   []container    `yaml:"containers"`
	Volumes                      []volume       `yaml:"volumes"`
	SecurityContext              podSecurityCtx `yaml:"securityContext"`
}

// podSecurityCtx is the identity the pod runs under, and the group its volumes
// would be handed to.
type podSecurityCtx struct {
	RunAsNonRoot *bool `yaml:"runAsNonRoot"`
	RunAsUser    *int  `yaml:"runAsUser"`
	RunAsGroup   *int  `yaml:"runAsGroup"`
	FSGroup      *int  `yaml:"fsGroup"`
}

type container struct {
	Name            string               `yaml:"name"`
	Env             []envVar             `yaml:"env"`
	Ports           []containerPort      `yaml:"ports"`
	VolumeMounts    []volumeMount        `yaml:"volumeMounts"`
	SecurityContext containerSecurityCtx `yaml:"securityContext"`
	Resources       resources            `yaml:"resources"`
	LivenessProbe   probe                `yaml:"livenessProbe"`
	ReadinessProbe  probe                `yaml:"readinessProbe"`
}

// containerSecurityCtx is the confinement of the process, which the identity
// above does not reach.
type containerSecurityCtx struct {
	AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
	ReadOnlyRootFilesystem   *bool `yaml:"readOnlyRootFilesystem"`
	Capabilities             struct {
		Drop []string `yaml:"drop"`
	} `yaml:"capabilities"`
	SeccompProfile struct {
		Type string `yaml:"type"`
	} `yaml:"seccompProfile"`
}

// resources is what the scheduler places the pod against.
type resources struct {
	Requests struct {
		Memory string `yaml:"memory"`
	} `yaml:"requests"`
	Limits struct {
		Memory string `yaml:"memory"`
	} `yaml:"limits"`
}

type probe struct {
	HTTPGet struct {
		Path string `yaml:"path"`
		Port string `yaml:"port"`
	} `yaml:"httpGet"`
}

type containerPort struct {
	Name          string `yaml:"name"`
	ContainerPort int    `yaml:"containerPort"`
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
	Name   string `yaml:"name"`
	Secret struct {
		SecretName string       `yaml:"secretName"`
		Items      []secretItem `yaml:"items"`
	} `yaml:"secret"`
}

type secretItem struct {
	Key  string `yaml:"key"`
	Path string `yaml:"path"`
}

func TestKustomizationIsAComponentListingTheManifest(t *testing.T) {
	// An overlay lists this directory under components, and kustomize refuses a
	// Kustomization there. A manifest dropped from resources stays a file in
	// the repository that no overlay renders, and the cluster runs no console.
	var k struct {
		Kind      string   `yaml:"kind"`
		Resources []string `yaml:"resources"`
	}

	raw, err := os.ReadFile(kustomizationFile)
	if err != nil {
		t.Fatalf("reading %s: %v", kustomizationFile, err)
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parsing %s, which kustomize refuses to build: %v", kustomizationFile, err)
	}
	if k.Kind != "Component" {
		t.Errorf("%s has kind %q, want Component, which is what an overlay's components entry takes", kustomizationFile, k.Kind)
	}
	if want := []string{manifestFile}; !slices.Equal(k.Resources, want) {
		t.Errorf("%s lists the resources %v, want exactly %v", kustomizationFile, k.Resources, want)
	}
}

func TestTheConsoleIsConfinedToWhatItNeeds(t *testing.T) {
	// The image ends in distroless nonroot with USER nonroot:nonroot, which names
	// an identity and nothing else. This pod mounts the operator's htpasswd
	// line, a read_all token and the database password, and its pages face a
	// browser, so what a bug in the binary reaches is what these fields decide.
	// None of them fails a deployment when it is missing.
	doc := objectNamed(t, objects(t), "Deployment", console)
	pod := doc.Spec.Template.Spec
	c := containerOf(t, doc)

	if automount := pod.AutomountServiceAccountToken; automount == nil || *automount {
		t.Error("the pod does not set automountServiceAccountToken: false, although the console calls no Kubernetes API")
	}
	if runAsNonRoot := pod.SecurityContext.RunAsNonRoot; runAsNonRoot == nil || !*runAsNonRoot {
		t.Error("the pod does not set runAsNonRoot: true, so nothing but the image's own USER keeps the process off uid 0")
	}
	for field, got := range map[string]*int{
		"runAsUser":  pod.SecurityContext.RunAsUser,
		"runAsGroup": pod.SecurityContext.RunAsGroup,
	} {
		if got == nil || *got != nonroot {
			t.Errorf("%s = %s, want %d, the user the image runs as", field, intOrUnset(got), nonroot)
		}
	}
	if got := pod.SecurityContext.FSGroup; got != nil {
		t.Errorf("fsGroup = %d, want it unset: the console writes no volume, so a group to hand one to only widens what the pod may write", *got)
	}

	sc := c.SecurityContext
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("the container does not set allowPrivilegeEscalation: false, so a setuid binary reachable in the mount namespace can still raise its privileges")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("the container does not set readOnlyRootFilesystem: true, although the console writes nothing to disk")
	}
	if want := []string{"ALL"}; !slices.Equal(sc.Capabilities.Drop, want) {
		t.Errorf("capabilities.drop = %v, want %v; a process that binds unprivileged ports needs none of the default set", sc.Capabilities.Drop, want)
	}
	if want := "RuntimeDefault"; sc.SeccompProfile.Type != want {
		t.Errorf("seccompProfile.type = %q, want %q; without it the container runs with seccomp unconfined", sc.SeccompProfile.Type, want)
	}

	if c.Resources.Requests.Memory == "" {
		t.Error("no resources.requests.memory, so the pod is BestEffort and first in line for eviction")
	}
	if c.Resources.Limits.Memory == "" {
		t.Error("no resources.limits.memory, so a console that grows takes the node down rather than itself")
	}
}

func TestSecretsReachTheVariablesThatNameThem(t *testing.T) {
	// Three things have to agree: the variable names a path, a read-only mount
	// puts the Secret's directory at that path, and the volume carries the key
	// under the file name the path ends in. A mismatch in any of them leaves the
	// console restarting on a file it cannot read. The same secret set as a
	// plain variable is refused beside its *_FILE companion.
	doc := objectNamed(t, objects(t), "Deployment", console)
	c := containerOf(t, doc)

	for _, s := range fileSecrets {
		t.Run(s.variable, func(t *testing.T) {
			if _, set := envValue(c, s.plain); set {
				t.Errorf("%s is set, which the console refuses beside %s", s.plain, s.variable)
			}

			i := slices.IndexFunc(doc.Spec.Template.Spec.Volumes, func(v volume) bool { return v.Secret.SecretName == s.secret })
			if i < 0 {
				t.Fatalf("the pod declares no volume backed by secret %s, so %s names a file nothing mounts", s.secret, s.variable)
			}
			v := doc.Spec.Template.Spec.Volumes[i]
			if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != s.key {
				t.Fatalf("the %s volume carries the items %v, want the one key %s", s.secret, v.Secret.Items, s.key)
			}

			j := slices.IndexFunc(c.VolumeMounts, func(m volumeMount) bool { return m.Name == v.Name })
			if j < 0 {
				t.Fatalf("the container mounts %v and not volume %q, so secret %s reaches no path", c.VolumeMounts, v.Name, s.secret)
			}
			mount := c.VolumeMounts[j]
			if !mount.ReadOnly {
				t.Errorf("the mount of secret %s at %s is not read-only", s.secret, mount.MountPath)
			}

			value, set := envValue(c, s.variable)
			if !set {
				t.Fatalf("no %s, so the console looks for the secret in the environment, where this deployment does not put it", s.variable)
			}
			if want := path.Join(mount.MountPath, v.Secret.Items[0].Path); value != want {
				t.Errorf("%s = %q, want %q, which is where the %s volume mounts key %s", s.variable, value, want, s.secret, s.key)
			}
		})
	}

	// The three Secrets are what an overlay is told to supply. A fourth is one
	// no overlay creates, and the pod waits in ContainerCreating for it.
	var mounted, want []string
	for _, v := range doc.Spec.Template.Spec.Volumes {
		for _, item := range v.Secret.Items {
			mounted = append(mounted, v.Secret.SecretName+"/"+item.Key)
		}
	}
	for _, s := range fileSecrets {
		want = append(want, s.secret+"/"+s.key)
	}
	slices.Sort(mounted)
	slices.Sort(want)
	if !slices.Equal(mounted, want) {
		t.Errorf("the pod mounts the secret keys %v, want exactly %v, the ones %s tells an overlay to supply", mounted, want, kustomizationFile)
	}
}

func TestTheConsoleListensOnEveryInterface(t *testing.T) {
	// The console binds 127.0.0.1 unless told otherwise, which is an address
	// neither the kubelet nor the Gateway reaches: the probes fail and the route
	// answers 503. A port variable that differs from its container port leaves
	// the probes, the Service and the scrape job asking a port nothing listens
	// on.
	c := containerOf(t, objectNamed(t, objects(t), "Deployment", console))

	if value, _ := envValue(c, "TALLY_CONSOLE_HTTP_ADDR"); value != "0.0.0.0" {
		t.Errorf("TALLY_CONSOLE_HTTP_ADDR = %q, want 0.0.0.0, the address a pod is reached on from outside it", value)
	}
	for _, p := range []struct{ variable, port string }{
		{"TALLY_CONSOLE_HTTP_PORT", "http"},
		{"TALLY_CONSOLE_OPS_PORT", "ops"},
	} {
		i := slices.IndexFunc(c.Ports, func(cp containerPort) bool { return cp.Name == p.port })
		if i < 0 {
			t.Errorf("the container declares the ports %v and none named %s", c.Ports, p.port)
			continue
		}
		value, set := envValue(c, p.variable)
		if !set {
			t.Errorf("no %s, so the port the console listens on is its default and not one this manifest names", p.variable)
			continue
		}
		if want := strconv.Itoa(c.Ports[i].ContainerPort); value != want {
			t.Errorf("%s = %q, want %q, the container port named %s", p.variable, value, want, p.port)
		}
	}
}

func TestProbesAskTheOpsPort(t *testing.T) {
	// The page port wraps every path in the sign-in, so a probe there is
	// answered 401, and the kubelet restarts a healthy console for as long as
	// it runs.
	c := containerOf(t, objectNamed(t, objects(t), "Deployment", console))

	for _, p := range []struct {
		name, want string
		probe      probe
	}{
		{"livenessProbe", "/healthz", c.LivenessProbe},
		{"readinessProbe", "/readyz", c.ReadinessProbe},
	} {
		if got := p.probe.HTTPGet.Path; got != p.want {
			t.Errorf("%s asks %q, want %q", p.name, got, p.want)
		}
		if got := p.probe.HTTPGet.Port; got != "ops" {
			t.Errorf("%s asks port %q, want the port named ops, which serves the probes without a credential", p.name, got)
		}
	}
}

func TestTheConsoleReadsTheReportingAPIOfItsNamespace(t *testing.T) {
	// The URL names a Service of the base. A Service renamed there, or moved to
	// another port, leaves every page that reads the Reporting API on an error.
	// A plaintext URL without TALLY_CONSOLE_REPORTING_INSECURE is one the
	// console refuses at its start.
	const (
		variable = "TALLY_CONSOLE_REPORTING_URL"
		insecure = "TALLY_CONSOLE_REPORTING_INSECURE"
	)

	c := containerOf(t, objectNamed(t, objects(t), "Deployment", console))

	value, set := envValue(c, variable)
	if !set {
		t.Fatalf("no %s, so the console has nothing to read and refuses to start", variable)
	}
	u, err := url.Parse(value)
	if err != nil {
		t.Fatalf("%s = %q, which is no URL: %v", variable, value, err)
	}
	port := u.Port()
	if port == "" {
		// A URL without a port is one on the port of its scheme.
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}

	served := slices.ContainsFunc(objectsIn(t, reportingAPIFile), func(o object) bool {
		return o.Kind == "Service" && o.Metadata.Name == u.Hostname() &&
			slices.ContainsFunc(o.Spec.Ports, func(p servicePort) bool { return strconv.Itoa(p.Port) == port })
	})
	if !served {
		t.Errorf("%s = %q, but %s declares no Service %q on port %s", variable, value, reportingAPIFile, u.Hostname(), port)
	}

	got, _ := envValue(c, insecure)
	if allowed, err := strconv.ParseBool(got); err != nil || !allowed {
		t.Errorf("%s = %q beside %s = %q, want true, without which the console refuses a plaintext URL off loopback", insecure, got, variable, value)
	}
}

func TestTheRouteServesThePagesAlone(t *testing.T) {
	// The ops port answers /healthz, /readyz and /metrics without a credential,
	// so a route that reaches it publishes them on the console's hostname. A
	// parentRef naming a listener the Gateway does not have leaves the route
	// unattached, and the hostname answers nothing.
	docs := objects(t)
	route := objectNamed(t, docs, "HTTPRoute", console)
	svc := objectNamed(t, docs, "Service", console)

	if len(route.Spec.ParentRefs) != 1 || route.Spec.ParentRefs[0].Name != gateway || route.Spec.ParentRefs[0].SectionName != listener {
		t.Errorf("HTTPRoute %s attaches to %+v, want listener %s of Gateway %s alone", console, route.Spec.ParentRefs, listener, gateway)
	}
	gw := objectNamed(t, objectsIn(t, baseGatewayFile), "Gateway", gateway)
	if !slices.ContainsFunc(gw.Spec.Listeners, func(l gatewayListener) bool { return l.Name == listener }) {
		t.Errorf("Gateway %s in %s declares the listeners %v and none named %s", gateway, baseGatewayFile, gw.Spec.Listeners, listener)
	}
	// An overlay replaces the first hostname, so a second one would keep the
	// placeholder.
	if len(route.Spec.Hostnames) != 1 {
		t.Errorf("HTTPRoute %s carries the hostnames %v, want exactly one, the one an overlay replaces", console, route.Spec.Hostnames)
	}

	portOf := func(target string) int {
		i := slices.IndexFunc(svc.Spec.Ports, func(p servicePort) bool { return p.TargetPort == target })
		if i < 0 {
			t.Fatalf("Service %s declares the ports %+v and none targeting %s", console, svc.Spec.Ports, target)
		}
		return svc.Spec.Ports[i].Port
	}
	pages, ops := portOf("http"), portOf("ops")

	var backends []string
	for _, rule := range route.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			backends = append(backends, fmt.Sprintf("%s:%d", ref.Name, ref.Port))
			if ref.Name == console && ref.Port == ops {
				t.Errorf("HTTPRoute %s sends a rule to port %d of Service %s, which publishes the probes and the metrics without a credential", console, ops, console)
			}
		}
	}
	if want := []string{fmt.Sprintf("%s:%d", console, pages)}; !slices.Equal(backends, want) {
		t.Errorf("HTTPRoute %s sends its rules to %v, want exactly %v, the Service port that targets the pages", console, backends, want)
	}
}

func TestTheServiceCarriesThePortTheScrapeJobKeeps(t *testing.T) {
	// The store's console job discovers the pod through the endpointslice of
	// this Service and keeps the address whose Service and port names its regex
	// matches. A selector that misses the pod's labels leaves the slice empty,
	// and a port of another name is dropped by the keep rule; either way the
	// job resolves to no targets, and only TallyScrapeJobMissing says so.
	docs := objects(t)
	svc := objectNamed(t, docs, "Service", console)
	deployment := objectNamed(t, docs, "Deployment", console)

	selector := make(map[string]string, len(svc.Spec.Selector))
	for key, value := range svc.Spec.Selector {
		label, ok := value.(string)
		if !ok {
			t.Fatalf("Service %s selects %s by %v, which is no label value", console, key, value)
		}
		selector[key] = label
	}
	if labels := deployment.Spec.Template.Metadata.Labels; len(selector) == 0 || !maps.Equal(selector, labels) {
		t.Errorf("Service %s selects %v, want the pod labels %v of Deployment %s", console, selector, labels, console)
	}

	i := slices.IndexFunc(svc.Spec.Ports, func(p servicePort) bool { return p.Name == "ops" })
	if i < 0 {
		t.Fatalf("Service %s declares the ports %+v and none named ops, the one the scrape job keeps", console, svc.Spec.Ports)
	}
	port := svc.Spec.Ports[i]
	if port.TargetPort != "ops" {
		t.Errorf("Service %s port ops targets %q, want \"ops\", the container port that serves /metrics", console, port.TargetPort)
	}
	ports := containerOf(t, deployment).Ports
	j := slices.IndexFunc(ports, func(p containerPort) bool { return p.Name == port.TargetPort })
	if j < 0 {
		t.Fatalf("the console container declares no port named %q, which Service %s targets", port.TargetPort, console)
	}
	if port.Port != ports[j].ContainerPort {
		t.Errorf("Service %s declares port %d, want %d, the container port it targets", console, port.Port, ports[j].ContainerPort)
	}

	var cfg struct {
		ScrapeConfigs []scrapeJob `yaml:"scrape_configs"`
	}
	raw, err := os.ReadFile(baseScrapeFile)
	if err != nil {
		t.Fatalf("reading %s: %v", baseScrapeFile, err)
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parsing %s: %v", baseScrapeFile, err)
	}
	k := slices.IndexFunc(cfg.ScrapeConfigs, func(j scrapeJob) bool { return j.JobName == console })
	if k < 0 {
		t.Fatalf("%s declares no job %s, so nothing scrapes the console", baseScrapeFile, console)
	}
	want := svc.Metadata.Name + ";" + port.Name
	kept := false
	for _, r := range cfg.ScrapeConfigs[k].RelabelConfigs {
		if r.Action == "keep" && r.Regex == want {
			kept = true
		}
	}
	if !kept {
		t.Errorf("job %s of %s keeps no %q, so it resolves to no targets and nothing scrapes the console", console, baseScrapeFile, want)
	}
}

func TestTheComponentNamesNoImplementation(t *testing.T) {
	// Both overlays list this component, and so can a cluster behind another
	// Gateway API implementation, which has no CRD for Envoy Gateway's kinds.
	// What the console route takes from Envoy Gateway, its rate limit, is the
	// envoy-gateway component's. This is the rule TestBaseNamesNoImplementation
	// holds the base to.
	raw, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatalf("reading %s: %v", manifestFile, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("parsing %s: %v", manifestFile, err)
		}
		kind, _ := doc["kind"].(string)
		if apiVersion, _ := doc["apiVersion"].(string); strings.HasPrefix(apiVersion, envoyGroup+"/") {
			t.Errorf("%s declares %s in the API group %s, which belongs in the envoy-gateway component", manifestFile, kind, envoyGroup)
		}
		var walk func(v any, at string)
		walk = func(v any, at string) {
			switch v := v.(type) {
			case map[string]any:
				if v["group"] == envoyGroup {
					t.Errorf("%s declares %s, which references the API group %s at %s", manifestFile, kind, envoyGroup, at)
				}
				if v["type"] == "ExtensionRef" {
					t.Errorf("%s declares %s, which carries an ExtensionRef filter at %s, and what it references is one implementation's kind", manifestFile, kind, at)
				}
				for key, child := range v {
					walk(child, at+"/"+key)
				}
			case []any:
				for i, child := range v {
					walk(child, fmt.Sprintf("%s/%d", at, i))
				}
			}
		}
		walk(doc, "")
	}
}

// objects decodes every document of the manifest.
func objects(t *testing.T) []object {
	t.Helper()
	return objectsIn(t, manifestFile)
}

// objectsIn decodes every document of one file.
func objectsIn(t *testing.T, file string) []object {
	t.Helper()

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}

	var docs []object
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc object
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		docs = append(docs, doc)
	}
}

// objectNamed returns one document, failing if it is missing rather than
// asserting over a zero value.
func objectNamed(t *testing.T, docs []object, kind, name string) object {
	t.Helper()

	for _, doc := range docs {
		if doc.Kind == kind && doc.Metadata.Name == name {
			return doc
		}
	}
	t.Fatalf("no %s named %q among the documents read", kind, name)
	return object{}
}

// containerOf returns the console container of the Deployment's pod.
func containerOf(t *testing.T, doc object) container {
	t.Helper()

	for _, c := range doc.Spec.Template.Spec.Containers {
		if c.Name == console {
			return c
		}
	}
	t.Fatalf("%s %s carries no container %q", doc.Kind, doc.Metadata.Name, console)
	return container{}
}

// envValue reports what one variable of a container is set to, and whether it
// is set at all.
func envValue(c container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// intOrUnset prints an optional number for a failure message.
func intOrUnset(n *int) string {
	if n == nil {
		return "unset"
	}
	return strconv.Itoa(*n)
}

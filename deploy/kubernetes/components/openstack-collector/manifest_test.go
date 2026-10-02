// This file pins the collector Deployment's contract with the binary, with the
// two Secrets it reads, with the claim its outbox lies on and with the Service
// of the base it posts to. It fails quietly: a second replica or a rolling
// update puts two writers on one outbox file, a volume the process cannot write
// ends the pod on its first start, a *_FILE path that stopped matching its
// mount leaves the collector restarting on a file it cannot read, and a URL
// naming a Service the base no longer declares leaves every event in the
// outbox. None of it stops kustomize or an apply. The tests read the YAML from
// disk and need neither a cluster nor kustomize.
package openstackcollector_test

import (
	"bytes"
	"errors"
	"io"
	"net/url"
	"os"
	"path"
	"slices"
	"strconv"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	manifestFile      = "openstack-collector.yaml"
	kustomizationFile = "kustomization.yaml"

	// The Deployment and the container inside it, which carry the same name,
	// and the claim the outbox lies on.
	collector   = "openstack-collector"
	outboxClaim = "openstack-collector-outbox"

	// The ConfigMap an overlay generates the non-secret settings into.
	settingsConfigMap = "tally-openstack-collector"

	// The file of the base that declares the Service the collector posts to.
	reportingAPIFile = "../../base/reporting-api/reporting-api.yaml"

	// The user the image runs as, distroless nonroot.
	nonroot = 65532
)

// fileSecrets is every secret the collector takes from a mounted file, by the
// variable that names the file. plain is the variable that carries the same
// secret as an environment value, which this manifest must not set.
var fileSecrets = []struct {
	variable, plain, secret, key string
}{
	{"TALLY_OSC_AMQP_URL_FILE", "TALLY_OSC_AMQP_URL", "tally-collector-amqp", "amqp-url"},
	{"TALLY_OSC_TOKEN_FILE", "TALLY_OSC_TOKEN", "tally-collector-token", "ingest-token"},
}

// object is the part of a manifest document these tests assert over. yaml.v3
// ignores every field not named here, so one shape covers the claim, the
// Deployment and the Service of the base.
type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// PersistentVolumeClaim
		AccessModes []string `yaml:"accessModes"`
		// Deployment
		Replicas *int `yaml:"replicas"`
		Strategy struct {
			Type string `yaml:"type"`
		} `yaml:"strategy"`
		Template struct {
			Spec podSpec `yaml:"spec"`
		} `yaml:"template"`
		// Service
		Ports []servicePort `yaml:"ports"`
	} `yaml:"spec"`
}

type servicePort struct {
	Port int `yaml:"port"`
}

type podSpec struct {
	AutomountServiceAccountToken *bool          `yaml:"automountServiceAccountToken"`
	Containers                   []container    `yaml:"containers"`
	Volumes                      []volume       `yaml:"volumes"`
	SecurityContext              podSecurityCtx `yaml:"securityContext"`
}

// podSecurityCtx is the identity the pod runs under, and the group its volumes
// are handed to.
type podSecurityCtx struct {
	RunAsNonRoot *bool `yaml:"runAsNonRoot"`
	RunAsUser    *int  `yaml:"runAsUser"`
	RunAsGroup   *int  `yaml:"runAsGroup"`
	FSGroup      *int  `yaml:"fsGroup"`
}

type container struct {
	Name    string `yaml:"name"`
	EnvFrom []struct {
		ConfigMapRef struct {
			Name string `yaml:"name"`
		} `yaml:"configMapRef"`
	} `yaml:"envFrom"`
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
	TimeoutSeconds int `yaml:"timeoutSeconds"`
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
	PersistentVolumeClaim struct {
		ClaimName string `yaml:"claimName"`
	} `yaml:"persistentVolumeClaim"`
}

type secretItem struct {
	Key  string `yaml:"key"`
	Path string `yaml:"path"`
}

func TestKustomizationIsAComponentListingTheManifest(t *testing.T) {
	// An overlay lists this directory under components, and kustomize refuses a
	// Kustomization there. A manifest dropped from resources stays a file in
	// the repository that no overlay renders, and the cluster runs no collector.
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

func TestOneCollectorWritesTheOutbox(t *testing.T) {
	// The outbox is one SQLite file on a ReadWriteOnce claim. A second replica
	// opens the file of the first, and a rolling update starts the new pod while
	// the old one still holds it. Recreate is also what replaces a pod that
	// never became Ready, such as one with a wrong broker URL.
	doc := objectNamed(t, objects(t), "Deployment", collector)

	if got := doc.Spec.Replicas; got == nil || *got != 1 {
		t.Errorf("replicas = %s, want 1; a second collector would open the outbox file of the first", intOrUnset(got))
	}
	if got, want := doc.Spec.Strategy.Type, "Recreate"; got != want {
		t.Errorf("strategy.type = %q, want %q, which ends the old pod before the new one opens the outbox", got, want)
	}
}

func TestTheCollectorIsConfinedToWhatItNeeds(t *testing.T) {
	// The image ends in distroless nonroot with USER nonroot:nonroot, which names
	// an identity and nothing else. This pod mounts the broker URL with its
	// password and the ingest token, so what a bug in the binary reaches is what
	// these fields decide. Nothing here fails a deployment but fsGroup: a fresh
	// volume is root-owned, and without the group the collector exits on its
	// first start with "unable to open database file (14)".
	doc := objectNamed(t, objects(t), "Deployment", collector)
	pod := doc.Spec.Template.Spec
	c := containerOf(t, doc)

	if automount := pod.AutomountServiceAccountToken; automount == nil || *automount {
		t.Error("the pod does not set automountServiceAccountToken: false, although the collector calls no Kubernetes API")
	}
	if runAsNonRoot := pod.SecurityContext.RunAsNonRoot; runAsNonRoot == nil || !*runAsNonRoot {
		t.Error("the pod does not set runAsNonRoot: true, so nothing but the image's own USER keeps the process off uid 0")
	}
	for field, got := range map[string]*int{
		"runAsUser":  pod.SecurityContext.RunAsUser,
		"runAsGroup": pod.SecurityContext.RunAsGroup,
		"fsGroup":    pod.SecurityContext.FSGroup,
	} {
		if got == nil || *got != nonroot {
			t.Errorf("%s = %s, want %d, the user the image runs as and the group the outbox volume has to be writable for",
				field, intOrUnset(got), nonroot)
		}
	}

	sc := c.SecurityContext
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("the container does not set allowPrivilegeEscalation: false, so a setuid binary reachable in the mount namespace can still raise its privileges")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("the container does not set readOnlyRootFilesystem: true, although the collector writes to the outbox volume and to nothing else on disk")
	}
	if want := []string{"ALL"}; !slices.Equal(sc.Capabilities.Drop, want) {
		t.Errorf("capabilities.drop = %v, want %v; a process that binds an unprivileged port needs none of the default set", sc.Capabilities.Drop, want)
	}
	if want := "RuntimeDefault"; sc.SeccompProfile.Type != want {
		t.Errorf("seccompProfile.type = %q, want %q; without it the container runs with seccomp unconfined", sc.SeccompProfile.Type, want)
	}

	if c.Resources.Requests.Memory == "" {
		t.Error("no resources.requests.memory, so the pod is BestEffort and first in line for eviction")
	}
	if c.Resources.Limits.Memory == "" {
		t.Error("no resources.limits.memory, so a collector that grows takes the node down rather than itself")
	}
}

func TestSecretsReachTheVariablesThatNameThem(t *testing.T) {
	// Three things have to agree: the variable names a path, a read-only mount
	// puts the Secret's directory at that path, and the volume carries the key
	// under the file name the path ends in. A mismatch in any of them leaves the
	// collector restarting on a file it cannot read. The same secret set as a
	// plain variable is refused beside its *_FILE companion.
	doc := objectNamed(t, objects(t), "Deployment", collector)
	c := containerOf(t, doc)

	for _, s := range fileSecrets {
		t.Run(s.variable, func(t *testing.T) {
			if _, set := envValue(c, s.plain); set {
				t.Errorf("%s is set, which the collector refuses beside %s", s.plain, s.variable)
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
				t.Fatalf("no %s, so the collector looks for the secret in the environment, where this deployment does not put it", s.variable)
			}
			if want := path.Join(mount.MountPath, v.Secret.Items[0].Path); value != want {
				t.Errorf("%s = %q, want %q, which is where the %s volume mounts key %s", s.variable, value, want, s.secret, s.key)
			}
		})
	}
}

func TestTheOutboxLiesOnTheClaim(t *testing.T) {
	// An outbox path outside the claim's mount is a path on the read-only root
	// filesystem, and the collector exits on its first start. A path on an
	// emptyDir would start and lose every buffered event with the pod.
	const variable = "TALLY_OSC_BUFFER_PATH"

	docs := objects(t)
	doc := objectNamed(t, docs, "Deployment", collector)
	c := containerOf(t, doc)

	i := slices.IndexFunc(doc.Spec.Template.Spec.Volumes, func(v volume) bool {
		return v.PersistentVolumeClaim.ClaimName == outboxClaim
	})
	if i < 0 {
		t.Fatalf("the pod declares no volume backed by claim %s, so the outbox does not outlive the pod", outboxClaim)
	}
	name := doc.Spec.Template.Spec.Volumes[i].Name

	j := slices.IndexFunc(c.VolumeMounts, func(m volumeMount) bool { return m.Name == name })
	if j < 0 {
		t.Fatalf("the container mounts %v and not volume %q, so the claim reaches no path", c.VolumeMounts, name)
	}
	mount := c.VolumeMounts[j]
	if mount.ReadOnly {
		t.Errorf("the mount of claim %s at %s is read-only, so the collector cannot write its outbox", outboxClaim, mount.MountPath)
	}

	value, set := envValue(c, variable)
	if !set {
		t.Fatalf("no %s, so the collector has no outbox path and refuses to start", variable)
	}
	if got := path.Dir(value); got != mount.MountPath {
		t.Errorf("%s = %q, whose directory is %q, want a file in %q, where claim %s is mounted", variable, value, got, mount.MountPath, outboxClaim)
	}

	claim := objectNamed(t, docs, "PersistentVolumeClaim", outboxClaim)
	if want := []string{"ReadWriteOnce"}; !slices.Equal(claim.Spec.AccessModes, want) {
		t.Errorf("claim %s has the access modes %v, want %v: one pod writes the outbox", outboxClaim, claim.Spec.AccessModes, want)
	}
}

func TestProbesAskTheRoutesTheCollectorServes(t *testing.T) {
	// Liveness weighs the outbox alone and readiness the broker session too. The
	// two swapped restart the pod for as long as the broker is unreachable,
	// which no restart mends. A probe on a port the process does not listen on
	// fails every time, and the kubelet restarts a healthy collector. So does a
	// probe the kubelet gives up on before the handler has answered.
	const (
		variable = "TALLY_OSC_HTTP_PORT"

		// What the handler gives its outbox check, in seconds: probeTimeout in
		// cmd/tally-openstack-collector/main.go. The kubelet's default is one.
		handlerBudget = 2
	)

	c := containerOf(t, objectNamed(t, objects(t), "Deployment", collector))

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
		if got := p.probe.HTTPGet.Port; got != "http" {
			t.Errorf("%s asks port %q, want the port named http", p.name, got)
		}
		if got := p.probe.TimeoutSeconds; got <= handlerBudget {
			t.Errorf("%s has timeoutSeconds %d, want more than the %d the handler gives its outbox check", p.name, got, handlerBudget)
		}
	}

	i := slices.IndexFunc(c.Ports, func(p containerPort) bool { return p.Name == "http" })
	if i < 0 {
		t.Fatalf("the container declares the ports %v and none named http, which both probes ask", c.Ports)
	}
	value, set := envValue(c, variable)
	if !set {
		t.Fatalf("no %s, so the port the collector listens on is its default and not one this manifest names", variable)
	}
	if want := strconv.Itoa(c.Ports[i].ContainerPort); value != want {
		t.Errorf("%s = %q, want %q, the container port named http", variable, value, want)
	}
}

func TestSettingsComeFromTheConfigMap(t *testing.T) {
	// env wins over envFrom. A TALLY_OSC_CLOUD set here would be the cloud of
	// every deployment, whatever its overlay generates, and usage would be
	// booked to it. Without the envFrom entry the collector has no cloud and
	// exits on its first start.
	c := containerOf(t, objectNamed(t, objects(t), "Deployment", collector))

	if len(c.EnvFrom) != 1 || c.EnvFrom[0].ConfigMapRef.Name != settingsConfigMap {
		t.Errorf("envFrom = %+v, want one configMapRef named %s, the ConfigMap an overlay generates the settings into", c.EnvFrom, settingsConfigMap)
	}
	if value, set := envValue(c, "TALLY_OSC_CLOUD"); set {
		t.Errorf("env sets TALLY_OSC_CLOUD to %q, which overrides the cloud every overlay names in ConfigMap %s", value, settingsConfigMap)
	}
}

func TestTheCollectorPostsToTheReportingAPIOfItsNamespace(t *testing.T) {
	// The URL names a Service of the base. A Service renamed there, or moved to
	// another port, leaves the collector posting to an address nothing answers,
	// and every event stays in the outbox. A plaintext URL without
	// TALLY_OSC_REPORTING_INSECURE is one the collector refuses at its start, in
	// a pod make prod-up does not wait on.
	const (
		variable = "TALLY_OSC_REPORTING_URL"
		insecure = "TALLY_OSC_REPORTING_INSECURE"
	)

	c := containerOf(t, objectNamed(t, objects(t), "Deployment", collector))

	value, set := envValue(c, variable)
	if !set {
		t.Fatalf("no %s, so the collector has nowhere to post and refuses to start", variable)
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

	if u.Scheme != "https" {
		got, _ := envValue(c, insecure)
		if allowed, err := strconv.ParseBool(got); err != nil || !allowed {
			t.Errorf("%s = %q beside %s = %q, want true, without which the collector refuses a URL that is not https", insecure, got, variable, value)
		}
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
	t.Fatalf("%s declares no %s named %q", manifestFile, kind, name)
	return object{}
}

// containerOf returns the collector container of the Deployment's pod.
func containerOf(t *testing.T, doc object) container {
	t.Helper()

	for _, c := range doc.Spec.Template.Spec.Containers {
		if c.Name == collector {
			return c
		}
	}
	t.Fatalf("%s %s carries no container %q", doc.Kind, doc.Metadata.Name, collector)
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

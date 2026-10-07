// This file pins the migration Job's contract with the two binaries, with the
// Secret it reads both connection strings from and with the Service of the
// base it waits for. It fails quietly: the engine chain applied first runs
// before the reporting chain it reads through, a *_FILE path that stopped
// matching its mount fails every retry on a file the process cannot read, and a
// deadline kills a build migration 9 runs outside a transaction, which leaves
// an index the operator drops by hand. None of it stops kustomize or an apply.
// The tests read the YAML from disk and need neither a cluster nor kustomize.
package migrations_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	manifestFile      = "migrations.yaml"
	kustomizationFile = "kustomization.yaml"

	// The Job, and the Secret both connection strings are mounted from.
	job      = "tally-migrate"
	dbSecret = "tally-db"

	// The file of the base that declares the Service the Job waits for.
	timescaleDBFile = "../../base/timescaledb/timescaledb.yaml"

	// The user the images run as, distroless nonroot.
	nonroot = 65532
)

// chains is every migration the Job applies, in the order it applies them: the
// container that runs it, the image and the variable that names the file of
// its connection string, the variable that would carry the same string as an
// environment value, which this manifest must not set, and the key of Secret
// tally-db that string is mounted from.
var chains = []struct {
	container, image, variable, plain, key string
}{
	{"reporting", "tally-reporting-admin:dev", "TALLY_REPORTING_DB_URL_FILE", "TALLY_REPORTING_DB_URL", "db-url"},
	{"engine", "tally-engine:dev", "TALLY_ENGINE_DB_URL_FILE", "TALLY_ENGINE_DB_URL", "engine-db-url"},
}

// waitRe is the port check of the wait container: the host and the port it
// asks for.
var waitRe = regexp.MustCompile(`\bnc -z ([a-z0-9.-]+) ([0-9]+)\b`)

// object is the part of a manifest document these tests assert over. yaml.v3
// ignores every field not named here, so one shape covers the Job and the
// Service of the base.
type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// Job
		BackoffLimit            *int `yaml:"backoffLimit"`
		ActiveDeadlineSeconds   *int `yaml:"activeDeadlineSeconds"`
		TTLSecondsAfterFinished *int `yaml:"ttlSecondsAfterFinished"`
		Template                struct {
			Metadata struct {
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
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
	RestartPolicy                string         `yaml:"restartPolicy"`
	AutomountServiceAccountToken *bool          `yaml:"automountServiceAccountToken"`
	InitContainers               []container    `yaml:"initContainers"`
	Containers                   []container    `yaml:"containers"`
	Volumes                      []volume       `yaml:"volumes"`
	SecurityContext              podSecurityCtx `yaml:"securityContext"`
}

// podSecurityCtx is the identity the pod runs under.
type podSecurityCtx struct {
	RunAsNonRoot *bool `yaml:"runAsNonRoot"`
	RunAsUser    *int  `yaml:"runAsUser"`
	RunAsGroup   *int  `yaml:"runAsGroup"`
}

type container struct {
	Name            string               `yaml:"name"`
	Image           string               `yaml:"image"`
	Command         []string             `yaml:"command"`
	Args            []string             `yaml:"args"`
	Env             []envVar             `yaml:"env"`
	VolumeMounts    []volumeMount        `yaml:"volumeMounts"`
	SecurityContext containerSecurityCtx `yaml:"securityContext"`
	Resources       resources            `yaml:"resources"`
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
		CPU    string `yaml:"cpu"`
		Memory string `yaml:"memory"`
	} `yaml:"requests"`
	Limits struct {
		Memory string `yaml:"memory"`
	} `yaml:"limits"`
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
	// the repository that no overlay renders, and nothing migrates the cluster.
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

func TestTheReportingChainIsAppliedBeforeTheEngineChain(t *testing.T) {
	// Init containers run one after the other and the main container after
	// them, so their order is the order of the chains. The reporting chain goes
	// first, as make migrate applies it, and both go after the wait: a chain
	// that starts before the database answers fails the pod, and the Job
	// spends a retry on it.
	pod := podOf(t)

	var names []string
	for _, c := range pod.InitContainers {
		names = append(names, c.Name)
	}
	if want := []string{"wait-for-timescaledb", chains[0].container}; !slices.Equal(names, want) {
		t.Errorf("the init containers are %v, want %v in that order: the wait, then the reporting chain", names, want)
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Name != chains[1].container {
		t.Errorf("the Job runs %d containers, the first named %q, want the one container %s, which runs after every init container",
			len(pod.Containers), nameOfFirst(pod.Containers), chains[1].container)
	}

	for _, chain := range chains {
		c := containerNamed(t, pod, chain.container)
		if c.Image != chain.image {
			t.Errorf("container %s runs %q, want %s, the image an overlay's images entry maps to the release", c.Name, c.Image, chain.image)
		}
		if want := []string{"migrate"}; !slices.Equal(c.Args, want) || len(c.Command) != 0 {
			t.Errorf("container %s runs the command %v with the arguments %v, want the image's entrypoint with %v",
				c.Name, c.Command, c.Args, want)
		}
	}
}

func TestDatabaseSecretsReachTheVariablesThatNameThem(t *testing.T) {
	// Three things have to agree: the variable names a path, a read-only mount
	// puts the Secret's directory at that path, and the volume carries the key
	// under the file name the path ends in. A mismatch in any of them fails
	// every retry on a file the process cannot read. The same string set as a
	// plain variable is refused beside its *_FILE companion.
	pod := podOf(t)

	i := slices.IndexFunc(pod.Volumes, func(v volume) bool { return v.Secret.SecretName == dbSecret })
	if i < 0 {
		t.Fatalf("the pod declares no volume backed by secret %s, so neither connection string reaches a path", dbSecret)
	}
	v := pod.Volumes[i]

	var keys []string
	for _, item := range v.Secret.Items {
		keys = append(keys, item.Key)
	}
	if want := []string{chains[0].key, chains[1].key}; !slices.Equal(keys, want) {
		t.Errorf("the %s volume carries the keys %v, want exactly %v", dbSecret, keys, want)
	}

	for _, chain := range chains {
		t.Run(chain.variable, func(t *testing.T) {
			c := containerNamed(t, pod, chain.container)

			if _, set := envValue(c, chain.plain); set {
				t.Errorf("%s is set, which the binary refuses beside %s", chain.plain, chain.variable)
			}

			j := slices.IndexFunc(v.Secret.Items, func(item secretItem) bool { return item.Key == chain.key })
			if j < 0 {
				t.Fatalf("the %s volume carries no key %s", dbSecret, chain.key)
			}
			k := slices.IndexFunc(c.VolumeMounts, func(m volumeMount) bool { return m.Name == v.Name })
			if k < 0 {
				t.Fatalf("container %s mounts %v and not volume %q, so secret %s reaches no path", c.Name, c.VolumeMounts, v.Name, dbSecret)
			}
			mount := c.VolumeMounts[k]
			if !mount.ReadOnly {
				t.Errorf("container %s mounts secret %s at %s, and not read-only", c.Name, dbSecret, mount.MountPath)
			}

			value, set := envValue(c, chain.variable)
			if !set {
				t.Fatalf("no %s on container %s, so the binary looks for the connection string in the environment, where this Job does not put it",
					chain.variable, c.Name)
			}
			if want := path.Join(mount.MountPath, v.Secret.Items[j].Path); value != want {
				t.Errorf("%s = %q, want %q, which is where the %s volume mounts key %s", chain.variable, value, want, dbSecret, chain.key)
			}
		})
	}
}

func TestTheJobWaitsForTheServiceTheBaseDeclares(t *testing.T) {
	// The Postgres entrypoint listens on TCP only after its initdb scripts ran,
	// so an open port means the engine's database and the reader roles exist. A
	// wait on a name or a port the base no longer declares never ends, and the
	// pod sits in Init:0/2 with nothing failing.
	c := containerNamed(t, podOf(t), "wait-for-timescaledb")

	m := waitRe.FindStringSubmatch(strings.Join(c.Command, " "))
	if m == nil {
		t.Fatalf("the wait container runs %q, which carries no nc -z <host> <port>", c.Command)
	}
	host, port := m[1], m[2]

	served := slices.ContainsFunc(objectsIn(t, timescaleDBFile), func(o object) bool {
		return o.Kind == "Service" && o.Metadata.Name == host &&
			slices.ContainsFunc(o.Spec.Ports, func(p servicePort) bool { return strconv.Itoa(p.Port) == port })
	})
	if !served {
		t.Errorf("the wait asks for %s:%s, but %s declares no Service %q on port %s", host, port, timescaleDBFile, host, port)
	}
}

func TestAFailedMigrationIsRetriedAndNeverKilled(t *testing.T) {
	// A database that restarts under the Job fails it, which happens on the
	// deploy that changes Secret tally-db, so the Job retries. Migration 9
	// builds an index outside a transaction, and a deadline that kills the
	// build leaves the chunks it finished indexed with the migration
	// unrecorded, which the operator repairs by hand, and so does a cluster
	// autoscaler or Karpenter that evicts the pod to scale its node down or
	// consolidate it. Each reads its own annotation. A finished Job
	// removed by a TTL takes the log make prod-up and the operator read with
	// it.
	doc := objectNamed(t, objects(t), "Job", job)

	if got := doc.Spec.BackoffLimit; got == nil || *got != 3 {
		t.Errorf("backoffLimit = %s, want 3, which rides out a database that restarts under the Job", intOrUnset(got))
	}
	if got := doc.Spec.Template.Spec.RestartPolicy; got != "Never" {
		t.Errorf("restartPolicy = %q, want Never, so each retry is a pod of its own with its own log", got)
	}
	if got := doc.Spec.ActiveDeadlineSeconds; got != nil {
		t.Errorf("activeDeadlineSeconds = %d, want unset: a deadline can kill migration 9 in the middle of its index build", *got)
	}
	if got := doc.Spec.TTLSecondsAfterFinished; got != nil {
		t.Errorf("ttlSecondsAfterFinished = %d, want unset: the finished Job keeps its log until the next deploy replaces it", *got)
	}
	for key, want := range map[string]string{
		"cluster-autoscaler.kubernetes.io/safe-to-evict": "false",
		"karpenter.sh/do-not-disrupt":                    "true",
	} {
		if got := doc.Spec.Template.Metadata.Annotations[key]; got != want {
			t.Errorf("the pod template carries %s = %q, want %q: a scale-down can evict the pod in the middle of migration 9", key, got, want)
		}
	}
}

func TestTheJobIsConfinedToWhatItNeeds(t *testing.T) {
	// The images end in distroless nonroot with USER nonroot:nonroot, which
	// names an identity and nothing else. This pod mounts the connection
	// strings of both database owners, so what a bug in a binary reaches is
	// what these fields decide. Without a request the pod is BestEffort, and an
	// eviction in the middle of migration 9 leaves the index the how-to drops by
	// hand.
	pod := podOf(t)

	if automount := pod.AutomountServiceAccountToken; automount == nil || *automount {
		t.Error("the pod does not set automountServiceAccountToken: false, although the migrations call no Kubernetes API")
	}
	if runAsNonRoot := pod.SecurityContext.RunAsNonRoot; runAsNonRoot == nil || !*runAsNonRoot {
		t.Error("the pod does not set runAsNonRoot: true, so nothing but the image's own USER keeps the process off uid 0")
	}
	for field, got := range map[string]*int{
		"runAsUser":  pod.SecurityContext.RunAsUser,
		"runAsGroup": pod.SecurityContext.RunAsGroup,
	} {
		if got == nil || *got != nonroot {
			t.Errorf("%s = %s, want %d, the user the images run as", field, intOrUnset(got), nonroot)
		}
	}

	all := append(slices.Clone(pod.InitContainers), pod.Containers...)
	if len(all) != 3 {
		t.Fatalf("the pod runs %d containers, want the wait and the two chains", len(all))
	}
	for _, c := range all {
		t.Run(c.Name, func(t *testing.T) {
			sc := c.SecurityContext
			if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Error("allowPrivilegeEscalation is not false, so a setuid binary reachable in the mount namespace can still raise its privileges")
			}
			if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
				t.Error("readOnlyRootFilesystem is not true, although the container writes nothing to disk")
			}
			if want := []string{"ALL"}; !slices.Equal(sc.Capabilities.Drop, want) {
				t.Errorf("capabilities.drop = %v, want %v; a process that dials a database needs none of the default set", sc.Capabilities.Drop, want)
			}
			if want := "RuntimeDefault"; sc.SeccompProfile.Type != want {
				t.Errorf("seccompProfile.type = %q, want %q; without it the container runs with seccomp unconfined", sc.SeccompProfile.Type, want)
			}
			if c.Resources.Requests.CPU == "" || c.Resources.Requests.Memory == "" {
				t.Errorf("resources.requests = %+v, want cpu and memory, without which the pod is BestEffort and first in line for eviction",
					c.Resources.Requests)
			}
			if c.Resources.Limits.Memory == "" {
				t.Error("no resources.limits.memory, so a container that grows takes the node down rather than itself")
			}
		})
	}
}

// podOf returns the pod template of the Job.
func podOf(t *testing.T) podSpec {
	t.Helper()
	return objectNamed(t, objects(t), "Job", job).Spec.Template.Spec
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

// containerNamed returns one init or main container of the pod.
func containerNamed(t *testing.T, pod podSpec, name string) container {
	t.Helper()

	for _, c := range append(slices.Clone(pod.InitContainers), pod.Containers...) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the pod of Job %s carries no container %q", job, name)
	return container{}
}

// nameOfFirst names the first container of a list for a failure message.
func nameOfFirst(cs []container) string {
	if len(cs) == 0 {
		return ""
	}
	return cs[0].Name
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

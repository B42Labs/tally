// This file pins the scheduler CronJob's contract with the engine binary and
// with the secret it reads its two databases from. It fails quietly: a manifest
// that lost the tick argument runs the engine's help text and the Job succeeds,
// a *_FILE path that stopped matching the mount leaves every hour failing on a
// connection string the process cannot read, and a manifest without
// TALLY_ENGINE_COUNTER_SOURCES falls back to the default sources path, which
// errors on a cluster that mounts no file there. None of it stalls a rollout or
// fails a probe; it ends in a Job history nobody reads. The test reads the YAML
// from disk and needs no cluster.
package tallyengine_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	manifestFile      = "tally-engine.yaml"
	kustomizationFile = "kustomization.yaml"

	// The CronJob and the container inside it, which carry the same name.
	cronJob = "tally-engine"

	// The secret both connection strings come from and the directory the volume
	// carrying them is mounted at. Every *_FILE value below names one file in it.
	dbSecret  = "tally-db"
	secretDir = "/run/secrets/tally"
)

// dbSecretVars is every connection string this CronJob takes from dbSecret, by
// the variable that names the file it lands in. The tests below walk the map, so
// a secret the manifest adds belongs here and nowhere else in this file.
var dbSecretVars = map[string]string{
	"TALLY_ENGINE_DB_URL_FILE":           "engine-db-url",
	"TALLY_ENGINE_REPORTING_DB_URL_FILE": "engine-reporting-db-url",
}

// secretPath is where one key of dbSecret is mounted inside the container.
func secretPath(key string) string { return secretDir + "/" + key }

// object is the part of a manifest document this test asserts over. yaml.v3
// ignores every field not named here.
type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Schedule                string `yaml:"schedule"`
		ConcurrencyPolicy       string `yaml:"concurrencyPolicy"`
		StartingDeadlineSeconds *int   `yaml:"startingDeadlineSeconds"`
		JobTemplate             struct {
			Spec struct {
				ActiveDeadlineSeconds *int `yaml:"activeDeadlineSeconds"`
				Template              struct {
					Spec podSpec `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"jobTemplate"`
	} `yaml:"spec"`
}

type podSpec struct {
	Containers      []container    `yaml:"containers"`
	Volumes         []volume       `yaml:"volumes"`
	SecurityContext podSecurityCtx `yaml:"securityContext"`
}

// podSecurityCtx is the identity the pod runs under.
type podSecurityCtx struct {
	RunAsNonRoot *bool `yaml:"runAsNonRoot"`
}

type container struct {
	Name            string               `yaml:"name"`
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

func TestCronJobTicksHourly(t *testing.T) {
	// The grace window is counted in hours, so an hourly tick is what runs a
	// period within an hour of it becoming due. A daily schedule would hold a
	// finished month back by most of a day.
	doc := objectNamed(t, objects(t, manifestFile), "CronJob", cronJob)

	if got, want := doc.Spec.Schedule, "0 * * * *"; got != want {
		t.Errorf("schedule = %q, want %q, which is what runs a due period within the hour it became due", got, want)
	}
	// A run takes an advisory lock on the period it meters and fails rather than
	// waits when another process holds it, so a tick that outlasts its hour would
	// make the next one fail on the month it is still working through.
	if got, want := doc.Spec.ConcurrencyPolicy, "Forbid"; got != want {
		t.Errorf("concurrencyPolicy = %q, want %q, so a tick that is still running is never joined by a second one", got, want)
	}
}

func TestATickThatHangsIsKilledAndTheNextHourStarts(t *testing.T) {
	// Forbid needs an end to the tick it holds the next one behind. A query that
	// stops answering leaves the Job Active rather than Failed, and nothing else
	// would ever start another one: billing stops with no alert, because no Job
	// failed. The deadline on the CronJob is the other half -- unset, the
	// controller counts the missed schedules forever and refuses to start the Job
	// at all past a hundred of them, which deleting the stuck Job does not undo.
	doc := objectNamed(t, objects(t, manifestFile), "CronJob", cronJob)

	deadline := doc.Spec.JobTemplate.Spec.ActiveDeadlineSeconds
	if deadline == nil {
		t.Fatal("the Job sets no activeDeadlineSeconds, so a tick that hangs runs forever and concurrencyPolicy: Forbid suppresses every tick behind it")
	}
	if *deadline <= 0 || *deadline >= 3600 {
		t.Errorf("activeDeadlineSeconds = %d, want a bound inside the hour between two ticks", *deadline)
	}
	if doc.Spec.StartingDeadlineSeconds == nil {
		t.Error("the CronJob sets no startingDeadlineSeconds, so past a hundred missed schedules the controller stops starting the Job at all")
	}
}

func TestTheTickIsConfinedToWhatItNeeds(t *testing.T) {
	// The image ends in distroless nonroot with USER nonroot:nonroot, which names
	// an identity and nothing else: seccomp, the capability set, the writability
	// of the root filesystem and the way back to higher privileges all stay at
	// the permissive Kubernetes defaults. This pod mounts two database connection
	// strings, one of them writing the engine's own database, so what a bug in
	// the binary or in any dependency it dials out with reaches is exactly what
	// these fields decide. Nothing here fails a deployment, which is why it is
	// asserted rather than noticed: the tick runs identically either way.
	doc := objectNamed(t, objects(t, manifestFile), "CronJob", cronJob)
	c := containerNamed(t, doc, cronJob)

	if runAsNonRoot := pod(doc).SecurityContext.RunAsNonRoot; runAsNonRoot == nil || !*runAsNonRoot {
		t.Error("the pod does not set runAsNonRoot: true, so nothing but the image's own USER keeps the process off uid 0")
	}

	sc := c.SecurityContext
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("the container does not set allowPrivilegeEscalation: false, so a setuid binary reachable in the mount namespace can still raise its privileges")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("the container does not set readOnlyRootFilesystem: true, although the tick writes to two databases and to nothing on disk")
	}
	if !slices.Contains(sc.Capabilities.Drop, "ALL") {
		t.Errorf("capabilities.drop = %v, want it to name ALL; a process that binds no port and reads two mounted files needs none of the default set", sc.Capabilities.Drop)
	}
	if want := "RuntimeDefault"; sc.SeccompProfile.Type != want {
		t.Errorf("seccompProfile.type = %q, want %q; without it the container runs with seccomp unconfined", sc.SeccompProfile.Type, want)
	}
}

func TestTheTickDeclaresWhatItCosts(t *testing.T) {
	// A tick holds a whole billing month in memory at once, and a walk that
	// catches several up holds them one after another. Without requests the pod
	// is BestEffort: the highest oom_score_adj on the node, first in line for
	// eviction, and placed by a scheduler that had nothing to bin-pack it
	// against. An eviction mid-run leaves the month unbilled and the run row
	// behind, once an hour, with no signal past a pod that the history limit
	// rotates away.
	c := containerNamed(t, objectNamed(t, objects(t, manifestFile), "CronJob", cronJob), cronJob)

	if c.Resources.Requests.CPU == "" || c.Resources.Requests.Memory == "" {
		t.Errorf("resources.requests = %+v, want both cpu and memory; without them the pod is BestEffort and the scheduler places it blind",
			c.Resources.Requests)
	}
	if c.Resources.Limits.Memory == "" {
		t.Error("no resources.limits.memory, so a month larger than the node expects takes the node down rather than the tick")
	}
}

func TestCronJobRunsTheTickSubcommand(t *testing.T) {
	// The image's entrypoint is the engine binary, whose root command prints its
	// help and exits zero. An empty argument list is therefore an hourly Job that
	// succeeds without ever advancing a period.
	c := containerNamed(t, objectNamed(t, objects(t, manifestFile), "CronJob", cronJob), cronJob)

	if want := []string{"tick"}; !slices.Equal(c.Args, want) {
		t.Errorf("args = %v, want %v; anything else runs a subcommand the scheduler is not, and an empty list runs the help text on a Job that then reports success",
			c.Args, want)
	}
}

func TestDatabaseSecretsReachTheVariablesThatNameThem(t *testing.T) {
	// Three things have to agree: the variable names a path, the mount puts the
	// secret's directory at that path, and the volume carries the key under the
	// file name the path ends in. A mismatch in any of them leaves the tick
	// failing on an unreadable file once an hour.
	doc := objectNamed(t, objects(t, manifestFile), "CronJob", cronJob)
	c := containerNamed(t, doc, cronJob)

	var name string
	for _, v := range pod(doc).Volumes {
		if v.Secret.SecretName == dbSecret {
			name = v.Name
			break
		}
	}
	if name == "" {
		t.Fatalf("the pod declares no volume backed by secret %s, so neither connection string reaches the container", dbSecret)
	}
	mounted := slices.ContainsFunc(c.VolumeMounts, func(m volumeMount) bool {
		return m.Name == name && m.MountPath == secretDir
	})
	if !mounted {
		t.Fatalf("the container carries %v rather than a mount of volume %q at %q, so the *_FILE paths below point at nothing",
			c.VolumeMounts, name, secretDir)
	}

	items := volumeNamed(t, doc, name).Secret.Items
	for variable, key := range dbSecretVars {
		value, set := envValue(c, variable)
		if !set {
			t.Errorf("no %s, so the engine looks for the connection string in the environment, where this deployment does not put it", variable)
			continue
		}
		if want := secretPath(key); value != want {
			t.Errorf("%s = %q, want %q, which is where the %s volume mounts key %s", variable, value, want, dbSecret, key)
		}
		carried := slices.ContainsFunc(items, func(i secretItem) bool { return i.Key == key && i.Path == key })
		if !carried {
			t.Errorf("the %s volume carries %v rather than key %s at path %s, so %s points at a file the mount does not create",
				dbSecret, items, key, key, variable)
		}
	}
}

func TestCounterSourcesIsSetToTheEmptyPath(t *testing.T) {
	// Present and empty is the assertion. The engine reads the empty value as the
	// zero configuration, while an absent variable keeps the default path, and
	// reading a file nothing mounted there fails the tick before it opens a
	// database. An installation that measures counters mounts its sources file
	// and sets this variable to it.
	const variable = "TALLY_ENGINE_COUNTER_SOURCES"

	c := containerNamed(t, objectNamed(t, objects(t, manifestFile), "CronJob", cronJob), cronJob)

	value, set := envValue(c, variable)
	if !set {
		t.Fatalf("no %s, so the engine keeps its default sources path and every tick fails on a file this deployment does not mount", variable)
	}
	if value != "" {
		t.Errorf("%s = %q, want the empty value; a path is only correct where a sources file is mounted at it", variable, value)
	}
}

func TestKustomizationListsTheManifest(t *testing.T) {
	// Without the entry the CronJob stays a file in the repository that no
	// rendered overlay carries, and the cluster runs no scheduler at all.
	var k struct {
		Resources []string `yaml:"resources"`
	}

	raw, err := os.ReadFile(kustomizationFile)
	if err != nil {
		t.Fatalf("reading %s: %v", kustomizationFile, err)
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parsing %s, which kustomize refuses to build: %v", kustomizationFile, err)
	}
	if !slices.Contains(k.Resources, manifestFile) {
		t.Errorf("%s lists %v rather than %s, so nothing deploys the scheduler", kustomizationFile, k.Resources, manifestFile)
	}
}

// objects decodes every document of one manifest file.
func objects(t *testing.T, path string) []object {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the manifest: %v", err)
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
			t.Fatalf("parsing %s: %v", path, err)
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
	t.Fatalf("no %s named %q", kind, name)
	return object{}
}

// pod returns the pod of the CronJob, which sits two templates down: the job
// template carries the pod template the containers and volumes are in.
func pod(doc object) podSpec { return doc.Spec.JobTemplate.Spec.Template.Spec }

// containerNamed returns one container of the CronJob's pod.
func containerNamed(t *testing.T, doc object, name string) container {
	t.Helper()

	for _, c := range pod(doc).Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s %s carries no container %q", doc.Kind, doc.Metadata.Name, name)
	return container{}
}

// volumeNamed returns one volume of the CronJob's pod.
func volumeNamed(t *testing.T, doc object, name string) volume {
	t.Helper()

	for _, v := range pod(doc).Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("%s %s carries no volume %q", doc.Kind, doc.Metadata.Name, name)
	return volume{}
}

// envValue reports what one variable of a container is set to, and whether it is
// set at all. The two differ for this manifest: an empty value is a setting, an
// absent variable leaves the engine's own default in place.
func envValue(c container, name string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

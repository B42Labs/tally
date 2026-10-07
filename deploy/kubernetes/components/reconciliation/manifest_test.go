// This file pins the sync CronJob's contract with the Reporting API, with the
// collector's settings and with the Secret it authenticates with, and the
// patch's contract with the files the Reporting API reads. It fails quietly: a
// schedule past 30 minutes keeps TallySyncStale firing on a cloud that is
// reconciled, a call that outlasts its interval holds the next one behind
// Forbid, a URL naming a route or a Service that is not there fails every run
// in a Job history nobody reads, and a cloud named apart from the collector's
// books a second set of resources. None of it stops kustomize or an apply. The
// tests read the YAML from disk and need neither a cluster nor kustomize.
package reconciliation_test

import (
	"bytes"
	"errors"
	"io"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/b42labs/tally/internal/reporting/config"
)

const (
	syncFile          = "sync.yaml"
	patchFile         = "reporting-api.yaml"
	kustomizationFile = "kustomization.yaml"

	// The CronJob and its one container.
	cronJob       = "tally-sync"
	syncContainer = "sync"

	// The Deployment the patch mounts the clouds files into, and its container.
	reportingAPI = "reporting-api"

	// What the Reporting API reconciles and authenticates with, and the key of
	// the Secret.
	cloudsConfigMap = "tally-reconciliation"
	cloudsSecret    = "tally-reconciliation-auth"
	cloudsKey       = "clouds.yaml"

	// The Secret the sync authenticates with, and the ConfigMap and the key
	// the collector reads its cloud from.
	tokenSecret       = "tally-internal-token"
	settingsConfigMap = "tally-openstack-collector"
	cloudVariable     = "TALLY_OSC_CLOUD"

	// The files of the base and of the collector component that declare the
	// Reporting API, and the API description that declares its routes.
	reportingAPIFile = "../../base/reporting-api/reporting-api.yaml"
	collectorFile    = "../openstack-collector/openstack-collector.yaml"
	openAPIFile      = "../../../../api/reporting/openapi.yaml"

	// The route the sync calls, as the API description writes it.
	syncRoute = "/internal/sync/{cloud}"

	// The user the pod runs as, distroless nonroot.
	nonroot = 65532
)

var (
	// urlRe is the URL the sync posts to.
	urlRe = regexp.MustCompile(`http://[^"\s]+`)
	// tokenRe is the file the Authorization header is read from.
	tokenRe = regexp.MustCompile(`Authorization: Bearer \$\(cat ([^)]+)\)`)
)

// object is the part of a manifest document these tests assert over. yaml.v3
// ignores every field not named here, so one shape covers the CronJob, the
// patch and the objects of the base and the collector component.
type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// CronJob
		Schedule                string `yaml:"schedule"`
		ConcurrencyPolicy       string `yaml:"concurrencyPolicy"`
		StartingDeadlineSeconds *int   `yaml:"startingDeadlineSeconds"`
		JobTemplate             struct {
			Spec struct {
				ActiveDeadlineSeconds *int `yaml:"activeDeadlineSeconds"`
				BackoffLimit          *int `yaml:"backoffLimit"`
				Template              struct {
					Spec podSpec `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		} `yaml:"jobTemplate"`
		// Deployment
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
	RestartPolicy                string         `yaml:"restartPolicy"`
	AutomountServiceAccountToken *bool          `yaml:"automountServiceAccountToken"`
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
	Command         []string             `yaml:"command"`
	EnvFrom         []envFromSource      `yaml:"envFrom"`
	Env             []envVar             `yaml:"env"`
	VolumeMounts    []volumeMount        `yaml:"volumeMounts"`
	SecurityContext containerSecurityCtx `yaml:"securityContext"`
	Resources       resources            `yaml:"resources"`
}

// envFromSource is a ConfigMap a container takes every key of as a variable.
type envFromSource struct {
	ConfigMapRef struct {
		Name string `yaml:"name"`
	} `yaml:"configMapRef"`
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
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	ValueFrom struct {
		ConfigMapKeyRef struct {
			Name string `yaml:"name"`
			Key  string `yaml:"key"`
		} `yaml:"configMapKeyRef"`
	} `yaml:"valueFrom"`
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
	ConfigMap struct {
		Name string `yaml:"name"`
	} `yaml:"configMap"`
}

type secretItem struct {
	Key  string `yaml:"key"`
	Path string `yaml:"path"`
}

func TestKustomizationIsAComponentListingTheSyncAndThePatch(t *testing.T) {
	// An overlay lists this directory under components, and kustomize refuses a
	// Kustomization there. Without the resource no sync runs, and without the
	// patch every sync is answered 404: the Reporting API reconciles no cloud.
	var k struct {
		Kind      string   `yaml:"kind"`
		Resources []string `yaml:"resources"`
		Patches   []struct {
			Path string `yaml:"path"`
		} `yaml:"patches"`
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
	if want := []string{syncFile}; !slices.Equal(k.Resources, want) {
		t.Errorf("%s lists the resources %v, want exactly %v", kustomizationFile, k.Resources, want)
	}
	if len(k.Patches) != 1 || k.Patches[0].Path != patchFile {
		t.Errorf("%s lists the patches %+v, want exactly the one with path %s", kustomizationFile, k.Patches, patchFile)
	}
}

func TestTheSyncRunsEveryTenMinutesAndEndsBeforeTheNext(t *testing.T) {
	// The roadmap and the runbook of TallySyncStale assume a run every ten
	// minutes, and the alert fires when 30 pass without a completed one. A run
	// takes the cloud for its length and the next call is answered 409, so
	// Forbid holds the next run behind one that is still calling, and the
	// deadline is what ends a call that hangs before the next schedule. Without
	// startingDeadlineSeconds the controller refuses to start the Job at all
	// past a hundred missed schedules.
	const interval = 600

	doc := objectNamed(t, objectsIn(t, syncFile), "CronJob", cronJob)
	job := doc.Spec.JobTemplate.Spec

	if got, want := doc.Spec.Schedule, "*/10 * * * *"; got != want {
		t.Errorf("schedule = %q, want %q", got, want)
	}
	if got, want := doc.Spec.ConcurrencyPolicy, "Forbid"; got != want {
		t.Errorf("concurrencyPolicy = %q, want %q; a second call while a run holds the cloud is answered 409", got, want)
	}
	if got := doc.Spec.StartingDeadlineSeconds; got == nil || *got != 300 {
		t.Errorf("startingDeadlineSeconds = %s, want 300", intOrUnset(got))
	}
	if got := job.ActiveDeadlineSeconds; got == nil || *got != 540 || *got >= interval {
		t.Errorf("activeDeadlineSeconds = %s, want 540, under the interval of %d seconds, so Forbid never holds a run behind a call that hangs",
			intOrUnset(got), interval)
	}
	if got := job.BackoffLimit; got == nil || *got != 0 {
		t.Errorf("backoffLimit = %s, want 0: the next run is the retry", intOrUnset(got))
	}

	// The call is given the Reporting API's default budget plus the 20 seconds
	// a caller adds, and a client that gives up earlier cancels the run. A
	// deadline it does not end under kills the call instead.
	field, _ := reflect.TypeFor[config.Config]().FieldByName("SyncBudgetSeconds")
	budget, err := strconv.Atoi(field.Tag.Get("envDefault"))
	if err != nil {
		t.Fatalf("reading the default of TALLY_REPORTING_SYNC_BUDGET_S: %v", err)
	}
	c := syncContainerOf(t)
	i := slices.IndexFunc(c.Env, func(e envVar) bool { return e.Name == "SYNC_TIMEOUT_S" })
	if i < 0 {
		t.Fatal("the sync sets no SYNC_TIMEOUT_S, which its call takes its timeout from")
	}
	timeout, err := strconv.Atoi(c.Env[i].Value)
	if err != nil || timeout != budget+20 {
		t.Errorf("SYNC_TIMEOUT_S = %q, want %d, the default budget of %d plus 20 seconds", c.Env[i].Value, budget+20, budget)
	}
	if deadline := job.ActiveDeadlineSeconds; deadline != nil && timeout >= *deadline {
		t.Errorf("SYNC_TIMEOUT_S = %d, want it under activeDeadlineSeconds = %d, which kills the call otherwise", timeout, *deadline)
	}
}

func TestTheSyncCallsTheInternalRouteOfTheReportingAPI(t *testing.T) {
	// The URL names a Service of the base and a route of the API description,
	// and the token is read from the file the Secret is mounted as. A Service
	// renamed or moved to another port, a route renamed, or a token path that
	// stopped matching the mount fails every run, and TallySyncStale is the
	// first sign.
	c := syncContainerOf(t)
	command := strings.Join(c.Command, " ")

	for _, want := range []string{"http://reporting-api/internal/sync/$" + cloudVariable, "--post-data", `-T "$SYNC_TIMEOUT_S"`} {
		if !strings.Contains(command, want) {
			t.Errorf("the sync runs %q, which does not carry %s", command, want)
		}
	}

	raw := urlRe.FindString(command)
	u, err := url.Parse(raw)
	if raw == "" || err != nil {
		t.Fatalf("the sync runs %q, which carries no URL the Reporting API answers (%v)", command, err)
	}
	port := u.Port()
	if port == "" {
		// A URL without a port is one on the port of its scheme.
		port = "80"
	}
	served := slices.ContainsFunc(objectsIn(t, reportingAPIFile), func(o object) bool {
		return o.Kind == "Service" && o.Metadata.Name == u.Hostname() &&
			slices.ContainsFunc(o.Spec.Ports, func(p servicePort) bool { return strconv.Itoa(p.Port) == port })
	})
	if !served {
		t.Errorf("the sync posts to %s, but %s declares no Service %q on port %s", raw, reportingAPIFile, u.Hostname(), port)
	}

	route := strings.Replace(u.Path, "$"+cloudVariable, "{cloud}", 1)
	if route != syncRoute {
		t.Errorf("the sync posts to the path %q, which is %q as the API description writes it, want %s", u.Path, route, syncRoute)
	}
	if !slices.Contains(openAPIPaths(t), route) {
		t.Errorf("%s declares no path %s, which the sync posts to", openAPIFile, route)
	}

	m := tokenRe.FindStringSubmatch(command)
	if m == nil {
		t.Fatalf("the sync runs %q, whose Authorization header reads the token from no file", command)
	}
	if want := secretPath(t, cronJobPod(t), c, tokenSecret, "token"); m[1] != want {
		t.Errorf("the Authorization header reads the token from %s, want %s, which is where secret %s is mounted", m[1], want, tokenSecret)
	}
}

func TestTheSyncTakesItsCloudFromTheCollectorSettings(t *testing.T) {
	// The events carry the cloud of the collector's settings, and a sync under
	// another name would book a second set of resources beside them. One line
	// of collector.env names both when the sync reads the ConfigMap the
	// collector reads.
	c := syncContainerOf(t)

	i := slices.IndexFunc(c.Env, func(e envVar) bool { return e.Name == cloudVariable })
	if i < 0 {
		t.Fatalf("the sync sets no %s, so it posts to a route without a cloud", cloudVariable)
	}
	e := c.Env[i]
	if e.Value != "" {
		t.Errorf("%s = %q, a cloud of its own beside the one the collector reports under", cloudVariable, e.Value)
	}
	if ref := e.ValueFrom.ConfigMapKeyRef; ref.Name != settingsConfigMap || ref.Key != cloudVariable {
		t.Errorf("%s comes from ConfigMap %q key %q, want ConfigMap %s key %s, the cloud the collector reports under",
			cloudVariable, ref.Name, ref.Key, settingsConfigMap, cloudVariable)
	}

	// The collector reads the same ConfigMap, so the two name one cloud.
	collector := objectNamed(t, objectsIn(t, collectorFile), "Deployment", "openstack-collector")
	reads := slices.ContainsFunc(collector.Spec.Template.Spec.Containers, func(c container) bool {
		return slices.ContainsFunc(c.EnvFrom, func(f envFromSource) bool { return f.ConfigMapRef.Name == settingsConfigMap })
	})
	if !reads {
		t.Errorf("%s takes its settings from no ConfigMap %s, so the sync and the collector may name two clouds", collectorFile, settingsConfigMap)
	}
}

func TestThePatchPointsTheReportingAPIAtTheFilesItMounts(t *testing.T) {
	// The Reporting API reads the clouds config from the path its variable
	// names, and exits at startup when it cannot. The adapter authenticates
	// with the clouds.yaml its variable names, and without it every sync is
	// answered 500. A container name the base no longer carries adds a second
	// container without an image, which the apply refuses.
	patch := objectNamed(t, objectsIn(t, patchFile), "Deployment", reportingAPI)
	pod := patch.Spec.Template.Spec
	c := containerNamed(t, pod, reportingAPI)

	value, set := envValue(c, "TALLY_REPORTING_CLOUDS_CONFIG")
	if !set {
		t.Fatal("the patch sets no TALLY_REPORTING_CLOUDS_CONFIG, so the Reporting API reconciles no cloud")
	}
	i := slices.IndexFunc(pod.Volumes, func(v volume) bool { return v.ConfigMap.Name == cloudsConfigMap })
	if i < 0 {
		t.Fatalf("the patch declares no volume backed by ConfigMap %s", cloudsConfigMap)
	}
	mount := mountOf(t, c, pod.Volumes[i].Name)
	if got := path.Dir(value); got != mount.MountPath {
		t.Errorf("TALLY_REPORTING_CLOUDS_CONFIG = %q, whose directory is %q, want a file in %q, where ConfigMap %s is mounted",
			value, got, mount.MountPath, cloudsConfigMap)
	}
	if got, want := path.Base(value), "clouds-config.yaml"; got != want {
		t.Errorf("TALLY_REPORTING_CLOUDS_CONFIG names the file %q, want %q, the key the overlay generates the ConfigMap with", got, want)
	}

	value, set = envValue(c, "OS_CLIENT_CONFIG_FILE")
	if !set {
		t.Fatal("the patch sets no OS_CLIENT_CONFIG_FILE, so the adapter looks for clouds.yaml where nothing mounts it")
	}
	if want := secretPath(t, pod, c, cloudsSecret, cloudsKey); value != want {
		t.Errorf("OS_CLIENT_CONFIG_FILE = %q, want %q, which is where secret %s mounts key %s", value, want, cloudsSecret, cloudsKey)
	}

	base := objectNamed(t, objectsIn(t, reportingAPIFile), "Deployment", reportingAPI)
	containerNamed(t, base.Spec.Template.Spec, reportingAPI)
}

func TestTheSyncIsConfinedToWhatItNeeds(t *testing.T) {
	// busybox runs as root unless told otherwise. This pod mounts the token
	// every internal route of the Reporting API accepts, so what the container
	// reaches is what these fields decide. Without a request the pod is
	// BestEffort and first in line for eviction.
	pod := cronJobPod(t)
	c := syncContainerOf(t)

	if got := pod.RestartPolicy; got != "Never" {
		t.Errorf("restartPolicy = %q, want Never, so a failed call is one pod with its log", got)
	}
	if automount := pod.AutomountServiceAccountToken; automount == nil || *automount {
		t.Error("the pod does not set automountServiceAccountToken: false, although the sync calls no Kubernetes API")
	}
	if runAsNonRoot := pod.SecurityContext.RunAsNonRoot; runAsNonRoot == nil || !*runAsNonRoot {
		t.Error("the pod does not set runAsNonRoot: true, and busybox runs as root by default")
	}
	for field, got := range map[string]*int{
		"runAsUser":  pod.SecurityContext.RunAsUser,
		"runAsGroup": pod.SecurityContext.RunAsGroup,
	} {
		if got == nil || *got != nonroot {
			t.Errorf("%s = %s, want %d", field, intOrUnset(got), nonroot)
		}
	}

	sc := c.SecurityContext
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("the container does not set allowPrivilegeEscalation: false, so a setuid binary of the image can still raise its privileges")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("the container does not set readOnlyRootFilesystem: true, although wget writes the response to stdout and nothing to disk")
	}
	if want := []string{"ALL"}; !slices.Equal(sc.Capabilities.Drop, want) {
		t.Errorf("capabilities.drop = %v, want %v; a process that dials one Service needs none of the default set", sc.Capabilities.Drop, want)
	}
	if want := "RuntimeDefault"; sc.SeccompProfile.Type != want {
		t.Errorf("seccompProfile.type = %q, want %q; without it the container runs with seccomp unconfined", sc.SeccompProfile.Type, want)
	}
	if c.Resources.Requests.CPU == "" || c.Resources.Requests.Memory == "" {
		t.Errorf("resources.requests = %+v, want cpu and memory, without which the pod is BestEffort", c.Resources.Requests)
	}
	if c.Resources.Limits.Memory == "" {
		t.Error("no resources.limits.memory, so a container that grows takes the node down rather than itself")
	}
}

// cronJobPod returns the pod template of the CronJob.
func cronJobPod(t *testing.T) podSpec {
	t.Helper()
	return objectNamed(t, objectsIn(t, syncFile), "CronJob", cronJob).Spec.JobTemplate.Spec.Template.Spec
}

// syncContainerOf returns the one container of the CronJob's pod.
func syncContainerOf(t *testing.T) container {
	t.Helper()
	return containerNamed(t, cronJobPod(t), syncContainer)
}

// secretPath returns the path one key of a Secret has in a container: the
// mount path of the volume backed by the Secret, joined with the item path of
// the key. It fails when the volume, the item or a read-only mount is missing.
func secretPath(t *testing.T, pod podSpec, c container, secret, key string) string {
	t.Helper()

	i := slices.IndexFunc(pod.Volumes, func(v volume) bool { return v.Secret.SecretName == secret })
	if i < 0 {
		t.Fatalf("the pod declares no volume backed by secret %s", secret)
	}
	v := pod.Volumes[i]
	if len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != key {
		t.Fatalf("the %s volume carries the items %v, want the one key %s", secret, v.Secret.Items, key)
	}
	return path.Join(mountOf(t, c, v.Name).MountPath, v.Secret.Items[0].Path)
}

// mountOf returns the mount of one volume in a container, failing when the
// container does not mount it or mounts it writable.
func mountOf(t *testing.T, c container, volumeName string) volumeMount {
	t.Helper()

	i := slices.IndexFunc(c.VolumeMounts, func(m volumeMount) bool { return m.Name == volumeName })
	if i < 0 {
		t.Fatalf("container %s mounts %v and not volume %q", c.Name, c.VolumeMounts, volumeName)
	}
	if !c.VolumeMounts[i].ReadOnly {
		t.Errorf("container %s mounts volume %q at %s, and not read-only", c.Name, volumeName, c.VolumeMounts[i].MountPath)
	}
	return c.VolumeMounts[i]
}

// openAPIPaths returns the paths the API description declares.
func openAPIPaths(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(openAPIFile)
	if err != nil {
		t.Fatalf("reading %s: %v", openAPIFile, err)
	}
	var spec struct {
		Paths map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parsing %s: %v", openAPIFile, err)
	}
	paths := make([]string, 0, len(spec.Paths))
	for p := range spec.Paths {
		paths = append(paths, p)
	}
	return paths
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
	t.Fatalf("no %s named %q", kind, name)
	return object{}
}

// containerNamed returns one container of a pod.
func containerNamed(t *testing.T, pod podSpec, name string) container {
	t.Helper()

	for _, c := range pod.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("the pod carries no container %q", name)
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

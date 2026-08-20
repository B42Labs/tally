// This file pins the manifest properties that decide who may change
// Alertmanager's state and where a firing alert ends up. Each of them fails
// quietly: a route that forwards POST /api/v2/silences renders exactly like one
// that refuses it, a receiver that grew a delivery integration in this
// repository would notify a stranger from every deployment that takes the base,
// and a critical alert that repeats no sooner than the rest is only noticeable
// once an incident is four hours old. The test reads the YAML from disk and
// needs no cluster.
package alertmanager_test

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	manifestFile = "alertmanager.yaml"
	configFile   = "config.yaml"
)

// object is the part of a manifest document this test asserts over. yaml.v3
// ignores every field not named here, so one shape covers all three kinds.
type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// HTTPRouteFilter
		DirectResponse struct {
			StatusCode int `yaml:"statusCode"`
		} `yaml:"directResponse"`
		// StatefulSet
		Template struct {
			Spec struct {
				// Pointers, so a field the manifest leaves out is told apart
				// from one set to 0, which is root.
				SecurityContext struct {
					RunAsUser  *int64 `yaml:"runAsUser"`
					RunAsGroup *int64 `yaml:"runAsGroup"`
					FSGroup    *int64 `yaml:"fsGroup"`
				} `yaml:"securityContext"`
				Containers []container `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
		VolumeClaimTemplates []struct{} `yaml:"volumeClaimTemplates"`
		// HTTPRoute
		Rules []routeRule `yaml:"rules"`
	} `yaml:"spec"`
}

type container struct {
	Name            string   `yaml:"name"`
	Args            []string `yaml:"args"`
	SecurityContext struct {
		// Pointers for the same reason the pod's are: a field the manifest
		// leaves out defaults to the permissive value, and one written as
		// false has to be told apart from that.
		AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
		ReadOnlyRootFilesystem   *bool `yaml:"readOnlyRootFilesystem"`
		Capabilities             struct {
			Drop []string `yaml:"drop"`
		} `yaml:"capabilities"`
		SeccompProfile struct {
			Type string `yaml:"type"`
		} `yaml:"seccompProfile"`
	} `yaml:"securityContext"`
}

type routeRule struct {
	Matches []routeMatch `yaml:"matches"`
	Filters []struct {
		Type         string `yaml:"type"`
		ExtensionRef struct {
			Kind string `yaml:"kind"`
			Name string `yaml:"name"`
		} `yaml:"extensionRef"`
	} `yaml:"filters"`
	BackendRefs []struct {
		Name string `yaml:"name"`
	} `yaml:"backendRefs"`
}

// routeMatch carries the match type and the method as well as the path. All
// three decide what a rule intercepts: /api/v2 as Exact matches no silence path
// at all, and the same prefix is published for GET and refused for POST.
type routeMatch struct {
	Path struct {
		Type  string `yaml:"type"`
		Value string `yaml:"value"`
	} `yaml:"path"`
	Method string `yaml:"method"`
}

// publishedRead is one match the forwarding rule may carry, and publishedReads
// is every one of them. The two subtests over it read it from both sides: one
// fails on an entry that was dropped, the other on a match this list does not
// name. Both properties are needed, because what withholds GET /api/v2/status
// and GET /metrics is not a rule that names them but the absence of any match
// wide enough to reach them: consolidating the four /api/v2 reads into one
// PathPrefix /api/v2 names neither and publishes both.
type publishedRead struct {
	matchType string
	path      string
	carries   string
}

var publishedReads = []publishedRead{
	{"PathPrefix", "/api/v2/alerts", "the alert list and its groups"},
	{"PathPrefix", "/api/v2/silences", "the silence list"},
	{"PathPrefix", "/api/v2/silence", "one silence by id"},
	{"PathPrefix", "/api/v2/receivers", "the filter bar's receiver list"},
	{"PathPrefix", "/assets", "the UI's hashed bundle"},
	{"Exact", "/favicon.ico", "the icon"},
	{"Exact", "/", "the index the UI is served from"},
}

// alertConfig is the routing tree and the receiver list of config.yaml.
type alertConfig struct {
	// A pointer, so a file that declares no route at all is told apart from
	// one whose route is empty.
	Route     *routeNode       `yaml:"route"`
	Receivers []map[string]any `yaml:"receivers"`
}

type routeNode struct {
	Receiver       string      `yaml:"receiver"`
	RepeatInterval string      `yaml:"repeat_interval"`
	Matchers       []string    `yaml:"matchers"`
	Routes         []routeNode `yaml:"routes"`
}

func TestAlertmanagerRoute(t *testing.T) {
	docs := objects(t, manifestFile)
	route := objectNamed(t, docs, "HTTPRoute", "alertmanager")

	var deny, forward []routeRule
	for _, rule := range route.Spec.Rules {
		if len(rule.BackendRefs) == 0 {
			deny = append(deny, rule)
			continue
		}
		forward = append(forward, rule)
	}
	if len(deny) != 1 {
		t.Fatalf("the alertmanager HTTPRoute has %d rules without a backend, want the one that refuses the write API; without it every write path is answered by a 404 that says nothing",
			len(deny))
	}
	if len(forward) != 1 {
		t.Fatalf("the alertmanager HTTPRoute has %d rules with a backend, want the one that carries the UI", len(forward))
	}
	rule := deny[0]

	t.Run("answers the refused paths itself", func(t *testing.T) {
		// The Gateway API has no core way to answer without a backend, so the
		// refusal is Envoy Gateway's extension filter. A rule that names
		// neither a backend nor a filter answers 500, which reads as an outage
		// rather than as a decision.
		if len(rule.Filters) != 1 || rule.Filters[0].Type != "ExtensionRef" {
			t.Fatalf("the deny rule carries %d filters, want exactly one ExtensionRef; a rule with neither backend nor filter answers 500 rather than a refusal",
				len(rule.Filters))
		}

		ref := rule.Filters[0].ExtensionRef
		if ref.Kind != "HTTPRouteFilter" {
			t.Fatalf("the deny rule references kind %q, want HTTPRouteFilter", ref.Kind)
		}
		for _, doc := range docs {
			if doc.Kind != ref.Kind || doc.Metadata.Name != ref.Name {
				continue
			}
			if doc.Spec.DirectResponse.StatusCode != 403 {
				t.Errorf("%s %s responds %d, want 403", ref.Kind, ref.Name, doc.Spec.DirectResponse.StatusCode)
			}
			return
		}
		t.Errorf("the rule references %s/%s, which this file does not define, so the Gateway rejects the route and the host answers nothing",
			ref.Kind, ref.Name)
	})

	t.Run("names every write path", func(t *testing.T) {
		// Port 9093 answers reads and writes on one API, and the UI needs the
		// reads. What separates them is the method, so a missing entry here
		// publishes a write to whoever opens the host.
		for _, want := range []struct {
			prefix string
			method string
			opens  string
		}{
			{"/api/v2", "POST", "creating a silence and injecting an alert"},
			{"/api/v2", "PUT", "editing a silence"},
			{"/api/v2", "DELETE", "expiring a silence"},
			{"/-/reload", "", "re-reading the config on demand"},
			{"/debug", "", "the pprof endpoints"},
		} {
			if covers(rule, "PathPrefix", want.prefix, want.method) {
				continue
			}
			target := want.prefix
			if want.method != "" {
				target = want.method + " " + want.prefix
			}
			t.Errorf("the deny rule has no PathPrefix match for %s, so %s is answered by the Gateway's 404 rather than by a refusal that says why",
				target, want.opens)
		}
	})

	t.Run("publishes the reads the UI needs", func(t *testing.T) {
		// One side of the allowlist: a path dropped from publishedReads takes a
		// part of the UI with it.
		for _, want := range publishedReads {
			if !covers(forward[0], want.matchType, want.path, "GET") {
				t.Errorf("the forwarding rule has no GET match of type %s for %s, so %s is not published",
					want.matchType, want.path, want.carries)
			}
		}
		if got := forward[0].BackendRefs[0].Name; got != "alertmanager" {
			t.Errorf("the forwarding rule sends the host to %q, want alertmanager", got)
		}
	})

	t.Run("withholds what answers with the config", func(t *testing.T) {
		// The other side, and the reason the read side names paths rather than
		// the host. GET /api/v2/status and GET /metrics are reads, so the deny
		// rule above never sees them: what keeps them off the published host is
		// that no match here carries them. Asserting the absence of those two
		// values would not say that, because any match wider than the ones
		// above carries both without naming either.
		for _, m := range forward[0].Matches {
			if !slices.ContainsFunc(publishedReads, func(r publishedRead) bool {
				return r.matchType == m.Path.Type && r.path == m.Path.Value
			}) {
				t.Errorf("the forwarding rule publishes %s %s, which is not one of the reads the UI needs; a match wider than those carries GET /api/v2/status and the loaded config with it, and GET /metrics the delivery integrations it names",
					m.Path.Type, m.Path.Value)
			}
			// A match that names no method applies to every one of them, and
			// the Gateway API ranks its longer prefix above the deny rule's
			// PathPrefix /api/v2 plus method: the write API becomes reachable
			// from the published host without any match here naming it.
			if m.Method != "GET" {
				t.Errorf("the match for %s %s names method %q rather than GET, so it outranks the deny rule's /api/v2 method match and forwards POST %s to Alertmanager",
					m.Path.Type, m.Path.Value, m.Method, m.Path.Value)
			}
		}
	})
}

func TestAlertmanagerStatefulSet(t *testing.T) {
	t.Run("can write to the volume it claims", func(t *testing.T) {
		// prom/alertmanager runs as nobody, UID and GID 65534. Kubernetes
		// chowns a mounted volume to the pod's fsGroup or not at all, and most
		// CSI drivers hand a fresh claim back owned root:root 0755, so without
		// one Alertmanager cannot create the notification log or a silence
		// under --storage.path and the pod crash-loops. kind's local-path
		// provisioner mkdirs 0777 and an emptyDir is created 0777 by the
		// kubelet, so neither the dev overlay nor the shape this StatefulSet
		// replaced would have shown it.
		const nobody = int64(65534)

		sts := objectNamed(t, objects(t, manifestFile), "StatefulSet", "alertmanager")
		if len(sts.Spec.VolumeClaimTemplates) == 0 {
			t.Fatal("the StatefulSet claims no volume, so the notification log and the silence store are back on storage a pod roll drops")
		}

		sc := sts.Spec.Template.Spec.SecurityContext
		for _, f := range []struct {
			name string
			got  *int64
		}{
			{"fsGroup", sc.FSGroup},
			{"runAsUser", sc.RunAsUser},
			{"runAsGroup", sc.RunAsGroup},
		} {
			if f.got == nil {
				t.Errorf("the pod names no %s, so the claim stays owned by root and the container's %d cannot write to it", f.name, nobody)
				continue
			}
			if *f.got != nobody {
				t.Errorf("the pod names %s %d, want %d, which is the user prom/alertmanager runs as; a mismatch leaves the claim owned by someone else",
					f.name, *f.got, nobody)
			}
		}
	})

	t.Run("keeps the container controls off their defaults", func(t *testing.T) {
		// The pod-level block above names an identity and stops there. These
		// four are per-container and every one of them defaults the permissive
		// way, so a pod that names only the identity still lets a setuid binary
		// in the image raise this container's privileges, still carries the
		// capability set the runtime grants by default, still has the whole
		// image to write to, and still runs unconfined by seccomp. Alertmanager
		// needs none of it: it binds 9093, --config.file is a read-only
		// ConfigMap mount, and --storage.path is the claim, so the notification
		// log and the silence store are the only things it writes and both land
		// on a mounted volume. Nothing here fails a deployment, which is why it
		// is asserted rather than noticed: the pod runs identically either way.
		c := containerNamed(t, objects(t, manifestFile), "StatefulSet", "alertmanager", "alertmanager")
		sc := c.SecurityContext

		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Error("the container does not set allowPrivilegeEscalation: false, so a setuid binary in the image can still raise its privileges")
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Error("the container does not set readOnlyRootFilesystem: true, so the whole image is writable although every write goes to the claim under --storage.path")
		}
		if !slices.Contains(sc.Capabilities.Drop, "ALL") {
			t.Errorf("capabilities.drop = %v, want it to name ALL; a process binding 9093 needs none of the default set", sc.Capabilities.Drop)
		}
		if want := "RuntimeDefault"; sc.SeccompProfile.Type != want {
			t.Errorf("seccompProfile.type = %q, want %q; without it the container runs with seccomp unconfined", sc.SeccompProfile.Type, want)
		}
	})

	t.Run("turns the gossip cluster off", func(t *testing.T) {
		// One replica has no peer to settle with. Left at its default,
		// Alertmanager opens the cluster port and logs "gossip not settled" at
		// every start while it waits for members that never arrive, which is
		// noise no reader of these logs can act on.
		const arg = "--cluster.listen-address="

		c := containerNamed(t, objects(t, manifestFile), "StatefulSet", "alertmanager", "alertmanager")

		if !slices.Contains(c.Args, arg) {
			t.Errorf("args = %v, want one of them %q", c.Args, arg)
		}
	})
}

func TestAlertmanagerConfig(t *testing.T) {
	cfg := config(t)

	t.Run("routes everything to one receiver", func(t *testing.T) {
		// The child route below inherits this receiver, so a deployment that
		// names its own at the root covers both branches with it.
		if cfg.Route.Receiver != "default" {
			t.Errorf("the root route sends to %q, want default; a route whose receiver is not declared below stops Alertmanager from starting",
				cfg.Route.Receiver)
		}
	})

	t.Run("leaves delivery to the deployment", func(t *testing.T) {
		// Where an alert is delivered is a property of a deployment. An
		// integration written here would be carried by every overlay that takes
		// this base, dev clusters included, and notify whoever it names.
		if len(cfg.Receivers) != 1 {
			t.Fatalf("config.yaml declares %d receivers, want the one the routes name", len(cfg.Receivers))
		}

		r := cfg.Receivers[0]
		if name, _ := r["name"].(string); name != "default" {
			t.Errorf("the receiver is named %q, want default; the routes above name default and Alertmanager refuses a config whose receiver is missing", name)
		}
		for _, key := range slices.Sorted(maps.Keys(r)) {
			if key == "name" {
				continue
			}
			t.Errorf("the receiver carries %q, so this repository decides where a deployment's alerts go; an integration belongs in the overlay's replacement ConfigMap", key)
		}
	})

	t.Run("repeats a critical alert sooner", func(t *testing.T) {
		// The point of the child route. Inheriting the root's interval would
		// leave a critical alert as quiet as the rest of them.
		const matcher = `severity="critical"`

		var child *routeNode
		for i, r := range cfg.Route.Routes {
			if slices.Contains(r.Matchers, matcher) {
				child = &cfg.Route.Routes[i]
				break
			}
		}
		if child == nil {
			t.Fatalf("no child route matches %s, so a critical alert repeats on the root's interval", matcher)
		}

		root := interval(t, "the root route", cfg.Route.RepeatInterval)
		crit := interval(t, "the critical route", child.RepeatInterval)
		if crit >= root {
			t.Errorf("the critical route repeats every %s against the root's %s, so severity buys a reader nothing", crit, root)
		}
	})
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

// containerNamed returns one container of one workload.
func containerNamed(t *testing.T, docs []object, kind, workload, name string) container {
	t.Helper()

	for _, c := range objectNamed(t, docs, kind, workload).Spec.Template.Spec.Containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("%s %s carries no container %q", kind, workload, name)
	return container{}
}

// covers reports whether a rule carries a match of exactly this type, path and
// method. The type is asserted because it is what decides whether a match
// intercepts a sub-path: PathPrefix /api/v2 covers POST /api/v2/silences and
// Exact /api/v2 covers nothing a caller would send. An empty method means the
// match names none, which is what makes it apply to every method rather than to
// one.
func covers(rule routeRule, matchType, path, method string) bool {
	for _, m := range rule.Matches {
		if m.Path.Type == matchType && m.Path.Value == path && m.Method == method {
			return true
		}
	}
	return false
}

// config parses the routing tree Alertmanager runs with.
func config(t *testing.T) alertConfig {
	t.Helper()

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("reading %s: %v", configFile, err)
	}

	var cfg alertConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parsing %s, which Alertmanager refuses to start on: %v", configFile, err)
	}
	if cfg.Route == nil {
		t.Fatalf("%s declares no route, so Alertmanager refuses to start and nothing is notified", configFile)
	}
	return cfg
}

// interval parses one repeat_interval, failing on a value Alertmanager would
// reject at startup.
func interval(t *testing.T, what, value string) time.Duration {
	t.Helper()

	if value == "" {
		t.Fatalf("%s names no repeat_interval", what)
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		t.Fatalf("%s repeats every %q, which is not a duration: %v", what, value, err)
	}
	return d
}

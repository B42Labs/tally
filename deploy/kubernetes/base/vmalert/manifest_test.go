// This file pins the manifest properties that decide whether an evaluated rule
// reaches anyone and what the Gateway hands a stranger. Both fail quietly: a
// vmalert that has lost -notifier.url still starts, still evaluates every rule
// in rules.yaml and still answers its probes, while nothing is ever posted to
// Alertmanager, and a route that carries the root serves /-/reload, /flags,
// /metrics and /debug/pprof to whoever opens the host. `make check-alerting`
// reads rules.yaml and never this file. The test reads the YAML from disk and
// needs no cluster.
package vmalert_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

const manifestFile = "vmalert.yaml"

// object is the part of a manifest document this test asserts over. yaml.v3
// ignores every field not named here, so one shape covers both kinds.
type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// Deployment
		Template struct {
			Spec struct {
				Containers []container `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
		// HTTPRoute
		Rules []routeRule `yaml:"rules"`
	} `yaml:"spec"`
}

type container struct {
	Name string   `yaml:"name"`
	Args []string `yaml:"args"`
}

type routeRule struct {
	Matches []struct {
		Path struct {
			Type  string `yaml:"type"`
			Value string `yaml:"value"`
		} `yaml:"path"`
	} `yaml:"matches"`
}

func TestVmalertDeployment(t *testing.T) {
	c := containerNamed(t, objects(t, manifestFile), "Deployment", "vmalert", "vmalert")

	t.Run("names what it queries and where it posts", func(t *testing.T) {
		// Without -notifier.url vmalert evaluates every rule and posts none of
		// them. The pod stays Ready, the rules API keeps reporting their state,
		// and the only symptom is an Alertmanager that never receives anything.
		for _, arg := range []string{
			"-datasource.url=http://victoriametrics:8428",
			"-notifier.url=http://alertmanager:9093",
		} {
			if !slices.Contains(c.Args, arg) {
				t.Errorf("args = %v, want one of them %q", c.Args, arg)
			}
		}
	})

	t.Run("keeps the for timers outside the process", func(t *testing.T) {
		// The pair writes ALERTS_FOR_STATE to the store and reads it back at
		// startup. Editing rules.yaml rolls the pod by design, through the
		// generated ConfigMap name, so without either half every `for` in the
		// file silently restarts its clock on every edit: TallyScrapeTargetDown
		// waits out its 5m again and TallyExporterServiceSilent its 15m.
		for _, arg := range []string{
			"-remoteWrite.url=http://victoriametrics:8428",
			"-remoteRead.url=http://victoriametrics:8428",
		} {
			if !slices.Contains(c.Args, arg) {
				t.Errorf("args = %v, want one of them %q; the flags are a pair, and either alone leaves the state it carries in the process", c.Args, arg)
			}
		}
	})
}

func TestRouteWithholdsTheRoot(t *testing.T) {
	// vmalert answers /-/reload, /flags, /metrics and /debug/pprof at the root
	// alone. Under the /vmalert prefix they answer 400, so what keeps them off
	// the published host is that the route names prefixes instead of carrying
	// the host whole.
	want := []string{"/api/v1/alerts", "/api/v1/rules", "/vmalert"}

	route := routeNamed(t, objects(t, manifestFile), "vmalert")
	if len(route.Spec.Rules) != 1 {
		t.Fatalf("the vmalert HTTPRoute has %d rules, want the one that names the published prefixes", len(route.Spec.Rules))
	}

	var got []string
	for _, m := range route.Spec.Rules[0].Matches {
		if m.Path.Type != "PathPrefix" {
			t.Errorf("a match is of type %q, want PathPrefix", m.Path.Type)
			continue
		}
		got = append(got, m.Path.Value)
	}
	slices.Sort(got)

	if slices.Contains(got, "/") {
		t.Error("the route publishes the root, so /-/reload, /flags, /metrics and /debug/pprof are reachable through the Gateway")
	}
	if !slices.Equal(got, want) {
		t.Errorf("the route publishes %v, want %v", got, want)
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

// routeNamed returns one HTTPRoute, failing if it is missing rather than
// asserting over a zero value.
func routeNamed(t *testing.T, docs []object, name string) object {
	t.Helper()

	for _, doc := range docs {
		if doc.Kind == "HTTPRoute" && doc.Metadata.Name == name {
			return doc
		}
	}
	t.Fatalf("no HTTPRoute named %q", name)
	return object{}
}

// containerNamed returns one container of one workload.
func containerNamed(t *testing.T, docs []object, kind, workload, name string) container {
	t.Helper()

	for _, doc := range docs {
		if doc.Kind != kind || doc.Metadata.Name != workload {
			continue
		}
		for _, c := range doc.Spec.Template.Spec.Containers {
			if c.Name == name {
				return c
			}
		}
		t.Fatalf("%s %s carries no container %q", kind, workload, name)
	}
	t.Fatalf("no %s named %q", kind, workload)
	return container{}
}

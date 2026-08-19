// This file pins the manifest properties that keep an anonymous request from
// reaching further than a dashboard. Every one of them fails silently: a pod
// that mounts the wrong Secret key still passes its readiness probe, a route
// that publishes the datasource proxy still renders every panel, and a session
// cookie without Secure looks the same in the browser. The test reads the YAML
// from disk and needs no cluster.
package grafana_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The four files that between them bound what the datasource proxy reaches.
// Denying the proxy prefix at the Gateway is the weakest of them: the variable
// queries read /api/datasources/uid/<uid>/resources, which forwards a
// caller-supplied path the same way and cannot be denied without emptying every
// dropdown, and a percent-encoded spelling of the prefix reaches Grafana as the
// decoded path regardless. What bounds the tunnel is its far end, so the
// datasource file, the route map of the container it names, and the delete key
// on the store are asserted here alongside the route rather than apart.
const (
	grafanaManifest = "grafana.yaml"
	vmManifest      = "../victoriametrics/victoriametrics.yaml"
	datasourceFile  = "provisioning/datasources/vm.yaml"
	vmauthConfig    = "vmauth/auth.yaml"
)

// object is the part of a manifest document this test asserts over. yaml.v3
// ignores every field not named here, so one shape covers all four kinds.
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
		// Deployment and StatefulSet
		Template struct {
			Spec struct {
				Containers []container `yaml:"containers"`
				Volumes    []volume    `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
		// HTTPRoute
		Rules []routeRule `yaml:"rules"`
	} `yaml:"spec"`
}

type container struct {
	Name      string   `yaml:"name"`
	Args      []string `yaml:"args"`
	Env       []envVar `yaml:"env"`
	Resources struct {
		Limits map[string]string `yaml:"limits"`
	} `yaml:"resources"`
}

type envVar struct {
	Name      string `yaml:"name"`
	Value     string `yaml:"value"`
	ValueFrom struct {
		SecretKeyRef struct {
			Name string `yaml:"name"`
			Key  string `yaml:"key"`
		} `yaml:"secretKeyRef"`
	} `yaml:"valueFrom"`
}

type volume struct {
	Name   string `yaml:"name"`
	Secret struct {
		SecretName string `yaml:"secretName"`
		Items      []struct {
			Key  string `yaml:"key"`
			Path string `yaml:"path"`
		} `yaml:"items"`
	} `yaml:"secret"`
}

type routeRule struct {
	Matches []struct {
		Path struct {
			Value string `yaml:"value"`
		} `yaml:"path"`
	} `yaml:"matches"`
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

func TestGrafanaDeployment(t *testing.T) {
	c := grafanaContainer(t)

	t.Run("mounts the admin password by name", func(t *testing.T) {
		// Grafana's __FILE resolver logs an unreadable path and carries on with
		// its built-in password rather than exiting, so a Secret carrying a
		// differently-named key would leave admin/admin live on a pod that
		// passes its probes. Naming the key fails the mount instead.
		v := volumeNamed(t, objects(t, grafanaManifest), "Deployment", "grafana", "admin-password")

		if v.Secret.SecretName != "tally-grafana" {
			t.Errorf("secretName = %q, want %q", v.Secret.SecretName, "tally-grafana")
		}
		if len(v.Secret.Items) != 1 {
			t.Fatalf("the volume names %d keys, want exactly admin-password; any other key would mount and start the pod on the built-in password",
				len(v.Secret.Items))
		}
		if got := v.Secret.Items[0].Key; got != "admin-password" {
			t.Errorf("mounted key = %q, want %q", got, "admin-password")
		}
	})

	t.Run("marks the session cookie Secure", func(t *testing.T) {
		// Grafana derives the attribute from its own protocol, which is plain
		// HTTP behind the Gateway that terminates TLS. Without this the browser
		// attaches the session to the cleartext request the port-80 listener
		// answers with its redirect, where anyone on the path reads it.
		if got := envValue(c, "GF_SECURITY_COOKIE_SECURE"); got != "true" {
			t.Errorf("GF_SECURITY_COOKIE_SECURE = %q, want %q", got, "true")
		}
	})

	t.Run("caps the memory a query can take", func(t *testing.T) {
		// Grafana buffers a whole datasource response before re-encoding it. No
		// cgroup ceiling here leaves the node's OOM killer to choose, and the
		// datastores beside it are the candidates.
		if c.Resources.Limits["memory"] == "" {
			t.Error("no memory limit, so a wide query grows the process until the node evicts something")
		}
	})

	t.Run("caps what the read proxy runs at once", func(t *testing.T) {
		// vmauth defaults to 1000 requests in flight, against a memory limit
		// two orders of magnitude below Grafana's own. A Pod is Ready only
		// while every container is, so an OOM-killed sidecar drops the single
		// replica out of the EndpointSlice and the host answers 503 for
		// everything, login page included. Refusing a query costs one panel.
		vmauth := containerNamed(t, objects(t, grafanaManifest), "Deployment", "grafana", "vmauth")

		if _, ok := argValue(vmauth, "-maxConcurrentRequests="); !ok {
			t.Error("vmauth names no -maxConcurrentRequests, so a burst through the datasource proxy runs at the default of 1000 and an OOM kill takes the whole Grafana host with it")
		}
	})
}

func TestGrafanaRoute(t *testing.T) {
	docs := objects(t, grafanaManifest)

	t.Run("denies the datasource proxy", func(t *testing.T) {
		// /api/datasources/proxy/uid/<uid>/<path> forwards <path> to the
		// datasource URL and checks only datasources:query, which the viewer
		// role holds. Nothing here needs it, so it is refused. This is the
		// outer ring; TestDatasourceReachesReadsOnly covers the one that holds.
		const prefix = "/api/datasources/proxy"

		rule, ok := ruleMatching(docs, "grafana", prefix)
		if !ok {
			t.Fatalf("the grafana HTTPRoute has no rule for %s, so the / rule carries it to Grafana", prefix)
		}
		if len(rule.BackendRefs) != 0 {
			t.Errorf("the %s rule names %d backends, so the request is forwarded rather than refused",
				prefix, len(rule.BackendRefs))
		}
		if len(rule.Filters) != 1 || rule.Filters[0].Type != "ExtensionRef" {
			t.Fatalf("the %s rule carries no ExtensionRef filter, so it answers 500 rather than a refusal", prefix)
		}

		ref := rule.Filters[0].ExtensionRef
		for _, doc := range docs {
			if doc.Kind != ref.Kind || doc.Metadata.Name != ref.Name {
				continue
			}
			if doc.Spec.DirectResponse.StatusCode != 403 {
				t.Errorf("%s %s responds %d, want 403", ref.Kind, ref.Name, doc.Spec.DirectResponse.StatusCode)
			}
			return
		}
		t.Errorf("the rule references %s/%s, which this file does not define", ref.Kind, ref.Name)
	})
}

func TestDatasourceReachesReadsOnly(t *testing.T) {
	// Grafana forwards a caller-supplied path to the datasource URL from
	// /api/datasources/proxy/uid/<uid>/<path> and from
	// /api/datasources/uid/<uid>/resources/<path>, and both check
	// datasources:query alone. The dev overlay hands that permission to a
	// request without a session, and the second endpoint is what fills the
	// variable dropdowns, so it cannot be withheld at the Gateway. Whatever the
	// datasource URL answers is therefore reachable by anyone who can open a
	// dashboard: named at the store, it would publish /api/v1/write and the
	// admin API with it.
	vmauth := containerNamed(t, objects(t, grafanaManifest), "Deployment", "grafana", "vmauth")

	t.Run("names the read proxy rather than the store", func(t *testing.T) {
		addr, ok := argValue(vmauth, "-httpListenAddr=")
		if !ok {
			t.Fatal("the vmauth container names no -httpListenAddr, so it listens wherever its default is rather than where the datasource points")
		}
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			t.Errorf("vmauth listens on %q, want the pod loopback; on the pod IP the read proxy is published to the namespace", addr)
		}
		if got, want := datasourceURL(t, "VictoriaMetrics"), "http://"+addr; got != want {
			t.Errorf("datasource url = %q, want %q; the url is what the two proxy endpoints reach", got, want)
		}
	})

	t.Run("keeps vmauth's own endpoints off the datasource port", func(t *testing.T) {
		// /health, /metrics, /flags, /-/reload and /debug/pprof/* are answered
		// by vmauth ahead of url_map, so on the port the datasource URL names
		// they would sit inside the tunnel with no src_paths entry bounding
		// them: /debug/pprof/trace?seconds=300 buffered in Grafana, /-/reload
		// on demand, /flags and /metrics read off. A second address is what
		// makes the route map below the only thing that port consults.
		addr, _ := argValue(vmauth, "-httpListenAddr=")

		internal, ok := argValue(vmauth, "-httpInternalListenAddr=")
		if !ok {
			t.Fatal("vmauth names no -httpInternalListenAddr, so /debug/pprof, /metrics, /flags and /-/reload answer on the port the datasource URL names")
		}
		if internal == addr {
			t.Errorf("-httpInternalListenAddr = %q, the same address as -httpListenAddr; the built-in endpoints stay inside the tunnel", internal)
		}
		if !strings.HasPrefix(internal, "127.0.0.1:") {
			t.Errorf("-httpInternalListenAddr = %q, want the pod loopback", internal)
		}
	})

	t.Run("carries the read paths and refuses the rest", func(t *testing.T) {
		// Where the read paths carry on to.
		const store = "http://victoriametrics:8428"

		if got, ok := argValue(vmauth, "-auth.config="); !ok || got != "/etc/vmauth/auth.yaml" {
			t.Fatalf("vmauth reads its route map from %q, not the mounted %s this test asserts over", got, vmauthConfig)
		}

		routes := vmauthRoutes(t)
		for _, tc := range []struct {
			path string
			read bool
		}{
			// What a panel, a variable query and the datasource itself ask for.
			{"/api/v1/query", true},
			{"/api/v1/query_range", true},
			{"/api/v1/series", true},
			{"/api/v1/labels", true},
			{"/api/v1/label/cloud/values", true},
			{"/api/v1/metadata", true},
			{"/api/v1/status/buildinfo", true},
			// What port 8428 also serves and nothing here may reach.
			{"/api/v1/write", false},
			{"/api/v1/import", false},
			{"/api/v1/import/prometheus", false},
			{"/api/v1/admin/tsdb/delete_series", false},
			{"/snapshot/create", false},
		} {
			backend := vmauthBackend(routes, tc.path)
			switch {
			case tc.read && backend == "":
				t.Errorf("%s is refused, and a dashboard does not render without it", tc.path)
			case tc.read && backend != store:
				t.Errorf("%s reaches %q, want %q", tc.path, backend, store)
			case !tc.read && backend != "":
				t.Errorf("%s reaches %q, so whoever can open a dashboard reaches it through the datasource proxy", tc.path, backend)
			}
		}
	})
}

func TestVictoriaMetricsDeleteAPI(t *testing.T) {
	t.Run("takes a delete key from a Secret", func(t *testing.T) {
		// With no key, /api/v1/admin/tsdb/delete_series answers whoever reaches
		// port 8428 and drops a match of every series in the 13-month window.
		// The port is open to every pod in the namespace, so the Gateway route
		// is not what closes this.
		const arg = "-deleteAuthKey=$(VM_DELETE_AUTH_KEY)"

		c := containerNamed(t, objects(t, vmManifest), "StatefulSet", "victoriametrics", "victoriametrics")

		if !slices.Contains(c.Args, arg) {
			t.Errorf("args = %v, want one of them %q", c.Args, arg)
		}
		for _, e := range c.Env {
			if e.Name != "VM_DELETE_AUTH_KEY" {
				continue
			}
			if e.ValueFrom.SecretKeyRef.Name == "" || e.ValueFrom.SecretKeyRef.Key == "" {
				t.Error("VM_DELETE_AUTH_KEY is not read from a Secret, so the key is in the manifest")
			}
			return
		}
		t.Error("no VM_DELETE_AUTH_KEY variable, so the flag expands to the empty key and the endpoint stays open")
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

// grafanaContainer returns the one container of the Grafana Deployment.
func grafanaContainer(t *testing.T) container {
	t.Helper()
	return containerNamed(t, objects(t, grafanaManifest), "Deployment", "grafana", "grafana")
}

// containerNamed returns one container of one workload, failing if either is
// missing rather than asserting over a zero value.
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

// volumeNamed returns one volume of one workload.
func volumeNamed(t *testing.T, docs []object, kind, workload, name string) volume {
	t.Helper()

	for _, doc := range docs {
		if doc.Kind != kind || doc.Metadata.Name != workload {
			continue
		}
		for _, v := range doc.Spec.Template.Spec.Volumes {
			if v.Name == name {
				return v
			}
		}
		t.Fatalf("%s %s carries no volume %q", kind, workload, name)
	}
	t.Fatalf("no %s named %q", kind, workload)
	return volume{}
}

// ruleMatching returns the rule of one HTTPRoute whose path prefix is exactly
// the one given.
func ruleMatching(docs []object, route, prefix string) (routeRule, bool) {
	for _, doc := range docs {
		if doc.Kind != "HTTPRoute" || doc.Metadata.Name != route {
			continue
		}
		for _, rule := range doc.Spec.Rules {
			for _, m := range rule.Matches {
				if m.Path.Value == prefix {
					return rule, true
				}
			}
		}
	}
	return routeRule{}, false
}

// envValue returns the literal value of one environment variable, or "".
func envValue(c container, name string) string {
	for _, e := range c.Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

// argValue returns the value of one flag argument, written -flag=value.
func argValue(c container, flag string) (string, bool) {
	for _, a := range c.Args {
		if v, ok := strings.CutPrefix(a, flag); ok {
			return v, true
		}
	}
	return "", false
}

// datasourceURL returns the url of one provisioned datasource.
func datasourceURL(t *testing.T, name string) string {
	t.Helper()

	var file struct {
		Datasources []struct {
			Name string `yaml:"name"`
			URL  string `yaml:"url"`
		} `yaml:"datasources"`
	}
	decodeFile(t, datasourceFile, &file)

	for _, ds := range file.Datasources {
		if ds.Name == name {
			return ds.URL
		}
	}
	t.Fatalf("%s declares no datasource %q", datasourceFile, name)
	return ""
}

// vmauthRoute is one url_map entry, with its src_paths compiled the way vmauth
// compiles them: anchored, so an entry matches a request path whole or not at
// all, and a path no entry matches never leaves the pod.
type vmauthRoute struct {
	paths  []*regexp.Regexp
	prefix string
}

// vmauthRoutes returns the route map the read proxy runs with.
func vmauthRoutes(t *testing.T) []vmauthRoute {
	t.Helper()

	var cfg struct {
		UnauthorizedUser struct {
			URLMap []struct {
				SrcPaths  []string `yaml:"src_paths"`
				URLPrefix string   `yaml:"url_prefix"`
			} `yaml:"url_map"`
		} `yaml:"unauthorized_user"`
	}
	decodeFile(t, vmauthConfig, &cfg)

	if len(cfg.UnauthorizedUser.URLMap) == 0 {
		t.Fatalf("%s carries no unauthorized_user route map, so vmauth refuses every request and no panel renders", vmauthConfig)
	}

	routes := make([]vmauthRoute, 0, len(cfg.UnauthorizedUser.URLMap))
	for _, m := range cfg.UnauthorizedUser.URLMap {
		r := vmauthRoute{prefix: m.URLPrefix}
		for _, p := range m.SrcPaths {
			re, err := regexp.Compile("^(?:" + p + ")$")
			if err != nil {
				t.Fatalf("src_paths %q does not compile, and vmauth exits on it: %v", p, err)
			}
			r.paths = append(r.paths, re)
		}
		routes = append(routes, r)
	}
	return routes
}

// vmauthBackend returns the backend a request path reaches, or "" when no entry
// matches it and vmauth answers "missing route". That "" means refused for
// every path, rather than for every path vmauth does not answer itself, only
// because -httpInternalListenAddr moves the latter off this port; the subtest
// above is what holds that.
func vmauthBackend(routes []vmauthRoute, path string) string {
	for _, r := range routes {
		for _, re := range r.paths {
			if re.MatchString(path) {
				return r.prefix
			}
		}
	}
	return ""
}

// decodeFile parses one single-document file into v.
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

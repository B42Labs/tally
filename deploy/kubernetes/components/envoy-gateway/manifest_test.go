// This file pins what the stack takes from Envoy Gateway: the GatewayClass, the
// rate limit on the OTLP routes, and the two rules that answer a request with a
// 403. Every mismatch it looks for fails quietly. A policy whose target names
// no route leaves both OTLP hostnames unlimited, a JSON patch whose target the
// base does not declare is applied to nothing, and neither stops kustomize. A
// deny rule that references a filter no file declares renders as well, and the
// Gateway rejects the route on the cluster. The tests read the YAML from disk
// and need neither a cluster nor kustomize.
package envoygateway_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	kustomizationFile    = "kustomization.yaml"
	gatewayClassFile     = "gatewayclass.yaml"
	rateLimitFile        = "otlp-rate-limit.yaml"
	grafanaDenyFile      = "grafana-deny.yaml"
	alertmanagerDenyFile = "alertmanager-deny.yaml"

	// The base files that declare the Gateway and the routes this component
	// targets.
	baseGatewayFile      = "../../base/gateway/gateway.yaml"
	baseCollectorFile    = "../../base/otel-collector/otel-collector.yaml"
	baseGrafanaFile      = "../../base/grafana/grafana.yaml"
	baseAlertmanagerFile = "../../base/alertmanager/alertmanager.yaml"

	// The API group of the routes, and the group of Envoy Gateway's own kinds.
	routeGroup = "gateway.networking.k8s.io"
	envoyGroup = "gateway.envoyproxy.io"
)

// kustomization is the part of the component these tests assert over.
type kustomization struct {
	Kind      string   `yaml:"kind"`
	Resources []string `yaml:"resources"`
	Patches   []struct {
		Patch  string `yaml:"patch"`
		Target struct {
			Group string `yaml:"group"`
			Kind  string `yaml:"kind"`
			Name  string `yaml:"name"`
		} `yaml:"target"`
	} `yaml:"patches"`
}

// object is the part of a manifest document these tests assert over. yaml.v3
// ignores every field not named here, so one shape covers the three kinds of
// the component and the Gateway and the routes of the base.
type object struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// Gateway
		GatewayClassName string `yaml:"gatewayClassName"`
		// GatewayClass
		ControllerName string `yaml:"controllerName"`
		// BackendTrafficPolicy
		TargetRefs []targetRef `yaml:"targetRefs"`
		RateLimit  struct {
			Type  string `yaml:"type"`
			Local struct {
				Rules []struct {
					ClientSelectors []struct {
						SourceCIDR struct {
							Value string `yaml:"value"`
							Type  string `yaml:"type"`
						} `yaml:"sourceCIDR"`
					} `yaml:"clientSelectors"`
					Limit struct {
						Requests int    `yaml:"requests"`
						Unit     string `yaml:"unit"`
					} `yaml:"limit"`
				} `yaml:"rules"`
			} `yaml:"local"`
		} `yaml:"rateLimit"`
		// HTTPRouteFilter
		DirectResponse struct {
			StatusCode int `yaml:"statusCode"`
		} `yaml:"directResponse"`
	} `yaml:"spec"`
}

type targetRef struct {
	Group string `yaml:"group"`
	Kind  string `yaml:"kind"`
	Name  string `yaml:"name"`
}

// patchOp is one operation of a JSON patch whose value is a route rule.
type patchOp struct {
	Op    string    `yaml:"op"`
	Path  string    `yaml:"path"`
	Value routeRule `yaml:"value"`
}

type routeRule struct {
	Matches []routeMatch `yaml:"matches"`
	Filters []struct {
		Type         string    `yaml:"type"`
		ExtensionRef targetRef `yaml:"extensionRef"`
	} `yaml:"filters"`
	BackendRefs []struct {
		Name string `yaml:"name"`
	} `yaml:"backendRefs"`
}

type routeMatch struct {
	Path struct {
		Type  string `yaml:"type"`
		Value string `yaml:"value"`
	} `yaml:"path"`
	Method string `yaml:"method"`
}

func TestComponentDeclaresTheEnvoyObjects(t *testing.T) {
	// An overlay lists this directory under components, and kustomize refuses
	// a Kustomization there. The four files are everything the base leaves to
	// Envoy Gateway: one dropped from resources stays in the repository and
	// reaches no cluster, and both overlays still render.
	k := kustomizationOf(t)

	if k.Kind != "Component" {
		t.Errorf("%s has kind %q, want Component, which is what an overlay's components entry takes", kustomizationFile, k.Kind)
	}
	want := []string{gatewayClassFile, rateLimitFile, grafanaDenyFile, alertmanagerDenyFile}
	if !slices.Equal(k.Resources, want) {
		t.Fatalf("%s lists the resources %v, want exactly %v", kustomizationFile, k.Resources, want)
	}

	// The Gateway of the base names a class and declares none. Under a name
	// this component does not declare no controller picks the Gateway up, and
	// under another controller name Envoy Gateway does not.
	const controller = "gateway.envoyproxy.io/gatewayclass-controller"
	className := objectNamed(t, baseGatewayFile, "Gateway", "tally").Spec.GatewayClassName
	if className == "" {
		t.Fatalf("Gateway tally in %s names no gatewayClassName, so no controller picks it up", baseGatewayFile)
	}
	if got := objectNamed(t, gatewayClassFile, "GatewayClass", className).Spec.ControllerName; got != controller {
		t.Errorf("GatewayClass %s names the controller %q, want %q", className, got, controller)
	}
	objectNamed(t, rateLimitFile, "BackendTrafficPolicy", "otel-collector-otlp")
	objectNamed(t, grafanaDenyFile, "HTTPRouteFilter", "grafana-deny-datasource-proxy")
	objectNamed(t, alertmanagerDenyFile, "HTTPRouteFilter", "alertmanager-deny-writes")
}

func TestRateLimitTargetsTheOtlpRoutes(t *testing.T) {
	// The policy lives apart from the routes it limits. A target that names
	// nothing is accepted by kustomize and by the API server, and it leaves
	// both OTLP hostnames answering every request with a bcrypt comparison.
	policy := objectNamed(t, rateLimitFile, "BackendTrafficPolicy", "otel-collector-otlp")
	if len(policy.Spec.TargetRefs) == 0 {
		t.Fatalf("BackendTrafficPolicy otel-collector-otlp in %s has no targetRefs, and a policy without a target limits nothing", rateLimitFile)
	}

	base := objects(t, baseCollectorFile)
	for _, ref := range policy.Spec.TargetRefs {
		declared := slices.ContainsFunc(base, func(o object) bool {
			return inGroup(o, ref.Group) && o.Kind == ref.Kind && o.Metadata.Name == ref.Name
		})
		if !declared {
			t.Errorf("the policy targets %s %s in group %q, which %s does not declare, so the limit applies to nothing",
				ref.Kind, ref.Name, ref.Group, baseCollectorFile)
		}
	}

	for _, want := range []targetRef{
		{routeGroup, "HTTPRoute", "otel-collector-http"},
		{routeGroup, "GRPCRoute", "otel-collector-grpc"},
	} {
		if !slices.Contains(policy.Spec.TargetRefs, want) {
			t.Errorf("the policy does not target %s %s, so that OTLP hostname is unlimited", want.Kind, want.Name)
		}
	}
}

func TestRateLimitCountsPerClientAddress(t *testing.T) {
	// A policy that targets both routes and carries no limit is accepted as
	// well, and so is one whose rules share a bucket or leave a source out.
	limit := objectNamed(t, rateLimitFile, "BackendTrafficPolicy", "otel-collector-otlp").Spec.RateLimit
	if limit.Type != "Local" {
		t.Fatalf("the policy has the rate limit type %q, want Local; without a limit every request reaches the bcrypt comparison", limit.Type)
	}

	var perAddress, routeDefault bool
	for i, rule := range limit.Local.Rules {
		if rule.Limit.Requests <= 0 {
			t.Errorf("rule %d limits to %d requests, want a positive number", i, rule.Limit.Requests)
		}
		if rule.Limit.Unit != "Second" {
			t.Errorf("rule %d counts per %q, want Second; a longer window answers a publisher 429 once it has spent the window's requests", i, rule.Limit.Unit)
		}
		if len(rule.ClientSelectors) == 0 {
			routeDefault = true
			continue
		}
		for _, s := range rule.ClientSelectors {
			// A Distinct selector on a narrower range sends every address
			// outside it to the shared bucket of the route default.
			if s.SourceCIDR.Type == "Distinct" && s.SourceCIDR.Value == "0.0.0.0/0" {
				perAddress = true
			}
		}
	}
	if !perAddress {
		t.Errorf("no rule selects a Distinct sourceCIDR over 0.0.0.0/0, so every publisher outside the range draws from one bucket and emptying it takes no credential")
	}
	if !routeDefault {
		t.Errorf("no rule is without clientSelectors, so a source no selector matches, an IPv6 client for one, is unlimited")
	}
}

func TestGrafanaDenyRule(t *testing.T) {
	// /api/datasources/proxy/uid/<uid>/<path> forwards <path> to the
	// datasource URL and checks only datasources:query, which the viewer role
	// holds. Nothing in the base needs it, so it is refused. This is the outer
	// ring; TestDatasourceReachesReadsOnly in the base covers the one that
	// holds.
	const prefix = "/api/datasources/proxy"

	rule := denyRule(t, "grafana")
	if !covers(rule, "PathPrefix", prefix, "") {
		t.Errorf("the deny rule has no PathPrefix match for %s on every method, so the base's / rule carries it to Grafana", prefix)
	}
	assertRefusal(t, rule, "grafana", grafanaDenyFile, baseGrafanaFile)
}

func TestAlertmanagerDenyRule(t *testing.T) {
	rule := denyRule(t, "alertmanager")

	// Port 9093 answers reads and writes on one API, and the UI needs the
	// reads. The base's rule forwards GET alone, so a write is published by
	// nothing. What a missing entry here costs is the answer: the UI's silence
	// form gets the Gateway's 404 rather than a refusal that says why.
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
	assertRefusal(t, rule, "alertmanager", alertmanagerDenyFile, baseAlertmanagerFile)
}

// kustomizationOf decodes the component's kustomization.yaml.
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

// objects decodes every document of one manifest file.
func objects(t *testing.T, path string) []object {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
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

// objectNamed returns one object of one file, failing if it is missing rather
// than asserting over a zero value.
func objectNamed(t *testing.T, path, kind, name string) object {
	t.Helper()

	for _, doc := range objects(t, path) {
		if doc.Kind == kind && doc.Metadata.Name == name {
			return doc
		}
	}
	t.Fatalf("%s declares no %s named %q", path, kind, name)
	return object{}
}

// inGroup reports whether an object's apiVersion is in one API group.
func inGroup(o object, group string) bool {
	return strings.HasPrefix(o.APIVersion, group+"/")
}

// denyRule returns the rule the component adds to one HTTPRoute of the base.
// The patch has to be a single add at index 0. A replace there would take the
// base's forwarding rule out of the route, and an index past the end of the
// list fails the build of every overlay.
func denyRule(t *testing.T, route string) routeRule {
	t.Helper()

	var found [][]patchOp
	for i, p := range kustomizationOf(t).Patches {
		if p.Target.Group != routeGroup || p.Target.Kind != "HTTPRoute" || p.Target.Name != route {
			continue
		}
		var ops []patchOp
		if err := yaml.Unmarshal([]byte(p.Patch), &ops); err != nil {
			t.Fatalf("parsing patches[%d] of %s against HTTPRoute %s as a JSON patch: %v", i, kustomizationFile, route, err)
		}
		found = append(found, ops)
	}
	if len(found) != 1 {
		t.Fatalf("%s holds %d patches targeting HTTPRoute %s in group %s, want one", kustomizationFile, len(found), route, routeGroup)
	}

	ops := found[0]
	if len(ops) != 1 || ops[0].Op != "add" || ops[0].Path != "/spec/rules/0" {
		t.Fatalf("the patch on HTTPRoute %s in %s is not a single add at /spec/rules/0, which puts the deny rule ahead of the rule the base carries",
			route, kustomizationFile)
	}
	return ops[0].Value
}

// assertRefusal holds a deny rule to what makes it answer a 403: no backend,
// one ExtensionRef filter, a filter file that declares the HTTPRouteFilter the
// rule references, and a base file that declares the route the patch targets.
// The Gateway API has no core way to answer without a backend, so a rule that
// names neither a backend nor a filter answers 500, which reads as an outage
// rather than as a decision.
func assertRefusal(t *testing.T, rule routeRule, route, filterFile, baseFile string) {
	t.Helper()

	if len(rule.BackendRefs) != 0 {
		t.Errorf("the deny rule on HTTPRoute %s names %d backends, so the request is forwarded rather than refused", route, len(rule.BackendRefs))
	}
	if len(rule.Filters) != 1 || rule.Filters[0].Type != "ExtensionRef" {
		t.Fatalf("the deny rule on HTTPRoute %s carries %d filters, want exactly one ExtensionRef; a rule with neither backend nor filter answers 500 rather than a refusal",
			route, len(rule.Filters))
	}

	ref := rule.Filters[0].ExtensionRef
	if ref.Group != envoyGroup || ref.Kind != "HTTPRouteFilter" {
		t.Fatalf("the deny rule on HTTPRoute %s references %s in group %q, want HTTPRouteFilter in %s", route, ref.Kind, ref.Group, envoyGroup)
	}
	filters := objects(t, filterFile)
	i := slices.IndexFunc(filters, func(o object) bool {
		return inGroup(o, ref.Group) && o.Kind == ref.Kind && o.Metadata.Name == ref.Name
	})
	if i < 0 {
		t.Fatalf("the deny rule on HTTPRoute %s references %s %s, which %s does not declare, so the Gateway rejects the route and the host answers nothing",
			route, ref.Kind, ref.Name, filterFile)
	}
	if got := filters[i].Spec.DirectResponse.StatusCode; got != 403 {
		t.Errorf("%s %s responds %d, want 403", ref.Kind, ref.Name, got)
	}

	// kustomize applies a patch whose target no object matches to nothing,
	// without an error, and the overlay renders without the refusal.
	if base := objectNamed(t, baseFile, "HTTPRoute", route); !inGroup(base, routeGroup) {
		t.Errorf("HTTPRoute %s in %s has apiVersion %q, want the group %s the patch targets", route, baseFile, base.APIVersion, routeGroup)
	}
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

package adapters_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/reconciliation/adapters"
)

// testCloud is the clouds.yaml entry every test here authenticates with, and
// therefore the os_cloud every adapter_config names.
const testCloud = "tally-test"

// testRegion is the region the written clouds.yaml asks for and the recorded
// catalog answers with. The two have to agree: a region the catalog does not
// publish reads exactly like a service the cloud does not run.
const testRegion = "RegionOne"

// endpointHost is what the recorded catalog holds where the live server belongs.
// A test server's address exists only once it listens, so no fixture can record
// it and the helper substitutes it at serve time. The pagination links inside
// the recorded listings hold it for the same reason.
const endpointHost = "https://openstack.invalid"

// The paths the recorded catalog's endpoints put each listing at. A test names
// the listing it wants answered by its path, so the endpoint prefixes a
// deployment publishes stay in one place.
const (
	serversPath       = "/compute/v2.1/servers/detail"
	flavorsPath       = "/compute/v2.1/flavors/detail"
	volumesPath       = "/volume/v3/volumes/detail"
	floatingIPsPath   = "/network/v2.0/floatingips"
	imagesPath        = "/image/v2/images"
	loadBalancersPath = "/load-balancer/v2.0/lbaas/loadbalancers"
)

// keystoneToken renders the recorded token document for a running server:
// endpointHost becomes the server's own address, and every service type named
// in missing is dropped from the catalog, which is how a test builds a cloud
// that does not run that service.
func keystoneToken(t *testing.T, serverURL string, missing []string) []byte {
	t.Helper()

	recorded, err := os.ReadFile(filepath.Join("testdata", "keystone_token.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal([]byte(strings.ReplaceAll(string(recorded), endpointHost, serverURL)),
		&document); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	token, ok := document["token"].(map[string]any)
	if !ok {
		t.Fatalf("the fixture holds no token object")
	}
	catalog, ok := token["catalog"].([]any)
	if !ok {
		t.Fatalf("the fixture holds no service catalog")
	}

	kept := make([]any, 0, len(catalog))
	for _, entry := range catalog {
		published, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("the fixture holds a catalog entry that is not an object")
		}
		serviceType, ok := published["type"].(string)
		if !ok {
			t.Fatalf("the fixture holds a catalog entry without a type")
		}
		if slices.Contains(missing, serviceType) {
			continue
		}
		kept = append(kept, entry)
	}
	token["catalog"] = kept

	rendered, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return rendered
}

// request is one call the cloud answered, as much of it as a test holds the
// adapter to: which listing was asked for, and what was asked of it.
type request struct {
	path  string
	query url.Values
}

// cloud is a running OpenStack. It answers the token request, the listings a
// test registered on it, and nothing else, and it keeps every request it saw:
// a listing that is never sent is as much a defect as one that is answered
// wrongly, and neither shows in the observations alone.
type cloud struct {
	*httptest.Server
	mux *http.ServeMux

	mu   sync.Mutex
	seen []request
	// served counts the pages already handed out per listing path, which is what
	// makes a second request to one listing the second page of it.
	served map[string]int
}

// newCloud starts an OpenStack that authenticates. It answers the token request
// from the recorded fixture, leaving out the service types named in missing,
// and it answers the version documents the versionless catalog entries make
// gophercloud ask for.
func newCloud(t *testing.T, missing ...string) *cloud {
	t.Helper()

	c := &cloud{mux: http.NewServeMux(), served: map[string]int{}}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen = append(c.seen, request{path: r.URL.Path, query: r.URL.Query()})
		c.mu.Unlock()
		c.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(c.Close)

	token := keystoneToken(t, c.URL, missing)
	c.mux.HandleFunc("POST /v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Subject-Token", "gAAAAABtally")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(token)
	})

	// Glance, neutron and octavia are published without a version in the path,
	// the way a deployment publishes them, so gophercloud asks each of them
	// which major versions it speaks before it accepts the endpoint.
	for path, version := range map[string]string{
		"/image/":         "v2.15",
		"/network/":       "v2.0",
		"/load-balancer/": "v2.0",
	} {
		c.mux.HandleFunc("GET "+path+"{$}", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"versions": [{"id": %q, "status": "CURRENT"}]}`, version)
		})
	}

	// A listing no test registered holds nothing, which is what lets a test say
	// what one service holds and stay silent about the four it does not test.
	c.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	return c
}

// serve answers a listing from the recorded pages, one page per request, in the
// order they are named: a listing of two pages is registered as two fixtures,
// and the second is what following the first one's link leads to.
//
// A request past the last recorded page is answered with a 500, which is how a
// test builds a cloud that stops answering part way through a listing.
func (c *cloud) serve(t *testing.T, path string, fixtures ...string) {
	t.Helper()

	pages := make([][]byte, 0, len(fixtures))
	for _, name := range fixtures {
		pages = append(pages, c.fixture(t, name))
	}

	c.mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()

		if c.served[path] >= len(pages) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		page := pages[c.served[path]]
		c.served[path]++

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(page)
	})
}

// fixture reads a recorded response and points the links inside it at the
// running server, the same substitution the token document takes.
func (c *cloud) fixture(t *testing.T, name string) []byte {
	t.Helper()

	recorded, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return []byte(strings.ReplaceAll(string(recorded), endpointHost, c.URL))
}

// requestsTo is every request the cloud answered for one listing, in the order
// they arrived.
func (c *cloud) requestsTo(path string) []request {
	c.mu.Lock()
	defer c.mu.Unlock()

	var matched []request
	for _, seen := range c.seen {
		if seen.path == path {
			matched = append(matched, seen)
		}
	}
	return matched
}

// writeCloudsYAML points the process at a clouds.yaml that authenticates
// against serverURL. OS_CLIENT_CONFIG_FILE makes the written file the only search
// location, so the adapter runs its production lookup and still cannot reach a
// developer's real clouds.yaml.
//
// The other OS_* variables are emptied for the same reason: they override the
// file, and a shell that has them set from a real cloud would otherwise decide
// what the test authenticates as.
//
// Setting the environment is process-wide, which is why no test in this file
// runs in parallel.
func writeCloudsYAML(t *testing.T, serverURL string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "clouds.yaml")
	content := fmt.Sprintf(`clouds:
  %s:
    auth:
      auth_url: %s/v3
      username: tally
      password: reconcile
      project_id: 4c9d2f6b81e34a7f9b3c5d8e0a1f2b34
      user_domain_name: Default
    region_name: %s
    interface: public
`, testCloud, serverURL, testRegion)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("OS_CLIENT_CONFIG_FILE", path)
	for _, name := range []string{"OS_CACERT", "OS_CERT", "OS_INTERFACE", "OS_KEY", "OS_REGION_NAME"} {
		t.Setenv(name, "")
	}
}

// discardLogs is where a run's warnings go in a test that is not about them.
var discardLogs = slog.New(slog.DiscardHandler)

// drain runs a resource stream to its end and keeps both sides of it. What the
// stream did not report matters as much as what it did, so neither side is
// dropped.
func drain(t *testing.T, stream iter.Seq2[reconciliation.ObservedResource, error],
) ([]reconciliation.ObservedResource, []error) {
	t.Helper()

	var observed []reconciliation.ObservedResource
	var errs []error
	for resource, err := range stream {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		observed = append(observed, resource)
	}
	return observed, errs
}

// rendered is one observation as a test asserts on it: every field it carries,
// with the size in the JSON form the projection stores and the diff compares.
// That form is the assertion that matters for a quantity: 0.5 and the float
// artifact a rounded division would leave read differently here.
func rendered(t *testing.T, resource reconciliation.ObservedResource) string {
	t.Helper()

	size := "none"
	if resource.Size != nil {
		encoded, err := json.Marshal(resource.Size)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		size = string(encoded)
	}
	return fmt.Sprintf("type=%s id=%s project=%s state=%s size=%s created=%s deleted=%s",
		resource.ResourceType, resource.ResourceID, resource.ProjectID, resource.State, size,
		instant(resource.CreatedAt), instant(resource.DeletedAt))
}

// instant renders a timestamp the way an observation has to carry it. The zone
// is part of the assertion: a platform that reports a local time is only
// comparable against a projection row once it has been converted.
func instant(reported *time.Time) string {
	if reported == nil {
		return "none"
	}
	return reported.Format(time.RFC3339Nano) + " " + reported.Location().String()
}

// assertObserved holds a stream's observations against what the test expects,
// in order and in full.
func assertObserved(t *testing.T, observed []reconciliation.ObservedResource, want ...string) {
	t.Helper()

	got := make([]string, 0, len(observed))
	for _, resource := range observed {
		got = append(got, rendered(t, resource))
	}
	if !slices.Equal(got, want) {
		t.Errorf("ListResources() observed\n\t%s\nwant\n\t%s",
			strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
}

func TestOpenStackReportsThePlatformItObserves(t *testing.T) {
	if platform := adapters.NewOpenStack(time.Now, discardLogs).Platform(); platform != "openstack" {
		t.Errorf("Platform() = %q, want %q", platform, "openstack")
	}
}

func TestOpenStackRefusesAnUnusableConfig(t *testing.T) {
	adapter := adapters.NewOpenStack(time.Now, discardLogs)

	tests := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{
			name: "os_cloud is missing",
			cfg:  map[string]any{},
			want: "os_cloud",
		},
		{
			name: "os_cloud is empty",
			cfg:  map[string]any{"os_cloud": ""},
			want: "os_cloud",
		},
		{
			name: "os_cloud is not a string",
			cfg:  map[string]any{"os_cloud": 42},
			want: "os_cloud",
		},
		{
			name: "include_octavia is not a boolean",
			cfg:  map[string]any{"os_cloud": testCloud, "include_octavia": "yes"},
			want: "include_octavia",
		},
		{
			name: "a setting is unknown",
			cfg:  map[string]any{"os_cloud": testCloud, "region_name": testRegion},
			want: "region_name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := adapter.ResourceTypes(tt.cfg)
			if err == nil {
				t.Fatalf("ResourceTypes() error = nil, want an error naming %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ResourceTypes() error = %q, want it to name %q", err, tt.want)
			}

			// No cloud is reachable from here, and none has to be: a config the
			// adapter cannot read is refused before it authenticates.
			observed, errs := drain(t, adapter.ListResources(t.Context(), tt.cfg, nil))
			if len(observed) != 0 {
				t.Errorf("ListResources() yielded %d observations, want 0", len(observed))
			}
			if len(errs) != 1 {
				t.Fatalf("ListResources() yielded %d errors, want 1", len(errs))
			}
			if !strings.Contains(errs[0].Error(), tt.want) {
				t.Errorf("ListResources() error = %q, want it to name %q", errs[0], tt.want)
			}

			var enumErr *reconciliation.EnumerationError
			if errors.As(errs[0], &enumErr) {
				t.Errorf("ListResources() error = %q, want a plain error that aborts the run", errs[0])
			}
		})
	}
}

func TestOpenStackReportsTheResourceTypesItEnumerates(t *testing.T) {
	adapter := adapters.NewOpenStack(time.Now, discardLogs)

	tests := []struct {
		name string
		cfg  map[string]any
		want []string
	}{
		{
			name: "octavia is left out by default",
			cfg:  map[string]any{"os_cloud": testCloud},
			want: []string{"floating_ip", "image", "instance", "volume"},
		},
		{
			name: "octavia is enumerated on request",
			cfg:  map[string]any{"os_cloud": testCloud, "include_octavia": true},
			want: []string{"floating_ip", "image", "instance", "loadbalancer", "volume"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := adapter.ResourceTypes(tt.cfg)
			if err != nil {
				t.Fatalf("ResourceTypes() error = %v, want nil", err)
			}

			// The framework builds a set out of the answer, so only the members
			// are the adapter's promise, not the order they arrive in.
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Errorf("ResourceTypes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenStackAbortsWhenKeystoneRefusesTheCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	writeCloudsYAML(t, server.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))

	if len(observed) != 0 {
		t.Errorf("ListResources() yielded %d observations, want 0", len(observed))
	}
	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1", len(errs))
	}

	// A cloud that would not say what it holds has said nothing about any one
	// resource type, so the error may not be attributable to one: the run has to
	// end rather than treat the whole inventory as gone.
	var enumErr *reconciliation.EnumerationError
	if errors.As(errs[0], &enumErr) {
		t.Errorf("ListResources() error = %q, want a plain error that aborts the run", errs[0])
	}
}

func TestOpenStackAbortsWhenTheCloudIsNotInCloudsYAML(t *testing.T) {
	writeCloudsYAML(t, newCloud(t).URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": "os-prod-eu1"}, nil))

	if len(observed) != 0 {
		t.Errorf("ListResources() yielded %d observations, want 0", len(observed))
	}
	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1", len(errs))
	}
	if !strings.Contains(errs[0].Error(), "os-prod-eu1") {
		t.Errorf("ListResources() error = %q, want it to name the cloud it looked for", errs[0])
	}

	var enumErr *reconciliation.EnumerationError
	if errors.As(errs[0], &enumErr) {
		t.Errorf("ListResources() error = %q, want a plain error that aborts the run", errs[0])
	}
}

func TestOpenStackHoldsBackOnlyTheServiceTheCloudDoesNotPublish(t *testing.T) {
	writeCloudsYAML(t, newCloud(t, "load-balancer").URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud, "include_octavia": true}, nil))

	if len(observed) != 0 {
		t.Errorf("ListResources() yielded %d observations, want 0", len(observed))
	}
	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1", len(errs))
	}

	// The other four services answered, so only load balancers stay unknown and
	// the run carries on with what it could reach.
	var enumErr *reconciliation.EnumerationError
	if !errors.As(errs[0], &enumErr) {
		t.Fatalf("ListResources() error = %q, want an EnumerationError", errs[0])
	}
	if enumErr.ResourceType != "loadbalancer" {
		t.Errorf("EnumerationError.ResourceType = %q, want %q", enumErr.ResourceType, "loadbalancer")
	}
}

func TestOpenStackReachesEveryServiceItEnumerates(t *testing.T) {
	writeCloudsYAML(t, newCloud(t).URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud, "include_octavia": true}, nil))

	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}
	if len(observed) != 0 {
		t.Errorf("ListResources() yielded %d observations, want 0", len(observed))
	}
}

func TestOpenStackObservesEveryProjectsInstances(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, serversPath, "servers_page1.json", "servers_page2.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	requests := cloud.requestsTo(serversPath)
	if len(requests) != 2 {
		t.Fatalf("the cloud answered %d requests for %s, want 2, one per page",
			len(requests), serversPath)
	}

	// A sync of a cloud has to see what every project runs, not what the service
	// user happens to own. gophercloud spells the specification's all_tenants=1
	// as all_tenants=true, which nova reads as the same request.
	if got := requests[0].query.Get("all_tenants"); got != "true" {
		t.Errorf("the listing asked for all_tenants=%q, want %q", got, "true")
	}
	// The second page is reachable only by following the first page's link, so
	// the marker is what says the pagination was followed rather than repeated.
	if got := requests[1].query.Get("marker"); got != "7f3a1c58-9d2b-4e17-8c6a-2b5d0e9f4a31" {
		t.Errorf("the second page was asked for with marker=%q, want the last id of the first", got)
	}

	// 512 MiB is half a gibibyte and has to stay half a gibibyte, and the disk is
	// the root disk plus the ephemeral one. The states are nova's own, mapped
	// where Tally names the state differently (stopped) and passed through where
	// it has no name of its own for it (rescued).
	assertObserved(t, observed,
		`type=instance id=7f3a1c58-9d2b-4e17-8c6a-2b5d0e9f4a31 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size={"disk_gb":30,"flavor":"m1.small","ram_gb":0.5,"vcpus":1} `+
			`created=2026-07-14T09:12:33Z UTC deleted=none`,
		`type=instance id=2d8b6e10-4f3c-49a5-b7d2-6c1a8e5f0937 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=shutoff `+
			`size={"disk_gb":5,"flavor":"m1.tiny","ram_gb":1,"vcpus":1} `+
			`created=2026-07-20T16:45:02Z UTC deleted=none`,
		`type=instance id=9c4f7a23-1e6d-4b80-95af-3d2c7b1e6048 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=rescued `+
			`size={"disk_gb":5,"flavor":"m1.tiny","ram_gb":1,"vcpus":1} `+
			`created=2026-07-21T08:03:44Z UTC deleted=none`)
}

func TestOpenStackReadsTheFlavorsOfOneRunOnlyOnce(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, serversPath, "servers_flavor_ids.json")
	cloud.serve(t, flavorsPath, "flavors.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	// Two instances carry a flavor id and neither carries the flavor itself. One
	// listing answers both, and the second instance's id, which no flavor claims,
	// does not send the adapter back to the cloud either.
	requests := cloud.requestsTo(flavorsPath)
	if len(requests) != 1 {
		t.Fatalf("the cloud answered %d requests for %s, want 1", len(requests), flavorsPath)
	}
	// The flavors a deployment sells to single projects are private, and an
	// instance running on one is billed like any other.
	if got := requests[0].query.Get("is_public"); got != "None" {
		t.Errorf("the flavors were asked for with is_public=%q, want %q", got, "None")
	}

	// A flavor the cloud no longer publishes leaves the instance without a size.
	// That is not an error: the instance exists, and what it is made of is the
	// one thing this run did not learn, so the diff has nothing to compare and
	// leaves the size the projection already holds alone.
	assertObserved(t, observed,
		`type=instance id=1e5d9b74-3c2a-4f68-b019-7d4c6a2e8f53 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size={"disk_gb":40,"flavor":"m1.medium","ram_gb":4,"vcpus":2} `+
			`created=2026-02-03T11:20:15Z UTC deleted=none`,
		`type=instance id=6b0e2f81-5a7d-4c39-8e42-1f9b3d5c7a06 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=active `+
			`size=none created=2026-02-04T14:52:40Z UTC deleted=none`)
}

func TestOpenStackReportsAFailedFlavorListingForInstancesAlone(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, serversPath, "servers_flavor_ids.json")
	// No flavor page is recorded, so the first request for one fails.
	cloud.serve(t, flavorsPath)
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))

	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1", len(errs))
	}
	var enumErr *reconciliation.EnumerationError
	if !errors.As(errs[0], &enumErr) {
		t.Fatalf("ListResources() error = %q, want an EnumerationError", errs[0])
	}
	if enumErr.ResourceType != "instance" {
		t.Errorf("EnumerationError.ResourceType = %q, want %q", enumErr.ResourceType, "instance")
	}

	// The failure is reported once and the instances keep streaming: the cloud
	// said which ones exist, and only what they are made of stayed unknown.
	if requests := cloud.requestsTo(flavorsPath); len(requests) != 1 {
		t.Errorf("the cloud answered %d requests for %s, want 1", len(requests), flavorsPath)
	}
	assertObserved(t, observed,
		`type=instance id=1e5d9b74-3c2a-4f68-b019-7d4c6a2e8f53 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size=none created=2026-02-03T11:20:15Z UTC deleted=none`,
		`type=instance id=6b0e2f81-5a7d-4c39-8e42-1f9b3d5c7a06 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=active `+
			`size=none created=2026-02-04T14:52:40Z UTC deleted=none`)
}

func TestOpenStackObservesEveryProjectsVolumes(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, volumesPath, "volumes.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	requests := cloud.requestsTo(volumesPath)
	if len(requests) != 1 {
		t.Fatalf("the cloud answered %d requests for %s, want 1", len(requests), volumesPath)
	}
	if got := requests[0].query.Get("all_tenants"); got != "true" {
		t.Errorf("the listing asked for all_tenants=%q, want %q", got, "true")
	}

	// Cinder's status is booked as it reads, error_deleting included: a status
	// Tally renamed would be a state no collector event ever writes, and every
	// sync would report the same drift again.
	assertObserved(t, observed,
		`type=volume id=5b8c2d17-6e94-4a30-b7f1-2c8d5e0a9b64 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=in-use `+
			`size={"size_gb":100,"type":"ssd"} created=2026-06-02T08:15:00Z UTC deleted=none`,
		`type=volume id=c3f9a041-8b25-4d67-9e13-7a6c2b4d8e50 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=error_deleting `+
			`size={"size_gb":25,"type":"hdd"} created=2026-06-11T21:03:44Z UTC deleted=none`)
}

func TestOpenStackObservesEveryProjectsFloatingIPs(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, floatingIPsPath, "floatingips.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	// Every allocated address is active, whatever neutron says about it: an
	// address is billed for being allocated, and reporting the DOWN of a
	// detached one would be drift on every run for as long as it stays detached.
	//
	// The third address is one an old neutron reports without an address member
	// at all and the fourth one is unreadable. Both count as IPv4, because that
	// is what a deployment allocates unless it says otherwise, and an address
	// left unobserved would read as deleted.
	assertObserved(t, observed,
		`type=floating_ip id=e1a7c3b5-0d92-4f68-8a41-6b2c9d5e7f03 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size={"ip_version":4} created=2026-05-11T07:30:00Z UTC deleted=none`,
		`type=floating_ip id=f28d5a91-6c04-4e73-b1f8-9a2d7c3e5b46 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=active `+
			`size={"ip_version":6} created=2026-05-12T09:00:00Z UTC deleted=none`,
		`type=floating_ip id=a3b6d8f2-4e01-4c95-87a3-5d1b9c6e2f48 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=active `+
			`size={"ip_version":4} created=none deleted=none`,
		`type=floating_ip id=b45c9e17-2a68-4d03-91f7-6c8e0b3a5d92 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=active `+
			`size={"ip_version":4} created=2026-05-13T18:22:07Z UTC deleted=none`)
}

func TestOpenStackObservesEveryProjectsImages(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, imagesPath, "images.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	// The fixture holds three images and two of them are nothing a collector ever
	// booked: one belongs to no project, and one is still queued, which is what
	// glance says before the bits are uploaded and there is a size to bill.
	// 2684354560 bytes are two and a half gibibytes exactly.
	assertObserved(t, observed,
		`type=image id=0b1c2d3e-4f50-4617-8394-0516273849ab `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size={"size_gb":2.5} created=2026-03-01T12:00:00Z UTC deleted=none`)
}

func TestOpenStackObservesLoadBalancersOnlyOnRequest(t *testing.T) {
	t.Run("octavia is left alone by default", func(t *testing.T) {
		cloud := newCloud(t)
		cloud.serve(t, loadBalancersPath, "loadbalancers.json")
		writeCloudsYAML(t, cloud.URL)

		observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
			map[string]any{"os_cloud": testCloud}, nil))

		if len(errs) != 0 {
			t.Fatalf("ListResources() errors = %v, want none", errs)
		}
		if len(observed) != 0 {
			t.Errorf("ListResources() yielded %d observations, want 0", len(observed))
		}
		// A deployment that runs no octavia must not be asked about it at all, and
		// the recorded requests are the only place that shows.
		if requests := cloud.requestsTo(loadBalancersPath); len(requests) != 0 {
			t.Errorf("the cloud answered %d requests for %s, want none",
				len(requests), loadBalancersPath)
		}
	})

	t.Run("octavia is enumerated on request", func(t *testing.T) {
		cloud := newCloud(t)
		cloud.serve(t, loadBalancersPath, "loadbalancers.json")
		writeCloudsYAML(t, cloud.URL)

		observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
			map[string]any{"os_cloud": testCloud, "include_octavia": true}, nil))
		if len(errs) != 0 {
			t.Fatalf("ListResources() errors = %v, want none", errs)
		}

		// Octavia keeps a deleted load balancer in its listing, and one on its way
		// out is already paid for up to the delete the collector recorded.
		// Observing either would resurrect a resource that is gone.
		assertObserved(t, observed,
			`type=loadbalancer id=4a5b6c7d-8e90-4123-a456-7b8c9d0e1f23 `+
				`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
				`size={"listeners":2,"pools":1} created=2026-04-20T10:00:00Z UTC deleted=none`)
	})
}

func TestOpenStackObservesNothingOfAnEmptyCloud(t *testing.T) {
	cloud := newCloud(t)
	listings := map[string]string{
		serversPath:       "servers_empty.json",
		volumesPath:       "volumes_empty.json",
		floatingIPsPath:   "floatingips_empty.json",
		imagesPath:        "images_empty.json",
		loadBalancersPath: "loadbalancers_empty.json",
	}
	for path, fixture := range listings {
		cloud.serve(t, path, fixture)
	}
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud, "include_octavia": true}, nil))

	// A cloud that holds nothing is not a cloud that failed. Every type has to
	// come back empty and complete, or the missed-delete pass would skip the one
	// type a sync of an emptied project has the most to say about.
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}
	if len(observed) != 0 {
		t.Errorf("ListResources() yielded %d observations, want 0", len(observed))
	}
	for path := range listings {
		if requests := cloud.requestsTo(path); len(requests) != 1 {
			t.Errorf("the cloud answered %d requests for %s, want 1", len(requests), path)
		}
	}
}

func TestOpenStackHoldsBackOnlyTheTypeWhoseListingBreaks(t *testing.T) {
	cloud := newCloud(t)
	// The recorded first page links to a second one the cloud then fails to
	// answer, which is the outage that hits a sync mid-pagination.
	cloud.serve(t, serversPath, "servers_page1.json")
	cloud.serve(t, volumesPath, "volumes.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))

	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1", len(errs))
	}
	var enumErr *reconciliation.EnumerationError
	if !errors.As(errs[0], &enumErr) {
		t.Fatalf("ListResources() error = %q, want an EnumerationError", errs[0])
	}
	if enumErr.ResourceType != "instance" {
		t.Errorf("EnumerationError.ResourceType = %q, want %q", enumErr.ResourceType, "instance")
	}

	// What the broken listing did report stays reported, and the types after it
	// are enumerated as if nothing had happened: only instances are the type the
	// missed-delete pass has to leave alone.
	assertObserved(t, observed,
		`type=instance id=7f3a1c58-9d2b-4e17-8c6a-2b5d0e9f4a31 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size={"disk_gb":30,"flavor":"m1.small","ram_gb":0.5,"vcpus":1} `+
			`created=2026-07-14T09:12:33Z UTC deleted=none`,
		`type=volume id=5b8c2d17-6e94-4a30-b7f1-2c8d5e0a9b64 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=in-use `+
			`size={"size_gb":100,"type":"ssd"} created=2026-06-02T08:15:00Z UTC deleted=none`,
		`type=volume id=c3f9a041-8b25-4d67-9e13-7a6c2b4d8e50 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=error_deleting `+
			`size={"size_gb":25,"type":"hdd"} created=2026-06-11T21:03:44Z UTC deleted=none`)
}

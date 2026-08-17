package adapters_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/gophercloud/gophercloud/v2"

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
	// computeVersionPath is the nova endpoint itself, where the version document
	// that names the microversion range it speaks is published.
	computeVersionPath = "/compute/v2.1/"
	serversPath        = "/compute/v2.1/servers/detail"
	flavorsPath        = "/compute/v2.1/flavors/detail"
	volumesPath        = "/volume/v3/volumes/detail"
	floatingIPsPath    = "/network/v2.0/floatingips"
	imagesPath         = "/image/v2/images"
	loadBalancersPath  = "/load-balancer/v2.0/lbaas/loadbalancers"
)

// keystoneToken renders the recorded token document for a running server:
// endpointHost becomes the server's own address, every service type named in
// missing is dropped from the catalog, which is how a test builds a cloud that
// does not run that service, and every role name not in roles is dropped from
// the token, which is how a test builds an account that may not see every
// project.
func keystoneToken(t *testing.T, serverURL string, missing, roles []string) []byte {
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

	assigned, ok := token["roles"].([]any)
	if !ok {
		t.Fatalf("the fixture holds no roles")
	}
	keptRoles := make([]any, 0, len(assigned))
	for _, entry := range assigned {
		role, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("the fixture holds a role that is not an object")
		}
		name, ok := role["name"].(string)
		if !ok {
			t.Fatalf("the fixture holds a role without a name")
		}
		if !slices.Contains(roles, name) {
			continue
		}
		keptRoles = append(keptRoles, entry)
	}
	token["roles"] = keptRoles

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
// adapter to: which listing was asked for, what was asked of it, and the
// compute microversion it was asked at.
type request struct {
	path         string
	query        url.Values
	microversion string
}

// listing is one registered set of recorded pages together with the requests it
// answers. The live servers listing and the deleted-servers listing are sent to
// one path and are told apart by their query alone, so a path holds more than
// one of these.
type listing struct {
	answers func(url.Values) bool
	pages   [][]byte
	// served counts the pages already handed out, which is what makes a second
	// request to one listing the second page of it.
	served int
}

// cloud is a running OpenStack. It answers the token request, the listings a
// test registered on it, and nothing else, and it keeps every request it saw:
// a listing that is never sent is as much a defect as one that is answered
// wrongly, and neither shows in the observations alone.
type cloud struct {
	*httptest.Server
	mux *http.ServeMux
	// missing is the service types this cloud publishes no endpoint for, kept so
	// that a token reissued with other roles keeps the catalog it was built with.
	missing []string

	mu       sync.Mutex
	seen     []request
	listings map[string][]*listing
	// token is the document keystone answers with. It is held rather than closed
	// over so a test can reissue it with the roles of a lesser account.
	token []byte
	// refuseScope makes nova answer the scope probe the way it answers an account
	// its policy grants no listing across projects.
	refuseScope bool
	// cancelRun ends the run while the cloud is answering cancelOn, where a test
	// asked for that.
	cancelRun context.CancelFunc
	cancelOn  func(request) bool
}

// adminRoles is what the recorded token carries by default: the roles a
// deployment's admin-scoped reconciliation account is assigned under the stock
// policies. Nothing the adapter does reads them — what an account may list is
// the cloud's answer, not its token's — so a test says what the cloud refuses
// with refuseCrossProjectListings and reissues the token with useRoles only
// where the names themselves are the subject.
var adminRoles = []string{"admin", "reader"}

// newCloud starts an OpenStack that authenticates. It answers the token request
// from the recorded fixture, leaving out the service types named in missing,
// and it answers the version documents the versionless catalog entries make
// gophercloud ask for.
func newCloud(t *testing.T, missing ...string) *cloud {
	t.Helper()

	c := &cloud{mux: http.NewServeMux(), missing: missing, listings: map[string][]*listing{}}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		seen := request{
			path:         r.URL.Path,
			query:        r.URL.Query(),
			microversion: r.Header.Get("X-OpenStack-Nova-API-Version"),
		}
		c.seen = append(c.seen, seen)
		cancelRun := c.cancelRun != nil && c.cancelOn(seen)
		refuseScope := c.refuseScope
		c.mu.Unlock()

		if cancelRun {
			c.cancelRun()
			// The request stays unanswered until the cancelled client has dropped
			// it. Answering it would race the cancellation the test is about.
			<-r.Context().Done()
			return
		}

		// The scope probe is answered here rather than from a registered listing,
		// so that a test says what one service holds without also accounting for
		// the request that proves the account may read any of it.
		if seen.path == serversPath && probe(seen.query) {
			w.Header().Set("Content-Type", "application/json")
			if refuseScope {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"forbidden": {"message": "Policy does not allow `+
					`os_compute_api:servers:index:get_all_tenants to be performed.", "code": 403}}`)
				return
			}
			_, _ = io.WriteString(w, `{"servers": []}`)
			return
		}
		c.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(c.Close)

	c.token = keystoneToken(t, c.URL, missing, adminRoles)
	c.mux.HandleFunc("POST /v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
		c.mu.Lock()
		token := c.token
		c.mu.Unlock()

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

// refuseCrossProjectListings makes nova answer a listing across every project
// with the 403 its policy answers an account that may not read one, which is
// what a credential rotation that recreates an application credential without
// its role assignment leaves behind, and what a policy.yaml an operator
// narrowed does. Whatever the token's roles are called has no bearing on it:
// the cloud is what decides, so the cloud is what says no.
func (c *cloud) refuseCrossProjectListings() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.refuseScope = true
}

// useRoles makes the cloud issue tokens carrying exactly these roles, which is
// what a deployment whose policy.yaml resolves context_is_admin to a role of its
// own hands its reconciliation account.
func (c *cloud) useRoles(t *testing.T, roles ...string) {
	t.Helper()

	token := keystoneToken(t, c.URL, c.missing, roles)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
}

// serveComputeMicroversions publishes the version document nova answers at its
// versioned endpoint, which is what says whether the microversion the adapter
// wants is one this cloud speaks. A cloud that registers none answers with the
// mux's 204, which reads as a nova that publishes no range at all.
func (c *cloud) serveComputeMicroversions(t *testing.T, minimum, maximum string) {
	t.Helper()

	c.mux.HandleFunc("GET "+computeVersionPath+"{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"version": {"id": "v2.1", "status": "CURRENT", "version": %q, "min_version": %q}}`,
			maximum, minimum)
	})
}

// serveComputeVersionID publishes nova's version document with an id of the
// test's choosing. Everything gophercloud renders into the error of a failed
// negotiation comes off the wire — an id it cannot parse is echoed back inside
// it, and a nova the catalog publishes without a version in its path hands the
// whole response body back the same way — so how long that error is is the
// platform's to decide and not this adapter's.
func (c *cloud) serveComputeVersionID(t *testing.T, id string) {
	t.Helper()

	c.mux.HandleFunc("GET "+computeVersionPath+"{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"version": {"id": %q, "status": "CURRENT", "version": "2.100", "min_version": "2.1"}}`,
			id)
	})
}

// serve answers a listing from the recorded pages, one page per request, in the
// order they are named: a listing of two pages is registered as two fixtures,
// and the second is what following the first one's link leads to.
//
// A request past the last recorded page is answered with a 500, which is how a
// test builds a cloud that stops answering part way through a listing.
func (c *cloud) serve(t *testing.T, path string, fixtures ...string) {
	t.Helper()

	c.answer(t, path, live, fixtures...)
}

// serveDeleted answers the deleted-servers listing, the one request to the
// servers path that carries deleted=true. A cloud that registers none answers
// that request with a 500, which is how a test breaks the deleted listing while
// the live one keeps working.
func (c *cloud) serveDeleted(t *testing.T, fixtures ...string) {
	t.Helper()

	c.answer(t, serversPath, deleted, fixtures...)
}

// live, deleted and probe are the three requests that reach the servers path.
// Every other listing is registered as a live one, because no other is ever
// asked with deleted=true and no other is ever probed.
//
// The probe is the scope request the run sends before it observes anything: one
// server from the start of the listing. A page nova links on from carries the
// same limit back, so the marker is what tells the two apart — the probe asks
// for the head of the listing and never follows anything.
func live(query url.Values) bool    { return !probe(query) && !deleted(query) }
func deleted(query url.Values) bool { return query.Get("deleted") == "true" }
func probe(query url.Values) bool {
	return query.Get("limit") == "1" && query.Get("marker") == ""
}

// answer registers the recorded pages one set of requests is served from.
func (c *cloud) answer(t *testing.T, path string, answers func(url.Values) bool,
	fixtures ...string,
) {
	t.Helper()

	registered := &listing{answers: answers}
	for _, name := range fixtures {
		registered.pages = append(registered.pages, c.fixture(t, name))
	}

	first := len(c.listings[path]) == 0
	c.listings[path] = append(c.listings[path], registered)
	if !first {
		return
	}
	c.mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
		page, ok := c.page(path, r.URL.Query())
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(page)
	})
}

// page is the next recorded page for one request, and false where the cloud has
// none left for it: a listing asked for past its last page, or one no fixture
// answers at all.
func (c *cloud) page(path string, query url.Values) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, registered := range c.listings[path] {
		if !registered.answers(query) {
			continue
		}
		if registered.served >= len(registered.pages) {
			return nil, false
		}
		page := registered.pages[registered.served]
		registered.served++
		return page, true
	}
	return nil, false
}

// cancelDuring makes the cloud end the run while it is answering a request the
// predicate picks, and leave that request unanswered. A cancellation that
// arrives between two listings would prove nothing: the stream has to end from
// inside one.
func (c *cloud) cancelDuring(cancel context.CancelFunc, when func(request) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cancelRun, c.cancelOn = cancel, when
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

// requests is every request the cloud saw, in the order they arrived.
func (c *cloud) requests() []request {
	c.mu.Lock()
	defer c.mu.Unlock()

	return slices.Clone(c.seen)
}

// requestsTo is every listing request the cloud answered for one path, in the
// order they arrived. The scope probe is not one of them: it is sent to the
// servers path before any listing and is counted by scopeProbes, so a test that
// says how often a listing was asked for says that and nothing else.
func (c *cloud) requestsTo(path string) []request {
	var matched []request
	for _, seen := range c.requests() {
		if seen.path == path && !probe(seen.query) {
			matched = append(matched, seen)
		}
	}
	return matched
}

// scopeProbes is every scope request the cloud answered. A run sends exactly
// one, before it observes anything.
func (c *cloud) scopeProbes() []request {
	var matched []request
	for _, seen := range c.requests() {
		if seen.path == serversPath && probe(seen.query) {
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

// recordLogs is a logger together with everything written to it. A failure this
// adapter absorbs to keep the run whole reaches neither the stream nor the
// run's stats, so the log is the only place a test can hold it to having said
// that it happened.
//
// The handler is the JSON one the service runs with, so an instant a record
// carries reads here the way it reads in a deployment's log.
func recordLogs() (*slog.Logger, func() string) {
	var written strings.Builder

	return slog.New(slog.NewJSONHandler(&written, nil)), written.String
}

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
			// The scope is established against the cloud, so no setting names the
			// role it is granted by, and this one is refused like any other the
			// adapter does not know. A setting that named it would be the only guard
			// against a run that reads one project as the whole cloud, with its
			// right-hand side supplied by the config file: whoever may edit that file
			// could name a role the token already carries, and the run after it books
			// a delete for every projection row of every other project.
			name: "admin_role is not a setting",
			cfg:  map[string]any{"os_cloud": testCloud, "admin_role": "member"},
			want: "admin_role",
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

func TestOpenStackRefusesAnAccountThatCannotSeeEveryProject(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, serversPath, "servers_page1.json", "servers_page2.json")
	cloud.serve(t, floatingIPsPath, "floatingips.json")
	cloud.serve(t, imagesPath, "images.json")
	// The account of the entry may no longer list across projects. Its token still
	// carries every role the fixture assigns it, because a role name was never
	// what the reach depended on.
	cloud.refuseCrossProjectListings()
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud, "include_octavia": true}, nil))

	// neutron, glance and octavia would each have answered 200 with the account's
	// own project, and nothing downstream can tell that from the whole cloud: the
	// missed-delete pass would book a delete for every row of every other project.
	// So the run ends before the first listing rather than reporting one type at a
	// time, and it names the entry an operator has to repair.
	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1", len(errs))
	}
	var enumErr *reconciliation.EnumerationError
	if errors.As(errs[0], &enumErr) {
		t.Errorf("ListResources() error = %q, want a plain error that aborts the run", errs[0])
	}
	if !strings.Contains(errs[0].Error(), testCloud) {
		t.Errorf("ListResources() error = %q, want it to name the clouds.yaml entry", errs[0])
	}
	if len(observed) != 0 {
		t.Errorf("ListResources() yielded %d observations, want 0", len(observed))
	}

	// The cloud was asked, once, and it refused. That request is the whole of the
	// proof there is to have: only the cloud knows what its policy grants, so
	// nothing read off the token or out of adapter_config could have established
	// the same thing.
	if probes := cloud.scopeProbes(); len(probes) != 1 {
		t.Errorf("the cloud answered %d scope probes, want 1", len(probes))
	}

	// Not one listing followed it. An account that cannot prove its scope must
	// not observe at all, not even through the two listings that would have
	// refused it loudly.
	for _, path := range []string{serversPath, floatingIPsPath, imagesPath} {
		if requests := cloud.requestsTo(path); len(requests) != 0 {
			t.Errorf("the cloud answered %d requests for %s, want none", len(requests), path)
		}
	}
}

func TestOpenStackRefusesACloudThatPublishesNoComputeEndpoint(t *testing.T) {
	cloud := newCloud(t, "compute")
	cloud.serve(t, volumesPath, "volumes.json")
	cloud.serve(t, imagesPath, "images.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))

	// Nova is where the scope is established, so a cloud that publishes none
	// leaves the run with no way to establish it at all. That ends the run rather
	// than costing instances alone their completeness: the four listings after it
	// would go out unprobed, and three of them narrow to one project without
	// saying so.
	if len(observed) != 0 {
		t.Errorf("ListResources() yielded %d observations, want 0", len(observed))
	}
	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1", len(errs))
	}
	var enumErr *reconciliation.EnumerationError
	if errors.As(errs[0], &enumErr) {
		t.Errorf("ListResources() error = %q, want a plain error that aborts the run", errs[0])
	}
	if !strings.Contains(errs[0].Error(), testCloud) {
		t.Errorf("ListResources() error = %q, want it to name the clouds.yaml entry", errs[0])
	}
	for _, path := range []string{volumesPath, imagesPath} {
		if requests := cloud.requestsTo(path); len(requests) != 0 {
			t.Errorf("the cloud answered %d requests for %s, want none", len(requests), path)
		}
	}
}

func TestOpenStackObservesACloudThatGrantsTheScopeUnderAnotherRoleName(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, volumesPath, "volumes.json")
	// policy.yaml is a per-deployment file, so the role a cloud resolves
	// context_is_admin to is the deployment's to pick. This one renamed it, and
	// its reconciliation account holds that role and no stock one: every listing
	// reaches every project, and only the name says otherwise.
	cloud.useRoles(t, "cloud-operator")
	writeCloudsYAML(t, cloud.URL)

	// Nothing in adapter_config says so, and nothing has to. The run asks the
	// cloud whether it may list across every project, and the cloud is the only
	// thing that knows what name its policy grants that to.
	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(
		t.Context(), map[string]any{"os_cloud": testCloud}, nil))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	assertObserved(t, observed,
		`type=volume id=5b8c2d17-6e94-4a30-b7f1-2c8d5e0a9b64 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=in-use `+
			`size={"size_gb":100,"type":"ssd"} created=2026-06-02T08:15:00Z UTC deleted=none`,
		`type=volume id=c3f9a041-8b25-4d67-9e13-7a6c2b4d8e50 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=error_deleting `+
			`size={"size_gb":25,"type":"hdd"} created=2026-06-11T21:03:44Z UTC deleted=none`)
	if probes := cloud.scopeProbes(); len(probes) != 1 {
		t.Errorf("the cloud answered %d scope probes, want 1", len(probes))
	}
}

func TestOpenStackAsksNovaForTheFlavorItEmbeds(t *testing.T) {
	cloud := newCloud(t)
	// A nova that speaks the whole range up to the microversion the adapter wants.
	cloud.serveComputeMicroversions(t, "2.1", "2.100")
	cloud.serve(t, serversPath, "servers_page1.json", "servers_page2.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	// From 2.47 on nova reports a server's flavor out of the instance's own
	// record rather than out of the catalog, so an instance running on a flavor
	// the operator retired still says what it is made of. Without it the flavor
	// cache is the only source, a retired flavor leaves the instance sizeless,
	// and a create correction carrying no size is refused by the size schema on
	// every run of the cloud, forever.
	requests := cloud.requestsTo(serversPath)
	if len(requests) != 2 {
		t.Fatalf("the cloud answered %d requests for %s, want 2, one per page",
			len(requests), serversPath)
	}
	for i, seen := range requests {
		if seen.microversion != "2.47" {
			t.Errorf("page %d was asked for at microversion %q, want %q",
				i+1, seen.microversion, "2.47")
		}
	}
	if len(observed) != 3 {
		t.Errorf("ListResources() yielded %d observations, want the 3 instances", len(observed))
	}

	// The flavors are what the microversion makes unnecessary: every server
	// carries its own, so the catalog is never read.
	if requests := cloud.requestsTo(flavorsPath); len(requests) != 0 {
		t.Errorf("the cloud answered %d requests for %s, want none", len(requests), flavorsPath)
	}
}

func TestOpenStackListsAtNovasOwnMicroversionWhenItSpeaksNoNewerOne(t *testing.T) {
	cloud := newCloud(t)
	// A nova whose range ends below what the adapter would like. The listing is
	// negotiated, not demanded, so it still happens.
	cloud.serveComputeMicroversions(t, "2.1", "2.42")
	cloud.serve(t, serversPath, "servers_flavor_ids.json")
	cloud.serve(t, flavorsPath, "flavors.json")
	writeCloudsYAML(t, cloud.URL)

	logger, written := recordLogs()
	observed, errs := drain(t, adapters.NewOpenStack(time.Now, logger).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	requests := cloud.requestsTo(serversPath)
	if len(requests) != 1 {
		t.Fatalf("the cloud answered %d requests for %s, want 1", len(requests), serversPath)
	}
	if requests[0].microversion != "" {
		t.Errorf("the listing was asked for at microversion %q, want nova's default",
			requests[0].microversion)
	}
	// The flavor cache is what resolves a server that carries its flavor by id,
	// so the instances are still observed with the sizes they are billed for.
	if len(observed) != 2 {
		t.Errorf("ListResources() yielded %d observations, want the 2 instances", len(observed))
	}
	if requests := cloud.requestsTo(flavorsPath); len(requests) != 1 {
		t.Errorf("the cloud answered %d requests for %s, want 1", len(requests), flavorsPath)
	}

	// The run stays clean, which is the whole point of negotiating: nothing it
	// yields says the embedded flavor is gone. The fallback is only complete while
	// the cloud publishes every flavor its instances run on, and an instance on a
	// retired one is observed sizeless after this — a create the size schema then
	// refuses on every run, with nothing naming the cause. The log is where that
	// cause is, so it has to name both the microversion and the reason.
	if logged := written(); !strings.Contains(logged, "2.47") ||
		!strings.Contains(logged, "microversion") {
		t.Errorf("the run logged %q, want it to say why it read the flavor catalog", logged)
	}
}

func TestOpenStackBoundsTheReasonItLogsForAFailedNegotiation(t *testing.T) {
	cloud := newCloud(t)
	// A version document as long as whatever answered the request: an ingress
	// page, a WAF block page, a captive portal. Nothing bounds what a platform
	// answers with, so nothing but the adapter bounds what it writes about it —
	// and it writes this on every sync of every configured cloud, for as long as
	// the upstream stays broken, with nothing that throttles it.
	cloud.serveComputeVersionID(t, strings.Repeat("x", 64<<10))
	cloud.serve(t, serversPath, "servers_flavor_ids.json")
	cloud.serve(t, flavorsPath, "flavors.json")
	writeCloudsYAML(t, cloud.URL)

	logger, written := recordLogs()
	observed, errs := drain(t, adapters.NewOpenStack(time.Now, logger).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}
	// The fallback is the point of negotiating: the run still observes what the
	// cloud holds, whatever the version endpoint answered.
	if len(observed) != 2 {
		t.Errorf("ListResources() yielded %d observations, want the 2 instances", len(observed))
	}

	// The head of the reason is what an operator works from, and it still names
	// the microversion the run wanted, so the record says as much as it did
	// before. What it no longer does is write the platform's answer out in full.
	logged := written()
	if !strings.Contains(logged, "2.47") || !strings.Contains(logged, "truncated") {
		t.Errorf("the run logged %q, want the head of the reason and the cut marked", logged)
	}
	// The bound is 4 KiB, the one the run's stats already keep of a reason. The
	// rest of the record is the message and its two attributes.
	if len(logged) > 8<<10 {
		t.Errorf("the run logged %d bytes of one version document, want it bounded", len(logged))
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

	// The queued image is the one thing skipped: glance says that before the bits
	// are uploaded and there is a size to bill. The deactivated one still exists,
	// still occupies its 5 GiB of the store, and is still a live projection row —
	// glance publishes no notification that would have taken that row out, so
	// skipping it would book a delete for a resource that is there. It is
	// observed as the active image the collector booked, because "active" is the
	// only state a collector ever writes for an image. 2684354560 bytes are two
	// and a half gibibytes exactly.
	assertObserved(t, observed,
		`type=image id=0b1c2d3e-4f50-4617-8394-0516273849ab `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size={"size_gb":2.5} created=2026-03-01T12:00:00Z UTC deleted=none`,
		`type=image id=3e4f5061-7283-494a-b627-38495061728d `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size={"size_gb":5} created=2026-03-06T09:45:31Z UTC deleted=none`)
}

func TestOpenStackHoldsBackImagesGlanceNamesNoOwnerFor(t *testing.T) {
	cloud := newCloud(t)
	// Two of them, because a glance holding one of these holds as many as an
	// operator ever deleted a project of: the run has to say so once.
	cloud.serve(t, imagesPath, "images_ownerless.json")
	cloud.serve(t, volumesPath, "volumes.json")
	cloud.serve(t, loadBalancersPath, "loadbalancers.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud, "include_octavia": true}, nil))

	// The collector books such an image to the project of whoever registered it,
	// so it is a live projection row this run cannot match to an owner. The whole
	// type is held back rather than that row being booked deleted for an absence
	// the adapter caused: a delete is the one correction no later run undoes.
	//
	// One reason, whatever the listing holds. The type is already held back by the
	// first, and a run keeps 100 reasons at most (reconciliation/sync.go:59): one
	// per ownerless image would fill the whole record with copies of this and bury
	// whatever the listings after it failed with.
	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1: %v", len(errs), errs)
	}
	var enumErr *reconciliation.EnumerationError
	if !errors.As(errs[0], &enumErr) {
		t.Fatalf("ListResources() error = %q, want an EnumerationError", errs[0])
	}
	if enumErr.ResourceType != "image" {
		t.Errorf("EnumerationError.ResourceType = %q, want %q", enumErr.ResourceType, "image")
	}
	if !strings.Contains(errs[0].Error(), "1c2d3e4f-5061-4728-9405-1627384950bc") {
		t.Errorf("ListResources() error = %q, want it to name the image it could not place", errs[0])
	}

	// Only images stayed unknown. The types before them are enumerated as if
	// nothing had happened, and so is the one after: ending the image listing is
	// not ending the run.
	assertObserved(t, observed,
		`type=volume id=5b8c2d17-6e94-4a30-b7f1-2c8d5e0a9b64 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=in-use `+
			`size={"size_gb":100,"type":"ssd"} created=2026-06-02T08:15:00Z UTC deleted=none`,
		`type=volume id=c3f9a041-8b25-4d67-9e13-7a6c2b4d8e50 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=error_deleting `+
			`size={"size_gb":25,"type":"hdd"} created=2026-06-11T21:03:44Z UTC deleted=none`,
		`type=loadbalancer id=4a5b6c7d-8e90-4123-a456-7b8c9d0e1f23 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size={"listeners":2,"pools":1} created=2026-04-20T10:00:00Z UTC deleted=none`)
}

func TestOpenStackWalksTheImageListingPastTheOneItCannotPlace(t *testing.T) {
	cloud := newCloud(t)
	// Glance orders its listing newest first, so an ownerless image stands in
	// front of every image registered before it.
	cloud.serve(t, imagesPath, "images_ownerless_first.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))

	// The type is held back, which is what keeps the missed-delete pass off its
	// rows for this run.
	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1: %v", len(errs), errs)
	}
	var enumErr *reconciliation.EnumerationError
	if !errors.As(errs[0], &enumErr) {
		t.Fatalf("ListResources() error = %q, want an EnumerationError", errs[0])
	}
	if enumErr.ResourceType != "image" {
		t.Errorf("EnumerationError.ResourceType = %q, want %q", enumErr.ResourceType, "image")
	}

	// Holding the type back stops the deletes and nothing else: a create or an
	// update is still booked for every image the run observed. So the listing has
	// to be walked to its end. A run that stopped at the ownerless image would
	// leave every older image unobserved until somebody reaps the orphan by hand,
	// and its stats would read exactly as they do here — one error, the same
	// reason — with nothing saying that the type had stopped converging.
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

func TestOpenStackReportsWhatTheCloudAnsweredOnTheTypeThatFailed(t *testing.T) {
	cloud := newCloud(t)
	// No volume page is recorded, so the listing is answered with a 500.
	cloud.serve(t, volumesPath)
	cloud.serve(t, imagesPath, "images.json")
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
	if enumErr.ResourceType != "volume" {
		t.Errorf("EnumerationError.ResourceType = %q, want %q", enumErr.ResourceType, "volume")
	}

	// The cloud's own answer survives the wrapping. What cinder replied is the
	// difference between a service that is down and one that refused the
	// credentials, and an operator reading the run's errors needs it.
	var codeErr gophercloud.ErrUnexpectedResponseCode
	if !errors.As(errs[0], &codeErr) {
		t.Fatalf("ListResources() error = %q, want it to unwrap to the cloud's response code", errs[0])
	}
	if codeErr.Actual != http.StatusInternalServerError {
		t.Errorf("the reported response code = %d, want %d",
			codeErr.Actual, http.StatusInternalServerError)
	}

	// Only volumes stayed unknown. Glance answered, and its images are observed
	// after cinder failed.
	assertObserved(t, observed,
		`type=image id=0b1c2d3e-4f50-4617-8394-0516273849ab `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size={"size_gb":2.5} created=2026-03-01T12:00:00Z UTC deleted=none`,
		`type=image id=3e4f5061-7283-494a-b627-38495061728d `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
			`size={"size_gb":5} created=2026-03-06T09:45:31Z UTC deleted=none`)
}

func TestOpenStackObservesTheInstancesDeletedSinceTheLastRun(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, serversPath, "servers_empty.json")
	cloud.serveDeleted(t, "servers_deleted.json")
	writeCloudsYAML(t, cloud.URL)

	// The bound arrives in whatever zone the last run's timestamp came back in,
	// and nova is asked in UTC either way: two runs of one cloud have to walk the
	// window the sync missed, not the one the zone spelled. It is two hours back
	// rather than a fixed date, so the window the run asks for is the one the
	// bound names rather than the floor a stale bound is clamped to.
	since := time.Now().Add(-2 * time.Hour).Truncate(time.Second).
		In(time.FixedZone("+02:00", 2*60*60))

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, &since))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	requests := cloud.requestsTo(serversPath)
	if len(requests) != 2 {
		t.Fatalf("the cloud answered %d requests for %s, want 2: the live listing and the deleted one",
			len(requests), serversPath)
	}
	if got := requests[0].query.Get("deleted"); got != "" {
		t.Errorf("the live listing asked for deleted=%q, want the live inventory", got)
	}

	deleted := requests[1]
	if got := deleted.query.Get("deleted"); got != "true" {
		t.Errorf("the deleted listing asked for deleted=%q, want %q", got, "true")
	}
	// Without the bound the listing would be every instance the cloud ever ran,
	// and with the wrong one it would be a window this run is not responsible for.
	// The zone is the assertion that matters: the same instant spelled in +02:00
	// names a window two hours off the one the sync missed.
	if want := since.UTC().Format(time.RFC3339); deleted.query.Get("changes-since") != want {
		t.Errorf("the deleted listing asked for changes-since=%q, want %q",
			deleted.query.Get("changes-since"), want)
	}
	// A delete is as much every project's as an instance is.
	if got := deleted.query.Get("all_tenants"); got != "true" {
		t.Errorf("the deleted listing asked for all_tenants=%q, want %q", got, "true")
	}

	// A deleted instance is reported by its key alone: what it was and who owned
	// it is what the projection row already holds. The instant is nova's own, to
	// the precision nova reported it in, which is the whole reason for this pass.
	assertObserved(t, observed,
		`type=instance id=3a9e5c07-2b81-4d6f-9a34-5c7e1b0d8f26 `+
			`project= state= size=none created=none deleted=2026-08-14T10:31:07Z UTC`,
		`type=instance id=b6d2f483-7e15-4a90-8c73-0d5b9a1e6c42 `+
			`project= state= size=none created=none deleted=2026-08-15T22:03:41.5Z UTC`)
}

func TestOpenStackBoundsHowFarBackItAsksNovaForDeletions(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, serversPath, "servers_empty.json")
	cloud.serveDeleted(t, "servers_deleted.json")
	writeCloudsYAML(t, cloud.URL)

	// A cloud whose last run completed a week ago, which is what one outage or
	// one type of correction the pipeline keeps refusing leaves behind: a failed
	// run does not move the bound.
	now := time.Date(2026, 8, 17, 9, 15, 0, 0, time.UTC)
	since := now.Add(-7 * 24 * time.Hour)
	floor := now.Add(-24 * time.Hour)

	logger, written := recordLogs()
	if _, errs := drain(t, adapters.NewOpenStack(func() time.Time { return now }, logger).
		ListResources(t.Context(), map[string]any{"os_cloud": testCloud}, &since)); len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	requests := cloud.requestsTo(serversPath)
	if len(requests) != 2 {
		t.Fatalf("the cloud answered %d requests for %s, want 2: the live listing and the deleted one",
			len(requests), serversPath)
	}
	// A week of a busy cloud's churn does not fit the budget one run has, and a
	// run that times out leaves the bound where it was, so every following run
	// asks for a longer window and less of it fits. The floor is what stops that
	// ratchet: the deletes it leaves out are booked by the absence pass at poll
	// time, which is what the concept accepts for every other resource type.
	//
	// It is the injected clock the floor is measured from rather than the clock of
	// whatever machine the run happens on, so the window a stale bound is clamped
	// to is a stated instant here rather than a tolerance around the wall clock.
	got := requests[1].query.Get("changes-since")
	if want := floor.Format(time.RFC3339); got != want {
		t.Errorf("the deleted listing asked for changes-since=%q, want %q", got, want)
	}

	// A completed run's row looks the same whether its delete corrections carry
	// the instants nova reported or the poll times the absence pass books, so the
	// one place a clamped window shows is the log. A host whose clock ran away
	// would otherwise ask nova for a window that has not happened yet, observe
	// nothing, complete, and move the bound past every delete it missed.
	if logged := written(); !strings.Contains(logged, floor.Format(time.RFC3339)) {
		t.Errorf("the run logged %q, want it to name the window it clamped to", logged)
	}
}

func TestOpenStackAsksForNoDeletesOnTheFirstRunOfACloud(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, serversPath, "servers_page1.json", "servers_page2.json")
	cloud.serveDeleted(t, "servers_deleted.json")
	writeCloudsYAML(t, cloud.URL)

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, nil))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	// A run without a bound has no window behind it to catch up on. Asking nova
	// for every instance it ever destroyed would be a listing that grows with the
	// age of the cloud, and one this run has no use for.
	for _, seen := range cloud.requests() {
		if seen.query.Get("deleted") == "true" {
			t.Errorf("the cloud was asked for %s?%s, want no deleted listing without a bound",
				seen.path, seen.query.Encode())
		}
	}
	for _, resource := range observed {
		if resource.DeletedAt != nil {
			t.Errorf("ListResources() observed %s, want only live resources",
				rendered(t, resource))
		}
	}
	if len(observed) != 3 {
		t.Errorf("ListResources() yielded %d observations, want the 3 live instances", len(observed))
	}
}

func TestOpenStackSkipsADeletedInstanceNovaDidNotDate(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, serversPath, "servers_empty.json")
	cloud.serveDeleted(t, "servers_deleted_undated.json")
	writeCloudsYAML(t, cloud.URL)

	since := time.Date(2026, 8, 1, 4, 30, 0, 0, time.UTC)
	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, &since))
	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}

	// Two of the three deleted instances carry no terminated_at: one reported as
	// null, one without the member at all. Neither is something this pass can
	// date, and the absence pass books both at poll time anyway, so only the
	// instance nova did date is observed here.
	assertObserved(t, observed,
		`type=instance id=5e2c8a41-3f79-4b05-9d68-1a4b7c2e0f93 `+
			`project= state= size=none created=none deleted=2026-08-16T04:12:59Z UTC`)
}

func TestOpenStackHoldsBackInstancesWhenTheDeletedListingFails(t *testing.T) {
	cloud := newCloud(t)
	cloud.serve(t, serversPath, "servers_page1.json", "servers_page2.json")
	// No deleted page is recorded, so that half of the instance listing fails
	// while the live half answered in full.
	cloud.serveDeleted(t)
	cloud.serve(t, volumesPath, "volumes.json")
	writeCloudsYAML(t, cloud.URL)

	since := time.Date(2026, 8, 1, 4, 30, 0, 0, time.UTC)
	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud}, &since))

	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1", len(errs))
	}
	// The instances are incomplete even though every live one was observed. A
	// delete this run could not read must not be booked at poll time by the
	// absence pass: that date can never be corrected, while the next run walks
	// the same window again and books nova's own instant.
	var enumErr *reconciliation.EnumerationError
	if !errors.As(errs[0], &enumErr) {
		t.Fatalf("ListResources() error = %q, want an EnumerationError", errs[0])
	}
	if enumErr.ResourceType != "instance" {
		t.Errorf("EnumerationError.ResourceType = %q, want %q", enumErr.ResourceType, "instance")
	}
	if requests := cloud.requestsTo(serversPath); len(requests) != 3 {
		t.Errorf("the cloud answered %d requests for %s, want 3: two live pages and the deleted one",
			len(requests), serversPath)
	}

	// The live instances stay observed and the types after them are enumerated as
	// if nothing had happened.
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
			`created=2026-07-21T08:03:44Z UTC deleted=none`,
		`type=volume id=5b8c2d17-6e94-4a30-b7f1-2c8d5e0a9b64 `+
			`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=in-use `+
			`size={"size_gb":100,"type":"ssd"} created=2026-06-02T08:15:00Z UTC deleted=none`,
		`type=volume id=c3f9a041-8b25-4d67-9e13-7a6c2b4d8e50 `+
			`project=8a1b7c6d5e4f40398271a6b5c4d3e2f1 state=error_deleting `+
			`size={"size_gb":25,"type":"hdd"} created=2026-06-11T21:03:44Z UTC deleted=none`)
}

// assertRunEnded holds a cancelled run's errors to the one error it may yield.
// A cancelled run says nothing about the type it was reading and nothing about
// the types after it: an EnumerationError would claim the rest of the cloud was
// enumerated, and the missed-delete pass would then run over what nobody looked
// at.
func assertRunEnded(t *testing.T, errs []error) {
	t.Helper()

	if len(errs) != 1 {
		t.Fatalf("ListResources() yielded %d errors, want 1", len(errs))
	}
	var enumErr *reconciliation.EnumerationError
	if errors.As(errs[0], &enumErr) {
		t.Errorf("ListResources() error = %q, want a plain error that ends the run", errs[0])
	}
	if !errors.Is(errs[0], context.Canceled) {
		t.Errorf("ListResources() error = %q, want it to carry the cancellation", errs[0])
	}
}

func TestOpenStackEndsTheStreamWhenTheRunIsCancelled(t *testing.T) {
	t.Run("the cancellation reaches a listing", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		cloud := newCloud(t)
		cloud.serve(t, serversPath, "servers_page1.json", "servers_page2.json")
		cloud.serve(t, volumesPath, "volumes.json")
		// The second page is the one the run is cancelled in. It is reachable only
		// through the first page's link, so its marker is what tells the two apart.
		cloud.cancelDuring(cancel, func(seen request) bool {
			return seen.path == serversPath && seen.query.Get("marker") != ""
		})
		writeCloudsYAML(t, cloud.URL)

		observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(ctx,
			map[string]any{"os_cloud": testCloud}, nil))

		assertRunEnded(t, errs)

		// What the first page reported stays reported, and no service after nova
		// is asked anything at all.
		assertObserved(t, observed,
			`type=instance id=7f3a1c58-9d2b-4e17-8c6a-2b5d0e9f4a31 `+
				`project=4c9d2f6b81e34a7f9b3c5d8e0a1f2b34 state=active `+
				`size={"disk_gb":30,"flavor":"m1.small","ram_gb":0.5,"vcpus":1} `+
				`created=2026-07-14T09:12:33Z UTC deleted=none`)
		if requests := cloud.requestsTo(volumesPath); len(requests) != 0 {
			t.Errorf("the cloud answered %d requests for %s, want none after the run ended",
				len(requests), volumesPath)
		}
	})

	t.Run("the cancellation reaches a flavor", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		cloud := newCloud(t)
		cloud.serve(t, serversPath, "servers_flavor_ids.json")
		cloud.serve(t, flavorsPath, "flavors.json")
		cloud.serve(t, volumesPath, "volumes.json")
		// A flavor fetch is the one request a listing makes in the middle of a
		// page it has already read. A failed one costs the instance its size, but
		// a cancelled one ends the run like any other.
		cloud.cancelDuring(cancel, func(seen request) bool { return seen.path == flavorsPath })
		writeCloudsYAML(t, cloud.URL)

		observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(ctx,
			map[string]any{"os_cloud": testCloud}, nil))

		assertRunEnded(t, errs)

		// The instance whose flavor was being read is not observed without one,
		// and the page it came from is not walked to its end either.
		assertObserved(t, observed)
		if requests := cloud.requestsTo(volumesPath); len(requests) != 0 {
			t.Errorf("the cloud answered %d requests for %s, want none after the run ended",
				len(requests), volumesPath)
		}
	})
}

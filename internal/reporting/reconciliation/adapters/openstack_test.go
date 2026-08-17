package adapters_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
// it and the helper substitutes it at serve time.
const endpointHost = "https://openstack.invalid"

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

// newCloud starts an OpenStack that authenticates. It answers the token request
// from the recorded fixture, leaving out the service types named in missing,
// and it answers the version documents the versionless catalog entries make
// gophercloud ask for.
func newCloud(t *testing.T, missing ...string) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	token := keystoneToken(t, server.URL, missing)
	mux.HandleFunc("POST /v3/auth/tokens", func(w http.ResponseWriter, _ *http.Request) {
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
		mux.HandleFunc("GET "+path+"{$}", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"versions": [{"id": %q, "status": "CURRENT"}]}`, version)
		})
	}

	return server
}

// writeCloudsYAML points the process at a clouds.yaml that authenticates
// against server. OS_CLIENT_CONFIG_FILE makes the written file the only search
// location, so the adapter runs its production lookup and still cannot reach a
// developer's real clouds.yaml.
//
// The other OS_* variables are emptied for the same reason: they override the
// file, and a shell that has them set from a real cloud would otherwise decide
// what the test authenticates as.
//
// Setting the environment is process-wide, which is why no test in this file
// runs in parallel.
func writeCloudsYAML(t *testing.T, server *httptest.Server) {
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
`, testCloud, server.URL, testRegion)
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
	writeCloudsYAML(t, server)

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
	writeCloudsYAML(t, newCloud(t))

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
	writeCloudsYAML(t, newCloud(t, "load-balancer"))

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
	writeCloudsYAML(t, newCloud(t))

	observed, errs := drain(t, adapters.NewOpenStack(time.Now, discardLogs).ListResources(t.Context(),
		map[string]any{"os_cloud": testCloud, "include_octavia": true}, nil))

	if len(errs) != 0 {
		t.Fatalf("ListResources() errors = %v, want none", errs)
	}
	if len(observed) != 0 {
		t.Errorf("ListResources() yielded %d observations, want 0", len(observed))
	}
}

package simulator

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/providers/openstack/osmap"
)

// The month every oracle built here is folded over. The instants the tests pick
// lie inside it, so what a listing answers is decided by the intervals alone.
var (
	cloudFrom = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cloudTo   = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
)

// cloudTenant is the project every resource of these oracles belongs to.
const cloudTenant = "8a1b7c6d5e4f40398271a6b5c4d3e2f1"

// The paths the listings are served at. They are the ones a client builds out
// of the catalog's endpoints, so a route that moved is a service the catalog
// points at nothing.
const (
	serversPath       = "/compute/v2.1/servers/detail"
	flavorsPath       = "/compute/v2.1/flavors/detail"
	volumesPath       = "/volume/v3/volumes/detail"
	floatingIPsPath   = "/network/v2.0/floatingips"
	imagesPath        = "/image/v2/images"
	loadBalancersPath = "/load-balancer/v2.0/lbaas/loadbalancers"
)

// cloudDay is midnight at the start of the nth day of the month.
func cloudDay(n int) time.Time {
	return cloudFrom.AddDate(0, 0, n-1)
}

// cloudServer serves one hand-built oracle with the virtual clock frozen at at,
// so that a listing is answered at the instant the test picked however long the
// test takes. It comes back with the token an authenticated caller holds.
func cloudServer(t *testing.T, at time.Time, resources ...OracleResource) (*httptest.Server, string) {
	t.Helper()

	handler, err := NewCloudAPI(NewClock(at, 0, time.Now), Oracle{
		Cloud: "os-sim", PeriodFrom: cloudFrom, PeriodTo: cloudTo, Resources: resources,
	})
	if err != nil {
		t.Fatalf("NewCloudAPI() error = %v, want nil", err)
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, tokenOf(t, server)
}

// oneResource is a resource that lived over a single interval. Everything a
// listing decides is decided per interval, so one is enough for most of these
// tests, and the sizes are the ones the generator's own builders write.
func oneResource(resourceType, id string, from, to time.Time, state string,
	size map[string]any,
) OracleResource {
	return OracleResource{
		ResourceType: resourceType,
		ResourceID:   id,
		Intervals: []OracleInterval{
			{From: from, To: to, State: state, ProjectID: cloudTenant, Size: size},
		},
	}
}

// oneInstance is one instance that ran over a single interval, on the flavor
// these tests read the dimensions of.
func oneInstance(id string, from, to time.Time) OracleResource {
	return oneResource(typeInstance, id, from, to, stateActive, instanceSizeOf(largeFlavor))
}

// resizedInstance is one instance that was resized inside the month: two
// adjacent intervals, on different flavors and in different states. Everything
// a document reports about a resource is read off one of its intervals, and
// which one that is is what a second interval states.
func resizedInstance(id string, from, resized, to time.Time) OracleResource {
	return OracleResource{
		ResourceType: typeInstance,
		ResourceID:   id,
		Intervals: []OracleInterval{
			{
				From: from, To: resized, State: stateActive, ProjectID: cloudTenant,
				Size: instanceSizeOf(flavors[0]),
			},
			{
				From: resized, To: to, State: stateShutoff, ProjectID: cloudTenant,
				Size: instanceSizeOf(largeFlavor),
			},
		},
	}
}

// authenticate asks the fake keystone for a token the way a client does.
func authenticate(t *testing.T, server *httptest.Server, name, password string,
) (int, http.Header, string) {
	t.Helper()

	return postToken(t, server, fmt.Sprintf(
		`{"auth": {"identity": {"methods": ["password"], "password": `+
			`{"user": {"name": %q, "password": %q, "domain": {"name": "Default"}}}}}}`, name, password))
}

// postToken sends one token request with the body it is given, for the cases
// that state a document rather than a pair of credentials.
func postToken(t *testing.T, server *httptest.Server, body string) (int, http.Header, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		server.URL+"/v3/auth/tokens", strings.NewReader(body))
	if err != nil {
		t.Fatalf("building the token request: %v", err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /v3/auth/tokens: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the answer to POST /v3/auth/tokens: %v", err)
	}
	return resp.StatusCode, resp.Header, string(answer)
}

// tokenOf authenticates as the account the simulator holds and returns the
// token the answer's header carries.
func tokenOf(t *testing.T, server *httptest.Server) string {
	t.Helper()

	status, header, body := authenticate(t, server, cloudUsername, cloudPassword)
	if status != http.StatusCreated {
		t.Fatalf("POST /v3/auth/tokens = %d, want %d (body %q)", status, http.StatusCreated, body)
	}
	token := header.Get("X-Subject-Token")
	if token == "" {
		t.Fatal("the answer carries no X-Subject-Token, which is where a client reads the token")
	}
	return token
}

// askCloud sends one request under the token a caller holds. An empty token
// sends the header not at all, which is what a client that never authenticated
// looks like.
func askCloud(t *testing.T, server *httptest.Server, token, path string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatalf("building GET %s: %v", path, err)
	}
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the answer to GET %s: %v", path, err)
	}
	return resp.StatusCode, string(answer)
}

// deletedSince is the request a run sends for the instances the cloud destroyed
// since the bound it carries.
func deletedSince(since time.Time) string {
	query := url.Values{
		"deleted":       {"true"},
		"changes-since": {since.UTC().Format(time.RFC3339)},
		"all_tenants":   {"true"},
	}
	return serversPath + "?" + query.Encode()
}

// servedDocuments are the resources one listing answered with.
func servedDocuments(t *testing.T, body, member string) []map[string]any {
	t.Helper()

	var answer map[string][]map[string]any
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	documents, ok := answer[member]
	if !ok {
		t.Fatalf("the answer %q states no %s", body, member)
	}
	return documents
}

// servedIDs are the ids one listing answered with, in the order they arrived.
func servedIDs(t *testing.T, body, member string) []string {
	t.Helper()

	var ids []string
	for _, document := range servedDocuments(t, body, member) {
		id, _ := document["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// oneDocument is the single resource a listing was expected to answer with.
func oneDocument(t *testing.T, body, member string) map[string]any {
	t.Helper()

	documents := servedDocuments(t, body, member)
	if len(documents) != 1 {
		t.Fatalf("the %s listing answered with %d documents, want 1 (body %q)",
			member, len(documents), body)
	}
	return documents[0]
}

// holdsMember holds one member of a served document against what the oracle
// states it should be. A decoded number arrives as a float64, so a count is
// written as one.
func holdsMember(t *testing.T, document map[string]any, member string, want any) {
	t.Helper()

	if got := document[member]; got != want {
		t.Errorf("the document holds %s = %#v, want %#v", member, got, want)
	}
}

// listedIn is how many entries one member of a document holds, which is what a
// load balancer's listeners and pools are read by.
func listedIn(t *testing.T, document map[string]any, member string) int {
	t.Helper()

	entries, ok := document[member].([]any)
	if !ok {
		t.Fatalf("the document holds %s = %#v, want a list", member, document[member])
	}
	return len(entries)
}

// servedListing is the single resource one listing answered with, which is what
// a size is read out of.
func servedListing(t *testing.T, server *httptest.Server, token, path, member string,
) map[string]any {
	t.Helper()

	status, body := askCloud(t, server, token, path)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d (body %q)", path, status, http.StatusOK, body)
	}
	return oneDocument(t, body, member)
}

func TestFakeAPIIssuesOneTokenPerRun(t *testing.T) {
	server, token := cloudServer(t, cloudDay(1))

	status, _, body := authenticate(t, server, cloudUsername, cloudPassword)
	if status != http.StatusCreated {
		t.Fatalf("POST /v3/auth/tokens = %d, want %d (body %q)", status, http.StatusCreated, body)
	}

	var answer struct {
		Token struct {
			Catalog []struct {
				Type      string `json:"type"`
				Endpoints []struct {
					Interface string `json:"interface"`
					Region    string `json:"region"`
					URL       string `json:"url"`
				} `json:"endpoints"`
			} `json:"catalog"`
		} `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}

	// The catalog is what a client addresses every later request from, and the
	// host it names is the one the request arrived on: the same handler serves a
	// container reaching it under one address and this test under another.
	want := map[string]string{
		"compute":       "/compute/v2.1",
		"block-storage": "/volume/v3",
		"network":       "/network",
		"image":         "/image",
		"load-balancer": "/load-balancer",
	}
	if len(answer.Token.Catalog) != len(want) {
		t.Fatalf("the catalog holds %d services, want %d", len(answer.Token.Catalog), len(want))
	}
	for _, published := range answer.Token.Catalog {
		path, ok := want[published.Type]
		if !ok {
			t.Errorf("the catalog publishes %q, which no client of this cloud asks for", published.Type)
			continue
		}
		if len(published.Endpoints) != 1 {
			t.Errorf("%s publishes %d endpoints, want 1", published.Type, len(published.Endpoints))
			continue
		}
		endpoint := published.Endpoints[0]
		if endpoint.URL != server.URL+path {
			t.Errorf("%s is published at %q, want %q", published.Type, endpoint.URL, server.URL+path)
		}
		if endpoint.Interface != "public" || endpoint.Region != cloudRegion {
			t.Errorf("%s is published as %q in %q, want %q in %q", published.Type,
				endpoint.Interface, endpoint.Region, "public", cloudRegion)
		}
	}

	// The token the header carried is the one the listings answer to, and a
	// second authentication hands out the same one: it belongs to the run.
	if again := tokenOf(t, server); again != token {
		t.Errorf("a second authentication was handed %q, want the run's one token %q", again, token)
	}
}

func TestFakeAPIRefusesACallerItDidNotIssueTo(t *testing.T) {
	server, token := cloudServer(t, cloudDay(1), oneInstance("web-01", cloudDay(1), cloudTo))

	t.Run("credentials that are not the simulator's", func(t *testing.T) {
		status, header, body := authenticate(t, server, cloudUsername, "hunter2")
		if status != http.StatusUnauthorized {
			t.Fatalf("POST /v3/auth/tokens = %d, want %d (body %q)",
				status, http.StatusUnauthorized, body)
		}
		want := `{"error": {"code": 401, "title": "Unauthorized", ` +
			`"message": "the credentials are not the simulator's"}}`
		if body != want {
			t.Errorf("POST /v3/auth/tokens body = %q, want %q", body, want)
		}
		if issued := header.Get("X-Subject-Token"); issued != "" {
			t.Errorf("the refusal carries the token %q, want none", issued)
		}
	})

	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "a caller that never authenticated", token: ""},
		{name: "a token from another run", token: "0e3f1c2a-4b56-4d78-9a01-2b3c4d5e6f70"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := askCloud(t, server, tc.token, serversPath)
			if status != http.StatusUnauthorized {
				t.Fatalf("GET %s = %d, want %d (body %q)",
					serversPath, status, http.StatusUnauthorized, body)
			}
			want := `{"error": {"code": 401, "title": "Unauthorized", ` +
				`"message": "the token is not the one this run issued"}}`
			if body != want {
				t.Errorf("GET %s body = %q, want %q", serversPath, body, want)
			}
		})
	}

	// The token this run did issue reaches the same listing.
	if status, body := askCloud(t, server, token, serversPath); status != http.StatusOK {
		t.Errorf("GET %s with the run's token = %d, want %d (body %q)",
			serversPath, status, http.StatusOK, body)
	}
}

// TestFakeAPIBoundsTheTokenRequestItReads holds the one route that answers
// before a credential is checked to a body it could have meant. What the
// document carries is a user name and a password; a decoder without a bound
// buffers whatever an unauthenticated caller sends before it reads either of
// them, and a simulator that runs out of memory over one request takes the
// month, the bus and the fake API down with it half way through a drill.
func TestFakeAPIBoundsTheTokenRequestItReads(t *testing.T) {
	server, _ := cloudServer(t, cloudDay(1))

	// The credentials in it are the simulator's own, so what refuses the request
	// is the bound rather than the password: the padding is what carries the
	// document past a kilobyte.
	body := fmt.Sprintf(`{"auth": {"identity": {"methods": ["password"], "password": `+
		`{"user": {"name": %q, "password": %q}}}}, "padding": %q}`,
		cloudUsername, cloudPassword, strings.Repeat("a", tokenBodyMax))

	status, header, answer := postToken(t, server, body)
	if status != http.StatusUnauthorized {
		t.Fatalf("POST /v3/auth/tokens with a body past the bound = %d, want %d (body %q)",
			status, http.StatusUnauthorized, answer)
	}
	if issued := header.Get("X-Subject-Token"); issued != "" {
		t.Errorf("the refusal carries the token %q, want none", issued)
	}
}

// TestFakeAPIAnswersNothingOnAPathNoRouteClaims holds the API to the routes it
// registered. A cloud that answered every path would let a client walk away
// believing it had read a service this simulator does not serve.
func TestFakeAPIAnswersNothingOnAPathNoRouteClaims(t *testing.T) {
	server, token := cloudServer(t, cloudDay(1))

	status, body := askCloud(t, server, token, "/v3/projects")
	if status != http.StatusNotFound {
		t.Errorf("GET /v3/projects = %d, want %d (body %q)", status, http.StatusNotFound, body)
	}
}

func TestFakeAPIPublishesTheVersionsAClientNegotiates(t *testing.T) {
	server, token := cloudServer(t, cloudDay(1))

	// Nova publishes the microversion range it speaks, and the reader negotiates
	// 2.47 out of it: that is the microversion from which a server carries its own
	// flavor, and a range that ended below it would send the reader to the flavor
	// catalog instead.
	status, body := askCloud(t, server, token, "/compute/v2.1/")
	if status != http.StatusOK {
		t.Fatalf("GET /compute/v2.1/ = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	var compute struct {
		Version struct {
			ID         string `json:"id"`
			Version    string `json:"version"`
			MinVersion string `json:"min_version"`
		} `json:"version"`
	}
	if err := json.Unmarshal([]byte(body), &compute); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	if compute.Version.ID != "v2.1" {
		t.Errorf("nova publishes the version %q, want %q", compute.Version.ID, "v2.1")
	}
	if compute.Version.Version != "2.96" || compute.Version.MinVersion != "2.1" {
		t.Errorf("nova publishes the microversions %q to %q, want a range that holds 2.47",
			compute.Version.MinVersion, compute.Version.Version)
	}

	// The other three are published without a version in their path, so a client
	// asks each of them which major versions it speaks before it accepts the
	// endpoint.
	for _, tc := range []struct {
		name, path, want string
	}{
		{name: "glance", path: "/image/", want: "v2.15"},
		{name: "neutron", path: "/network/", want: "v2.0"},
		{name: "octavia", path: "/load-balancer/", want: "v2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := askCloud(t, server, token, tc.path)
			if status != http.StatusOK {
				t.Fatalf("GET %s = %d, want %d (body %q)", tc.path, status, http.StatusOK, body)
			}
			var answer struct {
				Versions []struct {
					ID string `json:"id"`
				} `json:"versions"`
			}
			if err := json.Unmarshal([]byte(body), &answer); err != nil {
				t.Fatalf("decoding %q: %v", body, err)
			}
			if len(answer.Versions) != 1 || answer.Versions[0].ID != tc.want {
				t.Errorf("GET %s published %v, want the single version %q",
					tc.path, answer.Versions, tc.want)
			}
		})
	}
}

// TestFakeAPIServesTheInventoryOfTheInstant is the whole of what a listing
// means: the cloud holds what the oracle says it held at the instant the clock
// stands at, and nothing that was already gone or has not happened yet.
func TestFakeAPIServesTheInventoryOfTheInstant(t *testing.T) {
	server, token := cloudServer(t, cloudDay(10),
		oneInstance("running", cloudDay(2), cloudTo),
		oneInstance("gone", cloudDay(2), cloudDay(5)),
		oneInstance("later", cloudDay(20), cloudTo))

	status, body := askCloud(t, server, token, serversPath)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d (body %q)", serversPath, status, http.StatusOK, body)
	}
	if got := servedIDs(t, body, "servers"); !slices.Equal(got, []string{"running"}) {
		t.Errorf("the live listing answered with %v, want the one instance that exists at day 10", got)
	}

	// The one whose last interval ended is deleted, and the one that has not been
	// created yet is in neither listing: an instance served early would be booked
	// as a resource the collector never recorded a create for.
	status, body = askCloud(t, server, token, deletedSince(cloudDay(1)))
	if status != http.StatusOK {
		t.Fatalf("GET the deleted listing = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if got := servedIDs(t, body, "servers"); !slices.Equal(got, []string{"gone"}) {
		t.Errorf("the deleted listing answered with %v, want the one instance deleted by day 10", got)
	}
}

// TestFakeAPIServesTheIntervalAResourceIsInAndDatesItByItsFirst covers the
// resource that lived through more than one interval, which is what a resize
// leaves behind. What a listing reports is read off the interval that holds the
// instant, while the created instant is read off the first one: a server that
// reported its resize as its creation would be booked as created twice.
func TestFakeAPIServesTheIntervalAResourceIsInAndDatesItByItsFirst(t *testing.T) {
	t.Run("the live listing", func(t *testing.T) {
		server, token := cloudServer(t, cloudDay(10),
			resizedInstance("resized", cloudDay(2), cloudDay(6), cloudTo))

		document := servedListing(t, server, token, serversPath, "servers")
		holdsMember(t, document, "id", "resized")
		// The state and the flavor are the ones of the interval day 10 is in, and
		// the instant the server was last changed at is where that interval began.
		holdsMember(t, document, "OS-EXT-STS:vm_state", "stopped")
		holdsMember(t, document, "updated", cloudDay(6).Format(time.RFC3339))
		embedded, ok := document["flavor"].(map[string]any)
		if !ok {
			t.Fatalf("the server holds flavor = %#v, want the embedded object", document["flavor"])
		}
		holdsMember(t, embedded, "original_name", largeFlavor.name)

		// The two instants the create is dated by are the first interval's, whatever
		// the server was resized to since.
		holdsMember(t, document, "created", cloudDay(2).Format(time.RFC3339))
		holdsMember(t, document, "OS-SRV-USG:launched_at", cloudDay(2).Format(fractionalLayout))
	})

	t.Run("the deleted listing", func(t *testing.T) {
		server, token := cloudServer(t, cloudDay(10),
			resizedInstance("gone", cloudDay(2), cloudDay(6), cloudDay(7)))

		status, body := askCloud(t, server, token, deletedSince(cloudDay(1)))
		if status != http.StatusOK {
			t.Fatalf("GET the deleted listing = %d, want %d (body %q)", status, http.StatusOK, body)
		}

		// What nova reports about a destroyed server is the server it was when it
		// was destroyed: the last interval it had, dated by its first one.
		document := oneDocument(t, body, "servers")
		holdsMember(t, document, "id", "gone")
		holdsMember(t, document, "updated", cloudDay(6).Format(time.RFC3339))
		holdsMember(t, document, "created", cloudDay(2).Format(time.RFC3339))
		holdsMember(t, document, "OS-SRV-USG:terminated_at", cloudDay(7).Format(fractionalLayout))
		embedded, ok := document["flavor"].(map[string]any)
		if !ok {
			t.Fatalf("the server holds flavor = %#v, want the embedded object", document["flavor"])
		}
		holdsMember(t, embedded, "original_name", largeFlavor.name)
	})
}

// TestFakeAPIServesTheInstancesDeletedInTheWindow holds the deleted listing to
// the window it was asked about. A run books a delete at the instant the
// platform performed it, and the bound it carries is the last run it completed:
// an instance outside that window is one another run already reported, or one
// this run has no business knowing about yet.
func TestFakeAPIServesTheInstancesDeletedInTheWindow(t *testing.T) {
	server, token := cloudServer(t, cloudDay(10),
		oneInstance("before-the-bound", cloudDay(1), cloudDay(3)),
		oneInstance("at-the-bound", cloudDay(1), cloudDay(5)),
		oneInstance("inside", cloudDay(1), cloudDay(7)),
		oneInstance("after-the-instant", cloudDay(1), cloudDay(20)))

	status, body := askCloud(t, server, token, deletedSince(cloudDay(5)))
	if status != http.StatusOK {
		t.Fatalf("GET the deleted listing = %d, want %d (body %q)", status, http.StatusOK, body)
	}

	document := oneDocument(t, body, "servers")
	holdsMember(t, document, "id", "inside")
	holdsMember(t, document, "status", "DELETED")
	holdsMember(t, document, "OS-SRV-USG:terminated_at", cloudDay(7).Format(fractionalLayout))

	// The instance whose interval runs past the instant the listing is answered at
	// is not deleted at all yet, and the live listing is where it belongs.
	status, body = askCloud(t, server, token, serversPath)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d (body %q)", serversPath, status, http.StatusOK, body)
	}
	if got := servedIDs(t, body, "servers"); !slices.Equal(got, []string{"after-the-instant"}) {
		t.Errorf("the live listing answered with %v, want the instance that is still running", got)
	}
}

func TestFakeAPIRefusesAWindowItCannotRead(t *testing.T) {
	server, token := cloudServer(t, cloudDay(10), oneInstance("web-01", cloudDay(1), cloudDay(5)))

	path := serversPath + "?deleted=true&changes-since=yesterday"
	status, body := askCloud(t, server, token, path)
	if status != http.StatusBadRequest {
		t.Fatalf("GET %s = %d, want %d (body %q)", path, status, http.StatusBadRequest, body)
	}
	if body != badChangesSince {
		t.Errorf("GET %s body = %q, want %q", path, body, badChangesSince)
	}
}

// TestFakeAPIHoldsTheInventoryAtTheEndOfTheMonth covers the clock that has run
// past the month. Every interval of an oracle ends inside the period, so a
// listing answered at the clock's own instant would report a cloud that lost
// everything the moment the month ended, and a sync run after a simulated month
// is exactly what the drill does.
func TestFakeAPIHoldsTheInventoryAtTheEndOfTheMonth(t *testing.T) {
	server, token := cloudServer(t, cloudTo.Add(72*time.Hour),
		oneInstance("outlived-the-month", cloudDay(2), cloudTo),
		oneInstance("deleted-inside-it", cloudDay(2), cloudDay(5)))

	status, body := askCloud(t, server, token, serversPath)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d (body %q)", serversPath, status, http.StatusOK, body)
	}
	if got := servedIDs(t, body, "servers"); !slices.Equal(got, []string{"outlived-the-month"}) {
		t.Errorf("the live listing answered with %v, want the instance that outlived the month", got)
	}

	status, body = askCloud(t, server, token, deletedSince(cloudFrom))
	if status != http.StatusOK {
		t.Fatalf("GET the deleted listing = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	document := oneDocument(t, body, "servers")
	holdsMember(t, document, "id", "deleted-inside-it")
	holdsMember(t, document, "OS-SRV-USG:terminated_at", cloudDay(5).Format(fractionalLayout))
}

// TestFakeAPIServesTheSizesTheOracleStates holds every listing to the size its
// service reports the resource by. The conversions are what a reader inverts,
// so an image served a quarter gibibyte short would be booked as resized on
// every sync of the month.
func TestFakeAPIServesTheSizesTheOracleStates(t *testing.T) {
	server, token := cloudServer(t, cloudDay(10),
		oneInstance("server-01", cloudDay(2), cloudTo),
		oneResource(typeVolume, "volume-01", cloudDay(2), cloudTo, stateInUse,
			volumeSizeOf(&volume{sizeGB: 100, volumeType: "ssd"})),
		oneResource(typeFloatingIP, "address-01", cloudDay(2), cloudTo, stateActive,
			floatingIPSizeOf()),
		oneResource(typeImage, "image-01", cloudDay(2), cloudTo, stateActive,
			imageSizeOf(&image{size: quarterGiB})),
		oneResource(typeLoadBalancer, "balancer-01", cloudDay(2), cloudTo, stateActive,
			loadBalancerSizeOf(2, 1)))

	t.Run("the flavor a server carries", func(t *testing.T) {
		document := servedListing(t, server, token, serversPath, "servers")
		holdsMember(t, document, "tenant_id", cloudTenant)
		holdsMember(t, document, "OS-EXT-STS:vm_state", "active")
		holdsMember(t, document, "created", cloudDay(2).Format(time.RFC3339))

		// From microversion 2.47 nova reports the flavor out of the instance's own
		// record, in the dimensions the reader converts and sums itself.
		embedded, ok := document["flavor"].(map[string]any)
		if !ok {
			t.Fatalf("the server holds flavor = %#v, want the embedded object", document["flavor"])
		}
		holdsMember(t, embedded, "original_name", largeFlavor.name)
		holdsMember(t, embedded, "vcpus", float64(largeFlavor.vcpus))
		holdsMember(t, embedded, "ram", float64(largeFlavor.memoryMB))
		holdsMember(t, embedded, "disk", float64(largeFlavor.rootGB))
		holdsMember(t, embedded, "ephemeral", float64(largeFlavor.ephemeralGB))
	})

	t.Run("the size and the type of a volume", func(t *testing.T) {
		document := servedListing(t, server, token, volumesPath, "volumes")
		holdsMember(t, document, "size", float64(100))
		holdsMember(t, document, "volume_type", "ssd")
		holdsMember(t, document, "status", stateInUse)
		holdsMember(t, document, "os-vol-tenant-attr:tenant_id", cloudTenant)
		holdsMember(t, document, "created_at", cloudDay(2).Format(fractionalLayout))
	})

	t.Run("the protocol an address is one of", func(t *testing.T) {
		document := servedListing(t, server, token, floatingIPsPath, "floatingips")
		// The oracle keeps no addresses: what an address is billed by is which
		// protocol it is an address of, and every simulated one is IPv4.
		holdsMember(t, document, "floating_ip_address", "203.0.113.1")
		holdsMember(t, document, "status", "ACTIVE")
		holdsMember(t, document, "project_id", cloudTenant)
	})

	t.Run("the bytes an image occupies", func(t *testing.T) {
		document := servedListing(t, server, token, imagesPath, "images")
		// A quarter gibibyte is what the size states as 0.25, and glance reports
		// the bytes it came from. The inversion is exact.
		holdsMember(t, document, "size", float64(quarterGiB))
		holdsMember(t, document, "status", "active")
		holdsMember(t, document, "owner", cloudTenant)
	})

	t.Run("the listeners and pools a load balancer holds", func(t *testing.T) {
		document := servedListing(t, server, token, loadBalancersPath, "loadbalancers")
		if got := listedIn(t, document, "listeners"); got != 2 {
			t.Errorf("the balancer holds %d listeners, want the 2 its size counts", got)
		}
		if got := listedIn(t, document, "pools"); got != 1 {
			t.Errorf("the balancer holds %d pools, want the 1 its size counts", got)
		}
		holdsMember(t, document, "provisioning_status", "ACTIVE")
		holdsMember(t, document, "project_id", cloudTenant)
	})
}

// TestFakeAPIPublishesTheFlavorCatalogOfTheWorld covers the listing a reader
// falls back to where it could not negotiate the microversion that embeds a
// server's flavor. A flavor the catalog leaves out costs every instance running
// on it its size.
func TestFakeAPIPublishesTheFlavorCatalogOfTheWorld(t *testing.T) {
	server, token := cloudServer(t, cloudDay(1))

	status, body := askCloud(t, server, token, flavorsPath)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d (body %q)", flavorsPath, status, http.StatusOK, body)
	}

	published := map[string]map[string]any{}
	for _, document := range servedDocuments(t, body, "flavors") {
		name, _ := document["name"].(string)
		published[name] = document
	}
	for _, held := range append(slices.Clone(flavors), bootVolumeFlavor) {
		document, ok := published[held.name]
		if !ok {
			t.Errorf("the catalog holds no %s, which instances of the month run on", held.name)
			continue
		}
		holdsMember(t, document, "id", held.flavorID)
		holdsMember(t, document, "vcpus", float64(held.vcpus))
		holdsMember(t, document, "ram", float64(held.memoryMB))
		holdsMember(t, document, "disk", float64(held.rootGB))
		holdsMember(t, document, "OS-FLV-EXT-DATA:ephemeral", float64(held.ephemeralGB))
	}
}

func TestNewCloudAPIRefusesAFlavorTheWorldDoesNotHold(t *testing.T) {
	size := instanceSizeOf(largeFlavor)
	size["flavor"] = "m1.nano"

	_, err := NewCloudAPI(NewClock(cloudFrom, 0, time.Now), Oracle{
		Cloud: "os-sim", PeriodFrom: cloudFrom, PeriodTo: cloudTo,
		Resources: []OracleResource{
			oneResource(typeInstance, "web-01", cloudDay(1), cloudTo, stateActive, size),
		},
	})
	if err == nil {
		t.Fatal("NewCloudAPI() error = nil, want the flavor refused: a server served without " +
			"its dimensions is observed sizeless")
	}
	want := `the oracle names the flavor "m1.nano" that the world does not hold`
	if err.Error() != want {
		t.Errorf("NewCloudAPI() error = %q, want %q", err, want)
	}
}

// TestFakeAPINamesTheStatesTheMappingNormalizes holds the reverse table against
// the one the collector and the reconciliation adapter both read. The fake
// serves the raw vm_state nova reports, the oracle states what the mapping
// books, and a row changed on one side alone would have the simulated cloud
// report drift on every instance it touched.
func TestFakeAPINamesTheStatesTheMappingNormalizes(t *testing.T) {
	for booked, raw := range novaVMStates {
		if got := osmap.VMState(raw); got != booked {
			t.Errorf("the fake serves %q for an instance the oracle books as %q, "+
				"and osmap.VMState(%q) = %q", raw, booked, raw, got)
		}
	}
}

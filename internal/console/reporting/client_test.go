package reporting

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testToken is the credential every request under test has to carry.
const testToken = "s3cr3t-token"

// testProjectID is the project the project-scoped reads address. Its literal
// form is what the expected paths are written with.
var testProjectID = uuid.MustParse("11111111-1111-4111-8111-111111111111")

// The window the two windowed routes are asked for. RFC 3339 escapes to
// %3A in a query, which is the form the API is actually asked in.
var (
	windowFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	windowTo   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
)

// The smallest valid answers of the routes under test. A paged list carries a
// cursor member, an unpaged one does not.
const (
	emptyPage = `{"items":[],"next_cursor":null}`
	emptyList = `{"items":[]}`

	projectBody = `{"id":"11111111-1111-4111-8111-111111111111","cloud":"os-sim","external_id":"p-1",` +
		`"platform":"openstack","name":null,"metadata":{},"created_at":"2026-03-01T00:00:00Z"}`

	resourceBody = `{"cloud":"os-sim","platform":"openstack","project_id":"p-1","resource_id":"vm/1",` +
		`"resource_type":"instance","state":"active","size":{},"created_at":"2026-03-01T00:00:00Z",` +
		`"deleted_at":null,"first_event_at":"2026-03-01T00:00:00Z",` +
		`"last_event_at":"2026-03-01T00:00:00Z","last_event_type":"instance.create",` +
		`"last_payload":null}`

	lifecycleBody = `{"resource":` + resourceBody + `,"events":[],"intervals":[],"warnings":[]}`

	summaryBody = `{"project":{"id":"11111111-1111-4111-8111-111111111111","cloud":"os-sim",` +
		`"external_id":"p-1"},"resource_types":[]}`
)

// call is one request a stub server saw.
type call struct {
	method string
	uri    string
	auth   string
}

// stubServer answers every request with body and appends what it was asked to
// calls. The recorder is read after the client call returned, which is after
// the handler that fills it has run.
func stubServer(t *testing.T, calls *[]call, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, call{
			method: r.Method,
			uri:    r.URL.RequestURI(),
			auth:   r.Header.Get("Authorization"),
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// answering builds a server that answers whatever it is asked with one status
// and one body.
func answering(t *testing.T, status int, contentType, body string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// testClient builds a client against a stub server. The stub speaks plain
// HTTP, so no CA file is involved.
func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()

	api, err := New(baseURL, testToken, "")
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	return api
}

// TestReadsCarryTheTokenAndTheQuery holds every read to the route it is
// documented for. The path segments, the filters, and their encoding are what
// decides whether the API answers the question a page asked, and the token has
// to be on all ten of them.
func TestReadsCarryTheTokenAndTheQuery(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantURI string
		call    func(context.Context, *Client) (Request, error)
	}{
		{
			name:    "a filtered project list",
			body:    emptyPage,
			wantURI: "/api/v1/projects?cloud=os-sim&cursor=abc&external_id=p-1&platform=openstack",
			call: func(ctx context.Context, c *Client) (Request, error) {
				_, request, err := c.ListProjects(ctx, ProjectsQuery{
					Platform:   "openstack",
					Cloud:      "os-sim",
					ExternalID: "p-1",
					Cursor:     "abc",
				})
				return request, err
			},
		},
		{
			name:    "one project",
			body:    projectBody,
			wantURI: "/api/v1/projects/11111111-1111-4111-8111-111111111111",
			call: func(ctx context.Context, c *Client) (Request, error) {
				_, request, err := c.GetProject(ctx, testProjectID)
				return request, err
			},
		},
		{
			name:    "the relations of one project",
			body:    emptyList,
			wantURI: "/api/v1/projects/11111111-1111-4111-8111-111111111111/relations",
			call: func(ctx context.Context, c *Client) (Request, error) {
				_, request, err := c.ListProjectRelations(ctx, testProjectID)
				return request, err
			},
		},
		{
			name:    "the projects one traversal reaches",
			body:    emptyList,
			wantURI: "/api/v1/projects/11111111-1111-4111-8111-111111111111/related",
			call: func(ctx context.Context, c *Client) (Request, error) {
				_, request, err := c.ListRelatedProjects(ctx, testProjectID)
				return request, err
			},
		},
		{
			name: "one project's summary",
			body: summaryBody,
			wantURI: "/api/v1/projects/11111111-1111-4111-8111-111111111111/summary" +
				"?from=2026-03-01T00%3A00%3A00Z&to=2026-04-01T00%3A00%3A00Z",
			call: func(ctx context.Context, c *Client) (Request, error) {
				_, request, err := c.GetProjectSummary(ctx, testProjectID, windowFrom, windowTo)
				return request, err
			},
		},
		{
			name: "a filtered resource list",
			body: emptyPage,
			wantURI: "/api/v1/resources?cloud=os-sim&cursor=abc&limit=50&project_id=p-1" +
				"&resource_type=instance&state=active&status=all",
			call: func(ctx context.Context, c *Client) (Request, error) {
				_, request, err := c.ListResources(ctx, ResourcesQuery{
					Cloud:        "os-sim",
					ProjectID:    "p-1",
					ResourceType: "instance",
					State:        "active",
					Status:       "all",
					Cursor:       "abc",
					Limit:        50,
				})
				return request, err
			},
		},
		{
			// A cloud and a resource id carrying a slash address one resource
			// each. Unescaped they would address a route that does not exist.
			name:    "one lifecycle addressed by slashed segments",
			body:    lifecycleBody,
			wantURI: "/api/v1/resources/os%2Fsim/instance/vm%2F1/lifecycle",
			call: func(ctx context.Context, c *Client) (Request, error) {
				_, request, err := c.GetLifecycle(ctx, "os/sim", "instance", "vm/1")
				return request, err
			},
		},
		{
			// No at= travels: the API answers 501 to a grouping asked for at an
			// instant, so a page asking for one would show an error instead of
			// counts.
			name:    "the resource counts of one grouping",
			body:    emptyList,
			wantURI: "/api/v1/stats/resources?group_by=cloud%2Cresource_type%2Cstate&status=all",
			call: func(ctx context.Context, c *Client) (Request, error) {
				_, request, err := c.ResourceStats(ctx, []string{"cloud", "resource_type", "state"}, "all")
				return request, err
			},
		},
		{
			name: "the event counts of one window",
			body: emptyList,
			wantURI: "/api/v1/stats/events?from=2026-03-01T00%3A00%3A00Z&group_by=cloud%2Cevent_type" +
				"&interval=1h&to=2026-04-01T00%3A00%3A00Z",
			call: func(ctx context.Context, c *Client) (Request, error) {
				_, request, err := c.EventStats(ctx, []string{"cloud", "event_type"}, "1h", windowFrom, windowTo)
				return request, err
			},
		},
		{
			name:    "the refused ingest items",
			body:    emptyPage,
			wantURI: "/api/v1/rejected-events?limit=5",
			call: func(ctx context.Context, c *Client) (Request, error) {
				_, request, err := c.ListRejectedEvents(ctx, 5)
				return request, err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls []call
			server := stubServer(t, &calls, test.body)

			request, err := test.call(context.Background(), testClient(t, server.URL))
			if err != nil {
				t.Fatalf("the call returned error = %v, want nil", err)
			}
			if len(calls) != 1 {
				t.Fatalf("the stub served %d requests, want 1", len(calls))
			}

			if calls[0].method != http.MethodGet {
				t.Errorf("the stub was asked with %s, want GET", calls[0].method)
			}
			if calls[0].uri != test.wantURI {
				t.Errorf("the stub was asked for %q, want %q", calls[0].uri, test.wantURI)
			}
			if want := "Bearer " + testToken; calls[0].auth != want {
				t.Errorf("the request carried Authorization %q, want %q", calls[0].auth, want)
			}
			// The reported request is what a page shows as its provenance, so
			// it has to be the call that was actually made rather than a
			// reconstruction of it.
			if request.Method != calls[0].method || request.Path != calls[0].uri {
				t.Errorf("Request = %+v, want the method and path the stub saw: %s %s",
					request, calls[0].method, calls[0].uri)
			}
		})
	}
}

// TestListProjectsReturnsOnePage holds the client to one call per page. A
// client that followed the cursor itself would read the whole registry for a
// page showing twenty rows.
func TestListProjectsReturnsOnePage(t *testing.T) {
	var calls []call
	server := stubServer(t, &calls, `{"items":[`+projectBody+`],"next_cursor":"abc"}`)

	page, _, err := testClient(t, server.URL).ListProjects(context.Background(), ProjectsQuery{})
	if err != nil {
		t.Fatalf("ListProjects() error = %v, want nil", err)
	}

	if len(page.Items) != 1 {
		t.Fatalf("ListProjects() returned %d projects, want 1", len(page.Items))
	}
	if page.NextCursor == nil || *page.NextCursor != "abc" {
		t.Errorf("ListProjects() returned next cursor %v, want abc", page.NextCursor)
	}
	if len(calls) != 1 {
		t.Errorf("the stub served %d requests, want 1: the cursor is returned, not followed", len(calls))
	}
}

// TestListProjectsRefusesARepeatedCursor stops a walk the API cannot end. A
// page naming the cursor it was given sends the reader clicking "next" onto the
// same rows forever.
func TestListProjectsRefusesARepeatedCursor(t *testing.T) {
	var calls []call
	server := stubServer(t, &calls, `{"items":[`+projectBody+`],"next_cursor":"abc"}`)

	page, _, err := testClient(t, server.URL).ListProjects(context.Background(), ProjectsQuery{Cursor: "abc"})
	if err == nil {
		t.Fatalf("ListProjects() = %d projects, want an error", len(page.Items))
	}
	for _, want := range []string{`"abc"`, "page forever"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ListProjects() error = %v, want it to name %s", err, want)
		}
	}
}

// TestListResourcesRefusesARepeatedCursor is the resource list's half of the
// same guard.
func TestListResourcesRefusesARepeatedCursor(t *testing.T) {
	var calls []call
	server := stubServer(t, &calls, `{"items":[`+resourceBody+`],"next_cursor":"abc"}`)

	page, _, err := testClient(t, server.URL).ListResources(context.Background(), ResourcesQuery{Cursor: "abc"})
	if err == nil {
		t.Fatalf("ListResources() = %d resources, want an error", len(page.Items))
	}
	for _, want := range []string{`"abc"`, "page forever"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ListResources() error = %v, want it to name %s", err, want)
		}
	}
}

// TestEmptyPagesDecode keeps an empty registry apart from a failed read. A page
// with no rows is the answer a fresh installation serves, and it has to reach
// the console as an empty list rather than as an error.
func TestEmptyPagesDecode(t *testing.T) {
	t.Run("no projects", func(t *testing.T) {
		var calls []call
		server := stubServer(t, &calls, emptyPage)

		page, _, err := testClient(t, server.URL).ListProjects(context.Background(), ProjectsQuery{})
		if err != nil {
			t.Fatalf("ListProjects() error = %v, want nil", err)
		}
		if len(page.Items) != 0 {
			t.Errorf("ListProjects() returned %d projects, want none", len(page.Items))
		}
		if page.NextCursor != nil {
			t.Errorf("ListProjects() returned next cursor %q, want none", *page.NextCursor)
		}
	})

	t.Run("no resources", func(t *testing.T) {
		var calls []call
		server := stubServer(t, &calls, emptyPage)

		page, _, err := testClient(t, server.URL).ListResources(context.Background(), ResourcesQuery{})
		if err != nil {
			t.Fatalf("ListResources() error = %v, want nil", err)
		}
		if len(page.Items) != 0 {
			t.Errorf("ListResources() returned %d resources, want none", len(page.Items))
		}
		if page.NextCursor != nil {
			t.Errorf("ListResources() returned next cursor %q, want none", *page.NextCursor)
		}
	})
}

// TestProblemsBecomeTypedErrors keeps the API's own diagnosis reachable. A page
// tells a project that is not there from a token that may not read it by the
// status and the problem type, which it can only do while both survive the
// error.
func TestProblemsBecomeTypedErrors(t *testing.T) {
	t.Run("a project that is not there", func(t *testing.T) {
		server := answering(t, http.StatusNotFound, "application/problem+json",
			`{"type":"urn:tally:error:not-found","title":"Not found","detail":"no such project","status":404}`)

		_, _, err := testClient(t, server.URL).GetProject(context.Background(), testProjectID)

		var problem *ProblemError
		if !errors.As(err, &problem) {
			t.Fatalf("GetProject() error = %v, want a *ProblemError", err)
		}
		if problem.Status != http.StatusNotFound {
			t.Errorf("Status = %d, want 404", problem.Status)
		}
		if problem.Type != "urn:tally:error:not-found" {
			t.Errorf("Type = %q, want urn:tally:error:not-found", problem.Type)
		}
		if problem.Title != "Not found" {
			t.Errorf("Title = %q, want Not found", problem.Title)
		}
		if problem.Detail != "no such project" {
			t.Errorf("Detail = %q, want no such project", problem.Detail)
		}
		for _, want := range []string{"404", "urn:tally:error:not-found", "Not found", "no such project"} {
			if !strings.Contains(problem.Error(), want) {
				t.Errorf("Error() = %q, want it to name %s", problem.Error(), want)
			}
		}
	})

	t.Run("a token the API does not know", func(t *testing.T) {
		server := answering(t, http.StatusUnauthorized, "application/problem+json",
			`{"type":"urn:tally:error:unauthorized","title":"Unauthorized","status":401}`)

		_, _, err := testClient(t, server.URL).ListProjects(context.Background(), ProjectsQuery{})

		var problem *ProblemError
		if !errors.As(err, &problem) {
			t.Fatalf("ListProjects() error = %v, want a *ProblemError", err)
		}
		if problem.Status != http.StatusUnauthorized {
			t.Errorf("Status = %d, want 401", problem.Status)
		}
		// A problem document without a detail reports the title alone rather
		// than a dangling colon.
		if want := "the API answered 401 urn:tally:error:unauthorized: Unauthorized"; problem.Error() != want {
			t.Errorf("Error() = %q, want %q", problem.Error(), want)
		}
	})

	t.Run("a gateway answering in HTML", func(t *testing.T) {
		server := answering(t, http.StatusInternalServerError, "text/html",
			"<html><body>500 Internal Server Error</body></html>")

		_, _, err := testClient(t, server.URL).ListResources(context.Background(), ResourcesQuery{})
		if err == nil {
			t.Fatalf("ListResources() error = nil, want an error")
		}
		for _, want := range []string{"500", "unreadable problem document"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ListResources() error = %v, want it to name %s", err, want)
			}
		}
		// Nothing about the answer was the API's diagnosis, so reporting it as
		// one would put an empty title on the page.
		var problem *ProblemError
		if errors.As(err, &problem) {
			t.Errorf("ListResources() error = %v, want it not to pass as a problem document", err)
		}
	})
}

// TestTransportErrorsNameTheEndpoint keeps an unreachable API apart from one
// that answered. Only the wrapped error says which host and which route failed,
// which is what tells a misconfigured URL from a route the API refuses.
func TestTransportErrorsNameTheEndpoint(t *testing.T) {
	var calls []call
	server := stubServer(t, &calls, emptyPage)
	api := testClient(t, server.URL)
	server.Close()

	_, _, err := api.ListResources(context.Background(), ResourcesQuery{})
	if err == nil {
		t.Fatalf("ListResources() error = nil, want an error")
	}
	for _, want := range []string{"calling http://127.0.0.1:", "/api/v1/resources"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ListResources() error = %v, want it to name %s", err, want)
		}
	}
}

// TestRedirectsAreNotFollowed keeps the token from travelling somewhere the
// operator did not name. Go strips the Authorization header only across hosts,
// so a redirect from https to http on the same host would carry the credential
// over in the clear; the answer is reported instead of followed.
func TestRedirectsAreNotFollowed(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect was followed, carrying Authorization %q", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, emptyPage)
	}))
	t.Cleanup(target.Close)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v1/projects", http.StatusFound)
	}))
	t.Cleanup(server.Close)

	_, _, err := testClient(t, server.URL).ListProjects(context.Background(), ProjectsQuery{})
	if err == nil {
		t.Fatalf("ListProjects() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "302") {
		t.Errorf("ListProjects() error = %v, want it to report the redirect's status", err)
	}
}

// TestNewTrimsOneTrailingSlash accepts the base URL in both forms an operator
// writes it. Without the trim the routes would be asked for under a doubled
// slash, which the API does not route.
func TestNewTrimsOneTrailingSlash(t *testing.T) {
	var calls []call
	server := stubServer(t, &calls, emptyPage)

	api, err := New(server.URL+"/", testToken, "")
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if _, _, err := api.ListRejectedEvents(context.Background(), 0); err != nil {
		t.Fatalf("ListRejectedEvents() error = %v, want nil", err)
	}

	if len(calls) != 1 {
		t.Fatalf("the stub served %d requests, want 1", len(calls))
	}
	// A limit of zero is left out, so the bare route is what is asked for.
	if want := "/api/v1/rejected-events"; calls[0].uri != want {
		t.Errorf("the stub was asked for %q, want %q", calls[0].uri, want)
	}
}

// writeCA writes one certificate as a PEM file and returns its path.
func writeCA(t *testing.T, der []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "ca.crt")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestNewRejectsACAFileWithoutPEM refuses a trust store that would trust
// nothing. Without the check every call would fail with a verification error
// that says nothing about the file behind it.
func TestNewRejectsACAFileWithoutPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.crt")
	if err := os.WriteFile(path, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := New("https://api.example", testToken, path)
	if err == nil {
		t.Fatalf("New() error = nil, want an error")
	}
	for _, want := range []string{path, "no PEM certificates"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("New() error = %v, want it to name %s", err, want)
		}
	}
}

// TestNewTrustsTheCAFile is what reaches a dev cluster whose certificates come
// from a CA the host does not know. The second case is the control: the same
// server without the file is refused, so the file is what made the first call
// work.
func TestNewTrustsTheCAFile(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, emptyPage)
	}))
	t.Cleanup(server.Close)

	t.Run("with the CA file", func(t *testing.T) {
		api, err := New(server.URL, testToken, writeCA(t, server.Certificate().Raw))
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		if _, _, err := api.ListRejectedEvents(context.Background(), 0); err != nil {
			t.Fatalf("ListRejectedEvents() error = %v, want nil", err)
		}
	})

	t.Run("without the CA file", func(t *testing.T) {
		api, err := New(server.URL, testToken, "")
		if err != nil {
			t.Fatalf("New() error = %v, want nil", err)
		}
		_, _, err = api.ListRejectedEvents(context.Background(), 0)
		if err == nil {
			t.Fatalf("ListRejectedEvents() error = nil, want a verification error")
		}
		if !strings.Contains(err.Error(), "certificate") {
			t.Errorf("ListRejectedEvents() error = %v, want a certificate verification error", err)
		}
	})
}

// TestNewKeepsTheTransportDefaults holds the CA file to changing the trust
// anchors alone. A bare transport literal inherits nothing from the default
// one, so naming a CA file would also drop the proxy, the dial timeout, and the
// handshake timeout.
func TestNewKeepsTheTransportDefaults(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)

	api, err := New("https://api.example", testToken, writeCA(t, server.Certificate().Raw))
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}

	transport, ok := api.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", api.http.Transport)
	}
	if transport.TLSClientConfig == nil || transport.TLSClientConfig.RootCAs == nil {
		t.Errorf("Transport carries no trust store, want the CA file's")
	}
	if transport.Proxy == nil {
		t.Errorf("Transport.Proxy = nil, want the default's, which reads HTTPS_PROXY")
	}
	if transport.DialContext == nil {
		t.Errorf("Transport.DialContext = nil, want the default's, which bounds the dial")
	}
	if transport.TLSHandshakeTimeout == 0 {
		t.Errorf("Transport.TLSHandshakeTimeout = 0, want the default's bound")
	}
}

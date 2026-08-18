package main

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// testToken is the credential every request under test has to carry.
const testToken = "s3cr3t-token"

// testClient builds a client against a stub server. The stub speaks plain HTTP,
// so no CA file is involved.
func testClient(t *testing.T, baseURL string) *client {
	t.Helper()

	api, err := newClient(baseURL, testToken, "")
	if err != nil {
		t.Fatalf("newClient() error = %v, want nil", err)
	}
	return api
}

// TestListResourcesFollowsTheCursor holds the walk to the paging contract: a
// page that names a next cursor is followed, and the filters travel with every
// call. A walk that stopped after the first page would bill a project's second
// page as if it did not exist.
func TestListResourcesFollowsTheCursor(t *testing.T) {
	var (
		queries []url.Values
		auths   []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		auths = append(auths, r.Header.Get("Authorization"))

		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = io.WriteString(w, `{"items":[{"resource_id":"abc-123"}],"next_cursor":"abc"}`)
			return
		}
		_, _ = io.WriteString(w, `{"items":[{"resource_id":"def-456"}],"next_cursor":null}`)
	}))
	defer server.Close()

	ids, err := testClient(t, server.URL).listResources(context.Background(), "os-prod-eu1", "proj-456")
	if err != nil {
		t.Fatalf("listResources() error = %v, want nil", err)
	}

	if want := []string{"abc-123", "def-456"}; strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("listResources() = %v, want %v", ids, want)
	}
	if len(queries) != 2 {
		t.Fatalf("the stub served %d requests, want 2", len(queries))
	}

	for i, query := range queries {
		for name, want := range map[string]string{
			"cloud":         "os-prod-eu1",
			"project_id":    "proj-456",
			"resource_type": "instance",
			"status":        "all",
			"limit":         "1000",
		} {
			if got := query.Get(name); got != want {
				t.Errorf("request %d asked for %s=%q, want %q", i, name, got, want)
			}
		}
		if want := "Bearer " + testToken; auths[i] != want {
			t.Errorf("request %d carried Authorization %q, want %q", i, auths[i], want)
		}
	}
	if got := queries[1].Get("cursor"); got != "abc" {
		t.Errorf("the second request asked for cursor=%q, want %q", got, "abc")
	}
	if got := queries[0].Get("cursor"); got != "" {
		t.Errorf("the first request asked for cursor=%q, want none", got)
	}
}

// TestListResourcesRefusesACursorThatDoesNotAdvance stops a walk the API cannot
// end. A page that names itself as the next one would be asked for forever,
// with its resources appended to the result on every turn, until the host runs
// out of memory.
func TestListResourcesRefusesACursorThatDoesNotAdvance(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[{"resource_id":"abc-123"}],"next_cursor":"stuck"}`)
	}))
	defer server.Close()

	_, err := testClient(t, server.URL).listResources(context.Background(), "os-prod-eu1", "proj-456")
	if err == nil {
		t.Fatalf("listResources() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "stuck") {
		t.Errorf("listResources() error = %v, want it to name the repeated cursor", err)
	}
	// The first page names the cursor and the second one repeats it, which is
	// where the walk stops: a third call would already be the loop.
	if calls != 2 {
		t.Errorf("the stub served %d requests, want 2", calls)
	}
}

// TestListResourcesRefusesMoreResourcesThanTheBudget bounds what the walk keeps
// in memory. limit= is a request the API is free to ignore, and a server that
// does serves pages of any size it likes; every one of them advances the
// cursor, so neither the repeat guard nor the page budget ends the growth. The
// walk stops on the resource past the budget rather than on the page.
func TestListResourcesRefusesMoreResourcesThanTheBudget(t *testing.T) {
	// A page a hundred times the size the walk asked for reaches the budget in
	// eleven pages rather than in a thousand.
	items := strings.TrimSuffix(strings.Repeat(`{"resource_id":"r"},`, 100*pageLimit), ",")

	served := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[`)
		_, _ = io.WriteString(w, items)
		_, _ = fmt.Fprintf(w, `],"next_cursor":"%d"}`, served)
	}))
	defer server.Close()

	ids, err := testClient(t, server.URL).listResources(context.Background(), "os-prod-eu1", "proj-456")
	if err == nil {
		t.Fatalf("listResources() = %d ids, want an error", len(ids))
	}
	for _, want := range []string{"proj-456", strconv.Itoa(resourceBudget)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("listResources() error = %v, want it to name %s", err, want)
		}
	}
	if ids != nil {
		t.Errorf("listResources() = %d ids, want none kept", len(ids))
	}
	// Ten pages fill the budget and the eleventh is where the walk gives up: a
	// walk that ran to the page budget would have served a thousand.
	if want := resourceBudget/(100*pageLimit) + 1; served != want {
		t.Errorf("the stub served %d pages, want %d", served, want)
	}
}

// TestClientMapsProblems keeps the API's own diagnosis in the error. The
// problem type is the part a reader recognizes the case by, so an unauthorized
// run and a history the API refuses to serve stay distinguishable from the
// error text alone.
func TestClientMapsProblems(t *testing.T) {
	t.Run("an unauthorized list", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"urn:tally:error:unauthorized","title":"Unauthorized",`+
				`"status":401,"detail":"the token is unknown"}`)
		}))
		defer server.Close()

		_, err := testClient(t, server.URL).listResources(context.Background(), "os-prod-eu1", "proj-456")
		if err == nil {
			t.Fatalf("listResources() error = nil, want an error")
		}
		for _, want := range []string{"urn:tally:error:unauthorized", "Unauthorized", "the token is unknown"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("listResources() error = %v, want it to name %s", err, want)
			}
		}
	})

	t.Run("a history the API refuses to serve", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"type":"urn:tally:error:history_too_long","title":"History too long",`+
				`"status":422}`)
		}))
		defer server.Close()

		_, err := testClient(t, server.URL).fetchHistory(context.Background(), "os-prod-eu1", "abc-123")
		if err == nil {
			t.Fatalf("fetchHistory() error = nil, want an error")
		}
		for _, want := range []string{"abc-123", "urn:tally:error:history_too_long", "History too long"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("fetchHistory() error = %v, want it to name %s", err, want)
			}
		}
	})
}

// TestClientReportsUnreadableAnswers keeps an answer the client cannot read
// apart from one the API refused in its own words. A gateway between the run
// and the API answers in HTML, and a connection cut mid-answer leaves valid
// JSON that simply stops: neither is a problem document, and the error has to
// say so rather than report an empty title.
func TestClientReportsUnreadableAnswers(t *testing.T) {
	t.Run("a gateway answering in HTML", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<html><body>502 Bad Gateway</body></html>")
		}))
		defer server.Close()

		_, err := testClient(t, server.URL).listResources(context.Background(), "os-prod-eu1", "proj-456")
		if err == nil {
			t.Fatalf("listResources() error = nil, want an error")
		}
		for _, want := range []string{"502", "unreadable problem document"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("listResources() error = %v, want it to name %s", err, want)
			}
		}
	})

	t.Run("an answer that stops halfway", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"items":[{"resource_id":"abc-`)
		}))
		defer server.Close()

		_, err := testClient(t, server.URL).listResources(context.Background(), "os-prod-eu1", "proj-456")
		if err == nil {
			t.Fatalf("listResources() error = nil, want an error")
		}
		for _, want := range []string{"decoding the answer", "/api/v1/resources"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("listResources() error = %v, want it to name %s", err, want)
			}
		}
	})
}

// TestClientPropagatesTransportErrors keeps a failed call wrapped rather than
// replaced: an unreachable API is a different problem from one that answered,
// and only the wrapped error says which host and which URL failed.
func TestClientPropagatesTransportErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	api := testClient(t, server.URL)
	server.Close()

	_, err := api.listResources(context.Background(), "os-prod-eu1", "proj-456")
	if err == nil {
		t.Fatalf("listResources() error = nil, want an error")
	}

	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Errorf("listResources() error = %v, want it to wrap a *url.Error", err)
	}
}

// TestNewClientRejectsABadCAFile refuses a trust store that would trust
// nothing. Without the check every call would fail with a verification error
// that says nothing about the file behind it.
func TestNewClientRejectsABadCAFile(t *testing.T) {
	t.Run("a file that is not there", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.crt")

		_, err := newClient("https://api.example", testToken, path)
		if err == nil {
			t.Fatalf("newClient() error = nil, want an error")
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("newClient() error = %v, want it to wrap os.ErrNotExist", err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("newClient() error = %v, want it to name the file %s", err, path)
		}
	})

	t.Run("a file without certificates", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.crt")
		if err := os.WriteFile(path, []byte("not a certificate\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		_, err := newClient("https://api.example", testToken, path)
		if err == nil {
			t.Fatalf("newClient() error = nil, want an error")
		}
		if !strings.Contains(err.Error(), "no PEM certificates") {
			t.Errorf("newClient() error = %v, want it to report an empty trust store", err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("newClient() error = %v, want it to name the file %s", err, path)
		}
	})
}

// TestNewClientRefusesAPlaintextURL keeps the credential off the wire. The
// token the run authenticates with reads every project's resources and their
// whole history, and it travels in a header on every call, so a base URL that
// is not https would hand it to anything on the path.
func TestNewClientRefusesAPlaintextURL(t *testing.T) {
	t.Run("a plaintext URL", func(t *testing.T) {
		_, err := newClient("http://api.example", testToken, "")
		if err == nil {
			t.Fatalf("newClient() error = nil, want an error")
		}
		for _, want := range []string{"http://api.example", "cleartext"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("newClient() error = %v, want it to name %s", err, want)
			}
		}
	})

	// Loopback is the exception: a port-forward to a dev cluster and the test
	// servers of this package both listen there, and nothing leaves the host.
	for name, baseURL := range map[string]string{
		"an https URL":         "https://api.example",
		"a loopback address":   "http://127.0.0.1:8443",
		"a loopback name":      "http://localhost:8443",
		"a loopback IPv6 host": "http://[::1]:8443",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newClient(baseURL, testToken, ""); err != nil {
				t.Errorf("newClient(%q) error = %v, want nil", baseURL, err)
			}
		})
	}
}

// TestClientDoesNotFollowRedirects keeps the token from travelling somewhere
// the operator did not name. Go strips the Authorization header only across
// hosts, so a redirect from https to http on the same host would carry the
// credential over in the clear; the answer is reported instead of followed.
func TestClientDoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect was followed, carrying Authorization %q", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[],"next_cursor":null}`)
	}))
	defer target.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL+"/api/v1/resources", http.StatusFound)
	}))
	defer server.Close()

	_, err := testClient(t, server.URL).listResources(context.Background(), "os-prod-eu1", "proj-456")
	if err == nil {
		t.Fatalf("listResources() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), "302") {
		t.Errorf("listResources() error = %v, want it to report the redirect's status", err)
	}
}

// TestNewClientKeepsTheTransportDefaults holds the CA file to changing the
// trust anchors alone. A bare transport literal inherits nothing from the
// default one, so a run that names a CA file would also lose the proxy, the
// dial timeout, and the handshake timeout: a Gateway that accepts the
// connection and never completes the handshake would hang the run instead of
// failing it.
func TestNewClientKeepsTheTransportDefaults(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "ca.crt")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	api, err := newClient("https://api.example", testToken, path)
	if err != nil {
		t.Fatalf("newClient() error = %v, want nil", err)
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

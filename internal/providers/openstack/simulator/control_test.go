package simulator

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// controlProgress is what the run reports to the endpoint under test. The two
// counts differ so that a document that swapped published for total fails
// rather than reading the same either way.
var controlProgress = Progress{
	From:      clockStart,
	To:        time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	Published: 12,
	Total:     231,
}

// controlServer serves the endpoint over a clock whose wall time is frozen, so
// the virtual now the document reports is the month's first instant however
// long the test takes. The clock comes back with the server because what a
// request did to the factor is asserted on it.
func controlServer(t *testing.T) (*Clock, *httptest.Server) {
	t.Helper()

	wall := time.Unix(0, 0)
	clock := NewClock(clockStart, 744, func() time.Time { return wall })
	server := httptest.NewServer(NewControlMux(clock, func() Progress { return controlProgress }))
	t.Cleanup(server.Close)
	return clock, server
}

// request sends one request to the endpoint and returns the status, the content
// type, and the body of the answer.
func request(t *testing.T, server *httptest.Server, method, path, body string) (int, string, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the answer to %s %s: %v", method, path, err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(answer)
}

// decodeDocument reads the clock document out of an answer's body.
func decodeDocument(t *testing.T, body string) clockDocument {
	t.Helper()

	var doc clockDocument
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	return doc
}

func TestClockEndpointReportsTheState(t *testing.T) {
	_, server := controlServer(t)

	status, contentType, body := request(t, server, http.MethodGet, "/clock", "")
	if status != http.StatusOK {
		t.Fatalf("GET /clock = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("GET /clock Content-Type = %q, want it to start with application/json", contentType)
	}

	doc := decodeDocument(t, body)
	want := clockDocument{
		VirtualNow: "2026-07-01T00:00:00Z",
		Factor:     744,
		Published:  12,
		Total:      231,
		PeriodFrom: "2026-07-01T00:00:00Z",
		PeriodTo:   "2026-08-01T00:00:00Z",
	}
	if doc != want {
		t.Errorf("GET /clock document = %+v, want %+v", doc, want)
	}

	status, contentType, body = request(t, server, http.MethodGet, "/healthz", "")
	if status != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if body != "ok" {
		t.Errorf("GET /healthz body = %q, want %q", body, "ok")
	}
	if !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("GET /healthz Content-Type = %q, want it to start with text/plain", contentType)
	}
}

// TestClockEndpointSetsTheFactor uses 0 as the new factor, which is the one
// value a missing member would otherwise be taken for, and the one an operator
// reaches for to let a run finish as fast as the broker takes it.
func TestClockEndpointSetsTheFactor(t *testing.T) {
	clock, server := controlServer(t)

	status, _, body := request(t, server, http.MethodPut, "/clock", `{"factor": 0}`)
	if status != http.StatusOK {
		t.Fatalf("PUT /clock = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if got := decodeDocument(t, body).Factor; got != 0 {
		t.Errorf("PUT /clock document factor = %g, want 0", got)
	}
	if got := clock.Factor(); got != 0 {
		t.Errorf("Factor() after PUT /clock = %g, want 0", got)
	}

	status, _, body = request(t, server, http.MethodGet, "/clock", "")
	if status != http.StatusOK {
		t.Fatalf("GET /clock = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if got := decodeDocument(t, body).Factor; got != 0 {
		t.Errorf("GET /clock factor after the change = %g, want 0", got)
	}
}

func TestClockEndpointRefusesABadBody(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "a body that is not JSON", body: "not json"},
		{name: "an object without the member", body: "{}"},
		{name: "a negative factor", body: `{"factor": -5}`},
		{name: "a factor that is not a number", body: `{"factor": "fast"}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock, server := controlServer(t)

			status, contentType, body := request(t, server, http.MethodPut, "/clock", c.body)
			if status != http.StatusBadRequest {
				t.Fatalf("PUT /clock %s = %d, want %d (body %q)",
					c.body, status, http.StatusBadRequest, body)
			}
			if !strings.HasPrefix(contentType, "text/plain") {
				t.Errorf("PUT /clock Content-Type = %q, want it to start with text/plain", contentType)
			}
			if body != badFactorBody {
				t.Errorf("PUT /clock body = %q, want %q", body, badFactorBody)
			}
			// The refusal leaves the run at the pace it was started with, rather
			// than at some pace the bad body was read as.
			if got := clock.Factor(); got != 744 {
				t.Errorf("Factor() after the refusal = %g, want 744", got)
			}
		})
	}
}

// TestClockEndpointRefusesOtherMethods holds the routes to the method they were
// registered with: the mux answers 405 itself, so neither handler ever sees a
// request it was not written for.
func TestClockEndpointRefusesOtherMethods(t *testing.T) {
	_, server := controlServer(t)

	status, _, body := request(t, server, http.MethodPost, "/clock", `{"factor": 1}`)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("POST /clock = %d, want %d (body %q)", status, http.StatusMethodNotAllowed, body)
	}

	status, _, body = request(t, server, http.MethodDelete, "/healthz", "")
	if status != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /healthz = %d, want %d (body %q)", status, http.StatusMethodNotAllowed, body)
	}
}

func TestControlAddrBindsLoopbackUnlessAskedOtherwise(t *testing.T) {
	// The endpoint carries no credential and PUT /clock changes the pace of a
	// run, so the address it binds is what keeps it out of reach. A bind on every
	// interface is a deployment's own decision, and a Config that never went
	// through Load does not fall into it.
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"the default Load resolves", Config{HTTPAddr: loopback, HTTPPort: 8080}, "127.0.0.1:8080"},
		{"a Config that never went through Load", Config{HTTPPort: 8080}, "127.0.0.1:8080"},
		{"the address a published deployment sets", Config{HTTPAddr: "0.0.0.0", HTTPPort: 8080}, "0.0.0.0:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.ControlAddr(); got != tc.want {
				t.Errorf("ControlAddr() = %q, want %q", got, tc.want)
			}
		})
	}
}

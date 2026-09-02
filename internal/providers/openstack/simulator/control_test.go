package simulator

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
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
	Held:      7,
	Holding:   true,
}

// controlServer serves the endpoint over a clock whose wall time is frozen, so
// the virtual now the document reports is the month's first instant however
// long the test takes. The clock comes back with the server because what a
// request did to the factor is asserted on it. release is what POST /release
// calls, and a nil one is a run that holds nothing back.
func controlServer(t *testing.T, release func() error) (*Clock, *httptest.Server) {
	t.Helper()

	wall := time.Unix(0, 0)
	clock := NewClock(clockStart, 744, func() time.Time { return wall })
	server := httptest.NewServer(NewControlMux(clock, func() Progress { return controlProgress }, release))
	t.Cleanup(server.Close)
	return clock, server
}

// request sends one request to the endpoint and returns the status, the content
// type, and the body of the answer.
func request(t *testing.T, server *httptest.Server, method, path, body string) (int, string, string) {
	t.Helper()

	return requestWithType(t, server, method, path, "", body)
}

// requestWithType sends one request under a content type of its own. An empty
// one sends none, the way curl sends none for a request without a body.
func requestWithType(t *testing.T, server *httptest.Server, method, path, contentType, body string,
) (int, string, string) {
	t.Helper()

	return requestWithHeader(t, server, method, path, "Content-Type", contentType, body)
}

// requestWithHeader sends one request carrying a header of its own, which is
// how a case sends what a browser puts on a request a page makes. An empty
// value sends the header not at all.
func requestWithHeader(t *testing.T, server *httptest.Server, method, path, header, value, body string,
) (int, string, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	if value != "" {
		req.Header.Set(header, value)
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
	_, server := controlServer(t, nil)

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
		Held:       7,
		Holding:    true,
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
	clock, server := controlServer(t, nil)

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
			clock, server := controlServer(t, nil)

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

// TestClockEndpointRefusesARequestABrowserSent covers the guard the release
// carries too. A cross-origin PUT is stopped by the preflight the mux answers
// 405, but a page that resolved the endpoint's address itself sends one
// same-origin, where no preflight stands in the way and the browser still
// appends the two headers this refuses on.
func TestClockEndpointRefusesARequestABrowserSent(t *testing.T) {
	cases := []struct {
		name   string
		header string
		value  string
	}{
		{name: "a page's own Origin", header: "Origin", value: "http://127.0.0.1:8091"},
		{name: "the mark the browser put on it", header: "Sec-Fetch-Site", value: "same-origin"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock, server := controlServer(t, nil)

			status, contentType, body := requestWithHeader(t, server, http.MethodPut, "/clock",
				c.header, c.value, `{"factor": 0}`)
			if status != http.StatusForbidden {
				t.Fatalf("PUT /clock from a page = %d, want %d (body %q)",
					status, http.StatusForbidden, body)
			}
			if !strings.HasPrefix(contentType, "text/plain") {
				t.Errorf("PUT /clock Content-Type = %q, want it to start with text/plain", contentType)
			}
			if body != badFactorOrigin {
				t.Errorf("PUT /clock body = %q, want %q", body, badFactorOrigin)
			}
			// The refusal leaves the run at the pace it was started with rather
			// than at the one the page asked for.
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
	_, server := controlServer(t, nil)

	status, _, body := request(t, server, http.MethodPost, "/clock", `{"factor": 1}`)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("POST /clock = %d, want %d (body %q)", status, http.StatusMethodNotAllowed, body)
	}

	status, _, body = request(t, server, http.MethodDelete, "/healthz", "")
	if status != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /healthz = %d, want %d (body %q)", status, http.StatusMethodNotAllowed, body)
	}
}

// TestHoldbackReleasesOnce covers the three answers a release gets and the one
// state change it makes: the first release closes the channel broadcast waits
// on, and every release after it finds nothing left to let out.
func TestHoldbackReleasesOnce(t *testing.T) {
	if err := newHoldback(0).release(); !errors.Is(err, errNothingHeld) {
		t.Errorf("release() on a run holding nothing = %v, want %v", err, errNothingHeld)
	}

	hb := newHoldback(3)
	if err := hb.release(); !errors.Is(err, errStillPublishing) {
		t.Errorf("release() while the month publishes = %v, want %v", err, errStillPublishing)
	}
	if got := hb.held(); got != 3 {
		t.Errorf("held() while the month publishes = %d, want 3", got)
	}

	hb.phase.Store(phaseHolding)
	if err := hb.release(); err != nil {
		t.Fatalf("release() while holding = %v, want nil", err)
	}
	if got := hb.held(); got != 0 {
		t.Errorf("held() after the release = %d, want 0", got)
	}
	select {
	case <-hb.released:
	default:
		t.Error("the released channel is open after the release, want it closed: it is what the run waits on")
	}

	if err := hb.release(); !errors.Is(err, errAlreadyReleased) {
		t.Errorf("a second release() = %v, want %v", err, errAlreadyReleased)
	}
}

// TestReleaseEndpointAnswersEachRefusal holds the endpoint to what it says
// about a release it cannot grant. Each refusal is the whole answer, because
// what the caller does about it is read off that one line.
func TestReleaseEndpointAnswersEachRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a run started without the switch", errNothingHeld},
		{"a month that is still publishing", errStillPublishing},
		{"a release that already happened", errAlreadyReleased},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, server := controlServer(t, func() error { return tc.err })

			status, contentType, body := request(t, server, http.MethodPost, "/release", "")
			if status != http.StatusConflict {
				t.Fatalf("POST /release = %d, want %d (body %q)", status, http.StatusConflict, body)
			}
			if want := "text/plain; charset=utf-8"; contentType != want {
				t.Errorf("POST /release Content-Type = %q, want %q", contentType, want)
			}
			if body != tc.err.Error() {
				t.Errorf("POST /release body = %q, want %q", body, tc.err.Error())
			}
		})
	}

	t.Run("a run holding notifications back", func(t *testing.T) {
		_, server := controlServer(t, func() error { return nil })

		status, contentType, body := request(t, server, http.MethodPost, "/release", "")
		if status != http.StatusOK {
			t.Fatalf("POST /release = %d, want %d (body %q)", status, http.StatusOK, body)
		}
		if !strings.HasPrefix(contentType, "application/json") {
			t.Errorf("POST /release Content-Type = %q, want it to start with application/json", contentType)
		}
		if got := decodeDocument(t, body).Total; got != controlProgress.Total {
			t.Errorf("POST /release document total = %d, want %d", got, controlProgress.Total)
		}
	})

	t.Run("a body a page in a browser could send unasked", func(t *testing.T) {
		_, server := controlServer(t, func() error {
			t.Error("the release ran, want the content type to have refused the request first")
			return nil
		})

		status, contentType, body := requestWithType(t, server, http.MethodPost, "/release",
			"application/x-www-form-urlencoded", "release=1")
		if status != http.StatusUnsupportedMediaType {
			t.Fatalf("POST /release as a form = %d, want %d (body %q)",
				status, http.StatusUnsupportedMediaType, body)
		}
		if !strings.HasPrefix(contentType, "text/plain") {
			t.Errorf("POST /release Content-Type = %q, want it to start with text/plain", contentType)
		}
		if body != badReleaseType {
			t.Errorf("POST /release body = %q, want %q", body, badReleaseType)
		}
	})

	t.Run("a bodyless request a page in a browser could send unasked", func(t *testing.T) {
		_, server := controlServer(t, func() error {
			t.Error("the release ran, want the Origin to have refused the request first")
			return nil
		})

		// A fetch without a body sends no content type at all, and needs no
		// preflight to reach loopback. What is left of it to refuse is the Origin
		// the browser put on it.
		status, contentType, body := requestWithHeader(t, server, http.MethodPost, "/release",
			"Origin", "https://example.invalid", "")
		if status != http.StatusForbidden {
			t.Fatalf("POST /release from a page = %d, want %d (body %q)",
				status, http.StatusForbidden, body)
		}
		if !strings.HasPrefix(contentType, "text/plain") {
			t.Errorf("POST /release Content-Type = %q, want it to start with text/plain", contentType)
		}
		if body != badReleaseOrigin {
			t.Errorf("POST /release body = %q, want %q", body, badReleaseOrigin)
		}
	})

	t.Run("a request a browser marked with Sec-Fetch-Site", func(t *testing.T) {
		_, server := controlServer(t, func() error {
			t.Error("the release ran, want Sec-Fetch-Site to have refused the request first")
			return nil
		})

		status, _, body := requestWithHeader(t, server, http.MethodPost, "/release",
			"Sec-Fetch-Site", "cross-site", "")
		if status != http.StatusForbidden {
			t.Fatalf("POST /release from a page = %d, want %d (body %q)",
				status, http.StatusForbidden, body)
		}
		if body != badReleaseOrigin {
			t.Errorf("POST /release body = %q, want %q", body, badReleaseOrigin)
		}
	})

	t.Run("a JSON body a script sent on purpose", func(t *testing.T) {
		_, server := controlServer(t, func() error { return nil })

		status, _, body := requestWithType(t, server, http.MethodPost, "/release",
			"application/json", "{}")
		if status != http.StatusOK {
			t.Fatalf("POST /release as JSON = %d, want %d (body %q)", status, http.StatusOK, body)
		}
	})

	t.Run("a method the route was not registered with", func(t *testing.T) {
		_, server := controlServer(t, func() error { return nil })

		status, _, body := request(t, server, http.MethodGet, "/release", "")
		if status != http.StatusMethodNotAllowed {
			t.Errorf("GET /release = %d, want %d (body %q)", status, http.StatusMethodNotAllowed, body)
		}
	})

	t.Run("a mux built without a release", func(t *testing.T) {
		_, server := controlServer(t, nil)

		status, _, body := request(t, server, http.MethodPost, "/release", "")
		if status != http.StatusConflict {
			t.Fatalf("POST /release = %d, want %d (body %q)", status, http.StatusConflict, body)
		}
		if body != errNothingHeld.Error() {
			t.Errorf("POST /release body = %q, want %q", body, errNothingHeld.Error())
		}
	})
}

// TestReleaseFollowsTheHoldingTheDocumentReports ties the document a caller
// polls to the release it sends next. The counts reach their hold values while
// the run is still on its way into the hold, so holding is the one member a
// script may act on, and the document the release answers with is the month one
// release short however fast the run publishes the rest.
func TestReleaseFollowsTheHoldingTheDocumentReports(t *testing.T) {
	const total, held = 231, 7

	hb := newHoldback(held)
	var published atomic.Int64
	// The last regular notification is on the bus: the count says so before the
	// run has entered the hold, which is where a caller polling on it is refused.
	published.Store(total - held)

	wall := time.Unix(0, 0)
	clock := NewClock(clockStart, 744, func() time.Time { return wall })
	progress := func() Progress {
		return Progress{
			From: clockStart, To: controlProgress.To,
			Published: int(published.Load()), Total: total,
			Held: hb.held(), Holding: hb.holding(),
		}
	}
	// A run at factor 0 publishes the held share as fast as the broker confirms
	// it, which is what a document read after the release would count.
	release := func() error {
		err := hb.release()
		if err == nil {
			published.Add(held)
		}
		return err
	}
	server := httptest.NewServer(NewControlMux(clock, progress, release))
	t.Cleanup(server.Close)

	status, _, body := request(t, server, http.MethodGet, "/clock", "")
	if status != http.StatusOK {
		t.Fatalf("GET /clock = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if doc := decodeDocument(t, body); doc.Holding {
		t.Errorf("GET /clock holding = true with %d of %d published, want false: "+
			"the run has not entered the hold", doc.Published, doc.Total)
	}
	status, _, body = request(t, server, http.MethodPost, "/release", "")
	if status != http.StatusConflict {
		t.Fatalf("POST /release before the hold = %d, want %d (body %q)",
			status, http.StatusConflict, body)
	}

	// What broadcast does once the last regular line is confirmed.
	hb.phase.Store(phaseHolding)

	status, _, body = request(t, server, http.MethodGet, "/clock", "")
	if status != http.StatusOK {
		t.Fatalf("GET /clock = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if doc := decodeDocument(t, body); !doc.Holding {
		t.Fatal("GET /clock holding = false while the run holds, want true: " +
			"it is the signal a release is sent on")
	}

	status, _, body = request(t, server, http.MethodPost, "/release", "")
	if status != http.StatusOK {
		t.Fatalf("POST /release on a reported hold = %d, want %d (body %q)",
			status, http.StatusOK, body)
	}
	doc := decodeDocument(t, body)
	if doc.Published != total-held {
		t.Errorf("POST /release document published = %d, want %d: the answer states the month "+
			"one release short, not however far the release got before it was written",
			doc.Published, total-held)
	}
	if doc.Held != 0 || doc.Holding {
		t.Errorf("POST /release document held = %d, holding = %t, want 0 and false",
			doc.Held, doc.Holding)
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

// runMux is the mux a run serves: the control routes, the inventory endpoint,
// and the fake OpenStack API beneath the two. The wall clock is frozen, so
// every document the endpoint answers with is the one the test picked however
// long the test takes. A nil exporter is a run with metrics turned off.
func runMux(t *testing.T, month Month, withExporter bool) *httptest.Server {
	t.Helper()

	wall := time.Unix(0, 0)
	clock := NewClock(cloudDay(10), 744, func() time.Time { return wall })
	api, err := NewCloudAPI(clock, month.Oracle)
	if err != nil {
		t.Fatalf("NewCloudAPI() error = %v, want nil", err)
	}

	var exporter http.Handler
	if withExporter {
		exporter = NewExporter(month, clock)
	}
	mux := NewControlMux(clock, func() Progress { return controlProgress }, func() error { return nil })
	mountRun(mux, api, exporter)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// servedMonth is one month a run serves: one tenant, and one instance of it
// that outlives the month, so both the scrape and the listing hold something.
func servedMonth() Month {
	return Month{
		Tenants: []Tenant{{ID: cloudTenant, Name: "tenant-a", Workload: workloadClassic}},
		Oracle:  testOracle(oneInstance("web-01", cloudDay(2), cloudTo)),
	}
}

func TestMetricsRouteIsMatchedAheadOfTheFakeAPI(t *testing.T) {
	server := runMux(t, servedMonth(), true)

	status, contentType, body := request(t, server, http.MethodGet, "/metrics", "")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if !strings.HasPrefix(contentType, "text/plain") {
		t.Errorf("GET /metrics Content-Type = %q, want it to start with text/plain", contentType)
	}
	if !strings.Contains(body, seriesNovaTotalVMs) {
		t.Errorf("GET /metrics states no %s, want the scrape and not the fake API's 404",
			seriesNovaTotalVMs)
	}

	status, _, body = request(t, server, http.MethodGet, "/healthz", "")
	if status != http.StatusOK || body != "ok" {
		t.Errorf("GET /healthz = %d with body %q, want %d and %q", status, body, http.StatusOK, "ok")
	}

	status, _, body = request(t, server, http.MethodGet, "/clock", "")
	if status != http.StatusOK {
		t.Fatalf("GET /clock = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	want := clockDocument{
		VirtualNow: cloudDay(10).Format(time.RFC3339),
		Factor:     744,
		Published:  controlProgress.Published,
		Total:      controlProgress.Total,
		Held:       controlProgress.Held,
		Holding:    controlProgress.Holding,
		PeriodFrom: cloudFrom.Format(time.RFC3339),
		PeriodTo:   cloudTo.Format(time.RFC3339),
	}
	if doc := decodeDocument(t, body); doc != want {
		t.Errorf("GET /clock document = %+v, want %+v", doc, want)
	}

	status, _, body = request(t, server, http.MethodPut, "/clock", `{"factor": 0}`)
	if status != http.StatusOK {
		t.Fatalf("PUT /clock = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if got := decodeDocument(t, body).Factor; got != 0 {
		t.Errorf("PUT /clock document factor = %g, want 0", got)
	}

	status, contentType, body = request(t, server, http.MethodPost, "/release", "")
	if status != http.StatusOK {
		t.Fatalf("POST /release = %d, want %d (body %q)", status, http.StatusOK, body)
	}
	if !strings.HasPrefix(contentType, "application/json") {
		t.Errorf("POST /release Content-Type = %q, want it to start with application/json", contentType)
	}

	// Everything the two patterns above leave is still the fake API's, so a sync
	// reads the month off the very listener a scrape reads the inventory off.
	status, body = askCloud(t, server, tokenOf(t, server), serversPath)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want %d (body %q)", serversPath, status, http.StatusOK, body)
	}
	if got := servedIDs(t, body, "servers"); !slices.Equal(got, []string{"web-01"}) {
		t.Errorf("the live listing answered with %v, want the instance the month holds at day 10", got)
	}
}

func TestMetricsRouteIsAbsentWhenMetricsAreOff(t *testing.T) {
	server := runMux(t, servedMonth(), false)

	status, _, body := request(t, server, http.MethodGet, "/metrics", "")
	if status != http.StatusNotFound {
		t.Errorf("GET /metrics with metrics off = %d, want %d from the fake API's catch-all (body %q)",
			status, http.StatusNotFound, body)
	}
	status, _, body = request(t, server, http.MethodGet, "/healthz", "")
	if status != http.StatusOK || body != "ok" {
		t.Errorf("GET /healthz with metrics off = %d with body %q, want %d and %q",
			status, body, http.StatusOK, "ok")
	}

	// A replay mounts neither: it holds the notifications of a month and no
	// oracle, so there is nothing to scrape and nothing to list.
	replayMux := NewControlMux(NewClock(cloudDay(10), 0, time.Now),
		func() Progress { return controlProgress }, nil)
	mountRun(replayMux, nil, nil)
	replay := httptest.NewServer(replayMux)
	t.Cleanup(replay.Close)

	status, _, body = request(t, replay, http.MethodGet, "/metrics", "")
	if status != http.StatusNotFound {
		t.Errorf("GET /metrics on the mux of a replay = %d, want %d (body %q)",
			status, http.StatusNotFound, body)
	}
}

package counters_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/engine/counters"
)

// vectorBody is what VictoriaMetrics answers for a query that selects one
// series: an instant vector of one sample, its value a string.
const vectorBody = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1773792000,"38.5"]}]}}`

// draftEnd is the instant the queries of these tests are run at.
var draftEnd = time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)

// testClient builds the client the tests query through: the retry policy the
// package default uses, with the waits cut to a millisecond and the number of
// retries set per test.
func testClient(retryMax int) *http.Client {
	rc := retryablehttp.NewClient()
	rc.Logger = nil
	rc.ErrorHandler = retryablehttp.PassthroughErrorHandler
	rc.RetryWaitMin = time.Millisecond
	rc.RetryWaitMax = time.Millisecond
	rc.RetryMax = retryMax
	return rc.StandardClient()
}

// recordedRequest is what a test server saw of one request.
type recordedRequest struct {
	method string
	path   string
	query  url.Values
}

// recorder collects the requests a test server saw. A retried query reaches the
// handler from the client's goroutine and the test reads the requests from its
// own, so the slice is guarded.
type recorder struct {
	mu       sync.Mutex
	requests []recordedRequest
}

func (r *recorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.requests = append(r.requests, recordedRequest{
		method: req.Method,
		path:   req.URL.Path,
		query:  req.URL.Query(),
	})
}

func (r *recorder) seen() []recordedRequest {
	r.mu.Lock()
	defer r.mu.Unlock()

	return slices.Clone(r.requests)
}

// newTestServer starts a server that records every request before handler
// answers it.
func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *recorder) {
	t.Helper()

	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// answer writes status and body, which is what most of these servers do.
func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// newQueryClient starts a server answering status and body and returns the
// client that queries it.
func newQueryClient(t *testing.T, retryMax, status int, body string) (*counters.VMClient, *recorder) {
	t.Helper()

	srv, rec := newTestServer(t, answer(status, body))
	c, err := counters.NewVMClient(srv.URL, testClient(retryMax))
	if err != nil {
		t.Fatalf("NewVMClient() error = %v, want nil", err)
	}
	return c, rec
}

func TestNewVMClient(t *testing.T) {
	t.Run("a url the engine cannot query is refused", func(t *testing.T) {
		tests := []struct {
			name    string
			baseURL string
			wantErr string
		}{
			{
				name:    "an unset url",
				baseURL: "",
				wantErr: "the VictoriaMetrics url is empty",
			},
			{
				name:    "a url that does not parse",
				baseURL: "://bad",
				wantErr: "parsing the VictoriaMetrics url:",
			},
			{
				name:    "a scheme that is not HTTP",
				baseURL: "ftp://vm:8428",
				wantErr: "must be http or https with a host",
			},
			{
				name:    "a url without a host",
				baseURL: "http://",
				wantErr: "must be http or https with a host",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := counters.NewVMClient(tc.baseURL, nil)
				if err == nil {
					t.Fatalf("NewVMClient(%q) error = nil, want %q", tc.baseURL, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("NewVMClient(%q) error = %q, want it to contain %q", tc.baseURL, err, tc.wantErr)
				}
			})
		}
	})

	// What that default does is run in TestDefaultHTTPClient, which reaches it
	// without going through a url.
	t.Run("a caller may pass no client and gets the package default", func(t *testing.T) {
		c, err := counters.NewVMClient("http://vm:8428", nil)
		if err != nil {
			t.Fatalf("NewVMClient() error = %v, want nil", err)
		}
		if c == nil {
			t.Fatal("NewVMClient() client = nil, want a client")
		}
	})
}

func TestVMClientQuery(t *testing.T) {
	t.Run("the request carries the expression and the draft's end", func(t *testing.T) {
		const expr = `sum(increase(x{cloud="os-prod-eu1"}[360h])) / 1e9`

		// The base url keeps its path prefix, whether or not it ends in a
		// slash, because vmauth publishes VictoriaMetrics under one.
		for _, base := range []string{"/vm", "/vm/"} {
			t.Run("a base url "+base, func(t *testing.T) {
				srv, rec := newTestServer(t, answer(http.StatusOK, vectorBody))
				c, err := counters.NewVMClient(srv.URL+base, testClient(0))
				if err != nil {
					t.Fatalf("NewVMClient() error = %v, want nil", err)
				}

				if _, err := c.Query(t.Context(), expr, draftEnd); err != nil {
					t.Fatalf("Query() error = %v, want nil", err)
				}

				seen := rec.seen()
				if len(seen) != 1 {
					t.Fatalf("saw %d requests, want 1", len(seen))
				}
				if seen[0].method != http.MethodGet {
					t.Errorf("method = %q, want %q", seen[0].method, http.MethodGet)
				}
				if want := "/vm/api/v1/query"; seen[0].path != want {
					t.Errorf("path = %q, want %q", seen[0].path, want)
				}
				if got := seen[0].query.Get("query"); got != expr {
					t.Errorf("query = %q, want %q", got, expr)
				}
				if got, want := seen[0].query.Get("time"), "2026-03-11T00:00:00Z"; got != want {
					t.Errorf("time = %q, want %q", got, want)
				}
			})
		}
	})

	t.Run("a draft ending between two seconds keeps its fraction", func(t *testing.T) {
		srv, rec := newTestServer(t, answer(http.StatusOK, vectorBody))
		c, err := counters.NewVMClient(srv.URL, testClient(0))
		if err != nil {
			t.Fatalf("NewVMClient() error = %v, want nil", err)
		}

		at := time.Date(2026, 3, 16, 0, 0, 0, 500_000_000, time.UTC)
		if _, err := c.Query(t.Context(), "up", at); err != nil {
			t.Fatalf("Query() error = %v, want nil", err)
		}

		seen := rec.seen()
		if len(seen) != 1 {
			t.Fatalf("saw %d requests, want 1", len(seen))
		}
		if got, want := seen[0].query.Get("time"), "2026-03-16T00:00:00.5Z"; got != want {
			t.Errorf("time = %q, want %q", got, want)
		}
	})

	t.Run("a vector of one series is its value", func(t *testing.T) {
		c, _ := newQueryClient(t, 0, http.StatusOK, vectorBody)

		got, err := c.Query(t.Context(), "up", draftEnd)
		if err != nil {
			t.Fatalf("Query() error = %v, want nil", err)
		}
		want, err := decimal.NewFromString("38.5")
		if err != nil {
			t.Fatalf("NewFromString() error = %v, want nil", err)
		}
		if !got.Equal(want) {
			t.Errorf("Query() = %s, want %s", got, want)
		}
	})

	t.Run("an empty result is zero rather than an error", func(t *testing.T) {
		c, _ := newQueryClient(t, 0, http.StatusOK,
			`{"status":"success","data":{"resultType":"vector","result":[]}}`)

		got, err := c.Query(t.Context(), "up", draftEnd)
		if err != nil {
			t.Fatalf("Query() error = %v, want nil", err)
		}
		if !got.IsZero() {
			t.Errorf("Query() = %s, want 0", got)
		}
	})

	t.Run("a result the engine cannot bill is an error", func(t *testing.T) {
		tests := []struct {
			name     string
			retryMax int
			status   int
			body     string
			wantErr  string
			// wantRequests is how often the query reached the server, which is
			// what says whether the status was retried.
			wantRequests int
			// wantAnswerShape is whether the failure is a property of this one
			// answer rather than of the store, which is what says whether a
			// caller may hold it against the next query.
			wantAnswerShape bool
		}{
			{
				name:            "more than one series, which no rule picks from",
				status:          http.StatusOK,
				body:            `{"status":"success","data":{"resultType":"vector","result":[{"value":[1773792000,"1"]},{"value":[1773792000,"2"]}]}}`,
				wantErr:         "returned 2 series, want at most one",
				wantRequests:    1,
				wantAnswerShape: true,
			},
			{
				name:            "a result that is not a vector",
				status:          http.StatusOK,
				body:            `{"status":"success","data":{"resultType":"scalar","result":[{"value":[1773792000,"1"]}]}}`,
				wantErr:         "returned a scalar result, want a vector",
				wantRequests:    1,
				wantAnswerShape: true,
			},
			{
				name:            "a value that is not a number",
				status:          http.StatusOK,
				body:            `{"status":"success","data":{"resultType":"vector","result":[{"value":[1773792000,"NaN"]}]}}`,
				wantErr:         `parsing the VictoriaMetrics value "NaN":`,
				wantRequests:    1,
				wantAnswerShape: true,
			},
			{
				// The single-series check below runs on a decoded body, which
				// is too late for the vector such a query returns.
				name:   "an answer too large to hold, from a query that did not aggregate",
				status: http.StatusOK,
				body: `{"status":"success","data":{"resultType":"vector","result":[` +
					strings.Repeat(`{"metric":{"resource_id":"abc-123"},"value":[1773792000,"1"]},`, 20000) +
					`{"metric":{},"value":[1773792000,"1"]}]}}`,
				wantErr:         "the VictoriaMetrics answer is larger than 1048576 bytes",
				wantRequests:    1,
				wantAnswerShape: true,
			},
			{
				// Whatever sits in front of VictoriaMetrics answers in
				// whatever form it likes, and none of it belongs in the log
				// line whole.
				name:         "an error page from in front of the store is quoted, not logged whole",
				status:       http.StatusBadGateway,
				body:         "<html>" + strings.Repeat("x", 500) + "</html>",
				wantErr:      "answered status 502: <html>" + strings.Repeat("x", 194),
				wantRequests: 1,
			},
			{
				// The bound is about the vector a query that does not
				// aggregate returns. An error page is as large as it likes,
				// and reporting it as a query that has to aggregate would send
				// oncall after a query that is correct.
				name:         "an error page larger than the bound on an answer",
				status:       http.StatusBadGateway,
				body:         "<html>" + strings.Repeat("x", 1<<20) + "</html>",
				wantErr:      "answered status 502: <html>" + strings.Repeat("x", 194),
				wantRequests: 1,
			},
			{
				name:         "a value serialized as a number rather than a string",
				status:       http.StatusOK,
				body:         `{"status":"success","data":{"resultType":"vector","result":[{"value":[1773792000,38.5]}]}}`,
				wantErr:      "decoding the VictoriaMetrics response:",
				wantRequests: 1,
			},
			{
				name:         "a series without a sample",
				status:       http.StatusOK,
				body:         `{"status":"success","data":{"resultType":"vector","result":[{"metric":{}}]}}`,
				wantErr:      "decoding the VictoriaMetrics response:",
				wantRequests: 1,
			},
			{
				name:         "a body that is not JSON",
				status:       http.StatusOK,
				body:         "not json",
				wantErr:      "decoding the VictoriaMetrics response:",
				wantRequests: 1,
			},
			{
				name:         "an error reported with status 200",
				status:       http.StatusOK,
				body:         `{"status":"error","errorType":"execution","error":"boom"}`,
				wantErr:      "answered status 200 execution: boom",
				wantRequests: 1,
			},
			{
				name:         "an invalid query, which is not retried",
				retryMax:     2,
				status:       http.StatusUnprocessableEntity,
				body:         `{"status":"error","errorType":"422","error":"unexpected token"}`,
				wantErr:      "answered status 422 422: unexpected token",
				wantRequests: 1,
			},
			{
				name:         "a server error whose body is not an error document",
				retryMax:     2,
				status:       http.StatusInternalServerError,
				body:         "internal error",
				wantErr:      "answered status 500: internal error",
				wantRequests: 3,
			},
			{
				name:         "a store that stays unavailable",
				retryMax:     2,
				status:       http.StatusServiceUnavailable,
				body:         `{"status":"error","errorType":"503","error":"too many concurrent requests"}`,
				wantErr:      "answered status 503",
				wantRequests: 3,
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				c, rec := newQueryClient(t, tc.retryMax, tc.status, tc.body)

				_, err := c.Query(t.Context(), "up", draftEnd)
				if err == nil {
					t.Fatalf("Query() error = nil, want %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("Query() error = %q, want it to contain %q", err, tc.wantErr)
				}
				if got := len(rec.seen()); got != tc.wantRequests {
					t.Errorf("saw %d requests, want %d", got, tc.wantRequests)
				}
				// A store that is down fails the next query the same way and is
				// worth backing off from; an answer only this query got is not.
				if got := errors.Is(err, counters.ErrAnswerShape); got != tc.wantAnswerShape {
					t.Errorf("errors.Is(err, ErrAnswerShape) = %t, want %t", got, tc.wantAnswerShape)
				}
			})
		}
	})

	t.Run("a store that is unavailable once is queried again", func(t *testing.T) {
		var attempts atomic.Int64
		srv, rec := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if attempts.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			answer(http.StatusOK, vectorBody)(w, r)
		})
		c, err := counters.NewVMClient(srv.URL, testClient(2))
		if err != nil {
			t.Fatalf("NewVMClient() error = %v, want nil", err)
		}

		got, err := c.Query(t.Context(), "up", draftEnd)
		if err != nil {
			t.Fatalf("Query() error = %v, want nil", err)
		}
		want, err := decimal.NewFromString("38.5")
		if err != nil {
			t.Fatalf("NewFromString() error = %v, want nil", err)
		}
		if !got.Equal(want) {
			t.Errorf("Query() = %s, want %s", got, want)
		}
		if seen := len(rec.seen()); seen != 2 {
			t.Errorf("saw %d requests, want 2", seen)
		}
	})

	t.Run("a canceled run stops the query", func(t *testing.T) {
		var once sync.Once
		started := make(chan struct{})
		srv, _ := newTestServer(t, func(_ http.ResponseWriter, r *http.Request) {
			once.Do(func() { close(started) })
			<-r.Context().Done()
		})
		c, err := counters.NewVMClient(srv.URL, testClient(2))
		if err != nil {
			t.Fatalf("NewVMClient() error = %v, want nil", err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go func() {
			<-started
			cancel()
		}()

		_, err = c.Query(ctx, "up", draftEnd)
		if err == nil {
			t.Fatal("Query() error = nil, want an error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Query() error = %v, want it to wrap context.Canceled", err)
		}
		if want := "querying VictoriaMetrics:"; !strings.Contains(err.Error(), want) {
			t.Errorf("Query() error = %q, want it to contain %q", err, want)
		}
	})
}

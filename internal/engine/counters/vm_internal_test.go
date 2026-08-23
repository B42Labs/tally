package counters

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

// defaultVectorBody is what VictoriaMetrics answers the queries below with: an
// instant vector of one series.
const defaultVectorBody = `{"status":"success","data":{"resultType":"vector","result":[{"value":[1773792000,"38.5"]}]}}`

// impatient is the client a caller gets when it passes none, with its waits cut
// to a millisecond and its retries to retryMax. Nothing else about it is
// changed, so what the cases below run is the configuration of a deployment
// rather than one a test rebuilt beside it.
func impatient(t *testing.T, retryMax int) *http.Client {
	t.Helper()

	c := defaultHTTPClient()
	rt, ok := c.Transport.(*retryablehttp.RoundTripper)
	if !ok {
		t.Fatalf("Transport = %T, want a *retryablehttp.RoundTripper", c.Transport)
	}
	rt.Client.RetryWaitMin = time.Millisecond
	rt.Client.RetryWaitMax = time.Millisecond
	rt.Client.RetryMax = retryMax
	return c
}

// TestDefaultHTTPClient runs the client a caller gets when it passes none. The
// query cases elsewhere build their own retry policy beside it, so without this
// one nothing exercises the package default: dropping its passthrough handler
// would replace the status Query reports with a bare "giving up after N
// attempt(s)", and dropping its nil logger would write every retry to stderr,
// past the engine's handler.
func TestDefaultHTTPClient(t *testing.T) {
	t.Run("one attempt is bounded and four retries are left to the policy", func(t *testing.T) {
		c := defaultHTTPClient()
		rt, ok := c.Transport.(*retryablehttp.RoundTripper)
		if !ok {
			t.Fatalf("Transport = %T, want a *retryablehttp.RoundTripper", c.Transport)
		}

		// The bound sits on the client each attempt goes through, below the
		// retrying transport, so it is per attempt rather than per query.
		if got := rt.Client.HTTPClient.Timeout; got != queryTimeout {
			t.Errorf("HTTPClient.Timeout = %s, want %s", got, queryTimeout)
		}
		if rt.Client.RetryMax != 4 {
			t.Errorf("RetryMax = %d, want 4", rt.Client.RetryMax)
		}
		if rt.Client.Logger != nil {
			t.Errorf("Logger = %#v, want nil, which is what keeps a retry line out of stderr", rt.Client.Logger)
		}
	})

	t.Run("a store that is unavailable once is queried again", func(t *testing.T) {
		var attempts atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if attempts.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, defaultVectorBody)
		}))
		t.Cleanup(srv.Close)

		c, err := NewVMClient(srv.URL, impatient(t, 2))
		if err != nil {
			t.Fatalf("NewVMClient() error = %v, want nil", err)
		}

		got, err := c.Query(t.Context(), "up", time.Now().UTC())
		if err != nil {
			t.Fatalf("Query() error = %v, want nil", err)
		}
		if want := "38.5"; got.String() != want {
			t.Errorf("Query() = %s, want %s", got, want)
		}
		if attempts.Load() != 2 {
			t.Errorf("the store saw %d attempts, want 2", attempts.Load())
		}
	})

	t.Run("an exhausted retry reports the store's answer, not the attempts", func(t *testing.T) {
		var attempts atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "internal error")
		}))
		t.Cleanup(srv.Close)

		c, err := NewVMClient(srv.URL, impatient(t, 2))
		if err != nil {
			t.Fatalf("NewVMClient() error = %v, want nil", err)
		}

		_, err = c.Query(t.Context(), "up", time.Now().UTC())
		if err == nil {
			t.Fatal("Query() error = nil, want an error")
		}
		if want := "answered status 500: internal error"; !strings.Contains(err.Error(), want) {
			t.Errorf("Query() error = %q, want it to contain %q", err, want)
		}
		if want := "giving up after"; strings.Contains(err.Error(), want) {
			t.Errorf("Query() error = %q, want it not to contain %q", err, want)
		}
		if attempts.Load() != 3 {
			t.Errorf("the store saw %d attempts, want 3", attempts.Load())
		}
	})
}

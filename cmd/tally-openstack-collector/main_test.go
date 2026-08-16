package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/health"
	"github.com/b42labs/tally/internal/providers/openstack"
)

// startupTimeout is how long a test waits for the collector to answer its first
// request. Binding a port takes milliseconds; the margin is for a loaded CI
// machine.
const startupTimeout = 10 * time.Second

// closedBroker is an AMQP URL nothing listens on. The consumer retries a failed
// connection in the background, so the process starts against it and the probes
// report the state instead.
const closedBroker = "amqp://guest:guest@127.0.0.1:1/"

// setEnv applies vars and blanks every other variable the collector reads, so a
// test never inherits a value from the developer's shell. A variable set to the
// empty string falls back to its default exactly as an unset one does.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()

	for name := range vars {
		if !slices.Contains(openstack.EnvNames, name) {
			t.Fatalf("test sets %s, which the collector does not read", name)
		}
	}
	for _, name := range openstack.EnvNames {
		t.Setenv(name, vars[name])
	}
}

// serveEnv is a configuration the collector starts on. Both the broker and the
// Reporting API point at a port nothing listens on: the consumer and the sender
// retry in the background, so the process comes up and the probes report the
// outage. The log level keeps the warnings about those retries out of the test
// output.
func serveEnv(t *testing.T, port int) map[string]string {
	t.Helper()

	return map[string]string{
		"TALLY_LOG_LEVEL":         "ERROR",
		"TALLY_OSC_HTTP_PORT":     strconv.Itoa(port),
		"TALLY_OSC_AMQP_URL":      closedBroker,
		"TALLY_OSC_CLOUD":         "test",
		"TALLY_OSC_REPORTING_URL": "https://127.0.0.1:1",
		"TALLY_OSC_TOKEN":         "test-token",
		"TALLY_OSC_BUFFER_PATH":   filepath.Join(t.TempDir(), "outbox.db"),
	}
}

func TestRunShutsDownWhenTheContextIsCancelled(t *testing.T) {
	port := freePort(t)
	setEnv(t, serveEnv(t, port))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx, false) }()

	waitForHealthz(t, port, done)
	cancel()

	assertShutdown(t, done)
}

// TestRunServesTheProbes holds the two probes to the difference between them
// while the broker is unreachable: readiness fails at once, and liveness keeps
// answering 200 because a broker is not what a restart brings back.
func TestRunServesTheProbes(t *testing.T) {
	port := freePort(t)
	setEnv(t, serveEnv(t, port))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, false) }()
	waitForHealthz(t, port, done)

	body, status := get(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", port))
	if status != http.StatusServiceUnavailable {
		t.Errorf("GET /readyz = %d, want %d while the broker is unreachable (body %q)",
			status, http.StatusServiceUnavailable, body)
	}
	// What failed is in the log and not in the body: the route is unauthenticated
	// and the error underneath names the outbox file and the driver behind it.
	if !strings.Contains(body, "not ready") {
		t.Errorf("GET /readyz body = %q, want the probe's own answer", body)
	}

	body, status = get(t, fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if status != http.StatusOK {
		t.Errorf("GET /healthz = %d, want %d while only the broker is unreachable (body %q)",
			status, http.StatusOK, body)
	}

	cancel()
	assertShutdown(t, done)
}

// TestProbesWeighTheBrokerAndTheOutboxDifferently drives the routes without the
// process around them, because what the liveness branches turn on is how long an
// outage has lasted and that is a clock rather than a wait.
//
// The two probes answer different questions. Readiness asks whether this pod
// should be sent traffic and fails on either dependency. Liveness asks whether
// restarting the process would help, which for a broker outage it does not: the
// sender is still draining the buffer, and a restart would abort that and start
// its backoff over for as long as the broker stays down.
func TestProbesWeighTheBrokerAndTheOutboxDifferently(t *testing.T) {
	const threshold = time.Minute

	// The consumer of both cases never ran, so it reports itself disconnected the
	// way it does while a broker is unreachable.
	t.Run("a broker outage never fails liveness", func(t *testing.T) {
		clock := &testClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
		mux := probeMux(t, openOutbox(t), clock, threshold)

		// Far past the threshold: the outage this leaves is the broker's alone.
		clock.advance(100 * threshold)

		if body, status := probe(t, mux, "/healthz"); status != http.StatusOK {
			t.Errorf("GET /healthz = %d, want %d however long the broker stays away (body %q)",
				status, http.StatusOK, body)
		}
		body, status := probe(t, mux, "/readyz")
		if status != http.StatusServiceUnavailable {
			t.Errorf("GET /readyz = %d, want %d while the consumer is disconnected (body %q)",
				status, http.StatusServiceUnavailable, body)
		}
	})

	// An outbox that has stopped answering is the other half: a restart reopens
	// the file, so liveness fails on it, but only once the outage has outlasted
	// the threshold.
	t.Run("an unusable outbox fails liveness once the threshold is past", func(t *testing.T) {
		clock := &testClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
		box := openOutbox(t)
		if err := box.Close(); err != nil {
			t.Fatalf("Close() error = %v, want nil", err)
		}
		mux := probeMux(t, box, clock, threshold)

		clock.advance(threshold / 2)
		if body, status := probe(t, mux, "/healthz"); status != http.StatusOK {
			t.Errorf("GET /healthz = %d, want %d inside the threshold (body %q)",
				status, http.StatusOK, body)
		}

		clock.advance(threshold)
		body, status := probe(t, mux, "/healthz")
		if status != http.StatusServiceUnavailable {
			t.Errorf("GET /healthz = %d, want %d once the outage outlasted the threshold (body %q)",
				status, http.StatusServiceUnavailable, body)
		}
		if !strings.Contains(body, "unhealthy for too long") {
			t.Errorf("GET /healthz body = %q, want it to say the outage outlasted the threshold", body)
		}
	})
}

// testClock is the time a tracker measures an outage against, moved by the test
// rather than by waiting.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// openOutbox opens a buffer on a fresh file. A test that wants an unusable one
// closes it, which is what an outbox that has stopped answering looks like to
// the probes.
func openOutbox(t *testing.T) *openstack.Outbox {
	t.Helper()

	box, err := openstack.OpenOutbox(filepath.Join(t.TempDir(), "outbox.db"))
	if err != nil {
		t.Fatalf("OpenOutbox() error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = box.Close() })
	return box
}

// probeMux builds the collector's routes over box and a consumer that was never
// run, with clock as the tracker's time. Metrics are off, which is the one route
// these cases do not exercise.
func probeMux(t *testing.T, box *openstack.Outbox, clock *testClock, threshold time.Duration) *http.ServeMux {
	t.Helper()

	cfg := openstack.Config{Cloud: "test"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newMux(cfg, openstack.NewConsumer(cfg, box, nil, logger), box, nil,
		health.New(clock.Now, threshold), logger)
}

// probe answers one probe request off the mux.
func probe(t *testing.T, mux *http.ServeMux, path string) (string, int) {
	t.Helper()

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Body.String(), recorder.Code
}

// TestRunServesTheScrapeRoute holds the assembled process to what the metrics
// switch decides: the registry the consumer and the sender record into is served
// under /metrics while the switch is on, and the route is gone while it is off.
// The broker is unreachable in both cases, which the exposition does not depend
// on.
func TestRunServesTheScrapeRoute(t *testing.T) {
	for name, tc := range map[string]struct {
		enabled string
		want    int
	}{
		"on by default":     {enabled: "", want: http.StatusOK},
		"off by the switch": {enabled: "false", want: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			port := freePort(t)
			env := serveEnv(t, port)
			env["TALLY_METRICS_ENABLED"] = tc.enabled
			setEnv(t, env)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(ctx, false) }()
			waitForHealthz(t, port, done)

			body, status := get(t, fmt.Sprintf("http://127.0.0.1:%d/metrics", port))

			if status != tc.want {
				t.Errorf("GET /metrics = %d, want %d (body %q)", status, tc.want, body)
			}
			if tc.want == http.StatusOK {
				if !strings.Contains(body, "go_goroutines") {
					t.Errorf("the body carries no go_goroutines series, want the exposition:\n%s", body)
				}
				if !strings.Contains(body, "tally_collector_delivered_total") {
					t.Errorf("the body carries no tally_collector_delivered_total series, "+
						"want the collector's own instruments:\n%s", body)
				}
			}

			cancel()
			assertShutdown(t, done)
		})
	}
}

func TestRunRefusesToStart(t *testing.T) {
	for _, name := range []string{
		"TALLY_OSC_AMQP_URL",
		"TALLY_OSC_CLOUD",
		"TALLY_OSC_REPORTING_URL",
		"TALLY_OSC_TOKEN",
		"TALLY_OSC_BUFFER_PATH",
	} {
		t.Run("when "+name+" is missing", func(t *testing.T) {
			env := serveEnv(t, freePort(t))
			env[name] = ""
			setEnv(t, env)

			assertRunFails(t, name)
		})
	}
}

// TestRunDumpsWithoutTheServeConfiguration pins the gate the dump mode runs
// under: it prints what the broker delivers and posts nothing, so it starts
// against the broker URL alone and asks for neither a cloud, nor a Reporting
// API, nor a buffer.
func TestRunDumpsWithoutTheServeConfiguration(t *testing.T) {
	setEnv(t, map[string]string{
		"TALLY_LOG_LEVEL":    "ERROR",
		"TALLY_OSC_AMQP_URL": closedBroker,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, true) }()

	// The dump serves no probe to wait for, so the cancellation comes after a
	// moment. A configuration gate would have refused it well before then, and a
	// refusal reaches the assertion below as an error rather than as a timeout.
	time.Sleep(50 * time.Millisecond)
	cancel()

	assertShutdown(t, done)
}

// assertRunFails checks that run refuses this configuration before it listens,
// and that the error says which variable to fix.
func assertRunFails(t *testing.T, want string) {
	t.Helper()

	err := run(context.Background(), false)
	if err == nil {
		t.Fatalf("run() error = nil, want one mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("run() error = %q, want it to mention %q", err, want)
	}
}

// assertShutdown waits for a cancelled run to return. An idle collector has
// nothing to drain, so it never needs the full budget: waiting for half of it
// separates a slow shutdown from a shutdown that never happens.
func assertShutdown(t *testing.T, done <-chan error) {
	t.Helper()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() error = %v, want nil after a cancelled context", err)
		}
	case <-time.After(shutdownTimeout / 2):
		t.Fatalf("run() did not return within %v of the cancellation", shutdownTimeout/2)
	}
}

// freePort reserves a port by taking one from the kernel and handing it back.
// It is the closest a test gets to naming a free port before the server binds
// it.
func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not a TCP address", listener.Addr())
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing the reserved port: %v", err)
	}
	return addr.Port
}

// get reads one URL off the running collector and returns the body and the
// status it answered with.
func get(t *testing.T, url string) (string, int) {
	t.Helper()

	resp, err := (&http.Client{Timeout: startupTimeout}).Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body of %s: %v", url, err)
	}
	return string(body), resp.StatusCode
}

// waitForHealthz polls the liveness probe until the collector answers it. A
// collector that just started is still inside its unhealthy threshold, so the
// probe reports 200 even though the broker behind it is unreachable.
func waitForHealthz(t *testing.T, port int, done <-chan error) {
	t.Helper()

	client := &http.Client{Timeout: startupTimeout}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)

	for deadline := time.Now().Add(startupTimeout); time.Now().Before(deadline); {
		select {
		case err := <-done:
			t.Fatalf("run() returned %v before the collector answered", err)
		default:
		}

		resp, err := client.Get(url)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /healthz = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		return
	}
	t.Fatalf("the collector did not answer on port %d within %v", port, startupTimeout)
}

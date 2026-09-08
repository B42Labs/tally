package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/console/config"
)

// startupTimeout is how long a test waits for the server to answer its first
// request. Binding a port takes milliseconds; the margin is for a loaded CI
// machine.
const startupTimeout = 10 * time.Second

// setEnv applies vars and blanks every other variable the console reads, so a
// test never inherits a value from the developer's shell. A variable set to the
// empty string falls back to its default exactly as an unset one does.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()

	for name := range vars {
		if !slices.Contains(config.EnvNames, name) {
			t.Fatalf("test sets %s, which the console does not read", name)
		}
	}
	for _, name := range config.EnvNames {
		t.Setenv(name, vars[name])
	}
}

// serverEnv is a configuration the console starts on. The engine database URL
// points at a port nothing listens on: the pool connects lazily, so the process
// starts and the pages reading that database answer 503 instead. The log level
// keeps those failures out of the test output.
func serverEnv(port int, reportingURL string) map[string]string {
	return map[string]string{
		"TALLY_LOG_LEVEL":             "ERROR",
		"TALLY_CONSOLE_HTTP_PORT":     strconv.Itoa(port),
		"TALLY_CONSOLE_REPORTING_URL": reportingURL,
		"TALLY_CONSOLE_API_TOKEN":     "test-token",
		"TALLY_CONSOLE_ENGINE_DB_URL": "postgres://tally:tally@127.0.0.1:1/tally_engine",
	}
}

func TestRunShutsDownWhenTheContextIsCancelled(t *testing.T) {
	port := freePort(t)
	setEnv(t, serverEnv(port, stubAPI(t).URL))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	waitForStylesheet(t, port, done)
	cancel()

	assertReturnsCleanly(t, done)
}

// TestRunServesTheConsoleOnTheLoopbackAddress holds the assembled process to
// the one thing its address decides: the pages are served to the machine the
// process runs on, and to nothing beyond it. The engine database is unreachable
// here, so the front page renders the error page for that read, which is still
// the console answering on the loopback address.
func TestRunServesTheConsoleOnTheLoopbackAddress(t *testing.T) {
	port := freePort(t)
	setEnv(t, serverEnv(port, stubAPI(t).URL))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	waitForStylesheet(t, port, done)

	body, status, contentType := get(t, fmt.Sprintf("http://127.0.0.1:%d/", port))

	if status != http.StatusServiceUnavailable {
		t.Errorf("GET / = %d, want %d (body %q)", status, http.StatusServiceUnavailable, body)
	}
	if !strings.HasPrefix(contentType, "text/html") {
		t.Errorf("GET / answered Content-Type %q, want it to start with text/html", contentType)
	}

	if address := externalIPv4(t); address != "" {
		// Nothing listens on that address, so the dial fails: refused, or
		// dropped by a firewall on the way, which the timeout bounds. A server
		// bound to 0.0.0.0 would answer here, and that is what this rules out.
		url := fmt.Sprintf("http://%s:%d/", address, port)
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get(url)
		if err == nil {
			// Drained before the close for the reason waitForStylesheet gives.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			t.Errorf("GET %s = %d, want a refused connection off the loopback address", url, resp.StatusCode)
		}
	} else {
		t.Log("this machine has no non-loopback IPv4 address, so only the loopback half is checked")
	}

	cancel()
	assertReturnsCleanly(t, done)
}

// TestRunReadsTheTokenFromItsFile follows one file-backed secret all the way to
// the wire: what the companion variable points at is what the Reporting API is
// called with, without the newline the file ends on.
func TestRunReadsTheTokenFromItsFile(t *testing.T) {
	api := stubAPI(t)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("writing the token file: %v", err)
	}

	port := freePort(t)
	env := serverEnv(port, api.URL)
	env["TALLY_CONSOLE_API_TOKEN"] = ""
	env["TALLY_CONSOLE_API_TOKEN_FILE"] = tokenFile
	setEnv(t, env)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	waitForStylesheet(t, port, done)

	get(t, fmt.Sprintf("http://127.0.0.1:%d/", port))

	if got, want := api.authorization(), "Bearer file-token"; got != want {
		t.Errorf("the API saw Authorization %q, want %q", got, want)
	}

	cancel()
	assertReturnsCleanly(t, done)
}

func TestRunRefusesToStart(t *testing.T) {
	// Nothing listens here. The subtests below fail before the console calls the
	// API, and a loopback host is what the configuration accepts without https.
	const unusedAPI = "http://127.0.0.1:1"

	t.Run("when the reporting URL is unset", func(t *testing.T) {
		env := serverEnv(freePort(t), unusedAPI)
		env["TALLY_CONSOLE_REPORTING_URL"] = ""
		setEnv(t, env)

		assertRunFails(t, "TALLY_CONSOLE_REPORTING_URL")
	})

	t.Run("when the reporting URL is plain http off the loopback", func(t *testing.T) {
		setEnv(t, serverEnv(freePort(t), "http://api.example.com"))

		assertRunFails(t, "TALLY_CONSOLE_API_TOKEN", "cleartext")
	})

	t.Run("when the CA file holds no certificate", func(t *testing.T) {
		caFile := filepath.Join(t.TempDir(), "ca.crt")
		if err := os.WriteFile(caFile, []byte("not a certificate\n"), 0o600); err != nil {
			t.Fatalf("writing the CA file: %v", err)
		}
		env := serverEnv(freePort(t), unusedAPI)
		env["TALLY_CONSOLE_CA_FILE"] = caFile
		setEnv(t, env)

		assertRunFails(t, caFile, "no PEM certificates")
	})

	t.Run("when the engine database URL is unset", func(t *testing.T) {
		env := serverEnv(freePort(t), unusedAPI)
		env["TALLY_CONSOLE_ENGINE_DB_URL"] = ""
		setEnv(t, env)

		assertRunFails(t, "TALLY_CONSOLE_ENGINE_DB_URL")
	})
}

// TestEnvExampleListsEveryVariable keeps the example file complete: a variable
// the console reads but nobody documents is one an operator finds out about
// from a failure.
func TestEnvExampleListsEveryVariable(t *testing.T) {
	example, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatalf("reading .env.example: %v", err)
	}

	for _, name := range config.EnvNames {
		if !strings.Contains(string(example), name) {
			t.Errorf(".env.example does not mention %s", name)
		}
	}
}

// apiStub is a Reporting API a test can point the console at. Every route
// answers an empty page, which is what the front page needs to get past its
// three API reads. The first Authorization header it sees is kept, because that
// is how a test reads back which token the process assembled itself with.
type apiStub struct {
	*httptest.Server

	mu   sync.Mutex
	auth string
}

// stubAPI starts that API on the loopback address and stops it with the test.
// Plain HTTP is what the configuration accepts for a loopback host: the token
// travels no further than this machine.
func stubAPI(t *testing.T) *apiStub {
	t.Helper()

	stub := &apiStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.record(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"next_cursor":null}`))
	}))
	t.Cleanup(stub.Close)
	return stub
}

// record keeps the header of the first request. A later one is dropped, so a
// page making three calls still reports what its first one carried.
func (s *apiStub) record(header string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.auth == "" {
		s.auth = header
	}
}

// authorization is the Authorization header of the first request the stub saw,
// and the empty string while it has seen none.
func (s *apiStub) authorization() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.auth
}

// assertReturnsCleanly waits for run to return after its context was cancelled.
func assertReturnsCleanly(t *testing.T, done <-chan error) {
	t.Helper()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() error = %v, want nil after a cancelled context", err)
		}
	case <-time.After(shutdownTimeout / 2):
		// An idle server has nothing to drain, so it never needs the full
		// budget. Waiting for half of it separates a slow shutdown from a
		// shutdown that never happens.
		t.Fatalf("run() did not return within %v of the cancellation", shutdownTimeout/2)
	}
}

// assertRunFails checks that run refuses this configuration before it listens,
// and that the error says what to fix. Several wants are read off the one
// refusal, so a message has to name all of them at once.
func assertRunFails(t *testing.T, wants ...string) {
	t.Helper()

	err := run(context.Background())
	if err == nil {
		t.Fatalf("run() error = nil, want one mentioning %q", wants)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("run() error = %q, want it to mention %q", err, want)
		}
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

// externalIPv4 is an IPv4 address of this machine that is not the loopback one,
// and the empty string on a machine that has none. Interfaces that are down are
// skipped: an address on one of them is not dialable either way, so it would
// prove nothing about where the console listens.
func externalIPv4(t *testing.T) string {
	t.Helper()

	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("listing the network interfaces: %v", err)
	}

	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			network, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ip := network.IP.To4(); ip != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	return ""
}

// get reads one URL off the running server and returns the body, the status it
// answered with, and the content type it declared.
func get(t *testing.T, url string) (string, int, string) {
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
	return string(body), resp.StatusCode, resp.Header.Get("Content-Type")
}

// waitForStylesheet polls the console's one asset until the server answers it.
// The stylesheet is served out of the binary, so it reports that the process
// listens without depending on either side it reads.
func waitForStylesheet(t *testing.T, port int, done <-chan error) {
	t.Helper()

	client := &http.Client{Timeout: startupTimeout}
	url := fmt.Sprintf("http://127.0.0.1:%d/static/console.css", port)

	for deadline := time.Now().Add(startupTimeout); time.Now().Before(deadline); {
		select {
		case err := <-done:
			t.Fatalf("run() returned %v before the server answered", err)
		default:
		}

		resp, err := client.Get(url)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		// The body is drained before the close, so the transport parks this
		// connection in its pool before the next request of the test starts.
		// A body closed unread is drained after Close returns, and a request
		// that dials while that drain runs ends up with two connections: the
		// parked one serves it, and the dialed one is pooled without ever
		// carrying a request, which the server then holds as new until
		// Shutdown gives up on it five seconds later.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /static/console.css = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		return
	}
	t.Fatalf("the server did not answer on port %d within %v", port, startupTimeout)
}

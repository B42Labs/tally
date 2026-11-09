package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/console/config"
)

// startupTimeout is how long a test waits for the server to answer its first
// request. Binding a port takes milliseconds; the margin is for a loaded CI
// machine.
const startupTimeout = 10 * time.Second

// The operator credential the running console is started with. The hash has the
// lowest cost the console accepts, so the sign-in does not slow the tests down.
const (
	testOperator = "test-operator"
	testPassword = "test-password"
	testHtpasswd = testOperator + ":$2y$10$TaXj3/P4n/MRdv90G7yxa.9R.9hbOAqM9LXxAeK83g8pUS4Px4Hr."
)

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
func serverEnv(ports consolePorts, reportingURL string) map[string]string {
	return map[string]string{
		"TALLY_LOG_LEVEL":             "ERROR",
		"TALLY_CONSOLE_HTTP_PORT":     strconv.Itoa(ports.page),
		"TALLY_CONSOLE_OPS_PORT":      strconv.Itoa(ports.ops),
		"TALLY_CONSOLE_REPORTING_URL": reportingURL,
		"TALLY_CONSOLE_API_TOKEN":     "test-token",
		"TALLY_CONSOLE_ENGINE_DB_URL": "postgres://tally:tally@127.0.0.1:1/tally_engine",
		"TALLY_CONSOLE_HTPASSWD":      testHtpasswd,
	}
}

func TestRunShutsDownWhenTheContextIsCancelled(t *testing.T) {
	ports := freePorts(t)
	cancel, done := startConsole(t, ports, serverEnv(ports, stubAPI(t).URL))
	cancel()

	assertReturnsCleanly(t, done)
	for _, port := range []int{ports.page, ports.ops} {
		if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
			_ = conn.Close()
			t.Errorf("port %d accepted a connection after the shutdown", port)
		}
	}
}

// TestRunServesTheConsoleOnTheLoopbackAddress holds the assembled process to
// the one thing its address decides: the pages are served to the machine the
// process runs on, and to nothing beyond it. The engine database is unreachable
// here, so the front page renders the error page for that read, which is still
// the console answering on the loopback address.
func TestRunServesTheConsoleOnTheLoopbackAddress(t *testing.T) {
	ports := freePorts(t)
	cancel, done := startConsole(t, ports, serverEnv(ports, stubAPI(t).URL))

	body, status, contentType := get(t, fmt.Sprintf("http://127.0.0.1:%d/", ports.page))

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
		url := fmt.Sprintf("http://%s:%d/", address, ports.page)
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

// TestRunRequiresTheCredential holds the assembled process to its sign-in,
// including for the one asset, which reveals nothing but still needs it.
func TestRunRequiresTheCredential(t *testing.T) {
	ports := freePorts(t)
	cancel, done := startConsole(t, ports, serverEnv(ports, stubAPI(t).URL))

	front := fmt.Sprintf("http://127.0.0.1:%d/", ports.page)
	stylesheet := fmt.Sprintf("http://127.0.0.1:%d/static/console.css", ports.page)

	cases := []struct {
		name           string
		url            string
		user, password string
		want           int
	}{
		{name: "the front page without the credential", url: front, want: http.StatusUnauthorized},
		{name: "the stylesheet without the credential", url: stylesheet, want: http.StatusUnauthorized},
		{
			name: "the stylesheet with a wrong password",
			url:  stylesheet, user: testOperator, password: "wrong-password",
			want: http.StatusUnauthorized,
		},
		{
			name: "the stylesheet with a wrong user",
			url:  stylesheet, user: "someone-else", password: testPassword,
			want: http.StatusUnauthorized,
		},
		{
			name: "the stylesheet with the credential",
			url:  stylesheet, user: testOperator, password: testPassword,
			want: http.StatusOK,
		},
	}

	for _, c := range cases {
		if _, status, _ := fetch(t, c.url, c.user, c.password); status != c.want {
			t.Errorf("%s = %d, want %d", c.name, status, c.want)
		}
	}

	cancel()
	assertReturnsCleanly(t, done)
}

// TestRunListensOnTheConfiguredAddress is the other half of what the loopback
// test rules out: an address that was asked for is the one the console binds.
func TestRunListensOnTheConfiguredAddress(t *testing.T) {
	ports := freePorts(t)
	env := serverEnv(ports, stubAPI(t).URL)
	env["TALLY_CONSOLE_HTTP_ADDR"] = "0.0.0.0"
	cancel, done := startConsole(t, ports, env)

	if address := externalIPv4(t); address != "" {
		assertStylesheetAnswersOn(t, address, ports.page)
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

	ports := freePorts(t)
	env := serverEnv(ports, api.URL)
	env["TALLY_CONSOLE_API_TOKEN"] = ""
	env["TALLY_CONSOLE_API_TOKEN_FILE"] = tokenFile
	cancel, done := startConsole(t, ports, env)

	get(t, fmt.Sprintf("http://127.0.0.1:%d/", ports.page))

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
		env := serverEnv(freePorts(t), unusedAPI)
		env["TALLY_CONSOLE_REPORTING_URL"] = ""
		setEnv(t, env)

		assertRunFails(t, "TALLY_CONSOLE_REPORTING_URL")
	})

	t.Run("when the reporting URL is plain http off the loopback", func(t *testing.T) {
		setEnv(t, serverEnv(freePorts(t), "http://api.example.com"))

		assertRunFails(t, "TALLY_CONSOLE_API_TOKEN", "cleartext")
	})

	t.Run("when the CA file holds no certificate", func(t *testing.T) {
		caFile := filepath.Join(t.TempDir(), "ca.crt")
		if err := os.WriteFile(caFile, []byte("not a certificate\n"), 0o600); err != nil {
			t.Fatalf("writing the CA file: %v", err)
		}
		env := serverEnv(freePorts(t), unusedAPI)
		env["TALLY_CONSOLE_CA_FILE"] = caFile
		setEnv(t, env)

		assertRunFails(t, caFile, "no PEM certificates")
	})

	t.Run("when the engine database URL is unset", func(t *testing.T) {
		env := serverEnv(freePorts(t), unusedAPI)
		env["TALLY_CONSOLE_ENGINE_DB_URL"] = ""
		setEnv(t, env)

		assertRunFails(t, "TALLY_CONSOLE_ENGINE_DB_URL")
	})

	t.Run("when the operator credential is unset", func(t *testing.T) {
		env := serverEnv(freePorts(t), unusedAPI)
		env["TALLY_CONSOLE_HTPASSWD"] = ""
		setEnv(t, env)

		assertRunFails(t, "TALLY_CONSOLE_HTPASSWD")
	})

	t.Run("when the operator credential holds a plain password", func(t *testing.T) {
		env := serverEnv(freePorts(t), unusedAPI)
		env["TALLY_CONSOLE_HTPASSWD"] = "operator:a-plain-password"
		setEnv(t, env)

		assertRunFails(t, "TALLY_CONSOLE_HTPASSWD", "line 1")
	})
}

// TestRunServesProbesWithoutEitherSide holds both probes to reporting the process
// alone: the engine database is unreachable, and the Reporting API goes away
// before they are asked.
func TestRunServesProbesWithoutEitherSide(t *testing.T) {
	ports := freePorts(t)
	stub := stubAPI(t)
	cancel, done := startConsole(t, ports, serverEnv(ports, stub.URL))
	stub.Close()

	for _, path := range []string{"/healthz", "/readyz"} {
		body, status, contentType := fetch(t, ports.opsURL(path), "", "")
		if status != http.StatusOK || body != "ok" {
			t.Errorf("GET %s = %d %q, want %d %q", path, status, body, http.StatusOK, "ok")
		}
		if want := "text/plain; charset=utf-8"; contentType != want {
			t.Errorf("GET %s answered Content-Type %q, want %q", path, contentType, want)
		}
	}

	cancel()
	assertReturnsCleanly(t, done)
}

// TestRunCountsOnTheOpsPort follows one refused and a known number of signed-in
// requests from the page port to the exposition.
func TestRunCountsOnTheOpsPort(t *testing.T) {
	ports := freePorts(t)
	cancel, done := startConsole(t, ports, serverEnv(ports, stubAPI(t).URL))

	stylesheet := ports.pageURL("/static/console.css")
	if _, status, _ := fetch(t, stylesheet, "", ""); status != http.StatusUnauthorized {
		t.Fatalf("the stylesheet without the credential = %d, want %d", status, http.StatusUnauthorized)
	}
	const signedIn = 2
	for range signedIn {
		if _, status, _ := get(t, stylesheet); status != http.StatusOK {
			t.Fatalf("the stylesheet with the credential = %d, want %d", status, http.StatusOK)
		}
	}

	body, status, _ := fetch(t, ports.opsURL("/metrics"), "", "")
	if status != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d", status, http.StatusOK)
	}
	// waitForStylesheet made one signed-in request of its own.
	for _, want := range []string{
		`tally_console_sign_in_refusals_total{reason="no_credential"} 1`,
		fmt.Sprintf(`tally_console_requests_total{route="/static/console.css",status="200"} %d`, signedIn+1),
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("the exposition does not carry %q:\n%s", want, body)
		}
	}

	cancel()
	assertReturnsCleanly(t, done)
}

func TestRunAnswers404ForMetricsThatAreSwitchedOff(t *testing.T) {
	ports := freePorts(t)
	env := serverEnv(ports, stubAPI(t).URL)
	env["TALLY_METRICS_ENABLED"] = "false"
	cancel, done := startConsole(t, ports, env)

	if _, status, _ := fetch(t, ports.opsURL("/metrics"), "", ""); status != http.StatusNotFound {
		t.Errorf("GET /metrics = %d, want %d", status, http.StatusNotFound)
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		if _, status, _ := fetch(t, ports.opsURL(path), "", ""); status != http.StatusOK {
			t.Errorf("GET %s = %d, want %d", path, status, http.StatusOK)
		}
	}

	cancel()
	assertReturnsCleanly(t, done)
}

// TestRunKeepsTheTwoPortsApart holds each listener to what it serves: the page
// port has no probe, and the ops port has no page and asks for no credential.
func TestRunKeepsTheTwoPortsApart(t *testing.T) {
	ports := freePorts(t)
	cancel, done := startConsole(t, ports, serverEnv(ports, stubAPI(t).URL))

	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		if _, status, _ := fetch(t, ports.pageURL(path), "", ""); status != http.StatusUnauthorized {
			t.Errorf("the page port for %s without the credential = %d, want %d", path, status, http.StatusUnauthorized)
		}
		if _, status, _ := get(t, ports.pageURL(path)); status != http.StatusNotFound {
			t.Errorf("the page port for %s with the credential = %d, want %d", path, status, http.StatusNotFound)
		}
	}
	for _, path := range []string{"/", "/static/console.css"} {
		_, status, header := fetchHeaders(t, ports.opsURL(path), "", "")
		if status != http.StatusNotFound {
			t.Errorf("the ops port for %s = %d, want %d", path, status, http.StatusNotFound)
		}
		if got := header.Get("WWW-Authenticate"); got != "" {
			t.Errorf("the ops port for %s asks for a credential: %q", path, got)
		}
	}

	cancel()
	assertReturnsCleanly(t, done)
}

func TestRunSendsTheSecurityHeadersWithTheFrontPage(t *testing.T) {
	ports := freePorts(t)
	cancel, done := startConsole(t, ports, serverEnv(ports, stubAPI(t).URL))

	_, _, header := fetchHeaders(t, ports.pageURL("/"), testOperator, testPassword)

	for name, want := range map[string]string{
		"Cache-Control":           "no-store",
		"Content-Security-Policy": "default-src 'none'; style-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'",
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	cancel()
	assertReturnsCleanly(t, done)
}

// TestRunBindsTheOpsPortOnTheConfiguredAddress is what the page port's two
// address tests are for the second listener.
func TestRunBindsTheOpsPortOnTheConfiguredAddress(t *testing.T) {
	address := externalIPv4(t)
	if address == "" {
		t.Skip("this machine has no non-loopback IPv4 address")
	}

	t.Run("the default address keeps it off the network", func(t *testing.T) {
		ports := freePorts(t)
		cancel, done := startConsole(t, ports, serverEnv(ports, stubAPI(t).URL))

		target := net.JoinHostPort(address, strconv.Itoa(ports.ops))
		conn, err := net.DialTimeout("tcp", target, 2*time.Second)
		var netErr net.Error
		switch {
		case err == nil:
			_ = conn.Close()
			t.Errorf("%s accepted a connection off the loopback address", target)
		case errors.As(err, &netErr) && netErr.Timeout():
			t.Logf("a firewall holds up connections to %s, so a refusal cannot be told apart", address)
		}

		cancel()
		assertReturnsCleanly(t, done)
	})

	t.Run("a wildcard address publishes it", func(t *testing.T) {
		ports := freePorts(t)
		env := serverEnv(ports, stubAPI(t).URL)
		env["TALLY_CONSOLE_HTTP_ADDR"] = "0.0.0.0"
		cancel, done := startConsole(t, ports, env)

		url := fmt.Sprintf("http://%s:%d/readyz", address, ports.ops)
		assertAnswersOn(t, newRequest(t, url, "", ""), address)

		cancel()
		assertReturnsCleanly(t, done)
	})
}

// TestRunRefusesAPortThatIsTaken holds each listener to failing at startup. The
// context ends a run that wrongly serves, which the nil error then reports.
func TestRunRefusesAPortThatIsTaken(t *testing.T) {
	holdPort := func(t *testing.T, port int) {
		t.Helper()

		held, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatalf("holding port %d: %v", port, err)
		}
		t.Cleanup(func() { _ = held.Close() })
	}
	runBriefly := func(t *testing.T) error {
		t.Helper()

		ctx, cancel := context.WithTimeout(t.Context(), startupTimeout)
		defer cancel()
		return run(ctx)
	}

	t.Run("the ops port", func(t *testing.T) {
		ports := freePorts(t)
		holdPort(t, ports.ops)
		setEnv(t, serverEnv(ports, stubAPI(t).URL))

		err := runBriefly(t)

		if err == nil || !strings.Contains(err.Error(), "listening for the probes and metrics") {
			t.Fatalf("run() error = %v, want one naming the probes and metrics", err)
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			t.Errorf("run() error = %v, want one wrapping EADDRINUSE", err)
		}
		// The page port was bound first, and a failed start hands it back.
		released, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", ports.page))
		if err != nil {
			t.Fatalf("binding the page port after the failed start: %v", err)
		}
		_ = released.Close()
	})

	t.Run("the page port", func(t *testing.T) {
		ports := freePorts(t)
		holdPort(t, ports.page)
		setEnv(t, serverEnv(ports, stubAPI(t).URL))

		if err := runBriefly(t); err == nil || !strings.Contains(err.Error(), "listening for the pages") {
			t.Fatalf("run() error = %v, want one naming the pages", err)
		}
	})
}

func TestRunTakesAPlaintextURLOnlyWithTheSwitch(t *testing.T) {
	const url = "http://reporting-api.invalid"

	t.Run("with the switch it starts", func(t *testing.T) {
		ports := freePorts(t)
		env := serverEnv(ports, url)
		env["TALLY_CONSOLE_REPORTING_INSECURE"] = "true"

		cancel, done := startConsole(t, ports, env)

		cancel()
		assertReturnsCleanly(t, done)
	})

	t.Run("without it the error names the switch", func(t *testing.T) {
		setEnv(t, serverEnv(freePorts(t), url))

		assertRunFails(t, "TALLY_CONSOLE_REPORTING_INSECURE")
	})
}

// closeFailsListener is a listener whose Close reports a failure, which is the
// one way to make Shutdown fail without holding a request for its whole budget.
type closeFailsListener struct{ net.Listener }

var errCloseFailed = errors.New("closing the listener failed")

func (l closeFailsListener) Close() error {
	_ = l.Listener.Close()
	return errCloseFailed
}

func TestAwaitStop(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	idle := func() *http.Server { return newServer(http.NotFoundHandler()) }

	t.Run("a cancelled context returns nil", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if err := awaitStop(ctx, logger, make(chan error, 2), idle(), idle()); err != nil {
			t.Errorf("awaitStop() error = %v, want nil", err)
		}
	})

	t.Run("a server that was closed is no failure", func(t *testing.T) {
		serveErr := make(chan error, 2)
		serveErr <- http.ErrServerClosed

		if err := awaitStop(t.Context(), logger, serveErr, idle(), idle()); err != nil {
			t.Errorf("awaitStop() error = %v, want nil", err)
		}
	})

	t.Run("a server that ended on its own is reported", func(t *testing.T) {
		broken := errors.New("the listener broke")
		serveErr := make(chan error, 2)
		serveErr <- broken

		err := awaitStop(t.Context(), logger, serveErr, idle(), idle())

		if !errors.Is(err, broken) || !strings.Contains(err.Error(), "serving HTTP") {
			t.Errorf("awaitStop() error = %v, want one wrapping the failure as serving HTTP", err)
		}
	})

	// failsToShutDown is a server whose Shutdown fails, serving until then. The
	// channel carries what Serve returned.
	failsToShutDown := func(t *testing.T) (*http.Server, <-chan error) {
		t.Helper()

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listening: %v", err)
		}
		failing := idle()
		served := make(chan error, 1)
		go func() { served <- failing.Serve(closeFailsListener{listener}) }()

		// A request that was answered proves the server tracks its listener.
		client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: startupTimeout}
		resp, err := client.Get("http://" + listener.Addr().String() + "/")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return failing, served
	}

	t.Run("a failed shutdown is reported", func(t *testing.T) {
		failing, served := failsToShutDown(t)

		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := awaitStop(ctx, logger, make(chan error, 2), failing, idle())

		if !errors.Is(err, errCloseFailed) || !strings.Contains(err.Error(), "shutting down") {
			t.Errorf("awaitStop() error = %v, want one wrapping the failure as shutting down", err)
		}
		<-served
	})

	t.Run("a serving failure and a failed shutdown are both reported", func(t *testing.T) {
		failing, served := failsToShutDown(t)
		broken := errors.New("the listener broke")
		serveErr := make(chan error, 2)
		serveErr <- broken

		err := awaitStop(t.Context(), logger, serveErr, failing, idle())

		if !errors.Is(err, broken) || !errors.Is(err, errCloseFailed) {
			t.Errorf("awaitStop() error = %v, want one wrapping both failures", err)
		}
		<-served
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

// consolePorts are the two ports one running console listens on.
type consolePorts struct {
	page, ops int
}

func (p consolePorts) pageURL(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", p.page, path)
}

func (p consolePorts) opsURL(path string) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", p.ops, path)
}

// freePorts reserves two distinct ports. The first stays held while the second
// is taken, because two calls of freePort can return one port twice, which the
// configuration refuses.
func freePorts(t *testing.T) consolePorts {
	t.Helper()

	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer func() { _ = held.Close() }()

	addr, ok := held.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address %v is not a TCP address", held.Addr())
	}
	return consolePorts{page: addr.Port, ops: freePort(t)}
}

// startConsole runs the console on env and returns once it answers. The
// returned function cancels the run, and the channel carries what run returned.
func startConsole(t *testing.T, ports consolePorts, env map[string]string) (context.CancelFunc, <-chan error) {
	t.Helper()

	setEnv(t, env)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	waitForStylesheet(t, ports.page, done)
	return cancel, done
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

// assertStylesheetAnswersOn asks an address of this machine for the stylesheet.
// The server has already answered on the loopback address, so a request that
// times out here was held up by a host firewall, which does that to a binary it
// does not know and says nothing about what the console binds. A refused
// connection does say, and fails the test.
func assertStylesheetAnswersOn(t *testing.T, address string, port int) {
	t.Helper()

	url := fmt.Sprintf("http://%s:%d/static/console.css", address, port)
	assertAnswersOn(t, newRequest(t, url, testOperator, testPassword), address)
}

// assertAnswersOn sends one request to an address of this machine and wants a
// 200, with the firewall allowance assertStylesheetAnswersOn describes.
func assertAnswersOn(t *testing.T, req *http.Request, address string) {
	t.Helper()

	url := req.URL.String()
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Logf("a firewall holds up connections to %s, so only the loopback half is checked", address)
		return
	}
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	// Drained before the close for the reason waitForStylesheet gives.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET %s = %d, want %d", url, resp.StatusCode, http.StatusOK)
	}
}

// get reads one URL off the running server as the operator.
func get(t *testing.T, url string) (string, int, string) {
	t.Helper()

	return fetch(t, url, testOperator, testPassword)
}

// newRequest builds a GET for url. An empty user sends no credential at all.
func newRequest(t *testing.T, url, user, password string) *http.Request {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building the request for %s: %v", url, err)
	}
	if user != "" {
		req.SetBasicAuth(user, password)
	}
	return req
}

// fetch reads one URL off the running server and returns the body, the status
// it answered with, and the content type it declared.
func fetch(t *testing.T, url, user, password string) (string, int, string) {
	t.Helper()

	body, status, header := fetchHeaders(t, url, user, password)
	return body, status, header.Get("Content-Type")
}

// fetchHeaders is fetch with every header of the answer.
func fetchHeaders(t *testing.T, url, user, password string) (string, int, http.Header) {
	t.Helper()

	resp, err := (&http.Client{Timeout: startupTimeout}).Do(newRequest(t, url, user, password))
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body of %s: %v", url, err)
	}
	return string(body), resp.StatusCode, resp.Header
}

// waitForStylesheet polls the console's one asset until the server answers it.
// The stylesheet is served out of the binary, so it reports that the process
// listens without depending on either side it reads.
func waitForStylesheet(t *testing.T, port int, done <-chan error) {
	t.Helper()

	client := &http.Client{Timeout: startupTimeout}
	url := fmt.Sprintf("http://127.0.0.1:%d/static/console.css", port)
	req := newRequest(t, url, testOperator, testPassword)

	for deadline := time.Now().Add(startupTimeout); time.Now().Before(deadline); {
		select {
		case err := <-done:
			t.Fatalf("run() returned %v before the server answered", err)
		default:
		}

		resp, err := client.Do(req)
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

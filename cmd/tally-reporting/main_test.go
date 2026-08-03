package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/reporting/config"
)

// startupTimeout is how long a test waits for the server to answer its first
// request. Binding a port takes milliseconds; the margin is for a loaded CI
// machine.
const startupTimeout = 10 * time.Second

// setEnv applies vars and blanks every other variable the server reads, so a
// test never inherits a value from the developer's shell. A variable set to the
// empty string falls back to its default exactly as an unset one does.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()

	for name := range vars {
		if !slices.Contains(config.EnvNames, name) {
			t.Fatalf("test sets %s, which the server does not read", name)
		}
	}
	for _, name := range config.EnvNames {
		t.Setenv(name, vars[name])
	}
}

// serverEnv is a configuration the server starts on. The database URL points at
// a port nothing listens on: the pool connects lazily, so the process starts
// and the probes report the outage instead. The log level keeps the probe's
// warnings about that unreachable database out of the test output.
func serverEnv(port int) map[string]string {
	return map[string]string{
		"TALLY_LOG_LEVEL":                "ERROR",
		"TALLY_REPORTING_HTTP_PORT":      strconv.Itoa(port),
		"TALLY_REPORTING_DB_URL":         "postgres://tally:tally@127.0.0.1:1/tally",
		"TALLY_REPORTING_INTERNAL_TOKEN": "test-token",
	}
}

func TestRunShutsDownWhenTheContextIsCancelled(t *testing.T) {
	port := freePort(t)
	setEnv(t, serverEnv(port))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	waitForHealthz(t, port, done)
	cancel()

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

func TestRunRefusesToStart(t *testing.T) {
	t.Run("when the OIDC JWKS URL names an unimplemented provider", func(t *testing.T) {
		env := serverEnv(freePort(t))
		env["TALLY_REPORTING_OIDC_JWKS_URL"] = "https://idp.example.com/jwks.json"
		setEnv(t, env)

		assertRunFails(t, "not implemented")
	})

	t.Run("when enforced authentication has no internal token", func(t *testing.T) {
		env := serverEnv(freePort(t))
		env["TALLY_REPORTING_AUTH_MODE"] = "enforced"
		env["TALLY_REPORTING_INTERNAL_TOKEN"] = ""
		setEnv(t, env)

		assertRunFails(t, "TALLY_REPORTING_INTERNAL_TOKEN")
	})

	t.Run("when the database URL is missing", func(t *testing.T) {
		env := serverEnv(freePort(t))
		env["TALLY_REPORTING_DB_URL"] = ""
		setEnv(t, env)

		assertRunFails(t, "TALLY_REPORTING_DB_URL")
	})
}

// assertRunFails checks that run refuses this configuration before it listens,
// and that the error says which variable to fix.
func assertRunFails(t *testing.T, want string) {
	t.Helper()

	err := run(context.Background())
	if err == nil {
		t.Fatalf("run() error = nil, want one mentioning %q", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("run() error = %q, want it to mention %q", err, want)
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

// waitForHealthz polls the liveness probe until the server answers it. A server
// that just started is still inside its unhealthy threshold, so the probe
// reports 200 even though the database behind it is unreachable.
func waitForHealthz(t *testing.T, port int, done <-chan error) {
	t.Helper()

	client := &http.Client{Timeout: startupTimeout}
	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)

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
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /healthz = %d, want %d", resp.StatusCode, http.StatusOK)
		}
		return
	}
	t.Fatalf("the server did not answer on port %d within %v", port, startupTimeout)
}

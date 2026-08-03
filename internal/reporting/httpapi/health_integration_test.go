package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// TestReadyzAgainstARealDatabase drives readiness through a database that
// really goes away and comes back, which is the state the probe exists for and
// the one a fake database cannot produce.
func TestReadyzAgainstARealDatabase(t *testing.T) {
	db := storetest.NewDB(t)
	ctx := t.Context()

	handler, err := NewRouter(Options{
		Logger:             slog.New(slog.NewJSONHandler(io.Discard, nil)),
		DB:                 containerPinger(t, db),
		UnhealthyThreshold: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewRouter() error = %v, want nil", err)
	}

	t.Run("is ready while the database runs", func(t *testing.T) {
		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/readyz", nil))

		if got := rec.Code; got != http.StatusOK {
			t.Fatalf("status = %d, want %d (body %q)", got, http.StatusOK, rec.Body)
		}
		if got := rec.Body.String(); got != "ok" {
			t.Errorf("body = %q, want %q", got, "ok")
		}
	})

	t.Run("is unready once the database is gone", func(t *testing.T) {
		// How long the database gets to shut down cleanly.
		stopTimeout := 10 * time.Second
		if err := db.Container.Stop(ctx, &stopTimeout); err != nil {
			t.Fatalf("stopping the database container: %v", err)
		}

		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/readyz", nil))

		assertProblem(t, rec, http.StatusServiceUnavailable, "urn:tally:error:unavailable")
	})

	t.Run("is ready again once the database is back", func(t *testing.T) {
		if err := db.Container.Start(ctx); err != nil {
			t.Fatalf("starting the database container again: %v", err)
		}

		// Postgres needs a moment to accept connections after the restart, and
		// the container comes back on a different host port.
		waitForReady(t, handler, time.Minute)
	})
}

// containerPinger checks the database at the address the container has right
// now. A restarted container is published on a new host port, so a pool built
// from the original URL would never reconnect: this resolves the endpoint on
// every ping instead.
func containerPinger(t *testing.T, db storetest.DB) DB {
	t.Helper()

	// The credentials stay the same across a restart; only the host port moves.
	base, err := url.Parse(db.URL)
	if err != nil {
		t.Fatalf("parsing the database URL %q: %v", db.URL, err)
	}

	return pingerFunc(func(ctx context.Context) error {
		host, err := db.Container.Host(ctx)
		if err != nil {
			return fmt.Errorf("reading the container host: %w", err)
		}
		port, err := db.Container.MappedPort(ctx, storetest.ContainerPort)
		if err != nil {
			return fmt.Errorf("reading the mapped database port: %w", err)
		}

		current := *base
		current.Host = net.JoinHostPort(host, port.Port())

		conn, err := pgx.Connect(ctx, current.String())
		if err != nil {
			return fmt.Errorf("connecting to the database: %w", err)
		}
		defer func() { _ = conn.Close(ctx) }()

		return conn.Ping(ctx)
	})
}

// waitForReady polls readiness until it passes or the deadline runs out.
func waitForReady(t *testing.T, handler http.Handler, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	for {
		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code == http.StatusOK {
			if got := rec.Body.String(); got != "ok" {
				t.Errorf("body = %q, want %q", got, "ok")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %d after waiting %v, want %d (body %q)",
				rec.Code, within, http.StatusOK, rec.Body)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Command tally-console serves the read-only console.
//
// It reads its configuration from the environment, refuses a configuration it
// cannot honor, and then serves the console's pages on the configured address
// and port. Every page needs the operator credential, because the process holds
// a read_all token and a connection as a member of tally_console_reader, and
// whoever signs in reads everything both of them read.
//
// A second listener on the same address serves /healthz, /readyz and /metrics
// without a credential, for a kubelet and a scraper. It has a port of its own
// so that a route publishing the pages cannot reach them.
//
// The pages come from two places: the Reporting API over HTTP, and the engine
// database, which is opened for reads only. Its pool connects lazily, so the
// process comes up while the database is unavailable and the pages that read it
// report the outage.
//
// SIGINT and SIGTERM begin a graceful shutdown: both listeners stop accepting
// connections, in-flight requests get a bounded budget to finish, and the
// process exits zero.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/b42labs/tally/internal/console/config"
	"github.com/b42labs/tally/internal/console/httpui"
	"github.com/b42labs/tally/internal/console/metrics"
	"github.com/b42labs/tally/internal/console/reporting"
	"github.com/b42labs/tally/internal/console/signin"
	"github.com/b42labs/tally/internal/console/store"
)

const (
	// readHeaderTimeout bounds how long a client may take to send its request
	// headers, which is what keeps a stalled connection from holding a slot.
	readHeaderTimeout = 5 * time.Second
	// readTimeout bounds the whole request, headers and body together.
	readTimeout = 30 * time.Second
	// writeTimeout bounds the response. Without it a client that stops reading
	// holds its handler goroutine for as long as it likes.
	writeTimeout = 60 * time.Second
	// idleTimeout bounds a kept-alive connection between requests. Go derives it
	// from readTimeout when it is zero, and a zero readTimeout clears the read
	// deadline entirely: idle connections would then pile up until the process
	// runs out of file descriptors.
	idleTimeout = 120 * time.Second
	// shutdownTimeout is what in-flight requests get once a signal arrives. In a
	// pod it stays inside the kubelet's default grace period of 30 seconds, so
	// the pages still rendering finish before the kubelet kills the process.
	shutdownTimeout = 10 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		// The configured logger may not exist yet, because loading the
		// configuration is the first thing that can fail.
		slog.Error("tally-console stopped", "error", err)
		os.Exit(1)
	}
}

// run assembles the console and serves it until ctx is cancelled or a server
// stops on its own. It returns nil for a shutdown that completed, so a signal
// leaves a zero exit status, and an error for everything else.
func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading the configuration: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	})).With("service", "tally-console")

	promReg := prometheus.NewRegistry()
	m := metrics.New(promReg)

	basic, err := signin.NewBasic(cfg.Htpasswd, logger, m.SignInRefused)
	if err != nil {
		return fmt.Errorf("TALLY_CONSOLE_HTPASSWD: %w", err)
	}

	api, err := reporting.New(cfg.ReportingURL, cfg.APIToken, cfg.CAFile)
	if err != nil {
		return fmt.Errorf("building the reporting client: %w", err)
	}

	db, err := store.New(ctx, cfg.EngineDBURL, logger)
	if err != nil {
		return fmt.Errorf("opening the engine database pool: %w", err)
	}
	defer db.Close()

	router, err := httpui.NewRouter(httpui.Options{Logger: logger, SignIn: basic.Require, Metrics: m, API: api, Store: db})
	if err != nil {
		return fmt.Errorf("building the router: %w", err)
	}

	pageAddr := net.JoinHostPort(cfg.HTTPAddr, strconv.Itoa(cfg.HTTPPort))
	opsAddr := net.JoinHostPort(cfg.HTTPAddr, strconv.Itoa(cfg.OpsPort))

	// Both ports are bound before either server serves. That makes a taken port
	// a startup error, and it is what makes /readyz true: the probe answers only
	// once both are bound.
	pageListener, err := net.Listen("tcp", pageAddr)
	if err != nil {
		return fmt.Errorf("listening for the pages on %s: %w", pageAddr, err)
	}
	opsListener, err := net.Listen("tcp", opsAddr)
	if err != nil {
		_ = pageListener.Close()
		return fmt.Errorf("listening for the probes and metrics on %s: %w", opsAddr, err)
	}

	pages := newServer(router)
	ops := newServer(newOpsMux(cfg.MetricsEnabled, m))

	// The channel holds one result per server, so that a serving goroutine ends
	// even when the shutdown path has stopped reading from it.
	serveErr := make(chan error, 2)
	url := "http://" + pageAddr + "/"
	opsURL := "http://" + opsAddr + "/"
	logger.Info("listening", "url", url, "ops_url", opsURL)
	go func() { serveErr <- pages.Serve(pageListener) }()
	go func() { serveErr <- ops.Serve(opsListener) }()

	return awaitStop(ctx, logger, serveErr, pages, ops)
}

// newServer builds one server with the timeouts every listener of the console
// runs under.
func newServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
}

// awaitStop waits for ctx to end or a server to stop on its own, and then shuts
// both servers down, the pages first. A server that ended with anything but
// http.ErrServerClosed and a shutdown that failed are both reported, so neither
// hides the other.
func awaitStop(
	ctx context.Context, logger *slog.Logger, serveErr <-chan error, pages, ops *http.Server,
) error {
	var served error
	select {
	case served = <-serveErr:
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	// The shutdown runs on its own context: ctx may be cancelled already, and the
	// budget belongs to the in-flight requests, not to the signal.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	pagesErr := pages.Shutdown(shutdownCtx)
	opsErr := ops.Shutdown(shutdownCtx)

	var failed error
	if served != nil && !errors.Is(served, http.ErrServerClosed) {
		failed = fmt.Errorf("serving HTTP: %w", served)
	}
	if err := errors.Join(pagesErr, opsErr); err != nil {
		failed = errors.Join(failed, fmt.Errorf("shutting down: %w", err))
	}
	return failed
}

// newOpsMux builds the three routes of the second listener. None of them
// carries a credential, which is what the conventions ask of every service
// (roadmap/00-conventions.md section 7). The listener has no access log either,
// because a probe every few seconds would bury the request lines.
func newOpsMux(metricsEnabled bool, m *metrics.Metrics) *http.ServeMux {
	mux := http.NewServeMux()

	// A probe answers in plain text. What reads it is an orchestrator that looks
	// at the status, so there is no error format to match.
	probe := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	}

	// Liveness reads neither the engine database nor the Reporting API, because
	// restarting the pod mends neither side.
	mux.HandleFunc("GET /healthz", probe)

	// Readiness reads neither side either. Taking the pod out of the Service
	// would replace the error page that names the failed side with the Gateway's
	// own 503, and restarting the pod mends neither side.
	mux.HandleFunc("GET /readyz", probe)

	// The switch decides the route and not the recording: the instruments keep
	// counting either way. A console serving no metrics answers 404 rather than
	// an empty exposition, which would read as a console nobody opened.
	if metricsEnabled {
		mux.Handle("GET /metrics", m.Handler())
	} else {
		mux.HandleFunc("GET /metrics", http.NotFound)
	}

	return mux
}

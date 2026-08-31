// Command tally-reporting serves the Reporting API.
//
// It reads its configuration from the environment, refuses a configuration it
// cannot honor, and then serves the routes of the OpenAPI contract on the
// configured port. Its database pool connects lazily, so the process comes up
// while TimescaleDB is unavailable and reports that state through the probes
// rather than through a failed start.
//
// SIGINT and SIGTERM begin a graceful shutdown: the server stops accepting
// connections, in-flight requests get a bounded budget to finish, and the
// process exits zero.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/config"
	"github.com/b42labs/tally/internal/reporting/httpapi"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/metrics"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/reconciliation/adapters"
	"github.com/b42labs/tally/internal/reporting/registry"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
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
	// shutdownTimeout is what in-flight requests get once a signal arrives. It
	// stays under the grace period Kubernetes gives a terminating pod, so the
	// process ends on its own terms rather than on SIGKILL.
	shutdownTimeout = 10 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil {
		// The configured logger may not exist yet, because loading the
		// configuration is the first thing that can fail.
		slog.Error("tally-reporting stopped", "error", err)
		os.Exit(1)
	}
}

// run assembles the service and serves it until ctx is cancelled or the server
// stops on its own. It returns nil for a shutdown that completed, so a signal
// leaves a zero exit status, and an error for everything else.
func run(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading the configuration: %w", err)
	}
	if err := cfg.ValidateServer(); err != nil {
		return fmt.Errorf("checking the configuration: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	})).With("service", "tally-reporting")

	if auth.Mode(cfg.AuthMode) == auth.ModeDisabled {
		logger.Warn("authentication is disabled, every request is served without a credential")
	}

	db, err := store.New(ctx, cfg.DBURL, cfg.DBMaxConns)
	if err != nil {
		return fmt.Errorf("opening the database pool: %w", err)
	}
	defer db.Close()

	queries := sqlcgen.New(db.Pool())
	// One Prometheus registry for the process. Every component that records gets
	// the same Metrics value, so what a batch counts and what a sync run counts
	// land in the instruments one scrape reads.
	promReg := prometheus.NewRegistry()
	m := metrics.New(promReg)

	// The pipeline owns the registry, which caches compiled size schemas for the
	// life of the process. Nothing else validates a size against a stored schema
	// today.
	pipeline := ingest.New(registry.New(), cfg.RequireSizeSchema, nil, m)

	// The clouds file is read here rather than per sync run, so a broken one
	// refuses the process instead of failing every request that reaches the sync
	// endpoint. The registry carries the OpenStack adapter, and LoadConfig checks
	// every configured cloud's adapter name and platform against it at startup:
	// a cloud that names an unregistered adapter, or one that observes a
	// different platform than the cloud declares, stops the process before it
	// listens.
	adapterRegistry := map[string]reconciliation.Adapter{
		"openstack": adapters.NewOpenStack(logger),
	}
	cloudsCfg, err := reconciliation.LoadConfig(cfg.CloudsConfigPath, adapterRegistry)
	if err != nil {
		return fmt.Errorf("loading the clouds config: %w", err)
	}
	syncer := reconciliation.New(db, pipeline, cloudsCfg, adapterRegistry, time.Now, m)

	router, err := httpapi.NewRouter(httpapi.Options{
		Logger:                   logger,
		DB:                       db,
		UnhealthyThreshold:       time.Duration(cfg.UnhealthyThresholdSeconds) * time.Second,
		Queries:                  queries,
		Store:                    db,
		AuthMode:                 auth.Mode(cfg.AuthMode),
		InternalToken:            cfg.InternalToken,
		Authenticator:            auth.NewStaticTokenAuthenticator(queries),
		Pipeline:                 pipeline,
		AttributingRelationTypes: cfg.AttributingRelationTypes,
		Syncer:                   syncer,
		Metrics:                  m,
		MetricsEnabled:           cfg.MetricsEnabled,
	})
	if err != nil {
		return fmt.Errorf("building the router: %w", err)
	}

	// The gauge reports what the projection holds rather than what passed
	// through it, so a reader of its own keeps it current. It runs only while
	// the scrape route is served: with the instrumentation off the route answers
	// 404, and every refresh would be a query nothing reads. The counters record
	// either way, because the components hold this Metrics value whatever the
	// flag says. The context is the process's, so the goroutine ends with it.
	if cfg.MetricsEnabled {
		go metrics.NewRefresher(db, m, logger).
			Run(ctx, time.Duration(cfg.MetricsRefreshSeconds)*time.Second)
	}

	server := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.HTTPPort),
		Handler:           router,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	// The channel is buffered so that the serving goroutine ends even when the
	// shutdown path stops reading from it.
	serveErr := make(chan error, 1)
	logger.Info("listening", "port", cfg.HTTPPort)
	go func() { serveErr <- server.ListenAndServe() }()

	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serving HTTP: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	// The shutdown runs on its own context: ctx is already cancelled, and the
	// budget belongs to the in-flight requests, not to the signal.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	return nil
}

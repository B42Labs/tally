// Command tally-console serves the read-only demo console.
//
// It reads its configuration from the environment, refuses a configuration it
// cannot honor, and then serves the console's pages on the configured port,
// bound to the loopback address alone. The process holds an admin token and has
// no sign-in of its own, so whoever reaches the port reads everything the token
// reads. The token has to carry the admin role, because the dead-letter list
// the overview reads is admin-only.
//
// The pages come from two places: the Reporting API over HTTP, and the engine
// database, which is opened for reads only. Its pool connects lazily, so the
// process comes up while the database is unavailable and the pages that read it
// report the outage.
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
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/b42labs/tally/internal/console/config"
	"github.com/b42labs/tally/internal/console/httpui"
	"github.com/b42labs/tally/internal/console/reporting"
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
	// shutdownTimeout is what in-flight requests get once a signal arrives. The
	// console is started from a terminal rather than deployed, so this budget is
	// all a Ctrl-C gives the pages still rendering before the process ends.
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

// run assembles the console and serves it until ctx is cancelled or the server
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

	api, err := reporting.New(cfg.ReportingURL, cfg.APIToken, cfg.CAFile)
	if err != nil {
		return fmt.Errorf("building the reporting client: %w", err)
	}

	db, err := store.New(ctx, cfg.EngineDBURL, logger)
	if err != nil {
		return fmt.Errorf("opening the engine database pool: %w", err)
	}
	defer db.Close()

	router, err := httpui.NewRouter(httpui.Options{Logger: logger, API: api, Store: db})
	if err != nil {
		return fmt.Errorf("building the router: %w", err)
	}

	// The address is the loopback one and not a configured interface. The pages
	// ask nobody who they are and the process carries an admin token, so a
	// listener on 0.0.0.0 would hand every reader of the network everything that
	// token reads.
	server := &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.HTTPPort)),
		Handler:           router,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	// The channel is buffered so that the serving goroutine ends even when the
	// shutdown path stops reading from it.
	serveErr := make(chan error, 1)
	url := "http://127.0.0.1:" + strconv.Itoa(cfg.HTTPPort) + "/"
	logger.Info("listening", "url", url)
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

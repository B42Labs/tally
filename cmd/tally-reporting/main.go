// Command tally-reporting is a placeholder for the Reporting API.
//
// It answers the liveness and readiness probes and nothing else, which is what
// the dev cluster needs to report every pod Ready and to prove the Gateway,
// route, and certificate chain reaches an application pod. The real service,
// with its chi router, configuration, OpenAPI contract, database pool, and
// authentication, replaces this file wholesale.
package main

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"
)

const (
	defaultPort       = "8080"
	readHeaderTimeout = 5 * time.Second
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "tally-reporting")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", respondOK)
	mux.HandleFunc("GET /readyz", respondOK)

	port := os.Getenv("TALLY_HTTP_PORT")
	if port == "" {
		port = defaultPort
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	logger.Info("listening", "port", port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func respondOK(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

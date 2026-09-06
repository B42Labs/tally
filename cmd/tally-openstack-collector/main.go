// Command tally-openstack-collector collects OpenStack usage events.
//
// It consumes oslo.messaging notifications from AMQP, maps them to Tally
// events, buffers them in a SQLite outbox, and posts them to the Reporting API
// from a loop of its own. Both loops retry what failed, so the process comes up
// while the broker or the Reporting API is unavailable and reports that state
// through the probes rather than through a failed start. Next to them it serves
// the probes and the Prometheus exposition on the configured port; it serves no
// API of its own.
//
// SIGINT and SIGTERM begin a graceful shutdown: the HTTP server stops accepting
// connections, the consumer stops reading from the broker, the sender finishes
// the attempt it is in, and the outbox is closed once both have returned. What
// is buffered stays in the file, where the next start picks it up, and the
// process exits zero.
//
// With --dump the process prints the notifications the broker delivers, one
// JSON line per delivery, and does nothing else: no HTTP, no outbox, no
// delivery, and the broker URL is the only configuration it reads. It is how
// the exchanges, the topics, and the event types of a deployment are checked
// before the collector is pointed at it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/b42labs/tally/internal/core/health"
	"github.com/b42labs/tally/internal/providers/openstack"
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
	// probeTimeout bounds a probe's outbox check. A probe that hangs is a probe
	// that reports nothing, so an outbox that stops answering has to look like a
	// failure well before Kubernetes gives up on the request.
	probeTimeout = 2 * time.Second
)

func main() {
	var dump bool
	// ExitOnError exits before Parse returns an error.
	_ = newFlagSet(&dump).Parse(os.Args[1:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, dump); err != nil {
		// The configured logger may not exist yet, because loading the
		// configuration is the first thing that can fail.
		slog.Error("tally-openstack-collector stopped", "error", err)
		os.Exit(1)
	}
}

// newFlagSet is the flag set the collector parses its arguments with. It
// writes --dump into dump. The set is built here rather than in main so that
// the flags a run accepts are readable without running the command.
func newFlagSet(dump *bool) *flag.FlagSet {
	fs := flag.NewFlagSet("tally-openstack-collector", flag.ExitOnError)
	fs.BoolVar(dump, "dump", false,
		"print the notifications the broker delivers as JSON lines instead of collecting them")
	return fs
}

// run loads the configuration and then either dumps notifications or collects
// them, until ctx is cancelled. It returns nil for a shutdown that completed, so
// a signal leaves a zero exit status, and an error for everything else.
func run(ctx context.Context, dump bool) error {
	cfg, err := openstack.Load()
	if err != nil {
		return fmt.Errorf("loading the configuration: %w", err)
	}
	// The modes are gated separately: dumping maps nothing and posts nothing, so
	// it asks for none of the values only the pipeline reads.
	if dump {
		err = cfg.ValidateDump()
	} else {
		err = cfg.ValidateServe()
	}
	if err != nil {
		return fmt.Errorf("checking the configuration: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	})).With("service", "tally-openstack-collector")
	// The mapping reports what it had to assume about a payload through the
	// default logger, so the default has to be the configured one: otherwise
	// those lines arrive unstructured and at a level nobody chose.
	slog.SetDefault(logger)

	if dump {
		return openstack.Dump(ctx, cfg, os.Stdout, logger)
	}
	return serve(ctx, cfg, logger)
}

// serve runs the collector: the consumer and the sender in the background, the
// probes and the exposition in front of them.
func serve(ctx context.Context, cfg openstack.Config, logger *slog.Logger) error {
	outbox, err := openstack.OpenOutbox(cfg.BufferPath)
	if err != nil {
		return fmt.Errorf("opening the outbox: %w", err)
	}
	// The handle is closed last on every path out of this function, which is what
	// the defers below are ordered for: they run in reverse, so the loops are
	// stopped and waited for first. Neither of them may touch a closed outbox.
	defer func() {
		if err := outbox.Close(); err != nil {
			logger.Error("closing the outbox failed", "error", err)
		}
	}()

	// One Prometheus registry for the process. The consumer and the sender record
	// into the same Metrics value, so what a notification counts and what a batch
	// counts land in the instruments one scrape reads. The two gauges are read
	// off the outbox at scrape time rather than recorded.
	promReg := prometheus.NewRegistry()
	m := openstack.NewMetrics(promReg,
		func() float64 { return float64(outbox.Depth()) },
		outbox.OldestBufferedSeconds)

	consumer := openstack.NewConsumer(cfg, outbox, m, logger)
	sender := openstack.NewSender(outbox, cfg.ReportingURL, cfg.Token, cfg.BatchMax,
		time.Duration(cfg.FlushIntervalSeconds)*time.Second, m, logger)

	// The loops run on a context of their own so that a server which never got to
	// listen ends them too. They would otherwise keep running until a signal
	// arrived, and the wait below would never return.
	loopCtx, stopLoops := context.WithCancel(ctx)
	var loops sync.WaitGroup
	defer loops.Wait()
	defer stopLoops()

	loops.Add(2)
	// Both return nil once their context is done, which is the only way they
	// return: a broker or a Reporting API that is unreachable is retried in the
	// background rather than ending the process. A signal therefore stops the
	// consumer where it stands and leaves the sender its current attempt.
	go func() {
		defer loops.Done()
		_ = consumer.Run(loopCtx)
	}()
	go func() {
		defer loops.Done()
		_ = sender.Run(loopCtx)
	}()

	tracker := health.New(time.Now, time.Duration(cfg.UnhealthyThresholdSeconds)*time.Second)
	server := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.HTTPPort),
		Handler:           newMux(cfg, consumer, outbox, m, tracker, logger),
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

// newMux builds the three routes the collector serves. None of them carries a
// credential, which is what the conventions ask of every service
// (roadmap/00-conventions.md section 7).
func newMux(cfg openstack.Config, consumer *openstack.Consumer, outbox *openstack.Outbox,
	m *openstack.Metrics, tracker *health.Tracker, logger *slog.Logger,
) *http.ServeMux {
	mux := http.NewServeMux()

	// Readiness fails on the first failed check, which takes this pod out of the
	// Service's endpoints while leaving it running. What failed goes to the log
	// rather than into the body: the route carries no credential, and the error
	// underneath names the outbox file and the driver that could not read it.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := check(r.Context(), consumer, outbox); err != nil {
			logger.Warn("readiness probe found the collector unready", "error", err)
			writeProbe(w, http.StatusServiceUnavailable, "the collector is not ready")
			return
		}
		writeProbe(w, http.StatusOK, "ok")
	})

	// Liveness weighs the outbox alone, and only against time: it fails once the
	// buffer has been unusable for longer than the configured threshold.
	//
	// The broker is deliberately not part of it. Restarting brings back neither a
	// broker nor a volume, but during a broker outage the sender is the loop that
	// can still make progress, draining what is already buffered. A liveness that
	// failed on the broker would restart every replica for the length of that
	// outage and abort each in-flight delivery with it, turning a broker outage
	// into a delivery outage that outlasts it. Readiness still reports the
	// disconnected consumer, which is what takes the pod out of the Service.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()

		err := outbox.Ping(ctx)
		if err == nil {
			tracker.RecordSuccess()
			writeProbe(w, http.StatusOK, "ok")
			return
		}

		logger.Warn("liveness probe found the outbox unusable", "error", err,
			"unhealthy_for", tracker.UnhealthyFor().Round(time.Second).String())
		if !tracker.Exhausted() {
			writeProbe(w, http.StatusOK, "ok")
			return
		}
		// How long the outage has lasted goes to the log rather than into the
		// body: the probe answers whoever asks, and that is a detail about this
		// process's state rather than about the caller's request.
		writeProbe(w, http.StatusServiceUnavailable, "the collector has been unhealthy for too long")
	})

	// The switch decides the route and not the recording: the consumer and the
	// sender hold their Metrics value whatever it says, so the instruments keep
	// counting and switching the route back on costs nothing. A build serving no
	// metrics answers 404 rather than an empty exposition, which would read as a
	// collector with no activity.
	if cfg.MetricsEnabled {
		mux.Handle("GET /metrics", m.Handler())
	} else {
		mux.HandleFunc("GET /metrics", http.NotFound)
	}

	return mux
}

// check runs what readiness reports on: the consumer holds a connection to the
// broker, and the outbox answers. The error names which of the two failed. The
// outbox check takes a bound of its own, so a probe never outlasts the request
// that asked it.
func check(ctx context.Context, consumer *openstack.Consumer, outbox *openstack.Outbox) error {
	if !consumer.Connected() {
		return errors.New("the AMQP consumer is not connected")
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if err := outbox.Ping(ctx); err != nil {
		return fmt.Errorf("the outbox is unreachable: %w", err)
	}
	return nil
}

// writeProbe answers a probe in plain text. The collector serves no API, so
// there is no error format its probes would have to match, and what reads them
// is an orchestrator that looks at the status.
func writeProbe(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

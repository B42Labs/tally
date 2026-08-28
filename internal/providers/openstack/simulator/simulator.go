package simulator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/providers/openstack"
)

// The bounds the control server is served under. They are the collector's, in
// cmd/tally-openstack-collector/main.go, and they are here for the same
// reasons.
const (
	// readHeaderTimeout bounds how long a client may take to send its request
	// headers, which keeps a stalled connection from holding a slot.
	readHeaderTimeout = 5 * time.Second
	// readTimeout bounds the whole request, headers and body together.
	readTimeout = 30 * time.Second
	// writeTimeout bounds the response. Without it a client that stops reading
	// holds its handler goroutine for as long as it likes.
	writeTimeout = 60 * time.Second
	// idleTimeout bounds a kept-alive connection between requests. Go derives it
	// from readTimeout when it is zero, and a zero readTimeout clears the read
	// deadline entirely, so idle connections would pile up.
	idleTimeout = 120 * time.Second
	// shutdownTimeout is what an in-flight request gets once the month is over.
	// It is short because the only caller is somebody watching the run.
	shutdownTimeout = 10 * time.Second
)

// outDirMode is the mode the output directory is created with. The files in it
// are read by whoever runs the simulator and by a replay, and they carry no
// secret.
const outDirMode = 0o755

// RunOptions is what a run needs beyond its environment: which month to
// generate from which seed, how fast to publish it, and where it goes. The
// flags of the run subcommand map onto it one to one, which is why the errors
// quote flag names rather than field names.
type RunOptions struct {
	// Period is the billing month to generate, as YYYY-MM.
	Period string
	// Seed decides the shape of the simulated cloud.
	Seed uint64
	// Factor is how many virtual seconds pass per wall second. Zero is
	// unbounded: the month goes out as fast as the broker takes it.
	Factor float64
	// Out is the directory notifications.jsonl and events.jsonl are written to.
	// Empty writes nothing.
	Out string
	// WaitForCollector is how long to wait for a consumer on the collector's
	// queue before the first notification goes out. Zero disables the wait.
	WaitForCollector time.Duration
}

// Validate checks the options and returns the month Period names.
//
// It exists so the command refuses a mistyped flag before it dials the broker,
// which is where a run would otherwise notice. Run calls it again itself,
// because an exported function cannot rely on its caller having done so.
func (o RunOptions) Validate() (from, to time.Time, err error) {
	from, to, err = period.Parse(o.Period)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--period: %w", err)
	}
	// The one place the simulator reads the wall clock. A month that has not
	// ended is one the engine marks with runs.WarningPeriodNotEnded
	// (internal/engine/runs/runs.go) whenever it is billed, so a simulated month
	// reaching into the future would put that warning on every drill run over it.
	if to.After(time.Now()) {
		return time.Time{}, time.Time{}, fmt.Errorf(
			"--period: %s has not ended yet; it ends %s, and the engine warns about a period that has not ended",
			o.Period, to.Format(time.RFC3339))
	}
	if err := validatePacing(o.Factor, o.WaitForCollector); err != nil {
		return time.Time{}, time.Time{}, err
	}
	return from, to, nil
}

// ReplayOptions is what a replay needs: the file to publish and the pacing to
// publish it under. The month itself comes from the file, so there is no seed
// and no period here.
type ReplayOptions struct {
	// In is the notifications.jsonl a run wrote.
	In string
	// Factor is how many virtual seconds pass per wall second. Zero is
	// unbounded.
	Factor float64
	// WaitForCollector is how long to wait for a consumer on the collector's
	// queue before the first notification goes out. Zero disables the wait.
	WaitForCollector time.Duration
}

// Validate checks the pacing options, the way RunOptions.Validate does. The
// file is not opened here: reading it is the replay's own first step, and a
// check that opened it would either read the month twice or leave a handle
// behind.
func (o ReplayOptions) Validate() error {
	return validatePacing(o.Factor, o.WaitForCollector)
}

// validatePacing checks the two options both subcommands take. Both are
// durations or rates that only make sense forwards: a negative factor runs the
// month backwards, and a negative wait is a wait nobody can serve. NaN is
// refused with the negative factor, for the reason Clock.SetFactor gives: it
// passes every comparison a bound is written as.
func validatePacing(factor float64, wait time.Duration) error {
	if math.IsNaN(factor) || factor < 0 {
		return fmt.Errorf("--factor: %g must be zero or positive", factor)
	}
	if wait < 0 {
		return fmt.Errorf("--wait-for-collector: %s must be zero or positive", wait)
	}
	return nil
}

// queued is one notification ready for the bus: the addressing it goes out
// under, the bytes, and the virtual instant the clock releases it at. The whole
// month is turned into these before the first one is published, so a rendering
// error ends the run with nothing published rather than halfway through.
type queued struct {
	exchange   string
	routingKey string
	eventType  string
	messageID  string
	at         time.Time
	body       []byte
}

// Run generates one month of notifications and publishes it, writes it to
// files, or both.
//
// The publisher is dialled by the caller rather than here. That is what lets a
// caller have the service exchanges declared before a collector starts: the
// collector declares them passively and reconnects until somebody has declared
// them, and a collector started first spends the run reconnecting. A nil
// publisher is file mode, where the month is written out and nothing reaches a
// bus.
//
// Both files are complete before the first notification is published, so a run
// that is interrupted halfway still leaves a whole month on disk to replay.
//
// A cancelled context is a clean stop: what went out stays out, and Run returns
// nil, so SIGINT and SIGTERM leave exit status 0 the way they do for the
// collector. A failed publish is an error instead, and what an operator does
// about it is rerun the same seed, period and cloud: that renders the same
// message ids, and ingestion deduplicates whatever was already delivered.
func Run(ctx context.Context, cfg Config, opts RunOptions, publisher *Publisher, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	from, to, err := opts.Validate()
	if err != nil {
		return err
	}
	if publisher == nil && opts.Out == "" {
		return errors.New("set " + envAMQPURL + " or pass --out: the run has nowhere to publish")
	}

	schedule, err := Generate(opts.Seed, from, to, cfg.Cloud)
	if err != nil {
		return err
	}

	logger.Info("starting",
		"seed", opts.Seed,
		"period", opts.Period,
		"cloud", cfg.Cloud,
		"factor", opts.Factor,
		"transitions", len(schedule),
		"billable", len(schedule.Billable()),
		"broker", publisher != nil,
		"out", opts.Out)

	if opts.Out != "" {
		if err := os.MkdirAll(opts.Out, outDirMode); err != nil {
			return fmt.Errorf("creating %s: %w", opts.Out, err)
		}
		if err := WriteStream(filepath.Join(opts.Out, "notifications.jsonl"), schedule); err != nil {
			return err
		}
		if err := WriteEvents(filepath.Join(opts.Out, "events.jsonl"), cfg.Cloud, schedule); err != nil {
			return err
		}
	}

	if publisher == nil {
		logger.Info("completed", "published", 0, "total", len(schedule))
		return nil
	}

	lines := make([]queued, 0, len(schedule))
	for _, transition := range schedule {
		body, err := Render(transition)
		if err != nil {
			return err
		}
		lines = append(lines, queued{
			exchange:   transition.Exchange,
			routingKey: routingKey,
			eventType:  transition.EventType,
			messageID:  transition.MessageID,
			at:         transition.At,
			body:       body,
		})
	}
	return broadcast(ctx, cfg, publisher, logger, opts.Factor, opts.WaitForCollector, from, to, lines)
}

// Replay publishes a month a run recorded, in the order the file holds it.
//
// The publisher is dialled by the caller, as it is for Run, and for the same
// reason. A replay has no file mode: the month already exists as a file, so a
// nil publisher leaves it nothing to do.
//
// A cancelled context is a clean stop and a failed publish is an error, the way
// Run treats them: the recorded message ids are the ones the run published, so
// a repeated replay is deduplicated by ingestion.
func Replay(ctx context.Context, cfg Config, opts ReplayOptions, publisher *Publisher, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	if err := opts.Validate(); err != nil {
		return err
	}
	// ValidateReplay refuses a missing broker URL long before this, so reaching
	// here without a publisher is a caller of this package rather than an
	// operator. It is still answered rather than dereferenced.
	if publisher == nil {
		return errors.New("set " + envAMQPURL + ": a replay has nowhere to publish")
	}

	read, err := ReadStream(opts.In)
	if err != nil {
		return err
	}

	lines := make([]queued, 0, len(read))
	for number, line := range read {
		// ReadStream already parsed every body, so this cannot fail; parsing it
		// again is how the timestamp, the type and the id are read out of it
		// without ReadStream having to carry a second form of each line.
		notification, err := openstack.ParseEnvelope(line.Body)
		if err != nil {
			return fmt.Errorf("%s: line %d: %w", opts.In, number+1, err)
		}
		lines = append(lines, queued{
			exchange:   line.Exchange,
			routingKey: line.RoutingKey,
			eventType:  notification.EventType,
			messageID:  notification.MessageID,
			at:         notification.Timestamp,
			body:       line.Body,
		})
	}

	from, to := lines[0].at, lines[len(lines)-1].at
	logger.Info("starting",
		"in", opts.In,
		"factor", opts.Factor,
		"transitions", len(lines),
		"from", from.Format(time.RFC3339),
		"to", to.Format(time.RFC3339))

	return broadcast(ctx, cfg, publisher, logger, opts.Factor, opts.WaitForCollector, from, to, lines)
}

// broadcast puts the lines on the bus in the order they stand in, paced by a
// virtual clock that starts at from, and serves the control endpoint while it
// does.
//
// The endpoint is served for the length of the publishing and no longer,
// because pacing is the only thing it changes and there is nothing to pace
// before the first notification or after the last one.
//
// Lines go out in their own order rather than in timestamp order. One whose
// instant lies before the previous one is published at once, because SleepUntil
// returns immediately when the clock has already passed it, so a recorded file
// that is not perfectly sorted still replays whole.
func broadcast(ctx context.Context, cfg Config, publisher *Publisher, logger *slog.Logger,
	factor float64, wait time.Duration, from, to time.Time, lines []queued,
) error {
	if err := publisher.AwaitConsumer(ctx, collectorQueue, wait); err != nil {
		if ctx.Err() != nil {
			logger.Info("stopped", "published", 0, "total", len(lines))
			return nil
		}
		return err
	}

	// The endpoint carries no credential, so it binds the address the
	// configuration names rather than every interface the machine has: on
	// loopback by default, and on 0.0.0.0 where a deployment publishes the port
	// on purpose, as the compose stack does. A configured port of 0 lets the
	// operating system pick one, which is what a test uses to run several
	// simulators at once.
	listener, err := net.Listen("tcp", cfg.ControlAddr())
	if err != nil {
		return fmt.Errorf("serving HTTP: %w", err)
	}

	clock := NewClock(from, factor, time.Now)
	var published atomic.Int64
	progress := func() Progress {
		return Progress{From: from, To: to, Published: int(published.Load()), Total: len(lines)}
	}
	server := &http.Server{
		Handler:           NewControlMux(clock, progress),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}
	// The channel is buffered so that the serving goroutine ends even when
	// nothing reads from it.
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	// Every path out of here closes the endpoint, including the one that reports
	// a failed publish. A shutdown that fails is logged rather than returned: the
	// month is over by then, and the error the caller wants is the one about the
	// month.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutting the control endpoint down failed", "error", err)
		}
	}()

	port := cfg.HTTPPort
	if addr, ok := listener.Addr().(*net.TCPAddr); ok {
		port = addr.Port
	}
	logger.Info("listening", "port", port)

	for _, line := range lines {
		err := clock.SleepUntil(ctx, line.at)
		if err == nil {
			err = publisher.Publish(ctx, line.exchange, line.routingKey, line.body)
		}
		if err != nil {
			if ctx.Err() != nil {
				logger.Info("stopped", "published", published.Load(), "total", len(lines))
				return nil
			}
			return err
		}
		published.Add(1)
		logger.Debug("published",
			"event_type", line.eventType,
			"exchange", line.exchange,
			"message_id", line.messageID,
			"timestamp", line.at.Format(time.RFC3339))
	}

	// A control endpoint that never came up is reported here rather than while
	// the month runs: the month is what the operator asked for, and losing the
	// pacing endpoint is no reason to stop publishing it.
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving HTTP: %w", err)
		}
	default:
	}

	logger.Info("completed", "published", published.Load(), "total", len(lines))
	return nil
}

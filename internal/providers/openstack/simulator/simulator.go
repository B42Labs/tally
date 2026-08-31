package simulator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	// Out is the directory notifications.jsonl, events.jsonl and oracle.json are
	// written to, and held-back.jsonl beside them when a switch holds something
	// back. Empty writes nothing.
	Out string
	// WaitForCollector is how long to wait for a consumer on the collector's
	// queue before the first notification goes out. Zero disables the wait.
	WaitForCollector time.Duration
	// Faults are the switch names of --faults, which ParseFaults reads. Empty is
	// every switch off.
	Faults []string
	// RegisterProjects registers the month's tenants, its Gardener projects, and
	// the infrastructure_tenant relation between each project and its tenant
	// with the Reporting API before the first file is written or the first
	// notification published. What it needs is the environment, which Config
	// checks through ValidateRegistration.
	RegisterProjects bool
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
	if _, err := ParseFaults(o.Faults); err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--faults: %w", err)
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
// With RegisterProjects the month's tenants, its Gardener projects and the
// relations between them are registered with the Reporting API first, before
// anything is written or published.
//
// The files are complete before the first notification is published, so a run
// that is interrupted halfway still leaves a whole month on disk to replay.
// There are three of them, and a fourth, held-back.jsonl, when the held-back
// switch keeps part of the month off the bus.
//
// While the month goes out, the run serves it as a fake OpenStack API as well,
// on the control endpoint's listener and for exactly as long, the hold
// included. A reconciliation sync reads the cloud the notifications describe
// while they are published. File mode serves neither of the two: nothing is in
// flight there.
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
	// Checked here as well as in the subcommand, because an exported function
	// cannot rely on its caller having checked.
	if opts.RegisterProjects {
		if err := cfg.ValidateRegistration(); err != nil {
			return fmt.Errorf("checking the configuration: %w", err)
		}
	}
	if publisher == nil && opts.Out == "" {
		return errors.New("set " + envAMQPURL + " or pass --out: the run has nowhere to publish")
	}

	// Validate has read the switches once already; this is the parsed value
	// itself, which it does not hand back.
	faults, err := ParseFaults(opts.Faults)
	if err != nil {
		return fmt.Errorf("--faults: %w", err)
	}

	month, err := GenerateMonth(opts.Seed, from, to, cfg.Cloud, faults)
	if err != nil {
		return err
	}
	schedule := month.Schedule

	logger.Info("starting",
		"seed", opts.Seed,
		"period", opts.Period,
		"cloud", cfg.Cloud,
		"factor", opts.Factor,
		"faults", faults.Names(),
		"register", opts.RegisterProjects,
		"transitions", len(schedule),
		"billable", len(schedule.Billable()),
		"stream", len(month.Stream),
		"held", len(month.Held),
		"resources", len(month.Oracle.Resources),
		"broker", publisher != nil,
		"out", opts.Out)

	// Between the month and the files: a registration that fails leaves neither
	// a file nor a notification behind, so what an operator finds afterwards is
	// the registry the run got as far as, and nothing else of the month. It runs
	// in file mode too, which is how an operator prepares the registry before a
	// replay puts the recorded month on a bus. A context that ends here is the
	// clean stop the rest of the run answers a signal with.
	if opts.RegisterProjects {
		regs, err := RegistrationsOf(month, cfg.GardenCloud)
		if err != nil {
			return fmt.Errorf("registering the projects: %w", err)
		}
		report, err := NewRegistrar(cfg.ReportingURL, cfg.APIToken, logger).Register(ctx, regs)
		if err != nil {
			// The rows that got through stay in the registry and the rerun finds
			// them, so how far the registration came is said whichever way it
			// ended: the counts of the line below are the only place the whole of
			// it is, and "stopped" is about the notifications rather than the rows.
			logger.Info("registration incomplete",
				"projects_created", report.ProjectsCreated, "projects_existing", report.ProjectsExisting,
				"relations_created", report.RelationsCreated, "relations_existing", report.RelationsExisting)
			if ctx.Err() != nil {
				logger.Info("stopped", "published", 0, "total", len(month.Stream)+len(month.Held))
				return nil
			}
			return fmt.Errorf("registering the projects: %w", err)
		}
		logger.Info("registered",
			"reporting_url", cfg.ReportingURL,
			"projects_created", report.ProjectsCreated, "projects_existing", report.ProjectsExisting,
			"relations_created", report.RelationsCreated, "relations_existing", report.RelationsExisting)
	}

	if opts.Out != "" {
		if err := os.MkdirAll(opts.Out, outDirMode); err != nil {
			return fmt.Errorf("creating %s: %w", opts.Out, err)
		}
		// The files of an earlier run go first, all four of them and before the
		// first of this month's is written: written over one by one, whichever
		// ones a failed write never reached would stay beside this month's, and a
		// directory reused across runs would carry another month's oracle, events,
		// or held share into a replay or a drill. A removal that fails does not
		// stop the others, and every one of them that failed is named, so the
		// paths a failed run reports are what is left of the earlier month in the
		// directory.
		var removeErr error
		for _, name := range []string{
			"notifications.jsonl", "events.jsonl", "oracle.json", "held-back.jsonl",
		} {
			path := filepath.Join(opts.Out, name)
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				removeErr = errors.Join(removeErr, fmt.Errorf("removing %s: %w", path, err))
			}
		}
		if removeErr != nil {
			return removeErr
		}
		if err := WriteStream(filepath.Join(opts.Out, "notifications.jsonl"), month.Stream); err != nil {
			return err
		}
		if err := WriteEvents(filepath.Join(opts.Out, "events.jsonl"), cfg.Cloud, schedule); err != nil {
			return err
		}
		if err := WriteOracle(filepath.Join(opts.Out, "oracle.json"), month.Oracle); err != nil {
			return err
		}
		// The fourth file, when the switch holds something back.
		if len(month.Held) > 0 {
			if err := WriteStream(filepath.Join(opts.Out, "held-back.jsonl"), month.Held); err != nil {
				return err
			}
		}
	}

	if publisher == nil {
		logger.Info("completed", "published", 0, "total", len(month.Stream)+len(month.Held))
		return nil
	}

	lines, err := renderQueue(month.Stream)
	if err != nil {
		return err
	}
	held, err := renderQueue(month.Held)
	if err != nil {
		return err
	}

	// The wait comes before the clock is built, because virtual time starts
	// running the moment it is: a clock built first would have run through part
	// of the month while the collector was still connecting, and the run would
	// publish that part in one burst.
	if err := publisher.AwaitConsumer(ctx, collectorQueue, opts.WaitForCollector); err != nil {
		// A context that ends here is the clean stop the rest of the run answers
		// a signal with, at the one point where nothing has gone out yet.
		if ctx.Err() != nil {
			logger.Info("stopped", "published", 0, "total", len(lines)+len(held), "held", len(held))
			return nil
		}
		return err
	}
	clock := NewClock(from, opts.Factor, time.Now)
	api, err := NewCloudAPI(clock, month.Oracle)
	if err != nil {
		return fmt.Errorf("building the fake OpenStack API: %w", err)
	}

	return broadcast(ctx, cfg, publisher, logger, clock, from, to, lines, held, api)
}

// renderQueue turns a schedule into the notifications the bus carries. The
// whole of it is rendered before the first one is published, so a rendering
// error ends the run with nothing published rather than halfway through.
func renderQueue(schedule Schedule) ([]queued, error) {
	lines := make([]queued, 0, len(schedule))
	for _, transition := range schedule {
		body, err := Render(transition)
		if err != nil {
			return nil, err
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
	return lines, nil
}

// Replay publishes a month a run recorded, in the order the file holds it.
//
// The publisher is dialled by the caller, as it is for Run, and for the same
// reason. A replay has no file mode: the month already exists as a file, so a
// nil publisher leaves it nothing to do.
//
// It serves the control endpoint the way a run does, and no fake OpenStack API:
// the recorded file holds the notifications of a month, not the oracle a
// listing is answered out of.
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

	// Before the clock, for the reason Run states.
	if err := publisher.AwaitConsumer(ctx, collectorQueue, opts.WaitForCollector); err != nil {
		if ctx.Err() != nil {
			logger.Info("stopped", "published", 0, "total", len(lines), "held", 0)
			return nil
		}
		return err
	}
	clock := NewClock(from, opts.Factor, time.Now)

	return broadcast(ctx, cfg, publisher, logger, clock, from, to, lines, nil, nil)
}

// broadcast puts the lines on the bus in the order they stand in, paced by the
// clock the caller started at from, holds the held ones back until a release
// arrives, and serves the control endpoint through all of it.
//
// The endpoint is served for the length of the run and no longer. It changes
// when the month goes out, and there is nothing left to decide before the first
// notification or after the last one, so it comes up with the publishing and
// stays up through the hold.
//
// An api is mounted on the same listener, under every path the control routes
// leave: those are method-qualified and beat the catch-all, so the fake
// OpenStack API answers the rest and lives exactly as long as the endpoint. A
// nil api mounts nothing, which is what a replay passes.
//
// A held share is published after the last regular line, once POST /release
// asks for it. Every held instant lies before the clock by then, so the
// notifications go out as fast as the broker confirms them, each under the
// timestamp inside the month it always carried. A context that ends during the
// hold is a clean stop with the held share never published.
func broadcast(ctx context.Context, cfg Config, publisher *Publisher, logger *slog.Logger,
	clock *Clock, from, to time.Time, lines, held []queued, api http.Handler,
) error {
	hb := newHoldback(len(held))
	total := len(lines) + len(held)
	var published atomic.Int64
	// How every end of a run is answered: a cancelled context is a clean stop,
	// reported as one and answered with nil, and anything else is the error it
	// is. SIGINT and SIGTERM leave exit status 0 through this.
	stop := func(err error) error {
		if ctx.Err() == nil {
			return err
		}
		logger.Info("stopped", "published", published.Load(), "total", total, "held", hb.held())
		return nil
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

	progress := func() Progress {
		return Progress{
			From: from, To: to,
			Published: int(published.Load()), Total: total,
			Held: hb.held(), Holding: hb.holding(),
		}
	}
	mux := NewControlMux(clock, progress, hb.release)
	if api != nil {
		mux.Handle("/", api)
	}
	server := &http.Server{
		Handler:           mux,
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

	if err := publishLines(ctx, clock, publisher, logger, lines, &published); err != nil {
		return stop(err)
	}

	if len(held) > 0 {
		hb.phase.Store(phaseHolding)
		logger.Info("holding", "held", len(held), "hint", "POST /release")
		select {
		case <-ctx.Done():
			return stop(ctx.Err())
		case <-hb.released:
			logger.Info("releasing", "held", len(held))
			// A failed publish here is the error it is during the month: what the
			// operator does about it is rerun the same seed, period and cloud.
			if err := publishLines(ctx, clock, publisher, logger, held, &published); err != nil {
				return stop(err)
			}
		}
	}

	// A control endpoint that never came up is reported here rather than while
	// the month runs: the month is what the operator asked for, and losing the
	// endpoint is no reason to stop publishing it.
	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving HTTP: %w", err)
		}
	default:
	}

	logger.Info("completed", "published", published.Load(), "total", total)
	return nil
}

// publishLines puts the lines on the bus in the order they stand in, paced by
// the clock, and counts every one the broker confirmed.
//
// Lines go out in their own order rather than in timestamp order. One whose
// instant lies before the previous one is published at once, because SleepUntil
// returns immediately when the clock has already passed it, so a recorded file
// that is not perfectly sorted still replays whole.
func publishLines(ctx context.Context, clock *Clock, publisher *Publisher, logger *slog.Logger,
	lines []queued, published *atomic.Int64,
) error {
	for _, line := range lines {
		err := clock.SleepUntil(ctx, line.at)
		if err == nil {
			err = publisher.Publish(ctx, line.exchange, line.routingKey, line.body)
		}
		if err != nil {
			return err
		}
		published.Add(1)
		logger.Debug("published",
			"event_type", line.eventType,
			"exchange", line.exchange,
			"message_id", line.messageID,
			"timestamp", line.at.Format(time.RFC3339))
	}
	return nil
}

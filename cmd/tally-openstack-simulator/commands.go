package main

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/b42labs/tally/internal/providers/openstack/simulator"
)

// defaultFactor is the pacing a run takes when nobody asks for another one: 744
// virtual seconds per wall second puts a 31-day month on the bus in an hour,
// which is long enough to watch a collector work and short enough to sit
// through.
const defaultFactor = 744

// defaultWaitForCollector is how long a run waits for the collector to appear
// on its queue before it publishes. A topic exchange drops what no queue is
// bound to, so a month published into an empty broker is a month lost; two
// minutes cover a collector that is still starting.
const defaultWaitForCollector = 2 * time.Minute

// The confirmation both subcommands ask for before they dial a broker that is
// not on this machine. What a run puts on a broker is booked as real usage by
// whatever collector consumes it, so the flag is what turns a copied production
// URL from a typo into a decision; simulator.EnsureLocalBroker holds the rest of
// the reason.
const (
	allowRemoteBrokerFlag  = "allow-remote-broker"
	allowRemoteBrokerUsage = "publish to a broker that is not on this machine, " +
		"knowing that a collector books what a run publishes as real usage"
)

// newRunCmd builds the run subcommand.
func newRunCmd() *cobra.Command {
	var opts simulator.RunOptions
	// Not a field of RunOptions: what it decides happens before the run begins,
	// when the broker is dialled, and the run itself never reads it.
	var allowRemoteBroker bool

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Generate a month of notifications and publish or write it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The configuration is read when the subcommand runs rather than when
			// the tree is built, so building the tree, and with it --help, needs no
			// environment at all.
			cfg, err := simulator.Load()
			if err != nil {
				return fmt.Errorf("loading the configuration: %w", err)
			}
			if err := cfg.ValidateRun(); err != nil {
				return fmt.Errorf("checking the configuration: %w", err)
			}
			// The flags are checked before the broker is dialled: a mistyped period
			// or a negative factor is the operator's own mistake, and reporting it
			// here costs neither a connection nor a wait.
			if _, _, err := opts.Validate(); err != nil {
				return err
			}

			logger := newLogger(cmd.OutOrStdout(), cfg)

			// A run without a broker URL is file mode: the month is written out and
			// nothing is dialled, which is what makes a month on a machine with no
			// broker possible at all.
			var publisher *simulator.Publisher
			if cfg.AMQPURL != "" {
				connected, closePublisher, err := connect(cfg, allowRemoteBroker, logger)
				if err != nil {
					return err
				}
				defer closePublisher()
				publisher = connected
			}
			return simulator.Run(cmd.Context(), cfg, opts, publisher, logger)
		},
	}

	cmd.Flags().StringVar(&opts.Period, "period", "",
		"billing month to simulate, YYYY-MM; it must have ended")
	cmd.Flags().Uint64Var(&opts.Seed, "seed", 1, "seed of the month's shape")
	cmd.Flags().Float64Var(&opts.Factor, "factor", defaultFactor,
		"virtual seconds per wall second; 0 publishes as fast as the broker confirms")
	cmd.Flags().StringVar(&opts.Out, "out", "",
		"directory to write notifications.jsonl, events.jsonl and oracle.json to, "+
			"and held-back.jsonl when a switch holds notifications back")
	cmd.Flags().DurationVar(&opts.WaitForCollector, "wait-for-collector", defaultWaitForCollector,
		"how long to wait for a consumer on the collector's queue before publishing; 0 disables the wait")
	cmd.Flags().StringSliceVar(&opts.Faults, "faults", nil,
		"fault switches to turn on, comma-separated: "+strings.Join(simulator.FaultNames, ", ")+
			"; every switch is off by default")
	cmd.Flags().BoolVar(&allowRemoteBroker, allowRemoteBrokerFlag, false, allowRemoteBrokerUsage)
	_ = cmd.MarkFlagRequired("period")
	return cmd
}

// newReplayCmd builds the replay subcommand.
func newReplayCmd() *cobra.Command {
	var opts simulator.ReplayOptions
	var allowRemoteBroker bool

	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Publish a recorded notifications.jsonl",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Read here rather than while the tree is built, for the reason run
			// states.
			cfg, err := simulator.Load()
			if err != nil {
				return fmt.Errorf("loading the configuration: %w", err)
			}
			// A replay has no file mode, so the broker is asked for before anything
			// else: the recorded month is already a file, and publishing it is the
			// whole of what this subcommand does.
			if err := cfg.ValidateReplay(); err != nil {
				return fmt.Errorf("checking the configuration: %w", err)
			}
			if err := opts.Validate(); err != nil {
				return err
			}

			logger := newLogger(cmd.OutOrStdout(), cfg)

			publisher, closePublisher, err := connect(cfg, allowRemoteBroker, logger)
			if err != nil {
				return err
			}
			defer closePublisher()
			return simulator.Replay(cmd.Context(), cfg, opts, publisher, logger)
		},
	}

	cmd.Flags().StringVar(&opts.In, "in", "", "path of a notifications.jsonl written by run --out")
	cmd.Flags().Float64Var(&opts.Factor, "factor", defaultFactor,
		"virtual seconds per wall second; 0 publishes as fast as the broker confirms")
	cmd.Flags().DurationVar(&opts.WaitForCollector, "wait-for-collector", defaultWaitForCollector,
		"how long to wait for a consumer on the collector's queue before publishing; 0 disables the wait")
	cmd.Flags().BoolVar(&allowRemoteBroker, allowRemoteBrokerFlag, false, allowRemoteBrokerUsage)
	_ = cmd.MarkFlagRequired("in")
	return cmd
}

// newCompareCmd builds the compare subcommand.
func newCompareCmd() *cobra.Command {
	var opts simulator.CompareOptions

	cmd := &cobra.Command{
		Use:   "compare",
		Short: "Compare an engine export of the month against the oracle a run wrote",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Neither the configuration nor a logger: the comparison reads the three
			// files its flags name and nothing else, so it needs no environment and
			// no broker.
			report, err := simulator.Compare(opts)
			if err != nil {
				return err
			}
			if err := write(cmd.OutOrStdout(), report.Lines()...); err != nil {
				return err
			}
			// A month that differs ends the process with exit status 1, through the
			// os.Exit(1) main gives every error, and cobra prints the count to
			// stderr under the lines. A drill that runs unattended then fails where
			// it ran rather than in whoever reads the output later.
			if len(report.Differences) > 0 {
				return fmt.Errorf("%d resources differ from the oracle", len(report.Differences))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&opts.Oracle, "oracle", "", "path of an oracle.json written by run --out")
	cmd.Flags().StringVar(&opts.Export, "export", "",
		"directory tally-engine export --format csv --out wrote; rated.csv is read from it")
	cmd.Flags().StringVar(&opts.Pricing, "pricing", "", "pricing model YAML the run rated with")
	_ = cmd.MarkFlagRequired("oracle")
	_ = cmd.MarkFlagRequired("export")
	_ = cmd.MarkFlagRequired("pricing")
	return cmd
}

// newLogger builds the logger both subcommands log through and makes it the
// process default. The default is what the control endpoint writes its factor
// changes through, so leaving it unset would put those lines on stderr,
// unstructured and at a level nobody chose.
func newLogger(out io.Writer, cfg simulator.Config) *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	})).With("service", "tally-openstack-simulator")
	slog.SetDefault(logger)
	return logger
}

// connect dials the broker cfg names and returns it together with the function
// that gives the connection back. The close error is logged rather than
// returned, because by the time it runs the answer the caller waits for is the
// one about the month.
//
// Which broker it is gets checked here rather than deeper down because this is
// the last point before the dial, and a month published onto the wrong broker
// is one nothing takes back.
func connect(cfg simulator.Config, allowRemoteBroker bool, logger *slog.Logger) (*simulator.Publisher, func(), error) {
	if !allowRemoteBroker {
		if err := simulator.EnsureLocalBroker(cfg.AMQPURL); err != nil {
			return nil, nil, err
		}
	}
	publisher, err := simulator.Connect(cfg.AMQPURL)
	if err != nil {
		return nil, nil, err
	}
	return publisher, func() {
		if err := publisher.Close(); err != nil {
			logger.Error("closing the broker connection failed", "error", err)
		}
	}, nil
}

// write puts every line on w. A failed write is reported rather than dropped:
// output the operator's terminal never received must not leave the process with
// a zero exit status.
func write(w io.Writer, lines ...string) error {
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return fmt.Errorf("writing the output: %w", err)
		}
	}
	return nil
}

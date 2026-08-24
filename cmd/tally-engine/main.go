// Command tally-engine is the metering engine's operator tool and its
// scheduler entrypoint. An operator drives a billing period through it: run
// meters and rates a period, finalize closes it, detect-late and correct deal
// with what arrived afterwards, and export writes the result out. The tick
// subcommand is the same tree run unattended by an hourly CronJob.
//
// It is also the only thing that runs DDL on the engine database. Its migrate
// subcommand applies the embedded goose chain, migrate-status reports which of
// it a database carries, and migrate-down-to runs the chain's down migrations.
// Nothing else migrates as a side effect, so a schema change stays an
// operator's decision rather than something a scheduled run brings along.
//
// The migrate subcommands, periods list, the pricing import and list, and run,
// finalize and tick work. What is left is the interface the later Phase 3
// packages fill in: detect-late, correct and export each carry their full flag
// surface, check what they were given, and then report that they are not
// implemented. The command line is fixed here so the packages that arrive
// behind it change what a command does rather than how it is called.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.1.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/b42labs/tally/internal/engine/config"
	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/store"
)

func main() {
	// The tree runs on a context the process's own termination cancels, which is
	// what the tick needs: it spends minutes inside database calls, and without a
	// cancellable context a query that stops answering keeps the CronJob's Job
	// alive past its deadline, where concurrencyPolicy: Forbid suppresses every
	// tick behind it. Cancellation reaches the run's bookkeeping too, which
	// writes the failed run and gives the period lock back on its own context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Cobra has already written the error to stderr by the time Execute
	// returns it.
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

// newRootCmd assembles the command tree. Nothing in it reads the process's
// arguments or streams, so a test drives the same tree through SetArgs,
// SetOut, and SetErr.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "tally-engine",
		Short: "Meter, rate, and finalize the billing periods",
		// A database that is down is not a usage mistake, and dumping the help
		// text after one buries the line that says what went wrong.
		SilenceUsage: true,
	}

	periods := &cobra.Command{
		Use:   "periods",
		Short: "Work with the billing periods",
	}
	periods.AddCommand(newPeriodsListCmd())

	pricing := &cobra.Command{
		Use:   "pricing",
		Short: "Work with the pricing catalogs",
	}
	pricing.AddCommand(
		newPricingImportCmd(),
		newPricingListCmd(),
	)

	root.AddCommand(
		newMigrateCmd(),
		newMigrateStatusCmd(),
		newMigrateDownToCmd(),
		periods,
		newRunCmd(),
		newFinalizeCmd(),
		newDetectLateCmd(),
		newCorrectCmd(),
		pricing,
		newExportCmd(),
		newTickCmd(),
	)
	return root
}

// loadConfig reads the environment and runs the database gate over it. The
// subcommands call this when they run rather than when they are built, so
// building the tree, and with it --help, needs no configuration at all.
func loadConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, fmt.Errorf("loading the configuration: %w", err)
	}
	if err := cfg.ValidateDB(); err != nil {
		return config.Config{}, fmt.Errorf("checking the configuration: %w", err)
	}
	return cfg, nil
}

// openStore is the setup every database-backed subcommand shares: the
// configuration, and a pool on the engine database it names. The caller closes
// the store.
func openStore(ctx context.Context) (*store.Store, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return openEngine(ctx, cfg)
}

// openEngine opens the checked pool on the engine database cfg names. It is
// openStore over a configuration that is already loaded, which is what
// openPipeline works from: one configuration gates both databases.
func openEngine(ctx context.Context, cfg config.Config) (*store.Store, error) {
	db, err := store.New(ctx, cfg.DBURL)
	if err != nil {
		return nil, err
	}
	// A pool opens on a database that carries an older schema as happily as on a
	// migrated one, so the schema is what the store is asked about first.
	if err := db.CheckSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// pipeline is everything a run needs beside the month it meters: the engine
// database it is written to, the reporting database it is read from, the
// counter sources it measures, and the client that answers the metricsql ones.
// run assembles it for the period it was given, tick for every period that is
// due.
type pipeline struct {
	cfg            config.Config
	engine         *store.Store
	reporting      *source.DB
	counterSources counters.Config
	vm             counters.Querier
}

// openPipeline reads the configuration, gates it over both databases, and opens
// everything a run works through. The caller closes the pipeline.
func openPipeline(ctx context.Context) (*pipeline, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	// The reporting database is where the resources and events of a period come
	// from, so a run without it is refused here rather than at the first query.
	if err := cfg.ValidateReporting(); err != nil {
		return nil, fmt.Errorf("checking the configuration: %w", err)
	}

	// The counter sources are read before either database is dialed: a file the
	// deployment mounted wrong is an operator's mistake, and finding it costs
	// nothing here.
	counterSources, err := counters.Load(cfg.CounterSourcesPath)
	if err != nil {
		return nil, err
	}
	// A VictoriaMetrics client is built only where a source is queried against
	// it, which is what lets a deployment that measures no metricsql counter
	// leave the endpoint unset. The variable is named here rather than gated in
	// config, because whether it is needed is the counter sources' answer.
	var vm counters.Querier
	if counterSources.HasMetricsQL() {
		client, err := counters.NewVMClient(cfg.VMURL, nil)
		if err != nil {
			return nil, fmt.Errorf("TALLY_ENGINE_VM_URL: %w", err)
		}
		vm = client
	}

	engine, err := openEngine(ctx, cfg)
	if err != nil {
		return nil, err
	}
	reporting, err := source.New(ctx, cfg.ReportingDBURL)
	if err != nil {
		engine.Close()
		return nil, err
	}
	return &pipeline{
		cfg:            cfg,
		engine:         engine,
		reporting:      reporting,
		counterSources: counterSources,
		vm:             vm,
	}, nil
}

// Close gives both pools back.
func (p *pipeline) Close() {
	p.reporting.Close()
	p.engine.Close()
}

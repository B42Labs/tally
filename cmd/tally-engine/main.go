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
// The migrate subcommands and periods list work. The rest of the tree is the
// interface the later Phase 3 packages fill in: each of those subcommands
// carries its full flag surface, checks what it was given, and then reports
// that it is not implemented. The command line is fixed here so the packages
// that arrive behind it change what a command does rather than how it is
// called.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.1.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/b42labs/tally/internal/engine/config"
	"github.com/b42labs/tally/internal/engine/store"
)

func main() {
	// Cobra has already written the error to stderr by the time Execute
	// returns it.
	if err := newRootCmd().Execute(); err != nil {
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

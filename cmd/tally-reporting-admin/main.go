// Command tally-reporting-admin provisions and revokes the Reporting API's
// credentials.
//
// The HTTP contract has no credential endpoints: a credential is issued by an
// operator who already holds database access, not over the API that credential
// opens. The CLI therefore works on the reporting database directly, through
// the same TALLY_REPORTING_DB_URL the server reads. On the dev cluster that
// connection goes through the Gateway's postgres listener.
//
// It is also the only thing that runs DDL. Its migrate subcommand applies the
// embedded goose chain, migrate-status reports which of it a database carries,
// and migrate-down-to runs the chain's down migrations. The server migrates
// nothing at startup, so a schema change stays an operator's decision rather
// than a side effect of a pod restart.
//
// A new token is printed once, alone on stdout, so it can be redirected into a
// file or a secret. The database receives its sha256 digest and never the
// token itself.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.4.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/b42labs/tally/internal/reporting/config"
	"github.com/b42labs/tally/internal/reporting/store"
)

// actor is what the audit log records for every change made here. Which person
// ran the CLI is not something the process knows; the database credentials it
// used are.
const actor = "admin-cli"

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
		Use:   "tally-reporting-admin",
		Short: "Provision and revoke the Reporting API's credentials",
		// A database that is down is not a usage mistake, and dumping the help
		// text after one buries the line that says what went wrong.
		SilenceUsage: true,
	}
	root.AddCommand(
		newMigrateCmd(),
		newMigrateStatusCmd(),
		newMigrateDownToCmd(),
		newCreateIngestCredentialCmd(),
		newCreateAPITokenCmd(),
		newRevokeIngestCredentialCmd(),
		newRevokeAPITokenCmd(),
	)
	return root
}

// loadConfig reads the environment and runs the admin gate over it. The
// subcommands call this when they run rather than when they are built, so
// building the tree, and with it --help, needs no configuration at all.
func loadConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, fmt.Errorf("loading the configuration: %w", err)
	}
	if err := cfg.ValidateAdmin(); err != nil {
		return config.Config{}, fmt.Errorf("checking the configuration: %w", err)
	}
	return cfg, nil
}

// openStore is the setup every database-backed subcommand shares: the
// configuration, and a pool on the database it names. The caller closes the
// store.
func openStore(ctx context.Context) (*store.Store, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return store.New(ctx, cfg.DBURL, cfg.DBMaxConns)
}

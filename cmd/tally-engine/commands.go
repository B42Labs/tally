package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/scheduler"
	"github.com/b42labs/tally/internal/engine/store"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

// errNotImplemented is what a subcommand whose package has not arrived yet
// returns once it has checked its flags. The message is the same for all of
// them: which Phase 3 package is missing is nothing an operator can act on,
// that the command is not usable yet is.
var errNotImplemented = errors.New("not implemented: arrives with a later Phase 3 package")

// newMigrateCmd builds the migrate subcommand.
func newMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply the pending migrations of the engine database",
		Long: "Apply the pending migrations of the engine database.\n\n" +
			"No other subcommand runs DDL, so this is what brings a database to the schema the engine expects.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			applied, err := store.Migrate(cmd.Context(), cfg.DBURL)
			if err != nil {
				return err
			}
			if len(applied) == 0 {
				return write(cmd.OutOrStdout(), "nothing to apply")
			}

			lines := make([]string, 0, len(applied))
			for _, version := range applied {
				lines = append(lines, fmt.Sprintf("applied migration %d", version))
			}
			return write(cmd.OutOrStdout(), lines...)
		},
	}
}

// newMigrateStatusCmd builds the migrate-status subcommand.
func newMigrateStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate-status",
		Short: "Report which migrations the engine database carries",
		Long: "Report which migrations the engine database carries.\n\n" +
			"It answers which schema a database is on before code that assumes a newer one is run against it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			status, err := store.MigrationStatus(cmd.Context(), cfg.DBURL)
			if err != nil {
				return err
			}

			lines := make([]string, 0, len(status))
			for _, migration := range status {
				state := "pending"
				if migration.Applied {
					state = "applied"
				}
				lines = append(lines, fmt.Sprintf("migration %d %s", migration.Version, state))
			}
			return write(cmd.OutOrStdout(), lines...)
		},
	}
}

// newMigrateDownToCmd builds the migrate-down-to subcommand.
func newMigrateDownToCmd() *cobra.Command {
	var confirmed bool

	cmd := &cobra.Command{
		Use:   "migrate-down-to <version>",
		Short: "Roll the engine database back to a migration version",
		Long: "Roll the engine database back to a migration version.\n\n" +
			"Every migration above the given version is undone, which drops the data its tables hold. " +
			"Passing 0 leaves an empty schema. The chain ships these down migrations, so this is what runs them.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			version, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("version: %q is not a number: %w", args[0], err)
			}
			if version < 0 {
				return fmt.Errorf("version: %d must not be negative", version)
			}
			// Dropping tables is not something to do because a command line was
			// half-typed, so the intent is stated separately from the target.
			if !confirmed {
				return errors.New("--yes: rolling back drops the data of every migration above the target")
			}

			cfg, err := loadConfig()
			if err != nil {
				return err
			}

			rolledBack, err := store.MigrateDownTo(cmd.Context(), cfg.DBURL, version)
			if err != nil {
				return err
			}
			if len(rolledBack) == 0 {
				return write(cmd.OutOrStdout(), "nothing to roll back")
			}

			lines := make([]string, 0, len(rolledBack))
			for _, applied := range rolledBack {
				lines = append(lines, fmt.Sprintf("rolled back migration %d", applied))
			}
			return write(cmd.OutOrStdout(), lines...)
		},
	}

	cmd.Flags().BoolVar(&confirmed, "yes", false, "confirm that the data of the rolled-back migrations may be dropped")
	return cmd
}

// newPeriodsListCmd builds the periods list subcommand.
func newPeriodsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the billing periods and their status",
		Long: "List the billing periods and their status.\n\n" +
			"A line is a period's YYYY-MM month and the status it carries: open, grace, or finalized. " +
			"A finalized period also names the run that closed it and when that happened.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()

			rows, err := sqlcgen.New(db.Pool()).ListBillingPeriods(cmd.Context())
			if err != nil {
				return fmt.Errorf("listing the billing periods: %w", err)
			}
			if len(rows) == 0 {
				return write(cmd.OutOrStdout(), "no billing periods")
			}

			lines := make([]string, 0, len(rows))
			for _, row := range rows {
				line := period.Format(row.PeriodFrom.Time) + " " + row.Status
				if row.FinalizedRunID.Valid {
					// A pgtype uuid holds the same [16]byte google/uuid wraps, so
					// the conversion is what renders it in the canonical form.
					line += fmt.Sprintf(" finalized_run=%s", uuid.UUID(row.FinalizedRunID.Bytes))
				}
				// The two columns are nullable on their own, so the timestamp is
				// printed only where there is one. A zero time rendered here reads
				// as a close in the year 1 rather than as an absent value, and this
				// listing is what an operator reconciles an ERP against.
				if row.FinalizedAt.Valid {
					line += fmt.Sprintf(" finalized_at=%s", row.FinalizedAt.Time.UTC().Format(time.RFC3339))
				}
				lines = append(lines, line)
			}
			return write(cmd.OutOrStdout(), lines...)
		},
	}
}

// newRunCmd builds the run subcommand.
func newRunCmd() *cobra.Command {
	var month string
	var clouds []string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Meter and rate one billing period",
		Long: "Meter and rate one billing period.\n\n" +
			"The run reads the period's resources and events from the reporting database, derives the usage " +
			"records of every project, and rates them against the imported pricing catalog. It leaves a run " +
			"row the other subcommands work on.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, to, err := validatePeriod(month)
			if err != nil {
				return err
			}

			p, err := openPipeline(cmd.Context())
			if err != nil {
				return err
			}
			defer p.Close()

			result, err := runs.Execute(cmd.Context(), p.engine.Pool(), p.reporting, runs.Options{
				PeriodFrom:               from,
				PeriodTo:                 to,
				Clouds:                   clouds,
				AttributingRelationTypes: p.cfg.AttributingRelationTypes,
				Counters:                 p.counterSources,
				VM:                       p.vm,
			})
			// A run that committed and then failed to give its period lock back
			// billed the month: what it produced is printed and the failure named
			// beside it, rather than leaving the operator with an error and no run
			// id to tell whether the month was billed at all.
			if err != nil && !errors.Is(err, runs.ErrLockReleaseFailed) {
				return err
			}
			lines := runLines(month, result)
			if err != nil {
				lines = append(lines, fmt.Sprintf("warning: %s", err))
			}
			return write(cmd.OutOrStdout(), lines...)
		},
	}

	cmd.Flags().StringVar(&month, "period", "", "billing month to meter, YYYY-MM")
	cmd.Flags().StringSliceVar(&clouds, "clouds", nil,
		"comma-separated clouds to meter; empty meters every configured cloud")
	_ = cmd.MarkFlagRequired("period")
	return cmd
}

// newFinalizeCmd builds the finalize subcommand.
func newFinalizeCmd() *cobra.Command {
	var month, runID string

	cmd := &cobra.Command{
		Use:   "finalize",
		Short: "Finalize a completed run and close its billing period",
		Long: "Finalize a completed run and close its billing period.\n\n" +
			"The run's records become immutable and the period stops taking new ones. What arrives afterwards " +
			"is booked by a correction, which records the difference between the finalized run and a fresh " +
			"metering as credit and debit deltas; the finalized run stays as it is. A completed correction is " +
			"finalized through this same command, which makes its deltas and credit notes immutable and leaves " +
			"the period naming the run that closed it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, _, err := validatePeriod(month)
			if err != nil {
				return err
			}
			id, err := validateRunID(runID)
			if err != nil {
				return err
			}

			// Closing a period reads and writes the engine database alone: the
			// records it turns immutable are already stored.
			db, err := openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()

			kind, err := runs.Finalize(cmd.Context(), db.Pool(), from, id)
			if err != nil {
				return err
			}
			// A correction closes itself alone: the period keeps naming the
			// regular run that closed it, which is what periods list prints.
			if kind == runs.KindCorrection {
				return write(cmd.OutOrStdout(), fmt.Sprintf("correction run %s finalized for %s", id, month))
			}
			return write(cmd.OutOrStdout(), fmt.Sprintf("run %s finalized, period %s closed", id, month))
		},
	}

	cmd.Flags().StringVar(&month, "period", "", "billing month the run bills, YYYY-MM")
	cmd.Flags().StringVar(&runID, "run", "", "id of the run to finalize")
	_ = cmd.MarkFlagRequired("period")
	_ = cmd.MarkFlagRequired("run")
	return cmd
}

// newDetectLateCmd builds the detect-late subcommand.
func newDetectLateCmd() *cobra.Command {
	var month string

	cmd := &cobra.Command{
		Use:   "detect-late",
		Short: "Report the events that reached a metered period late",
		Long: "Report the events that reached a metered period late.\n\n" +
			"It names what the reporting database received after the run that bills the period read it. " +
			"Nothing is changed: booking those events is what correct does.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if _, _, err := validatePeriod(month); err != nil {
				return err
			}
			return errNotImplemented
		},
	}

	cmd.Flags().StringVar(&month, "period", "", "billing month to check, YYYY-MM")
	_ = cmd.MarkFlagRequired("period")
	return cmd
}

// newCorrectCmd builds the correct subcommand.
func newCorrectCmd() *cobra.Command {
	var month string

	cmd := &cobra.Command{
		Use:   "correct",
		Short: "Meter a finalized billing period again as a correction",
		Long: "Meter a finalized billing period again as a correction.\n\n" +
			"The correction run supersedes the run that billed the period and records the difference between " +
			"the two. The finalized run stays as it is, because its numbers may already have reached an ERP.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if _, _, err := validatePeriod(month); err != nil {
				return err
			}
			return errNotImplemented
		},
	}

	cmd.Flags().StringVar(&month, "period", "", "billing month to correct, YYYY-MM")
	_ = cmd.MarkFlagRequired("period")
	return cmd
}

// newPricingImportCmd builds the pricing import subcommand.
func newPricingImportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "import <file>",
		Short: "Import a pricing catalog from a YAML file",
		Long: "Import a pricing catalog from a YAML file.\n\n" +
			"A catalog is imported once and then referred to by its version, which every rated record carries, " +
			"so a price change never rewrites what an earlier run billed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return fmt.Errorf("reading the pricing model: %w", err)
			}
			// What the parse reports is what is wrong with the model, not which
			// file held it, and an operator keeps one file per version.
			model, doc, err := pricing.Parse(data)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}

			db, err := openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()

			alreadyImported, err := pricing.Import(cmd.Context(), sqlcgen.New(db.Pool()), model, doc)
			if err != nil {
				return err
			}
			if alreadyImported {
				return write(cmd.OutOrStdout(), fmt.Sprintf("pricing model %s already imported", model.Version))
			}
			return write(cmd.OutOrStdout(), fmt.Sprintf("imported pricing model %s valid from %s",
				model.Version, model.ValidFrom.Format(time.RFC3339)))
		},
	}
}

// newPricingListCmd builds the pricing list subcommand.
func newPricingListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the imported pricing catalogs",
		Long: "List the imported pricing catalogs.\n\n" +
			"A line is a catalog's version and when it was imported, which is what a run's pricing version " +
			"refers back to.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()

			rows, err := sqlcgen.New(db.Pool()).ListPricingModels(cmd.Context())
			if err != nil {
				return fmt.Errorf("listing the pricing models: %w", err)
			}
			if len(rows) == 0 {
				return write(cmd.OutOrStdout(), "no pricing models")
			}

			lines := make([]string, 0, len(rows))
			for _, row := range rows {
				lines = append(lines, fmt.Sprintf("%s valid_from=%s currency=%s imported_at=%s",
					row.Version,
					row.ValidFrom.Time.UTC().Format(time.RFC3339),
					row.Currency,
					row.ImportedAt.Time.UTC().Format(time.RFC3339)))
			}
			return write(cmd.OutOrStdout(), lines...)
		},
	}
}

// newExportCmd builds the export subcommand.
func newExportCmd() *cobra.Command {
	var runID, format, out string

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export the project statements of a run",
		Long: "Export the project statements of a run.\n\n" +
			"The statements are written into the output directory, as json or as csv. Exporting a finalized " +
			"run twice yields the same files, because a finalized run's records no longer change.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if _, err := validateRunID(runID); err != nil {
				return err
			}
			if err := validateFormat(format); err != nil {
				return err
			}
			return errNotImplemented
		},
	}

	cmd.Flags().StringVar(&runID, "run", "", "id of the run to export")
	cmd.Flags().StringVar(&format, "format", "", "output format: json or csv")
	cmd.Flags().StringVar(&out, "out", "", "directory the exported files are written to")
	_ = cmd.MarkFlagRequired("run")
	_ = cmd.MarkFlagRequired("format")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

// newTickCmd builds the tick subcommand.
func newTickCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tick",
		Short: "Run the scheduler tick the hourly CronJob invokes",
		Long: "Run the scheduler tick the hourly CronJob invokes.\n\n" +
			"It advances every billing period whose next step is due: a period whose grace window has passed " +
			"gets its run, and a completed run is finalized where TALLY_ENGINE_AUTO_FINALIZE allows it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := openPipeline(cmd.Context())
			if err != nil {
				return err
			}
			defer p.Close()

			report, tickErr := scheduler.Tick(cmd.Context(), p.engine.Pool(), time.Now().UTC(), scheduler.Options{
				GraceHours:   p.cfg.GraceHours,
				AutoFinalize: p.cfg.AutoFinalize,
				// A scheduled run meters every cloud: which clouds a month is
				// billed over is an operator's choice, and the tick has none.
				Execute: func(ctx context.Context, from, to time.Time) (uuid.UUID, error) {
					result, err := runs.Execute(ctx, p.engine.Pool(), p.reporting, runs.Options{
						PeriodFrom:               from,
						PeriodTo:                 to,
						AttributingRelationTypes: p.cfg.AttributingRelationTypes,
						Counters:                 p.counterSources,
						VM:                       p.vm,
					})
					return result.RunID, err
				},
			})
			// What the tick did is printed before what went wrong with it is
			// returned: a walk that broke on one month still moved the others,
			// and the exit status is what the CronJob reads the failure from.
			if err := write(cmd.OutOrStdout(), tickLines(report)...); err != nil {
				return err
			}
			return tickErr
		},
	}
}

// runLines is what run prints for a finished run: the runs of the period it
// took over, what it produced, and the warnings it recorded.
func runLines(month string, result runs.Result) []string {
	var lines []string
	for _, id := range result.Reclaimed {
		lines = append(lines, fmt.Sprintf("reclaimed stale run %s", id))
	}
	lines = append(lines,
		fmt.Sprintf("run %s completed for %s with pricing model %s", result.RunID, month, result.PricingVersion),
		fmt.Sprintf("metered %d candidates into %d usage records, %d rated records and %d project statements",
			result.Stats.Candidates, result.Stats.UsageRecords, result.Stats.RatedRecords, result.Stats.Statements))
	for _, id := range result.Superseded {
		lines = append(lines, fmt.Sprintf("superseded run %s", id))
	}
	for _, warning := range result.Stats.Warnings {
		lines = append(lines, fmt.Sprintf("warning: %s: %s", warning.Code, warning.Detail))
	}

	// The findings of the passes are counted rather than printed: a period of a
	// large deployment carries thousands of them, and runs.stats holds every one
	// in full for the operator who goes looking.
	stats := result.Stats
	recorded := len(stats.MeteringWarnings) + len(stats.CounterWarnings) + len(stats.AttributionWarnings) +
		len(stats.Unpriced) + len(stats.Unreadable) + len(stats.UnregisteredProjects)
	if recorded > 0 {
		lines = append(lines, fmt.Sprintf(
			"warnings recorded in runs.stats: %d metering, %d counter, %d attribution, "+
				"%d unpriced resource types, %d unreadable fields, %d unregistered projects",
			len(stats.MeteringWarnings), len(stats.CounterWarnings), len(stats.AttributionWarnings),
			len(stats.Unpriced), len(stats.Unreadable), len(stats.UnregisteredProjects)))
	}
	return lines
}

// tickLines is what tick prints: one line per step a month took, in the order
// the lifecycle takes them. A month the tick looked at and left alone prints
// nothing, and a tick where that was every month says so rather than printing
// nothing at all, which reads like a tick that never ran. A month the tick held
// back prints too: a month whose runs keep failing is retried ever more rarely,
// and nothing else would say that it is waiting rather than fine. So do the
// months the walk's cap left out entirely, and a month that was billed by a run
// whose period lock stayed behind.
//
// The completion line is left out where the month was closed as well. The two
// steps are one run's, and a tick that finalized a month an earlier tick
// metered reports that run without having executed it.
func tickLines(report scheduler.Report) []string {
	var lines []string
	for _, month := range report {
		// The months the walk is capped at print before the oldest month it did
		// reach, because that is where they end. No tick ever reaches them again.
		if month.SkippedBefore > 0 {
			lines = append(lines, fmt.Sprintf(
				"%d months before %s were skipped, and are billed with tally-engine run --period",
				month.SkippedBefore, month.Month))
		}
		if month.Transition != "" {
			lines = append(lines, fmt.Sprintf("%s %s", month.Month, month.Transition))
		}
		switch {
		case month.Finalized:
			lines = append(lines, fmt.Sprintf("%s run %s finalized", month.Month, month.RunID))
		case month.RunID != uuid.Nil:
			lines = append(lines, fmt.Sprintf("%s run %s completed", month.Month, month.RunID))
		}
		if month.Warning != nil {
			lines = append(lines, fmt.Sprintf("%s warning: %s", month.Month, month.Warning))
		}
		if !month.RetryAfter.IsZero() {
			lines = append(lines, fmt.Sprintf("%s not metered after %d failed runs, retried from %s",
				month.Month, month.Failures, month.RetryAfter.UTC().Format(time.RFC3339)))
		}
		if month.Err != nil {
			lines = append(lines, fmt.Sprintf("%s failed: %s", month.Month, month.Err))
		}
	}
	if len(lines) == 0 {
		return []string{"nothing due"}
	}
	return lines
}

// validatePeriod checks the shape of a --period flag and returns the interval
// the month covers. That the operator named a month at all is checked before
// anything else happens, so a mistyped flag never reaches a configuration or a
// database.
func validatePeriod(value string) (from, to time.Time, err error) {
	from, to, err = period.Parse(value)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--period: %w", err)
	}
	return from, to, nil
}

// validateRunID checks that a --run flag names a run in the form the run ids
// are stored in, and returns the run it names.
func validateRunID(value string) (uuid.UUID, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("--run: %q is not a uuid: %w", value, err)
	}
	return id, nil
}

// validateFormat checks a --format flag against the two formats the export
// writes.
func validateFormat(value string) error {
	if value != "json" && value != "csv" {
		return fmt.Errorf("--format: %q must be json or csv", value)
	}
	return nil
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

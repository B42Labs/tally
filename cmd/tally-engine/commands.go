package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/spf13/cobra"

	"github.com/b42labs/tally/internal/engine/export"
	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/scheduler"
	"github.com/b42labs/tally/internal/engine/store"
	"github.com/b42labs/tally/internal/engine/store/sqlcgen"
)

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
				AdjustmentRelationTypes:  p.cfg.AdjustmentRelationTypes,
				AdjustmentDepth:          p.cfg.AdjustmentDepth,
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
		RunE: func(cmd *cobra.Command, _ []string) error {
			from, to, err := validatePeriod(month)
			if err != nil {
				return err
			}

			// Nothing is metered here, so the counter sources are not read: a
			// machine that does not carry the file still lists the late events.
			dbs, err := openDatabases(cmd.Context())
			if err != nil {
				return err
			}
			defer dbs.Close()

			report, err := runs.DetectLate(cmd.Context(), dbs.engine.Pool(), dbs.reporting, from, to)
			if err != nil {
				return err
			}
			return write(cmd.OutOrStdout(), detectLateLines(month, report)...)
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
			"The correction meters the month again from the full event history with the pricing version the " +
			"finalized run used, stores every non-zero difference against the latest finalized run as a credit " +
			"or debit delta, and renders one credit note per affected project. The finalized run stays as it " +
			"is, because its numbers may already have reached an ERP; a completed correction is finalized with " +
			"finalize, and the next correction diffs against it.",
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

			result, err := runs.Correct(cmd.Context(), p.engine.Pool(), p.reporting, runs.CorrectOptions{
				PeriodFrom:               from,
				PeriodTo:                 to,
				AttributingRelationTypes: p.cfg.AttributingRelationTypes,
				AdjustmentRelationTypes:  p.cfg.AdjustmentRelationTypes,
				AdjustmentDepth:          p.cfg.AdjustmentDepth,
				Counters:                 p.counterSources,
				VM:                       p.vm,
			})
			// A correction that committed and then failed to give its period lock
			// back booked the deltas: what it produced is printed and the failure
			// named beside it, rather than leaving the operator with an error and
			// no run id to tell whether the month was corrected at all.
			if err != nil && !errors.Is(err, runs.ErrLockReleaseFailed) {
				return err
			}
			lines := correctLines(month, result)
			if err != nil {
				lines = append(lines, fmt.Sprintf("warning: %s", err))
			}
			return write(cmd.OutOrStdout(), lines...)
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

// The two formats the export writes. They name the exporter the command builds
// and the lines it prints, so the two never disagree about which of them a
// --format the validation let through belongs to.
const (
	formatJSON = "json"
	formatCSV  = "csv"
)

// newExportCmd builds the export subcommand.
func newExportCmd() *cobra.Command {
	var runID, format, out string

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export the project statements of a run",
		Long: "Export the project statements of a run.\n\n" +
			"json writes run.json and one statement document per project, or one credit note per project " +
			"for a correction run. csv writes the rated records into rated.csv, and the deltas of a " +
			"correction into deltas.csv. Both formats write the run's partner settlement beside them, " +
			"kickbacks.json or kickbacks.csv, empty when the run owes nobody. --out has to be empty or " +
			"absent, so what it holds afterwards is one run's artifacts and nothing an earlier export of " +
			"another run left there. Exporting a finalized run twice into a clean directory yields the " +
			"same files, because a finalized run's records no longer change.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, err := validateRunID(runID)
			if err != nil {
				return err
			}
			if err := validateFormat(format); err != nil {
				return err
			}
			if err := validateOut(out); err != nil {
				return err
			}

			// Everything an export renders was written to the engine database by
			// the run, so this reads that one database.
			db, err := openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()

			// The run is read before an exporter is built, and an exporter
			// creates its directory only once it has rendered everything: a run
			// id no row carries, and a run no export is produced from, leave the
			// --out directory uncreated.
			run, err := export.Load(cmd.Context(), db.Pool(), id)
			if err != nil {
				return err
			}

			// A completed run is not immutable: a run of the same period
			// supersedes it, and an export of a superseded run hands an ERP
			// numbers that no longer bill the month. The status is read again
			// outside the snapshot Load read under, so a run that moved while its
			// records were being read is refused rather than written out. What is
			// left after this is the rendering and the writing, which the row
			// lock the export deliberately does not take would not cover either.
			current, err := sqlcgen.New(db.Pool()).GetRun(cmd.Context(), pgtype.UUID{Bytes: id, Valid: true})
			if err != nil {
				return fmt.Errorf("re-reading the run %s: %w", id, err)
			}
			if current.Status != run.Status {
				return fmt.Errorf("run %s became %s while it was read, and was not exported", id, current.Status)
			}

			// The format names the implementation. Every consumer of billing
			// artifacts sits behind BillingExporter, so an ERP adapter is chosen
			// here rather than written into the command.
			var exporter export.BillingExporter
			switch format {
			case formatJSON:
				exporter = export.JSONFiles{Dir: out}
			case formatCSV:
				exporter = export.CSVFiles{Dir: out}
			}

			// --out is read again for the reason the status is: what the flag
			// check looked at, it looked at before a month of rated records was
			// read, and a directory that was empty then is not one that is still
			// empty minutes later. What is left after this is the rendering and
			// the writing, which is as narrow as this gets without a lock.
			if err := validateOut(out); err != nil {
				return err
			}
			if err := exporter.Export(cmd.Context(), run); err != nil {
				return err
			}
			return write(cmd.OutOrStdout(), exportLines(period.Format(run.PeriodFrom), format, out, run)...)
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

// newKickbacksCmd builds the kickbacks subcommand.
func newKickbacksCmd() *cobra.Command {
	var month, runID, format, beneficiary string

	cmd := &cobra.Command{
		Use:   "kickbacks",
		Short: "Report the kickbacks a run owes its partners",
		Long: "Report the kickbacks a run owes its partners.\n\n" +
			"The document lists, per partner and currency, the kickback total, the number of projects it came " +
			"from and one entry per kickback record. json prints the settlement document, csv one row per " +
			"record. A month alone reports the regular run that bills it; a correction named with --run " +
			"reports the differences to the run it corrects, negative where usage was corrected down. Only a " +
			"completed or finalized run is reported. A partner named with --beneficiary is reported alone, " +
			"which is what that partner receives, and a partner the run settles nothing for is refused; " +
			"without it the document holds every partner the run owes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The flags are checked before a configuration is read, so an
			// operator who mistyped one hears about that flag rather than about
			// the environment of the machine the command ran on.
			from, _, err := validatePeriod(month)
			if err != nil {
				return err
			}
			var id uuid.UUID
			if runID != "" {
				if id, err = validateRunID(runID); err != nil {
					return err
				}
			}
			if err := validateFormat(format); err != nil {
				return err
			}

			// Everything a report renders was written to the engine database by
			// the run, so this reads that one database.
			db, err := openStore(cmd.Context())
			if err != nil {
				return err
			}
			defer db.Close()

			// A month named without a run is the regular run that bills it: what
			// a partner is settled for a month is what that run settled, and the
			// differences a correction booked on top are reached with --run.
			if runID == "" {
				if id, err = export.PeriodRun(cmd.Context(), db.Pool(), from); err != nil {
					return err
				}
			}

			// The settlement alone: this report renders none of the statements
			// and none of the rated records a full export does, and a month of
			// them is tens of thousands of rows to read and decode.
			run, err := export.LoadKickbacks(cmd.Context(), db.Pool(), id)
			if err != nil {
				return err
			}
			// A run of another month settles what that month owed, so it is
			// refused rather than reported under the month the operator named:
			// this document is what a partner is paid from.
			if !run.PeriodFrom.Equal(from) {
				return fmt.Errorf("%w: run %s bills %s, not %s",
					runs.ErrPeriodMismatch, id, period.Format(run.PeriodFrom), month)
			}

			// The status is read again outside the snapshot Load read under, the
			// way the export reads it: a completed run is superseded by a second
			// run of its period, and a superseded run bills nobody, so a
			// settlement rendered from one names a payout no partner is owed.
			current, err := sqlcgen.New(db.Pool()).GetRun(cmd.Context(), pgtype.UUID{Bytes: id, Valid: true})
			if err != nil {
				return fmt.Errorf("re-reading the run %s: %w", id, err)
			}
			if current.Status != run.Status {
				return fmt.Errorf("run %s became %s while it was read, and was not reported", id, current.Status)
			}

			// A partner named with --beneficiary is settled from their own
			// kickbacks alone. The document holds every partner the run owes,
			// which is what finance reconciles a month with, and a copy of that
			// document handed to one partner names what the others are paid and,
			// through the base of every breakdown entry, what each customer
			// project was billed.
			//
			// A name the run settles nothing under is refused rather than
			// reported: the filter compares adjustment_records.beneficiary
			// exactly, so a mistyped name leaves a well-formed document naming
			// the run, the kind and the month with an empty list of partners
			// under it, and nothing in that document, and no exit status beside
			// it, tells a partner mailer or an ERP importer a typo apart from a
			// month the partner is genuinely owed nothing for.
			if beneficiary != "" {
				if !slices.ContainsFunc(run.Kickbacks, func(k export.Kickback) bool {
					return k.Beneficiary == beneficiary
				}) {
					return fmt.Errorf("run %s settles no kickbacks for %s, so there is no settlement to report",
						id, beneficiary)
				}
				run.Kickbacks = slices.DeleteFunc(run.Kickbacks, func(k export.Kickback) bool {
					return k.Beneficiary != beneficiary
				})
			}

			var body []byte
			switch format {
			case formatJSON:
				body, err = export.KickbacksJSON(run)
			case formatCSV:
				body, err = export.KickbacksCSV(run)
			}
			if err != nil {
				return err
			}

			// One write, and nothing else goes to stdout: the report pipes into
			// a file or a partner mailer as it is. Every refusal reaches stderr
			// through cobra.
			if _, err := cmd.OutOrStdout().Write(body); err != nil {
				return fmt.Errorf("writing the output: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&month, "period", "", "billing month to report, YYYY-MM")
	cmd.Flags().StringVar(&runID, "run", "", "id of the run to report; the month's regular run when absent")
	cmd.Flags().StringVar(&format, "format", formatJSON, "output format: json or csv")
	cmd.Flags().StringVar(&beneficiary, "beneficiary", "",
		"report only this partner's kickbacks; every partner the run owes when absent")
	_ = cmd.MarkFlagRequired("period")
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
						AdjustmentRelationTypes:  p.cfg.AdjustmentRelationTypes,
						AdjustmentDepth:          p.cfg.AdjustmentDepth,
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
	// A deployment whose project graph carries no adjustments gets no line at
	// all: a run that applied none is the ordinary case, and a zero printed
	// beside every run reads as a setting that was turned off by mistake.
	if result.Stats.AdjustmentRecords > 0 {
		lines = append(lines, fmt.Sprintf("applied %d pricing adjustments", result.Stats.AdjustmentRecords))
	}
	for _, id := range result.Superseded {
		lines = append(lines, fmt.Sprintf("superseded run %s", id))
	}
	for _, warning := range result.Stats.Warnings {
		lines = append(lines, fmt.Sprintf("warning: %s: %s", warning.Code, warning.Detail))
	}

	if line, ok := recordedWarningsLine(result.Stats); ok {
		lines = append(lines, line)
	}
	return lines
}

// exportLines is what export prints for a finished export: the run it wrote,
// the month that run bills, and one line per file the format left in the
// directory. The counts say how much of the month is in those files, so a run
// that billed nobody says so here rather than in a listing of the directory.
// The partner settlement, kickbacks.json or kickbacks.csv, is written by both
// formats, so its line stands under either of them.
func exportLines(month, format, out string, run export.Run) []string {
	lines := []string{fmt.Sprintf("run %s exported for %s as %s into %s", run.ID, month, format, out)}
	correction := run.Kind == runs.KindCorrection
	// What a regular run settles for a partner is a kickback, and what a
	// correction settles is the difference to the run it corrects.
	kickbacks := "kickbacks"
	if correction {
		kickbacks = "kickback deltas"
	}

	switch format {
	case formatJSON:
		// What a regular run bills a project is a statement, and what a
		// correction hands it is a credit note, which is what the files are
		// named after too.
		documents := "statements"
		if correction {
			documents = "credit notes"
		}
		lines = append(lines,
			fmt.Sprintf("wrote run.json and %d %s", len(run.Statements), documents),
			fmt.Sprintf("wrote kickbacks.json with %d %s", len(run.Kickbacks), kickbacks))
	case formatCSV:
		lines = append(lines, fmt.Sprintf("wrote rated.csv with %d rated records", len(run.Rated)))
		if correction {
			lines = append(lines, fmt.Sprintf("wrote deltas.csv with %d deltas", len(run.Deltas)))
		}
		lines = append(lines, fmt.Sprintf("wrote kickbacks.csv with %d %s", len(run.Kickbacks), kickbacks))
	}
	return lines
}

// detectLateLines is what detect-late prints: the run the events are held
// against and when it read the period, then the resources whose events arrived
// after that, and how a period that has some is booked. The resources the
// report left out are counted rather than named, the way recordedWarningsLine
// counts: what a correction re-meters is a number, and the fleet decides how
// big it is.
func detectLateLines(month string, report runs.LateReport) []string {
	lines := []string{
		fmt.Sprintf("run %s read %s at %s", report.RunID, month, report.SnapshotAt.UTC().Format(time.RFC3339)),
	}
	if len(report.Resources) == 0 {
		return append(lines, "no events arrived later")
	}

	for _, late := range report.Resources {
		lines = append(lines, fmt.Sprintf("%s/%s/%s/%s: %d late events, last received %s",
			late.Resource.Cloud, late.Resource.Platform, late.Resource.ResourceType, late.Resource.ResourceID,
			late.Events, late.LastReceivedAt.UTC().Format(time.RFC3339)))
	}
	if report.Truncated > 0 {
		lines = append(lines, fmt.Sprintf("and %d more resources with late events", report.Truncated))
	}
	return append(lines, fmt.Sprintf("book them with tally-engine correct --period %s", month))
}

// correctLines is what correct prints for a finished correction: the runs of
// the period it took over, the finalized run it diffed against, what it
// metered, and the deltas it wrote. A correction that found none says so, which
// is the answer an operator who ran it after a detect-late gets.
func correctLines(month string, result runs.CorrectionResult) []string {
	var lines []string
	for _, id := range result.Reclaimed {
		lines = append(lines, fmt.Sprintf("reclaimed stale run %s", id))
	}
	lines = append(lines,
		fmt.Sprintf("run %s completed as a correction of run %s for %s with pricing model %s",
			result.RunID, result.CorrectsRunID, month, result.PricingVersion),
		fmt.Sprintf("metered %d candidates into %d usage records and %d rated records",
			result.Stats.Candidates, result.Stats.UsageRecords, result.Stats.RatedRecords))
	// What a correction changed about a discount or a kickback is booked as an
	// adjustment delta, which the rated count does not carry: a correction that
	// re-rated nothing and moved a partner's kickback all the same would read as
	// a month that stands.
	switch {
	case result.Stats.AdjustmentDeltas > 0:
		lines = append(lines, fmt.Sprintf("%d deltas and %d adjustment deltas in %d credit notes",
			result.Stats.Deltas, result.Stats.AdjustmentDeltas, result.Stats.Statements))
	case result.Stats.Deltas > 0:
		lines = append(lines, fmt.Sprintf("%d deltas in %d credit notes",
			result.Stats.Deltas, result.Stats.Statements))
	default:
		lines = append(lines, fmt.Sprintf("no deltas: the finalized numbers of %s stand", month))
	}
	for _, id := range result.Superseded {
		lines = append(lines, fmt.Sprintf("superseded correction run %s", id))
	}
	for _, warning := range result.Stats.Warnings {
		lines = append(lines, fmt.Sprintf("warning: %s: %s", warning.Code, warning.Detail))
	}

	if line, ok := recordedWarningsLine(result.Stats.Stats); ok {
		lines = append(lines, line)
	}
	return lines
}

// recordedWarningsLine counts the findings of the passes rather than printing
// them: a period of a large deployment carries thousands of them, and runs.stats
// holds every one in full for the operator who goes looking. A pass that found
// nothing gets no line.
func recordedWarningsLine(stats runs.Stats) (string, bool) {
	recorded := len(stats.MeteringWarnings) + len(stats.CounterWarnings) + len(stats.AttributionWarnings) +
		len(stats.AdjustmentWarnings) + len(stats.Unpriced) + len(stats.Unreadable) +
		len(stats.UnregisteredProjects)
	if recorded == 0 {
		return "", false
	}
	return fmt.Sprintf(
		"warnings recorded in runs.stats: %d metering, %d counter, %d attribution, %d adjustment, "+
			"%d unpriced resource types, %d unreadable fields, %d unregistered projects",
		len(stats.MeteringWarnings), len(stats.CounterWarnings), len(stats.AttributionWarnings),
		len(stats.AdjustmentWarnings), len(stats.Unpriced), len(stats.Unreadable),
		len(stats.UnregisteredProjects)), true
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
	if value != formatJSON && value != formatCSV {
		return fmt.Errorf("--format: %q must be %s or %s", value, formatJSON, formatCSV)
	}
	return nil
}

// validateOut checks a --out against what is already in it. The directory an
// export writes into holds one run's files and only that run's: nothing
// enumerates or removes what is there, so a correction exported over the
// regular run's drop directory would leave that run's 500 statements beside the
// credit notes, and an ERP that reads the directory bills every project of the
// month a second time. An operator who means to replace an export empties the
// directory, which is the one step that says so.
//
// The check is not a lock: two exports that name one directory both pass it,
// and it is the writing that would have to be exclusive. It is read when the
// flags are checked, so an operator hears about the directory before a month is
// read, and again immediately before the export writes, where what is left of
// the window between the two is the rendering rather than four queries over a
// month of rated records.
func validateOut(value string) error {
	entries, err := os.ReadDir(value)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("--out: reading %s: %w", value, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("--out: %s is not empty, and an export does not remove what an earlier one left there", value)
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

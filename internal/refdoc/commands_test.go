package refdoc

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// newFixtureRoot builds a command tree carrying every shape the roots of this
// repository use: a group with a leaf under it, arguments in a Use line, a Long
// that repeats the Short, a required flag, a hidden flag, and the flag types
// the commands declare.
func newFixtureRoot() *cobra.Command {
	var (
		period  string
		clouds  []string
		labels  []string
		seed    uint64
		factor  float64
		wait    time.Duration
		dryRun  bool
		verbose bool
	)
	nothing := func(*cobra.Command, []string) {}

	root := &cobra.Command{
		Use:   "tally-fixture",
		Short: "Meter and rate the fixture",
	}

	migrate := &cobra.Command{
		Use:   "migrate-down-to <version>",
		Short: "Roll the fixture database back to a migration version",
		Long:  "Roll the fixture database back to a migration version",
		Run:   nothing,
	}

	export := &cobra.Command{
		Use:   "export",
		Short: "Export the statements of a run",
		Long: "Export the statements of a run.\n\n" +
			"A rollup writes one document per group, rollup-<key>.json or rollup.csv, " +
			"summing the statements of its members.",
		Run: nothing,
	}
	export.Flags().StringVar(&period, "period", "", "billing month to export, as YYYY-MM")
	export.Flags().StringSliceVar(&clouds, "clouds", nil, "clouds to export, comma-separated")
	export.Flags().StringArrayVar(&labels, "label", nil, "label to stamp on every document")
	export.Flags().Uint64Var(&seed, "seed", 1, "seed of the month's shape")
	export.Flags().Float64Var(&factor, "factor", 1.5, "virtual seconds per wall second")
	export.Flags().DurationVar(&wait, "wait", 30*time.Second, "how long to wait for a consumer")
	export.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be written and write nothing")
	export.Flags().BoolVar(&verbose, "verbose", false, "log every statement the run touched")
	_ = export.Flags().MarkHidden("verbose")
	_ = export.MarkFlagRequired("period")

	pricing := &cobra.Command{
		Use:   "pricing",
		Short: "Work with the pricing catalogs",
	}
	pricing.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the imported pricing catalogs",
		Run:   nothing,
	})

	root.AddCommand(
		migrate,
		export,
		pricing,
		&cobra.Command{Use: "secret", Short: "Never on a page", Hidden: true, Run: nothing},
		&cobra.Command{Use: "completion", Short: "Generate the shell completion script", Run: nothing},
		&cobra.Command{Use: "help", Short: "Help about any command", Run: nothing},
	)
	return root
}

func TestCommands(t *testing.T) {
	got, err := Commands(newFixtureRoot())
	if err != nil {
		t.Fatalf("Commands() error = %v, want nil", err)
	}

	assertWant(t, "commands.want.md", got)
}

func TestCommandsRendersEachCommandShape(t *testing.T) {
	got, err := Commands(newFixtureRoot())
	if err != nil {
		t.Fatalf("Commands() error = %v, want nil", err)
	}

	for _, want := range []string{
		// The arguments of a Use line belong in the heading, and a nested
		// command is headed by its whole path.
		"### `tally-fixture migrate-down-to <version>`",
		"### `tally-fixture pricing list`",
		// A group lists what it holds.
		"| Subcommand | Purpose |\n| --- | --- |\n| `list` | List the imported pricing catalogs |",
		// A required flag, the two list shapes, and the defaults that say
		// nothing.
		"| `--period` | string | none | yes | billing month to export, as YYYY-MM |",
		"| `--clouds` | list, comma-separated | none | no |",
		"| `--label` | string, repeatable | none | no |",
		"| `--seed` | integer | `1` | no |",
		"| `--factor` | number | `1.5` | no |",
		"| `--wait` | duration | `30s` | no |",
		"| `--dry-run` | boolean | `false` | no |",
		// A placeholder in the prose is a code span rather than markup.
		"`rollup-<key>.json`",
		// A command without flags says so.
		"This command takes no flags.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

func TestCommandsLeavesOutWhatBelongsToCobra(t *testing.T) {
	got, err := Commands(newFixtureRoot())
	if err != nil {
		t.Fatalf("Commands() error = %v, want nil", err)
	}

	for _, absent := range []string{"secret", "Never on a page", "completion", "Help about any command", "verbose"} {
		if strings.Contains(got, absent) {
			t.Errorf("the rendering carries %q, want it left out:\n%s", absent, got)
		}
	}
}

func TestCommandsDoesNotRepeatALongThatEqualsTheShort(t *testing.T) {
	got, err := Commands(newFixtureRoot())
	if err != nil {
		t.Fatalf("Commands() error = %v, want nil", err)
	}

	short := "Roll the fixture database back to a migration version"
	if n := strings.Count(got, short); n != 2 {
		// Once in the root's subcommand table and once as the command's own
		// paragraph. A third is the Long repeating the Short.
		t.Errorf("the short appears %d times, want 2:\n%s", n, got)
	}
}

func TestCommandsRejectsANilCommand(t *testing.T) {
	_, err := Commands(nil)
	if err == nil {
		t.Fatal("Commands(nil) error = nil, want an error")
	}
	if want := "refdoc: nil command"; err.Error() != want {
		t.Errorf("Commands(nil) error = %q, want %q", err, want)
	}
}

func TestCommandsRendersNothingForAnUnavailableRoot(t *testing.T) {
	// A root that neither runs nor holds a runnable command is not a tool
	// anybody invokes, so there is nothing to write about it.
	got, err := Commands(&cobra.Command{Use: "empty", Short: "Runs nothing"})
	if err != nil {
		t.Fatalf("Commands() error = %v, want nil", err)
	}
	if got != "" {
		t.Errorf("Commands() = %q, want nothing", got)
	}
}

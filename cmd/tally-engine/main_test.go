package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/engine/config"
	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/scheduler"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/store/storetest"
	reportingtest "github.com/b42labs/tally/internal/reporting/store/storetest"
	enginemigrations "github.com/b42labs/tally/migrations/engine"
)

// runID stands in for a run wherever a subcommand takes one. Nothing has to
// exist behind it: the cases that pass it stop at their flags or end in the
// not-implemented report. The periods list test seeds the run it names itself,
// because billing_periods.finalized_run_id points at runs.
const runID = "3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4"

// notImplemented is what every subcommand a later Phase 3 package fills in
// reports once its flags check out.
const notImplemented = "not implemented: arrives with a later Phase 3 package"

func TestHelpNeedsNoEnvironment(t *testing.T) {
	blankEnvironment(t)

	// The root and every leaf of the tree. Building the tree reads no
	// configuration, so the help of all of them works on a machine that has
	// none.
	for _, tc := range []struct {
		name string
		path []string
	}{
		{"tally-engine", nil},
		{"migrate", []string{"migrate"}},
		{"migrate-status", []string{"migrate-status"}},
		{"migrate-down-to", []string{"migrate-down-to"}},
		{"periods list", []string{"periods", "list"}},
		{"run", []string{"run"}},
		{"finalize", []string{"finalize"}},
		{"detect-late", []string{"detect-late"}},
		{"correct", []string{"correct"}},
		{"pricing import", []string{"pricing", "import"}},
		{"pricing list", []string{"pricing", "list"}},
		{"export", []string{"export"}},
		{"tick", []string{"tick"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, err := runCLI(t, append(tc.path, "--help")...)
			if err != nil {
				t.Fatalf("%s --help error = %v, want nil (stderr %q)", tc.name, err, stderr)
			}
			if want := "Usage:"; !strings.Contains(stdout, want) {
				t.Errorf("stdout = %q, want the usage text, containing %q", stdout, want)
			}
		})
	}
}

func TestMigrateCLI(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("applies the chain to a database that has none of it", func(t *testing.T) {
		useDatabase(t, db.NewSiblingDB(t, "migrate_cli"))

		stdout, stderr, err := runCLI(t, "migrate")
		if err != nil {
			t.Fatalf("migrate error = %v, want nil (stderr %q)", err, stderr)
		}
		for _, version := range chainVersions() {
			if want := fmt.Sprintf("applied migration %d", version); !strings.Contains(stdout, want) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, want)
			}
		}

		stdout, stderr, err = runCLI(t, "migrate")
		if err != nil {
			t.Fatalf("the second migrate error = %v, want nil (stderr %q)", err, stderr)
		}
		if want := "nothing to apply\n"; stdout != want {
			t.Errorf("stdout of the second run = %q, want %q", stdout, want)
		}
	})

	t.Run("migrate-status names the version of every migration", func(t *testing.T) {
		useDatabase(t, db.NewSiblingDB(t, "status_cli"))

		stdout, stderr, err := runCLI(t, "migrate-status")
		if err != nil {
			t.Fatalf("migrate-status error = %v, want nil (stderr %q)", err, stderr)
		}
		if want := migrationStatusOutput("pending"); stdout != want {
			t.Errorf("stdout = %q, want %q", stdout, want)
		}

		if _, stderr, err = runCLI(t, "migrate"); err != nil {
			t.Fatalf("migrate error = %v, want nil (stderr %q)", err, stderr)
		}

		stdout, stderr, err = runCLI(t, "migrate-status")
		if err != nil {
			t.Fatalf("migrate-status error = %v, want nil (stderr %q)", err, stderr)
		}
		if want := migrationStatusOutput("applied"); stdout != want {
			t.Errorf("stdout after migrating = %q, want %q", stdout, want)
		}
	})

	t.Run("migrate-down-to runs the chain's down migrations", func(t *testing.T) {
		useDatabase(t, db.NewSiblingDB(t, "down_cli"))
		if _, stderr, err := runCLI(t, "migrate"); err != nil {
			t.Fatalf("migrate error = %v, want nil (stderr %q)", err, stderr)
		}

		t.Run("and refuses to without --yes", func(t *testing.T) {
			_, _, err := runCLI(t, "migrate-down-to", "0")
			if err == nil {
				t.Fatal("migrate-down-to error = nil, want the missing confirmation reported")
			}
			if want := "--yes"; !strings.Contains(err.Error(), want) {
				t.Errorf("migrate-down-to error = %q, want it to mention %q", err, want)
			}
		})

		stdout, stderr, err := runCLI(t, "migrate-down-to", "0", "--yes")
		if err != nil {
			t.Fatalf("migrate-down-to error = %v, want nil (stderr %q)", err, stderr)
		}
		for _, version := range chainVersions() {
			if want := fmt.Sprintf("rolled back migration %d", version); !strings.Contains(stdout, want) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, want)
			}
		}

		// The rolled-back database takes the chain again, which is what makes
		// the down migrations a way back rather than a dead end.
		stdout, stderr, err = runCLI(t, "migrate")
		if err != nil {
			t.Fatalf("migrate after the rollback error = %v, want nil (stderr %q)", err, stderr)
		}
		for _, version := range chainVersions() {
			if want := fmt.Sprintf("applied migration %d", version); !strings.Contains(stdout, want) {
				t.Errorf("stdout after the rollback = %q, want it to contain %q", stdout, want)
			}
		}
	})

	t.Run("migrate-down-to refuses a version it cannot use", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			args []string
			want string
		}{
			{"a version that is not a number", []string{"migrate-down-to", "latest", "--yes"}, "not a number"},
			// Everything after -- is an argument, which is what keeps cobra from
			// reading -1 as a bundle of shorthand flags.
			{"a negative version", []string{"migrate-down-to", "--yes", "--", "-1"}, "must not be negative"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				useDatabase(t, db.URL)

				_, _, err := runCLI(t, tc.args...)
				if err == nil {
					t.Fatalf("migrate-down-to error = nil, want one mentioning %q", tc.want)
				}
				if !strings.Contains(err.Error(), tc.want) {
					t.Errorf("migrate-down-to error = %q, want it to contain %q", err, tc.want)
				}
			})
		}
	})

	t.Run("reports a database it cannot reach", func(t *testing.T) {
		useDatabase(t, "postgres://nobody@127.0.0.1:1/none")

		if _, _, err := runCLI(t, "migrate"); err == nil {
			t.Fatal("migrate error = nil, want the connection failure")
		}
	})
}

func TestPeriodsList(t *testing.T) {
	db := storetest.NewDB(t)
	useDatabase(t, db.URL)

	stdout, stderr, err := runCLI(t, "periods", "list")
	if err != nil {
		t.Fatalf("periods list error = %v, want nil (stderr %q)", err, stderr)
	}
	if want := "no billing periods\n"; stdout != want {
		t.Errorf("stdout of a database without periods = %q, want %q", stdout, want)
	}

	// The run the finalized period names. It is seeded first because a period
	// may only name a run that exists.
	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO runs (id, period_from, period_to, status)
		 VALUES ($1, '2026-02-01T00:00:00Z', '2026-03-01T00:00:00Z', 'finalized')`,
		uuid.MustParse(runID)); err != nil {
		t.Fatalf("seeding the run that closed the period: %v", err)
	}

	// The rows are seeded in the wrong order on purpose: the query orders by
	// period_from, so the output is chronological either way.
	//
	// The January row is a run id without a timestamp, which the two nullable
	// columns allow a partial write to leave behind. What the listing must not
	// do with it is print a zero time, which reads as a close in the year 1
	// rather than as a value the database never held.
	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO billing_periods (period_from, period_to, status, finalized_run_id, finalized_at)
		 VALUES ('2026-03-01T00:00:00Z', '2026-04-01T00:00:00Z', 'open', NULL, NULL),
		        ('2026-02-01T00:00:00Z', '2026-03-01T00:00:00Z', 'finalized', $1, '2026-03-04T12:00:00Z'),
		        ('2026-01-01T00:00:00Z', '2026-02-01T00:00:00Z', 'grace', $1, NULL)`,
		uuid.MustParse(runID)); err != nil {
		t.Fatalf("seeding the billing periods: %v", err)
	}

	stdout, stderr, err = runCLI(t, "periods", "list")
	if err != nil {
		t.Fatalf("periods list error = %v, want nil (stderr %q)", err, stderr)
	}
	want := "2026-01 grace finalized_run=" + runID + "\n" +
		"2026-02 finalized finalized_run=" + runID + " finalized_at=2026-03-04T12:00:00Z\n" +
		"2026-03 open\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	t.Run("refuses a configuration without a database url", func(t *testing.T) {
		blankEnvironment(t)

		stdout, _, err := runCLI(t, "periods", "list")
		if err == nil {
			t.Fatal("periods list error = nil, want the missing database url reported")
		}
		if want := "TALLY_ENGINE_DB_URL: must be set"; !strings.Contains(err.Error(), want) {
			t.Errorf("periods list error = %q, want it to contain %q", err, want)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing printed for a query that never ran", stdout)
		}
	})

	t.Run("reports a database it cannot reach", func(t *testing.T) {
		useDatabase(t, "postgres://nobody@127.0.0.1:1/none")

		_, _, err := runCLI(t, "periods", "list")
		if err == nil {
			t.Fatal("periods list error = nil, want the connection failure")
		}
		// The schema gate is the first read the subcommand makes, so it is the
		// one the unreachable database fails on. It reports that read rather
		// than passing it, which would leave the listing to fail on it instead.
		if want := "reading the schema version:"; !strings.Contains(err.Error(), want) {
			t.Errorf("periods list error = %q, want it to contain %q", err, want)
		}
	})

	// The url that cannot be reached above parses; this one does not, and the
	// two leave the operator with different things to fix.
	t.Run("reports a database url it cannot parse", func(t *testing.T) {
		useDatabase(t, "not-a-database-url")

		_, _, err := runCLI(t, "periods", "list")
		if err == nil {
			t.Fatal("periods list error = nil, want the malformed url reported")
		}
		if want := "parsing the database url:"; !strings.Contains(err.Error(), want) {
			t.Errorf("periods list error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("refuses a database on an older schema than the build", func(t *testing.T) {
		useDatabase(t, db.NewSiblingDB(t, "schema_gate"))
		if _, stderr, err := runCLI(t, "migrate"); err != nil {
			t.Fatalf("migrate error = %v, want nil (stderr %q)", err, stderr)
		}
		// Rolling the chain back leaves the migrator's bookkeeping behind, which
		// is the state a database whose migrate step lagged is in: it answers,
		// and what it answers is a schema this build cannot work on.
		if _, stderr, err := runCLI(t, "migrate-down-to", "0", "--yes"); err != nil {
			t.Fatalf("migrate-down-to error = %v, want nil (stderr %q)", err, stderr)
		}

		_, _, err := runCLI(t, "periods", "list")
		if err == nil {
			t.Fatal("periods list error = nil, want the schema version reported")
		}
		want := fmt.Sprintf("the database is on schema version 0, this build needs %d",
			enginemigrations.Version)
		if !strings.Contains(err.Error(), want) {
			t.Errorf("periods list error = %q, want it to contain %q", err, want)
		}
	})
}

func TestPricingCLI(t *testing.T) {
	db := storetest.NewDB(t)
	useDatabase(t, db.URL)

	stdout, stderr, err := runCLI(t, "pricing", "list")
	if err != nil {
		t.Fatalf("pricing list error = %v, want nil (stderr %q)", err, stderr)
	}
	if want := "no pricing models\n"; stdout != want {
		t.Errorf("stdout of a database without pricing models = %q, want %q", stdout, want)
	}

	// The committed example, which is the file the operator documentation points
	// at. Importing it here is what keeps it importable.
	const example = "../../pricing/2026-03.yaml"

	stdout, stderr, err = runCLI(t, "pricing", "import", example)
	if err != nil {
		t.Fatalf("pricing import error = %v, want nil (stderr %q)", err, stderr)
	}
	if want := "imported pricing model 2026-03 valid from 2026-03-01T00:00:00Z\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	// The same file again. A version is imported once, so the second run reports
	// what is already stored instead of writing or refusing.
	stdout, stderr, err = runCLI(t, "pricing", "import", example)
	if err != nil {
		t.Fatalf("the second pricing import error = %v, want nil (stderr %q)", err, stderr)
	}
	if want := "pricing model 2026-03 already imported\n"; stdout != want {
		t.Errorf("stdout of the second import = %q, want %q", stdout, want)
	}

	// A version that becomes valid before the one already stored. The listing
	// orders by valid_from, so it comes out first however late it was imported.
	earlier := writeModel(t, "2026-02.yaml", modelYAML("2026-02", "2026-02-01T00:00:00Z", "0.02"))
	stdout, stderr, err = runCLI(t, "pricing", "import", earlier)
	if err != nil {
		t.Fatalf("importing the earlier model error = %v, want nil (stderr %q)", err, stderr)
	}
	if want := "imported pricing model 2026-02 valid from 2026-02-01T00:00:00Z\n"; stdout != want {
		t.Errorf("stdout of the earlier model = %q, want %q", stdout, want)
	}

	stdout, stderr, err = runCLI(t, "pricing", "list")
	if err != nil {
		t.Fatalf("pricing list error = %v, want nil (stderr %q)", err, stderr)
	}
	want := fmt.Sprintf("2026-02 valid_from=2026-02-01T00:00:00Z currency=EUR imported_at=%s\n"+
		"2026-03 valid_from=2026-03-01T00:00:00Z currency=EUR imported_at=%s\n",
		importedAt(t, db, "2026-02"), importedAt(t, db, "2026-03"))
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	t.Run("refuses a model the schema does not accept", func(t *testing.T) {
		// A currency that is not a three-letter code. Nothing else about the file
		// differs, so what the import ends on is the validation.
		invalid := strings.Replace(modelYAML("2026-04", "2026-04-01T00:00:00Z", "0.02"), `"EUR"`, `"Euro"`, 1)
		path := writeModel(t, "bad-currency.yaml", invalid)

		stdout, _, err := runCLI(t, "pricing", "import", path)
		if err == nil {
			t.Fatal("pricing import error = nil, want the invalid model reported")
		}
		if want := "validating the pricing model"; !strings.Contains(err.Error(), want) {
			t.Errorf("pricing import error = %q, want it to contain %q", err, want)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("pricing import error = %q, want it to name the file %q", err, path)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing printed by an import that was refused", stdout)
		}
	})

	t.Run("refuses a stored version that prices something else", func(t *testing.T) {
		// The version the example imported above, under another price. An invoice
		// names the version it was rated from, so a corrected price belongs in a
		// new version rather than over this one.
		path := writeModel(t, "conflict.yaml", modelYAML("2026-03", "2026-03-01T00:00:00Z", "0.03"))

		stdout, _, err := runCLI(t, "pricing", "import", path)
		if err == nil {
			t.Fatal("pricing import error = nil, want the version conflict reported")
		}
		if !errors.Is(err, pricing.ErrVersionConflict) {
			t.Errorf("pricing import error = %v, want one matching ErrVersionConflict", err)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing printed by an import that was refused", stdout)
		}
	})

	t.Run("reports a file it cannot read", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "missing.yaml")

		stdout, _, err := runCLI(t, "pricing", "import", path)
		if err == nil {
			t.Fatal("pricing import error = nil, want the unreadable file reported")
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("pricing import error = %q, want it to name the file %q", err, path)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing printed by an import that read nothing", stdout)
		}
	})

	t.Run("refuses a configuration without a database url", func(t *testing.T) {
		blankEnvironment(t)

		stdout, _, err := runCLI(t, "pricing", "list")
		if err == nil {
			t.Fatal("pricing list error = nil, want the missing database url reported")
		}
		if want := "TALLY_ENGINE_DB_URL: must be set"; !strings.Contains(err.Error(), want) {
			t.Errorf("pricing list error = %q, want it to contain %q", err, want)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing printed for a query that never ran", stdout)
		}
	})
}

// modelYAML is the smallest model the schema accepts: one platform, one
// resource type, one dimension. The price is a string, the way the committed
// example spells its prices.
func modelYAML(version, validFrom, price string) string {
	return fmt.Sprintf(`version: %q
valid_from: %q
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: %q
`, version, validFrom, price)
}

// writeModel puts a pricing model file in a directory of its own and returns
// its path.
func writeModel(t *testing.T, name, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

// importedAt is when the database stamped the given version, in the form the
// listing prints it. The column is filled by the database itself, so the
// expected output can only be built from what was stored.
func importedAt(t *testing.T, db storetest.DB, version string) string {
	t.Helper()

	var stamp time.Time
	if err := db.Store.Pool().QueryRow(t.Context(),
		"SELECT imported_at FROM pricing_models WHERE version = $1", version).Scan(&stamp); err != nil {
		t.Fatalf("reading imported_at of the pricing model %s: %v", version, err)
	}
	return stamp.UTC().Format(time.RFC3339)
}

// TestRunAndFinalizeCLI drives one billing month through the two subcommands
// that bill it: run meters it over both databases, finalize closes it, and
// periods list reads back what that left behind.
func TestRunAndFinalizeCLI(t *testing.T) {
	f := newPipelineFixture(t)

	from, _ := billingMonth(-2)
	month := period.Format(from)
	const cloud = "os-cli-run"
	f.seedProject(t, cloud, "proj-cli-run")
	f.seedInstance(t, cloud, "i-cli-run", "proj-cli-run", from)

	stdout, stderr, err := runCLI(t, "run", "--period", month, "--clouds", cloud)
	if err != nil {
		t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
	}
	// The id is the database's, not the test's, so the expected output is built
	// from the run the CLI opened.
	id := f.completedRun(t, from)
	want := fmt.Sprintf("run %s completed for %s with pricing model %s\n"+
		"metered 1 candidates into 1 usage records, 1 rated records and 1 project statements\n",
		id, month, modelVersion)
	if stdout != want {
		t.Errorf("stdout of run = %q, want %q", stdout, want)
	}

	stdout, stderr, err = runCLI(t, "finalize", "--period", month, "--run", id.String())
	if err != nil {
		t.Fatalf("finalize error = %v, want nil (stderr %q)", err, stderr)
	}
	if want := fmt.Sprintf("run %s finalized, period %s closed\n", id, month); stdout != want {
		t.Errorf("stdout of finalize = %q, want %q", stdout, want)
	}

	stdout, stderr, err = runCLI(t, "periods", "list")
	if err != nil {
		t.Fatalf("periods list error = %v, want nil (stderr %q)", err, stderr)
	}
	want = fmt.Sprintf("%s finalized finalized_run=%s finalized_at=%s\n", month, id, f.finalizedAt(t, from))
	if stdout != want {
		t.Errorf("stdout of periods list = %q, want %q", stdout, want)
	}

	t.Run("warns about a month that has not ended", func(t *testing.T) {
		current, currentTo := billingMonth(0)
		currentMonth := period.Format(current)

		// No --clouds, which meters every cloud. The one resource above lived in
		// another month, so this month has nothing in it but the warning.
		stdout, stderr, err := runCLI(t, "run", "--period", currentMonth)
		if err != nil {
			t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
		}
		for _, want := range []string{
			fmt.Sprintf("run %s completed for %s with pricing model %s\n",
				f.completedRun(t, current), currentMonth, modelVersion),
			"metered 0 candidates into 0 usage records, 0 rated records and 0 project statements\n",
			fmt.Sprintf("warning: %s: period_to %s has not passed yet",
				runs.WarningPeriodNotEnded, currentTo.Format(time.RFC3339)),
		} {
			if !strings.Contains(stdout, want) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, want)
			}
		}
	})

	t.Run("refuses a configuration without a reporting database url", func(t *testing.T) {
		useDatabase(t, f.engine.URL)

		stdout, _, err := runCLI(t, "run", "--period", month)
		if err == nil {
			t.Fatal("run error = nil, want the missing reporting database url reported")
		}
		if want := "TALLY_ENGINE_REPORTING_DB_URL: must be set"; !strings.Contains(err.Error(), want) {
			t.Errorf("run error = %q, want it to contain %q", err, want)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing printed by a run that never opened", stdout)
		}
	})
}

// TestDetectLateAndCorrectCLI drives a finalized month through the two
// subcommands that deal with what reached it afterwards: detect-late names the
// resources whose events arrived after the run read the period, and correct
// books the difference between the finalized numbers and a fresh metering.
func TestDetectLateAndCorrectCLI(t *testing.T) {
	f := newPipelineFixture(t)

	from, _ := billingMonth(-3)
	month := period.Format(from)
	const (
		cloud      = "os-cli-correct"
		project    = "proj-cli-correct"
		resourceID = "i-cli-correct"
		resizeID   = "ev-resize-" + resourceID
	)
	f.seedProject(t, cloud, project)
	f.seedInstance(t, cloud, resourceID, project, from)

	if _, stderr, err := runCLI(t, "run", "--period", month, "--clouds", cloud); err != nil {
		t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
	}
	id := f.completedRun(t, from)

	stdout, stderr, err := runCLI(t, "finalize", "--period", month, "--run", id.String())
	if err != nil {
		t.Fatalf("finalize error = %v, want nil (stderr %q)", err, stderr)
	}
	if want := fmt.Sprintf("run %s finalized, period %s closed\n", id, month); stdout != want {
		t.Errorf("stdout of finalize = %q, want %q", stdout, want)
	}

	// Nothing reached the reporting database since the run read it, which is
	// what a month that is simply done answers.
	stdout, stderr, err = runCLI(t, "detect-late", "--period", month)
	if err != nil {
		t.Fatalf("detect-late error = %v, want nil (stderr %q)", err, stderr)
	}
	want := fmt.Sprintf("run %s read %s at %s\nno events arrived later\n", id, month, f.snapshotAt(t, id))
	if stdout != want {
		t.Errorf("stdout of detect-late = %q, want %q", stdout, want)
	}

	// A resize halfway through the instance's life, which halves its vcpus for
	// the second half of it. The event is dated inside the finalized month and
	// stamped received now, so the finalized run billed the month without it.
	f.seedEvent(t, cloud, resourceID, project, resizeID, "compute.instance.resize.end", from.Add(48*time.Hour),
		`{"state":"active","size":{"vcpus":2,"ram_gb":8,"disk_gb":80,"flavor":"m1.large"}}`)

	stdout, stderr, err = runCLI(t, "detect-late", "--period", month)
	if err != nil {
		t.Fatalf("the second detect-late error = %v, want nil (stderr %q)", err, stderr)
	}
	want = fmt.Sprintf("run %s read %s at %s\n"+
		"%s/openstack/instance/%s: 1 late events, last received %s\n"+
		"book them with tally-engine correct --period %s\n",
		id, month, f.snapshotAt(t, id), cloud, resourceID, f.receivedAt(t, resizeID), month)
	if stdout != want {
		t.Errorf("stdout of detect-late = %q, want %q", stdout, want)
	}

	stdout, stderr, err = runCLI(t, "correct", "--period", month)
	if err != nil {
		t.Fatalf("correct error = %v, want nil (stderr %q)", err, stderr)
	}
	correction := f.completedCorrection(t, from)
	want = fmt.Sprintf("run %s completed as a correction of run %s for %s with pricing model %s\n"+
		"metered 1 candidates into 2 usage records and 2 rated records\n"+
		"1 deltas in 1 credit notes\n", correction, id, month, modelVersion)
	if stdout != want {
		t.Errorf("stdout of correct = %q, want %q", stdout, want)
	}

	// The one delta behind that count: the finalized run billed 48 hours at 4
	// vcpus, the correction 24 hours at 4 and 24 at 2, both at 0.02 per hour.
	var dimension, oldAmount, newAmount, delta string
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT dimension, old_amount::text, new_amount::text, delta::text
		 FROM correction_deltas WHERE run_id = $1`, correction).
		Scan(&dimension, &oldAmount, &newAmount, &delta); err != nil {
		t.Fatalf("reading the delta of the correction %s: %v", correction, err)
	}
	if dimension != "vcpus" || oldAmount != "3.84" || newAmount != "2.88" || delta != "-0.96" {
		t.Errorf("the delta = %s %s -> %s (%s), want vcpus 3.84 -> 2.88 (-0.96)",
			dimension, oldAmount, newAmount, delta)
	}

	stdout, stderr, err = runCLI(t, "finalize", "--period", month, "--run", correction.String())
	if err != nil {
		t.Fatalf("finalizing the correction error = %v, want nil (stderr %q)", err, stderr)
	}
	if want := fmt.Sprintf("correction run %s finalized for %s\n", correction, month); stdout != want {
		t.Errorf("stdout of the correction's finalize = %q, want %q", stdout, want)
	}

	// The same month again, now against the finalized correction. Nothing
	// arrived since, so the fresh metering matches it and there is nothing to
	// book.
	stdout, stderr, err = runCLI(t, "correct", "--period", month)
	if err != nil {
		t.Fatalf("the second correct error = %v, want nil (stderr %q)", err, stderr)
	}
	want = fmt.Sprintf("run %s completed as a correction of run %s for %s with pricing model %s\n"+
		"metered 1 candidates into 2 usage records and 2 rated records\n"+
		"no deltas: the finalized numbers of %s stand\n",
		f.completedCorrection(t, from), correction, month, modelVersion, month)
	if stdout != want {
		t.Errorf("stdout of the second correct = %q, want %q", stdout, want)
	}

	// A correction closes itself alone, so the period still names the regular
	// run that closed it and the instant that closed it.
	stdout, stderr, err = runCLI(t, "periods", "list")
	if err != nil {
		t.Fatalf("periods list error = %v, want nil (stderr %q)", err, stderr)
	}
	want = fmt.Sprintf("%s finalized finalized_run=%s finalized_at=%s\n", month, id, f.finalizedAt(t, from))
	if stdout != want {
		t.Errorf("stdout of periods list = %q, want %q", stdout, want)
	}

	t.Run("detect-late needs no counter sources", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing.yaml")
		t.Setenv("TALLY_ENGINE_COUNTER_SOURCES", missing)

		// Nothing is measured, so the file is never read. The run the late events
		// are held against is now the finalized correction.
		stdout, stderr, err := runCLI(t, "detect-late", "--period", month)
		if err != nil {
			t.Fatalf("detect-late error = %v, want nil (stderr %q)", err, stderr)
		}
		if want := fmt.Sprintf("run %s read %s at ", correction, month); !strings.HasPrefix(stdout, want) {
			t.Errorf("stdout of detect-late = %q, want it to start with %q", stdout, want)
		}

		// correct meters the month again, so it fails on the file a run fails on.
		_, _, correctErr := runCLI(t, "correct", "--period", month)
		if correctErr == nil {
			t.Fatal("correct error = nil, want the unreadable counter sources reported")
		}
		_, _, runErr := runCLI(t, "run", "--period", month)
		if runErr == nil {
			t.Fatal("run error = nil, want the unreadable counter sources reported")
		}
		if correctErr.Error() != runErr.Error() {
			t.Errorf("correct error = %q, want the same as the run error %q", correctErr, runErr)
		}
		for _, want := range []string{"reading the counter sources " + missing, "no such file"} {
			if !strings.Contains(correctErr.Error(), want) {
				t.Errorf("correct error = %q, want it to contain %q", correctErr, want)
			}
		}
	})

	// detect-late is the one subcommand that opens the two databases through
	// openDatabases rather than through openPipeline, so the gate over the
	// reporting url is a call site of its own and is refused here rather than at
	// the first query.
	t.Run("refuses a configuration without a reporting database url", func(t *testing.T) {
		useDatabase(t, f.engine.URL)

		stdout, _, err := runCLI(t, "detect-late", "--period", month)
		if err == nil {
			t.Fatal("detect-late error = nil, want the missing reporting database url reported")
		}
		if want := "TALLY_ENGINE_REPORTING_DB_URL: must be set"; !strings.Contains(err.Error(), want) {
			t.Errorf("detect-late error = %q, want it to contain %q", err, want)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing printed by a detect-late that never opened", stdout)
		}
	})
}

// TestTickCLI drives the scheduler entrypoint over both databases: what it
// prints is the whole of what the CronJob's log holds.
func TestTickCLI(t *testing.T) {
	f := newPipelineFixture(t)

	// The running month is the only period the engine knows, so the walk that
	// starts at the earliest period row reaches no month that has ended.
	current, currentTo := billingMonth(0)
	f.seedPeriod(t, current, currentTo, "open")

	stdout, stderr, err := runCLI(t, "tick")
	if err != nil {
		t.Fatalf("tick error = %v, want nil (stderr %q)", err, stderr)
	}
	if want := "nothing due\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	// A month in grace whose window has long passed, which is what the tick
	// meters. The month between it and the running one has no row yet and is
	// walked as well, so the tick opens it and ends its open phase.
	due, dueTo := billingMonth(-2)
	f.seedPeriod(t, due, dueTo, "grace")
	previous, _ := billingMonth(-1)

	stdout, stderr, err = runCLI(t, "tick")
	if err != nil {
		t.Fatalf("the second tick error = %v, want nil (stderr %q)", err, stderr)
	}
	want := fmt.Sprintf("%s run %s completed\n%s open -> grace\n",
		period.Format(due), f.completedRun(t, due), period.Format(previous))
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	t.Run("refuses a configuration without a reporting database url", func(t *testing.T) {
		useDatabase(t, f.engine.URL)

		stdout, _, err := runCLI(t, "tick")
		if err == nil {
			t.Fatal("tick error = nil, want the missing reporting database url reported")
		}
		if want := "TALLY_ENGINE_REPORTING_DB_URL: must be set"; !strings.Contains(err.Error(), want) {
			t.Errorf("tick error = %q, want it to contain %q", err, want)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want nothing printed by a tick that never walked", stdout)
		}
	})
}

// modelVersion is the version of the pricing model the run and tick cases are
// rated with, which they print beside the run.
const modelVersion = "v1"

// pipelineFixture is the pair of databases a run works over: the engine
// database it is written to, and the reporting database it reads its resources
// and events from. The CLI is pointed at both through the environment, the way
// an operator points it at them.
type pipelineFixture struct {
	engine    storetest.DB
	reporting reportingtest.DB
}

// newPipelineFixture starts both databases and imports the pricing model every
// case is rated with, through the CLI that an operator imports it with.
func newPipelineFixture(t *testing.T) pipelineFixture {
	t.Helper()

	f := pipelineFixture{engine: storetest.NewDB(t), reporting: reportingtest.NewDB(t)}
	usePipeline(t, f.engine.URL, f.reporting.URL)

	// Valid from long before every month the cases meter, so each of them
	// resolves this one model.
	validFrom, _ := billingMonth(-36)
	path := writeModel(t, "pipeline.yaml", modelYAML(modelVersion, validFrom.Format(time.RFC3339), "0.02"))
	if _, stderr, err := runCLI(t, "pricing", "import", path); err != nil {
		t.Fatalf("importing the pricing model: %v (stderr %q)", err, stderr)
	}
	return f
}

// billingMonth is the UTC billing month offset months from the one the test
// runs in: 0 is the running month, -2 two months back. The months are derived
// from the clock rather than written down, because whether a month has ended is
// what one of the cases is about and what the others must not run into.
func billingMonth(offset int) (from, to time.Time) {
	now := time.Now().UTC()
	from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, offset, 0)
	return from, from.AddDate(0, 1, 0)
}

// seedProject registers a project, which is what gets its statement a document
// keyed to the registry rather than an entry in unregistered_projects.
func (f pipelineFixture) seedProject(t *testing.T, cloud, externalID string) {
	t.Helper()

	if _, err := f.reporting.Store.Pool().Exec(t.Context(),
		`INSERT INTO projects (platform, cloud, external_id) VALUES ('openstack', $1, $2)`,
		cloud, externalID); err != nil {
		t.Fatalf("seeding the project %s: %v", externalID, err)
	}
}

// seedInstance writes the projection row and the create and delete events of
// one instance, alive from the second day of its month to the fourth. Being
// created and deleted inside its own month is what confines it to that month.
func (f pipelineFixture) seedInstance(t *testing.T, cloud, resourceID, projectID string, from time.Time) {
	t.Helper()

	created, deleted := from.Add(24*time.Hour), from.Add(72*time.Hour)
	const size = `{"vcpus":4,"ram_gb":8,"disk_gb":80,"flavor":"m1.large"}`

	if _, err := f.reporting.Store.Pool().Exec(t.Context(),
		`INSERT INTO current_resources (cloud, platform, resource_type, resource_id, project_id,
		                                state, size, created_at, deleted_at, last_event_type, last_event_at)
		 VALUES ($1, 'openstack', 'instance', $2, $3, 'deleted', $4::jsonb, $5, $6,
		         'compute.instance.delete.end', $6)`,
		cloud, resourceID, projectID, size, created, deleted); err != nil {
		t.Fatalf("seeding the projection row of %s: %v", resourceID, err)
	}
	for _, ev := range []struct {
		id, eventType string
		at            time.Time
		payload       any
	}{
		{"ev-create-" + resourceID, "compute.instance.create.end", created, `{"state":"active","size":` + size + `}`},
		{"ev-delete-" + resourceID, "compute.instance.delete.end", deleted, nil},
	} {
		if _, err := f.reporting.Store.Pool().Exec(t.Context(),
			`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
			                     resource_type, resource_id, project_id, payload)
			 VALUES ($1, $2, $3, 'openstack', $4, 'instance', $5, $6, $7::jsonb)`,
			ev.id, ev.at, ev.eventType, cloud, resourceID, projectID, ev.payload); err != nil {
			t.Fatalf("seeding the event %s: %v", ev.id, err)
		}
	}
}

// seedEvent writes one event of a resource that already has a projection row.
// received_at is left to the database, which stamps it now, so an event seeded
// after a run is one that reached the reporting database after that run read
// it.
func (f pipelineFixture) seedEvent(
	t *testing.T,
	cloud, resourceID, projectID, eventID, eventType string,
	at time.Time,
	payload string,
) {
	t.Helper()

	if _, err := f.reporting.Store.Pool().Exec(t.Context(),
		`INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
		                     resource_type, resource_id, project_id, payload)
		 VALUES ($1, $2, $3, 'openstack', $4, 'instance', $5, $6, $7::jsonb)`,
		eventID, at, eventType, cloud, resourceID, projectID, payload); err != nil {
		t.Fatalf("seeding the event %s: %v", eventID, err)
	}
}

// seedPeriod writes a billing period in the status a case starts from, which is
// what puts its month into the walk of a tick.
func (f pipelineFixture) seedPeriod(t *testing.T, from, to time.Time, status string) {
	t.Helper()

	if _, err := f.engine.Store.Pool().Exec(t.Context(),
		`INSERT INTO billing_periods (period_from, period_to, status) VALUES ($1, $2, $3)`,
		from, to, status); err != nil {
		t.Fatalf("seeding the %s period %s: %v", status, period.Format(from), err)
	}
}

// completedRun is the run the CLI completed for a month. The id is read back
// rather than parsed out of the output the assertions are about.
func (f pipelineFixture) completedRun(t *testing.T, from time.Time) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT id FROM runs WHERE period_from = $1 AND status = 'completed'`, from).Scan(&id); err != nil {
		t.Fatalf("reading the completed run of %s: %v", period.Format(from), err)
	}
	return id
}

// completedCorrection is the correction the CLI completed for a month. A
// correction that has been finalized no longer matches, so this names the one
// the last correct left behind.
func (f pipelineFixture) completedCorrection(t *testing.T, from time.Time) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT id FROM runs WHERE period_from = $1 AND kind = 'correction' AND status = 'completed'`,
		from).Scan(&id); err != nil {
		t.Fatalf("reading the completed correction of %s: %v", period.Format(from), err)
	}
	return id
}

// snapshotAt is the instant a run read the reporting database at, in the form
// detect-late prints it. The run recorded it in its stats, so the expected
// output can only be built from what was stored.
func (f pipelineFixture) snapshotAt(t *testing.T, id uuid.UUID) string {
	t.Helper()

	var stamp string
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT stats->>'snapshot_at' FROM runs WHERE id = $1`, id).Scan(&stamp); err != nil {
		t.Fatalf("reading snapshot_at of the run %s: %v", id, err)
	}
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		t.Fatalf("parsing snapshot_at %q of the run %s: %v", stamp, id, err)
	}
	return at.UTC().Format(time.RFC3339)
}

// receivedAt is when the reporting database stamped an event, in the form
// detect-late prints it. The column is filled by the database itself, so the
// expected output can only be built from what was stored.
func (f pipelineFixture) receivedAt(t *testing.T, eventID string) string {
	t.Helper()

	var stamp time.Time
	if err := f.reporting.Store.Pool().QueryRow(t.Context(),
		`SELECT received_at FROM events WHERE event_id = $1`, eventID).Scan(&stamp); err != nil {
		t.Fatalf("reading received_at of the event %s: %v", eventID, err)
	}
	return stamp.UTC().Format(time.RFC3339)
}

// finalizedAt is when the database stamped the closed period, in the form the
// listing prints it. The column is filled by the database itself, so the
// expected output can only be built from what was stored.
func (f pipelineFixture) finalizedAt(t *testing.T, from time.Time) string {
	t.Helper()

	var stamp time.Time
	if err := f.engine.Store.Pool().QueryRow(t.Context(),
		`SELECT finalized_at FROM billing_periods WHERE period_from = $1`, from).Scan(&stamp); err != nil {
		t.Fatalf("reading finalized_at of the period %s: %v", period.Format(from), err)
	}
	return stamp.UTC().Format(time.RFC3339)
}

// TestTickLinesReportsWhatTheWalkLeftBehind pins the two lines a tick prints
// beside the steps a month took. Both stand for a month that is not fine while
// the tick's exit status is zero, so nothing else would say they happened: the
// months the walk's cap left out are never reached again by any tick, and a run
// whose period lock stayed behind billed its month all the same.
func TestTickLinesReportsWhatTheWalkLeftBehind(t *testing.T) {
	billed := uuid.MustParse(runID)

	lines := tickLines(scheduler.Report{
		{
			Month:         "2023-04",
			SkippedBefore: 12,
			RunID:         billed,
			Warning:       errors.New("metering 2023-04: the period lock could not be released"),
		},
		{Month: "2023-05"},
	})

	want := []string{
		"12 months before 2023-04 were skipped, and are billed with tally-engine run --period",
		"2023-04 run " + billed.String() + " completed",
		"2023-04 warning: metering 2023-04: the period lock could not be released",
	}
	if len(lines) != len(want) {
		t.Fatalf("tickLines() = %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("tickLines()[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestDetectLateLinesCountsWhatItDidNotName pins the line that stands for the
// resources the report left out. A period finalized before its ingest had
// settled carries a late resource per resource of the fleet, and the report
// names at most source.LateResourceLimit of them; without this line an operator
// would read the ones it named as the whole of what a correction re-meters.
func TestDetectLateLinesCountsWhatItDidNotName(t *testing.T) {
	id := uuid.MustParse(runID)
	readAt := time.Date(2026, 4, 2, 3, 4, 5, 0, time.UTC)

	lines := detectLateLines("2026-03", runs.LateReport{
		RunID:      id,
		Kind:       runs.KindRegular,
		SnapshotAt: readAt,
		Resources: []source.LateResource{{
			Resource: source.Resource{
				Cloud: "os-prod", Platform: "openstack", ResourceType: "instance", ResourceID: "abc-123",
			},
			Events:         2,
			LastReceivedAt: readAt.Add(time.Hour),
		}},
		Truncated: 41,
	})

	want := []string{
		"run " + id.String() + " read 2026-03 at 2026-04-02T03:04:05Z",
		"os-prod/openstack/instance/abc-123: 2 late events, last received 2026-04-02T04:04:05Z",
		"and 41 more resources with late events",
		"book them with tally-engine correct --period 2026-03",
	}
	if len(lines) != len(want) {
		t.Fatalf("detectLateLines() = %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("detectLateLines()[%d] = %q, want %q", i, lines[i], want[i])
		}
	}
}

// TestWriteReportsAFailedWrite pins what write promises: output the operator's
// terminal never received must not leave the process with a zero exit status.
// Every command above writes into a bytes.Buffer, whose writes never fail, so
// nothing else in this file reaches the branch.
func TestWriteReportsAFailedWrite(t *testing.T) {
	err := write(failingWriter{}, "a line")
	if err == nil {
		t.Fatal("write() error = nil, want the failed write reported")
	}
	if want := "writing the output:"; !strings.Contains(err.Error(), want) {
		t.Errorf("write() error = %q, want it to contain %q", err, want)
	}
}

// failingWriter fails every write, the way a pipe does once the reader on the
// other end is gone.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

// TestStubsValidateThenRefuse pins what the subcommands of the later Phase 3
// packages already answer: the flags they take, what they refuse, and the one
// error they end with. That contract is what those packages are written
// against, so it is checked before any of them exists.
func TestStubsValidateThenRefuse(t *testing.T) {
	blankEnvironment(t)

	t.Run("flags that check out end in the not-implemented report", func(t *testing.T) {
		for _, args := range [][]string{
			{"export", "--run", runID, "--format", "json", "--out", "./out"},
			{"export", "--run", runID, "--format", "csv", "--out", "./out"},
		} {
			t.Run(strings.Join(args, " "), func(t *testing.T) {
				stdout, _, err := runCLI(t, args...)
				if err == nil {
					t.Fatalf("error = nil, want %q", notImplemented)
				}
				if err.Error() != notImplemented {
					t.Errorf("error = %q, want %q", err, notImplemented)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want nothing printed by a command that did nothing", stdout)
				}
			})
		}
	})

	t.Run("a missing required flag is reported", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			args []string
			flag string
		}{
			{"run without a period", []string{"run"}, "period"},
			{"finalize without a period", []string{"finalize", "--run", runID}, "period"},
			{"detect-late without a period", []string{"detect-late"}, "period"},
			{"correct without a period", []string{"correct"}, "period"},
			{"finalize without a run", []string{"finalize", "--period", "2026-03"}, "run"},
			{"export without a run", []string{"export", "--format", "json", "--out", "./out"}, "run"},
			{"export without a format", []string{"export", "--run", runID, "--out", "./out"}, "format"},
			{"export without an out", []string{"export", "--run", runID, "--format", "json"}, "out"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, _, err := runCLI(t, tc.args...)
				if err == nil {
					t.Fatalf("error = nil, want the missing --%s reported", tc.flag)
				}
				for _, want := range []string{"required flag", tc.flag} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
			})
		}
	})

	t.Run("export refuses a format it does not write", func(t *testing.T) {
		_, _, err := runCLI(t, "export", "--run", runID, "--format", "xml", "--out", "./out")
		if err == nil {
			t.Fatal("export error = nil, want the unknown format reported")
		}
		for _, want := range []string{"json", "csv"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("export error = %q, want it to name %q", err, want)
			}
		}
	})

	t.Run("pricing import takes exactly one file", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"no file at all", []string{"pricing", "import"}},
			{"two files", []string{"pricing", "import", "a.yaml", "b.yaml"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, _, err := runCLI(t, tc.args...)
				if err == nil {
					t.Fatal("pricing import error = nil, want the argument count reported")
				}
				if want := "accepts 1 arg"; !strings.Contains(err.Error(), want) {
					t.Errorf("pricing import error = %q, want it to contain %q", err, want)
				}
			})
		}
	})
}

// TestFlagsAreCheckedBeforeTheConfiguration pins the order the subcommands that
// bill a period work in: what they were given is checked before anything is
// read. An operator who mistyped a flag gets that flag back, on a machine that
// has no configuration and reaches no database.
func TestFlagsAreCheckedBeforeTheConfiguration(t *testing.T) {
	blankEnvironment(t)

	t.Run("run refuses a period that is not a month", func(t *testing.T) {
		_, _, err := runCLI(t, "run", "--period", "2026-3")
		if err == nil {
			t.Fatal("run error = nil, want the malformed period reported")
		}
		if want := `--period: "2026-3" is not a YYYY-MM month`; err.Error() != want {
			t.Errorf("run error = %q, want %q", err, want)
		}
	})

	t.Run("finalize refuses a run that is not a uuid", func(t *testing.T) {
		_, _, err := runCLI(t, "finalize", "--period", "2026-03", "--run", "not-a-uuid")
		if err == nil {
			t.Fatal("finalize error = nil, want the malformed run id reported")
		}
		if want := `--run: "not-a-uuid" is not a uuid`; !strings.Contains(err.Error(), want) {
			t.Errorf("finalize error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("detect-late refuses a period that is not a month", func(t *testing.T) {
		_, _, err := runCLI(t, "detect-late", "--period", "2026-3")
		if err == nil {
			t.Fatal("detect-late error = nil, want the malformed period reported")
		}
		if want := `--period: "2026-3" is not a YYYY-MM month`; err.Error() != want {
			t.Errorf("detect-late error = %q, want %q", err, want)
		}
	})

	t.Run("correct refuses a period that is not a month", func(t *testing.T) {
		_, _, err := runCLI(t, "correct", "--period", "2026-3")
		if err == nil {
			t.Fatal("correct error = nil, want the malformed period reported")
		}
		if want := `--period: "2026-3" is not a YYYY-MM month`; err.Error() != want {
			t.Errorf("correct error = %q, want %q", err, want)
		}
	})
}

// TestEnvExampleListsEveryVariable keeps the example file complete: a variable
// the CLI reads but nobody documents is one an operator finds out about from a
// failure.
func TestEnvExampleListsEveryVariable(t *testing.T) {
	example, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatalf("reading .env.example: %v", err)
	}

	for _, name := range config.EnvNames {
		if !strings.Contains(string(example), name) {
			t.Errorf(".env.example does not mention %s", name)
		}
	}
}

// TestCounterSourcesExampleLoads keeps the example sources file loadable: a
// format change that is not mirrored there would leave an operator with an
// example the engine refuses.
func TestCounterSourcesExampleLoads(t *testing.T) {
	cfg, err := counters.Load("counter-sources.example.yaml")
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(cfg.Sources) != 2 {
		t.Fatalf("len(cfg.Sources) = %d, want 2", len(cfg.Sources))
	}

	egress := cfg.Sources[0]
	if egress.Platform != "openstack" {
		t.Errorf("sources[0].Platform = %q, want %q", egress.Platform, "openstack")
	}
	if egress.ResourceType != "instance" {
		t.Errorf("sources[0].ResourceType = %q, want %q", egress.ResourceType, "instance")
	}
	if egress.Metric != "egress_gb" {
		t.Errorf("sources[0].Metric = %q, want %q", egress.Metric, "egress_gb")
	}
	if egress.Kind != counters.KindMetricsQL {
		t.Errorf("sources[0].Kind = %q, want %q", egress.Kind, counters.KindMetricsQL)
	}
	if !egress.Required {
		t.Error("sources[0].Required = false, want true")
	}
	for _, placeholder := range []string{"{cloud}", "{resource_id}", "{window}"} {
		if !strings.Contains(egress.Query, placeholder) {
			t.Errorf("sources[0].Query = %q, want it to contain %s", egress.Query, placeholder)
		}
	}

	pulls := cfg.Sources[1]
	if pulls.Platform != "harbor" {
		t.Errorf("sources[1].Platform = %q, want %q", pulls.Platform, "harbor")
	}
	if pulls.ResourceType != "repository" {
		t.Errorf("sources[1].ResourceType = %q, want %q", pulls.ResourceType, "repository")
	}
	if pulls.Metric != "pulls" {
		t.Errorf("sources[1].Metric = %q, want %q", pulls.Metric, "pulls")
	}
	if pulls.Kind != counters.KindEvents {
		t.Errorf("sources[1].Kind = %q, want %q", pulls.Kind, counters.KindEvents)
	}
	if pulls.EventType != "repository.pull" {
		t.Errorf("sources[1].EventType = %q, want %q", pulls.EventType, "repository.pull")
	}
	if !pulls.Required {
		t.Error("sources[1].Required = false, want true")
	}

	if !cfg.HasMetricsQL() {
		t.Error("cfg.HasMetricsQL() = false, want true")
	}
}

// blankEnvironment blanks every variable the CLI reads, so a value in the
// developer's shell never reaches the code under test. A variable set to the
// empty string falls back to its default exactly as an unset one does.
func blankEnvironment(t *testing.T) {
	t.Helper()

	for _, name := range config.EnvNames {
		t.Setenv(name, "")
	}
}

// useDatabase points the CLI at dbURL and blanks every other variable it reads.
func useDatabase(t *testing.T, dbURL string) {
	t.Helper()

	blankEnvironment(t)
	t.Setenv("TALLY_ENGINE_DB_URL", dbURL)
}

// usePipeline points the CLI at both databases a run works over. Everything
// else stays blank, which leaves the counter sources at the empty path: the
// zero configuration, measuring no counter metric.
func usePipeline(t *testing.T, dbURL, reportingURL string) {
	t.Helper()

	useDatabase(t, dbURL)
	t.Setenv("TALLY_ENGINE_REPORTING_DB_URL", reportingURL)
}

// runCLI executes one command in process and returns its stdout, its stderr,
// and the error it ended with. It is the same command tree main runs, built
// over buffers instead of the process's streams.
func runCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()

	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err := root.ExecuteContext(t.Context())
	return stdout.String(), stderr.String(), err
}

// chainVersions is every version of the embedded migration chain, oldest
// first. The chain is numbered sequentially from 1, so its highest version
// names the whole of it; a gap in the numbering fails the tests above.
func chainVersions() []int64 {
	versions := make([]int64, 0, enginemigrations.Version)
	for version := int64(1); version <= enginemigrations.Version; version++ {
		versions = append(versions, version)
	}
	return versions
}

// migrationStatusOutput is what migrate-status prints for a chain whose
// migrations are all in the given state. The command reports every migration,
// so the whole chain has to be spelled out to compare stdout exactly.
func migrationStatusOutput(state string) string {
	var out strings.Builder
	for _, version := range chainVersions() {
		fmt.Fprintf(&out, "migration %d %s\n", version, state)
	}
	return out.String()
}

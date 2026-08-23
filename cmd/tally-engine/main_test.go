package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/engine/config"
	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/store/storetest"
	enginemigrations "github.com/b42labs/tally/migrations/engine"
)

// runID stands in for a run wherever a subcommand takes one. The subcommands
// that would read one are not implemented, so nothing has to exist behind it
// there; the periods list test seeds the run itself, because
// billing_periods.finalized_run_id points at runs.
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
			{"run", "--period", "2026-03"},
			{"run", "--period", "2026-03", "--clouds", "os-prod-eu1,os-prod-eu2"},
			{"finalize", "--period", "2026-03", "--run", runID},
			{"detect-late", "--period", "2026-03"},
			{"correct", "--period", "2026-03"},
			// The file is never opened here, so it does not have to exist.
			{"pricing", "import", "pricing/2026-03.yaml"},
			{"pricing", "list"},
			{"export", "--run", runID, "--format", "json", "--out", "./out"},
			{"export", "--run", runID, "--format", "csv", "--out", "./out"},
			{"tick"},
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

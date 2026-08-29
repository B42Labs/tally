package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/providers/openstack"
	"github.com/b42labs/tally/internal/providers/openstack/simulator"
)

// endedMonth is a billing month that is over and stays over, which is what the
// cases that generate one need: a run refuses a month that has not ended.
const endedMonth = "2026-07"

// simulatedCloud is the cloud the generated months are booked to. It is the
// salt of every identifier in them, so the cases that compare two runs have to
// use the same one.
const simulatedCloud = "os-sim"

// closedBroker is an AMQP URL nothing listens on. It is a usable URL on
// loopback, so the run gets as far as dialing it and fails there rather than at
// the parse or at the check of which broker it is.
const closedBroker = "amqp://guest:guest@127.0.0.1:1/"

// remoteBroker is an AMQP URL of a broker somewhere else, which is the shape of
// a production URL copied into the simulator's environment.
const remoteBroker = "amqp://guest:hunter2@rabbit.control-plane.example:5672/"

// nonBillableNotifications is how many notifications of a generated month the
// collector maps to no event: the ones that are published to describe the
// month's shape without being billable. They are the unsized image.create of
// every image, two for each of the three classic projects and one for each of
// the two Gardener tenants. notifications.jsonl holds every notification and
// events.jsonl only the billable ones, so the two files differ by exactly this
// many lines.
const nonBillableNotifications = 8

func TestHelpNeedsNoEnvironment(t *testing.T) {
	blankEnvironment(t)

	// The root and both leaves. Building the tree reads no configuration, so the
	// help of all of them works on a machine that has none.
	for _, tc := range []struct {
		name string
		path []string
	}{
		{"tally-openstack-simulator", nil},
		{"run", []string{"run"}},
		{"replay", []string{"replay"}},
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

func TestRunRequiresAPeriod(t *testing.T) {
	useCloud(t)

	_, stderr, err := runCLI(t, "run", "--out", t.TempDir())
	if err == nil {
		t.Fatalf("run error = nil, want the missing period reported (stderr %q)", stderr)
	}
	if want := `"period"`; !strings.Contains(err.Error(), want) {
		t.Errorf("run error = %q, want it to contain %q", err, want)
	}
}

func TestRunRefusesAPeriodThatHasNotEnded(t *testing.T) {
	useCloud(t)

	// The month the test runs in, which is the one case where the answer depends
	// on the clock rather than on the flags.
	from, to := billingMonth(0)
	month := from.Format("2006-01")

	_, stderr, err := runCLI(t, "run", "--period", month, "--out", t.TempDir())
	if err == nil {
		t.Fatalf("run error = nil, want the running month refused (stderr %q)", stderr)
	}
	want := fmt.Sprintf("--period: %s has not ended yet; it ends %s, "+
		"and the engine warns about a period that has not ended", month, to.Format(time.RFC3339))
	if err.Error() != want {
		t.Errorf("run error = %q, want %q", err, want)
	}
}

func TestRunRefusesToRunNowhere(t *testing.T) {
	useCloud(t)

	_, stderr, err := runCLI(t, "run", "--period", endedMonth)
	if err == nil {
		t.Fatalf("run error = nil, want the missing destination reported (stderr %q)", stderr)
	}
	want := "set TALLY_SIM_AMQP_URL or pass --out: the run has nowhere to publish"
	if err.Error() != want {
		t.Errorf("run error = %q, want %q", err, want)
	}
}

func TestRunWritesTheMonthInFileMode(t *testing.T) {
	useCloud(t)

	dir := t.TempDir()
	if _, stderr, err := runCLI(t,
		"run", "--period", endedMonth, "--seed", "1", "--out", dir); err != nil {
		t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
	}

	notifications, notificationLines := readLines(t, filepath.Join(dir, "notifications.jsonl"))
	for number, raw := range notificationLines {
		var line simulator.Line
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("notifications.jsonl line %d: %v", number+1, err)
		}
		// The bodies are read by the collector's own decoder, so a body it would
		// refuse is a month no collector could consume.
		if _, err := openstack.ParseEnvelope(line.Body); err != nil {
			t.Fatalf("notifications.jsonl line %d: ParseEnvelope() error = %v, want nil", number+1, err)
		}
	}

	events, eventLines := readLines(t, filepath.Join(dir, "events.jsonl"))
	for number, raw := range eventLines {
		var e event.Event
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatalf("events.jsonl line %d: %v", number+1, err)
		}
		if err := e.Validate(); err != nil {
			t.Fatalf("events.jsonl line %d: Validate() error = %v, want nil", number+1, err)
		}
		if e.Cloud != simulatedCloud {
			t.Errorf("events.jsonl line %d: Cloud = %q, want %q", number+1, e.Cloud, simulatedCloud)
		}
		if e.Source != event.SourceCollector {
			t.Errorf("events.jsonl line %d: Source = %q, want %q",
				number+1, e.Source, event.SourceCollector)
		}
	}

	if want := len(eventLines) + nonBillableNotifications; len(notificationLines) != want {
		t.Errorf("notifications.jsonl has %d lines, want %d: %d events plus the %d non-billable ones",
			len(notificationLines), want, len(eventLines), nonBillableNotifications)
	}

	t.Run("is byte-identical on a second run", func(t *testing.T) {
		second := t.TempDir()
		if _, stderr, err := runCLI(t,
			"run", "--period", endedMonth, "--seed", "1", "--out", second); err != nil {
			t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
		}

		for name, first := range map[string][]byte{
			"notifications.jsonl": notifications,
			"events.jsonl":        events,
		} {
			again, _ := readLines(t, filepath.Join(second, name))
			if !bytes.Equal(first, again) {
				t.Errorf("%s differs between two runs of the same seed, period, and cloud", name)
			}
		}
	})
}

func TestRunRefusesBadInput(t *testing.T) {
	notADirectory := writeFile(t, t.TempDir(), "notifications.jsonl", "")
	urlFile := writeFile(t, t.TempDir(), "amqp-url", closedBroker)
	emptyURLFile := writeFile(t, t.TempDir(), "empty-amqp-url", "")

	for _, tc := range []struct {
		name string
		// env is applied over the blanked environment, after the cloud every case
		// but one needs. A case that names a variable with an empty value blanks
		// it again.
		env  map[string]string
		args []string
		// want is the whole error, contains a fragment of it. Each case sets one
		// of the two: the errors that quote a temporary path can only be matched
		// in part.
		want     string
		contains string
		// absent is a fragment the error must not carry, which is how the case
		// about a secret checks that the secret stayed out of it.
		absent string
	}{
		{
			name: "a period that is not a month",
			args: []string{"--period", "2026-7", "--out", t.TempDir()},
			want: `--period: "2026-7" is not a YYYY-MM month`,
		},
		{
			name: "a negative factor",
			// The value is attached with =, because pflag reads a separate -1 as a
			// flag rather than as the value of --factor.
			args: []string{"--period", endedMonth, "--factor=-1", "--out", t.TempDir()},
			want: "--factor: -1 must be zero or positive",
		},
		{
			name: "a negative wait for the collector",
			args: []string{"--period", endedMonth, "--wait-for-collector=-1s", "--out", t.TempDir()},
			want: "--wait-for-collector: -1s must be zero or positive",
		},
		{
			name: "a log level in the wrong case",
			env:  map[string]string{"TALLY_LOG_LEVEL": "info"},
			args: []string{"--period", endedMonth, "--out", t.TempDir()},
			want: `loading the configuration: TALLY_LOG_LEVEL: "info" must be DEBUG, INFO, WARN, or ERROR`,
		},
		{
			name: "a broker URL given twice",
			env: map[string]string{
				"TALLY_SIM_AMQP_URL":      closedBroker,
				"TALLY_SIM_AMQP_URL_FILE": urlFile,
			},
			args: []string{"--period", endedMonth, "--out", t.TempDir()},
			want: "loading the configuration: set TALLY_SIM_AMQP_URL or TALLY_SIM_AMQP_URL_FILE, not both",
		},
		{
			name:     "a broker URL file nobody filled",
			env:      map[string]string{"TALLY_SIM_AMQP_URL_FILE": emptyURLFile},
			args:     []string{"--period", endedMonth, "--out", t.TempDir()},
			contains: fmt.Sprintf("TALLY_SIM_AMQP_URL_FILE: file %s is empty", emptyURLFile),
		},
		{
			name: "no cloud",
			env:  map[string]string{"TALLY_SIM_CLOUD": ""},
			args: []string{"--period", endedMonth, "--out", t.TempDir()},
			want: "checking the configuration: TALLY_SIM_CLOUD: must be set",
		},
		{
			name:     "a broker URL that is not a URL",
			env:      map[string]string{"TALLY_SIM_AMQP_URL": "not a url"},
			args:     []string{"--period", endedMonth},
			contains: "TALLY_SIM_AMQP_URL is not a usable AMQP URL",
			// The URL carries the broker password, and the parser formats the whole
			// input into its own error, so that error is replaced rather than
			// wrapped.
			absent: "not a url",
		},
		{
			name:     "a broker that is not on this machine",
			env:      map[string]string{"TALLY_SIM_AMQP_URL": remoteBroker},
			args:     []string{"--period", endedMonth},
			contains: "TALLY_SIM_AMQP_URL names the broker rabbit.control-plane.example, which is not on this machine",
			// The refusal names the host and nothing else of the URL: the rest of it
			// is the broker password.
			absent: "hunter2",
		},
		{
			name: "a factor that is not a number",
			// pflag parses NaN through strconv, and NaN passes every comparison a
			// bound is written as, so it has to be refused by name.
			args: []string{"--period", endedMonth, "--factor=NaN", "--out", t.TempDir()},
			want: "--factor: NaN must be zero or positive",
		},
		{
			name:     "a broker nothing listens on",
			env:      map[string]string{"TALLY_SIM_AMQP_URL": closedBroker},
			args:     []string{"--period", endedMonth},
			contains: "dialing the broker:",
		},
		{
			name:     "an output directory under a regular file",
			args:     []string{"--period", endedMonth, "--out", filepath.Join(notADirectory, "sub")},
			contains: fmt.Sprintf("creating %s:", filepath.Join(notADirectory, "sub")),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useCloud(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}

			_, stderr, err := runCLI(t, append([]string{"run"}, tc.args...)...)
			if err == nil {
				t.Fatalf("run error = nil, want a failure (stderr %q)", stderr)
			}
			if tc.want != "" && err.Error() != tc.want {
				t.Errorf("run error = %q, want %q", err, tc.want)
			}
			if tc.contains != "" && !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("run error = %q, want it to contain %q", err, tc.contains)
			}
			if tc.absent != "" && strings.Contains(err.Error(), tc.absent) {
				t.Errorf("run error = %q, want it to keep %q out", err, tc.absent)
			}
		})
	}
}

func TestReplayRequiresABroker(t *testing.T) {
	blankEnvironment(t)

	_, stderr, err := runCLI(t, "replay", "--in", filepath.Join(t.TempDir(), "notifications.jsonl"))
	if err == nil {
		t.Fatalf("replay error = nil, want the missing broker reported (stderr %q)", stderr)
	}
	want := "checking the configuration: TALLY_SIM_AMQP_URL: must be set"
	if err.Error() != want {
		t.Errorf("replay error = %q, want %q", err, want)
	}
}

func TestReplayRefusesBadInput(t *testing.T) {
	blankEnvironment(t)
	t.Setenv("TALLY_SIM_AMQP_URL", closedBroker)

	t.Run("a negative factor", func(t *testing.T) {
		// The file is never read: the flags are checked before the broker is
		// dialled, and the dial is what would come first otherwise.
		_, stderr, err := runCLI(t, "replay", "--in", "x", "--factor=-1")
		if err == nil {
			t.Fatalf("replay error = nil, want the negative factor refused (stderr %q)", stderr)
		}
		if want := "--factor: -1 must be zero or positive"; err.Error() != want {
			t.Errorf("replay error = %q, want %q", err, want)
		}
	})

	t.Run("a broker that is not on this machine", func(t *testing.T) {
		t.Setenv("TALLY_SIM_AMQP_URL", remoteBroker)

		_, stderr, err := runCLI(t, "replay", "--in", "x")
		if err == nil {
			t.Fatalf("replay error = nil, want the remote broker refused (stderr %q)", stderr)
		}
		want := "TALLY_SIM_AMQP_URL names the broker rabbit.control-plane.example, which is not on this machine"
		if !strings.Contains(err.Error(), want) {
			t.Errorf("replay error = %q, want it to contain %q", err, want)
		}
	})

	t.Run("no file to replay", func(t *testing.T) {
		_, stderr, err := runCLI(t, "replay")
		if err == nil {
			t.Fatalf("replay error = nil, want the missing file reported (stderr %q)", stderr)
		}
		if want := `"in"`; !strings.Contains(err.Error(), want) {
			t.Errorf("replay error = %q, want it to contain %q", err, want)
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

	for _, name := range simulator.EnvNames {
		if !strings.Contains(string(example), name) {
			t.Errorf(".env.example does not mention %s", name)
		}
	}
}

// blankEnvironment blanks every variable the CLI reads, so a value in the
// developer's shell never reaches the code under test. A variable set to the
// empty string falls back to its default exactly as an unset one does.
func blankEnvironment(t *testing.T) {
	t.Helper()

	for _, name := range simulator.EnvNames {
		t.Setenv(name, "")
	}
}

// useCloud names the cloud a run generates for and blanks every other variable
// the CLI reads. There is no broker in it, so a run under it is file mode.
func useCloud(t *testing.T) {
	t.Helper()

	blankEnvironment(t)
	t.Setenv("TALLY_SIM_CLOUD", simulatedCloud)
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

// billingMonth is the UTC billing month offset months from the one the test
// runs in: 0 is the running month, -2 two months back. The months are derived
// from the clock rather than written down, because whether a month has ended is
// what one of the cases is about.
func billingMonth(offset int) (from, to time.Time) {
	now := time.Now().UTC()
	from = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, offset, 0)
	return from, from.AddDate(0, 1, 0)
}

// writeFile puts content in a file named name under dir and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// readLines reads a JSONL file and returns its bytes together with its lines.
// The bytes are what two runs of the same seed are compared over, and the lines
// are what each document in them is checked through.
func readLines(t *testing.T, path string) ([]byte, [][]byte) {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if len(content) == 0 {
		t.Fatalf("%s is empty", path)
	}
	return content, bytes.Split(bytes.TrimSuffix(content, []byte("\n")), []byte("\n"))
}

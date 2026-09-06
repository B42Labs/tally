package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/period"
	"github.com/b42labs/tally/internal/providers/openstack"
	"github.com/b42labs/tally/internal/providers/openstack/simulator"
	"github.com/b42labs/tally/internal/refdoc"
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

// closedRegistry is a Reporting API URL nothing listens on. It is a usable URL
// on loopback, so a registration gets as far as posting the first project and
// fails there rather than at the check of the environment. https, because the
// admin api token travels on it and plaintext is refused unless it is asked
// for.
const closedRegistry = "https://127.0.0.1:1"

// testAPIToken is the credential a registration authenticates with. The case
// about a registry nothing listens on searches the failure for it, because a
// message carrying the token would put it into whatever an operator pastes it
// into.
const testAPIToken = "tly_a_test"

// testOTLPPassword is the Basic password of the push. The cases about the OTLP
// configuration search their failure for it, for the reason the api token's
// cases give: a message an operator pastes into a ticket carries no credential.
const testOTLPPassword = "s3cret-of-the-test"

// gardenCloud is the cloud the two Gardener projects are registered under. It
// differs from simulatedCloud because a cloud is one installation of one
// platform, and the registry keys a row by its cloud.
const gardenCloud = "garden-sim"

// What seed 1 over endedMonth registers: a row per tenant of the month plus one
// per Gardener project, and one infrastructure_tenant relation per Gardener
// project. RegistrationsOf decides them, and these are the numbers that have to
// reach the registry over the wire.
const (
	registeredProjects  = 8
	registeredRelations = 2
)

// nonBillableNotifications is how many notifications of a month the collector
// maps to no event: the unsized image.create, one per image, and every
// notification of the noise catalogue (docs/openstack-simulator.md, "The
// noise"). notifications.jsonl holds every notification and events.jsonl only
// the billable ones, so the two files differ by exactly this many lines.
//
// The count comes from the month the generator renders rather than from a
// number written down here. A measured one would fail every time the catalogue
// gains a type or an offset adds a transition, with nothing in the failure to
// tell a deliberate change from a generator that lost half a month, and
// re-measuring is the same edit either way.
func nonBillableNotifications(t *testing.T, seed uint64, month, cloud string) int {
	t.Helper()

	from, to, err := period.Parse(month)
	if err != nil {
		t.Fatalf("period.Parse(%q) error = %v, want nil", month, err)
	}
	generated, err := simulator.GenerateMonth(seed, from, to, cloud, simulator.Faults{})
	if err != nil {
		t.Fatalf("GenerateMonth() error = %v, want nil", err)
	}

	count := 0
	for _, transition := range generated.Schedule {
		if !transition.Billable {
			count++
		}
	}
	return count
}

func TestHelpNeedsNoEnvironment(t *testing.T) {
	blankEnvironment(t)

	// The root and every leaf. Building the tree reads no configuration, so the
	// help of all of them works on a machine that has none.
	for _, tc := range []struct {
		name string
		path []string
	}{
		{"tally-openstack-simulator", nil},
		{"run", []string{"run"}},
		{"replay", []string{"replay"}},
		{"compare", []string{"compare"}},
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

	oracle, err := simulator.ReadOracle(filepath.Join(dir, "oracle.json"))
	if err != nil {
		t.Fatalf("ReadOracle() error = %v, want nil", err)
	}
	oracleBytes, _ := readLines(t, filepath.Join(dir, "oracle.json"))
	from, to, err := period.Parse(endedMonth)
	if err != nil {
		t.Fatalf("period.Parse(%q) error = %v, want nil", endedMonth, err)
	}
	if oracle.Cloud != simulatedCloud || oracle.Seed != 1 {
		t.Errorf("oracle.json states seed %d of %q, want seed 1 of %q", oracle.Seed, oracle.Cloud,
			simulatedCloud)
	}
	if !oracle.PeriodFrom.Equal(from) || !oracle.PeriodTo.Equal(to) {
		t.Errorf("oracle.json covers [%s, %s), want the [%s, %s) the run was given",
			oracle.PeriodFrom.Format(time.RFC3339), oracle.PeriodTo.Format(time.RFC3339),
			from.Format(time.RFC3339), to.Format(time.RFC3339))
	}

	// The traffic a file-mode run records although it pushes nothing: one row
	// per interval of every instance, ordered by resource id and then by from,
	// which is the order the oracle states its rows in.
	if len(oracle.Traffic) == 0 {
		t.Error("oracle.json states no traffic row, want one per interval of every instance")
	}
	if !slices.IsSortedFunc(oracle.Traffic, func(a, b simulator.OracleTraffic) int {
		if c := strings.Compare(a.ResourceID, b.ResourceID); c != 0 {
			return c
		}
		return a.From.Compare(b.From)
	}) {
		t.Error("oracle.json states its traffic rows out of order, want them by resource id and then by from")
	}
	instances := make(map[string]bool)
	for _, resource := range oracle.Resources {
		if resource.ResourceType == "instance" {
			instances[resource.ResourceID] = true
		}
	}
	for _, row := range oracle.Traffic {
		if !instances[row.ResourceID] {
			t.Errorf("oracle.json states traffic for %q, want every row to name an instance it holds",
				row.ResourceID)
		}
	}

	skipped := nonBillableNotifications(t, 1, endedMonth, simulatedCloud)
	if want := len(eventLines) + skipped; len(notificationLines) != want {
		t.Errorf("notifications.jsonl has %d lines, want %d: %d events plus the %d non-billable ones",
			len(notificationLines), want, len(eventLines), skipped)
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
			"oracle.json":         oracleBytes,
		} {
			again, _ := readLines(t, filepath.Join(second, name))
			if !bytes.Equal(first, again) {
				t.Errorf("%s differs between two runs of the same seed, period, and cloud", name)
			}
		}
	})
}

// TestRunWritesTheHeldBackFileInFileMode covers the fourth file a run writes:
// the notifications the held-back switch keeps off the bus. They are missing
// from notifications.jsonl, so the two files together are the month
// events.jsonl and the noise account for.
func TestRunWritesTheHeldBackFileInFileMode(t *testing.T) {
	useCloud(t)

	dir := t.TempDir()
	if _, stderr, err := runCLI(t, "run", "--period", endedMonth, "--seed", "1",
		"--faults", "held-back", "--out", dir); err != nil {
		t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
	}

	heldPath := filepath.Join(dir, "held-back.jsonl")
	_, heldLines := readLines(t, heldPath)
	// Through the reader a replay uses, so the file is one the simulator itself
	// can put on a bus later.
	held, err := simulator.ReadStream(heldPath)
	if err != nil {
		t.Fatalf("ReadStream(%s) error = %v, want nil", heldPath, err)
	}
	if len(held) != len(heldLines) {
		t.Errorf("ReadStream read %d notifications of the %d lines of held-back.jsonl",
			len(held), len(heldLines))
	}

	_, notificationLines := readLines(t, filepath.Join(dir, "notifications.jsonl"))
	_, eventLines := readLines(t, filepath.Join(dir, "events.jsonl"))
	// The switch leaves the schedule alone, so the non-billable notifications are
	// the ones a month without it carries.
	skipped := nonBillableNotifications(t, 1, endedMonth, simulatedCloud)
	if got, want := len(notificationLines)+len(heldLines), len(eventLines)+skipped; got != want {
		t.Errorf("notifications.jsonl and held-back.jsonl hold %d lines together, want %d: "+
			"%d events plus the %d non-billable ones", got, want, len(eventLines), skipped)
	}

	t.Run("a second run without the switch takes the file away", func(t *testing.T) {
		if _, stderr, err := runCLI(t,
			"run", "--period", endedMonth, "--seed", "1", "--out", dir); err != nil {
			t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
		}
		if _, err := os.Stat(heldPath); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Stat(%s) error = %v, want it to wrap fs.ErrNotExist: a directory reused "+
				"across runs must not carry the held share of another month", heldPath, err)
		}
	})

	t.Run("a rerun that fails partway takes every earlier file away too", func(t *testing.T) {
		reused := t.TempDir()
		stale := writeFile(t, reused, "held-back.jsonl", "{}\n")
		staleOracle := writeFile(t, reused, "oracle.json", "{}\n")
		// A directory in the way of one of the files is a run that ends partway
		// through the output directory. Whatever ends it there, no file of the
		// earlier month may stay behind: a drill would compare this month against
		// that oracle, and a replay would put that held share on the bus. Each
		// directory holds a file of its own so the run cannot take it away and
		// carry on, and there are two of them because what an operator clears out
		// after such a run is every path the run could not take away, not the
		// first one it reached.
		obstructions := []string{
			filepath.Join(reused, "notifications.jsonl"),
			filepath.Join(reused, "events.jsonl"),
		}
		for _, obstruction := range obstructions {
			if err := os.Mkdir(obstruction, 0o755); err != nil {
				t.Fatalf("making the directory that fails the run: %v", err)
			}
			writeFile(t, obstruction, "keep", "")
		}

		_, stderr, err := runCLI(t, "run", "--period", endedMonth, "--seed", "1", "--out", reused)
		if err == nil {
			t.Fatalf("run error = nil, want the failure over %v (stderr %q)", obstructions, stderr)
		}
		for _, obstruction := range obstructions {
			if !strings.Contains(err.Error(), obstruction) {
				t.Errorf("run error = %v, want it to name %s too: a path the run could not take "+
					"away is a file of the earlier month still in the directory", err, obstruction)
			}
		}
		for _, path := range []string{stale, staleOracle} {
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Stat(%s) error = %v, want it to wrap fs.ErrNotExist: a run that fails "+
					"must not leave a file of another month behind", path, err)
			}
		}
	})
}

// registryStub stands in for the project registry of the Reporting API. It
// counts the projects and the relations it was posted and keeps every
// credential it was sent, which is what a case holds a registration against.
type registryStub struct {
	*httptest.Server
	mu             sync.Mutex
	projects       int
	relations      int
	authorizations []string
}

// newRegistryStub starts a registry that holds nothing yet: every post is a row
// that did not exist, so every answer is a 201 carrying the id the relations
// address the row by.
func newRegistryStub(t *testing.T) *registryStub {
	t.Helper()

	stub := &registryStub{}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.authorizations = append(stub.authorizations, r.Header.Get("Authorization"))
		if r.Method == http.MethodPost {
			switch {
			case strings.HasSuffix(r.URL.Path, "/relations"):
				stub.relations++
			case r.URL.Path == "/api/v1/projects":
				stub.projects++
			}
		}
		stub.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		// The registration reads what a project is already related to before it
		// creates a relation, and this registry holds none.
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/relations") {
			if _, err := fmt.Fprint(w, `{"items":[]}`); err != nil {
				t.Errorf("answering %s %s: %v", r.Method, r.URL.Path, err)
			}
			return
		}
		w.WriteHeader(http.StatusCreated)
		if _, err := fmt.Fprintf(w, `{"id":%q}`, uuid.New()); err != nil {
			t.Errorf("answering %s %s: %v", r.Method, r.URL.Path, err)
		}
	}))
	t.Cleanup(stub.Close)
	return stub
}

// counts is how many projects and relations the stub was posted.
func (s *registryStub) counts() (projects, relations int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.projects, s.relations
}

// credentials is the Authorization header of every request the stub was sent.
func (s *registryStub) credentials() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.authorizations)
}

// holdCredentialsTo holds every credential the stub was sent against that
// token. A stub that was sent nothing fails the case rather than passing it: a
// check over an empty list is a registration that never happened.
func holdCredentialsTo(t *testing.T, stub *registryStub, token string) {
	t.Helper()

	credentials := stub.credentials()
	if len(credentials) == 0 {
		t.Fatalf("the registry was sent no request at all, want the month registered")
	}
	for _, credential := range credentials {
		if want := "Bearer " + token; credential != want {
			t.Errorf("the registry was sent the credential %q, want %q", credential, want)
		}
	}
}

// logLine is the JSON log line the run wrote under that message. The run logs
// through the command's own output, so what a case reads is the stdout runCLI
// hands back.
func logLine(t *testing.T, stdout, message string) map[string]any {
	t.Helper()

	for _, raw := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		var line map[string]any
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			continue
		}
		if line["msg"] == message {
			return line
		}
	}
	t.Fatalf("the run logged no %q line, and the case needs one:\n%s", message, stdout)
	return nil
}

// TestRunRegistersTheMonthInFileMode covers --register-projects: the tenants of
// the month, the two Gardener projects, and the relation that attributes a
// tenant's cost to the project running on it reach the registry before the
// month is written. File mode registers as well, because that is how an
// operator prepares the registry for a replay of the recorded month.
func TestRunRegistersTheMonthInFileMode(t *testing.T) {
	useCloud(t)
	stub := newRegistryStub(t)
	t.Setenv("TALLY_SIM_REPORTING_URL", stub.URL)
	// The stub serves plaintext on this machine, which is the one place a
	// registration may carry the admin api token in the clear, and it has to be
	// asked for the way a deployment asks for it.
	t.Setenv("TALLY_SIM_REPORTING_INSECURE", "true")
	t.Setenv("TALLY_SIM_API_TOKEN", testAPIToken)
	t.Setenv("TALLY_SIM_GARDEN_CLOUD", gardenCloud)

	dir := t.TempDir()
	stdout, stderr, err := runCLI(t,
		"run", "--register-projects", "--period", endedMonth, "--seed", "1", "--out", dir)
	if err != nil {
		t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
	}

	projects, relations := stub.counts()
	if projects != registeredProjects || relations != registeredRelations {
		t.Errorf("the registry was posted %d projects and %d relations, want %d and %d: "+
			"a row per tenant, a row per Gardener project, and a relation per Gardener project",
			projects, relations, registeredProjects, registeredRelations)
	}
	holdCredentialsTo(t, stub, testAPIToken)

	// The month is written too, and after the registration: a run that registers
	// leaves the same three files behind as one that does not.
	for _, name := range []string{"notifications.jsonl", "events.jsonl", "oracle.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("Stat(%s) error = %v, want nil", filepath.Join(dir, name), err)
		}
	}

	// The log is what an operator reads the registration off, so it states that
	// the switch was on and what the registry answered.
	if starting := logLine(t, stdout, "starting"); starting["register"] != true {
		t.Errorf("the starting line carries register = %v, want true", starting["register"])
	}
	registered := logLine(t, stdout, "registered")
	if got := registered["reporting_url"]; got != stub.URL {
		t.Errorf("the registered line carries reporting_url = %v, want %q", got, stub.URL)
	}
	for key, want := range map[string]float64{
		"projects_created":   registeredProjects,
		"projects_existing":  0,
		"relations_created":  registeredRelations,
		"relations_existing": 0,
	} {
		if got, ok := registered[key].(float64); !ok || got != want {
			t.Errorf("the registered line carries %s = %v, want %v", key, registered[key], want)
		}
	}

	t.Run("without the flag", func(t *testing.T) {
		quiet := newRegistryStub(t)
		t.Setenv("TALLY_SIM_REPORTING_URL", quiet.URL)

		if _, stderr, err := runCLI(t,
			"run", "--period", endedMonth, "--seed", "1", "--out", t.TempDir()); err != nil {
			t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
		}
		if projects, relations := quiet.counts(); projects+relations > 0 {
			t.Errorf("the registry was posted %d projects and %d relations, want nothing at all: "+
				"a run without --register-projects registers no row", projects, relations)
		}
	})

	t.Run("the token comes from a file", func(t *testing.T) {
		fromFile := newRegistryStub(t)
		t.Setenv("TALLY_SIM_REPORTING_URL", fromFile.URL)
		t.Setenv("TALLY_SIM_API_TOKEN", "")
		t.Setenv("TALLY_SIM_API_TOKEN_FILE", writeFile(t, t.TempDir(), "api-token", "tly_a_x\n"))

		if _, stderr, err := runCLI(t, "run", "--register-projects", "--period", endedMonth,
			"--seed", "1", "--out", t.TempDir()); err != nil {
			t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
		}
		// The trailing newline a Kubernetes Secret volume writes is not part of
		// the credential.
		holdCredentialsTo(t, fromFile, "tly_a_x")
	})
}

func TestRunRefusesBadInput(t *testing.T) {
	notADirectory := writeFile(t, t.TempDir(), "notifications.jsonl", "")
	urlFile := writeFile(t, t.TempDir(), "amqp-url", closedBroker)
	emptyURLFile := writeFile(t, t.TempDir(), "empty-amqp-url", "")
	tokenFile := writeFile(t, t.TempDir(), "api-token", "tly_a_x\n")
	emptyTokenFile := writeFile(t, t.TempDir(), "empty-api-token", "")
	passwordFile := writeFile(t, t.TempDir(), "otlp-password", "pw\n")
	emptyPasswordFile := writeFile(t, t.TempDir(), "empty-otlp-password", "")
	// The output directory of the case about an unknown fault switch. It is held
	// outside the case so the body can read it back: a refused switch has to end
	// the run before the month is written.
	faultOut := t.TempDir()
	// The same for the case about a registry nothing listens on: a registration
	// that fails ends the run before the month is written.
	registerOut := t.TempDir()
	// And for the two the metrics are refused by: the grid is checked with the
	// other flags and the endpoint before the broker is dialled, so neither
	// reaches the month either.
	intervalOut := t.TempDir()
	pushOut := t.TempDir()

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
		// emptyDir is a directory the run must have written nothing into, which
		// is how a case checks that it ended before it generated the month.
		emptyDir string
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
		{
			name: "a fault switch nobody named that way",
			args: []string{"--period", endedMonth, "--faults", "bogus", "--out", faultOut},
			want: `--faults: unknown fault switch "bogus"; the switches are ` +
				`pre-existing, missing-create, duplicates, reordering, refused-shapes, held-back`,
			emptyDir: faultOut,
		},
		{
			name: "two fault switches that exclude each other",
			args: []string{
				"--period", endedMonth, "--faults", "pre-existing,missing-create",
				"--out", t.TempDir(),
			},
			want: "--faults: pre-existing and missing-create exclude each other",
		},
		{
			name: "a metrics interval of zero",
			// The value is attached with =, for the reason the negative factor's
			// case gives.
			args:     []string{"--period", endedMonth, "--metrics-interval=0", "--out", intervalOut},
			want:     "--metrics-interval: 0s must be a whole number of seconds between 30s and 24h0m0s",
			emptyDir: intervalOut,
		},
		{
			name: "a negative metrics interval",
			args: []string{"--period", endedMonth, "--metrics-interval=-5m", "--out", t.TempDir()},
			want: "--metrics-interval: -5m0s must be a whole number of seconds between 30s and 24h0m0s",
		},
		{
			name: "a metrics interval that is not whole seconds",
			args: []string{"--period", endedMonth, "--metrics-interval=1500ms", "--out", t.TempDir()},
			want: "--metrics-interval: 1.5s must be a whole number of seconds between 30s and 24h0m0s",
		},
		{
			// A month is placed whole before it is published, and a grid an order
			// of magnitude below Ceilometer's is where that stops fitting into
			// memory, so it is refused rather than started and killed.
			name: "a metrics interval finer than the grid a month is placed on",
			args: []string{"--period", endedMonth, "--metrics-interval=1s", "--out", t.TempDir()},
			want: "--metrics-interval: 1s must be a whole number of seconds between 30s and 24h0m0s",
		},
		{
			name: "a metrics interval longer than a day",
			args: []string{"--period", endedMonth, "--metrics-interval=25h", "--out", t.TempDir()},
			want: "--metrics-interval: 25h0m0s must be a whole number of seconds between 30s and 24h0m0s",
		},
		{
			name: "a push without a user",
			env:  map[string]string{"TALLY_SIM_OTLP_URL": "https://127.0.0.1:1/v1/metrics"},
			args: []string{"--period", endedMonth, "--out", pushOut},
			want: "checking the configuration: TALLY_SIM_OTLP_USER: " +
				"must be set when TALLY_SIM_OTLP_URL is set",
			emptyDir: pushOut,
		},
		{
			name: "a push without a password",
			env: map[string]string{
				"TALLY_SIM_OTLP_URL":  "https://127.0.0.1:1/v1/metrics",
				"TALLY_SIM_OTLP_USER": "tally",
			},
			args: []string{"--period", endedMonth, "--out", t.TempDir()},
			want: "checking the configuration: TALLY_SIM_OTLP_PASSWORD: " +
				"must be set when TALLY_SIM_OTLP_URL is set",
		},
		{
			name: "a plaintext push nobody allowed",
			env: map[string]string{
				"TALLY_SIM_OTLP_URL":      "http://127.0.0.1:1/v1/metrics",
				"TALLY_SIM_OTLP_USER":     "tally",
				"TALLY_SIM_OTLP_PASSWORD": testOTLPPassword,
			},
			args:     []string{"--period", endedMonth, "--out", t.TempDir()},
			contains: "TALLY_SIM_OTLP_INSECURE",
			absent:   testOTLPPassword,
		},
		{
			name: "an OTLP password given twice",
			env: map[string]string{
				"TALLY_SIM_OTLP_PASSWORD":      testOTLPPassword,
				"TALLY_SIM_OTLP_PASSWORD_FILE": passwordFile,
			},
			args: []string{"--period", endedMonth, "--out", t.TempDir()},
			want: "loading the configuration: set TALLY_SIM_OTLP_PASSWORD or " +
				"TALLY_SIM_OTLP_PASSWORD_FILE, not both",
			absent: testOTLPPassword,
		},
		{
			name:     "an OTLP password file nobody filled",
			env:      map[string]string{"TALLY_SIM_OTLP_PASSWORD_FILE": emptyPasswordFile},
			args:     []string{"--period", endedMonth, "--out", t.TempDir()},
			contains: fmt.Sprintf("TALLY_SIM_OTLP_PASSWORD_FILE: file %s is empty", emptyPasswordFile),
		},
		{
			name: "a registration without a Reporting API",
			args: []string{"--register-projects", "--period", endedMonth, "--out", t.TempDir()},
			want: "checking the configuration: TALLY_SIM_REPORTING_URL: " +
				"must be set when --register-projects is on",
		},
		{
			name: "a registration without a token",
			env:  map[string]string{"TALLY_SIM_REPORTING_URL": closedRegistry},
			args: []string{"--register-projects", "--period", endedMonth, "--out", t.TempDir()},
			want: "checking the configuration: TALLY_SIM_API_TOKEN: " +
				"must be set when --register-projects is on",
		},
		{
			name: "a registration without a cloud for the Gardener projects",
			env: map[string]string{
				"TALLY_SIM_REPORTING_URL": closedRegistry,
				"TALLY_SIM_API_TOKEN":     testAPIToken,
			},
			args: []string{"--register-projects", "--period", endedMonth, "--out", t.TempDir()},
			want: "checking the configuration: TALLY_SIM_GARDEN_CLOUD: " +
				"must be set when --register-projects is on",
		},
		{
			name: "the Gardener projects under the tenants' cloud",
			env: map[string]string{
				"TALLY_SIM_REPORTING_URL": closedRegistry,
				"TALLY_SIM_API_TOKEN":     testAPIToken,
				"TALLY_SIM_GARDEN_CLOUD":  simulatedCloud,
			},
			args: []string{"--register-projects", "--period", endedMonth, "--out", t.TempDir()},
			want: `checking the configuration: TALLY_SIM_GARDEN_CLOUD: "os-sim" must differ from ` +
				"TALLY_SIM_CLOUD: a cloud is one installation of one platform",
		},
		{
			name: "a Reporting API that is not an HTTP URL",
			env: map[string]string{
				"TALLY_SIM_REPORTING_URL": "ftp://api",
				"TALLY_SIM_API_TOKEN":     testAPIToken,
				"TALLY_SIM_GARDEN_CLOUD":  gardenCloud,
			},
			args: []string{"--register-projects", "--period", endedMonth, "--out", t.TempDir()},
			want: "checking the configuration: " +
				`TALLY_SIM_REPORTING_URL: "ftp://api" must be an absolute http(s) URL with no ` +
				"query or fragment, because the registry route is appended to it",
		},
		{
			name: "a token given twice",
			env: map[string]string{
				"TALLY_SIM_API_TOKEN":      testAPIToken,
				"TALLY_SIM_API_TOKEN_FILE": tokenFile,
			},
			args: []string{"--register-projects", "--period", endedMonth, "--out", t.TempDir()},
			want: "loading the configuration: set TALLY_SIM_API_TOKEN or TALLY_SIM_API_TOKEN_FILE, not both",
		},
		{
			name:     "a token file nobody filled",
			env:      map[string]string{"TALLY_SIM_API_TOKEN_FILE": emptyTokenFile},
			args:     []string{"--register-projects", "--period", endedMonth, "--out", t.TempDir()},
			contains: fmt.Sprintf("TALLY_SIM_API_TOKEN_FILE: file %s is empty", emptyTokenFile),
		},
		{
			name: "a registry nothing listens on",
			env: map[string]string{
				"TALLY_SIM_REPORTING_URL": closedRegistry,
				"TALLY_SIM_API_TOKEN":     testAPIToken,
				"TALLY_SIM_GARDEN_CLOUD":  gardenCloud,
			},
			args:     []string{"--register-projects", "--period", endedMonth, "--out", registerOut},
			contains: "registering the projects: POST /api/v1/projects:",
			// The failure names the route and the dial, and the token stays out of
			// it: an operator pastes such a message into a ticket.
			absent: testAPIToken,
			// The registration runs before the month is written, so a run that
			// cannot register leaves no file behind either.
			emptyDir: registerOut,
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
			if tc.emptyDir != "" {
				entries, err := os.ReadDir(tc.emptyDir)
				if err != nil {
					t.Fatalf("reading %s: %v", tc.emptyDir, err)
				}
				if len(entries) > 0 {
					t.Errorf("%s holds %d entries, want the run to have written nothing", tc.emptyDir, len(entries))
				}
			}
		})
	}
}

// TestRunHelpListsTheFaultSwitches holds the help of run to the six switches.
// A switch --faults takes but the help does not name is one nobody reaches
// without reading the source.
func TestRunHelpListsTheFaultSwitches(t *testing.T) {
	blankEnvironment(t)

	stdout, stderr, err := runCLI(t, "run", "--help")
	if err != nil {
		t.Fatalf("run --help error = %v, want nil (stderr %q)", err, stderr)
	}
	if want := "--faults"; !strings.Contains(stdout, want) {
		t.Fatalf("stdout = %q, want the flag %q in it", stdout, want)
	}
	for _, name := range simulator.FaultNames {
		if !strings.Contains(stdout, name) {
			t.Errorf("run --help does not name the fault switch %q, want all of %v listed",
				name, simulator.FaultNames)
		}
	}
}

// TestRunHelpListsTheMetricsInterval holds the help of run to the grid the
// metrics lie on and to the interval it takes when nobody names one. How many
// points a month is pushed as follows from that grid, so an operator who has to
// change it has to find it.
func TestRunHelpListsTheMetricsInterval(t *testing.T) {
	blankEnvironment(t)

	stdout, stderr, err := runCLI(t, "run", "--help")
	if err != nil {
		t.Fatalf("run --help error = %v, want nil (stderr %q)", err, stderr)
	}
	for _, want := range []string{"--metrics-interval", "300s"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("run --help does not name %q, want it in the help of the flag", want)
		}
	}
}

// TestRunHelpListsTheRegistrySwitch holds the help of run to the switch that
// registers the month. A switch the help does not name is one nobody reaches
// without reading the source.
func TestRunHelpListsTheRegistrySwitch(t *testing.T) {
	blankEnvironment(t)

	stdout, stderr, err := runCLI(t, "run", "--help")
	if err != nil {
		t.Fatalf("run --help error = %v, want nil (stderr %q)", err, stderr)
	}
	for _, want := range []string{"--register-projects", "TALLY_SIM_REPORTING_URL"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("run --help does not name %q, want it in the help of the switch", want)
		}
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

// testModel prices three of the five resource types a month holds and leaves
// the other two unpriced, the way the shipped models do. It is the same
// fixture the simulator package's own comparison tests read, repeated here
// because a test binary reaches no other package's test files.
const testModel = `
version: "test"
valid_from: "2026-01-01T00:00:00Z"
currency: "EUR"

pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.001"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.09"
    volume:
      dimensions:
        - metric: "size_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.0001"
        - metric: "minutes"
          type: "time_gauge"
          price_per_unit_hour: "0.00001"
    floating_ip:
      dimensions:
        - metric: "count"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
`

// The two resource types the cases name: the one the counter dimension belongs
// to, and the one the case about a lost resource drops from the export.
const (
	instanceType = "instance"
	volumeType   = "volume"
)

// testRunID is the run every rendered row belongs to. A comparison reads
// neither the run nor its kind, so one id stands for the whole export.
const testRunID = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

// testEgress is the quantity every counter row carries. No transition of a
// simulated month meters egress, so no comparison reads the quantity of a
// counter row.
const testEgress = "12.3456"

// ratedHeader is the header of the rated.csv tally-engine export --format csv
// writes. A comparison finds its columns by name, and these are the names the
// two commands agree on.
var ratedHeader = []string{
	"run_id", "kind", "corrects_run_id", "period_from", "period_to",
	"cloud", "platform", "resource_type", "resource_id", "project_id", "state",
	"from_ts", "to_ts", "dimension", "quantity", "amount", "currency",
}

// columnResourceID is where resource_id stands in ratedHeader, which is how
// the case about a lost resource finds the rows to drop.
const columnResourceID = 8

// timeGauges are the time gauge dimensions testModel prices each resource type
// by. A type the map does not hold is one the model leaves unpriced, so no row
// of the export is written for its resources and no comparison examines them.
var timeGauges = map[string][]string{
	instanceType:  {"vcpus", "ram_gb", "disk_gb"},
	volumeType:    {"size_gb", "minutes"},
	"floating_ip": {"count"},
}

func TestCompareRequiresItsFlags(t *testing.T) {
	blankEnvironment(t)

	// Cobra names every missing flag at once, so each case checks the one it is
	// about rather than the whole message.
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"nothing to compare", nil, `"oracle"`},
		{"an oracle alone", []string{"--oracle", "x"}, `"export"`},
		{"an oracle and an export", []string{"--oracle", "x", "--export", "y"}, `"pricing"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := runCLI(t, append([]string{"compare"}, tc.args...)...)
			if err == nil {
				t.Fatalf("compare error = nil, want the missing flag reported (stderr %q)", stderr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("compare error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestCompareMatchesAndDiffers runs the comparison over an export rendered
// from the oracle itself. Such an export is what an engine that billed the
// month exactly as the generator built it would write, so the cases hold the
// command against a month it must pass and against one it must fail.
func TestCompareMatchesAndDiffers(t *testing.T) {
	useCloud(t)

	dir := t.TempDir()
	if _, stderr, err := runCLI(t,
		"run", "--period", endedMonth, "--seed", "1", "--out", dir); err != nil {
		t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
	}
	oraclePath := filepath.Join(dir, "oracle.json")
	oracle, err := simulator.ReadOracle(oraclePath)
	if err != nil {
		t.Fatalf("ReadOracle() error = %v, want nil", err)
	}
	model := writeFile(t, t.TempDir(), "pricing.yaml", testModel)
	rows := ratedOf(t, oracle)

	t.Run("an export that bills the month as the oracle states it", func(t *testing.T) {
		stdout, stderr, err := runCLI(t, "compare",
			"--oracle", oraclePath, "--export", writeRated(t, rows), "--pricing", model)
		if err != nil {
			t.Fatalf("compare error = %v, want nil (stderr %q)", err, stderr)
		}
		want := fmt.Sprintf("the export matches the oracle over %d resources", pricedResources(oracle))
		if last := lastLine(stdout); last != want {
			t.Errorf("last line = %q, want %q (stdout %q)", last, want, stdout)
		}
	})

	t.Run("an export a volume never reached", func(t *testing.T) {
		id := firstResourceID(t, oracle, volumeType)
		kept := make([][]string, 0, len(rows))
		for _, row := range rows {
			if row[columnResourceID] != id {
				kept = append(kept, row)
			}
		}

		stdout, _, err := runCLI(t, "compare",
			"--oracle", oraclePath, "--export", writeRated(t, kept), "--pricing", model)
		if err == nil {
			t.Fatalf("compare error = nil, want the missing volume reported (stdout %q)", stdout)
		}
		if want := "1 resources differ from the oracle"; err.Error() != want {
			t.Errorf("compare error = %q, want %q", err, want)
		}
		want := fmt.Sprintf("%s %s: missing from the export", volumeType, id)
		if !strings.Contains(stdout, want+"\n") {
			t.Errorf("stdout = %q, want a line %q", stdout, want)
		}
	})

	t.Run("a resource a switch touched", func(t *testing.T) {
		// A month of its own, because a switch names the resources it touched in
		// the oracle, and the oracle of the plain month above names none.
		faultyDir := t.TempDir()
		if _, stderr, err := runCLI(t, "run", "--period", endedMonth, "--seed", "1",
			"--faults", simulator.FaultMissingCreate, "--out", faultyDir); err != nil {
			t.Fatalf("run error = %v, want nil (stderr %q)", err, stderr)
		}
		faultyPath := filepath.Join(faultyDir, "oracle.json")
		faultyOracle, err := simulator.ReadOracle(faultyPath)
		if err != nil {
			t.Fatalf("ReadOracle() error = %v, want nil", err)
		}

		resourceType, id := firstTouchedResource(t, faultyOracle, simulator.FaultMissingCreate)
		faultyRows := ratedOf(t, faultyOracle)
		kept := make([][]string, 0, len(faultyRows))
		for _, row := range faultyRows {
			if row[columnResourceID] != id {
				kept = append(kept, row)
			}
		}

		stdout, _, err := runCLI(t, "compare",
			"--oracle", faultyPath, "--export", writeRated(t, kept), "--pricing", model)
		if err == nil {
			t.Fatalf("compare error = nil, want the missing resource reported (stdout %q)", stdout)
		}
		if want := "1 resources differ from the oracle"; err.Error() != want {
			t.Errorf("compare error = %q, want %q", err, want)
		}
		// The switch stands beside the difference it explains, and under the
		// differences stand the switches the month ran with.
		want := fmt.Sprintf("%s %s: missing from the export (touched by %s)",
			resourceType, id, simulator.FaultMissingCreate)
		if !strings.Contains(stdout, want+"\n") {
			t.Errorf("stdout = %q, want a line %q", stdout, want)
		}
		want = "the month ran with the fault switches " + simulator.FaultMissingCreate
		if !strings.Contains(stdout, want+"\n") {
			t.Errorf("stdout = %q, want a line %q", stdout, want)
		}
	})

	t.Run("an export directory holding no rated.csv", func(t *testing.T) {
		_, stderr, err := runCLI(t, "compare",
			"--oracle", oraclePath, "--export", t.TempDir(), "--pricing", model)
		if err == nil {
			t.Fatalf("compare error = nil, want the missing rated.csv reported (stderr %q)", stderr)
		}
		if want := "rated.csv"; !strings.Contains(err.Error(), want) {
			t.Errorf("compare error = %q, want it to contain %q", err, want)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("compare error = %v, want it to wrap fs.ErrNotExist", err)
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

// ratedOf renders the rows an export holds for a month the engine billed
// without a fault: the header, and one row per resource of a priced type,
// interval and dimension of that type. A time gauge carries the quantity the
// oracle states, and the counter one nothing reads.
func ratedOf(t *testing.T, oracle simulator.Oracle) [][]string {
	t.Helper()

	rows := [][]string{ratedHeader}
	for _, resource := range oracle.Resources {
		dimensions, priced := timeGauges[resource.ResourceType]
		if !priced {
			continue
		}
		for _, interval := range resource.Intervals {
			for _, dimension := range dimensions {
				rows = append(rows, ratedRow(oracle, resource, interval,
					dimension, quantityOf(t, dimension, interval)))
			}
			if resource.ResourceType == instanceType {
				rows = append(rows, ratedRow(oracle, resource, interval, "egress_gb", testEgress))
			}
		}
	}
	return rows
}

// ratedRow renders one row of rated.csv, in the order ratedHeader names the
// columns.
func ratedRow(oracle simulator.Oracle, resource simulator.OracleResource,
	interval simulator.OracleInterval, dimension, quantity string,
) []string {
	return []string{
		testRunID, "regular", "",
		instantCell(oracle.PeriodFrom), instantCell(oracle.PeriodTo),
		oracle.Cloud, "openstack", resource.ResourceType, resource.ResourceID,
		interval.ProjectID, interval.State,
		instantCell(interval.From), instantCell(interval.To), dimension, quantity,
		"0.00", "EUR",
	}
}

// quantityOf is the quantity a rated row carries for one time gauge dimension
// of an interval: one for the dimension that prices a resource by its
// existence, the minutes the interval lasts, and otherwise the size member the
// dimension is named after, at the four places an export prints a quantity to.
// A dimension the interval states no number for fails the case rather than
// being read as a difference.
func quantityOf(t *testing.T, dimension string, interval simulator.OracleInterval) string {
	t.Helper()

	switch dimension {
	case "count":
		return "1.0000"
	case "minutes":
		return money.Minutes(int64(interval.To.Sub(interval.From) / time.Second)).StringFixed(4)
	}
	number, ok := interval.Size[dimension].(json.Number)
	if !ok {
		t.Fatalf("the interval carries %s = %v, want the json.Number the dimension is priced by",
			dimension, interval.Size[dimension])
	}
	// From the text the oracle carries rather than through a float: a quantity
	// that went through one is no longer the number the notification stated.
	value, err := decimal.NewFromString(number.String())
	if err != nil {
		t.Fatalf("NewFromString(%q) error = %v, want nil", number.String(), err)
	}
	return value.StringFixed(4)
}

// instantCell renders an instant the way an export writes one.
func instantCell(instant time.Time) string {
	return instant.UTC().Format(time.RFC3339Nano)
}

// writeRated writes the rows of an export into a rated.csv of its own and
// returns the directory holding it, which is what --export names.
func writeRated(t *testing.T, rows [][]string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "rated.csv")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	writer := csv.NewWriter(file)
	if err := writer.WriteAll(rows); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return dir
}

// pricedResources counts the resources a comparison examines: the ones
// testModel prices, and no others.
func pricedResources(oracle simulator.Oracle) int {
	count := 0
	for _, resource := range oracle.Resources {
		if _, priced := timeGauges[resource.ResourceType]; priced {
			count++
		}
	}
	return count
}

// firstResourceID is the id of the oracle's first resource of that type.
func firstResourceID(t *testing.T, oracle simulator.Oracle, resourceType string) string {
	t.Helper()

	for _, resource := range oracle.Resources {
		if resource.ResourceType == resourceType {
			return resource.ResourceID
		}
	}
	t.Fatalf("the oracle holds no %s, and the case needs one", resourceType)
	return ""
}

// firstTouchedResource is the oracle's first resource of a priced type that the
// switch touched, by type and id. A resource of an unpriced type carries no row
// in an export, so dropping one would report no difference at all.
func firstTouchedResource(t *testing.T, oracle simulator.Oracle, fault string) (resourceType, id string) {
	t.Helper()

	for _, resource := range oracle.Resources {
		if _, priced := timeGauges[resource.ResourceType]; !priced {
			continue
		}
		if slices.Contains(resource.Faults, fault) {
			return resource.ResourceType, resource.ResourceID
		}
	}
	t.Fatalf("the oracle names no resource of a priced type the %s switch touched", fault)
	return "", ""
}

// lastLine is the last line of what a command printed, which is the verdict of
// a report.
func lastLine(stdout string) string {
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	return lines[len(lines)-1]
}

// TestReferencePageIsCurrent holds the command line reference page of this
// binary to the tree it documents. A subcommand or a flag added here without
// the page being regenerated fails this test rather than leaving a reader with
// a tree the binary no longer has.
func TestReferencePageIsCurrent(t *testing.T) {
	text, err := refdoc.Commands(newRootCmd())
	if err != nil {
		t.Fatalf("rendering the command tree: %v", err)
	}

	refdoc.Verify(t, "../../docs/reference/command-line/tally-openstack-simulator.md",
		map[string]string{"commands": text})
}

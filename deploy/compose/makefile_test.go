// This file holds `make simulator-up` to the compose stack it starts. Both
// mistakes it looks for surface late. A node without an address lets the
// target build two images and issue a credential before compose refuses the
// empty extra_hosts entry, and a variable compose reads but the target never
// writes into .env is interpolated as the empty string, which starts the stack
// on an empty token, period or node address. The guard runs against a
// stand-in docker, so no test reaches an engine.
package compose_test

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const (
	makefilePath = "../../Makefile"

	// What simulator-up prints when the node guard refuses, and once it let the
	// target through: the first line of the image build.
	nodeRefusal       = "has no address on the kind network"
	simulatorUpPassed = "==> building tally-openstack-collector"
)

// envPrintfRe matches the format of the printf simulator-up writes .env with,
// and envNameRe the names in it. composeVarRe matches a variable compose
// interpolates.
var (
	envPrintfRe  = regexp.MustCompile(`printf '([^']*)' \\\n[^\n]*> deploy/compose/\.env\n`)
	envNameRe    = regexp.MustCompile(`([A-Z0-9_]+)=%s`)
	composeVarRe = regexp.MustCompile(`\$\{([A-Z0-9_]+)`)
)

func TestSimulatorUpRefusesANodeWithoutAnAddress(t *testing.T) {
	// The stand-in answers docker inspect the way the engine does for each node,
	// and fails every other call, so a run the guard lets through stops at the
	// first image it builds.
	cases := []struct {
		name, inspect, want, notWant string
	}{
		{"no node", "echo 'Error: No such object: tally-control-plane' >&2; exit 1", nodeRefusal, simulatorUpPassed},
		{"a stopped node", "echo", nodeRefusal, simulatorUpPassed},
		{"a running node", "echo 172.18.0.2", simulatorUpPassed, nodeRefusal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			script := "#!/bin/sh\n[ \"$1\" = inspect ] || exit 1\n" + tc.inspect + "\n"
			if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
				t.Fatalf("writing the docker stand-in: %v", err)
			}

			if out, code := runSimulatorUp(t, bin); code == 0 || !strings.Contains(out, tc.want) || strings.Contains(out, tc.notWant) {
				t.Errorf("make simulator-up exited %d, want a failure carrying %q and not %q:\n%s", code, tc.want, tc.notWant, out)
			}
		})
	}
}

func TestSimulatorUpWritesEveryVariableComposeReads(t *testing.T) {
	// compose warns about a variable .env lacks and starts the stack on the
	// empty string in its place, so a name the Makefile and compose.yaml spell
	// apart fails at `compose up` or later, in a container's log.
	raw, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatalf("reading %s: %v", makefilePath, err)
	}
	format := envPrintfRe.FindSubmatch(raw)
	if format == nil {
		t.Fatalf("%s writes deploy/compose/.env with no printf this test recognizes, so it would assert over nothing", makefilePath)
	}
	var written []string
	for _, name := range envNameRe.FindAllSubmatch(format[1], -1) {
		written = append(written, string(name[1]))
	}

	compose, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatalf("reading %s: %v", composePath, err)
	}
	read := map[string]bool{}
	for _, name := range composeVarRe.FindAllSubmatch(compose, -1) {
		read[string(name[1])] = true
	}
	for _, name := range slices.Sorted(maps.Keys(read)) {
		if !slices.Contains(written, name) {
			t.Errorf("%s reads ${%s}, which simulator-up does not write into .env (it writes %v)", composePath, name, written)
		}
	}
}

// runSimulatorUp runs simulator-up from the repository root with bin first on
// the path, and returns what make printed and its exit code.
func runSimulatorUp(t *testing.T, bin string) (string, int) {
	t.Helper()

	makefile, err := filepath.Abs(makefilePath)
	if err != nil {
		t.Fatalf("resolving %s: %v", makefilePath, err)
	}
	cmd := exec.Command("make", "--silent", "--file", makefile, "simulator-up", "SIM_PERIOD=2026-07", "CLUSTER_NAME=tally")
	cmd.Dir = filepath.Dir(makefile)
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return slices.Contains([]string{"MAKEFLAGS", "MAKELEVEL", "MFLAGS", "PATH"}, name)
	}), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatalf("running make simulator-up: %v", err)
	return "", 0
}

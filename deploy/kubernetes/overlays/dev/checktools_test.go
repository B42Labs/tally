// This file runs `make check-tools` against stand-ins for every tool it
// probes, with nothing else on the path but the few commands the Makefile
// itself calls. The Go probe leaves Go's stderr on the terminal, so the line a
// Go older than the `toolchain` line of go.mod prints while it downloads that
// toolchain stands above the `ok` line, and lesson 1 tells the reader exactly
// that. A probe that captured stderr again would fold the download into the
// `ok` line, and one that took every failure for a missing Go would send a
// reader whose Go answers an error off to install it.
package dev_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const goModFile = "../../../../go.mod"

// goLineRe matches the `go` line of go.mod, the minimum the Go probe compares
// against.
var goLineRe = regexp.MustCompile(`(?m)^go ([0-9][0-9.]*)$`)

// toolchainLineRe matches the `toolchain` line of go.mod, the Go that a Go
// older than it downloads on its first run.
var toolchainLineRe = regexp.MustCompile(`(?m)^toolchain go([0-9][0-9.]*)$`)

// kindConfigFile is the cluster configuration whose `# kind-minimum:` line
// names the oldest kind check-tools accepts.
const kindConfigFile = "../../../kind/kind.yaml"

// kindMinimumRe matches the `# kind-minimum:` line of deploy/kind/kind.yaml,
// the minimum the kind probe compares against.
var kindMinimumRe = regexp.MustCompile(`(?m)^# kind-minimum: v([0-9][0-9.]*)$`)

func TestCheckToolsPrintsWhatGoAnsweredOnItsOwnLine(t *testing.T) {
	goMin, toolchain := goModVersions(t)

	// The curl probe runs right before Go's, so its line is the one above Go's
	// when Go writes nothing to stderr.
	const curlLine = "ok       curl            curl stand-in"
	cases := []struct {
		name   string
		goStub string // no stand-in at all when empty
		want   []string
		passes bool
	}{
		{
			"a first run downloads the toolchain",
			"echo 'go: downloading go" + toolchain + " (linux/amd64)' >&2\necho go" + toolchain + "\n",
			[]string{"go: downloading go" + toolchain + " (linux/amd64)", "ok       go              go" + toolchain + ", at or above the go " + goMin + " of go.mod"},
			true,
		},
		{
			"go answers an error",
			"echo 'go: cannot find GOROOT directory: /nowhere' >&2\nexit 2\n",
			[]string{"go: cannot find GOROOT directory: /nowhere", "broken   go              answered an error, printed above"},
			false,
		},
		{
			"go is older than go.mod",
			"echo go1.20.0\n",
			[]string{curlLine, "old      go              go1.20.0 is under the go " + goMin + " go.mod asks for"},
			false,
		},
		{
			"go is not on the path",
			"",
			[]string{curlLine, "missing  go              not on the path, and every binary here is built with it"},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bin := toolStandIns(t)
			if tc.goStub != "" {
				writeStub(t, bin, "go", tc.goStub)
			}

			out, code := runCheckTools(t, bin)

			lines := strings.Split(out, "\n")
			at := slices.IndexFunc(lines, func(line string) bool {
				fields := strings.Fields(line)
				return len(fields) > 1 && fields[1] == "go"
			})
			if at < 1 {
				t.Fatalf("make check-tools printed no line for go below another line:\n%s", out)
			}
			if got := lines[at-1 : at+1]; !slices.Equal(got, tc.want) {
				t.Errorf("the go line and the one above it are %q, want %q:\n%s", got, tc.want, out)
			}
			if passed := code == 0; passed != tc.passes {
				t.Errorf("make check-tools exited %d, want it to pass: %t:\n%s", code, tc.passes, out)
			}
		})
	}
}

func TestCheckToolsHoldsKindToTheMinimumOfTheKindConfig(t *testing.T) {
	kindMin := kindMinimum(t)
	goMin, _ := goModVersions(t)

	// The kind probe runs between the docker compose probe and the kubectl one.
	composeLine := fmt.Sprintf("%-8s %-15s %s", "ok", "docker compose", "docker stand-in")
	kubectlLine := fmt.Sprintf("%-8s %-15s %s", "ok", "kubectl", "kubectl stand-in")
	cases := []struct {
		name     string
		kindStub string // no stand-in at all when empty
		args     []string
		status   string
		text     string
		passes   bool
	}{
		{
			"kind is at the minimum",
			"echo 'kind v" + kindMin + " go1.26.3 linux/amd64'\n",
			nil,
			"ok",
			"kind v" + kindMin + " go1.26.3 linux/amd64, at or above the v" + kindMin + " of deploy/kind/kind.yaml",
			true,
		},
		{
			"kind is above the minimum",
			"echo 'kind v99.0.0 go1.26.3 linux/amd64'\n",
			nil,
			"ok",
			"kind v99.0.0 go1.26.3 linux/amd64, at or above the v" + kindMin + " of deploy/kind/kind.yaml",
			true,
		},
		{
			"a development build compares by its release digits",
			"echo 'kind v99.0.0-alpha+abc123 go1.26.3 linux/amd64'\n",
			nil,
			"ok",
			"kind v99.0.0-alpha+abc123 go1.26.3 linux/amd64, at or above the v" + kindMin + " of deploy/kind/kind.yaml",
			true,
		},
		{
			"a development build under the minimum compares by its release digits",
			"echo 'kind v0.31.0-alpha+abc123 go1.26.3 linux/amd64'\n",
			nil,
			"old",
			"v0.31.0 is under the v" + kindMin + " deploy/kind/kind.yaml asks for",
			false,
		},
		{
			"a pre-release of the minimum is older than the minimum",
			"echo 'kind v" + kindMin + "-alpha.17+abc123 go1.26.3 linux/amd64'\n",
			nil,
			"old",
			"v" + kindMin + "-alpha.17 is a pre-release of the v" + kindMin + " deploy/kind/kind.yaml asks for",
			false,
		},
		{
			"kind is older than the kind config asks for",
			"echo 'kind v0.30.0 go1.24.6 linux/amd64'\n",
			nil,
			"old",
			"v0.30.0 is under the v" + kindMin + " deploy/kind/kind.yaml asks for",
			false,
		},
		{
			"kind compares by version, not by text",
			"echo 'kind v0.9.0 go1.15.2 linux/amd64'\n",
			nil,
			"old",
			"v0.9.0 is under the v" + kindMin + " deploy/kind/kind.yaml asks for",
			false,
		},
		{
			"kind is not on the path",
			"",
			nil,
			"missing",
			"not on the path, and creating the dev cluster needs it",
			false,
		},
		{
			"kind answers an error",
			"echo 'kind: exec format error' >&2\nexit 1\n",
			nil,
			"broken",
			"kind: exec format error",
			false,
		},
		{
			"kind names no version",
			"echo 'kind stand-in'\n",
			nil,
			"broken",
			"kind stand-in, which names no version",
			false,
		},
		{
			"no minimum to compare against",
			"echo 'kind v" + kindMin + " go1.26.3 linux/amd64'\n",
			[]string{"KIND_MIN_VERSION="},
			"broken",
			"deploy/kind/kind.yaml names no kind-minimum line",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bin := toolStandIns(t)
			writeStub(t, bin, "go", "echo go"+goMin+"\n")
			if tc.kindStub == "" {
				if err := os.Remove(filepath.Join(bin, "kind")); err != nil {
					t.Fatalf("removing the kind stand-in: %v", err)
				}
			} else {
				writeStub(t, bin, "kind", tc.kindStub)
			}

			out, code := runCheckTools(t, bin, tc.args...)

			lines := strings.Split(out, "\n")
			var at []int
			for i, line := range lines {
				if fields := strings.Fields(line); len(fields) > 1 && fields[1] == "kind" {
					at = append(at, i)
				}
			}
			if len(at) != 1 || at[0] < 1 || at[0]+1 >= len(lines) {
				t.Fatalf("make check-tools printed %d lines for kind, want one between two others:\n%s", len(at), out)
			}
			want := []string{composeLine, fmt.Sprintf("%-8s %-15s %s", tc.status, "kind", tc.text), kubectlLine}
			if got := lines[at[0]-1 : at[0]+2]; !slices.Equal(got, want) {
				t.Errorf("the kind line and the lines around it are %q, want %q:\n%s", got, want, out)
			}
			if passed := code == 0; passed != tc.passes {
				t.Errorf("make check-tools exited %d, want it to pass: %t:\n%s", code, tc.passes, out)
			}
		})
	}
}

// goModVersions returns the versions on the `go` and the `toolchain` lines of
// go.mod.
func goModVersions(t *testing.T) (goMin, toolchain string) {
	t.Helper()

	raw, err := os.ReadFile(goModFile)
	if err != nil {
		t.Fatalf("reading %s: %v", goModFile, err)
	}
	goLine := goLineRe.FindSubmatch(raw)
	if goLine == nil {
		t.Fatalf("%s has no go line", goModFile)
	}
	toolchainLine := toolchainLineRe.FindSubmatch(raw)
	if toolchainLine == nil {
		t.Fatalf("%s has no toolchain line", goModFile)
	}
	return string(goLine[1]), string(toolchainLine[1])
}

// kindMinimum returns the version on the `# kind-minimum:` line of
// deploy/kind/kind.yaml, and fails the test when the file has no such line.
func kindMinimum(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(kindConfigFile)
	if err != nil {
		t.Fatalf("reading %s: %v", kindConfigFile, err)
	}
	line := kindMinimumRe.FindSubmatch(raw)
	if line == nil {
		t.Fatal("deploy/kind/kind.yaml has no kind-minimum line")
	}
	return string(line[1])
}

// toolStandIns returns a directory that holds a stand-in answering for every
// tool check-tools probes except Go, and links to the commands the Makefile
// runs on its own, so that directory alone can be the path. The kind stand-in
// answers `kind v<minimum> go1.26.3 linux/amd64`, with the minimum the
// `# kind-minimum:` line of deploy/kind/kind.yaml names. The docker stand-in
// fails `docker info`, which leaves out the line on the engine's size.
func toolStandIns(t *testing.T) string {
	t.Helper()

	bin := t.TempDir()
	for _, tool := range []string{"git", "kubectl", "jq", "curl"} {
		writeStub(t, bin, tool, "echo '"+tool+" stand-in'\n")
	}
	writeStub(t, bin, "kind", "echo 'kind v"+kindMinimum(t)+" go1.26.3 linux/amd64'\n")
	writeStub(t, bin, "docker", "[ \"$1\" = info ] && exit 1\necho 'docker stand-in'\n")
	for _, command := range []string{"bash", "grep", "head", "sed", "sort"} {
		path, err := exec.LookPath(command)
		if err != nil {
			t.Fatalf("finding %s: %v", command, err)
		}
		if err := os.Symlink(path, filepath.Join(bin, command)); err != nil {
			t.Fatalf("linking %s: %v", command, err)
		}
	}
	return bin
}

// runCheckTools runs check-tools from the repository root with bin as the
// whole path, and args after the target on the make command line, and returns
// what make printed and its exit code.
func runCheckTools(t *testing.T, bin string, args ...string) (string, int) {
	t.Helper()

	makefile, err := filepath.Abs(makefileFile)
	if err != nil {
		t.Fatalf("resolving %s: %v", makefileFile, err)
	}
	cmd := exec.Command("make", append([]string{"--silent", "--file", makefile, "check-tools"}, args...)...)
	cmd.Dir = filepath.Dir(makefile)
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return slices.Contains([]string{"MAKEFLAGS", "MAKELEVEL", "MFLAGS", "PATH"}, name)
	}), "PATH="+bin)
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatalf("running make check-tools: %v", err)
	return "", 0
}

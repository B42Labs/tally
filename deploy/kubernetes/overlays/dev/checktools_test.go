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

func TestCheckToolsPrintsWhatGoAnsweredOnItsOwnLine(t *testing.T) {
	raw, err := os.ReadFile(goModFile)
	if err != nil {
		t.Fatalf("reading %s: %v", goModFile, err)
	}
	goLine := goLineRe.FindSubmatch(raw)
	if goLine == nil {
		t.Fatalf("%s has no go line", goModFile)
	}
	goMin := string(goLine[1])
	toolchainLine := toolchainLineRe.FindSubmatch(raw)
	if toolchainLine == nil {
		t.Fatalf("%s has no toolchain line", goModFile)
	}
	toolchain := string(toolchainLine[1])

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

// toolStandIns returns a directory that holds a stand-in answering for every
// tool check-tools probes except Go, and links to the commands the Makefile
// runs on its own, so that directory alone can be the path. The docker
// stand-in fails `docker info`, which leaves out the line on the engine's size.
func toolStandIns(t *testing.T) string {
	t.Helper()

	bin := t.TempDir()
	for _, tool := range []string{"git", "kind", "kubectl", "jq", "curl"} {
		writeStub(t, bin, tool, "echo '"+tool+" stand-in'\n")
	}
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
// whole path, and returns what make printed and its exit code.
func runCheckTools(t *testing.T, bin string) (string, int) {
	t.Helper()

	makefile, err := filepath.Abs(makefileFile)
	if err != nil {
		t.Fatalf("resolving %s: %v", makefileFile, err)
	}
	cmd := exec.Command("make", "--silent", "--file", makefile, "check-tools")
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

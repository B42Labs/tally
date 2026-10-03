// This file runs kind_load, the macro `make up` puts every image onto the kind
// node with, against stand-ins for docker, kind and kubectl. Each mistake it
// looks for is quiet until a `make up` meets it. An archive of the engine's
// platform rather than the node's holds nothing the node runs once
// DOCKER_DEFAULT_PLATFORM made the two differ, a failed load that does not end
// the loop leaves the next image to decide the status of the whole step, and an
// archive left behind after a failure puts a full image into the temporary
// directory on every failed run.
package dev_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	makefileFile = "../../../../Makefile"

	// What the stand-in kubectl reports for the node, and what the stand-in
	// docker reports for the engine.
	nodePlatform   = "linux/amd64"
	enginePlatform = "linux/arm64"
)

func TestKindLoadMovesAnArchiveOfTheNodePlatform(t *testing.T) {
	// The probe loops over two images the way the loops of `up` do. A save that
	// fails is never loaded, and a load that fails ends the loop before the
	// second image, under make 3.81 as well, which runs the recipe without
	// errexit.
	cases := []struct {
		name                   string
		saveStatus, loadStatus int
		want                   []string
	}{
		{"every load succeeds", 0, 0, []string{
			"saved first:dev as " + nodePlatform, "loaded first:dev into tally",
			"saved second:dev as " + nodePlatform, "loaded second:dev into tally",
		}},
		{"the load fails", 0, 1, []string{"saved first:dev as " + nodePlatform, "loaded first:dev into tally"}},
		{"the save fails", 1, 0, []string{"saved first:dev as " + nodePlatform}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, tmp := t.TempDir(), t.TempDir()
			log := filepath.Join(t.TempDir(), "calls")
			writeStub(t, bin, "kubectl", "printf '%s' '"+nodePlatform+"'\n")
			writeStub(t, bin, "docker", fmt.Sprintf(`case "$1" in
version) echo '%s'; exit 0 ;;
save) shift ;;
*) exit 2 ;;
esac
while [ $# -gt 1 ]; do
	case "$1" in
	--platform) platform="$2"; shift 2 ;;
	-o) out="$2"; shift 2 ;;
	*) exit 2 ;;
	esac
done
echo "saved $1 as $platform" >> '%s'
[ %d -eq 0 ] || exit %[3]d
printf '%%s' "$1" > "$out"
`, enginePlatform, log, tc.saveStatus))
			writeStub(t, bin, "kind", fmt.Sprintf(`[ "$1 $2 $4" = 'load image-archive --name' ] || exit 2
echo "loaded $(cat "$3") into $5" >> '%s'
exit %d
`, log, tc.loadStatus))

			out, code := runKindLoadProbe(t, bin, tmp, "first:dev", "second:dev")

			if failed := code != 0; failed != (tc.saveStatus != 0 || tc.loadStatus != 0) {
				t.Errorf("the probe exited %d with the save exiting %d and the load %d:\n%s", code, tc.saveStatus, tc.loadStatus, out)
			}
			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatalf("reading what the stand-ins were called with: %v\n%s", err, out)
			}
			if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !slices.Equal(got, tc.want) {
				t.Errorf("the stand-ins were called as %q, want %q:\n%s", got, tc.want, out)
			}
			if left, err := os.ReadDir(tmp); err != nil || len(left) != 0 {
				t.Errorf("the temporary directory holds %v (error %v) after the probe, want nothing", left, err)
			}
		})
	}
}

// writeStub puts an executable shell script named name into dir.
func writeStub(t *testing.T, dir, name, script string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("writing the %s stand-in: %v", name, err)
	}
}

// runKindLoadProbe runs kind_load over images from the repository root, with
// bin first on the path and tmp as the temporary directory, and returns what
// make printed and its exit code.
func runKindLoadProbe(t *testing.T, bin, tmp string, images ...string) (string, int) {
	t.Helper()

	makefile, err := filepath.Abs(makefileFile)
	if err != nil {
		t.Fatalf("resolving %s: %v", makefileFile, err)
	}
	probe := filepath.Join(t.TempDir(), "probe.mk")
	recipe := "probe:\n\t@for image in " + strings.Join(images, " ") + "; do $(call kind_load,$$image) || exit 1; done\n"
	if err := os.WriteFile(probe, []byte(recipe), 0o600); err != nil {
		t.Fatalf("writing %s: %v", probe, err)
	}

	cmd := exec.Command("make", "--silent", "--file", makefile, "--file", probe, "probe", "CLUSTER_NAME=tally")
	cmd.Dir = filepath.Dir(makefile)
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return slices.Contains([]string{"MAKEFLAGS", "MAKELEVEL", "MFLAGS", "PATH", "TMPDIR"}, name)
	}), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "TMPDIR="+tmp)
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatalf("running the kind_load probe: %v", err)
	return "", 0
}

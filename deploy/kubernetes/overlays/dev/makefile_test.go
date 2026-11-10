// This file runs kind_load, the macro `make up` puts every image onto the kind
// node with, against stand-ins for docker, kind and kubectl. Each mistake it
// looks for is quiet until a `make up` meets it. An archive of the engine's
// platform rather than the node's holds nothing the node runs once
// DOCKER_DEFAULT_PLATFORM made the two differ, a failed load that does not end
// the loop leaves the next image to decide the status of the whole step, and an
// archive left behind after a failure puts a full image into the temporary
// directory on every failed run.
//
// It runs console_token, the macro `make up` issues the console's token with,
// against stand-ins for kubectl and go the same way. A token issued on every
// run leaves one api_tokens row per `make up`, a failed read of the Secret
// taken for a missing one issues a second token against an unreachable
// cluster, and a Secret created without the restart leaves a pod that started
// before it on the token it read then.
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

	// The token the stand-in for go prints, and the calls console_token makes,
	// as the stand-ins log them.
	consoleToken        = "tally_pat_console-probe-token"
	getConsoleSecret    = "kubectl --context kind-tally -n tally get secret tally-console-token --ignore-not-found -o name"
	issueConsoleToken   = "go run ./cmd/tally-reporting-admin create-api-token --role read_all --description in-cluster console"
	createConsoleSecret = "kubectl --context kind-tally -n tally create secret generic tally-console-token --from-file=api-token=/dev/stdin"
	restartConsole      = "kubectl --context kind-tally -n tally rollout restart deployment/console"
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

func TestConsoleTokenIsIssuedOnceIntoTheSecret(t *testing.T) {
	// Each case sets what the stand-ins answer and wants the calls they were
	// made with, in order. A step that fails ends the macro before the next
	// one, under make 3.81 as well, which runs the recipe without errexit.
	cases := []struct {
		name         string
		secretExists bool
		getStatus    int
		goMode       string // token, empty or fail
		createStatus int
		want         []string
		wantOutput   string
		wantFailure  bool
	}{
		{
			name: "the Secret exists", secretExists: true, goMode: "token",
			want:       []string{getConsoleSecret},
			wantOutput: "the console token is in tally-console-token already",
		},
		{
			name: "the Secret is absent", goMode: "token",
			want:       []string{getConsoleSecret, issueConsoleToken, createConsoleSecret, restartConsole},
			wantOutput: "issuing a read_all api token for the console",
		},
		{
			name: "create-api-token fails", goMode: "fail",
			want:        []string{getConsoleSecret, issueConsoleToken},
			wantFailure: true,
		},
		{
			name: "create-api-token prints nothing", goMode: "empty",
			want:        []string{getConsoleSecret, issueConsoleToken},
			wantOutput:  "printed no token",
			wantFailure: true,
		},
		{
			name: "the Secret cannot be read", getStatus: 1, goMode: "token",
			want:        []string{getConsoleSecret},
			wantFailure: true,
		},
		{
			name: "the Secret cannot be created", goMode: "token", createStatus: 1,
			want:        []string{getConsoleSecret, issueConsoleToken, createConsoleSecret},
			wantFailure: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, tmp := t.TempDir(), t.TempDir()
			dir := t.TempDir()
			log, stdin := filepath.Join(dir, "calls"), filepath.Join(dir, "stdin")

			found := ""
			if tc.secretExists {
				found = "secret/tally-console-token"
			}
			writeStub(t, bin, "kubectl", fmt.Sprintf(`echo "kubectl $*" >> '%s'
case " $* " in
*" get secret "*) [ -z '%s' ] || echo '%[2]s'; exit %d ;;
*" create secret "*) cat > '%s'; exit %d ;;
esac
exit 0
`, log, found, tc.getStatus, stdin, tc.createStatus))
			writeStub(t, bin, "go", fmt.Sprintf(`echo "go $*" >> '%s'
case '%s' in
token) echo '%s'; echo 'created api_tokens 0b5c1a2e-0000-4000-8000-000000000001' >&2 ;;
fail) echo 'error: connecting to the database' >&2; exit 1 ;;
esac
exit 0
`, log, tc.goMode, consoleToken))

			out, code := runProbe(t, bin, tmp, "probe:\n\t@$(call console_token)\n")

			if failed := code != 0; failed != tc.wantFailure {
				t.Errorf("the probe exited %d, want a failure: %t:\n%s", code, tc.wantFailure, out)
			}
			if tc.wantOutput != "" && !strings.Contains(out, tc.wantOutput) {
				t.Errorf("the probe printed no %q:\n%s", tc.wantOutput, out)
			}
			raw, err := os.ReadFile(log)
			if err != nil {
				t.Fatalf("reading what the stand-ins were called with: %v\n%s", err, out)
			}
			if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); !slices.Equal(got, tc.want) {
				t.Errorf("the stand-ins were called as %q, want %q:\n%s", got, tc.want, out)
			}

			// The token reaches the Secret through stdin, without the newline
			// the admin CLI ends it with, which the console would read as part
			// of the token.
			if slices.Contains(tc.want, createConsoleSecret) {
				got, err := os.ReadFile(stdin)
				if err != nil {
					t.Fatalf("reading what the create read on stdin: %v", err)
				}
				if string(got) != consoleToken {
					t.Errorf("the create read %q on stdin, want %q", got, consoleToken)
				}
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

	recipe := "probe:\n\t@for image in " + strings.Join(images, " ") + "; do $(call kind_load,$$image) || exit 1; done\n"
	return runProbe(t, bin, tmp, recipe)
}

// runProbe runs the target probe of a makefile holding recipe beside the
// repository's Makefile, from the repository root, with bin first on the path
// and tmp as the temporary directory, and returns what make printed and its
// exit code.
func runProbe(t *testing.T, bin, tmp, recipe string) (string, int) {
	t.Helper()

	makefile, err := filepath.Abs(makefileFile)
	if err != nil {
		t.Fatalf("resolving %s: %v", makefileFile, err)
	}
	probe := filepath.Join(t.TempDir(), "probe.mk")
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
	t.Fatalf("running the probe: %v", err)
	return "", 0
}

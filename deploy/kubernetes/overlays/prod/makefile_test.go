// This file runs the prod targets of the Makefile up to the refusal of each
// guard, and every guard stands in front of a failure that is quiet otherwise.
// An empty secret value applies like any other, and Grafana comes up on its
// default admin password. A migration chain that does not match the image
// leaves the old pod Ready on a schema it does not know. A listener already on
// the forwarded port answers the probe, and the migration reaches whatever
// database is behind it. The targets run in a throwaway Git repository with an
// empty kubeconfig and a context nothing names, so a guard that lets one
// through fails at kubectl rather than at a cluster.
package prod_test

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	makefileFile = "../../../../Makefile"

	// The context the targets run with, which no kubeconfig names, and the
	// release the stand-in overlay deploys.
	noContext = "tally-test-no-such-context"
	release   = "v0.0.1"

	// What the targets print once every guard let them through.
	prodUpPassed      = "==> installing the certificate issuer"
	prodMigratePassed = "ERROR: the port-forward to TimescaleDB never answered"
)

// prodTargetRe matches the rule of a prod target and the first line of its
// recipe.
var prodTargetRe = regexp.MustCompile(`(?m)^(prod-[a-z-]+):.*\n(.*)$`)

// emptyValueRe and placeholderRe match what an example secret file leaves for
// the operator to fill in.
var (
	emptyValueRe  = regexp.MustCompile(`(?m)^([^#=\n]+=)[ \t]*$`)
	placeholderRe = regexp.MustCompile(`<[a-z-]+>`)
)

func TestEveryProdTargetStartsWithTheContextGuard(t *testing.T) {
	// helm takes an empty --kube-context for the current context, so the guard
	// is all that keeps prod-addons off whichever cluster that is. A prod
	// target added without the guard, or with a command above it, can act
	// there too.
	raw, err := os.ReadFile(makefileFile)
	if err != nil {
		t.Fatalf("reading %s: %v", makefileFile, err)
	}
	rules := prodTargetRe.FindAllStringSubmatch(string(raw), -1)
	if len(rules) == 0 {
		t.Fatalf("%s declares no prod target, so this test would assert over nothing", makefileFile)
	}
	for _, rule := range rules {
		if rule[2] != "\t$(call prod_context_guard)" {
			t.Errorf("the recipe of %s starts with %q, want the context guard", rule[1], rule[2])
		}
	}
}

func TestProdUpRefusesASecretThatIsNotFilledIn(t *testing.T) {
	// The Secret carries the key either way, so an empty value applies like any
	// other: Grafana keeps its default admin password when admin-password is
	// empty, and VictoriaMetrics serves delete_series to every pod when
	// delete-auth-key is. A value left on the example's placeholder applies the
	// same way. The content "" removes the file.
	cases := []struct {
		name, file, content, want string
	}{
		{"a missing file", "tally-grafana.env", "", "tally-grafana.env is missing"},
		{"an empty value", "tally-grafana.env", "admin-password=\n", "tally-grafana.env leaves admin-password empty or on a placeholder"},
		{"a value of blanks", "tally-vm-admin.env", "delete-auth-key=  \n", "tally-vm-admin.env leaves delete-auth-key empty or on a placeholder"},
		{
			"a placeholder", "tally-db.env",
			"password=0a1b\nengine-password=0a1b\ndb-url=postgres://tally:<password>@timescaledb:5432/tally_reporting?sslmode=disable\n",
			"tally-db.env leaves db-url empty or on a placeholder",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overlay := prodOverlay(t, release)
			file := filepath.Join(overlay, "secrets", tc.file)
			var err error
			if tc.content == "" {
				err = os.Remove(file)
			} else {
				err = os.WriteFile(file, []byte(tc.content), 0o600)
			}
			if err != nil {
				t.Fatalf("preparing %s: %v", file, err)
			}

			out, code := runMake(t, checkout(t, release), "prod-up", overlay)
			if code == 0 || !strings.Contains(out, tc.want) {
				t.Errorf("make prod-up exited %d, want a refusal carrying %q:\n%s", code, tc.want, out)
			}
			if strings.Contains(out, prodUpPassed) {
				t.Errorf("make prod-up went on to the cluster after the refusal:\n%s", out)
			}
		})
	}

	t.Run("every value filled in", func(t *testing.T) {
		if out, _ := runMake(t, checkout(t, release), "prod-up", prodOverlay(t, release)); !strings.Contains(out, prodUpPassed) {
			t.Errorf("make prod-up did not get past its checks to %q:\n%s", prodUpPassed, out)
		}
	})
}

func TestProdTargetsRefuseACheckoutThatIsNotTheDeployedRelease(t *testing.T) {
	// go run migrates with the chain of the checkout, and the Reporting API's
	// readiness refuses only a schema behind its build. A chain ahead of the
	// image leaves the old pod Ready on a schema it does not know, and one
	// behind it leaves the new pod unready with nothing naming why.
	passed := map[string]string{"prod-up": prodUpPassed, "prod-migrate": prodMigratePassed}

	// go:embed takes every .sql file in the directory whatever Git makes of it,
	// so a migration git status leaves out is refused all the same: one an
	// excludes file ignores, and one status.showUntrackedFiles=no hides.
	excludes := filepath.Join(t.TempDir(), "excludes")
	if err := os.WriteFile(excludes, []byte("*.sql\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hidden := map[string][]string{
		"":                         nil,
		", ignored":                {"core.excludesFile", excludes},
		", hidden from git status": {"status.showUntrackedFiles", "no"},
	}

	for target, next := range passed {
		t.Run(target+" at another tag", func(t *testing.T) {
			out, code := runMake(t, checkout(t, "v0.0.2"), target, prodOverlay(t, release), freePort(t))
			want := "ERROR: the prod overlay deploys " + release + ", but the checkout is at v0.0.2"
			if code == 0 || !strings.Contains(out, want) {
				t.Errorf("make %s exited %d, want a refusal carrying %q:\n%s", target, code, want, out)
			}
		})

		for how, config := range hidden {
			t.Run(target+" with a migration the tag does not carry"+how, func(t *testing.T) {
				dir := checkout(t, release)
				if config != nil {
					cmd := exec.Command("git", append([]string{"config"}, config...)...)
					cmd.Dir = dir
					cmd.Env = testEnv()
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git config %s: %v\n%s", strings.Join(config, " "), err, out)
					}
				}
				migrations := filepath.Join(dir, "migrations", "reporting")
				if err := os.MkdirAll(migrations, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(migrations, "0099_drop.sql"), []byte("-- +goose Up\n"), 0o644); err != nil {
					t.Fatal(err)
				}

				out, code := runMake(t, dir, target, prodOverlay(t, release), freePort(t))
				if want := "ERROR: migrations/reporting differs from the tag"; code == 0 || !strings.Contains(out, want) {
					t.Errorf("make %s exited %d, want a refusal carrying %q:\n%s", target, code, want, out)
				}
			})
		}

		t.Run(target+" at the tag", func(t *testing.T) {
			if out, _ := runMake(t, checkout(t, release), target, prodOverlay(t, release), freePort(t)); !strings.Contains(out, next) {
				t.Errorf("make %s did not get past its checks to %q:\n%s", target, next, out)
			}
		})
	}
}

func TestProdMigrateRefusesAPortSomethingListensOn(t *testing.T) {
	// A port-forward left running from the credential step, or any other
	// listener, answers the probe as if it were the forward, and the migration
	// runs against whatever database is behind it.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening on a port: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("closing the listener: %v", err)
		}
	})
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)

	out, code := runMake(t, checkout(t, release), "prod-migrate", prodOverlay(t, release), "PROD_DB_PORT="+port)
	if want := "ERROR: something already listens on 127.0.0.1:" + port; code == 0 || !strings.Contains(out, want) {
		t.Errorf("make prod-migrate exited %d, want a refusal carrying %q:\n%s", code, want, out)
	}
}

func TestProdMigrateSaysWhyThePortForwardFailed(t *testing.T) {
	// A forward fails at once for a context kubectl does not know, a Service
	// that is not there or a port-forward RBAC denies, and what kubectl said is
	// the only account of which it was.
	out, code := runMake(t, checkout(t, release), "prod-migrate", prodOverlay(t, release), freePort(t))
	_, said, found := strings.Cut(out, "kubectl said:\n")
	first, _, _ := strings.Cut(said, "\n")
	if code == 0 || !found || first == "" || strings.HasPrefix(first, "make: ") {
		t.Errorf("make prod-migrate exited %d, want the failure of the forward followed by what kubectl said:\n%s", code, out)
	}
}

// runMake runs one target of the Makefile in dir against overlay, and returns
// what it printed and the status it exited with.
func runMake(t *testing.T, dir, target, overlay string, vars ...string) (string, int) {
	t.Helper()

	makefile, err := filepath.Abs(makefileFile)
	if err != nil {
		t.Fatalf("resolving %s: %v", makefileFile, err)
	}
	args := append([]string{"--silent", "--file", makefile, target, "PROD_CONTEXT=" + noContext, "PROD_OVERLAY=" + overlay}, vars...)
	cmd := exec.Command("make", args...)
	cmd.Dir = dir
	cmd.Env = testEnv()
	out, err := cmd.CombinedOutput()

	var exit *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &exit):
		return string(out), exit.ExitCode()
	}
	t.Fatalf("running make %s: %v", target, err)
	return "", 0
}

// testEnv is the environment of every process these tests start: the test's
// own, less what a calling make or Git hook hands down, with a kubeconfig that
// names no cluster and no Git configuration outside the repository.
func testEnv() []string {
	env := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return slices.Contains([]string{"MAKEFLAGS", "MAKELEVEL", "MFLAGS", "KUBECONFIG", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"}, name)
	})
	return append(env, "KUBECONFIG="+os.DevNull, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
}

// checkout returns a Git repository whose one commit carries tag. The targets
// run in it, so the release guard reads its tags rather than those of the
// repository under test.
func checkout(t *testing.T, tag string) string {
	t.Helper()

	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"-c", "user.name=tally", "-c", "user.email=tally@example.com", "commit", "--quiet", "--allow-empty", "--message", "release"},
		{"tag", tag},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = testEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	return dir
}

// prodOverlay returns a directory standing in for the prod overlay: a
// kustomization.yaml deploying tag, and every example secret file of the real
// overlay beside a copy with each empty value and placeholder filled in.
func prodOverlay(t *testing.T, tag string) string {
	t.Helper()

	examples, err := filepath.Glob("secrets/*.env.example")
	if err != nil || len(examples) == 0 {
		t.Fatalf("finding the example secret files: %d found, error %v", len(examples), err)
	}
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		kustomizationFile: "images:\n  - name: " + reportingImage + "\n    newTag: " + tag + "\n",
	}
	for _, example := range examples {
		raw, err := os.ReadFile(example)
		if err != nil {
			t.Fatal(err)
		}
		files[example] = string(raw)
		filled := emptyValueRe.ReplaceAllString(string(raw), "${1}0a1b")
		files[strings.TrimSuffix(example, ".example")] = placeholderRe.ReplaceAllString(filled, "0a1b")
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// freePort returns the assignment of a port nothing listens on to PROD_DB_PORT.
func freePort(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("closing the listener on port %d: %v", port, err)
	}
	return "PROD_DB_PORT=" + strconv.Itoa(port)
}

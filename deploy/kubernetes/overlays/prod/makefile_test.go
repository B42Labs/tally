// This file runs the prod targets of the Makefile up to the refusal of each
// guard, and every guard stands in front of a failure that is quiet otherwise.
// An empty secret value applies like any other, and Grafana comes up on its
// default admin password. A secret file without a key of its example applies
// too, and the pod that mounts the Secret does not start. An empty cloud
// applies, and the collector exits in a pod no step of the target waits on. A
// clouds config that names another cloud than the collector applies as well,
// and every sync is answered 404. Two images at two tags leave one of them
// under the manifests of another release. The targets run in a throwaway Git
// repository with an empty kubeconfig and a context nothing names, so a guard
// that lets one through fails at kubectl rather than at a cluster.
//
// What prod-up does with the migration Job runs against a stand-in for
// kubectl: a shell script that logs every call and answers a get of the Job
// with a state the test chooses, one before the apply and one for each poll
// after it. A Job that has not finished and is deleted anyway is a migration
// killed in the middle, which can leave a half-built index, and a failed Job
// that is not read is a Reporting API that stays unready with nothing naming
// why.
package prod_test

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

const (
	makefileFile = "../../../../Makefile"

	// The context the targets run with, which no kubeconfig names, and the
	// release the stand-in overlay deploys.
	noContext = "tally-test-no-such-context"
	release   = "v0.0.1"

	// What prod-up prints once every guard let it through.
	prodUpPassed = "==> installing the certificate issuer"

	// The states of the migration Job the stand-in for kubectl answers with,
	// as the get prod-up runs prints them: nothing for no Job, the name alone
	// for one that has not finished, and the types of its true conditions
	// beside it for one that has.
	jobAbsent   = ""
	jobRunning  = "tally-migrate:"
	jobComplete = "tally-migrate:SuccessCriteriaMet Complete"
	jobFailed   = "tally-migrate:FailureTarget Failed"

	// The calls prod-up makes, as the stand-in logs them, that the tests look
	// for and order.
	deleteJobArg = "delete job tally-migrate --ignore-not-found"
	applyArg     = "apply -k "
	jobLogsArg   = "logs -l batch.kubernetes.io/job-name=tally-migrate"
	reportingArg = "rollout status deployment/reporting-api"
)

// kubectlStub stands in for kubectl. It logs its arguments to the file log
// beside it and answers a get of a Job: with the content of before until it
// has logged an apply -k, and from then on with line n of after for the nth
// get, or with its last line once n is past it. A get fails while a file fail
// lies beside it, and so does a get past the 20th after the apply, which ends
// a wait that would otherwise poll forever. Every other call prints nothing
// and exits 0.
const kubectlStub = `#!/bin/sh
dir="$(dirname "$0")"
echo "$*" >> "$dir/log"
case " $* " in
*" get job "*)
	[ ! -f "$dir/fail" ] || exit 1
	if grep -q '^apply -k ' "$dir/log"; then
		n=$(sed -n '/^apply -k /,$p' "$dir/log" | grep -c ' get job ')
		[ "$n" -le 20 ] || exit 1
		awk -v n="$n" 'NR <= n { state = $0 } END { print state }' "$dir/after"
	else
		cat "$dir/before"
	fi
	;;
esac
exit 0
`

// sleepStub stands in for sleep and returns at once, so a test of the wait
// polls without waiting between the polls.
const sleepStub = "#!/bin/sh\nexit 0\n"

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
			"password=0a1b\nengine-password=0a1b\n" +
				"db-url=postgres://tally:<password>@timescaledb:5432/tally_reporting?sslmode=disable\n" +
				"engine-db-url=postgres://tally:0a1b@timescaledb:5432/tally_engine?sslmode=disable\n" +
				"engine-reporting-db-url=postgres://tally_engine:0a1b@timescaledb:5432/tally_reporting?sslmode=disable\n",
			"tally-db.env leaves db-url empty or on a placeholder",
		},
		// The file of a deployment from before the scheduler ran here carries
		// three keys, and the two engine keys are what the upgrade adds.
		{
			"three of five keys", "tally-db.env",
			"password=0a1b\nengine-password=0a1b\ndb-url=postgres://tally:0a1b@timescaledb:5432/tally_reporting?sslmode=disable\n",
			"tally-db.env lacks engine-db-url engine-reporting-db-url; copy the lines from",
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

func TestProdUpRefusesACollectorWithoutACloud(t *testing.T) {
	// The ConfigMap carries the key either way, so an empty cloud applies like
	// any other. The collector then exits with "TALLY_OSC_CLOUD: must be set"
	// in a pod make prod-up does not wait on, and nothing in the run says so.
	// The content "" removes the file.
	cases := []struct {
		name, content, want string
	}{
		{"a missing file", "", "collector.env is missing"},
		{"an empty value", "TALLY_OSC_CLOUD=\n", "collector.env leaves TALLY_OSC_CLOUD empty"},
		{"a value of blanks", "TALLY_OSC_CLOUD=  \n", "collector.env leaves TALLY_OSC_CLOUD empty"},
		{"a commented-out line", "# TALLY_OSC_CLOUD=os-test\n", "collector.env leaves TALLY_OSC_CLOUD empty"},
		{"no line for the cloud", "TALLY_OSC_EXCHANGES=nova\n", "collector.env leaves TALLY_OSC_CLOUD empty"},
		// kustomize keeps the blanks around a value, and the clouds config,
		// which is YAML, drops them, so the sync asks for a cloud the Reporting
		// API does not reconcile.
		{"a blank after the cloud", "TALLY_OSC_CLOUD=os-test \n", "collector.env leaves TALLY_OSC_CLOUD empty or with whitespace around it"},
		{"a blank before the cloud", "TALLY_OSC_CLOUD= os-test\n", "collector.env leaves TALLY_OSC_CLOUD empty or with whitespace around it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overlay := prodOverlay(t, release)
			file := filepath.Join(overlay, collectorSettingsFile)
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
}

func TestProdUpRefusesAReconciliationThatIsNotConfigured(t *testing.T) {
	// The Secret and the ConfigMap render from whatever the files hold. A
	// clouds.yaml left on its placeholders fails every sync with a 500, a
	// clouds config that names another cloud than collector.env runs a sync
	// that is answered 404 every ten minutes, and the shipped config with its
	// empty names ends the Reporting API at startup. make prod-up reads the
	// cloud line as text, so a quoted name is refused as well. The content ""
	// removes the file.
	const auth, config = "secrets/clouds.yaml", "reconciliation/clouds-config.yaml"

	cases := []struct {
		name, file, content, want string
	}{
		{"a missing clouds.yaml", auth, "", auth + " is missing; copy "},
		{
			"a clouds.yaml on a placeholder", auth,
			"clouds:\n  os-test:\n    auth_type: v3applicationcredential\n    auth:\n      auth_url: <auth-url>\n",
			auth + " still carries a placeholder",
		},
		{"a missing clouds config", config, "", config + " does not name the cloud os-test of collector.env"},
		{"the shipped clouds config", config, shippedCloudsConfig(t), config + " does not name the cloud os-test of collector.env"},
		{"another cloud", config, cloudsConfig(t, "os-other", "os-test"), config + " does not name the cloud os-test of collector.env"},
		{"a quoted cloud", config, cloudsConfig(t, `"os-test"`, "os-test"), config + " does not name the cloud os-test of collector.env"},
		{"an empty os_cloud", config, cloudsConfig(t, "os-test", ""), config + " leaves os_cloud empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overlay := prodOverlay(t, release)
			file := filepath.Join(overlay, tc.file)
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
}

func TestProdUpRefusesACheckoutThatIsNotTheDeployedRelease(t *testing.T) {
	// The checkout supplies the manifests the images run under, and the
	// credential and catalog steps of the how-to run the CLIs of the checkout
	// against the schema of the image.
	t.Run("at another tag", func(t *testing.T) {
		out, code := runMake(t, checkout(t, "v0.0.2"), "prod-up", prodOverlay(t, release))
		want := "ERROR: the prod overlay deploys " + release + ", but the checkout is at v0.0.2; check out " + release + ", so the manifests match the images"
		if code == 0 || !strings.Contains(out, want) {
			t.Errorf("make prod-up exited %d, want a refusal carrying %q:\n%s", code, want, out)
		}
	})

	// The overlay deploys one release. With two tags the checkout can be at
	// one of them only, and the image at the other runs under manifests that
	// are not its own.
	t.Run("with two images at two tags", func(t *testing.T) {
		overlay := prodOverlay(t, release)
		file := filepath.Join(overlay, kustomizationFile)
		if err := os.WriteFile(file, []byte(imagesAt(release, "v0.0.2")), 0o600); err != nil {
			t.Fatalf("preparing %s: %v", file, err)
		}

		out, code := runMake(t, checkout(t, release), "prod-up", overlay)
		want := "ERROR: the prod overlay names more than one tag (" + release + " v0.0.2)"
		if code == 0 || !strings.Contains(out, want) {
			t.Errorf("make prod-up exited %d, want a refusal carrying %q:\n%s", code, want, out)
		}
		if strings.Contains(out, prodUpPassed) {
			t.Errorf("make prod-up went on after the refusal:\n%s", out)
		}
	})

	t.Run("at the tag", func(t *testing.T) {
		if out, _ := runMake(t, checkout(t, release), "prod-up", prodOverlay(t, release)); !strings.Contains(out, prodUpPassed) {
			t.Errorf("make prod-up did not get past its checks to %q:\n%s", prodUpPassed, out)
		}
	})
}

func TestProdUpLeavesAMigrationThatHasNotFinished(t *testing.T) {
	// A Job's pod template is immutable, so prod-up deletes the Job of the
	// previous run before it applies. Deleting one that is still running kills
	// its migration, and migration 9 of the reporting chain builds an index
	// outside a transaction that a kill leaves half built.
	stub := newKubectlStub(t, jobRunning, jobComplete)

	out, code := runMake(t, checkout(t, release), "prod-up", prodOverlay(t, release), "PROD_KUBECTL="+stub)
	if want := "the migration Job tally-migrate of an earlier run has not finished"; code == 0 || !strings.Contains(out, want) {
		t.Errorf("make prod-up exited %d, want a refusal carrying %q:\n%s", code, want, out)
	}
	calls := stubCalls(t, stub)
	for _, arg := range []string{deleteJobArg, applyArg} {
		if i := callIndex(calls, arg); i >= 0 {
			t.Errorf("make prod-up called kubectl %s with a Job that has not finished; its calls were:\n%s", calls[i], strings.Join(calls, "\n"))
		}
	}
}

func TestProdUpReplacesTheMigrationJobBeforeItApplies(t *testing.T) {
	// No Job, a completed one and a failed one are each deleted before the
	// apply, which would otherwise fail on the immutable pod template of the
	// old Job. A failed Job of an earlier run is what an operator reruns
	// prod-up on. The wait for the new Job comes before the wait for the
	// Reporting API, whose readiness needs the migrated schema, and the Job's
	// log is printed once it has completed. The exit status is not asserted:
	// the stand-in overlay has no hosts.yaml for the lines after the waits.
	for name, before := range map[string]string{"no Job": jobAbsent, "a completed Job": jobComplete, "a failed Job": jobFailed} {
		t.Run(name, func(t *testing.T) {
			stub := newKubectlStub(t, before, jobComplete)

			out, _ := runMake(t, checkout(t, release), "prod-up", prodOverlay(t, release), "PROD_KUBECTL="+stub)
			calls := stubCalls(t, stub)
			order := []int{callIndex(calls, deleteJobArg), callIndex(calls, applyArg), callIndex(calls, jobLogsArg), callIndex(calls, reportingArg)}
			if slices.Contains(order, -1) || !slices.IsSorted(order) {
				t.Errorf("make prod-up called kubectl %s, then apply -k, then %s, then %s at the calls %v of:\n%s\nit printed:\n%s",
					deleteJobArg, jobLogsArg, reportingArg, order, strings.Join(calls, "\n"), out)
			}
		})
	}
}

func TestProdUpStopsOnAFailedMigration(t *testing.T) {
	// The Reporting API answers its readiness probe with 503 until the schema
	// is at the version its build expects, so waiting for it after a failed
	// migration spends the whole wait on a pod that cannot become ready. The
	// Job's log is what says why it failed.
	stub := newKubectlStub(t, jobAbsent, jobFailed)

	out, code := runMake(t, checkout(t, release), "prod-up", prodOverlay(t, release), "PROD_KUBECTL="+stub)
	if want := "the migration Job tally-migrate failed; its log is above"; code == 0 || !strings.Contains(out, want) {
		t.Errorf("make prod-up exited %d, want a failure carrying %q:\n%s", code, want, out)
	}
	calls := stubCalls(t, stub)
	if callIndex(calls, jobLogsArg) < 0 {
		t.Errorf("make prod-up did not print the log of the failed Job; its calls were:\n%s", strings.Join(calls, "\n"))
	}
	if callIndex(calls, reportingArg) >= 0 {
		t.Errorf("make prod-up waited for the Reporting API after a failed migration; its calls were:\n%s", strings.Join(calls, "\n"))
	}
}

func TestProdUpWaitsForAMigrationThatFinishesLater(t *testing.T) {
	// On a cluster the new Job is still running when prod-up first reads it,
	// so the wait polls until it has completed, and only then prints its log
	// and waits for the Reporting API. The exit status is not asserted: the
	// stand-in overlay has no hosts.yaml for the lines after the waits.
	stub := newKubectlStub(t, jobAbsent, jobRunning, jobRunning, jobComplete)

	out, _ := runMake(t, checkout(t, release), "prod-up", prodOverlay(t, release), "PROD_KUBECTL="+stub, noSleep(stub))
	calls := stubCalls(t, stub)
	order := []int{callIndex(calls, applyArg), callIndex(calls, jobLogsArg), callIndex(calls, reportingArg)}
	if slices.Contains(order, -1) || !slices.IsSorted(order) {
		t.Errorf("make prod-up called kubectl apply -k, then %s, then %s at the calls %v of:\n%s\nit printed:\n%s",
			jobLogsArg, reportingArg, order, strings.Join(calls, "\n"), out)
	}
}

func TestProdUpStopsWhenItCannotReadTheMigrationJob(t *testing.T) {
	// A read that fails prints nothing, which is also what kubectl prints for
	// no Job. Taken for one, it deletes a Job whose migration may still be
	// running.
	stub := newKubectlStub(t, jobRunning, jobComplete)
	if err := os.WriteFile(filepath.Join(filepath.Dir(stub), "fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, code := runMake(t, checkout(t, release), "prod-up", prodOverlay(t, release), "PROD_KUBECTL="+stub)
	calls := stubCalls(t, stub)
	if code == 0 || callIndex(calls, deleteJobArg) >= 0 || callIndex(calls, applyArg) >= 0 {
		t.Errorf("make prod-up exited %d and went on after a failed read of the Job; its calls were:\n%s", code, strings.Join(calls, "\n"))
	}
}

func TestProdUpGivesUpOnAMigrationThatDoesNotFinish(t *testing.T) {
	// The wait has an end, and the Job is left running at it: deleting it
	// would kill the migration. A Job that never finishes is replaced by no
	// later step either.
	stub := newKubectlStub(t, jobAbsent, jobRunning)

	out, code := runMake(t, checkout(t, release), "prod-up", prodOverlay(t, release), "PROD_KUBECTL="+stub, "PROD_MIGRATE_WAIT_S=5", noSleep(stub))
	if want := "the migration Job tally-migrate did not finish in 5s"; code == 0 || !strings.Contains(out, want) {
		t.Errorf("make prod-up exited %d, want a failure carrying %q:\n%s", code, want, out)
	}
	calls := stubCalls(t, stub)
	var deletes int
	for _, call := range calls {
		if strings.Contains(call, "delete job") {
			deletes++
		}
	}
	if deletes != 1 {
		t.Errorf("make prod-up deleted a Job %d times, want once, before the apply; its calls were:\n%s", deletes, strings.Join(calls, "\n"))
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

// imagesAt returns a kustomization.yaml that deploys the Reporting API at one
// tag and the collector at another.
func imagesAt(reportingTag, collectorTag string) string {
	return "images:\n" +
		"  - name: " + reportingImage + "\n    newTag: " + reportingTag + "\n" +
		"  - name: " + collectorImage + "\n    newTag: " + collectorTag + "\n"
}

// prodOverlay returns a directory standing in for the prod overlay: a
// kustomization.yaml deploying both images at tag, a collector.env that names
// a cloud, a clouds config that names it too, and every example secret file of
// the real overlay beside a copy with each empty value and placeholder filled
// in.
func prodOverlay(t *testing.T, tag string) string {
	t.Helper()

	examples, err := filepath.Glob("secrets/*.env.example")
	if err != nil || len(examples) == 0 {
		t.Fatalf("finding the example secret files: %d found, error %v", len(examples), err)
	}
	examples = append(examples, cloudsSecretFile+".example")
	dir := t.TempDir()
	for _, sub := range []string{"secrets", filepath.Dir(cloudsConfigFile)} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		kustomizationFile:     imagesAt(tag, tag),
		collectorSettingsFile: "TALLY_OSC_CLOUD=os-test\n",
		cloudsConfigFile:      cloudsConfig(t, "os-test", "os-test"),
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

// shippedCloudsConfig returns the clouds config of the real overlay.
func shippedCloudsConfig(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(cloudsConfigFile)
	if err != nil {
		t.Fatalf("reading %s: %v", cloudsConfigFile, err)
	}
	return string(raw)
}

// cloudsConfig returns the clouds config of the real overlay with its cloud
// and os_cloud lines set to the two values.
func cloudsConfig(t *testing.T, cloud, osCloud string) string {
	t.Helper()

	lines := strings.Split(shippedCloudsConfig(t), "\n")
	for line, value := range map[string]string{"  - cloud:": cloud, "      os_cloud:": osCloud} {
		i := slices.Index(lines, line)
		if i < 0 {
			t.Fatalf("%s carries no line %q to fill", cloudsConfigFile, line)
		}
		lines[i] = strings.TrimRight(line+" "+value, " ")
	}
	return strings.Join(lines, "\n")
}

// newKubectlStub writes the stand-in for kubectl into a directory of its own,
// with the state it answers a get of the Job with before the apply and the one
// for each poll after it, and returns its path. The stand-in for sleep goes
// beside it.
func newKubectlStub(t *testing.T, before string, after ...string) string {
	t.Helper()

	dir := t.TempDir()
	stub := filepath.Join(dir, "kubectl")
	for name, content := range map[string]string{"before": before, "after": strings.Join(after, "\n") + "\n", "log": ""} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{"kubectl": kubectlStub, "sleep": sleepStub} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return stub
}

// noSleep returns the make variable that puts the directory of the stand-in
// for kubectl first on PATH, so the recipe runs the stand-in for sleep beside
// it.
func noSleep(stub string) string {
	return "PATH=" + filepath.Dir(stub) + string(os.PathListSeparator) + os.Getenv("PATH")
}

// stubCalls returns the arguments of every call the stand-in for kubectl
// logged, one call per entry, in order.
func stubCalls(t *testing.T, stub string) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(filepath.Dir(stub), "log"))
	if err != nil {
		t.Fatalf("reading the calls of the stand-in for kubectl: %v", err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

// callIndex returns the index of the first call that carries arg, or -1.
func callIndex(calls []string, arg string) int {
	return slices.IndexFunc(calls, func(call string) bool { return strings.Contains(call, arg) })
}

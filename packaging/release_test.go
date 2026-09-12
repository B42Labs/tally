// This file pins the release to the tag that triggers it, and the workflow to
// what the Makefile builds. Nothing else covers either: no pull request runs
// the release workflow, so a target renamed out from under it, a permission
// dropped, or an artifact attached without being signed would surface on the
// one run that publishes, after a tag was pushed and cannot be pushed again.
// The test runs the script the way the workflow does, through sh, and reads
// the workflow as a file; it needs neither dpkg nor a runner.
package packaging_test

import (
	"errors"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	// The script the release workflow reads the package version from. It is run
	// through sh, so the mode the file carries in the repository never matters.
	// This test runs beside it; the workflow runs from the repository root, so
	// it names the second path.
	releaseVersionScript   = "release-version.sh"
	releaseVersionFromRoot = "packaging/" + releaseVersionScript

	// The workflow that publishes a release, read from here.
	releaseWorkflowPath = "../.github/workflows/release.yaml"

	// The tag pattern that triggers it, and the job that does the work.
	releaseTagPattern = "v*"
	releaseJob        = "release"

	// The bundle the attestation is written to. It is the one file a release
	// carries that is not among the attestation's subjects, because an
	// attestation cannot be a subject of itself.
	attestationBundle = "dist/attestation.sigstore.json"
)

// acceptedTags are the tags the script maps rather than refuses, with the
// version each one puts into the package. A prerelease arrives with a tilde:
// dpkg reads everything after a hyphen as the package revision and sorts
// 1.2.3-rc.1 above 1.2.3, while a tilde sorts below every other character.
var acceptedTags = []struct{ tag, want string }{
	{"v0.0.0", "0.0.0"},
	{"v0.1.0", "0.1.0"},
	{"v1.2.3", "1.2.3"},
	{"v1.2.3-rc.1", "1.2.3~rc.1"},
	{"v10.20.30-beta.2", "10.20.30~beta.2"},
}

func TestReleaseVersionMapsATagToAPackageVersion(t *testing.T) {
	for _, tc := range acceptedTags {
		t.Run(tc.tag, func(t *testing.T) {
			stdout, stderr, code := releaseVersion(t, tc.tag)

			if code != 0 {
				t.Fatalf("%s %s exited %d, want 0: %s", releaseVersionScript, tc.tag, code, stderr)
			}
			if got := strings.TrimSuffix(stdout, "\n"); got != tc.want {
				t.Errorf("%s %s printed %q, want %q", releaseVersionScript, tc.tag, got, tc.want)
			}
			if stderr != "" {
				t.Errorf("%s %s wrote %q to stderr, want nothing", releaseVersionScript, tc.tag, stderr)
			}
		})
	}
}

func TestReleaseVersionRefusesWhatIsNotAReleaseTag(t *testing.T) {
	// Each of these would otherwise reach dpkg as a version: a tag with no
	// leading v, one that is not three numbers, a leading zero, an empty or
	// hyphenated prerelease part, build metadata, a name, and the empty string
	// a caller passes when it read no tag at all.
	for _, tag := range []string{
		"1.2.3",
		"v1.2",
		"v1.2.3.4",
		"v01.2.3",
		"v1.2.3-",
		"v1.2.3+build.5",
		"v1.2.3-rc-1",
		"vlatest",
		"",
	} {
		t.Run(tag, func(t *testing.T) {
			stdout, stderr, code := releaseVersion(t, tag)

			if code != 1 {
				t.Errorf("%s %q exited %d, want 1", releaseVersionScript, tag, code)
			}
			// Nothing on stdout, so a caller that captures the output stamps an
			// empty version into the package rather than this message.
			if stdout != "" {
				t.Errorf("%s %q printed %q, want nothing", releaseVersionScript, tag, stdout)
			}
			if want := "'" + tag + "' is not a release tag"; !strings.Contains(stderr, want) {
				t.Errorf("%s %q wrote %q to stderr, which does not carry %q", releaseVersionScript, tag, stderr, want)
			}
		})
	}
}

func TestReleaseVersionRefusesAMissingArgument(t *testing.T) {
	// An absent argument is not an empty tag: a workflow step whose variable
	// went missing is a different fault from a tag that is malformed, and the
	// message says so.
	stdout, stderr, code := releaseVersion(t)

	if code != 1 {
		t.Errorf("%s with no argument exited %d, want 1", releaseVersionScript, code)
	}
	if stdout != "" {
		t.Errorf("%s with no argument printed %q, want nothing", releaseVersionScript, stdout)
	}
	if want := "usage: release-version.sh <tag>"; !strings.Contains(stderr, want) {
		t.Errorf("%s with no argument wrote %q to stderr, which does not carry %q", releaseVersionScript, stderr, want)
	}
}

func TestReleaseWorkflowTriggersOnAVersionTag(t *testing.T) {
	got := releaseWorkflow(t).On.Push.Tags

	if want := []string{releaseTagPattern}; !slices.Equal(got, want) {
		t.Fatalf("the workflow triggers on %v, want %v", got, want)
	}

	// A tag the script maps that the trigger does not match is a release
	// nobody gets, and the run that would have said so never starts.
	for _, tc := range acceptedTags {
		matched, err := path.Match(releaseTagPattern, tc.tag)
		if err != nil {
			t.Fatalf("matching %q against %q: %v", tc.tag, releaseTagPattern, err)
		}
		if !matched {
			t.Errorf("%s is a tag release-version.sh maps, but %q does not match it", tc.tag, releaseTagPattern)
		}
	}
}

func TestReleaseWorkflowHoldsThePermissionsTheAttestationNeeds(t *testing.T) {
	// Dropping one of these fails the one run that publishes, and only after
	// the package was built: contents creates the release, id-token mints the
	// OIDC token the Sigstore certificate is issued against, and attestations
	// stores the attestation on the repository.
	want := map[string]string{
		"contents":     "write",
		"id-token":     "write",
		"attestations": "write",
	}

	got := releaseJobOf(t).Permissions
	if len(got) != len(want) {
		t.Fatalf("the %s job holds %v, want exactly %v", releaseJob, got, want)
	}
	for name, access := range want {
		if got[name] != access {
			t.Errorf("the %s job holds %s: %q, want %q", releaseJob, name, got[name], access)
		}
	}
}

func TestReleaseWorkflowStampsTheTagIntoThePackage(t *testing.T) {
	// The mapping lives in the script and nowhere else, and the version it
	// produces reaches the package through the Makefile. A second copy in YAML
	// would drift from the one the tests above cover.
	if run := releaseStep(t, "version").Run; !strings.Contains(run, "sh "+releaseVersionFromRoot) {
		t.Errorf("the version step does not run %s:\n%s", releaseVersionFromRoot, run)
	}

	build := releaseStep(t, "build").Run
	for _, want := range []string{"make sbom", "DEB_VERSION="} {
		if !strings.Contains(build, want) {
			t.Errorf("the build step does not carry %q:\n%s", want, build)
		}
	}
	if target := "\nsbom:"; !strings.Contains(read(t, makefilePath), target) {
		t.Errorf("the Makefile has no %q target, which the build step calls", strings.TrimSpace(target))
	}
}

func TestReleaseWorkflowAttestsEveryFileItPublishes(t *testing.T) {
	// A file attached to a release without being a subject of the attestation
	// is a file an operator cannot check the provenance of, and nothing else
	// would report it.
	attest := releaseStep(t, "attest")
	if action := "actions/attest@"; !strings.HasPrefix(attest.Uses, action) {
		t.Fatalf("the attest step uses %q rather than %s, so its subjects are named but not signed", attest.Uses, action)
	}

	attested := distPaths(attest.With["subject-path"])
	published := distPaths(releaseStep(t, "publish").Run)

	for _, file := range published {
		if file == attestationBundle {
			continue
		}
		if !slices.Contains(attested, file) {
			t.Errorf("the release publishes %s, which the attest step does not name as a subject", file)
		}
	}
	for _, file := range attested {
		if !slices.Contains(published, file) {
			t.Errorf("the attest step signs %s, which the release does not publish", file)
		}
	}
	if !slices.Contains(published, attestationBundle) {
		t.Errorf("the release does not publish %s, so a host that cannot reach the attestations API has nothing to verify against", attestationBundle)
	}
}

// releaseWorkflow reads the release workflow. The `on` key survives yaml.v3 as
// the string it is written as, rather than being resolved to the boolean true.
func releaseWorkflow(t *testing.T) releaseSpec {
	t.Helper()

	var spec releaseSpec
	if err := yaml.Unmarshal([]byte(read(t, releaseWorkflowPath)), &spec); err != nil {
		t.Fatalf("parsing %s: %v", releaseWorkflowPath, err)
	}
	return spec
}

// releaseJobOf returns the job that builds and publishes the release.
func releaseJobOf(t *testing.T) releaseJobSpec {
	t.Helper()

	job, ok := releaseWorkflow(t).Jobs[releaseJob]
	if !ok {
		t.Fatalf("%s carries no %s job", releaseWorkflowPath, releaseJob)
	}
	return job
}

// releaseStep returns one named step of that job. The steps are addressed by
// name, so a renamed step fails here rather than silently dropping a check.
func releaseStep(t *testing.T, name string) releaseStepSpec {
	t.Helper()

	for _, step := range releaseJobOf(t).Steps {
		if step.Name == name {
			return step
		}
	}
	t.Fatalf("the %s job carries no step named %q", releaseJob, name)
	return releaseStepSpec{}
}

// distPathRe matches a path into dist/ as a workflow writes one: a step's
// subject list or the argument list of gh release create.
var distPathRe = regexp.MustCompile(`dist/[A-Za-z0-9_.*-]+`)

// distPaths returns the dist/ paths a step names, deduplicated and sorted, so
// two lists written in different orders compare equal.
func distPaths(step string) []string {
	found := distPathRe.FindAllString(step, -1)
	slices.Sort(found)
	return slices.Compact(found)
}

// releaseSpec is the part of the workflow this file asserts over. yaml.v3
// ignores every key not named here.
type releaseSpec struct {
	On struct {
		Push struct {
			Tags []string
		}
	}
	Jobs map[string]releaseJobSpec
}

type releaseJobSpec struct {
	Permissions map[string]string
	Steps       []releaseStepSpec
}

type releaseStepSpec struct {
	Name string
	Uses string
	With map[string]string
	Run  string
}

// releaseVersion runs the script over the given arguments and returns what it
// wrote to stdout and stderr and the status it exited with. A failure to start
// the script at all fails the run rather than being reported as a refusal.
func releaseVersion(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var out, errOut strings.Builder
	cmd := exec.Command("sh", append([]string{releaseVersionScript}, args...)...)
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	var exit *exec.ExitError
	switch err := cmd.Run(); {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatalf("running %s: %v", releaseVersionScript, err)
	}
	return out.String(), errOut.String(), code
}

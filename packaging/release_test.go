// This file pins the release to the tag that triggers it. The version a
// release publishes is read off that tag by release-version.sh and by nothing
// else, so the mapping is covered here rather than left to the one run that
// would exercise it: a tag is pushed once, and a version it stamped wrongly is
// in a package operators already downloaded. The test runs the script the way
// the workflow does, through sh, and needs neither dpkg nor a runner.
package packaging_test

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// The script the release workflow reads the package version from. It is run
// through sh, so the mode the file carries in the repository never matters.
const releaseVersionScript = "release-version.sh"

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

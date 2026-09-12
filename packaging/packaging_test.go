// This file pins the Debian package to what the collector reads and to where
// the package puts it. Every mismatch it looks for fails quietly: a variable
// the environment file does not carry leaves the service running on the default
// it was meant to replace, a buffer path outside the state directory puts the
// undelivered events somewhere ProtectSystem=strict makes read-only, a unit
// pointing at a path the package does not ship fails only on the host that
// installs it, and a postremove that deletes the outbox destroys usage that
// reached no other copy. The test reads the four files from disk; it builds
// nothing, needs neither Docker nor dpkg, and so runs on the macOS machines
// this is developed on as well as in CI.
package packaging_test

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/b42labs/tally/internal/providers/openstack"
)

const (
	// The files this test reads. The spec sits at the repository root, beside
	// the Dockerfile it is the host counterpart of; the rest sit here.
	specPath       = "../nfpm.yaml"
	makefilePath   = "../Makefile"
	unitPath       = "tally-openstack-collector.service"
	defaultPath    = "tally-openstack-collector.default"
	postremovePath = "postremove.sh"

	packageName = "tally-openstack-collector"

	// Where the package puts what it ships. These are the paths an operator
	// scripts against and the unit and the environment file refer to.
	binaryDst      = "/usr/bin/tally-openstack-collector"
	unitDst        = "/lib/systemd/system/tally-openstack-collector.service"
	defaultDst     = "/etc/default/tally-openstack-collector"
	amqpSecretDst  = "/etc/tally/amqp-url"
	tokenSecretDst = "/etc/tally/ingest-token"
	stateDst       = "/var/lib/tally/collector"

	// The account the service runs under. It is the shared Tally name rather
	// than the collector's, so a later package for another service uses it too.
	serviceUser = "tally"

	// The content type that keeps dpkg from overwriting a file the operator
	// edited, and the one dependency the package declares.
	conffile   = "config|noreplace"
	dependency = "adduser"

	// The two secrets the environment file names through their *_FILE
	// companions and never sets itself: setting a secret beside its companion
	// is an error the collector refuses to start on.
	amqpVar  = "TALLY_OSC_AMQP_URL"
	tokenVar = "TALLY_OSC_TOKEN"
)

// spec is the part of nfpm.yaml this file asserts over. yaml.v3 ignores every
// field not named here.
type spec struct {
	Name     string
	Arch     string
	Platform string
	Version  string
	Depends  []string
	Contents []content
	Scripts  map[string]string
}

type content struct {
	Src      string
	Dst      string
	Type     string
	FileInfo fileInfo `yaml:"file_info"`
}

// fileInfo carries the mode as YAML resolves it: a leading zero makes the
// literal octal, so 0750 arrives as 0o750 and compares against a Go octal
// literal directly.
type fileInfo struct {
	Mode  int
	Owner string
	Group string
}

func TestPackageShipsWhatTheCollectorNeeds(t *testing.T) {
	// The six entries are the installed surface. A dropped one leaves a service
	// with no unit, no configuration or no directory to buffer into; a mode or
	// an owner that drifts either puts the broker password where every account
	// on the control node reads it, or leaves the collector unable to write its
	// own outbox.
	file := loadSpec(t)

	if file.Name != packageName {
		t.Errorf("name = %q, want %q, the name of the binary and of the unit", file.Name, packageName)
	}
	if !slices.Contains(file.Depends, dependency) {
		t.Errorf("depends = %v, want it to hold %q, which postinstall.sh calls addgroup and adduser from",
			file.Depends, dependency)
	}

	want := map[string]content{
		binaryDst:      {Src: "./bin/tally-openstack-collector-linux-${GOARCH}", FileInfo: fileInfo{Mode: 0o755}},
		unitDst:        {Src: "./packaging/" + unitPath},
		defaultDst:     {Src: "./packaging/" + defaultPath, Type: conffile, FileInfo: fileInfo{Mode: 0o644}},
		amqpSecretDst:  {Src: "./packaging/secrets/amqp-url", Type: conffile, FileInfo: fileInfo{Mode: 0o640, Owner: "root", Group: serviceUser}},
		tokenSecretDst: {Src: "./packaging/secrets/ingest-token", Type: conffile, FileInfo: fileInfo{Mode: 0o640, Owner: "root", Group: serviceUser}},
		stateDst:       {Type: "dir", FileInfo: fileInfo{Mode: 0o750, Owner: serviceUser, Group: serviceUser}},
	}

	shipped := make(map[string]content, len(file.Contents))
	for _, entry := range file.Contents {
		shipped[entry.Dst] = entry
	}
	if dsts, wantDsts := slices.Sorted(maps.Keys(shipped)), slices.Sorted(maps.Keys(want)); !slices.Equal(dsts, wantDsts) {
		t.Fatalf("the package installs %v, want %v", dsts, wantDsts)
	}

	for dst, expected := range want {
		entry := shipped[dst]
		if entry.Src != expected.Src {
			t.Errorf("%s is built from src %q, want %q", dst, entry.Src, expected.Src)
		}
		if entry.Type != expected.Type {
			t.Errorf("%s has type %q, want %q", dst, entry.Type, expected.Type)
		}
		if entry.FileInfo.Mode != expected.FileInfo.Mode {
			t.Errorf("%s has mode %#o, want %#o", dst, entry.FileInfo.Mode, expected.FileInfo.Mode)
		}
		if entry.FileInfo.Owner != expected.FileInfo.Owner || entry.FileInfo.Group != expected.FileInfo.Group {
			t.Errorf("%s is owned %q:%q, want %q:%q",
				dst, entry.FileInfo.Owner, entry.FileInfo.Group, expected.FileInfo.Owner, expected.FileInfo.Group)
		}
	}

	// Every source but the binary is committed, so a renamed file fails here
	// rather than in the middle of a release build.
	for _, entry := range file.Contents {
		if entry.Src == "" || strings.HasPrefix(entry.Src, "./bin/") {
			continue
		}
		if _, err := os.Stat(filepath.Join("..", entry.Src)); err != nil {
			t.Errorf("%s is built from %s, which is not there: %v", entry.Dst, entry.Src, err)
		}
	}
	for action, script := range file.Scripts {
		if _, err := os.Stat(filepath.Join("..", script)); err != nil {
			t.Errorf("the %s script is %s, which is not there: %v", action, script, err)
		}
	}

	// The binary is the one source `make deb` produces, so the two names have
	// to agree. They disagree loudly at build time, and only there.
	binary := strings.TrimPrefix(shipped[binaryDst].Src, "./")
	if recipe := "-o " + strings.ReplaceAll(binary, "${GOARCH}", "$(DEB_GOARCH)"); !strings.Contains(read(t, makefilePath), recipe) {
		t.Errorf("the Makefile does not build %q, which nfpm.yaml packages; the deb target writes a different name", recipe)
	}
}

func TestDefaultFileListsEveryVariableAndNoOther(t *testing.T) {
	// The collector ignores a variable it does not know, so a misspelled name in
	// the environment file is a default in place of a setting. A variable the
	// file omits is one an operator finds out about from a failure.
	set, commented := settings(t)

	for name := range set {
		if !slices.Contains(openstack.EnvNames, name) {
			t.Errorf("%s is set although the collector reads no variable of that name (openstack.EnvNames), so its value never arrives", name)
		}
	}
	for name := range commented {
		if !slices.Contains(openstack.EnvNames, name) {
			t.Errorf("%s is offered as a commented default although the collector reads no variable of that name (openstack.EnvNames)", name)
		}
	}

	for _, name := range openstack.EnvNames {
		if name == amqpVar || name == tokenVar {
			// Deliberately absent: both reach the process through their *_FILE
			// companion, and setting a secret beside its companion is refused.
			if _, ok := set[name]; ok {
				t.Errorf("%s is set beside %s_FILE, which the collector refuses to start on", name, name)
			}
			if _, ok := commented[name]; ok {
				t.Errorf("%s is offered as a commented default, which an operator who uncomments it is refused for", name)
			}
			continue
		}
		_, isSet := set[name]
		_, isCommented := commented[name]
		if !isSet && !isCommented {
			t.Errorf("%s appears in neither form, so the file does not document it", name)
		}
	}
}

func TestDefaultFilePointsIntoThePackage(t *testing.T) {
	// The three paths tie the running service to what the package installed. A
	// secret path that drifts reads a file nothing ships, and a buffer path
	// outside the state directory lands under ProtectSystem=strict, where the
	// collector may not write.
	set, _ := settings(t)

	paths := map[string]string{
		amqpVar + "_FILE":  amqpSecretDst,
		tokenVar + "_FILE": tokenSecretDst,
	}
	for name, want := range paths {
		if set[name] != want {
			t.Errorf("%s = %q, want %q, the file the package ships at 0640", name, set[name], want)
		}
	}

	if path := set["TALLY_OSC_BUFFER_PATH"]; !strings.HasPrefix(path, stateDst+"/") {
		t.Errorf("TALLY_OSC_BUFFER_PATH = %q, want a path under %q, the directory the package owns and the unit recreates",
			path, stateDst)
	}

	// Both are required and have no default, so an unset one is what the first
	// start reports. A value here would book a customer's usage to whatever
	// cloud this file happened to name.
	for _, name := range []string{"TALLY_OSC_CLOUD", "TALLY_OSC_REPORTING_URL"} {
		value, ok := set[name]
		if !ok {
			t.Errorf("%s is not in the file, so an operator is not told to fill it", name)
			continue
		}
		if value != "" {
			t.Errorf("%s = %q, want it shipped empty: no value here is right for a deployment", name, value)
		}
	}
}

func TestUnitRunsWhatThePackageInstalled(t *testing.T) {
	// The unit is the only thing that starts the collector on a packaged host.
	// A path here that the package does not ship fails on the host rather than
	// in this repository, and a missing [Install] section leaves
	// `systemctl enable` refusing the unit.
	unit := read(t, unitPath)
	directives := settingsFrom(unit)

	want := map[string]string{
		"User":            serviceUser,
		"Group":           serviceUser,
		"EnvironmentFile": defaultDst,
		"ExecStart":       binaryDst,
		"StateDirectory":  strings.TrimPrefix(stateDst, "/var/lib/"),
		"WantedBy":        "multi-user.target",
	}
	for name, value := range want {
		if directives[name] != value {
			t.Errorf("%s = %q, want %q", name, directives[name], value)
		}
	}
	if !strings.Contains(unit, "[Install]") {
		t.Errorf("%s carries no [Install] section, so systemctl enable refuses it", unitPath)
	}

	// StartLimit* belong to [Unit]; systemd ignores them under [Service], which
	// leaves a failing service restarting without a bound.
	// Cut on the section header at the start of a line: the comment above those
	// two directives names [Service] as the place they do not belong.
	unitSection, _, ok := strings.Cut(unit, "\n[Service]")
	if !ok {
		t.Fatalf("%s carries no [Service] section", unitPath)
	}
	for _, directive := range []string{"StartLimitIntervalSec=", "StartLimitBurst="} {
		if !strings.Contains(unitSection, directive) {
			t.Errorf("%s is not in the [Unit] section, where systemd reads it", directive)
		}
	}
}

func TestPurgeKeepsTheOutbox(t *testing.T) {
	// Between the acknowledgement on the bus and the delivery to the Reporting
	// API, an event lives in the outbox and nowhere else. No path through the
	// package manager may destroy it, so the script deletes nothing: it drops
	// the emptied credential directory with rmdir and says what it kept.
	script := read(t, postremovePath)

	if regexp.MustCompile(`(^|[^\w-])rm\s`).MatchString(script) {
		t.Errorf("%s runs rm, and the only state it could delete is the outbox, which holds events that reached no other copy",
			postremovePath)
	}
	if !strings.Contains(script, "rmdir /etc/tally") {
		t.Errorf("%s does not rmdir /etc/tally, so a purge leaves the credential directory behind", postremovePath)
	}
	if !strings.Contains(script, stateDst+"/outbox.db") {
		t.Errorf("%s does not name %s/outbox.db, so a purge says nothing about the file it kept", postremovePath, stateDst)
	}
}

// loadSpec decodes the package definition from disk.
func loadSpec(t *testing.T) spec {
	t.Helper()

	var file spec
	if err := yaml.Unmarshal([]byte(read(t, specPath)), &file); err != nil {
		t.Fatalf("parsing %s, which nfpm refuses to package: %v", specPath, err)
	}
	return file
}

// settings reads the environment file: the variables it sets, and the ones it
// offers as a commented default. A commented line counts as documentation, not
// as configuration, so the two are kept apart.
func settings(t *testing.T) (set, commented map[string]string) {
	t.Helper()

	content := read(t, defaultPath)
	set = settingsFrom(content)

	commented = make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimPrefix(strings.TrimSpace(line), "#")
		name, value, ok := strings.Cut(trimmed, "=")
		if !ok || !strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if name = strings.TrimSpace(name); strings.HasPrefix(name, "TALLY_") && !strings.Contains(name, " ") {
			commented[name] = strings.TrimSpace(value)
		}
	}
	return set, commented
}

// settingsFrom reads the KEY=VALUE lines of a file, skipping comments, blank
// lines and the section headers of a unit.
func settingsFrom(content string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "[") {
			continue
		}
		if name, value, ok := strings.Cut(line, "="); ok {
			values[strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	return values
}

// read returns a file's content, failing when it is missing rather than
// asserting over an empty string.
func read(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(content)
}

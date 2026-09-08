package refdoc

import (
	"strings"
	"testing"
)

// realMakefile is the Makefile of this repository, and the number of public
// targets the dev-stack page lists. A target added without a help comment is
// noticed here.
const (
	realMakefile    = "../../Makefile"
	realMakeTargets = 17
)

func TestMakeTargets(t *testing.T) {
	got, err := MakeTargets(readFixture(t, "Makefile"))
	if err != nil {
		t.Fatalf("MakeTargets() error = %v, want nil", err)
	}

	assertWant(t, "maketargets.want.md", got)
}

func TestMakeTargetsSkipsCommentsThatAreNotHelp(t *testing.T) {
	makefile := "# comment\n" +
		"## target:no space\n" +
		"##target: x\n" +
		"## these targets need the cluster\n" +
		"## up: create the cluster\n" +
		"up:\n"

	got, err := MakeTargets([]byte(makefile))
	if err != nil {
		t.Fatalf("MakeTargets() error = %v, want nil", err)
	}

	// The header and its separator are the two lines that are not a target.
	if n := countHeadings(got, "| ") - 2; n != 1 {
		t.Errorf("the Makefile rendered %d targets, want 1:\n%s", n, got)
	}
	if want := "| `up` | create the cluster |"; !strings.Contains(got, want) {
		t.Errorf("the rendering does not carry %q:\n%s", want, got)
	}
}

func TestMakeTargetsEscapesAPipe(t *testing.T) {
	got, err := MakeTargets([]byte("## docs: serve | build\ndocs:\n"))
	if err != nil {
		t.Fatalf("MakeTargets() error = %v, want nil", err)
	}

	if want := "| `docs` | serve \\| build |"; !strings.Contains(got, want) {
		t.Errorf("the rendering does not carry %q:\n%s", want, got)
	}
}

// A help comment is prose, and the site reads a bare <key> in it as markup, so
// a placeholder reaches the page in a code span like every other one.
func TestMakeTargetsEscapesAPlaceholder(t *testing.T) {
	got, err := MakeTargets([]byte("## export: write <key>.json beside the statements\nexport:\n"))
	if err != nil {
		t.Fatalf("MakeTargets() error = %v, want nil", err)
	}

	if want := "| `export` | write `<key>.json` beside the statements |"; !strings.Contains(got, want) {
		t.Errorf("the rendering does not carry %q:\n%s", want, got)
	}
}

// One rule may name several targets at once, and each of them is a target the
// file defines.
func TestMakeTargetsReadsAGroupedRule(t *testing.T) {
	makefile := "## docs: serve the site\n" +
		"## docs-build: build the site\n" +
		"docs docs-build: node_modules/.package-lock.json\n" +
		"\tnpm run $@\n"

	got, err := MakeTargets([]byte(makefile))
	if err != nil {
		t.Fatalf("MakeTargets() error = %v, want nil", err)
	}

	for _, want := range []string{"| `docs` | serve the site |", "| `docs-build` | build the site |"} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}
}

// A variable assignment carries a colon of its own, with or without a space in
// front of it, and still defines no target.
func TestMakeTargetsReadsNoTargetFromAnAssignment(t *testing.T) {
	const want = `refdoc: make help comment "SHELL" names no target`

	for _, assignment := range []string{"SHELL:=/usr/bin/env bash", "SHELL := /usr/bin/env bash"} {
		t.Run(assignment, func(t *testing.T) {
			got, err := MakeTargets([]byte("## SHELL: the shell every recipe runs in\n" + assignment + "\n"))
			if err == nil {
				t.Fatalf("MakeTargets() error = nil, want %q", want)
			}
			if err.Error() != want {
				t.Errorf("MakeTargets() error = %q, want %q", err, want)
			}
			if got != "" {
				t.Errorf("MakeTargets() = %q, want an empty string", got)
			}
		})
	}
}

func TestMakeTargetsReportsNoTargets(t *testing.T) {
	const want = "refdoc: no make targets"

	cases := map[string]string{
		"an empty file":        "",
		"nothing but comments": "# a\n# b\n",
	}

	for name, makefile := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := MakeTargets([]byte(makefile))
			if err == nil {
				t.Fatalf("MakeTargets() error = nil, want %q", want)
			}
			if err.Error() != want {
				t.Errorf("MakeTargets() error = %q, want %q", err, want)
			}
			if got != "" {
				t.Errorf("MakeTargets() = %q, want an empty string", got)
			}
		})
	}
}

func TestMakeTargetsReportsADuplicate(t *testing.T) {
	const want = `refdoc: make target "up" has two help comments`

	got, err := MakeTargets([]byte("## up: a\n## up: b\nup:\n"))
	if err == nil {
		t.Fatalf("MakeTargets() error = nil, want %q", want)
	}
	if err.Error() != want {
		t.Errorf("MakeTargets() error = %q, want %q", err, want)
	}
	if got != "" {
		t.Errorf("MakeTargets() = %q, want an empty string", got)
	}
}

func TestMakeTargetsReportsAHelpCommentWithoutATarget(t *testing.T) {
	const want = `refdoc: make help comment "Cluster" names no target`

	got, err := MakeTargets([]byte("## Cluster: the targets below need the cluster\nup:\n"))
	if err == nil {
		t.Fatalf("MakeTargets() error = nil, want %q", want)
	}
	if err.Error() != want {
		t.Errorf("MakeTargets() error = %q, want %q", err, want)
	}
	if got != "" {
		t.Errorf("MakeTargets() = %q, want an empty string", got)
	}
}

func TestMakeTargetsRendersTheTargetsOfThisRepository(t *testing.T) {
	const (
		firstRow = "| `check-tools` | check that the tools the dev stack and the tutorials need answer |"
		lastRow  = "| `docs-build` | build the documentation site; a dead internal link fails the build |"
	)

	got, err := MakeTargets(readRepositoryFile(t, realMakefile))
	if err != nil {
		t.Fatalf("MakeTargets() error = %v, want nil", err)
	}

	// The header and its separator are the two lines that are not a target.
	if n := countHeadings(got, "| ") - 2; n != realMakeTargets {
		t.Errorf("the Makefile rendered %d targets, want %d:\n%s", n, realMakeTargets, got)
	}
	for _, want := range []string{firstRow, lastRow} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendering does not carry %q:\n%s", want, got)
		}
	}

	// The rows stand in the order the Makefile writes the targets in, so the
	// first target of the file is the row under the separator and the last one
	// closes the table.
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if lines[2] != firstRow {
		t.Errorf("the third line is %q, want %q", lines[2], firstRow)
	}
	if last := lines[len(lines)-1]; last != lastRow {
		t.Errorf("the last row is %q, want %q", last, lastRow)
	}
}

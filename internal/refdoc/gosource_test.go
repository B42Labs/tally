package refdoc

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// readFixture returns the bytes of a source under testdata. The renderers take
// bytes rather than a path, so this is what a caller does with the file it
// generates a page from.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	src, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	return src
}

// assertWant compares rendered text with the file that holds what the renderer
// is meant to produce.
func assertWant(t *testing.T, name, got string) {
	t.Helper()

	path := filepath.Join("testdata", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the expectation: %v", err)
	}
	if want := string(raw); got != want {
		t.Errorf("rendered text differs from %s:\ngot\n%s\nwant\n%s", path, got, want)
	}
}

// assertRendersRealSource checks that a source of this repository renders at
// all. What the page says about it is the page's business; that the renderer
// gets through the file is this package's.
func assertRendersRealSource(t *testing.T, path string, render func(src []byte) (string, error)) {
	t.Helper()

	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	rendered, err := render(src)
	if err != nil {
		t.Fatalf("rendering %s: %v", path, err)
	}
	if rendered == "" {
		t.Errorf("rendering %s produced nothing", path)
	}
}

func TestJoinComment(t *testing.T) {
	cases := map[string]struct {
		src  string
		want string
	}{
		"absent": {
			src:  "package p\n\ntype T struct{}\n",
			want: "",
		},
		"one line": {
			src:  "package p\n\n// T is the type.\ntype T struct{}\n",
			want: "T is the type.",
		},
		"several lines": {
			src:  "package p\n\n// T is the type\n// this test parses.\ntype T struct{}\n",
			want: "T is the type this test parses.",
		},
		"two paragraphs": {
			src:  "package p\n\n// T is the type.\n//\n// It has a second paragraph.\ntype T struct{}\n",
			want: "T is the type. It has a second paragraph.",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			file, err := parser.ParseFile(token.NewFileSet(), sourceName, tc.src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parsing the source: %v", err)
			}
			decl, ok := findType(file, "T")
			if !ok {
				t.Fatal("the fixture declares no type T")
			}
			if got := joinComment(decl.doc); got != tc.want {
				t.Errorf("joinComment() = %q, want %q", got, tc.want)
			}
		})
	}
}

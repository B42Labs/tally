package refdoc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// recorder stands in for testing.TB so Verify can be checked for the failures
// it is meant to report, which a *testing.T cannot be made to do. The embedded
// TB is nil: only the three methods below are ever called on it.
type recorder struct {
	testing.TB
	messages []string
	errs     []error
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.record(format, args)
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.record(format, args)
	runtime.Goexit()
}

// record keeps the formatted message and every error among the arguments, so a
// test can assert on the text and on the cause behind it.
func (r *recorder) record(format string, args []any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			r.errs = append(r.errs, err)
		}
	}
}

// runRecorded calls fn with a recorder and waits for it. It runs in a goroutine
// of its own because Fatalf ends the goroutine it is called from, the way the
// real one does.
func runRecorded(fn func(t testing.TB)) *recorder {
	rec := &recorder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(rec)
	}()
	<-done
	return rec
}

// writePage writes content to a page of its own and returns its path.
func writePage(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "page.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing the page: %v", err)
	}
	return path
}

// readPage returns what a page holds.
func readPage(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the page: %v", err)
	}
	return string(raw)
}

// onePage carries one block between hand-written prose.
const onePage = `# Settings

Prose above.

<!-- refdoc:begin settings -->
| Variable |
| --- |
<!-- refdoc:end settings -->

Prose below.
`

// twoBlockPage carries two blocks, so that a call handing one of them can be
// checked for leaving the other alone.
const twoBlockPage = `<!-- refdoc:begin first -->
one
<!-- refdoc:end first -->

<!-- refdoc:begin second -->
two
<!-- refdoc:end second -->
`

func TestVerifyAcceptsAMatchingPage(t *testing.T) {
	t.Setenv(updateEnv, "")
	page := writePage(t, onePage)

	rec := runRecorded(func(t testing.TB) {
		Verify(t, page, map[string]string{"settings": "| Variable |\n| --- |\n"})
	})

	if len(rec.messages) != 0 {
		t.Errorf("a matching page failed: %v", rec.messages)
	}
}

func TestVerifyAcceptsAnEmptyBlock(t *testing.T) {
	t.Setenv(updateEnv, "")
	page := writePage(t, "<!-- refdoc:begin empty -->\n<!-- refdoc:end empty -->\n")

	rec := runRecorded(func(t testing.TB) {
		Verify(t, page, map[string]string{"empty": ""})
	})

	if len(rec.messages) != 0 {
		t.Errorf("adjacent markers did not compare equal to the empty text: %v", rec.messages)
	}
}

func TestVerifyRejectsAPageItCannotCheck(t *testing.T) {
	t.Setenv(updateEnv, "")

	cases := map[string]struct {
		content string
		blocks  map[string]string
		want    string
	}{
		"no blocks": {
			content: onePage,
			blocks:  map[string]string{},
			want:    "Verify called with no blocks",
		},
		"no begin marker": {
			content: "# Settings\n",
			blocks:  map[string]string{"settings": ""},
			want:    `no block "settings" (add the markers)`,
		},
		"no end marker": {
			content: "<!-- refdoc:begin settings -->\ntext\n",
			blocks:  map[string]string{"settings": ""},
			want:    `block "settings" is not terminated`,
		},
		"block twice": {
			content: onePage + onePage,
			blocks:  map[string]string{"settings": ""},
			want:    `block "settings" appears twice`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			page := writePage(t, tc.content)

			rec := runRecorded(func(t testing.TB) { Verify(t, page, tc.blocks) })

			if len(rec.messages) != 1 {
				t.Fatalf("messages = %v, want exactly one", rec.messages)
			}
			if !strings.Contains(rec.messages[0], tc.want) {
				t.Errorf("message = %q, want it to carry %q", rec.messages[0], tc.want)
			}
			if !strings.Contains(rec.messages[0], page) {
				t.Errorf("message = %q, want it to name the page %q", rec.messages[0], page)
			}
		})
	}
}

func TestVerifyReportsAPageThatIsNotThere(t *testing.T) {
	t.Setenv(updateEnv, "")
	page := filepath.Join(t.TempDir(), "absent.md")

	rec := runRecorded(func(t testing.TB) {
		Verify(t, page, map[string]string{"settings": ""})
	})

	if len(rec.messages) != 1 {
		t.Fatalf("messages = %v, want exactly one", rec.messages)
	}
	if !strings.Contains(rec.messages[0], page) {
		t.Errorf("message = %q, want it to name the page %q", rec.messages[0], page)
	}
	if len(rec.errs) != 1 || !errors.Is(rec.errs[0], fs.ErrNotExist) {
		t.Errorf("errors = %v, want one that is fs.ErrNotExist", rec.errs)
	}
}

func TestVerifyReportsADifferingBlock(t *testing.T) {
	t.Setenv(updateEnv, "")
	page := writePage(t, onePage)

	rec := runRecorded(func(t testing.TB) {
		Verify(t, page, map[string]string{"settings": "| Variable | Type |\n| --- | --- |\n"})
	})

	if len(rec.messages) != 1 {
		t.Fatalf("messages = %v, want exactly one", rec.messages)
	}
	for _, want := range []string{page, `"settings"`, "run make generate", "want: | Variable | Type |", "got: | Variable |"} {
		if !strings.Contains(rec.messages[0], want) {
			t.Errorf("message = %q, want it to carry %q", rec.messages[0], want)
		}
	}
	if readPage(t, page) != onePage {
		t.Error("the page was rewritten without TALLY_UPDATE_DOCS=1")
	}
}

func TestVerifyCapsTheReportedDifferences(t *testing.T) {
	t.Setenv(updateEnv, "")
	var stored, rendered strings.Builder
	for i := range 25 {
		fmt.Fprintf(&stored, "stored %d\n", i)
		fmt.Fprintf(&rendered, "rendered %d\n", i)
	}
	page := writePage(t, "<!-- refdoc:begin settings -->\n"+stored.String()+"<!-- refdoc:end settings -->\n")

	rec := runRecorded(func(t testing.TB) {
		Verify(t, page, map[string]string{"settings": rendered.String()})
	})

	if len(rec.messages) != 1 {
		t.Fatalf("messages = %v, want exactly one", rec.messages)
	}
	if got := strings.Count(rec.messages[0], "want: "); got != maxDiffPairs {
		t.Errorf("reported pairs = %d, want %d", got, maxDiffPairs)
	}
}

func TestVerifyReportsAnAbsentLine(t *testing.T) {
	t.Setenv(updateEnv, "")
	page := writePage(t, "<!-- refdoc:begin settings -->\none\n<!-- refdoc:end settings -->\n")

	rec := runRecorded(func(t testing.TB) {
		Verify(t, page, map[string]string{"settings": "one\ntwo\n"})
	})

	if len(rec.messages) != 1 {
		t.Fatalf("messages = %v, want exactly one", rec.messages)
	}
	if !strings.Contains(rec.messages[0], "want: two\ngot: (absent)") {
		t.Errorf("message = %q, want it to report the absent line", rec.messages[0])
	}
}

func TestVerifyChecksOnlyTheBlocksItIsHanded(t *testing.T) {
	t.Setenv(updateEnv, "")
	page := writePage(t, twoBlockPage)

	rec := runRecorded(func(t testing.TB) {
		Verify(t, page, map[string]string{"first": "one\n"})
	})

	if len(rec.messages) != 0 {
		t.Errorf("the block that was not handed was checked: %v", rec.messages)
	}
}

func TestVerifyRewritesTheBlockAndNothingElse(t *testing.T) {
	t.Setenv(updateEnv, "1")
	page := writePage(t, twoBlockPage)

	rec := runRecorded(func(t testing.TB) {
		Verify(t, page, map[string]string{"first": "rendered\nlines\n"})
	})

	if len(rec.messages) != 0 {
		t.Fatalf("the rewrite failed: %v", rec.messages)
	}
	want := strings.Replace(twoBlockPage, "one\n", "rendered\nlines\n", 1)
	if got := readPage(t, page); got != want {
		t.Errorf("page =\n%s\nwant\n%s", got, want)
	}

	// The page is now what the renderer produces, so a run without the switch
	// has nothing left to report.
	t.Setenv(updateEnv, "")
	rec = runRecorded(func(t testing.TB) {
		Verify(t, page, map[string]string{"first": "rendered\nlines\n"})
	})
	if len(rec.messages) != 0 {
		t.Errorf("the rewritten page still fails: %v", rec.messages)
	}
}

func TestVerifyKeepsEveryConcurrentRewriteOfOnePage(t *testing.T) {
	// A page carries blocks of several packages, and `make generate` runs their
	// test binaries at once. A rewrite reads the page and writes it back whole,
	// so one that is not exclusive keeps the block of whoever wrote last and
	// drops the rest.
	t.Setenv(updateEnv, "1")

	const blocks = 8
	var content strings.Builder
	for i := range blocks {
		fmt.Fprintf(&content, "<!-- refdoc:begin block%d -->\nold\n<!-- refdoc:end block%d -->\n", i, i)
	}
	page := writePage(t, content.String())

	start := make(chan struct{})
	var wg sync.WaitGroup
	recs := make([]*recorder, blocks)
	for i := range blocks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			recs[i] = runRecorded(func(t testing.TB) {
				Verify(t, page, map[string]string{
					fmt.Sprintf("block%d", i): fmt.Sprintf("rendered %d\n", i),
				})
			})
		}()
	}
	close(start)
	wg.Wait()

	for i, rec := range recs {
		if len(rec.messages) != 0 {
			t.Errorf("the rewrite of block %d failed: %v", i, rec.messages)
		}
	}
	got := readPage(t, page)
	for i := range blocks {
		if want := fmt.Sprintf("rendered %d", i); !strings.Contains(got, want) {
			t.Errorf("the page lost the rewrite of block %d:\n%s", i, got)
		}
	}
}

func TestVerifyRewritesABlockToNothing(t *testing.T) {
	t.Setenv(updateEnv, "1")
	page := writePage(t, twoBlockPage)

	rec := runRecorded(func(t testing.TB) {
		Verify(t, page, map[string]string{"second": ""})
	})

	if len(rec.messages) != 0 {
		t.Fatalf("the rewrite failed: %v", rec.messages)
	}
	want := strings.Replace(twoBlockPage, "two\n", "", 1)
	if got := readPage(t, page); got != want {
		t.Errorf("page =\n%s\nwant\n%s", got, want)
	}
}

func TestFenced(t *testing.T) {
	cases := map[string]struct {
		lang string
		body []byte
		want string
	}{
		"nil body": {
			lang: "text",
			body: nil,
			want: "```text\n```\n",
		},
		"empty body": {
			lang: "text",
			body: []byte(""),
			want: "```text\n```\n",
		},
		"body without a newline": {
			lang: "json",
			body: []byte("{}"),
			want: "```json\n{}\n```\n",
		},
		"body with a trailing newline": {
			lang: "json",
			body: []byte("{}\n"),
			want: "```json\n{}\n```\n",
		},
		"body with several trailing newlines": {
			lang: "json",
			body: []byte("{}\n\n\n"),
			want: "```json\n{}\n```\n",
		},
		"body holding a fence": {
			lang: "markdown",
			body: []byte("```go\nx := 1\n```\n"),
			want: "````markdown\n```go\nx := 1\n```\n````\n",
		},
		"no language": {
			lang: "",
			body: []byte("plain"),
			want: "```\nplain\n```\n",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := Fenced(tc.lang, tc.body); got != tc.want {
				t.Errorf("Fenced() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTableEscapesThePipe(t *testing.T) {
	got := table([]string{"Name", "Meaning"}, [][]string{{"sep", "a | b"}})
	want := "| Name | Meaning |\n| --- | --- |\n| sep | a \\| b |\n"

	if got != want {
		t.Errorf("table() = %q, want %q", got, want)
	}
}

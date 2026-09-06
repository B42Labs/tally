// Package refdoc renders the parts of the reference pages that state what the
// code exposes, and checks that the pages still hold what the renderers
// produce.
//
// A generated block is the text between two marker lines in a Markdown page:
//
//	<!-- refdoc:begin settings -->
//	(rendered text)
//	<!-- refdoc:end settings -->
//
// The name matches ^[a-z0-9-]+$, is unique within the page, and the end marker
// repeats it. Everything outside the markers is hand-written and stays byte for
// byte as it is. The text between them belongs to a renderer and is replaced
// whole: it is the lines between the markers joined by newlines and closed by
// one, and it is empty for a block whose markers are adjacent.
//
// Verify is the only function here that touches a page. The renderers return
// strings, and a test hands them to Verify together with the page they belong
// in, so a page that drifts from the code fails `go test ./...` rather than
// being noticed by a reader. Verify checks the blocks it is handed and no
// others.
//
// With TALLY_UPDATE_DOCS=1 in the environment, Verify writes the rendered text
// into the page instead of reporting the mismatch, which is how the pages are
// regenerated after a change to the code they describe. A rewrite holds the
// exclusive lock of the page while it reads and writes it, so the test binaries
// of the packages that write blocks of one page may regenerate it at once.
package refdoc

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// updateEnv is the variable that turns a mismatch into a rewrite of the page.
const updateEnv = "TALLY_UPDATE_DOCS"

// maxDiffPairs bounds how many differing lines a mismatch reports. A renderer
// whose output changed wholesale differs in every line, and the first pairs
// already say what happened.
const maxDiffPairs = 20

// minFence is the shortest code fence Markdown accepts.
const minFence = 3

// Verify compares every named block of page with the text a renderer produced
// for it, and fails t for each block that differs. The map is keyed by block
// name; a name the page carries no markers for is a failure of its own, because
// a renderer whose block was dropped from the page would otherwise be verified
// against nothing.
//
// With TALLY_UPDATE_DOCS=1 a differing block is rewritten instead: the page then
// holds the rendered text, and the markers and every byte outside them are
// unchanged.
func Verify(t testing.TB, page string, blocks map[string]string) {
	t.Helper()

	if len(blocks) == 0 {
		t.Fatalf("%s: Verify called with no blocks", page)
	}

	// A rewrite reads the page and writes it back whole, and one page carries
	// blocks of several packages, whose test binaries run at once. The lock is
	// taken before the read, so the read, the edit and the write are one step
	// against the other binaries and none of their updates is written over.
	if os.Getenv(updateEnv) == "1" {
		unlock, err := lockPage(page)
		if err != nil {
			t.Fatalf("%s: %v", page, err)
		}
		defer unlock()
	}

	raw, err := os.ReadFile(page)
	if err != nil {
		t.Fatalf("%s: %v", page, err)
	}

	// The names are walked in order so that a page with several stale blocks
	// reports them the same way on every run.
	lines := strings.Split(string(raw), "\n")
	rewritten := false
	for _, name := range slices.Sorted(maps.Keys(blocks)) {
		begin, end, err := locate(lines, name)
		if err != nil {
			t.Fatalf("%s: %v", page, err)
		}

		want := blocks[name]
		got := blockText(lines[begin+1 : end])
		if got == want {
			continue
		}
		if os.Getenv(updateEnv) != "1" {
			t.Errorf("%s: block %q differs from its source, run make generate\n%s",
				page, name, difference(want, got))
			continue
		}
		lines = slices.Concat(lines[:begin+1], textLines(want), lines[end:])
		rewritten = true
	}

	if !rewritten {
		return
	}
	if err := os.WriteFile(page, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatalf("%s: %v", page, err)
	}
}

// lockPage takes the exclusive lock of the page and returns the call that
// releases it. The lock is the one flock(2) keeps per open file, so it holds
// against another process and against another goroutine of this one, and
// closing the file releases it whichever way Verify returns.
func lockPage(page string) (func(), error) {
	f, err := os.OpenFile(page, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("locking the page: %w", err)
	}
	return func() { _ = f.Close() }, nil
}

// locate returns the indices of the two marker lines of the named block. The
// end marker it reports is the first one after the begin marker, so an end
// marker of another block above it is not mistaken for this block's.
func locate(lines []string, name string) (begin, end int, err error) {
	beginMarker := fmt.Sprintf("<!-- refdoc:begin %s -->", name)
	endMarker := fmt.Sprintf("<!-- refdoc:end %s -->", name)

	begin, end = -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case beginMarker:
			if begin >= 0 {
				return 0, 0, fmt.Errorf("block %q appears twice", name)
			}
			begin = i
		case endMarker:
			if begin >= 0 && end < 0 {
				end = i
			}
		}
	}
	if begin < 0 {
		return 0, 0, fmt.Errorf("no block %q (add the markers)", name)
	}
	if end < 0 {
		return 0, 0, fmt.Errorf("block %q is not terminated", name)
	}
	return begin, end, nil
}

// blockText renders the lines between two markers as the text a renderer is
// compared with.
func blockText(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// textLines is the inverse of blockText: the closing newline ends the last line
// rather than opening an empty one, and empty text is no line at all.
func textLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// difference lists the first differing lines of two block texts as want and got
// pairs. A line one of the two sides does not have reads (absent).
func difference(want, got string) string {
	wantLines, gotLines := textLines(want), textLines(got)

	var b strings.Builder
	pairs := 0
	for i := 0; i < max(len(wantLines), len(gotLines)) && pairs < maxDiffPairs; i++ {
		w, g := lineAt(wantLines, i), lineAt(gotLines, i)
		if w == g {
			continue
		}
		fmt.Fprintf(&b, "want: %s\ngot: %s\n", w, g)
		pairs++
	}
	return b.String()
}

// lineAt returns line i, or the marker for a side that ends before it.
func lineAt(lines []string, i int) string {
	if i >= len(lines) {
		return "(absent)"
	}
	return lines[i]
}

// Fenced renders body as a fenced code block tagged lang. The fence is one
// backtick longer than the longest run of backticks in the body, so a body that
// carries a fence of its own stays inside the block. The body ends in exactly
// one newline, and an empty body renders an empty block.
func Fenced(lang string, body []byte) string {
	fence := strings.Repeat("`", max(minFence, longestRun(body, '`')+1))
	trimmed := strings.TrimRight(string(body), "\n")

	var b strings.Builder
	b.WriteString(fence + lang + "\n")
	if trimmed != "" {
		b.WriteString(trimmed + "\n")
	}
	b.WriteString(fence + "\n")
	return b.String()
}

// longestRun returns the length of the longest run of c in body.
func longestRun(body []byte, c byte) int {
	longest, run := 0, 0
	for _, b := range body {
		if b != c {
			run = 0
			continue
		}
		run++
		longest = max(longest, run)
	}
	return longest
}

// code renders s as a code span. Every identifier, path, template and literal a
// renderer emits goes through it, so no angle bracket and no brace reaches the
// site's template engine bare.
func code(s string) string {
	return "`" + s + "`"
}

// cell renders s as the content of a table cell. The pipe separates the columns
// of a GitHub-flavoured table, so a pipe in the text is escaped.
func cell(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

// yesNo is how a table answers a question a cell holds.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// prosePattern is one whitespace-delimited token of prose.
var prosePattern = regexp.MustCompile(`\S+`)

// escapePlaceholders wraps every whitespace-delimited token holding a <...>
// placeholder in a code span. A command's help text names its arguments and its
// output files that way, and the site reads a bare <key> as markup rather than
// as text. A token that already carries a backtick is left alone, so nothing is
// wrapped twice. The whitespace between the tokens is kept, because the prose
// it separates is rendered as it was written.
func escapePlaceholders(s string) string {
	return prosePattern.ReplaceAllStringFunc(s, wrapPlaceholder)
}

// wrapPlaceholder is one token of prose, in a code span where it holds a
// placeholder.
func wrapPlaceholder(token string) string {
	open := strings.Index(token, "<")
	if open < 0 || !strings.Contains(token[open:], ">") || strings.Contains(token, "`") {
		return token
	}
	return code(token)
}

// table renders a header row, its separator, and one row per entry. Every cell
// is escaped, so a renderer passes the text it wants to show rather than the
// Markdown for it.
func table(header []string, rows [][]string) string {
	separator := make([]string, len(header))
	for i := range separator {
		separator[i] = "---"
	}

	var b strings.Builder
	writeRow(&b, header)
	writeRow(&b, separator)
	for _, row := range rows {
		writeRow(&b, row)
	}
	return b.String()
}

// writeRow writes one table row.
func writeRow(b *strings.Builder, cells []string) {
	b.WriteString("|")
	for _, c := range cells {
		b.WriteString(" " + cell(c) + " |")
	}
	b.WriteString("\n")
}

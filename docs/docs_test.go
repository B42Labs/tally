// This file pins every page of the documentation site to the conventions
// docs/contributing/authoring-conventions.md states. Every mismatch it looks
// for fails quietly: a page no sidebar links is published and never opened, a
// heading style drifts across six parallel pull requests before anyone
// compares two of them, a quadrant naming another directory files the page
// under a promise the directory does not keep, and a sidebar entry pointing at
// nothing renders as a dead link. The pages are docs/index.md and the Markdown
// under the five section directories; the documents that predate the site are
// the ones srcExclude in docs/.vitepress/config.mts names, and are not read. A
// Markdown file in neither set is published past every rule here, so it fails
// a test of its own. The rules that judge a heading or a link are exercised
// against inputs that break them as well, because a gate no input ever fails
// is a gate nobody can trust. Whether the site builds is what `make
// docs-build` answers, by running VitePress over the same files; this test
// reads them from disk and needs no Node toolchain.
package docs_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	sidebarFile     = ".vitepress/sidebar.json"
	navFile         = ".vitepress/nav.json"
	configFile      = ".vitepress/config.mts"
	properNounsFile = ".vitepress/proper-nouns.txt"
	rootPage        = "index.md"
	rootQuadrant    = "orientation"
)

// sections maps each section directory to the quadrant its pages declare.
var sections = map[string]string{
	"tutorials":    "tutorial",
	"how-to":       "how-to",
	"reference":    "reference",
	"explanation":  "explanation",
	"contributing": "contributing",
}

// audiences are the readers a page may address.
var audiences = []string{"operator", "integrator", "contributor", "all"}

// headingRe matches an ATX heading and captures its level and its text.
var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)

// fenceRe matches an opening or a closing code fence and captures its marker.
// A fence closes on the same character at the same length or longer, so the
// marker has to be carried rather than a flag flipped: a page documenting
// Markdown nests a shorter fence inside a longer one, and a flag would read the
// example as prose and the prose as an example.
var fenceRe = regexp.MustCompile("^(`{3,}|~{3,})")

// srcExcludeRe captures the body of the srcExclude array of the VitePress
// configuration, and quotedRe the patterns inside it.
var (
	srcExcludeRe = regexp.MustCompile(`(?s)srcExclude:\s*\[(.*?)]`)
	quotedRe     = regexp.MustCompile(`'([^']*)'`)
)

// codeSpanRe matches an inline code span. A span holds an identifier rather
// than a word, so the sentence-case rule counts it as one capitalised token.
var codeSpanRe = regexp.MustCompile("`[^`]*`")

// punctuation is stripped from both ends of a heading word before the word is
// judged, so a heading may end in a question mark or hold a parenthesis.
const punctuation = `.,:;?!()[]"'“”‘’`

// frontmatter is the part of a page's YAML block this file asserts over. A key
// it does not name belongs to VitePress and is ignored.
type frontmatter struct {
	Title       string
	Description string
	Quadrant    string
	Audience    string
	Layout      string
}

// page is one Markdown file of the site, split at its frontmatter. bodyLine is
// the line number of the first body line, so a heading reports the line the
// editor jumps to rather than its offset in the body.
type page struct {
	path     string
	front    frontmatter
	body     []string
	bodyLine int
}

// heading is one ATX heading of a page's body.
type heading struct {
	line  int
	level int
	text  string
}

// sidebarItem is one entry of the sidebar: a group, which carries items, or a
// link to a page. encoding/json matches the lowercase keys of the file to
// these fields.
type sidebarItem struct {
	Text      string
	Link      string
	Collapsed bool
	Items     []sidebarItem
}

// navItem is one entry of the top navigation. It is flat: the navigation of
// this site is one entry per section.
type navItem struct {
	Text        string
	Link        string
	ActiveMatch string
}

func TestEveryPageCarriesTheFrontmatterContract(t *testing.T) {
	for _, path := range pagePaths(t) {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			p, err := loadPage(t, path)
			if err != nil {
				t.Fatalf("%s:1: %v", path, err)
			}

			if strings.TrimSpace(p.front.Title) == "" {
				t.Errorf("%s: frontmatter title is empty", path)
			}
			if strings.TrimSpace(p.front.Description) == "" {
				t.Errorf("%s: frontmatter description is empty", path)
			}

			quadrant := rootQuadrant
			if path != rootPage {
				quadrant = sections[strings.SplitN(path, "/", 2)[0]]
			}
			if p.front.Quadrant != quadrant {
				t.Errorf("%s: quadrant %q, want %q for this directory", path, p.front.Quadrant, quadrant)
			}
			if !slices.Contains(audiences, p.front.Audience) {
				t.Errorf("%s: audience %q not in %v", path, p.front.Audience, audiences)
			}
			if p.front.Layout != "" {
				t.Errorf("%s: layout %q is not allowed: the site has no landing page", path, p.front.Layout)
			}
		})
	}
}

func TestHeadingsAreSentenceCaseWithOneH1(t *testing.T) {
	nouns := properNouns(t)

	for _, path := range pagePaths(t) {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			p, err := loadPage(t, path)
			if err != nil {
				// TestEveryPageCarriesTheFrontmatterContract reports the page
				// that does not parse; without a frontmatter block there is no
				// title to compare an H1 against either.
				t.Skip(err)
			}

			var h1s []heading
			previous := 0
			for _, h := range headings(p) {
				for _, word := range offendingWords(h.text, nouns) {
					t.Errorf("%s:%d: heading %q: %q is not sentence case (add it to docs/.vitepress/proper-nouns.txt if it is a proper noun)",
						p.path, h.line, h.text, word)
				}
				// VitePress builds the on-page outline out of the levels, so a
				// skipped level nests a group under a heading that is not there
				// and a screen reader reads the hole out.
				if previous != 0 && h.level > previous+1 {
					t.Errorf("%s:%d: heading %q is an H%d under an H%d: no level may be skipped",
						p.path, h.line, h.text, h.level, previous)
				}
				previous = h.level
				if h.level == 1 {
					h1s = append(h1s, h)
				}
			}

			// The H1 is the page's own name: VitePress renders it as the
			// document title and the local search indexes it, so a second one
			// splits the page and a missing one leaves it unnamed.
			if len(h1s) != 1 {
				t.Fatalf("%s: %d H1 headings, want 1", p.path, len(h1s))
			}
			if h1s[0].text != p.front.Title {
				t.Errorf("%s: H1 %q does not equal title %q", p.path, h1s[0].text, p.front.Title)
			}
		})
	}
}

func TestSidebarReachesEveryPageAndOnlyPages(t *testing.T) {
	raw, err := os.ReadFile(sidebarFile)
	if err != nil {
		t.Fatalf("reading %s: %v", sidebarFile, err)
	}

	var sidebar map[string][]sidebarItem
	if err := json.Unmarshal(raw, &sidebar); err != nil {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			t.Fatalf("parsing %s: %v at offset %d", sidebarFile, err, syntaxErr.Offset)
		}
		t.Fatalf("parsing %s: %v", sidebarFile, err)
	}

	// pages maps every page of the site to whether a sidebar entry links it.
	pages := map[string]bool{}
	for _, path := range pagePaths(t) {
		pages[path] = false
	}

	var walk func(key string, items []sidebarItem)
	walk = func(key string, items []sidebarItem) {
		for _, item := range items {
			switch {
			case item.Text == "" && item.Link == "":
				t.Errorf("sidebar: group under %q has no text", key)
			case item.Text == "":
				t.Errorf("sidebar: entry %q has no text", item.Link)
			}
			if item.Link != "" {
				// The page set decides, not the disk: a link to a document
				// that predates the site resolves to a file that exists and is
				// still not a page VitePress builds.
				path, ok := resolveLink(item.Link)
				if _, isPage := pages[path]; ok && isPage {
					pages[path] = true
				} else {
					t.Errorf("sidebar link %q is not a page of the site", item.Link)
				}
			}
			walk(key, item.Items)
		}
	}

	for _, key := range slices.Sorted(maps.Keys(sidebar)) {
		if !isSectionKey(key) {
			t.Errorf("sidebar key %q is not a section (want / or /<section>/)", key)
		}
		walk(key, sidebar[key])
	}

	// The root page is reached through the site title rather than through a
	// sidebar entry.
	for _, path := range slices.Sorted(maps.Keys(pages)) {
		if path == rootPage {
			continue
		}
		t.Run(path, func(t *testing.T) {
			if !pages[path] {
				t.Errorf("%s: not reachable from the sidebar (add it to docs/.vitepress/sidebar.json)", path)
			}
		})
	}
}

func TestNavReachesEverySectionAndOnlyPages(t *testing.T) {
	raw, err := os.ReadFile(navFile)
	if err != nil {
		t.Fatalf("reading %s: %v", navFile, err)
	}

	var nav []navItem
	if err := json.Unmarshal(raw, &nav); err != nil {
		t.Fatalf("parsing %s: %v", navFile, err)
	}

	pages := map[string]bool{}
	for _, path := range pagePaths(t) {
		pages[path] = true
	}

	// linked records the section each entry leads into, so a section the
	// navigation forgets is reported below rather than passing unnoticed.
	linked := map[string]bool{}
	for _, item := range nav {
		if strings.TrimSpace(item.Text) == "" {
			t.Errorf("nav entry %q has no text", item.Link)
		}
		path, ok := resolveLink(item.Link)
		if !ok || !pages[path] {
			t.Errorf("nav link %q is not a page of the site", item.Link)
			continue
		}
		// activeMatch keeps the entry highlighted while the reader is inside
		// the section, so it is a section prefix behind a start anchor.
		if !isSectionKey(strings.TrimPrefix(item.ActiveMatch, "^")) {
			t.Errorf("nav entry %q: activeMatch %q names no section", item.Text, item.ActiveMatch)
		}
		linked[strings.SplitN(path, "/", 2)[0]] = true
	}

	for _, section := range slices.Sorted(maps.Keys(sections)) {
		if !linked[section] {
			t.Errorf("%s: no entry in the top navigation (add it to docs/.vitepress/nav.json)", section)
		}
	}
}

func TestNoMarkdownFileEscapesTheGate(t *testing.T) {
	patterns := srcExclude(t)

	pages := map[string]bool{}
	for _, path := range pagePaths(t) {
		pages[path] = true
	}

	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".vitepress" {
				return fs.SkipDir
			}
			return nil
		}
		path = filepath.ToSlash(path)
		if !strings.HasSuffix(path, ".md") || pages[path] || isExcluded(path, patterns) {
			return nil
		}
		t.Errorf("%s: neither a page of the site nor named by srcExclude in %s, so VitePress publishes it and no rule above reads it",
			path, configFile)
		return nil
	})
	if err != nil {
		t.Fatalf("walking the documentation tree: %v", err)
	}
}

// pagePaths returns every page of the site, slash separated and relative to
// this directory: the root page, then the Markdown under each section
// directory. Nothing else under docs/ is a page, so the documents that predate
// the site are invisible to these tests.
func pagePaths(t *testing.T) []string {
	t.Helper()

	paths := []string{rootPage}
	for _, section := range slices.Sorted(maps.Keys(sections)) {
		err := filepath.WalkDir(section, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !entry.IsDir() && strings.HasSuffix(path, ".md") {
				paths = append(paths, filepath.ToSlash(path))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", section, err)
		}
	}
	return paths
}

// loadPage reads one page and splits its frontmatter from its body. A page
// that does not read at all is a broken checkout rather than a page to report,
// so it fails the run.
func loadPage(t *testing.T, path string) (page, error) {
	t.Helper()

	raw, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	lines := strings.Split(string(raw), "\n")
	if lines[0] != "---" {
		return page{}, errors.New("no frontmatter block")
	}
	for i, line := range lines[1:] {
		if line != "---" {
			continue
		}
		closing := i + 1
		p := page{path: path, body: lines[closing+1:], bodyLine: closing + 2}
		if err := yaml.Unmarshal([]byte(strings.Join(lines[1:closing], "\n")), &p.front); err != nil {
			return page{}, fmt.Errorf("frontmatter: %w", err)
		}
		return p, nil
	}
	return page{}, errors.New("unterminated frontmatter")
}

// headings returns the ATX headings of a page's body. A line inside a fenced
// code block is code, whatever it starts with.
func headings(p page) []heading {
	var found []heading
	fence := ""
	for i, line := range p.body {
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			switch {
			case fence == "":
				fence = m[1]
			case m[1][0] == fence[0] && len(m[1]) >= len(fence):
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			found = append(found, heading{line: p.bodyLine + i, level: len(m[1]), text: m[2]})
		}
	}
	return found
}

// offendingWords returns the words of a heading that break sentence case: a
// first word starting with neither an uppercase letter nor a digit, and a
// later word that is capitalised without carrying a second uppercase letter or
// a digit, such as OpenStack, API or H1, and without being a listed proper
// noun.
func offendingWords(text string, nouns map[string]bool) []string {
	var words []string
	for _, word := range strings.Fields(codeSpanRe.ReplaceAllString(text, "CODE")) {
		if word = strings.Trim(word, punctuation); word != "" {
			words = append(words, word)
		}
	}

	var offending []string
	for i, word := range words {
		first, size := utf8.DecodeRuneInString(word)
		if i == 0 {
			if !unicode.IsUpper(first) && !unicode.IsDigit(first) {
				offending = append(offending, word)
			}
			continue
		}
		if !unicode.IsUpper(first) || nouns[word] {
			continue
		}
		if !strings.ContainsFunc(word[size:], func(r rune) bool { return unicode.IsUpper(r) || unicode.IsDigit(r) }) {
			offending = append(offending, word)
		}
	}
	return offending
}

// properNouns reads the words a heading may capitalise after its first word.
// It is read before any page, so a run that cannot find the file fails instead
// of passing every heading it was meant to judge.
func properNouns(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(properNounsFile)
	if err != nil {
		t.Fatalf("reading %s: %v", properNounsFile, err)
	}

	return parseProperNouns(string(raw))
}

// parseProperNouns reads one word per line, where # starts a comment and a
// blank line is skipped.
func parseProperNouns(raw string) map[string]bool {
	nouns := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if word := strings.TrimSpace(line); word != "" {
			nouns[word] = true
		}
	}
	return nouns
}

// resolveLink turns a sidebar link into the page path it names. A link that is
// not root relative, or that carries an extension, names no page: VitePress
// serves extensionless paths, so both forms reach the reader as a dead link.
func resolveLink(link string) (string, bool) {
	if !strings.HasPrefix(link, "/") || strings.HasSuffix(link, ".md") || strings.HasSuffix(link, ".html") {
		return "", false
	}
	if strings.HasSuffix(link, "/") {
		return strings.TrimPrefix(link, "/") + "index.md", true
	}
	return strings.TrimPrefix(link, "/") + ".md", true
}

// isSectionKey reports whether a sidebar key is the root prefix or one of the
// section prefixes. VitePress matches the key against the URL, so a key naming
// no section renders its groups on no page.
func isSectionKey(key string) bool {
	if key == "/" {
		return true
	}
	name := strings.TrimSuffix(strings.TrimPrefix(key, "/"), "/")
	return sections[name] != "" && key == "/"+name+"/"
}

// srcExclude returns the patterns of the srcExclude array of the VitePress
// configuration, which is the site's own list of the documents that predate
// it. The array is read rather than repeated here so that the two cannot
// disagree: a path dropped from it turns its file into a published page, and
// TestNoMarkdownFileEscapesTheGate has to see that on the same run.
func srcExclude(t *testing.T) []string {
	t.Helper()

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("reading %s: %v", configFile, err)
	}

	array := srcExcludeRe.FindStringSubmatch(string(raw))
	if array == nil {
		t.Fatalf("%s: no srcExclude array", configFile)
	}

	var patterns []string
	for _, match := range quotedRe.FindAllStringSubmatch(array[1], -1) {
		pattern := match[1]
		// isExcluded reads a plain path or a directory glob and nothing else,
		// so a pattern in another shape would silently cover no file.
		if strings.Contains(strings.TrimSuffix(pattern, "/**"), "*") {
			t.Fatalf("%s: srcExclude pattern %q is neither a path nor a <directory>/** glob", configFile, pattern)
		}
		patterns = append(patterns, pattern)
	}
	if len(patterns) == 0 {
		t.Fatalf("%s: srcExclude is empty", configFile)
	}
	return patterns
}

// isExcluded reports whether one of the srcExclude patterns covers a path.
func isExcluded(path string, patterns []string) bool {
	for _, pattern := range patterns {
		directory, glob := strings.CutSuffix(pattern, "/**")
		if glob {
			if strings.HasPrefix(path, directory+"/") {
				return true
			}
			continue
		}
		if path == pattern {
			return true
		}
	}
	return false
}

// The rules above judge a corpus that satisfies them, so the tests below feed
// each rule the input that has to fail it. Without them the corpus tests stay
// green when the rule stops deciding anything.

func TestHeadingsReadPastNestedCodeFences(t *testing.T) {
	p := page{path: "example.md", bodyLine: 1, body: []string{
		"# Page",
		"````markdown",
		"```json",
		"# Fenced by the example",
		"```",
		"## Fenced between two examples",
		"````",
		"## Real heading",
	}}

	var got []string
	for _, h := range headings(p) {
		got = append(got, h.text)
	}
	if want := []string{"Page", "Real heading"}; !slices.Equal(got, want) {
		t.Errorf("headings = %q, want %q", got, want)
	}
}

func TestOffendingWordsJudgesSentenceCase(t *testing.T) {
	for _, tc := range []struct {
		name  string
		text  string
		nouns map[string]bool
		want  []string
	}{
		{name: "sentence case passes", text: "Find your path"},
		{name: "title case is reported", text: "Find Your Path", want: []string{"Your", "Path"}},
		{name: "lowercase first word is reported", text: "find your path", want: []string{"find"}},
		{name: "digit first word passes", text: "3 ways to rate"},
		{name: "acronym passes", text: "The API contract"},
		{name: "camel case passes", text: "Collect from OpenStack"},
		{name: "listed proper noun passes", text: "Running Ceilometer", nouns: map[string]bool{"Ceilometer": true}},
		{name: "unlisted proper noun is reported", text: "Running Ceilometer", want: []string{"Ceilometer"}},
		{name: "code span counts as capitalised", text: "Using `tally export`"},
		{name: "trailing question mark is stripped", text: "Why Diátaxis?", nouns: map[string]bool{"Diátaxis": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := offendingWords(tc.text, tc.nouns); !slices.Equal(got, tc.want) {
				t.Errorf("offendingWords(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

func TestResolveLinkTakesOnlyRootRelativeExtensionlessLinks(t *testing.T) {
	for _, tc := range []struct {
		name string
		link string
		want string
	}{
		{name: "directory link names its index", link: "/how-to/", want: "how-to/index.md"},
		{name: "page link gains the extension", link: "/contributing/authoring-conventions", want: "contributing/authoring-conventions.md"},
		{name: "root link names the root page", link: "/", want: "index.md"},
		{name: "relative link names no page", link: "how-to/"},
		{name: "markdown extension names no page", link: "/alerting.md"},
		{name: "html extension names no page", link: "/how-to/index.html"},
		{name: "external link names no page", link: "https://example.com/how-to/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := resolveLink(tc.link)
			if ok != (tc.want != "") || got != tc.want {
				t.Errorf("resolveLink(%q) = %q, %t, want %q, %t", tc.link, got, ok, tc.want, tc.want != "")
			}
		})
	}
}

func TestParseProperNounsSkipsCommentsAndBlankLines(t *testing.T) {
	nouns := parseProperNouns("# a comment\n\nCeilometer\nGardener # why it is listed\n   \n")

	want := map[string]bool{"Ceilometer": true, "Gardener": true}
	if !maps.Equal(nouns, want) {
		t.Errorf("parseProperNouns = %v, want %v", nouns, want)
	}
}

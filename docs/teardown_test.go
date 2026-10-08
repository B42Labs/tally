// This file holds the tear-down lesson to the directories the other lessons
// export into. Its rm -rf names each of them rather than ~/tally-tutorial as a
// whole, so a directory a lesson starts writing that the line leaves out
// survives the tear-down, and the rmdir after it then tells the reader that
// something the lessons did not write lives in ~/tally-tutorial.
package docs_test

import (
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const teardownPage = "tutorials/tear-down-your-local-tally.md"

// exportDirRe matches a path under ~/tally-tutorial and captures the directory
// directly below it, the one a lesson exports into.
var exportDirRe = regexp.MustCompile(`~/tally-tutorial/([A-Za-z0-9._-]+)`)

func TestTearDownRemovesEveryExportDirectory(t *testing.T) {
	named := map[string]bool{}
	for _, path := range pagePaths(t) {
		if !strings.HasPrefix(path, "tutorials/") || path == teardownPage {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for _, match := range exportDirRe.FindAllStringSubmatch(string(raw), -1) {
			named[match[1]] = true
		}
	}

	raw, err := os.ReadFile(teardownPage)
	if err != nil {
		t.Fatalf("reading %s: %v", teardownPage, err)
	}
	removed := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if operands, ok := strings.CutPrefix(strings.TrimSpace(line), "rm -rf "); ok {
			for _, match := range exportDirRe.FindAllStringSubmatch(operands, -1) {
				removed[match[1]] = true
			}
		}
	}
	if len(removed) == 0 {
		t.Fatalf("%s has no rm -rf line naming a directory under ~/tally-tutorial, so this test would compare against nothing", teardownPage)
	}

	for _, dir := range slices.Sorted(maps.Keys(named)) {
		if !removed[dir] {
			t.Errorf("a lesson names ~/tally-tutorial/%s, which the rm -rf of %s leaves behind", dir, teardownPage)
		}
	}
	for _, dir := range slices.Sorted(maps.Keys(removed)) {
		if !named[dir] {
			t.Errorf("the rm -rf of %s removes ~/tally-tutorial/%s, which no lesson names", teardownPage, dir)
		}
	}
}

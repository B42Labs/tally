package refdoc

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// makeHelpRe matches the help comment above a public target: two hashes, a
// space, the target, a colon and a space, then what it does.
var makeHelpRe = regexp.MustCompile(`^## ([A-Za-z0-9_-]+): (.+?)\s*$`)

// makeRuleRe matches the line a target is defined on: the names at the start of
// the line and the colon their prerequisites follow. One rule may name several
// targets at once. A variable assignment, `NAME := value`, carries a colon of
// its own and defines no target.
var makeRuleRe = regexp.MustCompile(`^([A-Za-z0-9_-]+(?: +[A-Za-z0-9_-]+)*) *:(?:[^=]|$)`)

// MakeTargets renders the public targets of a Makefile: one row per help
// comment of the form `## target: description`, in the order the file writes
// them. A target is public when it carries such a comment; a recipe without
// one is an implementation detail of another target and is not listed. A help
// comment naming something the file defines no rule for is an error: a comment
// that groups the targets below it reads as one, and publishing it would
// document a target `make` does not have.
func MakeTargets(makefile []byte) (string, error) {
	lines := strings.Split(string(makefile), "\n")
	rules := makeRules(lines)

	var rows [][]string
	seen := map[string]bool{}
	for _, line := range lines {
		match := makeHelpRe.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		name := match[1]
		if seen[name] {
			return "", fmt.Errorf("refdoc: make target %q has two help comments", name)
		}
		if !rules[name] {
			return "", fmt.Errorf("refdoc: make help comment %q names no target", name)
		}
		seen[name] = true
		rows = append(rows, []string{code(name), escapePlaceholders(oneLine(match[2]))})
	}
	if len(rows) == 0 {
		return "", errors.New("refdoc: no make targets")
	}
	return table([]string{"Target", "What it does"}, rows), nil
}

// makeRules is every target the file defines a rule for, which is what a help
// comment is read against.
func makeRules(lines []string) map[string]bool {
	rules := map[string]bool{}
	for _, line := range lines {
		match := makeRuleRe.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		for _, name := range strings.Fields(match[1]) {
			rules[name] = true
		}
	}
	return rules
}

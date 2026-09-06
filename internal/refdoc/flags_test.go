package refdoc

import (
	"flag"
	"strings"
	"testing"
	"time"
)

// newFixtureFlagSet builds a flag set carrying one flag per shape the programs
// of this repository declare.
func newFixtureFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("tally-fixture", flag.ContinueOnError)
	fs.Bool("dump", false, "print the notifications the broker delivers as JSON lines")
	fs.String("cloud", "", "the cloud the instances live in, os-prod-eu1 for example")
	fs.String("month", "2026-01", "the calendar month to rate, as YYYY-MM")
	fs.Int("workers", 4, "how many resources are rated at once")
	fs.Duration("timeout", 30*time.Second, "how long one call may take")
	return fs
}

func TestFlags(t *testing.T) {
	got, err := Flags(newFixtureFlagSet())
	if err != nil {
		t.Fatalf("Flags() error = %v, want nil", err)
	}

	assertWant(t, "flags.want.md", got)
}

func TestFlagsRendersEachFlagShape(t *testing.T) {
	got, err := Flags(newFixtureFlagSet())
	if err != nil {
		t.Fatalf("Flags() error = %v, want nil", err)
	}

	for _, want := range []string{
		"| `--dump` | boolean | `false` | print the notifications the broker delivers as JSON lines |",
		"| `--cloud` | string | none | the cloud the instances live in, os-prod-eu1 for example |",
		"| `--month` | string | `2026-01` |",
		"| `--workers` | integer | `4` |",
		"| `--timeout` | duration | `30s` |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the table does not carry %q:\n%s", want, got)
		}
	}
}

func TestFlagsUnquotesTheUsage(t *testing.T) {
	// A backquoted word in the usage names the value the flag takes, which is
	// what the type column then reads and what the description drops the
	// backquotes from.
	fs := flag.NewFlagSet("tally-fixture", flag.ContinueOnError)
	fs.String("out", "", "directory to write `rollup-<key>.json` to")

	got, err := Flags(fs)
	if err != nil {
		t.Fatalf("Flags() error = %v, want nil", err)
	}

	want := "| `--out` | `rollup-<key>.json` | none | directory to write `rollup-<key>.json` to |"
	if !strings.Contains(got, want) {
		t.Errorf("the table does not carry %q:\n%s", want, got)
	}
}

func TestFlagsRendersAPlaceholderAsACodeSpan(t *testing.T) {
	fs := flag.NewFlagSet("tally-fixture", flag.ContinueOnError)
	fs.String("out", "", "directory to write rollup-<key>.json to")

	got, err := Flags(fs)
	if err != nil {
		t.Fatalf("Flags() error = %v, want nil", err)
	}

	if !strings.Contains(got, "directory to write `rollup-<key>.json` to") {
		t.Errorf("the placeholder is not a code span:\n%s", got)
	}
}

func TestFlagsRendersASetWithoutFlags(t *testing.T) {
	got, err := Flags(flag.NewFlagSet("tally-fixture", flag.ContinueOnError))
	if err != nil {
		t.Fatalf("Flags() error = %v, want nil", err)
	}

	if want := "This program takes no flags.\n"; got != want {
		t.Errorf("Flags() = %q, want %q", got, want)
	}
}

func TestFlagsRejectsANilSet(t *testing.T) {
	_, err := Flags(nil)
	if err == nil {
		t.Fatal("Flags(nil) error = nil, want an error")
	}
	if want := "refdoc: nil flag set"; err.Error() != want {
		t.Errorf("Flags(nil) error = %q, want %q", err, want)
	}
}

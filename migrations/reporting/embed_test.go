package reporting_test

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"

	reportingmigrations "github.com/b42labs/tally/migrations/reporting"
)

// TestVersionMatchesTheChain keeps the constant readiness compares against from
// falling behind the files it stands for. A migration added without raising it
// would let a pod serve traffic on a schema its code is newer than.
func TestVersionMatchesTheChain(t *testing.T) {
	names, err := fs.Glob(reportingmigrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("listing the embedded migrations: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("the package embeds no migration")
	}

	var highest int64
	for _, name := range names {
		prefix, _, found := strings.Cut(name, "_")
		if !found {
			t.Fatalf("migration %q does not start with a version prefix", name)
		}
		version, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("migration %q: parsing the version prefix: %v", name, err)
		}
		highest = max(highest, version)
	}

	if highest != reportingmigrations.Version {
		t.Errorf("Version = %d, want %d, the highest embedded migration",
			reportingmigrations.Version, highest)
	}
}

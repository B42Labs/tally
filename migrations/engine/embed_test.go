package engine_test

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"

	enginemigrations "github.com/b42labs/tally/migrations/engine"
)

// TestVersionMatchesTheChain keeps the constant that names the expected schema
// from falling behind the files it stands for. A migration added without
// raising it would let code run against a schema it is newer than.
func TestVersionMatchesTheChain(t *testing.T) {
	names, err := fs.Glob(enginemigrations.FS, "*.sql")
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

	if highest != enginemigrations.Version {
		t.Errorf("Version = %d, want %d, the highest embedded migration",
			enginemigrations.Version, highest)
	}
}

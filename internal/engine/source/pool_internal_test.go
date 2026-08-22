// This file is in the package rather than beside it because what it holds is
// the pool's own configuration, which no exported symbol hands out: a Snapshot
// runs the queries of this package and nothing else, so the bounds the server
// applies are reachable only from here.
package source

import (
	"testing"

	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// TestPoolBoundsWhatAReadHoldsOpen asks the server what it will enforce on a
// connection of the pool. A run reads every candidate through one transaction,
// so an unbounded read holds the vacuum horizon and its locks on the chunks it
// read for as long as it hangs, and the run never fails.
func TestPoolBoundsWhatAReadHoldsOpen(t *testing.T) {
	db, err := New(t.Context(), storetest.NewDB(t).URL)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	t.Cleanup(db.Close)

	conn, err := db.pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquiring a connection: %v", err)
	}
	defer conn.Release()

	// The settings as the server states them, which is the largest unit the
	// value divides by.
	for setting, want := range map[string]string{
		"statement_timeout":                   "5min",
		"idle_in_transaction_session_timeout": "10min",
	} {
		var got string
		if err := conn.QueryRow(t.Context(), "SHOW "+setting).Scan(&got); err != nil {
			t.Fatalf("reading %s: %v", setting, err)
		}
		if got != want {
			t.Errorf("%s = %q, want %q; a read past it holds the snapshot open with nothing to end it",
				setting, got, want)
		}
	}
}

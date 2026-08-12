package reconciliation_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/b42labs/tally/internal/reporting/reconciliation"
)

// errCinderDown stands in for whatever a platform client returns when one of
// its services is unreachable.
var errCinderDown = errors.New("connection refused")

func TestEnumerationError(t *testing.T) {
	err := error(&reconciliation.EnumerationError{
		ResourceType: "storage.volume",
		Err:          errCinderDown,
	})

	t.Run("the message names the type that stayed incomplete", func(t *testing.T) {
		for _, want := range []string{"storage.volume", "connection refused"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Error() = %q, want it to name %q", err, want)
			}
		}
	})

	t.Run("the platform error stays matchable", func(t *testing.T) {
		if !errors.Is(err, errCinderDown) {
			t.Errorf("errors.Is(%v, errCinderDown) = false, want true", err)
		}
	})

	// The sync tells this error apart from every other one to decide whether to
	// skip the type or abort the run, so it has to survive being wrapped by the
	// layers between the adapter and the diff.
	t.Run("a wrapped enumeration error is still recognizable", func(t *testing.T) {
		var enumErr *reconciliation.EnumerationError
		if !errors.As(errors.Join(errors.New("collecting the inventory"), err), &enumErr) {
			t.Fatal("errors.As() = false, want true")
		}
		if enumErr.ResourceType != "storage.volume" {
			t.Errorf("ResourceType = %q, want %q", enumErr.ResourceType, "storage.volume")
		}
	})
}

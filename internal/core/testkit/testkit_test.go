package testkit

import (
	"fmt"
	"testing"
	"time"
)

// recorder stands in for *testing.T so the assertions can be checked for the
// failures they are supposed to report.
type recorder struct {
	failures []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func TestAssertValidEventAcceptsBuilderOutput(t *testing.T) {
	var rec recorder
	assertValidEvent(&rec, NewEventBuilder().BuildJSON(t))

	if len(rec.failures) != 0 {
		t.Errorf("builder output was rejected: %v", rec.failures)
	}
}

func TestAssertValidEventRejectsEnvelopeViolation(t *testing.T) {
	// A create event without a size breaks the envelope rule, which is exactly
	// what a provider collector must not be allowed to ship.
	raw := NewEventBuilder().WithoutSize().BuildJSON(t)

	var rec recorder
	assertValidEvent(&rec, raw)

	if len(rec.failures) == 0 {
		t.Fatal("a create event without a size was accepted")
	}
}

func TestAssertValidEventRejectsMalformedJSON(t *testing.T) {
	var rec recorder
	assertValidEvent(&rec, []byte(`{`))

	if len(rec.failures) == 0 {
		t.Fatal("malformed JSON was accepted")
	}
}

func TestAssertDeterministicIDsAcceptsStableFunction(t *testing.T) {
	var rec recorder
	assertDeterministicIDs(&rec, func() string { return "stable" })

	if len(rec.failures) != 0 {
		t.Errorf("a stable function was rejected: %v", rec.failures)
	}
}

func TestAssertDeterministicIDsRejectsUnstableFunction(t *testing.T) {
	calls := 0
	var rec recorder
	assertDeterministicIDs(&rec, func() string {
		calls++
		return fmt.Sprintf("id-%d", calls)
	})

	if calls != 2 {
		t.Errorf("the function was called %d times, want 2", calls)
	}
	if len(rec.failures) == 0 {
		t.Fatal("an unstable function was accepted")
	}
}

func TestEventBuilderCopiesOnEveryChange(t *testing.T) {
	base := NewEventBuilder()
	derived := base.WithEventID("other")

	if base.Build().EventID == derived.Build().EventID {
		t.Error("With* mutated the base builder instead of returning a copy")
	}
}

func TestEventBuilderStored(t *testing.T) {
	receivedAt := time.Date(2026, time.March, 1, 0, 0, 1, 0, time.UTC)

	stored := NewEventBuilder().BuildStored(receivedAt)
	if !stored.ReceivedAt.Equal(receivedAt) {
		t.Errorf("ReceivedAt = %v, want %v", stored.ReceivedAt, receivedAt)
	}
	if stored.EventID != NewEventBuilder().Build().EventID {
		t.Error("BuildStored changed the event")
	}
}

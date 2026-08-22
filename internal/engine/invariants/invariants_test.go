package invariants_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/engine/invariants"
)

// The period every case is checked over: March 2026.
var (
	feb10 = day(time.February, 10)
	feb15 = day(time.February, 15)
	feb20 = day(time.February, 20)
	mar1  = day(time.March, 1)
	mar5  = day(time.March, 5)
	mar10 = day(time.March, 10)
	mar15 = day(time.March, 15)
	mar16 = day(time.March, 16)
	mar20 = day(time.March, 20)
	apr1  = day(time.April, 1)
	apr15 = day(time.April, 15)

	// A boundary with a sub-second part, which no sum of whole seconds holds.
	mar16Half = mar16.Add(500 * time.Millisecond)

	periodFrom, periodTo = mar1, apr1
)

func day(month time.Month, d int) time.Time {
	return time.Date(2026, month, d, 0, 0, 0, 0, time.UTC)
}

type option func(*event.Stored)

func withState(state string) option {
	return func(s *event.Stored) { s.Payload.State = &state }
}

func withReceivedAt(ts time.Time) option {
	return func(s *event.Stored) { s.ReceivedAt = ts }
}

func withProject(id string) option {
	return func(s *event.Stored) { s.ProjectID = id }
}

func withResource(resourceType, id string) option {
	return func(s *event.Stored) { s.ResourceType, s.ResourceID = resourceType, id }
}

// ev builds a stored event. Defaults keep each case to the one dimension it
// exercises: the same instance of the same project, received at the event time.
func ev(id, eventType string, ts time.Time, opts ...option) event.Stored {
	s := event.Stored{
		Event: event.Event{
			EventID:      id,
			Timestamp:    ts,
			EventType:    eventType,
			Platform:     "openstack",
			Cloud:        "os-prod-eu1",
			ResourceType: "instance",
			ResourceID:   "i-1",
			ProjectID:    "p-1",
			Source:       event.SourceCollector,
		},
		ReceivedAt: ts,
	}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

type spanOption func(*invariants.Span)

func withSeconds(seconds int64) spanOption {
	return func(s *invariants.Span) { s.Seconds = seconds }
}

func withCount(count int) spanOption {
	return func(s *invariants.Span) { s.Count = count }
}

// span builds the usage draft metering derives for an interval: the whole
// seconds of its own duration, and the implicit count of one. The options set
// what a case needs to be wrong.
func span(from, to time.Time, opts ...spanOption) invariants.Span {
	s := invariants.Span{
		From:    from,
		To:      to,
		Seconds: int64(to.Sub(from) / time.Second),
		Count:   1,
	}
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

// invariantsOf reduces a report to the sorted, unique invariant names in it,
// which is what a case asserts on when the number of details does not matter.
func invariantsOf(violations []invariants.Violation) []string {
	names := make([]string, 0, len(violations))
	for _, v := range violations {
		names = append(names, v.Invariant)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func TestCheckAcceptsTheSpansMeteringDerives(t *testing.T) {
	for name, tc := range map[string]struct {
		spans   []invariants.Span
		history []event.Stored
	}{
		"alive for the whole month": {
			spans:   []invariants.Span{span(mar1, apr1)},
			history: []event.Stored{ev("e1", "compute.instance.create.end", feb15, withState("active"))},
		},
		"resized mid-month": {
			spans: []invariants.Span{span(mar1, mar16), span(mar16, apr1)},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", feb15, withState("active")),
				ev("e2", "compute.instance.resize.end", mar16, withState("active")),
			},
		},
		// What traces a boundary is the instant, not how the timestamp was
		// written: the resize below is mar16 stated in another zone.
		"an event of another zone traces the boundary it is the instant of": {
			spans: []invariants.Span{span(mar1, mar16), span(mar16, apr1)},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", feb15, withState("active")),
				ev("e2", "compute.instance.resize.end", mar16.In(time.FixedZone("CET", 3600)),
					withState("active")),
			},
		},
		"created mid-month": {
			spans:   []invariants.Span{span(mar16, apr1)},
			history: []event.Stored{ev("e1", "compute.instance.create.end", mar16, withState("active"))},
		},
		"deleted mid-month": {
			spans: []invariants.Span{span(mar1, mar16)},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", feb15, withState("active")),
				ev("e2", "compute.instance.delete.end", mar16),
			},
		},
		"created and deleted inside the month": {
			spans: []invariants.Span{span(mar10, mar20)},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", mar10, withState("active")),
				ev("e2", "compute.instance.delete.end", mar20),
			},
		},
		// The gap between the delete and the second create is one the spans are
		// not meant to close, so it is no violation.
		"created again after a delete": {
			spans: []invariants.Span{span(mar1, mar10), span(mar20, apr1)},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", mar1, withState("active")),
				ev("e2", "compute.instance.delete.end", mar10),
				ev("e3", "compute.instance.create.end", mar20, withState("active")),
			},
		},
		// The create was missed, so the life starts at the first event there is.
		"history starting with an update": {
			spans:   []invariants.Span{span(mar5, apr1)},
			history: []event.Stored{ev("e1", "compute.instance.power_on", mar5, withState("active"))},
		},
		"transferred to another project mid-month": {
			spans: []invariants.Span{span(mar1, mar16), span(mar16, apr1)},
			history: []event.Stored{
				ev("e1", "volume.create.end", feb15, withState("available"), withResource("volume", "v-1")),
				ev("e2", "volume.transfer.accept.end", mar16, withState("available"),
					withResource("volume", "v-1"), withProject("p-2")),
			},
		},
		// Check sorts the spans itself, so the order they arrive in decides
		// nothing.
		"spans arriving newest first": {
			spans: []invariants.Span{span(mar16, apr1), span(mar1, mar16)},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", feb15, withState("active")),
				ev("e2", "compute.instance.resize.end", mar16, withState("active")),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := invariants.Check(tc.spans, tc.history, periodFrom, periodTo); len(got) != 0 {
				t.Errorf("Check reported %d violations, want none: %+v", len(got), got)
			}
		})
	}
}

func TestCheckSubSecondBoundaries(t *testing.T) {
	history := []event.Stored{
		ev("e1", "compute.instance.create.end", feb15, withState("active")),
		ev("e2", "compute.instance.resize.end", mar16Half, withState("active")),
	}

	t.Run("exact durations still tile the period", func(t *testing.T) {
		spans := []invariants.Span{span(mar1, mar16Half), span(mar16Half, apr1)}
		// Each half of the split loses half a second to truncation, so the two
		// bill 2678399 of the month's 2678400 seconds.
		if spans[0].Seconds != 1296000 || spans[1].Seconds != 1382399 {
			t.Fatalf("seconds = %d, %d, want 1296000, 1382399", spans[0].Seconds, spans[1].Seconds)
		}

		if got := invariants.Check(spans, history, periodFrom, periodTo); len(got) != 0 {
			t.Errorf("Check reported %d violations, want none: %+v", len(got), got)
		}
	})

	t.Run("seconds rounded past the duration break coverage", func(t *testing.T) {
		spans := []invariants.Span{
			span(mar1, mar16Half, withSeconds(1296000)),
			span(mar16Half, apr1, withSeconds(1382400)),
		}

		got := invariantsOf(invariants.Check(spans, history, periodFrom, periodTo))
		if want := []string{invariants.InvariantCoverage}; !slices.Equal(got, want) {
			t.Errorf("invariants = %v, want %v", got, want)
		}
	})
}

func TestCheckReportsViolations(t *testing.T) {
	for name, tc := range map[string]struct {
		spans   []invariants.Span
		history []event.Stored
		want    []string
	}{
		"spans that overlap": {
			spans: []invariants.Span{span(mar1, mar16), span(mar10, apr1)},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", feb15, withState("active")),
				ev("e2", "compute.instance.power_off", mar10, withState("shutoff")),
				ev("e3", "compute.instance.resize.end", mar16, withState("active")),
			},
			want: []string{invariants.InvariantCoverage, invariants.InvariantNoGapsOverlaps},
		},
		"a gap over time the resource lived through": {
			spans: []invariants.Span{span(mar1, mar10), span(mar20, apr1)},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", feb15, withState("active")),
				ev("e2", "compute.instance.power_off", mar10, withState("shutoff")),
				ev("e3", "compute.instance.power_on", mar20, withState("active")),
			},
			want: []string{invariants.InvariantCoverage, invariants.InvariantNoGapsOverlaps},
		},
		// Nothing follows the span, so the missing rest of the month is caught by
		// the totals rather than by a gap.
		"a span missing at the end of the period": {
			spans: []invariants.Span{span(mar1, mar16)},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", feb15, withState("active")),
				ev("e2", "compute.instance.resize.end", mar16, withState("active")),
			},
			want: []string{invariants.InvariantCoverage},
		},
		"seconds that are not the span's own duration": {
			spans: []invariants.Span{span(mar1, mar16, withSeconds(1))},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", feb15, withState("active")),
				ev("e2", "compute.instance.delete.end", mar16),
			},
			want: []string{invariants.InvariantCoverage},
		},
		// An update after the delete reopens an interval the resource was not
		// alive in. The gap starts at the delete, so only the totals catch it.
		"an update after the delete": {
			spans: []invariants.Span{span(mar1, mar10), span(mar20, apr1)},
			history: []event.Stored{
				ev("e1", "compute.instance.create.end", mar1, withState("active")),
				ev("e2", "compute.instance.delete.end", mar10),
				ev("e3", "compute.instance.power_on", mar20, withState("active")),
			},
			want: []string{invariants.InvariantCoverage},
		},
		"spans without any history": {
			spans:   []invariants.Span{span(mar1, mar16), span(mar16, apr1)},
			history: nil,
			want:    []string{invariants.InvariantCoverage, invariants.InvariantTraceability},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := invariantsOf(invariants.Check(tc.spans, tc.history, periodFrom, periodTo))
			if !slices.Equal(got, tc.want) {
				t.Errorf("invariants = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckTraceabilityNamesTheBoundary(t *testing.T) {
	// The two spans tile the month, so only the boundary between them is wrong:
	// no event happened on March 15.
	violations := invariants.Check(
		[]invariants.Span{span(mar1, mar15), span(mar15, apr1)},
		[]event.Stored{ev("e1", "compute.instance.create.end", feb15, withState("active"))},
		periodFrom, periodTo)

	got := invariantsOf(violations)
	if want := []string{invariants.InvariantTraceability}; !slices.Equal(got, want) {
		t.Fatalf("invariants = %v, want %v", got, want)
	}
	if !slices.ContainsFunc(violations, func(v invariants.Violation) bool {
		return strings.Contains(v.Detail, "2026-03-15T00:00:00Z")
	}) {
		t.Errorf("no detail names the boundary 2026-03-15T00:00:00Z: %+v", violations)
	}
}

func TestCheckReportsEveryCountBreach(t *testing.T) {
	violations := invariants.Check(
		[]invariants.Span{span(mar1, mar16, withCount(0)), span(mar16, apr1, withCount(2))},
		[]event.Stored{
			ev("e1", "compute.instance.create.end", feb15, withState("active")),
			ev("e2", "compute.instance.resize.end", mar16, withState("active")),
		},
		periodFrom, periodTo)

	got := invariantsOf(violations)
	if want := []string{invariants.InvariantImplicitCount}; !slices.Equal(got, want) {
		t.Fatalf("invariants = %v, want %v", got, want)
	}
	if len(violations) != 2 {
		t.Errorf("got %d violations, want one per span: %+v", len(violations), violations)
	}
}

func TestCheckReportsEveryBreach(t *testing.T) {
	// One report carries every breach, so a failed run is diagnosed at once.
	violations := invariants.Check(
		[]invariants.Span{span(mar1, mar16), span(mar10, apr1, withCount(3))},
		[]event.Stored{
			ev("e1", "compute.instance.create.end", feb15, withState("active")),
			ev("e2", "compute.instance.power_off", mar10, withState("shutoff")),
			ev("e3", "compute.instance.resize.end", mar16, withState("active")),
		},
		periodFrom, periodTo)

	got := invariantsOf(violations)
	for _, want := range []string{invariants.InvariantNoGapsOverlaps, invariants.InvariantImplicitCount} {
		if !slices.Contains(got, want) {
			t.Errorf("invariants = %v, want %q among them", got, want)
		}
	}
	if len(violations) < 2 {
		t.Errorf("got %d violations, want one per breach: %+v", len(violations), violations)
	}
}

func TestCheckWithoutSpans(t *testing.T) {
	for name, tc := range map[string]struct {
		history []event.Stored
		want    []string
	}{
		"no history at all": {history: nil},
		"a life that ended before the period": {history: []event.Stored{
			ev("e1", "compute.instance.create.end", feb10, withState("active")),
			ev("e2", "compute.instance.delete.end", feb20),
		}},
		"a life that starts after the period": {history: []event.Stored{
			ev("e1", "compute.instance.create.end", apr15, withState("active")),
		}},
		// The create predates what the events table still holds, so the delete
		// is the only event there is: the life opens and closes at the same
		// instant, which bills nothing and breaches nothing.
		"a history that is nothing but a delete": {history: []event.Stored{
			ev("e1", "compute.instance.delete.end", mar10),
		}},
		"a life covering the period": {
			history: []event.Stored{ev("e1", "compute.instance.create.end", feb15, withState("active"))},
			want:    []string{invariants.InvariantCoverage},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := invariantsOf(invariants.Check(nil, tc.history, periodFrom, periodTo))
			if !slices.Equal(got, tc.want) {
				t.Errorf("invariants = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckDerivesLivesInTimelineOrder(t *testing.T) {
	// Both events carry the same timestamp and arrive delete first, so only the
	// receipt order decides whether the resource ends the instant alive or dead.
	t.Run("the delete received last closes the life", func(t *testing.T) {
		history := []event.Stored{
			ev("e-delete", "compute.instance.delete.end", mar10, withReceivedAt(mar10.Add(time.Hour))),
			ev("e-create", "compute.instance.create.end", mar10, withState("active"), withReceivedAt(mar10)),
		}

		if got := invariants.Check(nil, history, periodFrom, periodTo); len(got) != 0 {
			t.Errorf("Check reported %d violations, want none: %+v", len(got), got)
		}
	})

	t.Run("the create received last reopens the life", func(t *testing.T) {
		history := []event.Stored{
			ev("e-delete", "compute.instance.delete.end", mar10, withReceivedAt(mar10)),
			ev("e-create", "compute.instance.create.end", mar10, withState("active"),
				withReceivedAt(mar10.Add(time.Hour))),
		}

		got := invariantsOf(invariants.Check(nil, history, periodFrom, periodTo))
		if want := []string{invariants.InvariantCoverage}; !slices.Equal(got, want) {
			t.Errorf("invariants = %v, want %v (the resource lives on past the delete)", got, want)
		}
	})
}

func TestViolationJSONShape(t *testing.T) {
	// The report is serialized into runs.stats, so the field names are part of
	// what an operator reads.
	got, err := json.Marshal(invariants.Violation{Invariant: invariants.InvariantCoverage, Detail: "x"})
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}
	if want := `{"invariant":"coverage","detail":"x"}`; string(got) != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
}

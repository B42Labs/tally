// Package invariants holds the usage drafts metering derives for one resource
// and one billing period to the four checks they must pass before anything is
// persisted: no gaps or overlaps, coverage, traceability, and an implicit count
// of one. A breach fails the whole run with a report of every violation found,
// so a run never writes usage that is silently wrong (roadmap WP 3.3).
//
// The checks are computed independently of timeline.Build: the lives a resource
// had are derived here from the categories of its events, not from the fold the
// drafts came out of, so that a bug in the fold cannot hide behind a check
// derived from the same fold.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.3.
package invariants

import (
	"fmt"
	"slices"
	"time"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/timeline"
)

const (
	// InvariantNoGapsOverlaps is breached when two spans of a resource overlap,
	// or when they leave a gap over time the resource lived through.
	InvariantNoGapsOverlaps = "no_gaps_overlaps"
	// InvariantCoverage is breached when the spans bill more or less time than
	// the resource lived through the period, or when a span's seconds are not
	// the whole seconds of its own duration.
	InvariantCoverage = "coverage"
	// InvariantTraceability is breached when a span boundary is neither a period
	// edge nor the timestamp of an event in the resource's history.
	InvariantTraceability = "traceability"
	// InvariantImplicitCount is breached when a span carries a count other than
	// one, the count existence pricing bills a resource by.
	InvariantImplicitCount = "implicit_count"
)

// Span is one usage draft reduced to what the checks read: the half-open
// interval [From, To) it bills, the whole seconds it bills over that interval,
// and its count metric.
type Span struct {
	From, To time.Time
	Seconds  int64
	Count    int
}

// Violation is one breached invariant. Invariant is one of the constants of
// this package, Detail a sentence naming the instants involved, which reaches
// an operator in the run's violation report rather than the code.
type Violation struct {
	Invariant string `json:"invariant"`
	Detail    string `json:"detail"`
}

// life is a half-open [start, end) interval a resource existed over, with end
// nil while it still exists.
type life struct {
	start time.Time
	end   *time.Time
}

// Check holds the usage drafts of one resource against the four invariants over
// the period [periodFrom, periodTo). The spans may arrive in any order; Check
// sorts a clone of them by From. The history is the resource's full event
// history, which Check reads both for the lives the resource had and for the
// timestamps a span boundary is traced to.
//
// It returns one violation per breach rather than the first one, so a failed run
// is diagnosed from a single report, and nothing at all when the drafts hold.
func Check(spans []Span, history []event.Stored, periodFrom, periodTo time.Time) []Violation {
	ordered := slices.Clone(spans)
	slices.SortStableFunc(ordered, func(a, b Span) int { return a.From.Compare(b.From) })
	lives := livesOf(history)

	var violations []Violation
	violations = append(violations, checkGapsOverlaps(ordered, lives)...)
	violations = append(violations, checkCoverage(ordered, lives, periodFrom, periodTo)...)
	violations = append(violations, checkTraceability(ordered, history, periodFrom, periodTo)...)
	violations = append(violations, checkImplicitCount(ordered)...)
	return violations
}

// livesOf derives the intervals a resource existed over from the categories of
// its events. The history may arrive in any order and is sorted the way every
// consumer folding a history sorts it, by timeline.Compare.
//
// The first event opens a life whatever its category: a history starting with an
// update or a delete has lost its create, and the earliest instant the resource
// is known to have existed at is the one it is known from. A first event that is
// a delete opens and closes a life at the same instant, which bills nothing. A
// delete closes the open life, a create that follows one opens the next, and an
// update opens nothing.
//
// A resource deleted and created again inside a period therefore has two lives.
// Reading the lifetime as the single interval [start, deleted_at or ∞) the
// roadmap names would fail such a resource for the gap between them, a gap its
// drafts are right to leave because timeline.Build reopens the timeline on that
// second create on purpose.
func livesOf(history []event.Stored) []life {
	ordered := slices.Clone(history)
	slices.SortStableFunc(ordered, timeline.Compare)

	var (
		lives []life
		open  bool
	)
	for i, e := range ordered {
		category := event.Categorize(e.EventType)

		if i == 0 && category != event.CategoryCreate {
			lives = append(lives, life{start: e.Timestamp})
			open = true
		}
		switch category {
		case event.CategoryCreate:
			if !open {
				lives = append(lives, life{start: e.Timestamp})
				open = true
			}
		case event.CategoryDelete:
			if open {
				end := e.Timestamp
				lives[len(lives)-1].end = &end
				open = false
			}
		case event.CategoryUpdate:
		}
	}
	return lives
}

// checkGapsOverlaps reports every pair of consecutive spans that does not meet.
// The two directions are not held to the same test: an overlap bills an instant
// twice wherever it falls, while a gap is only wrong over time the resource
// lived through, because a delete and a later create leave one the spans are not
// meant to close.
func checkGapsOverlaps(spans []Span, lives []life) []Violation {
	var violations []Violation
	for i := 0; i+1 < len(spans); i++ {
		current, next := spans[i], spans[i+1]
		switch {
		case current.To.After(next.From):
			violations = append(violations, Violation{
				Invariant: InvariantNoGapsOverlaps,
				Detail: fmt.Sprintf("the span ending %s overlaps the next one starting %s",
					instant(current.To), instant(next.From)),
			})
		case current.To.Before(next.From) && livedAt(lives, current.To):
			violations = append(violations, Violation{
				Invariant: InvariantNoGapsOverlaps,
				Detail: fmt.Sprintf("the span ending %s does not meet the next one starting %s while the resource lived",
					instant(current.To), instant(next.From)),
			})
		}
	}
	return violations
}

// checkCoverage reports the spans billing more or less time than the resource
// lived through the period, and every span whose seconds are not the whole
// seconds of its own duration.
//
// The totals are compared as durations rather than as the roadmap's sum of
// seconds. Event timestamps may carry sub-second parts, and truncating each span
// on its own loses up to a second per split, which would fail a set of drafts
// that tiles the period exactly. Whenever every boundary falls on a whole
// second, the comparison is the roadmap's formula.
func checkCoverage(spans []Span, lives []life, periodFrom, periodTo time.Time) []Violation {
	var violations []Violation

	var billed time.Duration
	for _, span := range spans {
		billed += span.To.Sub(span.From)
	}
	var lived time.Duration
	for _, l := range lives {
		end := periodTo
		if l.end != nil {
			end = *l.end
		}
		lived += overlap(l.start, end, periodFrom, periodTo)
	}
	if billed != lived {
		violations = append(violations, Violation{
			Invariant: InvariantCoverage,
			Detail:    fmt.Sprintf("the spans cover %s, the resource lived %s of the period", billed, lived),
		})
	}

	for _, span := range spans {
		if want := int64(span.To.Sub(span.From) / time.Second); span.Seconds != want {
			violations = append(violations, Violation{
				Invariant: InvariantCoverage,
				Detail: fmt.Sprintf("the span %s..%s carries %d seconds, its duration has %d",
					instant(span.From), instant(span.To), span.Seconds, want),
			})
		}
	}
	return violations
}

// checkTraceability reports every span boundary that is neither a period edge
// nor the timestamp of an event: a boundary no event stands behind is one the
// fold invented.
//
// The history is indexed once rather than scanned per boundary. Both operands
// grow with the event count — a resource that flaps between two states yields
// one span per event — so scanning would cost the square of a history nothing
// bounds, and it would cost it on the path where the drafts are correct.
func checkTraceability(spans []Span, history []event.Stored, periodFrom, periodTo time.Time) []Violation {
	// The key is the instant, which is what Time.Equal compares: the same
	// instant in another zone, or carrying a monotonic reading, keys the same.
	timestamps := make(map[int64]struct{}, len(history))
	for _, e := range history {
		timestamps[e.Timestamp.UnixNano()] = struct{}{}
	}

	var violations []Violation
	for _, span := range spans {
		for _, boundary := range []struct{ ts, edge time.Time }{{span.From, periodFrom}, {span.To, periodTo}} {
			if boundary.ts.Equal(boundary.edge) {
				continue
			}
			if _, traced := timestamps[boundary.ts.UnixNano()]; traced {
				continue
			}
			violations = append(violations, Violation{
				Invariant: InvariantTraceability,
				Detail:    fmt.Sprintf("the span boundary %s matches no event", instant(boundary.ts)),
			})
		}
	}
	return violations
}

// checkImplicitCount reports every span that does not carry a count of one. The
// count is what prices a resource by its existence, such as a floating ip, so a
// span carrying anything else bills the wrong number of resources.
func checkImplicitCount(spans []Span) []Violation {
	var violations []Violation
	for _, span := range spans {
		if span.Count != 1 {
			violations = append(violations, Violation{
				Invariant: InvariantImplicitCount,
				Detail: fmt.Sprintf("the span %s..%s carries count %d, want 1",
					instant(span.From), instant(span.To), span.Count),
			})
		}
	}
	return violations
}

// livedAt reports whether any life covers ts. The lives are half-open, so the
// instant a life ends at is one the resource no longer existed in.
//
// A resource has one life per create/delete pair of its history, a handful at
// most, so they are scanned rather than searched.
func livedAt(lives []life, ts time.Time) bool {
	for _, l := range lives {
		if !l.start.After(ts) && (l.end == nil || ts.Before(*l.end)) {
			return true
		}
	}
	return false
}

// overlap returns how much of [fromA, toA) falls into [fromB, toB), and zero
// when the two do not meet.
func overlap(fromA, toA, fromB, toB time.Time) time.Duration {
	from, to := fromA, toA
	if fromB.After(from) {
		from = fromB
	}
	if toB.Before(to) {
		to = toB
	}
	if !to.After(from) {
		return 0
	}
	return to.Sub(from)
}

// instant formats a timestamp for a violation detail. The scale is the one the
// events carry, so a boundary with a sub-second part shows it.
func instant(ts time.Time) string {
	return ts.Format(time.RFC3339Nano)
}

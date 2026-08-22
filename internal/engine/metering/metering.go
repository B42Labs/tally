// Package metering folds each candidate resource's event history into the usage
// drafts of one billing period: the intervals the fold implies, clipped to the
// period, carrying the quantities every later number is derived from. It
// persists nothing. Writing the usage rows and the run's stats belongs to a
// later package, so a period can be metered without a handle on the database it
// will be written to.
//
// Every draft of a resource is held against internal/engine/invariants before it
// is handed out. A breach fails the whole resource set with a report of every
// violating resource rather than letting one wrong resource through.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.3.
package metering

import (
	"context"
	"fmt"
	"time"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/core/timeline"
	"github.com/b42labs/tally/internal/engine/invariants"
	"github.com/b42labs/tally/internal/engine/source"
)

// WarningCandidateWithoutHistory marks a candidate the projection lists whose
// events are missing or all lie at or after the period end. It is metered as
// nothing, because there is no history to bill it from.
const WarningCandidateWithoutHistory = "candidate_without_history"

// The two usage fields the engine derives rather than reads: the minutes an
// interval's seconds are worth, and the count that prices a resource by its
// existence. The names are reserved. Nothing keeps a provider from reporting a
// size field of either name — every seeded size schema takes additional
// properties, and operators register their own — and a size that does is
// refused rather than merged: the engine's value would drop a listener or
// object count the customer is billed for, and the provider's would bill a
// quantity nothing here derived.
const (
	usageMinutes = "minutes"
	usageCount   = "count"
)

// InvariantReservedUsageField is what a resource whose size carries one of
// those names is reported as. It stands beside the invariants package's own
// codes in a run's violation report rather than in that package: such a
// resource has no drafts to hold against an invariant, so the breach is found
// here, one step before invariants.Check runs.
const InvariantReservedUsageField = "reserved_usage_field"

// UsageDraft is one interval of a resource clipped to the billing period.
// Seconds is the whole seconds of [FromTS, ToTS), a sub-second remainder
// truncated (D2), and Usage is decision D9's object: every size field of the
// interval verbatim, plus the minutes those seconds are worth and the count of
// one that prices a resource by its existence.
type UsageDraft struct {
	State, ProjectID string
	FromTS, ToTS     time.Time
	Seconds          int64
	Usage            map[string]any
}

// Source is what metering reads a period from, implemented by *source.Snapshot.
// The interface lives here rather than next to that type so the loop below can
// be exercised without a database.
type Source interface {
	Candidates(ctx context.Context, clouds []string, periodFrom, periodTo time.Time) ([]source.Resource, error)
	History(ctx context.Context, r source.Resource, periodTo time.Time) ([]event.Stored, error)
}

var _ Source = (*source.Snapshot)(nil)

// Result is what one metering pass produced.
type Result struct {
	// Candidates counts every candidate examined, including those that yielded
	// no drafts.
	Candidates int
	Resources  []ResourceUsage
	Warnings   []Warning
}

// ResourceUsage is one resource and the drafts the period bills it as.
type ResourceUsage struct {
	Resource source.Resource
	Drafts   []UsageDraft
}

// Warning names a resource that was metered on incomplete data. Code is
// WarningCandidateWithoutHistory or timeline.WarningHistoryStartsWithoutCreate.
// A warning does not fail the run: it reaches an operator through the run's
// stats.
type Warning struct {
	Cloud        string `json:"cloud"`
	ResourceType string `json:"resource_type"`
	ResourceID   string `json:"resource_id"`
	Code         string `json:"code"`
}

// ViolationError is what Meter fails a period with: every resource the period
// cannot be billed from, and nothing else. No drafts come back with it, because
// a run that found one wrong resource keeps no partial output.
type ViolationError struct {
	Resources []ResourceViolations
}

// Error reports how many resources breached an invariant. The breaches
// themselves are in Resources, which the run writes to its stats.
func (e *ViolationError) Error() string {
	return fmt.Sprintf("%d resources violate the metering invariants", len(e.Resources))
}

// ResourceViolations is one resource and the metering rules it breached: the
// invariants its drafts failed, or the reserved usage field that stopped the
// drafts from being built at all.
type ResourceViolations struct {
	Cloud        string                 `json:"cloud"`
	ResourceType string                 `json:"resource_type"`
	ResourceID   string                 `json:"resource_id"`
	Violations   []invariants.Violation `json:"violations"`
}

// MeterResource folds one resource's history into the drafts the period
// [periodFrom, periodTo) bills it as. It is the whole of what metering computes;
// Meter is this over a source's candidate list.
//
// A history that is empty, or a period with no length, bills nothing and yields
// an empty slice. An interval whose size carries one of the reserved usage
// fields is an error rather than a draft.
func MeterResource(events []event.Stored, periodFrom, periodTo time.Time) ([]UsageDraft, error) {
	return draftsOf(timeline.Build(events), periodFrom, periodTo)
}

// Meter folds every candidate of the period into its drafts. The candidates of
// the named clouds are read from src, and each one's history is loaded, folded,
// and checked on its own: the loop runs sequentially over the one connection the
// snapshot holds and holds a single resource's events at a time, which is what
// lets a resource with years of history be metered at all. The drafts are the
// other half of the footprint, and they are all kept until Meter returns: the
// pass hands out every resource's usage or none of it, so there is nothing it
// could hand on early.
//
// A resource whose drafts breach an invariant is collected rather than returned
// on, so that the error names every violating resource of the period. Any
// violation fails the whole pass: the error is a *ViolationError and no drafts
// come back with it.
//
// The errors of src are returned unchanged, so a caller reads a canceled context
// or a failed query as the source reported it. A resource whose size carries one
// of the reserved usage fields is collected the same way a breached invariant
// is: it is a resource nothing can bill correctly, and one such size persists in
// the events table and reaches every later period, so a run that named one of
// them and stopped would have an operator fix the period's resources one run at
// a time.
func Meter(ctx context.Context, src Source, periodFrom, periodTo time.Time, clouds []string) (*Result, error) {
	candidates, err := src.Candidates(ctx, clouds, periodFrom, periodTo)
	if err != nil {
		return nil, err
	}

	result := &Result{Candidates: len(candidates), Resources: make([]ResourceUsage, 0, len(candidates))}
	var violations []ResourceViolations

	for _, r := range candidates {
		history, err := src.History(ctx, r, periodTo)
		if err != nil {
			return nil, err
		}

		if len(history) == 0 {
			result.Warnings = append(result.Warnings, warningOf(r, WarningCandidateWithoutHistory))
			continue
		}

		tl := timeline.Build(history)
		for _, code := range tl.Warnings {
			result.Warnings = append(result.Warnings, warningOf(r, code))
		}

		drafts, err := draftsOf(tl, periodFrom, periodTo)
		if err != nil {
			violations = append(violations, ResourceViolations{
				Cloud:        r.Cloud,
				ResourceType: r.ResourceType,
				ResourceID:   r.ResourceID,
				Violations: []invariants.Violation{
					{Invariant: InvariantReservedUsageField, Detail: err.Error()},
				},
			})
			continue
		}
		spans := make([]invariants.Span, 0, len(drafts))
		for _, draft := range drafts {
			spans = append(spans, invariants.Span{
				From:    draft.FromTS,
				To:      draft.ToTS,
				Seconds: draft.Seconds,
				Count:   countOf(draft.Usage),
			})
		}
		if breached := invariants.Check(spans, history, periodFrom, periodTo); len(breached) > 0 {
			violations = append(violations, ResourceViolations{
				Cloud:        r.Cloud,
				ResourceType: r.ResourceType,
				ResourceID:   r.ResourceID,
				Violations:   breached,
			})
			continue
		}

		// A resource whose intervals all fall outside the period was examined
		// and bills nothing, so it is counted but carries no usage.
		if len(drafts) > 0 {
			result.Resources = append(result.Resources, ResourceUsage{Resource: r, Drafts: drafts})
		}
	}

	if len(violations) > 0 {
		return nil, &ViolationError{Resources: violations}
	}
	return result, nil
}

// draftsOf turns the intervals of a folded history into drafts. Meter folds a
// history once and reads it both for its warnings and through here, so a draft
// is built the same way whichever entry point produced it.
//
// Its only error is an interval whose size carries one of the reserved usage
// fields, which is what Meter reports as InvariantReservedUsageField.
func draftsOf(tl timeline.Timeline, periodFrom, periodTo time.Time) ([]UsageDraft, error) {
	drafts := make([]UsageDraft, 0, len(tl.Intervals))

	for _, iv := range tl.Intervals {
		start, end, ok := clip(iv, periodFrom, periodTo)
		if !ok {
			continue
		}
		seconds := int64(end.Sub(start) / time.Second)

		for _, reserved := range []string{usageMinutes, usageCount} {
			if _, taken := iv.Size[reserved]; taken {
				return nil, fmt.Errorf("the size of the interval starting %s carries the reserved usage field %q",
					iv.Start.Format(time.RFC3339Nano), reserved)
			}
		}

		usage := make(map[string]any, len(iv.Size)+2)
		// The size is copied as the payload envelope decoded it, which leaves a
		// JSON number a float64. Nothing here computes with those values, and
		// encoding/json writes a float64 of 4 back out as 4, so the size a
		// customer sees is the size the provider sent.
		for key, value := range iv.Size {
			usage[key] = value
		}
		usage[usageMinutes] = money.NewQuantity(money.Minutes(seconds))
		usage[usageCount] = 1

		drafts = append(drafts, UsageDraft{
			State:     iv.State,
			ProjectID: iv.ProjectID,
			FromTS:    start,
			ToTS:      end,
			Seconds:   seconds,
			Usage:     usage,
		})
	}

	return drafts, nil
}

// clip cuts an interval down to the part of it that lies in the period. An
// interval that started earlier is billed from the period's start, and one that
// is still open, or ends later, is billed to the period's end. ok is false when
// nothing of the interval is left, which is an interval outside the period or
// one clipped to no length.
//
// It is the single place a boundary is decided, so MeterResource and Meter
// cannot disagree on where a draft begins.
func clip(iv timeline.Interval, periodFrom, periodTo time.Time) (start, end time.Time, ok bool) {
	start = iv.Start
	if start.Before(periodFrom) {
		start = periodFrom
	}
	end = periodTo
	if iv.End != nil && iv.End.Before(periodTo) {
		end = *iv.End
	}
	return start, end, end.After(start)
}

// countOf reads the count metric out of a usage object. A missing count, or one
// that is not an int, reports zero, which is a count the implicit count
// invariant rejects rather than one that quietly passes as one.
func countOf(usage map[string]any) int {
	count, _ := usage[usageCount].(int)
	return count
}

// warningOf names a resource in a warning.
func warningOf(r source.Resource, code string) Warning {
	return Warning{
		Cloud:        r.Cloud,
		ResourceType: r.ResourceType,
		ResourceID:   r.ResourceID,
		Code:         code,
	}
}

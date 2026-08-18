package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/core/timeline"
)

const (
	// minutePlaces is the scale minute quantities are rendered at. It matches
	// what money.Minutes rounds to, so the output shows the quantity that was
	// rated rather than a shortened one.
	minutePlaces = 4
	// minutesPerHour turns a minute quantity into the hours prices are given in.
	minutesPerHour = 60
)

// period is the half-open span [From, To) usage is reported for.
type period struct {
	From, To time.Time
}

// parseMonth reads a calendar month as YYYY-MM and returns the UTC span it
// covers. The end is the first instant of the next month, so a month is
// expressed the same way an interval is and the two clip against each other
// without a special case for the last second of the month.
func parseMonth(value string) (period, error) {
	from, err := time.Parse("2006-01", value)
	if err != nil {
		return period{}, fmt.Errorf("reading the month %q: %w", value, err)
	}
	return period{From: from, To: from.AddDate(0, 1, 0)}, nil
}

// usageRecord is one interval clipped to the reported period. Seconds is what
// the clipping produced, and Minutes is the billable quantity derived from it:
// the duration is kept as integer seconds up to that single conversion.
type usageRecord struct {
	From, To time.Time
	State    string
	Size     map[string]any
	Seconds  int64
	Minutes  decimal.Decimal
}

// buildRecords clips a resource's intervals to the period. An interval that
// started before the period is billed from the period's start, and an open one
// is billed to its end: the month is what is being reported, so nothing outside
// it may reach the output.
func buildRecords(tl timeline.Timeline, p period) []usageRecord {
	records := make([]usageRecord, 0, len(tl.Intervals))

	for _, interval := range tl.Intervals {
		start := interval.Start
		if start.Before(p.From) {
			start = p.From
		}
		end := p.To
		if interval.End != nil && interval.End.Before(p.To) {
			end = *interval.End
		}
		// An interval that lies entirely outside the period clips to nothing,
		// and an empty span bills nothing, so neither becomes a record.
		if !end.After(start) {
			continue
		}

		seconds := int64(end.Sub(start) / time.Second)
		records = append(records, usageRecord{
			From:    start.UTC(),
			To:      end.UTC(),
			State:   interval.State,
			Size:    interval.Size,
			Seconds: seconds,
			Minutes: money.Minutes(seconds),
		})
	}

	return records
}

// sizeDecimal reads one metric out of a size object as a decimal. The size
// arrives as decoded JSON, where a number is a float64, so the value is taken
// back to its JSON literal and parsed from there: that keeps the float out of
// the money path, which roadmap/00-conventions.md section 6 forbids it from.
func sizeDecimal(size map[string]any, metric string) (decimal.Decimal, error) {
	value, ok := size[metric]
	if !ok {
		return decimal.Decimal{}, fmt.Errorf("the size carries no %s", metric)
	}

	literal, err := json.Marshal(value)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("encoding the size value of %s: %w", metric, err)
	}
	quantity, err := decimal.NewFromString(string(literal))
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("reading the size value of %s as a number: %w", metric, err)
	}
	return quantity, nil
}

// rate prices one record against every configured dimension, and returns its
// line items keyed by metric together with their subtotal. Each dimension is
// computed at full precision and rounded once at the end, and the subtotal is
// the sum of those rounded costs, so the subtotal always equals the line items
// the output shows.
//
// A state the pricing names no modifier for is billed at the full price, which
// is what an active instance is.
func rate(rec usageRecord, pr pricing) (map[string]dimensionDoc, decimal.Decimal, error) {
	modifier := decimal.NewFromInt(1)
	if factor, ok := pr.StateModifiers[rec.State]; ok {
		modifier = factor
	}
	hours := money.Div(rec.Minutes, decimal.NewFromInt(minutesPerHour))

	dimensions := make(map[string]dimensionDoc, len(pr.Dimensions))
	subtotal := decimal.Zero
	for _, dim := range pr.Dimensions {
		quantity, err := sizeDecimal(rec.Size, dim.Metric)
		if err != nil {
			return nil, decimal.Zero, fmt.Errorf("rating the interval starting %s: %w",
				rec.From.Format(time.RFC3339), err)
		}

		cost := money.Round2(hours.Mul(quantity).Mul(dim.PricePerUnitHour).Mul(modifier))
		dimensions[dim.Metric] = dimensionDoc{
			Quantity: jsonQuantity{Decimal: quantity},
			Cost:     money.NewAmount(cost),
		}
		subtotal = subtotal.Add(cost)
	}

	return dimensions, subtotal, nil
}

// checkInvariants holds the derived records against what metering promises:
// they stay inside the period, each of them bills time, and together they tile
// the part of the period the resource lived through without a gap or an
// overlap. It returns one line per breach rather than the first, so a broken
// run is diagnosed from one output.
//
// How many seconds the records bill in total is not among the checks. The only
// figures an expectation could be derived from are the interval boundaries the
// clipping already read, so a comparison against it would restate the clipping
// rather than hold it to anything.
func checkInvariants(records []usageRecord, tl timeline.Timeline, p period) []string {
	violations := []string{}

	ordered := slices.Clone(records)
	slices.SortStableFunc(ordered, func(a, b usageRecord) int { return a.From.Compare(b.From) })

	for i, rec := range ordered {
		if rec.From.Before(p.From) || rec.To.After(p.To) {
			violations = append(violations, fmt.Sprintf("the record %s..%s lies outside the period %s..%s",
				rec.From.Format(time.RFC3339), rec.To.Format(time.RFC3339),
				p.From.Format(time.RFC3339), p.To.Format(time.RFC3339)))
		}
		if !rec.To.After(rec.From) {
			violations = append(violations, fmt.Sprintf("the record %s..%s bills no time",
				rec.From.Format(time.RFC3339), rec.To.Format(time.RFC3339)))
		}
		// Two records that do not meet have lost or double-billed the time
		// between them. The two directions are not held to the same test: an
		// overlap bills a second twice wherever it falls, while a gap is only
		// wrong over time the resource lived through, because a delete and a
		// later create leave one the records are not meant to close and
		// reporting it would fail a run whose numbers are right.
		if i+1 < len(ordered) {
			next := ordered[i+1]
			switch {
			case rec.To.After(next.From):
				violations = append(violations, fmt.Sprintf("the record ending %s overlaps the next one starting %s",
					rec.To.Format(time.RFC3339), next.From.Format(time.RFC3339)))
			case rec.To.Before(next.From) && existedAt(tl, rec.To):
				violations = append(violations, fmt.Sprintf("the record ending %s does not meet the next one starting %s",
					rec.To.Format(time.RFC3339), next.From.Format(time.RFC3339)))
			}
		}
	}

	return violations
}

// existedAt reports whether the resource was alive at ts, which is whether any
// interval of its timeline covers that instant. The intervals are half-open, so
// the instant a life ends at is one the resource no longer existed in.
//
// Build emits the intervals in ascending order and never overlapping, so the
// only one that can cover ts is the last one starting at or before it. It is
// found by search rather than by a scan: a history whose every record boundary
// is a lifecycle gap asks this once per record, and scanning would make the
// invariant check quadratic over a history the API served in one answer.
func existedAt(tl timeline.Timeline, ts time.Time) bool {
	i, found := slices.BinarySearchFunc(tl.Intervals, ts,
		func(interval timeline.Interval, ts time.Time) int { return interval.Start.Compare(ts) })
	if !found {
		i--
	}
	if i < 0 {
		return false
	}
	interval := tl.Intervals[i]
	return interval.End == nil || ts.Before(*interval.End)
}

// jsonMinutes is a minute quantity that serializes at its full scale. A bare
// decimal renders 14400.0000 as 14400, which hides the scale the quantity was
// rated at.
type jsonMinutes struct {
	decimal.Decimal
}

// MarshalJSON renders the quantity as a JSON number with four decimal places.
func (m jsonMinutes) MarshalJSON() ([]byte, error) {
	return []byte(m.StringFixed(minutePlaces)), nil
}

// jsonQuantity is a size quantity that serializes as the number it is, without
// the quotes a decimal marshals itself with.
type jsonQuantity struct {
	decimal.Decimal
}

// MarshalJSON renders the quantity as a JSON number at the scale it carries.
func (q jsonQuantity) MarshalJSON() ([]byte, error) {
	return []byte(q.String()), nil
}

// document is the slice's whole output: one project's rated usage for one
// month.
type document struct {
	Cloud     string        `json:"cloud"`
	ProjectID string        `json:"project_id"`
	Period    periodDoc     `json:"period"`
	Currency  string        `json:"currency"`
	Resources []resourceDoc `json:"resources"`
	Total     money.Amount  `json:"total"`
}

// periodDoc is the reported span, half-open as everywhere else.
type periodDoc struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// resourceDoc is one resource's rated usage. Warnings come from the fold and
// violations from the invariant checks: both are reported next to the numbers
// they qualify rather than to a log nobody reads next to the output.
type resourceDoc struct {
	ResourceID string       `json:"resource_id"`
	Warnings   []string     `json:"warnings"`
	Violations []string     `json:"violations"`
	Records    []recordDoc  `json:"records"`
	Total      money.Amount `json:"total"`
}

// recordDoc is one billed interval with its line items.
type recordDoc struct {
	From       time.Time               `json:"from"`
	To         time.Time               `json:"to"`
	State      string                  `json:"state"`
	Seconds    int64                   `json:"seconds"`
	Minutes    jsonMinutes             `json:"minutes"`
	Dimensions map[string]dimensionDoc `json:"dimensions"`
	Subtotal   money.Amount            `json:"subtotal"`
}

// dimensionDoc is what one priced metric contributed to a record.
type dimensionDoc struct {
	Quantity jsonQuantity `json:"quantity"`
	Cost     money.Amount `json:"cost"`
}

// buildResourceDoc rates one folded history for the period. The second result
// is false for a resource that bills nothing in it, which the caller leaves out
// of the document: an empty history and a resource that lived outside the month
// both produce that.
func buildResourceDoc(resourceID string, tl timeline.Timeline, p period, pr pricing) (resourceDoc, bool, error) {
	records := buildRecords(tl, p)
	if len(records) == 0 {
		return resourceDoc{}, false, nil
	}

	// The fold's warnings are copied rather than referenced, so that the
	// document holds an empty array for a clean resource instead of the null a
	// nil slice marshals to.
	warnings := make([]string, 0, len(tl.Warnings))
	warnings = append(warnings, tl.Warnings...)

	doc := resourceDoc{
		ResourceID: resourceID,
		Warnings:   warnings,
		Violations: checkInvariants(records, tl, p),
		Records:    make([]recordDoc, 0, len(records)),
	}

	total := decimal.Zero
	for _, rec := range records {
		dimensions, subtotal, err := rate(rec, pr)
		if err != nil {
			return resourceDoc{}, false, err
		}

		doc.Records = append(doc.Records, recordDoc{
			From:       rec.From,
			To:         rec.To,
			State:      rec.State,
			Seconds:    rec.Seconds,
			Minutes:    jsonMinutes{Decimal: rec.Minutes},
			Dimensions: dimensions,
			Subtotal:   money.NewAmount(subtotal),
		})
		total = total.Add(subtotal)
	}
	doc.Total = money.NewAmount(total)

	return doc, true, nil
}

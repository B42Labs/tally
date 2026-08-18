package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/core/timeline"
)

// instant parses an RFC 3339 timestamp a test states inline.
func instant(t *testing.T, value string) time.Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parsing %q: %v", value, err)
	}
	return parsed
}

// march is the period every clipping test reports.
func march(t *testing.T) period {
	t.Helper()

	p, err := parseMonth("2026-03")
	if err != nil {
		t.Fatalf("parseMonth() error = %v, want nil", err)
	}
	return p
}

// goldenSize is the instance the golden numbers are rated for: the concept's
// section 3.4 example. The values are float64 because that is what a decoded
// JSON payload carries.
func goldenSize() map[string]any {
	return map[string]any{
		"vcpus":   float64(4),
		"ram_gb":  float64(8),
		"disk_gb": float64(80),
		"flavor":  "m1.large",
	}
}

// storedEvent builds one history entry. received_at follows the timestamp,
// which is what a collector that delivered promptly produces.
func storedEvent(t *testing.T, eventID, eventType, timestamp, state string, size map[string]any) event.Stored {
	t.Helper()

	ts := instant(t, timestamp)
	return event.Stored{
		Event: event.Event{
			EventID:      eventID,
			Timestamp:    ts,
			EventType:    eventType,
			Platform:     "openstack",
			Cloud:        "os-prod-eu1",
			ResourceType: "instance",
			ResourceID:   "abc-123",
			ProjectID:    "proj-456",
			Source:       event.SourceCollector,
			Payload:      event.PayloadEnvelope{State: &state, Size: size},
		},
		ReceivedAt: ts,
	}
}

// goldenTimeline folds the WP 1.15 history: an instance created in February,
// powered off on 03-11, and powered on again on 03-21. It is folded rather than
// written out, so the tests rate what the shared fold produces.
func goldenTimeline(t *testing.T) timeline.Timeline {
	t.Helper()

	return timeline.Build([]event.Stored{
		storedEvent(t, "e1", "compute.instance.create.end", "2026-02-10T08:00:00Z", "active", goldenSize()),
		storedEvent(t, "e2", "compute.instance.power_off", "2026-03-11T00:00:00Z", "shutoff", nil),
		storedEvent(t, "e3", "compute.instance.power_on", "2026-03-21T00:00:00Z", "active", nil),
	})
}

// prototypePricing loads the pricing the golden numbers are rated against.
func prototypePricing(t *testing.T) pricing {
	t.Helper()

	pr, err := loadPricing(filepath.Join("..", "..", "pricing", "prototype.yaml"))
	if err != nil {
		t.Fatalf("loadPricing() error = %v, want nil", err)
	}
	return pr
}

func TestParseMonth(t *testing.T) {
	t.Run("a month becomes the half-open span it covers", func(t *testing.T) {
		p, err := parseMonth("2026-03")
		if err != nil {
			t.Fatalf("parseMonth() error = %v, want nil", err)
		}

		if want := instant(t, "2026-03-01T00:00:00Z"); !p.From.Equal(want) {
			t.Errorf("From = %s, want %s", p.From.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		if want := instant(t, "2026-04-01T00:00:00Z"); !p.To.Equal(want) {
			t.Errorf("To = %s, want %s", p.To.Format(time.RFC3339), want.Format(time.RFC3339))
		}
	})

	// A month that is almost right is the likelier mistake, and rating the wrong
	// span silently is what the refusal prevents.
	for name, value := range map[string]string{
		"a month without its leading zero": "2026-3",
		"a month that does not exist":      "2026-13",
		"an empty value":                   "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseMonth(value); err == nil {
				t.Fatalf("parseMonth(%q) error = nil, want an error", value)
			}
		})
	}
}

func TestBuildRecordsClipsToThePeriod(t *testing.T) {
	p := march(t)
	end := instant(t, "2026-02-20T00:00:00Z")
	late := instant(t, "2026-05-02T00:00:00Z")

	t.Run("an interval straddling the start is billed from the start", func(t *testing.T) {
		closed := instant(t, "2026-03-05T00:00:00Z")
		records := buildRecords(timeline.Timeline{Intervals: []timeline.Interval{
			{Start: instant(t, "2026-02-10T08:00:00Z"), End: &closed, State: "active"},
		}}, p)

		if len(records) != 1 {
			t.Fatalf("buildRecords() = %d records, want 1", len(records))
		}
		if !records[0].From.Equal(p.From) {
			t.Errorf("From = %s, want the period's start %s",
				records[0].From.Format(time.RFC3339), p.From.Format(time.RFC3339))
		}
	})

	t.Run("an open interval is billed to the end of the period", func(t *testing.T) {
		records := buildRecords(timeline.Timeline{Intervals: []timeline.Interval{
			{Start: instant(t, "2026-03-20T00:00:00Z"), State: "active"},
		}}, p)

		if len(records) != 1 {
			t.Fatalf("buildRecords() = %d records, want 1", len(records))
		}
		if !records[0].To.Equal(p.To) {
			t.Errorf("To = %s, want the period's end %s",
				records[0].To.Format(time.RFC3339), p.To.Format(time.RFC3339))
		}
	})

	t.Run("an interval before the period bills nothing", func(t *testing.T) {
		records := buildRecords(timeline.Timeline{Intervals: []timeline.Interval{
			{Start: instant(t, "2026-02-10T08:00:00Z"), End: &end, State: "active"},
		}}, p)

		if len(records) != 0 {
			t.Errorf("buildRecords() = %d records, want none", len(records))
		}
	})

	t.Run("an interval starting at the period's end bills nothing", func(t *testing.T) {
		records := buildRecords(timeline.Timeline{Intervals: []timeline.Interval{
			{Start: p.To, End: &late, State: "active"},
		}}, p)

		if len(records) != 0 {
			t.Errorf("buildRecords() = %d records, want none", len(records))
		}
	})

	t.Run("an empty history bills nothing", func(t *testing.T) {
		if records := buildRecords(timeline.Build(nil), p); len(records) != 0 {
			t.Errorf("buildRecords() = %d records, want none", len(records))
		}
	})

	// A delete is how an instance's billing ends, and it ends it where the
	// delete says rather than at the end of the month.
	t.Run("a deleted resource is billed to its deletion", func(t *testing.T) {
		deleted := instant(t, "2026-03-20T00:00:00Z")
		tl := timeline.Build([]event.Stored{
			storedEvent(t, "e1", "compute.instance.create.end", "2026-03-05T00:00:00Z", "active", goldenSize()),
			storedEvent(t, "e2", "compute.instance.delete.end", "2026-03-20T00:00:00Z", "deleted", nil),
		})

		records := buildRecords(tl, p)
		if len(records) != 1 {
			t.Fatalf("buildRecords() = %d records, want 1", len(records))
		}
		if !records[0].To.Equal(deleted) {
			t.Errorf("To = %s, want the deletion %s",
				records[0].To.Format(time.RFC3339), deleted.Format(time.RFC3339))
		}
		if want := int64(15 * 24 * 60 * 60); records[0].Seconds != want {
			t.Errorf("Seconds = %d, want %d", records[0].Seconds, want)
		}
		if violations := checkInvariants(records, tl, p); len(violations) != 0 {
			t.Errorf("checkInvariants() = %v, want none", violations)
		}
	})

	// The golden history, clipped to March: ten days active, ten days off, and
	// the remaining eleven days active again. These are the seconds the WP 1.15
	// table's minutes are derived from.
	t.Run("the golden history yields the golden quantities", func(t *testing.T) {
		records := buildRecords(goldenTimeline(t), p)

		want := []struct {
			from, to, state, minutes string
			seconds                  int64
		}{
			{"2026-03-01T00:00:00Z", "2026-03-11T00:00:00Z", "active", "14400", 864000},
			{"2026-03-11T00:00:00Z", "2026-03-21T00:00:00Z", "shutoff", "14400", 864000},
			{"2026-03-21T00:00:00Z", "2026-04-01T00:00:00Z", "active", "15840", 950400},
		}
		if len(records) != len(want) {
			t.Fatalf("buildRecords() = %d records, want %d", len(records), len(want))
		}
		for i, expected := range want {
			got := records[i]
			if !got.From.Equal(instant(t, expected.from)) || !got.To.Equal(instant(t, expected.to)) {
				t.Errorf("records[%d] = %s..%s, want %s..%s", i,
					got.From.Format(time.RFC3339), got.To.Format(time.RFC3339), expected.from, expected.to)
			}
			if got.State != expected.state {
				t.Errorf("records[%d].State = %q, want %q", i, got.State, expected.state)
			}
			if got.Seconds != expected.seconds {
				t.Errorf("records[%d].Seconds = %d, want %d", i, got.Seconds, expected.seconds)
			}
			if !got.Minutes.Equal(decimal.RequireFromString(expected.minutes)) {
				t.Errorf("records[%d].Minutes = %s, want %s", i, got.Minutes, expected.minutes)
			}
		}
	})
}

func TestSizeDecimal(t *testing.T) {
	t.Run("a decoded JSON number keeps its value", func(t *testing.T) {
		for metric, want := range map[string]string{"vcpus": "4", "disk_gb": "80"} {
			got, err := sizeDecimal(goldenSize(), metric)
			if err != nil {
				t.Fatalf("sizeDecimal(%q) error = %v, want nil", metric, err)
			}
			if !got.Equal(decimal.RequireFromString(want)) {
				t.Errorf("sizeDecimal(%q) = %s, want %s", metric, got, want)
			}
		}
	})

	// A size assembled in Go rather than decoded carries plain ints, which the
	// same path has to read.
	t.Run("an integer value keeps its value", func(t *testing.T) {
		got, err := sizeDecimal(map[string]any{"vcpus": 4}, "vcpus")
		if err != nil {
			t.Fatalf("sizeDecimal() error = %v, want nil", err)
		}
		if !got.Equal(decimal.NewFromInt(4)) {
			t.Errorf("sizeDecimal() = %s, want 4", got)
		}
	})

	t.Run("a metric the size does not carry is refused", func(t *testing.T) {
		_, err := sizeDecimal(map[string]any{"vcpus": float64(4)}, "ram_gb")
		if err == nil {
			t.Fatalf("sizeDecimal() error = nil, want an error")
		}
		if !strings.Contains(err.Error(), "ram_gb") {
			t.Errorf("sizeDecimal() error = %v, want it to name ram_gb", err)
		}
	})

	t.Run("a metric that is not a number is refused", func(t *testing.T) {
		_, err := sizeDecimal(goldenSize(), "flavor")
		if err == nil {
			t.Fatalf("sizeDecimal() error = nil, want an error")
		}
		if !strings.Contains(err.Error(), "flavor") {
			t.Errorf("sizeDecimal() error = %v, want it to name flavor", err)
		}
	})
}

// TestRateAppliesPricesAndModifiers is the golden-numbers gate on the rating
// itself: the first row of the WP 1.15 table, and what the modifiers do to it.
func TestRateAppliesPricesAndModifiers(t *testing.T) {
	pr := prototypePricing(t)
	golden := usageRecord{
		From:    instant(t, "2026-03-01T00:00:00Z"),
		To:      instant(t, "2026-03-11T00:00:00Z"),
		State:   "active",
		Size:    goldenSize(),
		Seconds: 864000,
		Minutes: money.Minutes(864000),
	}

	for name, tc := range map[string]struct {
		state    string
		costs    map[string]string
		subtotal string
	}{
		"active is billed in full": {
			state:    "active",
			costs:    map[string]string{"vcpus": "19.20", "ram_gb": "9.60", "disk_gb": "19.20"},
			subtotal: "48.00",
		},
		"shutoff is halved": {
			state:    "shutoff",
			costs:    map[string]string{"vcpus": "9.60", "ram_gb": "4.80", "disk_gb": "9.60"},
			subtotal: "24.00",
		},
		"shelved is free": {
			state:    "shelved",
			costs:    map[string]string{"vcpus": "0.00", "ram_gb": "0.00", "disk_gb": "0.00"},
			subtotal: "0.00",
		},
		"a state with no modifier is billed in full": {
			state:    "rescued",
			costs:    map[string]string{"vcpus": "19.20", "ram_gb": "9.60", "disk_gb": "19.20"},
			subtotal: "48.00",
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := golden
			rec.State = tc.state

			dimensions, subtotal, err := rate(rec, pr)
			if err != nil {
				t.Fatalf("rate() error = %v, want nil", err)
			}

			if len(dimensions) != len(tc.costs) {
				t.Fatalf("rate() = %d dimensions, want %d", len(dimensions), len(tc.costs))
			}
			for metric, dim := range dimensions {
				want, ok := tc.costs[metric]
				if !ok {
					t.Errorf("rate() priced %q, which the case does not expect", metric)
					continue
				}
				if got := dim.Cost.StringFixed(2); got != want {
					t.Errorf("%s cost = %s, want %s", metric, got, want)
				}
			}
			if got := subtotal.StringFixed(2); got != tc.subtotal {
				t.Errorf("Subtotal = %s, want %s", got, tc.subtotal)
			}
		})
	}

	t.Run("a priced metric the size does not carry is refused", func(t *testing.T) {
		rec := golden
		rec.Size = map[string]any{"vcpus": float64(4), "disk_gb": float64(80)}

		_, _, err := rate(rec, pr)
		if err == nil {
			t.Fatalf("rate() error = nil, want an error")
		}
		for _, want := range []string{"2026-03-01T00:00:00Z", "ram_gb"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("rate() error = %v, want it to name %s", err, want)
			}
		}
	})
}

func TestCheckInvariantsTripsOnBadRecords(t *testing.T) {
	p := march(t)
	tl := goldenTimeline(t)

	// A record is only ever built from an interval, so these are stated by hand:
	// they are what a broken clipping would produce.
	record := func(from, to string, seconds int64) usageRecord {
		return usageRecord{
			From:    instant(t, from),
			To:      instant(t, to),
			State:   "active",
			Size:    goldenSize(),
			Seconds: seconds,
			Minutes: money.Minutes(seconds),
		}
	}

	for name, tc := range map[string]struct {
		records []usageRecord
		want    []string
	}{
		"an overlap between two records": {
			records: []usageRecord{
				record("2026-03-01T00:00:00Z", "2026-03-12T00:00:00Z", 950400),
				record("2026-03-11T00:00:00Z", "2026-04-01T00:00:00Z", 1814400),
			},
			want: []string{"2026-03-12T00:00:00Z", "2026-03-11T00:00:00Z"},
		},
		"a gap between two records": {
			records: []usageRecord{
				record("2026-03-01T00:00:00Z", "2026-03-05T00:00:00Z", 345600),
				record("2026-03-06T00:00:00Z", "2026-04-01T00:00:00Z", 2246400),
			},
			want: []string{"2026-03-05T00:00:00Z", "2026-03-06T00:00:00Z"},
		},
		"a record outside the period": {
			records: []usageRecord{
				record("2026-02-25T00:00:00Z", "2026-04-01T00:00:00Z", 2678400),
			},
			want: []string{"2026-02-25T00:00:00Z", "2026-03-01T00:00:00Z"},
		},
		"a record that bills no time": {
			records: []usageRecord{
				record("2026-03-05T00:00:00Z", "2026-03-05T00:00:00Z", 0),
			},
			want: []string{"bills no time"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			violations := checkInvariants(tc.records, tl, p)
			if len(violations) == 0 {
				t.Fatalf("checkInvariants() = no violations, want one naming %v", tc.want)
			}

			joined := strings.Join(violations, "\n")
			for _, want := range tc.want {
				if !strings.Contains(joined, want) {
					t.Errorf("checkInvariants() = %v, want a violation naming %s", violations, want)
				}
			}
		})
	}

	t.Run("the golden records are clean", func(t *testing.T) {
		if violations := checkInvariants(buildRecords(tl, p), tl, p); len(violations) != 0 {
			t.Errorf("checkInvariants() = %v, want none", violations)
		}
	})

	t.Run("an empty history breaks nothing", func(t *testing.T) {
		if violations := checkInvariants(nil, timeline.Build(nil), p); len(violations) != 0 {
			t.Errorf("checkInvariants() = %v, want none", violations)
		}
	})

	// An OpenStack notification carries microseconds, so a transition inside the
	// month lands between two seconds. The two records still meet there: a
	// boundary is carried over as it stands, and only the durations derived from
	// it are truncated.
	t.Run("a history with sub-second timestamps breaks nothing", func(t *testing.T) {
		tl := timeline.Build([]event.Stored{
			storedEvent(t, "e1", "compute.instance.create.end", "2026-03-01T12:00:00.5Z", "active", goldenSize()),
			storedEvent(t, "e2", "compute.instance.power_off", "2026-03-11T00:00:00.1Z", "shutoff", nil),
		})

		records := buildRecords(tl, p)
		if len(records) != 2 {
			t.Fatalf("buildRecords() = %d records, want 2", len(records))
		}
		if violations := checkInvariants(records, tl, p); len(violations) != 0 {
			t.Errorf("checkInvariants() = %v, want none", violations)
		}
	})

	// A create after a delete is the resource coming back: two lives with three
	// days between them the resource did not exist for.
	recreated := timeline.Build([]event.Stored{
		storedEvent(t, "e1", "compute.instance.create.end", "2026-03-01T00:00:00Z", "active", goldenSize()),
		storedEvent(t, "e2", "compute.instance.delete.end", "2026-03-02T00:00:00Z", "deleted", nil),
		storedEvent(t, "e3", "compute.instance.create.end", "2026-03-05T00:00:00Z", "active", goldenSize()),
	})

	// The days between the two lives are not time the resource lived, so the
	// records on either side of them are not meant to meet: reporting that as a
	// gap would fail the run on numbers that are all right.
	t.Run("a resource that was deleted and created again breaks nothing", func(t *testing.T) {
		records := buildRecords(recreated, p)
		if len(records) != 2 {
			t.Fatalf("buildRecords() = %d records, want 2", len(records))
		}
		if violations := checkInvariants(records, recreated, p); len(violations) != 0 {
			t.Errorf("checkInvariants() = %v, want none", violations)
		}
	})

	// An overlap is wrong wherever it falls, including at the instant a life
	// ends. Only a gap is forgiven over time the resource did not exist for, and
	// a second the records bill twice is not that: the resource lived through it
	// once, whether or not it lived through the instant the overlap ends at.
	t.Run("an overlap at the end of a life is still reported", func(t *testing.T) {
		deleted := timeline.Build([]event.Stored{
			storedEvent(t, "e1", "compute.instance.create.end", "2026-03-01T00:00:00Z", "active", goldenSize()),
			storedEvent(t, "e2", "compute.instance.delete.end", "2026-03-10T00:00:00Z", "deleted", nil),
		})

		// The one life billed twice: the record ends where the resource does, so
		// the resource no longer existed at the instant the overlap closes at.
		violations := checkInvariants([]usageRecord{
			record("2026-03-01T00:00:00Z", "2026-03-10T00:00:00Z", 777600),
			record("2026-03-01T00:00:00Z", "2026-03-10T00:00:00Z", 777600),
		}, deleted, p)

		if len(violations) != 1 || !strings.Contains(violations[0], "overlaps") {
			t.Errorf("checkInvariants() = %v, want the double-billed life reported", violations)
		}
	})

	// Only the gap between two lives is forgiven. One inside a life is time the
	// clipping lost, and it is reported even when the same history carries a
	// legitimate gap next to it.
	t.Run("a gap inside a life is still reported", func(t *testing.T) {
		violations := checkInvariants([]usageRecord{
			record("2026-03-01T00:00:00Z", "2026-03-02T00:00:00Z", 86400),
			record("2026-03-05T00:00:00Z", "2026-03-06T00:00:00Z", 86400),
			record("2026-03-07T00:00:00Z", "2026-04-01T00:00:00Z", 2160000),
		}, recreated, p)

		if len(violations) != 1 || !strings.Contains(violations[0], "2026-03-06T00:00:00Z") {
			t.Errorf("checkInvariants() = %v, want the gap inside the second life alone", violations)
		}
	})
}

// TestWarningsPassThrough keeps what the fold could not establish visible in
// the output: a history that starts mid-life bills from an unknown start, and
// the reader of the numbers has to be told so.
func TestWarningsPassThrough(t *testing.T) {
	tl := timeline.Build([]event.Stored{
		storedEvent(t, "e1", "compute.instance.power_on", "2026-03-01T00:00:00Z", "active", goldenSize()),
	})
	if !slices.Contains(tl.Warnings, timeline.WarningHistoryStartsWithoutCreate) {
		t.Fatalf("Warnings = %v, want %q", tl.Warnings, timeline.WarningHistoryStartsWithoutCreate)
	}

	doc, billed, err := buildResourceDoc("abc-123", tl, march(t), prototypePricing(t))
	if err != nil {
		t.Fatalf("buildResourceDoc() error = %v, want nil", err)
	}
	if !billed {
		t.Fatalf("buildResourceDoc() billed = false, want the resource in the document")
	}
	if !slices.Contains(doc.Warnings, timeline.WarningHistoryStartsWithoutCreate) {
		t.Errorf("Warnings = %v, want %q", doc.Warnings, timeline.WarningHistoryStartsWithoutCreate)
	}
}

// TestBuildResourceDocDropsAResourceThatBillsNothing keeps a resource that
// lived outside the month out of the document: it is not an empty entry with a
// zero total, which a reader would take for an instance that ran for free.
func TestBuildResourceDocDropsAResourceThatBillsNothing(t *testing.T) {
	tl := timeline.Build([]event.Stored{
		storedEvent(t, "e1", "compute.instance.create.end", "2026-01-10T00:00:00Z", "active", goldenSize()),
		storedEvent(t, "e2", "compute.instance.delete.end", "2026-02-05T00:00:00Z", "deleted", nil),
	})

	doc, billed, err := buildResourceDoc("abc-123", tl, march(t), prototypePricing(t))
	if err != nil {
		t.Fatalf("buildResourceDoc() error = %v, want nil", err)
	}
	if billed {
		t.Errorf("buildResourceDoc() billed = true with %+v, want the resource left out", doc)
	}
}

// TestDocumentSerializationKeepsPrecision reads the encoded bytes rather than
// the values behind them: what a customer reconciles against an invoice is the
// output, and a subtotal rendered as 48 rather than 48.00 is already wrong
// there.
func TestDocumentSerializationKeepsPrecision(t *testing.T) {
	t.Run("a rated record keeps its scale", func(t *testing.T) {
		doc := document{
			Cloud:     "os-prod-eu1",
			ProjectID: "proj-456",
			Period:    periodDoc{From: instant(t, "2026-03-01T00:00:00Z"), To: instant(t, "2026-04-01T00:00:00Z")},
			Currency:  "EUR",
			Resources: []resourceDoc{{
				ResourceID: "abc-123",
				Warnings:   []string{},
				Violations: []string{},
				Records: []recordDoc{{
					From:    instant(t, "2026-03-01T00:00:00Z"),
					To:      instant(t, "2026-03-11T00:00:00Z"),
					State:   "active",
					Seconds: 864000,
					Minutes: jsonMinutes{Decimal: money.Minutes(864000)},
					Dimensions: map[string]dimensionDoc{"vcpus": {
						Quantity: jsonQuantity{Decimal: decimal.NewFromInt(4)},
						Cost:     money.NewAmount(decimal.RequireFromString("19.2")),
					}},
					Subtotal: money.NewAmount(decimal.RequireFromString("48")),
				}},
				Total: money.NewAmount(decimal.RequireFromString("48")),
			}},
			Total: money.NewAmount(decimal.RequireFromString("48")),
		}

		encoded, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("Marshal() error = %v, want nil", err)
		}

		for _, want := range []string{
			`"minutes":14400.0000`,
			`"quantity":4`,
			`"cost":19.20`,
			`"subtotal":48.00`,
			`"total":48.00`,
			`"warnings":[]`,
			`"violations":[]`,
			`"from":"2026-03-01T00:00:00Z"`,
		} {
			if !strings.Contains(string(encoded), want) {
				t.Errorf("the document does not carry %s:\n%s", want, encoded)
			}
		}
	})

	t.Run("a project without resources renders empty lists", func(t *testing.T) {
		encoded, err := json.Marshal(document{Resources: []resourceDoc{}, Total: money.NewAmount(decimal.Zero)})
		if err != nil {
			t.Fatalf("Marshal() error = %v, want nil", err)
		}

		for _, want := range []string{`"resources":[]`, `"total":0.00`} {
			if !strings.Contains(string(encoded), want) {
				t.Errorf("the document does not carry %s:\n%s", want, encoded)
			}
		}
	})
}

// TestExistedAt pins the lookup the gap check forgives a gap by. It answers
// from the ordering timeline.Build guarantees rather than from a scan, so the
// boundaries of a life, and an instant before every life, are what the search
// has to get right.
func TestExistedAt(t *testing.T) {
	// Two lives with three days between them, the second one still open.
	tl := timeline.Build([]event.Stored{
		storedEvent(t, "e1", "compute.instance.create.end", "2026-03-01T00:00:00Z", "active", goldenSize()),
		storedEvent(t, "e2", "compute.instance.delete.end", "2026-03-02T00:00:00Z", "deleted", nil),
		storedEvent(t, "e3", "compute.instance.create.end", "2026-03-05T00:00:00Z", "active", goldenSize()),
	})

	for name, tc := range map[string]struct {
		ts   string
		want bool
	}{
		"before the first life":              {ts: "2026-02-28T00:00:00Z", want: false},
		"the instant the first life starts":  {ts: "2026-03-01T00:00:00Z", want: true},
		"inside the first life":              {ts: "2026-03-01T12:00:00Z", want: true},
		"the instant the first life ends":    {ts: "2026-03-02T00:00:00Z", want: false},
		"between the two lives":              {ts: "2026-03-03T00:00:00Z", want: false},
		"the instant the second life starts": {ts: "2026-03-05T00:00:00Z", want: true},
		"long after the open life started":   {ts: "2027-01-01T00:00:00Z", want: true},
	} {
		t.Run(name, func(t *testing.T) {
			if got := existedAt(tl, instant(t, tc.ts)); got != tc.want {
				t.Errorf("existedAt(%s) = %t, want %t", tc.ts, got, tc.want)
			}
		})
	}
}

// TestExistedAtOnAnEmptyTimeline holds the lookup to answering rather than
// panicking for a history that folded to nothing: a resource the API served no
// events for reaches the invariant check the same way any other does.
func TestExistedAtOnAnEmptyTimeline(t *testing.T) {
	if existedAt(timeline.Build(nil), instant(t, "2026-03-01T00:00:00Z")) {
		t.Error("existedAt() = true on an empty timeline, want false")
	}
}

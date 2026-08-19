package httpapi

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// TestGroupEventCounts pins what the handler does with the counted buckets: the
// source is summed away unless the request grouped by it, the bucket is stated
// in UTC, and the items keep the order the contract states.
func TestGroupEventCounts(t *testing.T) {
	// The rows arrive as the query orders them, one bucket at a time. The second
	// one is read in a zone other than UTC, which a connection may hand over: a
	// bucket is one instant whatever zone it was decoded in, so it has to fold
	// into the same item as the first and to be stated in UTC.
	berlin := time.FixedZone("CEST", 2*60*60)
	rows := []sqlcgen.CountEventBucketsRow{
		{Bucket: bucketAt(t, "2026-07-01T00:00:00Z", time.UTC), Cloud: "os-prod-eu1", EventType: "instance.create", Source: "collector", Events: 4},
		{Bucket: bucketAt(t, "2026-07-01T00:00:00Z", berlin), Cloud: "os-prod-eu1", EventType: "instance.create", Source: "reconciliation", Events: 1},
		{Bucket: bucketAt(t, "2026-07-01T00:00:00Z", time.UTC), Cloud: "os-prod-eu1", EventType: "volume.delete", Source: "collector", Events: 2},
		{Bucket: bucketAt(t, "2026-07-01T01:00:00Z", time.UTC), Cloud: "os-dev-eu1", EventType: "instance.create", Source: "collector", Events: 3},
	}

	tests := map[string]struct {
		rows    []sqlcgen.CountEventBucketsRow
		groupBy []GetEventStatsParamsGroupBy
		want    []string
	}{
		"a grouping without source sums the pipelines of a bucket": {
			rows: rows,
			groupBy: []GetEventStatsParamsGroupBy{
				GetEventStatsParamsGroupByCloud,
				GetEventStatsParamsGroupByEventType,
			},
			want: []string{
				"2026-07-01T00:00:00Z|os-prod-eu1|instance.create|-=5",
				"2026-07-01T00:00:00Z|os-prod-eu1|volume.delete|-=2",
				"2026-07-01T01:00:00Z|os-dev-eu1|instance.create|-=3",
			},
		},
		"a grouping naming source keeps the pipelines apart": {
			rows: rows,
			groupBy: []GetEventStatsParamsGroupBy{
				GetEventStatsParamsGroupByCloud,
				GetEventStatsParamsGroupByEventType,
				GetEventStatsParamsGroupBySource,
			},
			want: []string{
				"2026-07-01T00:00:00Z|os-prod-eu1|instance.create|collector=4",
				"2026-07-01T00:00:00Z|os-prod-eu1|instance.create|reconciliation=1",
				"2026-07-01T00:00:00Z|os-prod-eu1|volume.delete|collector=2",
				"2026-07-01T01:00:00Z|os-dev-eu1|instance.create|collector=3",
			},
		},
		"a window holding no event yields no item": {
			rows: nil,
			groupBy: []GetEventStatsParamsGroupBy{
				GetEventStatsParamsGroupByCloud,
				GetEventStatsParamsGroupByEventType,
			},
			want: []string{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			items := groupEventCounts(tc.rows, tc.groupBy)
			if items == nil {
				t.Fatal("items = nil, want an empty array rather than a null one")
			}
			if got := renderEventItems(items); !slices.Equal(got, tc.want) {
				t.Errorf("items = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBucketWidth pins the two widths the route offers against the literals the
// query casts to an interval: the enum of the contract is what a caller names,
// and Postgres is what reads the result.
//
// The last case is the one the enum keeps unreachable today. It is here because
// an interval added to the contract regenerates the constants and validates
// without touching the mapping, and answering it as an hour would bucket a
// request by a width the answer carries no field to report.
func TestBucketWidth(t *testing.T) {
	tests := map[GetEventStatsParamsInterval]struct {
		width string
		ok    bool
	}{
		N1h:  {width: "1 hour", ok: true},
		N1d:  {width: "1 day", ok: true},
		"5m": {},
		"1w": {},
		"":   {},
	}

	for interval, want := range tests {
		t.Run(string(interval), func(t *testing.T) {
			width, ok := bucketWidth(interval)
			if width != want.width || ok != want.ok {
				t.Errorf("bucketWidth(%q) = %q, %v, want %q, %v",
					interval, width, ok, want.width, want.ok)
			}
		})
	}
}

// bucketAt is one bucket start as the query hands it over, read in the zone the
// connection decoded it in.
func bucketAt(t *testing.T, instant string, zone *time.Location) pgtype.Timestamptz {
	t.Helper()

	at, err := time.Parse(time.RFC3339, instant)
	if err != nil {
		t.Fatalf("parsing %q: %v", instant, err)
	}
	return pgtype.Timestamptz{Time: at.In(zone), Valid: true}
}

// renderEventItems states the items as strings, so that a mismatch prints what
// the members hold rather than the address the optional source carries. An
// absent source renders as a dash, which is what tells it from one holding the
// empty string.
func renderEventItems(items []EventStatsItem) []string {
	rendered := make([]string, len(items))
	for i, item := range items {
		rendered[i] = fmt.Sprintf("%s|%s|%s|%s=%d", item.Bucket.Format(time.RFC3339),
			item.Cloud, item.EventType, renderOptional(item.Source), item.Count)
	}
	return rendered
}

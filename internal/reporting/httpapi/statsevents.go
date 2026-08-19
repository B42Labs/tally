package httpapi

import (
	"cmp"
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// eventStatsDetail answers every failure of this route a caller can do nothing
// about.
const eventStatsDetail = "the event counts could not be read"

// maxEventStatsRows is how many groups this route counts at most. The answer is
// not paginated, so without a bound the memory one request costs is set by the
// window and the interval it names: the number of buckets is the window divided
// by the width, and every bucket carries a row per cloud, event type, and source
// seen in it.
//
// A request above the bound is refused rather than answered short, so a client
// never mistakes a truncated grouping for a whole one. It is far above what a
// dashboard reads: a year of hourly buckets is 8760 of them, which the bound
// holds for a cloud reporting one event type from one pipeline.
const maxEventStatsRows = 10000

// maxEventStatsWindow is how wide a window one request may count. It bounds the
// read rather than the response: the query's own LIMIT sits behind a GROUP BY,
// so the database walks and aggregates every event of the window before a single
// row can be discarded, and what that costs is set by the events the window
// holds and not by the buckets it names. A bound read on the buckets would say
// nothing about it — ten thousand of them at a daily width is twenty-seven years
// of the archive, aggregated whole into an answer of a few thousand rows.
//
// It is a year and a month, which is the longest span a dashboard asks for and
// leaves a full year reachable from any day of the next one. At the finest width
// this route buckets by it is 9600 buckets, so it stays under the row bound as
// well for a fleet reporting one event type from one pipeline into one cloud.
//
// A wider window is refused before the count is issued, which is what keeps the
// cost of the refusal off the database.
const maxEventStatsWindow = 400 * 24 * time.Hour

// largeResultDetail answers a request that groups into more rows than one answer
// carries. It names what the caller changes to get through, because the bound
// itself says nothing about that.
const largeResultDetail = "this request groups into more rows than the unpaginated statistics serve; " +
	"ask for a narrower window or a coarser interval"

// GetEventStats answers the stored events of one window counted per time bucket
// and per combination of the dimensions the request groups by.
//
// The bucketing is the database's, so what decides which bucket an event falls
// into is the same time_bucket call the hypertable is chunked along, and the
// handler adds the rows up onto the requested dimensions afterwards.
//
// The counts are event-scoped the way the event list is: a project token counts
// every event whose project_id names one of its projects, including the events a
// resource carried before it was transferred away.
func (s *server) GetEventStats(w http.ResponseWriter, r *http.Request, params GetEventStatsParams) {
	ctx := r.Context()

	// cloud and event_type are what an item is read by, so a grouping without
	// them counts events it cannot name. The rule spans two members of one list,
	// which the contract cannot express; a dimension outside the enum and a
	// repeated one are refused by the validator in front of this.
	if !slices.Contains(params.GroupBy, GetEventStatsParamsGroupByCloud) ||
		!slices.Contains(params.GroupBy, GetEventStatsParamsGroupByEventType) {
		problem.Write(w, http.StatusBadRequest, problem.TypeValidation,
			"Validation failed", "group_by must name cloud and event_type")
		return
	}

	width, ok := bucketWidth(params.Interval)
	if !ok {
		// The contract's enum is what keeps this unreachable, so a request that
		// gets here means the contract declares a width this route does not
		// bucket by. It is refused the way a route no guard covers is: taking the
		// case offline says so, where bucketing it by some other width would not.
		Logger(ctx).Error("no bucket width covers this interval", "interval", params.Interval)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal,
			"Internal error", eventStatsDetail)
		return
	}
	// The window is refused on how wide it is, before anything is read: what the
	// aggregate costs is the events the window holds, which is a number this
	// service does not control, so the request is turned away where the refusal
	// costs nothing rather than after the database has walked the archive. A
	// window ending at or before its start is negative here and passes, which is
	// the empty answer the half-open window means.
	if params.To.Sub(params.From) > maxEventStatsWindow {
		refuseLargeResult(ctx, w, params)
		return
	}

	scope, ok := s.queryScope(w, r)
	if !ok {
		return
	}
	// A filtered scope holding no project reaches no event at all, so the empty
	// answer is given here rather than asked of the database.
	if !scope.Unfiltered && len(scope.Refs) == 0 {
		writeJSON(w, EventStatsList{Items: []EventStatsItem{}})
		return
	}

	clouds, projects := scopeFilter(scope)

	// A from at or past to counts nothing, which is what the half-open window
	// means and needs no case of its own: each bound is a valid instant on its
	// own, and the contract cannot express a rule spanning two parameters.
	rows, err := s.queries.CountEventBuckets(ctx, sqlcgen.CountEventBucketsParams{
		BucketWidth:   width,
		FromTs:        pgtype.Timestamptz{Time: params.From, Valid: true},
		ToTs:          pgtype.Timestamptz{Time: params.To, Valid: true},
		ScopeClouds:   clouds,
		ScopeProjects: projects,
		RowCap:        maxEventStatsRows + 1,
	})
	if err != nil {
		writeInternal(ctx, w, "counting event buckets", err, eventStatsDetail)
		return
	}
	// The query reads one row past the bound, so a row set of exactly the bound
	// is a whole answer and one longer is the refusal.
	if len(rows) > maxEventStatsRows {
		refuseLargeResult(ctx, w, params)
		return
	}

	writeJSON(w, EventStatsList{Items: groupEventCounts(rows, params.GroupBy)})
}

// bucketWidth maps the interval a request names onto the literal the query casts
// to an interval.
//
// The contract's enum makes these two cases exhaustive, and the second return
// value is what says so rather than assuming it: an interval added to the enum
// compiles and validates without touching this, and a width silently substituted
// for the one asked for is a count the answer carries no field to detect.
func bucketWidth(interval GetEventStatsParamsInterval) (string, bool) {
	switch interval {
	case N1h:
		return "1 hour", true
	case N1d:
		return "1 day", true
	}
	return "", false
}

// refuseLargeResult answers a request above one of the two bounds, the span its
// window covers or the rows its grouping fills, and logs what was asked for: the
// bound being hit at all is an operational signal, and the response says nothing
// about the request that hit it.
//
// The row bound counts what the query returns, which is the finest grouping
// there is, so dropping a dimension does not lower it: the query groups by all
// of them, and the collapsing happens on rows the refusal has already been
// decided on. A narrower window is what gets a request through either bound, and
// a coarser interval the row bound alone.
func refuseLargeResult(ctx context.Context, w http.ResponseWriter, params GetEventStatsParams) {
	Logger(ctx).Warn("refusing an event grouping above the bound",
		"from", params.From, "to", params.To, "interval", params.Interval,
		"bound", maxEventStatsRows)
	problem.Write(w, http.StatusUnprocessableEntity, problem.TypeResultTooLarge,
		"Result too large", largeResultDetail)
}

// eventGroup is the combination of values one item stands for, which is what the
// counts are keyed by while they are added up. The source stays the empty string
// for a request that did not group by it, so the rows of one bucket that differ
// in nothing else merge there.
//
// The bucket is keyed in UTC, the zone the answer states it in, which is also
// what makes two rows of one bucket compare equal.
type eventGroup struct {
	bucket    time.Time
	cloud     string
	eventType string
	source    string
}

// groupEventCounts adds the counted rows up onto the dimensions the request
// asked for and renders them as the answer the contract promises. The source
// member is set exactly when the grouping names it: the count stands for one
// pipeline only when it was taken per pipeline.
//
// The items come out ordered the way the contract states, and the slice is never
// nil: a window holding no event is answered as the empty array.
func groupEventCounts(rows []sqlcgen.CountEventBucketsRow,
	groupBy []GetEventStatsParamsGroupBy,
) []EventStatsItem {
	bySource := slices.Contains(groupBy, GetEventStatsParamsGroupBySource)

	counts := make(map[eventGroup]int64, len(rows))
	for _, row := range rows {
		group := eventGroup{
			bucket:    row.Bucket.Time.UTC(),
			cloud:     row.Cloud,
			eventType: row.EventType,
		}
		if bySource {
			group.source = row.Source
		}
		counts[group] += row.Events
	}

	items := make([]EventStatsItem, 0, len(counts))
	for _, group := range slices.SortedFunc(maps.Keys(counts), compareEventGroups) {
		items = append(items, EventStatsItem{
			Bucket:    group.bucket,
			Cloud:     group.cloud,
			EventType: group.eventType,
			Source:    groupedValue(bySource, group.source),
			Count:     counts[group],
		})
	}
	return items
}

// compareEventGroups orders two groups by their bucket first and by their
// dimensions after it, in the order the contract states.
func compareEventGroups(a, b eventGroup) int {
	return cmp.Or(
		a.bucket.Compare(b.bucket),
		strings.Compare(a.cloud, b.cloud),
		strings.Compare(a.eventType, b.eventType),
		strings.Compare(a.source, b.source),
	)
}

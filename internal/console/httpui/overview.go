package httpui

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/reporting/httpapi"
)

// What the overview reads. The window is the last day in hourly buckets, which
// is what a demo run of the simulator fills, and the two limits keep the page
// to what fits on a screen.
const (
	eventWindow   = 24 * time.Hour
	eventInterval = "1h"
	rejectedLimit = 5
	runLimit      = 20
)

// overviewData is the front page: what the API counts, what the ingest path
// refused, and what the engine has run.
type overviewData struct {
	Stats    listing[statRow]
	Events   listing[httpapi.EventStatsItem]
	Rejected listing[httpapi.DeadLetteredEvent]
	Periods  listing[billingPeriodRow]
	Runs     listing[runRow]
	From     time.Time
	To       time.Time
}

// The columns of the overview's tables. The share bar of the counts draws the
// count and is sorted through it.
var (
	statColumns = []column[statRow]{
		textCol("cloud", func(r statRow) string { return r.Cloud }),
		textCol("resource type", func(r statRow) string { return r.ResourceType }),
		textCol("state", func(r statRow) string { return r.State }),
		countCol("count", func(r statRow) int64 { return r.Count }),
		plainCol[statRow]("share"),
	}
	eventColumns = []column[httpapi.EventStatsItem]{
		textCol("bucket", func(r httpapi.EventStatsItem) string { return stamp(r.Bucket) }),
		textCol("cloud", func(r httpapi.EventStatsItem) string { return r.Cloud }),
		textCol("event type", func(r httpapi.EventStatsItem) string { return r.EventType }),
		countCol("count", func(r httpapi.EventStatsItem) int64 { return r.Count }),
	}
	rejectedColumns = []column[httpapi.DeadLetteredEvent]{
		textCol("received at", func(r httpapi.DeadLetteredEvent) string { return stamp(r.ReceivedAt) }),
		textCol("reason", func(r httpapi.DeadLetteredEvent) string { return r.Reason }),
	}
	periodColumns = []column[billingPeriodRow]{
		textCol("from", func(r billingPeriodRow) string { return stamp(r.Period.From) }),
		textCol("to", func(r billingPeriodRow) string { return stamp(r.Period.To) }),
		textCol("status", func(r billingPeriodRow) string { return r.Period.Status }),
		textCol("finalized by", func(r billingPeriodRow) string { return idText(r.Period.FinalizedRunID) }),
		textCol("finalized at", func(r billingPeriodRow) string { return zeroStamp(r.Period.FinalizedAt) }),
	}
	runColumns = []column[runRow]{
		textCol("run", func(r runRow) string { return r.Run.ID.String() }),
		textCol("period", func(r runRow) string { return stamp(r.Run.PeriodFrom) }),
		textCol("kind", func(r runRow) string { return r.Run.Kind }),
		textCol("status", func(r runRow) string { return r.Run.Status }),
		textCol("pricing", func(r runRow) string { return r.Run.PricingVersion }),
		textCol("started", func(r runRow) string { return zeroStamp(r.Run.StartedAt) }),
		textCol("completed", func(r runRow) string { return zeroStamp(r.Run.CompletedAt) }),
	}
)

// statRow is one counted group with the bar it is drawn as.
type statRow struct {
	Cloud        string
	ResourceType string
	State        string
	Count        int64
	Width        int64
}

// runRow is one metering run and the page that shows what it billed.
type runRow struct {
	Run  store.Run
	Link string
}

// overview reads both sides and shows what each one holds. The API sections and
// the engine sections are independent: a database that has run nothing yet
// renders empty period and run tables next to the counts the API answered.
func (h *handlers) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var src sources

	now := h.now()
	from, to := now.Add(-eventWindow), now

	stats, request, err := h.api.ResourceStats(ctx, []string{"cloud", "resource_type", "state"}, "all")
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	events, request, err := h.api.EventStats(ctx, []string{"cloud", "event_type"}, eventInterval, from, to)
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	rejected, request, err := h.api.ListRejectedEvents(ctx, rejectedLimit)
	src.api(request)
	if err != nil {
		h.failFrom(w, r, apiFailed(err), src)
		return
	}

	periods, err := h.store.ListPeriods(ctx)
	src.query("ListPeriods")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	runs, err := h.store.ListRuns(ctx, runLimit)
	src.query("ListRuns")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	data := overviewData{
		Stats:    tabulate(r, "stats", statColumns, statRows(stats.Items)),
		Events:   tabulate(r, "events", eventColumns, events.Items),
		Rejected: tabulate(r, "rejected", rejectedColumns, rejected.Items),
		Periods:  tabulate(r, "periods", periodColumns, billingPeriodRows(periods)),
		Runs:     tabulate(r, "runs", runColumns, runRows(runs)),
		From:     from,
		To:       to,
	}
	h.render(w, r, "overview", page{Title: "Overview", Sources: src, Data: data})
}

// statRows scales every group against the largest one, so the bars of one table
// are comparable to each other and to nothing else.
func statRows(items []httpapi.ResourceStatsItem) []statRow {
	var largest int64
	for _, item := range items {
		if item.Count > largest {
			largest = item.Count
		}
	}

	rows := make([]statRow, 0, len(items))
	for _, item := range items {
		rows = append(rows, statRow{
			Cloud:        item.Cloud,
			ResourceType: item.ResourceType,
			State:        optString(item.State, absent),
			Count:        item.Count,
			Width:        barWidth(decimal.NewFromInt(item.Count), decimal.NewFromInt(largest)),
		})
	}
	return rows
}

// runRows links every run to the page that shows what it billed.
func runRows(runs []store.Run) []runRow {
	rows := make([]runRow, 0, len(runs))
	for _, run := range runs {
		rows = append(rows, runRow{Run: run, Link: link("/run", "id", run.ID.String())})
	}
	return rows
}

// billingPeriodRow is one billing month of the overview, the page that shows
// it, and the page of the run that closed it, which a month that is not closed
// has none of.
type billingPeriodRow struct {
	Period  store.Period
	Link    string
	RunLink string
}

// billingPeriodRows links every month to its period page, and a closed month
// to the run that closed it.
func billingPeriodRows(periods []store.Period) []billingPeriodRow {
	rows := make([]billingPeriodRow, 0, len(periods))
	for _, billing := range periods {
		row := billingPeriodRow{Period: billing, Link: periodLink(billing.From)}
		if billing.FinalizedRunID != uuid.Nil {
			row.RunLink = link("/run", "id", billing.FinalizedRunID.String())
		}
		rows = append(rows, row)
	}
	return rows
}

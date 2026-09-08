package httpui

import (
	"net/http"
	"time"

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
	Stats    []statRow
	Events   []httpapi.EventStatsItem
	Rejected []httpapi.DeadLetteredEvent
	Periods  []store.Period
	Runs     []runRow
	From     time.Time
	To       time.Time
}

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
		Stats:    statRows(stats.Items),
		Events:   events.Items,
		Rejected: rejected.Items,
		Periods:  periods,
		Runs:     runRows(runs),
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

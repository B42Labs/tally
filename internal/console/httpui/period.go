package httpui

import (
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/engine/period"
)

// monthParameter names the billing month a period page is about, in the
// YYYY-MM form tally-engine reads a --period in.
const monthParameter = "month"

// periodData is one billing month: where it stands, what it bills once its
// standing runs are added up, and every run the engine recorded for it.
type periodData struct {
	Period store.Period
	// FinalizedLink is the page of the run that closed the month, and empty for
	// a month that is not closed.
	FinalizedLink string
	// Totals is what the statements of the standing runs add up to, per
	// currency, sorted by currency.
	Totals []currencyTotal
	Runs   listing[periodRunRow]
}

// periodRunRow is one run of a month, the page that shows it, and what its own
// statements add up to. A run that does not stand is listed for the audit it
// leaves and counts for nothing.
type periodRunRow struct {
	Run        store.Run
	Link       string
	Statements int64
	Totals     []currencyTotal
	Sort       decimal.Decimal
	Standing   bool
}

// periodRunColumns is the run table of a period page.
var periodRunColumns = []column[periodRunRow]{
	textCol("run", func(r periodRunRow) string { return r.Run.ID.String() }),
	textCol("kind", func(r periodRunRow) string { return r.Run.Kind }),
	textCol("status", func(r periodRunRow) string { return r.Run.Status }),
	textCol("pricing", func(r periodRunRow) string { return r.Run.PricingVersion }),
	textCol("started", func(r periodRunRow) string { return zeroStamp(r.Run.StartedAt) }),
	textCol("completed", func(r periodRunRow) string { return zeroStamp(r.Run.CompletedAt) }),
	countCol("statements", func(r periodRunRow) int64 { return r.Statements }),
	numberCol("total", func(r periodRunRow) decimal.Decimal { return r.Sort }),
}

// periodLink is the page of the billing month that starts at from.
func periodLink(from time.Time) string {
	return link("/period", monthParameter, period.Format(from))
}

// readMonth reads the month a period page is about, which it cannot be built
// without.
func readMonth(r *http.Request) (time.Time, error) {
	value, err := stringParameter(r, monthParameter)
	if err != nil {
		return time.Time{}, err
	}
	from, _, err := period.Parse(value)
	if err != nil {
		return time.Time{}, unreadableMonth(monthParameter, value)
	}
	return from, nil
}

// billingPeriod shows one billing month: where it stands, every run the engine
// recorded for it, and what the month bills, which is what the statements of
// its standing runs add up to by the rule the project page adds one project up
// by. A month no run ever opened has no row, and the page answers 404 for it.
func (h *handlers) billingPeriod(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var src sources

	from, err := readMonth(r)
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}

	billing, err := h.store.GetPeriod(ctx, from)
	src.query("GetPeriod")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	recorded, err := h.store.ListRunsForPeriod(ctx, from)
	src.query("ListRunsForPeriod")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	totals, err := h.store.ListRunTotalsForPeriod(ctx, from)
	src.query("ListRunTotalsForPeriod")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	rows, standing := periodRunRows(recorded, totals)
	data := periodData{
		Period: billing,
		Totals: standing,
		Runs:   tabulate(r, "runs", periodRunColumns, rows),
	}
	if billing.FinalizedRunID != uuid.Nil {
		data.FinalizedLink = link("/run", "id", billing.FinalizedRunID.String())
	}

	h.render(w, r, "period", page{
		Title:   "Billing period " + period.Format(from),
		Sources: src,
		Data:    data,
	})
}

// periodRunRows puts every run of a month beside what its statements add up to,
// in the order the runs started, and adds the standing ones up per currency. A
// total whose run the listing does not hold is dropped: it belongs to a run the
// page does not show.
func periodRunRows(recorded []store.Run, totals []store.RunTotal) ([]periodRunRow, []currencyTotal) {
	byRun := make(map[uuid.UUID][]store.RunTotal, len(recorded))
	for _, total := range totals {
		byRun[total.RunID] = append(byRun[total.RunID], total)
	}

	sums := make(map[string]decimal.Decimal)
	rows := make([]periodRunRow, 0, len(recorded))
	for _, run := range recorded {
		row := periodRunRow{
			Run:      run,
			Link:     link("/run", "id", run.ID.String()),
			Standing: slices.Contains(standingStatuses, run.Status),
		}
		for _, total := range byRun[run.ID] {
			row.Totals = append(row.Totals, currencyTotal{Total: total.Total, Currency: total.Currency})
			row.Statements += total.Statements
			if row.Standing {
				sums[total.Currency] = sums[total.Currency].Add(total.Total)
			}
		}
		if len(row.Totals) > 0 {
			row.Sort = row.Totals[0].Total
		}
		rows = append(rows, row)
	}

	var standing []currencyTotal
	for _, currency := range slices.Sorted(maps.Keys(sums)) {
		standing = append(standing, currencyTotal{Total: sums[currency], Currency: currency})
	}
	return rows, standing
}

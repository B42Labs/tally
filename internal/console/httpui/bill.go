package httpui

import (
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/engine/statements"
)

// A statement is rendered as a bill in sections: the project's own line items,
// then one section per related cost. A section opens with a summary table of
// its items, sorted by total descending unless the viewer sorts it otherwise,
// whose rows lead to the item's detail block below. The details follow the
// summary's order and filter, so filtering the summary to one resource shows
// that resource's detail alone. A detail block is a details element that
// starts folded and holds one row per metric of every period, with the
// period's total as a row of its own. The resource of a summary row leads to
// its block unfolded: the link names the block in the open parameter and in
// its fragment, so the page comes back with that block open and the browser
// scrolls to it.
const (
	// openParameter names the detail block the page renders unfolded.
	openParameter = "open"
	// periodTotal is the key the engine writes a period's total under, beside
	// the metrics, in the cost map of the period.
	periodTotal = "total"
	// itemsTable is the summary table of the statement's own line items, and
	// itemAnchor the prefix of their detail blocks. A related cost's summary
	// and blocks carry its index, so every table of the page has a name of its
	// own for its sort and filter parameters.
	itemsTable = "items"
	itemAnchor = "li"
)

// billSection is one section of the bill: the summary of its items and, for
// a related cost, what the section is about.
type billSection struct {
	Currency string
	Summary  listing[itemRow]
	Related  *statements.RelatedCost
}

// itemRow is one line item, in the summary table and as the detail block the
// summary row leads to.
type itemRow struct {
	Anchor string
	// OpenLink is the page with this block unfolded and scrolled to.
	OpenLink     string
	ResourceType string
	ResourceID   string
	// Description is what the engine wrote about the item beyond its type and
	// id, the flavour for example, and empty when it only repeats them.
	Description string
	Hours       decimal.Decimal
	Total       decimal.Decimal
	// Open is whether the detail block is rendered unfolded, which the open
	// parameter decides for one block at a time.
	Open         bool
	ResourceLink string
	Metrics      listing[metricRow]
}

// metricRow is one metric of one period of an item, or the period's total,
// which carries no usage.
type metricRow struct {
	State    string
	Hours    decimal.Decimal
	Metric   string
	Usage    decimal.Decimal
	Cost     decimal.Decimal
	Modifier decimal.Decimal
	Subtotal bool
}

// itemColumns is the summary table of a section.
var itemColumns = []column[itemRow]{
	textCol("resource type", func(r itemRow) string { return r.ResourceType }),
	textCol("resource", func(r itemRow) string { return r.ResourceID }),
	textCol("description", func(r itemRow) string { return r.Description }),
	numberCol("hours", func(r itemRow) decimal.Decimal { return r.Hours }),
	numberCol("total", func(r itemRow) decimal.Decimal { return r.Total }),
}

// metricColumns is the detail table of an item.
var metricColumns = []column[metricRow]{
	textCol("state", func(r metricRow) string { return r.State }),
	numberCol("hours", func(r metricRow) decimal.Decimal { return r.Hours }),
	textCol("metric", func(r metricRow) string { return r.Metric }),
	numberCol("usage", func(r metricRow) decimal.Decimal { return r.Usage }),
	numberCol("cost", func(r metricRow) decimal.Decimal { return r.Cost }),
	numberCol("state modifier", func(r metricRow) decimal.Decimal { return r.Modifier }),
}

// buildBillSection lays one section out. The table name is what the summary's
// sort and filter parameters carry, and the anchor prefix is what the detail
// blocks are addressed by, the index of the item appended. The cloud is what
// the resource links are built with, and no link is built without it, which
// is a statement whose key the page could not read.
func buildBillSection(
	r *http.Request, table, anchor, cloud, currency string, items []statements.LineItem,
) billSection {
	rows := make([]itemRow, 0, len(items))
	for index, item := range items {
		rows = append(rows, buildItemRow(r, fmt.Sprintf("%s-%d", anchor, index), cloud, item))
	}
	return billSection{
		Currency: currency,
		Summary:  tabulateBy(r, table, itemColumns, rows, descending+"total"),
	}
}

// buildItemRow lays one item out, summary and detail alike.
func buildItemRow(r *http.Request, anchor, cloud string, item statements.LineItem) itemRow {
	values := r.URL.Query()
	opened := maps.Clone(values)
	opened.Set(openParameter, anchor)
	row := itemRow{
		Anchor:       anchor,
		OpenLink:     href(r.URL.Path, opened) + "#" + anchor,
		ResourceType: item.ResourceType,
		ResourceID:   item.ResourceID,
		Total:        item.Total.Decimal,
		Open:         values.Get(openParameter) == anchor,
	}
	if item.Description != item.ResourceType+" "+item.ResourceID {
		row.Description = item.Description
	}
	if cloud != "" {
		row.ResourceLink = link("/resource", "cloud", cloud, "type", item.ResourceType, "id", item.ResourceID)
	}

	var metrics []metricRow
	for _, period := range item.Periods {
		row.Hours = row.Hours.Add(period.Hours.Decimal)
		metrics = append(metrics, metricRows(period)...)
	}
	row.Metrics = tabulate(r, anchor, metricColumns, metrics)
	return row
}

// metricRows flattens one period: one row per metric the period carries usage
// or cost for, in the order of their names, then the period's total. The total
// is the one the engine wrote where it wrote one, and the metrics' sum where
// it did not.
func metricRows(period statements.Period) []metricRow {
	names := map[string]struct{}{}
	for name := range period.Usage {
		names[name] = struct{}{}
	}
	for name := range period.Cost {
		if name != periodTotal {
			names[name] = struct{}{}
		}
	}

	rows := make([]metricRow, 0, len(names)+1)
	sum := decimal.Zero
	for _, name := range slices.Sorted(maps.Keys(names)) {
		cost := period.Cost[name].Decimal
		sum = sum.Add(cost)
		rows = append(rows, metricRow{
			State:    period.State,
			Hours:    period.Hours.Decimal,
			Metric:   name,
			Usage:    period.Usage[name].Decimal,
			Cost:     cost,
			Modifier: period.StateModifier.Decimal,
		})
	}

	total := sum
	if written, ok := period.Cost[periodTotal]; ok {
		total = written.Decimal
	}
	return append(rows, metricRow{
		State:    period.State,
		Hours:    period.Hours.Decimal,
		Metric:   periodTotal,
		Cost:     total,
		Modifier: period.StateModifier.Decimal,
		Subtotal: true,
	})
}

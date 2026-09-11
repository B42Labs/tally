package httpui

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/engine/corrections"
	"github.com/b42labs/tally/internal/engine/runs"
)

// creditNoteData is one stored credit note rendered as what it is. A correction
// run stores a credit note under each key rather than a statement: per resource
// the dimensions the correction and the run it corrects disagree on, with what
// that run billed, what the correction rated and the difference. The page lays
// it out the way the bill lays out a statement, its own items as one section
// and one section per related cost, with a folding block per item that holds
// one row per dimension.
type creditNoteData struct {
	Key          string
	Cloud        string
	Project      string
	RunID        uuid.UUID
	RunLink      string
	CorrectsLink string
	Note         corrections.CreditNote
	Adjustments  listing[corrections.AdjustmentChange]
	Items        creditSection
	Related      []creditSection
	// Export is the file the JSON export writes for the note, or why there is
	// none.
	Export exportView
}

// creditSection is one section of a note: the summary of its items and, for a
// related cost, what the section is about.
type creditSection struct {
	Currency string
	Summary  listing[creditItemRow]
	Related  *corrections.RelatedCost
}

// creditItemRow is one item of a note, in the summary table and as the detail
// block the summary row leads to.
type creditItemRow struct {
	Anchor string
	// OpenLink is the page with this block unfolded and scrolled to.
	OpenLink     string
	ResourceType string
	ResourceID   string
	Total        decimal.Decimal
	// Open is whether the detail block is rendered unfolded, which the open
	// parameter decides for one block at a time.
	Open         bool
	ResourceLink string
	Dimensions   listing[changeRow]
}

// changeRow is one dimension of one item of a note: what the corrected run
// billed, what the correction rated, and the difference the project is credited
// or debited for.
type changeRow struct {
	Dimension string
	Old       decimal.Decimal
	New       decimal.Decimal
	Delta     decimal.Decimal
}

// The columns of a credit note's tables: the summary of a section, the
// dimensions of an item, and the adjustment changes of the note.
var (
	creditItemColumns = []column[creditItemRow]{
		textCol("resource type", func(r creditItemRow) string { return r.ResourceType }),
		textCol("resource", func(r creditItemRow) string { return r.ResourceID }),
		numberCol("total", func(r creditItemRow) decimal.Decimal { return r.Total }),
	}
	changeColumns = []column[changeRow]{
		textCol("dimension", func(r changeRow) string { return r.Dimension }),
		numberCol("old", func(r changeRow) decimal.Decimal { return r.Old }),
		numberCol("new", func(r changeRow) decimal.Decimal { return r.New }),
		numberCol("delta", func(r changeRow) decimal.Decimal { return r.Delta }),
	}
	adjustmentChangeColumns = []column[corrections.AdjustmentChange]{
		textCol("type", func(r corrections.AdjustmentChange) string { return r.Type }),
		textCol("relation type", func(r corrections.AdjustmentChange) string { return r.RelationType }),
		textCol("target", func(r corrections.AdjustmentChange) string { return r.RelationTarget }),
		textCol("scope", func(r corrections.AdjustmentChange) string { return r.Scope }),
		numberCol("rate", func(r corrections.AdjustmentChange) decimal.Decimal { return r.Rate.Decimal }),
		numberCol("old", func(r corrections.AdjustmentChange) decimal.Decimal { return r.Old.Decimal }),
		numberCol("new", func(r corrections.AdjustmentChange) decimal.Decimal { return r.New.Decimal }),
		numberCol("delta", func(r corrections.AdjustmentChange) decimal.Decimal { return r.Delta.Decimal }),
	}
)

// storesCreditNotes reports whether a run stores a credit note under each key
// rather than a statement. It is the rule the export decodes a stored document
// by, so the bill and the export block under its head read the same document.
func storesCreditNotes(run store.Run) bool {
	return run.Kind == runs.KindCorrection
}

// creditNote shows the credit note a correction run stored under key. The run
// is already read, by the statement handler that chose this page for its kind.
// The page decodes the note and prints it, and computes none of it again.
func (h *handlers) creditNote(
	w http.ResponseWriter, r *http.Request, run store.Run, key string, stored store.Statement, src sources,
) {
	note, err := readCreditNote(stored)
	if err != nil {
		h.failFrom(w, r,
			documentFailed(fmt.Errorf("reading the credit note of %s in run %s: %w", key, run.ID, err)), src)
		return
	}

	cloud, project, linkCloud := splitKey(key)
	data := creditNoteData{
		Key:          key,
		Cloud:        cloud,
		Project:      project,
		RunID:        run.ID,
		RunLink:      link("/run", "id", run.ID.String()),
		CorrectsLink: link("/run", "id", note.CorrectsRunID),
		Note:         note,
		Adjustments:  tabulate(r, "adjustments", adjustmentChangeColumns, note.Adjustments),
		Export:       buildExportView(run, key, stored.Document),
	}

	data.Items = buildCreditSection(r, itemsTable, itemAnchor, linkCloud, note.Currency, note.LineItems)
	for index := range note.RelatedCosts {
		related := &note.RelatedCosts[index]
		prefix := fmt.Sprintf("rc-%d", index)
		section := buildCreditSection(r, prefix, prefix+"-"+itemAnchor, linkCloud, note.Currency, related.LineItems)
		section.Related = related
		data.Related = append(data.Related, section)
	}

	h.render(w, r, "creditnote", page{
		Title:   "Credit note " + key,
		Sources: src,
		Data:    data,
	})
}

// readCreditNote decodes a stored document and refuses one that is not a credit
// note. JSON accepts any object into a struct of optional members, so a
// statement stored under a correction run would render as a note of nothing
// rather than fail; a credit note is the document that names the run it
// corrects.
func readCreditNote(stored store.Statement) (corrections.CreditNote, error) {
	var note corrections.CreditNote
	if err := json.Unmarshal(stored.Document, &note); err != nil {
		return corrections.CreditNote{}, err
	}
	if note.Currency == "" || note.BillingPeriod.From == "" || note.CorrectsRunID == "" {
		return corrections.CreditNote{}, errors.New("not a credit note document")
	}
	return note, nil
}

// buildCreditSection lays one section of a note out, the way buildBillSection
// lays out one of a statement. The summary opens in the note's own order rather
// than by total: a note credits some items and debits others, so neither
// direction of the total puts the largest movement first, and a click on the
// heading still sorts.
func buildCreditSection(
	r *http.Request, table, anchor, cloud, currency string, items []corrections.LineItem,
) creditSection {
	rows := make([]creditItemRow, 0, len(items))
	for index, item := range items {
		rows = append(rows, buildCreditItemRow(r, fmt.Sprintf("%s-%d", anchor, index), cloud, item))
	}
	return creditSection{
		Currency: currency,
		Summary:  tabulate(r, table, creditItemColumns, rows),
	}
}

// buildCreditItemRow lays one item of a note out, summary and detail alike: one
// row per dimension, in the order of their names. No resource link is built
// without the cloud, which is a key the page could not read.
func buildCreditItemRow(r *http.Request, anchor, cloud string, item corrections.LineItem) creditItemRow {
	opening, open := openLink(r, anchor)
	row := creditItemRow{
		Anchor:       anchor,
		OpenLink:     opening,
		ResourceType: item.ResourceType,
		ResourceID:   item.ResourceID,
		Total:        item.Total.Decimal,
		Open:         open,
	}
	if cloud != "" {
		row.ResourceLink = link("/resource", "cloud", cloud, "type", item.ResourceType, "id", item.ResourceID)
	}

	changes := make([]changeRow, 0, len(item.Dimensions))
	for _, name := range slices.Sorted(maps.Keys(item.Dimensions)) {
		change := item.Dimensions[name]
		changes = append(changes, changeRow{
			Dimension: name,
			Old:       change.Old.Decimal,
			New:       change.New.Decimal,
			Delta:     change.Delta.Decimal,
		})
	}
	row.Dimensions = tabulate(r, anchor, changeColumns, changes)
	return row
}

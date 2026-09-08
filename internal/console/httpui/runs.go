package httpui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/engine/statements"
)

// runData is one metering run: what it billed, and what it moved if it
// corrected an earlier one.
type runData struct {
	Run          store.Run
	CorrectsLink string
	CatalogLink  string
	Statements   []statementBar
	Deltas       []store.Delta
}

// statementBar is one project's total of a run, drawn against the largest total
// the run produced.
type statementBar struct {
	Row   store.StatementRow
	Width int64
	Link  string
}

// statementData is one stored statement document rendered as the bill it is.
type statementData struct {
	Key      string
	Cloud    string
	Project  string
	RunID    uuid.UUID
	RunLink  string
	Document statements.Document
}

// run shows what one metering run produced. The deltas of a correction run are
// a section of their own, and a run that moved nothing has none.
func (h *handlers) run(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var src sources

	id, err := uuidParameter(r, "id")
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}

	run, err := h.store.GetRun(ctx, id)
	src.query("GetRun")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	billed, err := h.store.ListStatements(ctx, id)
	src.query("ListStatements")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	deltas, err := h.store.ListCorrectionDeltas(ctx, id)
	src.query("ListCorrectionDeltas")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	data := runData{Run: run, Statements: statementBars(id, billed), Deltas: deltas}
	if run.CorrectsRunID != uuid.Nil {
		data.CorrectsLink = link("/run", "id", run.CorrectsRunID.String())
	}
	if run.PricingVersion != "" {
		data.CatalogLink = link("/catalog", "version", run.PricingVersion)
	}

	h.render(w, r, "run", page{
		Title:   "Run " + run.ID.String(),
		Sources: src,
		Data:    data,
	})
}

// statement shows one stored document as a bill. The document is what the run
// wrote: the page decodes it and prints it, and computes none of it again.
func (h *handlers) statement(w http.ResponseWriter, r *http.Request) {
	var src sources

	runID, err := uuidParameter(r, "run")
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}
	key, err := stringParameter(r, "key")
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}

	stored, err := h.store.GetStatement(r.Context(), runID, key)
	src.query("GetStatement")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	document, err := readStatement(stored)
	if err != nil {
		h.failFrom(w, r,
			documentFailed(fmt.Errorf("reading the statement of %s in run %s: %w", key, runID, err)), src)
		return
	}

	data := statementData{
		Key:      key,
		Cloud:    absent,
		Project:  key,
		RunID:    runID,
		RunLink:  link("/run", "id", runID.String()),
		Document: document,
	}
	// A key this cannot read is shown as it is stored: the page is about that
	// stored row, and a guessed pair would name one nothing was stored under.
	if cloud, project, keyErr := statements.ParseKey(key); keyErr == nil {
		data.Cloud, data.Project = cloud, project
	}

	h.render(w, r, "statement", page{
		Title:   "Statement " + key,
		Sources: src,
		Data:    data,
	})
}

// readStatement decodes a stored document and refuses one that is not a
// statement. JSON accepts any object into a struct of pointers and optional
// members, so a document without a currency or a billing period would render as
// a bill of nothing rather than fail.
func readStatement(stored store.Statement) (statements.Document, error) {
	var document statements.Document
	if err := json.Unmarshal(stored.Document, &document); err != nil {
		return statements.Document{}, err
	}
	if document.Currency == "" || document.BillingPeriod.From == "" {
		return statements.Document{}, errors.New("not a statement document")
	}
	return document, nil
}

// statementBars scales every total against the largest one of the run. The run
// id is not on a row: the rows are the statements of one run, and it is what
// the link back to each document is keyed by beside the project.
func statementBars(runID uuid.UUID, rows []store.StatementRow) []statementBar {
	largest := decimal.Zero
	for _, row := range rows {
		if row.Total.GreaterThan(largest) {
			largest = row.Total
		}
	}

	bars := make([]statementBar, 0, len(rows))
	for _, row := range rows {
		bars = append(bars, statementBar{
			Row:   row,
			Width: barWidth(row.Total, largest),
			Link:  link("/statement", "run", runID.String(), "key", row.Key),
		})
	}
	return bars
}

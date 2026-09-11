package httpui

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/store"
	projectcore "github.com/b42labs/tally/internal/core/project"
	"github.com/b42labs/tally/internal/engine/export"
)

// A run's export is the two documents tally-engine export --format json writes
// beside the statements: run.json, the index that names every statement file
// and what it totals, and kickbacks.json, what the run settles for its
// partners. The console serves the export's own bytes for both, rendered from
// the export's own read of the run, the way it serves the file of a statement.
const (
	// runFileRoute serves the run.json of a run on its own, as JSON rather than
	// as a page.
	runFileRoute = "/run.json"
	// kickbacksFileRoute serves the kickbacks.json of a run the same way.
	kickbacksFileRoute = "/kickbacks.json"
)

// runExportView is what the run page says about the export of its run: the two
// files and what the run owes each partner, or, where the export writes
// nothing, why.
type runExportView struct {
	// Note says why the page shows no file: a run the export does not read, or
	// one whose statements it refuses.
	Note       string
	Index      exportView
	Settlement exportView
	Partners   listing[partnerOwed]
}

// partnerOwed is what a run settles for one partner in one currency, as the
// settlement the export renders says, and the page of that partner.
type partnerOwed struct {
	Beneficiary string
	Currency    string
	Projects    int64
	Total       decimal.Decimal
	Link        string
}

// partnerOwedColumns is the settlement table of a run page.
var partnerOwedColumns = []column[partnerOwed]{
	textCol("partner", func(r partnerOwed) string { return r.Beneficiary }),
	textCol("currency", func(r partnerOwed) string { return r.Currency }),
	countCol("projects", func(r partnerOwed) int64 { return r.Projects }),
	numberCol("kickback", func(r partnerOwed) decimal.Decimal { return r.Total }),
}

// settlementDocument is the part of kickbacks.json the settlement table prints:
// per partner and currency, what the partner is owed and how many projects that
// came off. The breakdown under each entry is not read, because the table
// prints none of it.
type settlementDocument struct {
	Beneficiaries []struct {
		Beneficiary   string          `json:"beneficiary"`
		Currency      string          `json:"currency"`
		KickbackTotal decimal.Decimal `json:"kickback_total"`
		Projects      int64           `json:"projects"`
	} `json:"beneficiaries"`
}

// renderRunExport renders the two documents an export of the run writes beside
// the statements. The index comes first: a statement the export refuses
// refuses the whole export, and an export that refuses the run writes no
// settlement either.
func renderRunExport(run export.Run) (index, settlement []byte, err error) {
	if index, err = export.RunJSON(run); err != nil {
		return nil, nil, err
	}
	if settlement, err = export.KickbacksJSON(run); err != nil {
		return nil, nil, err
	}
	return index, settlement, nil
}

// readSettlement reads what a run owes each partner off the kickbacks.json the
// export rendered for it, in the order the document holds them, which is by
// partner and then by currency. The totals are the export's: the page adds
// nothing up again.
func readSettlement(body []byte) ([]partnerOwed, error) {
	var document settlementDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, err
	}

	owed := make([]partnerOwed, 0, len(document.Beneficiaries))
	for _, entry := range document.Beneficiaries {
		owed = append(owed, partnerOwed{
			Beneficiary: entry.Beneficiary,
			Currency:    entry.Currency,
			Projects:    entry.Projects,
			Total:       entry.KickbackTotal,
			// A partner is a registry row whose cloud is its platform, and the
			// project page resolves it by that pair.
			Link: projectLink(projectcore.PlatformPartner, entry.Beneficiary),
		})
	}
	return owed, nil
}

// buildRunExport renders the export of a run for its page. A run the export
// does not read has no file, and nothing is read for it; a run that moved out
// of the statuses the export reads between the page's two reads says so the
// same way. Any other failed read is returned, and the page fails as it does
// for its other reads. A run whose statements the export refuses leaves the
// page standing: the page says what the export said, and the two routes are
// where that refusal is an error.
func (h *handlers) buildRunExport(r *http.Request, run store.Run, src *sources) (runExportView, error) {
	if !export.Exportable(run.Status) {
		return runExportView{Note: notExported(run).Error()}, nil
	}

	loaded, err := h.store.LoadRunExport(r.Context(), run.ID)
	src.query("LoadRunExport")
	if errors.Is(err, export.ErrRunNotExportable) {
		return runExportView{Note: err.Error()}, nil
	}
	if err != nil {
		return runExportView{}, err
	}

	index, settlement, err := renderRunExport(loaded)
	if err != nil {
		return runExportView{Note: "the export refuses this run: " + err.Error()}, nil
	}
	owed, err := readSettlement(settlement)
	if err != nil {
		return runExportView{Note: "the export refuses this run: " + err.Error()}, nil
	}

	id := run.ID.String()
	return runExportView{
		Index:      fileView(export.RunFileName, runFileRoute, id, index),
		Settlement: fileView(export.KickbacksJSONFileName, kickbacksFileRoute, id, settlement),
		Partners:   tabulate(r, "settlement", partnerOwedColumns, owed),
	}, nil
}

// fileView is one file of a run's export as the page prints it: its name, its
// bytes, and the two links to the route that serves it.
func fileView(name, route, runID string, body []byte) exportView {
	return exportView{
		File:         name,
		JSON:         string(body),
		Size:         len(body),
		OpenLink:     link(route, "run", runID),
		DownloadLink: link(route, "run", runID, downloadParameter, "1"),
	}
}

// runFile serves the run.json of a run.
func (h *handlers) runFile(w http.ResponseWriter, r *http.Request) {
	h.serveRunFile(w, r, export.RunFileName)
}

// kickbacksFile serves the kickbacks.json of a run.
func (h *handlers) kickbacksFile(w http.ResponseWriter, r *http.Request) {
	h.serveRunFile(w, r, export.KickbacksJSONFileName)
}

// serveRunFile serves one of the two files the export writes for a run beside
// its statements: the bytes alone, as JSON, opened in the browser or, with the
// download parameter, saved under the name the export gives the file. A run the
// export does not read has no such file and is answered 404, and a run whose
// statements the export refuses 503, both on the error page.
func (h *handlers) serveRunFile(w http.ResponseWriter, r *http.Request, name string) {
	ctx := r.Context()
	var src sources

	runID, err := uuidParameter(r, "run")
	if err != nil {
		h.failFrom(w, r, err, src)
		return
	}

	run, err := h.store.GetRun(ctx, runID)
	src.query("GetRun")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}
	if !export.Exportable(run.Status) {
		h.fail(w, r, http.StatusNotFound, "the export writes no file for this run", notExported(run), src)
		return
	}

	loaded, err := h.store.LoadRunExport(ctx, runID)
	src.query("LoadRunExport")
	if errors.Is(err, export.ErrRunNotExportable) {
		h.fail(w, r, http.StatusNotFound, "the export writes no file for this run", err, src)
		return
	}
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	index, settlement, err := renderRunExport(loaded)
	if err != nil {
		h.failFrom(w, r, documentFailed(err), src)
		return
	}
	body := index
	if name == export.KickbacksJSONFileName {
		body = settlement
	}

	disposition := "inline"
	if r.URL.Query().Get(downloadParameter) != "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Content-Disposition", contentDisposition(disposition, name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

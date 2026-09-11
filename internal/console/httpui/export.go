package httpui

import (
	"fmt"
	"mime"
	"net/http"
	"strings"

	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/engine/export"
	"github.com/b42labs/tally/internal/engine/statements"
)

// A statement's export is the file tally-engine export --format json writes for
// it, and the console serves those bytes rather than the stored document. The
// engine database keeps a document as JSONB, which holds the members in an
// order of its own, and the export renders every document again in the order
// the concept prints it in: the stored bytes carry the right values in a file
// no ERP receives. The rendering is the export's own, called on the stored
// document, so the page, the route and an exported file hold the same bytes.
const (
	// exportRoute serves the file on its own, as JSON rather than as a page.
	exportRoute = "/statement.json"
	// downloadParameter turns the answer of exportRoute into an attachment.
	downloadParameter = "download"
	// jsonContentType is what exportRoute answers with.
	jsonContentType = "application/json"
)

// exportView is what the statement page says about the file the export writes
// for the statement: the file, its bytes and the two links to it, or, where
// there is no file, why.
type exportView struct {
	File         string
	JSON         string
	Size         int
	OpenLink     string
	DownloadLink string
	// Note says why the page shows no file: a run the export does not read, or
	// a stored document the export refuses.
	Note string
}

// buildExportView renders the file the export writes for one statement of a
// run. A document the export refuses leaves the bill standing: the page says
// what the export said, and the route is where that refusal is an error.
func buildExportView(run store.Run, key string, document []byte) exportView {
	if !export.Exportable(run.Status) {
		return exportView{Note: notExported(run).Error()}
	}
	body, err := export.RenderStatement(run.ID, run.Kind, statements.Statement{Key: key, Document: document})
	if err != nil {
		return exportView{Note: "the export refuses this statement: " + err.Error()}
	}

	runID := run.ID.String()
	return exportView{
		File:         export.DocumentFileName(run.Kind, key),
		JSON:         string(body),
		Size:         len(body),
		OpenLink:     link(exportRoute, "run", runID, "key", key),
		DownloadLink: link(exportRoute, "run", runID, "key", key, downloadParameter, "1"),
	}
}

// notExported says why a run's statements have no file, in the words the
// export refuses such a run with.
func notExported(run store.Run) error {
	return fmt.Errorf("run %s is %s, and only a completed or finalized run is exported", run.ID, run.Status)
}

// statementExport serves the file the export writes for one statement: the
// bytes alone, as JSON, opened in the browser or, with the download parameter,
// saved under the name the export gives the file. A run the export does not
// read has no such file and is answered 404, and a document the export refuses
// 503, both on the error page.
func (h *handlers) statementExport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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

	stored, err := h.store.GetStatement(ctx, runID, key)
	src.query("GetStatement")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}

	run, err := h.store.GetRun(ctx, runID)
	src.query("GetRun")
	if err != nil {
		h.failFrom(w, r, storeFailed(err), src)
		return
	}
	if !export.Exportable(run.Status) {
		h.fail(w, r, http.StatusNotFound, "the export writes no file for this statement", notExported(run), src)
		return
	}

	body, err := export.RenderStatement(run.ID, run.Kind, statements.Statement{Key: key, Document: stored.Document})
	if err != nil {
		h.failFrom(w, r, documentFailed(err), src)
		return
	}

	disposition := "inline"
	if r.URL.Query().Get(downloadParameter) != "" {
		disposition = "attachment"
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Content-Disposition", contentDisposition(disposition, export.DocumentFileName(run.Kind, key)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// contentDisposition names the file twice. The plain filename parameter is for
// a client that reads nothing else, curl -J for one. A browser takes filename*
// over it, whose value RFC 8187 percent-encodes, and that is the one that keeps
// the name: Chrome reads a percent escape in the plain parameter as one, so the
// %2F the export puts between the cloud and the project would come back as a
// slash and the file would be saved under a name the export never gave it.
func contentDisposition(disposition, name string) string {
	var encoded strings.Builder
	for i := range len(name) {
		c := name[i]
		if isAttrChar(c) {
			encoded.WriteByte(c)
			continue
		}
		fmt.Fprintf(&encoded, "%%%02X", c)
	}
	return mime.FormatMediaType(disposition, map[string]string{"filename": name}) +
		"; filename*=UTF-8''" + encoded.String()
}

// isAttrChar reports whether RFC 8187 lets a byte stand for itself in an
// extended parameter value. Every other byte is percent-encoded.
func isAttrChar(c byte) bool {
	if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
		return true
	}
	return strings.IndexByte("!#$&+-.^_`|~", c) >= 0
}

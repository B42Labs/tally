package httpui

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/b42labs/tally/internal/console/reporting"
)

// paramError is a query parameter a page cannot be built without: one that was
// not sent, or one that was sent in a form the page cannot read.
type paramError struct {
	name   string
	reason string
}

// Error names the parameter and what is wrong with it, which is what the viewer
// has to fix in the URL.
func (e *paramError) Error() string {
	return fmt.Sprintf("the parameter %s %s", e.name, e.reason)
}

// missingParameter reports a parameter the request did not carry.
func missingParameter(name string) error {
	return &paramError{name: name, reason: "is missing"}
}

// unreadableInstant reports a bound or an instant that was sent in a form no
// page can read.
func unreadableInstant(name string, err error) error {
	return &paramError{name: name, reason: "is not an instant: " + err.Error()}
}

// unreadableUUID reports a parameter that was sent but is no id.
func unreadableUUID(name string, err error) error {
	return &paramError{name: name, reason: fmt.Sprintf("is not a UUID: %v", err)}
}

// unreadableMonth reports a month that was sent in a form other than the
// YYYY-MM a billing period is named by.
func unreadableMonth(name, value string) error {
	return &paramError{name: name, reason: fmt.Sprintf("is not a YYYY-MM month: %q", value)}
}

// apiError tags a failure the Reporting API returned, storeError one the engine
// database returned, and documentError a stored document that did not decode
// into what the page renders. A handler tags an error where it receives it, so
// the error page says which side failed because the call that failed said so,
// not because something matched on the message.
type apiError struct{ err error }

// Error reports the API's own diagnosis unchanged.
func (e apiError) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped error, so a problem document stays reachable
// through errors.As.
func (e apiError) Unwrap() error { return e.err }

// apiFailed tags err as a failed Reporting API call.
func apiFailed(err error) error { return apiError{err: err} }

// storeError tags a failed read of the engine database.
type storeError struct{ err error }

// Error reports the store's message, which names the query that failed.
func (e storeError) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped error, so errors.Is still finds pgx.ErrNoRows.
func (e storeError) Unwrap() error { return e.err }

// storeFailed tags err as a failed read of the engine database.
func storeFailed(err error) error { return storeError{err: err} }

// documentError tags a stored document the page could not read: bytes that are
// no JSON, or JSON that is not the document the page renders.
type documentError struct{ err error }

// Error reports what was read and why it could not be.
func (e documentError) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped decoding failure.
func (e documentError) Unwrap() error { return e.err }

// documentFailed tags err as a stored document that could not be read.
func documentFailed(err error) error { return documentError{err: err} }

// lookupError tags a pair nothing is registered under: a cloud and external
// id the project list, filtered by both, answered with no project for.
type lookupError struct{ err error }

// Error names the pair that was looked up.
func (e lookupError) Error() string { return e.err.Error() }

// Unwrap exposes the wrapped error.
func (e lookupError) Unwrap() error { return e.err }

// nothingRegistered tags err as a lookup that found nothing.
func nothingRegistered(err error) error { return lookupError{err: err} }

// failFrom answers a failed page by what the error is tagged as. The tag
// decides the status and the heading; the error itself, wrapped as the handler
// wrapped it, is what the page and the log carry.
func (h *handlers) failFrom(w http.ResponseWriter, r *http.Request, err error, src sources) {
	var param *paramError
	if errors.As(err, &param) {
		h.fail(w, r, http.StatusBadRequest, "a required parameter is missing or unreadable", err, src)
		return
	}

	var fromAPI apiError
	if errors.As(err, &fromAPI) {
		var problem *reporting.ProblemError
		if errors.As(err, &problem) {
			switch problem.Status {
			case http.StatusNotFound:
				h.fail(w, r, http.StatusNotFound, "the Reporting API holds nothing under that id", err, src)
				return
			case http.StatusUnauthorized:
				// The console cannot fix its own credential, so the failure is
				// this deployment's rather than the request's.
				h.fail(w, r, http.StatusBadGateway, "the Reporting API refused the configured token", err, src)
				return
			}
		}
		h.fail(w, r, http.StatusBadGateway, "the Reporting API call failed", err, src)
		return
	}

	var fromLookup lookupError
	if errors.As(err, &fromLookup) {
		h.fail(w, r, http.StatusNotFound, "the Reporting API registers nothing under that pair", err, src)
		return
	}

	var fromStore storeError
	if errors.As(err, &fromStore) {
		if errors.Is(err, pgx.ErrNoRows) {
			h.fail(w, r, http.StatusNotFound, "nothing is stored under that key", err, src)
			return
		}
		h.fail(w, r, http.StatusServiceUnavailable, "the engine database could not be read", err, src)
		return
	}

	var fromDocument documentError
	if errors.As(err, &fromDocument) {
		h.fail(w, r, http.StatusServiceUnavailable, "the stored document could not be read", err, src)
		return
	}

	h.fail(w, r, http.StatusInternalServerError, "the page could not be built", err, src)
}

// errorData is what the error page renders: the whole wrapped error as text.
type errorData struct {
	Message string
}

// fail answers one failed page and is the only path that writes a failure to
// the viewer. The error text is on the page rather than behind a correlation
// id: this console has one viewer, that viewer runs the demo, and the fastest
// way to tell them what broke is to print it.
//
// The same error goes to the log whole, at ERROR for a failure of the console
// or the sides it reads, at WARN for a request that asked for something wrong.
func (h *handlers) fail(
	w http.ResponseWriter, r *http.Request, status int, heading string, err error, src sources,
) {
	level := slog.LevelWarn
	if status >= http.StatusInternalServerError {
		level = slog.LevelError
	}
	h.logger.Log(r.Context(), level, heading, "status", status, "path", r.URL.Path, "error", err)

	tpl, ok := h.pages[errorPage]
	if !ok {
		h.logger.Error("the error page is not parsed", "path", r.URL.Path)
		http.Error(w, heading, status)
		return
	}

	var body bytes.Buffer
	p := page{Title: heading, Sources: src, Data: errorData{Message: err.Error()}}.forRequest(r)
	// The error page is what every other page falls back to, so it cannot fall
	// back to itself. What is left when it fails is the status and one line.
	if execErr := tpl.ExecuteTemplate(&body, "layout", p); execErr != nil {
		h.logger.Error("rendering the error page", "path", r.URL.Path, "error", execErr)
		http.Error(w, heading, status)
		return
	}

	w.Header().Set("Content-Type", htmlContentType)
	w.WriteHeader(status)
	_, _ = body.WriteTo(w)
}

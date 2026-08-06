package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// server implements the generated ServerInterface, which grows a method per
// operation as the contract does. The probes need nothing but the database
// seam, so an Options that leaves the fields below unset still serves them.
type server struct {
	db     DB
	health *healthTracker
	// store is what the handlers that write run their transactions on: an event
	// batch and the audit row naming it land together or not at all.
	store *store.Store
	// queries is the pool-bound handle the reads go through.
	queries *sqlcgen.Queries
	// pipeline is what a submitted event batch is put through.
	pipeline *ingest.Pipeline
}

// writeJSON sends one successful response body. Every success this API has is a
// 200 carrying JSON, so the status is not a parameter.
//
// The documents passed here are the generated response types, so encoding fails
// only when the connection does, and the status line is already gone by then.
func writeJSON(w http.ResponseWriter, document any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(document)
}

// writeInternal answers a failure the caller can do nothing about. The cause is
// logged against the request under message, and the body carries detail alone:
// what broke inside this service is not something a client is told.
func writeInternal(ctx context.Context, w http.ResponseWriter, message string, err error, detail string) {
	Logger(ctx).Error(message, "error", err)
	problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "Internal error", detail)
}

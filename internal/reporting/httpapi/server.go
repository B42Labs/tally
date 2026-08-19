package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/b42labs/tally/internal/core/health"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/ingest"
	"github.com/b42labs/tally/internal/reporting/metrics"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// server implements the generated ServerInterface, which grows a method per
// operation as the contract does. The probes need nothing but the database
// seam, so an Options that leaves the fields below unset still serves them.
type server struct {
	db     DB
	health *health.Tracker
	// store is what the handlers that write run their transactions on: an event
	// batch and the audit row naming it land together or not at all.
	store *store.Store
	// queries is the pool-bound handle the reads go through.
	queries *sqlcgen.Queries
	// pipeline is what a submitted event batch is put through.
	pipeline *ingest.Pipeline
	// attributingTypes are the relation types the cycle guard walks. An empty
	// list disables the guard.
	attributingTypes []string
	// syncer runs the reconciliation of one cloud the internal sync route asks
	// for.
	syncer *reconciliation.Syncer
	// metrics holds the instruments the scrape route serves. A nil value is a
	// build without instrumentation, which that route answers 404.
	metrics *metrics.Metrics
	// metricsEnabled is whether the scrape route serves the instruments at all.
	metricsEnabled bool
	// now is the clock the project summary clips an open interval at, so that a
	// window reaching into the future reports no time that has not been served
	// yet. It comes from Options.Now, which is what pins it in a test.
	now func() time.Time
}

// writeJSON sends one successful response body under the 200 almost every
// success of this API carries.
//
// The documents passed here are the generated response types, so encoding fails
// only when the connection does, and the status line is already gone by then.
func writeJSON(w http.ResponseWriter, document any) {
	writeJSONStatus(w, http.StatusOK, document)
}

// writeJSONStatus is writeJSON for the successes that answer another status,
// the 201 of a registration for example. A caller that has already set response
// headers keeps them: nothing is written before the status line here.
func writeJSONStatus(w http.ResponseWriter, status int, document any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(document)
}

// writeInternal answers a failure the caller can do nothing about. The cause is
// logged against the request under message, and the body carries detail alone:
// what broke inside this service is not something a client is told.
func writeInternal(ctx context.Context, w http.ResponseWriter, message string, err error, detail string) {
	Logger(ctx).Error(message, "error", err)
	problem.Write(w, http.StatusInternalServerError, problem.TypeInternal, "Internal error", detail)
}

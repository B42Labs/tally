package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
)

// defaultSyncBudget is how long a sync run may take on a router whose options
// name no budget, which is a router built without a config: config.Load always
// hands the binary a positive value. It mirrors the envDefault of
// TALLY_REPORTING_SYNC_BUDGET_S and has to change with it. A budget exists
// because the run has to end while the connection that asked for it is still
// there to be answered: a run that outlives its response is one nobody learns
// the outcome of while it goes on holding the cloud's advisory lock against the
// next attempt.
const defaultSyncBudget = 45 * time.Second

// syncAnswerMargin is how long past the budget the response of a sync may still
// be written. It is the gap the default budget of 45 seconds has always left
// under the server's write timeout of 60, so a deployment on the default keeps
// the deadline it had. It does not cover the worst case of the two writes a run
// ends with: reconciliation bounds each at a completionBudget of 10 seconds, so
// a run can return 20 seconds past its budget, which is the figure a caller's
// own timeout is documented against.
const syncAnswerMargin = 15 * time.Second

// SyncCloud reconciles one cloud and answers with what the run did.
//
// It runs synchronously, so whatever drives the sync schedule learns the
// outcome from the response instead of polling for it, and it is bounded by the
// budget the deployment configured in TALLY_REPORTING_SYNC_BUDGET_S, 45 seconds
// by default, so a run that cannot finish inside the time the response has ends
// instead of outliving it. The response is held open for that budget plus
// syncAnswerMargin, whatever the server's write timeout is. A run that ended on
// the budget is answered 500 like any other failed run; the sync_runs row it
// leaves says the same.
//
// No audit row is written here. sync_runs is the operational record of a run,
// and keeping one run in two places would only let the two drift apart; the
// package documentation of reconciliation states that deviation from the
// rebuild next door.
//
// The request may carry a body naming the instant the run is at, which the run
// then stamps its row and its corrections with instead of reading a clock. Only
// a deployment that set TALLY_REPORTING_SYNC_ALLOW_AT takes one: everywhere
// else a body carrying "at" is refused, and the refusal comes before the run
// starts, so no sync_runs row is left behind by a request nothing reconciled
// for. A request with no body syncs at wall time, and so does one whose body is
// empty or carries no "at".
func (s *server) SyncCloud(w http.ResponseWriter, r *http.Request, cloud string) {
	ctx := r.Context()

	var body SyncRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		problem.Write(w, http.StatusBadRequest, problem.TypeValidation, "Validation failed",
			`the request body must be a JSON object whose optional member "at" is an RFC 3339 instant`)
		return
	}
	if body.At != nil && !s.syncAllowAt {
		problem.Write(w, http.StatusBadRequest, problem.TypeValidation, "Validation failed",
			"this deployment does not take a sync instant; TALLY_REPORTING_SYNC_ALLOW_AT is off")
		return
	}

	// The server arms its write timeout when it reads the request, so a budget
	// past that timeout would end in an answer the connection no longer takes.
	// A writer that has no deadline to move still serves the run: the sync_runs
	// row records it whether or not the answer arrives.
	if err := http.NewResponseController(w).SetWriteDeadline(
		time.Now().Add(s.syncBudget + syncAnswerMargin)); err != nil {
		Logger(ctx).Warn("setting the write deadline of a sync", "error", err, "cloud", cloud)
	}

	runCtx, cancel := context.WithTimeout(ctx, s.syncBudget)
	defer cancel()

	result, err := s.syncer.Sync(runCtx, cloud, body.At)
	switch {
	case errors.Is(err, reconciliation.ErrUnknownCloud):
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound,
			"Not found", "the configuration names no such cloud")
		return
	case errors.Is(err, reconciliation.ErrAlreadyRunning):
		problem.Write(w, http.StatusConflict, problem.TypeConflict,
			"Conflict", "a sync for this cloud is already running")
		return
	case err != nil:
		// What the run did before it failed is what tells a run that got nowhere
		// from one that corrected most of a fleet, and the run id is what an
		// operator reads the rest of the record back by. The caller gets none of
		// it: the errors carry platform detail.
		Logger(ctx).Error("syncing a cloud", "error", err, "cloud", cloud,
			"sync_run_id", result.RunID, "errors", result.Stats.Errors)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal,
			"Internal error", "the sync run failed")
		return
	}

	writeJSON(w, SyncResult{
		SyncRunId: result.RunID,
		Stats: SyncStats{
			Created: result.Stats.Created,
			Updated: result.Stats.Updated,
			Deleted: result.Stats.Deleted,
		},
	})
}

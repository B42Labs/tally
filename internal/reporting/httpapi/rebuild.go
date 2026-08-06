package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/b42labs/tally/internal/reporting/audit"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/projection"
)

// The audit trail a rebuild leaves. The action names the projection table and
// the verb, which is the form every action in the log takes.
const (
	auditObjectResources = "current_resources"
	actionRebuild        = auditObjectResources + ".rebuild"
)

// rebuildBudget is how long a rebuild may run. It stays under the server's write
// timeout, so the run ends while the connection that asked for it is still there
// to be answered: a rebuild that outlives its response is one nobody can
// observe, and it would go on holding resource locks against the ingest path
// while the caller retries.
const rebuildBudget = 45 * time.Second

// RebuildProjection replays the history of every resource the filter selects
// and answers once the replay is done.
//
// It runs synchronously, so the caller learns whether the rebuild worked
// instead of having to poll for it, and it is bounded by rebuildBudget, so a
// run that cannot finish in the time the response has ends instead of outliving
// it. The replay takes one transaction per batch of resources rather than one
// for the whole run, which is what keeps it from holding the fleet's locks at
// once; the audit row is therefore written after the fact rather than alongside
// the change.
func (s *server) RebuildProjection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var body RebuildRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// The validator checked this body against the contract before the
		// request got here, so a failure now is this service disagreeing with
		// itself rather than a caller error.
		writeInternal(ctx, w, "decoding a rebuild request the contract accepted", err,
			"the request could not be read")
		return
	}

	// A member the caller left out filters nothing, which is what the empty
	// string means to projection.Filter.
	var filter projection.Filter
	if body.Cloud != nil {
		filter.Cloud = *body.Cloud
	}
	if body.ResourceType != nil {
		filter.ResourceType = *body.ResourceType
	}

	runCtx, cancel := context.WithTimeout(ctx, rebuildBudget)
	defer cancel()

	rebuilt, err := projection.Rebuild(runCtx, s.store, filter)
	if err != nil {
		// How far the run got is the only thing that tells a rebuild which did
		// nothing from one which did most of the fleet, and a run that stops
		// partway keeps every batch it committed.
		Logger(ctx).Error("rebuilding the projection", "error", err, "rebuilt", rebuilt,
			"cloud", filter.Cloud, "resource_type", filter.ResourceType)
		problem.Write(w, http.StatusInternalServerError, problem.TypeInternal,
			"Internal error", "the projection could not be rebuilt")
		return
	}

	if err := audit.Log(ctx, s.queries, audit.Entry{
		Actor:      audit.ActorInternal,
		Action:     actionRebuild,
		ObjectType: auditObjectResources,
		Details: map[string]any{
			"cloud":         filter.Cloud,
			"resource_type": filter.ResourceType,
			"rebuilt":       rebuilt,
		},
	}); err != nil {
		writeInternal(ctx, w, "recording a rebuild", err,
			"the rebuild could not be recorded")
		return
	}

	writeJSON(w, RebuildResult{Rebuilt: rebuilt})
}

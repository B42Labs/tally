package httpapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

// The plumbing the query routes share. Each of them lives in a file of its own,
// and the pagination protocol, the scope resolution, and the filter mapping are
// one contract across all of them, so they are stated here once rather than in
// whichever endpoint file happened to need them first.

// defaultPageSize is how long a page is when the request names no limit. The
// contract declares the same number, and bounds the parameter, so a limit that
// reaches a handler is either absent or between 1 and 1000.
const defaultPageSize = 100

// badCursorDetail answers every cursor this API cannot use. Which part of it was
// wrong goes to the log rather than to the client: a cursor is opaque, so a
// caller has nothing to fix inside one and can only walk the list again.
const badCursorDetail = "the cursor is not one this API issued"

// errNoPrincipal is what a query route reports when the request context carries
// no principal. Every route the dispatch table puts behind the query guard has
// one, in enforced mode from the token and in disabled mode synthetically, so
// finding none is this service disagreeing with itself.
var errNoPrincipal = errors.New("the request context carries no query principal")

// queryScope resolves what the request's principal may read, and answers the
// request itself when it cannot: a route the dispatch table puts behind the
// query guard has no answer without a scope.
func (s *server) queryScope(w http.ResponseWriter, r *http.Request) (auth.Scope, bool) {
	ctx := r.Context()

	principal, ok := auth.QueryFromContext(ctx)
	if !ok {
		writeInternal(ctx, w, "serving a query route without a principal", errNoPrincipal,
			"the request could not be served")
		return auth.Scope{}, false
	}
	scope, err := auth.ResolveProjectScope(ctx, s.queries, principal)
	if err != nil {
		writeInternal(ctx, w, "resolving the project scope", err,
			"the request could not be served")
		return auth.Scope{}, false
	}
	return scope, true
}

// pageLimit resolves how long a page is: the limit the request names, or the
// default when it names none. The contract bounds the parameter, so a value that
// reaches here is between 1 and 1000.
func pageLimit(limit *int) int {
	if limit == nil {
		return defaultPageSize
	}
	return *limit
}

// refuseCursor answers a cursor this API cannot use, and logs which part of it
// was wrong.
func refuseCursor(ctx context.Context, w http.ResponseWriter, err error) {
	Logger(ctx).Warn("refusing a cursor", "error", err)
	problem.Write(w, http.StatusBadRequest, problem.TypeValidation,
		"Validation failed", badCursorDetail)
}

// trimPage cuts a page read one row longer than it is served back to its length,
// and reports whether that extra row was there. It is what tells "the page is
// full" from "there is more" without a second query counting the rest, and so
// what decides whether a cursor is sent.
func trimPage[Row any](rows []Row, limit int) ([]Row, bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}

// scopeFilter zips the projects a filtered principal reads into the two parallel
// arrays the queries pair positionally, so that clouds[i] belongs to
// projects[i]. Pairing them is what keeps a project id one cloud uses from
// matching the rows another cloud stores under the same id.
//
// An unfiltered principal yields two nil slices, which pgx sends as NULL and the
// queries read as no scope filter at all.
func scopeFilter(scope auth.Scope) (clouds, projects []string) {
	if scope.Unfiltered {
		return nil, nil
	}
	clouds = make([]string, len(scope.Refs))
	projects = make([]string, len(scope.Refs))
	for i, ref := range scope.Refs {
		clouds[i] = ref.Cloud
		projects[i] = ref.ExternalID
	}
	return clouds, projects
}

// reachesProject reports whether a filtered scope holds the project a request
// asked for by name. The cloud is left out of the comparison: the pair filter of
// the query narrows the answer to the cloud the token holds the project in
// anyway, so a matching external id is enough to let the request through.
func reachesProject(scope auth.Scope, externalID string) bool {
	return slices.ContainsFunc(scope.Refs, func(ref auth.ProjectRef) bool {
		return ref.ExternalID == externalID
	})
}

// reachesPair reports whether a filtered scope holds the (cloud, project id)
// pair a projection row names. Both halves are compared here, unlike the lists,
// where the query's own pair filter has already narrowed the rows.
func reachesPair(scope auth.Scope, cloud, projectID string) bool {
	return slices.ContainsFunc(scope.Refs, func(ref auth.ProjectRef) bool {
		return ref.Cloud == cloud && ref.ExternalID == projectID
	})
}

// filterText maps one optional string filter onto its query parameter. A filter
// the request left out becomes NULL, which the query reads as every value.
func filterText(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *value, Valid: true}
}

// filterInstant maps one bound of a time window onto its query parameter.
func filterInstant(value *time.Time) pgtype.Timestamptz {
	if value == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *value, Valid: true}
}

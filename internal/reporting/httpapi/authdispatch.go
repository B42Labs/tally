package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

// resourceTypeRoute is the chi pattern the two per-type operations share. It is
// named once because the dispatch table keys two methods on it, and it has to
// be the pattern the generated wiring registers, letter for letter.
const resourceTypeRoute = "/api/v1/resource-types/{platform}/{resource_type}"

// resourceRoute is the chi pattern the per-resource reads are mounted under. The
// two operations append their own segment to it, and it has to be the pattern
// the generated wiring registers, letter for letter.
const resourceRoute = "/api/v1/resources/{cloud}/{resource_type}/{resource_id}"

// projectRoute is the chi pattern the two per-project operations share. It is
// named once because the dispatch table keys two methods on it, and it has to
// be the pattern the generated wiring registers, letter for letter.
const projectRoute = "/api/v1/projects/{id}"

// projectSummaryRoute is the chi pattern the per-project summary is mounted
// under. It is built from projectRoute because it hangs off it, and it has to be
// the pattern the generated wiring registers, letter for letter.
const projectSummaryRoute = projectRoute + "/summary"

// The chi patterns the relation operations of one project are mounted under.
// They are built from projectRoute because they hang off it, and each of them
// has to be the pattern the generated wiring registers, letter for letter.
const (
	projectRelationsRoute = projectRoute + "/relations"
	projectRelationRoute  = projectRelationsRoute + "/{relation_id}"
	projectRelatedRoute   = projectRoute + "/related"
)

// newAuthDispatch builds the middleware that puts each route behind the guard
// its class needs.
//
// It is handed to the generated wrapper, so it runs after chi has matched a
// route and the matched pattern is what the rule is looked up by. Keying on the
// pattern rather than on the request path is what keeps the table finite and
// exact: no path parameter can shape a request into another rule.
//
// A method and pattern the table does not name is refused with a 500 and never
// reaches its handler. That is the fail-closed property of this seam: adding an
// operation to the contract without deciding who may call it takes the route
// offline instead of serving it to everyone.
func newAuthDispatch(opts Options) MiddlewareFunc {
	// A nil guard is a route that is served without a credential.
	guards := map[string]MiddlewareFunc{
		http.MethodGet + " /healthz": nil,
		http.MethodGet + " /readyz":  nil,
		http.MethodGet + " /metrics": nil,

		http.MethodGet + " /api/v1/events":  auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),
		http.MethodPost + " /api/v1/events": auth.Ingest(opts.Queries, opts.AuthMode, Logger),

		http.MethodGet + " /api/v1/resources":               auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),
		http.MethodGet + " " + resourceRoute + "/events":    auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),
		http.MethodGet + " " + resourceRoute + "/lifecycle": auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),

		// The project registry is the organizational structure a project scope is
		// built out of, so no project-scoped view of it exists: reading it takes
		// read_all, and changing it is an operator's job.
		http.MethodGet + " /api/v1/projects":  auth.Query(opts.Authenticator, auth.RoleReadAll, opts.AuthMode, Logger),
		http.MethodGet + " " + projectRoute:   auth.Query(opts.Authenticator, auth.RoleReadAll, opts.AuthMode, Logger),
		http.MethodPost + " /api/v1/projects": auth.Query(opts.Authenticator, auth.RoleAdmin, opts.AuthMode, Logger),
		http.MethodPatch + " " + projectRoute: auth.Query(opts.Authenticator, auth.RoleAdmin, opts.AuthMode, Logger),

		// The relations of a project are the same structure seen from one row of
		// it, so they are guarded the way the registry itself is.
		http.MethodGet + " " + projectRelationsRoute:   auth.Query(opts.Authenticator, auth.RoleReadAll, opts.AuthMode, Logger),
		http.MethodGet + " " + projectRelatedRoute:     auth.Query(opts.Authenticator, auth.RoleReadAll, opts.AuthMode, Logger),
		http.MethodPost + " " + projectRelationsRoute:  auth.Query(opts.Authenticator, auth.RoleAdmin, opts.AuthMode, Logger),
		http.MethodPatch + " " + projectRelationRoute:  auth.Query(opts.Authenticator, auth.RoleAdmin, opts.AuthMode, Logger),
		http.MethodDelete + " " + projectRelationRoute: auth.Query(opts.Authenticator, auth.RoleAdmin, opts.AuthMode, Logger),

		// The statistics are the query endpoints seen in aggregate, so they are
		// guarded the way those are. The summary hangs off a registry route whose
		// read takes read_all, and it is guarded lower on purpose: it reports what
		// a project ran rather than what the registry holds, so a project token
		// reaches its own projects here, and the handler answers a foreign one the
		// 404 an unknown id gets. Its answer names the project rather than
		// carrying the registry row, which is what keeps the rule above intact.
		http.MethodGet + " /api/v1/stats/resources": auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),
		http.MethodGet + " /api/v1/stats/events":    auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),
		http.MethodGet + " " + projectSummaryRoute:  auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),

		// The dead-letter list is admin-only like the two writes above and the
		// registration below: a refused item carries whatever a collector sent,
		// which no project scope narrows.
		http.MethodGet + " /api/v1/rejected-events": auth.Query(opts.Authenticator, auth.RoleAdmin, opts.AuthMode, Logger),

		http.MethodGet + " /api/v1/resource-types": auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),
		http.MethodGet + " " + resourceTypeRoute:   auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),
		http.MethodPut + " " + resourceTypeRoute:   auth.Query(opts.Authenticator, auth.RoleAdmin, opts.AuthMode, Logger),

		http.MethodPost + " /internal/projection/rebuild": auth.Internal(opts.InternalToken, opts.AuthMode),
		http.MethodPost + " /internal/sync/{cloud}":       auth.Internal(opts.InternalToken, opts.AuthMode),
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route := r.Method + " " + chi.RouteContext(r.Context()).RoutePattern()

			guard, known := guards[route]
			switch {
			case !known:
				Logger(r.Context()).Error("no authentication rule covers this route", "route", route)
				problem.Write(w, http.StatusInternalServerError, problem.TypeInternal,
					"Internal error", "the request could not be served")
			case guard == nil:
				next.ServeHTTP(w, r)
			default:
				guard(next).ServeHTTP(w, r)
			}
		})
	}
}

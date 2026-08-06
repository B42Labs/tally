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

		http.MethodPost + " /api/v1/events": auth.Ingest(opts.Queries, opts.AuthMode, Logger),

		http.MethodGet + " /api/v1/resource-types": auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),
		http.MethodGet + " " + resourceTypeRoute:   auth.Query(opts.Authenticator, auth.RoleProject, opts.AuthMode, Logger),
		http.MethodPut + " " + resourceTypeRoute:   auth.Query(opts.Authenticator, auth.RoleAdmin, opts.AuthMode, Logger),

		http.MethodPost + " /internal/projection/rebuild": auth.Internal(opts.InternalToken, opts.AuthMode),
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

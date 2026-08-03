// Package httpapi is the HTTP surface of the Reporting API: the router, the
// middleware stack every request passes through, and the handlers behind the
// generated routes.
//
// The contract in api/reporting/openapi.yaml drives both halves of this
// package. openapi.gen.go carries the routes generated from it, and the same
// document validates every incoming request at runtime, so a request that the
// contract does not describe never reaches a handler.
//
// Errors leave as RFC 9457 problems (see the problem subpackage), which is what
// makes a validation failure, a panic, and an unreachable database look alike
// to a client.
//
// The normative specification is roadmap/00-conventions.md section 7.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/go-chi/chi/v5"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

// Options configures the router.
type Options struct {
	// Logger is the base logger every request logger derives from. It defaults
	// to the process-wide default logger.
	Logger *slog.Logger
	// DB is what the probes check. It is required.
	DB DB
	// UnhealthyThreshold is how long the database may stay unreachable before
	// liveness fails and Kubernetes restarts the pod.
	UnhealthyThreshold time.Duration
	// Now is the clock the liveness threshold is measured on. It defaults to
	// time.Now and exists so tests can control the threshold.
	Now func() time.Time
}

// NewRouter assembles the API: the middleware stack, the request validator, and
// the generated routes.
//
// It fails when the embedded contract does not load or does not validate. That
// keeps a broken contract from starting a server whose request validation is
// weaker than the document promises.
func NewRouter(opts Options) (http.Handler, error) {
	spec, err := loadSpec(GetSpec)
	if err != nil {
		return nil, err
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	r := chi.NewRouter()

	// The validator rejects unknown paths and methods before chi routes at all.
	// These two handlers cover what reaches chi anyway, so that both layers
	// answer a client the same way.
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		problem.Write(w, http.StatusNotFound, problem.TypeNotFound,
			"Not found", "this path is not part of the API")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		problem.Write(w, http.StatusMethodNotAllowed, problem.TypeMethodNotAllowed,
			"Method not allowed", "this path does not accept this method")
	})

	// Order matters: the id has to exist before anything logs, and the recoverer
	// has to sit outside the validator and the handlers it protects.
	r.Use(newRequestID(logger))
	r.Use(requestLogging)
	r.Use(recoverer)
	r.Use(newValidator(spec))

	srv := &server{
		db:     opts.DB,
		health: newHealthTracker(now, opts.UnhealthyThreshold),
	}
	return HandlerWithOptions(srv, ChiServerOptions{BaseRouter: r}), nil
}

// newValidator builds the middleware that checks every request against the
// contract.
//
// Its authentication function fails closed. The contract describes only the
// unauthenticated health probes today, and the middlewares that guard the
// authenticated routes arrive with those routes (see internal/reporting/auth).
// Until they are mounted here, an operation that declares a security scheme is
// refused rather than served: adding one to the contract must not put a route
// online that nothing checks a credential for.
func newValidator(spec *openapi3.T) func(http.Handler) http.Handler {
	return nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		Options: openapi3filter.Options{
			AuthenticationFunc: func(context.Context, *openapi3filter.AuthenticationInput) error {
				return errUnenforcedSecurity
			},
		},
		ErrorHandlerWithOpts: func(_ context.Context, err error, w http.ResponseWriter, r *http.Request, opts nethttpmiddleware.ErrorHandlerOpts) {
			status, typ, title := validationProblem(err, opts.StatusCode)
			if status >= http.StatusInternalServerError {
				// The validator's own failures carry spec internals: schema
				// paths, ref names, decoder detail. The operator gets those
				// through the log, the caller gets none of them.
				Logger(r.Context()).Error("validating the request", "error", err)
				problem.Write(w, status, typ, title, "the request could not be checked")
				return
			}
			if status == http.StatusUnauthorized {
				// RFC 9110 asks a 401 to name the scheme the caller should have
				// used, and the API accepts exactly one.
				w.Header().Set("WWW-Authenticate", "Bearer")
			}
			problem.Write(w, status, typ, title, validationDetail(err), fieldErrors(err)...)
		},
	})
}

// errUnenforcedSecurity is what the validator answers an operation that
// declares a security scheme with, as long as no middleware enforces one.
var errUnenforcedSecurity = errors.New("the security schemes of this contract are not enforced yet")

// validationDetail is the sentence a rejected request is answered with. It says
// what happened without echoing the submitted values back, which the field
// errors carry instead.
func validationDetail(err error) string {
	if errors.Is(err, errUnenforcedSecurity) {
		return "this endpoint requires a credential this build does not check"
	}
	return "the request does not match the API contract"
}

// fieldErrors turns what the validator rejected into the per-field entries the
// contract's errors array carries. A failure it cannot attribute to one place
// yields none, which leaves the client with the detail alone.
func fieldErrors(err error) []problem.FieldError {
	var reqErr *openapi3filter.RequestError
	if !errors.As(err, &reqErr) {
		return nil
	}

	switch {
	case reqErr.Parameter != nil:
		return []problem.FieldError{{
			Loc: reqErr.Parameter.In + "." + reqErr.Parameter.Name,
			Msg: reason(reqErr),
		}}
	case reqErr.RequestBody != nil:
		return []problem.FieldError{bodyFieldError(reqErr)}
	}
	return nil
}

// bodyFieldError points at the member of the request body that was rejected, as
// precisely as the validator names it.
func bodyFieldError(err *openapi3filter.RequestError) problem.FieldError {
	var schemaErr *openapi3.SchemaError
	if !errors.As(err.Err, &schemaErr) {
		return problem.FieldError{Loc: "body", Msg: reason(err)}
	}

	loc := "body"
	if pointer := schemaErr.JSONPointer(); len(pointer) > 0 {
		loc += "." + strings.Join(pointer, ".")
	}
	return problem.FieldError{Loc: loc, Msg: schemaErr.Reason}
}

// reason is the short half of a validator error: what is wrong, without the
// library's wrapping and without the value that was sent.
func reason(err *openapi3filter.RequestError) string {
	if err.Reason != "" {
		return err.Reason
	}
	return "does not match the contract"
}

// validationProblem maps a rejected request to the problem this API answers
// with. It takes the status the validator suggests and the error itself,
// because the suggestion alone is not enough: the middleware reports every
// unmatched route as 404, so a request that hits a known path with the wrong
// method is only recognizable from the error.
func validationProblem(err error, suggested int) (status int, typ, title string) {
	switch {
	case errors.Is(err, routers.ErrMethodNotAllowed):
		return http.StatusMethodNotAllowed, problem.TypeMethodNotAllowed, "Method not allowed"
	case suggested == http.StatusUnauthorized:
		return http.StatusUnauthorized, problem.TypeUnauthorized, "Unauthorized"
	case suggested == http.StatusNotFound:
		return http.StatusNotFound, problem.TypeNotFound, "Not found"
	case suggested == http.StatusInternalServerError:
		return http.StatusInternalServerError, problem.TypeInternal, "Internal error"
	default:
		// Everything the validator can still report is the request failing the
		// contract, whether it names 400 or 422.
		return http.StatusBadRequest, problem.TypeValidation, "Validation failed"
	}
}

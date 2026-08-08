package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

// TestAuthDispatch pins the seam the generated routes are mounted behind: which
// guard a request meets is decided by the pattern chi matched, and a pattern the
// table does not name is refused rather than served.
func TestAuthDispatch(t *testing.T) {
	t.Run("refuses a route no rule covers and never reaches its handler", func(t *testing.T) {
		// An operation that reached the contract without an entry in the dispatch
		// table looks like this: chi matches it, and nothing says who may call it.
		ran := false
		handler := dispatchOn(t, http.MethodGet, "/widgets", &ran)

		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/widgets", nil))

		assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
		if ran {
			t.Error("the wrapped handler ran for a route no rule covers")
		}
	})

	t.Run("serves the probes without demanding a credential", func(t *testing.T) {
		for _, pattern := range []string{"/healthz", "/readyz"} {
			t.Run(pattern, func(t *testing.T) {
				ran := false
				handler := dispatchOn(t, http.MethodGet, pattern, &ran)

				rec := serve(handler, httptest.NewRequest(http.MethodGet, pattern, nil))

				if got := rec.Code; got != http.StatusNoContent {
					t.Errorf("status = %d, want %d (body %q)", got, http.StatusNoContent, rec.Body)
				}
				if !ran {
					t.Error("the probe handler did not run, want it served without a credential")
				}
			})
		}
	})

	t.Run("puts the events list behind a credential of its own", func(t *testing.T) {
		// The list shares its pattern with the ingest endpoint, so what says the
		// table keyed the two methods apart is a GET meeting a guard rather than
		// being refused as a route no rule covers.
		ran := false
		handler := dispatchOn(t, http.MethodGet, "/api/v1/events", &ran)

		rec := serve(handler, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

		assertChallenged(t, rec)
		if ran {
			t.Error("the list handler ran for a request carrying no credential")
		}
	})

	t.Run("puts the resource reads behind a credential", func(t *testing.T) {
		// The two per-resource routes are keyed on the pattern rather than on a
		// path, so what says the table names them is a request whose path
		// parameters carry values no rule could match.
		for _, tc := range []struct{ pattern, path string }{
			{pattern: "/api/v1/resources", path: "/api/v1/resources"},
			{
				pattern: resourceRoute + "/events",
				path:    "/api/v1/resources/os-prod-eu1/instance/i-1/events",
			},
			{
				pattern: resourceRoute + "/lifecycle",
				path:    "/api/v1/resources/os-prod-eu1/instance/i-1/lifecycle",
			},
		} {
			t.Run(tc.pattern, func(t *testing.T) {
				ran := false
				handler := dispatchOn(t, http.MethodGet, tc.pattern, &ran)

				rec := serve(handler, httptest.NewRequest(http.MethodGet, tc.path, nil))

				assertChallenged(t, rec)
				if ran {
					t.Error("the handler ran for a request carrying no credential")
				}
			})
		}
	})
}

// dispatchOn mounts the dispatch middleware on one chi route the way the
// generated wrapper does, inside the route rather than in front of the router,
// so that the matched pattern is what the rule is looked up by. The wrapped
// handler sets ran and answers 204, which tells a served request from a refused
// one.
//
// The request-id middleware is in front of it because the refusal path logs
// against the request logger, which keeps that line out of the test output.
func dispatchOn(t *testing.T, method, pattern string, ran *bool) http.Handler {
	t.Helper()

	guarded := newAuthDispatch(Options{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*ran = true
		w.WriteHeader(http.StatusNoContent)
	}))

	r := chi.NewRouter()
	r.Use(newRequestID(slog.New(slog.NewJSONHandler(io.Discard, nil))))
	r.Method(method, pattern, guarded)
	return r
}

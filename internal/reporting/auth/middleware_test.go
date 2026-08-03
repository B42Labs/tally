package auth_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
)

// internalToken is the shared secret the Internal middleware is built with in
// these tests. It never leaves the test binary.
const internalToken = "internal-token"

func TestMiddlewaresRejectARequestWithoutAUsableBearerToken(t *testing.T) {
	// The three ways a request arrives without a token this API can look at.
	// None of them may reach a lookup, which is why the queries are nil and the
	// authenticator fails the test when it is called.
	headers := map[string]map[string]string{
		"no Authorization header": nil,
		"another scheme":          {"Authorization": "Basic xyz"},
		"an empty bearer token":   {"Authorization": "Bearer "},
	}

	for name, build := range map[string]func(*testing.T) func(http.Handler) http.Handler{
		"Ingest": func(*testing.T) func(http.Handler) http.Handler {
			return auth.Ingest(nil, auth.ModeEnforced, nil)
		},
		"Query": func(t *testing.T) func(http.Handler) http.Handler {
			return auth.Query(neverAuthenticates(t), auth.RoleProject, auth.ModeEnforced, nil)
		},
		"Internal": func(*testing.T) func(http.Handler) http.Handler {
			return auth.Internal(internalToken, auth.ModeEnforced)
		},
	} {
		t.Run(name, func(t *testing.T) {
			for reason, header := range headers {
				t.Run("rejects a request with "+reason, func(t *testing.T) {
					ran := false

					rec := serve(build(t)(records(&ran)), request(t, http.MethodGet, "/api/v1/events", header))

					assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthorized)
					if got, want := rec.Header().Get("WWW-Authenticate"), "Bearer"; got != want {
						t.Errorf("WWW-Authenticate = %q, want %q", got, want)
					}
					if ran {
						t.Error("the wrapped handler ran for a rejected request")
					}
				})
			}
		})
	}
}

func TestInternal(t *testing.T) {
	t.Run("passes a request carrying the internal token", func(t *testing.T) {
		ran := false

		rec := serve(auth.Internal(internalToken, auth.ModeEnforced)(records(&ran)),
			request(t, http.MethodPost, "/internal/sync-runs", bearer(internalToken)))

		if got := rec.Code; got != http.StatusNoContent {
			t.Errorf("status = %d, want %d (body %q)", got, http.StatusNoContent, rec.Body)
		}
		if !ran {
			t.Error("the wrapped handler did not run")
		}
	})

	t.Run("matches the scheme without regard to case", func(t *testing.T) {
		ran := false

		rec := serve(auth.Internal(internalToken, auth.ModeEnforced)(records(&ran)),
			request(t, http.MethodPost, "/internal/sync-runs", map[string]string{
				"Authorization": "bearer " + internalToken,
			}))

		if got := rec.Code; got != http.StatusNoContent {
			t.Errorf("status = %d, want %d (body %q)", got, http.StatusNoContent, rec.Body)
		}
		if !ran {
			t.Error("the wrapped handler did not run")
		}
	})

	t.Run("rejects a request carrying another token", func(t *testing.T) {
		ran := false

		rec := serve(auth.Internal(internalToken, auth.ModeEnforced)(records(&ran)),
			request(t, http.MethodPost, "/internal/sync-runs", bearer("wrong")))

		assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthorized)
		if ran {
			t.Error("the wrapped handler ran for a rejected request")
		}
	})

	t.Run("rejects a token that only starts like the internal one", func(t *testing.T) {
		ran := false

		rec := serve(auth.Internal(internalToken, auth.ModeEnforced)(records(&ran)),
			request(t, http.MethodPost, "/internal/sync-runs", bearer(internalToken[:len(internalToken)-1])))

		assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthorized)
		if ran {
			t.Error("the wrapped handler ran for a rejected request")
		}
	})
}

func TestMiddlewaresWithAuthenticationDisabled(t *testing.T) {
	t.Run("Ingest serves a request without a credential", func(t *testing.T) {
		ran := false
		handler := auth.Ingest(nil, auth.ModeDisabled, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ran = true
			if p, ok := auth.IngestFromContext(r.Context()); ok {
				t.Errorf("IngestFromContext() = %+v, true; want no principal", p)
			}
			w.WriteHeader(http.StatusNoContent)
		}))

		rec := serve(handler, request(t, http.MethodPost, "/api/v1/events", nil))

		if got := rec.Code; got != http.StatusNoContent {
			t.Errorf("status = %d, want %d (body %q)", got, http.StatusNoContent, rec.Body)
		}
		if !ran {
			t.Error("the wrapped handler did not run")
		}
	})

	t.Run("Query serves a request without a credential as an admin", func(t *testing.T) {
		ran := false
		handler := auth.Query(neverAuthenticates(t), auth.RoleAdmin, auth.ModeDisabled, nil)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ran = true
				principal, ok := auth.QueryFromContext(r.Context())
				if !ok {
					t.Fatal("QueryFromContext() found no principal, want the synthetic one")
				}
				if got := principal.Role; got != auth.RoleAdmin {
					t.Errorf("principal role = %q, want %q", got, auth.RoleAdmin)
				}
				w.WriteHeader(http.StatusNoContent)
			}))

		rec := serve(handler, request(t, http.MethodGet, "/api/v1/events", nil))

		if got := rec.Code; got != http.StatusNoContent {
			t.Errorf("status = %d, want %d (body %q)", got, http.StatusNoContent, rec.Body)
		}
		if !ran {
			t.Error("the wrapped handler did not run")
		}
	})

	t.Run("Internal serves a request without a credential", func(t *testing.T) {
		ran := false

		rec := serve(auth.Internal(internalToken, auth.ModeDisabled)(records(&ran)),
			request(t, http.MethodPost, "/internal/sync-runs", nil))

		if got := rec.Code; got != http.StatusNoContent {
			t.Errorf("status = %d, want %d (body %q)", got, http.StatusNoContent, rec.Body)
		}
		if !ran {
			t.Error("the wrapped handler did not run")
		}
	})
}

// authenticatorFunc adapts a function to auth.Authenticator.
type authenticatorFunc func(ctx context.Context, token string) (auth.QueryPrincipal, error)

// Authenticate calls f.
func (f authenticatorFunc) Authenticate(ctx context.Context, token string) (auth.QueryPrincipal, error) {
	return f(ctx, token)
}

// neverAuthenticates is an Authenticator that fails the test when it is
// reached, which is how a test states that the middleware has to answer before
// looking a token up.
func neverAuthenticates(t *testing.T) auth.Authenticator {
	t.Helper()

	return authenticatorFunc(func(context.Context, string) (auth.QueryPrincipal, error) {
		t.Error("the authenticator was called, want the request answered before the lookup")
		return auth.QueryPrincipal{}, nil
	})
}

// records is a handler that answers 204 and reports through ran that it was
// reached, which is what tells a passed request from a rejected one.
func records(ran *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*ran = true
		w.WriteHeader(http.StatusNoContent)
	})
}

// bearer is the Authorization header presenting token.
func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// request builds a request with the given headers set.
func request(t *testing.T, method, target string, headers map[string]string) *http.Request {
	t.Helper()

	r := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		r.Header.Set(name, value)
	}
	return r
}

// serve runs one request through handler and returns what it wrote.
func serve(handler http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, r)
	return rec
}

// assertProblem checks that the response is the problem document the API
// promises for this failure.
func assertProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, typ string) {
	t.Helper()

	if got := rec.Code; got != status {
		t.Errorf("status = %d, want %d (body %q)", got, status, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type = %q, want %q", got, problem.ContentType)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding the body %q: %v", rec.Body.String(), err)
	}
	if got := body["type"]; got != typ {
		t.Errorf("body type = %v, want %v", got, typ)
	}
}

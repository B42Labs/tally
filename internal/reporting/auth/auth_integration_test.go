package auth_test

import (
	"bytes"
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/auth"
	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
	"github.com/b42labs/tally/internal/reporting/store/storetest"
)

// TestAuthAgainstARealDatabase drives the middlewares against the credential
// tables themselves. Revocation and the role lattice live in SQL as much as in
// Go, so a stubbed lookup would prove nothing about either.
func TestAuthAgainstARealDatabase(t *testing.T) {
	db := storetest.NewDB(t)
	q := sqlcgen.New(db.Store.Pool())

	t.Run("Ingest hands the credential to the handler", func(t *testing.T) {
		const (
			platform = "openstack"
			cloud    = "os-ingest"
		)
		id, token := seedIngestCredential(t, q, platform, cloud)

		ran := false
		handler := auth.Ingest(q, auth.ModeEnforced, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ran = true
			principal, ok := auth.IngestFromContext(r.Context())
			if !ok {
				t.Fatal("IngestFromContext() found no principal, want the seeded credential")
			}
			if principal.CredentialID != id {
				t.Errorf("principal credential id = %s, want %s", principal.CredentialID, id)
			}
			if principal.Platform != platform || principal.Cloud != cloud {
				t.Errorf("principal platform, cloud = %q, %q; want %q, %q",
					principal.Platform, principal.Cloud, platform, cloud)
			}
			w.WriteHeader(http.StatusNoContent)
		}))

		rec := serve(handler, request(t, http.MethodPost, "/api/v1/events", bearer(token)))

		if got := rec.Code; got != http.StatusNoContent {
			t.Errorf("status = %d, want %d (body %q)", got, http.StatusNoContent, rec.Body)
		}
		if !ran {
			t.Error("the wrapped handler did not run")
		}
	})

	t.Run("Ingest rejects a token no credential was issued for", func(t *testing.T) {
		token, err := auth.GenerateIngestToken()
		if err != nil {
			t.Fatalf("generating the token: %v", err)
		}

		ran := false

		rec := serve(auth.Ingest(q, auth.ModeEnforced, nil)(records(&ran)),
			request(t, http.MethodPost, "/api/v1/events", bearer(token)))

		assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthorized)
		if ran {
			t.Error("the wrapped handler ran for a rejected request")
		}
	})

	t.Run("Ingest rejects a credential the moment it is revoked", func(t *testing.T) {
		id, token := seedIngestCredential(t, q, "openstack", "os-revoked")
		handler := auth.Ingest(q, auth.ModeEnforced, nil)(records(new(bool)))

		if got := serve(handler, request(t, http.MethodPost, "/api/v1/events", bearer(token))).Code; got != http.StatusNoContent {
			t.Fatalf("status before the revocation = %d, want %d", got, http.StatusNoContent)
		}

		if err := q.RevokeIngestCredential(t.Context(), id); err != nil {
			t.Fatalf("RevokeIngestCredential() error = %v, want nil", err)
		}

		rec := serve(handler, request(t, http.MethodPost, "/api/v1/events", bearer(token)))

		assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthorized)
	})

	t.Run("Query rejects an api token the moment it is revoked", func(t *testing.T) {
		id, token := seedAPIToken(t, q, auth.RoleAdmin, nil)
		handler := auth.Query(auth.NewStaticTokenAuthenticator(q), auth.RoleAdmin, auth.ModeEnforced, nil)(records(new(bool)))

		if got := serve(handler, request(t, http.MethodGet, "/api/v1/events", bearer(token))).Code; got != http.StatusNoContent {
			t.Fatalf("status before the revocation = %d, want %d", got, http.StatusNoContent)
		}

		if err := q.RevokeAPIToken(t.Context(), id); err != nil {
			t.Fatalf("RevokeAPIToken() error = %v, want nil", err)
		}

		rec := serve(handler, request(t, http.MethodGet, "/api/v1/events", bearer(token)))

		assertProblem(t, rec, http.StatusUnauthorized, problem.TypeUnauthorized)
	})

	t.Run("Query hands the api token to the handler", func(t *testing.T) {
		scope := []uuid.UUID{uuid.New(), uuid.New()}
		id, token := seedAPIToken(t, q, auth.RoleProject, scope)

		handler := auth.Query(auth.NewStaticTokenAuthenticator(q), auth.RoleProject, auth.ModeEnforced, nil)(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				principal, ok := auth.QueryFromContext(r.Context())
				if !ok {
					t.Fatal("QueryFromContext() found no principal, want the seeded token")
				}
				if principal.TokenID != id {
					t.Errorf("principal token id = %s, want %s", principal.TokenID, id)
				}
				if principal.Role != auth.RoleProject {
					t.Errorf("principal role = %q, want %q", principal.Role, auth.RoleProject)
				}
				if !slices.Equal(principal.ProjectIDs, scope) {
					t.Errorf("principal project ids = %v, want %v", principal.ProjectIDs, scope)
				}
				w.WriteHeader(http.StatusNoContent)
			}))

		rec := serve(handler, request(t, http.MethodGet, "/api/v1/events", bearer(token)))

		if got := rec.Code; got != http.StatusNoContent {
			t.Errorf("status = %d, want %d (body %q)", got, http.StatusNoContent, rec.Body)
		}
	})

	t.Run("Query enforces the role lattice", func(t *testing.T) {
		authenticator := auth.NewStaticTokenAuthenticator(q)
		_, adminToken := seedAPIToken(t, q, auth.RoleAdmin, nil)
		_, readAllToken := seedAPIToken(t, q, auth.RoleReadAll, nil)
		_, projectToken := seedAPIToken(t, q, auth.RoleProject, []uuid.UUID{uuid.New()})

		for name, tc := range map[string]struct {
			token    string
			required auth.Role
			want     int
		}{
			"an admin token passes a read_all route":      {adminToken, auth.RoleReadAll, http.StatusNoContent},
			"an admin token passes an admin route":        {adminToken, auth.RoleAdmin, http.StatusNoContent},
			"a read_all token is refused an admin route":  {readAllToken, auth.RoleAdmin, http.StatusForbidden},
			"a read_all token passes a project route":     {readAllToken, auth.RoleProject, http.StatusNoContent},
			"a project token is refused a read_all route": {projectToken, auth.RoleReadAll, http.StatusForbidden},
			"a project token is refused an admin route":   {projectToken, auth.RoleAdmin, http.StatusForbidden},
			"a project token passes a project route":      {projectToken, auth.RoleProject, http.StatusNoContent},
		} {
			t.Run(name, func(t *testing.T) {
				ran := false

				rec := serve(auth.Query(authenticator, tc.required, auth.ModeEnforced, nil)(records(&ran)),
					request(t, http.MethodGet, "/api/v1/events", bearer(tc.token)))

				if tc.want == http.StatusForbidden {
					assertProblem(t, rec, http.StatusForbidden, problem.TypeForbidden)
					if ran {
						t.Error("the wrapped handler ran for a refused request")
					}
					return
				}
				if got := rec.Code; got != tc.want {
					t.Errorf("status = %d, want %d (body %q)", got, tc.want, rec.Body)
				}
				if !ran {
					t.Error("the wrapped handler did not run")
				}
			})
		}
	})

	t.Run("a database that cannot be reached is an internal error, not a bad credential", func(t *testing.T) {
		broken := closedQueries(t, db.URL)
		token, err := auth.GenerateIngestToken()
		if err != nil {
			t.Fatalf("generating the token: %v", err)
		}

		t.Run("Ingest reports it and logs it", func(t *testing.T) {
			logs := &bytes.Buffer{}
			logger := slog.New(slog.NewJSONHandler(logs, nil))
			ran := false

			rec := serve(auth.Ingest(broken, auth.ModeEnforced, func(context.Context) *slog.Logger { return logger })(records(&ran)),
				request(t, http.MethodPost, "/api/v1/events", bearer(token)))

			assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
			if ran {
				t.Error("the wrapped handler ran for a failed lookup")
			}
			if !strings.Contains(logs.String(), "looking up the ingest credential") {
				t.Errorf("the captured log does not report the failed lookup:\n%s", logs)
			}
			if strings.Contains(logs.String(), token) {
				t.Error("the captured log contains the presented token")
			}
		})

		t.Run("Query reports it through the default logger when it was given none", func(t *testing.T) {
			ran := false

			rec := serve(auth.Query(auth.NewStaticTokenAuthenticator(broken), auth.RoleProject, auth.ModeEnforced, nil)(records(&ran)),
				request(t, http.MethodGet, "/api/v1/events", bearer(token)))

			assertProblem(t, rec, http.StatusInternalServerError, problem.TypeInternal)
			if ran {
				t.Error("the wrapped handler ran for a failed lookup")
			}
		})
	})

	t.Run("ResolveProjectScope", func(t *testing.T) {
		const cloud = "os-scope"
		first := seedProject(t, db, cloud, "p-1")
		second := seedProject(t, db, cloud, "p-2")

		t.Run("maps the registry ids that name a project and drops the ones that do not", func(t *testing.T) {
			scope, err := auth.ResolveProjectScope(t.Context(), q, auth.QueryPrincipal{
				Role:       auth.RoleProject,
				ProjectIDs: []uuid.UUID{first, uuid.New(), second},
			})
			if err != nil {
				t.Fatalf("ResolveProjectScope() error = %v, want nil", err)
			}

			if scope.Unfiltered {
				t.Error("scope is unfiltered, want a project token filtered by its projects")
			}
			slices.SortFunc(scope.Refs, func(a, b auth.ProjectRef) int { return cmp.Compare(a.ExternalID, b.ExternalID) })
			want := []auth.ProjectRef{{Cloud: cloud, ExternalID: "p-1"}, {Cloud: cloud, ExternalID: "p-2"}}
			if !slices.Equal(scope.Refs, want) {
				t.Errorf("ResolveProjectScope() refs = %v, want %v", scope.Refs, want)
			}
		})

		t.Run("a project token whose projects are all gone stays filtered", func(t *testing.T) {
			// The dangerous case: zero refs must not read as "no project filter",
			// or deleting a project would widen the token to every project.
			scope, err := auth.ResolveProjectScope(t.Context(), q, auth.QueryPrincipal{
				Role:       auth.RoleProject,
				ProjectIDs: []uuid.UUID{uuid.New()},
			})
			if err != nil {
				t.Fatalf("ResolveProjectScope() error = %v, want nil", err)
			}
			if scope.Unfiltered {
				t.Error("scope is unfiltered, want a project token to stay filtered")
			}
			if len(scope.Refs) != 0 {
				t.Errorf("ResolveProjectScope() refs = %v, want none", scope.Refs)
			}
		})

		t.Run("a principal that is not project-scoped is unfiltered", func(t *testing.T) {
			for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleReadAll} {
				t.Run(string(role), func(t *testing.T) {
					scope, err := auth.ResolveProjectScope(t.Context(), q, auth.QueryPrincipal{Role: role})
					if err != nil {
						t.Fatalf("ResolveProjectScope() error = %v, want nil", err)
					}
					if !scope.Unfiltered {
						t.Error("scope is filtered, want an unfiltered one")
					}
					if len(scope.Refs) != 0 {
						t.Errorf("ResolveProjectScope() refs = %v, want none", scope.Refs)
					}
				})
			}
		})

		t.Run("names itself when the query fails", func(t *testing.T) {
			_, err := auth.ResolveProjectScope(t.Context(), closedQueries(t, db.URL), auth.QueryPrincipal{
				Role:       auth.RoleProject,
				ProjectIDs: []uuid.UUID{first},
			})
			if err == nil {
				t.Fatal("ResolveProjectScope() error = nil, want the query failure")
			}
			if !strings.HasPrefix(err.Error(), "resolving project scope: ") {
				t.Errorf("ResolveProjectScope() error = %q, want it prefixed %q", err, "resolving project scope: ")
			}
		})
	})
}

// seedIngestCredential issues an ingest token, stores its hash, and returns the
// credential's id together with the token itself.
func seedIngestCredential(t *testing.T, q *sqlcgen.Queries, platform, cloud string) (uuid.UUID, string) {
	t.Helper()

	token, err := auth.GenerateIngestToken()
	if err != nil {
		t.Fatalf("generating the ingest token: %v", err)
	}
	id, err := q.CreateIngestCredential(t.Context(), sqlcgen.CreateIngestCredentialParams{
		Platform:    platform,
		Cloud:       cloud,
		TokenHash:   auth.HashToken(token),
		Description: pgtype.Text{String: "auth integration test", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateIngestCredential() error = %v, want nil", err)
	}
	return id, token
}

// seedAPIToken issues a query token for the given role and scope, stores its
// hash, and returns the row's id together with the token itself.
func seedAPIToken(t *testing.T, q *sqlcgen.Queries, role auth.Role, projectIDs []uuid.UUID) (uuid.UUID, string) {
	t.Helper()

	token, err := auth.GenerateAPIToken()
	if err != nil {
		t.Fatalf("generating the api token: %v", err)
	}
	// project_ids is NOT NULL, so a token that names no project carries an
	// empty array rather than a nil slice, which pgx would send as NULL.
	if projectIDs == nil {
		projectIDs = []uuid.UUID{}
	}
	id, err := q.CreateAPIToken(t.Context(), sqlcgen.CreateAPITokenParams{
		TokenHash:   auth.HashToken(token),
		Role:        string(role),
		ProjectIds:  projectIDs,
		Description: pgtype.Text{String: "auth integration test", Valid: true},
	})
	if err != nil {
		t.Fatalf("CreateAPIToken() error = %v, want nil", err)
	}
	return id, token
}

// seedProject registers one project and returns its registry id.
func seedProject(t *testing.T, db storetest.DB, cloud, externalID string) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO projects (platform, cloud, external_id) VALUES ('openstack', $1, $2) RETURNING id`,
		cloud, externalID).Scan(&id); err != nil {
		t.Fatalf("inserting the project %q: %v", externalID, err)
	}
	return id
}

// closedQueries opens a pool of its own against the test database and closes it
// again. Every query on the result fails with something other than
// pgx.ErrNoRows, which is the outage the middlewares have to tell from a bad
// credential, and the pool the other tests share stays untouched.
func closedQueries(t *testing.T, dbURL string) *sqlcgen.Queries {
	t.Helper()

	s, err := store.New(t.Context(), dbURL, 1)
	if err != nil {
		t.Fatalf("store.New() error = %v, want nil", err)
	}
	s.Close()
	return sqlcgen.New(s.Pool())
}

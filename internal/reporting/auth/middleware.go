// Package auth authenticates the callers of the Reporting API. It mints and
// hashes the two kinds of bearer token, resolves a presented token into a
// principal, and hands that principal to the handlers through the request
// context.
//
// Three middlewares guard the three classes of route: Ingest for the ingest
// endpoints, Query for the read endpoints, and Internal for /internal/*. All
// three answer a missing, malformed, unknown, or revoked credential with a 401
// problem document, and a lookup that fails for any other reason with a 500: a
// database outage must not read as a bad credential.
//
// httpapi picks one of them per route, after chi has matched the route, so the
// matched pattern decides which guard a request meets and which role it needs.
// A route the pattern table does not name is refused there rather than served,
// which keeps a new operation from going online with its credential unchecked.
//
// The Authenticator interface is where an OIDC provider slots in later. Today
// the only implementation is the static lookup against the api_tokens table.
//
// The middlewares take an accessor for the request-scoped logger instead of
// importing httpapi, because httpapi is what mounts them.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.4.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// Mode says whether the middlewares check anything. It mirrors the values of
// TALLY_REPORTING_AUTH_MODE, which the server maps onto these constants.
type Mode string

const (
	// ModeEnforced is the production mode: every request needs a credential.
	ModeEnforced Mode = "enforced"
	// ModeDisabled serves every request without checking a credential. It
	// exists for development and tests.
	ModeDisabled Mode = "disabled"
)

// The scheme and the headers of the bearer exchange, spelled out here because
// the failure paths all repeat them.
const (
	authorizationHeader = "Authorization"
	challengeHeader     = "WWW-Authenticate"
	bearerScheme        = "Bearer"
)

// Ingest builds the middleware guarding the ingest endpoints. It resolves the
// bearer token to an ingest credential and puts that credential in the request
// context, where IngestFromContext reads it.
//
// In ModeDisabled it checks nothing, so a handler finds no credential in the
// context.
//
// The logger accessor reports lookup failures against the request-scoped
// logger; a nil accessor falls back to slog.Default.
func Ingest(q *sqlcgen.Queries, mode Mode, logger func(context.Context) *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mode == ModeDisabled {
				next.ServeHTTP(w, r)
				return
			}

			token, ok := bearerToken(r)
			if !ok {
				writeChallenge(w, "the request carries no bearer token")
				return
			}

			credential, err := q.GetIngestCredentialByTokenHash(r.Context(), HashToken(token))
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					writeChallenge(w, "the bearer token is unknown or revoked")
					return
				}
				loggerFor(r.Context(), logger).Error("looking up the ingest credential", "error", err)
				writeInternal(w)
				return
			}

			next.ServeHTTP(w, r.WithContext(withIngest(r.Context(), IngestPrincipal{
				CredentialID: credential.ID,
				Platform:     credential.Platform,
				Cloud:        credential.Cloud,
			})))
		})
	}
}

// Authenticator turns the bearer token of a query request into the principal it
// stands for. It returns an error matching pgx.ErrNoRows when the token is
// unknown or revoked, and any other error when the lookup itself failed.
//
// It is the seam for accepting credentials the api_tokens table knows nothing
// about, such as the OIDC JWTs behind TALLY_REPORTING_OIDC_JWKS_URL.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (QueryPrincipal, error)
}

// StaticTokenAuthenticator resolves the tokens issued by the admin CLI, which
// live in the api_tokens table.
type StaticTokenAuthenticator struct {
	q *sqlcgen.Queries
}

// NewStaticTokenAuthenticator returns an Authenticator reading from q.
func NewStaticTokenAuthenticator(q *sqlcgen.Queries) *StaticTokenAuthenticator {
	return &StaticTokenAuthenticator{q: q}
}

// Authenticate looks the token's hash up in api_tokens. The query skips revoked
// rows, so a revoked token is reported the same way an unknown one is: as
// pgx.ErrNoRows, which the returned error wraps.
func (a *StaticTokenAuthenticator) Authenticate(ctx context.Context, token string) (QueryPrincipal, error) {
	row, err := a.q.GetAPITokenByTokenHash(ctx, HashToken(token))
	if err != nil {
		return QueryPrincipal{}, fmt.Errorf("looking up the api token: %w", err)
	}
	return QueryPrincipal{
		TokenID:    row.ID,
		Role:       Role(row.Role),
		ProjectIDs: row.ProjectIds,
	}, nil
}

// Query builds the middleware guarding a class of read endpoints. It
// authenticates the bearer token, refuses a principal whose role does not cover
// required with a 403, and otherwise puts the principal in the request context,
// where QueryFromContext reads it.
//
// In ModeDisabled it serves every request as an admin, so a development setup
// reaches the same handlers a token would.
//
// The logger accessor reports lookup failures against the request-scoped
// logger; a nil accessor falls back to slog.Default.
func Query(a Authenticator, required Role, mode Mode, logger func(context.Context) *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mode == ModeDisabled {
				next.ServeHTTP(w, r.WithContext(withQuery(r.Context(), QueryPrincipal{Role: RoleAdmin})))
				return
			}

			token, ok := bearerToken(r)
			if !ok {
				writeChallenge(w, "the request carries no bearer token")
				return
			}

			principal, err := a.Authenticate(r.Context(), token)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					writeChallenge(w, "the bearer token is unknown or revoked")
					return
				}
				loggerFor(r.Context(), logger).Error("authenticating the query token", "error", err)
				writeInternal(w)
				return
			}

			if !principal.Role.Allows(required) {
				problem.Write(w, http.StatusForbidden, problem.TypeForbidden,
					"Forbidden", "this token's role does not cover this endpoint")
				return
			}

			next.ServeHTTP(w, r.WithContext(withQuery(r.Context(), principal)))
		})
	}
}

// Internal builds the middleware guarding /internal/*, the routes the other
// Tally components call. They share one secret rather than holding a row in
// api_tokens, so the comparison happens here and in constant time.
//
// In ModeDisabled it serves every request.
func Internal(token string, mode Mode) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if mode == ModeDisabled {
				next.ServeHTTP(w, r)
				return
			}

			presented, ok := bearerToken(r)
			if !ok {
				writeChallenge(w, "the request carries no bearer token")
				return
			}
			if subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				writeChallenge(w, "the bearer token is not the internal token")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// bearerToken returns the token of the Authorization header. The second result
// is false when the header is absent, names another scheme, or carries no
// token, all of which are answered the same way.
func bearerToken(r *http.Request) (string, bool) {
	scheme, value, found := strings.Cut(r.Header.Get(authorizationHeader), " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}
	token := strings.TrimSpace(value)
	return token, token != ""
}

// writeChallenge answers a request whose credential this API cannot accept. The
// challenge header names the scheme the caller should have used, as RFC 9110
// requires of a 401.
func writeChallenge(w http.ResponseWriter, detail string) {
	w.Header().Set(challengeHeader, bearerScheme)
	problem.Write(w, http.StatusUnauthorized, problem.TypeUnauthorized, "Unauthorized", detail)
}

// writeInternal answers a request whose credential could not be checked at all.
// The detail says nothing about the token, which is unrelated to the failure.
func writeInternal(w http.ResponseWriter) {
	problem.Write(w, http.StatusInternalServerError, problem.TypeInternal,
		"Internal error", "the credential could not be checked")
}

// loggerFor returns the logger a failure is reported through: the one the
// caller's accessor holds for this request, or the default logger when the
// caller passed no accessor.
func loggerFor(ctx context.Context, logger func(context.Context) *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}
	return logger(ctx)
}

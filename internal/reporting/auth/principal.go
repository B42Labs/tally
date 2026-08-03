package auth

import (
	"context"

	"github.com/google/uuid"
)

// Role is what an API token may do. The roles form a chain: admin contains
// read_all, and read_all contains project.
type Role string

const (
	// RoleAdmin may use every endpoint, including the administrative ones.
	RoleAdmin Role = "admin"
	// RoleReadAll may read the data of every project.
	RoleReadAll Role = "read_all"
	// RoleProject may read the data of the projects its token names.
	RoleProject Role = "project"
)

// roleRank orders the roles: a role covers every role that ranks no higher than
// itself. A role that is missing here covers nothing, which is how a token row
// holding a role this code does not know ends up with no access rather than
// with the wrong one.
var roleRank = map[Role]int{
	RoleProject: 1,
	RoleReadAll: 2,
	RoleAdmin:   3,
}

// Allows reports whether a caller holding r may enter a route that requires the
// given role.
func (r Role) Allows(required Role) bool {
	held, ok := roleRank[r]
	if !ok {
		return false
	}
	need, ok := roleRank[required]
	if !ok {
		return false
	}
	return held >= need
}

// IngestPrincipal is the credential an accepted ingest request presented. The
// platform and the cloud are the ones the credential was issued for, so a
// handler takes them from here rather than from the request body: a credential
// can only report for its own cloud.
type IngestPrincipal struct {
	// CredentialID is the ingest_credentials row the token resolved to.
	CredentialID uuid.UUID
	// Platform is the platform the credential reports for, openstack for
	// example.
	Platform string
	// Cloud is the cloud the credential reports for.
	Cloud string
}

// QueryPrincipal is the API token an accepted query request presented.
type QueryPrincipal struct {
	// TokenID is the api_tokens row the token resolved to.
	TokenID uuid.UUID
	// Role is what the token may do.
	Role Role
	// ProjectIDs are the registry project ids a project-role token is limited
	// to. It is empty for the other roles, which see every project.
	ProjectIDs []uuid.UUID
}

// contextKey is the private type of this package's context keys, so no other
// package can collide with them.
type contextKey int

const (
	contextKeyIngest contextKey = iota
	contextKeyQuery
)

// withIngest returns ctx carrying the authenticated ingest credential.
func withIngest(ctx context.Context, p IngestPrincipal) context.Context {
	return context.WithValue(ctx, contextKeyIngest, p)
}

// IngestFromContext returns the ingest credential the request authenticated
// with. The second result is false outside an ingest route and on requests
// served while authentication is disabled.
func IngestFromContext(ctx context.Context) (IngestPrincipal, bool) {
	p, ok := ctx.Value(contextKeyIngest).(IngestPrincipal)
	return p, ok
}

// withQuery returns ctx carrying the authenticated API token.
func withQuery(ctx context.Context, p QueryPrincipal) context.Context {
	return context.WithValue(ctx, contextKeyQuery, p)
}

// QueryFromContext returns the API token the request authenticated with. The
// second result is false outside a query route.
func QueryFromContext(ctx context.Context) (QueryPrincipal, bool) {
	p, ok := ctx.Value(contextKeyQuery).(QueryPrincipal)
	return p, ok
}

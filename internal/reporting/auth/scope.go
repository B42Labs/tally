package auth

import (
	"context"
	"fmt"

	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// ProjectRef identifies a project the way the event and resource tables do,
// which is not by its registry id.
type ProjectRef struct {
	// Cloud is the cloud the project lives in.
	Cloud string
	// ExternalID is the project id that cloud uses.
	ExternalID string
}

// Scope is what a principal may read. The two cases it separates look alike in
// a bare slice of refs and mean the opposite: no filter at all, and a filter
// nothing passes.
type Scope struct {
	// Unfiltered says the principal reads every project. Refs is empty then.
	Unfiltered bool
	// Refs are the projects a filtered principal reads, empty when it reaches
	// none. A caller filters on these whenever Unfiltered is false, so a token
	// whose projects are gone reads nothing rather than everything.
	Refs []ProjectRef
}

// ResolveProjectScope reports what a query principal may read. An admin or a
// read_all token is unfiltered. A project token resolves to the (cloud,
// external id) pairs the query filters run on, which is how the events and
// resources tables name a project.
//
// A project token stays filtered even when it resolves to no pair: registry ids
// that name no project drop out, which narrows the scope rather than widening
// it, so a project removed from the registry stops being visible instead of
// turning its token into an unfiltered one.
//
// The roles are matched against an allow-list, the way roleRank ranks them: a
// role this build does not know is an error rather than an unfiltered scope, so
// a token row written by a newer build reads nothing rather than everything.
func ResolveProjectScope(ctx context.Context, q *sqlcgen.Queries, p QueryPrincipal) (Scope, error) {
	switch p.Role {
	case RoleAdmin, RoleReadAll:
		return Scope{Unfiltered: true}, nil
	case RoleProject:
	default:
		return Scope{}, fmt.Errorf("resolving project scope: role %q is not one this build knows", p.Role)
	}

	rows, err := q.GetProjectRefsByIDs(ctx, p.ProjectIDs)
	if err != nil {
		return Scope{}, fmt.Errorf("resolving project scope: %w", err)
	}

	refs := make([]ProjectRef, 0, len(rows))
	for _, row := range rows {
		refs = append(refs, ProjectRef{Cloud: row.Cloud, ExternalID: row.ExternalID})
	}
	return Scope{Refs: refs}, nil
}

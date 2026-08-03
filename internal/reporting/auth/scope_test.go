package auth_test

import (
	"strings"
	"testing"

	"github.com/b42labs/tally/internal/reporting/auth"
)

// TestResolveProjectScopeRefusesUnknownRoles covers the roles no allow-list
// arm names. Resolving them to an unfiltered scope would hand a token row this
// build cannot interpret — one a newer build wrote, or one whose role column
// was mistyped — every project in the database.
//
// The queries are nil on purpose: a refused role must be refused before any
// lookup runs.
func TestResolveProjectScopeRefusesUnknownRoles(t *testing.T) {
	for name, role := range map[string]auth.Role{
		"a role this build does not know": auth.Role("root"),
		"the zero value":                  auth.Role(""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			scope, err := auth.ResolveProjectScope(t.Context(), nil, auth.QueryPrincipal{Role: role})
			if err == nil {
				t.Fatalf("ResolveProjectScope() error = nil, want the role to be refused")
			}
			if !strings.HasPrefix(err.Error(), "resolving project scope: ") {
				t.Errorf("ResolveProjectScope() error = %q, want it prefixed %q", err, "resolving project scope: ")
			}
			if scope.Unfiltered {
				t.Error("scope is unfiltered, want a refused role to reach no project")
			}
		})
	}
}

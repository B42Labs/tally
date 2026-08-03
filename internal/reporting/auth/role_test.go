package auth_test

import (
	"testing"

	"github.com/b42labs/tally/internal/reporting/auth"
)

func TestRoleAllows(t *testing.T) {
	// The full lattice, held role against required role. A role covers itself
	// and everything below it. A role this code does not know covers nothing,
	// and so does no role cover a requirement it does not know.
	for name, tc := range map[string]struct {
		held     auth.Role
		required auth.Role
		want     bool
	}{
		"admin covers admin":                          {auth.RoleAdmin, auth.RoleAdmin, true},
		"admin covers read_all":                       {auth.RoleAdmin, auth.RoleReadAll, true},
		"admin covers project":                        {auth.RoleAdmin, auth.RoleProject, true},
		"read_all does not cover admin":               {auth.RoleReadAll, auth.RoleAdmin, false},
		"read_all covers read_all":                    {auth.RoleReadAll, auth.RoleReadAll, true},
		"read_all covers project":                     {auth.RoleReadAll, auth.RoleProject, true},
		"project does not cover admin":                {auth.RoleProject, auth.RoleAdmin, false},
		"project does not cover read_all":             {auth.RoleProject, auth.RoleReadAll, false},
		"project covers project":                      {auth.RoleProject, auth.RoleProject, true},
		"an unknown role does not cover admin":        {auth.Role("root"), auth.RoleAdmin, false},
		"an unknown role does not cover read_all":     {auth.Role("root"), auth.RoleReadAll, false},
		"an unknown role does not cover project":      {auth.Role("root"), auth.RoleProject, false},
		"an empty role covers nothing":                {auth.Role(""), auth.RoleProject, false},
		"admin does not cover an unknown requirement": {auth.RoleAdmin, auth.Role("root"), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.held.Allows(tc.required); got != tc.want {
				t.Errorf("Role(%q).Allows(%q) = %t, want %t", tc.held, tc.required, got, tc.want)
			}
		})
	}
}

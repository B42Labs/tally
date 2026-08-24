package attribution_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/engine/attribution"
	"github.com/b42labs/tally/internal/engine/source"
)

// The two attributing relation types the cases use. Only their text reaches an
// attribution, so a case that asserts a type gives its two edges different ones.
const (
	infrastructureTenant = "infrastructure_tenant"
	managedTenant        = "managed_tenant"
)

// projectID and relationID derive a uuid from a number, so a case names its
// projects and its relations by that number and the ascending order of the
// numbers is the ascending order of the ids. Relations reach Resolve in that
// order, which is what breaks a tie between two paths of equal length.
func projectID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0000000a-0000-0000-0000-%012d", n))
}

func relationID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0000000b-0000-0000-0000-%012d", n))
}

// project is one registry entry. Only its id is walked; the platform and the
// external id travel along so a case reads like the registry it mirrors.
func project(n int, platform, externalID string) source.Project {
	return source.Project{
		ID:         projectID(n),
		Platform:   platform,
		Cloud:      "prod",
		ExternalID: externalID,
	}
}

// relation is edge n, from the project that attributes to the one it attributes
// away. The validity is left zero: Resolve is given the relations that overlap
// the period, so it reads neither end of theirs.
func relation(n, from, to int, relationType string) source.Relation {
	return source.Relation{
		ID:           relationID(n),
		SourceID:     projectID(from),
		TargetID:     projectID(to),
		RelationType: relationType,
	}
}

// assertIDs holds one project list against the ids expected in it, in order.
func assertIDs(t *testing.T, name string, got, want []uuid.UUID) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// assertAttributed holds one project's attribution against the top-level
// project it bills under and the relation type of the edge that claimed it.
func assertAttributed(t *testing.T, resolution attribution.Resolution, attributed, root uuid.UUID, relationType string) {
	t.Helper()

	got, ok := resolution.Attributed[attributed]
	if !ok {
		t.Fatalf("Attributed[%v] is missing, want root %v", attributed, root)
	}
	want := attribution.Attribution{Root: root, RelationType: relationType}
	if got != want {
		t.Errorf("Attributed[%v] = %+v, want %+v", attributed, got, want)
	}
}

// assertWarnings holds the warnings against the ones expected, in the order
// they are found. A case that expects none passes nil.
func assertWarnings(t *testing.T, got, want []attribution.Warning) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Errorf("Warnings = %+v, want %+v", got, want)
	}
}

// TestResolveGardenerGraph resolves the graph of the concept's example: a
// Gardener project that attributes the two OpenStack tenants of its shoots, and
// an unrelated OpenStack project beside it. The same_owner relation the concept
// draws is not an attributing type and thus never loaded, so it is not here.
func TestResolveGardenerGraph(t *testing.T) {
	projects := []source.Project{
		project(1, "openstack", "team-alpha-os"),
		project(2, "gardener", "team-alpha"),
		project(3, "openstack", "shoot-abc-123"),
		project(4, "openstack", "shoot-def-456"),
	}
	relations := []source.Relation{
		relation(1, 2, 3, infrastructureTenant),
		relation(2, 2, 4, infrastructureTenant),
	}

	resolution := attribution.Resolve(projects, relations)

	assertIDs(t, "TopLevel", resolution.TopLevel, []uuid.UUID{projectID(1), projectID(2)})
	if len(resolution.Attributed) != 2 {
		t.Fatalf("Attributed = %d projects, want 2", len(resolution.Attributed))
	}
	assertAttributed(t, resolution, projectID(3), projectID(2), infrastructureTenant)
	assertAttributed(t, resolution, projectID(4), projectID(2), infrastructureTenant)
	assertWarnings(t, resolution.Warnings, nil)
}

// TestResolveShorterPathWins resolves a project two paths of unequal length
// reach: A → C directly, and A → B → C over one hop more. The direct edge
// claims it, and the edge that arrives a level later is the warning.
func TestResolveShorterPathWins(t *testing.T) {
	projects := []source.Project{
		project(1, "gardener", "a"),
		project(2, "openstack", "b"),
		project(3, "openstack", "c"),
	}
	relations := []source.Relation{
		relation(1, 1, 2, infrastructureTenant),
		relation(2, 1, 3, infrastructureTenant),
		relation(3, 2, 3, managedTenant),
	}

	resolution := attribution.Resolve(projects, relations)

	assertIDs(t, "TopLevel", resolution.TopLevel, []uuid.UUID{projectID(1)})
	assertAttributed(t, resolution, projectID(2), projectID(1), infrastructureTenant)
	// C bills under A either way; what the shorter path decides is the relation
	// type the statement shows it under.
	assertAttributed(t, resolution, projectID(3), projectID(1), infrastructureTenant)
	assertWarnings(t, resolution.Warnings, []attribution.Warning{{
		Code:       attribution.WarningMultiplePaths,
		ProjectID:  projectID(3),
		RelationID: relationID(3).String(),
	}})
}

// TestResolveTieBreaksOnRelationID resolves a project two top-level projects
// each claim directly, so the two paths are of the same length and only the
// relation id separates them.
func TestResolveTieBreaksOnRelationID(t *testing.T) {
	projects := []source.Project{
		project(1, "gardener", "a"),
		project(2, "gardener", "b"),
		project(3, "openstack", "c"),
	}
	relations := []source.Relation{
		relation(1, 1, 3, infrastructureTenant),
		relation(2, 2, 3, managedTenant),
	}

	resolution := attribution.Resolve(projects, relations)

	assertIDs(t, "TopLevel", resolution.TopLevel, []uuid.UUID{projectID(1), projectID(2)})
	assertAttributed(t, resolution, projectID(3), projectID(1), infrastructureTenant)
	assertWarnings(t, resolution.Warnings, []attribution.Warning{{
		Code:       attribution.WarningMultiplePaths,
		ProjectID:  projectID(3),
		RelationID: relationID(2).String(),
	}})

	// The tie is what a walk that iterates a map would decide differently every
	// time, and the invoice of a period must not depend on which run billed it.
	if again := attribution.Resolve(projects, relations); !reflect.DeepEqual(resolution, again) {
		t.Errorf("Resolve() = %+v on the second call, want %+v", again, resolution)
	}
}

// TestResolveChainFlattens resolves A → B → C. C bills under A rather than
// under B, carrying the type of the edge it was claimed over.
func TestResolveChainFlattens(t *testing.T) {
	projects := []source.Project{
		project(1, "gardener", "a"),
		project(2, "gardener", "b"),
		project(3, "openstack", "c"),
	}
	relations := []source.Relation{
		relation(1, 1, 2, infrastructureTenant),
		relation(2, 2, 3, managedTenant),
	}

	resolution := attribution.Resolve(projects, relations)

	assertIDs(t, "TopLevel", resolution.TopLevel, []uuid.UUID{projectID(1)})
	assertAttributed(t, resolution, projectID(2), projectID(1), infrastructureTenant)
	assertAttributed(t, resolution, projectID(3), projectID(1), managedTenant)
	assertWarnings(t, resolution.Warnings, nil)
}

// TestResolveCycle resolves graphs the registry is meant to refuse. The walk
// ends on all of them, which is what a case returning at all proves.
func TestResolveCycle(t *testing.T) {
	t.Run("a cycle no root reaches is orphaned rather than billed nowhere", func(t *testing.T) {
		projects := []source.Project{
			project(1, "gardener", "a"),
			project(2, "openstack", "x"),
			project(3, "openstack", "y"),
		}
		relations := []source.Relation{
			relation(1, 2, 3, infrastructureTenant),
			relation(2, 3, 2, infrastructureTenant),
		}

		resolution := attribution.Resolve(projects, relations)

		// Both members are attributed away, so neither is top level, and neither
		// is claimed, so neither is attributed either. Standalone is all that is
		// left, and the warning is what makes that visible.
		assertIDs(t, "TopLevel", resolution.TopLevel, []uuid.UUID{projectID(1)})
		if len(resolution.Attributed) != 0 {
			t.Errorf("Attributed = %+v, want none", resolution.Attributed)
		}
		assertWarnings(t, resolution.Warnings, []attribution.Warning{
			{Code: attribution.WarningCycle, ProjectID: projectID(2)},
			{Code: attribution.WarningCycle, ProjectID: projectID(3)},
		})
	})

	t.Run("a project that attributes itself is orphaned too", func(t *testing.T) {
		projects := []source.Project{
			project(1, "gardener", "a"),
			project(2, "openstack", "z"),
		}
		relations := []source.Relation{relation(1, 2, 2, infrastructureTenant)}

		resolution := attribution.Resolve(projects, relations)

		assertIDs(t, "TopLevel", resolution.TopLevel, []uuid.UUID{projectID(1)})
		if len(resolution.Attributed) != 0 {
			t.Errorf("Attributed = %+v, want none", resolution.Attributed)
		}
		assertWarnings(t, resolution.Warnings, []attribution.Warning{
			{Code: attribution.WarningCycle, ProjectID: projectID(2)},
		})
	})

	t.Run("a cycle a root reaches is claimed under that root", func(t *testing.T) {
		projects := []source.Project{
			project(1, "gardener", "a"),
			project(2, "gardener", "b"),
			project(3, "openstack", "c"),
		}
		relations := []source.Relation{
			relation(1, 1, 2, infrastructureTenant),
			relation(2, 2, 3, managedTenant),
			relation(3, 3, 2, managedTenant),
		}

		resolution := attribution.Resolve(projects, relations)

		assertIDs(t, "TopLevel", resolution.TopLevel, []uuid.UUID{projectID(1)})
		assertAttributed(t, resolution, projectID(2), projectID(1), infrastructureTenant)
		assertAttributed(t, resolution, projectID(3), projectID(1), managedTenant)
		// The edge back into B is walked once, after B was claimed, and is a
		// second path rather than a cycle the walk has to break out of.
		assertWarnings(t, resolution.Warnings, []attribution.Warning{{
			Code:       attribution.WarningMultiplePaths,
			ProjectID:  projectID(2),
			RelationID: relationID(3).String(),
		}})
	})
}

// TestResolveEmptyRelations resolves a registry no attributing relation covers,
// which is what a deployment that turned attribution off loads.
func TestResolveEmptyRelations(t *testing.T) {
	projects := []source.Project{
		project(1, "gardener", "a"),
		project(2, "openstack", "b"),
		project(3, "openstack", "c"),
	}

	cases := []struct {
		name      string
		relations []source.Relation
	}{
		{"no relations at all", nil},
		{"an empty relation list", []source.Relation{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolution := attribution.Resolve(projects, tc.relations)

			assertIDs(t, "TopLevel", resolution.TopLevel,
				[]uuid.UUID{projectID(1), projectID(2), projectID(3)})
			if len(resolution.Attributed) != 0 {
				t.Errorf("Attributed = %+v, want none", resolution.Attributed)
			}
			assertWarnings(t, resolution.Warnings, nil)
		})
	}
}

// TestResolveEmptyProjects resolves an empty registry: there is nothing to bill
// and nothing to warn about.
func TestResolveEmptyProjects(t *testing.T) {
	cases := []struct {
		name     string
		projects []source.Project
	}{
		{"no projects at all", nil},
		{"an empty project list", []source.Project{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resolution := attribution.Resolve(tc.projects, nil)

			if !reflect.DeepEqual(resolution, attribution.Resolution{}) {
				t.Errorf("Resolve() = %+v, want the zero resolution", resolution)
			}
		})
	}
}

// TestWarningJSON marshals the two warnings the way the run writes them into
// runs.stats. A cycle warning has no relation to name and says so by leaving
// the key out rather than by carrying an empty uuid.
func TestWarningJSON(t *testing.T) {
	cases := []struct {
		name     string
		warning  attribution.Warning
		document string
	}{
		{
			name: "a project two paths reach names the losing relation",
			warning: attribution.Warning{
				Code:       attribution.WarningMultiplePaths,
				ProjectID:  projectID(3),
				RelationID: relationID(2).String(),
			},
			document: `{"code":"attribution_multiple_paths",` +
				`"project_id":"0000000a-0000-0000-0000-000000000003",` +
				`"relation_id":"0000000b-0000-0000-0000-000000000002"}`,
		},
		{
			name: "a project in a cycle names no relation",
			warning: attribution.Warning{
				Code:      attribution.WarningCycle,
				ProjectID: projectID(2),
			},
			document: `{"code":"attribution_cycle",` +
				`"project_id":"0000000a-0000-0000-0000-000000000002"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.warning)
			if err != nil {
				t.Fatalf("Marshal() error = %v, want nil", err)
			}
			if string(data) != tc.document {
				t.Errorf("Marshal() = %s, want %s", data, tc.document)
			}
		})
	}
}

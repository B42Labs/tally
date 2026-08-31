package simulator

import (
	"reflect"
	"testing"
	"time"
)

// The two clouds the registrations of a month are read under. They differ
// because the tenants belong to an OpenStack installation and the Gardener
// projects to a Gardener installation, and the registry keys a row by its
// cloud.
const (
	tenantsCloud = "os-sim"
	gardenCloud  = "garden-sim"
)

// TestRegistrationsNameEveryTenantOnce covers what a month registers. Every row
// the registry is meant to hold is decided here, so the test holds the
// registrations against the month they were read from: a tenant without a row
// is usage nobody can bill, a duplicate key is a row the registry refuses, and
// a relation pointing at a tenant the month never ran in attributes the cost of
// a Gardener project to nothing.
func TestRegistrationsNameEveryTenantOnce(t *testing.T) {
	month := namedMonth(t, 1, july2026, tenantsCloud)

	registrations, err := RegistrationsOf(month, gardenCloud)
	if err != nil {
		t.Fatalf("RegistrationsOf(month, %q) error = %v, want nil", gardenCloud, err)
	}

	wantProjects := len(month.Tenants) + len(month.GardenerProjects)
	if len(registrations.Projects) != wantProjects {
		t.Fatalf("the month registers %d projects, want %d: a row per tenant and a row per Gardener "+
			"project", len(registrations.Projects), wantProjects)
	}
	for i, tenant := range month.Tenants {
		got := registrations.Projects[i]
		want := ProjectRegistration{
			Platform: "openstack",
			Key:      ProjectKey{Cloud: tenantsCloud, ExternalID: tenant.ID},
			Name:     tenant.Name,
			Metadata: map[string]any{"created_by": "tally-openstack-simulator"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("project %d = %+v, want %+v: a tenant is registered under the cloud it ran in, by "+
				"the id its notifications carry", i, got, want)
		}
	}

	wantGardener := []ProjectRegistration{
		{
			Platform: "gardener",
			Key:      ProjectKey{Cloud: gardenCloud, ExternalID: "alpha"},
			Name:     "Gardener project alpha",
			Metadata: map[string]any{"created_by": "tally-openstack-simulator"},
		},
		{
			Platform: "gardener",
			Key:      ProjectKey{Cloud: gardenCloud, ExternalID: "beta"},
			Name:     "Gardener project beta",
			Metadata: map[string]any{"created_by": "tally-openstack-simulator"},
		},
	}
	if got := registrations.Projects[len(month.Tenants):]; !reflect.DeepEqual(got, wantGardener) {
		t.Errorf("the Gardener rows are %+v, want %+v: a project is registered under the garden cloud, by "+
			"the name an operator knows its shoots under", got, wantGardener)
	}

	// The registry keys a row by its cloud and external id, so two rows on one
	// key are one row, and the second registration would overwrite or be refused.
	keys := make(map[ProjectKey]int, len(registrations.Projects))
	openstackIDs := make(map[string]bool, len(month.Tenants))
	for i, project := range registrations.Projects {
		if first, ok := keys[project.Key]; ok {
			t.Errorf("project %d and project %d both carry the key %+v, want %d distinct keys: two rows on "+
				"one key are one row", first, i, project.Key, wantProjects)
		}
		keys[project.Key] = i
		if project.Platform == "openstack" {
			openstackIDs[project.Key.ExternalID] = true
		}
	}

	// One report per unregistered id rather than one per interval, because a
	// tenant without a row carries hundreds of them.
	unregistered := make(map[string]string)
	for _, resource := range month.Oracle.Resources {
		for _, interval := range resource.Intervals {
			if !openstackIDs[interval.ProjectID] {
				unregistered[interval.ProjectID] = resource.ResourceType + " " + resource.ResourceID
			}
		}
	}
	for id, resource := range unregistered {
		t.Errorf("the oracle bills the %s to the project %s, which no registration names, want every "+
			"project of the month registered: usage under an unregistered id is booked against no row",
			resource, id)
	}

	if len(registrations.Relations) != len(month.GardenerProjects) {
		t.Fatalf("the month registers %d relations, want %d: one per Gardener project",
			len(registrations.Relations), len(month.GardenerProjects))
	}
	if want := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC); !month.Oracle.PeriodFrom.Equal(want) {
		t.Fatalf("the oracle states the period from %s, want %s: the relations are valid from the start of "+
			"the month", month.Oracle.PeriodFrom.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	for i, relation := range registrations.Relations {
		if relation.RelationType != "infrastructure_tenant" {
			t.Errorf("relation %d is of type %q, want %q: it is the type the engine attributes along by "+
				"default", i, relation.RelationType, "infrastructure_tenant")
		}
		if !relation.ValidFrom.Equal(month.Oracle.PeriodFrom) {
			t.Errorf("relation %d is valid from %s, want %s: usage of the first day is attributed too",
				i, relation.ValidFrom.Format(time.RFC3339), month.Oracle.PeriodFrom.Format(time.RFC3339))
		}
		if _, ok := keys[relation.Source]; !ok {
			t.Errorf("relation %d points from %+v, which no registration names, want a registered row: "+
				"the registry refuses a relation to a row it does not hold", i, relation.Source)
		}
		if _, ok := keys[relation.Target]; !ok {
			t.Errorf("relation %d points at %+v, which no registration names, want a registered row",
				i, relation.Target)
		}
	}

	first := registrations.Relations[0]
	wantSource := ProjectKey{Cloud: gardenCloud, ExternalID: "alpha"}
	wantTarget := ProjectKey{Cloud: tenantsCloud, ExternalID: month.GardenerProjects[0].TenantID}
	if first.Source != wantSource || first.Target != wantTarget {
		t.Errorf("relation 0 points from %+v at %+v, want from %+v at %+v: the project is the source and "+
			"the tenant it runs on the target", first.Source, first.Target, wantSource, wantTarget)
	}

	wantShoots := [][]string{{"api-prod", "api-dev"}, {"batch"}}
	for i, want := range wantShoots {
		got, ok := registrations.Relations[i].Metadata["shoots"].([]string)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("relation %d states the shoots %v, want %v: the metadata says which shoots the "+
				"attributed tenant runs", i, registrations.Relations[i].Metadata["shoots"], want)
		}
	}

	for i, project := range registrations.Projects {
		if got := project.Metadata["created_by"]; got != "tally-openstack-simulator" {
			t.Errorf("project %d states created_by = %v, want %q: an operator reading the registry sees "+
				"what wrote the row", i, got, "tally-openstack-simulator")
		}
	}
	for i, relation := range registrations.Relations {
		if got := relation.Metadata["created_by"]; got != "tally-openstack-simulator" {
			t.Errorf("relation %d states created_by = %v, want %q", i, got, "tally-openstack-simulator")
		}
	}
}

// TestRegistrationsOfRefusesTheWrongGardenCloud covers the two garden clouds
// that name no Gardener installation. A cloud is one installation of one
// platform, so the Gardener projects cannot be keyed under the cloud the
// tenants are keyed under, and a run that registered them there would point
// every relation at a row of the OpenStack installation.
func TestRegistrationsOfRefusesTheWrongGardenCloud(t *testing.T) {
	month := namedMonth(t, 1, july2026, tenantsCloud)

	tests := []struct {
		name        string
		gardenCloud string
		wantErr     string
	}{
		{
			name:        "empty",
			gardenCloud: "",
			wantErr:     "the garden cloud is empty",
		},
		{
			name:        "the tenants' cloud",
			gardenCloud: tenantsCloud,
			wantErr: `the garden cloud "os-sim" is the tenants' cloud: a cloud is one installation of one ` +
				`platform`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := RegistrationsOf(month, tt.gardenCloud)

			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("RegistrationsOf(month, %q) error = %v, want %q", tt.gardenCloud, err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, Registrations{}) {
				t.Errorf("RegistrationsOf(month, %q) = %+v, want nothing: a refused call registers no row",
					tt.gardenCloud, got)
			}
		})
	}
}

// TestRegistrationsOfAnEmptyMonth covers a month that names no tenant. It
// registers nothing rather than failing, because an empty world is a world, and
// what the registry is fed is read off the month rather than assumed of it.
func TestRegistrationsOfAnEmptyMonth(t *testing.T) {
	got, err := RegistrationsOf(Month{}, gardenCloud)
	if err != nil {
		t.Fatalf("RegistrationsOf(Month{}, %q) error = %v, want nil", gardenCloud, err)
	}

	if len(got.Projects) != 0 || len(got.Relations) != 0 {
		t.Errorf("a month naming no tenant registers %d projects and %d relations, want none of either",
			len(got.Projects), len(got.Relations))
	}
}

// This file states what one generated month registers with the project
// registry of the Reporting API: a row per tenant, a row per Gardener project,
// and the relation that attributes a tenant's cost to the project running on
// it. The rows are decided from the month alone, without a network, so what a
// run would register is a value a test can hold against the oracle.
package simulator

import (
	"errors"
	"fmt"
	"time"
)

// ProjectKey is the pair the registry keys a project by.
type ProjectKey struct{ Cloud, ExternalID string }

// ProjectRegistration is one projects row to register.
type ProjectRegistration struct {
	Platform string
	Key      ProjectKey
	Name     string
	Metadata map[string]any
}

// RelationRegistration is one relation to create between two registered rows.
type RelationRegistration struct {
	Source, Target ProjectKey
	RelationType   string
	Metadata       map[string]any
	ValidFrom      time.Time
}

// Registrations is what one month registers: the rows first, the relations
// between them second.
type Registrations struct {
	Projects  []ProjectRegistration
	Relations []RelationRegistration
}

// relationInfrastructureTenant is the relation type that attributes a tenant's
// cost to the Gardener project, the default of the engine and of the Reporting
// API. It is mirrored here rather than imported, the way sender.go mirrors the
// ingest result: the simulator is a client of the API.
const relationInfrastructureTenant = "infrastructure_tenant"

// createdBy is the metadata member every row and relation carries, so an
// operator reading the registry sees what wrote it.
const createdBy = "tally-openstack-simulator"

// RegistrationsOf returns the rows and relations the month registers: one
// openstack row per tenant under the cloud the month was rendered for, one
// gardener row per Gardener project under gardenCloud, and one
// infrastructure_tenant relation per Gardener project, pointing at the tenant
// its shoots run on and valid from the start of the month.
//
// The two clouds have to differ because a cloud is one installation of one
// platform. A Gardener project registered under the tenants' cloud would key a
// row of the OpenStack installation, and the relation would then point the
// project at itself.
func RegistrationsOf(month Month, gardenCloud string) (Registrations, error) {
	if gardenCloud == "" {
		return Registrations{}, errors.New("the garden cloud is empty")
	}
	if gardenCloud == month.Oracle.Cloud {
		return Registrations{}, fmt.Errorf(
			"the garden cloud %q is the tenants' cloud: a cloud is one installation of one platform",
			gardenCloud)
	}

	var registrations Registrations
	for _, tenant := range month.Tenants {
		registrations.Projects = append(registrations.Projects, ProjectRegistration{
			Platform: "openstack",
			Key:      ProjectKey{Cloud: month.Oracle.Cloud, ExternalID: tenant.ID},
			Name:     tenant.Name,
			Metadata: map[string]any{"created_by": createdBy},
		})
	}
	for _, gp := range month.GardenerProjects {
		registrations.Projects = append(registrations.Projects, ProjectRegistration{
			Platform: "gardener",
			Key:      ProjectKey{Cloud: gardenCloud, ExternalID: gp.Name},
			Name:     "Gardener project " + gp.Name,
			Metadata: map[string]any{"created_by": createdBy},
		})
	}
	for _, gp := range month.GardenerProjects {
		registrations.Relations = append(registrations.Relations, RelationRegistration{
			Source:       ProjectKey{Cloud: gardenCloud, ExternalID: gp.Name},
			Target:       ProjectKey{Cloud: month.Oracle.Cloud, ExternalID: gp.TenantID},
			RelationType: relationInfrastructureTenant,
			Metadata:     map[string]any{"created_by": createdBy, "shoots": gp.Shoots},
			ValidFrom:    month.Oracle.PeriodFrom,
		})
	}
	return registrations, nil
}

package httpapi

import (
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// TestGroupResourceCounts pins the property the one static query stands on: a
// coarser grouping is the sum of the counted rows over the dimensions it drops,
// so every grouping the route takes is answered from the rows counted along all
// five. The items carry the optional members the request grouped by and no
// others, and they come out in the order the contract states.
func TestGroupResourceCounts(t *testing.T) {
	rows := []sqlcgen.CountCurrentResourcesGroupedRow{
		{
			Cloud: "os-prod-eu1", Platform: "openstack", ResourceType: "instance",
			State: "active", ProjectID: "p-1", Resources: 2,
		},
		{
			Cloud: "os-prod-eu1", Platform: "openstack", ResourceType: "instance",
			State: "shutoff", ProjectID: "p-1", Resources: 3,
		},
		{
			Cloud: "os-prod-eu1", Platform: "openstack", ResourceType: "instance",
			State: "active", ProjectID: "p-2", Resources: 5,
		},
		{
			Cloud: "os-dev-eu1", Platform: "openstack", ResourceType: "volume",
			State: "active", ProjectID: "p-1", Resources: 7,
		},
	}

	tests := map[string]struct {
		rows    []sqlcgen.CountCurrentResourcesGroupedRow
		groupBy []GetResourceStatsParamsGroupBy
		want    []string
	}{
		"the coarsest grouping sums every row of a resource type": {
			rows: rows,
			groupBy: []GetResourceStatsParamsGroupBy{
				GetResourceStatsParamsGroupByCloud,
				GetResourceStatsParamsGroupByResourceType,
			},
			want: []string{
				"os-dev-eu1/volume/-/-/-=7",
				"os-prod-eu1/instance/-/-/-=10",
			},
		},
		"grouping by state keeps the states apart and sums the projects": {
			rows: rows,
			groupBy: []GetResourceStatsParamsGroupBy{
				GetResourceStatsParamsGroupByCloud,
				GetResourceStatsParamsGroupByResourceType,
				GetResourceStatsParamsGroupByState,
			},
			want: []string{
				"os-dev-eu1/volume/active/-/-=7",
				"os-prod-eu1/instance/active/-/-=7",
				"os-prod-eu1/instance/shutoff/-/-=3",
			},
		},
		"grouping by project and platform carries both members": {
			rows: rows,
			groupBy: []GetResourceStatsParamsGroupBy{
				GetResourceStatsParamsGroupByCloud,
				GetResourceStatsParamsGroupByResourceType,
				GetResourceStatsParamsGroupByPlatform,
				GetResourceStatsParamsGroupByProjectId,
			},
			want: []string{
				"os-dev-eu1/volume/-/openstack/p-1=7",
				"os-prod-eu1/instance/-/openstack/p-1=5",
				"os-prod-eu1/instance/-/openstack/p-2=5",
			},
		},
		"a fleet the query counted nothing of yields no item": {
			rows: nil,
			groupBy: []GetResourceStatsParamsGroupBy{
				GetResourceStatsParamsGroupByCloud,
				GetResourceStatsParamsGroupByResourceType,
			},
			want: []string{},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			items := groupResourceCounts(tc.rows, tc.groupBy)
			if items == nil {
				t.Fatal("items = nil, want an empty array rather than a null one")
			}
			if got := renderResourceItems(items); !slices.Equal(got, tc.want) {
				t.Errorf("items = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestStatsStatusFilter pins how the status parameter reaches the counting
// query, the default a request that names none included: the contract declares
// it and the generated binding does not apply it.
func TestStatsStatusFilter(t *testing.T) {
	active := GetResourceStatsParamsStatusActive
	deleted := GetResourceStatsParamsStatusDeleted
	all := GetResourceStatsParamsStatusAll

	tests := map[string]struct {
		status *GetResourceStatsParamsStatus
		want   pgtype.Bool
	}{
		"no status counts the fleet that lives": {status: nil, want: pgtype.Bool{Valid: true}},
		"active counts the fleet that lives":    {status: &active, want: pgtype.Bool{Valid: true}},
		"deleted counts the gone ones alone":    {status: &deleted, want: pgtype.Bool{Bool: true, Valid: true}},
		"all counts both, which is no filter":   {status: &all, want: pgtype.Bool{}},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := statsStatusFilter(tc.status); got != tc.want {
				t.Errorf("statsStatusFilter() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// renderResourceItems states the items as strings, so that a mismatch prints
// what the members hold rather than the addresses the optional ones carry. An
// absent member renders as a dash, which is what tells it from one holding the
// empty string.
func renderResourceItems(items []ResourceStatsItem) []string {
	rendered := make([]string, len(items))
	for i, item := range items {
		rendered[i] = fmt.Sprintf("%s/%s/%s/%s/%s=%d", item.Cloud, item.ResourceType,
			renderOptional(item.State), renderOptional(item.Platform),
			renderOptional(item.ProjectId), item.Count)
	}
	return rendered
}

// renderOptional states one optional member of a statistics item, a dash for the
// member the answer leaves out.
func renderOptional(value *string) string {
	if value == nil {
		return "-"
	}
	return *value
}

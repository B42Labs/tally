package httpapi

import (
	"cmp"
	"context"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/httpapi/problem"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// resourceStatsDetail answers every failure of this route a caller can do
// nothing about.
const resourceStatsDetail = "the resource counts could not be read"

// maxResourceStatsRows is how many groups this route counts at most. The answer
// is not paginated, so without a bound the memory one request costs is set by
// what the fleet holds: four of the five dimensions carry a handful of values
// each, but project_id carries one per tenant, so the row set grows with the
// number of projects a cloud has and every row of it is held three times over
// before a byte is written.
//
// A request above the bound is refused rather than answered short, so a client
// never mistakes a truncated grouping for a whole one. It is the bound the event
// statistics count under, because both routes answer out of one unpaginated
// slice and neither is worth more of a pod's memory than the other.
const maxResourceStatsRows = 10000

// largeFleetDetail answers a fleet carrying more groups than one answer holds.
// It names what the caller changes to get through: the query counts along all
// five dimensions whatever the request groups by, so a coarser grouping does not
// lower the row set the refusal is decided on, and the part of the fleet the
// request covers is what does.
const largeFleetDetail = "this fleet carries more groups than the unpaginated statistics serve; " +
	"a narrower status is what lowers it, and a coarser group_by does not"

// historicCountsDetail answers a request that names the instant to count at. The
// projection holds the present alone, and replaying the histories of a whole
// fleet is what the Phase 3 usage records are for.
const historicCountsDetail = "historic counts arrive with the Phase 3 usage records; " +
	"omit at for the current counts"

// GetResourceStats answers the projection counted along the dimensions the
// request groups by, one item per combination of values the fleet carries.
//
// One static query counts along all five dimensions and the handler adds its
// rows up onto the requested ones. Counts are additive, so a coarser grouping is
// the sum of the finer rows over the dimensions it drops, and the thirty-one
// groupings the route takes need one query rather than thirty-one.
//
// A project token counts the resources whose (cloud, project_id) pair one of its
// projects names, which is the pair the resource list narrows its rows by.
func (s *server) GetResourceStats(w http.ResponseWriter, r *http.Request, params GetResourceStatsParams) {
	ctx := r.Context()

	// cloud and resource_type are what an item is read by, so a grouping without
	// them counts resources it cannot name. The rule spans two members of one
	// list, which the contract cannot express; a dimension outside the enum and a
	// repeated one are refused by the validator in front of this.
	if !slices.Contains(params.GroupBy, GetResourceStatsParamsGroupByCloud) ||
		!slices.Contains(params.GroupBy, GetResourceStatsParamsGroupByResourceType) {
		problem.Write(w, http.StatusBadRequest, problem.TypeValidation,
			"Validation failed", "group_by must name cloud and resource_type")
		return
	}
	// Every at is refused, whatever instant it names. The projection holds the
	// present alone, and an instant meaning "now" cannot be told from a historic
	// one: the two differ by however long the request took to arrive.
	if params.At != nil {
		problem.Write(w, http.StatusNotImplemented, problem.TypeNotImplemented,
			"Not implemented", historicCountsDetail)
		return
	}

	scope, ok := s.queryScope(w, r)
	if !ok {
		return
	}
	// A filtered scope holding no project reaches no resource at all, so the
	// empty answer is given here rather than asked of the database.
	if !scope.Unfiltered && len(scope.Refs) == 0 {
		writeJSON(w, ResourceStatsList{Items: []ResourceStatsItem{}})
		return
	}

	clouds, projects := scopeFilter(scope)
	rows, err := s.queries.CountCurrentResourcesGrouped(ctx, sqlcgen.CountCurrentResourcesGroupedParams{
		Deleted:       statsStatusFilter(params.Status),
		ScopeClouds:   clouds,
		ScopeProjects: projects,
		RowCap:        maxResourceStatsRows + 1,
	})
	if err != nil {
		writeInternal(ctx, w, "counting resources", err, resourceStatsDetail)
		return
	}
	// The query reads one row past the bound, so a row set of exactly the bound
	// is a whole answer and one longer is the refusal.
	if len(rows) > maxResourceStatsRows {
		refuseLargeFleet(ctx, w, params)
		return
	}

	writeJSON(w, ResourceStatsList{Items: groupResourceCounts(rows, params.GroupBy)})
}

// refuseLargeFleet answers a request whose grouping fills more rows than one
// answer carries, and logs what was asked for: the bound being hit at all is an
// operational signal, and the response says nothing about the request that hit
// it.
func refuseLargeFleet(ctx context.Context, w http.ResponseWriter, params GetResourceStatsParams) {
	Logger(ctx).Warn("refusing a resource grouping above the bound",
		"group_by", params.GroupBy, "status", params.Status, "bound", maxResourceStatsRows)
	problem.Write(w, http.StatusUnprocessableEntity, problem.TypeResultTooLarge,
		"Result too large", largeFleetDetail)
}

// resourceGroup is the combination of values one item stands for, which is what
// the counts are keyed by while they are added up. A dimension the request did
// not group by stays the empty string, so every row of the query projects onto
// the same key and the counts of the rows merge there.
type resourceGroup struct {
	cloud        string
	resourceType string
	state        string
	platform     string
	projectID    string
}

// groupResourceCounts adds the counted rows up onto the dimensions the request
// asked for and renders them as the answer the contract promises.
//
// An optional member is set exactly when the grouping names its dimension: the
// value stands for the resources of one state, platform, or project only when
// the count was taken per state, platform, or project.
//
// The items come out ordered the way the contract states, with a dimension
// outside the grouping compared as the empty string it was keyed by, and the
// slice is never nil: a fleet the grouping counts nothing of is answered as the
// empty array.
func groupResourceCounts(rows []sqlcgen.CountCurrentResourcesGroupedRow,
	groupBy []GetResourceStatsParamsGroupBy,
) []ResourceStatsItem {
	byState := slices.Contains(groupBy, GetResourceStatsParamsGroupByState)
	byPlatform := slices.Contains(groupBy, GetResourceStatsParamsGroupByPlatform)
	byProject := slices.Contains(groupBy, GetResourceStatsParamsGroupByProjectId)

	counts := make(map[resourceGroup]int64, len(rows))
	for _, row := range rows {
		group := resourceGroup{cloud: row.Cloud, resourceType: row.ResourceType}
		if byState {
			group.state = row.State
		}
		if byPlatform {
			group.platform = row.Platform
		}
		if byProject {
			group.projectID = row.ProjectID
		}
		counts[group] += row.Resources
	}

	items := make([]ResourceStatsItem, 0, len(counts))
	for _, group := range slices.SortedFunc(maps.Keys(counts), compareResourceGroups) {
		items = append(items, ResourceStatsItem{
			Cloud:        group.cloud,
			ResourceType: group.resourceType,
			State:        groupedValue(byState, group.state),
			Platform:     groupedValue(byPlatform, group.platform),
			ProjectId:    groupedValue(byProject, group.projectID),
			Count:        counts[group],
		})
	}
	return items
}

// compareResourceGroups orders two groups over their five dimensions, in the
// order the contract states.
func compareResourceGroups(a, b resourceGroup) int {
	return cmp.Or(
		strings.Compare(a.cloud, b.cloud),
		strings.Compare(a.resourceType, b.resourceType),
		strings.Compare(a.state, b.state),
		strings.Compare(a.platform, b.platform),
		strings.Compare(a.projectID, b.projectID),
	)
}

// groupedValue renders one optional dimension of a statistics item: the value
// when the request grouped by it, and nothing otherwise. The two statistics
// routes share it, because both carry the members a grouping names and leave the
// others out rather than sending the empty string.
func groupedValue(grouped bool, value string) *string {
	if !grouped {
		return nil
	}
	return &value
}

// statsStatusFilter maps the status parameter of this route onto the one boolean
// the query splits the fleet with: false counts the rows that live, true the
// deleted ones, and NULL both. It is filterStatus for the second operation that
// declares the parameter, which the generator types apart from the first.
//
// A request naming no status counts the active rows. The contract's default says
// so, and the generated binding does not apply it, so it is applied here.
func statsStatusFilter(status *GetResourceStatsParamsStatus) pgtype.Bool {
	value := GetResourceStatsParamsStatusActive
	if status != nil {
		value = *status
	}
	if value == GetResourceStatsParamsStatusAll {
		return pgtype.Bool{}
	}
	return pgtype.Bool{Bool: value == GetResourceStatsParamsStatusDeleted, Valid: true}
}

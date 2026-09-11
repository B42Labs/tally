package httpui

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/console/reporting"
	"github.com/b42labs/tally/internal/console/store"
	projectcore "github.com/b42labs/tally/internal/core/project"
	"github.com/b42labs/tally/internal/engine/export"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
	"github.com/b42labs/tally/internal/reporting/httpapi"
)

// rollupView is a meta-project's rollup, or why the engine refused it.
type rollupView struct {
	// Note is the engine's refusal, which the page prints in place of the
	// table.
	Note    string
	Periods listing[rollupRow]
}

// rollupRow is one billing period of a meta-project: every member a run that
// stands billed, which the row folds open, and what the group is billed for
// the period once those runs are added up.
type rollupRow struct {
	From    time.Time
	Members []rollupMemberRow
	// Count is how many members the runs billed, each counted once however
	// many of the runs billed it.
	Count int64
	// Totals is what the groups of the runs add up to, per currency, and Sort
	// is the amount the column is ordered by, the way a period of a project
	// adds up.
	Totals   []currencyTotal
	Sort     decimal.Decimal
	Status   string
	Searched string
}

// rollupMemberRow is one member as one run billed it, with the pages of its
// statement and of its project.
type rollupMemberRow struct {
	Run           store.Run
	Member        export.RollupMember
	StatementLink string
	ProjectLink   string
}

// rollupColumns is a meta-project's rollup, one row per period.
var rollupColumns = []column[rollupRow]{
	textCol("period", func(r rollupRow) string { return r.Searched }),
	textCol("status", func(r rollupRow) string { return r.Status }),
	countCol("members", func(r rollupRow) int64 { return r.Count }),
	numberCol("total", func(r rollupRow) decimal.Decimal { return r.Sort }),
}

// refusedRollup is a run the engine would not sum under the meta-project, one
// that billed its members in two currencies for example. The page prints it in
// place of the table rather than failing, because the rest of the page reads
// fine, the way the run page prints an export it refuses.
type refusedRollup struct {
	run uuid.UUID
	err error
}

// Error names the run and what the engine said about it.
func (e refusedRollup) Error() string {
	return "the engine refuses the rollup of run " + e.run.String() + ": " + e.err.Error()
}

// Unwrap exposes the engine's own error.
func (e refusedRollup) Unwrap() error { return e.err }

// lastInstant is the last instant a relation can be valid at inside a billing
// period. The period is half-open and both databases store instants to the
// microsecond, so it is the microsecond before the next period starts.
func lastInstant(billing store.Period) time.Time {
	return billing.To.Add(-time.Microsecond)
}

// buildRollup sums what the members of a meta-project are billed, per billing
// period, oldest first. The sum is the one tally-engine export --rollup
// member_of writes, export.BuildRollup over each run that stands, and a period
// adds its runs up the way a period of a project adds up its statements.
//
// The export reads the membership out of the registry's database: every
// relation whose validity overlaps the period. The console reads the registry
// through the Reporting API, which answers the relations valid at one instant,
// so it asks at the first and at the last instant of the period. A membership
// that began and ended between the two is valid at neither and is not counted.
func (h *handlers) buildRollup(r *http.Request, project httpapi.Project, src *sources) (*rollupView, error) {
	ctx := r.Context()

	periods, err := h.store.ListPeriods(ctx)
	src.query("ListPeriods")
	if err != nil {
		return nil, storeFailed(err)
	}

	group := statements.Key(project.Cloud, project.ExternalId)
	// The projects BuildRollup is handed: the meta-project every relation
	// reaches, and each member once it has been read.
	held := map[uuid.UUID]source.Project{project.Id: {
		ID: project.Id, Platform: project.Platform, Cloud: project.Cloud, ExternalID: project.ExternalId,
	}}

	var rows []rollupRow
	// The periods come newest first, and the table reads them oldest first.
	for _, billing := range slices.Backward(periods) {
		row, rolled, err := h.rollupPeriod(ctx, project.Id, group, billing, held, src)
		var refused refusedRollup
		if errors.As(err, &refused) {
			return &rollupView{Note: refused.Error()}, nil
		}
		if err != nil {
			return nil, err
		}
		if rolled {
			rows = append(rows, row)
		}
	}
	return &rollupView{Periods: tabulate(r, "rollup", rollupColumns, rows)}, nil
}

// rollupPeriod sums one billing period of a meta-project and reports whether a
// run that stands billed a member in it. A period no relation reaches at either
// instant, and one in which no run stands, is read no further.
func (h *handlers) rollupPeriod(
	ctx context.Context, id uuid.UUID, group string, billing store.Period,
	held map[uuid.UUID]source.Project, src *sources,
) (rollupRow, bool, error) {
	relations, err := h.membership(ctx, id, billing, src)
	if err != nil || len(relations) == 0 {
		return rollupRow{}, false, err
	}

	recorded, err := h.store.ListRunsForPeriod(ctx, billing.From)
	src.query("ListRunsForPeriod")
	if err != nil {
		return rollupRow{}, false, storeFailed(err)
	}
	var standing []store.Run
	for _, run := range recorded {
		if slices.Contains(standingStatuses, run.Status) {
			standing = append(standing, run)
		}
	}
	if len(standing) == 0 {
		return rollupRow{}, false, nil
	}

	if err := h.resolveMembers(ctx, relations, held, src); err != nil {
		return rollupRow{}, false, err
	}
	projects := slices.Collect(maps.Values(held))

	row := rollupRow{From: billing.From, Status: absent}
	sums := make(map[string]decimal.Decimal)
	for _, run := range standing {
		loaded, err := h.store.LoadRunExport(ctx, run.ID)
		src.query("LoadRunExport")
		if errors.Is(err, export.ErrRunNotExportable) {
			// The run stopped standing between the two reads, and a run that
			// does not stand counts for nothing.
			continue
		}
		if err != nil {
			return rollupRow{}, false, storeFailed(err)
		}
		if run.Kind == regularKind {
			row.Status = run.Status
		}

		rollup, err := export.BuildRollup(loaded, projectcore.RelationMemberOf, projects, relations)
		if err != nil {
			return rollupRow{}, false, refusedRollup{run: run.ID, err: err}
		}
		for _, summed := range rollup.Groups {
			if summed.Key != group {
				continue
			}
			for _, member := range summed.Members {
				row.Members = append(row.Members, rollupMemberRow{
					Run:           run,
					Member:        member,
					StatementLink: statementLink(run.ID, member.StatementKey),
					ProjectLink:   projectLink(member.Cloud, member.ProjectID),
				})
			}
			sums[summed.Currency] = sums[summed.Currency].Add(summed.Total)
		}
	}
	if len(row.Members) == 0 {
		return rollupRow{}, false, nil
	}
	row.describe(sums)
	return row, true, nil
}

// membership reads the member_of relations that reach a meta-project at the
// first and at the last instant of a billing period, one entry per relation. A
// relation valid at both instants is read twice and kept once.
func (h *handlers) membership(
	ctx context.Context, id uuid.UUID, billing store.Period, src *sources,
) ([]source.Relation, error) {
	byID := make(map[uuid.UUID]source.Relation)
	for _, at := range []time.Time{billing.From, lastInstant(billing)} {
		list, request, err := h.api.ListProjectRelations(ctx, id, reporting.RelationsQuery{
			Direction:    string(httpapi.Incoming),
			RelationType: projectcore.RelationMemberOf,
			At:           at,
		})
		src.api(request)
		if err != nil {
			return nil, apiFailed(err)
		}
		for _, relation := range list.Items {
			byID[relation.Id] = source.Relation{
				ID:           relation.Id,
				SourceID:     relation.SourceId,
				TargetID:     relation.TargetId,
				RelationType: relation.RelationType,
				ValidFrom:    relation.ValidFrom,
				ValidTo:      relation.ValidTo,
			}
		}
	}

	// By id, the order the export reads its relations in: BuildRollup names the
	// currency it met first when it refuses a group, so the order decides what
	// a refusal says.
	relations := slices.Collect(maps.Values(byID))
	slices.SortFunc(relations, func(a, b source.Relation) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	return relations, nil
}

// resolveMembers reads the project every relation leaves, once per request:
// BuildRollup names a member by the cloud and the external id its registry row
// carries, and refuses a relation whose source it was not handed.
func (h *handlers) resolveMembers(
	ctx context.Context, relations []source.Relation, held map[uuid.UUID]source.Project, src *sources,
) error {
	for _, relation := range relations {
		if _, read := held[relation.SourceID]; read {
			continue
		}
		member, request, err := h.api.GetProject(ctx, relation.SourceID)
		src.api(request)
		if err != nil {
			return apiFailed(err)
		}
		held[relation.SourceID] = source.Project{
			ID: member.Id, Platform: member.Platform, Cloud: member.Cloud, ExternalID: member.ExternalId,
		}
	}
	return nil
}

// describe counts the members of a period, adds its groups up per currency,
// and says what a filter matches it on: the period, and the run kind, the cloud
// and the project of every member under it.
func (r *rollupRow) describe(sums map[string]decimal.Decimal) {
	for _, currency := range slices.Sorted(maps.Keys(sums)) {
		r.Totals = append(r.Totals, currencyTotal{Total: sums[currency], Currency: currency})
	}
	if len(r.Totals) > 0 {
		r.Sort = r.Totals[0].Total
	}

	counted := make(map[string]bool, len(r.Members))
	searched := []string{stamp(r.From)}
	for _, member := range r.Members {
		counted[member.Member.StatementKey] = true
		searched = append(searched, member.Run.Kind, member.Member.Cloud, member.Member.ProjectID)
	}
	r.Count = int64(len(counted))
	r.Searched = strings.Join(searched, " ")
}

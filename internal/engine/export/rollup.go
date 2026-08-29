package export

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/core/project"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
)

// The prefix one group's document is named under and the file the table is
// written to. A rollup document is one per group, the way a statement is one
// per project, and the table holds every group of the run.
const (
	rollupPrefix      = "rollup-"
	rollupCSVFileName = "rollup.csv"
)

// rollupHeader is the column order of rollup.csv. The first five columns are
// the ones every table of an export carries, so a row says which run and which
// month it belongs to on its own. relation_type says whether the target is a
// meta-project or a partner, target_cloud and target_project_id name it, and
// cloud and project_id name the member the total on the row was billed to: a
// group's total is the sum of its rows rather than a column of its own.
var rollupHeader = []string{
	"run_id", "kind", "corrects_run_id", "period_from", "period_to",
	"relation_type", "target_cloud", "target_project_id", "cloud", "project_id",
	"total", "currency",
}

// Rollup is what an export sums under the virtual projects of one relation
// type: the meta-projects a run's projects are members of, or the partners that
// manage them.
type Rollup struct {
	// RelationType is project.RelationMemberOf or project.RelationManagedBy.
	RelationType string
	// Groups is never nil and is ordered by Key. A run none of whose statements
	// is related to a virtual project rolls up to no group at all.
	Groups []RollupGroup
}

// RollupGroup is one meta-project or partner and what the run billed under it.
type RollupGroup struct {
	// Key is statements.Key(Cloud, ProjectID) of the target, which is what its
	// document is named after.
	Key string
	// Cloud and ProjectID are the target as the registry holds it. A virtual
	// project carries its platform as its cloud.
	Cloud, ProjectID string
	// Platform is the target's registry platform, meta or partner.
	Platform string
	// Members is ordered by StatementKey and holds at least one entry: a target
	// none of whose members the run billed is no group.
	Members []RollupMember
	// Total is the sum of the member totals, and Currency is the currency every
	// one of them was billed in.
	Total    decimal.Decimal
	Currency string
}

// RollupMember is one project of a group, as the run's statement billed it.
type RollupMember struct {
	// StatementKey is the key the run stored the statement under; Cloud and
	// ProjectID are its two halves as the registry holds them.
	StatementKey     string
	Cloud, ProjectID string
	Total            decimal.Decimal
	Currency         string
}

// rollupMemberKey says which member a group already counts: the group's key and
// the statement key under it.
type rollupMemberKey struct{ group, member string }

// BuildRollup sums a run's statements under the virtual projects the relations
// reach. One group is one target: the meta-project or the partner a relation
// points at, the projects related to it that the run billed, and the sum of what
// those statements carry. Attribution and billing stay per project, so a group's
// total is the sum of the member totals it lists rather than a number of its
// own.
//
// The relations are walked one hop each. A member of a meta-project that is
// itself a member of a second one is counted under the first alone, because a
// rollup reports what is directly related to a target rather than what a walk
// reaches from it. A source is counted once per target however many of the
// given relations reach that target from it, so a membership closed and opened
// again inside the period is one member rather than two. A source related to two
// targets is a member of both groups and both sum it: the groups of a rollup are
// not disjoint, and their totals add up to more than the run billed.
//
// A source the run billed no statement for adds nothing, and a target none of
// whose members the run billed is no group. Such a relation is skipped before
// its target is judged, so one registry row of a cloud this run never metered
// refuses no month it has no part in. A target a billed source reaches whose
// platform is not meta or partner is refused: such a project owns resources and
// carries a statement of its own, which a rollup under it would leave out, and
// the document would then report a total that is neither the group's nor the
// project's.
func BuildRollup(run Run, relationType string, projects []source.Project, relations []source.Relation) (Rollup, error) {
	if !project.IsVirtualRelationType(relationType) {
		return Rollup{}, fmt.Errorf("the rollup relation type %q is not member_of or managed_by", relationType)
	}

	registry := make(map[uuid.UUID]source.Project, len(projects))
	for _, entry := range projects {
		registry[entry.ID] = entry
	}
	billed := make(map[string]statements.Statement, len(run.Statements))
	for _, statement := range run.Statements {
		billed[statement.Key] = statement
	}

	groups := make(map[string]*RollupGroup, len(relations))
	counted := make(map[rollupMemberKey]bool, len(relations))
	for _, relation := range relations {
		// The caller asked for one relation type, and a relation of another one
		// would be summed under a target the type does not reach: a managed_by edge
		// in a member_of rollup puts a partner's projects under a customer's total.
		if relation.RelationType != relationType {
			return Rollup{}, fmt.Errorf("relation %s is of type %s, and the rollup is over %s",
				relation.ID, relation.RelationType, relationType)
		}
		// Both ends have to be among the projects, the way adjustments.New holds
		// them to the same snapshot: a group is named after its target's cloud and
		// external id, and a member after its source's.
		from, held := registry[relation.SourceID]
		if !held {
			return Rollup{}, fmt.Errorf("relation %s names the project %s, which the registry snapshot does not hold",
				relation.ID, relation.SourceID)
		}
		target, held := registry[relation.TargetID]
		if !held {
			return Rollup{}, fmt.Errorf("relation %s names the project %s, which the registry snapshot does not hold",
				relation.ID, relation.TargetID)
		}
		// A relation whose source the run billed nothing for contributes no member,
		// so nothing about its target enters a total: it is skipped before it is
		// judged below, rather than judged and refused over a project the run has
		// no statement for.
		key := statements.Key(from.Cloud, from.ExternalID)
		statement, billedHere := billed[key]
		if !billedHere {
			continue
		}
		if !project.IsVirtualPlatform(target.Platform) {
			return Rollup{}, fmt.Errorf(
				"relation %s reaches %s/%s, whose platform %q is not meta or partner, "+
					"and a rollup sums under a virtual project alone",
				relation.ID, target.Cloud, target.ExternalID, target.Platform)
		}
		targetKey := statements.Key(target.Cloud, target.ExternalID)
		if counted[rollupMemberKey{group: targetKey, member: key}] {
			continue
		}
		counted[rollupMemberKey{group: targetKey, member: key}] = true

		group, opened := groups[targetKey]
		if !opened {
			group = &RollupGroup{
				Key:       targetKey,
				Cloud:     target.Cloud,
				ProjectID: target.ExternalID,
				Platform:  target.Platform,
				Total:     decimal.Zero,
				Currency:  statement.Currency,
			}
			groups[targetKey] = group
		} else if group.Currency != statement.Currency {
			// A sum over two currencies is not a total anybody invoices, the way two
			// currencies under one partner are two entries of a settlement.
			return Rollup{}, fmt.Errorf("the rollup of %s holds statements in %s and in %s",
				group.Key, group.Currency, statement.Currency)
		}
		group.Members = append(group.Members, RollupMember{
			StatementKey: key,
			Cloud:        from.Cloud,
			ProjectID:    from.ExternalID,
			Total:        statement.Total,
			Currency:     statement.Currency,
		})
		// The statement totals are two-place amounts the rating rounded already, so
		// adding them is what keeps the group equal to the lines it lists.
		group.Total = group.Total.Add(statement.Total)
	}

	// Non-nil, so a run that rolls up to nothing carries an empty list rather
	// than a null, the way an export without statements carries one.
	result := make([]RollupGroup, 0, len(groups))
	for _, group := range groups {
		slices.SortFunc(group.Members, func(a, b RollupMember) int {
			return cmp.Compare(a.StatementKey, b.StatementKey)
		})
		result = append(result, *group)
	}
	slices.SortFunc(result, func(a, b RollupGroup) int { return cmp.Compare(a.Key, b.Key) })
	return Rollup{RelationType: relationType, Groups: result}, nil
}

// LoadRollup reads the membership out of the reporting database and sums the
// run under it. The relations are the ones of the given type whose validity
// overlaps the run's period, which is the same overlap query the run resolved
// its adjustments from.
//
// The registry is read when the export runs rather than taken off what the run
// recorded (author's decision of 2026-08-29, named here per guardrail 10 of
// roadmap/00-conventions.md). A rollup is therefore a function of the run and of
// the registry as it stands at that moment, rather than of the run alone: two
// exports of one finalized run differ where a relation was created or closed
// retroactively between them.
func LoadRollup(ctx context.Context, reporting *source.DB, run Run, relationType string) (Rollup, error) {
	rollup, err := loadRollup(ctx, reporting, run, relationType)
	if err != nil {
		return Rollup{}, fmt.Errorf("rolling up run %s over %s: %w", run.ID, relationType, err)
	}
	return rollup, nil
}

// loadRollup is the read LoadRollup wraps: the projects and the relations of one
// reporting snapshot, and the sum over them.
func loadRollup(ctx context.Context, reporting *source.DB, run Run, relationType string) (Rollup, error) {
	snap, err := reporting.Snapshot(ctx)
	if err != nil {
		return Rollup{}, err
	}
	// The close that ends the snapshot, on a context no cancellation reaches, the
	// way load ends its own transaction. The error is dropped because there is
	// nothing a failed rollback of a read-only transaction changes.
	defer func() { _ = snap.Close(context.WithoutCancel(ctx)) }()

	projects, err := snap.Projects(ctx)
	if err != nil {
		return Rollup{}, err
	}
	relations, err := snap.Relations(ctx, []string{relationType}, run.PeriodFrom, run.PeriodTo)
	if err != nil {
		return Rollup{}, err
	}
	return BuildRollup(run, relationType, projects, relations)
}

// RollupFileName is the file one group is written to: the rollup- prefix, the
// escaped key of the target and .json. The key is escaped twice and falls back
// to its digest past the length a file name holds, for the reasons escapedName
// gives.
func RollupFileName(key string) string {
	return escapedName(rollupPrefix, key)
}

// rollupDocument is rollup-<key>.json: which target the group sums under, the
// period and the kind of the run that produced it, and one entry per member
// beside the file that member's invoice was written to. Nothing here names the
// run itself, the way a statement document does not: run.json is the index that
// ties a file to its run. The field order is the order it is marshalled in.
type rollupDocument struct {
	BillingPeriod statements.BillingPeriod `json:"billing_period"`
	ProjectID     string                   `json:"project_id"`
	Platform      string                   `json:"platform"`
	RelationType  string                   `json:"relation_type"`
	Kind          string                   `json:"kind"`
	// CorrectsRunID is the run a correction's rollup corrects. A pointer, so a
	// regular run renders null the way runDocument and kickbacksDocument render
	// the run they correct: one export's documents say the same thing about an
	// absent value.
	CorrectsRunID *string             `json:"corrects_run_id"`
	Members       []rollupMemberEntry `json:"members"`
	Total         money.Amount        `json:"total"`
	Currency      string              `json:"currency"`
}

// rollupMemberEntry is one member under a group: the file its statement was
// written to, the pair it bills, and the total that statement carries. The two
// halves of the key are unescaped, the way the index carries them.
type rollupMemberEntry struct {
	File      string       `json:"file"`
	Cloud     string       `json:"cloud"`
	ProjectID string       `json:"project_id"`
	Total     money.Amount `json:"total"`
	Currency  string       `json:"currency"`
}

// renderRollup renders one group of a rollup. files is the name every statement
// of the run was written under, keyed by its statement key: a member names the
// file its invoice is in rather than the name its key renders to, so a document
// that had to give its name up to a case-fold collision is still the one the
// group points at. A member no name was recorded for is refused: an empty
// pointer beside a total that still balances is worse than no document at all,
// because an ERP walking members[].file finds nothing and the group reads as
// authoritative all the same.
func renderRollup(run Run, rollup Rollup, group RollupGroup, files map[string]string) ([]byte, error) {
	document := rollupDocument{
		BillingPeriod: statements.BillingPeriod{
			From: instant(run.PeriodFrom),
			To:   instant(run.PeriodTo),
		},
		ProjectID:    group.ProjectID,
		Platform:     group.Platform,
		RelationType: rollup.RelationType,
		Kind:         run.Kind,
		Members:      make([]rollupMemberEntry, 0, len(group.Members)),
		Total:        money.NewAmount(group.Total),
		Currency:     group.Currency,
	}
	if run.CorrectsRunID != uuid.Nil {
		corrects := run.CorrectsRunID.String()
		document.CorrectsRunID = &corrects
	}
	for _, member := range group.Members {
		file, wrote := files[member.StatementKey]
		if !wrote {
			return nil, fmt.Errorf("the rollup of %s names the member %s, which run %s wrote no statement for",
				group.Key, member.StatementKey, run.ID)
		}
		document.Members = append(document.Members, rollupMemberEntry{
			File:      file,
			Cloud:     member.Cloud,
			ProjectID: member.ProjectID,
			Total:     money.NewAmount(member.Total),
			Currency:  member.Currency,
		})
	}

	body, err := marshal(document)
	if err != nil {
		return nil, fmt.Errorf("rendering %s of run %s: %w", RollupFileName(group.Key), run.ID, err)
	}
	return body, nil
}

// RollupCSV renders rollup.csv: the header and one row per member of every
// group, in the order the rollup holds them. Every row carries the run, its kind
// and its period beside the target it is summed under, the way a row of
// kickbacks.csv carries them, so a row says which run, which month and which
// meta-project it belongs to on its own.
//
// An export without a rollup and a rollup with no groups both render the header
// alone: an empty table says the run rolled nothing up, and a missing file says
// nothing at all.
func RollupCSV(run Run) ([]byte, error) {
	rows := [][]string{rollupHeader}
	if run.Rollup != nil {
		corrects := correctsOf(run)
		from, to := instant(run.PeriodFrom), instant(run.PeriodTo)

		for _, group := range run.Rollup.Groups {
			for _, member := range group.Members {
				rows = append(rows, []string{
					run.ID.String(), run.Kind, corrects, from, to,
					cell(run.Rollup.RelationType), cell(group.Cloud), cell(group.ProjectID),
					cell(member.Cloud), cell(member.ProjectID),
					// The total does not go through cell: a leading minus is the sign of
					// what a correction credits rather than a formula.
					member.Total.StringFixed(money.AmountPlaces),
					member.Currency,
				})
			}
		}
	}
	return table(run, rollupCSVFileName, rows)
}

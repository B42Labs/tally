// Package projects holds the domain logic of the project registry graph: the
// point-in-time traversal that answers what a project is related to, and the
// cycle guard a new cost-attributing relation has to pass. Both are walks over
// project_relations rather than reads of one row, so they live beside the
// queries instead of in the handlers. Phase 3 attribution walks the same graph
// and reuses this package.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.9.
package projects

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// ErrCycle is what GuardCycle returns for a relation that would close a cycle
// over the attributing relation types. Attribution is a forest, so a cycle in
// it would bill the same cost twice.
var ErrCycle = errors.New("the relation would close a cycle over the attributing relation types")

// Related is one project a walk reached. RelationType is the type of the edge
// that reached it, Depth the number of edges between it and the project the
// walk started from, and Path the relation ids of those edges in walk order, so
// len(Path) == Depth.
type Related struct {
	ProjectID    uuid.UUID
	RelationType string
	Depth        int
	Path         []uuid.UUID
}

// step is one project of the current level together with the relations that led
// to it. The path travels with the frontier, so it is dropped once the level it
// belongs to is walked.
type step struct {
	project uuid.UUID
	path    []uuid.UUID
}

// Traverse walks the outgoing relations of start that are valid at at, up to
// depth edges out, and returns every project it reaches. A relationType that is
// not nil narrows the walk to relations of that type.
//
// The walk is breadth-first and one query per level, so a depth of ten costs
// ten queries whatever the graph looks like. First visit wins: a project the
// walk has already reached is never reached again, which terminates a cycle and
// keeps start itself out of the result. The projects come back in the order
// they were visited, which is the order of the frontier over the order of the
// query, and nothing comes back for a depth below one or for a project with no
// outgoing relation valid at at.
//
// Bounding depth is the caller's business: the API caps it at ten.
func Traverse(ctx context.Context, q *sqlcgen.Queries, start uuid.UUID, depth int,
	relationType *string, at time.Time,
) ([]Related, error) {
	visited := map[uuid.UUID]bool{start: true}
	frontier := []step{{project: start}}

	var related []Related
	for level := 1; level <= depth && len(frontier) > 0; level++ {
		rows, err := q.ListRelationsValidAt(ctx, sqlcgen.ListRelationsValidAtParams{
			SourceIds:    projectIDs(frontier),
			At:           pgtype.Timestamptz{Time: at, Valid: true},
			RelationType: relationTypeFilter(relationType),
		})
		if err != nil {
			return nil, fmt.Errorf("listing the relations at depth %d: %w", level, err)
		}

		// The query sorts by source id, the frontier holds the sources in the
		// order they were visited: walking the frontier over the grouped rows is
		// what makes the result depend on the graph rather than on how the ids
		// happen to sort.
		outgoing := bySource(rows)
		var next []step
		for _, from := range frontier {
			for _, row := range outgoing[from.project] {
				if visited[row.TargetID] {
					continue
				}
				visited[row.TargetID] = true

				path := make([]uuid.UUID, 0, len(from.path)+1)
				path = append(append(path, from.path...), row.ID)
				related = append(related, Related{
					ProjectID:    row.TargetID,
					RelationType: row.RelationType,
					Depth:        level,
					Path:         path,
				})
				next = append(next, step{project: row.TargetID, path: path})
			}
		}
		frontier = next
	}
	return related, nil
}

// GuardCycle reports whether a relation from source to target would close a
// cycle over attributingTypes. It walks the active relations of those types out
// of target and returns ErrCycle as soon as source is among them, an edge
// straight back to source included, and nil once the walk runs out of projects.
//
// Only active relations count: one that was closed carries no cost any more. An
// empty attributingTypes finds no relation at all, so nothing is a cycle. The
// walk needs no depth cap, because a project is only ever visited once.
func GuardCycle(ctx context.Context, q *sqlcgen.Queries, source, target uuid.UUID,
	attributingTypes []string,
) error {
	visited := map[uuid.UUID]bool{target: true}
	frontier := []uuid.UUID{target}

	for len(frontier) > 0 {
		rows, err := q.ListActiveAttributingRelations(ctx, sqlcgen.ListActiveAttributingRelationsParams{
			SourceIds:     frontier,
			RelationTypes: attributingTypes,
		})
		if err != nil {
			return fmt.Errorf("listing the active attributing relations: %w", err)
		}

		var next []uuid.UUID
		for _, row := range rows {
			if row.TargetID == source {
				return ErrCycle
			}
			if visited[row.TargetID] {
				continue
			}
			visited[row.TargetID] = true
			next = append(next, row.TargetID)
		}
		frontier = next
	}
	return nil
}

// projectIDs is the projects of one frontier, as the queries take them.
func projectIDs(frontier []step) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(frontier))
	for _, from := range frontier {
		ids = append(ids, from.project)
	}
	return ids
}

// bySource groups the relations of one level under the project they leave,
// keeping the order the query returned them in.
func bySource(rows []sqlcgen.ProjectRelation) map[uuid.UUID][]sqlcgen.ProjectRelation {
	grouped := make(map[uuid.UUID][]sqlcgen.ProjectRelation)
	for _, row := range rows {
		grouped[row.SourceID] = append(grouped[row.SourceID], row)
	}
	return grouped
}

// relationTypeFilter maps the optional type filter onto its query parameter. A
// walk that names no type passes NULL, which the query reads as every type.
func relationTypeFilter(relationType *string) pgtype.Text {
	if relationType == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *relationType, Valid: true}
}

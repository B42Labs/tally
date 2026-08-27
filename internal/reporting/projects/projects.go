// Package projects holds the domain logic of the project registry: the
// registration every path that writes a projects row goes through, the
// point-in-time traversal that answers what a project is related to, and the
// cycle guard a new cost-attributing relation has to pass. The two walks are
// over project_relations rather than reads of one row, and the registration is
// one rule the HTTP handler and the admin CLI share, so all three live beside
// the queries instead of in the handlers. Phase 3 attribution walks the same
// graph and reuses this package.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.9. The cloud a virtual project carries is decision D1 of
// roadmap/05-phase-5-commercial-pricing.md.
package projects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/core/project"
	"github.com/b42labs/tally/internal/reporting/audit"
	"github.com/b42labs/tally/internal/reporting/store/sqlcgen"
)

// The audit trail a registration leaves, whichever path wrote it.
const (
	AuditObject  = "projects"
	ActionCreate = AuditObject + ".create"
)

// The unique key of the registry, as Postgres reports it when a registration
// collides with it. Matching the constraint name and not the code alone is what
// keeps another unique violation from being answered as a duplicate
// registration.
const (
	uniqueViolation      = "23505"
	projectKeyConstraint = "projects_cloud_external_id_key"
)

// ErrVirtualKey refuses a registration that breaks decision D1.
var ErrVirtualKey = errors.New("a project of a virtual platform carries its platform as its cloud, and no other project carries a virtual platform as its cloud")

// ErrAlreadyRegistered refuses a (cloud, external_id) pair the registry holds.
var ErrAlreadyRegistered = errors.New("a project with this cloud and external id is already registered")

// Registration is what a caller registers.
type Registration struct {
	Platform, Cloud, ExternalID string
	Name                        pgtype.Text
	Metadata                    json.RawMessage // the stored document; empty registers {}
}

// Register writes the projects row and the audit row naming it through q, which
// is the handle of the caller's transaction: a registration the log does not
// account for never reaches the database. A pair that breaks decision D1 is
// refused before the database is touched, so a virtual project keeps carrying
// its platform as its cloud.
//
// Platform, Cloud and ExternalID are not checked for emptiness. The contract's
// minLength: 1 on the three members of CreateProject covers the HTTP path, and
// the admin CLI refuses an empty external id and sets the other two from a
// constant.
func Register(ctx context.Context, q *sqlcgen.Queries, actor string, r Registration) (sqlcgen.Project, error) {
	if (project.IsVirtualPlatform(r.Platform) || project.IsVirtualPlatform(r.Cloud)) &&
		r.Platform != r.Cloud {
		return sqlcgen.Project{}, ErrVirtualKey
	}

	metadata := r.Metadata
	if len(metadata) == 0 {
		// The column is NOT NULL and the contract answers metadata as an object,
		// so a registration that carries none registers the empty one.
		metadata = []byte("{}")
	}

	stored, err := q.InsertProject(ctx, sqlcgen.InsertProjectParams{
		Platform:   r.Platform,
		Cloud:      r.Cloud,
		ExternalID: r.ExternalID,
		Name:       r.Name,
		Metadata:   metadata,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation &&
			pgErr.ConstraintName == projectKeyConstraint {
			return sqlcgen.Project{}, fmt.Errorf("%w: %w", ErrAlreadyRegistered, err)
		}
		return sqlcgen.Project{}, fmt.Errorf("registering (%s, %s): %w", r.Cloud, r.ExternalID, err)
	}

	if err := audit.Log(ctx, q, audit.Entry{
		Actor:      actor,
		Action:     ActionCreate,
		ObjectType: AuditObject,
		ObjectID:   stored.ID.String(),
	}); err != nil {
		return sqlcgen.Project{}, err
	}
	return stored, nil
}

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

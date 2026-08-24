// Package attribution resolves who pays for whose usage: one breadth-first walk
// over the attributing relations of a billing period, started from every
// project no relation attributes away. Resolve is a pure function over the
// project registry a caller already read. It reads nothing and writes nothing
// itself.
//
// Attribution is exclusive. A project an attributing relation names is billed
// under its attributor and nowhere else, so no cost is counted twice and none
// is dropped. Chains flatten onto the project they start at: in A → B → C both
// B and C bill under A, and C carries the relation type of the B → C edge it
// was claimed over, because that edge is what its costs travel to A along.
//
// The walk is deterministic, because the same graph and the same period have to
// yield the same invoice however often they are billed. The shortest path
// claims a project first, and among paths of equal length the smallest relation
// id does, which the walk gets from taking the relations in the id order they
// are loaded in. Every further edge into a project already claimed is a
// WarningMultiplePaths, so the path not taken is visible to an operator rather
// than silently discarded.
//
// A project that is attributed away but that no top-level project reaches sits
// in a cycle. That does not fail the run: the project is reported as orphaned
// with a WarningCycle and billed standalone, because a corrupt graph should
// cost one wrongly rooted statement rather than a whole period's billing. The
// registry refuses to create a cycle (Phase 1, WP 1.9), and the walk does not
// rely on that alone. Its termination is structural, not a guard: a project is
// claimed at most once, so a level that claims nothing empties the frontier.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.7.
package attribution

import (
	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/engine/source"
)

// WarningMultiplePaths marks a project more than one attributing path reaches.
// It names the losing relation: the walk claimed the project over a shorter
// path, or over a path of the same length whose relation id is smaller.
const WarningMultiplePaths = "attribution_multiple_paths"

// WarningCycle marks a project that is attributed away but that no top-level
// project reaches, which only a cycle among attributing relations can produce.
// The project is billed standalone.
const WarningCycle = "attribution_cycle"

// Resolution is who the period bills what under.
type Resolution struct {
	// TopLevel holds the projects that get a statement of their own, in the
	// order the projects came in.
	TopLevel []uuid.UUID
	// Attributed maps every project billed under another one to the attribution
	// it was claimed by. Its keys and TopLevel are disjoint.
	Attributed map[uuid.UUID]Attribution
	// Warnings is what the resolution reports to an operator through the run's
	// stats, in the order it was found. It does not fail the run.
	Warnings []Warning
}

// Attribution is where one project's costs are billed.
type Attribution struct {
	// Root is the top-level project the costs appear on the statement of, which
	// is where the walk started rather than the project one edge back.
	Root uuid.UUID
	// RelationType is the type of the winning edge, the one that claimed the
	// project. A statement shows it beside the related costs it introduces.
	RelationType string
}

// Warning is one finding of the walk. It is JSON-tagged because the run writes
// the warnings into runs.stats verbatim.
type Warning struct {
	// Code is WarningMultiplePaths or WarningCycle.
	Code string `json:"code"`
	// ProjectID is the project the finding is about: the one claimed twice, or
	// the one sitting in the cycle.
	ProjectID uuid.UUID `json:"project_id"`
	// RelationID is the losing relation as text, absent on a cycle warning,
	// which is about a project rather than about any one of its edges.
	RelationID string `json:"relation_id,omitempty"`
}

// Resolve walks the project graph and returns what the period bills where.
// Every relation is a directed edge from the attributor to the project it
// attributes away, and relations are expected in ascending id order, the order
// source.Snapshot.Relations returns them in, because that order is what breaks
// ties between paths of equal length.
//
// Relations are taken as given: filtering them by attributing type and by
// overlap with the period happened when they were loaded (D4). Without
// relations every project is top level. Without projects there is nothing to
// bill and the zero Resolution comes back.
func Resolve(projects []source.Project, relations []source.Relation) Resolution {
	if len(projects) == 0 {
		return Resolution{}
	}

	// A project any edge targets is billed under its attributor; every other
	// one is a root the walk starts from.
	attributedAway := make(map[uuid.UUID]bool, len(relations))
	for _, relation := range relations {
		attributedAway[relation.TargetID] = true
	}

	resolution := Resolution{Attributed: make(map[uuid.UUID]Attribution)}
	frontier := make(map[uuid.UUID]bool, len(projects))
	for _, project := range projects {
		if !attributedAway[project.ID] {
			resolution.TopLevel = append(resolution.TopLevel, project.ID)
			frontier[project.ID] = true
		}
	}

	// One breadth-first search from the whole top-level set at once, rather than
	// one per root: a project every level of it reaches is claimed at the level
	// it is first reached from, which is the shortest path any root has to it.
	// The frontier is only ever tested for membership, never iterated, so the
	// order the edges of a level are walked in is the order of the relations
	// slice and nothing else.
	for len(frontier) > 0 {
		claimed := make(map[uuid.UUID]bool)
		for _, relation := range relations {
			if !frontier[relation.SourceID] {
				continue
			}
			if _, taken := resolution.Attributed[relation.TargetID]; taken {
				resolution.Warnings = append(resolution.Warnings, Warning{
					Code:       WarningMultiplePaths,
					ProjectID:  relation.TargetID,
					RelationID: relation.ID.String(),
				})
				continue
			}
			resolution.Attributed[relation.TargetID] = Attribution{
				Root:         rootOf(resolution.Attributed, relation.SourceID),
				RelationType: relation.RelationType,
			}
			claimed[relation.TargetID] = true
		}
		// What this level claimed is what the next level walks out of, which is
		// what flattens a chain onto its root.
		frontier = claimed
	}

	// A project that is attributed away and that the walk never claimed cannot
	// be reached from any root, so its attributors reach it only through each
	// other. A self-loop is that case too and needs no rule of its own.
	for _, project := range projects {
		if !attributedAway[project.ID] {
			continue
		}
		if _, taken := resolution.Attributed[project.ID]; taken {
			continue
		}
		resolution.Warnings = append(resolution.Warnings, Warning{
			Code:      WarningCycle,
			ProjectID: project.ID,
		})
	}
	return resolution
}

// rootOf is the top-level project a claim made by id bills under: the root id
// was itself claimed for, or id when nothing claimed it, which is what a
// top-level project is.
func rootOf(attributed map[uuid.UUID]Attribution, id uuid.UUID) uuid.UUID {
	if attribution, taken := attributed[id]; taken {
		return attribution.Root
	}
	return id
}

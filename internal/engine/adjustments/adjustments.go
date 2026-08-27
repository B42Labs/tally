// Package adjustments resolves the pricing adjustments a project's relations
// carry and applies them to the rated amounts of its statement. New parses the
// graph of one run, Adjust applies it to one statement. Both are pure: the
// package does no I/O.
//
// The resolution is a breadth-first walk from the statement's project over the
// outgoing adjustment-carrying relations, bounded by the depth the caller
// configures (decision D6). Every relation is visited once, so a relation two
// paths reach adjusts once, and a cycle among the relations ends on the visited
// set. The relations are taken as given: the caller loads the ones whose
// validity overlaps the period, and each of them applies to the whole period
// (decision D4).
//
// The collected adjustments apply in the order surcharge, discount, project
// discount, kickback (decision D3), ties broken by relation id and then by the
// position of the element in its relation's array. A surcharge is computed on
// the base, so two surcharges add rather than compound. A discount and a
// project discount are computed on the running net, so they stack
// multiplicatively. A kickback is computed on the running net and leaves it
// alone: it is what a partner is owed rather than what the customer pays.
//
// The "running net per scope partition" of the roadmap's pseudocode is read
// here as one running amount per (platform, resource type) bucket of the bases.
// An adjustment covers the buckets its scope matches (decision D5, through
// adjustment.ScopeMatches), and its line is rounded once, by money.Round2
// (decision D7). That rounded amount is then apportioned back over the buckets
// it covers, every bucket but the last taking its own rounded share and the
// last taking the remainder, so the shares sum to the line exactly and every
// bucket stays a two-place amount.
//
// A kickback whose relation target is not a partner is skipped, and New reports
// it as WarningKickbackTargetNotPartner. A commission owed to a project that is
// not a partner entity is a mistake in the registry rather than an intent to
// bill, and the run says so instead of paying it.
//
// The normative specification is roadmap/05-phase-5-commercial-pricing.md,
// WP 5.3.
package adjustments

import (
	"bytes"
	"cmp"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/adjustment"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/core/project"
	"github.com/b42labs/tally/internal/engine/source"
)

// WarningKickbackTargetNotPartner marks a relation whose kickbacks were not
// applied because its target is not a partner.
const WarningKickbackTargetNotPartner = "adjustment_kickback_target_not_partner"

// Warning is one finding of the resolution. It is JSON-tagged because the run
// writes the warnings into runs.stats verbatim.
type Warning struct {
	// Code is WarningKickbackTargetNotPartner.
	Code string `json:"code"`
	// RelationID is the relation whose kickbacks were dropped, as text.
	RelationID string `json:"relation_id"`
	// TargetPlatform is the platform of that relation's target, which is what
	// the kickbacks were dropped over.
	TargetPlatform string `json:"target_platform"`
	// TargetID is the external id of that relation's target.
	TargetID string `json:"target_id"`
}

// Base is one rated line of a statement, own or related: what the scoped sums
// are built from.
type Base struct {
	// Platform and ResourceType are what an adjustment's scope is matched
	// against.
	Platform, ResourceType string
	// Amount is what the line was rated at.
	Amount decimal.Decimal
}

// Line is one applied adjustment as the document renders it and a record
// stores it. The field order is the order it is marshalled in.
type Line struct {
	Type         string `json:"type"`
	RelationType string `json:"relation_type"`
	// RelationTarget is the external id of the relation's target, which is the
	// beneficiary of a kickback.
	RelationTarget string     `json:"relation_target"`
	RelationID     string     `json:"relation_id"`
	Scope          string     `json:"scope"`
	Description    string     `json:"description,omitempty"`
	Rate           money.Rate `json:"rate"`
	// Base is the amount the rate was applied to.
	Base money.Amount `json:"base"`
	// Amount is signed: a discount is negative.
	Amount money.Amount `json:"amount"`
}

// Outcome is what the adjustments of one statement come to.
type Outcome struct {
	// BaseCost is the sum of the bases, the total Phase 3 rated.
	BaseCost decimal.Decimal
	// Lines holds every collected adjustment in application order, a zero one
	// included, so the document shows everything that reached the statement. It
	// is nil when the walk collected nothing.
	Lines []Line
	// NetCost is BaseCost plus every non-kickback amount: what the customer
	// pays.
	NetCost decimal.Decimal
	// KickbackTotal is the sum of the kickback amounts, which NetCost does not
	// hold.
	KickbackTotal decimal.Decimal
}

// Adjuster holds the parsed adjustment graph of one run.
type Adjuster struct {
	edges []edge
	// bySource indexes the edges by the project they leave, so a walk reads the
	// relations of its frontier rather than every relation of the run. Without
	// it a statement costs one pass over the whole graph per level, and a run
	// bills as many statements as the registry holds projects.
	bySource map[uuid.UUID][]int
	depth    int
}

// edge is one relation with what it adjusts. The target travels along because a
// kickback names it as the beneficiary and because its platform decides whether
// a kickback is paid at all.
type edge struct {
	relation    source.Relation
	target      source.Project
	adjustments []adjustment.Adjustment
	// err is what the relation's stored pricing_adjustments failed to be read
	// with. It is kept here rather than returned by New so that only a walk
	// which reaches the relation fails: see the comment on New.
	err error
}

// applied is one collected adjustment with where it came from: the relation and
// its target for the line, and the position in the relation's array to order
// two adjustments of one relation and one type.
type applied struct {
	adjustment adjustment.Adjustment
	relation   source.Relation
	target     source.Project
	index      int
}

// bucket is one (platform, resource type) partition of the bases: what a scope
// is matched against and what a running net is kept per.
type bucket struct {
	platform     string
	resourceType string
}

// New parses the adjustments of every relation once per run, rather than once
// per statement. The relations are expected in ascending id order, the order
// source.Snapshot.Relations returns them in, and the edges keep it.
//
// A depth below 1 is an error: a walk that does not take its first level reads
// no relation at all, and asking for that is a mistake rather than a way to
// turn adjustments off (an empty relation type list is that).
//
// A relation whose pricing_adjustments member the schema refuses is refused as
// well, because a relation whose stored array cannot be read must not bill as
// though it carried nothing. It is refused by the Adjust of the first statement
// whose walk reaches that relation rather than here: project_relations.metadata
// is a free-form document one tenant writes, and a stale one of a project this
// run bills nothing from would otherwise fail every run and every tick of the
// whole deployment. Metadata without the member is an edge that carries
// nothing.
//
// Both ends of every relation have to be among the projects, which the
// registry's foreign keys and the snapshot already hold; the check is here
// because a line names its target's external id.
//
// The kickbacks of a relation whose target is not a partner are dropped, one
// warning per relation, and the other adjustments of that relation stay. The
// warnings come back in the order the relations were given.
func New(relations []source.Relation, projects []source.Project, depth int) (*Adjuster, []Warning, error) {
	if depth < 1 {
		return nil, nil, fmt.Errorf("the adjustment walk depth is %d, and has to be at least 1", depth)
	}

	registry := make(map[uuid.UUID]source.Project, len(projects))
	for _, entry := range projects {
		registry[entry.ID] = entry
	}

	var warnings []Warning
	edges := make([]edge, 0, len(relations))
	for _, relation := range relations {
		from, held := registry[relation.SourceID]
		if !held {
			return nil, nil, fmt.Errorf(
				"relation %s names the project %s, which the registry snapshot does not hold",
				relation.ID, relation.SourceID)
		}
		target, held := registry[relation.TargetID]
		if !held {
			return nil, nil, fmt.Errorf(
				"relation %s names the project %s, which the registry snapshot does not hold",
				relation.ID, relation.TargetID)
		}

		parsed, _, err := adjustment.FromMetadata(relation.Metadata)
		if err != nil {
			err = fmt.Errorf("the pricing adjustments of relation %s (%s %s/%s -> %s/%s): %w",
				relation.ID, relation.RelationType,
				from.Cloud, from.ExternalID, target.Cloud, target.ExternalID, err)
			edges = append(edges, edge{relation: relation, target: target, err: err})
			continue
		}
		if target.Platform != project.PlatformPartner {
			var dropped bool
			parsed, dropped = withoutKickbacks(parsed)
			if dropped {
				warnings = append(warnings, Warning{
					Code:           WarningKickbackTargetNotPartner,
					RelationID:     relation.ID.String(),
					TargetPlatform: target.Platform,
					TargetID:       target.ExternalID,
				})
			}
		}
		edges = append(edges, edge{relation: relation, target: target, adjustments: parsed})
	}

	bySource := make(map[uuid.UUID][]int, len(edges))
	for i, e := range edges {
		bySource[e.relation.SourceID] = append(bySource[e.relation.SourceID], i)
	}
	return &Adjuster{edges: edges, bySource: bySource, depth: depth}, warnings, nil
}

// Adjust applies to one project's rated amounts what the walk from it collects.
// A project no relation names, or a project whose relations carry nothing,
// comes back as its base cost with no lines. The one error is a relation the
// walk reaches whose stored adjustments could not be read, which New kept for
// this call: such a statement is not billed at all rather than billed as though
// the relation carried nothing.
//
// The bases are summed per bucket, and an adjustment applies to the buckets its
// scope covers. Bases of more than one platform are what an attributed
// project's related costs bring in, and they are adjusted along with the
// project's own.
func (a *Adjuster) Adjust(project uuid.UUID, bases []Base) (Outcome, error) {
	walked, err := a.collect(project)
	if err != nil {
		return Outcome{}, err
	}

	buckets, base := sumBases(bases)
	running := make(map[bucket]decimal.Decimal, len(buckets))

	outcome := Outcome{BaseCost: decimal.Zero, KickbackTotal: decimal.Zero}
	for _, b := range buckets {
		running[b] = base[b]
		outcome.BaseCost = outcome.BaseCost.Add(base[b])
	}
	outcome.NetCost = outcome.BaseCost

	for _, collected := range walked {
		covered := matching(buckets, collected.adjustment.Scope)
		negative := collected.adjustment.Type == adjustment.TypeDiscount ||
			collected.adjustment.Type == adjustment.TypeProjectDiscount

		// A surcharge is rated on the base, everything else on the running net
		// of the buckets its scope covers.
		from := running
		if collected.adjustment.Type == adjustment.TypeSurcharge {
			from = base
		}
		lineBase := sum(from, covered)
		amount := money.Round2(lineBase.Mul(collected.adjustment.Rate))
		if negative {
			amount = amount.Neg()
		}
		outcome.Lines = append(outcome.Lines, collected.line(lineBase, amount))

		if collected.adjustment.Type == adjustment.TypeKickback {
			outcome.KickbackTotal = outcome.KickbackTotal.Add(amount)
			continue
		}
		outcome.NetCost = outcome.NetCost.Add(amount)

		// The line's own amount is apportioned over the buckets it covers, the
		// last of them taking what the rounded shares of the others left, so the
		// shares sum to the line and every bucket keeps two places.
		rest := amount
		for i, b := range covered {
			part := rest
			if i < len(covered)-1 {
				part = money.Round2(from[b].Mul(collected.adjustment.Rate))
				if negative {
					part = part.Neg()
				}
				rest = rest.Sub(part)
			}
			running[b] = running[b].Add(part)
		}
	}
	return outcome, nil
}

// collect walks the graph from one project and returns what it finds, in the
// order it is applied in. A relation is visited at most once however many paths
// reach it (decision D6), which is what ends a cycle as well: a level that
// visits nothing empties the frontier. A relation the walk reaches whose stored
// adjustments New could not read fails the whole call, so nothing is billed
// short of the adjustments that relation carries.
//
// The frontier is read through bySource, so the walk costs the relations of the
// projects it reaches rather than every relation of the run. The visited set
// already made the collected set independent of the order the edges are taken
// in, and the sort below fixes the order the adjustments apply in.
func (a *Adjuster) collect(start uuid.UUID) ([]applied, error) {
	frontier := map[uuid.UUID]bool{start: true}
	visited := make(map[uuid.UUID]bool)

	// The lowest-numbered edge the walk reached that carries a failure, so the
	// error a run reports is the same one however the frontier was walked: the
	// edges are in relation id order.
	unreadable := -1
	var collected []applied
	for level := 1; level <= a.depth && len(frontier) > 0; level++ {
		next := make(map[uuid.UUID]bool)
		for from := range frontier {
			for _, i := range a.bySource[from] {
				e := a.edges[i]
				if visited[e.relation.ID] {
					continue
				}
				visited[e.relation.ID] = true
				next[e.relation.TargetID] = true
				if e.err != nil {
					if unreadable < 0 || i < unreadable {
						unreadable = i
					}
					continue
				}
				for index, parsed := range e.adjustments {
					collected = append(collected, applied{
						adjustment: parsed,
						relation:   e.relation,
						target:     e.target,
						index:      index,
					})
				}
			}
		}
		frontier = next
	}
	if unreadable >= 0 {
		return nil, a.edges[unreadable].err
	}

	slices.SortFunc(collected, func(x, y applied) int {
		if order := cmp.Compare(typeRank(x.adjustment.Type), typeRank(y.adjustment.Type)); order != 0 {
			return order
		}
		// The ids compare as their 16 bytes, which is the order the relations
		// are loaded in (decision D4).
		if order := bytes.Compare(x.relation.ID[:], y.relation.ID[:]); order != 0 {
			return order
		}
		return cmp.Compare(x.index, y.index)
	})
	return collected, nil
}

// line renders one collected adjustment against the base it was rated on and
// the amount it came to.
func (c applied) line(base, amount decimal.Decimal) Line {
	return Line{
		Type:           c.adjustment.Type,
		RelationType:   c.relation.RelationType,
		RelationTarget: c.target.ExternalID,
		RelationID:     c.relation.ID.String(),
		Scope:          c.adjustment.Scope,
		Description:    c.adjustment.Description,
		Rate:           money.NewRate(c.adjustment.Rate),
		Base:           money.NewAmount(base),
		Amount:         money.NewAmount(amount),
	}
}

// withoutKickbacks drops the kickbacks of one relation and reports whether it
// carried any. The clone leaves the caller's slice as it was.
func withoutKickbacks(adjustments []adjustment.Adjustment) ([]adjustment.Adjustment, bool) {
	kept := slices.DeleteFunc(slices.Clone(adjustments), func(parsed adjustment.Adjustment) bool {
		return parsed.Type == adjustment.TypeKickback
	})
	return kept, len(kept) != len(adjustments)
}

// sumBases sums the bases per bucket and returns the buckets in the order the
// apportioning walks them: by platform, then by resource type.
func sumBases(bases []Base) ([]bucket, map[bucket]decimal.Decimal) {
	sums := make(map[bucket]decimal.Decimal, len(bases))
	buckets := make([]bucket, 0, len(bases))
	for _, b := range bases {
		key := bucket{platform: b.Platform, resourceType: b.ResourceType}
		if _, held := sums[key]; !held {
			buckets = append(buckets, key)
			sums[key] = decimal.Zero
		}
		sums[key] = sums[key].Add(b.Amount)
	}

	slices.SortFunc(buckets, func(x, y bucket) int {
		if order := cmp.Compare(x.platform, y.platform); order != 0 {
			return order
		}
		return cmp.Compare(x.resourceType, y.resourceType)
	})
	return buckets, sums
}

// matching is the buckets one scope covers, in bucket order. It is empty for a
// scope no rated amount of the statement falls under, and such an adjustment
// touches nothing.
func matching(buckets []bucket, scope string) []bucket {
	covered := make([]bucket, 0, len(buckets))
	for _, b := range buckets {
		if adjustment.ScopeMatches(scope, b.platform, b.resourceType) {
			covered = append(covered, b)
		}
	}
	return covered
}

// sum adds up the amounts of the given buckets.
func sum(amounts map[bucket]decimal.Decimal, buckets []bucket) decimal.Decimal {
	total := decimal.Zero
	for _, b := range buckets {
		total = total.Add(amounts[b])
	}
	return total
}

// typeRank is the position of a type in the application order of decision D3.
// Every type the schema admits is in it.
func typeRank(adjustmentType string) int {
	return slices.Index(adjustment.TypeOrder, adjustmentType)
}

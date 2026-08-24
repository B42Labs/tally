// Package corrections is the arithmetic and the rendering of a correction run:
// the amounts one metering and rating pass produced per key, the differences
// between two such passes, and the credit note every affected project is handed
// for them.
//
// The package is pure. It reads nothing and writes nothing, the way rating and
// statements read and write nothing, so a period is diffed from the results a
// caller already holds. The key the amounts are diffed by is decision D6's: the
// cloud, platform, resource type and resource id of the resource, the project
// that owned it, and the dimension it was billed on. The grouping of the deltas
// into documents is the one statements.Build applies to rated records, over
// deltas instead: a project's own resources are its line items, and every
// project attributed to it arrives as a related cost beside them.
//
// The orchestration is elsewhere. The run row that carries the correction, the
// period lock it runs under, and the writing of the deltas and the credit notes
// live in internal/engine/runs.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.9.
package corrections

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/attribution"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
)

// Key is what the amounts of two passes are diffed by (D6): the resource an
// amount was rated for, the project that owned the resource while it ran, and
// the dimension it was billed on. Two amounts under one key are the same line
// of the same invoice, one rated before the late events arrived and one after.
type Key struct {
	Cloud, Platform, ResourceType, ResourceID, ProjectID, Dimension string
}

// Delta is one non-zero difference between the two passes. Delta is New minus
// Old: a credit is negative and a debit positive, so usage that turned out
// smaller than the finalized run billed is money owed back.
type Delta struct {
	Key
	Old, New, Delta decimal.Decimal
}

// BuildResult is what one rendering pass produced: the credit notes the
// correction is settled from, and the project ids they were billed under that
// the registry does not hold.
type BuildResult struct {
	// Statements holds one entry per project that gets a credit note, sorted by
	// Key.
	Statements []statements.Statement
	// Unregistered holds one entry per project id no registry row matched,
	// sorted by cloud and then by project id.
	Unregistered []statements.UnregisteredProject
}

// CreditNote is one project's credit note: the deltas the correction credits or
// debits it for, and the run they correct. The field order is the order the
// document is marshalled in.
type CreditNote struct {
	BillingPeriod statements.BillingPeriod `json:"billing_period"`
	ProjectID     string                   `json:"project_id"`
	Platform      string                   `json:"platform"`
	CorrectsRunID string                   `json:"corrects_run_id"`
	LineItems     []LineItem               `json:"line_items"`
	RelatedCosts  []RelatedCost            `json:"related_costs"`
	Total         money.Amount             `json:"total"`
	Currency      string                   `json:"currency"`
}

// LineItem is one resource's deltas as one project is credited or debited for
// them: one entry per dimension the two passes disagree on, and what they add
// up to.
type LineItem struct {
	ResourceType string            `json:"resource_type"`
	ResourceID   string            `json:"resource_id"`
	Platform     string            `json:"platform"`
	Dimensions   map[string]Change `json:"dimensions"`
	Total        money.Amount      `json:"total"`
}

// Change is one dimension of one resource on the note: what the run being
// corrected billed, what the correction rated, and the difference the project
// is credited or debited for.
type Change struct {
	Old   money.Amount `json:"old"`
	New   money.Amount `json:"new"`
	Delta money.Amount `json:"delta"`
}

// RelatedCost is one attributed project's deltas on the credit note of the
// project they are billed under: the type of the edge that claimed it, who it
// is, and the same line items it would carry standalone.
type RelatedCost struct {
	RelationType string       `json:"relation_type"`
	ProjectID    string       `json:"project_id"`
	Platform     string       `json:"platform"`
	LineItems    []LineItem   `json:"line_items"`
	Total        money.Amount `json:"total"`
}

// Amounts sums what one pass rated, per key. usage and rated are index-aligned
// per resource the way rating produced them: record j of a rated resource rates
// draft j of the same resource in usage, and the project that draft carried
// owns the amounts of the record. A rated resource that usage does not hold,
// and one whose record count differs from its draft count, are errors rather
// than sums built from whatever lines up: an amount filed under the wrong
// project is a delta against a line that never existed.
//
// Every amount was rounded where it was rated, and the sums here are never
// rounded again (roadmap/00-conventions.md section 6). A dimension that cost
// nothing is summed all the same, so the two passes are diffed over every
// dimension they were held against rather than over the ones that cost money.
//
// A pass with no rated resources yields an empty map, which is what a period
// that billed nothing is diffed as: every key of the other side becomes a
// delta.
func Amounts(usage []metering.ResourceUsage, rated rating.Result) (map[Key]decimal.Decimal, error) {
	drafts := make(map[source.Resource][]metering.UsageDraft, len(usage))
	for _, resource := range usage {
		drafts[resource.Resource] = resource.Drafts
	}

	amounts := make(map[Key]decimal.Decimal)
	for _, resource := range rated.Resources {
		metered, held := drafts[resource.Resource]
		if !held {
			return nil, fmt.Errorf("the rated resource %s carries no metered usage", name(resource.Resource))
		}
		if len(resource.Records) != len(metered) {
			return nil, fmt.Errorf("the rated resource %s carries %d records for %d usage drafts",
				name(resource.Resource), len(resource.Records), len(metered))
		}

		for i, record := range resource.Records {
			// The drafts of a resource that changed hands mid-period name two
			// projects, and each of them is billed for the records of its own
			// drafts, so the resource reaches two keys here.
			draft := metered[i]
			for _, dimension := range record.Amounts {
				key := Key{
					Cloud:        resource.Resource.Cloud,
					Platform:     resource.Resource.Platform,
					ResourceType: resource.Resource.ResourceType,
					ResourceID:   resource.Resource.ResourceID,
					ProjectID:    draft.ProjectID,
					Dimension:    dimension.Metric,
				}
				amounts[key] = amounts[key].Add(dimension.Amount)
			}
		}
	}
	return amounts, nil
}

// Diff is what changed between the two passes: one Delta per key they disagree
// on, sorted by cloud, platform, resource type, resource id, project id, and
// dimension. A key one side does not hold counts as zero there, so a resource
// the correction no longer bills is credited its whole amount and one it bills
// for the first time is debited for it.
//
// A key the two sides agree on is left out, one both of them rated at zero
// included: a delta of 0.00 is nothing to credit and nothing to invoice.
// Passes that agree on everything, two empty ones included, yield nil rather
// than an empty slice.
func Diff(old, current map[Key]decimal.Decimal) []Delta {
	var deltas []Delta
	for key, amount := range current {
		// A key old does not hold reads as the zero decimal, which is what the
		// correction rating it for the first time diffs against.
		deltas = appendDelta(deltas, key, old[key], amount)
	}
	for key, amount := range old {
		if _, held := current[key]; held {
			continue
		}
		deltas = appendDelta(deltas, key, amount, decimal.Zero)
	}

	slices.SortFunc(deltas, func(a, b Delta) int {
		return cmp.Or(
			cmp.Compare(a.Cloud, b.Cloud),
			cmp.Compare(a.Platform, b.Platform),
			cmp.Compare(a.ResourceType, b.ResourceType),
			cmp.Compare(a.ResourceID, b.ResourceID),
			cmp.Compare(a.ProjectID, b.ProjectID),
			cmp.Compare(a.Dimension, b.Dimension),
		)
	})
	return deltas
}

// appendDelta keeps one difference where there is one. The two amounts are
// already rounded, and their difference is not rounded again.
func appendDelta(deltas []Delta, key Key, old, current decimal.Decimal) []Delta {
	difference := current.Sub(old)
	if difference.IsZero() {
		return deltas
	}
	return append(deltas, Delta{Key: key, Old: old, New: current, Delta: difference})
}

// BuildCreditNotes renders one credit note per project that owns at least one
// delta. The grouping is the one statements.Build applies to a period's rated
// records, over the deltas instead: a project's own resources are its line
// items, and every project attributed to it arrives as a related cost beside
// them. A note is stored under the key its project's statement is stored under
// (statements.Key), so a credit note lands where the document it corrects did.
//
// Of the resolution only Attributed is read, as in statements.Build. A project
// it does not name is credited under itself, and a project id the registry does
// not hold is credited standalone under that raw id and named in
// BuildResult.Unregistered.
//
// A note's total is the sum of its line items and its related costs, so a
// project that is owed money reads a negative total. Correcting nothing is not
// an error: without deltas there is nothing to credit and no document comes
// back.
func BuildCreditNotes(
	periodFrom, periodTo time.Time,
	correctsRunID uuid.UUID,
	currency string,
	deltas []Delta,
	projects []source.Project,
	res attribution.Resolution,
) (BuildResult, error) {
	if len(deltas) == 0 {
		return BuildResult{}, nil
	}

	byPair := make(map[ownerKey]source.Project, len(projects))
	byID := make(map[uuid.UUID]source.Project, len(projects))
	for _, project := range projects {
		byPair[ownerKey{project.Cloud, project.ExternalID}] = project
		byID[project.ID] = project
	}

	owners := make(map[ownerKey]*owner)
	for _, delta := range deltas {
		resource := source.Resource{
			Cloud:        delta.Cloud,
			Platform:     delta.Platform,
			ResourceType: delta.ResourceType,
			ResourceID:   delta.ResourceID,
		}
		item := ownerOf(owners, byPair, ownerKey{delta.Cloud, delta.ProjectID}).item(resource)
		item.changes[delta.Dimension] = Change{
			Old:   money.NewAmount(delta.Old),
			New:   money.NewAmount(delta.New),
			Delta: money.NewAmount(delta.Delta),
		}
		item.total = item.total.Add(delta.Delta)
	}

	billingPeriod := statements.BillingPeriod{
		From: periodFrom.UTC().Format(time.RFC3339),
		To:   periodTo.UTC().Format(time.RFC3339),
	}
	documents := make(map[ownerKey]*builder)
	var result BuildResult

	for _, own := range owners {
		// Every owner reached here holds at least one line item: it was created
		// by the delta that produced one.
		items := own.lineItems()

		if !own.registered {
			result.Unregistered = append(result.Unregistered, statements.UnregisteredProject{
				Cloud:     own.key.cloud,
				ProjectID: own.key.projectID,
				Resources: len(items),
			})
			// A cloud is one installation of one platform, so the resources of a
			// pair cannot disagree on which one that is.
			documentOf(documents, own.key, own.key.projectID, items[0].Platform).add(items)
			continue
		}

		if attributed, claimed := res.Attributed[own.project.ID]; claimed {
			// A chain is already flattened onto the root the walk started at, and
			// a root the registry does not hold is one nothing can be keyed to:
			// its deltas stay on their own project's note rather than on a
			// document named after nobody.
			if root, known := byID[attributed.Root]; known {
				document := documentOf(documents, ownerKey{root.Cloud, root.ExternalID}, root.ExternalID, root.Platform)
				document.related = append(document.related, related{
					key: own.key,
					cost: RelatedCost{
						RelationType: attributed.RelationType,
						ProjectID:    own.project.ExternalID,
						Platform:     own.project.Platform,
						LineItems:    items,
						Total:        money.NewAmount(sum(items)),
					},
				})
				continue
			}
		}

		documentOf(documents, own.key, own.project.ExternalID, own.project.Platform).add(items)
	}

	result.Statements = make([]statements.Statement, 0, len(documents))
	for _, document := range documents {
		statement, err := document.render(billingPeriod, correctsRunID, currency)
		if err != nil {
			return BuildResult{}, err
		}
		result.Statements = append(result.Statements, statement)
	}
	slices.SortFunc(result.Statements, func(a, b statements.Statement) int {
		return cmp.Compare(a.Key, b.Key)
	})
	slices.SortFunc(result.Unregistered, func(a, b statements.UnregisteredProject) int {
		return cmp.Or(
			cmp.Compare(a.Cloud, b.Cloud),
			cmp.Compare(a.ProjectID, b.ProjectID),
		)
	})
	return result, nil
}

// ownerKey is the pair a delta belongs to: the cloud its resource was metered
// in and the project id it was billed under, which is the pair the registry is
// matched on (projects.cloud, projects.external_id).
type ownerKey struct {
	cloud     string
	projectID string
}

// owner is one such pair while the pass runs: the registry row behind it, where
// there is one, and one line item per resource the pair is credited for.
type owner struct {
	key        ownerKey
	project    source.Project
	registered bool
	items      map[source.Resource]*lineItem
}

// lineItem is one resource's line while its dimensions arrive.
type lineItem struct {
	resource source.Resource
	changes  map[string]Change
	total    decimal.Decimal
}

// builder is one credit note while its parts arrive. A root is reached both by
// its own deltas and by every project attributed to it, in no fixed order, so
// the document is identified on each reach and ordered when it is rendered.
type builder struct {
	key       string
	projectID string
	platform  string
	items     []LineItem
	related   []related
}

// related is one related-costs entry and the pair of the project it credits,
// which is what the entries of a root are sorted by. The pair rather than the
// key it renders to, for the reason statements.related carries.
type related struct {
	key  ownerKey
	cost RelatedCost
}

// ownerOf is the pair's owner, created on first reach. A pair the registry does
// not hold is credited standalone under its raw id, an empty project id
// included: deltas that name no project are money owed all the same.
func ownerOf(owners map[ownerKey]*owner, byPair map[ownerKey]source.Project, key ownerKey) *owner {
	own, held := owners[key]
	if !held {
		project, registered := byPair[key]
		own = &owner{
			key:        key,
			project:    project,
			registered: registered,
			items:      make(map[source.Resource]*lineItem),
		}
		owners[key] = own
	}
	return own
}

// item is the resource's line item under this owner, created on first reach.
func (o *owner) item(resource source.Resource) *lineItem {
	item, held := o.items[resource]
	if !held {
		item = &lineItem{resource: resource, changes: make(map[string]Change)}
		o.items[resource] = item
	}
	return item
}

// lineItems renders the owner's lines, sorted by platform, resource type, and
// resource id. The dimensions of a line are a map, which encoding/json writes
// with its keys sorted.
func (o *owner) lineItems() []LineItem {
	items := make([]LineItem, 0, len(o.items))
	for _, item := range o.items {
		items = append(items, LineItem{
			ResourceType: item.resource.ResourceType,
			ResourceID:   item.resource.ResourceID,
			Platform:     item.resource.Platform,
			Dimensions:   item.changes,
			Total:        money.NewAmount(item.total),
		})
	}
	slices.SortFunc(items, func(a, b LineItem) int {
		return cmp.Or(
			cmp.Compare(a.Platform, b.Platform),
			cmp.Compare(a.ResourceType, b.ResourceType),
			cmp.Compare(a.ResourceID, b.ResourceID),
		)
	})
	return items
}

// documentOf is the credit note being built for the pair, created on first
// reach. The map is keyed on the pair rather than on the text it renders to, so
// what a document holds is decided before any text is joined: merging two pairs
// would credit one customer for the other's resources.
//
// The project a document names is the one it is keyed to, so a root reached
// over an attributed project first is named after the root rather than after
// whoever arrived with it.
func documentOf(documents map[ownerKey]*builder, key ownerKey, projectID, platform string) *builder {
	document, held := documents[key]
	if !held {
		document = &builder{
			key:       statements.Key(key.cloud, key.projectID),
			projectID: projectID,
			platform:  platform,
			items:     []LineItem{},
		}
		documents[key] = document
	}
	return document
}

// add puts one owner's lines on the note. Only the project the note is keyed to
// bills its lines here; every other one arrives as a related cost.
func (d *builder) add(items []LineItem) {
	d.items = append(d.items, items...)
}

// render marshals the credit note. The lines and the related costs are already
// ordered among themselves, the related entries are ordered here, and the map
// keys of a line are ordered by encoding/json, so the bytes are a function of
// the deltas alone.
func (d *builder) render(
	billingPeriod statements.BillingPeriod,
	correctsRunID uuid.UUID,
	currency string,
) (statements.Statement, error) {
	slices.SortFunc(d.related, func(a, b related) int {
		return cmp.Or(
			cmp.Compare(a.key.cloud, b.key.cloud),
			cmp.Compare(a.key.projectID, b.key.projectID),
		)
	})

	note := CreditNote{
		BillingPeriod: billingPeriod,
		ProjectID:     d.projectID,
		Platform:      d.platform,
		CorrectsRunID: correctsRunID.String(),
		LineItems:     d.items,
		RelatedCosts:  make([]RelatedCost, 0, len(d.related)),
		Currency:      currency,
	}
	total := sum(d.items)
	for _, entry := range d.related {
		note.RelatedCosts = append(note.RelatedCosts, entry.cost)
		total = total.Add(entry.cost.Total.Decimal)
	}
	note.Total = money.NewAmount(total)

	body, err := json.Marshal(note)
	if err != nil {
		return statements.Statement{}, fmt.Errorf("marshalling the credit note of %s: %w", d.key, err)
	}
	return statements.Statement{Key: d.key, Document: body, Total: total, Currency: currency}, nil
}

// sum adds up line item totals. They are already rounded, and an aggregate is
// never rounded again.
func sum(items []LineItem) decimal.Decimal {
	total := decimal.Decimal{}
	for _, item := range items {
		total = total.Add(item.Total.Decimal)
	}
	return total
}

// name identifies a resource in an error.
func name(resource source.Resource) string {
	return fmt.Sprintf("%s/%s/%s/%s",
		resource.Cloud, resource.Platform, resource.ResourceType, resource.ResourceID)
}

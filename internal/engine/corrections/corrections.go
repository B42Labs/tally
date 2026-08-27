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
// Adjustments are neither a resource nor a dimension, so the adjustment records
// of the two passes are diffed on a key of their own: the statement they were
// applied to, the relation they came from, and the type, the scope and the rate
// of the element. Their differences render on the credit note beside the rated
// deltas, and the note's total is what the correction settles after them
// (roadmap/05-phase-5-commercial-pricing.md, WP 5.3).
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

	"github.com/b42labs/tally/internal/core/adjustment"
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

// AdjustmentKey is what the adjustment records of two passes are diffed by: the
// statement the adjustment was applied to, the relation it came from, and the
// type, the scope and the rate of the element. Two elements of one relation
// that agree on all three are one adjustment as far as the diff goes, because
// nothing else on the line tells them apart.
//
// Rate is the six-place text money.Rate renders, the scale the adjustments
// schema admits, because a decimal is not a map key.
type AdjustmentKey struct {
	StatementKey, RelationID, Type, Scope, Rate string
}

// AdjustmentAmount is one side of the adjustment diff: what the records under
// one key add up to, the relation they came from and the rate they were applied
// at. The relation and the rate travel with the amount so a delta names them
// without the statements being read again.
type AdjustmentAmount struct {
	RelationType, RelationTarget string
	// RateValue is AdjustmentKey.Rate as the decimal it was rendered from, which
	// is what a credit-note line writes.
	RateValue decimal.Decimal
	Amount    decimal.Decimal
}

// AdjustmentDelta is one non-zero difference between the adjustments of the two
// passes. Delta is New minus Old, the way Delta.Delta is, and it carries the
// sign of the amounts it is taken between: a discount the correction applies
// less of comes out positive, which is money the project owes back.
type AdjustmentDelta struct {
	AdjustmentKey
	RelationType, RelationTarget string
	// RateValue is AdjustmentKey.Rate as a decimal, the way AdjustmentAmount
	// carries it.
	RateValue       decimal.Decimal
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
	// BaseDelta is what the line items and the related costs add up to before
	// the adjustments, NetDelta what they come to after them, and KickbackDelta
	// what a partner's commission changed by beside the net rather than as part
	// of it. The four members are nil on a note no adjustment delta reached,
	// whose bytes hold none of them. Total is the net delta where they are
	// there, which is what the correction settles.
	//
	// None of them carries a currency of its own: every amount on the note is in
	// the currency Currency names, the way Total already renders.
	BaseDelta     *money.Amount      `json:"base_delta,omitempty"`
	Adjustments   []AdjustmentChange `json:"adjustments,omitempty"`
	NetDelta      *money.Amount      `json:"net_delta,omitempty"`
	KickbackDelta *money.Amount      `json:"kickback_delta,omitempty"`
	Total         money.Amount       `json:"total"`
	Currency      string             `json:"currency"`
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

// AdjustmentChange is one adjustment on the credit note: the relation it came
// from, what the run being corrected applied, what the correction applied, and
// the difference the project is credited or debited for. The field order is the
// order it is marshalled in.
type AdjustmentChange struct {
	Type           string       `json:"type"`
	RelationType   string       `json:"relation_type"`
	RelationTarget string       `json:"relation_target"`
	RelationID     string       `json:"relation_id"`
	Scope          string       `json:"scope"`
	Rate           money.Rate   `json:"rate"`
	Old            money.Amount `json:"old"`
	New            money.Amount `json:"new"`
	Delta          money.Amount `json:"delta"`
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

// AdjustmentAmounts sums what the adjustments of one pass came to, per key. A
// statement carries its applied adjustments beside its total, and two elements
// of one relation that agree on type, scope and rate are summed under one key.
//
// A pass whose statements carry no adjustments yields an empty map, which is
// what a period that adjusted nothing is diffed as: every key of the other side
// becomes a delta.
func AdjustmentAmounts(sts []statements.Statement) map[AdjustmentKey]AdjustmentAmount {
	amounts := make(map[AdjustmentKey]AdjustmentAmount)
	for _, statement := range sts {
		for _, line := range statement.Adjustments {
			key := AdjustmentKey{
				StatementKey: statement.Key,
				RelationID:   line.RelationID,
				Type:         line.Type,
				Scope:        line.Scope,
				Rate:         line.Rate.StringFixed(money.RatePlaces),
			}
			amounts[key] = AdjustmentAmount{
				RelationType:   line.RelationType,
				RelationTarget: line.RelationTarget,
				RateValue:      line.Rate.Decimal,
				Amount:         amounts[key].Amount.Add(line.Amount.Decimal),
			}
		}
	}
	return amounts
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

// DiffAdjustments is what changed between the adjustments of the two passes:
// one AdjustmentDelta per key they disagree on, sorted by statement key,
// relation id, the application order of the type (adjustment.TypeOrder), scope
// and rate. A key one side does not hold counts as zero there, so an adjustment
// the correction no longer applies is credited back whole and one it applies
// for the first time is charged whole.
//
// A key the two sides agree on is left out, one both of them applied at zero
// included. Passes that adjusted the same, two empty ones included, yield nil
// rather than an empty slice.
func DiffAdjustments(old, current map[AdjustmentKey]AdjustmentAmount) []AdjustmentDelta {
	var deltas []AdjustmentDelta
	for key, amount := range current {
		// A key old does not hold reads as the zero decimal, which is what the
		// correction applying the adjustment for the first time diffs against.
		deltas = appendAdjustmentDelta(deltas, key, amount, old[key].Amount, amount.Amount)
	}
	for key, amount := range old {
		if _, held := current[key]; held {
			continue
		}
		deltas = appendAdjustmentDelta(deltas, key, amount, amount.Amount, decimal.Zero)
	}

	slices.SortFunc(deltas, func(a, b AdjustmentDelta) int {
		return cmp.Or(
			cmp.Compare(a.StatementKey, b.StatementKey),
			cmp.Compare(a.RelationID, b.RelationID),
			cmp.Compare(slices.Index(adjustment.TypeOrder, a.Type), slices.Index(adjustment.TypeOrder, b.Type)),
			cmp.Compare(a.Scope, b.Scope),
			cmp.Compare(a.Rate, b.Rate),
		)
	})
	return deltas
}

// appendAdjustmentDelta keeps one difference where there is one. The relation
// and the rate come from the side the key is read off, which is the current one
// wherever it holds the key. The two amounts are already rounded, and their
// difference is not rounded again.
func appendAdjustmentDelta(
	deltas []AdjustmentDelta,
	key AdjustmentKey,
	relation AdjustmentAmount,
	old, current decimal.Decimal,
) []AdjustmentDelta {
	difference := current.Sub(old)
	if difference.IsZero() {
		return deltas
	}
	return append(deltas, AdjustmentDelta{
		AdjustmentKey:  key,
		RelationType:   relation.RelationType,
		RelationTarget: relation.RelationTarget,
		RateValue:      relation.RateValue,
		Old:            old,
		New:            current,
		Delta:          difference,
	})
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
// An adjustment delta lands on the note of the statement key it names, and
// creates that note where no rated delta did. Such a note shows what its lines
// came to before the adjustments, the changes themselves, and what they come to
// after them; its total is the net delta. A note no adjustment delta reached
// renders as it does without adjustments at all.
//
// A note's total is the sum of its line items and its related costs, adjusted
// by the changes it carries, so a project that is owed money reads a negative
// total. Correcting nothing is not an error: without deltas of either kind
// there is nothing to credit and no document comes back.
func BuildCreditNotes(
	periodFrom, periodTo time.Time,
	correctsRunID uuid.UUID,
	currency string,
	deltas []Delta,
	adjustmentDeltas []AdjustmentDelta,
	projects []source.Project,
	res attribution.Resolution,
) (BuildResult, error) {
	if len(deltas) == 0 && len(adjustmentDeltas) == 0 {
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

	for _, change := range adjustmentDeltas {
		document, err := adjustedDocument(documents, byPair, change.StatementKey)
		if err != nil {
			return BuildResult{}, err
		}
		document.changes = append(document.changes, change)
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
	// changes holds the adjustment deltas of the note, in the order
	// DiffAdjustments sorted them.
	changes []AdjustmentDelta
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

// adjustedDocument is the note an adjustment delta lands on: the one the rated
// deltas already created for the statement key, or a new one for the pair the
// key names. An adjustment reaches a statement over a relation of a registered
// project, so a pair the registry does not hold is refused rather than credited
// under a document named after nobody.
func adjustedDocument(
	documents map[ownerKey]*builder,
	byPair map[ownerKey]source.Project,
	key string,
) (*builder, error) {
	cloud, projectID, err := statements.ParseKey(key)
	if err != nil {
		return nil, fmt.Errorf("reading the statement key of an adjustment delta: %w", err)
	}

	pair := ownerKey{cloud: cloud, projectID: projectID}
	if document, held := documents[pair]; held {
		return document, nil
	}
	project, registered := byPair[pair]
	if !registered {
		return nil, fmt.Errorf("the adjustment deltas of %s name a project the registry does not hold", key)
	}
	return documentOf(documents, pair, project.ExternalID, project.Platform), nil
}

// add puts one owner's lines on the note. Only the project the note is keyed to
// bills its lines here; every other one arrives as a related cost.
func (d *builder) add(items []LineItem) {
	d.items = append(d.items, items...)
}

// render marshals the credit note. The lines and the related costs are already
// ordered among themselves, the related entries are ordered here, the changes
// keep the order DiffAdjustments sorted them in, and the map keys of a line are
// ordered by encoding/json, so the bytes are a function of the deltas alone.
//
// The changes apply to the sum of every line the note credits, its own and the
// related ones, because that sum is what the adjustments of the two passes were
// applied to. A note without changes leaves the four adjustment members nil and
// is the document an unadjusted correction renders.
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

	if len(d.changes) > 0 {
		baseDelta := money.NewAmount(total)
		net, kickback := total, decimal.Zero
		changes := make([]AdjustmentChange, 0, len(d.changes))
		for _, change := range d.changes {
			changes = append(changes, AdjustmentChange{
				Type:           change.Type,
				RelationType:   change.RelationType,
				RelationTarget: change.RelationTarget,
				RelationID:     change.RelationID,
				Scope:          change.Scope,
				Rate:           money.NewRate(change.RateValue),
				Old:            money.NewAmount(change.Old),
				New:            money.NewAmount(change.New),
				Delta:          money.NewAmount(change.Delta),
			})

			// A kickback is what a partner is owed rather than what the customer
			// pays, so it stands beside the net rather than in it.
			if change.Type == adjustment.TypeKickback {
				kickback = kickback.Add(change.Delta)
				continue
			}
			net = net.Add(change.Delta)
		}

		netDelta := money.NewAmount(net)
		kickbackDelta := money.NewAmount(kickback)
		note.BaseDelta = &baseDelta
		note.Adjustments = changes
		note.NetDelta = &netDelta
		note.KickbackDelta = &kickbackDelta
		total = net
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

// Package statements renders what a billing period is invoiced from: one
// statement document per top-level project, in the format the concept fixes
// (README section 3.4), holding a line item per resource, a period per usage
// draft, and the costs of every project attributed to that one beside its own.
// Build is a pure function. It reads nothing and writes nothing, so a period is
// rendered from the metering, rating, and attribution results a caller already
// holds.
//
// Every number in a document comes from those results, which is why the
// renderer takes no pricing model. An amount was rounded where it was rated,
// and every total here is a sum of already-rounded amounts (roadmap/
// 00-conventions.md section 6), so a total equals the sum of the line items
// printed under it. The one value derived here is the hours a period shows,
// its minutes over sixty at two places, and that value is display only: nothing
// is computed from it.
//
// Attribution is exclusive, and a draft carries the project that owned the
// resource while the draft ran. A resource transferred mid-period therefore
// appears on two statements, with the periods each project owned it for, rather
// than being billed whole to whoever holds it at the end of the month.
//
// A draft whose project id has no registry row is not an error. It is billed
// standalone under that raw id and named in BuildResult.Unregistered, which the
// run reports through runs.stats: usage somebody consumed is not dropped
// because nobody registered the project it ran in.
//
// A caller that passes an adjuster has the pricing adjustments of the
// statement's project applied here: the document then shows the base cost the
// period was rated at, one line per adjustment, the net cost and the kickbacks
// a partner is owed. Its total is the net cost, which is what the customer
// pays. A statement no adjustment reaches holds none of those members and
// renders the same bytes as one built without an adjuster.
//
// The normative specification is roadmap/03-phase-3-metering-rating.md, WP 3.7,
// and roadmap/05-phase-5-commercial-pricing.md, WP 5.3, for the adjustment
// members.
package statements

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/adjustments"
	"github.com/b42labs/tally/internal/engine/attribution"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/source"
)

// The two usage fields a description is read from, in this order: the flavor a
// compute resource was created from, and the type a volume carries, which is
// the same field rating selects a type modifier by. A draft that holds neither
// as text is described by what it is rather than by what it was built from.
const (
	usageFlavor = "flavor"
	usageType   = "type"
)

// costTotal is the key a period's cost object holds the record's total under.
// The name is reserved the way metering reserves the usage fields it derives
// (metering.InvariantReservedUsageField): a pricing dimension of that metric
// would be rendered over the total, or the total over it, and either way one of
// the two numbers a customer reconciles the line against would be gone. Such a
// model is refused rather than rendered.
const costTotal = "total"

// minutesPerHour turns the minutes a draft ran into the hours its period shows.
var minutesPerHour = decimal.NewFromInt(60)

// BuildResult is what one rendering pass produced: the documents the period is
// invoiced from, and the project ids they were billed under that the registry
// does not hold.
type BuildResult struct {
	// Statements holds one entry per project that gets a document, sorted by
	// Key.
	Statements []Statement
	// Unregistered holds one entry per project id no registry row matched,
	// sorted by cloud and then by project id.
	Unregistered []UnregisteredProject
}

// Statement is one project's document and the two values a caller stores beside
// it without opening the document again.
type Statement struct {
	// Key is what the statement is stored under: the cloud and the external id
	// of its project, each percent-escaped and joined by a slash. External ids
	// are unique per cloud only, which is why the cloud is part of the key. It
	// is a database key rather than a file name.
	//
	// Neither half is constrained against the separator, which is why both are
	// escaped: os-prod paired with eu/acme would otherwise render what
	// os-prod/eu paired with acme does, and the two projects would meet on the
	// unique key over (run_id, project_id) with nothing but a duplicate-key
	// error to say which pairs produced it. Escaped, every pair renders a key no
	// other pair does.
	Key string
	// Document is the rendered document, marshalled once here. Go marshals a map
	// with its keys sorted, so the same input yields the same bytes.
	Document []byte
	// Total is the document's total, the same decimal the document shows.
	Total decimal.Decimal
	// Currency is the currency of the model the period was rated with.
	Currency string
	// Adjustments holds the adjustment lines the document shows, which the run
	// stores as the statement's adjustment records. It is nil on a statement no
	// adjustment reached.
	Adjustments []adjustments.Line
}

// UnregisteredProject is a project id drafts carried that the registry does not
// hold, and how many of the period's resources were billed under it. Resources
// counts resources, not their drafts. The run writes the slice into
// runs.stats.unregistered_projects, which is what the JSON tags name the fields
// for.
type UnregisteredProject struct {
	Cloud     string `json:"cloud"`
	ProjectID string `json:"project_id"`
	Resources int    `json:"resources"`
}

// Document is one project's statement. The field order is the order the
// document is marshalled in, which is the order the concept prints it in.
type Document struct {
	BillingPeriod BillingPeriod `json:"billing_period"`
	ProjectID     string        `json:"project_id"`
	Platform      string        `json:"platform"`
	LineItems     []LineItem    `json:"line_items"`
	RelatedCosts  []RelatedCost `json:"related_costs"`
	// BaseCost is what the line items and the related costs add up to before
	// the adjustments, NetCost what they come to after them, and KickbackTotal
	// what a partner is owed beside the net cost rather than as part of it. The
	// four members are nil on a statement no adjustment reached, whose bytes
	// hold none of them. Total is the net cost where they are there, which is
	// what the customer pays.
	//
	// None of them carries a currency of its own: every amount in the document
	// is in the currency Currency names, the way Total already renders.
	BaseCost      *money.Amount      `json:"base_cost,omitempty"`
	Adjustments   []adjustments.Line `json:"adjustments,omitempty"`
	NetCost       *money.Amount      `json:"net_cost,omitempty"`
	KickbackTotal *money.Amount      `json:"kickback_total,omitempty"`
	Total         money.Amount       `json:"total"`
	Currency      string             `json:"currency"`
}

// BillingPeriod is the half-open interval the document bills, both ends in UTC
// and RFC 3339.
type BillingPeriod struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// LineItem is one resource as one project is billed for it: every period of the
// resource that project owned it for, and what they add up to.
type LineItem struct {
	ResourceType string       `json:"resource_type"`
	ResourceID   string       `json:"resource_id"`
	Platform     string       `json:"platform"`
	Description  string       `json:"description"`
	Periods      []Period     `json:"periods"`
	Total        money.Amount `json:"total"`
}

// Period is one usage draft rendered: the state the resource was in, the hours
// it was in it, the quantities every dimension was rated from, what each of
// them cost, and what the state was billed at. Cost holds one key per dimension
// plus costTotal.
type Period struct {
	State         string                    `json:"state"`
	Hours         money.Amount              `json:"hours"`
	Usage         map[string]money.Quantity `json:"usage"`
	Cost          map[string]money.Amount   `json:"cost"`
	StateModifier money.Quantity            `json:"state_modifier"`
}

// RelatedCost is one attributed project's costs on the statement of the project
// they are billed under: the type of the edge that claimed it, who it is, and
// the same line items it would carry standalone.
type RelatedCost struct {
	RelationType string       `json:"relation_type"`
	ProjectID    string       `json:"project_id"`
	Platform     string       `json:"platform"`
	LineItems    []LineItem   `json:"line_items"`
	Total        money.Amount `json:"total"`
}

// Build renders the period's statements. usage and rated are index-aligned per
// resource the way rating produced them: record j of a rated resource rates
// draft j of the same resource in usage. A rated resource that usage does not
// hold, and one whose record count differs from its draft count, are errors
// rather than documents rendered from whatever lines up: a statement short by a
// period is one nobody can tell from a correct one.
//
// Of the resolution only Attributed is read. A project it does not name is
// billed under itself, which is what a top-level project, an orphaned one, and
// one no relation touched all are.
//
// The adjuster applies the pricing adjustments of a project's relations to the
// document it is billed on, and nil renders every document unadjusted. The walk
// starts at the project the statement is keyed to, so an attributed project's
// own relations reach no statement: its costs are billed on its root's
// statement, and only the root's relations adjust them.
//
// Rendering nothing is not an error: a period without usage and without rated
// resources yields no statements.
func Build(
	periodFrom, periodTo time.Time,
	usage []metering.ResourceUsage,
	rated rating.Result,
	projects []source.Project,
	res attribution.Resolution,
	adjuster *adjustments.Adjuster,
) (BuildResult, error) {
	if len(usage) == 0 && len(rated.Resources) == 0 {
		return BuildResult{}, nil
	}

	drafts := make(map[source.Resource][]metering.UsageDraft, len(usage))
	for _, resource := range usage {
		drafts[resource.Resource] = resource.Drafts
	}
	byPair := make(map[ownerKey]source.Project, len(projects))
	byID := make(map[uuid.UUID]source.Project, len(projects))
	for _, project := range projects {
		byPair[ownerKey{project.Cloud, project.ExternalID}] = project
		byID[project.ID] = project
	}

	owners := make(map[ownerKey]*owner)
	for _, resource := range rated.Resources {
		metered, held := drafts[resource.Resource]
		if !held {
			return BuildResult{}, fmt.Errorf("the rated resource %s carries no metered usage", name(resource.Resource))
		}
		if len(resource.Records) != len(metered) {
			return BuildResult{}, fmt.Errorf("the rated resource %s carries %d records for %d usage drafts",
				name(resource.Resource), len(resource.Records), len(metered))
		}

		for i, record := range resource.Records {
			draft := metered[i]
			period, total, err := periodOf(record, draft)
			if err != nil {
				return BuildResult{}, err
			}
			// The draft's project owns the resource for as long as the draft ran,
			// so a resource that changed hands mid-period reaches two owners here.
			item := ownerOf(owners, byPair, ownerKey{resource.Resource.Cloud, draft.ProjectID}).item(resource.Resource)
			item.periods = append(item.periods, period)
			item.total = item.total.Add(total)
			// Every draft describes the resource again, and the last one is what
			// the line shows: see describe.
			item.description = describe(resource.Resource, draft.Usage)
		}
	}

	billingPeriod := BillingPeriod{
		From: periodFrom.UTC().Format(time.RFC3339),
		To:   periodTo.UTC().Format(time.RFC3339),
	}
	documents := make(map[ownerKey]*builder)
	var result BuildResult

	for _, own := range owners {
		// Every owner reached here holds at least one line item: it was created by
		// the record that produced one.
		items := own.lineItems()

		if !own.registered {
			result.Unregistered = append(result.Unregistered, UnregisteredProject{
				Cloud:     own.key.cloud,
				ProjectID: own.key.projectID,
				Resources: len(items),
			})
			// A cloud is one installation of one platform, so the resources of a
			// pair cannot disagree on which one that is.
			documentOf(documents, own.key, own.key.projectID, items[0].Platform, uuid.Nil).add(items)
			continue
		}

		if attributed, claimed := res.Attributed[own.project.ID]; claimed {
			// A chain is already flattened onto the root the walk started at, so
			// the entry lands on the root's statement however many edges away it
			// was claimed from. A root the registry does not hold is one nothing
			// can be keyed to, and its costs stay on their own project's statement
			// rather than on a document named after nobody.
			if root, known := byID[attributed.Root]; known {
				document := documentOf(
					documents, ownerKey{root.Cloud, root.ExternalID}, root.ExternalID, root.Platform, root.ID)
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

		documentOf(documents, own.key, own.project.ExternalID, own.project.Platform, own.project.ID).add(items)
	}

	result.Statements = make([]Statement, 0, len(documents))
	for _, document := range documents {
		statement, err := document.render(billingPeriod, rated.Currency, adjuster)
		if err != nil {
			return BuildResult{}, err
		}
		result.Statements = append(result.Statements, statement)
	}
	slices.SortFunc(result.Statements, func(a, b Statement) int {
		return cmp.Compare(a.Key, b.Key)
	})
	slices.SortFunc(result.Unregistered, func(a, b UnregisteredProject) int {
		return cmp.Or(
			cmp.Compare(a.Cloud, b.Cloud),
			cmp.Compare(a.ProjectID, b.ProjectID),
		)
	})
	return result, nil
}

// ownerKey is the pair a draft's costs belong to: the cloud its resource was
// metered in and the project id the draft carried, which is the pair the
// registry is matched on (projects.cloud, projects.external_id).
type ownerKey struct {
	cloud     string
	projectID string
}

// owner is one such pair while the pass runs: the registry row behind it, where
// there is one, and one line item per resource billed to the pair.
type owner struct {
	key        ownerKey
	project    source.Project
	registered bool
	items      map[source.Resource]*lineItem
}

// lineItem is one resource's line while its periods are collected.
type lineItem struct {
	resource    source.Resource
	description string
	periods     []Period
	total       decimal.Decimal
}

// builder is one statement while its parts arrive. A root is reached both by
// its own usage and by every project attributed to it, in no fixed order, so
// the document is identified on each reach and ordered when it is rendered.
type builder struct {
	key       string
	projectID string
	platform  string
	// project is the registry id of the project the document is keyed to, and
	// uuid.Nil for a pair the registry does not hold. The adjustment walk starts
	// at the project, and a pair the registry does not hold is one no relation
	// names.
	project uuid.UUID
	items   []LineItem
	related []related
}

// related is one related-costs entry and the pair of the project it bills,
// which is what the entries of a root are sorted by. The pair rather than the
// key it renders to: the escaping Key applies would order two entries by
// where their halves happen to carry a separator rather than by whose costs
// they are.
type related struct {
	key  ownerKey
	cost RelatedCost
}

// ownerOf is the pair's owner, created on first reach. A pair the registry does
// not hold is billed standalone under its raw id, an empty project id included:
// drafts that name no project are usage all the same.
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
		item = &lineItem{resource: resource}
		o.items[resource] = item
	}
	return item
}

// lineItems renders the owner's lines, sorted by platform, resource type, and
// resource id. The periods of a line keep the order their drafts came in, which
// is the order of the period they ran in.
func (o *owner) lineItems() []LineItem {
	items := make([]LineItem, 0, len(o.items))
	for _, item := range o.items {
		items = append(items, LineItem{
			ResourceType: item.resource.ResourceType,
			ResourceID:   item.resource.ResourceID,
			Platform:     item.resource.Platform,
			Description:  item.description,
			Periods:      item.periods,
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

// documentOf is the statement being built for the pair, created on first reach.
// The map is keyed on the pair rather than on the text it renders to, so what a
// document holds is decided before any text is joined: merging two pairs would
// put one customer's resource ids, states, hours and costs on the other's
// invoice while the project the document is named after is whichever of them
// was reached first. Key escapes both halves, so the two documents carry two
// keys as well.
//
// The project a document names is the one it is keyed to, so a root reached
// over an attributed project first is named after the root rather than after
// whoever arrived with it.
func documentOf(
	documents map[ownerKey]*builder,
	key ownerKey,
	projectID, platform string,
	project uuid.UUID,
) *builder {
	document, held := documents[key]
	if !held {
		document = &builder{
			key:       Key(key.cloud, key.projectID),
			projectID: projectID,
			platform:  platform,
			project:   project,
			items:     []LineItem{},
		}
		documents[key] = document
	}
	return document
}

// add puts one owner's lines on the statement. Only the project the statement
// is keyed to bills its lines here; every other one arrives as a related cost.
func (d *builder) add(items []LineItem) {
	d.items = append(d.items, items...)
}

// render marshals the statement. The lines and the related costs are already
// ordered among themselves, the related entries are ordered here, and the map
// keys of a period are ordered by encoding/json, so the bytes are a function of
// the input alone.
//
// The adjustments are applied to the sum of every line the statement bills, its
// own and the related ones, because that sum is what the customer is invoiced.
// A walk that collects nothing leaves the four adjustment members nil, and the
// document is the one an unadjusted build renders. A pair the registry does not
// hold is billed under its raw project id, which no relation names, so it is
// rendered unadjusted as well. A relation the walk reaches whose stored
// adjustments cannot be read fails the build, because the statement would
// otherwise be billed short of what that relation carries.
func (d *builder) render(
	billingPeriod BillingPeriod,
	currency string,
	adjuster *adjustments.Adjuster,
) (Statement, error) {
	slices.SortFunc(d.related, func(a, b related) int {
		return cmp.Or(
			cmp.Compare(a.key.cloud, b.key.cloud),
			cmp.Compare(a.key.projectID, b.key.projectID),
		)
	})

	document := Document{
		BillingPeriod: billingPeriod,
		ProjectID:     d.projectID,
		Platform:      d.platform,
		LineItems:     d.items,
		RelatedCosts:  make([]RelatedCost, 0, len(d.related)),
		Currency:      currency,
	}
	total := sum(d.items)
	for _, entry := range d.related {
		document.RelatedCosts = append(document.RelatedCosts, entry.cost)
		total = total.Add(entry.cost.Total.Decimal)
	}

	if adjuster != nil && d.project != uuid.Nil {
		outcome, err := adjuster.Adjust(d.project, bases(d.items, document.RelatedCosts))
		if err != nil {
			return Statement{}, fmt.Errorf("adjusting the statement of %s: %w", d.key, err)
		}
		if len(outcome.Lines) > 0 {
			baseCost := money.NewAmount(total)
			netCost := money.NewAmount(outcome.NetCost)
			kickbackTotal := money.NewAmount(outcome.KickbackTotal)
			document.BaseCost = &baseCost
			document.Adjustments = outcome.Lines
			document.NetCost = &netCost
			document.KickbackTotal = &kickbackTotal
			total = outcome.NetCost
		}
	}
	document.Total = money.NewAmount(total)

	body, err := json.Marshal(document)
	if err != nil {
		return Statement{}, fmt.Errorf("marshalling the statement of %s: %w", d.key, err)
	}
	return Statement{
		Key:         d.key,
		Document:    body,
		Total:       total,
		Currency:    currency,
		Adjustments: document.Adjustments,
	}, nil
}

// bases is every rated line the statement bills, its own and those of the
// projects attributed to it, as the amounts an adjustment is scoped against. A
// related line carries the platform and the resource type it was rated under,
// so an adjustment scoped to a platform reaches the lines of that platform
// wherever on the document they sit.
func bases(items []LineItem, related []RelatedCost) []adjustments.Base {
	collected := make([]adjustments.Base, 0, len(items))
	for _, item := range items {
		collected = append(collected, adjustments.Base{
			Platform:     item.Platform,
			ResourceType: item.ResourceType,
			Amount:       item.Total.Decimal,
		})
	}
	for _, cost := range related {
		for _, item := range cost.LineItems {
			collected = append(collected, adjustments.Base{
				Platform:     item.Platform,
				ResourceType: item.ResourceType,
				Amount:       item.Total.Decimal,
			})
		}
	}
	return collected
}

// periodOf renders one rated record and the draft it rates, and returns the
// record's total beside it. Every dimension the record was held against is
// emitted, one that cost nothing included, so a line shows what it was billed
// on rather than only what it was charged for.
func periodOf(record rating.RecordRating, draft metering.UsageDraft) (Period, decimal.Decimal, error) {
	usage := make(map[string]money.Quantity, len(record.Amounts))
	cost := make(map[string]money.Amount, len(record.Amounts)+1)
	total := decimal.Decimal{}

	for _, amount := range record.Amounts {
		if amount.Metric == costTotal {
			return Period{}, decimal.Decimal{}, fmt.Errorf(
				"the pricing dimension metric %q is reserved: a period holds the record's total under that key", amount.Metric)
		}
		usage[amount.Metric] = money.NewQuantity(amount.Quantity)
		cost[amount.Metric] = money.NewAmount(amount.Amount)
		total = total.Add(amount.Amount)
	}
	cost[costTotal] = money.NewAmount(total)

	return Period{
		State:         draft.State,
		Hours:         money.NewAmount(money.Round2(money.Div(money.Minutes(draft.Seconds), minutesPerHour))),
		Usage:         usage,
		Cost:          cost,
		StateModifier: money.NewQuantity(record.StateModifier),
	}, total, nil
}

// describe names a resource the way an invoice line reads. The description is
// mechanical: the flavor a resource was created from, or the type it carries,
// ahead of what it is, and what it is ahead of its id where it carries neither
// as text. Nothing here is read from the pricing model, so a line reads the
// same however the resource is priced.
//
// A line carries one description however many drafts it was built from, and
// the drafts of a resource do not have to agree: an instance resized
// mid-period is metered as one draft per flavor. The description of the last
// draft is the one the line shows, which is what the resource was at the end of
// the period being invoiced. The periods underneath it carry the quantities
// each of them was billed on, so what the earlier drafts were rated at is on
// the line whatever it is described as.
func describe(resource source.Resource, usage map[string]any) string {
	if flavor, text := usage[usageFlavor].(string); text {
		return fmt.Sprintf("%s %s", flavor, resource.ResourceType)
	}
	if kind, text := usage[usageType].(string); text {
		return fmt.Sprintf("%s %s", kind, resource.ResourceType)
	}
	return fmt.Sprintf("%s %s", resource.ResourceType, resource.ResourceID)
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

// Key joins a cloud and a project id into the key a statement is stored under.
// Both halves are escaped first, so the slash between them is the only
// separator the key holds and no two pairs render the same one: see
// Statement.Key. A credit note of a correction run is stored under the same
// key, so a project's credit note lands where its statement did.
func Key(cloud, projectID string) string {
	return fmt.Sprintf("%s/%s", url.PathEscape(cloud), url.PathEscape(projectID))
}

// ParseKey splits the key Key rendered back into the cloud and the project id
// it was built from: the run.json index of an export names both beside every
// statement file, which is all a stored key has to be read back for. Exactly
// one slash is required, because escaping both halves leaves the separator as
// the only slash a key holds. A key with none or with two was never rendered
// by Key, and guessing which slash separated it would name a pair nothing was
// ever stored under. An empty half is a key of its own: a draft that names no
// project is stored under an empty project half.
func ParseKey(key string) (cloud, projectID string, err error) {
	if n := strings.Count(key, "/"); n != 1 {
		return "", "", fmt.Errorf("the statement key %q is not cloud/project: it holds %d slashes, not one", key, n)
	}
	escapedCloud, escapedProject, _ := strings.Cut(key, "/")
	if cloud, err = url.PathUnescape(escapedCloud); err != nil {
		return "", "", fmt.Errorf("the statement key %q is not cloud/project: %w", key, err)
	}
	if projectID, err = url.PathUnescape(escapedProject); err != nil {
		return "", "", fmt.Errorf("the statement key %q is not cloud/project: %w", key, err)
	}
	return cloud, projectID, nil
}

// name identifies a resource in an error.
func name(resource source.Resource) string {
	return fmt.Sprintf("%s/%s/%s/%s",
		resource.Cloud, resource.Platform, resource.ResourceType, resource.ResourceID)
}

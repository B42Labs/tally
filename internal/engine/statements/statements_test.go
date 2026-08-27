package statements_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/adjustments"
	"github.com/b42labs/tally/internal/engine/attribution"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
)

// conceptModel is the part of pricing/2026-03.yaml the cases below bill
// against. They parse it rather than building a pricing.Model by hand, so what
// is rendered is what an operator's file yields.
const conceptModel = `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.001"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.09"
      state_modifiers:
        shelved: "0.0"
        shutoff: "0.5"
    volume:
      dimensions:
        - metric: "size_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.0001"
      type_modifiers:
        ssd: "1.0"
        hdd: "0.5"
  gardener:
    shoot:
      dimensions:
        - metric: "worker_count"
          type: "time_gauge"
          price_per_unit_hour: "0.10"
      state_modifiers:
        hibernated: "0.0"
`

// reservedModel prices a dimension under the name a period holds its record's
// total under. The schema takes any non-empty metric, so the name is refused
// where the document is rendered rather than where the model is imported.
const reservedModel = `version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"
pricing:
  openstack:
    instance:
      dimensions:
        - metric: "total"
          type: "counter"
          price_per_unit: "1"
`

// The two clouds the cases meter in. External ids are unique per cloud only, so
// every statement key carries the cloud its project lives in.
const (
	openstackCloud = "os-prod"
	gardenerCloud  = "gardener-prod"
)

// infrastructureTenant is the attributing relation type of the concept's
// example, and managedBy the relation type its pricing adjustments hang off.
const (
	infrastructureTenant = "infrastructure_tenant"
	managedBy            = "managed_by"
)

// adjustmentDepth is how many levels the adjustment walk of a case takes. The
// cases below name a partner one edge away, so any depth of at least one
// resolves them.
const adjustmentDepth = 3

// The period the cases bill: March 2026, the one the concept works through.
var (
	periodFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodTo   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
)

// mustParse parses a model the case expects to hold.
func mustParse(t *testing.T, document string) pricing.Model {
	t.Helper()

	model, _, err := pricing.Parse([]byte(document))
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	return model
}

// projectID derives a uuid from a number, so a case names its projects and its
// relations by that number and reads like the registry it mirrors.
func projectID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0000000a-0000-0000-0000-%012d", n))
}

func relationID(n int) uuid.UUID {
	return uuid.MustParse(fmt.Sprintf("0000000b-0000-0000-0000-%012d", n))
}

// project is registry entry n.
func project(n int, cloud, platform, externalID string) source.Project {
	return source.Project{
		ID:         projectID(n),
		Platform:   platform,
		Cloud:      cloud,
		ExternalID: externalID,
	}
}

// relation is edge n, from the project that attributes to the one it attributes
// away. The validity is left zero: Resolve is given the relations that overlap
// the period.
func relation(n, from, to int) source.Relation {
	return source.Relation{
		ID:           relationID(n),
		SourceID:     projectID(from),
		TargetID:     projectID(to),
		RelationType: infrastructureTenant,
	}
}

// relationWith is edge n of a given relation type, carrying the metadata a
// pricing adjustment lives in. Empty metadata is a relation that adjusts
// nothing, the way a relation created without one is stored.
func relationWith(n, from, to int, relationType, metadata string) source.Relation {
	edge := relation(n, from, to)
	edge.RelationType = relationType
	if metadata != "" {
		edge.Metadata = json.RawMessage(metadata)
	}
	return edge
}

// adjusterOver resolves the adjustments of the given relations, which the case
// expects to hold. A warning means the case built a relation it did not mean
// to, so it fails there rather than on the numbers it produces.
func adjusterOver(t *testing.T, relations []source.Relation, projects []source.Project) *adjustments.Adjuster {
	t.Helper()

	adjuster, warnings, err := adjustments.New(relations, projects, adjustmentDepth)
	if err != nil {
		t.Fatalf("adjustments.New() error = %v, want nil", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("adjustments.New() warnings = %+v, want none", warnings)
	}
	return adjuster
}

// resource is one candidate of the period.
func resource(cloud, platform, resourceType, resourceID string) source.Resource {
	return source.Resource{
		Cloud:        cloud,
		Platform:     platform,
		ResourceType: resourceType,
		ResourceID:   resourceID,
	}
}

// draft builds a usage draft the way metering builds one: the size fields of
// the interval, plus the minutes its seconds are worth and the count of one
// that prices a resource by its existence. The hours are what the case reads;
// the draft carries the seconds they are.
func draft(state, owner string, hours int64, size map[string]any) metering.UsageDraft {
	return draftSeconds(state, owner, hours*3600, size)
}

// draftSeconds is the same draft over a duration that is not whole hours, which
// is what an interval starting or ending between two full hours is metered as.
func draftSeconds(state, owner string, seconds int64, size map[string]any) metering.UsageDraft {
	usage := map[string]any{
		"minutes": money.NewQuantity(money.Minutes(seconds)),
		"count":   1,
	}
	maps.Copy(usage, size)

	return metering.UsageDraft{
		State:     state,
		ProjectID: owner,
		Seconds:   seconds,
		Usage:     usage,
	}
}

// m1Large is the flavor of the concept's example, spelled the way the payload
// envelope decodes a size: every JSON number is a float64.
func m1Large(size map[string]any) map[string]any {
	instance := map[string]any{
		"flavor":  "m1.large",
		"vcpus":   float64(4),
		"ram_gb":  float64(8),
		"disk_gb": float64(80),
	}
	maps.Copy(instance, size)
	return instance
}

// build renders one period the way a run does: the drafts are rated with the
// model and handed to Build with the registry they were metered against.
func build(
	t *testing.T,
	model pricing.Model,
	usage []metering.ResourceUsage,
	projects []source.Project,
	res attribution.Resolution,
) statements.BuildResult {
	t.Helper()

	return buildAdjusted(t, model, usage, projects, res, nil)
}

// buildAdjusted renders the same period with an adjuster over the project
// graph, which is what a run hands in once it has resolved the adjustments of
// the period.
func buildAdjusted(
	t *testing.T,
	model pricing.Model,
	usage []metering.ResourceUsage,
	projects []source.Project,
	res attribution.Resolution,
	adjuster *adjustments.Adjuster,
) statements.BuildResult {
	t.Helper()

	result, err := statements.Build(periodFrom, periodTo, usage, rating.Rate(model, usage), projects, res, adjuster)
	if err != nil {
		t.Fatalf("Build() error = %v, want nil", err)
	}
	return result
}

// keys is what the pass produced, in the order it produced it.
func keys(result statements.BuildResult) []string {
	found := make([]string, 0, len(result.Statements))
	for _, statement := range result.Statements {
		found = append(found, statement.Key)
	}
	return found
}

// statementOf is the statement under key, or a failed case.
func statementOf(t *testing.T, result statements.BuildResult, key string) statements.Statement {
	t.Helper()

	for _, statement := range result.Statements {
		if statement.Key == key {
			return statement
		}
	}
	t.Fatalf("no statement is keyed %q, got %v", key, keys(result))
	return statements.Statement{}
}

// documentOf reads a statement's document back. Going through the JSON holds
// the tags and the marshalled shape against what the concept fixes rather than
// asserting on the Go values the renderer happened to build.
func documentOf(t *testing.T, statement statements.Statement) statements.Document {
	t.Helper()

	var document statements.Document
	if err := json.Unmarshal(statement.Document, &document); err != nil {
		t.Fatalf("unmarshalling the document of %s: %v", statement.Key, err)
	}
	return document
}

// assertDecimal holds one value against the text it is expected to spell, by
// value, so 19.20 and 19.2 are the same amount.
func assertDecimal(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()

	if expected := decimal.RequireFromString(want); !got.Equal(expected) {
		t.Errorf("%s = %s, want %s", name, got, want)
	}
}

// assertCost holds one entry of a period's cost object against the amount it is
// expected to carry. A metric the object does not hold fails the case: a
// dimension is rendered whatever it cost.
func assertCost(t *testing.T, period statements.Period, metric, want string) {
	t.Helper()

	amount, held := period.Cost[metric]
	if !held {
		t.Errorf("cost %q is missing, want %s", metric, want)
		return
	}
	assertDecimal(t, "cost "+metric, amount.Decimal, want)
}

// assertUsage holds one entry of a period's usage object against the quantity
// it was rated from.
func assertUsage(t *testing.T, period statements.Period, metric, want string) {
	t.Helper()

	quantity, held := period.Usage[metric]
	if !held {
		t.Errorf("usage %q is missing, want %s", metric, want)
		return
	}
	assertDecimal(t, "usage "+metric, quantity.Decimal, want)
}

// assertAmount holds one of the document's optional amount members against the
// text it is expected to spell. A member the document does not carry fails the
// case: the four adjustment members are rendered together or not at all.
func assertAmount(t *testing.T, name string, got *money.Amount, want string) {
	t.Helper()

	if got == nil {
		t.Errorf("%s is missing, want %s", name, want)
		return
	}
	assertDecimal(t, name, got.Decimal, want)
}

// assertMemberOrder holds the marshalled document against the order the concept
// prints its members in. The order is a property of the bytes, so it is read
// off them: a decoded document carries the members in the order the Go type
// declares them whatever the bytes held. Each name is searched for behind the
// one ahead of it, so a name a nested object carries as well is not what the
// next one is placed against.
func assertMemberOrder(t *testing.T, body []byte, members ...string) {
	t.Helper()

	rest := body
	for _, member := range members {
		name := []byte(`"` + member + `":`)
		at := bytes.Index(rest, name)
		if at < 0 {
			t.Fatalf("the document holds no %q behind the members ahead of it:\n%s", member, body)
		}
		rest = rest[at+len(name):]
	}
}

// assertUnadjusted holds a statement against carrying no adjustment at all:
// none of the four document members, no member name in the bytes, and no line
// on the statement itself.
func assertUnadjusted(t *testing.T, statement statements.Statement) {
	t.Helper()

	if statement.Adjustments != nil {
		t.Errorf("Adjustments of %s = %+v, want none", statement.Key, statement.Adjustments)
	}
	document := documentOf(t, statement)
	if document.BaseCost != nil || document.Adjustments != nil ||
		document.NetCost != nil || document.KickbackTotal != nil {
		t.Errorf("the document of %s decodes adjustment members, want none:\n%s", statement.Key, statement.Document)
	}
	for _, member := range []string{"base_cost", "adjustments", "net_cost", "kickback_total"} {
		if bytes.Contains(statement.Document, []byte(`"`+member+`":`)) {
			t.Errorf("the document of %s renders %q, want it left out:\n%s", statement.Key, member, statement.Document)
		}
	}
}

// powerCycleUsage is the usage of the concept's worked example, one instance
// that ran for ten days, was shut off for ten and ran for the rest of March. It
// is rated at 128.45, which is what the adjustment cases start from.
func powerCycleUsage() []metering.ResourceUsage {
	return []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts: []metering.UsageDraft{
			draft("active", "proj-456", 240, m1Large(map[string]any{"egress_gb": float64(18.0)})),
			draft("shutoff", "proj-456", 240, m1Large(map[string]any{"egress_gb": float64(0)})),
			draft("active", "proj-456", 264, m1Large(map[string]any{"egress_gb": float64(22.5)})),
		},
	}}
}

// TestBuildRelatedCostsGolden renders the concept's related-costs example: a
// Gardener project billed for the management fee of its shoot, and the
// OpenStack tenant that shoot's workers run in attributed to it. The tenant
// gets no statement of its own, because attribution is exclusive.
func TestBuildRelatedCostsGolden(t *testing.T) {
	projects := []source.Project{
		project(1, gardenerCloud, "gardener", "team-alpha"),
		project(2, openstackCloud, "openstack", "shoot-abc-os-tenant"),
	}
	usage := []metering.ResourceUsage{
		{
			Resource: resource(gardenerCloud, "gardener", "shoot", "shoot-abc"),
			Drafts: []metering.UsageDraft{
				draft("active", "team-alpha", 744, map[string]any{"worker_count": float64(3)}),
			},
		},
		{
			Resource: resource(openstackCloud, "openstack", "instance", "worker-1"),
			Drafts: []metering.UsageDraft{
				draft("active", "shoot-abc-os-tenant", 744, map[string]any{
					"vcpus":   float64(8),
					"ram_gb":  float64(16),
					"disk_gb": float64(160),
				}),
			},
		},
	}

	res := attribution.Resolve(projects, []source.Relation{relation(1, 1, 2)})
	result := build(t, mustParse(t, conceptModel), usage, projects, res)

	if got := keys(result); !reflect.DeepEqual(got, []string{"gardener-prod/team-alpha"}) {
		t.Fatalf("statement keys = %v, want only gardener-prod/team-alpha: the tenant bills under it", got)
	}
	statement := result.Statements[0]
	assertDecimal(t, "Total", statement.Total, "520.80")
	if statement.Currency != "EUR" {
		t.Errorf("Currency = %q, want %q", statement.Currency, "EUR")
	}

	document := documentOf(t, statement)
	if document.ProjectID != "team-alpha" || document.Platform != "gardener" {
		t.Errorf("document names %s/%s, want team-alpha/gardener", document.ProjectID, document.Platform)
	}
	assertDecimal(t, "document total", document.Total.Decimal, "520.80")

	if len(document.LineItems) != 1 {
		t.Fatalf("LineItems = %d, want 1, the shoot", len(document.LineItems))
	}
	assertDecimal(t, "shoot total", document.LineItems[0].Total.Decimal, "223.20")
	assertCost(t, document.LineItems[0].Periods[0], "worker_count", "223.20")

	if len(document.RelatedCosts) != 1 {
		t.Fatalf("RelatedCosts = %d, want 1, the tenant", len(document.RelatedCosts))
	}
	related := document.RelatedCosts[0]
	if related.RelationType != infrastructureTenant {
		t.Errorf("RelationType = %q, want %q", related.RelationType, infrastructureTenant)
	}
	if related.ProjectID != "shoot-abc-os-tenant" || related.Platform != "openstack" {
		t.Errorf("related cost names %s/%s, want shoot-abc-os-tenant/openstack", related.ProjectID, related.Platform)
	}
	assertDecimal(t, "related total", related.Total.Decimal, "297.60")

	if len(related.LineItems) != 1 {
		t.Fatalf("related LineItems = %d, want 1, the worker", len(related.LineItems))
	}
	worker := related.LineItems[0].Periods[0]
	// The egress the worker never sent is rendered at zero rather than dropped,
	// so the line shows every dimension it was held against.
	for _, want := range []struct{ metric, amount string }{
		{"vcpus", "119.04"}, {"ram_gb", "59.52"}, {"disk_gb", "119.04"}, {"egress_gb", "0.00"}, {"total", "297.60"},
	} {
		assertCost(t, worker, want.metric, want.amount)
	}
}

// TestBuildPowerCycleDocument renders the concept's worked example: one
// instance that ran for ten days, was shut off for ten, and ran for the rest of
// March. The document is held against the fixture byte for byte, and its
// numbers against the ones the concept fixes, so the fixture cannot pass by
// agreeing with whatever the renderer produced.
func TestBuildPowerCycleDocument(t *testing.T) {
	projects := []source.Project{project(1, openstackCloud, "openstack", "proj-456")}
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts: []metering.UsageDraft{
			draft("active", "proj-456", 240, m1Large(map[string]any{"egress_gb": float64(18.0)})),
			draft("shutoff", "proj-456", 240, m1Large(map[string]any{"egress_gb": float64(0)})),
			draft("active", "proj-456", 264, m1Large(map[string]any{"egress_gb": float64(22.5)})),
		},
	}}

	result := build(t, mustParse(t, conceptModel), usage, projects, attribution.Resolve(projects, nil))

	if got := keys(result); !reflect.DeepEqual(got, []string{"os-prod/proj-456"}) {
		t.Fatalf("statement keys = %v, want only os-prod/proj-456", got)
	}
	statement := result.Statements[0]
	assertDecimal(t, "Total", statement.Total, "128.45")

	document := documentOf(t, statement)
	if len(document.LineItems) != 1 {
		t.Fatalf("LineItems = %d, want 1", len(document.LineItems))
	}
	item := document.LineItems[0]
	if item.Description != "m1.large instance" {
		t.Errorf("Description = %q, want %q", item.Description, "m1.large instance")
	}
	if len(item.Periods) != 3 {
		t.Fatalf("Periods = %d, want 3, one per draft", len(item.Periods))
	}

	periods := []struct {
		name     string
		state    string
		hours    string
		modifier string
		egress   string
		total    string
	}{
		{"active for 240 hours", "active", "240.00", "1", "1.62", "49.62"},
		{"shut off for 240 hours, at half the time_gauge price", "shutoff", "240.00", "0.5", "0.00", "24.00"},
		{"active for 264 hours, the egress rounded half away from zero", "active", "264.00", "1", "2.03", "54.83"},
	}
	for i, want := range periods {
		t.Run(want.name, func(t *testing.T) {
			period := item.Periods[i]
			if period.State != want.state {
				t.Errorf("State = %q, want %q", period.State, want.state)
			}
			assertDecimal(t, "Hours", period.Hours.Decimal, want.hours)
			assertDecimal(t, "StateModifier", period.StateModifier.Decimal, want.modifier)
			assertCost(t, period, "egress_gb", want.egress)
			assertCost(t, period, "total", want.total)
			assertUsage(t, period, "vcpus", "4")
		})
	}
	assertDecimal(t, "line item total", item.Total.Decimal, "128.45")
	assertDecimal(t, "document total", document.Total.Decimal, "128.45")

	// The fixture is the bytes Build produced: an amount is written at two
	// places and a quantity at four, and a JSON object's keys come out sorted.
	want, err := os.ReadFile(filepath.Join("testdata", "power_cycle.json"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	if !bytes.Equal(statement.Document, want) {
		t.Errorf("Document =\n%s\nwant\n%s", statement.Document, want)
	}
}

// TestBuildAdjustedDocument bills the concept's worked example through the
// reseller of Phase 5: proj-456 is managed by a partner whose relation carries
// a discount of 15 percent and a kickback of 10 percent on everything the
// period was rated at. The document shows what it was rated at, what each
// adjustment came to and what the customer pays, and its total is the net cost.
func TestBuildAdjustedDocument(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "proj-456"),
		project(2, "partner", "partner", "partner-corp"),
	}
	relations := []source.Relation{relationWith(1, 1, 2, managedBy, `{"pricing_adjustments":[
		{"type": "discount", "rate": "0.15", "scope": "all", "description": "reseller discount"},
		{"type": "kickback", "rate": "0.10", "scope": "all"}
	]}`)}

	result := buildAdjusted(t, mustParse(t, conceptModel), powerCycleUsage(), projects,
		attribution.Resolve(projects, nil), adjusterOver(t, relations, projects))

	statement := statementOf(t, result, "os-prod/proj-456")
	assertDecimal(t, "Total", statement.Total, "109.18")
	if len(statement.Adjustments) != 2 {
		t.Fatalf("Statement.Adjustments = %d, want 2, the discount and the kickback", len(statement.Adjustments))
	}

	document := documentOf(t, statement)
	assertAmount(t, "base_cost", document.BaseCost, "128.45")
	assertAmount(t, "net_cost", document.NetCost, "109.18")
	assertAmount(t, "kickback_total", document.KickbackTotal, "10.92")
	// The total is what the customer pays, so the kickback the partner is owed
	// sits beside it rather than in it.
	assertDecimal(t, "document total", document.Total.Decimal, "109.18")

	if len(document.Adjustments) != 2 {
		t.Fatalf("adjustments = %d, want 2", len(document.Adjustments))
	}
	lines := []struct {
		name        string
		kind        string
		rate        string
		base        string
		amount      string
		description string
	}{
		// 15 percent of the rated total is 19.2675, rounded half away from zero.
		{"the discount off the rated total", "discount", "0.15", "128.45", "-19.27", "reseller discount"},
		// The kickback is rated on the net cost, 10.918 of it, and leaves it be.
		{"the kickback off the net cost", "kickback", "0.10", "109.18", "10.92", ""},
	}
	for i, want := range lines {
		t.Run(want.name, func(t *testing.T) {
			line := document.Adjustments[i]
			if line.Type != want.kind {
				t.Errorf("type = %q, want %q", line.Type, want.kind)
			}
			if line.RelationType != managedBy || line.RelationTarget != "partner-corp" {
				t.Errorf("the line names %s/%s, want %s/partner-corp",
					line.RelationType, line.RelationTarget, managedBy)
			}
			if line.RelationID != relationID(1).String() {
				t.Errorf("relation_id = %q, want %q", line.RelationID, relationID(1))
			}
			if line.Scope != "all" {
				t.Errorf("scope = %q, want all", line.Scope)
			}
			if line.Description != want.description {
				t.Errorf("description = %q, want %q", line.Description, want.description)
			}
			assertDecimal(t, "rate", line.Rate.Decimal, want.rate)
			assertDecimal(t, "base", line.Base.Decimal, want.base)
			assertDecimal(t, "amount", line.Amount.Decimal, want.amount)
			// The statement carries the lines the document shows, which is what
			// the run stores as its adjustment records.
			if got := statement.Adjustments[i].Amount.StringFixed(2); got != want.amount {
				t.Errorf("Statement.Adjustments[%d] = %s, want %s", i, got, want.amount)
			}
		})
	}

	assertMemberOrder(t, statement.Document,
		"billing_period", "project_id", "platform", "line_items", "related_costs",
		"base_cost", "adjustments", "net_cost", "kickback_total", "total", "currency")
}

// TestBuildAdjustsRelatedCosts adjusts a statement over the costs of the
// project attributed to it. The workers of the shoot are billed on team-alpha's
// document as a related cost, so a discount scoped to the OpenStack instances
// reaches them: the customer pays one document, however many projects the lines
// under it were metered in.
//
// The second half hangs the same discount off the tenant's own relation. The
// walk starts at the project the statement is keyed to, which the tenant is
// not, so nothing reaches the document and it renders the bytes an unadjusted
// build renders.
func TestBuildAdjustsRelatedCosts(t *testing.T) {
	projects := []source.Project{
		project(1, gardenerCloud, "gardener", "team-alpha"),
		project(2, openstackCloud, "openstack", "shoot-abc-os-tenant"),
		project(3, "partner", "partner", "partner-corp"),
	}
	usage := []metering.ResourceUsage{
		{
			Resource: resource(gardenerCloud, "gardener", "shoot", "shoot-abc"),
			Drafts: []metering.UsageDraft{
				draft("active", "team-alpha", 744, map[string]any{"worker_count": float64(3)}),
			},
		},
		{
			Resource: resource(openstackCloud, "openstack", "instance", "worker-1"),
			Drafts: []metering.UsageDraft{
				draft("active", "shoot-abc-os-tenant", 744, map[string]any{
					"vcpus":   float64(8),
					"ram_gb":  float64(16),
					"disk_gb": float64(160),
				}),
			},
		},
	}
	const discount = `{"pricing_adjustments": [{"type": "discount", "rate": "0.10", "scope": "openstack.instance"}]}`

	model := mustParse(t, conceptModel)
	res := attribution.Resolve(projects, []source.Relation{relation(1, 1, 2)})

	adjusted := buildAdjusted(t, model, usage, projects, res,
		adjusterOver(t, []source.Relation{relationWith(2, 1, 3, managedBy, discount)}, projects))

	statement := statementOf(t, adjusted, "gardener-prod/team-alpha")
	assertDecimal(t, "Total", statement.Total, "491.04")
	document := documentOf(t, statement)
	assertAmount(t, "base_cost", document.BaseCost, "520.80")
	assertAmount(t, "net_cost", document.NetCost, "491.04")
	assertAmount(t, "kickback_total", document.KickbackTotal, "0.00")
	if len(document.Adjustments) != 1 {
		t.Fatalf("adjustments = %d, want 1, the discount", len(document.Adjustments))
	}
	// The shoot is out of scope and the tenant's worker is in it, although the
	// worker is billed here as a related cost rather than as a line of its own.
	assertDecimal(t, "base", document.Adjustments[0].Base.Decimal, "297.60")
	assertDecimal(t, "amount", document.Adjustments[0].Amount.Decimal, "-29.76")

	unreached := buildAdjusted(t, model, usage, projects, res,
		adjusterOver(t, []source.Relation{relationWith(2, 2, 3, managedBy, discount)}, projects))

	untouched := statementOf(t, unreached, "gardener-prod/team-alpha")
	assertDecimal(t, "Total of the statement the walk does not reach", untouched.Total, "520.80")
	assertUnadjusted(t, untouched)
	want := statementOf(t, build(t, model, usage, projects, res), "gardener-prod/team-alpha")
	if !bytes.Equal(untouched.Document, want.Document) {
		t.Errorf("Document =\n%s\nwant the bytes of a build without an adjuster\n%s",
			untouched.Document, want.Document)
	}
}

// TestBuildWithoutAdjustmentsIsByteIdentical renders the concept's worked
// example three ways that adjust nothing. A statement no adjustment reaches
// carries none of the four members, so the document is the fixture byte for
// byte and an operator reading it sees what Phase 3 renders.
func TestBuildWithoutAdjustmentsIsByteIdentical(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "proj-456"),
		project(2, "partner", "partner", "partner-corp"),
	}
	want, err := os.ReadFile(filepath.Join("testdata", "power_cycle.json"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}

	cases := []struct {
		name     string
		adjuster *adjustments.Adjuster
	}{
		{name: "no adjuster at all", adjuster: nil},
		{name: "an adjuster over no relations", adjuster: adjusterOver(t, nil, projects)},
		{
			name: "a relation that carries no metadata",
			adjuster: adjusterOver(t,
				[]source.Relation{relationWith(1, 1, 2, managedBy, "")}, projects),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := buildAdjusted(t, mustParse(t, conceptModel), powerCycleUsage(), projects,
				attribution.Resolve(projects, nil), tc.adjuster)

			statement := statementOf(t, result, "os-prod/proj-456")
			assertDecimal(t, "Total", statement.Total, "128.45")
			assertUnadjusted(t, statement)
			if !bytes.Equal(statement.Document, want) {
				t.Errorf("Document =\n%s\nwant\n%s", statement.Document, want)
			}
		})
	}
}

// TestBuildUnregisteredProjectIsNotAdjusted bills drafts under a project id no
// registry row matches. The pair is billed standalone, and there is no project
// to walk from: the discount of a registered project that carries the same
// external id in another cloud is not that pair's, and an external id is unique
// per cloud only.
func TestBuildUnregisteredProjectIsNotAdjusted(t *testing.T) {
	projects := []source.Project{
		project(1, gardenerCloud, "gardener", "proj-456"),
		project(2, "partner", "partner", "partner-corp"),
	}
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts:   []metering.UsageDraft{draft("active", "proj-456", 240, m1Large(nil))},
	}}
	relations := []source.Relation{relationWith(1, 1, 2, managedBy,
		`{"pricing_adjustments": [{"type": "discount", "rate": "0.5", "scope": "all"}]}`)}

	result := buildAdjusted(t, mustParse(t, conceptModel), usage, projects,
		attribution.Resolve(projects, nil), adjusterOver(t, relations, projects))

	if got := keys(result); !reflect.DeepEqual(got, []string{"os-prod/proj-456"}) {
		t.Fatalf("statement keys = %v, want only os-prod/proj-456, billed under its raw id", got)
	}
	unregistered := []statements.UnregisteredProject{
		{Cloud: openstackCloud, ProjectID: "proj-456", Resources: 1},
	}
	if !reflect.DeepEqual(result.Unregistered, unregistered) {
		t.Errorf("Unregistered = %+v, want %+v", result.Unregistered, unregistered)
	}
	assertDecimal(t, "Total", result.Statements[0].Total, "48.00")
	assertUnadjusted(t, result.Statements[0])
}

// TestBuildPartialHours renders drafts that did not run for whole hours. Hours
// is the one value the renderer derives, the minutes of the draft over sixty at
// two places, and both roundings on that path are what a period shorter than an
// hour reads off.
func TestBuildPartialHours(t *testing.T) {
	projects := []source.Project{project(1, openstackCloud, "openstack", "proj-456")}
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts: []metering.UsageDraft{
			draftSeconds("active", "proj-456", 5400, m1Large(nil)),
			draftSeconds("active", "proj-456", 100, m1Large(nil)),
		},
	}}

	result := build(t, mustParse(t, conceptModel), usage, projects, attribution.Resolve(projects, nil))

	document := documentOf(t, statementOf(t, result, "os-prod/proj-456"))
	periods := document.LineItems[0].Periods
	if len(periods) != 2 {
		t.Fatalf("Periods = %d, want 2, one per draft", len(periods))
	}
	assertDecimal(t, "Hours of an hour and a half", periods[0].Hours.Decimal, "1.50")
	// A hundred seconds are 1.6667 minutes, which is 0.0278 hours before the two
	// places an amount is rendered at.
	assertDecimal(t, "Hours of a hundred seconds", periods[1].Hours.Decimal, "0.03")
	if !bytes.Contains(result.Statements[0].Document, []byte(`"hours":0.03`)) {
		t.Errorf("Document = %s, want the hours of the second period rendered as 0.03",
			result.Statements[0].Document)
	}
}

// TestBuildResizedResourceDescription bills one resource whose drafts describe
// it differently, which is what an instance resized mid-period is metered as.
// The line carries one description, the last draft's, and every period it was
// billed on all the same.
func TestBuildResizedResourceDescription(t *testing.T) {
	projects := []source.Project{project(1, openstackCloud, "openstack", "proj-456")}
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts: []metering.UsageDraft{
			draft("active", "proj-456", 240, m1Large(map[string]any{"flavor": "m1.small", "vcpus": float64(1)})),
			draft("active", "proj-456", 240, m1Large(map[string]any{"flavor": "m1.large"})),
		},
	}}

	result := build(t, mustParse(t, conceptModel), usage, projects, attribution.Resolve(projects, nil))

	item := documentOf(t, statementOf(t, result, "os-prod/proj-456")).LineItems[0]
	if item.Description != "m1.large instance" {
		t.Errorf("Description = %q, want %q: the last draft describes the line", item.Description, "m1.large instance")
	}
	if len(item.Periods) != 2 {
		t.Fatalf("Periods = %d, want 2: both flavors are billed", len(item.Periods))
	}
	assertUsage(t, item.Periods[0], "vcpus", "1")
	assertUsage(t, item.Periods[1], "vcpus", "4")
}

// TestBuildUnregistered bills the drafts of project ids the registry does not
// hold. Each pair is billed standalone under its raw id and counted once per
// resource, and a draft that names no project at all is such a pair rather than
// an error.
func TestBuildUnregistered(t *testing.T) {
	usage := []metering.ResourceUsage{
		{
			Resource: resource(openstackCloud, "openstack", "instance", "ghost-1"),
			Drafts:   []metering.UsageDraft{draft("active", "ghost", 240, m1Large(nil))},
		},
		{
			Resource: resource(openstackCloud, "openstack", "instance", "ghost-2"),
			Drafts: []metering.UsageDraft{
				draft("active", "ghost", 120, m1Large(nil)),
				draft("shutoff", "ghost", 120, m1Large(nil)),
			},
		},
		{
			Resource: resource(openstackCloud, "openstack", "instance", "nameless-1"),
			Drafts:   []metering.UsageDraft{draft("active", "", 240, m1Large(nil))},
		},
	}

	result := build(t, mustParse(t, conceptModel), usage, nil, attribution.Resolution{})

	if got := keys(result); !reflect.DeepEqual(got, []string{"os-prod/", "os-prod/ghost"}) {
		t.Fatalf("statement keys = %v, want os-prod/ and os-prod/ghost", got)
	}
	want := []statements.UnregisteredProject{
		{Cloud: openstackCloud, ProjectID: "", Resources: 1},
		{Cloud: openstackCloud, ProjectID: "ghost", Resources: 2},
	}
	if !reflect.DeepEqual(result.Unregistered, want) {
		t.Errorf("Unregistered = %+v, want %+v: a resource counts once however many drafts it has", result.Unregistered, want)
	}

	// The pair has no registry row to take a platform from, so the statement
	// shows the one its resources ran on.
	ghost := documentOf(t, statementOf(t, result, "os-prod/ghost"))
	if ghost.ProjectID != "ghost" || ghost.Platform != "openstack" {
		t.Errorf("document names %s/%s, want ghost/openstack", ghost.ProjectID, ghost.Platform)
	}
	if len(ghost.LineItems) != 2 {
		t.Errorf("LineItems = %d, want 2, one per resource", len(ghost.LineItems))
	}
}

// TestBuildRootWithAttributedRecordsOnly renders a root whose own usage is
// empty. It gets a statement all the same, because the costs of the project
// attributed to it are billed nowhere else, and a second attributed project
// that consumed nothing adds no entry to it.
func TestBuildRootWithAttributedRecordsOnly(t *testing.T) {
	projects := []source.Project{
		project(1, gardenerCloud, "gardener", "team-alpha"),
		project(2, openstackCloud, "openstack", "tenant-a"),
		project(3, openstackCloud, "openstack", "tenant-b"),
	}
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "worker-1"),
		Drafts:   []metering.UsageDraft{draft("active", "tenant-a", 240, m1Large(nil))},
	}}

	res := attribution.Resolve(projects, []source.Relation{relation(1, 1, 2), relation(2, 1, 3)})
	result := build(t, mustParse(t, conceptModel), usage, projects, res)

	if got := keys(result); !reflect.DeepEqual(got, []string{"gardener-prod/team-alpha"}) {
		t.Fatalf("statement keys = %v, want only gardener-prod/team-alpha", got)
	}
	statement := result.Statements[0]
	// An empty line item list is rendered as an empty array rather than as null,
	// which is what the concept's example prints for related costs.
	if !bytes.Contains(statement.Document, []byte(`"line_items":[],`)) {
		t.Errorf("Document = %s, want an empty line_items array", statement.Document)
	}

	document := documentOf(t, statement)
	if len(document.LineItems) != 0 {
		t.Errorf("LineItems = %d, want none: the root metered nothing", len(document.LineItems))
	}
	if len(document.RelatedCosts) != 1 {
		t.Fatalf("RelatedCosts = %d, want 1: tenant-b has no records", len(document.RelatedCosts))
	}
	if document.RelatedCosts[0].ProjectID != "tenant-a" {
		t.Errorf("RelatedCosts[0] bills %q, want tenant-a", document.RelatedCosts[0].ProjectID)
	}
	assertDecimal(t, "Total", statement.Total, "48.00")
}

// TestBuildOrphanBilledStandalone bills a project that sits in a cycle among
// attributing relations. No root reaches it, so it is billed under itself
// rather than lost between its attributors.
func TestBuildOrphanBilledStandalone(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "cycle-a"),
		project(2, openstackCloud, "openstack", "cycle-b"),
	}
	res := attribution.Resolve(projects, []source.Relation{relation(1, 1, 2), relation(2, 2, 1)})
	orphans := 0
	for _, warning := range res.Warnings {
		if warning.Code == attribution.WarningCycle {
			orphans++
		}
	}
	if orphans != 2 || len(res.Attributed) != 0 {
		t.Fatalf("the cycle resolved to %d orphans and %d attributions, want 2 and 0",
			orphans, len(res.Attributed))
	}

	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts:   []metering.UsageDraft{draft("active", "cycle-a", 240, m1Large(nil))},
	}}

	result := build(t, mustParse(t, conceptModel), usage, projects, res)

	if got := keys(result); !reflect.DeepEqual(got, []string{"os-prod/cycle-a"}) {
		t.Fatalf("statement keys = %v, want only os-prod/cycle-a", got)
	}
	document := documentOf(t, result.Statements[0])
	if len(document.LineItems) != 1 || len(document.RelatedCosts) != 0 {
		t.Errorf("document holds %d line items and %d related costs, want 1 and 0",
			len(document.LineItems), len(document.RelatedCosts))
	}
}

// TestBuildUnknownRootBilledStandalone bills a project the resolution
// attributes to a root the registry does not hold. There is nothing to key a
// document to, so its costs stay on its own statement rather than on one named
// after nobody.
func TestBuildUnknownRootBilledStandalone(t *testing.T) {
	projects := []source.Project{project(2, openstackCloud, "openstack", "tenant-a")}
	res := attribution.Resolution{Attributed: map[uuid.UUID]attribution.Attribution{
		projectID(2): {Root: projectID(99), RelationType: infrastructureTenant},
	}}
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "worker-1"),
		Drafts:   []metering.UsageDraft{draft("active", "tenant-a", 240, m1Large(nil))},
	}}

	result := build(t, mustParse(t, conceptModel), usage, projects, res)

	if got := keys(result); !reflect.DeepEqual(got, []string{"os-prod/tenant-a"}) {
		t.Fatalf("statement keys = %v, want only os-prod/tenant-a: the root is not in the registry", got)
	}
	document := documentOf(t, result.Statements[0])
	if len(document.LineItems) != 1 || len(document.RelatedCosts) != 0 {
		t.Errorf("document holds %d line items and %d related costs, want 1 and 0",
			len(document.LineItems), len(document.RelatedCosts))
	}
	assertDecimal(t, "Total", result.Statements[0].Total, "48.00")
}

// TestBuildMidPeriodTransfer splits a resource that changed hands mid-period.
// A draft carries the project that owned the resource while it ran, so each
// project is billed for the periods it owned it and for nothing else.
func TestBuildMidPeriodTransfer(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "proj-old"),
		project(2, openstackCloud, "openstack", "proj-new"),
	}
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts: []metering.UsageDraft{
			draft("active", "proj-old", 120, m1Large(nil)),
			draft("active", "proj-new", 120, m1Large(nil)),
		},
	}}

	result := build(t, mustParse(t, conceptModel), usage, projects, attribution.Resolve(projects, nil))

	if got := keys(result); !reflect.DeepEqual(got, []string{"os-prod/proj-new", "os-prod/proj-old"}) {
		t.Fatalf("statement keys = %v, want one per owner", got)
	}
	for _, key := range []string{"os-prod/proj-new", "os-prod/proj-old"} {
		statement := statementOf(t, result, key)
		document := documentOf(t, statement)
		if len(document.LineItems) != 1 {
			t.Fatalf("%s: LineItems = %d, want 1", key, len(document.LineItems))
		}
		if got := len(document.LineItems[0].Periods); got != 1 {
			t.Errorf("%s: Periods = %d, want 1, the half it owned", key, got)
		}
		assertDecimal(t, key+" total", statement.Total, "24.00")
	}
}

// TestBuildSameExternalIDAcrossClouds bills two projects that share an external
// id in different clouds. An external id is unique per cloud only, so the pair
// a draft is billed to carries the cloud its resource was metered in: the two
// keep their own statement, their own platform and their own total.
func TestBuildSameExternalIDAcrossClouds(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "proj-456"),
		project(2, gardenerCloud, "gardener", "proj-456"),
	}
	usage := []metering.ResourceUsage{
		{
			Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
			Drafts:   []metering.UsageDraft{draft("active", "proj-456", 240, m1Large(nil))},
		},
		{
			Resource: resource(gardenerCloud, "gardener", "shoot", "shoot-abc"),
			Drafts: []metering.UsageDraft{
				draft("active", "proj-456", 240, map[string]any{"worker_count": float64(3)}),
			},
		},
	}

	result := build(t, mustParse(t, conceptModel), usage, projects, attribution.Resolve(projects, nil))

	want := []string{"gardener-prod/proj-456", "os-prod/proj-456"}
	if got := keys(result); !reflect.DeepEqual(got, want) {
		t.Fatalf("statement keys = %v, want %v: the id names two projects, one per cloud", got, want)
	}
	for _, tc := range []struct{ key, platform, total string }{
		{"gardener-prod/proj-456", "gardener", "72.00"},
		{"os-prod/proj-456", "openstack", "48.00"},
	} {
		statement := statementOf(t, result, tc.key)
		assertDecimal(t, tc.key+" total", statement.Total, tc.total)
		document := documentOf(t, statement)
		if document.Platform != tc.platform {
			t.Errorf("%s is billed on %q, want %q", tc.key, document.Platform, tc.platform)
		}
		if len(document.LineItems) != 1 {
			t.Errorf("%s: LineItems = %d, want 1, its own resource", tc.key, len(document.LineItems))
		}
	}
}

// TestBuildSeparatedStatementKeys bills two pairs whose unescaped keys would
// render the same. A slash is legal in a cloud name and in an external id, so
// os-prod paired with eu/acme joins to what os-prod/eu paired with acme joins
// to. Both halves are escaped, so each pair renders its own key. One key for
// two pairs would reach Persist as two rows the unique key over
// (run_id, project_id) refuses, with a duplicate-key error naming neither pair
// and no re-run of an immutable period that could get past it.
func TestBuildSeparatedStatementKeys(t *testing.T) {
	nestedCloud := openstackCloud + "/eu"
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "eu/acme"),
		project(2, nestedCloud, "openstack", "acme"),
	}
	usage := []metering.ResourceUsage{
		{
			Resource: resource(openstackCloud, "openstack", "instance", "claimant-1"),
			Drafts:   []metering.UsageDraft{draft("active", "eu/acme", 240, m1Large(nil))},
		},
		{
			Resource: resource(nestedCloud, "openstack", "instance", "neighbour-1"),
			Drafts:   []metering.UsageDraft{draft("active", "acme", 240, m1Large(nil))},
		},
	}

	result := build(t, mustParse(t, conceptModel), usage, projects, attribution.Resolve(projects, nil))

	want := []string{"os-prod%2Feu/acme", "os-prod/eu%2Facme"}
	if got := keys(result); !reflect.DeepEqual(got, want) {
		t.Fatalf("statement keys = %v, want %v: two pairs render two keys", got, want)
	}
	for _, tc := range []struct{ key, named string }{
		{"os-prod%2Feu/acme", "acme"},
		{"os-prod/eu%2Facme", "eu/acme"},
	} {
		document := documentOf(t, statementOf(t, result, tc.key))
		if document.ProjectID != tc.named {
			t.Errorf("%s names %q, want %q: each key is the one of its own pair", tc.key, document.ProjectID, tc.named)
		}
		if len(document.LineItems) != 1 {
			t.Errorf("the document of %s holds %d line items, want 1: neither pair bills on the other",
				tc.key, len(document.LineItems))
		}
		assertDecimal(t, tc.key+" total", statementOf(t, result, tc.key).Total, "48.00")
	}
}

// TestBuildZeroAmountDimensions renders a dimension the resource carries no
// quantity for. It is billed at zero and shown all the same, in both objects a
// period holds: a line short by a whole metric has to be visible.
func TestBuildZeroAmountDimensions(t *testing.T) {
	projects := []source.Project{project(1, openstackCloud, "openstack", "proj-456")}
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts:   []metering.UsageDraft{draft("active", "proj-456", 240, m1Large(nil))},
	}}

	result := build(t, mustParse(t, conceptModel), usage, projects, attribution.Resolve(projects, nil))

	document := documentOf(t, statementOf(t, result, "os-prod/proj-456"))
	period := document.LineItems[0].Periods[0]
	assertUsage(t, period, "egress_gb", "0")
	assertCost(t, period, "egress_gb", "0.00")
	assertCost(t, period, "total", "48.00")
}

// TestBuildEmptyInputs renders a period nothing was metered or rated in. There
// is nothing to bill, which is not an error.
func TestBuildEmptyInputs(t *testing.T) {
	cases := []struct {
		name  string
		usage []metering.ResourceUsage
		rated rating.Result
	}{
		{name: "nil", usage: nil, rated: rating.Result{}},
		{
			name:  "empty",
			usage: []metering.ResourceUsage{},
			rated: rating.Result{Currency: "EUR", Resources: []rating.ResourceRating{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := statements.Build(periodFrom, periodTo, tc.usage, tc.rated, nil, attribution.Resolution{}, nil)
			if err != nil {
				t.Fatalf("Build() error = %v, want nil", err)
			}
			if len(result.Statements) != 0 || len(result.Unregistered) != 0 {
				t.Errorf("Build() = %+v, want nothing billed", result)
			}
		})
	}
}

// TestBuildReservedTotalMetric refuses a model that prices a dimension under
// the name a period holds its record's total under. Rendering it would drop one
// of the two numbers a customer reconciles the line against.
func TestBuildReservedTotalMetric(t *testing.T) {
	projects := []source.Project{project(1, openstackCloud, "openstack", "proj-456")}
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts:   []metering.UsageDraft{draft("active", "proj-456", 240, m1Large(nil))},
	}}
	model := mustParse(t, reservedModel)

	_, err := statements.Build(periodFrom, periodTo, usage, rating.Rate(model, usage), projects,
		attribution.Resolve(projects, nil), nil)

	if err == nil {
		t.Fatalf("Build() error = nil, want the reserved metric refused")
	}
	if !strings.Contains(err.Error(), `"total"`) {
		t.Errorf("Build() error = %v, want the metric named", err)
	}
}

// TestBuildAlignmentErrors refuses rated input that does not line up with the
// usage it was rated from. Either way a statement would be short by a period,
// or hold one of another resource, with nothing to say so.
func TestBuildAlignmentErrors(t *testing.T) {
	instance := resource(openstackCloud, "openstack", "instance", "abc-123")
	record := rating.RecordRating{
		Amounts:       []rating.DimensionAmount{{Metric: "vcpus", Amount: decimal.NewFromInt(1), Quantity: decimal.NewFromInt(4)}},
		StateModifier: decimal.NewFromInt(1),
	}

	cases := []struct {
		name  string
		usage []metering.ResourceUsage
		rated rating.Result
	}{
		{
			name:  "the rated resource was never metered",
			usage: nil,
			rated: rating.Result{
				Currency:  "EUR",
				Resources: []rating.ResourceRating{{Resource: instance, Records: []rating.RecordRating{record}}},
			},
		},
		{
			name: "more records than drafts",
			usage: []metering.ResourceUsage{{
				Resource: instance,
				Drafts:   []metering.UsageDraft{draft("active", "proj-456", 240, m1Large(nil))},
			}},
			rated: rating.Result{
				Currency:  "EUR",
				Resources: []rating.ResourceRating{{Resource: instance, Records: []rating.RecordRating{record, record}}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := statements.Build(periodFrom, periodTo, tc.usage, tc.rated, nil, attribution.Resolution{}, nil)
			if err == nil {
				t.Fatalf("Build() error = nil, want the misaligned resource refused")
			}
			if !strings.Contains(err.Error(), "abc-123") {
				t.Errorf("Build() error = %v, want the resource named", err)
			}
		})
	}
}

// TestBuildDeterministicOrder renders a period whose input arrives in no
// particular order. The same period has to yield the same documents however
// often it is billed, so everything a document holds is ordered by what it is
// rather than by when it arrived.
func TestBuildDeterministicOrder(t *testing.T) {
	projects := []source.Project{
		project(1, gardenerCloud, "gardener", "team-alpha"),
		project(2, openstackCloud, "openstack", "tenant-b"),
		project(3, openstackCloud, "openstack", "tenant-a"),
		project(4, openstackCloud, "openstack", "proj-b"),
		project(5, openstackCloud, "openstack", "proj-a"),
	}
	relations := []source.Relation{relation(1, 1, 2), relation(2, 1, 3)}
	usage := []metering.ResourceUsage{
		{
			Resource: resource(openstackCloud, "openstack", "volume", "vol-9"),
			Drafts: []metering.UsageDraft{
				draft("in-use", "proj-a", 288, map[string]any{"size_gb": float64(200), "type": "hdd"}),
			},
		},
		{
			Resource: resource(openstackCloud, "openstack", "instance", "inst-b"),
			Drafts:   []metering.UsageDraft{draft("active", "proj-b", 240, m1Large(nil))},
		},
		{
			Resource: resource(openstackCloud, "openstack", "instance", "worker-b"),
			Drafts:   []metering.UsageDraft{draft("active", "tenant-b", 240, m1Large(nil))},
		},
		{
			Resource: resource(openstackCloud, "openstack", "instance", "inst-a"),
			// A flavor that is not text is no name to describe the resource by, so
			// the description falls through to what the resource is.
			Drafts: []metering.UsageDraft{draft("active", "proj-a", 240, m1Large(map[string]any{"flavor": float64(42)}))},
		},
		{
			Resource: resource(openstackCloud, "openstack", "instance", "worker-a"),
			Drafts:   []metering.UsageDraft{draft("active", "tenant-a", 240, m1Large(nil))},
		},
	}

	model := mustParse(t, conceptModel)
	res := attribution.Resolve(projects, relations)
	result := build(t, model, usage, projects, res)

	want := []string{"gardener-prod/team-alpha", "os-prod/proj-a", "os-prod/proj-b"}
	if got := keys(result); !reflect.DeepEqual(got, want) {
		t.Fatalf("statement keys = %v, want %v", got, want)
	}

	root := documentOf(t, statementOf(t, result, "gardener-prod/team-alpha"))
	if len(root.RelatedCosts) != 2 {
		t.Fatalf("RelatedCosts = %d, want 2", len(root.RelatedCosts))
	}
	if root.RelatedCosts[0].ProjectID != "tenant-a" || root.RelatedCosts[1].ProjectID != "tenant-b" {
		t.Errorf("related costs bill %q then %q, want tenant-a then tenant-b",
			root.RelatedCosts[0].ProjectID, root.RelatedCosts[1].ProjectID)
	}

	projA := documentOf(t, statementOf(t, result, "os-prod/proj-a"))
	if len(projA.LineItems) != 2 {
		t.Fatalf("LineItems = %d, want 2", len(projA.LineItems))
	}
	if projA.LineItems[0].ResourceID != "inst-a" || projA.LineItems[1].ResourceID != "vol-9" {
		t.Errorf("line items are %q then %q, want inst-a then vol-9",
			projA.LineItems[0].ResourceID, projA.LineItems[1].ResourceID)
	}
	if got := projA.LineItems[0].Description; got != "instance inst-a" {
		t.Errorf("Description = %q, want %q", got, "instance inst-a")
	}
	// A volume carries no flavor, so its type is what the line is described by.
	if got := projA.LineItems[1].Description; got != "hdd volume" {
		t.Errorf("Description = %q, want %q", got, "hdd volume")
	}

	again := build(t, model, usage, projects, res)
	for i, statement := range result.Statements {
		if !bytes.Equal(statement.Document, again.Statements[i].Document) {
			t.Errorf("the document of %s differs between two builds:\n%s\n%s",
				statement.Key, statement.Document, again.Statements[i].Document)
		}
	}
}

// TestParseKey reads a stored key back into the pair Key joined. The run.json
// index of an export names the cloud and the project id beside every statement
// file, and both are recovered from the key alone, so a half holding a slash
// or a percent has to survive the round trip. A key no Key ever rendered is
// refused rather than split at some slash anyway, which would name a pair
// nothing was stored under.
func TestParseKey(t *testing.T) {
	roundTrips := []struct {
		name      string
		cloud     string
		projectID string
	}{
		{name: "a pair holding neither slash nor percent", cloud: openstackCloud, projectID: "proj-456"},
		{name: "a cloud holding a slash", cloud: openstackCloud + "/a", projectID: "b"},
		{name: "a project id holding a slash", cloud: openstackCloud, projectID: "a/b"},
		{name: "both halves holding what escaping rewrites", cloud: "50%", projectID: "eu/acme"},
		{name: "the empty project id of a draft naming none", cloud: openstackCloud, projectID: ""},
	}
	for _, tc := range roundTrips {
		t.Run(tc.name, func(t *testing.T) {
			key := statements.Key(tc.cloud, tc.projectID)
			cloud, projectID, err := statements.ParseKey(key)
			if err != nil {
				t.Fatalf("ParseKey(%q) error = %v, want the pair it was built from", key, err)
			}
			if cloud != tc.cloud || projectID != tc.projectID {
				t.Errorf("ParseKey(%q) = %q, %q, want %q, %q", key, cloud, projectID, tc.cloud, tc.projectID)
			}
		})
	}

	refused := []struct {
		name string
		key  string
	}{
		{name: "no slash to separate two halves", key: "os-prod"},
		{name: "a second slash no escaping would have left", key: "a/b/c"},
		{name: "a half no unescaping reads", key: "os-prod/%zz"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			cloud, projectID, err := statements.ParseKey(tc.key)
			if err == nil {
				t.Fatalf("ParseKey(%q) = %q, %q, error = nil, want the key refused", tc.key, cloud, projectID)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("%q", tc.key)) {
				t.Errorf("ParseKey(%q) error = %v, want the key named", tc.key, err)
			}
		})
	}
}

// FuzzParseKeyRoundTrip holds ParseKey to the algebraic invariant it carries:
// every pair Key joined comes back out of it as that pair. Clouds and project
// ids come from a cloud registry rather than from the engine, so the pairs the
// two functions have to survive are not a list anybody wrote down: a project id
// that is literally %2F, one holding control bytes, and one that is not valid
// UTF-8 all reach Key, and the interaction between escaping both halves and
// escaping the joined key again is where a pair would come back as another one.
func FuzzParseKeyRoundTrip(f *testing.F) {
	f.Add("os-prod", "proj-456")
	f.Add("os-prod/a", "b")
	f.Add("50%", "eu/acme")
	f.Add("os-prod", "%2F")
	f.Add("", "")

	f.Fuzz(func(t *testing.T, cloud, projectID string) {
		key := statements.Key(cloud, projectID)
		gotCloud, gotProject, err := statements.ParseKey(key)
		if err != nil {
			t.Fatalf("ParseKey(%q) error = %v, want the pair Key joined", key, err)
		}
		if gotCloud != cloud || gotProject != projectID {
			t.Errorf("ParseKey(%q) = %q, %q, want %q, %q", key, gotCloud, gotProject, cloud, projectID)
		}
	})
}

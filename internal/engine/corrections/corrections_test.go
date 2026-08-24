package corrections_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/attribution"
	"github.com/b42labs/tally/internal/engine/corrections"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
)

// conceptModel is the part of pricing/2026-03.yaml the cases below bill
// against. They parse it rather than building a pricing.Model by hand, so what
// is diffed is what an operator's file yields.
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

// The two clouds the cases meter in. External ids are unique per cloud only, so
// every credit note key carries the cloud its project lives in.
const (
	openstackCloud = "os-prod"
	gardenerCloud  = "gardener-prod"
)

// infrastructureTenant is the attributing relation type of the concept's
// example.
const infrastructureTenant = "infrastructure_tenant"

// The period the cases correct: March 2026, the one the concept works through.
var (
	periodFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodTo   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
)

// correctsRunID is the finalized run the credit notes below correct.
var correctsRunID = uuid.MustParse("3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4")

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

// key is one key of the diff, spelled in the order D6 lists it.
func key(cloud, platform, resourceType, resourceID, projectID, dimension string) corrections.Key {
	return corrections.Key{
		Cloud:        cloud,
		Platform:     platform,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		ProjectID:    projectID,
		Dimension:    dimension,
	}
}

// delta is one difference of the diff, built from the two amounts as text so a
// case reads the numbers it credits.
func delta(k corrections.Key, old, current string) corrections.Delta {
	before := decimal.RequireFromString(old)
	after := decimal.RequireFromString(current)

	return corrections.Delta{Key: k, Old: before, New: after, Delta: after.Sub(before)}
}

// notes renders one correction the way a run does: the deltas are handed to
// BuildCreditNotes with the registry they were diffed against.
func notes(
	t *testing.T,
	deltas []corrections.Delta,
	projects []source.Project,
	res attribution.Resolution,
) corrections.BuildResult {
	t.Helper()

	result, err := corrections.BuildCreditNotes(periodFrom, periodTo, correctsRunID, "EUR", deltas, projects, res)
	if err != nil {
		t.Fatalf("BuildCreditNotes() error = %v, want nil", err)
	}
	return result
}

// keys is what the pass produced, in the order it produced it.
func keys(result corrections.BuildResult) []string {
	found := make([]string, 0, len(result.Statements))
	for _, statement := range result.Statements {
		found = append(found, statement.Key)
	}
	return found
}

// noteOf reads a credit note back. Going through the JSON holds the tags and
// the marshalled shape against what the correction is settled from rather than
// asserting on the Go values the renderer happened to build.
func noteOf(t *testing.T, statement statements.Statement) corrections.CreditNote {
	t.Helper()

	var note corrections.CreditNote
	if err := json.Unmarshal(statement.Document, &note); err != nil {
		t.Fatalf("unmarshalling the credit note of %s: %v", statement.Key, err)
	}
	return note
}

// assertDecimal holds one value against the text it is expected to spell, by
// value, so -9.60 and -9.6 are the same amount.
func assertDecimal(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()

	if expected := decimal.RequireFromString(want); !got.Equal(expected) {
		t.Errorf("%s = %s, want %s", name, got, want)
	}
}

// assertAmount holds one key of an amount map against the text it is expected
// to spell. A key the map does not hold fails the case.
func assertAmount(t *testing.T, amounts map[corrections.Key]decimal.Decimal, k corrections.Key, want string) {
	t.Helper()

	amount, held := amounts[k]
	if !held {
		t.Errorf("the %s amount of %s is missing, want %s", k.Dimension, k.ResourceID, want)
		return
	}
	assertDecimal(t, k.Dimension, amount, want)
}

// assertChange holds one dimension of a line item against the three amounts the
// credit note shows for it.
func assertChange(t *testing.T, item corrections.LineItem, dimension, old, current, difference string) {
	t.Helper()

	change, held := item.Dimensions[dimension]
	if !held {
		t.Errorf("dimension %q is missing, want a delta of %s", dimension, difference)
		return
	}
	assertDecimal(t, dimension+" old", change.Old.Decimal, old)
	assertDecimal(t, dimension+" new", change.New.Decimal, current)
	assertDecimal(t, dimension+" delta", change.Delta.Decimal, difference)
}

// lines names the line items of a credit note the way they are ordered.
func lines(items []corrections.LineItem) []string {
	found := make([]string, 0, len(items))
	for _, item := range items {
		found = append(found, fmt.Sprintf("%s/%s/%s", item.Platform, item.ResourceType, item.ResourceID))
	}
	return found
}

// TestAmountsPowerCycle sums the concept's re-metered example: the instance
// that ran for ten days, was shut off for ten, and ran for the rest of March.
// The three drafts reach one key per dimension, and the dimension the instance
// never used is summed at zero rather than left out, because a key the
// correction does not carry is a delta against the finalized run.
func TestAmountsPowerCycle(t *testing.T) {
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts: []metering.UsageDraft{
			draft("active", "proj-456", 240, m1Large(nil)),
			draft("shutoff", "proj-456", 240, m1Large(nil)),
			draft("active", "proj-456", 264, m1Large(nil)),
		},
	}}

	amounts, err := corrections.Amounts(usage, rating.Rate(mustParse(t, conceptModel), usage))
	if err != nil {
		t.Fatalf("Amounts() error = %v, want nil", err)
	}

	if len(amounts) != 4 {
		t.Fatalf("Amounts() = %d keys, want 4, one per dimension of the instance", len(amounts))
	}
	for _, want := range []struct{ dimension, amount string }{
		{"vcpus", "49.92"}, {"ram_gb", "24.96"}, {"disk_gb", "49.92"}, {"egress_gb", "0.00"},
	} {
		assertAmount(t, amounts, key(openstackCloud, "openstack", "instance", "abc-123", "proj-456", want.dimension),
			want.amount)
	}
}

// TestAmountsMidPeriodTransfer sums a resource two projects owned during the
// period. The amounts of a record are filed under the project its own draft
// named, so each owner reaches one key per dimension for the hours it held the
// resource: a resource that changed hands is billed to two customers, and an
// amount under the wrong one would be a delta against a line that never
// existed.
func TestAmountsMidPeriodTransfer(t *testing.T) {
	usage := []metering.ResourceUsage{{
		Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
		Drafts: []metering.UsageDraft{
			draft("active", "proj-a", 360, m1Large(nil)),
			draft("active", "proj-b", 384, m1Large(nil)),
		},
	}}

	amounts, err := corrections.Amounts(usage, rating.Rate(mustParse(t, conceptModel), usage))
	if err != nil {
		t.Fatalf("Amounts() error = %v, want nil", err)
	}

	if len(amounts) != 8 {
		t.Fatalf("Amounts() = %d keys, want 8, one per dimension for each of the two owners", len(amounts))
	}
	for _, want := range []struct{ owner, dimension, amount string }{
		{"proj-a", "vcpus", "28.80"},
		{"proj-a", "ram_gb", "14.40"},
		{"proj-a", "disk_gb", "28.80"},
		{"proj-a", "egress_gb", "0.00"},
		{"proj-b", "vcpus", "30.72"},
		{"proj-b", "ram_gb", "15.36"},
		{"proj-b", "disk_gb", "30.72"},
		{"proj-b", "egress_gb", "0.00"},
	} {
		assertAmount(t,
			amounts,
			key(openstackCloud, "openstack", "instance", "abc-123", want.owner, want.dimension),
			want.amount)
	}
}

// TestAmountsEmpty sums a pass that rated nothing. The map is empty and usable,
// so it diffs against the finalized run's amounts the way a period that billed
// nothing should: every key of the other side is credited whole.
func TestAmountsEmpty(t *testing.T) {
	amounts, err := corrections.Amounts(nil, rating.Result{})
	if err != nil {
		t.Fatalf("Amounts() error = %v, want nil", err)
	}
	if amounts == nil {
		t.Fatalf("Amounts() = nil, want an empty map")
	}
	if len(amounts) != 0 {
		t.Errorf("Amounts() = %d keys, want none", len(amounts))
	}
}

// TestAmountsMisaligned sums passes whose ratings and drafts do not line up.
// Both are errors naming the resource rather than sums built from whatever
// lines up: an amount filed under the wrong project would be a delta against a
// line that never existed.
func TestAmountsMisaligned(t *testing.T) {
	rated := rating.Result{
		Currency: "EUR",
		Resources: []rating.ResourceRating{{
			Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
			Records:  []rating.RecordRating{{}, {}},
		}},
	}

	t.Run("a rated resource nothing metered", func(t *testing.T) {
		amounts, err := corrections.Amounts(nil, rated)
		if amounts != nil {
			t.Errorf("Amounts() = %v, want no amounts beside the error", amounts)
		}
		want := "the rated resource os-prod/openstack/instance/abc-123 carries no metered usage"
		if err == nil || err.Error() != want {
			t.Fatalf("Amounts() error = %v, want %q", err, want)
		}
	})

	t.Run("two records over three drafts", func(t *testing.T) {
		usage := []metering.ResourceUsage{{
			Resource: resource(openstackCloud, "openstack", "instance", "abc-123"),
			Drafts: []metering.UsageDraft{
				draft("active", "proj-456", 240, m1Large(nil)),
				draft("shutoff", "proj-456", 240, m1Large(nil)),
				draft("active", "proj-456", 264, m1Large(nil)),
			},
		}}

		amounts, err := corrections.Amounts(usage, rated)
		if amounts != nil {
			t.Errorf("Amounts() = %v, want no amounts beside the error", amounts)
		}
		want := "the rated resource os-prod/openstack/instance/abc-123 carries 2 records for 3 usage drafts"
		if err == nil || err.Error() != want {
			t.Fatalf("Amounts() error = %v, want %q", err, want)
		}
	})
}

// TestDiff diffs two passes that disagree on some keys and agree on others. A
// key one side does not hold counts as zero there, a key both sides rated the
// same is left out, and the deltas come back in the order D6 lists the key in.
func TestDiff(t *testing.T) {
	shoot := key(gardenerCloud, "gardener", "shoot", "shoot-abc", "team-alpha", "worker_count")
	disk := key(openstackCloud, "openstack", "instance", "abc-123", "proj-456", "disk_gb")
	vcpus := key(openstackCloud, "openstack", "instance", "abc-123", "proj-456", "vcpus")
	transferred := key(openstackCloud, "openstack", "instance", "abc-123", "proj-999", "vcpus")
	second := key(openstackCloud, "openstack", "instance", "xyz-789", "proj-456", "vcpus")
	unchanged := key(openstackCloud, "openstack", "volume", "vol-1", "proj-456", "size_gb")
	free := key(openstackCloud, "openstack", "volume", "vol-2", "proj-456", "size_gb")

	// The keys go in unordered on both sides, so the order the deltas come back
	// in is the one Diff applies rather than the one they were written in.
	old := map[corrections.Key]decimal.Decimal{
		second:      decimal.RequireFromString("3.00"),
		unchanged:   decimal.RequireFromString("5.00"),
		vcpus:       decimal.RequireFromString("59.52"),
		free:        decimal.RequireFromString("0.00"),
		disk:        decimal.RequireFromString("12.00"),
		transferred: decimal.RequireFromString("1.00"),
	}
	current := map[corrections.Key]decimal.Decimal{
		unchanged:   decimal.RequireFromString("5.00"),
		transferred: decimal.RequireFromString("2.00"),
		shoot:       decimal.RequireFromString("7.50"),
		free:        decimal.RequireFromString("0.00"),
		vcpus:       decimal.RequireFromString("49.92"),
		second:      decimal.RequireFromString("4.00"),
	}

	got := corrections.Diff(old, current)

	order := make([]corrections.Key, 0, len(got))
	for _, difference := range got {
		order = append(order, difference.Key)
	}
	want := []corrections.Key{shoot, disk, vcpus, transferred, second}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("Diff() keys = %v, want %v", order, want)
	}

	cases := []struct {
		name                     string
		delta                    corrections.Delta
		old, current, difference string
	}{
		{"a resource the correction rates lower", got[2], "59.52", "49.92", "-9.60"},
		{"a key only the finalized run holds", got[1], "12.00", "0.00", "-12.00"},
		{"a key only the correction holds", got[0], "0.00", "7.50", "7.50"},
		{"a resource the correction rates higher", got[3], "1.00", "2.00", "1.00"},
	}
	for _, want := range cases {
		t.Run(want.name, func(t *testing.T) {
			assertDecimal(t, "Old", want.delta.Old, want.old)
			assertDecimal(t, "New", want.delta.New, want.current)
			assertDecimal(t, "Delta", want.delta.Delta, want.difference)
		})
	}
}

// TestDiffNothing diffs passes that agree. Nothing to correct is nil rather
// than an empty slice, so a caller writes no rows at all.
func TestDiffNothing(t *testing.T) {
	amounts := map[corrections.Key]decimal.Decimal{
		key(openstackCloud, "openstack", "instance", "abc-123", "proj-456", "vcpus"):  decimal.RequireFromString("59.52"),
		key(openstackCloud, "openstack", "instance", "abc-123", "proj-456", "ram_gb"): decimal.RequireFromString("0.00"),
	}

	t.Run("two passes that rated nothing", func(t *testing.T) {
		empty := map[corrections.Key]decimal.Decimal{}
		if got := corrections.Diff(empty, empty); got != nil {
			t.Errorf("Diff() = %v, want nil", got)
		}
	})

	t.Run("two passes that rated the same", func(t *testing.T) {
		if got := corrections.Diff(amounts, amounts); got != nil {
			t.Errorf("Diff() = %v, want nil", got)
		}
	})
}

// TestBuildCreditNotesPowerCycle renders the concept's correction: the instance
// finalized as active all March, re-metered with the shutoff the late events
// revealed. The note is held against the fixture byte for byte, and its numbers
// against the ones the concept fixes, so the fixture cannot pass by agreeing
// with whatever the renderer produced.
func TestBuildCreditNotesPowerCycle(t *testing.T) {
	projects := []source.Project{project(1, openstackCloud, "openstack", "proj-456")}
	instance := func(dimension string) corrections.Key {
		return key(openstackCloud, "openstack", "instance", "abc-123", "proj-456", dimension)
	}
	deltas := []corrections.Delta{
		delta(instance("vcpus"), "59.52", "49.92"),
		delta(instance("ram_gb"), "29.76", "24.96"),
		delta(instance("disk_gb"), "59.52", "49.92"),
	}

	result := notes(t, deltas, projects, attribution.Resolve(projects, nil))

	if got := keys(result); !reflect.DeepEqual(got, []string{"os-prod/proj-456"}) {
		t.Fatalf("credit note keys = %v, want only os-prod/proj-456", got)
	}
	if len(result.Unregistered) != 0 {
		t.Errorf("Unregistered = %v, want none: the project is registered", result.Unregistered)
	}
	statement := result.Statements[0]
	assertDecimal(t, "Total", statement.Total, "-24.00")
	if statement.Currency != "EUR" {
		t.Errorf("Currency = %q, want %q", statement.Currency, "EUR")
	}

	note := noteOf(t, statement)
	if note.CorrectsRunID != correctsRunID.String() {
		t.Errorf("CorrectsRunID = %q, want %q", note.CorrectsRunID, correctsRunID.String())
	}
	if note.RelatedCosts == nil {
		t.Errorf("RelatedCosts = null, want an empty list: nothing is attributed to the project")
	}
	if len(note.RelatedCosts) != 0 {
		t.Errorf("RelatedCosts = %v, want none", note.RelatedCosts)
	}
	if len(note.LineItems) != 1 {
		t.Fatalf("LineItems = %d, want 1, the instance", len(note.LineItems))
	}

	item := note.LineItems[0]
	assertChange(t, item, "vcpus", "59.52", "49.92", "-9.60")
	assertChange(t, item, "ram_gb", "29.76", "24.96", "-4.80")
	assertChange(t, item, "disk_gb", "59.52", "49.92", "-9.60")
	assertDecimal(t, "line item total", item.Total.Decimal, "-24.00")
	assertDecimal(t, "note total", note.Total.Decimal, "-24.00")

	// The fixture is the bytes BuildCreditNotes produced: an amount is written
	// at two places, and a JSON object's keys come out sorted.
	want, err := os.ReadFile(filepath.Join("testdata", "credit_note_power_cycle.json"))
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	if !bytes.Equal(statement.Document, want) {
		t.Errorf("Document =\n%s\nwant\n%s", statement.Document, want)
	}
}

// TestBuildCreditNotesAttributed credits the concept's related-costs example: a
// Gardener project and the OpenStack tenant its shoot's workers run in, both
// corrected downwards. The tenant gets no note of its own, because attribution
// is exclusive, and its deltas land on the root's note as related costs.
func TestBuildCreditNotesAttributed(t *testing.T) {
	projects := []source.Project{
		project(1, gardenerCloud, "gardener", "team-alpha"),
		project(2, openstackCloud, "openstack", "shoot-abc-os-tenant"),
	}
	worker := func(dimension string) corrections.Key {
		return key(openstackCloud, "openstack", "instance", "worker-1", "shoot-abc-os-tenant", dimension)
	}
	deltas := []corrections.Delta{
		delta(key(gardenerCloud, "gardener", "shoot", "shoot-abc", "team-alpha", "worker_count"), "223.20", "148.80"),
		delta(worker("vcpus"), "119.04", "59.52"),
		delta(worker("ram_gb"), "59.52", "29.76"),
	}

	t.Run("the tenant is credited on the root's note", func(t *testing.T) {
		res := attribution.Resolve(projects, []source.Relation{relation(1, 1, 2)})
		result := notes(t, deltas, projects, res)

		if got := keys(result); !reflect.DeepEqual(got, []string{"gardener-prod/team-alpha"}) {
			t.Fatalf("credit note keys = %v, want only gardener-prod/team-alpha: the tenant credits under it", got)
		}
		statement := result.Statements[0]
		assertDecimal(t, "Total", statement.Total, "-163.68")

		note := noteOf(t, statement)
		if note.ProjectID != "team-alpha" || note.Platform != "gardener" {
			t.Errorf("the note names %s/%s, want team-alpha/gardener", note.ProjectID, note.Platform)
		}
		assertDecimal(t, "note total", note.Total.Decimal, "-163.68")

		if len(note.LineItems) != 1 {
			t.Fatalf("LineItems = %d, want 1, the shoot", len(note.LineItems))
		}
		assertDecimal(t, "shoot total", note.LineItems[0].Total.Decimal, "-74.40")

		if len(note.RelatedCosts) != 1 {
			t.Fatalf("RelatedCosts = %d, want 1, the tenant", len(note.RelatedCosts))
		}
		related := note.RelatedCosts[0]
		if related.RelationType != infrastructureTenant {
			t.Errorf("RelationType = %q, want %q", related.RelationType, infrastructureTenant)
		}
		if related.ProjectID != "shoot-abc-os-tenant" || related.Platform != "openstack" {
			t.Errorf("the related cost names %s/%s, want shoot-abc-os-tenant/openstack",
				related.ProjectID, related.Platform)
		}
		assertDecimal(t, "related total", related.Total.Decimal, "-89.28")

		if len(related.LineItems) != 1 {
			t.Fatalf("related LineItems = %d, want 1, the worker", len(related.LineItems))
		}
		assertChange(t, related.LineItems[0], "vcpus", "119.04", "59.52", "-59.52")
		assertChange(t, related.LineItems[0], "ram_gb", "59.52", "29.76", "-29.76")
	})

	// The common shape of a correction: the workers of a Gardener shoot are
	// re-metered while the shoot's own worker_count is unchanged, so the root is
	// reached through the project attributed to it and through nothing else.
	t.Run("a root whose own resources changed nothing", func(t *testing.T) {
		res := attribution.Resolve(projects, []source.Relation{relation(1, 1, 2)})
		result := notes(t, []corrections.Delta{
			delta(worker("vcpus"), "119.04", "59.52"),
			delta(worker("ram_gb"), "59.52", "29.76"),
		}, projects, res)

		if got := keys(result); !reflect.DeepEqual(got, []string{"gardener-prod/team-alpha"}) {
			t.Fatalf("credit note keys = %v, want only gardener-prod/team-alpha", got)
		}
		statement := result.Statements[0]
		// An empty line item list is rendered as an empty array rather than as
		// null, which is what statements.Build renders one as.
		if !bytes.Contains(statement.Document, []byte(`"line_items":[],`)) {
			t.Errorf("Document = %s, want an empty line_items array", statement.Document)
		}
		assertDecimal(t, "Total", statement.Total, "-89.28")

		note := noteOf(t, statement)
		if len(note.LineItems) != 0 {
			t.Errorf("LineItems = %d, want none: the root's own resources changed nothing",
				len(note.LineItems))
		}
		if len(note.RelatedCosts) != 1 {
			t.Fatalf("RelatedCosts = %d, want 1, the tenant", len(note.RelatedCosts))
		}
		assertDecimal(t, "related total", note.RelatedCosts[0].Total.Decimal, "-89.28")
		// The whole of the note is what the tenant costs, so a root that metered
		// nothing itself is still credited for what was billed under it.
		assertDecimal(t, "note total", note.Total.Decimal, "-89.28")
	})

	t.Run("a root the registry does not hold", func(t *testing.T) {
		res := attribution.Resolution{
			Attributed: map[uuid.UUID]attribution.Attribution{
				projectID(2): {Root: projectID(99), RelationType: infrastructureTenant},
			},
		}
		result := notes(t, deltas, projects, res)

		want := []string{"gardener-prod/team-alpha", "os-prod/shoot-abc-os-tenant"}
		if got := keys(result); !reflect.DeepEqual(got, want) {
			t.Fatalf("credit note keys = %v, want %v: the tenant is credited under itself", got, want)
		}
		assertDecimal(t, "tenant total", result.Statements[1].Total, "-89.28")
	})
}

// TestBuildCreditNotesUnregistered credits deltas under a project id the
// registry does not hold. They are credited standalone under that raw id and
// counted once per resource, so money owed to somebody nobody registered is
// visible rather than dropped.
func TestBuildCreditNotesUnregistered(t *testing.T) {
	deltas := []corrections.Delta{
		delta(key(openstackCloud, "openstack", "instance", "ghost-2", "proj-unknown", "vcpus"), "5.00", "0.00"),
		delta(key(openstackCloud, "openstack", "instance", "ghost-1", "proj-unknown", "vcpus"), "10.00", "8.00"),
	}

	result := notes(t, deltas, nil, attribution.Resolution{})

	if got := keys(result); !reflect.DeepEqual(got, []string{"os-prod/proj-unknown"}) {
		t.Fatalf("credit note keys = %v, want only os-prod/proj-unknown", got)
	}
	want := []statements.UnregisteredProject{{Cloud: openstackCloud, ProjectID: "proj-unknown", Resources: 2}}
	if !reflect.DeepEqual(result.Unregistered, want) {
		t.Errorf("Unregistered = %v, want %v", result.Unregistered, want)
	}

	note := noteOf(t, result.Statements[0])
	if note.ProjectID != "proj-unknown" || note.Platform != "openstack" {
		t.Errorf("the note names %s/%s, want proj-unknown/openstack", note.ProjectID, note.Platform)
	}
	if got := lines(note.LineItems); !reflect.DeepEqual(got,
		[]string{"openstack/instance/ghost-1", "openstack/instance/ghost-2"}) {
		t.Errorf("line items = %v, want the two resources ordered by id", got)
	}
	assertDecimal(t, "note total", note.Total.Decimal, "-7.00")
}

// TestBuildCreditNotesNoDeltas renders a correction that found nothing. There
// is no document to write and no project to name, so the zero result comes back
// rather than an empty note per registered project.
func TestBuildCreditNotesNoDeltas(t *testing.T) {
	projects := []source.Project{project(1, openstackCloud, "openstack", "proj-456")}

	got, err := corrections.BuildCreditNotes(
		periodFrom, periodTo, correctsRunID, "EUR", nil, projects, attribution.Resolve(projects, nil))
	if err != nil {
		t.Fatalf("BuildCreditNotes() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got, corrections.BuildResult{}) {
		t.Errorf("BuildCreditNotes() = %v, want the zero result", got)
	}
}

// TestBuildCreditNotesMidPeriodTransfer credits one resource two projects owned
// during the period. Each of them is credited for what it owned, on a note of
// its own, the way the statement of the period billed each of them for its own
// drafts.
func TestBuildCreditNotesMidPeriodTransfer(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "proj-a"),
		project(2, openstackCloud, "openstack", "proj-b"),
	}
	deltas := []corrections.Delta{
		delta(key(openstackCloud, "openstack", "instance", "abc-123", "proj-a", "vcpus"), "19.20", "9.60"),
		delta(key(openstackCloud, "openstack", "instance", "abc-123", "proj-b", "vcpus"), "40.32", "40.00"),
	}

	result := notes(t, deltas, projects, attribution.Resolve(projects, nil))

	if got := keys(result); !reflect.DeepEqual(got, []string{"os-prod/proj-a", "os-prod/proj-b"}) {
		t.Fatalf("credit note keys = %v, want one per owner of the instance", got)
	}
	for i, want := range []struct{ key, total string }{
		{"os-prod/proj-a", "-9.60"},
		{"os-prod/proj-b", "-0.32"},
	} {
		t.Run(want.key, func(t *testing.T) {
			note := noteOf(t, result.Statements[i])
			if len(note.LineItems) != 1 {
				t.Fatalf("LineItems = %d, want 1, the instance", len(note.LineItems))
			}
			assertDecimal(t, "line item total", note.LineItems[0].Total.Decimal, want.total)
			assertDecimal(t, "note total", note.Total.Decimal, want.total)
		})
	}
}

// TestBuildCreditNotesOrder credits deltas that arrive unordered, across two
// clouds and several resources. The notes come back ordered by key and their
// lines by platform, resource type, and resource id, so the same deltas render
// the same bytes however they were handed in.
func TestBuildCreditNotesOrder(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "proj-a"),
		project(2, gardenerCloud, "gardener", "team-alpha"),
	}
	// The bucket names a second platform under the cloud its project lives in,
	// which one installation does not carry. It is here because the platform is
	// the first thing the lines are ordered by, and the order has to hold
	// whatever the deltas name.
	deltas := []corrections.Delta{
		delta(key(openstackCloud, "openstack", "volume", "vol-1", "proj-a", "size_gb"), "4.00", "3.00"),
		delta(key(gardenerCloud, "gardener", "shoot", "shoot-b", "team-alpha", "worker_count"), "10.00", "9.00"),
		delta(key(openstackCloud, "openstack", "instance", "abc-999", "proj-a", "vcpus"), "2.00", "1.00"),
		delta(key(openstackCloud, "ceph", "bucket", "bkt-1", "proj-a", "storage_gb"), "1.00", "0.50"),
		delta(key(gardenerCloud, "gardener", "shoot", "shoot-a", "team-alpha", "worker_count"), "10.00", "8.00"),
		delta(key(openstackCloud, "openstack", "instance", "abc-123", "proj-a", "vcpus"), "3.00", "1.00"),
	}

	result := notes(t, deltas, projects, attribution.Resolve(projects, nil))

	if got := keys(result); !reflect.DeepEqual(got, []string{"gardener-prod/team-alpha", "os-prod/proj-a"}) {
		t.Fatalf("credit note keys = %v, want them ordered by key", got)
	}

	gardener := lines(noteOf(t, result.Statements[0]).LineItems)
	if want := []string{"gardener/shoot/shoot-a", "gardener/shoot/shoot-b"}; !reflect.DeepEqual(gardener, want) {
		t.Errorf("the lines of team-alpha = %v, want %v", gardener, want)
	}
	openstack := lines(noteOf(t, result.Statements[1]).LineItems)
	want := []string{
		"ceph/bucket/bkt-1",
		"openstack/instance/abc-123",
		"openstack/instance/abc-999",
		"openstack/volume/vol-1",
	}
	if !reflect.DeepEqual(openstack, want) {
		t.Errorf("the lines of proj-a = %v, want %v", openstack, want)
	}
}

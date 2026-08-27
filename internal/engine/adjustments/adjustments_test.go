package adjustments_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/adjustment"
	"github.com/b42labs/tally/internal/engine/adjustments"
	"github.com/b42labs/tally/internal/engine/source"
)

// The clouds of the registry the cases mirror. A partner and a meta-project
// carry their platform as their cloud (decision D1).
const (
	openstackCloud = "os-prod"
	partnerCloud   = "partner"
	metaCloud      = "meta"
)

// The two relation types that carry adjustments.
const (
	managedBy = "managed_by"
	memberOf  = "member_of"
)

// projectID and relationID derive a uuid from a number, so a case names its
// projects and its relations by that number and the ascending order of the
// numbers is the ascending order of the ids. Relations reach New in that order,
// which is what orders two adjustments of the same type.
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

// relation is edge n, from the adjusted project to the one whose metadata
// adjusts it. The validity is left zero: New is given the relations that
// overlap the period (decision D4).
func relation(n, from, to int, relationType, metadata string) source.Relation {
	edge := source.Relation{
		ID:           relationID(n),
		SourceID:     projectID(from),
		TargetID:     projectID(to),
		RelationType: relationType,
	}
	if metadata != "" {
		edge.Metadata = json.RawMessage(metadata)
	}
	return edge
}

// metadata is the relation metadata document that carries the given elements.
func metadata(elements ...string) string {
	return `{"pricing_adjustments":[` + strings.Join(elements, ",") + `]}`
}

// element is one adjustment of that array.
func element(adjustmentType, rate, scope string) string {
	return fmt.Sprintf(`{"type":%q,"rate":%q,"scope":%q}`, adjustmentType, rate, scope)
}

// base is one rated line of the statement being adjusted.
func base(platform, resourceType, amount string) adjustments.Base {
	return adjustments.Base{
		Platform:     platform,
		ResourceType: resourceType,
		Amount:       decimal.RequireFromString(amount),
	}
}

// newAdjuster builds the adjuster of a case that expects neither warnings nor
// an error.
func newAdjuster(t *testing.T, relations []source.Relation, projects []source.Project, depth int) *adjustments.Adjuster {
	t.Helper()

	adjuster, warnings, err := adjustments.New(relations, projects, depth)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("New() warnings = %+v, want none", warnings)
	}
	return adjuster
}

// adjusted is the outcome of one Adjust a case expects to succeed.
func adjusted(
	t *testing.T,
	adjuster *adjustments.Adjuster,
	project uuid.UUID,
	bases []adjustments.Base,
) adjustments.Outcome {
	t.Helper()

	outcome, err := adjuster.Adjust(project, bases)
	if err != nil {
		t.Fatalf("Adjust() error = %v, want nil", err)
	}
	return outcome
}

// assertDecimal holds one decimal against the amount expected of it, at the two
// places money is rounded and rendered at.
func assertDecimal(t *testing.T, name string, got decimal.Decimal, want string) {
	t.Helper()

	if !got.Equal(decimal.RequireFromString(want)) {
		t.Errorf("%s = %v, want %v", name, got.StringFixed(2), want)
	}
}

// assertLine holds one line against everything but its relation type, which
// only the cases that give their edges different types assert.
func assertLine(
	t *testing.T, got adjustments.Line,
	adjustmentType, relationTarget string, relation uuid.UUID,
	scope, rate, lineBase, amount string,
) {
	t.Helper()

	if got.Type != adjustmentType {
		t.Errorf("Type = %v, want %v", got.Type, adjustmentType)
	}
	if got.RelationTarget != relationTarget {
		t.Errorf("RelationTarget = %v, want %v", got.RelationTarget, relationTarget)
	}
	if got.RelationID != relation.String() {
		t.Errorf("RelationID = %v, want %v", got.RelationID, relation)
	}
	if got.Scope != scope {
		t.Errorf("Scope = %v, want %v", got.Scope, scope)
	}
	if !got.Rate.Equal(decimal.RequireFromString(rate)) {
		t.Errorf("Rate = %v, want %v", got.Rate, rate)
	}
	assertDecimal(t, "Base", got.Base.Decimal, lineBase)
	assertDecimal(t, "Amount", got.Amount.Decimal, amount)
}

// assertLineCount holds the number of lines against the number expected, and
// ends the case where they differ: every further assertion indexes into them.
func assertLineCount(t *testing.T, outcome adjustments.Outcome, want int) {
	t.Helper()

	if len(outcome.Lines) != want {
		t.Fatalf("Lines = %d lines (%+v), want %d", len(outcome.Lines), outcome.Lines, want)
	}
}

// resellerGraph is the registry of the concept's example: one OpenStack project
// managed by one partner.
func resellerGraph() []source.Project {
	return []source.Project{
		project(1, openstackCloud, "openstack", "customer-proj-1"),
		project(2, partnerCloud, "partner", "partner-corp"),
	}
}

// TestAdjustResellerRelation bills the concept's reseller case: a discount the
// customer sees and a kickback the partner is owed, both on the same relation.
func TestAdjustResellerRelation(t *testing.T) {
	relations := []source.Relation{
		relation(1, 1, 2, managedBy, metadata(
			element("discount", "0.15", "all"),
			element("kickback", "0.10", "all"),
		)),
	}
	adjuster := newAdjuster(t, relations, resellerGraph(), 3)

	outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
		base("openstack", "instance", "1200.00"),
	})

	assertDecimal(t, "BaseCost", outcome.BaseCost, "1200.00")
	assertLineCount(t, outcome, 2)
	assertLine(t, outcome.Lines[0], "discount", "partner-corp", relationID(1), "all", "0.15", "1200.00", "-180.00")
	if outcome.Lines[0].RelationType != managedBy {
		t.Errorf("RelationType = %v, want %v", outcome.Lines[0].RelationType, managedBy)
	}
	assertLine(t, outcome.Lines[1], "kickback", "partner-corp", relationID(1), "all", "0.10", "1020.00", "102.00")
	assertDecimal(t, "NetCost", outcome.NetCost, "1020.00")
	assertDecimal(t, "KickbackTotal", outcome.KickbackTotal, "102.00")
}

// TestAdjustStacksDiscountsMultiplicatively holds the second discount against
// what the first one left, not against the base.
func TestAdjustStacksDiscountsMultiplicatively(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "customer-proj-1"),
		project(2, partnerCloud, "partner", "partner-corp"),
		project(3, partnerCloud, "partner", "partner-two"),
	}
	relations := []source.Relation{
		relation(1, 1, 2, managedBy, metadata(element("discount", "0.10", "all"))),
		relation(2, 1, 3, managedBy, metadata(element("discount", "0.15", "all"))),
	}
	adjuster := newAdjuster(t, relations, projects, 3)

	outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
		base("openstack", "instance", "100.00"),
	})

	assertLineCount(t, outcome, 2)
	assertLine(t, outcome.Lines[0], "discount", "partner-corp", relationID(1), "all", "0.10", "100.00", "-10.00")
	assertLine(t, outcome.Lines[1], "discount", "partner-two", relationID(2), "all", "0.15", "90.00", "-13.50")
	assertDecimal(t, "NetCost", outcome.NetCost, "76.50")
}

// TestAdjustAppliesTypesInFixedOrder applies the three types of one relation in
// the order of decision D3, whatever order the array lists them in.
func TestAdjustAppliesTypesInFixedOrder(t *testing.T) {
	relations := []source.Relation{
		relation(1, 1, 2, managedBy, metadata(
			element("kickback", "0.10", "all"),
			element("discount", "0.15", "all"),
			element("surcharge", "0.10", "all"),
		)),
	}
	adjuster := newAdjuster(t, relations, resellerGraph(), 3)

	outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
		base("openstack", "instance", "1000.00"),
	})

	assertLineCount(t, outcome, 3)
	assertLine(t, outcome.Lines[0], "surcharge", "partner-corp", relationID(1), "all", "0.10", "1000.00", "100.00")
	assertLine(t, outcome.Lines[1], "discount", "partner-corp", relationID(1), "all", "0.15", "1100.00", "-165.00")
	assertLine(t, outcome.Lines[2], "kickback", "partner-corp", relationID(1), "all", "0.10", "935.00", "93.50")
	assertDecimal(t, "NetCost", outcome.NetCost, "935.00")
	assertDecimal(t, "KickbackTotal", outcome.KickbackTotal, "93.50")
}

// TestAdjustAddsSurchargesOnTheBase rates the second surcharge on the base the
// first one was rated on, so the two add rather than compound.
func TestAdjustAddsSurchargesOnTheBase(t *testing.T) {
	relations := []source.Relation{
		relation(1, 1, 2, managedBy, metadata(
			element("surcharge", "0.10", "all"),
			element("surcharge", "0.05", "all"),
		)),
	}
	adjuster := newAdjuster(t, relations, resellerGraph(), 3)

	outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
		base("openstack", "instance", "100.00"),
	})

	assertLineCount(t, outcome, 2)
	assertLine(t, outcome.Lines[0], "surcharge", "partner-corp", relationID(1), "all", "0.10", "100.00", "10.00")
	assertLine(t, outcome.Lines[1], "surcharge", "partner-corp", relationID(1), "all", "0.05", "100.00", "5.00")
	assertDecimal(t, "NetCost", outcome.NetCost, "115.00")
}

// TestAdjustAppliesProjectDiscountAfterDiscount orders the types rather than
// the relations: the project discount of the smaller relation id still applies
// after the discount of the larger one.
func TestAdjustAppliesProjectDiscountAfterDiscount(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "customer-proj-1"),
		project(2, partnerCloud, "partner", "partner-corp"),
		project(3, metaCloud, "meta", "customer-alpha"),
	}
	relations := []source.Relation{
		relation(1, 1, 3, memberOf, metadata(element("project_discount", "0.05", "all"))),
		relation(2, 1, 2, managedBy, metadata(element("discount", "0.10", "all"))),
	}
	adjuster := newAdjuster(t, relations, projects, 3)

	outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
		base("openstack", "instance", "100.00"),
	})

	assertLineCount(t, outcome, 2)
	assertLine(t, outcome.Lines[0], "discount", "partner-corp", relationID(2), "all", "0.10", "100.00", "-10.00")
	assertLine(t, outcome.Lines[1], "project_discount", "customer-alpha", relationID(1), "all", "0.05", "90.00", "-4.50")
	assertDecimal(t, "NetCost", outcome.NetCost, "85.50")
}

// TestAdjustPartitionsByScope rates an adjustment on the buckets its scope
// covers, which may be one of them, all of them, or none.
func TestAdjustPartitionsByScope(t *testing.T) {
	bases := []adjustments.Base{
		base("openstack", "instance", "100.00"),
		base("openstack", "volume", "50.00"),
	}
	cases := []struct {
		name     string
		scope    string
		lineBase string
		amount   string
		net      string
	}{
		{name: "one resource type", scope: "openstack.instance", lineBase: "100.00", amount: "-20.00", net: "130.00"},
		{name: "a whole platform", scope: "openstack", lineBase: "150.00", amount: "-30.00", net: "120.00"},
		{name: "a platform the statement has nothing of", scope: "gardener", lineBase: "0.00", amount: "0.00", net: "150.00"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			relations := []source.Relation{
				relation(1, 1, 2, managedBy, metadata(element("discount", "0.20", testCase.scope))),
			}
			adjuster := newAdjuster(t, relations, resellerGraph(), 3)

			outcome := adjusted(t, adjuster, projectID(1), bases)

			assertLineCount(t, outcome, 1)
			assertLine(t, outcome.Lines[0], "discount", "partner-corp", relationID(1),
				testCase.scope, "0.20", testCase.lineBase, testCase.amount)
			assertDecimal(t, "NetCost", outcome.NetCost, testCase.net)
		})
	}
}

// TestAdjustApportionsOverBuckets rounds one line once and spreads it over the
// buckets it covers, the last bucket taking the remainder. What the buckets
// took is read off the base of a second discount scoped to one of them.
func TestAdjustApportionsOverBuckets(t *testing.T) {
	bases := []adjustments.Base{
		base("openstack", "instance", "33.33"),
		base("openstack", "volume", "33.33"),
		base("gardener", "shoot", "33.33"),
	}
	cases := []struct {
		name       string
		scope      string
		secondBase string
	}{
		{name: "the first bucket in bucket order", scope: "gardener", secondBase: "30.00"},
		{name: "the second bucket in bucket order", scope: "openstack.instance", secondBase: "30.00"},
		{name: "the last bucket, which took the remainder", scope: "openstack.volume", secondBase: "29.99"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			relations := []source.Relation{
				relation(1, 1, 2, managedBy, metadata(element("discount", "0.10", "all"))),
				relation(2, 1, 2, managedBy, metadata(element("discount", "0.10", testCase.scope))),
			}
			adjuster := newAdjuster(t, relations, resellerGraph(), 3)

			outcome := adjusted(t, adjuster, projectID(1), bases)

			assertLineCount(t, outcome, 2)
			assertLine(t, outcome.Lines[0], "discount", "partner-corp", relationID(1), "all", "0.10", "99.99", "-10.00")
			assertLine(t, outcome.Lines[1], "discount", "partner-corp", relationID(2),
				testCase.scope, "0.10", testCase.secondBase, "-3.00")
			assertDecimal(t, "NetCost", outcome.NetCost, "86.99")
		})
	}

	t.Run("the discount alone", func(t *testing.T) {
		relations := []source.Relation{
			relation(1, 1, 2, managedBy, metadata(element("discount", "0.10", "all"))),
		}
		adjuster := newAdjuster(t, relations, resellerGraph(), 3)

		outcome := adjusted(t, adjuster, projectID(1), bases)

		assertDecimal(t, "BaseCost", outcome.BaseCost, "99.99")
		assertLineCount(t, outcome, 1)
		assertLine(t, outcome.Lines[0], "discount", "partner-corp", relationID(1), "all", "0.10", "99.99", "-10.00")
		assertDecimal(t, "NetCost", outcome.NetCost, "89.99")
	})
}

// TestAdjustKickbackLeavesTheNetAlone emits what the partner is owed without
// taking it off what the customer pays.
func TestAdjustKickbackLeavesTheNetAlone(t *testing.T) {
	relations := []source.Relation{
		relation(1, 1, 2, managedBy, metadata(element("kickback", "0.10", "all"))),
	}
	adjuster := newAdjuster(t, relations, resellerGraph(), 3)

	outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
		base("openstack", "instance", "100.00"),
	})

	assertLineCount(t, outcome, 1)
	assertLine(t, outcome.Lines[0], "kickback", "partner-corp", relationID(1), "all", "0.10", "100.00", "10.00")
	assertDecimal(t, "NetCost", outcome.NetCost, "100.00")
	assertDecimal(t, "KickbackTotal", outcome.KickbackTotal, "10.00")
}

// TestAdjustLimitsTheWalkDepth inherits a discount over two hops, which a walk
// of one level does not reach.
func TestAdjustLimitsTheWalkDepth(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "customer-proj-1"),
		project(2, metaCloud, "meta", "customer-alpha"),
		project(3, metaCloud, "meta", "group-north"),
	}
	relations := []source.Relation{
		relation(1, 1, 2, memberOf, ""),
		relation(2, 2, 3, memberOf, metadata(element("project_discount", "0.05", "all"))),
	}
	cases := []struct {
		name  string
		depth int
		lines int
		net   string
	}{
		{name: "one level short of the discount", depth: 1, lines: 0, net: "100.00"},
		{name: "the level the discount sits on", depth: 2, lines: 1, net: "95.00"},
		{name: "one level past the discount", depth: 3, lines: 1, net: "95.00"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			adjuster := newAdjuster(t, relations, projects, testCase.depth)

			outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
				base("openstack", "instance", "100.00"),
			})

			assertLineCount(t, outcome, testCase.lines)
			if testCase.lines > 0 {
				assertLine(t, outcome.Lines[0], "project_discount", "group-north", relationID(2),
					"all", "0.05", "100.00", "-5.00")
			}
			assertDecimal(t, "NetCost", outcome.NetCost, testCase.net)
		})
	}
}

// TestAdjustVisitsEveryRelationOnce applies the discount of a relation two
// paths reach once (decision D6).
func TestAdjustVisitsEveryRelationOnce(t *testing.T) {
	projects := []source.Project{
		project(1, openstackCloud, "openstack", "customer-proj-1"),
		project(2, metaCloud, "meta", "team-north"),
		project(3, metaCloud, "meta", "team-south"),
		project(4, metaCloud, "meta", "customer-alpha"),
		project(5, partnerCloud, "partner", "partner-corp"),
	}
	relations := []source.Relation{
		relation(1, 1, 2, memberOf, ""),
		relation(2, 1, 3, memberOf, ""),
		relation(3, 2, 4, memberOf, ""),
		relation(4, 3, 4, memberOf, ""),
		relation(5, 4, 5, managedBy, metadata(element("discount", "0.10", "all"))),
	}
	adjuster := newAdjuster(t, relations, projects, 3)

	outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
		base("openstack", "instance", "100.00"),
	})

	assertLineCount(t, outcome, 1)
	assertLine(t, outcome.Lines[0], "discount", "partner-corp", relationID(5), "all", "0.10", "100.00", "-10.00")
	assertDecimal(t, "NetCost", outcome.NetCost, "90.00")
}

// TestAdjustEndsOnACycle walks a graph the registry refuses to create: each of
// two projects a member of the other. The visited set and the depth end it, and
// each relation adjusts once.
func TestAdjustEndsOnACycle(t *testing.T) {
	projects := []source.Project{
		project(1, metaCloud, "meta", "group-north"),
		project(2, metaCloud, "meta", "group-south"),
	}
	relations := []source.Relation{
		relation(1, 1, 2, memberOf, metadata(element("discount", "0.10", "all"))),
		relation(2, 2, 1, memberOf, metadata(element("discount", "0.10", "all"))),
	}
	adjuster := newAdjuster(t, relations, projects, 3)

	outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
		base("openstack", "instance", "100.00"),
	})

	assertLineCount(t, outcome, 2)
	assertLine(t, outcome.Lines[0], "discount", "group-south", relationID(1), "all", "0.10", "100.00", "-10.00")
	assertLine(t, outcome.Lines[1], "discount", "group-north", relationID(2), "all", "0.10", "90.00", "-9.00")
	assertDecimal(t, "NetCost", outcome.NetCost, "81.00")
}

// TestAdjustKeepsArrayOrderOnTies applies two adjustments of one relation and
// one type in the order the array lists them in, which is the last tiebreaker.
func TestAdjustKeepsArrayOrderOnTies(t *testing.T) {
	cases := []struct {
		name    string
		first   string
		second  string
		amounts [2]string
		bases   [2]string
	}{
		{
			name:  "the smaller rate first",
			first: "0.10", second: "0.20",
			bases: [2]string{"100.00", "90.00"}, amounts: [2]string{"-10.00", "-18.00"},
		},
		{
			name:  "the larger rate first",
			first: "0.20", second: "0.10",
			bases: [2]string{"100.00", "80.00"}, amounts: [2]string{"-20.00", "-8.00"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			relations := []source.Relation{
				relation(1, 1, 2, managedBy, metadata(
					element("discount", testCase.first, "all"),
					element("discount", testCase.second, "all"),
				)),
			}
			adjuster := newAdjuster(t, relations, resellerGraph(), 3)

			outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
				base("openstack", "instance", "100.00"),
			})

			assertLineCount(t, outcome, 2)
			assertLine(t, outcome.Lines[0], "discount", "partner-corp", relationID(1), "all",
				testCase.first, testCase.bases[0], testCase.amounts[0])
			assertLine(t, outcome.Lines[1], "discount", "partner-corp", relationID(1), "all",
				testCase.second, testCase.bases[1], testCase.amounts[1])
		})
	}
}

// TestNewIsIndependentOfRelationOrder bills the same statement from the same
// relations handed over in three different orders. The same graph and the same
// period have to yield the same invoice however often they are billed.
func TestNewIsIndependentOfRelationOrder(t *testing.T) {
	relations := []source.Relation{
		relation(1, 1, 2, managedBy, metadata(element("surcharge", "0.10", "all"))),
		relation(2, 1, 2, managedBy, metadata(element("discount", "0.15", "all"))),
		relation(3, 1, 2, managedBy, metadata(element("kickback", "0.10", "all"))),
	}
	orders := [][]source.Relation{
		{relations[0], relations[1], relations[2]},
		{relations[2], relations[1], relations[0]},
		{relations[1], relations[2], relations[0]},
	}

	var outcomes []adjustments.Outcome
	for _, order := range orders {
		adjuster := newAdjuster(t, order, resellerGraph(), 3)
		outcomes = append(outcomes, adjusted(t, adjuster, projectID(1), []adjustments.Base{
			base("openstack", "instance", "1000.00"),
		}))
	}

	assertLineCount(t, outcomes[0], 3)
	assertLine(t, outcomes[0].Lines[0], "surcharge", "partner-corp", relationID(1), "all", "0.10", "1000.00", "100.00")
	assertLine(t, outcomes[0].Lines[1], "discount", "partner-corp", relationID(2), "all", "0.15", "1100.00", "-165.00")
	assertLine(t, outcomes[0].Lines[2], "kickback", "partner-corp", relationID(3), "all", "0.10", "935.00", "93.50")
	for i, outcome := range outcomes[1:] {
		if !reflect.DeepEqual(outcome, outcomes[0]) {
			t.Errorf("Adjust() of order %d = %+v, want %+v", i+1, outcome, outcomes[0])
		}
	}
}

// TestNewWarnsOnKickbackToNonPartner drops the kickbacks of a relation that
// does not point at a partner and keeps everything else that relation carries.
func TestNewWarnsOnKickbackToNonPartner(t *testing.T) {
	cases := []struct {
		name     string
		target   source.Project
		platform string
		external string
	}{
		{
			name:     "a meta-project",
			target:   project(2, metaCloud, "meta", "customer-alpha"),
			platform: "meta", external: "customer-alpha",
		},
		{
			name:     "a real project",
			target:   project(2, openstackCloud, "openstack", "customer-proj-2"),
			platform: "openstack", external: "customer-proj-2",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			projects := []source.Project{
				project(1, openstackCloud, "openstack", "customer-proj-1"),
				testCase.target,
			}
			relations := []source.Relation{
				relation(1, 1, 2, memberOf, metadata(
					element("project_discount", "0.05", "all"),
					element("kickback", "0.10", "all"),
				)),
			}

			adjuster, warnings, err := adjustments.New(relations, projects, 3)
			if err != nil {
				t.Fatalf("New() error = %v, want nil", err)
			}
			want := []adjustments.Warning{{
				Code:           adjustments.WarningKickbackTargetNotPartner,
				RelationID:     relationID(1).String(),
				TargetPlatform: testCase.platform,
				TargetID:       testCase.external,
			}}
			if !reflect.DeepEqual(warnings, want) {
				t.Errorf("New() warnings = %+v, want %+v", warnings, want)
			}

			outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
				base("openstack", "instance", "100.00"),
			})

			assertLineCount(t, outcome, 1)
			assertLine(t, outcome.Lines[0], "project_discount", testCase.external, relationID(1),
				"all", "0.05", "100.00", "-5.00")
			assertDecimal(t, "NetCost", outcome.NetCost, "95.00")
			assertDecimal(t, "KickbackTotal", outcome.KickbackTotal, "0.00")
		})
	}
}

// TestAdjustRefusesUnreadableAdjustments fails the statement whose walk reaches
// a relation whose stored adjustments cannot be read, rather than billing it as
// though the relation carried none.
func TestAdjustRefusesUnreadableAdjustments(t *testing.T) {
	t.Run("adjustments the schema refuses", func(t *testing.T) {
		relations := []source.Relation{
			relation(1, 1, 2, managedBy, `{"pricing_adjustments":[{"type":"discount","rate":0.15,"scope":"all"}]}`),
		}
		adjuster := newAdjuster(t, relations, resellerGraph(), 3)

		_, err := adjuster.Adjust(projectID(1), []adjustments.Base{
			base("openstack", "instance", "100.00"),
		})

		if err == nil {
			t.Fatal("Adjust() error = nil, want an error")
		}
		for _, want := range []string{
			"the pricing adjustments of relation ",
			relationID(1).String(),
			"os-prod/customer-proj-1",
			"partner/partner-corp",
			"do not match the schema",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Adjust() error = %v, want it to contain %q", err, want)
			}
		}
		var invalid *adjustment.InvalidError
		if !errors.As(err, &invalid) {
			t.Errorf("Adjust() error = %v, want an *adjustment.InvalidError", err)
		}
	})

	t.Run("metadata that is not an object", func(t *testing.T) {
		relations := []source.Relation{relation(1, 1, 2, managedBy, `[]`)}
		adjuster := newAdjuster(t, relations, resellerGraph(), 3)

		_, err := adjuster.Adjust(projectID(1), nil)

		if err == nil || !strings.Contains(err.Error(), "decoding the relation metadata") {
			t.Fatalf("Adjust() error = %v, want it to contain %q", err, "decoding the relation metadata")
		}
	})

	t.Run("a depth below one", func(t *testing.T) {
		_, _, err := adjustments.New(nil, resellerGraph(), 0)

		want := "the adjustment walk depth is 0, and has to be at least 1"
		if err == nil || err.Error() != want {
			t.Fatalf("New() error = %v, want %q", err, want)
		}
	})
}

// TestAdjustBillsPastAnUnreachableUnreadableRelation keeps one tenant's stale
// relation metadata out of every other tenant's statement. The engine loads the
// adjustment relations of the whole deployment, so a document written before
// the API validated the member -- or by an import that went around it -- would
// otherwise fail every run and every tick until somebody edited the reporting
// database by hand.
func TestAdjustBillsPastAnUnreachableUnreadableRelation(t *testing.T) {
	projects := append(resellerGraph(),
		project(3, openstackCloud, "openstack", "customer-proj-3"),
		project(4, metaCloud, "meta", "customer-beta"),
	)
	relations := []source.Relation{
		relation(1, 1, 2, managedBy, metadata(element("discount", "0.15", "all"))),
		// Another tenant's, reachable from neither project 1 nor project 2.
		relation(2, 3, 4, memberOf, `{"pricing_adjustments":[]}`),
	}
	adjuster := newAdjuster(t, relations, projects, 3)

	outcome := adjusted(t, adjuster, projectID(1), []adjustments.Base{
		base("openstack", "instance", "1000.00"),
	})

	assertLineCount(t, outcome, 1)
	assertLine(t, outcome.Lines[0], "discount", "partner-corp", relationID(1),
		"all", "0.15", "1000.00", "-150.00")
	assertDecimal(t, "NetCost", outcome.NetCost, "850.00")

	// The tenant that owns the unreadable relation is still refused.
	if _, err := adjuster.Adjust(projectID(3), nil); err == nil {
		t.Error("Adjust() of the project the relation leaves error = nil, want an error")
	}
}

// TestNewRefusesUnknownProject refuses a relation whose ends the snapshot does
// not hold, because a line names its target's external id.
func TestNewRefusesUnknownProject(t *testing.T) {
	projects := []source.Project{project(1, openstackCloud, "openstack", "customer-proj-1")}
	relations := []source.Relation{
		relation(1, 1, 9, managedBy, metadata(element("discount", "0.10", "all"))),
	}

	_, _, err := adjustments.New(relations, projects, 3)

	want := "which the registry snapshot does not hold"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("New() error = %v, want it to contain %q", err, want)
	}
	if !strings.Contains(err.Error(), projectID(9).String()) {
		t.Errorf("New() error = %v, want it to name the project %v", err, projectID(9))
	}
}

// TestAdjustWithoutRelations bills a statement nothing adjusts, whether the run
// holds no relations at all or none that names the project.
func TestAdjustWithoutRelations(t *testing.T) {
	bases := []adjustments.Base{
		base("openstack", "instance", "100.00"),
		base("openstack", "volume", "50.00"),
	}
	assertUnadjusted := func(t *testing.T, outcome adjustments.Outcome) {
		t.Helper()

		assertDecimal(t, "BaseCost", outcome.BaseCost, "150.00")
		if outcome.Lines != nil {
			t.Errorf("Lines = %+v, want nil", outcome.Lines)
		}
		assertDecimal(t, "NetCost", outcome.NetCost, "150.00")
		assertDecimal(t, "KickbackTotal", outcome.KickbackTotal, "0.00")
	}

	t.Run("a run without relations", func(t *testing.T) {
		adjuster := newAdjuster(t, nil, resellerGraph(), 3)

		assertUnadjusted(t, adjusted(t, adjuster, projectID(1), bases))
	})

	t.Run("a project no relation names", func(t *testing.T) {
		projects := append(resellerGraph(), project(3, openstackCloud, "openstack", "customer-proj-3"))
		relations := []source.Relation{
			relation(1, 1, 2, managedBy, metadata(element("discount", "0.10", "all"))),
		}
		adjuster := newAdjuster(t, relations, projects, 3)

		assertUnadjusted(t, adjusted(t, adjuster, projectID(3), bases))
	})
}

// TestAdjustWithoutBases still shows the adjustment that reached the statement,
// on a base of nothing.
func TestAdjustWithoutBases(t *testing.T) {
	relations := []source.Relation{
		relation(1, 1, 2, managedBy, metadata(element("discount", "0.10", "all"))),
	}
	adjuster := newAdjuster(t, relations, resellerGraph(), 3)

	outcome := adjusted(t, adjuster, projectID(1), nil)

	assertDecimal(t, "BaseCost", outcome.BaseCost, "0.00")
	assertLineCount(t, outcome, 1)
	assertLine(t, outcome.Lines[0], "discount", "partner-corp", relationID(1), "all", "0.10", "0.00", "0.00")
	assertDecimal(t, "NetCost", outcome.NetCost, "0.00")
}

// TestMarshalJSON renders a line and a warning the way the statement document
// and the run's stats carry them.
func TestMarshalJSON(t *testing.T) {
	adjust := func(t *testing.T, kickback string) adjustments.Outcome {
		t.Helper()

		relations := []source.Relation{
			relation(1, 1, 2, managedBy, metadata(element("discount", "0.15", "all"), kickback)),
		}
		return adjusted(t, newAdjuster(t, relations, resellerGraph(), 3), projectID(1), []adjustments.Base{
			base("openstack", "instance", "1200.00"),
		})
	}
	assertJSON := func(t *testing.T, value any, want string) {
		t.Helper()

		got, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("Marshal() error = %v, want nil", err)
		}
		if string(got) != want {
			t.Errorf("Marshal() = %s, want %s", got, want)
		}
	}

	t.Run("a line without a description", func(t *testing.T) {
		outcome := adjust(t, element("kickback", "0.10", "all"))

		assertLineCount(t, outcome, 2)
		assertJSON(t, outcome.Lines[1], fmt.Sprintf(
			`{"type":"kickback","relation_type":"managed_by","relation_target":"partner-corp",`+
				`"relation_id":%q,"scope":"all","rate":0.100000,"base":1020.00,"amount":102.00}`,
			relationID(1)))
	})

	t.Run("a line with a description", func(t *testing.T) {
		outcome := adjust(t, `{"type":"kickback","rate":"0.10","scope":"all",`+
			`"description":"Reseller commission on net revenue"}`)

		assertLineCount(t, outcome, 2)
		assertJSON(t, outcome.Lines[1], fmt.Sprintf(
			`{"type":"kickback","relation_type":"managed_by","relation_target":"partner-corp",`+
				`"relation_id":%q,"scope":"all","description":"Reseller commission on net revenue",`+
				`"rate":0.100000,"base":1020.00,"amount":102.00}`,
			relationID(1)))
	})

	t.Run("a warning", func(t *testing.T) {
		assertJSON(t, adjustments.Warning{
			Code:           adjustments.WarningKickbackTargetNotPartner,
			RelationID:     relationID(1).String(),
			TargetPlatform: "meta",
			TargetID:       "customer-alpha",
		}, fmt.Sprintf(
			`{"code":"adjustment_kickback_target_not_partner","relation_id":%q,`+
				`"target_platform":"meta","target_id":"customer-alpha"}`,
			relationID(1)))
	})
}

package runs

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/adjustment"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/adjustments"
	"github.com/b42labs/tally/internal/engine/attribution"
	"github.com/b42labs/tally/internal/engine/corrections"
	"github.com/b42labs/tally/internal/engine/counters"
	"github.com/b42labs/tally/internal/engine/invariants"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
)

// The resource the pure cases below build their rows from.
var metered = source.Resource{
	Cloud: "os-prod-eu1", Platform: "openstack", ResourceType: "instance", ResourceID: "def-456",
}

// adjustedRelation is the relation the adjustments of the pure cases below came
// from, as the text a line carries it in.
const adjustedRelation = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

// instantOf is one instant of the fixtures below.
func instantOf(t *testing.T, text string) time.Time {
	t.Helper()

	ts, err := time.Parse(time.RFC3339, text)
	if err != nil {
		t.Fatalf("parsing the instant %q: %v", text, err)
	}
	return ts
}

// amount is one rated dimension of a record.
func amount(t *testing.T, metric, value string) rating.DimensionAmount {
	t.Helper()

	rated, err := decimal.NewFromString(value)
	if err != nil {
		t.Fatalf("parsing the amount %q: %v", value, err)
	}
	return rating.DimensionAmount{Metric: metric, Amount: rated, Quantity: decimal.NewFromInt(4)}
}

// ratedOf is one rated resource over the records the case passes.
func ratedOf(records ...rating.RecordRating) rating.Result {
	return rating.Result{
		Currency:  "EUR",
		Resources: []rating.ResourceRating{{Resource: metered, Records: records}},
	}
}

// TestStatsJSONShape pins the object a run stores in runs.stats. The field
// names are what an operator reads and greps a finished run by, and what the
// CLI and every later query read the run's counts from.
func TestStatsJSONShape(t *testing.T) {
	snapshotAt := instantOf(t, "2026-03-01T00:00:00Z")
	from := instantOf(t, "2026-03-10T00:00:00Z")
	to := instantOf(t, "2026-03-11T00:00:00Z")
	project := uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8")

	stats := Stats{
		SnapshotAt:        &snapshotAt,
		Candidates:        3,
		UsageRecords:      4,
		RatedRecords:      5,
		Statements:        2,
		AdjustmentRecords: 6,
		Warnings: []Warning{{
			Code:   WarningPeriodNotEnded,
			Detail: "period_to 2026-04-01T00:00:00Z has not passed yet",
		}},
		MeteringWarnings: []metering.Warning{{
			Cloud: metered.Cloud, ResourceType: metered.ResourceType, ResourceID: metered.ResourceID,
			Code: metering.WarningCandidateWithoutHistory,
		}},
		CounterWarnings: []counters.Warning{{
			Cloud: metered.Cloud, ResourceType: metered.ResourceType, ResourceID: metered.ResourceID,
			Metric: "egress_gb", FromTS: from, ToTS: to,
			Code: counters.WarningCounterSourceFailed, Detail: "the store did not answer",
		}},
		AttributionWarnings: []attribution.Warning{{
			Code: attribution.WarningCycle, ProjectID: project,
		}},
		AdjustmentWarnings: []adjustments.Warning{{
			Code: adjustments.WarningKickbackTargetNotPartner, RelationID: adjustedRelation,
			TargetPlatform: "meta", TargetID: "customer-alpha",
		}},
		Unpriced: []rating.UnpricedResourceType{{
			Platform: metered.Platform, ResourceType: "volume", Count: 1,
		}},
		Unreadable: []rating.UnreadableQuantity{{
			Platform: metered.Platform, ResourceType: metered.ResourceType, Field: "vcpus", Count: 2,
		}},
		UnregisteredProjects: []statements.UnregisteredProject{{
			Cloud: metered.Cloud, ProjectID: "proj-456", Resources: 1,
		}},
		Violations: []metering.ResourceViolations{{
			Cloud: metered.Cloud, ResourceType: metered.ResourceType, ResourceID: metered.ResourceID,
			Violations: []invariants.Violation{{
				Invariant: invariants.InvariantCoverage, Detail: "the spans cover 24h0m0s",
			}},
		}},
		Error: "the reporting database did not answer",
	}

	got, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}

	want := `{"snapshot_at":"2026-03-01T00:00:00Z",` +
		`"candidates":3,"usage_records":4,"rated_records":5,"statements":2,"adjustment_records":6,` +
		`"warnings":[{"code":"period_not_ended",` +
		`"detail":"period_to 2026-04-01T00:00:00Z has not passed yet"}],` +
		`"metering_warnings":[{"cloud":"os-prod-eu1","resource_type":"instance","resource_id":"def-456",` +
		`"code":"candidate_without_history"}],` +
		`"counter_warnings":[{"cloud":"os-prod-eu1","resource_type":"instance","resource_id":"def-456",` +
		`"metric":"egress_gb","from_ts":"2026-03-10T00:00:00Z","to_ts":"2026-03-11T00:00:00Z",` +
		`"code":"counter_source_failed","detail":"the store did not answer"}],` +
		`"attribution_warnings":[{"code":"attribution_cycle",` +
		`"project_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"}],` +
		`"adjustment_warnings":[{"code":"adjustment_kickback_target_not_partner",` +
		`"relation_id":"` + adjustedRelation + `",` +
		`"target_platform":"meta","target_id":"customer-alpha"}],` +
		`"unpriced":[{"platform":"openstack","resource_type":"volume","count":1}],` +
		`"unreadable":[{"platform":"openstack","resource_type":"instance","field":"vcpus","count":2}],` +
		`"unregistered_projects":[{"cloud":"os-prod-eu1","project_id":"proj-456","resources":1}],` +
		`"violations":[{"cloud":"os-prod-eu1","resource_type":"instance","resource_id":"def-456",` +
		`"violations":[{"invariant":"coverage","detail":"the spans cover 24h0m0s"}]}],` +
		`"error":"the reporting database did not answer"}`
	if string(got) != want {
		t.Errorf("Marshal =\n%s\nwant\n%s", got, want)
	}
}

// TestStatsJSONOmitsEmptyLists pins what a run that found nothing to report
// stores: the four counts, and no key at all for the lists it has nothing in.
// An absent key is what tells a run without warnings from one whose warnings
// were dropped somewhere.
func TestStatsJSONOmitsEmptyLists(t *testing.T) {
	got, err := json.Marshal(Stats{Candidates: 1, UsageRecords: 2, RatedRecords: 4, Statements: 1})
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}

	want := `{"candidates":1,"usage_records":2,"rated_records":4,"statements":1}`
	if string(got) != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
}

// TestCorrectionStatsJSONShape pins the object a correction stores in
// runs.stats: the keys of a regular run, flattened, with the delta counts
// beside them. An operator reads a correction the way they read a run, and the
// keys that are new are the two that say how much it found.
func TestCorrectionStatsJSONShape(t *testing.T) {
	snapshotAt := instantOf(t, "2026-03-01T00:00:00Z")

	got, err := json.Marshal(CorrectionStats{
		Stats: Stats{
			SnapshotAt: &snapshotAt, Candidates: 3, UsageRecords: 4, RatedRecords: 5,
			Statements: 2, Error: "x",
		},
		Deltas:           7,
		AdjustmentDeltas: 2,
	})
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}

	want := `{"snapshot_at":"2026-03-01T00:00:00Z",` +
		`"candidates":3,"usage_records":4,"rated_records":5,"statements":2,` +
		`"error":"x","deltas":7,"adjustment_deltas":2}`
	if string(got) != want {
		t.Errorf("Marshal =\n%s\nwant\n%s", got, want)
	}

	// A correction that found nothing reports its counts and delta counts of
	// zero: the keys stay, because no deltas is what such a correction says.
	got, err = json.Marshal(CorrectionStats{
		Stats: Stats{Candidates: 1, UsageRecords: 2, RatedRecords: 4, Statements: 1},
	})
	if err != nil {
		t.Fatalf("Marshal error = %v, want nil", err)
	}
	want = `{"candidates":1,"usage_records":2,"rated_records":4,"statements":1,` +
		`"deltas":0,"adjustment_deltas":0}`
	if string(got) != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
}

// deltaOf is one difference over the resource the pure cases build their rows
// from, at the two amounts the case passes as text.
func deltaOf(t *testing.T, dimension, old, current string) corrections.Delta {
	t.Helper()

	oldAmount, err := decimal.NewFromString(old)
	if err != nil {
		t.Fatalf("parsing the old amount %q: %v", old, err)
	}
	newAmount, err := decimal.NewFromString(current)
	if err != nil {
		t.Fatalf("parsing the new amount %q: %v", current, err)
	}
	return corrections.Delta{
		Key: corrections.Key{
			Cloud: metered.Cloud, Platform: metered.Platform,
			ResourceType: metered.ResourceType, ResourceID: metered.ResourceID,
			ProjectID: "proj-456", Dimension: dimension,
		},
		Old: oldAmount, New: newAmount, Delta: newAmount.Sub(oldAmount),
	}
}

// numericText is what a numeric parameter carries, as the text it reaches the
// column as. It keeps the assertions on money off floats
// (roadmap/00-conventions.md section 6).
func numericText(t *testing.T, value pgtype.Numeric) string {
	t.Helper()

	stored, err := value.Value()
	if err != nil {
		t.Fatalf("reading the numeric back: %v", err)
	}
	text, isText := stored.(string)
	if !isText {
		t.Fatalf("the numeric reads as a %T, want the text of the amount", stored)
	}
	return text
}

// TestDeltaRowsOversized refuses a delta whose old amount, new amount or
// difference is past what NUMERIC(14,2) holds. Two amounts the column holds can
// differ by one it does not, so all three are checked, and a refusal comes back
// with no rows at all.
func TestDeltaRowsOversized(t *testing.T) {
	t.Run("the largest amount the column holds is written", func(t *testing.T) {
		rows, err := deltaRows(uuid.New(), uuid.New(),
			[]corrections.Delta{deltaOf(t, "vcpus", "0.00", "999999999999.99")}, "EUR")
		if err != nil {
			t.Fatalf("deltaRows() error = %v, want nil", err)
		}
		if len(rows) != 1 {
			t.Errorf("deltaRows() = %d rows, want the delta at the bound written", len(rows))
		}
	})

	for _, tc := range []struct {
		name       string
		old, given string
	}{
		{name: "the old amount past the bound", old: "1000000000000.00", given: "0.00"},
		{name: "the new amount past the bound", old: "0.00", given: "1000000000000.00"},
		// Both amounts fit and their difference does not, which is the one of
		// the three the two sides alone do not report.
		{name: "the difference past the bound", old: "-600000000000.00", given: "600000000000.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := deltaRows(uuid.New(), uuid.New(), []corrections.Delta{
				deltaOf(t, "ram_gb", "1.00", "2.00"),
				deltaOf(t, "vcpus", tc.old, tc.given),
			}, "EUR")
			if err == nil {
				t.Fatal("deltaRows() error = nil, want the oversized delta refused")
			}
			if rows != nil {
				t.Errorf("deltaRows() = %v, want no rows: a refused delta leaves nothing to write", rows)
			}
			if !strings.Contains(err.Error(), name(metered)) {
				t.Errorf("deltaRows() error = %q, want it to name the resource %q", err, name(metered))
			}
			if !strings.Contains(err.Error(), "vcpus") {
				t.Errorf("deltaRows() error = %q, want it to name the dimension vcpus", err)
			}
			if !strings.Contains(err.Error(), "out of range") {
				t.Errorf("deltaRows() error = %q, want it to say a usage value is out of range", err)
			}
		})
	}
}

// TestDeltaRowsEmpty is a correction that found nothing: no rows, and the empty
// slice rather than a nil one, because the write skips a delta list of no
// length rather than passing a nil through the COPY.
func TestDeltaRowsEmpty(t *testing.T) {
	rows, err := deltaRows(uuid.New(), uuid.New(), nil, "EUR")
	if err != nil {
		t.Fatalf("deltaRows() error = %v, want nil", err)
	}
	if rows == nil {
		t.Fatal("deltaRows() = nil, want an empty slice")
	}
	if len(rows) != 0 {
		t.Errorf("deltaRows() = %d rows, want none", len(rows))
	}
}

// TestDeltaRowsShape is what one delta reaches the column as: the two runs it
// names, the six fields of its key, and the three amounts as the text of the
// decimals rather than as floats.
func TestDeltaRowsShape(t *testing.T) {
	runID, correctsRunID := uuid.New(), uuid.New()

	rows, err := deltaRows(runID, correctsRunID, []corrections.Delta{
		deltaOf(t, "vcpus", "59.52", "49.92"),
		deltaOf(t, "ram_gb", "29.76", "24.96"),
	}, "EUR")
	if err != nil {
		t.Fatalf("deltaRows() error = %v, want nil", err)
	}
	if len(rows) != 2 {
		t.Fatalf("deltaRows() = %d rows, want one per delta", len(rows))
	}

	row := rows[0]
	if row.RunID != uuidValue(runID) || row.CorrectsRunID != uuidValue(correctsRunID) {
		t.Errorf("row runs = (%v, %v), want (%v, %v)",
			row.RunID, row.CorrectsRunID, uuidValue(runID), uuidValue(correctsRunID))
	}
	if row.Cloud != metered.Cloud || row.Platform != metered.Platform ||
		row.ResourceType != metered.ResourceType || row.ResourceID != metered.ResourceID {
		t.Errorf("row resource = %s/%s/%s/%s, want %s",
			row.Cloud, row.Platform, row.ResourceType, row.ResourceID, name(metered))
	}
	if row.ProjectID != "proj-456" || row.Dimension != "vcpus" {
		t.Errorf("row key = (%s, %s), want (proj-456, vcpus)", row.ProjectID, row.Dimension)
	}
	if row.Currency != "EUR" {
		t.Errorf("row currency = %q, want EUR", row.Currency)
	}
	for _, tc := range []struct {
		field string
		value pgtype.Numeric
		want  string
	}{
		{field: "old_amount", value: row.OldAmount, want: "59.52"},
		{field: "new_amount", value: row.NewAmount, want: "49.92"},
		{field: "delta", value: row.Delta, want: "-9.60"},
	} {
		if got := numericText(t, tc.value); got != tc.want {
			t.Errorf("row %s = %s, want %s", tc.field, got, tc.want)
		}
	}

	// The ids are generated here rather than left to the column default,
	// because COPY evaluates no defaults.
	if rows[0].ID == rows[1].ID {
		t.Error("both rows carry one id, want an id per row")
	}
	for i, row := range rows {
		if row.ID == (pgtype.UUID{}) {
			t.Errorf("row %d carries no id, want the one it is written under", i)
		}
	}
}

// lineOf is one applied adjustment of the statements the cases below build
// their rows from, at the rate and the two amounts the case passes as text.
func lineOf(t *testing.T, adjustmentType, target, rate, adjustedBase, adjusted string) adjustments.Line {
	t.Helper()

	return adjustments.Line{
		Type:           adjustmentType,
		RelationType:   "managed_by",
		RelationTarget: target,
		RelationID:     adjustedRelation,
		Scope:          "all",
		Rate:           money.NewRate(decimal.RequireFromString(rate)),
		Base:           money.NewAmount(decimal.RequireFromString(adjustedBase)),
		Amount:         money.NewAmount(decimal.RequireFromString(adjusted)),
	}
}

// adjustedStatements are the two statements of the shape case: one that two
// adjustments reached and one that none did.
func adjustedStatements(t *testing.T) []statements.Statement {
	t.Helper()

	return []statements.Statement{
		{
			Key:      "os-prod-eu1/customer-proj-1",
			Currency: "EUR",
			Adjustments: []adjustments.Line{
				lineOf(t, adjustment.TypeDiscount, "partner-corp", "0.15", "1200.00", "-180.00"),
				lineOf(t, adjustment.TypeKickback, "partner-corp", "0.10", "1020.00", "102.00"),
			},
		},
		{Key: "os-prod-eu1/customer-proj-2", Currency: "EUR"},
	}
}

// TestAdjustmentRowsShape is what one adjustment line reaches the columns as:
// the statement it was applied to, the relation it came from, and the rate at
// six places beside the base and the amount at two, each as the text of the
// decimal rather than as a float. The beneficiary is the relation's target on a
// kickback and nothing on the other types, because a kickback is the one
// adjustment somebody is owed the amount of. A statement no adjustment reached
// writes no row.
func TestAdjustmentRowsShape(t *testing.T) {
	runID := uuid.New()

	rows, err := adjustmentRows(runID, adjustedStatements(t))
	if err != nil {
		t.Fatalf("adjustmentRows() error = %v, want nil", err)
	}
	if len(rows) != 2 {
		t.Fatalf("adjustmentRows() = %d rows, want one per adjustment line", len(rows))
	}

	for i, row := range rows {
		if row.RunID != uuidValue(runID) {
			t.Errorf("row %d run = %v, want %v", i, row.RunID, uuidValue(runID))
		}
		if row.ProjectID != "os-prod-eu1/customer-proj-1" {
			t.Errorf("row %d statement = %q, want the one its line was applied to", i, row.ProjectID)
		}
		if want := uuidValue(uuid.MustParse(adjustedRelation)); row.RelationID != want {
			t.Errorf("row %d relation = %v, want %v", i, row.RelationID, want)
		}
		if row.RelationType != "managed_by" || row.RelationTarget != "partner-corp" {
			t.Errorf("row %d relation = (%s, %s), want (managed_by, partner-corp)",
				i, row.RelationType, row.RelationTarget)
		}
		if row.Scope != "all" || row.Currency != "EUR" {
			t.Errorf("row %d = (%s, %s), want (all, EUR)", i, row.Scope, row.Currency)
		}
	}

	for _, tc := range []struct {
		index                        int
		adjustmentType               string
		rate, adjustedBase, adjusted string
		beneficiary                  string
	}{
		{
			index: 0, adjustmentType: adjustment.TypeDiscount,
			rate: "0.150000", adjustedBase: "1200.00", adjusted: "-180.00",
		},
		{
			index: 1, adjustmentType: adjustment.TypeKickback,
			rate: "0.100000", adjustedBase: "1020.00", adjusted: "102.00",
			beneficiary: "partner-corp",
		},
	} {
		row := rows[tc.index]
		if row.Type != tc.adjustmentType {
			t.Errorf("row %d type = %q, want %q", tc.index, row.Type, tc.adjustmentType)
		}
		for _, field := range []struct {
			name  string
			value pgtype.Numeric
			want  string
		}{
			{name: "rate", value: row.Rate, want: tc.rate},
			{name: "base", value: row.Base, want: tc.adjustedBase},
			{name: "amount", value: row.Amount, want: tc.adjusted},
		} {
			if got := numericText(t, field.value); got != field.want {
				t.Errorf("row %d %s = %s, want %s", tc.index, field.name, got, field.want)
			}
		}
		switch {
		case tc.beneficiary == "" && row.Beneficiary.Valid:
			t.Errorf("row %d beneficiary = %q, want none: a %s is owed to nobody",
				tc.index, row.Beneficiary.String, tc.adjustmentType)
		case tc.beneficiary != "" && (!row.Beneficiary.Valid || row.Beneficiary.String != tc.beneficiary):
			t.Errorf("row %d beneficiary = %+v, want %q", tc.index, row.Beneficiary, tc.beneficiary)
		}
	}

	// The ids are generated here rather than left to the column default,
	// because COPY evaluates no defaults.
	if rows[0].ID == rows[1].ID {
		t.Error("both rows carry one id, want an id per row")
	}
}

// TestAdjustmentRowsOversized refuses an adjustment whose base or amount is
// past what NUMERIC(14,2) holds. The database would report it as a numeric
// field overflow naming the column alone, which says nothing about the
// statement it was applied to or the relation it came from.
func TestAdjustmentRowsOversized(t *testing.T) {
	adjusted := func(line adjustments.Line) []statements.Statement {
		return []statements.Statement{{
			Key: "os-prod-eu1/customer-proj-1", Currency: "EUR",
			Adjustments: []adjustments.Line{line},
		}}
	}

	t.Run("the largest base the column holds is written", func(t *testing.T) {
		rows, err := adjustmentRows(uuid.New(), adjusted(
			lineOf(t, adjustment.TypeDiscount, "partner-corp", "0.15", "999999999999.99", "-180.00")))
		if err != nil {
			t.Fatalf("adjustmentRows() error = %v, want nil", err)
		}
		if len(rows) != 1 {
			t.Errorf("adjustmentRows() = %d rows, want the base at the bound written", len(rows))
		}
	})

	t.Run("an amount past the bound", func(t *testing.T) {
		rows, err := adjustmentRows(uuid.New(), adjusted(
			lineOf(t, adjustment.TypeKickback, "partner-corp", "0.10", "1200.00", "1000000000000.00")))
		if err == nil {
			t.Fatal("adjustmentRows() error = nil, want the oversized amount refused")
		}
		if rows != nil {
			t.Errorf("adjustmentRows() = %v, want no rows: a refused adjustment leaves nothing to write", rows)
		}
		for _, want := range []string{
			adjustment.TypeKickback, adjustedRelation, "os-prod-eu1/customer-proj-1",
			"past the 999999999999.99 the column holds",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("adjustmentRows() error = %q, want it to name %q", err, want)
			}
		}
	})
}

// TestAdjustmentRowsEmpty is a run that adjusted nothing: no rows, and the
// empty slice rather than a nil one, because the write skips an adjustment list
// of no length rather than passing a nil through the COPY.
func TestAdjustmentRowsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		sts  []statements.Statement
	}{
		{name: "no statements at all"},
		{name: "statements no adjustment reached", sts: []statements.Statement{
			{Key: "os-prod-eu1/customer-proj-1", Currency: "EUR"},
			{Key: "os-prod-eu1/customer-proj-2", Currency: "EUR"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := adjustmentRows(uuid.New(), tc.sts)
			if err != nil {
				t.Fatalf("adjustmentRows() error = %v, want nil", err)
			}
			if rows == nil {
				t.Fatal("adjustmentRows() = nil, want an empty slice")
			}
			if len(rows) != 0 {
				t.Errorf("adjustmentRows() = %d rows, want none", len(rows))
			}
		})
	}
}

// wantRatedRow is one expected rated row of the case below.
type wantRatedRow struct {
	dimension   string
	usageRecord pgtype.UUID
}

// TestUsageAndRatedRowsAlign is the pair's contract: every draft becomes one
// usage record, every dimension of the record that rates it becomes one rated
// record, and that rated record names the usage record of its own draft. The
// ids are generated in Go, which is what lets both tables go in over COPY.
func TestUsageAndRatedRowsAlign(t *testing.T) {
	runID := uuid.New()
	first := instantOf(t, "2026-03-01T00:00:00Z")
	second := instantOf(t, "2026-03-16T00:00:00Z")
	third := instantOf(t, "2026-04-01T00:00:00Z")

	usage, ids, err := usageRows(runID, []metering.ResourceUsage{{
		Resource: metered,
		Drafts: []metering.UsageDraft{
			{
				State: "active", ProjectID: "proj-456", FromTS: first, ToTS: second, Seconds: 1296000,
				Usage: map[string]any{"vcpus": 2, "minutes": money.NewQuantity(decimal.NewFromInt(21600))},
			},
			{
				State: "active", ProjectID: "proj-456", FromTS: second, ToTS: third, Seconds: 1382400,
				Usage: map[string]any{"vcpus": 4, "minutes": money.NewQuantity(decimal.NewFromInt(23040))},
			},
		},
	}})
	if err != nil {
		t.Fatalf("usageRows() error = %v, want nil", err)
	}
	if len(usage) != 2 {
		t.Fatalf("usageRows() = %d rows, want one per draft", len(usage))
	}
	if usage[0].Seconds != 1296000 || usage[1].Seconds != 1382400 {
		t.Errorf("seconds = %d and %d, want 1296000 and 1382400", usage[0].Seconds, usage[1].Seconds)
	}
	// The quantity is stored at the four places it is rendered at, beside the
	// size field the provider reported.
	if want := `{"minutes":21600.0000,"vcpus":2}`; string(usage[0].Usage) != want {
		t.Errorf("usage of the first draft = %s, want %s", usage[0].Usage, want)
	}
	if usage[0].RunID != uuidValue(runID) {
		t.Errorf("run id = %v, want %v", usage[0].RunID, uuidValue(runID))
	}

	rows, err := ratedRows(runID, ratedOf(
		rating.RecordRating{Amounts: []rating.DimensionAmount{
			amount(t, "vcpus", "14.40"), amount(t, "egress_gb", "0.00"),
		}},
		rating.RecordRating{Amounts: []rating.DimensionAmount{amount(t, "vcpus", "30.72")}},
	), ids)
	if err != nil {
		t.Fatalf("ratedRows() error = %v, want nil", err)
	}
	if len(rows) != 3 {
		t.Fatalf("ratedRows() = %d rows, want one per dimension of every record", len(rows))
	}

	for i, want := range []wantRatedRow{
		{dimension: "vcpus", usageRecord: usage[0].ID},
		{dimension: "egress_gb", usageRecord: usage[0].ID},
		{dimension: "vcpus", usageRecord: usage[1].ID},
	} {
		if rows[i].Dimension != want.dimension {
			t.Errorf("row %d dimension = %q, want %q", i, rows[i].Dimension, want.dimension)
		}
		if rows[i].UsageRecordID != want.usageRecord {
			t.Errorf("row %d usage record = %v, want the id of its own draft %v",
				i, rows[i].UsageRecordID, want.usageRecord)
		}
		if rows[i].Currency != "EUR" {
			t.Errorf("row %d currency = %q, want EUR", i, rows[i].Currency)
		}
	}
	if ids[metered][0] != uuid.UUID(usage[0].ID.Bytes) {
		t.Errorf("the id of the first draft = %v, want the id its row carries %v",
			ids[metered][0], uuid.UUID(usage[0].ID.Bytes))
	}
}

// TestRatedRowsOversizedAmount refuses an amount past what NUMERIC(14,2) holds.
// The database would report it as a numeric field overflow naming the column
// alone, which says nothing about the resource whose usage was rated into it.
func TestRatedRowsOversizedAmount(t *testing.T) {
	ids := map[source.Resource][]uuid.UUID{metered: {uuid.New()}}

	t.Run("the largest amount the column holds is written", func(t *testing.T) {
		rows, err := ratedRows(uuid.New(), ratedOf(
			rating.RecordRating{Amounts: []rating.DimensionAmount{amount(t, "vcpus", "999999999999.99")}},
		), ids)
		if err != nil {
			t.Fatalf("ratedRows() error = %v, want nil", err)
		}
		if len(rows) != 1 {
			t.Errorf("ratedRows() = %d rows, want the amount at the bound written", len(rows))
		}
	})

	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "past the bound", value: "1000000000000.00"},
		{name: "past the bound as a credit", value: "-1000000000000.00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ratedRows(uuid.New(), ratedOf(
				rating.RecordRating{Amounts: []rating.DimensionAmount{
					amount(t, "ram_gb", "1.00"), amount(t, "vcpus", tc.value),
				}},
			), ids)
			if err == nil {
				t.Fatal("ratedRows() error = nil, want the oversized amount refused")
			}
			if !strings.Contains(err.Error(), name(metered)) {
				t.Errorf("ratedRows() error = %q, want it to name the resource %q", err, name(metered))
			}
			if !strings.Contains(err.Error(), "vcpus") {
				t.Errorf("ratedRows() error = %q, want it to name the dimension vcpus", err)
			}
			if !strings.Contains(err.Error(), "out of range") {
				t.Errorf("ratedRows() error = %q, want it to say a usage value is out of range", err)
			}
		})
	}
}

// TestRatedRowsMisalignedResources refuses rated records that do not line up
// with the drafts they are supposed to rate. Writing them against whichever
// usage record happens to line up would leave a run whose rated records name
// another resource's usage.
func TestRatedRowsMisalignedResources(t *testing.T) {
	record := rating.RecordRating{Amounts: []rating.DimensionAmount{amount(t, "vcpus", "14.40")}}

	t.Run("a rated resource metering did not produce", func(t *testing.T) {
		_, err := ratedRows(uuid.New(), ratedOf(record), map[source.Resource][]uuid.UUID{})
		if err == nil {
			t.Fatal("ratedRows() error = nil, want the unmetered resource refused")
		}
		if want := "the rated resource " + name(metered) + " carries no metered usage"; err.Error() != want {
			t.Errorf("ratedRows() error = %q, want %q", err, want)
		}
	})

	t.Run("more records than drafts", func(t *testing.T) {
		ids := map[source.Resource][]uuid.UUID{metered: {uuid.New()}}

		_, err := ratedRows(uuid.New(), ratedOf(record, record), ids)
		if err == nil {
			t.Fatal("ratedRows() error = nil, want the record count refused")
		}
		want := "the rated resource " + name(metered) + " carries 2 records for 1 usage drafts"
		if err.Error() != want {
			t.Errorf("ratedRows() error = %q, want %q", err, want)
		}
	})

	t.Run("fewer records than drafts", func(t *testing.T) {
		ids := map[source.Resource][]uuid.UUID{metered: {uuid.New(), uuid.New()}}

		_, err := ratedRows(uuid.New(), ratedOf(record), ids)
		if err == nil {
			t.Fatal("ratedRows() error = nil, want the record count refused")
		}
		want := "the rated resource " + name(metered) + " carries 1 records for 2 usage drafts"
		if err.Error() != want {
			t.Errorf("ratedRows() error = %q, want %q", err, want)
		}
	})
}

// TestLateEventsError pins what a failed read of the late events reports. The
// read of a period past the reporting database's compression policy is bounded
// by the statement_timeout alone, and that period is the one a late-arrival
// correction is most likely due for, so the timeout is named for what it is and
// carries the correction that books the events without the report. Every other
// failure is the database's own and passes through unchanged, a statement
// somebody canceled by hand included: Postgres reports that under the same
// SQLSTATE and only the message tells the two apart.
func TestLateEventsError(t *testing.T) {
	t.Run("the statement timeout", func(t *testing.T) {
		timedOut := fmt.Errorf("listing the late events: %w", &pgconn.PgError{
			Code: queryCanceled, Message: "canceling statement due to statement timeout",
		})

		err := lateEventsError("2025-01", timedOut)
		for _, want := range []string{
			"statement timeout", "compression policy",
			"tally-engine correct --period 2025-01",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("lateEventsError() error = %q, want it to name %q", err, want)
			}
		}
		if !errors.Is(err, timedOut) {
			t.Errorf("lateEventsError() error = %v, want the database's own error wrapped", err)
		}
	})

	t.Run("a canceled statement that is not the timeout", func(t *testing.T) {
		canceled := fmt.Errorf("listing the late events: %w", &pgconn.PgError{
			Code: queryCanceled, Message: "canceling statement due to user request",
		})

		if err := lateEventsError("2025-01", canceled); err != canceled {
			t.Errorf("lateEventsError() error = %v, want the read's own error %v", err, canceled)
		}
	})

	t.Run("any other failure", func(t *testing.T) {
		dropped := fmt.Errorf("listing the late events: %w", &pgconn.PgError{
			Code: "42P01", Message: `relation "events" does not exist`,
		})

		if err := lateEventsError("2025-01", dropped); err != dropped {
			t.Errorf("lateEventsError() error = %v, want the read's own error %v", err, dropped)
		}
	})
}

// The golden suite: the worked examples of the concept, run end to end through
// the engine. A case seeds events into a reporting database, meters, measures,
// rates and attributes them through runs.Execute, and checks the usage records,
// the amounts and the statement documents that come out against numbers written
// down by hand.
//
// Every case bills March 2026, [2026-03-01T00:00:00Z, 2026-04-01T00:00:00Z),
// which is the period README section 3.4 computes its examples over. The
// expectations come from that section and from pricing/2026-03.yaml, never from
// a previous run: a failure here is a report about the engine, so the answer to
// one is to find what changed in the engine, not to write the engine's new
// number into expected.json.
package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/adjustment"
	"github.com/b42labs/tally/internal/engine/corrections"
	"github.com/b42labs/tally/internal/engine/export"
	"github.com/b42labs/tally/internal/engine/invariants"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/scheduler"
	"github.com/b42labs/tally/internal/engine/source"
)

// TestGoldenStubQuerier pins the stub the metricsql sources of the suite are
// answered by. It needs no database, so the one seam every counter case rests
// on is checked even where Docker is out of reach.
func TestGoldenStubQuerier(t *testing.T) {
	const expr = `egress_gb{cloud="os-golden-e2e",resource_id="abc-123"}[240h]`
	at := time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)

	querier := newStubQuerier(t, []metricEntry{{Query: expr, At: at, Value: "18.0"}})

	t.Run("reports a canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if _, err := querier.Query(ctx, expr, at); !errors.Is(err, context.Canceled) {
			t.Errorf("Query() error = %v, want context.Canceled", err)
		}
	})

	t.Run("answers a stubbed pair", func(t *testing.T) {
		got, err := querier.Query(t.Context(), expr, at)
		if err != nil {
			t.Fatalf("Query() error = %v, want nil", err)
		}
		if want := decimal.RequireFromString("18.0"); !got.Equal(want) {
			t.Errorf("Query() = %s, want %s", got, want)
		}
	})

	t.Run("refuses an instant nothing was stubbed for", func(t *testing.T) {
		unstubbed := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

		_, err := querier.Query(t.Context(), expr, unstubbed)
		if err == nil {
			t.Fatal("Query() error = nil, want one")
		}
		want := `no stubbed metric for "egress_gb{cloud=\"os-golden-e2e\",resource_id=\"abc-123\"}[240h]" ` +
			`at 2026-04-01T00:00:00Z`
		if err.Error() != want {
			t.Errorf("Query() error = %q, want %q", err.Error(), want)
		}
	})
}

// TestGoldenExpandBulk pins the invariant the generators of a case rest on:
// every event one stands for lies strictly inside its interval, so none of them
// falls on an instant the timeline splits at and is metered into the interval
// beside the one it was written for. Like the stub above it needs no database.
func TestGoldenExpandBulk(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 3, 18, 0, 0, 0, 0, time.UTC)

	t.Run("a generator of count 0 stands for nothing", func(t *testing.T) {
		got := expandBulk(t, []bulkGenerator{{EventType: "repository.pull", From: from, To: to}})
		if len(got) != 0 {
			t.Errorf("events = %+v, want none", got)
		}
	})

	t.Run("every event lies strictly inside the interval", func(t *testing.T) {
		got := expandBulk(t, []bulkGenerator{{EventType: "repository.pull", Count: 812, From: from, To: to}})
		if len(got) != 812 {
			t.Fatalf("events = %d, want 812", len(got))
		}
		for i, ev := range got {
			if !ev.Timestamp.After(from) || !ev.Timestamp.Before(to) {
				t.Errorf("event %d is stamped %s, want strictly inside [%s, %s)",
					i, instant(ev.Timestamp), instant(from), instant(to))
			}
		}
	})

	t.Run("generators of one type name distinct events", func(t *testing.T) {
		got := expandBulk(t, []bulkGenerator{
			{
				EventType: "repository.pull", Count: 3, From: from, To: to,
				Cloud: "hb-golden-harbor", ResourceType: "repository", ResourceID: "team-alpha/app",
			},
			{
				EventType: "repository.pull", Count: 3, From: to, To: periodTo,
				Cloud: "hb-golden-harbor", ResourceType: "repository", ResourceID: "team-alpha/app",
			},
			// The same type over the same window on a second repository: the
			// pair the events table would refuse on (event_id, timestamp) if
			// the id stood for the interval alone.
			{
				EventType: "repository.pull", Count: 3, From: from, To: to,
				Cloud: "hb-golden-harbor", ResourceType: "repository", ResourceID: "team-beta/app",
			},
			// The same id under a second resource type: a resource is keyed by
			// the triple, so this is a fourth resource rather than the first
			// one over again.
			{
				EventType: "repository.pull", Count: 3, From: from, To: to,
				Cloud: "hb-golden-harbor", ResourceType: "artifact", ResourceID: "team-alpha/app",
			},
			// The first generator's window carried to the end of the period.
			// The step differs, so these events insert beside the first
			// generator's rather than conflicting with them, and an id without
			// To would file two windows of one repository under one set of ids
			// without anything downstream noticing.
			{
				EventType: "repository.pull", Count: 3, From: from, To: periodTo,
				Cloud: "hb-golden-harbor", ResourceType: "repository", ResourceID: "team-alpha/app",
			},
			// The first generator's window and count off by one: the step
			// differs again, and the first three ids would be the first
			// generator's if the id left the count out.
			{
				EventType: "repository.pull", Count: 4, From: from, To: to,
				Cloud: "hb-golden-harbor", ResourceType: "repository", ResourceID: "team-alpha/app",
			},
		})
		ids := make(map[string]bool, len(got))
		for _, ev := range got {
			if ids[ev.EventID] {
				t.Errorf("the event id %s stands for two events", ev.EventID)
			}
			ids[ev.EventID] = true
		}
		if len(ids) != 19 {
			t.Errorf("distinct event ids = %d, want the 19 the six generators stand for", len(ids))
		}
	})
}

// TestGoldenAdjustmentExpectations pins the two derivations a commercial case
// rests on: the adjustment records the statements of an expected.json describe,
// and the kickbacks a run of it has to settle. Both are read out of the fixture
// rather than out of the run, so they are what a case holds the engine to. Like
// the two tests above they need no database.
func TestGoldenAdjustmentExpectations(t *testing.T) {
	const (
		key          = "os-golden-x/proj-x"
		plainKey     = "os-golden-x/proj-plain"
		beneficiary  = "partner-corp"
		relationType = "managed_by"
	)
	relation := uuid.New()
	seeded := seededRegistry{relations: []uuid.UUID{relation}}

	// The pair of adjustments the concept spells for a reseller relation: a
	// discount off the base cost, and a commission on the net cost it leaves.
	adjusted := expectedStatement{
		Key: key,
		Adjustments: []expectedAdjustment{
			{
				Type: adjustment.TypeDiscount, Relation: 0,
				RelationType: relationType, RelationTarget: beneficiary,
				Scope: "all", Description: "Reseller end-customer discount",
				Rate:   decimal.RequireFromString("0.15"),
				Base:   decimal.RequireFromString("1200.00"),
				Amount: decimal.RequireFromString("-180.00"),
			},
			{
				Type: adjustment.TypeKickback, Relation: 0,
				RelationType: relationType, RelationTarget: beneficiary,
				Scope: "all", Description: "Reseller commission on net revenue",
				Rate:   decimal.RequireFromString("0.10"),
				Base:   decimal.RequireFromString("1020.00"),
				Amount: decimal.RequireFromString("102.00"),
			},
		},
	}
	plain := expectedStatement{Key: plainKey}

	t.Run("derives one record per adjustment line", func(t *testing.T) {
		got := expectedRecords([]expectedStatement{adjusted}, seeded)
		if len(got) != 2 {
			t.Fatalf("records = %+v, want the two lines of the statement", got)
		}
		for i, want := range []storedAdjustment{
			{
				projectID: key, relationID: relation.String(),
				relationType: relationType, relationTarget: beneficiary,
				typ: adjustment.TypeDiscount, scope: "all",
				rate: "0.150000", base: "1200.00", amount: "-180.00", currency: currency,
			},
			{
				projectID: key, relationID: relation.String(),
				relationType: relationType, relationTarget: beneficiary,
				beneficiary: beneficiary, hasBeneficiary: true,
				typ: adjustment.TypeKickback, scope: "all",
				rate: "0.100000", base: "1020.00", amount: "102.00", currency: currency,
			},
		} {
			if got[i] != want {
				t.Errorf("record %d = %+v, want %+v", i, got[i], want)
			}
		}
	})

	t.Run("derives no record for a statement without lines", func(t *testing.T) {
		if got := expectedRecords([]expectedStatement{plain}, seeded); len(got) != 0 {
			t.Errorf("records = %+v, want none", got)
		}
	})

	t.Run("accepts the kickback the statement describes", func(t *testing.T) {
		assertKickbacks(t, []export.Kickback{{
			Beneficiary:  beneficiary,
			Currency:     currency,
			StatementKey: key,
			Cloud:        "os-golden-x",
			ProjectID:    "proj-x",
			RelationID:   relation,
			Scope:        "all",
			Rate:         decimal.RequireFromString("0.10"),
			Base:         decimal.RequireFromString("1020.00"),
			Amount:       decimal.RequireFromString("102.00"),
		}}, []expectedStatement{adjusted}, seeded)
	})

	t.Run("accepts an empty settlement for a statement without lines", func(t *testing.T) {
		assertKickbacks(t, nil, []expectedStatement{plain}, seeded)
	})
}

// TestGolden runs the golden suite. Every case gets its own pair of databases
// inside the one pair of containers the fixture starts, so the cases share the
// containers without sharing anything they bill, and the image and the
// migration chains are paid for once rather than once per case.
//
// The cases of the loop meter one period once and read the result back. The
// ones after it carry a month further than that first pass.
func TestGolden(t *testing.T) {
	f := newGoldenFixture(t)

	for _, name := range []string{
		"instance_resize",
		"hetzner_upgrade",
		"volume_resize_retype",
		"shoot_scale_hibernate",
		"harbor_counters",
		"e2e_power_cycle",
		"related_costs",
		"virtual_relations",
	} {
		t.Run(name, func(t *testing.T) {
			c := loadCase(t, name)
			want := loadExpected(t, name)
			dbs := f.caseDatabases(t, name)

			seeded := seedRegistry(t, dbs, c.Registry)
			seedEvents(t, dbs, c.Events)

			result, err := runs.Execute(t.Context(), dbs.engine, dbs.source, c.options(t, want.Clouds))
			if err != nil {
				t.Fatalf("runs.Execute: %v", err)
			}
			assertClean(t, result)
			assertStats(t, result.Stats, want.Stats)
			assertUsage(t, dbs, result.RunID, want.Usage)

			run, err := export.Load(t.Context(), dbs.engine, result.RunID)
			if err != nil {
				t.Fatalf("export.Load: %v", err)
			}
			assertRated(t, run.Rated, want.Rated)
			assertStatements(t, run.Statements, want.Statements, want.AbsentStatements, seeded)
			assertAdjustmentRecords(t, dbs, result.RunID, want.Statements, seeded)
			assertKickbacks(t, run.Kickbacks, want.Statements, seeded)
		})
	}

	t.Run("correction_credit", func(t *testing.T) { goldenCorrectionCredit(t, f) })
	t.Run("reproducibility", func(t *testing.T) { goldenReproducibility(t, f) })
	t.Run("invariant_violation", func(t *testing.T) { goldenInvariantViolation(t, f) })
	t.Run("scheduler_drill", func(t *testing.T) { goldenSchedulerDrill(t, f) })
}

// goldenCorrectionCredit runs the correction chain of the concept over the
// pricing model the suite rates with: a month billed as one active interval,
// closed, and then reached by the power cycle that arrives after the invoice
// went out. The credit note it checks is the one README section 3.4 computes,
// down to the three deltas and the -24.00 they add up to, and the second
// correction after it checks that a period whose late events have been settled
// has nothing left to credit.
func goldenCorrectionCredit(t *testing.T, f goldenFixture) {
	const (
		cloud = "os-golden-credit"
		key   = cloud + "/proj-456"
	)

	c := loadCase(t, "correction_credit")
	dbs := f.caseDatabases(t, "correction_credit")
	ctx := t.Context()

	seedRegistry(t, dbs, c.Registry)
	seedEvents(t, dbs, c.Events)

	regular, err := runs.Execute(ctx, dbs.engine, dbs.source, c.options(t, []string{cloud}))
	if err != nil {
		t.Fatalf("runs.Execute: %v", err)
	}
	assertClean(t, regular)

	// What the month was billed at before anything arrived late: one interval
	// over the whole of March, the three gauges of the instance, and the egress
	// the model prices that nothing measured, billed at zero.
	run, err := export.Load(ctx, dbs.engine, regular.RunID)
	if err != nil {
		t.Fatalf("export.Load: %v", err)
	}
	wantAmounts := map[string]decimal.Decimal{
		"vcpus":     decimal.RequireFromString("59.52"),
		"ram_gb":    decimal.RequireFromString("29.76"),
		"disk_gb":   decimal.RequireFromString("59.52"),
		"egress_gb": decimal.RequireFromString("0.00"),
	}
	if len(run.Rated) != len(wantAmounts) {
		t.Fatalf("rated records = %d, want %d", len(run.Rated), len(wantAmounts))
	}
	byDimension := make(map[string]export.RatedRecord, len(run.Rated))
	for _, record := range run.Rated {
		byDimension[record.Dimension] = record
	}
	for dimension, want := range wantAmounts {
		record, rated := byDimension[dimension]
		if !rated {
			t.Errorf("no rated record for the %s of %s", dimension, key)
			continue
		}
		if !record.Amount.Equal(want) {
			t.Errorf("the %s amount = %s, want %s", dimension, record.Amount, want)
		}
		if !record.FromTS.Equal(periodFrom) {
			t.Errorf("the %s record starts at %s, want %s",
				dimension, instant(record.FromTS), instant(periodFrom))
		}
		if record.Currency != currency {
			t.Errorf("the currency of the %s record = %q, want %q", dimension, record.Currency, currency)
		}
	}
	if len(run.Statements) != 1 {
		t.Fatalf("statements = %d, want the one project of the case", len(run.Statements))
	}
	if got := run.Statements[0].Key; got != key {
		t.Errorf("statement key = %q, want %q", got, key)
	}
	if want := decimal.RequireFromString("148.80"); !run.Statements[0].Total.Equal(want) {
		t.Errorf("the total of %s = %s, want %s", key, run.Statements[0].Total, want)
	}

	kind, err := runs.Finalize(ctx, dbs.engine, periodFrom, regular.RunID)
	if err != nil {
		t.Fatalf("runs.Finalize: %v", err)
	}
	if kind != runs.KindRegular {
		t.Errorf("runs.Finalize closed a %q, want a %q", kind, runs.KindRegular)
	}
	status, finalized := periodRow(t, dbs)
	if status != "finalized" {
		t.Errorf("the billing period is %q, want finalized", status)
	}
	if finalized != regular.RunID {
		t.Errorf("the period was closed by %s, want the regular run %s", finalized, regular.RunID)
	}

	// The power cycle, arriving after the month was closed. Its events are
	// dated inside the period and reach the reporting database at an instant
	// past the one the finalized run read it at.
	seedLate(t, dbs, c)

	late, err := runs.DetectLate(ctx, dbs.engine, dbs.source, periodFrom, periodTo)
	if err != nil {
		t.Fatalf("runs.DetectLate: %v", err)
	}
	if late.RunID != regular.RunID {
		t.Errorf("the late events are held against %s, want the finalized run %s", late.RunID, regular.RunID)
	}
	if late.Kind != runs.KindRegular {
		t.Errorf("the run the late events are held against is a %q, want a %q", late.Kind, runs.KindRegular)
	}
	if late.Truncated != 0 {
		t.Errorf("Truncated = %d, want none: the case has one resource", late.Truncated)
	}
	if len(late.Resources) != 1 {
		t.Fatalf("late resources = %v, want the one instance the power cycle reached", late.Resources)
	}
	wantResource := source.Resource{
		Cloud: cloud, Platform: "openstack", ResourceType: "instance", ResourceID: "abc-123",
	}
	if late.Resources[0].Resource != wantResource {
		t.Errorf("the late resource = %+v, want %+v", late.Resources[0].Resource, wantResource)
	}
	if late.Resources[0].Events != 2 {
		t.Errorf("the late resource carries %d events, want the two of the power cycle",
			late.Resources[0].Events)
	}

	first, err := runs.Correct(ctx, dbs.engine, dbs.source, c.correctOptions(t))
	if err != nil {
		t.Fatalf("runs.Correct: %v", err)
	}
	if first.CorrectsRunID != regular.RunID {
		t.Errorf("CorrectsRunID = %s, want the finalized run %s", first.CorrectsRunID, regular.RunID)
	}
	if first.PricingVersion != pricingVersion {
		t.Errorf("PricingVersion = %q, want the %q the corrected run rated with",
			first.PricingVersion, pricingVersion)
	}
	assertNoWarnings(t, first.Stats.Stats)
	for _, count := range []struct {
		name      string
		got, want int
	}{
		{"deltas", first.Stats.Deltas, 3},
		{"usage_records", first.Stats.UsageRecords, 3},
		{"rated_records", first.Stats.RatedRecords, 12},
		{"statements", first.Stats.Statements, 1},
	} {
		if count.got != count.want {
			t.Errorf("correction stats %s = %d, want %d", count.name, count.got, count.want)
		}
	}

	credit, err := export.Load(ctx, dbs.engine, first.RunID)
	if err != nil {
		t.Fatalf("export.Load: %v", err)
	}
	// The three gauges the shutoff interval halved, in the order the diff sorts
	// its keys. The egress both passes rated at zero is no difference, so the
	// correction writes no delta for it.
	wantDeltas := []struct{ dimension, old, new, delta string }{
		{"disk_gb", "59.52", "49.92", "-9.60"},
		{"ram_gb", "29.76", "24.96", "-4.80"},
		{"vcpus", "59.52", "49.92", "-9.60"},
	}
	if len(credit.Deltas) != len(wantDeltas) {
		t.Fatalf("deltas = %d, want %d", len(credit.Deltas), len(wantDeltas))
	}
	for i, w := range wantDeltas {
		got := credit.Deltas[i]
		for _, field := range []struct{ name, got, want string }{
			{"dimension", got.Dimension, w.dimension},
			{"cloud", got.Cloud, cloud},
			{"resource_id", got.ResourceID, "abc-123"},
			{"project_id", got.ProjectID, "proj-456"},
			{"currency", got.Currency, currency},
		} {
			if field.got != field.want {
				t.Errorf("delta %d: %s = %q, want %q", i, field.name, field.got, field.want)
			}
		}
		for _, amount := range []struct {
			name string
			got  decimal.Decimal
			want string
		}{
			{"old", got.Old, w.old},
			{"new", got.New, w.new},
			// The embedded corrections.Delta carries a field of its own name,
			// so the difference is one level down from the export record.
			{"delta", got.Delta.Delta, w.delta},
		} {
			if want := decimal.RequireFromString(amount.want); !amount.got.Equal(want) {
				t.Errorf("delta %d: the %s amount of %s = %s, want %s",
					i, amount.name, w.dimension, amount.got, want)
			}
		}
	}

	if len(credit.Statements) != 1 {
		t.Fatalf("credit notes = %d, want the one project of the case", len(credit.Statements))
	}
	note := credit.Statements[0]
	if note.Key != key {
		t.Errorf("credit note key = %q, want %q", note.Key, key)
	}
	if want := decimal.RequireFromString("-24.00"); !note.Total.Equal(want) {
		t.Errorf("the total of the credit note of %s = %s, want %s", key, note.Total, want)
	}
	if note.Currency != currency {
		t.Errorf("the currency of the credit note of %s = %q, want %q", key, note.Currency, currency)
	}
	var document corrections.CreditNote
	if err := json.Unmarshal(note.Document, &document); err != nil {
		t.Fatalf("decoding the credit note of %s: %v", key, err)
	}
	if document.CorrectsRunID != regular.RunID.String() {
		t.Errorf("the credit note corrects run %s, want the finalized run %s",
			document.CorrectsRunID, regular.RunID)
	}
	// Who the note is for and what it covers. The period is written out rather
	// than taken from the options the correction was called with: a note stamped
	// with the month beside the one it credits would agree with those.
	assertBillingPeriod(t, "the credit note of "+key, document.BillingPeriod,
		expectedBillingPeriod{From: "2026-03-01T00:00:00Z", To: "2026-04-01T00:00:00Z"})
	for _, field := range []struct{ name, got, want string }{
		{"project", document.ProjectID, "proj-456"},
		{"platform", document.Platform, "openstack"},
	} {
		if field.got != field.want {
			t.Errorf("the %s of the credit note of %s = %q, want %q",
				field.name, key, field.got, field.want)
		}
	}
	if len(document.RelatedCosts) != 0 {
		t.Errorf("the credit note of %s carries the related costs %+v, want none: "+
			"the case registers one project", key, document.RelatedCosts)
	}

	// The body of the note, which is what the project reads. The deltas above
	// are the rows the correction wrote; these are the lines it rendered from
	// them, and nothing so far has compared the two.
	if len(document.LineItems) != 1 {
		t.Fatalf("the credit note of %s carries %d line items, want the one instance the power cycle reached",
			key, len(document.LineItems))
	}
	line := document.LineItems[0]
	for _, field := range []struct{ name, got, want string }{
		{"resource_type", line.ResourceType, "instance"},
		{"resource_id", line.ResourceID, "abc-123"},
		{"platform", line.Platform, "openstack"},
	} {
		if field.got != field.want {
			t.Errorf("the line of the credit note of %s: %s = %q, want %q",
				key, field.name, field.got, field.want)
		}
	}
	if want := decimal.RequireFromString("-24.00"); !line.Total.Equal(want) {
		t.Errorf("the total of the line of the credit note of %s = %s, want %s", key, line.Total, want)
	}
	if len(line.Dimensions) != len(wantDeltas) {
		t.Errorf("the line of the credit note of %s credits %d dimensions, want %d",
			key, len(line.Dimensions), len(wantDeltas))
	}
	for _, w := range wantDeltas {
		change, credited := line.Dimensions[w.dimension]
		if !credited {
			t.Errorf("the line of the credit note of %s credits no %s", key, w.dimension)
			continue
		}
		for _, amount := range []struct {
			name string
			got  decimal.Decimal
			want string
		}{
			{"old", change.Old.Decimal, w.old},
			{"new", change.New.Decimal, w.new},
			{"delta", change.Delta.Decimal, w.delta},
		} {
			if want := decimal.RequireFromString(amount.want); !amount.got.Equal(want) {
				t.Errorf("the %s of the %s on the credit note of %s = %s, want %s",
					amount.name, w.dimension, key, amount.got, want)
			}
		}
	}

	if kind, err = runs.Finalize(ctx, dbs.engine, periodFrom, first.RunID); err != nil {
		t.Fatalf("runs.Finalize: %v", err)
	} else if kind != runs.KindCorrection {
		t.Errorf("runs.Finalize closed a %q, want a %q", kind, runs.KindCorrection)
	}

	// The finalized correction is the period's truth now, and the power cycle
	// is in it, so correcting again finds nothing to credit.
	second, err := runs.Correct(ctx, dbs.engine, dbs.source, c.correctOptions(t))
	if err != nil {
		t.Fatalf("runs.Correct: %v", err)
	}
	assertNoWarnings(t, second.Stats.Stats)
	if second.CorrectsRunID != first.RunID {
		t.Errorf("CorrectsRunID = %s, want the finalized correction %s", second.CorrectsRunID, first.RunID)
	}
	if second.Stats.Deltas != 0 {
		t.Errorf("Stats.Deltas = %d, want none: the power cycle has been settled", second.Stats.Deltas)
	}
	if second.Stats.Statements != 0 {
		t.Errorf("Stats.Statements = %d, want none: there is nothing to credit", second.Stats.Statements)
	}
	settled, err := export.Load(ctx, dbs.engine, second.RunID)
	if err != nil {
		t.Fatalf("export.Load: %v", err)
	}
	if len(settled.Deltas) != 0 {
		t.Errorf("deltas = %+v, want none", settled.Deltas)
	}
	if len(settled.Statements) != 0 {
		t.Errorf("credit notes = %d, want none", len(settled.Statements))
	}
	if got := runStatus(t, dbs, second.RunID); got != "completed" {
		t.Errorf("status = %q, want completed: a correction that found nothing is a correction", got)
	}

	// Nothing has arrived since the correction closed, and the events that had
	// are below its snapshot, so the period reads as settled.
	late, err = runs.DetectLate(ctx, dbs.engine, dbs.source, periodFrom, periodTo)
	if err != nil {
		t.Fatalf("runs.DetectLate: %v", err)
	}
	if late.RunID != first.RunID {
		t.Errorf("the late events are held against %s, want the finalized correction %s",
			late.RunID, first.RunID)
	}
	if late.Kind != runs.KindCorrection {
		t.Errorf("the run the late events are held against is a %q, want a %q",
			late.Kind, runs.KindCorrection)
	}
	if len(late.Resources) != 0 {
		t.Errorf("late resources = %+v, want none: the correction re-metered them", late.Resources)
	}
}

// goldenReproducibility meters one period twice from the same events and checks
// that the two runs bill the same thing. It is the phase's third exit criterion:
// a rerun of a month is what an operator does after a failed pass, and a rerun
// that prices the month differently would make the invoice depend on when it was
// produced. Both runs are exported to disk, so what is compared is the artifact
// a customer receives and not only the rows behind it.
//
// The case it meters is the power cycle of the loop above, in databases of its
// own. Two runs that agree with each other and with nothing else would be
// reproducibly wrong, so the first of them is held against that case's
// expectations whole rather than against one total written out again here.
func goldenReproducibility(t *testing.T, f goldenFixture) {
	const (
		cloud = "os-golden-e2e"
		key   = cloud + "/proj-456"
	)

	c := loadCase(t, "e2e_power_cycle")
	want := loadExpected(t, "e2e_power_cycle")
	dbs := f.caseDatabases(t, "reproducibility")
	ctx := t.Context()

	seeded := seedRegistry(t, dbs, c.Registry)
	seedEvents(t, dbs, c.Events)
	dir := t.TempDir()

	// The second run supersedes the first, and export.Load refuses a superseded
	// run, so the first run is read and written out before the second opens.
	first, err := runs.Execute(ctx, dbs.engine, dbs.source, c.options(t, want.Clouds))
	if err != nil {
		t.Fatalf("runs.Execute: %v", err)
	}
	assertClean(t, first)
	assertStats(t, first.Stats, want.Stats)
	assertUsage(t, dbs, first.RunID, want.Usage)

	a, err := export.Load(ctx, dbs.engine, first.RunID)
	if err != nil {
		t.Fatalf("export.Load: %v", err)
	}
	assertRated(t, a.Rated, want.Rated)
	assertStatements(t, a.Statements, want.Statements, want.AbsentStatements, seeded)
	if err := (export.JSONFiles{Dir: filepath.Join(dir, "a")}).Export(ctx, a); err != nil {
		t.Fatalf("JSONFiles.Export: %v", err)
	}

	second, err := runs.Execute(ctx, dbs.engine, dbs.source, c.options(t, want.Clouds))
	if err != nil {
		t.Fatalf("runs.Execute: %v", err)
	}
	assertClean(t, second)
	if !slices.Equal(second.Superseded, []uuid.UUID{first.RunID}) {
		t.Errorf("Superseded = %v, want the first run %s", second.Superseded, first.RunID)
	}

	b, err := export.Load(ctx, dbs.engine, second.RunID)
	if err != nil {
		t.Fatalf("export.Load: %v", err)
	}
	if err := (export.JSONFiles{Dir: filepath.Join(dir, "b")}).Export(ctx, b); err != nil {
		t.Fatalf("JSONFiles.Export: %v", err)
	}

	if len(b.Statements) != len(a.Statements) {
		t.Fatalf("the second run billed %d statements, want the %d of the first",
			len(b.Statements), len(a.Statements))
	}
	for i, want := range a.Statements {
		got := b.Statements[i]
		if got.Key != want.Key {
			t.Errorf("statement %d: key = %q, want %q", i, got.Key, want.Key)
		}
		if !bytes.Equal(got.Document, want.Document) {
			t.Errorf("the document of %s = %s, want %s", want.Key, got.Document, want.Document)
		}
		if !got.Total.Equal(want.Total) {
			t.Errorf("the total of %s = %s, want %s", want.Key, got.Total, want.Total)
		}
		if got.Currency != want.Currency {
			t.Errorf("the currency of %s = %q, want %q", want.Key, got.Currency, want.Currency)
		}
	}

	if len(b.Rated) != len(a.Rated) {
		t.Fatalf("the second run rated %d records, want the %d of the first", len(b.Rated), len(a.Rated))
	}
	for i, want := range a.Rated {
		got := b.Rated[i]
		if got.Resource != want.Resource {
			t.Errorf("rated record %d: resource = %+v, want %+v", i, got.Resource, want.Resource)
		}
		for _, field := range []struct{ name, got, want string }{
			{"project_id", got.ProjectID, want.ProjectID},
			{"state", got.State, want.State},
			{"dimension", got.Dimension, want.Dimension},
			{"currency", got.Currency, want.Currency},
		} {
			if field.got != field.want {
				t.Errorf("rated record %d: %s = %q, want %q", i, field.name, field.got, field.want)
			}
		}
		if !got.FromTS.Equal(want.FromTS) {
			t.Errorf("rated record %d: from = %s, want %s", i, instant(got.FromTS), instant(want.FromTS))
		}
		if !got.ToTS.Equal(want.ToTS) {
			t.Errorf("rated record %d: to = %s, want %s", i, instant(got.ToTS), instant(want.ToTS))
		}
		if !got.Quantity.Equal(want.Quantity) {
			t.Errorf("rated record %d: the %s quantity = %s, want %s",
				i, want.Dimension, got.Quantity, want.Quantity)
		}
		if !got.Amount.Equal(want.Amount) {
			t.Errorf("rated record %d: the %s amount = %s, want %s",
				i, want.Dimension, got.Amount, want.Amount)
		}
	}

	document := export.DocumentFileName(runs.KindRegular, key)
	if want := "statement-os-golden-e2e%2Fproj-456.json"; document != want {
		t.Fatalf("export.DocumentFileName = %q, want %q", document, want)
	}
	documentA := readExported(t, filepath.Join(dir, "a", document))
	documentB := readExported(t, filepath.Join(dir, "b", document))
	if !bytes.Equal(documentA, documentB) {
		t.Errorf("the two exports of %s differ:\n%s\n%s", document, documentA, documentB)
	}

	// run_id, started_at, completed_at and stats.snapshot_at name the pass
	// rather than what it billed, and they differ between two runs of one
	// period by design. What has to agree is the pricing version and the index
	// of the documents, which is what exportedRun holds.
	indexA := readRunIndex(t, filepath.Join(dir, "a"))
	indexB := readRunIndex(t, filepath.Join(dir, "b"))
	for _, index := range []struct {
		dir string
		run exportedRun
	}{{dir: "a", run: indexA}, {dir: "b", run: indexB}} {
		if index.run.PricingVersion == nil {
			t.Fatalf("the run.json of %s carries no pricing version, want %q", index.dir, pricingVersion)
		}
		if *index.run.PricingVersion != pricingVersion {
			t.Errorf("the pricing version of %s = %q, want %q",
				index.dir, *index.run.PricingVersion, pricingVersion)
		}
	}
	if len(indexB.Statements) != len(indexA.Statements) {
		t.Fatalf("the run.json of the second run indexes %d statements, want the %d of the first",
			len(indexB.Statements), len(indexA.Statements))
	}
	for i, want := range indexA.Statements {
		got := indexB.Statements[i]
		for _, field := range []struct{ name, got, want string }{
			{"file", got.File, want.File},
			{"cloud", got.Cloud, want.Cloud},
			{"project_id", got.ProjectID, want.ProjectID},
			{"currency", got.Currency, want.Currency},
		} {
			if field.got != field.want {
				t.Errorf("indexed statement %d: %s = %q, want %q", i, field.name, field.got, field.want)
			}
		}
		if !got.Total.Equal(want.Total) {
			t.Errorf("indexed statement %d: total = %s, want %s", i, got.Total, want.Total)
		}
	}
}

// exportedRun is the part of an exported run.json two runs of one period have
// to agree on: the version they rated with, and the documents they wrote beside
// the projects those bill.
type exportedRun struct {
	PricingVersion *string            `json:"pricing_version"`
	Statements     []exportedDocument `json:"statements"`
}

// exportedDocument is one entry of the index run.json carries.
type exportedDocument struct {
	File      string          `json:"file"`
	Cloud     string          `json:"cloud"`
	ProjectID string          `json:"project_id"`
	Total     decimal.Decimal `json:"total"`
	Currency  string          `json:"currency"`
}

// readRunIndex reads the run.json one export wrote.
func readRunIndex(t *testing.T, dir string) exportedRun {
	t.Helper()

	path := filepath.Join(dir, "run.json")
	var index exportedRun
	decodeJSON(t, path, readExported(t, path), &index)
	return index
}

// readExported reads one file an export wrote, naming the path a failure is on.
func readExported(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the exported %s: %v", path, err)
	}
	return data
}

// goldenInvariantViolation is the negative case of the suite: a period the
// engine has to refuse. One instance of it carries an update after its delete,
// which reopens its timeline and bills time no life of the resource covers, so
// the coverage invariant is breached. The run must fail on that resource, name
// it alone, and leave nothing behind that a later step could mistake for a
// billed month: no records, no export, and a period still open.
func goldenInvariantViolation(t *testing.T, f goldenFixture) {
	const cloud = "os-golden-violation"

	c := loadCase(t, "invariant_violation")
	dbs := f.caseDatabases(t, "invariant_violation")
	ctx := t.Context()

	seedRegistry(t, dbs, c.Registry)
	seedEvents(t, dbs, c.Events)

	result, err := runs.Execute(ctx, dbs.engine, dbs.source, c.options(t, []string{cloud}))
	if err == nil {
		t.Fatal("runs.Execute error = nil, want the violating resource reported")
	}
	var violation *metering.ViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("runs.Execute error = %v, want a *metering.ViolationError", err)
	}
	// The sound instance shares the period with the broken one, and this length
	// is what says it was not swept up with it.
	if len(violation.Resources) != 1 {
		t.Fatalf("violating resources = %+v, want the one instance the update reopened",
			violation.Resources)
	}
	reported := violation.Resources[0]
	for _, field := range []struct{ name, got, want string }{
		{"cloud", reported.Cloud, cloud},
		{"resource_type", reported.ResourceType, "instance"},
		{"resource_id", reported.ResourceID, "i-broken"},
	} {
		if field.got != field.want {
			t.Errorf("the violating resource: %s = %q, want %q", field.name, field.got, field.want)
		}
	}
	if !slices.ContainsFunc(reported.Violations, func(v invariants.Violation) bool {
		return v.Invariant == invariants.InvariantCoverage
	}) {
		t.Errorf("the violations of i-broken = %+v, want one of %q",
			reported.Violations, invariants.InvariantCoverage)
	}

	if result.RunID == uuid.Nil {
		t.Fatal("runs.Execute reported no run id, want the run it opened and failed")
	}
	if got := runStatus(t, dbs, result.RunID); got != "failed" {
		t.Errorf("status = %q, want failed", got)
	}
	// The stats are what an operator reads the period from, so they carry the
	// same report the caller got.
	if !reflect.DeepEqual(result.Stats.Violations, violation.Resources) {
		t.Errorf("stats violations = %+v, want the reported %+v",
			result.Stats.Violations, violation.Resources)
	}
	if result.Stats.Error != err.Error() {
		t.Errorf("stats error = %q, want the failure %q", result.Stats.Error, err.Error())
	}

	for _, table := range []string{"usage_records", "rated_records", "project_statements"} {
		if got := countRows(t, dbs, table, result.RunID); got != 0 {
			t.Errorf("the failed run holds %d rows in %s, want none", got, table)
		}
	}

	if _, err := export.Load(ctx, dbs.engine, result.RunID); !errors.Is(err, export.ErrRunNotExportable) {
		t.Errorf("export.Load error = %v, want one matching export.ErrRunNotExportable", err)
	}

	if status, _ := periodRow(t, dbs); status != "open" {
		t.Errorf("the billing period is %q, want open: a month nothing billed is still to be billed",
			status)
	}

	// The whole period is refused, so the instance whose history does describe a
	// life is not billed either, by this run or by any other.
	var sound int
	if err := dbs.engine.QueryRow(ctx,
		`SELECT count(*) FROM usage_records WHERE resource_id = 'i-sound'`).Scan(&sound); err != nil {
		t.Fatalf("counting the usage records of i-sound: %v", err)
	}
	if sound != 0 {
		t.Errorf("i-sound holds %d usage records, want none", sound)
	}
}

// goldenSchedulerDrill is the phase's fifth exit criterion: one month
// carried through an operator's calendar by the tick. March is moved into grace
// when it ends, left alone for the whole grace window, metered at the end of it
// and not metered again, finalized by hand, credited over the power cycle that
// arrives after the invoice, and then walked once more without being touched.
// The clock is an argument of scheduler.Tick, so the days of April are stepped
// through in one test.
func goldenSchedulerDrill(t *testing.T, f goldenFixture) {
	const (
		cloud = "os-golden-drill"
		key   = cloud + "/proj-456"
	)

	c := loadCase(t, "scheduler_drill")
	dbs := f.caseDatabases(t, "scheduler_drill")
	ctx := t.Context()

	seedRegistry(t, dbs, c.Registry)
	seedEvents(t, dbs, c.Events)

	// The executor the CLI wires runs.Execute into. It counts its calls, because
	// half of what the drill checks is the ticks that do not meter.
	calls := 0
	exec := func(ctx context.Context, from, to time.Time) (uuid.UUID, error) {
		calls++
		if !from.Equal(periodFrom) || !to.Equal(periodTo) {
			t.Errorf("the tick metered [%s, %s), want the March of the case [%s, %s)",
				instant(from), instant(to), instant(periodFrom), instant(periodTo))
		}
		opts := c.options(t, []string{cloud})
		opts.PeriodFrom, opts.PeriodTo = from, to
		r, err := runs.Execute(ctx, dbs.engine, dbs.source, opts)
		return r.RunID, err
	}
	tick := func(now time.Time) scheduler.Report {
		report, err := scheduler.Tick(ctx, dbs.engine, now, scheduler.Options{
			GraceHours: 72, AutoFinalize: false, Execute: exec,
		})
		if err != nil {
			t.Fatalf("scheduler.Tick at %s: %v", instant(now), err)
		}
		return report
	}
	// month is the single entry every tick past the end of March reports. A
	// second month in the walk, a failure or a warning is a finding of its own,
	// so they are checked here rather than at each of the steps below.
	month := func(report scheduler.Report, now time.Time) scheduler.MonthReport {
		if len(report) != 1 {
			t.Fatalf("the tick at %s walked %d months, want the one of the case",
				instant(now), len(report))
		}
		got := report[0]
		if got.Month != "2026-03" {
			t.Errorf("the tick at %s walked %q, want 2026-03", instant(now), got.Month)
		}
		if got.Err != nil {
			t.Errorf("the tick at %s failed the month: %v", instant(now), got.Err)
		}
		if got.Warning != nil {
			t.Errorf("the tick at %s warned: %v", instant(now), got.Warning)
		}
		return got
	}

	// The row tally-engine run --period writes. The walk starts at the earliest
	// billing period the engine knows, so without it a tick would walk the month
	// before its clock rather than the March of the case.
	if _, err := dbs.engine.Exec(ctx,
		`INSERT INTO billing_periods (period_from, period_to, status) VALUES ($1, $2, 'open')`,
		periodFrom, periodTo); err != nil {
		t.Fatalf("recording the billing period %s: %v", instant(periodFrom), err)
	}

	// The last second of March: the month has not ended, so nothing is due.
	if report := tick(time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)); len(report) != 0 {
		t.Errorf("the tick inside March walked %+v, want nothing", report)
	}
	if status, _ := periodRow(t, dbs); status != "open" {
		t.Errorf("the billing period is %q, want open", status)
	}
	if got := countRuns(t, dbs); got != 0 {
		t.Errorf("the period holds %d runs, want none", got)
	}
	if calls != 0 {
		t.Errorf("the executor was called %d times, want none inside March", calls)
	}

	// The first instant of April: March has ended, and its open phase with it.
	now := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	ended := month(tick(now), now)
	if want := "open -> grace"; ended.Transition != want {
		t.Errorf("the tick at the end of March reported %q, want %q", ended.Transition, want)
	}
	if ended.RunID != uuid.Nil {
		t.Errorf("the tick at the end of March metered run %s, want the grace window first", ended.RunID)
	}
	if status, _ := periodRow(t, dbs); status != "grace" {
		t.Errorf("the billing period is %q, want grace", status)
	}
	if calls != 0 {
		t.Errorf("the executor was called %d times, want none: the grace window has just opened", calls)
	}

	// The last second of the 72 hours late events are waited for.
	now = time.Date(2026, 4, 3, 23, 59, 59, 0, time.UTC)
	waiting := month(tick(now), now)
	if waiting.Transition != "" {
		t.Errorf("the tick inside the grace window reported the transition %q, want none",
			waiting.Transition)
	}
	if waiting.RunID != uuid.Nil {
		t.Errorf("the tick inside the grace window metered run %s, want nothing yet", waiting.RunID)
	}
	if calls != 0 {
		t.Errorf("the executor was called %d times, want none inside the grace window", calls)
	}
	if got := countRuns(t, dbs); got != 0 {
		t.Errorf("the period holds %d runs, want none inside the grace window", got)
	}

	// The grace window has passed: this is the tick that bills the month.
	now = time.Date(2026, 4, 4, 0, 0, 0, 0, time.UTC)
	billed := month(tick(now), now)
	if billed.RunID == uuid.Nil {
		t.Fatal("the tick at the end of the grace window metered no run, want the regular run of March")
	}
	regular := billed.RunID
	if billed.Finalized {
		t.Error("the tick closed March, want it left open: AutoFinalize is off")
	}
	if calls != 1 {
		t.Errorf("the executor was called %d times, want once", calls)
	}
	if status, _ := periodRow(t, dbs); status != "grace" {
		t.Errorf("the billing period is %q, want grace: an operator closes a metered month", status)
	}
	run, err := export.Load(ctx, dbs.engine, regular)
	if err != nil {
		t.Fatalf("export.Load: %v", err)
	}
	if run.Status != "completed" {
		t.Errorf("the metered run is %q, want completed", run.Status)
	}
	if len(run.Statements) != 1 {
		t.Fatalf("statements = %d, want the one project of the case", len(run.Statements))
	}
	if got := run.Statements[0].Key; got != key {
		t.Errorf("statement key = %q, want %q", got, key)
	}
	if want := decimal.RequireFromString("148.80"); !run.Statements[0].Total.Equal(want) {
		t.Errorf("the total of %s = %s, want %s", key, run.Statements[0].Total, want)
	}

	// The next hourly pass finds a month that carries a completed run, and such
	// a month is not metered a second time.
	now = time.Date(2026, 4, 5, 0, 0, 0, 0, time.UTC)
	if got := month(tick(now), now); got.RunID != uuid.Nil {
		t.Errorf("the tick after the run metered %s, want nothing", got.RunID)
	}
	if calls != 1 {
		t.Errorf("the executor was called %d times, want the one of the grace step", calls)
	}
	if got := countRuns(t, dbs); got != 1 {
		t.Errorf("the period holds %d runs, want the one the tick metered", got)
	}

	kind, err := runs.Finalize(ctx, dbs.engine, periodFrom, regular)
	if err != nil {
		t.Fatalf("runs.Finalize: %v", err)
	}
	if kind != runs.KindRegular {
		t.Errorf("runs.Finalize closed a %q, want a %q", kind, runs.KindRegular)
	}
	status, finalized := periodRow(t, dbs)
	if status != "finalized" {
		t.Errorf("the billing period is %q, want finalized", status)
	}
	if finalized != regular {
		t.Errorf("the period was closed by %s, want the metered run %s", finalized, regular)
	}

	// The power cycle, arriving after the month was closed.
	seedLate(t, dbs, c)

	late, err := runs.DetectLate(ctx, dbs.engine, dbs.source, periodFrom, periodTo)
	if err != nil {
		t.Fatalf("runs.DetectLate: %v", err)
	}
	if late.RunID != regular {
		t.Errorf("the late events are held against %s, want the finalized run %s", late.RunID, regular)
	}
	if late.Kind != runs.KindRegular {
		t.Errorf("the run the late events are held against is a %q, want a %q", late.Kind, runs.KindRegular)
	}
	if late.Truncated != 0 {
		t.Errorf("Truncated = %d, want none: the case has one resource", late.Truncated)
	}
	if len(late.Resources) != 1 {
		t.Fatalf("late resources = %v, want the one instance the power cycle reached", late.Resources)
	}
	wantResource := source.Resource{
		Cloud: cloud, Platform: "openstack", ResourceType: "instance", ResourceID: "abc-123",
	}
	if late.Resources[0].Resource != wantResource {
		t.Errorf("the late resource = %+v, want %+v", late.Resources[0].Resource, wantResource)
	}
	if late.Resources[0].Events != 2 {
		t.Errorf("the late resource carries %d events, want the two of the power cycle",
			late.Resources[0].Events)
	}

	correction, err := runs.Correct(ctx, dbs.engine, dbs.source, c.correctOptions(t))
	if err != nil {
		t.Fatalf("runs.Correct: %v", err)
	}
	assertNoWarnings(t, correction.Stats.Stats)
	if correction.CorrectsRunID != regular {
		t.Errorf("CorrectsRunID = %s, want the finalized run %s", correction.CorrectsRunID, regular)
	}
	if correction.Stats.Deltas != 3 {
		t.Errorf("Stats.Deltas = %d, want the three gauges the shutoff interval halved",
			correction.Stats.Deltas)
	}

	credit, err := export.Load(ctx, dbs.engine, correction.RunID)
	if err != nil {
		t.Fatalf("export.Load: %v", err)
	}
	// The three gauges of the instance, in the order the diff sorts its keys.
	wantDeltas := []struct{ dimension, delta string }{
		{"disk_gb", "-9.60"},
		{"ram_gb", "-4.80"},
		{"vcpus", "-9.60"},
	}
	if len(credit.Deltas) != len(wantDeltas) {
		t.Fatalf("deltas = %d, want %d", len(credit.Deltas), len(wantDeltas))
	}
	for i, w := range wantDeltas {
		got := credit.Deltas[i]
		if got.Dimension != w.dimension {
			t.Errorf("delta %d: dimension = %q, want %q", i, got.Dimension, w.dimension)
		}
		// The embedded corrections.Delta carries a field of its own name, so the
		// difference is one level down from the export record.
		if want := decimal.RequireFromString(w.delta); !got.Delta.Delta.Equal(want) {
			t.Errorf("delta %d: the %s difference = %s, want %s",
				i, w.dimension, got.Delta.Delta, want)
		}
	}
	if len(credit.Statements) != 1 {
		t.Fatalf("credit notes = %d, want the one project of the case", len(credit.Statements))
	}
	note := credit.Statements[0]
	if note.Key != key {
		t.Errorf("credit note key = %q, want %q", note.Key, key)
	}
	if want := decimal.RequireFromString("-24.00"); !note.Total.Equal(want) {
		t.Errorf("the total of the credit note of %s = %s, want %s", key, note.Total, want)
	}
	// The month the note credits, written out rather than taken from the options
	// the correction was called with: a correction derives its period from the
	// run it corrects, and a note stamped with the month beside this one would
	// carry every amount above it unchanged.
	var document corrections.CreditNote
	if err := json.Unmarshal(note.Document, &document); err != nil {
		t.Fatalf("decoding the credit note of %s: %v", key, err)
	}
	assertBillingPeriod(t, "the credit note of "+key, document.BillingPeriod,
		expectedBillingPeriod{From: "2026-03-01T00:00:00Z", To: "2026-04-01T00:00:00Z"})
	for _, field := range []struct{ name, got, want string }{
		{"project", document.ProjectID, "proj-456"},
		{"platform", document.Platform, "openstack"},
		{"corrects_run_id", document.CorrectsRunID, regular.String()},
	} {
		if field.got != field.want {
			t.Errorf("the %s of the credit note of %s = %q, want %q",
				field.name, key, field.got, field.want)
		}
	}

	// A finalized month is walked and left alone. The correction an operator ran
	// against it stands, and the tick meters nothing.
	now = time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC)
	closed := month(tick(now), now)
	if closed.Transition != "" {
		t.Errorf("the tick after the finalization reported the transition %q, want none",
			closed.Transition)
	}
	if closed.RunID != uuid.Nil {
		t.Errorf("the tick after the finalization named run %s, want none", closed.RunID)
	}
	if closed.Finalized {
		t.Error("the tick closed March, want it already closed by the operator")
	}
	if calls != 1 {
		t.Errorf("the executor was called %d times, want the one of the grace step", calls)
	}
	if got := runStatus(t, dbs, correction.RunID); got != "completed" {
		t.Errorf("the correction is %q, want completed", got)
	}
	if status, _ := periodRow(t, dbs); status != "finalized" {
		t.Errorf("the billing period is %q, want finalized", status)
	}
}

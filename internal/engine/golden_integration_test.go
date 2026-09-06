// The golden suite: the worked examples of the concept, run end to end through
// the engine. A case seeds events into a reporting database, meters, measures,
// rates and attributes them through runs.Execute, and checks the usage records,
// the amounts and the statement documents that come out against numbers written
// down by hand.
//
// Every case bills March 2026, [2026-03-01T00:00:00Z, 2026-04-01T00:00:00Z),
// which is the period docs/explanation/worked-examples.md computes its
// examples over, and the temporal case bills the April after it as well. The
// expectations come from that section, from WP 5.6 of
// roadmap/05-phase-5-commercial-pricing.md (the commercial cases) and from
// pricing/2026-03.yaml, never from a previous run: a failure here is a report
// about the engine, so the answer to one is to find what changed in the
// engine, not to write the engine's new number into expected.json.
package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	"github.com/b42labs/tally/internal/engine/statements"
	"github.com/b42labs/tally/internal/reporting/httpapi"
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
		"reseller",
		"scoped_discount",
		"inherited_member_discount",
		// registry.json lists the three adjustments of this case kickback
		// first, against the surcharge → discount → kickback order
		// expected.json chains the bases in. The reversed input is what proves
		// the engine applies its canonical order rather than the order of the
		// document, so tidying the fixture to match expected.json would leave
		// the case passing without testing what it is named after.
		"order_and_stacking",
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
	t.Run("temporal", func(t *testing.T) { goldenTemporal(t, f) })
	t.Run("phase3_regression", func(t *testing.T) { goldenPhase3Regression(t, f) })
	t.Run("adjusted_reproducibility", func(t *testing.T) { goldenAdjustedReproducibility(t, f) })
	t.Run("auditability_drill", func(t *testing.T) { goldenAuditabilityDrill(t, f) })
	t.Run("adjusted_correction", func(t *testing.T) { goldenAdjustedCorrection(t, f) })
}

// goldenCorrectionCredit runs the correction chain of the concept over the
// pricing model the suite rates with: a month billed as one active interval,
// closed, and then reached by the power cycle that arrives after the invoice
// went out. The credit note it checks is the one
// docs/explanation/worked-examples.md computes, down to the three deltas and
// the -24.00 they add up to, and the second correction after it checks that a
// period whose late events have been settled has nothing left to credit.
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

	assertSameStatements(t, b.Statements, a.Statements, "the second run")

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

// assertSameStatements checks that two runs of one period billed the same
// statements: the same keys, the same documents byte for byte, the same totals
// and the same currency. what names the run the statements in got came from, so
// a failure says which of the two runs of a drill differs.
func assertSameStatements(t *testing.T, got, want []statements.Statement, what string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s billed %d statements, want %d", what, len(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if g.Key != w.Key {
			t.Errorf("statement %d: key = %q, want %q", i, g.Key, w.Key)
		}
		if !bytes.Equal(g.Document, w.Document) {
			t.Errorf("the document of %s = %s, want %s", w.Key, g.Document, w.Document)
		}
		if !g.Total.Equal(w.Total) {
			t.Errorf("the total of %s = %s, want %s", w.Key, g.Total, w.Total)
		}
		if g.Currency != w.Currency {
			t.Errorf("the currency of %s = %q, want %q", w.Key, g.Currency, w.Currency)
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

// settlement is the part of a kickbacks.json a drill reads: what each partner
// is owed for the month, and over how many projects that total was summed.
type settlement struct {
	Beneficiaries []struct {
		Beneficiary   string          `json:"beneficiary"`
		KickbackTotal decimal.Decimal `json:"kickback_total"`
		Projects      int             `json:"projects"`
	} `json:"beneficiaries"`
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

// goldenTemporal bills one instance over two months across a relation that
// closes inside the first of them. Decision D4 of
// roadmap/03-phase-3-metering-rating.md holds a relation to a period iff
// valid_from < period_to AND (valid_to IS NULL OR valid_to > period_from), so
// the edge that ends on 2026-03-15 applies to March whole and to April not at
// all: the day it closed on does not prorate the month it lies in.
//
// It is a case of its own because the loop above meters the March the period
// constants name, and this one bills April on top of it in the same databases.
// Both months rate with the model the suite imports: PricingModelForPeriod
// (internal/engine/store/queries.sql) takes the latest model whose valid_from
// is at or before the start of the period, so the April run reports the 2026-03
// that assertClean holds every run of the suite to. The two are regular runs of
// different periods, so the second supersedes nothing the first billed.
func goldenTemporal(t *testing.T, f goldenFixture) {
	c := loadCase(t, "temporal")
	march := loadExpectedFile(t, "temporal", "expected.json")
	april := loadExpectedFile(t, "temporal", "expected_april.json")
	dbs := f.caseDatabases(t, "temporal")
	ctx := t.Context()

	seeded := seedRegistry(t, dbs, c.Registry)
	seedEvents(t, dbs, c.Events)

	// One month billed and held to the file that carries its numbers.
	check := func(opts runs.Options, want expectedFile) (runs.Result, export.Run) {
		result, err := runs.Execute(ctx, dbs.engine, dbs.source, opts)
		if err != nil {
			t.Fatalf("runs.Execute: %v", err)
		}
		assertClean(t, result)
		assertStats(t, result.Stats, want.Stats)
		assertUsage(t, dbs, result.RunID, want.Usage)

		run, err := export.Load(ctx, dbs.engine, result.RunID)
		if err != nil {
			t.Fatalf("export.Load: %v", err)
		}
		assertRated(t, run.Rated, want.Rated)
		assertStatements(t, run.Statements, want.Statements, want.AbsentStatements, seeded)
		assertAdjustmentRecords(t, dbs, result.RunID, want.Statements, seeded)
		assertKickbacks(t, run.Kickbacks, want.Statements, seeded)
		return result, run
	}

	// March, the month the relation's last day lies in: the discount is applied
	// to the whole of it, and the invoice is the net cost it leaves.
	check(c.options(t, march.Clouds), march)

	// April, the month after the relation closed. The same events, the same
	// model, and a statement at the base cost.
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	opts := c.options(t, april.Clouds)
	opts.PeriodFrom, opts.PeriodTo = from, to
	result, run := check(opts, april)
	if result.Stats.AdjustmentRecords != 0 {
		t.Errorf("Stats.AdjustmentRecords = %d, want none: the relation closed before April",
			result.Stats.AdjustmentRecords)
	}
	if records := readAdjustmentRecords(t, dbs, result.RunID); len(records) != 0 {
		t.Errorf("the April run stored the adjustment records %+v, want none", records)
	}
	if len(run.Kickbacks) != 0 {
		t.Errorf("the April run settles the kickbacks %+v, want none", run.Kickbacks)
	}
}

// goldenPhase3Regression is the sixth row of WP 5.6, the phase's golden suite
// table: a graph whose relations carry no adjustment leaves every statement at
// the bytes Phase 3 rendered. "Byte-identical to the Phase 3 goldens" is proven
// here as byte equality between two runs over the virtual_relations case, whose
// managed_by and member_of edges hold no pricing_adjustments: one run with the
// adjustment walk on, the defaults every case of the loop runs with, and one
// with it off, which is the render path Phase 3 shipped. Without a relation
// type to walk, runs.produce builds no adjuster and statements.Build renders
// the document without one.
//
// Two runs that agree with each other and with nothing else would agree on the
// wrong document, so both are also held to that case's expected.json, which
// carries the Phase 3 numbers.
func goldenPhase3Regression(t *testing.T, f goldenFixture) {
	c := loadCase(t, "virtual_relations")
	want := loadExpected(t, "virtual_relations")
	dbs := f.caseDatabases(t, "phase3_regression")
	ctx := t.Context()

	seeded := seedRegistry(t, dbs, c.Registry)
	seedEvents(t, dbs, c.Events)
	dir := t.TempDir()

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
	// No statement of the file lists base_cost, so this is what says the walk
	// left the four commercial members off the document it reached.
	assertStatements(t, a.Statements, want.Statements, want.AbsentStatements, seeded)
	assertAdjustmentRecords(t, dbs, first.RunID, want.Statements, seeded)
	assertKickbacks(t, a.Kickbacks, want.Statements, seeded)
	if first.Stats.AdjustmentRecords != 0 {
		t.Errorf("Stats.AdjustmentRecords = %d, want none: the edges of the case adjust nothing",
			first.Stats.AdjustmentRecords)
	}
	if records := readAdjustmentRecords(t, dbs, first.RunID); len(records) != 0 {
		t.Errorf("the walking run stored the adjustment records %+v, want none", records)
	}
	if len(a.Kickbacks) != 0 {
		t.Errorf("the walking run settles the kickbacks %+v, want none", a.Kickbacks)
	}
	// The second run supersedes the first, and export.Load refuses a superseded
	// run, so the first run is read and written out before the second opens.
	if err := (export.JSONFiles{Dir: filepath.Join(dir, "a")}).Export(ctx, a); err != nil {
		t.Fatalf("JSONFiles.Export: %v", err)
	}

	// The same period without a relation type to walk: adjustments are off.
	opts := c.options(t, want.Clouds)
	opts.AdjustmentRelationTypes = nil
	second, err := runs.Execute(ctx, dbs.engine, dbs.source, opts)
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

	assertSameStatements(t, b.Statements, a.Statements, "the run without the walk")

	// The document a customer receives, on disk. kickbacks.json names the run it
	// was settled from, so it is not the file the two exports are compared on.
	document := export.DocumentFileName(runs.KindRegular, "gd-golden-virtual/team-alpha")
	if want := "statement-gd-golden-virtual%2Fteam-alpha.json"; document != want {
		t.Fatalf("export.DocumentFileName = %q, want %q", document, want)
	}
	documentA := readExported(t, filepath.Join(dir, "a", document))
	documentB := readExported(t, filepath.Join(dir, "b", document))
	if !bytes.Equal(documentA, documentB) {
		t.Errorf("the two exports of %s differ:\n%s\n%s", document, documentA, documentB)
	}

	if len(b.Kickbacks) != 0 {
		t.Errorf("the run without the walk settles the kickbacks %+v, want none", b.Kickbacks)
	}
	if records := readAdjustmentRecords(t, dbs, second.RunID); len(records) != 0 {
		t.Errorf("the run without the walk stored the adjustment records %+v, want none", records)
	}
}

// goldenAdjustedReproducibility meters one adjusted period twice from the same
// events and checks that the two runs bill and settle the same thing. It is the
// phase's second exit criterion: a rerun of a month is what an operator does
// after a failed pass, and with adjustments in play a rerun that settled a
// partner differently would pay that partner twice or at another amount. So the
// drill compares the invoice, the rows a payout is summed from and the
// settlement documents.
//
// The case it meters is the inherited member discount of the loop above, in
// databases of its own. Two runs that agree with each other and with nothing
// else would be reproducibly wrong, so the first of them is held against that
// case's expectations whole rather than against a total written out again here.
func goldenAdjustedReproducibility(t *testing.T, f goldenFixture) {
	c := loadCase(t, "inherited_member_discount")
	want := loadExpected(t, "inherited_member_discount")
	dbs := f.caseDatabases(t, "adjusted_reproducibility")
	ctx := t.Context()

	seeded := seedRegistry(t, dbs, c.Registry)
	seedEvents(t, dbs, c.Events)
	dir := t.TempDir()

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
	assertAdjustmentRecords(t, dbs, first.RunID, want.Statements, seeded)
	assertKickbacks(t, a.Kickbacks, want.Statements, seeded)
	// The second run supersedes the first, and export.Load refuses a superseded
	// run, so the first run is read and written out before the second opens.
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

	assertSameStatements(t, b.Statements, a.Statements, "the second run")

	// The rows a payout is summed from, compared whole. DeepEqual is sound over
	// these two slices: the query orders both, it reads back every column but id
	// and run_id, and the relation ids are the registry's, which both runs read
	// the same. So the two slices are equal exactly where the two runs adjusted
	// the same.
	recordsA := readAdjustmentRecords(t, dbs, first.RunID)
	recordsB := readAdjustmentRecords(t, dbs, second.RunID)
	if !reflect.DeepEqual(recordsA, recordsB) {
		t.Errorf("the second run stored the adjustment records %+v, want the %+v of the first",
			recordsB, recordsA)
	}

	if len(b.Kickbacks) != len(a.Kickbacks) {
		t.Fatalf("the second run settles %d kickbacks, want the %d of the first",
			len(b.Kickbacks), len(a.Kickbacks))
	}
	for i, want := range a.Kickbacks {
		got := b.Kickbacks[i]
		for _, field := range []struct{ name, got, want string }{
			{"beneficiary", got.Beneficiary, want.Beneficiary},
			{"currency", got.Currency, want.Currency},
			{"statement_key", got.StatementKey, want.StatementKey},
			{"cloud", got.Cloud, want.Cloud},
			{"project_id", got.ProjectID, want.ProjectID},
			{"scope", got.Scope, want.Scope},
		} {
			if field.got != field.want {
				t.Errorf("kickback %d: %s = %q, want %q", i, field.name, field.got, field.want)
			}
		}
		if got.RelationID != want.RelationID {
			t.Errorf("kickback %d: relation_id = %s, want %s", i, got.RelationID, want.RelationID)
		}
		for _, amount := range []struct {
			name      string
			got, want decimal.Decimal
		}{
			{"rate", got.Rate, want.Rate},
			{"base", got.Base, want.Base},
			{"amount", got.Amount, want.Amount},
		} {
			if !amount.got.Equal(amount.want) {
				t.Errorf("kickback %d: %s = %s, want %s", i, amount.name, amount.got, amount.want)
			}
		}
	}

	// The three documents the customers receive, on disk.
	for _, statement := range []struct{ key, file string }{
		{"os-golden-member/proj-alpha-1", "statement-os-golden-member%2Fproj-alpha-1.json"},
		{"os-golden-member/proj-alpha-2", "statement-os-golden-member%2Fproj-alpha-2.json"},
		{"os-golden-member/proj-beta", "statement-os-golden-member%2Fproj-beta.json"},
	} {
		document := export.DocumentFileName(runs.KindRegular, statement.key)
		if document != statement.file {
			t.Fatalf("export.DocumentFileName = %q, want %q", document, statement.file)
		}
		documentA := readExported(t, filepath.Join(dir, "a", document))
		documentB := readExported(t, filepath.Join(dir, "b", document))
		if !bytes.Equal(documentA, documentB) {
			t.Errorf("the two exports of %s differ:\n%s\n%s", document, documentA, documentB)
		}
	}

	// The documents the partner is paid from. Both name the run they belong to,
	// and nothing else in them differs between two runs of one period, so each
	// is compared with its own run id replaced by one placeholder.
	jsonA, err := export.KickbacksJSON(a)
	if err != nil {
		t.Fatalf("export.KickbacksJSON: %v", err)
	}
	jsonB, err := export.KickbacksJSON(b)
	if err != nil {
		t.Fatalf("export.KickbacksJSON: %v", err)
	}
	csvA, err := export.KickbacksCSV(a)
	if err != nil {
		t.Fatalf("export.KickbacksCSV: %v", err)
	}
	csvB, err := export.KickbacksCSV(b)
	if err != nil {
		t.Fatalf("export.KickbacksCSV: %v", err)
	}
	anonymize := func(document []byte, run export.Run) []byte {
		return bytes.ReplaceAll(document, []byte(run.ID.String()), []byte("<run>"))
	}
	for _, settled := range []struct {
		name string
		a, b []byte
	}{
		{"kickbacks.json", anonymize(jsonA, a), anonymize(jsonB, b)},
		{"kickbacks.csv", anonymize(csvA, a), anonymize(csvB, b)},
	} {
		if !bytes.Equal(settled.a, settled.b) {
			t.Errorf("the two settlements in %s differ:\n%s\n%s", settled.name, settled.a, settled.b)
		}
	}

	// What the partner is owed for the month, read off both settlements: one
	// beneficiary, the 14.14 of proj-alpha-1 and the 7.07 of proj-alpha-2, over
	// the two projects those came off.
	for _, document := range []struct {
		name string
		data []byte
	}{
		{"kickbacks.json of a", jsonA},
		{"kickbacks.json of b", jsonB},
	} {
		var got settlement
		decodeJSON(t, document.name, document.data, &got)
		if len(got.Beneficiaries) != 1 {
			t.Fatalf("the %s settles %d beneficiaries, want the one partner of the case",
				document.name, len(got.Beneficiaries))
		}
		entry := got.Beneficiaries[0]
		if entry.Beneficiary != "partner-corp" {
			t.Errorf("the %s settles %q, want %q", document.name, entry.Beneficiary, "partner-corp")
		}
		if want := decimal.RequireFromString("21.21"); !entry.KickbackTotal.Equal(want) {
			t.Errorf("the kickback total of %s = %s, want %s", document.name, entry.KickbackTotal, want)
		}
		if entry.Projects != 2 {
			t.Errorf("the kickback total of %s came off %d projects, want the two of the group",
				document.name, entry.Projects)
		}
	}
}

// goldenAuditabilityDrill answers the question an operator is asked about an
// invoice, "why does this project get 15 % off?", out of stored data alone. It
// is the phase's third exit criterion. Every adjustment_records row names the
// relation it came from by id, so the walk starts at a row of an adjusted
// statement, resolves that statement's project through
// GET /api/v1/projects?cloud=&external_id=, and finds the relation by that id in
// GET /api/v1/projects/{id}/relations?direction=outgoing&at=<period start>.
//
// The contract defines no GET on a single relation: api/reporting/openapi.yaml
// carries a patch and a delete under that path and nothing else, so the drill
// walks the two list endpoints instead (author's decision of 2026-08-31, named
// here per guardrail 10 of roadmap/00-conventions.md).
//
// The at is the start of the period the row was billed in, because a relation
// closed inside the month is no longer valid at now. The relation of the reseller
// case is open, so the drill also reads it without at and holds that walk to the
// same relation.
//
// The answer then stands on what the served relation carries: a managed_by edge
// to partner-corp whose metadata spells the "Reseller end-customer discount" the
// row was computed from.
func goldenAuditabilityDrill(t *testing.T, f goldenFixture) {
	c := loadCase(t, "reseller")
	want := loadExpected(t, "reseller")
	dbs := f.caseDatabases(t, "auditability_drill")
	ctx := t.Context()

	seedRegistry(t, dbs, c.Registry)
	seedEvents(t, dbs, c.Events)

	result, err := runs.Execute(ctx, dbs.engine, dbs.source, c.options(t, want.Clouds))
	if err != nil {
		t.Fatalf("runs.Execute: %v", err)
	}
	assertClean(t, result)

	api := newRegistryAPI(t, dbs)

	// The rows the invoice's adjustment lines were stored as: the discount off
	// the base cost, and the kickback the partner is paid on what it leaves.
	rows := readAdjustmentRecords(t, dbs, result.RunID)
	if len(rows) != 2 {
		t.Fatalf("adjustment records = %+v, want the discount and the kickback of the case", rows)
	}

	// The partner both relations reach, resolved once by the pair that keys the
	// registry.
	var partners httpapi.ProjectList
	api.get(t, "/api/v1/projects?cloud=partner&external_id=partner-corp", &partners)
	if len(partners.Items) != 1 {
		t.Fatalf("the registry serves %d projects for partner/partner-corp, want the one the case registers",
			len(partners.Items))
	}

	// The metadata the case seeded, re-encoded the way the served one is. The
	// served Relation.Metadata is a map[string]interface{}, so marshalling both
	// sides yields objects with their keys sorted, and every value of this
	// fixture's metadata is a string, so no number passes through a float.
	var seededMetadata any
	if err := json.Unmarshal(c.Registry.Relations[0].Metadata, &seededMetadata); err != nil {
		t.Fatalf("decoding the metadata of the relation of the case: %v", err)
	}
	wantMetadata, err := json.Marshal(seededMetadata)
	if err != nil {
		t.Fatalf("encoding the metadata of the relation of the case: %v", err)
	}

	for _, row := range rows {
		cloud, externalID, err := statements.ParseKey(row.projectID)
		if err != nil {
			t.Fatalf("the project of the %s record: %v", row.typ, err)
		}
		var projects httpapi.ProjectList
		api.get(t, "/api/v1/projects?cloud="+url.QueryEscape(cloud)+
			"&external_id="+url.QueryEscape(externalID), &projects)
		if len(projects.Items) != 1 {
			t.Fatalf("the registry serves %d projects for %s, want the one the record names",
				len(projects.Items), row.projectID)
		}
		project := projects.Items[0]

		for _, walk := range []struct{ target, when string }{
			{
				target: fmt.Sprintf("/api/v1/projects/%s/relations?direction=outgoing&at=%s",
					project.Id, url.QueryEscape("2026-03-01T00:00:00Z")),
				when: "at the period start",
			},
			{
				target: fmt.Sprintf("/api/v1/projects/%s/relations?direction=outgoing", project.Id),
				when:   "at now",
			},
		} {
			var relations httpapi.RelationList
			api.get(t, walk.target, &relations)
			at := slices.IndexFunc(relations.Items, func(r httpapi.Relation) bool {
				return r.Id.String() == row.relationID
			})
			if at < 0 {
				t.Fatalf("the registry serves no relation %s for %s %s",
					row.relationID, row.projectID, walk.when)
			}
			relation := relations.Items[at]

			if relation.RelationType != row.relationType {
				t.Errorf("the served relation %s is a %q, want the %q the record names",
					row.relationID, relation.RelationType, row.relationType)
			}
			if relation.TargetId != partners.Items[0].Id {
				t.Errorf("the served relation %s reaches the project %s, want the partner %s",
					row.relationID, relation.TargetId, partners.Items[0].Id)
			}
			served, err := json.Marshal(relation.Metadata)
			if err != nil {
				t.Fatalf("encoding the metadata of the served relation %s: %v", row.relationID, err)
			}
			if !bytes.Equal(served, wantMetadata) {
				t.Errorf("the metadata of the served relation %s = %s, want %s",
					row.relationID, served, wantMetadata)
			}

			// The element of that metadata the row was computed from, read by the
			// schema the rating engine reads it by.
			member, err := json.Marshal(relation.Metadata[adjustment.MetadataKey])
			if err != nil {
				t.Fatalf("encoding the adjustments of the served relation %s: %v", row.relationID, err)
			}
			parsed, err := adjustment.Parse(member)
			if err != nil {
				t.Fatalf("reading the adjustments of the served relation %s: %v", row.relationID, err)
			}
			if !slices.ContainsFunc(parsed, func(a adjustment.Adjustment) bool {
				return a.Type == row.typ && a.Scope == row.scope &&
					a.Rate.Equal(decimal.RequireFromString(row.rate))
			}) {
				t.Errorf("the served relation %s carries no %s adjustment of scope %s at the rate %s",
					row.relationID, row.typ, row.scope, row.rate)
			}
		}
	}
}

// goldenAdjustedCorrection runs the correction chain over an adjusted month. It
// is the phase's fourth exit criterion. The reseller case is billed, finalized,
// reached by a power cycle that arrives after the invoice, and corrected: the
// late events move the base, and the credit note has to carry the discount's and
// the kickback's share of that move beside the usage deltas, so that the credit
// note and the partner's settlement follow the corrected usage line for line. A
// second correction after the first is finalized finds nothing left to credit.
func goldenAdjustedCorrection(t *testing.T, f goldenFixture) {
	const (
		cloud = "os-golden-reseller"
		key   = cloud + "/customer-proj-1"
	)

	c := loadCase(t, "reseller")
	want := loadExpected(t, "reseller")
	dbs := f.caseDatabases(t, "adjusted_correction")
	ctx := t.Context()

	seeded := seedRegistry(t, dbs, c.Registry)
	seedEvents(t, dbs, c.Events)

	// The month as it was invoiced: the instance from 2026-03-07 at the base
	// cost 1200.00, the 15 % discount off it, and the partner's 10 % on the net
	// cost it leaves. The case writes those numbers down, so the run is held to
	// its expectations whole rather than to totals spelled out again here.
	regular, err := runs.Execute(ctx, dbs.engine, dbs.source, c.options(t, want.Clouds))
	if err != nil {
		t.Fatalf("runs.Execute: %v", err)
	}
	assertClean(t, regular)
	assertStats(t, regular.Stats, want.Stats)
	assertUsage(t, dbs, regular.RunID, want.Usage)

	run, err := export.Load(ctx, dbs.engine, regular.RunID)
	if err != nil {
		t.Fatalf("export.Load: %v", err)
	}
	assertRated(t, run.Rated, want.Rated)
	assertStatements(t, run.Statements, want.Statements, want.AbsentStatements, seeded)
	assertAdjustmentRecords(t, dbs, regular.RunID, want.Statements, seeded)
	assertKickbacks(t, run.Kickbacks, want.Statements, seeded)

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
		Cloud: cloud, Platform: "openstack", ResourceType: "instance", ResourceID: "i-reseller",
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
		{"adjustment_deltas", first.Stats.AdjustmentDeltas, 2},
		{"adjustment_records", first.Stats.AdjustmentRecords, 2},
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
	// The instance ran 600 hours from 2026-03-07. The power cycle puts the 120
	// hours from 2026-03-17 to 2026-03-22 into shutoff, which the model rates at
	// the modifier 0.5, so the correction rates 540 effective hours: vcpus
	// 50 x 540 x 0.02 = 540.00, ram_gb 100 x 540 x 0.005 = 270.00, disk_gb
	// 500 x 540 x 0.001 = 270.00. They come in the order the diff sorts its
	// keys. The egress both passes rated at zero is no difference, so the
	// correction writes no delta for it.
	wantDeltas := []struct{ dimension, old, new, delta string }{
		{"disk_gb", "300.00", "270.00", "-30.00"},
		{"ram_gb", "300.00", "270.00", "-30.00"},
		{"vcpus", "600.00", "540.00", "-60.00"},
	}
	if len(credit.Deltas) != len(wantDeltas) {
		t.Fatalf("deltas = %d, want %d", len(credit.Deltas), len(wantDeltas))
	}
	for i, w := range wantDeltas {
		got := credit.Deltas[i]
		for _, field := range []struct{ name, got, want string }{
			{"dimension", got.Dimension, w.dimension},
			{"cloud", got.Cloud, cloud},
			{"resource_id", got.ResourceID, "i-reseller"},
			{"project_id", got.ProjectID, "customer-proj-1"},
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
	// The note settles the net delta, which is what the customer is credited.
	if want := decimal.RequireFromString("-102.00"); !note.Total.Equal(want) {
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
	assertBillingPeriod(t, "the credit note of "+key, document.BillingPeriod,
		expectedBillingPeriod{From: "2026-03-01T00:00:00Z", To: "2026-04-01T00:00:00Z"})
	for _, field := range []struct{ name, got, want string }{
		{"project", document.ProjectID, "customer-proj-1"},
		{"platform", document.Platform, "openstack"},
	} {
		if field.got != field.want {
			t.Errorf("the %s of the credit note of %s = %q, want %q",
				field.name, key, field.got, field.want)
		}
	}
	if len(document.RelatedCosts) != 0 {
		t.Errorf("the credit note of %s carries the related costs %+v, want none: "+
			"the case registers the partner beside the customer and relates them, "+
			"which is not an attributing edge", key, document.RelatedCosts)
	}

	if len(document.LineItems) != 1 {
		t.Fatalf("the credit note of %s carries %d line items, want the one instance the power cycle reached",
			key, len(document.LineItems))
	}
	line := document.LineItems[0]
	for _, field := range []struct{ name, got, want string }{
		{"resource_type", line.ResourceType, "instance"},
		{"resource_id", line.ResourceID, "i-reseller"},
		{"platform", line.Platform, "openstack"},
	} {
		if field.got != field.want {
			t.Errorf("the line of the credit note of %s: %s = %q, want %q",
				key, field.name, field.got, field.want)
		}
	}
	// The line credits the usage, so its total is the base delta and not the
	// net one the note settles.
	if want := decimal.RequireFromString("-120.00"); !line.Total.Equal(want) {
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

	// The commercial part of the note, which a correction of an unadjusted month
	// leaves off. The base falls from 1200.00 to 1080.00, so the discount off it
	// goes from -180.00 to 0.15 x 1080.00 = -162.00, the net cost from 1020.00 to
	// 918.00, and the partner's commission from 102.00 to 0.10 x 918.00 = 91.80.
	for _, member := range []struct {
		name    string
		missing bool
	}{
		{"base_delta", document.BaseDelta == nil},
		{"net_delta", document.NetDelta == nil},
		{"kickback_delta", document.KickbackDelta == nil},
	} {
		if member.missing {
			t.Fatalf("the credit note of %s carries no %s, want the one the adjustments moved",
				key, member.name)
		}
	}
	for _, amount := range []struct {
		name string
		got  decimal.Decimal
		want string
	}{
		{"base delta", document.BaseDelta.Decimal, "-120.00"},
		{"net delta", document.NetDelta.Decimal, "-102.00"},
		{"kickback delta", document.KickbackDelta.Decimal, "-10.20"},
	} {
		if want := decimal.RequireFromString(amount.want); !amount.got.Equal(want) {
			t.Errorf("the %s of the credit note of %s = %s, want %s", amount.name, key, amount.got, want)
		}
	}

	if len(document.Adjustments) != 2 {
		t.Fatalf("the credit note of %s carries %d adjustments, want the discount and the kickback",
			key, len(document.Adjustments))
	}
	for i, w := range []struct {
		typ, rate, old, new, delta string
	}{
		{adjustment.TypeDiscount, "0.15", "-180.00", "-162.00", "18.00"},
		{adjustment.TypeKickback, "0.10", "102.00", "91.80", "-10.20"},
	} {
		got := document.Adjustments[i]
		for _, field := range []struct{ name, got, want string }{
			{"type", got.Type, w.typ},
			{"relation_type", got.RelationType, "managed_by"},
			{"relation_target", got.RelationTarget, "partner-corp"},
			{"relation_id", got.RelationID, seeded.relations[0].String()},
			{"scope", got.Scope, "all"},
		} {
			if field.got != field.want {
				t.Errorf("adjustment %d of the credit note of %s: %s = %q, want %q",
					i, key, field.name, field.got, field.want)
			}
		}
		for _, amount := range []struct {
			name string
			got  decimal.Decimal
			want string
		}{
			{"rate", got.Rate.Decimal, w.rate},
			{"old", got.Old.Decimal, w.old},
			{"new", got.New.Decimal, w.new},
			{"delta", got.Delta.Decimal, w.delta},
		} {
			if want := decimal.RequireFromString(amount.want); !amount.got.Equal(want) {
				t.Errorf("adjustment %d of the credit note of %s: %s = %s, want %s",
					i, key, amount.name, amount.got, want)
			}
		}
	}

	// What the partner is settled for the correction. A correction's kickbacks
	// are the differences to the run it corrects, so the base and the amount are
	// the new ones minus the old.
	if len(credit.Kickbacks) != 1 {
		t.Fatalf("kickbacks = %+v, want the one the correction moved", credit.Kickbacks)
	}
	paid := credit.Kickbacks[0]
	for _, field := range []struct{ name, got, want string }{
		{"beneficiary", paid.Beneficiary, "partner-corp"},
		{"currency", paid.Currency, currency},
		{"statement_key", paid.StatementKey, key},
		{"cloud", paid.Cloud, cloud},
		{"project_id", paid.ProjectID, "customer-proj-1"},
		{"scope", paid.Scope, "all"},
	} {
		if field.got != field.want {
			t.Errorf("the settled kickback: %s = %q, want %q", field.name, field.got, field.want)
		}
	}
	if paid.RelationID != seeded.relations[0] {
		t.Errorf("the settled kickback came off relation %s, want the one of the case %s",
			paid.RelationID, seeded.relations[0])
	}
	for _, amount := range []struct {
		name string
		got  decimal.Decimal
		want string
	}{
		{"rate", paid.Rate, "0.10"},
		{"base", paid.Base, "-102.00"},
		{"amount", paid.Amount, "-10.20"},
	} {
		if want := decimal.RequireFromString(amount.want); !amount.got.Equal(want) {
			t.Errorf("the settled kickback: %s = %s, want %s", amount.name, amount.got, want)
		}
	}

	// The rows behind that settlement. They carry what the correction applied
	// rather than the differences: the discount off the 1080.00 the month now
	// costs, and the commission on the 918.00 it leaves. type sorts discount
	// before kickback under one project and relation, which is the order the
	// query reads them in.
	records := readAdjustmentRecords(t, dbs, first.RunID)
	if len(records) != 2 {
		t.Fatalf("adjustment records = %+v, want the discount and the kickback of the correction", records)
	}
	for i, w := range []struct {
		typ, base, amount, beneficiary string
		hasBeneficiary                 bool
	}{
		{typ: adjustment.TypeDiscount, base: "1080.00", amount: "-162.00"},
		{
			typ: adjustment.TypeKickback, base: "918.00", amount: "91.80",
			beneficiary: "partner-corp", hasBeneficiary: true,
		},
	} {
		got := records[i]
		for _, field := range []struct{ name, got, want string }{
			{"type", got.typ, w.typ},
			{"project_id", got.projectID, key},
			{"relation_id", got.relationID, seeded.relations[0].String()},
			{"beneficiary", got.beneficiary, w.beneficiary},
		} {
			if field.got != field.want {
				t.Errorf("adjustment record %d: %s = %q, want %q", i, field.name, field.got, field.want)
			}
		}
		if got.hasBeneficiary != w.hasBeneficiary {
			t.Errorf("adjustment record %d carries a beneficiary = %t, want %t",
				i, got.hasBeneficiary, w.hasBeneficiary)
		}
		for _, amount := range []struct{ name, got, want string }{
			{"base", got.base, w.base},
			{"amount", got.amount, w.amount},
		} {
			stored, err := decimal.NewFromString(amount.got)
			if err != nil {
				t.Errorf("the stored %s of adjustment record %d: %v", amount.name, i, err)
				continue
			}
			if want := decimal.RequireFromString(amount.want); !stored.Equal(want) {
				t.Errorf("adjustment record %d: %s = %s, want %s", i, amount.name, stored, want)
			}
		}
	}

	if kind, err = runs.Finalize(ctx, dbs.engine, periodFrom, first.RunID); err != nil {
		t.Fatalf("runs.Finalize: %v", err)
	} else if kind != runs.KindCorrection {
		t.Errorf("runs.Finalize closed a %q, want a %q", kind, runs.KindCorrection)
	}

	// The finalized correction is the period's truth now, and the power cycle is
	// in it, so correcting again finds nothing to credit and nothing to settle.
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
	if second.Stats.AdjustmentDeltas != 0 {
		t.Errorf("Stats.AdjustmentDeltas = %d, want none: the adjustments come out as they were credited",
			second.Stats.AdjustmentDeltas)
	}
	if second.Stats.Statements != 0 {
		t.Errorf("Stats.Statements = %d, want none: there is nothing to credit", second.Stats.Statements)
	}
	// A correction applies the relation's adjustments to what it rated, whatever
	// the run before it applied, so it stores its own rows where nothing moved.
	if second.Stats.AdjustmentRecords != 2 {
		t.Errorf("Stats.AdjustmentRecords = %d, want the two the correction applied",
			second.Stats.AdjustmentRecords)
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
	if len(settled.Kickbacks) != 0 {
		t.Errorf("kickbacks = %+v, want none: the partner was settled by the first correction",
			settled.Kickbacks)
	}
	if got := runStatus(t, dbs, second.RunID); got != "completed" {
		t.Errorf("status = %q, want completed: a correction that found nothing is a correction", got)
	}
}

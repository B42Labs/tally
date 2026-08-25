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
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/engine/corrections"
	"github.com/b42labs/tally/internal/engine/export"
	"github.com/b42labs/tally/internal/engine/runs"
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
	} {
		t.Run(name, func(t *testing.T) {
			c := loadCase(t, name)
			want := loadExpected(t, name)
			dbs := f.caseDatabases(t, name)

			seedRegistry(t, dbs, c.Registry)
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
			assertStatements(t, run.Statements, want.Statements, want.AbsentStatements)
		})
	}

	t.Run("correction_credit", func(t *testing.T) { goldenCorrectionCredit(t, f) })
	t.Run("reproducibility", func(t *testing.T) { goldenReproducibility(t, f) })
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

	seedRegistry(t, dbs, c.Registry)
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
	assertStatements(t, a.Statements, want.Statements, want.AbsentStatements)
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

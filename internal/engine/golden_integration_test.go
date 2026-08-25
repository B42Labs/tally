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
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/engine/export"
	"github.com/b42labs/tally/internal/engine/runs"
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

// TestGolden runs the cases that meter one period once and read the result
// back. Each of them gets its own pair of databases inside the containers the
// fixture starts, so the cases share the containers and run in any order.
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
}

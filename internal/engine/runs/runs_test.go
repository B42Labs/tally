package runs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/attribution"
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
		SnapshotAt:   &snapshotAt,
		Candidates:   3,
		UsageRecords: 4,
		RatedRecords: 5,
		Statements:   2,
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
		`"candidates":3,"usage_records":4,"rated_records":5,"statements":2,` +
		`"warnings":[{"code":"period_not_ended",` +
		`"detail":"period_to 2026-04-01T00:00:00Z has not passed yet"}],` +
		`"metering_warnings":[{"cloud":"os-prod-eu1","resource_type":"instance","resource_id":"def-456",` +
		`"code":"candidate_without_history"}],` +
		`"counter_warnings":[{"cloud":"os-prod-eu1","resource_type":"instance","resource_id":"def-456",` +
		`"metric":"egress_gb","from_ts":"2026-03-10T00:00:00Z","to_ts":"2026-03-11T00:00:00Z",` +
		`"code":"counter_source_failed","detail":"the store did not answer"}],` +
		`"attribution_warnings":[{"code":"attribution_cycle",` +
		`"project_id":"6ba7b810-9dad-11d1-80b4-00c04fd430c8"}],` +
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

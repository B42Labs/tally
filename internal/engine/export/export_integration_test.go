// This file pins what Load reads out of a real database, which is what the
// cases beside it cannot see: the order the three listings hand their rows back
// in, the quantity a rated record is shown at, the values a run is refused
// over, and the round trip a document takes through JSONB. The column stores a
// parsed value and hands it back in its own key order and spacing, so a stored
// statement reaches an ERP as the document the statements package rendered only
// if it re-renders into those bytes after the round trip.
package export_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b42labs/tally/internal/engine/export"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/store/storetest"
)

// The two instants every seeded run carries. They are fixed rather than now(),
// because timestamptz keeps microseconds and an assertion over the column is
// only exact if what went in was.
var (
	runStartedAt   = time.Date(2026, 4, 4, 0, 0, 0, 0, time.UTC)
	runCompletedAt = time.Date(2026, 4, 4, 0, 1, 0, 0, time.UTC)
)

// pricingVersion is the version the seeded runs rated with, and the total the
// power-cycle statement carries.
const (
	pricingVersion  = "2026-03"
	powerCycleTotal = "128.45"
)

// runSeed is the run a case loads. An empty pricingVersion is the NULL a run of
// a period no model priced carries, a nil correctsRunID the NULL of a run that
// corrects nothing, a zero completedAt the NULL of a run no end was written
// for, and empty stats the object a run that counted nothing keeps.
type runSeed struct {
	kind           string
	status         string
	correctsRunID  uuid.UUID
	pricingVersion string
	stats          string
	completedAt    time.Time
}

// seedRun writes one run of the March 2026 period and returns its id. The
// insert is plain SQL: the lifecycle that opens and closes runs is not what an
// export is read through, and a status such as 'superseded' is one no
// transition this package can call reaches.
func seedRun(t *testing.T, db storetest.DB, seed runSeed) uuid.UUID {
	t.Helper()

	var corrects *uuid.UUID
	if seed.correctsRunID != uuid.Nil {
		corrects = &seed.correctsRunID
	}
	var version *string
	if seed.pricingVersion != "" {
		version = &seed.pricingVersion
	}
	var completedAt *time.Time
	if !seed.completedAt.IsZero() {
		completedAt = &seed.completedAt
	}
	stats := seed.stats
	if stats == "" {
		stats = "{}"
	}

	var id uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, kind, corrects_run_id, pricing_version,
		                   status, stats, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)
		 RETURNING id`,
		periodFrom, periodTo, seed.kind, corrects, version,
		seed.status, stats, runStartedAt, completedAt).Scan(&id); err != nil {
		t.Fatalf("seeding the %s %s run: %v", seed.status, seed.kind, err)
	}
	return id
}

// seedCompletedRun writes the regular run most cases start from: completed,
// priced, and ended.
func seedCompletedRun(t *testing.T, db storetest.DB) uuid.UUID {
	t.Helper()

	return seedRun(t, db, runSeed{
		kind:           runs.KindRegular,
		status:         "completed",
		pricingVersion: pricingVersion,
		completedAt:    runCompletedAt,
	})
}

// finalizeRun closes a seeded run. The records of a finalized run cannot be
// written, so a case that needs one seeds its rows under the completed run and
// moves the status afterwards, which is the transition trg_runs_immutable
// leaves open.
func finalizeRun(t *testing.T, db storetest.DB, runID uuid.UUID) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`UPDATE runs SET status = 'finalized' WHERE id = $1`, runID); err != nil {
		t.Fatalf("finalizing the run %s: %v", runID, err)
	}
}

// usageSeed is one usage draft under a run: the resource it belongs to, the
// interval it covers, and the usage object the quantity of a rated record is
// read back out of.
type usageSeed struct {
	cloud      string
	resourceID string
	projectID  string
	state      string
	from       time.Time
	to         time.Time
	usage      string
}

// seedUsage writes that draft and returns its id. The platform and the resource
// type are the ones the fixture resource carries, so the cloud and the resource
// id alone decide the order the listing walks two drafts in.
func seedUsage(t *testing.T, db storetest.DB, runID uuid.UUID, seed usageSeed) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO usage_records (run_id, cloud, platform, resource_type, resource_id, project_id,
		                            state, from_ts, to_ts, seconds, usage)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb)
		 RETURNING id`,
		runID, seed.cloud, instance.Platform, instance.ResourceType, seed.resourceID, seed.projectID,
		seed.state, seed.from, seed.to, int64(seed.to.Sub(seed.from)/time.Second),
		seed.usage).Scan(&id); err != nil {
		t.Fatalf("seeding the usage record of %s: %v", seed.resourceID, err)
	}
	return id
}

// seedRated writes one rated amount over a usage draft.
func seedRated(t *testing.T, db storetest.DB, runID, usageID uuid.UUID, dimension, amount string) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO rated_records (run_id, usage_record_id, dimension, amount, currency)
		 VALUES ($1, $2, $3, $4, $5)`,
		runID, usageID, dimension, numeric(t, amount), currency); err != nil {
		t.Fatalf("seeding the %s amount %s: %v", dimension, amount, err)
	}
}

// seedStatement writes one project statement of a run. The document goes in as
// jsonb, which is the column an export reads it back out of.
func seedStatement(t *testing.T, db storetest.DB, runID uuid.UUID, key string, document []byte, total string) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO project_statements (run_id, project_id, document, total, currency)
		 VALUES ($1, $2, $3::jsonb, $4, $5)`,
		runID, key, document, numeric(t, total), currency); err != nil {
		t.Fatalf("seeding the statement %s: %v", key, err)
	}
}

// deltaSeed is one delta row of a correction: the key it belongs to and the two
// amounts the correction moved between.
type deltaSeed struct {
	cloud      string
	resourceID string
	projectID  string
	dimension  string
	old        string
	current    string
	difference string
}

// seedDelta writes that row. The insert is plain SQL for the reason seedRun's
// is: what a case asserts is the read, so it is not also what sets it up.
func seedDelta(t *testing.T, db storetest.DB, runID, correctsRunID uuid.UUID, seed deltaSeed) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO correction_deltas (run_id, corrects_run_id, cloud, platform, resource_type,
		                                resource_id, project_id, dimension,
		                                old_amount, new_amount, delta, currency)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		runID, correctsRunID, seed.cloud, instance.Platform, instance.ResourceType,
		seed.resourceID, seed.projectID, seed.dimension,
		numeric(t, seed.old), numeric(t, seed.current), numeric(t, seed.difference),
		currency); err != nil {
		t.Fatalf("seeding the %s delta of %s: %v", seed.dimension, seed.resourceID, err)
	}
}

// numeric is an amount on its way into a NUMERIC(14,2) column. It reaches the
// column as text rather than through a float (roadmap/00-conventions.md
// section 6). "NaN" is one of the values it reads, which is how a case seeds
// the stored non-number an export is refused over.
func numeric(t *testing.T, amount string) pgtype.Numeric {
	t.Helper()

	var value pgtype.Numeric
	if err := value.Scan(amount); err != nil {
		t.Fatalf("reading the amount %q: %v", amount, err)
	}
	return value
}

// stamp renders a timestamp the way a case compares it. Two instants that are
// the same moment in different locations are not the same time.Time, and what
// pgx hands back carries the session's location rather than the one a case
// wrote.
func stamp(ts time.Time) string {
	return ts.UTC().Format(time.RFC3339)
}

// assertMessage holds an error to the values its message has to name, so a
// refusal an operator reads says which row it was about.
func assertMessage(t *testing.T, err error, want ...string) {
	t.Helper()

	for _, value := range want {
		if !strings.Contains(err.Error(), value) {
			t.Errorf("Load() error = %v, want it to name %q", err, value)
		}
	}
}

// load reads one run on the test's own context.
func load(t *testing.T, db storetest.DB, runID uuid.UUID) export.Run {
	t.Helper()

	run, err := export.Load(t.Context(), db.Store.Pool(), runID)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	return run
}

// TestLoadReadsARunWhole pins what an exporter receives for one completed run:
// every column of the row, the statements in key order, the rated records in
// the order the listing returns them, and the empty delta list a regular run
// has. The rows are written in none of those orders, so what comes back is
// decided by the queries rather than by the writes.
func TestLoadReadsARunWhole(t *testing.T) {
	db := storetest.NewDB(t)

	runID := seedRun(t, db, runSeed{
		kind:           runs.KindRegular,
		status:         "completed",
		pricingVersion: pricingVersion,
		stats:          `{"resources": 2}`,
		completedAt:    runCompletedAt,
	})

	// The resource ids sort against their clouds, so an ordering that read the
	// resource before the cloud would hand the two resources back the other way
	// round.
	first := seedUsage(t, db, runID, usageSeed{
		cloud: "os-b", resourceID: "a-1", projectID: "proj-b", state: "active",
		from: periodFrom, to: periodTo, usage: `{"vcpus": 4}`,
	})
	second := seedUsage(t, db, runID, usageSeed{
		cloud: "os-a", resourceID: "z-9", projectID: "proj-a", state: "shutoff",
		from: periodFrom, to: periodTo, usage: `{"vcpus": 4}`,
	})
	// Written in none of the orders the case asserts.
	seedRated(t, db, runID, first, "vcpus", "1.00")
	seedRated(t, db, runID, second, "vcpus", "10.00")
	seedRated(t, db, runID, first, "ram_gb", "2.00")
	seedRated(t, db, runID, second, "ram_gb", "20.00")

	// The second key sorts ahead of the first one, which is written first.
	seedStatement(t, db, runID, "os-b/proj-b", []byte(`{"project_id": "proj-b"}`), "3.00")
	seedStatement(t, db, runID, "os-a/proj-a", []byte(`{"project_id": "proj-a"}`), "30.00")

	run := load(t, db, runID)

	t.Run("reads every column of the run row", func(t *testing.T) {
		if run.ID != runID {
			t.Errorf("the run is %s, want %s", run.ID, runID)
		}
		if run.Kind != runs.KindRegular {
			t.Errorf("the run is a %q run, want %q", run.Kind, runs.KindRegular)
		}
		if run.Status != "completed" {
			t.Errorf("the run is %q, want completed", run.Status)
		}
		if stamp(run.PeriodFrom) != stamp(periodFrom) || stamp(run.PeriodTo) != stamp(periodTo) {
			t.Errorf("the run covers %s to %s, want %s to %s",
				stamp(run.PeriodFrom), stamp(run.PeriodTo), stamp(periodFrom), stamp(periodTo))
		}
		if run.PricingVersion != pricingVersion {
			t.Errorf("the run priced against %q, want %q", run.PricingVersion, pricingVersion)
		}
		if run.CorrectsRunID != uuid.Nil {
			t.Errorf("the run corrects %s, want the zero id of a regular run", run.CorrectsRunID)
		}
		if stamp(run.StartedAt) != stamp(runStartedAt) || stamp(run.CompletedAt) != stamp(runCompletedAt) {
			t.Errorf("the run ran from %s to %s, want %s to %s",
				stamp(run.StartedAt), stamp(run.CompletedAt), stamp(runStartedAt), stamp(runCompletedAt))
		}
		if got, want := string(run.Stats), `{"resources": 2}`; got != want {
			t.Errorf("the run carries the stats %s, want %s", got, want)
		}
	})

	t.Run("reads the statements in key order", func(t *testing.T) {
		type statement struct{ key, total, currency string }

		got := make([]statement, 0, len(run.Statements))
		for _, entry := range run.Statements {
			got = append(got, statement{entry.Key, entry.Total.StringFixed(2), entry.Currency})
		}
		want := []statement{
			{"os-a/proj-a", "30.00", currency},
			{"os-b/proj-b", "3.00", currency},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the statements are %v, want %v", got, want)
		}
	})

	t.Run("reads the rated records in cloud, resource and dimension order", func(t *testing.T) {
		type record struct {
			cloud, platform, resourceType, resourceID, projectID, state string
			from, to, dimension, quantity, amount, currency             string
		}

		got := make([]record, 0, len(run.Rated))
		for _, entry := range run.Rated {
			got = append(got, record{
				entry.Resource.Cloud, entry.Resource.Platform, entry.Resource.ResourceType,
				entry.Resource.ResourceID, entry.ProjectID, entry.State,
				stamp(entry.FromTS), stamp(entry.ToTS), entry.Dimension,
				entry.Quantity.StringFixed(4), entry.Amount.StringFixed(2), entry.Currency,
			})
		}
		want := []record{
			{
				"os-a", instance.Platform, instance.ResourceType, "z-9", "proj-a", "shutoff",
				stamp(periodFrom), stamp(periodTo), "ram_gb", "0.0000", "20.00", currency,
			},
			{
				"os-a", instance.Platform, instance.ResourceType, "z-9", "proj-a", "shutoff",
				stamp(periodFrom), stamp(periodTo), "vcpus", "4.0000", "10.00", currency,
			},
			{
				"os-b", instance.Platform, instance.ResourceType, "a-1", "proj-b", "active",
				stamp(periodFrom), stamp(periodTo), "ram_gb", "0.0000", "2.00", currency,
			},
			{
				"os-b", instance.Platform, instance.ResourceType, "a-1", "proj-b", "active",
				stamp(periodFrom), stamp(periodTo), "vcpus", "4.0000", "1.00", currency,
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the rated records are %v, want %v", got, want)
		}
	})

	t.Run("reads no deltas for a regular run", func(t *testing.T) {
		if len(run.Deltas) != 0 {
			t.Errorf("the run carries the deltas %v, want none: a regular run corrects nothing", run.Deltas)
		}
	})
}

// TestLoadReadsARunThatCarriesNothing pins the two runs an export is short of a
// value on. A run that billed nobody is exported as the empty month it is, and
// the columns a run leaves NULL reach an exporter as the absence they stand
// for rather than as a missing field.
func TestLoadReadsARunThatCarriesNothing(t *testing.T) {
	db := storetest.NewDB(t)

	t.Run("a run that billed nobody", func(t *testing.T) {
		run := load(t, db, seedCompletedRun(t, db))

		if len(run.Statements) != 0 {
			t.Errorf("the run carries the statements %v, want none", run.Statements)
		}
		if len(run.Rated) != 0 {
			t.Errorf("the run carries the rated records %v, want none", run.Rated)
		}
		if len(run.Deltas) != 0 {
			t.Errorf("the run carries the deltas %v, want none", run.Deltas)
		}
	})

	t.Run("a run whose nullable columns are null", func(t *testing.T) {
		runID := seedRun(t, db, runSeed{kind: runs.KindRegular, status: "completed"})

		run := load(t, db, runID)

		if run.PricingVersion != "" {
			t.Errorf("the run priced against %q, want the empty string of a run no version was written for",
				run.PricingVersion)
		}
		if !run.CompletedAt.IsZero() {
			t.Errorf("the run completed %s, want the zero time of a run no end was written for",
				stamp(run.CompletedAt))
		}
		if run.CorrectsRunID != uuid.Nil {
			t.Errorf("the run corrects %s, want the zero id of a run that corrects nothing", run.CorrectsRunID)
		}
		// An unrestricted run has to render as a list rather than as a null: a
		// null reads as an export that does not say which clouds it covered.
		if run.Clouds == nil || len(run.Clouds) != 0 {
			t.Errorf("the run names the clouds %#v, want an empty list that is not nil", run.Clouds)
		}
	})
}

// TestLoadDerivesTheQuantityFromTheUsage pins the number a rated record is
// shown at. It is not stored beside the amount: it is read back out of the
// usage object the amount was computed from, at the four places every quantity
// carries. A dimension the object does not hold, and a value nothing reads a
// number from, were both rated at zero by the run that stored them, so the
// export shows the zero they were billed at rather than refusing a record an
// invoice already carries.
func TestLoadDerivesTheQuantityFromTheUsage(t *testing.T) {
	db := storetest.NewDB(t)
	runID := seedCompletedRun(t, db)

	cases := []struct {
		name      string
		usage     string
		dimension string
		quantity  string
	}{
		{name: "a number the object holds", usage: `{"vcpus": 4}`, dimension: "vcpus", quantity: "4.0000"},
		{name: "a dimension the object does not hold", usage: `{"vcpus": 4}`, dimension: "ram_gb", quantity: "0.0000"},
		{name: "a number stored as a digit string", usage: `{"count": "2.5"}`, dimension: "count", quantity: "2.5000"},
		{name: "a value that is not a number", usage: `{"flag": true}`, dimension: "flag", quantity: "0.0000"},
	}

	// One draft per case, over consecutive days of the period, so the listing
	// hands the records back in the order the cases are written in.
	for i, c := range cases {
		from := periodFrom.AddDate(0, 0, i)
		usageID := seedUsage(t, db, runID, usageSeed{
			cloud: instance.Cloud, resourceID: instance.ResourceID, projectID: projectID,
			state: "active", from: from, to: from.AddDate(0, 0, 1), usage: c.usage,
		})
		seedRated(t, db, runID, usageID, c.dimension, "1.00")
	}

	run := load(t, db, runID)

	if len(run.Rated) != len(cases) {
		t.Fatalf("the run carries %d rated records, want %d", len(run.Rated), len(cases))
	}
	for i, c := range cases {
		record := run.Rated[i]
		if record.Dimension != c.dimension {
			t.Fatalf("record %d is the %s one, want %s", i, record.Dimension, c.dimension)
		}
		if got := record.Quantity.StringFixed(4); got != c.quantity {
			t.Errorf("%s: the quantity of %s is %s, want %s", c.name, c.usage, got, c.quantity)
		}
	}
}

// TestLoadReadsTheDeltasOfACorrection pins what a correction hands an exporter
// on top of its records: every delta it wrote, in the order corrections.Diff
// sorted them in. The rows land through a copy, which keeps no order of its
// own, so a credit note prints them in that order only if the read restores it.
func TestLoadReadsTheDeltasOfACorrection(t *testing.T) {
	db := storetest.NewDB(t)

	corrected := seedRun(t, db, runSeed{
		kind:           runs.KindRegular,
		status:         "finalized",
		pricingVersion: pricingVersion,
		completedAt:    runCompletedAt,
	})
	correction := seedRun(t, db, runSeed{
		kind:           runs.KindCorrection,
		status:         "completed",
		correctsRunID:  corrected,
		pricingVersion: pricingVersion,
		completedAt:    runCompletedAt,
	})

	// Written in none of the orders the case asserts, with the resource ids
	// sorting against their clouds and one resource split over two projects, so
	// that every key column of the ordering is read.
	seedDelta(t, db, correction, corrected, deltaSeed{
		cloud: "os-b", resourceID: "a-1", projectID: "proj-a", dimension: "vcpus",
		old: "8.00", current: "6.00", difference: "-2.00",
	})
	seedDelta(t, db, correction, corrected, deltaSeed{
		cloud: "os-a", resourceID: "z-9", projectID: "proj-a", dimension: "vcpus",
		old: "59.52", current: "49.92", difference: "-9.60",
	})
	seedDelta(t, db, correction, corrected, deltaSeed{
		cloud: "os-a", resourceID: "z-9", projectID: "proj-b", dimension: "vcpus",
		old: "4.00", current: "5.00", difference: "1.00",
	})
	seedDelta(t, db, correction, corrected, deltaSeed{
		cloud: "os-a", resourceID: "z-9", projectID: "proj-a", dimension: "ram_gb",
		old: "29.76", current: "24.96", difference: "-4.80",
	})

	run := load(t, db, correction)

	if run.Kind != runs.KindCorrection {
		t.Errorf("the run is a %q run, want %q", run.Kind, runs.KindCorrection)
	}
	if run.CorrectsRunID != corrected {
		t.Errorf("the run corrects %s, want %s", run.CorrectsRunID, corrected)
	}

	type row struct {
		cloud, platform, resourceType, resourceID, projectID, dimension string
		old, current, difference, currency                              string
	}

	got := make([]row, 0, len(run.Deltas))
	for _, entry := range run.Deltas {
		got = append(got, row{
			entry.Cloud, entry.Platform, entry.ResourceType, entry.ResourceID,
			entry.ProjectID, entry.Dimension,
			entry.Old.StringFixed(2), entry.New.StringFixed(2), entry.Delta.Delta.StringFixed(2),
			entry.Currency,
		})
	}
	want := []row{
		{
			"os-a", instance.Platform, instance.ResourceType, "z-9", "proj-a", "ram_gb",
			"29.76", "24.96", "-4.80", currency,
		},
		{
			"os-a", instance.Platform, instance.ResourceType, "z-9", "proj-a", "vcpus",
			"59.52", "49.92", "-9.60", currency,
		},
		{
			"os-a", instance.Platform, instance.ResourceType, "z-9", "proj-b", "vcpus",
			"4.00", "5.00", "1.00", currency,
		},
		{
			"os-b", instance.Platform, instance.ResourceType, "a-1", "proj-a", "vcpus",
			"8.00", "6.00", "-2.00", currency,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the deltas are %v, want %v", got, want)
	}
}

// TestLoadCarriesAStoredDocumentThroughJSONB pins the round trip a statement
// takes. It is stored in a jsonb column, which parses the document and hands it
// back in its own key order and spacing, and what an ERP receives has to be the
// document the statements package rendered all the same. The file the export
// writes is compared against the golden of the unit cases, which is rendered
// from the same fixture without a database in between.
func TestLoadCarriesAStoredDocumentThroughJSONB(t *testing.T) {
	db := storetest.NewDB(t)

	runID := seedCompletedRun(t, db)
	seedStatement(t, db, runID, statementKey, fixture(t, "statements", "power_cycle.json"), powerCycleTotal)
	// The month is closed after its statement is written: the records of a
	// finalized run are immutable, and a finalized run is what an export reads.
	finalizeRun(t, db, runID)

	run := load(t, db, runID)
	if run.Status != "finalized" {
		t.Fatalf("the run is %q, want finalized", run.Status)
	}

	dir := t.TempDir()
	if err := jsonFiles(dir).Export(t.Context(), run); err != nil {
		t.Fatalf("Export() error = %v, want nil", err)
	}

	// The document alone is compared. run.json names the run id the database
	// generated, which is not the one the golden was written from, and the unit
	// cases pin that file already.
	got := read(t, filepath.Join(dir, statementFile))
	want := read(t, filepath.Join("testdata", "golden", "regular", statementFile))
	if !bytes.Equal(got, want) {
		t.Errorf("%s =\n%s\nwant\n%s", statementFile, got, want)
	}
}

// TestLoadRefusals pins every value an export is refused over. A run that is
// not exportable is one whose records bill nothing, and a stored value that is
// not a number or not an object is one nothing can be rendered from: an export
// short of a value, or holding a zero where a number was meant, is one nobody
// can tell from a correct one, so each of them is reported by naming the row it
// was read from.
func TestLoadRefusals(t *testing.T) {
	db := storetest.NewDB(t)
	pool := db.Store.Pool()

	t.Run("a run id no row carries", func(t *testing.T) {
		unknown := uuid.New()

		_, err := export.Load(t.Context(), pool, unknown)
		if !errors.Is(err, runs.ErrRunNotFound) {
			t.Fatalf("Load() error = %v, want one matching ErrRunNotFound", err)
		}
		assertMessage(t, err, unknown.String())
	})

	t.Run("a run that is neither completed nor finalized", func(t *testing.T) {
		// The three statuses a period accumulates beside the run that bills it:
		// one still metering, one that broke, and one another run replaced.
		for _, status := range []string{"running", "failed", "superseded"} {
			t.Run(status, func(t *testing.T) {
				runID := seedRun(t, db, runSeed{kind: runs.KindRegular, status: status})

				_, err := export.Load(t.Context(), pool, runID)
				if !errors.Is(err, export.ErrRunNotExportable) {
					t.Fatalf("Load() error = %v, want one matching ErrRunNotExportable", err)
				}
				assertMessage(t, err, runID.String(), status)
			})
		}
	})

	t.Run("a rated amount that is not a number", func(t *testing.T) {
		runID := seedCompletedRun(t, db)
		usageID := seedUsage(t, db, runID, usageSeed{
			cloud: instance.Cloud, resourceID: instance.ResourceID, projectID: projectID,
			state: "active", from: periodFrom, to: periodTo, usage: `{"vcpus": 4}`,
		})
		seedRated(t, db, runID, usageID, "vcpus", "NaN")

		_, err := export.Load(t.Context(), pool, runID)
		if err == nil {
			t.Fatalf("Load() error = nil, want the stored non-number refused")
		}
		assertMessage(t, err, instance.ResourceID, projectID, "vcpus")
	})

	t.Run("a delta that is not a number", func(t *testing.T) {
		corrected := seedCompletedRun(t, db)
		correction := seedRun(t, db, runSeed{
			kind:           runs.KindCorrection,
			status:         "completed",
			correctsRunID:  corrected,
			pricingVersion: pricingVersion,
			completedAt:    runCompletedAt,
		})
		seedDelta(t, db, correction, corrected, deltaSeed{
			cloud: instance.Cloud, resourceID: instance.ResourceID, projectID: projectID,
			dimension: "vcpus", old: "59.52", current: "NaN", difference: "NaN",
		})

		_, err := export.Load(t.Context(), pool, correction)
		if err == nil {
			t.Fatalf("Load() error = nil, want the stored non-number refused")
		}
		assertMessage(t, err, instance.ResourceID, projectID, "vcpus")
	})

	t.Run("a statement total that is not a number", func(t *testing.T) {
		runID := seedCompletedRun(t, db)
		seedStatement(t, db, runID, statementKey, []byte(`{"project_id": "proj-456"}`), "NaN")

		_, err := export.Load(t.Context(), pool, runID)
		if err == nil {
			t.Fatalf("Load() error = nil, want the stored non-number refused")
		}
		assertMessage(t, err, statementKey)
	})

	t.Run("a usage object that is not an object", func(t *testing.T) {
		runID := seedCompletedRun(t, db)
		usageID := seedUsage(t, db, runID, usageSeed{
			cloud: instance.Cloud, resourceID: instance.ResourceID, projectID: projectID,
			state: "active", from: periodFrom, to: periodTo, usage: `[1,2]`,
		})
		seedRated(t, db, runID, usageID, "vcpus", "19.20")

		_, err := export.Load(t.Context(), pool, runID)
		if err == nil {
			t.Fatalf("Load() error = nil, want the stored array refused")
		}
		assertMessage(t, err, "decoding the usage of", instance.ResourceID,
			periodFrom.Format(time.RFC3339Nano), periodTo.Format(time.RFC3339Nano))
	})

	t.Run("a context that is already canceled", func(t *testing.T) {
		runID := seedCompletedRun(t, db)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := export.Load(ctx, pool, runID)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Load() error = %v, want one matching context.Canceled", err)
		}
	})

	t.Run("a database that cannot be reached", func(t *testing.T) {
		// The pool opens no connection until one is asked for, so the address
		// nothing listens on is reported by the read rather than here.
		unreachable, err := pgxpool.New(t.Context(), "postgres://nobody@127.0.0.1:1/none")
		if err != nil {
			t.Fatalf("opening the pool on an unreachable database: %v", err)
		}
		defer unreachable.Close()

		runID := uuid.New()
		if _, err = export.Load(t.Context(), unreachable, runID); err == nil {
			t.Fatalf("Load() error = nil, want the unreachable database reported")
		}
		assertMessage(t, err, "reading the run "+runID.String()+":")
	})
}

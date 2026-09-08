// This file pins what the console reads out of a real engine database, which
// is what a unit test cannot see: the order each listing hands its rows back
// in, the values the nullable columns come back as, the round trip a stored
// document takes through JSONB, and what a listing does with an amount that is
// not a number. The pages built on this package render exactly these rows, so
// the order and the conversions are the contract, not an implementation detail.
package store_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/b42labs/tally/internal/console/store"
	"github.com/b42labs/tally/internal/engine/pricing"
	enginesqlcgen "github.com/b42labs/tally/internal/engine/store/sqlcgen"
	"github.com/b42labs/tally/internal/engine/store/storetest"
)

// The two months the fixture holds: March 2026 is finalized, April 2026 is
// still open.
var (
	periodFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	periodTo   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	openFrom   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	openTo     = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
)

// The instants the two runs and the finalization carry. They are fixed rather
// than now(), because timestamptz keeps microseconds and an assertion over the
// column is only exact if what went in was.
var (
	regularStartedAt   = time.Date(2026, 4, 1, 0, 5, 0, 0, time.UTC)
	regularCompletedAt = time.Date(2026, 4, 1, 0, 6, 0, 0, time.UTC)
	correctionStarted  = time.Date(2026, 4, 8, 0, 0, 0, 0, time.UTC)
	finalizedAt        = time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)
)

// The two intervals the instance was metered over in the regular run.
var (
	segmentAFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	segmentATo   = time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)
	segmentBFrom = time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)
	segmentBTo   = time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)
)

// The resource every usage record, rated record and delta below belongs to, and
// the project keys the statements are stored under.
const (
	cloud        = "os-sim"
	platform     = "openstack"
	resourceType = "instance"
	resourceID   = "vm-1"

	projectKey   = "os-prod/proj-456"
	drKey        = "os-dr/proj-789"
	secondDRKey  = "os-dr/proj-999"
	currency     = "EUR"
	statedTotal  = "128.45"
	pricingModel = "2026-03"
)

// The catalog and the statement the fixture is seeded from. The statement is
// the export package's golden document, so what a console page renders is the
// document the engine actually stores.
const (
	catalogFile   = "../../../pricing/2026-03.yaml"
	statementFile = "../../../internal/engine/export/testdata/golden/regular/statement-os-prod%2Fproj-456.json"
)

// fixture is the seeded database, the store under test, and the log the store
// writes its skipped rows to.
type fixture struct {
	store *store.Store
	logs  *bytes.Buffer
	// regular is the run that billed March, correction the later run that
	// corrects it.
	regular    uuid.UUID
	correction uuid.UUID
	// statement is the document seeded under projectKey, before the round trip
	// through JSONB.
	statement []byte
}

// newFixture starts a database, seeds the March 2026 story into it, and opens
// the console store on the result. One container serves every subtest: the
// reads under test share a fixture rather than a database each.
func newFixture(t *testing.T) fixture {
	t.Helper()

	db := storetest.NewDB(t)
	logs := &bytes.Buffer{}
	s, err := store.New(t.Context(), db.URL, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatalf("opening the console store: %v", err)
	}
	t.Cleanup(s.Close)

	f := fixture{store: s, logs: logs, statement: readFile(t, statementFile)}

	f.regular = seedRun(t, db, runSeed{
		kind:           "regular",
		status:         "completed",
		pricingVersion: pricingModel,
		clouds:         []string{cloud},
		stats:          `{"resources": 1}`,
		startedAt:      regularStartedAt,
		completedAt:    regularCompletedAt,
	})
	// The correction carries no completion, which is the NULL a run that did not
	// get to the end of its pass leaves in completed_at.
	f.correction = seedRun(t, db, runSeed{
		kind:           "correction",
		status:         "completed",
		correctsRunID:  f.regular,
		pricingVersion: pricingModel,
		startedAt:      correctionStarted,
	})

	segmentA := seedUsage(t, db, f.regular, usageSeed{
		state: "active", from: segmentAFrom, to: segmentATo,
		usage: `{"vcpus": 4, "ram_gb": 8}`,
	})
	seedRated(t, db, f.regular, segmentA, "ram_gb", "12.00")
	seedRated(t, db, f.regular, segmentA, "vcpus", "48.00")

	segmentB := seedUsage(t, db, f.regular, usageSeed{
		state: "stopped", from: segmentBFrom, to: segmentBTo,
		usage: `{"vcpus": 4, "ram_gb": 8}`,
	})
	seedRated(t, db, f.regular, segmentB, "ram_gb", "2.40")
	seedRated(t, db, f.regular, segmentB, "vcpus", "9.60")
	// The amount a rating never produces and a page can show nothing for. It
	// sorts ahead of both dimensions of its segment, so a listing that let it
	// through would be caught by the order as well as by the count.
	seedRated(t, db, f.regular, segmentB, "disk_gb", "NaN")

	// The same resource under the correction run, which is what makes the
	// correction the newest run that metered it.
	corrected := seedUsage(t, db, f.correction, usageSeed{
		state: "active", from: segmentAFrom, to: segmentATo,
		usage: `{"vcpus": 4, "ram_gb": 8}`,
	})
	seedRated(t, db, f.correction, corrected, "vcpus", "50.00")

	seedStatement(t, db, f.regular, projectKey, f.statement, statedTotal)
	seedStatement(t, db, f.regular, drKey, []byte(`{"total": 22.32}`), "22.32")
	seedStatement(t, db, f.regular, secondDRKey, []byte(`{"total": 22.32}`), "22.32")
	// A total no page can render, under the run the good ones are not under.
	seedStatement(t, db, f.correction, projectKey, []byte(`{}`), "NaN")

	seedDelta(t, db, f.correction, f.regular, deltaSeed{
		dimension: "ram_gb", old: "12.00", current: "12.00", difference: "0.00",
	})
	seedDelta(t, db, f.correction, f.regular, deltaSeed{
		dimension: "vcpus", old: "48.00", current: "50.00", difference: "2.00",
	})

	seedPricing(t, db)

	// The run is closed after its records are written: trg_stmt_immutable and
	// its siblings refuse a record write under a finalized run.
	finalizeRun(t, db, f.regular)
	seedBillingPeriod(t, db, periodFrom, periodTo, "finalized", f.regular)
	seedBillingPeriod(t, db, openFrom, openTo, "open", uuid.Nil)

	return f
}

func TestStore(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()

	t.Run("ListPeriods returns the newest month first", func(t *testing.T) {
		periods, err := f.store.ListPeriods(ctx)
		if err != nil {
			t.Fatalf("listing the periods: %v", err)
		}
		if len(periods) != 2 {
			t.Fatalf("got %d periods, want 2", len(periods))
		}
		if stamp(periods[0].From) != stamp(openFrom) || periods[0].Status != "open" {
			t.Errorf("got the first period %s %s, want the open April",
				stamp(periods[0].From), periods[0].Status)
		}
		// The open month names no run and no instant, which reaches a page as the
		// two zero values rather than as a pointer it has to check.
		if periods[0].FinalizedRunID != uuid.Nil || !periods[0].FinalizedAt.IsZero() {
			t.Errorf("got the open period finalized by %s at %s, want neither",
				periods[0].FinalizedRunID, periods[0].FinalizedAt)
		}
		if stamp(periods[1].To) != stamp(periodTo) || periods[1].Status != "finalized" {
			t.Errorf("got the second period ending %s %s, want the finalized March",
				stamp(periods[1].To), periods[1].Status)
		}
		if periods[1].FinalizedRunID != f.regular || stamp(periods[1].FinalizedAt) != stamp(finalizedAt) {
			t.Errorf("got March finalized by %s at %s, want %s at %s",
				periods[1].FinalizedRunID, stamp(periods[1].FinalizedAt), f.regular, stamp(finalizedAt))
		}
	})

	t.Run("ListRuns is bounded and newest first", func(t *testing.T) {
		newest, err := f.store.ListRuns(ctx, 1)
		if err != nil {
			t.Fatalf("listing the newest run: %v", err)
		}
		if len(newest) != 1 || newest[0].ID != f.correction {
			t.Fatalf("got %d runs led by %v, want the correction alone", len(newest), idsOf(newest))
		}

		all, err := f.store.ListRuns(ctx, 20)
		if err != nil {
			t.Fatalf("listing the runs: %v", err)
		}
		if want := []uuid.UUID{f.correction, f.regular}; !reflect.DeepEqual(idsOf(all), want) {
			t.Errorf("got the runs %v, want %v", idsOf(all), want)
		}
	})

	t.Run("GetRun reads every column of a run", func(t *testing.T) {
		regular, err := f.store.GetRun(ctx, f.regular)
		if err != nil {
			t.Fatalf("reading the regular run: %v", err)
		}
		if regular.Kind != "regular" || regular.Status != "finalized" {
			t.Errorf("got the regular run %s %s, want a finalized regular one", regular.Kind, regular.Status)
		}
		if regular.PricingVersion != pricingModel {
			t.Errorf("got the pricing version %q, want %q", regular.PricingVersion, pricingModel)
		}
		if regular.CorrectsRunID != uuid.Nil {
			t.Errorf("got the regular run correcting %s, want nothing", regular.CorrectsRunID)
		}
		if stamp(regular.PeriodFrom) != stamp(periodFrom) || stamp(regular.PeriodTo) != stamp(periodTo) {
			t.Errorf("got the period [%s, %s), want [%s, %s)",
				stamp(regular.PeriodFrom), stamp(regular.PeriodTo), stamp(periodFrom), stamp(periodTo))
		}
		if stamp(regular.StartedAt) != stamp(regularStartedAt) ||
			stamp(regular.CompletedAt) != stamp(regularCompletedAt) {
			t.Errorf("got the run running [%s, %s], want [%s, %s]",
				stamp(regular.StartedAt), stamp(regular.CompletedAt),
				stamp(regularStartedAt), stamp(regularCompletedAt))
		}
		if want := []string{cloud}; !reflect.DeepEqual(regular.Clouds, want) {
			t.Errorf("got the clouds %v, want %v", regular.Clouds, want)
		}
		if !sameJSON(t, regular.Stats, []byte(`{"resources": 1}`)) {
			t.Errorf("got the stats %s, want the seeded object", regular.Stats)
		}

		correction, err := f.store.GetRun(ctx, f.correction)
		if err != nil {
			t.Fatalf("reading the correction run: %v", err)
		}
		if correction.CorrectsRunID != f.regular {
			t.Errorf("got the correction correcting %s, want %s", correction.CorrectsRunID, f.regular)
		}
		// The run has no completed_at, and the empty cloud list is the run that
		// was not restricted to any.
		if !correction.CompletedAt.IsZero() {
			t.Errorf("got the correction completed at %s, want the zero time", stamp(correction.CompletedAt))
		}
		if len(correction.Clouds) != 0 {
			t.Errorf("got the clouds %v, want none", correction.Clouds)
		}
	})

	t.Run("GetRun names itself on a run that does not exist", func(t *testing.T) {
		_, err := f.store.GetRun(ctx, uuid.New())
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("got %v, want pgx.ErrNoRows", err)
		}
		if !strings.HasPrefix(err.Error(), "GetRun:") {
			t.Errorf("got the message %q, want it to start with GetRun:", err)
		}
	})

	t.Run("ListStatements orders by total and breaks the tie on the key", func(t *testing.T) {
		rows, err := f.store.ListStatements(ctx, f.regular)
		if err != nil {
			t.Fatalf("listing the statements: %v", err)
		}
		want := []string{
			projectKey + " " + statedTotal,
			drKey + " 22.32",
			secondDRKey + " 22.32",
		}
		got := make([]string, 0, len(rows))
		for _, row := range rows {
			got = append(got, row.Key+" "+row.Total.StringFixed(2))
			if row.Currency != currency {
				t.Errorf("got the currency %q of %s, want %q", row.Currency, row.Key, currency)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got the statements %v, want %v", got, want)
		}
	})

	t.Run("ListStatements skips a total that is not a number", func(t *testing.T) {
		f.logs.Reset()
		rows, err := f.store.ListStatements(ctx, f.correction)
		if err != nil {
			t.Fatalf("listing the statements of the correction: %v", err)
		}
		if len(rows) != 0 {
			t.Fatalf("got %d statements, want none", len(rows))
		}
		assertSkipped(t, f.logs, "ListStatements", projectKey)
	})

	t.Run("GetStatement returns the stored document", func(t *testing.T) {
		got, err := f.store.GetStatement(ctx, f.regular, projectKey)
		if err != nil {
			t.Fatalf("reading the statement: %v", err)
		}
		// The column stores a parsed value and hands it back in its own key order
		// and spacing, so the document is compared as JSON rather than as bytes.
		if !sameJSON(t, got.Document, f.statement) {
			t.Errorf("got the document %s, want the seeded one", got.Document)
		}
		if got.Total.StringFixed(2) != statedTotal || got.Currency != currency {
			t.Errorf("got the total %s %s, want %s %s",
				got.Total.StringFixed(2), got.Currency, statedTotal, currency)
		}
	})

	t.Run("GetStatement refuses a pair that was never billed", func(t *testing.T) {
		if _, err := f.store.GetStatement(ctx, f.regular, "os-prod/nobody"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("got %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("GetStatement refuses a total that is not a number", func(t *testing.T) {
		_, err := f.store.GetStatement(ctx, f.correction, projectKey)
		if err == nil {
			t.Fatal("got a statement whose total is a NaN, want an error")
		}
		for _, want := range []string{"GetStatement:", "not a number", projectKey} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("got the message %q, want it to name %q", err, want)
			}
		}
	})

	t.Run("ListStatementsForProject joins the run of every month", func(t *testing.T) {
		f.logs.Reset()
		rows, err := f.store.ListStatementsForProject(ctx, projectKey)
		if err != nil {
			t.Fatalf("listing the history of %s: %v", projectKey, err)
		}
		// The correction's statement carries the NaN total, so the history holds
		// the one row a page can render.
		if len(rows) != 1 {
			t.Fatalf("got %d months, want 1", len(rows))
		}
		row := rows[0]
		if row.RunID != f.regular || row.Kind != "regular" || row.Status != "finalized" {
			t.Errorf("got the month billed by %s (%s, %s), want the finalized regular run %s",
				row.RunID, row.Kind, row.Status, f.regular)
		}
		if stamp(row.PeriodFrom) != stamp(periodFrom) {
			t.Errorf("got the period starting %s, want %s", stamp(row.PeriodFrom), stamp(periodFrom))
		}
		if row.Total.StringFixed(2) != statedTotal || row.Currency != currency {
			t.Errorf("got the total %s %s, want %s %s",
				row.Total.StringFixed(2), row.Currency, statedTotal, currency)
		}
		assertSkipped(t, f.logs, "ListStatementsForProject", projectKey)
	})

	t.Run("ListPricingModels returns the version in force last first", func(t *testing.T) {
		models, err := f.store.ListPricingModels(ctx)
		if err != nil {
			t.Fatalf("listing the pricing models: %v", err)
		}
		want := []string{"2026-04", pricingModel}
		got := make([]string, 0, len(models))
		for _, model := range models {
			got = append(got, model.Version)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got the versions %v, want %v", got, want)
		}
		if models[1].Currency != currency || models[1].ImportedAt.IsZero() {
			t.Errorf("got %s in %s imported at %s, want %s and an instant",
				models[1].Version, models[1].Currency, models[1].ImportedAt, currency)
		}
	})

	t.Run("GetPricingModel returns the catalog it was imported from", func(t *testing.T) {
		got, err := f.store.GetPricingModel(ctx, pricingModel)
		if err != nil {
			t.Fatalf("reading the pricing model: %v", err)
		}
		if got.Version != pricingModel || got.Currency != currency {
			t.Errorf("got %s in %s, want %s in %s", got.Version, got.Currency, pricingModel, currency)
		}
		if stamp(got.ValidFrom) != stamp(periodFrom) {
			t.Errorf("got it valid from %s, want %s", stamp(got.ValidFrom), stamp(periodFrom))
		}
		// The document is what a pricing page renders, so it has to come back as
		// the catalog the parser accepts rather than as opaque bytes.
		parsed, err := pricing.ParseDocument(got.Document)
		if err != nil {
			t.Fatalf("parsing the stored document: %v", err)
		}
		if parsed.Version != pricingModel {
			t.Errorf("got the parsed version %q, want %q", parsed.Version, pricingModel)
		}
	})

	t.Run("GetPricingModel refuses a version that was never imported", func(t *testing.T) {
		if _, err := f.store.GetPricingModel(ctx, "1999-01"); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("got %v, want pgx.ErrNoRows", err)
		}
	})

	t.Run("LatestRunWithResource finds the newest run that metered it", func(t *testing.T) {
		id, ok, err := f.store.LatestRunWithResource(ctx, cloud, resourceType, resourceID)
		if err != nil {
			t.Fatalf("resolving the run of %s: %v", resourceID, err)
		}
		if !ok || id != f.correction {
			t.Errorf("got %s (found %t), want the correction %s", id, ok, f.correction)
		}
	})

	t.Run("LatestRunWithResource reports a resource no run metered", func(t *testing.T) {
		id, ok, err := f.store.LatestRunWithResource(ctx, cloud, resourceType, "vm-404")
		if err != nil {
			t.Fatalf("resolving the run of an unmetered resource: %v", err)
		}
		if ok || id != uuid.Nil {
			t.Errorf("got %s (found %t), want the nil id and false", id, ok)
		}
	})

	t.Run("ListResourceSegments orders by interval and dimension", func(t *testing.T) {
		f.logs.Reset()
		segments, err := f.store.ListResourceSegments(ctx, f.regular, cloud, resourceType, resourceID)
		if err != nil {
			t.Fatalf("listing the segments: %v", err)
		}
		want := []string{
			"active " + stamp(segmentAFrom) + " ram_gb 12.00",
			"active " + stamp(segmentAFrom) + " vcpus 48.00",
			"stopped " + stamp(segmentBFrom) + " ram_gb 2.40",
			"stopped " + stamp(segmentBFrom) + " vcpus 9.60",
		}
		got := make([]string, 0, len(segments))
		for _, segment := range segments {
			got = append(got, segment.State+" "+stamp(segment.From)+" "+
				segment.Dimension+" "+segment.Amount.StringFixed(2))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got the segments %v, want %v", got, want)
		}
		if segments[0].Seconds != int64(segmentATo.Sub(segmentAFrom)/time.Second) {
			t.Errorf("got %d seconds, want %d",
				segments[0].Seconds, int64(segmentATo.Sub(segmentAFrom)/time.Second))
		}
		if stamp(segments[0].To) != stamp(segmentATo) {
			t.Errorf("got the segment ending %s, want %s", stamp(segments[0].To), stamp(segmentATo))
		}
		if !sameJSON(t, segments[0].Usage, []byte(`{"vcpus": 4, "ram_gb": 8}`)) {
			t.Errorf("got the usage %s, want the seeded object", segments[0].Usage)
		}
		assertSkipped(t, f.logs, "ListResourceSegments", "disk_gb")
	})

	t.Run("ListCorrectionDeltas returns what the correction moved", func(t *testing.T) {
		deltas, err := f.store.ListCorrectionDeltas(ctx, f.correction)
		if err != nil {
			t.Fatalf("listing the deltas: %v", err)
		}
		want := []string{"ram_gb 12.00 12.00 0.00", "vcpus 48.00 50.00 2.00"}
		got := make([]string, 0, len(deltas))
		for _, delta := range deltas {
			got = append(got, delta.Dimension+" "+delta.OldAmount.StringFixed(2)+" "+
				delta.NewAmount.StringFixed(2)+" "+delta.Delta.StringFixed(2))
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got the deltas %v, want %v", got, want)
		}
		if deltas[0].Cloud != cloud || deltas[0].Platform != platform ||
			deltas[0].ResourceType != resourceType || deltas[0].ResourceID != resourceID {
			t.Errorf("got the key %s/%s/%s/%s, want %s/%s/%s/%s",
				deltas[0].Cloud, deltas[0].Platform, deltas[0].ResourceType, deltas[0].ResourceID,
				cloud, platform, resourceType, resourceID)
		}
	})

	t.Run("ListCorrectionDeltas returns none for a regular run", func(t *testing.T) {
		deltas, err := f.store.ListCorrectionDeltas(ctx, f.regular)
		if err != nil {
			t.Fatalf("listing the deltas of the regular run: %v", err)
		}
		if len(deltas) != 0 {
			t.Fatalf("got %d deltas, want none", len(deltas))
		}
	})
}

// TestNew covers the two halves of opening the store, neither of which needs a
// database: a url that is not one is refused where it is parsed, and a database
// that does not answer is only reached by the first read.
func TestNew(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	t.Run("a url that cannot be parsed is refused", func(t *testing.T) {
		_, err := store.New(t.Context(), "not a database url", logger)
		if err == nil {
			t.Fatal("got a store on an unparsable url, want an error")
		}
		if !strings.Contains(err.Error(), "parsing the database url") {
			t.Errorf("got the message %q, want it to name the parse", err)
		}
	})

	t.Run("a database that does not answer fails the read", func(t *testing.T) {
		s, err := store.New(t.Context(), "postgres://tally:tally@127.0.0.1:1/x", logger)
		if err != nil {
			t.Fatalf("opening the store on an unreachable database: %v", err)
		}
		defer s.Close()

		_, err = s.ListPeriods(t.Context())
		if err == nil {
			t.Fatal("got the periods of an unreachable database, want an error")
		}
		if !strings.HasPrefix(err.Error(), "ListPeriods:") {
			t.Errorf("got the message %q, want it to start with ListPeriods:", err)
		}
	})
}

// runSeed is one run of the fixture. An empty pricingVersion is the NULL a run
// of a period no model priced carries, a nil correctsRunID the NULL of a run
// that corrects nothing, and a zero completedAt the NULL of a run no end was
// written for.
type runSeed struct {
	kind           string
	status         string
	correctsRunID  uuid.UUID
	pricingVersion string
	clouds         []string
	stats          string
	startedAt      time.Time
	completedAt    time.Time
}

// seedRun writes one run of the March 2026 period and returns its id. The
// insert is plain SQL: what a case asserts is the read, so the lifecycle that
// opens and closes runs is not also what sets it up.
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
	clouds := seed.clouds
	if clouds == nil {
		clouds = []string{}
	}

	var id uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO runs (period_from, period_to, kind, corrects_run_id, pricing_version,
		                   status, clouds, stats, started_at, completed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::text[], $8::jsonb, $9, $10)
		 RETURNING id`,
		periodFrom, periodTo, seed.kind, corrects, version,
		seed.status, clouds, stats, seed.startedAt, completedAt).Scan(&id); err != nil {
		t.Fatalf("seeding the %s %s run: %v", seed.status, seed.kind, err)
	}
	return id
}

// finalizeRun closes a seeded run. The records of a finalized run cannot be
// written, so the fixture seeds its rows under the completed run and moves the
// status afterwards, which is the transition trg_runs_immutable leaves open.
func finalizeRun(t *testing.T, db storetest.DB, runID uuid.UUID) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`UPDATE runs SET status = 'finalized' WHERE id = $1`, runID); err != nil {
		t.Fatalf("finalizing the run %s: %v", runID, err)
	}
}

// usageSeed is one metered interval of the fixture's instance.
type usageSeed struct {
	state string
	from  time.Time
	to    time.Time
	usage string
}

// seedUsage writes that interval and returns its id, which a rated record is
// written against.
func seedUsage(t *testing.T, db storetest.DB, runID uuid.UUID, seed usageSeed) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Store.Pool().QueryRow(t.Context(),
		`INSERT INTO usage_records (run_id, cloud, platform, resource_type, resource_id, project_id,
		                            state, from_ts, to_ts, seconds, usage)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb)
		 RETURNING id`,
		runID, cloud, platform, resourceType, resourceID, projectKey,
		seed.state, seed.from, seed.to, int64(seed.to.Sub(seed.from)/time.Second),
		seed.usage).Scan(&id); err != nil {
		t.Fatalf("seeding the %s usage record of %s: %v", seed.state, resourceID, err)
	}
	return id
}

// seedRated writes one rated amount over a usage record.
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
// jsonb, which is the column a console page reads it back out of.
func seedStatement(t *testing.T, db storetest.DB, runID uuid.UUID, key string, document []byte, total string) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO project_statements (run_id, project_id, document, total, currency)
		 VALUES ($1, $2, $3::jsonb, $4, $5)`,
		runID, key, document, numeric(t, total), currency); err != nil {
		t.Fatalf("seeding the statement %s: %v", key, err)
	}
}

// deltaSeed is one row a correction moved: the dimension it belongs to and the
// two amounts it moved between.
type deltaSeed struct {
	dimension  string
	old        string
	current    string
	difference string
}

// seedDelta writes that row under the correction run.
func seedDelta(t *testing.T, db storetest.DB, runID, correctsRunID uuid.UUID, seed deltaSeed) {
	t.Helper()

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO correction_deltas (run_id, corrects_run_id, cloud, platform, resource_type,
		                                resource_id, project_id, dimension,
		                                old_amount, new_amount, delta, currency)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		runID, correctsRunID, cloud, platform, resourceType, resourceID, projectKey, seed.dimension,
		numeric(t, seed.old), numeric(t, seed.current), numeric(t, seed.difference),
		currency); err != nil {
		t.Fatalf("seeding the %s delta of %s: %v", seed.dimension, resourceID, err)
	}
}

// seedBillingPeriod writes one month. A finalized period names the run that
// closed it and when that happened, and a period of every other status names
// neither, which is the pairing the CHECK of migration 0001 admits.
func seedBillingPeriod(t *testing.T, db storetest.DB, from, to time.Time, status string, finalizedRunID uuid.UUID) {
	t.Helper()

	var closedBy *uuid.UUID
	var closedAt *time.Time
	if status == "finalized" {
		closedBy, closedAt = &finalizedRunID, &finalizedAt
	}

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO billing_periods (period_from, period_to, status, finalized_run_id, finalized_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		from, to, status, closedBy, closedAt); err != nil {
		t.Fatalf("seeding the %s billing period: %v", status, err)
	}
}

// seedPricing imports the shipped March catalog through the engine's own
// importer, so the document a console page reads is the one an import writes,
// and adds a second version beside it. The second one is a plain row: what the
// listing is asserted on is the order of two versions, not a second catalog.
func seedPricing(t *testing.T, db storetest.DB) {
	t.Helper()

	model, doc, err := pricing.Parse(readFile(t, catalogFile))
	if err != nil {
		t.Fatalf("parsing the shipped pricing model: %v", err)
	}
	if _, err := pricing.Import(t.Context(), enginesqlcgen.New(db.Store.Pool()), model, doc); err != nil {
		t.Fatalf("importing the shipped pricing model: %v", err)
	}

	if _, err := db.Store.Pool().Exec(t.Context(),
		`INSERT INTO pricing_models (version, valid_from, currency, document)
		 VALUES ('2026-04', $1, $2, '{}'::jsonb)`,
		openFrom, currency); err != nil {
		t.Fatalf("seeding the second pricing version: %v", err)
	}
}

// numeric maps a decimal string to the parameter an amount column takes. It
// carries "NaN" as well, which is the value the listings are asserted to skip.
func numeric(t *testing.T, amount string) pgtype.Numeric {
	t.Helper()

	var value pgtype.Numeric
	if err := value.Scan(amount); err != nil {
		t.Fatalf("reading the amount %q: %v", amount, err)
	}
	return value
}

// readFile reads a fixture beside the module, failing the test rather than the
// case that uses it.
func readFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

// stamp renders a timestamp the way a case compares it. Two instants that are
// the same moment in different locations are not the same time.Time, and what
// pgx hands back carries the session's location rather than the one a case
// wrote.
func stamp(ts time.Time) string {
	return ts.UTC().Format(time.RFC3339)
}

// idsOf is the run listing reduced to what its order is asserted on.
func idsOf(runs []store.Run) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(runs))
	for _, run := range runs {
		ids = append(ids, run.ID)
	}
	return ids
}

// sameJSON reports whether two documents carry the same value. JSONB stores a
// parsed value and hands it back in its own key order and spacing, so bytes are
// not what a stored document is compared by.
func sameJSON(t *testing.T, got, want []byte) bool {
	t.Helper()

	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decoding the stored document: %v", err)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("decoding the seeded document: %v", err)
	}
	return reflect.DeepEqual(gotValue, wantValue)
}

// assertSkipped holds the log to the row a listing dropped: the message, the
// query that dropped it, and the column an operator finds it by.
func assertSkipped(t *testing.T, logs *bytes.Buffer, query, key string) {
	t.Helper()

	line := logs.String()
	for _, want := range []string{"skipping a row whose amount is not a number", "query=" + query, key} {
		if !strings.Contains(line, want) {
			t.Errorf("got the log %q, want it to carry %q", line, want)
		}
	}
}

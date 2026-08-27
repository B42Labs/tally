package export

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/runs"
)

// The two files a CSV export writes off the rating. rated.csv is every run's,
// deltas.csv only a correction's. kickbacks.csv, every run's as well, is named
// in kickbacks.go beside the renderer that fills it.
const (
	ratedFileName  = "rated.csv"
	deltasFileName = "deltas.csv"
)

// The header rows, which are the column orders the roadmap fixes for WP 3.10.
// Every row carries the run, its kind and its period, so a row says which run
// and which month it belongs to on its own: this format has no run.json beside
// the data the way the JSON one does.
var (
	ratedHeader = []string{
		"run_id", "kind", "corrects_run_id", "period_from", "period_to",
		"cloud", "platform", "resource_type", "resource_id", "project_id", "state",
		"from_ts", "to_ts", "dimension", "quantity", "amount", "currency",
	}
	deltasHeader = []string{
		"run_id", "corrects_run_id", "period_from", "period_to",
		"cloud", "platform", "resource_type", "resource_id", "project_id", "dimension",
		"old_amount", "new_amount", "delta", "currency",
	}
)

// CSVFiles writes a run as CSV files into a directory: rated.csv, one row per
// rated record, kickbacks.csv, one row per kickback the run settles for a
// partner, and, for a correction run, deltas.csv, one row per delta. It is the
// BillingExporter an ERP that imports tables rather than documents is fed from.
type CSVFiles struct {
	// Dir is where the files are written. It is created, with every parent it
	// needs, when the export has rendered everything.
	Dir string
}

// Export writes the run's tables into Dir. Every file is rendered before the
// directory is touched, the way the JSON writer renders before it writes, and a
// write that fails takes the table before it with it: a correction that left
// rated.csv in place without deltas.csv beside it is one an ERP imports the
// debits of while the credits it owes back are nowhere.
//
// A run with no rated records writes rated.csv holding its header alone, a
// correction with no deltas writes deltas.csv holding its header alone, and a
// run that owes no partner writes kickbacks.csv holding its header alone: an
// empty table says the run produced no rows, and a missing file says nothing at
// all.
//
// The context is not read, for the reason JSONFiles.Export gives.
func (c CSVFiles) Export(_ context.Context, run Run) error {
	rated, err := ratedTable(run)
	if err != nil {
		return err
	}
	var deltas []byte
	correction := run.Kind == runs.KindCorrection
	if correction {
		if deltas, err = deltasTable(run); err != nil {
			return err
		}
	}

	kickbacks, err := KickbacksCSV(run)
	if err != nil {
		return err
	}

	if err := prepareDir(c.Dir); err != nil {
		return err
	}
	files := []artifact{{name: ratedFileName, body: rated}}
	if correction {
		files = append(files, artifact{name: deltasFileName, body: deltas})
	}
	files = append(files, artifact{name: kickbacksCSVFileName, body: kickbacks})
	return writeFiles(c.Dir, files)
}

// ratedTable renders rated.csv: the header and one row per rated record, in the
// order the records were loaded in.
func ratedTable(run Run) ([]byte, error) {
	corrects := correctsOf(run)
	from, to := instant(run.PeriodFrom), instant(run.PeriodTo)

	rows := [][]string{ratedHeader}
	for _, record := range run.Rated {
		rows = append(rows, []string{
			run.ID.String(), run.Kind, corrects, from, to,
			cell(record.Resource.Cloud), cell(record.Resource.Platform),
			cell(record.Resource.ResourceType), cell(record.Resource.ResourceID),
			cell(record.ProjectID), cell(record.State),
			instant(record.FromTS), instant(record.ToTS), cell(record.Dimension),
			record.Quantity.StringFixed(money.QuantityPlaces),
			record.Amount.StringFixed(money.AmountPlaces),
			record.Currency,
		})
	}
	return table(run, ratedFileName, rows)
}

// deltasTable renders deltas.csv: the header and one row per delta, in the
// order corrections.Diff sorted them. The delta of a credit is negative, as it
// is stored, so a sum over the column is what the correction owes back.
func deltasTable(run Run) ([]byte, error) {
	corrects := correctsOf(run)
	from, to := instant(run.PeriodFrom), instant(run.PeriodTo)

	rows := [][]string{deltasHeader}
	for _, delta := range run.Deltas {
		rows = append(rows, []string{
			run.ID.String(), corrects, from, to,
			cell(delta.Cloud), cell(delta.Platform), cell(delta.ResourceType), cell(delta.ResourceID),
			cell(delta.ProjectID), cell(delta.Dimension),
			delta.Old.StringFixed(money.AmountPlaces),
			delta.New.StringFixed(money.AmountPlaces),
			// The embedded corrections.Delta carries the difference under the same
			// name as the type it is embedded from, which is why it is two hops down.
			delta.Delta.Delta.StringFixed(money.AmountPlaces),
			delta.Currency,
		})
	}
	return table(run, deltasFileName, rows)
}

// table renders one file: the header row and the rows under it, through
// encoding/csv, so a field holding a comma, a quote or a newline is quoted the
// way RFC 4180 asks for. The default comma and the LF line ending are kept, so
// one run's table is one sequence of bytes wherever the export runs.
func table(run Run, name string, rows [][]string) ([]byte, error) {
	var body bytes.Buffer
	writer := csv.NewWriter(&body)

	if err := writer.WriteAll(rows); err != nil {
		return nil, fmt.Errorf("rendering %s of run %s: %w", name, run.ID, err)
	}
	return body.Bytes(), nil
}

// cell renders one free-text column. A leading =, +, -, @, tab or carriage
// return makes Excel and LibreOffice evaluate the cell as a formula, and every
// identifier the tables above render is text an event carried in: rated.csv is
// written to be imported into an ERP and opened in a spreadsheet, where such a
// value reaches HYPERLINK, WEBSERVICE or DDE on the finance workstation.
// Quoting does not help, because both applications strip the quotes and
// evaluate what is inside. The apostrophe is the prefix both of them read as
// "this is text". The numeric columns do not go through here: the leading minus
// of a credit is part of the number.
func cell(field string) string {
	if field == "" {
		return field
	}
	switch field[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + field
	}
	return field
}

// correctsOf is the corrected run's id as a column carries it: the id for a
// correction run, and the empty field for a regular one, which corrects
// nothing.
func correctsOf(run Run) string {
	if run.CorrectsRunID == uuid.Nil {
		return ""
	}
	return run.CorrectsRunID.String()
}

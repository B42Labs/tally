package simulator

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/pricing"
)

// The comparison answers one question about a drill: did the engine bill the
// month the generator built? The oracle states the intervals the month was
// meant to be billed over, rated.csv states the intervals the engine billed,
// and every resource of a priced type is held against the other side interval
// by interval.
//
// What is compared is what the generator decided: the bounds of every
// interval, the state and the project it was booked under, and the quantity of
// every time gauge dimension the model prices the resource type by. What is
// not compared is what the pricing model decides. An amount is what the model
// charges for a quantity, so a comparison that recomputed one would hold the
// model against itself. A counter dimension is left out for a reason of its
// own: no transition of a simulated month meters egress, so the quantity an
// export carries for one is read from nothing the oracle states.
//
// A resource type the model does not price reaches no rated record at all,
// because rating.Rate skips it, so its resources are counted per type instead
// of being reported as missing. An export of another cloud, another month or
// another pricing model is refused rather than compared: each of the three
// would otherwise report the engine for a month it never billed.

// platformOpenStack is the platform a simulated month is booked under. A rated
// record of another platform belongs to another collector, and one of another
// cloud to another deployment, so neither is held against this oracle.
const platformOpenStack = "openstack"

// The two metrics the engine derives rather than reads off a size: the count
// of one that prices a resource by its existence, and the minutes an interval
// lasts. A size states neither, which is why a dimension named after them is
// expected from the interval itself.
const (
	metricCount   = "count"
	metricMinutes = "minutes"
)

// ratedFile is the table of a CSV export the comparison reads. The other files
// beside it hold the kickbacks and the deltas of a run, which say nothing
// about how the month was metered.
const ratedFile = "rated.csv"

// CompareOptions names the three files one comparison reads.
type CompareOptions struct {
	// Oracle is the path of the oracle.json a run wrote.
	Oracle string
	// Export is the directory tally-engine export --format csv --out wrote.
	Export string
	// Pricing is the path of the pricing model file the run rated with. The
	// model is read from the file rather than from the export, because a CSV
	// export carries no model: it says which quantity was billed and at which
	// amount, and not which dimensions the resource types were held against.
	Pricing string
}

// Validate reports the first member a comparison cannot run without. The three
// are checked in the order a caller passes them, so an invocation missing all
// of them is told about its first flag rather than its last.
func (o CompareOptions) Validate() error {
	switch {
	case o.Oracle == "":
		return errors.New("--oracle: must be set")
	case o.Export == "":
		return errors.New("--export: must be set")
	case o.Pricing == "":
		return errors.New("--pricing: must be set")
	}
	return nil
}

// Difference is one resource the export and the oracle disagree about: the
// first difference found on it, in the order the intervals run in, and how
// many further ones it carries. Only the first is spelled out, because a
// resource billed under the wrong project differs in every interval it has,
// and a report that printed all of them would bury the resource beside it.
type Difference struct {
	ResourceType string
	ResourceID   string
	Detail       string
	More         int
}

// UnpricedType is a resource type the pricing model does not price and the
// number of resources of it the oracle states. They are counted rather than
// compared: the rating pass skips such a resource, so the export holds no
// record of it, and a comparison that reported each one missing would report
// the engine for doing what the model asks.
type UnpricedType struct {
	ResourceType string
	Resources    int
}

// Report is what one comparison found.
type Report struct {
	// Compared counts every resource examined: the oracle's resources of a
	// priced type, and the resources the export books that the oracle does not
	// hold.
	Compared int
	// Unpriced counts the oracle's resources per type the model does not price,
	// sorted by type.
	Unpriced []UnpricedType
	// Skipped counts the rated records of another cloud or another platform,
	// which an export of a deployment that bills more than the simulated cloud
	// carries beside the ones this oracle is about.
	Skipped int
	// Differences holds one entry per differing resource, sorted by resource
	// type and then by id.
	Differences []Difference
	// PricingVersion is the version of the model the comparison read, so that a
	// report says which prices the unpriced types were unpriced by.
	PricingVersion string
}

// Lines renders the report as the lines a command prints: the differences
// first, then the types nothing was compared for, then the records of other
// clouds, and last the verdict. Both slices are printed in the order the
// comparison sorted them into.
func (r Report) Lines() []string {
	lines := make([]string, 0, len(r.Differences)+len(r.Unpriced)+2)
	for _, difference := range r.Differences {
		line := fmt.Sprintf("%s %s: %s", difference.ResourceType, difference.ResourceID, difference.Detail)
		if difference.More > 0 {
			line += fmt.Sprintf(" (and %d more)", difference.More)
		}
		lines = append(lines, line)
	}
	for _, entry := range r.Unpriced {
		lines = append(lines, fmt.Sprintf(
			"%s: %d resources are not priced by pricing model %s and were not compared",
			entry.ResourceType, entry.Resources, r.PricingVersion))
	}
	if r.Skipped > 0 {
		lines = append(lines, fmt.Sprintf("skipped %d rated records of other clouds or platforms", r.Skipped))
	}
	if len(r.Differences) == 0 {
		return append(lines, fmt.Sprintf("the export matches the oracle over %d resources", r.Compared))
	}
	return append(lines, fmt.Sprintf("%d of %d resources differ from the oracle",
		len(r.Differences), r.Compared))
}

// Compare holds the CSV export in a directory against the oracle of the month
// it was rated from, priced by the model the run rated with.
func Compare(opts CompareOptions) (Report, error) {
	if err := opts.Validate(); err != nil {
		return Report{}, err
	}
	oracle, err := ReadOracle(opts.Oracle)
	if err != nil {
		return Report{}, err
	}

	data, err := os.ReadFile(opts.Pricing)
	if err != nil {
		return Report{}, fmt.Errorf("reading the pricing model %s: %w", opts.Pricing, err)
	}
	model, _, err := pricing.Parse(data)
	if err != nil {
		return Report{}, fmt.Errorf("reading the pricing model %s: %w", opts.Pricing, err)
	}

	rows, err := readRated(filepath.Join(opts.Export, ratedFile))
	if err != nil {
		return Report{}, err
	}
	return compare(oracle, rows, model)
}

// ratedRow is one row of rated.csv, holding the columns a comparison reads.
// The run, its kind, the run it corrects, the amount and the currency are not
// read: they say what the export is, and not how the month was metered.
type ratedRow struct {
	Cloud        string
	Platform     string
	ResourceType string
	ResourceID   string
	ProjectID    string
	State        string
	Dimension    string
	From         time.Time
	To           time.Time
	PeriodFrom   time.Time
	PeriodTo     time.Time
	Quantity     decimal.Decimal
}

// ratedColumns are the columns readRated needs, in the order the header is
// checked for them. The columns are located by name rather than by position,
// so a run that appends a column of its own is still read.
var ratedColumns = []string{
	"cloud", "platform", "resource_type", "resource_id", "project_id", "state",
	"from_ts", "to_ts", "dimension", "quantity", "period_from", "period_to",
}

// readRated reads rated.csv. Every error names the file and, past the header,
// the line the reader stopped on: an export is a table of thousands of rows,
// and a message that named the value alone would leave whoever reads it
// searching for the row it came from.
func readRated(path string) ([]ratedRow, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	reader := csv.NewReader(file)
	header, err := reader.Read()
	switch {
	case errors.Is(err, io.EOF):
		return nil, fmt.Errorf("%s holds no header", path)
	case err != nil:
		return nil, fmt.Errorf("%s: line 1: %w", path, err)
	}

	positions := make(map[string]int, len(header))
	for i, name := range header {
		positions[name] = i
	}
	for _, name := range ratedColumns {
		if _, ok := positions[name]; !ok {
			return nil, fmt.Errorf("%s: the header lacks the column %q", path, name)
		}
	}

	// The header is line 1, so the first record is line 2. Every record holds as
	// many fields as the header, which encoding/csv enforces on its own, so a
	// column's position is a field of every record read past here.
	rows := make([]ratedRow, 0)
	for line := 2; ; line++ {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return rows, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, line, err)
		}
		row, err := rowOf(record, positions)
		if err != nil {
			return nil, fmt.Errorf("%s: line %d: %w", path, line, err)
		}
		rows = append(rows, row)
	}
}

// rowOf reads one record into a row. The free-text cells are taken as they
// stand: the export prefixes a cell starting with =, +, -, @, a tab or a
// carriage return with an apostrophe so that a spreadsheet does not evaluate
// it, and no identifier, state or dimension of a simulated month starts with
// one of those, so nothing here has an apostrophe to strip.
func rowOf(record []string, at map[string]int) (ratedRow, error) {
	row := ratedRow{
		Cloud:        record[at["cloud"]],
		Platform:     record[at["platform"]],
		ResourceType: record[at["resource_type"]],
		ResourceID:   record[at["resource_id"]],
		ProjectID:    record[at["project_id"]],
		State:        record[at["state"]],
		Dimension:    record[at["dimension"]],
	}

	for _, column := range []struct {
		name string
		into *time.Time
	}{
		{"from_ts", &row.From},
		{"to_ts", &row.To},
		{"period_from", &row.PeriodFrom},
		{"period_to", &row.PeriodTo},
	} {
		value := record[at[column.name]]
		instant, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return ratedRow{}, fmt.Errorf("%s %q: %w", column.name, value, err)
		}
		*column.into = instant
	}

	value := record[at["quantity"]]
	quantity, err := decimal.NewFromString(value)
	if err != nil {
		return ratedRow{}, fmt.Errorf("quantity %q: %w", value, err)
	}
	row.Quantity = quantity
	return row, nil
}

// pair is one interval of one resource as the export books it: the rows of
// every dimension carry the same bounds, state and project, and differ in the
// dimension and the quantity alone, so they are read back into one interval
// holding a quantity per dimension.
type pair struct {
	From       time.Time
	To         time.Time
	State      string
	ProjectID  string
	Quantities map[string]decimal.Decimal
}

// exportResource is one resource of the export.
type exportResource struct {
	// pairs are the intervals of the resource, sorted by their start once every
	// row has been read.
	pairs []*pair
	// index finds the interval a row belongs to, keyed by the two instants it
	// runs between. Both ends are kept as nanoseconds since the epoch, so two
	// rows spelling one instant in different offsets group into one interval,
	// the way time.Time.Equal reads them.
	index map[[2]int64]*pair
	// conflict is the first interval two rows booked under different states or
	// projects. Such a resource is reported and compared no further: the export
	// says two things about one interval, and there is no single one of them to
	// hold the oracle against.
	conflict *pair
	// duplicate is the first dimension of one interval two rows rate. A month
	// billed twice over carries the very same row twice, so the quantities
	// alone hold nothing of it: the second row writes what the first one wrote,
	// and a comparison that read the map back would find the month right.
	duplicate *duplicateRow
}

// duplicateRow is one dimension of one interval the export rates more than
// once.
type duplicateRow struct {
	pair      *pair
	dimension string
}

// add reads one row into the interval it belongs to, opening that interval the
// first time a row names it.
func (e *exportResource) add(row ratedRow) {
	key := [2]int64{row.From.UnixNano(), row.To.UnixNano()}
	held, ok := e.index[key]
	if !ok {
		held = &pair{
			From:       row.From,
			To:         row.To,
			State:      row.State,
			ProjectID:  row.ProjectID,
			Quantities: make(map[string]decimal.Decimal),
		}
		e.index[key] = held
		e.pairs = append(e.pairs, held)
	}
	if e.conflict == nil && (held.State != row.State || held.ProjectID != row.ProjectID) {
		e.conflict = held
	}
	if _, repeated := held.Quantities[row.Dimension]; repeated && e.duplicate == nil {
		e.duplicate = &duplicateRow{pair: held, dimension: row.Dimension}
	}
	held.Quantities[row.Dimension] = row.Quantity
}

// found collects the differences of a comparison per resource, in the order
// they were found. A resource it holds nothing for is one the export books the
// way the oracle states it.
type found map[resourceKey][]string

// note records one difference on a resource.
func (f found) note(key resourceKey, format string, args ...any) {
	f[key] = append(f[key], fmt.Sprintf(format, args...))
}

// compare holds the rows of an export against the oracle.
//
// The errors it returns are the ones no report could be written from: an
// export that bills another cloud, one that bills another month, and a model
// that prices the rated resource types differently than the run did, which is
// held in both directions — a dimension the export rates and the model does not
// price, and one the model prices that no record rates by. Each of them would
// turn every resource of the month into a difference, and a report of that is
// not a finding about the engine.
func compare(oracle Oracle, rows []ratedRow, model pricing.Model) (Report, error) {
	report := Report{PricingVersion: model.Version}

	kept := make([]ratedRow, 0, len(rows))
	for _, row := range rows {
		if row.Platform != platformOpenStack || row.Cloud != oracle.Cloud {
			report.Skipped++
			continue
		}
		kept = append(kept, row)
	}
	if len(kept) == 0 {
		return Report{}, fmt.Errorf(
			"rated.csv holds no rated record of cloud %s on platform %s: "+
				"the run that wrote it did not bill this month", oracle.Cloud, platformOpenStack)
	}

	for _, row := range kept {
		if !row.PeriodFrom.Equal(oracle.PeriodFrom) || !row.PeriodTo.Equal(oracle.PeriodTo) {
			return Report{}, fmt.Errorf("rated.csv bills [%s, %s) and the oracle describes [%s, %s)",
				instantText(row.PeriodFrom), instantText(row.PeriodTo),
				instantText(oracle.PeriodFrom), instantText(oracle.PeriodTo))
		}
	}

	entries := model.Pricing[platformOpenStack]
	rated := make(map[string]map[string]bool)
	for _, row := range kept {
		entry, ok := entries[row.ResourceType]
		if !ok || !prices(entry, row.Dimension) {
			return Report{}, fmt.Errorf(
				"rated.csv rates %s by %s, which pricing model %s does not price: "+
					"pass the model the run rated with", row.ResourceType, row.Dimension, model.Version)
		}
		if rated[row.ResourceType] == nil {
			rated[row.ResourceType] = make(map[string]bool)
		}
		rated[row.ResourceType][row.Dimension] = true
	}

	// The gate above walks the export to the model, and this one walks the model
	// to the export. A model that prices a rated resource type by a time gauge
	// no record of that type carries is not the model the run rated with either,
	// and comparing through it would report every interval of every resource of
	// the type for a dimension the run never rated. The types are walked in
	// sorted order, so a model that parts ways in more than one of them names
	// the same one on every run.
	for _, resourceType := range slices.Sorted(maps.Keys(rated)) {
		for _, dimension := range entries[resourceType].Dimensions {
			if dimension.Type != pricing.TypeTimeGauge || rated[resourceType][dimension.Metric] {
				continue
			}
			return Report{}, fmt.Errorf(
				"pricing model %s prices %s by %s and rated.csv rates no record by it: "+
					"pass the model the run rated with", model.Version, resourceType, dimension.Metric)
		}
	}

	exported := make(map[resourceKey]*exportResource)
	for _, row := range kept {
		key := resourceKey{resourceType: row.ResourceType, resourceID: row.ResourceID}
		resource, ok := exported[key]
		if !ok {
			resource = &exportResource{index: make(map[[2]int64]*pair)}
			exported[key] = resource
		}
		resource.add(row)
	}
	for _, resource := range exported {
		slices.SortFunc(resource.pairs, func(a, b *pair) int { return a.From.Compare(b.From) })
	}

	differences := make(found)
	unpriced := make(map[string]int)
	stated := make(map[resourceKey]bool, len(oracle.Resources))

	for _, resource := range oracle.Resources {
		key := resourceKey{resourceType: resource.ResourceType, resourceID: resource.ResourceID}
		stated[key] = true

		entry, ok := entries[resource.ResourceType]
		if !ok {
			unpriced[resource.ResourceType]++
			continue
		}
		report.Compared++

		booked, ok := exported[key]
		switch {
		case !ok:
			differences.note(key, "missing from the export")
		case booked.conflict != nil:
			differences.note(key, "the export books %s under more than one state or project",
				boundsText(booked.conflict.From, booked.conflict.To))
		case booked.duplicate != nil:
			differences.note(key, "the export rates %s by %s more than once",
				boundsText(booked.duplicate.pair.From, booked.duplicate.pair.To),
				booked.duplicate.dimension)
		default:
			compareIntervals(differences, key, entry, resource.Intervals, booked.pairs)
		}
	}

	for key := range exported {
		if stated[key] {
			continue
		}
		report.Compared++
		differences.note(key, "not in the oracle")
	}

	for key, details := range differences {
		report.Differences = append(report.Differences, Difference{
			ResourceType: key.resourceType,
			ResourceID:   key.resourceID,
			Detail:       details[0],
			More:         len(details) - 1,
		})
	}
	slices.SortFunc(report.Differences, func(a, b Difference) int {
		if c := strings.Compare(a.ResourceType, b.ResourceType); c != 0 {
			return c
		}
		return strings.Compare(a.ResourceID, b.ResourceID)
	})
	for resourceType, resources := range unpriced {
		report.Unpriced = append(report.Unpriced, UnpricedType{ResourceType: resourceType, Resources: resources})
	}
	slices.SortFunc(report.Unpriced, func(a, b UnpricedType) int {
		return strings.Compare(a.ResourceType, b.ResourceType)
	})
	return report, nil
}

// prices reports whether the entry holds a dimension of that metric.
func prices(entry pricing.ResourcePricing, metric string) bool {
	return slices.ContainsFunc(entry.Dimensions, func(d pricing.Dimension) bool { return d.Metric == metric })
}

// compareIntervals holds one resource's stated intervals against the ones the
// export books it over, index by index rather than by their bounds. An
// interval the engine split in two shifts every interval behind it, and a
// comparison that matched them up by their bounds would report the split as
// two unrelated findings instead of the one place the two folds part ways.
func compareIntervals(differences found, key resourceKey, entry pricing.ResourcePricing,
	expected []OracleInterval, actual []*pair,
) {
	for i := 0; i < len(expected) || i < len(actual); i++ {
		switch {
		case i >= len(actual):
			differences.note(key, "the export lacks %s", boundsText(expected[i].From, expected[i].To))
		case i >= len(expected):
			differences.note(key, "the export books %s, which the oracle does not hold",
				boundsText(actual[i].From, actual[i].To))
		case !expected[i].From.Equal(actual[i].From) || !expected[i].To.Equal(actual[i].To):
			differences.note(key, "the oracle expects %s and the export books %s",
				boundsText(expected[i].From, expected[i].To), boundsText(actual[i].From, actual[i].To))
		default:
			compareInterval(differences, key, entry, expected[i], actual[i])
		}
	}
}

// compareInterval holds one interval the two sides agree on the bounds of
// against what the export booked it as.
func compareInterval(differences found, key resourceKey, entry pricing.ResourcePricing,
	expected OracleInterval, actual *pair,
) {
	over := boundsText(expected.From, expected.To)
	if actual.State != expected.State {
		differences.note(key, "state %q over %s, the oracle expects %q", actual.State, over, expected.State)
	}
	if actual.ProjectID != expected.ProjectID {
		differences.note(key, "project %s over %s, the oracle expects %s", actual.ProjectID, over, expected.ProjectID)
	}

	for _, dimension := range entry.Dimensions {
		if dimension.Type != pricing.TypeTimeGauge {
			continue
		}
		booked, ok := actual.Quantities[dimension.Metric]
		if !ok {
			differences.note(key, "no %s quantity over %s", dimension.Metric, over)
			continue
		}
		// The expected quantity is rounded the way the engine rounds one before
		// it rates it, so a size of more than four places is held to the value
		// the export prints rather than to one no document carries.
		want := money.RoundQuantity(expectedQuantity(dimension.Metric, expected))
		if !want.Equal(booked) {
			differences.note(key, "%s %s over %s, the oracle expects %s", dimension.Metric,
				booked.StringFixed(money.QuantityPlaces), over, want.StringFixed(money.QuantityPlaces))
		}
	}
}

// expectedQuantity is what one time gauge dimension of an interval is billed
// over: the count of one that prices a resource by its existence, the minutes
// the interval lasts, or the size member the dimension is named after.
//
// A member the size does not hold, and one holding text rather than a number,
// are zero: that is what the engine bills a dimension it reads no quantity for,
// so the comparison expects the same rather than reporting a difference the
// pricing model is the cause of.
func expectedQuantity(metric string, interval OracleInterval) decimal.Decimal {
	switch metric {
	case metricCount:
		return decimal.NewFromInt(1)
	case metricMinutes:
		return money.Minutes(int64(interval.To.Sub(interval.From) / time.Second))
	}

	number, ok := interval.Size[metric].(json.Number)
	if !ok {
		return decimal.Zero
	}
	// The oracle is decoded under Decoder.UseNumber, so the text here is the
	// text the notification carried, and a number that is no decimal has been
	// refused by the JSON decoder long before.
	value, err := decimal.NewFromString(number.String())
	if err != nil {
		return decimal.Zero
	}
	return value
}

// instantText renders one instant the way a difference names it: in UTC and to
// the second, which is the resolution every transition of a month happens at.
func instantText(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// boundsText renders the half-open interval [from, to).
func boundsText(from, to time.Time) string {
	return "[" + instantText(from) + ", " + instantText(to) + ")"
}

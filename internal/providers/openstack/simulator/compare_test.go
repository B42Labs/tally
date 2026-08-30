package simulator

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/event"
	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/engine/metering"
	"github.com/b42labs/tally/internal/engine/pricing"
	"github.com/b42labs/tally/internal/engine/rating"
	"github.com/b42labs/tally/internal/engine/source"
	"github.com/b42labs/tally/internal/providers/openstack"
)

// testModel prices the three resource types of a month and leaves the other
// two unpriced, which is what pricing/2026-03.yaml does with an image and a
// load balancer as well. The prices are the ones of that file: nothing here
// reads an amount, so what they are decides nothing, and taking them from the
// shipped model keeps the fixture a model an operator could have written.
const testModel = `
version: "test"
valid_from: "2026-01-01T00:00:00Z"
currency: "EUR"

pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.001"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.09"
    volume:
      dimensions:
        - metric: "size_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.0001"
        - metric: "minutes"
          type: "time_gauge"
          price_per_unit_hour: "0.00001"
    floating_ip:
      dimensions:
        - metric: "count"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
`

// volumeModel prices the volumes of a month and nothing else, for the report a
// model that prices little produces: four of the five resource types of a
// month are then counted rather than compared.
const volumeModel = `
version: "volumes"
valid_from: "2026-01-01T00:00:00Z"
currency: "EUR"

pricing:
  openstack:
    volume:
      dimensions:
        - metric: "size_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.0001"
`

// wideVolumeModel prices a volume by a dimension beside the one volumeModel
// prices it by. It is a model of another run than the one that wrote the
// export: an export rendered under volumeModel rates no volume by disk_gb, and
// a comparison that read the month through this model would report every
// interval of every volume for a quantity the run never rated.
const wideVolumeModel = `
version: "wide"
valid_from: "2026-01-01T00:00:00Z"
currency: "EUR"

pricing:
  openstack:
    volume:
      dimensions:
        - metric: "size_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.0001"
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.001"
`

// textModel prices an instance by the two things no size states a number for:
// the flavor's name, which every instance size holds as text, and a metric no
// size holds at all. The engine bills a dimension it reads no quantity for at
// zero, so an export rated under this model carries a zero for both.
const textModel = `
version: "text"
valid_from: "2026-01-01T00:00:00Z"
currency: "EUR"

pricing:
  openstack:
    instance:
      dimensions:
        - metric: "flavor"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "iops"
          type: "time_gauge"
          price_per_unit_hour: "0.001"
`

// testRunID is the run every rendered row belongs to. A comparison reads
// neither the run nor its kind, so one id stands for every export a test
// writes.
const testRunID = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

// testEgress is the quantity every counter row carries. No transition of a
// simulated month meters egress, so the value is one a comparison must not
// read: what it does with it is a test of its own.
const testEgress = "12.3456"

// ratedTestHeader is the header of rated.csv, which is ratedHeader of
// internal/engine/export/csv.go. That one is unexported, and the columns are
// what the two packages agree on rather than what either of them holds, so it
// is written out here.
var ratedTestHeader = []string{
	"run_id", "kind", "corrects_run_id", "period_from", "period_to",
	"cloud", "platform", "resource_type", "resource_id", "project_id", "state",
	"from_ts", "to_ts", "dimension", "quantity", "amount", "currency",
}

// The positions a mutation reaches a cell by, in the order ratedTestHeader
// lists the columns.
const (
	columnPeriodFrom   = 3
	columnPeriodTo     = 4
	columnCloud        = 5
	columnPlatform     = 6
	columnResourceType = 7
	columnResourceID   = 8
	columnProjectID    = 9
	columnState        = 10
	columnFromTS       = 11
	columnToTS         = 12
	columnDimension    = 13
	columnQuantity     = 14
)

// sampleRatedRow is one row of rated.csv spelled out, so that the reader's own
// tests hold it against values written by hand rather than against a generated
// month.
var sampleRatedRow = []string{
	testRunID, "regular", "",
	"2026-07-01T00:00:00Z", "2026-08-01T00:00:00Z",
	testCloud, platformOpenStack, "volume", "1c38e4d1-42db-4271-bd32-9653e80a3603",
	"3f4b0d2e-0a1f-4b6a-9a6d-0a3c1b2d4e5f", "available",
	"2026-07-04T09:00:00Z", "2026-07-05T09:00:00Z", "size_gb", "50.0000", "0.12", "EUR",
}

// parseModel parses a pricing model fixture.
func parseModel(t *testing.T, document string) pricing.Model {
	t.Helper()

	model, _, err := pricing.Parse([]byte(document))
	if err != nil {
		t.Fatalf("pricing.Parse() error = %v, want nil", err)
	}
	return model
}

// oracleOf is seed 1's July 2026 over testCloud as a comparison reads it:
// written to a file and read back, so every number of a size is the
// json.Number ReadOracle decodes rather than the one the generator held.
func oracleOf(t *testing.T) Oracle {
	t.Helper()

	path := writeOracle(t, generatedOracle(t, 1))
	oracle, err := ReadOracle(path)
	if err != nil {
		t.Fatalf("ReadOracle(%s) error = %v, want nil", path, err)
	}
	return oracle
}

// writeOracle writes an oracle into a directory of its own and returns its
// path.
func writeOracle(t *testing.T, oracle Oracle) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "oracle.json")
	if err := WriteOracle(path, oracle); err != nil {
		t.Fatalf("WriteOracle(%s) error = %v, want nil", path, err)
	}
	return path
}

// writeModel writes a pricing model fixture into a directory of its own and
// returns its path.
func writeModel(t *testing.T, document string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "pricing.yaml")
	if err := os.WriteFile(path, []byte(document), streamFileMode); err != nil {
		t.Fatalf("WriteFile(%s) error = %v, want nil", path, err)
	}
	return path
}

// writeCSV writes rows verbatim, the header among them, into a directory of
// its own and returns the file's path.
func writeCSV(t *testing.T, rows [][]string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rated.csv")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, streamFileMode)
	if err != nil {
		t.Fatalf("OpenFile(%s) error = %v, want nil", path, err)
	}
	defer func() { _ = file.Close() }()

	writer := csv.NewWriter(file)
	if err := writer.WriteAll(rows); err != nil {
		t.Fatalf("WriteAll(%s) error = %v, want nil", path, err)
	}
	return path
}

// writeRated writes an export's rows under the header and returns the
// directory holding rated.csv, which is what --export names.
func writeRated(t *testing.T, rows [][]string) string {
	t.Helper()

	return filepath.Dir(writeCSV(t, append([][]string{ratedTestHeader}, rows...)))
}

// ratedOf renders the rows an export holds for a month the engine rated
// without a fault: one per resource of a priced type, interval, and dimension
// of its pricing entry. A time gauge carries the quantity the oracle states,
// and a counter the one nothing reads.
func ratedOf(t *testing.T, oracle Oracle, model pricing.Model) [][]string {
	t.Helper()

	rows := make([][]string, 0, len(oracle.Resources))
	for _, resource := range oracle.Resources {
		entry, ok := model.Pricing[platformOpenStack][resource.ResourceType]
		if !ok {
			continue
		}
		for _, interval := range resource.Intervals {
			for _, dimension := range entry.Dimensions {
				quantity := testEgress
				if dimension.Type == pricing.TypeTimeGauge {
					quantity = testQuantityOf(t, dimension.Metric, interval).StringFixed(money.QuantityPlaces)
				}
				rows = append(rows, ratedRowOf(oracle, resource, interval, dimension.Metric, quantity))
			}
		}
	}
	return rows
}

// ratedRowOf renders one row of rated.csv.
func ratedRowOf(oracle Oracle, resource OracleResource, interval OracleInterval,
	dimension, quantity string,
) []string {
	return []string{
		testRunID, "regular", "",
		instantCell(oracle.PeriodFrom), instantCell(oracle.PeriodTo),
		oracle.Cloud, platformOpenStack, resource.ResourceType, resource.ResourceID,
		interval.ProjectID, interval.State,
		instantCell(interval.From), instantCell(interval.To), dimension, quantity,
		"0.00", "EUR",
	}
}

// testQuantityOf is the quantity a rated row carries for one time gauge
// dimension of an interval: the count of one that prices a resource by its
// existence, the minutes the interval lasts, or the size member the dimension
// is named after. A dimension the size states nothing for fails the test, so a
// fixture pricing a metric no month reports is caught here rather than read as
// a difference.
func testQuantityOf(t *testing.T, metric string, interval OracleInterval) decimal.Decimal {
	t.Helper()

	switch metric {
	case metricCount:
		return decimal.NewFromInt(1)
	case metricMinutes:
		return money.Minutes(int64(interval.To.Sub(interval.From) / time.Second))
	}

	number, ok := interval.Size[metric].(json.Number)
	if !ok {
		t.Fatalf("the interval carries %s = %v, want the json.Number the dimension is priced by",
			metric, interval.Size[metric])
	}
	value, err := decimal.NewFromString(number.String())
	if err != nil {
		t.Fatalf("NewFromString(%q) error = %v, want nil", number.String(), err)
	}
	return money.RoundQuantity(value)
}

// instantCell renders an instant the way an export writes one.
func instantCell(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// over renders the interval a difference names.
func over(from, to time.Time) string {
	return fmt.Sprintf("[%s, %s)", from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
}

// runCompare writes the three files a comparison reads and runs it.
func runCompare(t *testing.T, oracle Oracle, rows [][]string, document string) (Report, error) {
	t.Helper()

	return Compare(CompareOptions{
		Oracle:  writeOracle(t, oracle),
		Export:  writeRated(t, rows),
		Pricing: writeModel(t, document),
	})
}

// compareRows runs a comparison that has to produce a report.
func compareRows(t *testing.T, oracle Oracle, rows [][]string, document string) Report {
	t.Helper()

	report, err := runCompare(t, oracle, rows, document)
	if err != nil {
		t.Fatalf("Compare() error = %v, want nil", err)
	}
	return report
}

// pickResource is the first resource of the oracle of that type, with that
// many intervals, and in that state over its first interval; an empty state
// takes any. It fails the test rather than come back empty: every case below
// names a shape seed 1 holds, and a seed that stopped holding one is a fixture
// to fix rather than a case to pass over.
func pickResource(t *testing.T, oracle Oracle, resourceType string, intervals int, state string) OracleResource {
	t.Helper()

	for _, resource := range oracle.Resources {
		if resource.ResourceType != resourceType || len(resource.Intervals) != intervals {
			continue
		}
		if state != "" && resource.Intervals[0].State != state {
			continue
		}
		return resource
	}
	t.Fatalf("seed 1 holds no %s over %d intervals in state %q, want one to mutate",
		resourceType, intervals, state)
	return OracleResource{}
}

// rowsOf matches every row of one resource.
func rowsOf(id string) func([]string) bool {
	return func(row []string) bool { return row[columnResourceID] == id }
}

// rowsOfDimension matches the row one dimension of one resource is rated by.
func rowsOfDimension(id, dimension string) func([]string) bool {
	return func(row []string) bool {
		return row[columnResourceID] == id && row[columnDimension] == dimension
	}
}

// editRows copies the table and hands every matching row to change, so the
// mutation of one case leaves the table the next one starts from as it was.
func editRows(rows [][]string, match func([]string) bool, change func([]string)) [][]string {
	edited := make([][]string, 0, len(rows))
	for _, row := range rows {
		if match(row) {
			row = slices.Clone(row)
			change(row)
		}
		edited = append(edited, row)
	}
	return edited
}

// dropRows copies the table without the matching rows.
func dropRows(rows [][]string, match func([]string) bool) [][]string {
	kept := make([][]string, 0, len(rows))
	for _, row := range rows {
		if !match(row) {
			kept = append(kept, row)
		}
	}
	return kept
}

// copyRows copies the table and appends a changed copy of every matching row.
func copyRows(rows [][]string, match func([]string) bool, change func([]string)) [][]string {
	copied := slices.Clone(rows)
	for _, row := range rows {
		if !match(row) {
			continue
		}
		added := slices.Clone(row)
		change(added)
		copied = append(copied, added)
	}
	return copied
}

// reported is the head of a report, for the message of a test that expected
// none of its lines. A month runs to hundreds of resources, and a failure that
// printed every one of them would bury the first.
func reported(report Report) string {
	lines := report.Lines()
	if len(lines) > 10 {
		lines = lines[:10]
	}
	return strings.Join(lines, "\n")
}

// sameDifference reports whether two differences say the same thing about one
// resource. The switches are held by value, where a difference that names none
// and one whose list is empty are the same finding.
func sameDifference(a, b Difference) bool {
	return a.ResourceType == b.ResourceType && a.ResourceID == b.ResourceID &&
		a.Detail == b.Detail && a.More == b.More && slices.Equal(a.Faults, b.Faults)
}

// differenceOf is the report's difference about one resource, or a failed test.
func differenceOf(t *testing.T, report Report, id string) Difference {
	t.Helper()

	for _, difference := range report.Differences {
		if difference.ResourceID == id {
			return difference
		}
	}
	t.Fatalf("the report holds no difference about %s:\n%s", id, reported(report))
	return Difference{}
}

// pricedResources counts the oracle's resources the model prices and the ones
// it does not, per type.
func pricedResources(oracle Oracle, model pricing.Model) (priced int, unpriced map[string]int) {
	unpriced = make(map[string]int)
	for _, resource := range oracle.Resources {
		if _, ok := model.Pricing[platformOpenStack][resource.ResourceType]; ok {
			priced++
			continue
		}
		unpriced[resource.ResourceType]++
	}
	return priced, unpriced
}

// TestCompareAcceptsWhatTheEngineRates rates seed 1's month through the engine
// and holds the export it would have written against the oracle. The rows come
// from the collector's mapping, the metering fold and the rating pass, so the
// one thing this test can fail on is the comparison reporting a month that was
// billed the way it was meant to be.
func TestCompareAcceptsWhatTheEngineRates(t *testing.T) {
	to := july2026.AddDate(0, 1, 0)
	month, err := GenerateMonth(1, july2026, to, testCloud, Faults{})
	if err != nil {
		t.Fatalf("GenerateMonth(1, %s, %q) error = %v, want nil", july2026.Format(time.RFC3339), testCloud, err)
	}

	history := make(map[resourceKey][]event.Stored)
	for _, transition := range month.Schedule.Billable() {
		mapped, ok := openstack.MapNotification(parse(t, render(t, transition)), testCloud)
		if !ok {
			t.Fatalf("the mapping records nothing for %s at %s, want an event per billable transition",
				transition.EventType, transition.At.Format(time.RFC3339))
		}
		key := resourceKey{resourceType: mapped.ResourceType, resourceID: mapped.ResourceID}
		history[key] = append(history[key], event.Stored{Event: mapped})
	}

	usage := make([]metering.ResourceUsage, 0, len(history))
	drafts := make(map[source.Resource][]metering.UsageDraft, len(history))
	for key, events := range history {
		metered, err := metering.MeterResource(events, july2026, to)
		if err != nil {
			t.Fatalf("MeterResource(%s %s) error = %v, want nil", key.resourceType, key.resourceID, err)
		}
		// A resource the period bills nothing for reaches no rated record, and
		// the oracle states none for it either.
		if len(metered) == 0 {
			continue
		}
		resource := source.Resource{
			Cloud:        testCloud,
			Platform:     platformOpenStack,
			ResourceType: key.resourceType,
			ResourceID:   key.resourceID,
		}
		usage = append(usage, metering.ResourceUsage{Resource: resource, Drafts: metered})
		drafts[resource] = metered
	}

	model := parseModel(t, testModel)
	result := rating.Rate(model, usage)

	var rows [][]string
	for _, rated := range result.Resources {
		metered := drafts[rated.Resource]
		for i, record := range rated.Records {
			draft := metered[i]
			for _, amount := range record.Amounts {
				rows = append(rows, []string{
					testRunID, "regular", "",
					instantCell(month.Oracle.PeriodFrom), instantCell(month.Oracle.PeriodTo),
					rated.Resource.Cloud, rated.Resource.Platform,
					rated.Resource.ResourceType, rated.Resource.ResourceID,
					draft.ProjectID, draft.State,
					instantCell(draft.FromTS), instantCell(draft.ToTS), amount.Metric,
					amount.Quantity.StringFixed(money.QuantityPlaces),
					amount.Amount.StringFixed(money.AmountPlaces),
					result.Currency,
				})
			}
		}
	}

	report := compareRows(t, month.Oracle, rows, testModel)
	if len(report.Differences) != 0 {
		t.Errorf("Compare() differences = %d, want none:\n%s", len(report.Differences), reported(report))
	}

	priced, unpriced := pricedResources(month.Oracle, model)
	if report.Compared != priced {
		t.Errorf("Compare() compared = %d, want the %d priced resources of the oracle", report.Compared, priced)
	}
	want := []UnpricedType{
		{ResourceType: "image", Resources: unpriced["image"]},
		{ResourceType: "loadbalancer", Resources: unpriced["loadbalancer"]},
	}
	if !slices.Equal(report.Unpriced, want) {
		t.Errorf("Compare() unpriced = %v, want %v", report.Unpriced, want)
	}
	if report.Skipped != 0 {
		t.Errorf("Compare() skipped = %d, want none", report.Skipped)
	}

	lines := report.Lines()
	last, wantLast := lines[len(lines)-1], fmt.Sprintf("the export matches the oracle over %d resources", priced)
	if last != wantLast {
		t.Errorf("the last line = %q, want %q", last, wantLast)
	}
}

// TestCompareReportsEveryKindOfDifference mutates one resource of an export
// that matches and holds the report against the one difference the mutation
// left behind. Every case reports its resource once, however many of its rows
// it changed, and leaves the other resources of the month alone.
func TestCompareReportsEveryKindOfDifference(t *testing.T) {
	oracle := oracleOf(t)
	model := parseModel(t, testModel)
	rows := ratedOf(t, oracle, model)
	priced, _ := pricedResources(oracle, model)

	volume := pickResource(t, oracle, "volume", 1, "")
	volumeSpan := volume.Intervals[0]
	split := pickResource(t, oracle, "volume", 2, "")
	splitSpan := split.Intervals[1]
	instance := pickResource(t, oracle, "instance", 1, stateActive)
	instanceSpan := instance.Intervals[0]
	address := pickResource(t, oracle, "floating_ip", 1, "")
	addressSpan := address.Intervals[0]

	const unknownID = "11111111-1111-1111-1111-111111111111"
	const otherProject = "44444444-4444-4444-4444-444444444444"
	vcpus := testQuantityOf(t, "vcpus", instanceSpan)
	wrongVCPUs := vcpus.Add(decimal.NewFromInt(1))

	for _, tc := range []struct {
		name     string
		mutate   func([][]string) [][]string
		want     Difference
		compared int
	}{
		{
			name:   "a resource the export lost",
			mutate: func(rows [][]string) [][]string { return dropRows(rows, rowsOf(volume.ResourceID)) },
			want: Difference{
				ResourceType: "volume", ResourceID: volume.ResourceID,
				Detail: "missing from the export",
			},
		},
		{
			name: "a resource the oracle never held",
			mutate: func(rows [][]string) [][]string {
				return copyRows(rows, rowsOf(volume.ResourceID), func(row []string) {
					row[columnResourceID] = unknownID
				})
			},
			want: Difference{
				ResourceType: "volume", ResourceID: unknownID,
				Detail: "not in the oracle",
			},
			compared: priced + 1,
		},
		{
			name: "an interval the export lost",
			mutate: func(rows [][]string) [][]string {
				return dropRows(rows, func(row []string) bool {
					return row[columnResourceID] == split.ResourceID &&
						row[columnFromTS] == instantCell(splitSpan.From)
				})
			},
			want: Difference{
				ResourceType: "volume", ResourceID: split.ResourceID,
				Detail: "the export lacks " + over(splitSpan.From, splitSpan.To),
			},
		},
		{
			name: "an interval the oracle never held",
			mutate: func(rows [][]string) [][]string {
				return copyRows(rows, rowsOf(address.ResourceID), func(row []string) {
					row[columnFromTS] = instantCell(addressSpan.From.Add(time.Hour))
					row[columnToTS] = instantCell(addressSpan.To.Add(time.Hour))
				})
			},
			want: Difference{
				ResourceType: "floating_ip", ResourceID: address.ResourceID,
				Detail: "the export books " +
					over(addressSpan.From.Add(time.Hour), addressSpan.To.Add(time.Hour)) +
					", which the oracle does not hold",
			},
		},
		{
			name: "an interval that ends too early",
			mutate: func(rows [][]string) [][]string {
				return editRows(rows, rowsOf(volume.ResourceID), func(row []string) {
					row[columnToTS] = instantCell(volumeSpan.To.Add(-time.Second))
				})
			},
			want: Difference{
				ResourceType: "volume", ResourceID: volume.ResourceID,
				Detail: fmt.Sprintf("the oracle expects %s and the export books %s",
					over(volumeSpan.From, volumeSpan.To), over(volumeSpan.From, volumeSpan.To.Add(-time.Second))),
			},
		},
		{
			name: "an interval booked in another state",
			mutate: func(rows [][]string) [][]string {
				return editRows(rows, rowsOf(instance.ResourceID), func(row []string) {
					row[columnState] = stateShutoff
				})
			},
			want: Difference{
				ResourceType: "instance", ResourceID: instance.ResourceID,
				Detail: fmt.Sprintf("state %q over %s, the oracle expects %q",
					stateShutoff, over(instanceSpan.From, instanceSpan.To), stateActive),
			},
		},
		{
			name: "an interval booked to another project",
			mutate: func(rows [][]string) [][]string {
				return editRows(rows, rowsOf(volume.ResourceID), func(row []string) {
					row[columnProjectID] = otherProject
				})
			},
			want: Difference{
				ResourceType: "volume", ResourceID: volume.ResourceID,
				Detail: fmt.Sprintf("project %s over %s, the oracle expects %s",
					otherProject, over(volumeSpan.From, volumeSpan.To), volumeSpan.ProjectID),
			},
		},
		{
			name: "a quantity of its own",
			mutate: func(rows [][]string) [][]string {
				return editRows(rows, rowsOfDimension(instance.ResourceID, "vcpus"), func(row []string) {
					row[columnQuantity] = wrongVCPUs.StringFixed(money.QuantityPlaces)
				})
			},
			want: Difference{
				ResourceType: "instance", ResourceID: instance.ResourceID,
				Detail: fmt.Sprintf("vcpus %s over %s, the oracle expects %s",
					wrongVCPUs.StringFixed(money.QuantityPlaces),
					over(instanceSpan.From, instanceSpan.To), vcpus.StringFixed(money.QuantityPlaces)),
			},
		},
		{
			name: "a dimension the export never rated",
			mutate: func(rows [][]string) [][]string {
				return dropRows(rows, rowsOfDimension(instance.ResourceID, "disk_gb"))
			},
			want: Difference{
				ResourceType: "instance", ResourceID: instance.ResourceID,
				Detail: "no disk_gb quantity over " + over(instanceSpan.From, instanceSpan.To),
			},
		},
		{
			name: "one interval booked twice over",
			mutate: func(rows [][]string) [][]string {
				return editRows(rows, rowsOfDimension(instance.ResourceID, "vcpus"), func(row []string) {
					row[columnState] = stateShutoff
				})
			},
			want: Difference{
				ResourceType: "instance", ResourceID: instance.ResourceID,
				Detail: "the export books " + over(instanceSpan.From, instanceSpan.To) +
					" under more than one state or project",
			},
		},
		{
			name: "one interval rated twice by one dimension",
			mutate: func(rows [][]string) [][]string {
				return copyRows(rows, rowsOfDimension(volume.ResourceID, "size_gb"), func([]string) {})
			},
			want: Difference{
				ResourceType: "volume", ResourceID: volume.ResourceID,
				Detail: "the export rates " + over(volumeSpan.From, volumeSpan.To) +
					" by size_gb more than once",
			},
		},
		{
			name: "a resource that differs twice",
			mutate: func(rows [][]string) [][]string {
				changed := editRows(rows, rowsOf(instance.ResourceID), func(row []string) {
					row[columnState] = stateShutoff
				})
				return editRows(changed, rowsOfDimension(instance.ResourceID, "vcpus"), func(row []string) {
					row[columnQuantity] = wrongVCPUs.StringFixed(money.QuantityPlaces)
				})
			},
			want: Difference{
				ResourceType: "instance", ResourceID: instance.ResourceID,
				Detail: fmt.Sprintf("state %q over %s, the oracle expects %q",
					stateShutoff, over(instanceSpan.From, instanceSpan.To), stateActive),
				More: 1,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := compareRows(t, oracle, tc.mutate(rows), testModel)
			if len(report.Differences) != 1 {
				t.Fatalf("Compare() differences = %d, want one:\n%s", len(report.Differences), reported(report))
			}
			if !sameDifference(report.Differences[0], tc.want) {
				t.Errorf("Compare() difference = %+v, want %+v", report.Differences[0], tc.want)
			}

			compared := tc.compared
			if compared == 0 {
				compared = priced
			}
			if report.Compared != compared {
				t.Errorf("Compare() compared = %d, want %d", report.Compared, compared)
			}
			lines := report.Lines()
			last := lines[len(lines)-1]
			wantLast := fmt.Sprintf("1 of %d resources differ from the oracle", compared)
			if last != wantLast {
				t.Errorf("the last line = %q, want %q", last, wantLast)
			}
			// The count of the further differences is rendered onto the line of
			// the resource that carries them, which is the half of the field an
			// operator reads.
			suffix := ""
			if tc.want.More > 0 {
				suffix = fmt.Sprintf(" (and %d more)", tc.want.More)
			}
			wantFirst := fmt.Sprintf("%s %s: %s%s", tc.want.ResourceType, tc.want.ResourceID,
				tc.want.Detail, suffix)
			if lines[0] != wantFirst {
				t.Errorf("the first line = %q, want %q", lines[0], wantFirst)
			}
		})
	}
}

// TestCompareCopiesTheFaultsOntoADifference holds the switches a report
// carries: the ones the month ran with, and the ones that touched the resource
// a difference is about. A difference beside its switches is one the drill's
// write-up explains in a line, and one without them is a finding about the
// engine.
func TestCompareCopiesTheFaultsOntoADifference(t *testing.T) {
	oracle := oracleOf(t)
	// Stated by hand, because no switch touches a resource of a generated month
	// yet. The order is the document's rather than FaultNames', which is what a
	// report keeps and its lines sort.
	oracle.Faults = []string{FaultHeldBack, FaultDuplicates}
	touched := pickResource(t, oracle, "volume", 1, "")
	for i := range oracle.Resources {
		if oracle.Resources[i].ResourceID == touched.ResourceID {
			oracle.Resources[i].Faults = []string{FaultMissingCreate}
		}
	}
	instance := pickResource(t, oracle, "instance", 1, stateActive)
	rows := ratedOf(t, oracle, parseModel(t, testModel))

	t.Run("an export that differs on both sides", func(t *testing.T) {
		const unknownID = "22222222-2222-2222-2222-222222222222"
		mutated := copyRows(dropRows(rows, rowsOf(touched.ResourceID)), rowsOf(instance.ResourceID),
			func(row []string) { row[columnResourceID] = unknownID })

		report := compareRows(t, oracle, mutated, testModel)
		if len(report.Differences) != 2 {
			t.Fatalf("Compare() differences = %d, want two:\n%s", len(report.Differences), reported(report))
		}

		missing := differenceOf(t, report, touched.ResourceID)
		if want := []string{FaultMissingCreate}; !slices.Equal(missing.Faults, want) {
			t.Errorf("the difference about %s names the switches %v, want %v",
				touched.ResourceID, missing.Faults, want)
		}
		if invented := differenceOf(t, report, unknownID); len(invented.Faults) != 0 {
			t.Errorf("the difference about the resource the oracle does not hold names the switches %v, "+
				"want none", invented.Faults)
		}
		if !slices.Equal(report.Faults, oracle.Faults) {
			t.Errorf("Compare() faults = %v, want the oracle's %v", report.Faults, oracle.Faults)
		}
	})

	t.Run("an export the oracle matches", func(t *testing.T) {
		report := compareRows(t, oracle, rows, testModel)
		if len(report.Differences) != 0 {
			t.Errorf("Compare() differences = %d, want none:\n%s", len(report.Differences), reported(report))
		}
		line := "the month ran with the fault switches duplicates, held-back"
		if lines := report.Lines(); !slices.Contains(lines, line) {
			t.Errorf("Lines() = %v, want a line %q", lines, line)
		}
	})
}

// TestReportLinesNameTheFaultSwitches renders the reports a month with switches
// on produces. A switch is named on the line of the resource it touched and
// once for the month, and neither changes the verdict: whether a difference is
// the one a switch was turned on for is what the drill's write-up decides.
func TestReportLinesNameTheFaultSwitches(t *testing.T) {
	const id = "1c38e4d1-42db-4271-bd32-9653e80a3603"
	const detail = "missing from the export"

	for _, tc := range []struct {
		name   string
		report Report
		want   []string
	}{
		{
			name: "a difference on a resource a switch touched",
			report: Report{
				Compared: 4,
				Differences: []Difference{{
					ResourceType: "instance", ResourceID: id,
					Detail: detail, Faults: []string{FaultMissingCreate},
				}},
			},
			want: []string{
				"instance " + id + ": missing from the export (touched by missing-create)",
				"1 of 4 resources differ from the oracle",
			},
		},
		{
			name: "a difference that carries further ones",
			report: Report{
				Compared: 4,
				Differences: []Difference{{
					ResourceType: "instance", ResourceID: id,
					Detail: detail, More: 3, Faults: []string{FaultMissingCreate},
				}},
			},
			want: []string{
				"instance " + id + ": missing from the export (and 3 more) (touched by missing-create)",
				"1 of 4 resources differ from the oracle",
			},
		},
		{
			name:   "the switches the month ran with",
			report: Report{Compared: 4, Faults: []string{FaultHeldBack, FaultDuplicates}},
			want: []string{
				"the month ran with the fault switches duplicates, held-back",
				"the export matches the oracle over 4 resources",
			},
		},
		{
			name: "a month no switch ran with",
			report: Report{
				Compared:    4,
				Differences: []Difference{{ResourceType: "instance", ResourceID: id, Detail: detail}},
			},
			want: []string{
				"instance " + id + ": missing from the export",
				"1 of 4 resources differ from the oracle",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if lines := tc.report.Lines(); !slices.Equal(lines, tc.want) {
				t.Errorf("Lines() = %v, want %v", lines, tc.want)
			}
		})
	}
}

// TestCompareLeavesCounterDimensionsOut holds the one quantity a comparison
// must not read. Nothing of a simulated month meters egress, so the counter of
// an export is read from a source no oracle knows, and holding it against one
// would report every instance of the month.
func TestCompareLeavesCounterDimensionsOut(t *testing.T) {
	oracle := oracleOf(t)
	model := parseModel(t, testModel)
	rows := editRows(ratedOf(t, oracle, model), func(row []string) bool {
		return row[columnDimension] == "egress_gb"
	}, func(row []string) {
		row[columnQuantity] = "99.9999"
	})

	report := compareRows(t, oracle, rows, testModel)
	if len(report.Differences) != 0 {
		t.Errorf("Compare() differences = %d, want none:\n%s", len(report.Differences), reported(report))
	}
}

// TestCompareReportsWhatIsNotPriced runs a model that prices the volumes of a
// month and nothing else. The four other types reach no rated record at all,
// because the rating pass skips them, so a comparison counts them per type
// instead of reporting every one of them missing.
func TestCompareReportsWhatIsNotPriced(t *testing.T) {
	oracle := oracleOf(t)
	model := parseModel(t, volumeModel)
	rows := ratedOf(t, oracle, model)

	report := compareRows(t, oracle, rows, volumeModel)
	if len(report.Differences) != 0 {
		t.Errorf("Compare() differences = %d, want none:\n%s", len(report.Differences), reported(report))
	}

	priced, unpriced := pricedResources(oracle, model)
	if report.Compared != priced {
		t.Errorf("Compare() compared = %d, want the %d volumes of the oracle", report.Compared, priced)
	}
	want := []UnpricedType{
		{ResourceType: "floating_ip", Resources: unpriced["floating_ip"]},
		{ResourceType: "image", Resources: unpriced["image"]},
		{ResourceType: "instance", Resources: unpriced["instance"]},
		{ResourceType: "loadbalancer", Resources: unpriced["loadbalancer"]},
	}
	if !slices.Equal(report.Unpriced, want) {
		t.Errorf("Compare() unpriced = %v, want %v", report.Unpriced, want)
	}

	lines := report.Lines()
	for _, entry := range want {
		line := fmt.Sprintf("%s: %d resources are not priced by pricing model volumes and were not compared",
			entry.ResourceType, entry.Resources)
		if !slices.Contains(lines, line) {
			t.Errorf("Lines() = %v, want a line %q", lines, line)
		}
	}
}

// TestCompareRefusesAModelTheRunDidNotRateWith holds the gate that keeps a
// comparison from reading an export through prices it was not rated with. A
// model that prices less than the run did would turn every record it does not
// know into a difference, and the resource type it named would be the one
// thing about the report that was right.
func TestCompareRefusesAModelTheRunDidNotRateWith(t *testing.T) {
	oracle := oracleOf(t)
	model := parseModel(t, testModel)
	rows := ratedOf(t, oracle, model)
	volume := pickResource(t, oracle, "volume", 1, "")
	image := pickResource(t, oracle, "image", 1, "")

	for _, tc := range []struct {
		name   string
		mutate func([][]string) [][]string
		want   string
	}{
		{
			name: "a dimension the model does not hold",
			mutate: func(rows [][]string) [][]string {
				return copyRows(rows, rowsOfDimension(volume.ResourceID, "size_gb"), func(row []string) {
					row[columnDimension] = "iops"
				})
			},
			want: "rated.csv rates volume by iops, which pricing model test does not price: " +
				"pass the model the run rated with",
		},
		{
			name: "a resource type the model does not hold",
			mutate: func(rows [][]string) [][]string {
				return copyRows(rows, rowsOfDimension(volume.ResourceID, "size_gb"), func(row []string) {
					row[columnResourceType] = "image"
					row[columnResourceID] = image.ResourceID
				})
			},
			want: "rated.csv rates image by size_gb, which pricing model test does not price: " +
				"pass the model the run rated with",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runCompare(t, oracle, tc.mutate(rows), testModel)
			if err == nil || err.Error() != tc.want {
				t.Errorf("Compare() error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestCompareRefusesAModelThatPricesMoreThanTheRunRatedBy holds the other half
// of the same gate. A model that prices a rated resource type by a time gauge
// nothing in the export rates it by is not the model the run rated with either,
// and a comparison that read the month through it would report every interval
// of every resource of that type for a dimension the run never rated. A counter
// is outside the gate, because a comparison reads none of them.
func TestCompareRefusesAModelThatPricesMoreThanTheRunRatedBy(t *testing.T) {
	oracle := oracleOf(t)

	t.Run("a time gauge no record rates", func(t *testing.T) {
		rows := ratedOf(t, oracle, parseModel(t, volumeModel))

		want := "pricing model wide prices volume by disk_gb and rated.csv rates no record by it: " +
			"pass the model the run rated with"
		if _, err := runCompare(t, oracle, rows, wideVolumeModel); err == nil || err.Error() != want {
			t.Errorf("Compare() error = %v, want %q", err, want)
		}
	})

	t.Run("a counter no record rates", func(t *testing.T) {
		rows := dropRows(ratedOf(t, oracle, parseModel(t, testModel)), func(row []string) bool {
			return row[columnDimension] == "egress_gb"
		})

		report := compareRows(t, oracle, rows, testModel)
		if len(report.Differences) != 0 {
			t.Errorf("Compare() differences = %d, want none:\n%s", len(report.Differences), reported(report))
		}
	})
}

// TestCompareExpectsZeroForASizeMemberThatIsNoNumber holds the fallback the
// comparison shares with the engine: a dimension named after a size member the
// interval states as text, or does not state at all, is billed at zero rather
// than reported. The expectation is the engine's own, so a comparison that read
// such a member as a difference would report every instance of the month for
// what the pricing model asked for.
func TestCompareExpectsZeroForASizeMemberThatIsNoNumber(t *testing.T) {
	oracle := oracleOf(t)
	model := parseModel(t, textModel)
	zero := decimal.Zero.StringFixed(money.QuantityPlaces)

	var rows [][]string
	for _, resource := range oracle.Resources {
		if resource.ResourceType != "instance" {
			continue
		}
		for _, interval := range resource.Intervals {
			for _, dimension := range []string{"flavor", "iops"} {
				rows = append(rows, ratedRowOf(oracle, resource, interval, dimension, zero))
			}
		}
	}

	report := compareRows(t, oracle, rows, textModel)
	if len(report.Differences) != 0 {
		t.Errorf("Compare() differences = %d, want none:\n%s", len(report.Differences), reported(report))
	}
	priced, _ := pricedResources(oracle, model)
	if report.Compared != priced {
		t.Errorf("Compare() compared = %d, want the %d instances of the oracle", report.Compared, priced)
	}
}

// TestCompareSkipsTheRecordsOfAnotherPlatform holds the platform half of the
// row filter. An export of a deployment that bills Harbor beside its cloud
// carries the records of another collector, and a comparison that let them
// through would refuse the whole month over a resource type the OpenStack
// prices say nothing about.
func TestCompareSkipsTheRecordsOfAnotherPlatform(t *testing.T) {
	oracle := oracleOf(t)
	model := parseModel(t, testModel)
	rows := ratedOf(t, oracle, model)
	volume := pickResource(t, oracle, "volume", 1, "")

	beside := copyRows(rows, rowsOf(volume.ResourceID), func(row []string) {
		row[columnPlatform] = "harbor"
		row[columnResourceType] = "project"
		row[columnDimension] = "storage_gb"
	})
	skipped := len(beside) - len(rows)

	report := compareRows(t, oracle, beside, testModel)
	if len(report.Differences) != 0 {
		t.Errorf("Compare() differences = %d, want none:\n%s", len(report.Differences), reported(report))
	}
	if report.Skipped != skipped {
		t.Errorf("Compare() skipped = %d, want the %d records of the other platform", report.Skipped, skipped)
	}
}

// TestCompareRefusesAnExportOfAnotherMonth refuses the export of a period the
// oracle says nothing about. Every resource of it would be missing from the
// one and unknown to the other, and a report of that says nothing about how
// either month was billed.
func TestCompareRefusesAnExportOfAnotherMonth(t *testing.T) {
	oracle := oracleOf(t)
	model := parseModel(t, testModel)
	june := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	rows := editRows(ratedOf(t, oracle, model), func([]string) bool { return true }, func(row []string) {
		row[columnPeriodFrom] = instantCell(june)
		row[columnPeriodTo] = instantCell(june.AddDate(0, 1, 0))
	})

	want := "rated.csv bills [2026-06-01T00:00:00Z, 2026-07-01T00:00:00Z) and " +
		"the oracle describes [2026-07-01T00:00:00Z, 2026-08-01T00:00:00Z)"
	_, err := runCompare(t, oracle, rows, testModel)
	if err == nil || err.Error() != want {
		t.Errorf("Compare() error = %v, want %q", err, want)
	}
}

// TestCompareRefusesAnExportWithoutTheCloud covers the two sides of the cloud
// filter: an export of a deployment that bills more than the simulated cloud
// is compared over the records of that cloud alone, and one that holds none of
// them at all is refused rather than reported as a month nothing was billed
// for.
func TestCompareRefusesAnExportWithoutTheCloud(t *testing.T) {
	oracle := oracleOf(t)
	model := parseModel(t, testModel)
	rows := ratedOf(t, oracle, model)
	instance := pickResource(t, oracle, "instance", 1, stateActive)

	t.Run("no record of the cloud", func(t *testing.T) {
		other := editRows(rows, func([]string) bool { return true }, func(row []string) {
			row[columnCloud] = "os-other"
		})
		want := "rated.csv holds no rated record of cloud os-test on platform openstack: " +
			"the run that wrote it did not bill this month"
		if _, err := runCompare(t, oracle, other, testModel); err == nil || err.Error() != want {
			t.Errorf("Compare() error = %v, want %q", err, want)
		}
	})

	t.Run("the records of another cloud beside them", func(t *testing.T) {
		beside := copyRows(rows, rowsOf(instance.ResourceID), func(row []string) {
			row[columnCloud] = "os-other"
		})
		skipped := len(beside) - len(rows)

		report := compareRows(t, oracle, beside, testModel)
		if len(report.Differences) != 0 {
			t.Errorf("Compare() differences = %d, want none:\n%s", len(report.Differences), reported(report))
		}
		if report.Skipped != skipped {
			t.Errorf("Compare() skipped = %d, want the %d records of the other cloud", report.Skipped, skipped)
		}
		line := fmt.Sprintf("skipped %d rated records of other clouds or platforms", skipped)
		if lines := report.Lines(); !slices.Contains(lines, line) {
			t.Errorf("Lines() = %v, want a line %q", lines, line)
		}
	})
}

// TestCompareReportsAMissingPricingModel names the file a comparison could not
// read the prices from. Without them nothing says which dimensions a resource
// type is billed by, so there is no report to write.
func TestCompareReportsAMissingPricingModel(t *testing.T) {
	oracle := oracleOf(t)
	rows := ratedOf(t, oracle, parseModel(t, testModel))

	t.Run("a file that is not there", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "pricing.yaml")
		_, err := Compare(CompareOptions{
			Oracle:  writeOracle(t, oracle),
			Export:  writeRated(t, rows),
			Pricing: path,
		})
		if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("reading the pricing model %s:", path)) {
			t.Fatalf("Compare() error = %v, want one naming %s", err, path)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("errors.Is(err, fs.ErrNotExist) = false, want true for %v", err)
		}
	})

	t.Run("a file that is no model", func(t *testing.T) {
		path := writeModel(t, "version: 1\n")
		prefix := fmt.Sprintf("reading the pricing model %s: ", path)
		_, err := Compare(CompareOptions{
			Oracle:  writeOracle(t, oracle),
			Export:  writeRated(t, rows),
			Pricing: path,
		})
		if err == nil || !strings.HasPrefix(err.Error(), prefix) || err.Error() == prefix {
			t.Errorf("Compare() error = %v, want one starting %q and saying what is wrong", err, prefix)
		}
	})
}

// TestReadRatedReportsAMissingFile names the file the export does not hold. A
// directory without rated.csv is the export of a run that wrote another format
// or none at all.
func TestReadRatedReportsAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rated.csv")

	_, err := readRated(path)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("readRated(%s) error = %v, want one naming the file", path, err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("errors.Is(err, fs.ErrNotExist) = false, want true for %v", err)
	}
}

// TestReadRatedReportsAMissingColumn names the column the header lacks. A
// table read past a missing column would compare the cells of another one.
func TestReadRatedReportsAMissingColumn(t *testing.T) {
	header := slices.DeleteFunc(slices.Clone(ratedTestHeader), func(name string) bool { return name == "from_ts" })
	row := slices.Clone(sampleRatedRow)
	path := writeCSV(t, [][]string{header, slices.Delete(row, columnFromTS, columnFromTS+1)})

	want := fmt.Sprintf("%s: the header lacks the column %q", path, "from_ts")
	if _, err := readRated(path); err == nil || err.Error() != want {
		t.Errorf("readRated() error = %v, want %q", err, want)
	}
}

// TestReadRatedReportsAFileWithoutAHeader refuses a file that names no column
// at all. An export always writes its header, so an empty file is a truncated
// one rather than a run that rated nothing.
func TestReadRatedReportsAFileWithoutAHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rated.csv")
	if err := os.WriteFile(path, nil, streamFileMode); err != nil {
		t.Fatalf("WriteFile(%s) error = %v, want nil", path, err)
	}

	want := fmt.Sprintf("%s holds no header", path)
	if _, err := readRated(path); err == nil || err.Error() != want {
		t.Errorf("readRated() error = %v, want %q", err, want)
	}
}

// TestReadRatedNamesTheBadLine holds the line number and the cell of a value
// that reads as nothing. An export runs to thousands of rows, and an error
// naming the value alone would leave whoever reads it searching for the row it
// came from.
func TestReadRatedNamesTheBadLine(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rows   [][]string
		prefix string
	}{
		{
			name: "a quantity on line 3",
			rows: [][]string{
				sampleRatedRow,
				editRows([][]string{sampleRatedRow}, func([]string) bool { return true },
					func(row []string) { row[columnQuantity] = "abc" })[0],
			},
			prefix: `line 3: quantity "abc": `,
		},
		{
			name: "an instant on line 4",
			rows: [][]string{
				sampleRatedRow,
				sampleRatedRow,
				editRows([][]string{sampleRatedRow}, func([]string) bool { return true },
					func(row []string) { row[columnFromTS] = "yesterday" })[0],
			},
			prefix: `line 4: from_ts "yesterday": `,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCSV(t, append([][]string{ratedTestHeader}, tc.rows...))
			prefix := path + ": " + tc.prefix

			_, err := readRated(path)
			if err == nil || !strings.HasPrefix(err.Error(), prefix) {
				t.Errorf("readRated() error = %v, want one starting %q", err, prefix)
			}
		})
	}
}

// TestReadRatedAcceptsAHeaderAlone reads the export of a run that rated
// nothing. The file is the table it is: a run that billed no resource writes
// its header and no row under it.
func TestReadRatedAcceptsAHeaderAlone(t *testing.T) {
	path := writeCSV(t, [][]string{ratedTestHeader})

	rows, err := readRated(path)
	if err != nil {
		t.Fatalf("readRated() error = %v, want nil", err)
	}
	if rows == nil || len(rows) != 0 {
		t.Errorf("readRated() rows = %v, want an empty table", rows)
	}
}

// TestReadRatedLocatesColumnsByName reads one row out of two files that spell
// the columns in opposite orders. A reader that took the positions of the
// header it knows would read every cell of the second file out of the wrong
// column.
func TestReadRatedLocatesColumnsByName(t *testing.T) {
	reversed := func(row []string) []string {
		flipped := slices.Clone(row)
		slices.Reverse(flipped)
		return flipped
	}
	want := ratedRow{
		Cloud:        testCloud,
		Platform:     platformOpenStack,
		ResourceType: "volume",
		ResourceID:   "1c38e4d1-42db-4271-bd32-9653e80a3603",
		ProjectID:    "3f4b0d2e-0a1f-4b6a-9a6d-0a3c1b2d4e5f",
		State:        "available",
		Dimension:    "size_gb",
		From:         time.Date(2026, 7, 4, 9, 0, 0, 0, time.UTC),
		To:           time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC),
		PeriodFrom:   july2026,
		PeriodTo:     july2026.AddDate(0, 1, 0),
		Quantity:     decimal.NewFromInt(50),
	}

	for _, tc := range []struct {
		name string
		rows [][]string
	}{
		{name: "the order the export writes", rows: [][]string{ratedTestHeader, sampleRatedRow}},
		{name: "the order reversed", rows: [][]string{reversed(ratedTestHeader), reversed(sampleRatedRow)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := readRated(writeCSV(t, tc.rows))
			if err != nil {
				t.Fatalf("readRated() error = %v, want nil", err)
			}
			if len(rows) != 1 {
				t.Fatalf("readRated() rows = %d, want one", len(rows))
			}
			if field := differs(rows[0], want); field != "" {
				t.Errorf("readRated() row = %+v, want %+v: %s differs", rows[0], want, field)
			}
		})
	}
}

// differs names the first field two rows disagree on, or the empty string.
// Instants and quantities are held against one another by value, so a row read
// in another offset or another spelling of the same number is the same row.
func differs(a, b ratedRow) string {
	switch {
	case a.Cloud != b.Cloud:
		return "Cloud"
	case a.Platform != b.Platform:
		return "Platform"
	case a.ResourceType != b.ResourceType:
		return "ResourceType"
	case a.ResourceID != b.ResourceID:
		return "ResourceID"
	case a.ProjectID != b.ProjectID:
		return "ProjectID"
	case a.State != b.State:
		return "State"
	case a.Dimension != b.Dimension:
		return "Dimension"
	case !a.From.Equal(b.From):
		return "From"
	case !a.To.Equal(b.To):
		return "To"
	case !a.PeriodFrom.Equal(b.PeriodFrom):
		return "PeriodFrom"
	case !a.PeriodTo.Equal(b.PeriodTo):
		return "PeriodTo"
	case !a.Quantity.Equal(b.Quantity):
		return "Quantity"
	}
	return ""
}

// TestCompareOptionsValidate names the first flag a comparison cannot run
// without, so an invocation missing several of them is told about the one to
// pass first rather than about its last.
func TestCompareOptionsValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts CompareOptions
		want string
	}{
		{name: "nothing at all", opts: CompareOptions{}, want: "--oracle: must be set"},
		{name: "the oracle alone", opts: CompareOptions{Oracle: "oracle.json"}, want: "--export: must be set"},
		{
			name: "the oracle and the export",
			opts: CompareOptions{Oracle: "oracle.json", Export: "export"},
			want: "--pricing: must be set",
		},
		{
			name: "all three",
			opts: CompareOptions{Oracle: "oracle.json", Export: "export", Pricing: "pricing.yaml"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.Validate()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Validate() error = %v, want nil", err)
			case tc.want != "" && (err == nil || err.Error() != tc.want):
				t.Errorf("Validate() error = %v, want %q", err, tc.want)
			}
		})
	}
}

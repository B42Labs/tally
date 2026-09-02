package simulator

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// testMetricsInterval is the grid the tests place their samples on. It is
// Ceilometer's own polling interval, which is what a month is pushed under.
const testMetricsInterval = 300 * time.Second

// The instants the profile is read at. July 1st 2026 is a Wednesday, so 10:00
// is an office hour, 06:00 a fringe hour and 02:00 a quiet one.
var (
	officeInstant = time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	fringeInstant = time.Date(2026, 7, 1, 6, 0, 0, 0, time.UTC)
	quietInstant  = time.Date(2026, 7, 1, 2, 0, 0, 0, time.UTC)
)

// testOracle is one hand-built oracle over the month of the api tests.
func testOracle(resources ...OracleResource) Oracle {
	return Oracle{
		Cloud:      testCloud,
		PeriodFrom: cloudFrom,
		PeriodTo:   cloudTo,
		Resources:  resources,
	}
}

// withWorkload names the workload of a hand-built resource. The fixtures of
// api_test.go leave it unset, because nothing a listing answers is decided by
// it, and the traffic level of an instance is read off it.
func withWorkload(resource OracleResource, workload string) OracleResource {
	resource.Workload = workload
	return resource
}

// spanInstance is one instance that ran through the states it is given, each of
// them four hours long from the first instant.
func spanInstance(id, workload string, from time.Time, states ...string) OracleResource {
	intervals := make([]OracleInterval, 0, len(states))
	for _, state := range states {
		to := from.Add(4 * time.Hour)
		intervals = append(intervals, OracleInterval{
			From: from, To: to, State: state, ProjectID: cloudTenant, Size: instanceSizeOf(largeFlavor),
		})
		from = to
	}
	return OracleResource{
		ResourceType: typeInstance, ResourceID: id, Workload: workload, Intervals: intervals,
	}
}

// trafficOf places the traffic of an oracle or fails the test.
func trafficOf(t *testing.T, oracle Oracle, seed uint64, interval time.Duration,
) ([]Sample, []OracleTraffic) {
	t.Helper()

	samples, rows, err := TrafficOf(oracle, seed, interval)
	if err != nil {
		t.Fatalf("TrafficOf(oracle, %d, %s) error = %v, want nil", seed, interval, err)
	}
	return samples, rows
}

// sampleOrder is the order TrafficOf states its samples in: by instant, then by
// name, then by the resource the sample is about.
func sampleOrder(a, b Sample) int {
	if c := a.At.Compare(b.At); c != 0 {
		return c
	}
	if c := strings.Compare(a.Name, b.Name); c != 0 {
		return c
	}
	return strings.Compare(a.Labels["resource_id"], b.Labels["resource_id"])
}

// seriesOf picks the samples of one series about one resource, in the order
// they were stated in.
func seriesOf(samples []Sample, name, resourceID string) []Sample {
	picked := make([]Sample, 0)
	for _, sample := range samples {
		if sample.Name == name && sample.Labels["resource_id"] == resourceID {
			picked = append(picked, sample)
		}
	}
	return picked
}

// namedSamples picks every sample of one series.
func namedSamples(samples []Sample, name string) []Sample {
	picked := make([]Sample, 0)
	for _, sample := range samples {
		if sample.Name == name {
			picked = append(picked, sample)
		}
	}
	return picked
}

// instancesOf is every instance the oracle holds.
func instancesOf(oracle Oracle) []OracleResource {
	held := make([]OracleResource, 0)
	for _, resource := range oracle.Resources {
		if resource.ResourceType == typeInstance {
			held = append(held, resource)
		}
	}
	return held
}

// labelKeys is the sorted list of the labels a sample carries, which is what a
// series is held against the exporter's own spelling by.
func labelKeys(sample Sample) []string {
	keys := make([]string, 0, len(sample.Labels))
	for key := range sample.Labels {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func TestTrafficLiesOnTheGrid(t *testing.T) {
	month := faultyMonth(t, 1, Faults{})
	samples, _, err := TrafficOf(month.Oracle, 1, testMetricsInterval)
	if err != nil {
		t.Fatalf("TrafficOf() error = %v, want nil", err)
	}
	if len(samples) == 0 {
		t.Fatal("TrafficOf() placed no sample, want the counters of a month of instances")
	}

	for _, sample := range samples {
		if offset := sample.At.Sub(month.Oracle.PeriodFrom); offset%testMetricsInterval != 0 {
			t.Fatalf("%s of %s lies %s past the start of the month, which is no whole step of %s",
				sample.Name, sample.Labels["resource_id"], offset, testMetricsInterval)
		}
	}

	for _, instance := range instancesOf(month.Oracle) {
		egress := len(seriesOf(samples, egressSeries, instance.ResourceID))
		ingress := len(seriesOf(samples, ingressSeries, instance.ResourceID))
		if egress != ingress {
			t.Errorf("instance %s carries %d outgoing and %d incoming samples, want one of each per step",
				instance.ResourceID, egress, ingress)
		}
		// An instance that lived and died between two grid instants is polled at
		// none of them, the way Ceilometer never sees a runner that was gone
		// before its next poll.
		first := gridStepAtOrAfter(month.Oracle.PeriodFrom, instance.Intervals[0].From, testMetricsInterval)
		polled := first.Before(instance.Intervals[len(instance.Intervals)-1].To)
		if polled != (egress > 0) {
			t.Errorf("instance %s over %s..%s carries %d samples, want polled = %v",
				instance.ResourceID, instance.Intervals[0].From,
				instance.Intervals[len(instance.Intervals)-1].To, egress, polled)
		}
	}

	for i := 1; i < len(samples); i++ {
		if sampleOrder(samples[i-1], samples[i]) >= 0 {
			t.Fatalf("sample %d (%s of %s at %s) does not follow sample %d (%s of %s at %s)",
				i, samples[i].Name, samples[i].Labels["resource_id"], samples[i].At,
				i-1, samples[i-1].Name, samples[i-1].Labels["resource_id"], samples[i-1].At)
		}
	}
}

func TestTrafficCountersNeverDecreaseAndStartAtZero(t *testing.T) {
	month := faultyMonth(t, 2, Faults{})
	samples, _ := trafficOf(t, month.Oracle, 2, testMetricsInterval)

	for _, instance := range instancesOf(month.Oracle) {
		for _, name := range []string{egressSeries, ingressSeries} {
			series := seriesOf(samples, name, instance.ResourceID)
			if len(series) == 0 {
				continue
			}
			want := gridStepAtOrAfter(month.Oracle.PeriodFrom, instance.Intervals[0].From, testMetricsInterval)
			if !series[0].At.Equal(want) {
				t.Errorf("%s of %s begins at %s, want the first grid step at or after %s, which is %s",
					name, instance.ResourceID, series[0].At, instance.Intervals[0].From, want)
			}
			if series[0].Value != 0 {
				t.Errorf("%s of %s begins at %d bytes, want a counter that starts at 0",
					name, instance.ResourceID, series[0].Value)
			}
			for i := 1; i < len(series); i++ {
				if series[i].Value < series[i-1].Value {
					t.Fatalf("%s of %s falls from %d to %d at %s, want a counter that never decreases",
						name, instance.ResourceID, series[i-1].Value, series[i].Value, series[i].At)
				}
			}
		}
	}
}

func TestTrafficAccruesOnlyWhileActive(t *testing.T) {
	instance := spanInstance("srv-states", workloadClassic, cloudDay(1).Add(8*time.Hour),
		stateActive, stateShutoff, stateShelved, stateResized, stateActive)
	samples, _ := trafficOf(t, testOracle(instance), 7, testMetricsInterval)

	series := seriesOf(samples, egressSeries, instance.ResourceID)
	if len(series) < len(instance.Intervals) {
		t.Fatalf("the instance carries %d samples over %d intervals, want at least one per interval",
			len(series), len(instance.Intervals))
	}

	// The value of a sample is what the steps before it accrued, so the step
	// that begins at one sample is the increment to the next.
	for i := 1; i < len(series); i++ {
		increment := series[i].Value - series[i-1].Value
		state := ""
		for _, interval := range instance.Intervals {
			if !series[i-1].At.Before(interval.From) && series[i-1].At.Before(interval.To) {
				state = interval.State
			}
		}
		switch state {
		case stateActive:
			if increment <= 0 {
				t.Errorf("the step at %s ran active and accrued %d bytes, want a positive count",
					series[i-1].At, increment)
			}
		case "":
			t.Fatalf("the step at %s lies in no interval of the instance", series[i-1].At)
		default:
			if increment != 0 {
				t.Errorf("the step at %s was %s and accrued %d bytes, want nothing while the instance moves nothing",
					series[i-1].At, state, increment)
			}
		}
	}
}

func TestTrafficLevelsFollowTheWorkloadAndTheProfile(t *testing.T) {
	// 75 over 100 divides every level of the table exactly, so the office steps
	// below are the quotients they state rather than rounded ones.
	const jitter = 75
	stepSeconds := int64(testMetricsInterval / time.Second)

	classic := trafficLevels[workloadClassic]
	gardener := trafficLevels[workloadGardener]

	classicOffice := stepBytes(classic.egress, officeInstant, stepSeconds, jitter)
	gardenerOffice := stepBytes(gardener.egress, officeInstant, stepSeconds, jitter)
	if classicOffice <= 0 {
		t.Fatalf("stepBytes(%d, office) = %d, want a positive count", classic.egress, classicOffice)
	}
	if gardenerOffice != 4*classicOffice {
		t.Errorf("the gardener office step is %d and the classic one %d, want four times as much",
			gardenerOffice, classicOffice)
	}

	profile := []struct {
		name string
		at   time.Time
		want int64
	}{
		{"an office hour of a working day", officeInstant, classicOffice},
		// The hours weigh 10, 3 and 1 against each other and the one division
		// comes last, so a quiet step is a tenth of the office step and a fringe
		// step three tenths of it.
		{"a quiet hour", quietInstant, classicOffice / 10},
		{"a fringe hour", fringeInstant, 3 * classicOffice / 10},
	}
	for _, want := range profile {
		t.Run(want.name, func(t *testing.T) {
			got := stepBytes(classic.egress, want.at, stepSeconds, jitter)
			if got != want.want {
				t.Errorf("stepBytes(classic, %s) = %d, want %d", want.at, got, want.want)
			}
		})
	}

	fringe := stepBytes(classic.egress, fringeInstant, stepSeconds, jitter)
	quiet := stepBytes(classic.egress, quietInstant, stepSeconds, jitter)
	if fringe <= quiet || fringe >= classicOffice {
		t.Errorf("the fringe step is %d, want it between the quiet step %d and the office step %d",
			fringe, quiet, classicOffice)
	}

	levels := []struct {
		workload string
		times    int64
		divided  int64
	}{
		{workloadClassic, 1, 4},
		{workloadGardener, 1, 2},
		{workloadCI, 2, 1},
	}
	for _, level := range levels {
		t.Run(level.workload+" ingress", func(t *testing.T) {
			held := trafficLevels[level.workload]
			if held.ingress*level.divided != held.egress*level.times {
				t.Errorf("the %s level receives %d and sends %d, want %d/%d of the egress",
					level.workload, held.ingress, held.egress, level.times, level.divided)
			}
		})
	}
}

func TestTrafficHoldsTheLongestInterval(t *testing.T) {
	// A whole day of office seconds at the highest level and the largest jitter
	// is the biggest product the arithmetic has to carry. Its numerator is about
	// 1.11e18, and one that had overflowed an int64 would come back negative.
	stepSeconds := int64(maxMetricsInterval / time.Second)
	level := trafficLevels[workloadGardener]

	numerator := level.egress * officeWeight * stepSeconds * jitterMax
	if numerator <= 0 {
		t.Fatalf("the numerator of a step of %s is %d, want it inside an int64",
			maxMetricsInterval, numerator)
	}

	// 8589934592 bytes an office hour over 86400 seconds at 150 over 100.
	const want = 309237645312
	if got := stepBytes(level.egress, officeInstant, stepSeconds, jitterMax); got != want {
		t.Errorf("stepBytes over a step of %s = %d, want %d", maxMetricsInterval, got, want)
	}
}

func TestTrafficStopsAtTheInstancesEnd(t *testing.T) {
	month := faultyMonth(t, 3, Faults{})
	samples, _ := trafficOf(t, month.Oracle, 3, testMetricsInterval)
	steps := int(month.Oracle.PeriodTo.Sub(month.Oracle.PeriodFrom) / testMetricsInterval)

	deleted := 0
	for _, instance := range instancesOf(month.Oracle) {
		end := instance.Intervals[len(instance.Intervals)-1].To
		series := seriesOf(samples, egressSeries, instance.ResourceID)
		for _, sample := range series {
			if !sample.At.Before(end) {
				t.Fatalf("instance %s ended at %s and is still sampled at %s, want no sample past its end",
					instance.ResourceID, end, sample.At)
			}
		}
		if !end.Before(month.Oracle.PeriodTo) {
			continue
		}
		deleted++
		if len(series) >= steps {
			t.Errorf("instance %s was deleted at %s and carries %d samples, want fewer than the %d of the month",
				instance.ResourceID, end, len(series), steps)
		}
	}
	if deleted == 0 {
		t.Fatal("the month deleted no instance, so nothing states that a deleted one stops being polled")
	}
}

func TestTrafficIsDeterministic(t *testing.T) {
	month := faultyMonth(t, 4, Faults{})

	before := renderedStream(t, "before.jsonl", month.Stream)
	samples, rows := trafficOf(t, month.Oracle, 4, testMetricsInterval)
	again, rowsAgain := trafficOf(t, month.Oracle, 4, testMetricsInterval)
	after := renderedStream(t, "after.jsonl", month.Stream)

	if !reflect.DeepEqual(samples, again) {
		t.Error("two runs of TrafficOf over one oracle placed two different sets of samples")
	}
	if !reflect.DeepEqual(rows, rowsAgain) {
		t.Error("two runs of TrafficOf over one oracle recorded two different sets of rows")
	}

	// The jitter is drawn from a stream of its own, so the notifications of the
	// month read the same whether the traffic was placed or not.
	if !reflect.DeepEqual(before, after) {
		t.Error("the notifications of the month changed while the traffic was placed")
	}
	plain := faultyMonth(t, 4, Faults{})
	if !reflect.DeepEqual(before, renderedStream(t, "plain.jsonl", plain.Stream)) {
		t.Error("a second month of the same seed renders other notifications than the one the traffic was placed for")
	}

	moved, _ := trafficOf(t, month.Oracle, 5, testMetricsInterval)
	if len(moved) != len(samples) {
		t.Fatalf("the seed changed the sample count from %d to %d, want the grid to hold",
			len(samples), len(moved))
	}
	if reflect.DeepEqual(samples, moved) {
		t.Error("another seed placed the very same bytes, want the jitter to move with it")
	}
}

// renderedStream writes one stream to a file of the test's own directory and
// reads it back, which is how a run states what it published.
func renderedStream(t *testing.T, name string, schedule Schedule) []byte {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := WriteStream(path, schedule); err != nil {
		t.Fatalf("WriteStream(%s) error = %v, want nil", name, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s error = %v, want nil", name, err)
	}
	return body
}

func TestTrafficOfAnEmptyOracle(t *testing.T) {
	cases := []struct {
		name   string
		oracle Oracle
	}{
		{"an oracle with no resource at all", testOracle()},
		{"an oracle that holds no instance", testOracle(oneResource(typeVolume, "vol-1",
			cloudDay(2), cloudDay(3), stateAvailable, volumeSizeOf(&volume{sizeGB: 50, volumeType: "ssd"})))},
	}
	for _, held := range cases {
		t.Run(held.name, func(t *testing.T) {
			samples, rows, err := TrafficOf(held.oracle, 1, testMetricsInterval)
			if err != nil {
				t.Fatalf("TrafficOf() error = %v, want nil", err)
			}
			if samples == nil || len(samples) != 0 {
				t.Errorf("TrafficOf() samples = %v, want an empty slice that is not nil", samples)
			}
			if rows == nil || len(rows) != 0 {
				t.Errorf("TrafficOf() rows = %v, want an empty slice that is not nil", rows)
			}
		})
	}
}

func TestTrafficRefusesAnUnknownWorkload(t *testing.T) {
	instance := withWorkload(oneInstance("srv-batch", cloudDay(2), cloudDay(3)), "batch")

	samples, rows, err := TrafficOf(testOracle(instance), 1, testMetricsInterval)
	if err == nil {
		t.Fatalf("TrafficOf() over an unknown workload error = nil, want a refusal; it placed %d samples",
			len(samples))
	}
	if rows != nil {
		t.Errorf("TrafficOf() recorded %d rows beside its error, want none", len(rows))
	}
	for _, want := range []string{"srv-batch", `"batch"`, workloadGardener} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("TrafficOf() error = %q, want it to name %s", err, want)
		}
	}
}

func TestTrafficRowsStateEveryInterval(t *testing.T) {
	// The grid runs from midnight in whole steps of five minutes, so the middle
	// interval below begins a minute past a step and ends before the next one:
	// it holds no step at all.
	day := cloudDay(2)
	brief := OracleResource{
		ResourceType: typeInstance, ResourceID: "srv-a", Workload: workloadClassic,
		Intervals: []OracleInterval{
			{
				From: day.Add(8 * time.Hour), To: day.Add(10*time.Hour + time.Minute),
				State: stateActive, ProjectID: cloudTenant, Size: instanceSizeOf(largeFlavor),
			},
			{
				From: day.Add(10*time.Hour + time.Minute), To: day.Add(10*time.Hour + 2*time.Minute),
				State: stateActive, ProjectID: cloudTenant, Size: instanceSizeOf(flavors[0]),
			},
			{
				From: day.Add(10*time.Hour + 2*time.Minute), To: day.Add(14 * time.Hour),
				State: stateActive, ProjectID: cloudTenant, Size: instanceSizeOf(largeFlavor),
			},
		},
	}
	resized := withWorkload(resizedInstance("srv-b", day.Add(9*time.Hour),
		day.Add(11*time.Hour), day.Add(13*time.Hour)), workloadGardener)

	oracle := testOracle(brief, resized)
	samples, rows := trafficOf(t, oracle, 11, testMetricsInterval)

	wantRows := 0
	for _, instance := range instancesOf(oracle) {
		wantRows += len(instance.Intervals)
	}
	if len(rows) != wantRows {
		t.Fatalf("TrafficOf() recorded %d rows over %d intervals, want one row per interval",
			len(rows), wantRows)
	}
	for i := 1; i < len(rows); i++ {
		if c := strings.Compare(rows[i-1].ResourceID, rows[i].ResourceID); c > 0 ||
			(c == 0 && !rows[i-1].From.Before(rows[i].From)) {
			t.Fatalf("row %d (%s from %s) does not follow row %d (%s from %s), want them by id and then by start",
				i, rows[i].ResourceID, rows[i].From, i-1, rows[i-1].ResourceID, rows[i-1].From)
		}
	}

	if rows[1].EgressBytes != 0 || rows[1].IngressBytes != 0 {
		t.Errorf("the interval of one minute between two steps carries %d and %d bytes, want none",
			rows[1].EgressBytes, rows[1].IngressBytes)
	}

	// The jitter is drawn once per instance, in oracle order, so the first draw
	// of a fresh stream belongs to the first instance.
	stream := metricsStream(11)
	for _, instance := range instancesOf(oracle) {
		drawn := jitterMin + stream.Int64N(jitterMax-jitterMin+1)
		level := trafficLevels[instance.Workload]
		steps := instanceSteps(instance, oracle.PeriodFrom, testMetricsInterval, level, drawn)

		var egress, ingress int64
		for i, interval := range instance.Intervals {
			var placed int64
			for _, step := range steps {
				if step.at.Before(interval.From) || !step.at.Before(interval.To) {
					continue
				}
				placed += step.egress
			}
			row := rows[slices.IndexFunc(rows, func(held OracleTraffic) bool {
				return held.ResourceID == instance.ResourceID && held.From.Equal(interval.From)
			})]
			if row.EgressBytes != placed {
				t.Errorf("row %d of %s states %d bytes, want the %d its steps placed",
					i, instance.ResourceID, row.EgressBytes, placed)
			}
			egress += row.EgressBytes
			ingress += row.IngressBytes
		}

		// The last step of an instance is placed in its last row and observed by
		// no sample, the way Ceilometer's last poll precedes a delete.
		last := steps[len(steps)-1]
		series := seriesOf(samples, egressSeries, instance.ResourceID)
		if got := series[len(series)-1].Value + last.egress; got != egress {
			t.Errorf("the rows of %s sum to %d, want the last sample %d plus its last step %d",
				instance.ResourceID, egress, series[len(series)-1].Value, last.egress)
		}
		incoming := seriesOf(samples, ingressSeries, instance.ResourceID)
		if got := incoming[len(incoming)-1].Value + last.ingress; got != ingress {
			t.Errorf("the rows of %s sum to %d incoming bytes, want %d",
				instance.ResourceID, ingress, got)
		}
	}
}

func TestTrafficFollowsTheInterval(t *testing.T) {
	month := faultyMonth(t, 6, Faults{})
	fine, _ := trafficOf(t, month.Oracle, 6, testMetricsInterval)
	coarse, _ := trafficOf(t, month.Oracle, 6, 2*testMetricsInterval)

	for _, sample := range coarse {
		if offset := sample.At.Sub(month.Oracle.PeriodFrom); offset%(2*testMetricsInterval) != 0 {
			t.Fatalf("%s of %s lies %s past the start of the month, which is no whole step of the coarser grid",
				sample.Name, sample.Labels["resource_id"], offset)
		}
	}

	for _, instance := range instancesOf(month.Oracle) {
		dense := len(seriesOf(fine, egressSeries, instance.ResourceID))
		sparse := len(seriesOf(coarse, egressSeries, instance.ResourceID))
		// A span holds half as many steps of twice the length, give or take the
		// one step its two ends fall inside.
		if sparse < dense/2-1 || sparse > dense/2+1 {
			t.Errorf("instance %s carries %d samples on the coarse grid and %d on the fine one, "+
				"want about half as many", instance.ResourceID, sparse, dense)
		}
	}
}

func TestInventoryBeforeTheMonthHoldsNothing(t *testing.T) {
	month := faultyMonth(t, 1, Faults{})
	samples := InventoryAt(month, month.Oracle.PeriodFrom.Add(-time.Hour))

	empty := []string{
		seriesNovaTotalVMs, seriesCinderVolumes, seriesNeutronFloatingIPs,
		seriesGlanceImages, seriesLoadBalancerTotal,
	}
	for _, name := range empty {
		held := namedSamples(samples, name)
		if len(held) != 1 {
			t.Fatalf("%s is stated %d times, want once", name, len(held))
		}
		if held[0].Value != 0 {
			t.Errorf("%s = %d before the month began, want 0", name, held[0].Value)
		}
	}

	silent := []string{
		seriesNovaServerStatus, seriesCinderVolumeStatus, seriesCinderVolumeGB,
		seriesGlanceImageBytes, seriesNeutronFloatingIP, seriesNeutronRouter,
		seriesLoadBalancerStatus,
	}
	for _, name := range silent {
		if held := namedSamples(samples, name); len(held) != 0 {
			t.Errorf("%s is stated %d times before the month began, want no resource at all",
				name, len(held))
		}
	}

	limits := map[string]int64{
		seriesLimitsInstancesUsed: 0, seriesLimitsInstancesMax: limitInstancesMax,
		seriesLimitsVCPUsUsed: 0, seriesLimitsVCPUsMax: limitVCPUsMax,
		seriesLimitsMemoryUsed: 0, seriesLimitsMemoryMax: limitMemoryMaxMB,
	}
	for name, want := range limits {
		held := namedSamples(samples, name)
		if len(held) != len(month.Tenants) {
			t.Fatalf("%s is stated %d times, want one per tenant, of which there are %d",
				name, len(held), len(month.Tenants))
		}
		for _, sample := range held {
			if sample.Value != want {
				t.Errorf("%s of %s = %d, want %d",
					name, sample.Labels["tenant_id"], sample.Value, want)
			}
		}
	}
}

func TestInventoryIsHeldAtTheEndOfTheMonth(t *testing.T) {
	month := faultyMonth(t, 1, Faults{})
	end := InventoryAt(month, month.Oracle.PeriodTo)
	past := InventoryAt(month, month.Oracle.PeriodTo.AddDate(0, 0, 1))

	if !reflect.DeepEqual(end, past) {
		t.Error("a day past the month reports another world than its last instant, want the end held")
	}

	outlived := make(map[string]bool)
	for _, instance := range instancesOf(month.Oracle) {
		if !instance.Intervals[len(instance.Intervals)-1].To.Before(month.Oracle.PeriodTo) {
			outlived[instance.ResourceID] = true
		}
	}
	if len(outlived) == 0 {
		t.Fatal("no instance outlived the month, so nothing states what its last instant holds")
	}

	served := namedSamples(past, seriesNovaServerStatus)
	if len(served) != len(outlived) {
		t.Fatalf("the end of the month serves %d servers, want the %d that outlived it",
			len(served), len(outlived))
	}
	for _, sample := range served {
		if !outlived[sample.Labels["id"]] {
			t.Errorf("server %s is served at the end of the month, want only the ones that outlived it",
				sample.Labels["id"])
		}
	}
}

func TestInventoryFoldsTheRoutersOutOfTheSchedule(t *testing.T) {
	month := faultyMonth(t, 1, Faults{})
	workloads := make(map[string]string, len(month.Tenants))
	for _, tenant := range month.Tenants {
		workloads[tenant.ID] = tenant.Workload
	}

	counted := make(map[string]int)
	for _, sample := range namedSamples(InventoryAt(month, july2026.AddDate(0, 0, 9)), seriesNeutronRouter) {
		if sample.Value != 1 {
			t.Errorf("router %s = %d, want 1", sample.Labels["id"], sample.Value)
		}
		counted[workloads[sample.Labels["project_id"]]]++
	}

	if counted[workloadCI] != 1 {
		t.Errorf("the CI tenant holds %d routers, want the one its network was built with",
			counted[workloadCI])
	}
	if counted[workloadGardener] == 0 {
		t.Error("no shoot holds a router, want one per shoot alive on the tenth day")
	}
	// The classic tenants' networks pre-exist the month, so neutron announced no
	// router of theirs and the fold finds none.
	if counted[workloadClassic] != 0 {
		t.Errorf("the classic tenants hold %d routers, want none", counted[workloadClassic])
	}
}

func TestInventoryDropsADeletedRouter(t *testing.T) {
	month := Month{
		Schedule: Schedule{
			{At: cloudDay(2), EventType: routerCreateType, ResourceID: "rtr-1", ProjectID: cloudTenant},
			{At: cloudDay(3), EventType: routerCreateType, ResourceID: "rtr-2", ProjectID: cloudTenant},
			{At: cloudDay(4), EventType: routerDeleteType, ResourceID: "rtr-1", ProjectID: cloudTenant},
		},
		Oracle: testOracle(),
	}

	standing := func(at time.Time) []string {
		held := make([]string, 0)
		for _, sample := range namedSamples(InventoryAt(month, at), seriesNeutronRouter) {
			held = append(held, sample.Labels["id"])
		}
		return held
	}
	if got, want := standing(cloudDay(3)), []string{"rtr-1", "rtr-2"}; !slices.Equal(got, want) {
		t.Errorf("the routers of the third day are %v, want %v", got, want)
	}
	// The gauge of a torn-down router is what a panel would otherwise hold at 1
	// for the rest of the month.
	if got, want := standing(cloudDay(5)), []string{"rtr-2"}; !slices.Equal(got, want) {
		t.Errorf("the routers of the fifth day are %v, want %v: the deleted one is gone", got, want)
	}
}

// TestTheRouterFoldWalksForwardLikeSingleReadings holds the fold a run carries
// through a month against the one a single reading builds. The push hands one
// fold to every grid step of the month and the scrape builds a fresh one per
// request, so a cursor that walked past a transition would leave the pushed
// world stating routers the scraped one never states.
func TestTheRouterFoldWalksForwardLikeSingleReadings(t *testing.T) {
	schedule := make(Schedule, 0)
	for day := 1; day <= 12; day++ {
		schedule = append(schedule,
			Transition{
				At: cloudDay(day), EventType: routerCreateType,
				ResourceID: fmt.Sprintf("rtr-%d", day), ProjectID: cloudTenant,
			},
			// A transition of another family at the same instant: the fold walks
			// over it and has to leave the cursor on the one behind it.
			Transition{
				At: cloudDay(day), EventType: imageCreateType,
				ResourceID: fmt.Sprintf("img-%d", day), ProjectID: cloudTenant,
			})
		if day%3 == 0 {
			schedule = append(schedule, Transition{
				At: cloudDay(day), EventType: routerDeleteType,
				ResourceID: fmt.Sprintf("rtr-%d", day-1), ProjectID: cloudTenant,
			})
		}
	}

	walked := &routerFold{schedule: schedule}
	for day := 1; day <= 12; day++ {
		at := cloudDay(day)
		got, want := walked.at(at), (&routerFold{schedule: schedule}).at(at)
		if !slices.Equal(got, want) {
			t.Fatalf("the fold walked to day %d holds %v, want the %v a single reading holds",
				day, got, want)
		}
		if len(got) == 0 {
			t.Fatalf("day %d holds no router at all, want the ones created up to it", day)
		}
	}
}

func TestInventoryCarriesNoScrapeLabels(t *testing.T) {
	month := faultyMonth(t, 1, Faults{})

	for _, sample := range InventoryAt(month, cloudDay(10)) {
		for _, forbidden := range []string{"platform", "cloud"} {
			if _, held := sample.Labels[forbidden]; held {
				t.Fatalf("%s carries a %s label, want it from the scrape job alone",
					sample.Name, forbidden)
			}
		}
	}
}

func TestInventoryReportsMemoryInMegabytes(t *testing.T) {
	at := cloudDay(3)
	month := Month{
		Tenants: []Tenant{{ID: cloudTenant, Name: "tenant-a", Workload: workloadClassic}},
		Oracle: testOracle(
			oneInstance("srv-1", cloudDay(2), cloudDay(4)),
			oneInstance("srv-2", cloudDay(2), cloudDay(4)),
		),
	}
	samples := InventoryAt(month, at)

	want := map[string]int64{
		seriesLimitsInstancesUsed: 2,
		seriesLimitsInstancesMax:  limitInstancesMax,
		seriesLimitsVCPUsUsed:     2 * int64(largeFlavor.vcpus),
		seriesLimitsVCPUsMax:      limitVCPUsMax,
		// Nova reports megabytes, and the size states the flavor's memory in
		// gibibytes, so the limit is the summed ram_gb times 1024.
		seriesLimitsMemoryUsed: 2 * int64(largeFlavor.memoryMB),
		seriesLimitsMemoryMax:  limitMemoryMaxMB,
	}
	for name, value := range want {
		held := namedSamples(samples, name)
		if len(held) != 1 {
			t.Fatalf("%s is stated %d times, want once for the one tenant", name, len(held))
		}
		if held[0].Value != value {
			t.Errorf("%s = %d, want %d", name, held[0].Value, value)
		}
		if held[0].Labels["tenant_id"] != cloudTenant {
			t.Errorf("%s names the tenant %q, want %q", name, held[0].Labels["tenant_id"], cloudTenant)
		}
	}
}

func TestInventoryToleratesAMalformedSize(t *testing.T) {
	at := cloudDay(3)
	month := Month{
		Tenants: []Tenant{{ID: cloudTenant, Name: "tenant-a", Workload: workloadClassic}},
		Oracle: testOracle(
			oneResource(typeInstance, "srv-1", cloudDay(2), cloudDay(4), stateActive, map[string]any{}),
			oneResource(typeVolume, "vol-1", cloudDay(2), cloudDay(4), stateInUse,
				map[string]any{"type": "ssd"}),
		),
	}
	samples := InventoryAt(month, at)

	servers := namedSamples(samples, seriesNovaServerStatus)
	if len(servers) != 1 {
		t.Fatalf("the server is stated %d times, want once even without a flavor", len(servers))
	}
	if servers[0].Labels["flavor_id"] != "" {
		t.Errorf("the server names the flavor %q, want none for a size that states no flavor",
			servers[0].Labels["flavor_id"])
	}

	zero := []string{seriesLimitsVCPUsUsed, seriesLimitsMemoryUsed, seriesCinderVolumeGB}
	for _, name := range zero {
		held := namedSamples(samples, name)
		if len(held) != 1 {
			t.Fatalf("%s is stated %d times, want once", name, len(held))
		}
		if held[0].Value != 0 {
			t.Errorf("%s = %d, want a size member nobody stated to count as 0", name, held[0].Value)
		}
	}
}

func TestInventoryNamesTheExportersLabels(t *testing.T) {
	at := cloudDay(3)
	month := Month{
		Tenants: []Tenant{{ID: cloudTenant, Name: "tenant-a", Workload: workloadClassic}},
		Schedule: Schedule{{
			At: cloudDay(2), EventType: routerCreateType, ResourceID: "rtr-1", ProjectID: cloudTenant,
		}},
		Oracle: testOracle(
			oneResource(typeInstance, "srv-1", cloudDay(2), cloudDay(4), stateShutoff,
				instanceSizeOf(largeFlavor)),
			oneResource(typeVolume, "vol-1", cloudDay(2), cloudDay(4), stateInUse,
				volumeSizeOf(&volume{sizeGB: 100, volumeType: "ssd"})),
			oneResource(typeImage, "img-1", cloudDay(2), cloudDay(4), stateActive,
				imageSizeOf(&image{size: quarterGiB})),
			oneResource(typeFloatingIP, "fip-1", cloudDay(2), cloudDay(4), stateActive,
				floatingIPSizeOf()),
			oneResource(typeLoadBalancer, "lb-1", cloudDay(2), cloudDay(4), stateActive,
				loadBalancerSizeOf(2, 1)),
		),
	}
	samples := InventoryAt(month, at)

	families := []struct {
		name   string
		labels []string
	}{
		{seriesNovaServerStatus, []string{"flavor_id", "id", "name", "status", "tenant_id", "uuid"}},
		{seriesCinderVolumeStatus, []string{"id", "name", "status", "tenant_id", "volume_type"}},
		{seriesCinderVolumeGB, []string{"id", "name", "tenant_id", "volume_type"}},
		{seriesGlanceImageBytes, []string{"id", "name", "tenant_id"}},
		{seriesNeutronFloatingIP, []string{"id", "project_id", "status"}},
		{seriesNeutronRouter, []string{"id", "project_id", "status"}},
		{
			seriesLoadBalancerStatus,
			[]string{"id", "name", "operating_status", "project_id", "provisioning_status"},
		},
		{seriesLimitsInstancesUsed, []string{"tenant_id"}},
		{seriesIdentityProjectInfo, []string{"domain_id", "enabled", "id", "name"}},
		{seriesIdentityProjects, nil},
		{seriesNovaTotalVMs, nil},
	}
	for _, family := range families {
		t.Run(family.name, func(t *testing.T) {
			held := namedSamples(samples, family.name)
			if len(held) != 1 {
				t.Fatalf("%s is stated %d times, want once", family.name, len(held))
			}
			if got := labelKeys(held[0]); !slices.Equal(got, family.labels) {
				t.Errorf("%s carries the labels %v, want %v", family.name, got, family.labels)
			}
		})
	}

	for _, sample := range samples {
		if sample.Kind != KindGauge {
			t.Errorf("%s is stated as kind %d, want every inventory series as a gauge",
				sample.Name, sample.Kind)
		}
		if !sample.At.Equal(at) {
			t.Errorf("%s is stated at %s, want the instant the world was read at, %s",
				sample.Name, sample.At, at)
		}
	}

	// The status is what nova reports, not what the collector books: the oracle
	// says shutoff and nova says stopped.
	if got := namedSamples(samples, seriesNovaServerStatus)[0].Labels["status"]; got != "stopped" {
		t.Errorf("the server is served as %q, want nova's own word for a shutoff instance", got)
	}
	if got := namedSamples(samples, seriesGlanceImageBytes)[0].Value; got != quarterGiB {
		t.Errorf("the image is stated as %d bytes, want %d", got, quarterGiB)
	}
	info := namedSamples(samples, seriesIdentityProjectInfo)[0]
	if info.Labels["domain_id"] != "default" || info.Labels["enabled"] != "true" {
		t.Errorf("the project is stated in domain %q and enabled %q, want default and true",
			info.Labels["domain_id"], info.Labels["enabled"])
	}
	if got := namedSamples(samples, seriesIdentityProjects)[0].Value; got != int64(len(month.Tenants)) {
		t.Errorf("%s = %d, want the %d tenants of the month",
			seriesIdentityProjects, got, len(month.Tenants))
	}
}

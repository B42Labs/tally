package simulator

import (
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
)

// The metrics of a month are the second face of the simulated cloud. The
// notifications say what the cloud did, and these say what a monitoring stack
// would have seen while it did it: the network counters Ceilometer polls off
// every instance, and the inventory gauges the OpenStack exporter scrapes off
// the services.
//
// Both faces are placed here rather than inside the pusher and the endpoint
// that carry them. One function per face means a pushed month and a scraped
// live view state one world instead of two descriptions of it, and neither of
// them reads a code path of the other.

// SampleKind tells a gauge from a counter.
type SampleKind int

const (
	// KindGauge is a value read at an instant, encoded as an OTLP gauge.
	KindGauge SampleKind = iota
	// KindCounter is a cumulative monotonic sum, encoded as an OTLP sum.
	KindCounter
)

// Sample is one point of one series: the metric name, the labels beyond the
// ones a scrape job or the pusher adds, the value, and the virtual instant it
// belongs to. Kind is KindGauge or KindCounter, which decides whether the push
// encodes it as an OTLP gauge or as a cumulative monotonic sum.
//
// The samples of one series over one interval share one Labels map, and nothing
// mutates it. A caller that has to carry a label of its own builds a map of its
// own.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  int64
	At     time.Time
	Kind   SampleKind
}

// The two counters Ceilometer polls per instance. The names are the ones its
// Prometheus exporter publishes them under, so a dashboard written for a real
// cloud reads a simulated one.
const (
	egressSeries  = "ceilometer_network_outgoing_bytes_total"
	ingressSeries = "ceilometer_network_incoming_bytes_total"
)

// trafficLevel is what one instance of a workload sends and receives over an
// office hour. Every other hour of the month is this level weighed by the
// working-week profile of profile.go.
type trafficLevel struct {
	egress  int64
	ingress int64
}

// trafficLevels is the level of each of the three workloads: a classic tenant's
// server is a machine a person works on, a shoot's worker carries what runs on
// it, and a CI runner is small.
//
// The ingress is the egress times 1/4 for a classic tenant, times 1/2 for a
// shoot and times 2/1 for a runner, and each of the three quotients is exact. A
// runner pulls its image and its dependencies and sends little back, which is
// the one of the three that receives more than it sends.
var trafficLevels = map[string]trafficLevel{
	workloadClassic:  {egress: 2147483648, ingress: 536870912},
	workloadGardener: {egress: 8589934592, ingress: 4294967296},
	workloadCI:       {egress: 268435456, ingress: 536870912},
}

// The per-instance jitter: a numerator drawn once per instance over a fixed
// denominator, so two instances of one workload do not report the same byte
// count while neither of them leaves its workload's level behind.
const (
	jitterMin = 50
	jitterMax = 150
	jitterDen = 100
)

// secondsPerHour is what a level stated per office hour is converted to the
// seconds of one grid step with.
const secondsPerHour = 3600

// maxMetricsInterval is the longest grid step the byte arithmetic holds for.
// The numerator of stepBytes is a level times the office weight times the
// step's seconds times the jitter, and the largest of those products is
// 8589934592 * 10 * 86400 * 150, about 1.11e18, which an int64 carries with
// room to spare. A run refuses a longer interval before it generates a month.
const maxMetricsInterval = 24 * time.Hour

// minMetricsInterval is the finest grid a month is placed on. TrafficOf places
// the samples of the whole period before the first notification goes out and
// holds them for the length of the run, and their count grows as the grid
// shrinks: a July of seed 1 carries about 320,000 samples on Ceilometer's 300s
// grid, ten times that at 30s, and about a hundred million at 1s, which is
// where the process is killed for its memory before it publishes anything. A
// run refuses a shorter interval before it generates a month, the way it
// refuses a longer one.
const minMetricsInterval = 30 * time.Second

// metricsSalt mixes the metrics into the state of their stream, the way
// faultSalt mixes a switch's name into the state of its own.
func metricsSalt() uint64 {
	digest := fnv.New64a()
	// A hash.Hash never fails a write, which is why the error is not examined.
	_, _ = digest.Write([]byte("metrics\x00traffic"))
	return digest.Sum64()
}

// metricsStream is the generator the traffic jitter is drawn from: the seed of
// the run together with a salt of its own.
//
// It is never the shape stream, the identifier stream or the noise stream. A
// month whose traffic was placed therefore consumes those three exactly as one
// whose traffic was not, and the two render byte-identical notifications.
func metricsStream(seed uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, metricsSalt()))
}

// stepBytes is what one grid step of one series accrues: the level, which is
// stated per office hour, weighed by the hour the step lies in and by the
// step's share of an hour, and moved by the instance's jitter. A night step
// therefore carries a tenth of an office step of the same level.
//
// Every factor is an int64 and the one division comes last. A byte count is a
// usage quantity that reaches an invoice, and a quotient that went through a
// float64 would arrive carrying digits nobody placed.
//
// The weight is read off the step's start alone, so a step that straddles the
// boundary between two hours carries the weight of the hour it began in.
func stepBytes(perOfficeHour int64, at time.Time, stepSeconds, jitter int64) int64 {
	return perOfficeHour * int64(hourWeight(at)) * stepSeconds * jitter /
		(officeWeight * secondsPerHour * jitterDen)
}

// gridStepAtOrAfter is the first instant of the grid from, from+interval,
// from+2*interval and so on that is at or after at. Every series of a month is
// placed on the one grid counted from the period's start, so two instances
// sampled around one instant carry the very same timestamp.
func gridStepAtOrAfter(from, at time.Time, interval time.Duration) time.Time {
	if !at.After(from) {
		return from
	}
	steps := (at.Sub(from) + interval - 1) / interval
	return from.Add(steps * interval)
}

// trafficStep is one grid step of one instance: the instant it was polled at,
// the project it ran in then, and the bytes the step accrued in each direction.
type trafficStep struct {
	at        time.Time
	projectID string
	egress    int64
	ingress   int64
}

// instanceSteps places the grid steps of one instance. It walks every grid
// instant from the start of the instance's first interval to the end of its
// last one, which is where Ceilometer starts polling an instance and where it
// stops.
//
// A step accrues bytes only while the interval it starts in is active. A
// stopped, a shelved and a resized instance move nothing, so their steps carry
// zero and the counter behind them stays flat, which is what a real counter of
// a stopped instance does.
//
// A step that falls into a gap between two intervals belongs to no state, so it
// accrues nothing. It is booked under the project of the interval before it,
// which is the last project the instance is known to have run in.
func instanceSteps(resource OracleResource, from time.Time, interval time.Duration,
	level trafficLevel, jitter int64,
) []trafficStep {
	if len(resource.Intervals) == 0 {
		return nil
	}
	intervals := resource.Intervals
	end := intervals[len(intervals)-1].To
	stepSeconds := int64(interval / time.Second)

	// The grid instants between the instance's first one and its end, which is
	// exactly what the loop below places: an instance live for a whole month
	// carries thousands of them, and a slice grown from nothing copies them over
	// and over on its way there.
	first := gridStepAtOrAfter(from, intervals[0].From, interval)
	steps := make([]trafficStep, 0, max(0, int(end.Sub(first)/interval)+1))
	projectID := intervals[0].ProjectID
	cursor := 0
	for s := first; s.Before(end); s = s.Add(interval) {
		for cursor < len(intervals) && !s.Before(intervals[cursor].To) {
			cursor++
		}
		step := trafficStep{at: s}
		if cursor < len(intervals) && !s.Before(intervals[cursor].From) {
			projectID = intervals[cursor].ProjectID
			if intervals[cursor].State == stateActive {
				step.egress = stepBytes(level.egress, s, stepSeconds, jitter)
				step.ingress = stepBytes(level.ingress, s, stepSeconds, jitter)
			}
		}
		step.projectID = projectID
		steps = append(steps, step)
	}
	return steps
}

// TrafficOf places the network traffic of a month: the samples every instance
// is pushed under, and the rows the oracle records them by.
//
// The samples come back ordered by their instant and then by name and resource
// id, so a pusher walks them with one cursor instead of indexing them. Their
// value is the running sum of the steps before them, which is what a cumulative
// counter reports: the first sample of every series is 0, and the sample at one
// instant states the bytes accrued before it.
//
// The rows come back ordered by resource id and then by From, because the
// oracle's instances are sorted by their id and the intervals of one instance
// by their start. A row states the exact sum of the steps placed inside its
// interval, so the increment of an instance's last step lands in its last row
// and in no sample, the way Ceilometer's last poll precedes a delete.
//
// The one refusal is an instance whose workload is none of the three: that is
// an oracle another build wrote, and a level this one would have to guess at.
// A byte count nobody stated is worse than no month at all.
func TrafficOf(oracle Oracle, seed uint64, interval time.Duration) ([]Sample, []OracleTraffic, error) {
	samples := make([]Sample, 0)
	rows := make([]OracleTraffic, 0)
	jitter := metricsStream(seed)

	for _, resource := range oracle.Resources {
		if resource.ResourceType != typeInstance {
			continue
		}
		level, ok := trafficLevels[resource.Workload]
		if !ok {
			return nil, nil, fmt.Errorf(
				"instance %s runs the workload %q, which no traffic level is stated for; "+
					"the levels are %s, %s and %s",
				resource.ResourceID, resource.Workload, workloadClassic, workloadGardener, workloadCI)
		}
		// One draw per instance, in oracle order, so the stream is consumed the
		// same way whatever a month holds beside its instances.
		drawn := jitterMin + jitter.Int64N(jitterMax-jitterMin+1)
		steps := instanceSteps(resource, oracle.PeriodFrom, interval, level, drawn)

		// Two samples per step, the egress and the ingress of it.
		samples = slices.Grow(samples, 2*len(steps))
		samples = appendTraffic(samples, oracle.Cloud, resource.ResourceID, steps)
		rows = appendRows(rows, resource, steps)
	}

	slices.SortFunc(samples, func(a, b Sample) int {
		if c := a.At.Compare(b.At); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Labels["resource_id"], b.Labels["resource_id"])
	})
	return samples, rows, nil
}

// appendTraffic states the two counters of one instance over its steps. The
// labels change only where the instance changed project, so the samples of one
// interval share the map their project was written into.
func appendTraffic(samples []Sample, cloud, resourceID string, steps []trafficStep) []Sample {
	var (
		labels          map[string]string
		projectID       string
		egress, ingress int64
	)
	for _, step := range steps {
		if labels == nil || step.projectID != projectID {
			projectID = step.projectID
			labels = map[string]string{
				"platform":      platformOpenStack,
				"cloud":         cloud,
				"resource_type": typeInstance,
				"resource_id":   resourceID,
				"project_id":    projectID,
			}
		}
		samples = append(samples,
			Sample{Name: egressSeries, Labels: labels, Value: egress, At: step.at, Kind: KindCounter},
			Sample{Name: ingressSeries, Labels: labels, Value: ingress, At: step.at, Kind: KindCounter})
		egress += step.egress
		ingress += step.ingress
	}
	return samples
}

// appendRows records one row per interval of the instance, whether a step fell
// inside it or not: an interval shorter than the grid holds no step and is
// stated with no bytes rather than left out, so a comparison reads the row of
// every interval the oracle holds.
//
// The steps and the intervals are both in order, so one cursor walks them
// together. A step that fell into a gap between two intervals is summed into
// the row that follows it, which adds nothing: it is the same step
// instanceSteps accrued no bytes for.
func appendRows(rows []OracleTraffic, resource OracleResource, steps []trafficStep) []OracleTraffic {
	cursor := 0
	for _, interval := range resource.Intervals {
		row := OracleTraffic{ResourceID: resource.ResourceID, From: interval.From, To: interval.To}
		for ; cursor < len(steps) && steps[cursor].at.Before(interval.To); cursor++ {
			row.EgressBytes += steps[cursor].egress
			row.IngressBytes += steps[cursor].ingress
		}
		rows = append(rows, row)
	}
	return rows
}

// The series the four dashboards read and the drill requires, under the names
// and the label spellings the OpenStack exporter publishes them with. A name
// this endpoint spelled its own way is a panel that stays empty against a
// simulated cloud and fills against a real one.
const (
	seriesNovaServerStatus    = "openstack_nova_server_status"
	seriesCinderVolumeStatus  = "openstack_cinder_volume_status"
	seriesCinderVolumeGB      = "openstack_cinder_volume_gb"
	seriesGlanceImageBytes    = "openstack_glance_image_bytes"
	seriesNeutronFloatingIP   = "openstack_neutron_floating_ip"
	seriesNeutronRouter       = "openstack_neutron_router"
	seriesLoadBalancerStatus  = "openstack_loadbalancer_loadbalancer_status"
	seriesLimitsInstancesUsed = "openstack_nova_limits_instances_used"
	seriesLimitsInstancesMax  = "openstack_nova_limits_instances_max"
	seriesLimitsVCPUsUsed     = "openstack_nova_limits_vcpus_used"
	seriesLimitsVCPUsMax      = "openstack_nova_limits_vcpus_max"
	seriesLimitsMemoryUsed    = "openstack_nova_limits_memory_used"
	seriesLimitsMemoryMax     = "openstack_nova_limits_memory_max"
	seriesIdentityProjects    = "openstack_identity_projects"
	seriesIdentityProjectInfo = "openstack_identity_project_info"
	seriesNovaTotalVMs        = "openstack_nova_total_vms"
	seriesCinderVolumes       = "openstack_cinder_volumes"
	seriesNeutronFloatingIPs  = "openstack_neutron_floating_ips"
	seriesGlanceImages        = "openstack_glance_images"
	seriesLoadBalancerTotal   = "openstack_loadbalancer_total_loadbalancers"
)

// The quota a project is reported against. The simulated world has no quota, so
// the three are constants: they exist because the drilldown's gauge panels
// divide a used series by a maximum one, and a panel without the divisor shows
// a division by an absent series rather than a ratio.
const (
	limitInstancesMax = 100
	limitVCPUsMax     = 400
	limitMemoryMaxMB  = 819200
)

// The two transitions of the noise catalogue the routers of a month are folded
// out of.
const (
	routerCreateType = "router.create.end"
	routerDeleteType = "router.delete.end"
)

// router is one neutron router of the month: the id it was announced under and
// the project it was created in.
type router struct {
	id        string
	projectID string
}

// routerFold folds the routers of a schedule out of it, in the order they were
// created in.
//
// Routers are the one family of the inventory the oracle does not hold: nothing
// bills a router, so the generator books none, and the noise catalogue is where
// neutron announces them. The classic tenants' networks pre-exist the month and
// neutron announces nothing about a resource that was there before the first
// transition, so those projects report no router at all, which is what the
// simulated world holds.
//
// The cursor is what a run pushes a month through: it never moves back, so the
// grid of a whole month costs one pass over the schedule rather than a pass per
// grid step, which is a schedule of fifteen thousand transitions walked from
// its head nine thousand times.
type routerFold struct {
	schedule Schedule
	cursor   int
	live     []router
}

// at is the routers that stand at the instant at.
//
// The instants it is asked about only ever move forward, because the fold
// carries what the ones before them left behind. A caller that reads an earlier
// instant builds a fold of its own, which is what every single reading does.
func (f *routerFold) at(at time.Time) []router {
	for ; f.cursor < len(f.schedule); f.cursor++ {
		transition := f.schedule[f.cursor]
		// The schedule is in instant order, so the first transition past at is
		// where the fold stops.
		if transition.At.After(at) {
			break
		}
		switch transition.EventType {
		case routerCreateType:
			f.live = append(f.live, router{id: transition.ResourceID, projectID: transition.ProjectID})
		case routerDeleteType:
			f.live = slices.DeleteFunc(f.live, func(held router) bool {
				return held.id == transition.ResourceID
			})
		}
	}
	return f.live
}

// projectUsage is what one project holds at an instant, in the units nova's
// limits report: a count of servers, their vcpus, and their memory in
// megabytes.
type projectUsage struct {
	instances int64
	vcpus     int64
	memoryMB  int64
}

// InventoryAt is the world of the month as it stands at the virtual instant at:
// one gauge per live resource of the six families the oracle holds, the routers
// the schedule left standing, the limits of every project, and the counts the
// cloud reports about itself.
//
// An instant past the end of the month is answered at that end, the way the
// fake API answers a listing from a clock that has run past it: every interval
// of the oracle ends inside the month, and a later instant would report a cloud
// that lost everything at once.
//
// A size member an interval does not carry reads as zero, which is what
// sizeDecimal answers with, so one malformed interval costs its own series a
// value instead of failing a whole scrape. The status of a server is the word
// nova reports rather than the one the collector books it under.
//
// The samples carry neither a platform nor a cloud label. A real exporter
// carries neither: both come from the scrape job's static labels, and an
// endpoint that stated them as well would push the job's own under
// exported_cloud. A series the table gives no labels of its own carries no map.
func InventoryAt(month Month, at time.Time) []Sample {
	return inventoryAt(month, at, &routerFold{schedule: month.Schedule})
}

// inventoryAt is InventoryAt over a fold the caller keeps. A run reads one
// inventory per grid step and hands the same fold to every one of them, so a
// month folds its routers once instead of once per step.
func inventoryAt(month Month, at time.Time, routers *routerFold) []Sample {
	at = at.UTC()
	if at.After(month.Oracle.PeriodTo) {
		at = month.Oracle.PeriodTo.UTC()
	}

	samples := make([]Sample, 0)
	usage := make(map[string]projectUsage)
	var instances, volumes, addresses, images, balancers int64

	for _, resource := range month.Oracle.Resources {
		interval, ok := liveAt(resource, at, month.Oracle.PeriodTo)
		if !ok {
			continue
		}
		id := resource.ResourceID
		switch resource.ResourceType {
		case typeInstance:
			held, _ := flavorByName(sizeString(interval.Size, "flavor"))
			samples = append(samples, gauge(seriesNovaServerStatus, map[string]string{
				"id":        id,
				"uuid":      id,
				"name":      id,
				"tenant_id": interval.ProjectID,
				"status":    novaVMStates[interval.State],
				"flavor_id": held.flavorID,
			}, 1, at))

			used := usage[interval.ProjectID]
			used.instances++
			used.vcpus += int64(sizeInt(interval.Size, "vcpus"))
			// Nova's limits report megabytes where the size states gibibytes.
			used.memoryMB += sizeDecimal(interval.Size, "ram_gb").Mul(mebibytesPerGibibyte).IntPart()
			usage[interval.ProjectID] = used
			instances++

		case typeVolume:
			volumeType := sizeString(interval.Size, "type")
			samples = append(samples,
				gauge(seriesCinderVolumeStatus, map[string]string{
					"id":          id,
					"name":        id,
					"tenant_id":   interval.ProjectID,
					"status":      interval.State,
					"volume_type": volumeType,
				}, 1, at),
				gauge(seriesCinderVolumeGB, map[string]string{
					"id":          id,
					"name":        id,
					"tenant_id":   interval.ProjectID,
					"volume_type": volumeType,
				}, int64(sizeInt(interval.Size, "size_gb")), at))
			volumes++

		case typeImage:
			samples = append(samples, gauge(seriesGlanceImageBytes, map[string]string{
				"id":        id,
				"name":      id,
				"tenant_id": interval.ProjectID,
			}, sizeDecimal(interval.Size, "size_gb").Mul(bytesPerGibibyte).IntPart(), at))
			images++

		case typeFloatingIP:
			samples = append(samples, gauge(seriesNeutronFloatingIP, map[string]string{
				"id":         id,
				"project_id": interval.ProjectID,
				"status":     "ACTIVE",
			}, 1, at))
			addresses++

		case typeLoadBalancer:
			samples = append(samples, gauge(seriesLoadBalancerStatus, map[string]string{
				"id":                  id,
				"name":                id,
				"project_id":          interval.ProjectID,
				"provisioning_status": "ACTIVE",
				"operating_status":    "ONLINE",
			}, 1, at))
			balancers++
		}
	}

	for _, held := range routers.at(at) {
		samples = append(samples, gauge(seriesNeutronRouter, map[string]string{
			"id":         held.id,
			"project_id": held.projectID,
			"status":     "ACTIVE",
		}, 1, at))
	}

	for _, tenant := range month.Tenants {
		used := usage[tenant.ID]
		// The six limits of one project share the one label the table gives them.
		limits := map[string]string{"tenant_id": tenant.ID}
		samples = append(samples,
			gauge(seriesLimitsInstancesUsed, limits, used.instances, at),
			gauge(seriesLimitsInstancesMax, limits, limitInstancesMax, at),
			gauge(seriesLimitsVCPUsUsed, limits, used.vcpus, at),
			gauge(seriesLimitsVCPUsMax, limits, limitVCPUsMax, at),
			gauge(seriesLimitsMemoryUsed, limits, used.memoryMB, at),
			gauge(seriesLimitsMemoryMax, limits, limitMemoryMaxMB, at),
			gauge(seriesIdentityProjectInfo, map[string]string{
				"id":        tenant.ID,
				"name":      tenant.Name,
				"domain_id": "default",
				"enabled":   "true",
			}, 1, at))
	}

	return append(samples,
		gauge(seriesIdentityProjects, nil, int64(len(month.Tenants)), at),
		gauge(seriesNovaTotalVMs, nil, instances, at),
		gauge(seriesCinderVolumes, nil, volumes, at),
		gauge(seriesNeutronFloatingIPs, nil, addresses, at),
		gauge(seriesGlanceImages, nil, images, at),
		gauge(seriesLoadBalancerTotal, nil, balancers, at))
}

// gauge is one inventory sample. Every one of them states the world at the
// instant it was read, which is what a gauge is.
func gauge(name string, labels map[string]string, value int64, at time.Time) Sample {
	return Sample{Name: name, Labels: labels, Value: value, At: at, Kind: KindGauge}
}

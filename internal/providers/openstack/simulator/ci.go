package simulator

import "time"

// The CI tenant is the OpenStack project a build system runs its jobs in. It
// boots one runner per job and destroys it when the job ends, so a runner's
// whole life is shorter than the gap between two notifications of a classic
// tenant. That is the shape the other workloads do not produce: a month of
// resources that are created and closed inside it, hundreds of them, none of
// which the projection carries into the next period.
//
// A pipeline is started by somebody pushing, so the bursts fall into the
// working hours of a Monday to Friday of the real calendar and the weekends
// hold none of them.
//
// A runner is called runner-<the first eight hex digits of its instance id>,
// the way a build system names a throwaway machine after the job it was
// requested for. The name is cosmetic: nothing is metered by it, and it is
// there so that a dumped month reads like a deployment rather than like a list
// of ids.

// The shape of one burst: how many runners it holds and how far apart they are
// requested. runnerGapCeiling is exclusive, the way span draws whole seconds
// below the duration it is handed.
const (
	minBurstRunners  = 2
	maxBurstRunners  = 5
	minRunnerGap     = 1 * time.Second
	runnerGapCeiling = 4 * time.Second
)

// longestBurst is how long after its first runner the last runner of a burst
// boots: the gaps of the widest burst, each of them the longest one a draw
// yields. It is derived from the burst above rather than written out, because
// the first runner is drawn so that even the last one of its burst comes up
// before the working hours end, and a burst widened past a constant that stated
// its own number would push runners past that end unnoticed.
const longestBurst = (maxBurstRunners - 1) * (runnerGapCeiling - time.Second)

// ci generates the CI tenant's month. The image comes first because every
// runner boots from it, and it is the one resource of this tenant that outlives
// a single job.
func (g *generator) ci() {
	g.tenantImage(g.ciTenant)

	for _, d := range workingDays(g.from, g.to) {
		// A push starts several jobs at once, which is why the day's runners come
		// in bursts rather than one at a time: the runners of one burst are
		// requested seconds apart and run side by side.
		for range 4 + g.shape.IntN(5) {
			t := drawInstant(g.shape, at(d, officeFrom, 0), at(d, officeTo, 0).Add(-longestBurst))
			for range minBurstRunners + g.shape.IntN(maxBurstRunners-minBurstRunners+1) {
				g.runner(t)
				t = t.Add(span(g.shape, minRunnerGap, runnerGapCeiling))
			}
		}
	}
}

// runner boots one runner and destroys it again once its job is over. It holds
// no volume and no address, and nothing keeps it: the pipeline that asked for
// it is done with it by the delete, and the month refers to it through neither
// its name nor its id afterwards.
func (g *generator) runner(t time.Time) {
	tenant := g.ciTenant
	id := g.identifiers.nextUUID()
	inst := &instance{
		id:        id,
		name:      "runner-" + id[:8],
		flavor:    runnerFlavors[g.shape.IntN(len(runnerFlavors))],
		host:      computeHosts[g.shape.IntN(len(computeHosts))],
		imageID:   tenant.images[0].id,
		createdAt: t,
	}

	g.emit(t, "compute.instance.create.end", computePublisher(inst), inst.id, tenant,
		instanceCreatePayload(tenant, inst, g.cloud))

	deletedAt := t.Add(span(g.shape, 3*time.Minute, 40*time.Minute))
	g.emit(deletedAt, "compute.instance.delete.end", computePublisher(inst), inst.id, tenant,
		instanceDeletePayload(tenant, inst, deletedAt))
}

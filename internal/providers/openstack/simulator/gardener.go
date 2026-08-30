package simulator

import (
	"fmt"
	"slices"
	"strconv"
	"time"
)

// A Gardener shoot is simulated from the OpenStack side, the way the platform
// underneath one sees it: the machine controller boots and destroys nova
// servers, the cloud controller gives every service of type LoadBalancer an
// octavia load balancer with a floating address, and the CSI driver turns a
// persistent volume claim into a cinder volume. Nothing here speaks to a
// Kubernetes API, and nothing of the cluster itself is rendered.
//
// Only the billable side of a shoot is generated: the workers, the volumes, the
// addresses, the image the workers boot from, and the load balancers. The
// network, the subnet, the router, and the ports are named in the payloads that
// refer to them. The security group and the keypair are named by nothing yet:
// they are held for the issue that adds the neutron and keystone side, which is
// where the notifications about all of them belong.
//
// The names are the shapes Gardener's OpenStack extension and the
// machine-controller-manager give the resources they create: a technical id
// shoot--<project>--<shoot>, a worker <technical id>-worker-z1-<5 hex>-<5 hex>,
// a root volume named after the worker it carries, a claim
// <technical id>-dynamic-pvc-<volume id>, a load balancer
// kube_service_<technical id>_<namespace>_<service>, and a keypair
// <technical id>-ssh-publickey. They are cosmetic: nothing is metered by a
// name, and they are there so that a dumped month reads like a real deployment.

// newGardenerProjects returns the projects a month is generated from, built
// fresh on every call. A shoot carries the state of the month it is walked
// through, such as which workers are alive at this moment, so a package
// variable would hand one Generate call's fleet to the next and the second
// month would come out a different one.
//
// The three shoots are the three shapes a cluster has over a billing month: one
// that runs the whole month and rolls its workers once, one that hibernates
// every night and boots its workers from volumes, and one that is created and
// deleted inside the month.
func newGardenerProjects() []*gardenerProject {
	return []*gardenerProject{
		{
			name: "alpha",
			shoots: []*shoot{
				{name: "api-prod", flavor: xlargeFlavor},
				{name: "api-dev", flavor: bootVolumeFlavor, bootFromVolume: true, hibernates: true},
			},
		},
		{
			name:   "beta",
			shoots: []*shoot{{name: "batch", flavor: largeFlavor, transient: true}},
		},
	}
}

// gardener generates one Gardener project's month. The image comes first
// because every worker of every shoot boots from it.
func (g *generator) gardener(gp *gardenerProject) {
	g.tenantImage(gp.tenant)
	for _, s := range gp.shoots {
		g.shoot(gp, s)
	}
}

// tenantImage uploads the one image a machine-driven tenant boots its servers
// from. It is uploaded in the first half hour of the month and never deleted:
// the fleet on top of it is created and destroyed all month long, and an image
// that went with one of those steps would leave the next boot without one.
func (g *generator) tenantImage(p *project) {
	img := p.images[0]
	img.createdAt = g.from.Add(span(g.shape, 0, 30*time.Minute))
	uploadedAt := img.createdAt.Add(span(g.shape, 30*time.Second, 120*time.Second))
	// Every quarter gibibyte from one to four, the way a classic image is sized.
	img.size = int64(4+g.shape.IntN(13)) * quarterGiB

	g.emit(img.createdAt, imageCreateType, imagePublisher, img.id, p, imageCreatePayload(p, img))
	g.uploadImage(p, img, uploadedAt)
}

// shoot creates one cluster and walks it through every day it exists.
//
// The one-off steps of the month are drawn before the first of them is
// generated, because each of them is a day the day loop has to recognise when
// it reaches it. A transient shoot is created and deleted while its team is at
// work, and a shoot that outlives the month comes up in the first hours of it,
// where Gardener's reconciliation would have picked it up.
func (g *generator) shoot(gp *gardenerProject, s *shoot) {
	s.baseWorkers = 3 + g.shape.IntN(2)
	if s.transient {
		s.createdAt = drawWorkingInstant(g.shape, g.from.Add(2*day), g.from.Add(8*day))
		s.deletedAt = drawWorkingInstant(g.shape, g.from.Add(18*day), g.from.Add(27*day))
	} else {
		s.createdAt = g.from.Add(span(g.shape, time.Hour, 4*time.Hour))
	}
	if !s.hibernates && !s.transient {
		s.rollingUpdateDay = at(drawWorkingInstant(g.shape, g.from.Add(8*day), g.from.Add(20*day)), 0, 0)
		s.secondBalancerDay = at(drawWorkingInstant(g.shape, g.from.Add(3*day), g.from.Add(20*day)), 0, 0)
		s.listenerDay = at(drawWorkingInstant(g.shape, g.from.Add(5*day), g.from.Add(25*day)), 0, 0)
	}
	// A development cluster publishes a second service or it does not, which is
	// the one shoot whose balancer count depends on the seed.
	if s.hibernates && g.shape.IntN(2) == 0 {
		s.secondBalancerDay = at(drawWorkingInstant(g.shape, g.from.Add(3*day), g.from.Add(20*day)), 0, 0)
	}

	// The network the shoot's servers hold their addresses on, built from the
	// ids the shoot was named with. Its range is the one the shoot's VIP
	// addresses already lie in, and the notifications about the network itself
	// are a later package's. Building it draws nothing.
	s.network = &network{
		id:              s.networkID,
		subnetID:        s.subnetID,
		routerID:        s.routerID,
		securityGroupID: s.securityGroupID,
		name:            s.technicalID,
		cidr:            fmt.Sprintf("10.250.%d.0/24", s.index),
	}

	// The creation sequence: the workers come up seconds apart, the workloads
	// that claim storage are scheduled once the nodes are ready, and the ingress
	// controller's load balancer follows within the hour.
	t := s.createdAt.Add(span(g.shape, 30*time.Second, 90*time.Second))
	for range s.baseWorkers {
		g.bootWorker(s, t)
		t = t.Add(span(g.shape, 3*time.Second, 10*time.Second))
	}
	claims := 2 + g.shape.IntN(2)
	t = s.createdAt.Add(span(g.shape, 5*time.Minute, 30*time.Minute))
	for range claims {
		g.createClaim(s, t)
		t = t.Add(span(g.shape, time.Second, 20*time.Second))
	}
	g.createLoadBalancer(s, s.createdAt.Add(span(g.shape, 10*time.Minute, 60*time.Minute)),
		"kube_service_"+s.technicalID+"_ingress_nginx-ingress-controller", 2, 2)

	for d := at(s.createdAt, 0, 0); d.Before(g.to); d = d.AddDate(0, 0, 1) {
		g.shootDay(s, d)
		// The tear-down is the last thing the shoot does, so the days after it
		// are days it no longer exists on.
		if s.transient && d.Equal(at(s.deletedAt, 0, 0)) {
			break
		}
	}
}

// shootDay generates one day of a shoot, in the order the day runs: the cluster
// wakes up, the autoscaler adds workers for the working hours, the one-off
// steps of the day happen, the workloads move their claims around, the
// autoscaler gives the workers back in the evening, and the cluster hibernates
// or is torn down.
//
// Everything the working hours bring is generated only on a day the shoot is
// fully alive, from the morning to the evening. A day it is created, torn down,
// or hibernating through is a day the autoscaler and the workloads have no full
// window on, and rendering their steps into a fraction of one would put load on
// a cluster that was not there yet.
func (g *generator) shootDay(s *shoot, d time.Time) {
	if s.hibernates && !s.awake && workingDay(d) {
		t := at(d, officeFrom, 0)
		var last time.Time
		for range s.baseWorkers {
			last = t
			g.bootWorker(s, t)
			t = t.Add(span(g.shape, 3*time.Second, 10*time.Second))
		}
		// The claims outlive the night and are mounted again by the workloads the
		// new workers run, so cinder reports them in use from the wake-up on. They
		// go onto the first worker two seconds apart, once the last of the pool is
		// up.
		for i, claim := range s.claims {
			g.attach(s.owner.tenant, claim, s.workers[0],
				last.Add(attachLag+time.Duration(2*i)*time.Second))
		}
		s.awake = true
	}

	alive := s.createdAt.Before(at(d, officeFrom, 0)) &&
		(s.deletedAt.IsZero() || !s.deletedAt.Before(at(d, officeTo, 0))) && s.awake

	if workingDay(d) && alive {
		t := drawInstant(g.shape, at(d, 8, 0), at(d, 12, 0))
		for range 1 + g.shape.IntN(2) {
			s.added = append(s.added, g.bootWorker(s, t))
			t = t.Add(span(g.shape, 3*time.Second, 10*time.Second))
		}
	}

	// A rolling update replaces the workers of the base pool one at a time: the
	// new machine is up before the old one goes, which is what keeps the cluster
	// at its capacity while its nodes are exchanged. The workers the autoscaler
	// added today are left alone, because they already run the new version.
	if d.Equal(s.rollingUpdateDay) {
		t := drawWorkingInstant(g.shape, at(d, 9, 0), at(d, 15, 0))
		for _, w := range slices.Clone(s.workers) {
			if slices.Contains(s.added, w) {
				continue
			}
			g.bootWorker(s, t)
			deletedAt := t.Add(span(g.shape, 2*time.Minute, 5*time.Minute))
			g.deleteWorker(s, w, deletedAt)
			t = deletedAt.Add(span(g.shape, time.Minute, 3*time.Minute))
		}
	}

	if !s.secondBalancerDay.IsZero() && d.Equal(s.secondBalancerDay) {
		g.createLoadBalancer(s, drawWorkingInstant(g.shape, at(d, 9, 0), at(d, 17, 0)),
			"kube_service_"+s.technicalID+"_default_api", 1, 1)
	}

	// A service that publishes another port gets another listener on the
	// balancer it already has. Creating the listener sends no notification of
	// its own, the way octavia notifies on the load balancer alone, so the
	// balancer's update is where the new count arrives.
	if !s.listenerDay.IsZero() && d.Equal(s.listenerDay) {
		lb := s.loadBalancers[0]
		lb.listenerIDs = append(lb.listenerIDs, g.identifiers.nextUUID())
		g.emit(drawWorkingInstant(g.shape, at(d, 9, 0), at(d, 17, 0)),
			"octavia.loadbalancer.update.end", "", lb.id, s.owner.tenant,
			loadBalancerPayload(s.owner.tenant, s, lb, true))
	}

	if workingDay(d) && alive {
		g.claimActivity(s, d)
	}

	if workingDay(d) && alive && len(s.added) > 0 {
		t := drawInstant(g.shape, at(d, 16, 0), at(d, 18, 30))
		for _, w := range slices.Clone(s.added) {
			g.deleteWorker(s, w, t)
			t = t.Add(span(g.shape, 10*time.Second, 60*time.Second))
		}
		s.added = nil
	}

	// Hibernation destroys every worker and keeps everything else: the claims
	// hold the cluster's data over the night and the balancers hold their
	// addresses, so a hibernating shoot is billed for its storage and its
	// network throughout and for its servers by the working day.
	if s.hibernates && s.awake {
		t := at(d, officeTo, 0)
		for _, w := range slices.Clone(s.workers) {
			g.deleteWorker(s, w, t)
			t = t.Add(span(g.shape, 5*time.Second, 15*time.Second))
		}
		// The claims are detached rather than deleted: they hold the data over the
		// night, and cinder reports them available until the next wake-up mounts
		// them again.
		for i, claim := range s.claims {
			g.detach(s.owner.tenant, claim, t.Add(time.Second+time.Duration(2*i)*time.Second))
		}
		s.awake = false
	}

	if s.transient && d.Equal(at(s.deletedAt, 0, 0)) {
		g.tearDown(s)
	}
}

// bootWorker creates one worker of the shoot and returns it. A worker that
// boots from a volume has its root volume provisioned seconds before the server
// exists, because the image is written to that volume first and the server is
// booted off it afterwards.
func (g *generator) bootWorker(s *shoot, t time.Time) *instance {
	tenant := s.owner.tenant
	id := g.identifiers.nextUUID()
	// The machine-controller-manager names a machine after its worker pool and
	// two short pieces of randomness, which is what these two slices of the
	// identifier stand in for.
	name := fmt.Sprintf("%s-worker-z1-%s-%s", s.technicalID, id[:5], id[24:29])
	w := &instance{
		id:        id,
		name:      name,
		host:      computeHosts[g.shape.IntN(len(computeHosts))],
		flavor:    s.flavor,
		createdAt: t,
	}

	if s.bootFromVolume {
		vol := &volume{
			id:         g.identifiers.nextUUID(),
			name:       name,
			sizeGB:     rootVolumeSizeGB,
			volumeType: rootVolumeType,
			createdAt:  t.Add(-span(g.shape, 5*time.Second, 15*time.Second)),
		}
		// The worker holds the volume before the attach is rendered, because a
		// root volume is mounted as the server's first disk and every other
		// volume as its second.
		w.bootVolume = vol
		g.createVolume(tenant, vol, vol.createdAt)
		g.attach(tenant, vol, w, vol.createdAt.Add(attachLag))
	} else {
		w.imageID = tenant.images[0].id
	}

	g.createInstance(tenant, w, s.network, t)
	s.workers = append(s.workers, w)
	return w
}

// deleteWorker destroys one worker, and its root volume with it: a machine that
// boots from a volume takes that volume when it goes, so the volume is billed
// for exactly as long as the worker is. Every worker a shoot loses goes through
// here, whether it is scaled down, rolled, hibernated, or torn down.
func (g *generator) deleteWorker(s *shoot, w *instance, t time.Time) {
	tenant := s.owner.tenant
	g.destroyInstance(tenant, w, t)

	if w.bootVolume != nil {
		deletedAt := t.Add(span(g.shape, 20*time.Second, 40*time.Second))
		g.deleteVolume(tenant, w.bootVolume, deletedAt)
	}

	s.workers = slices.DeleteFunc(s.workers, func(other *instance) bool { return other == w })
	s.added = slices.DeleteFunc(s.added, func(other *instance) bool { return other == w })
}

// createClaim provisions one persistent volume claim. The CSI driver names the
// volume after the claim it stands for, and the volume is in use from the
// moment it exists: the pod that asked for it is waiting on the mount.
func (g *generator) createClaim(s *shoot, t time.Time) {
	tenant := s.owner.tenant
	id := g.identifiers.nextUUID()
	vol := &volume{
		id:         id,
		name:       s.technicalID + "-dynamic-pvc-" + id,
		sizeGB:     claimSizesGB[g.shape.IntN(len(claimSizesGB))],
		volumeType: volumeTypes[g.shape.IntN(len(volumeTypes))],
		createdAt:  t,
	}

	g.createVolume(tenant, vol, t)

	// The claim is mounted by the first worker of the shoot. A shoot with no
	// worker at that moment, which the month never produces, attaches it to
	// nothing rather than reaching into an empty pool.
	var w *instance
	if len(s.workers) > 0 {
		w = s.workers[0]
	}
	g.attach(tenant, vol, w, t.Add(attachLag))
	s.claims = append(s.claims, vol)
}

// claimActivity is what a working day does to a cluster's storage: a workload
// is deployed and claims a volume, one that ran out of room is expanded, and
// one whose workload is gone is released. Each of the three happens on a draw
// of its own, so a day brings any combination of them, none included.
//
// A claim created today is no candidate for either of the other two, because a
// resize or a release of a volume in the hour it was provisioned is not a day
// in the life of a cluster. The shoot's first claim is never released: it is
// what carries the state that outlives every workload, and it keeps a month
// from being a month in which every claim of a shoot is gone by the end.
func (g *generator) claimActivity(s *shoot, d time.Time) {
	tenant := s.owner.tenant
	lo, hi := at(d, 8, 0), at(d, 18, 30)

	if g.shape.IntN(3) == 0 {
		g.createClaim(s, drawInstant(g.shape, lo, hi))
	}

	// The volume that grew today is left out of the release below: two
	// notifications about one volume in the same second are two the projection
	// cannot order, and a release of what was just expanded says nothing.
	var grown *volume
	if g.shape.IntN(4) == 0 {
		var candidates []*volume
		for _, vol := range s.claims {
			// A claim grows at most twice, which is what keeps a month from
			// doubling one volume into a size no cluster carries.
			if vol.createdAt.Before(d) && vol.resizes < 2 {
				candidates = append(candidates, vol)
			}
		}
		if len(candidates) > 0 {
			t := drawInstant(g.shape, lo, hi)
			vol := candidates[g.shape.IntN(len(candidates))]
			// The .start half reports the claim at the size it still has.
			g.noise(t.Add(-stepLead), "volume.resize.start", volumePublisher, vol.id, tenant,
				volumeStatusPayload(tenant, vol, "extending"))
			vol.sizeGB *= 2
			vol.resizes++
			g.emit(t, "volume.resize.end", volumePublisher, vol.id, tenant,
				volumeStatePayload(tenant, vol))
			grown = vol
		}
	}

	if g.shape.IntN(5) == 0 {
		var candidates []*volume
		for _, vol := range s.claims[1:] {
			if vol.createdAt.Before(d) && vol != grown {
				candidates = append(candidates, vol)
			}
		}
		if len(candidates) > 0 {
			t := drawInstant(g.shape, lo, hi)
			vol := candidates[g.shape.IntN(len(candidates))]
			g.deleteVolume(tenant, vol, t)
			s.claims = slices.DeleteFunc(s.claims, func(other *volume) bool { return other == vol })
		}
	}
}

// createLoadBalancer publishes one service of type LoadBalancer: octavia
// creates the balancer, neutron gives its VIP port a floating address, and the
// listeners and pools of the service arrive on the update that follows.
//
// The update is the notification the balancer's size is booked from. Octavia
// reports the listeners and the pools on an update alone, and the create is
// sent before the service's ports are attached, so the create carries a
// balancer of zero listeners and zero pools.
func (g *generator) createLoadBalancer(s *shoot, t time.Time, name string, listeners, pools int) {
	tenant := s.owner.tenant
	lb := &loadBalancer{id: g.identifiers.nextUUID(), name: name, vipPortID: g.identifiers.nextUUID()}
	for range listeners {
		lb.listenerIDs = append(lb.listenerIDs, g.identifiers.nextUUID())
	}
	for range pools {
		lb.poolIDs = append(lb.poolIDs, g.identifiers.nextUUID())
	}
	// The VIP comes from the counter the shoot's ports already draw from. A
	// balancer sits on the shoot's own subnet, which is the range of its third
	// octet, and neutron allocates every address of a subnet once: a second
	// allocator over the same range would hand a worker's port and a VIP the
	// same fixed address while both are up.
	lb.vipAddress = s.network.nextAddress()
	lb.fip = &floatingIP{
		id:      g.identifiers.nextUUID(),
		address: floatingPrefix + strconv.Itoa(1+g.addresses[g.assigned]),
		// The address of a load balancer is associated with its VIP port at once,
		// which is what makes it the one address of a month that is up from the
		// moment it is allocated.
		portID:       lb.vipPortID,
		fixedAddress: lb.vipAddress,
		routerID:     s.routerID,
	}
	g.assigned++
	s.loadBalancers = append(s.loadBalancers, lb)

	g.emit(t, "octavia.loadbalancer.create.end", "", lb.id, tenant,
		loadBalancerPayload(tenant, s, lb, false))
	allocatedAt := t.Add(span(g.shape, 2*time.Second, 10*time.Second))
	g.emit(allocatedAt, "floatingip.create.end", networkPublisher, lb.fip.id, tenant,
		floatingIPCreatePayload(tenant, lb.fip, g.networkID))

	g.emit(t.Add(span(g.shape, 60*time.Second, 300*time.Second)),
		"octavia.loadbalancer.update.end", "", lb.id, tenant,
		loadBalancerPayload(tenant, s, lb, true))
}

// tearDown deletes a shoot in the order Gardener does, which is the order its
// resources depend on each other in: the services go first, address before
// balancer, then the workers, then the claims they held. Nothing of the shoot
// is emitted after the last of them. A claim is attached unless the shoot
// hibernated, so its delete carries the detach cinder sends three seconds
// before it.
func (g *generator) tearDown(s *shoot) {
	tenant := s.owner.tenant
	t := s.deletedAt

	for _, lb := range s.loadBalancers {
		g.emit(t, "floatingip.delete.end", networkPublisher, lb.fip.id, tenant,
			floatingIPDeletePayload(lb.fip))
		t = t.Add(span(g.shape, 5*time.Second, 15*time.Second))
		g.emit(t, "octavia.loadbalancer.delete.end", "", lb.id, tenant,
			loadBalancerPayload(tenant, s, lb, false))
		t = t.Add(span(g.shape, 5*time.Second, 15*time.Second))
	}
	for _, w := range slices.Clone(s.workers) {
		g.deleteWorker(s, w, t)
		t = t.Add(span(g.shape, 5*time.Second, 15*time.Second))
	}
	for _, vol := range s.claims {
		g.deleteVolume(tenant, vol, t)
		t = t.Add(span(g.shape, 5*time.Second, 15*time.Second))
	}
	s.claims = nil
}

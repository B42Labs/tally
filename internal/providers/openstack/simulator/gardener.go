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
// The billable side of a shoot is the workers, the volumes, the addresses, the
// image the workers boot from, and the load balancers. Everything a cluster
// needs besides them is announced by the helpers of noise.go around those
// transitions and billed for by nothing: the network, the subnet, the router,
// the security group with its rules, the keypair, the ports, the DNS records,
// and the certificate the ingress terminates on.
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

// gardener generates one Gardener project's month. The tenant is announced
// first because nothing in it exists before it does, the image follows because
// every worker of every shoot boots from it, and the zone follows the image
// because the shoots publish their records into it.
//
// An image drawn at exactly the month start shares its instant with the
// project's creation. The sort of the month is stable, so the announcement
// stays where it was emitted: ahead of the tenant's first resource.
func (g *generator) gardener(gp *gardenerProject) {
	g.announceTenant(gp.tenant, g.from)
	g.tenantImage(gp.tenant)
	g.createZone(gp, g.from.Add(2*time.Second))
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

	g.emit(img.createdAt, imageCreateType, imagePublisher, img.id, p, imageCreatePayload(p, img),
		unbooked)
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

	// What Gardener creates before the first worker can boot: the network the
	// servers hold their addresses on, the security group, the keypair, and the
	// record the API server answers on.
	g.shootInfrastructure(s, s.createdAt)

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
//
// Two of the day's steps take a keystone token: the wake-up and the rolling
// update, each of which is one reconciliation Gardener starts. The autoscaler
// and the claim activity take none. One record per action somebody or a
// controller starts is what keeps the month's authentications a number a reader
// can follow.
func (g *generator) shootDay(s *shoot, d time.Time) {
	if s.hibernates && !s.awake && workingDay(d) {
		t := at(d, officeFrom, 0)
		g.authenticate(s.owner.tenant, t.Add(-authenticateLead))
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

	fullyAlive := s.createdAt.Before(at(d, officeFrom, 0)) &&
		(s.deletedAt.IsZero() || !s.deletedAt.Before(at(d, officeTo, 0))) && s.awake

	if workingDay(d) && fullyAlive {
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
		g.authenticate(s.owner.tenant, t.Add(-authenticateLead))
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
			loadBalancerPayload(s.owner.tenant, s, lb, true),
			alive(stateActive, loadBalancerSizeOf(len(lb.listenerIDs), len(lb.poolIDs))))
	}

	if workingDay(d) && fullyAlive {
		g.claimActivity(s, d)
	}

	if workingDay(d) && fullyAlive && len(s.added) > 0 {
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
				volumeStatePayload(tenant, vol), alive(volumeStateOf(vol), volumeSizeOf(vol)))
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

// createLoadBalancer publishes one service of type LoadBalancer: neutron gives
// the balancer the port it holds its VIP on, octavia creates the balancer, the
// floating address follows, and the listeners and pools of the service arrive
// on the update after that.
//
// The update is the notification the balancer's size is booked from. Octavia
// reports the listeners and the pools on an update alone, and the create is
// sent before the service's ports are attached, so the create carries a
// balancer of zero listeners and zero pools.
//
// The first balancer of a shoot is what its ingress record points at, since
// that is the address every service of the cluster is reached under. A balancer
// that terminates TLS holds the certificate for it in barbican, which is four
// audit records twenty seconds after the create and before the update.
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

	// The port the balancer holds its VIP on. Octavia is the device behind it,
	// and it is bound to no host: the VIP lives on the balancer's amphorae and
	// not on a compute.
	vipPort := newPort(s.network, lb.vipPortID, lb.vipAddress, "lb-"+lb.id, "Octavia", "")
	g.noise(t.Add(-vipPortLead), "port.create.start", networkPublisher, lb.vipPortID, tenant,
		portRequestPayload(tenant, vipPort))
	g.noise(t.Add(-vipPortLead+time.Second), "port.create.end", networkPublisher, lb.vipPortID, tenant,
		portPayload(tenant, vipPort, false))

	g.emit(t, "octavia.loadbalancer.create.end", "", lb.id, tenant,
		loadBalancerPayload(tenant, s, lb, false), alive(stateActive, loadBalancerSizeOf(0, 0)))
	allocatedAt := t.Add(span(g.shape, 2*time.Second, 10*time.Second))
	g.emit(allocatedAt, "floatingip.create.end", networkPublisher, lb.fip.id, tenant,
		floatingIPCreatePayload(tenant, lb.fip, g.networkID), alive(stateActive, floatingIPSizeOf()))

	// The ingress record is created with the first balancer of the shoot and
	// points at its address, which is the wildcard every service of the cluster
	// is published under.
	if len(s.loadBalancers) == 1 {
		s.ingressRecord = &recordSet{
			id:         g.noiseIDs.nextUUID(),
			name:       "*.ingress." + s.name + "." + s.owner.zoneName,
			recordType: "A",
			records:    []string{lb.fip.address},
		}
		g.createRecordSet(s.owner, s.ingressRecord, t.Add(ingressRecordLag))
	}

	// The https listener is the second of the catalog, so a balancer that gets
	// two of them is the one that terminates TLS. Its certificate goes into
	// barbican as a secret with the private key and a container that holds the
	// two together.
	if listeners >= 2 {
		lb.secretID, lb.containerID = g.noiseIDs.nextUUID(), g.noiseIDs.nextUUID()
		g.barbicanCall(tenant, t.Add(certificateLag), "POST", "/v1/secrets",
			secretsType, lb.secretID)
		g.barbicanCall(tenant, t.Add(certificateLag+2*time.Second), "POST", "/v1/containers",
			containersType, lb.containerID)
	}

	g.emit(t.Add(span(g.shape, 60*time.Second, 300*time.Second)),
		"octavia.loadbalancer.update.end", "", lb.id, tenant,
		loadBalancerPayload(tenant, s, lb, true),
		alive(stateActive, loadBalancerSizeOf(len(lb.listenerIDs), len(lb.poolIDs))))
}

// tearDown deletes a shoot in the order Gardener does, which is the order its
// resources depend on each other in: the services go first, address before
// balancer and the VIP port and the certificate after it, then the workers,
// then the claims they held, and the infrastructure underneath them all last.
// Nothing of the shoot is emitted after its network is gone. A claim is
// attached unless the shoot hibernated, so its delete carries the detach cinder
// sends three seconds before it.
func (g *generator) tearDown(s *shoot) {
	tenant := s.owner.tenant
	t := s.deletedAt

	g.authenticate(tenant, s.deletedAt.Add(-authenticateLead))

	for _, lb := range s.loadBalancers {
		g.emit(t, "floatingip.delete.end", networkPublisher, lb.fip.id, tenant,
			floatingIPDeletePayload(lb.fip), deleted)
		t = t.Add(span(g.shape, 5*time.Second, 15*time.Second))
		g.emit(t, "octavia.loadbalancer.delete.end", "", lb.id, tenant,
			loadBalancerPayload(tenant, s, lb, false), deleted)
		// The VIP port goes with the balancer that held it.
		g.noise(t.Add(time.Second), "port.delete.start", networkPublisher, lb.vipPortID, tenant,
			neutronDeletePayload("port", lb.vipPortID))
		g.noise(t.Add(2*time.Second), "port.delete.end", networkPublisher, lb.vipPortID, tenant,
			neutronDeletePayload("port", lb.vipPortID))
		// The container goes before the secret it holds, the way a certificate is
		// given back: the container refers to the secret, and barbican refuses a
		// secret another resource still names.
		if lb.containerID != "" {
			g.barbicanCall(tenant, t.Add(3*time.Second), "DELETE", "/v1/containers/"+lb.containerID,
				containersType, lb.containerID)
			g.barbicanCall(tenant, t.Add(5*time.Second), "DELETE", "/v1/secrets/"+lb.secretID,
				secretsType, lb.secretID)
		}
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
	g.tearDownInfrastructure(s, t)
	s.claims = nil
}

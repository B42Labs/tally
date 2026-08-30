package simulator

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"
)

// The noise is what a real bus carries beside the notifications the collector
// bills: the scheduler's placement decisions, the ports of every server, the
// networks, subnets, routers and security groups underneath them, the
// keypairs, keystone's authentications, designate's zones and record sets,
// barbican's audit records, the attach and the detach of a volume, and the
// .start half of every step that has one. The collector receives every one of
// them and counts it as skipped, so a month without them is a month whose skip
// counters stay at zero and whose ratio of billable to received says nothing.
//
// Three rules hold for everything this file renders.
//
// Noise is never billable. The collector's mapping claims nothing for any of
// these types, and WriteEvents refuses a transition that is marked billable and
// mapped to nothing.
//
// Noise takes no draw from the shape stream and no identifier from the
// identifier stream. Every noise instant is a fixed offset from the billable
// instant it belongs to, and every identifier a noise resource needs comes from
// the noise identifier stream, its message id included. That is what keeps the
// billable transitions of a seed, a period, and a cloud the ones they would be
// without the catalogue: same instants, same ids, same payloads, same message
// ids.
//
// Noise is rendered by the helpers of this file and by nothing else. The
// billable workload calls into them, so what a month is billed for and what it
// merely carries stay two things a reader can tell apart.

// The fixed distances of every noise instant from the billable instant it
// belongs to. Each of them is a whole number of seconds, and the ones that
// gather around a single billable instant differ from each other, so no two
// notifications about one resource fall in the same second.
//
// The infrastructure of a shoot is the one sequence without a constant of its
// own: it runs one transition per second, from 1 to 16 seconds after the
// shoot's creation instant. The daily compute.instance.exists audits need no
// constant either: they sit at the midnight itself, pushed on by whole seconds
// for as long as the instance already reports a transition at that second.
//
// Two distances the billable month already states are reused rather than
// written again here: volumeProvision (render.go) is how long before its create
// a volume was requested, and volumeDeleteLead (workload.go) is the gap between
// an instance delete and the delete of the volumes it held.
const (
	// schedulerLead is where a boot begins: the
	// scheduler.select_destinations.start, with its .end a second later.
	schedulerLead = 25 * time.Second
	// createStartLead is the compute.instance.create.start of a boot.
	createStartLead = 20 * time.Second
	// portCreateLead is the port.create.start of the server's port, with its
	// .end a second later.
	portCreateLead = 17 * time.Second
	// portBindLead is the port.update.start that binds the port to the host the
	// server runs on, with its .end a second later.
	portBindLead = 12 * time.Second
	// instanceDeleteLead is where the pre-delete sequence begins: the update at
	// -5s, the exists at -4s, the delete.start at -3s, the shutdown.start at
	// -2s, and the shutdown.end at -1s.
	instanceDeleteLead = 5 * time.Second
	// portDeleteLag is the port.delete.start after an instance delete, with its
	// .end a second later.
	portDeleteLag = 1 * time.Second
	// detachLag is the volume.detach.start after an instance delete.
	detachLag = 3 * time.Second
	// stepLead is the .start half of a paired step, before its .end.
	stepLead = 5 * time.Second
	// attachLag is the volume.attach.start after a volume create, or after the
	// last worker of a wake-up.
	attachLag = 1 * time.Second
	// volumeDetachLead is the volume.detach.start before a volume delete.
	volumeDetachLead = 3 * time.Second
	// volumeDeleteStartLead is the volume.delete.start of that same delete.
	volumeDeleteStartLead = 1 * time.Second
	// transferLead is the volume.transfer.accept.start.
	transferLead = 1 * time.Second
	// imagePrepareLead is the image.prepare before an upload, and
	// imageActivateLag the image.activate after it.
	imagePrepareLead = 1 * time.Second
	imageActivateLag = 1 * time.Second
	// authenticateLead is the identity.authenticate before an action somebody
	// or a controller starts.
	authenticateLead = 2 * time.Second
	// vipPortLead is the port.create.start of a load balancer's VIP port, with
	// its .end a second later.
	vipPortLead = 2 * time.Second
	// ingressRecordLag is the dns.recordset.create of the ingress record, which
	// needs the address of the first load balancer.
	ingressRecordLag = 12 * time.Second
	// certificateLag is where the four barbican records begin, one second
	// apart.
	certificateLag = 20 * time.Second
)

// The CADF target types barbican reports its resources under. A secret holds
// the private key of a load balancer's certificate, and a container holds the
// certificate that goes with it.
const (
	secretsType    = "service/security/keymanager/secrets"
	containersType = "service/security/keymanager/containers"
)

// updateLeads are the distances of the three compute.instance.update of a boot
// before the create: networking, block_device_mapping, and spawning, in that
// order.
var updateLeads = [3]time.Duration{15 * time.Second, 10 * time.Second, 5 * time.Second}

// buildingTasks are the task states a server passes through while it is built.
// The three updates of a boot each report the move from one of them to the
// next, which is why the four names yield three notifications.
var buildingTasks = [4]string{"scheduling", "networking", "block_device_mapping", "spawning"}

// noiseIdentifiers seeds the noise identifier stream, the third generator of a
// month. It carries the cloud and the billing month the way the identifier
// stream does, so another cloud or another month renames the announced
// resources as well, and the prefix is what keeps the two streams from drawing
// one sequence twice.
func noiseIdentifiers(seed uint64, cloud string, month time.Time) idReader {
	return idReader{src: rand.New(rand.NewPCG(seed, identifierSalt("noise\x00"+cloud, month)))}
}

// noise records one transition the collector does not bill. It is emit with
// Billable false: the mapping claims nothing for any type of this catalogue,
// and a transition that says otherwise fails the write of the expected events.
// It is also marked as the catalogue's, which is what sends its message id to
// the noise stream.
func (g *generator) noise(at time.Time, eventType, publisherID, resourceID string,
	requester *project, payload map[string]any,
) {
	g.schedule = append(g.schedule, Transition{
		At:          at,
		EventType:   eventType,
		Exchange:    exchangeFor(eventType),
		Billable:    false,
		noise:       true,
		Workload:    g.workload,
		PublisherID: publisherID,
		ProjectID:   requester.id,
		UserID:      requester.userID,
		ResourceID:  resourceID,
		Payload:     payload,
	})
}

// newPort builds the port of a device on a network, taking the network's subnet
// and security group with it because neutron reports both on every port.
//
// The MAC address is derived from the port id rather than drawn: it is the
// fa:16:3e prefix every OpenStack port carries followed by three bytes of the
// id, so a port costs one identifier instead of two.
func newPort(net *network, id, address, deviceID, deviceOwner, host string) *port {
	return &port{
		id:              id,
		networkID:       net.id,
		subnetID:        net.subnetID,
		securityGroupID: net.securityGroupID,
		address:         address,
		macAddress:      "fa:16:3e:" + id[0:2] + ":" + id[2:4] + ":" + id[4:6],
		deviceID:        deviceID,
		deviceOwner:     deviceOwner,
		host:            host,
	}
}

// createInstance renders one boot: everything nova and neutron put on the bus
// around the create the collector bills, and the create itself.
//
// The order is the one a deployment sends them in. The scheduler picks the host
// and answers, nova announces the server it is about to build and reports its
// progress through three updates, and neutron creates the port the server holds
// its address on and binds it to the host in between. The create the collector
// books comes last, because it is what the server is billed from.
//
// The port is the server's port on the network it is created on: a classic or a
// CI tenant's own network, and a shoot's network for a worker. It is built here
// because a server that never existed has none, and it is kept on the instance
// so the delete can release it.
func (g *generator) createInstance(p *project, inst *instance, net *network, t time.Time) {
	inst.port = newPort(net, g.noiseIDs.nextUUID(), net.nextAddress(), inst.id, "compute:nova", inst.host)

	g.noise(t.Add(-schedulerLead), "scheduler.select_destinations.start", schedulerPublisher,
		inst.id, p, schedulerPayload(p, inst))
	g.noise(t.Add(-schedulerLead+time.Second), "scheduler.select_destinations.end", schedulerPublisher,
		inst.id, p, schedulerPayload(p, inst))
	g.noise(t.Add(-createStartLead), "compute.instance.create.start", computePublisher(inst),
		inst.id, p, instanceCreateStartPayload(p, inst, g.cloud))
	for i, lead := range updateLeads {
		u := t.Add(-lead)
		g.noise(u, "compute.instance.update", computePublisher(inst), inst.id, p,
			instanceUpdatePayload(p, inst, "building", buildingTasks[i], buildingTasks[i+1], at(u, 0, 0), u))
	}

	g.noise(t.Add(-portCreateLead), "port.create.start", networkPublisher, inst.port.id, p,
		portRequestPayload(p, inst.port))
	g.noise(t.Add(-portCreateLead+time.Second), "port.create.end", networkPublisher, inst.port.id, p,
		portPayload(p, inst.port, false))
	g.noise(t.Add(-portBindLead), "port.update.start", networkPublisher, inst.port.id, p,
		portBindingPayload(inst.port))
	g.noise(t.Add(-portBindLead+time.Second), "port.update.end", networkPublisher, inst.port.id, p,
		portPayload(p, inst.port, true))

	g.emit(t, "compute.instance.create.end", computePublisher(inst), inst.id, p,
		instanceCreatePayload(p, inst, g.cloud), alive(stateActive, instanceSizeOf(inst.flavor)))
}

// destroyInstance renders one delete: the sequence nova sends before it, the
// delete the collector bills, and the port neutron releases after it.
//
// Nova announces the delete as an update to the deleting task, audits what the
// server did on the day up to here, and then tears it down, which is a delete
// start and the shutdown around it. The audit is the pre-delete existence
// notification of a real deployment: a server that goes reports what it used
// before it is gone. Cinder detaches the volumes the server still held once it
// is gone, so a volume that outlives its server is available from there on.
func (g *generator) destroyInstance(p *project, inst *instance, t time.Time) {
	begin := t.Add(-instanceDeleteLead)
	g.noise(begin, "compute.instance.update", computePublisher(inst), inst.id, p,
		instanceUpdatePayload(p, inst, "active", nil, "deleting", at(t, 0, 0), t))
	g.noise(begin.Add(time.Second), "compute.instance.exists", computePublisher(inst), inst.id, p,
		instanceExistsPayload(instanceCreatePayload(p, inst, g.cloud), at(t, 0, 0), t))
	g.noise(begin.Add(2*time.Second), "compute.instance.delete.start", computePublisher(inst),
		inst.id, p, instanceShelvePayload(p, inst, "active"))
	g.noise(begin.Add(3*time.Second), "compute.instance.shutdown.start", computePublisher(inst),
		inst.id, p, instanceShelvePayload(p, inst, "active"))
	g.noise(begin.Add(4*time.Second), "compute.instance.shutdown.end", computePublisher(inst),
		inst.id, p, instanceShelvePayload(p, inst, "active"))

	g.emit(t, "compute.instance.delete.end", computePublisher(inst), inst.id, p,
		instanceDeletePayload(p, inst, t), deleted)

	// Nothing in the month creates a server without a port, so the branch is
	// what keeps the helper from dereferencing a nil rather than a shape a
	// workload produces.
	if inst.port != nil {
		g.noise(t.Add(portDeleteLag), "port.delete.start", networkPublisher, inst.port.id, p,
			neutronDeletePayload("port", inst.port.id))
		g.noise(t.Add(portDeleteLag+time.Second), "port.delete.end", networkPublisher, inst.port.id, p,
			neutronDeletePayload("port", inst.port.id))
	}

	for _, vol := range inst.volumes {
		if vol.attached {
			g.detach(p, vol, t.Add(detachLag))
		}
	}
	if inst.bootVolume != nil && inst.bootVolume.attached {
		g.detach(p, inst.bootVolume, t.Add(detachLag))
	}
	inst.deletedAt = t
}

// createVolume renders one volume create: the request cinder accepted and the
// create the collector bills.
//
// The start is the created_at the billable payload already reports, eight
// seconds before the volume is ready. The caller sets vol.createdAt before it
// calls, because both payloads are dated from it.
func (g *generator) createVolume(p *project, vol *volume, t time.Time) {
	g.noise(t.Add(-volumeProvision), "volume.create.start", volumePublisher, vol.id, p,
		volumeCreateStartPayload(p, vol))

	// The mapping books a create under a fixed available, and the attach that follows is noise.
	g.emit(t, "volume.create.end", volumePublisher, vol.id, p, volumeCreatePayload(p, vol),
		alive(stateAvailable, volumeSizeOf(vol)))
}

// attach connects a volume to a server. Cinder announces the attach with the
// volume connected to nothing yet and reports it in use a second later, holding
// the attachment record it handed out until the detach gives it back.
//
// The volume is attached from here on, which is what puts the in-use status
// into every billable notification about it: a payload rendered after this
// point reports the volume the way cinder reports one a server holds.
//
// The server may be nil. The attachment then names no instance and no host, and
// the volume is attached all the same, so a billable payload rendered afterwards
// reports in-use exactly as it does for a volume a server holds.
func (g *generator) attach(p *project, vol *volume, inst *instance, t time.Time) {
	vol.attachment = attachmentOf(g.noiseIDs.nextUUID(), vol, inst, t)

	g.noise(t, "volume.attach.start", volumePublisher, vol.id, p,
		volumeAttachmentPayload(p, vol, "attaching", nil))
	g.noise(t.Add(time.Second), "volume.attach.end", volumePublisher, vol.id, p,
		volumeAttachmentPayload(p, vol, "in-use", vol.attachment))

	vol.attached = true
}

// detach disconnects a volume from the server that held it. The .start half
// repeats the attachment of that server one last time and the .end carries
// none, which is the volume as cinder reports it from here on: available, and
// connected to nothing.
func (g *generator) detach(p *project, vol *volume, t time.Time) {
	g.noise(t, "volume.detach.start", volumePublisher, vol.id, p,
		volumeAttachmentPayload(p, vol, "detaching", vol.attachment))
	g.noise(t.Add(time.Second), "volume.detach.end", volumePublisher, vol.id, p,
		volumeAttachmentPayload(p, vol, "available", nil))

	vol.attached = false
	vol.attachment = nil
}

// deleteVolume renders one volume delete: the detach of a volume a server still
// holds, the delete cinder announces, and the delete the collector bills. A
// volume whose server is already gone was detached with it and is deleted
// without a second one.
func (g *generator) deleteVolume(p *project, vol *volume, t time.Time) {
	if vol.attached {
		g.detach(p, vol, t.Add(-volumeDetachLead))
	}
	g.noise(t.Add(-volumeDeleteStartLead), "volume.delete.start", volumePublisher, vol.id, p,
		volumeStatusPayload(p, vol, "deleting"))

	g.emit(t, "volume.delete.end", volumePublisher, vol.id, p, volumeDeletePayload(p, vol, t),
		deleted)
}

// uploadImage renders one image upload: glance taking the bits in, the upload
// the collector bills, and the activation a second later.
//
// The prepare is the image while its content is still arriving, which is the
// one notification about it that carries neither a size nor a checksum. The
// activate repeats the payload of the upload unchanged, because nothing about
// the image changes when glance flips it to active.
func (g *generator) uploadImage(p *project, img *image, uploadedAt time.Time) {
	g.noise(uploadedAt.Add(-imagePrepareLead), "image.prepare", imagePublisher, img.id, p,
		imagePreparePayload(p, img))

	g.emit(uploadedAt, "image.upload", imagePublisher, img.id, p,
		imageUploadPayload(p, img, uploadedAt), alive(stateActive, imageSizeOf(img)))

	g.noise(uploadedAt.Add(imageActivateLag), "image.activate", imagePublisher, img.id, p,
		imageUploadPayload(p, img, uploadedAt))
}

// buildNetwork renders the network a tenant works on: neutron's network, the
// subnet inside it, the router out of it, and the interface that puts the
// router on the subnet.
//
// The CI tenant's network at the month start and every shoot's network go
// through here. The classic tenants' networks pre-exist the month, and neutron
// announces nothing about a resource that was there before the first
// transition.
func (g *generator) buildNetwork(p *project, net *network, t time.Time) {
	// The port the router holds its interface on. It is kept on the network so
	// that the delete of the interface names the port the create named.
	net.routerPortID = g.noiseIDs.nextUUID()

	g.noise(t, "network.create.start", networkPublisher, net.id, p, networkRequestPayload(net))
	g.noise(t.Add(time.Second), "network.create.end", networkPublisher, net.id, p,
		networkPayload(p, net))
	g.noise(t.Add(2*time.Second), "subnet.create.start", networkPublisher, net.subnetID, p,
		subnetRequestPayload(net))
	g.noise(t.Add(3*time.Second), "subnet.create.end", networkPublisher, net.subnetID, p,
		subnetPayload(p, net))
	g.noise(t.Add(4*time.Second), "router.create.start", networkPublisher, net.routerID, p,
		routerRequestPayload(net, g.networkID))
	g.noise(t.Add(5*time.Second), "router.create.end", networkPublisher, net.routerID, p,
		routerPayload(p, net, g.networkID))
	g.noise(t.Add(6*time.Second), "router.interface.create", networkPublisher, net.routerID, p,
		routerInterfacePayload(p, net, net.routerPortID))
}

// shootInfrastructure renders everything Gardener creates before the first
// worker of a shoot boots. It is one reconciliation, so it takes one token, and
// what follows that token runs a second apart.
//
// The seventeen transitions of it, from two seconds before the shoot's creation
// instant to sixteen after it: the authentication, the network, the subnet, the
// router and its interface, the security group with the two rules of a worker
// pool, the keypair the workers are reachable under, and the record set the API
// server answers on.
func (g *generator) shootInfrastructure(s *shoot, t time.Time) {
	p := s.owner.tenant
	// The network the shoot's servers hold their addresses on, built from the
	// ids the shoot was named with. Its range is the one the shoot's VIP
	// addresses already lie in.
	s.network = &network{
		id:              s.networkID,
		subnetID:        s.subnetID,
		routerID:        s.routerID,
		securityGroupID: s.securityGroupID,
		name:            s.technicalID,
		cidr:            fmt.Sprintf("10.250.%d.0/24", s.index),
	}

	g.authenticate(p, t.Add(-authenticateLead))
	g.buildNetwork(p, s.network, t.Add(time.Second))

	g.noise(t.Add(8*time.Second), "security_group.create.start", networkPublisher, s.securityGroupID, p,
		securityGroupRequestPayload(s))
	g.noise(t.Add(9*time.Second), "security_group.create.end", networkPublisher, s.securityGroupID, p,
		securityGroupPayload(p, s))
	for i := range 2 {
		id := g.noiseIDs.nextUUID()
		s.securityGroupRuleIDs = append(s.securityGroupRuleIDs, id)
		g.noise(t.Add(time.Duration(10+2*i)*time.Second), "security_group_rule.create.start",
			networkPublisher, id, p, securityGroupRulePayload(p, s, i, ""))
		g.noise(t.Add(time.Duration(11+2*i)*time.Second), "security_group_rule.create.end",
			networkPublisher, id, p, securityGroupRulePayload(p, s, i, id))
	}

	// A keypair has no id, so the resource it is reported under is the user that
	// holds it and the name it was imported under. The user id is salted with
	// the cloud and the month the way every other identifier is, which keeps two
	// clouds' keypairs apart.
	keypair := p.userID + ":" + s.keypairName
	g.noise(t.Add(14*time.Second), "keypair.import.start", apiPublisher, keypair, p,
		keypairPayload(p, s.keypairName))
	g.noise(t.Add(15*time.Second), "keypair.import.end", apiPublisher, keypair, p,
		keypairPayload(p, s.keypairName))

	// The API server of a shoot runs in Gardener's seed, which this world does
	// not simulate, so the address the record points at is a placeholder from
	// the RFC 5737 range beside the one the floating addresses come from.
	s.apiRecord = &recordSet{
		id:         g.noiseIDs.nextUUID(),
		name:       "api." + s.name + "." + s.owner.zoneName,
		recordType: "A",
		records:    []string{fmt.Sprintf("198.51.100.%d", s.index)},
	}
	g.createRecordSet(s.owner, s.apiRecord, t.Add(16*time.Second))
}

// tearDownInfrastructure gives back what shootInfrastructure took, in the order
// the resources depend on each other: the records the cluster was reached
// under, the keypair, the security group, the router's interface, the router,
// the subnet, and the network. Nothing of the shoot is emitted after the
// network.delete.end.
//
// The cursor moves a second per transition, which is what keeps the two halves
// of one delete out of one second.
func (g *generator) tearDownInfrastructure(s *shoot, t time.Time) {
	p := s.owner.tenant
	// The cursor of the sequence. The two record sets advance it themselves,
	// because designate renders them through a helper of its own.
	step := func(eventType, publisherID, resourceID string, payload map[string]any) {
		g.noise(t, eventType, publisherID, resourceID, p, payload)
		t = t.Add(time.Second)
	}

	g.deleteRecordSet(s.owner, s.apiRecord, t)
	t = t.Add(time.Second)
	// A shoot that never published a service has no ingress record to give back.
	if s.ingressRecord != nil {
		g.deleteRecordSet(s.owner, s.ingressRecord, t)
		t = t.Add(time.Second)
	}

	keypair := p.userID + ":" + s.keypairName
	step("keypair.delete.start", apiPublisher, keypair, keypairPayload(p, s.keypairName))
	step("keypair.delete.end", apiPublisher, keypair, keypairPayload(p, s.keypairName))

	step("security_group.delete.start", networkPublisher, s.securityGroupID,
		neutronDeletePayload("security_group", s.securityGroupID))
	step("security_group.delete.end", networkPublisher, s.securityGroupID,
		neutronDeletePayload("security_group", s.securityGroupID))

	step("router.interface.delete", networkPublisher, s.routerID,
		routerInterfacePayload(p, s.network, s.network.routerPortID))
	step("router.delete.start", networkPublisher, s.routerID,
		neutronDeletePayload("router", s.routerID))
	step("router.delete.end", networkPublisher, s.routerID,
		neutronDeletePayload("router", s.routerID))

	step("subnet.delete.start", networkPublisher, s.subnetID,
		neutronDeletePayload("subnet", s.subnetID))
	step("subnet.delete.end", networkPublisher, s.subnetID,
		neutronDeletePayload("subnet", s.subnetID))

	step("network.delete.start", networkPublisher, s.networkID,
		neutronDeletePayload("network", s.networkID))
	step("network.delete.end", networkPublisher, s.networkID,
		neutronDeletePayload("network", s.networkID))
}

// announceTenant is what keystone sends when a tenant is created: the project
// and the user that works in it, a second apart.
func (g *generator) announceTenant(p *project, t time.Time) {
	g.noise(t, "identity.project.created", identityPublisher, p.id, p, identityPayload(p.id))
	g.noise(t.Add(time.Second), "identity.user.created", identityPublisher, p.userID, p,
		identityPayload(p.userID))
}

// authenticate is the token somebody or a controller takes before it acts.
//
// The resource of the transition is the CADF record's own id and not the user
// it was issued for. A user authenticates several times a day, two of those may
// fall into one second, and a record id is what keeps the rule that two
// notifications about one resource are a second apart trivially true.
func (g *generator) authenticate(p *project, t time.Time) {
	id := g.noiseIDs.nextUUID()
	g.noise(t, "identity.authenticate", identityPublisher, id, p, authenticatePayload(p, id, t))
}

// createZone publishes the designate zone a Gardener project's records live in.
// The zones are never deleted: a project outlives every shoot it holds, and the
// zone is what the records of the next one are created under.
func (g *generator) createZone(gp *gardenerProject, t time.Time) {
	g.noise(t, "dns.zone.create", dnsPublisher, gp.zoneID, gp.tenant,
		zonePayload(gp.tenant, gp, t))
}

// createRecordSet publishes one record set into a project's zone.
func (g *generator) createRecordSet(gp *gardenerProject, rs *recordSet, t time.Time) {
	g.noise(t, "dns.recordset.create", dnsPublisher, rs.id, gp.tenant,
		recordSetPayload(gp.tenant, gp, rs, "CREATE", t))
}

// deleteRecordSet withdraws one record set from a project's zone.
func (g *generator) deleteRecordSet(gp *gardenerProject, rs *recordSet, t time.Time) {
	g.noise(t, "dns.recordset.delete", dnsPublisher, rs.id, gp.tenant,
		recordSetPayload(gp.tenant, gp, rs, "DELETE", t))
}

// barbicanCall renders one call against barbican's API as the two records its
// audit middleware sends: the request when it arrives, and the response a
// second later. Each of the two is its own resource, because a call is one
// thing that happened and not two steps of a resource's life.
//
// The method is what the action and the status of the response are read off: a
// POST creates the secret or the container the path names and is answered with
// a 201, and a DELETE gives it back and is answered with a 204.
func (g *generator) barbicanCall(p *project, t time.Time,
	method, path, targetType, targetID string,
) {
	reqID, respID := g.noiseIDs.nextUUID(), g.noiseIDs.nextUUID()
	action, code := "create", 201
	if method == "DELETE" {
		action, code = "delete", 204
	}

	g.noise(t, "audit.http.request", barbicanPublisher, reqID, p,
		auditPayload(p, g.cloud, reqID, action, "pending", path, targetType, targetID, 0, t))
	response := t.Add(time.Second)
	g.noise(response, "audit.http.response", barbicanPublisher, respID, p,
		auditPayload(p, g.cloud, respID, action, "success", path, targetType, targetID, code, response))
}

// auditFlavorMembers are the members a resize carries over into the audits that
// follow it. They are what nova's resize payload reports of the flavor the
// server moved to, and the disk of an audit is summed from two of them.
//
// The flavor's uuid is not among them, because nova's resize payload does not
// carry one: an audit names the flavor under instance_flavor_id all the same,
// and nova reads that from its own catalog. flavorByTypeID stands in for that
// catalog, so an audit after a resize does not report the new flavor's name
// beside the boot flavor's uuid.
var auditFlavorMembers = [6]string{
	"vcpus", "memory_mb", "root_gb", "ephemeral_gb", "instance_type", "instance_type_id",
}

// auditEntry is one instance under audit: who reports it, which project it runs
// in, and the payload its audits repeat. base starts as the create and is
// carried forward from there, because nova audits the server as it stands at
// the end of the day and not as it was booted.
type auditEntry struct {
	id        string
	publisher string
	projectID string
	userID    string
	workload  string
	base      map[string]any
	// deletedAt is when the instance went, and the zero time while it is still
	// there. The instance is audited once more after it, for the part of the day
	// it was still running, and is dropped afterwards.
	deletedAt time.Time
}

// audits renders nova's periodic existence audit: every instance that existed
// during a calendar day of the month is reported at the following midnight with
// a compute.instance.exists over that day.
//
// The period is the day, which is what a deployment sets
// instance_usage_audit_period to when it wants daily records. The default nova
// ships is the month, and a monthly period would put no audit inside the
// simulated month at all, because the first one falls on the midnight that ends
// it. The hour would put twenty-four times as many lines on the bus for the
// same single type.
//
// The audits are the one part of the catalogue that is not rendered where the
// billable transition is. Which instances existed on a day is known only once
// every workload has run, so this is a pass over the finished schedule.
//
// An audit sits at the midnight itself. When the instance already reports a
// transition at that second, the audit is pushed on by whole seconds until it
// finds a free one, which keeps two notifications about one resource a second
// apart. What the audit repeats is the instance as it stands: a resize moves
// the flavor over, and every .end moves the state over, so the audits after a
// resize report the flavor the server runs on from then on. A deleted instance
// is audited one last time, at the midnight that follows its delete.
func (g *generator) audits() {
	sorted := slices.Clone(g.schedule)
	slices.SortStableFunc(sorted, func(a, b Transition) int { return a.At.Compare(b.At) })

	// Every instant every resource of the month already reports. It is what an
	// audit is pushed past, and it is built from the whole schedule because the
	// second a midnight falls on may be taken by any of the transitions there.
	type reported struct {
		resourceID string
		at         time.Time
	}
	occupied := make(map[reported]struct{}, len(sorted))
	for _, tr := range sorted {
		occupied[reported{tr.ResourceID, tr.At}] = struct{}{}
	}

	// The instances under audit, in the order they first appeared, and the same
	// entries by id. The slice is what the flush walks: a map walk would order
	// the audits of one midnight differently from run to run, and a month whose
	// order depends on the run is no longer the seed's.
	var fleet []*auditEntry
	byID := make(map[string]*auditEntry)

	flush := func(midnight time.Time) {
		for _, entry := range fleet {
			// An instance that went before the day this audit covers has nothing
			// left to report.
			if !entry.deletedAt.IsZero() && !entry.deletedAt.After(midnight.Add(-day)) {
				continue
			}
			at := midnight
			for _, taken := occupied[reported{entry.id, at}]; taken; _, taken = occupied[reported{entry.id, at}] {
				at = at.Add(time.Second)
			}
			// The entry carries the ids of the instance and not the project it
			// runs in, which is why the transition is written out here instead of
			// through g.noise, which takes a *project.
			g.schedule = append(g.schedule, Transition{
				At:          at,
				EventType:   "compute.instance.exists",
				Exchange:    exchangeFor("compute.instance.exists"),
				Billable:    false,
				noise:       true,
				Workload:    entry.workload,
				PublisherID: entry.publisher,
				ProjectID:   entry.projectID,
				UserID:      entry.userID,
				ResourceID:  entry.id,
				Payload:     instanceExistsPayload(entry.base, midnight.Add(-day), midnight),
			})
		}

		// An instance created and deleted between two midnights is audited once,
		// at the midnight that follows, and is dropped here.
		fleet = slices.DeleteFunc(fleet, func(entry *auditEntry) bool {
			if entry.deletedAt.IsZero() {
				return false
			}
			delete(byID, entry.id)
			return true
		})
	}

	midnight := g.from.AddDate(0, 0, 1)
	for _, tr := range sorted {
		// Every midnight the transition has passed is audited before the
		// transition is folded in, so a day reports the instances as they stood
		// when it ended.
		for midnight.Before(g.to) && !tr.At.Before(midnight) {
			flush(midnight)
			midnight = midnight.Add(day)
		}

		if tr.EventType == "compute.instance.create.end" {
			entry := &auditEntry{
				id:        tr.ResourceID,
				publisher: tr.PublisherID,
				projectID: tr.ProjectID,
				userID:    tr.UserID,
				workload:  tr.Workload,
				base:      maps.Clone(tr.Payload),
			}
			fleet = append(fleet, entry)
			byID[entry.id] = entry
			continue
		}

		// An id without a create.end behind it is nothing this pass audits: the
		// resources of every other service, and the halves of a boot nova sends
		// before the create. Only the .end of a step carries the instance
		// forward, because that is where a step reports what it left behind.
		entry, known := byID[tr.ResourceID]
		if !known || !strings.HasPrefix(tr.EventType, "compute.instance.") ||
			!strings.HasSuffix(tr.EventType, ".end") {
			continue
		}

		switch tr.EventType {
		case "compute.instance.resize.end", "compute.instance.finish_resize.end":
			for _, member := range auditFlavorMembers {
				if value, ok := tr.Payload[member]; ok {
					entry.base[member] = value
				}
			}
			// The resize payload reports the two disks and not their sum, so the
			// member an audit carries is summed here. A payload without them
			// leaves the disk of the audit at what it was.
			root, rootOK := tr.Payload["root_gb"].(int)
			ephemeral, ephemeralOK := tr.Payload["ephemeral_gb"].(int)
			if rootOK && ephemeralOK {
				entry.base["disk_gb"] = root + ephemeral
			}
			// The uuid of the flavor comes from the catalog rather than from the
			// payload. A type id the catalog does not hold leaves the member at
			// what it was, which is a resize the month never renders.
			if typeID, ok := tr.Payload["instance_type_id"].(int); ok {
				if moved, found := flavorByTypeID(typeID); found {
					entry.base["instance_flavor_id"] = moved.flavorID
				}
			}
		case "compute.instance.delete.end":
			entry.deletedAt = tr.At
			entry.base["state"] = "deleted"
		default:
			if state, ok := tr.Payload["state"].(string); ok {
				entry.base["state"] = state
			}
		}
	}

	// Every midnight left of the month, up to but not including the one that
	// ends it: an audit at the end of the period would fall into the month that
	// follows.
	for midnight.Before(g.to) {
		flush(midnight)
		midnight = midnight.Add(day)
	}
}

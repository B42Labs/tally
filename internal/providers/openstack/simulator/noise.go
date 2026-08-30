package simulator

import (
	"math/rand/v2"
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
// shoot's creation instant. The audit records of a tenant sit at the midnight
// itself, which is a lag of zero and needs no constant either.
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
		instanceCreatePayload(p, inst, g.cloud))
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
		instanceDeletePayload(p, inst, t))

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

	g.emit(t, "volume.create.end", volumePublisher, vol.id, p, volumeCreatePayload(p, vol))
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

	g.emit(t, "volume.delete.end", volumePublisher, vol.id, p, volumeDeletePayload(p, vol, t))
}

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

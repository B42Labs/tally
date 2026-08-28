package simulator

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/b42labs/tally/internal/engine/period"
)

// Transition is one lifecycle step of the simulated cloud: what happened, when,
// and everything the notification about it needs. It is the simulator's own
// form, one level above the wire, so that a month can be sorted, counted, and
// asserted on before any of it is serialized.
type Transition struct {
	// At is the virtual instant the notification is published at. It lies
	// inside the generated month.
	At time.Time
	// EventType is the type the emitting service would use, such as
	// "compute.instance.create.end".
	EventType string
	// Exchange is the notification exchange the type belongs on. A deployment
	// gives each service its own, and the collector binds its queue to all four.
	Exchange string
	// Billable reports whether the collector's mapping records an event for this
	// notification. It is false only for the image.create that precedes an
	// upload: that one carries no size yet, and the mapping skips it on purpose.
	Billable bool
	// MessageID is the oslo message id, which the Reporting API deduplicates on.
	// It is drawn after the month is sorted, so the ids run in the order the
	// notifications are published.
	MessageID string
	// PublisherID names the service instance that emitted the notification.
	PublisherID string
	// ProjectID is the project the request ran in. It becomes both
	// _context_project_id and _context_tenant_id, since services differ in which
	// of the two they set and the collector reads either.
	ProjectID string
	// UserID is the user the request ran as, which becomes _context_user_id.
	UserID string
	// ResourceID is the resource the transition is about. The rendered payload
	// carries it under whatever member the emitting service names it in, so this
	// field is what a test holds the mapping's result against.
	ResourceID string
	// Payload is the service's description of the resource, ready to be
	// marshalled as the notification's payload.
	Payload map[string]any
}

// Schedule is one month of transitions in publication order.
type Schedule []Transition

// Billable returns the transitions the collector records an event for. It is
// what a run counts its expected events by: the rest reaches the collector and
// is skipped there, so counting the whole schedule would overstate the month.
func (s Schedule) Billable() []Transition {
	billable := make([]Transition, 0, len(s))
	for _, transition := range s {
		if transition.Billable {
			billable = append(billable, transition)
		}
	}
	return billable
}

// routingKey is the topic every notification is published under. oslo puts the
// topic in the routing key itself rather than as a prefix of one, and it is the
// key the collector binds its queue with.
const routingKey = "notifications.info"

// imageCreateType is the one type the simulator emits that the mapping skips.
// Glance announces an image before its bits are uploaded, so that first
// notification has no size to bill, and the upload that follows is what the
// image is booked from.
const imageCreateType = "image.create"

// exchangeFor names the exchange a type is published on. The four are the
// service exchanges of nova, cinder, neutron, and glance. A type outside them
// is one this package does not generate, and it is reported as the empty
// exchange rather than guessed at, because a wrong exchange is a notification
// no bound queue receives.
func exchangeFor(eventType string) string {
	switch {
	case strings.HasPrefix(eventType, "compute."):
		return "nova"
	case strings.HasPrefix(eventType, "volume."):
		return "cinder"
	case strings.HasPrefix(eventType, "floatingip."):
		return "neutron"
	case strings.HasPrefix(eventType, "image."):
		return "glance"
	default:
		return ""
	}
}

// The size of the simulated cloud. It is small on purpose: what a run has to
// cover is every notification type and every shape of resource life, not a
// realistic tenant count.
const (
	projectCount        = 3
	imagesPerProject    = 2
	instancesPerProject = 4
	// addressPoolSize is how many host addresses the floating network holds. A
	// /24 with its network and broadcast addresses left out is 254, and drawing
	// the month's addresses as a permutation of it keeps any two of them apart.
	addressPoolSize = 254
)

// day is the unit most of the workload's spans are stated in.
const day = 24 * time.Hour

// The fixed distances inside a delete sequence. A deployment tears a server
// down in this order, and the gaps are what keep the notifications about one
// resource a second or more apart.
const (
	releaseLead      = 10 * time.Second
	volumeDeleteLead = 60 * time.Second
	volumeDeleteGap  = 10 * time.Second
	resizeDuration   = 60 * time.Second
)

// shapeStream is the second half of the shape generator's state. It is a
// constant so that the shape of a month depends on the seed alone.
const shapeStream = 1

// Generate builds one month of the simulated cloud's lifecycle transitions,
// sorted by their instant.
//
// The month runs on two generators. The shape generator draws everything that
// decides what happens and when, and it is seeded by the seed alone: the same
// seed describes the same cloud whichever period or deployment it is run for.
// The identifier generator draws every id, and it is seeded by the seed
// together with the cloud and the billing month, so the same seed on another
// cloud or in another month publishes fresh resources and fresh message ids
// while the same triple republishes the very notifications ingestion has
// already deduplicated.
//
// Splitting them is what makes both properties hold at once. Identifiers are
// drawn in a fixed order before the first transition is generated, so how many
// of them a month costs depends on the shape and never on the salt, and a month
// keeps its shape when the cloud changes.
func Generate(seed uint64, from, to time.Time, cloud string) (Schedule, error) {
	// A caller that reached here without going through period.Parse is caught
	// before it produces a month that no billing period covers.
	month := time.Date(from.UTC().Year(), from.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	if !from.Equal(month) || !to.Equal(month.AddDate(0, 1, 0)) {
		return nil, fmt.Errorf("%q is not a UTC month", from.Format(time.RFC3339))
	}

	shape := rand.New(rand.NewPCG(seed, shapeStream))
	identifiers := idReader{src: rand.New(rand.NewPCG(seed, identifierSalt(cloud, month)))}

	g := newGenerator(shape, identifiers, month, month.AddDate(0, 1, 0), cloud)
	g.run()
	if len(g.schedule) == 0 {
		return nil, errors.New("the generated month holds no transitions")
	}

	slices.SortStableFunc(g.schedule, func(a, b Transition) int { return a.At.Compare(b.At) })
	// The message ids come last so that they run in publication order, which is
	// what a dumped month reads like when it is compared against a real bus.
	for i := range g.schedule {
		g.schedule[i].MessageID = identifiers.nextUUID()
	}
	return g.schedule, nil
}

// identifierSalt mixes the cloud and the billing month into the identifier
// generator's state. Without it, two clouds fed the same seed would publish
// notifications carrying the same message ids, and ingestion would take the
// second cloud's month for a redelivery of the first one's.
func identifierSalt(cloud string, from time.Time) uint64 {
	digest := fnv.New64a()
	// A hash.Hash never fails a write, which is why the error is not examined.
	_, _ = digest.Write([]byte(cloud + "\x00" + period.Format(from)))
	return digest.Sum64()
}

// idReader draws bytes from the identifier generator. uuid takes an io.Reader
// and math/rand/v2's generator is not one, so this adapter puts every
// identifier of a month on one stream, in one order, and therefore makes them
// reproducible.
type idReader struct {
	src *rand.Rand
}

// Read fills p and never fails. A trailing partial block costs a whole draw, so
// how many draws a buffer costs depends on its length alone.
func (r idReader) Read(p []byte) (int, error) {
	var block [8]byte
	for offset := 0; offset < len(p); offset += len(block) {
		binary.LittleEndian.PutUint64(block[:], r.src.Uint64())
		copy(p[offset:], block[:])
	}
	return len(p), nil
}

// nextUUID draws one identifier in the form nova, cinder, neutron, and glance
// name their resources. The reader cannot fail, so Must never panics.
func (r idReader) nextUUID() string {
	return uuid.Must(uuid.NewRandomFromReader(r)).String()
}

// nextHexID draws one identifier in the 32 character form keystone hands out
// project and user ids in.
func (r idReader) nextHexID() string {
	var raw [16]byte
	_, _ = r.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// generator is one month under construction: the draws that shape it, the world
// they populated, and the transitions so far.
type generator struct {
	shape *rand.Rand
	from  time.Time
	to    time.Time
	cloud string

	// networkID is the external network the month's addresses come from. There
	// is one per month, the way a deployment has one.
	networkID string
	// addresses is the month's address pool as a permutation, and assigned is
	// how much of it has been handed out. Taking them in order means no address
	// is used twice inside a month.
	addresses []int
	assigned  int

	projects []*project
	schedule Schedule
}

// newGenerator draws every identifier of the month and returns the generator
// that walks the world they name. The order of the draws is fixed here and
// finished before the first transition is generated, which is what keeps the
// number of identifiers a function of the shape alone.
func newGenerator(shape *rand.Rand, identifiers idReader, from, to time.Time, cloud string) *generator {
	g := &generator{shape: shape, from: from, to: to, cloud: cloud}

	g.networkID = identifiers.nextUUID()
	g.addresses = identifiers.src.Perm(addressPoolSize)

	for range projectCount {
		g.projects = append(g.projects, &project{id: identifiers.nextHexID()})
	}
	for index, p := range g.projects {
		p.userID = identifiers.nextHexID()
		for i := range imagesPerProject {
			p.images = append(p.images, &image{id: identifiers.nextUUID(), name: imageNames[i]})
		}
		for i := range instancesPerProject {
			p.instances = append(p.instances, &instance{
				id:   identifiers.nextUUID(),
				name: fmt.Sprintf("%s-%02d", instanceNames[i], index+1),
			})
		}
		for _, inst := range p.instances {
			// The volume count is the one draw the identifier pass takes from the
			// shape, because how many volumes exist is part of the shape and how
			// many identifiers they cost follows from it.
			for i := range 1 + shape.IntN(2) {
				inst.volumes = append(inst.volumes, &volume{
					id:   identifiers.nextUUID(),
					name: fmt.Sprintf("%s-data-%d", inst.name, i+1),
				})
			}
		}
		for _, inst := range p.instances {
			inst.fip = &floatingIP{id: identifiers.nextUUID()}
		}
		p.spare = &volume{
			id:   identifiers.nextUUID(),
			name: fmt.Sprintf("handover-%02d", index+1),
		}
	}
	return g
}

// run generates every project's month. The phases are ordered the way a tenant
// works: the images come first because an instance boots from one, the volumes
// and addresses follow the instances they belong to, and the tear-down comes
// last.
func (g *generator) run() {
	for index, p := range g.projects {
		g.images(p)
		g.instances(p)
		g.volumes(p)
		g.floatingIPs(p)
		g.deleteFirstInstance(p)
		g.spareVolume(p, g.projects[(index+1)%len(g.projects)])
	}
}

// emit records one transition. The project is the one the request ran in, which
// is the owner everywhere except on a transfer, where the accepting project
// makes the request that moves the volume to itself.
func (g *generator) emit(at time.Time, eventType, publisherID, resourceID string,
	requester *project, payload map[string]any,
) {
	g.schedule = append(g.schedule, Transition{
		At:          at,
		EventType:   eventType,
		Exchange:    exchangeFor(eventType),
		Billable:    eventType != imageCreateType,
		PublisherID: publisherID,
		ProjectID:   requester.id,
		UserID:      requester.userID,
		ResourceID:  resourceID,
		Payload:     payload,
	})
}

// images gives a project its two images and deletes the second one before the
// month is over, so that every month carries an image that lives through it and
// one that does not. Each image is announced before it has content and uploaded
// shortly after, which is the pair the mapping's skip rule is written for.
func (g *generator) images(p *project) {
	for index, img := range p.images {
		img.createdAt = g.from.Add(span(g.shape, 0, 2*time.Hour))
		uploadedAt := img.createdAt.Add(span(g.shape, 30*time.Second, 120*time.Second))
		// Every quarter gibibyte from one to four.
		img.size = int64(4+g.shape.IntN(13)) * quarterGiB

		g.emit(img.createdAt, imageCreateType, imagePublisher, img.id, p, imageCreatePayload(p, img))
		g.emit(uploadedAt, "image.upload", imagePublisher, img.id, p,
			imageUploadPayload(p, img, uploadedAt))

		if index == len(p.images)-1 {
			deletedAt := g.to.Add(-span(g.shape, day, 7*day))
			g.emit(deletedAt, "image.delete", imagePublisher, img.id, p,
				imageDeletePayload(p, img, deletedAt))
		}
	}
}

// instances boots the project's servers. The first one is deleted inside the
// month and every other one outlives it, so a month always holds both a
// resource the projection closes and resources it carries forward.
func (g *generator) instances(p *project) {
	for index, inst := range p.instances {
		inst.flavor = largeFlavor
		if index > 0 {
			inst.flavor = flavors[g.shape.IntN(len(flavors))]
		}
		inst.host = computeHosts[g.shape.IntN(len(computeHosts))]
		inst.createdAt = g.from.Add(span(g.shape, 2*time.Hour, 6*time.Hour))
		inst.imageID = p.images[g.shape.IntN(len(p.images))].id

		g.emit(inst.createdAt, "compute.instance.create.end", computePublisher(inst), inst.id, p,
			instanceCreatePayload(p, inst, g.cloud))

		// The end bounds what the instance may still report, so the delete
		// instant is drawn here rather than in the tear-down phase that uses it.
		end := g.to
		if index == 0 {
			inst.deletedAt = g.to.Add(-span(g.shape, day, 10*day))
			end = inst.deletedAt
		}
		g.lifetime(p, inst, index, end)
	}
}

// lifetime walks one instance through the month: power cycles first, then a
// resize, then a shelve, each a gap apart. The first instance always resizes
// and the second always shelves, which is what makes every seed produce every
// recorded notification type instead of most of them.
//
// A step whose last notification would fall at or after the instance's end
// stops the walk. Dropping the rest with it is what keeps a deleted instance
// from reporting anything after its delete, and a step from straddling the
// month's end with only its first half inside the period.
func (g *generator) lifetime(p *project, inst *instance, index int, end time.Time) {
	cursor := inst.createdAt.Add(span(g.shape, time.Hour, 3*day))

	for range 1 + g.shape.IntN(3) {
		poweredOnAt := cursor.Add(span(g.shape, 2*time.Hour, 48*time.Hour))
		if !poweredOnAt.Before(end) {
			return
		}
		g.emit(cursor, "compute.instance.power_off.end", computePublisher(inst), inst.id, p,
			instancePowerPayload(p, inst, "stopped"))
		g.emit(poweredOnAt, "compute.instance.power_on.end", computePublisher(inst), inst.id, p,
			instancePowerPayload(p, inst, "active"))
		cursor = poweredOnAt.Add(span(g.shape, time.Hour, 3*day))
	}

	if index == 0 || g.shape.IntN(2) == 0 {
		finishedAt := cursor.Add(resizeDuration)
		if !finishedAt.Before(end) {
			return
		}
		// Both halves of a resize report the flavor the instance is moving to,
		// and every notification after them reports it as well.
		inst.flavor = otherFlavor(g.shape, inst.flavor)
		g.emit(cursor, "compute.instance.resize.end", computePublisher(inst), inst.id, p,
			instanceResizePayload(p, inst, "resized"))
		g.emit(finishedAt, "compute.instance.finish_resize.end", computePublisher(inst), inst.id, p,
			instanceResizePayload(p, inst, "active"))
		cursor = finishedAt.Add(span(g.shape, time.Hour, 3*day))
	}

	if index == 1 || g.shape.IntN(3) == 0 {
		unshelvedAt := cursor.Add(span(g.shape, day, 5*day))
		if !unshelvedAt.Before(end) {
			return
		}
		g.emit(cursor, "compute.instance.shelve_offload.end", computePublisher(inst), inst.id, p,
			instanceShelvePayload(p, inst, "shelved_offloaded"))
		g.emit(unshelvedAt, "compute.instance.unshelve.end", computePublisher(inst), inst.id, p,
			instanceShelvePayload(p, inst, "active"))
	}
}

// volumes gives every instance its one or two volumes. The attach itself is not
// a notification the collector maps, so the world records it and every later
// notification about the volume reports the in-use status cinder would.
func (g *generator) volumes(p *project) {
	for index, inst := range p.instances {
		for i, vol := range inst.volumes {
			vol.createdAt = inst.createdAt.Add(span(g.shape, 30*time.Second, 300*time.Second))
			vol.sizeGB = volumeSizesGB[g.shape.IntN(len(volumeSizesGB))]
			vol.volumeType = volumeTypes[g.shape.IntN(len(volumeTypes))]

			g.emit(vol.createdAt, "volume.create.end", volumePublisher, vol.id, p,
				volumeCreatePayload(p, vol))
			vol.attached = true

			// The first instance's volumes are deleted with it and report nothing
			// in between, which is the shortest volume life a month holds.
			if index > 0 {
				g.changeVolume(p, vol, index == 1 && i == 0)
			}
		}
	}
}

// changeVolume grows a volume and moves it to another type, each on its own
// draw unless the volume is the forced one. Forcing the second instance's first
// volume is what puts a volume.resize.end and a volume.retype into every month
// whatever the seed draws.
func (g *generator) changeVolume(p *project, vol *volume, forced bool) {
	resizes := forced || g.shape.IntN(2) == 0
	retypes := forced || g.shape.IntN(3) == 0

	var resizedAt time.Time
	if resizes {
		resizedAt = g.from.Add(span(g.shape, 3*day, 20*day))
		vol.sizeGB *= 2
		g.emit(resizedAt, "volume.resize.end", volumePublisher, vol.id, p, volumeStatePayload(p, vol))
	}
	if retypes {
		retypedAt := g.from.Add(span(g.shape, 3*day, 20*day))
		// Both instants are drawn from the same span, so a retype that would land
		// on or before the resize is pushed past it: two notifications about one
		// resource in the same second are two the projection cannot order.
		if resizes && !retypedAt.After(resizedAt) {
			retypedAt = resizedAt.Add(time.Second)
		}
		vol.volumeType = otherVolumeType(g.shape, vol.volumeType)
		g.emit(retypedAt, "volume.retype", volumePublisher, vol.id, p, volumeStatePayload(p, vol))
	}
}

// floatingIPs gives every instance an address. The third instance's is released
// mid-month, which is a floating IP that is billed for part of the period
// without the instance behind it being deleted.
func (g *generator) floatingIPs(p *project) {
	for index, inst := range p.instances {
		fip := inst.fip
		fip.address = floatingPrefix + strconv.Itoa(1+g.addresses[g.assigned])
		g.assigned++

		createdAt := inst.createdAt.Add(span(g.shape, 10*time.Second, 60*time.Second))
		g.emit(createdAt, "floatingip.create.end", networkPublisher, fip.id, p,
			floatingIPCreatePayload(p, fip, g.networkID))

		if index == 2 {
			releasedAt := g.from.Add(span(g.shape, 8*day, 22*day))
			g.emit(releasedAt, "floatingip.delete.end", networkPublisher, fip.id, p,
				floatingIPDeletePayload(fip))
		}
	}
}

// deleteFirstInstance tears the first instance down in the order a deployment
// does: the address is released, the server is destroyed, and the volumes that
// were attached to it follow one after another.
func (g *generator) deleteFirstInstance(p *project) {
	inst := p.instances[0]

	g.emit(inst.deletedAt.Add(-releaseLead), "floatingip.delete.end", networkPublisher,
		inst.fip.id, p, floatingIPDeletePayload(inst.fip))
	g.emit(inst.deletedAt, "compute.instance.delete.end", computePublisher(inst), inst.id, p,
		instanceDeletePayload(p, inst, inst.deletedAt))

	for i, vol := range inst.volumes {
		deletedAt := inst.deletedAt.Add(volumeDeleteLead + time.Duration(i)*volumeDeleteGap)
		g.emit(deletedAt, "volume.delete.end", volumePublisher, vol.id, p,
			volumeDeletePayload(p, vol, deletedAt))
	}
}

// spareVolume creates the volume that changes hands and hands it to the next
// project. A transfer is the one event that moves a resource from one project
// to another, and both halves of the month have to be billed to the right one.
func (g *generator) spareVolume(p, accepting *project) {
	vol := p.spare
	vol.createdAt = g.from.Add(span(g.shape, 2*time.Hour, 6*time.Hour))
	vol.sizeGB = volumeSizesGB[g.shape.IntN(len(volumeSizesGB))]
	vol.volumeType = volumeTypes[g.shape.IntN(len(volumeTypes))]
	g.emit(vol.createdAt, "volume.create.end", volumePublisher, vol.id, p, volumeCreatePayload(p, vol))

	// The accepting project makes the request and owns the volume from here on,
	// so it is the project in the payload as well as in the request context.
	acceptedAt := g.from.Add(span(g.shape, 10*day, 20*day))
	g.emit(acceptedAt, "volume.transfer.accept.end", volumePublisher, vol.id, accepting,
		volumeStatePayload(accepting, vol))
}

// span draws a duration from [lo, hi) at whole second granularity. Every
// instant of a month is a whole number of seconds after midnight because of it,
// which is what lets a step that ends before an instance's end stay a full
// second before it: the projection orders events by their timestamp, and two in
// the same second are two it cannot order.
func span(shape *rand.Rand, lo, hi time.Duration) time.Duration {
	return lo + time.Duration(shape.Int64N(int64((hi-lo)/time.Second)))*time.Second
}

// otherFlavor draws a flavor that is not the current one. The draw is over one
// entry less than the catalog and the collision is mapped onto the last entry,
// which keeps every other flavor equally likely.
func otherFlavor(shape *rand.Rand, current flavor) flavor {
	drawn := shape.IntN(len(flavors) - 1)
	if flavors[drawn].name == current.name {
		drawn = len(flavors) - 1
	}
	return flavors[drawn]
}

// otherVolumeType draws a type that is not the current one, the way
// otherFlavor does. A retype onto the type the volume already has would be a
// notification that changes nothing.
func otherVolumeType(shape *rand.Rand, current string) string {
	drawn := shape.IntN(len(volumeTypes) - 1)
	if volumeTypes[drawn] == current {
		drawn = len(volumeTypes) - 1
	}
	return volumeTypes[drawn]
}

// computePublisher is the publisher id of the compute an instance runs on. oslo
// publishes a nova notification under the service name and the host.
func computePublisher(inst *instance) string {
	return "compute." + inst.host
}

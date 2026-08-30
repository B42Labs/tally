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
	// gives each service its own, and a month uses eight: nova, cinder,
	// neutron, glance, octavia, keystone, designate, and barbican. The
	// collector's default binds nova, neutron, cinder, and glance, and a
	// deployment lists the other four itself.
	Exchange string
	// Billable reports whether the collector's mapping records an event for this
	// notification. It is false on the image.create that precedes an upload,
	// which carries no size yet and is skipped on purpose, and on every
	// transition of the noise catalogue (noise.go).
	Billable bool
	// noise reports whether the transition belongs to that catalogue. Nothing on
	// the wire carries it and nothing outside this package reads it: it is what
	// tells the noise apart from the one skipped billable type when the message
	// ids are drawn, so the catalogue takes its ids from its own stream.
	noise bool
	// Workload names which of the three workloads emitted the transition, one of
	// the workload constants. Nothing on the wire carries it: it is what a test
	// and the later reconciliation oracle tell the workloads of one month apart
	// by, since the tenants of a month are otherwise ids alone.
	Workload string
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

// The workloads a month is made of. The classic tenants are the ones a person
// works in, a Gardener project's shoots are driven by their machine controller,
// and the CI tenant is driven by its pipelines.
const (
	workloadClassic  = "classic"
	workloadGardener = "gardener"
	workloadCI       = "ci"
)

// exchangeFor names the exchange a type is published on. The eight are the
// service exchanges of nova, cinder, neutron, glance, octavia, keystone,
// designate, and barbican. A type outside them is one this package does not
// generate, and it is reported as the empty exchange rather than guessed at,
// because a wrong exchange is a notification no bound queue receives.
func exchangeFor(eventType string) string {
	switch {
	case strings.HasPrefix(eventType, "compute."),
		strings.HasPrefix(eventType, "scheduler."),
		strings.HasPrefix(eventType, "keypair."):
		return "nova"
	case strings.HasPrefix(eventType, "volume."):
		return "cinder"
	case strings.HasPrefix(eventType, "floatingip."),
		strings.HasPrefix(eventType, "network."),
		strings.HasPrefix(eventType, "subnet."),
		strings.HasPrefix(eventType, "router."),
		strings.HasPrefix(eventType, "port."),
		// Without the trailing dot, so that the one prefix covers both
		// security_group. and security_group_rule.
		strings.HasPrefix(eventType, "security_group"):
		return "neutron"
	case strings.HasPrefix(eventType, "image."):
		return "glance"
	case strings.HasPrefix(eventType, "octavia."):
		return "octavia"
	case strings.HasPrefix(eventType, "identity."):
		return "keystone"
	case strings.HasPrefix(eventType, "dns."):
		return "designate"
	case strings.HasPrefix(eventType, "audit."):
		return "barbican"
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
// Splitting them is what makes both properties hold at once, and the rule that
// keeps them apart runs in both directions: an identifier comes from the
// identifier stream and never from the shape stream, and what happens, when,
// and therefore how many identifiers a month draws comes from the shape stream
// and the period's calendar and never from the value of an identifier. The same
// seed, period, and cloud draw the same identifiers in the same order, and
// another cloud or another month renames everything while the shape stays the
// shape stream's.
//
// A third generator names the resources of the noise catalogue (noise.go). It
// is salted the way the identifier generator is, and it is a stream of its own
// so that the identifier generator draws the billable month and nothing else,
// down to the message ids: a noise transition takes its own from the third
// stream, so a catalogue that grows by one transition renumbers nothing the
// collector bills.
func Generate(seed uint64, from, to time.Time, cloud string) (Schedule, error) {
	// A caller that reached here without going through period.Parse is caught
	// before it produces a month that no billing period covers.
	month := time.Date(from.UTC().Year(), from.UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
	if !from.Equal(month) || !to.Equal(month.AddDate(0, 1, 0)) {
		return nil, fmt.Errorf("%q is not a UTC month", from.Format(time.RFC3339))
	}

	shape := rand.New(rand.NewPCG(seed, shapeStream))
	identifiers := idReader{src: rand.New(rand.NewPCG(seed, identifierSalt(cloud, month)))}

	g := newGenerator(shape, identifiers, noiseIdentifiers(seed, cloud, month),
		month, month.AddDate(0, 1, 0), cloud)
	g.run()
	if len(g.schedule) == 0 {
		return nil, errors.New("the generated month holds no transitions")
	}

	slices.SortStableFunc(g.schedule, func(a, b Transition) int { return a.At.Compare(b.At) })
	// The message ids come last so that they run in publication order, which is
	// what a dumped month reads like when it is compared against a real bus.
	// Each of the two streams hands out its own: a noise message id drawn from
	// the identifier stream would tie the ids of the billable month to how much
	// noise stands beside it, and a catalogue one transition longer would
	// renumber every event the collector books for every seed.
	for i := range g.schedule {
		if g.schedule[i].noise {
			g.schedule[i].MessageID = g.noiseIDs.nextUUID()
			continue
		}
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
	// identifiers is the month's identifier stream, kept past the fixed draws
	// because the resources a month churns are named when they are created: how
	// many workers or runners a month makes follows from its calendar.
	identifiers idReader
	// noiseIDs is the month's third stream, salted the way the identifier
	// stream is and drawn by noise.go alone. Holding it apart is what lets the
	// identifier stream draw exactly what it draws without the noise.
	noiseIDs idReader
	from     time.Time
	to       time.Time
	cloud    string

	// networkID is the external network the month's addresses come from. There
	// is one per month, the way a deployment has one.
	networkID string
	// addresses is the month's address pool as a permutation, and assigned is
	// how much of it has been handed out. Taking them in order means no address
	// is used twice inside a month.
	addresses []int
	assigned  int

	projects         []*project
	gardenerProjects []*gardenerProject
	ciTenant         *project

	// workload is the one emit tags its transitions with. run sets it before
	// each block of the month, so a transition carries the workload that was
	// running when it was generated.
	workload string
	schedule Schedule
}

// newGenerator draws the identifiers of the world a month starts from and
// returns the generator that walks it. The fixed resources are the ones that
// exist before the first transition: the tenants, their users and images, the
// classic tenants' servers, volumes and addresses, and the network, subnet,
// router, and security group of a shoot. They are drawn here, in this order.
//
// What a month churns is drawn from the same stream at the moment the generator
// creates the resource: the workers of a shoot and their root volumes, the
// workers a rolling update puts in their place, the claims, the load balancers
// with their VIP ports, listeners, pools, and addresses, and the CI runners.
// How many of them a month makes follows from its calendar, so drawing them
// here would mean counting the month's days twice.
//
// The identifier stream draws the billable month and nothing besides it. Every
// identifier of a resource that exists only to be announced comes from the
// noise identifier stream instead: the classic and CI tenants' networks, the
// zones, the ports, the attachments, the security group rules, the record sets,
// the secrets and containers, and the CADF record ids. The fixed ones are drawn
// below, after the last draw the identifier stream makes, and the rest at the
// moment the resource is created.
func newGenerator(shape *rand.Rand, identifiers, noiseIDs idReader,
	from, to time.Time, cloud string,
) *generator {
	g := &generator{
		shape: shape, identifiers: identifiers, noiseIDs: noiseIDs,
		from: from, to: to, cloud: cloud,
	}

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

	shoots := 0
	for _, gp := range newGardenerProjects() {
		gp.tenant = &project{id: identifiers.nextHexID()}
		gp.tenant.userID = identifiers.nextHexID()
		gp.tenant.images = []*image{{id: identifiers.nextUUID(), name: gardenerImageName}}
		for _, s := range gp.shoots {
			s.owner = gp
			// The technical id is what Gardener names a shoot's OpenStack
			// resources after, and every name of the shoot is built from it.
			s.technicalID = "shoot--" + gp.name + "--" + s.name
			s.keypairName = s.technicalID + "-ssh-publickey"
			shoots++
			s.index = shoots
			s.awake = true
			s.networkID = identifiers.nextUUID()
			s.subnetID = identifiers.nextUUID()
			s.routerID = identifiers.nextUUID()
			s.securityGroupID = identifiers.nextUUID()
		}
		g.gardenerProjects = append(g.gardenerProjects, gp)
	}

	g.ciTenant = &project{id: identifiers.nextHexID()}
	g.ciTenant.userID = identifiers.nextHexID()
	g.ciTenant.images = []*image{{id: identifiers.nextUUID(), name: ciImageName}}

	// The networks the tenants are announced on and the zones their records live
	// in. A classic tenant's network has no router of its own, because it
	// pre-exists the month and neutron announces nothing about it.
	for index, p := range g.projects {
		p.network = &network{
			id:       g.noiseIDs.nextUUID(),
			subnetID: g.noiseIDs.nextUUID(),
			name:     fmt.Sprintf("tenant-%02d", index+1),
			cidr:     fmt.Sprintf("192.168.%d.0/24", index+1),
		}
	}
	g.ciTenant.network = &network{
		id:       g.noiseIDs.nextUUID(),
		subnetID: g.noiseIDs.nextUUID(),
		routerID: g.noiseIDs.nextUUID(),
		name:     "ci",
		cidr:     "10.100.0.0/24",
	}
	for _, gp := range g.gardenerProjects {
		gp.zoneID = g.noiseIDs.nextUUID()
		gp.zoneName = gp.name + "." + cloud + ".example."
	}
	return g
}

// run generates the month of every workload, one block after another, and tags
// what each block emits with the workload it came from.
//
// The classic tenants come first. Their phases are ordered the way a tenant
// works: the images come first because an instance boots from one, the volumes
// and addresses follow the instances they belong to, and the tear-down comes
// last. The Gardener projects follow, each shoot walking its own days, and the
// CI tenant comes last, bursting its runners through the working days of the
// month.
func (g *generator) run() {
	g.workload = workloadClassic
	for index, p := range g.projects {
		g.images(p)
		g.instances(p)
		g.volumes(p)
		g.floatingIPs(p)
		g.deleteFirstInstance(p)
		g.spareVolume(p, g.projects[(index+1)%len(g.projects)])
	}

	g.workload = workloadGardener
	for _, gp := range g.gardenerProjects {
		g.gardener(gp)
	}

	g.workload = workloadCI
	g.ci()
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
		Workload:    g.workload,
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

		g.createInstance(p, inst, p.network, inst.createdAt)

		// The end bounds what the instance may still report, so the delete
		// instant is drawn here rather than in the tear-down phase that uses it.
		// The last step of a deleted instance lies before the sequence its
		// delete begins with, which is why the bound is the first notification
		// of that sequence and not the delete: a step whose .end would fall
		// inside those five seconds is dropped the way a step at or after the
		// delete is dropped, and no draw moves either way.
		end := g.to
		if index == 0 {
			inst.deletedAt = g.to.Add(-span(g.shape, day, 10*day))
			end = inst.deletedAt.Add(-instanceDeleteLead)
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
//
// Every step is a pair: its .start half is rendered stepLead before its .end,
// and the bound keeps both halves before the instance's end.
func (g *generator) lifetime(p *project, inst *instance, index int, end time.Time) {
	cursor := inst.createdAt.Add(span(g.shape, time.Hour, 3*day))

	for range 1 + g.shape.IntN(3) {
		poweredOnAt := cursor.Add(span(g.shape, 2*time.Hour, 48*time.Hour))
		if !poweredOnAt.Before(end) {
			return
		}
		g.noise(cursor.Add(-stepLead), "compute.instance.power_off.start", computePublisher(inst),
			inst.id, p, instancePowerPayload(p, inst, "active"))
		g.emit(cursor, "compute.instance.power_off.end", computePublisher(inst), inst.id, p,
			instancePowerPayload(p, inst, "stopped"))
		g.noise(poweredOnAt.Add(-stepLead), "compute.instance.power_on.start", computePublisher(inst),
			inst.id, p, instancePowerPayload(p, inst, "stopped"))
		g.emit(poweredOnAt, "compute.instance.power_on.end", computePublisher(inst), inst.id, p,
			instancePowerPayload(p, inst, "active"))
		cursor = poweredOnAt.Add(span(g.shape, time.Hour, 3*day))
	}

	if index == 0 || g.shape.IntN(2) == 0 {
		finishedAt := cursor.Add(resizeDuration)
		if !finishedAt.Before(end) {
			return
		}
		// The start of a resize is the one .start half that reports another
		// flavor than its .end: it announces the server as it still is, which is
		// why it is rendered before the new flavor is drawn.
		g.noise(cursor.Add(-stepLead), "compute.instance.resize.start", computePublisher(inst),
			inst.id, p, instanceResizePayload(p, inst, "active"))
		// Both halves of a resize report the flavor the instance is moving to,
		// and every notification after them reports it as well.
		inst.flavor = otherFlavor(g.shape, inst.flavor)
		g.emit(cursor, "compute.instance.resize.end", computePublisher(inst), inst.id, p,
			instanceResizePayload(p, inst, "resized"))
		g.noise(finishedAt.Add(-stepLead), "compute.instance.finish_resize.start", computePublisher(inst),
			inst.id, p, instanceResizePayload(p, inst, "resized"))
		g.emit(finishedAt, "compute.instance.finish_resize.end", computePublisher(inst), inst.id, p,
			instanceResizePayload(p, inst, "active"))
		cursor = finishedAt.Add(span(g.shape, time.Hour, 3*day))
	}

	if index == 1 || g.shape.IntN(3) == 0 {
		unshelvedAt := cursor.Add(span(g.shape, day, 5*day))
		if !unshelvedAt.Before(end) {
			return
		}
		g.noise(cursor.Add(-stepLead), "compute.instance.shelve_offload.start", computePublisher(inst),
			inst.id, p, instanceShelvePayload(p, inst, "active"))
		g.emit(cursor, "compute.instance.shelve_offload.end", computePublisher(inst), inst.id, p,
			instanceShelvePayload(p, inst, "shelved_offloaded"))
		g.noise(unshelvedAt.Add(-stepLead), "compute.instance.unshelve.start", computePublisher(inst),
			inst.id, p, instanceShelvePayload(p, inst, "shelved_offloaded"))
		g.emit(unshelvedAt, "compute.instance.unshelve.end", computePublisher(inst), inst.id, p,
			instanceShelvePayload(p, inst, "active"))
	}
}

// volumes gives every instance its one or two volumes. Each of them is attached
// to the server it was created for, which cinder announces under a type the
// collector does not map, and the volume reports the in-use status from there
// on.
func (g *generator) volumes(p *project) {
	for index, inst := range p.instances {
		for i, vol := range inst.volumes {
			vol.createdAt = inst.createdAt.Add(span(g.shape, 30*time.Second, 300*time.Second))
			vol.sizeGB = volumeSizesGB[g.shape.IntN(len(volumeSizesGB))]
			vol.volumeType = volumeTypes[g.shape.IntN(len(volumeTypes))]

			g.createVolume(p, vol, vol.createdAt)
			g.attach(p, vol, inst, vol.createdAt.Add(attachLag))

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
		// The .start half of a resize reports the volume as it still is, which is
		// why it is rendered before the new size is set.
		g.noise(resizedAt.Add(-stepLead), "volume.resize.start", volumePublisher, vol.id, p,
			volumeStatusPayload(p, vol, "extending"))
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
// were attached to it follow one after another. Destroying the server detaches
// them, so no delete here renders a detach of its own.
func (g *generator) deleteFirstInstance(p *project) {
	inst := p.instances[0]

	g.emit(inst.deletedAt.Add(-releaseLead), "floatingip.delete.end", networkPublisher,
		inst.fip.id, p, floatingIPDeletePayload(inst.fip))
	g.destroyInstance(p, inst, inst.deletedAt)

	for i, vol := range inst.volumes {
		deletedAt := inst.deletedAt.Add(volumeDeleteLead + time.Duration(i)*volumeDeleteGap)
		g.deleteVolume(p, vol, deletedAt)
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
	g.createVolume(p, vol, vol.createdAt)

	// The accepting project makes the request and owns the volume from here on,
	// so it is the project in the payload as well as in the request context.
	acceptedAt := g.from.Add(span(g.shape, 10*day, 20*day))
	// The .start half still reports the volume as the old owner's, because it
	// moves with the .end.
	g.noise(acceptedAt.Add(-transferLead), "volume.transfer.accept.start", volumePublisher, vol.id,
		accepting, volumeStatePayload(p, vol))
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

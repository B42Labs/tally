package simulator

import (
	"fmt"
	"strings"
	"time"
)

// flavor is one entry of the simulated nova flavor catalog. It carries all
// three names nova reports a flavor under, because a notification repeats all
// of them and the mapping reads the one it is metered by.
type flavor struct {
	name        string
	vcpus       int
	memoryMB    int
	rootGB      int
	ephemeralGB int
	typeID      int
	flavorID    string
}

// flavors is the catalog every simulated instance runs on. The four are the
// ones a stock nova ships, and m1.large is the flavor the recorded
// compute.instance.create.end was captured with, down to its flavor id. It is
// also the only one with ephemeral disk, so a month that always creates an
// m1.large always exercises the mapping's root_gb plus ephemeral_gb sum.
//
// The flavor ids are catalog constants and not salted with the period or the
// cloud: a flavor outlives a billing month, and two clouds running the same
// deployment tool have it under the same id.
var flavors = []flavor{
	{
		name: "m1.small", vcpus: 1, memoryMB: 2048, rootGB: 20, ephemeralGB: 0,
		typeID: 2, flavorID: "1b7f4c20-3e5a-4d18-8f92-2a6c9b0d7e41",
	},
	{
		name: "m1.medium", vcpus: 2, memoryMB: 4096, rootGB: 40, ephemeralGB: 0,
		typeID: 3, flavorID: "4e6a8d31-5c92-4b07-a13d-8f2b6c4e0a95",
	},
	{
		name: "m1.large", vcpus: 4, memoryMB: 8192, rootGB: 40, ephemeralGB: 40,
		typeID: 5, flavorID: "8d2c1e40-0b7a-4a3e-9d61-5b8c7a2f1e93",
	},
	{
		name: "m1.xlarge", vcpus: 8, memoryMB: 16384, rootGB: 160, ephemeralGB: 0,
		typeID: 7, flavorID: "c05a7f92-1d4b-4e63-b872-3a9d5c1e8f04",
	},
}

// largeFlavor is the flavor the first instance of every project is created
// with. Pinning one instance to it is what puts the recorded flavor id and the
// ephemeral disk into every generated month.
var largeFlavor = flavors[2]

// xlargeFlavor is the flavor a production shoot's workers run on. It is named
// beside largeFlavor so that the catalog is read by position in one place: a
// workload that picked its flavor by index would change what it runs on when an
// entry is inserted before it.
var xlargeFlavor = flavors[3]

// bootVolumeFlavor is the flavor a server that boots from a volume runs on. A
// server reports the root disk of its flavor whether it boots from one or not,
// so a flavor without root disk is what keeps the root volume from being billed
// twice, once as disk and once as volume. It sits outside flavors so that the
// draws over the catalog keep the shape they have.
var bootVolumeFlavor = flavor{
	name: "c1.large", vcpus: 4, memoryMB: 8192, rootGB: 0, ephemeralGB: 0,
	typeID: 9, flavorID: "e2d4b6a8-0c1e-4f3a-95b7-6d8f0a2c4e61",
}

// runnerFlavors are the flavors a CI runner is drawn from. A runner holds one
// job and is gone again, so it runs on the two small flavors of the catalog.
var runnerFlavors = []flavor{flavors[0], flavors[1]}

// volumeTypes are the cinder types a simulated volume carries. ssd and hdd
// carry a type_modifier in pricing/2026-03.yaml; standard carries none there
// and is rated at the implicit modifier 1. Drawing from all three is what makes
// a month price both paths.
var volumeTypes = []string{"ssd", "hdd", "standard"}

// volumeSizesGB are the sizes a volume is created with, in gibibytes.
var volumeSizesGB = []int{50, 100, 200}

// claimSizesGB are the sizes a persistent volume claim is created with, in
// gibibytes. A claim holds the data of one workload and starts smaller than a
// volume a tenant creates by hand.
var claimSizesGB = []int{10, 20, 50}

// The root volume of a worker that boots from one. Its size comes from the
// shoot rather than from the flavor, and a Kubernetes worker keeps its root
// filesystem on the fast type.
const (
	rootVolumeSizeGB = 50
	rootVolumeType   = "ssd"
)

// quarterGiB is the step image sizes are drawn on. Glance reports bytes and the
// mapping divides them into gibibytes, so a size that is a whole number of
// quarter gibibytes turns into an exact decimal instead of a repeating one.
const quarterGiB = 268435456

// computeHosts are the nova computes the instances of the simulated cloud run
// on. An instance keeps the host it was created on for its whole life, which is
// what a deployment without live migration looks like.
var computeHosts = []string{"compute-01", "compute-02", "compute-03", "compute-04"}

// The publishers and hosts the other three services report. They are the ones
// the recorded notifications carry, so a rendered notification names the same
// services an operator sees on a real bus.
const (
	volumePublisher  = "volume.storage-01@ceph"
	volumeHost       = "storage-01@ceph#ceph"
	networkPublisher = "network.neutron-01"
	imagePublisher   = "image.glance-01"
	availabilityZone = "nova"
	// floatingPrefix is the network the floating addresses come from. It is the
	// documentation range of RFC 5737, which no deployment routes.
	floatingPrefix = "203.0.113."
)

// The publishers of the services the noise catalogue speaks for. None of the
// recorded fixtures holds one, so they are written in oslo's <service>.<host>
// form rather than copied: a publisher id is what an operator reads the
// emitting service off, and nothing is metered by it.
const (
	schedulerPublisher = "scheduler.controller-01"
	// apiPublisher is nova-api, which is the service that sends the keypair
	// notifications rather than a compute.
	apiPublisher      = "api.controller-01"
	identityPublisher = "identity.keystone-01"
	dnsPublisher      = "central.designate-01"
	barbicanPublisher = "barbican.barbican-01"
)

// barbicanPort is the port of the barbican endpoint an audit record names,
// which is https://barbican.<cloud>.example:9311.
const barbicanPort = 9311

// imageNames and instanceNames are what the resources of a project are called.
// The names are cosmetic: nothing is metered by them, and they exist so that a
// dumped month reads like a deployment rather than like a list of ids.
var (
	imageNames    = []string{"debian-13-golden", "ubuntu-24.04"}
	instanceNames = []string{"web", "db", "batch", "cache"}
)

// The images the two machine-driven workloads boot from. A shoot's workers run
// the distribution Gardener ships, and a CI runner runs the one its jobs are
// built on.
const (
	gardenerImageName = "gardenlinux-1592.4"
	ciImageName       = "ubuntu-24.04-ci"
)

// project is one tenant of the simulated cloud together with everything it
// owns. The workload walks it once per month, so the resources are held as
// pointers: a step changes the resource the next step reports.
type project struct {
	id     string
	userID string

	// network is the tenant's own network with its subnet. It is nil on a
	// Gardener tenant, whose shoots each carry one of their own.
	network *network

	images    []*image
	instances []*instance
	// spare is the volume that is never attached and changes hands inside the
	// month, which is the only way a resource moves between two projects.
	spare *volume
}

// image is one glance image. Its size is the one the upload reports, since the
// create that comes first has none yet.
type image struct {
	id        string
	name      string
	size      int64
	createdAt time.Time
}

// instance is one nova server. Its flavor is the current one: a resize
// replaces it, and every notification after that reports the new one.
type instance struct {
	id        string
	name      string
	host      string
	flavor    flavor
	imageID   string
	createdAt time.Time
	// deletedAt is when the instance is destroyed, and the zero instant on one
	// that outlives the month. On the classic tenants' first instance it is
	// drawn with the create, because it bounds what the instance may still
	// report.
	deletedAt time.Time
	// port is the neutron port the server holds its address on. It is built
	// when the instance is created.
	port    *port
	volumes []*volume
	fip     *floatingIP
	// bootVolume is the volume the server boots from, and nil on a server that
	// boots from an image.
	bootVolume *volume
}

// volume is one cinder volume. An attach and a detach are notifications the
// collector does not map, and attached is what cinder reports as the status on
// the billable ones.
type volume struct {
	id         string
	name       string
	sizeGB     int
	volumeType string
	attached   bool
	// attachment is what the attach reported, held so that the detach repeats
	// the same attachment, and cleared by that detach.
	attachment map[string]any
	createdAt  time.Time
	// resizes is how often the volume has grown. A claim grows at most twice,
	// and the count does not follow from the size: 10 grown twice and 20 grown
	// once are both 40.
	resizes int
}

// floatingIP is one neutron address. It has no timestamps of its own, because
// neutron reports neither a created_at nor a deleted_at for one.
type floatingIP struct {
	id      string
	address string
	// The port the address is associated with, the address behind that port, and
	// the router that carries the traffic. All three are empty on an address
	// that is associated with nothing, which every classic tenant's address is.
	portID       string
	fixedAddress string
	routerID     string
}

// network is one neutron network with its subnet and its router. The classic
// tenants and the CI tenant have one each, and every shoot carries the one
// Gardener creates for it.
//
// routerID is empty on a classic tenant's network, which pre-exists the month
// and is therefore never announced. routerPortID is the port of the router's
// interface on the subnet, drawn when the network is built. securityGroupID is
// set on a shoot's network alone, because a shoot is the only workload whose
// group is created inside the month.
//
// The cidr is 192.168.<project>.0/24 on a classic tenant counting from 1,
// 10.100.0.0/24 on the CI tenant, and 10.250.<shoot>.0/24 on a shoot, which is
// the range that shoot's VIP addresses already lie in.
type network struct {
	id, subnetID, routerID, routerPortID, securityGroupID, name, cidr string
	// hosts counts the addresses handed to ports.
	hosts int
}

// prefix is the cidr up to its last dot, which is the part every address on the
// network shares.
func (n *network) prefix() string {
	return n.cidr[:strings.LastIndex(n.cidr, ".")]
}

// nextAddress hands out the next address of the subnet. The addresses of ports
// are cosmetic, and a network that hands out more than 240 of them starts over,
// the way a real subnet hands a released address to the next port that asks.
func (n *network) nextAddress() string {
	address := fmt.Sprintf("%s.%d", n.prefix(), 10+n.hosts%240)
	n.hosts++
	return address
}

// port is one neutron port: the address a device holds on a network. The subnet
// and the security group are copied from that network, because neutron reports
// both on the port itself.
//
// deviceOwner is compute:nova on a server's port and Octavia on the VIP port of
// a load balancer, and deviceID names the device behind it: the instance id, or
// lb-<load balancer id>.
type port struct {
	id, networkID, subnetID, securityGroupID, address, macAddress, deviceID, deviceOwner, host string
}

// recordSet is one designate record set in a Gardener project's zone.
type recordSet struct {
	id, name, recordType string
	records              []string
}

// gardenerProject is one Gardener project: the shoots it holds and the
// OpenStack tenant they run on. Gardener bills the tenant, and the project is
// the name an operator knows the shoots under.
type gardenerProject struct {
	name   string
	tenant *project
	shoots []*shoot
	// zoneID and zoneName are the designate zone the shoots' records live in.
	// The zone is <project name>.<cloud>.example., and the cloud is in the name
	// for the reason it is in the glance URL of an instance create payload: two
	// clouds fed the same seed must not report the same one.
	zoneID, zoneName string
}

// shoot is one Kubernetes cluster Gardener runs on the tenant. Its workers are
// nova servers, its persistent volume claims are cinder volumes, and each of
// its services of type LoadBalancer is an octavia load balancer with a floating
// address.
type shoot struct {
	name, technicalID string
	owner             *gardenerProject
	// index counts the shoots of all projects from 1. It is the third octet of
	// the shoot's VIP addresses, which keeps two shoots off each other's range.
	index          int
	flavor         flavor
	bootFromVolume bool
	hibernates     bool
	// transient marks the shoot that is created and deleted inside the month.
	transient   bool
	baseWorkers int

	networkID, subnetID, routerID, securityGroupID, keypairName string
	// network holds the four ids above in the form the neutron notifications
	// report them, together with the addresses the shoot's ports are handed.
	network *network
	// securityGroupRuleIDs are the two rules of the shoot's security group.
	securityGroupRuleIDs []string
	// apiRecord is the record set of the shoot's API server. ingressRecord is
	// the one of its ingress controller, and it is nil until the first load
	// balancer has an address to point at.
	apiRecord, ingressRecord *recordSet

	// createdAt is when the shoot comes up. deletedAt is the zero instant on a
	// shoot that outlives the month.
	createdAt, deletedAt time.Time
	// The midnights of the days the shoot's one-off steps fall on. Each is the
	// zero instant when the shoot has no such step.
	rollingUpdateDay, secondBalancerDay, listenerDay time.Time

	awake bool
	// workers are the servers alive now, and added are the ones the autoscaler
	// added today.
	workers, added []*instance
	claims         []*volume
	loadBalancers  []*loadBalancer
}

// loadBalancer is one octavia load balancer. The id slices hold every listener
// and every pool the balancer has, so their lengths are the counts an update
// reports.
type loadBalancer struct {
	id, name, vipPortID, vipAddress string
	listenerIDs, poolIDs            []string
	fip                             *floatingIP
	// secretID and containerID are the barbican secret and container holding
	// the balancer's certificate. Both are empty on a balancer that terminates
	// no https listener.
	secretID, containerID string
}

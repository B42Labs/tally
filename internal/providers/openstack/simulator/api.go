package simulator

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// The simulated cloud has two faces. The bus carries what happened, one
// notification per transition, and the collector books a month out of it. This
// is the other face: the OpenStack API a reconciliation sync reads to learn
// what the cloud holds right now, so that a drill can hold the projection the
// notifications built against the cloud they came from.
//
// What it serves is the oracle rather than the world the generator walked. The
// oracle states, per resource, the intervals of constant state, size and
// project the month intended, so the inventory of an instant is the intervals
// that contain it, and the resources whose last interval ended earlier are the
// ones nova reports as deleted. A sync is then answerable to the same statement
// the comparison holds an export to.
//
// Every document is shaped after the recorded fixtures under
// internal/reporting/reconciliation/adapters/testdata: the same members, and
// the same timestamp formats, down to the zone one service writes and the next
// leaves out. Those are the documents gophercloud provably decodes. Nothing
// here imports the Reporting API and nothing there imports the simulator: the
// two meet over the wire, which is what the drill is about.

// The credentials the simulated keystone accepts. They are fixed rather than
// drawn, because whatever authenticates here is configured from the same
// compose file that starts the simulator, and a password nobody can predict
// would be one nobody can write into a clouds.yaml either. The cloud they open
// is a generated month on a developer's machine.
const (
	cloudUsername = "tally-sync"
	cloudPassword = "tally-dev-sync-password"
)

// cloudRegion is the region the catalog publishes every endpoint in. A
// clouds.yaml that asks for another one reads exactly like a cloud that does
// not run the service.
const cloudRegion = "RegionOne"

// tokenBodyMax bounds the body of POST /v3/auth/tokens. The request carries a
// user name and a password, so a kilobyte is far more than it needs, and the
// bound keeps the one route that answers before a credential is checked from
// reading whatever an unauthenticated caller sends.
const tokenBodyMax = 1 << 10

// tokenLifetime is how long an issued token is good for, measured in wall time:
// a client checks the expiry against its own clock, not against the simulated
// month, and a drill that runs for an hour must not be logged out half way
// through it.
const tokenLifetime = 24 * time.Hour

// The messages the two refusals carry. Each names which of the two credentials
// failed, because what a caller does about them differs: one is a clouds.yaml
// that does not match this run, the other a token from a run that has ended.
const (
	badCredentials = "the credentials are not the simulator's"
	badToken       = "the token is not the one this run issued"
)

// unauthorizedBody is the document both refusals are answered with, in the
// shape keystone answers one. The status is repeated inside it because that is
// where a client that logs the body reads it from.
const unauthorizedBody = `{"error": {"code": 401, "title": "Unauthorized", "message": "%s"}}`

// badChangesSince is what a deleted listing is refused with when the window it
// asks for is not an instant. Nova answers that one in plain text, and so does
// this.
const badChangesSince = "changes-since must be an RFC 3339 instant"

// The version documents the endpoints publish. Nova's is the versioned one, and
// it reaches past 2.47, so the adapter negotiates the microversion that embeds
// a server's flavor rather than falling back to the flavor catalog. The other
// three are published without a version in their path, the way a deployment
// publishes them, so a client asks each of them which major versions it speaks
// before it accepts the endpoint.
const (
	computeVersions      = `{"version": {"id": "v2.1", "status": "CURRENT", "version": "2.96", "min_version": "2.1"}}`
	imageVersions        = `{"versions": [{"id": "v2.15", "status": "CURRENT"}]}`
	networkVersions      = `{"versions": [{"id": "v2.0", "status": "CURRENT"}]}`
	loadBalancerVersions = `{"versions": [{"id": "v2.0", "status": "CURRENT"}]}`
)

// The resource types the oracle books its resources under, which are also the
// ones the reconciliation adapter enumerates. Each listing serves exactly one
// of them.
const (
	typeInstance     = "instance"
	typeVolume       = "volume"
	typeFloatingIP   = "floating_ip"
	typeImage        = "image"
	typeLoadBalancer = "loadbalancer"
)

// The timestamp layouts the services write. Nova's created and glance's
// created_at carry the zone the way RFC 3339 does; nova's usage extension and
// cinder write six fractional digits without one; neutron and octavia write
// neither. Each is the form the recorded fixture carries, and a client parses
// the member with the layout its service is known for.
const (
	fractionalLayout = "2006-01-02T15:04:05.000000"
	zonelessLayout   = "2006-01-02T15:04:05"
	tokenLayout      = "2006-01-02T15:04:05.000000Z"
)

// The values every document repeats where a member the recorded fixtures carry
// is nothing the oracle states. Nothing is metered by them: an address is one
// out of the range no deployment routes, and the four identifiers are fixed so
// that two documents of one run name the same network the way one cloud would.
const (
	cloudUserID    = "8f1e4b2c9a744d1e9c0b5a6d3e7f8091"
	cloudProjectID = "4c9d2f6b81e34a7f9b3c5d8e0a1f2b34"
	cloudHostID    = "5c1f9a3b7d2e4680a9c3b5d7e1f2a4c6"
	cloudImageID   = "0b1c2d3e4f50461783940516273849ab"
	cloudNetworkID = "b7d3c1a9-5e28-4f60-9c14-8a3b5d7e2f91"
	cloudSubnetID  = "c8e4d2b0-6f39-4a71-8d25-9b4c6e8a1f30"
	cloudPortID    = "4f2a8c60-1d75-4b39-a8e2-3c6b9d1f5a07"
	cloudAddress   = floatingPrefix + "1"
)

// novaVMStates maps the state the collector books an instance under back to the
// raw vm_state nova reports it as. It is the inverse of the table osmap.VMState
// normalizes with, written out here because the fake serves what nova would say
// while the oracle says what the collector books.
//
// The four are the states a generated month leaves an instance in. What keeps
// the two tables from drifting apart is
// TestFakeAPINamesTheStatesTheMappingNormalizes, which holds every row here
// against osmap.VMState.
var novaVMStates = map[string]string{
	stateActive:  "active",
	stateShutoff: "stopped",
	stateShelved: "shelved_offloaded",
	stateResized: "resized",
}

// catalogEntry is one service the simulated keystone publishes: the type and
// the name a client looks it up by, and the path its endpoint is served under.
// The identifiers are the ones the recorded token document carries; nothing
// looks a service up by them.
type catalogEntry struct {
	id, endpointID, serviceType, name, path string
}

// cloudCatalog is what the token document publishes. The five are the services
// the reconciliation adapter builds a client for, and a cloud that published
// one of them less would leave that one resource type unobserved.
var cloudCatalog = []catalogEntry{
	{
		id: "1a2b3c4d5e6f7081920a1b2c3d4e5f60", endpointID: "2b3c4d5e6f708192a0b1c2d3e4f50617",
		serviceType: "compute", name: "nova", path: "/compute/v2.1",
	},
	{
		id: "3c4d5e6f708192a0b1c2d3e4f5061728", endpointID: "4d5e6f708192a0b1c2d3e4f506172839",
		serviceType: "block-storage", name: "cinderv3", path: "/volume/v3",
	},
	{
		id: "5e6f708192a0b1c2d3e4f50617283940", endpointID: "6f708192a0b1c2d3e4f5061728394051",
		serviceType: "network", name: "neutron", path: "/network",
	},
	{
		id: "708192a0b1c2d3e4f506172839405162", endpointID: "8192a0b1c2d3e4f50617283940516273",
		serviceType: "image", name: "glance", path: "/image",
	},
	{
		id: "92a0b1c2d3e4f5061728394051627384", endpointID: "a0b1c2d3e4f506172839405162738495",
		serviceType: "load-balancer", name: "octavia", path: "/load-balancer",
	},
}

// cloudAPI answers the listings of one month. The oracle is indexed by resource
// type once, because every request walks one type and nothing about the month
// changes while the run is up.
type cloudAPI struct {
	clock    *Clock
	periodTo time.Time
	token    string
	byType   map[string][]OracleResource
}

// NewCloudAPI serves a generated month as the OpenStack API a reconciliation
// sync reads. The clock is the run's own, so the inventory a listing answers
// with is the one the simulated month has reached, and a sync started half way
// through a month sees half a month.
//
// The routes are method-qualified, so a path no route claims and a method no
// route was registered with are both refused by the mux rather than by a
// handler that has to guess what was meant.
//
// The one token this API accepts is drawn per call: two runs of the simulator
// never share one, and a client holding the token of a run that has ended is
// told so rather than served the month that came after it.
//
// It fails on an oracle whose instances name a flavor this world does not hold.
// That oracle was written by another build, and a server served without the
// dimensions of its flavor would be observed sizeless: the sync would book a
// correction for a resource nobody changed.
func NewCloudAPI(clock *Clock, oracle Oracle) (http.Handler, error) {
	api := &cloudAPI{
		clock:    clock,
		periodTo: oracle.PeriodTo.UTC(),
		token:    uuid.NewString(),
		byType:   make(map[string][]OracleResource),
	}
	for _, resource := range oracle.Resources {
		api.byType[resource.ResourceType] = append(api.byType[resource.ResourceType], resource)
		if resource.ResourceType != typeInstance {
			continue
		}
		for _, interval := range resource.Intervals {
			name := sizeString(interval.Size, "flavor")
			if _, ok := flavorByName(name); !ok {
				return nil, fmt.Errorf("the oracle names the flavor %q that the world does not hold", name)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v3/auth/tokens", api.issueToken)

	mux.HandleFunc("GET /compute/v2.1/{$}", api.authorized(serveVersions(computeVersions)))
	mux.HandleFunc("GET /image/{$}", api.authorized(serveVersions(imageVersions)))
	mux.HandleFunc("GET /network/{$}", api.authorized(serveVersions(networkVersions)))
	mux.HandleFunc("GET /load-balancer/{$}", api.authorized(serveVersions(loadBalancerVersions)))

	mux.HandleFunc("GET /compute/v2.1/servers/detail", api.authorized(api.serveServers))
	mux.HandleFunc("GET /compute/v2.1/flavors/detail", api.authorized(serveFlavors))
	mux.HandleFunc("GET /volume/v3/volumes/detail",
		api.listing(typeVolume, "volumes", volumeDocument))
	mux.HandleFunc("GET /network/v2.0/floatingips",
		api.listing(typeFloatingIP, "floatingips", floatingIPDocument))
	mux.HandleFunc("GET /image/v2/images",
		api.listing(typeImage, "images", imageDocument))
	mux.HandleFunc("GET /load-balancer/v2.0/lbaas/loadbalancers",
		api.listing(typeLoadBalancer, "loadbalancers", loadBalancerDocument))

	return mux, nil
}

// issueToken answers the keystone v3 password request. The credentials are read
// out of the identity the request states rather than out of its scope: what a
// client asks to be scoped to is the one project this cloud has.
func (a *cloudAPI) issueToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Auth struct {
			Identity struct {
				Password struct {
					User struct {
						Name     string `json:"name"`
						Password string `json:"password"`
					} `json:"user"`
				} `json:"password"`
			} `json:"identity"`
		} `json:"auth"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, tokenBodyMax)).Decode(&body); err != nil {
		unauthorized(w, badCredentials)
		return
	}

	user := body.Auth.Identity.Password.User
	if user.Name != cloudUsername || user.Password != cloudPassword {
		unauthorized(w, badCredentials)
		return
	}

	w.Header().Set("X-Subject-Token", a.token)
	writeJSON(w, http.StatusCreated, tokenDocument(r.Host))
}

// tokenDocument is the token an authenticated client works from: what it may
// do, and the catalog every request after it is addressed from.
//
// The endpoint URLs are built from the Host header of the request that asked
// for the token rather than from an address the simulator was configured with.
// One handler then serves a container reaching it under
// host.docker.internal:8091 and a test reaching it under an httptest address,
// and each reads a catalog pointing back at the address it used.
func tokenDocument(host string) map[string]any {
	catalog := make([]map[string]any, 0, len(cloudCatalog))
	for _, entry := range cloudCatalog {
		catalog = append(catalog, map[string]any{
			"id":   entry.id,
			"type": entry.serviceType,
			"name": entry.name,
			"endpoints": []map[string]any{{
				"id":        entry.endpointID,
				"interface": "public",
				"region":    cloudRegion,
				"region_id": cloudRegion,
				"url":       "http://" + host + entry.path,
			}},
		})
	}

	issued := time.Now().UTC()
	return map[string]any{"token": map[string]any{
		"methods":    []string{"password"},
		"expires_at": issued.Add(tokenLifetime).Format(tokenLayout),
		"issued_at":  issued.Format(tokenLayout),
		"audit_ids":  []string{"3T2dc1CGQxyJsHdDu1xkcw"},
		"user": map[string]any{
			"id":                  cloudUserID,
			"name":                cloudUsername,
			"password_expires_at": nil,
			"domain":              map[string]any{"id": "default", "name": "Default"},
		},
		"project": map[string]any{
			"id":     cloudProjectID,
			"name":   "service",
			"domain": map[string]any{"id": "default", "name": "Default"},
		},
		// The account reads every project of the cloud, which is the scope a
		// reconciliation sync refuses to run without.
		"roles":   []map[string]any{{"id": "7c1a4e6b3d2f5081ae8b1c4d6e9f0a52", "name": "admin"}},
		"catalog": catalog,
	}}
}

// authorized demands the token this run issued. Every route but the token
// request carries it, discovery included: a client authenticates once and then
// sends the token on everything, so a request without it is one that never
// authenticated here.
func (a *cloudAPI) authorized(handle http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != a.token {
			unauthorized(w, badToken)
			return
		}
		handle(w, r)
	}
}

// serveVersions answers with one of the fixed version documents.
func serveVersions(document string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSONBody(w, http.StatusOK, document)
	}
}

// listing answers one resource type's live inventory under the member name its
// service publishes it as.
func (a *cloudAPI) listing(resourceType, member string,
	document func(OracleResource, OracleInterval) map[string]any,
) http.HandlerFunc {
	return a.authorized(func(w http.ResponseWriter, _ *http.Request) {
		writeListing(w, member, a.documents(resourceType, a.effective(), document))
	})
}

// serveServers answers both listings nova publishes at one path. The deleted
// one is the request that carries deleted=true together with the window it asks
// about; everything else is the live listing, the admin-scope probe a run sends
// before it observes anything included. That probe asks for one server across
// every project, and a whole page answers it.
func (a *cloudAPI) serveServers(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if asked := query.Get("changes-since"); asked != "" {
		since, err := time.Parse(time.RFC3339, asked)
		if err != nil {
			writeText(w, http.StatusBadRequest, badChangesSince)
			return
		}
		if query.Get("deleted") == "true" {
			writeListing(w, "servers", a.deletedServers(a.effective(), since))
			return
		}
	}

	writeListing(w, "servers", a.documents(typeInstance, a.effective(), a.serverDocument))
}

// serveFlavors answers the flavor catalog of the world. A client reads it only
// where it could not negotiate the microversion that embeds a server's flavor,
// and the five are the ones every simulated instance runs on.
func serveFlavors(w http.ResponseWriter, _ *http.Request) {
	documents := make([]map[string]any, 0, len(flavors)+1)
	for _, held := range flavors {
		documents = append(documents, flavorDocument(held))
	}
	documents = append(documents, flavorDocument(bootVolumeFlavor))
	writeListing(w, "flavors", documents)
}

// effective is the instant a listing is answered at: where the virtual clock
// stands, held at the end of the month. Every interval of the oracle ends
// inside the month, so a clock that has run past it would otherwise report a
// cloud that lost everything at once, and the resources that outlived the month
// are exactly the ones a drill compares after it.
func (a *cloudAPI) effective() time.Time {
	now := a.clock.Now().UTC()
	if now.After(a.periodTo) {
		return a.periodTo
	}
	return now
}

// documents is the listing of the resources of one type that exist at at.
func (a *cloudAPI) documents(resourceType string, at time.Time,
	document func(OracleResource, OracleInterval) map[string]any,
) []map[string]any {
	held := a.byType[resourceType]
	documents := make([]map[string]any, 0, len(held))
	for _, resource := range held {
		interval, ok := liveAt(resource, at, a.periodTo)
		if !ok {
			continue
		}
		documents = append(documents, document(resource, interval))
	}
	return documents
}

// deletedServers are the instances the month destroyed inside the window
// (since, at]: strictly after the bound the run carries, and no later than the
// instant the listing is answered at. An instance deleted at the bound itself
// was reported by the run that set it.
func (a *cloudAPI) deletedServers(at, since time.Time) []map[string]any {
	held := a.byType[typeInstance]
	documents := make([]map[string]any, 0, len(held))
	for _, resource := range held {
		gone, ok := deletedAt(resource, at, a.periodTo)
		if !ok || !gone.After(since) {
			continue
		}
		documents = append(documents, a.deletedServer(resource, gone))
	}
	return documents
}

// liveAt is the interval a resource is in at the instant at, and false where it
// is in none: one whose first interval has not started yet, and one that is
// already gone.
//
// The intervals are half-open, so the one that holds at starts at or before it
// and ends after it. The end of the month is the exception: a resource whose
// last interval ends there outlived the month rather than being deleted at its
// last instant, and a clock standing on that instant still serves it.
func liveAt(resource OracleResource, at, periodTo time.Time) (OracleInterval, bool) {
	for _, interval := range resource.Intervals {
		if at.Before(interval.From) {
			continue
		}
		if at.Before(interval.To) || (at.Equal(interval.To) && at.Equal(periodTo)) {
			return interval, true
		}
	}
	return OracleInterval{}, false
}

// deletedAt is when a resource was deleted, as it stands at the instant at, and
// false for one that is still there or was never created. A last interval that
// ends before the month does is a resource the month deleted, and that end is
// when it happened.
func deletedAt(resource OracleResource, at, periodTo time.Time) (time.Time, bool) {
	if len(resource.Intervals) == 0 {
		return time.Time{}, false
	}
	end := resource.Intervals[len(resource.Intervals)-1].To
	if end.Before(periodTo) && !at.Before(end) {
		return end, true
	}
	return time.Time{}, false
}

// serverDocument is one live server as nova reports it. The state is the raw
// vm_state the collector's mapping normalizes, and the flavor is embedded the
// way microversion 2.47 reports it: the reader negotiates that microversion, so
// a server without the embedded flavor would send it to the catalog for
// something it already has.
//
// The created instant comes from the first interval rather than from the one
// the server is in now: a resize opens an interval of its own, and a server
// that reported the resize as its creation would be booked as created twice.
func (a *cloudAPI) serverDocument(resource OracleResource, interval OracleInterval) map[string]any {
	held, _ := flavorByName(sizeString(interval.Size, "flavor"))
	created := resource.Intervals[0].From.UTC()
	return map[string]any{
		"id":        resource.ResourceID,
		"name":      resource.ResourceID,
		"status":    "ACTIVE",
		"tenant_id": interval.ProjectID,
		"user_id":   cloudUserID,
		"created":   created.Format(time.RFC3339),
		"updated":   interval.From.UTC().Format(time.RFC3339),
		"hostId":    cloudHostID,
		// The status above is what an operator reads, and the vm_state below is
		// what a reader books, which is why a stopped server is served ACTIVE and
		// stopped at once: nova reports the two independently, and only the second
		// of them is metered.
		"OS-EXT-STS:task_state":       nil,
		"OS-EXT-STS:vm_state":         novaVMStates[interval.State],
		"OS-EXT-STS:power_state":      1,
		"OS-EXT-AZ:availability_zone": availabilityZone,
		"OS-SRV-USG:launched_at":      created.Format(fractionalLayout),
		"OS-SRV-USG:terminated_at":    nil,
		"flavor": map[string]any{
			"id":            held.flavorID,
			"original_name": held.name,
			"vcpus":         held.vcpus,
			"ram":           held.memoryMB,
			"disk":          held.rootGB,
			"ephemeral":     held.ephemeralGB,
			"swap":          0,
			"disabled":      false,
			"is_public":     true,
			"extra_specs":   map[string]any{},
		},
		"image":           map[string]any{"id": cloudImageID},
		"addresses":       map[string]any{},
		"security_groups": []map[string]any{{"name": "default"}},
		"metadata":        map[string]any{},
	}
}

// deletedServer is one instance nova reports as destroyed. It carries the
// members of the server it was, so that a reader decoding one listing decodes
// the other, and the two the reader books it by: the key and the instant the
// month deleted it at.
func (a *cloudAPI) deletedServer(resource OracleResource, gone time.Time) map[string]any {
	document := a.serverDocument(resource, resource.Intervals[len(resource.Intervals)-1])
	document["status"] = "DELETED"
	document["OS-EXT-STS:vm_state"] = "deleted"
	document["OS-EXT-STS:power_state"] = 0
	document["OS-SRV-USG:terminated_at"] = gone.UTC().Format(fractionalLayout)
	return document
}

// flavorDocument is one flavor as nova's catalog reports it: the memory in
// mebibytes and the two disks apart, which is the form the reader converts and
// sums itself.
func flavorDocument(held flavor) map[string]any {
	return map[string]any{
		"id":                         held.flavorID,
		"name":                       held.name,
		"ram":                        held.memoryMB,
		"disk":                       held.rootGB,
		"vcpus":                      held.vcpus,
		"swap":                       "",
		"rxtx_factor":                1,
		"OS-FLV-EXT-DATA:ephemeral":  held.ephemeralGB,
		"OS-FLV-DISABLED:disabled":   false,
		"os-flavor-access:is_public": true,
		"description":                nil,
	}
}

// volumeDocument is one volume as cinder reports it. The state the oracle books
// is cinder's own word for it, so it is served as it stands.
func volumeDocument(resource OracleResource, interval OracleInterval) map[string]any {
	return map[string]any{
		"id":                           resource.ResourceID,
		"name":                         resource.ResourceID,
		"description":                  nil,
		"status":                       interval.State,
		"size":                         sizeInt(interval.Size, "size_gb"),
		"volume_type":                  sizeString(interval.Size, "type"),
		"bootable":                     "false",
		"encrypted":                    false,
		"multiattach":                  false,
		"availability_zone":            availabilityZone,
		"created_at":                   resource.Intervals[0].From.UTC().Format(fractionalLayout),
		"updated_at":                   interval.From.UTC().Format(fractionalLayout),
		"os-vol-tenant-attr:tenant_id": interval.ProjectID,
		"os-vol-host-attr:host":        volumeHost,
		"user_id":                      cloudUserID,
		"attachments":                  []map[string]any{},
		"metadata":                     map[string]any{},
	}
}

// floatingIPDocument is one address as neutron reports it. The oracle keeps no
// addresses, because the one thing an address is billed by is which protocol it
// is an address of, so every one of them is served from the range no deployment
// routes. Neutron names the owner twice and both members carry it.
func floatingIPDocument(resource OracleResource, interval OracleInterval) map[string]any {
	return map[string]any{
		"id":                  resource.ResourceID,
		"status":              "ACTIVE",
		"floating_ip_address": cloudAddress,
		"floating_network_id": cloudNetworkID,
		"fixed_ip_address":    nil,
		"port_id":             nil,
		"router_id":           nil,
		"project_id":          interval.ProjectID,
		"tenant_id":           interval.ProjectID,
		"created_at":          resource.Intervals[0].From.UTC().Format(zonelessLayout),
		"updated_at":          interval.From.UTC().Format(zonelessLayout),
		"revision_number":     1,
		"description":         "",
		"tags":                []string{},
	}
}

// imageDocument is one image as glance reports it. Glance reports bytes where
// the size states gibibytes, and the multiplication is exact: every image of a
// month is a whole number of quarter gibibytes, and the decimal it goes through
// carries the digits a float would have lost.
func imageDocument(resource OracleResource, interval OracleInterval) map[string]any {
	bytes := sizeDecimal(interval.Size, "size_gb").Mul(bytesPerGibibyte).IntPart()
	return map[string]any{
		"id":               resource.ResourceID,
		"name":             resource.ResourceID,
		"status":           "active",
		"visibility":       imageVisibility,
		"owner":            interval.ProjectID,
		"size":             bytes,
		"virtual_size":     bytes * imageVirtualSizeFactor,
		"checksum":         strings.ReplaceAll(resource.ResourceID, "-", ""),
		"container_format": imageContainerFormat,
		"disk_format":      imageDiskFormat,
		"min_disk":         imageMinDiskGB,
		"min_ram":          imageMinRAMMB,
		"protected":        false,
		"os_hidden":        false,
		"created_at":       resource.Intervals[0].From.UTC().Format(time.RFC3339),
		"updated_at":       interval.From.UTC().Format(time.RFC3339),
		"file":             "/v2/images/" + resource.ResourceID + "/file",
		"schema":           "/v2/schemas/image",
		"tags":             []string{},
	}
}

// loadBalancerDocument is one load balancer as octavia reports it.
func loadBalancerDocument(resource OracleResource, interval OracleInterval) map[string]any {
	return map[string]any{
		"id":                  resource.ResourceID,
		"name":                resource.ResourceID,
		"description":         "",
		"provisioning_status": "ACTIVE",
		"operating_status":    "ONLINE",
		"admin_state_up":      true,
		"project_id":          interval.ProjectID,
		"vip_address":         cloudAddress,
		"vip_network_id":      cloudNetworkID,
		"vip_subnet_id":       cloudSubnetID,
		"vip_port_id":         cloudPortID,
		"provider":            "amphora",
		"availability_zone":   nil,
		"flavor_id":           nil,
		"created_at":          resource.Intervals[0].From.UTC().Format(zonelessLayout),
		"updated_at":          interval.From.UTC().Format(zonelessLayout),
		"listeners":           attachedTo(resource.ResourceID, "listener", sizeInt(interval.Size, "listeners")),
		"pools":               attachedTo(resource.ResourceID, "pool", sizeInt(interval.Size, "pools")),
		"tags":                []string{},
	}
}

// attachedTo are the listeners or the pools a load balancer holds. Octavia
// reports each of them as an object with an id, and the size states how many
// there are rather than which, so an id built from the balancer's own is as
// much as either has to carry: what a balancer is billed by is the two counts.
func attachedTo(balancer, kind string, count int) []map[string]any {
	attached := make([]map[string]any, 0, count)
	for n := 1; n <= count; n++ {
		attached = append(attached, map[string]any{"id": fmt.Sprintf("%s-%s-%d", balancer, kind, n)})
	}
	return attached
}

// sizeString reads one named member of a size object, and the empty string
// where the size holds none.
func sizeString(size map[string]any, member string) string {
	value, _ := size[member].(string)
	return value
}

// sizeInt reads one whole number of a size object.
func sizeInt(size map[string]any, member string) int {
	return int(sizeDecimal(size, member).IntPart())
}

// sizeDecimal reads one number of a size object. Every number a size builder
// writes is a json.Number, and an oracle read back under UseNumber carries the
// same, so a member of another type is one no generator of this world wrote and
// counts as nothing.
func sizeDecimal(size map[string]any, member string) decimal.Decimal {
	number, ok := size[member].(json.Number)
	if !ok {
		return decimal.Zero
	}
	value, err := decimal.NewFromString(string(number))
	if err != nil {
		return decimal.Zero
	}
	return value
}

// writeListing answers one listing. Every one of them fits in a single page and
// carries no link to a next: a generated month holds a few hundred resources,
// and a fake that paginated would test a client's pagination against itself
// rather than the month.
func writeListing(w http.ResponseWriter, member string, documents []map[string]any) {
	writeJSON(w, http.StatusOK, map[string]any{member: documents})
}

// writeJSON answers with a document this API built.
func writeJSON(w http.ResponseWriter, status int, document any) {
	encoded, err := json.Marshal(document)
	if err != nil {
		writeText(w, http.StatusInternalServerError, "the document could not be encoded")
		return
	}
	writeJSONBody(w, status, string(encoded))
}

// writeJSONBody answers with a document that is already written out, which the
// version documents and the refusals are.
func writeJSONBody(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// unauthorized refuses a request that did not authenticate, naming which of the
// two credentials it was.
func unauthorized(w http.ResponseWriter, message string) {
	writeJSONBody(w, http.StatusUnauthorized, fmt.Sprintf(unauthorizedBody, message))
}

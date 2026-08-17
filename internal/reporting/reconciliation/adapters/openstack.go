// Package adapters holds the platform adapters the Reporting API registers for
// reconciliation. The framework in internal/reporting/reconciliation does
// everything a sync does apart from observing the platform, so an adapter is
// the whole of what a new provider contributes.
//
// The OpenStack adapter reads a cloud through the clouds.yaml every other
// OpenStack client on the host reads. Its credentials therefore never enter
// Tally's own configuration: adapter_config names an entry in that file, and
// the file alone says what the entry means, so a rotated password reaches the
// next sync without a change to Tally.
//
// The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
// WP 1.13.
package adapters

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"
	"github.com/gophercloud/gophercloud/v2/openstack/image/v2/images"
	"github.com/gophercloud/gophercloud/v2/openstack/loadbalancer/v2/loadbalancers"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/layer3/floatingips"
	"github.com/gophercloud/gophercloud/v2/pagination"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/providers/openstack/osmap"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
)

// openStack observes one OpenStack cloud. It holds no configuration of its own:
// a configuration reaches it per call, so one instance serves every configured
// OpenStack cloud of a deployment. What it does hold is what the process around
// it decides rather than one cloud: the clock a run measures its windows
// against, and the log a run writes what it worked around to.
type openStack struct {
	now    func() time.Time
	logger *slog.Logger
}

// NewOpenStack is the adapter for OpenStack clouds. Every call it receives
// authenticates from the clouds.yaml entry its configuration names, which is
// what keeps a long-lived process from holding a token past its lifetime.
//
// now is where every window a run bounds itself by is measured from. It is
// injected the way the framework's own poll time is (reconciliation.New), so
// that a test states the instant a run reads rather than racing the clock of
// the machine it runs on.
//
// logger is where a failure this adapter absorbs rather than reports is
// written. Stats.Errors is reachable through an EnumerationError alone, and
// that costs the resource type its completeness, so a fallback that deliberately
// keeps the run whole has nowhere else to say that it happened.
func NewOpenStack(now func() time.Time, logger *slog.Logger) reconciliation.Adapter {
	return &openStack{now: now, logger: logger}
}

// Platform is the platform this adapter observes. It is the same string the
// collector writes into every event, which is what lets LoadConfig catch a
// cloud wired to an adapter for a different platform.
func (a *openStack) Platform() string {
	return "openstack"
}

// openStackConfig is one cloud's adapter_config after parsing.
type openStackConfig struct {
	// osCloud is the entry in clouds.yaml this cloud authenticates with.
	osCloud string
	// includeOctavia reports whether load balancers belong to the inventory. It
	// is off by default: a deployment that runs no octavia would otherwise fail
	// to enumerate a type it does not have, on every sync, forever.
	includeOctavia bool
}

// parseConfig reads one cloud's adapter_config. It refuses a setting it does
// not know rather than ignoring it, because an operator who misspells
// include_octavia would otherwise get exactly the sync they did not ask for and
// nothing that says so.
func parseConfig(cfg map[string]any) (openStackConfig, error) {
	// Sorted, so a configuration with two mistakes in it names the same one on
	// every run rather than whichever the map iteration reached first.
	for _, key := range slices.Sorted(maps.Keys(cfg)) {
		switch key {
		case "os_cloud", "include_octavia":
		default:
			return openStackConfig{}, fmt.Errorf("unknown setting %q", key)
		}
	}

	raw, ok := cfg["os_cloud"]
	if !ok {
		return openStackConfig{}, fmt.Errorf("os_cloud must be set")
	}
	osCloud, ok := raw.(string)
	if !ok {
		return openStackConfig{}, fmt.Errorf("os_cloud must be a string, got %T", raw)
	}
	if osCloud == "" {
		return openStackConfig{}, fmt.Errorf("os_cloud must not be empty")
	}

	parsed := openStackConfig{osCloud: osCloud}
	if raw, ok := cfg["include_octavia"]; ok {
		includeOctavia, ok := raw.(bool)
		if !ok {
			return openStackConfig{}, fmt.Errorf("include_octavia must be a boolean, got %T", raw)
		}
		parsed.includeOctavia = includeOctavia
	}
	return parsed, nil
}

// service is one resource type together with the OpenStack service client its
// listing reads from. Locating a service in the catalog is what fails on a
// cloud that does not run it, and keeping the two together is what makes that
// failure attributable to the one resource type it concerns.
type service struct {
	resourceType string
	newClient    func(*gophercloud.ProviderClient, gophercloud.EndpointOpts) (*gophercloud.ServiceClient, error)
	// list observes what the service holds. It returns false once the consumer
	// has stopped reading the stream, which ends the run.
	list func(context.Context, *gophercloud.ServiceClient, *observer) bool
}

// services is what this configuration observes, one entry per resource type.
// ResourceTypes and ListResources both read it, so the types a run reports
// cannot drift from the types it attempts.
func (c openStackConfig) services() []service {
	list := []service{
		{resourceType: "instance", newClient: openstack.NewComputeV2, list: listInstances},
		{resourceType: "volume", newClient: openstack.NewBlockStorageV3, list: listVolumes},
		{resourceType: "floating_ip", newClient: openstack.NewNetworkV2, list: listFloatingIPs},
		{resourceType: "image", newClient: openstack.NewImageV2, list: listImages},
	}
	if c.includeOctavia {
		list = append(list, service{
			resourceType: "loadbalancer",
			newClient:    openstack.NewLoadBalancerV2,
			list:         listLoadBalancers,
		})
	}
	return list
}

// ResourceTypes is the set of resource types this adapter enumerates under cfg.
func (a *openStack) ResourceTypes(cfg map[string]any) ([]string, error) {
	parsed, err := parseConfig(cfg)
	if err != nil {
		return nil, err
	}

	services := parsed.services()
	types := make([]string, 0, len(services))
	for _, svc := range services {
		types = append(types, svc.resourceType)
	}
	return types, nil
}

// ListResources streams the live inventory of one OpenStack cloud.
//
// The framework validates the clouds file but not what an adapter makes of an
// adapter_config, so this stream is the first place a mistake in one can be
// reported. A configuration that does not parse, a clouds.yaml entry that
// cannot be resolved, and a Keystone that refuses the credentials all say
// nothing about any single resource type, so each is yielded as a plain error
// and the run is abandoned. Reporting them per type would let a sync conclude
// that a cloud it never reached holds nothing.
//
// A service missing from the catalog is the opposite case: it is a fact about
// one resource type. It is yielded as an EnumerationError, the remaining types
// are still enumerated, and the missed-delete pass leaves that one type's rows
// alone.
func (a *openStack) ListResources(ctx context.Context, cfg map[string]any, since *time.Time,
) iter.Seq2[reconciliation.ObservedResource, error] {
	return func(yield func(reconciliation.ObservedResource, error) bool) {
		parsed, err := parseConfig(cfg)
		if err != nil {
			yield(reconciliation.ObservedResource{}, err)
			return
		}

		authOptions, endpointOptions, tlsConfig, err := clouds.Parse(clouds.WithCloudName(parsed.osCloud))
		if err != nil {
			yield(reconciliation.ObservedResource{},
				fmt.Errorf("reading the clouds.yaml entry %q: %w", parsed.osCloud, err))
			return
		}

		provider, err := config.NewProviderClient(ctx, authOptions, config.WithTLSConfig(tlsConfig))
		if err != nil {
			yield(reconciliation.ObservedResource{},
				fmt.Errorf("authenticating against the cloud %q: %w", parsed.osCloud, err))
			return
		}

		for _, svc := range parsed.services() {
			out := observer{resourceType: svc.resourceType, yield: yield}

			// Building the client is what looks the service up in the catalog, so
			// a cloud that publishes no endpoint for it fails here, and fails for
			// this resource type alone.
			client, err := svc.newClient(provider, endpointOptions)
			if err != nil {
				if !out.fail(fmt.Errorf("building the service client: %w", err)) {
					return
				}
				continue
			}

			if !svc.list(ctx, client, &out) {
				return
			}
		}
	}
}

// observer hands one resource type's listing to the stream. It carries the
// type, so a listing says what it observed without repeating which type it
// speaks for, and it remembers a consumer that stopped reading: the iterator
// contract forbids a second yield after the first one returned false, and a
// listing that has already read a whole page would otherwise have to check
// after every observation it builds from it.
type observer struct {
	resourceType string
	yield        func(reconciliation.ObservedResource, error) bool
	stopped      bool
}

// observe hands one resource to the stream and reports whether the consumer is
// still reading.
func (o *observer) observe(resource reconciliation.ObservedResource) bool {
	resource.ResourceType = o.resourceType
	return o.hand(resource, nil)
}

// fail reports that this resource type stayed incomplete. Whatever went wrong,
// the consequence is the one thing an EnumerationError says: the missed-delete
// pass leaves this type's rows alone, and the run carries on with the types
// that finished.
func (o *observer) fail(err error) bool {
	return o.hand(reconciliation.ObservedResource{},
		&reconciliation.EnumerationError{ResourceType: o.resourceType, Err: err})
}

// hand is the one place this adapter's stream is written to.
func (o *observer) hand(resource reconciliation.ObservedResource, err error) bool {
	if o.stopped {
		return false
	}
	o.stopped = !o.yield(resource, err)
	return !o.stopped
}

// enumerate walks one listing to its end and observes what its pages hold. A
// request that fails part way through leaves as an EnumerationError: the pages
// before it stay observed, and this one type is what the run does not know the
// whole of.
//
// observe reports whether the consumer is still reading, and a false ends the
// walk, so a stream nobody reads stops asking the cloud for more of it.
func enumerate[T any](ctx context.Context, out *observer, pager pagination.Pager,
	extract func(pagination.Page) ([]T, error), observe func(T) bool,
) bool {
	err := pager.EachPage(ctx, func(_ context.Context, page pagination.Page) (bool, error) {
		found, err := extract(page)
		if err != nil {
			return false, err
		}
		for _, item := range found {
			if !observe(item) {
				return false, nil
			}
		}
		return true, nil
	})
	if err != nil {
		return out.fail(err)
	}
	return !out.stopped
}

// listInstances observes every project's instances. The flavor cache is built
// here rather than held by the adapter, which is what bounds it to one run: a
// flavor the operator edited between two syncs is read again by the next one.
func listInstances(ctx context.Context, client *gophercloud.ServiceClient, out *observer) bool {
	cache := flavorCache{client: client}
	return enumerate(ctx, out, servers.List(client, servers.ListOpts{AllTenants: true}),
		servers.ExtractServers, func(server servers.Server) bool {
			// A cloud that would not say what its flavors are still says which
			// instances exist, so the failure is reported for the type and the
			// instance is observed without the size nova did not describe.
			size, err := instanceSize(ctx, &cache, server)
			if err != nil && !out.fail(err) {
				return false
			}
			return out.observe(reconciliation.ObservedResource{
				ResourceID: server.ID,
				ProjectID:  server.TenantID,
				State:      osmap.VMState(server.VmState),
				Size:       size,
				CreatedAt:  timestamp(server.Created),
			})
		})
}

// listVolumes observes every project's volumes.
func listVolumes(ctx context.Context, client *gophercloud.ServiceClient, out *observer) bool {
	return enumerate(ctx, out, volumes.List(client, volumes.ListOpts{AllTenants: true}),
		volumes.ExtractVolumes, func(volume volumes.Volume) bool {
			return out.observe(reconciliation.ObservedResource{
				ResourceID: volume.ID,
				ProjectID:  volume.TenantID,
				// Cinder's status is booked as it reads, the way the collector books
				// it (mapping.go:283-288).
				State: volume.Status,
				Size: map[string]any{
					"size_gb": quantity(int64(volume.Size)),
					"type":    volume.VolumeType,
				},
				CreatedAt: timestamp(volume.CreatedAt),
			})
		})
}

// listFloatingIPs observes every project's floating IP addresses.
func listFloatingIPs(ctx context.Context, client *gophercloud.ServiceClient, out *observer) bool {
	return enumerate(ctx, out, floatingips.List(client, nil),
		floatingips.ExtractFloatingIPs, func(address floatingips.FloatingIP) bool {
			// An address the cloud does not spell readably counts as IPv4, which is
			// the whole of what this adapter does about it: an address left
			// unobserved would read as deleted.
			version, _ := osmap.IPVersion(address.FloatingIP)
			return out.observe(reconciliation.ObservedResource{
				ResourceID: address.ID,
				// Neutron names the owner twice and older deployments fill only the
				// second of the two.
				ProjectID: cmp.Or(address.ProjectID, address.TenantID),
				// An allocated address is billed whether or not it is attached, so
				// the collector books every one of them as active (mapping.go:166).
				// Reporting neutron's own ACTIVE/DOWN here would make every detached
				// address drift on every run.
				State:     "active",
				Size:      map[string]any{"ip_version": version},
				CreatedAt: timestamp(address.CreatedAt),
			})
		})
}

// listImages observes every project's images.
func listImages(ctx context.Context, client *gophercloud.ServiceClient, out *observer) bool {
	return enumerate(ctx, out, images.List(client, nil),
		images.ExtractImages, func(image images.Image) bool {
			// An image without an owner is booked to no project, and one whose bits
			// are not uploaded yet has no size to book (mapping.go:294-297). Neither
			// is something the collector ever recorded, so observing them would be
			// drift the sync itself invented.
			if image.Owner == "" || image.Status != images.ImageStatusActive {
				return true
			}
			return out.observe(reconciliation.ObservedResource{
				ResourceID: image.ID,
				ProjectID:  image.Owner,
				State:      "active",
				Size: map[string]any{
					"size_gb": json.Number(
						money.Div(decimal.NewFromInt(image.SizeBytes), osmap.BytesPerGibibyte).String()),
				},
				CreatedAt: timestamp(image.CreatedAt),
			})
		})
}

// listLoadBalancers observes every project's load balancers.
func listLoadBalancers(ctx context.Context, client *gophercloud.ServiceClient, out *observer) bool {
	return enumerate(ctx, out, loadbalancers.List(client, nil),
		loadbalancers.ExtractLoadBalancers, func(balancer loadbalancers.LoadBalancer) bool {
			state := strings.ToLower(balancer.ProvisioningStatus)
			// Octavia keeps a deleted load balancer in its listing. Observing one
			// would resurrect a resource the collector already booked as gone.
			if state == "deleted" || state == "pending_delete" {
				return true
			}
			return out.observe(reconciliation.ObservedResource{
				ResourceID: balancer.ID,
				ProjectID:  balancer.ProjectID,
				State:      state,
				Size: map[string]any{
					"listeners": len(balancer.Listeners),
					"pools":     len(balancer.Pools),
				},
				CreatedAt: timestamp(balancer.CreatedAt),
			})
		})
}

// flavorCache resolves the flavors of a run's instances. A cloud of any size
// runs far more instances than it publishes flavors, so the flavors are read
// once, in full, on the first instance that needs them, rather than one GET per
// instance. A run whose servers all carry their flavor embedded reads none.
type flavorCache struct {
	client *gophercloud.ServiceClient
	byID   map[string]flavors.Flavor
}

// lookup resolves one flavor id. The second return says whether the cloud
// publishes that flavor at all, which it does not for one an operator deleted
// while instances of it keep running.
//
// The error is the one the single fetch failed with, and only the call that
// made that fetch sees it: a cloud whose flavor listing is broken says so once
// per run rather than once per instance.
func (c *flavorCache) lookup(ctx context.Context, id string) (flavors.Flavor, bool, error) {
	if c.byID == nil {
		c.byID = make(map[string]flavors.Flavor)
		if err := c.fetch(ctx); err != nil {
			return flavors.Flavor{}, false, err
		}
	}
	flavor, ok := c.byID[id]
	return flavor, ok, nil
}

// fetch reads every flavor the cloud publishes. AllAccess is what includes the
// private flavors a deployment sells to single projects; a cloud that refuses
// it answers with the public ones, and the instances on a private flavor then
// miss the cache instead of failing the listing.
func (c *flavorCache) fetch(ctx context.Context) error {
	return flavors.ListDetail(c.client, flavors.ListOpts{AccessType: flavors.AllAccess}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			found, err := flavors.ExtractFlavors(page)
			if err != nil {
				return false, err
			}
			for _, flavor := range found {
				c.byID[flavor.ID] = flavor
			}
			return true, nil
		})
}

// instanceSize describes an instance the way the rating engine meters it. A
// nova of microversion 2.47 or later embeds the whole flavor in the server, and
// an older one embeds its id alone, which is what the cache is for.
//
// A size is nil where neither source describes one. That reads as "no evidence"
// to the diff rather than as a size that became empty, which is the right
// answer for a flavor the cloud no longer publishes: the instance exists, and
// what it is made of is not something this run learned.
func instanceSize(ctx context.Context, cache *flavorCache, server servers.Server,
) (map[string]any, error) {
	if size, ok := embeddedSize(server.Flavor); ok {
		return size, nil
	}

	id, _ := server.Flavor["id"].(string)
	flavor, found, err := cache.lookup(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return flavorSize(int64(flavor.VCPUs), int64(flavor.RAM), int64(flavor.Disk),
		int64(flavor.Ephemeral), flavor.Name), nil
}

// embeddedSize reads the flavor nova embeds in a server. Every member has to be
// there: a deployment answering an older microversion embeds the id alone, and
// half a size object would be booked as a resize on every single sync.
func embeddedSize(embedded map[string]any) (map[string]any, bool) {
	name, ok := embedded["original_name"].(string)
	if !ok {
		return nil, false
	}
	vcpus, haveVCPUs := embeddedInt(embedded, "vcpus")
	ram, haveRAM := embeddedInt(embedded, "ram")
	disk, haveDisk := embeddedInt(embedded, "disk")
	ephemeral, haveEphemeral := embeddedInt(embedded, "ephemeral")
	if !haveVCPUs || !haveRAM || !haveDisk || !haveEphemeral {
		return nil, false
	}
	return flavorSize(vcpus, ram, disk, ephemeral, name), true
}

// embeddedInt reads one member of the embedded flavor. encoding/json decodes an
// untyped number as a float64, and nova reports these four in whole vCPUs,
// mebibytes and gibibytes, so the integer is the whole of the value and no
// quantity is derived from a float.
func embeddedInt(embedded map[string]any, key string) (int64, bool) {
	value, ok := embedded[key].(float64)
	if !ok {
		return 0, false
	}
	return int64(value), true
}

// flavorSize builds a size object out of a flavor's dimensions, in the units
// the collector books them in (mapping.go:300-333): nova reports memory in MiB
// and the two disks separately, while a size object holds gibibytes and their
// sum. The quotient goes through the exact decimals every quantity in Tally is
// calculated in and lands as a JSON number literal, so 512 MiB is booked as 0.5
// and not as whatever a float would render.
func flavorSize(vcpus, ram, disk, ephemeral int64, name string) map[string]any {
	return map[string]any{
		"vcpus":  quantity(vcpus),
		"ram_gb": json.Number(money.Div(decimal.NewFromInt(ram), osmap.MebibytesPerGibibyte).String()),
		"disk_gb": json.Number(
			decimal.NewFromInt(disk).Add(decimal.NewFromInt(ephemeral)).String()),
		"flavor": name,
	}
}

// quantity renders a whole number as the JSON literal a size object carries it
// as, which is the form the projection stores and compares against.
func quantity(value int64) json.Number {
	return json.Number(strconv.FormatInt(value, 10))
}

// timestamp is a platform-reported instant as an observation carries it: absent
// where the platform reported none, and in UTC where it did, so two runs of one
// resource are compared as instants rather than as the zones two APIs happened
// to spell them in.
func timestamp(reported time.Time) *time.Time {
	if reported.IsZero() {
		return nil
	}
	utc := reported.UTC()
	return &utc
}

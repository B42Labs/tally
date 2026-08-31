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
	"github.com/gophercloud/gophercloud/v2/openstack/utils"
	"github.com/gophercloud/gophercloud/v2/pagination"
	"github.com/shopspring/decimal"

	"github.com/b42labs/tally/internal/core/money"
	"github.com/b42labs/tally/internal/providers/openstack/osmap"
	"github.com/b42labs/tally/internal/reporting/reconciliation"
)

// openStack observes one OpenStack cloud. It holds no configuration of its own:
// a configuration reaches it per call, so one instance serves every configured
// OpenStack cloud of a deployment. What it does hold is what the process around
// it decides rather than one cloud: the log a run writes what it worked around
// to.
type openStack struct {
	logger *slog.Logger
}

// NewOpenStack is the adapter for OpenStack clouds. Every call it receives
// authenticates from the clouds.yaml entry its configuration names, which is
// what keeps a long-lived process from holding a token past its lifetime.
//
// logger is where a failure this adapter absorbs rather than reports is
// written. Stats.Errors is reachable through an EnumerationError alone, and
// that costs the resource type its completeness, so a fallback that deliberately
// keeps the run whole has nowhere else to say that it happened.
func NewOpenStack(logger *slog.Logger) reconciliation.Adapter {
	return &openStack{logger: logger}
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

// services is what cfg observes, one entry per resource type. ResourceTypes and
// ListResources both read it, so the types a run reports cannot drift from the
// types it attempts.
//
// since bounds the deleted listing of the one type that has one, and nothing
// else; at is the instant that listing's floor is measured from. ResourceTypes
// names the types alone and passes no bound, so at is never read there.
func (a *openStack) services(cfg openStackConfig, since *time.Time, at time.Time) []service {
	// Instances are the one type this cloud will name its deletions for, so the
	// bound a run carries reaches this listing and no other.
	instances := func(ctx context.Context, client *gophercloud.ServiceClient,
		out *observer,
	) bool {
		return a.listInstances(ctx, client, out, since, at)
	}

	list := []service{
		{resourceType: "instance", newClient: openstack.NewComputeV2, list: instances},
		{resourceType: "volume", newClient: openstack.NewBlockStorageV3, list: listVolumes},
		{resourceType: "floating_ip", newClient: openstack.NewNetworkV2, list: listFloatingIPs},
		{resourceType: "image", newClient: openstack.NewImageV2, list: listImages},
	}
	if cfg.includeOctavia {
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

	services := a.services(parsed, nil, time.Time{})
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
//
// An account that cannot see every project is a third case, and the loudest of
// the three: it is refused before a single resource is observed. Nothing
// downstream can tell a narrowed listing from a complete one, so a run that
// cannot prove its scope must not observe at all.
//
// since bounds the deleted listing alone, never the live one. It is the last
// completed run of this cloud, so the deleted listing walks exactly the window
// this sync may have missed. at is the instant this run is at, and how far back
// that window may reach is measured from it.
func (a *openStack) ListResources(ctx context.Context, cfg map[string]any, since *time.Time,
	at time.Time,
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

		if err := probeAdminScope(ctx, provider, endpointOptions); err != nil {
			yield(reconciliation.ObservedResource{},
				fmt.Errorf("the clouds.yaml entry %q cannot observe the whole cloud: %w",
					parsed.osCloud, err))
			return
		}

		for _, svc := range a.services(parsed, since, at) {
			out := observer{resourceType: svc.resourceType, yield: yield}

			// Building the client is what looks the service up in the catalog, so
			// a cloud that publishes no endpoint for it fails here, and fails for
			// this resource type alone.
			client, err := svc.newClient(provider, endpointOptions)
			if err != nil {
				if !out.fail(ctx, fmt.Errorf("building the service client: %w", err)) {
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

// probeAdminScope sends the one listing a cloud refuses a lesser account
// outright, before any of the three that would narrow silently.
//
// Only two of the five listings say so themselves: nova and cinder answer an
// account without the reach with a 403 on all_tenants. Floating IP addresses,
// images and load balancers carry no such flag — neutron, glance and octavia
// narrow the listing to the caller's own project and answer 200. That answer is
// indistinguishable from a complete one, so the missed-delete pass books a
// delete correction for every projection row of every other project, the
// endpoint reports a completed run, and no later run with a restored account
// undoes it: the diff skips a row it already holds as deleted.
//
// So the scope is established by asking the cloud, not by reading a name off
// the token. Only the cloud knows what its policy.yaml resolves
// context_is_admin to, and a deployment that renamed that role needs no setting
// here: whatever the role is called, an account that holds it is answered and
// one that does not is refused. A name in adapter_config could establish
// neither — it would only say which name to compare against, so a name that
// happens to be in the token would pass the check while the three silent
// listings still narrowed, and the run would wipe every other project's rows
// with nothing to say it had.
func probeAdminScope(ctx context.Context, provider *gophercloud.ProviderClient,
	endpoint gophercloud.EndpointOpts,
) error {
	client, err := openstack.NewComputeV2(provider, endpoint)
	if err != nil {
		return fmt.Errorf("building the compute client its scope is established with: %w", err)
	}
	// One server is the whole of the proof: nova refuses the request itself, so
	// what it would have answered with does not matter. The walk stops at the
	// first page because the limit alone would not stop it — nova answers a
	// limited listing with a link to the next one, and AllPages would follow that
	// through every instance of the cloud, one at a time.
	return servers.List(client, servers.ListOpts{AllTenants: true, Limit: 1}).EachPage(ctx,
		func(_ context.Context, page pagination.Page) (bool, error) {
			_, err := servers.ExtractServers(page)
			return false, err
		})
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
//
// A cancelled run is the one failure that is not attributable. It says nothing
// about this resource type and nothing about the types after it, so the stream
// ends here with a plain error and no further type is attempted: the framework
// refuses the partial inventory of a cancelled run anyway
// (reconciliation/sync.go:432-438), and an EnumerationError per remaining type
// would only bury the one thing that happened.
func (o *observer) fail(ctx context.Context, err error) bool {
	if cause := ctx.Err(); cause != nil {
		o.hand(reconciliation.ObservedResource{},
			fmt.Errorf("the run ended while enumerating %s: %w", o.resourceType, cause))
		// Nothing is yielded after it, whether or not the consumer would read on.
		o.stopped = true
		return false
	}
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
		return out.fail(ctx, err)
	}
	return !out.stopped
}

// embeddedFlavorMicroversion is the compute microversion from which nova reports
// a server's flavor in full rather than by id, out of the instance's own record
// rather than out of the catalog. It is what lets an instance on a flavor the
// operator retired still say what it is made of.
const embeddedFlavorMicroversion = "2.47"

// listInstances observes every project's instances, and the ones nova destroyed
// within the window this run is responsible for. The flavor cache is built here
// rather than held by the adapter, which is what bounds it to one run: a flavor
// the operator edited between two syncs is read again by the next one.
func (a *openStack) listInstances(ctx context.Context, client *gophercloud.ServiceClient,
	out *observer, since *time.Time, at time.Time,
) bool {
	// Negotiated rather than demanded: a nova too old for the microversion and an
	// endpoint that publishes no range at all both answer the listing the way they
	// always did, and the flavor cache below is what resolves a server that
	// carries its flavor by id.
	//
	// Discovery is an HTTP request, so it fails for reasons that say nothing about
	// the range nova speaks as well — a proxy answering 503, a reset connection —
	// and gophercloud reports those as the endpoint publishing no range whenever
	// the catalog entry carries a version, which is how every deployment
	// publishes nova. The two cannot be told apart here, so both fall back. The
	// reason is logged rather than dropped: the fallback is only complete while
	// the cloud still publishes every flavor its instances run on, and an
	// instance on a retired one is observed without a size after this.
	if negotiated, err := utils.RequireMicroversion(ctx, *client, embeddedFlavorMicroversion); err == nil {
		client = &negotiated
	} else {
		a.logger.Warn("reading the flavors from the catalog, because the compute microversion "+
			"that embeds them could not be negotiated",
			"microversion", embeddedFlavorMicroversion, "error", truncate(err.Error()))
	}

	cache := flavorCache{client: client}
	live := enumerate(ctx, out, servers.List(client, servers.ListOpts{AllTenants: true}),
		servers.ExtractServers, func(server servers.Server) bool {
			// A cloud that would not say what its flavors are still says which
			// instances exist, so the failure is reported for the type and the
			// instance is observed without the size nova did not describe.
			size, err := instanceSize(ctx, &cache, server)
			if err != nil && !out.fail(ctx, err) {
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
	if !live {
		return false
	}

	// A run without a bound is the first completed run of this cloud. It has no
	// window behind it to catch up on, and what the projection holds while the
	// cloud does not is exactly what the absence pass books.
	if since == nil {
		return true
	}
	return a.listDeletedInstances(ctx, client, out, *since, at)
}

// maxDeletedWindow is how far back a run asks nova for the servers it
// destroyed. A bound older than this is not worth the pages it costs: the
// absence pass books those deletes at poll time anyway, while a deleted listing
// that no longer fits the run's budget is what keeps the bound from ever moving
// forward again.
const maxDeletedWindow = 24 * time.Hour

// listDeletedInstances observes the instances nova reports as deleted since the
// last completed run. It is the only pass that can date a missed delete at the
// instant the platform performed it: the absence pass books one at poll time,
// and no later run corrects that, because the diff skips a row it already holds
// as deleted (reconciliation/sync.go:524-534).
//
// A listing that fails here costs the instance type its completeness, even
// though the live listing succeeded. That is the point of it: the absence pass
// must not book the same deletes at poll time in this run, and a failed run
// does not move the bound the next one starts from
// (reconciliation/sync.go:377-391), so the window is walked again and the real
// instants still land.
func (a *openStack) listDeletedInstances(ctx context.Context, client *gophercloud.ServiceClient,
	out *observer, since, at time.Time,
) bool {
	// The floor is the one thing a run asks the cloud for that the instant the
	// run is at decides rather than the database. Both clamps end at it, and both
	// are logged: the deletes a clamp leaves out are booked by the absence pass at
	// poll time, and a completed run says nothing about which of its delete
	// corrections carry the platform's own instant, so the log is the one place a
	// clamped window shows at all.
	floor := at.Add(-maxDeletedWindow)

	// A cloud that has not completed a run in a week would otherwise ask nova
	// for a week of churn, inside the same budget the five live listings share,
	// and time out before it can complete and move the bound. The window would
	// then be longer on every following run: a ratchet with no way back, and one
	// nothing but a hand-written sync_runs row recovers from.
	if since.Before(floor) {
		a.logger.Warn("asking nova only for the newest part of the window this run is "+
			"responsible for, because the last completed run is older than the window",
			"bound", since.UTC(), "asking_since", floor.UTC(), "window", maxDeletedWindow)
		since = floor
	}

	// A bound after the instant this run is at asks nova for a window that has
	// not happened yet, and nova answers that with an empty listing rather than
	// with a refusal: the type would count as enumerated, and every delete inside
	// the window be booked by the absence pass at poll time instead of at nova's
	// own terminated_at. The bound is the newest run this cloud completed and
	// started_at is the instant a run was told it ran at, so a run told an instant
	// ahead of this one is what puts it there, a host whose clock ran away as
	// much. The run walks the whole window it may instead, which is the one it is
	// answerable for either way.
	if since.After(at) {
		a.logger.Warn("asking nova for the whole window this run is responsible for, "+
			"because the last completed run started after the instant this run is at",
			"bound", since.UTC(), "asking_since", floor.UTC(), "at", at.UTC(),
			"window", maxDeletedWindow)
		since = floor
	}

	opts := deletedServerOpts{ListOpts: servers.ListOpts{
		AllTenants:   true,
		ChangesSince: since.UTC().Format(time.RFC3339),
	}}
	var skipped int
	var newest time.Time
	ok := enumerate(ctx, out, servers.List(client, opts), servers.ExtractServers,
		func(server servers.Server) bool {
			deletedAt := timestamp(server.TerminatedAt)
			// A nova that reports no terminated_at has nothing this pass could add:
			// the absence pass books that delete at poll time, and an instant
			// invented here would be worse than the approximation it replaces.
			if deletedAt == nil {
				return true
			}
			// changes-since is a lower bound and the compute API has no upper one,
			// so the listing runs to nova's own present rather than to the instant
			// this run is at. A run told an instant behind that present — a replay,
			// a drill re-run, a bound that clamped the window back to the floor —
			// would otherwise book a delete dated after the run it belongs to, and
			// therefore outside the period the run was told it reconciles. The
			// absence pass books that one at poll time, like every delete this pass
			// leaves out.
			if deletedAt.After(at) {
				skipped++
				if deletedAt.After(newest) {
					newest = *deletedAt
				}
				return true
			}
			// A deleted resource is reported by its key alone. What it was, how big
			// it was and who owned it is what the projection row already holds
			// (reconciliation/sync.go:468-491).
			return out.observe(reconciliation.ObservedResource{
				ResourceID: server.ID,
				DeletedAt:  deletedAt,
			})
		})

	// The third clamp is logged for the same reason the two window clamps are: a
	// completed run says nothing about which of its delete corrections carry the
	// platform's own instant. A nova whose clock runs ahead of this host by more
	// than one sync interval leaves every delete of every run past the instant the
	// run is at, so this pass books nothing and the absence pass dates all of them
	// at poll time, permanently — and nothing but this line tells that apart from
	// a window in which the cloud destroyed nothing.
	if skipped > 0 {
		a.logger.Warn("leaving the instances nova destroyed after the instant this run is at "+
			"to the absence pass, because a correction dated past that instant falls outside "+
			"the period this run reconciles",
			"skipped", skipped, "newest", newest.UTC(), "at", at.UTC())
	}
	return ok
}

// deletedServerOpts asks nova for the servers it destroyed since an instant.
// servers.ListOpts carries changes-since but has no deleted field at all, so
// the query is where that half of the request comes from: nova reads
// deleted=true&changes-since=<RFC3339> as the listing of what it destroyed
// within the window, and a listing without changes-since would be every
// instance the cloud ever had.
type deletedServerOpts struct {
	servers.ListOpts
}

// ToServerListQuery renders the listing's query, the embedded options and the
// deleted flag the compute API has no option for.
func (o deletedServerOpts) ToServerListQuery() (string, error) {
	query, err := gophercloud.BuildQueryString(o.ListOpts)
	if err != nil {
		return "", err
	}
	values := query.Query()
	values.Set("deleted", "true")
	return "?" + values.Encode(), nil
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
	// Whether this listing has already reported an ownerless image, which is what
	// keeps the run to one reason for however many of them glance holds.
	var reported bool
	return enumerate(ctx, out, images.List(client, nil),
		images.ExtractImages, func(image images.Image) bool {
			// glance holds an image in its listing before its bits are uploaded and
			// after it was removed, and neither is a resource the collector booked:
			// an image is registered without a size and reaches the projection with
			// the upload that follows (mapping.go:178-197, 294-297). A deactivated
			// image is the opposite case. It still exists, still occupies the store,
			// and glance publishes no notification that would have taken its row out
			// of the projection, so skipping it would book a delete for a resource
			// that is there.
			if image.Status != images.ImageStatusActive &&
				image.Status != images.ImageStatusDeactivated {
				return true
			}
			// An image glance names no owner for is one the collector booked to the
			// project of whoever registered it (mapping.go:252-265). This run cannot
			// say which that was, so the whole type stays incomplete rather than one
			// row being deleted for an absence the adapter caused.
			//
			// Only the first of them is reported. A glance holding a hundred would
			// otherwise fill the whole of Stats.Errors with copies of one reason: the
			// cap is 100 (reconciliation/sync.go:59), images are the fourth of five
			// listings, and whatever the fifth failed with would never reach the row
			// an operator reads.
			//
			// The listing still walks to its end. Holding the type back stops the
			// deletes and nothing else — the diff still books a create or an update
			// for every image this run observed (reconciliation/sync.go:536-589), and
			// glance orders its listing newest first, so ending here would leave every
			// image older than the ownerless one unobserved until somebody reaps it by
			// hand: a create the run missed would never be corrected, and a size or
			// owner that drifted would never settle.
			if image.Owner == "" {
				if reported {
					return true
				}
				reported = true
				return out.fail(ctx,
					fmt.Errorf("glance reports the image %s without an owner", image.ID))
			}
			return out.observe(reconciliation.ObservedResource{
				ResourceID: image.ID,
				ProjectID:  image.Owner,
				// The state glance reports is not booked: the collector writes
				// "active" for every image it records and has no notification that
				// would write another, so reporting a deactivation here would be
				// drift no collector event could ever settle.
				State: "active",
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

// maxLoggedErrorBytes is how much of a platform's error text one log record
// keeps. It is the bound the run's stats already apply, for the same reason
// (reconciliation/sync.go:61-66): what a platform answered is rendered into the
// error verbatim, so nothing but this bounds how long one is, and a log record
// this adapter writes is written once per sync of every configured cloud
// forever, with nothing that throttles it.
const maxLoggedErrorBytes = 4 << 10

// truncate bounds one reason to what an operator works from. The cut can fall
// inside a character the platform sent, which the log's encoder replaces the way
// it replaces every other byte that is not UTF-8.
func truncate(reason string) string {
	if len(reason) <= maxLoggedErrorBytes {
		return reason
	}
	return reason[:maxLoggedErrorBytes] + "… (truncated)"
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

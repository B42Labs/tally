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
	"context"
	"fmt"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/config"
	"github.com/gophercloud/gophercloud/v2/openstack/config/clouds"

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
}

// services is what this configuration observes, one entry per resource type.
// ResourceTypes and ListResources both read it, so the types a run reports
// cannot drift from the types it attempts.
func (c openStackConfig) services() []service {
	list := []service{
		{resourceType: "instance", newClient: openstack.NewComputeV2},
		{resourceType: "volume", newClient: openstack.NewBlockStorageV3},
		{resourceType: "floating_ip", newClient: openstack.NewNetworkV2},
		{resourceType: "image", newClient: openstack.NewImageV2},
	}
	if c.includeOctavia {
		list = append(list, service{resourceType: "loadbalancer", newClient: openstack.NewLoadBalancerV2})
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
			// Building the client is what looks the service up in the catalog, so
			// a cloud that publishes no endpoint for it fails here, and fails for
			// this resource type alone.
			if _, err := svc.newClient(provider, endpointOptions); err != nil {
				if !yield(reconciliation.ObservedResource{}, &reconciliation.EnumerationError{
					ResourceType: svc.resourceType,
					Err:          fmt.Errorf("building the service client: %w", err),
				}) {
					return
				}
			}
		}
	}
}

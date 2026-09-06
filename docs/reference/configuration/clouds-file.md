---
title: Clouds file
description: The YAML file that names the clouds the Reporting API can reconcile, and the settings of the OpenStack adapter.
quadrant: reference
audience: operator
---

# Clouds file

`TALLY_REPORTING_CLOUDS_CONFIG` names the file the Reporting API reads its
clouds from. The file is read once at startup, in
[`cmd/tally-reporting/main.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-reporting/main.go),
so a file that does not parse or an entry that does not hold refuses the process
rather than failing every request that reaches the sync endpoint.

The variable unset means no clouds are configured. `POST /internal/sync/{cloud}`
then answers 404 with `the configuration names no such cloud` for every cloud.

## Entries

The document holds one key, `clouds:`, whose value is a list of entries in file
order. One entry configures one cloud.

### The entry

<!-- refdoc:begin entry -->
#### `CloudConfig`

CloudConfig is one syncable cloud: which platform it belongs to, which adapter observes it, and the settings that adapter needs.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `cloud` | string | always | Cloud is the cloud name. It is the same name the events carry and the same one the internal sync endpoint takes as its path segment. |
| `platform` | string | always | Platform is the platform the cloud belongs to. It is stated here as well as implied by the adapter, so that a cloud wired to the wrong adapter is caught when the file is read rather than when the events land. |
| `adapter` | string | always | Adapter is the key of the adapter in the registry the process passes to LoadConfig. |
| `adapter_config` | object | always | AdapterConfig is handed to the adapter unchanged. It stays uninterpreted here because only the adapter knows what belongs in it, which is what lets a new provider add settings without touching the framework. |
<!-- refdoc:end entry -->

## What is checked at startup

`LoadConfig` in
[`internal/reporting/reconciliation/config.go`](https://github.com/B42Labs/tally/blob/main/internal/reporting/reconciliation/config.go)
checks that:

- the file is readable and parses as YAML.
- `cloud` is set on every entry.
- no cloud name is configured twice.
- `platform` is set.
- `adapter` is set and names an adapter the process registers. The registry
  carries `openstack`.
- the named adapter observes the platform the entry declares.

An entry is named in the error by its cloud, or by its position as `clouds[0]`
while it carries none.

`adapter_config` is not checked at startup. The framework hands it to the
adapter unchanged and has no hook for what an adapter makes of its own
settings, so a mistake in one surfaces on the first sync run of that cloud. That
run leaves a `sync_runs` row at status `failed` whose `stats.errors` names the
setting, and the endpoint answers 500. The response carries no reason: these
errors carry platform detail. The run's log line and the row are where the
reason is read back.

## The OpenStack adapter

`adapter: openstack` takes two settings, parsed in
[`internal/reporting/reconciliation/adapters/openstack.go`](https://github.com/B42Labs/tally/blob/main/internal/reporting/reconciliation/adapters/openstack.go).

`os_cloud` is required and is a non-empty string. It is the entry in
`clouds.yaml` this cloud authenticates with, and it is unrelated to the `cloud`
of the entry above.

`include_octavia` is optional, is a boolean, and is false by default. True adds
`loadbalancer` to the resource types the adapter enumerates.

Any other key is refused with `unknown setting` and the key named. A key whose
value is of the wrong type is refused with the type it holds.

`OS_CLIENT_CONFIG_FILE` names the `clouds.yaml` gophercloud reads. It is not a
Tally variable; the
[Reporting API settings](/reference/configuration/tally-reporting) page states
what leaving it unset costs.

## Example

[`deploy/kubernetes/overlays/dev/reconciliation/clouds-config.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/overlays/dev/reconciliation/clouds-config.yaml)
configures the one cloud the development cluster reconciles.

<!-- refdoc:begin example -->
```yaml
# The clouds this dev cluster can reconcile: the one the compose stack's
# simulator serves. The name matches the cloud the simulated month's events are
# booked under, so the sync endpoint and the metering rows meet under os-sim.
clouds:
  - cloud: os-sim
    platform: openstack
    adapter: openstack
    adapter_config:
      # Which entry of the mounted clouds.yaml the adapter authenticates with.
      os_cloud: os-sim
      # The simulator serves Octavia, so the load balancers it reports are part
      # of what a drill compares against the oracle.
      include_octavia: true
```
<!-- refdoc:end example -->

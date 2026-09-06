---
title: OpenStack notification mapping
description: Which oslo notification types the collector records, the Tally event each becomes, and how its state and size are read.
quadrant: reference
audience: integrator
---

# OpenStack notification mapping

The OpenStack collector turns oslo notifications into canonical events with a
table. The table is data, the `mappings` literal in
[`internal/providers/openstack/mapping.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/mapping.go),
because oslo names and payload members differ per OpenStack release: adapting
the collector to a deployment is an edit to that literal and to nothing around
it.

A notification whose type the table does not name produces no event. The
collector counts it under `tally_collector_skipped_total`, labelled by the oslo
type, and acknowledges the delivery.

Mapping itself never fails. A payload the table did not understand still becomes
an event, and the Reporting API dead-letters it with the validation reason it
broke. That leaves a record of the notification, where dropping it in the
collector would have been silent.

## The table

The state, the size and the skip columns name the function the entry derives the
value with, which is what to look up in the same file.

<!-- refdoc:begin mapping -->
| Oslo event type | Tally event type | Resource type | State | Size | Resource id | Project id | Skipped when |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `compute.instance.create.end` | `compute.instance.create.end` | `instance` | `vmState` | `instanceSize` | `instance_id` | `tenant_id` | none |
| `compute.instance.delete.end` | `compute.instance.delete.end` | `instance` | none | none | `instance_id` | `tenant_id` | none |
| `compute.instance.resize.end` | `compute.instance.resize.end` | `instance` | `vmState` | `instanceSize` | `instance_id` | `tenant_id` | none |
| `compute.instance.finish_resize.end` | `compute.instance.resize.end` | `instance` | `vmState` | `instanceSize` | `instance_id` | `tenant_id` | none |
| `compute.instance.shelve_offload.end` | `compute.instance.shelve` | `instance` | `fixedState("shelved")` | none | `instance_id` | `tenant_id` | none |
| `compute.instance.unshelve.end` | `compute.instance.unshelve` | `instance` | `fixedState("active")` | none | `instance_id` | `tenant_id` | none |
| `compute.instance.power_off.end` | `compute.instance.power_off` | `instance` | `fixedState("shutoff")` | none | `instance_id` | `tenant_id` | none |
| `compute.instance.power_on.end` | `compute.instance.power_on` | `instance` | `fixedState("active")` | none | `instance_id` | `tenant_id` | none |
| `volume.create.end` | `volume.create.end` | `volume` | `fixedState("available")` | `volumeSize` | `volume_id` | `tenant_id` | none |
| `volume.delete.end` | `volume.delete.end` | `volume` | none | none | `volume_id` | `tenant_id` | none |
| `volume.resize.end` | `volume.resize.end` | `volume` | `volumeStatus` | `volumeSize` | `volume_id` | `tenant_id` | none |
| `volume.retype` | `volume.retype` | `volume` | `volumeStatus` | `volumeSize` | `volume_id` | `tenant_id` | none |
| `volume.transfer.accept.end` | `volume.transfer.accept.end` | `volume` | `volumeStatus` | `volumeSize` | `volume_id` | `tenant_id` | none |
| `floatingip.create.end` | `floatingip.create.end` | `floating_ip` | `fixedState("active")` | `floatingIPSize` | `floatingip.id` | `floatingip.tenant_id` | none |
| `floatingip.delete.end` | `floatingip.delete.end` | `floating_ip` | none | none | `floatingip_id` | request context | none |
| `image.upload` | `image.create` | `image` | `fixedState("active")` | `imageSize` | `id` | `owner` | none |
| `image.create` | `image.create` | `image` | `fixedState("active")` | `imageSize` | `id` | `owner` | `unsizedImage` |
| `image.delete` | `image.delete` | `image` | none | none | `id` | `owner` | none |
| `octavia.loadbalancer.create.end` | `octavia.loadbalancer.create.end` | `loadbalancer` | `fixedState("active")` | `loadBalancerSize` | `loadbalancer_id` or `id` | `project_id` | none |
| `octavia.loadbalancer.update.end` | `octavia.loadbalancer.update.end` | `loadbalancer` | `fixedState("active")` | `loadBalancerSize` | `loadbalancer_id` or `id` | `project_id` | none |
| `octavia.loadbalancer.delete.end` | `octavia.loadbalancer.delete.end` | `loadbalancer` | none | none | `loadbalancer_id` or `id` | `project_id` | none |
<!-- refdoc:end mapping -->

## Identity

`event_id` is the oslo `message_id`, which is unique per notification and is
what makes a redelivery a duplicate at ingestion rather than a second event. A
notification that carries none is given the deterministic id
`internal/core/ids.DeterministicEventID` derives from the platform, the cloud,
the resource id, the mapped event type and the timestamp.

`platform` is `openstack`, `cloud` is the cloud named by `TALLY_OSC_CLOUD` in
the collector's configuration, and `source` is `collector`.

`payload.provider` carries `oslo_event_type`, the oslo type as it arrived, so an
event that was renamed on the way in is traced back to the notification it came
from.

The owning project is resolved in three steps. The payload path of the entry
wins, because it describes the resource while the request context describes
whoever made the call, and the two differ when an administrator acts on another
project's resource. Where the entry names no path, or the path leads nowhere,
the context project id is taken, and where that is empty the context tenant id
is. An entry with no path at all is the `request context` the table's project id
column names.

## State rules

`vmState` reads `state` out of the payload and normalizes it: `stopped` becomes
`shutoff` and `shelved_offloaded` becomes `shelved`. A state the normalization
table has no entry for passes through as nova reported it, because substituting
something known for an unknown one would hide it, and an absent `state` stays
empty.

`fixedState` reads nothing. The notification type already says what the state
became, so the entry names the value in the table itself.

`volumeStatus` reads `status`. The events it serves change a volume's size or
its owner rather than its status, and cinder does not always repeat the status
in them, so an absent one falls back to `available`.

## Size builders

A builder returns a size object even where it can read nothing, so a create says
"this is the size" rather than "the size did not change". A member whose source
is absent, out of bounds, or of another type is left out of the object rather
than defaulted, and the Reporting API then refuses the event against the
registered size schema rather than booking a value nobody reported.

`instanceSize` reads `vcpus` into `vcpus`, `memory_mb` divided by 1024 into
`ram_gb`, `root_gb` plus `ephemeral_gb` into `disk_gb`, and `instance_type` into
`flavor`. Either disk member may be absent and counts as zero then, an instance
without ephemeral storage reports no `ephemeral_gb`, and a payload naming
neither leaves `disk_gb` out altogether.

`volumeSize` reads `size` into `size_gb` and `volume_type` into `type`. A retype
already names the type the volume was moved to, so one builder serves every
volume event.

`imageSize` reads `size`, which glance reports in bytes, divided by 1073741824
into `size_gb`.

`floatingIPSize` reads `floatingip.floating_ip_address` and records which
protocol it is an address of under `ip_version`. An address that is absent or
unreadable counts as version 4, which is what a deployment allocates unless it
says otherwise, and a skipped event would cost the address its whole billing
record.

`loadBalancerSize` counts the elements of the `listeners` and the `pools`
arrays. An absent member and a null one both count as zero, because a service
with none of something leaves the member out rather than sending an empty array.
A member that is not an array is left out.

## Skipped notifications

`unsizedImage` skips an `image.create` whose payload carries no `size`, or a
size that is not positive. Glance creates an image before its bits are uploaded,
and the `image.upload` that follows carries the real size and is the
notification the image is booked from. Both types map to the Tally event
`image.create`.

Octavia publishes the load balancer dictionary the controller carried through
the flow that finished, and two shapes of it are in circulation. The one the
worker passes between its own tasks names the load balancer `loadbalancer_id`
and carries no status at all; the one octavia's admin guide records names it
`id` and repeats `provisioning_status`. The three octavia entries read
`loadbalancer_id` and fall back to `id`, and the fallback is consulted only once
the first path came back empty, so a payload carrying both spellings is read
from the first.

The state of the octavia create and update is fixed at `active` rather than read
from the payload, because both are sent by the task that follows the one marking
the load balancer active. The delete carries no state, as every delete does.

## See also

The [canonical event](/reference/formats/canonical-event) page states the shape
the mapped events take and the size schemas they are validated against. The
[tally-openstack-collector](/reference/command-line/tally-openstack-collector)
page states how the collector is run.

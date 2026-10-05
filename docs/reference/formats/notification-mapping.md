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
| `compute.instance.finish_resize.end` | `compute.instance.resize.end` | `instance` | `vmState` | `instanceSize` | `instance_id` | `tenant_id` | none |
| `compute.instance.resize.confirm.end` | `compute.instance.resize.confirm.end` | `instance` | `vmState` | `instanceSize` | `instance_id` | `tenant_id` | none |
| `compute.instance.resize.revert.end` | `compute.instance.resize.revert.end` | `instance` | `vmState` | `instanceSize` | `instance_id` | `tenant_id` | none |
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
| `octavia.loadbalancer.update.end` | `octavia.loadbalancer.update.end` | `loadbalancer` | `fixedState("active")` | none | `loadbalancer_id` or `id` | `project_id` | none |
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

The mapping reads the project and does not judge it. An instance or a volume a
service created in its own project, an octavia amphora for instance, is booked
to that project like any other.
[Zero-rate a service project](/how-to/openstack/zero-rate-a-service-project)
bills such a project at zero.

## State rules

`vmState` reads `state` out of the payload and normalizes it: `stopped` becomes
`shutoff`, `shelved_offloaded` becomes `shelved`, and `resized` becomes
`active`. Nova reports a server it resized while stopped as `resized` as well
and keeps it powered off, and neither the payload nor the table tells the two
apart, so such a server is booked `active` until the confirm or the revert
reports it stopped. A state the normalization table has no entry for passes
through as nova reported it, because substituting something known for an
unknown one would hide it, and an absent `state` stays empty.

A collector or a Reporting API up to v0.5.0 records `resized` as nova reports
it. The reconciliation sync normalizes through the same table as the collector,
so while the two run different versions, it books a correction for every
instance it finds waiting for its resize to be confirmed or reverted. The first
sync after both are upgraded corrects the instances an earlier version left at
`resized`; without reconciliation, such an instance keeps `resized` until its
next event. A `resized` state modifier in a pricing model rates only the
intervals an earlier version booked.

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

`volumeSize` reads `size` into `size_gb` and `volume_type` into `type`. Cinder
reports the volume type's id in `volume_type`, on a retype the id of the type
the volume was moved to, so one builder serves every volume event. The collector
sends the id as it reads. The Reporting API replaces it with the type's name at
ingest once a reconciliation run stored the cloud's types, as
[Size names](/reference/formats/canonical-event#size-names) states.

`imageSize` reads `size`, which glance reports in bytes, divided by 1073741824
into `size_gb`.

`floatingIPSize` reads `floatingip.floating_ip_address` and records which
protocol it is an address of under `ip_version`. An address that is absent or
unreadable counts as version 4, which is what a deployment allocates unless it
says otherwise, and a skipped event would cost the address its whole billing
record.

`loadBalancerSize` serves the create alone. It counts the elements of the
`listeners` and the `pools` arrays, and an absent member and a null one both
count as zero. The dictionary octavia's worker publishes names neither, whatever
the load balancer holds, so a create books `{"listeners": 0, "pools": 0}`. A
member that is not an array is left out.

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

## The size of a load balancer

Octavia publishes a load balancer without its collections. The create, the
update and the delete carry the dictionary of the flow that finished
([`notification_tasks.py`](https://github.com/openstack/octavia/blob/d3a882b734cdcc176301c77e20cac9f7720451b0/octavia/controller/worker/v2/tasks/notification_tasks.py#L31-L40)),
and octavia-lib's `to_dict` leaves out every member that is a list
([`data_models.py`](https://github.com/openstack/octavia-lib/blob/0444975bf3cf6c5eca7d191d5c672df75ba4970f/octavia_lib/api/drivers/data_models.py#L60-L68)),
so neither `listeners` nor `pools` is in any of the three, whatever the load
balancer holds. The same holds on 2025.1, 2026.2 and master. The update
therefore states no size, and the create states zero of both. Octavia sends
nothing at all when a listener or a pool is added or removed.

The counts reach Tally through reconciliation alone. A sync of a cloud whose
entry sets `include_octavia` reads them off the API and books them as a
`sync.update` dated at the sync's own instant, so the time between the change
and that sync is booked at the earlier counts. Without such a sync every load
balancer stays at zero of both. Each correction counts in
`tally_sync_resources_reconciled_total` under `action="updated"`.
[How reconciliation observes a cloud](/explanation/how-reconciliation-observes-a-cloud)
explains the sync, and [reconcile a cloud](/how-to/openstack/reconcile-a-cloud)
sets it up.

A sync can run ahead of the collector, while the collector works off a backlog
or restarts. It then books a load balancer the collector has not delivered yet
as a `sync.create` with the counts it reads, dated at the `created_at` the API
reports. The collector's create follows it: octavia dates the create at the end
of its flow, no earlier than `created_at`, and the fold orders the events of
one instant by when they were received. The create's zero counts are then the
current ones, and the load balancer is booked at zero of both from the create's
instant until the next sync books a `sync.update`.

A collector up to v0.5.0 books `{"listeners": 0, "pools": 0}` on every
`octavia.loadbalancer.update.end`, which resets a count a sync booked until the
next sync books it again. Upgrading the collector ends that, and the intervals
already booked at zero stay as they are.

## The resize sequence

The table books two notifications of every resize or cold migration: the
destination's `compute.instance.finish_resize.end`, and the
`compute.instance.resize.confirm.end` or `compute.instance.resize.revert.end`
that ends it.

`compute.instance.resize.end` comes from the source host before the destination
applies the new flavor, so it carries the flavor the instance is leaving. The
table has no entry for it, and the collector counts it under
`tally_collector_skipped_total`.

`compute.instance.finish_resize.end` carries the new flavor and `vm_state`
`resized`. It is booked as `compute.instance.resize.end` with the state
`active`.

`compute.instance.resize.confirm.end` repeats the new flavor, and
`compute.instance.resize.revert.end` carries the flavor the instance went back
to. Each is booked under its own name with the state nova set, `active` or
`stopped`, which `vmState` records as `active` or `shutoff`. Nova confirms a
resize on its own only when `resize_confirm_window` is set; its default of 0
leaves the instance in `resized` until the user confirms or reverts.

## See also

The [canonical event](/reference/formats/canonical-event) page states the shape
the mapped events take and the size schemas they are validated against. The
[tally-openstack-collector](/reference/command-line/tally-openstack-collector)
page states how the collector is run.

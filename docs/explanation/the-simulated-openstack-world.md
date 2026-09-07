---
title: The simulated OpenStack world
description: What the simulator renders, why the month is deterministic, and why the noise catalogue is generated beside the notifications that get billed.
quadrant: explanation
audience: all
---

# The simulated OpenStack world

The simulator is one binary that puts a month of oslo.messaging notifications on
a RabbitMQ broker the way nova, cinder, neutron, glance, octavia, keystone,
designate, and barbican put them there.
[The collector](/explanation/how-the-collector-consumes-a-bus) consumes them
unmodified, off its ordinary `tally-notifications` queue, so a month of usage
reaches Tally with no OpenStack deployment behind it. This page is what the
month is made of and why it is made that way. The command lines, the flags and
the file formats are on
[the `tally-openstack-simulator` reference page](/reference/command-line/tally-openstack-simulator),
and the procedures are in
[run a month](/how-to/simulator/run-a-month),
[replay a recorded month](/how-to/simulator/replay-a-recorded-month),
[compare an export](/how-to/simulator/compare-an-export),
[switch on faults](/how-to/simulator/switch-on-faults),
[reconcile the simulated cloud](/how-to/simulator/reconcile-the-simulated-cloud)
and
[register simulated projects](/how-to/simulator/register-simulated-projects).

## The world and the workload

The simulated cloud has three classic projects, two Gardener projects on two
tenants, one CI tenant, and one external network. It is small on purpose: what
a run has to cover is every notification type and every shape of resource life,
not a realistic tenant count. Every tenant of it is addressed by its id alone:
no notification, payload, or log line of a month carries a tenant name.

Each tenant works on a network of its own. The classic tenants' networks are
`192.168.<n>.0/24`, they pre-exist the month, and neutron announces nothing
about them. The CI tenant's `10.100.0.0/24` with its router is built at the
start of the month, and every shoot carries a `10.250.<index>.0/24`, the range
that shoot's VIP addresses lie in. The Gardener and the CI tenants are created
in keystone at the month start; the classic tenants are there before the first
transition. Each Gardener project holds one designate zone its shoots publish
their records into, `alpha.<cloud>.example.` and `beta.<cloud>.example.`.

The flavor catalog holds four entries: `m1.small` with 1 vCPU, 2048 MB of
memory, and a 20 GB root disk; `m1.medium` with 2, 4096, and 40; `m1.large`
with 4, 8192, and 40 GB root plus 40 GB ephemeral; `m1.xlarge` with 8, 16384,
and 160. `m1.large` is the only one with ephemeral disk, and the first instance
of every project runs on it, so every month exercises the mapping's sum of root
and ephemeral disk. Beside the catalog stands `c1.large` with 4 vCPUs, 8192 MB
of memory, and no root disk. It is the flavor of a worker that boots from a
volume: a server reports the root disk of its flavor whether it boots from one
or not, so a flavor without root disk keeps the root volume from being billed
twice, once as disk and once as volume.

A volume carries one of the types `ssd`, `hdd`, and `standard`. The first two
appear under `type_modifiers` in `pricing/2026-03.yaml` and `standard` does
not, so a month prices both paths. A persistent volume claim is created with
10, 20, or 50 GB, and the root volume of a worker that boots from one is 50 GB
of `ssd`. Image sizes are drawn at quarter-gibibyte steps from 1 GiB to 4 GiB,
which keeps the mapping's division into gibibytes on exact decimals.

Instances run on `compute-01` to `compute-04` and keep the host they were
created on. Cinder publishes as `storage-01@ceph`, neutron as `neutron-01`, and
glance as `glance-01`. The services of the noise catalogue publish as
`scheduler.controller-01`, `api.controller-01` (nova-api is what sends the
keypair notifications), `identity.keystone-01`, `central.designate-01`, and
`barbican.barbican-01`. Floating addresses come from `203.0.113.0/24`, the
documentation range of RFC 5737, drawn as a permutation so that no address is
handed out twice inside a month.

Every classic project gets:

- two images, each announced by an unsized `image.create` and uploaded 30 to
  120 seconds later by an `image.upload`. The second image is deleted in the
  last week of the month.
- four instances, created within the first six hours, each with one to three
  power cycles. A resize always falls on the first instance and on any other
  with probability 1/2; a shelve always on the second and on any other with
  probability 1/3. The first instance is deleted in the last ten days of the
  month, together with its floating IP and its volumes.
- one or two volumes per instance. The second instance's first volume always
  resizes and retypes.
- one floating IP per instance. The third instance's is released mid-month,
  while the instance behind it lives on.
- one spare volume, transferred to the next project.

### The Gardener projects

Two Gardener projects run their shoots on two OpenStack tenants. Nothing of the
Kubernetes side is rendered: what the simulator generates is what the platform
underneath a shoot creates in OpenStack, the workers of the machine controller,
the volumes of the CSI driver, and the load balancers of the cloud controller.
Each tenant uploads one image, `gardenlinux-1592.4`, in the first half hour of
the month and never deletes it, because the fleet that boots from it is created
and destroyed all month long. The project names below reach a notification
through the technical ids of the shoots, `shoot--alpha--api-prod` and the like,
and the tenants they run on carry no name of their own.

`alpha` runs on a tenant of its own with two shoots:

- `api-prod` on `m1.xlarge`, booted from the tenant image, created in the first
  hours of the month and alive through it. It rolls its workers once on a
  working day between 8 and 20 days into the month, takes a second load
  balancer between 3 and 20 days in, and a third listener on its first balancer
  between 5 and 25 days in.
- `api-dev` on `c1.large` from a 50 GB root volume, created in the first hours
  as well. It hibernates at 19:00 of every day it is awake and wakes at 07:00
  of every working day. Half the seeds give it a second load balancer.

`beta` runs on the other tenant with one shoot: `batch` on `m1.large`, created
in the working hours of a day between 2 and 8 days into the month and torn down
in the working hours of one between 18 and 27 days in.

A shoot's life renders:

- three or four workers, booted seconds apart. A worker that boots from a
  volume reports its `volume.create.end` 5 to 15 seconds before its
  `compute.instance.create.end` and carries an empty `image_ref_url`.
- two or three claims, each of them a `volume.create.end`. On a working day a
  claim is added with probability 1/3, one is doubled with probability 1/4 (at
  most twice per claim), and one is deleted with probability 1/5, never the
  first one.
- an autoscaler that adds one or two workers in the morning and gives them back
  in the evening of every working day the shoot is fully alive.
- a rolling update that boots a replacement and deletes the worker it replaces
  minutes later.
- hibernation, which deletes every worker and its root volume and keeps the
  claims, the balancers, and their addresses.
- a tear-down in the order the resources depend on each other: the address,
  the balancer with its VIP port and its certificate, the workers, the claims,
  and the infrastructure underneath them.

A load balancer renders three notifications. `octavia.loadbalancer.create.end`
carries no listeners and no pools, a `floatingip.create.end` gives its VIP port
an address, and an `octavia.loadbalancer.update.end` one to five minutes later
carries the listeners and the pools, which is what the balancer's size is
booked from. The address of a balancer is associated from the moment it is
allocated: it names its `port_id`, its `fixed_ip_address`, and its `router_id`,
and its status is `ACTIVE`. The classic tenants' addresses are allocated
unassociated instead, with the three members `null` and the status `DOWN`.
Every octavia notification carries a `publisher_id` of `null`, the way the
recorded samples do.

The names are the shapes Gardener's OpenStack extension and the
machine-controller-manager give the resources they create: the technical id
`shoot--<project>--<shoot>`, a worker
`<technical id>-worker-z1-<5 hex>-<5 hex>`, a root volume named after the
worker it carries, a claim `<technical id>-dynamic-pvc-<volume id>`, a load
balancer `kube_service_<technical id>_<namespace>_<service>`, and a keypair
`<technical id>-ssh-publickey`. They are cosmetic: nothing is metered by a name.

Gardener builds a shoot's infrastructure before its first worker boots: the
network with its subnet, the router out of it and the interface that puts the
router on the subnet, the security group with the two rules of a worker pool,
the keypair the workers are reachable under, and the record set the API server
answers on, `api.<shoot>.<project>.<cloud>.example.`. The first load balancer of
a shoot adds the ingress record `*.ingress.<shoot>.<project>.<cloud>.example.`,
and a balancer with an `https` listener holds its certificate in barbican as a
secret and a container. The tear-down gives all of it back in the order the
resources depend on each other and ends on the `network.delete.end`, after which
nothing of the shoot is emitted. "The noise" below holds those sequences with
the second each of their notifications falls on.

### The CI tenant

The CI tenant uploads the image `ubuntu-24.04-ci` and boots runners on every
Monday to Friday of the month: 4 to 8 bursts of 2 to 5 runners each, the
runners of one burst 1 to 3 seconds apart. A runner runs on `m1.small` or
`m1.medium`, is called `runner-<8 hex>`, and is deleted 3 to 40 minutes after
its create. It holds no volume and no address.

### The profile

The Gardener and the CI workload draw their instants on a working-week profile.
Every hour of the period carries a weight: 10 for a Monday to Friday between
07:00 and 19:00 UTC, 3 for 05:00 to 07:00 and 19:00 to 23:00 of those days, and
1 for every other hour, the nights and the weekends. What a machine drives (a
scale-up, the claim activity, a CI burst) is drawn on those weights. What
somebody triggers (a shoot's creation and its deletion, the rolling update day,
a second balancer, a listener) falls on the working hours alone.

The working days are the Mondays to Fridays of the real calendar of the
simulated period: July 2026 begins on a Wednesday and has 23 of them. The
profile follows that calendar rather than a synthetic one, which is the author's
decision of 2026-08-29. The consequence is that the same seed run over another
month keeps the classic tenants at their offsets from the month start and moves
the shoot and the CI activity onto that month's working days.

## The noise

Beside the 21 types the mapping knows, a month renders 62 the collector bills
nothing for. They are what a real bus carries around every billable transition:
the scheduler's placement decisions, the ports of every server, the networks,
subnets, routers and security groups underneath them, the keypairs, keystone's
authentications, designate's zones and record sets, barbican's audit records,
the attach and the detach of a volume, and the `.start` half of every step that
has one. The collector receives all of them and counts each as skipped, so a
month without them is a month whose skip counters stay at zero and whose ratio
of billable to received says nothing.

Three rules hold for the whole catalogue.

It is never billable. The collector's mapping claims nothing for any of these
types, and `WriteEvents` refuses a transition that is marked billable and mapped
to nothing, which is what `TestEveryBillableTransitionMapsToAnEvent` holds a
month to.

It takes no draw from the shape stream and no identifier from the identifier
stream. Every noise instant is a fixed offset in whole seconds from the billable
instant it belongs to, and every identifier comes from a third generator, the
noise identifier stream, salted with the cloud and the month the way the
identifier stream is. The billable transitions of a seed, a period, and a cloud
therefore keep every instant, id, and payload they had without the catalogue,
message ids aside.

It is rendered by
[`noise.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/simulator/noise.go) and by nothing
else, so what a month is billed for and what it merely carries stay two things a
reader can tell apart.

### A boot and a delete

A boot is ten notifications before the create the collector books, at these
distances from it in seconds. The scheduler picks the host and answers,
`scheduler.select_destinations.start` at -25 and its `.end` at -24. Nova
announces the server at -20 with `compute.instance.create.start` and reports its
progress through three `compute.instance.update` at -15, -10, and -5: scheduling
to networking, networking to block_device_mapping, and block_device_mapping to
spawning. Neutron creates the port the server holds its address on,
`port.create.start` at -17 with its `.end` at -16, and binds it to the compute,
`port.update.start` at -12 with its `.end` at -11. The billable
`compute.instance.create.end` comes last, because it is what the server is
billed from.

A delete opens with a `compute.instance.update` to the deleting task at -5. The
pre-delete `compute.instance.exists` at -4 reports what the server used on the
day so far, `compute.instance.delete.start` at -3 and the shutdown around it,
`.start` at -2 and `.end` at -1, tear it down, and the billable
`compute.instance.delete.end` follows. Neutron releases the port afterwards,
`port.delete.start` at +1 with its `.end` at +2, and cinder detaches every
volume the server still held, `volume.detach.start` at +3 with its `.end` at
+4.

### The daily audits

`compute.instance.exists` is nova's periodic existence audit. Every instance
that existed during a calendar day of the month is reported at the following
midnight with an audit over that day, which is what a deployment gets from
`instance_usage_audit_period = day`. The default nova ships is the month, and a
monthly period would put no audit inside the simulated month at all, because the
first one falls on the midnight that ends it. An hourly period would put
twenty-four times as many lines on the bus for the same single type.

An audit sits at the midnight itself and is pushed on by whole seconds while the
instance already reports a transition at that second, which keeps two
notifications about one resource a second apart. An instance created and deleted
between two midnights is audited once, at the midnight that follows. What an
audit repeats is the instance as it stands: a resize moves the flavor over, and
every `.end` moves the state over.

### The paired steps

Seven steps send a `.start` five seconds before the `.end` the collector books:
`compute.instance.power_off`, `compute.instance.power_on`,
`compute.instance.resize`, `compute.instance.finish_resize`,
`compute.instance.shelve_offload`, `compute.instance.unshelve`, and
`volume.resize`. `volume.transfer.accept.start` comes one second before its
`.end`. That the catalogue carries both halves of every such step is the
author's decision of 2026-08-30.

### The volumes and the images

A volume create is announced by `volume.create.start` eight seconds before it,
which is the `created_at` the billable payload reports. `volume.attach.start` at
+1 and its `.end` at +2 connect the volume to a server and hand out an
attachment record naming the server, its compute, and the device: `/dev/vda` for
a root volume and `/dev/vdb` otherwise. A delete of an attached volume detaches
it first, at -3 and -2, and `volume.delete.start` follows at -1. The claims of
`api-dev` are detached at every hibernation and attached again at every wake-up.
The spare volume a project hands over is never attached.

An image is prepared before its upload and activated after it. `image.prepare`
at -1 is the image while its content is still arriving, the one notification
about it that carries neither a size nor a checksum, and `image.activate` at +1
repeats the payload of the upload unchanged.

### A shoot's infrastructure

What Gardener creates before the first worker boots runs one transition per
second, from one second after the shoot's creation instant to sixteen after it,
with an `identity.authenticate` two seconds before it: the network and its
subnet, the router and the interface that puts it on the subnet, the security
group with two rules (SSH from anywhere, and everything from the group itself),
each of them a `.start` and an `.end`, the keypair the workers are reachable
under, and a `dns.recordset.create` for
`api.<shoot>.<project>.<cloud>.example.`. That record points at a placeholder
from the RFC 5737 range, because the API server of a shoot runs in Gardener's
seed and this world does not simulate one.

The tear-down after the last claim gives it all back one second apart, in the
order the resources depend on each other: a `dns.recordset.delete` for the api
record and one for the ingress record, the keypair, the security group, the
router's interface, the router, the subnet, and the network. Nothing of the
shoot follows its `network.delete.end`.

### The load balancers

A balancer holds its VIP on a neutron port with the device owner `Octavia`,
created by `port.create.start` two seconds before the billable create and its
`.end` a second later. The address follows the create. The first balancer of a
shoot carries the cluster's ingress record: a `dns.recordset.create` at +12
publishes `*.ingress.<shoot>.<project>.<cloud>.example.` with the balancer's
floating address. The balancer with the `https` listener terminates TLS, and its
certificate goes into barbican as four audit records from +20 to +23, an
`audit.http.request` and an `audit.http.response` for `POST /v1/secrets` and for
`POST /v1/containers`. All of it lies before the update the balancer's size is
booked from. A torn-down balancer is followed by the delete of its port at +1
and +2, and by four `DELETE` records from +3 to +6 when it held a certificate.

### The tenants and the CADF records

The Gardener and the CI tenants are created in keystone at the month start:
`identity.project.created` at the first second and `identity.user.created` a
second later. Each Gardener project's zone `<project>.<cloud>.example.` follows
two seconds in as a `dns.zone.create`, and the CI tenant's network
`10.100.0.0/24` with its router is built two seconds in. The classic tenants
pre-exist the month on their `192.168.<n>.0/24` networks and are announced by
nothing.

`identity.authenticate` is rendered two seconds before a shoot's creation, a
wake-up, a rolling update, a tear-down, and every CI burst, and by nothing else,
which is one record per action somebody or a controller starts.

Every CADF record is reported under its own record id, keystone's
authentications as well as barbican's audit records: a user authenticates
several times a day and two of those may fall into one second. A keypair has no
id of its own and is reported under `<user id>:<keypair name>`. The barbican
endpoint the audit records name is `https://barbican.<cloud>.example:9311`.

### The catalogue

| Notification | Exchange | Sequence |
| --- | --- | --- |
| `scheduler.select_destinations.start` | `nova` | boot |
| `scheduler.select_destinations.end` | `nova` | boot |
| `compute.instance.create.start` | `nova` | boot |
| `compute.instance.update` | `nova` | boot, delete |
| `compute.instance.exists` | `nova` | daily audit, delete |
| `compute.instance.delete.start` | `nova` | delete |
| `compute.instance.shutdown.start` | `nova` | delete |
| `compute.instance.shutdown.end` | `nova` | delete |
| `compute.instance.power_off.start` | `nova` | paired step |
| `compute.instance.power_on.start` | `nova` | paired step |
| `compute.instance.resize.start` | `nova` | paired step |
| `compute.instance.finish_resize.start` | `nova` | paired step |
| `compute.instance.shelve_offload.start` | `nova` | paired step |
| `compute.instance.unshelve.start` | `nova` | paired step |
| `keypair.import.start` | `nova` | shoot infrastructure |
| `keypair.import.end` | `nova` | shoot infrastructure |
| `keypair.delete.start` | `nova` | shoot tear-down |
| `keypair.delete.end` | `nova` | shoot tear-down |
| `volume.create.start` | `cinder` | volume |
| `volume.delete.start` | `cinder` | volume |
| `volume.resize.start` | `cinder` | paired step |
| `volume.transfer.accept.start` | `cinder` | paired step |
| `volume.attach.start` | `cinder` | volume |
| `volume.attach.end` | `cinder` | volume |
| `volume.detach.start` | `cinder` | volume, delete |
| `volume.detach.end` | `cinder` | volume, delete |
| `network.create.start` | `neutron` | shoot infrastructure, tenant |
| `network.create.end` | `neutron` | shoot infrastructure, tenant |
| `network.delete.start` | `neutron` | shoot tear-down |
| `network.delete.end` | `neutron` | shoot tear-down |
| `subnet.create.start` | `neutron` | shoot infrastructure, tenant |
| `subnet.create.end` | `neutron` | shoot infrastructure, tenant |
| `subnet.delete.start` | `neutron` | shoot tear-down |
| `subnet.delete.end` | `neutron` | shoot tear-down |
| `router.create.start` | `neutron` | shoot infrastructure, tenant |
| `router.create.end` | `neutron` | shoot infrastructure, tenant |
| `router.interface.create` | `neutron` | shoot infrastructure, tenant |
| `router.interface.delete` | `neutron` | shoot tear-down |
| `router.delete.start` | `neutron` | shoot tear-down |
| `router.delete.end` | `neutron` | shoot tear-down |
| `security_group.create.start` | `neutron` | shoot infrastructure |
| `security_group.create.end` | `neutron` | shoot infrastructure |
| `security_group.delete.start` | `neutron` | shoot tear-down |
| `security_group.delete.end` | `neutron` | shoot tear-down |
| `security_group_rule.create.start` | `neutron` | shoot infrastructure |
| `security_group_rule.create.end` | `neutron` | shoot infrastructure |
| `port.create.start` | `neutron` | boot, load balancer |
| `port.create.end` | `neutron` | boot, load balancer |
| `port.update.start` | `neutron` | boot |
| `port.update.end` | `neutron` | boot |
| `port.delete.start` | `neutron` | delete, load balancer |
| `port.delete.end` | `neutron` | delete, load balancer |
| `image.prepare` | `glance` | image |
| `image.activate` | `glance` | image |
| `identity.project.created` | `keystone` | tenant |
| `identity.user.created` | `keystone` | tenant |
| `identity.authenticate` | `keystone` | before every action a controller starts |
| `dns.zone.create` | `designate` | tenant |
| `dns.recordset.create` | `designate` | shoot infrastructure, load balancer |
| `dns.recordset.delete` | `designate` | shoot tear-down |
| `audit.http.request` | `barbican` | load balancer |
| `audit.http.response` | `barbican` | load balancer |

No recorded sample exists for any of the 62. The fixtures under
[`internal/providers/openstack/testdata/golden/notifications/`](https://github.com/B42Labs/tally/tree/main/internal/providers/openstack/testdata/golden/notifications)
are the collector's, and it maps none of these types, so a real deployment never
had one recorded there. The member sets are the simulator's own, chosen after
the legacy notification payloads of the services, and
`TestNoisePayloadsCarryTheirMembers` pins them the way the fixtures pin the
billable ones. `TestEverySeedRendersTheWholeCatalogue` holds seeds 1 to 5 to the
whole list, so a type that leaves the catalogue fails the suite.

The collector's label limiter admits 100 distinct `event_type` values, which is
`LabelValueLimit` in
[`internal/providers/openstack/metrics.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/metrics.go),
and the consumed and the skipped series share that bound. The 83 types of a
month stay inside it, so no series of a simulated month lands under
`event_type="other"`. `TestAMonthStaysInsideTheCollectorsLabelBudget` holds the
month to the bound, so a catalogue or a mapping that grows past it fails the
suite rather than silently folding the types that arrive last.

The workload renders 83 oslo notification types: the 21 of the table below,
which the collector's mapping knows, and the 62 of the catalogue above, which it
skips.

| Notification | Exchange | Billable |
| --- | --- | --- |
| `compute.instance.create.end` | `nova` | yes |
| `compute.instance.delete.end` | `nova` | yes |
| `compute.instance.resize.end` | `nova` | yes |
| `compute.instance.finish_resize.end` | `nova` | yes |
| `compute.instance.power_off.end` | `nova` | yes |
| `compute.instance.power_on.end` | `nova` | yes |
| `compute.instance.shelve_offload.end` | `nova` | yes |
| `compute.instance.unshelve.end` | `nova` | yes |
| `volume.create.end` | `cinder` | yes |
| `volume.delete.end` | `cinder` | yes |
| `volume.resize.end` | `cinder` | yes |
| `volume.retype` | `cinder` | yes |
| `volume.transfer.accept.end` | `cinder` | yes |
| `floatingip.create.end` | `neutron` | yes |
| `floatingip.delete.end` | `neutron` | yes |
| `image.create` | `glance` | no |
| `image.upload` | `glance` | yes |
| `image.delete` | `glance` | yes |
| `octavia.loadbalancer.create.end` | `octavia` | yes |
| `octavia.loadbalancer.update.end` | `octavia` | yes |
| `octavia.loadbalancer.delete.end` | `octavia` | yes |

`image.create` is rendered in the unsized form glance emits before an upload,
and the mapping skips it on purpose: the `image.upload` that follows is the
first notification with a size to bill. A load balancer is billed from its
update: the create carries no listeners and no pools, and the mapping counts
both as 0 there.

Billable here means the collector books the notification as an event, not that
the engine prices it. `pricing/2026-03.yaml`, the model
`tally-engine pricing import` loads, prices `instance`, `volume`, and
`floating_ip` and no `loadbalancer`, so a rated month counts every balancer of
it under `runs.stats.unpriced` instead of billing it. Pricing the resource type
is a change to that model and not to this simulator.

The forced steps of the classic tenants and of the shoots (the resize on the
first instance, the shelve on the second, the resize and retype of the second
instance's first volume, the rolling update of `api-prod`, the tear-down of
`batch`, and the load balancers) are what make every seed render every type
rather than most of them. The test suite holds seeds 1 to 5 over July 2026
against the recorded samples under
[`internal/providers/openstack/testdata/golden/notifications/`](https://github.com/B42Labs/tally/tree/main/internal/providers/openstack/testdata/golden/notifications),
so a sample the catalog does not render fails the suite. The check holds the
octavia types against every seed as well, because every seed's `batch` tears its
balancer down and every shoot updates the balancers it creates.

## Determinism

The shape of the classic tenants (what happens, when, and with which sizes) is
a function of `--seed` alone. The shape of the Gardener and the CI workload is a
function of the seed and the period's calendar, because their steps fall on the
working days of the month. The identifiers (the project and user ids, the
resource ids, the floating addresses, the message ids) are a function of the
seed together with the period and the cloud. The identifiers of the resources a
month churns (the workers, their root volumes, the claims, the load balancers
with their ports, listeners, pools, and addresses, and the CI runners) come from
that same salted stream at the moment the resource is created.

A third generator names the resources that exist to be announced and never
billed: the tenants' networks, the zones, the ports, the attachments, the
security group rules, the record sets, the secrets and containers, and the CADF
record ids. It is salted with the cloud and the period the way the identifier
stream is, and holding the two apart is what leaves the identifier stream
drawing the billable month alone. That holds for the message ids as well: they
are drawn over the sorted schedule once it stands, and a noise transition takes
its own from the third stream, so a catalogue one transition longer renumbers
nothing the collector books. The one place the catalogue reaches into the
billable month is the bound of a deleted instance's last lifetime step, which
lies before the five seconds of nova's pre-delete sequence rather than at the
delete itself.

The fault switches draw from streams of their own, one per switch, seeded by the
seed together with the switch's name. A fault stream carries neither the cloud
nor the month: what it hands out is which resource or notification the switch
reaches, and the message id of a refused twin, which no collector stores. None
of them draws from the stream the month's shape comes from, so a run with every
switch off consumes the three streams above the way a run without the switches
consumes them.

The traffic jitter is drawn from a fourth stream, seeded by the seed together
with a salt of its own, `metrics\x00traffic`. It never touches the shape stream,
the identifier stream, or the noise stream, so a month whose traffic was placed
consumes those three exactly as one whose traffic was not, and the notifications
of a month are the same bytes whatever its metrics say. The metrics interval
joins the seed, the period, and the cloud among the inputs the oracle depends
on: a coarser grid rounds each step's integer division differently, so two runs
of one seed under two intervals state traffic figures that differ by a few bytes
per interval.

The same seed, period, and cloud therefore publish byte-identical
notifications, and a rerun of the same build costs nothing at the far end: the
collector books the oslo message id as the event's `event_id`, and the
Reporting API stores an event once per (`event_id`, `timestamp`), which
[the collector page](/explanation/how-the-collector-consumes-a-bus) describes. Another cloud or
another month publishes fresh resources under fresh message ids, so two
collectors fed by two simulated clouds never collide on `event_id` at one
Reporting API.

Across two builds that disagree about the month it is a different matter. A
message id is the n-th draw of a stream over the schedule a build renders, so a
build that adds, drops, or moves a transition hands the same period a second
set of `event_id`, and the (`event_id`, `timestamp`) key absorbs neither set
into the other: the Reporting API then holds the period twice, with every
count, rollup, and invoice over it doubled and nothing logged. No subcommand of
`tally-reporting-admin` deletes what an ingest wrote, so the way back is to drop
the reporting database the events landed in, which for the dev cluster is
`make down` followed by `make up`. Regenerating a period against a database that
already holds it is therefore a decision, not a repeat.

Two of the fault switches fall under that rule inside one build. `pre-existing`
and `missing-create` move an instance's create across the period start, which
reorders the sorted schedule the message ids are drawn over, so every id of the
month is another one: such a run beside the plain month of the same seed,
period, and cloud leaves the Reporting API holding the period twice. The other
four keep every id, because none of them moves a transition in time or adds one
to the schedule. Publish a month with one of the two under another `SIM_CLOUD`,
or drop the reporting database before it.

Between two months of the same length the classic tenants' notifications sit at
the same offsets from the month start. The transitions anchored on the month's
end (the second image's delete and the first instance's) move with its length.
The activity of the shoots and of the CI tenant follows each month's own
working days.

## The oracle

`oracle.json` states the month the generator built. It holds the `format` of the
document, the `cloud` and the `seed` it was rendered from, the month as
`period_from` and `period_to`, the `resources` the month bills, the `counts`
of the events the collector has to record, and the `traffic` its instances
moved. A resource names its type, its id, and its workload (`classic`,
`gardener`, or `ci`), and lists the intervals of constant state, size, and
project it was billed over. The spare volume of a classic project has two of
them, meeting at the transfer that hands it to the next project. The document's
members are on
[the simulator reference page](/reference/command-line/tally-openstack-simulator#the-oracle).

The state is the one the billable notification reports, translated the way
[`mapping.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/mapping.go)
translates it. A power-off books `shutoff`, a shelve `shelved`, and an
unshelve, a power-on, and a `finish_resize` book `active`. A
`compute.instance.resize.end` books `resized` for the sixty seconds until its
`finish_resize`, because `osmap.VMState` passes
nova's `resized` through unchanged. A volume books `available` on its create
whatever attaches it a second later, which is the state the mapping fixes on
`volume.create.end`, and on a resize, a retype, and a transfer it books `in-use`
when the world holds it attached and `available` otherwise. A state change that
only a non-billable notification carries, the attach and the detach of the noise
catalogue, changes nothing in the oracle until the next billable notification
reports it. A floating IP, an image, and a load balancer are `active` from their
create to their delete.

The size is the simulator's own view of the resource, written as JSON numbers
that carry every digit. An instance holds `vcpus`, `ram_gb` (the reported memory
in MiB over 1024), `disk_gb` (the root disk plus the ephemeral one), and
`flavor`; a volume `size_gb` and `type`; an image `size_gb`, its bytes over
2^30, which is exact because every image size is a whole number of quarter
gibibytes; a floating IP `ip_version` 4. A load balancer holds `listeners` and
`pools`: 0 and 0 on its create, which is rendered without either list and whose
absent members the mapping counts as zero, and the two lengths on its update.

The fold splits an interval where the state, the size, or the project changes,
closes it on a delete, keeps two consecutive facts of equal state, size, and
project in one interval, and drops an interval of no length. Every interval is
then clipped to the month: a start before the period start moves to it, an open
end or one past the period end moves to the period end, and a resource whose
intervals all fall outside the month is left out. A month with every fault
switch off emits no instant outside the period, so the clip there only closes
the open interval of a resource that outlives it. The other end of the rule is
what the `pre-existing` and the `missing-create` switches rely on: the
transitions they move behind the period start open an interval the clip pulls
back to it.

The counts are one entry per project and Tally event type, one count per
billable notification the collector records. The event types are the mapping's:
an `image.upload` counts as `image.create`, a `compute.instance.power_off.end`
as `compute.instance.power_off`, and the transfer of a spare volume counts under
the accepting project. They are what `GET /api/v1/stats/events` of the Reporting
API holds for the month.

The traffic is one row per instance and per interval of that instance: the
`resource_id`, the interval's `from` and `to`, and the `egress_bytes` and
`ingress_bytes` the instance moved over it. The rows are ordered by resource id
and then by `from`.

```json
{
  "resource_id": "079faae9-9d39-426f-a963-769cb12aa629",
  "from": "2026-07-01T03:17:02Z",
  "to": "2026-07-02T05:36:15Z",
  "egress_bytes": 39956080648,
  "ingress_bytes": 9989019968
}
```

A row states the exact sum of the grid steps of "The metric series" that fall
inside its interval, so an interval the instance spent `shutoff`, `shelved`, or
`resized` carries 0, and an interval shorter than the grid, which holds no step
at all, carries 0 as well rather than being left out: a comparison reads a row
of every interval the oracle holds. The rows stand beside the resources rather
than inside their intervals because of the fold: two adjacent facts that repeat
the state, the size, and the project are kept in one interval, and a traffic
figure inside an interval would make two such facts compare unequal and split
that fold.

The oracle is folded by the simulator's own code out of what the generator knows
while it emits a billable transition, not out of the rendered notification and
not out of what the engine made of it. A payload that lost a member renders a
notification the collector maps to a size nobody meant, and an oracle read back
from that notification would agree with it. The fold imports neither
`internal/core/timeline` nor `internal/engine/metering`, which
`TestOracleUsesNoEngineFold` holds it to. `TestOracleAgreesWithTheMapping` holds
its vocabulary against the collector's, and `TestOracleAgreesWithTheEngineFold`
folds seeds 1 to 5 twice, once here and once through the engine over the events
the mapping makes of the same transitions, so a drift between the two folds
fails a test rather than a drill.

A run that publishes without `--out` leaves no oracle behind. A file-mode `run`
of the same seed, period, and cloud writes the oracle of the month it
published, because the same triple renders the same month byte for byte, within
one build. Another build renders another month from the same seed
as soon as the generator gains a billable transition or a size member, so the
oracle has to be folded by the binary that published the month. `format` is what
makes that visible where it can be: it is raised whenever what the generator
books, what a size holds, or what the document states changes, and `ReadOracle`
refuses a document of another format, one that states a member this build does
not read, and one that leaves a member it does read unstated. The last of the
three is the one nothing else would catch, because JSON leaves an absent member
at its zero value: an oracle without its sizes would be compared, and every
`time_gauge` dimension of every priced resource would come out as a difference
the engine did not cause. This build is at format 3, the number the `traffic`
rows were added in, so a file an earlier build wrote at format 2 is refused
rather than compared.

## The fault switches

`run --faults <name>[,<name>...]` turns fault switches on, and every one of the
six is off by default. A switch changes what the bus carries and never what the
simulated cloud did. The oracle is folded from the generator's own facts, so it
states the same intervals whichever switches are on; its counts are the events
the collector has to record, and a switch that keeps a notification off the bus
for good is the one place they move. A run with a switch on is therefore held
against the month the cloud lived, and a difference names the resource where the
collector's picture of it parted from the oracle's.

Every switch draws from a stream of its own, seeded by the seed together with
the switch's name. None of them draws from the stream the month's shape comes
from, so a run with every switch off renders byte for byte what the seed, the
period, and the cloud render on their own, and which resources one switch
touches does not move when another switch is on beside it. `missing-create`
draws from the `pre-existing` stream: the two exclude each other, and one stream
between them means both pick the same instances for one seed. A run that names
both is refused with `pre-existing and missing-create exclude each other`, and
one that names something else with `unknown fault switch`, which lists the six.

`oracle.json` states the switches the run was started with under `faults`, and
every resource carries a `faults` member of its own naming the switches that
touched it. `compare` reads both: a difference on a touched resource carries
` (touched by <names>)` behind it, and the line `the month ran with the fault
switches <names>` stands above the verdict. The verdict and the exit status are
the ones a month with no switch on prints, so a marked difference is still a
difference and counts as one. The mark says which switch reached the resource
and nothing about whether the difference is the one it was turned on for; that
is what a drill's write-up decides. A difference no mark names is a finding
about the engine.

The two pre-existing switches work on the classic tenants' instances alone, with
the volumes and the floating address of a picked instance. The shoots and the CI
runners are left out of them by decision: their servers are created and deleted
inside the month, and a classic instance is the one whose life a lead of up to
30 days moves behind the period start without changing anything else about it.

## The metric series

A run pushes the month's network traffic counters and inventory gauges to an
OTLP endpoint on the same virtual clock, and serves the inventory on the
`/metrics` path of its control listener.

### The grid

Every series of a month lies on one grid, the whole steps of
`--metrics-interval` counted from the period start. The flag defaults to `300s`,
the interval Ceilometer polls at out of the box, and it takes a whole number of
seconds between 30s and 24h. Whole seconds because a step's bytes are counted in
seconds, and a grid of half seconds would drop that half out of every step it
places; 24h because the byte arithmetic below is a product of int64 factors, and
a longer step is where it stops fitting into one; 30s because the traffic of the
whole period is placed before the first notification goes out, and a July of
seed 1 that carries about 320,000 samples on the 300s grid carries ten times
that at 30s and about a hundred million at 1s, which is where the run is killed
for its memory instead of publishing anything.

The grid is counted from the period start rather than from each instance's own
first interval, so two instances sampled around one instant carry the very same
timestamp and one query reads a step as one point per series.

### The traffic

Every instance carries two cumulative counters,
`ceilometer_network_outgoing_bytes_total` and
`ceilometer_network_incoming_bytes_total`, under the names Ceilometer's
Prometheus exporter publishes them with, so a dashboard written for a real cloud
reads a simulated one.

What an instance moves is stated per office hour and per workload: a classic
tenant's server sends 2 GiB, a shoot's worker 8 GiB, and a CI runner 256 MiB.
The ingress is the egress times 1/4 for a classic tenant, times 1/2 for a shoot,
and times 2/1 for a runner, which pulls its image and its dependencies and sends
little back; each of the three quotients is exact.

Every other hour of the month is that level weighed by the working-week profile
of "The profile" above. A step carries `hourWeight(step)/officeWeight` of the
level and the step's share of an hour, so a night step accrues a tenth of an
office step of the same length. The weight is read off the step's start, so a
step that straddles the boundary between two hours carries the weight of the
hour it began in. A jitter drawn once per instance, a numerator in [50, 150]
over a denominator of 100, keeps two instances of one workload from reporting
the same byte count without taking either of them out of its level:

```text
bytes = level * hourWeight(step) * step_seconds * jitter
        / (officeWeight * 3600 * 100)
```

`hourWeight` is 10 in an office hour, 3 in the fringe around it, and 1 at night
and on a weekend; `officeWeight` is 10. Every factor is an int64 and the one
division comes last. A byte count is a usage quantity that reaches an invoice,
and a quotient that went through a float64 would arrive carrying digits nobody
placed.

A step accrues bytes only while the interval its start falls in is `active`. A
`shutoff`, a `shelved`, and a `resized` instance move nothing, so their steps
carry zero and the counter behind them stays flat, which is what the counter of
a stopped machine does. A step that falls into a gap between two intervals
belongs to no state and accrues nothing either.

A series begins at 0 on the first grid step at or after the instance's first
interval start and ends before the end of its last interval, which is where
Ceilometer starts polling an instance and where it stops. The value at an
instant is what accrued before it, the way a cumulative counter reports: the
increment of the last step therefore lands in the oracle's last traffic row of
that instance and in no sample at all, the way Ceilometer's last poll precedes a
delete.

Five labels identify a point: `platform`, `cloud`, `resource_type="instance"`,
`resource_id`, and `project_id`, the project the instance ran in over the
interval the step lies in.

### The inventory

The inventory is the world of the month as it stands at one instant: one gauge
per live resource, the limits of every project, and the counts the cloud reports
about itself. The series it holds are on
[the simulator reference page](/reference/command-line/tally-openstack-simulator#the-inventory-endpoint).

The names and the label spellings are the exporter's own. A name this endpoint
spelled its own way is a panel that stays empty against a simulated cloud and
fills against a real one. The `id`, the `uuid`, and the `name` of a resource are
all its generated id, because the oracle these gauges are folded out of holds no
display name; the fake OpenStack API answers a listing the same way.

The `status` of a server is nova's own word, read through the same table the
fake OpenStack API answers a listing with, so it is `active`, `stopped`,
`shelved_offloaded`, or `resized` where the oracle states the state the
collector books. The memory is `ram_gb` times 1024, because nova's limits report
megabytes where a size states gibibytes. The three maxima are constants: the
simulated world has no quota, and the drilldown's gauge panels divide a used
series by a maximum one, so a panel without the divisor would show a division by
an absent series rather than a ratio. A size member an interval does not carry
reads as zero, so one malformed interval costs its own series a value instead of
failing a whole scrape.

The routers are the one family the oracle does not hold, because nothing bills a
router and the generator books none. They are folded out of the schedule's
`router.create.end` and `router.delete.end` instead, which are transitions of
the noise catalogue. The classic tenants' networks pre-exist the month and
neutron announces nothing about them, so those projects report no router at all,
which is what the simulated world holds.

The samples carry neither `platform` nor `cloud`. A real exporter carries
neither: both come from the static labels of the scrape job, and an endpoint
that stated them as well would push the job's own under `exported_cloud`. A
pushed point carries both, because a push has no scrape job to take them from.

### The push

`TALLY_SIM_OTLP_URL` is the OTLP/HTTP endpoint a run posts to, and an empty one
is a run without a push. The document is OTLP/HTTP JSON, written by
[`otlp.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/simulator/otlp.go)
rather than by the OpenTelemetry SDK: the SDK stamps a point with the instant it collected it,
and every point of a month belongs to a virtual instant months away from the
wall clock; `timeUnixNano` is where a point carries that instant. A value
travels as an `asInt` string, which keeps a byte count exact where a float64
stops being exact above 2^53. The two counters go out as a monotonic cumulative
`sum` and the inventory as a `gauge`. No metric declares a unit and the counter
names already end in `_total`, so the OpenTelemetry collector's
`prometheusremotewrite` exporter appends nothing and the store keeps the name
that was sent.

One request carries at most 5000 data points, and when a batch goes out is the
clock's decision. At the default factor of 744 the run has not reached the next
grid instant by the time it has folded this one, so the batch of a grid step
leaves as the clock passes that step, about one request per 0.4 wall seconds on
a 300s grid. At factor 0 virtual time stands still, no step ever waits, and the
batch fills to the cap instead, which is what keeps a month at factor 0 from
being one request per step.

## What a month renders

Seed 1 over `2026-07` renders 15727 notifications, 1812 of them billable. Nine
of the other 13915 are the unsized `image.create`, one per image: two per
classic project, one per Gardener tenant, and one for the CI tenant. The
remaining 13906 are the noise catalogue. The month carries 83 distinct
`event_type` values. The shape of a month is the seed's alone, so the counts
below hold on every cloud. They are those of a run with every fault switch off.
What each switch changes about them is on
[the simulator reference page](/reference/command-line/tally-openstack-simulator#the-fault-switches).

`tally_collector_skipped_total` ends the month at these values. Of the 1753
`compute.instance.exists`, 1094 are daily audits and 659 are the audit nova
sends before a delete.

| `event_type` | Value |
| --- | --- |
| `compute.instance.update` | 2672 |
| `compute.instance.exists` | 1753 |
| `port.create.start` | 676 |
| `port.create.end` | 676 |
| `compute.instance.create.start` | 671 |
| `scheduler.select_destinations.start` | 671 |
| `scheduler.select_destinations.end` | 671 |
| `port.update.start` | 671 |
| `port.update.end` | 671 |
| `port.delete.start` | 660 |
| `port.delete.end` | 660 |
| `compute.instance.delete.start` | 659 |
| `compute.instance.shutdown.start` | 659 |
| `compute.instance.shutdown.end` | 659 |
| `volume.attach.start` | 203 |
| `volume.attach.end` | 203 |
| `volume.detach.start` | 186 |
| `volume.detach.end` | 186 |
| `volume.create.start` | 169 |
| `identity.authenticate` | 158 |
| `volume.delete.start` | 146 |
| `compute.instance.power_off.start` | 29 |
| `compute.instance.power_on.start` | 29 |
| `volume.resize.start` | 18 |
| `compute.instance.shelve_offload.start` | 10 |
| `compute.instance.unshelve.start` | 10 |
| `image.create` | 9 |
| `image.prepare` | 9 |
| `image.activate` | 9 |
| `audit.http.request` | 8 |
| `audit.http.response` | 8 |
| `compute.instance.resize.start` | 7 |
| `compute.instance.finish_resize.start` | 7 |
| `dns.recordset.create` | 6 |
| `security_group_rule.create.start` | 6 |
| `security_group_rule.create.end` | 6 |
| `network.create.start` | 4 |
| `network.create.end` | 4 |
| `subnet.create.start` | 4 |
| `subnet.create.end` | 4 |
| `router.create.start` | 4 |
| `router.create.end` | 4 |
| `router.interface.create` | 4 |
| `identity.project.created` | 3 |
| `identity.user.created` | 3 |
| `keypair.import.start` | 3 |
| `keypair.import.end` | 3 |
| `security_group.create.start` | 3 |
| `security_group.create.end` | 3 |
| `volume.transfer.accept.start` | 3 |
| `dns.zone.create` | 2 |
| `dns.recordset.delete` | 2 |
| `keypair.delete.start` | 1 |
| `keypair.delete.end` | 1 |
| `security_group.delete.start` | 1 |
| `security_group.delete.end` | 1 |
| `router.interface.delete` | 1 |
| `router.delete.start` | 1 |
| `router.delete.end` | 1 |
| `subnet.delete.start` | 1 |
| `subnet.delete.end` | 1 |
| `network.delete.start` | 1 |
| `network.delete.end` | 1 |

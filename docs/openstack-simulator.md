# OpenStack notification simulator

The simulator is one binary that puts a month of oslo.messaging notifications
on a RabbitMQ broker the way nova, cinder, neutron, glance, octavia,
keystone, designate, and barbican put them there. The collector described in
[`openstack-collector.md`](openstack-collector.md) consumes them unmodified,
off its ordinary `tally-notifications` queue, and posts the mapped events to
the Reporting API, so a month of usage reaches Tally with no OpenStack
deployment behind it. The month is rendered from a small simulated cloud and
paced by a virtual clock, so a 31-day month goes out in the wall time the
factor compresses it into.

A `run` serves the month it publishes as a fake OpenStack API on the control
listener, which "The fake OpenStack API" below describes. It pushes the month's
network traffic counters and inventory gauges to an OTLP endpoint on the same
virtual clock, and serves the inventory on `GET /metrics` of that listener,
which "The metric series" below describes. #65 is the meta issue the simulated
workloads belong to, and this document describes its first stage. The noise, the
notifications the collector does not bill, is rendered and described under "The
noise" below. The oracle of a month and the comparison against an engine export
are described under "The oracle" below, the six fault switches under "The fault
switches", and the registration of the month's tenants and Gardener projects
with the project registry under "The project registry".
The drill #51 cites this simulator as the way a month reaches the Reporting API.
The workload renders octavia's three load balancer types from the shoots' load
balancers.

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
[`noise.go`](../internal/providers/openstack/simulator/noise.go) and by nothing
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
[`internal/providers/openstack/testdata/golden/notifications/`](../internal/providers/openstack/testdata/golden/notifications)
are the collector's, and it maps none of these types, so a real deployment never
had one recorded there. The member sets are the simulator's own, chosen after
the legacy notification payloads of the services, and
`TestNoisePayloadsCarryTheirMembers` pins them the way the fixtures pin the
billable ones. `TestEverySeedRendersTheWholeCatalogue` holds seeds 1 to 5 to the
whole list, so a type that leaves the catalogue fails the suite.

The collector's label limiter admits 100 distinct `event_type` values, which is
`LabelValueLimit` in
[`internal/providers/openstack/metrics.go`](../internal/providers/openstack/metrics.go),
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
[`internal/providers/openstack/testdata/golden/notifications/`](../internal/providers/openstack/testdata/golden/notifications),
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
[`openstack-collector.md`](openstack-collector.md) describes. Another cloud or
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

## Running a month against the dev cluster

The stack posts into the Reporting API of the kind dev cluster, so `make up`
comes first. Then:

```sh
make simulator-up SIM_PERIOD=2026-07
```

`SIM_CLOUD` defaults to `os-sim`, `SIM_SEED` to 1, and `SIM_FACTOR` to 744. A
factor of 744 puts a 31-day month on the bus in an hour. `SIM_FAULTS` is empty,
which is every fault switch off; it takes the switch names of "The fault
switches" below, comma-separated, as in `SIM_FAULTS=held-back`.
`SIM_REGISTER_PROJECTS` is `false`, and `true` registers the month's tenants and
Gardener projects with the dev registry before the first notification goes out,
the way "The project registry" describes; any other value is refused with
`ERROR: SIM_REGISTER_PROJECTS must be true or false` before an image is built.
`SIM_GARDEN_CLOUD` defaults to `garden-sim` and is the cloud the two Gardener
rows are registered under.

The target builds the collector and the simulator image, writes the dev CA to
`tally-ca.crt`, and issues an ingest credential for `SIM_CLOUD` with
`tally-reporting-admin create-ingest-credential`; with
`SIM_REGISTER_PROJECTS=true` it issues an admin api token beside it with
`tally-reporting-admin create-api-token --role admin`. It writes the cloud, the
period, the seed, the factor, the fault switches as `TALLY_SIM_FAULTS`, the
switch as `TALLY_SIM_REGISTER_PROJECTS`, the garden cloud as
`TALLY_SIM_GARDEN_CLOUD`, the OTLP credential of `SIM_OTLP_USER` (`tally`) and
`SIM_OTLP_PASSWORD` (`tally-dev-otlp-password`, the literal the dev overlay
generates into the `tally-otlp-auth` Secret) as `TALLY_SIM_OTLP_USER` and
`TALLY_SIM_OTLP_PASSWORD`, the grid of `SIM_METRICS_INTERVAL` (`300s`) as
`TALLY_SIM_METRICS_INTERVAL`, and the two tokens as `TALLY_OSC_TOKEN` and
`TALLY_SIM_API_TOKEN` into `deploy/compose/.env`, where the api token is the
empty string with the switch off. The file is removed and written again under
`umask 077`, because an admin api token in a world-readable file is one every
other user of the machine can register with. Then it starts the three containers
of [`../deploy/compose/compose.yaml`](../deploy/compose/compose.yaml): the
broker, the collector image, and the simulator.

It does not rebuild what runs in the cluster and applies no migration: loading
the Reporting API image into kind and applying the migration chain are both
`make up`. After a change to the Reporting API or to the migrations, run
`make up` again before this target; otherwise the stack posts into the image and
the schema the last `make up` left there.

The period has to lie in the past. `run` refuses a month that has not ended,
because the engine warns about a period that has not ended, and a simulated
month reaching into the future would carry that warning into every run over it.

Seven URLs come out of the stack:

- `http://127.0.0.1:15672`, the broker's management UI, guest/guest
- `http://127.0.0.1:8090/metrics`, the collector
- `http://127.0.0.1:8091/clock`, the simulator's control endpoint
- `http://127.0.0.1:8091/metrics`, the simulator's inventory, the database
  exporter stand-in of "The metric series"
- `https://api.tally.127-0-0-1.nip.io:8443/api/v1`, the Reporting API
- `https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics`, the OTLP endpoint the
  series are pushed to
- `https://vm.tally.127-0-0-1.nip.io:8443/targets`, the scrape targets of the
  dev cluster, where the `openstack-db-exporter` job goes green while the run
  publishes

Every host port is bound to `127.0.0.1` and lies above 1024;
`deploy/kind/kind.yaml` states why. The collector's container reaches the
cluster through `extra_hosts`, which maps `api.tally.127-0-0-1.nip.io` to
`host-gateway`, and `tally-ca.crt` is mounted read-only at the path
`SSL_CERT_FILE` names, which is what makes the Gateway's certificate verify. The
simulator's container carries the same three, and a second `extra_hosts` entry
beside them, `otlp.tally.127-0-0-1.nip.io`: a registration posts to the Gateway
over the same kind of client the collector delivers with, and the push takes
that second name to the same Gateway.

The collector service of the stack lists all eight exchanges in
`TALLY_OSC_EXCHANGES`:
`nova,neutron,cinder,glance,octavia,keystone,designate,barbican`. A topic
exchange copies a message only to the queues bound to it, so a collector
left at its default of four sees the nova, cinder, neutron, and glance noise
counted as skipped and nothing at all from the other four: no load balancer, no
tenant or authentication record, no zone or record set, and no certificate audit
record. Those series are absent from its counters rather than zero.
[`openstack-collector.md`](openstack-collector.md), "Exchanges and topics", is
where that default and the reason it leaves octavia out are described.

The simulator waits for a consumer on the collector's `tally-notifications`
queue before its first publish, because a topic exchange drops what no queue is
bound to and a month published into an empty broker is a month lost.
`--wait-for-collector` bounds that wait, two minutes by default, and `0`
disables it. A wait that runs out ends the run with an error naming the fix:
start the collector first, or pass `--wait-for-collector 0` to publish anyway.

Finish the month at once instead of waiting the factor out:

```sh
curl -X PUT -d '{"factor": 0}' http://127.0.0.1:8091/clock
```

Let the notifications a run with `SIM_FAULTS=held-back` keeps back out:

```sh
curl -X POST http://127.0.0.1:8091/release
```

Build a backlog on the durable queue and drain it again:

```sh
docker compose -f deploy/compose/compose.yaml stop collector
docker compose -f deploy/compose/compose.yaml start collector
```

The messages are persistent, so a backlog survives a broker restart as well.

A publish the broker does not confirm ends the run with exit status 1. Rerun it
with the same seed, period, and cloud: that renders the same message ids, and
ingestion deduplicates whatever was already delivered. SIGINT and SIGTERM stop
a run with exit status 0, and what went out stays out.

`make simulator-down` removes the containers, the outbox volume, and
`deploy/compose/.env`, so the next `simulator-up` starts from a stack that
carries nothing of the last one. It resets the stack and not the dev reporting
database: what a run already delivered stays ingested, and no subcommand of
`tally-reporting-admin` deletes an event. Running the same period again under
another seed or another cloud therefore adds a second, disjoint set of rows
beside the first and inflates the usage the API reports. To start the period
over, drop the ingested data first with `make down && make up`, or with

```sh
TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable' \
  go run ./cmd/tally-reporting-admin migrate-down-to 0
make migrate
```

`simulator-up` issues a fresh ingest credential every time on purpose: the dev
cluster may have been recreated since the last one, and a collector holding a
token the database no longer knows retries forever.

The month's resources are readable through the Reporting API with an admin
token:

```sh
token="$(TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable' \
  go run ./cmd/tally-reporting-admin create-api-token --role admin \
  --description 'openstack simulator')"
curl --cacert tally-ca.crt -H "Authorization: Bearer $token" \
  'https://api.tally.127-0-0-1.nip.io:8443/api/v1/resources?cloud=os-sim'
```

A run started with `SIM_REGISTER_PROJECTS=true` carries one already:
`deploy/compose/.env` holds it as `TALLY_SIM_API_TOKEN`.

## What the collector shows

Seed 1 over `2026-07` renders 15727 notifications, 1812 of them billable. Nine
of the other 13915 are the unsized `image.create`, one per image: two per
classic project, one per Gardener tenant, and one for the CI tenant. The
remaining 13906 are the noise catalogue. The month carries 83 distinct
`event_type` values. The shape of a month is the seed's alone, so the counts
below hold on every cloud. They are those of a run with every fault switch off;
what a month with a switch on carries stands under "The fault switches".

`curl http://127.0.0.1:8090/readyz` answers 200 once the collector holds its
broker connection and its outbox. On `http://127.0.0.1:8090/metrics`:

- `tally_collector_consumed_total` grows per event type while the month goes
  out, and carries the three `octavia.loadbalancer.*.end` series among the
  others.
- `tally_collector_skipped_total` climbs per type beside it, and it climbs
  faster: 13915 of the month's 15727 notifications are ones the mapping claims
  nothing for.
- Neither counter carries an `event_type="other"` series. The month's 83 types
  stay inside the bound of 100 label values the two of them share.
- `tally_collector_unparseable_total` stays 0. Anything else is a rendered body
  the collector could not read.
- `tally_collector_delivered_total` rises above 0 within two minutes at factor
  744. A counter that stays at 0 means the events sit in the outbox and the
  Reporting API is not taking them.

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

`docker compose -f deploy/compose/compose.yaml logs collector` shows neither an
`x509` error nor a `401` when the CA and the token are right.

## File mode and replay

`run --out DIR` writes three files, and a fourth when the held-back switch keeps
part of the month off the bus. It writes the whole month before it publishes
anything, so a run interrupted halfway still leaves a complete month on disk.
With no broker configured it writes the files and publishes nothing:

```sh
TALLY_SIM_CLOUD=os-sim tally-openstack-simulator run \
  --period 2026-07 --seed 1 --out /tmp/m
```

`notifications.jsonl` holds one line per notification with the `exchange` and
the `routing_key` it goes out under and the `body` it carries. That body is the
oslo envelope: an `oslo.version` of `2.0` and the notification itself as a JSON
string under `oslo.message`, which is the double encoding a real deployment
puts on the bus.

```json
{"exchange":"glance","routing_key":"notifications.info","body":{"oslo.message":"{\"event_type\":\"image.upload\",\"message_id\":\"6b6ed551-3eb5-4bea-b319-091466e44e7d\",\"payload\":{\"id\":\"72d2d72f-464a-424a-b045-587732b7a432\",\"owner\":\"d5a8024946ddf673277b9e2490643a2c\",\"size\":3489660928,\"status\":\"active\"},\"publisher_id\":\"image.glance-01\",\"timestamp\":\"2026-07-01 00:25:25.000000\"}","oslo.version":"2.0"}}
```

`events.jsonl` holds one canonical event per billable notification, computed by
the collector's own parser and mapping table rather than by the simulator's
idea of them. For seed 1 the first file therefore has 13915 more lines than the
second. A broker and `--out` combine: a run does both.

`oracle.json` is the third of them, the generator's statement of what the month
was meant to meter to: the intervals every billable resource was to be billed
over and the events the collector has to record per project. "The oracle" below
describes it.

`held-back.jsonl` is the fourth, in the form of `notifications.jsonl` and
holding what the held-back switch keeps back. Every run removes all four files
an earlier one left in the directory before it writes the first of its own, so
what a directory holds is one month and nothing of another, whether the run
holds something back, holds nothing, or fails partway through the files. A path
one of the removals cannot take away ends the run there, with the rest gone,
and every path that stayed is named.

`replay --in FILE --factor N` publishes a captured file onto a broker, with the
virtual clock started at the first line's timestamp, so a month needs neither
the generator nor the seed that produced it. The message ids are the recorded
ones, so a second replay of the same file is deduplicated at ingest. A line
whose timestamp lies before the previous one is published at once, which is
what lets a file that is not perfectly sorted replay whole.

`ReadStream` reads the file before the first message goes out and refuses an
empty file, a line that is not JSON, and a body without a timestamp. A replay
that would fail halfway through a month fails with nothing published.

A held-back file is replayed the way any other stream file is:
`replay --in DIR/held-back.jsonl --factor 0` puts it on the bus at once. The
factor is what matters there. The replay clock starts at the first line's
timestamp and the held instants are spread over the whole month, so a replay at
744 would trickle the file out over an hour. A `pre-existing` month replays from
its earliest pre-month create rather than from the month start, for the same
reason: the clock starts at the first line, and the burst a run publishes ahead
of the month is paced by the lead the switch drew.

## The oracle

`oracle.json` states the month the generator built. It holds the `format` of the
document, the `cloud` and the `seed` it was rendered from, the month as
`period_from` and `period_to`, the `resources` the month bills, the `counts`
of the events the collector has to record, and the `traffic` its instances
moved. A resource names its type, its id, and its workload (`classic`,
`gardener`, or `ci`), and lists the intervals of constant state, size, and
project it was billed over. The spare volume of a
classic project has two of them, meeting at the transfer that hands it to the
next project:

```json
{
  "resource_type": "volume",
  "resource_id": "1746df50-32e2-480a-9cc2-8773e40058a9",
  "workload": "classic",
  "intervals": [
    {
      "from": "2026-07-01T04:39:38Z",
      "to": "2026-07-20T08:06:41Z",
      "state": "available",
      "project_id": "34e991db9fc6466f8ca69b43f70fce65",
      "size": {
        "size_gb": 200,
        "type": "hdd"
      }
    },
    {
      "from": "2026-07-20T08:06:41Z",
      "to": "2026-08-01T00:00:00Z",
      "state": "available",
      "project_id": "018504a6cc10019a40e3f9eef4dae529",
      "size": {
        "size_gb": 200,
        "type": "hdd"
      }
    }
  ]
}
```

The resources are sorted by type and then by id, the intervals by their start,
and the counts by project and then by event type. Two runs of one seed, period,
and cloud therefore write a file a diff reports nothing about.

The state is the one the billable notification reports, translated the way
[`mapping.go`](../internal/providers/openstack/mapping.go) translates it. A
power-off books `shutoff`, a shelve `shelved`, and an unshelve, a power-on, and
a `finish_resize` book `active`. A `compute.instance.resize.end` books `resized`
for the sixty seconds until its `finish_resize`, because `osmap.VMState` passes
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

The compose stack publishes without `--out` and leaves no oracle behind. A
file-mode `run` of the same seed, period, and cloud writes the oracle of the
month it published, because the same triple renders the same month byte for
byte — within one build. Another build renders another month from the same seed
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

### Comparing an export

Three commands hold what the engine billed against the oracle:

```sh
tally-engine run --period 2026-07
tally-engine export --run <id> --format csv --out /tmp/export
tally-openstack-simulator compare --oracle /tmp/m/oracle.json \
  --export /tmp/export --pricing pricing/2026-03.yaml
```

`compare` reads `rated.csv` out of the export directory and the pricing model
the run rated with. Per resource it compares the bounds of every interval, the
state and the project it was booked under, and the quantity of every
`time_gauge` dimension the model prices the resource type by: a dimension named
after a size member against that member, `count` against 1, `minutes` against
the interval's whole seconds over sixty, and a member the size lacks or holds as
text against 0. The quantities are compared as decimals rounded to four places,
which is the rounding the export prints. The intervals of the two sides are held
against each other by index rather than by their bounds, so an interval one fold
split in two is reported at the place the two folds part ways rather than on
every interval behind it.

The `counter` dimensions are left out, `egress_gb` among them: `increase()`
extrapolates a counter over the edges of the window it is asked about, so the
quantity an export carries differs from the exact integral the generator placed
in the interval, and the `traffic` rows of the oracle are what a drill reads
that figure off instead. Left out as well are the amounts, the state modifiers,
the statement totals, and the resource types the model does not price. Under
`pricing/2026-03.yaml` an `image` and a `loadbalancer` are unpriced, so the
rating pass writes no record of one, and the report names each such type on a
line of its own instead of calling its resources missing:

```text
image: 9 resources are not priced by pricing model 2026-03 and were not compared
loadbalancer: 5 resources are not priced by pricing model 2026-03 and were not compared
```

Rated records of another cloud or another platform are skipped and counted on
one more line, `skipped N rated records of other clouds or platforms`, which is
what an export of a deployment that bills more than the simulated cloud carries.

`rated.csv` is read rather than the JSON statements because a statement period
carries hours and no timestamps and folds attributed projects into related
costs, so an interval cannot be matched against it. A rated record carries
`from_ts`, `to_ts`, `state`, `project_id`, `dimension`, and `quantity`, and its
`project_id` is the usage draft's project, the tenant that owned the resource.

A difference is one line, `<resource_type> <resource_id>: <detail>`, with
` (and N more)` appended when the resource carries further ones: a resource
booked under the wrong project differs in every interval it has, and a report
that spelled all of them out would bury the resource beside it. The lines are
sorted by resource type and then by id, and every instant in them is written in
UTC as RFC 3339. The details:

- `missing from the export` and `not in the oracle`, for a resource one side
  holds and the other does not.
- `the export lacks [a, b)` and `the export books [c, d), which the oracle does
  not hold`, when the two sides carry a different number of intervals for one
  resource.
- `the oracle expects [a, b) and the export books [c, d)`, for two intervals
  that differ in their bounds.
- `state "x" over [a, b), the oracle expects "y"` and `project x over [a, b),
  the oracle expects y`.
- `<dimension> <actual> over [a, b), the oracle expects <expected>`, both at
  four places, and `no <dimension> quantity over [a, b)` when the export books
  the interval without that dimension.
- `the export books [a, b) under more than one state or project` and `the export
  rates [a, b) by <dimension> more than once`, the two details that stop the
  comparison of their resource: the first because there is no single booking to
  hold the oracle against, the second because a month billed twice over carries
  the very same row twice, and the quantity read back off the second one is the
  right quantity.

The last line is the verdict, `the export matches the oracle over N resources`
or `N of M resources differ from the oracle`. A comparison that matches exits 0.
One that differs exits 1, and cobra prints the error `N resources differ from
the oracle` on stderr under the lines, so a drill that runs unattended fails
where it ran. Every other failure exits 1 as well.

Four exports are refused rather than compared, because each of them would turn
every resource of the month into a difference: a `rated.csv` without a rated
record of the oracle's cloud on platform `openstack` (`rated.csv holds no rated
record of cloud os-sim on platform openstack: the run that wrote it did not bill
this month`), one whose period columns name another month (`rated.csv bills
[a, b) and the oracle describes [c, d)`), one rating a resource type or a
dimension the model at hand does not price (`rated.csv rates instance by vcpus,
which pricing model 2026-03 does not price: pass the model the run rated with`),
and one the model prices a rated resource type by a `time_gauge` no record rates
it by (`pricing model 2026-03 prices instance by disk_gb and rated.csv rates no
record by it: pass the model the run rated with`). The last two are one gate held
in both directions: a model that prices otherwise than the run rated with is not
the model to read an export through, whichever of the two names the dimension. A
`counter` is outside it, because a comparison reads none of them.

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

### pre-existing

One in three of the twelve classic instances starts before the month, between 1
and 30 days ahead of it, and its volumes and its floating address start with it.
Every transition of such an instance is published, the ones before the period
start included: the virtual clock starts at the month start, so they go out in a
burst ahead of the month's own first notification. The engine holds the whole
history of the resource and bills it from the month start, and the oracle clips
its intervals to that same instant, which is the rule "The oracle" states.

No notification is added or dropped, so the collector's counters are the ones a
month with no switch on shows: 1812 consumed and 13915 skipped for seed 1 over
`2026-07`, 60 of the consumed events carrying a timestamp before the month. The
engine writes no warning about them, and the comparison reports no difference.
What the switch shows is a resource whose life began before the period, billed
over the part of it the period holds.

Seed 1 over `2026-07` moves 5 instances, their 8 volumes, and their 5 floating
addresses behind the month start, 18 resources the oracle marks `pre-existing`,
and renders 176 transitions before the period start, 60 of them billable. The
month holds the 15727 notifications and the 1812 billable events of the month
with the switch off.

### missing-create

The switch picks the same instances `pre-existing` picks, with the same leads,
and drops every transition before the period start from the schedule and from
the stream. The bus carries a resource whose create it never saw, and the
collector first hears of one of them through a notification from inside the
month. The daily `compute.instance.exists` audits stay: the audit pass runs over
the finished schedule before the drop, so an instance whose create was dropped
is still reported by the audit of every day it exists on.

The engine warns `history_starts_without_create` once per touched resource it
sees, which is what a deployment collects when a collector was started
mid-month. The oracle states every touched resource with its intervals clipped
to the month start, the way it does with `pre-existing`, and its counts drop the
events of the notifications the bus never carried. A resource the collector
first sees mid-month is billed from that notification on, so the comparison
reports it as a difference, marked ` (touched by missing-create)`. No correction
closes that gap: the notifications were never delivered, so a later run over the
same period reads the same history. A difference the mark does not name is a
finding about the engine.

Seed 1 over `2026-07` drops the 176 pre-month transitions of the same 18
resources, 60 of them billable: 15551 notifications instead of 15727 and 1752
billable events instead of 1812. The collector ends the month with 1752 consumed
and 13799 skipped.

### duplicates

One in 20 of the billable transitions is published a second time, byte for byte
and under the message id of the original, ten notifications later or behind the
last one when the month ends before the distance is walked. The copy travels the
route its original travels and the collector maps both, so
`tally_collector_consumed_total` counts the repeat. The Reporting API stores one
event per (`event_id`, `timestamp`) and counts the second under
`tally_events_deduplicated_total`, which is the deduplication a rerun of a month
relies on as well. The export and the comparison are those of the month with the
switch off, and the engine writes no warning.

Seed 1 over `2026-07` adds 86 copies on 85 resources: 15813 notifications
instead of 15727, with `events.jsonl` and the oracle at the same 1812 events.

### reordering

One in 10 of the resources with at least two billable transitions has its first
one published directly behind its second. The timestamps do not move: what
changes is the order the collector consumes them in, the order a requeued
delivery or a second consumer produces. The projection and the engine sort a
resource's history by timestamp before they fold it, so the export is the export
of the month with the switch off, the comparison reports no difference, and no
counter of the collector moves.

Seed 1 over `2026-07` swaps the first two billable notifications of 87
resources, in a month of the same 15727 notifications and 1812 events.

### refused-shapes

Per billable transition the switch draws once in 400: one draw puts an oversized
twin behind it, two a truncated one, and twenty a versioned one, which is drawn
for nova alone because the versioned format is nova's. A twin follows its
original directly, on the same exchange and under a fresh message id, and the
collector refuses all three.

The versioned twin is what a nova configured for versioned notifications would
have sent: an `instance.*` type name and the payload under `nova_object.data`,
the format [`openstack-collector.md`](openstack-collector.md) refuses under
"Required OpenStack service settings". `ParseEnvelope` reads it and the mapping
table claims nothing for the type, so it is counted as skipped under its
versioned type name. The truncated twin is an envelope whose inner
`oslo.message` is cut in half, which the collector's second decode fails on. The
oversized twin carries a padding member that puts its body past the 1 MiB the
collector reads a delivery with, so it is counted before anything is parsed.
Both of them count as unparseable. `events.jsonl`, the oracle, and the
comparison are those of the month with the switch off, because a twin is
billable to nobody.

The `notifications.jsonl` of such a month is written like any other and cannot
be replayed. `ReadStream` bounds a line at 1 MiB, which the oversized twin is
past, and it runs the collector's `ParseEnvelope` over every body, which the
truncated twin fails. Either one ends the replay before the first message goes
out.

Seed 1 over `2026-07` adds 87 twins on 84 resources, 67 versioned, 15 truncated,
and 5 oversized: 15814 notifications instead of 15727. The collector ends the
month with `tally_collector_unparseable_total` at 20 and three `instance.*`
series beside the month's 83 type names, which leaves it inside the bound of 100
label values its two type-labelled counters share.

### held-back

One in 20 of the billable transitions is kept off the bus and written to
`held-back.jsonl` instead. The run publishes the rest of the month and then
holds: `/clock` reports the hold under `holding` and its size under `held`, and
the notifications go out when `POST /release` arrives. A run stopped while it
holds exits 0 with the held share never published. What the switch renders is a
late arrival, the events that reach the Reporting API after the run that bills
the period read it.

The stack publishes the month with the switch on, and a file-mode run of the
same seed, period, and cloud writes the oracle of it, the way "The oracle"
describes:

```sh
make simulator-up SIM_PERIOD=2026-07 SIM_FAULTS=held-back
TALLY_SIM_CLOUD=os-sim tally-openstack-simulator run \
  --period 2026-07 --seed 1 --faults held-back --out /tmp/m
```

The stack holds once `http://127.0.0.1:8091/clock` reports `holding` true. The
month as the collector recorded it is metered, closed, and held against the
oracle:

```sh
tally-engine run --period 2026-07
tally-engine finalize --period 2026-07 --run <run id>
tally-engine export --run <run id> --format csv --out /tmp/export
tally-openstack-simulator compare --oracle /tmp/m/oracle.json \
  --export /tmp/export --pricing pricing/2026-03.yaml
```

That comparison lists the held resources as differences, each marked
` (touched by held-back)`, because the run rated the month without their
notifications. Then the held share goes out and the closed month is corrected:

```sh
curl -X POST http://127.0.0.1:8091/release
tally-engine detect-late --period 2026-07
tally-engine correct --period 2026-07
tally-engine export --run <correction id> --format csv --out /tmp/corrected
tally-openstack-simulator compare --oracle /tmp/m/oracle.json \
  --export /tmp/corrected --pricing pricing/2026-03.yaml
```

`detect-late` names the events the reporting database received after the run
that bills the period read it, which are the released ones. `correct` meters the
month again from the full history and books the difference against the finalized
run as deltas. The comparison of its export matches the oracle over every
resource, and the line naming the switch stands above that verdict as it stands
above the other.

The collector's counters carry the hold: 1728 consumed while the month
publishes, 1812 once the release is through, and 13915 skipped either way. Seed
1 over `2026-07` holds 84 notifications back on 81 resources, so
`notifications.jsonl` has 15643 lines, `held-back.jsonl` 84, and the month books
the 1812 events of the month with the switch off.

## The project registry

`run --register-projects` registers the month with the project registry of the
Reporting API, before the first file is written and before the first
notification goes out. The switch is off by default: a run without it puts a
month on a bus or into files and reads no registry at all.

Four variables carry what a registration needs, and a run without the switch
reads them and ignores them. `TALLY_SIM_REPORTING_URL` is the Reporting API the
rows are posted to. It has to be absolute and carry a host, with no query and
no fragment, because the registry route is appended to it, and it has to be
`https`: the api token travels in a header on every request, and that token is
of role `admin`, so it is not scoped to one `(platform, cloud)` pair the way an
ingest token is, and whoever reads it off the wire writes the whole registry.
`TALLY_SIM_REPORTING_INSECURE=true` is how a plaintext one is asked for, the way
the collector's `TALLY_OSC_REPORTING_INSECURE` is; the compose stack posts to
the Gateway over `https` and sets neither. `TALLY_SIM_API_TOKEN` is the
credential, an api token of role `admin`, because `POST /api/v1/projects` and
`POST /api/v1/projects/{id}/relations` demand that role; it is a secret, so
`TALLY_SIM_API_TOKEN_FILE` carries it as well, and setting both is an error.
`TALLY_SIM_GARDEN_CLOUD` is the cloud the two Gardener rows are registered
under, and it has to differ from `TALLY_SIM_CLOUD`: a cloud is one installation
of one platform, so a Gardener project keyed under the tenants' cloud would be a
row of the OpenStack installation, and its relation would then point it at
itself. A missing or mistyped value ends the subcommand with `checking the
configuration: ...` before a broker is dialled.

A month registers eight rows and two relations. Six rows are `openstack` rows
under `TALLY_SIM_CLOUD`, keyed by the keystone project id of the tenant and
named `tenant-01`, `tenant-02`, `tenant-03`, `ci`,
`Infrastructure tenant of alpha`, and `Infrastructure tenant of beta`. Nothing
on the bus carries a tenant name, so the registry is the one place the ids of a
month are given one. The other two are `gardener` rows under
`TALLY_SIM_GARDEN_CLOUD`, with the external ids `alpha` and `beta` and the
names `Gardener project alpha` and `Gardener project beta`. Each Gardener row
then takes one relation of type `infrastructure_tenant` to the tenant its shoots
run on, valid from the first instant of the month (`valid_from`
`2026-07-01T00:00:00Z` for `2026-07`) and with no end. Every row and every
relation carries `created_by: tally-openstack-simulator` in its metadata, and a
relation's metadata carries `shoots` beside it: `["api-prod", "api-dev"]` for
`alpha`, `["batch"]` for `beta`.

A rerun of the same seed, period, cloud, and garden cloud posts the same eight
rows again. A `409` on a project is a row the registry already holds: it is
looked up by its `(cloud, external_id)` key, its id is what the relations point
at, and neither its name nor its metadata is patched. A `409` on a relation is
one that is already active, and it stays as it stands. The run goes on and logs
`registered` with `projects_existing` 8 and `relations_existing` 2.

A rerun under another period, seed, or cloud is refused at the first relation.
The Gardener rows are keyed by `alpha` and `beta` and are the ones the earlier
run registered, while the tenants are keyed by identifiers the month is salted
into, so the second run would relate one Gardener project to a second tenant.
The registry keys an open relation by `(source_id, target_id, relation_type)`,
which makes that a new relation rather than a `409`, and two relations
attributing at once put two months of a tenant's cost into one statement. Every
relation is therefore preceded by two reads of
`GET /api/v1/projects/{id}/relations?direction=outgoing&relation_type=infrastructure_tenant`,
one at `at=<the first instant of the month>` and one as the relations stand now,
and a relation to another tenant in either answer ends the run. What the message
says is decided by when that relation starts. One that started in an earlier
month is ended where this month begins, so the message names the
`PATCH /api/v1/projects/{id}/relations/{relation_id}` that does it and the
instant it has to end at. One that starts at or after the first instant of this
month cannot be: `PATCH` answers a `valid_to` that is not after the stored
`valid_from` with `422`, so there is no such instant, and the message says so
and leaves registering this month under another garden cloud
(`TALLY_SIM_GARDEN_CLOUD`) as the way on.

Two reads, because the engine bills a period by the relations that overlap it
(`valid_from < period_to AND (valid_to IS NULL OR valid_to > period_from)`) and
not by the ones that are open while it runs. The read at the first instant of
the month is one instant the period is billed by, and it finds a relation that
was closed after the month as well: `DELETE /api/v1/projects/{id}/relations/{relation_id}`
sets `valid_to` to now, which is after every instant of a simulated period, so
such a relation goes on attributing that period. The read as they stand now
finds a relation that starts after this month and would attribute beside the new
one from its own start on. A relation that ends no later than the first instant
of the month is left alone by both: it attributes nothing of the month being
registered.

Two reads at two instants are less than that overlap, though, so this check is
what one run can see and not a rule the registry enforces. A relation that
starts inside the month and was closed no later than now is valid at neither
instant, and it attributes the month all the same: such a relation passes the
check, and a second one is created beside it. Reading the overlap itself would
need a `from`/`to` filter on the route, which this API does not offer; the
invariant the check stands in for belongs to the Reporting API, as one open
attributing relation per `(source_id, relation_type)`, answered `409`. Two
registrations running at once are past the check for the same reason: both can
read an answer without the other's relation and both create one.

A registration that fails ends the run with exit status 1, with no file written
and no notification published. A refused token is the one failure whose fix is
not in the answer, so its message ends with
`TALLY_SIM_API_TOKEN has to be an api token of role admin`; an unreachable API
and any other unexpected status end the run the same way. The rows such a run
got through stay in the registry, and the rerun finds them. SIGINT while the
registration runs is the clean stop the rest of a run answers a signal with,
exit status 0.

File mode registers as well.
`run --register-projects --period 2026-07 --out /tmp/m` needs no broker and
registers the month it writes, which is how an operator prepares the registry
before a `replay` puts that month on a bus. `replay` itself registers nothing,
because the recorded notifications are all it reads.

The log carries the registration. `starting` says `register=true`, and
`registered` closes the registration with `reporting_url`, `projects_created`,
`projects_existing`, `relations_created`, and `relations_existing`. Every step
ahead of it is one `Info` line of its own: `registered project` or
`project already registered` per row, `related` or `relation already active`
per relation. A registration that ends early logs `registration incomplete` with
the same four counts instead, whether it was refused or stopped: the rows it got
through stay in the registry, and that line is where the whole of what reached
it is.

The broker of a run is refused unless it is on this machine, and
`--allow-remote-broker` is the confirmation that lets one through. The Reporting
API is guarded by its scheme instead: a plaintext one is refused unless
`TALLY_SIM_REPORTING_INSECURE=true` is set, because the admin token travels on
it. Which deployment the rows land in is not guarded beyond that — registering
needs an api token of role `admin`, and issuing one against a deployment is that
decision already.

The registry of the compose stack is readable with the admin token
`simulator-up` wrote into `deploy/compose/.env`. That file is written under
`umask 077`, because the token in it writes the whole registry; the id it was
issued under is on the `simulator-up` output, and
`tally-reporting-admin revoke-api-token <id>` is what ends it, because
`simulator-down` deletes the file and revokes nothing.

```sh
curl --cacert tally-ca.crt \
  -H "Authorization: Bearer $(grep TALLY_SIM_API_TOKEN deploy/compose/.env | cut -d= -f2)" \
  'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=os-sim'
```

The relations hang below the row they start at, so walking alpha's takes its id
first: the `id` of the `garden-sim`/`alpha` row that
`GET /api/v1/projects?cloud=garden-sim` lists.

```sh
curl --cacert tally-ca.crt \
  -H "Authorization: Bearer $(grep TALLY_SIM_API_TOKEN deploy/compose/.env | cut -d= -f2)" \
  'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/<alpha id>/related?relation_type=infrastructure_tenant'
```

The rows outlive `make simulator-down`, which drops the containers and the
outbox volume and touches the dev registry not at all, so a second
`simulator-up` for a later period is the rerun the registration refuses until
the relations of the first one end no later than the first instant of the second
period. No subcommand of `tally-reporting-admin` deletes a project, and a
relation is never removed: `PATCH /api/v1/projects/{id}/relations/{relation_id}`
with a `valid_to` is what ends one at a chosen instant, and
`DELETE /api/v1/projects/{id}/relations/{relation_id}` ends one at now, which is
after the period a simulated month covers and therefore no way past the check.
A second `simulator-up` for the same period or an earlier one has no instant to
end them at either, because they start at or after that period's first instant:
those runs need a garden cloud of their own, `TALLY_SIM_GARDEN_CLOUD`.

What the rows are for shows once the month is billed. The engine's default
attributing relation type is `infrastructure_tenant`, so

```sh
tally-engine run --period 2026-07
tally-engine export --run <id> --format json --out /tmp/statements
```

writes `statement-garden-sim%2Falpha.json` and
`statement-garden-sim%2Fbeta.json`. The two Gardener projects have no usage of
their own, and a statement is opened for each of them all the same: their
`related_costs` carry the line items of the tenant the shoots run on, under the
relation type `infrastructure_tenant`. The two Gardener tenants get no statement
of their own, and `runs.stats.unregistered_projects` names nothing, because
every tenant the month books usage for is registered. `rated.csv` does not
change and neither does `compare`: a rated record carries the tenant that owned
the resource as `project_id`, the attribution stands in the statements alone,
and `compare` reads `rated.csv`.

## The control endpoint

Both subcommands serve a control endpoint on `TALLY_SIM_HTTP_ADDR` and
`TALLY_SIM_HTTP_PORT` while they publish and no longer: what it decides is the
pace of the month and, for a run with the held-back switch, when the held share
goes out. Neither of the two is what the month contains, and there is nothing to
decide before the first notification or after the last one. It carries no
credential, so what keeps it out of reach is the address it binds: loopback,
unless a deployment names another one. In the compose stack it binds `0.0.0.0`
inside the container, where loopback would be reachable from nothing, and the
container's port 8080 is published on the host's `127.0.0.1:8091`. Within that
reach the worst a caller does is make the run go at another speed, or end its
hold early.

`GET /healthz` answers `ok`. `GET /clock` answers the document of that moment:

```json
{"virtual_now":"2026-07-09T14:22:00Z","factor":744,"published":52,"total":15727,"held":84,"holding":false,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
```

`held` is how many notifications the held-back switch still keeps off the bus:
0 for a run started without it, and 0 from the moment a release let them out.
`total` counts the regular and the held lines together, so `published` reaches
it only once everything is out.

`holding` is whether the run waits for a release, and it is the one member a
release may be sent on. It turns true when the last regular notification is on
the bus and false again when a release lets the held share out. `published`
equal to `total` minus `held` is the same month a moment earlier: the count is
raised by the notification that was confirmed, and the run enters the hold
after it, so a release sent on the count alone can still be refused.

`PUT /clock` with a body of `{"factor": N}`, where N is zero or positive,
rebases the clock on the virtual instant it has reached and answers the same
document. The month itself does not move: only what comes after the change runs
at another speed. A body that is not JSON, one without the member, and one with
a negative factor all answer 400 with
`factor must be a JSON object with a number member "factor" that is zero or
positive`. A factor change a page in a browser sent answers 403 with `the factor
does not take a request a browser sent`.

`POST /release` publishes the share a run with the held-back switch kept back.
It answers 200 with the clock document as it stood the moment before the
release, so the document reports the published count of a month one release
short however fast the run publishes the rest; `held` in it is 0 and `holding`
false, the two members the release changed. The three refusals answer 409 and
name the run they arrived at:

- `nothing is held back: the run was started without the held-back switch`
- `the month is still publishing; release once /clock reports holding true`
- `the held-back notifications were already released`

A release a page in a browser sent answers 403 with `release does not take a
request a browser sent`, the way a factor change does. A browser puts `Origin`
on every request a page makes whose method is neither `GET` nor `HEAD`, the
same-origin ones included, and `Sec-Fetch-Site` alongside it, whether the page
submitted a form, called `fetch` without a body, or reached for `sendBeacon`;
that is what keeps a page an operator happens to visit from ending a hold or
changing the pace. A cross-origin `PUT` is stopped a step earlier, by the
preflight the endpoint answers 405, but a page that resolved the endpoint's
address itself sends one same-origin, where no preflight stands in the way. A
release with a body of another media type than `application/json` answers 415
with `release takes application/json or no body`. `curl -X POST` and a script
send neither header nor a content type and are unaffected.

Any other method on one of the three routes answers 405. A run in file mode
serves nothing.

## The fake OpenStack API

Reconciliation is the one path a stream of notifications cannot reach. A sync
asks the cloud what it holds right now, diffs that against the projection the
notifications built, and books the difference as corrections, which
[`openstack-reconciliation.md`](openstack-reconciliation.md) describes. A `run`
therefore serves the month it publishes as an OpenStack API beside the bus, out
of the same oracle a comparison holds an export to. The two faces state one
world, so the only drift a sync finds is the drift a fault switch caused.

`POST /v3/auth/tokens` is the keystone v3 password request, and the credentials
are the fixed pair `tally-sync` and `tally-dev-sync-password`. Whatever
authenticates here is configured from a clouds.yaml this repository carries, and
a drawn password would be one nobody can write into that file. Every run
issues one token of its own, which every other route demands in `X-Auth-Token`,
so a client holding the token of a run that has ended is told so rather than
served the month that came after it. The catalog the token carries is built from
the Host header of the request that asked for it, so a pod reaching the API
under `host.docker.internal:8091` and a test reaching it under a local address
each read a catalog pointing back at the address they used.

The listings are the ones the reconciliation adapter reads: nova's servers,
cinder's volumes, neutron's floating addresses, glance's images, and octavia's
load balancers. Nova's path serves two of them. A request carrying
`deleted=true` with a `changes-since` instant is answered with the instances the
month destroyed inside that window, which is the listing that dates a missed
delete at the instant the platform performed it; everything else is the live
listing, the admin-scope probe a run sends before it observes anything included.
The flavor catalog stands beside them and goes unread where a client negotiates
the microversion that carries a server's flavor in the server: nova publishes
2.96 here, past the 2.47 that microversion arrived in.
Every document is answered from the oracle at the instant the run's virtual
clock stands at, clamped to the end of the month, so a clock that ran past the
month still serves the resources that outlived it.

The API is up for exactly as long as the control endpoint and on the same
listener: while a `run` publishes or holds, at `TALLY_SIM_HTTP_ADDR` and
`TALLY_SIM_HTTP_PORT`, which the compose stack publishes on the host's
`127.0.0.1:8091`. The control routes are method-qualified and are matched first,
so each of them answers what it answered before and every path they leave
reaches the month. A run in file mode serves neither of the two. A replay serves
the control routes alone, because a recorded file holds the notifications of a
month and no oracle to answer a listing out of: `POST /v3/auth/tokens` on a
replay is a path no route claims and is answered 404.

What a sync finds follows from the switches the run was started with.
`pre-existing` publishes every transition of the instances it moves behind the
month start, so the projection holds them and a sync corrects nothing about
them; what it gives a drill is a resource whose life began before the period,
served as created at the period's first instant, because that is where the
oracle clips it. `missing-create` drops those pre-month transitions, so the
projection holds nothing of a touched resource until a notification from inside
the month names one, and a sync that runs before that books a `sync.create` for
it at the period's first instant. `held-back` keeps one in 20 of the billable
transitions off the bus until `POST /release`, so the projection lags the cloud
for as long as the run holds: a resource the projection does not hold yet is
created, a size or a state that changed without a notification is updated, and
an instance the month deleted is booked at the `terminated_at` nova reports, as
long as that instant is inside the 24 hours the deleted window is clamped to. A
held delete of a volume, an address, an image, or a load balancer is found by
absence and dated at the instant the sync was told it runs at.

`pre-existing` and `missing-create` exclude each other, so a fault list carries
at most one of them: `pre-existing,held-back` is a month with lives that began
before the period and a projection that lags, and `missing-create,held-back` one
whose pre-month creates never arrive.

The dev Kubernetes overlay wires the Reporting API to the fake API.
`deploy/kubernetes/overlays/dev/reconciliation/` holds the clouds config and the
clouds.yaml the pod reads. The clouds config is generated into a ConfigMap and
the clouds.yaml into a Secret, because that one carries the cloud's password,
and the two are projected together at `/etc/tally/clouds`. The overlay sets
`TALLY_REPORTING_SYNC_ALLOW_AT` to true. The cloud is `os-sim`, the `SIM_CLOUD`
default, reached from the pod in the kind node at
`http://host.docker.internal:8091/v3`. A drill under another cloud name edits
both files of that directory.

The internal sync route is not published through the Gateway, so a drill reaches
it over a port-forward, and what it tells each sync is where the month stands,
which `GET /clock` answers under `virtual_now`, held at the `period_to` of the
same document:

```sh
kubectl -n tally port-forward svc/reporting-api 8082:80 &
while true; do
  doc="$(curl -s http://127.0.0.1:8091/clock)"
  at="$(jq -r 'if .virtual_now > .period_to then .period_to else .virtual_now end' <<<"$doc")"
  curl -sS -X POST -H "Authorization: Bearer tally-dev-internal-token" \
    -H 'Content-Type: application/json' -d "{\"at\": \"$at\"}" \
    http://127.0.0.1:8082/internal/sync/os-sim
  echo
  sleep 60
done
```

`tally-dev-internal-token` is the internal token of the dev overlay. Sixty wall
seconds are about 12.4 virtual hours at the default factor of 744, which keeps
two consecutive instants inside the day the deleted-servers window is clamped
to, so a delete the run held back is booked at nova's own `terminated_at` rather
than at the instant the sync was told.

The clock is held at the end of the month because the listings are: a clock past
`period_to` is answered with the inventory of the month's last instant, and an
`at` past it would date every correction that inventory causes outside the period
a drill compares. The hold of `held-back` is where that happens by itself — the
run waits for `POST /release` while virtual time keeps running at the factor, so
five wall minutes spent looking at the hold are about 62 virtual hours past the
end of the month.

The loop belongs inside the publishing month: the fake API goes down with the
run, and the instants told to the runs of one cloud must not go backwards.
Nothing schedules it here and no target starts it; the drill of #51 folds it
into its procedure.

## The metric series

A `run` states the month a second time, as the series a monitoring stack would
have recorded while it went by: the two network counters Ceilometer polls off
every instance, and the inventory gauges the OpenStack database exporter reads
off the service databases of a real cloud. Both are placed by
[`metrics.go`](../internal/providers/openstack/simulator/metrics.go) out of the
month the notifications describe, so the pushed series, the scraped endpoint,
and the oracle state one world instead of three descriptions of it. A `replay`
places neither of the two: a recorded file holds notifications and no month to
fold them out of.

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
about itself.

| Series | Labels | Value |
| --- | --- | --- |
| `openstack_nova_server_status` | `id`, `uuid`, `name`, `tenant_id`, `status`, `flavor_id` | 1 |
| `openstack_cinder_volume_status` | `id`, `name`, `tenant_id`, `status`, `volume_type` | 1 |
| `openstack_cinder_volume_gb` | `id`, `name`, `tenant_id`, `volume_type` | the volume's `size_gb` |
| `openstack_glance_image_bytes` | `id`, `name`, `tenant_id` | the image's size in bytes |
| `openstack_neutron_floating_ip` | `id`, `project_id`, `status` | 1 |
| `openstack_neutron_router` | `id`, `project_id`, `status` | 1 |
| `openstack_loadbalancer_loadbalancer_status` | `id`, `name`, `project_id`, `provisioning_status`, `operating_status` | 1 |
| `openstack_nova_limits_instances_used` and `_max` | `tenant_id` | the project's live instances, and 100 |
| `openstack_nova_limits_vcpus_used` and `_max` | `tenant_id` | their summed vcpus, and 400 |
| `openstack_nova_limits_memory_used` and `_max` | `tenant_id` | their summed memory in MB, and 819200 |
| `openstack_identity_projects` | none | the count of tenants |
| `openstack_identity_project_info` | `id`, `name`, `domain_id="default"`, `enabled="true"` | 1 |
| `openstack_nova_total_vms` | none | the live instances of the cloud |
| `openstack_cinder_volumes` | none | its live volumes |
| `openstack_neutron_floating_ips` | none | its live floating addresses |
| `openstack_glance_images` | none | its live images |
| `openstack_loadbalancer_total_loadbalancers` | none | its live load balancers |

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
[`otlp.go`](../internal/providers/openstack/simulator/otlp.go) rather than by
the OpenTelemetry SDK: the SDK stamps a point with the instant it collected it,
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

A request is retried under the policy the engine's counter client queries
VictoriaMetrics with: a connection error, a 429, which is what the Gateway's
`BackendTrafficPolicy` answers a run that pushes faster than the rate it allows,
and a 5xx except 501, four retries at most; a 4xx such as the 401 of a wrong
password comes back at once. A batch that still fails ends the push with
`pushing metrics to <url>: ...` and exit status 1, reported once the last
notification is on the bus: the month is what the operator asked for, and the
exit status is what says the metric half of it failed. SIGINT and SIGTERM stop
the push the way they stop the publishing, with exit status 0.

A `replay` pushes nothing and serves no `/metrics`. A `run` in file mode pushes
nothing either, and it writes the traffic rows of the month into its oracle all
the same. The compose stack pushes to the dev Gateway's
`https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics` under the credential the
dev overlay generates into the `tally-otlp-auth` Secret.

The points lie on the simulated period rather than on the wall clock of the run
that sent them. A dashboard therefore reads the month by setting its time range
to the period, not to the hour the run took.

### The endpoint

`GET /metrics` on the control listener serves the inventory in the Prometheus
exposition format while a run publishes, out of the same `InventoryAt` the push
reads and clamped to the end of the month the way a listing of the fake
OpenStack API is. The registry behind it is private and holds that one
collector: no Go metrics and no process metrics, because an endpoint that stands
in for an exporter of the cloud has no business reporting the heap and the file
descriptors of the process that runs the drill.

`TALLY_METRICS_ENABLED=false` registers no route, and the path is then left to
the fake OpenStack API, which answers it with the 404 it answers any unknown
path with.

The dev overlay's `openstack-db-exporter` scrape job reads that endpoint at
`host.docker.internal:8091` under `cloud: os-sim`, which is where a pod in the
kind node reaches the port the compose stack publishes on the host
([`scrape.yaml`](../deploy/kubernetes/overlays/dev/victoriametrics/scrape.yaml)).
The job is up while a run publishes and down between runs, where
`TallyScrapeTargetDown` fires for it after five minutes, the way it does against
the base's placeholder target. The `ceilometer` job keeps its placeholder: the
recorded Ceilometer default is the Pushgateway path, and the traffic of a
simulated month reaches the store over OTLP.

### The counter

[`counter-sources.yaml`](../deploy/kubernetes/overlays/dev/counter-sources.yaml)
of the dev overlay measures `egress_gb` of an OpenStack instance from the
counter a push writes:

```yaml
sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    required: false
    query: >
      sum(increase(ceilometer_network_outgoing_bytes_total{cloud="{cloud}",
          resource_id="{resource_id}"}[{window}])) / (1024 * 1024 * 1024)
```

`required: false` because a dev cluster where no month was ever simulated holds
none of that series, and a required source whose query fails takes the whole
tick down with it, every scheduled hour; a failed query of an optional source
yields a `counter_source_failed` warning on a run that finished. The divisor is
2^30, which is what `bytesPerGibibyte` converts a reported size with, so the
export's quantity and the oracle's bytes are in one unit. The overlay mounts the
generated ConfigMap into the `tally-engine` CronJob at
`/etc/tally/counter-sources.yaml` and points `TALLY_ENGINE_COUNTER_SOURCES` at
that path. A metricsql source is queried at the end of every usage draft, which
for a simulated month is an instant inside the period the points lie on, so
`rated.csv` of a run over that month carries a non-zero `egress_gb` quantity for
every instance that was active.

`compare` leaves the `counter` dimensions out all the same. `increase()`
extrapolates a counter over the edges of the window it is asked about, so the
quantity an export carries differs from the exact integral the generator placed
in the interval, and a comparison that held the two equal would report the
extrapolation as an engine difference. The oracle's `traffic` rows are what a
drill reads the intended figure off.

## Configuration

[`../cmd/tally-openstack-simulator/.env.example`](../cmd/tally-openstack-simulator/.env.example)
lists every variable with its default and its meaning.

| Variable | Default | Read by | Purpose |
| --- | --- | --- | --- |
| `TALLY_LOG_LEVEL` | `INFO` | both | slog threshold: `DEBUG`, `INFO`, `WARN`, or `ERROR`, matched exactly. |
| `TALLY_SIM_HTTP_ADDR` | `127.0.0.1` | both, while publishing | address the control endpoint binds. It carries no credential, so a bind on every interface is a deployment's own decision; the compose stack sets `0.0.0.0` because it publishes the port on the host's loopback itself. |
| `TALLY_SIM_HTTP_PORT` | `8080` | both, while publishing | port of the control endpoint. |
| `TALLY_SIM_AMQP_URL` | none | `run` optional, `replay` required | broker the notifications are published to. It carries the broker password, so it also accepts `TALLY_SIM_AMQP_URL_FILE`; setting both is an error. |
| `TALLY_SIM_CLOUD` | none | `run` | cloud the month belongs to: the salt of every generated identifier and the cloud of `events.jsonl`. |
| `TALLY_SIM_REPORTING_URL` | none | `run` with `--register-projects` | Reporting API the projects are registered with. It has to be absolute and carry a host, with no query and no fragment, because the registry route is appended to it, and it has to be `https` unless `TALLY_SIM_REPORTING_INSECURE` says otherwise. A run without the switch reads it and ignores it. |
| `TALLY_SIM_REPORTING_INSECURE` | `false` | `run` with `--register-projects` | allow a plaintext Reporting API. The api token travels in a header on every request and is of role `admin`, so it is not scoped to one cloud the way an ingest token is. |
| `TALLY_SIM_API_TOKEN` | none | `run` with `--register-projects` | credential of the registration, an api token of role `admin`, which `POST /api/v1/projects` and `POST /api/v1/projects/{id}/relations` demand. It also accepts `TALLY_SIM_API_TOKEN_FILE`; setting both is an error. |
| `TALLY_SIM_GARDEN_CLOUD` | none | `run` with `--register-projects` | cloud the two Gardener rows are registered under. It has to differ from `TALLY_SIM_CLOUD`: a cloud is one installation of one platform. |
| `TALLY_SIM_OTLP_URL` | none | `run` | OTLP/HTTP endpoint the traffic counters and the inventory gauges are pushed to. Empty is a run without a push. It has to be absolute and carry a host, to carry no userinfo, query or fragment, because the credential of a push belongs in `TALLY_SIM_OTLP_USER` and `TALLY_SIM_OTLP_PASSWORD` and a URL that carries one hands it to every error the push writes, and to be `https` unless `TALLY_SIM_OTLP_INSECURE` says otherwise, because the Basic password travels on it. |
| `TALLY_SIM_OTLP_USER` | none | `run` with `TALLY_SIM_OTLP_URL` | Basic user of the push. The endpoint in front of the collector answers an unauthenticated push with a 401. |
| `TALLY_SIM_OTLP_PASSWORD` | none | `run` with `TALLY_SIM_OTLP_URL` | Basic password of the push. It also accepts `TALLY_SIM_OTLP_PASSWORD_FILE`; setting both is an error. |
| `TALLY_SIM_OTLP_INSECURE` | `false` | `run` | allow a plaintext OTLP endpoint. The password travels on every request, so an `http` endpoint hands it to whoever reads the wire. |
| `TALLY_METRICS_ENABLED` | `true` | `run`, while publishing | serve the inventory on `GET /metrics` of the control listener, which "The metric series" describes. `false` registers no route, and the fake OpenStack API answers that path with its own 404. |

An empty `TALLY_SIM_AMQP_URL` puts `run` in file mode, where `--out` is what it
writes to instead, and `replay` refuses to start without one.

The broker reaches off this machine, and what a run puts on it is not a test
message. The exchanges, the declare arguments, and the shape
of every notification are the ones a real deployment carries, the wire format
names no cloud, and a collector books what it consumes under its own
`TALLY_OSC_CLOUD` whatever `TALLY_SIM_CLOUD` said. A month published onto a
production broker is therefore a month of invented usage in that deployment's
billing data, and it stays there: ingestion deduplicates a second run into
nothing, and no subcommand of `tally-reporting-admin` deletes an event. Both
subcommands refuse a broker that is not on the machine they run on for that
reason, and `--allow-remote-broker` is the confirmation that lets one through.
The compose stack passes it, because its broker is the container next door.

`run` takes `--period` (required, `YYYY-MM`), `--seed` (1), `--factor` (744),
`--out` (empty), `--wait-for-collector` (two minutes), `--metrics-interval`
(`300s`), `--faults` (empty), `--register-projects` (off), and
`--allow-remote-broker` (off). `--faults` takes
the six switch names `pre-existing`, `missing-create`, `duplicates`,
`reordering`, `refused-shapes`, and `held-back`, comma-separated; empty is every
switch off, and "The fault switches" describes what each of them does.
`--register-projects` registers the month's tenants, the two Gardener projects,
and their `infrastructure_tenant` relations before anything is written or
published, which "The project registry" describes. `--metrics-interval` is the
grid the traffic and the inventory samples lie on, which "The metric series"
describes. `replay` takes `--in`
(required),
`--factor` (744), `--wait-for-collector` (two minutes), and
`--allow-remote-broker` (off). `compare` takes `--oracle`, `--export`, and
`--pricing`, all three required, and reads no variable of the table at all: it
holds three files against each other and touches neither a broker nor the
Reporting API.

`TALLY_METRICS_ENABLED` switches the inventory endpoint of "The metric series"
above.

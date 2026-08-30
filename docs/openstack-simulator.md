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

It has no fault switches, no oracle to hold a run against, no fake OpenStack
API, and no metrics endpoint of its own. #65 is the meta issue the simulated
workloads belong to, and this document describes its first stage. The noise,
the notifications the collector does not bill, is rendered and described under
"The noise" below; #87 owns the oracle, #88 the fault switches, and #89 the
project registry; #66 the fake API and the clock seam; #67 the traffic series
and `/metrics`. The drill #51 cites this simulator as the way a month reaches
the Reporting API. The workload renders octavia's three load balancer types
from the shoots' load balancers.

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
factor of 744 puts a 31-day month on the bus in an hour.

The target builds the collector and the simulator image, writes the dev CA to
`tally-ca.crt`, issues an ingest credential for `SIM_CLOUD` with
`tally-reporting-admin create-ingest-credential`, writes the cloud, the period,
the seed, the factor, and that token into `deploy/compose/.env`, and starts the
three containers of
[`../deploy/compose/compose.yaml`](../deploy/compose/compose.yaml): the broker,
the collector image, and the simulator.

It does not rebuild what runs in the cluster and applies no migration: loading
the Reporting API image into kind and applying the migration chain are both
`make up`. After a change to the Reporting API or to the migrations, run
`make up` again before this target; otherwise the stack posts into the image and
the schema the last `make up` left there.

The period has to lie in the past. `run` refuses a month that has not ended,
because the engine warns about a period that has not ended, and a simulated
month reaching into the future would carry that warning into every run over it.

Four URLs come out of the stack:

- `http://127.0.0.1:15672`, the broker's management UI, guest/guest
- `http://127.0.0.1:8090/metrics`, the collector
- `http://127.0.0.1:8091/clock`, the simulator's control endpoint
- `https://api.tally.127-0-0-1.nip.io:8443/api/v1`, the Reporting API

Every host port is bound to `127.0.0.1` and lies above 1024;
`deploy/kind/kind.yaml` states why. The collector's container reaches the
cluster through `extra_hosts`, which maps `api.tally.127-0-0-1.nip.io` to
`host-gateway`, and `tally-ca.crt` is mounted read-only at the path
`SSL_CERT_FILE` names, which is what makes the Gateway's certificate verify.

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

## What the collector shows

Seed 1 over `2026-07` renders 15727 notifications, 1812 of them billable. Nine
of the other 13915 are the unsized `image.create`, one per image: two per
classic project, one per Gardener tenant, and one for the CI tenant. The
remaining 13906 are the noise catalogue. The month carries 83 distinct
`event_type` values. The shape of a month is the seed's alone, so the counts
below hold on every cloud.

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

`run --out DIR` writes the whole month before it publishes anything, so a run
interrupted halfway still leaves a complete month on disk. With no broker
configured it writes the files and publishes nothing:

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

`replay --in FILE --factor N` publishes a captured file onto a broker, with the
virtual clock started at the first line's timestamp, so a month needs neither
the generator nor the seed that produced it. The message ids are the recorded
ones, so a second replay of the same file is deduplicated at ingest. A line
whose timestamp lies before the previous one is published at once, which is
what lets a file that is not perfectly sorted replay whole.

`ReadStream` reads the file before the first message goes out and refuses an
empty file, a line that is not JSON, and a body without a timestamp. A replay
that would fail halfway through a month fails with nothing published.

## The control endpoint

Both subcommands serve a control endpoint on `TALLY_SIM_HTTP_ADDR` and
`TALLY_SIM_HTTP_PORT` while they publish and no longer: pacing is the only thing
it changes, and there is nothing to pace before the first notification or after
the last one. It carries no credential, so what keeps it out of reach is the
address it binds: loopback, unless a deployment names another one. In the
compose stack it binds `0.0.0.0` inside the container, where loopback would be
reachable from nothing, and the container's port 8080 is published on the host's
`127.0.0.1:8091`. Within that reach the worst a caller does is make the run go
at another speed.

`GET /healthz` answers `ok`. `GET /clock` answers the document of that moment:

```json
{"virtual_now":"2026-07-09T14:22:00Z","factor":744,"published":52,"total":15727,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
```

`PUT /clock` with a body of `{"factor": N}`, where N is zero or positive,
rebases the clock on the virtual instant it has reached and answers the same
document. The month itself does not move: only what comes after the change runs
at another speed. A body that is not JSON, one without the member, and one with
a negative factor all answer 400 with
`factor must be a JSON object with a number member "factor" that is zero or
positive`. Any other method on either route answers 405. A run in file mode
serves nothing.

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

An empty `TALLY_SIM_AMQP_URL` puts `run` in file mode, where `--out` is what it
writes to instead, and `replay` refuses to start without one.

The broker is the one setting that reaches off this machine, and what a run puts
on it is not a test message. The exchanges, the declare arguments, and the shape
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
`--out` (empty), `--wait-for-collector` (two minutes), and
`--allow-remote-broker` (off). `replay` takes `--in` (required), `--factor`
(744), `--wait-for-collector` (two minutes), and `--allow-remote-broker` (off).

`TALLY_METRICS_ENABLED` is not read: the simulator exports no metrics, and #67
owns the endpoint that would serve them.

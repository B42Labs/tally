---
title: OpenStack simulator (tally-openstack-simulator)
description: Every subcommand and flag of the simulator, its control endpoint, its fake OpenStack API, its inventory endpoint, its fault switches and the files a run writes.
quadrant: reference
audience: operator
---

# OpenStack simulator (tally-openstack-simulator)

`tally-openstack-simulator` publishes one simulated month of OpenStack
notifications.

`run` generates the month from `--seed` and `--period` and publishes it onto the
broker `TALLY_SIM_AMQP_URL` names, at `--factor` virtual seconds per wall
second. With `--out` it writes the month to `notifications.jsonl`,
`events.jsonl` and `oracle.json` instead, and to `held-back.jsonl` beside them
when a switch holds notifications back; with both it does both. With `--faults`
it turns fault switches on, each of which changes what the bus carries and never
the month the oracle states. With `--register-projects` it registers the month's
tenants, its two Gardener projects and their `infrastructure_tenant` relations
with the Reporting API `TALLY_SIM_REPORTING_URL` names before it writes or
publishes anything.

`replay` publishes a `notifications.jsonl` an earlier run wrote, which puts the
same month on a bus again without the generator behind it. `compare` reads the
oracle a run wrote, an engine export of the month and the pricing model the run
rated with, and lists every resource whose metered intervals or quantities
differ from the oracle.

The tree is built in
[`cmd/tally-openstack-simulator/main.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-openstack-simulator/main.go)
and its subcommands in
[`cmd/tally-openstack-simulator/commands.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-openstack-simulator/commands.go).

## Commands

<!-- refdoc:begin commands -->
### `tally-openstack-simulator`

Publish a simulated month of OpenStack notifications

```text
tally-openstack-simulator
```

| Subcommand | Purpose |
| --- | --- |
| `compare` | Compare an engine export of the month against the oracle a run wrote |
| `replay` | Publish a recorded notifications.jsonl |
| `run` | Generate a month of notifications and publish or write it |

This command takes no flags.

### `tally-openstack-simulator compare`

Compare an engine export of the month against the oracle a run wrote

```text
tally-openstack-simulator compare [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--export` | string | none | yes | directory tally-engine export --format csv --out wrote; rated.csv is read from it |
| `--oracle` | string | none | yes | path of an oracle.json written by run --out |
| `--pricing` | string | none | yes | pricing model YAML the run rated with |

### `tally-openstack-simulator replay`

Publish a recorded notifications.jsonl

```text
tally-openstack-simulator replay [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--allow-remote-broker` | boolean | `false` | no | publish to a broker that is not on this machine, knowing that a collector books what a run publishes as real usage |
| `--factor` | number | `744` | no | virtual seconds per wall second; 0 publishes as fast as the broker confirms |
| `--in` | string | none | yes | path of a notifications.jsonl written by run --out |
| `--wait-for-collector` | duration | `2m0s` | no | how long to wait for a consumer on the collector's queue before publishing; 0 disables the wait |

### `tally-openstack-simulator run`

Generate a month of notifications and publish or write it

```text
tally-openstack-simulator run [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--allow-remote-broker` | boolean | `false` | no | publish to a broker that is not on this machine, knowing that a collector books what a run publishes as real usage |
| `--factor` | number | `744` | no | virtual seconds per wall second; 0 publishes as fast as the broker confirms |
| `--faults` | list, comma-separated | none | no | fault switches to turn on, comma-separated: pre-existing, missing-create, duplicates, reordering, refused-shapes, held-back; every switch is off by default |
| `--metrics-interval` | duration | `5m0s` | no | grid the traffic and inventory samples lie on, counted from the period start; whole seconds, at least 30s and at most 24h; 300s is the interval Ceilometer polls at |
| `--out` | string | none | no | directory to write notifications.jsonl, events.jsonl and oracle.json to, and held-back.jsonl when a switch holds notifications back |
| `--period` | string | none | yes | billing month to simulate, YYYY-MM; it must have ended |
| `--register-projects` | boolean | `false` | no | register every tenant of the month, the Gardener projects, and their infrastructure_tenant relations with the Reporting API TALLY_SIM_REPORTING_URL names; off by default |
| `--seed` | integer | `1` | no | seed of the month's shape |
| `--wait-for-collector` | duration | `2m0s` | no | how long to wait for a consumer on the collector's queue before publishing; 0 disables the wait |
<!-- refdoc:end commands -->

## Modes and refusals

A `run` without `TALLY_SIM_AMQP_URL` is file mode: nothing is dialled and the
month is written out, which is what makes a month on a machine with no broker
possible at all. File mode needs `--out`, and a run with neither the variable
nor the flag is refused with `set TALLY_SIM_AMQP_URL or pass --out: the run has
nowhere to publish`. A run with both publishes and writes. `run` asks for
`TALLY_SIM_CLOUD` either way; `replay` asks for `TALLY_SIM_AMQP_URL` alone,
because it has no file mode and the cloud travels in the recorded
notifications.

A broker that is not on this machine is refused, and `--allow-remote-broker` is
the confirmation that lets one through: what a run puts on a broker is booked as
real usage by whatever collector consumes it, and nothing deletes it again. Both
`run` and `replay` take the flag.

`--faults` takes the six switch names, comma-separated. A name outside the six
is refused, and the refusal names them. `pre-existing` and `missing-create` exclude each
other, because both pick the instances they work on from the same set. A name
given twice is the run a name given once is.

`--period` has to name a month that has ended, `--factor` has to be zero or
positive, `--wait-for-collector` has to be zero or positive, and
`--metrics-interval` has to be a whole number of seconds between 30s and 24h.
Every one of them is checked before a broker is dialled.

## Exit status and signals

SIGINT and SIGTERM stop a run cleanly: what went out stays out, and the process
ends with exit status 0. A run stopped while it holds the held-back share exits
0 with that share never published.

`compare` exits 1 when any resource differs from the oracle, with the count on
stderr under the report. Every other failure exits 1 as well.

## The control endpoint

The routes and the messages below are the ones
[`internal/providers/openstack/simulator/control.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/simulator/control.go)
writes.

`run` and `replay` serve a control endpoint on `TALLY_SIM_HTTP_ADDR` and
`TALLY_SIM_HTTP_PORT` while they publish and no longer. What it decides is the
pace of the month and, for a run with the `held-back` switch, when the held
share goes out. It carries no credential, so what keeps it out of reach is the
address it binds: loopback, unless a deployment names another one. A run in file
mode serves nothing.

`GET /healthz` answers 200 with `ok`.

`GET /clock` answers 200 with the clock document of that moment.

`PUT /clock` takes a JSON object with a `factor` member that is zero or
positive, rebases the clock on the virtual instant it has reached, and answers
the same document. A body that is not JSON, one without the member and one with
a negative factor all answer 400 with `factor must be a JSON object with a
number member "factor" that is zero or positive`. A request a page in a browser
sent answers 403 with `the factor does not take a request a browser sent`.

`POST /release` publishes the share a run with the `held-back` switch kept back.
It answers 200 with the document as it stood the moment before the release, with
`held` 0 and `holding` false, the two members the release changed. The three
refusals answer 409 and name the run they arrived at: `nothing is held back: the
run was started without the held-back switch`, `the month is still publishing;
release once /clock reports holding true`, and `the held-back notifications were
already released`. A request a page in a browser sent answers 403 with `release
does not take a request a browser sent`, and a body of another media type than
`application/json` answers 415 with `release takes application/json or no body`.

Both routes that change something read `Origin` and `Sec-Fetch-Site`, which a
browser puts on every request a page makes whose method is neither `GET` nor
`HEAD`. `curl` and a script send neither and are unaffected.

`holding` is the one member a release may be sent on. It turns true when the
last regular notification is on the bus and false again when a release lets the
held share out. `published` equal to `total` minus `held` is the same month a
moment earlier, so a release sent on the counts alone can still be refused.

During a `replay`, where the control routes are all that is mounted, a method a
route is not registered with answers 405. During a `run` the fake OpenStack API
stands under every path the control routes leave, so such a request reaches that
API and is answered 404.

### The clock document

<!-- refdoc:begin clock-document -->
#### `clockDocument`

clockDocument is what /clock answers. The instants are RFC 3339 in UTC because the reader is a person or a script watching a run, and a zoneless timestamp would leave both guessing which zone the simulator ran in.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `virtual_now` | string | always |  |
| `factor` | number | always |  |
| `published` | integer | always |  |
| `total` | integer | always |  |
| `held` | integer | always |  |
| `holding` | boolean | always |  |
| `period_from` | string | always |  |
| `period_to` | string | always |  |
<!-- refdoc:end clock-document -->

## The fake OpenStack API

A `run` serves the month it publishes as an OpenStack API beside the bus, out of
the same oracle a comparison holds an export to. It is what a reconciliation
sync reads to learn what the cloud holds right now. The routes and the fixed
values below are declared in
[`internal/providers/openstack/simulator/api.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/simulator/api.go).

`POST /v3/auth/tokens` is the keystone v3 password request. The credentials are
the fixed pair `tally-sync` and `tally-dev-sync-password`: whatever
authenticates here is configured from a clouds.yaml this repository carries, and
a drawn password would be one nobody can write into that file. Every run issues
one token of its own, which every other route demands in `X-Auth-Token`, so a
client holding the token of a run that has ended is told so rather than served
the month that came after it. The catalog the token carries is built from the
`Host` header of the request that asked for it, so a pod and a test each read a
catalog pointing back at the address they used.

Four routes answer a version document, one per service that publishes its
endpoint without a version in its path: `GET /compute/v2.1/`, `GET /image/`,
`GET /network/` and `GET /load-balancer/`. Nova's reaches past microversion
2.47, so a client negotiates the microversion that embeds a server's flavor in
the server rather than falling back to the flavor catalog.

Six routes answer a listing: `GET /compute/v2.1/servers/detail`,
`GET /compute/v2.1/flavors/detail`, `GET /volume/v3/volumes/detail`,
`GET /network/v2.0/floatingips`, `GET /image/v2/images` and
`GET /load-balancer/v2.0/lbaas/loadbalancers`.

Nova's server path serves two listings. A request carrying `deleted=true`
together with a `changes-since` instant is answered with the instances the month
destroyed inside that window, which is the listing that dates a missed delete at
the instant the platform performed it; a `changes-since` that is not an RFC 3339
instant answers 400 with `changes-since must be an RFC 3339 instant`. The
reconciliation adapter asks no further back than 24 hours, a bound stated in
[`internal/reporting/reconciliation/adapters/openstack.go`](https://github.com/B42Labs/tally/blob/main/internal/reporting/reconciliation/adapters/openstack.go).
Everything else is the live listing, the admin-scope probe a sync sends before
it observes anything included.

Every document is answered at the instant the run's virtual clock stands at,
clamped to the end of the month, so a clock that ran past the month still serves
the resources that outlived it.

The API is up for exactly as long as the control endpoint and on the same
listener: while a `run` publishes or holds. A run in file mode serves neither of
the two, and a `replay` serves the control routes alone, because a recorded file
holds notifications and no oracle to answer a listing out of.
`POST /v3/auth/tokens` on a replay is a path no route claims and is answered
404.

## The inventory endpoint

`GET /metrics` on the same listener serves the inventory of the month in the
Prometheus exposition format, while `TALLY_METRICS_ENABLED` is true. A run that
sets it to false registers no route there, and the fake API answers the path
with its own 404. The endpoint stands in for the OpenStack database exporter a
real cloud runs beside its services, so a dashboard written against a real cloud
fills against a simulated one.

The names and the label spellings below are declared in
[`internal/providers/openstack/simulator/metrics.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/simulator/metrics.go),
and the five aggregate series are pinned by
[`internal/providers/openstack/simulator/exporter_test.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/simulator/exporter_test.go)
against the alert that reads them off a database exporter. A scrape carries one
gauge per live resource, the limits of every project, and the counts the cloud
reports about itself.

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

The `id`, the `uuid` and the `name` of a resource are all its generated id,
because the oracle these gauges are folded out of holds no display name; the
fake OpenStack API answers a listing the same way.

## The fault switches

The six switches and the shares they draw are declared in
[`internal/providers/openstack/simulator/faults.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/simulator/faults.go).
A switch changes what the bus carries and never what the simulated cloud did, so
the oracle of a month states the same usage whichever switches are on. Every
switch is off by default.

### `pre-existing`

One in three of the classic tenants' instances starts before the month, and its
volumes and its floating address start with it. Every transition of such an
instance is published, the ones before the period start included, so they go out
in a burst ahead of the month's own first notification. No notification is added or
dropped. The projection holds the whole history and bills it from the month
start, and the oracle clips the resource's intervals to that same instant. What
the switch shows is a resource whose life began before the period, billed over
the part of it the period holds.

### `missing-create`

The switch picks the same instances `pre-existing` picks, with the same leads,
and drops every transition before the period start from the schedule and from
the stream. The bus carries a resource whose create it never saw, and the
collector first hears of one through a notification from inside the month. The
daily `compute.instance.exists` audits stay, because the audit pass runs over
the finished schedule before the drop. The engine warns
`history_starts_without_create` once per touched resource, and a comparison
reports every touched resource as a difference, marked with the switch that
caused it. No correction closes that gap: the notifications were never
delivered, so a later run over the same period reads the same history.

### `duplicates`

One in 20 of the billable transitions is published a second time, byte for byte
and under the message id of the original, ten notifications later or behind the
last one when the month ends before that distance is walked. The copy travels
the route its original travels and the collector maps both, so the consumed
counter counts the repeat. The Reporting API stores one event per (`event_id`,
`timestamp`) and counts the second as deduplicated. The export and the
comparison are those of the month with the switch off, and the engine writes no
warning.

### `reordering`

One in 10 of the resources with at least two billable transitions has its first
one published directly behind its second. The timestamps do not move: what
changes is the order the collector consumes them in, the order a requeued
delivery or a second consumer produces. The projection and the engine sort a
resource's history by timestamp before they fold it, so the export is the export
of the month with the switch off, the comparison reports no difference, and no
counter of the collector moves.

### `refused-shapes`

Per billable transition the switch draws once in 400 for a twin the collector
refuses: one draw puts an oversized twin behind it, two a truncated one, and
twenty a versioned one, which is drawn for nova alone because the versioned
format is nova's. A twin follows its original directly, on the same exchange and
under a fresh message id. The versioned twin carries an
`instance.*` type name and its payload under `nova_object.data`, which parses
and maps to nothing, so it counts as skipped under its versioned type name. The
truncated twin is an envelope whose inner `oslo.message` is cut in half, and the
oversized twin carries a padding member that puts its body past 1 MiB; both
count as unparseable. `events.jsonl`, the oracle and the comparison are those of
the month with the switch off, because a twin is billable to nobody. A
`notifications.jsonl` of such a month cannot be replayed: the oversized twin is
past the line bound `replay` reads with, and the truncated one fails its
envelope parse.

### `held-back`

One in 20 of the billable transitions is kept off the bus and written to
`held-back.jsonl` instead. The run publishes the rest of the month and then
holds: `GET /clock` reports the hold under `holding` and its size under `held`,
and the notifications go out when `POST /release` arrives. What the switch
renders is a late arrival, the events that reach the Reporting API after the run
that bills the period read it. A run stopped while it holds exits 0 with the
held share never published.

## Files

`run --out <directory>` writes three files, and a fourth when `held-back` keeps
part of the month off the bus. The whole month is written before anything is
published, so a run interrupted halfway still leaves a complete month on disk.

`notifications.jsonl` holds one line per notification, with the `exchange` and
the `routing_key` it goes out under and the `body` it carries. That body is the
oslo envelope: an `oslo.version` of `2.0` and the notification itself as a JSON
string under `oslo.message`, which is the double encoding a real deployment puts
on the bus.

`events.jsonl` holds one canonical event per billable notification, computed by
the collector's own envelope parser and mapping table rather than by the
simulator's idea of them. It states what ingestion has to hold once the month is
consumed.

`oracle.json` is the generator's statement of what the month was meant to meter
to. `held-back.jsonl` is the fourth, in the form of `notifications.jsonl` and
holding what the `held-back` switch keeps back.

Every run removes all four files an earlier one left in the directory before it
writes the first of its own, so what a directory holds is one month and nothing
of another. A path one of the removals cannot take away ends the run there, and
every path that stayed is named.

### The stream line

<!-- refdoc:begin stream-line -->
#### `Line`

Line is one line of notifications.jsonl: the message body a service put on the bus, together with the addressing it was published under. The addressing travels with the body because the body does not carry it: an exchange is what decides which queue a notification reaches, and a replay that guessed it would publish a month no collector receives.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `exchange` | string | always | Exchange is the service exchange the notification belongs on, one of nova, cinder, neutron, glance, octavia, keystone, designate, and barbican. |
| `routing_key` | string | always | RoutingKey is the topic the notification was published under. |
| `body` | object | always | Body is the oslo envelope as Render produced it, kept as raw JSON so a replay republishes the very bytes the run generated rather than a re-encoding of them. |
<!-- refdoc:end stream-line -->

### The oracle

<!-- refdoc:begin oracle -->
#### `Oracle`

Oracle is the generator's statement of what a month contained: for every billable resource the intervals of constant state, size and project it intended, clipped to the month, and the count of events it expects the collector to record per project and Tally event type.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `format` | integer | always |  |
| `cloud` | string | always |  |
| `seed` | integer | always |  |
| `period_from` | string, RFC 3339 UTC | always |  |
| `period_to` | string, RFC 3339 UTC | always |  |
| `resources` | array of [OracleResource](#oracleresource) | always |  |
| `counts` | array of [OracleCount](#oraclecount) | always |  |
| `faults` | list, comma-separated | always | Faults holds the fault switches the month ran with, in FaultNames order. A month nobody passed --faults to states an empty list. |
| `traffic` | array of [OracleTraffic](#oracletraffic) | always | Traffic holds one row per instance and per interval of that instance, ordered by resource id and then by From. Run fills it from the metric samples once the month is generated, so buildOracle states an empty list. The rows lie outside OracleInterval on purpose: foldFacts merges two adjacent facts that repeat the state, project and size, and a traffic figure inside the interval would make two mergeable intervals compare unequal and split that fold. |

#### `OracleResource`

OracleResource is one billable resource of the month and the intervals it was meant to be billed over, ordered by their start.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `resource_type` | string | always |  |
| `resource_id` | string | always |  |
| `workload` | string | always |  |
| `intervals` | array of [OracleInterval](#oracleinterval) | always |  |
| `faults` | list, comma-separated | always | Faults holds the fault switches that touched this resource, in FaultNames order. A resource none of them touched states an empty list. |

#### `OracleInterval`

OracleInterval is a half-open span [From, To) over which a resource's state, size and project did not change. Both ends lie inside the month.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `from` | string, RFC 3339 UTC | always |  |
| `to` | string, RFC 3339 UTC | always |  |
| `state` | string | always |  |
| `project_id` | string | always |  |
| `size` | object | always |  |

#### `OracleCount`

OracleCount is how many events of one Tally event type the month expects a project to have recorded.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `project_id` | string | always |  |
| `event_type` | string | always |  |
| `count` | integer | always |  |

#### `OracleTraffic`

OracleTraffic is the network traffic one instance was given over one of its intervals: the exact sum of the grid steps the generator placed inside it.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `resource_id` | string | always |  |
| `from` | string, RFC 3339 UTC | always |  |
| `to` | string, RFC 3339 UTC | always |  |
| `egress_bytes` | integer | always |  |
| `ingress_bytes` | integer | always |  |
<!-- refdoc:end oracle -->

The resources are sorted by type and then by id, the intervals by their start,
and the counts by project and then by event type. Two runs of one seed, period
and cloud therefore write a file a diff reports nothing about.

The `format` member of the document is this constant.

<!-- refdoc:begin oracle-format -->
| Name | Value | Meaning |
| --- | --- | --- |
| `oracleFormat` | `3` | oracleFormat is the format of the document this build writes and reads. It is what tells an oracle of this build from one another build wrote: the two things a comparison holds an oracle and an export together by, the cloud and the period, both pass for an oracle of the same month folded by a generator that has since gained a billable transition or a size member, and every resource that changed would then be reported as a difference the engine did not cause. Whoever changes what the generator books, what a size holds, or what this document states raises the number, and an oracle written before that change is refused rather than compared. What holds them to it is TestOracleFormatCoversTheGeneratorsBookedSurface, which fails on a booked transition, a size member, a member of this document or a state the number was not raised for. The guard runs both ways: ReadOracle refuses a document of another format, and DisallowUnknownFields refuses one that states a member this build does not read. Format 2 added the faults member on the document and on every resource. Format 3 added the traffic member on the document. |
<!-- refdoc:end oracle-format -->

`replay --in <file>` takes a stream file, `notifications.jsonl` or
`held-back.jsonl`. The virtual clock starts at the first line's timestamp, so a
month needs neither the generator nor the seed that produced it, and the message
ids are the recorded ones, so a second replay of the same file is deduplicated
at ingest. A line whose timestamp lies before the previous one is published at
once, which is what lets a file that is not perfectly sorted replay whole.

The file is read whole by `ReadStream` in
[`internal/providers/openstack/simulator/stream.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/simulator/stream.go)
before the first message goes out. An empty file, a line that is not JSON, a
line longer than 1 MiB and a body without a usable timestamp are each refused
there, so a replay that would fail halfway through a month fails with nothing
published.

## Project registration

`run --register-projects` registers the month with the project registry of the
Reporting API, before the first file is written and before the first
notification goes out. It runs in file mode too, which is how an operator
prepares the registry before a `replay` puts the recorded month on a bus.
`replay` itself registers nothing.

Four variables carry what a registration needs, and a run without the switch
reads them and ignores them. `TALLY_SIM_REPORTING_URL` is the Reporting API the
rows are posted to; it has to be absolute, carry a host, and carry no query and
no fragment, because the registry route is appended to it. Its scheme has to be
`https`, because the api token travels in a header on every request; a plaintext
URL is asked for with `TALLY_SIM_REPORTING_INSECURE=true`. `TALLY_SIM_API_TOKEN`
is that token, of role `admin`, because the two registry routes demand that
role; it is a secret, so `TALLY_SIM_API_TOKEN_FILE` carries it as well, and
setting both is an error. `TALLY_SIM_GARDEN_CLOUD` is the cloud the two Gardener
rows are registered under, and it has to differ from `TALLY_SIM_CLOUD`: a cloud
is one installation of one platform, so a Gardener project keyed under the
tenants' cloud would be a row of the OpenStack installation, and its relation
would then point it at itself.

A month registers one `openstack` row per tenant under `TALLY_SIM_CLOUD`, keyed
by the keystone project id of the tenant and carrying its name: nothing on the
bus carries a tenant name, so the registry is the one place the ids of a month
are given one. It registers two `gardener` rows under `TALLY_SIM_GARDEN_CLOUD`,
with the external ids `alpha` and `beta`. Each Gardener row then takes one
relation of type `infrastructure_tenant` to the tenant its shoots run on, valid
from the first instant of the month and with no end. Every row and every
relation carries `created_by: tally-openstack-simulator` in its metadata, and a
relation's metadata names the shoots beside it.

A rerun of the same seed, period, cloud and garden cloud posts the same rows
again. A project the registry already holds is looked up by its
`(cloud, external_id)` key and neither its name nor its metadata is patched, and
a relation that is already active stays as it stands.

A rerun under another period, seed or cloud is refused at the first relation.
The Gardener rows are the ones the earlier run registered, while the tenants are
keyed by identifiers the month is salted into, so the second run would relate
one Gardener project to a second tenant, and two relations attributing at once
put two months of a tenant's cost into one statement. Every relation is
therefore preceded by two reads of the outgoing `infrastructure_tenant`
relations, one at the first instant of the month and one as they stand now, and
a relation to another tenant in either answer ends the run. A relation that
started in an earlier month can be ended where this month begins, and the
message names the route that does it and the instant. One that starts at or
after the first instant of this month cannot be ended before it starts, so the
message leaves registering under another `TALLY_SIM_GARDEN_CLOUD` as the way on.

A registration that fails ends the run with exit status 1, with no file written
and no notification published. The rows it got through stay in the registry, and
the rerun finds them. SIGINT while the registration runs is the clean stop the
rest of a run answers a signal with, exit status 0.

## See also

The [simulator settings](/reference/configuration/tally-openstack-simulator)
page lists every variable with its default.

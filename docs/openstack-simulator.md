# OpenStack notification simulator

The simulator is one binary that puts a month of oslo.messaging notifications
on a RabbitMQ broker the way nova, cinder, neutron, and glance put them there.
The collector described in [`openstack-collector.md`](openstack-collector.md)
consumes them unmodified, off its ordinary `tally-notifications` queue, and
posts the mapped events to the Reporting API, so a month of usage reaches Tally
with no OpenStack deployment behind it. The month is rendered from a small
simulated cloud and paced by a virtual clock, so a 31-day month goes out in the
wall time the factor compresses it into.

It has no fault switches, no oracle to hold a run against, no fake OpenStack
API, and no metrics endpoint of its own. #65 owns the workloads, the noise, the
faults, and the oracle; #66 the fake API and the clock seam; #67 the traffic
series and `/metrics`. The drill #51 cites this simulator as the way a month
reaches the Reporting API. Octavia notifications wait for #64.

## The world and the workload

The simulated cloud has three projects and one external network. It is small on
purpose: what a run has to cover is every notification type and every shape of
resource life, not a realistic tenant count.

The flavor catalog holds four entries: `m1.small` with 1 vCPU, 2048 MB of
memory, and a 20 GB root disk; `m1.medium` with 2, 4096, and 40; `m1.large`
with 4, 8192, and 40 GB root plus 40 GB ephemeral; `m1.xlarge` with 8, 16384,
and 160. `m1.large` is the only one with ephemeral disk, and the first instance
of every project runs on it, so every month exercises the mapping's sum of root
and ephemeral disk.

A volume carries one of the types `ssd`, `hdd`, and `standard`. The first two
appear under `type_modifiers` in `pricing/2026-03.yaml` and `standard` does
not, so a month prices both paths. Image sizes are drawn at quarter-gibibyte
steps from 1 GiB to 4 GiB, which keeps the mapping's division into gibibytes on
exact decimals.

Instances run on `compute-01` to `compute-04` and keep the host they were
created on. Cinder publishes as `storage-01@ceph`, neutron as `neutron-01`, and
glance as `glance-01`. Floating addresses come from `203.0.113.0/24`, the
documentation range of RFC 5737, drawn as a permutation so that no address is
handed out twice inside a month.

Every project gets:

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

The workload renders 18 oslo notification types.

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

`image.create` is rendered in the unsized form glance emits before an upload,
and the mapping skips it on purpose: the `image.upload` that follows is the
first notification with a size to bill.

The forced steps (the resize on the first instance, the shelve on the second,
the resize and retype of the second instance's first volume) are what make
every seed render every type rather than most of them. The test suite holds
seeds 1 to 5 over July 2026 against the recorded samples under
[`internal/providers/openstack/testdata/golden/notifications/`](../internal/providers/openstack/testdata/golden/notifications),
so a sample the catalog does not render fails the suite. The octavia
notifications of #64 are such a sample.

## Determinism

The shape of a month (what happens, when, and with which sizes) is a function
of `--seed` alone. The identifiers (the project and user ids, the resource ids,
the floating addresses, the message ids) are a function of the seed together
with the period and the cloud.

The same seed, period, and cloud therefore publish byte-identical
notifications, and a rerun costs nothing at the far end: the collector books
the oslo message id as the event's `event_id`, and the Reporting API stores an
event once per (`event_id`, `timestamp`), which
[`openstack-collector.md`](openstack-collector.md) describes. Another cloud or
another month publishes fresh resources under fresh message ids, so two
collectors fed by two simulated clouds never collide on `event_id` at one
Reporting API.

Between two months of the same length the notifications sit at the same offsets
from the month start. The transitions anchored on the month's end (the second
image's delete and the first instance's) move with its length.

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

Seed 1 over `2026-07` renders 180 notifications, 174 of them billable. The
other six are the unsized `image.create`, one per image of the three projects.

`curl http://127.0.0.1:8090/readyz` answers 200 once the collector holds its
broker connection and its outbox. On `http://127.0.0.1:8090/metrics`:

- `tally_collector_consumed_total` grows per event type while the month goes
  out.
- `tally_collector_skipped_total{event_type="image.create"}` ends at 6 for that
  seed and period.
- `tally_collector_unparseable_total` stays 0. Anything else is a rendered body
  the collector could not read.
- `tally_collector_delivered_total` rises above 0 within two minutes at factor
  744. A counter that stays at 0 means the events sit in the outbox and the
  Reporting API is not taking them.

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
idea of them. For seed 1 the first file therefore has six more lines than the
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
{"virtual_now":"2026-07-09T14:22:00Z","factor":744,"published":52,"total":180,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
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

# OpenStack event collector

The collector reads oslo.messaging notifications straight off the broker of an
OpenStack deployment over AMQP, with nothing in between. It maps every
notification it knows to a canonical Tally event, writes that event to a local
SQLite outbox, and a second loop posts the buffered events to the Reporting
API's `POST /api/v1/events`.

Delivery is at-least-once. A delivery is acknowledged on the bus only after the
mapped event is committed to the outbox, and a batch is deleted from the outbox
only after the API answered 200. Both retries are safe: the event carries the
oslo `message_id` as its `event_id`, and the API stores an event once per
(`event_id`, `timestamp`). A redelivered notification, a resent batch, and an
outbox replayed after a restart all arrive as the same event and are counted as
duplicates.

The collector runs next to the OpenStack control plane, close to its broker.
`make images` builds its image as `tally-openstack-collector:dev`; `make up`
deploys only the Tally services into the dev cluster and does not load it.
`make simulator-up` runs it on the developer's machine beside a broker and a
simulated month of notifications;
[`openstack-simulator.md`](openstack-simulator.md) describes that stack.

## Required OpenStack service settings

Nova must emit unversioned notifications and must notify on `vm_state` changes.
The collector reads the unversioned format only: `event_type`, `message_id`,
`timestamp`, and a flat `payload`, taken from the `oslo.message` member of the
envelope. A nova configured for `versioned` notifications publishes other type
names and wraps its payload in `nova_object.data`, which the mapping table does
not know.

Nova, neutron, cinder, and glance must publish through the `messagingv2`
notification driver. A service left on `noop` sends nothing, and the collector
has nothing to consume for it.

```ini
[DEFAULT]
# nova only
notification_format = unversioned
notify_on_state_change = vm_state

[oslo_messaging_notifications]
# nova, neutron, cinder, and glance
driver = messagingv2
```

The broker must cap the message size, because the collector cannot. It bounds
the bodies it parses at 1 MiB, but that check runs once the AMQP client has
already assembled the whole message in memory, and RabbitMQ implements no
`prefetch_size`. What is resident is therefore `TALLY_OSC_PREFETCH` times the
broker's largest permitted message: at the default prefetch of 100 and
RabbitMQ's own default `max_message_size` of 128 MiB, one publisher is enough to
put the collector past any sane pod memory limit, and none of those deliveries
are acknowledged, so the next pod is handed the same batch. Set RabbitMQ's
`max_message_size` so that `TALLY_OSC_PREFETCH` times that value fits the pod's
memory limit — 4 MiB is far above any oslo notification and leaves 400 MiB
resident at the default prefetch — or lower `TALLY_OSC_PREFETCH` to match a
limit the deployment cannot change.

## Exchanges and topics

`TALLY_OSC_EXCHANGES` names the service exchanges the collector binds its queue
to. `TALLY_OSC_TOPICS` names the notification topics bound on each of them. Both
are deployment configuration on the OpenStack side: an exchange is a service's
`control_exchange`, a topic is one of its `notification_topics`. The defaults
`nova,neutron,cinder,glance` and `notifications.info` match the stock settings.
A deployment that renamed an exchange or publishes on a topic of its own lists
its values instead.

The collector declares the exchanges passively and creates none of them, so an
exchange that does not exist on the broker fails the connection with an error
naming it. Its own queue is `tally-notifications`, durable and bound to every
exchange and topic pair. Notifications therefore pile up in that queue while the
collector is down and are consumed when it returns.

## The ingest credential

An ingest credential is scoped to one (platform, cloud) pair. Issue one on the
Tally side:

```sh
tally-reporting-admin create-ingest-credential \
  --platform openstack \
  --cloud <cloud> \
  --description 'event collector for <cloud>'
```

The token is printed on stdout one time and is not recoverable afterwards. It
goes into `TALLY_OSC_TOKEN`, or into a file named by `TALLY_OSC_TOKEN_FILE`,
which is the path a Kubernetes Secret volume takes.

`TALLY_OSC_CLOUD` must be the cloud the credential was issued for. The API
checks every item of a batch against the credential's scope and refuses an event
whose platform or cloud lies outside it, with the reason `scope`, while the rest
of the batch is stored. A refused item is never resent, so a `TALLY_OSC_CLOUD`
that does not match the credential loses every event the collector sends under
it.

## Verifying a deployment with the dump

Oslo type names and payload members differ per OpenStack release. Check what a
deployment actually publishes before the collector is pointed at it:

```sh
export TALLY_OSC_AMQP_URL='amqp://user:password@rabbitmq.example:5672/'
export TALLY_OSC_EXCHANGES=nova,neutron,cinder,glance
export TALLY_OSC_TOPICS=notifications.info
tally-openstack-collector --dump
```

The AMQP variables above are everything the dump reads. It needs no cloud, no
Reporting API, no token, and no outbox. It prints one JSON line per delivery
with the exchange, the routing key, the message id, the event type, the
timestamp, and the payload. A body it cannot parse is printed under
`unparseable`, with the credentials an oslo request context carries
(`_context_auth_token`, `_context_password`) replaced by `[redacted]` and the
rest cut off after 512 bytes. A body that is not JSON at all — a
msgpack-serialized notification, for one — is reported by its size alone and not
printed, because that redaction is written against JSON quoting and cannot reach
a credential in those bytes. The dump's output is a file that gets attached to
tickets, and a Keystone token stays valid for hours.

The dump consumes through a server-named, exclusive, auto-deleting queue and
acknowledges automatically. A topic exchange copies every message to every bound
queue, so what the dump prints is a copy: the durable `tally-notifications`
queue, Ceilometer, and a collector running at the same time keep receiving
theirs. An interrupted dump leaves no queue behind.

With the dump running, perform the operations that are meant to be billed: boot
and delete an instance, create and resize a volume, allocate and release a
floating IP, upload and delete an image. Then compare what it printed against
the mapping table in
[`internal/providers/openstack/mapping.go`](../internal/providers/openstack/mapping.go):

- the printed `event_type` values against the oslo types listed below,
- the payload members each entry reads (`instance_id`, `tenant_id`, `vcpus`,
  `memory_mb`, `root_gb`, `ephemeral_gb`, `instance_type`, `volume_id`, `size`,
  `volume_type`, `floatingip.id`, `owner`) against the payloads printed,
- the deliveries against the recorded samples under
  [`internal/providers/openstack/testdata/golden/notifications/`](../internal/providers/openstack/testdata/golden/notifications),
  whose expected events sit next to them under
  [`testdata/golden/events/`](../internal/providers/openstack/testdata/golden/events).

Where the deployment diverges, edit the table entry and the fixture pair for
that type. The table is data for exactly this reason: adapting the collector to
a release is an edit to it and to nothing around it. A type absent from the
table is counted as skipped and recorded nowhere.

### Mapped oslo event types

| oslo `event_type` | Tally `event_type` |
| --- | --- |
| `compute.instance.create.end` | `compute.instance.create.end` |
| `compute.instance.delete.end` | `compute.instance.delete.end` |
| `compute.instance.resize.end` | `compute.instance.resize.end` |
| `compute.instance.finish_resize.end` | `compute.instance.resize.end` |
| `compute.instance.shelve_offload.end` | `compute.instance.shelve` |
| `compute.instance.unshelve.end` | `compute.instance.unshelve` |
| `compute.instance.power_off.end` | `compute.instance.power_off` |
| `compute.instance.power_on.end` | `compute.instance.power_on` |
| `volume.create.end` | `volume.create.end` |
| `volume.delete.end` | `volume.delete.end` |
| `volume.resize.end` | `volume.resize.end` |
| `volume.retype` | `volume.retype` |
| `volume.transfer.accept.end` | `volume.transfer.accept.end` |
| `floatingip.create.end` | `floatingip.create.end` |
| `floatingip.delete.end` | `floatingip.delete.end` |
| `image.create` | `image.create` |
| `image.upload` | `image.create` |
| `image.delete` | `image.delete` |

An `image.create` whose payload carries no size, or a size of zero, is skipped.
Glance creates an image before its bits are uploaded, and the `image.upload`
that follows is what the image is booked from.

## Refused events

An item the API refuses does not fail its batch. The answer to
`POST /api/v1/events` names each refused item with its index, its event id, and
the reason, the collector logs one warning per item, and the batch is deleted
from the outbox with the rest of it. Refused items are not retried.

What the API keeps of a refused item depends on the reason. An item that failed
validation, such as an event without a resource id or with a size the resource
type does not accept, is stored with the raw body it was submitted as and is
readable through `GET /api/v1/rejected-events` (admin role, one page per call,
ordered by the instant of refusal). An item outside the credential's scope
leaves an audit row of action `events.scope_violation` and no dead-letter row,
so it does not show up in that view. The collector's log and the `audit_log`
table are where it is found.

Mapping itself never fails. A notification whose payload the table did not
understand still becomes an event, gets refused at ingestion, and lands in the
dead-letter view with the reason it broke. That is what makes the view the place
to look after a release upgrade changed a payload.

Size is the exception to at-least-once, and it is bounded on both sides of the
buffer. A delivery whose body is larger than 1 MiB is acknowledged unread and
counted as unparseable, and a notification whose mapped event is larger than
64 KiB is acknowledged and counted as skipped: an oslo notification is
kilobytes, and anything past these bounds is a message the collector could
neither parse nor ever deliver, so keeping it would only hold up every message
behind it. Both are logged with the exchange, the routing key, and the size.
At the far end a 413 halves the batch until it fits, and the batch size then
grows back gradually rather than jumping to `TALLY_OSC_BATCH_MAX`, so a
Reporting API or an ingress with a smaller body limit settles on a size that
fits. Only an event past the 64 KiB bound is dropped when it is refused alone.
A smaller one is kept and retried like any other refusal: below that bound the
413 describes the destination, not the event, and an ingress configured with a
small `client_max_body_size` would otherwise drain the entire outbox into
nothing one event at a time.

## Configuration, storage, and health

[`cmd/tally-openstack-collector/.env.example`](../cmd/tally-openstack-collector/.env.example)
lists every variable with its default and its meaning. Two of them decide how a
deployed collector behaves: where the outbox lives, and where the probes answer.

`TALLY_OSC_BUFFER_PATH` points at the SQLite outbox and belongs on a volume that
outlives the pod. Between the acknowledgement on the bus and the delivery to the
Reporting API, an event lives in that file and nowhere else.

The volume is sized from `TALLY_OSC_BUFFER_MAX_EVENTS`, which is the depth the
collector stops consuming at. A buffered event runs a few hundred bytes, so the
default of a million events reaches roughly half a gigabyte before the bound
stops it, and that is what the volume has to hold for the collector to survive
a Reporting API outage without the notifications piling up on the bus. The file
is opened with incremental auto-vacuum and every delivered batch reclaims freed
pages back to the filesystem, so an outage that filled the buffer does not leave
the volume full once the backlog has drained.

`TALLY_OSC_REPORTING_URL` is the base URL the ingest path is appended to. It has
to be absolute and carry a host, and it has to be `https`, because the ingest
token travels in a header on every flush and the link between the OpenStack
control plane and Tally is not one the cluster's own TLS covers. A deployment
where that link is trusted sets `TALLY_OSC_REPORTING_INSECURE=true` to allow a
plaintext one; the collector refuses to start otherwise.

`TALLY_OSC_HTTP_PORT` is the port of `/healthz`, `/readyz`, and `/metrics`. The
collector serves no API of its own.

Readiness fails as soon as the broker connection or the outbox does, which takes
the pod out of service while leaving it running. What failed is in the log; the
body says only that the collector is not ready, because the route carries no
credential.

Liveness weighs the outbox alone, and fails only once it has been unusable
without a break for `TALLY_OSC_UNHEALTHY_THRESHOLD_S` seconds, 600 by default.
A broker outage never fails it: restarting brings back no broker, and while the
broker is away the sender is the loop still making progress, draining the buffer
to the Reporting API. Restarting the pod would abort that delivery and start its
backoff over for as long as the outage lasts.

---
title: How the collector consumes a bus
description: Why the OpenStack collector acknowledges late, buffers on disk, and treats an oversized message as the one thing it drops.
quadrant: explanation
audience: all
---

# How the collector consumes a bus

The collector sits between an OpenStack broker and the Reporting API and owns
one guarantee: a notification it acknowledged is a notification Tally will get.
This page says what that guarantee costs and where it stops. The flags and
variables are on
[the `tally-openstack-collector` reference page](/reference/command-line/tally-openstack-collector),
and the steps to point a collector at a cloud are in
[connect the collector](/how-to/openstack/connect-the-collector).

## At-least-once over an outbox

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

Consume, map, buffer, then acknowledge, with the sender as a loop of its own, is
the architecture WP 1.12 of
[the Phase 1 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/01-phase-1-core-platform-openstack.md)
specified. The order is what carries the guarantee: an acknowledgement before
the commit would lose an event to a crash, and a commit without an
acknowledgement would replay one the outbox already holds.

## What the services publish

Nova must emit unversioned notifications and must notify on `vm_state` changes.
The collector reads the unversioned format only: `event_type`, `message_id`,
`timestamp`, and a flat `payload`, taken from the `oslo.message` member of the
envelope. A nova configured for `versioned` notifications publishes other type
names and wraps its payload in `nova_object.data`, which the mapping table does
not know.

Octavia notifies on a load balancer's create, update, and delete, and on nothing
below it: a listener, a pool, a member, or a health monitor changes without a
notification, and so does a failover. Reconciliation is what books those. The
notifications are on by default, since `[controller_worker] event_notifications`
defaults to `True`, but octavia sends none until the `messagingv2` driver is
set, because oslo's own default for that setting is the empty string. A service
left on `noop` sends nothing, and the collector has nothing to consume for it.

## What bounds resident memory

The broker must cap the message size, because the collector cannot. It bounds
the bodies it parses at 1 MiB, but that check runs once the AMQP client has
already assembled the whole message in memory, and RabbitMQ implements no
`prefetch_size`. What is resident is therefore `TALLY_OSC_PREFETCH` times the
broker's largest permitted message: at the default prefetch of 100 and
RabbitMQ's own default `max_message_size` of 128 MiB, one publisher is enough to
put the collector past any sane pod memory limit, and none of those deliveries
are acknowledged, so the next pod is handed the same batch. Set RabbitMQ's
`max_message_size` so that `TALLY_OSC_PREFETCH` times that value fits the pod's
memory limit (4 MiB is far above any oslo notification and leaves 400 MiB
resident at the default prefetch), or lower `TALLY_OSC_PREFETCH` to match a
limit the deployment cannot change.

## The queue and the exchanges

The collector declares the exchanges passively and creates none of them, so an
exchange that does not exist on the broker fails the connection with an error
naming it. Its own queue is `tally-notifications`, durable and bound to every
exchange and topic pair. Notifications therefore pile up in that queue while the
collector is down and are consumed when it returns.

Octavia's `control_exchange` is `octavia`, and the default leaves it out on
purpose. Because that declare is passive, a collector listing an exchange the
broker does not carry never connects at all, so a default naming `octavia` would
stop every deployment that runs none. A deployment with octavia sets
`TALLY_OSC_EXCHANGES=nova,neutron,cinder,glance,octavia`. Until it does,
octavia's notifications reach no queue of this collector and show up in none of
its counters, `tally_collector_skipped_total` included: a topic exchange copies
a message only to the queues bound to it.

## What the dump can and cannot show

Oslo type names and payload members differ per OpenStack release, so
`tally-openstack-collector --dump` exists to show what a deployment actually
publishes before the collector is pointed at it.

The AMQP variables are everything the dump reads. It needs no cloud, no
Reporting API, no token, and no outbox. It prints one JSON line per delivery
with the exchange, the routing key, the message id, the event type, the
timestamp, and the payload. A body it cannot parse is printed under
`unparseable`, with the credentials an oslo request context carries
(`_context_auth_token`, `_context_password`) replaced by `[redacted]` and the
rest cut off after 512 bytes. A body that is not JSON at all, a
msgpack-serialized notification for one, is reported by its size alone and not
printed, because that redaction is written against JSON quoting and cannot reach
a credential in those bytes. The dump's output is a file that gets attached to
tickets, and a Keystone token stays valid for hours.

The dump consumes through a server-named, exclusive, auto-deleting queue and
acknowledges automatically. A topic exchange copies every message to every bound
queue, so what the dump prints is a copy: the durable `tally-notifications`
queue, Ceilometer, and a collector running at the same time keep receiving
theirs. An interrupted dump leaves no queue behind.

Where the deployment diverges from
[the notification mapping](/reference/formats/notification-mapping), edit the
table entry and the fixture pair for that type. The table is data for exactly
this reason: adapting the collector to a release is an edit to it and to nothing
around it. A type absent from the table is counted as skipped and recorded
nowhere.

## Why mapping never fails and size is the exception

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

## The outbox on disk

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
plaintext one; the collector refuses to start otherwise. Every variable named
here is listed with its default on
[the collector settings page](/reference/configuration/tally-openstack-collector).

## Readiness and liveness

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

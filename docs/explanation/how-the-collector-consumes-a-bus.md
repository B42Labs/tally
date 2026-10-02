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

The collector's own queue is `tally-notifications`, durable and bound to every
topic on every listed exchange the broker carries. Notifications therefore pile
up in that queue while the collector is down and are consumed when it returns.

The collector probes each exchange in `TALLY_OSC_EXCHANGES` with a passive
declare and creates none of them. The options a service declares its exchange
with differ per deployment, so a collector that created one would have to guess
them, and creating one needs a permission on the service exchanges that the
collector should not hold. The default list is `nova,neutron,openstack,glance`,
where `openstack` is oslo's default exchange: cinder sets no `control_exchange`
and publishes there.

An exchange the broker does not carry is skipped. The collector logs which ones
are missing, binds the others, and probes each missing one again after 1 s,
doubling to 60 s, until it exists and is bound. A fresh cloud is in that state
as a matter of course: glance declares its exchange with its first notification,
so the exchange is missing until the first image is uploaded. RabbitMQ logs each
probe of a missing exchange as a channel error.

What was published on an exchange before the collector bound it is not
collected, because a topic exchange drops a message no queue is bound to. A
missing exchange therefore costs the notifications of that one service until it
appears, and those of no other.

When none of the listed exchanges exists, the session fails and the collector
reconnects: a queue bound to nothing has nothing to consume, and a wrong vhost
must not read as a ready collector. `TALLY_OSC_REQUIRE_EXCHANGES=true` extends
that stop to a single missing exchange, for a deployment that would rather
collect nothing than a part of the cloud. The collector then consumes nothing
until the broker carries every exchange it lists.

Octavia's `control_exchange` is `octavia`, and the default leaves it out on
purpose: a cloud without octavia would report the exchange missing for as long
as the collector runs. A deployment with octavia sets
`TALLY_OSC_EXCHANGES=nova,neutron,openstack,glance,octavia`. Until it does,
octavia's notifications reach no queue of this collector and show up in none of
its counters, `tally_collector_skipped_total` included: a topic exchange copies
a message only to the queues bound to it.

## The default queues oslo declares

Before it publishes a notification, oslo.messaging declares a queue named after
the routing key and binds it to the exchange, so that a consumer that arrives
later finds what it missed. The function is
[`_publish_and_creates_default_queue`](https://opendev.org/openstack/oslo.messaging/src/branch/stable/2025.1/oslo_messaging/_drivers/impl_rabbit.py).
The routing key is the topic with the priority appended, so one default queue
exists per priority: `notifications.info`, `notifications.error` and so on.

The collector reads none of them and binds `tally-notifications`. A topic
exchange copies each message to every bound queue, so a queue of its own hands
the collector every notification and leaves Ceilometer's copies in the queue
Ceilometer consumes. A collector that consumed `notifications.info` would share
that queue's messages with Ceilometer, would have to declare the queue with the
arguments oslo chose, and would not empty the queues of the other priorities.

On a cloud without Ceilometer nothing consumes the default queues. Without a
consumer and without a policy they grow until the broker's memory or disk alarm
stops every publisher, and the publishers are the OpenStack services. A
[policy](https://www.rabbitmq.com/docs/policies) that bounds the length of a
queue and the lifetime of a message keeps them small, and
[connect the collector](/how-to/openstack/connect-the-collector#cap-the-default-notification-queues)
has the commands.

## Classic and quorum

The collector declares `tally-notifications` as a quorum queue unless
`TALLY_OSC_QUEUE_TYPE` is `classic`. A classic queue lives on one node of a
broker cluster, and its backlog is unavailable while that node is down. The
collector relies on that backlog: at the buffer bound, and while the collector
itself is down, the notifications wait on the bus. A
[quorum queue](https://www.rabbitmq.com/docs/quorum-queues) is replicated
across the nodes and stays available while a majority of them is up.

From RabbitMQ 4.0 a quorum queue has a delivery limit of 20, and drops a
message that came back to the queue more often than that. On RabbitMQ 4.3.6
what counts is a delivery whose connection closed before it was acknowledged. A
`basic.nack` with requeue does not count, and that is what the collector sends
once per second while the outbox refuses an insert and once per pause at the
buffer bound. The limit reaches the collector through its sessions. A session
holds up to `TALLY_OSC_PREFETCH` deliveries unacknowledged, and one that ends
returns them. While the outbox refuses inserts nothing is acknowledged, so
every reconnect and every restart returns the same notifications, and the
twenty-first would lose them. The collector therefore declares the queue with
`x-delivery-limit` set to `-1`, which disables the limit, so an operator has no
policy to set for it. A policy that sets `delivery-limit` on this queue takes
the guarantee away all the same: RabbitMQ 4.3.6 lets a positive limit from a
policy or an operator policy win over the argument, and the collector cannot
see policies.

A broker older than 4.0 reads `-1` as a limit: RabbitMQ 3.13.7 drops a message
on its first requeue. The collector refuses to declare a quorum queue there.
The session ends before it declares anything, and the collector reconnects.
The default asks for a quorum queue, so on such a broker the collector consumes
nothing until `TALLY_OSC_QUEUE_TYPE=classic` is set.

The type is fixed when a queue is declared. Where the queue exists with the
other type the broker refuses the declare, and the collector reports the
mismatch and deletes nothing, because deleting a queue discards its backlog.
Moving the queue to the other type is an operator's step, in
[connect the collector](/how-to/openstack/connect-the-collector#move-the-queue-to-another-type).
The default is `quorum`. `classic` sends no queue type at all, which is the
declare of every collector up to v0.2.0, so a deployment upgraded from one
finds a queue the default declare is refused over and either sets `classic` or
moves the queue. `classic` is the setting for a broker older than RabbitMQ 4.0
and for an operator who wants the queue on one node.

## What the broker account may do

The collector declares its queue, binds it and consumes from it. It publishes
nothing, declares no exchange and deletes nothing, so its account gets by on
RabbitMQ's three
[permission patterns](https://www.rabbitmq.com/docs/access-control) at their
narrowest: configure and write on its own queue, and read on that queue and on
the service exchanges. The dump's server-named queue is the one other name the
patterns carry.

Those patterns still allow more than the collector does. The service exchanges
carry RPC as well as notifications: the calls the services make to each other,
with their arguments and a request context that holds a Keystone token. Read
permission on a topic exchange allows a binding with any routing key, so an
account with the three patterns alone can bind `#` on `nova` and is handed a
copy of every message published there. A topic permission is what keeps the
account to the notification topics. With one in place, RabbitMQ 4.3.6 refuses a
binding of `#`, of `conductor` and of `notifications.error` on `nova` with
`ACCESS_REFUSED - read access to topic`, and accepts `notifications.info`.

A topic permission is checked when a binding is made and not when a message is
routed, so a binding that predates it stays. The permission therefore belongs
in place before the account is used for the first time, which is the order
[connect the collector](/how-to/openstack/connect-the-collector#create-the-broker-account)
creates the account in.

## What the dump can and cannot show

Oslo type names and payload members differ per OpenStack release, so
`tally-openstack-collector --dump` exists to show what a deployment actually
publishes before the collector is pointed at it.

The AMQP variables are everything the dump reads. It needs no cloud, no
Reporting API, no token, and no outbox. It prints one JSON line per delivery
with the exchange, the routing key, the message id, the event type, the
timestamp, and the payload.

A payload carries more than the mapping reads. Nova's
`scheduler.select_destinations` notifications carry the request context with
its Keystone token inside the payload, and cinder's `volume.attach` and
`volume.detach` notifications carry `connection_info` with the Ceph monitors,
the user and the secret UUID. The dump therefore replaces the value of every
member whose name contains `password`, `token`, `secret` or `connection_info`,
in any letter case and at any depth, with `[redacted]`, whatever the value is.
The rule goes by fragments and not by exact names because services name the
same credential differently (`auth_token`, `_context_auth_token`,
`auth_password`), and no member the mapping reads matches one.

A body the dump cannot parse is printed under `unparseable`, with the same
members replaced and the rest cut off after 512 bytes. A body that is not JSON
at all, a msgpack-serialized notification for one, is reported by its size
alone, and so is an envelope whose `oslo.message` is not JSON, because the rule
reads member names and those bytes have none it can find.

The rule cannot reach a credential under a name that matches none of the four
fragments, nor one inside a string value, such as a URL that carries a
password. Project ids, user ids, host names and addresses are printed as they
arrived. The output is therefore a file to delete once the comparison is done
and not one to attach to a ticket, and a Keystone token stays valid for hours.

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

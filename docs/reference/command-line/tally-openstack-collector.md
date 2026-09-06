---
title: OpenStack collector (tally-openstack-collector)
description: The two modes of the OpenStack collector, its flag, its AMQP consumption, its HTTP routes and the bounds it applies to a notification.
quadrant: reference
audience: operator
---

# OpenStack collector (tally-openstack-collector)

`tally-openstack-collector` collects OpenStack usage events. It consumes
oslo.messaging notifications from AMQP, maps them to Tally events, buffers them
in a SQLite outbox, and posts them to the Reporting API from a loop of its own.
Both loops retry what failed, so the process comes up while the broker or the
Reporting API is unavailable and reports that state through the probes rather
than through a failed start. Beside them it serves the probes and the Prometheus
exposition on the configured port; it serves no API of its own. The process is
assembled in
[`cmd/tally-openstack-collector/main.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-openstack-collector/main.go).

## Flags

<!-- refdoc:begin flags -->
| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--dump` | boolean | `false` | print the notifications the broker delivers as JSON lines instead of collecting them |
<!-- refdoc:end flags -->

## Modes

Collecting is the mode without a flag: the consumer, the sender, the outbox and
the HTTP server all run, and the configuration gate asks for everything the
pipeline reads.

`--dump` prints the notifications the broker delivers, one JSON line per
delivery, and does nothing else: no HTTP, no outbox, no delivery. Its gate asks
for `TALLY_OSC_AMQP_URL` alone. The mode is how the exchanges, the topics and
the event types of a deployment are checked before the collector is pointed at
it.

## AMQP consumption

The queue, the consumer tag and the exchange kind are declared in
[`internal/providers/openstack/osloamqp.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/osloamqp.go).
The exchanges and the topics are configuration, and their defaults stand in
[`internal/providers/openstack/config.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/config.go).

The collector's own queue is `tally-notifications`, declared durable, so
notifications pile up in it while the collector is down and are consumed when it
returns. It is bound to every pair of exchange and topic the configuration
names. `TALLY_OSC_EXCHANGES` defaults to `nova,neutron,cinder,glance` and
`TALLY_OSC_TOPICS` to `notifications.info`, which are the stock OpenStack
settings; a topic is the routing key itself and not a prefix of one, because
that is how oslo publishes.

The exchanges are declared passively and none of them is created. Which options
a service declared its exchange with differs per deployment, so a collector that
declared them itself would have to guess. An exchange the broker does not carry
fails the declare, closes the channel, and the collector reconnects with the
error naming the exchange it waits for.

Octavia's `control_exchange` is `octavia` and the default leaves it out on
purpose: because the declare is passive, a default naming it would stop every
deployment that runs none. A deployment with octavia sets
`TALLY_OSC_EXCHANGES=nova,neutron,cinder,glance,octavia`. Until it does,
octavia's notifications reach no queue of this collector and show up in none of
its counters.

The consumer registers under the tag `tally-openstack-collector` and
acknowledges nothing automatically. A delivery is acknowledged after the outbox
has committed the event it maps to, so a failed acknowledgement costs a
redelivery and never an event. A delivery the outbox refuses is requeued
instead, and stays on the broker until a buffer that works takes it.

## Bounds

Two sizes bound a notification on its way into the buffer, both stated as
constants in
[`internal/providers/openstack/osloamqp.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/osloamqp.go).

A delivery whose body is larger than 1 MiB is acknowledged unread and counted as
unparseable. The bound comes before the parse: the parse is what an oversized
body would take the process down in, and a process that dies there never
acknowledges the delivery.

A notification whose mapped event is larger than 64 KiB is acknowledged and
counted as skipped. An event that large is one the ingest endpoint refuses for
its size, so buffering it would put it in front of every event behind it.

At the far end a 413 halves the batch until it fits, and the batch size then
grows back gradually rather than jumping to `TALLY_OSC_BATCH_MAX`, so a
Reporting API or an ingress with a smaller body limit settles on a size that
fits. Only an event past the 64 KiB bound is dropped when it is refused alone. A
smaller one is kept and retried like any other refusal: below that bound the 413
describes the destination and not the event.

## Refused items

An item the API refuses does not fail its batch. The answer to
`POST /api/v1/events` names each refused item with its index, its event id and
the reason, the collector logs one warning per item, and the batch is deleted
from the outbox with the rest of it. Refused items are not retried.

What the API keeps of a refused item depends on the reason. An item that failed
validation is stored with the raw body it was submitted as and is readable
through `GET /api/v1/rejected-events`. An item outside the credential's scope
leaves an audit row of action `events.scope_violation` and no dead-letter row,
so it does not show up in that view; the collector's log and the `audit_log`
table are where it is found.

Mapping itself never fails. A notification whose payload the table did not
understand still becomes an event, gets refused at ingestion, and lands in the
dead-letter view with the reason it broke.

## HTTP routes

Three routes are served on `TALLY_OSC_HTTP_PORT`, none of them with a
credential. Each probe answers in plain text.

`GET /readyz` answers 200 with `ok` while the consumer holds a connection to the
broker and the outbox answers. It answers 503 with `the collector is not ready`
otherwise, which takes the pod out of the Service's endpoints while leaving it
running. What failed goes to the log rather than into the body.

`GET /healthz` weighs the outbox alone, and only against time. It answers 200
with `ok` while the outbox answers, and it keeps answering 200 while the outbox
has been unusable for fewer seconds than `TALLY_OSC_UNHEALTHY_THRESHOLD_S`.
Past that threshold it answers 503 with
`the collector has been unhealthy for too long`. The broker is deliberately not
part of it: during a broker outage the sender is the loop that can still make
progress, and a liveness that failed on the broker would restart every replica
for the length of that outage.

Both probes bound their outbox check at 2 seconds, so a probe never outlasts the
request that asked it.

`GET /metrics` serves the Prometheus exposition. A deployment that sets
`TALLY_METRICS_ENABLED` to `false` is answered 404 there rather than an empty
exposition, which would read as a collector with no activity. The consumer and
the sender keep counting either way.

## Signals and exit status

SIGINT and SIGTERM begin a graceful shutdown. The HTTP server stops accepting
connections, in-flight requests get 10 seconds, the consumer stops reading from
the broker, the sender finishes the attempt it is in, and the outbox is closed
once both have returned. What is buffered stays in the file, where the next
start picks it up, and the process exits 0.

Every other failure exits 1: a configuration that was refused, an outbox that
could not be opened, and a listener that ended with anything but a closed
server.

## See also

The collector settings page lists every variable with its default. The
notification mapping page states which event type maps to which Tally event, and
the OpenStack metrics page states the series the exposition carries.

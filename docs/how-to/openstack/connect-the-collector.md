---
title: Connect the collector to an OpenStack cloud
description: Configure the OpenStack services, the broker and the collector so that one cloud's notifications reach the Reporting API.
quadrant: how-to
audience: operator
---

# Connect the collector to an OpenStack cloud

This guide points one `tally-openstack-collector` at one OpenStack cloud. At
the end the services publish the notifications the collector reads, its queue
is bound to their exchanges, and the events of that cloud arrive at the
Reporting API. What the collector guarantees between those two ends is in
[how the collector consumes a bus](/explanation/how-the-collector-consumes-a-bus).

## Before you start

- A broker the collector reaches over AMQP, with a user that may declare a
  durable queue and bind it to the service exchanges.
- Access to the configuration files of nova, neutron, cinder, glance and
  octavia, and permission to restart them.
- A shell that may run
  [`tally-reporting-admin`](/reference/command-line/tally-reporting-admin)
  against the reporting database, for the ingest credential this cloud reports
  under.
  [Issue and revoke credentials](/how-to/openstack/issue-and-revoke-credentials)
  has those steps.
- The [collector settings](/reference/configuration/tally-openstack-collector)
  page, which lists every variable this guide sets with its default.
- A collector to point at the cloud. This guide exports the variables into a
  shell;
  [install the collector from the Debian package](/how-to/openstack/install-the-debian-package)
  puts the same settings into `/etc/default/tally-openstack-collector` and runs
  it as a systemd service instead.

## Configure the OpenStack services

1. Set nova to the unversioned notification format and to notifying on
   `vm_state` changes, and set the notification driver on nova, neutron,
   cinder, glance and octavia. The first block belongs to nova alone, the
   second to all five:

   ```ini
   [DEFAULT]
   # nova only
   notification_format = unversioned
   notify_on_state_change = vm_state

   [oslo_messaging_notifications]
   # nova, neutron, cinder, glance, and octavia
   driver = messagingv2
   ```

   Octavia's `[controller_worker] event_notifications` defaults to `True` and
   needs no change. Oslo's own default for the driver is the empty string, so
   octavia stays silent until the second block is set on it.

2. Restart the services whose configuration changed, and check that they came
   back:

   ```sh
   systemctl restart nova-api nova-compute neutron-server cinder-api glance-api octavia-api
   systemctl is-active nova-api nova-compute neutron-server cinder-api glance-api octavia-api
   ```

   ```text
   active
   active
   active
   active
   active
   active
   ```

## Cap the broker's message size

1. Set RabbitMQ's `max_message_size` in `rabbitmq.conf` so that
   `TALLY_OSC_PREFETCH` times that value fits the collector pod's memory limit.
   RabbitMQ's own default is 128 MiB. 4 MiB is far above any oslo notification
   and leaves 400 MiB resident at the default prefetch of 100:

   ```ini
   max_message_size = 4194304
   ```

2. Restart the broker and read back what it runs with:

   ```sh
   rabbitmqctl eval 'application:get_env(rabbit, max_message_size).'
   ```

   ```text
   {ok,4194304}
   ```

3. Where the deployment cannot change that bound, lower `TALLY_OSC_PREFETCH`
   until the product fits the memory limit. At RabbitMQ's own default of
   128 MiB, a prefetch of 3 holds 384 MiB:

   ```sh
   export TALLY_OSC_PREFETCH=3
   ```

## Bind the exchanges and topics

1. Name the service exchanges in `TALLY_OSC_EXCHANGES` and the notification
   topics in `TALLY_OSC_TOPICS`. An exchange is a service's `control_exchange`,
   a topic one of its `notification_topics`. The defaults
   `nova,neutron,cinder,glance` and `notifications.info` are the stock
   settings; a deployment that runs octavia lists it as well, and one that
   renamed an exchange or publishes on a topic of its own lists its values
   instead:

   ```sh
   export TALLY_OSC_EXCHANGES=nova,neutron,cinder,glance,octavia
   export TALLY_OSC_TOPICS=notifications.info
   ```

2. Check that the broker carries every exchange you listed. The collector
   declares them passively and creates none, so one the broker does not carry
   fails the connection with its name in the error:

   ```sh
   rabbitmqctl list_exchanges name type | grep -E '^(nova|neutron|cinder|glance|octavia)[[:space:]]'
   ```

   ```text
   nova	topic
   neutron	topic
   cinder	topic
   glance	topic
   octavia	topic
   ```

## Configure the collector

1. Set `TALLY_OSC_CLOUD` to the cloud the ingest credential was issued for, and
   hand the collector the token in `TALLY_OSC_TOKEN` or in the file
   `TALLY_OSC_TOKEN_FILE` names, which is the path a Kubernetes Secret volume
   takes. The API refuses an event whose cloud lies outside the credential's
   scope with the reason `scope`, and a refused item is never resent:

   ```sh
   export TALLY_OSC_CLOUD=os-prod-eu1
   export TALLY_OSC_TOKEN_FILE=/run/secrets/tally/ingest-token
   ```

2. Point the sender at the Reporting API. `TALLY_OSC_REPORTING_URL` has to be
   absolute, carry a host and use `https`; a deployment whose link to Tally is
   trusted sets `TALLY_OSC_REPORTING_INSECURE=true` for a plaintext one, and
   the collector refuses to start otherwise:

   ```sh
   export TALLY_OSC_REPORTING_URL=https://tally-reporting.internal
   ```

3. Put `TALLY_OSC_BUFFER_PATH` on a volume that outlives the pod, sized from
   `TALLY_OSC_BUFFER_MAX_EVENTS`. A buffered event runs a few hundred bytes, so
   the default of a million events reaches roughly half a gigabyte:

   ```sh
   export TALLY_OSC_BUFFER_PATH=/var/lib/tally/outbox.db
   export TALLY_OSC_BUFFER_MAX_EVENTS=1000000
   ```

4. Set `TALLY_OSC_HTTP_PORT` to the port `/healthz`, `/readyz` and `/metrics`
   answer on, then start the collector and keep its log:

   ```sh
   export TALLY_OSC_HTTP_PORT=8080
   tally-openstack-collector 2>&1 | tee collector.log
   ```

   ```json
   {"time":"2026-07-09T14:22:00.512Z","level":"INFO","msg":"listening","port":8080}
   ```

## Check the result

1. In a second shell, ask for readiness. It answers 200 while the consumer
   holds the broker connection and the outbox answers; the
   [`tally-openstack-collector` reference page](/reference/command-line/tally-openstack-collector)
   states all three routes:

   ```sh
   curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/readyz
   ```

   ```text
   200
   ```

2. Read the two counters twice, a minute apart, while the cloud is in use. Both
   rise:

   ```sh
   curl -sS http://127.0.0.1:8080/metrics | grep -E '^tally_collector_(consumed|delivered)_total'
   ```

   ```text
   tally_collector_consumed_total{event_type="compute.instance.create.end"} 14
   tally_collector_delivered_total 12
   ```

3. Count the two failures that keep delivered events at zero. An `x509` error
   is a Reporting API certificate the collector does not trust, and a 401 is an
   ingest token the API refused:

   ```sh
   grep -cE 'x509|answered 401' collector.log
   ```

   ```text
   0
   ```

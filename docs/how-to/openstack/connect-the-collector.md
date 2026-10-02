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

- A broker the collector reaches over AMQP, and an administrator shell on it
  for `rabbitmqctl`. The account the collector connects with is created in
  [create the broker account](/how-to/openstack/connect-the-collector#create-the-broker-account).
  The default queue type needs RabbitMQ 4.0 or newer, and an older broker
  takes `TALLY_OSC_QUEUE_TYPE=classic`, which
  [choose the queue type](/how-to/openstack/connect-the-collector#choose-the-queue-type)
  covers.
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

## Enable notifications on a Kolla deployment

Kolla-Ansible, and OSISM, which deploys through it, renders `driver = noop`
into `[oslo_messaging_notifications]` of a service unless one of the service's
notification topics is enabled, and it enables the `notifications` topic for
Ceilometer alone. On such a deployment these steps take the place of the
section above.

1. Enable the `notifications` topic of the five services in the Kolla
   configuration, which is `/etc/kolla/globals.yml`, or
   `environments/kolla/configuration.yml` on OSISM:

   ```yaml
   nova_notification_topics:
     - name: notifications
       enabled: true
   neutron_notification_topics:
     - name: notifications
       enabled: true
   cinder_notification_topics:
     - name: notifications
       enabled: true
   glance_notification_topics:
     - name: notifications
       enabled: true
   octavia_notification_topics:
     - name: notifications
       enabled: true
   ```

   Each variable replaces the role's whole list. A deployment that runs the
   Designate sink keeps the sink's topic as a second entry under nova and
   neutron:

   ```yaml
     - name: "{{ designate_notifications_topic_name }}"
       enabled: "{{ designate_enable_notifications_sink | bool }}"
   ```

2. Leave nova's notification settings alone where Ceilometer, Designate or the
   Infoblox IPAM agent is enabled. `notification_format` defaults to
   `unversioned`, and Kolla sets `notify_on_state_change = vm_and_task_state`
   for those three. That value is a superset of `vm_state`: it adds
   `compute.instance.update` notifications, which the collector counts as
   skipped. Where none of the three is enabled, set the option in a nova
   override, `/etc/kolla/config/nova.conf`, or
   `environments/kolla/files/overlays/nova.conf` on OSISM:

   ```ini
   [notifications]
   notify_on_state_change = vm_state
   ```

3. Reconfigure the five services:

   ```sh
   kolla-ansible reconfigure -i <inventory> --tags nova,cinder,neutron,glance,octavia
   ```

   On OSISM, once per service:

   ```sh
   osism apply -a reconfigure nova
   ```

4. Read the rendered section back from one of the containers:

   ```sh
   docker exec nova_api grep -A3 '^\[oslo_messaging_notifications\]' /etc/nova/nova.conf | grep -v '^transport_url'
   ```

   ```text
   [oslo_messaging_notifications]
   driver = messagingv2
   topics = notifications
   ```

   The section also holds `transport_url`, which carries the broker password
   and is filtered out for that reason.

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

## Cap the default notification queues

oslo.messaging declares a queue of its own per notification topic and
priority, and the collector reads none of them.
[The default queues oslo declares](/explanation/how-the-collector-consumes-a-bus#the-default-queues-oslo-declares)
has the reasons. On a Kolla or OSISM deployment the broker runs in a container,
and every `rabbitmqctl` call on this page runs as
`docker exec rabbitmq rabbitmqctl` there.

1. List the default queues with their consumers:

   ```sh
   rabbitmqctl list_queues name consumers messages | grep -E '^notifications\.'
   ```

   ```text
   notifications.info	0	18342
   notifications.error	0	27
   ```

   A consumer count of 0 means nothing reads the queue at this moment. Before
   capping it, confirm that the cloud runs no service that consumes it, which
   Ceilometer's notification agent does: an agent that is stopped or restarting
   shows 0 as well, and the length cap drops the oldest messages of the backlog
   it would have read as soon as the policy is set. A queue a service consumes
   is that service's backlog and gets no cap.

2. Cap them with a policy that keeps 10000 messages per queue and drops a
   message after 600000 ms:

   ```sh
   rabbitmqctl set_policy -p / --apply-to queues notifications-cap '^notifications\.' '{"max-length":10000,"message-ttl":600000}'
   ```

   The pattern does not match `tally-notifications`. Where a service consumes
   one of these queues, narrow the pattern to the others. RabbitMQ applies one
   policy per queue, so on a broker whose existing policy already matches these
   queues, add the two keys to that policy instead.

3. Read the policy back:

   ```sh
   rabbitmqctl list_policies -p /
   ```

   ```text
   vhost	name	pattern	apply-to	definition	priority
   /	notifications-cap	^notifications\.	queues	{"max-length":10000,"message-ttl":600000}	0
   ```

## Bind the exchanges and topics

1. Name the service exchanges in `TALLY_OSC_EXCHANGES` and the notification
   topics in `TALLY_OSC_TOPICS`. An exchange is a service's `control_exchange`,
   a topic one of its `notification_topics`. The defaults
   `nova,neutron,openstack,glance` and `notifications.info` are the stock
   settings: cinder sets no `control_exchange` and publishes on oslo's default,
   `openstack`. A deployment that runs octavia lists it as well, and one that
   renamed an exchange or publishes on a topic of its own lists its values
   instead:

   ```sh
   export TALLY_OSC_EXCHANGES=nova,neutron,openstack,glance,octavia
   export TALLY_OSC_TOPICS=notifications.info
   ```

2. Check which of the exchanges you listed the broker carries. The collector
   creates none of them:

   ```sh
   rabbitmqctl list_exchanges name type | grep -E '^(nova|neutron|openstack|glance|octavia)[[:space:]]'
   ```

   ```text
   nova	topic
   neutron	topic
   openstack	topic
   glance	topic
   octavia	topic
   ```

   An exchange missing from that output is skipped with a warning and bound
   within a minute of appearing. On a fresh cloud glance's exchange appears
   with the first image notification. That notification, and any published
   before the collector binds, reaches no queue, so create and delete one image
   before the cloud goes into billing. `TALLY_OSC_REQUIRE_EXCHANGES=true` makes
   the collector wait for every exchange instead.

## Create the broker account

The account holds three permission patterns and one topic permission per
exchange.
[What the broker account may do](/explanation/how-the-collector-consumes-a-bus#what-the-broker-account-may-do)
has the reasons. The patterns are not enough on RabbitMQ 4.3.0, which
[broker permissions](/reference/command-line/tally-openstack-collector#broker-permissions)
covers.

1. Create the account and grant it the three patterns, configure, write and
   read in that order, on the virtual host the services' `transport_url` names,
   which is `/` on Kolla. `add_user` reads the password from standard input,
   which keeps it out of the shell history and the process list:

   ```sh
   rabbitmqctl add_user tally < tally-broker-password
   rabbitmqctl set_permissions -p / tally \
     '^(tally-notifications|amq\.gen-.*)$' \
     '^(tally-notifications|amq\.gen-.*)$' \
     '^(tally-notifications|amq\.gen-.*|nova|neutron|openstack|glance|octavia)$'
   ```

   The file holds the password on one line; remove it afterwards. In a
   container the first call is
   `docker exec -i rabbitmq rabbitmqctl add_user tally < tally-broker-password`.

   The read pattern lists the exchanges in `TALLY_OSC_EXCHANGES` and no others,
   the ones the broker does not carry yet included. A cloud without octavia
   leaves it out here and in step 2. The `amq\.gen-.*` alternative is the
   server-named queue of `--dump`, and an account that never runs the dump can
   leave it out.

2. Restrict what the account may bind, once per exchange in the read pattern.
   The topic read pattern has to match every topic in `TALLY_OSC_TOPICS`:

   ```sh
   for exchange in nova neutron openstack glance octavia; do
     rabbitmqctl set_topic_permissions -p / tally "$exchange" '^$' '^notifications\.info$'
   done
   ```

   An exchange in the read pattern without a topic permission is readable
   under every routing key, RPC included. RabbitMQ 4.3.6 answers
   `Exchange glance does not exist` for an exchange the broker does not carry,
   where 3.13.7 accepts the call. The account is unrestricted on that exchange
   from the moment it appears, so repeat the call as soon as it does, which for
   glance on a fresh cloud is after the first image notification. Then list
   what the account bound there, because a binding that predates the topic
   permission stays:

   ```sh
   rabbitmqctl list_bindings source_name destination_name routing_key | grep -E '^glance[[:space:]]+(tally-notifications|amq\.gen-)'
   ```

   ```text
   glance	tally-notifications	notifications.info
   ```

   A line with another routing key than a topic in `TALLY_OSC_TOPICS` is a
   binding made while the exchange was unrestricted. It keeps copying messages
   until it is unbound or its queue is deleted.

3. Read both back:

   ```sh
   rabbitmqctl list_user_permissions tally
   ```

   ```text
   vhost	configure	write	read
   /	^(tally-notifications|amq\.gen-.*)$	^(tally-notifications|amq\.gen-.*)$	^(tally-notifications|amq\.gen-.*|nova|neutron|openstack|glance|octavia)$
   ```

   ```sh
   rabbitmqctl list_topic_permissions -p /
   ```

   ```text
   user	exchange	write	read
   tally	glance	^$	^notifications\.info$
   tally	neutron	^$	^notifications\.info$
   tally	nova	^$	^notifications\.info$
   tally	octavia	^$	^notifications\.info$
   tally	openstack	^$	^notifications\.info$
   ```

   Every exchange in the read pattern has a row. One without a row is
   unrestricted, and is the one step 2 has to be repeated for.

## Choose the queue type

The collector declares `tally-notifications` as a quorum queue unless
`TALLY_OSC_QUEUE_TYPE` is `classic`.
[Classic and quorum](/explanation/how-the-collector-consumes-a-bus#classic-and-quorum)
compares the two types.

1. Read the broker version:

   ```sh
   rabbitmqctl version
   ```

   ```text
   4.3.6
   ```

2. On RabbitMQ 4.0 or newer, leave `TALLY_OSC_QUEUE_TYPE` unset. On an older
   broker, or to keep the queue on one node, set it to `classic`:

   ```sh
   export TALLY_OSC_QUEUE_TYPE=classic
   ```

   A collector at the default on an older broker logs
   `TALLY_OSC_QUEUE_TYPE=quorum needs RabbitMQ 4.0 or newer and the broker reports <version>`,
   ending on `set TALLY_OSC_QUEUE_TYPE=classic for this broker`, and consumes
   nothing.

3. Where a collector has run against this broker before, list the queue it
   left:

   ```sh
   rabbitmqctl list_queues name type arguments messages consumers | grep -E '^tally-notifications[[:space:]]'
   ```

   ```text
   tally-notifications	classic	[{"x-queue-type","classic"}]	0	1
   ```

   A collector up to v0.2.0, and one set to `classic`, declared the queue
   without arguments. The broker refuses the default declare over that queue,
   and over a quorum queue whose arguments carry no `x-delivery-limit` of -1,
   which is what a virtual host with `default_queue_type` `quorum` created.
   The collector then logs
   `the queue exists with other arguments than TALLY_OSC_QUEUE_TYPE=quorum declares`
   and consumes nothing, while the queue stays bound and keeps filling.

   Either set `TALLY_OSC_QUEUE_TYPE=classic` before the collector is upgraded,
   which keeps the queue, or move the queue with the steps of
   [move the queue to another type](/how-to/openstack/connect-the-collector#move-the-queue-to-another-type).

## Move the queue to another type

These steps cover the upgrade from a collector up to v0.2.0 to the default,
and any later change of `TALLY_OSC_QUEUE_TYPE`.

1. Keep the collector that fits the existing queue running until the message
   count, the fourth column, is 0. That collector is the older release, or the
   new one with the old setting. Where the collector was already restarted
   with the new setting, set the value back and restart it first:

   ```sh
   rabbitmqctl list_queues name type arguments messages consumers | grep -E '^tally-notifications[[:space:]]'
   ```

   ```text
   tally-notifications	classic	[{"x-queue-type","classic"}]	0	1
   ```

2. Stop the collector and delete the queue:

   ```sh
   rabbitmqctl delete_queue tally-notifications
   ```

3. Start the collector with the new setting. For the default that is the
   upgraded collector with `TALLY_OSC_QUEUE_TYPE` unset.

Notifications published between the stop and the delete are discarded with the
queue, and those published between the delete and the new binding reach no
queue. [Reconcile a cloud](/how-to/openstack/reconcile-a-cloud) finds what
those notifications reported.

The way back is the same sequence. A collector set to `classic`, and a
downgrade to v0.2.0, declares the queue without arguments, and the broker
refuses that declare over the quorum queue. Wait until the message count is 0
with the quorum collector still running, stop it, delete the queue, and start
the other collector.

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

4. Read the collector's queue back. The line below is what the default
   declares, and a queue under `classic` prints `classic` in the type column:

   ```sh
   rabbitmqctl list_queues name type arguments effective_policy_definition consumers | grep -E '^tally-notifications[[:space:]]'
   ```

   ```text
   tally-notifications	quorum	[{"x-queue-type","quorum"},{"x-delivery-limit",-1}]	#{}	1
   ```

   A policy or operator policy that matches `tally-notifications` and sets
   `delivery-limit` to a positive number overrides the argument. The effective
   policy definition, which is `#{}` where no policy matches, must carry no
   `delivery-limit`; exclude `tally-notifications` from such a policy's
   pattern.

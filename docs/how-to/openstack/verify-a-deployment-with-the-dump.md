---
title: Verify a deployment with the dump
description: Print what an OpenStack deployment publishes with the collector's dump mode and compare it against the notification mapping.
quadrant: how-to
audience: operator
---

# Verify a deployment with the dump

Oslo type names and payload members differ per OpenStack release. This guide
prints what one deployment actually publishes, before a collector is pointed at
it, and compares that against the entries the mapping table carries.

## Before you start

- The broker's AMQP URL. The dump reads the AMQP variables alone: no cloud, no
  Reporting API, no token and no outbox
  ([the collector's modes](/reference/command-line/tally-openstack-collector#modes)).
- A project on the cloud where you may boot an instance, create a volume,
  allocate a floating IP, upload an image and create a load balancer, and the
  `openstack` client configured for it.
- `jq`, for reading the JSON lines the dump prints.
- The [collector settings](/reference/configuration/tally-openstack-collector)
  page, which lists the three variables this guide exports.

## Start the dump

1. Export the AMQP variables and start the dump, keeping its lines in a file:

   ```sh
   export TALLY_OSC_AMQP_URL='amqp://user:password@rabbitmq.example:5672/'
   export TALLY_OSC_EXCHANGES=nova,neutron,cinder,glance,octavia
   export TALLY_OSC_TOPICS=notifications.info
   tally-openstack-collector --dump | tee dump.jsonl
   ```

   ```json
   {"exchange":"nova","routing_key":"notifications.info","message_id":"e3d6f0f4-5b2f-4b1a-9a2b-1c3d5e7f9a0b","event_type":"compute.instance.create.end","timestamp":"2026-03-01T10:00:00Z","payload":{"instance_id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","tenant_id":"9c4a1b2d3e4f5061728394a5b6c7d8e9","vcpus":4,"memory_mb":8192,"root_gb":40,"ephemeral_gb":40,"instance_type":"m1.large"}}
   ```

   The dump consumes through a server-named, exclusive, auto-deleting queue, so
   the durable `tally-notifications` queue, Ceilometer and a collector running
   at the same time keep receiving their own copies. An interrupted dump leaves
   no queue behind.

## Perform the billable operations

1. In a second shell, run the operations that are meant to be billed:

   ```sh
   openstack server create --flavor m1.large --image cirros --network private dump-check
   openstack server delete dump-check
   openstack volume create --size 1 dump-check
   openstack volume set --size 2 dump-check
   openstack volume delete dump-check
   openstack floating ip create public
   openstack floating ip delete <address>
   openstack image create --file cirros.img dump-check
   openstack image delete dump-check
   openstack loadbalancer create --vip-subnet-id private-subnet --name dump-check
   openstack loadbalancer delete dump-check
   ```

2. Stop the dump with Ctrl-C once the last operation has been published, and
   count what it collected:

   ```sh
   wc -l < dump.jsonl
   ```

   ```text
   15
   ```

## Compare against the mapping

1. Reduce the file to the event types the deployment published and compare that
   list against the oslo types on the
   [notification mapping](/reference/formats/notification-mapping) page:

   ```sh
   jq -r '.event_type' dump.jsonl | sort | uniq -c
   ```

   ```text
      1 compute.instance.create.end
      1 compute.instance.delete.end
      1 floatingip.create.end
      1 floatingip.delete.end
      1 image.create
      1 image.delete
      1 image.upload
      3 null
      1 octavia.loadbalancer.create.end
      1 octavia.loadbalancer.delete.end
      1 volume.create.end
      1 volume.delete.end
      1 volume.resize.end
   ```

   A line the parser refused carries no `event_type` and counts under `null`.

2. Compare the payload members each entry reads against the payloads printed.
   For an instance those are `instance_id`, `tenant_id`, `vcpus`, `memory_mb`,
   `root_gb`, `ephemeral_gb` and `instance_type`:

   ```sh
   jq -c 'select(.event_type == "compute.instance.create.end") | .payload
     | {instance_id, tenant_id, vcpus, memory_mb, root_gb, ephemeral_gb, instance_type}' dump.jsonl
   ```

   ```json
   {"instance_id":"1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d","tenant_id":"9c4a1b2d3e4f5061728394a5b6c7d8e9","vcpus":4,"memory_mb":8192,"root_gb":40,"ephemeral_gb":40,"instance_type":"m1.large"}
   ```

   A member printed as `null` is one the release names differently, and the
   other resource types read `volume_id`, `size`, `volume_type`,
   `floatingip.id`, `owner`, `loadbalancer_id`, `id`, `project_id`, `listeners`
   and `pools`.

3. Compare the deliveries against the recorded samples under
   [`internal/providers/openstack/testdata/golden/`](https://github.com/B42Labs/tally/tree/main/internal/providers/openstack/testdata/golden/),
   whose `notifications` hold one recorded envelope each and whose `events`
   hold the event each envelope maps to. Where the deployment diverges, edit
   the mapping entry and the fixture pair for that type. A type the table does
   not carry is counted as skipped and recorded nowhere.

## Check the result

1. The dump prints one JSON line per delivery, carrying the exchange, the
   routing key, the message id, the event type, the timestamp and the payload:

   ```sh
   jq -c 'del(.payload)' dump.jsonl | head -2
   ```

   ```json
   {"exchange":"nova","routing_key":"notifications.info","message_id":"e3d6f0f4-5b2f-4b1a-9a2b-1c3d5e7f9a0b","event_type":"compute.instance.create.end","timestamp":"2026-03-01T10:00:00Z"}
   {"exchange":"cinder","routing_key":"notifications.info","message_id":"b7c8d9e0-1f2a-4b3c-8d4e-5f6a7b8c9d0e","event_type":"volume.create.end","timestamp":"2026-03-01T10:04:11Z"}
   ```

2. A body the parser refuses prints under `unparseable`, with the credentials
   an oslo request context carries replaced by `[redacted]` and the rest cut
   off after 512 bytes:

   ```sh
   jq -c 'select(.unparseable) | {exchange, unparseable}' dump.jsonl
   ```

   ```json
   {"exchange":"nova","unparseable":"{\"_context_auth_token\": \"[redacted]\", \"_context_password\": \"[redacted]\", \"event_type\": \"compute.instance.update\", \"payload\": {"}
   ```

3. A body that is not JSON at all, a msgpack-serialized notification for one,
   is reported by its size and not printed:

   ```sh
   jq -r 'select(.unparseable) | .unparseable' dump.jsonl | grep 'bytes that are not JSON'
   ```

   ```text
   2114 bytes that are not JSON
   ```

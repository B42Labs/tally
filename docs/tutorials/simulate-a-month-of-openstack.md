---
title: Simulate a month of OpenStack
description: Start the simulator stack beside the dev cluster, publish one generated month of OpenStack notifications onto its broker, and read the month back through the Reporting API.
quadrant: tutorial
audience: all
---
<!-- Shown output captured on 2026-09-07 from commit d0d5905 with kind v0.32.0, kubectl v1.36.1, Docker Desktop 4.86.0, Go 1.27.1 on macOS 15.7.4. -->

# Simulate a month of OpenStack

In this lesson you start the simulator stack beside the cluster of lesson 1. It
publishes one generated month of OpenStack notifications onto a broker the
collector consumes from. You watch the month arrive in the Reporting API,
finish it at once instead of waiting its pace out, and read it back.

At the end you hold the month of July 2026 of the simulated cloud `os-sim` in
the reporting database, minus the share the simulator keeps back for a later
track.

This lesson takes about 15 minutes.

## Before you start

- The state lesson 1 leaves: the kind cluster from `make up`, `tally-ca.crt` at
  the repository root, `TALLY_REPORTING_DB_URL` and `TALLY_API_TOKEN` in the
  shell, and that shell at the repository root.
- Docker Desktop running, with `docker compose` on the path. The stack of this
  lesson runs beside the cluster, as three containers of its own.

If you closed that shell, restore it with this block:

```sh
export TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
TALLY_API_TOKEN="$(go run ./cmd/tally-reporting-admin create-api-token --role admin --description 'tutorial')"
export TALLY_API_TOKEN
```

A token is printed once, so a closed shell means a new token. The one lesson 1
minted stays valid until it is revoked the way
[issue and revoke credentials](/how-to/openstack/issue-and-revoke-credentials)
says. `make simulator-up` writes `tally-ca.crt` again if the file is missing.

## Start the simulator stack

1. Start the stack for July 2026 with the held-back switch on:

   ```sh
   make simulator-up SIM_PERIOD=2026-07 SIM_FAULTS=held-back
   ```

   The month `2026-07` is the one period this track uses everywhere. The cloud
   `os-sim`, the seed 1 and the pace, factor 744, are the `Makefile` defaults,
   and [run a simulated month](/how-to/simulator/run-a-month) covers the
   variables that change them. `held-back` is the one fault switch this track
   turns on, and the others are in
   [switch on fault switches](/how-to/simulator/switch-on-faults).

   The target first builds the collector and the simulator images, which is
   Docker build output, and prints `==> writing the dev CA to tally-ca.crt`.
   The lines shown here start where it issues the credential:

   ```text
   ==> issuing an ingest credential for os-sim
   created ingest_credentials 0ce1a769-1794-47e5-97c8-6241a087f49a
   the token above is printed this one time: store it now, it will not be shown again
   docker compose -f deploy/compose/compose.yaml up -d
   ```

   The credential is the ingest token the collector reports under. It went into
   `deploy/compose/.env` and is printed nowhere else. The id is your own.

   Compose then pulls the broker image and starts the three containers,
   `rabbitmq`, `collector` and `simulator`, as a progress display the terminal
   redraws in place. These are the last lines:

   ```text
   Simulator stack is up:
     http://127.0.0.1:15672                               broker UI, guest/guest
     http://127.0.0.1:8090/metrics                        collector
     http://127.0.0.1:8091/clock                          simulator control
     http://127.0.0.1:8091/metrics                        simulator inventory, the database exporter stand-in
     https://api.tally.127-0-0-1.nip.io:8443/api/v1       Reporting API
     https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics  OTLP endpoint the series are pushed to
     https://vm.tally.127-0-0-1.nip.io:8443/targets       scrape targets

   Finish the month at once with:
     curl -X PUT -d '{"factor": 0}' http://127.0.0.1:8091/clock
   Release the held-back notifications of a run with SIM_FAULTS=held-back with:
     curl -X POST http://127.0.0.1:8091/release
   Inspect the registry with the admin token in deploy/compose/.env:
     curl --cacert tally-ca.crt -H "Authorization: Bearer $(grep TALLY_SIM_API_TOKEN deploy/compose/.env | cut -d= -f2)" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=os-sim'
   Reconcile the cloud the run serves: docs/how-to/simulator/reconcile-the-simulated-cloud.md
   ```

   The seven URLs have to match. The four hint lines below them are printed on
   every run. This track follows the first of them, the `PUT /clock` of a later
   step, and never the second: the `POST /release` line is what lets the held
   share out, which the billing track does.

   Three containers now run beside the cluster. The month of July 2026 of seed
   1 on the cloud `os-sim`, with its six tenants, goes onto the bus at factor
   744, which puts the 31 days on the bus in an hour. The `held-back` switch
   keeps 84 of the month's 1812 billable notifications off the bus, and the
   simulator holds them until a release.

   `ERROR: set SIM_PERIOD to the past month to simulate, e.g. make simulator-up SIM_PERIOD=2026-07`
   in place of the build output means the period was left off the command. A
   `dial tcp` error from the admin CLI at the credential step means the cluster
   is not up, so run the `make up` of lesson 1 again.

## Watch the clock

1. Read the simulator's clock:

   ```sh
   curl -s http://127.0.0.1:8091/clock
   ```

   ```json
   {"virtual_now":"2026-07-01T07:24:26Z","factor":744,"published":430,"total":15727,"held":84,"holding":false,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
   ```

   A minute later the same call answers:

   ```sh
   curl -s http://127.0.0.1:8091/clock
   ```

   ```json
   {"virtual_now":"2026-07-01T19:48:56Z","factor":744,"published":902,"total":15727,"held":84,"holding":false,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
   ```

   `factor` 744, `total` 15727, `held` 84, `period_from` `2026-07-01T00:00:00Z`
   and `period_to` `2026-08-01T00:00:00Z` have to match. `virtual_now` and
   `published` are your own and grow between the two reads: on the run they
   stood at 430 and then at 902 notifications, twelve virtual hours apart.
   `holding` is false while the month publishes. Connection refused on 8091
   means the simulator is not running, and
   `docker compose -f deploy/compose/compose.yaml ps` shows the three
   containers while
   `docker compose -f deploy/compose/compose.yaml logs simulator` says why it
   stopped.

## Watch the events arrive

1. Count what the Reporting API holds, grouped by cloud and resource type:

   ```sh
   curl --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/stats/resources?group_by=cloud,resource_type'
   ```

   ```json
   {"items":[{"cloud":"os-sim","count":13,"resource_type":"floating_ip"},{"cloud":"os-sim","count":9,"resource_type":"image"},{"cloud":"os-sim","count":15,"resource_type":"instance"},{"cloud":"os-sim","count":2,"resource_type":"loadbalancer"},{"cloud":"os-sim","count":25,"resource_type":"volume"}]}
   ```

   A minute later:

   ```sh
   curl --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/stats/resources?group_by=cloud,resource_type'
   ```

   ```json
   {"items":[{"cloud":"os-sim","count":13,"resource_type":"floating_ip"},{"cloud":"os-sim","count":9,"resource_type":"image"},{"cloud":"os-sim","count":20,"resource_type":"instance"},{"cloud":"os-sim","count":2,"resource_type":"loadbalancer"},{"cloud":"os-sim","count":29,"resource_type":"volume"}]}
   ```

   The counts are your own. They are the resources the collector has delivered
   at the moment of the call, 15 instances on the run and 20 a minute later.
   The five resource types are the month's. An empty `items` list two minutes
   after the start means the collector has delivered nothing yet, which the
   counter read of the step "Wait for the collector to deliver" diagnoses.

2. Read one row of the fleet:

   ```sh
   curl --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/resources?cloud=os-sim&resource_type=instance&limit=1'
   ```

   ```json
   {"items":[{"cloud":"os-sim","created_at":"2026-07-01T03:17:02Z","deleted_at":null,"last_event_at":"2026-07-02T05:36:15Z","last_event_type":"compute.instance.power_off","last_payload":{"provider":{"oslo_event_type":"compute.instance.power_off.end"},"state":"shutoff"},"platform":"openstack","project_id":"d5a8024946ddf673277b9e2490643a2c","resource_id":"079faae9-9d39-426f-a963-769cb12aa629","resource_type":"instance","size":{"disk_gb":20,"flavor":"m1.small","ram_gb":2,"vcpus":1},"state":"shutoff"}],"next_cursor":"WyJvcy1zaW0iLCJpbnN0YW5jZSIsIjA3OWZhYWU5LTlkMzktNDI2Zi1hOTYzLTc2OWNiMTJhYTYyOSJd"}
   ```

   That is one row of the projection. Its `state` is `shutoff`, the state the
   last event left it in. `project_id` names the tenant by id alone, the way
   the simulated cloud names its tenants, and `size` carries `vcpus`, `ram_gb`,
   `disk_gb` and `flavor`. `next_cursor` is the handle for the next page. Which
   row comes first is your own while the month is still arriving.

## Finish the month at once

1. Set the clock's factor to 0:

   ```sh
   curl -X PUT -d '{"factor": 0}' http://127.0.0.1:8091/clock
   ```

   ```json
   {"virtual_now":"2026-07-02T13:14:44Z","factor":0,"published":1174,"total":15727,"held":84,"holding":false,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
   ```

   The route rebases the clock on the virtual instant it has reached and
   answers the clock document. `factor` 0 has to match, and `virtual_now` and
   `published` are your own. A 400 with
   `factor must be a JSON object with a number member "factor" that is zero or positive`
   means the body was not the JSON object shown.

2. Read the clock again once the rest of the month is out:

   ```sh
   curl -s http://127.0.0.1:8091/clock
   ```

   On the run the answer below came 15 seconds after the factor changed. A
   slower machine takes a few minutes.

   ```json
   {"virtual_now":"2026-07-02T13:14:44Z","factor":0,"published":15643,"total":15727,"held":84,"holding":true,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
   ```

   `published` 15643, `held` 84, `holding` true and `total` 15727 have to
   match. `virtual_now` is your own: at factor 0 the clock no longer advances,
   so it stays at the instant the factor became 0 while the rest of the month
   goes out at once. The simulator now waits for a release the billing track
   sends, and this lesson sends none.

## Wait for the collector to deliver

1. Sum the collector's counters over their `event_type` labels:

   ```sh
   curl -s http://127.0.0.1:8090/metrics | awk '
     /^tally_collector_consumed_total/ { c += $2 }
     /^tally_collector_skipped_total/ { s += $2 }
     /^tally_collector_unparseable_total/ { u += $2 }
     /^tally_collector_buffer_depth/ { d = $2 }
     END { print "consumed", c, "skipped", s, "unparseable", u, "depth", d }'
   ```

   ```text
   consumed 1728 skipped 13915 unparseable 0 depth 0
   ```

   Repeat the read until `depth` reads 0, which on the run was three minutes
   after the month went out. The four numbers have to match. 1728 is the
   month's 1812 billable notifications minus the 84 held ones, 13915 is the
   notifications the mapping claims nothing for, and the depth is the outbox
   the collector drains into the Reporting API.

2. Read what the Reporting API took:

   ```sh
   curl -s http://127.0.0.1:8090/metrics | grep ^tally_collector_delivered_total
   ```

   ```text
   tally_collector_delivered_total 1728
   ```

   The 1728 delivered are the 1728 consumed. A depth that does not fall while a
   `tally_collector_delivered_total` stays at 0 means the Reporting API refuses
   the batches, and
   `docker compose -f deploy/compose/compose.yaml logs collector` shows the
   `401` or the `x509` line. The cure is `make simulator-down` and then
   `make simulator-up SIM_PERIOD=2026-07 SIM_FAULTS=held-back` again, which
   issues a fresh credential and writes the CA file again.

## Read the month back

1. Count the whole month, the deleted resources included:

   ```sh
   curl --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/stats/resources?group_by=cloud,resource_type&status=all'
   ```

   ```json
   {"items":[{"cloud":"os-sim","count":16,"resource_type":"floating_ip"},{"cloud":"os-sim","count":9,"resource_type":"image"},{"cloud":"os-sim","count":668,"resource_type":"instance"},{"cloud":"os-sim","count":5,"resource_type":"loadbalancer"},{"cloud":"os-sim","count":169,"resource_type":"volume"}]}
   ```

   Every count has to match. They are the seed's: 16 floating IPs, 9 images,
   668 instances, 5 load balancers and 169 volumes. `status=all` counts the
   deleted resources too, which the default `active` leaves out.

2. Read the first event the month stored:

   ```sh
   curl --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/events?cloud=os-sim&limit=1'
   ```

   ```json
   {"items":[{"cloud":"os-sim","event_id":"7d16f9f3-740a-4670-9898-bc32b30fd3f9","event_type":"image.create","payload":{"provider":{"oslo_event_type":"image.upload"},"size":{"size_gb":3.5},"state":"active"},"platform":"openstack","project_id":"005be5adeef3d87e280d03d9d57c38b4","received_at":"2026-09-07T12:37:37.784207Z","resource_id":"d45cc80a-b7ea-4988-8873-16b54ec1a39d","resource_type":"image","source":"collector","timestamp":"2026-07-01T00:21:32Z"}],"next_cursor":"WyIyMDI2LTA3LTAxVDAwOjIxOjMyWiIsIjdkMTZmOWYzLTc0MGEtNDY3MC05ODk4LWJjMzJiMzBmZDNmOSJd"}
   ```

   It is an `image.create` at `2026-07-01T00:21:32Z` with its `size`.
   `event_id`, `timestamp`, `resource_id`, `project_id` and the payload are the
   seed's and have to match. `received_at` is your own, the wall-clock instant
   the collector delivered the event.

## See the metrics land in the store

1. Count the instances that pushed traffic during the month:

   ```sh
   curl --cacert tally-ca.crt -G 'https://vm.tally.127-0-0-1.nip.io:8443/api/v1/query' --data-urlencode 'query=count(count_over_time(ceilometer_network_outgoing_bytes_total{cloud="os-sim"}[31d]))' --data-urlencode 'time=2026-08-01T00:00:00Z'
   ```

   ```json
   {"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1785542400,"667"]}]},"stats":{"seriesFetched": "667","executionTimeMsec":2}}
   ```

   667 has to match, the instances of the month that pushed traffic. The series
   carry July 2026 timestamps, which is why the query is asked at the end of
   the month with `time` and over a window that spans the month, and why lesson
   4 sets the dashboards' time range to July.

   The plain instant query, without the window and without `time`, is evaluated
   at the wall clock, where the month's series ended a month ago:

   ```sh
   curl --cacert tally-ca.crt -G 'https://vm.tally.127-0-0-1.nip.io:8443/api/v1/query' --data-urlencode 'query=count(ceilometer_network_outgoing_bytes_total{cloud="os-sim"})'
   ```

   ```json
   {"status":"success","data":{"resultType":"vector","result":[]},"stats":{"seriesFetched": "0","executionTimeMsec":0}}
   ```

   An empty answer within 30 seconds of the push is the store's latency offset
   instead, described under
   [check the result](/how-to/observability/fill-the-dashboards#check-the-result)
   of the dashboards guide.

## What you learned

- The month holds three classic projects, two Gardener tenants, one CI tenant
  and one external network:
  [the world and the workload](/explanation/the-simulated-openstack-world#the-world-and-the-workload).
- The `held-back` switch keeps one in 20 of the billable transitions off the
  bus until a release lets them out:
  [the fault switches](/explanation/the-simulated-openstack-world#the-fault-switches).
- The collector consumes the broker, keeps what it read in an outbox and
  delivers it to the Reporting API in batches:
  [at-least-once over an outbox](/explanation/how-the-collector-consumes-a-bus#at-least-once-over-an-outbox).
- The stats and the resource row you read come from the projection the events
  fold into:
  [the projection](/explanation/events-as-the-source-of-truth#the-projection).
- The traffic and the inventory series reach the store over OTLP, timestamped
  in the simulated month:
  [what is pushed](/explanation/the-openstack-metrics-pipeline#what-is-pushed).

## Where to go next

[Meter and rate your first month](/tutorials/meter-and-rate-your-first-month)
turns the month you just ingested into rated usage.

It starts from the state this lesson leaves behind: everything lesson 1 left,
plus the compose stack running with the simulator holding 84 notifications, so
`GET /clock` answers `holding` true, the collector idle with an empty outbox,
and the reporting database holding the month of seed 1 on `os-sim` minus that
held share. The project registry is empty. Nothing of this lesson registers the
six tenants, and a lesson of the billing track does that.

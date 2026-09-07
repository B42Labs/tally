---
title: Fill the dashboards on a dev cluster
description: "Push real events and synthetic exporter series into a dev cluster so that every panel of all four dashboards carries a value."
quadrant: how-to
audience: contributor
---

# Fill the dashboards on a dev cluster

This guide fills all four dashboards on a dev cluster, in two parts. The first
pushes real events through the Reporting API, which moves the `tally_` series
and the projection the variables read. The second pushes the OpenStack exporter
series the resource panels read, which no dev cluster produces, to the OTLP
endpoint.

Two costs come with the second part, both accepted. The synthetic series stay
in the dev store for the 13-month retention VictoriaMetrics runs with, and
nothing ages the points out inside a year; `make down` deletes the cluster and
its volume, and that is the shorter way out than reaching
`/api/v1/admin/tsdb/delete_series` on the pod with the `-deleteAuthKey` the
generated `tally-vm-admin` Secret holds. The labels also claim a cloud, a
project and reconciliation runs that never happened, so running this against a
store any invoice is derived from puts invented usage into the billing record.

## Before you start

- A dev cluster from `make up`, with Grafana published and reachable.
- A shell at the repository root, for `make -s ca`, `go run` and the pushes.
- `tally-ca.crt` written by `make -s ca > tally-ca.crt`, the dev CA that signed
  the Gateway's certificate.
- The OTLP credential the dev overlay generates,
  `tally:tally-dev-otlp-password`.
- The [Grafana dashboards](/reference/observability/dashboards) reference page,
  which states which panel reads which series and which variable.

## Push real events

1. Issue an ingest credential for the cloud the batches report under. Ingest is
   authenticated per (platform, cloud). The command prints the raw token to
   stdout and the id plus the one-time notice to stderr, which is what lets the
   token be captured while the notice stays readable:

   ```sh
   export TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
   TOKEN="$(go run ./cmd/tally-reporting-admin create-ingest-credential \
     --platform openstack --cloud os-prod-eu1)"
   ```

2. Push a small batch for cloud `os-prod-eu1` and project `drill-project`. The
   fields are the canonical event schema of
   [`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md)
   section 4, and each `payload.size` validates against the schema the
   migration chain seeds for its resource type:

   ```sh
   curl --cacert tally-ca.crt -X POST \
     -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     'https://api.tally.127-0-0-1.nip.io:8443/api/v1/events' \
     --data-binary @- <<'EOF'
   [
     {
       "event_id": "drill-instance-create",
       "timestamp": "2026-08-19T09:00:00Z",
       "event_type": "compute.instance.create.end",
       "platform": "openstack",
       "cloud": "os-prod-eu1",
       "resource_type": "instance",
       "resource_id": "drill-instance-1",
       "project_id": "drill-project",
       "source": "collector",
       "payload": {
         "state": "active",
         "size": {"vcpus": 4, "ram_gb": 16, "disk_gb": 80, "flavor": "m1.large"}
       }
     },
     {
       "event_id": "drill-volume-create",
       "timestamp": "2026-08-19T09:05:00Z",
       "event_type": "volume.create.end",
       "platform": "openstack",
       "cloud": "os-prod-eu1",
       "resource_type": "volume",
       "resource_id": "drill-volume-1",
       "project_id": "drill-project",
       "source": "collector",
       "payload": {
         "state": "available",
         "size": {"size_gb": 100, "type": "ssd"}
       }
     }
   ]
   EOF
   ```

   The answer names what the batch did:

   ```json
   {"accepted": 2, "duplicates": 0, "rejected": []}
   ```

   That moves `tally_events_ingested_total` and, once the refresher has run,
   `tally_current_resources`, which is the series both variables are read from.

3. Send the identical batch a second time. Ingestion is idempotent on
   `(event_id, timestamp)`, so nothing is stored and the dedup counter moves:

   ```json
   {"accepted": 0, "duplicates": 2, "rejected": []}
   ```

4. Send one create without `payload.size`, alone. The request itself is
   authorized and readable, so the call is still answered 200; the item is
   refused inside the batch and kept server-side under the reason it was
   refused, which `GET /api/v1/rejected-events` serves:

   ```sh
   curl --cacert tally-ca.crt -X POST \
     -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     'https://api.tally.127-0-0-1.nip.io:8443/api/v1/events' \
     --data-binary @- <<'EOF'
   {
     "event_id": "drill-volume-sizeless",
     "timestamp": "2026-08-19T09:10:00Z",
     "event_type": "volume.create.end",
     "platform": "openstack",
     "cloud": "os-prod-eu1",
     "resource_type": "volume",
     "resource_id": "drill-volume-2",
     "project_id": "drill-project",
     "source": "collector",
     "payload": {"state": "available"}
   }
   EOF
   ```

   ```json
   {
     "accepted": 0,
     "duplicates": 0,
     "rejected": [
       {
         "index": 0,
         "event_id": "drill-volume-sizeless",
         "reason": "schema: payload.size: required on create events"
       }
     ]
   }
   ```

   `tally_events_rejected_total` counts that under the check that refused it,
   so the reason label reads `schema` rather than the full sentence.

5. Send one update on the instance whose timestamp lies before its create. The
   incremental fold cannot place a late event, so the ingest transaction
   replays the resource's projection row from its whole history and
   `tally_projection_replays_total` counts the replay:

   ```sh
   curl --cacert tally-ca.crt -X POST \
     -H "Authorization: Bearer $TOKEN" \
     -H 'Content-Type: application/json' \
     'https://api.tally.127-0-0-1.nip.io:8443/api/v1/events' \
     --data-binary @- <<'EOF'
   {
     "event_id": "drill-instance-late-update",
     "timestamp": "2026-08-19T08:30:00Z",
     "event_type": "compute.instance.update",
     "platform": "openstack",
     "cloud": "os-prod-eu1",
     "resource_type": "instance",
     "resource_id": "drill-instance-1",
     "project_id": "drill-project",
     "source": "collector",
     "payload": {"state": "active"}
   }
   EOF
   ```

   ```json
   {"accepted": 1, "duplicates": 0, "rejected": []}
   ```

   The instance history now starts with an update, which the fold reports as
   the warning `history_starts_without_create` on
   `GET /api/v1/resources/os-prod-eu1/instance/drill-instance-1/lifecycle`.
   That is this guide's doing, not drift.

6. Wait for the counters to reach the store. They arrive on the next scrape of
   the `reporting-api` job, which runs every 30 seconds.
   `tally_current_resources` takes longer: the gauge is re-derived from the
   projection every `TALLY_REPORTING_METRICS_REFRESH_S` seconds, 60 by default,
   and the scrape follows that, so the fleet panels and the two variables fill
   within about 90 seconds of the first batch.

## Push synthetic exporter series

1. Read what the payload below carries before you send it. The project label
   differs per service, and the payload follows that: nova, cinder and glance
   name the owning project `tenant_id`, neutron and octavia name it
   `project_id`, and `openstack_identity_projects` carries no project label at
   all (see the coverage table under
   [coverage against the concept](/explanation/the-openstack-metrics-pipeline#coverage-against-the-concept)).
   The `CLOUD`, `TENANT` and `PROJECT` attribute sets carry that difference.
   The exporter's per-resource labels, `id` and `name` among them, are left
   out: the panels count series and filter on the cloud and the project, so
   nothing reads them. The three `tally_sync_*` series are cumulative monotonic
   sums with two datapoints two minutes apart and growing values, because the
   panels read them through `increase()` and a single point has no slope. Their
   label values are the ones the reconciliation service emits: `completed` or
   `failed` for a run status, and `created`, `updated` or `deleted` for a
   reconciled resource.

2. Push it. The OpenStack panels read the database exporter's series, no dev
   cluster runs that exporter, and this one push writes exactly the series
   those panels read and no others:

   ```sh
   NOW="$(date +%s)000000000"
   THEN="$(( $(date +%s) - 120 ))000000000"
   CLOUD='[{"key":"platform","value":{"stringValue":"openstack"}},{"key":"cloud","value":{"stringValue":"os-prod-eu1"}}]'
   TENANT='[{"key":"platform","value":{"stringValue":"openstack"}},{"key":"cloud","value":{"stringValue":"os-prod-eu1"}},{"key":"tenant_id","value":{"stringValue":"drill-project"}}]'
   PROJECT='[{"key":"platform","value":{"stringValue":"openstack"}},{"key":"cloud","value":{"stringValue":"os-prod-eu1"}},{"key":"project_id","value":{"stringValue":"drill-project"}}]'
   RUNS='[{"key":"cloud","value":{"stringValue":"os-prod-eu1"}},{"key":"status","value":{"stringValue":"completed"}}]'
   RECONCILED='[{"key":"cloud","value":{"stringValue":"os-prod-eu1"}},{"key":"action","value":{"stringValue":"created"}}]'
   ERRORS='[{"key":"cloud","value":{"stringValue":"os-prod-eu1"}}]'

   curl --cacert tally-ca.crt -X POST \
     --user tally:tally-dev-otlp-password \
     -H 'Content-Type: application/json' \
     'https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics' \
     --data-binary @- <<EOF
   {"resourceMetrics":[{"scopeMetrics":[{"metrics":[
   {"name":"openstack_identity_projects","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$CLOUD}]}},
   {"name":"openstack_nova_limits_instances_used","gauge":{"dataPoints":[{"asDouble":3,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
   {"name":"openstack_nova_limits_instances_max","gauge":{"dataPoints":[{"asDouble":10,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
   {"name":"openstack_nova_limits_vcpus_used","gauge":{"dataPoints":[{"asDouble":12,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
   {"name":"openstack_nova_limits_vcpus_max","gauge":{"dataPoints":[{"asDouble":40,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
   {"name":"openstack_nova_limits_memory_used","gauge":{"dataPoints":[{"asDouble":24576,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
   {"name":"openstack_nova_limits_memory_max","gauge":{"dataPoints":[{"asDouble":81920,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
   {"name":"openstack_nova_server_status","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
   {"name":"openstack_cinder_volume_status","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
   {"name":"openstack_cinder_volume_gb","gauge":{"dataPoints":[{"asDouble":100,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
   {"name":"openstack_glance_image_bytes","gauge":{"dataPoints":[{"asDouble":21474836480,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
   {"name":"openstack_neutron_floating_ip","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$PROJECT}]}},
   {"name":"openstack_neutron_router","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$PROJECT}]}},
   {"name":"openstack_loadbalancer_loadbalancer_status","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$PROJECT}]}},
   {"name":"tally_collector_buffer_depth","gauge":{"dataPoints":[{"asDouble":12,"timeUnixNano":"$NOW"}]}},
   {"name":"tally_collector_oldest_buffered_seconds","gauge":{"dataPoints":[{"asDouble":4,"timeUnixNano":"$NOW"}]}},
   {"name":"tally_sync_runs_total","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[
     {"asDouble":4,"timeUnixNano":"$THEN","attributes":$RUNS},
     {"asDouble":5,"timeUnixNano":"$NOW","attributes":$RUNS}]}},
   {"name":"tally_sync_resources_reconciled_total","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[
     {"asDouble":11,"timeUnixNano":"$THEN","attributes":$RECONCILED},
     {"asDouble":13,"timeUnixNano":"$NOW","attributes":$RECONCILED}]}},
   {"name":"tally_sync_errors_total","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[
     {"asDouble":2,"timeUnixNano":"$THEN","attributes":$ERRORS},
     {"asDouble":3,"timeUnixNano":"$NOW","attributes":$ERRORS}]}}
   ]}]}]}
   EOF
   ```

   The push answers 200 with an empty OTLP export response, `{}` or
   `{"partialSuccess":{}}`. Anything the collector rejects comes back as a 4xx
   with the reason in the body.

## Check the result

1. Open `https://grafana.tally.127-0-0-1.nip.io:8443`, go to the folder
   `Tally`, and select `os-prod-eu1` in the Cloud variable. On Tally / Project
   Drilldown select `drill-project` in the Project variable, which the
   `openstack_nova_limits_instances_used` series above supplies. Every panel on
   all four dashboards then carries a value. The panels built on `rate()` and
   `increase()` show this as one spike over their window rather than a level,
   because each series was pushed once.

2. A simulated month fills the same `openstack_*` panels without the second
   part. `make simulator-up SIM_PERIOD=2026-07` pushes the traffic counters and
   the inventory gauges of a generated cloud and serves that inventory to the
   `openstack-db-exporter` job, which
   [run a simulated month](/how-to/simulator/run-a-month) covers. Those series
   carry `cloud="os-sim"` and lie on the simulated period, so the panels and the
   Project variable fill with the Cloud variable on `os-sim` and the time range
   set to that month rather than to the hour the run took. The second part stays
   the way to fill the panels without a run.

3. On the scrape-health stat, `ceilometer` reports `up == 0` while
   `reporting-api` and `otel-collector` report `up == 1`.
   `openstack-db-exporter` reports `up == 0` as well between simulator runs, and
   `up == 1` while one publishes. Both are the designed dev state described
   under [scrape jobs](/reference/observability/metrics#scrape-jobs), not a
   failure.

4. Run the same OTLP push without `--user`. It answers `401` and
   `{"code":16,"message":"no basic auth provided"}`, and writes nothing. That is
   the check that the receivers are not open to whoever resolves the hostname.

5. A panel that is still empty within 30 seconds of a push has lost no point.
   VictoriaMetrics evaluates queries behind wall clock by
   `-search.latencyOffset`, 30 seconds by default, so a query over the window
   still being written comes back empty; the dashboards refresh every minute,
   and the panel fills on one of the next refreshes. The full explanation is
   under
   [check the result](/how-to/observability/publish-metrics-over-otlp#check-the-result)
   of the OTLP guide.

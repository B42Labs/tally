---
title: Grafana dashboards
description: The four provisioned Grafana dashboards, their uids, variables and every panel with the expression it reads.
quadrant: reference
audience: operator
---

# Grafana dashboards

Grafana provisions four dashboard files from
[`deploy/kubernetes/base/grafana/dashboards`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/grafana/dashboards),
which reach the pod through a generated ConfigMap. The provider in
[`provisioning/dashboards/tally.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/grafana/provisioning/dashboards/tally.yaml)
loads them into the folder `Tally` with `disableDeletion: true` and
`allowUiUpdates: false`, so an edit or a delete in the UI is refused and the
files in the repository are what a cluster serves.

Every panel target names the datasource uid `victoriametrics`, which is fixed in
[`provisioning/datasources/vm.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/grafana/provisioning/datasources/vm.yaml)
rather than generated. That datasource points at the vmauth container in the
Grafana pod, on this pod's loopback, which carries the Prometheus read paths on
to the store and refuses everything else.

[`dashboards_test.go`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/grafana/dashboards_test.go)
pins the file set the ConfigMap ships, that every file parses, the uids and the
titles, the datasource uid on every query target, the variables the expressions
read, and the drift note of the reconciliation dashboard.

## Dashboards

A panel with no query target is listed with `none` as its expression.

<!-- refdoc:begin dashboards -->
### `fleet-overview.json`

Title `Tally / Fleet Overview`, uid `tally-fleet-overview`.

| Variable | Multi | Query |
| --- | --- | --- |
| `platform` | yes | `label_values(tally_current_resources, platform)` |
| `cloud` | yes | `label_values(tally_current_resources{platform=~"$platform"}, cloud)` |

| Panel | Type | Expression |
| --- | --- | --- |
| Resources by type and state | `barchart` | `sum by (resource_type, state) (tally_current_resources{platform=~"$platform", cloud=~"$cloud"})` |
| Resource count trend | `timeseries` | `sum by (resource_type, state) (tally_current_resources{platform=~"$platform", cloud=~"$cloud"})` |
| Clouds reporting | `stat` | `count(count by (cloud) (tally_current_resources))` |
| Projects (OpenStack) | `stat` | `sum(openstack_identity_projects{cloud=~"$cloud"})` |
| Top 10 projects by instance count | `table` | `topk(10, sum by (tenant_id) (openstack_nova_limits_instances_used{cloud=~"$cloud"}))` |

### `ingestion-health.json`

Title `Tally / Ingestion Health`, uid `tally-ingestion-health`.

| Variable | Multi | Query |
| --- | --- | --- |
| `platform` | yes | `label_values(tally_current_resources, platform)` |
| `cloud` | yes | `label_values(tally_current_resources{platform=~"$platform"}, cloud)` |

| Panel | Type | Expression |
| --- | --- | --- |
| Event ingest rate | `timeseries` | `sum by (cloud, source) (rate(tally_events_ingested_total{platform=~"$platform", cloud=~"$cloud"}[5m]))` |
| Dedup rate | `timeseries` | `sum by (cloud) (rate(tally_events_deduplicated_total{cloud=~"$cloud"}[5m]))` |
| Rejected events | `timeseries` | `sum by (cloud, reason) (increase(tally_events_rejected_total{cloud=~"$cloud"}[1h]))` |
| Collector buffer depth | `timeseries` | `tally_collector_buffer_depth` |
| Oldest buffered event age | `stat` | `tally_collector_oldest_buffered_seconds` |
| Projection replays | `timeseries` | `sum by (cloud) (rate(tally_projection_replays_total{cloud=~"$cloud"}[15m]))` |
| Scrape health | `stat` | `up{job=~"reporting-api\|openstack-db-exporter\|ceilometer\|otel-collector"}` |

### `project-drilldown.json`

Title `Tally / Project Drilldown`, uid `tally-project-drilldown`.

| Variable | Multi | Query |
| --- | --- | --- |
| `platform` | yes | `label_values(tally_current_resources, platform)` |
| `cloud` | yes | `label_values(tally_current_resources{platform=~"$platform"}, cloud)` |
| `project_id` | no | `label_values(openstack_nova_limits_instances_used, tenant_id)` |
| `api_base` | no | `https://api.tally.127-0-0-1.nip.io:8443` |

| Panel | Type | Expression |
| --- | --- | --- |
| Resources by type | `timeseries` | `count(openstack_nova_server_status{cloud=~"$cloud", tenant_id=~"$project_id"})` |
| Resources by type | `timeseries` | `count(openstack_cinder_volume_status{cloud=~"$cloud", tenant_id=~"$project_id"})` |
| Resources by type | `timeseries` | `count(openstack_neutron_floating_ip{cloud=~"$cloud", project_id=~"$project_id"})` |
| Resources by type | `timeseries` | `count(openstack_neutron_router{cloud=~"$cloud", project_id=~"$project_id"})` |
| Resources by type | `timeseries` | `count(openstack_glance_image_bytes{cloud=~"$cloud", tenant_id=~"$project_id"})` |
| Resources by type | `timeseries` | `count(openstack_loadbalancer_loadbalancer_status{cloud=~"$cloud", project_id=~"$project_id"})` |
| Volume capacity | `timeseries` | `sum(openstack_cinder_volume_gb{cloud=~"$cloud", tenant_id=~"$project_id"})` |
| Quota usage | `gauge` | `sum(openstack_nova_limits_instances_used{cloud=~"$cloud", tenant_id=~"$project_id"}) / sum(openstack_nova_limits_instances_max{cloud=~"$cloud", tenant_id=~"$project_id"})` |
| Quota usage | `gauge` | `sum(openstack_nova_limits_vcpus_used{cloud=~"$cloud", tenant_id=~"$project_id"}) / sum(openstack_nova_limits_vcpus_max{cloud=~"$cloud", tenant_id=~"$project_id"})` |
| Quota usage | `gauge` | `sum(openstack_nova_limits_memory_used{cloud=~"$cloud", tenant_id=~"$project_id"}) / sum(openstack_nova_limits_memory_max{cloud=~"$cloud", tenant_id=~"$project_id"})` |
| Recent lifecycle activity | `timeseries` | `sum by (event_type) (rate(tally_events_ingested_total{cloud=~"$cloud"}[5m]))` |

### `reconciliation-drift.json`

Title `Tally / Reconciliation Drift`, uid `tally-reconciliation-drift`.

| Variable | Multi | Query |
| --- | --- | --- |
| `platform` | yes | `label_values(tally_current_resources, platform)` |
| `cloud` | yes | `label_values(tally_current_resources{platform=~"$platform"}, cloud)` |

| Panel | Type | Expression |
| --- | --- | --- |
| Reconciled resources by action | `timeseries` | `sum by (cloud, action) (increase(tally_sync_resources_reconciled_total{cloud=~"$cloud"}[1h]))` |
| Sync errors | `timeseries` | `sum by (cloud) (increase(tally_sync_errors_total{cloud=~"$cloud"}[1h]))` |
| Sync runs by status | `stat` | `sum by (cloud, status) (increase(tally_sync_runs_total{cloud=~"$cloud"}[6h]))` |
| Drift interpretation | `text` | none |
<!-- refdoc:end dashboards -->

## Variables

Every dashboard carries `platform` and `cloud`. Both are read off
`tally_current_resources`, `cloud` narrowed by whatever `platform` is set to,
and both are multi-select with an `All` option whose value is `.*`.

`project-drilldown.json` carries two more. `project_id` is read off the
`tenant_id` label of `openstack_nova_limits_instances_used`, which the exporter
reports once per project. `api_base` is a textbox rather than a query, and the
panel link of `Recent lifecycle activity` is built from it: it opens
`GET /api/v1/events` for the selected project.

## See also

The [metrics](/reference/observability/metrics) page states every `tally_`
series these panels read. The
[alert rules](/reference/observability/alert-rules) page states what fires on
them.

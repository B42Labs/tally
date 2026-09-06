---
title: Metrics
description: Every Prometheus series the Reporting API and the OpenStack collector expose, by name, type, labels and meaning, and the scrape jobs that read them.
quadrant: reference
audience: operator
---

# Metrics

The Reporting API and the OpenStack collector each serve their instruments in
the Prometheus exposition format at `GET /metrics`, on the port the service
listens on and without a credential. Every series either of them owns is named
`tally_`. The Go runtime and the process collectors are registered beside them,
so a scrape carries the `go_` and `process_` series as well. Those report the
process rather than the product and are not listed below.

`TALLY_METRICS_ENABLED=false` makes the route answer 404 instead of an
exposition. On the Reporting API it also stops the gauge refresher. The two
settings pages,
[Reporting API settings](/reference/configuration/tally-reporting) and
[OpenStack collector settings](/reference/configuration/tally-openstack-collector),
carry the variable with its default.

Both services bound one scrape the same way, in
[`internal/reporting/metrics/metrics.go`](https://github.com/B42Labs/tally/blob/main/internal/reporting/metrics/metrics.go)
and
[`internal/providers/openstack/metrics.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/metrics.go).
Three scrapes are served at once, and a fourth arriving while those three run
is answered 503. A scrape that outlives the budget of 10 seconds is cut off.

## Reporting API

<!-- refdoc:begin reporting-api -->
| Metric | Type | Labels | Help |
| --- | --- | --- | --- |
| `tally_current_resources` | gauge | `platform`, `cloud`, `resource_type`, `state` | Resources the projection holds, by state. |
| `tally_events_deduplicated_total` | counter | `cloud` | Events dropped because the same event was already stored. |
| `tally_events_ingested_total` | counter | `platform`, `cloud`, `resource_type`, `event_type`, `source` | Events stored by the ingest pipeline. |
| `tally_events_rejected_total` | counter | `cloud`, `reason` | Events refused by the ingest pipeline, by the kind of check that refused them. |
| `tally_ingest_unvalidated_size_total` | counter | `platform`, `resource_type` | Events stored with a size no registered schema validated. |
| `tally_projection_replays_total` | counter | `cloud` | Projection rows folded again from a resource's whole event history. |
| `tally_sync_errors_total` | counter | `cloud` | Errors reconciliation runs reported. |
| `tally_sync_resources_reconciled_total` | counter | `cloud`, `action` | Resources a reconciliation run created, updated, or deleted. |
| `tally_sync_runs_total` | counter | `cloud`, `status` | Reconciliation runs that finished, by status. |
<!-- refdoc:end reporting-api -->

The instruments are declared in
[`internal/reporting/metrics/metrics.go`](https://github.com/B42Labs/tally/blob/main/internal/reporting/metrics/metrics.go).

`resource_type`, `event_type` and `state` carry values an ingested event
decides. Each of the three admits 128 distinct values, on a first-seen basis,
and records everything past that under `other`. A value longer than 128
characters is recorded under `other` too.

`reason` on `tally_events_rejected_total` is the check that refused the event,
which is the text of the pipeline's reason before its first colon: `schema`,
`size_schema` or `scope`.

`tally_current_resources` is written by the refresher rather than by the ingest
path. The refresher counts the projection rows per platform, cloud, resource
type and state, and it runs on the interval
`TALLY_REPORTING_METRICS_REFRESH_S` names. A group the projection no longer
holds is deleted from the gauge on the next run, and an empty projection leaves
the gauge without series.

## OpenStack collector

<!-- refdoc:begin collector -->
| Metric | Type | Labels | Help |
| --- | --- | --- | --- |
| `tally_collector_buffer_depth` | gauge | none | Events waiting in the outbox. |
| `tally_collector_consumed_total` | counter | `event_type` | Notifications mapped to an event and buffered. |
| `tally_collector_delivered_total` | counter | none | Events the Reporting API accepted. |
| `tally_collector_delivery_errors_total` | counter | none | Delivery attempts the Reporting API did not accept. |
| `tally_collector_oldest_buffered_seconds` | gauge | none | Age of the oldest event waiting in the outbox, 0 when it is empty, and NaN when the buffer cannot be read. |
| `tally_collector_skipped_total` | counter | `event_type` | Notifications the mapping table produced no event for. |
| `tally_collector_unparseable_total` | counter | none | AMQP deliveries whose body could not be parsed. |
<!-- refdoc:end collector -->

The instruments are declared in
[`internal/providers/openstack/metrics.go`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack/metrics.go).

`event_type` carries the oslo type off the wire. It admits 100 distinct values
and records everything past that under `other`. The bound is the collector's
own; the Reporting API bounds its labels at a different number, and the two do
not track each other.

`tally_collector_delivered_total` counts the events one answer says the
Reporting API accepted, not the events a batch carried. A resent batch is
answered as duplicates and an item the API dead-lettered was refused, so
neither counts here.

`tally_collector_oldest_buffered_seconds` is 0 while the outbox is empty and
NaN while the buffer cannot be read.

## OpenStack simulator

The simulator serves `GET /metrics` on its control listener while a run
publishes. What it exposes is the inventory of the simulated month, in the
shape of the OpenStack database exporter a real cloud runs beside its services,
and none of it is a `tally_` series. The
[inventory endpoint](/reference/command-line/tally-openstack-simulator#the-inventory-endpoint)
section of its page states every one of those.

## Scrape jobs

The store reads these four jobs, from
[`deploy/kubernetes/base/victoriametrics/scrape.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/victoriametrics/scrape.yaml).

<!-- refdoc:begin scrape-jobs -->
| Job | Interval | Timeout | Targets | Static labels |
| --- | --- | --- | --- | --- |
| `reporting-api` | `30s` | none | discovered, role `endpointslice`, kept by `reporting-api;http` | none |
| `openstack-db-exporter` | `300s` | `60s` | `os-db-exporter:9180` | `cloud=os-prod-eu1`, `platform=openstack` |
| `ceilometer` | `60s` | none | `ceilometer-exporter:9101` | `cloud=os-prod-eu1`, `platform=openstack` |
| `otel-collector` | `15s` | none | discovered, role `endpointslice`, kept by `otel-collector;metrics` | none |
<!-- refdoc:end scrape-jobs -->

The static `platform` and `cloud` labels on the two OpenStack jobs put a
third-party exporter's samples in the coordinate system the
[label convention](/reference/formats/label-convention) page states. The two
in-cluster jobs carry no such labels.

The targets and the cloud name of the two OpenStack jobs are placeholders: those
exporters run beside an OpenStack control plane rather than in this cluster. A
deployment replaces the whole file rather than patching it. Its overlay declares
a `configMapGenerator` of its own for `victoriametrics-scrape`, marked
`behavior: replace`, carrying the overlay's `scrape.yaml` with that deployment's
addresses and cloud. The dev overlay's
[`victoriametrics/scrape.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/overlays/dev/victoriametrics/scrape.yaml)
is such a file.

`up` is a series per target, so a job that discovers no target emits no `up`
series at all rather than `up == 0`. A Deployment scaled to zero, a renamed
Service, a renamed port, a removed RoleBinding and an unreachable API server all
take a discovering job off the target list instead of turning it red.

## See also

The [alert rules](/reference/observability/alert-rules) page states which of
these series are watched and what fires on them. The
[Grafana dashboards](/reference/observability/dashboards) page states which
panel reads which of them.

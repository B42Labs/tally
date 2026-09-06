---
title: Alert rules
description: Every alerting and recording rule vmalert evaluates, with its expression, severity, wait and runbook, and the routing Alertmanager ships with.
quadrant: reference
audience: operator
---

# Alert rules

vmalert evaluates
[`deploy/kubernetes/base/vmalert/rules.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/vmalert/rules.yaml)
against the store on the group's interval. An expression that keeps returning a
series for as long as its `for` says is posted to Alertmanager as a firing
alert.

`make check-alerting` loads the rules and the Alertmanager configuration into
the binaries the cluster runs, from the images the manifests pin: the vmalert
image with `-dryRun` over `rules.yaml`, and the Alertmanager image's
`amtool check-config` over `config.yaml`. Docker is its only prerequisite and
no cluster is involved. It is what says the expressions parse.

[`rules_test.go`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/vmalert/rules_test.go)
pins the rest from disk: the alert names and the order they stand in, the
severities, the runbook annotations and the pages they name, the recorded series
the anomaly rule reads, and the scrape jobs the last three rules select.

The `runbook` annotation carries a path in this repository rather than a URL, so
it is read in a checkout rather than followed from Alertmanager. It becomes the
published address once the how-to quadrant carries the runbooks.

## Rules

<!-- refdoc:begin rules -->
Group `tally`, evaluated every `1m`.

### `TallyCloudEventsSilent`

| Property | Value |
| --- | --- |
| Severity | `critical` |
| For | `15m` |
| Runbook | `docs/runbooks/TallyCloudEventsSilent.md` |

Summary:

```text
No collector events from {{ $labels.cloud }} for >1h (was active in last 24h)
```

Expression:

```promql
sum by (cloud) (increase(tally_events_ingested_total{source="collector"}[1h])) == 0
and sum by (cloud) (increase(tally_events_ingested_total{source="collector"}[24h])) > 0
```

### `TallyEventsRejected`

| Property | Value |
| --- | --- |
| Severity | `warning` |
| For | none |
| Runbook | none |

Summary:

```text
{{ $value | printf "%.0f" }} events from {{ $labels.cloud }} rejected in 15m (reason {{ $labels.reason }})
```

Expression:

```promql
sum by (cloud, reason) (increase(tally_events_rejected_total[15m])) > 10
```

### `TallySyncErrors`

| Property | Value |
| --- | --- |
| Severity | `warning` |
| For | none |
| Runbook | none |

Summary:

```text
Reconciliation of {{ $labels.cloud }} reported errors in the last hour
```

Expression:

```promql
sum by (cloud) (increase(tally_sync_errors_total[1h])) > 0
```

### `TallySyncStale`

| Property | Value |
| --- | --- |
| Severity | `critical` |
| For | none |
| Runbook | `docs/runbooks/TallySyncStale.md` |

Summary:

```text
No completed reconciliation run for {{ $labels.cloud }} in 30m
```

Expression:

```promql
sum by (cloud) (increase(tally_sync_runs_total{status="completed"}[30m])) == 0
```

### `TallyReconciliationDriftHigh`

| Property | Value |
| --- | --- |
| Severity | `warning` |
| For | none |
| Runbook | none |

Summary:

```text
Reconciliation corrected {{ $value | printf "%.0f" }} resources in {{ $labels.cloud }} over 6h
```

Expression:

```promql
sum by (cloud) (increase(tally_sync_resources_reconciled_total[6h])) > 50
```

### `TallyCollectorBufferAging`

| Property | Value |
| --- | --- |
| Severity | `warning` |
| For | none |
| Runbook | none |

Summary:

```text
Oldest buffered collector event is {{ $value | printf "%.0f" }}s old; the Reporting API is unreachable from the provider side
```

Expression:

```promql
tally_collector_oldest_buffered_seconds > 600
```

### `tally:current_resources:sum`

Recorded series.

Expression:

```promql
sum by (cloud, resource_type) (tally_current_resources)
```

### `TallyResourceCountAnomaly`

| Property | Value |
| --- | --- |
| Severity | `info` |
| For | `30m` |
| Runbook | none |

Summary:

```text
{{ $labels.resource_type }} count in {{ $labels.cloud }} is above 1.5x its 7-day baseline
```

Expression:

```promql
tally:current_resources:sum
  > 1.5 * (avg_over_time(tally:current_resources:sum[7d] offset 6h) + 10)
```

### `TallyRecordedSeriesMissing`

| Property | Value |
| --- | --- |
| Severity | `warning` |
| For | `15m` |
| Runbook | none |

Summary:

```text
vmalert is not writing tally:current_resources:sum while tally_current_resources is still scraped, so TallyResourceCountAnomaly evaluates nothing
```

Expression:

```promql
absent(tally:current_resources:sum)
  and on () (count(tally_current_resources) > 0)
```

### `TallyScrapeTargetDown`

| Property | Value |
| --- | --- |
| Severity | `critical` |
| For | `5m` |
| Runbook | `docs/runbooks/TallyScrapeTargetDown.md` |

Summary:

```text
Scrape target {{ $labels.instance }} of job {{ $labels.job }} is down
```

Expression:

```promql
up{job=~"reporting-api|openstack-db-exporter|ceilometer|otel-collector"} == 0
```

### `TallyScrapeJobMissing`

| Property | Value |
| --- | --- |
| Severity | `critical` |
| For | `5m` |
| Runbook | `docs/runbooks/TallyScrapeJobMissing.md` |

Summary:

```text
Scrape job {{ $labels.job }} resolves to no targets
```

Expression:

```promql
absent(up{job="reporting-api"}) or absent(up{job="otel-collector"})
```

### `TallyExporterServiceSilent`

| Property | Value |
| --- | --- |
| Severity | `critical` |
| For | `15m` |
| Runbook | `docs/runbooks/TallyExporterServiceSilent.md` |

Summary:

```text
Database exporter for {{ $labels.cloud }} is up but a whole service emits no series
```

Expression:

```promql
(up{job="openstack-db-exporter"} == 1) unless on (cloud, instance) openstack_nova_total_vms{job="openstack-db-exporter"}
or (up{job="openstack-db-exporter"} == 1) unless on (cloud, instance) openstack_cinder_volumes{job="openstack-db-exporter"}
or (up{job="openstack-db-exporter"} == 1) unless on (cloud, instance) openstack_neutron_floating_ips{job="openstack-db-exporter"}
or (up{job="openstack-db-exporter"} == 1) unless on (cloud, instance) openstack_glance_images{job="openstack-db-exporter"}
or (up{job="openstack-db-exporter"} == 1) unless on (cloud, instance) openstack_loadbalancer_total_loadbalancers{job="openstack-db-exporter"}
```
<!-- refdoc:end rules -->

## The recorded series

The vmalert Deployment in
[`deploy/kubernetes/base/vmalert/vmalert.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/vmalert/vmalert.yaml)
takes `-datasource.url` and `-remoteWrite.url`, both
`http://victoriametrics:8428`. It reads what a rule selects through the first
and writes what a rule records through the second, so a recorded series is
queryable only once the write path has carried it back into the store.

`TallyResourceCountAnomaly` reads `tally:current_resources:sum` on both sides of
its comparison, so the whole rule sits behind that pair of flags.
`TallyRecordedSeriesMissing` watches the pair: it fires while
`tally:current_resources:sum` is absent and `tally_current_resources` is still
being scraped.

## Routing

Alertmanager routes a fired alert by
[`deploy/kubernetes/base/alertmanager/config.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/alertmanager/config.yaml).
The child route inherits the receiver from its parent and tightens the repeat
interval alone.

<!-- refdoc:begin routing -->
| Setting | Value |
| --- | --- |
| Receiver | `default` |
| Group by | `alertname`, `cloud` |
| Group wait | `30s` |
| Group interval | `5m` |
| Repeat interval | `4h` |

| Matchers | Overrides |
| --- | --- |
| `severity="critical"` | `repeat_interval: 1h` |

The receivers are `default`; none carries an integration.
<!-- refdoc:end routing -->

An alert reaching a receiver that names no integration is grouped and held in
Alertmanager, where the UI and `amtool` show it and nothing else does.

A deployment adds its own delivery by replacing the generated ConfigMap from its
overlay rather than by patching the base. The overlay declares a
`configMapGenerator` of its own for `alertmanager-config`, marked
`behavior: replace`, carrying a `config.yaml` that names `webhook_configs` or
`email_configs` under the receiver.

## See also

The [metrics](/reference/observability/metrics) page states every series these
rules read. The [Grafana dashboards](/reference/observability/dashboards) page
states which panel reads which of them.

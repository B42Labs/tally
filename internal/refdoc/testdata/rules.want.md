Group `fixture`, evaluated every `1m`.

### `FixtureCloudSilent`

| Property | Value |
| --- | --- |
| Severity | `critical` |
| For | `15m` |
| Runbook | `docs/runbooks/FixtureCloudSilent.md` |

Summary:

```text
No collector events from {{ $labels.cloud }} for >1h
```

Expression:

```promql
sum by (cloud) (increase(tally_events_ingested_total{source="collector"}[1h])) == 0
and sum by (cloud) (increase(tally_events_ingested_total{source="collector"}[24h])) > 0
```

### `FixtureSyncStale`

| Property | Value |
| --- | --- |
| Severity | `warning` |
| For | none |
| Runbook | none |

Summary:

```text
No completed reconciliation run for {{ $labels.cloud }} in 30m
```

Expression:

```promql
sum by (cloud) (increase(tally_sync_runs_total{status="completed"}[30m])) == 0
```

### `fixture:current_resources:sum`

Recorded series.

Expression:

```promql
sum by (cloud, resource_type) (tally_current_resources)
```

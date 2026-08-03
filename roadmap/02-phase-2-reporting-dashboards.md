# 02 – Phase 2: Reporting & Dashboards

> Prerequisites: Phase 1 complete (events flowing, `/metrics` exposed, projection healthy).
> Read [00-conventions.md](00-conventions.md) first.

## Goal

Make the collected data **visible and operable**: aggregation endpoints on the Reporting API,
Grafana dashboards over VictoriaMetrics, and alerting that catches exactly the failure modes
the concept declares billing-critical (collector outage, schema drift, reconciliation drift).

**Out of scope**: anything monetary (Phase 3), new providers (Phase 4).

## Decisions made by this document

| # | Decision | Rationale |
|---|---|---|
| D1 | The concept's Phase-2 items "lifecycle queries" and "`/metrics` endpoint" were already pulled into Phase 1 (WP 1.8, WP 1.11) — Phase 2 only *extends* them | Ingestion health had to be observable from day one |
| D2 | Alerting stack = **vmalert + Alertmanager** (VictoriaMetrics-native) | No Prometheus server exists in this architecture |
| D3 | Dashboards are **provisioned as code** (JSON in the repo); no hand-edited dashboards | Reproducible deployments |
| D4 | "Anomaly detection" (concept) = threshold + baseline-comparison PromQL rules, no ML | Right-sized; ML anomaly detection is not a billing requirement |
| D5 | Aggregation endpoints compute from `events`/`current_resources` (SQL), not from VictoriaMetrics | The API's truth is the event store; VM aggregations belong on dashboards |

---

## Work packages

### WP 2.1 – Reporting API: aggregation & stats endpoints

**Create** stats handlers in `internal/reporting/httpapi` + `internal/reporting/stats`
service module (SQL against `events` / `current_resources`).

```
GET /api/v1/stats/resources?group_by=cloud,resource_type[,state|platform|project_id]&at=<ts>
    → counts from the projection (at=now) or — when `at` is historic — from a timeline
      replay is NOT attempted; historic `at` returns 501 in this phase (documented; the
      engine's usage records answer historic questions in Phase 3)
    → { "items": [ {"cloud": "...", "resource_type": "...", "count": 42}, ... ] }

GET /api/v1/stats/events?group_by=cloud,event_type[,source]&from=&to=&interval=1h|1d
    → time-bucketed event counts (TimescaleDB time_bucket)
    → { "items": [ {"bucket": "2026-07-01T00:00:00Z", "cloud": "...",
                     "event_type": "...", "count": 12}, ... ] }

GET /api/v1/projects/{id}/summary?from=&to=
    → per resource_type within the window, derived from the event history via
      internal/core/timeline (clipped to [from, to)):
      { "project": {...}, "resource_types": [
          { "resource_type": "instance",
            "active_now": 5, "created": 2, "deleted": 1,
            "total_minutes": 216000 } ] }
```

- All endpoints respect QueryAuth/RBAC exactly like Phase-1 query endpoints
  (`project` role → own projects only).
- `stats/events` must use `time_bucket()` and hit `idx_events_type` — add an
  `EXPLAIN`-based regression test that the plan does not seq-scan the hypertable for
  bounded windows.

**Tests**: aggregation correctness against a seeded event fixture; RBAC; interval bucketing
across month boundaries; 501 for historic `at`.

---

### WP 2.2 – Grafana deployment & provisioning

**Create** `deploy/kubernetes/base/grafana/`:

```
grafana/
├── provisioning/
│   ├── datasources/vm.yaml        # type: prometheus, url: http://victoriametrics:8428
│   └── dashboards/tally.yaml      # file provider → /var/lib/grafana/dashboards
└── dashboards/
    ├── fleet-overview.json
    ├── project-drilldown.json
    ├── ingestion-health.json
    └── reconciliation-drift.json
```

Add a `grafana` component to the kustomize base: Deployment (image `grafana/grafana`),
provisioning tree and dashboard JSON mounted from ConfigMaps (`configMapGenerator` — editing
a dashboard file re-rolls the pod), admin password from a Secret, Service, and an
`HTTPRoute` on the existing Gateway — dev hostname `grafana.tally.127-0-0-1.nip.io`,
`GF_SERVER_ROOT_URL` derived from the route hostname. In dev that root URL has to carry the
`:8443` host port (`https://grafana.tally.127-0-0-1.nip.io:8443`), or every absolute link and
redirect Grafana emits points at a closed port. The dev overlay enables anonymous
viewer access. No new host ports and no cluster changes — the route is live as soon as it
is applied. The same base deploys unchanged to a real cluster via the prod overlay.

**Acceptance criteria**: `make up` → `https://grafana.tally.127-0-0-1.nip.io:8443` shows all
four dashboards with data from the dev cluster.

---

### WP 2.3 – Dashboards (panel specifications)

Every panel query below is the contract; the JSON files implement them. All dashboards have
template variables `$platform`, `$cloud` (multi-select, sourced from
`label_values(tally_current_resources, cloud)`).

**1. Fleet overview** (`fleet-overview.json`)

| Panel | Type | Query |
|---|---|---|
| Resources by type & state | stacked bar | `sum by (resource_type, state) (tally_current_resources{platform=~"$platform", cloud=~"$cloud"})` |
| Resource count trend | time series | same, over dashboard range |
| Clouds reporting | stat | `count(count by (cloud) (tally_current_resources))` |
| Projects with resources (OpenStack) | stat | `openstack_keystone_projects_total{cloud=~"$cloud"}` |
| Top 10 projects by instance count | table | `topk(10, sum by (project_id) (openstack_nova_instances{cloud=~"$cloud"}))` |

**2. Project drilldown** (`project-drilldown.json`, extra variable `$project_id`)

| Panel | Type | Query |
|---|---|---|
| Resources by type | time series | `sum by (resource_type, state) (tally_current_resources{...})` filtered per project via exporter metrics (`openstack_nova_instances{project_id="$project_id"}` etc.) |
| Volume capacity | time series | `sum(openstack_cinder_volume_size_gb{project_id="$project_id", cloud=~"$cloud"})` |
| Quota usage | gauge | `openstack_nova_instances{project_id="$project_id"} / openstack_nova_quota_instances{project_id="$project_id"}` (analog cores/RAM) |
| Recent lifecycle events | table | via `tally_events_ingested_total` rate by `event_type` (project-level event *content* comes from the API, not VM — link panel to `GET /api/v1/events?project_id=…`) |

**3. Ingestion health** (`ingestion-health.json`) — the operationally critical one

| Panel | Type | Query |
|---|---|---|
| Event ingest rate | time series | `sum by (cloud, source) (rate(tally_events_ingested_total[5m]))` |
| Dedup rate | time series | `sum by (cloud) (rate(tally_events_deduplicated_total[5m]))` |
| Rejected events | time series + stat | `sum by (cloud, reason) (increase(tally_events_rejected_total[1h]))` |
| Collector buffer depth | time series | `tally_collector_buffer_depth` |
| Oldest buffered event age | stat (thresholds 60s/600s) | `tally_collector_oldest_buffered_seconds` |
| Projection replays | time series | `sum by (cloud) (rate(tally_projection_replays_total[15m]))` |
| API up / scrape health | stat | `up{job=~"reporting-api|openstack-db-exporter|ceilometer"}` |

**4. Reconciliation drift** (`reconciliation-drift.json`)

| Panel | Type | Query |
|---|---|---|
| Reconciled resources by action | time series | `sum by (cloud, action) (increase(tally_sync_resources_reconciled_total[1h]))` |
| Sync errors | time series | `sum by (cloud) (increase(tally_sync_errors_total[1h]))` |
| Sync runs status | stat | `sum by (cloud, status) (increase(tally_sync_runs_total[6h]))` |
| Drift interpretation note | text panel | "Non-zero `created`/`deleted` here = collectors missed events → investigate before period finalization" |

---

### WP 2.4 – Alerting (vmalert + Alertmanager)

**Create** `deploy/kubernetes/base/vmalert/rules.yaml`,
`deploy/kubernetes/base/alertmanager/config.yaml`; add both as components to the kustomize
base — Deployment + config ConfigMap + Service each (`vmalert` flags:
`-datasource.url=http://victoriametrics:8428
-notifier.url=http://alertmanager:9093 -rule=/etc/vmalert/rules.yaml` — the in-cluster
Service names resolve as-is). Both UIs get `HTTPRoute`s on the existing Gateway for
debugging: `vmalert.tally.127-0-0-1.nip.io`, `alertmanager.tally.127-0-0-1.nip.io`.

Rules (`groups: [{name: tally, interval: 1m, rules: [...]}]`):

```yaml
# 1. Collector gone silent — THE billing-critical alert (concept §3.2 known limitations)
- alert: TallyCloudEventsSilent
  expr: |
    sum by (cloud) (increase(tally_events_ingested_total{source="collector"}[1h])) == 0
    and sum by (cloud) (increase(tally_events_ingested_total{source="collector"}[24h])) > 0
  for: 15m
  labels: { severity: critical }
  annotations:
    summary: "No collector events from {{ $labels.cloud }} for >1h (was active in last 24h)"

# 2. Schema drift → billing data loss in the making
- alert: TallyEventsRejected
  expr: sum by (cloud, reason) (increase(tally_events_rejected_total[15m])) > 10
  labels: { severity: warning }

# 3. Reconciliation failing → drift accumulates unnoticed
- alert: TallySyncErrors
  expr: sum by (cloud) (increase(tally_sync_errors_total[1h])) > 0
  labels: { severity: warning }

- alert: TallySyncStale            # no successful sync in 30 min (expected cadence: 10 min)
  expr: sum by (cloud) (increase(tally_sync_runs_total{status="completed"}[30m])) == 0
  labels: { severity: critical }

# 4. High drift = collectors missing events even though sync papers over it
- alert: TallyReconciliationDriftHigh
  expr: sum by (cloud) (increase(tally_sync_resources_reconciled_total[6h])) > 50
  labels: { severity: warning }

# 5. Collector buffer growing / aging (Reporting API unreachable from the provider side)
- alert: TallyCollectorBufferAging
  expr: tally_collector_oldest_buffered_seconds > 600
  labels: { severity: warning }

# 6. Resource-count anomaly ("sudden resource spikes" per concept)
- alert: TallyResourceCountAnomaly
  expr: |
    sum by (cloud, resource_type) (tally_current_resources)
      > 1.5 * (avg_over_time(sum by (cloud, resource_type) (tally_current_resources)[7d:1h]) + 10)
  for: 30m
  labels: { severity: info }

# 7. Scrape targets down
- alert: TallyScrapeTargetDown
  expr: up{job=~"reporting-api|openstack-db-exporter|otel-collector"} == 0
  for: 5m
  labels: { severity: critical }
```

Alertmanager: single default receiver (webhook/email placeholder) + route `severity=critical`
with tighter repeat interval; receiver endpoints are deployment-specific (env-substituted).

**Tests / verification**: `vmalert -dryRun` on the rules file in CI; a scripted drill
(`docs/drills/phase2.md`) that scales the collector to zero on the dev cluster
(`kubectl -n tally scale deployment/... --replicas=0`) and observes
`TallyCloudEventsSilent` firing.

---

## Phase exit criteria

1. Four dashboards provisioned from the repo, rendering on the dev cluster.
2. All vmalert rules load (`-dryRun` in CI); the collector-silent drill fires and resolves.
3. Aggregation endpoints documented in OpenAPI and covered by RBAC tests.
4. Runbook stubs in `docs/runbooks/` for the three critical alerts (`TallyCloudEventsSilent`,
   `TallySyncStale`, `TallyScrapeTargetDown`) — symptom, impact on billing, first checks.

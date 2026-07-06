# 04 – Phase 4: Additional Providers & Services

> Prerequisites: Phase 1 (core + provider pattern proven with OpenStack). Pricing for the new
> providers becomes billable once Phase 3 is live, but collectors/exporters/adapters can be
> built and run against Phase 1 alone.
> Read [00-conventions.md](00-conventions.md) first.

## Goal

Onboard **Hetzner Cloud, STACKIT, IONOS** as platform providers and **Gardener, Harbor** as
service integrations — each following the provider checklist from the concept (§3.1). Every
provider is independent: they can be implemented in any order, by different implementers, and
each has its own exit checklist.

## Decisions made by this document

| # | Decision | Rationale |
|---|---|---|
| D1 | Before the first new provider: extract the generic collector runtime (outbox, sender, health, metrics) from `tally_openstack_collector` into `libs/tally-collector` | Five providers must not copy-paste buffering/delivery code |
| D2 | Polling collectors (Hetzner/STACKIT/IONOS) persist their **cursor in the same SQLite DB as the outbox**, updated in the same transaction as the outbox insert | Restart-safe exactly-once *mapping* (delivery stays at-least-once; `event_id` dedup covers the rest) |
| D3 | STACKIT and IONOS event-API specifics are marked `VERIFY` — the WP starts with a documented API verification step | Concept relies on audit/activity APIs whose exact shape must be confirmed against current vendor docs |
| D4 | Gardener collector = Kubernetes watch on `Shoot` resources (party: `kubernetes` Python client), cursor = `resourceVersion` | Gardener has no message bus to consume; the API server watch is the native event source |
| D5 | Harbor collector = **webhook receiver** (push/delete) + periodic storage poll; pulls are counted from webhook events | Harbor natively pushes webhooks; polling alone would miss short-lived tags |
| D6 | Every provider ships golden mapping fixtures + must pass the `tally_core.testing` conformance kit before its first deploy | Uniform quality gate |

---

## WP 4.0 – Shared collector runtime `libs/tally-collector`

**Create** package `tally_collector`; refactor `tally_openstack_collector` onto it (behavior
unchanged — its Phase-1 tests must stay green).

Components:

- `Outbox` — SQLite (WAL) queue as specified in Phase 1 WP 1.12, plus a `cursors` table:
  `cursors(name TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at TEXT NOT NULL)`.
- `Sender` — batch delivery loop (≤500, id order, exponential backoff 1 s→300 s + jitter,
  delete on HTTP 200) against `POST /api/v1/events`.
- `CollectorApp` — wiring: config (env prefix per provider), structlog, `/healthz`, `/metrics`
  (`tally_collector_*` metric family from WP 1.12), graceful shutdown (flush, close SQLite).
- `PollingSource` base for cursor-based collectors:

  ```python
  class PollingSource(Protocol):
      cursor_name: str
      async def poll(self, cursor: str | None) -> tuple[list[Event], str | None]:
          """Return (mapped events, new cursor). Runtime persists outbox rows and the
          cursor in ONE SQLite transaction, then sleeps poll_interval."""
  ```

**Acceptance**: OpenStack collector runs on the shared runtime; conformance kit green;
kill/restart tests (buffer + cursor survive) pass at the library level.

---

## WP 4.1 – Hetzner Cloud provider

Package `providers/hetzner/` → `tally_hetzner_collector`, `tally_hetzner_exporter`; adapter
`reconciliation/adapters/hetzner.py`. API: `https://api.hetzner.cloud/v1`, token auth.

### Resource types & size schemas (register via `PUT /api/v1/resource-types/hetzner/...`)

```json
// server
{ "type": "object", "required": ["vcpus", "ram_gb", "disk_gb", "server_type"],
  "properties": { "vcpus": {"type": "integer", "minimum": 1},
                  "ram_gb": {"type": "number"}, "disk_gb": {"type": "number"},
                  "server_type": {"type": "string"} }, "additionalProperties": true }
// volume
{ "type": "object", "required": ["size_gb"],
  "properties": { "size_gb": {"type": "number", "exclusiveMinimum": 0} },
  "additionalProperties": true }
// floating_ip
{ "type": "object", "required": ["ip_version", "type"],
  "properties": { "ip_version": {"enum": [4, 6]}, "type": {"type": "string"} },
  "additionalProperties": true }
// load_balancer
{ "type": "object", "required": ["type"],
  "properties": { "type": {"type": "string"}, "targets": {"type": "integer", "minimum": 0} },
  "additionalProperties": true }
```

### Event collector (PollingSource on the Actions API)

- Poll `GET /actions?sort=id:asc` (per-resource-kind action endpoints where the global one
  doesn't cover them — verify against current API docs), cursor = last processed action `id`.
- `event_id` = `hetzner-action-{action.id}` (the concept's requirement: Hetzner action ID).
- `project_id`: Hetzner has projects at the API-token level, not in resources — the collector
  is configured with `TALLY_HC_PROJECT_ID` (one collector instance per Hetzner project;
  document this in the provider README).
- Mapping table (`mapping.py`, data-driven like OpenStack):

| Hetzner action | tally event_type | payload |
|---|---|---|
| `create_server` | `server.create` | state from server status (`running`/`off`), size from server_type catalog (`GET /server_types`, cached): `{vcpus: cores, ram_gb: memory, disk_gb: disk, server_type: name}` |
| `delete_server` | `server.delete` | — |
| `change_server_type` | `server.resize` | new size from server_type |
| `start_server` / `stop_server` / `shutdown_server` | `server.power_on` / `server.power_off` | state `running` / `off` |
| `create_volume` / `delete_volume` / `resize_volume` | `volume.create` / `volume.delete` / `volume.resize` | `{size_gb: size}` |
| `assign_floating_ip` etc. lifecycle | `floating_ip.create` / `floating_ip.delete` | `{ip_version, type}` |
| `create_load_balancer` / `delete_load_balancer` / target changes | `load_balancer.create` / `.delete` / `load_balancer.target.update` | `{type, targets: n}` |

- Action timestamps: use `action.finished` (fall back to `started`).
- Enrichment calls (`GET /servers/{id}`) may 404 for fast-deleted resources → emit the event
  with the last known size from a local cache; on cache miss, emit without `size` (UPDATE
  category) and let reconciliation fill in — never drop the event.

### Metrics exporter

Poll `GET /servers`, `/volumes`, `/floating_ips`, `/load_balancers` every 60 s → Prometheus
exposition with mandatory labels (`platform="hetzner"`, `cloud`, `resource_type`,
`project_id`, `resource_id`):

```
hetzner_server_info{..., server_type="cx41", status="running"} 1
hetzner_server_vcpus{...} 4          hetzner_server_ram_gb{...} 16
hetzner_volume_size_gb{...} 100      hetzner_floating_ip_info{...} 1
hetzner_load_balancer_info{..., type="lb11"} 1
```

### Reconciliation adapter

`list_resources`: same four listings → `ObservedResource` (size via server_types cache;
`created` timestamps from the API). Hetzner does not list deleted resources → deletes are
detected by absence (poll-time timestamps; concept's known limitation).

### Pricing & scrape config

Add `hetzner:` section to the pricing model (concept §3.4 values as the starting point) and a
scrape job with `labels: {platform: "hetzner", cloud: "<cloud>"}`.

**Exit checklist WP 4.1**: conformance kit green on golden action fixtures; buffer/cursor
restart drill; recon drill (delete a server while collector is stopped → `sync.delete`);
metering golden test: upgrade cx21→cx31 on 03-15 ⇒ minutes 20160/24480 (concept Ex. 2).

---

## WP 4.2 – STACKIT provider

Package `providers/stackit/`. **Step 0 (`VERIFY`, D3)**: confirm against current STACKIT docs —
(a) which audit/activity API exposes resource lifecycle events, its pagination + retention;
(b) whether compute/volume APIs are OpenStack-compatible enough to reuse the OpenStack
exporter/adapter code paths. Write the findings to `providers/stackit/README.md` **before**
implementing; adjust the mapping below accordingly.

- Resource types & schemas: `server {vcpus, ram_gb, disk_gb, machine_type}`,
  `volume {size_gb, type}`, `database {type, flavor, storage_gb}`,
  `kubernetes_cluster {node_count, machine_type}` (JSON Schemas analogous to WP 4.1).
- Event collector: `PollingSource` over the verified activity API; cursor = event id or
  timestamp watermark (API-dependent); `event_id` = native audit-record id, else
  `deterministic_event_id()`. Tally event types: `server.create/.delete/.resize`,
  `server.power_on/.power_off`, `volume.*`, `database.*`, `kubernetes_cluster.create/.delete/
  .scale`.
- Exporter + reconciliation adapter: poll the resource APIs (reuse OpenStack code where
  Step 0 confirmed compatibility, e.g. via openstacksdk pointed at STACKIT endpoints).
- Pricing section `stackit:` per concept §3.4; scrape job with static labels.

**Exit checklist**: Step-0 verification doc committed; then same gates as WP 4.1 (conformance,
restart drill, recon drill, one metering golden: resize mid-month).

---

## WP 4.3 – IONOS Cloud provider

Package `providers/ionos/`. **Step 0 (`VERIFY`, D3)**: confirm the request/audit API
(`https://api.ionos.com/docs/cloud/v6/` — requests endpoint) for lifecycle tracking: filtering,
ordering, retention, and whether request status exposes completion timestamps.

- Resource types & schemas: `server {cores, ram_gb, type}`, `volume {size_gb, type, bus}`,
  `nic {lan_id, firewall_active}`, `managed_kubernetes {node_count, cores, ram_gb}`.
  Note: IONOS nests resources under datacenters — `resource_id` = `{datacenter_id}/{id}`
  to keep ids unique per cloud.
- Event collector: `PollingSource` over the requests API; cursor = request `createdDate`
  watermark + seen-id set for the boundary instant; `event_id` = request UUID. Tally event
  types: `server.create/.delete/.resize`, `volume.*`, `nic.create/.delete`,
  `managed_kubernetes.create/.delete/.scale`.
- Exporter + reconciliation adapter: enumerate datacenters → servers/volumes/nics; k8s clusters
  via the managed-k8s API.
- Pricing section `ionos:` (dimensions `cores`, `ram_gb` per concept); scrape job.

**Exit checklist**: as WP 4.2.

---

## WP 4.4 – Gardener integration (service provider + cross-platform relations)

Package `providers/gardener/` → `tally_gardener_collector`, `tally_gardener_exporter`.

### Event collector (D4)

- Watch `shoots.core.gardener.cloud/v1beta1` across configured namespaces (kubeconfig via
  `TALLY_GD_KUBECONFIG`); cursor = last `resourceVersion` (persisted via WP 4.0); on watch
  expiry (410 Gone) → relist and reconcile-diff against a local shoot snapshot (also in
  SQLite) so no transition is lost.
- Derive events from spec/status transitions:

| Transition | tally event_type | payload |
|---|---|---|
| Shoot added | `shoot.create.end` | state `active`, size `{worker_count, machine_type, kubernetes_version}` (sum over worker pools; `machine_type` of the primary pool) |
| Shoot deleted | `shoot.delete.end` | — |
| `spec.hibernation.enabled` false→true / true→false | `shoot.hibernate.start` / `shoot.hibernate.end` | state `hibernated` / `active` |
| worker pool min/max/actual count change | `shoot.worker.scale` | new size |
| machine type change | `shoot.worker.machine_type_change` | new size |
| k8s version change | `shoot.kubernetes.upgrade` | new size (not billable; kept for lifecycle) |

- `event_id` = `deterministic_event_id()` (Kubernetes has no native event id usable here);
  `project_id` = Gardener project name; `resource_id` = `{namespace}/{shoot_name}`.

### Project auto-registration (concept §5.4 — implemented via the Phase-1 ingestion hook)

On `shoot.create.end` the collector (not the core) additionally calls the registry API:

1. `POST /api/v1/projects` for the infrastructure tenant
   (`platform` = shoot's infra provider, `cloud` = the infra cloud from collector config
   mapping, `external_id` = the shoot's technical tenant/project id, name
   `"Infrastructure tenant for {shoot}"`) — 409 (already exists) is fine.
2. `POST /api/v1/projects/{gardener-project-uuid}/relations`
   `{target_id, relation_type: "infrastructure_tenant", metadata: {shoot_name, created_by:
   "tally-gardener-collector"}}` — 409 (active relation exists) is fine.
3. On `shoot.delete.end`: `DELETE .../relations/{id}` (closes with `valid_to`; nothing is ever
   hard-deleted — March costs stay attributable, per the concept).

The collector therefore needs an **api token (role `admin`)** in addition to its ingest token —
scoped setup documented in the provider README.

### Exporter

From the watch snapshot: `gardener_shoot_info{project, name, kubernetes_version,
infrastructure}`, `gardener_shoot_worker_count{project, name, pool}`,
`gardener_shoot_worker_machine_type{...}`, `gardener_shoot_status{...}` — plus the mandatory
tally labels (`platform="gardener"`, `cloud`, `resource_type="shoot"`, `project_id`,
`resource_id`).

### Reconciliation adapter

List all shoots via the API server → `ObservedResource` (state from hibernation status; size as
above). Catches watch gaps beyond the relist logic.

**Exit checklist**: conformance kit; hibernation golden test (concept Ex. 4 — 4 splits:
15840/18720/4320/5760, hibernated dims 0.00 with `state_modifiers.hibernated: 0.0`);
auto-registration drill: shoot create → project + relation exist; shoot delete → relation
closed, March statement still attributes worker costs (Phase-3 related-costs golden re-run
with relation closed in April).

---

## WP 4.5 – Harbor integration (counter-based usage)

Package `providers/harbor/` → `tally_harbor_collector` (webhook receiver + poller),
`tally_harbor_exporter`.

### Event collector (D5 — hybrid)

- **Webhook receiver** (FastAPI, `POST /webhook`, secret-token auth): Harbor project webhooks
  for `PUSH_ARTIFACT`, `DELETE_ARTIFACT`, `PULL_ARTIFACT`. Mapping:

| Harbor webhook | tally event_type | payload |
|---|---|---|
| `PUSH_ARTIFACT` | `repository.push` | state `active`, size `{storage_gb: <repo storage after push>, image_count}` — storage fetched via `GET /projects/{p}/repositories/{r}` after the hook |
| `DELETE_ARTIFACT` | `repository.delete_artifact` (UPDATE) | new `{storage_gb, image_count}` |
| `PULL_ARTIFACT` | `repository.pull` | state `active` (counter event; aggregated per period by the Phase-3 `events` counter source — no split) |

- First push to a new repo additionally emits `repository.create`; repo disappearance
  (poller) emits `repository.delete`.
- **Storage poller** (PollingSource, e.g. every 10 min): walks projects/repositories, compares
  storage/image_count against the local snapshot → emits `repository.push`-equivalent size
  updates missed by webhooks (and acts as the de-facto reconciliation data source).
- `event_id`: webhook = `deterministic_event_id()` over (repo, type, occur_at); Harbor webhook
  retries carry the same `occur_at` → dedup holds.
- `resource_id` = `{project}/{repository}`; `project_id` = Harbor project name.

### Resource type schema

```json
// (harbor, repository)
{ "type": "object", "required": ["storage_gb"],
  "properties": { "storage_gb": {"type": "number", "minimum": 0},
                  "image_count": {"type": "integer", "minimum": 0} },
  "additionalProperties": true }
```

### Exporter & reconciliation

Exporter: `harbor_repository_info`, `harbor_repository_storage_bytes`,
`harbor_project_quota_storage_bytes`, `harbor_project_pull_total`, `harbor_project_push_total`
(+ tally labels). Reconciliation adapter: repository listing → `ObservedResource`
(state `active`, size `{storage_gb, image_count}`).

### Counter sources (Phase 3 config additions)

```yaml
- {platform: harbor, resource_type: repository, metric: pulls,  kind: events, event_type: repository.pull}
- {platform: harbor, resource_type: repository, metric: pushes, kind: events, event_type: repository.push}
- {platform: harbor, resource_type: repository, metric: egress_gb, kind: metricsql,
   query: 'sum(increase(harbor_project_egress_bytes{cloud="{cloud}", project="{project_id}"}[{window}])) / 1e9',
   required: false}   # VERIFY: Harbor egress metric availability; else drop the dimension
```

**Exit checklist**: conformance kit; golden test = concept Ex. 5 (storage 10→15 GB on 03-18 ⇒
minutes 24480/20160 with sliced counters 812/47 then 711/23); webhook retry → no duplicate
events; poller catches a size change with webhooks disabled.

---

## Cross-cutting: per-provider Definition of Done

Every provider WP is complete when (concept §3.1 checklist, made testable):

1. Ingest credential issued & documented (`tally-reporting-admin create-ingest-credential`).
2. Collector passes the conformance kit + restart/buffer drill; deployed with `/healthz` +
   `/metrics`.
3. Exporter scraped by VictoriaMetrics with mandatory labels (verify via `/api/v1/series`).
4. Reconciliation adapter wired into `clouds.yaml`; drift drill passes.
5. Resource types registered (size schemas above); ingest rejects malformed sizes.
6. Projects registered (+ relations where applicable).
7. Pricing section present; one metering+rating golden test per provider green.
8. Phase-2 dashboards/alerts show the new cloud (template variables pick it up automatically).

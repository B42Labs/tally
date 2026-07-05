# Tally – Reporting, Metering & Rating for Cloud Platforms

## 1. Overview

Tally is a **cloud-platform-agnostic** architecture for **reporting, metering, and rating**. It collects metrics, lifecycle events, and inventory data from arbitrary cloud platforms, records usage in neutral units, and applies configurable pricing models. The resulting rated usage data can feed billing systems, cost dashboards, capacity planning, or chargeback workflows.

The system uses a **provider pattern**: each cloud platform (OpenStack, Hetzner, STACKIT, IONOS, …) implements a thin integration layer (exporter + event collector), while all core components — Reporting API, metrics store, metering, and rating — remain shared and platform-independent.

**OpenStack** serves as the first concrete provider implementation. The architecture is designed so that additional platforms and services (e.g. Gardener, Harbor) can be integrated following the same pattern.

### Goals

- Collect runtime data (metrics) from cloud resources centrally — regardless of the underlying platform
- Record lifecycle events of cloud resources without gaps
- Export inventory data from platform-specific sources (databases, APIs)
- Meter usage in neutral, platform-independent units (minutes, counts, sizes)
- Rate metered usage by applying configurable pricing models
- Ensure extensibility for additional cloud platforms and services

### Design Principles

- **Platform-agnostic data model**: Unified schemas for metrics and events across all platforms and services
- **Provider pattern**: Each cloud platform registers with its resource types, exporters, and event collectors
- **VictoriaMetrics as metrics store**: All metrics are stored in VictoriaMetrics (PromQL/MetricsQL and Remote-Write compatible)
- **Project as first-class entity**: Projects are registered with their platform affiliation; cross-platform dependencies are modeled as metadata-enriched directed relations
- **Dual ingestion with reconciliation**: Events provide real-time data; periodic API sync ensures consistency and catches missed events
- **Events as single source of truth**: The append-only event history is authoritative; `current_resources` is a derived projection that can be rebuilt at any time by replay
- **Billing-grade ingestion**: At-least-once delivery with idempotent, deduplicated event ingestion; out-of-order and late events are handled by projection replay
- **Cloud instance dimension**: `platform` identifies the platform *type*, `cloud` identifies the concrete *installation* — multiple deployments of the same platform (e.g. two OpenStack clouds) are first-class
- **Metering separated from rating**: Usage is recorded in neutral units (minutes, counts, sizes) first; pricing is applied as a separate step
- **Reproducible billing**: Versioned pricing models, temporally valid project relations, and immutable finalized billing periods with delta-based corrections — re-processing a past period always yields the same result
- **Decimal money arithmetic**: All monetary calculation uses decimal types with defined rounding rules — never binary floats

---

## 2. Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                        CLOUD PLATFORM (Provider)                            │
│                                                                             │
│  Any cloud platform: OpenStack, Hetzner, STACKIT, IONOS, ...                │
│                                                                             │
│  ┌────────────────────┐     ┌────────────────────┐                          │
│  │ Platform-specific  │     │ Platform-specific  │                          │
│  │ Metrics Exporter   │     │ Event Collector    │                          │
│  │                    │     │                    │                          │
│  │ Exposes /metrics   │     │ Sends lifecycle    │                          │
│  │ (Prometheus fmt)   │     │ events via HTTP    │                          │
│  │                    │     │ (buffered,         │                          │
│  │                    │     │  at-least-once)    │                          │
│  └─────────┬──────────┘     └─────────┬──────────┘                          │
│            │                          │                                     │
└────────────┼──────────────────────────┼─────────────────────────────────────┘
             │ /metrics (scrape)        │ POST /api/v1/events
             ▼                          ▼
   ┌──────────────────┐      ┌────────────────────┐
   │ VictoriaMetrics  │      │ Reporting API      │───▶ reconciliation (CronJob)
   │                  │      │ (event store,      │     polls platform APIs,
   │ Central metrics  │◀─────┤  project registry, │◀─── writes synthetic events
   │ store            │scrape│  /metrics)         │
   └────────┬─────────┘      └─────────┬──────────┘
            │                          │
            │                          ▼
            │                ┌────────────────────┐
            │                │ PostgreSQL +       │
            │                │ TimescaleDB        │
            │                │ (events = source   │
            │                │  of truth,         │
            │                │  current_resources,│
            │                │  projects)         │
            │                └─────────┬──────────┘
            │                          │
            ▼                          ▼
   ┌─────────────────────────────────────────────┐
   │              Metering + Rating              │
   │                                             │
   │  1. Metering: usage in neutral units        │
   │  2. Rating: apply versioned pricing model   │
   │  → rated usage data / billing output        │
   └─────────────────────────────────────────────┘
```

---

## 3. Core Components (Platform-Agnostic)

### 3.1 Provider Interface

Each cloud platform must implement two integration points:

| Component | Responsibility | Output |
|-----------|---------------|--------|
| **Metrics Exporter** | Expose inventory and runtime metrics in Prometheus exposition format | `/metrics` endpoint, scraped by VictoriaMetrics |
| **Event Collector** | Send lifecycle events (create, delete, resize, state changes) to the Reporting API via HTTP POST | `POST /api/v1/events` |

Additionally, each provider:
- Registers its projects in the Project Registry
- Defines its resource types and their `size` schema
- Defines its pricing model
- Optionally implements a reconciliation adapter (for the Reporting API to poll the platform's API)

#### Label Convention (mandatory for all providers)

All metrics must carry the following labels:

```
platform="openstack|hetzner|stackit|ionos|..."   # platform type
cloud="os-prod-eu1|hetzner-main|..."             # concrete installation of that platform
resource_type="instance|volume|server|..."
project_id="<project-identifier>"
resource_id="<resource-identifier>"
```

`platform` names the platform *type*, `cloud` names the concrete *installation*. Two OpenStack
clouds share `platform="openstack"` but differ in `cloud`. Resource and project identifiers are
only assumed unique within `(cloud, resource_type)` — all keys and joins therefore include `cloud`.

#### Event Schema (mandatory for all providers)

All events sent to the Reporting API must contain the following fields:

```json
{
  "event_id":      "provider-native unique ID (idempotency key)",
  "timestamp":     "ISO 8601",
  "event_type":    "resource.action[.phase]",
  "platform":      "openstack|hetzner|stackit|ionos|...",
  "cloud":         "installation identifier, e.g. os-prod-eu1",
  "resource_type": "instance|volume|server|...",
  "resource_id":   "UUID or unique identifier",
  "project_id":    "project identifier",
  "source":        "collector | reconciliation",
  "payload":       {}
}
```

`event_id` is the deduplication key. Providers use the platform's native event/action ID where
one exists (oslo.messaging `message_id`, Hetzner action ID, ...); otherwise a deterministic hash
of `(cloud, resource_id, event_type, timestamp)`. `source` distinguishes real collector events
from synthetic events generated by reconciliation.

#### Delivery Semantics (mandatory for all providers)

- **At-least-once**: Collectors retry with exponential backoff and buffer events locally
  (disk-backed) while the Reporting API is unreachable — events are never dropped client-side.
- **Idempotent ingestion**: The Reporting API deduplicates on `event_id`; replaying a batch is
  always safe.
- **No ordering assumption**: Events may arrive out of order. The Reporting API handles this via
  projection rebuild (see 3.2); collectors do not need to guarantee ordering.

#### Checklist for New Provider Integration

1. **Obtain ingest credentials** – Register the `cloud` instance; receive a service token / mTLS identity scoped to that `(platform, cloud)`
2. **Implement metrics exporter** – Expose platform-specific metrics with unified label schema
3. **Implement event collector** – Send lifecycle events to Reporting API (with `event_id`, local buffering, retries)
4. **Implement reconciliation adapter** – Enable the Reporting API to poll the platform's API for drift detection
5. **Register projects & relations** – Register platform projects in the Project Registry; define relations to dependent infrastructure projects
6. **Register resource types** – Register each resource type with a JSON Schema for its `size` object (used for ingest validation)
7. **Define pricing model** – Create pricing configuration for the platform's resource types
8. **Add scrape config** – Extend VictoriaMetrics with new scrape target

---

### 3.2 Reporting API (new, to be implemented)

REST API with three roles:

1. **Event sink**: Receives events from any provider's event collector (idempotent — deduplicates on `event_id`)
2. **Query interface**: Enables querying the event history per resource
3. **Reconciliation**: Periodically syncs against platform APIs (via provider-specific adapters) to catch missed events and correct drift

#### API Endpoints

```
# Event ingestion (accepts single events and batches; deduplicates on event_id)
POST /api/v1/events
  Body: { "event_id": "openstack-oslo-msg-9c1f...",
          "event_type": "compute.instance.create.end",
          "timestamp": "2026-03-20T10:15:00Z",
          "platform": "openstack",
          "cloud": "os-prod-eu1",
          "resource_type": "instance",
          "resource_id": "abc-123",
          "project_id": "proj-456",
          "source": "collector",
          "payload": { ... } }

# Event queries
GET  /api/v1/events?cloud=...&project_id=...&resource_type=...&from=...&to=...
GET  /api/v1/resources/{cloud}/{resource_type}/{resource_id}/events

# Resource lifecycle
GET  /api/v1/resources/{cloud}/{resource_type}/{resource_id}/lifecycle
  → Returns the complete lifecycle (create → resize → delete)

# Resource inventory (current state derived from events)
GET  /api/v1/resources?cloud=...&project_id=...&resource_type=...&status=active

# Resource type registry (size schema validation at ingest)
PUT  /api/v1/resource-types/{platform}/{resource_type}   -- register/update JSON Schema for size
GET  /api/v1/resource-types

# Reconciliation (internal, called by CronJob, one per cloud installation)
POST /internal/sync/{cloud}        -- polls that installation's APIs, reconciles current_resources

# Health
GET  /healthz
GET  /readyz
  → Readiness fails if DB is unreachable or if both event ingestion
    and API sync are unavailable simultaneously
  → Liveness fails if unhealthy for longer than configurable threshold (default: 600s)

# Metrics (optional, for VictoriaMetrics scraping)
GET  /metrics
  → Exposes e.g. event_count{platform, cloud, resource_type, event_type}
  → sync_resources_reconciled{cloud}, sync_errors{cloud},
    events_rejected_total{cloud}, events_deduplicated_total{cloud}
  → As well as runtime aggregations derivable from events
```

**Events retention**: Unlimited (legally relevant).

#### Authentication & Authorization

- **Ingest** (`POST /api/v1/events`): mTLS or per-provider service tokens. A credential is
  scoped to one `(platform, cloud)` and cannot submit events for other installations — a
  compromised collector cannot manipulate other clouds' billing data.
- **Internal endpoints** (`/internal/sync/*`): Reachable only from the cluster-internal
  network, authenticated with a dedicated service identity.
- **Query endpoints**: OIDC/token-based with project-scoped RBAC — a consumer only sees
  projects it is authorized for. Metering/rating runs use a read-all service role.
- **Audit log**: All write operations (events, projects, relations, resource types) are
  audit-logged with the acting credential identity.

#### Ingest Validation & Dead Letter

Incoming events are validated against the event schema and — for `payload.size` — against the
JSON Schema registered for the resource type. Invalid events are rejected (HTTP 400) **and**
stored in a `rejected_events` dead-letter table, so schema drift on the provider side becomes
operationally visible instead of silently corrupting billing data.

#### Data Model (PostgreSQL + TimescaleDB)

```sql
-- Events table (TimescaleDB hypertable with compression), append-only source of truth
CREATE TABLE events (
    event_id        TEXT NOT NULL,          -- provider-native idempotency key
    timestamp       TIMESTAMPTZ NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_type      TEXT NOT NULL,
    platform        TEXT NOT NULL,          -- platform type: 'openstack', 'hetzner', ...
    cloud           TEXT NOT NULL,          -- concrete installation: 'os-prod-eu1', ...
    resource_type   TEXT NOT NULL,          -- 'instance', 'volume', 'server', ...
    resource_id     TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    source          TEXT NOT NULL DEFAULT 'collector',  -- 'collector' | 'reconciliation'
    payload         JSONB,
    -- TimescaleDB requires the partitioning column in every unique constraint,
    -- so the primary key is (event_id, timestamp); dedup works because a
    -- duplicate event carries the same event_id AND the same timestamp.
    PRIMARY KEY (event_id, timestamp)
);

-- Convert to TimescaleDB hypertable for automatic partitioning + compression
SELECT create_hypertable('events', 'timestamp');

-- Indexes for typical queries
CREATE INDEX idx_events_resource ON events (cloud, resource_type, resource_id, timestamp);
CREATE INDEX idx_events_project  ON events (project_id, timestamp);
CREATE INDEX idx_events_type     ON events (event_type, timestamp);

-- Dead letter for events that fail schema validation
CREATE TABLE rejected_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    reason       TEXT NOT NULL,
    raw          JSONB NOT NULL
);

-- Current resource state: DERIVED PROJECTION over the event history.
-- Updated on every event and by reconciliation; rebuildable at any time by replay.
-- Resource IDs are only unique per (cloud, resource_type) — Hetzner uses small
-- integer IDs with independent sequences per resource type.
CREATE TABLE current_resources (
    cloud           TEXT NOT NULL,          -- concrete installation: 'os-prod-eu1', ...
    platform        TEXT NOT NULL,          -- platform type: 'openstack', 'hetzner', ...
    resource_type   TEXT NOT NULL,          -- 'instance', 'volume', 'server', ...
    resource_id     TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    state           TEXT NOT NULL,          -- 'active', 'shutoff', 'shelved', 'deleted', ...
    size            JSONB DEFAULT '{}',     -- resource-specific: {"vcpus": 4, "ram_gb": 8, "disk_gb": 80}
    created_at      TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    last_event_type TEXT NOT NULL,
    last_event_at   TIMESTAMPTZ NOT NULL,
    last_payload    JSONB,
    PRIMARY KEY (cloud, resource_type, resource_id)
);

CREATE INDEX idx_current_resources_project ON current_resources (project_id);
CREATE INDEX idx_current_resources_type    ON current_resources (resource_type, state);
```

The `size` JSONB field stores resource-specific dimensions. Any change to `size`, `state`, or `project_id` (ownership transfer) triggers a metering split (see Metering & Rating Engine). Examples per resource type:

| Resource Type | Platform | `size` Example | Billable Change Events |
|---------------|----------|----------------|----------------------|
| `instance` | OpenStack | `{"vcpus": 4, "ram_gb": 8, "disk_gb": 80, "flavor": "m1.large"}` | resize (flavor change), shelve/unshelve, power on/off |
| `volume` | OpenStack | `{"size_gb": 100, "type": "ssd"}` | resize (size change), retype (SSD → HDD) |
| `floating_ip` | OpenStack | `{"ip_version": 4}` | create/delete only |
| `image` | OpenStack | `{"size_gb": 2.5}` | create/delete only |
| `server` | Hetzner | `{"vcpus": 4, "ram_gb": 16, "disk_gb": 80, "server_type": "cx41"}` | upgrade/downgrade, power on/off |
| `server` | STACKIT | `{"vcpus": 8, "ram_gb": 32, "disk_gb": 160, "machine_type": "c1.8"}` | resize, power on/off |
| `server` | IONOS | `{"cores": 4, "ram_gb": 16, "type": "ENTERPRISE"}` | resize, power on/off |
| `shoot` | Gardener | `{"worker_count": 3, "machine_type": "m1.xlarge", "kubernetes_version": "1.29"}` | worker.scale, hibernate/wake |
| `loadbalancer` | OpenStack | `{"listeners": 2, "pools": 1}` | listener/pool add/remove |
| `repository` | Harbor | `{"storage_gb": 12.5, "image_count": 47}` | push (storage grows), delete (storage shrinks) |

#### Source of Truth & Projection Rebuild

The append-only `events` table is the **single source of truth**. `current_resources` is a
derived projection:

- Events arriving in order (`timestamp >= last_event_at`) update the projection incrementally.
- An event arriving **out of order or late** (`timestamp < last_event_at`) triggers a replay:
  the projection row is rebuilt from the resource's full event history. Ingestion is therefore
  order-independent and deterministic.
- The Metering Engine derives its interval timelines from the event history, not from the
  projection — the projection exists for fast inventory queries and reconciliation diffing.

#### Reconciliation

The Reporting API periodically polls platform APIs (via CronJob, recommended: every 10 minutes) using provider-specific reconciliation adapters. Each adapter knows how to list resources from its platform and compare against `current_resources`. Differences are resolved:

| Situation | Action |
|-----------|--------|
| Resource exists in platform API but not in DB | Insert into `current_resources`, generate synthetic create event |
| Resource exists in DB but not in platform API | Mark as deleted, generate synthetic delete event |
| Resource attributes differ (e.g. flavor after resize) | Update `current_resources`, generate synthetic resize event |

This dual-ingestion pattern (real-time events + periodic sync) ensures no resource is missed, even if event notifications are lost.

Synthetic events are marked with `source: "reconciliation"` and carry a deterministic
`event_id` (hash of sync run + resource), so re-running a sync never duplicates them.

**Known limitations** (accepted, must be monitored):

- Synthetic delete events carry the poll time, not the actual deletion time — up to one sync
  interval of overbilling per lost delete event. Where the platform API exposes real
  timestamps (e.g. Nova `deleted_at` via `GET /servers?deleted=true`), the adapter uses those
  instead of the poll time.
- A resource that is created **and** deleted between two sync runs and whose events were lost
  is invisible. Keep sync intervals short and alert when a cloud's event ingestion rate drops
  to zero (collector outage indicator).

**Technology**: Python with FastAPI (rapid development, async support, automatic OpenAPI documentation).

TimescaleDB extension is used for automatic partitioning, native compression on event data, and optimized time-range queries.

---

### 3.3 VictoriaMetrics

Central metrics store for all runtime and inventory data from all platforms.

**Deployment**: Single node or cluster depending on scale requirements. Single binary with low operational overhead.

**Key advantages**:
- ~10x better compression than Prometheus
- Native long-term storage (no Thanos/Cortex needed)
- Better suited for high cardinality
- Fully PromQL compatible + extended MetricsQL
- Multi-tenancy (Enterprise feature — license cost; not required for the initial scope)

**Scrape targets** (grows with each provider):

| Target | Interval | Metrics |
|--------|----------|---------|
| Provider metrics exporters | 60s | Inventory data, quotas, resource info |
| Reporting API `/metrics` | 30s | Event counts, derived metrics |
| OTel Collector | 15s | Runtime metrics from platforms that push |

**Metrics retention**: 13 months (for year-over-year comparisons and billing periods).

---

### 3.4 Metering & Rating Engine (new, to be implemented)

Generates rated usage data in two distinct phases: **Metering** (usage in neutral units) and **Rating** (apply pricing). This separation allows usage data to be validated independently before monetary values are attached. The engine works identically regardless of the source platform.

**Database**: Own dedicated database (clean separation of concerns from the Reporting API database).

**Billing periods** are calendar months in **UTC** (configurable per installation; UTC avoids
DST edge cases with 23/25-hour days). A period is metered as a **batch run after the period
ends plus a grace period** — see Billing Period Lifecycle below.

#### Architecture

```
┌───────────────────────┐     ┌──────────────────────┐
│ VictoriaMetrics       │     │ Reporting API        │
│                       │     │ (events +            │
│ "What resources did   │     │  current_resources + │
│  the instance have?"  │     │  project graph)      │
│                       │     │                      │
└───────────┬───────────┘     └──────────┬───────────┘
            │                            │
            ▼                            ▼
      ┌─────────────────────────────────────────┐
      │         Metering (Phase 1)              │
      │                                         │
      │  → Usage records in minutes per         │
      │    resource per billing period          │
      │  → Platform-agnostic                    │
      └──────────────────┬──────────────────────┘
                         │
                         ▼
      ┌─────────────────────────────────────────┐
      │          Rating (Phase 2)               │
      │                                         │
      │  → Apply platform-specific pricing      │
      │  → Generate billing data with costs     │
      └─────────────────────────────────────────┘
```

#### Billing Period Lifecycle

```
open ──(period ends)──▶ grace (default: 72h) ──(metering+rating run)──▶ finalized ──▶ corrected*
```

- **Grace period**: After a period ends, metering waits (default: 72 h) for late events from
  collector backlogs and reconciliation before the finalizing run.
- **Idempotent runs**: Metering/rating runs are versioned (`run_id`). Re-running before
  finalization atomically supersedes the previous run's records.
- **Finalization**: Finalized usage and rating records are **immutable** — they may already
  have been handed to an external billing/ERP system.
- **Corrections** (*): Events arriving after finalization, or discovered metering bugs, produce
  a **correction run**: delta usage records and credit/debit line items that reference the
  original run. Finalized records are never modified in place.
- **Reproducibility**: Every rated record references its `run_id` and the pricing model version
  used; re-processing a past period with the same inputs yields identical results.

#### Engine Data Model (dedicated database)

```sql
-- Versioned metering/rating runs
CREATE TABLE runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    period_from     TIMESTAMPTZ NOT NULL,
    period_to       TIMESTAMPTZ NOT NULL,
    kind            TEXT NOT NULL,              -- 'regular' | 'correction'
    corrects_run_id UUID REFERENCES runs(id),   -- set for correction runs
    pricing_version TEXT,                       -- pricing model version used by rating
    status          TEXT NOT NULL,              -- 'running' | 'completed' | 'finalized' | 'superseded' | 'failed'
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);

-- Metering output (Phase 1)
CREATE TABLE usage_records (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL REFERENCES runs(id),
    cloud           TEXT NOT NULL,
    platform        TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    state           TEXT NOT NULL,
    from_ts         TIMESTAMPTZ NOT NULL,
    to_ts           TIMESTAMPTZ NOT NULL,
    usage           JSONB NOT NULL              -- {"minutes": ..., "count": 1, ...}
);

-- Rating output (Phase 2); money is NUMERIC — never floats
CREATE TABLE rated_records (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL REFERENCES runs(id),
    usage_record_id UUID NOT NULL REFERENCES usage_records(id),
    dimension       TEXT NOT NULL,              -- usage metric this cost refers to
    amount          NUMERIC(14,2) NOT NULL,     -- negative amounts occur in correction runs
    currency        TEXT NOT NULL
);
```

#### Phase 1: Metering

Calculates resource usage per monthly billing period. Usage is captured as a **`usage` JSONB object** with metric-specific fields. This supports three usage types:

| Usage Type | Description | Example Metrics |
|-----------|-------------|-----------------|
| **Time-based** | Resource exists for a duration | `minutes` (always present for time-based resources) |
| **Gauge-based** | Resource has a measurable size/quantity at a point in time | `storage_gb`, `worker_count`, `vcpus` |
| **Counter-based** | Accumulated events/volume within the billing period | `pulls`, `pushes`, `egress_gb`, `api_calls` |

A single usage record can combine all three types. Time-based and gauge-based metrics are derived from the event history; counter-based metrics are aggregated from the events table or VictoriaMetrics.

**Flow:**

1. **Resolve project graph**: Query Project Registry for related projects **as valid during the billing period** (relations are temporally scoped, see 3.5)
2. **Determine resources**: Find all resources of the project and its related projects that existed at any point within the billing period (from the event history; `current_resources` serves as a fast index for currently existing resources)
3. **Calculate time + gauge usage**: For each resource, replay its event history into an interval timeline (each interval has constant `size`, `state`, and `project_id`), then calculate `minutes` per interval clipped to the billing period
4. **Aggregate counter usage**: Query events table / VictoriaMetrics for accumulated metrics (pulls, traffic, etc.) sliced per interval within the billing period

**Generic splitting rule**: Any change to a resource's `size`, `state`, or `project_id` in its event history triggers a split. The metering creates **two records** at the change timestamp – one for the old configuration up to T, one for the new configuration from T onwards. This applies uniformly to all resource types across all platforms:

| Platform | Resource Type | Change Event | Split Trigger |
|----------|---------------|-------------|---------------|
| OpenStack | Instance | `compute.instance.resize.end` | `size` changes (vcpus, ram_gb, disk_gb, flavor) |
| OpenStack | Instance | `compute.instance.shelve` / `unshelve` | `state` changes (active → shelved → active) |
| OpenStack | Instance | `compute.instance.power_off` / `power_on` | `state` changes (active → shutoff → active) |
| OpenStack | Volume | `volume.resize.end` | `size.size_gb` changes (e.g. 100 → 200) |
| OpenStack | Volume | `volume.retype` | `size.type` changes (e.g. ssd → hdd) |
| OpenStack | Volume | `volume.transfer.accept.end` | `project_id` changes (ownership transfer — usage attributed to the old project up to T, to the new project from T) |
| Hetzner | Server | `server.upgrade` / `server.downgrade` | `size` changes (server_type) |
| STACKIT | Server | `server.resize` | `size` changes (machine_type) |
| Gardener | Shoot | `shoot.worker.scale` | `size.worker_count` changes (e.g. 3 → 5) |
| Gardener | Shoot | `shoot.hibernate.start` / `end` | `state` changes (active → hibernated → active) |
| Harbor | Repository | `repository.push` / image delete | `size.storage_gb` changes |

**Concurrency protection**: PostgreSQL advisory locks per `(cloud, resource)` serialize metering runs against concurrent projection rebuilds (late-event replays) and against parallel runs of the same period.

**Metering invariants** (checked on every run; violations abort the run instead of producing silently wrong data):

- **No gaps, no overlaps**: The usage records of a resource within a period are contiguous and non-overlapping
- **Coverage**: The sum of `minutes` across all splits equals the intersection of the resource's lifetime with the billing period (e.g. March 2026 = 31 days = 44,640 minutes for a resource existing all month)
- **Traceability**: Every split boundary corresponds to an event in the history
- **Implicit count**: Every usage record carries `count: 1`, so purely existence-priced resources (e.g. floating IPs) can be priced via the `count` metric (omitted in the examples below for brevity)

The worked examples below double as golden tests for the engine.

#### Metering Output Examples

Each usage record contains a generic `usage` object combining time, gauge, and counter metrics as applicable.

**Example 1: OpenStack instance resize mid-month**

VM `def-456` is resized from m1.small to m1.large on March 16:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "proj-456",
  "platform": "openstack",
  "usage_records": [
    {
      "resource_type": "instance",
      "resource_id": "def-456",
      "state": "active",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-16T00:00:00Z",
      "usage": {
        "minutes": 21600,
        "vcpus": 2, "ram_gb": 4, "disk_gb": 40, "flavor": "m1.small",
        "egress_gb": 12.3, "ingress_gb": 5.1
      }
    },
    {
      "resource_type": "instance",
      "resource_id": "def-456",
      "state": "active",
      "from": "2026-03-16T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": {
        "minutes": 23040,
        "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "flavor": "m1.large",
        "egress_gb": 24.7, "ingress_gb": 10.2
      }
    }
  ]
}
```

**Example 2: Hetzner server upgrade mid-month**

Server `srv-001` is upgraded from CX21 to CX31 on March 15:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "hetzner-proj-42",
  "platform": "hetzner",
  "usage_records": [
    {
      "resource_type": "server",
      "resource_id": "srv-001",
      "state": "running",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-15T00:00:00Z",
      "usage": {
        "minutes": 20160,
        "vcpus": 2, "ram_gb": 4, "disk_gb": 40, "server_type": "cx21"
      }
    },
    {
      "resource_type": "server",
      "resource_id": "srv-001",
      "state": "running",
      "from": "2026-03-15T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": {
        "minutes": 24480,
        "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "server_type": "cx31"
      }
    }
  ]
}
```

**Example 3: OpenStack volume resize + retype**

Volume `vol-789` is extended from 100 GB to 200 GB on March 10, then retyped from SSD to HDD on March 20:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "proj-456",
  "platform": "openstack",
  "usage_records": [
    {
      "resource_type": "volume",
      "resource_id": "vol-789",
      "state": "in-use",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-10T00:00:00Z",
      "usage": { "minutes": 12960, "size_gb": 100, "type": "ssd" }
    },
    {
      "resource_type": "volume",
      "resource_id": "vol-789",
      "state": "in-use",
      "from": "2026-03-10T00:00:00Z",
      "to": "2026-03-20T00:00:00Z",
      "usage": { "minutes": 14400, "size_gb": 200, "type": "ssd" }
    },
    {
      "resource_type": "volume",
      "resource_id": "vol-789",
      "state": "in-use",
      "from": "2026-03-20T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": { "minutes": 17280, "size_gb": 200, "type": "hdd" }
    }
  ]
}
```

**Example 4: Gardener Shoot worker scaling + hibernation**

Shoot `shoot-abc` scales from 3 to 5 workers on March 12, then hibernates on March 25 and wakes on March 28:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "team-alpha",
  "platform": "gardener",
  "usage_records": [
    {
      "resource_type": "shoot",
      "resource_id": "shoot-abc",
      "state": "active",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-12T00:00:00Z",
      "usage": { "minutes": 15840, "worker_count": 3, "machine_type": "m1.xlarge" }
    },
    {
      "resource_type": "shoot",
      "resource_id": "shoot-abc",
      "state": "active",
      "from": "2026-03-12T00:00:00Z",
      "to": "2026-03-25T00:00:00Z",
      "usage": { "minutes": 18720, "worker_count": 5, "machine_type": "m1.xlarge" }
    },
    {
      "resource_type": "shoot",
      "resource_id": "shoot-abc",
      "state": "hibernated",
      "from": "2026-03-25T00:00:00Z",
      "to": "2026-03-28T00:00:00Z",
      "usage": { "minutes": 4320, "worker_count": 5, "machine_type": "m1.xlarge" }
    },
    {
      "resource_type": "shoot",
      "resource_id": "shoot-abc",
      "state": "active",
      "from": "2026-03-28T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": { "minutes": 5760, "worker_count": 5, "machine_type": "m1.xlarge" }
    }
  ]
}
```

**Example 5: Harbor repository (counter-based + gauge-based)**

Repository `team-alpha/app` exists all month; storage grows from 10 GB to 15 GB on March 18:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "harbor-team-alpha",
  "platform": "harbor",
  "usage_records": [
    {
      "resource_type": "repository",
      "resource_id": "team-alpha/app",
      "state": "active",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-18T00:00:00Z",
      "usage": {
        "minutes": 24480,
        "storage_gb": 10,
        "pulls": 812,
        "pushes": 47,
        "egress_gb": 38.5
      }
    },
    {
      "resource_type": "repository",
      "resource_id": "team-alpha/app",
      "state": "active",
      "from": "2026-03-18T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": {
        "minutes": 20160,
        "storage_gb": 15,
        "pulls": 711,
        "pushes": 23,
        "egress_gb": 31.2
      }
    }
  ]
}
```

#### Phase 2: Rating

Applies the pricing model to usage records. This is a pure calculation step with no external queries.

1. **Load pricing model**: Load the pricing model **version valid for the billing period** (see Pricing Model below) with configurable prices per platform, resource type, usage metric, and state
2. **Calculate costs per dimension**: Each dimension references a key from the `usage` object. `state_modifier` and `type_modifier` (from pricing config, each default 1.0) combine multiplicatively on time-based costs:
   - Time × gauge: `cost = (usage.minutes / 60) × usage.{gauge_metric} × price_per_unit_per_hour × state_modifier × type_modifier`
   - Counter-based: `cost = usage.{counter_metric} × price_per_unit` (not affected by modifiers)
3. **Aggregate**: Sum per resource, per project, and across related projects
4. **Generate output**: Structured billing data with related costs attributed to source project

#### Money & Rounding (normative)

- All monetary computation uses **decimal arithmetic** (PostgreSQL `NUMERIC`, Python `decimal`)
  — binary floats are forbidden wherever money is computed or stored.
- Per-dimension costs are computed at full precision and rounded **half-up to 2 decimal places
  per dimension per usage record** (matching the examples below). Aggregates (per resource, per
  project, per period) are sums of the rounded values — totals always equal the sum of their
  visible line items.
- The currency is defined per pricing model version (initially EUR); values in different
  currencies are never aggregated.

#### Pricing Model (configurable, versioned, per platform)

Pricing models are **versioned with validity periods**. Rating a billing period uses the version
valid for that period; every rated record references the version used. Price changes create a
new version — existing versions are never edited, so re-rating past periods is always
reproducible.

Each dimension references a metric key from the `usage` object. The `type` field determines the formula:

```yaml
version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"   # applies to billing periods starting at/after this instant
currency: "EUR"

pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"              # key in usage object
          type: "time_gauge"           # cost = (minutes/60) × vcpus × price
          price_per_unit_hour: 0.02
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: 0.005
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: 0.001
        - metric: "egress_gb"
          type: "counter"              # cost = egress_gb × price
          price_per_unit: 0.09
      state_modifiers:
        shelved: 0.0                   # shelved instances are free
        shutoff: 0.5                   # powered off = 50% of active price
    volume:
      dimensions:
        - metric: "size_gb"
          type: "time_gauge"
          price_per_unit_hour: 0.0001
      type_modifiers:                  # per usage.type; combines multiplicatively with state_modifiers
        ssd: 1.0
        hdd: 0.5
    floating_ip:
      dimensions:
        - metric: "count"              # implicit count = 1 per existing resource (see metering invariants)
          type: "time_gauge"
          price_per_unit_hour: 0.005

  hetzner:
    server:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: 0.015
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: 0.004
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: 0.0008
      state_modifiers:
        off: 0.5

  stackit:
    server:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: 0.025
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: 0.006
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: 0.0012

  ionos:
    server:
      dimensions:
        - metric: "cores"
          type: "time_gauge"
          price_per_unit_hour: 0.022
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: 0.005

  gardener:
    shoot:
      dimensions:
        - metric: "worker_count"
          type: "time_gauge"
          price_per_unit_hour: 0.10
      state_modifiers:
        hibernated: 0.0                # hibernated shoots are free

  harbor:
    repository:
      dimensions:
        - metric: "storage_gb"
          type: "time_gauge"
          price_per_unit_hour: 0.00005
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: 0.12
        - metric: "pulls"
          type: "counter"
          price_per_unit: 0.0          # pulls are free, tracked for reporting only
```

#### Future Extension: Commercial Pricing (Kickbacks, Reseller, Project-Specific Adjustments)

The current pricing model covers per-platform, per-resource-type pricing with state and type modifiers. For commercial scenarios — reseller partnerships, kickback agreements, or project-specific discounts — the system can be extended by combining **Project Registry relations** (see 3.5) with **pricing overlays** on those relations. No schema changes are required.

##### Core Idea: Pricing Adjustments Live on Relations

Commercial pricing adjustments are not a separate subsystem — they are metadata on project relations. This ensures that pricing logic is always tied to a concrete, auditable relationship between entities (e.g. "this project is managed by this reseller").

The existing `metadata` JSONB column on `project_relations` carries the adjustment definitions. The rating engine resolves these during cost calculation.

##### New Relation Types

Building on the relation model from 3.5:

```
Reseller "Partner Corp"       (entry in projects, platform="partner")
  "customer-proj-1"  ─managed_by→  "Partner Corp"
  "customer-proj-2"  ─managed_by→  "Partner Corp"

Meta-Project "Customer Alpha"  (entry in projects, platform="meta")
  "team-alpha-os"    ─member_of→   "Customer Alpha"
  "team-alpha"       ─member_of→   "Customer Alpha"
```

Relation metadata carries the pricing adjustments:

```json
{
  "source_id": "customer-proj-1",
  "target_id": "partner-corp",
  "relation_type": "managed_by",
  "metadata": {
    "pricing_adjustments": [
      {
        "type": "discount",
        "description": "Reseller end-customer discount",
        "rate": 0.15,
        "scope": "all"
      },
      {
        "type": "kickback",
        "description": "Reseller commission on net revenue",
        "rate": 0.10,
        "scope": "all"
      }
    ]
  }
}
```

Adjustment types:

| Type | Description | Calculated On |
|------|-------------|---------------|
| `discount` | Reduces the end-customer price | Base cost |
| `kickback` | Commission paid to the relation target (e.g. reseller) | Net cost (after discounts) |
| `surcharge` | Additional fee (e.g. managed-service markup) | Base cost |
| `project_discount` | Project- or customer-specific discount (e.g. volume, loyalty) | Base cost |

The `scope` field controls granularity: `"all"` applies to all resource types, `"openstack.instance"` only to OpenStack instances, etc.

##### Rating Engine Extension

The rating calculation extends from two to three steps:

1. **Base cost**: Calculated as before (usage × price × state/type modifiers)
2. **Resolve relations**: Traverse the project's relations to collect all applicable `pricing_adjustments`
3. **Apply adjustments**: Deterministic application order: `surcharge` → `discount` → `project_discount` → `kickback`. Surcharges are applied on the base cost; discounts stack multiplicatively on the surcharged amount; kickbacks are computed on the resulting net amount and emitted as separate line items (they do not change the customer's net cost). Adjustments of the same type are ordered by relation `id` for reproducibility

Output example for a reseller-managed project:

```json
{
  "project_id": "customer-proj-1",
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "base_cost_eur": 1200.00,
  "adjustments": [
    {
      "type": "discount",
      "relation_type": "managed_by",
      "relation_target": "Partner Corp",
      "rate": 0.15,
      "amount_eur": -180.00
    },
    {
      "type": "kickback",
      "relation_type": "managed_by",
      "relation_target": "Partner Corp",
      "rate": 0.10,
      "base_eur": 1020.00,
      "amount_eur": 102.00
    }
  ],
  "net_cost_eur": 1020.00,
  "kickback_eur": 102.00
}
```

##### Why Relations, Not Separate Config

- **Auditability**: Every pricing adjustment is traceable to a specific relation ("why does this project get 15% off?" → because it's `managed_by` Partner Corp)
- **Lifecycle**: When a reseller relationship ends, the relation is closed (`valid_to` is set, see 3.5) — the adjustment stops applying to subsequent billing periods, while past periods remain reproducible because the relation history is preserved
- **Transitivity**: The existing `depth` parameter on relation queries allows inherited adjustments (e.g. a meta-project discount that applies to all member projects)
- **No schema changes**: Uses the existing `project_relations.metadata` JSONB column and additive `relation_type` values

#### End-to-End Example: OpenStack VM with State Changes

A VM `abc-123` (m1.large: 4 vCPUs, 8 GB RAM, 80 GB disk) runs the full month of March, but is **powered off** from March 11 to March 21 (10 days = 14400 minutes).

**Step 1 – Metering** produces three usage records (split at each state change):

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "proj-456",
  "platform": "openstack",
  "usage_records": [
    {
      "resource_type": "instance",
      "resource_id": "abc-123",
      "state": "active",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-11T00:00:00Z",
      "usage": { "minutes": 14400, "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 18.0 }
    },
    {
      "resource_type": "instance",
      "resource_id": "abc-123",
      "state": "shutoff",
      "from": "2026-03-11T00:00:00Z",
      "to": "2026-03-21T00:00:00Z",
      "usage": { "minutes": 14400, "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 0 }
    },
    {
      "resource_type": "instance",
      "resource_id": "abc-123",
      "state": "active",
      "from": "2026-03-21T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": { "minutes": 15840, "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 22.5 }
    }
  ]
}
```

**Step 2 – Rating** applies the pricing model with `state_modifiers` (`active` = 1.0, `shutoff` = 0.5):

```
Record 1 (active, 10 days):
  vcpus:  (14400/60) × 4 × 0.02 × 1.0  = 19.20 EUR
  ram_gb: (14400/60) × 8 × 0.005 × 1.0  =  9.60 EUR
  disk_gb:(14400/60) × 80 × 0.001 × 1.0 = 19.20 EUR
  egress: 18.0 × 0.09                    =  1.62 EUR
  → subtotal: 49.62 EUR

Record 2 (shutoff, 10 days – 50% modifier on time_gauge, no egress):
  vcpus:  (14400/60) × 4 × 0.02 × 0.5  =  9.60 EUR
  ram_gb: (14400/60) × 8 × 0.005 × 0.5  =  4.80 EUR
  disk_gb:(14400/60) × 80 × 0.001 × 0.5 =  9.60 EUR
  egress: 0 × 0.09                       =  0.00 EUR
  → subtotal: 24.00 EUR

Record 3 (active, 11 days):
  vcpus:  (15840/60) × 4 × 0.02 × 1.0  = 21.12 EUR
  ram_gb: (15840/60) × 8 × 0.005 × 1.0  = 10.56 EUR
  disk_gb:(15840/60) × 80 × 0.001 × 1.0 = 21.12 EUR
  egress: 22.5 × 0.09                    =  2.03 EUR
  → subtotal: 54.83 EUR
```

**Step 3 – Rating Output** aggregates per resource, showing the state breakdown:

```json
{
  "billing_period": {
    "from": "2026-03-01T00:00:00Z",
    "to": "2026-04-01T00:00:00Z"
  },
  "project_id": "proj-456",
  "platform": "openstack",
  "line_items": [
    {
      "resource_type": "instance",
      "resource_id": "abc-123",
      "platform": "openstack",
      "description": "m1.large instance",
      "periods": [
        {
          "state": "active",
          "hours": 240,
          "usage": { "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 18.0 },
          "cost": { "vcpus": 19.20, "ram_gb": 9.60, "disk_gb": 19.20, "egress_gb": 1.62, "total": 49.62 },
          "state_modifier": 1.0
        },
        {
          "state": "shutoff",
          "hours": 240,
          "usage": { "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 0 },
          "cost": { "vcpus": 9.60, "ram_gb": 4.80, "disk_gb": 9.60, "egress_gb": 0, "total": 24.00 },
          "state_modifier": 0.5
        },
        {
          "state": "active",
          "hours": 264,
          "usage": { "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 22.5 },
          "cost": { "vcpus": 21.12, "ram_gb": 10.56, "disk_gb": 21.12, "egress_gb": 2.03, "total": 54.83 },
          "state_modifier": 1.0
        }
      ],
      "total": 128.45
    }
  ],
  "related_costs": [],
  "total": 128.45,
  "currency": "EUR"
}
```

**Note**: A VM that runs the entire month of March (744 hours) at full price would cost 148.80 EUR plus egress. With 10 days powered off (50% modifier), the total drops to 128.45 EUR. A shelved VM (`state_modifier: 0.0`) would cost 0 EUR for the shelved period.

#### Exclusive Cost Attribution (No Double Billing)

Related costs must never double-bill a resource:

- **Each project is billed in exactly one place.** A project attributed to another via an
  attributing relation (e.g. `infrastructure_tenant`) is excluded from direct billing — its
  costs appear only as `related_costs` of the attributing project.
- **Attributing relations must form a forest.** Cycles are rejected at relation creation and
  traversal is cycle-safe. If a project is reachable via multiple attribution paths, it is
  attributed exactly once (shortest path wins; ties broken by relation `id`) and a warning is
  emitted.
- **Service prices are management fees.** The Gardener `worker_count` price covers the managed
  service on top of the infrastructure — the worker VMs themselves are billed via
  `related_costs`, not hidden in the shoot price.

#### Example with Related Costs (Gardener + OpenStack)

```json
{
  "billing_period": {
    "from": "2026-03-01T00:00:00Z",
    "to": "2026-04-01T00:00:00Z"
  },
  "project_id": "team-alpha",
  "platform": "gardener",
  "line_items": [
    {
      "resource_type": "shoot",
      "resource_id": "shoot-abc",
      "platform": "gardener",
      "description": "Shoot cluster shoot-abc (management fee)",
      "periods": [
        { "state": "active", "hours": 744, "usage": { "worker_count": 3 },
          "cost": { "worker_count": 223.20, "total": 223.20 }, "state_modifier": 1.0 }
      ],
      "total": 223.20
    }
  ],
  "related_costs": [
    {
      "relation_type": "infrastructure_tenant",
      "project_id": "shoot-abc-os-tenant",
      "platform": "openstack",
      "line_items": [
        {
          "resource_type": "instance",
          "resource_id": "worker-1",
          "platform": "openstack",
          "description": "m1.xlarge worker node",
          "periods": [
            { "state": "active", "hours": 744,
              "usage": { "vcpus": 8, "ram_gb": 16, "disk_gb": 160 },
              "cost": { "vcpus": 119.04, "ram_gb": 59.52, "disk_gb": 119.04, "total": 297.60 },
              "state_modifier": 1.0 }
          ],
          "total": 297.60
        }
      ],
      "total": 297.60
    }
  ],
  "total": 520.80,
  "currency": "EUR"
}
```

---

### 3.5 Project Registry (part of Reporting API)

Projects are first-class entities. Each platform registers its projects; cross-platform dependencies are modeled as directed, metadata-enriched relations between projects.

**Design**: No virtual or meta-projects at this stage. Only real projects from real platforms, linked by typed relations. The schema is designed so that grouping entities (e.g. meta-projects for customers) can be introduced later as an additional `relation_type` (e.g. `member_of`) without schema changes.

**Temporal validity**: Relations carry `valid_from` / `valid_to` and are **closed, never hard-deleted**. Metering and rating resolve the graph as of the billing period being processed — deleting a shoot in April does not detach its March infrastructure costs, and re-processing past periods is always reproducible.

**Project relation sync**: Event-driven (e.g. on `shoot.create.end`, the infrastructure tenant is automatically registered and linked).

#### Data Model (PostgreSQL, same database as events)

```sql
CREATE TABLE projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    platform    TEXT NOT NULL,          -- platform type: 'openstack', 'hetzner', 'gardener', ...
    cloud       TEXT NOT NULL,          -- concrete installation: 'os-prod-eu1', ...
    external_id TEXT NOT NULL,          -- project_id in the respective installation
    name        TEXT,                   -- optional, human-readable
    metadata    JSONB DEFAULT '{}',     -- extensible, intentionally schema-free
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (cloud, external_id)         -- external IDs are only unique per installation
);

CREATE TABLE project_relations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id       UUID NOT NULL REFERENCES projects(id),
    target_id       UUID NOT NULL REFERENCES projects(id),
    relation_type   TEXT NOT NULL,      -- e.g. 'infrastructure_tenant', 'same_owner'
    metadata        JSONB DEFAULT '{}', -- enrichment: e.g. shoot name, context, timeframe
    valid_from      TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to        TIMESTAMPTZ,        -- NULL = active; relations are never hard-deleted
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- at most one active relation per (source, target, type)
CREATE UNIQUE INDEX uq_relations_active
    ON project_relations (source_id, target_id, relation_type)
    WHERE valid_to IS NULL;

CREATE INDEX idx_relations_source ON project_relations (source_id);
CREATE INDEX idx_relations_target ON project_relations (target_id);
```

#### API Endpoints

```
# Project CRUD
POST   /api/v1/projects
GET    /api/v1/projects?platform=...
GET    /api/v1/projects/{id}
PATCH  /api/v1/projects/{id}

# Project relations (with metadata)
POST   /api/v1/projects/{id}/relations
       Body: { "target_id": "...", "relation_type": "infrastructure_tenant",
               "metadata": { "shoot_name": "abc", "created_by": "gardener-controller" } }
GET    /api/v1/projects/{id}/relations?direction=outgoing|incoming|both&relation_type=...
PATCH  /api/v1/projects/{id}/relations/{relation_id}
DELETE /api/v1/projects/{id}/relations/{relation_id}
       -- closes the relation (sets valid_to = now); never hard-deletes

# Resolve: all transitively related projects (cycle-safe traversal)
GET    /api/v1/projects/{id}/related?depth=1&relation_type=infrastructure_tenant&at=2026-03-01T00:00:00Z
       -- 'at' (optional) resolves the graph as of a point in time (default: now)
```

#### Example: Gardener ↔ OpenStack

```
OpenStack Project "team-alpha-os"    (customer's direct VMs)
Gardener Project  "team-alpha"       (managed Kubernetes)
  └─ infrastructure_tenant → OpenStack Project "shoot-abc-123"
  └─ infrastructure_tenant → OpenStack Project "shoot-def-456"

Cross-platform ownership:
  "team-alpha-os" ←same_owner→ "team-alpha"
```

#### Future Extension: Meta-Projects (no schema change required)

```
Meta-Project "Customer Alpha"  (new entry in projects, platform="meta")
  "team-alpha-os"   ─member_of→ "Customer Alpha"
  "team-alpha"      ─member_of→ "Customer Alpha"
```

**Note**: Project-to-resource mapping is NOT modeled here. Resources reference their `project_id` via metrics and events as before. The Project Registry only models project-to-project relations.

---

## 4. Provider: OpenStack (Reference Implementation)

OpenStack serves as the first fully implemented provider. This section describes the OpenStack-specific components that feed into the platform-agnostic core.

### 4.1 Metrics: Ceilometer (existing, pipeline configuration required)

Ceilometer's **Polling Agent** collects runtime metrics at 300s intervals (trade-off accuracy vs. load):
- CPU utilization, vCPU time
- RAM usage
- Disk I/O, disk size
- Network I/O (ingress/egress)
- Instance uptime

#### Metrics Transport

OTel Collector as universal middleware for all providers, forwarding to VictoriaMetrics via
Remote Write. **Verify per deployed Ceilometer version** whether a native OpenTelemetry/OTLP
publisher is available; if not, use Ceilometer's `prometheus`/`http` publisher with a matching
OTel Collector receiver as translation layer.

### 4.2 Events: openstack-event-collector (new, to be implemented)

Ceilometer's notification pipeline publishes its own sample/notification format — it cannot emit
the Tally event schema directly. Instead, a dedicated lightweight **openstack-event-collector**
service implements the provider event-collector role:

- **Consumes notifications directly from oslo.messaging** (the same bus Ceilometer listens on):
  - `compute.instance.create.end` / `delete.end` / `resize.end`
  - `compute.instance.shelve` / `unshelve`
  - `compute.instance.power_on` / `power_off`
  - `volume.create.end` / `volume.delete.end` / `volume.resize.end` / `volume.retype`
  - `volume.transfer.accept.end` (ownership change → project split)
  - `floatingip.create.end` / `floatingip.delete.end`
  - `image.create` / `image.delete`
  - etc.
- **Transforms** them into the Tally event schema; uses the oslo.messaging `message_id` as
  `event_id`
- **Buffers** events on disk and delivers them to the Reporting API with retries
  (at-least-once)

### 4.3 OpenStack DB Exporter

Standalone service that reads directly from OpenStack databases and exposes metrics in Prometheus exposition format (compatible with VictoriaMetrics scraping).

**Existing implementation**: [openstack_database_exporter](https://github.com/vexxhost/openstack_database_exporter) by VEXXHOST – evaluate this existing open-source exporter first, extend if necessary.

**Purpose**: Capture inventory and state information that goes beyond Ceilometer metrics.

**Deployment**: On the control plane as a pod alongside the OpenStack services.

**Database access**: Read-only user on the respective service databases.

**Example metrics**:

```
# Instance inventory
openstack_nova_instances{project_id="...", state="active", flavor="m1.large"} 5
openstack_nova_instances{project_id="...", state="shutoff", flavor="m1.small"} 2

# Flavor information (info metric)
openstack_nova_flavor_info{flavor_id="...", name="m1.large", vcpus="4", ram_mb="8192", disk_gb="80"} 1

# Volumes
openstack_cinder_volumes{project_id="...", status="in-use", type="ssd"} 10
openstack_cinder_volume_size_gb{project_id="...", volume_id="..."} 100

# Network
openstack_neutron_floating_ips{project_id="...", status="ACTIVE"} 3
openstack_neutron_ports{project_id="...", status="ACTIVE"} 12
openstack_neutron_routers{project_id="..."} 2

# Quotas
openstack_nova_quota_instances{project_id="..."} 50
openstack_nova_quota_cores{project_id="..."} 100
openstack_nova_quota_ram_mb{project_id="..."} 204800

# Keystone
openstack_keystone_projects_total 42
```

**Implementation details**:
- Technology: Python or Go
- Configurable: Which OpenStack services/databases to connect to
- Polling interval: Configurable (recommendation: 60s)
- Health/ready endpoints for Kubernetes

**Databases to export from**:

| Service | Database | Relevant Tables |
|---------|----------|-----------------|
| Nova | nova, nova_api | instances, flavors, quotas |
| Neutron | neutron | ports, floatingips, routers, networks, subnets |
| Cinder | cinder | volumes, snapshots, quotas |
| Keystone | keystone | projects |
| Glance | glance | images |
| Octavia | octavia | load_balancers, listeners, pools |

### 4.4 OpenStack Reconciliation Adapter

The reconciliation adapter for OpenStack polls the OpenStack APIs (Nova, Cinder, Neutron, Keystone, Glance, Octavia) and compares the result against `current_resources` for the respective `cloud` installation.

**Trigger**: CronJob, recommended every 10 minutes.

**APIs polled**:
- Nova: `GET /servers` (instances)
- Cinder: `GET /volumes` (volumes, snapshots)
- Neutron: `GET /v2.0/floatingips`, `GET /v2.0/routers`
- Glance: `GET /v2/images`
- Octavia: `GET /v2/lbs`

### 4.5 OpenStack Architecture Diagram

```
┌────────────────────────────────────────────────────────────────────────────┐
│                         OPENSTACK CONTROL PLANE                            │
│                                                                            │
│  ┌──────────────────────────────┐         ┌─────────────────────────────┐  │
│  │ OpenStack Services           │         │ OpenStack DB Exporter       │  │
│  │ (Nova, Neutron, Cinder, ...) │         │ (read-only access to        │  │
│  │                              │◀────────┤  Nova/Neutron/Cinder/       │  │
│  │  APIs · DBs · oslo.messaging │         │  Keystone/... DBs)          │  │
│  └──────┬───────────────┬───────┘         └──────────────┬──────────────┘  │
│         │ polling       │ notifications                  │                 │
│         ▼               ▼                                │                 │
│  ┌───────────────┐  ┌─────────────────────────┐          │                 │
│  │ Ceilometer    │  │ openstack-event-        │          │                 │
│  │ Polling Agent │  │ collector               │          │                 │
│  │ (metrics only)│  │ (oslo notifications →   │          │                 │
│  │               │  │  Tally event schema,    │          │                 │
│  │               │  │  buffered delivery)     │          │                 │
│  └──────┬────────┘  └───────────┬─────────────┘          │                 │
│         │                       │                        │                 │
└─────────┼───────────────────────┼────────────────────────┼─────────────────┘
          │ OTLP                  │ POST /api/v1/events    │ /metrics
          │                       │                        └──▶ scraped by
          ▼                       ▼                             VictoriaMetrics
  ┌────────────────┐    ┌──────────────────────┐
  │ OTel Collector │    │ Reporting API        │───▶ reconciliation (CronJob)
  │ (OTLP in,      │    │ (event store,        │     polls OpenStack APIs,
  │  Remote Write  │    │  reconciliation,     │◀─── writes synthetic events
  │  out)          │    │  project registry)   │
  └───────┬────────┘    └──────────┬───────────┘
          │ Remote Write           │
          ▼                        ▼
  ┌─────────────────┐    ┌──────────────────────┐
  │ VictoriaMetrics │    │ PostgreSQL +         │
  │ (also scrapes   │    │ TimescaleDB          │
  │  DB Exporter &  │    │ (events,             │
  │  Reporting API  │    │  current_resources,  │
  │  /metrics)      │    │  projects)           │
  └────────┬────────┘    └──────────┬───────────┘
           │                        │
           ▼                        ▼
  ┌─────────────────────────────────────────────┐
  │              Metering + Rating              │
  └─────────────────────────────────────────────┘
```

---

## 5. Future Providers

### 5.1 Hetzner Cloud

Hetzner Cloud provides a REST API for all resource management. Integration follows the standard provider pattern.

**Metrics Exporter**: Polls the [Hetzner Cloud API](https://docs.hetzner.cloud/) for servers, volumes, floating IPs, load balancers, and exposes them as Prometheus metrics.

**Event Collector**: Hetzner provides an [Actions API](https://docs.hetzner.cloud/#actions) that tracks all resource changes. The event collector periodically polls this API and forwards events to the Reporting API. The Hetzner action ID serves as `event_id`; the collector persists its polling cursor so restarts neither lose nor duplicate actions.

**Resource Types**:

| Resource Type | `size` Fields | Key Events |
|---------------|--------------|------------|
| `server` | `vcpus`, `ram_gb`, `disk_gb`, `server_type` | create, delete, upgrade, downgrade, power on/off |
| `volume` | `size_gb` | create, delete, resize |
| `floating_ip` | `ip_version`, `type` | create, delete |
| `load_balancer` | `type`, `targets` | create, delete, add/remove target |

**Reconciliation Adapter**: Polls `GET /servers`, `GET /volumes`, `GET /floating_ips`, `GET /load_balancers`.

### 5.2 STACKIT

STACKIT provides OpenStack-compatible APIs for compute and block storage plus proprietary APIs for managed services.

**Metrics Exporter**: Polls STACKIT APIs for servers, volumes, and managed services. For OpenStack-compatible resources, the OpenStack DB Exporter can potentially be reused.

**Event Collector**: Polls STACKIT's audit/activity APIs for resource lifecycle events.

**Resource Types**:

| Resource Type | `size` Fields | Key Events |
|---------------|--------------|------------|
| `server` | `vcpus`, `ram_gb`, `disk_gb`, `machine_type` | create, delete, resize, power on/off |
| `volume` | `size_gb`, `type` | create, delete, resize |
| `database` | `type`, `flavor`, `storage_gb` | create, delete, resize |
| `kubernetes_cluster` | `node_count`, `machine_type` | create, delete, scale |

### 5.3 IONOS Cloud

IONOS Cloud provides a REST API and Terraform provider for infrastructure management.

**Metrics Exporter**: Polls the [IONOS Cloud API](https://api.ionos.com/docs/cloud/v6/) for data centers, servers, volumes, and network resources.

**Event Collector**: Polls the IONOS request/audit API for resource lifecycle changes.

**Resource Types**:

| Resource Type | `size` Fields | Key Events |
|---------------|--------------|------------|
| `server` | `cores`, `ram_gb`, `type` | create, delete, resize |
| `volume` | `size_gb`, `type`, `bus` | create, delete, resize |
| `nic` | `lan_id`, `firewall_active` | create, delete |
| `managed_kubernetes` | `node_count`, `cores`, `ram_gb` | create, delete, scale |

### 5.4 Gardener (Service Integration)

Gardener manages Kubernetes clusters across multiple cloud platforms. It demonstrates cross-platform project relations (Gardener project → infrastructure tenant on OpenStack/Hetzner/STACKIT/IONOS).

**Exporter**:

```
gardener_shoot_info{project="...", name="...", kubernetes_version="1.29", infrastructure="openstack"} 1
gardener_shoot_worker_count{project="...", name="...", pool="workers"} 3
gardener_shoot_worker_machine_type{project="...", name="...", pool="workers", type="m1.xlarge"} 1
gardener_shoot_status{project="...", name="...", status="healthy"} 1
```

**Event Collector**:

```
shoot.create.end
shoot.delete.end
shoot.hibernate.start
shoot.hibernate.end
shoot.worker.scale                  -- worker count change → metering split
shoot.worker.machine_type_change    -- machine type change → metering split
```

**Project Registration (event-driven)**: When a Shoot cluster is created (`shoot.create.end`), the event collector automatically:

1. **Registers the infrastructure tenant** as a new project in the Project Registry (on whichever platform the Shoot uses):
   ```
   POST /api/v1/projects
   { "platform": "openstack", "external_id": "shoot-abc-os-tenant",
     "name": "Infrastructure tenant for shoot-abc" }
   ```

2. **Creates an `infrastructure_tenant` relation** from the Gardener project to the new infrastructure project:
   ```
   POST /api/v1/projects/{gardener-project-id}/relations
   { "target_id": "{new-infra-project-id}",
     "relation_type": "infrastructure_tenant",
     "metadata": { "shoot_name": "shoot-abc", "created_by": "gardener-controller" } }
   ```

3. On `shoot.delete.end`, the relation is **closed** (`valid_to` = deletion time). Neither the relation nor the project entry is deleted — past billing periods must remain attributable and reproducible.

This ensures that infrastructure costs from any cloud platform are always attributable to the originating Gardener project.

### 5.5 Harbor (Service Integration)

Harbor is a container registry with project-based access control. It demonstrates the model's support for **counter-based** usage metrics (pulls, pushes, traffic) alongside time-based resource tracking.

**Exporter**:

```
harbor_repository_info{project="...", name="app", tags="47"} 1
harbor_repository_storage_bytes{project="...", name="app"} 13421772800
harbor_project_quota_storage_bytes{project="..."} 107374182400
harbor_project_pull_total{project="..."} 1523
harbor_project_push_total{project="..."} 70
```

**Event Collector**:

```
repository.push                     -- image pushed → storage_gb may change → metering split
repository.delete                   -- image/tag deleted → storage_gb may change → metering split
repository.pull                     -- image pulled → counter metric (no split, aggregated per period)
project.create / project.delete     -- project lifecycle
```

**Project Registration**: Harbor projects are registered in the Project Registry like any other platform:

```
POST /api/v1/projects
{ "platform": "harbor", "external_id": "team-alpha",
  "name": "Team Alpha container registry" }
```

If the Harbor project stores images used by a Gardener Shoot, a relation can be established:

```
POST /api/v1/projects/{harbor-project-id}/relations
{ "target_id": "{gardener-project-id}",
  "relation_type": "image_source",
  "metadata": { "description": "Provides container images for shoot workloads" } }
```

---

## 6. Phased Roadmap

### Phase 1 – Core Platform + OpenStack Provider (Foundation)

| Task | Description |
|------|-------------|
| Reporting API (MVP) | Idempotent event ingestion (`event_id` dedup, batches), `current_resources` projection with replay, basic query endpoints, ingest validation + dead letter, provider-agnostic reconciliation framework |
| AuthN/AuthZ | Per-provider ingest credentials scoped to `(platform, cloud)`, project-scoped RBAC for queries, audit log |
| Resource type registry | JSON Schema registration per resource type; `size` validation at ingest |
| Project Registry | Project + relation tables (temporal validity) in Reporting API DB, CRUD endpoints |
| OTel Collector | Deploy as universal metrics ingestion layer (OTLP in, Remote Write to VictoriaMetrics) |
| VictoriaMetrics | Set up with initial scrape configs |
| OpenStack DB Exporter | Evaluate [vexxhost/openstack_database_exporter](https://github.com/vexxhost/openstack_database_exporter), adapt if needed, and deploy on the control plane |
| OpenStack Ceilometer pipeline | Configure metrics pipeline: Ceilometer → OTel Collector → VictoriaMetrics |
| openstack-event-collector | New service: oslo.messaging notifications → Tally event schema → Reporting API (buffered, at-least-once) |
| OpenStack reconciliation adapter | Implement adapter that polls OpenStack APIs |
| Vertical slice | One resource type (OpenStack instance) end-to-end: event → usage record → rated record (prototype of the Phase 3 engines) — validates event schema and splitting rules before broad rollout |

### Phase 2 – Reporting & Dashboards

| Task | Description |
|------|-------------|
| Extend Reporting API | Lifecycle queries, aggregations, `/metrics` endpoint |
| Grafana dashboards | On VictoriaMetrics data: resource overview, usage trends (multi-platform) |
| Alerting | Anomaly detection (sudden resource spikes, etc.) |

### Phase 3 – Metering & Rating

| Task | Description |
|------|-------------|
| Metering Engine | Platform-agnostic usage records per resource per billing period (size/state/project splitting, invariant checks, golden tests) |
| Billing period lifecycle | Grace period, run versioning, finalization, correction runs with delta records |
| Rating Engine | Apply versioned pricing model to usage records → rated usage data with costs (decimal arithmetic, normative rounding) |
| Pricing model | Define and make configurable per platform, with versioning and validity periods |
| Integration | Connect to external billing/ERP system (if applicable), including the correction/credit-note flow |

### Phase 4 – Additional Providers & Services

| Task | Description |
|------|-------------|
| Hetzner provider | Metrics exporter + event collector + reconciliation adapter |
| STACKIT provider | Metrics exporter + event collector + reconciliation adapter |
| IONOS provider | Metrics exporter + event collector + reconciliation adapter |
| Gardener integration | Exporter + event collector following the provider pattern |
| Harbor integration | Exporter + event collector following the provider pattern |
| Additional provider OTel integration | Onboard new providers into shared OTel Collector |

### Phase 5 – Commercial Pricing & Partner Models

| Task | Description |
|------|-------------|
| Meta-Projects | Introduce `member_of` relations for grouping projects under customers |
| Reseller relations | New `managed_by` relation type linking projects to reseller/partner entities |
| Relation-based pricing adjustments | Rating engine resolves `pricing_adjustments` from relation metadata (discounts, kickbacks, surcharges) |
| Kickback reporting | Separate output for partner commissions, aggregated per reseller and billing period |
| Volume/loyalty discounts | Project-specific or customer-group discounts via `member_of` relation metadata |


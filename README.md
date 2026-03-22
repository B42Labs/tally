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
- **Metering separated from rating**: Usage is recorded in neutral units (minutes, counts, sizes) first; pricing is applied as a separate step

---

## 2. Architecture

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                       CLOUD PLATFORM (Provider)                             │
│                                                                             │
│  Any cloud platform: OpenStack, Hetzner, STACKIT, IONOS, ...                │
│                                                                             │
│  ┌────────────────────┐     ┌────────────────────┐                          │
│  │  Platform-specific │     │  Platform-specific  │                         │
│  │  Metrics Exporter  │     │  Event Collector    │                         │
│  │                    │     │                     │                         │
│  │  Exposes /metrics  │     │  Sends lifecycle    │                         │
│  │  (Prometheus fmt)  │     │  events via HTTP    │                         │
│  └────────┬───────────┘     └──────────┬──────────┘                         │
│           │                            │                                    │
└───────────┼────────────────────────────┼────────────────────────────────────┘
            │ /metrics                   │ POST /events
            ▼                            ▼
  ┌──────────────────┐        ┌────────────────────┐
  │ VictoriaMetrics  │        │  Reporting API     │
  │                  │        │                    │
  │  scrape ─────────┼───────▶│  POST /events      │
  │                  │        │  GET  /resources   │
  │  Central metrics │        │  GET  /events      │
  │  store           │        │  GET  /metrics ──┐ │
  └────────┬─────────┘        └───────┬──────────┘ │
           │                          │     ▲      │
           │                          ▼     │      │
           │                   ┌──────────────────┐│
           │                   │ PostgreSQL +     ││
           │                   │ TimescaleDB      ││
           │◀──────────────────┤ (events,         ││
           │   scrape          │  current_resources│
           │                   │  projects)       ││
           └────────┬──────────└──────┬───────────┘│
                    │                 │    ▲       │
                    │                 │    │ reconciliation (CronJob)
                    │                 │    │ polls platform APIs
                    ▼                 ▼    │
           ┌─────────────────────────────────────┐
           │        Metering + Rating            │
           │                                     │
           │  1. Metering: usage in minutes      │
           │  2. Rating: apply pricing model     │
           │  → generate billing data            │
           └─────────────────────────────────────┘
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
platform="openstack|hetzner|stackit|ionos|..."
resource_type="instance|volume|server|..."
project_id="<project-identifier>"
resource_id="<resource-identifier>"
```

#### Event Schema (mandatory for all providers)

All events sent to the Reporting API must contain the following fields:

```json
{
  "timestamp":     "ISO 8601",
  "event_type":    "resource.action[.phase]",
  "platform":      "openstack|hetzner|stackit|ionos|...",
  "resource_type": "instance|volume|server|...",
  "resource_id":   "UUID or unique identifier",
  "project_id":    "project identifier",
  "payload":       {}
}
```

#### Checklist for New Provider Integration

1. **Implement metrics exporter** – Expose platform-specific metrics with unified label schema
2. **Implement event collector** – Send lifecycle events to Reporting API
3. **Implement reconciliation adapter** – Enable the Reporting API to poll the platform's API for drift detection
4. **Register projects & relations** – Register platform projects in the Project Registry; define relations to dependent infrastructure projects
5. **Define resource types** – Document `size` schema per resource type
6. **Define pricing model** – Create pricing configuration for the platform's resource types
7. **Add scrape config** – Extend VictoriaMetrics with new scrape target

---

### 3.2 Reporting API (new, to be implemented)

REST API with three roles:

1. **Event sink**: Receives events from any provider's event collector
2. **Query interface**: Enables querying the event history per resource
3. **Reconciliation**: Periodically syncs against platform APIs (via provider-specific adapters) to catch missed events and correct drift

#### API Endpoints

```
# Event ingestion
POST /api/v1/events
  Body: { "event_type": "compute.instance.create.end",
          "timestamp": "2026-03-20T10:15:00Z",
          "platform": "openstack",
          "resource_type": "instance",
          "resource_id": "abc-123",
          "project_id": "proj-456",
          "payload": { ... } }

# Event queries
GET  /api/v1/resources/{resource_id}/events
GET  /api/v1/events?project_id=...&resource_type=...&from=...&to=...

# Resource lifecycle
GET  /api/v1/resources/{resource_id}/lifecycle
  → Returns the complete lifecycle (create → resize → delete)

# Resource inventory (current state derived from events)
GET  /api/v1/resources?project_id=...&resource_type=...&status=active

# Reconciliation (internal, called by CronJob)
POST /internal/sync/{platform}     -- polls platform APIs, reconciles current_resources

# Health
GET  /healthz
GET  /readyz
  → Readiness fails if DB is unreachable or if both event ingestion
    and API sync are unavailable simultaneously
  → Liveness fails if unhealthy for longer than configurable threshold (default: 600s)

# Metrics (optional, for VictoriaMetrics scraping)
GET  /metrics
  → Exposes e.g. event_count{platform, resource_type, event_type}
  → sync_resources_reconciled{platform}, sync_errors{platform}
  → As well as runtime aggregations derivable from events
```

**Events retention**: Unlimited (legally relevant).

#### Data Model (PostgreSQL + TimescaleDB)

```sql
-- Events table (TimescaleDB hypertable with compression)
CREATE TABLE events (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    timestamp       TIMESTAMPTZ NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_type      TEXT NOT NULL,
    platform        TEXT NOT NULL,          -- 'openstack', 'hetzner', 'stackit', 'ionos', ...
    resource_type   TEXT NOT NULL,          -- 'instance', 'volume', 'server', ...
    resource_id     TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    payload         JSONB
);

-- Convert to TimescaleDB hypertable for automatic partitioning + compression
SELECT create_hypertable('events', 'timestamp');

-- Indexes for typical queries
CREATE INDEX idx_events_resource ON events (resource_id, timestamp);
CREATE INDEX idx_events_project  ON events (project_id, timestamp);
CREATE INDEX idx_events_type     ON events (event_type, timestamp);

-- Current resource state (real table, updated on every event and by reconciliation)
-- Unlike a materialized view, this is always up-to-date and can be used for billing.
CREATE TABLE current_resources (
    resource_id     TEXT PRIMARY KEY,
    resource_type   TEXT NOT NULL,          -- 'instance', 'volume', 'server', ...
    platform        TEXT NOT NULL,          -- 'openstack', 'hetzner', 'stackit', 'ionos', ...
    project_id      TEXT NOT NULL,
    state           TEXT NOT NULL,          -- 'active', 'shutoff', 'shelved', 'deleted', ...
    size            JSONB DEFAULT '{}',     -- resource-specific: {"vcpus": 4, "ram_gb": 8, "disk_gb": 80}
    created_at      TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    last_event_type TEXT NOT NULL,
    last_event_at   TIMESTAMPTZ NOT NULL,
    last_payload    JSONB
);

CREATE INDEX idx_current_resources_project ON current_resources (project_id);
CREATE INDEX idx_current_resources_type    ON current_resources (resource_type, state);
```

The `size` JSONB field stores resource-specific dimensions. Any change to `size` or `state` triggers a metering split (see Metering & Rating Engine). Examples per resource type:

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

#### Reconciliation

The Reporting API periodically polls platform APIs (via CronJob, recommended: every 10 minutes) using provider-specific reconciliation adapters. Each adapter knows how to list resources from its platform and compare against `current_resources`. Differences are resolved:

| Situation | Action |
|-----------|--------|
| Resource exists in platform API but not in DB | Insert into `current_resources`, generate synthetic create event |
| Resource exists in DB but not in platform API | Mark as deleted, generate synthetic delete event |
| Resource attributes differ (e.g. flavor after resize) | Update `current_resources`, generate synthetic resize event |

This dual-ingestion pattern (real-time events + periodic sync) ensures no resource is missed, even if event notifications are lost.

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
- Multi-tenancy (Enterprise feature)

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

#### Phase 1: Metering

Calculates resource usage per monthly billing period. Usage is captured as a **`usage` JSONB object** with metric-specific fields. This supports three usage types:

| Usage Type | Description | Example Metrics |
|-----------|-------------|-----------------|
| **Time-based** | Resource exists for a duration | `minutes` (always present for time-based resources) |
| **Gauge-based** | Resource has a measurable size/quantity at a point in time | `storage_gb`, `worker_count`, `vcpus` |
| **Counter-based** | Accumulated events/volume within the billing period | `pulls`, `pushes`, `egress_gb`, `api_calls` |

A single usage record can combine all three types. Time-based and gauge-based metrics come from `current_resources` and lifecycle events; counter-based metrics are aggregated from the events table or VictoriaMetrics.

**Flow:**

1. **Resolve project graph**: Query Project Registry for related projects (e.g. Gardener project → all infrastructure tenants on any platform)
2. **Determine resources**: Query `current_resources` for all resources of the project and its related projects within the billing period
3. **Calculate time + gauge usage**: For each resource, calculate `minutes = end_time - start_time` within the billing period, carry forward `size` dimensions from `current_resources`
4. **Aggregate counter usage**: Query events table / VictoriaMetrics for accumulated metrics (pulls, traffic, etc.) within the billing period

**Generic splitting rule**: Any change to a resource's `size` or `state` in `current_resources` triggers a split. The metering creates **two records** at the change timestamp – one for the old configuration up to T, one for the new configuration from T onwards. This applies uniformly to all resource types across all platforms:

| Platform | Resource Type | Change Event | Split Trigger |
|----------|---------------|-------------|---------------|
| OpenStack | Instance | `compute.instance.resize.end` | `size` changes (vcpus, ram_gb, disk_gb, flavor) |
| OpenStack | Instance | `compute.instance.shelve` / `unshelve` | `state` changes (active → shelved → active) |
| OpenStack | Instance | `compute.instance.power_off` / `power_on` | `state` changes (active → shutoff → active) |
| OpenStack | Volume | `volume.resize.end` | `size.size_gb` changes (e.g. 100 → 200) |
| OpenStack | Volume | `volume.retype` | `size.type` changes (e.g. ssd → hdd) |
| Hetzner | Server | `server.upgrade` / `server.downgrade` | `size` changes (server_type) |
| STACKIT | Server | `server.resize` | `size` changes (machine_type) |
| Gardener | Shoot | `shoot.worker.scale` | `size.worker_count` changes (e.g. 3 → 5) |
| Gardener | Shoot | `shoot.hibernate.start` / `end` | `state` changes (active → hibernated → active) |
| Harbor | Repository | `repository.push` / image delete | `size.storage_gb` changes |

**Concurrency protection**: PostgreSQL advisory locks per resource prevent duplicate metering records when events and reconciliation sync run concurrently.

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
        "minutes": 21600,
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
        "minutes": 23040,
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
      "usage": { "minutes": 15840, "size_gb": 200, "type": "hdd" }
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
      "usage": { "minutes": 4320, "worker_count": 5, "machine_type": "m1.xlarge" }
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
        "minutes": 18720,
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

1. **Load pricing model**: Read configurable prices per platform, resource type, usage metric, and state
2. **Calculate costs per dimension**: Each dimension references a key from the `usage` object. The `state_modifier` (from pricing config, default 1.0) scales time-based costs by state:
   - Time × gauge: `cost = (usage.minutes / 60) × usage.{gauge_metric} × price_per_unit_per_hour × state_modifier`
   - Counter-based: `cost = usage.{counter_metric} × price_per_unit` (not affected by state_modifier)
3. **Aggregate**: Sum per resource, per project, and across related projects
4. **Generate output**: Structured billing data with related costs attributed to source project

#### Pricing Model (configurable, per platform)

Each dimension references a metric key from the `usage` object. The `type` field determines the formula:

```yaml
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
      type_modifiers:                  # modifier per volume type
        ssd: 1.0
        hdd: 0.5
    floating_ip:
      dimensions:
        - metric: "count"
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
3. **Apply adjustments**: Calculate discounts, surcharges, and kickbacks; generate separate line items

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
- **Lifecycle**: When a reseller relationship ends (relation deleted), the pricing adjustment disappears automatically — no orphaned config
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

**Note**: A VM that runs the entire month at full price would cost 144.00 EUR. With 10 days powered off (50% modifier), the total drops to 128.45 EUR. A shelved VM (`state_modifier: 0.0`) would cost 0 EUR for the shelved period.

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
      "description": "Shoot cluster shoot-abc",
      "periods": [
        { "state": "active", "hours": 720, "usage": { "worker_count": 3 },
          "cost": { "worker_count": 216.00, "total": 216.00 }, "state_modifier": 1.0 }
      ],
      "total": 216.00
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
            { "state": "active", "hours": 720,
              "usage": { "vcpus": 8, "ram_gb": 16, "disk_gb": 160 },
              "cost": { "vcpus": 115.20, "ram_gb": 57.60, "disk_gb": 115.20, "total": 288.00 },
              "state_modifier": 1.0 }
          ],
          "total": 288.00
        }
      ],
      "total": 288.00
    }
  ],
  "total": 504.00,
  "currency": "EUR"
}
```

---

### 3.5 Project Registry (part of Reporting API)

Projects are first-class entities. Each platform registers its projects; cross-platform dependencies are modeled as directed, metadata-enriched relations between projects.

**Design**: No virtual or meta-projects at this stage. Only real projects from real platforms, linked by typed relations. The schema is designed so that grouping entities (e.g. meta-projects for customers) can be introduced later as an additional `relation_type` (e.g. `member_of`) without schema changes.

**Project relation sync**: Event-driven (e.g. on `shoot.create.end`, the infrastructure tenant is automatically registered and linked).

#### Data Model (PostgreSQL, same database as events)

```sql
CREATE TABLE projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    platform    TEXT NOT NULL,          -- 'openstack', 'hetzner', 'stackit', 'ionos', 'gardener', ...
    external_id TEXT NOT NULL,          -- project_id in the respective platform
    name        TEXT,                   -- optional, human-readable
    metadata    JSONB DEFAULT '{}',     -- extensible, intentionally schema-free
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (platform, external_id)
);

CREATE TABLE project_relations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id       UUID NOT NULL REFERENCES projects(id),
    target_id       UUID NOT NULL REFERENCES projects(id),
    relation_type   TEXT NOT NULL,      -- e.g. 'infrastructure_tenant', 'same_owner'
    metadata        JSONB DEFAULT '{}', -- enrichment: e.g. shoot name, context, timeframe
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (source_id, target_id, relation_type)
);

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

# Resolve: all transitively related projects
GET    /api/v1/projects/{id}/related?depth=1&relation_type=infrastructure_tenant
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

### 4.1 Ceilometer (existing, pipeline configuration required)

Ceilometer is the central data collection component in OpenStack.

**Polling Agent** – Collects metrics at 300s intervals (trade-off accuracy vs. load):
- CPU utilization, vCPU time
- RAM usage
- Disk I/O, disk size
- Network I/O (ingress/egress)
- Instance uptime

**Notification Agent** – Listens on oslo.messaging for events:
- `compute.instance.create.end`
- `compute.instance.delete.end`
- `compute.instance.resize.end`
- `compute.instance.shelve` / `unshelve`
- `compute.instance.power_on` / `power_off`
- `volume.create.end` / `volume.delete.end`
- `volume.resize.end`
- `volume.retype`
- `floatingip.create.end` / `floatingip.delete.end`
- `image.create` / `image.delete`
- etc.

**Pipeline configuration** – Two outputs:

| Data Type | Target | Transport |
|-----------|--------|-----------|
| Metrics | VictoriaMetrics | OTel Collector |
| Events | Reporting API | HTTP POST |

#### Metrics Transport

OTel Collector as universal middleware for all providers. Ceilometer sends metrics via OTLP to the OTel Collector, which forwards them to VictoriaMetrics via Remote Write.

### 4.2 OpenStack DB Exporter

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

### 4.3 OpenStack Reconciliation Adapter

The reconciliation adapter for OpenStack polls the OpenStack APIs (Nova, Cinder, Neutron, Keystone, Glance, Octavia) and compares the result against `current_resources` where `platform = 'openstack'`.

**Trigger**: CronJob, recommended every 10 minutes.

**APIs polled**:
- Nova: `GET /servers` (instances)
- Cinder: `GET /volumes` (volumes, snapshots)
- Neutron: `GET /v2.0/floatingips`, `GET /v2.0/routers`
- Glance: `GET /v2/images`
- Octavia: `GET /v2/lbs`

### 4.4 OpenStack Architecture Diagram

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    OPENSTACK CONTROL PLANE                              │
│                                                                         │
│  ┌───────────────┐     ┌─────────────────────┐     ┌──────────────────┐ │
│  │  OpenStack    │     │    Ceilometer       │     │  OpenStack DB    │ │
│  │  Services     │     │                     │     │  Exporter        │ │
│  │  (Nova,       │────▶│  Polling Agent      │     │                  │ │
│  │   Neutron,    │     │  Notification Agent │     │  ┌─────────────┐ │ │
│  │   Cinder ...) │     │                     │     │  │ Nova DB     │ │ │
│  │              ─┼─────┼──────────┐          │     │  │ Neutron DB  │ │ │
│  │  ┌─────────┐  │     │          │          │     │  │ Cinder DB   │ │ │
│  │  │   DBs   │──┼─────┼──────────┼──────────┼─────┼─▶│ Keystone DB │ │ │
│  │  └─────────┘  │     │          │          │     │  └─────────────┘ │ │
│  └───────────────┘     └────┬─────┴────┬─────┘     └─────────┬────────┘ │
│                             │ Metrics  │ Events              │          │
└─────────────────────────────┼──────────┼─────────────────────┼──────────┘
                              │ OTLP     │                     │ /metrics
                              ▼          ▼                     │
                    ┌──────────────┐  ┌────────────────────┐   │
                    │OTel Collector│  │  Reporting API     │   │
                    │              │  │                    │   │
                    │  OTLP in     │  │  POST /events      │   │
                    │  Remote Write│  │  GET  /resources   │   │
                    │  out         │  │  GET  /events      │   │
                    └──────┬───────┘  │  GET  /metrics ───┐│   │
                           │          └───────┬───────────┘│   │
                           │                  │     ▲      │   │
                           ▼                  ▼     │      │   │
                    ┌─────────────────┐  ┌──────────────────┐  │   │
                    │ VictoriaMetrics │  │ PostgreSQL +     │  │   │
                    │                 │  │ TimescaleDB      │  │   │
                    │                 │◀─┤ (events,         │  │   │
                    │  scrape ────────┼──┤  current_resources  │   │
                    │  scrape ────────┼──┤  projects)       │  │   │
                    └────────┬────────┘  └──────┬───────────┘  │   │
                             │                  │    ▲         │   │
                             │                  │    │ reconciliation (CronJob)
                             │                  │    │ polls OpenStack APIs
                             ▼                  ▼    │
                    ┌─────────────────────────────────────┐
                    │        Metering + Rating            │
                    └─────────────────────────────────────┘
```

---

## 5. Future Providers

### 5.1 Hetzner Cloud

Hetzner Cloud provides a REST API for all resource management. Integration follows the standard provider pattern.

**Metrics Exporter**: Polls the [Hetzner Cloud API](https://docs.hetzner.cloud/) for servers, volumes, floating IPs, load balancers, and exposes them as Prometheus metrics.

**Event Collector**: Hetzner provides an [Actions API](https://docs.hetzner.cloud/#actions) that tracks all resource changes. The event collector periodically polls this API and forwards events to the Reporting API.

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

3. On `shoot.delete.end`, the relation (and optionally the project entry) is removed.

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
| Reporting API (MVP) | Event ingestion, `current_resources` table, basic query endpoints, provider-agnostic reconciliation framework |
| Project Registry | Project + relation tables in Reporting API DB, CRUD endpoints |
| OTel Collector | Deploy as universal metrics ingestion layer (OTLP in, Remote Write to VictoriaMetrics) |
| VictoriaMetrics | Set up with initial scrape configs |
| OpenStack DB Exporter | Evaluate [vexxhost/openstack_database_exporter](https://github.com/vexxhost/openstack_database_exporter), adapt if needed, and deploy on the control plane |
| OpenStack Ceilometer pipeline | Configure: metrics → OTel Collector → VictoriaMetrics, events → Reporting API |
| OpenStack reconciliation adapter | Implement adapter that polls OpenStack APIs |

### Phase 2 – Reporting & Dashboards

| Task | Description |
|------|-------------|
| Extend Reporting API | Lifecycle queries, aggregations, `/metrics` endpoint |
| Grafana dashboards | On VictoriaMetrics data: resource overview, usage trends (multi-platform) |
| Alerting | Anomaly detection (sudden resource spikes, etc.) |

### Phase 3 – Metering & Rating

| Task | Description |
|------|-------------|
| Metering Engine | Platform-agnostic usage records in minutes per resource per billing period (with resize/state splitting) |
| Rating Engine | Apply per-platform pricing model to usage records → rated usage data with costs |
| Pricing model | Define and make configurable per platform |
| Integration | Connect to external billing/ERP system (if applicable) |

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


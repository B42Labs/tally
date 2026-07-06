# 01 – Phase 1: Core Platform + OpenStack Provider (Foundation)

> Prerequisites: none (first phase). Read [00-conventions.md](00-conventions.md) first — its
> stack, layout, schema, and guardrail decisions are assumed throughout.

## Goal

At the end of Phase 1:

1. The **Reporting API** ingests events idempotently, maintains the `current_resources`
   projection (with out-of-order replay), serves query endpoints, validates payloads against a
   resource-type registry, dead-letters invalid events, and runs a provider-agnostic
   reconciliation framework — all authenticated and audit-logged.
2. The **Project Registry** (part of the Reporting API) manages projects and temporally valid
   relations.
3. The **OpenStack provider** is fully integrated: `openstack-event-collector` (oslo.messaging →
   Tally events, buffered at-least-once), reconciliation adapter, Ceilometer → OTel Collector →
   VictoriaMetrics pipeline, and the OpenStack DB exporter deployed.
4. A **vertical slice** proves one resource type (OpenStack instance) end-to-end:
   event → usage record → rated record, with exact golden numbers.

**Out of scope for Phase 1**: dashboards/alerting (Phase 2), the production metering/rating
engine (Phase 3 — only the throwaway vertical-slice prototype here), non-OpenStack providers
(Phase 4), pricing adjustments (Phase 5).

## Decisions made by this document (refinements over the concept)

| # | Decision | Rationale |
|---|---|---|
| D1 | Normalized payload envelope (`payload.state`, `payload.size`) is mandatory; collectors do all provider-specific mapping | Keeps projection/timeline/metering 100 % platform-agnostic (see conventions §4.1) |
| D2 | Event-type effect derived from `create`/`delete` verbs in `event_type` (conventions §4.2) | No per-platform code in the core |
| D3 | Ingest auth = per-`(platform, cloud)` bearer tokens (hashed at rest); mTLS deferred | Simplest secure MVP; mTLS is an infrastructure add-on later |
| D4 | Query auth = static API tokens with roles (`admin`, `read_all`, `project`); OIDC behind a config flag as a later extension | RBAC semantics fixed now, IdP integration decoupled |
| D5 | Scope violations (credential vs. event `platform`/`cloud`) are rejected + audit-logged but **not** dead-lettered | They are security events, not schema drift |
| D6 | Cloud installations are configured via a YAML file (`TALLY_CLOUDS_CONFIG`), not DB rows | Reconciliation credentials live next to deployment config, not in the API DB |
| D7 | Batch limit 1000 events; the response always enumerates per-item outcomes | Lets collectors delete their buffer on any 200 |
| D8 | Synthetic events use event types `sync.create` / `sync.update` / `sync.delete` | Categorize correctly under D2; trivially identifiable |
| D9 | Missing size-schema registration: accept + warn by default (`TALLY_INGEST_REQUIRE_SIZE_SCHEMA=false`), strict mode for production | Don't block onboarding on registry completeness; strictness is a deploy choice |

---

## Work packages

Implement in numerical order. Each WP states files, contract, acceptance criteria.

```
WP1.1 scaffolding ─▶ WP1.2 tally-core ─▶ WP1.3 API skeleton+DB ─▶ WP1.4 auth/audit
  ─▶ WP1.5 resource-type registry ─▶ WP1.6 ingestion ─▶ WP1.7 projection
  ─▶ WP1.8 query endpoints ─▶ WP1.9 project registry ─▶ WP1.10 reconciliation framework
  ─▶ WP1.11 service metrics ─▶ WP1.12 openstack-event-collector
  ─▶ WP1.13 openstack reconciliation adapter ─▶ WP1.14 metrics pipeline
  ─▶ WP1.15 vertical slice
```

---

### WP 1.1 – Repository scaffolding & dev stack

**Create**

- `pyproject.toml` (uv workspace: `libs/tally-core`, `services/reporting-api`,
  `providers/openstack/event-collector`), `uv.lock`, root `ruff.toml`, `mypy.ini`
- `Makefile` targets: `up` / `down` (compose stack), `test`, `lint`, `typecheck`,
  `migrate` (alembic upgrade head), `fmt`
- `deploy/compose/docker-compose.yaml` with services:
  - `timescaledb` — image `timescale/timescaledb:latest-pg16`, DB `tally_reporting`,
    healthcheck `pg_isready`
  - `victoriametrics` — image `victoriametrics/victoria-metrics`, args
    `-retentionPeriod=13 -promscrape.config=/etc/vm/scrape.yaml`, mount
    `deploy/compose/victoriametrics/scrape.yaml`
  - `otel-collector` — image `otel/opentelemetry-collector-contrib`, mount
    `deploy/compose/otel-collector/config.yaml`
  - `reporting-api` — built from `services/reporting-api/Dockerfile`, depends on `timescaledb`
- `.github/workflows/ci.yaml`: matrix over packages → `uv sync`, `ruff check`, `mypy`, `pytest`
- `.env.example` files per service

**Acceptance criteria**

- `make up` brings up TimescaleDB, VictoriaMetrics, OTel Collector; `make test` runs an empty
  test suite green in CI.
- `docker compose ps` shows all services healthy.

---

### WP 1.2 – Shared core library `libs/tally-core`

**Create** `src/tally_core/`:

`schemas/event.py` — Pydantic v2 models:

```python
class PayloadEnvelope(BaseModel):
    model_config = ConfigDict(extra="allow")
    state: str | None = None
    size: dict[str, Any] | None = None
    provider: dict[str, Any] | None = None

class Event(BaseModel):
    event_id: str = Field(min_length=1, max_length=256)
    timestamp: AwareDatetime            # normalized to UTC in a validator
    event_type: str = Field(pattern=r"^[a-z0-9_]+(\.[a-z0-9_]+)+$")
    platform: str
    cloud: str
    resource_type: str
    resource_id: str
    project_id: str
    source: Literal["collector", "reconciliation"] = "collector"
    payload: PayloadEnvelope = PayloadEnvelope()

def categorize(event_type: str) -> Literal["CREATE", "DELETE", "UPDATE"]: ...
```

Cross-field validation (as model validators): `CREATE` events require `payload.state` and
`payload.size`; every event requires `payload.state` except `DELETE`.

`ids.py`:

```python
def deterministic_event_id(platform: str, cloud: str, resource_id: str,
                           event_type: str, timestamp: datetime) -> str:
    raw = f"{cloud}:{resource_id}:{event_type}:{timestamp.astimezone(UTC).isoformat()}"
    return f"{platform}-{hashlib.sha256(raw.encode()).hexdigest()}"

def synthetic_event_id(sync_run_id: str, cloud: str, resource_type: str,
                       resource_id: str, kind: str) -> str:
    raw = f"{sync_run_id}:{cloud}:{resource_type}:{resource_id}:{kind}"
    return f"recon-{hashlib.sha256(raw.encode()).hexdigest()}"
```

`timeline.py` — the shared folding algorithm (conventions §5):

```python
@dataclass(frozen=True)
class Interval:
    start: datetime          # inclusive
    end: datetime | None     # exclusive; None = open (resource still in this config)
    state: str
    size: dict[str, Any]
    project_id: str

@dataclass(frozen=True)
class Timeline:
    intervals: list[Interval]
    created_at: datetime | None      # ts of first CREATE (None if history starts mid-life)
    deleted_at: datetime | None      # ts of the DELETE event, if any
    warnings: list[str]              # e.g. "history_starts_without_create"

def build_timeline(events: Sequence[Event]) -> Timeline:
    # 1. sort by (timestamp, received_at, event_id)  — received_at passed alongside
    # 2. fold: track (state, size, project_id); snapshot changes (deep-compare size)
    #    close the current interval and open a new one at e.timestamp
    # 3. DELETE closes the last interval at e.timestamp and sets deleted_at
    #    (state "deleted" produces no interval — deleted resources accrue nothing)
    # 4. drop zero-length intervals (start == end)
    # 5. events that change nothing produce no interval boundary
```

`money.py`: `DECIMAL_CTX` (prec 28, `ROUND_HALF_UP`), `round_money(d) -> Decimal` (quantize
`0.01`), `minutes(seconds: int) -> Decimal` (quantize `0.0001`), custom JSON encoder preserving
2 dp for money.

`testing/` — conformance kit (used by every provider, extended in Phase 4):
`assert_valid_event(dict)`, `assert_deterministic_ids(fn)`, fixture events builder.

**Tests (unit)**

- Timeline: single create; create→delete; create→resize→delete; equal-timestamp ties broken by
  `received_at` then `event_id`; no-op event does not split; zero-length interval dropped;
  history starting without CREATE yields warning; DELETE without prior events.
- `categorize()` table test incl. `sync.create`, `volume.transfer.accept.end` → UPDATE.
- Money: `round_money(Decimal("2.025")) == Decimal("2.03")` (half-up), encoder output `"2.03"`.

---

### WP 1.3 – Reporting API skeleton + database schema

**Create** `services/reporting-api/`: FastAPI app factory (`main.py`), `config.py`
(pydantic-settings, prefix `TALLY_REPORTING_`), `db.py` (async engine/session), `models.py`,
Alembic setup + **migration 0001** with the full schema:

```sql
-- Append-only source of truth (TimescaleDB hypertable, compressed)
CREATE TABLE events (
    event_id        TEXT NOT NULL,
    timestamp       TIMESTAMPTZ NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_type      TEXT NOT NULL,
    platform        TEXT NOT NULL,
    cloud           TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    source          TEXT NOT NULL DEFAULT 'collector',
    payload         JSONB,
    PRIMARY KEY (event_id, timestamp)
);
SELECT create_hypertable('events', 'timestamp');
ALTER TABLE events SET (timescaledb.compress,
    timescaledb.compress_segmentby = 'cloud,resource_type');
SELECT add_compression_policy('events', INTERVAL '90 days');

CREATE INDEX idx_events_resource ON events (cloud, resource_type, resource_id, timestamp);
CREATE INDEX idx_events_project  ON events (project_id, timestamp);
CREATE INDEX idx_events_type     ON events (event_type, timestamp);
CREATE INDEX idx_events_received ON events (received_at);   -- late-event detection (Phase 3)

CREATE TABLE rejected_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    reason       TEXT NOT NULL,           -- 'schema' | 'size_schema' | 'payload_envelope'
    raw          JSONB NOT NULL
);

-- Derived projection; rows are never removed (deleted resources keep their row)
CREATE TABLE current_resources (
    cloud           TEXT NOT NULL,
    platform        TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    state           TEXT NOT NULL,
    size            JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    last_event_type TEXT NOT NULL,
    last_event_at   TIMESTAMPTZ NOT NULL,
    last_payload    JSONB,
    PRIMARY KEY (cloud, resource_type, resource_id)
);
CREATE INDEX idx_current_resources_project ON current_resources (project_id);
CREATE INDEX idx_current_resources_type    ON current_resources (resource_type, state);

CREATE TABLE resource_types (
    platform      TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    size_schema   JSONB NOT NULL,          -- JSON Schema draft 2020-12
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform, resource_type)
);

CREATE TABLE projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    platform    TEXT NOT NULL,
    cloud       TEXT NOT NULL,
    external_id TEXT NOT NULL,
    name        TEXT,
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (cloud, external_id)
);

CREATE TABLE project_relations (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id     UUID NOT NULL REFERENCES projects(id),
    target_id     UUID NOT NULL REFERENCES projects(id),
    relation_type TEXT NOT NULL,
    metadata      JSONB NOT NULL DEFAULT '{}',
    valid_from    TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to      TIMESTAMPTZ,             -- NULL = active; never hard-deleted
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (source_id <> target_id)
);
CREATE UNIQUE INDEX uq_relations_active
    ON project_relations (source_id, target_id, relation_type) WHERE valid_to IS NULL;
CREATE INDEX idx_relations_source ON project_relations (source_id);
CREATE INDEX idx_relations_target ON project_relations (target_id);

CREATE TABLE ingest_credentials (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    platform    TEXT NOT NULL,
    cloud       TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,      -- sha256 hex of the bearer token
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE TABLE api_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash  TEXT NOT NULL UNIQUE,
    role        TEXT NOT NULL,             -- 'admin' | 'read_all' | 'project'
    project_ids UUID[] NOT NULL DEFAULT '{}',   -- for role='project'
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE TABLE audit_log (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor       TEXT NOT NULL,             -- credential/token id or 'internal'
    action      TEXT NOT NULL,             -- e.g. 'events.ingest', 'projects.create'
    object_type TEXT,
    object_id   TEXT,
    details     JSONB
);

CREATE TABLE sync_runs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    cloud        TEXT NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    status       TEXT NOT NULL DEFAULT 'running',  -- 'running'|'completed'|'failed'
    stats        JSONB NOT NULL DEFAULT '{}'       -- {created: n, updated: n, deleted: n, errors: [..]}
);
CREATE INDEX idx_sync_runs_cloud ON sync_runs (cloud, started_at DESC);
```

Also: `GET /healthz`, `GET /readyz` (readiness = DB reachable; liveness = unhealthy-for >
`TALLY_REPORTING_UNHEALTHY_THRESHOLD_S`, default 600), RFC 9457 exception handlers, structlog
setup, request-ID middleware.

**Config** (`.env.example`): `TALLY_REPORTING_DB_URL`, `TALLY_REPORTING_HTTP_PORT=8080`,
`TALLY_REPORTING_AUTH_MODE=enforced|disabled`, `TALLY_REPORTING_INTERNAL_TOKEN`,
`TALLY_INGEST_REQUIRE_SIZE_SCHEMA=false`, `TALLY_REPORTING_CLOUDS_CONFIG=/etc/tally/clouds.yaml`.

**Acceptance criteria**: `make migrate` creates the schema on the compose stack; hypertable +
compression policy verified in an integration test (`SELECT * FROM timescaledb_information.hypertables`).

---

### WP 1.4 – AuthN/AuthZ + audit log

**Create** `auth.py`, `audit.py`, admin CLI (`tally-reporting-admin`, Typer).

- Token formats: ingest `tly_i_<hex32>`, api `tly_a_<hex32>` (random 32 bytes hex). Lookup by
  `sha256(token)`; constant-time compare not required (hash lookup), but tokens never logged.
- FastAPI dependencies:
  - `IngestAuth` → returns `(credential_id, platform, cloud)`; 401 if missing/unknown/revoked.
  - `QueryAuth(required_role)` → `admin` ⊇ `read_all` ⊇ `project`; role `project` restricts
    query endpoints to its `project_ids` (filter injected into queries; forbidden filters → 403).
  - `InternalAuth` → shared secret `TALLY_REPORTING_INTERNAL_TOKEN` for `/internal/*`.
  - `TALLY_REPORTING_AUTH_MODE=disabled` short-circuits all three (dev/tests only; log a warning
    at startup).
- Audit: helper `audit(actor, action, object_type, object_id, details)`; call on **every**
  write operation (ingest summary per batch, project/relation/resource-type mutations,
  credential admin actions, sync runs).
- CLI: `create-ingest-credential --platform --cloud --description`,
  `create-api-token --role [--project-id ...]`, `revoke-...` — prints the token exactly once.
- **RBAC on query endpoints** maps `project_id` strings: a `project`-role token holds registry
  UUIDs; the dependency resolves them to `(cloud, external_id)` pairs and filters event/resource
  queries on those `project_id` values.
- OIDC extension point (implement interface, leave provider off): `TALLY_REPORTING_OIDC_JWKS_URL`
  — when set, `Bearer` JWTs are accepted, `role`/`projects` claims mapped like api_tokens.

**Tests**: 401/403 matrix per endpoint class; revoked token rejected; project-scoped token sees
only its projects (integration); audit rows written.

---

### WP 1.5 – Resource type registry

**Create** `routers/resource_types.py` + service module.

```
PUT /api/v1/resource-types/{platform}/{resource_type}    (role: admin)
    Body: { "size_schema": { ...JSON Schema draft 2020-12... } }
    → 200; 400 if the schema itself does not compile
GET /api/v1/resource-types                               (any query role)
GET /api/v1/resource-types/{platform}/{resource_type}
```

- Compiled validators cached in-process, keyed by `(platform, resource_type, updated_at)`.
- Seed migration/fixture registers the OpenStack Phase-1 types:

```json
// (openstack, instance)
{ "type": "object",
  "required": ["vcpus", "ram_gb", "disk_gb", "flavor"],
  "properties": { "vcpus": {"type": "integer", "minimum": 1},
                  "ram_gb": {"type": "number", "exclusiveMinimum": 0},
                  "disk_gb": {"type": "number", "minimum": 0},
                  "flavor": {"type": "string"} },
  "additionalProperties": true }
// (openstack, volume)
{ "type": "object", "required": ["size_gb", "type"],
  "properties": { "size_gb": {"type": "number", "exclusiveMinimum": 0},
                  "type": {"type": "string"} }, "additionalProperties": true }
// (openstack, floating_ip)
{ "type": "object", "required": ["ip_version"],
  "properties": { "ip_version": {"enum": [4, 6]} }, "additionalProperties": true }
// (openstack, image)
{ "type": "object", "required": ["size_gb"],
  "properties": { "size_gb": {"type": "number", "minimum": 0} }, "additionalProperties": true }
```

**Tests**: register/overwrite/list; invalid JSON Schema → 400; ingest-side validation covered in
WP 1.6 tests.

---

### WP 1.6 – Event ingestion

**Create** `routers/events.py` + `ingestion.py` service module.

```
POST /api/v1/events        (auth: ingest credential)
  Body: Event | Event[]        (max 1000 items → 413 beyond)
  → 200 {
      "accepted": 12,
      "duplicates": 2,
      "rejected": [ {"index": 3, "event_id": "…", "reason": "size_schema: 'vcpus' is a required property"} ]
    }
```

Per-item pipeline (order matters):

1. **Pydantic validation** (`tally_core.schemas.Event`, incl. payload-envelope rules)
   → fail: `rejected` with `reason="schema: …"` **and** row in `rejected_events`.
2. **Scope check**: `event.platform == cred.platform and event.cloud == cred.cloud`
   → fail: `rejected` with `reason="scope"`, audit-log entry, **no** dead-letter row (D5).
   (Reconciliation-internal ingestion bypasses this check.)
3. **Size validation**: if `payload.size` present and a schema is registered for
   `(platform, resource_type)` → validate; unregistered type: accept + increment
   `tally_ingest_unvalidated_size_total` (or reject when `TALLY_INGEST_REQUIRE_SIZE_SCHEMA=true`)
   → fail: `rejected`, `reason="size_schema: …"`, dead-letter row.
4. **Insert** all surviving items in one transaction:
   `INSERT INTO events (...) VALUES ... ON CONFLICT (event_id, timestamp) DO NOTHING`
   — items not inserted count as `duplicates`.
5. **Projection update** (WP 1.7) inside the same transaction, grouped per resource key.
6. Response is always 200 when the request itself was authorized and parseable — collectors
   treat 200 as "safe to delete from buffer" (rejected items are dead-lettered server-side and
   must not be retried).

Internal API for reconciliation: `ingest_events(session, events, source="reconciliation")` —
same pipeline minus auth/scope, same dedup + projection.

**Tests (integration, testcontainers)**

- Single + batch ingest; exact response counts.
- Replaying the identical batch → all `duplicates`, DB state unchanged (byte-identical
  projection rows).
- Invalid event → dead-lettered with reason; valid siblings in same batch still accepted.
- Scope mismatch → rejected, audited, not dead-lettered.
- 1001 items → 413.
- Size-schema violation (`vcpus: "four"`) → rejected + dead-lettered.

---

### WP 1.7 – Projection (`current_resources`)

**Create** `projection.py`.

Concurrency: before touching a resource's projection row, take a transaction-scoped advisory
lock **on the reporting DB** — the same lock the Phase-3 engine takes:

```sql
SELECT pg_advisory_xact_lock(
    hashtextextended(:cloud || ':' || :resource_type || ':' || :resource_id, 0));
```

Algorithm (per resource key, with its batch of just-inserted events):

```python
async def apply(session, key, new_events):          # new_events sorted (ts, received_at, event_id)
    await advisory_xact_lock(session, key)
    row = await load_projection_row(session, key)   # SELECT ... FOR UPDATE
    oldest = new_events[0]
    if row is None or oldest.timestamp >= row.last_event_at:
        for e in new_events:
            row = apply_incremental(row, e)         # cheap path
        await upsert(session, row)
    else:                                           # out-of-order / late event
        await replay(session, key)                  # rebuild from full history
        metrics.projection_replays.labels(cloud=key.cloud).inc()

def apply_incremental(row, e):
    cat = categorize(e.event_type)
    if cat == "CREATE":
        row.created_at = e.timestamp; row.deleted_at = None
        row.state = e.payload.state; row.size = e.payload.size
    elif cat == "DELETE":
        row.deleted_at = e.timestamp; row.state = "deleted"
    else:  # UPDATE
        if e.payload.state is not None: row.state = e.payload.state
        if e.payload.size  is not None: row.size  = e.payload.size
    row.project_id = e.project_id
    row.platform = e.platform
    row.last_event_type, row.last_event_at, row.last_payload = e.event_type, e.timestamp, e.payload
    return row

async def replay(session, key):
    events = await load_all_events(session, key)    # full history, ordered
    tl = build_timeline(events)                     # tally_core.timeline
    # final snapshot = last interval (or 'deleted' state); upsert row from it
```

Rules:

- Projection rows are **never deleted**; a deleted resource keeps its row
  (`state='deleted'`, `deleted_at` set) — Phase 3 uses this as the candidate index.
- `replay()` must be resilient to histories that start without a CREATE (timeline warning).
- Expose `POST /internal/projection/rebuild` (InternalAuth; body: optional
  `cloud`/`resource_type` filter) that replays projections in batches — the operational
  guarantee that the projection is always rebuildable.

**Tests (integration)**

- In-order create→resize→delete: projection matches each step.
- Out-of-order: ingest create + delete, then a resize with an earlier timestamp → replay yields
  the same row as in-order ingestion (property: **ingestion order never changes the final
  projection** — test with shuffled permutations of a fixed event set).
- Equal timestamps: later `received_at` wins.
- Full rebuild endpoint reproduces identical rows (snapshot compare).

---

### WP 1.8 – Query endpoints

**Create** `routers/events.py` (GET), `routers/resources.py`.

```
GET /api/v1/events?cloud=&platform=&project_id=&resource_type=&event_type=&source=&from=&to=&limit=&cursor=
    → { items: [Event+received_at], next_cursor }        sorted (timestamp, event_id)

GET /api/v1/resources/{cloud}/{resource_type}/{resource_id}/events
    → full ordered history of one resource

GET /api/v1/resources/{cloud}/{resource_type}/{resource_id}/lifecycle
    → { "resource": {...projection row...},
        "events":   [...ordered history...],
        "intervals": [ {"from": "...", "to": "...|null", "state": "...",
                        "size": {...}, "project_id": "..."} ],   // build_timeline() output
        "warnings": [] }

GET /api/v1/resources?cloud=&platform=&project_id=&resource_type=&state=&status=active|deleted|all&limit=&cursor=
    → projection rows; status=active ⇒ state != 'deleted' (default), deleted ⇒ state = 'deleted'

GET /api/v1/rejected-events?from=&to=&limit=&cursor=      (role: admin)   -- dead-letter review
```

All query endpoints enforce QueryAuth; `project`-role tokens get a mandatory `project_id`
filter (403 on attempts to query outside scope).

**Tests**: filter combinations, pagination stability (no dup/miss across pages), lifecycle
intervals for the resize example, RBAC filtering.

---

### WP 1.9 – Project Registry

**Create** `routers/projects.py` + service module.

```
POST   /api/v1/projects                        (role: admin)
       { platform, cloud, external_id, name?, metadata? }  → 201 | 409 on (cloud, external_id) conflict
GET    /api/v1/projects?platform=&cloud=&external_id=&limit=&cursor=
GET    /api/v1/projects/{id}
PATCH  /api/v1/projects/{id}                   (role: admin)   -- name, metadata only

POST   /api/v1/projects/{id}/relations         (role: admin)
       { target_id, relation_type, metadata?, valid_from? }
       → 201; 409 if an active relation (same source, target, type) exists
       → 422 if relation_type is attributing and would create a cycle (see below)
GET    /api/v1/projects/{id}/relations?direction=outgoing|incoming|both&relation_type=&at=
PATCH  /api/v1/projects/{id}/relations/{relation_id}          -- metadata, valid_to only
DELETE /api/v1/projects/{id}/relations/{relation_id}
       → closes: valid_to = now(); repeated DELETE = 204 no-op; never hard-deletes

GET    /api/v1/projects/{id}/related?depth=1&relation_type=&at=2026-03-01T00:00:00Z
```

Semantics:

- A relation is **valid at `t`** iff `valid_from <= t AND (valid_to IS NULL OR valid_to > t)`.
  `at` defaults to now.
- `related` = BFS over *outgoing* relations valid at `at`, filtered by `relation_type` if given,
  up to `depth` (default 1, max 10). Cycle-safe: track visited project ids. Response items:
  `{ project, relation_type, depth, path: [relation ids] }`.
- **Attributing relation types** (config list, default `["infrastructure_tenant"]`): on
  creation, walk outgoing attributing edges from `target_id`; if `source_id` is reachable →
  422 (cycle). This keeps attribution a forest per the concept's no-double-billing rules.

**Event-driven registration hook** (used by Gardener in Phase 4, spec'd now): ingestion exposes
a post-ingest hook interface `on_event(event)`; Phase 1 registers only a no-op default.

**Tests**: CRUD; active-uniqueness (409); close & recreate; temporal `at` queries (relation
closed in March invisible for `at` in April but visible for `at` in March); traversal with
depth/cycles; attributing-cycle rejection.

---

### WP 1.10 – Reconciliation framework (provider-agnostic)

**Create** `reconciliation/framework.py`, `routers/internal.py`.

Cloud configuration (`TALLY_REPORTING_CLOUDS_CONFIG`, YAML):

```yaml
clouds:
  - cloud: os-prod-eu1
    platform: openstack
    adapter: openstack
    adapter_config:
      os_cloud: os-prod-eu1        # entry name in openstacksdk clouds.yaml
      include_octavia: false
```

Adapter protocol:

```python
@dataclass(frozen=True)
class ObservedResource:
    resource_type: str
    resource_id: str
    project_id: str
    state: str                       # normalized tally state
    size: dict[str, Any]
    created_at: datetime | None      # real creation time if the API exposes it
    deleted_at: datetime | None      # set only for resources reported as deleted

class ReconciliationAdapter(Protocol):
    platform: str
    def list_resources(self, cfg: dict, since: datetime | None
                       ) -> AsyncIterator[ObservedResource]:
        """Yield all live resources; MAY additionally yield recently deleted
        resources (deleted_at set) when the platform exposes them."""
```

Sync orchestration:

```
POST /internal/sync/{cloud}      (InternalAuth; triggered by CronJob every 10 min)
  → 200 { "sync_run_id": "...", "stats": {"created": n, "updated": n, "deleted": n} }
  → 409 if a sync for this cloud is already running (advisory lock on 'sync:'+cloud)
```

Diff algorithm:

```python
async def sync(cloud_cfg) -> SyncStats:
    run = insert_sync_run(cloud)
    observed = {(r.resource_type, r.resource_id): r
                async for r in adapter.list_resources(cfg, since=last_success(cloud))}
    db = load_projection_rows(cloud)                    # incl. state='deleted'
    synthetic: list[Event] = []
    for key, obs in observed.items():
        if obs.deleted_at:                               # platform reports real deletion
            if db.get(key) and db[key].state != "deleted":
                synthetic.append(ev("sync.delete", ts=obs.deleted_at, state="deleted"))
            continue
        row = db.get(key)
        if row is None or row.state == "deleted":        # missed create (or resurrection)
            synthetic.append(ev("sync.create", ts=obs.created_at or now(),
                                state=obs.state, size=obs.size))
        elif (row.state, row.size, row.project_id) != (obs.state, obs.size, obs.project_id):
            synthetic.append(ev("sync.update", ts=now(), state=obs.state, size=obs.size))
    for key, row in db.items():                          # in DB, not observed → missed delete
        if row.state != "deleted" and key not in observed_live_keys:
            synthetic.append(ev("sync.delete", ts=now(), state="deleted"))
    ingest_events(session, synthetic, source="reconciliation")   # normal pipeline: dedup + projection
    complete_sync_run(run, stats)
```

- `event_id` = `tally_core.ids.synthetic_event_id(run.id, cloud, rtype, rid, kind)` —
  deterministic per run; re-POSTing the run's batch never duplicates.
- Timestamps: use real platform timestamps (`created_at`/`deleted_at`) when available,
  otherwise poll time — the concept's accepted limitation; both cases must be covered by tests.
- Failure of one resource listing (e.g. Cinder down) fails the run
  (`status='failed'`, `stats.errors`), never emits partial deletes for the failed
  resource types (**critical**: a down platform API must not mass-delete resources —
  the adapter reports which resource types it successfully enumerated, and the
  missed-delete pass runs only for those).

**Tests (integration, with a fake adapter)**: each diff branch; failed listing emits no
deletes; determinism (same run re-applied → 0 new events); concurrent sync → 409.

---

### WP 1.11 – Service metrics (`/metrics`)

**Create** `metrics.py`; instrument WP 1.6/1.7/1.10 code paths.

| Metric | Type | Labels |
|---|---|---|
| `tally_events_ingested_total` | counter | `platform, cloud, resource_type, event_type, source` |
| `tally_events_deduplicated_total` | counter | `cloud` |
| `tally_events_rejected_total` | counter | `cloud, reason` |
| `tally_ingest_unvalidated_size_total` | counter | `platform, resource_type` |
| `tally_projection_replays_total` | counter | `cloud` |
| `tally_sync_runs_total` | counter | `cloud, status` |
| `tally_sync_resources_reconciled_total` | counter | `cloud, action` (`created/updated/deleted`) |
| `tally_sync_errors_total` | counter | `cloud` |
| `tally_current_resources` | gauge | `platform, cloud, resource_type, state` (refreshed periodically from the projection) |

(The concept's informal names `event_count`, `sync_resources_reconciled`, … map to these.)

**Tests**: scrape `/metrics` after an ingest + sync in integration tests; assert counters.

---

### WP 1.12 – `openstack-event-collector` (new service)

**Create** `providers/openstack/event-collector/` → package `tally_openstack_collector`
(`main.py`, `config.py`, `mapping.py`, `buffer.py`, `sender.py`, Dockerfile).

Architecture: **consume → map → buffer (SQLite) → ack**, with an independent sender loop —
at-least-once end-to-end:

```
oslo.messaging bus ──▶ NotificationListener ──▶ map to Tally event ──▶ INSERT into outbox ──▶ ack
                                                     (unmapped types: ack + skip metric)
outbox (SQLite, WAL) ──▶ sender loop: batch ≤500 ──▶ POST /api/v1/events ──▶ on 200: DELETE batch
                                                     on error: exponential backoff 1s→300s + jitter
```

- **Consumer**: `oslo.messaging` `get_notification_listener`, transport
  `TALLY_OSC_TRANSPORT_URL` (RabbitMQ), topics `TALLY_OSC_TOPICS` (default `notifications`),
  pool `tally-collector` (so Ceilometer keeps receiving its own copies). Ack only after the
  outbox insert committed; on mapping crash → requeue.
- **Outbox** (`buffer.py`): SQLite at `TALLY_OSC_BUFFER_PATH` (PVC/volume), WAL mode:
  `outbox(id INTEGER PRIMARY KEY AUTOINCREMENT, event_json TEXT NOT NULL, created_at TEXT NOT NULL)`.
  Backpressure: if outbox exceeds `TALLY_OSC_BUFFER_MAX_EVENTS` (default 1,000,000) → stop
  consuming (events wait on the bus), never drop.
- **Sender**: batches in id order; a 200 (regardless of per-item rejects — those are
  dead-lettered server-side) deletes the batch; 4xx/5xx/network keeps it. `event_id` =
  oslo `message_id` ⇒ redelivery is safe.

**Mapping table** (`mapping.py` — data-driven dict, one entry per oslo `event_type`; unmapped
types are counted and skipped). `resource_id` ← `payload.instance_id` / `volume_id` / `id`;
`project_id` ← `payload.tenant_id` / `project_id`; `timestamp` ← notification timestamp.

| oslo event_type | tally event_type | payload mapping |
|---|---|---|
| `compute.instance.create.end` | same | state ← vm_state map; size `{vcpus, ram_gb: memory_mb/1024, disk_gb: root_gb+ephemeral_gb, flavor: instance_type}` |
| `compute.instance.delete.end` | same | — |
| `compute.instance.resize.end` (and alias `compute.instance.finish_resize.end`) | `compute.instance.resize.end` | new size as above |
| `compute.instance.shelve_offload.end` | `compute.instance.shelve` | state `shelved` |
| `compute.instance.unshelve.end` | `compute.instance.unshelve` | state `active` |
| `compute.instance.power_off.end` | `compute.instance.power_off` | state `shutoff` |
| `compute.instance.power_on.end` | `compute.instance.power_on` | state `active` |
| `volume.create.end` | same | state `available`→later `in-use` via `volume.attach.end` (optional), size `{size_gb: size, type: volume_type}` |
| `volume.delete.end` | same | — |
| `volume.resize.end` | same | size `{size_gb: size, type}` |
| `volume.retype` | same | size `{size_gb, type: new type}` |
| `volume.transfer.accept.end` | same | `project_id` = new tenant; state/size unchanged → include current `state` |
| `floatingip.create.end` | same | state `active`, size `{ip_version}` |
| `floatingip.delete.end` | same | — |
| `image.create` / `image.upload` | `image.create` | state `active`, size `{size_gb: size_bytes/1024³}` (emit on `upload` when size known) |
| `image.delete` | same | — |

vm_state → tally state map: `active→active`, `stopped→shutoff`, `shelved_offloaded→shelved`,
`paused→paused`, `suspended→suspended`, `error→error`.

> ⚠️ Exact oslo notification names/payloads vary by OpenStack release. The mapping table is
> **data**, not code — verifying it against the target deployment (e.g. via a notification dump)
> is an explicit task in the acceptance criteria. Nova must run with `notify_on_state_change =
> vm_state` and `notification_format = unversioned` (or the collector handles versioned
> payloads — pick per deployment and document).

**Config**: `TALLY_OSC_TRANSPORT_URL`, `TALLY_OSC_TOPICS`, `TALLY_OSC_CLOUD`,
`TALLY_OSC_REPORTING_URL`, `TALLY_OSC_TOKEN`, `TALLY_OSC_BUFFER_PATH`,
`TALLY_OSC_BATCH_MAX=500`, `TALLY_OSC_FLUSH_INTERVAL_S=5`, `TALLY_OSC_BUFFER_MAX_EVENTS`.

**Observability**: `/healthz` (consumer connected + outbox writable), `/metrics`:
`tally_collector_consumed_total{event_type}`, `tally_collector_skipped_total{event_type}`,
`tally_collector_buffer_depth` (gauge), `tally_collector_delivered_total`,
`tally_collector_delivery_errors_total`, `tally_collector_oldest_buffered_seconds` (gauge).

**Tests**

- Unit: every mapping-table entry with a captured sample notification → exact Tally event
  (golden JSON fixtures under `tests/golden/notifications/`); conformance kit
  (`tally_core.testing`) passes for all produced events.
- Integration: fake Reporting API — kill it, produce events, restart it → all events delivered
  exactly once by `event_id`; collector restart mid-buffer loses nothing (outbox survives).

---

### WP 1.13 – OpenStack reconciliation adapter

**Create** `reconciliation/adapters/openstack.py` (dependency: `openstacksdk`; connects via the
`os_cloud` entry from `adapter_config`).

Listings → `ObservedResource` (all with normalized state + size per WP 1.12 conventions):

| Resource type | API | Notes |
|---|---|---|
| `instance` | Nova `GET /servers/detail?all_tenants=1` | size via flavor (embedded or flavor cache `GET /flavors/detail`); vm_state → state map |
| `instance` (deleted) | Nova `GET /servers/detail?all_tenants=1&deleted=true&changes-since={last_success}` | yields `deleted_at` from the API → real deletion timestamps |
| `volume` | Cinder `GET /volumes/detail?all_tenants=1` | size `{size_gb, type}` |
| `floating_ip` | Neutron `GET /v2.0/floatingips` | size `{ip_version}` |
| `image` | Glance `GET /v2/images` | size `{size_gb}`; skip images without owner |
| `loadbalancer` | Octavia `GET /v2/lbs` | only when `include_octavia: true` |

- The adapter reports per-resource-type success so the framework's missed-delete pass only runs
  for fully enumerated types (WP 1.10 failure rule).
- Pagination handled for all listings; project scoping: `project_id` from the resource's
  `tenant_id`/`project_id` field.

**Tests**: unit tests against recorded API fixtures (respx/vcr-style JSON), one test per diff
scenario wired through the WP 1.10 framework with this adapter mocked at the HTTP layer.

---

### WP 1.14 – Metrics pipeline: Ceilometer → OTel → VictoriaMetrics, DB exporter

**Create/modify**: `deploy/compose/otel-collector/config.yaml`,
`deploy/compose/victoriametrics/scrape.yaml`, docs under `docs/openstack-metrics.md`.

OTel Collector config (universal middleware for all providers):

```yaml
receivers:
  otlp: { protocols: { grpc: {}, http: {} } }
processors:
  batch: {}
exporters:
  prometheusremotewrite:
    endpoint: http://victoriametrics:8428/api/v1/write
service:
  pipelines:
    metrics: { receivers: [otlp], processors: [batch], exporters: [prometheusremotewrite] }
```

Ceilometer: **verify per deployed version** whether a native OTLP publisher exists; otherwise
configure the `prometheus` publisher and scrape it (document the chosen path):

```yaml
# ceilometer pipeline.yaml (fallback path)
sinks:
  - name: tally_sink
    publishers:
      - prometheus://ceilometer-exporter:9101/metrics
```

VictoriaMetrics scrape config — **every scrape job attaches the mandatory `platform` and
`cloud` labels** via static `labels:` (this is how third-party exporters that don't know Tally's
label convention get normalized):

```yaml
scrape_configs:
  - job_name: reporting-api
    scrape_interval: 30s
    static_configs: [{ targets: ["reporting-api:8080"] }]
  - job_name: openstack-db-exporter
    scrape_interval: 60s
    static_configs:
      - targets: ["os-db-exporter:9180"]
        labels: { platform: "openstack", cloud: "os-prod-eu1" }
  - job_name: ceilometer
    scrape_interval: 60s
    static_configs:
      - targets: ["ceilometer-exporter:9101"]
        labels: { platform: "openstack", cloud: "os-prod-eu1" }
  - job_name: otel-collector
    scrape_interval: 15s
    static_configs: [{ targets: ["otel-collector:8888"] }]
```

**OpenStack DB Exporter** — evaluation task for
[vexxhost/openstack_database_exporter](https://github.com/vexxhost/openstack_database_exporter):

1. Checklist against concept §4.3: metrics for Nova instances/flavors/quotas, Cinder
   volumes/sizes, Neutron FIPs/ports/routers, Keystone projects, Glance images, Octavia LBs.
2. Verify label output can be relabeled to the Tally convention (`project_id`, `resource_id`
   where applicable) — write down the relabel_configs.
3. Deploy read-only DB users (SQL grants documented in `docs/openstack-metrics.md`).
4. Gaps → decide extend-upstream vs. supplement with a small custom exporter; record the
   decision in `docs/openstack-metrics.md`.

**Acceptance criteria**: on the dev stack, a metric pushed via OTLP appears in VictoriaMetrics
(`/api/v1/query`); scrape configs load without error; evaluation doc committed.

---

### WP 1.15 – Vertical slice (throwaway prototype, golden-numbers gate)

**Create** `services/engine/prototypes/vertical_slice.py` (explicitly throwaway — Phase 3
replaces it; only `tally_core.timeline` carries over).

CLI: `python vertical_slice.py --cloud os-prod-eu1 --project proj-456 --month 2026-03
--reporting-url ... --pricing pricing/prototype.yaml`

Steps: fetch instance events via `GET /api/v1/resources?...` + per-resource event queries →
`build_timeline()` → clip intervals to the month → usage records (`minutes` from integer
seconds) → rate with a minimal hardcoded pricing config (the concept's §3.4 OpenStack instance
prices) → print JSON, assert invariants (coverage, no gaps/overlaps).

**Golden test** (end-to-end through a real Reporting API instance): ingest these events…

```
compute.instance.create.end   2026-02-10T08:00:00Z  state=active  size={vcpus:4, ram_gb:8, disk_gb:80, flavor:"m1.large"}
compute.instance.power_off    2026-03-11T00:00:00Z  state=shutoff
compute.instance.power_on     2026-03-21T00:00:00Z  state=active
```

…then for March 2026 the slice must output **exactly** (concept §3.4 end-to-end example):

| # | interval | state | minutes | vcpus € | ram € | disk € | subtotal € |
|---|---|---|---|---|---|---|---|
| 1 | 03-01 → 03-11 | active | 14400 | 19.20 | 9.60 | 19.20 | 48.00 |
| 2 | 03-11 → 03-21 | shutoff (mod 0.5) | 14400 | 9.60 | 4.80 | 9.60 | 24.00 |
| 3 | 03-21 → 04-01 | active | 15840 | 21.12 | 10.56 | 21.12 | 52.80 |

Total **124.80 EUR** (the concept's 128.45 additionally includes egress counters, which the
slice does not implement — egress is Phase 3 counter-metric scope; document this delta in the
slice README).

Plus the resize example (concept Example 1): create @ 2026-03-01 (m1.small 2/4/40) + resize @
2026-03-16 (m1.large 4/8/80) → minutes 21600 / 23040.

**Acceptance criteria**: golden numbers exact; invariant assertions trip on synthetic bad input
(overlapping intervals) — proving the checks work.

---

## Phase exit criteria

1. All WP acceptance tests green in CI; `make up && make migrate` yields a working stack.
2. **Durability drill** (scripted, `docs/drills/phase1.md`): stop the Reporting API for 10
   minutes under event load → collector buffers; restart → zero loss, zero duplicates
   (verified by `event_id` count).
3. **Rebuild drill**: `POST /internal/projection/rebuild` after random-order re-ingestion of a
   day's events → projection identical (checksum over sorted rows).
4. **Reconciliation drill**: delete a resource's delete-event scenario (simulate missed
   delete via fake adapter) → synthetic `sync.delete` appears, projection corrected, metrics
   incremented.
5. Vertical-slice golden numbers exact.

## Risks & edge cases to keep in view

- oslo notification names/payloads differ per release — mapping table is data + must be
  verified against the target cloud (WP 1.12 warning).
- A platform API outage during sync must never mass-delete (WP 1.10 failure rule — test it).
- TimescaleDB compression: `UPDATE`s on compressed chunks are restricted — events are
  append-only anyway; never add an UPDATE path against `events`.
- Batch ingestion holds one transaction incl. projection updates — fine for ≤1000 events;
  if p95 latency becomes a problem, split projection updates into per-resource transactions
  *after* the insert transaction (still safe: replay heals any crash window) — note as a
  documented optimization, do not build it preemptively.

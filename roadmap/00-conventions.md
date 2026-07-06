# 00 – Binding Conventions

Everything in this document is **normative** for all phases. Phase documents reference it
instead of repeating it. If code and this document disagree, the code is wrong.

---

## 1. Technology stack (binding decisions)

The concept leaves some technology choices open ("Python or Go"). This roadmap fixes them so
that generated code is consistent:

| Concern | Decision | Notes |
|---|---|---|
| Language (all services) | **Python ≥ 3.12** | One language across the monorepo; oslo.messaging (OpenStack) is Python-only anyway |
| Web framework | **FastAPI** (latest 0.x) + Uvicorn | Automatic OpenAPI docs, Pydantic integration |
| Data validation | **Pydantic v2** | All wire schemas are Pydantic models |
| ORM / DB access | **SQLAlchemy 2.0 (async) + asyncpg** | Textual SQL is allowed (and expected) for TimescaleDB-specific DDL, advisory locks, and hot-path queries |
| Migrations | **Alembic** | One migration chain per service database |
| Databases | **PostgreSQL 16 + TimescaleDB 2.x** (Reporting API), **PostgreSQL 16** plain (Engine) | Use the `timescale/timescaledb:latest-pg16` image for dev |
| Metrics store | **VictoriaMetrics single-node** | `-retentionPeriod=13` (months) |
| Metrics pipeline | **OpenTelemetry Collector (contrib)** | OTLP in, Prometheus Remote Write out |
| Service metrics | **prometheus-client** | Every service exposes `/metrics` |
| Logging | **structlog**, JSON to stdout | Fields: `timestamp`, `level`, `event`, `service`, plus context |
| CLI | **Typer** | Engine and admin CLIs |
| HTTP client | **httpx** (async) | Collectors, reconciliation adapters, VM queries |
| JSON Schema validation | **jsonschema** (draft 2020-12) | Resource-type `size` schemas, pricing model validation |
| Dependency management | **uv** with a workspace at repo root | Each package has its own `pyproject.toml`; `uv.lock` at root |
| Lint / format | **ruff** (lint + format) | Config at repo root |
| Type checking | **mypy --strict** | Per-package |
| Tests | **pytest + pytest-asyncio**, **testcontainers** for Postgres/TimescaleDB | Integration tests must run against real Postgres, not SQLite |
| Containerization | **Docker**, multi-stage builds, one image per service | Dev stack via `docker compose` in `deploy/compose/` |
| CI | **GitHub Actions** | `lint` → `typecheck` → `test` per package, on every PR |

Version pins live in `pyproject.toml` files; this document intentionally does not pin exact
minor versions.

---

## 2. Repository layout (binding)

```
tally/
├── README.md                      # concept document (do not modify casually)
├── roadmap/                       # these documents
├── pyproject.toml                 # uv workspace root (members below)
├── uv.lock
├── Makefile                       # dev entry points: make up / test / lint / migrate
├── libs/
│   ├── tally-collector/           # Phase 4: shared collector runtime  →  import tally_collector
│   └── tally-core/                # shared package  →  import tally_core
│       └── src/tally_core/
│           ├── schemas/           # Event, EventBatch, payload envelope, common enums
│           ├── timeline.py        # event → interval folding (shared by API & engine)
│           ├── money.py           # Decimal helpers, rounding
│           ├── ids.py             # deterministic event-id hashing
│           └── testing/           # provider conformance test kit, fixtures
├── services/
│   ├── reporting-api/             # →  import tally_reporting
│   │   ├── alembic/
│   │   └── src/tally_reporting/
│   │       ├── main.py            # FastAPI app factory
│   │       ├── config.py
│   │       ├── db.py
│   │       ├── models.py          # SQLAlchemy models
│   │       ├── auth.py
│   │       ├── audit.py
│   │       ├── projection.py      # current_resources incremental update + replay
│   │       ├── metrics.py         # prometheus counters/gauges
│   │       ├── routers/
│   │       │   ├── events.py
│   │       │   ├── resources.py
│   │       │   ├── resource_types.py
│   │       │   ├── projects.py
│   │       │   ├── stats.py       # Phase 2
│   │       │   └── internal.py    # /internal/sync, health
│   │       └── reconciliation/
│   │           ├── framework.py   # adapter protocol, diff engine, synthetic events
│   │           └── adapters/
│   │               ├── openstack.py            # Phase 1
│   │               ├── hetzner.py              # Phase 4
│   │               └── ...
│   └── engine/                    # Phase 3  →  import tally_engine
│       ├── alembic/
│       └── src/tally_engine/
├── providers/
│   ├── openstack/
│   │   └── event-collector/       # →  import tally_openstack_collector
│   ├── hetzner/                   # Phase 4
│   ├── gardener/                  # Phase 4
│   └── harbor/                    # Phase 4
├── pricing/                       # versioned pricing model YAML files (Phase 3)
├── deploy/
│   ├── compose/                   # local dev stack
│   │   ├── docker-compose.yaml
│   │   ├── victoriametrics/scrape.yaml
│   │   ├── otel-collector/config.yaml
│   │   └── grafana/               # Phase 2: provisioning + dashboards JSON
│   └── kubernetes/                # manifests/helm — added when first deployed to k8s
└── .github/workflows/ci.yaml
```

Naming rules:

- Directory names use `kebab-case`, Python packages use `snake_case` with `tally_` prefix.
- All API routes live under `/api/v1/...`; internal-only routes under `/internal/...`.
- Database identifiers use `snake_case`.

---

## 3. Domain vocabulary (use these words, exactly)

| Term | Meaning |
|---|---|
| **platform** | Platform *type*: `openstack`, `hetzner`, `stackit`, `ionos`, `gardener`, `harbor`, later `meta`, `partner` |
| **cloud** | Concrete *installation* of a platform, e.g. `os-prod-eu1`. Two OpenStack clouds share `platform` but differ in `cloud`. |
| **resource key** | The triple `(cloud, resource_type, resource_id)`. Resource IDs are only unique within this triple — **every** key, join, lock, and cache must include `cloud`. |
| **event** | Immutable lifecycle fact, append-only, deduplicated on `event_id`. The `events` table is the single source of truth. |
| **projection** | `current_resources` — derived, rebuildable state. Never authoritative. |
| **collector** | Provider-side service that pushes events (`source: "collector"`). |
| **reconciliation** | Server-side periodic sync that emits synthetic events (`source: "reconciliation"`). |
| **timeline / interval** | Per-resource sequence of half-open intervals `[from, to)` with constant `(state, size, project_id)`, folded from its event history. |
| **usage record** | Metering output: one interval clipped to a billing period, with a `usage` object. |
| **rated record** | Rating output: money per dimension per usage record. |
| **run** | One versioned metering+rating execution (`regular` or `correction`). |
| **billing period** | Calendar month in **UTC**. `[first day 00:00:00Z, first day of next month 00:00:00Z)`. |

---

## 4. Canonical event schema (normative wire contract)

Every event POSTed to `POST /api/v1/events` (single object or array of up to 1000):

```json
{
  "event_id":      "string, 1..256 chars, globally unique idempotency key",
  "timestamp":     "ISO 8601 with timezone (stored as UTC)",
  "event_type":    "resource.action[.phase]  e.g. compute.instance.create.end",
  "platform":      "openstack | hetzner | ...",
  "cloud":         "installation id, e.g. os-prod-eu1",
  "resource_type": "instance | volume | server | ...",
  "resource_id":   "string",
  "project_id":    "string  (the owner AT/AFTER this event)",
  "source":        "collector | reconciliation",
  "payload":       { "...": "see payload envelope below" }
}
```

Rules:

- `event_id` comes from the provider's native event/action ID where one exists
  (oslo.messaging `message_id`, Hetzner action ID). Otherwise it is the deterministic hash
  `sha256("{cloud}:{resource_id}:{event_type}:{timestamp_iso}")`, hex, prefixed with the
  platform name (`tally_core.ids.deterministic_event_id()` is the single implementation).
- Duplicate = same `(event_id, timestamp)`. Ingestion is idempotent; replaying a batch is safe.
- Reusing an `event_id` with a *different* timestamp is a provider bug; the API cannot detect it
  cheaply (PK includes the hypertable partition column) and providers must not do it.

### 4.1 Normalized payload envelope (refinement over the concept)

The concept shows payloads informally. This roadmap makes the payload **normative**, because the
platform-agnostic core (projection, timeline, metering) must interpret it without
provider-specific code. Collectors do the provider→normalized mapping; the core stays generic.

```json
{
  "state": "active",              // REQUIRED on every event: resource state AT/AFTER the event
  "size":  { "vcpus": 4, ... },   // full replacement size object; REQUIRED on create and on any
                                  // size-changing event; OMITTED means "size unchanged"
  "provider": { ... }             // OPTIONAL free-form raw provider data (debugging/audit only —
                                  // never read by core logic)
}
```

- `state` values are provider-defined strings (`active`, `shutoff`, `shelved`, `running`,
  `hibernated`, …). The core treats them as opaque; pricing `state_modifiers` reference them.
  The special state `deleted` is set by the core on delete events.
- `size` must validate against the JSON Schema registered for `(platform, resource_type)` in the
  resource-type registry whenever it is present.
- On ownership transfer the event's top-level `project_id` is the **new** owner; the previous
  owner is implicit in the prior event history.

### 4.2 Event-type categorization (normative)

The core derives the *effect* of an event purely from its `event_type`:

```python
def categorize(event_type: str) -> Literal["CREATE", "DELETE", "UPDATE"]:
    parts = event_type.split(".")
    if "create" in parts: return "CREATE"
    if "delete" in parts: return "DELETE"
    return "UPDATE"
```

- `CREATE` sets `created_at = timestamp` and requires `payload.size` + `payload.state`.
- `DELETE` sets `deleted_at = timestamp` and forces `state = "deleted"`.
- `UPDATE` applies `payload.state` / `payload.size` / top-level `project_id` changes.
- Collectors must only forward **billable** events (e.g. `.end` phases, not `.start`), except
  where a `.start` itself is billable (`shoot.hibernate.start` changes state).
- Synthetic reconciliation events use the types `sync.create`, `sync.update`, `sync.delete`
  (which categorize correctly by the rule above).

---

## 5. Timeline & interval semantics (normative)

Used by the projection replay (Phase 1), the lifecycle endpoint (Phase 1), and the metering
engine (Phase 3). One shared implementation: `tally_core/timeline.py`.

- Events of one resource are ordered by **`(timestamp, received_at, event_id)`** — a total,
  deterministic order even for equal timestamps.
- Intervals are **half-open `[from, to)`**. An event at exactly a period boundary belongs to the
  *next* interval/period. A month period is `[2026-03-01T00:00:00Z, 2026-04-01T00:00:00Z)`.
- An interval closes only when something *billable* changes: `state`, `size` (deep equality), or
  `project_id`. Events that change nothing billable do not split.
- Zero-length intervals (two changes at the same instant) are dropped.
- A resource's timeline starts at its first `CREATE` event; if the first event in history is not
  a `CREATE` (missed create), the timeline starts at that first event and a warning metric is
  incremented.
- Time arithmetic is done in **integer seconds**; `minutes` values are derived as
  `Decimal(seconds) / 60` (see money rules).

---

## 6. Money & rounding (normative)

Single implementation in `tally_core/money.py`:

- All money math uses `decimal.Decimal` with a context of 28 significant digits.
  **`float` is forbidden** anywhere money, prices, or usage quantities are computed or stored —
  enforce with a lint rule / code-review checklist item.
- Database storage: `NUMERIC` (money: `NUMERIC(14,2)`); never `real`/`double precision`.
- Rounding mode: **`ROUND_HALF_UP`**.
- Per-dimension costs are computed at full precision, then rounded half-up to **2 decimal
  places per dimension per usage record**. All aggregates (resource, project, period) are sums
  of the rounded values — a total always equals the sum of its visible line items.
- One currency per pricing-model version (initially `EUR`). Never aggregate across currencies.
- JSON serialization: monetary and usage decimals are rendered as JSON numbers with their full
  quantized precision (e.g. `19.20` → `19.2` is *not* acceptable in exports; use a custom
  encoder that preserves 2 decimal places for money).

---

## 7. HTTP API conventions

- **Errors**: RFC 9457 `application/problem+json`:
  ```json
  { "type": "urn:tally:error:validation", "title": "Validation failed",
    "status": 400, "detail": "...", "errors": [ {"loc": ..., "msg": ...} ] }
  ```
- **Timestamps**: ISO 8601 UTC (`2026-03-01T00:00:00Z`) everywhere, in and out.
- **Pagination**: `?limit=` (default 100, max 1000) and `?cursor=` (opaque, base64 of the last
  sort key). List responses: `{ "items": [...], "next_cursor": "..." | null }`.
- **Auth**: `Authorization: Bearer <token>` (details in Phase 1 WP 1.9).
- **Health**: every service exposes `GET /healthz` (liveness) and `GET /readyz` (readiness)
  without auth; semantics per the concept (§3.2).
- **Metrics**: every service exposes `GET /metrics` (Prometheus exposition) without auth,
  metric names prefixed `tally_`.

---

## 8. Configuration conventions

- Configuration exclusively via environment variables, prefix `TALLY_`, parsed with
  `pydantic-settings` in each service's `config.py`.
- Common variables (every service): `TALLY_LOG_LEVEL` (default `INFO`),
  `TALLY_HTTP_PORT` (service default), `TALLY_METRICS_ENABLED` (default `true`).
- Secrets (DB URLs, tokens) are env vars too; support the `*_FILE` convention
  (`TALLY_DB_URL_FILE=/run/secrets/db-url`) for container-mounted secrets.
- Every service ships a `.env.example` listing all variables with defaults and comments.

---

## 9. Testing standards

- **Unit tests**: pure logic (timeline folding, money, mapping tables) — no I/O, fast.
- **Integration tests**: against real Postgres/TimescaleDB via testcontainers; cover ingestion,
  dedup, projection replay, reconciliation diffing, metering runs.
- **Golden tests**: the worked examples from the concept are encoded with **exact** expected
  values (they are reproduced in the phase documents). Golden test fixtures live as JSON files
  under `<package>/tests/golden/`.
- **Conformance kit** (`tally_core.testing`): reusable assertions every provider collector must
  pass (schema validity, deterministic `event_id`s, payload envelope rules, buffering behavior).
- Every WP's acceptance criteria list the tests that must exist; CI runs them all.

---

## 10. Guardrails for code generation (read before every WP)

1. **Never** use `float` for money, prices, or usage quantities. `Decimal` + `NUMERIC` only.
2. **Never** update or delete rows in `events` — append-only, unlimited retention. Corrections
   happen via new events and correction runs, not edits.
3. **Never** treat `current_resources` as a source of truth — it must be rebuildable from
   `events` at any time; any logic that cannot survive a projection rebuild is wrong.
4. **Every** resource-level key, query, lock, and join includes `cloud` (and `resource_type`).
5. Intervals are half-open `[from, to)`; event ordering ties break by `(received_at, event_id)`.
6. Ingestion must stay idempotent: replaying any batch of events yields the same DB state.
7. Finalized runs are immutable — enforce in the DB (trigger), not only in application code.
8. All timestamps are stored and compared as UTC (`TIMESTAMPTZ`); billing periods are UTC months.
9. Rounding is half-up, 2 dp, per dimension per usage record; totals are sums of rounded values.
10. When a spec here conflicts with convenience, the spec wins; surface conflicts in the PR
    description instead of silently deviating.

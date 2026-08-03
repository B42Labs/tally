# 00 – Binding Conventions

Everything in this document is **normative** for all phases. Phase documents reference it
instead of repeating it. If code and this document disagree, the code is wrong.

---

## 1. Technology stack (binding decisions)

The concept fixes the implementation language as **Go**; this roadmap fixes the rest of the
stack so that generated code is consistent:

| Concern | Decision | Notes |
|---|---|---|
| Language (all services) | **Go ≥ 1.25** (toolchain pinned in `go.mod`) | One language across the monorepo. OpenStack's oslo notifications are consumed directly over AMQP (WP 1.12) — no Python dependency |
| Module layout | **Single Go module** `github.com/b42labs/tally` | Binaries under `cmd/`, all shared code under `internal/` (see §2) |
| HTTP server | **net/http + chi** router | Middlewares: request ID, auth, RFC 9457 errors |
| API contract | **Contract-first OpenAPI 3.1**: `api/<service>/openapi.yaml`; types + server interfaces generated with **oapi-codegen**; request validation via the kin-openapi middleware | Replaces FastAPI's code-first OpenAPI — the endpoint specs in these documents are the contract, the YAML is their machine-readable form |
| Data validation | Generated request types + explicit `Validate()` methods for cross-field rules | Wire schemas live in the OpenAPI document |
| DB access | **pgx v5** (no ORM); **sqlc** for typed static queries | Textual SQL is the norm (TimescaleDB DDL, advisory locks, hot-path queries) — exactly what sqlc consumes |
| Migrations | **goose**, plain SQL files embedded via `embed.FS` | One migration chain per service database (`migrations/reporting/`, `migrations/engine/`) |
| Databases | **PostgreSQL 16 + TimescaleDB 2.x** (Reporting API), **PostgreSQL 16** plain (Engine) | Use the `timescale/timescaledb:latest-pg16` image for dev |
| Metrics store | **VictoriaMetrics single-node** | `-retentionPeriod=13` (months) |
| Metrics pipeline | **OpenTelemetry Collector (contrib)** | OTLP in, Prometheus Remote Write out |
| Service metrics | **prometheus/client_golang** | Every service exposes `/metrics` |
| Logging | **log/slog** (stdlib), JSON handler to stdout | Fields: `time`, `level`, `msg`, `service`, plus context |
| CLI | **cobra** | Engine and admin CLIs |
| HTTP client | **net/http**; **hashicorp/go-retryablehttp** for collectors and reconciliation adapters | VM queries, event delivery |
| OpenStack SDK | **gophercloud v2** + `gophercloud/utils` (clouds.yaml) | Reconciliation adapter (WP 1.13) |
| Kubernetes client | **client-go** + Gardener typed API (`github.com/gardener/gardener`) | Gardener watch (Phase 4) |
| JSON Schema validation | **santhosh-tekuri/jsonschema** v6 (draft 2020-12) | Resource-type `size` schemas, pricing model validation |
| Decimal arithmetic | **shopspring/decimal**, wrapped by `internal/core/money` | The only rounding/division entry points live in that package (§6) |
| Collector outbox | **modernc.org/sqlite** (CGO-free SQLite), WAL mode | Keeps collector binaries statically cross-compilable |
| YAML | **gopkg.in/yaml.v3** | clouds.yaml, pricing models, counter sources |
| Configuration | Env vars parsed with **caarlos0/env**; shared helper for the `*_FILE` convention | see §8 |
| Lint / format | **gofumpt** + **golangci-lint** | Config at repo root; `forbidigo` rules ban float use in money paths (§6) |
| Tests | **go test**; **testcontainers-go** for Postgres/TimescaleDB | Integration tests must run against real Postgres, not mocks |
| Containerization | **Docker**, one multi-stage root `Dockerfile` (`ARG CMD`) → static binary on distroless, one image per service | The same images run in dev and prod |
| Dev environment | **kind** + **kustomize** (`kubectl apply -k`); **Tilt** for the build → `kind load` → redeploy loop | Cluster config `deploy/kind/kind.yaml`; manifests under `deploy/kubernetes/` (§2) — the dev stack runs on Kubernetes from day one, dev/prod share the same kustomize base |
| Routing / ingress | **Gateway API** implemented by **Envoy Gateway**: one `Gateway`, per-service `HTTPRoute`/`GRPCRoute`, `TCPRoute` for Postgres in dev | The identical Gateway + routes in dev and prod; only hostnames and TLS issuer differ per overlay. No per-service NodePorts or port-forwards |
| Dev hostnames & TLS | **nip.io** wildcard DNS (`*.tally.127-0-0-1.nip.io` → 127.0.0.1) + **cert-manager** with a self-signed CA `ClusterIssuer` | Real URLs and HTTPS in dev without `/etc/hosts` edits; prod swaps hostnames and issuer (e.g. ACME), nothing else |
| CI | **GitHub Actions** | `lint` → `vet` → `test`, on every PR |

Dependency versions are pinned in `go.mod`/`go.sum`; this document intentionally does not pin
exact minor versions.

> **Decision note (2026-07, language switch)**: earlier drafts fixed Python/FastAPI, mainly
> because oslo.messaging (OpenStack's notification bus client) is Python-only. That argument is
> resolved: oslo notifications are plain JSON in an `oslo.message` envelope on RabbitMQ — the
> collector consumes them directly with an AMQP client (see WP 1.12). Everything else favors
> Go: static single-binary collectors on control planes, gophercloud, client-go for the
> Gardener watch, first-class Prometheus tooling, and a surrounding ecosystem
> (VictoriaMetrics, OTel Collector, Grafana) that is itself Go-native.

---

## 2. Repository layout (binding)

One Go module at the repo root (`github.com/b42labs/tally`); every binary is a `cmd/` package,
every shared implementation lives under `internal/` (nothing is importable from outside the
repo — deliberate: the wire contracts, not Go APIs, are the public interface).

```
tally/
├── README.md                      # concept document (do not modify casually)
├── roadmap/                       # these documents
├── go.mod / go.sum                # single module: github.com/b42labs/tally
├── Makefile                       # dev entry points: make up / dev / test / lint / migrate
├── Dockerfile                     # multi-stage, ARG CMD → builds ./cmd/${CMD}; one image per service
├── Tiltfile                       # dev loop: docker_build → kind load → redeploy on change
├── api/
│   └── reporting/openapi.yaml     # Reporting API contract (oapi-codegen input)
├── cmd/
│   ├── tally-reporting/           # Reporting API server
│   ├── tally-reporting-admin/     # admin CLI (cobra)
│   ├── tally-engine/              # Phase 3: engine CLI + scheduler
│   ├── tally-openstack-collector/ # Phase 1: OpenStack event collector
│   └── ...                        # Phase 4: further collectors/exporters
├── internal/
│   ├── core/                      # shared domain library (no I/O)
│   │   ├── event/                 # Event, payload envelope, Categorize()
│   │   ├── timeline/              # event → interval folding (shared by API & engine)
│   │   ├── money/                 # decimal helpers, rounding, JSON encoding
│   │   ├── ids/                   # deterministic event-id hashing
│   │   └── testkit/               # provider conformance test kit, fixtures
│   ├── reporting/                 # Reporting API service
│   │   ├── config/  store/  auth/  audit/
│   │   ├── httpapi/               # handlers: events, resources, resource types,
│   │   │                          #   projects, stats (Phase 2), internal/sync, health
│   │   ├── ingest/                # ingestion pipeline
│   │   ├── projection/            # current_resources incremental update + replay
│   │   ├── metrics/               # prometheus counters/gauges
│   │   └── reconciliation/        # adapter interface, diff engine, synthetic events
│   │       └── adapters/          # openstack (Phase 1), hetzner (Phase 4), ...
│   ├── engine/                    # Phase 3: metering & rating engine
│   ├── collector/                 # Phase 4: shared collector runtime (outbox, sender)
│   └── providers/
│       ├── openstack/             # oslo AMQP consumer + mapping (WP 1.12)
│       ├── hetzner/               # Phase 4
│       ├── gardener/              # Phase 4
│       └── harbor/                # Phase 4
├── migrations/
│   ├── reporting/                 # goose SQL chain (embedded via embed.FS)
│   └── engine/
├── pricing/                       # versioned pricing model YAML files (Phase 3)
├── deploy/
│   ├── kind/
│   │   └── kind.yaml              # kind cluster config: pinned node image; host ports 8081/8443/5432 → Gateway
│   └── kubernetes/
│       ├── base/                  # kustomize base — one directory per component
│       │   ├── kustomization.yaml
│       │   ├── gateway/           # GatewayClass + Gateway (http/https/postgres) + wildcard Certificate
│       │   ├── timescaledb/       # StatefulSet + PVC + Service + TCPRoute
│       │   ├── victoriametrics/   # StatefulSet + scrape.yaml ConfigMap + HTTPRoute
│       │   ├── otel-collector/    # Deployment + config.yaml ConfigMap + HTTPRoute/GRPCRoute
│       │   ├── reporting-api/     # Deployment + Service + HTTPRoute
│       │   └── grafana/           # Phase 2: provisioning + dashboards JSON (ConfigMaps) + HTTPRoute
│       └── overlays/
│           ├── dev/               # kind: namespace tally, nip.io hostnames, self-signed CA, replicas: 1
│           └── prod/              # real hostnames + issuer; added when first deployed to a real cluster
└── .github/workflows/ci.yaml
```

Naming rules:

- Binary/directory names under `cmd/` use `kebab-case`; Go package names are short, lower-case,
  no underscores (`timeline`, `httpapi`, `projection`).
- All API routes live under `/api/v1/...`; internal-only routes under `/internal/...`.
- Database identifiers use `snake_case`.
- Test fixtures live in `testdata/` next to their package (Go convention); golden files under
  `testdata/golden/`.

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
  platform name (`internal/core/ids.DeterministicEventID()` is the single implementation;
  the canonical timestamp rendering is RFC 3339 UTC, `time.RFC3339Nano`).
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

```go
func Categorize(eventType string) Category { // CREATE | DELETE | UPDATE
	parts := strings.Split(eventType, ".")
	if slices.Contains(parts, "create") {
		return CategoryCreate
	}
	if slices.Contains(parts, "delete") {
		return CategoryDelete
	}
	return CategoryUpdate
}
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
engine (Phase 3). One shared implementation: `internal/core/timeline`.

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

Single implementation in `internal/core/money`:

- All money math uses `decimal.Decimal` (shopspring/decimal). **`float32`/`float64` are
  forbidden** anywhere money, prices, or usage quantities are computed, stored, or serialized —
  enforced with `forbidigo` lint rules (no `decimal.NewFromFloat`, no `InexactFloat64` in money
  paths) plus a code-review checklist item. Decimals are constructed from strings or integers
  only.
- Divisions always go through the `money` helpers, which use explicit-precision
  `DivRound(…, 28)`; the package-level `decimal.DivisionPrecision` variable is never relied on.
- Database storage: `NUMERIC` (money: `NUMERIC(14,2)`); never `real`/`double precision`.
  pgx maps `NUMERIC` ↔ `decimal.Decimal` via the pgx-shopspring-decimal codec.
- Rounding mode: **`ROUND_HALF_UP`** (ties away from zero) — `money.Round2()` is the single
  rounding entry point (shopspring's `Round` has exactly these semantics).
- Per-dimension costs are computed at full precision, then rounded half-up to **2 decimal
  places per dimension per usage record**. All aggregates (resource, project, period) are sums
  of the rounded values — a total always equals the sum of its visible line items.
- One currency per pricing-model version (initially `EUR`). Never aggregate across currencies.
- JSON serialization: monetary and usage decimals are rendered as JSON numbers with their full
  quantized precision (e.g. `19.20` → `19.2` is *not* acceptable in exports; the `money`
  package provides a marshaller that preserves 2 decimal places for money).

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
- **Auth**: `Authorization: Bearer <token>` (details in Phase 1 WP 1.4).
- **Health**: every service exposes `GET /healthz` (liveness) and `GET /readyz` (readiness)
  without auth; semantics per the concept (§3.2).
- **Metrics**: every service exposes `GET /metrics` (Prometheus exposition) without auth,
  metric names prefixed `tally_`.

---

## 8. Configuration conventions

- Configuration exclusively via environment variables, prefix `TALLY_`, parsed with
  `caarlos0/env` in each service's `config` package.
- Common variables (every service): `TALLY_LOG_LEVEL` (default `INFO`),
  `TALLY_HTTP_PORT` (service default), `TALLY_METRICS_ENABLED` (default `true`).
- Secrets (DB URLs, tokens) are env vars too; support the `*_FILE` convention
  (`TALLY_DB_URL_FILE=/run/secrets/db-url`) for file-mounted secrets (Kubernetes Secret
  volumes).
- Every service ships a `.env.example` listing all variables with defaults and comments.

---

## 9. Testing standards

- **Unit tests**: pure logic (timeline folding, money, mapping tables) — no I/O, fast.
- **Integration tests**: against real Postgres/TimescaleDB via testcontainers; cover ingestion,
  dedup, projection replay, reconciliation diffing, metering runs.
- **Golden tests**: the worked examples from the concept are encoded with **exact** expected
  values (they are reproduced in the phase documents). Golden test fixtures live as JSON files
  under `testdata/golden/` next to their package.
- **Conformance kit** (`internal/core/testkit`): reusable assertions every provider collector
  must pass (schema validity, deterministic `event_id`s, payload envelope rules, buffering
  behavior).
- Every WP's acceptance criteria list the tests that must exist; CI runs them all.

---

## 10. Guardrails for code generation (read before every WP)

1. **Never** use `float32`/`float64` for money, prices, or usage quantities.
   `decimal.Decimal` + `NUMERIC` only.
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

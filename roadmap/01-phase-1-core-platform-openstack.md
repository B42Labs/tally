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
3. The **OpenStack provider** is fully integrated: `openstack-event-collector` (oslo
   notifications via AMQP → Tally events, buffered at-least-once), reconciliation adapter,
   Ceilometer → OTel Collector → VictoriaMetrics pipeline, and the OpenStack DB exporter
   deployed.
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
WP1.1 scaffolding ─▶ WP1.2 core library ─▶ WP1.3 API skeleton+DB ─▶ WP1.4 auth/audit
  ─▶ WP1.5 resource-type registry ─▶ WP1.6 ingestion ─▶ WP1.7 projection
  ─▶ WP1.8 query endpoints ─▶ WP1.9 project registry ─▶ WP1.10 reconciliation framework
  ─▶ WP1.11 service metrics ─▶ WP1.12 openstack-event-collector
  ─▶ WP1.13 openstack reconciliation adapter ─▶ WP1.14 metrics pipeline
  ─▶ WP1.15 vertical slice
```

---

### WP 1.1 – Repository scaffolding & dev cluster

**Create**

- `go.mod` / `go.sum` (module `github.com/b42labs/tally`, toolchain pinned), root
  `.golangci.yml` (incl. the conventions-§6 `forbidigo` money rules), root `Dockerfile`
  (multi-stage, `ARG CMD` → builds `./cmd/${CMD}` into a distroless image)
- `Makefile` targets: `up` (create the kind cluster if absent → install the pinned
  Envoy Gateway and cert-manager release manifests → build images → `kind load
  docker-image` → `kubectl apply -k deploy/kubernetes/overlays/dev` → wait for rollout),
  `down` (delete the kind cluster), `dev` (`tilt up`), `ca` (print the dev CA certificate
  for local trust), `test`, `lint` (golangci-lint), `fmt` (gofumpt), `migrate` (goose up,
  both chains), `generate` (oapi-codegen, sqlc); add-on versions are pinned in the Makefile
- `deploy/kind/kind.yaml` — cluster name `tally`, pinned `kindest/node` image, and exactly
  three `extraPortMappings`: `80`, `443`, `5432`, wired to the Envoy proxy Service (fixed
  NodePorts, pinned via an `EnvoyProxy` config in the dev overlay). **All** traffic enters
  through the Gateway — no per-service host ports.
- `deploy/kubernetes/base/gateway/` — `GatewayClass` `tally` (Envoy Gateway controller) and
  one `Gateway` `tally` with listeners `http` (:80, `RequestRedirect` → https), `https`
  (:443, wildcard certificate from a cert-manager `Certificate`), `postgres` (:5432, TCP;
  Gateway API experimental channel for `TCPRoute`). Dev hostname scheme
  `*.tally.127-0-0-1.nip.io` — nip.io resolves it to `127.0.0.1`, so dev has real URLs
  without `/etc/hosts` edits.
- `deploy/kubernetes/base/` (kustomize base) with components:
  - `timescaledb/` — StatefulSet, image `timescale/timescaledb:latest-pg16`, DB
    `tally_reporting`, PVC, readiness probe `pg_isready`, Service, `TCPRoute` on the
    `postgres` listener → dev: `db.tally.127-0-0-1.nip.io:5432` (hostname is cosmetic —
    TCP routing is by listener port)
  - `victoriametrics/` — StatefulSet, image `victoriametrics/victoria-metrics`, args
    `-retentionPeriod=13 -promscrape.config=/etc/vm/scrape.yaml`, scrape config mounted
    from a ConfigMap (source file `deploy/kubernetes/base/victoriametrics/scrape.yaml`,
    via `configMapGenerator`), PVC, Service, `HTTPRoute` → dev:
    `vm.tally.127-0-0-1.nip.io`
  - `otel-collector/` — Deployment, image `otel/opentelemetry-collector-contrib`, config
    mounted from a ConfigMap (source file
    `deploy/kubernetes/base/otel-collector/config.yaml`), Service; `HTTPRoute` → dev:
    `otlp.tally.127-0-0-1.nip.io` (OTLP/HTTP, :4318) and `GRPCRoute` → dev:
    `otlp-grpc.tally.127-0-0-1.nip.io` (OTLP/gRPC, :4317)
  - `reporting-api/` — Deployment, image built from the root `Dockerfile` with
    `CMD=tally-reporting`; liveness `/healthz`, readiness `/readyz`; DB URL from a Secret
    via the `*_FILE` convention (conventions §8); init container waiting on TimescaleDB
    readiness; Service, `HTTPRoute` → dev: `api.tally.127-0-0-1.nip.io`
- `deploy/kubernetes/overlays/dev/` — namespace `tally`, nip.io hostname patches on all
  routes, self-signed CA `ClusterIssuer` (cert-manager), `EnvoyProxy` config pinning the
  proxy Service NodePorts to match `kind.yaml`, dev-only Secret values, `replicas: 1`
  everywhere; the prod overlay later swaps hostnames and issuer — Gateway and routes stay
  identical
- `Tiltfile` — `docker_build` per service image (root `Dockerfile`, `ARG CMD`),
  `k8s_yaml(kustomize('deploy/kubernetes/overlays/dev'))`, resource grouping with the
  nip.io URLs attached as resource links (no port-forwards needed — the Gateway serves
  them); `tilt up` gives rebuild-on-change and streamed logs for all services
- `.github/workflows/ci.yaml`: `golangci-lint run` → `go vet ./...` → `go test ./...`
  (testcontainers-based integration tests included — CI needs Docker, not kind)
- `.env.example` files per service

**Acceptance criteria**

- `make up` creates the kind cluster, installs Envoy Gateway + cert-manager, and deploys
  the dev overlay; `kubectl get pods -n tally` shows all pods `Ready` (TimescaleDB,
  VictoriaMetrics, OTel Collector, Reporting API); the `Gateway` reports `Programmed`.
- `https://vm.tally.127-0-0-1.nip.io` serves the VictoriaMetrics UI (`curl --cacert
  <(make -s ca)` verifies cleanly; plain `http://` redirects to `https://`).
- `psql "host=db.tally.127-0-0-1.nip.io port=5432 ..."` reaches TimescaleDB through the
  Gateway's TCP listener (prerequisite for `make migrate`).
- `tilt up` turns green for every resource; changing Go code rebuilds and redeploys the
  affected service automatically.
- `make test` runs an empty test suite green in CI.

---

### WP 1.2 – Shared core library `internal/core`

**Create** `internal/core/...`:

`event` — wire types + validation:

```go
type PayloadEnvelope struct {
	State    *string        `json:"state,omitempty"`    // resource state AT/AFTER the event
	Size     map[string]any `json:"size,omitempty"`     // full-replacement size object
	Provider map[string]any `json:"provider,omitempty"` // raw provider data (audit only)
	// unknown extra fields are preserved on round-trip (custom (Un)MarshalJSON)
}

type Event struct {
	EventID      string          `json:"event_id"`      // 1..256 chars
	Timestamp    time.Time       `json:"timestamp"`     // RFC 3339 with offset; normalized to UTC
	EventType    string          `json:"event_type"`    // ^[a-z0-9_]+(\.[a-z0-9_]+)+$
	Platform     string          `json:"platform"`
	Cloud        string          `json:"cloud"`
	ResourceType string          `json:"resource_type"`
	ResourceID   string          `json:"resource_id"`
	ProjectID    string          `json:"project_id"`
	Source       Source          `json:"source"`        // "collector" (default) | "reconciliation"
	Payload      PayloadEnvelope `json:"payload"`
}

// Stored pairs an Event with its server-side received_at (ordering tiebreaker).
type Stored struct {
	Event
	ReceivedAt time.Time
}

func (e *Event) Validate() error           // field rules + cross-field rules below
func Categorize(eventType string) Category // CREATE | DELETE | UPDATE (conventions §4.2)
```

Cross-field validation (inside `Validate()`): `CREATE` events require `payload.state` and
`payload.size`; every event requires `payload.state` except `DELETE`.

`ids`:

```go
func DeterministicEventID(platform, cloud, resourceID, eventType string, ts time.Time) string {
	raw := cloud + ":" + resourceID + ":" + eventType + ":" + ts.UTC().Format(time.RFC3339Nano)
	sum := sha256.Sum256([]byte(raw))
	return platform + "-" + hex.EncodeToString(sum[:])
}

func SyntheticEventID(syncRunID, cloud, resourceType, resourceID, kind string) string {
	raw := syncRunID + ":" + cloud + ":" + resourceType + ":" + resourceID + ":" + kind
	sum := sha256.Sum256([]byte(raw))
	return "recon-" + hex.EncodeToString(sum[:])
}
```

`timeline` — the shared folding algorithm (conventions §5):

```go
type Interval struct {
	Start     time.Time      // inclusive
	End       *time.Time     // exclusive; nil = open (resource still in this config)
	State     string
	Size      map[string]any
	ProjectID string
}

type Timeline struct {
	Intervals []Interval
	CreatedAt *time.Time // ts of first CREATE (nil if history starts mid-life)
	DeletedAt *time.Time // ts of the DELETE event, if any
	Warnings  []string   // e.g. "history_starts_without_create"
}

func Build(events []event.Stored) Timeline {
	// 1. sort by (timestamp, received_at, event_id)
	// 2. fold: track (state, size, project_id); on billable changes (deep-compare size)
	//    close the current interval and open a new one at e.Timestamp
	// 3. DELETE closes the last interval at e.Timestamp and sets DeletedAt
	//    (state "deleted" produces no interval — deleted resources accrue nothing)
	// 4. drop zero-length intervals (start == end)
	// 5. events that change nothing produce no interval boundary
}
```

`money`: `Round2(d)` (quantize `0.01`, half-up), `Minutes(seconds int64)`
(`Decimal(seconds)/60`, quantize `0.0001`), `Div(a, b)` (`DivRound`, 28 digits), custom JSON
marshaller preserving 2 dp for money (conventions §6).

`testkit` — conformance kit (used by every provider, extended in Phase 4):
`AssertValidEvent(t, raw)`, `AssertDeterministicIDs(t, fn)`, fixture events builder.

**Tests (unit)**

- Timeline: single create; create→delete; create→resize→delete; equal-timestamp ties broken by
  `received_at` then `event_id`; no-op event does not split; zero-length interval dropped;
  history starting without CREATE yields warning; DELETE without prior events.
- `Categorize()` table test incl. `sync.create`, `volume.transfer.accept.end` → UPDATE.
- Money: `Round2(dec("2.025")) == dec("2.03")` (half-up), marshaller output `2.03`.

---

### WP 1.3 – Reporting API skeleton + database schema

**Create** `cmd/tally-reporting` (server main) and `internal/reporting/`: `config` (env
parsing, prefix `TALLY_REPORTING_`), `store` (pgx pool, transaction helpers), `httpapi`
(chi router wiring, middleware); goose setup + **migration 0001**
(`migrations/reporting/0001_init.sql`) with the full schema:

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
`TALLY_REPORTING_UNHEALTHY_THRESHOLD_S`, default 600), RFC 9457 error middleware, slog
setup, request-ID middleware.

**Config** (`.env.example`): `TALLY_REPORTING_DB_URL`, `TALLY_REPORTING_HTTP_PORT=8080`,
`TALLY_REPORTING_AUTH_MODE=enforced|disabled`, `TALLY_REPORTING_INTERNAL_TOKEN`,
`TALLY_INGEST_REQUIRE_SIZE_SCHEMA=false`, `TALLY_REPORTING_CLOUDS_CONFIG=/etc/tally/clouds.yaml`.

**Acceptance criteria**: `make migrate` creates the schema on the dev cluster (reaching
TimescaleDB through the Gateway's `postgres` listener at `db.tally.127-0-0-1.nip.io:5432`);
`GET https://api.tally.127-0-0-1.nip.io/healthz` returns 200 through the Gateway; hypertable +
compression policy verified in an integration test (`SELECT * FROM timescaledb_information.hypertables`).

---

### WP 1.4 – AuthN/AuthZ + audit log

**Create** `internal/reporting/auth`, `internal/reporting/audit`, admin CLI
(`cmd/tally-reporting-admin`, cobra).

- Token formats: ingest `tly_i_<hex32>`, api `tly_a_<hex32>` (random 32 bytes hex). Lookup by
  `sha256(token)`; constant-time compare not required (hash lookup), but tokens never logged.
- Auth middlewares (chi), exposed as request-context helpers:
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
  UUIDs; the middleware resolves them to `(cloud, external_id)` pairs and filters event/resource
  queries on those `project_id` values.
- OIDC extension point (implement interface, leave provider off): `TALLY_REPORTING_OIDC_JWKS_URL`
  — when set, `Bearer` JWTs are accepted, `role`/`projects` claims mapped like api_tokens.

**Tests**: 401/403 matrix per endpoint class; revoked token rejected; project-scoped token sees
only its projects (integration); audit rows written.

---

### WP 1.5 – Resource type registry

**Create** resource-type handlers in `internal/reporting/httpapi` + registry service module.

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

**Create** event ingestion handlers in `internal/reporting/httpapi` + `internal/reporting/ingest`.

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

1. **Schema validation** (`core/event` `Validate()`, incl. payload-envelope rules)
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

Internal API for reconciliation: `ingest.Ingest(ctx, tx, events, ingest.SourceReconciliation)`
— same pipeline minus auth/scope, same dedup + projection.

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

**Create** `internal/reporting/projection`.

Concurrency: before touching a resource's projection row, take a transaction-scoped advisory
lock **on the reporting DB** — the same lock the Phase-3 engine takes:

```sql
SELECT pg_advisory_xact_lock(
    hashtextextended(:cloud || ':' || :resource_type || ':' || :resource_id, 0));
```

Algorithm (per resource key, with its batch of just-inserted events):

```go
func Apply(ctx context.Context, tx pgx.Tx, key ResourceKey, newEvents []event.Stored) error {
	// newEvents sorted (ts, received_at, event_id)
	advisoryXactLock(ctx, tx, key)
	row := loadProjectionRow(ctx, tx, key) // SELECT ... FOR UPDATE
	oldest := newEvents[0]
	if row == nil || !oldest.Timestamp.Before(row.LastEventAt) {
		for _, e := range newEvents {
			row = applyIncremental(row, e) // cheap path
		}
		upsert(ctx, tx, row)
	} else { // out-of-order / late event
		replay(ctx, tx, key) // rebuild from full history
		metrics.ProjectionReplays.WithLabelValues(key.Cloud).Inc()
	}
}

func applyIncremental(row *Row, e event.Stored) *Row {
	switch event.Categorize(e.EventType) {
	case event.CategoryCreate:
		row.CreatedAt, row.DeletedAt = &e.Timestamp, nil
		row.State, row.Size = *e.Payload.State, e.Payload.Size
	case event.CategoryDelete:
		row.DeletedAt, row.State = &e.Timestamp, "deleted"
	default: // UPDATE
		if e.Payload.State != nil {
			row.State = *e.Payload.State
		}
		if e.Payload.Size != nil {
			row.Size = e.Payload.Size
		}
	}
	row.ProjectID, row.Platform = e.ProjectID, e.Platform
	row.LastEventType, row.LastEventAt, row.LastPayload = e.EventType, e.Timestamp, e.Payload
	return row
}

func replay(ctx context.Context, tx pgx.Tx, key ResourceKey) error {
	events := loadAllEvents(ctx, tx, key) // full history, ordered
	tl := timeline.Build(events)          // internal/core/timeline
	// final snapshot = last interval (or 'deleted' state); upsert row from it
}
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

**Create** query handlers in `internal/reporting/httpapi` (events GET, resources).

```
GET /api/v1/events?cloud=&platform=&project_id=&resource_type=&event_type=&source=&from=&to=&limit=&cursor=
    → { items: [Event+received_at], next_cursor }        sorted (timestamp, event_id)

GET /api/v1/resources/{cloud}/{resource_type}/{resource_id}/events
    → full ordered history of one resource

GET /api/v1/resources/{cloud}/{resource_type}/{resource_id}/lifecycle
    → { "resource": {...projection row...},
        "events":   [...ordered history...],
        "intervals": [ {"from": "...", "to": "...|null", "state": "...",
                        "size": {...}, "project_id": "..."} ],   // timeline.Build() output
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

**Create** project-registry handlers in `internal/reporting/httpapi` +
`internal/reporting/registry` service module.

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
a post-ingest hook interface `OnEvent(ctx, event)`; Phase 1 registers only a no-op default.

**Tests**: CRUD; active-uniqueness (409); close & recreate; temporal `at` queries (relation
closed in March invisible for `at` in April but visible for `at` in March); traversal with
depth/cycles; attributing-cycle rejection.

---

### WP 1.10 – Reconciliation framework (provider-agnostic)

**Create** `internal/reporting/reconciliation` (framework) + internal sync handlers in
`internal/reporting/httpapi`.

Cloud configuration (`TALLY_REPORTING_CLOUDS_CONFIG`, YAML):

```yaml
clouds:
  - cloud: os-prod-eu1
    platform: openstack
    adapter: openstack
    adapter_config:
      os_cloud: os-prod-eu1        # entry name in clouds.yaml (resolved via gophercloud/utils)
      include_octavia: false
```

Adapter interface:

```go
type ObservedResource struct {
	ResourceType string
	ResourceID   string
	ProjectID    string
	State        string         // normalized tally state
	Size         map[string]any
	CreatedAt    *time.Time     // real creation time if the API exposes it
	DeletedAt    *time.Time     // set only for resources reported as deleted
}

type Adapter interface {
	Platform() string
	// ListResources streams all live resources; MAY additionally yield recently
	// deleted resources (DeletedAt set) when the platform exposes them.
	ListResources(ctx context.Context, cfg map[string]any, since *time.Time,
	) iter.Seq2[ObservedResource, error]
}
```

Sync orchestration:

```
POST /internal/sync/{cloud}      (InternalAuth; triggered by CronJob every 10 min)
  → 200 { "sync_run_id": "...", "stats": {"created": n, "updated": n, "deleted": n} }
  → 409 if a sync for this cloud is already running (advisory lock on 'sync:'+cloud)
```

Diff algorithm (Go-flavored pseudocode):

```go
func sync(ctx context.Context, cloudCfg CloudConfig) (SyncStats, error) {
	run := insertSyncRun(cloud)
	observed := collect(adapter.ListResources(ctx, cfg, lastSuccess(cloud)))
	//        → map[(resource_type, resource_id)]ObservedResource
	db := loadProjectionRows(cloud) // incl. state='deleted'
	var synthetic []event.Event
	for key, obs := range observed {
		if obs.DeletedAt != nil { // platform reports real deletion
			if row, ok := db[key]; ok && row.State != "deleted" {
				synthetic = append(synthetic, ev("sync.delete", obs.DeletedAt, "deleted", nil))
			}
			continue
		}
		row, ok := db[key]
		switch {
		case !ok || row.State == "deleted": // missed create (or resurrection)
			synthetic = append(synthetic, ev("sync.create",
				coalesce(obs.CreatedAt, now()), obs.State, obs.Size))
		case changed(row, obs): // state, size, or project_id differ
			synthetic = append(synthetic, ev("sync.update", now(), obs.State, obs.Size))
		}
	}
	for key, row := range db { // in DB, not observed → missed delete
		if row.State != "deleted" && !observedLive[key] {
			synthetic = append(synthetic, ev("sync.delete", now(), "deleted", nil))
		}
	}
	ingest.Ingest(ctx, tx, synthetic, ingest.SourceReconciliation) // normal pipeline: dedup + projection
	completeSyncRun(run, stats)
}
```

- `event_id` = `ids.SyntheticEventID(run.ID, cloud, rtype, rid, kind)` —
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

**Create** `internal/reporting/metrics`; instrument WP 1.6/1.7/1.10 code paths.

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

**Create** `cmd/tally-openstack-collector` + `internal/providers/openstack`
(`osloamqp.go`, `mapping.go`, `outbox.go`, `sender.go`); image via the root Dockerfile.

Architecture: **consume → map → buffer (SQLite) → ack**, with an independent sender loop —
at-least-once end-to-end:

```
RabbitMQ (oslo notifications) ──▶ AMQP consumer ──▶ map to Tally event ──▶ INSERT into outbox ──▶ ack
                                                    (unmapped types: ack + skip metric)
outbox (SQLite, WAL) ──▶ sender loop: batch ≤500 ──▶ POST /api/v1/events ──▶ on 200: DELETE batch
                                                    on error: exponential backoff 1s→300s + jitter
```

- **Consumer** (`osloamqp.go`, `rabbitmq/amqp091-go`): oslo.messaging notifications are plain
  AMQP messages — no Python library required. The collector declares its **own durable queue**
  `tally-notifications` and binds it to the notification topic(s) (`TALLY_OSC_TOPICS`, default
  `notifications.info`) on each configured service exchange (`TALLY_OSC_EXCHANGES`, default
  `nova,neutron,cinder,glance`). An own queue replicates oslo's listener-pool semantics —
  Ceilometer keeps receiving its own copies untouched. The message body is the oslo envelope
  `{"oslo.version": "2.0", "oslo.message": "<json string>"}`; the inner document carries
  `message_id`, `event_type`, `timestamp`, `payload`. Manual acks with bounded prefetch
  (QoS): ack only after the outbox insert committed; on mapping crash → nack + requeue.
- **Outbox** (`outbox.go`): SQLite (modernc.org/sqlite) at `TALLY_OSC_BUFFER_PATH`
  (PVC/volume), WAL mode:
  `outbox(id INTEGER PRIMARY KEY AUTOINCREMENT, event_json TEXT NOT NULL, created_at TEXT NOT NULL)`.
  Backpressure: if outbox exceeds `TALLY_OSC_BUFFER_MAX_EVENTS` (default 1,000,000) → stop
  consuming (events wait on the bus), never drop.
- **Sender** (`sender.go`): batches in id order; a 200 (regardless of per-item rejects — those
  are dead-lettered server-side) deletes the batch; 4xx/5xx/network keeps it. `event_id` =
  oslo `message_id` ⇒ redelivery is safe.

**Mapping table** (`mapping.go` — data-driven table, one entry per oslo `event_type`; unmapped
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

> ⚠️ Exact oslo notification names/payloads vary by OpenStack release, and exchange/topic
> names depend on deployment configuration (`control_exchange`, `notification_topics`). The
> mapping table is **data**, not code — verifying it (and the exchange/queue bindings) against
> the target deployment via a notification dump is an explicit task in the acceptance criteria.
> Nova must run with `notify_on_state_change = vm_state` and `notification_format =
> unversioned` (or the collector handles versioned payloads — pick per deployment and
> document).

**Config**: `TALLY_OSC_AMQP_URL` (RabbitMQ), `TALLY_OSC_EXCHANGES`, `TALLY_OSC_TOPICS`,
`TALLY_OSC_CLOUD`, `TALLY_OSC_REPORTING_URL`, `TALLY_OSC_TOKEN`, `TALLY_OSC_BUFFER_PATH`,
`TALLY_OSC_BATCH_MAX=500`, `TALLY_OSC_FLUSH_INTERVAL_S=5`, `TALLY_OSC_BUFFER_MAX_EVENTS`.

**Observability**: `/healthz` (consumer connected + outbox writable), `/metrics`:
`tally_collector_consumed_total{event_type}`, `tally_collector_skipped_total{event_type}`,
`tally_collector_buffer_depth` (gauge), `tally_collector_delivered_total`,
`tally_collector_delivery_errors_total`, `tally_collector_oldest_buffered_seconds` (gauge).

**Tests**

- Unit: every mapping-table entry with a captured sample notification → exact Tally event
  (golden JSON fixtures under `testdata/golden/notifications/`); conformance kit
  (`internal/core/testkit`) passes for all produced events.
- Integration (RabbitMQ testcontainer): publish captured oslo envelopes → collector consumes,
  parses the envelope, maps, and buffers them (validates the hand-rolled AMQP layer).
- Integration: fake Reporting API — kill it, produce events, restart it → all events delivered
  exactly once by `event_id`; collector restart mid-buffer loses nothing (outbox survives).

---

### WP 1.13 – OpenStack reconciliation adapter

**Create** `internal/reporting/reconciliation/adapters/openstack.go` (dependency:
**gophercloud v2**; connects via the `os_cloud` entry from `adapter_config`, resolved through
`gophercloud/utils` clouds.yaml support).

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

**Tests**: unit tests against recorded API fixtures (JSON served via `httptest`), one test per
diff scenario wired through the WP 1.10 framework with this adapter mocked at the HTTP layer.

---

### WP 1.14 – Metrics pipeline: Ceilometer → OTel → VictoriaMetrics, DB exporter

**Create/modify**: `deploy/kubernetes/base/otel-collector/config.yaml`,
`deploy/kubernetes/base/victoriametrics/scrape.yaml` (both are ConfigMap sources — a change
re-rolls the pods via kustomize's generated ConfigMap names), docs under
`docs/openstack-metrics.md`.

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

**Acceptance criteria**: on the dev cluster, a metric pushed to
`https://otlp.tally.127-0-0-1.nip.io` (OTLP/HTTP through the Gateway) appears in
VictoriaMetrics (`https://vm.tally.127-0-0-1.nip.io/api/v1/query`); scrape configs load
without error; evaluation doc committed.

---

### WP 1.15 – Vertical slice (throwaway prototype, golden-numbers gate)

**Create** `cmd/tally-vertical-slice` (explicitly throwaway — Phase 3 replaces it; only
`internal/core/timeline` carries over).

CLI: `go run ./cmd/tally-vertical-slice --cloud os-prod-eu1 --project proj-456 --month 2026-03
--reporting-url ... --pricing pricing/prototype.yaml`

Steps: fetch instance events via `GET /api/v1/resources?...` + per-resource event queries →
`timeline.Build()` → clip intervals to the month → usage records (`minutes` from integer
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

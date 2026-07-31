# 03 – Phase 3: Metering & Rating Engine

> Prerequisites: Phase 1 complete (event history, projection, project registry).
> Read [00-conventions.md](00-conventions.md) first — especially §5 (timeline), §6 (money).

## Goal

A production **Metering & Rating Engine** (`cmd/tally-engine` + `internal/engine`) that:

1. **Meters** usage per resource per UTC calendar month into neutral units
   (splits on `size`/`state`/`project_id` changes; invariant-checked),
2. **Rates** usage with versioned pricing models (decimal arithmetic, normative rounding),
3. Implements the **billing period lifecycle**
   (`open → grace → finalized → corrected*`) with immutable finalized runs and delta-based
   correction runs,
4. Attributes **related costs** across the project graph without double billing,
5. **Exports** billing artifacts for external billing/ERP systems.

The concept's worked examples are the golden test suite (WP 3.11) — exact numbers, no tolerance.

**Out of scope**: pricing adjustments/kickbacks (Phase 5), non-OpenStack counter sources beyond
the configured interface (extended per provider in Phase 4).

## Decisions made by this document

| # | Decision | Rationale |
|---|---|---|
| D1 | The engine reads the Reporting DB **directly, read-only** (dedicated PG role), not via HTTP | Metering scans full event histories; paging that over HTTP is waste. The API stays the write path; the event schema is the stable contract |
| D2 | Internal time math in **integer seconds**; `usage.minutes = Decimal(seconds)/60` quantized to 4 dp | Exact coverage invariants; concept examples (whole minutes) reproduce exactly |
| D3 | Per-resource advisory locks are **not** taken for metering reads; instead: one engine-side advisory lock per period (no parallel runs) + `REPEATABLE READ` snapshot on the Reporting DB | `events` is append-only — a snapshot is consistent by construction. The concept's per-resource locks remain what the Phase-1 projection writer uses. Late arrivals during a run are caught by late-event detection (WP 3.9) |
| D4 | Relation validity for attribution: a relation applies to a period iff it **overlaps** the period (`valid_from < period_to AND (valid_to IS NULL OR valid_to > period_from)`); no intra-period proration of relations (v1 limitation, documented) | Satisfies the concept's "deleting a shoot in April does not detach March costs" in both directions |
| D5 | Runs auto-execute after grace via a scheduler `tick`; **finalization is manual by default** (`TALLY_ENGINE_AUTO_FINALIZE=false`) | Finalized data may reach ERP — a human gate is the safe default |
| D6 | Correction runs **fully re-meter** the period, then diff against the finalized run per `(resource key, project_id, dimension)`; only non-zero deltas become credit/debit records | Simple, deterministic, and reproducible; incremental diffing is an optimization with no correctness gain |
| D7 | Counter metrics come from a configurable `counter-sources.yaml` (two source kinds: `events` count and `metricsql` query) | Keeps the engine platform-agnostic; providers add entries, not code |
| D8 | Immutability of finalized runs is enforced by **database triggers**, not only application code | Guardrail 7 of the conventions |
| D9 | `usage` carries all size fields verbatim (numeric *and* string, e.g. `flavor`) plus `minutes`, `count: 1`, and counter metrics | Matches the concept's examples; string fields drive `type_modifiers` |

---

## Engine database schema (migration 0001, plain PostgreSQL 16)

```sql
CREATE TABLE billing_periods (
    period_from  TIMESTAMPTZ PRIMARY KEY,          -- e.g. 2026-03-01T00:00:00Z
    period_to    TIMESTAMPTZ NOT NULL,             -- exclusive
    status       TEXT NOT NULL DEFAULT 'open',     -- 'open' | 'grace' | 'finalized'
    finalized_run_id UUID,
    finalized_at TIMESTAMPTZ,
    CHECK (period_from < period_to)
);

CREATE TABLE runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    period_from     TIMESTAMPTZ NOT NULL,
    period_to       TIMESTAMPTZ NOT NULL,
    kind            TEXT NOT NULL DEFAULT 'regular',   -- 'regular' | 'correction'
    corrects_run_id UUID REFERENCES runs(id),          -- set for corrections
    pricing_version TEXT,
    status          TEXT NOT NULL DEFAULT 'running',
        -- 'running' | 'completed' | 'finalized' | 'superseded' | 'failed'
    clouds          TEXT[] NOT NULL DEFAULT '{}',      -- empty = all configured clouds
    stats           JSONB NOT NULL DEFAULT '{}',
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);
CREATE INDEX idx_runs_period ON runs (period_from, status);

CREATE TABLE usage_records (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id        UUID NOT NULL REFERENCES runs(id),
    cloud         TEXT NOT NULL,
    platform      TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL,
    project_id    TEXT NOT NULL,
    state         TEXT NOT NULL,
    from_ts       TIMESTAMPTZ NOT NULL,
    to_ts         TIMESTAMPTZ NOT NULL,
    seconds       BIGINT NOT NULL,                 -- exact; invariant checks use this
    usage         JSONB NOT NULL,                  -- {"minutes": 21600, "count": 1, "vcpus": 4, ...}
    CHECK (from_ts < to_ts)
);
CREATE INDEX idx_usage_run      ON usage_records (run_id);
CREATE INDEX idx_usage_resource ON usage_records (run_id, cloud, resource_type, resource_id, from_ts);
CREATE INDEX idx_usage_project  ON usage_records (run_id, project_id);

CREATE TABLE rated_records (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL REFERENCES runs(id),
    usage_record_id UUID NOT NULL REFERENCES usage_records(id),
    dimension       TEXT NOT NULL,                 -- usage key this cost refers to
    amount          NUMERIC(14,2) NOT NULL,        -- negative in correction runs
    currency        TEXT NOT NULL
);
CREATE INDEX idx_rated_run ON rated_records (run_id);

-- Delta records produced by correction runs (WP 3.9)
CREATE TABLE correction_deltas (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL REFERENCES runs(id),      -- the correction run
    corrects_run_id UUID NOT NULL REFERENCES runs(id),      -- the finalized run
    cloud           TEXT NOT NULL,
    platform        TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    dimension       TEXT NOT NULL,
    old_amount      NUMERIC(14,2) NOT NULL,
    new_amount      NUMERIC(14,2) NOT NULL,
    delta           NUMERIC(14,2) NOT NULL,        -- new - old; credit < 0, debit > 0
    currency        TEXT NOT NULL
);

CREATE TABLE pricing_models (
    version     TEXT PRIMARY KEY,                  -- e.g. '2026-03'
    valid_from  TIMESTAMPTZ NOT NULL UNIQUE,
    currency    TEXT NOT NULL,
    document    JSONB NOT NULL,                    -- the validated YAML as JSON
    imported_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE project_statements (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id     UUID NOT NULL REFERENCES runs(id),
    project_id TEXT NOT NULL,                      -- registry (cloud, external_id) rendered id
    document   JSONB NOT NULL,                     -- rating output doc (concept §3.4 format)
    total      NUMERIC(14,2) NOT NULL,
    currency   TEXT NOT NULL,
    UNIQUE (run_id, project_id)
);

-- D8: finalized runs are immutable — enforced in the DB
CREATE FUNCTION forbid_finalized_mutation() RETURNS trigger AS $$
DECLARE r_status TEXT;
BEGIN
    SELECT status INTO r_status FROM runs WHERE id = COALESCE(OLD.run_id, NEW.run_id);
    IF r_status = 'finalized' THEN
        RAISE EXCEPTION 'records of finalized run % are immutable', COALESCE(OLD.run_id, NEW.run_id);
    END IF;
    RETURN COALESCE(NEW, OLD);
END $$ LANGUAGE plpgsql;

CREATE TRIGGER trg_usage_immutable  BEFORE UPDATE OR DELETE ON usage_records
    FOR EACH ROW EXECUTE FUNCTION forbid_finalized_mutation();
CREATE TRIGGER trg_rated_immutable  BEFORE UPDATE OR DELETE ON rated_records
    FOR EACH ROW EXECUTE FUNCTION forbid_finalized_mutation();
CREATE TRIGGER trg_stmt_immutable   BEFORE UPDATE OR DELETE ON project_statements
    FOR EACH ROW EXECUTE FUNCTION forbid_finalized_mutation();
-- plus: forbid UPDATE on runs rows whose status = 'finalized' (separate trigger; the only
-- allowed transition into 'finalized' happens while status = 'completed')
```

**Config** (`.env.example`): `TALLY_ENGINE_DB_URL`,
`TALLY_ENGINE_REPORTING_DB_URL` (read-only role: `GRANT SELECT ON events, current_resources,
projects, project_relations`), `TALLY_ENGINE_VM_URL`, `TALLY_ENGINE_GRACE_HOURS=72`,
`TALLY_ENGINE_AUTO_FINALIZE=false`,
`TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES=infrastructure_tenant`,
`TALLY_ENGINE_COUNTER_SOURCES=/etc/tally/counter-sources.yaml`.

---

## Work packages

```
WP3.1 skeleton+CLI ─▶ WP3.2 reporting source ─▶ WP3.3 metering ─▶ WP3.4 counters
  ─▶ WP3.5 pricing ─▶ WP3.6 rating ─▶ WP3.7 attribution+statements
  ─▶ WP3.8 period lifecycle ─▶ WP3.9 corrections ─▶ WP3.10 export ─▶ WP3.11 golden suite
```

### WP 3.1 – Engine skeleton, DB, CLI

**Create** `cmd/tally-engine` + `internal/engine` per conventions layout; goose migration 0001
(`migrations/engine/0001_init.sql`, schema above); cobra CLI `tally-engine` with subcommands
(implemented across the WPs):

```
tally-engine periods list
tally-engine run --period 2026-03 [--clouds os-prod-eu1,...]
tally-engine finalize --period 2026-03 --run <run-id>
tally-engine detect-late --period 2026-03
tally-engine correct --period 2026-03
tally-engine pricing import pricing/2026-03.yaml
tally-engine pricing list
tally-engine export --run <run-id> --format json|csv --out ./out
tally-engine tick                      # scheduler entrypoint (CronJob, hourly)
```

`--period` format `YYYY-MM` → `[YYYY-MM-01T00:00:00Z, first-of-next-month)`.

**Acceptance**: migration applies; `tally-engine periods list` shows statuses; CLI has
`--help` for every command.

### WP 3.2 – Reporting source (read-only access layer)

**Create** `internal/engine/source`. All queries run inside one `REPEATABLE READ` transaction
per run (consistent snapshot, D3). The snapshot start time is recorded in
`runs.stats.snapshot_at`.

Candidate resources for a period (uses the projection as index — rows are never deleted):

```sql
SELECT cloud, platform, resource_type, resource_id
FROM current_resources
WHERE cloud = ANY(:clouds)
  AND (deleted_at IS NULL OR deleted_at >= :period_from)
  AND (created_at IS NULL OR created_at < :period_to)
```

Per candidate, the **full** event history up to the period end (state at period start requires
history from the beginning; retention is unlimited):

```sql
SELECT event_id, timestamp, received_at, event_type, project_id, payload
FROM events
WHERE cloud = :cloud AND resource_type = :rt AND resource_id = :rid
  AND timestamp < :period_to
ORDER BY timestamp, received_at, event_id
```

Also: project graph loaders (`projects`, `project_relations` with the D4 overlap predicate).

**Tests**: candidate query against seeded projections (created-before/within, deleted-before/
within/after period); snapshot isolation (concurrent insert not visible mid-run).

### WP 3.3 – Metering

**Create** `internal/engine/metering` (uses `internal/core/timeline`).

```go
func MeterResource(events []event.Stored, periodFrom, periodTo time.Time) []UsageDraft {
	tl := timeline.Build(events)
	var drafts []UsageDraft
	for _, iv := range tl.Intervals {
		start := laterOf(iv.Start, periodFrom)
		end := earlierOf(coalesce(iv.End, periodTo), periodTo)
		if !start.Before(end) {
			continue // outside period
		}
		seconds := int64(end.Sub(start) / time.Second)
		usage := mergeUsage(map[string]any{
			"minutes": money.Minutes(seconds), // Decimal, 4 dp
			"count":   1,
		}, iv.Size) // D9: size fields verbatim
		drafts = append(drafts, UsageDraft{
			State: iv.State, ProjectID: iv.ProjectID,
			FromTS: start, ToTS: end, Seconds: seconds, Usage: usage,
		})
	}
	return drafts
}
```

Splitting needs no extra code: `timeline.Build` already splits exactly on billable changes
(state, size, project_id) — the concept's split table (resize, shelve, retype, transfer,
worker.scale, hibernate, push, …) is covered generically.

**Invariants** (`internal/engine/invariants`) — checked per resource per period; any violation
⇒ run status `failed` with a violation report in `runs.stats`, **no partial output kept**:

1. **No gaps/overlaps**: drafts sorted by `from_ts`; `draft[i].to_ts == draft[i+1].from_ts`.
2. **Coverage**: `sum(seconds) == overlap_seconds(lifetime, period)` where lifetime =
   `[timeline start, deleted_at or ∞)` — computed independently from the timeline bounds.
3. **Traceability**: every boundary except period edges equals some event timestamp.
4. **Implicit count**: every draft has `usage.count == 1`.

**Tests (unit, exact)** — minutes per the concept examples:

| Scenario | Splits (March 2026) | minutes |
|---|---|---|
| Resize 03-16 (Example 1) | 03-01→03-16, →04-01 | 21600, 23040 |
| Hetzner upgrade 03-15 (Ex. 2) | 03-01→03-15, →04-01 | 20160, 24480 |
| Volume resize 03-10 + retype 03-20 (Ex. 3) | 3 splits | 12960, 14400, 17280 |
| Shoot scale 03-12, hibernate 03-25→03-28 (Ex. 4) | 4 splits | 15840, 18720, 4320, 5760 |
| Harbor storage change 03-18 (Ex. 5) | 2 splits | 24480, 20160 |
| Power off 03-11→03-21 (E2E) | 3 splits | 14400, 14400, 15840 |

Plus: created mid-month; deleted mid-month; created+deleted within month; existed all month
(44640); event exactly at period boundary (belongs to next period — half-open); resource
whose history starts without CREATE (bills from first event + warning in `runs.stats`);
ownership transfer (usage before T on old `project_id`, after T on new — two drafts).

### WP 3.4 – Counter metrics

**Create** `internal/engine/counters`; config file `counter-sources.yaml`:

```yaml
sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: >
      sum(increase(ceilometer_network_outgoing_bytes{cloud="{cloud}",
          resource_id="{resource_id}"}[{window}])) / 1e9
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
    event_type: repository.pull
```

- `kind: metricsql` → `GET {VM_URL}/api/v1/query?query=<rendered>&time={to_ts}` with
  `{window} = to_ts - from_ts` (rendered like `360h`); result parsed as `Decimal`, quantized
  4 dp; empty result = `0`. Placeholders: `{cloud}`, `{resource_id}`, `{project_id}`, `{window}`.
- `kind: events` → `SELECT count(*) FROM events WHERE cloud=… AND resource_type=… AND
  resource_id=… AND event_type=:et AND timestamp >= :from AND timestamp < :to` (per usage
  interval — counters are sliced per split, as in concept Example 5).
- Counter values are merged into the interval's `usage` object.
- Failures: a failing metricsql source fails the run (billing data must not silently omit
  revenue-relevant counters); per-source `required: false` opt-out exists for
  reporting-only metrics.

**Tests**: rendering, slicing per interval, Decimal parse, empty result, required-failure.

### WP 3.5 – Pricing model

**Create** `internal/engine/pricing` + `pricing/` directory at repo root; the concept §3.4 YAML (versions,
`valid_from`, `currency`, `pricing.<platform>.<resource_type>.dimensions[]`,
`state_modifiers`, `type_modifiers`) is the format. Validation schema
(`pricing.schema.json`, JSON Schema 2020-12) — core constraints:

- `version` (string, required, unique), `valid_from` (date-time), `currency` (ISO 4217),
- each dimension: `metric` (string), `type` (`"time_gauge" | "counter"`), exactly one of
  `price_per_unit_hour` (time_gauge) / `price_per_unit` (counter); prices are **strings or
  numbers parsed as Decimal — never float** (decode YAML scalars into `string`, then
  `decimal.NewFromString` — never through `float64`),
- `state_modifiers` / `type_modifiers`: map string → Decimal ≥ 0.

`pricing import`: validate → insert (`INSERT` only; **re-importing an existing version with
different content is an error** — versions are immutable; fixing a price = new version).
Version selection for a period: `SELECT * FROM pricing_models WHERE valid_from <= :period_from
ORDER BY valid_from DESC LIMIT 1` → error if none.

**Tests**: schema validation failures; immutability on re-import; version selection incl.
boundary `valid_from == period_from`.

### WP 3.6 – Rating

**Create** `internal/engine/rating`. Pure function over usage records + pricing document —
no I/O.

Per usage record, per dimension of `pricing[platform][resource_type].dimensions`:

```go
stateMod := modifierOr1(pm.StateModifiers, record.State)
typeMod := modifierOr1(pm.TypeModifiers, record.Usage.String("type"))

var cost decimal.Decimal
switch dim.Type {
case "time_gauge":
	qty := record.Usage.Decimal(dim.Metric) // e.g. vcpus; count; size_gb — missing key = 0
	cost = money.Div(record.Usage.Decimal("minutes"), dec(60)).
		Mul(qty).Mul(dim.PricePerUnitHour).Mul(stateMod).Mul(typeMod)
case "counter":
	qty := record.Usage.Decimal(dim.Metric)
	cost = qty.Mul(dim.PricePerUnit) // modifiers do NOT apply
}

amount := money.Round2(cost) // half-up, 2 dp — per dimension per record
```

- Missing metric key → amount `0.00` (still emitted: traceability, e.g. free `pulls`).
- One `rated_records` row per (usage record × dimension). Aggregates are sums of the rounded
  amounts (conventions §6).
- Resource types without a pricing entry: skipped + listed in `runs.stats.unpriced` (warning —
  a resource type that should be billed but has no price must be visible, not silently free).

**Tests (unit, exact — from the concept E2E example)**:

| record | dimension | expected |
|---|---|---|
| active 240 h | vcpus / ram_gb / disk_gb / egress(18.0) | 19.20 / 9.60 / 19.20 / 1.62 |
| shutoff 240 h (mod 0.5) | vcpus / ram_gb / disk_gb / egress(0) | 9.60 / 4.80 / 9.60 / 0.00 |
| active 264 h | vcpus / ram_gb / disk_gb / egress(22.5) | 21.12 / 10.56 / 21.12 / **2.03** (2.025 half-up) |

Volume type modifier: hdd 200 GB, 288 h → `288 × 200 × 0.0001 × 0.5 = 2.88`. Shelved instance
→ all time_gauge dims `0.00`. Floating IP via `count`: full March
`744 × 1 × 0.005 = 3.72`.

### WP 3.7 – Attribution & project statements

**Create** `internal/engine/attribution`, `internal/engine/statements`.

1. Map usage/rated records to **registry projects** via `(cloud, project_id)` =
   `(projects.cloud, projects.external_id)`. Unregistered project ids: billed standalone +
   listed in `runs.stats.unregistered_projects` (warning).
2. Load relations of attributing types (config, default `infrastructure_tenant`) that overlap
   the period (D4). Build the attribution graph over registry project UUIDs.
3. **Exclusive attribution** (concept §3.4): BFS from every project that is *not* itself
   attributed away; a project reachable via attributing relations is billed **only** under its
   attributor. Shortest path wins; ties broken by smallest relation `id`; multiple paths ⇒
   warning in `runs.stats`. Cycle-safety: visited set (creation-side prevention exists in
   Phase 1, WP 1.9 — do not rely on it alone).
4. Statement document per top-level project — exactly the concept's rating output format:
   `line_items` (per resource, with `periods[]`: state, hours, usage, per-dimension cost,
   state_modifier, resource total) + `related_costs[]` (per attributed project:
   relation_type, project, its line_items, total) + `total`, `currency`. `hours` in the
   document = `minutes / 60` quantized 2 dp (display only; not used in math).
5. Persist to `project_statements`.

**Golden test** (concept §3.4 related-costs example): Gardener `team-alpha` with shoot
(744 h × 3 workers × 0.10 = **223.20**) + attributed OpenStack tenant `shoot-abc-os-tenant`
(worker m1.xlarge 8/16/160: 119.04 + 59.52 + 119.04 = **297.60**) ⇒ statement total
**520.80**, and `shoot-abc-os-tenant` gets **no own statement**.

### WP 3.8 – Billing period lifecycle & runs

**Create** `internal/engine/runs`, `internal/engine/scheduler`.

Run orchestration (`tally-engine run`):

```
1. pg_advisory_xact_lock(hashtextextended('period:' || :period_from, 0))   -- engine DB; no parallel runs
2. refuse if billing_periods.status = 'finalized' (regular runs; corrections go via WP 3.9)
3. INSERT runs (status='running', pricing_version=<resolved>)
4. metering (WP 3.3/3.4) → invariants → rating (WP 3.6) → attribution/statements (WP 3.7)
5. on success, in one transaction:
     UPDATE runs SET status='superseded'
       WHERE period_from=:pf AND kind='regular' AND status='completed';
     UPDATE runs SET status='completed', completed_at=now() WHERE id=:run_id;
6. on any failure: status='failed', stats.error populated; previous completed run stays valid
```

Superseded runs' records stay in the DB (audit) but are excluded from all exports/queries by
default (`runs.status = 'completed' | 'finalized'` filter).

Finalization (`tally-engine finalize`): requires run `status='completed'` for that period;
transaction: `runs.status='finalized'`, `billing_periods.status='finalized'`,
`finalized_run_id`, `finalized_at`. After commit, the D8 triggers make the data immutable.
Re-running `run --period` on a finalized period → hard error pointing to `correct`.

Scheduler (`tally-engine tick`, CronJob hourly):

```
for each month M with period_to <= now():
    ensure billing_periods row exists
    if status == 'open'  and now() >= period_to:                        status = 'grace'
    if status == 'grace' and now() >= period_to + GRACE_HOURS:
        if no completed/finalized regular run: execute run(M)
        if TALLY_ENGINE_AUTO_FINALIZE and run completed: finalize(M)
```

**Tests**: parallel `run` blocked (advisory lock); supersede semantics; re-run after
finalization refused; trigger blocks UPDATE/DELETE on finalized records (expect DB exception);
tick state machine (freeze time via injected `now()`).

### WP 3.9 – Corrections

**Create** `internal/engine/corrections`.

Late-event detection (`tally-engine detect-late --period`), against the Reporting DB:

```sql
SELECT cloud, resource_type, resource_id, count(*), max(received_at)
FROM events
WHERE timestamp >= :period_from AND timestamp < :period_to
  AND received_at > :finalized_snapshot_at          -- runs.stats.snapshot_at of the finalized run
GROUP BY 1, 2, 3
```

Correction run (`tally-engine correct --period`, D6):

1. Requires a finalized run F. `INSERT runs (kind='correction', corrects_run_id=F, ...)`.
2. **Full re-meter + re-rate** of the period with the **same pricing version as F**
   (corrections fix usage, not prices) — output stored under the correction run like a normal
   run (usage_records, rated_records, statements).
3. Diff per `(cloud, platform, resource_type, resource_id, project_id, dimension)`:
   `old = Σ rated F`, `new = Σ rated correction`; emit `correction_deltas` rows where
   `delta = new − old ≠ 0.00`.
4. Correction statements: per project a credit-note document — delta line items only,
   referencing `corrects_run_id`; negative totals are credits.
5. Corrections are themselves finalizable (immutable) and correctable — chain via
   `corrects_run_id` (a later correction diffs against *the finalized correction's* new
   amounts, i.e. against the latest finalized truth, not F twice; implement as: baseline =
   F's amounts patched by all finalized corrections in `started_at` order).

**Golden test**: VM `abc-123` finalized as active all March (no egress):
vcpus 59.52 + ram 29.76 + disk 59.52 = **148.80**. Late events then reveal
power_off 03-11 / power_on 03-21. Correction re-meters to 48.00 + 24.00 + 52.80 = 124.80 and
must emit exactly: vcpus **−9.60**, ram_gb **−4.80**, disk_gb **−9.60** ⇒ credit **−24.00**.
Running `correct` again immediately → zero deltas, no rows.

### WP 3.10 – Export / ERP integration

**Create** `internal/engine/export` (JSON + CSV writers, interface `BillingExporter`).

- `tally-engine export --run <id> --format json --out ./out/` → one
  `statement-{project}.json` per project (the WP 3.7 document) + `run.json` (run metadata,
  pricing version, stats).
- CSV format (`rated.csv`): `run_id, kind, corrects_run_id, period_from, period_to, cloud,
  platform, resource_type, resource_id, project_id, state, from_ts, to_ts, dimension,
  quantity, amount, currency` — one row per rated record; corrections export `deltas.csv`
  from `correction_deltas` (credit/debit line items referencing the original run).
- `BillingExporter` interface (`Export(ctx, run) error`) so an ERP adapter (SFTP drop, REST
  push) can be added without touching the engine; file export is the MVP implementation.
- Money serialization per conventions §6 (2 dp preserved).

**Tests**: golden file comparison for both formats over the WP 3.11 fixture set.

### WP 3.11 – Golden test suite (phase gate)

**Create** `internal/engine/testdata/golden/` — end-to-end: seeded Reporting DB (events) +
pricing YAML → `run` → assert exact usage minutes, per-dimension amounts, statement totals:

| Golden case | Source | Asserts |
|---|---|---|
| `instance_resize` | concept Ex. 1 | minutes 21600/23040; both records' dims |
| `hetzner_upgrade` | Ex. 2 | minutes 20160/24480 |
| `volume_resize_retype` | Ex. 3 | minutes 12960/14400/17280; hdd modifier 0.5 |
| `shoot_scale_hibernate` | Ex. 4 | 4 splits; hibernated dims 0.00 |
| `harbor_counters` | Ex. 5 | counters sliced per split (812/47/38.5 then 711/23/31.2) |
| `e2e_power_cycle` | §3.4 E2E | subtotals 49.62 / 24.00 / 54.83; total **128.45** (incl. egress 18.0 / 0 / 22.5 via a stubbed metricsql source) |
| `related_costs` | §3.4 Gardener | 223.20 + 297.60 = **520.80**; tenant not billed directly |
| `correction_credit` | WP 3.9 | deltas −9.60/−4.80/−9.60 |
| `reproducibility` | any | re-running the same period twice ⇒ byte-identical statements (modulo run ids/timestamps) |

---

## Phase exit criteria

1. All golden cases exact; invariant violations abort with reports (negative test included).
2. Immutability proven at the DB level (trigger tests).
3. Reproducibility: same inputs ⇒ identical outputs across runs and across re-rating with the
   same pricing version.
4. A full month of dev-stack OpenStack data meters, rates, finalizes, exports without warnings
   (or with each warning triaged).
5. Scheduler drill: simulated clock walk over a period boundary drives
   `open → grace → completed run`; finalization performed manually; late event → detect-late
   lists it → correction produces the expected credit.

## Risks & edge cases

- **Timeline start without CREATE** (onboarding an existing cloud): resources bill from their
  first event — for the very first period after onboarding, run reconciliation *before* the
  first metering run so `sync.create` events (with real `created_at` where available) exist.
- **Clock skew / poll-time deletes**: synthetic deletes carry poll time (concept's accepted
  limitation) — up to one sync interval of overbilling; monitored via Phase-2 drift alerts.
- **Huge event histories**: candidate loop is per-resource and streamable; do not load all
  clouds' events at once. If replay-from-genesis becomes slow (years of data), add periodic
  per-resource snapshots — *documented future optimization, not built now*.
- **Decimal ↔ JSONB**: PostgreSQL JSONB numbers are arbitrary-precision — always bind
  usage/money values as `decimal.Decimal` (pgx handles `NUMERIC` via the shopspring codec;
  for JSONB serialize via the `internal/core/money` marshaller), never through `float64`
  round-trips.

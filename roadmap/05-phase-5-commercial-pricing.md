# 05 – Phase 5: Commercial Pricing & Partner Models

> Prerequisites: Phase 3 (rating engine, statements, finalization) and the Phase-1 project
> registry. Read [00-conventions.md](00-conventions.md) first.
> Per the concept, this phase requires **no schema changes** — it uses `projects` rows with new
> `platform` values, additive `relation_type` values, and the `project_relations.metadata`
> JSONB column.

## Goal

1. **Meta-projects** (`member_of`) group real projects under customers.
2. **Reseller relations** (`managed_by`) link projects to partner entities.
3. The **rating engine resolves `pricing_adjustments`** from relation metadata — discounts,
   surcharges, project discounts, kickbacks — deterministically and reproducibly.
4. **Kickback reporting** aggregates partner commissions per reseller per billing period.
5. **Volume/loyalty discounts** ride on `member_of` relation metadata.

## Decisions made by this document

| # | Decision | Rationale |
|---|---|---|
| D1 | Grouping/partner entities are `projects` rows with `platform="meta"` / `platform="partner"` and `cloud` set to the same literal (`"meta"` / `"partner"`) | Satisfies `UNIQUE (cloud, external_id)` without schema change; no resources ever reference these projects |
| D2 | `pricing_adjustments` in relation metadata are validated at write time (Reporting API) against a JSON Schema — invalid adjustments are rejected with 422, not discovered at rating time | A malformed rate must not surface for the first time during a billing run |
| D3 | Application order (fixed): `surcharge` → `discount` → `project_discount` → `kickback`; surcharges add on base (`base × Σ rates`), discounts and project_discounts stack **multiplicatively** on the surcharged amount, kickbacks are computed on the resulting net and emitted as **separate line items** that do not change the customer's net | Concept §3.4 rating-extension rules, made arithmetic-precise |
| D4 | Same-type adjustments are ordered by relation `id` (UUID sort) — reproducibility | Concept requirement |
| D5 | `scope` grammar: `"all"` \| `"<platform>"` \| `"<platform>.<resource_type>"`; an adjustment's base is the sum of rated amounts matching its scope | Concept names `"all"` and `"openstack.instance"`; platform-wide is the obvious middle tier |
| D6 | Adjustments are collected by BFS over **outgoing** adjustment-carrying relations (`managed_by`, `member_of`; configurable) up to `TALLY_ENGINE_ADJUSTMENT_DEPTH` (default 3), relations valid per the Phase-3 D4 overlap rule; each relation's adjustments apply once (dedupe by relation id) | Enables inherited customer-level discounts per the concept's transitivity note |
| D7 | Adjustment amounts are rounded half-up to 2 dp **per adjustment line item**; the chain applies rounded amounts, so every total equals the sum of its visible line items | Conventions §6 applied to adjustments |
| D8 | Kickback beneficiaries are the relation **target** projects (`platform="partner"`); kickbacks land in a dedicated `adjustment_records` table for aggregation | Reporting needs a queryable source, not just JSONB documents |

---

## Work packages

```
WP5.1 registry conventions ─▶ WP5.2 adjustment validation ─▶ WP5.3 rating extension
  ─▶ WP5.4 kickback reporting ─▶ WP5.5 volume/loyalty discounts ─▶ WP5.6 golden suite
```

### WP 5.1 – Meta-projects & partner entities (registry conventions)

**Modify** `services/reporting-api` — configuration and docs only:

- Reserve `platform` values `meta` and `partner` (constant list `VIRTUAL_PLATFORMS`); events
  carrying a virtual platform are **rejected at ingest** (`reason="schema: virtual platform"`)
  — these projects never own resources.
- New relation types (documentation + type list): `member_of` (project → meta-project),
  `managed_by` (project → partner). Both are **non-attributing** (they must *not* join the
  Phase-3 exclusive-attribution set — a reseller is not billed the customer's usage; verify by
  test).
- Admin CLI sugar: `tally-reporting-admin create-meta-project --external-id customer-alpha
  --name "Customer Alpha"` and `create-partner --external-id partner-corp --name "Partner
  Corp"` (thin wrappers over `POST /api/v1/projects`).

Example target state (concept §3.4):

```
projects: (platform=partner, cloud=partner, external_id=partner-corp)
          (platform=meta,    cloud=meta,    external_id=customer-alpha)
relations: customer-proj-1 ─managed_by→ partner-corp   (metadata: pricing_adjustments…)
           team-alpha-os   ─member_of→  customer-alpha
           team-alpha      ─member_of→  customer-alpha
```

**Tests**: virtual-platform events rejected; relations to/from virtual projects CRUD like any
other; attribution set unaffected by `managed_by`/`member_of` (Phase-3 golden re-run).

### WP 5.2 – `pricing_adjustments` validation (Reporting API)

**Create** `adjustments_schema.json` in `internal/core` (embedded via `embed.FS`, shared with
the engine); enforce it in `POST/PATCH /api/v1/projects/{id}/relations` whenever
`metadata.pricing_adjustments` is present:

```json
{ "type": "array", "minItems": 1,
  "items": { "type": "object",
    "required": ["type", "rate", "scope"],
    "properties": {
      "type":  { "enum": ["discount", "kickback", "surcharge", "project_discount"] },
      "rate":  { "type": "string", "pattern": "^0(\\.\\d{1,6})?$|^1(\\.0{1,6})?$" },
      "scope": { "type": "string",
                 "pattern": "^all$|^[a-z0-9_]+(\\.[a-z0-9_]+)?$" },
      "description": { "type": "string", "maxLength": 500 }
    },
    "additionalProperties": false } }
```

- `rate` is a **string** (`"0.15"`) parsed as `Decimal` — floats never enter the pipeline
  (guardrail 1). Range `[0, 1]`.
- Changing adjustments = close the relation (`DELETE` → `valid_to`) and create a successor —
  past periods keep resolving the old relation (temporal validity does the versioning; PATCH
  of `metadata.pricing_adjustments` on a relation that has been used by a finalized run is
  rejected with 409 — check against the engine? **Simplification**: PATCH of
  `pricing_adjustments` is always rejected with 409 + hint "close and recreate"; only
  `valid_to` and non-pricing metadata are patchable).

**Tests**: schema acceptance/rejection matrix; PATCH-rejection; close-and-recreate flow
preserves the old relation row.

### WP 5.3 – Rating engine extension: resolve & apply adjustments

**Create** `internal/engine/adjustments`; extend WP 3.7 statements.
Engine migration adds:

```sql
CREATE TABLE adjustment_records (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id        UUID NOT NULL REFERENCES runs(id),
    project_id    TEXT NOT NULL,               -- the billed project
    relation_id   UUID NOT NULL,               -- provenance (auditability)
    beneficiary   TEXT,                        -- partner external_id (kickbacks only)
    type          TEXT NOT NULL,               -- discount|kickback|surcharge|project_discount
    scope         TEXT NOT NULL,
    rate          NUMERIC(8,6) NOT NULL,
    base          NUMERIC(14,2) NOT NULL,      -- amount the rate was applied to
    amount        NUMERIC(14,2) NOT NULL,      -- signed: discounts negative
    currency      TEXT NOT NULL
);
CREATE INDEX idx_adjustments_run ON adjustment_records (run_id, beneficiary);
-- + the WP 3.8 immutability trigger on this table
```

Resolution & application per project statement (after WP 3.7 produced `base` line items;
language-neutral pseudocode — all arithmetic via `internal/core/money` decimals):

```
applyAdjustments(statement, relationsAtPeriod, cfg) -> Statement:
    adjs = collect(statement.project, relationsAtPeriod,
                   types = cfg.adjustmentRelationTypes,           // ['managed_by','member_of']
                   depth = cfg.adjustmentDepth)                   // BFS, dedupe by relation id
    sort adjs by (ORDER[adj.type], adj.relationID)                // D3 + D4

    scopedBase(scope) -> Decimal:                                 // D5, over rated amounts
        Σ r.amount for r in statement.rated
          where matches(scope, r.platform, r.resourceType)

    // running net per scope partition — starts from the scoped bases
    for a in adjs where a.type == "surcharge":
        amount = Round2(scopedBase(a.scope) × a.rate)             // positive
        emit(a, amount); add to running net
    for a in adjs where a.type in ("discount", "project_discount"):
        // multiplicative on the running net of its scope
        amount = Round2(−runningNet(a.scope) × a.rate)
        emit(a, amount); add
    net = baseTotal + Σ emitted amounts
    for a in adjs where a.type == "kickback":
        amount = Round2(runningNet(a.scope) × a.rate)             // separate item, net unchanged
        emit(a, amount, beneficiary = a.relationTarget)
    statement.document += { base_cost: baseTotal, adjustments: [...],
                            net_cost: net, kickback_total: Σ kickbacks }
```

- Statement JSON extends the concept's output example exactly (`base_cost_eur`,
  `adjustments[]` with `type`, `relation_type`, `relation_target`, `rate`, `base_eur` for
  kickbacks, `amount_eur`, then `net_cost_eur`, `kickback_eur`).
- Projects without adjustments produce byte-identical statements to Phase 3 (regression guard).
- Corrections (WP 3.9) re-apply adjustments on the re-metered amounts with the **relations as
  valid for the period** — deltas therefore include adjustment effects automatically.

**Tests**: order determinism (shuffled input relations ⇒ identical output); scope partitioning;
multiplicative stacking (two discounts 10 % + 15 % on 100.00 → 100 → −10.00 → −13.50 ⇒ net
76.50); kickback does not change net; depth-limited inheritance; relation closed before the
period ⇒ no adjustment; overlap rule (closed mid-period ⇒ still applies — Phase-3 D4).

### WP 5.4 – Kickback reporting

**Create** `tally-engine kickbacks --period 2026-03 [--run <id>] [--format json|csv]`:

```sql
SELECT beneficiary, currency, sum(amount) AS kickback_total,
       count(DISTINCT project_id) AS projects
FROM adjustment_records
WHERE run_id = :run AND type = 'kickback'
GROUP BY beneficiary, currency
ORDER BY beneficiary
```

- JSON output: per beneficiary the total plus per-project breakdown (`project_id`, `base`,
  `rate`, `amount`) — the partner-facing settlement document.
- CSV: `period_from, period_to, run_id, beneficiary, project_id, scope, rate, base, amount,
  currency`.
- Export integrates with WP 3.10 (`BillingExporter` gains `export_kickbacks(run)`).
- Only `completed`/`finalized` runs are reportable; correction runs produce kickback deltas
  the same way (negative when usage was corrected down).

**Tests**: aggregation over multiple projects/resellers; correction-delta kickbacks.

### WP 5.5 – Volume & loyalty discounts

No new mechanics — codify the pattern and its guardrails:

- Customer-group discount = `pricing_adjustments` of type `project_discount` on the
  **`member_of`** relation (inherited via D6 BFS to every member project).
- Volume tiers are **not** auto-computed in this phase: operations sets/updates the rate per
  period explicitly (close + recreate the relation per WP 5.2). Automatic tiering (rate as a
  function of period usage) is documented as a future extension — it would make rating
  non-monotonic w.r.t. late events and needs its own correction semantics; do not build it
  ad hoc.
- Add an optional customer **rollup export**: `tally-engine export --run <id>
  --rollup member_of` → per meta-project one document summing its members' statements
  (reporting convenience only; attribution and billing stay per project).

**Tests**: member inherits the discount; non-members unaffected; rollup totals = Σ member
totals.

### WP 5.6 – Golden suite (phase gate)

| Case | Setup | Exact expectations |
|---|---|---|
| `reseller` (concept §3.4) | `customer-proj-1 ─managed_by→ partner-corp`, discount 0.15 + kickback 0.10, scope `all`; base 1200.00 | discount **−180.00**, net **1020.00**, kickback **102.00** (base_eur 1020.00); customer net unchanged by kickback |
| `scoped_discount` | discount 0.20 scope `openstack.instance`; project with instance 100.00 + volume 50.00 | adjustment −20.00; total 130.00 |
| `inherited_member_discount` | `proj ─member_of→ customer-alpha` (project_discount 0.05), depth 2 | −5 % on the member's net |
| `order_and_stacking` | surcharge 0.10 + discount 0.15 + kickback 0.10 on base 1000.00 | surcharge +100.00 → 1100.00; discount −165.00 → net 935.00; kickback 93.50 |
| `temporal` | relation closed 2026-03-15 | applies to March (overlap), not to April |
| `phase3_regression` | no adjustment relations | statements byte-identical to Phase-3 goldens |

---

## Phase exit criteria

1. WP 5.6 golden suite exact; Phase-3 golden suite still green (no regression).
2. Reproducibility: re-running a period with adjustments yields identical statements,
   adjustment records, and kickback reports.
3. Auditability drill: for one adjusted statement, walk `adjustment_records.relation_id` back
   to the relation and its metadata via the registry API ("why does this project get 15 % off?"
   answerable from data alone).
4. Finalization + correction flow exercised once with adjustments in play (credit note
   includes adjustment and kickback deltas).

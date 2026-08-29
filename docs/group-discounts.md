# Customer-group discounts

A customer that runs several projects is modelled as a meta-project: a registry
row of platform `meta` that owns no resources and that each of the customer's
projects points at with a `member_of` relation. That relation carries the
customer's discount, a `project_discount` in its `pricing_adjustments`, and the
rating engine applies it to every project the relation reaches. Attribution and
billing stay per project: each project keeps its own statement, and the group is
a view over those statements.

This document states the one pattern that expresses such a discount, how its
rate is changed for a later month, why the engine derives no volume tiers from
usage, and what the rollup export writes.

## Expressing a group discount

Register the meta-project once:

```bash
tally-reporting-admin create-meta-project \
  --external-id customer-alpha \
  --name "Customer Alpha"
```

The command prints the new project id on stdout, and that id is what the
relations name as their target.

Then relate every project of the customer to it. The call leaves the member
project, `POST /api/v1/projects/{id}/relations`, and carries the discount on the
relation:

```json
{
  "target_id": "<the meta-project id>",
  "relation_type": "member_of",
  "metadata": {
    "pricing_adjustments": [
      {"type": "project_discount", "rate": "0.05", "scope": "all", "description": "Customer Alpha group discount"}
    ]
  }
}
```

The rate is a string, never a number. The array is validated against
`internal/core/adjustment/adjustments_schema.json` when the relation is written,
and an array the schema refuses is answered 422 with one field error per
violation.

A run resolves the adjustments of a statement by walking the relation types
named in `TALLY_ENGINE_ADJUSTMENT_RELATION_TYPES` (default
`managed_by,member_of`) outward from the statement's project, up to
`TALLY_ENGINE_ADJUSTMENT_DEPTH` levels (default `3`). The discount sits on the
relation rather than on a project, so it reaches every member that holds one. A
`project_discount` applies after every `discount`, on the running net.

The member's statement then carries `base_cost`, one `adjustments` line whose
`relation_type` is `member_of` and whose `relation_target` is the meta-project's
external id (`customer-alpha`), and `net_cost`; `total` is the net cost. A
project rated at 3.84 under a rate of 0.05 gets one line of amount -0.19 and a
net cost of 3.65.

Relations the meta-project itself holds are part of the same walk. A
`managed_by` from the meta-project to a partner is at depth 2 from every member
and reaches all of them, which is how a partner's terms apply to a whole
customer group.

The one pattern is the `project_discount` on the `member_of` relation. Do not
put a `discount` on each member's own relations: the rate is then stored once
per project and drifts apart at the next change. Do not put the adjustment on
any other relation of the member, because the walk applies whatever it finds and
the group's terms then depend on relations that mean something else. Do not edit
statements: a statement is what a run produced from the registry, and a
finalized month stays reproducible from the relations alone.

## Changing the rate for a later month

The adjustments a relation carries are fixed for its lifetime. A `PATCH` whose
`pricing_adjustments` differ from the stored ones is answered 409 with the
detail `the pricing adjustments of a relation are fixed for its lifetime; close
this relation and create a successor that carries the new ones`.

A rate change effective 2026-04-01 is two calls. First close the current
relation at the boundary:

```http
PATCH /api/v1/projects/{id}/relations/{relation_id}
Content-Type: application/json

{"valid_to": "2026-04-01T00:00:00Z"}
```

Then create the successor over the same pair and type, valid from the same
instant and carrying the new rate:

```http
POST /api/v1/projects/{id}/relations
Content-Type: application/json

{
  "target_id": "<the meta-project id>",
  "relation_type": "member_of",
  "valid_from": "2026-04-01T00:00:00Z",
  "metadata": {
    "pricing_adjustments": [
      {"type": "project_discount", "rate": "0.08", "scope": "all", "description": "Customer Alpha group discount"}
    ]
  }
}
```

The successor is created as soon as `valid_to` is set on its predecessor, well
before the boundary passes. The registry's unique index over (source, target,
type), `uq_relations_active` in `migrations/reporting/0001_init.sql`, is partial
and covers open relations only, so a triple whose predecessor is closed is
created again even while that predecessor is still valid.

`DELETE /api/v1/projects/{id}/relations/{relation_id}` closes a relation at now.
It is not the call for a rate change, because now is somewhere inside the
running month.

Close at the month boundary, never mid-month. A relation applies to a whole
period as soon as its validity overlaps that period at any instant, that is
`valid_from < period_to AND (valid_to IS NULL OR valid_to > period_from)`
(decision D4 of phase 3), and so does its successor. A relation closed on
2026-04-15 and a successor valid from that instant therefore both apply to
April, and April's statements carry both discounts.

A month that is already finalized is not changed by the successor. A correction
run re-meters its own period and resolves the relations valid for that period,
so the relation that covered March keeps applying to March however many
successors follow it.

## Why volume tiers are not computed

The engine does not derive a rate from the usage of the period it rates. Such a
rate changes when a late event moves the period's usage across a tier boundary,
so the same month rates differently depending on when it is rated. A correction
would then have to re-derive the rate as well as the base it applies to, and
that is a correction semantics the engine does not have: a correction re-meters
and re-rates its period with the relations valid for it, and the rate those
relations carry is stored rather than computed.

Automatic tiering is recorded as a future extension in
`roadmap/05-phase-5-commercial-pricing.md` (WP 5.5). Until it exists, operations
sets a tier's rate per period explicitly, through the flow of the section above:
close the relation at the month boundary, create the successor with the rate the
next period is owed.

## The meta-project rollup export

An export sums a run's statements under the meta-projects the billed projects
are members of:

```bash
tally-engine export --run <id> --format json --out <dir> --rollup member_of
```

`--rollup managed_by` sums under partners instead. `--format csv` writes the
table rather than the documents. `TALLY_ENGINE_REPORTING_DB_URL` has to be set
whenever `--rollup` is passed: an export otherwise reads the engine database
alone, and the membership lives in the registry.

The json export writes one `rollup-<key>.json` per meta-project beside the
statements, where `<key>` is the target's cloud and external id escaped the way
a statement file name is. A virtual project carries its platform as its cloud,
so `customer-alpha` is written to `rollup-meta%2Fcustomer-alpha.json`. The
document holds `billing_period`, `project_id`, `platform`, `relation_type`,
`kind`, `corrects_run_id` (null on a regular run), `members` with `file`,
`cloud`, `project_id`, `total` and `currency` each, `total` and `currency`.
`run.json` carries a `rollup` member naming every rollup document with its
`members` count and its `total`. The csv export writes `rollup.csv` with the
columns `run_id`, `kind`, `corrects_run_id`, `period_from`, `period_to`,
`relation_type`, `target_cloud`, `target_project_id`, `cloud`, `project_id`,
`total` and `currency`, one row per member.

The sum follows the direct `member_of` relations of the period and nothing
further: a meta-project that is itself a member of a larger one is not followed,
and its members are summed under it alone. A member is counted once however many
relations reach the target from it in the period, so a membership closed and
opened again inside the month is one member rather than two. A project is listed
under every meta-project it belongs to, so two rollups are not disjoint and
their totals add up to more than the run billed. Nothing is summed for a project
the run billed under another project's statement: a project attributed to a root
through an `infrastructure_tenant` relation has its costs on the root's
statement, and it is the root's membership that decides where they land. A
rollup of a correction run sums that run's credit notes. A group's total equals
the sum of the member totals to the cent, and the statements themselves are
unchanged by it.

The membership is read from the registry at the moment the export runs rather
than stored with the run (author's decision of 2026-08-29). Two exports of one
finalized run therefore differ when a relation was created or closed
retroactively between them.

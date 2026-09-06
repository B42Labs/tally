---
title: Commercial pricing on relations
description: "Why discounts, surcharges and kickbacks are metadata on project relations rather than a pricing subsystem of their own."
quadrant: explanation
audience: all
---

# Commercial pricing on relations

A reseller partnership, a kickback agreement and a customer-specific discount
are commercial facts about a relationship between two parties. Tally stores them
where that relationship already lives: in the `metadata` column of a project
relation. The rating engine resolves them while it rates, and no table and no
column was added for them.

## Why adjustments live on relations

An adjustment defined on a relation is tied to a concrete, auditable
relationship between two entities: this project is managed by this reseller.
Four things follow from that.

- Auditability: every adjustment is traceable to one relation, so the answer to
  "why does this project get 15% off?" is the `managed_by` relation to Partner
  Corp.
- Lifecycle: when a reseller relationship ends, the relation is closed and the
  adjustment stops applying to later billing periods, while past periods stay
  reproducible because the relation history is kept.
- Transitivity: the depth of the relation walk carries an inherited adjustment,
  such as a meta-project discount that applies to every member project.
- No schema change: the existing `project_relations.metadata` column and
  additive `relation_type` values hold everything.

## Meta-projects and partners

A meta-project groups the projects of one customer. A partner is the entity a
reseller relation points at.

```text
Reseller "Partner Corp"       (entry in projects, platform="partner")
  "customer-proj-1"  ─managed_by→  "Partner Corp"
  "customer-proj-2"  ─managed_by→  "Partner Corp"

Meta-Project "Customer Alpha"  (entry in projects, platform="meta")
  "team-alpha-os"    ─member_of→   "Customer Alpha"
  "team-alpha"       ─member_of→   "Customer Alpha"
```

Both are ordinary `projects` rows: platform `meta` or `partner`, with `cloud`
set to the same literal, which keeps them inside `UNIQUE (cloud, external_id)`
without a table of their own (decision D1 of
[the commercial pricing roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/05-phase-5-commercial-pricing.md)).
No resource ever references them.

The rule is a rule of the schema rather than of the Go code alone
([migration 0010](https://github.com/B42Labs/tally/blob/main/migrations/reporting/0010_reserve_virtual_platforms.sql)),
and the two literals are named in one place
([`internal/core/project`](https://github.com/B42Labs/tally/blob/main/internal/core/project/project.go)).
A meta-project is created with `tally-reporting-admin create-meta-project` and a
partner with `tally-reporting-admin create-partner`.

## What an adjustment carries

The `metadata` column of a relation carries a `pricing_adjustments` array.

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

`scope` controls the granularity. `all` covers every resource type, a bare
platform such as `openstack` covers that platform, and `platform.resource_type`
such as `openstack.instance` covers one resource type of it (decision D5). An
adjustment's base is the sum of the rated amounts its scope matches.

The array is validated at write time against
[the adjustments schema](https://github.com/B42Labs/tally/blob/main/internal/core/adjustment/adjustments_schema.json),
and a document that breaks it is refused with 422. A malformed rate must not
surface for the first time during a billing run (decision D2).

## How adjustments apply

Rating computes the base cost first. The adjustments then apply in a fixed
order: surcharge, discount, project discount, kickback (decision D3).

- A surcharge is computed on the base cost, so two surcharges add rather than
  compound.
- A discount and a project discount are computed on the running net, so they
  stack multiplicatively on the surcharged amount.
- A kickback is computed on the resulting net and is emitted as a separate line
  item. It is what a partner is owed, and it leaves the customer's net alone.

Adjustments of the same type are ordered by relation id, so two runs over one
graph chain them the same way (decision D4). The adjustments themselves are
collected by a breadth-first walk from the statement's project over its outgoing
`managed_by` and `member_of` relations, bounded by
`TALLY_ENGINE_ADJUSTMENT_DEPTH` (default 3). Every relation is visited once, so
a relation two paths reach adjusts once and a cycle among the relations ends on
the visited set (decision D6). Each adjustment line is rounded once, and the
chain applies the rounded amounts (decision D7, see
[money and rounding](/explanation/money-and-rounding)). The implementation is
[`internal/engine/adjustments`](https://github.com/B42Labs/tally/blob/main/internal/engine/adjustments/adjustments.go).

## A reseller example

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

## Kickbacks and the rollup

A kickback is owed to the partner the relation points at, and a kickback line
lands in `adjustment_records`
([migration 0002](https://github.com/B42Labs/tally/blob/main/migrations/engine/0002_adjustment_records.sql))
beside the statement documents. What a partner is paid is a sum across every
project of a run, and a sum over JSONB documents is not a query the reporting
side can serve (decision D8). `tally-engine kickbacks` reports that settlement,
and an export writes it beside the statements.

The rollup goes the other way: one document per meta-project or partner, summing
the statements of its members, with the membership read from the registry at
export time
([`internal/engine/export`](https://github.com/B42Labs/tally/blob/main/internal/engine/export/export.go)).

## The golden cases

Five cases under
[internal/engine/testdata/golden/](https://github.com/B42Labs/tally/blob/main/internal/engine/testdata/golden/)
hold this arithmetic to exact numbers.

- `reseller` is the concept's own example: a 15% discount and a 10% kickback on
  one `managed_by` relation turn a base of 1200.00 EUR into a net of 1020.00 EUR
  and a commission of 102.00 EUR, and the partner gets no statement of its own.
- `scoped_discount` holds the scope grammar: a discount scoped
  `openstack.instance` takes 20.00 EUR off the project's instance and leaves its
  volume untouched, so the statement totals 130.00 EUR on a base of 150.00 EUR.
- `inherited_member_discount` holds the depth of the walk: a group discount on
  each member's `member_of` relation and a kickback on the meta-project's own
  `managed_by` relation both reach the member statements.
- `order_and_stacking` holds the fixed order: its relation lists the kickback
  first, and the run still chains the surcharge on 1000.00 EUR, the discount on
  1100.00 EUR and the kickback on 935.00 EUR.
- `virtual_relations` holds that `managed_by` and `member_of` never attribute
  cost: the partner and the meta-project get no statement, while the
  `infrastructure_tenant` relation still bills the OpenStack tenant as a related
  cost.

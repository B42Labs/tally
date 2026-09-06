---
title: Pricing adjustments
description: The pricing_adjustments array a relation carries, its schema, the order it is applied in and the lines it produces on a statement.
quadrant: reference
audience: integrator
---

# Pricing adjustments

A commercial term is an element of the `pricing_adjustments` array in the
`metadata` of a project relation. The array is written with
`POST /api/v1/projects/{id}/relations` and
`PATCH /api/v1/projects/{id}/relations/{relation_id}`, both on the
[Reporting API](/reference/api/reporting-api). A relation whose metadata does
not hold the member adjusts nothing.

An array the schema refuses is answered 422 with one field error per violation,
located at `body.metadata.pricing_adjustments.<index>.<member>`. The location is
empty below the member where the array as a whole is refused, so a refusal of
the array itself is reported at `body.metadata.pricing_adjustments`.

One relation carries at most 64 elements. A longer array is one violation naming
the length rather than one violation per element.

The adjustments of a relation are fixed for its lifetime. An update that sends
the member back unchanged is accepted, and one that changes it is answered 409.

## Schema

The array is held to
[`internal/core/adjustment/adjustments_schema.json`](https://github.com/B42Labs/tally/blob/main/internal/core/adjustment/adjustments_schema.json),
which is embedded in the binary and is the one place the API reads the member
from.

### The objects

<!-- refdoc:begin schema -->
`Tally pricing adjustments`, an array.

#### `root`

Each item is an object. The array holds at least 1 item.

| Property | Type | Required | Constraints |
| --- | --- | --- | --- |
| `type` | enum | yes | `discount`, `kickback`, `surcharge`, `project_discount` |
| `rate` | string | yes | `^0(\.\d{1,6})?$\|^1(\.0{1,6})?$` |
| `scope` | string | yes | `^all$\|^[a-z0-9_]+(\.[a-z0-9_]+)?$` |
| `description` | string | no | maxLength 500 |

No other property is allowed.
<!-- refdoc:end schema -->

## Application

The adjustments of one statement are collected by a breadth-first walk from the
statement's project over its outgoing relations. The relation types the walk
follows are named by `TALLY_ENGINE_ADJUSTMENT_RELATION_TYPES`, which is
`managed_by` and `member_of` by default, and the walk takes at most
`TALLY_ENGINE_ADJUSTMENT_DEPTH` levels. Every relation is visited once, so a
relation two paths reach adjusts once and a cycle ends on the visited set.

What the walk collected is applied in the order surcharge, discount, project
discount, kickback. Two adjustments of one type are ordered by relation id, and
two of one relation by their position in its array.

A surcharge is computed on the base, so two surcharges add rather than compound.
A discount and a project discount are computed on the running net, so they stack
multiplicatively. A kickback is computed on the running net and leaves it alone:
it is a line of its own, what a partner is owed rather than what the customer
pays.

`scope` is `all`, one platform, or a platform and a resource type separated by a
dot. `all` covers every rated amount, `<platform>` covers that whole
platform, and `<platform>.<resource_type>` covers one resource type of it.
The comparison is exact and case sensitive. A scope no rated amount of the
statement falls under touches nothing.

Each line is rounded once, to two places, and the rounded amount is then
apportioned back over the platform and resource type buckets the scope covers,
every bucket but the last taking its own rounded share and the last taking the
remainder. The shares therefore sum to the line exactly.

A kickback on a relation whose target is not a partner is dropped, and the run
records the warning `adjustment_kickback_target_not_partner`. The other
adjustments of that relation stay.

A relation the walk reaches whose stored array cannot be read fails the
statement. It is not billed as though the relation carried nothing.

The [commercial pricing on relations](/explanation/commercial-pricing-on-relations)
page states why the terms sit on relations and why the order is this one.

## Lines

### The line

<!-- refdoc:begin line -->
#### `Line`

Line is one applied adjustment as the document renders it and a record stores it. The field order is the order it is marshalled in.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `type` | string | always |  |
| `relation_type` | string | always |  |
| `relation_target` | string | always | RelationTarget is the external id of the relation's target, which is the beneficiary of a kickback. |
| `relation_id` | string | always |  |
| `scope` | string | always |  |
| `description` | string | omitted when empty |  |
| `rate` | decimal, 6 places | always |  |
| `base` | decimal, 2 places | always | Base is the amount the rate was applied to. |
| `amount` | decimal, 2 places | always | Amount is signed: a discount is negative. |
<!-- refdoc:end line -->

A statement carries the lines under `adjustments`, beside `base_cost`,
`net_cost` and `kickback_total`. `base_cost` is what the line items and the
related costs add up to before the adjustments, `net_cost` what they come to
after them, and `total` is the net cost. `kickback_total` stands beside the net
cost rather than inside it. All four members are absent from a statement no
adjustment reached.

A kickback also lands in the `adjustment_records` table and in the run's partner
settlement, which the [export formats](/reference/formats/exports) page states.

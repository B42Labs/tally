---
title: Model a customer group with a discount
description: Register a meta-project, relate the customer's projects to it with the group discount on the relation, and move the rate at a month boundary.
quadrant: how-to
audience: operator
---

# Model a customer group with a discount

This guide expresses one customer's group discount: a meta-project that owns no
resources, a `member_of` relation from every project of the customer to it, and
the `project_discount` that relation carries. Attribution and billing stay per
project, so each project keeps its own statement and the group is a view over
those statements. Why the adjustment sits on the relation is in
[commercial pricing on relations](/explanation/commercial-pricing-on-relations).

## Before you start

- The reporting database reachable from the machine you run the CLI on, and its
  connection string. `tally-reporting-admin` opens a pool on that database and
  talks to no API.
- The Reporting API reachable, and an API token for it. Which credential each
  route takes is on the
  [Reporting API endpoints](/reference/api/reporting-api) page.
- The registry ids of the customer's projects, which the relations leave.
- `tally-reporting-admin` at the version the API runs, with its subcommands on
  the [reporting admin CLI](/reference/command-line/tally-reporting-admin)
  page.
- The [Reporting API settings](/reference/configuration/tally-reporting) page,
  which lists the variables the CLI reads, `TALLY_REPORTING_DB_URL` among them.

## Register the meta-project

1. Point the admin CLI at the reporting database and register the customer
   once. The new project's registry id is printed alone on stdout, and that id
   is what the relations name as their target:

   ```sh
   export TALLY_REPORTING_DB_URL='postgres://tally:password@db.internal:5432/tally_reporting?sslmode=require'
   tally-reporting-admin create-meta-project \
     --external-id customer-alpha \
     --name "Customer Alpha"
   ```

   ```text
   7b8c9d0e-1f2a-4b3c-8d4e-5f6a7b8c9d0e
   ```

   An external id the registry already holds is refused, and the CLI exits 1
   with the reason on stderr.

## Relate the customer's projects

1. Create one relation per project of the customer. The call leaves the member
   project and carries the discount on the relation:

   ```http
   POST /api/v1/projects/{id}/relations
   Content-Type: application/json

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

   The rate is a string, never a number. The array is validated against the
   [pricing adjustments](/reference/formats/pricing-adjustments) schema when
   the relation is written, and an array the schema refuses is answered 422
   with one field error per violation.

2. The one pattern is the `project_discount` on the `member_of` relation. Put
   no `discount` on the member's other relations, where the rate is then stored
   once per project and drifts apart at the next change. Put the adjustment on
   no other relation of the member either: the walk applies whatever it finds,
   and the group's terms then hang off relations that mean something else. Edit
   no statements: a statement is what a run produced from the registry, and a
   finalized month stays reproducible from the relations alone.

3. A run collects the adjustments of a statement by walking the relation types
   `TALLY_ENGINE_ADJUSTMENT_RELATION_TYPES` names, `managed_by,member_of` by
   default, outward from the statement's project and up to
   `TALLY_ENGINE_ADJUSTMENT_DEPTH` levels, 3 by default. The relation the
   discount sits on therefore reaches every member that holds one, and
   [how adjustments apply](/explanation/commercial-pricing-on-relations#how-adjustments-apply)
   states the order the lines are applied in.

## Change the rate for a later month

1. The adjustments a relation carries are fixed for its lifetime. A `PATCH`
   whose `pricing_adjustments` differ from the stored ones is answered 409 with
   the detail:

   ```text
   the pricing adjustments of a relation are fixed for its lifetime; close this relation and create a successor that carries the new ones
   ```

2. Close the current relation at the month boundary the new rate takes effect
   on:

   ```http
   PATCH /api/v1/projects/{id}/relations/{relation_id}
   Content-Type: application/json

   {"valid_to": "2026-04-01T00:00:00Z"}
   ```

3. Create the successor over the same pair and type, valid from the same
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

   The successor is created as soon as `valid_to` is set on its predecessor,
   well before the boundary passes. The registry's unique index over source,
   target and type covers open relations alone, so a triple whose predecessor
   is closed is created again while that predecessor is still valid.

4. Close at the month boundary, never mid-month. A relation applies to a whole
   period as soon as its validity overlaps that period at any instant, and so
   does its successor: a relation closed on 2026-04-15 and a successor valid
   from that instant both apply to April, and April's statements carry both
   discounts. `DELETE /api/v1/projects/{id}/relations/{relation_id}` closes a
   relation at now, which is somewhere inside the running month, so it is not
   the call for a rate change.

## Check the result

1. Export a run of a month the relation covers
   ([export a run](/how-to/engine/export-a-run)) and read the member's
   statement. It carries one `adjustments` line whose `relation_type` is
   `member_of` and whose `relation_target` is the meta-project's external id:

   ```sh
   jq '{base_cost, adjustments, net_cost, total}' \
     ./2026-07/json/statement-os-prod-eu1%2F9c4a1b2d3e4f5061728394a5b6c7d8e9.json
   ```

   ```json
   {
     "base_cost": 3.84,
     "adjustments": [
       {
         "type": "project_discount",
         "relation_type": "member_of",
         "relation_target": "customer-alpha",
         "relation_id": "5f6a7b8c-9d0e-4f1a-8b2c-3d4e5f6a7b8c",
         "scope": "all",
         "description": "Customer Alpha group discount",
         "rate": 0.050000,
         "base": 3.84,
         "amount": -0.19
       }
     ],
     "net_cost": 3.65,
     "total": 3.65
   }
   ```

   A project rated at 3.84 under a rate of 0.05 gets one line of amount -0.19
   and a net cost of 3.65, and `total` is the net cost.

2. A statement no adjustment reached carries none of `base_cost`,
   `adjustments`, `net_cost` and `kickback_total`, and its `total` is what the
   line items and the related costs add up to.

3. A month that is already finalized is not changed by a successor relation. A
   correction run re-meters its own period and resolves the relations valid for
   that period, so the relation that covered March keeps applying to March
   however many successors follow it.

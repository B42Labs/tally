---
title: Zero-rate a service project
description: Register the project an OpenStack service keeps its own resources in, and bill it at zero or under a project of the operator.
quadrant: how-to
audience: operator
---

# Zero-rate a service project

This guide takes the project an OpenStack service keeps its own resources in,
octavia's amphorae for instance, off what a billing run invoices. It registers
the project, and then either bills it at zero through a membership that carries
a full discount or bills it under a project of the operator. Why those
resources are metered at all is in
[resources a service creates for itself](/explanation/project-registry-relations-and-attribution#resources-a-service-creates-for-itself).

## Before you start

- A cloud whose events already reach the Reporting API, through the collector
  or a sync.
- The id keystone gives the service project. Kolla calls the project `service`:
  `openstack project show service -f value -c id`.
- The Reporting API reachable, and an API token for it. Which credential each
  route takes is on the
  [Reporting API endpoints](/reference/api/reporting-api) page.
- The reporting database reachable from the machine you run the CLI on, and
  `tally-reporting-admin` at the version the API runs. Its subcommands are on
  the [reporting admin CLI](/reference/command-line/tally-reporting-admin)
  page, and the
  [Reporting API settings](/reference/configuration/tally-reporting) page names
  `TALLY_REPORTING_DB_URL`.
- `tally-engine` and `jq` on the machine, for the check.

## Find what the service project holds

1. List the resources the cloud's events booked to the service project:

   ```http
   GET /api/v1/resources?cloud=os-prod-eu1&project_id=4f6a8c0e2b4d4f6a8c0e2b4d4f6a8c0e&status=all
   ```

   `items` holds one row per resource, deleted ones included, and a
   `next_cursor` that is not null means there are more pages. An amphora is an
   `instance` whose `size.flavor` is the flavor octavia boots amphorae from,
   `amphora` on a Kolla deployment, and its boot volume is a `volume`. An empty
   `items` means nothing was ever booked to that project, and this guide has
   nothing to change. Without `status=all` the answer leaves out the deleted
   resources, and a run still bills the hours they ran in its month.

## Register the service project

1. Register the project under the id keystone gives it:

   ```http
   POST /api/v1/projects
   Content-Type: application/json

   {"platform": "openstack", "cloud": "os-prod-eu1", "external_id": "4f6a8c0e2b4d4f6a8c0e2b4d4f6a8c0e", "name": "service"}
   ```

   The answer is 201, and its `id` is the registry id the relations below name,
   `2a3b4c5d-6e7f-4a8b-9c0d-1e2f3a4b5c6d` in this guide. A 409 means the
   registry already holds the pair, and this call reads the id back:

   ```http
   GET /api/v1/projects?cloud=os-prod-eu1&external_id=4f6a8c0e2b4d4f6a8c0e2b4d4f6a8c0e
   ```

## Bill the service project at zero

1. Point the admin CLI at the reporting database and register the meta-project
   the service project becomes a member of. The id printed alone on stdout is
   the relation's target:

   ```sh
   export TALLY_REPORTING_DB_URL='postgres://tally:password@db.internal:5432/tally_reporting?sslmode=require'
   tally-reporting-admin create-meta-project \
     --external-id internal-usage \
     --name "Internal usage"
   ```

   ```text
   8c9d0e1f-2a3b-4c4d-8e5f-6a7b8c9d0e1f
   ```

   An external id the registry already holds is refused, and the CLI exits 1.

2. Relate the service project to the meta-project. The path's `{id}` is the
   service project's registry id:

   ```http
   POST /api/v1/projects/{id}/relations
   Content-Type: application/json

   {
     "target_id": "8c9d0e1f-2a3b-4c4d-8e5f-6a7b8c9d0e1f",
     "relation_type": "member_of",
     "valid_from": "2026-10-01T00:00:00Z",
     "metadata": {
       "pricing_adjustments": [
         {"type": "project_discount", "rate": "1", "scope": "all", "description": "Service project, not invoiced"}
       ]
     }
   }
   ```

   The rate is a string, never a number. `valid_from` is the first instant of
   the earliest month that is not finalized yet. Left out, it is the instant of
   the call, which covers the running month and none before it. A finalized
   month stays as it was billed. Changing the rate later means closing the
   relation and creating a successor, as
   [change the rate for a later month](/how-to/engine/model-a-customer-group#change-the-rate-for-a-later-month)
   shows.

## Bill the service project under an operator project instead

This section replaces the one before it: an attributed project has no statement
of its own, so a discount on its membership reaches nothing.

1. Create the relation from the project that pays. The path's `{id}` is the
   registry id of the registered project that pays, the operator's own:

   ```http
   POST /api/v1/projects/{id}/relations
   Content-Type: application/json

   {
     "target_id": "2a3b4c5d-6e7f-4a8b-9c0d-1e2f3a4b5c6d",
     "relation_type": "infrastructure_tenant",
     "valid_from": "2026-10-01T00:00:00Z"
   }
   ```

   The target is the service project's registry id. The service project's line
   items then appear as `related_costs` on the statement of the project that
   pays.

## Check the result

1. Run the month ([run a period](/how-to/engine/run-a-period)) and export the
   run ([export a run](/how-to/engine/export-a-run)). A service project billed
   at zero has this statement:

   ```sh
   jq '{base_cost, adjustments, net_cost, total}' \
     ./2026-10/json/statement-os-prod-eu1%2F4f6a8c0e2b4d4f6a8c0e2b4d4f6a8c0e.json
   ```

   ```json
   {
     "base_cost": 38.31,
     "adjustments": [
       {
         "type": "project_discount",
         "relation_type": "member_of",
         "relation_target": "internal-usage",
         "relation_id": "6a7b8c9d-0e1f-4a2b-8c3d-4e5f6a7b8c9d",
         "scope": "all",
         "description": "Service project, not invoiced",
         "rate": 1.000000,
         "base": 38.31,
         "amount": -38.31
       }
     ],
     "net_cost": 0.00,
     "total": 0.00
   }
   ```

   The line items stay on the statement, the one adjustment takes the whole
   base cost off, and `net_cost` and `total` are 0.00.

2. Check that the run no longer reports the project as unregistered:

   ```sh
   jq '[.stats.unregistered_projects // [] | .[] | select(.project_id == "4f6a8c0e2b4d4f6a8c0e2b4d4f6a8c0e")] | length' ./2026-10/json/run.json
   ```

   ```text
   0
   ```

3. Where the service project is billed under an operator project, the run's
   index lists no statement for it:

   ```sh
   jq '[.statements[] | select(.project_id == "4f6a8c0e2b4d4f6a8c0e2b4d4f6a8c0e")] | length' ./2026-10/json/run.json
   ```

   ```text
   0
   ```

   The statement of the project that pays carries one entry under
   `related_costs` whose `relation_type` is `infrastructure_tenant` and whose
   `project_id` is the service project's id.

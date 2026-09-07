---
title: Register simulated projects
description: Register the tenants and the Gardener projects of a simulated month with the dev registry, read the rows back, and bill the month through the relations.
quadrant: how-to
audience: contributor
---

# Register simulated projects

This guide registers a simulated month with the project registry of the
Reporting API, walks the relations it wrote, and bills the month so the
attribution shows up in the statements. What the tenants and the Gardener
projects of such a month are is in
[the simulated OpenStack world](/explanation/the-simulated-openstack-world),
and what a relation does to a bill is in
[project registry, relations and attribution](/explanation/project-registry-relations-and-attribution).

## Before you start

- A dev cluster from `make up` and the compose stack of
  [run a simulated month](/how-to/simulator/run-a-month).
- An api token of role `admin`, which the two registry routes demand.
  `make simulator-up SIM_REGISTER_PROJECTS=true` issues one and writes it into
  `deploy/compose/.env` as `TALLY_SIM_API_TOKEN`.
- `tally-ca.crt` at the repository root, which `make simulator-up` writes.
- `tally-engine` for the metering and the export at the end.
- The [simulator command line](/reference/command-line/tally-openstack-simulator)
  page, which states what
  [project registration](/reference/command-line/tally-openstack-simulator#project-registration)
  posts and what it refuses.
- The [simulator settings](/reference/configuration/tally-openstack-simulator)
  page, which lists the four variables a registration reads:
  `TALLY_SIM_REPORTING_URL`, `TALLY_SIM_REPORTING_INSECURE`,
  `TALLY_SIM_API_TOKEN` and `TALLY_SIM_GARDEN_CLOUD`.

## Register the month

1. Start the stack with the switch on. `run --register-projects` registers the
   month with the project registry before the first file is written and before
   the first notification goes out, and `SIM_REGISTER_PROJECTS=true` is what
   turns it on for the stack:

   ```sh
   make simulator-up SIM_PERIOD=2026-07 SIM_REGISTER_PROJECTS=true
   ```

   The switch is off by default: a run without it puts a month on a bus or into
   files and reads no registry at all.

## Read the rows back

1. List what the month registered under the cloud:

   ```sh
   curl --cacert tally-ca.crt \
     -H "Authorization: Bearer $(grep TALLY_SIM_API_TOKEN deploy/compose/.env | cut -d= -f2)" \
     'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=os-sim'
   ```

2. Walk a Gardener project's relations. They hang below the row they start at,
   so take the `id` of the `garden-sim`/`alpha` row that
   `GET /api/v1/projects?cloud=garden-sim` lists first:

   ```sh
   curl --cacert tally-ca.crt \
     -H "Authorization: Bearer $(grep TALLY_SIM_API_TOKEN deploy/compose/.env | cut -d= -f2)" \
     'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/<alpha id>/related?relation_type=infrastructure_tenant'
   ```

## Register a second month

1. End the relations of the first month before you register a later period. The
   rows outlive `make simulator-down`, which drops the containers and the outbox
   volume and touches the dev registry not at all, so a second `simulator-up` for
   a later period is the rerun the registration refuses until the relations of
   the first one end no later than the first instant of the second period. No
   subcommand of `tally-reporting-admin` deletes a project, and a relation is
   never removed:
   `PATCH /api/v1/projects/{id}/relations/{relation_id}` with a `valid_to` ends
   one at a chosen instant, and
   `DELETE /api/v1/projects/{id}/relations/{relation_id}` ends one at now, which
   is after the period a simulated month covers and therefore no way past the
   check.

2. Give a rerun of the same period or an earlier one a garden cloud of its own,
   `TALLY_SIM_GARDEN_CLOUD`. Such a run has no instant to end the relations at,
   because they start at or after that period's first instant.

## Bill the month

1. Meter the month and export it as JSON. The engine's default attributing
   relation type is `infrastructure_tenant`:

   ```sh
   tally-engine run --period 2026-07
   tally-engine export --run <id> --format json --out /tmp/statements
   ```

   That writes `statement-garden-sim%2Falpha.json` and
   `statement-garden-sim%2Fbeta.json`. The two Gardener projects have no usage of
   their own, and a statement is opened for each of them all the same: their
   `related_costs` carry the line items of the tenant the shoots run on, under
   the relation type `infrastructure_tenant`. The two Gardener tenants get no
   statement of their own. `rated.csv` does not change and neither does
   `compare`: a rated record carries the tenant that owned the resource as
   `project_id`, and the attribution stands in the statements alone.

## Check the result

1. List the registered tenants of the cloud. `GET /api/v1/projects?cloud=os-sim`
   lists six rows, one `openstack` row per tenant of the month, keyed by the
   keystone project id and carrying the tenant's name:

   ```sh
   curl --cacert tally-ca.crt \
     -H "Authorization: Bearer $(grep TALLY_SIM_API_TOKEN deploy/compose/.env | cut -d= -f2)" \
     'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=os-sim'
   ```

2. Read `runs.stats.unregistered_projects` off the run that billed the month. It
   names nothing, because every tenant the month books usage for is registered.

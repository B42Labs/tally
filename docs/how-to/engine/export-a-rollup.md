---
title: Export a rollup per customer group
description: Sum a run's statements under the meta-projects or the partners its projects relate to, as documents or as one table.
quadrant: how-to
audience: operator
---

# Export a rollup per customer group

This guide sums one run's statements under the groups its projects belong to
and writes one document per group beside the statements. The statements
themselves are unchanged by it. What each document holds is in
[rollup](/reference/formats/exports#rollup).

## Before you start

- Both databases reachable from the machine you run the CLI on: the engine's
  own and the Reporting API's. A rollup reads the membership from the registry
  when the export runs, so an export with `--rollup` needs the second one.
- The id of the run to export, and the relation type to sum under,
  `member_of` or `managed_by`. The groups themselves are the meta-projects and
  partners the projects relate to
  ([model a customer group](/how-to/engine/model-a-customer-group)).
- One empty directory per export, or a path that does not exist yet.
- `tally-engine` at the version the deployment runs, with its subcommands on
  the [engine CLI](/reference/command-line/tally-engine) page.
- The [engine settings](/reference/configuration/tally-engine) page, which
  names the two variables this guide sets.

## Write the group documents

1. Export both databases. An export without `--rollup` reads the engine one
   alone, and `TALLY_ENGINE_REPORTING_DB_URL` is what makes the membership
   readable:

   ```sh
   export TALLY_ENGINE_DB_URL='postgres://tally:password@db.internal:5432/tally_engine?sslmode=require'
   export TALLY_ENGINE_REPORTING_DB_URL='postgres://tally_engine:password@db.internal:5432/tally_reporting?sslmode=require'
   ```

2. Sum the run under the meta-projects its projects are members of. The rollup
   line stands beside the lines a plain export writes:

   ```sh
   tally-engine export --run <run id> --format json --out ./2026-07/groups --rollup member_of
   ```

   ```text
   run <run id> exported for 2026-07 as json into ./2026-07/groups
   wrote run.json and <n> statements
   wrote kickbacks.json with 0 kickbacks
   wrote <k> rollup documents over member_of
   ```

3. `--rollup managed_by` sums under the partners that manage the projects
   instead, and `--format csv` writes the table rather than the documents:

   ```sh
   tally-engine export --run <run id> --format csv --out ./2026-07/partners --rollup managed_by
   ```

   ```text
   run <run id> exported for 2026-07 as csv into ./2026-07/partners
   wrote rated.csv with <n> rated records
   wrote kickbacks.csv with 0 kickbacks
   wrote rollup.csv with <m> members over managed_by
   ```

   `rollup.csv` carries one row per member of every group, with the group on
   the row.

## Check the result

1. One document per group stands beside the statements. A virtual project
   carries its platform as its cloud, so the meta-project `customer-alpha` is
   written to `rollup-meta%2Fcustomer-alpha.json`:

   ```sh
   ls ./2026-07/groups
   ```

   ```text
   kickbacks.json
   rollup-meta%2Fcustomer-alpha.json
   run.json
   statement-os-prod-eu1%2F9c4a1b2d3e4f5061728394a5b6c7d8e9.json
   ```

2. `run.json` carries a `rollup` member naming every group document with its
   member count and its total:

   ```sh
   jq .rollup ./2026-07/groups/run.json
   ```

   ```json
   {
     "relation_type": "member_of",
     "documents": [
       {
         "file": "rollup-meta%2Fcustomer-alpha.json",
         "cloud": "meta",
         "project_id": "customer-alpha",
         "members": 3,
         "total": 412.77,
         "currency": "EUR"
       }
     ]
   }
   ```

   `documents` is empty rather than absent where the rollup reached no group at
   all.

3. A group's total equals the sum of the member totals it lists, to the cent:

   ```sh
   jq '{total, members: [.members[].total]}' \
     ./2026-07/groups/rollup-meta%2Fcustomer-alpha.json
   ```

   ```json
   {
     "total": 412.77,
     "members": [128.45, 199.98, 84.34]
   }
   ```

4. Two groups are not disjoint: a project is listed under every group it
   belongs to, and the totals of two groups add up to more than the run billed.
   Nothing is summed for a project the run billed under another project's
   statement, and the root's membership is what decides where those costs land.

5. Two exports of one finalized run differ where a relation was created or
   closed between them. The membership is read from the registry when the
   export runs rather than stored with the run.

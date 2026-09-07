---
title: Export a run
description: Write one run out as documents and as a table, and read the index, the statements and the rated records it leaves.
quadrant: how-to
audience: operator
---

# Export a run

This guide writes one run's result into a directory, once as JSON documents and
once as a CSV table. What every file and every member holds is in the
[export formats](/reference/formats/exports) reference.

## Before you start

- The engine database reachable from the machine you run the CLI on, and its
  connection string.
- The id of the run to export, completed or finalized.
- One empty directory per export, or a path that does not exist yet.
- `jq` on the machine, for the two reads below.
- `tally-engine` at the version the deployment runs, with its subcommands on
  the [engine CLI](/reference/command-line/tally-engine) page.
- The [engine settings](/reference/configuration/tally-engine) page, which
  names `TALLY_ENGINE_DB_URL`, the one variable this guide sets.

## Write the documents

1. Point the engine at its own database. An export without `--rollup` reads
   that database alone:

   ```sh
   export TALLY_ENGINE_DB_URL='postgres://tally:password@db.internal:5432/tally_engine?sslmode=require'
   ```

2. Export the run as JSON. The first line names the run, the period and the
   directory, and one line per kind of file follows it:

   ```sh
   tally-engine export --run <run id> --format json --out ./2026-07/json
   ```

   ```text
   run <run id> exported for 2026-07 as json into ./2026-07/json
   wrote run.json and <n> statements
   wrote kickbacks.json with 0 kickbacks
   ```

3. Read the run's stats off the index. `run.json` is written last, after every
   file it names is on stable storage:

   ```sh
   jq .stats ./2026-07/json/run.json
   ```

   ```json
   {
     "snapshot_at": "2026-08-03T00:00:00Z",
     "candidates": 812,
     "usage_records": 1204,
     "rated_records": 3612,
     "statements": 6,
     "unpriced": [
       {"platform": "openstack", "resource_type": "image", "count": 4},
       {"platform": "openstack", "resource_type": "loadbalancer", "count": 2}
     ]
   }
   ```

   The four counts are always there, and a list stands beside them for each
   class of finding the run recorded. A clean run reads as the counts alone.

4. Read the costs one project carries for another. A project that a relation
   attributes another project's resources to carries them under
   `related_costs` rather than in its own line items, with the type of the
   relation that claimed them. A Gardener project whose shoots run on an
   infrastructure tenant has no usage of its own and gets a statement of just
   that shape:

   ```sh
   jq '.related_costs[] | {relation_type, project_id, total}' \
     ./2026-07/json/statement-garden-prod-eu1%2Falpha.json
   ```

   ```json
   {
     "relation_type": "infrastructure_tenant",
     "project_id": "9c4a1b2d3e4f5061728394a5b6c7d8e9",
     "total": 184.20
   }
   ```

## Write the table

1. Export the same run as CSV, into a directory of its own:

   ```sh
   tally-engine export --run <run id> --format csv --out ./2026-07/csv
   ```

   ```text
   run <run id> exported for 2026-07 as csv into ./2026-07/csv
   wrote rated.csv with <n> rated records
   wrote kickbacks.csv with 0 kickbacks
   ```

2. An `--out` that already holds files is refused, and nothing is written:

   ```sh
   tally-engine export --run <run id> --format csv --out ./2026-07/csv
   ```

   ```text
   --out: ./2026-07/csv is not empty, and an export does not remove what an earlier one left there
   ```

## Check the result

1. The JSON directory holds the index, one statement per project the run
   billed, and the settlement:

   ```sh
   ls ./2026-07/json
   ```

   ```text
   kickbacks.json
   run.json
   statement-garden-prod-eu1%2Falpha.json
   statement-os-prod-eu1%2F9c4a1b2d3e4f5061728394a5b6c7d8e9.json
   ```

   A file is named after the statement key, the cloud and the project id joined
   by a slash, escaped twice, which is where the `%2F` comes from.

2. The CSV directory holds the rated records and the settlement:

   ```sh
   ls ./2026-07/csv
   ```

   ```text
   kickbacks.csv
   rated.csv
   ```

3. Exporting one finalized run twice into two clean directories yields the same
   files: a finalized run's records no longer change. An export with
   [`--rollup`](/how-to/engine/export-a-rollup) is the exception: it reads the
   membership from the registry when the export runs.

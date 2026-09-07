---
title: Correct a finalized period
description: Meter a finalized month again, book the differences as credit notes, and export them.
quadrant: how-to
audience: operator
---

# Correct a finalized period

This guide books what reached a finalized month after it closed. The correction
meters the month again from the full event history with the pricing version the
finalized run used, stores every non-zero difference as a delta, and renders one
credit note per affected project. The finalized run stays as it is. What a
correction is held against, and what it leaves the month with, is in
[billing period lifecycle and corrections](/explanation/billing-period-lifecycle-and-corrections).

## Before you start

- A finalized month and the late events to book. Which resources took them is
  what [detect late events](/how-to/engine/detect-late-events) reports.
- Both databases reachable from the machine you run the CLI on, and the counter
  sources file: a correction meters the month again, so it reads what
  [run a period](/how-to/engine/run-a-period) reads.
- Two empty directories for the exports, or two paths that do not exist yet.
- `tally-engine` at the version the deployment runs, with its subcommands on
  the [engine CLI](/reference/command-line/tally-engine) page.
- The [engine settings](/reference/configuration/tally-engine) page, which
  lists the variables this guide sets with their defaults.

## Book the late events

1. Export the inputs of a metering pass. `TALLY_ENGINE_VM_URL` is needed where
   the counter sources file declares a metricsql source:

   ```sh
   export TALLY_ENGINE_DB_URL='postgres://tally:password@db.internal:5432/tally_engine?sslmode=require'
   export TALLY_ENGINE_REPORTING_DB_URL='postgres://tally_engine:password@db.internal:5432/tally_reporting?sslmode=require'
   export TALLY_ENGINE_COUNTER_SOURCES=./counter-sources.yaml
   export TALLY_ENGINE_VM_URL=http://127.0.0.1:8428
   ```

2. Meter the month again. The correction gets a run of its own and names the
   run it diffs against:

   ```sh
   tally-engine correct --period 2026-07
   ```

   ```text
   run <correction id> completed as a correction of run <run id> for 2026-07 with pricing model 2026-03
   metered <n> candidates into <n> usage records and <n> rated records
   <n> deltas in <k> credit notes
   warnings recorded in runs.stats: 0 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 0 unregistered projects
   ```

   A correction that moved an adjustment as well prints
   `<n> deltas and <n> adjustment deltas in <k> credit notes` on the third
   line.

3. Close the correction. The period keeps naming the regular run that closed
   it, and the next correction diffs against this one:

   ```sh
   tally-engine finalize --period 2026-07 --run <correction id>
   ```

   ```text
   correction run <correction id> finalized for 2026-07
   ```

4. Export the correction as a table and as documents, into a directory each:

   ```sh
   tally-engine export --run <correction id> --format csv --out ./2026-07/corrected
   tally-engine export --run <correction id> --format json --out ./2026-07/notes
   ```

   ```text
   run <correction id> exported for 2026-07 as csv into ./2026-07/corrected
   wrote rated.csv with <n> rated records
   wrote deltas.csv with <m> deltas
   wrote kickbacks.csv with 0 kickback deltas
   run <correction id> exported for 2026-07 as json into ./2026-07/notes
   wrote run.json and <k> credit notes
   wrote kickbacks.json with 0 kickback deltas
   ```

   The json export writes one `credit-note-*.json` per affected project beside
   `run.json`. What such a document holds is in
   [credit notes](/reference/formats/exports#credit-notes).

## Check the result

1. Read the third line of `correct`. `<n> deltas in <k> credit notes` is the
   month moving, and `no deltas: the finalized numbers of 2026-07 stand` is the
   re-metering arriving at what the finalized run already billed. The second
   answer still leaves a correction run behind, and finalizing it is what makes
   the next correction diff against it.

2. Count the documents the json export wrote. There is one credit note per
   project a delta reached, and none for a project the correction did not move:

   ```sh
   ls ./2026-07/notes
   ```

   ```text
   credit-note-os-prod-eu1%2F9c4a1b2d3e4f5061728394a5b6c7d8e9.json
   kickbacks.json
   run.json
   ```

3. An `--out` that already holds files is refused, and nothing is written:

   ```text
   --out: ./2026-07/notes is not empty, and an export does not remove what an earlier one left there
   ```

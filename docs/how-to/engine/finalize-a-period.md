---
title: Finalize a period
description: Close one billing month on a completed run and read the period back with its finalized run and instant.
quadrant: how-to
audience: operator
---

# Finalize a period

This guide closes one billing month. The run's records become immutable, the
period stops taking new ones, and what arrives afterwards is booked by a
correction. Where the gate sits in the month's life is in
[billing period lifecycle and corrections](/explanation/billing-period-lifecycle-and-corrections).

## Before you start

- The engine database reachable from the machine you run the CLI on, and its
  connection string.
- A completed [run](/how-to/engine/run-a-period) of the month, and its id.
- `tally-engine` at the version the deployment runs, with its subcommands on
  the [engine CLI](/reference/command-line/tally-engine) page.
- The [engine settings](/reference/configuration/tally-engine) page, which
  names `TALLY_ENGINE_DB_URL`, the one variable this guide sets.

## Close the month

1. Point the engine at its own database. `finalize` reads and writes that
   database alone, because the records it turns immutable are already stored:

   ```sh
   export TALLY_ENGINE_DB_URL='postgres://tally:password@db.internal:5432/tally_engine?sslmode=require'
   ```

2. Name the month and the run that bills it:

   ```sh
   tally-engine finalize --period 2026-07 --run <run id>
   ```

   ```text
   run <run id> finalized, period 2026-07 closed
   ```

3. Metering the month again is refused, and the error opens on `the billing
   period is finalized`. It comes before a run row exists. The command below
   runs under the environment
   [run a period](/how-to/engine/run-a-period) sets:

   ```sh
   tally-engine run --period 2026-07
   ```

   ```text
   the billing period is finalized
   ```

   `TALLY_ENGINE_AUTO_FINALIZE` lets the hourly tick take this step for a
   completed run instead. It is `false` by default, so the close stays a
   command somebody runs.

## Check the result

1. List the periods. The month reads `finalized` and names the run that closed
   it and when:

   ```sh
   tally-engine periods list
   ```

   ```text
   2026-07 finalized finalized_run=<run id> finalized_at=<instant>
   2026-08 open
   ```

   A period is `open`, `grace` or `finalized`, and only a finalized one carries
   the two `finalized_` values. The `2026-08` line beside it is the month the
   hourly tick opened; a database with no period at all answers `no billing
   periods`.

2. Events that reached the reporting database after the finalized run read the
   month are found by
   [detect-late](/how-to/engine/detect-late-events) and booked by
   [correct](/how-to/engine/correct-a-finalized-period). The finalized run
   itself stays as it is.

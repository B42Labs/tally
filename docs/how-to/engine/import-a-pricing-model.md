---
title: Import a pricing model
description: Load a pricing model file into the engine database and read back the version a run rates against.
quadrant: how-to
audience: operator
---

# Import a pricing model

This guide imports one pricing model file into the engine database, where a run
resolves the prices of the period it rates. What such a file may declare is in
the [pricing model file](/reference/formats/pricing-model) reference.

## Before you start

- The engine database reachable from the machine you run the CLI on, and its
  connection string.
- The pricing model file, with a `valid_from` at or before the first instant of
  the month it is to price.
  [`pricing/2026-03.yaml`](https://github.com/B42Labs/tally/blob/main/pricing/2026-03.yaml)
  is the model the worked examples are priced with.
- `tally-engine` at the version the deployment runs, with its subcommands on
  the [engine CLI](/reference/command-line/tally-engine) page.
- The [engine settings](/reference/configuration/tally-engine) page, which
  names `TALLY_ENGINE_DB_URL`, the one variable this guide sets.

## Import the file

1. Point the engine at its own database. `pricing import` reads that database
   and nothing else:

   ```sh
   export TALLY_ENGINE_DB_URL='postgres://tally:password@db.internal:5432/tally_engine?sslmode=require'
   ```

2. Import the file. The line names the version it stored and the instant that
   version is valid from:

   ```sh
   tally-engine pricing import pricing/2026-03.yaml
   ```

   ```text
   imported pricing model 2026-03 valid from 2026-03-01T00:00:00Z
   ```

3. A second import of the same file stores nothing and says so:

   ```sh
   tally-engine pricing import pricing/2026-03.yaml
   ```

   ```text
   pricing model 2026-03 already imported
   ```

   A file that carries a version already imported and prices something else is
   refused with `this version is already imported and prices something else,
   and a corrected price belongs in a new version`. A corrected price goes in
   under a version of its own.

## Check the result

1. List the catalogs the database holds. The version the file declares stands
   on a line with its validity, its currency and when it was imported:

   ```sh
   tally-engine pricing list
   ```

   ```text
   2026-03 valid_from=2026-03-01T00:00:00Z currency=EUR imported_at=2026-07-09T14:22:00Z
   ```

2. A database that holds no catalog at all answers `no pricing models`, and a
   [run](/how-to/engine/run-a-period) of a period no imported model is valid
   for fails with `no pricing model is valid for this period`.

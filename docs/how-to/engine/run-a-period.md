---
title: Run a period
description: Set the engine's four inputs and meter and rate one billing month into a run the other subcommands work on.
quadrant: how-to
audience: operator
---

# Run a period

This guide meters and rates one billing month. The run reads the period's
resources and events from the reporting database, derives the usage records of
every project and rates them against the imported pricing model, and it leaves
a run row behind that finalization, correction and export work on. What the two
stages do is in
[metering separated from rating](/explanation/metering-separated-from-rating).

## Before you start

- Both databases reachable from the machine you run the CLI on: the engine's
  own and the Reporting API's, the second one through a login role that is a
  member of `tally_engine_reader`.
- A [pricing model](/how-to/engine/import-a-pricing-model) imported and valid
  for the month you are about to meter.
- The counter sources file, and a VictoriaMetrics endpoint the shell reaches
  where that file declares a metricsql source. The
  [counter sources file](/reference/configuration/counter-sources-file)
  reference states what an entry holds.
- `tally-engine` at the version the deployment runs, with its subcommands on
  the [engine CLI](/reference/command-line/tally-engine) page.
- The [engine settings](/reference/configuration/tally-engine) page, which
  lists the four variables this guide sets with their defaults.

## Set the engine's environment

1. Export the four inputs of a run and open the path to the metrics store:

   ```sh
   export TALLY_ENGINE_DB_URL='postgres://tally:password@db.internal:5432/tally_engine?sslmode=require'
   export TALLY_ENGINE_REPORTING_DB_URL='postgres://tally_engine:password@db.internal:5432/tally_reporting?sslmode=require'
   export TALLY_ENGINE_COUNTER_SOURCES=./counter-sources.yaml
   export TALLY_ENGINE_VM_URL=http://127.0.0.1:8428
   kubectl -n tally port-forward svc/victoriametrics 8428:8428 &
   ```

   - `TALLY_ENGINE_DB_URL` is the engine's own database, the one the run row
     and its records are written to.
   - `TALLY_ENGINE_REPORTING_DB_URL` reads the reporting database, where the
     resources, the events and the project relations are. The login role it
     names is a member of `tally_engine_reader`, the group role migration 0008
     of the reporting chain grants SELECT on the four tables metering reads.
   - `TALLY_ENGINE_COUNTER_SOURCES` points at the counter sources file. It
     defaults to `/etc/tally/counter-sources.yaml`, the path the scheduler's
     ConfigMap volume mounts at, which a shell outside the cluster does not
     carry.
   - `TALLY_ENGINE_VM_URL` names the VictoriaMetrics instance the metricsql
     sources are queried against, and the port-forward reaches Service
     `victoriametrics` on port 8428. A deployment whose counter sources
     declare no metricsql source leaves both out.

2. The file is read before either database is dialed, so a path that names no
   file ends the run there:

   ```sh
   tally-engine run --period 2026-07
   ```

   ```text
   reading the counter sources ./counter-sources.yaml: open ./counter-sources.yaml: no such file or directory
   ```

## Meter and rate the month

1. Run the month. The three lines are the run and its id, what it metered, and
   the findings it recorded:

   ```sh
   tally-engine run --period 2026-07
   ```

   ```text
   run <run id> completed for 2026-07 with pricing model 2026-03
   metered <n> candidates into <n> usage records, <n> rated records and <n> project statements
   warnings recorded in runs.stats: 0 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 0 unregistered projects
   ```

2. Where an hourly tick metered the month first, a `superseded run <run id>`
   line per superseded run stands between the second and the third line. The
   run named there is the one this run took over from, and its records no
   longer bill the period.

3. The two unpriced entries above are `openstack/image` and
   `openstack/loadbalancer`, the resource types the imported model prices
   nothing for. `runs.stats.unpriced` holds them as
   `{platform, resource_type, count}`, and `count` counts resources rather than
   their drafts.

4. A `counter_source_failed` warning is a metricsql source the run could not
   measure, so the metric is left out of that interval. Restart the
   port-forward, check that `TALLY_ENGINE_VM_URL` answers, and run the month
   again; the new run supersedes the one that carries the warnings and names it
   as `superseded run <run id>` on its own output.

## Check the result

1. Read the last line of the run. Each of its seven counts is one class of
   finding the run stored in `runs.stats`:

   ```text
   warnings recorded in runs.stats: 0 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 0 unregistered projects
   ```

   - metering: resources metered on incomplete data, such as a candidate with
     no history or a history that starts without a create.
   - counter: counter sources that failed or whose identity values no query may
     carry, one per draft and metric.
   - attribution: projects claimed by more than one attributor, and projects
     sitting in a cycle.
   - adjustment: relations whose kickbacks were dropped because the target is
     not a partner.
   - unpriced resource types: `(platform, resource_type)` pairs the model
     prices nothing for, with the resources of each.
   - unreadable fields: usage fields a value was stored under that no quantity
     could be read from. The field is billed the way an absent one is, at zero.
   - unregistered projects: project ids the drafts carried that the registry
     does not hold. Each is billed standalone.

2. A run that failed exits 1 with the reason on stderr and leaves its run row
   as `failed` with the same reason in its stats. A period another process is
   metering, and a period that is already finalized, are both refused before a
   row exists.

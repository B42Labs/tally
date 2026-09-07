---
title: Detect late events
description: Ask a metered month which events reached the reporting database after the run that bills it read them.
quadrant: how-to
audience: operator
---

# Detect late events

This guide reports the events that arrived after the run that bills a month
read it. Nothing is changed by it: booking those events is what
[correct a finalized period](/how-to/engine/correct-a-finalized-period) does.
Why a month takes late events at all is in
[billing period lifecycle and corrections](/explanation/billing-period-lifecycle-and-corrections).

## Before you start

- Both databases reachable from the machine you run the CLI on: the engine's
  own and the Reporting API's, the second one through a login role that is a
  member of `tally_engine_reader`.
- A [finalized](/how-to/engine/finalize-a-period) run of the month. The report
  is held against the latest finalized run of the period and the instant that
  run read the events at.
- `tally-engine` at the version the deployment runs, with its subcommands on
  the [engine CLI](/reference/command-line/tally-engine) page.
- The [engine settings](/reference/configuration/tally-engine) page, which
  names the two variables this guide sets.

## Ask the month what arrived late

1. Export both databases. `detect-late` reads the run and its snapshot from the
   engine database and the events from the reporting one:

   ```sh
   export TALLY_ENGINE_DB_URL='postgres://tally:password@db.internal:5432/tally_engine?sslmode=require'
   export TALLY_ENGINE_REPORTING_DB_URL='postgres://tally_engine:password@db.internal:5432/tally_reporting?sslmode=require'
   ```

2. Name the month. The first line states the run and the instant its events are
   held against, so what the report compares is said before it:

   ```sh
   tally-engine detect-late --period 2026-07
   ```

   ```text
   run <run id> read 2026-07 at <snapshot>
   no events arrived later
   ```

3. A month that did take late events lists one resource per line, then the
   command that books them:

   ```sh
   tally-engine detect-late --period 2026-07
   ```

   ```text
   run <run id> read 2026-07 at <snapshot>
   os-prod-eu1/openstack/instance/<resource id>: <n> late events, last received <instant>
   book them with tally-engine correct --period 2026-07
   ```

   The resources past the report's cap are counted on one further line,
   `and <n> more resources with late events`.

## Check the result

1. Read which of the two answers came back. `no events arrived later` means
   every event the period bills was already in the reporting database when the
   run read it, and the finalized numbers stand.

2. A resource line names the cloud, the platform, the resource type and the
   resource id, how many of its events arrived after the snapshot, and when the
   last of them did. The month's numbers do not yet account for them:
   [correct](/how-to/engine/correct-a-finalized-period) is what books them into
   credit notes.

3. A period with no finalized run is refused, and the error names the two
   commands that produce one:

   ```text
   2026-07 has no finalized run, and late events are late against one: tally-engine run --period 2026-07 and tally-engine finalize produce it
   ```

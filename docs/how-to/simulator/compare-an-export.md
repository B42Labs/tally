---
title: Compare an export against the oracle
description: Hold the CSV export of a metered simulated month against the oracle the generator wrote, and read the report it prints.
quadrant: how-to
audience: contributor
---

# Compare an export against the oracle

This guide meters a simulated month, exports the run as CSV, and holds that
export against `oracle.json`, the generator's own statement of the month. The
month the oracle describes is in
[the simulated OpenStack world](/explanation/the-simulated-openstack-world).

## Before you start

- The `oracle.json` of the month, written by a `run --out` of the same seed,
  period, and cloud, which
  [replay a recorded month](/how-to/simulator/replay-a-recorded-month) covers.
- A month whose events the Reporting API holds, and both databases reachable
  from the machine you run `tally-engine` on.
- An empty directory for the export. `compare` reads `rated.csv` out of it, so
  the export is the CSV one of [export a run](/how-to/engine/export-a-run).
- The pricing model file the run rated with, such as `pricing/2026-03.yaml`.
- The [simulator command line](/reference/command-line/tally-openstack-simulator)
  page, which states what
  [the oracle](/reference/command-line/tally-openstack-simulator#the-oracle)
  holds.
- The [simulator settings](/reference/configuration/tally-openstack-simulator)
  page, which states that `compare` reads none of the variables: it takes the
  three files it compares from its flags.

## Compare the export

1. Meter the month, export the run as CSV, and hold the export against the
   oracle:

   ```sh
   tally-engine run --period 2026-07
   tally-engine export --run <id> --format csv --out /tmp/export
   tally-openstack-simulator compare --oracle /tmp/m/oracle.json \
     --export /tmp/export --pricing pricing/2026-03.yaml
   ```

2. Read the report against what it compares. Per resource it holds the bounds of
   every interval, the state and the project it was booked under, and the
   quantity of every `time_gauge` dimension the model prices the resource type
   by: a dimension named after a size member against that member, `count`
   against 1, `minutes` against the interval's whole seconds over sixty, and a
   member the size lacks or holds as text against 0. The quantities are compared
   as decimals rounded to four places, the rounding the export prints. The
   intervals of the two sides are held against each other by index rather than by
   their bounds, so an interval one fold split in two is reported at the place
   the two folds part ways.

3. Expect the unpriced resource types on lines of their own rather than among
   the differences. The `counter` dimensions are left out, `egress_gb` among
   them, and so are the amounts, the state modifiers, the statement totals, and
   the resource types the model does not price. Under `pricing/2026-03.yaml` an
   `image` and a `loadbalancer` are unpriced:

   ```text
   image: 9 resources are not priced by pricing model 2026-03 and were not compared
   loadbalancer: 5 resources are not priced by pricing model 2026-03 and were not compared
   ```

   Rated records of another cloud or another platform are skipped and counted on
   one more line, `skipped N rated records of other clouds or platforms`, which
   is what an export of a deployment that bills more than the simulated cloud
   carries.

## Check the result

1. Read the last line, the verdict. It is `the export matches the oracle over N
   resources` or `N of M resources differ from the oracle`. A comparison that
   matches exits 0. One that differs exits 1, and cobra prints the error
   `N resources differ from the oracle` on stderr under the lines, so a drill
   that runs unattended fails where it ran.

2. Read the differences above it. Each is one line,
   `<resource_type> <resource_id>: <detail>`, with ` (and N more)` appended when
   the resource carries further ones. The lines are sorted by resource type and
   then by id, and every instant in them is written in UTC as RFC 3339. The
   details:

   - `missing from the export` and `not in the oracle`, for a resource one side
     holds and the other does not.
   - `the export lacks [a, b)` and `the export books [c, d), which the oracle
     does not hold`, when the two sides carry a different number of intervals for
     one resource.
   - `the oracle expects [a, b) and the export books [c, d)`, for two intervals
     that differ in their bounds.
   - `state "x" over [a, b), the oracle expects "y"` and `project x over [a, b),
     the oracle expects y`.
   - `<dimension> <actual> over [a, b), the oracle expects <expected>`, both at
     four places, and `no <dimension> quantity over [a, b)` when the export books
     the interval without that dimension.
   - `the export books [a, b) under more than one state or project` and
     `the export rates [a, b) by <dimension> more than once`, the two details
     that stop the comparison of their resource.

3. Read a refusal as an export that was not compared at all. Four of them are
   refused, because each would turn every resource of the month into a
   difference:

   - A `rated.csv` without a rated record of the oracle's cloud on platform
     `openstack`: `rated.csv holds no rated record of cloud os-sim on platform
     openstack: the run that wrote it did not bill this month`.
   - One whose period columns name another month: `rated.csv bills [a, b) and
     the oracle describes [c, d)`.
   - One rating a resource type or a dimension the model at hand does not price:
     `rated.csv rates instance by vcpus, which pricing model 2026-03 does not
     price: pass the model the run rated with`.
   - One the model prices a rated resource type by a `time_gauge` no record rates
     it by: `pricing model 2026-03 prices instance by disk_gb and rated.csv rates
     no record by it: pass the model the run rated with`.

   The last two are one gate held in both directions. A `counter` is outside it,
   because a comparison reads none of them.

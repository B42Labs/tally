---
title: Finalize the month and export it
description: Close July 2026 on the attributed run, see that the month cannot be metered again, and export it as JSON documents and as CSV tables.
quadrant: tutorial
audience: all
---
<!-- Shown output captured on 2026-09-07 from commit 01d2aff with kind v0.32.0, kubectl v1.36.1, Docker Desktop 4.86.0, Go 1.27.1 on macOS 15.7.4. -->

# Finalize the month and export it

In this lesson you close July 2026 on the run Attribute a tenant to its
Gardener project made, see the engine refuse to meter the month again and
refuse a second close, and export the finalized run twice as JSON and once as
CSV.

At the end the engine database holds `2026-07` finalized on `RUN_ID`, three new
export directories stand under `~/tally-tutorial/` (`2026-07-final`,
`2026-07-final-again` and `2026-07-final-csv`), and the simulator still holds
its 84 notifications for the lesson after this one.

This lesson takes about 5 minutes.

## Before you start

- The state
  [Attribute a tenant to its Gardener project](/tutorials/attribute-a-tenant-to-its-gardener-project)
  leaves:

  - everything Pay a reseller a kickback left;
  - the registry complete, with eight projects, the meta-project `acme`, the
    partner `cloudhouse` and six relations;
  - `RUN_ID` on the attributed run;
  - the export under `~/tally-tutorial/2026-07-attributed`.

- `jq` 1.8.1 (`jq --version`).

If you closed that shell, restore it with this block:

```sh
export TALLY_ENGINE_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_engine?sslmode=disable'
export TALLY_ENGINE_REPORTING_DB_URL='postgres://tally_engine:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
export RUN_ID="$(jq -r .run_id ~/tally-tutorial/2026-07-attributed/run.json)"
```

This lesson meters nothing, so it needs no token, no counter sources and no
port-forward. `finalize` and `periods list` read the engine database, and
`export` reads the reporting database as well, for the membership its rollup
sums. `RUN_ID` is read back off the last export's `run.json`, which carries the
id of the attributed run.

A connection error from any `go run` command means the cluster is not up, and
`kind get clusters` then prints nothing. That is cured by
[Set up your local Tally](/tutorials/set-up-your-local-tally).

## Close the month

1. Close July 2026 on the attributed run:

   ```sh
   go run ./cmd/tally-engine finalize --period 2026-07 --run "$RUN_ID"
   ```

   ```text
   run 77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 finalized, period 2026-07 closed
   ```

   The line has to match with your own run id in it, the id `RUN_ID` carries.
   The run and the period move in one transaction.

   Finalization is a person's step because the finalized numbers may reach an
   ERP, and the hourly tick never takes it on the dev stack:
   [why finalization is a human gate](/explanation/billing-period-lifecycle-and-corrections#why-finalization-is-a-human-gate).

   An error opening on `the run is not completed` and naming the run as
   `superseded` means `RUN_ID` names a run a later run replaced. The last line
   of the restore block above sets it to the id the last export carries, which
   is the cure where only the shell lost it. Where that export is itself the
   stale one, because the month was metered again after it was written, meter it
   once more in a shell that carries `TALLY_ENGINE_COUNTER_SOURCES`,
   `TALLY_ENGINE_VM_URL` and the VictoriaMetrics port-forward, which
   [Attribute a tenant to its Gardener project](/tutorials/attribute-a-tenant-to-its-gardener-project)
   restores: `go run ./cmd/tally-engine run --period 2026-07` prints a fresh
   completed run id on its first line. Its warnings line has to read
   `0 counter` before you close over that run, because a run made without the
   counter source or without the port-forward completes with the `egress_gb`
   quantity missing rather than with an error, and a finalized run is changed by
   nothing but a correction. Put the id of that first line into `RUN_ID` with
   `export RUN_ID=<that id>` before you close over it, because `RUN_ID` still
   names the run the fresh one superseded. Closing over the fresh run produces
   the same numbers, because nothing in the month or the registry changed.
   An error opening on `the billing period is finalized` means this step ran
   before.

2. List the periods the engine holds:

   ```sh
   go run ./cmd/tally-engine periods list
   ```

   ```text
   2026-07 finalized finalized_run=77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 finalized_at=2026-09-07T22:04:35Z
   2026-08 grace
   ```

   The first line has to read `2026-07 finalized`, and its `finalized_run` has
   to be the id in `RUN_ID`. `finalized_at` is your own.

   The `2026-08` line is the tick's: the cluster's hourly `tally-engine`
   scheduler opens the months that have ended and moves them into their grace
   window. Its status is your own, `grace` on this run, and the line is absent
   on a machine whose cluster has not passed an hour mark yet.

## See that the month is closed

1. Meter the month again:

   ```sh
   go run ./cmd/tally-engine run --period 2026-07
   ```

   ```text
   Error: the billing period is finalized: 2026-07 was closed by run 77fb51f8-82c9-4d52-98a2-7fd3e0b43c53, and a finalized period is changed with tally-engine correct --period 2026-07
   exit status 1
   ```

   The error has to match with your own run id in it. The refusal comes before
   a run row exists, and it names the command that changes a finalized period,
   `tally-engine correct --period 2026-07`, which the next lesson runs.

2. Close the month a second time:

   ```sh
   go run ./cmd/tally-engine finalize --period 2026-07 --run "$RUN_ID"
   ```

   ```text
   Error: the run is not completed: run 77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 is finalized, and a period is closed over a completed run, which tally-engine run --period 2026-07 produces
   exit status 1
   ```

   The error has to match with your own run id in it. The run's own status,
   `finalized`, is what refuses the second close, and it is checked before the
   period is.

   The records of a finalized run are held by database triggers rather than by
   the CLI alone, so a script against the engine database cannot rewrite them
   either:
   [why a finalized run is immutable](/explanation/billing-period-lifecycle-and-corrections#why-a-finalized-run-is-immutable).
   The hourly tick leaves a finalized month alone.

## Export the statements as JSON

1. Export the finalized run with a rollup over `member_of`:

   ```sh
   go run ./cmd/tally-engine export --run "$RUN_ID" --format json --out ~/tally-tutorial/2026-07-final --rollup member_of
   ```

   ```text
   run 77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 exported for 2026-07 as json into /Users/berendt/tally-tutorial/2026-07-final
   wrote run.json and 6 statements
   wrote kickbacks.json with 1 kickbacks
   wrote 1 rollup documents over member_of
   ```

   The last three lines have to match. The run id and the home directory are
   your own.

2. Read the head of the index the export wrote:

   ```sh
   jq '{run_id, kind, status, pricing_version, corrects_run_id}' ~/tally-tutorial/2026-07-final/run.json
   ```

   ```json
   {
     "run_id": "77fb51f8-82c9-4d52-98a2-7fd3e0b43c53",
     "kind": "regular",
     "status": "finalized",
     "pricing_version": "2026-03",
     "corrects_run_id": null
   }
   ```

   `kind` `regular`, `status` `finalized`, `pricing_version` `2026-03` and
   `corrects_run_id` null have to match. `run_id` is your own. `status` is the
   one member that differs from the export Attribute a tenant to its Gardener
   project wrote of the same run.

3. Export the same run a second time, into a directory of its own:

   ```sh
   go run ./cmd/tally-engine export --run "$RUN_ID" --format json --out ~/tally-tutorial/2026-07-final-again --rollup member_of
   ```

   ```text
   run 77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 exported for 2026-07 as json into /Users/berendt/tally-tutorial/2026-07-final-again
   wrote run.json and 6 statements
   wrote kickbacks.json with 1 kickbacks
   wrote 1 rollup documents over member_of
   ```

   The same three lines have to match.

4. Compare the two directories:

   ```sh
   diff -r ~/tally-tutorial/2026-07-final ~/tally-tutorial/2026-07-final-again
   ```

   The command prints nothing: the two directories are byte-identical, the
   rollup included, because no relation changed between the two exports. The
   statements are a function of the run alone, but a rollup reads the registry
   when the export runs, so a relation created or closed retroactively between
   two exports of one finalized run changes the rollup document, and with it the
   rollup index `run.json` carries:
   [rollup](/reference/formats/exports#rollup).

   An
   `Error: --out: ... is not empty, and an export does not remove what an earlier one left there`
   refusal instead of the four lines above means the directory was used before,
   and a fresh directory name is the cure.

## Export the statements as CSV

1. Export the same run as tables:

   ```sh
   go run ./cmd/tally-engine export --run "$RUN_ID" --format csv --out ~/tally-tutorial/2026-07-final-csv
   ```

   ```text
   run 77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 exported for 2026-07 as csv into /Users/berendt/tally-tutorial/2026-07-final-csv
   wrote rated.csv with 3101 rated records
   wrote kickbacks.csv with 1 kickbacks
   ```

   `wrote rated.csv with 3101 rated records` and
   `wrote kickbacks.csv with 1 kickbacks` have to match. 3101 is the rated
   record count the `metered` line of every run of this track printed.

2. List what the export wrote:

   ```sh
   ls ~/tally-tutorial/2026-07-final-csv
   ```

   ```text
   kickbacks.csv
   rated.csv
   ```

   Two files, and no index beside them: every row of a table carries the run
   and its period, so a table says which run it belongs to on its own.

3. Read the header and the first two rows of the rated table:

   ```sh
   head -3 ~/tally-tutorial/2026-07-final-csv/rated.csv
   ```

   ```text
   run_id,kind,corrects_run_id,period_from,period_to,cloud,platform,resource_type,resource_id,project_id,state,from_ts,to_ts,dimension,quantity,amount,currency
   77fb51f8-82c9-4d52-98a2-7fd3e0b43c53,regular,,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,os-sim,openstack,floating_ip,11327c4f-5563-4b70-9769-7ec877ff59b8,018504a6cc10019a40e3f9eef4dae529,active,2026-07-01T02:31:36Z,2026-07-22T01:55:52Z,count,1.0000,2.52,EUR
   77fb51f8-82c9-4d52-98a2-7fd3e0b43c53,regular,,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,os-sim,openstack,floating_ip,28b8f170-6b0c-474a-8b0d-a65b479c8e98,d5a8024946ddf673277b9e2490643a2c,active,2026-07-01T03:07:27Z,2026-08-01T00:00:00Z,count,1.0000,3.70,EUR
   ```

   The header has to match, and it is the column order of the table. There is
   one row per rated record, a resource, a state span and a dimension each. The
   run id in every row is your own, and the rest of a row is the seed's.

4. Count the lines of the table:

   ```sh
   wc -l < ~/tally-tutorial/2026-07-final-csv/rated.csv
   ```

   ```text
       3102
   ```

   3102 has to match, the 3101 records and the header. The leading spaces are
   how the `wc` of macOS pads its count.

5. Count the rows of one tenant:

   ```sh
   awk -F, '$10 == "005be5adeef3d87e280d03d9d57c38b4"' ~/tally-tutorial/2026-07-final-csv/rated.csv | wc -l
   ```

   ```text
        827
   ```

   827 has to match: the rows whose tenth column, `project_id`, is the tenant
   of `alpha`. The table carries the tenant that owned each resource, and the
   attribution to `alpha` stands in the statements alone, which is why the JSON
   export has no statement for that tenant while the table has its rows.

6. Read the kickback table:

   ```sh
   cat ~/tally-tutorial/2026-07-final-csv/kickbacks.csv
   ```

   ```text
   run_id,kind,corrects_run_id,period_from,period_to,beneficiary,cloud,project_id,relation_id,scope,rate,base,amount,currency
   77fb51f8-82c9-4d52-98a2-7fd3e0b43c53,regular,,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,cloudhouse,os-sim,10e287d5788957a2a331cabd1b5dccdf,2ccfc925-6f65-41e5-a180-4d12d25e283a,all,0.100000,593.55,59.36,EUR
   ```

   The header has to match, and there is one row, the `cloudhouse` kickback of
   Pay a reseller a kickback, with the ids in it your own.

## What you learned

- A period moves from open through grace to finalized, and the close is the one
  step a person takes:
  [the lifecycle](/explanation/billing-period-lifecycle-and-corrections#the-lifecycle).
- A finalized run cannot be metered again or rewritten, because the database
  refuses it:
  [why a finalized run is immutable](/explanation/billing-period-lifecycle-and-corrections#why-a-finalized-run-is-immutable).
- The statements an export writes are reproducible because they are a function
  of the run and nothing else, while a rollup reads the registry as it runs:
  [exports](/explanation/billing-period-lifecycle-and-corrections#exports).
- The files an export writes are in
  [files](/reference/formats/exports#files), the columns of the tables in
  [CSV tables](/reference/formats/exports#csv-tables), and the flags of
  `finalize`, `periods list` and `export` in
  [tally-engine](/reference/command-line/tally-engine).

## Where to go next

[Book the late events as a credit note](/tutorials/book-the-late-events-as-a-credit-note)
releases the 84 notifications the simulator holds and books them as a
correction of the month you just closed.

It starts from the state this lesson leaves behind:

- everything Attribute a tenant to its Gardener project left;
- `2026-07` finalized on `RUN_ID` in the engine database;
- the three export directories `~/tally-tutorial/2026-07-final`,
  `2026-07-final-again` and `2026-07-final-csv`;
- the simulator still holding its 84 notifications.

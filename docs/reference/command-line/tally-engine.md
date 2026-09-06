---
title: Metering engine CLI (tally-engine)
description: Every subcommand and flag of the engine CLI, what each one needs from the environment and what it prints.
quadrant: reference
audience: operator
---

# Metering engine CLI (tally-engine)

`tally-engine` is the metering engine's operator tool and its scheduler
entrypoint. An operator drives a billing period through it: `run` meters and
rates a period, `finalize` closes it, `detect-late` and `correct` deal with what
arrived afterwards, `export` writes the result out, and `kickbacks` reports what
a run owes its partners. `tick` is the same tree run unattended by an hourly
CronJob.

It is also the only thing that runs DDL on the engine database. `migrate`
applies the embedded goose chain, `migrate-status` reports which of it a
database carries, and `migrate-down-to` runs the chain's down migrations.
Nothing else migrates as a side effect, so a schema change stays an operator's
decision rather than something a scheduled run brings along. The tree is built
in
[`cmd/tally-engine/main.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-engine/main.go)
and its subcommands in
[`cmd/tally-engine/commands.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-engine/commands.go).

## Environment

Every setting comes from the environment, under the names the metering engine
settings page lists. It is read when a subcommand runs rather than when the tree
is built, so `--help` needs no configuration at all.

How much of it a subcommand needs differs, and each gate refuses a missing value
before the first query rather than at it.

`migrate`, `migrate-status`, `migrate-down-to`, `periods list`, `finalize`,
`pricing import`, `pricing list`, `kickbacks` and an `export` without `--rollup`
ask for the engine database alone, through `TALLY_ENGINE_DB_URL`. A pool opens
on a database carrying an older schema as happily as on a migrated one, so every
subcommand but the three migration ones checks the schema before it works.

`detect-late` and `export --rollup` ask for both databases: the engine one and
the reporting one, through `TALLY_ENGINE_REPORTING_DB_URL`. The resources, the
events and the project relations come from the reporting database, and a rollup
reads the membership it sums under when the export runs.

`run`, `correct` and `tick` ask for both databases and for the counter sources
file `TALLY_ENGINE_COUNTER_SOURCES` names. The file is read before either
database is dialed. Only a file that declares a metricsql source makes a
VictoriaMetrics client necessary, and that client is built from
`TALLY_ENGINE_VM_URL`; a deployment measuring no metricsql counter leaves the
endpoint unset.

## Output

Every subcommand writes its lines to stdout and its refusals to stderr.

`run` prints one line per stale run of the period it took over, as
`reclaimed stale run <run id>`, then
`run <run id> completed for <period> with pricing model <version>` and
`metered <n> candidates into <n> usage records, <n> rated records and <n> project statements`.
A run that applied adjustments adds `applied <n> pricing adjustments`, and a run
that superseded an earlier run of the same period adds
`superseded run <run id>` for each one. A warning the run recorded on its result
prints as `warning: <code>: <detail>`. The findings held in `runs.stats` are
counted rather than named, on one line reading
`warnings recorded in runs.stats: <n> metering, <n> counter, <n> attribution, <n> adjustment, <n> unpriced resource types, <n> unreadable fields, <n> unregistered projects`.

`export` prints
`run <run id> exported for <period> as <format> into <directory>` and then one
line per file the format left there. `--format json` writes
`wrote run.json and <n> statements`, with `credit notes` in place of
`statements` for a correction run, and
`wrote kickbacks.json with <n> kickbacks`, with `kickback deltas` in place of
`kickbacks` for a correction. `--format csv` writes
`wrote rated.csv with <n> rated records`, then
`wrote deltas.csv with <n> deltas` for a correction, then
`wrote kickbacks.csv with <n> kickbacks`. A `--rollup` export adds
`wrote <n> rollup documents over <relation type>` under json and
`wrote rollup.csv with <n> members over <relation type>` under csv.

`detect-late` prints `run <run id> read <period> at <instant>` first, so what
the events are held against is stated before them. A period nothing arrived for
prints `no events arrived later`. Every other resource prints as
`<cloud>/<platform>/<resource type>/<resource id>: <n> late events, last received <instant>`,
the resources past the report's cap are counted on
`and <n> more resources with late events`, and the last line is
`book them with tally-engine correct --period <period>`.

`correct` prints `reclaimed stale run <run id>` the way `run` does, then
`run <run id> completed as a correction of run <run id> for <period> with pricing model <version>`
and `metered <n> candidates into <n> usage records and <n> rated records`. What
it booked prints as `<n> deltas and <n> adjustment deltas in <n> credit notes`,
as `<n> deltas in <n> credit notes` where it moved no adjustment, and as
`no deltas: the finalized numbers of <period> stand` where it found none.
`superseded correction run <run id>`, the `warning:` lines and the
`runs.stats` count follow as they do for a run.

`tick` prints one line per step a month took, in the order the lifecycle takes
them: `<month> <transition>` for a period that changed status,
`<month> run <run id> completed` or `<month> run <run id> finalized` for the run
it executed, `<month> warning: <message>`,
`<month> not metered after <n> failed runs, retried from <instant>` for a month
the backoff holds back, and `<month> failed: <message>` for a month that broke.
A month the walk's cap left out prints
`<n> months before <month> were skipped, and are billed with tally-engine run --period`.
A tick that moved nothing prints `nothing due`.

`kickbacks` writes the settlement document alone. Nothing else reaches stdout,
so the report pipes into a file or a partner mailer as it is.

## Exit status and signals

A subcommand that did what it was asked exits 0, and every error exits 1 with
the reason on stderr. `tick` is the one that prints before it fails: a walk that
broke on one month still moved the others, so what it did goes to stdout and the
exit status carries the failure.

SIGINT and SIGTERM cancel the context the tree runs on, which reaches the
database calls a run spends its minutes in. The bookkeeping of an interrupted
run writes the failed run and gives the period lock back on a context of its
own, so the next run finds the period rather than a lock nothing holds.

## The scheduler

[`deploy/kubernetes/base/tally-engine/tally-engine.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/tally-engine/tally-engine.yaml)
runs `tick` as a CronJob on the schedule `0 * * * *`, with
`concurrencyPolicy: Forbid`. A run takes an advisory lock on the period it
meters and fails rather than waits when another process holds it, so a tick that
outlasted its hour would make the next one fail on the month it is still working
through.

## Commands

<!-- refdoc:begin commands -->
### `tally-engine`

Meter, rate, and finalize the billing periods

```text
tally-engine
```

| Subcommand | Purpose |
| --- | --- |
| `correct` | Meter a finalized billing period again as a correction |
| `detect-late` | Report the events that reached a metered period late |
| `export` | Export the project statements of a run |
| `finalize` | Finalize a completed run and close its billing period |
| `kickbacks` | Report the kickbacks a run owes its partners |
| `migrate` | Apply the pending migrations of the engine database |
| `migrate-down-to` | Roll the engine database back to a migration version |
| `migrate-status` | Report which migrations the engine database carries |
| `periods` | Work with the billing periods |
| `pricing` | Work with the pricing catalogs |
| `run` | Meter and rate one billing period |
| `tick` | Run the scheduler tick the hourly CronJob invokes |

This command takes no flags.

### `tally-engine correct`

Meter a finalized billing period again as a correction

Meter a finalized billing period again as a correction.

The correction meters the month again from the full event history with the pricing version the finalized run used, stores every non-zero difference against the latest finalized run as a credit or debit delta, and renders one credit note per affected project. The finalized run stays as it is, because its numbers may already have reached an ERP; a completed correction is finalized with finalize, and the next correction diffs against it.

```text
tally-engine correct [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--period` | string | none | yes | billing month to correct, YYYY-MM |

### `tally-engine detect-late`

Report the events that reached a metered period late

Report the events that reached a metered period late.

It names what the reporting database received after the run that bills the period read it. Nothing is changed: booking those events is what correct does.

```text
tally-engine detect-late [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--period` | string | none | yes | billing month to check, YYYY-MM |

### `tally-engine export`

Export the project statements of a run

Export the project statements of a run.

json writes run.json and one statement document per project, or one credit note per project for a correction run. csv writes the rated records into rated.csv, and the deltas of a correction into deltas.csv. Both formats write the run's partner settlement beside them, kickbacks.json or kickbacks.csv, empty when the run owes nobody. --rollup member_of or --rollup managed_by writes one rollup document per meta-project or partner beside the statements, `rollup-<key>.json` or rollup.csv, summing the statements of its members; it reads the membership from the reporting database when the export runs, so TALLY_ENGINE_REPORTING_DB_URL has to be set for it. --out has to be empty or absent, so what it holds afterwards is one run's artifacts and nothing an earlier export of another run left there. Exporting a finalized run twice into a clean directory yields the same files, because a finalized run's records no longer change. A --rollup export is the exception: the membership is read from the registry when the export runs, so two exports differ where a relation was created or closed between them.

```text
tally-engine export [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--format` | string | none | yes | output format: json or csv |
| `--out` | string | none | yes | directory the exported files are written to |
| `--rollup` | string | none | no | relation type to sum the statements under, member_of or managed_by: one document per meta-project or partner; no rollup when absent |
| `--run` | string | none | yes | id of the run to export |

### `tally-engine finalize`

Finalize a completed run and close its billing period

Finalize a completed run and close its billing period.

The run's records become immutable and the period stops taking new ones. What arrives afterwards is booked by a correction, which records the difference between the finalized run and a fresh metering as credit and debit deltas; the finalized run stays as it is. A completed correction is finalized through this same command, which makes its deltas and credit notes immutable and leaves the period naming the run that closed it.

```text
tally-engine finalize [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--period` | string | none | yes | billing month the run bills, YYYY-MM |
| `--run` | string | none | yes | id of the run to finalize |

### `tally-engine kickbacks`

Report the kickbacks a run owes its partners

Report the kickbacks a run owes its partners.

The document lists, per partner and currency, the kickback total, the number of projects it came from and one entry per kickback record. json prints the settlement document, csv one row per record. A month alone reports the regular run that bills it; a correction named with --run reports the differences to the run it corrects, negative where usage was corrected down. Only a completed or finalized run is reported. A partner named with --beneficiary is reported alone, which is what that partner receives, and a partner the run settles nothing for is refused; without it the document holds every partner the run owes.

```text
tally-engine kickbacks [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--beneficiary` | string | none | no | report only this partner's kickbacks; every partner the run owes when absent |
| `--format` | string | `json` | no | output format: json or csv |
| `--period` | string | none | yes | billing month to report, YYYY-MM |
| `--run` | string | none | no | id of the run to report; the month's regular run when absent |

### `tally-engine migrate`

Apply the pending migrations of the engine database

Apply the pending migrations of the engine database.

No other subcommand runs DDL, so this is what brings a database to the schema the engine expects.

```text
tally-engine migrate
```

This command takes no flags.

### `tally-engine migrate-down-to <version>`

Roll the engine database back to a migration version

Roll the engine database back to a migration version.

Every migration above the given version is undone, which drops the data its tables hold. Passing 0 leaves an empty schema. The chain ships these down migrations, so this is what runs them.

```text
tally-engine migrate-down-to <version> [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--yes` | boolean | `false` | no | confirm that the data of the rolled-back migrations may be dropped |

### `tally-engine migrate-status`

Report which migrations the engine database carries

Report which migrations the engine database carries.

It answers which schema a database is on before code that assumes a newer one is run against it.

```text
tally-engine migrate-status
```

This command takes no flags.

### `tally-engine periods`

Work with the billing periods

```text
tally-engine periods
```

| Subcommand | Purpose |
| --- | --- |
| `list` | List the billing periods and their status |

This command takes no flags.

### `tally-engine periods list`

List the billing periods and their status

List the billing periods and their status.

A line is a period's YYYY-MM month and the status it carries: open, grace, or finalized. A finalized period also names the run that closed it and when that happened.

```text
tally-engine periods list
```

This command takes no flags.

### `tally-engine pricing`

Work with the pricing catalogs

```text
tally-engine pricing
```

| Subcommand | Purpose |
| --- | --- |
| `import` | Import a pricing catalog from a YAML file |
| `list` | List the imported pricing catalogs |

This command takes no flags.

### `tally-engine pricing import <file>`

Import a pricing catalog from a YAML file

Import a pricing catalog from a YAML file.

A catalog is imported once and then referred to by its version, which every rated record carries, so a price change never rewrites what an earlier run billed.

```text
tally-engine pricing import <file>
```

This command takes no flags.

### `tally-engine pricing list`

List the imported pricing catalogs

List the imported pricing catalogs.

A line is a catalog's version and when it was imported, which is what a run's pricing version refers back to.

```text
tally-engine pricing list
```

This command takes no flags.

### `tally-engine run`

Meter and rate one billing period

Meter and rate one billing period.

The run reads the period's resources and events from the reporting database, derives the usage records of every project, and rates them against the imported pricing catalog. It leaves a run row the other subcommands work on.

```text
tally-engine run [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--clouds` | list, comma-separated | none | no | comma-separated clouds to meter; empty meters every configured cloud |
| `--period` | string | none | yes | billing month to meter, YYYY-MM |

### `tally-engine tick`

Run the scheduler tick the hourly CronJob invokes

Run the scheduler tick the hourly CronJob invokes.

It advances every billing period whose next step is due: a period whose grace window has passed gets its run, and a completed run is finalized where TALLY_ENGINE_AUTO_FINALIZE allows it.

```text
tally-engine tick
```

This command takes no flags.
<!-- refdoc:end commands -->

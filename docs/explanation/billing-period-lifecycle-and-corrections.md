---
title: Billing period lifecycle and corrections
description: "Why a finalized period is never edited and a late event becomes a credit note instead."
quadrant: explanation
audience: all
---

# Billing period lifecycle and corrections

A billing period is billed once and then closed. What arrives after it was
closed is not folded back into it: the closed numbers stay as they were, and the
difference is settled as a document of its own. This page says why, and what the
engine does at each step.

## The lifecycle

```text
open ──(period ends)──▶ grace ──(metering and rating run)──▶ finalized
```

A correction is a further run over a finalized period rather than a fourth
stored status. The status a period is stored with is `open`, `grace` or
`finalized`
([migration 0001](https://github.com/B42Labs/tally/blob/main/migrations/engine/0001_init.sql)).

`tally-engine tick` runs hourly as a CronJob
([the engine manifest](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/tally-engine/tally-engine.yaml)).
It walks the months that have ended, moves a month that has just ended from open
into grace, and has a month metered once its grace window has passed. The window
is `TALLY_ENGINE_GRACE_HOURS`, 72 hours by default
([`internal/engine/scheduler`](https://github.com/B42Labs/tally/blob/main/internal/engine/scheduler/scheduler.go)).

The window exists because the last events of a month do not all arrive inside
it. A collector that was buffering to disk drains afterwards, and a
reconciliation sync writes synthetic events for what the collector lost. Metering
the month at midnight on the first would bill the gap.

## Why finalization is a human gate

Finalized usage and rating records are immutable, because they may already have
been handed to an external billing or ERP system. Editing them would leave two
versions of one invoice in circulation.

`tick` finalizes a completed run only where `TALLY_ENGINE_AUTO_FINALIZE` allows
it. Otherwise closing the month is `tally-engine finalize`, a person's decision
(decision D5 of
[the metering and rating roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/03-phase-3-metering-rating.md)).

## Runs before finalization

Runs are versioned by `run_id`. Re-running a period before it is finalized
supersedes the previous run's records in the same transaction, so a reader of
the period never sees two completed runs
([`internal/engine/runs`](https://github.com/B42Labs/tally/blob/main/internal/engine/runs/runs.go)).
Every rated record references its run and the pricing model version rating used,
so re-processing a past period with the same inputs yields identical results.

## Why a finalized run is immutable

The immutability is enforced by database triggers rather than by application
code alone (decision D8). An `UPDATE` or a `DELETE` against the records of a
finalized run is refused by the database, so a script run by hand against the
engine database cannot quietly rewrite a closed month.

## Corrections

Two commands handle what arrives late. `tally-engine detect-late` reports the
resources whose events reached the reporting database after the finalized run
read it, so an operator sees what a correction would move before running one.

`tally-engine correct` meters the period again in full, then diffs its amounts
against those of the finalized run per resource, project and dimension. Only the
non-zero deltas are stored, in `correction_deltas`, and each affected project is
handed one credit note rendered from them. A full re-meter is deterministic and
reproducible; incremental diffing would be an optimisation with no gain in
correctness (decision D6,
[`internal/engine/corrections`](https://github.com/B42Labs/tally/blob/main/internal/engine/corrections/corrections.go)).
The finalized records are never modified in place. The credit note of the
`correction_credit` golden case, three deltas that add up to 24.00 EUR, is in
[worked examples](/explanation/worked-examples).

## Exports

An export reads a completed or a finalized run and nothing else: the superseded,
failed and running rows a period accumulates stay in the database for audit. An
export is a function of the run it reads, with every ordering fixed by the
queries and no artifact recording when it was written, so exporting the same
finalized run twice yields byte-identical files
([`internal/engine/export`](https://github.com/B42Labs/tally/blob/main/internal/engine/export/export.go)).

## The commands

`tally-engine` drives the lifecycle from the command line. These are the
subcommands that belong to it; the tree carries others, and the flags of each
one are documented on the
[tally-engine reference page](/reference/command-line/tally-engine).

- `run` meters and rates one billing period.
- `finalize` finalizes a completed run and closes its billing period.
- `detect-late` reports the events that reached a metered period late.
- `correct` meters a finalized billing period again as a correction.
- `export` exports the project statements of a run.
- `kickbacks` reports the kickbacks a run owes its partners.
- `tick` runs the scheduler tick the hourly CronJob invokes.
- `periods list` lists the billing periods and their status.
- `pricing import` imports a pricing catalog from a YAML file.
- `pricing list` lists the imported pricing catalogs.

---
title: Switch on fault switches
description: Run a simulated month with one or more of the six fault switches on, and read what each of them changes on the bus, in the counters and in the comparison.
quadrant: how-to
audience: contributor
---

# Switch on fault switches

This guide turns fault switches on for a simulated month and reads what they
leave behind. A switch changes what the bus carries and never what the simulated
cloud did, so the oracle states the same intervals whichever switches are on.
What each switch stands for is in
[the fault switches](/explanation/the-simulated-openstack-world#the-fault-switches).

## Before you start

- A month you can run, either as the compose stack of
  [run a simulated month](/how-to/simulator/run-a-month) or as a file-mode
  `run --out`.
- The `oracle.json` of the same seed, period, and cloud, for the comparison that
  [compare an export against the oracle](/how-to/simulator/compare-an-export)
  covers.
- `tally-engine` for the metering, the finalization and the correction the
  `held-back` month asks for.
- The [simulator command line](/reference/command-line/tally-openstack-simulator)
  page, which lists
  [the fault switches](/reference/command-line/tally-openstack-simulator#the-fault-switches)
  one by one.
- The [simulator settings](/reference/configuration/tally-openstack-simulator)
  page, which lists the variables a run reads beside the flags below.

## Turn a switch on

1. Name the switches on the run. `run --faults <name>[,<name>...]` takes them
   comma-separated, and every one of the six is off by default:

   ```sh
   TALLY_SIM_CLOUD=os-sim tally-openstack-simulator run \
     --period 2026-07 --seed 1 --faults duplicates,reordering --out /tmp/m
   ```

   `make simulator-up SIM_FAULTS=duplicates,reordering` carries the same names
   into the compose stack.

2. Keep the two pre-existing switches apart. A run that names both is refused
   before it publishes anything:

   ```sh
   TALLY_SIM_CLOUD=os-sim tally-openstack-simulator run \
     --period 2026-07 --seed 1 --faults pre-existing,missing-create --out /tmp/m
   ```

   ```text
   pre-existing and missing-create exclude each other
   ```

   A run that names something outside the six is refused with `unknown fault
   switch`, and that refusal lists them.

3. Read the marks a faulted run leaves. `oracle.json` states the switches the run
   was started with under `faults`, and every resource carries a `faults` member
   of its own naming the switches that touched it. `compare` reads both: a
   difference on a touched resource carries ` (touched by <names>)` behind it,
   and the line `the month ran with the fault switches <names>` stands above the
   verdict. The verdict and the exit status are the ones a month with no switch
   on prints, so a marked difference is still a difference and counts as one.

## What each switch changes

The figures are those of seed 1 over `2026-07`, whose month with every switch
off carries 15727 notifications and 1812 billable events.

| Switch | What the bus carries | What the collector's counters show | What `compare` shows | Seed 1 over `2026-07` |
| --- | --- | --- | --- | --- |
| `pre-existing` | Every transition of the picked classic instances, the ones before the period start included, in a burst ahead of the month's own first notification. No notification is added or dropped. | The counters of a month with no switch on: 1812 consumed and 13915 skipped, 60 of the consumed events carrying a timestamp before the month. | No difference, and the engine writes no warning. | 5 instances, their 8 volumes and their 5 floating addresses move behind the month start, 18 resources the oracle marks `pre-existing`, with 176 pre-month transitions, 60 of them billable. |
| `missing-create` | The same instances and the same leads as `pre-existing`, with every transition before the period start dropped from the schedule and from the stream. The daily `compute.instance.exists` audits stay. | 1752 consumed and 13799 skipped. | Every touched resource as a difference marked ` (touched by missing-create)`. The engine warns `history_starts_without_create` once per touched resource it sees. | The 176 pre-month transitions of the same 18 resources are dropped, 60 of them billable: 15551 notifications instead of 15727 and 1752 billable events instead of 1812. |
| `duplicates` | One in 20 of the billable transitions a second time, byte for byte and under the message id of the original, ten notifications later. | `tally_collector_consumed_total` counts the repeat, and the Reporting API counts the second copy under `tally_events_deduplicated_total`. | The export and the comparison of the month with the switch off, and the engine writes no warning. | 86 copies on 85 resources: 15813 notifications instead of 15727, with `events.jsonl` and the oracle at the same 1812 events. |
| `reordering` | The first billable transition of a resource directly behind its second, for one in 10 of the resources with at least two of them. The timestamps do not move. | No counter of the collector moves. | No difference: the projection and the engine sort a resource's history by timestamp before they fold it. | The first two billable notifications of 87 resources are swapped, in a month of the same 15727 notifications and 1812 events. |
| `refused-shapes` | A twin behind its original, on the same exchange and under a fresh message id, drawn once in 400 per billable transition: oversized, truncated, or versioned, the last for nova alone. | `tally_collector_unparseable_total` at 20, for the oversized and the truncated twins. A versioned twin is counted as skipped under its versioned type name, which puts three `instance.*` series beside the month's 83 type names. | The export and the comparison of the month with the switch off, because a twin is billable to nobody. | 87 twins on 84 resources, 67 versioned, 15 truncated and 5 oversized: 15814 notifications instead of 15727. The `notifications.jsonl` of such a month cannot be replayed. |
| `held-back` | Every billable transition but one in 20. The rest goes to `held-back.jsonl`, and the run holds until `POST /release` lets it out. | 1728 consumed while the month publishes, 1812 once the release is through, and 13915 skipped either way. | The held resources as differences marked ` (touched by held-back)`, until the export of the correction is compared. | 84 notifications held back on 81 resources: `notifications.jsonl` has 15643 lines, `held-back.jsonl` 84, and the month books the 1812 events of the month with the switch off. |

## Run a held-back month

1. Publish the month with the switch on, and write the oracle of it from a
   file-mode run of the same seed, period, and cloud:

   ```sh
   make simulator-up SIM_PERIOD=2026-07 SIM_FAULTS=held-back
   TALLY_SIM_CLOUD=os-sim tally-openstack-simulator run \
     --period 2026-07 --seed 1 --faults held-back --out /tmp/m
   ```

2. Meter the month as the collector recorded it, close it, and hold it against
   the oracle, once `http://127.0.0.1:8091/clock` reports `holding` true:

   ```sh
   tally-engine run --period 2026-07
   tally-engine finalize --period 2026-07 --run <run id>
   tally-engine export --run <run id> --format csv --out /tmp/export
   tally-openstack-simulator compare --oracle /tmp/m/oracle.json \
     --export /tmp/export --pricing pricing/2026-03.yaml
   ```

   That comparison lists the held resources as differences, each marked
   ` (touched by held-back)`.

3. Let the held share out and correct the closed month, the way
   [correct a finalized period](/how-to/engine/correct-a-finalized-period)
   describes:

   ```sh
   curl -X POST http://127.0.0.1:8091/release
   tally-engine detect-late --period 2026-07
   tally-engine correct --period 2026-07
   tally-engine export --run <correction id> --format csv --out /tmp/corrected
   tally-openstack-simulator compare --oracle /tmp/m/oracle.json \
     --export /tmp/corrected --pricing pricing/2026-03.yaml
   ```

## Check the result

1. Hold the collector's counters against the row of the switch you ran with. A
   month with `pre-existing`, `duplicates` or `reordering` on ends at the 1812
   consumed and 13915 skipped of a month with no switch on; `missing-create` ends
   at 1752 and 13799; `refused-shapes` puts
   `tally_collector_unparseable_total` at 20; `held-back` reaches 1812 consumed
   only once the release is through.

2. Read the `faults` member of `oracle.json` back. It names the switches the run
   was started with, and every resource the run touched names its own.

3. Read the marks in the comparison. The mark says which switch reached the
   resource and nothing about whether the difference is the one the switch was
   turned on for. A difference no mark names is a finding about the engine.

4. Compare the corrected export of the `held-back` month. `detect-late` names the
   released events, `correct` books the difference against the finalized run as
   deltas, and the comparison of that export matches the oracle over every
   resource, with the line naming the switch above the verdict.

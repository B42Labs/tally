---
title: Metering separated from rating
description: "Why usage is recorded in neutral units before any price is applied, and how the engine does both today."
quadrant: explanation
audience: all
---

# Metering separated from rating

The metering engine bills a month in two stages. Metering records what was used
in neutral units. Rating applies a price to those units. Everything that decides
how much a resource costs sits in the second stage, and nothing in the first one
knows about money.

## Why two stages

The separation lets usage be validated before any monetary value is attached to
it, and it lets the engine work the same way whatever platform the usage came
from. The engine keeps its own database, apart from the reporting database it
reads.

Billing periods are calendar months in UTC, which avoids the 23-hour and
25-hour days a daylight saving switch would otherwise hand the engine. A period
is metered as a batch run after the period has ended plus a grace window, which
[billing period lifecycle and corrections](/explanation/billing-period-lifecycle-and-corrections)
describes.

```text
┌──────────────────────────────────┐  ┌──────────────────────────────────┐
│ Reporting database               │  │ VictoriaMetrics                  │
│ (read-only, tally_engine_reader) │  │                                  │
│                                  │  │ counter metrics per resource,    │
│ events, current_resources and    │  │ read back with MetricsQL         │
│ the project graph                │  │                                  │
└─────────────────┬────────────────┘  └─────────────────┬────────────────┘
                  │                                     │
                  ▼                                     ▼
    ┌────────────────────────────────────────────────────────────────┐
    │                            Metering                            │
    │                                                                │
    │  usage records in neutral units, per resource                  │
    │  and billing period, platform-agnostic                         │
    └────────────────────────────────┬───────────────────────────────┘
                                     │
                                     ▼
    ┌────────────────────────────────────────────────────────────────┐
    │                             Rating                             │
    │                                                                │
    │  apply the pricing model version valid for the period          │
    │  rated records, statements and exports                         │
    └────────────────────────────────────────────────────────────────┘
```

The engine reads the reporting database itself, over its own connection and
through the read-only role `tally_engine_reader`, rather than calling the
Reporting API: metering scans whole event histories, and paging those over HTTP
would buy nothing (decision D1 of
[the metering and rating roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/03-phase-3-metering-rating.md)).
Counter metrics are the second input, read back from VictoriaMetrics with a
MetricsQL query.

## Metering

Usage is captured as a `usage` object with metric-specific fields, and it covers
three kinds of usage.

| Usage type | Description | Example metrics |
| --- | --- | --- |
| Time-based | The resource exists for a duration | `minutes`, always present for a time-based resource |
| Gauge-based | The resource has a measurable size at a point in time | `storage_gb`, `worker_count`, `vcpus` |
| Counter-based | Events or volume accumulated inside the billing period | `pulls`, `pushes`, `egress_gb`, `api_calls` |

A single usage record can combine all three. Time and gauge metrics are derived
from the event history; counter metrics are aggregated from the events table or
from VictoriaMetrics.

A run meters a period in four steps.

1. Resolve the project graph as valid during the billing period, because
   relations are temporally scoped (see
   [project registry, relations and exclusive attribution](/explanation/project-registry-relations-and-attribution)).
2. Determine the resources of the project and its related projects that existed
   at any point inside the period, from the event history. `current_resources`
   is the fast index over the resources that exist right now.
3. Replay each resource's event history into an interval timeline, where an
   interval has a constant `size`, `state` and `project_id`, and compute the
   `minutes` of each interval clipped to the period.
4. Aggregate the counter metrics per interval inside the period.

Time is computed in integer seconds, and `usage.minutes` is that second count
divided by 60 and quantised to four decimal places (decision D2, the constant
`money.QuantityPlaces` in
[`internal/core/money`](https://github.com/B42Labs/tally/blob/main/internal/core/money/money.go)).
A `usage` object carries every size field verbatim, string fields such as
`flavor` included, plus `minutes`, `count: 1` and the counter metrics
(decision D9).

## The splitting rule

Any change to a resource's `size`, `state` or `project_id` in its event history
triggers a split. Metering writes two records at the change timestamp, one for
the old configuration up to that instant and one for the new configuration from
it. The rule holds for every resource type on every platform.

The engine bills the Hetzner, STACKIT, Gardener and Harbor histories in the
table below from seeded events today, while no collector emits such events
([the providers that do not exist yet](/explanation/providers-that-do-not-exist-yet)).

| Platform | Resource type | Change event | Split trigger |
| --- | --- | --- | --- |
| OpenStack | Instance | `compute.instance.resize.end` | `size` changes (vcpus, ram_gb, disk_gb, flavor) |
| OpenStack | Instance | `compute.instance.shelve` / `unshelve` | `state` changes (active to shelved to active) |
| OpenStack | Instance | `compute.instance.power_off` / `power_on` | `state` changes (active to shutoff to active) |
| OpenStack | Volume | `volume.resize.end` | `size.size_gb` changes (100 to 200) |
| OpenStack | Volume | `volume.retype` | `size.type` changes (ssd to hdd) |
| OpenStack | Volume | `volume.transfer.accept.end` | `project_id` changes (ownership transfer: usage is attributed to the old project up to T and to the new one from T) |
| Hetzner (Phase 4 design) | Server | `server.upgrade` / `server.downgrade` | `size` changes (server_type) |
| STACKIT (Phase 4 design) | Server | `server.resize` | `size` changes (machine_type) |
| Gardener (Phase 4 design) | Shoot | `shoot.worker.scale` | `size.worker_count` changes (3 to 5) |
| Gardener (Phase 4 design) | Shoot | `shoot.hibernate.start` / `end` | `state` changes (active to hibernated to active) |
| Harbor (Phase 4 design) | Repository | `repository.push` / image delete | `size.storage_gb` changes |

## Concurrency

A run takes one engine-side advisory lock per billing period, so two runs of the
same period never overlap. Its reads of the reporting database all go through
one `REPEATABLE READ` snapshot, which is consistent by construction because
`events` is append-only. Per-resource advisory locks are not taken for metering
reads; they remain what the projection writer uses (decision D3,
[`internal/engine/source`](https://github.com/B42Labs/tally/blob/main/internal/engine/source/source.go)).

## The four invariants

Four invariants are checked on every run, and a breach fails the run before
anything is persisted, rather than producing usage that is silently wrong
([`internal/engine/invariants`](https://github.com/B42Labs/tally/blob/main/internal/engine/invariants/invariants.go)).

- No gaps and no overlaps: the usage records of a resource inside a period are
  contiguous and do not overlap.
- Coverage: the sum of `minutes` over the splits equals the intersection of the
  resource's lifetime with the period. A resource that exists through the whole
  of March 2026 is billed 44,640 minutes.
- Traceability: every split boundary is an edge of the period or the timestamp
  of an event in the resource's history.
- Implicit count: every usage record carries `count: 1`, so a resource priced by
  its existence alone, a floating IP for example, is priced through the `count`
  metric.

## Counters

Counter metrics come from the counter-sources file that
`TALLY_ENGINE_COUNTER_SOURCES` names. A source measures its metric in one of two
ways: an `events` source counts the events of one type inside the interval, and
a `metricsql` source runs a query against VictoriaMetrics over the interval. The
pass runs while the reporting snapshot is still open, so a counter and the
records it slices into see the same data.

A required source whose query fails fails the run, because billing data must not
silently omit a revenue-relevant counter. An optional source leaves the metric
out of that record and yields a warning instead (decision D7,
[`internal/engine/counters`](https://github.com/B42Labs/tally/blob/main/internal/engine/counters/counters.go)).
The dev overlay's
[counter-sources.yaml](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/overlays/dev/counter-sources.yaml)
is an example file.

## Rating

Rating is a pure calculation with no external queries. It loads the pricing
model version valid for the billing period, computes a cost per dimension,
aggregates per resource, per project and across related projects, and generates
the output. Each dimension names a key of the `usage` object, and the type of
the dimension picks the formula.

- Time and gauge:
  `cost = (usage.minutes / 60) × usage.<gauge_metric> × price_per_unit_hour × state_modifier × type_modifier`
- Counter: `cost = usage.<counter_metric> × price_per_unit`, which no modifier
  touches.

`state_modifier` and `type_modifier` both default to 1.0 and combine
multiplicatively on time-based costs. A resource type the pricing model does not
price is skipped and counted as a warning in the run's stats rather than billed
as free
([`internal/engine/rating`](https://github.com/B42Labs/tally/blob/main/internal/engine/rating/rating.go)).

One run chains metering, counters, rating, attribution, adjustments and
statement rendering over one reporting snapshot, and writes what came out in one
transaction, so a reader of the period never sees a run with half its records
([`internal/engine/runs`](https://github.com/B42Labs/tally/blob/main/internal/engine/runs/runs.go)).
The rounding every amount goes through is
[money and rounding](/explanation/money-and-rounding).

## Why a pricing model is versioned

A pricing model is versioned with validity periods. Rating a billing period uses
the version valid for that period, and every rated record references the version
it used. A price change is a new version, and a version is never edited, so
re-rating a past period yields the same numbers.

[pricing/2026-03.yaml](https://github.com/B42Labs/tally/blob/main/pricing/2026-03.yaml)
is the example model of the concept, and it is what the golden suite rates with.
The file format is documented in the reference quadrant.

[Worked examples](/explanation/worked-examples) carries the five metering output
examples of the concept, each with the golden case it seeded.

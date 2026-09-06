---
title: Events as the source of truth
description: Why the append-only event history is authoritative and everything else, the projection included, is derived from it.
quadrant: explanation
audience: all
---

# Events as the source of truth

One table decides what happened: `events`. Every other piece of state in the
reporting database, the inventory projection included, is folded out of it and
can be folded again. This page argues why that ordering is worth the cost, and
what follows from it for the projection, the schema and the intervals metering
reads.

## Why the history is append-only

The `events` table is append-only. A row is written once and never updated, and
events are retained without limit, because a billing record is legally relevant
long after the resource it describes is gone.

Append-only is what makes the history an argument rather than a claim. Any
number a statement carries can be traced back to the events it was folded from,
and folding the same events again produces the same number. Nothing in the
system is allowed to correct history in place; a correction is another event.

Two fields keep the history honest. `event_id` is the deduplication key, so a
collector that replays a batch after a timeout adds nothing. `source` says where
an event came from, `collector` for one a provider pushed and `reconciliation`
for a synthetic one the periodic sync wrote, so a resource that was repaired
looks different from one that was never missed while both end up with a usable
history.

An event that fails validation never reaches `events`. It is answered 400 and
written to `rejected_events`, the dead letter described in
[dual ingestion and reconciliation](/explanation/dual-ingestion-and-reconciliation).

## Why the primary key carries the timestamp

The primary key of `events` is `(event_id, timestamp)` rather than `event_id`
alone. TimescaleDB requires the partitioning column in every unique constraint,
and `timestamp` is what partitions the hypertable, so a key on `event_id` alone
cannot exist.

Deduplication still works, because a duplicate event carries the same `event_id`
and the same timestamp: the two rows collide on the composite key exactly as
they would on the single one. Reusing an `event_id` with a different timestamp
is a provider bug the API cannot detect cheaply, and providers must not do it.

## The projection

`current_resources` holds one row per `(cloud, resource_type, resource_id)`
saying what a resource is right now. The row is derived state. Every row in the
table can be thrown away and folded again from the events it came from, and this
is the operational guarantee behind reading it at all.

Two paths write a row. `Apply` folds a batch of just-ingested events onto the
row as it stands, at one read and one write per resource key. That works only
while no event of the batch is older than what the row already folded, because
an incremental fold cannot slot an event underneath the ones already applied. A
late event therefore sends `Apply` to `Replay`, which folds the resource's whole
history through `internal/core/timeline`. Both paths write the same row for the
same history, which is what takes the arrival order out of the result.

Both paths take a transaction-scoped advisory lock on the resource key before
they read anything, so two transactions touching the same resource fold one
after the other, and a rebuild waits for the ingest transaction holding the key
instead of writing over its result.

Rows are never deleted. A deleted resource keeps its row with state `deleted`
and `deleted_at` set, which is the index later phases scan for candidates. An
operator who wants the whole projection folded again asks for it through
`POST /internal/projection/rebuild`.

The metering engine derives its interval timelines from the event history and
not from the projection. The projection exists for fast inventory queries and
for diffing against a platform API during reconciliation, and nothing bills off
it. The implementation is
[`internal/reporting/projection`](https://github.com/B42Labs/tally/blob/main/internal/reporting/projection/projection.go).

## What an event carries

The envelope of an event's payload is normalised before it reaches the core. A
collector maps the provider's own shape onto `payload.state`, the state of the
resource at or after the event, and `payload.size`, the full replacement size
object. That mapping is the collector's job so that the projection, the timeline
and the metering stay free of provider-specific code (decision D1 of
[the Phase 1 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/01-phase-1-core-platform-openstack.md)).

The effect of an event is derived from its type and nothing else. A type
carrying the verb `create` creates, one carrying `delete` deletes, and anything
else updates (decision D2). A new event type from a new platform therefore
categorizes correctly without a line of code in the core.

A `size` is validated against the JSON Schema registered for its
`(platform, resource_type)` pair. A pair with no registered schema is accepted
with a warning, so a registry that lags behind a new resource type does not stop
collection; setting `TALLY_INGEST_REQUIRE_SIZE_SCHEMA` turns that into a
rejection for a production deployment (decision D9,
[`internal/reporting/registry`](https://github.com/B42Labs/tally/blob/main/internal/reporting/registry/registry.go)).

## What a size looks like

The `size` object holds the dimensions of a resource that pricing can charge
for, and its shape is specific to the resource type. Any change to `size`,
`state` or `project_id` closes the current interval and opens a new one, which
is what
[metering separated from rating](/explanation/metering-separated-from-rating)
calls a split.

The OpenStack rows below are what a collector produces today. The rows marked
"(Phase 4 design)" are designs: no collector produces those events, and they are
listed to show that the shape holds across platforms.

| Resource type | Platform | `size` example | Billable change events |
| --- | --- | --- | --- |
| `instance` | OpenStack | `{"vcpus": 4, "ram_gb": 8, "disk_gb": 80, "flavor": "m1.large"}` | resize (flavor change), shelve and unshelve, power on and off |
| `volume` | OpenStack | `{"size_gb": 100, "type": "ssd"}` | resize (size change), retype (SSD to HDD) |
| `floating_ip` | OpenStack | `{"ip_version": 4}` | create and delete only |
| `image` | OpenStack | `{"size_gb": 2.5}` | create and delete only |
| `loadbalancer` | OpenStack | `{"listeners": 2, "pools": 1}` | listener or pool added or removed |
| `server` | Hetzner (Phase 4 design) | `{"vcpus": 4, "ram_gb": 16, "disk_gb": 80, "server_type": "cx41"}` | upgrade and downgrade, power on and off |
| `server` | STACKIT (Phase 4 design) | `{"vcpus": 8, "ram_gb": 32, "disk_gb": 160, "machine_type": "c1.8"}` | resize, power on and off |
| `server` | IONOS (Phase 4 design) | `{"cores": 4, "ram_gb": 16, "type": "ENTERPRISE"}` | resize, power on and off |
| `shoot` | Gardener (Phase 4 design) | `{"worker_count": 3, "machine_type": "m1.xlarge", "kubernetes_version": "1.29"}` | worker scale, hibernate and wake |
| `repository` | Harbor (Phase 4 design) | `{"storage_gb": 12.5, "image_count": 47}` | push (storage grows), delete (storage shrinks) |

## The tables

The reporting database holds ten tables. `events` is the source of truth and
`rejected_events` its dead letter. `current_resources` is the projection folded
from `events`. `resource_types` holds the size schemas, `projects` and
`project_relations` the registry and the graph over it. `ingest_credentials` and
`api_tokens` are the two credential stores, `audit_log` records every write, and
`sync_runs` records each reconciliation run.

`events` is a TimescaleDB hypertable, partitioned by `timestamp` and compressed
by a policy on chunks older than 90 days, segmented by `cloud` and
`resource_type` so that a query for one installation reads its own segments. The
schema itself is a contract rather than an argument: it is
[migration 0001](https://github.com/B42Labs/tally/blob/main/migrations/reporting/0001_init.sql).

## Intervals

Every interval in the system is half-open, `[from, to)`. A billing period is a
calendar month in UTC, so March is
`[2026-03-01T00:00:00Z, 2026-04-01T00:00:00Z)`. UTC rather than a local zone,
because a daylight saving switch would otherwise hand the system 23-hour and
25-hour days to bill.

An event landing exactly on a boundary belongs to the next interval, never to
the one that ends there. That single rule is what keeps a resource from being
billed twice for the same instant, and it holds for interval boundaries inside a
month as much as for the month boundary itself. The normative text is section 5
of
[the roadmap conventions](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md).

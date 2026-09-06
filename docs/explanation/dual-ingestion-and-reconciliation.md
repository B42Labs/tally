---
title: Dual ingestion and reconciliation
description: Why events and a periodic sync are both needed, what the sync repairs, and the two gaps it accepts.
quadrant: explanation
audience: all
---

# Dual ingestion and reconciliation

Events are the fast path and they are lossy. A broker drops a notification, a
collector's disk fills, a network partition outlives a retry budget, and the
history is missing a fact nobody will notice. A periodic sync against the
platform's own API is the slow path that finds those gaps and closes them. The
two together are what makes the history trustworthy; neither is enough alone.

## Delivery semantics

Delivery from a collector to the Reporting API is at-least-once. A collector
retries with exponential backoff and buffers what it cannot deliver on disk, so
an unreachable API delays events instead of losing them. Nothing is dropped on
the client side, which is the only place a loss would be invisible.

At-least-once only works because ingestion is idempotent. The API deduplicates
on `event_id`, so replaying a batch is always safe and a collector never has to
reason about what the API already holds.

There is no ordering assumption either. Events may arrive in any order, and the
projection folds a late one by replaying the resource's history rather than by
refusing it (see
[events as the source of truth](/explanation/events-as-the-source-of-truth)).
A collector is therefore free to drain its buffer in whatever order it finds
convenient.

A batch holds at most 1000 events, and the response enumerates the outcome of
every item in it rather than answering with a single verdict. That is what lets
a collector delete its buffer on any 200: it can see which items were stored,
which were duplicates and which were rejected, without a second request
(decision D7 of
[the Phase 1 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/01-phase-1-core-platform-openstack.md)).

## Three roles, one service

`tally-reporting` plays three roles at once, and they share one database for a
reason: all three read or write the same event history.

It is the event sink that receives events from any provider's collector and
deduplicates them on `event_id`. It is the query interface that answers for the
event history of a resource, its lifecycle and the current inventory. It is the
reconciliation loop that periodically syncs against a platform API through a
provider-specific adapter, to catch the events that never arrived and to correct
the drift they left behind.

## Dead letter

An incoming event is validated against the event schema, and its `payload.size`
against the JSON Schema registered for its resource type. An event that fails
either check is answered 400 and stored in `rejected_events`.

Rejecting alone would be enough for correctness, but not for operations. The
dead letter is what makes schema drift on the provider side visible: a
collector that starts emitting a field of the wrong type produces a growing pile
of rows with a reason attached, instead of quietly corrupting the numbers a
statement is built from. Someone can look at the rows and see what changed.

## Reconciliation

Reconciliation polls a platform's own API through a provider-specific adapter,
lists what the platform currently holds and diffs that against
`current_resources`. The difference is fed back as ordinary events, taking the
same deduplication, storage and projection route a collector's events take, so a
resource the sync corrected ends up with the same kind of history as one that
was never missed.

| Situation | Action |
| --- | --- |
| The platform API holds a resource the projection does not | Insert it into `current_resources`, write a synthetic create event |
| The projection holds a resource the platform API does not | Mark it deleted, write a synthetic delete event |
| The attributes differ, a flavor after a resize for example | Update `current_resources`, write a synthetic update event |

Synthetic events carry the types `sync.create`, `sync.update` and `sync.delete`
(decision D8), which categorize correctly under the create and delete verb rule
and are trivially identifiable in a history. Each carries
`source: "reconciliation"` and a deterministic `event_id` derived from the sync
run and the resource, so re-running a sync never duplicates them.

A sync is started per cloud through `POST /internal/sync/{cloud}` and recorded
in `sync_runs`. The repository ships no CronJob for it: what drives the schedule
belongs to the deployment. The concept suggested short intervals, and the two
limitations below say why.

A platform API that stops answering must read as "no information" and never as
"everything was deleted". An adapter that cannot enumerate a resource type says
so and carries on with its remaining types, and the missed-delete pass then runs
only over the types that finished. A type that legitimately holds nothing is
still a type that finished, so the pass works from the positive set of types
that enumerated rather than from the set of failures
([`internal/reporting/reconciliation`](https://github.com/B42Labs/tally/blob/main/internal/reporting/reconciliation/reconciliation.go)).

Where a platform can tell the sync when something was deleted, the adapter asks.
The OpenStack adapter lists nova's deleted servers for the window a run is
responsible for, so a synthetic delete for an instance carries the real deletion
time rather than the time the sync happened to look.

## Known limitations

Two gaps are accepted rather than closed. Both are monitored.

A synthetic delete event carries the poll time and not the actual deletion time.
Each lost delete event therefore costs up to one sync interval of overbilling.
The exception is a platform API that exposes real timestamps, as nova does
through `GET /servers?deleted=true`, where the adapter uses the reported
`deleted_at` instead of the poll time.

A resource created and deleted between two sync runs, whose events were both
lost, is invisible to the system: neither run ever saw it, so no history says it
existed. Nothing can recover it after the fact. Keeping sync intervals short
bounds the window in which this can happen, and a cloud whose event ingestion
rate drops to zero is alerted on, because a collector outage is what opens the
window wide enough to matter.

`TallyCloudEventsSilent` is that alert. It fires when a cloud that produced
collector events in the last 24 hours has produced none for an hour, and the
runbook for `TallyCloudEventsSilent` says what to do about it. It sits beside
`TallySyncErrors`, `TallySyncStale` and `TallyReconciliationDriftHigh` in
[the alerting rules](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/vmalert/rules.yaml).

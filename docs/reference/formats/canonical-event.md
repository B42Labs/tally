---
title: Canonical event
description: The wire shape of a Tally event, its payload envelope, the bounds and rules ingestion applies, and what an ingest call answers.
quadrant: reference
audience: integrator
---

# Canonical event

Every event reaches Tally through `POST /api/v1/events` in the shape below,
whether a collector pushed it or a reconciliation run produced it. The
normative text is section 4 of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md),
and the types here are rendered from
[`internal/core/event/event.go`](https://github.com/B42Labs/tally/blob/main/internal/core/event/event.go),
which implements it.

## Members

An event carries ten members. `payload` is the normalized envelope every
platform is mapped into, so the projection, the timeline and the metering pass
read one shape whatever produced it. A member the envelope does not name
survives a decode and an encode untouched, which keeps an event byte-faithful
to what the provider sent.

### The event

<!-- refdoc:begin event -->
#### `Event`

Event is an immutable lifecycle fact. The events table is the system's single source of truth; everything else is derived from it and rebuildable.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `event_id` | string | always |  |
| `timestamp` | string, RFC 3339 UTC | always |  |
| `event_type` | string | always |  |
| `platform` | string | always |  |
| `cloud` | string | always |  |
| `resource_type` | string | always |  |
| `resource_id` | string | always |  |
| `project_id` | string | always |  |
| `source` | `Source` | always |  |
| `payload` | [PayloadEnvelope](#payloadenvelope) | always |  |

#### `PayloadEnvelope`

PayloadEnvelope is the normalized payload every event carries. Collectors map provider data into it so that projection, timeline, and metering never need provider-specific code. Unknown fields survive a decode/encode round-trip untouched, which keeps an event byte-faithful to what the provider sent even as this struct grows.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `state` | string or null | omitted when empty | State is the resource state at or after the event. It is required on every event except a delete, where the core sets "deleted" itself. |
| `size` | object | omitted when empty | Size is the full replacement size object, required on create and on any size-changing event. Absent means the size did not change. |
| `provider` | object | omitted when empty | Provider is free-form raw provider data, kept for debugging and audit. Core logic never reads it. |
<!-- refdoc:end event -->

## Bounds

`Validate` reports every rule an event breaks at once rather than the first one.
The four lengths it holds an event to are declared in `event.go`.

<!-- refdoc:begin bounds -->
| Name | Value | Meaning |
| --- | --- | --- |
| `eventIDMaxLen` | `256` | eventIDMaxLen bounds event_id so it fits the database column and stays a usable idempotency key. |
| `eventTypeMaxLen` | `512` | eventTypeMaxLen bounds event_type for the same reason as identifierMaxLen below: idx_events_type indexes it next to timestamp. The pattern alone lets a value of any length through, and one past the btree limit fails the insert rather than the event, which would take down whatever batch it travelled in. |
| `identifierMaxLen` | `512` | identifierMaxLen bounds the fields that identify a resource. They are indexed columns, and a value past the btree limit fails the insert rather than the event, which would take down whatever batch the event travelled in. |
| `stateMaxLen` | `512` | stateMaxLen bounds payload.state for the same reason. The projection writes it to current_resources.state, which idx_current_resources_type indexes next to resource_type and idx_current_resources_stats next to platform, cloud, resource_type and project_id. Those four are bounded above, so a state within this bound keeps the widest of the tuples under the btree limit. |
<!-- refdoc:end bounds -->

`identifierMaxLen` bounds `platform`, `cloud`, `resource_type`, `resource_id`
and `project_id`. All five are required, and an empty one is refused.

`event_type` has to match the regular expression `^[a-z0-9_]+(\.[a-z0-9_]+)+$`,
which `event.go` declares as `eventTypePattern`: two or more parts of lower-case
letters, digits and underscores, separated by dots. The length is checked before
the pattern, so an over-long type is refused for its length and the refusal does
not quote the value back.

## Sources and categories

`source` names the pipeline an event came from. The category is the effect the
event has on a resource. Both sets of values are declared in `event.go`.

<!-- refdoc:begin sources -->
| Name | Value | Meaning |
| --- | --- | --- |
| `SourceCollector` | `collector` | SourceCollector marks an event pushed by a provider-side collector. It is the default: an event that names no source is treated as a collector event. |
| `SourceReconciliation` | `reconciliation` | SourceReconciliation marks a synthetic event emitted by a server-side sync run to correct drift between a platform and the projection. |
| `CategoryCreate` | `create` | CategoryCreate starts a resource's life: it sets created_at and requires a full payload (state and size). |
| `CategoryUpdate` | `update` | CategoryUpdate changes a resource's state, size, or owner. |
| `CategoryDelete` | `delete` | CategoryDelete ends a resource's life: it sets deleted_at and forces the state to "deleted". |
<!-- refdoc:end sources -->

`Categorize` derives the category from `event_type` alone, which is what keeps
the core free of per-platform code. It splits the type on dots: a part `create`
makes the event a create, a part `delete` makes it a delete, and every other
type is an update. Section 4.2 of the conventions states the same rule.

A create sets `created_at` to the event's timestamp and requires `payload.size`
beside `payload.state`. A delete sets `deleted_at` and forces the state to
`deleted`. An update applies `payload.state`, `payload.size` and the top-level
`project_id` as the event carries them.

The synthetic events a reconciliation run emits are typed `sync.create`,
`sync.update` and `sync.delete`, which categorize under the same rule.

## Rules

- A duplicate is the same `(event_id, timestamp)` pair. Ingestion is idempotent,
  so replaying a batch stores nothing twice. Reusing an `event_id` with another
  timestamp is a provider bug the API does not detect: the primary key holds the
  hypertable partition column.
- `event_id` is the provider's own event or action id where one exists, such as
  the oslo `message_id`. Where none exists it is the deterministic hash
  `sha256("{cloud}:{resource_id}:{event_type}:{timestamp_iso}")`, hex and
  prefixed with the platform name.
  `internal/core/ids.DeterministicEventID` is the single implementation of it,
  and the timestamp in it is rendered RFC 3339 UTC.
- `platform` and `cloud` are neither `meta` nor `partner`. The two literals name
  the virtual projects, which own no resources and carry their platform as their
  cloud, so an item holding either literal in either field is refused.
- `payload.state` is the resource state at or after the event. It is required on
  every event except a delete, where the core sets `deleted` itself.
- `payload.size` is the full replacement size object. It is required on a create
  and on any event that changes the size; absent means the size did not change.
- `source` may be left out, and it defaults to `collector`. A stored event
  reports the pipeline that ingested it rather than the value the item claimed,
  so an item submitted to `POST /api/v1/events` is stored as `collector`
  whatever it says. A value outside `collector` and `reconciliation` refuses the
  item.
- `project_id` is the project owning the resource at or after the event. On an
  ownership transfer the event names the new owner, and the previous one stays
  implicit in the earlier history.

## Size schemas

`payload.size` is validated against the JSON Schema the resource type registry
holds for the pair `(platform, resource_type)`, whenever the payload carries a
size. Five pairs ride the migration chain, so every database the chain reaches
knows them without an operator registering anything: four in
[`migrations/reporting/0002_seed_resource_types.sql`](https://github.com/B42Labs/tally/blob/main/migrations/reporting/0002_seed_resource_types.sql)
and the load balancer in
[`migrations/reporting/0006_seed_loadbalancer_type.sql`](https://github.com/B42Labs/tally/blob/main/migrations/reporting/0006_seed_loadbalancer_type.sql).

| Resource type | Required members |
| --- | --- |
| `instance` | `vcpus`, `ram_gb`, `disk_gb`, `flavor` |
| `volume` | `size_gb`, `type` |
| `floating_ip` | `ip_version` |
| `image` | `size_gb` |
| `loadbalancer` | `listeners`, `pools` |

All five are registered under the platform `openstack`, and all five admit
further members, so a size that carries more than the schema names is accepted.

The registry is listed, read and written through the resource type operations of
the [Reporting API](/reference/api/reporting-api). A size for a pair no row
registers is accepted unvalidated by default and counted; setting
`TALLY_INGEST_REQUIRE_SIZE_SCHEMA` to `true` refuses it instead. The
[Reporting API settings](/reference/configuration/tally-reporting) page states
the variable.

## Delivery

The body of `POST /api/v1/events` is one event or an array of at most 1000 of
them. A longer array is answered 413 and none of it is stored.

The answer is 200 whenever the request itself was authorized and readable,
whatever the individual items did. It carries `accepted`, `duplicates` and
`rejected` as
[`IngestResult`](/reference/api/reporting-api-schemas#ingestresult) declares
them. A batch lands whole or not at all, and a batch of nothing but refused
items still commits.

An item this API refuses is kept server-side with the reason it was refused and
named in `rejected` with its index and its event id. A collector may drop the
whole batch from its buffer once the call returns, and must not retry a refused
item. What was kept is read back through `GET /api/v1/rejected-events`.

An item outside the credential's scope is the exception. It leaves an audit row
of action `events.scope_violation` and no dead-letter row, so it is found in the
audit log rather than in that view.

`tally_ingest_unvalidated_size_total` counts the events whose size was taken
unvalidated, labelled by `platform` and `resource_type`.

## See also

The [Reporting API endpoints](/reference/api/reporting-api) page states the
route, its credential and its errors. The
[label convention](/reference/formats/label-convention) page states the
vocabulary the members above are named in.

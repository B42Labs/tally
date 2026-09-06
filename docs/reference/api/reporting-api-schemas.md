---
title: Reporting API schemas
description: Every component schema of the Reporting API, property by property, rendered from the OpenAPI document.
quadrant: reference
audience: integrator
---

# Reporting API schemas

These are the component schemas of
[`api/reporting/openapi.yaml`](https://github.com/B42Labs/tally/blob/main/api/reporting/openapi.yaml),
the document [Reporting API endpoints](/reference/api/reporting-api) is
rendered from. An operation there names the schema of each body it reads and
each body it answers with, and links it here.

## Schemas

<!-- refdoc:begin schemas -->
### `CreateProject`

The project to register. Its `(cloud, external_id)` pair has to be one the registry does not hold yet.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `cloud` | string | yes | The installation the project lives in, os-prod-eu1 for example. A virtual project carries its platform here, and no real project carries `meta` or `partner`. |
| `external_id` | string | yes | The project as its cloud names it. |
| `metadata` | object | no | Whatever else is worth keeping about the project. A registration that leaves it out stores the empty object. The stored document is bounded at 65536 bytes as the database normalizes it: a number a body spells as an exponent is stored, and answered, spelled out. A write whose document is past the bound is answered 422 and stores nothing. |
| `name` | string | no | What the project is called. |
| `platform` | string | yes | The platform the project lives on, openstack for example. `meta` and `partner` are reserved for the two kinds of virtual project that own no resources: the meta-project and the partner. |

### `CreateRelation`

The relation to create. It leaves the project the path names, so only the other end is given here.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `metadata` | object | no | Whatever else is worth keeping about the relation. A creation that leaves it out stores the empty object. The member `pricing_adjustments` is accepted as a non-empty array of at most 64 objects with `type` (one of `discount`, `kickback`, `surcharge`, `project_discount`), `rate` (a decimal string from `0` to `1` with at most six fractional digits, never a number), `scope` (`all`, a platform, or `platform.resource_type`, in lower-case letters, digits and underscores) and an optional `description` of at most 500 characters, and no other members. The rule enforced is the schema `internal/core/adjustment/adjustments_schema.json`, which the rating engine reads the array by. An array the schema refuses is answered 422 with one field error per violation, located as `body.metadata.pricing_adjustments.<index>.<member>`. The stored document is bounded at 65536 bytes as the database normalizes it: a number a body spells as an exponent is stored, and answered, spelled out. A write whose document is past the bound is answered 422 and stores nothing. |
| `relation_type` | string | yes | What the relation means. `infrastructure_tenant` attributes the cost of the target to the source and is the default attributing type. `member_of` groups the source under a meta-project (platform `meta`), and `managed_by` places the source under a partner (platform `partner`); neither of the two ever attributes cost. The field is a free string, so any other type is stored as it arrives. |
| `target_id` | string, `uuid`, `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$` | yes | The project the relation reaches. It has to be registered, and it cannot be the project the relation leaves. |
| `valid_from` | string, `date-time` | no | When the relation starts being valid. It defaults to the instant the relation is written. |

### `DeadLetterList`

One page of dead-lettered events.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `items` | array of [DeadLetteredEvent](#deadletteredevent) | yes | The refused items of this page, ordered by received_at and id. |
| `next_cursor` | string or null | yes | Where the next page starts, passed back as `cursor`. It is null on the last page. |

### `DeadLetteredEvent`

One ingest item this API refused, as the dead-letter table holds it.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `id` | string, `uuid` | yes | The dead-letter row. A refused item has no event id this API can trust, so the row's own id is what names it. |
| `raw` | any | yes | The item as it was submitted, with NUL characters scrubbed at ingest. Nothing is constrained here: what a collector sent need not be a JSON object, and this member carries whatever it was. |
| `reason` | string | yes | Why ingestion refused the item, a schema violation for example, prefixed the way the ingest response reports it. |
| `received_at` | string, `date-time` | yes | When this API refused the item. |

### `EventInput`

One canonical event. The members below are described rather than constrained; the normative schema is roadmap/00-conventions.md section 4.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `cloud` | any | no | The installation the resource lives in, os-prod-eu1 for example. `meta` and `partner` are the clouds of the virtual projects, so an item carrying either refuses the item as well. |
| `event_id` | any | no | Globally unique idempotency key of 1 to 256 characters. An event id resubmitted with the same timestamp counts as a duplicate. |
| `event_type` | any | no | What happened, written resource.action with an optional phase, for example compute.instance.create.end. |
| `payload` | any | no | The normalized payload envelope: the state at or after the event, the full replacement size when it changed, and optional raw provider data. |
| `platform` | any | no | The platform the resource lives on, openstack for example. `meta` and `partner` name the two kinds of virtual project, which own no resources; an item carrying either refuses the item. |
| `project_id` | any | no | The project owning the resource at or after this event. |
| `resource_id` | any | no | The resource this event is about. |
| `resource_type` | any | no | The kind of resource, instance or volume for example. |
| `source` | any | no | Where the event came from. The stored event always reports the pipeline that ingested it, so a collector cannot mark its events as reconciliation output. The member may be left out; a value outside `collector` and `reconciliation` refuses the item. |
| `timestamp` | any | no | When the event happened, ISO 8601 with a timezone. |

### `EventList`

One page of stored events.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `items` | array of [StoredEvent](#storedevent) | yes | The events of this page, ordered by timestamp and event id. |
| `next_cursor` | string or null | yes | Where the next page starts, passed back as `cursor`. It is null on the last page. |

### `EventStatsItem`

One group of the event counts: the bucket it falls in, the values its dimensions carry, and how many events carry all of them.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `bucket` | string, `date-time` | yes | When the bucket starts, in UTC. A bucket covers `interval` from there on, the start inclusive and the end exclusive. |
| `cloud` | string | yes | The installation the counted events came from. |
| `count` | integer, `int64` | yes | How many events the group holds. |
| `event_type` | string | yes | The type the counted events carry. |
| `source` | string | no | Which pipeline produced the counted events, collector or reconciliation. The member is there exactly when `group_by` names `source`. |

### `EventStatsList`

The event counts of one window.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `items` | array of [EventStatsItem](#eventstatsitem) | yes | The groups, ordered by bucket, cloud, event type, and source. It is the empty array when the window holds no event. |

### `IngestResult`

What one ingest call did with the batch it was given.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `accepted` | integer | yes | How many events the call stored. |
| `duplicates` | integer | yes | How many items the database already held. |
| `rejected` | array of [RejectedEvent](#rejectedevent) | yes | The items the call refused, one entry each. |

### `Lifecycle`

One resource, its event history, and the billable intervals that history folds into. `resource` is always there: a resource this API holds no projection row for is answered 404 rather than with an empty lifecycle.

The two halves are derived from two different event sets. `events` and `intervals` carry the history this token may read, while `resource` is the projection row, folded from every event the resource has under no scope. On a resource that changed projects the row therefore reports a `created_at` from before the reader owned it, and the first interval starts at the transfer.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `events` | array of [StoredEvent](#storedevent) | yes | The full history the fold ran on, ordered by `(timestamp, received_at, event_id)`. |
| `intervals` | array of [LifecycleInterval](#lifecycleinterval) | yes | The billable intervals the history implies, oldest first. Two billable changes at the same instant leave no interval between them, so a resource created and deleted at one instant has none at all. |
| `resource` | [Resource](#resource) | yes | One resource as the projection holds it: what its event history says it is right now. |
| `warnings` | array of string | yes | What the fold could not trust, one entry each. A history whose first event is not a create carries history_starts_without_create. |

### `LifecycleInterval`

One half-open span `[from, to)` over which nothing billable about a resource changed.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `from` | string, `date-time` | yes | When the interval starts, the inclusive bound. |
| `project_id` | string | yes | The project owning the resource over the interval. |
| `size` | object | yes | The size the resource had over the interval. |
| `state` | string | yes | The state the resource was in over the interval. |
| `to` | string or null, `date-time` | yes | When the interval ends, the exclusive bound. It is null while the interval is open, which the last interval of a living resource is. |

### `Problem`

RFC 9457 problem detail. Every error response in this API uses this shape, served as application/problem+json.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `detail` | string | no | Human-readable explanation specific to this occurrence. |
| `errors` | array of object | no | Per-field details, set when the problem is a validation failure. |
| `status` | integer | yes | The HTTP status code of this response. |
| `title` | string | yes | Short human-readable summary of the problem type. |
| `type` | string | yes | URI reference identifying the problem type. |

### `Project`

One registered project. The registry is keyed by `(cloud, external_id)`, and `id` is what the other operations address the project by.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `cloud` | string | yes | The installation the project lives in, os-prod-eu1 for example. A virtual project carries its platform here, and no real project carries `meta` or `partner`. |
| `created_at` | string, `date-time` | yes | When the project was registered. |
| `external_id` | string | yes | The project as its cloud names it, which is the id an event carries. |
| `id` | string, `uuid` | yes | The project, as this API names it. |
| `metadata` | object | yes | Whatever else was stored about the project. It is the empty object for a project registered without any. |
| `name` | string or null | yes | What the project is called, null for a project registered without a name. |
| `platform` | string | yes | The platform the project lives on, openstack for example. `meta` and `partner` are reserved for the two kinds of virtual project that own no resources: the meta-project and the partner. |

### `ProjectActivity`

What one resource type of a project did inside the window of a summary.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `active_now` | integer | yes | How many resources of the type the project holds right now, counted off the projection rows that are not deleted. It describes the present rather than the window, so a type the window saw nothing of still reports what the project runs of it today. |
| `created` | integer | yes | How many resources of the type began their life inside the window. |
| `deleted` | integer | yes | How many resources of the type ended their life inside the window. |
| `resource_type` | string | yes | The kind of resource this row is about. |
| `total_minutes` | integer, `int64` | yes | How long the resources of the type ran inside the window, in whole minutes, truncated. An interval still open is counted up to the instant of the request, so a window reaching into the future carries no time that has not been served yet. |

### `ProjectList`

One page of registered projects.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `items` | array of [Project](#project) | yes | The projects of this page, ordered by cloud and external id. |
| `next_cursor` | string or null | yes | Where the next page starts, passed back as `cursor`. It is null on the last page. |

### `ProjectRef`

Which project an answer is about, as much of it as a project-scoped read carries. The registry row itself — the name, the operator-set metadata, the platform, and when it was registered — is what `getProject` serves, and reading that takes `read_all`.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `cloud` | string | yes | The installation the project lives in, os-prod-eu1 for example. |
| `external_id` | string | yes | The project as its cloud names it, which is the id an event carries. |
| `id` | string, `uuid` | yes | The project, as this API names it. |

### `ProjectSummary`

One project and what its resource types did inside one window.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `project` | [ProjectRef](#projectref) | yes | Which project an answer is about, as much of it as a project-scoped read carries. The registry row itself — the name, the operator-set metadata, the platform, and when it was registered — is what `getProject` serves, and reading that takes `read_all`. |
| `resource_types` | array of [ProjectActivity](#projectactivity) | yes | One row per resource type the project has events or resources of, ordered by resource type. It is the empty array for a project with neither. |

### `RebuildRequest`

Which resources to replay. A member left out filters nothing, so an empty object rebuilds every resource the events table knows.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `cloud` | string | no | Replay only the resources of this cloud. |
| `resource_type` | string | no | Replay only the resources of this type. |

### `RebuildResult`

What one rebuild replayed.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `rebuilt` | integer | yes | How many resources were replayed. |

### `RegisterResourceType`

The size schema to register for a resource type.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `size_schema` | object | yes | A JSON Schema draft 2020-12 document. It is compiled before it is stored, so the document a registration accepts is exactly the one ingestion can apply later. |

### `RejectedEvent`

One refused item and the reason it was refused.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `event_id` | string | yes | The event id the item carried. It is the empty string for an item that carried none, which is why the index is what identifies the item. |
| `index` | integer | yes | The item's position in the submitted batch, counted from zero. |
| `reason` | string | yes | Why the item was refused, for example "size_schema: 'vcpus' is a required property" or 'schema: platform: "meta" is a virtual platform, which never carries resources'. |

### `RelatedProject`

One project a traversal reached.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `depth` | integer | yes | How many relations lie between the project the walk started from and this one. |
| `path` | array of string, `uuid` | yes | The relation ids from the start to this project in walk order, so it holds `depth` of them. |
| `project` | [Project](#project) | yes | One registered project. The registry is keyed by `(cloud, external_id)`, and `id` is what the other operations address the project by. |
| `relation_type` | string | yes | The type of the relation the walk arrived on. |

### `RelatedProjectList`

The projects one traversal reached.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `items` | array of [RelatedProject](#relatedproject) | yes | The projects the walk reached, in the order it visited them. |

### `Relation`

One relation between two projects. It is valid at `t` iff `valid_from <= t AND (valid_to IS NULL OR valid_to > t)`, and it is closed rather than deleted, so a read at an earlier instant still finds it.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `created_at` | string, `date-time` | yes | When the relation was written. |
| `id` | string, `uuid` | yes | The relation, as this API names it. |
| `metadata` | object | yes | Whatever else was stored about the relation. It is the empty object for a relation created without any. The member `pricing_adjustments`, when present, holds a non-empty array of at most 64 objects with `type` (one of `discount`, `kickback`, `surcharge`, `project_discount`), `rate` (a decimal string from `0` to `1` with at most six fractional digits, never a number), `scope` (`all`, a platform, or `platform.resource_type`, in lower-case letters, digits and underscores) and an optional `description` of at most 500 characters, and no other members. The schema `internal/core/adjustment/adjustments_schema.json` decided the array at the write, and the rating engine reads it by that same schema. |
| `relation_type` | string | yes | What the relation means. `infrastructure_tenant` attributes the cost of the target to the source and is the default attributing type. `member_of` groups the source under a meta-project (platform `meta`), and `managed_by` places the source under a partner (platform `partner`); neither of the two ever attributes cost. The field is a free string, so any other type is stored as it arrives. |
| `source_id` | string, `uuid` | yes | The project the relation leaves. |
| `target_id` | string, `uuid` | yes | The project the relation reaches. |
| `valid_from` | string, `date-time` | yes | When the relation starts being valid, the inclusive bound. |
| `valid_to` | string or null, `date-time` | yes | When the relation stops being valid, the exclusive bound. It is null while the relation is open. |

### `RelationList`

The relations of one project at one instant.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `items` | array of [Relation](#relation) | yes | The relations valid at that instant, ordered by created_at and id. |

### `Resource`

One resource as the projection holds it: what its event history says it is right now.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `cloud` | string | yes | The installation the resource lives in. |
| `created_at` | string or null, `date-time` | yes | When the resource was created, as the projection folded it from the resource's whole history. It is null for a history that never showed a create. The projection is not scoped to the reading token, so on a resource that changed projects this is the create of the project it came from and predates the reader's ownership: what a project is billed for is the lifecycle's `intervals`, never `created_at` paired with `deleted_at`. |
| `deleted_at` | string or null, `date-time` | yes | When the resource was deleted, null while it lives. |
| `last_event_at` | string, `date-time` | yes | When that event happened. |
| `last_event_type` | string | yes | The type of the newest event folded into this row. It is a `sync.create`, `sync.update`, or `sync.delete` when the newest thing that happened to the resource was a correction by the reconciliation framework rather than a report by a collector. |
| `last_payload` | object or null | yes | The payload envelope of that event, member for member as it was stored, and null for a row that holds none. |
| `platform` | string | yes | The platform the resource lives on. |
| `project_id` | string | yes | The project owning the resource now. |
| `resource_id` | string | yes | The resource itself, as its cloud names it. |
| `resource_type` | string | yes | The kind of resource. |
| `size` | object | yes | The size the resource has now, member for member as its events reported it. It is the empty object for a history that reported no size. |
| `state` | string | yes | The state the resource is in now. A deleted resource carries deleted, which the server sets itself rather than reading it off the delete event. |

### `ResourceList`

One page of current resources.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `items` | array of [Resource](#resource) | yes | The resources of this page, ordered by cloud, resource type, and resource id. |
| `next_cursor` | string or null | yes | Where the next page starts, passed back as `cursor`. It is null on the last page. |

### `ResourceStatsItem`

One group of the resource counts: the values its dimensions carry, and how many resources carry all of them.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `cloud` | string | yes | The installation the counted resources live in. |
| `count` | integer, `int64` | yes | How many resources the group holds. |
| `platform` | string | no | The platform the counted resources live on. The member is there exactly when `group_by` names `platform`. |
| `project_id` | string | no | The project owning the counted resources, named the way its cloud names it. The member is there exactly when `group_by` names `project_id`. |
| `resource_type` | string | yes | The kind of resource that was counted. |
| `state` | string | no | The state the counted resources are in. The member is there exactly when `group_by` names `state`. |

### `ResourceStatsList`

The resource counts of one grouping.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `items` | array of [ResourceStatsItem](#resourcestatsitem) | yes | The groups, ordered by cloud, resource type, state, platform, and project id. It is the empty array when the grouping counts no resource at all. |

### `ResourceType`

One registered resource type and the size schema the sizes reported for it are validated against.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `platform` | string | yes | The platform the resource type belongs to. |
| `resource_type` | string | yes | The resource type within that platform. |
| `size_schema` | object | yes | The registered JSON Schema draft 2020-12 document. |
| `updated_at` | string, `date-time` | yes | When the schema was last written. |

### `ResourceTypeList`

The registered resource types.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `items` | array of [ResourceType](#resourcetype) | yes | The resource types, ordered by platform and resource type. |
| `next_cursor` | string or null | yes | Where the next page starts. It is always null today: the registry holds one row per resource type, so every registration fits into one answer. The registry list stays a single page, while the query lists page with cursors. |

### `StoredEvent`

One event as the events table holds it: what was submitted, plus the instant this API stored it.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `cloud` | string | yes | The installation the resource lives in. |
| `event_id` | string | yes | The idempotency key the event was submitted under. |
| `event_type` | string | yes | What happened, written resource.action with an optional phase, for example compute.instance.create.end. The reconciliation framework writes the second family, `sync.create`, `sync.update`, and `sync.delete`: a correction is about no one service, so it names none. `source` is the member to branch on rather than the prefix — it is `reconciliation` for exactly those events. |
| `payload` | object or null | yes | The normalized payload envelope, member for member as it was stored. It is null for an event stored without one, which a delete event may be. The members are not constrained here: an envelope keeps the fields a provider sent beyond the ones this API names. |
| `platform` | string | yes | The platform the resource lives on. |
| `project_id` | string | yes | The project owning the resource at or after this event. |
| `received_at` | string, `date-time` | yes | When this API stored the event. |
| `resource_id` | string | yes | The resource the event is about. |
| `resource_type` | string | yes | The kind of resource the event is about. |
| `source` | string | yes | Which pipeline produced the event, collector or reconciliation. It is what the server recorded rather than what the submitter claimed. |
| `timestamp` | string, `date-time` | yes | When the event happened. |

### `SyncRequest`

When the run happens. The member is optional, and a request that leaves it out, like one that carries no body at all, syncs at wall time.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `at` | string, `date-time` | no | Run the sync at this instant instead of at wall time: the run's `sync_runs` row starts here, and every correction the run books carries this timestamp. The deployment has to set `TALLY_REPORTING_SYNC_ALLOW_AT` for the member to be accepted. It exists for a development deployment reconciling a simulated cloud, whose clock is not the wall clock. |

### `SyncResult`

What one sync run did.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `stats` | [SyncStats](#syncstats) | yes | The tally of one sync run, which is one that finished clean. |
| `sync_run_id` | string | yes | The id of the `sync_runs` row this run wrote. It is what an operator reads the run back by, and what the synthetic event ids of the run are derived from. |

### `SyncStats`

The tally of one sync run, which is one that finished clean.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `created` | integer | yes | How many resources the run reported to the projection as new, one synthetic event each. |
| `deleted` | integer | yes | How many it reported as gone. |
| `updated` | integer | yes | How many it corrected the state, size, or owner of. |

### `UpdateProject`

What to change about a project. A member the request leaves out stays as it is, so at least one of them has to be present.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `metadata` | object | no | The metadata the project carries from now on. It replaces the stored object wholesale rather than being merged into it, so a request carrying it carries every member the project keeps. The stored document is bounded at 65536 bytes as the database normalizes it: a number a body spells as an exponent is stored, and answered, spelled out. A write whose document is past the bound is answered 422 and stores nothing. |
| `name` | string | no | What the project is called from now on. |

### `UpdateRelation`

What to change about a relation. A member the request leaves out stays as it is, so at least one of them has to be present.

| Property | Type | Required | Description |
| --- | --- | --- | --- |
| `metadata` | object | no | The metadata the relation carries from now on. It replaces the stored object wholesale rather than being merged into it, so a request carrying it carries every member the relation keeps. The `pricing_adjustments` of a relation are fixed for its lifetime. A document whose member differs from the stored one, by being added, dropped, reordered or changed in any value, is answered 409. To change the other members, send the member back exactly as the relation answers it; to change the adjustments, close the relation and create a successor. The stored document is bounded at 65536 bytes as the database normalizes it: a number a body spells as an exponent is stored, and answered, spelled out. A write whose document is past the bound is answered 422 and stores nothing. |
| `valid_to` | string, `date-time` | no | When the relation stops being valid. It has to be after `valid_from`. The member is not nullable: an open relation is closed here and a closed one gets its close instant corrected, while reopening a closed relation is not supported. |

### `Uuid`

A string, format `uuid`, matching `^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`.
<!-- refdoc:end schemas -->

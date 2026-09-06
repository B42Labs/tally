---
title: Reporting API endpoints
description: Every route of the Reporting API with its parameters, bodies, responses, credential and error format, rendered from the OpenAPI document.
quadrant: reference
audience: integrator
---

# Reporting API endpoints

The Reporting API is specified by
[`api/reporting/openapi.yaml`](https://github.com/B42Labs/tally/blob/main/api/reporting/openapi.yaml).
`make generate` produces the server's routing and its models from that
document, and the service validates every request against it before a handler
sees it. The tables and sections below are rendered from the same document.

The document is OpenAPI 3.0.3 and carries no `servers` block, so a client sets
the base URL of the deployment it talks to.

## Conventions

The four conventions below are fixed by section 7 of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md).
Each paragraph states what the document declares for one of them.

Every timestamp the document declares is a `string` with `format: date-time`,
in a query parameter as in a body. Section 7 asks for ISO 8601 in UTC, in and
out.

Four lists page: `GET /api/v1/events`, `GET /api/v1/resources`,
`GET /api/v1/projects` and `GET /api/v1/rejected-events`. Each takes `limit`,
whose bounds and default stand on the row of the operation below, and `cursor`,
an opaque string a caller passes back as it received it. A page is answered as
`{"items": [...], "next_cursor": ...}`, where `next_cursor` is a string or
null and null is the last page. `GET /api/v1/resource-types` answers those two
members and takes neither parameter: its `next_cursor` is always null.

Every response of every operation declares the header `X-Request-ID`. The
server adopts the id a caller sends and generates one otherwise. The Headers
column of an operation lists what a status carries beyond it.

Every error status of every operation references one shared response, which
carries a problem document as `application/problem+json`.

## Authentication

<!-- refdoc:begin security -->
| Scheme | Type | Description |
| --- | --- | --- |
| `apiToken` | `http bearer` | An API token passed as `Authorization: Bearer <token>`. Each operation names the role it needs; a token below it is answered 403. |
| `ingestToken` | `http bearer` | An ingest credential, issued per (platform, cloud) and passed as `Authorization: Bearer <token>`. It may report events for that pair alone. |
| `internalToken` | `http bearer` | The shared secret the other Tally components call the internal routes with, passed as `Authorization: Bearer <token>`. It is not issued to public callers. |
<!-- refdoc:end security -->

Each operation below names the scheme it takes. `GET /healthz`, `GET /readyz`
and `GET /metrics` take none. A token is issued under one scheme and refused by
the operations of the other two.

An API token carries a role and a project scope. A request above the token's
role, and a request for a project outside its scope, are both answered 403. An
operation that needs more than the lowest role says so in its description.

## Errors

Every error is one RFC 9457 problem document. It always carries `type`,
`title` and `status`; it carries `detail` where there is more to say, and
`errors`, a list of `loc` and `msg` pairs, when a request failed validation.
The members are listed under
[`Problem`](/reference/api/reporting-api-schemas#problem).

A client branches on `type`. The values are the constants of
[`internal/reporting/httpapi/problem`](https://github.com/B42Labs/tally/blob/main/internal/reporting/httpapi/problem/problem.go).

<!-- refdoc:begin problem-types -->
| Name | Value | Meaning |
| --- | --- | --- |
| `TypeValidation` | `urn:tally:error:validation` | TypeValidation marks a request the OpenAPI contract rejects. |
| `TypeUnauthorized` | `urn:tally:error:unauthorized` | TypeUnauthorized marks a request that carries no usable credential. |
| `TypeForbidden` | `urn:tally:error:forbidden` | TypeForbidden marks an authenticated caller that may not do this. |
| `TypeNotFound` | `urn:tally:error:not_found` | TypeNotFound marks a path or resource that does not exist. |
| `TypeMethodNotAllowed` | `urn:tally:error:method_not_allowed` | TypeMethodNotAllowed marks a known path addressed with the wrong method. |
| `TypeConflict` | `urn:tally:error:conflict` | TypeConflict marks a write that collides with existing state, such as a project whose (cloud, external_id) is already registered or a relation triple that is already active. |
| `TypePayloadTooLarge` | `urn:tally:error:payload_too_large` | TypePayloadTooLarge marks a request that carries more than the endpoint takes at once, such as an event batch above the item limit. |
| `TypeHistoryTooLong` | `urn:tally:error:history_too_long` | TypeHistoryTooLong marks a resource or project whose stored history is longer than the unpaginated per-resource reads answer at once. |
| `TypeResultTooLarge` | `urn:tally:error:result_too_large` | TypeResultTooLarge marks a query whose answer would be larger than this API serves unpaginated, such as an event grouping over too wide a window. |
| `TypeNotImplemented` | `urn:tally:error:not_implemented` | TypeNotImplemented marks a parameter or a capability a later phase delivers, such as counting resources at a historic instant. |
| `TypeRelationCycle` | `urn:tally:error:relation_cycle` | TypeRelationCycle marks a relation creation that would close a cycle over the relation types that attribute cost. |
| `TypeInternal` | `urn:tally:error:internal` | TypeInternal marks a failure the caller cannot do anything about. |
| `TypeUnavailable` | `urn:tally:error:unavailable` | TypeUnavailable marks a dependency the service needs and cannot reach. |
<!-- refdoc:end problem-types -->

## Operations

One section per operation, in the order the router matches the paths. A
section names the credential the operation takes, the parameters it reads off
the request, the body it accepts and every status it answers with.

<!-- refdoc:begin operations -->
### `GET /readyz`

Readiness probe

Reports whether the service can serve traffic. It fails while the database is unreachable, which takes the pod out of rotation without restarting it.

No credential.

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The service is ready to serve traffic. | `text/plain` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |
| `503` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /metrics`

Service metrics

Serves the instruments of this service in the Prometheus exposition format: the nine `tally_` series over the ingest, projection, and reconciliation paths, together with the Go runtime and process collectors.

The route carries no credential, which is what roadmap/00-conventions.md section 7 asks of every service, and the Gateway publishes `/api/v1` alone, so it stays reachable from inside the cluster only. A deployment whose configuration turns the instrumentation off is answered 404 here.

No credential.

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The current values of this service's instruments. | `text/plain` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `POST /internal/projection/rebuild`

Rebuild the projection

Replays the event history of every resource the filter selects into the projection and answers once it is done. It is the operational guarantee behind the derived rows: whatever a projection row holds, the history it comes from can produce it again.

Security: `internalToken`

The request body is `application/json`, a [RebuildRequest](/reference/api/reporting-api-schemas#rebuildrequest).

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The rebuild finished. | [RebuildResult](/reference/api/reporting-api-schemas#rebuildresult) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `413` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /healthz`

Liveness probe

Reports whether the process should keep running. It fails only after the service has been unhealthy for longer than the configured threshold, so that a transient database outage restarts nothing.

No credential.

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The service is alive. | `text/plain` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |
| `503` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/stats/resources`

Count the current resources per group

Returns the projection counted along the dimensions `group_by` names, one item per combination of values that carries at least one resource. A combination no resource carries is left out rather than served as a zero. The order is `(cloud, resource_type, state, platform, project_id)` ascending, where a dimension outside the grouping compares as the empty string.

The answer is never paginated: one item stands for however many resources the group holds, so the answer grows with the number of combinations the fleet shows rather than with the fleet. Four of the five dimensions carry a handful of values each; `project_id` carries one per tenant, so a fleet showing more combinations than one answer holds is refused 422 (`urn:tally:error:result_too_large`) rather than served truncated. The counts are taken along all five dimensions whatever `group_by` names, so a coarser grouping does not lower that bound; the part of the fleet `status` selects is what does.

A project token counts the resources whose (cloud, project_id) pair one of its projects names, which is the pair the resource list narrows its rows by.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `group_by` | `query` | yes | array of `cloud`, `resource_type`, `state`, `platform`, `project_id` | Which dimensions the counts are grouped by, as a comma-separated list. `cloud` and `resource_type` have to be among them, because they are what an item is read by; a grouping that leaves either out is answered 400. The rule spans the members of one list, which this schema cannot express, so the handler is what enforces it. |
| `status` | `query` | no | `active`, `deleted`, `all`, default `active` | Which part of the fleet to count. `active` counts the rows whose state is not deleted, `deleted` counts those alone, and `all` counts both. |
| `at` | `query` | no | string, `date-time` | The instant the counts describe. Leaving it out asks for the current counts, which is what the projection holds. Any value at all is answered 501 (`urn:tally:error:not_implemented`): counting a past instant means replaying the histories, and the Phase 3 usage records are what answer that. A value meaning "now" cannot be told from a historic one, because the two differ by however long the request took, so omitting the parameter rather than sending a timestamp is how the current counts are asked for. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The counts, one item per group. | [ResourceStatsList](/reference/api/reporting-api-schemas#resourcestatslist) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `422` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |
| `501` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/stats/events`

Count the stored events per time bucket

Returns the stored events of the window counted per time bucket and per combination of the dimensions `group_by` names. `from` and `to` select the half-open window `[from, to)` on the event timestamp: an event at exactly `from` is counted, one at exactly `to` is not, and a `from` at or past `to` holds nothing. The buckets are aligned on UTC hours and days rather than on `from`, so the first one can start before the window; every bucket counts the events inside the window alone.

The order is `(bucket, cloud, event_type, source)` ascending, and a bucket no event falls into is left out rather than served as a zero. The answer is never paginated: a request that groups into more rows than one answer carries is refused 422 (`urn:tally:error:result_too_large`) rather than answered with a truncated count. The bound counts the finest grouping there is, which is what the read returns, so a narrower window or a coarser interval is what gets such a request through; leaving `source` out of `group_by` does not lower it.

A second bound is read on the window itself, `to - from`, and it is decided before anything is counted: the aggregate behind the count walks every event of the window before its row limit discards anything, so what a request costs is set by the events the window holds rather than by the buckets it names. A window wider than a year and a month is refused 422 whatever the interval, rather than aggregated across the archive and then thrown away.

The counts are event-scoped the way the event list is: a project token counts every event whose `project_id` names one of its projects, including the events a resource carried before it was transferred away.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `group_by` | `query` | yes | array of `cloud`, `event_type`, `source` | Which dimensions the counts are grouped by, as a comma-separated list. `cloud` and `event_type` have to be among them, because they are what an item is read by; a grouping that leaves either out is answered 400. The rule spans the members of one list, which this schema cannot express, so the handler is what enforces it. |
| `from` | `query` | yes | string, `date-time` | Count only the events at or after this instant, the inclusive bound of the window. |
| `to` | `query` | yes | string, `date-time` | Count only the events before this instant, the exclusive bound of the window. |
| `interval` | `query` | yes | `1h`, `1d` | How wide one bucket is. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The counts, one item per bucket and group. | [EventStatsList](/reference/api/reporting-api-schemas#eventstatslist) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `422` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/resources`

List the current resources

Returns the projection rows, one per resource, narrowed by whichever filters the request carries. The order is `(cloud, resource_type, resource_id)` ascending.

One call answers one page. `items` carries the rows, and `next_cursor` is what the next call passes as `cursor`; a null `next_cursor` is the end of the walk. The cursor holds a position and nothing else, so presenting it together with other filters is well defined: the filters of that call apply from that position onwards.

A deleted resource keeps its row, with `state` deleted and `deleted_at` set. Rows are never removed, so a resource that is gone stays readable here and through its history.

A project token reads the rows whose (cloud, project_id) pair one of its projects names. The pair is what decides, so a project id another cloud uses for something else stays out of the answer.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `cloud` | `query` | no | string | Serve only the resources of this cloud. |
| `platform` | `query` | no | string | Serve only the resources of this platform. |
| `project_id` | `query` | no | string | Serve only the resources of this project, named the way its cloud names it. A token asking for a project outside its scope is answered 403. |
| `resource_type` | `query` | no | string | Serve only the resources of this type. |
| `state` | `query` | no | string | Serve only the resources whose current state is exactly this, shutoff for example. |
| `status` | `query` | no | `active`, `deleted`, `all`, default `active` | Which part of the fleet to serve. `active` serves the rows whose state is not deleted, `deleted` serves those alone, and `all` serves both. `state` and `status` are independent filters, so a contradictory pair such as `state=active&status=deleted` yields the empty page. |
| `limit` | `query` | no | integer, 1 to 1000, default `100` | How many resources one page carries at most. |
| `cursor` | `query` | no | string | The `next_cursor` of the page before this one. It is opaque: a client passes it back as it received it and reads nothing out of it. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | One page of current resources. | [ResourceList](/reference/api/reporting-api-schemas#resourcelist) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/resource-types`

List the registered resource types

Returns every registered (platform, resource_type) pair together with the size schema its events are checked against.

Security: `apiToken`

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The registered resource types. | [ResourceTypeList](/reference/api/reporting-api-schemas#resourcetypelist) | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/rejected-events`

List the dead-lettered events

Returns every ingest item this API refused server-side, with the reason it was refused and the raw body as it was submitted. The order is `(received_at, id)` ascending. `from` and `to` select the half-open window `[from, to)` on `received_at`, which is when the item was refused rather than when its event claims to have happened: an item received at exactly `from` is served, one received at exactly `to` is not.

One call answers one page. `items` carries the items, and `next_cursor` is what the next call passes as `cursor`; a null `next_cursor` is the end of the walk. The cursor holds a position and nothing else, so presenting it together with other filters is well defined: the filters of that call apply from that position onwards.

The operation is admin-only: callers below the admin role are answered 403. No project scope applies, because the raw JSON of a refused item carries no reliable project attribution.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `from` | `query` | no | string, `date-time` | Serve only the items refused at or after this instant, the inclusive bound of the window. |
| `to` | `query` | no | string, `date-time` | Serve only the items refused before this instant, the exclusive bound of the window. |
| `limit` | `query` | no | integer, 1 to 1000, default `100` | How many items one page carries at most. |
| `cursor` | `query` | no | string | The `next_cursor` of the page before this one. It is opaque: a client passes it back as it received it and reads nothing out of it. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | One page of dead-lettered events. | [DeadLetterList](/reference/api/reporting-api-schemas#deadletterlist) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/projects`

List the registered projects

Returns the registered projects, narrowed by whichever filters the request carries. The order is `(cloud, external_id)` ascending.

One call answers one page. `items` carries the projects, and `next_cursor` is what the next call passes as `cursor`; a null `next_cursor` is the end of the walk. The cursor holds a position and nothing else, so presenting it together with other filters is well defined: the filters of that call apply from that position onwards.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `platform` | `query` | no | string | Serve only the projects of this platform. |
| `cloud` | `query` | no | string | Serve only the projects of this cloud. |
| `external_id` | `query` | no | string | Serve only the projects their cloud names this way. It is not a key on its own, so two clouds using the same external id are both served unless `cloud` narrows the answer. |
| `limit` | `query` | no | integer, 1 to 1000, default `100` | How many projects one page carries at most. |
| `cursor` | `query` | no | string | The `next_cursor` of the page before this one. It is opaque: a client passes it back as it received it and reads nothing out of it. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | One page of registered projects. | [ProjectList](/reference/api/reporting-api-schemas#projectlist) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `POST /api/v1/projects`

Register a project

Registers one project. The registry is keyed by `(cloud, external_id)`: a pair it already holds is answered 409 rather than replaced, so a repeated registration never overwrites what an operator entered before.

A registration whose `platform` is `meta` or `partner` has to carry that same literal as its `cloud`, and no other platform may carry `meta` or `partner` as its `cloud`. A registration breaking that rule is answered 422 without being written.

The `id` the answer carries is how the other operations address the project. An external id alone does not, because two clouds may name different projects the same way.

Security: `apiToken`

The request body is `application/json`, a [CreateProject](/reference/api/reporting-api-schemas#createproject).

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `201` | The project as it is now registered. | [Project](/reference/api/reporting-api-schemas#project) | `Location` |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `409` | The request failed. The body says how. | `application/problem+json` | none |
| `422` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/events`

List stored events

Returns every stored event, narrowed by whichever filters the request carries. The order is `(timestamp, event_id)` ascending. `from` and `to` select the half-open window `[from, to)` on the event timestamp: an event at exactly `from` is served, one at exactly `to` is not.

One call answers one page. `items` carries the events, and `next_cursor` is what the next call passes as `cursor`; a null `next_cursor` is the end of the walk. The cursor holds a position and nothing else, so presenting it together with other filters is well defined: the filters of that call apply from that position onwards.

This list is event-scoped rather than resource-scoped. A project token reads every event whose `project_id` names one of its projects, including the events a resource carried before it was transferred away. The per-resource reads scope every event the same way, so a transfer moves the resource to its new project without handing that project the history the resource carried under the old one.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `cloud` | `query` | no | string | Serve only the events of this cloud. |
| `platform` | `query` | no | string | Serve only the events of this platform. |
| `project_id` | `query` | no | string | Serve only the events of this project, named the way its cloud names it. A token asking for a project outside its scope is answered 403. |
| `resource_type` | `query` | no | string | Serve only the events about resources of this type. |
| `event_type` | `query` | no | string | Serve only the events of this type, volume.create for example. |
| `source` | `query` | no | `collector`, `reconciliation` | Serve only the events the named pipeline produced. |
| `from` | `query` | no | string, `date-time` | Serve only the events at or after this instant, the inclusive bound of the window. |
| `to` | `query` | no | string, `date-time` | Serve only the events before this instant, the exclusive bound of the window. |
| `limit` | `query` | no | integer, 1 to 1000, default `100` | How many events one page carries at most. |
| `cursor` | `query` | no | string | The `next_cursor` of the page before this one. It is opaque: a client passes it back as it received it and reads nothing out of it. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | One page of stored events. | [EventList](/reference/api/reporting-api-schemas#eventlist) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `POST /api/v1/events`

Ingest events

Stores a batch of canonical events. The body is either one event or an array of at most 1000 of them.

The answer is 200 whenever the request itself was authorized and readable, whatever the individual items did: an item this API refuses is kept server-side with the reason it was refused and reported in `rejected`. A collector may therefore drop the whole batch from its buffer once the call returns, and must not retry a rejected item.

Security: `ingestToken`

The request body is `application/json`, an [EventInput](/reference/api/reporting-api-schemas#eventinput) or an array of [EventInput](/reference/api/reporting-api-schemas#eventinput).

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The batch was processed. The body says what happened to it. | [IngestResult](/reference/api/reporting-api-schemas#ingestresult) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `413` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `POST /internal/sync/{cloud}`

Reconcile one cloud

Runs one sync of the cloud. A sync asks the platform's adapter what the cloud currently holds, diffs that observation against the projection, and feeds the difference back as synthetic events through the ordinary ingest path, so a resource a run corrected ends up with the same kind of history as one that was never missed.

The run is synchronous: the answer carries the stats of the run that just happened rather than a handle to poll. A cloud the configuration does not name is answered 404, and a cloud another run is holding is answered 409, because two syncs of one cloud would diff the same projection rows against two overlapping observations.

The request may carry a body naming the instant the run is at. Only a deployment that sets `TALLY_REPORTING_SYNC_ALLOW_AT` accepts it; anywhere else such a body is answered 400 and no run starts.

Security: `internalToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `cloud` | `path` | yes | string | The installation to reconcile, os-prod-eu1 for example. |

The request body is `application/json`, a [SyncRequest](/reference/api/reporting-api-schemas#syncrequest).

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The sync run finished. | [SyncResult](/reference/api/reporting-api-schemas#syncresult) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `409` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/projects/{id}/summary`

Summarize what one project ran inside a window

Returns the project together with what each of its resource types did inside the half-open window `[from, to)`: how many resources of the type began or ended their life in it, how many minutes they ran within it, and how many of the type the project holds right now.

The three window numbers come from folding the histories of the project's resources with the fold every other read of a history runs, clipped to the window, and an interval still open is counted up to the instant of the request. A resource that changed hands counts here over the part of the window this project held it: the transfer ends its minutes here and begins the new owner's, so a resource is billed to one project at a time. `created` and `deleted` are read off the project's own events, so a transfer moves neither. `active_now` is counted off the projection instead, so it describes the present whatever the window covers. A `from` at or past `to` is an empty window: created, deleted, and the minutes come out zero, while `active_now` stays what it is.

A project this registry does not hold and a project outside the token's scope are answered the same 404, body for body, so a caller cannot tell the projects another token holds from ids that name nothing.

The embedded `project` names the project and nothing more. This route is reachable with a `project` token, and the registry row it hangs off is not: the name and the operator-set metadata are served by `getProject`, which takes `read_all`.

A project whose history is longer than one summary folds at once is answered 422 (`urn:tally:error:history_too_long`) rather than a summary folded from part of it. The Phase 3 usage records are what answer a history that long.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `id` | `path` | yes | [Uuid](/reference/api/reporting-api-schemas#uuid) | The project the summary is about. |
| `from` | `query` | yes | string, `date-time` | When the window starts, the inclusive bound. |
| `to` | `query` | yes | string, `date-time` | When the window ends, the exclusive bound. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The project and what its resource types did in the window. | [ProjectSummary](/reference/api/reporting-api-schemas#projectsummary) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `422` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/projects/{id}/relations`

List the relations of one project

Returns the relations of the project that are valid at `at`, ordered by `(created_at, id)`. A relation is valid at `t` iff `valid_from <= t AND (valid_to IS NULL OR valid_to > t)`, so the answer is the point-in-time view of the neighborhood and history is read by passing a past `at`.

The answer is never paginated. A project this registry does not hold is answered 404, which is what tells an empty neighborhood from an unknown project.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `id` | `path` | yes | [Uuid](/reference/api/reporting-api-schemas#uuid) | The project the relations are addressed under. |
| `direction` | `query` | no | `outgoing`, `incoming`, `both`, default `both` | Which relations to serve: `outgoing` the ones leaving the project, `incoming` the ones reaching it, `both` either. |
| `relation_type` | `query` | no | string | Serve only the relations of this type. |
| `at` | `query` | no | string, `date-time` | The instant the answer describes. It defaults to now. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The relations valid at that instant. | [RelationList](/reference/api/reporting-api-schemas#relationlist) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `POST /api/v1/projects/{id}/relations`

Relate one project to another

Creates one relation leaving the project the path names. A relation is valid at `t` iff `valid_from <= t AND (valid_to IS NULL OR valid_to > t)`, and `valid_from` defaults to the instant the relation is written.

One triple of source, target and `relation_type` carries a single open relation at a time. A triple that is already active is answered 409; the same triple after a close is created again.

A source project this registry does not hold is answered 404. A target it does not hold, and a target that is the source itself, are answered 422.

A relation of one of the configured attributing types keeps attribution a forest. Its creation walks the active attributing relations out of `target_id` first, and a relation that reaches the source again is answered 422 (`urn:tally:error:relation_cycle`) without being written. A type outside that list is created without the walk.

A `metadata.pricing_adjustments` array the adjustments schema refuses is answered 422 (`urn:tally:error:validation`) with one field error per violation, located as `body.metadata.pricing_adjustments.<index>.<member>`.

A relation to or from a virtual project is created, listed, updated and closed like any other.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `id` | `path` | yes | [Uuid](/reference/api/reporting-api-schemas#uuid) | The project the relations are addressed under. |

The request body is `application/json`, a [CreateRelation](/reference/api/reporting-api-schemas#createrelation).

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `201` | The relation as it is now stored. | [Relation](/reference/api/reporting-api-schemas#relation) | `Location` |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `409` | The request failed. The body says how. | `application/problem+json` | none |
| `422` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/projects/{id}/related`

List the projects one project reaches

Walks the outgoing relations that are valid at `at`, up to `depth` relations out, and returns every project the walk reaches. A relation is valid at `t` iff `valid_from <= t AND (valid_to IS NULL OR valid_to > t)`, so the answer is the point-in-time view of the graph and history is read by passing a past `at`.

The walk is breadth-first and visits a project once, which terminates a cycle and keeps the project of the path out of the answer. The items come in the order they were visited, and `path` names the relations the walk took to reach each of them.

The answer is never paginated. A project this registry does not hold is answered 404.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `id` | `path` | yes | [Uuid](/reference/api/reporting-api-schemas#uuid) | The project the traversal starts from. |
| `depth` | `query` | no | integer, 1 to 10, default `1` | How many relations out the walk goes. |
| `relation_type` | `query` | no | string | Walk only the relations of this type. |
| `at` | `query` | no | string, `date-time` | The instant the answer describes. It defaults to now. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The projects the walk reached. | [RelatedProjectList](/reference/api/reporting-api-schemas#relatedprojectlist) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/projects/{id}`

Read one registered project

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `id` | `path` | yes | [Uuid](/reference/api/reporting-api-schemas#uuid) | The project, as this API names it. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The registered project. | [Project](/reference/api/reporting-api-schemas#project) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `PATCH /api/v1/projects/{id}`

Update one registered project

Changes the name or the metadata of one project. A member the request leaves out stays as it is, and `metadata` is replaced wholesale rather than merged, so a request carrying it carries every member the project keeps.

Neither the platform nor the `(cloud, external_id)` key is writable here. They are what the registry is keyed by, and a project that moves to another cloud is a different project.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `id` | `path` | yes | [Uuid](/reference/api/reporting-api-schemas#uuid) | The project, as this API names it. |

The request body is `application/json`, an [UpdateProject](/reference/api/reporting-api-schemas#updateproject).

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The project as it now stands. | [Project](/reference/api/reporting-api-schemas#project) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/resource-types/{platform}/{resource_type}`

Read one registered resource type

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `platform` | `path` | yes | string | The platform the resource type belongs to, openstack for example. |
| `resource_type` | `path` | yes | string | The resource type within that platform, instance for example. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The registered resource type. | [ResourceType](/reference/api/reporting-api-schemas#resourcetype) | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `PUT /api/v1/resource-types/{platform}/{resource_type}`

Register a resource type

Registers the size schema of one (platform, resource_type) pair, or replaces the schema already registered for it. The document is compiled before it is stored, so a schema that does not compile is refused and nothing changes.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `platform` | `path` | yes | string | The platform the resource type belongs to, openstack for example. |
| `resource_type` | `path` | yes | string | The resource type within that platform, instance for example. |

The request body is `application/json`, a [RegisterResourceType](/reference/api/reporting-api-schemas#registerresourcetype).

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The resource type as it is now registered. | [ResourceType](/reference/api/reporting-api-schemas#resourcetype) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `413` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `PATCH /api/v1/projects/{id}/relations/{relation_id}`

Update one relation

Changes the metadata or the end of one relation. A member the request leaves out stays as it is, and `metadata` is replaced wholesale rather than merged.

`valid_to` has to be after `valid_from`, which is 422 otherwise. It is also where a closed relation gets its close instant corrected; the member is not nullable, so reopening a closed relation is not supported.

A relation the path does not name, and a relation that does not leave the project of the path, are both answered 404.

A `metadata.pricing_adjustments` array the adjustments schema refuses is answered 422, and a document whose `pricing_adjustments` differs from the stored one is answered 409 (`urn:tally:error:conflict`).

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `id` | `path` | yes | [Uuid](/reference/api/reporting-api-schemas#uuid) | The project the relation leaves. |
| `relation_id` | `path` | yes | [Uuid](/reference/api/reporting-api-schemas#uuid) | The relation itself, as this API names it. |

The request body is `application/json`, an [UpdateRelation](/reference/api/reporting-api-schemas#updaterelation).

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The relation as it now stands. | [Relation](/reference/api/reporting-api-schemas#relation) | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `409` | The request failed. The body says how. | `application/problem+json` | none |
| `422` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `DELETE /api/v1/projects/{id}/relations/{relation_id}`

Close one relation

Closes the relation by setting `valid_to` to now. The row is never deleted, so a read at an earlier `at` still finds the relation.

A relation that is already closed is answered 204 as well, and the stored `valid_to` does not move: the close instant a relation was given is the one it keeps. A relation this API does not hold, and one that does not leave the project of the path, are answered 404.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `id` | `path` | yes | [Uuid](/reference/api/reporting-api-schemas#uuid) | The project the relation leaves. |
| `relation_id` | `path` | yes | [Uuid](/reference/api/reporting-api-schemas#uuid) | The relation itself, as this API names it. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `204` | The relation is closed. | none | none |
| `400` | The request failed. The body says how. | `application/problem+json` | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `403` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/resources/{cloud}/{resource_type}/{resource_id}/lifecycle`

Read the folded lifecycle of one resource

Returns the resource's history folded into the half-open billable intervals of roadmap/00-conventions.md section 5, next to the projection row and the events the fold ran on. It is the fold the projection replay runs, so the intervals here are the ones every derived row comes from.

`warnings` names what the fold could not trust, a history that starts without a create for example.

The read is gated and scoped the way the per-resource history is: a resource this API holds no projection row for and a resource outside the token's scope are answered the same 404, the events a project token folds are its own projects' events, and a resource with more than 10000 stored events is answered 422 (`urn:tally:error:history_too_long`). Folding a scoped history is what keeps the intervals a project reads the spans it is billed for: a transferred resource is folded from the transfer onwards, which `warnings` reports as a history that starts without a create.

The embedded `resource` is the projection row and is not scoped, so its `created_at` can predate the reader's ownership while the intervals start at the transfer. Bill from `intervals`.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `cloud` | `path` | yes | string | The installation the resource lives in, os-prod-eu1 for example. |
| `resource_type` | `path` | yes | string | The kind of resource, instance or volume for example. |
| `resource_id` | `path` | yes | string | The resource itself, as its cloud names it. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The resource, its history, and the intervals it folds into. | [Lifecycle](/reference/api/reporting-api-schemas#lifecycle) | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `422` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |

### `GET /api/v1/resources/{cloud}/{resource_type}/{resource_id}/events`

Read the event history of one resource

Returns the history of one resource, ordered by `(timestamp, received_at, event_id)`. The answer is never paginated: one call carries every event the request may see, and `next_cursor` is always null. A resource with more than 10000 stored events is answered 422 (`urn:tally:error:history_too_long`) rather than a truncated history; `GET /api/v1/events` pages such a history.

The read is gated on the project the resource's projection row names today. A project token whose scope does not hold that (cloud, project_id) pair is answered the 404 an unknown resource gets, so a resource outside the scope cannot be told from one that does not exist. Every served event is scoped the same way, so a project reads the part of the history its own projects carried and no more: after a transfer the old project stops reading the resource here, and the new one reads the events stored since the transfer rather than the whole history. `GET /api/v1/events` is where the old project keeps reading the events it carried.

Security: `apiToken`

| Name | In | Required | Type | Description |
| --- | --- | --- | --- | --- |
| `cloud` | `path` | yes | string | The installation the resource lives in, os-prod-eu1 for example. |
| `resource_type` | `path` | yes | string | The kind of resource, instance or volume for example. |
| `resource_id` | `path` | yes | string | The resource itself, as its cloud names it. |

| Status | Description | Body | Headers |
| --- | --- | --- | --- |
| `200` | The ordered history of the resource, with `next_cursor` null. | [EventList](/reference/api/reporting-api-schemas#eventlist) | none |
| `401` | The request failed. The body says how. | `application/problem+json` | none |
| `404` | The request failed. The body says how. | `application/problem+json` | none |
| `422` | The request failed. The body says how. | `application/problem+json` | none |
| `500` | The request failed. The body says how. | `application/problem+json` | none |
<!-- refdoc:end operations -->

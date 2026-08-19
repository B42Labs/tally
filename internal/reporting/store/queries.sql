-- name: CreateIngestCredential :one
INSERT INTO ingest_credentials (platform, cloud, token_hash, description)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: GetIngestCredentialByTokenHash :one
SELECT id, platform, cloud, token_hash, description, created_at, revoked_at
FROM ingest_credentials
WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: GetIngestCredentialForUpdate :one
SELECT id, platform, cloud, token_hash, description, created_at, revoked_at
FROM ingest_credentials
WHERE id = $1
FOR UPDATE;

-- name: RevokeIngestCredential :exec
UPDATE ingest_credentials
SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL;

-- name: CreateAPIToken :one
INSERT INTO api_tokens (token_hash, role, project_ids, description)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: GetAPITokenByTokenHash :one
SELECT id, token_hash, role, project_ids, description, created_at, revoked_at
FROM api_tokens
WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: GetAPITokenForUpdate :one
SELECT id, token_hash, role, project_ids, description, created_at, revoked_at
FROM api_tokens
WHERE id = $1
FOR UPDATE;

-- name: RevokeAPIToken :exec
UPDATE api_tokens
SET revoked_at = now()
WHERE id = $1 AND revoked_at IS NULL;

-- name: InsertAuditLog :exec
INSERT INTO audit_log (actor, action, object_type, object_id, details)
VALUES ($1, $2, $3, $4, $5);

-- name: GetProjectRefsByIDs :many
SELECT id, cloud, external_id
FROM projects
WHERE id = ANY($1::uuid[]);

-- name: InsertEvent :one
INSERT INTO events (event_id, timestamp, event_type, platform, cloud,
                    resource_type, resource_id, project_id, source, payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (event_id, timestamp) DO NOTHING
RETURNING received_at;

-- name: InsertRejectedEvent :exec
INSERT INTO rejected_events (reason, raw)
VALUES ($1, $2);

-- name: UpsertResourceType :one
INSERT INTO resource_types (platform, resource_type, size_schema)
VALUES ($1, $2, $3)
ON CONFLICT (platform, resource_type) DO UPDATE
SET size_schema = EXCLUDED.size_schema,
    updated_at = now()
RETURNING platform, resource_type, size_schema, updated_at;

-- name: GetResourceType :one
SELECT platform, resource_type, size_schema, updated_at
FROM resource_types
WHERE platform = $1 AND resource_type = $2;

-- name: ListResourceTypes :many
SELECT platform, resource_type, size_schema, updated_at
FROM resource_types
ORDER BY platform, resource_type;

-- name: GetCurrentResourceForUpdate :one
SELECT cloud, platform, resource_type, resource_id, project_id, state, size,
       created_at, deleted_at, last_event_type, last_event_at, last_payload
FROM current_resources
WHERE cloud = $1 AND resource_type = $2 AND resource_id = $3
FOR UPDATE;

-- name: UpsertCurrentResource :exec
INSERT INTO current_resources (cloud, platform, resource_type, resource_id,
                               project_id, state, size, created_at, deleted_at,
                               last_event_type, last_event_at, last_payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (cloud, resource_type, resource_id) DO UPDATE
SET platform = EXCLUDED.platform,
    project_id = EXCLUDED.project_id,
    state = EXCLUDED.state,
    size = EXCLUDED.size,
    created_at = EXCLUDED.created_at,
    deleted_at = EXCLUDED.deleted_at,
    last_event_type = EXCLUDED.last_event_type,
    last_event_at = EXCLUDED.last_event_at,
    last_payload = EXCLUDED.last_payload;

-- The history of one resource. Two callers read it: the projection replay, which
-- folds every event there is under no scope and no bound, and the two
-- per-resource routes of the API, which pass both. A NULL page size is every
-- row, which is what the replay folds.
-- name: ListEventsForResource :many
SELECT event_id, timestamp, received_at, event_type, platform, cloud,
       resource_type, resource_id, project_id, source, payload
FROM events
WHERE cloud = $1 AND resource_type = $2 AND resource_id = $3
  -- The same pair filter the event list runs, per event rather than per
  -- resource: a transfer moves the resource to its new project, and the events
  -- it carried before the transfer stay the old project's alone.
  AND (sqlc.narg('scope_clouds')::text[] IS NULL
       OR (cloud, project_id) IN (SELECT unnest(sqlc.narg('scope_clouds')::text[]),
                                         unnest(sqlc.narg('scope_projects')::text[])))
ORDER BY timestamp, received_at, event_id
LIMIT sqlc.narg('page_size');

-- How long the history above is under the same scope, counted no further than
-- probe_limit. The unpaginated per-resource routes ask this before they read
-- anything, because the bound they enforce is a memory bound and the read above
-- materializes every row it returns before the caller sees the first one:
-- deciding the refusal off the rows themselves would cost exactly the payloads
-- the refusal exists to avoid.
--
-- The answer saturates at probe_limit, and a caller reads it as "at least this
-- many" rather than as the length of the history. A plain count(*) cannot stop
-- early: it would answer the bounded question of whether the history is too long
-- by walking every event the resource has, on exactly the request the bound
-- exists to keep cheap, and this table has no ceiling on how many that is. The
-- inner LIMIT is what stops the walk at the only rows the decision needs.
-- name: CountEventsForResource :one
SELECT count(*)
FROM (
    SELECT 1
    FROM events
    WHERE cloud = $1 AND resource_type = $2 AND resource_id = $3
      AND (sqlc.narg('scope_clouds')::text[] IS NULL
           OR (cloud, project_id) IN (SELECT unnest(sqlc.narg('scope_clouds')::text[]),
                                             unnest(sqlc.narg('scope_projects')::text[])))
    LIMIT sqlc.arg('probe_limit')
) probe;

-- name: ListResourceKeys :many
SELECT DISTINCT cloud, resource_type, resource_id
FROM events
WHERE (sqlc.narg('cloud')::text IS NULL OR cloud = sqlc.narg('cloud'))
  AND (sqlc.narg('resource_type')::text IS NULL OR resource_type = sqlc.narg('resource_type'))
ORDER BY cloud, resource_type, resource_id;

-- name: LockResource :exec
SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':' || $2::text || ':' || $3::text, 0));

-- name: ListEvents :many
SELECT event_id, timestamp, received_at, event_type, platform, cloud,
       resource_type, resource_id, project_id, source, payload
FROM events
WHERE (sqlc.narg('cloud')::text IS NULL OR cloud = sqlc.narg('cloud'))
  AND (sqlc.narg('platform')::text IS NULL OR platform = sqlc.narg('platform'))
  AND (sqlc.narg('project_id')::text IS NULL OR project_id = sqlc.narg('project_id'))
  AND (sqlc.narg('resource_type')::text IS NULL OR resource_type = sqlc.narg('resource_type'))
  AND (sqlc.narg('event_type')::text IS NULL OR event_type = sqlc.narg('event_type'))
  AND (sqlc.narg('source')::text IS NULL OR source = sqlc.narg('source'))
  AND (sqlc.narg('from_ts')::timestamptz IS NULL OR timestamp >= sqlc.narg('from_ts'))
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR timestamp < sqlc.narg('to_ts'))
  AND (sqlc.narg('scope_clouds')::text[] IS NULL
       OR (cloud, project_id) IN (SELECT unnest(sqlc.narg('scope_clouds')::text[]),
                                         unnest(sqlc.narg('scope_projects')::text[])))
  -- Both bounds are cast: inside a row comparison sqlc reads the type of the
  -- second placeholder off the first one, which would make the event id a
  -- timestamp in the generated parameters.
  AND (sqlc.narg('cursor_ts')::timestamptz IS NULL
       OR (timestamp, event_id) > (sqlc.narg('cursor_ts')::timestamptz,
                                   sqlc.narg('cursor_event_id')::text))
ORDER BY timestamp, event_id
LIMIT sqlc.arg('page_size');

-- name: ListCurrentResources :many
SELECT cloud, platform, resource_type, resource_id, project_id, state, size,
       created_at, deleted_at, last_event_type, last_event_at, last_payload
FROM current_resources
WHERE (sqlc.narg('cloud')::text IS NULL OR cloud = sqlc.narg('cloud'))
  AND (sqlc.narg('platform')::text IS NULL OR platform = sqlc.narg('platform'))
  AND (sqlc.narg('project_id')::text IS NULL OR project_id = sqlc.narg('project_id'))
  AND (sqlc.narg('resource_type')::text IS NULL OR resource_type = sqlc.narg('resource_type'))
  AND (sqlc.narg('state')::text IS NULL OR state = sqlc.narg('state'))
  -- The status filter of the API, as the one boolean the two halves of the
  -- fleet differ in: true serves the deleted rows, false the ones that live,
  -- and NULL both.
  AND (sqlc.narg('deleted')::boolean IS NULL OR (state = 'deleted') = sqlc.narg('deleted'))
  AND (sqlc.narg('scope_clouds')::text[] IS NULL
       OR (cloud, project_id) IN (SELECT unnest(sqlc.narg('scope_clouds')::text[]),
                                         unnest(sqlc.narg('scope_projects')::text[])))
  -- Every bound is cast: inside a row comparison sqlc reads the type of the
  -- later placeholders off the first one, which is why the events cursor above
  -- casts both of its bounds too.
  AND (sqlc.narg('cursor_cloud')::text IS NULL
       OR (cloud, resource_type, resource_id) > (sqlc.narg('cursor_cloud')::text,
                                                 sqlc.narg('cursor_resource_type')::text,
                                                 sqlc.narg('cursor_resource_id')::text))
ORDER BY cloud, resource_type, resource_id
LIMIT sqlc.arg('page_size');

-- name: ListRejectedEvents :many
SELECT id, received_at, reason, raw
FROM rejected_events
WHERE (sqlc.narg('from_ts')::timestamptz IS NULL OR received_at >= sqlc.narg('from_ts'))
  AND (sqlc.narg('to_ts')::timestamptz IS NULL OR received_at < sqlc.narg('to_ts'))
  -- Both bounds are cast, for the reason the events cursor above names.
  AND (sqlc.narg('cursor_ts')::timestamptz IS NULL
       OR (received_at, id) > (sqlc.narg('cursor_ts')::timestamptz, sqlc.narg('cursor_id')::uuid))
ORDER BY received_at, id
LIMIT sqlc.arg('page_size');

-- The read path of one projection row. It is GetCurrentResourceForUpdate
-- without the row lock: a read must not make a writer wait on it.
-- name: GetCurrentResource :one
SELECT cloud, platform, resource_type, resource_id, project_id, state, size,
       created_at, deleted_at, last_event_type, last_event_at, last_payload
FROM current_resources
WHERE cloud = $1 AND resource_type = $2 AND resource_id = $3;

-- name: InsertProject :one
INSERT INTO projects (platform, cloud, external_id, name, metadata)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, platform, cloud, external_id, name, metadata, created_at;

-- name: GetProject :one
SELECT id, platform, cloud, external_id, name, metadata, created_at
FROM projects
WHERE id = $1;

-- The batch form of the read above, for the callers that resolve a whole set of
-- ids at once. GetProjectRefsByIDs is the same read narrowed to the two columns
-- an event needs.
-- name: GetProjectsByIDs :many
SELECT id, platform, cloud, external_id, name, metadata, created_at
FROM projects
WHERE id = ANY($1::uuid[]);

-- name: ListProjects :many
SELECT id, platform, cloud, external_id, name, metadata, created_at
FROM projects
WHERE (sqlc.narg('platform')::text IS NULL OR platform = sqlc.narg('platform'))
  AND (sqlc.narg('cloud')::text IS NULL OR cloud = sqlc.narg('cloud'))
  AND (sqlc.narg('external_id')::text IS NULL
       OR external_id = sqlc.narg('external_id'))
  -- Both bounds are cast, for the reason the events cursor above names.
  AND (sqlc.narg('cursor_cloud')::text IS NULL
       OR (cloud, external_id) > (sqlc.narg('cursor_cloud')::text,
                                  sqlc.narg('cursor_external_id')::text))
ORDER BY cloud, external_id
LIMIT sqlc.arg('page_size');

-- A NULL parameter leaves the column as it stands, so a request that carries
-- one field updates that field alone.
-- name: UpdateProject :one
UPDATE projects
SET name = COALESCE(sqlc.narg('name'), name),
    metadata = COALESCE(sqlc.narg('metadata'), metadata)
WHERE id = $1
RETURNING id, platform, cloud, external_id, name, metadata, created_at;

-- A NULL valid_from means the relation starts when it is written, which is what
-- a request that names no start asks for.
-- name: InsertProjectRelation :one
INSERT INTO project_relations (source_id, target_id, relation_type, metadata,
                               valid_from)
VALUES ($1, $2, $3, $4, COALESCE(sqlc.narg('valid_from')::timestamptz, now()))
RETURNING id, source_id, target_id, relation_type, metadata, valid_from,
          valid_to, created_at;

-- The source project is part of every single-relation key: a relation is
-- addressed under the project it leaves, and an id that belongs to another
-- project reads as absent rather than as someone else's row.
-- name: GetProjectRelation :one
SELECT id, source_id, target_id, relation_type, metadata, valid_from, valid_to,
       created_at
FROM project_relations
WHERE id = $1 AND source_id = $2;

-- The relations of one project as they stood at one instant. The two direction
-- flags are what the direction filter of the API comes down to: a relation
-- counts when it leaves the project and outgoing is set, or reaches it and
-- incoming is.
-- name: ListProjectRelations :many
SELECT id, source_id, target_id, relation_type, metadata, valid_from, valid_to,
       created_at
FROM project_relations
WHERE ((sqlc.arg('outgoing')::boolean AND source_id = sqlc.arg('project_id'))
       OR (sqlc.arg('incoming')::boolean AND target_id = sqlc.arg('project_id')))
  AND valid_from <= sqlc.arg('at')
  AND (valid_to IS NULL OR valid_to > sqlc.arg('at'))
  AND (sqlc.narg('relation_type')::text IS NULL
       OR relation_type = sqlc.narg('relation_type'))
ORDER BY created_at, id;

-- name: UpdateProjectRelation :one
UPDATE project_relations
SET metadata = COALESCE(sqlc.narg('metadata'), metadata),
    valid_to = COALESCE(sqlc.narg('valid_to'), valid_to)
WHERE id = $1 AND source_id = $2
RETURNING id, source_id, target_id, relation_type, metadata, valid_from,
          valid_to, created_at;

-- A relation is closed rather than deleted, so that a read at an earlier
-- instant still finds it. The row count tells the close apart from the relation
-- that was closed already, which no longer matches.
-- name: CloseProjectRelation :execrows
UPDATE project_relations
SET valid_to = now()
WHERE id = $1 AND source_id = $2 AND valid_to IS NULL;

-- The relations of a whole set of projects at one instant, for the callers that
-- walk the graph rather than serve one project's list.
-- name: ListRelationsValidAt :many
SELECT id, source_id, target_id, relation_type, metadata, valid_from, valid_to,
       created_at
FROM project_relations
WHERE source_id = ANY(sqlc.arg('source_ids')::uuid[])
  AND valid_from <= sqlc.arg('at')
  AND (valid_to IS NULL OR valid_to > sqlc.arg('at'))
  AND (sqlc.narg('relation_type')::text IS NULL
       OR relation_type = sqlc.narg('relation_type'))
ORDER BY source_id, created_at, id;

-- The walk the cycle check runs, which asks about the graph as it is rather
-- than as it stood at some instant: only the open relations of the attributing
-- types can carry cost, so only they can close a cycle that matters.
-- name: ListActiveAttributingRelations :many
SELECT id, source_id, target_id, relation_type, metadata, valid_from, valid_to,
       created_at
FROM project_relations
WHERE source_id = ANY(sqlc.arg('source_ids')::uuid[])
  AND valid_to IS NULL
  AND relation_type = ANY(sqlc.arg('relation_types')::text[])
ORDER BY source_id, created_at, id;

-- Serializes the creation of attributing relations, so that two racing inserts
-- cannot each pass the cycle walk and leave a cycle behind.
-- name: LockAttributingRelations :exec
SELECT pg_advisory_xact_lock(hashtextextended('project_relations:attributing', 0));

-- The run row is written before the adapter is called, so that a run in flight
-- can be seen while it works: status defaults to 'running' in the schema, and
-- the row is left at that value until the run finishes.
-- name: InsertSyncRun :one
INSERT INTO sync_runs (cloud)
VALUES ($1)
RETURNING id;

-- name: CompleteSyncRun :exec
UPDATE sync_runs
SET status = $2, stats = $3, completed_at = now()
WHERE id = $1;

-- Reads a finished run back, for the tests that compare the stored row against
-- the response the sync returned.
-- name: GetSyncRun :one
SELECT id, cloud, started_at, completed_at, status, stats
FROM sync_runs
WHERE id = $1;

-- Where an incremental sync starts. The bound is the previous run's started_at
-- rather than its completed_at, so that the window overlaps that run and a
-- change made while it was working cannot fall between the two.
-- name: GetLastCompletedSyncStartedAt :one
SELECT started_at
FROM sync_runs
WHERE cloud = $1 AND status = 'completed'
ORDER BY started_at DESC
LIMIT 1;

-- The stored side of the reconciliation diff, in the six columns the diff
-- reads. Deleted resources are part of the answer: one the adapter reports
-- again has come back, and telling that apart from a first sighting needs the
-- row. last_event_at is what a correction has to be dated past to reach the
-- row at all: one the fold orders before it replays the history instead.
-- name: ListCurrentResourcesByCloud :many
SELECT resource_type, resource_id, project_id, state, size, last_event_at
FROM current_resources
WHERE cloud = $1;

-- Keeps two syncs of one cloud from running at once. The lock is held for the
-- session rather than for the transaction, as LockResource is, because a run
-- spans several transactions.
-- name: TrySyncLock :one
SELECT pg_try_advisory_lock(hashtextextended('sync:' || $1::text, 0));

-- name: UnlockSync :exec
SELECT pg_advisory_unlock(hashtextextended('sync:' || $1::text, 0));

-- The fleet the projection holds, grouped the way tally_current_resources is
-- labeled. The gauge is derived from this count rather than from the events as
-- they are folded, so it cannot drift from the rows the API serves. Deleted
-- resources keep their row and are counted under state 'deleted'.
-- name: CountCurrentResources :many
SELECT platform, cloud, resource_type, state, COUNT(*) AS resources
FROM current_resources
GROUP BY platform, cloud, resource_type, state;

-- The projection counted along all five dimensions the resource statistics can
-- group by. The route takes any non-empty combination of them, which is
-- thirty-one groupings and would be thirty-one static queries. A coarser
-- grouping is the sum of these rows over the dimensions it drops, so this one
-- query answers every combination and the handler adds the rows up in Go.
--
-- The status filter and the scope pair are the ones ListCurrentResources runs,
-- so the counts cover the rows the list serves and nothing else.
--
-- row_cap bounds what one request materializes. Four of the five dimensions are
-- low-cardinality, but project_id is one value per tenant, so the row set grows
-- with the fleet's tenants rather than with the shapes it shows, and the answer
-- is not paginated.
-- name: CountCurrentResourcesGrouped :many
SELECT cloud, platform, resource_type, state, project_id, count(*) AS resources
FROM current_resources
WHERE (sqlc.narg('deleted')::boolean IS NULL OR (state = 'deleted') = sqlc.narg('deleted'))
  AND (sqlc.narg('scope_clouds')::text[] IS NULL
       OR (cloud, project_id) IN (SELECT unnest(sqlc.narg('scope_clouds')::text[]),
                                         unnest(sqlc.narg('scope_projects')::text[])))
GROUP BY cloud, platform, resource_type, state, project_id
LIMIT sqlc.arg('row_cap');

-- The event statistics route, as one row per bucket and group. The window is
-- required and half-open, which is what keeps the read on the time dimension the
-- hypertable is chunked by: a query without both bounds has every chunk there is
-- to walk.
--
-- Both casts are there for sqlc rather than for Postgres. time_bucket is not in
-- its catalog, so without ::timestamptz the bucket column comes out as an
-- interface{} in the generated row; and the width reaches interval through text
-- because a placeholder cast straight to interval generates a pgtype.Interval,
-- where the width the route carries is a string.
--
-- row_cap bounds what one request materializes: the number of buckets grows with
-- the window divided by the width, and each bucket carries a row per cloud,
-- event type and source seen in it.
-- name: CountEventBuckets :many
SELECT time_bucket((sqlc.arg('bucket_width')::text)::interval, timestamp)::timestamptz AS bucket,
       cloud, event_type, source, count(*) AS events
FROM events
WHERE timestamp >= sqlc.arg('from_ts')::timestamptz
  AND timestamp < sqlc.arg('to_ts')::timestamptz
  AND (sqlc.narg('scope_clouds')::text[] IS NULL
       OR (cloud, project_id) IN (SELECT unnest(sqlc.narg('scope_clouds')::text[]),
                                         unnest(sqlc.narg('scope_projects')::text[])))
GROUP BY 1, 2, 3, 4
ORDER BY 1, 2, 3, 4
LIMIT sqlc.arg('row_cap');

-- How many events one project summary folds, counted no further than
-- probe_limit. The summary asks this before it runs the read below, and it
-- counts that read's own row set rather than a narrower one: a project's own
-- events say nothing about how long the histories they pull in are, because a
-- resource it holds a single event of carries the events of every project it was
-- transferred between. Counting the project's events alone would therefore bound
-- nothing about the read, which materializes and sorts the joined set whole
-- before the caller sees the first row. The inner LIMIT is what stops the walk
-- at the rows the decision needs, and the answer saturates there rather than
-- reporting how long the set is.
-- name: CountProjectFoldEvents :one
SELECT count(*)
FROM (
    SELECT 1
    FROM events e
    JOIN (
        SELECT DISTINCT own.resource_type, own.resource_id
        FROM events own
        WHERE own.cloud = $1 AND own.project_id = $2
          AND own.timestamp < sqlc.arg('to_ts')::timestamptz
    ) touched ON touched.resource_type = e.resource_type
             AND touched.resource_id = e.resource_id
    WHERE e.cloud = $1 AND e.timestamp < sqlc.arg('to_ts')::timestamptz
    LIMIT sqlc.arg('probe_limit')
) probe;

-- The events one project summary folds: for every resource the project holds an
-- event of, that resource's whole history up to an instant, ordered the way the
-- per-resource history read orders.
--
-- The set is taken per resource rather than per event because a project's own
-- events do not say when a resource left it. An ownership transfer writes the
-- new project onto the event that moves the resource, so a slice narrowed to
-- project_id ends at the last event before the transfer and folds into an
-- interval that never closes, which the summary would then accrue up to the
-- instant of the request while the new owner accrues the same resource from the
-- transfer on. Reading each resource whole puts the transfer inside the fold,
-- where the project every interval carries is what says whose it is. The read
-- narrows on (cloud, resource_type, resource_id), which is what
-- idx_events_resource leads with and whose first two columns are what the
-- hypertable segments its compressed chunks by.
--
-- No payload is read. The fold takes state and size out of it only to split a
-- run of intervals where the billable configuration changed, and the pieces of a
-- split run add up to the run itself, so no number this summary reports depends
-- on them. They are what this read would cost: the column is JSONB the schema
-- puts no bound on, and every row of it would be unmarshalled into a map.
--
-- Only the upper bound of the window filters the read. The events before it are
-- what says which resources the project was already running when the window
-- opened, and the fold clips its intervals to the window itself. An event at or
-- after the bound cannot change anything inside [from, to), so that is the one
-- end the read can drop.
-- name: ListProjectFoldEvents :many
SELECT e.event_id, e.timestamp, e.received_at, e.event_type, e.platform, e.cloud,
       e.resource_type, e.resource_id, e.project_id, e.source
FROM events e
JOIN (
    SELECT DISTINCT own.resource_type, own.resource_id
    FROM events own
    WHERE own.cloud = $1 AND own.project_id = $2
      AND own.timestamp < sqlc.arg('to_ts')::timestamptz
) touched ON touched.resource_type = e.resource_type
         AND touched.resource_id = e.resource_id
WHERE e.cloud = $1 AND e.timestamp < sqlc.arg('to_ts')::timestamptz
ORDER BY e.timestamp, e.received_at, e.event_id
LIMIT sqlc.arg('page_size');

-- What the project summary calls active now: per resource type, the resources
-- the projection holds for the project as it stands. It counts the present
-- rather than the window, so a resource that was transferred away counts for its
-- new owner and never here, whatever the window saw it do under this project. A
-- deleted resource keeps its projection row, so it is excluded by its state
-- rather than by its absence.
-- name: CountProjectResourcesByType :many
SELECT resource_type, count(*) AS resources
FROM current_resources
WHERE cloud = $1 AND project_id = $2 AND state <> 'deleted'
GROUP BY resource_type;

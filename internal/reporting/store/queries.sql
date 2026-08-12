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

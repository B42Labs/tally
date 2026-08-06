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

-- name: ListEventsForResource :many
SELECT event_id, timestamp, received_at, event_type, platform, cloud,
       resource_type, resource_id, project_id, source, payload
FROM events
WHERE cloud = $1 AND resource_type = $2 AND resource_id = $3
ORDER BY timestamp, received_at, event_id;

-- name: ListResourceKeys :many
SELECT DISTINCT cloud, resource_type, resource_id
FROM events
WHERE (sqlc.narg('cloud')::text IS NULL OR cloud = sqlc.narg('cloud'))
  AND (sqlc.narg('resource_type')::text IS NULL OR resource_type = sqlc.narg('resource_type'))
ORDER BY cloud, resource_type, resource_id;

-- name: LockResource :exec
SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':' || $2::text || ':' || $3::text, 0));

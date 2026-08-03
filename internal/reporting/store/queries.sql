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

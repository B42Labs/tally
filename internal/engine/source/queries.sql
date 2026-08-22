-- The first statement of the REPEATABLE READ transaction a run opens. Reading
-- now() takes the snapshot every later query in that transaction sees, and the
-- value it returns is that snapshot's time, which the run records in
-- runs.stats.snapshot_at.
-- name: SnapshotTime :one
SELECT now()::timestamptz AS snapshot_at;

-- The resources a period can bill. The projection is the index: its rows are
-- never deleted, so a resource that lived during the period is still there to
-- be found, whatever happened to it since. A NULL cloud list means every cloud.
-- name: ListCandidates :many
SELECT cloud, platform, resource_type, resource_id
FROM current_resources
WHERE (sqlc.narg('clouds')::text[] IS NULL
       OR cloud = ANY(sqlc.narg('clouds')::text[]))
  AND (deleted_at IS NULL OR deleted_at >= sqlc.arg('period_from')::timestamptz)
  AND (created_at IS NULL OR created_at < sqlc.arg('period_to')::timestamptz)
ORDER BY cloud, resource_type, resource_id;

-- The full history of one candidate up to the period end. The fold starts at
-- the first event there is, because the state the resource holds at the period
-- start is whatever the events before it left behind.
-- name: ListHistory :many
SELECT event_id, timestamp, received_at, event_type, platform, cloud,
       resource_type, resource_id, project_id, source, payload
FROM events
WHERE cloud = $1 AND resource_type = $2 AND resource_id = $3
  AND timestamp < sqlc.arg('period_to')::timestamptz
ORDER BY timestamp, received_at, event_id;

-- The project registry, read whole: attribution walks the graph over these
-- UUIDs, and the resources it walks from carry the external id rather than the
-- registry's.
-- name: ListProjects :many
SELECT id, platform, cloud, external_id
FROM projects
ORDER BY cloud, external_id;

-- The relations that apply to a period, which are the ones whose validity
-- overlaps it (D4). A relation closed inside the period still carries the cost
-- the period produced before it closed; v1 does not prorate a relation within a
-- period.
-- name: ListRelationsOverlapping :many
SELECT id, source_id, target_id, relation_type, valid_from, valid_to
FROM project_relations
WHERE relation_type = ANY(sqlc.arg('relation_types')::text[])
  AND valid_from < sqlc.arg('period_to')::timestamptz
  AND (valid_to IS NULL OR valid_to > sqlc.arg('period_from')::timestamptz)
ORDER BY id;

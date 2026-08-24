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

-- The events of one type a counter measures inside one usage interval. The
-- interval is half-open, [from_ts, to_ts), the same bound the interval itself
-- has, and the key carries the cloud and the resource type beside the id
-- because an id is unique only within both. idx_events_resource serves the
-- predicate on uncompressed chunks; a chunk past the 90-day compression policy
-- is segmented by (cloud, resource_type) alone, so the resource id and the
-- event type are filtered after decompression, which is what a correction of an
-- older period pays.
-- name: CountEvents :one
SELECT count(*)
FROM events
WHERE cloud = $1 AND resource_type = $2 AND resource_id = $3
  AND event_type = sqlc.arg('event_type')
  AND timestamp >= sqlc.arg('from_ts')::timestamptz
  AND timestamp < sqlc.arg('to_ts')::timestamptz;

-- The late events of one billing period, the detection of WP 3.9
-- (roadmap/03-phase-3-metering-rating.md, under "Late-event detection"): the
-- events dated inside the period that were received strictly after the instant
-- the caller passes, the runs.stats.snapshot_at of the finalized run, grouped
-- per resource. The range over timestamp is answered by chunk exclusion, and
-- the lower bound on received_at by idx_events_received (reporting migration
-- 0009) on the uncompressed chunks; a chunk past the 90-day compression policy
-- is segmented by (cloud, resource_type) and ordered by nothing, so its batches
-- carry no bounds on received_at and are filtered after decompression, which is
-- what detecting the late events of an older period pays.
--
-- The groups are bounded by max_resources. A period finalized before its ingest
-- had settled has one group per resource of the fleet, and what a caller does
-- with the groups is print one line each. The groups the limit cuts off are
-- counted all the same: resources is how many arrived late whatever the limit
-- let through, and every row carries it, so a caller that read one row knows
-- how many it did not.
--
-- The strict comparison leaves one window open, and the issue holds it as a
-- Non-Goal: an ingest transaction that began before the snapshot and committed
-- after it carries a received_at below snapshot_at, so its events are not
-- listed here although a correction of the period re-meters them. Ingest
-- transactions are milliseconds long, and the window is recorded here rather
-- than papered over with a margin.
-- name: ListLateEvents :many
WITH late AS (
    SELECT cloud, platform, resource_type, resource_id,
           count(*)::bigint AS events, max(received_at)::timestamptz AS last_received_at
    FROM events
    WHERE timestamp >= sqlc.arg('period_from')::timestamptz
      AND timestamp < sqlc.arg('period_to')::timestamptz
      AND received_at > sqlc.arg('since')::timestamptz
    GROUP BY cloud, platform, resource_type, resource_id
)
SELECT cloud, platform, resource_type, resource_id, events, last_received_at,
       (count(*) OVER ())::bigint AS resources
FROM late
ORDER BY cloud, platform, resource_type, resource_id
LIMIT sqlc.arg('max_resources')::bigint;

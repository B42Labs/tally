-- The period list of the overview page: what months exist, where each one
-- stands, and which run closed the finalized ones.
-- name: ListPeriods :many
SELECT period_from, period_to, status, finalized_run_id, finalized_at
FROM billing_periods
ORDER BY period_from DESC;

-- The run list of the overview page, newest first and bounded by the caller.
-- name: ListRuns :many
SELECT id, period_from, period_to, kind, corrects_run_id, pricing_version,
       status, clouds, stats, started_at, completed_at
FROM runs
ORDER BY started_at DESC
LIMIT $1;

-- The header of a run page: the eleven columns a run carries.
-- name: GetRun :one
SELECT id, period_from, period_to, kind, corrects_run_id, pricing_version,
       status, clouds, stats, started_at, completed_at
FROM runs
WHERE id = $1;

-- The statement table of a run page, largest total first. The project key
-- breaks a tie between two equal totals, so the page renders one order.
-- name: ListStatements :many
SELECT project_id, total, currency
FROM project_statements
WHERE run_id = $1
ORDER BY total DESC, project_id;

-- The statement page: the stored document and the total it sums to.
-- name: GetStatement :one
SELECT document, total, currency
FROM project_statements
WHERE run_id = $1 AND project_id = $2;

-- The history of one project across runs, oldest month first.
-- name: ListStatementsForProject :many
SELECT s.run_id, r.period_from, r.kind, r.status, s.total, s.currency
FROM project_statements s
JOIN runs r ON r.id = s.run_id
WHERE s.project_id = $1
ORDER BY r.period_from, r.started_at;

-- The pricing page's version list, the version in force last first.
-- name: ListPricingModels :many
SELECT version, valid_from, currency, imported_at
FROM pricing_models
ORDER BY valid_from DESC;

-- One pricing version with the catalog document the pricing page renders.
-- name: GetPricingModel :one
SELECT version, valid_from, currency, imported_at, document
FROM pricing_models
WHERE version = $1;

-- The run a resource page defaults to: the newest run that metered that
-- resource at all.
-- name: LatestRunWithResource :one
SELECT r.id
FROM runs r
WHERE EXISTS (
    SELECT 1 FROM usage_records u
    WHERE u.run_id = r.id AND u.cloud = $1 AND u.resource_type = $2 AND u.resource_id = $3
)
ORDER BY r.started_at DESC
LIMIT 1;

-- The timeline of a resource page: every metered segment of one run with the
-- amounts it was rated at.
-- name: ListResourceSegments :many
SELECT u.state, u.from_ts, u.to_ts, u.seconds, u.usage,
       r.dimension, r.amount, r.currency
FROM rated_records r
JOIN usage_records u ON u.id = r.usage_record_id
WHERE r.run_id = $1 AND u.cloud = $2 AND u.resource_type = $3 AND u.resource_id = $4
ORDER BY u.from_ts, r.dimension;

-- The delta table of a correction run page: what the correction moved, per key.
-- name: ListCorrectionDeltas :many
SELECT cloud, platform, resource_type, resource_id, project_id, dimension,
       old_amount, new_amount, delta, currency
FROM correction_deltas
WHERE run_id = $1
ORDER BY cloud, platform, resource_type, resource_id, project_id, dimension;

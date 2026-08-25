-- name: ListBillingPeriods :many
SELECT period_from, status, finalized_run_id, finalized_at
FROM billing_periods
ORDER BY period_from;

-- name: InsertPricingModel :execrows
INSERT INTO pricing_models (version, valid_from, currency, document)
VALUES ($1, $2, $3, $4)
ON CONFLICT (version) DO NOTHING;

-- name: GetPricingModel :one
SELECT version, valid_from, currency, document, imported_at
FROM pricing_models
WHERE version = $1;

-- name: ListPricingModels :many
SELECT version, valid_from, currency, imported_at
FROM pricing_models
ORDER BY valid_from;

-- name: PricingModelForPeriod :one
SELECT version, valid_from, currency, document, imported_at
FROM pricing_models
WHERE valid_from <= $1
ORDER BY valid_from DESC
LIMIT 1;

-- name: InsertProjectStatement :exec
INSERT INTO project_statements (run_id, project_id, document, total, currency)
VALUES ($1, $2, $3, $4, $5);

-- name: ListProjectStatements :many
SELECT id, run_id, project_id, document, total, currency
FROM project_statements
WHERE run_id = $1
ORDER BY project_id;

-- One process at a time meters a period. The lock is an advisory one rather
-- than a row lock, because it is taken before the billing_periods row of the
-- period exists. Its key is hashed from the period start as text, which the
-- caller renders in UTC and RFC 3339, so that the same instant always reaches
-- the same key: a timestamptz argument would be rendered by the server and the
-- key would follow that session's DateStyle and TimeZone.
-- name: TryLockPeriod :one
SELECT pg_try_advisory_lock(hashtextextended('period:' || sqlc.arg(period_from)::text, 0));

-- Releases what TryLockPeriod took. An advisory lock is released by key, so the
-- key expression has to stay identical to the one above.
-- name: UnlockPeriod :one
SELECT pg_advisory_unlock(hashtextextended('period:' || sqlc.arg(period_from)::text, 0));

-- Records that a period exists. The scheduler walks the months it is
-- responsible for and calls this for each of them, so a period already known
-- keeps the status and the finalization columns it has.
-- name: UpsertBillingPeriod :exec
INSERT INTO billing_periods (period_from, period_to)
VALUES ($1, $2)
ON CONFLICT (period_from) DO NOTHING;

-- name: GetBillingPeriod :one
SELECT period_from, period_to, status, finalized_run_id, finalized_at
FROM billing_periods
WHERE period_from = $1;

-- name: SetBillingPeriodStatus :exec
UPDATE billing_periods
SET status = $2
WHERE period_from = $1;

-- Closes a period. The three columns move in one statement because the check
-- constraint on billing_periods ties them together: a period is finalized
-- exactly when it names the run that closed it and the time that happened.
-- name: FinalizeBillingPeriod :exec
UPDATE billing_periods
SET status = 'finalized', finalized_run_id = $2, finalized_at = now()
WHERE period_from = $1;

-- The first period the engine knows, where the scheduler starts its walk. The
-- aggregate answers over an empty table too, with one NULL row.
-- name: EarliestBillingPeriod :one
SELECT min(period_from)::timestamptz FROM billing_periods;

-- Opens a run. status stays at the column default 'running' and completed_at
-- stays null until the run ends. The caller writes its records under the
-- returned id and reports the returned start time. A regular run binds NULL for
-- corrects_run_id, which is an invalid pgtype.UUID; a correction binds the run
-- it corrects.
-- name: InsertRun :one
INSERT INTO runs (period_from, period_to, kind, corrects_run_id, pricing_version, clouds)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, started_at;

-- The first statement of a transaction that writes records. The trigger on the
-- record tables locks the run FOR SHARE for every row it checks, and a
-- transaction holding that lock which then updates the same run row has to
-- escalate to FOR NO KEY UPDATE, which deadlocks two writers of one run.
-- Taking the stronger lock up front is the ordering the comment on
-- forbid_finalized_mutation in migration 0001 asks for.
-- name: LockRun :one
SELECT id FROM runs WHERE id = $1 FOR NO KEY UPDATE;

-- Reads a run under that same lock, which is how finalization sees a status
-- that no concurrent writer can move underneath it.
-- name: GetRunForUpdate :one
SELECT id, period_from, period_to, kind, corrects_run_id, pricing_version,
       status, clouds, stats, started_at, completed_at
FROM runs
WHERE id = $1
FOR NO KEY UPDATE;

-- The same eleven columns without a FOR clause. An export only reads a run, so
-- it takes no row lock: the FOR NO KEY UPDATE above would queue the export
-- behind a run being finalized, and hold that finalization up in turn.
-- name: GetRun :one
SELECT id, period_from, period_to, kind, corrects_run_id, pricing_version,
       status, clouds, stats, started_at, completed_at
FROM runs
WHERE id = $1;

-- The three ends of a run. stats carries what the run counted, and the failed
-- one keeps it: a run that broke halfway is read for how far it got.
--
-- The two ends of a run in flight name the status they move from and report how
-- many rows they moved. A run this process opened can have been failed
-- underneath it by the reclaim below, and putting it back would return a run
-- the period has already replaced to that period's numbers: trg_runs_immutable
-- fires on OLD.status = 'finalized' alone, so 'failed' -> 'completed' passes the
-- database untouched. The caller reads the count and refuses rather than
-- writing over a status another process settled.
-- name: CompleteRun :execrows
UPDATE runs
SET status = 'completed', completed_at = now(), stats = $2
WHERE id = $1 AND status = 'running';

-- name: FailRun :execrows
UPDATE runs
SET status = 'failed', completed_at = now(), stats = $2
WHERE id = $1 AND status = 'running';

-- name: FinalizeRun :exec
UPDATE runs
SET status = 'finalized'
WHERE id = $1;

-- Retires the completed runs of the given kind that a new run of that kind
-- replaces, so that a period carries at most one completed run per kind and a
-- sum over that kind counts one set of records. It runs inside the new run's
-- write transaction while that run is still 'running', so the filter on
-- 'completed' never matches the run doing the superseding.
-- name: SupersedeCompletedRuns :many
UPDATE runs
SET status = 'superseded'
WHERE period_from = $1 AND kind = $2 AND status = 'completed'
RETURNING id;

-- Reclaims the runs of a period that are still in flight with no process
-- behind them. A killed process leaves its run at 'running' forever, and the
-- period is blocked for as long as that row reads as a run in progress. The
-- reason is merged into stats rather than written over it, so whatever the run
-- had already counted survives.
--
-- Age is what stands for the missing process, and the period lock cannot: that
-- lock is session scoped on one pooled connection which stays protocol-idle for
-- the whole run, so anything that closes only that connection -- an idle
-- timeout, a reaper, a failover -- releases it while the process keeps metering.
-- Two hours is longer than a run that is still alive: the CronJob's Job is
-- killed after fifty minutes (activeDeadlineSeconds), and a month an operator
-- meters by hand is minutes of work. Waiting costs nothing, because no query
-- counts a 'running' row.
-- name: ReclaimStaleRuns :many
UPDATE runs
SET status = 'failed',
    completed_at = now(),
    stats = stats || jsonb_build_object('error', 'the run''s process ended without completing it')
WHERE period_from = $1
  AND status = 'running'
  AND started_at < now() - interval '2 hours'
RETURNING id;

-- Whether the period already has a regular run that got to the end. A
-- finalized run counts as one: it is a completed run that closed its period.
-- name: HasCompleteRegularRun :one
SELECT EXISTS (
    SELECT 1 FROM runs
    WHERE period_from = $1 AND kind = 'regular' AND status IN ('completed', 'finalized')
);

-- How often the metering of a period failed in a row: the failed regular runs it
-- carries since the last one that got to the end, and when the last of them
-- stopped. Without this the tick meters a month again on every hourly pass for
-- as long as it keeps failing, and a month that never stops failing -- an event
-- ordering its history carries and no later event repairs -- costs a full
-- metering pass and another stats blob every hour for the life of the
-- deployment. The scheduler spaces its retries out over this count.
-- name: ConsecutiveFailedRuns :one
WITH last_success AS (
    SELECT coalesce(max(started_at), '-infinity'::timestamptz) AS started_at
    FROM runs
    WHERE period_from = $1 AND kind = 'regular'
      AND status IN ('completed', 'finalized', 'superseded')
)
SELECT count(*)::int AS failures, max(runs.completed_at)::timestamptz AS last_failure
FROM runs, last_success
WHERE runs.period_from = $1 AND runs.kind = 'regular' AND runs.status = 'failed'
  AND runs.started_at > last_success.started_at;

-- The run a period's current numbers come from. Superseding leaves at most one
-- completed regular run per period, and the ordering settles which one a reader
-- takes if it ever sees more.
-- name: LatestCompletedRegularRun :one
SELECT id FROM runs
WHERE period_from = $1 AND kind = 'regular' AND status = 'completed'
ORDER BY started_at DESC
LIMIT 1;

-- The latest finalized truth of a period, which a correction diffs against and
-- then names in corrects_run_id: the regular run that closed the period for
-- the first correction, the last finalized correction after that (roadmap
-- WP 3.9, item 5). detect-late reads the snapshot time out of its stats.
-- name: LatestFinalizedRun :one
SELECT id, kind, corrects_run_id, pricing_version, clouds, stats, started_at
FROM runs
WHERE period_from = $1 AND status = 'finalized'
ORDER BY started_at DESC
LIMIT 1;

-- The amounts of one run summed per the key a correction diffs by (decision D6:
-- cloud, platform, resource type, resource id, project id, dimension). The
-- currency is grouped too, so a run rated in one currency yields one row per
-- key. idx_rated_run serves the filter.
-- name: SumRatedByRun :many
SELECT u.cloud, u.platform, u.resource_type, u.resource_id, u.project_id, r.dimension,
       sum(r.amount)::numeric AS amount, r.currency
FROM rated_records r
JOIN usage_records u ON u.id = r.usage_record_id
WHERE r.run_id = $1
GROUP BY u.cloud, u.platform, u.resource_type, u.resource_id, u.project_id, r.dimension, r.currency
ORDER BY u.cloud, u.platform, u.resource_type, u.resource_id, u.project_id, r.dimension;

-- One row per rated record of a run, joined to the usage record it rates. The
-- ordering is a total one: the drafts of one resource never overlap (draftsOf
-- in internal/engine/metering/metering.go builds them from the intervals of a
-- folded history), so from_ts is unique per resource within a run, and rating
-- emits one record per usage record and dimension. That total order is what
-- makes two exports of one run byte-identical. idx_rated_run serves the filter.
-- name: ListRatedRecords :many
SELECT u.cloud, u.platform, u.resource_type, u.resource_id, u.project_id, u.state,
       u.from_ts, u.to_ts, u.usage, r.dimension, r.amount, r.currency
FROM rated_records r
JOIN usage_records u ON u.id = r.usage_record_id
WHERE r.run_id = $1
ORDER BY u.cloud, u.platform, u.resource_type, u.resource_id, u.from_ts, r.dimension;

-- The delta rows of a correction run, in the order corrections.Diff sorts by
-- (internal/engine/corrections/corrections.go), so an export prints them as the
-- correction computed them. idx_delta_run serves the filter.
-- name: ListCorrectionDeltas :many
SELECT cloud, platform, resource_type, resource_id, project_id, dimension,
       old_amount, new_amount, delta, currency
FROM correction_deltas
WHERE run_id = $1
ORDER BY cloud, platform, resource_type, resource_id, project_id, dimension;

-- The record writes of a run, over the COPY protocol: a month of metering is
-- tens of thousands of rows, and one round trip per row is what that costs.
-- All three name id rather than leaving it to the column default, because COPY
-- evaluates no defaults. The caller generating the ids is also what lets a
-- rated record name the usage record it was rated from before either row is
-- written.
-- name: CreateUsageRecords :copyfrom
INSERT INTO usage_records (id, run_id, cloud, platform, resource_type, resource_id,
                           project_id, state, from_ts, to_ts, seconds, usage)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- name: CreateRatedRecords :copyfrom
INSERT INTO rated_records (id, run_id, usage_record_id, dimension, amount, currency)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: CreateCorrectionDeltas :copyfrom
INSERT INTO correction_deltas (id, run_id, corrects_run_id, cloud, platform, resource_type, resource_id,
                               project_id, dimension, old_amount, new_amount, delta, currency)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

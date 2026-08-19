-- The index the resource statistics are counted through. Their query groups by
-- (cloud, platform, resource_type, state, project_id) over the whole table, and
-- none of the access paths current_resources has leads with all five:
-- idx_current_resources_fleet from 0005 carries the first four in another order
-- but not project_id, the primary key is (cloud, resource_type, resource_id),
-- idx_current_resources_project indexes project_id alone, and
-- idx_current_resources_type indexes (resource_type, state). The planner is left
-- with a sequential scan of the heap plus a hash aggregate, and that heap is
-- wide — size and last_payload are JSONB on every row — and grows monotonically,
-- because a deleted resource keeps its row (0001_init.sql). GET
-- /api/v1/stats/resources runs that shape once per request, on demand, rather
-- than on the metrics ticker 0005 was written for.
--
-- Adding project_id to the fleet index rather than building a second one is what
-- keeps the write side where 0005 left it: projection.Apply upserts
-- current_resources inside the ingest transaction, so every index over the
-- columns an update touches is maintained per folded event. The gauge 0005 built
-- its index for groups by (platform, cloud, resource_type, state), which is a
-- prefix of this one, so it keeps its index-only scan and the table carries one
-- btree where it would otherwise carry two.
--
-- The five columns are bounded by event.Validate at 512 bytes each, which leaves
-- the widest possible tuple inside the 2704 bytes a btree entry may take. 0005
-- repaired the rows a state above that bound could have left behind, so the
-- values this index is built over are already within it.
--
-- The normative specification is roadmap/02-phase-2-reporting-dashboards.md,
-- WP 2.1.
--
-- The build runs concurrently and therefore outside a transaction, for the
-- reason 0005 gives: a plain CREATE INDEX takes a SHARE lock on the table for
-- the length of the build, and SHARE conflicts with the ROW EXCLUSIVE an INSERT
-- needs, so it would stop ingestion until the index is there. The drop is
-- concurrent for the same reason.
--
-- Neither direction is a single statement and neither runs in a transaction, so
-- goose records the migration only once both statements have gone through: a
-- build that aborts, a lock_timeout on the drop, or a connection lost between
-- them leaves the chain recorded one version below with half the work done.
-- Every statement here is therefore written to be rerunnable. The drops say IF
-- EXISTS, and every build is preceded by a concurrent drop of the name it
-- builds, which clears whatever the failed run left behind: the INVALID index a
-- failed concurrent build leaves, which the planner ignores, or the finished one
-- a run that died after it never got to record. The rerun rebuilds the index
-- rather than recording the migration over it, which is what CREATE INDEX
-- CONCURRENTLY IF NOT EXISTS would do to the INVALID case, and it needs no
-- operator with DDL rights in the middle of a deploy.
--
-- The order matters in both directions: the index the last statement drops is
-- the one the statement before it made redundant, so no read is left without a
-- path at any point.

-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS idx_current_resources_stats;

CREATE INDEX CONCURRENTLY idx_current_resources_stats
    ON current_resources (platform, cloud, resource_type, state, project_id);

DROP INDEX CONCURRENTLY IF EXISTS idx_current_resources_fleet;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_current_resources_fleet;

CREATE INDEX CONCURRENTLY idx_current_resources_fleet
    ON current_resources (platform, cloud, resource_type, state);

DROP INDEX CONCURRENTLY IF EXISTS idx_current_resources_stats;

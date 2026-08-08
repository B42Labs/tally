-- The index the scoped event reads seek through. Every request from a
-- project-role token carries the (cloud, project_id) pair filter of WP 1.8, and
-- 0001 indexes project_id alone, which cannot serve a filter on the pair: the
-- planner is left with the time-dimension scan plus a per-row check, so the rows
-- it walks to fill one page grow with the share of the table the token does not
-- hold.
--
-- The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
-- WP 1.8.
--
-- The build runs per chunk and therefore outside a transaction. A plain CREATE
-- INDEX on a hypertable takes a SHARE lock on every chunk and holds all of them
-- until the last one is built, and SHARE conflicts with the ROW EXCLUSIVE an
-- INSERT needs: ingestion would stop for the length of the whole build, which is
-- longest on exactly the events table this index is worth having on. The
-- per-chunk form releases each chunk's lock when that chunk is done, so writes to
-- every other chunk keep flowing while it works.
--
-- A build that dies partway leaves the chunks it finished indexed and the
-- migration unrecorded, so a rerun fails on the index that is already there
-- rather than silently leaving the rest unindexed. Dropping the index and
-- rerunning is what recovers from that.
--
-- The directive is read per file rather than per direction, so the rollback runs
-- outside a transaction too: goose drops the index and records the rollback as
-- two separable steps, and a rollback that dies between them leaves the index
-- gone with the migration still recorded as applied. The drop says IF EXISTS so
-- that rerunning it is what repairs that, rather than failing on the index it
-- already dropped and leaving the version row to be edited by hand.

-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX idx_events_cloud_project ON events (cloud, project_id, timestamp)
    WITH (timescaledb.transaction_per_chunk);

-- +goose Down
DROP INDEX IF EXISTS idx_events_cloud_project;

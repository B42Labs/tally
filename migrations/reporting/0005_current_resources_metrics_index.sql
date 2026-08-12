-- The index the current-resources gauge is counted through. Its query groups by
-- (platform, cloud, resource_type, state) over the whole table, and none of the
-- access paths 0001 gives current_resources leads with platform: the primary key
-- is (cloud, resource_type, resource_id), idx_current_resources_project indexes
-- project_id, and idx_current_resources_type indexes (resource_type, state). The
-- planner is left with a sequential scan of the heap plus a hash aggregate, and
-- that heap is wide — size and last_payload are JSONB on every row — and grows
-- monotonically, because a deleted resource keeps its row (0001_init.sql). The
-- count runs on a ticker (TALLY_REPORTING_METRICS_REFRESH_S, default 60) against
-- the pool the request path shares, so the scan pulls the whole table through
-- shared buffers every minute and evicts the pages the queries need.
--
-- Leading with the grouping columns turns that into an index-only scan and a
-- grouped aggregate over four narrow text columns. It is paid for on the write
-- side: projection.Apply upserts current_resources inside the ingest
-- transaction and state is one of the columns an update touches, so every
-- folded event maintains this index too. idx_current_resources_type already
-- carries state for the same reason, which is what makes the added cost one
-- more btree of the same shape rather than a new kind of cost.
--
-- The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
-- WP 1.11.
--
-- The build runs concurrently and therefore outside a transaction. A plain
-- CREATE INDEX takes a SHARE lock on the table for the length of the build, and
-- SHARE conflicts with the ROW EXCLUSIVE an INSERT needs: current_resources is
-- written from inside the ingest transaction, so blocking it blocks every batch
-- until the index is there. Unlike events this is a plain table and not a
-- hypertable, so the concurrent form applies rather than the per-chunk one.
--
-- A concurrent build that fails leaves an INVALID index behind, which the
-- planner ignores. The statement does not say IF NOT EXISTS, so a rerun fails on
-- that leftover rather than recording the migration over an index nothing uses;
-- dropping it and rerunning is what recovers from that.
--
-- The repair ahead of the build is what keeps a rerun off that path to begin
-- with. event.Validate bounds payload.state to 512 bytes, but only for events
-- validated after that bound existed: a database that ran the chain up to 0004
-- could have taken a state of a few thousand characters, because the only index
-- over it then was idx_current_resources_type and a short resource_type left
-- room for one. This index puts platform and cloud in front of the same value,
-- so the tuple grows by their length and a state that fit before no longer
-- does. The build would abort on that one row and leave the INVALID index for
-- an operator to drop by hand. Truncating brings those rows to the bound the
-- code now enforces, which is where they would have been had it always existed.
-- A rebuild replaying the original event writes the untruncated state back, but
-- that path already failed the same way against idx_current_resources_type and
-- is not what this migration can answer.
--
-- The bound is 512 bytes and the truncation counts characters, which are not the
-- same unit: taking 512 bytes of text can cut a multi-byte character in half and
-- yield something the database refuses as invalid UTF-8. A UTF-8 character is at
-- most four bytes, so 128 characters is always within the bound.
--
-- The directive is read per file rather than per direction, so the rollback runs
-- outside a transaction too: goose drops the index and records the rollback as
-- two separable steps, and a rollback that dies between them leaves the index
-- gone with the migration still recorded as applied. The drop says IF EXISTS so
-- that rerunning it is what repairs that, rather than failing on the index it
-- already dropped and leaving the version row to be edited by hand. It is also
-- the drop that clears the INVALID leftover above, which the same rerun path
-- reaches.

-- +goose NO TRANSACTION
-- +goose Up
UPDATE current_resources
    SET state = left(state, 128)
    WHERE octet_length(state) > 512;

CREATE INDEX CONCURRENTLY idx_current_resources_fleet
    ON current_resources (platform, cloud, resource_type, state);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_current_resources_fleet;

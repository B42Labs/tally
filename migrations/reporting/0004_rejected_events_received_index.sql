-- The index the dead-letter list seeks through. It orders by (received_at, id)
-- and resumes a walk on that pair, and 0001 gives rejected_events its primary
-- key alone, so without this every page is a full scan of a TOAST-heavy JSONB
-- table plus a sort — page 100 costing what page 1 did. 0001 leaves the events
-- table's received_at unindexed on purpose, because nothing read it; this table
-- is written only when ingestion refuses an item, so the insert contention that
-- argument is about does not arise here.
--
-- The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
-- WP 1.8.
--
-- The build runs concurrently and therefore outside a transaction. A plain CREATE
-- INDEX takes a SHARE lock on the table for the length of the build, and SHARE
-- conflicts with the ROW EXCLUSIVE an INSERT needs: this table is written from
-- inside the ingest transaction, so blocking it blocks every batch carrying a
-- refused item until the index is there.
--
-- A concurrent build that fails leaves an INVALID index behind, which the planner
-- ignores. The statement does not say IF NOT EXISTS, so a rerun fails on that
-- leftover rather than recording the migration over an index nothing uses;
-- dropping it and rerunning is what recovers from that.
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
CREATE INDEX CONCURRENTLY idx_rejected_events_received ON rejected_events (received_at, id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS idx_rejected_events_received;

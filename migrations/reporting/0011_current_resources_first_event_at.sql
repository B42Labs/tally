-- When a resource was first seen, on the projection row that says what it is
-- now.
--
-- The row carries created_at, deleted_at and last_event_at, and none of them
-- answers that question for every resource. created_at is what the fold read off
-- a create event, so a history whose first event is not a create leaves it NULL:
-- the resource is in the projection, is billed, and carries no instant that
-- places it in time at all. Reading the events for it is the per-resource
-- lifecycle route, one round trip per resource, which a listing over the fleet
-- cannot pay.
--
-- first_event_at is the timestamp of the earliest event folded into the row. It
-- is set on every row, so a reader takes one column instead of branching on
-- whether created_at is NULL, and on a history that starts with a create the two
-- hold the same instant. It says when the resource entered the record, not what
-- anyone owes: what a period bills is the intervals the fold derives, and
-- ListCandidates in internal/engine/source/queries.sql goes on selecting on
-- created_at.
--
-- Like created_at, the column is not scoped to whoever reads it. The projection
-- is one row per resource whatever project owned it when each event arrived, so
-- on a resource that changed projects this is the first event of the project it
-- came from and predates the reader's ownership.
--
-- The backfill runs in two statements. The first takes the earliest timestamp
-- per key from the events table, which idx_events_resource from 0001 indexes as
-- (cloud, resource_type, resource_id, timestamp). It is an inner join, so it
-- reaches every row the history still explains and no other. The second fills
-- what is left with last_event_at. Nothing in this schema removes an event, so
-- the second statement is expected to match nothing on a deployment; it is there
-- because SET NOT NULL below refuses to run while one row is unfilled, and a
-- projection row whose history has gone is a row the chain must still get past.
-- last_event_at is NOT NULL and is itself the timestamp of an event that was
-- folded into the row, so it is the earliest instant such a row can still prove.
--
-- 0010 added its constraint NOT VALID to keep the ACCESS EXCLUSIVE off a scan of
-- the whole table. That reasoning does not carry here and the column is set NOT
-- NULL the ordinary way: the backfill above has already visited and rewritten
-- every row, so the scan SET NOT NULL runs is not what this migration costs, and
-- there is no way to add a column to every row without writing every row. What
-- this migration costs is that backfill, and on a deployment whose events are
-- past the 90-day compression policy of 0001 it costs the read of the compressed
-- chunks the aggregate has to open.
--
-- Both directions carry a lock_timeout for the reason 0010 gives: ALTER TABLE
-- takes ACCESS EXCLUSIVE on a table every request path reads, this chain runs
-- while the old binary is still serving, and Postgres queues every later lock
-- request behind a waiting ACCESS EXCLUSIVE. The bound turns an unbounded stall
-- into a migration that fails naming the lock and an operator who reruns it.
-- SET LOCAL dies with its transaction and goose runs each direction in one of
-- its own, so the rollback sets it again rather than inheriting it. The rollback
-- drops what it finds, the way 0008, 0009 and 0010 do, so that a column an
-- operator already dropped by hand does not leave version 11 recorded as applied
-- and goose_db_version to be edited.
--
-- This file takes the chain to version 11, which is the version a binary built
-- from this tree expects: migrations/reporting/embed.go carries it and the
-- Reporting API's readiness refuses a database on 10, so the pod stays out of
-- rotation until the chain is applied. Running tally-reporting-admin migrate is
-- part of the deploy that rolls a Reporting API image built from here, and it
-- goes first. In the dev cluster make up covers it, because it runs make
-- migrate.

-- +goose Up
SET LOCAL lock_timeout = '3s';

ALTER TABLE current_resources ADD COLUMN first_event_at TIMESTAMPTZ;

UPDATE current_resources cr
   SET first_event_at = e.first_at
  FROM (SELECT cloud, resource_type, resource_id, min(timestamp) AS first_at
          FROM events
         GROUP BY cloud, resource_type, resource_id) e
 WHERE cr.cloud = e.cloud
   AND cr.resource_type = e.resource_type
   AND cr.resource_id = e.resource_id;

UPDATE current_resources
   SET first_event_at = last_event_at
 WHERE first_event_at IS NULL;

ALTER TABLE current_resources ALTER COLUMN first_event_at SET NOT NULL;

-- +goose Down
SET LOCAL lock_timeout = '3s';

ALTER TABLE current_resources DROP COLUMN IF EXISTS first_event_at;

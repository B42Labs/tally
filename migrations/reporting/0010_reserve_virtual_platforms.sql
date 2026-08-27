-- The two virtual platforms of decision D1 (roadmap/05-phase-5-commercial-
-- pricing.md), made a rule of the schema rather than of the Go code alone.
--
-- A meta-project (platform "meta") and a partner (platform "partner") are
-- ordinary projects rows that own no resource and carry their platform as their
-- cloud, which is what keeps them inside UNIQUE (cloud, external_id) without a
-- table of their own. projects.Register refuses a registration that breaks
-- that, but every path this deployment offers an operator is not a
-- registration: tally-reporting-admin exists because operators hold direct
-- database access, and a hand-written INSERT would otherwise place a real
-- project inside the virtual namespace. The registry carries comparable
-- invariants in Postgres already -- CHECK (source_id <> target_id) on
-- project_relations, api_tokens_project_scope on api_tokens -- so this one
-- belongs there too.
--
-- The chain refuses to run against a database that already breaks the rule,
-- rather than adding the constraints and leaving the rows behind. Both
-- literals were free-form text in `platform` until this chain, so a deployment
-- may hold rows that were legitimate when they were written and are refused
-- from here on; UPDATE cannot be guessed for them, because only the operator
-- knows which real cloud a row belonged to. Failing here names the rows while
-- the old binary is still serving, which is the one moment they can be fixed
-- without an outage. The counts are what refuses them, and they run before any
-- lock stronger than ACCESS SHARE is taken, so the rows are named without the
-- tables being held.
--
-- current_resources stands in for the event history: its rows are never
-- removed, so a resource that was ever ingested under a virtual literal still
-- has one, and scanning it costs nothing next to a full scan of the compressed
-- events hypertable.
--
-- Counting those rows is not what keeps them away, though, so the projection
-- carries the rule as a constraint of its own. The count alone would guarantee
-- nothing: the old binary accepts a virtual cloud and goes on ingesting under
-- it until the new image is rolled, and the rows an operator renames to get
-- past the count are written back by the first projection rebuild, which
-- replays the events history rather than reading this table. Under the
-- constraint that rebuild leaves the key alone and ends naming it, instead of
-- restoring the row silently, which is what the count was reaching for: the
-- owner of a resource is resolved by (cloud, project_id), so a row under the
-- cloud "meta" bills a meta-project for a resource.
--
-- Both constraints are added under a lock_timeout, because ALTER TABLE ADD
-- CONSTRAINT needs ACCESS EXCLUSIVE on a table every request path reads and
-- this chain runs while the old binary is still serving. A lock request of that
-- strength queues behind whatever holds the table -- a metering run reads
-- projects inside one REPEATABLE READ transaction that lasts the whole run --
-- and Postgres puts every later request behind the queued ACCESS EXCLUSIVE, so
-- an unbounded wait here stalls authorization for the whole project-scoped API
-- rather than one statement. The bound turns that into a migration that fails
-- naming the lock and an operator who reruns it.
--
-- The bound is on the wait, though, not on the hold: once the lock is granted
-- it is held until this transaction commits, and a constraint added without NOT
-- VALID spends that time scanning the whole table for rows breaking it. On
-- current_resources -- one row per resource the fleet ever held, and no index on
-- either column -- that scan is the outage the lock_timeout was meant to avoid,
-- and it looks for rows the count above has already proved are not there. So the
-- projection's constraint is added NOT VALID: Postgres enforces it against every
-- later insert and update either way, and the ACCESS EXCLUSIVE is held for the
-- catalog write alone. projects carries one row per project and validates in the
-- time the catalog write takes, so it is added the ordinary way.
--
-- The rollback carries the lock_timeout again rather than inheriting it: SET
-- LOCAL dies with the transaction, and goose runs each direction in one of its
-- own. DROP CONSTRAINT needs ACCESS EXCLUSIVE exactly as ADD CONSTRAINT does,
-- and a rollback is run at the moment an unbounded wait for it can least be
-- afforded.
--
-- The rollback also drops what it finds. An operator who dropped a constraint by
-- hand to get an urgent write through would otherwise meet a rollback that dies
-- on the constraint that is already gone and leaves version 10 recorded as
-- applied, to be edited out of goose_db_version by hand; 0008 and 0009 guard
-- the same case in the same way.

-- +goose Up
SET LOCAL lock_timeout = '3s';

-- +goose StatementBegin
DO $$
DECLARE
    offending BIGINT;
BEGIN
    SELECT count(*) INTO offending FROM projects
     WHERE NOT (platform = cloud
                OR (platform NOT IN ('meta', 'partner')
                    AND cloud NOT IN ('meta', 'partner')));
    IF offending > 0 THEN
        RAISE EXCEPTION 'the projects table holds % row(s) breaking decision D1', offending
            USING HINT = 'a project whose platform or cloud is meta or partner must carry that'
                         ' literal in both columns; correct the rows reported by'
                         ' SELECT id, platform, cloud, external_id FROM projects WHERE NOT'
                         ' (platform = cloud OR (platform NOT IN (''meta'', ''partner'') AND'
                         ' cloud NOT IN (''meta'', ''partner'')))';
    END IF;

    SELECT count(*) INTO offending FROM current_resources
     WHERE platform IN ('meta', 'partner') OR cloud IN ('meta', 'partner');
    IF offending > 0 THEN
        RAISE EXCEPTION 'the current_resources table holds % row(s) under a virtual platform or cloud', offending
            USING HINT = 'meta and partner name projects that own no resource, and this version'
                         ' dead-letters every event carrying either as its platform or its'
                         ' cloud; point that collector at a real platform and cloud before'
                         ' migrating, or it goes silent. Renaming these rows is what lets the'
                         ' chain through, and it holds only until something rebuilds the'
                         ' projection: the events behind them keep the literal they were'
                         ' written with, so that rebuild leaves those keys stale and ends'
                         ' naming them until the events are gone';
    END IF;

    SELECT count(*) INTO offending FROM resource_types
     WHERE platform IN ('meta', 'partner');
    IF offending > 0 THEN
        RAISE EXCEPTION 'the resource_types table holds % row(s) under a virtual platform', offending
            USING HINT = 'meta and partner are reserved for projects that own no resource, so no'
                         ' resource type may be registered under either';
    END IF;
END $$;
-- +goose StatementEnd

ALTER TABLE projects ADD CONSTRAINT projects_virtual_key CHECK (
    platform = cloud
    OR (platform NOT IN ('meta', 'partner') AND cloud NOT IN ('meta', 'partner'))
);

ALTER TABLE current_resources ADD CONSTRAINT current_resources_virtual_key CHECK (
    platform NOT IN ('meta', 'partner') AND cloud NOT IN ('meta', 'partner')
) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '3s';

ALTER TABLE current_resources DROP CONSTRAINT IF EXISTS current_resources_virtual_key;
ALTER TABLE projects DROP CONSTRAINT IF EXISTS projects_virtual_key;

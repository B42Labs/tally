-- The role the metering engine reads this database through. Decision D1 has the
-- engine scan the event history over its own connection rather than over the
-- API, and it never writes here, so it gets SELECT on the four tables metering
-- reads and nothing else: not goose_db_version, and none of the tables that
-- carry credentials, tokens, or the audit log. A connection string that leaks
-- reads resource history and stops there.
--
-- tally_engine_reader is a group role without a login. A deployment creates the
-- role that actually connects and grants it membership:
--
--     CREATE ROLE <user> LOGIN PASSWORD '...';
--     GRANT tally_engine_reader TO <user>;
--
-- which is where the password belongs. A password written here would travel in
-- every image built from this tree and be the same in every deployment.
--
-- Roles live in the cluster rather than in one database, so this chain may meet
-- a role that is already there: a deployment provisions it ahead of the chain,
-- and the dev stack's initdb script
-- deploy/kubernetes/base/timescaledb/02-create-engine-reader.sh creates it so
-- the login role can be made a member before the chain runs. Reading pg_roles
-- first and creating the role only when it is missing would leave the two
-- steps racing: goose serializes its runs with an advisory lock, but that lock
-- carries the database it was taken in while roles carry none, so a chain
-- against another database on the same server is not held against this one.
-- Creating the role and treating the role that is already there as success
-- covers the race and the deployment that got there first alike. That takes
-- two error codes, because the two ways of arriving there are reported
-- differently: CREATE ROLE looks the name up in the catalogs first and reports
-- duplicate_object when it finds it, and a session that passed that lookup
-- before the other one committed reaches the unique index over pg_authid and
-- fails on it with unique_violation. Nothing serializes the two, so the second
-- code is the one a genuine race raises.
--
-- What no guard here covers is a migrating role without CREATEROLE: it may not
-- create the role at all, and this chain then fails naming the statement to run
-- beforehand. A deployment that hardens the migrating role that far has to
-- provision tally_engine_reader itself.
--
-- The grant on events reaches the hypertable's chunks: TimescaleDB propagates
-- it to them, including the chunks written after this migration.
--
-- The rollback revokes and keeps the role. Another database on the same server
-- may hold memberships in it, which dropping it would take with it, and
-- re-applying the Up direction succeeds on the role that is still there. The
-- revoke runs only while pg_roles still holds the role, so a rollback on a
-- server where someone dropped it by hand succeeds instead of dying on a role
-- that is no longer there.
--
-- The normative specification is roadmap/03-phase-3-metering-rating.md,
-- decision D1 and WP 3.2.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    CREATE ROLE tally_engine_reader NOLOGIN;
EXCEPTION
    WHEN duplicate_object OR unique_violation THEN
        NULL;  -- a deployment, or a chain against another database, got there first
    WHEN insufficient_privilege THEN
        RAISE EXCEPTION 'creating the role tally_engine_reader: %', SQLERRM
            USING HINT = 'create the role beforehand with CREATE ROLE tally_engine_reader NOLOGIN,'
                         ' or grant CREATEROLE to the migrating role';
END $$;
-- +goose StatementEnd

GRANT SELECT ON events, current_resources, projects, project_relations TO tally_engine_reader;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tally_engine_reader') THEN
        REVOKE SELECT ON events, current_resources, projects, project_relations FROM tally_engine_reader;
    END IF;
END $$;
-- +goose StatementEnd

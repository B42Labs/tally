-- The role the console reads this database through. tally-console renders what
-- the engine wrote and never writes here, so it gets SELECT on the eight tables
-- of this chain and nothing else: not goose_db_version_engine, and no right to
-- insert, update, delete or truncate. A connection string that leaks reads
-- billing data and stops there.
--
-- tally_console_reader is a group role without a login. The role that actually
-- connects is tally_console, which tally-engine ensure-console-role creates,
-- gives its password and makes a member of this one:
--
--     CREATE ROLE tally_console;
--     ALTER ROLE tally_console WITH LOGIN PASSWORD '...';
--     GRANT tally_console_reader TO tally_console;
--
-- which is where the password belongs. A password written here would travel in
-- every image built from this tree and be the same in every deployment.
--
-- Roles live in the server rather than in one database, so this chain may meet
-- a role that is already there: a chain against another database on the same
-- server created it first. goose serializes its runs with an advisory lock, but
-- that lock carries the database it was taken in while roles carry none, so a
-- chain against another database is not held against this one. Creating the
-- role and treating the role that is already there as success covers that
-- race. It takes two error codes, because the two ways of arriving there are
-- reported differently: CREATE ROLE looks the name up in the catalogs first and
-- reports duplicate_object when it finds it, and a session that passed that
-- lookup before the other one committed reaches the unique index over pg_authid
-- and fails on it with unique_violation.
--
-- A migrating role without CREATEROLE may not create the role, and CREATE ROLE
-- checks that before it looks the name up, so it fails on a role that is there
-- as well. The Up direction therefore reads pg_roles first and creates the
-- role only when it is missing: a role made beforehand passes, and a missing
-- one fails naming the statement to run beforehand. The read narrows the race
-- above without closing it, which the two error codes still do.
-- tally-engine ensure-console-role needs more than this chain: CREATEROLE, and
-- ADMIN OPTION on this role when someone else created it.
--
-- The grant names the tables one by one. A later migration that adds a table
-- the console reads adds the grant with it; the console store's integration
-- tests connect as tally_console, so a missing grant fails there.
--
-- The rollback revokes and keeps the role. Another database on the same server
-- may hold memberships in it, tally_console's among them, which dropping it
-- would take with it, and re-applying the Up direction succeeds on the role
-- that is still there. The revoke runs only while pg_roles still holds the
-- role, so a rollback on a server where someone dropped it by hand succeeds
-- instead of dying on a role that is no longer there.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tally_console_reader') THEN
        CREATE ROLE tally_console_reader NOLOGIN;
    END IF;
EXCEPTION
    WHEN duplicate_object OR unique_violation THEN
        NULL;  -- a chain against another database on this server got there first
    WHEN insufficient_privilege THEN
        RAISE EXCEPTION 'creating the role tally_console_reader: %', SQLERRM
            USING HINT = 'create the role beforehand with CREATE ROLE tally_console_reader NOLOGIN,'
                         ' or grant CREATEROLE to the migrating role;'
                         ' tally-engine ensure-console-role needs CREATEROLE either way';
END $$;
-- +goose StatementEnd

GRANT SELECT ON adjustment_records, billing_periods, correction_deltas, pricing_models,
    project_statements, rated_records, runs, usage_records TO tally_console_reader;

-- +goose Down
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'tally_console_reader') THEN
        REVOKE SELECT ON adjustment_records, billing_periods, correction_deltas, pricing_models,
            project_statements, rated_records, runs, usage_records FROM tally_console_reader;
    END IF;
END $$;
-- +goose StatementEnd

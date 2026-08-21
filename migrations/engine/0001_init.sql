-- The complete Phase 3 schema of the metering engine: billing periods, the runs
-- that meter and rate them, the usage, rated, delta and statement records a run
-- produces, the versioned pricing models, and the triggers that hold a finalized
-- run's records immutable.
--
-- The normative specification is roadmap/03-phase-3-metering-rating.md, the
-- engine database schema block and WP 3.1.
--
-- Some things here go past the roadmap's literal SQL. All of them are sanctioned
-- by issue #34 and named here rather than applied silently, per guardrail 10 of
-- roadmap/00-conventions.md:
--   1. The roadmap leaves the runs trigger as a trailing prose note. It is
--      concrete below as forbid_finalized_run_mutation, guarded by
--      WHEN (OLD.status = 'finalized') so that the one legal transition stays
--      open: finalizing updates a row whose status still reads 'completed'.
--      The shared forbid_finalized_mutation cannot serve on runs, because it
--      resolves the run through a run_id column that runs does not have.
--   2. correction_deltas carries a run_id and belongs to a correction run that
--      can itself be finalized, so it gets the same record trigger as the three
--      tables the roadmap lists.
--   3. The record triggers also fire on INSERT. Guardrail 7 holds a finalized
--      run immutable, and what a run bills is the sum over its records: adding
--      rows to a finalized run changes that sum exactly as changing a row would.
--      No legitimate flow needs it open, because a correction run writes its
--      rows under its own run_id, which is not finalized while it writes them.
--   4. billing_periods carries the integrity its columns imply: the run that
--      closed a period is a run that exists, and the three finalization columns
--      move together.

-- +goose Up
CREATE TABLE billing_periods (
    period_from  TIMESTAMPTZ PRIMARY KEY,          -- e.g. 2026-03-01T00:00:00Z
    period_to    TIMESTAMPTZ NOT NULL,             -- exclusive
    status       TEXT NOT NULL DEFAULT 'open',     -- 'open' | 'grace' | 'finalized'
    finalized_run_id UUID,
    finalized_at TIMESTAMPTZ,
    CHECK (period_from < period_to),
    -- A finalized period names the run that closed it and when that happened,
    -- and only a finalized one does. Without this, a status put back to 'open'
    -- over a closed month reads as a month that was never billed.
    CHECK ((status = 'finalized') = (finalized_run_id IS NOT NULL AND finalized_at IS NOT NULL))
);

CREATE TABLE runs (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    period_from     TIMESTAMPTZ NOT NULL,
    period_to       TIMESTAMPTZ NOT NULL,
    kind            TEXT NOT NULL DEFAULT 'regular',   -- 'regular' | 'correction'
    corrects_run_id UUID REFERENCES runs(id),          -- set for corrections
    pricing_version TEXT,
    status          TEXT NOT NULL DEFAULT 'running',
        -- 'running' | 'completed' | 'finalized' | 'superseded' | 'failed'
    clouds          TEXT[] NOT NULL DEFAULT '{}',      -- empty = all configured clouds
    stats           JSONB NOT NULL DEFAULT '{}',
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at    TIMESTAMPTZ
);
CREATE INDEX idx_runs_period ON runs (period_from, status);
-- Only one regular run per period may be in flight: two of them would each leave
-- a full set of records over the same month, and every sum over those records
-- would count both. That rule is not a unique index over the in-flight statuses
-- here, because such an index has no way back. A run whose process is killed
-- keeps its 'running' status with nothing behind it, and an OOMKill, a drained
-- node or a deleted pod are routine for the CronJob the run subcommand ships as.
-- The index would then reject every later attempt at that month, the operator's
-- retry and the hourly tick alike, until someone writes an UPDATE by hand
-- against the production billing database. Enforcing the rule and reclaiming a
-- stale run are one decision, and both belong to the run lifecycle of WP 3.8,
-- which is also what first writes a run row.

-- The run that closed a period has to be a run that exists: periods list prints
-- this id, and one that resolves to nothing is indistinguishable there from one
-- that does. The constraint is added here rather than on the column, because
-- billing_periods is created above the table it points at.
ALTER TABLE billing_periods ADD CONSTRAINT billing_periods_finalized_run_fkey
    FOREIGN KEY (finalized_run_id) REFERENCES runs(id);

CREATE TABLE usage_records (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id        UUID NOT NULL REFERENCES runs(id),
    cloud         TEXT NOT NULL,
    platform      TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL,
    project_id    TEXT NOT NULL,
    state         TEXT NOT NULL,
    from_ts       TIMESTAMPTZ NOT NULL,
    to_ts         TIMESTAMPTZ NOT NULL,
    seconds       BIGINT NOT NULL,                 -- exact; invariant checks use this
    usage         JSONB NOT NULL,                  -- {"minutes": 21600, "count": 1, "vcpus": 4, ...}
    CHECK (from_ts < to_ts)
);
CREATE INDEX idx_usage_run      ON usage_records (run_id);
CREATE INDEX idx_usage_resource ON usage_records (run_id, cloud, resource_type, resource_id, from_ts);
CREATE INDEX idx_usage_project  ON usage_records (run_id, project_id);

CREATE TABLE rated_records (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL REFERENCES runs(id),
    usage_record_id UUID NOT NULL REFERENCES usage_records(id),
    dimension       TEXT NOT NULL,                 -- usage key this cost refers to
    amount          NUMERIC(14,2) NOT NULL,        -- negative in correction runs
    currency        TEXT NOT NULL
);
CREATE INDEX idx_rated_run ON rated_records (run_id);

-- Delta records produced by correction runs (WP 3.9)
CREATE TABLE correction_deltas (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL REFERENCES runs(id),      -- the correction run
    corrects_run_id UUID NOT NULL REFERENCES runs(id),      -- the finalized run
    cloud           TEXT NOT NULL,
    platform        TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    dimension       TEXT NOT NULL,
    old_amount      NUMERIC(14,2) NOT NULL,
    new_amount      NUMERIC(14,2) NOT NULL,
    delta           NUMERIC(14,2) NOT NULL,        -- new - old; credit < 0, debit > 0
    currency        TEXT NOT NULL
);
-- Both columns are read as point lookups, by the export of a correction run and
-- by the chaining of one correction onto the last. PostgreSQL indexes neither
-- side of a foreign key on its own.
CREATE INDEX idx_delta_run      ON correction_deltas (run_id);
CREATE INDEX idx_delta_corrects ON correction_deltas (corrects_run_id);

CREATE TABLE pricing_models (
    version     TEXT PRIMARY KEY,                  -- e.g. '2026-03'
    valid_from  TIMESTAMPTZ NOT NULL UNIQUE,
    currency    TEXT NOT NULL,
    document    JSONB NOT NULL,                    -- the validated YAML as JSON
    imported_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE project_statements (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id     UUID NOT NULL REFERENCES runs(id),
    project_id TEXT NOT NULL,                      -- registry (cloud, external_id) rendered id
    document   JSONB NOT NULL,                     -- rating output doc (concept §3.4 format)
    total      NUMERIC(14,2) NOT NULL,
    currency   TEXT NOT NULL,
    UNIQUE (run_id, project_id)
);

-- D8: finalized runs are immutable, enforced in the database and not only in
-- application code (guardrail 7 of roadmap/00-conventions.md).
-- +goose StatementBegin
CREATE FUNCTION forbid_finalized_mutation() RETURNS trigger AS $$
DECLARE locked_run record;
BEGIN
    -- Both run ids are read. OLD is what keeps the records of a finalized run
    -- from being changed or removed, NEW what keeps records from being added to
    -- one, or moved onto one by an update that rewrites run_id. The side that
    -- does not belong to the operation is null, which IN skips.
    --
    -- The WHERE clause carries the ids and nothing else, and the status is read
    -- off the locked row below. FOR SHARE is what holds the answer against a
    -- finalize that commits between this read and the write, but only for rows
    -- that reach the lock: a status filter is applied underneath it, against
    -- this statement's own snapshot, so a run this transaction still sees as
    -- 'completed' would be dropped before anything is locked and the finalize
    -- would stay free to commit. Locking the row unqualified makes PostgreSQL
    -- wait that finalize out and hand back the version it committed. Every
    -- matched row is walked, because both ids are locked and either of them may
    -- be the finalized run.
    --
    -- The lock is taken on every record write, and FOR SHARE conflicts with the
    -- FOR NO KEY UPDATE that a plain UPDATE on runs takes. A transaction that
    -- writes records for a run and also updates that run's row, which the
    -- lifecycle of WP 3.8 does every time it carries the run's stats along,
    -- must therefore take the run row first, with
    --   SELECT id FROM runs WHERE id = $1 FOR NO KEY UPDATE
    -- before its first record write. Escalating mid-transaction instead
    -- deadlocks two writers of the same run: both hold FOR SHARE on it, both
    -- then ask for FOR NO KEY UPDATE, and PostgreSQL resolves that by aborting
    -- one of them with everything it had metered.
    FOR locked_run IN
        SELECT id, status FROM public.runs WHERE id IN (OLD.run_id, NEW.run_id) FOR SHARE
    LOOP
        IF locked_run.status = 'finalized' THEN
            RAISE EXCEPTION 'records of finalized run % are immutable', locked_run.id;
        END IF;
    END LOOP;
    RETURN COALESCE(NEW, OLD);
END $$ LANGUAGE plpgsql
-- The schema is pinned because a relation name is resolved in the caller's
-- session, and PostgreSQL searches that session's temporary schema first unless
-- pg_temp is named. An unqualified runs would otherwise read whatever table a
-- writer decides it means, which is CVE-2018-1058 and turns the guarantee below
-- into one anyone holding CREATE TEMPORARY can step around.
SET search_path = pg_catalog, public, pg_temp;
-- +goose StatementEnd

CREATE TRIGGER trg_usage_immutable  BEFORE INSERT OR UPDATE OR DELETE ON usage_records
    FOR EACH ROW EXECUTE FUNCTION forbid_finalized_mutation();
CREATE TRIGGER trg_rated_immutable  BEFORE INSERT OR UPDATE OR DELETE ON rated_records
    FOR EACH ROW EXECUTE FUNCTION forbid_finalized_mutation();
CREATE TRIGGER trg_stmt_immutable   BEFORE INSERT OR UPDATE OR DELETE ON project_statements
    FOR EACH ROW EXECUTE FUNCTION forbid_finalized_mutation();
-- A delta row names its correction run in run_id, and a correction run is
-- finalized like any other, so the same rule applies to it.
CREATE TRIGGER trg_delta_immutable  BEFORE INSERT OR UPDATE OR DELETE ON correction_deltas
    FOR EACH ROW EXECUTE FUNCTION forbid_finalized_mutation();

-- The run row itself. forbid_finalized_mutation reads the status through a
-- run_id column, which runs does not have, so the rule needs its own function
-- over OLD.id.
-- +goose StatementBegin
CREATE FUNCTION forbid_finalized_run_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'run % is finalized and immutable', OLD.id;
END $$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- The WHEN guard is what leaves the one legal transition open: the update that
-- finalizes a run reads a row whose status is still 'completed'.
CREATE TRIGGER trg_runs_immutable BEFORE UPDATE OR DELETE ON runs
    FOR EACH ROW WHEN (OLD.status = 'finalized') EXECUTE FUNCTION forbid_finalized_run_mutation();

-- +goose Down
DROP TABLE project_statements;
DROP TABLE correction_deltas;
DROP TABLE rated_records;
DROP TABLE usage_records;
DROP TABLE pricing_models;
-- Ahead of runs: billing_periods names the run that closed a period.
DROP TABLE billing_periods;
DROP TABLE runs;
-- Dropping a table takes its triggers with it, so only the functions are left.
DROP FUNCTION forbid_finalized_mutation();
DROP FUNCTION forbid_finalized_run_mutation();

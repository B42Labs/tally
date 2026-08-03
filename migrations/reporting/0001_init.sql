-- The complete Phase 1 schema of the Reporting API: the append-only events
-- hypertable, the projection derived from it, and the tables that carry
-- projects, credentials, and operational records.
--
-- The normative specification is roadmap/01-phase-1-core-platform-openstack.md,
-- WP 1.3.

-- +goose Up
-- The chain needs TimescaleDB 2.13 or newer: the events table below becomes a
-- hypertable through by_range, which arrived in that release. Installing or
-- upgrading the extension takes superuser, which the migrating role usually is
-- not, so this only names the missing provisioning step. Without it the chain
-- dies on create_hypertable with a message that mentions neither TimescaleDB
-- nor what to do about it, and the transaction leaves the database empty.
-- +goose StatementBegin
DO $$
DECLARE
    installed TEXT;
BEGIN
    SELECT extversion INTO installed FROM pg_extension WHERE extname = 'timescaledb';
    IF installed IS NULL THEN
        RAISE EXCEPTION 'the timescaledb extension must be installed in this database before migrating';
    END IF;
    -- Element-wise array comparison, so 2.9.3 ranks below 2.13 where a string
    -- comparison would not. The suffix goes first: extversion carries forms
    -- like 2.13.0-dev.
    IF string_to_array(split_part(installed, '-', 1), '.')::INT[] < ARRAY[2, 13] THEN
        RAISE EXCEPTION 'timescaledb % is too old: this chain creates the events hypertable with by_range, which needs 2.13 or newer', installed;
    END IF;
END $$;
-- +goose StatementEnd

-- Append-only source of truth (TimescaleDB hypertable, compressed)
CREATE TABLE events (
    event_id        TEXT NOT NULL,
    timestamp       TIMESTAMPTZ NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_type      TEXT NOT NULL,
    platform        TEXT NOT NULL,
    cloud           TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    source          TEXT NOT NULL DEFAULT 'collector',
    payload         JSONB,
    PRIMARY KEY (event_id, timestamp)
);
-- The dimension-builder form, not the positional one deprecated in 2.13: the
-- image tag the dev stack and the tests pull is not pinned to a release that
-- still carries the legacy overload.
SELECT create_hypertable('events', by_range('timestamp'));
ALTER TABLE events SET (timescaledb.compress,
    timescaledb.compress_segmentby = 'cloud,resource_type');
SELECT add_compression_policy('events', INTERVAL '90 days');

CREATE INDEX idx_events_resource ON events (cloud, resource_type, resource_id, timestamp);
CREATE INDEX idx_events_project  ON events (project_id, timestamp);
CREATE INDEX idx_events_type     ON events (event_type, timestamp);
-- No index on received_at: it is strictly increasing, so every insert on this
-- write path would contend on the same leaf page for a query only the Phase 3
-- late-event detection issues. That index arrives with the feature.

CREATE TABLE rejected_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    reason       TEXT NOT NULL,           -- 'schema' | 'size_schema' | 'payload_envelope'
    raw          JSONB NOT NULL
);

-- Derived projection; rows are never removed (deleted resources keep their row)
CREATE TABLE current_resources (
    cloud           TEXT NOT NULL,
    platform        TEXT NOT NULL,
    resource_type   TEXT NOT NULL,
    resource_id     TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    state           TEXT NOT NULL,
    size            JSONB NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ,
    deleted_at      TIMESTAMPTZ,
    last_event_type TEXT NOT NULL,
    last_event_at   TIMESTAMPTZ NOT NULL,
    last_payload    JSONB,
    PRIMARY KEY (cloud, resource_type, resource_id)
);
CREATE INDEX idx_current_resources_project ON current_resources (project_id);
CREATE INDEX idx_current_resources_type    ON current_resources (resource_type, state);

CREATE TABLE resource_types (
    platform      TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    size_schema   JSONB NOT NULL,          -- JSON Schema draft 2020-12
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (platform, resource_type)
);

CREATE TABLE projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    platform    TEXT NOT NULL,
    cloud       TEXT NOT NULL,
    external_id TEXT NOT NULL,
    name        TEXT,
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (cloud, external_id)
);

CREATE TABLE project_relations (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    source_id     UUID NOT NULL REFERENCES projects(id),
    target_id     UUID NOT NULL REFERENCES projects(id),
    relation_type TEXT NOT NULL,
    metadata      JSONB NOT NULL DEFAULT '{}',
    valid_from    TIMESTAMPTZ NOT NULL DEFAULT now(),
    valid_to      TIMESTAMPTZ,             -- NULL = active; never hard-deleted
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (source_id <> target_id)
);
CREATE UNIQUE INDEX uq_relations_active
    ON project_relations (source_id, target_id, relation_type) WHERE valid_to IS NULL;
CREATE INDEX idx_relations_source ON project_relations (source_id);
CREATE INDEX idx_relations_target ON project_relations (target_id);

CREATE TABLE ingest_credentials (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    platform    TEXT NOT NULL,
    cloud       TEXT NOT NULL,
    token_hash  TEXT NOT NULL UNIQUE,      -- sha256 hex of the bearer token
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ
);

CREATE TABLE api_tokens (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash  TEXT NOT NULL UNIQUE,
    role        TEXT NOT NULL,             -- 'admin' | 'read_all' | 'project'
    project_ids UUID[] NOT NULL DEFAULT '{}',   -- for role='project'
    description TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ,
    -- A project token without projects would resolve to an empty scope, which a
    -- query filter reads as "no project named" rather than "no project
    -- allowed". The column default makes that row easy to write by accident.
    CONSTRAINT api_tokens_project_scope
        CHECK (role <> 'project' OR cardinality(project_ids) > 0)
);

CREATE TABLE audit_log (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    actor       TEXT NOT NULL,             -- credential/token id or 'internal'
    action      TEXT NOT NULL,             -- e.g. 'events.ingest', 'projects.create'
    object_type TEXT,
    object_id   TEXT,
    details     JSONB
);
-- The two questions the log answers: what happened lately, and what happened to
-- one row. Both scan the whole table without these.
CREATE INDEX idx_audit_log_at     ON audit_log (at DESC);
CREATE INDEX idx_audit_log_object ON audit_log (object_type, object_id, at DESC);

CREATE TABLE sync_runs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    cloud        TEXT NOT NULL,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    status       TEXT NOT NULL DEFAULT 'running',  -- 'running'|'completed'|'failed'
    stats        JSONB NOT NULL DEFAULT '{}'       -- {created: n, updated: n, deleted: n, errors: [..]}
);
CREATE INDEX idx_sync_runs_cloud ON sync_runs (cloud, started_at DESC);

-- +goose Down
DROP TABLE sync_runs;
DROP TABLE audit_log;
DROP TABLE api_tokens;
DROP TABLE ingest_credentials;
DROP TABLE project_relations;
DROP TABLE projects;
DROP TABLE resource_types;
DROP TABLE current_resources;
DROP TABLE rejected_events;
-- Dropping the hypertable takes its chunks and its compression policy with it.
DROP TABLE events;

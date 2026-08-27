-- The applied pricing adjustments of a run: one row per adjustment a run put on
-- a project statement, with the relation it came from, the rate, the amount the
-- rate was applied to and the signed amount it produced.
--
-- The normative specification is roadmap/05-phase-5-commercial-pricing.md,
-- WP 5.3 and decision D8. The rows exist beside the statement documents because
-- a kickback is settled per beneficiary across all of a run's projects, and a
-- sum over JSONB documents is not a query the reporting side can serve.
--
-- The table is a run artifact like the records of migration 0001, so it carries
-- the same forbid_finalized_mutation trigger: what a run pays out is the sum
-- over these rows, and a finalized run is immutable (guardrail 7 of
-- roadmap/00-conventions.md).
--
-- Two columns go past the roadmap's table. They are named here rather than
-- added silently, per guardrail 10 of roadmap/00-conventions.md: relation_type
-- and relation_target hold the kind of relation the adjustment was collected
-- from and the external id it points at, so a record and a credit-note line
-- name that relation without a second lookup in the reporting database.
-- beneficiary stays what D8 defines, a partner's external id, and it is set on
-- kickback rows alone.

-- +goose Up
CREATE TABLE adjustment_records (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id          UUID NOT NULL REFERENCES runs(id),
    project_id      TEXT NOT NULL,             -- the statement key, as project_statements.project_id
    relation_id     UUID NOT NULL,             -- provenance: project_relations.id in the reporting database
    relation_type   TEXT NOT NULL,
    relation_target TEXT NOT NULL,             -- the target's external id
    beneficiary     TEXT,                      -- the partner's external id, kickbacks only
    type            TEXT NOT NULL,             -- surcharge | discount | project_discount | kickback
    scope           TEXT NOT NULL,
    rate            NUMERIC(8,6) NOT NULL,
    base            NUMERIC(14,2) NOT NULL,    -- the amount the rate was applied to
    amount          NUMERIC(14,2) NOT NULL,    -- signed: discounts negative
    currency        TEXT NOT NULL,
    -- What a partner is paid is a sum over the rows idx_adjustments_run serves,
    -- and that sum reads a beneficiary as a kickback. A beneficiary on any other
    -- type turns a customer's discount into a payout, on rows a finalized run
    -- holds immutable, so the invariant the header states is checked here rather
    -- than left to the one writer in internal/engine/runs/store.go.
    CHECK ((type = 'kickback') = (beneficiary IS NOT NULL))
);
CREATE INDEX idx_adjustments_run ON adjustment_records (run_id, beneficiary);
CREATE TRIGGER trg_adjustment_immutable BEFORE INSERT OR UPDATE OR DELETE ON adjustment_records
    FOR EACH ROW EXECUTE FUNCTION forbid_finalized_mutation();

-- +goose Down
DROP TABLE adjustment_records;

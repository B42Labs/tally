-- The names a size member stands for, per cloud.
--
-- A platform can report a size member on its event path under an id while its
-- listing reports the same thing under a name. Cinder is the case this table
-- exists for: every volume notification carries the volume type's id in
-- volume_type, and the volume listing a reconciliation run reads carries the
-- type's name. A price list keys type_modifiers by name, so an interval opened
-- by an event that carries the id would match no modifier and be billed at
-- factor 1.
--
-- A row says: on this cloud, an event of this resource type that carries value
-- under the size member member means name. A reconciliation run writes the rows
-- of the cloud it syncs, from the listing its adapter reads, and replaces all of
-- them in one transaction. The ingest pipeline reads them and replaces a
-- matching value with its name before it validates, stores and folds the event.
-- The table names no platform and no member on purpose, so the pipeline that
-- reads it learns no platform's vocabulary: the rows alone decide.
--
-- tally_engine_reader gets no grant. The engine reads the events, which already
-- carry the name, and never this table.
--
-- The rollback drops what it finds, the way 0008, 0009 and 0010 do, so that a
-- table an operator already dropped by hand does not leave version 12 recorded
-- as applied and goose_db_version to be edited. A rollback across 12 loses the
-- names: a re-applied 0012 starts empty, and until the next run of a cloud stores
-- them again, that cloud's volume events are stored with the type id.
--
-- This file takes the chain to version 12, which is the version a binary built
-- from this tree expects: migrations/reporting/embed.go carries it and the
-- Reporting API's readiness refuses a database on 11, so the pod stays out of
-- rotation until the chain is applied. Running tally-reporting-admin migrate is
-- part of the deploy that rolls a Reporting API image built from here, and it
-- goes first. In the dev cluster make up covers it, because it runs make
-- migrate.

-- +goose Up
CREATE TABLE size_names (
    cloud         TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    member        TEXT NOT NULL,
    value         TEXT NOT NULL,
    name          TEXT NOT NULL,
    PRIMARY KEY (cloud, resource_type, member, value)
);

-- +goose Down
DROP TABLE IF EXISTS size_names;

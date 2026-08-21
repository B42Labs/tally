-- The metering engine's database, beside the reporting one on the same server.
-- The Postgres entrypoint runs the scripts under /docker-entrypoint-initdb.d
-- only against an empty data directory, so this creates tally_engine on a fresh
-- volume's first start and never again.

CREATE DATABASE tally_engine;

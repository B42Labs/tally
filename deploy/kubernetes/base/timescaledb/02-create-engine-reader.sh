#!/bin/bash
# The roles the metering engine reads the reporting database through: the group
# role that carries the read-only grants, and the login role a deployment
# connects as. Migration 0008 of the reporting chain creates
# tally_engine_reader too, but only when it is missing, so creating it here is
# what lets the membership be granted before the chain has run. The migration
# then finds the role and only grants.
#
# The password is not written here. This directory is the deployment surface dev
# and prod share, so a password in it would be the password of every deployment
# built on the base, published in the repository. It comes from the tally-db
# secret through TALLY_ENGINE_PASSWORD instead, which each overlay fills with
# its own value; an empty one fails initdb rather than leaving a login role
# behind that anything reaching the Gateway's postgres listener can use.
#
# Reading the environment is what makes this a shell script rather than SQL.
# Like 01-create-engine-db.sql it runs against an empty data directory alone, so
# a dev cluster whose volume predates it needs one `make down && make up` before
# the roles exist.

if [ -z "${TALLY_ENGINE_PASSWORD:-}" ]; then
	echo "TALLY_ENGINE_PASSWORD is empty: the tally-db secret has to carry engine-password" >&2
	exit 1
fi

# The password reaches psql as a variable rather than through the heredoc. An
# unquoted heredoc would paste it into the SQL text as it stands, where the
# first apostrophe ends the literal and the rest of the value is read as
# statements: a syntax error for the apostrophe a generated password happens to
# carry, and whatever was written after it for one that was chosen, run as the
# superuser this connects as. :'engine_password' quotes the value psql was
# handed, so the password is data whatever it contains, and the delimiter is
# quoted so that nothing in the body is the shell's to expand.
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" \
	--set=engine_password="$TALLY_ENGINE_PASSWORD" <<-'EOSQL'
	CREATE ROLE tally_engine_reader NOLOGIN;
	CREATE ROLE tally_engine LOGIN PASSWORD :'engine_password';
	GRANT tally_engine_reader TO tally_engine;
EOSQL

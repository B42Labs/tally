---
title: Issue and revoke credentials
description: Issue an ingest credential for a collector and a query token for a reader with tally-reporting-admin, and revoke either one.
quadrant: how-to
audience: operator
---

# Issue and revoke credentials

This guide issues the two credentials the Reporting API takes, an ingest
credential scoped to one (platform, cloud) pair and a query token carrying one
role, and revokes either of them. Every command runs against the reporting
database named by `TALLY_REPORTING_DB_URL`. The CLI opens a pool on that
database and talks to no API.

## Before you start

- The reporting database reachable from the machine you run the CLI on, and
  its connection string.
- `tally-reporting-admin` at the version the API runs, with its subcommands and
  flags on the
  [reporting admin CLI](/reference/command-line/tally-reporting-admin) page.
- The registry ids of the projects a project-scoped token may read, if you
  issue one.
- The [Reporting API settings](/reference/configuration/tally-reporting) page,
  which lists the variables the CLI reads from the environment.

## Issue an ingest credential

1. Export the connection string and check that the CLI reaches the database:

   ```sh
   export TALLY_REPORTING_DB_URL='postgres://tally:password@db.internal:5432/tally_reporting?sslmode=require'
   tally-reporting-admin migrate-status | tail -1
   ```

   ```text
   migration 10 applied
   ```

2. Issue the credential for the cloud the collector reports under:

   ```sh
   tally-reporting-admin create-ingest-credential \
     --platform openstack \
     --cloud <cloud> \
     --description 'event collector for <cloud>'
   ```

   ```text
   tly_i_6eb7f2ea8ffbb37f44d41bdc3382d193c3de752f89d5bafe7b85afc93a65c32b
   created ingest_credentials 8f0c2f34-1a4d-4c2e-9a53-6b1c0f5e77a1
   the token above is printed this one time: store it now, it will not be shown again
   ```

3. The token goes to stdout alone and the two notices to stderr, so a
   redirection keeps the token and leaves the notices on the terminal. Write it
   to the file the collector reads as `TALLY_OSC_TOKEN_FILE`
   ([connect the collector](/how-to/openstack/connect-the-collector)):

   ```sh
   install -m 600 /dev/null /run/secrets/tally/ingest-token
   tally-reporting-admin create-ingest-credential \
     --platform openstack --cloud os-prod-eu1 \
     --description 'event collector for os-prod-eu1' > /run/secrets/tally/ingest-token
   ```

   ```text
   created ingest_credentials 8f0c2f34-1a4d-4c2e-9a53-6b1c0f5e77a1
   the token above is printed this one time: store it now, it will not be shown again
   ```

   The `install` line is what sets the mode, because the redirection sets one
   only where it creates the file. On a path that does not exist it creates the
   file under the umask the shell carries, `0022` on a stock installation,
   which leaves the token readable by every account on the machine. On a path
   that does exist it truncates the file and leaves the mode it found: reissuing
   a leaked credential into the path the previous one was written to would
   restore exactly the exposure the reissue is performed to end.
   `install -m 600 /dev/null` writes `0600` in both cases. The credential is
   long-lived — nothing expires it, and `revoke-ingest-credential` is what ends
   it — and it authenticates `POST /api/v1/events` for this one (platform,
   cloud) pair, so whoever reads the file writes billing events for that cloud.

## Issue a query token

1. Issue a token for one of the three roles, `admin`, `read_all` or `project`:

   ```sh
   tally-reporting-admin create-api-token \
     --role read_all \
     --description 'monthly export job'
   ```

   ```text
   tly_a_14c2529eb4498c5d1ffd6915d05bf58a91bdda796af59f41d480d11c099d0479
   created api_tokens 3f0b7d1e-5c62-4a8f-b0d1-2e7a9c4f6b03
   the token above is printed this one time: store it now, it will not be shown again
   ```

2. A token for a set of projects takes `--role project` and one `--project-id`
   per project, each the registry id of that project:

   ```sh
   tally-reporting-admin create-api-token \
     --role project \
     --project-id 5c9f1a02-3d47-4e18-8b6a-9f0c21d7e455 \
     --project-id 7b2e4c11-08a9-4d3f-bc57-1e6d3a0f9c82 \
     --description 'partner portal'
   ```

   ```text
   tly_a_14c2529eb4498c5d1ffd6915d05bf58a91bdda796af59f41d480d11c099d0479
   created api_tokens 9d41b8c7-0e35-42a6-8f19-c73b5a2d604e
   the token above is printed this one time: store it now, it will not be shown again
   ```

   A `--role` outside the three roles and a `--project-id` that is not a uuid
   exit 1 with the reason on stderr and write no row.

## Revoke a credential

1. Revoke an ingest credential by the id its notice printed:

   ```sh
   tally-reporting-admin revoke-ingest-credential 8f0c2f34-1a4d-4c2e-9a53-6b1c0f5e77a1
   ```

   ```text
   revoked ingest_credentials 8f0c2f34-1a4d-4c2e-9a53-6b1c0f5e77a1
   ```

2. Revoke a query token the same way. An id no row carries exits 1 with
   `not found`:

   ```sh
   tally-reporting-admin revoke-api-token 3f0b7d1e-5c62-4a8f-b0d1-2e7a9c4f6b03
   ```

   ```text
   revoked api_tokens 3f0b7d1e-5c62-4a8f-b0d1-2e7a9c4f6b03
   ```

## Check the result

1. Issue the query token of the section above with its output captured, so the
   two calls below read it from the environment. The token goes to stdout and
   the notices to stderr, so the substitution holds the token, the notices stay
   on the terminal, and the token itself never reaches the shell's history:

   ```sh
   TALLY_API_TOKEN="$(tally-reporting-admin create-api-token \
     --role read_all --description 'monthly export job')"
   ```

   ```text
   created api_tokens 3f0b7d1e-5c62-4a8f-b0d1-2e7a9c4f6b03
   the token above is printed this one time: store it now, it will not be shown again
   ```

2. Before the revocation, the query token reads the API:

   ```sh
   curl -sS -o /dev/null -w '%{http_code}\n' \
     -H "Authorization: Bearer $TALLY_API_TOKEN" \
     https://tally-reporting.internal/api/v1/projects
   ```

   ```text
   200
   ```

3. After it, the same call is answered 401:

   ```sh
   tally-reporting-admin revoke-api-token 3f0b7d1e-5c62-4a8f-b0d1-2e7a9c4f6b03
   curl -sS -o /dev/null -w '%{http_code}\n' \
     -H "Authorization: Bearer $TALLY_API_TOKEN" \
     https://tally-reporting.internal/api/v1/projects
   ```

   ```text
   revoked api_tokens 3f0b7d1e-5c62-4a8f-b0d1-2e7a9c4f6b03
   401
   ```

4. Revoking a second time reports the row as revoked and exits 0. Nothing
   changed, so nothing is written to the audit log:

   ```sh
   tally-reporting-admin revoke-api-token 3f0b7d1e-5c62-4a8f-b0d1-2e7a9c4f6b03
   echo $?
   ```

   ```text
   api_tokens 3f0b7d1e-5c62-4a8f-b0d1-2e7a9c4f6b03: already revoked
   0
   ```

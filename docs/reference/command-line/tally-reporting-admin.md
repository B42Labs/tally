---
title: Reporting admin CLI (tally-reporting-admin)
description: Every subcommand and flag of the admin CLI that issues credentials, registers virtual projects and migrates the reporting database.
quadrant: reference
audience: operator
---

# Reporting admin CLI (tally-reporting-admin)

`tally-reporting-admin` issues and revokes the Reporting API's credentials,
registers its virtual projects, and applies the migrations of its database. The
HTTP contract has no credential routes: a credential is issued by an operator
who already holds database access, not over the API that credential opens.

The CLI therefore talks to the reporting database and never to the API. It
opens a pool on `TALLY_REPORTING_DB_URL`, the same connection string the server
reads, and it is the only thing that runs DDL against that database. The
command tree is built in
[`cmd/tally-reporting-admin/main.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-reporting-admin/main.go)
and its subcommands in
[`cmd/tally-reporting-admin/commands.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-reporting-admin/commands.go).

## Environment

The CLI reads the server's configuration package, so the variables and their
defaults are the server's, and the Reporting API settings page lists them.

Its own gate asks for `TALLY_REPORTING_DB_URL` alone. The CLI serves nothing, so
the internal token and the OIDC URL the server's gate insists on are not asked
for here. `TALLY_REPORTING_DB_MAX_CONNS` bounds the pool a subcommand opens.

The environment is read when a subcommand runs, not when the tree is built, so
`--help` needs no configuration at all.

## Output

A new token is printed once, alone on stdout, so it can be redirected into a
file or a secret. The notice beside it goes to stderr: `created` with the table
and the new row's id, then the line that says the token is shown this one time.
The database receives the sha256 digest of the token and never the token
itself, which is what the `token_hash` columns hold.

`create-meta-project` and `create-partner` print the new project's registry id
alone on stdout, so a command substitution passes it as the `target_id` of a
relation. `registered` with the noun and the external id goes to stderr.

A revocation prints one line on stdout: `revoked` with the table and the id, or
the line that says the row was already revoked.

Every change is written to the audit log under the actor `admin-cli`. Which
person ran the CLI is not something the process knows; the database credentials
it used are. A revocation that changed nothing writes no audit row.

## Exit status

A subcommand that did what it was asked exits 0. Revoking a credential that is
already revoked is one of those: what the operator asked for holds, so it is
reported and not failed.

Every refusal exits 1 with the reason on stderr. A flag that was passed empty, a
`--role` outside the three roles, a `--project-id` that is not a uuid, a
`migrate-down-to` without `--yes`, an id no row carries, an external id the
registry already holds, and a database that cannot be reached all end that way.

## Commands

<!-- refdoc:begin commands -->
### `tally-reporting-admin`

Provision the Reporting API's credentials and virtual projects

```text
tally-reporting-admin
```

| Subcommand | Purpose |
| --- | --- |
| `create-api-token` | Issue a query token for one role |
| `create-ingest-credential` | Issue an ingest credential for one cloud |
| `create-meta-project` | Register a meta-project that groups real projects under a customer |
| `create-partner` | Register a partner entity that manages real projects |
| `migrate` | Apply the pending migrations of the reporting database |
| `migrate-down-to` | Roll the database back to a migration version |
| `migrate-status` | Report which migrations the database carries |
| `revoke-api-token` | Revoke a query token |
| `revoke-ingest-credential` | Revoke an ingest credential |

This command takes no flags.

### `tally-reporting-admin create-api-token`

Issue a query token for one role

```text
tally-reporting-admin create-api-token [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--description` | string | none | no | note kept with the token, such as who asked for it |
| `--project-id` | string, repeatable | none | no | registry id of a project a project token may read, repeatable |
| `--role` | string | none | yes | what the token may do: admin, read_all, or project |

### `tally-reporting-admin create-ingest-credential`

Issue an ingest credential for one cloud

```text
tally-reporting-admin create-ingest-credential [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--cloud` | string | none | yes | cloud the credential reports for |
| `--description` | string | none | no | note kept with the credential, such as who asked for it |
| `--platform` | string | none | yes | platform the credential reports for, openstack for example |

### `tally-reporting-admin create-meta-project`

Register a meta-project that groups real projects under a customer

```text
tally-reporting-admin create-meta-project [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--external-id` | string | none | yes | id of the meta-project as the registry names it |
| `--name` | string | none | no | human-readable name kept with the row |

### `tally-reporting-admin create-partner`

Register a partner entity that manages real projects

```text
tally-reporting-admin create-partner [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--external-id` | string | none | yes | id of the partner as the registry names it |
| `--name` | string | none | no | human-readable name kept with the row |

### `tally-reporting-admin migrate`

Apply the pending migrations of the reporting database

Apply the pending migrations of the reporting database.

The API server runs no DDL, so this is what brings a database to the schema the server expects.

```text
tally-reporting-admin migrate
```

This command takes no flags.

### `tally-reporting-admin migrate-down-to <version>`

Roll the database back to a migration version

Roll the database back to a migration version.

Every migration above the given version is undone, which drops the data its tables hold. Passing 0 leaves an empty schema. The chain ships these down migrations, so this is what runs them.

```text
tally-reporting-admin migrate-down-to <version> [flags]
```

| Flag | Type | Default | Required | Description |
| --- | --- | --- | --- | --- |
| `--yes` | boolean | `false` | no | confirm that the data of the rolled-back migrations may be dropped |

### `tally-reporting-admin migrate-status`

Report which migrations the database carries

Report which migrations the database carries.

It answers which schema a database is on before code that assumes a newer one is deployed against it.

```text
tally-reporting-admin migrate-status
```

This command takes no flags.

### `tally-reporting-admin revoke-api-token <id>`

Revoke a query token

```text
tally-reporting-admin revoke-api-token <id>
```

This command takes no flags.

### `tally-reporting-admin revoke-ingest-credential <id>`

Revoke an ingest credential

```text
tally-reporting-admin revoke-ingest-credential <id>
```

This command takes no flags.
<!-- refdoc:end commands -->

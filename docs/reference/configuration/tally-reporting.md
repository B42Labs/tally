---
title: Reporting API settings
description: Every environment variable the Reporting API server and the admin CLI read.
quadrant: reference
audience: operator
---

# Reporting API settings

`tally-reporting` and `tally-reporting-admin` load the same configuration
package, so the table below is the configuration of both. Every setting comes
from the environment, and every variable Tally defines carries the prefix
`TALLY_`. Section 8 of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md)
states that rule.

A secret is given inline or as a path to a file. The File-backed column names
the companion variable, and
[`internal/core/envsecret`](https://github.com/B42Labs/tally/blob/main/internal/core/envsecret/envsecret.go)
applies it: the companion is the variable's name plus the suffix `_FILE`, and a
companion that holds a path makes that file's content the value. One trailing
newline is trimmed, which is what Kubernetes writes into a Secret volume. An
empty file is refused, and so is setting a variable and its `_FILE` companion at
the same time.

`TALLY_LOG_LEVEL` takes `DEBUG`, `INFO`, `WARN` or `ERROR`. The match is exact,
so a lower-case `info` is refused.

## Settings

<!-- refdoc:begin settings -->
| Variable | Type | Default | File-backed | Governs |
| --- | --- | --- | --- | --- |
| `TALLY_LOG_LEVEL` | string | `INFO` | no | LogLevel is the slog threshold, one of DEBUG, INFO, WARN, or ERROR. |
| `TALLY_REPORTING_HTTP_PORT` | integer | `8080` | no | HTTPPort is the port the API server listens on. |
| `TALLY_REPORTING_DB_URL` | string | none | yes (`TALLY_REPORTING_DB_URL_FILE`) | DBURL is the PostgreSQL connection string. It has no default because a guessed database is worse than none. Supports the *_FILE convention. |
| `TALLY_REPORTING_DB_MAX_CONNS` | integer | `10` | no | DBMaxConns bounds the connection pool. It is sized against the database's max_connections divided by the replica count, which is what pgxpool cannot know: left to itself it derives the bound from the node's CPU count, so a pod landing on a large node opens far more connections than the database budgeted for. |
| `TALLY_REPORTING_AUTH_MODE` | string | `enforced` | no | AuthMode is "enforced" or "disabled". Disabled short-circuits every authentication middleware and exists for development and tests only. |
| `TALLY_REPORTING_INTERNAL_TOKEN` | string | none | yes (`TALLY_REPORTING_INTERNAL_TOKEN_FILE`) | InternalToken is the shared secret guarding the /internal/* routes. Supports the *_FILE convention. |
| `TALLY_REPORTING_UNHEALTHY_THRESHOLD_S` | integer | `600` | no | UnhealthyThresholdSeconds is how long readiness may keep failing before liveness fails too and the orchestrator restarts the pod. |
| `TALLY_REPORTING_OIDC_JWKS_URL` | string | none | no | OIDCJWKSURL points at an OIDC provider's JWKS. It is the extension point for accepting Bearer JWTs, and nothing implements it yet, so a set value refuses startup instead of being ignored. |
| `TALLY_INGEST_REQUIRE_SIZE_SCHEMA` | boolean | `false` | no | RequireSizeSchema is ingest's strict mode: when true, an event whose payload carries a size for a (platform, resource_type) pair with no registered schema is rejected; the default accepts it unvalidated, so a registry that lags behind a new resource type does not stop collection. The variable has no REPORTING infix because roadmap section WP 1.3 names it TALLY_INGEST_REQUIRE_SIZE_SCHEMA. |
| `TALLY_REPORTING_ATTRIBUTING_RELATION_TYPES` | list, comma-separated | `infrastructure_tenant` | no | AttributingRelationTypes are the relation types the cycle guard walks when a relation is created; an empty list disables the guard. Setting the variable to the empty string yields that empty list. "member_of" and "managed_by" reach a virtual project and attribute no cost, so a list naming either is refused. |
| `TALLY_REPORTING_CLOUDS_CONFIG` | string | none | no | CloudsConfigPath is the path to the deployment's clouds YAML, the file the reconciliation framework reads at startup to learn which clouds it can sync. It has no *_FILE companion because the value is a path already, not a secret. Unset is valid and means no clouds are configured, so every sync answers 404. Whether the file exists and parses is checked by reconciliation.LoadConfig, not here. |
| `TALLY_REPORTING_SYNC_ALLOW_AT` | boolean | `false` | no | SyncAllowAt lets POST /internal/sync/{cloud} take the instant a run is at from its request body, which the run then stamps its row and its corrections with instead of reading a clock. It is for a development deployment reconciling a simulated cloud, whose clock is not the wall clock; a production deployment keeps the default, where such a request is refused before a run starts. |
| `TALLY_METRICS_ENABLED` | boolean | `true` | no | MetricsEnabled exposes the instrumentation: false makes GET /metrics answer 404 and stops the gauge refresher. The instruments still exist and keep counting either way, so turning the flag back on costs nothing and loses only the samples of the window it was off. The variable has no REPORTING infix because roadmap section 8 lists it among the common variables every service reads. |
| `TALLY_REPORTING_METRICS_REFRESH_S` | integer | `60` | no | MetricsRefreshSeconds is the interval on which the tally_current_resources gauge is re-derived from the projection. |
<!-- refdoc:end settings -->

## What is checked

`Load` runs for both programs. It resolves the two file-backed secrets and then
checks:

- `TALLY_LOG_LEVEL` is one of the four levels above.
- `TALLY_REPORTING_AUTH_MODE` is `enforced` or `disabled`.
- `TALLY_REPORTING_UNHEALTHY_THRESHOLD_S` is positive.
- `TALLY_REPORTING_DB_MAX_CONNS` is positive.
- `TALLY_REPORTING_METRICS_REFRESH_S` is positive.
- `TALLY_REPORTING_ATTRIBUTING_RELATION_TYPES` carries no empty entry. The
  variable set to the empty string is the empty list, which turns the cycle
  guard off; an empty entry anywhere else is a stray comma and is refused.
- `TALLY_REPORTING_ATTRIBUTING_RELATION_TYPES` names neither `member_of` nor
  `managed_by`. Both reach a virtual project and attribute no cost.

`ValidateServer` is the server's startup gate. It adds:

- `TALLY_REPORTING_DB_URL` is set.
- `TALLY_REPORTING_INTERNAL_TOKEN` is set, unless `TALLY_REPORTING_AUTH_MODE`
  is `disabled`.
- `TALLY_REPORTING_OIDC_JWKS_URL` is unset. Nothing implements OIDC
  authentication, so a value there refuses startup rather than being ignored.

`ValidateAdmin` is the admin CLI's gate. It asks for `TALLY_REPORTING_DB_URL`
and nothing else: the CLI works on the database directly and serves no route.

## Example

[`cmd/tally-reporting/.env.example`](https://github.com/B42Labs/tally/blob/main/cmd/tally-reporting/.env.example)
lists every variable with its default and a comment.
[`cmd/tally-reporting-admin/.env.example`](https://github.com/B42Labs/tally/blob/main/cmd/tally-reporting-admin/.env.example)
lists the one the CLI needs.

One variable in the server's file is not Tally's. `OS_CLIENT_CONFIG_FILE` is
gophercloud's, and it names the `clouds.yaml` the OpenStack reconciliation
adapter authenticates from, the way it does for every other OpenStack client on
a host. Setting it makes the named file the only location searched. Left unset,
the search starts at the process working directory and reaches
`/etc/openstack/clouds.yaml` last, so anything that can write a `clouds.yaml`
into the working directory outranks the mounted Secret. The
[clouds file](/reference/configuration/clouds-file) page states which entry of
that file a cloud authenticates with.

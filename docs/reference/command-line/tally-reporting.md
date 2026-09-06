---
title: Reporting API server (tally-reporting)
description: What the Reporting API server process takes, serves and does on a signal.
quadrant: reference
audience: operator
---

# Reporting API server (tally-reporting)

`tally-reporting` is the process that serves the Reporting API. It reads its
configuration from the environment, refuses a configuration it cannot honor,
and then serves the routes of the OpenAPI contract on the configured port. The
process is assembled in
[`cmd/tally-reporting/main.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-reporting/main.go).

## Invocation

The process takes no flags and reads no arguments. Every setting comes from the
environment, under the names the
[Reporting API settings](/reference/configuration/tally-reporting) page lists.

## What it serves

The API routes are the operations of
[Reporting API endpoints](/reference/api/reporting-api), and their bodies are
the schemas of
[Reporting API schemas](/reference/api/reporting-api-schemas). The router is
built from the same OpenAPI document, which validates every request before a
handler sees it.

`GET /healthz` and `GET /readyz` are served without a credential. `/readyz`
fails while the database is unreachable, which takes the pod out of rotation
without restarting it.

`GET /metrics` serves the Prometheus exposition, also without a credential.
A deployment that sets `TALLY_METRICS_ENABLED` to `false` is answered 404 there,
and the gauge refresher that keeps the projection counts current does not run.

## Startup

The database pool connects lazily. The process therefore comes up while
TimescaleDB is unavailable and reports that state through the probes rather
than through a failed start. It runs no DDL: bringing a database to the schema
the server expects is
[`tally-reporting-admin migrate`](/reference/command-line/tally-reporting-admin),
so a schema change stays an operator's decision rather than a side effect of a
pod restart.

The clouds file `TALLY_REPORTING_CLOUDS_CONFIG` names is read once at startup
rather than per sync run. Every configured cloud's adapter name and platform is
checked against the registered adapters there, so a cloud that names an
unregistered adapter, or one that observes a different platform than the cloud
declares, stops the process before it listens. The
[clouds file](/reference/configuration/clouds-file) page states the file's
shape.

Authentication turned off with `TALLY_REPORTING_AUTH_MODE=disabled` is logged as
a warning at startup: every request is then served without a credential.

A configuration the server cannot honor exits 1 before the port is bound, with
the message naming the variable. `TALLY_REPORTING_DB_URL` has to be set;
`TALLY_REPORTING_INTERNAL_TOKEN` has to be set unless authentication is
disabled; `TALLY_REPORTING_OIDC_JWKS_URL` is refused, because OIDC
authentication is not implemented.

## Signals and exit status

SIGINT and SIGTERM begin a graceful shutdown. The server stops accepting
connections and in-flight requests get 10 seconds to finish, which stays under
the grace period Kubernetes gives a terminating pod. The process then exits 0.

Every other failure exits 1: a configuration that was refused, a database pool
that could not be opened, a clouds file that could not be loaded, a router that
could not be built, and a listener that ended with anything but a closed
server.

## Logging

The process writes JSON lines to stdout. Every line carries
`service=tally-reporting`, and the level is the one `TALLY_LOG_LEVEL` names:
`DEBUG`, `INFO`, `WARN` or `ERROR`.

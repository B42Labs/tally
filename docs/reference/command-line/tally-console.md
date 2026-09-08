---
title: Demo console (tally-console)
description: "What the read-only demo console serves, the admin token it needs, and how make console starts it."
quadrant: reference
audience: operator
---

# Demo console (tally-console)

`tally-console` is a read-only web console over the data Tally already holds:
the projects, resources and lifecycles the Reporting API serves, and the
billing periods, runs, statements and pricing catalogs the engine stored. It is
a demo instrument. It runs on a developer's machine, writes nothing on either
side, is deployed nowhere, and has no sign-in, so whoever reaches its port reads
everything its token reads. Every page is HTML the process rendered; it ships no
JavaScript and loads one stylesheet. The process is assembled in
[`cmd/tally-console/main.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-console/main.go).

## Invocation

The process takes no flags and reads no arguments. Every setting comes from the
environment, under the names the settings table below lists.

## What it serves

| Route | What it reads |
| --- | --- |
| `/` | Reporting API: resource counts by cloud, resource type and state; event counts of the last 24 hours per hour; the five newest refused events. Engine: the billing periods, the 20 newest runs. |
| `/projects` | API: one page of projects, filtered by `platform`, `cloud`, `cursor`. |
| `/project?id=` | API: the project, its relations, the projects a traversal reaches, its summary over the current month. Engine: one row per statement of the project. |
| `/resources` | API: one page of resources, filtered by `cloud`, `project_id`, `resource_type`, `state`, `status`, `cursor`. |
| `/resource?cloud=&type=&id=` | API: the lifecycle. Engine: the newest run that metered the resource, or the one `run=` names, and its rated segments; a timeline of both. |
| `/pricing` | Engine: the imported catalog versions. |
| `/catalog?version=` | Engine: one catalog, parsed by the engine's own parser. |
| `/run?id=` | Engine: the run, its statements, its correction deltas. |
| `/statement?run=&key=` | Engine: one statement document rendered as a bill. |
| `/static/console.css` | The embedded stylesheet, the only asset the pages load. |

Every identifier travels in a query parameter rather than in a path segment,
because a cloud name, a resource id and a statement key may each carry a slash.

Paging is one page per request. A listing the API answered with a cursor carries
a next link that repeats the filters and adds that cursor, and nothing follows a
cursor on its own.

Every page ends with a provenance panel naming the reads it was built from: each
Reporting API request as its method and path with the query string, and each
engine read under the name its query carries in
[`internal/console/store/queries.sql`](https://github.com/B42Labs/tally/blob/main/internal/console/store/queries.sql).

A failure renders one error page carrying the wrapped error: 400 for a parameter
that is missing or unreadable, 404 for a lookup that found nothing and for an
API 404, 502 for a Reporting API call that failed or that the API refused the
token for, and 503 for an engine query that failed or a stored document that
does not decode. A path the console has no page for is answered 404 on that same
error page.

Amounts are rendered at two decimal places and quantities at four, the scales
the engine rounds them to. A catalog price is rendered with the digits the
pricing document carries, because a price of `0.00005` per unit hour rounded to
a money scale would print as zero.

## Environment

Every setting comes from the environment. The two secrets accept the `*_FILE`
companion the rest of Tally uses, the variable's name plus the suffix `_FILE`
holding a path whose content becomes the value, applied by
[`internal/core/envsecret`](https://github.com/B42Labs/tally/blob/main/internal/core/envsecret/envsecret.go).

<!-- refdoc:begin settings -->
| Variable | Type | Default | File-backed | Governs |
| --- | --- | --- | --- | --- |
| `TALLY_LOG_LEVEL` | string | `INFO` | no | LogLevel is the slog threshold, one of DEBUG, INFO, WARN, or ERROR. |
| `TALLY_CONSOLE_HTTP_PORT` | integer | `8095` | no | HTTPPort is the port the console listens on, bound to the loopback address only. The default is 8095 because 8090 and 8091 are taken by the simulator compose stack. |
| `TALLY_CONSOLE_REPORTING_URL` | string | none | no | ReportingURL is the base URL of the Reporting API without the /api/v1 suffix. It has to be set, and it has to be https unless its host is loopback, because the API token rides on every call. |
| `TALLY_CONSOLE_API_TOKEN` | string | none | yes (`TALLY_CONSOLE_API_TOKEN_FILE`) | APIToken is the bearer token the console reads the Reporting API with. It has to carry the admin role, because the dead-letter list the overview reads is admin-only. It has to be set. Supports the *_FILE convention. |
| `TALLY_CONSOLE_CA_FILE` | string | none | no | CAFile is a PEM file holding the CA that signed the Reporting API's certificate. Empty trusts the system store. |
| `TALLY_CONSOLE_ENGINE_DB_URL` | string | none | yes (`TALLY_CONSOLE_ENGINE_DB_URL_FILE`) | EngineDBURL is the PostgreSQL connection string of the engine database, opened for reads only. It has to be set. Supports the *_FILE convention. |
<!-- refdoc:end settings -->

## The token

`TALLY_CONSOLE_API_TOKEN` has to carry the `admin` role. The overview reads
`GET /api/v1/rejected-events`, the dead-letter list, and that route is
admin-only: a `read_all` token is answered 403 there. Every other read the
console makes takes `read_all` or `project`, so the dead-letter list alone
decides the role. Such a token is issued by
[`tally-reporting-admin create-api-token`](/reference/command-line/tally-reporting-admin).

The console sends nothing but GET and binds the loopback address. That is what
makes an admin token acceptable here, and why the process belongs on a laptop
rather than in a deployment.

## Startup

`make console` starts the console against the dev cluster. It issues a token
with `tally-reporting-admin create-api-token --role admin`, writes the dev CA to
`tally-ca.crt`, and runs the process against
`https://api.tally.127-0-0-1.nip.io:8443` and the dev engine database, printing
`http://127.0.0.1:8095/`. The token is issued fresh per run and comes before the
CA, so on a machine without a dev cluster the target fails on the admin CLI's
connection error.

The process binds `127.0.0.1` on `TALLY_CONSOLE_HTTP_PORT`, so the console is
reachable from the machine it runs on and nowhere else. The engine pool connects
lazily, so the process comes up while that database is unavailable and the pages
reading it report the outage.

A configuration the console cannot honor exits 1 before the port is bound, with
the message naming the variable. `TALLY_CONSOLE_REPORTING_URL`,
`TALLY_CONSOLE_API_TOKEN` and `TALLY_CONSOLE_ENGINE_DB_URL` have to be set. A
reporting URL that is not `https` and whose host is not loopback is refused,
because the token rides on every call and would travel in cleartext. A
`TALLY_CONSOLE_CA_FILE` holding no PEM certificate is refused with the file
named.

## Signals and exit status

SIGINT and SIGTERM begin a graceful shutdown. The server stops accepting
connections and in-flight requests get 10 seconds to finish. The process then
exits 0.

Every other failure exits 1: a configuration that was refused, a Reporting API
client that could not be built, an engine pool that could not be opened, a
router that could not be built, and a listener that ended with anything but a
closed server.

## Logging

The process writes JSON lines to stdout. Every line carries
`service=tally-console`, and the level is the one `TALLY_LOG_LEVEL` names:
`DEBUG`, `INFO`, `WARN` or `ERROR`. Every error page is logged with the wrapped
error it shows, at WARN for a request that asked for something wrong and at
ERROR for a failure of the console or of a side it reads. A stored row whose
amount is not a number is left out of the listing that read it and logged at
WARN, naming the query and the row's key columns.

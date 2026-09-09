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
| `/project?id=` or `/project?cloud=&external_id=` | API: the project, addressed by its id or resolved from the pair a resource names it with, its relations, the projects a traversal reaches, its summary over the window `from` and `to`, this month by default, and one page of the project's resources, which the summary rows fold open into. Engine: every statement of the project, grouped into the periods that were billed. |
| `/resources` | API: one page of up to 1000 resources under `status`, filtered by `cloud`, `project_id`, `resource_type`, `state`, `cursor`. The console reads that page one of two ways, chosen with `mode`: at the instant `at`, now by default, or over the window `from` and `to`. It prints each kept row's lifetime in hours, links its project by cloud and external id, and folds its last payload. |
| `/resource?cloud=&type=&id=` | API: the lifecycle. Engine: the newest run that metered the resource, or the one `run=` names, and its rated segments; a timeline of both. |
| `/pricing` | Engine: the imported catalog versions. |
| `/catalog?version=` | Engine: one catalog, parsed by the engine's own parser. |
| `/run?id=` | Engine: the run, its statements, its correction deltas. |
| `/statement?run=&key=` | Engine: one statement document rendered as a bill: its head, its adjustments, a summary table of its line items sorted by total, and a folding detail block per item with one row per metric and period. |
| `/static/console.css` | The embedded stylesheet, the only asset the pages load. |
| `/theme` | Nothing. The one route that is not a page: it takes the theme form's POST, keeps the choice in a cookie, and sends the viewer back. |

Every identifier travels in a query parameter rather than in a path segment,
because a cloud name, a resource id and a statement key may each carry a slash.

A resource names its project by cloud and external id, not by the id the API
assigns, so the project links on the resource pages carry that pair. The
project page resolves it through the project list filtered by both, an exact
match on each, and a pair nothing is registered under is answered 404. The
relations of a project link either end that is not the project of the page, so
a relation another project leaves leads to it and one this project leaves
leads to what it reaches.

What a project ran is read over a window, `from` and `to`, with the same inputs
and the same spans the fleet is read over: the last 24 hours, the last 7 days,
this month, last month. The window is this month while the request names
neither bound. The summary route takes both bounds, so a request carrying one
and not the other is answered 400 naming the missing one, and so is a `to`
that is not after `from`. Each bound is read as RFC 3339 or as the form the
inputs write. The rest of the page is read as it stands now, whatever the
window is: the relations, the projects a traversal reaches, and every statement
a run wrote for the project.

Each resource type of that summary folds open into the resources of the project
that lived in the window, by the rule the fleet reads a window by, sorted by
their ids, each with its state, its creation, its deletion and its lifetime in
hours, and each linking its own page. The filter above the table matches a row
on its type and on the resources folded under it, so a resource id finds the
type it sits in. The two numbers come from two places: the summary counts by
folding the events of the window, and the fold lists what the projection holds
today, so a resource the project has since handed on is counted and not listed,
and a type the projection holds nothing of is drawn without a fold. The
resources are read as one page of at most 1000, the widest the API serves, and
a project that holds more says under the table that a type may have run
resources the fold does not name.

The statements are one row per billing period rather than one per statement. A
period accumulates them: the regular run that billed it, every run that
replaced one of those, and every correction booked against the run that closed
it. The row carries what the project is charged for the period, which is the
sum of the statements whose run stands, and folds open into all of them, each
with its kind, its status, its own total and a link to the statement itself. A
statement that counts for nothing is drawn in the muted colour and says it was
replaced.

A run stands when its status is `completed` or `finalized`, which are the two
an export reads; a `superseded` run was replaced by another of its kind and a
`failed` one billed nothing, and neither is added up. This is why a correction
adds to the period rather than replacing it: a correction re-meters the period
whole and stores the difference against the run it corrects, so its statement
is a credit note over the run that closed the month, and the two together are
what the project owes. A period is billed in one currency, so the sum is one
amount; a period whose standing statements disagree prints each currency
rather than adding them up. The status of the period is the standing regular
run's, and a period whose every run was replaced stands at nothing and is
charged nothing. The filter matches a period on its instant and on the kinds
and statuses folded under it, so `correction` finds the months that hold one.

Paging is one page per request. A listing the API answered with a cursor carries
a next link that repeats the filters and adds that cursor, and nothing follows a
cursor on its own.

## Sorting and filtering

Every listing table can be sorted by a column and filtered by a text, and both
happen on the console's side of the wire. The console ships no JavaScript, so a
heading is a link that reloads the page with the order in the query string, and
the filter box above a table is a form that reloads it with the text. Two
parameters carry the state of one table, named after the table so that the
tables of one page keep their own:

| Parameter | Value |
| --- | --- |
| `<table>.sort` | The key of a column, which is its heading with spaces as underscores: `resource_type`. A leading hyphen sorts descending: `-count`. A key no column carries leaves the rows in the order they were read. |
| `<table>.q` | A text. A row stays when any of its cells contains the text, compared without regard to case. Whitespace around the text is dropped. |

So `/?stats.sort=-count&stats.q=os-sim` shows the resource counts of `os-sim`
with the largest first.

The first click on a heading sorts text ascending and a number descending, and
the next click flips the direction. Text sorts by its lower-cased form; a
number sorts by its value, so 10 comes after 2. A column that draws a bar
neither sorts nor filters. A filtered table says how many of its rows match,
and one the filter emptied says so in place of the rows. The filter form
carries every other parameter of the page along as hidden inputs, so applying
a filter keeps the page's identifiers, its cursor, and the order and filter of
every other table.

The tables and their names per page:

| Page | Tables |
| --- | --- |
| `/` | `stats`, `events`, `rejected`, `periods`, `runs` |
| `/projects` | `projects` |
| `/project` | `relations`, `related`, `activity`, `statements` |
| `/resources` | `resources` |
| `/resource` | `segments`, `events` |
| `/pricing` | `models` |
| `/catalog` | `dimensions` |
| `/run` | `statements`, `deltas` |
| `/statement` | `adjustments`; `items` and `rc-<n>`, the summaries of the line items and of the n-th related cost; `li-<n>` and `rc-<n>-li-<n>`, the metric tables of the items, which sort and carry no filter |

A paged listing, `/projects` and `/resources`, is sorted and filtered within
the page the API answered, which its row count says, and its next link carries
both parameters along. The timeline of a resource page draws every segment
whatever the segment table is filtered to.

A statement is rendered in sections, the project's own line items and then one
per related cost. A section opens with a summary table of its items, resource
type, resource, description, hours and total, sorted by total descending until
the viewer sorts it otherwise. The resource of a row leads to the item's detail
block below, and the blocks follow the summary's order and filter, so filtering
the summary to one resource shows that resource's detail alone. Every block
starts folded. The resource of a summary row leads to its block unfolded: the
link names the block in `open` and in its fragment, so the page comes back
with that block open and scrolled to, and any other block unfolds by hand. A
block's heading links the resource's page, and its table holds one row per
metric of every period, with the period's total as a row of its own. A description that
only repeats the item's type and id is left out. A value of exactly zero is
printed in the muted colour, so the amounts that are not zero stand out.

## The fleet at an instant and over a window

The resource list is read under one status, and that page is then read one of
two ways. The status is the API's own filter, chosen with the switch on the
`show` line above the table: `active` serves the rows whose state is not
deleted and is the default, `deleted` serves those alone, and `all` serves
both. A `status` that is none of the three is answered 400. Switching the
status drops the cursor, because a cursor positions a walk through one status
and means nothing in another. A deleted resource is only on the page when the
status admits it, so looking back starts with switching the status to `all`.

The two ways are the console's own filters, applied to the rows the API
served, and the switch on the `when` line chooses between them: **at an
instant**, which answers what runs right now or what ran at one moment, and
**over a window**, which answers what lived between two moments. The page
draws the inputs and the presets of the chosen way alone, so a window and an
instant are never asked for at once. Which way a request means is `mode`,
either `instant` or `window`; a request that names no mode means the window
when it carries `from` or `to`, and the instant otherwise. A `mode` that is
neither is answered 400. What the other way would be asked with is ignored,
so a stale `at` on a window page changes nothing, and switching ways drops the
parameters of the way being left.

Each of `at`, `from` and `to` is read as RFC 3339 or as the form the inputs
write, `2026-03-15T12:00`, which carries no zone and is read as UTC; one that
does not parse is answered 400.

The instant is `at`, and the page opens on it: a resource existed at it when
it was created at or before it and not deleted at or before it; a resource
whose history shows no create has no creation time and is taken to have
existed all along. Without `at` the instant is now, so the page opens on what
runs right now. The instant presets are now, which is no parameter at all, 24
hours ago, 7 days ago, the start of this month and the start of last month.
The instant input stays empty while nothing is pinned, so the page opens on
now without claiming an instant was chosen.

The window is `from` and `to`, half-open: a resource existed in it when it was
created before `to` and not deleted at or before `from`. Either bound may be
left out, which leaves the window open on that side, and a `to` that is not
after `from` is answered 400. A window with neither bound holds every row of
the page, whenever it lived. The window presets are the last 24 hours, the
last 7 days, this month, last month, and any time, which is the window with
neither bound.

The page asks the API for 1000 rows, the most one page carries, so that the
filters are applied to as much of the fleet as one call holds; the fleet of the
simulated month fits. A page the API followed with a cursor, or one reached by
a cursor, counts its rows as one page, and its next link carries the status,
the mode, the window and the instant along. The line beside the status switch
says how many of the rows the API served the filters kept and what they were
kept for, and a page the filters emptied says so in place of the rows.

Every row prints its lifetime in hours at two places: from its creation to
its deletion, or to now for a resource still there, whatever the page is read
at. A resource whose history starts without a create has no
creation time, which the API leaves null and the fold reports as
`history_starts_without_create`; the row prints `unknown` for its creation and
its lifetime, and the line above the table counts such rows. The resource page
names the event such a history starts with and counts the lifetime from it,
which is where the fold starts the intervals a run bills. The last payload of
a row is folded under a line that counts its keys, so the listing stays one
line per row until a payload is opened.

## Theme

The pages follow the operating system's light or dark setting on their own. The
form at the right end of the navigation bar pins one side: it posts `theme` as
`light`, `dark` or `auto` to `/theme`, together with `back`, the path of the
page it stands on. `light` and `dark` are kept in the cookie `theme` for a
year, and `auto` deletes it. The console answers 303 to `back`, or to `/` when
`back` is not a path of the console, so the form cannot send the viewer
elsewhere. A page renders the cookie's value as the `data-theme` attribute of
its root element, which the stylesheet reads, and a request without the cookie
renders no attribute. A `theme` that is none of the three is answered 400 on
the error page, and a GET of `/theme` 405.

Every page ends with a provenance panel naming the reads it was built from: each
Reporting API request as its method and path with the query string, and each
engine read under the name its query carries in
[`internal/console/store/queries.sql`](https://github.com/B42Labs/tally/blob/main/internal/console/store/queries.sql).

A failure renders one error page carrying the wrapped error: 400 for a parameter
that is missing or unreadable, 404 for a lookup that found nothing and for an
API 404, 502 for a Reporting API call that failed or that the API refused the
token for, and 503 for an engine query that failed or a stored document that
does not decode. A path the console has no page for is answered 404 on that same
error page, and a route asked with a method it does not answer 405.

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

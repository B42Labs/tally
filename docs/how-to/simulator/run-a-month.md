---
title: Run a simulated month against the dev cluster
description: Publish a generated OpenStack month onto the dev cluster's bus at a pace you set, and read back what the collector and the Reporting API took.
quadrant: how-to
audience: contributor
---

# Run a simulated month against the dev cluster

This guide publishes one generated OpenStack month onto the broker the dev
cluster's collector consumes from, paces it, stops it again, and reads the month
back through the Reporting API. What such a month holds is in
[the simulated OpenStack world](/explanation/the-simulated-openstack-world).

## Before you start

- A dev cluster from `make up`. The stack posts into its Reporting API, and
  `make simulator-up` rebuilds nothing that runs in the cluster and applies no
  migration, so a change to the Reporting API or to the migrations takes another
  `make up` first.
- Docker with `docker compose`, for the three containers of the stack: the
  broker, the collector image, and the simulator.
- A shell at the repository root, for the `make` targets and for `go run`.
- The month to render, as `SIM_PERIOD`. It has to lie in the past: `run` refuses
  a month that has not ended.
- The [simulator command line](/reference/command-line/tally-openstack-simulator)
  page, which states the routes of
  [the control endpoint](/reference/command-line/tally-openstack-simulator#the-control-endpoint)
  this guide calls.
- The [simulator settings](/reference/configuration/tally-openstack-simulator)
  page, which lists every `TALLY_SIM_` variable `make simulator-up` writes into
  `deploy/compose/.env`.

## Start the stack

1. Bring the dev cluster up, then start the simulator stack for the month:

   ```sh
   make up
   make simulator-up SIM_PERIOD=2026-07
   ```

   `SIM_CLOUD` defaults to `os-sim`, `SIM_SEED` to 1, and `SIM_FACTOR` to 744. A
   factor of 744 puts a 31-day month on the bus in an hour. `SIM_FAULTS` is
   empty, which is every fault switch off; it takes the switch names,
   comma-separated, as in `SIM_FAULTS=held-back`, which
   [switch on fault switches](/how-to/simulator/switch-on-faults) covers.
   `SIM_REGISTER_PROJECTS` is `false`, and `true` registers the month's tenants
   and Gardener projects with the dev registry before the first notification goes
   out, which
   [register simulated projects](/how-to/simulator/register-simulated-projects)
   covers; any other value is refused with
   `ERROR: SIM_REGISTER_PROJECTS must be true or false` before an image is
   built. `SIM_GARDEN_CLOUD` defaults to `garden-sim` and is the cloud the two
   Gardener rows are registered under.

2. Take the seven URLs the stack prints:

   - `http://127.0.0.1:15672`, the broker's management UI, guest/guest
   - `http://127.0.0.1:8090/metrics`, the collector
   - `http://127.0.0.1:8091/clock`, the simulator's control endpoint
   - `http://127.0.0.1:8091/metrics`, the simulator's inventory, the database
     exporter stand-in of
     [the metric series](/explanation/the-simulated-openstack-world#the-metric-series)
   - `https://api.tally.127-0-0-1.nip.io:8443/api/v1`, the Reporting API
   - `https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics`, the OTLP endpoint the
     series are pushed to
   - `https://vm.tally.127-0-0-1.nip.io:8443/targets`, the scrape targets of the
     dev cluster, where the `openstack-db-exporter` job goes green while the run
     publishes

3. Watch the first publish. The simulator waits for a consumer on the collector's
   `tally-notifications` queue before it, and `--wait-for-collector` bounds that
   wait, two minutes by default, with `0` disabling it. A wait that runs out ends
   the run with an error naming the fix: start the collector first, or pass
   `--wait-for-collector 0` to publish anyway.

## Pace the month

1. Finish the month at once instead of waiting the factor out. The route rebases
   the clock on the virtual instant it has reached and answers the clock
   document:

   ```sh
   curl -X PUT -d '{"factor": 0}' http://127.0.0.1:8091/clock
   ```

   ```json
   {"virtual_now":"2026-07-09T14:22:00Z","factor":0,"published":52,"total":15727,"held":0,"holding":false,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
   ```

2. Let the notifications a run with `SIM_FAULTS=held-back` keeps back out. The
   answer is the document as it stood the moment before the release, with `held`
   0 and `holding` false:

   ```sh
   curl -X POST http://127.0.0.1:8091/release
   ```

   ```json
   {"virtual_now":"2026-08-01T00:00:00Z","factor":744,"published":15643,"total":15727,"held":0,"holding":false,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
   ```

3. Build a backlog on the durable queue and drain it again. The messages are
   persistent, so a backlog survives a broker restart as well:

   ```sh
   docker compose -f deploy/compose/compose.yaml stop collector
   docker compose -f deploy/compose/compose.yaml start collector
   ```

4. Rerun a run whose publish the broker did not confirm, with the same seed,
   period, and cloud. Such a publish ends the run with exit status 1, a rerun
   renders the same message ids, and ingestion deduplicates whatever was already
   delivered. SIGINT and SIGTERM stop a run with exit status 0, and what went out
   stays out.

## Stop the stack

1. Remove the containers, the outbox volume, and `deploy/compose/.env`, so the
   next `simulator-up` starts from a stack that carries nothing of the last one:

   ```sh
   make simulator-down
   ```

   It resets the stack and not the dev reporting database: what a run already
   delivered stays ingested, and no subcommand of `tally-reporting-admin` deletes
   an event. Running the same period again under another seed or another cloud
   adds a second, disjoint set of rows beside the first and inflates the usage
   the API reports.

2. Start the period over by dropping the ingested data first, with
   `make down && make up`, or with:

   ```sh
   TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable' \
     go run ./cmd/tally-reporting-admin migrate-down-to 0
   make migrate
   ```

## Read the month

1. Issue an admin token and list what the month booked under the cloud:

   ```sh
   token="$(TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable' \
     go run ./cmd/tally-reporting-admin create-api-token --role admin \
     --description 'openstack simulator')"
   curl --cacert tally-ca.crt -H "Authorization: Bearer $token" \
     'https://api.tally.127-0-0-1.nip.io:8443/api/v1/resources?cloud=os-sim'
   ```

   A run started with `SIM_REGISTER_PROJECTS=true` carries a token already:
   `deploy/compose/.env` holds it as `TALLY_SIM_API_TOKEN`. Which tokens the
   admin CLI issues, and how one is ended, is in
   [issue and revoke credentials](/how-to/openstack/issue-and-revoke-credentials).

## Check the result

1. Ask the collector whether it holds its broker connection and its outbox:

   ```sh
   curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8090/readyz
   ```

   ```text
   200
   ```

2. Read the counters on `http://127.0.0.1:8090/metrics`:

   - `tally_collector_consumed_total` grows per event type while the month goes
     out, and carries the three `octavia.loadbalancer.*.end` series among the
     others.
   - `tally_collector_skipped_total` climbs per type beside it, and it climbs
     faster: 13915 of the month's 15727 notifications are ones the mapping claims
     nothing for.
   - Neither counter carries an `event_type="other"` series. The month's 83 types
     stay inside the bound of 100 label values the two of them share.
   - `tally_collector_unparseable_total` stays 0. Anything else is a rendered
     body the collector could not read.
   - `tally_collector_delivered_total` rises above 0 within two minutes at factor
     744. A counter that stays at 0 means the events sit in the outbox and the
     Reporting API is not taking them.

3. Hold the totals against the seed. Seed 1 over `2026-07` renders 15727
   notifications, 1812 of them billable, and 83 distinct `event_type` values. The
   shape of a month is the seed's alone, so those counts hold on every cloud.
   They are the counts of a run with every fault switch off.

4. Read the collector's log. It shows neither an `x509` error nor a `401` when
   the CA and the token are right:

   ```sh
   docker compose -f deploy/compose/compose.yaml logs collector
   ```

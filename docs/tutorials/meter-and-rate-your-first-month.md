---
title: Meter and rate your first month
description: Point the metering engine at the month lesson 2 ingested, import a pricing model, run July 2026, and export and read the project statements it rates.
quadrant: tutorial
audience: all
---
<!-- Shown output captured on 2026-09-07 from commit d0d5905 with kind v0.32.0, kubectl v1.36.1, Docker Desktop 4.86.0, Go 1.27.1 on macOS 15.7.4. -->

# Meter and rate your first month

In this lesson you point the metering engine at the reporting database lesson 2
filled and at its own database, import a pricing model, run the month, read
what the run recorded, export it, and read one project's statement.

At the end you hold one completed, not finalized run of July 2026 in the engine
database, its run id in a variable of your shell, and the JSON export with six
project statements under `~/tally-tutorial/2026-07`.

This lesson takes about 5 minutes.

## Before you start

- The state lesson 2 leaves: the kind cluster with the dev overlay, the
  simulator stack holding its 84 notifications, the month of July 2026 in the
  reporting database, `tally-ca.crt` at the repository root, and that shell at
  the repository root.
- `jq` on the path, the one lesson 1 asks for.
- The engine commands below run with `go run` from the repository root, the way
  lesson 1 ran the admin CLI.

If you closed that shell, the first step of this lesson exports everything a
run needs. `TALLY_REPORTING_DB_URL` and `TALLY_API_TOKEN`, the two variables
lesson 1 exported, are not read by the engine, so a new shell may be missing
both. Lesson 4 needs neither.

## Point the engine at both databases

1. Export the four inputs of a run and open the path to the metrics store:

   ```sh
   export TALLY_ENGINE_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_engine?sslmode=disable'
   export TALLY_ENGINE_REPORTING_DB_URL='postgres://tally_engine:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
   export TALLY_ENGINE_COUNTER_SOURCES=deploy/kubernetes/overlays/dev/counter-sources.yaml
   export TALLY_ENGINE_VM_URL=http://127.0.0.1:8428
   kubectl --context kind-tally -n tally port-forward svc/victoriametrics 8428:8428 &
   ```

   ```text
   Forwarding from 127.0.0.1:8428 -> 8428
   Forwarding from [::1]:8428 -> 8428
   ```

   - `TALLY_ENGINE_DB_URL` is the engine's own database, `tally_engine`, on the
     Gateway's TCP listener, where the run row and its records go.
   - `TALLY_ENGINE_REPORTING_DB_URL` reads the reporting database lesson 2
     filled, as the login role `tally_engine`, a reader the dev overlay
     creates.
   - `TALLY_ENGINE_COUNTER_SOURCES` names the dev cluster's counter sources
     file, which declares where the egress counter of an instance comes from.
   - `TALLY_ENGINE_VM_URL` is the metrics store the port-forward publishes on
     `127.0.0.1:8428`, and that forward is what lets the run read the egress
     counter from the store. The engine reaches the store over plain http, so
     the Gateway's https hostname is not used here.

   The two `Forwarding from` lines are what the background job prints. A
   `Handling connection for 8428` line appears each time the run queries the
   store, and both are your own.

   The values are the ones the Phase 3 drill of this repository exports, and
   [run a period](/how-to/engine/run-a-period) states what each variable
   defaults to.

2. Check that the store answers through the forward:

   ```sh
   curl -s http://127.0.0.1:8428/health
   ```

   ```text
   OK
   ```

   `OK` has to match. A connection refused here means the port-forward is not
   running, so start it again with the last line of the block above.

## Import the pricing model

1. Import the model:

   ```sh
   go run ./cmd/tally-engine pricing import pricing/2026-03.yaml
   ```

   ```text
   imported pricing model 2026-03 valid from 2026-03-01T00:00:00Z
   ```

   The line has to match. `pricing/2026-03.yaml` is the model this repository
   ships, and [import a pricing model](/how-to/engine/import-a-pricing-model)
   covers writing another. It is valid from March 2026, so it prices July.

   It prices an instance by `vcpus`, `ram_gb`, `disk_gb` and `egress_gb`, a
   volume by `size_gb` and a floating IP by `count`. It prices neither `image`
   nor `loadbalancer`, the two types the run below records as unpriced. What a
   model file may declare is in the
   [pricing model file](/reference/formats/pricing-model) reference.

2. List what the engine database holds:

   ```sh
   go run ./cmd/tally-engine pricing list
   ```

   ```text
   2026-03 valid_from=2026-03-01T00:00:00Z currency=EUR imported_at=2026-09-07T12:46:13Z
   ```

   The version, `valid_from` and `currency=EUR` have to match. `imported_at` is
   your own.

3. A second import of the same file changes nothing:

   ```sh
   go run ./cmd/tally-engine pricing import pricing/2026-03.yaml
   ```

   ```text
   pricing model 2026-03 already imported
   ```

   The line has to match. Nothing is stored, and the version already in the
   database stays the one a run rates against.

## Run the month

1. Meter and rate July 2026:

   ```sh
   go run ./cmd/tally-engine run --period 2026-07
   ```

   ```text
   run 66a9c8b9-2e20-43ef-9f8e-7e421e1832f9 completed for 2026-07 with pricing model 2026-03
   metered 867 candidates into 945 usage records, 3101 rated records and 6 project statements
   warnings recorded in runs.stats: 38 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 6 unregistered projects
   ```

   The `metered` line and the warnings line have to match. The run id on the
   first line is your own. `2026-07` is the period this track uses everywhere,
   and the run takes about a second.

   The run is completed, not finalized:
   [the lifecycle](/explanation/billing-period-lifecycle-and-corrections#the-lifecycle)
   sets the two states apart, and the billing track finalizes.

2. Put the id from the first line of your own output in place of this one:

   ```sh
   export RUN_ID=66a9c8b9-2e20-43ef-9f8e-7e421e1832f9
   ```

   Every command below reads `RUN_ID`.

3. Read what stands there instead if the run printed something else:

   - `no pricing model is valid for this period` means the import above was
     skipped. Import the model and run the month again.
   - An error opening on `another run of this period is in progress` means the
     hourly tick of the cluster's `tally-engine` scheduler holds the period.
     Wait a minute and run the month again.
   - A `superseded run <run id>` line between the second and the third line
     means such a tick metered the month first. It changes nothing below.
   - A non-zero `counter` count on the warnings line means the port-forward
     died before the run read the store. Start it again and run the month
     again; the new run supersedes this one.
   - `reading the counter sources deploy/kubernetes/overlays/dev/counter-sources.yaml: open deploy/kubernetes/overlays/dev/counter-sources.yaml: no such file or directory`
     means the shell is not at the repository root, or the export of
     `TALLY_ENGINE_COUNTER_SOURCES` was skipped.

## Read the warnings

1. Read the last line of the run once more. Three of its seven classes are
   non-zero:

   ```text
   warnings recorded in runs.stats: 38 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 6 unregistered projects
   ```

   Unpriced resource types, 2: `openstack/image` and `openstack/loadbalancer`,
   the two types the model prices nothing for. The run met 9 images and 5 load
   balancers, the counts `jq .stats` shows below, and each of them is left off
   every statement.

   Unregistered projects, 6: the six tenant ids of the month, because the
   project registry is empty. Each is billed standalone under its id. The ids
   stand with their resource counts in the `unregistered_projects` list below,
   at 309, 16, 447, 15, 16 and 16 resources. The billing track's registration
   lesson fills the registry, and
   [register simulated projects](/how-to/simulator/register-simulated-projects)
   is the how-to behind it.

   Metering warnings, 38: every one of them is `history_starts_without_create`,
   a resource whose stored history begins with a later transition because its
   create is among the 84 notifications the simulator holds back. They are 28
   instances, 9 volumes and 1 floating IP. 23 of the instances are CI runners
   of the CI tenant `10e287d5788957a2a331cabd1b5dccdf`, 3 are workers of the
   Gardener tenant `005be5adeef3d87e280d03d9d57c38b4`, 1 is a worker of the
   Gardener tenant `e31f9083a7e5ee15071a3bd53cb2bac7` and 1 is an instance of
   the classic project `d5a8024946ddf673277b9e2490643a2c`. The 9 volumes belong
   to `005be5adeef3d87e280d03d9d57c38b4` and the floating IP to
   `e31f9083a7e5ee15071a3bd53cb2bac7`. These counts and these ids are the
   seed's and have to match. The billing track's correction lesson settles the
   38 once the held share is released.

   The other four classes read 0: no counter source failed, no project was
   claimed twice, no kickback was dropped and no usage field was unreadable. A
   non-zero `counter` count means the port-forward died before the run read the
   store, the fourth mismatch above.

## Export the statements

1. Export the run as JSON:

   ```sh
   mkdir -p ~/tally-tutorial && go run ./cmd/tally-engine export --run "$RUN_ID" --format json --out ~/tally-tutorial/2026-07
   ```

   ```text
   run 66a9c8b9-2e20-43ef-9f8e-7e421e1832f9 exported for 2026-07 as json into /Users/berendt/tally-tutorial/2026-07
   wrote run.json and 6 statements
   wrote kickbacks.json with 0 kickbacks
   ```

   The second and the third line have to match. The run id and the home
   directory on the first line are your own. JSON is the format this track
   exports in because a statement is readable no other way, and
   [export a run](/how-to/engine/export-a-run) covers the CSV table.

2. List what the export wrote:

   ```sh
   ls ~/tally-tutorial/2026-07
   ```

   ```text
   kickbacks.json
   run.json
   statement-os-sim%2F005be5adeef3d87e280d03d9d57c38b4.json
   statement-os-sim%2F018504a6cc10019a40e3f9eef4dae529.json
   statement-os-sim%2F10e287d5788957a2a331cabd1b5dccdf.json
   statement-os-sim%2F34e991db9fc6466f8ca69b43f70fce65.json
   statement-os-sim%2Fd5a8024946ddf673277b9e2490643a2c.json
   statement-os-sim%2Fe31f9083a7e5ee15071a3bd53cb2bac7.json
   ```

   The eight names have to match. A statement file is named after the cloud and
   the tenant id joined by a slash, escaped, which is where the `%2F` comes
   from.

3. Read the run's stats off the index, with the 38 metering warnings counted
   by resource type:

   ```sh
   jq '.stats | .metering_warnings |= (group_by(.resource_type) | map({code: .[0].code, resource_type: .[0].resource_type, count: length}))' ~/tally-tutorial/2026-07/run.json
   ```

   ```json
   {
     "unpriced": [
       {
         "count": 9,
         "platform": "openstack",
         "resource_type": "image"
       },
       {
         "count": 5,
         "platform": "openstack",
         "resource_type": "loadbalancer"
       }
     ],
     "candidates": 867,
     "statements": 6,
     "snapshot_at": "2026-09-07T12:46:14.395319Z",
     "rated_records": 3101,
     "usage_records": 945,
     "metering_warnings": [
       {
         "code": "history_starts_without_create",
         "resource_type": "floating_ip",
         "count": 1
       },
       {
         "code": "history_starts_without_create",
         "resource_type": "instance",
         "count": 28
       },
       {
         "code": "history_starts_without_create",
         "resource_type": "volume",
         "count": 9
       }
     ],
     "unregistered_projects": [
       {
         "cloud": "os-sim",
         "resources": 309,
         "project_id": "005be5adeef3d87e280d03d9d57c38b4"
       },
       {
         "cloud": "os-sim",
         "resources": 16,
         "project_id": "018504a6cc10019a40e3f9eef4dae529"
       },
       {
         "cloud": "os-sim",
         "resources": 447,
         "project_id": "10e287d5788957a2a331cabd1b5dccdf"
       },
       {
         "cloud": "os-sim",
         "resources": 15,
         "project_id": "34e991db9fc6466f8ca69b43f70fce65"
       },
       {
         "cloud": "os-sim",
         "resources": 16,
         "project_id": "d5a8024946ddf673277b9e2490643a2c"
       },
       {
         "cloud": "os-sim",
         "resources": 16,
         "project_id": "e31f9083a7e5ee15071a3bd53cb2bac7"
       }
     ]
   }
   ```

   The three lists are the warnings line as lists: the 38 metering warnings
   counted by resource type, the 2 unpriced resource types and the 6
   unregistered projects. The four counts beside them are the `metered` line
   of the run. `snapshot_at` is your own, the instant the run opened its
   reporting snapshot.

4. An export into a directory that already holds files is refused:

   ```sh
   go run ./cmd/tally-engine export --run "$RUN_ID" --format json --out ~/tally-tutorial/2026-07
   ```

   ```text
   Error: --out: /Users/berendt/tally-tutorial/2026-07 is not empty, and an export does not remove what an earlier one left there
   exit status 1
   ```

   Nothing is written, and a second export needs a directory of its own. The
   path in the line is your own.

## Read a project statement

1. Read the total of every statement:

   ```sh
   jq -r '"\(.project_id)\t\(.total)\t\(.currency)"' ~/tally-tutorial/2026-07/statement-*.json
   ```

   ```text
   005be5adeef3d87e280d03d9d57c38b4	3661.32	EUR
   018504a6cc10019a40e3f9eef4dae529	876.63	EUR
   10e287d5788957a2a331cabd1b5dccdf	698.29	EUR
   34e991db9fc6466f8ca69b43f70fce65	598.39	EUR
   d5a8024946ddf673277b9e2490643a2c	905.10	EUR
   e31f9083a7e5ee15071a3bd53cb2bac7	651.24	EUR
   ```

   The six totals have to match: they are the seed's usage rated at the model's
   prices. The largest is `3661.32` for `005be5adeef3d87e280d03d9d57c38b4`, one
   of the two Gardener tenants, and the reads below open that statement. `jq`
   1.8.1 prints the amounts as the file carries them, while an older `jq`
   re-renders `905.10` as `905.1`.

2. Count its line items:

   ```sh
   jq '.line_items | length' ~/tally-tutorial/2026-07/statement-os-sim%2F005be5adeef3d87e280d03d9d57c38b4.json
   ```

   ```text
   309
   ```

   The count has to match. There is one line item per resource the project is
   billed for: 169 instances, 137 volumes and 3 floating IPs on this statement.
   Images and load balancers carry no line item at all, because the model
   prices neither.

3. Read the first line item:

   ```sh
   jq '.line_items[0]' ~/tally-tutorial/2026-07/statement-os-sim%2F005be5adeef3d87e280d03d9d57c38b4.json
   ```

   ```json
   {
     "resource_type": "floating_ip",
     "resource_id": "9aca6b44-4a7f-4c45-b4fc-074281931c58",
     "platform": "openstack",
     "description": "floating_ip 9aca6b44-4a7f-4c45-b4fc-074281931c58",
     "periods": [
       {
         "state": "active",
         "hours": 563.57,
         "usage": {
           "count": 1.0000
         },
         "cost": {
           "count": 2.82,
           "total": 2.82
         },
         "state_modifier": 1.0000
       }
     ],
     "total": 2.82
   }
   ```

   It is a floating IP with one period. A period is one state of one resource
   for a span of hours: `usage` holds the quantity per dimension, `count` 1.0000
   here at four decimal places, `cost` what each dimension cost and their
   `total` at two places, and `state_modifier` the factor the state is billed
   at, 1.0000 for `active`. `hours` 563.57 is how long the address existed
   within the month. The whole document has to match.

4. Read the first instance line item:

   ```sh
   jq '[.line_items[] | select(.resource_type == "instance")][0]' ~/tally-tutorial/2026-07/statement-os-sim%2F005be5adeef3d87e280d03d9d57c38b4.json
   ```

   ```json
   {
     "resource_type": "instance",
     "resource_id": "01775514-2a32-4ba5-8b1e-4e2aeac85b8c",
     "platform": "openstack",
     "description": "c1.large instance",
     "periods": [
       {
         "state": "active",
         "hours": 12.00,
         "usage": {
           "disk_gb": 0.0000,
           "egress_gb": 72.4533,
           "ram_gb": 8.0000,
           "vcpus": 4.0000
         },
         "cost": {
           "disk_gb": 0.00,
           "egress_gb": 6.52,
           "ram_gb": 0.48,
           "total": 7.96,
           "vcpus": 0.96
         },
         "state_modifier": 1.0000
       }
     ],
     "total": 7.96
   }
   ```

   An instance carries four dimensions, and the document has to match.
   `egress_gb`, 72.4533 here, is in no notification: it is the counter the run
   read from the metrics store through the port-forward, the
   `ceilometer_network_outgoing_bytes_total` series lesson 2 pushed, summed over
   the period and converted to gibibytes. `disk_gb` is 0 because `c1.large` has
   no root disk, and the worker on it boots from a volume that is billed as a
   volume line item instead.

5. Read what the project owes:

   ```sh
   jq '{total, currency}' ~/tally-tutorial/2026-07/statement-os-sim%2F005be5adeef3d87e280d03d9d57c38b4.json
   ```

   ```json
   {
     "total": 3661.32,
     "currency": "EUR"
   }
   ```

   The total is the sum of the line items, each dimension rounded once to two
   places and then summed, in the one currency the model names. How the amounts
   are held and rounded is in
   [money and rounding](/explanation/money-and-rounding), and how they were
   derived in
   [metering separated from rating](/explanation/metering-separated-from-rating).
   Every member of the document is in
   [statements](/reference/formats/exports#statements).

## What you learned

- A run meters first, into usage records per state and resource, and rates
  those records against the model afterwards:
  [why two stages](/explanation/metering-separated-from-rating#why-two-stages).
- `egress_gb` is a counter, read from the metrics store through the counter
  sources file rather than from a notification:
  [counters](/explanation/metering-separated-from-rating#counters).
- The amounts on a statement are decimals, rounded once per dimension:
  [why rounding happens once](/explanation/money-and-rounding#why-rounding-happens-once).
- A project the registry does not hold is billed standalone under its id:
  [projects as first-class entities](/explanation/project-registry-relations-and-attribution#projects-as-first-class-entities).
- A completed run is not final. Another run of the period supersedes it, and
  finalization is a step of its own that this track does not take:
  [runs before finalization](/explanation/billing-period-lifecycle-and-corrections#runs-before-finalization).

## Where to go next

[Watch the month in Grafana](/tutorials/watch-the-month-in-grafana) reads the
month you just rated off the dashboards.

It starts from the state this lesson leaves behind: everything lesson 2 left,
plus the pricing model `2026-03` and one completed, not finalized run of
`2026-07` in the engine database, the four `TALLY_ENGINE_` variables and
`RUN_ID` in the shell, the JSON export under `~/tally-tutorial/2026-07`, and
the port-forward on `127.0.0.1:8428`, which stays running in that shell.

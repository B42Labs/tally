---
title: Book the late events as a credit note
description: Release the notifications the simulator held back, see the engine find them as late, book them as a correction of the closed month, read the credit notes and close the correction.
quadrant: tutorial
audience: all
---
<!-- Shown output captured on 2026-09-07 from commit 01d2aff with kind v0.32.0, kubectl v1.36.1, Docker Desktop 4.86.0, Go 1.27.1 on macOS 15.7.4. -->

# Book the late events as a credit note

In this lesson you let the simulator publish the 84 notifications it held back,
see the engine find them as late arrivals, book them as a correction of the
month you closed, read the credit notes the correction writes, and close the
correction.

At the end the simulator container has exited after publishing its held share,
the engine database holds one finalized correction of `2026-07` beside the
finalized run, the credit notes are under `~/tally-tutorial/2026-07-notes`, and
`CORRECTION_ID` is in the shell. This is the last lesson of the billing track.

This lesson takes about 5 minutes.

## Before you start

- The state
  [Finalize the month and export it](/tutorials/finalize-the-month-and-export-it)
  leaves:

  - everything Attribute a tenant to its Gardener project left;
  - `2026-07` finalized on `RUN_ID`;
  - the three export directories `~/tally-tutorial/2026-07-final`,
    `2026-07-final-again` and `2026-07-final-csv`;
  - the simulator still holding its 84 notifications.

- The shell that holds the port-forward Meter and rate your first month
  started: a correction meters the month again and reads the egress counter
  from the store through it.
- Docker Desktop running, with the three containers of the simulator stack.
- `jq` on the path, which the `make check-tools` of lesson 1 called.

If you closed that shell, restore it with this block:

```sh
export TALLY_ENGINE_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_engine?sslmode=disable'
export TALLY_ENGINE_REPORTING_DB_URL='postgres://tally_engine:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
export TALLY_ENGINE_COUNTER_SOURCES=deploy/kubernetes/overlays/dev/counter-sources.yaml
export TALLY_ENGINE_VM_URL=http://127.0.0.1:8428
kubectl --context kind-tally -n tally port-forward svc/victoriametrics 8428:8428 &
export RUN_ID="$(jq -r .run_id ~/tally-tutorial/2026-07-final/run.json)"
```

No registry call follows, so this lesson needs no token and no `tally-ca.crt`.
The correction meters, so it needs the four engine variables and the
port-forward. `RUN_ID` is read back off the finalized export's `run.json`, and
this lesson reads it only to compare it with what `periods list` prints.

A connection error from any `go run` command means the cluster is not up, and
`kind get clusters` then prints nothing. That is cured by
[Set up your local Tally](/tutorials/set-up-your-local-tally).

## Release the held share

1. Read the simulator's clock:

   ```sh
   curl -s http://127.0.0.1:8091/clock
   ```

   ```json
   {"virtual_now":"2026-07-02T07:30:56Z","factor":0,"published":15643,"total":15727,"held":84,"holding":true,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
   ```

   `factor` 0, `published` 15643, `total` 15727, `held` 84 and `holding` true
   have to match, the state Simulate a month of OpenStack left the simulator
   in. `virtual_now` is your own.

2. Let the held notifications out:

   ```sh
   curl -s -X POST http://127.0.0.1:8091/release
   ```

   ```json
   {"virtual_now":"2026-07-02T07:30:56Z","factor":0,"published":15643,"total":15727,"held":0,"holding":false,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
   ```

   The route answers with the document as it stood the moment before the
   release, with `held` 0 and `holding` false, the two members the release
   changed. `published` 15643, `total` 15727, `held` 0 and `holding` false have
   to match, and `virtual_now` is your own.

   The 84 notifications go onto the bus at once, each under the July timestamp
   it always carried. That is what a late arrival at the Reporting API is: an
   event inside July received after the run that billed July read the month.
   `held-back` is the switch that kept them off the bus:
   [the fault switches](/explanation/the-simulated-openstack-world#the-fault-switches).

   A 409 with `the held-back notifications were already released`, or a refused
   connection, means the release was sent before, because the simulator exits
   once its share is out. A 409 with
   `the month is still publishing; release once /clock reports holding true`
   means
   [Simulate a month of OpenStack](/tutorials/simulate-a-month-of-openstack) did
   not finish.

## Watch the notifications arrive

1. Sum the collector's counters over their `event_type` labels:

   ```sh
   curl -s http://127.0.0.1:8090/metrics | awk '
     /^tally_collector_consumed_total/ { c += $2 }
     /^tally_collector_skipped_total/ { s += $2 }
     /^tally_collector_unparseable_total/ { u += $2 }
     /^tally_collector_buffer_depth/ { d = $2 }
     END { print "consumed", c, "skipped", s, "unparseable", u, "depth", d }'
   ```

   ```text
   consumed 1812 skipped 13915 unparseable 0 depth 0
   ```

   Repeat the read until `depth` reads 0. The four numbers have to match. 1812
   is the 1728 Simulate a month of OpenStack counted plus the 84, 13915 is
   unchanged because the held share carried only billable transitions, and a
   depth of 0 is the outbox drained into the Reporting API.

2. Read the clock once more:

   ```sh
   curl -sS http://127.0.0.1:8091/clock
   ```

   ```text
   curl: (7) Failed to connect to 127.0.0.1 port 8091 after 0 ms: Couldn't connect to server
   ```

   The simulator's run ended once the whole month was out, so the control
   endpoint answers nothing and the container has exited. `-S` shows the error
   that `-s` alone hides, and the wording after `(7)` is your own `curl`'s.

   From now on the `openstack-db-exporter` scrape job has no target, so
   `TallyScrapeTargetDown` fires for it about five minutes later, beside the
   `ceilometer` one Watch the month in Grafana found. That is the designed
   state between simulator runs.

## Ask the month what arrived late

1. Report what reached the reporting database after the finalized run read it:

   ```sh
   go run ./cmd/tally-engine detect-late --period 2026-07
   ```

   ```text
   run 77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 read 2026-07 at 2026-09-07T21:28:09Z
   os-sim/openstack/floating_ip/c8d4f029-050e-488c-a7d3-6e93f99561ff: 1 late events, last received 2026-09-07T22:05:22Z
   os-sim/openstack/floating_ip/e04358d1-1601-403c-b0d8-25f6f708c8ba: 1 late events, last received 2026-09-07T22:05:22Z
   os-sim/openstack/image/6a493a19-c938-461b-883f-5444a7310e78: 1 late events, last received 2026-09-07T22:05:22Z
   ```

   77 more lines of the same shape follow, one per resource, and the report
   ends with these two lines:

   ```text
   os-sim/openstack/volume/f74f35a4-f24f-49e3-8952-21554eefbebe: 1 late events, last received 2026-09-07T22:05:22Z
   book them with tally-engine correct --period 2026-07
   ```

   The report names 81 resources, 78 of them with one late event and 3 with
   two, which is the 84: 62 instances, 15 volumes, 2 floating IPs, 1 image and
   1 load balancer. The resource ids and their counts are the seed's and have
   to match, while the run id, which is `RUN_ID`, the snapshot and every
   `last received` instant are your own.

   A late event is one whose `received_at` is after the instant the finalized
   run read the month and whose timestamp is inside July. The report reads and
   changes nothing:
   [corrections](/explanation/billing-period-lifecycle-and-corrections#corrections).

   `no events arrived later` means the collector has not delivered the share
   yet, which the counter read above shows. An error
   `2026-07 has no finalized run, and late events are late against one: tally-engine run --period 2026-07 and tally-engine finalize produce it`
   means Finalize the month and export it was skipped.

## Book them as a correction

1. Meter the month again and diff it against the finalized run:

   ```sh
   go run ./cmd/tally-engine correct --period 2026-07
   ```

   ```text
   run 9a8770d7-89d9-45dc-a127-7edfe77b9018 completed as a correction of run 77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 for 2026-07 with pricing model 2026-03
   metered 871 candidates into 996 usage records and 3265 rated records
   184 deltas and 5 adjustment deltas in 6 credit notes
   warnings recorded in runs.stats: 0 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 0 unregistered projects
   ```

   The `metered` line, `184 deltas and 5 adjustment deltas in 6 credit notes`
   and the warnings line have to match. Both run ids are your own, the second
   being `RUN_ID`.

   A correction is a run of its own that names the run it corrects. It meters
   the month again in full with the finalized run's pricing model, diffs the
   amounts against the finalized run per resource, project and dimension, and
   stores the non-zero deltas. The finalized run stays as it is.

   The counts moved because the 84 notifications changed the histories. There
   are 871 candidates rather than 867: the four new ones are resources whose
   create and delete were both held, so the finalized run never saw them. The
   usage records are 996 and the rated records 3265, rather than 945 and 3101.
   The warnings line reads `0 metering`: the 38
   `history_starts_without_create` warnings of Meter and rate your first month
   are gone because every create is in the history now. The 184 deltas reach
   every one of the six projects, and all five adjustment lines of the month
   moved with their bases, which is the `5 adjustment deltas`.

   Which way a delta goes depends on which notification was held. A held delete
   left the finalized run without the resource's end, so it billed the resource
   to the end of the month, and the correction credits the hours after the
   delete. A held create left it without the resource's start, so it billed
   nothing before the first event it had, and the correction debits the hours
   the resource lived. Both are among the 84: of the CI tenant's 37 runners in
   its note, 20 are credits and 17 are debits, and across all six notes 35 line
   items are credits and 36 debits.

2. Put the first id on the first line of your own output in place of this one:

   ```sh
   export CORRECTION_ID=9a8770d7-89d9-45dc-a127-7edfe77b9018
   ```

   Every command below reads `CORRECTION_ID`.

3. Read what stands there instead if the correction printed something else:

   - An error opening on `the billing period is not finalized` means Finalize
     the month and export it was skipped.
   - `no deltas: the finalized numbers of 2026-07 stand` on the third line
     means the release never reached the database, which the counter read above
     shows.
   - An error opening on `another run of this period is in progress` means
     another `tally-engine correct` of this period is live, in this shell or in
     another one. Let that run end; the hourly tick is not it, because the tick
     leaves a finalized month alone.
   - A non-zero `counter` count on the warnings line means the port-forward
     died before the correction read the store. Start it again and run the
     correction again.

## Read the credit notes

1. Export the correction:

   ```sh
   go run ./cmd/tally-engine export --run "$CORRECTION_ID" --format json --out ~/tally-tutorial/2026-07-notes
   ```

   ```text
   run 9a8770d7-89d9-45dc-a127-7edfe77b9018 exported for 2026-07 as json into /Users/berendt/tally-tutorial/2026-07-notes
   wrote run.json and 6 credit notes
   wrote kickbacks.json with 1 kickback deltas
   ```

   `wrote run.json and 6 credit notes` and
   `wrote kickbacks.json with 1 kickback deltas` have to match. The run id and
   the home directory are your own. A correction is exported the way a run is,
   and what it writes is a credit note per project and the kickback deltas
   rather than statements and kickbacks.

2. List what the export wrote:

   ```sh
   ls ~/tally-tutorial/2026-07-notes
   ```

   ```text
   credit-note-garden-sim%2Falpha.json
   credit-note-garden-sim%2Fbeta.json
   credit-note-os-sim%2F018504a6cc10019a40e3f9eef4dae529.json
   credit-note-os-sim%2F10e287d5788957a2a331cabd1b5dccdf.json
   credit-note-os-sim%2F34e991db9fc6466f8ca69b43f70fce65.json
   credit-note-os-sim%2Fd5a8024946ddf673277b9e2490643a2c.json
   kickbacks.json
   run.json
   ```

   Six `credit-note-<key>.json` files stand beside `run.json` and
   `kickbacks.json`, one per project a delta reached. The eight names have to
   match.

3. List every credit note with its total:

   ```sh
   jq -r '.statements[] | "\(.cloud)/\(.project_id)\t\(.total)"' ~/tally-tutorial/2026-07-notes/run.json
   ```

   ```text
   garden-sim/alpha	-513.91
   garden-sim/beta	10.21
   os-sim/018504a6cc10019a40e3f9eef4dae529	-2.58
   os-sim/10e287d5788957a2a331cabd1b5dccdf	-580.49
   os-sim/34e991db9fc6466f8ca69b43f70fce65	18.28
   os-sim/d5a8024946ddf673277b9e2490643a2c	225.72
   ```

   One signed total per note, a negative one a credit and a positive one a
   debit. All six have to match. The CI tenant's is the largest credit,
   `alpha`'s the next, and the classic project
   `d5a8024946ddf673277b9e2490643a2c` carries the largest debit. The CI tenant,
   `alpha`, `beta` and that classic project are the four tenants the 38
   warnings of Meter and rate your first month named, and the two other Acme
   members got a note as well, from held transitions of their own.

4. Read the head of the CI tenant's note:

   ```sh
   jq '{corrects_run_id, items: (.line_items | length), base_delta, adjustments, net_delta, kickback_delta, total}' ~/tally-tutorial/2026-07-notes/credit-note-os-sim%2F10e287d5788957a2a331cabd1b5dccdf.json
   ```

   ```json
   {
     "corrects_run_id": "77fb51f8-82c9-4d52-98a2-7fd3e0b43c53",
     "items": 37,
     "base_delta": -682.93,
     "adjustments": [
       {
         "type": "discount",
         "relation_type": "managed_by",
         "relation_target": "cloudhouse",
         "relation_id": "2ccfc925-6f65-41e5-a180-4d12d25e283a",
         "scope": "all",
         "rate": 0.150000,
         "old": -104.74,
         "new": -2.30,
         "delta": 102.44
       },
       {
         "type": "kickback",
         "relation_type": "managed_by",
         "relation_target": "cloudhouse",
         "relation_id": "2ccfc925-6f65-41e5-a180-4d12d25e283a",
         "scope": "all",
         "rate": 0.100000,
         "old": 59.36,
         "new": 1.31,
         "delta": -58.05
       }
     ],
     "net_delta": -580.49,
     "kickback_delta": -58.05,
     "total": -580.49
   }
   ```

   `corrects_run_id` is `RUN_ID` and is your own, and the rest has to match.
   The note carries 37 line items and a `base_delta` of -682.93, the corrected
   base cost being 15.36 in place of 698.29. Each `adjustments` entry carries
   `old`, `new` and `delta`: the discount moves from -104.74 to -2.30, fifteen
   percent of the corrected base, so its delta is 102.44; the kickback moves
   from 59.36 to 1.31, ten percent of the corrected net of 13.06, so its delta
   is -58.05. `net_delta` -580.49 is the base delta plus the discount delta,
   `kickback_delta` is the kickback's delta, and `total` is the net delta.

5. Read the first line item of that note:

   ```sh
   jq '.line_items[0]' ~/tally-tutorial/2026-07-notes/credit-note-os-sim%2F10e287d5788957a2a331cabd1b5dccdf.json
   ```

   ```json
   {
     "resource_type": "instance",
     "resource_id": "06306b76-171b-4a72-a8d9-1c4b13583ed6",
     "platform": "openstack",
     "dimensions": {
       "disk_gb": {
         "old": 0.00,
         "new": 0.02,
         "delta": 0.02
       },
       "egress_gb": {
         "old": 0.00,
         "new": 0.01,
         "delta": 0.01
       },
       "ram_gb": {
         "old": 0.00,
         "new": 0.01,
         "delta": 0.01
       },
       "vcpus": {
         "old": 0.00,
         "new": 0.02,
         "delta": 0.02
       }
     },
     "total": 0.06
   }
   ```

   One runner with its four `dimensions`, each as `old`, `new` and `delta`, and
   a total of 0.06. Every `old` is 0.00 because the finalized run had this
   runner's delete and not its create, so it billed nothing for it, and the
   correction debits the minutes it lived. The document has to match.

6. Read what the Gardener project `alpha` gets:

   ```sh
   jq '{project_id, related_costs: [.related_costs[] | {relation_type, project_id, items: (.line_items | length), total}], total}' ~/tally-tutorial/2026-07-notes/credit-note-garden-sim%2Falpha.json
   ```

   ```json
   {
     "project_id": "alpha",
     "related_costs": [
       {
         "relation_type": "infrastructure_tenant",
         "project_id": "005be5adeef3d87e280d03d9d57c38b4",
         "items": 27,
         "total": -513.91
       }
     ],
     "total": -513.91
   }
   ```

   `alpha`'s note carries its tenant's 27 deltas under `related_costs` with the
   total -513.91, and the note's total is that. The document has to match. The
   correction attributes the way the run did: the tenant's deltas stand on the
   Gardener project's note and nowhere else.

7. Read what the correction moves for the partner:

   ```sh
   go run ./cmd/tally-engine kickbacks --period 2026-07 --run "$CORRECTION_ID"
   ```

   ```json
   {
     "run_id": "9a8770d7-89d9-45dc-a127-7edfe77b9018",
     "kind": "correction",
     "corrects_run_id": "77fb51f8-82c9-4d52-98a2-7fd3e0b43c53",
     "period_from": "2026-07-01T00:00:00Z",
     "period_to": "2026-08-01T00:00:00Z",
     "beneficiaries": [
       {
         "beneficiary": "cloudhouse",
         "currency": "EUR",
         "kickback_total": -58.05,
         "projects": 1,
         "breakdown": [
           {
             "cloud": "os-sim",
             "project_id": "10e287d5788957a2a331cabd1b5dccdf",
             "relation_id": "2ccfc925-6f65-41e5-a180-4d12d25e283a",
             "scope": "all",
             "rate": 0.100000,
             "base": -580.49,
             "amount": -58.05
           }
         ]
       }
     ]
   }
   ```

   `kind` `correction`, `corrects_run_id` naming the finalized run, one
   beneficiary `cloudhouse` with `kickback_total` -58.05, `projects` 1 and one
   `breakdown` entry with `base` -580.49 and `amount` -58.05 have to match. The
   ids are your own. A correction named with `--run` reports the differences to
   the run it corrects, so this is what `cloudhouse` is owed less than the
   settlement of Pay a reseller a kickback said.

## Close the correction

1. Finalize the correction run:

   ```sh
   go run ./cmd/tally-engine finalize --period 2026-07 --run "$CORRECTION_ID"
   ```

   ```text
   correction run 9a8770d7-89d9-45dc-a127-7edfe77b9018 finalized for 2026-07
   ```

   The line has to match with your own correction id in it. A correction closes
   itself alone.

2. List the periods the engine holds:

   ```sh
   go run ./cmd/tally-engine periods list
   ```

   ```text
   2026-07 finalized finalized_run=77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 finalized_at=2026-09-07T22:04:35Z
   2026-08 grace
   ```

   `finalized_run` still names `RUN_ID`, the regular run that closed the month,
   and not the correction. `finalized_at` and the `2026-08` line are your own.

3. Ask the month what arrived late once more:

   ```sh
   go run ./cmd/tally-engine detect-late --period 2026-07
   ```

   ```text
   run 9a8770d7-89d9-45dc-a127-7edfe77b9018 read 2026-07 at 2026-09-07T22:05:50Z
   no events arrived later
   ```

   The report now holds the events against the correction, whose id and
   snapshot are your own, and answers `no events arrived later`. The next
   correction would diff against this one.

## What you learned

- A closed month is never edited, because its numbers may already stand in an
  ERP:
  [why finalization is a human gate](/explanation/billing-period-lifecycle-and-corrections#why-finalization-is-a-human-gate).
- A correction diffs a full re-meter against the finalized run and stores only
  the non-zero deltas, one credit note per project:
  [corrections](/explanation/billing-period-lifecycle-and-corrections#corrections).
- The re-meter is full rather than a patch because a full one is deterministic
  and reproducible:
  [corrections](/explanation/billing-period-lifecycle-and-corrections#corrections).
- The simulator held one in 20 of the billable transitions back so that a late
  arrival could be produced on demand:
  [the fault switches](/explanation/the-simulated-openstack-world#the-fault-switches).
- The members of a credit note are in
  [credit notes](/reference/formats/exports#credit-notes), and the release
  route in
  [tally-openstack-simulator](/reference/command-line/tally-openstack-simulator).

## Where to go next

[Tear down your local Tally](/tutorials/tear-down-your-local-tally) is the way
back to a clean machine. It removes everything below.

A contributor who wants to hold the corrected export against the simulator's
oracle finds that in
[compare an export against the oracle](/how-to/simulator/compare-an-export).

This is the state the billing track leaves behind:

- everything the core track left, except that the simulator container has
  exited after publishing its held share, so `GET /clock` on
  `http://127.0.0.1:8091/clock` answers nothing;
- the registry holding eight projects (the six `os-sim` tenants,
  `garden-sim/alpha` and `garden-sim/beta`), the meta-project `meta/acme`, the
  partner `partner/cloudhouse` and six relations;
- the engine database holding the period `2026-07` finalized on the attributed
  run and one finalized correction of it;
- the seven export directories `~/tally-tutorial/2026-07-group`,
  `2026-07-reseller`, `2026-07-attributed`, `2026-07-final`,
  `2026-07-final-again`, `2026-07-final-csv` and `2026-07-notes` beside the
  core track's `2026-07`;
- in the shell, beside the seven variables of the core track: `ACME_1_ID`,
  `ACME_2_ID`, `ACME_3_ID`, `ACME_ID`, `CI_ID`, `PARTNER_ID`,
  `ALPHA_TENANT_ID`, `BETA_TENANT_ID`, `ALPHA_ID`, `BETA_ID` and
  `CORRECTION_ID`, with `RUN_ID` on the attributed run.

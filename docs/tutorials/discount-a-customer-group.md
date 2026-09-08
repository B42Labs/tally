---
title: Discount a customer group
description: Register three projects of the simulated cloud, group them under a customer with a discount on each membership, run July 2026 again, and read a discounted statement and the group's rollup.
quadrant: tutorial
audience: all
---
<!-- Shown output captured on 2026-09-07 from commit 01d2aff with kind v0.32.0, kubectl v1.36.1, Docker Desktop 4.86.0, Go 1.27.1 on macOS 15.7.4. -->

# Discount a customer group

In this lesson you register three projects of the simulated cloud in the
project registry, group them under a customer, put a 10% discount on each
membership, run July 2026 again, and read one discounted statement and the
group's rollup.

At the end you hold the project registry with its first four rows, a new run of
July 2026 with its id in `RUN_ID`, and the JSON export of that run under
`~/tally-tutorial/2026-07-group`.

This lesson takes about 5 minutes.

## Before you start

- The state [Watch the month in Grafana](/tutorials/watch-the-month-in-grafana)
  leaves, which is the state Meter and rate your first month leaves:

  - the kind cluster `tally` with the dev overlay;
  - the compose stack with the simulator holding its 84 notifications, so
    `GET /clock` on `http://127.0.0.1:8091/clock` answers `holding` true;
  - the reporting database holding the month of July 2026 minus the held share,
    with an empty project registry;
  - the engine database holding the pricing model `2026-03` and one completed,
    not finalized run of `2026-07`;
  - the JSON export of that run under `~/tally-tutorial/2026-07`;
  - `tally-ca.crt` at the repository root;
  - the VictoriaMetrics port-forward on `127.0.0.1:8428`;
  - the seven variables `TALLY_REPORTING_DB_URL`, `TALLY_API_TOKEN` (an admin
    token), `TALLY_ENGINE_DB_URL`, `TALLY_ENGINE_REPORTING_DB_URL`,
    `TALLY_ENGINE_COUNTER_SOURCES`, `TALLY_ENGINE_VM_URL` and `RUN_ID` in a
    shell at the repository root.

- `jq` on the path, which the `make check-tools` of lesson 1 called.

If you closed that shell, restore it with this block:

```sh
export TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
TALLY_API_TOKEN="$(go run ./cmd/tally-reporting-admin create-api-token --role admin --description 'tutorial')"
export TALLY_API_TOKEN
export TALLY_ENGINE_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_engine?sslmode=disable'
export TALLY_ENGINE_REPORTING_DB_URL='postgres://tally_engine:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
export TALLY_ENGINE_COUNTER_SOURCES=deploy/kubernetes/overlays/dev/counter-sources.yaml
export TALLY_ENGINE_VM_URL=http://127.0.0.1:8428
kubectl --context kind-tally -n tally port-forward svc/victoriametrics 8428:8428 &
make -s ca > tally-ca.crt
```

A token is printed once, so a closed shell means a new token. The one you minted
before stays valid until it is revoked, which takes the id on the
`created api_tokens <id>` line the command printed beside it, so keep that line
where you mean to revoke, the way
[issue and revoke credentials](/how-to/openstack/issue-and-revoke-credentials)
says. Every token this track mints goes with the cluster
[Tear down your local Tally](/tutorials/tear-down-your-local-tally) removes.
The port-forward line and the CA line are for a shell that lost them. `RUN_ID`
is not needed before this lesson sets it anew.

A connection error from any `go run` command means the cluster is not up, and
`kind get clusters` then prints nothing. `curl: (7)` on the API hostname means
the same. Both are cured by
[Set up your local Tally](/tutorials/set-up-your-local-tally).

## Register the three classic projects

1. Register the first of the three tenants:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d '{"platform": "openstack", "cloud": "os-sim", "external_id": "018504a6cc10019a40e3f9eef4dae529", "name": "Acme team 1"}' \
     https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects | jq .
   ```

   ```json
   {
     "cloud": "os-sim",
     "created_at": "2026-09-07T21:26:32.361891Z",
     "external_id": "018504a6cc10019a40e3f9eef4dae529",
     "id": "8e4e84bd-e6fb-4c38-a2eb-cde386f94202",
     "metadata": {},
     "name": "Acme team 1",
     "platform": "openstack"
   }
   ```

   `-d` makes the call a `POST /api/v1/projects`. The registry is keyed by
   cloud and external id, and the answer is the row as it is now registered.
   `cloud` `os-sim`, `external_id`, `name` `Acme team 1`, `platform`
   `openstack` and `metadata` `{}` have to match. `id` and `created_at` are
   your own.

   The three classic projects of the month are named after their place in the
   customer group, because the simulated cloud carries no tenant names on the
   bus.

   A 409 with `type` `urn:tally:error:conflict` and the detail
   `a project with this cloud and external id is already registered` means this
   step ran before, and the read-back below reads the id it needs anyway. A 401
   with `type` `urn:tally:error:unauthorized` means `TALLY_API_TOKEN` is empty
   in this shell, so mint one with the restore block above. A 403 with `type`
   `urn:tally:error:forbidden` means the token is not an admin token.

2. Register the other two:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d '{"platform": "openstack", "cloud": "os-sim", "external_id": "34e991db9fc6466f8ca69b43f70fce65", "name": "Acme team 2"}' \
     https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects | jq -c '{id, external_id, name}'
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d '{"platform": "openstack", "cloud": "os-sim", "external_id": "d5a8024946ddf673277b9e2490643a2c", "name": "Acme team 3"}' \
     https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects | jq -c '{id, external_id, name}'
   ```

   ```json
   {"id":"1bb9b4d0-b256-4e78-bc0c-385787c23495","external_id":"34e991db9fc6466f8ca69b43f70fce65","name":"Acme team 2"}
   {"id":"7e35789b-36b1-4f90-b5fb-f924d79ebfa6","external_id":"d5a8024946ddf673277b9e2490643a2c","name":"Acme team 3"}
   ```

   `external_id` and `name` have to match on both lines, and the ids are your
   own. The same three refusals apply.

3. Read the three ids back into the shell:

   ```sh
   ACME_1_ID="$(curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=os-sim&external_id=018504a6cc10019a40e3f9eef4dae529' | jq -r '.items[0].id')"
   ACME_2_ID="$(curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=os-sim&external_id=34e991db9fc6466f8ca69b43f70fce65' | jq -r '.items[0].id')"
   ACME_3_ID="$(curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=os-sim&external_id=d5a8024946ddf673277b9e2490643a2c' | jq -r '.items[0].id')"
   export ACME_1_ID ACME_2_ID ACME_3_ID
   echo "$ACME_1_ID $ACME_2_ID $ACME_3_ID"
   ```

   ```text
   8e4e84bd-e6fb-4c38-a2eb-cde386f94202 1bb9b4d0-b256-4e78-bc0c-385787c23495 7e35789b-36b1-4f90-b5fb-f924d79ebfa6
   ```

   The three ids are your own, and they are the ids the calls above printed.
   This read is what a reader who saw the 409 runs, because it reads what is
   registered whether this shell registered it or not. The registry answers
   `{"items": [...], "next_cursor": null}`, and `.items[0].id` is the id of the
   one row the cloud and the external id name.

   An id printing `null` means that registration did not happen, so `items` is
   empty, and the cure is the registration above. A relation sent with `null`
   in its path is answered 400 with `type` `urn:tally:error:validation` and one
   error located at `path.id`, because `null` is not a uuid.

## Create the customer group

1. Register the meta-project the three tenants become members of:

   ```sh
   ACME_ID="$(go run ./cmd/tally-reporting-admin create-meta-project --external-id acme --name 'Acme')"
   export ACME_ID
   echo "$ACME_ID"
   ```

   ```text
   registered meta-project acme
   13a5e981-edc2-4666-b22e-65e7696de02b
   ```

   `registered meta-project acme` is the CLI's notice on stderr. The id went to
   stdout and into the variable, and it is your own.

   A meta-project is a `projects` row with platform and cloud both `meta`. It
   owns no resource and exists to be the target of `member_of` relations:
   [meta-projects and partners](/explanation/commercial-pricing-on-relations#meta-projects-and-partners).

   `Error: meta-project acme: already registered` with exit status 1 means the
   row exists from an earlier run of this step, and
   `GET /api/v1/projects?cloud=meta&external_id=acme` piped into
   `jq -r '.items[0].id'` reads its id the way the read-back above reads the
   tenants'. `TALLY_REPORTING_DB_URL` has to be exported for the CLI to reach
   the registry, and a `dial tcp` error means it is not, or the cluster is
   down.

## Put the discount on the memberships

1. Relate the first tenant to the group:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d @- "https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/$ACME_1_ID/relations" <<EOF | jq .
   {
     "target_id": "$ACME_ID",
     "relation_type": "member_of",
     "valid_from": "2026-07-01T00:00:00Z",
     "metadata": {
       "pricing_adjustments": [
         {"type": "project_discount", "rate": "0.10", "scope": "all", "description": "Acme group discount"}
       ]
     }
   }
   EOF
   ```

   ```json
   {
     "created_at": "2026-09-07T21:26:57.706276Z",
     "id": "8d9aaff5-56f2-43ba-8e04-6a73906136cf",
     "metadata": {
       "pricing_adjustments": [
         {
           "description": "Acme group discount",
           "rate": "0.10",
           "scope": "all",
           "type": "project_discount"
         }
       ]
     },
     "relation_type": "member_of",
     "source_id": "8e4e84bd-e6fb-4c38-a2eb-cde386f94202",
     "target_id": "13a5e981-edc2-4666-b22e-65e7696de02b",
     "valid_from": "2026-07-01T00:00:00Z",
     "valid_to": null
   }
   ```

   The body is sent as a heredoc so that `$ACME_ID` expands inside real JSON.
   `<<EOF | jq .` is one command: the heredoc feeds `curl` and the pipe feeds
   `jq`.

   `relation_type` `member_of`, `valid_from` `2026-07-01T00:00:00Z`,
   `valid_to` null and the one `pricing_adjustments` entry have to match. `id`,
   `source_id`, `target_id` and `created_at` are your own.

   `valid_from` is the first instant of July because the default is the instant
   the relation is written, which is now and not in July, and a relation
   applies to a period only where its validity overlaps it. The rate is a
   string, `"0.10"`, never a number. The adjustments of a relation are fixed
   for its lifetime, and a change is a closed relation and a successor, as
   [model a customer group with a discount](/how-to/engine/model-a-customer-group)
   shows.

   A 409 with `type` `urn:tally:error:conflict` and the detail
   `a relation of this type between these projects is already active` means
   this step ran before and nothing needs doing. A 400 with `type`
   `urn:tally:error:validation` whose error is located at `body.target_id`
   means `ACME_ID` is empty in this shell. A 422 with `type`
   `urn:tally:error:validation`, the detail
   `the pricing adjustments of this relation do not match the adjustments schema`
   and an error at `body.metadata.pricing_adjustments.0.rate` reading
   `got number, want string` means the rate was sent as a number.

2. Relate the other two:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d @- "https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/$ACME_2_ID/relations" <<EOF | jq -c '{id, source_id, relation_type}'
   {
     "target_id": "$ACME_ID",
     "relation_type": "member_of",
     "valid_from": "2026-07-01T00:00:00Z",
     "metadata": {
       "pricing_adjustments": [
         {"type": "project_discount", "rate": "0.10", "scope": "all", "description": "Acme group discount"}
       ]
     }
   }
   EOF
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d @- "https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/$ACME_3_ID/relations" <<EOF | jq -c '{id, source_id, relation_type}'
   {
     "target_id": "$ACME_ID",
     "relation_type": "member_of",
     "valid_from": "2026-07-01T00:00:00Z",
     "metadata": {
       "pricing_adjustments": [
         {"type": "project_discount", "rate": "0.10", "scope": "all", "description": "Acme group discount"}
       ]
     }
   }
   EOF
   ```

   ```json
   {"id":"071e2e14-d251-4bd2-a229-3f15ec97eacf","source_id":"1bb9b4d0-b256-4e78-bc0c-385787c23495","relation_type":"member_of"}
   {"id":"14b925a3-08e4-4e27-ace8-5ebb3837747b","source_id":"7e35789b-36b1-4f90-b5fb-f924d79ebfa6","relation_type":"member_of"}
   ```

   `relation_type` has to match on both lines. The ids are your own, and
   `source_id` is the member each relation leaves.

3. Count the memberships the group holds:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" "https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/$ACME_ID/relations?direction=incoming&at=2026-07-01T00:00:00Z" | jq '.items | length'
   ```

   ```text
   3
   ```

   The count has to be 3, the three memberships valid at the first instant of
   July. `direction=incoming` lists the relations that reach the meta-project,
   and `at` is the instant the answer describes.

## Run the month again

1. Meter and rate July 2026 once more:

   ```sh
   go run ./cmd/tally-engine run --period 2026-07
   ```

   ```text
   run ee0b363e-2449-4085-bca4-218150d1e546 completed for 2026-07 with pricing model 2026-03
   metered 867 candidates into 945 usage records, 3101 rated records and 6 project statements
   applied 3 pricing adjustments
   superseded run 6ed8bcbd-1f79-4b65-b954-5fd346848128
   warnings recorded in runs.stats: 38 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 3 unregistered projects
   ```

   The `metered` line, `applied 3 pricing adjustments` and the warnings line
   have to match. Both run ids are your own, and the superseded one is the run
   Meter and rate your first month made.

   The `metered` counts are the ones that run printed, 867 candidates, 945
   usage records, 3101 rated records and 6 statements, because nothing in the
   month changed. An adjustment is a record of its own kind beside the rated
   records, which is what `applied 3` counts, one line per member statement.
   The warnings line now ends in `3 unregistered projects`: the three
   registered ones left the list.

   A run before finalization supersedes the run before it in the same
   transaction, so the period never has two completed runs:
   [runs before finalization](/explanation/billing-period-lifecycle-and-corrections#runs-before-finalization).

2. Put the id from the first line of your own output in place of this one:

   ```sh
   export RUN_ID=ee0b363e-2449-4085-bca4-218150d1e546
   ```

   Every command below reads `RUN_ID`.

3. Read what stands there instead if the run printed something else:

   - `no pricing model is valid for this period` means the model Meter and rate
     your first month imported is missing. Import it and run the month again.
   - An error opening on `another run of this period is in progress` means
     another `tally-engine run` of this period is live, in this shell or in
     another one. Let that run end; the hourly tick is not it, because the tick
     leaves a month that already carries a completed run alone.
   - A non-zero `counter` count on the warnings line means the port-forward
     died before the run read the store. Start it again and run the month
     again.
   - `reading the counter sources deploy/kubernetes/overlays/dev/counter-sources.yaml: open deploy/kubernetes/overlays/dev/counter-sources.yaml: no such file or directory`
     means the shell is not at the repository root, or
     `TALLY_ENGINE_COUNTER_SOURCES` is not exported.

## Read a discounted statement

1. Export the run with a rollup over `member_of`:

   ```sh
   go run ./cmd/tally-engine export --run "$RUN_ID" --format json --out ~/tally-tutorial/2026-07-group --rollup member_of
   ```

   ```text
   run ee0b363e-2449-4085-bca4-218150d1e546 exported for 2026-07 as json into /Users/berendt/tally-tutorial/2026-07-group
   wrote run.json and 6 statements
   wrote kickbacks.json with 0 kickbacks
   wrote 1 rollup documents over member_of
   ```

   The second, the third and the fourth line have to match, and
   `wrote 1 rollup documents over member_of` is the line `--rollup member_of`
   adds. The run id and the home directory are your own. The export of Meter
   and rate your first month under `~/tally-tutorial/2026-07` stays as the
   list-price comparison, and a directory that already holds files is refused,
   which is why every export of this track gets a directory of its own.

2. Read what the first Acme tenant owes:

   ```sh
   jq '{base_cost, adjustments, net_cost, total}' ~/tally-tutorial/2026-07-group/statement-os-sim%2F018504a6cc10019a40e3f9eef4dae529.json
   ```

   ```json
   {
     "base_cost": 876.63,
     "adjustments": [
       {
         "type": "project_discount",
         "relation_type": "member_of",
         "relation_target": "acme",
         "relation_id": "8d9aaff5-56f2-43ba-8e04-6a73906136cf",
         "scope": "all",
         "description": "Acme group discount",
         "rate": 0.100000,
         "base": 876.63,
         "amount": -87.66
       }
     ],
     "net_cost": 788.97,
     "total": 788.97
   }
   ```

   `base_cost` 876.63 is the total the same statement carried at list price.
   The one `adjustments` line has `type` `project_discount`, `relation_type`
   `member_of`, `relation_target` `acme`, `scope` `all`, `rate` 0.100000,
   `base` 876.63 and `amount` -87.66, the base times the rate rounded once to
   two places. `net_cost` 788.97 is the base plus the signed amount, and
   `total` is the net. All of these have to match. `relation_id` is your own,
   the id of the membership this line traces back to. The amount is rounded the
   way every other amount is:
   [why rounding happens once](/explanation/money-and-rounding#why-rounding-happens-once).

3. Read what a tenant outside the group owes:

   ```sh
   jq '{base_cost, total}' ~/tally-tutorial/2026-07-group/statement-os-sim%2F10e287d5788957a2a331cabd1b5dccdf.json
   ```

   ```json
   {
     "base_cost": null,
     "total": 698.29
   }
   ```

   This is the CI tenant, which no membership reaches, so its statement carries
   none of the four members `base_cost`, `adjustments`, `net_cost` and
   `kickback_total`, and `jq` prints an absent member as null. Its total 698.29
   has to match and is unchanged from the list-price run.

## Read the rollup

1. List what the export wrote:

   ```sh
   ls ~/tally-tutorial/2026-07-group
   ```

   ```text
   kickbacks.json
   rollup-meta%2Facme.json
   run.json
   statement-os-sim%2F005be5adeef3d87e280d03d9d57c38b4.json
   statement-os-sim%2F018504a6cc10019a40e3f9eef4dae529.json
   statement-os-sim%2F10e287d5788957a2a331cabd1b5dccdf.json
   statement-os-sim%2F34e991db9fc6466f8ca69b43f70fce65.json
   statement-os-sim%2Fd5a8024946ddf673277b9e2490643a2c.json
   statement-os-sim%2Fe31f9083a7e5ee15071a3bd53cb2bac7.json
   ```

   The nine names have to match. `rollup-meta%2Facme.json` stands beside
   `run.json`, `kickbacks.json` and the six statements.

2. Read the group's rollup document:

   ```sh
   jq . ~/tally-tutorial/2026-07-group/rollup-meta%2Facme.json
   ```

   ```json
   {
     "billing_period": {
       "from": "2026-07-01T00:00:00Z",
       "to": "2026-08-01T00:00:00Z"
     },
     "project_id": "acme",
     "platform": "meta",
     "relation_type": "member_of",
     "kind": "regular",
     "corrects_run_id": null,
     "members": [
       {
         "file": "statement-os-sim%2F018504a6cc10019a40e3f9eef4dae529.json",
         "cloud": "os-sim",
         "project_id": "018504a6cc10019a40e3f9eef4dae529",
         "total": 788.97,
         "currency": "EUR"
       },
       {
         "file": "statement-os-sim%2F34e991db9fc6466f8ca69b43f70fce65.json",
         "cloud": "os-sim",
         "project_id": "34e991db9fc6466f8ca69b43f70fce65",
         "total": 538.55,
         "currency": "EUR"
       },
       {
         "file": "statement-os-sim%2Fd5a8024946ddf673277b9e2490643a2c.json",
         "cloud": "os-sim",
         "project_id": "d5a8024946ddf673277b9e2490643a2c",
         "total": 814.59,
         "currency": "EUR"
       }
     ],
     "total": 2142.11,
     "currency": "EUR"
   }
   ```

   `project_id` `acme`, `platform` `meta`, `relation_type` `member_of`, `kind`
   `regular`, the three `members` with their files and their totals 788.97,
   538.55 and 814.59, and the `total` 2142.11, their sum, have to match. The
   rollup is read from the registry at export time and sums the statements
   without changing them: [rollup](/reference/formats/exports#rollup).

3. Read the index entry the export wrote for it:

   ```sh
   jq .rollup ~/tally-tutorial/2026-07-group/run.json
   ```

   ```json
   {
     "relation_type": "member_of",
     "documents": [
       {
         "file": "rollup-meta%2Facme.json",
         "cloud": "meta",
         "project_id": "acme",
         "members": 3,
         "total": 2142.11,
         "currency": "EUR"
       }
     ]
   }
   ```

   `members` 3 and the total have to match. The index names every rollup
   document the export wrote.

## What you learned

- Adjustments live on relations, so every discount line traces back to one
  relation and its target:
  [why adjustments live on relations](/explanation/commercial-pricing-on-relations#why-adjustments-live-on-relations).
- A meta-project is a project that owns nothing and exists to be related to:
  [meta-projects and partners](/explanation/commercial-pricing-on-relations#meta-projects-and-partners).
- A relation applies to a period its validity overlaps, which is why
  `valid_from` had to be in July:
  [temporal validity](/explanation/project-registry-relations-and-attribution#temporal-validity).
- A run before finalization supersedes the run before it:
  [runs before finalization](/explanation/billing-period-lifecycle-and-corrections#runs-before-finalization).
- The adjustments array is specified in
  [pricing adjustments](/reference/formats/pricing-adjustments) and the
  statement members `base_cost`, `adjustments`, `net_cost` and `total` in
  [statements](/reference/formats/exports#statements).

## Where to go next

[Pay a reseller a kickback](/tutorials/pay-a-reseller-a-kickback) puts one
tenant under a partner with a discount and a kickback.

It starts from the state this lesson leaves behind:

- everything Watch the month in Grafana left;
- the registry holding the three tenants, the meta-project `acme` and three
  `member_of` relations;
- `RUN_ID` on the discounted run, which superseded the list-price run;
- the export under `~/tally-tutorial/2026-07-group` beside the one under
  `~/tally-tutorial/2026-07`;
- `ACME_ID`, `ACME_1_ID`, `ACME_2_ID` and `ACME_3_ID` in the shell.

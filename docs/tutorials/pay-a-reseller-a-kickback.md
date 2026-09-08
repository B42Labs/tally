---
title: Pay a reseller a kickback
description: Register the CI tenant of the simulated cloud, create a partner, put the tenant under its management with a discount and a kickback, run July 2026 again, and read the tenant's statement and the kickback report.
quadrant: tutorial
audience: all
---
<!-- Shown output captured on 2026-09-07 from commit 01d2aff with kind v0.32.0, kubectl v1.36.1, Docker Desktop 4.86.0, Go 1.27.1 on macOS 15.7.4. -->

# Pay a reseller a kickback

In this lesson you register the CI tenant of the simulated cloud, create a
partner, put the tenant under the partner's management with a 15% discount and
a 10% kickback on one relation, run July 2026 again, and read the tenant's
statement and the kickback report.

At the end the registry holds the CI tenant and the partner `cloudhouse` beside
the rows of Discount a customer group, `RUN_ID` names a new run of July 2026,
the JSON export of that run is under `~/tally-tutorial/2026-07-reseller`, and
`CI_ID` and `PARTNER_ID` are in the shell.

This lesson takes about 5 minutes.

## Before you start

- The state [Discount a customer group](/tutorials/discount-a-customer-group)
  leaves:

  - everything Watch the month in Grafana left;
  - the registry holding the three tenants, the meta-project `acme` and three
    `member_of` relations;
  - `RUN_ID` on the discounted run;
  - the export under `~/tally-tutorial/2026-07-group`;
  - `ACME_ID`, `ACME_1_ID`, `ACME_2_ID` and `ACME_3_ID` in the shell.

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
is not needed before this lesson sets it anew, and this lesson reads none of
the four `ACME_` variables.

A connection error from any `go run` command means the cluster is not up, and
`kind get clusters` then prints nothing. `curl: (7)` on the API hostname means
the same. Both are cured by
[Set up your local Tally](/tutorials/set-up-your-local-tally).

## Register the CI tenant

1. Register the tenant the partner manages:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d '{"platform": "openstack", "cloud": "os-sim", "external_id": "10e287d5788957a2a331cabd1b5dccdf", "name": "ci"}' \
     https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects | jq .
   ```

   ```json
   {
     "cloud": "os-sim",
     "created_at": "2026-09-07T21:27:25.758052Z",
     "external_id": "10e287d5788957a2a331cabd1b5dccdf",
     "id": "d4fed605-70b4-4350-9d9e-1b8b58e1dbae",
     "metadata": {},
     "name": "ci",
     "platform": "openstack"
   }
   ```

   This is [the CI tenant](/explanation/the-simulated-openstack-world#the-ci-tenant),
   which boots and deletes runners on every working day of the month. It
   carries 447 resources, more than any other tenant of the month, and its
   total in the list-price run is 698.29.

   `cloud` `os-sim`, `external_id`, `name` `ci`, `platform` `openstack` and
   `metadata` `{}` have to match. `id` and `created_at` are your own.

   A 409 with `type` `urn:tally:error:conflict` and the detail
   `a project with this cloud and external id is already registered` means this
   step ran before, and the read-back below reads the id it needs anyway. A 401
   with `type` `urn:tally:error:unauthorized` means `TALLY_API_TOKEN` is empty
   in this shell, so mint one with the restore block above. A 403 with `type`
   `urn:tally:error:forbidden` means the token is not an admin token.

2. Read the id back into the shell:

   ```sh
   CI_ID="$(curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=os-sim&external_id=10e287d5788957a2a331cabd1b5dccdf' | jq -r '.items[0].id')"
   export CI_ID
   echo "$CI_ID"
   ```

   ```text
   d4fed605-70b4-4350-9d9e-1b8b58e1dbae
   ```

   The id is your own, and it is the id the registration printed. This read is
   what a reader who saw the 409 runs, because it reads what is registered
   whether this shell registered it or not. An id printing `null` means the
   registration did not happen, and the cure is the call above.

## Create the partner

1. Register the partner that manages the tenant:

   ```sh
   PARTNER_ID="$(go run ./cmd/tally-reporting-admin create-partner --external-id cloudhouse --name 'Cloudhouse')"
   export PARTNER_ID
   echo "$PARTNER_ID"
   ```

   ```text
   registered partner cloudhouse
   764c13ba-f5e1-4d3a-8253-2d0813016aee
   ```

   `registered partner cloudhouse` is the CLI's notice on stderr. The id went
   to stdout and into the variable, and it is your own.

   A partner is a `projects` row with platform and cloud both `partner`. It
   owns no resource and is what a `managed_by` relation points at:
   [meta-projects and partners](/explanation/commercial-pricing-on-relations#meta-projects-and-partners).

   Check that the `echo` printed one uuid before you go on. The target of the
   relation in the next step cannot be corrected inside July once it is sent,
   for the reason the run step gives.

   `Error: partner cloudhouse: already registered` with exit status 1 means the
   row exists from an earlier run of this step, and
   `GET /api/v1/projects?cloud=partner&external_id=cloudhouse` piped into
   `jq -r '.items[0].id'` reads its id the way the read-back above reads the
   tenant's.

## Put the discount and the kickback on the management

1. Relate the tenant to the partner:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d @- "https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/$CI_ID/relations" <<EOF | jq .
   {
     "target_id": "$PARTNER_ID",
     "relation_type": "managed_by",
     "valid_from": "2026-07-01T00:00:00Z",
     "metadata": {
       "pricing_adjustments": [
         {"type": "discount", "rate": "0.15", "scope": "all", "description": "Cloudhouse end-customer discount"},
         {"type": "kickback", "rate": "0.10", "scope": "all", "description": "Cloudhouse commission"}
       ]
     }
   }
   EOF
   ```

   ```json
   {
     "created_at": "2026-09-07T21:27:26.302705Z",
     "id": "2ccfc925-6f65-41e5-a180-4d12d25e283a",
     "metadata": {
       "pricing_adjustments": [
         {
           "description": "Cloudhouse end-customer discount",
           "rate": "0.15",
           "scope": "all",
           "type": "discount"
         },
         {
           "description": "Cloudhouse commission",
           "rate": "0.10",
           "scope": "all",
           "type": "kickback"
         }
       ]
     },
     "relation_type": "managed_by",
     "source_id": "d4fed605-70b4-4350-9d9e-1b8b58e1dbae",
     "target_id": "764c13ba-f5e1-4d3a-8253-2d0813016aee",
     "valid_from": "2026-07-01T00:00:00Z",
     "valid_to": null
   }
   ```

   `relation_type` `managed_by`, `valid_from` `2026-07-01T00:00:00Z`,
   `valid_to` null and the two `pricing_adjustments` entries, a `discount` of
   rate `0.15` and a `kickback` of rate `0.10`, both on scope `all`, have to
   match. `id`, `source_id`, `target_id` and `created_at` are your own.

   A `managed_by` relation places the tenant under the partner and attributes
   no cost. The rates are strings, `"0.15"` and `"0.10"`, never numbers, and
   `valid_from` is the first instant of July for the reason Discount a customer
   group gives.

   The engine applies what it collects in the fixed order surcharge, discount,
   project discount, kickback, whatever order the array has. A kickback is what
   the partner is owed rather than what the customer pays.

   A 409 with `type` `urn:tally:error:conflict` and the detail
   `a relation of this type between these projects is already active` means
   this step ran before. A 400 with `type` `urn:tally:error:validation` whose
   error is located at `body.target_id` means `PARTNER_ID` is empty in this
   shell. A 422 with `type` `urn:tally:error:validation` and an error at
   `body.metadata.pricing_adjustments.0.rate` reading `got number, want string`
   means a rate was sent as a number. A `target_id` that is the meta-project's
   id, `ACME_ID`, is stored by the registry like any other, and the run then
   drops the kickback with one `adjustment` warning, which the next step names.

## Run the month

1. Meter and rate July 2026 once more:

   ```sh
   go run ./cmd/tally-engine run --period 2026-07
   ```

   ```text
   run 72584a66-9bb2-4d5b-9cf2-fa43fcb6e886 completed for 2026-07 with pricing model 2026-03
   metered 867 candidates into 945 usage records, 3101 rated records and 6 project statements
   applied 5 pricing adjustments
   superseded run ee0b363e-2449-4085-bca4-218150d1e546
   warnings recorded in runs.stats: 38 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 2 unregistered projects
   ```

   The `metered` line, `applied 5 pricing adjustments` and the warnings line
   have to match. Both run ids are your own, and the superseded one is the run
   Discount a customer group made.

   `applied 5` counts the three membership lines plus the two lines of this
   relation, one per adjustment on the CI tenant's statement. The warnings line
   ends in `0 adjustment`, so no kickback was dropped, and in
   `2 unregistered projects`, the two Gardener tenants the next lesson
   registers.

2. Put the id from the first line of your own output in place of this one:

   ```sh
   export RUN_ID=72584a66-9bb2-4d5b-9cf2-fa43fcb6e886
   ```

   Every command below reads `RUN_ID`.

3. Read what stands there instead if the run printed something else:

   - `1 adjustment` on the warnings line together with a
     `warning: adjustment_kickback_target_not_partner` line means the
     relation's target is not the partner row, so the kickback was dropped and
     the discount stayed. There is no cure inside July: a relation is closed
     rather than deleted, `valid_to` has to be after `valid_from`, and a
     relation whose validity overlaps a period at any instant applies to it
     ([temporal validity](/explanation/project-registry-relations-and-attribution#temporal-validity)),
     so the run bills with that relation as it stands. A reader who wants the
     numbers of this page starts both tracks again from
     [Tear down your local Tally](/tutorials/tear-down-your-local-tally). This
     is why the step before had you check `PARTNER_ID`.
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

## Read the reseller's statement

1. Export the run:

   ```sh
   go run ./cmd/tally-engine export --run "$RUN_ID" --format json --out ~/tally-tutorial/2026-07-reseller
   ```

   ```text
   run 72584a66-9bb2-4d5b-9cf2-fa43fcb6e886 exported for 2026-07 as json into /Users/berendt/tally-tutorial/2026-07-reseller
   wrote run.json and 6 statements
   wrote kickbacks.json with 1 kickbacks
   ```

   The second and the third line have to match, and
   `wrote kickbacks.json with 1 kickbacks` is the settlement this run has that
   the run before had none of. The run id and the home directory are your own.
   A directory that already holds files is refused, so this export gets a
   directory of its own.

2. Read what the CI tenant owes:

   ```sh
   jq '{base_cost, adjustments, net_cost, kickback_total, total}' ~/tally-tutorial/2026-07-reseller/statement-os-sim%2F10e287d5788957a2a331cabd1b5dccdf.json
   ```

   ```json
   {
     "base_cost": 698.29,
     "adjustments": [
       {
         "type": "discount",
         "relation_type": "managed_by",
         "relation_target": "cloudhouse",
         "relation_id": "2ccfc925-6f65-41e5-a180-4d12d25e283a",
         "scope": "all",
         "description": "Cloudhouse end-customer discount",
         "rate": 0.150000,
         "base": 698.29,
         "amount": -104.74
       },
       {
         "type": "kickback",
         "relation_type": "managed_by",
         "relation_target": "cloudhouse",
         "relation_id": "2ccfc925-6f65-41e5-a180-4d12d25e283a",
         "scope": "all",
         "description": "Cloudhouse commission",
         "rate": 0.100000,
         "base": 593.55,
         "amount": 59.36
       }
     ],
     "net_cost": 593.55,
     "kickback_total": 59.36,
     "total": 593.55
   }
   ```

   `base_cost` 698.29 is the tenant's total at list price. The `discount` line
   is computed on that base: 698.29 times 0.15, rounded once to two places, is
   `amount` -104.74. `net_cost` 593.55 is the base plus that amount, and
   `total` is the net. The `kickback` line is computed on the running net:
   `base` 593.55 times 0.10, rounded once, is `amount` 59.36, and it leaves the
   net alone. `kickback_total` 59.36 stands beside `net_cost`, not inside it.

   All of these have to match. The two `relation_id` values are your own, and
   both name the one relation. The two lines stand in the order the engine
   applied them.

3. List what the export wrote:

   ```sh
   ls ~/tally-tutorial/2026-07-reseller
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

   The eight names have to match: six statements beside `run.json` and
   `kickbacks.json`. There is no `statement-partner%2Fcloudhouse.json`, because
   a partner is billed nothing and what it is owed is in the settlement.

## Read the kickback report

1. Report what the run owes its partners:

   ```sh
   go run ./cmd/tally-engine kickbacks --period 2026-07
   ```

   ```json
   {
     "run_id": "72584a66-9bb2-4d5b-9cf2-fa43fcb6e886",
     "kind": "regular",
     "corrects_run_id": null,
     "period_from": "2026-07-01T00:00:00Z",
     "period_to": "2026-08-01T00:00:00Z",
     "beneficiaries": [
       {
         "beneficiary": "cloudhouse",
         "currency": "EUR",
         "kickback_total": 59.36,
         "projects": 1,
         "breakdown": [
           {
             "cloud": "os-sim",
             "project_id": "10e287d5788957a2a331cabd1b5dccdf",
             "relation_id": "2ccfc925-6f65-41e5-a180-4d12d25e283a",
             "scope": "all",
             "rate": 0.100000,
             "base": 593.55,
             "amount": 59.36
           }
         ]
       }
     ]
   }
   ```

   `kind` `regular`, the one beneficiary `cloudhouse` in `EUR`, its
   `kickback_total` 59.36 equal to the statement's, `projects` 1 and the one
   `breakdown` entry with its cloud `os-sim`, its project
   `10e287d5788957a2a331cabd1b5dccdf`, its relation id, its scope `all`, its
   rate 0.100000, its base 593.55 and its amount 59.36 have to match. `run_id`
   and `relation_id` are your own.

   A month named alone reports the regular run that bills it. The document
   alone reaches stdout, so it pipes into a file as it is.

2. Report the same settlement as CSV:

   ```sh
   go run ./cmd/tally-engine kickbacks --period 2026-07 --format csv
   ```

   ```text
   run_id,kind,corrects_run_id,period_from,period_to,beneficiary,cloud,project_id,relation_id,scope,rate,base,amount,currency
   72584a66-9bb2-4d5b-9cf2-fa43fcb6e886,regular,,2026-07-01T00:00:00Z,2026-08-01T00:00:00Z,cloudhouse,os-sim,10e287d5788957a2a331cabd1b5dccdf,2ccfc925-6f65-41e5-a180-4d12d25e283a,all,0.100000,593.55,59.36,EUR
   ```

   The header has to match. There is one row per kickback record, and the ids
   in the row are your own.

3. Read the settlement the export left beside the statements:

   ```sh
   jq .beneficiaries ~/tally-tutorial/2026-07-reseller/kickbacks.json
   ```

   ```json
   [
     {
       "beneficiary": "cloudhouse",
       "currency": "EUR",
       "kickback_total": 59.36,
       "projects": 1,
       "breakdown": [
         {
           "cloud": "os-sim",
           "project_id": "10e287d5788957a2a331cabd1b5dccdf",
           "relation_id": "2ccfc925-6f65-41e5-a180-4d12d25e283a",
           "scope": "all",
           "rate": 0.100000,
           "base": 593.55,
           "amount": 59.36
         }
       ]
     }
   ]
   ```

   The export wrote the same settlement beside the statements, so what you hand
   the partner is in the export directory too.

## What you learned

- A partner is an ordinary registry row, and a kickback is a line of its own
  computed on the running net that leaves the customer's net alone:
  [how adjustments apply](/explanation/commercial-pricing-on-relations#how-adjustments-apply).
- What a partner is owed is a sum across statements, which is why the
  settlement is a report of its own:
  [kickbacks and the rollup](/explanation/commercial-pricing-on-relations#kickbacks-and-the-rollup).
- The rate is stored on the relation rather than derived from the period's
  volume, so a correction rates the same way:
  [why volume tiers are not computed](/explanation/commercial-pricing-on-relations#why-volume-tiers-are-not-computed).
- The settlement's members are in
  [kickback settlement](/reference/formats/exports#kickback-settlement) and the
  flags of `kickbacks` in
  [tally-engine](/reference/command-line/tally-engine).

## Where to go next

[Attribute a tenant to its Gardener project](/tutorials/attribute-a-tenant-to-its-gardener-project)
registers the two Gardener projects and moves their tenants' costs onto them.

It starts from the state this lesson leaves behind:

- everything Discount a customer group left;
- the registry holding four tenants, the meta-project `acme`, the partner
  `cloudhouse` and four relations;
- `RUN_ID` on this run, which superseded the discounted one;
- the export under `~/tally-tutorial/2026-07-reseller`;
- `CI_ID` and `PARTNER_ID` in the shell.

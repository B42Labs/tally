---
title: Attribute a tenant to its Gardener project
description: Register the two Gardener projects and the tenants their shoots run on, relate each project to its tenant, see the registry refuse a cycle, run July 2026 again, and read the tenant's costs on the Gardener project's statement.
quadrant: tutorial
audience: all
---
<!-- Shown output captured on 2026-09-07 from commit 01d2aff with kind v0.32.0, kubectl v1.36.1, Docker Desktop 4.86.0, Go 1.27.1 on macOS 15.7.4. -->

# Attribute a tenant to its Gardener project

In this lesson you register the two Gardener projects of the simulated month and
the two OpenStack tenants their shoots run on, relate each project to its tenant
with an `infrastructure_tenant` relation, see the registry refuse the reverse
relation as a cycle, run July 2026 again, and read the tenant's costs on the
Gardener project's statement and nowhere else.

At the end the registry holds every tenant of the month and both Gardener
projects, `RUN_ID` names the run that attributes the two tenants, the JSON
export of that run is under `~/tally-tutorial/2026-07-attributed`, and
`ALPHA_TENANT_ID`, `BETA_TENANT_ID`, `ALPHA_ID` and `BETA_ID` are in the shell.

This lesson takes about 5 minutes.

## Before you start

- The state [Pay a reseller a kickback](/tutorials/pay-a-reseller-a-kickback)
  leaves:

  - everything Discount a customer group left;
  - the registry holding four tenants, the meta-project `acme`, the partner
    `cloudhouse` and four relations;
  - `RUN_ID` on the reseller run;
  - the export under `~/tally-tutorial/2026-07-reseller`;
  - `CI_ID` and `PARTNER_ID` in the shell.

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
the four `ACME_` variables, `CI_ID` or `PARTNER_ID`.

A connection error from any `go run` command means the cluster is not up, and
`kind get clusters` then prints nothing. `curl: (7)` on the API hostname means
the same. Both are cured by
[Set up your local Tally](/tutorials/set-up-your-local-tally).

## Register the Gardener projects and their tenants

1. Register the tenant the shoots of `alpha` run on:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d '{"platform": "openstack", "cloud": "os-sim", "external_id": "005be5adeef3d87e280d03d9d57c38b4", "name": "Infrastructure tenant of alpha"}' \
     https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects | jq .
   ```

   ```json
   {
     "cloud": "os-sim",
     "created_at": "2026-09-07T21:28:08.425664Z",
     "external_id": "005be5adeef3d87e280d03d9d57c38b4",
     "id": "e9e6a14a-14c0-4fa6-95b2-6b284c873f0d",
     "metadata": {},
     "name": "Infrastructure tenant of alpha",
     "platform": "openstack"
   }
   ```

   This is the tenant with 309 resources and the largest statement of the
   list-price run, 3661.32. It is `alpha`'s tenant because `alpha` runs two
   shoots all month while `beta` runs one shoot for about two weeks:
   [the Gardener projects](/explanation/the-simulated-openstack-world#the-gardener-projects).

   `cloud` `os-sim`, `external_id`, `name` `Infrastructure tenant of alpha`,
   `platform` `openstack` and `metadata` `{}` have to match. `id` and
   `created_at` are your own.

   A 409 with `type` `urn:tally:error:conflict` and the detail
   `a project with this cloud and external id is already registered` means this
   step ran before, and the read-back below reads the id it needs anyway. A 401
   with `type` `urn:tally:error:unauthorized` means `TALLY_API_TOKEN` is empty
   in this shell, so mint one with the restore block above. A 403 with `type`
   `urn:tally:error:forbidden` means the token is not an admin token.

2. Register the other tenant and the two Gardener projects:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d '{"platform": "openstack", "cloud": "os-sim", "external_id": "e31f9083a7e5ee15071a3bd53cb2bac7", "name": "Infrastructure tenant of beta"}' \
     https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects | jq -c '{id, cloud, external_id, name}'
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d '{"platform": "gardener", "cloud": "garden-sim", "external_id": "alpha", "name": "Gardener project alpha"}' \
     https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects | jq -c '{id, cloud, external_id, name}'
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d '{"platform": "gardener", "cloud": "garden-sim", "external_id": "beta", "name": "Gardener project beta"}' \
     https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects | jq -c '{id, cloud, external_id, name}'
   ```

   ```json
   {"id":"75b72e57-da37-48db-adb8-fe00ac3f812a","cloud":"os-sim","external_id":"e31f9083a7e5ee15071a3bd53cb2bac7","name":"Infrastructure tenant of beta"}
   {"id":"0fec01ef-dc9b-4a2e-9545-10b8ed64f77f","cloud":"garden-sim","external_id":"alpha","name":"Gardener project alpha"}
   {"id":"c86423ed-e394-44a4-8e07-136fdd50e7dc","cloud":"garden-sim","external_id":"beta","name":"Gardener project beta"}
   ```

   `cloud`, `external_id` and `name` have to match on every line, and the ids
   are your own.

   A cloud is one installation of one platform, which is why the two Gardener
   rows carry the cloud `garden-sim` and the platform `gardener` rather than the
   tenants' `os-sim`. A Gardener project keyed under the tenants' cloud would be
   a row of the OpenStack installation.

   These are the rows the simulator's registration switch would have posted at
   start, without its metadata:
   [register simulated projects](/how-to/simulator/register-simulated-projects).

   The same three refusals apply.

3. Read the four ids back into the shell:

   ```sh
   ALPHA_TENANT_ID="$(curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=os-sim&external_id=005be5adeef3d87e280d03d9d57c38b4' | jq -r '.items[0].id')"
   BETA_TENANT_ID="$(curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=os-sim&external_id=e31f9083a7e5ee15071a3bd53cb2bac7' | jq -r '.items[0].id')"
   ALPHA_ID="$(curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=garden-sim&external_id=alpha' | jq -r '.items[0].id')"
   BETA_ID="$(curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=garden-sim&external_id=beta' | jq -r '.items[0].id')"
   export ALPHA_TENANT_ID BETA_TENANT_ID ALPHA_ID BETA_ID
   echo "$ALPHA_TENANT_ID $BETA_TENANT_ID $ALPHA_ID $BETA_ID"
   ```

   ```text
   e9e6a14a-14c0-4fa6-95b2-6b284c873f0d 75b72e57-da37-48db-adb8-fe00ac3f812a 0fec01ef-dc9b-4a2e-9545-10b8ed64f77f c86423ed-e394-44a4-8e07-136fdd50e7dc
   ```

   The four ids are your own, and they are the ids the calls above printed, in
   that order. This read is what a reader who saw a 409 runs, because it reads
   what is registered whether this shell registered it or not. An id printing
   `null` means that registration did not happen, and the cure is the
   registration above.

## Relate each project to its tenant

1. Name the tenant of the Gardener project `alpha`:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d @- "https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/$ALPHA_ID/relations" <<EOF | jq .
   {
     "target_id": "$ALPHA_TENANT_ID",
     "relation_type": "infrastructure_tenant",
     "valid_from": "2026-07-01T00:00:00Z"
   }
   EOF
   ```

   ```json
   {
     "created_at": "2026-09-07T21:28:08.615857Z",
     "id": "700508b3-e5e4-42ce-be04-970febb813f9",
     "metadata": {},
     "relation_type": "infrastructure_tenant",
     "source_id": "0fec01ef-dc9b-4a2e-9545-10b8ed64f77f",
     "target_id": "e9e6a14a-14c0-4fa6-95b2-6b284c873f0d",
     "valid_from": "2026-07-01T00:00:00Z",
     "valid_to": null
   }
   ```

   The relation leaves the project that pays and reaches the project whose costs
   move, so it starts at the Gardener project and names its tenant as
   `target_id`. `infrastructure_tenant` is the one relation type the engine
   attributes cost over by default (`TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES`),
   and it carries no adjustments.

   `relation_type` `infrastructure_tenant`, `valid_from` `2026-07-01T00:00:00Z`,
   `valid_to` null and `metadata` `{}` have to match. `id`, `source_id`,
   `target_id` and `created_at` are your own. `valid_from` is the first instant
   of July for the reason Discount a customer group gives.

   A 409 with `type` `urn:tally:error:conflict` and the detail
   `a relation of this type between these projects is already active` means this
   step ran before. A 400 with `type` `urn:tally:error:validation` whose error
   is located at `body.target_id` means `ALPHA_TENANT_ID` is empty in this
   shell.

2. Name the tenant of `beta`:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d @- "https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/$BETA_ID/relations" <<EOF | jq -c '{id, relation_type}'
   {
     "target_id": "$BETA_TENANT_ID",
     "relation_type": "infrastructure_tenant",
     "valid_from": "2026-07-01T00:00:00Z"
   }
   EOF
   ```

   ```json
   {"id":"cc791285-89f8-4c22-ba4e-7ee34b701853","relation_type":"infrastructure_tenant"}
   ```

   `relation_type` has to match, and the id is your own.

3. Walk what `alpha` attributes:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" "https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/$ALPHA_ID/related?relation_type=infrastructure_tenant" | jq '.items[] | {depth, relation_type, project: .project.external_id}'
   ```

   ```json
   {
     "depth": 1,
     "relation_type": "infrastructure_tenant",
     "project": "005be5adeef3d87e280d03d9d57c38b4"
   }
   ```

   The walk goes one relation out of `alpha` and reaches its tenant at `depth` 1
   over `infrastructure_tenant`. `project` is the tenant's external id and has
   to match, as do the depth and the type.

## See the registry refuse a cycle

1. Send the same relation the other way round:

   ```sh
   curl -s --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" -H 'Content-Type: application/json' \
     -d @- "https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects/$ALPHA_TENANT_ID/relations" <<EOF | jq .
   {
     "target_id": "$ALPHA_ID",
     "relation_type": "infrastructure_tenant",
     "valid_from": "2026-07-01T00:00:00Z"
   }
   EOF
   ```

   ```json
   {
     "type": "urn:tally:error:relation_cycle",
     "title": "Relation cycle",
     "status": 422,
     "detail": "this relation would close a cycle over the relation types that attribute cost"
   }
   ```

   The reverse relation would let the tenant attribute the project that already
   attributes it, and attribution is a forest. The registry walks the
   attributing relations out of the target first, finds the source, and answers
   422 without writing anything. `type` `urn:tally:error:relation_cycle`,
   `status` 422 and the detail
   `this relation would close a cycle over the relation types that attribute cost`
   have to match.

## Run the month

1. Meter and rate July 2026 once more:

   ```sh
   go run ./cmd/tally-engine run --period 2026-07
   ```

   ```text
   run 77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 completed for 2026-07 with pricing model 2026-03
   metered 867 candidates into 945 usage records, 3101 rated records and 6 project statements
   applied 5 pricing adjustments
   superseded run 72584a66-9bb2-4d5b-9cf2-fa43fcb6e886
   warnings recorded in runs.stats: 38 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 0 unregistered projects
   ```

   The `metered` line, `applied 5 pricing adjustments` and the warnings line
   have to match. Both run ids are your own, and the superseded one is the run
   Pay a reseller a kickback made.

   The statement count stays 6: the four tenants billed on their own and the two
   Gardener projects, while the two Gardener tenants get no statement. The
   warnings line reads `0 attribution`, so no tenant was claimed twice or sat in
   a cycle, and it ends in `0 unregistered projects`: the registry now holds
   every tenant of the month.

2. Put the id from the first line of your own output in place of this one:

   ```sh
   export RUN_ID=77fb51f8-82c9-4d52-98a2-7fd3e0b43c53
   ```

   Every command below reads `RUN_ID`.

3. Read what stands there instead if the run printed something else:

   - A non-zero `attribution` count on the warnings line means a second path
     claimed one of the tenants, and
     `GET /api/v1/projects/$ALPHA_TENANT_ID/relations?direction=incoming`,
     called with the `curl` prefix of the steps above, lists the relations that
     reach it.
   - `no pricing model is valid for this period` means the model Meter and rate
     your first month imported is missing. Import it and run the month again.
   - An error opening on `another run of this period is in progress` means
     another `tally-engine run` of this period is live, in this shell or in
     another one. Let that run end; the hourly tick is not it, because the tick
     leaves a month that already carries a completed run alone.
   - A non-zero `counter` count on the warnings line means the port-forward died
     before the run read the store. Start it again and run the month again.
   - `reading the counter sources deploy/kubernetes/overlays/dev/counter-sources.yaml: open deploy/kubernetes/overlays/dev/counter-sources.yaml: no such file or directory`
     means the shell is not at the repository root, or
     `TALLY_ENGINE_COUNTER_SOURCES` is not exported.

## Read the attributed statement

1. Export the run:

   ```sh
   go run ./cmd/tally-engine export --run "$RUN_ID" --format json --out ~/tally-tutorial/2026-07-attributed
   ```

   ```text
   run 77fb51f8-82c9-4d52-98a2-7fd3e0b43c53 exported for 2026-07 as json into /Users/berendt/tally-tutorial/2026-07-attributed
   wrote run.json and 6 statements
   wrote kickbacks.json with 1 kickbacks
   ```

   The second and the third line have to match. The run id and the home
   directory are your own.

2. List what the export wrote:

   ```sh
   ls ~/tally-tutorial/2026-07-attributed
   ```

   ```text
   kickbacks.json
   run.json
   statement-garden-sim%2Falpha.json
   statement-garden-sim%2Fbeta.json
   statement-os-sim%2F018504a6cc10019a40e3f9eef4dae529.json
   statement-os-sim%2F10e287d5788957a2a331cabd1b5dccdf.json
   statement-os-sim%2F34e991db9fc6466f8ca69b43f70fce65.json
   statement-os-sim%2Fd5a8024946ddf673277b9e2490643a2c.json
   ```

   The eight names have to match. `statement-garden-sim%2Falpha.json` and
   `statement-garden-sim%2Fbeta.json` stand beside four
   `statement-os-sim%2F<id>.json` files, and neither Gardener tenant has a
   statement any more.

3. Read what the Gardener project `alpha` owes:

   ```sh
   jq '{project_id, platform, own_items: (.line_items | length), related_costs: [.related_costs[] | {relation_type, project_id, platform, items: (.line_items | length), total}], total}' ~/tally-tutorial/2026-07-attributed/statement-garden-sim%2Falpha.json
   ```

   ```json
   {
     "project_id": "alpha",
     "platform": "gardener",
     "own_items": 0,
     "related_costs": [
       {
         "relation_type": "infrastructure_tenant",
         "project_id": "005be5adeef3d87e280d03d9d57c38b4",
         "platform": "openstack",
         "items": 309,
         "total": 3661.32
       }
     ],
     "total": 3661.32
   }
   ```

   `alpha` has 0 line items of its own and one related cost, of type
   `infrastructure_tenant`, for `005be5adeef3d87e280d03d9d57c38b4` on platform
   `openstack`, with 309 items and the total 3661.32, and its statement total is
   3661.32. All of it has to match. The 309 line items are the ones Meter and
   rate your first month read on the tenant's own statement, moved whole under
   `related_costs`.

## See that nothing is billed twice

1. List every statement of the run with its total:

   ```sh
   jq -r '.statements[] | "\(.cloud)/\(.project_id)\t\(.total)"' ~/tally-tutorial/2026-07-attributed/run.json
   ```

   ```text
   garden-sim/alpha	3661.32
   garden-sim/beta	651.24
   os-sim/018504a6cc10019a40e3f9eef4dae529	788.97
   os-sim/10e287d5788957a2a331cabd1b5dccdf	593.55
   os-sim/34e991db9fc6466f8ca69b43f70fce65	538.55
   os-sim/d5a8024946ddf673277b9e2490643a2c	814.59
   ```

   Six statements: the two Gardener projects at their tenants' totals, 3661.32
   and 651.24, and the four tenants at the totals the two lessons before
   produced. All six have to match.

2. Sum the statements of the reseller run and of this one:

   ```sh
   jq '[.statements[].total] | add' ~/tally-tutorial/2026-07-reseller/run.json
   ```

   ```text
   7048.22
   ```

   ```sh
   jq '[.statements[].total] | add' ~/tally-tutorial/2026-07-attributed/run.json
   ```

   ```text
   7048.220000000001
   ```

   The two sums are equal. The trailing digits of the second are `jq`'s
   floating-point rendering of the same sum, which it adds in double precision.
   Attribution moves costs from one statement to another and adds none.

3. Read the list of projects the run found no registry row for:

   ```sh
   jq '.stats.unregistered_projects' ~/tally-tutorial/2026-07-attributed/run.json
   ```

   ```text
   null
   ```

   The list is absent because the registry is full, so `jq` prints null. The
   list-price run of Meter and rate your first month carried six entries here.

## What you learned

- Every project is billed in exactly one place, and a project an attributing
  relation names is excluded from direct billing:
  [exclusive attribution](/explanation/project-registry-relations-and-attribution#exclusive-attribution).
- The engine resolves that in one breadth-first walk where the shortest path
  claims a project and the smallest relation id breaks a tie, and a second path
  is a warning rather than a second bill:
  [exclusive attribution](/explanation/project-registry-relations-and-attribution#exclusive-attribution).
- The pattern you built is the one the concept page draws:
  [a Gardener project and its infrastructure tenant](/explanation/project-registry-relations-and-attribution#a-gardener-project-and-its-infrastructure-tenant).
- A rendered statement of that shape, with a management fee of its own beside
  the related costs, is in
  [related costs: Gardener and OpenStack](/explanation/worked-examples#related-costs-gardener-and-openstack).
- The `related_costs` member is specified in
  [statements](/reference/formats/exports#statements) and the registry endpoints
  you called in [the Reporting API](/reference/api/reporting-api).

## Where to go next

[Finalize the month and export it](/tutorials/finalize-the-month-and-export-it)
closes July 2026 on the run you just made.

It starts from the state this lesson leaves behind:

- everything Pay a reseller a kickback left;
- the registry complete, with eight projects (the six `os-sim` tenants,
  `garden-sim/alpha` and `garden-sim/beta`), the meta-project `acme`, the
  partner `cloudhouse` and six relations;
- `RUN_ID` on the attributed run, which superseded the reseller run;
- the export under `~/tally-tutorial/2026-07-attributed`;
- `ALPHA_TENANT_ID`, `BETA_TENANT_ID`, `ALPHA_ID` and `BETA_ID` in the shell.

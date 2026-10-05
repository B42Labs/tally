---
title: Reconcile a cloud
description: Configure the OpenStack reconciliation adapter for one cloud and trigger a sync of it over the internal endpoint.
quadrant: how-to
audience: operator
---

# Reconcile a cloud

This guide gives the Reporting API what it needs to reconcile one OpenStack
cloud: a clouds.yaml it can read, an account that may list every project's
resources, an entry naming the cloud, and the call that runs a sync. What a run
observes and how it corrects the projection is in
[how reconciliation observes a cloud](/explanation/how-reconciliation-observes-a-cloud).

## Before you start

- The Reporting API deployed and reachable, and the shared internal token it
  guards the internal routes with.
- A `clouds.yaml` for the cloud, with an admin-scoped account, in a Secret the
  Reporting API pod mounts. A `secure.yaml` beside it is merged over it, which
  is where an entry's password belongs when the clouds.yaml itself is not a
  secret.
- The `openstack` client on your machine, reading the same file, for the
  account check.
- The [Reporting API settings](/reference/configuration/tally-reporting) page,
  which names `TALLY_REPORTING_CLOUDS_CONFIG` (the clouds file the API reads at
  startup), `TALLY_REPORTING_SYNC_ALLOW_AT`, `TALLY_REPORTING_SYNC_BUDGET_S` and
  `TALLY_REPORTING_SYNC_SETTLE_S`.

## Restrict the account to read requests

This section is optional. It replaces the account's password in the Secret with
an application credential that every API answers for the `GET` requests of a
sync alone
([a credential that only reads](/explanation/how-reconciliation-observes-a-cloud#a-credential-that-only-reads)).
It needs an entry on your machine that authenticates the same account with its
password, called `os-prod-eu1-password` in the steps.

1. Set `service_type` under `[keystone_authtoken]` in the configuration of each
   API the sync reads to that API's type in the catalog. List the types:

   ```sh
   openstack --os-cloud os-prod-eu1-password catalog list -c Name -c Type
   ```

   ```text
   +----------+---------------+
   | Name     | Type          |
   +----------+---------------+
   | nova     | compute       |
   | cinder   | block-storage |
   | neutron  | network       |
   | glance   | image         |
   | octavia  | load-balancer |
   | keystone | identity      |
   +----------+---------------+
   ```

   On a Kolla deployment cinder is the one API to change. Add the setting to
   `/etc/kolla/config/cinder.conf` and reconfigure cinder:

   ```ini
   [keystone_authtoken]
   service_type = block-storage
   ```

   ```sh
   kolla-ansible reconfigure -i multinode --tags cinder
   ```

2. Create the credential as the account itself:

   ```sh
   openstack --os-cloud os-prod-eu1-password application credential create \
     --role admin \
     --access-rules '[
       {"service": "compute", "method": "GET", "path": "/**/servers/detail"},
       {"service": "compute", "method": "GET", "path": "/**/flavors/detail"},
       {"service": "compute", "method": "GET", "path": "/**/"},
       {"service": "block-storage", "method": "GET", "path": "/**/volumes/detail"},
       {"service": "block-storage", "method": "GET", "path": "/**/types"},
       {"service": "network", "method": "GET", "path": "/**/floatingips"},
       {"service": "image", "method": "GET", "path": "/**/images"},
       {"service": "load-balancer", "method": "GET", "path": "/**/lbaas/loadbalancers"}
     ]' \
     -f yaml -c id -c secret \
     tally-reconciliation
   ```

   ```text
   id: 6b1f0c2d3e4f5a6b7c8d9e0f1a2b3c4d
   secret: <the secret, shown this once>
   ```

   The `service` of a rule is the `service_type` of step 1, not the name a
   client calls the API by. The `load-balancer` rule belongs to a cloud whose
   entry sets `include_octavia`; leave it out otherwise. `include_octavia` is
   also what gives a load balancer its listener and pool counts, which
   octavia's notifications leave out. Keystone shows the secret once.

   Keystone cannot change the rules of an existing credential. A credential
   created with seven rules, without the `types` one, ends every run `failed`
   on the volume type listing: create a new one with all eight and replace the
   entry with it in step 3.

3. Replace the entry the Secret carries. `clouds.yaml`:

   ```yaml
   clouds:
     os-prod-eu1:
       auth_type: v3applicationcredential
       auth:
         auth_url: https://keystone.example.com:5000/v3
         application_credential_id: 6b1f0c2d3e4f5a6b7c8d9e0f1a2b3c4d
       region_name: RegionOne
       interface: public
   ```

   `secure.yaml`:

   ```yaml
   clouds:
     os-prod-eu1:
       auth:
         application_credential_secret: <the secret>
   ```

   The entry names no user, no password and no project, because the credential
   carries its project. The `os-prod-eu1-password` entry stays on your machine
   and never reaches the Secret.

Continue with the sections below, using the new `os-prod-eu1` entry.

## Mount the clouds file

1. Mount the Secret at `/etc/openstack/`, the directory a Kubernetes Secret
   volume mounts at, and set `OS_CLIENT_CONFIG_FILE` to the file it carries.
   The variable makes that file the only location searched:

   ```yaml
   env:
     - name: OS_CLIENT_CONFIG_FILE
       value: /etc/openstack/clouds.yaml
   ```

2. Check that the pod reads the file you meant. Without the variable the search
   starts in the process working directory and reaches `/etc/openstack/` last:

   ```sh
   kubectl exec deployment/reporting-api -- ls /etc/openstack/
   ```

   ```text
   clouds.yaml
   secure.yaml
   ```

## Check the account

1. Run the three requests the adapter depends on, against the same file it
   reads:

   ```sh
   openstack --os-cloud os-prod-eu1 token issue
   openstack --os-cloud os-prod-eu1 server list --all-projects
   openstack --os-cloud os-prod-eu1 server list --all-projects --deleted
   ```

   ```text
   +------------+----------------------------------+
   | Field      | Value                            |
   +------------+----------------------------------+
   | expires    | 2026-07-09T15:22:00+0000         |
   | project_id | 9c4a1b2d3e4f5061728394a5b6c7d8e9 |
   | user_id    | 1f0e9d8c7b6a5948372615043f2e1d0c |
   +------------+----------------------------------+
   +--------------------------------------+--------+--------+
   | ID                                   | Name   | Status |
   +--------------------------------------+--------+--------+
   | 1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d | web-01 | ACTIVE |
   | 2b3c4d5e-6f7a-4b8c-9d0e-1f2a3b4c5d6e | db-01  | ACTIVE |
   +--------------------------------------+--------+--------+
   +--------------------------------------+---------+---------+
   | ID                                   | Name    | Status  |
   +--------------------------------------+---------+---------+
   | 3c4d5e6f-7a8b-4c9d-8e0f-2a3b4c5d6e7f | web-00  | DELETED |
   +--------------------------------------+---------+---------+
   ```

   The second call is the request every run probes the cloud with, and the
   third is the deleted-servers listing a missed delete is dated from. Both
   have to answer with more than the account's own project. A cloud that
   refuses the probe ends the run with an error naming the clouds.yaml entry,
   before a single listing follows.

   With a credential restricted to read requests, a 401 on the second or third
   call comes from an API that cannot validate the credential's rules. That is
   repaired in step 1 of
   [restrict the account to read requests](#restrict-the-account-to-read-requests).

## Name the cloud

1. Add one entry per cloud to the file `TALLY_REPORTING_CLOUDS_CONFIG` names:

   ```yaml
   clouds:
     - cloud: os-prod-eu1
       platform: openstack
       adapter: openstack
       adapter_config:
         os_cloud: os-prod-eu1
         include_octavia: true
   ```

   [`cloud`, `platform`, `adapter` and `adapter_config`](/reference/configuration/clouds-file#entries)
   are the members of an entry, and
   [`os_cloud` and `include_octavia`](/reference/configuration/clouds-file#the-openstack-adapter)
   are the two settings this adapter takes. Parsing is strict: a key the
   adapter does not know is refused with the key named.

2. Restart the Reporting API. It reads its clouds once, at startup:

   ```sh
   kubectl rollout restart deployment/reporting-api
   kubectl rollout status deployment/reporting-api
   ```

   ```text
   deployment "reporting-api" successfully rolled out
   ```

## Trigger a sync

1. Run one sync of one cloud:

   ```sh
   curl -sS -X POST \
     -H "Authorization: Bearer $TALLY_REPORTING_INTERNAL_TOKEN" \
     https://tally-reporting.internal/internal/sync/os-prod-eu1
   ```

   ```json
   {"sync_run_id": "...", "stats": {"created": 3, "updated": 1, "deleted": 2}}
   ```

   `POST /internal/sync/{cloud}` is not part of the public API
   ([the Reporting API reference](/reference/api/reporting-api)). It is guarded
   by the shared internal token, the value of
   `TALLY_REPORTING_INTERNAL_TOKEN` or of the file
   `TALLY_REPORTING_INTERNAL_TOKEN_FILE` names, presented as a bearer token,
   and it takes no other credential. Whatever drives the sync schedule calls
   it, one call per configured cloud.

## Tell a sync the instant it runs at (development only)

`TALLY_REPORTING_SYNC_ALLOW_AT` exists for a development deployment that
reconciles a simulated cloud, whose clock is not the wall clock. A production
deployment keeps the default, where such a request is refused before a run
starts. With the setting on, every caller of the internal endpoint chooses the
instant the run's `sync_runs` row starts at and the timestamp every correction
the run books carries, and the endpoint takes the one shared static bearer
token and nothing narrower — so the audit row of a back-dated run carries the
instant its caller picked.

1. Set `TALLY_REPORTING_SYNC_ALLOW_AT` on the deployment that is to take the
   instant. It is `false` by default:

   ```sh
   kubectl set env deployment/reporting-api TALLY_REPORTING_SYNC_ALLOW_AT=true
   ```

   ```text
   deployment.apps/reporting-api env updated
   ```

2. Name the instant the run happens at in a JSON body whose one optional member
   is an RFC 3339 timestamp:

   ```sh
   curl -sS -X POST \
     -H "Authorization: Bearer $TALLY_REPORTING_INTERNAL_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"at": "2026-07-09T14:22:00Z"}' \
     https://tally-reporting.internal/internal/sync/os-prod-eu1
   ```

   ```json
   {"sync_run_id": "...", "stats": {"created": 3, "updated": 1, "deleted": 2}}
   ```

   A call with no body, one whose body is empty, and one whose body carries no
   `at` are the call of the section above: the run reads the wall clock.

3. A deployment that sets nothing answers a body carrying `at` with 400. The
   refusal comes before the syncer runs, so the request leaves no `sync_runs`
   row behind:

   ```sh
   curl -sS -X POST \
     -H "Authorization: Bearer $TALLY_REPORTING_INTERNAL_TOKEN" \
     -H 'Content-Type: application/json' \
     -d '{"at": "2026-07-09T14:22:00Z"}' \
     https://tally-reporting.internal/internal/sync/os-prod-eu1
   ```

   ```json
   {"type":"urn:tally:error:validation","title":"Validation failed","status":400,"detail":"this deployment does not take a sync instant; TALLY_REPORTING_SYNC_ALLOW_AT is off"}
   ```

## Check the result

1. A run that finished is answered 200 with its id and the corrections it
   booked:

   ```sh
   curl -sS -X POST \
     -H "Authorization: Bearer $TALLY_REPORTING_INTERNAL_TOKEN" \
     https://tally-reporting.internal/internal/sync/os-prod-eu1
   ```

   ```json
   {"sync_run_id": "6c1f0a94-3b52-4d7e-8f10-2a5b7c9d0e13", "stats": {"created": 3, "updated": 1, "deleted": 2}}
   ```

2. A cloud the configuration does not name is answered 404. A cloud another run
   is holding is answered 409, and that lock lives in the database, so it holds
   across replicas. A run that recorded any error at all is answered 500, and
   its `sync_runs` row holds the reasons:

   ```sh
   curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
     -H "Authorization: Bearer $TALLY_REPORTING_INTERNAL_TOKEN" \
     https://tally-reporting.internal/internal/sync/os-prod-eu2
   ```

   ```text
   404
   ```

3. Read the last runs of the cloud back from the database:

   ```sql
   SELECT started_at, completed_at, status, stats
   FROM sync_runs
   WHERE cloud = 'os-prod-eu1'
   ORDER BY started_at DESC
   LIMIT 5;
   ```

   ```text
            started_at         |        completed_at        |  status   |                                                 stats
   ----------------------------+----------------------------+-----------+--------------------------------------------------------------------------------------------------------
    2026-07-09 14:22:00.512+00 | 2026-07-09 14:22:07.118+00 | completed | {"errors": [], "created": 3, "deleted": 2, "updated": 1, "deferred": {"recent": 0, "transitional": 0}}
    2026-07-09 13:22:00.401+00 | 2026-07-09 13:22:01.930+00 | failed    | {"errors": ["unknown setting \"include_octavia_lb\""]}
   ```

   `deferred` counts the corrections the run left to a later run: `transitional`
   for a resource the platform was still changing, `recent` for one changed
   inside `TALLY_REPORTING_SYNC_SETTLE_S` before the run. A count that stays
   above zero for the same cloud points at a resource stuck in transition.

   The Reporting API's log carries the same reasons on the request that
   triggered the run, together with the `sync_run_id` the row is found by.

## Give a large cloud a longer budget

A run is bounded by `TALLY_REPORTING_SYNC_BUDGET_S`, 45 seconds by default. A
cloud whose enumeration does not fit that budget completes no run until you
raise it.

1. Read how long the runs take:

   ```sql
   SELECT started_at, completed_at, completed_at - started_at AS took, status, stats->'errors'
   FROM sync_runs
   WHERE cloud = 'os-prod-eu1'
   ORDER BY started_at DESC
   LIMIT 5;
   ```

   ```text
            started_at         |        completed_at        |     took     | status |                                                ?column?
   ----------------------------+----------------------------+--------------+--------+---------------------------------------------------------------------------------------------------------
    2026-07-09 14:22:00.512+00 | 2026-07-09 14:22:45.530+00 | 00:00:45.018 | failed | ["listing the resources of os-prod-eu1: the run ended while enumerating server: context deadline exceeded"]
   ```

   A run that ended on the budget is `failed`, took about the budget, and
   carries `context deadline exceeded` in its errors.

2. Set the budget and restart:

   ```sh
   kubectl set env deployment/reporting-api TALLY_REPORTING_SYNC_BUDGET_S=300
   kubectl rollout status deployment/reporting-api
   ```

   ```text
   deployment.apps/reporting-api env updated
   deployment "reporting-api" successfully rolled out
   ```

   The value is in seconds, applies to every cloud of the deployment, and is
   refused at startup outside 1 to 86400.

3. Give whatever calls the route a client timeout of the budget plus 20
   seconds, `--max-time 320` for a budget of 300:

   ```sh
   curl -sS --max-time 320 -X POST \
     -H "Authorization: Bearer $TALLY_REPORTING_INTERNAL_TOKEN" \
     https://tally-reporting.internal/internal/sync/os-prod-eu1
   ```

   A client that gives up earlier closes the connection, which cancels the run
   and records it `failed`.

4. Keep the budget under the interval of the sync schedule. A call that arrives
   while a run holds the cloud is answered 409, and `TallySyncStale` fires when
   no run completes within 30 minutes.

5. Size the connection pool for the clouds you sync at the same time. A running
   sync keeps one connection of `TALLY_REPORTING_DB_MAX_CONNS` checked out for
   as long as it runs and takes a second one for each write, so a longer budget
   holds that connection longer. Raise `TALLY_REPORTING_DB_MAX_CONNS` by the
   number of clouds synced at the same time, or stagger the schedule so that
   their runs do not overlap.

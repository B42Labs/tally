# OpenStack reconciliation adapter

The adapter is the OpenStack half of the reconciliation loop. A sync asks it
what one cloud currently holds, the framework diffs that observation against the
projection, and the difference goes back in as synthetic events through the
ordinary ingest pipeline. A resource a run corrected therefore ends up with the
same kind of history as one that was never missed, and the run itself is
recorded in `sync_runs`.

Every run enumerates the instances, the volumes, the floating IP addresses, and
the images of all projects of the cloud, and its load balancers where
`include_octavia` is set. It also asks nova for the servers it destroyed since
the last completed run, which is the one listing that dates a missed delete at
the instant the platform performed it.

The adapter is compiled into `tally-reporting` and runs inside it. It has no
process, image, or port of its own. What a deployment provides is a clouds.yaml
the Reporting API pod can read, an account in it that may list every project's
resources, and something that calls the sync endpoint on a schedule.

## Where the credentials come from

The adapter reads a cloud through the clouds.yaml every other OpenStack client
on the host reads, using gophercloud's `openstack/config/clouds`. No credential
enters Tally's own configuration: `adapter_config` names an entry of that file,
and the file alone says what the entry means. A rotated password reaches the
next sync as soon as the file the pod reads carries it, with no change to Tally.

Set `OS_CLIENT_CONFIG_FILE` to the file the deployment mounts. It is then the
only location searched, which is what keeps a `clouds.yaml` that reached the
process by another route from outranking the Secret:

```yaml
env:
  - name: OS_CLIENT_CONFIG_FILE
    value: /etc/openstack/clouds.yaml
```

Without it the search runs over three locations, in order:

1. `clouds.yaml` in the working directory of the process,
2. `${XDG_CONFIG_HOME:-$HOME/.config}/openstack/clouds.yaml`,
3. `/etc/openstack/clouds.yaml`.

The mounted Secret is last of those three and the working directory is first,
and this one file decides which Keystone the adapter authenticates against and
whose inventory is written into billing records as corrections. Anything that
can drop a `clouds.yaml` into the working directory — a writable volume mounted
there, an artifact in a base layer, a sidecar sharing the volume — therefore
outranks the Secret. Setting the variable removes the question; a
`readOnlyRootFilesystem: true` on the pod is the other half of it.

The first file found is the one used, and a `secure.yaml` beside it is merged
over it, which is where an entry's password belongs when the clouds.yaml itself
is not a secret. `/etc/openstack/` is the directory a Kubernetes Secret volume
mounts at. A deployment with the file in none of those places fails every sync
of every OpenStack cloud, with the locations it searched named in the error.

Two OpenStack variables of the pod's environment still reach the parse:
`OS_REGION_NAME` overrides the entry's `region_name`, and `OS_INTERFACE`
overrides its `interface`. Both apply to every cloud the process syncs, so a
stray one retargets all of them. `OS_CLOUD` has no such effect, because the
`os_cloud` setting is what picks the entry.

## The account an entry names

The account has to be admin-scoped. The instance and the volume listings ask for
`all_tenants`, and the deleted-servers listing asks nova for `deleted=true`.
Both are admin operations in the stock policies. The floating IP and the image
listings pass no project filter at all: neutron and glance answer with every row
the account's policy lets it see, which for an admin-scoped account is the whole
cloud.

The run establishes that against the cloud, before it observes anything: it asks
nova for one server across every project, and a cloud that refuses that request
ends the run with an error naming the clouds.yaml entry. Not one listing
follows. This is not a formality: only nova and cinder answer a lesser account
with a 403, which the run would report as an enumeration error for that one
resource type. Neutron, glance and octavia have no `all_tenants` flag at all —
they narrow the listing to the caller's own project and answer `200 OK`. A
narrowed listing is a complete listing as far as the framework can tell, so
every resource of every other project would be a projection row the run did not
name, and the missed-delete pass would book a delete for each one. That
correction is permanent: the diff skips a row it already holds as deleted, so no
later run with a repaired account undoes it.

Nothing configures this, and nothing can. `policy.yaml` is a per-deployment
file, so a cloud that resolves `context_is_admin` to a role of its own name
needs no setting here — the cloud answers an account that holds that role and
refuses one that does not, whatever it is called. A setting naming the role
would establish nothing either way: it would only say which name to compare
against, so naming a role the token already carries would pass the check while
the three silent listings still narrowed.

Losing the reach is not a hypothetical. It is what a credential rotation that
recreates an application credential without its role assignment leaves behind,
and what anyone who can edit the mounted `clouds.yaml`/`secure.yaml` Secret can
arrange. The probe turns both into a failed run rather than into a wiped
projection.

An account can still be checked against the cloud from the command line, against
the same file the adapter reads:

```sh
openstack --os-cloud os-prod-eu1 token issue
openstack --os-cloud os-prod-eu1 server list --all-projects
openstack --os-cloud os-prod-eu1 server list --all-projects --deleted
```

The first names the roles the token carries. The second is the request the run
probes with: an account that may list every project's servers answers it with
more than its own project's instances, and the third with the servers nova has
destroyed.

## Configuring a cloud

The Reporting API reads its clouds at startup, from the YAML file
`TALLY_REPORTING_CLOUDS_CONFIG` points at. One entry configures one cloud:

```yaml
clouds:
  - cloud: os-prod-eu1
    platform: openstack
    adapter: openstack
    adapter_config:
      os_cloud: os-prod-eu1
      include_octavia: true
```

`cloud` is the installation. It is the name the collector's events carry, the
path segment the sync endpoint takes, and the name a run is recorded under.
`platform` and `adapter` are both `openstack` for this adapter; the process
checks the pair against the registered adapters when it reads the file and
refuses to start on a cloud that names an unknown adapter or an adapter for
another platform.

`adapter_config` carries the two settings the adapter has:

- `os_cloud` (required) is the entry in clouds.yaml this cloud authenticates
  with. It is unrelated to the `cloud` name above. The two are equal in the
  example because a deployment that names them alike is easier to read.
- `include_octavia` (optional, default false) adds `loadbalancer` to the
  enumerated types. It is off by default because a deployment that runs no
  octavia would otherwise fail to enumerate a type it does not have, on every
  sync, forever. A load balancer is reported with its listener and pool counts,
  and migration `0006_seed_loadbalancer_type.sql` registers the size schema
  those two are validated against, so on a database the chain seeded the
  corrections land whatever `TALLY_INGEST_REQUIRE_SIZE_SCHEMA` is set to.

  A database that already registered `openstack/loadbalancer` keeps its own
  document: the migration leaves an operator's row alone rather than fail the
  upgrade on the duplicate key, and it reports success either way. Once any
  schema is registered it is enforced in both modes — the setting only decides
  what happens to a pair nothing registers — so a document that does not accept
  `{"listeners": <integer>, "pools": <integer>}` refuses every load balancer
  correction with `size_schema: ...`, strict or lax. Compare the row against
  what the migration seeds before enabling `include_octavia`:

  ```sql
  SELECT size_schema FROM resource_types
  WHERE platform = 'openstack' AND resource_type = 'loadbalancer';
  ```

  Rolling the chain back below 6 deletes that row only while it still holds the
  document the migration wrote, marker included. A document that was edited
  through `PUT /resource-types/openstack/loadbalancer` — the marker is part of
  what a `GET` answers, so it rides along unless it is removed — is the
  operator's and survives the rollback.

Parsing is strict. A setting the adapter does not know is refused with the key
named rather than ignored, so an operator who misspells `include_octavia` learns
that instead of getting exactly the sync they did not ask for.

Nothing validates `adapter_config` at startup. The cloud name, the platform, and
the adapter of every entry are checked when the process reads the file, and the
framework has no hook for what an adapter makes of its own settings, so a
mistake in one surfaces on the first sync run of that cloud. That run aborts
before it observes anything: it leaves a `sync_runs` row at status `failed`
whose `stats.errors` names the setting, and the endpoint answers 500. The reason
is not in the response, because these errors carry platform detail. It is read
back from the row:

```sql
SELECT started_at, completed_at, status, stats
FROM sync_runs
WHERE cloud = 'os-prod-eu1'
ORDER BY started_at DESC
LIMIT 5;
```

The Reporting API's log carries the same reasons on the request that triggered
the run, together with the `sync_run_id` the row is found by.

## Triggering a sync

One sync of one cloud is one call:

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $TALLY_REPORTING_INTERNAL_TOKEN" \
  https://tally-reporting.internal/internal/sync/os-prod-eu1
```

`POST /internal/sync/{cloud}` is not part of the public API. It is guarded by
the shared internal token, the value of `TALLY_REPORTING_INTERNAL_TOKEN` or of
the file `TALLY_REPORTING_INTERNAL_TOKEN_FILE` names, presented as a bearer
token, and it takes no other credential. Whatever drives the sync schedule calls
it, one call per configured cloud.

The run is synchronous, so the caller learns the outcome from the response
rather than by polling for it:

```json
{"sync_run_id": "...", "stats": {"created": 3, "updated": 1, "deleted": 2}}
```

A cloud the configuration does not name is answered 404. A cloud another run is
holding is answered 409, because two syncs of one cloud would diff the same
projection rows against two overlapping observations; the lock behind that
answer lives in the database, so it holds across replicas. A run that recorded
any error at all is answered 500, and its `sync_runs` row holds the reasons.

A run is bounded at 45 seconds, under the server's write timeout of 60, so that
it ends while the connection that asked for it is still there to be answered. A
cloud whose enumeration does not fit that budget never completes a run. The work
that budget has to cover does not grow with the outage: the deleted-servers
listing is the one part of a run bounded by how long the cloud has gone without
a completed one, and it is clamped at 24 hours.

## Telling a sync the instant it runs at

The call may name the instant the run happens at, in a JSON body whose one
optional member is an RFC 3339 timestamp:

```sh
curl -sS -X POST \
  -H "Authorization: Bearer $TALLY_REPORTING_INTERNAL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"at": "2026-07-09T14:22:00Z"}' \
  https://tally-reporting.internal/internal/sync/os-prod-eu1
```

A call with no body, one whose body is empty, and one whose body carries no `at`
are the call the section above shows: the run reads the wall clock.

The member is taken only where the deployment sets
`TALLY_REPORTING_SYNC_ALLOW_AT`, and it is `false` by default. A deployment that
sets nothing answers a body carrying `at` with 400 and the detail `this
deployment does not take a sync instant; TALLY_REPORTING_SYNC_ALLOW_AT is off`.
That refusal comes before the syncer runs, so a request nothing reconciled for
leaves no `sync_runs` row behind. A body that is not a JSON object of that shape
is refused in the same place and with the same status.

A told instant is the run's one clock. It is read once and answered for the rest
of the run, so every correction the run books at poll time carries it however
long the run takes, and the 24 hours the deleted-servers window is clamped to
are measured back from it rather than from the wall clock. The run's
`sync_runs.started_at` is it as well, because that column is where the next run
of this cloud opens its window: a told run whose row said `now()` would send the
next one back to the wall clock. `completed_at` is not told. The row of a told
run therefore carries a virtual `started_at` beside the wall instant the run
finished at.

The instants told to the runs of one cloud must not go backwards, and nothing
refuses one that does. The bound a run starts from is the newest `started_at` of
the cloud's completed runs, so a run told an earlier instant leaves that bound
where it is, ahead of the instant the run is at. Nova answers a window that has
not happened yet with an empty listing rather than with a refusal, so the run
asks it for the whole 24 hours behind its own instant instead, and logs at warn
level that it did. The deletes older than that window are the ones the absence
pass dates at poll time, the same as after an outage. So are the deletes newer
than the instant the run is at: `changes-since` is a lower bound and nova has no
upper one, so a listing runs to the cloud's own present, and an instance the
cloud destroyed after the run was told it ran would otherwise carry a correction
dated outside the period the run reconciled. What it corrects still lands: a
correction dated behind the newest event of the row it corrects is dated one
microsecond past that event instead, because a correction the fold does not
order last decides nothing. Such a correction carries the instant the row forced
rather than the one the request named.

The seam is there for the development deployment that reconciles the simulated
cloud. That cloud lives in a generated month on a virtual clock, so a sync at
wall time would observe it outside its period.
[`openstack-simulator.md`](openstack-simulator.md) describes the fake OpenStack
API a sync reads it through and the loop that tells each sync where the month
stands.

## What a partial outage does to a run

A service that stops answering must never read as a cloud that holds nothing.
The adapter reports a failure it can attribute to one resource type as an
enumeration error for that type alone. Locating the service in the catalog and
every request of its listing are both such failures, so a cinder that is down
costs the run its volumes and nothing else. Three things follow from such a
failure.

The reason lands in the run's `stats.errors`, naming the type it concerns
(`enumerating volume: ...`), and `stats.error_count` counts it.

The missed-delete pass leaves that type alone. Only a type the run reached the
end of may conclude that a row it did not name is gone, so the volume rows of
the projection keep the state they hold rather than being booked as deleted.

The run ends `failed`, whatever it corrected for the types that finished. Those
corrections are kept: an enumeration failure is missing information, and the
facts the run did establish are facts either way.

Because the run is `failed`, it does not move the bound the next run starts
from. That bound is the `started_at` of the last run of the cloud that
completed, so the next sync walks the same window again and repairs what the
outage cost, as soon as the service answers.

A flavor listing the cloud refuses is reported the same way. The instances are
still observed, without the sizes nova did not describe, and the instance type
counts as incomplete for the run. A nova that speaks compute microversion 2.47
or later is not asked for its flavors at all: from that microversion on it
reports each server's flavor out of the instance's own record, so an instance
running on a flavor the operator has since retired still says what it is made
of. The microversion is negotiated, not demanded — an older nova answers the
listing as it always did and the flavor catalog is read as before.

An image glance names no owner for costs the run its images, in the same way and
for the same reason. The collector books such an image to the project of whoever
registered it, which this run has no way to recover, so the image type stays
incomplete rather than that live row being booked deleted. A deactivated image
is not one of these: it still exists, still occupies the store, and is observed
like any other.

A failure that says nothing about any single resource type ends the whole run
before a type is observed: an `adapter_config` that does not parse, a
clouds.yaml entry that cannot be resolved, and a Keystone that refuses the
credentials are all of that kind. Reporting them per type would let a sync
conclude that a cloud it never reached holds nothing.

## The instants a missed delete is booked at

Two passes book a delete the collector missed, and they date it differently.

The deleted-servers listing asks nova for `deleted=true` bounded by
`changes-since`, at the start of the last completed run, and every server it
returns is booked at the `terminated_at` nova reports. A cloud's first run has
no such bound and asks for no deleted servers, because it has no window behind
it to catch up on. The absence pass books what the observation did not name at
poll time, the instant the sync ran.

That window is clamped at 24 hours. A failed run does not move the bound, so a
cloud that has not completed one in a week would otherwise ask nova for a week
of its churn, inside the same 45 seconds the five live listings share, and time
out before it can complete and move the bound — every following run asking for
more and getting through less. What the clamp leaves out is not lost: those
deletes are booked by the absence pass at poll time, which is the approximation
every other resource type lives with anyway.

The difference between the two instants is what the resource is billed for. A
correction's timestamp is what the lifecycle records as the end of the resource,
so a delete booked at poll time bills every hour between the platform's
termination and the sync that noticed it. The approximation is not corrected
afterwards, because the diff skips a row it already holds as deleted: the first
delete correction of a resource is the one that stands.

That is why a failure of the deleted listing costs the instance type its
completeness even though the live listing succeeded. The absence pass must not
book those deletes at poll time in a run that could not read the platform's
instants, and the failed run leaves the window to the next one.

Instances are the only type a listing of deletions exists for. A missed delete
of a volume, a floating IP, an image, or a load balancer is found by absence
alone and dated at the poll that found it, so the interval between two syncs of
a cloud bounds how far such a correction can sit past the deletion it records.

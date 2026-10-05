---
title: How reconciliation observes a cloud
description: What one OpenStack sync establishes, why an admin-scoped account is checked before anything is observed, and how a missed delete is dated.
quadrant: explanation
audience: all
---

# How reconciliation observes a cloud

[Dual ingestion and reconciliation](/explanation/dual-ingestion-and-reconciliation)
argues why a periodic sync exists at all. This page is the OpenStack half of it:
what one run establishes about a cloud, what it refuses to conclude, and which
instant each correction ends up carrying. The steps to configure and trigger a
sync are in
[reconcile a cloud](/how-to/openstack/reconcile-a-cloud).

## What a run does

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

After the listings, a run reads cinder's volume types, the private ones
included, and stores each type's id with its name in the `size_names` table.
Cinder's notifications carry a volume's type by id while its volume listing
reports the name, and a price list keys `type_modifiers` by name. The stored
names are what the Reporting API replaces the id with when a collector event
arrives, so a collector event and a correction carry the same type
([size names](/reference/formats/canonical-event#size-names)). The type listing
sends no probe of its own: the listings before it already proved the scope.

The adapter is compiled into `tally-reporting` and runs inside it. It has no
process, image, or port of its own. What a deployment provides is a clouds.yaml
the Reporting API pod can read, an account in it that may list every project's
resources, and something that calls the sync endpoint on a schedule. That the
reconciliation credentials sit next to the deployment configuration rather than
in the API's database is decision D6 of
[the Phase 1 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/01-phase-1-core-platform-openstack.md).

## Where the credentials come from

The adapter reads a cloud through the clouds.yaml every other OpenStack client
on the host reads, using gophercloud's `openstack/config/clouds`. No credential
enters Tally's own configuration: `adapter_config` names an entry of that file,
and the file alone says what the entry means. A rotated password reaches the
next sync as soon as the file the pod reads carries it, with no change to Tally.

Without `OS_CLIENT_CONFIG_FILE` the search runs over three locations, in order:

1. `clouds.yaml` in the working directory of the process,
2. `${XDG_CONFIG_HOME:-$HOME/.config}/openstack/clouds.yaml`,
3. `/etc/openstack/clouds.yaml`.

The mounted Secret is last of those three and the working directory is first,
and this one file decides which Keystone the adapter authenticates against and
whose inventory is written into billing records as corrections. Anything that
can drop a `clouds.yaml` into the working directory (a writable volume mounted
there, an artifact in a base layer, a sidecar sharing the volume) therefore
outranks the Secret. Setting the variable to the file the deployment mounts
removes the question, because it is then the only location searched; a
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
follows. This is not a formality: only nova answers a lesser account with a 403,
which the run would report as an enumeration error for that one resource type.
Cinder accepts `all_tenants` and ignores it for an account that is not admin,
and neutron, glance and octavia have no such flag at all, so all four narrow the
listing to the caller's own project and answer `200 OK`. A narrowed listing is a
complete listing as far as the framework can tell, so every resource of every
other project would be a projection row the run did not name, and the
missed-delete pass would book a delete for each one. That correction is
permanent: the diff skips a row it already holds as deleted, so no later run
with a repaired account undoes it.

Nothing configures this, and nothing can. `policy.yaml` is a per-deployment
file, so a cloud that resolves `context_is_admin` to a role of its own name
needs no setting here: the cloud answers an account that holds that role and
refuses one that does not, whatever it is called. A setting naming the role
would establish nothing either way. It would only say which name to compare
against, so naming a role the token already carries would pass the check while
the four silent listings still narrowed.

Losing the reach is not a hypothetical. It is what a credential rotation that
recreates an application credential without its role assignment leaves behind,
and what anyone who can edit the mounted `clouds.yaml`/`secure.yaml` Secret can
arrange. The probe turns both into a failed run rather than into a wiped
projection.

## Why no reader account passes

Under the stock policies no reader role lists across projects. The probe and
both instance listings need nova's
`os_compute_api:servers:detail:get_all_tenants`, which nova checks before it
answers a request that carries `all_tenants`
([the server listing](https://github.com/openstack/nova/blob/ce37978276744e92d91251210a1d9c2784eea375/nova/api/openstack/compute/servers.py#L285-L291)).
The deleted-servers listing needs `os_compute_api:servers:allow_all_filters` as
well. Without it nova drops `deleted` from the query, because `deleted` is not
among the filters it takes from an account that lacks the rule
([the filters](https://github.com/openstack/nova/blob/ce37978276744e92d91251210a1d9c2784eea375/nova/api/openstack/compute/servers.py#L1436-L1458),
[their removal](https://github.com/openstack/nova/blob/ce37978276744e92d91251210a1d9c2784eea375/nova/api/openstack/compute/servers.py#L1538-L1554)).
Both rules resolve to `context_is_admin` and both are project-scoped
([the rules](https://github.com/openstack/nova/blob/ce37978276744e92d91251210a1d9c2784eea375/nova/policies/servers.py#L59-L97)),
and `context_is_admin` is `role:admin`
([`ADMIN`](https://github.com/openstack/nova/blob/ce37978276744e92d91251210a1d9c2784eea375/nova/policies/base.py#L40),
[`context_is_admin`](https://github.com/openstack/nova/blob/ce37978276744e92d91251210a1d9c2784eea375/nova/policies/base.py#L73-L77)).
A project reader is refused by the probe with a 403, and a system reader holds
neither the role nor a scope those rules accept.

Cinder, neutron and glance have no policy rule for a listing of every project.
They decide it by whether the request has an admin context, which is their
`context_is_admin` rule, `role:admin` unless a deployment changed it. Cinder
lists every project for such a context and the caller's own for any other
([the volume listing](https://github.com/openstack/cinder/blob/ef0d50cb18d0ceb5d70d500c9a032b0eb66e749b/cinder/volume/api.py#L665-L679),
[`context_is_admin`](https://github.com/openstack/cinder/blob/ef0d50cb18d0ceb5d70d500c9a032b0eb66e749b/cinder/policies/base.py#L153-L155)).
Neutron scopes a query to the caller's project unless the context is admin or
holds the `service` role OpenStack services call one another with
([the query scope](https://github.com/openstack/neutron-lib/blob/8c44d586e209a910495540284da7424e3db147c3/neutron_lib/db/utils.py#L167-L185),
[`context_is_admin`](https://github.com/openstack/neutron/blob/6dc774b25b9cc9b47dee127bfb41d67975677446/neutron/conf/policies/base.py#L102-L105)).
Glance treats every context that is not admin as a regular user
([the image query](https://github.com/openstack/glance/blob/a160d42e94dc5ea70cf3aa6d76e6bfaa39e47069/glance/db/sqlalchemy/api.py#L599),
[`is_admin`](https://github.com/openstack/glance/blob/a160d42e94dc5ea70cf3aa6d76e6bfaa39e47069/glance/api/policy.py#L93-L100)).
An account without the admin context is answered `200 OK` with its own project.

The probe proves nova's rule and nothing else. Under the stock policies that is
enough, because the role nova's rule asks for is the role the other services
treat as admin. A deployment that grants nova's two rules to a role of its own
in `policy.yaml`, and leaves the other services alone, hands that role a probe
it passes and four listings that narrow. The missed-delete pass then books a
delete for every volume, floating IP address, image and load balancer of every
other project, and no later run undoes it. Such a role is not supported. A role
a deployment makes `context_is_admin` in every service is an admin by another
name.

A reconciliation account that only reads is therefore established at the
credential, not at the role:
[a credential that only reads](#a-credential-that-only-reads).

## A credential that only reads

An application credential can carry access rules, each naming a service type, a
method and a path
([access rules](https://github.com/openstack/keystone/blob/537e65b2b1d4b8e0fe1faa09c8f882a6b2899b21/doc/source/user/application_credentials.rst?plain=1#L171-L228)).
The keystonemiddleware in front of each API compares every request made with the
credential's token against them and answers 401 when none matches
([the check](https://github.com/openstack/keystonemiddleware/blob/9401c513219f86008d1df380a10d57464bb20b2d/keystonemiddleware/auth_token/__init__.py#L545-L588)).
Rules for `GET` that name the requests a run sends leave the credential those
requests and nothing else.

A run sends `compute`, `block-storage`, `network`, `image` and `load-balancer`
nothing but `GET`. Eight paths need a rule: `servers/detail` for the probe and
both instance listings, `flavors/detail`, nova's version document, which the
run reads to negotiate the microversion, `volumes/detail`, `types` for cinder's
volume type listing, `floatingips`, `images` and `lbaas/loadbalancers`. The
version documents gophercloud reads from glance, neutron and octavia before it
uses them are answered before keystonemiddleware sees the request, so they need
none
([glance](https://github.com/openstack/glance/blob/a160d42e94dc5ea70cf3aa6d76e6bfaa39e47069/etc/glance-api-paste.ini#L42),
[neutron](https://github.com/openstack/neutron/blob/6dc774b25b9cc9b47dee127bfb41d67975677446/etc/api-paste.ini#L1-L15),
[octavia](https://github.com/openstack/octavia/blob/d3a882b734cdcc176301c77e20cac9f7720451b0/octavia/common/keystone.py#L25-L26)).
The only `POST` a run sends is the token request, once for the listings and
once for the volume types. It goes to keystone with the credential's secret,
before a token exists that a rule could apply to.

The role behind the credential is still `admin`, which is why the probe passes
and all five listings are complete. The restriction is on the credential: it
cannot write, and keystone does not let it create further credentials unless it
was created `--unrestricted`
([restricted credentials](https://github.com/openstack/keystone/blob/537e65b2b1d4b8e0fe1faa09c8f882a6b2899b21/doc/source/user/application_credentials.rst?plain=1#L144-L153)).
The account's password is not restricted by any of it, so it stays out of the
Secret.

An API accepts the credential under two conditions: its `[keystone_authtoken]`
section sets `service_type`, and that type is one the catalog lists. Otherwise
keystonemiddleware answers every request of the credential with 401
([the conditions](https://github.com/openstack/keystonemiddleware/blob/9401c513219f86008d1df380a10d57464bb20b2d/keystonemiddleware/auth_token/__init__.py#L551-L574)).
A rule's `service` is that type. Kolla renders `service_type = volume` for
cinder
([the template](https://github.com/openstack/kolla-ansible/blob/570f2b77142d7145e55f39126e3987002afe7494/ansible/roles/cinder/templates/cinder.conf.j2#L116-L117))
and registers cinder in the catalog as `block-storage` and `volumev3`
([the catalog entries](https://github.com/openstack/kolla-ansible/blob/570f2b77142d7145e55f39126e3987002afe7494/ansible/roles/cinder/defaults/main.yml#L346-L355)),
so cinder refuses the credential there until the setting is overridden.

A cloud that misses a condition fails the run with an error, never with a
narrowed listing. A nova that cannot validate the rules answers the probe 401,
and the run ends before a listing with
`the clouds.yaml entry "os-prod-eu1" cannot observe the whole cloud: ...`. Any
other API that answers 401 costs its resource type the run's completeness:
`enumerating volume: ...` lands in `stats.errors`, the run ends `failed`, and
the missed-delete pass leaves that type alone. An entry without its secret ends
the run with
`authenticating against the cloud "os-prod-eu1": You must provide an Application Credential Secret`,
and a credential keystone no longer accepts (expired or deleted) ends it with
keystone's refusal after the same `authenticating against the cloud` prefix.

Each path starts with `/**`, because a rule is matched against the whole path
the API receives, and that path carries the API version, on some deployments
the project ID, and on some a prefix the API is served under. `**` matches
across `/`, at the start of a pattern too
([the match](https://github.com/openstack/keystonemiddleware/blob/9401c513219f86008d1df380a10d57464bb20b2d/keystonemiddleware/auth_token/__init__.py#L280-L297)),
so `/**/servers/detail` matches the server listing in each of those shapes, and
`/**/` matches nova's version document in each of them. None of the eight
matches an image's data, a port or a backup: a leaked credential reads what a
run reads and nothing more.

The steps are in
[restrict the account to read requests](/how-to/openstack/reconcile-a-cloud#restrict-the-account-to-read-requests).

## Why adapter settings are checked on the first run

`include_octavia` adds `loadbalancer` to the enumerated types, and it is off by
default because a deployment that runs no octavia would otherwise fail to
enumerate a type it does not have, on every sync, forever. A load balancer is
reported with its listener and pool counts. No octavia notification carries
them, so the sync is their only source: the collector books a load balancer at
zero of both on its create, and the first sync that finds the load balancer out
of `pending_*` and not changed inside the settle window books the counts as a
`sync.update` (see [what a run defers](#what-a-run-defers)). Migration `0006_seed_loadbalancer_type.sql` registers the size
schema those two are validated against, so on a database the chain seeded the
corrections land whatever `TALLY_INGEST_REQUIRE_SIZE_SCHEMA` is set to.

A database that already registered `openstack/loadbalancer` keeps its own
document: the migration leaves an operator's row alone rather than fail the
upgrade on the duplicate key, and it reports success either way. Once any schema
is registered it is enforced in both modes (the setting only decides what
happens to a pair nothing registers), so a document that does not accept
`{"listeners": <integer>, "pools": <integer>}` refuses every load balancer
correction with `size_schema: ...`, strict or lax. Compare the row against what
the migration seeds before enabling `include_octavia`:

```sql
SELECT size_schema FROM resource_types
WHERE platform = 'openstack' AND resource_type = 'loadbalancer';
```

Rolling the chain back below 6 deletes that row only while it still holds the
document the migration wrote, marker included. A document that was edited
through `PUT /resource-types/openstack/loadbalancer` is the operator's and
survives the rollback, because the marker is part of what a `GET` answers and
rides along unless it is removed.

Parsing is strict. A setting the adapter does not know is refused with the key
named rather than ignored, so an operator who misspells `include_octavia` learns
that instead of getting exactly the sync they did not ask for. The keys
themselves are on
[the clouds file page](/reference/configuration/clouds-file).

Nothing validates `adapter_config` at startup. The cloud name, the platform, and
the adapter of every entry are checked when the process reads the file, and the
framework has no hook for what an adapter makes of its own settings, so a
mistake in one surfaces on the first sync run of that cloud. That run aborts
before it observes anything: it leaves a `sync_runs` row at status `failed`
whose `stats.errors` names the setting, and the endpoint answers 500. The reason
is not in the response, because these errors carry platform detail. It is read
back from the row instead.

## A synchronous, bounded run

A sync of one cloud is one call to `POST /internal/sync/{cloud}`, and the run is
synchronous, so the caller learns the outcome from the response rather than by
polling for it
([the Reporting API reference](/reference/api/reporting-api)).

A cloud the configuration does not name is answered 404. A cloud another run is
holding is answered 409, because two syncs of one cloud would diff the same
projection rows against two overlapping observations; the lock behind that
answer lives in the database, so it holds across replicas. A run that recorded
any error at all is answered 500, and its `sync_runs` row holds the reasons.

A run is bounded by `TALLY_REPORTING_SYNC_BUDGET_S`, 45 seconds unless the
deployment sets it. The route holds its response open for the budget and 15
seconds more, where every other route is held to the server's write timeout of
60 seconds, so that a run ends while the connection that asked for it is still
there to be answered. A cloud whose enumeration does not fit the budget
completes no run until the budget is raised
([give a large cloud a longer budget](/how-to/openstack/reconcile-a-cloud#give-a-large-cloud-a-longer-budget)).
The work that budget has to cover does not grow with the outage: the
deleted-servers listing is the one part of a run bounded by how long the cloud
has gone without a completed one, and it is clamped at 24 hours.

## A told instant

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
[The simulated OpenStack world](/explanation/the-simulated-openstack-world)
describes the fake OpenStack API a sync reads it through and the loop that tells
each sync where the month stands.

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
of. The microversion is negotiated, not demanded, so an older nova answers the
listing as it always did and the flavor catalog is read as before.

An image glance names no owner for costs the run its images, in the same way and
for the same reason. The collector books such an image to the project of whoever
registered it, which this run has no way to recover, so the image type stays
incomplete rather than that live row being booked deleted. A deactivated image
is not one of these: it still exists, still occupies the store, and is observed
like any other.

A volume type listing that fails costs no resource type its completeness,
because the names describe no resource. The reason lands in `stats.errors` as
`listing the size names of os-prod-eu1: listing the volume types: ...`. The run
ends `failed` and keeps every correction it made; like every failed run, it
does not move the bound the next run starts from. The names an earlier run
stored stay as they are, and collector events go on being resolved by them. A
credential whose access rules have no rule for `types` is one such case: cinder
answers its type listing 401 on every run.

A failure that says nothing about any single resource type ends the whole run
before a type is observed: an `adapter_config` that does not parse, a
clouds.yaml entry that cannot be resolved, and a Keystone that refuses the
credentials are all of that kind. Reporting them per type would let a sync
conclude that a cloud it never reached holds nothing.

## What a run defers

A run compares the cloud at the instant it looks with the events the collector
delivered up to then. Nova lists an instance in `building` seconds before it
sends `create.end`, and reports a `terminated_at` seconds before `delete.end`,
so a correction booked at that instant books what the collector is about to
book. A run therefore books no correction for a resource the platform reports
as in transition, or for one whose change lies inside the settle window before
the run. The next run books the correction if the difference is still there.

The adapter reports the evidence, and the framework applies the rules. Whether a
resource is in transition, and when the platform last changed it, comes from
these fields:

| Type | In transition | Last changed |
| --- | --- | --- |
| `instance` | any `OS-EXT-STS:task_state`, or `OS-EXT-STS:vm_state` `building` | `updated`, else `created` |
| `instance` from the deleted listing | never | not reported; the deletion carries `terminated_at` |
| `volume` | one of the twelve statuses below | `updated_at`, else `created_at` |
| `floating_ip` | never | `updated_at`, else `created_at` |
| `image` | never | `updated_at`, else `created_at` |
| `loadbalancer` | a `provisioning_status` that starts with `PENDING_` | `updated_at`, else `created_at` |

Nova sets a task state for every operation in flight and clears it when the
operation ends
([`task_states.py`](https://github.com/openstack/nova/blob/ce37978276744e92d91251210a1d9c2784eea375/nova/compute/task_states.py)),
so any task state covers a build, a delete, a resize and every other operation
without a list to maintain. `building` is the `vm_state` of an instance that was
never active
([`vm_states.py`](https://github.com/openstack/nova/blob/ce37978276744e92d91251210a1d9c2784eea375/nova/compute/vm_states.py#L38)).

Cinder holds a volume in `creating`, `deleting`, `managing`, `attaching`,
`detaching`, `reserved`, `restoring-backup`, `backing-up`, `downloading`,
`uploading`, `retyping` or `extending` while an operation on it is in flight.
Its other ten statuses are statuses a volume rests in
([`fields.py`](https://github.com/openstack/cinder/blob/ef0d50cb18d0ceb5d70d500c9a032b0eb66e749b/cinder/objects/fields.py#L168-L190)),
and a status cinder does not define is not a transition. Cinder reports
`updated_at` as null until a volume is first updated, which is why a resource
without an update instant is read at its creation.

Octavia's `PENDING_CREATE`, `PENDING_UPDATE` and `PENDING_DELETE` are a load
balancer it is still changing
([`constants.py`](https://github.com/openstack/octavia/blob/d3a882b734cdcc176301c77e20cac9f7720451b0/octavia/common/constants.py#L167-L175)).
A load balancer in `PENDING_DELETE` is observed for that reason: left out of the
listing, its row would be a live row the run did not name, and the absence pass
would book its delete before octavia sends `delete.end`. A `DELETED` one is not
observed.

The framework decides per correction, once it found the difference:

| Correction | Deferred as `transitional` | Deferred as `recent` |
| --- | --- | --- |
| Create: a live resource the projection holds no live row for | the resource is in transition | the platform changed it inside the window, or the row exists and its newest event is inside it |
| Update: the state, size or project differs | the resource is in transition | the platform changed it inside the window, or the row's newest event is inside it |
| Delete the platform reported, from nova's deleted listing | never | the deletion, or the row's newest event, is inside the window |
| Delete by absence: a live row of an enumerated type the listing did not name | never | the row's newest event is inside the window |

The window is `TALLY_REPORTING_SYNC_SETTLE_S` seconds, 60 by default, before the
instant the run looks. An instant after that one counts as inside it too: a
platform clock ahead of the Reporting API's host, or a told instant behind the
cloud's present, dates a change the run cannot place before itself. A resource
the platform gives no instant for is not deferred as recent. 0 turns the window
off, and a resource in transition is deferred all the same.

The row's newest event counts because a run reads the listing before the
projection. An instance created after nova answered, whose `create.end` the
collector delivered before the run read the rows, is a live row the listing did
not name, and without the rule the absence pass would book a delete for an
instance that exists.

A resource in transition is deferred for as long as it stays there, with no
cap: the collector books nothing before the `.end` notification either. One
correction counts once, as `transitional` when both reasons hold, and a
resource that matches its row is no correction and counts nowhere. What a run
deferred is in `stats.deferred` of its `sync_runs` row and in
`tally_sync_resources_deferred_total{cloud, reason}`. Both count corrections a
run did not book rather than distinct resources, so a resource deferred in five
runs counts five times, and a count that stays above zero for one cloud points
at a resource stuck in transition.

A correction a later run books is dated the way any correction is: a create at
the platform's creation instant, a reported delete at `terminated_at`, an update
and a delete by absence at the poll of the run that books them.

## The instants a missed delete is booked at

Two passes book a delete the collector missed, and they date it differently.

The deleted-servers listing asks nova for `deleted=true` bounded by
`changes-since`, at the start of the last completed run less the settle window,
and every server it returns is booked at the `terminated_at` nova reports. The
window is subtracted because a deletion a run deferred happened before that run
started: without it, the next run's listing would no longer name the deletion,
and the absence pass would date it at poll time. A cloud's first run has
no such bound and asks for no deleted servers, because it has no window behind
it to catch up on. The absence pass books what the observation did not name at
poll time, the instant the sync ran.

That window is clamped at 24 hours. A failed run does not move the bound, so a
cloud that has not completed one in a week would otherwise ask nova for a week
of its churn, inside the same budget the five live listings share, and time
out before it can complete and move the bound, every following run asking for
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

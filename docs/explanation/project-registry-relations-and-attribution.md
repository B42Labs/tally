---
title: Project registry, relations and exclusive attribution
description: "Why projects are first-class entities linked by temporally valid relations, and why every project is billed in exactly one place."
quadrant: explanation
audience: all
---

# Project registry, relations and exclusive attribution

A cloud resource belongs to a project, and a project on one platform often
exists because a project on another one asked for it. The registry is where that
second fact is written down, and attribution is the rule that keeps it from
billing the same resource twice.

## Projects as first-class entities

A project is an entity of its own rather than a label on a metric. Each platform
registers its projects, and a cross-platform dependency between two of them is a
directed relation that carries metadata.

The registry holds the real projects of real platforms. Since Phase 5 it also
holds meta-projects and partners, virtual projects that group real ones and own
no resources themselves (see
[commercial pricing on relations](/explanation/commercial-pricing-on-relations)).

## How projects and relations are registered

A real project is registered through `POST /api/v1/projects`, which is also what
the OpenStack simulator calls when its project-registration switch is on. A
meta-project or a partner is created with `tally-reporting-admin
create-meta-project` and `tally-reporting-admin create-partner`, which write to
the reporting database directly.

A relation is created, patched and closed through the relation endpoints of the
Reporting API, and the point-in-time traversal of the graph is
`GET /api/v1/projects/{id}/related` with its `at` parameter. The endpoints and
their parameters are documented in the
[Reporting API reference](/reference/api/reporting-api).

The concept has the registry filled by an event-driven sync, where a
`shoot.create.end` event registers the shoot's infrastructure tenant and links
it automatically. That sync is a Phase 4 design and is not built
([the providers that do not exist yet](/explanation/providers-that-do-not-exist-yet)).
Today a relation is written by whoever registers the project.

## Temporal validity

A relation carries `valid_from` and `valid_to`, and it is closed rather than
hard-deleted: setting `valid_to` ends it while the history stays readable.
Metering and rating resolve the graph as of the billing period being processed,
not as of now.

A relation applies to a period when its validity overlaps the period, and there
is no intra-period proration of relations (decision D4 of
[the metering and rating roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/03-phase-3-metering-rating.md)).
Deleting a shoot in April therefore does not detach its March infrastructure
costs, and re-processing March in May yields the March graph again.

## Exclusive attribution

Related costs must never double-bill a resource, so each project is billed in
exactly one place. A project that an attributing relation names, an
`infrastructure_tenant` for instance, is excluded from direct billing: its costs
appear only as the related costs of the project that attributes it. The service
price is a management fee on top of the infrastructure, so a Gardener shoot's
`worker_count` price covers the managed service and the worker VMs are billed as
related costs rather than hidden inside the shoot price.

The engine resolves this in one breadth-first walk over the attributing
relations of the period, started from every project no relation attributes away.
The shortest path claims a project, and among paths of equal length the smallest
relation id claims it. Every further path into a project already claimed is
reported as a warning, so the path not taken is visible instead of silently
discarded. A project that sits in a cycle is billed standalone with a warning
rather than failing the run, because a corrupt graph should cost one wrongly
rooted statement rather than a whole month's billing
([`internal/engine/attribution`](https://github.com/B42Labs/tally/blob/main/internal/engine/attribution/attribution.go)).
The registry refuses a relation that would close a cycle over the attributing
relation types in the first place
([`internal/reporting/projects`](https://github.com/B42Labs/tally/blob/main/internal/reporting/projects/projects.go)).

## A Gardener project and its infrastructure tenant

```text
OpenStack Project "team-alpha-os"    (customer's direct VMs)
Gardener Project  "team-alpha"       (managed Kubernetes)
  └─ infrastructure_tenant → OpenStack Project "shoot-abc-123"
  └─ infrastructure_tenant → OpenStack Project "shoot-def-456"

Cross-platform ownership:
  "team-alpha-os" ←same_owner→ "team-alpha"
```

`team-alpha` is billed for its shoots and for the OpenStack tenants those shoots
run on; `team-alpha-os` is billed for its own VMs. The `same_owner` relation
records that the two belong together without attributing anything. The statement
this produces is the last example of
[worked examples](/explanation/worked-examples).

## Resources a service creates for itself

An OpenStack service that builds its product out of other services' resources
owns them in a project of its own. Octavia's amphora provider boots every
amphora as a nova instance with the credentials of `[service_auth]`
([`clients.py`](https://github.com/openstack/octavia/blob/d3a882b734cdcc176301c77e20cac9f7720451b0/octavia/common/clients.py#L52)),
one per load balancer under the topology `SINGLE` and two under
`ACTIVE_STANDBY`
([`config.py`](https://github.com/openstack/octavia/blob/d3a882b734cdcc176301c77e20cac9f7720451b0/octavia/common/config.py#L497-L503)),
and with a volume driver configured it creates one cinder boot volume per
amphora
([`nova_driver.py`](https://github.com/openstack/octavia/blob/d3a882b734cdcc176301c77e20cac9f7720451b0/octavia/compute/drivers/nova_driver.py#L123-L141)).
Kolla scopes those credentials to the project `service`
([`defaults/main.yml`](https://github.com/openstack/kolla-ansible/blob/570f2b77142d7145e55f39126e3987002afe7494/ansible/roles/octavia/defaults/main.yml#L241)).

Nova and cinder notify for an amphora the way they do for any instance and any
volume, and the payload names the service project. The collector books the
resource there
([notification mapping](/reference/formats/notification-mapping)), and a sync
that lists every project's resources books the same
([how reconciliation observes a cloud](/explanation/how-reconciliation-observes-a-cloud)).
One load balancer therefore arrives as a `loadbalancer` in the customer's
project and as instances and volumes in the service project.

Tally has no filter by project or by flavor. Metering records what the cloud
holds, and whether a resource is billed is decided where the prices and the
relations are
([metering separated from rating](/explanation/metering-separated-from-rating)).
A filter in the collector alone would be undone by the next sync, which
observes the instance and books a create for it.

An amphora cannot be billed under the load balancer's project. The registry
relates projects, not resources, and the service project holds the amphorae of
every customer. What a customer pays for a load balancer is the price of the
resource type `loadbalancer`; what the service project's statement shows is
what running them costs the operator.

The operator has two ways to keep that statement out of what is invoiced, and
both are mechanisms that already exist. A `member_of` relation to a
meta-project, carrying a `project_discount` of rate `1`, leaves the line items
on the statement and brings its total to zero. An `infrastructure_tenant`
relation from a project of the operator moves the costs under that project as
related costs. A service project nobody registered is billed standalone and
named in `runs.stats.unregistered_projects`.
[Zero-rate a service project](/how-to/openstack/zero-rate-a-service-project)
has the steps.

## What the registry does not model

Project-to-resource mapping is not modelled here. A resource names its
`project_id` in the events and metrics it produces, and the registry models
project-to-project relations only.

The two tables and their indexes are a contract rather than an argument:
[migration 0001](https://github.com/B42Labs/tally/blob/main/migrations/reporting/0001_init.sql)
is where `projects` and `project_relations` are defined.

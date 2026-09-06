---
title: The providers that do not exist yet
description: The Phase 4 provider designs, kept because they are the argument that the pattern generalises, and the record that none of them is built.
quadrant: explanation
audience: all
---

# The providers that do not exist yet

None of what follows is built. `internal/providers/` holds `openstack` and
nothing else, `internal/collector` does not exist, and Phase 4 has no Meta Issue
behind it. The designs are kept because they are the argument that the provider
pattern generalises: five integrations, three of them platforms and two of them
services, each worked out far enough to be implemented, none of them
implemented.

The plan is
[the Phase 4 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/04-phase-4-additional-providers.md),
which refines the concept below in six decisions.

- D1 extracts the generic collector runtime (outbox, sender, health endpoints
  and metrics) out of the OpenStack collector into `internal/collector` before
  the first new provider, so that five providers do not copy one buffering and
  delivery implementation five times.
- D2 has a polling collector keep its cursor in the same SQLite database as its
  outbox, written in the transaction that inserts the outbox row, so a restart
  neither loses an action nor maps one twice while delivery stays at-least-once.
- D3 marks the STACKIT and IONOS event APIs `VERIFY`: the concept relies on
  audit and activity APIs whose exact shape has to be confirmed against the
  current vendor documentation, and each work package starts with that step.
- D4 makes the Gardener collector a Kubernetes watch on `Shoot` resources
  through Gardener's own typed Go API, with `resourceVersion` as the cursor,
  because Gardener has no message bus to consume and the API server watch is its
  native event source.
- D5 makes the Harbor collector a webhook receiver for pushes and deletes plus a
  periodic storage poll, because Harbor pushes webhooks natively and polling
  alone would miss a short-lived tag.
- D6 requires every provider to ship golden mapping fixtures and to pass the
  `internal/core/testkit` conformance kit before its first deployment.

## Hetzner

Hetzner Cloud manages every resource through one REST API, so the design follows
the provider pattern without a special case. The metrics exporter polls the
[Hetzner Cloud API](https://docs.hetzner.cloud/) for servers, volumes, floating
IPs and load balancers, and exposes them in the Prometheus exposition format.
The event
collector polls the Actions API, which records every resource change, and
forwards what it reads to the Reporting API; the Hetzner action id is the
`event_id`, and the collector persists its polling cursor, so a restart neither
loses actions nor repeats them. The reconciliation adapter lists servers,
volumes, floating IPs and load balancers.

| Resource type | `size` fields | Key events |
| --- | --- | --- |
| `server` | `vcpus`, `ram_gb`, `disk_gb`, `server_type` | create, delete, upgrade, downgrade, power on and off |
| `volume` | `size_gb` | create, delete, resize |
| `floating_ip` | `ip_version`, `type` | create, delete |
| `load_balancer` | `type`, `targets` | create, delete, add and remove target |

## STACKIT

STACKIT offers OpenStack-compatible APIs for compute and block storage and
proprietary APIs for its managed services, and the design splits along that
line. The metrics exporter polls the STACKIT APIs for servers, volumes and
managed services; for the OpenStack-compatible resources the OpenStack database
exporter could be reused. The event collector polls STACKIT's audit and activity
APIs for resource lifecycle events. That is the part decision D3 marks `VERIFY`:
the concept assumed a shape for those APIs that nobody has checked against the
vendor's current documentation.

| Resource type | `size` fields | Key events |
| --- | --- | --- |
| `server` | `vcpus`, `ram_gb`, `disk_gb`, `machine_type` | create, delete, resize, power on and off |
| `volume` | `size_gb`, `type` | create, delete, resize |
| `database` | `type`, `flavor`, `storage_gb` | create, delete, resize |
| `kubernetes_cluster` | `node_count`, `machine_type` | create, delete, scale |

## IONOS

IONOS Cloud manages infrastructure through a REST API and a Terraform provider,
and the design reads the API. The metrics exporter polls the
[IONOS Cloud API](https://api.ionos.com/docs/cloud/v6/) for data centers,
servers, volumes and network resources. The event collector polls the IONOS
request and audit API for lifecycle changes, and carries the same `VERIFY`
marker as STACKIT for the same reason (decision D3).

| Resource type | `size` fields | Key events |
| --- | --- | --- |
| `server` | `cores`, `ram_gb`, `type` | create, delete, resize |
| `volume` | `size_gb`, `type`, `bus` | create, delete, resize |
| `nic` | `lan_id`, `firewall_active` | create, delete |
| `managed_kubernetes` | `node_count`, `cores`, `ram_gb` | create, delete, scale |

## Gardener

Gardener manages Kubernetes clusters across several cloud platforms, which makes
it the design's case for cross-platform relations: a Gardener project owns
shoots whose worker nodes run in an infrastructure tenant on OpenStack, Hetzner,
STACKIT or IONOS, and the cost of that tenant is attributed back to the Gardener
project that caused it.

The exporter exposes the shoots and their worker pools:

```text
gardener_shoot_info{project="...", name="...", kubernetes_version="1.29", infrastructure="openstack"} 1
gardener_shoot_worker_count{project="...", name="...", pool="workers"} 3
gardener_shoot_worker_machine_type{project="...", name="...", pool="workers", type="m1.xlarge"} 1
gardener_shoot_status{project="...", name="...", status="healthy"} 1
```

The collector, a Kubernetes watch on `Shoot` resources per decision D4,
produces the events a shoot's life consists of:

```text
shoot.create.end
shoot.delete.end
shoot.hibernate.start
shoot.hibernate.end
shoot.worker.scale                  -- worker count change, so metering splits
shoot.worker.machine_type_change    -- machine type change, so metering splits
```

Registration is event-driven in the design. When a shoot is created, the
`shoot.create.end` event makes the collector register the infrastructure tenant
as a project on whichever platform the shoot uses:

```text
POST /api/v1/projects
{ "platform": "openstack", "external_id": "shoot-abc-os-tenant",
  "name": "Infrastructure tenant for shoot-abc" }
```

It then creates an `infrastructure_tenant` relation from the Gardener project to
that new infrastructure project:

```text
POST /api/v1/projects/{gardener-project-id}/relations
{ "target_id": "{new-infra-project-id}",
  "relation_type": "infrastructure_tenant",
  "metadata": { "shoot_name": "shoot-abc", "created_by": "gardener-controller" } }
```

On `shoot.delete.end` the relation is closed, with `valid_to` set to the
deletion time. Neither the relation nor the project entry is removed, because a
past billing period has to stay attributable and reproducible.

This is the automatic registration
[project registry, relations and exclusive attribution](/explanation/project-registry-relations-and-attribution)
names as not built. The registry and the relation endpoints exist; the collector
that would call them does not, so today a relation is written by whoever
registers the project.

## Harbor

Harbor is a container registry with project-based access control, and it is the
design's case for counter-based usage: pulls, pushes and traffic are counted per
billing period beside the time-based quantities everything else is metered in.

The exporter exposes the repositories, their storage and the project counters:

```text
harbor_repository_info{project="...", name="app", tags="47"} 1
harbor_repository_storage_bytes{project="...", name="app"} 13421772800
harbor_project_quota_storage_bytes{project="..."} 107374182400
harbor_project_pull_total{project="..."} 1523
harbor_project_push_total{project="..."} 70
```

The collector, a webhook receiver with a storage poll beside it per decision D5,
produces the registry's events, of which two split a metering interval:

```text
repository.push                     -- storage_gb may change, so metering splits
repository.delete                   -- storage_gb may change, so metering splits
repository.pull                     -- counter metric, aggregated per period
project.create / project.delete     -- project lifecycle
```

A Harbor project is registered like any other platform's project:

```text
POST /api/v1/projects
{ "platform": "harbor", "external_id": "team-alpha",
  "name": "Team Alpha container registry" }
```

Where a Harbor project stores the images a Gardener shoot runs, an optional
`image_source` relation records that:

```text
POST /api/v1/projects/{harbor-project-id}/relations
{ "target_id": "{gardener-project-id}",
  "relation_type": "image_source",
  "metadata": { "description": "Provides container images for shoot workloads" } }
```

## What exists of all this today

The engine already bills three of these platforms, from events a test seeds
rather than from events a collector produced: the Hetzner server upgrade, the
Gardener shoot that scales and hibernates, and the Harbor repository with its
counters are golden cases with exact expected numbers (see
[worked examples](/explanation/worked-examples)). The core is not waiting on a
design decision here. It is waiting on the collectors.

Until a provider exists, how-to guides and reference pages for it stay out of
scope for this site, per
[the documentation Meta Issue](https://github.com/B42Labs/tally/issues/104).

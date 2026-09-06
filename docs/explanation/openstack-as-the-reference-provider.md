---
title: OpenStack as the reference provider
description: How the one provider that exists feeds the shared core, and why each of its four parts is built the way it is.
quadrant: explanation
audience: all
---

# OpenStack as the reference provider

OpenStack is the one provider Tally has, and it is the reference for the
provider pattern: what it puts on its side of the two integration points is what
a second provider copies. Four parts make it up: an event collector, a
reconciliation adapter, a Ceilometer metrics pipeline and a database exporter.
The document that specified all four is
[the Phase 1 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/01-phase-1-core-platform-openstack.md),
where they are goal item 3.

## Metrics

Ceilometer's polling agent collects the runtime metrics: CPU utilisation and
vCPU time, RAM usage, disk I/O and disk size, network I/O in both directions,
and instance uptime. It polls every 300 seconds, a trade-off between how
precisely a sample places a change in time and the load the polling puts on the
cloud.

From there the samples take one of two paths into VictoriaMetrics. On the push
path a producer speaks OTLP to the OpenTelemetry Collector, which accepts OTLP
over gRPC and over HTTP and writes the points on to VictoriaMetrics with its
`prometheusremotewrite` exporter; Ceilometer's own OTLP publisher posts on that
path. On the pull path VictoriaMetrics scrapes the exporters: the OpenStack
database exporter every 300 seconds, and the Ceilometer exporter every 60
seconds where a deployment has Ceilometer publish to it instead of pushing OTLP
([the scrape configuration](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/victoriametrics/scrape.yaml)).

What makes a sample on either path usable is the label convention it carries.
That convention belongs to the shared core rather than to any provider, and the
exporters and publishers on the OpenStack side are what put it on the data (see
[architecture and the provider pattern](/explanation/architecture-and-the-provider-pattern)).

## Events

Ceilometer's notification pipeline publishes its own sample and notification
format and cannot emit the Tally event schema, so the lifecycle events come from
a collector of Tally's own,
[`tally-openstack-collector`](https://github.com/B42Labs/tally/blob/main/cmd/tally-openstack-collector),
built on
[`internal/providers/openstack`](https://github.com/B42Labs/tally/blob/main/internal/providers/openstack).

The collector consumes oslo.messaging notifications over AMQP, from the same bus
Ceilometer listens on, and maps each one to a canonical event with a normalised
`payload.state` and `payload.size` (see
[events as the source of truth](/explanation/events-as-the-source-of-truth)).
That mapping is the whole of the provider-specific work: nova's `vm_state`
vocabulary and the units OpenStack reports its quantities in stop at the
collector, and the core stores the canonical form. What comes out is buffered
in a SQLite outbox and posted to the Reporting API from a loop of its own, at
least once, with the oslo.messaging `message_id` as the `event_id`, so a
redelivered notification and a retried batch both deduplicate on arrival.

These are the notifications it consumes:

```text
compute.instance.create.end / delete.end / resize.end
compute.instance.finish_resize.end
compute.instance.shelve_offload.end / unshelve.end
compute.instance.power_on.end / power_off.end
volume.create.end / volume.delete.end / volume.resize.end / volume.retype
volume.transfer.accept.end
floatingip.create.end / floatingip.delete.end
image.upload / image.create / image.delete
octavia.loadbalancer.create.end / update.end / delete.end
```

The event type each of them becomes, and the size fields the mapping reads out
of its payload, are documented in the reference quadrant.

## The database exporter

A standalone service reads the OpenStack service databases through a read-only
user and exposes what it finds in the Prometheus exposition format. What it
carries is the inventory and the state Ceilometer does not measure: what exists,
in which project, in which state, and against which quota. It runs on the
control plane beside the OpenStack services, which is where those databases are
reachable.

The decision is to run VEXXHOST's
[openstack_database_exporter](https://github.com/vexxhost/openstack_database_exporter)
unchanged and to write no supplementary exporter beside it. Tally therefore
ships no exporter of its own for OpenStack; what it ships is the scrape job that
reads this one.

## The reconciliation adapter

The adapter that closes the gap between the event history and the cloud lives
under
[`internal/reporting/reconciliation/adapters`](https://github.com/B42Labs/tally/blob/main/internal/reporting/reconciliation/adapters/),
inside the Reporting API rather than in the collector: the framework around it
does everything a sync does apart from observing the platform, so the adapter is
the whole of what OpenStack contributes to reconciliation.

A run enumerates instances through nova, volumes through cinder, floating IPs
through neutron and images through glance, and load balancers through octavia
where the cloud's configuration asks for it. It also asks nova for the servers
it destroyed inside the window the run is responsible for, so a delete the
collector lost becomes a synthetic delete carrying the real deletion time rather
than the time the sync happened to look (see
[dual ingestion and reconciliation](/explanation/dual-ingestion-and-reconciliation)).

The repository ships no CronJob for the sync. What drives the schedule belongs
to the deployment, which is also what decides how much overbilling a lost delete
can cost.

## How the pieces fit together

```text
┌────────────────────────────────────────────────────────────────────────────┐
│                          OpenStack control plane                           │
│                                                                            │
│  ┌──────────────────────────────┐         ┌─────────────────────────────┐  │
│  │ OpenStack services           │         │ OpenStack database exporter │  │
│  │ (nova, neutron, cinder, ...) │         │ (read-only access to the    │  │
│  │                              │◀────────┤  nova/neutron/cinder/       │  │
│  │  APIs · DBs · oslo.messaging │         │  keystone/... databases)    │  │
│  └──────┬───────────────┬───────┘         └──────────────┬──────────────┘  │
│         │ polling       │ notifications                  │                 │
│         ▼               ▼                                │                 │
│  ┌───────────────┐  ┌─────────────────────────┐          │                 │
│  │ Ceilometer    │  │ tally-openstack-        │          │                 │
│  │ polling agent │  │ collector               │          │                 │
│  │ (metrics only)│  │ (oslo notifications →   │          │                 │
│  │               │  │  Tally event schema,    │          │                 │
│  │               │  │  buffered delivery)     │          │                 │
│  └──────┬────────┘  └───────────┬─────────────┘          │                 │
│         │                       │                        │                 │
└─────────┼───────────────────────┼────────────────────────┼─────────────────┘
          │ OTLP                  │ POST /api/v1/events    │ /metrics
          │                       │                        └──▶ scraped by
          ▼                       ▼                             VictoriaMetrics
  ┌────────────────┐    ┌──────────────────────┐
  │ OTel Collector │    │ tally-reporting      │───▶ reconciliation polls
  │ (OTLP in,      │    │ (event store,        │     the OpenStack APIs,
  │  remote write  │    │  reconciliation,     │◀─── writes synthetic events
  │  out)          │    │  project registry)   │
  └───────┬────────┘    └──────────┬───────────┘
          │ remote write           │
          ▼                        ▼
  ┌──────────────────┐  ┌──────────────────────┐
  │ VictoriaMetrics  │  │ PostgreSQL +         │
  │ (also scrapes    │  │ TimescaleDB          │
  │  the database    │  │ (events,             │
  │  exporter and    │  │  current_resources,  │
  │  tally-reporting │  │  projects)           │
  │  /metrics)       │  │                      │
  └────────┬─────────┘  └──────────┬───────────┘
           │ counter metrics       │ read-only,
           │ (MetricsQL)           │ tally_engine_reader
           ▼                       ▼
  ┌─────────────────────────────────────────────┐
  │                 tally-engine                │
  └─────────────────────────────────────────────┘
```

The engine sits under both columns and reaches neither through the Reporting
API: it queries VictoriaMetrics for the counter metrics and reads the reporting
database directly, over its own read-only role.

## The simulator

[`tally-openstack-simulator`](https://github.com/B42Labs/tally/blob/main/cmd/tally-openstack-simulator)
is how the four parts above are exercised without a cloud. A `run` generates one
month from a seed and publishes it as oslo.messaging notifications onto a
broker, at a configurable number of virtual seconds per wall second; with
`--out` it writes the month, its events and its oracle to files as well, and
with no broker configured that is all it does. While the month goes out, the run
serves a fake OpenStack API that answers the listings the reconciliation adapter
enumerates, pushes the month's traffic counters and inventory gauges over OTLP,
and with `--register-projects` registers the month's tenants, its two Gardener
projects and their `infrastructure_tenant` relations with the Reporting API
first. A `replay` publishes a notification file an
earlier run wrote, which puts the same month on a bus again without the
generator behind it. A `compare` reads that oracle, an engine export of the
month and the pricing model it was rated with, and lists every resource whose
metered intervals or quantities differ; it exits non-zero when anything does.

The collector consumes those notifications unmodified. The simulator sits on the
producing side of the bus and nothing on the consuming side knows it is there,
so a simulated month tests the collector rather than a test double.

That is also why it is the only data source of the tutorials: every lesson is
reproducible from a seed and needs no cloud account (decision D3 of
[the documentation Meta Issue](https://github.com/B42Labs/tally/issues/104)).

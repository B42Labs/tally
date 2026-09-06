---
title: Architecture and the provider pattern
description: Why the core is shared and every cloud contributes a thin adapter, and what runs today under that pattern.
quadrant: explanation
audience: all
---

# Architecture and the provider pattern

Tally is one core with a thin adapter per cloud. The core holds the data model,
the event store, the metrics store and the metering and rating engine, and none
of it knows which platform an event came from. A platform reaches it through two
integration points and two rules, and everything platform-specific lives on the
platform's side of them.

## The system at a glance

```text
┌─────────────────────────────────────────────────────────────────────────────┐
│                          Cloud platform (provider)                          │
│                                                                             │
│  ┌──────────────────┐     ┌──────────────────┐     ┌──────────────────┐     │
│  │ Metrics exporter │     │ OTLP publisher   │     │ Event collector  │     │
│  │                  │     │                  │     │                  │     │
│  │ exposes /metrics │     │ pushes runtime   │     │ posts lifecycle  │     │
│  │ (Prometheus fmt) │     │ metrics          │     │ events over HTTP │     │
│  │                  │     │                  │     │ (buffered,       │     │
│  │                  │     │                  │     │  at-least-once)  │     │
│  └─────────┬────────┘     └─────────┬────────┘     └─────────┬────────┘     │
│            │                        │                        │              │
└────────────┼────────────────────────┼────────────────────────┼──────────────┘
             │ /metrics (scrape)      │ OTLP push              │ POST /api/v1/events
             │                        ▼                        │
             │              ┌────────────────────┐             │
             │              │ OTel Collector     │             │
             │              │ remote write       │             │
             │              └─────────┬──────────┘             │
             │   ┌────────────────────┘                        │
             ▼   ▼                                             ▼
   ┌────────────────────┐                           ┌──────────────────────┐
   │ VictoriaMetrics    │                           │ tally-reporting      │
   │                    │                           │                      │
   │ central metrics    │◀──────────────────────────┤ event store,         │──▶ reconciliation
   │ store              │   scrape /metrics         │ project registry,    │    polls platform
   │                    │                           │ /metrics             │◀── APIs, writes
   └──────────┬─────────┘                           └──────────┬───────────┘    synthetic events
              │                                                │
              │                                                ▼
              │                                     ┌──────────────────────┐
              │                                     │ PostgreSQL +         │
              │                                     │ TimescaleDB          │
              │                                     │ events = source      │
              │                                     │ of truth,            │
              │                                     │ current_resources,   │
              │                                     │ projects             │
              │                                     └──────────┬───────────┘
              │ counter metrics                                │ read-only,
              │ (MetricsQL)                                    │ tally_engine_reader
              ▼                                                ▼
   ┌───────────────────────────────────────────────────────────────────────┐
   │                              tally-engine                             │
   │                                                                       │
   │  1. Metering: usage in neutral units                                  │
   │  2. Rating: apply the versioned pricing model                         │
   │  rated usage records, exports, statements                             │
   └───────────────────────────────────────────────────────────────────────┘
```

Two arrows in that picture are worth reading closely. Nothing flows from the
API into the metering engine: `tally-engine` reads the reporting database
itself, over its own connection, through the read-only role
`tally_engine_reader`. That is decision D1 of
[the metering and rating roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/03-phase-3-metering-rating.md),
and the read seam is
[`internal/engine/source`](https://github.com/B42Labs/tally/blob/main/internal/engine/source/source.go):
metering scans whole event histories, and paging those over HTTP would buy
nothing. The API stays the write path, and the event schema is the contract
between the two. The second arrow into the engine leaves VictoriaMetrics: a
counter metric is read back with a MetricsQL query rather than derived from the
event history.

The metrics path has two halves. A platform that exposes a Prometheus endpoint
is scraped by VictoriaMetrics directly. A platform that pushes instead sends
OTLP to the OTel Collector, which writes the points on to VictoriaMetrics.

## The two integration points

Each cloud platform implements two integration points.

| Component | Responsibility | Output |
| --- | --- | --- |
| Metrics exporter | Exposes inventory and runtime metrics in the Prometheus exposition format | a `/metrics` endpoint that VictoriaMetrics scrapes |
| Event collector | Sends lifecycle events (create, delete, resize, state change) to the Reporting API over HTTP | `POST /api/v1/events` |

Beside those two, each provider:

- registers its projects in the project registry
- declares its resource types and the JSON Schema of their `size` object
- defines its pricing model
- optionally supplies a reconciliation adapter, so the Reporting API can poll
  the platform's own API

## The two rules every provider keeps

The first rule is that `cloud` appears in every key and every join. `platform`
names the platform type and `cloud` names one concrete installation of it, so
two OpenStack clouds share `platform="openstack"` and differ in `cloud`. A
resource identifier and a project identifier are unique only within
`(cloud, resource_type)`, which is why a key, a join, a lock or a cache that
drops `cloud` collides across installations. Every metric carries the same
dimension, so a query filters by installation without joining anything.

The second rule is that `event_id` is the idempotency key. A provider uses the
platform's native event or action identifier where one exists, an oslo.messaging
`message_id` for example; where none exists it derives a deterministic hash of
the cloud, the resource, the event type and the timestamp. Ingestion
deduplicates on it, so a collector that replays a batch changes nothing.

The label convention and the event schema are contracts rather than arguments,
so they are not reproduced here. The normative text is sections 3 and 4 of
[the roadmap conventions](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md);
the reference quadrant is where that contract belongs on this site.

## What a provider supplies

- An ingest credential scoped to the `(platform, cloud)` pair.
- A metrics exporter in the Prometheus exposition format, carrying the label
  convention.
- An event collector that posts lifecycle events, buffers locally and retries.
- A reconciliation adapter that lists the platform's resources for drift
  detection.
- Its projects, and the relations from them to the infrastructure projects they
  depend on.
- Its resource types, each with a JSON Schema for its `size` object.
- Pricing entries for those resource types.
- A scrape target for VictoriaMetrics.

## Who may write what

Three kinds of bearer token reach the Reporting API, drawn from three separate
stores. A token issued for one class is refused by the operations of the other
two.

An ingest credential is issued per `(platform, cloud)` pair and may report
events for that pair alone, so a compromised collector cannot touch another
cloud's billing data. An API token carries one of three roles, `admin`,
`read_all` or `project`, and each read operation names the role it needs; a
token below it is answered 403. The internal routes, `POST /internal/sync/{cloud}`
and `POST /internal/projection/rebuild`, take a shared internal token that the
other Tally components hold.

Every token is a bearer token on the wire. mTLS is not built, and neither is
OIDC. That is decisions D3 and D4 of
[the Phase 1 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/01-phase-1-core-platform-openstack.md):
the RBAC semantics are fixed now and the identity provider integration is
decoupled from them. The `Authenticator` interface of
[`internal/reporting/auth`](https://github.com/B42Labs/tally/blob/main/internal/reporting/auth/middleware.go)
is the seam an OIDC provider slots into; the only implementation today is the
static lookup against the `api_tokens` table.

An event whose `platform` or `cloud` contradicts the credential that submitted
it is refused and written to the audit log. It is not dead-lettered: a scope
violation is a security event, not schema drift, and the two belong in different
places (decision D5).

The clouds a deployment knows are configured in a YAML file rather than in
database rows, so the reconciliation credentials live next to the deployment
configuration instead of in the API's own database (decision D6). The roadmap
names the variable `TALLY_CLOUDS_CONFIG`; the built one is
`TALLY_REPORTING_CLOUDS_CONFIG`, in
[`internal/reporting/config`](https://github.com/B42Labs/tally/blob/main/internal/reporting/config/config.go).
Leaving it unset is valid and means no cloud is configured, so every sync
answers 404.

Every write operation, over events, projects, relations and resource types, is
audit-logged with the identity of the credential that made it.

## Language and API contract

Tally is written in Go and each service ships as a single static binary, which
keeps the operational footprint small and puts the code in the same ecosystem as
Prometheus, OpenStack and Kubernetes tooling. The REST API is specified
contract-first as an OpenAPI document, and the server stubs and types are
generated from it.

## Why VictoriaMetrics

VictoriaMetrics is the central store for the runtime and inventory data of every
platform. It runs as a single node or as a cluster, depending on the scale the
deployment needs, and the single-binary form keeps the operational overhead low.
What it buys over Prometheus:

- around ten times better compression
- long-term storage in the product itself, so no Thanos and no Cortex
- better behaviour under high cardinality
- full PromQL compatibility plus the MetricsQL extensions
- multi-tenancy, an enterprise feature with a license cost that the initial
  scope does not need

The scrape target list grows with each provider.

| Target | Interval | Metrics |
| --- | --- | --- |
| Provider metrics exporters | 60s | inventory data, quotas, resource information |
| `tally-reporting` `/metrics` | 30s | event counts, derived metrics |
| OTel Collector | 15s | runtime metrics from platforms that push |

Metrics retention is thirteen months, which covers a year-over-year comparison
and every billing period inside it. The value is set by `-retentionPeriod=13` in
[the VictoriaMetrics manifest](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/victoriametrics/victoriametrics.yaml).

## The six binaries

Every service lives under `cmd/`.

- `tally-reporting` serves the Reporting API. It reads its configuration from
  the environment, refuses a configuration it cannot honor, and serves the
  routes of the OpenAPI contract. Its database pool connects lazily, so the
  process comes up while TimescaleDB is unavailable and reports that through its
  probes.
- `tally-reporting-admin` provisions and revokes the API's credentials and
  registers its virtual projects, working on the reporting database directly. It
  is also the only thing that runs DDL on that database.
- `tally-engine` is the metering engine's operator tool and its scheduler
  entrypoint: `run` meters and rates a period, `finalize` closes it,
  `detect-late` and `correct` deal with what arrived afterwards, `export` writes
  the result out, and `tick` runs the same tree unattended.
- `tally-openstack-collector` consumes oslo.messaging notifications from AMQP,
  maps them to Tally events, buffers them in a SQLite outbox and posts them to
  the Reporting API from a loop of its own.
- `tally-openstack-simulator` publishes one simulated month of OpenStack
  notifications onto a broker, or writes that month and its oracle to files, and
  compares an engine export against the oracle.
- `tally-vertical-slice` rates one project's instance usage for one calendar
  month and prints it as JSON. It is a throwaway prototype that proves the
  billing chain on numbers a reader can check by hand, and the metering engine
  replaces it.

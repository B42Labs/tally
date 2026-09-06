---
title: Goals and design principles
description: What Tally is for and the eleven principles the design follows, each linked to the page that argues it.
quadrant: explanation
audience: all
---

# Goals and design principles

Tally does reporting, metering and rating for cloud platforms. It collects
events, metrics and inventory data from a cloud, records the usage it finds in
neutral units, rates that usage with a versioned pricing model and exports the
result as statements. A billing system, a cost dashboard, a capacity plan or a
chargeback workflow reads those statements. Every platform reaches the system
through a provider pattern: the platform contributes a thin integration layer,
and the core behind it stays shared and platform-independent. OpenStack is the
one provider that exists, and the pattern is built for more
([the providers that do not exist yet](/explanation/providers-that-do-not-exist-yet)).

## Goals

- Collect runtime data from cloud resources centrally, whatever platform the
  resources live on.
- Record the lifecycle events of cloud resources without gaps.
- Export inventory data from platform-specific sources, a database or an API.
- Meter usage in neutral, platform-independent units: minutes, counts and
  sizes.
- Rate metered usage by applying a configurable pricing model.
- Stay extensible for further cloud platforms and services.

## Design principles

- Platform-agnostic data model: one schema for metrics and one for events, the
  same across every platform and service
  ([architecture and the provider pattern](/explanation/architecture-and-the-provider-pattern)).
- Provider pattern: each platform registers with its resource types, its
  exporters and its event collectors
  ([architecture and the provider pattern](/explanation/architecture-and-the-provider-pattern)).
- VictoriaMetrics as metrics store: every metric goes to VictoriaMetrics, which
  answers PromQL and MetricsQL and takes remote write
  ([architecture and the provider pattern](/explanation/architecture-and-the-provider-pattern)).
- Project as a first-class entity: a project is registered with the platform it
  belongs to, and a cross-platform dependency is a directed relation carrying
  metadata
  ([project registry, relations and exclusive attribution](/explanation/project-registry-relations-and-attribution)).
- Dual ingestion with reconciliation: events carry the real-time picture, and a
  periodic sync against the platform API catches what the events missed
  ([dual ingestion and reconciliation](/explanation/dual-ingestion-and-reconciliation)).
- Events as the single source of truth: the append-only event history is
  authoritative and `current_resources` is a derived projection a replay
  rebuilds at any time
  ([events as the source of truth](/explanation/events-as-the-source-of-truth)).
- Billing-grade ingestion: delivery is at-least-once, ingestion is idempotent
  and deduplicates on `event_id`, and a late or out-of-order event is folded in
  by projection replay
  ([events as the source of truth](/explanation/events-as-the-source-of-truth)).
- Cloud instance dimension: `platform` names the platform type and `cloud` names
  the concrete installation, so two OpenStack clouds are two first-class clouds
  ([architecture and the provider pattern](/explanation/architecture-and-the-provider-pattern)).
- Metering separated from rating: usage is recorded in neutral units first, and
  pricing is applied as a separate step
  ([metering separated from rating](/explanation/metering-separated-from-rating)).
- Reproducible billing: pricing models are versioned, project relations are
  temporally valid, a finalized billing period is immutable and a correction is
  a delta against it, so re-processing a past period always yields the same
  result
  ([billing period lifecycle and corrections](/explanation/billing-period-lifecycle-and-corrections)).
- Decimal money arithmetic: monetary values are computed in decimal types under
  defined rounding rules, never in binary floats
  ([money and rounding](/explanation/money-and-rounding)).

---
title: Roadmap
description: What each phase covers, what is built, and where the record the code was generated from lives.
quadrant: explanation
audience: all
---

# Roadmap

The work was planned in five phases, one document each, written so that a
code-generation model could implement a phase work package by work package
without re-deriving its design decisions. The phases depend on each other like
this:

```text
Phase 1 ──▶ Phase 2 (dashboards need Phase-1 metrics & endpoints)
   │
   └──────▶ Phase 3 (engine needs Phase-1 events, projections, project graph)
                │
                ├──▶ Phase 4 (new providers plug into the Phase-1 core;
                │             their pricing needs the Phase-3 engine)
                │
                └──▶ Phase 5 (pricing adjustments extend the Phase-3 rating engine
                              and the Phase-1 project registry)
```

Phase 2 and Phase 3 can proceed in parallel after Phase 1. Phase 4 providers can
be onboarded individually and independently of each other. Phase 5 requires
Phase 3.

## Phase 1: core platform and OpenStack provider

Phase 1 is the foundation everything else stands on: the Reporting API with
idempotent event ingestion, the `current_resources` projection and its replay,
the query endpoints, ingest validation with a dead letter, and the
provider-agnostic reconciliation framework, all authenticated per
`(platform, cloud)` and audit-logged. Beside it come the resource type registry
that validates a `size` object against a registered JSON Schema, the project
registry with its temporally valid relations, and the metrics deployment of OTel
Collector and VictoriaMetrics. The OpenStack provider is the phase's other half:
collector, reconciliation adapter, Ceilometer pipeline and database exporter. A
throwaway vertical slice took one resource type from event to rated record
before the Phase 3 engine existed.

The phase is built. Its document is
[the Phase 1 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/01-phase-1-core-platform-openstack.md)
and its Meta Issue is
[#1](https://github.com/B42Labs/tally/issues/1).

## Phase 2: reporting and dashboards

Phase 2 made the collected data visible and operable: aggregation endpoints on
the Reporting API, Grafana dashboards over VictoriaMetrics provisioned as code,
and alerting on the failure modes the concept calls billing-critical (a
collector outage, schema drift and reconciliation drift). The alerting stack is
vmalert with Alertmanager, because this architecture has no Prometheus server.

The phase is built. Its document is
[the Phase 2 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/02-phase-2-reporting-dashboards.md)
and its Meta Issue is
[#26](https://github.com/B42Labs/tally/issues/26).

## Phase 3: metering and rating

Phase 3 built the metering and rating engine: usage records per resource per
calendar month in neutral units, split on every change of size, state or
project; the versioned pricing model applied to them in decimal arithmetic with
normative rounding; the billing period lifecycle from open through grace to
finalized and corrected, with immutable finalized runs and delta-based
correction runs; and the attribution of related costs across the project graph
without double billing. The concept's worked examples became the engine's golden
test suite, with exact expected numbers and no tolerance.

The phase is built, and
[an acceptance drill](https://github.com/B42Labs/tally/issues/51) took a full
dev-stack month through metering, rating, finalization and export. The concept's
"Integration" row, which asks for a connection to an external billing or ERP
system, is built up to the `BillingExporter` seam:
[`internal/engine/export`](https://github.com/B42Labs/tally/blob/main/internal/engine/export/export.go)
reads one run out of the engine database and hands it to that interface, the
JSON and CSV file writers are its implementations, and no ERP adapter exists.
Its document is
[the Phase 3 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/03-phase-3-metering-rating.md)
and its Meta Issue is
[#33](https://github.com/B42Labs/tally/issues/33).

## Phase 4: additional providers and services

Phase 4 onboards Hetzner, STACKIT and IONOS as platform providers and Gardener
and Harbor as service integrations, each with a metrics exporter, an event
collector and a reconciliation adapter, and each independent of the others. It
also extracts the shared collector runtime the five would otherwise copy, and
brings the new providers into the OTel Collector deployment.

The phase is not started and has no Meta Issue. Its document is
[the Phase 4 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/04-phase-4-additional-providers.md),
and what it designs is described in
[the providers that do not exist yet](/explanation/providers-that-do-not-exist-yet).

## Phase 5: commercial pricing and partner models

Phase 5 put the commercial layer on top of the rating engine: meta-projects that
group projects under a customer through `member_of`, reseller relations through
`managed_by`, pricing adjustments resolved from relation metadata as surcharges,
discounts, project discounts and kickbacks, kickback reporting aggregated per
partner and billing period, and volume and loyalty discounts carried on group
membership. It needed no schema change, only new `platform` values, additive
relation types and the relation metadata column.

The phase is built, and
[a golden gate](https://github.com/B42Labs/tally/issues/59) proves it end to end
against the phase's own golden cases. Its document is
[the Phase 5 roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/05-phase-5-commercial-pricing.md)
and its Meta Issue is
[#53](https://github.com/B42Labs/tally/issues/53). What it built is described in
[commercial pricing on relations](/explanation/commercial-pricing-on-relations).

The five phase documents and the conventions beside them are the record the code
was generated from. They stay in `roadmap/` and are not restructured beyond the
status line a work package gets when it is implemented: that is decision D2 of
[the documentation Meta Issue](https://github.com/B42Labs/tally/issues/104), and
the rule at the end of
[the roadmap index](https://github.com/B42Labs/tally/blob/main/roadmap/README.md)
says the same thing. This site is where the concept now lives, and it links back
rather than copying: every explanation page names the decision that shaped what
it describes, so the argument and the record stay one click apart.

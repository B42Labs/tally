# Tally Implementation Roadmap

This directory turns the phased roadmap from the [concept document](https://b42labs.github.io/tally/explanation/roadmap)
into **implementation-ready phase documents**. Each document is written so that a code-generation
model (or a human developer) can implement the phase work-package by work-package without having
to re-derive design decisions.

## Documents

| File | Content |
|------|---------|
| [00-conventions.md](00-conventions.md) | **Binding** cross-phase conventions: technology stack, repository layout, coding standards, shared contracts (event schema, payload envelope, error format, money rules). Read this first — every phase document assumes it. |
| [01-phase-1-core-platform-openstack.md](01-phase-1-core-platform-openstack.md) | Phase 1 – Core platform (Reporting API, auth, resource-type registry, project registry, reconciliation framework) + OpenStack provider (event collector, reconciliation adapter, metrics pipeline) + vertical slice |
| [02-phase-2-reporting-dashboards.md](02-phase-2-reporting-dashboards.md) | Phase 2 – Reporting API extensions, Grafana dashboards, alerting |
| [03-phase-3-metering-rating.md](03-phase-3-metering-rating.md) | Phase 3 – Metering & Rating Engine, billing period lifecycle, pricing model, corrections, ERP export |
| [04-phase-4-additional-providers.md](04-phase-4-additional-providers.md) | Phase 4 – Hetzner, STACKIT, IONOS providers; Gardener and Harbor service integrations |
| [05-phase-5-commercial-pricing.md](05-phase-5-commercial-pricing.md) | Phase 5 – Meta-projects, reseller relations, relation-based pricing adjustments, kickback reporting |

## How to use these documents with a code-generation model

1. **Load context**: give the model `00-conventions.md` plus the phase document you are working
   on. The explanation quadrant of the documentation site (`https://b42labs.github.io/tally/explanation/`) is useful background but the phase documents are self-contained
   for their scope; where they refine or deviate from the concept, they say so explicitly in a
   "Decisions made by this document" section.
2. **Work package by work package**: each phase is broken into numbered work packages (WP) with
   explicit dependencies. Implement them in the listed order. Each WP defines:
   - the files/modules to create,
   - the exact contracts (schemas, endpoints, SQL, algorithms as pseudocode),
   - acceptance criteria and the tests that must pass.
3. **Tests are the contract**: worked examples from the concept are reproduced here as *golden
   tests* with exact expected values. A WP is done when its acceptance criteria and tests pass —
   not before, and "roughly matching" numbers are a failure.
4. **Do not improvise on the guardrails**: the guardrails in `00-conventions.md`
   (decimal money, append-only events, `cloud` in every key, half-open intervals, …) are
   invariants of the whole system. When a WP seems to conflict with a guardrail, the guardrail
   wins and the conflict must be surfaced, not silently resolved.

## Phase dependency graph

```
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

Phase 2 and Phase 3 can proceed in parallel after Phase 1. Phase 4 providers can be onboarded
individually and independently of each other. Phase 5 requires Phase 3.

## Status tracking

Keep a short status line per work package directly in the phase documents when implementation
starts (e.g. `> STATUS: done 2026-08-01, PR #12`). Do not restructure the documents otherwise —
they are the reference the code was generated from.

---
title: Explanation
description: Why Tally is built the way it is, with the alternatives that were weighed.
quadrant: explanation
audience: all
---

# Explanation

An explanation page is a discussion. It is written for understanding rather
than for use: it gives the reasoning behind a design, the alternatives that
were weighed against it and the reason one of them was chosen. It argues, and
it says what the choice costs.

An explanation page never states a bare contract. If you need the exact fields
of a surface, [Reference](/reference/) has them; an explanation tells you why
they have that shape.

[How this documentation is organised](/explanation/how-this-documentation-is-organised)
applies the same standard to the documentation itself.

## Reading order

The pages build on each other in this order.

- [Goals and design principles](/explanation/goals-and-design-principles) says
  what Tally is for and lists the eleven principles the pages below argue.
- [Architecture and the provider pattern](/explanation/architecture-and-the-provider-pattern)
  argues why the core is shared and every cloud contributes a thin adapter.
- [Events as the source of truth](/explanation/events-as-the-source-of-truth)
  argues why the append-only event history is authoritative and every other
  view is derived from it.
- [Dual ingestion and reconciliation](/explanation/dual-ingestion-and-reconciliation)
  explains why events and a periodic sync are both needed, and names the two
  gaps the sync accepts.
- [Metering separated from rating](/explanation/metering-separated-from-rating)
  argues why usage is recorded in neutral units before any price is applied.
- [Money and rounding](/explanation/money-and-rounding) argues why money is
  decimal end to end, rounded once per dimension and never summed across
  currencies.
- [Billing period lifecycle and corrections](/explanation/billing-period-lifecycle-and-corrections)
  explains why a finalized period is never edited and a late event becomes a
  credit note instead.
- [Project registry, relations and exclusive attribution](/explanation/project-registry-relations-and-attribution)
  argues why projects are first-class entities linked by temporally valid
  relations, and why every project is billed in exactly one place.
- [Commercial pricing on relations](/explanation/commercial-pricing-on-relations)
  argues why discounts, surcharges and kickbacks are metadata on those
  relations rather than a pricing subsystem of their own.
- [Worked examples](/explanation/worked-examples) walks the concept's worked
  examples and names the golden case each one seeded.
- [OpenStack as the reference provider](/explanation/openstack-as-the-reference-provider)
  shows how the one provider that exists feeds the shared core, and why each of
  its four parts has the shape it has.
- [The providers that do not exist yet](/explanation/providers-that-do-not-exist-yet)
  keeps the unbuilt provider designs as the argument that the pattern
  generalises, and records that none of them is built.
- [Roadmap](/explanation/roadmap) says what each phase covers and what is
  built.

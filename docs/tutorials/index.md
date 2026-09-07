---
title: Tutorials
description: Lessons that take you by the hand from an empty machine to a rated month, and on through finalization, corrections, commercial pricing and attribution.
quadrant: tutorial
audience: all
---

# Tutorials

A tutorial is a lesson. It takes you by the hand through a task you have not
done before, and it is written for someone who is still learning what the
pieces are called.

## What a tutorial promises

You will succeed. Every command is given in full, every version is pinned, and
the lesson is run from an empty machine before it is published.

You do not have to decide anything. Where a real deployment has a choice, the
lesson makes one for you and moves on. The trade-offs behind that choice belong
in [Explanation](/explanation/), and the other options belong in
[How-to guides](/how-to/). Every lesson runs against the local dev stack with
the simulator as its only data source, so the output a lesson shows is the
output of a real run and matches yours.

You end with a working mental model. After the last step you have a running
system you built yourself, and a name for each part of it.

## The learning path

### Core track

1. [Set up your local Tally](/tutorials/set-up-your-local-tally): clone the
   repository, bring the kind cluster with the dev overlay up, trust its CA,
   mint an API token and make a first call.
2. [Simulate a month of OpenStack](/tutorials/simulate-a-month-of-openstack):
   start the simulator stack for July 2026 with the held-back switch on, watch
   the month arrive, finish it at once and read it back.
3. [Meter and rate your first month](/tutorials/meter-and-rate-your-first-month):
   import the pricing model, run the month, read the warnings, export the run
   as JSON and read one project's statement.
4. [Watch the month in Grafana](/tutorials/watch-the-month-in-grafana): read the
   four dashboards against the simulated cloud and the simulated month, find
   the one alert the dev stack fires by design, and take the state the next
   track continues from.
5. [Tear down your local Tally](/tutorials/tear-down-your-local-tally): stop the
   stack, delete the cluster and remove what the lessons wrote.
   Assumes Set up your local Tally, or any later lesson.

The fifth lesson is where the track ends when you want a clean machine. A
reader going on to the billing track does it after that track's last lesson,
because the billing track starts with
[Discount a customer group](/tutorials/discount-a-customer-group) and continues
from the state the fourth lesson leaves behind.

### Billing track

1. [Discount a customer group](/tutorials/discount-a-customer-group): register
   three projects, group them under a customer with a discount on each
   membership, run the month again and read a discounted statement and the
   group's rollup. Assumes Watch the month in Grafana.
2. [Pay a reseller a kickback](/tutorials/pay-a-reseller-a-kickback): register
   the CI tenant, put it under a partner with a discount and a kickback, run
   the month and read the statement and the kickback report.
   Assumes Discount a customer group.
3. [Attribute a tenant to its Gardener project](/tutorials/attribute-a-tenant-to-its-gardener-project):
   register the two Gardener projects and their tenants, relate each project to
   its tenant, see the registry refuse a cycle, run the month and read the
   attributed statement. Assumes Pay a reseller a kickback.
4. [Finalize the month and export it](/tutorials/finalize-the-month-and-export-it):
   finalize the month on the attributed run, see the engine refuse to meter it
   again, and export it as JSON and as CSV.
   Assumes Attribute a tenant to its Gardener project.
5. [Book the late events as a credit note](/tutorials/book-the-late-events-as-a-credit-note):
   release the notifications the simulator held back, book them as a
   correction, read the credit notes and close the correction.
   Assumes Finalize the month and export it.

The fifth lesson is where the billing track ends, and the state it leaves is
written out on that page.
[Tear down your local Tally](/tutorials/tear-down-your-local-tally) is the way
back to a clean machine.

## The simulated cloud

Every lesson works on the same generated month: seed 1, July 2026, the cloud
`os-sim`, six tenants, of which three are classic projects, two are Gardener
tenants and one is a CI tenant. That month renders 15727 notifications, 1812 of
them billable, and 84 of those stay held back by the switch the second lesson
turns on, so the counts a lesson shows are the counts every machine gets. The
two Gardener projects are `alpha` and `beta`, and the billing track registers
them under the cloud `garden-sim`, one installation of one platform, beside the
tenants of `os-sim`.

What the month holds is in
[the simulated OpenStack world](/explanation/the-simulated-openstack-world).

## When to read this section

- You are new to Tally and want a guided first hour that ends with a rated
  month you built yourself.
- You contribute to Tally and want a known-good baseline of the dev stack, with
  every output pinned to one run, before you change it.

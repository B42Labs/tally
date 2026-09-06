---
title: Worked examples
description: "The concept's worked examples, kept verbatim, with the golden case each one seeded."
quadrant: explanation
audience: all
---

# Worked examples

The concept worked its metering and rating out on paper before any of it was
built. Those examples are kept here as they were written, each one beside the
golden case it seeded.

The expected numbers of a golden case are derived by hand from these examples
and from the pricing model. They are never regenerated from what the engine
produced, because a number the engine wrote says nothing about whether the
engine is right
([the golden harness](https://github.com/B42Labs/tally/blob/main/internal/engine/golden_fixture_test.go)).
Turning the examples into that suite is WP 3.11 of
[the metering and rating roadmap](https://github.com/B42Labs/tally/blob/main/roadmap/03-phase-3-metering-rating.md).

Examples 2, 4 and 5 run through the engine today from seeded events, while no
collector produces such events on any deployment
([the providers that do not exist yet](/explanation/providers-that-do-not-exist-yet)).
Every example is rated with
[pricing/2026-03.yaml](https://github.com/B42Labs/tally/blob/main/pricing/2026-03.yaml),
and every usage record the engine writes also carries `count: 1`, which the
examples omit.

## Example 1: OpenStack instance resize mid-month

The golden case is
[`instance_resize`](https://github.com/B42Labs/tally/blob/main/internal/engine/testdata/golden/instance_resize/),
which bills `def-456` for 21600 and 23040 minutes.

VM `def-456` is resized from m1.small to m1.large on March 16:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "proj-456",
  "platform": "openstack",
  "usage_records": [
    {
      "resource_type": "instance",
      "resource_id": "def-456",
      "state": "active",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-16T00:00:00Z",
      "usage": {
        "minutes": 21600,
        "vcpus": 2, "ram_gb": 4, "disk_gb": 40, "flavor": "m1.small",
        "egress_gb": 12.3, "ingress_gb": 5.1
      }
    },
    {
      "resource_type": "instance",
      "resource_id": "def-456",
      "state": "active",
      "from": "2026-03-16T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": {
        "minutes": 23040,
        "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "flavor": "m1.large",
        "egress_gb": 24.7, "ingress_gb": 10.2
      }
    }
  ]
}
```

## Example 2: Hetzner server upgrade mid-month

The golden case is
[`hetzner_upgrade`](https://github.com/B42Labs/tally/blob/main/internal/engine/testdata/golden/hetzner_upgrade/),
which bills `srv-001` for 20160 and 24480 minutes.

Server `srv-001` is upgraded from CX21 to CX31 on March 15:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "hetzner-proj-42",
  "platform": "hetzner",
  "usage_records": [
    {
      "resource_type": "server",
      "resource_id": "srv-001",
      "state": "running",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-15T00:00:00Z",
      "usage": {
        "minutes": 20160,
        "vcpus": 2, "ram_gb": 4, "disk_gb": 40, "server_type": "cx21"
      }
    },
    {
      "resource_type": "server",
      "resource_id": "srv-001",
      "state": "running",
      "from": "2026-03-15T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": {
        "minutes": 24480,
        "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "server_type": "cx31"
      }
    }
  ]
}
```

## Example 3: OpenStack volume resize and retype

The golden case is
[`volume_resize_retype`](https://github.com/B42Labs/tally/blob/main/internal/engine/testdata/golden/volume_resize_retype/),
which bills `vol-789` for 12960, 14400 and 17280 minutes.

Volume `vol-789` is extended from 100 GB to 200 GB on March 10, then retyped
from SSD to HDD on March 20:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "proj-456",
  "platform": "openstack",
  "usage_records": [
    {
      "resource_type": "volume",
      "resource_id": "vol-789",
      "state": "in-use",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-10T00:00:00Z",
      "usage": { "minutes": 12960, "size_gb": 100, "type": "ssd" }
    },
    {
      "resource_type": "volume",
      "resource_id": "vol-789",
      "state": "in-use",
      "from": "2026-03-10T00:00:00Z",
      "to": "2026-03-20T00:00:00Z",
      "usage": { "minutes": 14400, "size_gb": 200, "type": "ssd" }
    },
    {
      "resource_type": "volume",
      "resource_id": "vol-789",
      "state": "in-use",
      "from": "2026-03-20T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": { "minutes": 17280, "size_gb": 200, "type": "hdd" }
    }
  ]
}
```

## Example 4: Gardener shoot worker scaling and hibernation

The golden case is
[`shoot_scale_hibernate`](https://github.com/B42Labs/tally/blob/main/internal/engine/testdata/golden/shoot_scale_hibernate/),
which bills `shoot-abc` for 15840, 18720, 4320 and 5760 minutes.

Shoot `shoot-abc` scales from 3 to 5 workers on March 12, then hibernates on
March 25 and wakes on March 28:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "team-alpha",
  "platform": "gardener",
  "usage_records": [
    {
      "resource_type": "shoot",
      "resource_id": "shoot-abc",
      "state": "active",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-12T00:00:00Z",
      "usage": { "minutes": 15840, "worker_count": 3, "machine_type": "m1.xlarge" }
    },
    {
      "resource_type": "shoot",
      "resource_id": "shoot-abc",
      "state": "active",
      "from": "2026-03-12T00:00:00Z",
      "to": "2026-03-25T00:00:00Z",
      "usage": { "minutes": 18720, "worker_count": 5, "machine_type": "m1.xlarge" }
    },
    {
      "resource_type": "shoot",
      "resource_id": "shoot-abc",
      "state": "hibernated",
      "from": "2026-03-25T00:00:00Z",
      "to": "2026-03-28T00:00:00Z",
      "usage": { "minutes": 4320, "worker_count": 5, "machine_type": "m1.xlarge" }
    },
    {
      "resource_type": "shoot",
      "resource_id": "shoot-abc",
      "state": "active",
      "from": "2026-03-28T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": { "minutes": 5760, "worker_count": 5, "machine_type": "m1.xlarge" }
    }
  ]
}
```

## Example 5: Harbor repository with counters

The golden case is
[`harbor_counters`](https://github.com/B42Labs/tally/blob/main/internal/engine/testdata/golden/harbor_counters/),
which bills `team-alpha/app` for 24480 and 20160 minutes.

Repository `team-alpha/app` exists all month; storage grows from 10 GB to 15 GB
on March 18:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "harbor-team-alpha",
  "platform": "harbor",
  "usage_records": [
    {
      "resource_type": "repository",
      "resource_id": "team-alpha/app",
      "state": "active",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-18T00:00:00Z",
      "usage": {
        "minutes": 24480,
        "storage_gb": 10,
        "pulls": 812,
        "pushes": 47,
        "egress_gb": 38.5
      }
    },
    {
      "resource_type": "repository",
      "resource_id": "team-alpha/app",
      "state": "active",
      "from": "2026-03-18T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": {
        "minutes": 20160,
        "storage_gb": 15,
        "pulls": 711,
        "pushes": 23,
        "egress_gb": 31.2
      }
    }
  ]
}
```

## End to end: an OpenStack VM with state changes

The golden case is
[`e2e_power_cycle`](https://github.com/B42Labs/tally/blob/main/internal/engine/testdata/golden/e2e_power_cycle/),
which bills `abc-123` for 14400, 14400 and 15840 minutes.

A VM `abc-123` (m1.large: 4 vCPUs, 8 GB RAM, 80 GB disk) runs the full month of
March, but is powered off from March 11 to March 21 (10 days = 14400 minutes).

### Step 1: metering

Metering produces three usage records, split at each state change:

```json
{
  "billing_period": { "from": "2026-03-01T00:00:00Z", "to": "2026-04-01T00:00:00Z" },
  "project_id": "proj-456",
  "platform": "openstack",
  "usage_records": [
    {
      "resource_type": "instance",
      "resource_id": "abc-123",
      "state": "active",
      "from": "2026-03-01T00:00:00Z",
      "to": "2026-03-11T00:00:00Z",
      "usage": { "minutes": 14400, "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 18.0 }
    },
    {
      "resource_type": "instance",
      "resource_id": "abc-123",
      "state": "shutoff",
      "from": "2026-03-11T00:00:00Z",
      "to": "2026-03-21T00:00:00Z",
      "usage": { "minutes": 14400, "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 0 }
    },
    {
      "resource_type": "instance",
      "resource_id": "abc-123",
      "state": "active",
      "from": "2026-03-21T00:00:00Z",
      "to": "2026-04-01T00:00:00Z",
      "usage": { "minutes": 15840, "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 22.5 }
    }
  ]
}
```

### Step 2: rating

Rating applies the pricing model with `state_modifiers` (`active` = 1.0,
`shutoff` = 0.5):

```text
Record 1 (active, 10 days):
  vcpus:  (14400/60) × 4 × 0.02 × 1.0  = 19.20 EUR
  ram_gb: (14400/60) × 8 × 0.005 × 1.0  =  9.60 EUR
  disk_gb:(14400/60) × 80 × 0.001 × 1.0 = 19.20 EUR
  egress: 18.0 × 0.09                    =  1.62 EUR
  → subtotal: 49.62 EUR

Record 2 (shutoff, 10 days – 50% modifier on time_gauge, no egress):
  vcpus:  (14400/60) × 4 × 0.02 × 0.5  =  9.60 EUR
  ram_gb: (14400/60) × 8 × 0.005 × 0.5  =  4.80 EUR
  disk_gb:(14400/60) × 80 × 0.001 × 0.5 =  9.60 EUR
  egress: 0 × 0.09                       =  0.00 EUR
  → subtotal: 24.00 EUR

Record 3 (active, 11 days):
  vcpus:  (15840/60) × 4 × 0.02 × 1.0  = 21.12 EUR
  ram_gb: (15840/60) × 8 × 0.005 × 1.0  = 10.56 EUR
  disk_gb:(15840/60) × 80 × 0.001 × 1.0 = 21.12 EUR
  egress: 22.5 × 0.09                    =  2.03 EUR
  → subtotal: 54.83 EUR
```

### Step 3: rating output

The rating output aggregates per resource and shows the state breakdown:

```json
{
  "billing_period": {
    "from": "2026-03-01T00:00:00Z",
    "to": "2026-04-01T00:00:00Z"
  },
  "project_id": "proj-456",
  "platform": "openstack",
  "line_items": [
    {
      "resource_type": "instance",
      "resource_id": "abc-123",
      "platform": "openstack",
      "description": "m1.large instance",
      "periods": [
        {
          "state": "active",
          "hours": 240,
          "usage": { "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 18.0 },
          "cost": { "vcpus": 19.20, "ram_gb": 9.60, "disk_gb": 19.20, "egress_gb": 1.62, "total": 49.62 },
          "state_modifier": 1.0
        },
        {
          "state": "shutoff",
          "hours": 240,
          "usage": { "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 0 },
          "cost": { "vcpus": 9.60, "ram_gb": 4.80, "disk_gb": 9.60, "egress_gb": 0, "total": 24.00 },
          "state_modifier": 0.5
        },
        {
          "state": "active",
          "hours": 264,
          "usage": { "vcpus": 4, "ram_gb": 8, "disk_gb": 80, "egress_gb": 22.5 },
          "cost": { "vcpus": 21.12, "ram_gb": 10.56, "disk_gb": 21.12, "egress_gb": 2.03, "total": 54.83 },
          "state_modifier": 1.0
        }
      ],
      "total": 128.45
    }
  ],
  "related_costs": [],
  "total": 128.45,
  "currency": "EUR"
}
```

A VM that runs the entire month of March (744 hours) at full price costs 148.80
EUR plus egress. With ten days powered off at the 50% modifier, the total drops
to 128.45 EUR. A shelved VM (`state_modifier: 0.0`) costs 0 EUR for the shelved
period.

The credit that follows from the same power cycle is the golden case
`correction_credit`. It bills the month as one active interval, finalizes it,
and then corrects it with the power cycle arriving late. The three deltas add up
to a credit of 24.00 EUR. That case carries no `expected.json`: its expectations
live in
[the golden integration test](https://github.com/B42Labs/tally/blob/main/internal/engine/golden_integration_test.go).

## Related costs: Gardener and OpenStack

A Gardener shoot is billed to its project as a management fee, and the worker VM
it runs on is billed as a related cost of that project rather than hidden in the
shoot price (see
[project registry, relations and exclusive attribution](/explanation/project-registry-relations-and-attribution)).
The golden case is
[`related_costs`](https://github.com/B42Labs/tally/blob/main/internal/engine/testdata/golden/related_costs/),
which bills `shoot-abc` and `worker-1` for 44640 minutes each.

```json
{
  "billing_period": {
    "from": "2026-03-01T00:00:00Z",
    "to": "2026-04-01T00:00:00Z"
  },
  "project_id": "team-alpha",
  "platform": "gardener",
  "line_items": [
    {
      "resource_type": "shoot",
      "resource_id": "shoot-abc",
      "platform": "gardener",
      "description": "Shoot cluster shoot-abc (management fee)",
      "periods": [
        { "state": "active", "hours": 744, "usage": { "worker_count": 3 },
          "cost": { "worker_count": 223.20, "total": 223.20 }, "state_modifier": 1.0 }
      ],
      "total": 223.20
    }
  ],
  "related_costs": [
    {
      "relation_type": "infrastructure_tenant",
      "project_id": "shoot-abc-os-tenant",
      "platform": "openstack",
      "line_items": [
        {
          "resource_type": "instance",
          "resource_id": "worker-1",
          "platform": "openstack",
          "description": "m1.xlarge worker node",
          "periods": [
            { "state": "active", "hours": 744,
              "usage": { "vcpus": 8, "ram_gb": 16, "disk_gb": 160 },
              "cost": { "vcpus": 119.04, "ram_gb": 59.52, "disk_gb": 119.04, "total": 297.60 },
              "state_modifier": 1.0 }
          ],
          "total": 297.60
        }
      ],
      "total": 297.60
    }
  ],
  "total": 520.80,
  "currency": "EUR"
}
```

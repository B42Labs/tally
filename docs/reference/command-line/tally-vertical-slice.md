---
title: Vertical slice (tally-vertical-slice)
description: The flags, environment, exit status and output document of the throwaway vertical slice, with the golden numbers it rates.
quadrant: reference
audience: operator
---

# Vertical slice (tally-vertical-slice)

`tally-vertical-slice` rates one project's instance usage for one calendar month
and prints it as JSON. It reads the Reporting API, folds each instance's event
history with `internal/core/timeline`, clips the intervals to the month, rates
the records against a pricing file, and checks the metering invariants over what
it derived.

The slice is throwaway on purpose. It is the golden-numbers gate of WP 1.15 in
[`roadmap/01-phase-1-core-platform-openstack.md`](https://github.com/B42Labs/tally/blob/main/roadmap/01-phase-1-core-platform-openstack.md),
and its job is to prove the billing chain end to end, once, on figures a person
can check by hand. WP 3.3 of
[`roadmap/03-phase-3-metering-rating.md`](https://github.com/B42Labs/tally/blob/main/roadmap/03-phase-3-metering-rating.md)
replaces it wholesale with `internal/engine/metering`, which meters every
resource type against the project graph and versioned pricing models. Only the
timeline fold carries over.

That is why the clipping, the rating and the invariant checks live in this
command rather than in a package under `internal/`. They are the engine's work,
and a prototype's version of them in the core would give code that is meant to
be deleted a home other packages import. Nothing under `internal/` knows about
this command.

## Flags

<!-- refdoc:begin flags -->
| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--ca-file` | string | none | PEM bundle to trust instead of the system store, for a dev cluster's own CA |
| `--cloud` | string | none | the cloud the instances live in, os-prod-eu1 for example |
| `--month` | string | none | the calendar month to rate, as YYYY-MM |
| `--pricing` | string | none | path to the pricing file, pricing/prototype.yaml for example |
| `--project` | string | none | the project to rate, named the way its cloud names it |
| `--reporting-url` | string | none | https base URL of the Reporting API, without the /api/v1 suffix |
<!-- refdoc:end flags -->

`--cloud`, `--project`, `--month`, `--reporting-url` and `--pricing` are all
required, and a run missing one is refused before the first request, naming the
flag. `--ca-file` is optional and replaces the system trust store for the run,
which is what reaches a Gateway whose certificate the host does not verify on
its own.

## Environment

`TALLY_SLICE_TOKEN` is required and holds an API token of role `read_all`. It is
read from the environment rather than from a flag, so the credential never
appears in the process's `argv`. A run without it is refused before the first
request, with the name of the variable.

## Exit status

A clean run exits 0.

A run whose records break a metering invariant prints the document first and
then exits 1. The breaches stand in the resource's `violations` array: the
numbers are what the run is for, so they stay readable while the exit status
still reports the failure.

Any other error exits 1 without a document.

## The document

The run writes one JSON document to stdout: one project's rated usage for one
month, with one entry per resource and one record per billed interval.

### The members

<!-- refdoc:begin document -->
#### `document`

document is the slice's whole output: one project's rated usage for one month.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `cloud` | string | always |  |
| `project_id` | string | always |  |
| `period` | [periodDoc](#perioddoc) | always |  |
| `currency` | string | always |  |
| `resources` | array of [resourceDoc](#resourcedoc) | always |  |
| `total` | decimal, 2 places | always |  |

#### `periodDoc`

periodDoc is the reported span, half-open as everywhere else.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `from` | string, RFC 3339 UTC | always |  |
| `to` | string, RFC 3339 UTC | always |  |

#### `resourceDoc`

resourceDoc is one resource's rated usage. Warnings come from the fold and violations from the invariant checks: both are reported next to the numbers they qualify rather than to a log nobody reads next to the output.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `resource_id` | string | always |  |
| `warnings` | list, comma-separated | always |  |
| `violations` | list, comma-separated | always |  |
| `records` | array of [recordDoc](#recorddoc) | always |  |
| `total` | decimal, 2 places | always |  |

#### `recordDoc`

recordDoc is one billed interval with its line items.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `from` | string, RFC 3339 UTC | always |  |
| `to` | string, RFC 3339 UTC | always |  |
| `state` | string | always |  |
| `seconds` | integer | always |  |
| `minutes` | decimal, 4 places | always |  |
| `dimensions` | object of [dimensionDoc](#dimensiondoc) | always |  |
| `subtotal` | decimal, 2 places | always |  |

#### `dimensionDoc`

dimensionDoc is what one priced metric contributed to a record.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `quantity` | decimal, 4 places | always |  |
| `cost` | decimal, 2 places | always |  |
<!-- refdoc:end document -->

### The golden document

The slice rates the instance `abc-123` at 124.80 EUR and the project
`proj-456` at 237.60 EUR for March 2026, and prints this document for it.

```json
{
  "cloud": "os-prod-eu1",
  "project_id": "proj-456",
  "period": {
    "from": "2026-03-01T00:00:00Z",
    "to": "2026-04-01T00:00:00Z"
  },
  "currency": "EUR",
  "resources": [
    {
      "resource_id": "abc-123",
      "warnings": [],
      "violations": [],
      "records": [
        {
          "from": "2026-03-01T00:00:00Z",
          "to": "2026-03-11T00:00:00Z",
          "state": "active",
          "seconds": 864000,
          "minutes": 14400.0000,
          "dimensions": {
            "disk_gb": {
              "quantity": 80,
              "cost": 19.20
            },
            "ram_gb": {
              "quantity": 8,
              "cost": 9.60
            },
            "vcpus": {
              "quantity": 4,
              "cost": 19.20
            }
          },
          "subtotal": 48.00
        },
        {
          "from": "2026-03-11T00:00:00Z",
          "to": "2026-03-21T00:00:00Z",
          "state": "shutoff",
          "seconds": 864000,
          "minutes": 14400.0000,
          "dimensions": {
            "disk_gb": {
              "quantity": 80,
              "cost": 9.60
            },
            "ram_gb": {
              "quantity": 8,
              "cost": 4.80
            },
            "vcpus": {
              "quantity": 4,
              "cost": 9.60
            }
          },
          "subtotal": 24.00
        },
        {
          "from": "2026-03-21T00:00:00Z",
          "to": "2026-04-01T00:00:00Z",
          "state": "active",
          "seconds": 950400,
          "minutes": 15840.0000,
          "dimensions": {
            "disk_gb": {
              "quantity": 80,
              "cost": 21.12
            },
            "ram_gb": {
              "quantity": 8,
              "cost": 10.56
            },
            "vcpus": {
              "quantity": 4,
              "cost": 21.12
            }
          },
          "subtotal": 52.80
        }
      ],
      "total": 124.80
    },
    {
      "resource_id": "def-456",
      "warnings": [],
      "violations": [],
      "records": [
        {
          "from": "2026-03-01T00:00:00Z",
          "to": "2026-03-16T00:00:00Z",
          "state": "active",
          "seconds": 1296000,
          "minutes": 21600.0000,
          "dimensions": {
            "disk_gb": {
              "quantity": 40,
              "cost": 14.40
            },
            "ram_gb": {
              "quantity": 4,
              "cost": 7.20
            },
            "vcpus": {
              "quantity": 2,
              "cost": 14.40
            }
          },
          "subtotal": 36.00
        },
        {
          "from": "2026-03-16T00:00:00Z",
          "to": "2026-04-01T00:00:00Z",
          "state": "active",
          "seconds": 1382400,
          "minutes": 23040.0000,
          "dimensions": {
            "disk_gb": {
              "quantity": 80,
              "cost": 30.72
            },
            "ram_gb": {
              "quantity": 8,
              "cost": 15.36
            },
            "vcpus": {
              "quantity": 4,
              "cost": 30.72
            }
          },
          "subtotal": 76.80
        }
      ],
      "total": 112.80
    }
  ],
  "total": 237.60
}
```

Both `warnings` arrays are empty, and so are both `violations` arrays: the
golden history folds without a gap, and the records tile exactly the part of
March each instance lived through.

## The egress delta

The end-to-end example on [Worked examples](/explanation/worked-examples) rates
`abc-123` at 128.45 EUR. The whole difference is egress: that example bills
18.0 GB in the first active interval at 1.62 EUR and 22.5 GB in the third at
2.03 EUR, and 124.80 plus 3.65 is 128.45. The vCPU, RAM and disk costs of the
two agree interval for interval, and so do the three intervals themselves.

The slice bills no egress because egress is a counter metric rather than a
property of a resource's state timeline: it is summed from usage counters over
the period, which is Phase 3 scope. The delta is recorded rather than
approximated. An estimated egress figure would cost the golden numbers the one
property this prototype exists for, that a reader can recompute them from the
events and the price list alone.

## Verification

[`cmd/tally-vertical-slice/slice_integration_test.go`](https://github.com/B42Labs/tally/blob/main/cmd/tally-vertical-slice/slice_integration_test.go)
holds the same numbers in CI. It assembles the real router, ingest pipeline and
authenticator over a migrated TimescaleDB in a container, ingests the golden
events through them, and compares the printed document against the expected
figures value by value, the encoded precision included. Its fixture carries one
instance more than the document above, so the total it pins is 273.60 EUR over
three resources while the per-resource numbers of `abc-123` and `def-456` are
the ones printed here.

---
title: Pricing model file
description: "The YAML file a pricing catalog is imported from: its schema, its versioning and how a price is written."
quadrant: reference
audience: operator
---

# Pricing model file

A pricing catalog is a YAML file imported once with
`tally-engine pricing import <file>` and referred to afterwards by its
`version`, which every rated record carries. A price change is a new version and
a new import, so what an earlier run billed is never rewritten.

A price is written as a string or as a number. Either way it keeps the digits
the file spells and reaches `decimal.NewFromString` as that text, so nothing
between the file and the amount goes through a float. `valid_from` is an RFC
3339 timestamp; `ParseDocument` in
[`internal/engine/pricing/pricing.go`](https://github.com/B42Labs/tally/blob/main/internal/engine/pricing/pricing.go)
parses it and stores it as UTC, so a file that writes an offset selects the same
periods as one that writes `Z`.

## Schema

The file is held to
[`internal/engine/pricing/pricing.schema.json`](https://github.com/B42Labs/tally/blob/main/internal/engine/pricing/pricing.schema.json),
which is embedded in the binary. `pricing` is keyed by platform and then by
resource type.

### The objects

<!-- refdoc:begin schema -->
`Tally pricing model`, an object.

#### `root`

| Property | Type | Required | Constraints |
| --- | --- | --- | --- |
| `version` | string | yes | minLength 1 |
| `valid_from` | string | yes | none |
| `currency` | string | yes | `^[A-Z]{3}$` |
| `pricing` | object | yes | minProperties 1; values object (minProperties 1; values [resource](#resource)) |

No other property is allowed.

#### `resource`

| Property | Type | Required | Constraints |
| --- | --- | --- | --- |
| `dimensions` | array of [dimension](#dimension) | yes | minItems 1 |
| `state_modifiers` | object | no | values [price](#price) |
| `type_modifiers` | object | no | values [price](#price) |

No other property is allowed.

#### `dimension`

| Property | Type | Required | Constraints |
| --- | --- | --- | --- |
| `metric` | string | yes | minLength 1 |
| `type` | enum | yes | `time_gauge`, `counter` |
| `price_per_unit_hour` | [price](#price) | no | none |
| `price_per_unit` | [price](#price) | no | none |

No other property is allowed.

Exactly one of these alternatives holds:

- `price_per_unit_hour` is required, `price_per_unit` is absent, `type` is `time_gauge`
- `price_per_unit` is required, `price_per_unit_hour` is absent, `type` is `counter`

#### `price`

A number at least 0, or a string matching `^[0-9]+(\.[0-9]+)?$`.
<!-- refdoc:end schema -->

## Semantics

A `time_gauge` dimension is priced per unit and hour. The usage record carries
`minutes`; the hours are those minutes divided by 60, and the amount is the
hours times the quantity times `price_per_unit_hour`, times the state modifier
and the type modifier.

A `counter` dimension is priced per unit. The amount is the counter's value
times `price_per_unit`, whatever state the resource was in. Neither modifier
applies to a counter: a gigabyte of egress costs what it costs however the
resource it left was running.

`state_modifiers` is keyed by the state a usage record was in. A state the map
does not name is billed at `1`, and so is every state where the entry names no
modifier at all. The factor is rounded to four places before anything is billed
at it, which is the scale a statement prints it at.

`type_modifiers` is keyed by the value the usage carries under `type`, the size
member a volume and some servers carry. A resource that reports no type is
billed unmodified.

A resource type the model does not price is not billed as free. Its resources
are skipped and counted per platform and resource type, and the count reaches an
operator as `unpriced` in the run's stats.

A model carries one currency, and every amount a run rates with it is in that
currency.

The parser refuses a file before it becomes a version. It names the line, and it
refuses:

- a file holding more than one YAML document, because the model belongs in the
  first and a second one would be stored under a version that prices none of it.
- a mapping key that is not a string, and a key set twice.
- a scalar that is neither a string nor a number. An unquoted
  `2026-03-01T00:00:00Z` is a timestamp and an unquoted `true` is a boolean, and
  the message says which scalar to quote.
- a document nested more than 64 levels deep, one expanding to more than
  1048576 values, and one expanding to more than 16777216 bytes, which is what
  nested anchors do.
- an anchor that holds itself.

The reader that follows the parser refuses a document the schema rejects, a
`valid_from` that is not RFC 3339, and a resource type that prices one metric
twice, which would bill that metric twice under one name. The refusals live in
[`internal/engine/pricing/parse.go`](https://github.com/B42Labs/tally/blob/main/internal/engine/pricing/parse.go)
and the file beside it.

## Example

[`pricing/2026-03.yaml`](https://github.com/B42Labs/tally/blob/main/pricing/2026-03.yaml)
is the model the repository ships.

<!-- refdoc:begin example -->
```yaml
# The pricing model of the concept's worked examples
# (docs/explanation/worked-examples.md). A price may be written as a number or as a string: either
# way it keeps the digits spelled here and is read with decimal.NewFromString,
# never through a float. `tally-engine pricing import` loads this file.
version: "2026-03"
valid_from: "2026-03-01T00:00:00Z"
currency: "EUR"

pricing:
  openstack:
    instance:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.02"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.005"
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.001"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.09"
      state_modifiers:
        shelved: "0.0"
        shutoff: "0.5"
    volume:
      dimensions:
        - metric: "size_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.0001"
      type_modifiers:
        ssd: "1.0"
        hdd: "0.5"
    floating_ip:
      dimensions:
        - metric: "count"
          type: "time_gauge"
          price_per_unit_hour: "0.005"

  hetzner:
    server:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.015"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.004"
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.0008"
      state_modifiers:
        "off": "0.5"

  stackit:
    server:
      dimensions:
        - metric: "vcpus"
          type: "time_gauge"
          price_per_unit_hour: "0.025"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.006"
        - metric: "disk_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.0012"

  ionos:
    server:
      dimensions:
        - metric: "cores"
          type: "time_gauge"
          price_per_unit_hour: "0.022"
        - metric: "ram_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.005"

  gardener:
    shoot:
      dimensions:
        - metric: "worker_count"
          type: "time_gauge"
          price_per_unit_hour: "0.10"
      state_modifiers:
        hibernated: "0.0"

  harbor:
    repository:
      dimensions:
        - metric: "storage_gb"
          type: "time_gauge"
          price_per_unit_hour: "0.00005"
        - metric: "egress_gb"
          type: "counter"
          price_per_unit: "0.12"
        - metric: "pulls"
          type: "counter"
          price_per_unit: "0.0"
```
<!-- refdoc:end example -->

## See also

The [metering separated from rating](/explanation/metering-separated-from-rating)
page states why usage and price are two passes, and
[money and rounding](/explanation/money-and-rounding) states the arithmetic every
amount above is computed with.

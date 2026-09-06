---
title: Export formats
description: The files tally-engine export and tally-engine kickbacks write, their names, their JSON members and their CSV columns, with one example of each.
quadrant: reference
audience: operator
---

# Export formats

`tally-engine export` writes one run's billing artifacts into a directory, as
JSON documents or as CSV tables. `tally-engine kickbacks` writes the partner
settlement of one run on its own.

Only a completed or a finalized run is exported. The superseded, failed and
running rows a period accumulates stay in the database for audit, and
[`internal/engine/export/export.go`](https://github.com/B42Labs/tally/blob/main/internal/engine/export/export.go)
refuses them.

An export is a function of the run it reads. Every ordering is fixed by the
queries, no artifact records when it was written, and each file is written to a
temporary name and renamed into place, so exporting the same finalized run twice
yields byte-identical files. A `--rollup` export is the exception: its
membership is read from the registry at export time, so two exports differ where
a relation was created or closed between them.

Every timestamp is UTC and RFC 3339, at the precision the value carries, so a
whole second renders without a fraction. An amount carries two decimal places, a
usage quantity four, and the rate of an adjustment six.

## Files

`--out` names the directory, and it has to be empty or absent, so what it holds
afterwards is one run's artifacts and nothing an earlier export left there. The
JSON writer is
[`internal/engine/export/json.go`](https://github.com/B42Labs/tally/blob/main/internal/engine/export/json.go)
and the CSV writer
[`internal/engine/export/csv.go`](https://github.com/B42Labs/tally/blob/main/internal/engine/export/csv.go).

A JSON export writes:

- `run.json`, the index, written last, after every file it names is on stable
  storage.
- `statement-<key>.json`, one per project a regular run billed.
- `credit-note-<key>.json`, one per project a correction run credited or
  debited.
- `kickbacks.json`, the partner settlement. It is written for every run and
  carries an empty beneficiary list where the run owes nobody.
- `rollup-<key>.json`, one per group, on a run exported with `--rollup`.

A CSV export writes:

- `rated.csv`, one row per rated record.
- `deltas.csv`, one row per delta, on a correction run alone.
- `kickbacks.csv`, the settlement as a table.
- `rollup.csv`, one row per member of every group, on a run exported with
  `--rollup`.

`<key>` is the statement key, the cloud and the project id joined by a slash.
Both halves are escaped, and the key is escaped once more as a whole, so the
slash between them becomes `%2F` and a percent inside a half becomes `%25`. The
cloud `os-prod` with the project `proj-456` is therefore written to
`statement-os-prod%2Fproj-456.json`. The double escaping is what keeps two keys
apart that a single escaping would render the same file name for.

A name longer than 200 bytes is replaced by the SHA-256 digest of its key under
the same prefix, and so is the second of two names that differ in ASCII case
alone. Which pair such a file stands for is read off the index beside it.

## `run.json`

The index names the run, the pricing version it rated with, its stats, and one
entry per document beside the file that document was written to. Nothing in it
records when the export ran.

### The members

<!-- refdoc:begin run-json -->
#### `runDocument`

runDocument is run.json. The field order is the order it is marshalled in. Nothing here records when the export ran: exporting one finalized run twice yields the same bytes, and a timestamp of the writing would be the one value that does not.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `run_id` | string | always |  |
| `kind` | string | always |  |
| `corrects_run_id` | string or null | always | The three nullable values are pointers, so what the run row carries as NULL renders as null rather than as an empty string. |
| `period_from` | string | always |  |
| `period_to` | string | always |  |
| `status` | string | always |  |
| `pricing_version` | string or null | always |  |
| `clouds` | list, comma-separated | always |  |
| `started_at` | string | always |  |
| `completed_at` | string or null | always |  |
| `stats` | object | always |  |
| `statements` | array of [statementEntry](#statemententry) | always |  |
| `rollup` | [rollupIndex](#rollupindex) or null | omitted when empty | Rollup is absent from the index of an export that summed nothing under a meta-project or a partner, which is every export no rollup was asked for. |

#### `statementEntry`

statementEntry is one document in the index: the file it was written to, the pair it bills, and the total it carries. The two halves of the key are unescaped, so a reader of the index gets the cloud and the project id as the registry holds them.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `file` | string | always |  |
| `cloud` | string | always |  |
| `project_id` | string | always |  |
| `total` | decimal, 2 places | always |  |
| `currency` | string | always |  |

#### `rollupIndex`

rollupIndex is what the index says about the rollup: the relation type the run was summed over, and one entry per group. The document list is empty rather than absent where the rollup reached no group, which is what says the run rolled nothing up.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `relation_type` | string | always |  |
| `documents` | array of [rollupEntry](#rollupentry) | always |  |

#### `rollupEntry`

rollupEntry is one group in the index: the file it was written to, the virtual project it sums under, how many members it holds, and their total.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `file` | string | always |  |
| `cloud` | string | always |  |
| `project_id` | string | always |  |
| `members` | integer | always |  |
| `total` | decimal, 2 places | always |  |
| `currency` | string | always |  |
<!-- refdoc:end run-json -->

### Example

<!-- refdoc:begin example-run-json -->
```json
{
  "run_id": "3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4",
  "kind": "regular",
  "corrects_run_id": null,
  "period_from": "2026-03-01T00:00:00Z",
  "period_to": "2026-04-01T00:00:00Z",
  "status": "finalized",
  "pricing_version": "2026-03",
  "clouds": [],
  "started_at": "2026-04-04T00:00:00Z",
  "completed_at": "2026-04-04T00:01:00Z",
  "stats": {},
  "statements": [
    {
      "file": "statement-os-dr%2Fproj-789.json",
      "cloud": "os-dr",
      "project_id": "proj-789",
      "total": 22.32,
      "currency": "EUR"
    },
    {
      "file": "statement-os-prod%2Fproj-456.json",
      "cloud": "os-prod",
      "project_id": "proj-456",
      "total": 128.45,
      "currency": "EUR"
    }
  ]
}
```
<!-- refdoc:end example-run-json -->

## Statements

A regular run bills one document per project. The types are declared in
[`internal/engine/statements/statements.go`](https://github.com/B42Labs/tally/blob/main/internal/engine/statements/statements.go).

### The members

<!-- refdoc:begin statement -->
#### `Document`

Document is one project's statement. The field order is the order the document is marshalled in, which is the order the concept prints it in.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `billing_period` | [BillingPeriod](#billingperiod) | always |  |
| `project_id` | string | always |  |
| `platform` | string | always |  |
| `line_items` | array of [LineItem](#lineitem) | always |  |
| `related_costs` | array of [RelatedCost](#relatedcost) | always |  |
| `base_cost` | decimal, 2 places or null | omitted when empty | BaseCost is what the line items and the related costs add up to before the adjustments, NetCost what they come to after them, and KickbackTotal what a partner is owed beside the net cost rather than as part of it. The four members are nil on a statement no adjustment reached, whose bytes hold none of them. Total is the net cost where they are there, which is what the customer pays. None of them carries a currency of its own: every amount in the document is in the currency Currency names, the way Total already renders. |
| `adjustments` | array of `adjustments.Line` | omitted when empty |  |
| `net_cost` | decimal, 2 places or null | omitted when empty |  |
| `kickback_total` | decimal, 2 places or null | omitted when empty |  |
| `total` | decimal, 2 places | always |  |
| `currency` | string | always |  |

#### `BillingPeriod`

BillingPeriod is the half-open interval the document bills, both ends in UTC and RFC 3339.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `from` | string | always |  |
| `to` | string | always |  |

#### `LineItem`

LineItem is one resource as one project is billed for it: every period of the resource that project owned it for, and what they add up to.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `resource_type` | string | always |  |
| `resource_id` | string | always |  |
| `platform` | string | always |  |
| `description` | string | always |  |
| `periods` | array of [Period](#period) | always |  |
| `total` | decimal, 2 places | always |  |

#### `Period`

Period is one usage draft rendered: the state the resource was in, the hours it was in it, the quantities every dimension was rated from, what each of them cost, and what the state was billed at. Cost holds one key per dimension plus costTotal.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `state` | string | always |  |
| `hours` | decimal, 2 places | always |  |
| `usage` | object of decimal, 4 places | always |  |
| `cost` | object of decimal, 2 places | always |  |
| `state_modifier` | decimal, 4 places | always |  |

#### `RelatedCost`

RelatedCost is one attributed project's costs on the statement of the project they are billed under: the type of the edge that claimed it, who it is, and the same line items it would carry standalone.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `relation_type` | string | always |  |
| `project_id` | string | always |  |
| `platform` | string | always |  |
| `line_items` | array of [LineItem](#lineitem) | always |  |
| `total` | decimal, 2 places | always |  |
<!-- refdoc:end statement -->

### Example

<!-- refdoc:begin example-statement -->
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
          "hours": 240.00,
          "usage": {
            "disk_gb": 80.0000,
            "egress_gb": 18.0000,
            "ram_gb": 8.0000,
            "vcpus": 4.0000
          },
          "cost": {
            "disk_gb": 19.20,
            "egress_gb": 1.62,
            "ram_gb": 9.60,
            "total": 49.62,
            "vcpus": 19.20
          },
          "state_modifier": 1.0000
        },
        {
          "state": "shutoff",
          "hours": 240.00,
          "usage": {
            "disk_gb": 80.0000,
            "egress_gb": 0.0000,
            "ram_gb": 8.0000,
            "vcpus": 4.0000
          },
          "cost": {
            "disk_gb": 9.60,
            "egress_gb": 0.00,
            "ram_gb": 4.80,
            "total": 24.00,
            "vcpus": 9.60
          },
          "state_modifier": 0.5000
        },
        {
          "state": "active",
          "hours": 264.00,
          "usage": {
            "disk_gb": 80.0000,
            "egress_gb": 22.5000,
            "ram_gb": 8.0000,
            "vcpus": 4.0000
          },
          "cost": {
            "disk_gb": 21.12,
            "egress_gb": 2.03,
            "ram_gb": 10.56,
            "total": 54.83,
            "vcpus": 21.12
          },
          "state_modifier": 1.0000
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
<!-- refdoc:end example-statement -->

## Credit notes

A correction run hands each project a credit note instead: one entry per
dimension the two passes disagree on, with what the corrected run billed, what
the correction rated, and the difference. The types are declared in
[`internal/engine/corrections/corrections.go`](https://github.com/B42Labs/tally/blob/main/internal/engine/corrections/corrections.go).

### The members

<!-- refdoc:begin credit-note -->
#### `CreditNote`

CreditNote is one project's credit note: the deltas the correction credits or debits it for, and the run they correct. The field order is the order the document is marshalled in.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `billing_period` | `statements.BillingPeriod` | always |  |
| `project_id` | string | always |  |
| `platform` | string | always |  |
| `corrects_run_id` | string | always |  |
| `line_items` | array of [LineItem](#lineitem) | always |  |
| `related_costs` | array of [RelatedCost](#relatedcost) | always |  |
| `base_delta` | decimal, 2 places or null | omitted when empty | BaseDelta is what the line items and the related costs add up to before the adjustments, NetDelta what they come to after them, and KickbackDelta what a partner's commission changed by beside the net rather than as part of it. The four members are nil on a note no adjustment delta reached, whose bytes hold none of them. Total is the net delta where they are there, which is what the correction settles. None of them carries a currency of its own: every amount on the note is in the currency Currency names, the way Total already renders. |
| `adjustments` | array of [AdjustmentChange](#adjustmentchange) | omitted when empty |  |
| `net_delta` | decimal, 2 places or null | omitted when empty |  |
| `kickback_delta` | decimal, 2 places or null | omitted when empty |  |
| `total` | decimal, 2 places | always |  |
| `currency` | string | always |  |

#### `LineItem`

LineItem is one resource's deltas as one project is credited or debited for them: one entry per dimension the two passes disagree on, and what they add up to.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `resource_type` | string | always |  |
| `resource_id` | string | always |  |
| `platform` | string | always |  |
| `dimensions` | object of [Change](#change) | always |  |
| `total` | decimal, 2 places | always |  |

#### `Change`

Change is one dimension of one resource on the note: what the run being corrected billed, what the correction rated, and the difference the project is credited or debited for.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `old` | decimal, 2 places | always |  |
| `new` | decimal, 2 places | always |  |
| `delta` | decimal, 2 places | always |  |

#### `AdjustmentChange`

AdjustmentChange is one adjustment on the credit note: the relation it came from, what the run being corrected applied, what the correction applied, and the difference the project is credited or debited for. The field order is the order it is marshalled in.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `type` | string | always |  |
| `relation_type` | string | always |  |
| `relation_target` | string | always |  |
| `relation_id` | string | always |  |
| `scope` | string | always |  |
| `rate` | decimal, 6 places | always |  |
| `old` | decimal, 2 places | always |  |
| `new` | decimal, 2 places | always |  |
| `delta` | decimal, 2 places | always |  |

#### `RelatedCost`

RelatedCost is one attributed project's deltas on the credit note of the project they are billed under: the type of the edge that claimed it, who it is, and the same line items it would carry standalone.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `relation_type` | string | always |  |
| `project_id` | string | always |  |
| `platform` | string | always |  |
| `line_items` | array of [LineItem](#lineitem) | always |  |
| `total` | decimal, 2 places | always |  |
<!-- refdoc:end credit-note -->

### Examples

The index of a correction names its credit notes and the run it corrects.

<!-- refdoc:begin example-correction-run-json -->
```json
{
  "run_id": "4b9d2c17-6e85-4f3a-8a01-c5d4e6f7a8b9",
  "kind": "correction",
  "corrects_run_id": "3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4",
  "period_from": "2026-03-01T00:00:00Z",
  "period_to": "2026-04-01T00:00:00Z",
  "status": "finalized",
  "pricing_version": "2026-03",
  "clouds": [],
  "started_at": "2026-04-10T00:00:00Z",
  "completed_at": "2026-04-10T00:01:00Z",
  "stats": {},
  "statements": [
    {
      "file": "credit-note-os-prod%2Fproj-456.json",
      "cloud": "os-prod",
      "project_id": "proj-456",
      "total": -24.00,
      "currency": "EUR"
    }
  ]
}
```
<!-- refdoc:end example-correction-run-json -->

One credit note of that run:

<!-- refdoc:begin example-credit-note -->
```json
{
  "billing_period": {
    "from": "2026-03-01T00:00:00Z",
    "to": "2026-04-01T00:00:00Z"
  },
  "project_id": "proj-456",
  "platform": "openstack",
  "corrects_run_id": "3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4",
  "line_items": [
    {
      "resource_type": "instance",
      "resource_id": "abc-123",
      "platform": "openstack",
      "dimensions": {
        "disk_gb": {
          "old": 59.52,
          "new": 49.92,
          "delta": -9.60
        },
        "ram_gb": {
          "old": 29.76,
          "new": 24.96,
          "delta": -4.80
        },
        "vcpus": {
          "old": 59.52,
          "new": 49.92,
          "delta": -9.60
        }
      },
      "total": -24.00
    }
  ],
  "related_costs": [],
  "total": -24.00,
  "currency": "EUR"
}
```
<!-- refdoc:end example-credit-note -->

## Kickback settlement

`kickbacks.json` is written by every export and by
`tally-engine kickbacks --format json`. It holds, per beneficiary and currency,
the total the partner is owed, the number of projects it came off, and the rows
it was summed from. Two currencies under one partner are two entries, because a
sum over two of them is not a payout anybody can make.

A correction's settlement holds the differences to the run it corrects, in the
same shape, negative where usage was corrected down. `kind` and
`corrects_run_id` are what say which of the two a document holds. A key whose
amount difference is zero is left out.

### The members

<!-- refdoc:begin kickbacks -->
#### `kickbacksDocument`

kickbacksDocument is kickbacks.json: the run the settlement belongs to and one entry per partner it owes. The field order is the order it is marshalled in, and nothing here records when the report ran, for the reason runDocument gives. A correction's entries carry the differences to the run it corrects under the same shape a regular run's payouts take. Kind and CorrectsRunID are what say which of the two a document holds, so a partner reads a month and the correction of it with one reader rather than with two.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `run_id` | string | always |  |
| `kind` | string | always |  |
| `corrects_run_id` | string or null | always | A pointer, so a regular run renders null the way runDocument renders the run it corrects. |
| `period_from` | string | always |  |
| `period_to` | string | always |  |
| `beneficiaries` | array of [beneficiaryEntry](#beneficiaryentry) | always | Never nil, so a run that owes nobody renders an empty list rather than a null: it settles nothing, and a null would read as a report that does not say. |

#### `beneficiaryEntry`

beneficiaryEntry is what one partner is settled with in one currency: the total it is paid, the number of projects that total came off, and the rows it was summed from. Two currencies under one partner are two entries, because a sum over two of them is not a payout anybody can make.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `beneficiary` | string | always |  |
| `currency` | string | always |  |
| `kickback_total` | decimal, 2 places | always |  |
| `projects` | integer | always |  |
| `breakdown` | array of [kickbackEntry](#kickbackentry) | always |  |

#### `kickbackEntry`

kickbackEntry is one settled kickback under a partner: the statement it came off, the relation and the element it was computed from, and the three numbers the partner reconciles the payout against.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `cloud` | string | always |  |
| `project_id` | string | always | Two members for the reason json.go gives for the index entries: external project ids are unique per cloud only. |
| `relation_id` | string | always |  |
| `scope` | string | always |  |
| `rate` | decimal, 6 places | always |  |
| `base` | decimal, 2 places | always |  |
| `amount` | decimal, 2 places | always |  |
<!-- refdoc:end kickbacks -->

### Examples

What a regular run settles:

<!-- refdoc:begin example-kickbacks -->
```json
{
  "run_id": "3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4",
  "kind": "regular",
  "corrects_run_id": null,
  "period_from": "2026-03-01T00:00:00Z",
  "period_to": "2026-04-01T00:00:00Z",
  "beneficiaries": [
    {
      "beneficiary": "partner-corp",
      "currency": "EUR",
      "kickback_total": 172.00,
      "projects": 3,
      "breakdown": [
        {
          "cloud": "os-dr",
          "project_id": "customer-proj-1",
          "relation_id": "33333333-3333-3333-3333-333333333333",
          "scope": "openstack.instance",
          "rate": 0.050000,
          "base": 200.00,
          "amount": 10.00
        },
        {
          "cloud": "os-prod",
          "project_id": "customer-proj-1",
          "relation_id": "11111111-1111-1111-1111-111111111111",
          "scope": "all",
          "rate": 0.100000,
          "base": 1020.00,
          "amount": 102.00
        },
        {
          "cloud": "os-prod",
          "project_id": "customer-proj-2",
          "relation_id": "22222222-2222-2222-2222-222222222222",
          "scope": "all",
          "rate": 0.100000,
          "base": 500.00,
          "amount": 50.00
        },
        {
          "cloud": "os-prod",
          "project_id": "customer-proj-2",
          "relation_id": "55555555-5555-5555-5555-555555555555",
          "scope": "openstack.volume",
          "rate": 0.100000,
          "base": 100.00,
          "amount": 10.00
        }
      ]
    },
    {
      "beneficiary": "partner-two",
      "currency": "EUR",
      "kickback_total": 10.00,
      "projects": 1,
      "breakdown": [
        {
          "cloud": "os-prod",
          "project_id": "customer-proj-2",
          "relation_id": "44444444-4444-4444-4444-444444444444",
          "scope": "all",
          "rate": 0.020000,
          "base": 500.00,
          "amount": 10.00
        }
      ]
    }
  ]
}
```
<!-- refdoc:end example-kickbacks -->

What a correction of it owes on top:

<!-- refdoc:begin example-kickback-deltas -->
```json
{
  "run_id": "4b9d2c17-6e85-4f3a-8a01-c5d4e6f7a8b9",
  "kind": "correction",
  "corrects_run_id": "3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4",
  "period_from": "2026-03-01T00:00:00Z",
  "period_to": "2026-04-01T00:00:00Z",
  "beneficiaries": [
    {
      "beneficiary": "partner-corp",
      "currency": "EUR",
      "kickback_total": -52.04,
      "projects": 2,
      "breakdown": [
        {
          "cloud": "os-prod",
          "project_id": "customer-proj-1",
          "relation_id": "11111111-1111-1111-1111-111111111111",
          "scope": "all",
          "rate": 0.100000,
          "base": -20.40,
          "amount": -2.04
        },
        {
          "cloud": "os-prod",
          "project_id": "customer-proj-2",
          "relation_id": "22222222-2222-2222-2222-222222222222",
          "scope": "all",
          "rate": 0.100000,
          "base": -500.00,
          "amount": -50.00
        }
      ]
    }
  ]
}
```
<!-- refdoc:end example-kickback-deltas -->

## Rollup

`--rollup member_of` sums a run's statements under the meta-projects its
projects belong to, and `--rollup managed_by` under the partners that manage
them. One group is one target, and
[`internal/engine/export/rollup.go`](https://github.com/B42Labs/tally/blob/main/internal/engine/export/rollup.go)
sums it under these rules:

- The relations are walked one hop each. A meta-project that is itself a member
  of a second one is not followed, and its members are summed under it alone.
- A member is counted once per target, however many relations reach that target
  from it, so a membership closed and opened again inside the period is one
  member.
- A project is listed under every group it belongs to. Two groups are not
  disjoint, and their totals add up to more than the run billed.
- Nothing is summed for a project the run billed under another project's
  statement. Such a project has no statement of its own, and it is the root's
  membership that decides where its costs land.
- A rollup of a correction run sums that run's credit notes.
- A group's total is the sum of the member totals it lists, and the statements
  themselves are unchanged by it.

### The members

<!-- refdoc:begin rollup -->
#### `rollupDocument`

rollupDocument is `rollup-<key>.json:` which target the group sums under, the period and the kind of the run that produced it, and one entry per member beside the file that member's invoice was written to. Nothing here names the run itself, the way a statement document does not: run.json is the index that ties a file to its run. The field order is the order it is marshalled in.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `billing_period` | `statements.BillingPeriod` | always |  |
| `project_id` | string | always |  |
| `platform` | string | always |  |
| `relation_type` | string | always |  |
| `kind` | string | always |  |
| `corrects_run_id` | string or null | always | CorrectsRunID is the run a correction's rollup corrects. A pointer, so a regular run renders null the way runDocument and kickbacksDocument render the run they correct: one export's documents say the same thing about an absent value. |
| `members` | array of [rollupMemberEntry](#rollupmemberentry) | always |  |
| `total` | decimal, 2 places | always |  |
| `currency` | string | always |  |

#### `rollupMemberEntry`

rollupMemberEntry is one member under a group: the file its statement was written to, the pair it bills, and the total that statement carries. The two halves of the key are unescaped, the way the index carries them.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `file` | string | always |  |
| `cloud` | string | always |  |
| `project_id` | string | always |  |
| `total` | decimal, 2 places | always |  |
| `currency` | string | always |  |
<!-- refdoc:end rollup -->

### Examples

The index of a run exported with `--rollup` carries a `rollup` member naming
every group document:

<!-- refdoc:begin example-rollup-run-json -->
```json
{
  "run_id": "3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4",
  "kind": "regular",
  "corrects_run_id": null,
  "period_from": "2026-03-01T00:00:00Z",
  "period_to": "2026-04-01T00:00:00Z",
  "status": "finalized",
  "pricing_version": "2026-03",
  "clouds": [],
  "started_at": "2026-04-04T00:00:00Z",
  "completed_at": "2026-04-04T00:01:00Z",
  "stats": {},
  "statements": [
    {
      "file": "statement-os-dr%2Fproj-789.json",
      "cloud": "os-dr",
      "project_id": "proj-789",
      "total": 22.32,
      "currency": "EUR"
    },
    {
      "file": "statement-os-prod%2Fproj-456.json",
      "cloud": "os-prod",
      "project_id": "proj-456",
      "total": 128.45,
      "currency": "EUR"
    }
  ],
  "rollup": {
    "relation_type": "member_of",
    "documents": [
      {
        "file": "rollup-meta%2Fcustomer-alpha.json",
        "cloud": "meta",
        "project_id": "customer-alpha",
        "members": 1,
        "total": 128.45,
        "currency": "EUR"
      },
      {
        "file": "rollup-meta%2Fcustomer-beta.json",
        "cloud": "meta",
        "project_id": "customer-beta",
        "members": 2,
        "total": 150.77,
        "currency": "EUR"
      }
    ]
  }
}
```
<!-- refdoc:end example-rollup-run-json -->

One group of that run:

<!-- refdoc:begin example-rollup -->
```json
{
  "billing_period": {
    "from": "2026-03-01T00:00:00Z",
    "to": "2026-04-01T00:00:00Z"
  },
  "project_id": "customer-alpha",
  "platform": "meta",
  "relation_type": "member_of",
  "kind": "regular",
  "corrects_run_id": null,
  "members": [
    {
      "file": "statement-os-prod%2Fproj-456.json",
      "cloud": "os-prod",
      "project_id": "proj-456",
      "total": 128.45,
      "currency": "EUR"
    }
  ],
  "total": 128.45,
  "currency": "EUR"
}
```
<!-- refdoc:end example-rollup -->

## CSV tables

Every row carries the run and its period, so a row says which run and which
month it belongs to on its own: this format has no index beside the data the way
the JSON one does. `rated.csv`, `kickbacks.csv` and `rollup.csv` carry the run's
`kind` as well, and `deltas.csv` does not, because only a correction writes it.

The tables go through `encoding/csv`, so a field holding a comma, a quote or a
newline is quoted as RFC 4180 asks for. The separator is a comma and the line
ending is a line feed, wherever the export runs.

A free-text column whose value starts with `=`, `+`, `-`, `@`, a tab or a
carriage return is written with a leading apostrophe, which is what a
spreadsheet reads as "this is text" rather than as a formula. The numeric
columns do not take it: the leading minus of a credit is part of the number.

The header row of each table is its column order, and it is the first line of
each example below.

### `rated.csv`

<!-- refdoc:begin example-rated-csv -->
```csv
run_id,kind,corrects_run_id,period_from,period_to,cloud,platform,resource_type,resource_id,project_id,state,from_ts,to_ts,dimension,quantity,amount,currency
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,active,2026-03-01T00:00:00Z,2026-03-11T00:00:00Z,disk_gb,80.0000,19.20,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,active,2026-03-01T00:00:00Z,2026-03-11T00:00:00Z,egress_gb,18.0000,1.62,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,active,2026-03-01T00:00:00Z,2026-03-11T00:00:00Z,ram_gb,8.0000,9.60,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,active,2026-03-01T00:00:00Z,2026-03-11T00:00:00Z,vcpus,4.0000,19.20,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,shutoff,2026-03-11T00:00:00Z,2026-03-21T00:00:00Z,disk_gb,80.0000,9.60,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,shutoff,2026-03-11T00:00:00Z,2026-03-21T00:00:00Z,egress_gb,0.0000,0.00,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,shutoff,2026-03-11T00:00:00Z,2026-03-21T00:00:00Z,ram_gb,8.0000,4.80,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,shutoff,2026-03-11T00:00:00Z,2026-03-21T00:00:00Z,vcpus,4.0000,9.60,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,active,2026-03-21T00:00:00Z,2026-04-01T00:00:00Z,disk_gb,80.0000,21.12,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,active,2026-03-21T00:00:00Z,2026-04-01T00:00:00Z,egress_gb,22.5000,2.03,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,active,2026-03-21T00:00:00Z,2026-04-01T00:00:00Z,ram_gb,8.0000,10.56,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,active,2026-03-21T00:00:00Z,2026-04-01T00:00:00Z,vcpus,4.0000,21.12,EUR
```
<!-- refdoc:end example-rated-csv -->

### `deltas.csv`

<!-- refdoc:begin example-deltas-csv -->
```csv
run_id,corrects_run_id,period_from,period_to,cloud,platform,resource_type,resource_id,project_id,dimension,old_amount,new_amount,delta,currency
4b9d2c17-6e85-4f3a-8a01-c5d4e6f7a8b9,3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,disk_gb,59.52,49.92,-9.60,EUR
4b9d2c17-6e85-4f3a-8a01-c5d4e6f7a8b9,3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,ram_gb,29.76,24.96,-4.80,EUR
4b9d2c17-6e85-4f3a-8a01-c5d4e6f7a8b9,3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,os-prod,openstack,instance,abc-123,proj-456,vcpus,59.52,49.92,-9.60,EUR
```
<!-- refdoc:end example-deltas-csv -->

### `kickbacks.csv`

<!-- refdoc:begin example-kickbacks-csv -->
```csv
run_id,kind,corrects_run_id,period_from,period_to,beneficiary,cloud,project_id,relation_id,scope,rate,base,amount,currency
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,partner-corp,os-dr,customer-proj-1,33333333-3333-3333-3333-333333333333,openstack.instance,0.050000,200.00,10.00,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,partner-corp,os-prod,customer-proj-1,11111111-1111-1111-1111-111111111111,all,0.100000,1020.00,102.00,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,partner-corp,os-prod,customer-proj-2,22222222-2222-2222-2222-222222222222,all,0.100000,500.00,50.00,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,partner-corp,os-prod,customer-proj-2,55555555-5555-5555-5555-555555555555,openstack.volume,0.100000,100.00,10.00,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,partner-two,os-prod,customer-proj-2,44444444-4444-4444-4444-444444444444,all,0.020000,500.00,10.00,EUR
```
<!-- refdoc:end example-kickbacks-csv -->

### `rollup.csv`

<!-- refdoc:begin example-rollup-csv -->
```csv
run_id,kind,corrects_run_id,period_from,period_to,relation_type,target_cloud,target_project_id,cloud,project_id,total,currency
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,member_of,meta,customer-alpha,os-prod,proj-456,128.45,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,member_of,meta,customer-beta,os-dr,proj-789,22.32,EUR
3f1e6a58-9c24-4d0b-8f77-2a5c1b93e0d4,regular,,2026-03-01T00:00:00Z,2026-04-01T00:00:00Z,member_of,meta,customer-beta,os-prod,proj-456,128.45,EUR
```
<!-- refdoc:end example-rollup-csv -->

## See also

Every example on this page is a golden file of the export tests, under
[`internal/engine/export/testdata/golden/`](https://github.com/B42Labs/tally/blob/main/internal/engine/export/testdata/golden).
The [tally-engine](/reference/command-line/tally-engine) page states the flags
of `export` and `kickbacks`, and the
[pricing adjustments](/reference/formats/pricing-adjustments) page states where
the settled amounts come from.

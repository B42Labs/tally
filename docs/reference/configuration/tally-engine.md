---
title: Engine settings
description: Every environment variable the metering engine reads.
quadrant: reference
audience: operator
---

# Engine settings

Every subcommand of `tally-engine` loads the same configuration and then runs
the gate that fits it. Every setting comes from the environment, and every
variable Tally defines carries the prefix `TALLY_`. Section 8 of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md)
states that rule.

A secret is given inline or as a path to a file. The File-backed column names
the companion variable, and
[`internal/core/envsecret`](https://github.com/B42Labs/tally/blob/main/internal/core/envsecret/envsecret.go)
applies it: the companion is the variable's name plus the suffix `_FILE`, and a
companion that holds a path makes that file's content the value. One trailing
newline is trimmed, which is what Kubernetes writes into a Secret volume. An
empty file is refused, and so is setting a variable and its `_FILE` companion at
the same time.

`TALLY_LOG_LEVEL` takes `DEBUG`, `INFO`, `WARN` or `ERROR`. The match is exact,
so a lower-case `info` is refused.

## Settings

<!-- refdoc:begin settings -->
| Variable | Type | Default | File-backed | Governs |
| --- | --- | --- | --- | --- |
| `TALLY_LOG_LEVEL` | string | `INFO` | no | LogLevel is the slog threshold, one of DEBUG, INFO, WARN, or ERROR. |
| `TALLY_ENGINE_DB_URL` | string | none | yes (`TALLY_ENGINE_DB_URL_FILE`) | DBURL is the PostgreSQL connection string of the engine's own database, which holds the runs and their records. It has no default because a guessed database is worse than none. Supports the *_FILE convention. |
| `TALLY_ENGINE_REPORTING_DB_URL` | string | none | yes (`TALLY_ENGINE_REPORTING_DB_URL_FILE`) | ReportingDBURL is the PostgreSQL connection string of the Reporting API's database, which the engine reads events and resources from. It has no default for the same reason as DBURL. Migration 0008 of the reporting chain creates the group role tally_engine_reader and grants it SELECT on the four tables metering reads; a deployment connects as a login role that is a member of it. Supports the *_FILE convention. |
| `TALLY_ENGINE_VM_URL` | string | none | no | VMURL is the base URL of the VictoriaMetrics instance the metricsql counter sources are queried against. It is needed only when the counter sources file declares metricsql sources. It has no default because the endpoint differs per deployment, and no gate here: the first subcommand that queries VictoriaMetrics adds its own. |
| `TALLY_ENGINE_GRACE_HOURS` | integer | `72` | no | GraceHours is how long a run waits after its billing period ends before it executes, so events that arrive late still reach the run that bills them. Zero is a grace window of no hours, which runs the period the moment it closes. |
| `TALLY_ENGINE_AUTO_FINALIZE` | boolean | `false` | no | AutoFinalize lets a completed run finalize itself. It defaults to false because a finalized run is immutable and its data may reach an ERP, so the step gets a human gate. |
| `TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES` | list, comma-separated | `infrastructure_tenant` | no | AttributingRelationTypes are the relation types attribution walks when it bills a project under its attributor; an empty list bills every project on its own. Setting the variable to the empty string yields that empty list. "member_of" and "managed_by" reach a virtual project and attribute no cost, so a list naming either is refused. |
| `TALLY_ENGINE_ADJUSTMENT_RELATION_TYPES` | list, comma-separated | `managed_by,member_of` | no | AdjustmentRelationTypes are the relation types adjustment resolution walks from a statement's project to collect the pricing adjustments that apply to it, "managed_by" and "member_of" by default. Setting the variable to the empty string yields the empty list, which turns adjustments off. A type in both lists is walked by attribution and by adjustment resolution. |
| `TALLY_ENGINE_ADJUSTMENT_DEPTH` | integer | `3` | no | AdjustmentDepth is how many relation levels the walk follows from the statement's project; 1 is the project's own relations. It is at least 1. |
| `TALLY_ENGINE_COUNTER_SOURCES` | string | `/etc/tally/counter-sources.yaml` | no | CounterSourcesPath is the path to the counter sources YAML, the file that declares which counters exist and how each one is measured; cmd/tally-engine/counter-sources.example.yaml shows the format. Setting the variable to the empty string means no counter sources, which counters.Load reads as the zero configuration; a path to a file that does not exist is an error when the file is read, not a run without counters. It has no *_FILE companion because the value is a path already, not a secret. Whether the file parses is checked by the package that reads it, not here. |
<!-- refdoc:end settings -->

## What is checked

`Load` runs for every subcommand. It resolves the two file-backed secrets and
then checks:

- `TALLY_LOG_LEVEL` is one of the four levels above.
- `TALLY_ENGINE_GRACE_HOURS` is not negative. Zero is a grace window of no
  hours, which runs a period the moment it closes.
- `TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES` carries no empty entry. The
  variable set to the empty string is the empty list, which bills every project
  on its own; an empty entry anywhere else is a stray comma and is refused.
- `TALLY_ENGINE_ATTRIBUTING_RELATION_TYPES` names neither `member_of` nor
  `managed_by`. Both reach a virtual project and attribute no cost.
- `TALLY_ENGINE_ADJUSTMENT_RELATION_TYPES` carries no empty entry, and is read
  the same way: the empty string is the empty list, which turns adjustments off.
  No relation type is refused here, so a type named in both lists is walked by
  attribution and by adjustment resolution.
- `TALLY_ENGINE_ADJUSTMENT_DEPTH` is at least 1.
- `TALLY_ENGINE_COUNTER_SOURCES` set to the empty string means no counter
  sources. An unset variable keeps the default path.

`ValidateDB` is the gate of the subcommands backed by the engine's own
database. It adds `TALLY_ENGINE_DB_URL` is set.

`ValidateReporting` is the gate of the subcommands that read the Reporting
API's database. It adds `TALLY_ENGINE_REPORTING_DB_URL` is set.

`TALLY_ENGINE_VM_URL` has no gate in this package. The subcommand that queries
VictoriaMetrics brings its own.

## Example

[`cmd/tally-engine/.env.example`](https://github.com/B42Labs/tally/blob/main/cmd/tally-engine/.env.example)
lists every variable with its default and a comment. The
[counter sources file](/reference/configuration/counter-sources-file) page
states the format of the file `TALLY_ENGINE_COUNTER_SOURCES` points at.

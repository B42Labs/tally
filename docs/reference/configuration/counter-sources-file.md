---
title: Counter sources file
description: The YAML file that declares the counter metrics the engine measures per usage interval, and how each one is measured.
quadrant: reference
audience: operator
---

# Counter sources file

`TALLY_ENGINE_COUNTER_SOURCES` names the file the metering engine reads its
counter sources from. It defaults to `/etc/tally/counter-sources.yaml`. The
variable set to the empty string means no counter sources, and the engine then
measures none. A path that cannot be read is an error rather than a run without
counters, so a misconfigured path stops the run instead of dropping a billed
metric from every usage record of the period.

A counter is measured per usage interval of a resource, so a counter is sliced
wherever metering split that resource.
[`internal/engine/counters/counters.go`](https://github.com/B42Labs/tally/blob/main/internal/engine/counters/counters.go)
reads the file and checks every entry.

## Entries

The document holds one key, `sources:`, whose value is a list of entries in file
order. An empty document, one that holds only comments, and one without
`sources` each yield no counter sources and no error. Only the first YAML
document of the file is read; a second one is refused.

### The entry

<!-- refdoc:begin entry -->
#### `sourceFile`

sourceFile is one entry of that file. Required is a pointer so that an absent key can be told from required: false.

| Member | Type | Presence | Description |
| --- | --- | --- | --- |
| `platform` | string | always |  |
| `resource_type` | string | always |  |
| `metric` | string | always |  |
| `kind` | string | always |  |
| `event_type` | string | always |  |
| `query` | string | always |  |
| `required` | boolean or null | always |  |
<!-- refdoc:end entry -->

The presence column is the Go struct's rather than the file's: an entry carries
the keys its `kind` asks for and leaves the rest out.

`platform`, `resource_type` and `metric` are set on every entry. Any key the
entry does not declare is refused with the key named, so a misspelled
`event_typ` fails the run instead of leaving a source measuring nothing.

`kind` is `events` or `metricsql`. An events source counts the events of its
`event_type` that the resource recorded inside the interval, read from the
reporting database. A metricsql source runs its `query` as an instant query
against VictoriaMetrics at the interval's end. An events source sets
`event_type` and no `query`; a metricsql source sets `query` and no
`event_type`.

`required` applies to metricsql sources only and defaults to true. A failing
required source fails the run. `required: false` turns that failure into a
warning and leaves the metric out of that one interval. The key on an events
source is refused.

A query may use four placeholders: `{cloud}`, `{resource_id}`, `{project_id}`
and `{window}`. Any other placeholder is refused with its name. `{window}` is
the interval's length, rendered like `360h`, `90m` or `61s`.

The three identity values come from ingested event data and are substituted as
they are rather than escaped into the query. `inertIdentity` in
[`internal/engine/counters/vm.go`](https://github.com/B42Labs/tally/blob/main/internal/engine/counters/vm.go)
is what they are held against: a value carrying a character outside the ASCII
letters, the digits and `.`, `_`, `:`, `/`, `@` and `-` is refused when the
query is rendered. A quote, a backtick, a space and a brace are among the
characters that refusal covers.

Two rules keep such a value out of the places a MetricsQL query reads it as a
pattern, and both are checked when the file is read. A query may not match
`{cloud}`, `{resource_id}` or `{project_id}` with `=~` or `!~`. A query that
substitutes one of the three may not call `label_replace`, `label_transform`,
`label_match` or `label_mismatch`, whose pattern argument reads the value the
same way without a matcher to mark it.

The metric names `minutes` and `count` are reserved by the engine, which derives
both itself. One `(platform, resource_type, metric)` triple appears at most
once.

## Example

[`cmd/tally-engine/counter-sources.example.yaml`](https://github.com/B42Labs/tally/blob/main/cmd/tally-engine/counter-sources.example.yaml)
declares one source of each kind.

<!-- refdoc:begin example -->
```yaml
# Counter sources of the metering engine, read from the path
# TALLY_ENGINE_COUNTER_SOURCES names (default
# /etc/tally/counter-sources.yaml). A counter is measured per usage interval of
# a resource, so a counter is sliced wherever metering split that resource.
#
# An events source counts the events of its event_type the resource recorded
# inside the interval, read from the reporting database. A metricsql source
# runs its query as an instant query against VictoriaMetrics
# (TALLY_ENGINE_VM_URL, needed only when a metricsql source exists) at the
# interval's end; an empty result is 0 and the query has to aggregate to a
# single series. Either way the value is billed at four decimal places, so a
# metric measured one way and later the other keeps one shape.
#
# A query may use four placeholders: {cloud}, {resource_id}, {project_id} (the
# project of the interval) and {window} (the interval's length, rendered like
# 360h, 90m or 61s; an interval shorter than a second is measured over 1s). The
# three identity values come from ingested event data and are substituted as
# they are rather than escaped into the query, so one holding a character
# MetricsQL reads as syntax -- a quote, a backtick, a space, a brace -- is
# refused. That refusal fails a required source like any other failure, because
# a resource id is not what may decide whether a billed counter is measured; an
# optional source is warned under counter_identity_not_queryable instead, which
# is its own code because no rerun clears it. The values are inert as a literal
# only: a query may not match one with =~ or !~, where the value is read as a
# pattern and the dot an id may hold matches any character, and it may not call
# label_replace, label_transform, label_match or label_mismatch, whose pattern
# argument reads it the same way without a matcher to mark it.
#
# required applies to metricsql sources only and defaults to true: a failing
# required source fails the run. required: false turns that failure into a
# counter_source_failed warning and leaves the metric out of that interval,
# which is what a metric that is only reported wants.
#
# The keys of an entry are platform, resource_type, metric and kind, plus
# event_type for an events source and query (with optional required) for a
# metricsql source. Any other key is refused. The metric names minutes and
# count are reserved by the engine, and one (platform, resource_type, metric)
# may appear once.
#
# The series name in the first entry is the roadmap's illustration. A
# deployment queries the series name its own Ceilometer pipeline stores; see
# docs/openstack-metrics.md.

sources:
  - platform: openstack
    resource_type: instance
    metric: egress_gb
    kind: metricsql
    query: >
      sum(increase(ceilometer_network_outgoing_bytes{cloud="{cloud}",
          resource_id="{resource_id}"}[{window}])) / 1e9
  - platform: harbor
    resource_type: repository
    metric: pulls
    kind: events
    event_type: repository.pull
```
<!-- refdoc:end example -->

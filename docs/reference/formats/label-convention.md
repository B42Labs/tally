---
title: Label convention
description: The vocabulary and the labels every event, metric series and exporter in Tally carries.
quadrant: reference
audience: integrator
---

# Label convention

Tally names one thing one way across the events, the database, the metrics and
the exports. The words below are the vocabulary, and the two rules under them
are what the vocabulary exists for.

## Vocabulary

Section 3 of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md)
fixes these terms.

| Term | Meaning |
| --- | --- |
| platform | The platform type: `openstack`, `hetzner`, `stackit`, `ionos`, `gardener`, `harbor`, and later `meta` and `partner`. |
| cloud | One concrete installation of a platform, such as `os-prod-eu1`. Two OpenStack clouds share a `platform` and differ in `cloud`. |
| resource key | The triple `(cloud, resource_type, resource_id)`. A resource id is unique within that triple alone. |
| event | An immutable lifecycle fact, appended and deduplicated on `event_id`. The `events` table is the single source of truth. |
| projection | `current_resources`: derived, rebuildable state, never authoritative. |
| collector | A provider-side service that pushes events with `source` `collector`. |
| reconciliation | A server-side periodic sync that emits synthetic events with `source` `reconciliation`. |
| timeline, interval | The per-resource sequence of half-open intervals `[from, to)` with a constant `(state, size, project_id)`, folded from the resource's event history. |
| usage record | Metering output: one interval clipped to a billing period, with a `usage` object. |
| rated record | Rating output: money per dimension per usage record. |
| run | One versioned metering and rating execution, `regular` or `correction`. |
| billing period | A calendar month in UTC, `[first day 00:00:00Z, first day of the next month 00:00:00Z)`. |

## The two rules

`cloud` belongs in every key, join, lock and cache. A resource id is unique per
cloud only, so a key without the cloud puts two installations' resources on top
of each other. The
[architecture and the provider pattern](/explanation/architecture-and-the-provider-pattern)
page states what the rule buys.

`event_id` is the idempotency key. It is what a redelivery, a replayed batch and
a restarted collector are deduplicated on, and it is the one identifier a
provider is asked to produce or derive.

## Labels on metric series

Every series Tally exposes is named `tally_` and then the subsystem it belongs
to, such as `tally_events_ingested_total` and `tally_collector_consumed_total`.

`platform` and `cloud` are variable labels on the series they apply to. A
reporting series that counts per installation carries `cloud`, and one that
counts per platform and resource type carries `platform`. The collector's own
series carry neither: one collector process serves one cloud, so the pair is a
static label of its scrape job instead.

A third-party exporter is labelled the same way from the outside. The job that
scrapes it sets `platform` and `cloud` statically, which is what puts its
samples in the same coordinate system as Tally's own.

`project_id` and `resource_id` are the labels of a per-resource series. Neither
is a label Tally sets on its own counters, which stay bounded by cardinality;
they are what an exporter's per-resource series is relabelled into.

## Mapping an OpenStack exporter to the convention

An OpenStack database exporter names the owning project `tenant_id` and the
resource `uuid` or `id`. The block below is the `metric_relabel_configs` a
deployment adds to its `openstack-db-exporter` job to bring those names into the
convention. The base defines that job in
[`deploy/kubernetes/base/victoriametrics/scrape.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/victoriametrics/scrape.yaml).

```yaml
  - job_name: openstack-db-exporter
    scrape_interval: 300s
    scrape_timeout: 60s
    static_configs:
      - targets: ["os-db-exporter:9180"]
        labels: { platform: "openstack", cloud: "os-prod-eu1" }
    metric_relabel_configs:
      # tenant_id is the upstream name for the owning project on the nova,
      # cinder and glance series.
      - source_labels: [tenant_id]
        regex: (.+)
        target_label: project_id
      - regex: tenant_id
        action: labeldrop
      # uuid first, id second: the nova server_status series carries the same
      # value under both, and the neutron port series carries uuid alone.
      - source_labels: [uuid]
        regex: (.+)
        target_label: resource_id
      - source_labels: [id]
        regex: (.+)
        target_label: resource_id
      - regex: uuid|id
        action: labeldrop
```

Relabeling renames a label and cannot add one the exporter never emitted, and a
rule whose source label is absent leaves the sample as it was. The neutron port
series therefore ends up with `resource_id` read from its `uuid` and with no
`project_id` at all. The `openstack_nova_quota_*` series name their project by
name, under `tenant`, so they get no `project_id` either.

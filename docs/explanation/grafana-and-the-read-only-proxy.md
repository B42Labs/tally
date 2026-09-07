---
title: Grafana and the read-only proxy
description: Why Grafana reaches the metrics store through a filtering proxy, and why its datasource, provider and dashboards are provisioned from files.
quadrant: explanation
audience: all
---

# Grafana and the read-only proxy

The four dashboards, their uids and the series each of them reads are on
[the dashboards reference page](/reference/observability/dashboards), and the
steps are in
[install the Grafana dashboards](/how-to/observability/install-the-grafana-dashboards).
This page is why the provisioning has the shape it has.

## The datasource and the proxy

[`provisioning/datasources/vm.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/grafana/provisioning/datasources/vm.yaml)
declares one datasource: `VictoriaMetrics`, type `prometheus`, and the default.
Its uid is fixed at `victoriametrics` because every panel target in the
dashboard JSON names that uid; a generated one would leave each panel pointing
at a datasource Grafana does not have. `editable: false` keeps the UI from
offering edits that the next pod restart discards.

It is proxied to `http://127.0.0.1:8427` rather than to
`http://victoriametrics:8428`, and that is a deliberate hop. Grafana forwards a
caller-supplied path to the datasource URL from two endpoints that check
`datasources:query` alone, `/api/datasources/proxy/uid/<uid>/<path>` and
`/api/datasources/uid/<uid>/resources/<path>`, and the viewer role holds that
permission. The second is what fills every variable dropdown, so neither the
route nor Grafana can withhold it, and whatever the datasource URL answers is
reachable by anyone who can open a dashboard. The address is therefore a
[vmauth](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/grafana/vmauth/auth.yaml)
container beside Grafana in the same pod, listening on that pod's loopback: it
carries the Prometheus read paths on to `http://victoriametrics:8428` and
answers every other path, `/api/v1/write` and
`/api/v1/admin/tsdb/delete_series` included, with `missing route`. Widening the
list in `vmauth/auth.yaml` widens what a viewer reaches, which is what
[`manifest_test.go`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/grafana/manifest_test.go)
pins.

## The dashboard provider

[`provisioning/dashboards/tally.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/grafana/provisioning/dashboards/tally.yaml)
declares one file provider, `tally`. It reads `/var/lib/grafana/dashboards` and
puts what it finds in the folder `Tally`. `disableDeletion: true` and
`allowUiUpdates: false` say where the dashboards live: an edit or a delete made
in the UI would last until the next pod restart, so neither is allowed.

## The variables and the test that pins them

Each uid is written in the file rather than generated, because a saved link and
a dashboard link address a dashboard by it.

Every dashboard carries the multi-select variables `platform` and `cloud`, both
read off `tally_current_resources` and both with an "All" option whose value is
`.*`. `project-drilldown.json` adds `project_id`, read off the exporter's
`tenant_id` label, and the `api_base` textbox its event link is built from.

[`dashboards_test.go`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/grafana/dashboards_test.go)
pins that contract in CI: the file set the ConfigMap ships, that every file
parses, the uids and the titles, the datasource uid on every query target, the
variables the expressions read, and the drift note on the reconciliation
dashboard. Grafana reports a broken dashboard file in its own log alone, so
without the test a truncated file or a renamed datasource reaches a cluster
before anyone sees it.

## What the route publishes

The route publishes the whole host bar one prefix: `/api/datasources/proxy`
answers 403 from the Gateway. The dashboards do not use it, they query through
`/api/ds/query`, so it is refused rather than published. That rule is the outer
of two rings and the weaker one: the sibling endpoint
`/api/datasources/uid/<uid>/resources` forwards a caller-supplied path the same
way and cannot be denied without emptying every variable dropdown, and a
percent-encoded spelling of the prefix reaches Grafana as the decoded path
regardless. What bounds the tunnel is the far end, the read-only datasource URL
described under [the datasource and the proxy](#the-datasource-and-the-proxy).

## What a fresh cluster shows

On a cluster nothing has reported to, every panel backed by an exporter series
or a collector series shows "No data" rather than a query error. The `platform`
and `cloud` variables read `label_values(tally_current_resources, ...)`, which
resolves to nothing while the projection is empty. What keeps an empty variable
from breaking a panel is its "All" value `.*`: a selector `cloud=~".*"` matches
whatever the store holds, including nothing.

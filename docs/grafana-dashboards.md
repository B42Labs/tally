# Grafana dashboards

Grafana reads the metrics store and serves four dashboards over it. Its
datasource, its dashboard provider, and the dashboard files all come from this
repository, so a pod that restarts comes back serving what the tree says. This
document describes what is provisioned, how to reach it on a dev cluster, and
how to fill all four dashboards with data.

## What is provisioned

### The datasource

[`provisioning/datasources/vm.yaml`](../deploy/kubernetes/base/grafana/provisioning/datasources/vm.yaml)
declares one datasource: `VictoriaMetrics`, type `prometheus`, and the default.
Its uid is fixed at `victoriametrics` because every panel target in the
dashboard JSON names that uid; a generated one would leave each panel pointing
at a datasource Grafana does not have. `editable: false` keeps the UI from
offering edits that the next pod restart discards.

It is proxied to `http://127.0.0.1:8427` rather than to
`http://victoriametrics:8428`, and that is a deliberate hop. Grafana forwards a
caller-supplied path to the datasource URL from two endpoints that check
`datasources:query` alone — `/api/datasources/proxy/uid/<uid>/<path>` and
`/api/datasources/uid/<uid>/resources/<path>` — and the viewer role holds that
permission. The second is what fills every variable dropdown, so neither the
route nor Grafana can withhold it, and whatever the datasource URL answers is
reachable by anyone who can open a dashboard. The address is therefore a
[vmauth](../deploy/kubernetes/base/grafana/vmauth/auth.yaml) container beside
Grafana in the same pod, listening on that pod's loopback: it carries the
Prometheus read paths on to `http://victoriametrics:8428` and answers every
other path, `/api/v1/write` and `/api/v1/admin/tsdb/delete_series` included,
with `missing route`. Widening the list in `vmauth/auth.yaml` widens what a
viewer reaches, which is what
[`manifest_test.go`](../deploy/kubernetes/base/grafana/manifest_test.go) pins.

### The dashboard provider

[`provisioning/dashboards/tally.yaml`](../deploy/kubernetes/base/grafana/provisioning/dashboards/tally.yaml)
declares one file provider, `tally`. It reads `/var/lib/grafana/dashboards` and
puts what it finds in the folder `Tally`. `disableDeletion: true` and
`allowUiUpdates: false` say where the dashboards live: an edit or a delete made
in the UI would last until the next pod restart, so neither is allowed.

### The four dashboards

Four files under
[`dashboards/`](../deploy/kubernetes/base/grafana/dashboards), each mounted into
the pod from the generated `grafana-dashboards` ConfigMap:

| File | uid | Series it reads |
| --- | --- | --- |
| `fleet-overview.json` | `tally-fleet-overview` | `tally_current_resources`, `openstack_identity_projects`, `openstack_nova_limits_instances_used` |
| `project-drilldown.json` | `tally-project-drilldown` | the per-project nova, cinder, neutron, glance and octavia series, plus `tally_events_ingested_total` |
| `ingestion-health.json` | `tally-ingestion-health` | `tally_events_ingested_total`, `tally_events_deduplicated_total`, `tally_events_rejected_total`, `tally_projection_replays_total`, `tally_collector_buffer_depth`, `tally_collector_oldest_buffered_seconds`, `up` |
| `reconciliation-drift.json` | `tally-reconciliation-drift` | `tally_sync_resources_reconciled_total`, `tally_sync_errors_total`, `tally_sync_runs_total` |

Each uid is written in the file rather than generated, because a saved link and
a dashboard link address a dashboard by it.

Every dashboard carries the multi-select variables `platform` and `cloud`, both
read off `tally_current_resources` and both with an "All" option whose value is
`.*`. `project-drilldown.json` adds `project_id`, read off the exporter's
`tenant_id` label, and the `api_base` textbox its event link is built from.

[`dashboards_test.go`](../deploy/kubernetes/base/grafana/dashboards_test.go)
pins that contract in CI: the file set the ConfigMap ships, that every file
parses, the uids and the titles, the datasource uid on every query target, the
variables the expressions read, and the drift note on the reconciliation
dashboard. Grafana reports a broken dashboard file in its own log alone, so
without the test a truncated file or a renamed datasource reaches a cluster
before anyone sees it.

### Editing a provisioned file

The datasource, the provider, and the dashboards are three
`configMapGenerator` entries in
[`kustomization.yaml`](../deploy/kubernetes/base/grafana/kustomization.yaml).
Kustomize appends a content hash to each generated ConfigMap name, so editing
any of those files changes the name, which changes the pod spec that mounts it,
which rolls the pod. Grafana is never left serving a datasource or a dashboard
that no longer matches the file in the tree. It is the pattern the collector and
scrape configs follow, described under
[Editing either config](openstack-metrics.md#editing-either-config).

## Dev access

`make up` publishes Grafana on the dev overlay's hostname:

```text
https://grafana.tally.127-0-0-1.nip.io:8443
```

The overlay patches the HTTPRoute to that hostname and sets
`GF_SERVER_ROOT_URL` to the same URL including the `:8443` host port, because
kind publishes https there
([`deploy/kind/kind.yaml`](../deploy/kind/kind.yaml)) and a link Grafana renders
without the port sends the browser to a closed port.

The reachability check needs the dev CA that signed the Gateway's certificate:

```sh
make -s ca > tally-ca.crt
curl --cacert tally-ca.crt https://grafana.tally.127-0-0-1.nip.io:8443/api/health
```

It answers 200 with a JSON body whose `database` member is `ok`.

The overlay sets `GF_AUTH_ANONYMOUS_ENABLED` to `"true"` and
`GF_AUTH_ANONYMOUS_ORG_ROLE` to `Viewer`, so a request without a session renders
every dashboard and saves nothing. `admin` signs in with
`tally-dev-grafana-password`, the `admin-password` key of the generated
`tally-grafana` Secret
([`overlays/dev/kustomization.yaml`](../deploy/kubernetes/overlays/dev/kustomization.yaml)).

The route publishes the whole host bar one prefix: `/api/datasources/proxy`
answers 403 from the Gateway. The dashboards do not use it — they query through
`/api/ds/query` — so it is refused rather than published. That rule is the outer
of two rings and the weaker one: the sibling endpoint
`/api/datasources/uid/<uid>/resources` forwards a caller-supplied path the same
way and cannot be denied without emptying every variable dropdown, and a
percent-encoded spelling of the prefix reaches Grafana as the decoded path
regardless. What bounds the tunnel is the far end, the read-only datasource URL
described under [The datasource](#the-datasource).

The base sets no `GF_AUTH_ANONYMOUS_*` variable and leaves the hostname at the
placeholder `grafana.tally.example.com`. Without an overlay saying otherwise,
Grafana serves a login page.

## What a fresh cluster shows

On a cluster nothing has reported to, every panel backed by an exporter series
or a collector series shows "No data" rather than a query error. The `platform`
and `cloud` variables read `label_values(tally_current_resources, ...)`, which
resolves to nothing while the projection is empty. What keeps an empty variable
from breaking a panel is its "All" value `.*`: a selector `cloud=~".*"` matches
whatever the store holds, including nothing.

The scrape-health stat on Tally / Ingestion Health reports `up == 0` for
`openstack-db-exporter` and `ceilometer`. Both are static targets for exporters
that run beside an OpenStack control plane rather than in this cluster. That is
the designed dev state, described under
[Replacing the placeholders](openstack-metrics.md#replacing-the-placeholders),
and not a fault to chase.

## Acceptance drill

The drill fills all four dashboards on a dev cluster, in two parts. The first
pushes real events through the Reporting API, which is what moves the `tally_`
series and the projection the variables read. The second pushes the OpenStack
exporter series the resource panels read, which no dev cluster produces, to the
OTLP endpoint.

Everything below runs from the repository root against a running dev cluster and
uses the dev CA:

```sh
make -s ca > tally-ca.crt
```

### Part 1: real series through the Reporting API

Ingest is authenticated per (platform, cloud), so the first step issues a
credential. The command prints the raw token to stdout and the id plus the
one-time notice to stderr, which is what lets the token be captured while the
notice stays readable:

```sh
export TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
TOKEN="$(go run ./cmd/tally-reporting-admin create-ingest-credential \
  --platform openstack --cloud os-prod-eu1)"
```

Push a small batch for cloud `os-prod-eu1` and project `drill-project`. The
fields are the canonical event schema of
[`roadmap/00-conventions.md`](../roadmap/00-conventions.md) section 4, and each
`payload.size` validates against the schema the migration chain seeds for its
resource type:

```sh
curl --cacert tally-ca.crt -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  'https://api.tally.127-0-0-1.nip.io:8443/api/v1/events' \
  --data-binary @- <<'EOF'
[
  {
    "event_id": "drill-instance-create",
    "timestamp": "2026-08-19T09:00:00Z",
    "event_type": "compute.instance.create.end",
    "platform": "openstack",
    "cloud": "os-prod-eu1",
    "resource_type": "instance",
    "resource_id": "drill-instance-1",
    "project_id": "drill-project",
    "source": "collector",
    "payload": {
      "state": "active",
      "size": {"vcpus": 4, "ram_gb": 16, "disk_gb": 80, "flavor": "m1.large"}
    }
  },
  {
    "event_id": "drill-volume-create",
    "timestamp": "2026-08-19T09:05:00Z",
    "event_type": "volume.create.end",
    "platform": "openstack",
    "cloud": "os-prod-eu1",
    "resource_type": "volume",
    "resource_id": "drill-volume-1",
    "project_id": "drill-project",
    "source": "collector",
    "payload": {
      "state": "available",
      "size": {"size_gb": 100, "type": "ssd"}
    }
  }
]
EOF
```

The answer names what the batch did:

```json
{"accepted": 2, "duplicates": 0, "rejected": []}
```

That moves `tally_events_ingested_total` and, once the refresher has run,
`tally_current_resources`, which is the series both variables are read from.

Send the identical batch a second time. Ingestion is idempotent on
`(event_id, timestamp)`, so nothing is stored and the dedup counter moves:

```json
{"accepted": 0, "duplicates": 2, "rejected": []}
```

Send one create without `payload.size`, alone. The request itself is authorized
and readable, so the call is still answered 200; the item is refused inside the
batch and kept server-side under the reason it was refused, which
`GET /api/v1/rejected-events` serves:

```sh
curl --cacert tally-ca.crt -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  'https://api.tally.127-0-0-1.nip.io:8443/api/v1/events' \
  --data-binary @- <<'EOF'
{
  "event_id": "drill-volume-sizeless",
  "timestamp": "2026-08-19T09:10:00Z",
  "event_type": "volume.create.end",
  "platform": "openstack",
  "cloud": "os-prod-eu1",
  "resource_type": "volume",
  "resource_id": "drill-volume-2",
  "project_id": "drill-project",
  "source": "collector",
  "payload": {"state": "available"}
}
EOF
```

```json
{
  "accepted": 0,
  "duplicates": 0,
  "rejected": [
    {
      "index": 0,
      "event_id": "drill-volume-sizeless",
      "reason": "schema: payload.size: required on create events"
    }
  ]
}
```

`tally_events_rejected_total` counts that under the check that refused it, so
the reason label reads `schema` rather than the full sentence.

Send one update on the instance whose timestamp lies before its create. The
incremental fold cannot place a late event, so the ingest transaction replays
the resource's projection row from its whole history and
`tally_projection_replays_total` counts the replay:

```sh
curl --cacert tally-ca.crt -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  'https://api.tally.127-0-0-1.nip.io:8443/api/v1/events' \
  --data-binary @- <<'EOF'
{
  "event_id": "drill-instance-late-update",
  "timestamp": "2026-08-19T08:30:00Z",
  "event_type": "compute.instance.update",
  "platform": "openstack",
  "cloud": "os-prod-eu1",
  "resource_type": "instance",
  "resource_id": "drill-instance-1",
  "project_id": "drill-project",
  "source": "collector",
  "payload": {"state": "active"}
}
EOF
```

```json
{"accepted": 1, "duplicates": 0, "rejected": []}
```

The instance history now starts with an update, which the fold reports as the
warning `history_starts_without_create` on
`GET /api/v1/resources/os-prod-eu1/instance/drill-instance-1/lifecycle`. That is
the drill's doing, not drift.

The counters reach the store on the next scrape of the `reporting-api` job,
which runs every 30 seconds. `tally_current_resources` takes longer: the gauge
is re-derived from the projection every `TALLY_REPORTING_METRICS_REFRESH_S`
seconds, 60 by default, and the scrape follows that, so the fleet panels and the
two variables fill within about 90 seconds of the first batch.

### Part 2: synthetic series through OTLP

The OpenStack panels read the database exporter's series, and no dev cluster
runs that exporter. One OTLP push writes exactly the series those panels read
and no others.

The project label differs per service, and the payload follows that: nova,
cinder and glance name the owning project `tenant_id`, neutron and octavia name
it `project_id`, and `openstack_identity_projects` carries no project label at
all (see the coverage table under
[Coverage against the concept](openstack-metrics.md#coverage-against-the-concept)).
The `CLOUD`, `TENANT`, and `PROJECT` attribute sets below carry that difference.
The exporter's per-resource labels, `id` and `name` among them, are left out:
the panels count series and filter on the cloud and the project, so nothing
reads them.

The three `tally_sync_*` series are cumulative monotonic sums with two
datapoints two minutes apart and growing values, because the panels read them
through `increase()` and a single point has no slope. Their label values are the
ones the reconciliation service emits: `completed` or `failed` for a run status,
and `created`, `updated` or `deleted` for a reconciled resource.

```sh
NOW="$(date +%s)000000000"
THEN="$(( $(date +%s) - 120 ))000000000"
CLOUD='[{"key":"platform","value":{"stringValue":"openstack"}},{"key":"cloud","value":{"stringValue":"os-prod-eu1"}}]'
TENANT='[{"key":"platform","value":{"stringValue":"openstack"}},{"key":"cloud","value":{"stringValue":"os-prod-eu1"}},{"key":"tenant_id","value":{"stringValue":"drill-project"}}]'
PROJECT='[{"key":"platform","value":{"stringValue":"openstack"}},{"key":"cloud","value":{"stringValue":"os-prod-eu1"}},{"key":"project_id","value":{"stringValue":"drill-project"}}]'
RUNS='[{"key":"cloud","value":{"stringValue":"os-prod-eu1"}},{"key":"status","value":{"stringValue":"completed"}}]'
RECONCILED='[{"key":"cloud","value":{"stringValue":"os-prod-eu1"}},{"key":"action","value":{"stringValue":"created"}}]'
ERRORS='[{"key":"cloud","value":{"stringValue":"os-prod-eu1"}}]'

curl --cacert tally-ca.crt -X POST \
  --user tally:tally-dev-otlp-password \
  -H 'Content-Type: application/json' \
  'https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics' \
  --data-binary @- <<EOF
{"resourceMetrics":[{"scopeMetrics":[{"metrics":[
{"name":"openstack_identity_projects","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$CLOUD}]}},
{"name":"openstack_nova_limits_instances_used","gauge":{"dataPoints":[{"asDouble":3,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
{"name":"openstack_nova_limits_instances_max","gauge":{"dataPoints":[{"asDouble":10,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
{"name":"openstack_nova_limits_vcpus_used","gauge":{"dataPoints":[{"asDouble":12,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
{"name":"openstack_nova_limits_vcpus_max","gauge":{"dataPoints":[{"asDouble":40,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
{"name":"openstack_nova_limits_memory_used","gauge":{"dataPoints":[{"asDouble":24576,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
{"name":"openstack_nova_limits_memory_max","gauge":{"dataPoints":[{"asDouble":81920,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
{"name":"openstack_nova_server_status","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
{"name":"openstack_cinder_volume_status","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
{"name":"openstack_cinder_volume_gb","gauge":{"dataPoints":[{"asDouble":100,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
{"name":"openstack_glance_image_bytes","gauge":{"dataPoints":[{"asDouble":21474836480,"timeUnixNano":"$NOW","attributes":$TENANT}]}},
{"name":"openstack_neutron_floating_ip","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$PROJECT}]}},
{"name":"openstack_neutron_router","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$PROJECT}]}},
{"name":"openstack_loadbalancer_loadbalancer_status","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"$NOW","attributes":$PROJECT}]}},
{"name":"tally_collector_buffer_depth","gauge":{"dataPoints":[{"asDouble":12,"timeUnixNano":"$NOW"}]}},
{"name":"tally_collector_oldest_buffered_seconds","gauge":{"dataPoints":[{"asDouble":4,"timeUnixNano":"$NOW"}]}},
{"name":"tally_sync_runs_total","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[
  {"asDouble":4,"timeUnixNano":"$THEN","attributes":$RUNS},
  {"asDouble":5,"timeUnixNano":"$NOW","attributes":$RUNS}]}},
{"name":"tally_sync_resources_reconciled_total","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[
  {"asDouble":11,"timeUnixNano":"$THEN","attributes":$RECONCILED},
  {"asDouble":13,"timeUnixNano":"$NOW","attributes":$RECONCILED}]}},
{"name":"tally_sync_errors_total","sum":{"aggregationTemporality":2,"isMonotonic":true,"dataPoints":[
  {"asDouble":2,"timeUnixNano":"$THEN","attributes":$ERRORS},
  {"asDouble":3,"timeUnixNano":"$NOW","attributes":$ERRORS}]}}
]}]}]}
EOF
```

The push answers 200 with an empty OTLP export response, `{}` or
`{"partialSuccess":{}}`. Anything the collector rejects comes back as a 4xx with
the reason in the body.

Two costs of this part, both accepted:

- The synthetic series stay in the dev store for the 13-month retention
  VictoriaMetrics runs with, and nothing ages the points out inside a year.
  Removing them means reaching the pod directly:
  `/api/v1/admin/tsdb/delete_series` is on neither the read route the Gateway
  publishes nor the read paths the Grafana datasource goes through, and it takes
  the `-deleteAuthKey` that the generated `tally-vm-admin` Secret holds.
  `make down` deletes the cluster and its volume, and that is the shorter way
  out.
- The drill is for dev clusters. Its labels claim a cloud, a project, and
  reconciliation runs that never happened, so running it against a store any
  invoice is derived from puts invented usage into the billing record.

### Closing checks

Open `https://grafana.tally.127-0-0-1.nip.io:8443`, go to the folder `Tally`,
and select `os-prod-eu1` in the Cloud variable. On Tally / Project Drilldown
select `drill-project` in the Project variable, which the drill's
`openstack_nova_limits_instances_used` series supplies. Every panel on all four
dashboards then carries a value. The panels built on `rate()` and `increase()`
show the drill as one spike over their window rather than a level, because the
drill pushed each series once.

On the scrape-health stat, `openstack-db-exporter` and `ceilometer` report
`up == 0` while `reporting-api` and `otel-collector` report `up == 1`. Those two
jobs stay down for the reason above: the designed dev state, not a failure.

The same OTLP push without `--user` answers `401` and
`{"code":16,"message":"no basic auth provided"}`, and writes nothing. That is
the check that the receivers are not open to whoever resolves the hostname.

A panel that is still empty within 30 seconds of a push has lost no point.
VictoriaMetrics evaluates queries behind wall clock by `-search.latencyOffset`,
30 seconds by default, so a query over the window still being written comes back
empty; the dashboards refresh every minute, and the panel fills on one of the
next refreshes. The full explanation is in the
[acceptance drill](openstack-metrics.md#acceptance-drill) of the metrics
pipeline.

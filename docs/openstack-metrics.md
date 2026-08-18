# OpenStack metrics pipeline

Two paths carry numbers into Tally's metrics store and both end in
VictoriaMetrics. On the push path a producer speaks OTLP to the OpenTelemetry
Collector, which remote-writes what it received. On the pull path
VictoriaMetrics scrapes four jobs on its own schedule. Once a sample is
written the two are indistinguishable: a query cannot tell which path a series
arrived on.

## The pipeline

### What is pushed

The collector receives OTLP on both transports,
[`config.yaml`](../deploy/kubernetes/base/otel-collector/config.yaml): gRPC on
`0.0.0.0:4317` and HTTP on `0.0.0.0:4318`. Both are published through the
Gateway, gRPC by a `GRPCRoute` and HTTP by an `HTTPRoute`
([`otel-collector.yaml`](../deploy/kubernetes/base/otel-collector/otel-collector.yaml)),
each on a hostname of its own. The base names `otlp.tally.example.com` and
`otlp-grpc.tally.example.com`; the dev overlay patches both to the nip.io
domain, and https is published on host port 8443
([`deploy/kind/kind.yaml`](../deploy/kind/kind.yaml)), so on a dev cluster the
two endpoints are:

```text
https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics
https://otlp-grpc.tally.127-0-0-1.nip.io:8443
```

Both transports demand HTTP Basic credentials, and that is what keeps those two
hostnames from being a write interface to the billing store for whoever resolves
them: a request without credentials is refused with 401 before it reaches the
pipeline. The collector reads its users from an htpasswd file mounted from the
`tally-otlp-auth` Secret, so no credential lives in this tree; the dev overlay
generates `tally:tally-dev-otlp-password` for the drill at the end of this
document, and a real deployment puts a bcrypt hash in that Secret instead. The
file is read at startup, so a rotated Secret takes effect when the pod rolls,
which editing the Secret through the overlay does.

Basic and not a bearer token because of the publisher at the other end:
Ceilometer's OTLP publisher takes credentials from its target URL's userinfo and
has no option for an arbitrary header (see the publishing path below).

Inside the collector one pipeline handles metrics: the OTLP receivers feed
`memory_limiter`, then `batch`, which groups the points, and the
`prometheusremotewrite` exporter writes them to
`http://victoriametrics:8428/api/v1/write`. Neither processor adds, drops, or
renames a label, so a point is stored under the labels its producer gave it.

`memory_limiter` is first because that is the only position it can push back
from: it refuses a batch at the receiver rather than after the collector has
buffered it. Everything behind it holds data — `batch` by design, and
`prometheusremotewrite` in its retry queue — so an unreachable VictoriaMetrics
would otherwise grow the collector's heap until the node kills the pod, and
every buffered point would be gone. That is not a remote case: the store is one
replica on an RWO volume, so editing either config file rolls it and writes fail
for as long as it takes to come back. `memory_limiter` limits by percentage of
the container's memory limit, which is why `otel-collector.yaml` sets one.

VictoriaMetrics keeps 13 months, `-retentionPeriod=13` in
[`victoriametrics.yaml`](../deploy/kubernetes/base/victoriametrics/victoriametrics.yaml),
which is a full billing year plus the current month.

### What is scraped

VictoriaMetrics reads its own scrape config,
`-promscrape.config=/etc/vm/scrape.yaml`, and four jobs are configured in
[`scrape.yaml`](../deploy/kubernetes/base/victoriametrics/scrape.yaml):

| Job | Interval | Target | What it carries |
| --- | --- | --- | --- |
| `reporting-api` | 30s | discovered: the `http` port of the `reporting-api` endpoints | The Reporting API's service metrics: event counts and the aggregations derived from them. |
| `openstack-db-exporter` | 300s, timeout 60s | `os-db-exporter:9180` | Inventory read straight out of the OpenStack service databases. |
| `ceilometer` | 60s | `ceilometer-exporter:9101` | Ceilometer samples on the fallback publishing path (see below). |
| `otel-collector` | 15s | discovered: the `metrics` port of the `otel-collector` endpoints | The collector's own telemetry: what it accepted, what it exported, what it refused. |

The two OpenStack jobs carry static `platform` and `cloud` labels, because
third-party exporters do not know Tally's label convention (README section 3.1,
[`roadmap/00-conventions.md`](../roadmap/00-conventions.md) section 3). The
other two jobs carry no such labels: they export service metrics, not provider
resource metrics.

The two in-cluster jobs discover their targets through the Kubernetes API
(`kubernetes_sd_configs`, role `endpointslice`) rather than naming a Service
address. A Service address is one ClusterIP that kube-proxy resolves to an
arbitrary backend per connection, so a second replica of either Deployment would
leave every scrape landing on a different pod while all samples carry the same
`instance` label. One series would then interleave two independent counters,
`rate()` would read each decrease as a counter reset, and nothing would look
wrong: the target stays up and nothing is logged. Discovery gives every pod a
target and an `instance` label of its own. It reads endpointslices plus the pod
and service objects the labels come from, in its own namespace alone, which is
what the ServiceAccount, Role, and RoleBinding in
[`victoriametrics.yaml`](../deploy/kubernetes/base/victoriametrics/victoriametrics.yaml)
grant.

Both jobs relabel `instance` from the pod name. The default is `__address__`,
which for this role is the pod IP and port, and a pod IP goes back to the pool
when the pod is deleted and is handed to the next pod that needs one. That pod
would then continue the series its predecessor wrote — the same interleaving of
two counters in one series that discovery is here to avoid, arrived at from the
other direction. A pod name carries a random suffix and is not reused.

Discovery also changes what a broken job looks like. `up` is a synthetic series
per target, so a job that resolves to no targets produces no `up` series rather
than `up == 0`: a Deployment scaled to zero, a renamed Service, a renamed port,
a removed RoleBinding, or an unreachable API server all take the job off
`/targets` instead of turning it red. A pod that exists and refuses the
connection is still caught, because endpointslice discovery keeps not-ready
addresses as targets. Alerting on these two jobs therefore takes two rules, one
on `up == 0` and one on `absent(up{job="..."})`; both are in
[`roadmap/02-phase-2-reporting-dashboards.md`](../roadmap/02-phase-2-reporting-dashboards.md).

The database exporter's job is the one with an explicit `scrape_timeout`. Every
scrape of it runs the exporter's whole query set against the live OpenStack
service databases — 21 neutron tables alone, with `GROUP_CONCAT` and
`TIMESTAMPDIFF` over `ports`, `ipallocations`, and `standardattributes`. On a
cloud with six figures of ports that set outlasts the 10s default by a wide
margin, and dropping the HTTP request does not stop the queries: nothing in a
scrape path kills a server-side query. A short interval would then start a
second set on top of the first until the control plane's own database is
saturated and Nova, Neutron, and Keystone start timing out, with a flapping
target as the only symptom on Tally's side. 60s is what one scrape may cost,
300s leaves it room to finish, and the limits on the exporter's database user
(below) bound what a scrape that outruns both can still occupy.

### Editing either config

`config.yaml` and `scrape.yaml` are not applied as files. Each is the source of
a `configMapGenerator` in its component's `kustomization.yaml`, so kustomize
appends a content hash to the generated ConfigMap name. Editing either file
changes that name, which changes the pod spec that mounts it, which rolls the
pod. Neither service is left serving a config that no longer matches the file
in the tree.

### Replacing the placeholders

`os-db-exporter:9180`, `ceilometer-exporter:9101`, and the cloud name
`os-prod-eu1` are placeholders. Those exporters run beside an OpenStack control
plane and not in this cluster, so on dev both jobs stay down.

A real deployment does not patch the file. Its overlay declares a
`configMapGenerator` of its own for the same generated name and marks it
`behavior: replace`, which is the kustomize mechanism for overriding a generated
ConfigMap:

```yaml
configMapGenerator:
  - name: victoriametrics-scrape
    behavior: replace
    files:
      - scrape.yaml
```

The overlay's own `scrape.yaml` then names that deployment's exporter addresses
and its cloud.

### What is exposed and what is not

Three hostnames of this pipeline are attached to the Gateway's `https` listener,
which is the stack's ingress from outside the cluster
([`gateway.yaml`](../deploy/kubernetes/base/gateway/gateway.yaml)). They carry
different amounts of authority:

- The two OTLP hostnames accept metrics from whoever holds the Basic credentials
  above, and refuse everything else. What they accept is written to the billing
  store under the producer's own labels, which is why they are not open. Both
  are rate limited at the Gateway to 60 requests a second by the
  `BackendTrafficPolicy` in
  [`otel-collector.yaml`](../deploy/kubernetes/base/otel-collector/otel-collector.yaml),
  because refusing a request is the expensive half of Basic auth: the username
  is published here and in the publisher URL below, so every wrong password
  costs the collector a bcrypt comparison — tens of milliseconds of CPU against
  one replica's 100m request — while costing the sender nothing. The proxy
  answers a request over the limit with 429 before the collector sees it.
- `vm.tally.example.com` publishes VictoriaMetrics' read paths alone:
  `/api/v1/query`, `/api/v1/query_range`, `/targets`, and `/vmui`. The route
  matches those prefixes and nothing else, so `/api/v1/write` and
  `/api/v1/admin/tsdb/delete_series` are not reachable through the Gateway.
  VictoriaMetrics gates neither of them: it takes a remote write from anything
  that reaches it, and with no `-deleteAuthKey` set one request to the delete
  API drops a match of every series in the 13-month window. One consequence of
  the narrow route is that vmui's autocomplete, which calls `/api/v1/label/...`
  and `/api/v1/series`, stays empty; queries themselves work.

What the published read paths do not carry is a credential. Anything that
reaches `vm.` can read every series, and with it every project id, resource id,
and instance name in the store. On dev that hostname resolves to 127.0.0.1 and
the Gateway is bound to the developer's own machine
([`deploy/kind/kind.yaml`](../deploy/kind/kind.yaml)); a deployment that
publishes it anywhere else puts an authenticating proxy in front of it or drops
the route.

Inside the cluster nothing is gated, because `deploy/` carries no NetworkPolicy:
a pod can reach VictoriaMetrics' write and delete APIs, the collector's OTLP
ports, and the Reporting API's `/metrics`
([`internal/reporting/httpapi/metrics.go`](../internal/reporting/httpapi/metrics.go)),
which is the state that route documents. That gap belongs to the deployment as a
whole rather than to this pipeline, and it is not closed here.

The collector's port 8888 is published on the Service, so VictoriaMetrics can
scrape it, and no route sends it through the Gateway: it is reachable from
inside the cluster alone.

## The Ceilometer publishing path

Ceilometer reaches Tally on one of two paths: it posts OTLP to the collector, or
it pushes to a gateway that the `ceilometer` scrape job reads. Which path a
deployment takes depends on its Ceilometer version, so the version is the first
thing to establish.

### Establishing what the deployment can publish

The OTLP publisher exists upstream as
`ceilometer.publisher.opentelemetry_http:OpentelemetryHttpPublisher`. It arrived
in [commit c972cec](https://github.com/openstack/ceilometer/commit/c972cecb37ff82acc0b4e9caa2808e69edb297c2)
("Add opentelemetry publisher base on http", committed 2023-12-18), during the
2024.1 "Caracal" cycle. In
[Ceilometer's `setup.cfg`](https://github.com/openstack/ceilometer/blob/9ce091102513190dc08cdc67f8e077112129c192/setup.cfg)
at commit `9ce0911` the `ceilometer.sample.publisher` group lists both entry
points this document uses:

```ini
prometheus = ceilometer.publisher.prometheus:PrometheusPublisher
opentelemetryhttp = ceilometer.publisher.opentelemetry_http:OpentelemetryHttpPublisher
```

A Ceilometer older than 2024.1 has the `prometheus` entry point and not the
`opentelemetryhttp` one. Read the entry points of the installed package rather
than inferring them from a release name, because a distribution can carry
backports:

```sh
python3 -m pip show ceilometer
python3 -c 'from importlib.metadata import entry_points; \
  print(sorted(e.name for e in entry_points(group="ceilometer.sample.publisher")))'
```

The second command prints the publisher names this installation can resolve. A
list containing `opentelemetryhttp` is what the OTLP path needs; a list without
it settles the question, whatever the version string says.

### Publishing OTLP to the collector

On this path Ceilometer posts to the collector's OTLP/HTTP endpoint and nothing
scrapes it. One publisher in the pipeline's `sinks` is the whole configuration,
in `/etc/ceilometer/pipeline.yaml`:

```yaml
sources:
  - name: meter_source
    meters:
      - "cpu"
      - "memory.usage"
      - "disk.device.read.bytes"
      - "disk.device.write.bytes"
      - "network.incoming.bytes"
      - "network.outgoing.bytes"
    sinks:
      - tally
sinks:
  - name: tally
    publishers:
      - opentelemetryhttp://tally:<password>@otlp.tally.example.com:443/v1/metrics?ssl=true
```

The meter list is a subscription rather than a filter applied later, so `"*"`
would subscribe to every meter this Ceilometer polls for every resource in the
cloud, and the resulting series count would be set by the size of the cloud
instead of by what Tally bills. The store is one 5Gi volume with 13 months of
retention, no expansion and no capacity alert, and when it fills it stops
accepting writes on the push path and all four scrape jobs at once, with nothing
ageing out for a year. The list above is the instance runtime data README
section 4.1 names; check the names against the deployment's own `polling.yaml`
before using it, because which meters exist depends on the pollsters that
deployment runs.

The userinfo in the URL is how this publisher authenticates. It inherits the
HTTP publisher's URL handling, which turns `user:password@` into HTTP Basic
credentials and strips it from the target address; there is no option for an
arbitrary header, which is why the collector's receivers ask for Basic. That
same URL handling uses the URL scheme as the transport scheme only for `http`
and `https`; for any other scheme, including `opentelemetryhttp`, it falls back
to plain HTTP unless the `ssl` parameter says otherwise, so `?ssl=true` is
required for a TLS endpoint — and without it the credentials would go over plain
HTTP.

This path leaves the labels incomplete. The publisher attaches exactly three
attributes per sample, `resource_id`, `user_id`, and `project_id`, so nothing in
what it sends says which platform or which cloud the sample came from. The
static labels in `scrape.yaml` do not help: they belong to a scrape job, and a
pushed sample passes no scrape job. A deployment on this path attaches `platform`
and `cloud` itself, either in the publisher's configuration or with a processor
in its own collector config:

```yaml
processors:
  attributes/tally:
    actions:
      - key: platform
        value: openstack
        action: insert
      - key: cloud
        value: os-prod-eu1
        action: insert
```

The processor then goes into that deployment's metrics pipeline, ahead of the
exporter.

### Publishing to a Pushgateway that is scraped

On the fallback path Ceilometer pushes to a Prometheus Pushgateway and
VictoriaMetrics scrapes the Pushgateway. The `ceilometer` job already points at
it, so this path needs no change to `scrape.yaml`:

```yaml
sources:
  - name: meter_source
    meters:
      - "cpu"
      - "memory.usage"
      - "disk.device.read.bytes"
      - "disk.device.write.bytes"
      - "network.incoming.bytes"
      - "network.outgoing.bytes"
    sinks:
      - tally
sinks:
  - name: tally
    publishers:
      - prometheus://ceilometer-exporter:9101/metrics/job/ceilometer
```

The `/metrics/job/<job>` segment is what the Pushgateway API takes. With no
grouping label after the job segment the publisher pushes once per resource and
appends `/resource_id/<id>` to the path itself, so two samples of different
resources do not overwrite one another. Each sample is written with the labels
`resource_id`, `user_id`, and `project_id`, so two of the convention's five
labels are already right, and `platform` and `cloud` come from the job's static
labels when VictoriaMetrics scrapes. Neither path supplies `resource_type`. It
has to come from the meter name or from a rule the deployment writes.

What this path does not do is forget a resource. A group per `resource_id` is
created on the first push and the Pushgateway expires nothing — a group goes
away on an explicit `DELETE` or on a restart without persistence, and on nothing
else. When a tenant deletes an instance Ceilometer simply stops pushing for that
`resource_id`, the last sample stays in the gateway, and VictoriaMetrics keeps
scraping it every 60 seconds and stamps each scrape with the current time. The
series never goes stale, so a billing rule that reads usage from the presence of
a series over an interval keeps charging for a resource that no longer exists,
and no alert sees it. The group count grows with resource churn as a second
effect, until the response outgrows the scrape and every Ceilometer series
disappears at once.

A deployment on this path therefore runs a reaper: the deletion events the
event collector already consumes drive
`DELETE /metrics/job/ceilometer/resource_id/<id>` against the Pushgateway. The
OTLP path has no gateway and needs none.

### The recorded default

The default is the `prometheus` publisher. The OTLP path is preferable, one hop
shorter and with no Pushgateway to operate, but no OpenStack deployment was
available to establish that its Ceilometer carries the `opentelemetryhttp` entry
point. The default changes for a deployment whose version check above comes back
positive (author decision, 2026-08-17). Recording the Pushgateway as the default
records the reaper with it: without something that removes the group of a
deleted resource, that path overcharges by construction.

## Database exporter evaluation

This is a source-level evaluation. No OpenStack deployment was available to run
the exporter against, so the evidence is the upstream code of
[vexxhost/openstack_database_exporter](https://github.com/vexxhost/openstack_database_exporter),
read at commit `9d4895323e98c7752889abae5df4ed923cfae2b4`. Every file path and
metric name below is relative to that commit. Nothing here was observed on a
running exporter.

Metric names are built from a namespace and a per-service subsystem. The
namespace is `openstack` (`internal/collector/collector.go`). Two subsystems do
not match their service name: keystone's is `identity`
(`internal/collector/keystone/keystone.go`) and octavia's is `loadbalancer`
(`internal/collector/octavia/octavia.go`), so the keystone metrics read
`openstack_identity_*` and the octavia metrics `openstack_loadbalancer_*`.

### Coverage against the concept

One row per requirement of README section 4.3.

| Requirement | Verdict | Upstream file | Exposed metrics |
| --- | --- | --- | --- |
| Nova instances | covered | `internal/collector/nova/server.go` | `openstack_nova_server_status{id,uuid,tenant_id,status,flavor_id,availability_zone,host_id,hypervisor_hostname,name,user_id,instance_libvirt,address_ipv4,address_ipv6}`, `openstack_nova_server_local_gb{id,name,tenant_id}`, `openstack_nova_total_vms`, `openstack_nova_availability_zones` |
| Nova flavors | covered | `internal/collector/nova/flavors.go` | `openstack_nova_flavor{disk,id,is_public,name,ram,vcpus}`, `openstack_nova_flavors`, `openstack_nova_security_groups` |
| Nova quotas | partial | `internal/collector/nova/quotas.go`, `internal/collector/nova/limits.go` | `openstack_nova_quota_instances`, `openstack_nova_quota_cores`, `openstack_nova_quota_ram` and eleven further `openstack_nova_quota_*` series, all labelled `{domain_id,tenant,type}` and carrying no project id. `internal/collector/nova/limits.go` covers the same three resources with a project id: `openstack_nova_limits_instances_max`, `openstack_nova_limits_instances_used`, `openstack_nova_limits_vcpus_max`, `openstack_nova_limits_vcpus_used`, `openstack_nova_limits_memory_max`, `openstack_nova_limits_memory_used`, all labelled `{domain_id,tenant,tenant_id}` |
| Cinder volumes | covered | `internal/collector/cinder/volumes.go` | `openstack_cinder_volume_status{id,name,status,bootable,tenant_id,size,volume_type,server_id}`, `openstack_cinder_volumes`, `openstack_cinder_volume_status_counter{status}`, `openstack_cinder_up` |
| Cinder volume sizes | covered | `internal/collector/cinder/volumes.go` | `openstack_cinder_volume_gb{id,name,status,availability_zone,bootable,tenant_id,user_id,volume_type,server_id}` |
| Neutron floating IPs | covered | `internal/collector/neutron/floating_ips.go` | `openstack_neutron_floating_ip{floating_ip_address,floating_network_id,id,project_id,router_id,status}`, `openstack_neutron_floating_ips`, `openstack_neutron_floating_ips_associated_not_active` |
| Neutron ports | partial | `internal/collector/neutron/ports.go` | `openstack_neutron_port{admin_state_up,binding_vif_type,device_owner,fixed_ips,mac_address,network_id,status,uuid}`, `openstack_neutron_ports`, `openstack_neutron_ports_lb_not_active`, `openstack_neutron_ports_no_ips`. No project label: the `GetPorts` query in `sql/neutron/queries.sql` does not select `p.project_id` |
| Neutron routers | covered | `internal/collector/neutron/router_metrics.go` | `openstack_neutron_router{admin_state_up,external_network_id,id,name,project_id,status}`, `openstack_neutron_routers`, `openstack_neutron_routers_not_active` |
| Keystone projects | covered | `internal/collector/keystone/projects.go` | `openstack_identity_projects`, `openstack_identity_project_info{description,domain_id,enabled,id,is_domain,name,parent_id,tags}` |
| Glance images | covered | `internal/collector/glance/images.go` | `openstack_glance_image_bytes{id,name,tenant_id}`, `openstack_glance_image_created_at{id,name,tenant_id,visibility,hidden,status}`, `openstack_glance_images`, `openstack_glance_up` |
| Octavia load balancers | covered | `internal/collector/octavia/loadbalancer.go` | `openstack_loadbalancer_loadbalancer_status{id,name,project_id,operating_status,provisioning_status,provider,vip_address}`, `openstack_loadbalancer_total_loadbalancers`, `openstack_loadbalancer_up` |

The router metrics are in `router_metrics.go`, not in the file named
`routers.go`. That file holds a different collector, the L3 agent binding one
that exposes `openstack_neutron_l3_agent_of_router` and `openstack_neutron_up`.

### Mapping the labels to the convention

The upstream labels are close to Tally's convention but not identical. Three
services name the owning project `tenant_id` (nova servers, cinder volumes,
glance images) and three name it `project_id` (neutron floating IPs and routers,
octavia load balancers). The resource is `id` everywhere except the neutron port
series, which names it `uuid`; the nova `server_status` series carries the
instance UUID under both `id` and `uuid`.

A real deployment renames them on its `openstack-db-exporter` job. The rename
belongs in `metric_relabel_configs`, which acts on the labels of scraped
samples, and not in `relabel_configs`, which acts on the target's own label set
before the scrape:

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

A rule whose source label is absent does not match `(.+)` and leaves the sample
alone, so every series keeps whatever it had. That is also the bound on what
relabeling can do here: it renames labels and cannot add one the exporter never
emitted, which is why the two partial rows above stay partial. The neutron port
series gets no `project_id`, and the `openstack_nova_quota_*` series name their
project by name (`tenant`) and not by id.

### The read-only database user

The exporter reads one DSN per service from `<SERVICE>_DATABASE_URL` in oslo.db
format (`cmd/openstack-database-exporter/main.go`), for example
`NOVA_DATABASE_URL`, `NOVA_API_DATABASE_URL`, `CINDER_DATABASE_URL`. A service
whose variable is unset is skipped, and it logs that it skipped it. Nova takes
both of its variables or none of its collectors register, because the instance
rows and the flavor rows live in different databases. The same file defaults the
listen address to `:9180` and the metrics path to `/metrics`, which is where the
scrape target `os-db-exporter:9180` comes from.

The table lists below are the tables the `sql/<service>/queries.sql` files
select from, at the pinned commit. The syntax is MySQL/MariaDB, which is what
those queries are written against (backtick quoting, `TIMESTAMPDIFF`,
`GROUP_CONCAT`):

```sql
-- The host is the subnet the exporter runs in; replace it with the deployment's
-- own. '%' would take a connection from every host that reaches the database
-- port, and the grants below read the full tenant inventory of the cloud —
-- keystone.user and keystone.project, nova.instances, neutron.ports and
-- securitygroups, octavia.vip — from one password with no origin check. Add
-- REQUIRE SSL where the control plane's database serves TLS.
--
-- MAX_USER_CONNECTIONS bounds what a scrape that outruns its timeout can
-- occupy (see the scrape job above), and the floor it has to clear is one
-- connection pool per DSN. The exporter opens eight — nova, nova_api, cinder,
-- neutron, keystone, glance, octavia, placement — as this one account, each
-- pool keeps idle connections of its own, and the registry gathers the
-- collectors in parallel, so a scrape needs more than eight at once. A cap
-- below that does not bound the pile-up, it truncates the scrape: the pools
-- that lose the race get ERROR 1226, their collectors emit nothing, and the
-- exporter still answers 200 with up == 1, so every scrape is silently short
-- by a different set of services. Neither the target nor the scrape duration
-- shows it, which is why the alert for it reads the series themselves
-- (TallyExporterServiceSilent in
-- roadmap/02-phase-2-reporting-dashboards.md). 24 leaves each pool room and
-- still caps a runaway.
--
-- A cap on connections alone bounds how many queries pile up, not how long each
-- one holds its connection, and the queries outlive the scrape that started
-- them: nothing in a scrape path kills a server-side query, so a slow database
-- has scrape N still running when N+1 opens its own pools. MAX_STATEMENT_TIME
-- is what ends them. It is MariaDB's spelling and takes seconds; MySQL has no
-- per-account form, so there the cap is max_execution_time in milliseconds, set
-- on the server or on the exporter's sessions, which bounds read-only SELECTs
-- and so covers everything this account is granted. 30 sits under the job's 60s
-- scrape_timeout; a deployment whose slowest single query needs longer raises
-- it rather than let the cap truncate the scrape the way too low a connection
-- cap does.
CREATE USER 'tally_exporter'@'10.0.0.0/255.255.255.0' IDENTIFIED BY '<password>'
  WITH MAX_USER_CONNECTIONS 24 MAX_STATEMENT_TIME 30;

-- sql/nova/queries.sql
GRANT SELECT ON nova.instances TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON nova.services TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON nova.compute_nodes TO 'tally_exporter'@'10.0.0.0/255.255.255.0';

-- sql/nova_api/queries.sql
GRANT SELECT ON nova_api.flavors TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON nova_api.quotas TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON nova_api.quota_classes TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON nova_api.quota_usages TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON nova_api.aggregates TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON nova_api.aggregate_hosts TO 'tally_exporter'@'10.0.0.0/255.255.255.0';

-- sql/cinder/queries.sql
GRANT SELECT ON cinder.volumes TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON cinder.volume_types TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON cinder.volume_attachment TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON cinder.snapshots TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON cinder.quotas TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON cinder.quota_usages TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON cinder.services TO 'tally_exporter'@'10.0.0.0/255.255.255.0';

-- sql/neutron/queries.sql
GRANT SELECT ON neutron.routers TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.ports TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.floatingips TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.networks TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.networksegments TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.externalnetworks TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.networkrbacs TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.subnets TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.subnetpools TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.subnetpoolprefixes TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.dnsnameservers TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.ipallocations TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.ipallocationpools TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.ml2_port_bindings TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.securitygroups TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.securitygrouprules TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.standardattributes TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.tags TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.agents TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.ha_router_agent_port_bindings TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON neutron.quotas TO 'tally_exporter'@'10.0.0.0/255.255.255.0';

-- sql/keystone/queries.sql
GRANT SELECT ON keystone.project TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON keystone.project_tag TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON keystone.user TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON keystone.region TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON keystone.`group` TO 'tally_exporter'@'10.0.0.0/255.255.255.0';

-- sql/glance/queries.sql
GRANT SELECT ON glance.images TO 'tally_exporter'@'10.0.0.0/255.255.255.0';

-- sql/octavia/queries.sql
GRANT SELECT ON octavia.load_balancer TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON octavia.vip TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON octavia.pool TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON octavia.amphora TO 'tally_exporter'@'10.0.0.0/255.255.255.0';

-- sql/placement/queries.sql. Not in the concept's database table, but the nova
-- quota and limit collectors read usage from placement and report zero usage
-- without it.
GRANT SELECT ON placement.resource_providers TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON placement.inventories TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON placement.resource_classes TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON placement.allocations TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON placement.consumers TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON placement.projects TO 'tally_exporter'@'10.0.0.0/255.255.255.0';
GRANT SELECT ON placement.users TO 'tally_exporter'@'10.0.0.0/255.255.255.0';

FLUSH PRIVILEGES;
```

One thing `SELECT` alone does not cover: the volume query in
`sql/cinder/queries.sql` carries a `USE INDEX (volumes_service_uuid_idx)` hint,
and that index is created by `sql/cinder/indexes.sql` rather than by cinder
itself. A database that never ran that file fails the query, and a read-only
user cannot create the index. Applying `sql/cinder/indexes.sql`,
`sql/nova/indexes.sql`, and `sql/nova_api/indexes.sql` is a one-time job for an
account that may write DDL.

### Extend upstream, or supplement it

The concept asks to evaluate the existing exporter first and to extend it if
necessary (README section 4.3). The decision is to run it unchanged and to write
no supplementary exporter.

The evidence is the coverage table: nine of eleven requirements are covered, and
the two partial rows do not touch a resource type Tally meters.

- Neutron ports carry no project label. Tally bills instances, volumes, floating
  IPs, images, and load balancers, all of which are covered with a project label.
  A port is not a billed resource, and the port count remains readable per
  cluster.
- The `openstack_nova_quota_*` series name the project by name rather than by id.
  Quotas are not billed either, and `openstack_nova_limits_*` covers instances,
  vCPUs, and memory with `tenant_id` for the case where a project id is wanted.

Should either gap start to matter, the fix is small and belongs upstream rather
than in a fork: adding `p.project_id` to the `GetPorts` query and the label to
the port metric, and adding the project id to the quota descriptors. Deciding that
now would mean carrying a fork for output nothing reads.

## Acceptance drill

The drill pushes one point through the whole push path and reads it back out of
the store. It runs against a dev cluster from the host, so it needs the dev CA
that signed the Gateway's certificate.

```sh
make -s ca > tally-ca.crt
NOW="$(date +%s)000000000"
curl --cacert tally-ca.crt -X POST \
  --user tally:tally-dev-otlp-password \
  'https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics' \
  -H 'Content-Type: application/json' \
  -d '{"resourceMetrics":[{"scopeMetrics":[{"metrics":[{"name":"tally_metrics_drill","gauge":{"dataPoints":[{"asDouble":1,"timeUnixNano":"'"$NOW"'"}]}}]}]}]}'
curl --cacert tally-ca.crt \
  'https://vm.tally.127-0-0-1.nip.io:8443/api/v1/query?query=tally_metrics_drill'
```

The push answers HTTP 200 with an empty OTLP export response, `{}` or
`{"partialSuccess":{}}`. Anything the collector rejects comes back as a 4xx with
the reason in the body. The same call without `--user` answers `401` and
`{"code":16,"message":"no basic auth provided"}`, which is the check that the
receivers are not open.

The query answers with the one series, within 30 seconds of the push:

```json
{"status":"success","data":{"resultType":"vector","result":[
  {"metric":{"__name__":"tally_metrics_drill"},"value":[1787035962,"1"]}]}}
```

`__name__` is the only label. The payload carries no resource attributes, so the
remote-write exporter derives no `job` or `instance` label from it.

The wait is expected, not a fault. VictoriaMetrics evaluates instant queries
behind wall clock by `-search.latencyOffset`, 30 seconds by default, so that a
query does not read a window still being written. A query run immediately after
the push can therefore return an empty result while the point is already stored.
Repeat the query after the offset has passed before treating an empty answer as
a failure.

Target health is a separate page, `/targets`, and the same CA file reaches it:

```sh
curl --cacert tally-ca.crt 'https://vm.tally.127-0-0-1.nip.io:8443/targets'
```

Four jobs are listed. `reporting-api` and `otel-collector` are up.
`openstack-db-exporter` and `ceilometer` are down with an unresolved-host error,
because neither target exists in this cluster. That is the designed dev state
and not a fault to chase.

It is the designed state in dev alone. Both of those jobs are metering sources
for everything Tally bills, so a deployment where they are down produces the
same `/targets` page as a healthy dev cluster while the meter runs empty, and
the first symptom is an invoice that comes up short. A deployment that replaces
this file therefore alerts on `up == 0` for both jobs — they are static targets,
so their `up` series exists whatever the exporter is doing. The two discovered
jobs need the second rule as well, on `absent(up{job="..."})`, for the reason
above. Nothing in this tree alerts yet, because the tree carries no alerting
component.

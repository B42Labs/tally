---
title: The OpenStack metrics pipeline
description: Why a sample reaches VictoriaMetrics on either a push or a pull path, what each path costs, and why Tally ships no exporter of its own.
quadrant: explanation
audience: all
---

# The OpenStack metrics pipeline

The series names, the scrape jobs and their intervals are on
[the metrics reference page](/reference/observability/metrics), and the five
labels every provider sample carries are on
[the label convention page](/reference/formats/label-convention). This page
says why the pipeline has two ends and what each of them is bounded by. The
steps are in
[publish metrics over OTLP](/how-to/observability/publish-metrics-over-otlp)
and
[scrape the OpenStack exporters](/how-to/observability/scrape-the-openstack-exporters).

## Two paths into one store

Two paths carry numbers into Tally's metrics store and both end in
VictoriaMetrics. On the push path a producer speaks OTLP to the OpenTelemetry
Collector, which remote-writes what it received. On the pull path
VictoriaMetrics scrapes four jobs on its own schedule. Once a sample is
written the two are indistinguishable: a query cannot tell which path a series
arrived on.

## What is pushed

The OpenTelemetry Collector receives OTLP on both transports, gRPC and HTTP,
and both are published through the Gateway on a hostname of its own.

Both transports demand HTTP Basic credentials, and that is what keeps those two
hostnames from being a write interface to the billing store for whoever resolves
them: a request without credentials is refused with 401 before it reaches the
pipeline. The collector reads its users from an htpasswd file mounted from the
`tally-otlp-auth` Secret, so no credential lives in this tree; the dev overlay
generates `tally:tally-dev-otlp-password`, and a real deployment puts a bcrypt
hash in that Secret instead. The file is read at startup, so a rotated Secret
takes effect when the pod rolls, which editing the Secret through the overlay
does.

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
buffered it. Everything behind it holds data (`batch` by design, and
`prometheusremotewrite` in its retry queue), so an unreachable VictoriaMetrics
would otherwise grow the collector's heap until the node kills the pod, and
every buffered point would be gone. That is not a remote case: the store is one
replica on an RWO volume, so editing either config file rolls it and writes fail
for as long as it takes to come back. `memory_limiter` limits by percentage of
the container's memory limit, which is why `otel-collector.yaml` sets one.

VictoriaMetrics keeps 13 months, `-retentionPeriod=13` in
[`victoriametrics.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/victoriametrics/victoriametrics.yaml),
which is a full billing year plus the current month.

## What is scraped

The two OpenStack jobs carry static `platform` and `cloud` labels, because
third-party exporters do not know Tally's label convention
([the architecture page](/explanation/architecture-and-the-provider-pattern),
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md)
section 3). The other two jobs carry no such labels: they export service
metrics, not provider resource metrics.

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
[`victoriametrics.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/victoriametrics/victoriametrics.yaml)
grant.

Both jobs relabel `instance` from the pod name. The default is `__address__`,
which for this role is the pod IP and port, and a pod IP goes back to the pool
when the pod is deleted and is handed to the next pod that needs one. That pod
would then continue the series its predecessor wrote, the same interleaving of
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
[`roadmap/02-phase-2-reporting-dashboards.md`](https://github.com/B42Labs/tally/blob/main/roadmap/02-phase-2-reporting-dashboards.md).

The database exporter's job is the one with an explicit `scrape_timeout`. Every
scrape of it runs the exporter's whole query set against the live OpenStack
service databases, 21 neutron tables alone, with `GROUP_CONCAT` and
`TIMESTAMPDIFF` over `ports`, `ipallocations`, and `standardattributes`. On a
cloud with six figures of ports that set outlasts the 10s default by a wide
margin, and dropping the HTTP request does not stop the queries: nothing in a
scrape path kills a server-side query. A short interval would then start a
second set on top of the first until the control plane's own database is
saturated and Nova, Neutron, and Keystone start timing out, with a flapping
target as the only symptom on Tally's side. 60s is what one scrape may cost,
300s leaves it room to finish, and the limits on the exporter's database user
(below) bound what a scrape that outruns both can still occupy.

## What is exposed and what is not

Three hostnames of this pipeline are attached to the Gateway's `https` listener,
which is the stack's ingress from outside the cluster
([`gateway.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/gateway/gateway.yaml)).
They carry different amounts of authority:

- The two OTLP hostnames accept metrics from whoever holds the Basic credentials
  above, and refuse everything else. What they accept is written to the billing
  store under the producer's own labels, which is why they are not open. Both
  are rate limited at the Gateway to 60 requests a second by the
  `BackendTrafficPolicy` in
  [`otel-collector.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/otel-collector/otel-collector.yaml),
  because refusing a request is the expensive half of Basic auth: the username
  is published here and in the Ceilometer publisher's URL, so every wrong password
  costs the collector a bcrypt comparison, tens of milliseconds of CPU against
  one replica's 100m request, while costing the sender nothing. The proxy
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
([`deploy/kind/kind.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kind/kind.yaml));
a deployment that publishes it anywhere else puts an authenticating proxy in
front of it or drops the route.

Inside the cluster nothing is gated, because `deploy/` carries no NetworkPolicy:
a pod can reach VictoriaMetrics' write and delete APIs, the collector's OTLP
ports, and the Reporting API's `/metrics`
([`internal/reporting/httpapi/metrics.go`](https://github.com/B42Labs/tally/blob/main/internal/reporting/httpapi/metrics.go)),
which is the state that route documents. That gap belongs to the deployment as a
whole rather than to this pipeline, and it is not closed here.

The collector's port 8888 is published on the Service, so VictoriaMetrics can
scrape it, and no route sends it through the Gateway: it is reachable from
inside the cluster alone.

## The Ceilometer publishing path

Ceilometer reaches Tally on one of two paths: it posts OTLP to the collector, or
it pushes to a gateway that the `ceilometer` scrape job reads. Which path a
deployment takes depends on its Ceilometer version, so the version is the first
thing to establish, and
[publish metrics over OTLP](/how-to/observability/publish-metrics-over-otlp)
says how.

The userinfo in the URL is how the OTLP publisher authenticates. It inherits the
HTTP publisher's URL handling, which turns `user:password@` into HTTP Basic
credentials and strips it from the target address; there is no option for an
arbitrary header, which is why the collector's receivers ask for Basic. That
same URL handling uses the URL scheme as the transport scheme only for `http`
and `https`; for any other scheme, including `opentelemetryhttp`, it falls back
to plain HTTP unless the `ssl` parameter says otherwise, so `?ssl=true` is
required for a TLS endpoint, and without it the credentials would go over plain
HTTP.

This path leaves the labels incomplete. The publisher attaches exactly three
attributes per sample, `resource_id`, `user_id`, and `project_id`, so nothing in
what it sends says which platform or which cloud the sample came from. The
static labels in `scrape.yaml` do not help: they belong to a scrape job, and a
pushed sample passes no scrape job. A deployment on this path attaches
`platform` and `cloud` itself, either in the publisher's configuration or with
an attributes processor in its own collector config, ahead of the exporter.

On the fallback path Ceilometer pushes to a Prometheus Pushgateway and
VictoriaMetrics scrapes it. The `/metrics/job/<job>` segment is what the
Pushgateway API takes. With no grouping label after the job segment the
publisher pushes once per resource and appends `/resource_id/<id>` to the path
itself, so two samples of different resources do not overwrite one another. Each
sample is written with the labels `resource_id`, `user_id`, and `project_id`, so
two of the convention's five labels are already right, and `platform` and
`cloud` come from the job's static labels when VictoriaMetrics scrapes. Neither
path supplies `resource_type`. It has to come from the meter name or from a rule
the deployment writes.

What this path does not do is forget a resource. A group per `resource_id` is
created on the first push and the Pushgateway expires nothing: a group goes
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
point. The default changes for a deployment whose version check comes back
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

One row per requirement of
[the OpenStack page](/explanation/openstack-as-the-reference-provider).

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
before the scrape. The block itself is on
[the label convention page](/reference/formats/label-convention#mapping-an-openstack-exporter-to-the-convention).

A rule whose source label is absent does not match `(.+)` and leaves the sample
alone, so every series keeps whatever it had. That is also the bound on what
relabeling can do here: it renames labels and cannot add one the exporter never
emitted, which is why the two partial rows above stay partial. The neutron port
series gets no `project_id`, and the `openstack_nova_quota_*` series name their
project by name (`tenant`) and not by id.

### The read-only database user

The exporter reads the OpenStack service databases directly, so the account it
reads them with is the one thing standing between a scrape and the control
plane. It is granted `SELECT` on the tables its queries read and nothing else,
and it is
bound to the subnet the exporter runs in rather than to `%`, because those
grants read the full tenant inventory of the cloud from one password. Two caps
on the account are what keep a slow database from turning a scrape into an
outage: a connection cap bounds how many query sets pile up, and a statement
timeout ends the ones that outlive the scrape that started them, since nothing
in a scrape path kills a server-side query. Both have a floor. A connection cap
below one pool per DSN does not bound the pile-up, it truncates the scrape, and
the exporter still answers 200 with `up == 1`, so the alert for that reads the
series themselves rather than the target. The statements are in
[scrape the OpenStack exporters](/how-to/observability/scrape-the-openstack-exporters#create-the-read-only-database-user).

### Extend upstream, or supplement it

The concept asks to evaluate the existing exporter first and to extend it if
necessary ([the OpenStack page](/explanation/openstack-as-the-reference-provider)).
The decision is to run it unchanged and to write no supplementary exporter.

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

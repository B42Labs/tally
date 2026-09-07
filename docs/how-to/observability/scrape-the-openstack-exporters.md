---
title: Scrape the OpenStack exporters
description: "Replace the placeholder scrape targets with a deployment's own, create the account the database exporter reads with, and take the Pushgateway path for Ceilometer."
quadrant: how-to
audience: operator
---

# Scrape the OpenStack exporters

This guide points the store's two OpenStack scrape jobs at the exporters of one
deployment. At the end `scrape.yaml` names that deployment's addresses and its
cloud, the database exporter reads the control plane through an account of its
own, and Ceilometer reaches the store through a Pushgateway the `ceilometer`
job scrapes. What the pull path collects and what it cannot is in
[the OpenStack metrics pipeline](/explanation/the-openstack-metrics-pipeline).

## Before you start

- An overlay of your own over `deploy/kubernetes/base`, since a deployment
  replaces the scrape config rather than patching the base.
- [`openstack_database_exporter`](https://github.com/vexxhost/openstack_database_exporter)
  running beside the OpenStack control plane, reachable from the cluster.
- An account on the control plane's database that may create a user and grant
  on the service schemas, plus one that may write DDL for the index files.
- A Ceilometer whose publisher entry points carry `prometheus`, and a
  Prometheus Pushgateway it reaches, for the last section.
- The [metrics](/reference/observability/metrics) reference page, which states
  the four scrape jobs with their intervals, targets and static labels.

## Replace the scrape targets

1. Treat `os-db-exporter:9180`, `ceilometer-exporter:9101` and the cloud name
   `os-prod-eu1` in the base's `scrape.yaml` as placeholders. Those exporters
   run beside an OpenStack control plane and not in this cluster.

2. Declare a `configMapGenerator` of your own in the overlay for the same
   generated name and mark it `behavior: replace`, which is the kustomize
   mechanism for overriding a generated ConfigMap:

   ```yaml
   configMapGenerator:
     - name: victoriametrics-scrape
       behavior: replace
       files:
         - scrape.yaml
   ```

3. Write the overlay's own `scrape.yaml` with that deployment's exporter
   addresses and its cloud. Keep the static `platform` and `cloud` labels on
   the two OpenStack jobs, and take the relabel rules that put an exporter's
   own labels into the coordinate system from
   [mapping an OpenStack exporter to the convention](/reference/formats/label-convention#mapping-an-openstack-exporter-to-the-convention).

4. On a dev cluster the `ceilometer` job stays down and `openstack-db-exporter`
   is pointed at the OpenStack simulator instead.
   [`victoriametrics/scrape.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/overlays/dev/victoriametrics/scrape.yaml)
   of the dev overlay is such a file.

## Create the read-only database user

1. Set one DSN per service on the exporter, in `<SERVICE>_DATABASE_URL` in
   oslo.db format, such as `NOVA_DATABASE_URL`, `NOVA_API_DATABASE_URL` and
   `CINDER_DATABASE_URL`. A service whose variable is unset is skipped, and the
   exporter logs that it skipped it. Nova takes both of its variables or none of
   its collectors register, because the instance rows and the flavor rows live
   in different databases. The listen address defaults to `:9180` and the
   metrics path to `/metrics`, which is where the scrape target
   `os-db-exporter:9180` above comes from.

2. Create the account the exporter reads with and grant it `SELECT` on the
   tables its queries select from. The syntax is MySQL/MariaDB, which is what
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

3. Apply the exporter's index files with an account that may write DDL. `SELECT`
   alone does not cover them: the volume query in `sql/cinder/queries.sql`
   carries a `USE INDEX (volumes_service_uuid_idx)` hint, and that index is
   created by `sql/cinder/indexes.sql` rather than by cinder itself. A database
   that never ran that file fails the query, and a read-only user cannot create
   the index. Applying `sql/cinder/indexes.sql`, `sql/nova/indexes.sql` and
   `sql/nova_api/indexes.sql` is a one-time job.

## Publish Ceilometer to a Pushgateway

1. Point Ceilometer's sink at the Pushgateway. VictoriaMetrics scrapes the
   Pushgateway, and the `ceilometer` job already points at it, so this path
   needs no change to `scrape.yaml`:

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

2. Run a reaper beside it: the deletion events the event collector already
   consumes drive `DELETE /metrics/job/ceilometer/resource_id/<id>` against the
   Pushgateway. What a group the gateway never expires does to a bill is in
   [the Ceilometer publishing path](/explanation/the-openstack-metrics-pipeline#the-ceilometer-publishing-path).
   The OTLP path of
   [publish metrics over OTLP](/how-to/observability/publish-metrics-over-otlp)
   has no gateway and needs none.

## Check the result

1. Read the target page of the store. It needs the dev CA that signed the
   Gateway's certificate:

   ```sh
   make -s ca > tally-ca.crt
   curl --cacert tally-ca.crt 'https://vm.tally.127-0-0-1.nip.io:8443/targets'
   ```

2. Four jobs are listed. `reporting-api` and `otel-collector` are up.
   `ceilometer` is down with an unresolved-host error, because its target
   exists in no dev cluster. `openstack-db-exporter` is down between simulator
   runs, when nothing listens on `host.docker.internal:8091`, and up while
   `make simulator-up` publishes a month. Neither is a fault to chase on a dev
   cluster: both are the dev state as it is designed.

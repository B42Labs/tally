---
title: Publish metrics over OTLP
description: "Point one cloud's Ceilometer at the OpenTelemetry Collector, label what it sends, and set the credential the receivers ask for."
quadrant: how-to
audience: operator
---

# Publish metrics over OTLP

This guide puts one cloud's Ceilometer samples into the metrics store on the
push path. At the end Ceilometer posts to the collector's OTLP/HTTP endpoint,
the samples carry the `platform` and `cloud` labels the label convention asks
for, and the receivers accept the credential the publisher presents. What the
two paths into the store are and where each one ends is in
[the OpenStack metrics pipeline](/explanation/the-openstack-metrics-pipeline).

## Before you start

- A Ceilometer beside the OpenStack control plane, with write access to
  `/etc/ceilometer/pipeline.yaml` and permission to restart its agents.
- The OpenTelemetry Collector deployed, with its two OTLP hostnames published
  through the Gateway.
- Permission to edit the `tally-otlp-auth` Secret through the overlay and to
  roll the collector pod.
- A shell that reaches the cluster, for the rollout and the push at the end.
- The [metrics](/reference/observability/metrics) reference page, which states
  every `tally_` series the services expose and the four scrape jobs that run
  beside this path.

## Establish what Ceilometer can publish

1. Read the publisher entry points of the installed package rather than the
   release name:

   ```sh
   python3 -m pip show ceilometer
   python3 -c 'from importlib.metadata import entry_points; \
     print(sorted(e.name for e in entry_points(group="ceilometer.sample.publisher")))'
   ```

   The second command prints the publisher names this installation can
   resolve.

2. The list has to contain `opentelemetryhttp`. A Ceilometer older than 2024.1
   carries the `prometheus` entry point and not that one, and a list without it
   settles the question whatever the version string says. Such an installation
   takes the other path,
   [publish Ceilometer to a Pushgateway](/how-to/observability/scrape-the-openstack-exporters#publish-ceilometer-to-a-pushgateway).

## Configure the publisher

1. Name the meters and the collector's endpoint in one sink of
   `/etc/ceilometer/pipeline.yaml`. That publisher is the whole configuration
   of this path, and nothing scrapes Ceilometer on it:

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

   The meter list is a subscription and not a filter applied later, so `"*"`
   subscribes to every meter this Ceilometer polls for every resource in the
   cloud. Which meters exist depends on the pollsters the deployment runs, so
   check the names against its own `polling.yaml` first.

2. Keep `?ssl=true` on a TLS endpoint. The userinfo in the URL is the HTTP
   Basic credential the receivers ask for, and without that parameter the
   publisher falls back to plain HTTP and the credential goes with it. What the
   receivers accept, and why they take Basic rather than a bearer token, is in
   [what is pushed](/explanation/the-openstack-metrics-pipeline#what-is-pushed).

3. Attach `platform` and `cloud` yourself. The publisher sends exactly three
   attributes per sample, `resource_id`, `user_id` and `project_id`, so nothing
   in what it sends says which platform or which cloud the sample came from,
   and the static labels of a scrape job never reach a pushed sample. Set the
   two in the publisher's configuration, or with a processor in the
   deployment's own collector config:

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

   The processor then goes into that deployment's metrics pipeline, ahead of
   the exporter. The
   [label convention](/reference/formats/label-convention#labels-on-metric-series)
   page states the labels a series carries.

## Set the credential

1. Hash the credential the publisher URL carries. The collector reads its users
   from an htpasswd file, so no credential lives in the repository:

   ```sh
   htpasswd -nBC 12 tally
   ```

   ```text
   New password:
   Re-type new password:
   tally:$2y$12$<bcrypt hash>
   ```

   `-nB` prompts for the password rather than taking it from the command line.
   A password passed as an argument is written verbatim to the shell's history
   file and is readable in `ps` output and `/proc/<pid>/cmdline` for the life of
   the process, and it is the credential of the publisher URL below: whoever
   reads it back pushes series of their own into the billing store under labels
   of their choosing. `-C 12` is the bcrypt cost; `-B` on its own takes
   htpasswd's default of 5, which is 32 rounds where OWASP's floor is 1024, and
   the Secret this line lands in is readable by every role that holds
   `get secrets` in the namespace as well as by whoever holds an etcd backup.

2. Carry that line in the `tally-otlp-auth` Secret under the key `htpasswd`,
   generated from the overlay. The dev overlay generates the plain line
   `tally:tally-dev-otlp-password` instead, which is what the check below uses:

   ```yaml
   secretGenerator:
     - name: tally-otlp-auth
       literals:
         - htpasswd=tally:$2y$12$<bcrypt hash>
   ```

3. Roll the collector pod. The file is read at startup, so a rotated Secret
   takes effect when the pod rolls, which editing the Secret through the
   overlay does on its own:

   ```sh
   kubectl -n tally rollout status deployment/otel-collector
   ```

   ```text
   deployment "otel-collector" successfully rolled out
   ```

## Check the result

1. Push one point through the whole path from the host and read it back out of
   the store. The dev CA that signed the Gateway's certificate is what makes
   both calls trust the endpoint:

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
   `{"partialSuccess":{}}`. Anything the collector rejects comes back as a 4xx
   with the reason in the body.

2. The query answers with the one series, within 30 seconds of the push:

   ```json
   {"status":"success","data":{"resultType":"vector","result":[
     {"metric":{"__name__":"tally_metrics_drill"},"value":[1787035962,"1"]}]}}
   ```

   `__name__` is the only label. The payload carries no resource attributes, so
   the remote-write exporter derives no `job` or `instance` label from it.

3. The wait is expected, not a fault. VictoriaMetrics evaluates instant queries
   behind wall clock by `-search.latencyOffset`, 30 seconds by default, so that
   a query does not read a window still being written. A query run immediately
   after the push can therefore return an empty result while the point is
   already stored. Repeat the query after the offset has passed before treating
   an empty answer as a failure.

4. Run the same push without `--user`. It answers `401` and
   `{"code":16,"message":"no basic auth provided"}`, which is the check that the
   receivers are not open.

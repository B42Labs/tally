# Alerting

Two components turn the metrics store into notifications. vmalert evaluates the
rules of this repository against VictoriaMetrics once a minute and posts what
fires to Alertmanager, which groups it, deduplicates it, and hands it to a
receiver. No Prometheus server is involved: VictoriaMetrics is the only store,
and vmalert queries it over the Prometheus API the store answers. This document
describes what is deployed, what the rules ask, how to reach both on a dev
cluster, and how a deployment says where an alert is delivered.

## What is deployed

### vmalert

[`vmalert.yaml`](../deploy/kubernetes/base/vmalert/vmalert.yaml) runs one
replica of `victoriametrics/vmalert` behind the Service `vmalert` on port 8880.
Every flag it takes:

| Flag | Why it is set |
| --- | --- |
| `-rule=/etc/vmalert/rules.yaml` | The rules, mounted from the generated `vmalert-rules` ConfigMap. |
| `-datasource.url=http://victoriametrics:8428` | The store every expression is evaluated against, reached in-cluster by Service name. |
| `-notifier.url=http://alertmanager:9093` | Where a firing alert is posted, again in-cluster. |
| `-remoteWrite.url=http://victoriametrics:8428` | vmalert writes the `ALERTS` and `ALERTS_FOR_STATE` series for every pending and firing alert back into the store. |
| `-remoteRead.url=http://victoriametrics:8428` | At startup it reads `ALERTS_FOR_STATE` back and restores the pending timers from it. |
| `-external.url=$(VMALERT_EXTERNAL_URL)` | The value goes into the `generatorURL` of every alert vmalert sends, which is the "Source" link the Alertmanager UI offers. |

The remote pair is what keeps a `for` from restarting on every pod roll. An
alert that has been pending for ten of its fifteen minutes resumes at ten
minutes rather than at zero, and that matters here because editing `rules.yaml`
rolls the pod through the generated ConfigMap name. Without the pair, every edit
to the file would silently reset every timer in it.

`-external.url` reads its value from the argument, and Kubernetes expands
`$(VAR)` in an argument from the container's own environment. The base sets
`VMALERT_EXTERNAL_URL` to the placeholder `https://vmalert.tally.example.com`,
and each overlay patches that variable rather than the argument. An unpatched
value sends a reader who follows a Source link to a host that answers nothing.

### Alertmanager

[`alertmanager.yaml`](../deploy/kubernetes/base/alertmanager/alertmanager.yaml)
runs one replica of `prom/alertmanager` behind the Service `alertmanager` on
port 9093. Its flags:

| Flag | Why it is set |
| --- | --- |
| `--config.file=/etc/alertmanager/config.yaml` | The routing tree, mounted from the generated `alertmanager-config` ConfigMap. |
| `--storage.path=/alertmanager` | Where silences and the notification log are kept. |
| `--web.external-url=$(ALERTMANAGER_EXTERNAL_URL)` | Alertmanager builds the links it renders and the ones it puts into a notification from this value, so it is patched per overlay for the same reason `-external.url` is. |
| `--cluster.listen-address=` | The empty value turns the gossip cluster off. There is one replica and therefore no peer to settle with, and at its default Alertmanager opens the port and logs "gossip not settled" at every start while it waits for members that never arrive. |

The storage path is backed by a 1 GiB volume claim, which is why Alertmanager is
a StatefulSet rather than a Deployment, as VictoriaMetrics and TimescaleDB are.
What it holds is state: the notification log, which keeps a receiver from being
told a second time about a group it was already told about, and the silences an
on-call engineer set. Editing `config.yaml` rolls the pod by design (see below),
and on scratch storage that roll would deliver every firing group again at the
next `group_wait` and drop every silence. One replica with
`--cluster.listen-address=` has no peer to replicate either back from, so the
loss would be total rather than partial.

The claim is also why the pod carries a `securityContext` where nothing else in
the base does. `prom/alertmanager` runs as `nobody`, UID and GID 65534, and
Kubernetes chowns a mounted volume to the pod's `fsGroup` or not at all. Most
CSI drivers hand a freshly provisioned volume back owned `root:root` 0755, so
without `fsGroup: 65534` Alertmanager cannot create the notification log or a
silence under `--storage.path` and the pod crash-loops. kind's `local-path`
provisioner happens to create its directory 0777, so this would pass in the dev
overlay and fail wherever the base is deployed for real.

### The rules

[`rules.yaml`](../deploy/kubernetes/base/vmalert/rules.yaml) holds one group,
`tally`, evaluated every minute. The five critical rules carry a `runbook`
annotation naming the page a reader opens when the alert arrives.

| Alert | What the expression asks | `for` | Severity | Runbook |
| --- | --- | --- | --- | --- |
| `TallyCloudEventsSilent` | No collector event from a cloud in an hour, while it had one in the last 24 hours | 15m | critical | [TallyCloudEventsSilent.md](runbooks/TallyCloudEventsSilent.md) |
| `TallyEventsRejected` | More than 10 rejected events per (cloud, reason) in 15 minutes | none | warning | |
| `TallySyncErrors` | Any reconciliation error for a cloud in the last hour | none | warning | |
| `TallySyncStale` | No completed reconciliation run for a cloud in 30 minutes | none | critical | [TallySyncStale.md](runbooks/TallySyncStale.md) |
| `TallyReconciliationDriftHigh` | More than 50 resources corrected in a cloud over 6 hours | none | warning | |
| `TallyCollectorBufferAging` | `tally_collector_oldest_buffered_seconds` above 600 | none | warning | |
| `TallyResourceCountAnomaly` | A per (cloud, resource_type) count above `1.5 * (7-day baseline + 10)`, the baseline offset by 6h so the count is not inside it | 30m | info | |
| `TallyRecordedSeriesMissing` | `tally:current_resources:sum` absent while `tally_current_resources` is still scraped | 15m | warning | |
| `TallyScrapeTargetDown` | `up == 0` on one of four scrape jobs | 5m | critical | [TallyScrapeTargetDown.md](runbooks/TallyScrapeTargetDown.md) |
| `TallyScrapeJobMissing` | `absent(up{job=...})` for `reporting-api` or `otel-collector` | 5m | critical | [TallyScrapeJobMissing.md](runbooks/TallyScrapeJobMissing.md) |
| `TallyExporterServiceSilent` | The database exporter answers a scrape while a whole service emits no series for a cloud | 15m | critical | [TallyExporterServiceSilent.md](runbooks/TallyExporterServiceSilent.md) |

A rule without a `for` fires at the first evaluation whose expression returns a
series, so at most a minute after the condition holds. A `for` is on the rules
where one evaluation is not evidence. `TallyScrapeTargetDown` waits out a single
failed scrape, and `TallyExporterServiceSilent` waits three of the exporter's
300-second scrapes, so it reports a database connection cap that is too low
rather than one slow scrape.

`TallyScrapeTargetDown` matches
`up{job=~"reporting-api|openstack-db-exporter|ceilometer|otel-collector"}`. That
regex names four jobs, one more than
[`roadmap/02-phase-2-reporting-dashboards.md`](../roadmap/02-phase-2-reporting-dashboards.md)
lists, and the fourth is `ceilometer`. The recorded Ceilometer default pushes to
a gateway that job scrapes ([`openstack-metrics.md`](openstack-metrics.md)), so
on that path the job carries metering data, and a target of it that stops
answering costs samples the same way a target of the other three does.

`TallyResourceCountAnomaly` reads a recorded series, `tally:current_resources:sum`,
which the same group writes one rule earlier. Aggregating
`tally_current_resources` inside the baseline instead would make the store repeat
that aggregation once per step of the seven-day window on every minute
evaluation, and that store is the single replica Grafana and the Reporting API's
query path also read. vmalert writes the recorded series through
`-remoteWrite.url` and reads it back through `-datasource.url`, the same pair
that carries `ALERTS_FOR_STATE`.

`TallyRecordedSeriesMissing` is what watches that pair, because reading the
recorded series put both sides of the anomaly rule's comparison behind the
write path. A remote-write queue that overflows, or a store that was briefly
unavailable, leaves a hole in `tally:current_resources:sum`: past the store's
lookbehind the instant selector goes absent, the expression returns nothing on
every evaluation, and the anomaly rule stops watching with no `for` timer, no
notification and no log line to say so. The second clause,
`count(tally_current_resources) > 0`, is what keeps the rule quiet where the
absence is expected — `absent()` alone fires on any cluster that has not
reconciled yet, dev clusters included — so it reports the one case that is a
fault: the scrape path filling while the write path does not.

`TallyExporterServiceSilent` matches `up` against the exporter's series on
`(cloud, instance)` and selects `job="openstack-db-exporter"` on both sides. On
`(cloud)` alone, a deployment running a second exporter target for one cloud
would have the healthy target's series answer for the silent one, and any other
producer of a series by one of those names would suppress the alert for good.

`TallyScrapeJobMissing` covers what `TallyScrapeTargetDown` cannot see.
`reporting-api` and `otel-collector` discover their targets, so a job that
resolves to no targets emits no `up` series at all and the `up == 0` rule stays
silent. A Deployment scaled to zero, a renamed Service or port, a removed
RoleBinding, and an unreachable API server all land in the `absent()` rule.

### Editing rules.yaml or config.yaml

Neither file is applied as a file. Each is the source of a `configMapGenerator`
in its component's `kustomization.yaml`, generating `vmalert-rules` and
`alertmanager-config`, and kustomize appends a content hash to each name.
Editing a file changes the generated name, which changes the pod spec that
mounts it, which rolls the pod. Neither component is left evaluating rules or
routing by a config that no longer matches the tree. It is the pattern the
collector and scrape configs follow, described under
[Editing either config](openstack-metrics.md#editing-either-config).

### Where an alert is delivered

[`config.yaml`](../deploy/kubernetes/base/alertmanager/config.yaml) groups by
`alertname` and `cloud`, waits 30 seconds for a group to collect (`group_wait`),
sends an update at most every 5 minutes (`group_interval`), and repeats an
unresolved group every 4 hours (`repeat_interval`). One child route matches
`severity="critical"` and tightens `repeat_interval` alone, to 1 hour. It
inherits the receiver from its parent, so a deployment that names a receiver at
the root covers both branches with it.

The single receiver, `default`, names no integration, and it does so on purpose.
Where an alert is delivered is a property of a deployment and not of this
repository, so until one says otherwise an alert is grouped and held in
Alertmanager, where the UI and `amtool` show it.

A deployment adds its own delivery by replacing the generated ConfigMap from its
overlay rather than by patching the file in the base:

```yaml
configMapGenerator:
  - name: alertmanager-config
    behavior: replace
    files:
      - config.yaml
```

The overlay's own `config.yaml` then carries the `webhook_configs` or
`email_configs` under its receiver. That is the mechanism
[Replacing the placeholders](openstack-metrics.md#replacing-the-placeholders)
records for the scrape config, used here for the same reason: a file the base
owns stays the base's, and the deployment's copy is a file of its own.

## Dev access

`make up` publishes both on the dev overlay's hostnames:

```text
https://vmalert.tally.127-0-0-1.nip.io:8443/vmalert/
https://alertmanager.tally.127-0-0-1.nip.io:8443
```

The vmalert URL carries the `/vmalert/` path because the route publishes three
prefixes rather than the host: `/vmalert` for the UI and the JSON its pages
read, plus the two read endpoints `/api/v1/rules` and `/api/v1/alerts`. What is
left is `/-/reload`, `/flags`, `/metrics` and `/debug/pprof`, which vmalert
serves at the root alone, and the route does not carry the root, so each of them
gets the Gateway's 404. `/-/reload` stays reachable from inside the cluster,
where it re-reads the mounted files and therefore changes nothing on its own.

Alertmanager answers reads and writes on one API and one port, so its route
names what it publishes rather than carrying the host: `GET` on
`/api/v2/alerts`, `/api/v2/silences`, `/api/v2/silence/<id>` and
`/api/v2/receivers`, plus the index, its bundle under `/assets` and the icon.
Everything else gets the Gateway's 404, which includes the two reads that would
otherwise come with the host: `/api/v2/status` answers with the loaded config,
and therefore with whatever `webhook_configs` or `email_configs` a deployment
put into it, and `/metrics` names the delivery integrations it is configured
with. The UI's Status page reads the first of those and reports an error; that
is the endpoint the route withholds.

A second rule answers `POST`, `PUT` and `DELETE` under `/api/v2`, plus
`/-/reload` and `/debug`, with a 403. None of those is published either, so what
this rule adds is an answer that says why: the "New Silence" form the UI offers
is told it was refused rather than left with a 404. The read matches name `GET`
for that to work, because the Gateway API ranks a longer path prefix above a
method match and `/api/v2/silences` is longer than `/api/v2`.

Silences are created from inside the cluster instead, with the `amtool` the
image ships:

```sh
kubectl --context kind-tally -n tally exec statefulset/alertmanager -- \
  amtool --alertmanager.url=http://127.0.0.1:9093 silence add \
  --author=<who> --comment='<why>' --duration=2h alertname=<name>
```

Both hostnames need the dev CA that signed the Gateway's certificate:

```sh
make -s ca > tally-ca.crt
curl --cacert tally-ca.crt \
  https://vmalert.tally.127-0-0-1.nip.io:8443/api/v1/rules
```

The base leaves both hostnames at `vmalert.tally.example.com` and
`alertmanager.tally.example.com`, and both external URLs at the matching
placeholder. An overlay that publishes either component patches the route
hostname and the environment variable together, which is what the dev overlay
does.

## What a fresh cluster shows

About five minutes after `make up`, `TallyScrapeTargetDown` fires for the jobs
`openstack-db-exporter` and `ceilometer`. Both are static targets for exporters
that run beside an OpenStack control plane rather than in this cluster, which is
the designed dev state described under
[Replacing the placeholders](openstack-metrics.md#replacing-the-placeholders),
and not a fault to chase.

No other rule fires on a cluster nothing has reported to. `TallyScrapeJobMissing`
stays silent because both discovered jobs resolve to targets, and
`TallyExporterServiceSilent` needs an exporter target that answers a scrape,
which is the target that is down. The remaining seven read `tally_` series the
store does not carry yet, and an expression over nothing returns nothing.
`TallyRecordedSeriesMissing` is quiet for a different reason: its `absent()`
clause is true here, and the `count(tally_current_resources) > 0` it is paired
with is what keeps it from reporting a cluster that has not reconciled yet as a
stalled write path.

## When an expected alert does not appear

Read `/api/v1/rules` on the vmalert hostname and find the rule. A non-empty
`lastError` on it means the datasource query failed rather than that the
condition was false, and the message names what VictoriaMetrics answered.

`TallyCloudEventsSilent` never fires for a cloud that has never reported. Its
second clause,
`sum by (cloud) (increase(tally_events_ingested_total{source="collector"}[24h])) > 0`,
is empty for such a cloud, and an `and` with an empty side returns nothing. That
is what keeps a cloud that is idle by design or decommissioned from firing
forever, and it is also why the drill for the rule seeds an event first. The
scripted run is in [drills/phase2.md](drills/phase2.md).

## How CI validates

`make check-alerting` loads each file into the pinned image the cluster runs, so
Docker is the only prerequisite and no cluster is involved. It runs `rules.yaml`
through `vmalert -dryRun`, which catches an expression the deployed binary
cannot parse, and `config.yaml` through `amtool check-config`, which catches an
unknown or misspelled routing field. CI runs the target after the tests
([`.github/workflows/ci.yaml`](../.github/workflows/ci.yaml)).

The shape of the rules file is pinned separately by
[`rules_test.go`](../deploy/kubernetes/base/vmalert/rules_test.go): the alert
names and their order, the severities, the runbook annotations, the recorded
series the anomaly rule reads, and the scrape jobs the last three rules name.
The flags of both Deployments are pinned by their
[`manifest_test.go`](../deploy/kubernetes/base/vmalert/manifest_test.go) files.
Parsing says an expression is legal, not that it is the expression this
repository means to run.

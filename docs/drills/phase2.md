# Phase 2 acceptance drill: TallyCloudEventsSilent

`TallyCloudEventsSilent` is the alert the concept asks for under
[Known limitations](https://b42labs.github.io/tally/explanation/dual-ingestion-and-reconciliation#known-limitations): a cloud whose collector has
gone quiet while the cloud kept creating and deleting resources. This drill
takes one dev cluster from a seeded event through the pending state, the firing
state, the notification in Alertmanager, and the resolution, and it checks on
the way that the pending timer survives a vmalert restart.

The rule and the components it runs on are described in
[`../alerting.md`](../alerting.md). Every command below runs from the repository
root against a running dev cluster. The drill takes about 80 minutes of wall
clock, most of it waiting.

## Preparation

The Gateway's certificate is signed by the dev CA, so every call needs it:

```sh
make -s ca > tally-ca.crt
```

Both components answer before anything is seeded:

```sh
curl --cacert tally-ca.crt \
  https://vmalert.tally.127-0-0-1.nip.io:8443/api/v1/rules
curl --cacert tally-ca.crt \
  https://alertmanager.tally.127-0-0-1.nip.io:8443/api/v2/receivers
```

The first lists one group, `tally`, with the eleven alerting rules of
[`rules.yaml`](../../deploy/kubernetes/base/vmalert/rules.yaml) and the one
recording rule `TallyResourceCountAnomaly` reads. The second
answers 200 with the one receiver `config.yaml` declares, `default`.
`/api/v2/status`, which would answer with the loaded config, is not published
([alerting.md](../alerting.md#dev-access)); read it from inside the cluster if
you need it.

## Seed one collector event

The rule compares two windows over the same counter, and on a cluster nothing
has reported to both are empty. The second clause,
`increase(tally_events_ingested_total{source="collector"}[24h]) > 0`, then
returns nothing, and an `and` with an empty side returns nothing, so the rule
cannot fire and there is nothing to observe. The drill therefore seeds first:
one event now makes the 24-hour window non-empty for the next 24 hours, and the
1-hour window empties again an hour after it.

A seed the dashboard drill posted within the last 24 hours serves the same
purpose. If
[`../grafana-dashboards.md`](../grafana-dashboards.md#part-1-real-series-through-the-reporting-api)
has been run against this cluster inside that window, its events already satisfy
the second clause, and the timeline below runs from its last post rather than
from a fresh seed.

Ingest is authenticated per (platform, cloud), so issue a credential for cloud
`os-prod-eu1` the same way that drill does:

```sh
export TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
TOKEN="$(go run ./cmd/tally-reporting-admin create-ingest-credential \
  --platform openstack --cloud os-prod-eu1)"
```

Post one create event. The heredoc delimiter is unquoted, unlike the one in the
dashboard drill, because the body has to expand its `$(date ...)` calls:

```sh
curl --cacert tally-ca.crt -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  'https://api.tally.127-0-0-1.nip.io:8443/api/v1/events' \
  --data-binary @- <<EOF
{
  "event_id": "drill-alert-seed-$(date +%s)",
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "event_type": "compute.instance.create.end",
  "platform": "openstack",
  "cloud": "os-prod-eu1",
  "resource_type": "instance",
  "resource_id": "drill-alert-instance-$(date +%s)",
  "project_id": "drill-project",
  "source": "collector",
  "payload": {
    "state": "active",
    "size": {"vcpus": 4, "ram_gb": 16, "disk_gb": 80, "flavor": "m1.large"}
  }
}
EOF
```

The epoch suffix on both ids is what makes a repeated drill work. Ingestion is
idempotent on `(event_id, timestamp)`, so an id posted again with a timestamp it
already carries is stored as a duplicate and moves `tally_events_ingested_total`
by nothing, which is the one counter this drill reads.

The answer names what the batch did:

```json
{"accepted": 1, "duplicates": 0, "rejected": []}
```

Within 60 seconds, the 30-second scrape of the `reporting-api` job plus the
30-second query latency offset, the counter is readable in the store:

```sh
curl --cacert tally-ca.crt -G \
  'https://vm.tally.127-0-0-1.nip.io:8443/api/v1/query' \
  --data-urlencode 'query=sum by (cloud) (increase(tally_events_ingested_total{source="collector"}[1h]))'
```

It answers a value above 0 for `os-prod-eu1`. That is the drill's starting
state: both windows carry the seed, and the rule stays silent because its first
clause is false.

## Wait out the silence

Post nothing further for `os-prod-eu1`. Note the time of the seed; the timeline
runs from it.

At about 60 minutes the 1-hour window has emptied while the 24-hour window has
not, so the expression starts returning a series and the rule enters its 15
minute `for`:

```sh
curl --cacert tally-ca.crt \
  https://vmalert.tally.127-0-0-1.nip.io:8443/api/v1/alerts
```

It lists `TallyCloudEventsSilent{cloud="os-prod-eu1"}` with state `pending`.

At about 75 minutes the same call lists it as `firing`. Within `group_wait`, 30
seconds, Alertmanager has it too:

```sh
curl --cacert tally-ca.crt \
  https://alertmanager.tally.127-0-0-1.nip.io:8443/api/v2/alerts
```

The entry carries `receivers: [{"name": "default"}]` and
`status.state: active`. The receiver delivers nothing, which is the point: the
alert is held where the UI and `amtool` show it until a deployment names a
receiver of its own.

Open `https://alertmanager.tally.127-0-0-1.nip.io:8443` and follow the alert's
Source link. It opens
`https://vmalert.tally.127-0-0-1.nip.io:8443/vmalert/alert?group_id=<id>&alert_id=<id>`,
the rule's own page in vmalert, and that URL is the one `-external.url` supplies.

## Restart vmalert while it fires

The state of a firing alert lives in the store rather than in the process, and
this is what says so. While the alert fires, delete the pod:

```sh
kubectl --context kind-tally -n tally delete pod \
  -l app.kubernetes.io/name=vmalert
kubectl --context kind-tally -n tally rollout status deploy/vmalert
```

Once the new pod is Ready, `GET /api/v1/alerts` on the vmalert hostname lists
`TallyCloudEventsSilent{cloud="os-prod-eu1"}` as firing again, without another
15-minute pending phase. vmalert read `ALERTS_FOR_STATE` back through
`-remoteRead.url` and restored the timer. A run without that flag would show the
alert pending and start the 15 minutes over.

## Resolve it

Post one more event of the same shape, with fresh ids and a fresh timestamp:
rerun the seed command above. The 1-hour window is non-empty again, so the first
clause of the expression turns false.

Within about two minutes (the 30-second scrape, the 30-second latency offset,
and the one-minute evaluation interval) vmalert stops seeing the series, sends
the resolution to Alertmanager, and `GET /api/v2/alerts` no longer lists
`TallyCloudEventsSilent`. `GET /api/v1/alerts` on the vmalert hostname drops it
at the same evaluation.

## What else the run shows

The two `TallyScrapeTargetDown` alerts for `openstack-db-exporter` and
`ceilometer` are firing throughout and are expected. Both jobs are static
targets for exporters that run beside an OpenStack control plane rather than in
this cluster, which is the designed dev state of
[`../openstack-metrics.md`](../openstack-metrics.md#replacing-the-placeholders).

The seed call without its `Authorization` header answers 401 and moves no
counter, which is the check that ingest is not open to whoever resolves the
hostname.

A query that comes back empty within 30 seconds of a post has lost nothing.
VictoriaMetrics evaluates queries behind wall clock by `-search.latencyOffset`,
30 seconds by default, so a query over a window still being written answers
empty; repeat it after the offset before treating it as a failure. The full
explanation is in the
[acceptance drill](../openstack-metrics.md#acceptance-drill) of the metrics
pipeline.

Two costs of the run, both accepted:

- The seed events stay in the reporting database and their series stay in the
  dev store for the 13-month retention VictoriaMetrics runs with. `make down`
  deletes the cluster and its volumes, and that is the shorter way out.
- The drill is for dev clusters. It books an instance in a cloud and a project
  that do not exist, so running it against a store any invoice is derived from
  puts invented usage into the billing record.

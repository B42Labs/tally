---
title: Watch the month in Grafana
description: Open the dev cluster's Grafana as an anonymous viewer, point its four dashboards at the simulated cloud and the simulated month, and find the one alert the dev stack fires by design.
quadrant: tutorial
audience: all
---
<!-- Shown output captured on 2026-09-07 from commit 789d782 with kind v0.32.0, kubectl v1.36.1, Docker Desktop 4.86.0, Go 1.27.1 on macOS 15.7.4. -->

# Watch the month in Grafana

In this lesson you open the Grafana the cluster runs, point its four dashboards
at the simulated cloud and at the simulated month, read what each of them
shows, and find the one alert the dev stack fires by design in vmalert and in
Alertmanager.

At the end you have a name for what each dashboard and each alert component
reads, and the whole state of this track, written out, which the billing track
continues from.

This lesson takes about 5 minutes.

## Before you start

- The state lesson 3 leaves: the kind cluster with the dev overlay, the
  simulator stack holding its 84 notifications, the month of July 2026 in the
  reporting database, and one completed run of that month in the engine
  database.
- A browser.
- `tally-ca.crt` at the repository root, for the `curl` calls beside the
  browser. A new shell writes the file again with:

  ```sh
  make -s ca > tally-ca.crt
  ```

- The dashboards read only what lesson 2 wrote, the events and the pushed
  series, so the engine's run of lesson 3 changes nothing on them.

## Open Grafana

1. Ask Grafana whether it is up:

   ```sh
   curl --cacert tally-ca.crt https://grafana.tally.127-0-0-1.nip.io:8443/api/health
   ```

   ```json
   {
     "database": "ok",
     "version": "13.1.3",
     "commit": "45a27d64b64a82d666b06aa5c5bb3521587edb0d"
   }
   ```

   `database` `ok` has to match. `version` 13.1.3 is the image the manifests
   pin, so it matches at this commit and moves with the manifests. `commit` is
   Grafana's own build.

2. Open `https://grafana.tally.127-0-0-1.nip.io:8443` in the browser. The
   browser stops on a certificate warning: the certificate is signed by the dev
   CA of lesson 1, which the browser does not know, and that CA is not
   installed on purpose, because cert-manager creates a new one after every
   `make up`. Proceed past the warning for this hostname, in Firefox under its
   Advanced button.

   No login follows. The dev overlay lets an anonymous viewer read every
   dashboard, which is what `GF_AUTH_ANONYMOUS_ENABLED` with the Viewer role in
   `deploy/kubernetes/overlays/dev/kustomization.yaml` sets, and
   [install the Grafana dashboards](/how-to/observability/install-the-grafana-dashboards)
   covers a deployment with logins.

   Go to the folder `Tally`. It carries four dashboards, `Tally / Fleet
   Overview`, `Tally / Ingestion Health`, `Tally / Project Drilldown` and
   `Tally / Reconciliation Drift`, in the order the folder lists them, and
   [Grafana dashboards](/reference/observability/dashboards) names every panel
   and the expression it reads.

3. Read the same four through the API:

   ```sh
   curl --cacert tally-ca.crt 'https://grafana.tally.127-0-0-1.nip.io:8443/api/search?type=dash-db' | jq '.[] | {folderTitle, title, uid}'
   ```

   ```json
   {"folderTitle":"Tally","title":"Tally / Fleet Overview","uid":"tally-fleet-overview"}
   {"folderTitle":"Tally","title":"Tally / Ingestion Health","uid":"tally-ingestion-health"}
   {"folderTitle":"Tally","title":"Tally / Project Drilldown","uid":"tally-project-drilldown"}
   {"folderTitle":"Tally","title":"Tally / Reconciliation Drift","uid":"tally-reconciliation-drift"}
   ```

   The call carries no credential, so this is the anonymous viewer again. The
   four titles and the four uids have to match. A 401 page in the browser or
   here means anonymous access is off, which is not the dev overlay.

## Set the cloud and the month

1. Set the `cloud` variable to `os-sim` on every dashboard and leave `platform`
   on All. Both variables read the clouds off `tally_current_resources`, so
   `os-sim` is offered once the projection holds the month.

2. Use two time ranges. A dashboard has one range at a time and its panels are
   split across the two, so you set one range, read the panels that fit it,
   then set the other.

   The absolute range `2026-07-01 00:00:00` to `2026-08-01 00:00:00` fits every
   panel that reads an `openstack_*` or a `ceilometer_*` series, because the
   simulator pushed those at simulated time, inside July 2026. `Last 3 hours`
   fits every panel that reads a `tally_` series, because those were scraped
   off the Reporting API at the wall clock while the month went out, in the few
   minutes lesson 2 took. The dashboards open on `Last 6 hours` refreshing
   every minute. The next step names which panels are which per dashboard.

   Every dashboard file carries `"timezone": "utc"`, so both ranges are read in
   UTC and no time zone setting is needed.

## Read the four dashboards

The four read one store from four angles, and each of them narrows to what the
`cloud` variable is set to. What you see on them:

- `Tally / Fleet Overview`, the fleet the events built. It counts the resources
  the projection holds by type and state, draws the same counts as a trend, and
  says how many clouds report; two of its panels read the pushed inventory
  instead, the number of tenants and the ten with the most instances.
- `Tally / Ingestion Health`, the way in. It draws the events ingested per
  cloud and source, the duplicates dropped and the items rejected, the
  collector's buffer and its oldest buffered event, the projection replays, and
  whether each scrape target answers.
- `Tally / Project Drilldown`, one tenant at a time. It counts that tenant's
  servers, volumes, floating IPs, routers, images and load balancers, sums the
  GB its volumes hold, and reads its usage of the three nova quotas. Its last
  panel is the cloud's ingest split by event type, the one panel here that does
  not narrow to the tenant.
- `Tally / Reconciliation Drift`, what a sync against the cloud's own API
  changed. It draws the resources reconciled by action, the sync errors, and
  the sync runs by status, beside a text panel on how to read the three.

Which panels carry a value in this lesson and which stay empty is what the four
steps below say, and each of them names the time range its panels need.

1. `Tally / Fleet Overview`. With `Last 3 hours`, `Resources by type and state`
   shows 37 instances `active`, 630 `deleted` and 1 `shelved`, 16 volumes
   `available`, 12 `in-use` and 141 `deleted`, 9 floating IPs `active` and 7
   `deleted`, 7 images `active` and 2 `deleted`, and 4 load balancers `active`
   and 1 `deleted`. The whole month has ended, so most of the fleet is deleted.
   `Resource count trend` draws the same counts as lines that jump once, when
   the month went out, and `Clouds reporting` is 1.

   With the July range, `Projects (OpenStack)` is 6 and `Top 10 projects by
   instance count` lists the six tenant ids, four of them at 3 instances at the
   month's end and the CI tenant and the second Gardener tenant at 0. Over the
   month the CI tenant peaked at 10 and the large Gardener tenant at 11.

2. `Tally / Ingestion Health`. With `Last 3 hours`, `Event ingest rate` shows
   one spike for cloud `os-sim` and source `collector` over the minutes the
   month went out, 1728 events in all. How high the spike stands is your
   machine's, because it is those events over the span the collector took to
   drain its outbox: about five events per second where that took three
   minutes, and above 30 where it took half a minute.

   `Dedup rate` and `Rejected events` draw nothing, because the month carried
   no duplicate and no item was rejected, so those counters were never written.
   A panel over a counter that does not exist shows `No data`, which here reads
   as 0. `Collector buffer depth` and `Oldest buffered event age` show
   `No data` too: the collector runs in the compose stack on your machine and
   the cluster's store scrapes no target for it, so these two panels stay empty
   on the dev stack, and lesson 2 read those gauges off the collector directly.
   `Projection replays` draws nothing.

   `Scrape health` shows `reporting-api` and `otel-collector` up (1) and
   `ceilometer` down (0), which is the designed dev state, a placeholder target
   for an exporter that runs beside a real control plane.
   `openstack-db-exporter` is up (1), because the holding simulator still
   serves its inventory on port 8091, which that job scrapes. Between simulator
   runs it reads 0.

3. `Tally / Project Drilldown`. Pick `005be5adeef3d87e280d03d9d57c38b4` in the
   `project_id` variable, one of the six ids and the Gardener tenant with the
   largest statement of lesson 3.

   With the July range, at the end of July, `Resources by type` shows 3
   servers, 8 volumes, 4 floating IPs, 2 routers, 1 image and 4 load balancers,
   `Volume capacity` reads 480 GB, and the three `Quota usage` gauges read 3 %
   of the instance quota, 6 % of the vCPU quota and 6 % of the memory quota.
   With `Last 3 hours`, `Recent lifecycle activity` shows the ingest spike
   split by event type.

4. `Tally / Reconciliation Drift`. Every query panel is empty (`No data`),
   because no sync ran in this track, and the `Drift interpretation` text panel
   says how to read them once one does.
   [Reconcile the simulated cloud](/how-to/simulator/reconcile-the-simulated-cloud)
   runs that loop against this same stack.

A panel reading `No data` where a value is named above has the wrong one of the
two time ranges, or the `cloud` variable is not on `os-sim`.

## Find the alerts

1. Read what vmalert holds as firing:

   ```sh
   curl --cacert tally-ca.crt https://vmalert.tally.127-0-0-1.nip.io:8443/api/v1/alerts | jq '.data.alerts[] | {alertname: .labels.alertname, state, job: .labels.job}'
   ```

   ```json
   {
     "alertname": "TallyScrapeTargetDown",
     "state": "firing",
     "job": "ceilometer"
   }
   ```

   This one alert has to be there, and nothing else should be firing.
   `TallyScrapeTargetDown` fires about five minutes after `make up` for the
   `ceilometer` job, whose target is a placeholder for an exporter that runs
   beside a real control plane, and that is the designed dev state rather than
   a fault to chase:
   [TallyScrapeTargetDown](/how-to/alerts/TallyScrapeTargetDown) is its runbook
   and [alert rules](/reference/observability/alert-rules) states every rule.
   `openstack-db-exporter` does not fire because the holding simulator serves
   its inventory.

   `TallyCloudEventsSilent` fires for `os-sim` once no event has been ingested
   for over an hour from a cloud that reported in the last 24 hours, so a
   reader who reaches this step more than an hour after lesson 2 sees it beside
   the first. That one is the holding simulator publishing nothing rather than
   a fault.

2. Read the same alert as Alertmanager holds it:

   ```sh
   curl --cacert tally-ca.crt https://alertmanager.tally.127-0-0-1.nip.io:8443/api/v2/alerts | jq '.[] | {alertname: .labels.alertname, runbook: .annotations.runbook}'
   ```

   ```json
   {
     "alertname": "TallyScrapeTargetDown",
     "runbook": "https://b42labs.github.io/tally/how-to/alerts/TallyScrapeTargetDown"
   }
   ```

   The runbook annotation
   `https://b42labs.github.io/tally/how-to/alerts/TallyScrapeTargetDown` has to
   match. vmalert is what evaluates the rules against the store, and
   Alertmanager is what routes what fires.

3. The two UIs render the same in the browser:
   `https://vmalert.tally.127-0-0-1.nip.io:8443/vmalert/` shows the rules, their
   state and their last evaluation, and
   `https://alertmanager.tally.127-0-0-1.nip.io:8443` shows the firing alerts as
   routed. Both sit behind the same certificate warning as Grafana.

4. Read `/api/v1/rules` when an alert you expect is missing from the list:

   ```sh
   curl --cacert tally-ca.crt https://vmalert.tally.127-0-0-1.nip.io:8443/api/v1/rules | jq '.data.groups[].rules[] | {name, state, lastError}'
   ```

   ```json
   {"name":"TallyCloudEventsSilent","state":"inactive","lastError":""}
   {"name":"TallyEventsRejected","state":"inactive","lastError":""}
   {"name":"TallySyncErrors","state":"inactive","lastError":""}
   {"name":"TallySyncStale","state":"inactive","lastError":""}
   {"name":"TallyReconciliationDriftHigh","state":"inactive","lastError":""}
   {"name":"TallyCollectorBufferAging","state":"inactive","lastError":""}
   {"name":"tally:current_resources:sum","state":"","lastError":""}
   {"name":"TallyResourceCountAnomaly","state":"inactive","lastError":""}
   {"name":"TallyRecordedSeriesMissing","state":"inactive","lastError":""}
   {"name":"TallyScrapeTargetDown","state":"firing","lastError":""}
   {"name":"TallyScrapeJobMissing","state":"inactive","lastError":""}
   {"name":"TallyExporterServiceSilent","state":"inactive","lastError":""}
   ```

   Every `lastError` is empty. A non-empty one names the query that failed, and
   `state` tells `firing` from `inactive`. `tally:current_resources:sum` is a
   recording rule and carries no state.

## What you learned

- Grafana reads the store through a read-only proxy in its own pod rather than
  reaching the store directly:
  [the datasource and the proxy](/explanation/grafana-and-the-read-only-proxy#the-datasource-and-the-proxy).
- The pushed series carry the simulated month's timestamps and the scraped ones
  the wall clock's, which is why the dashboards need two time ranges here:
  [two paths into one store](/explanation/the-openstack-metrics-pipeline#two-paths-into-one-store).
- vmalert evaluates the rules against the store and Alertmanager routes what
  fires: [two components](/explanation/alerting-design#two-components).
- `TallyScrapeTargetDown` asks whether a configured scrape target answered:
  [what the rules ask](/explanation/alerting-design#what-the-rules-ask).
- The drift dashboard reads a sync loop this track never ran, which is why its
  query panels are empty:
  [reconciliation](/explanation/dual-ingestion-and-reconciliation#reconciliation).

## Where to go next

[Discount a customer group](/tutorials/discount-a-customer-group) is the first
lesson of the billing track, which continues from the state this lesson leaves
behind.

[Tear down your local Tally](/tutorials/tear-down-your-local-tally) is the way
back to a clean machine, and it belongs to neither track: a reader going on to
the billing track runs it after that track's last lesson rather than here.

This is the state this track leaves behind:

- the kind cluster `tally` with the dev overlay;
- the compose stack with the simulator holding 84 notifications, so
  `GET /clock` on `http://127.0.0.1:8091/clock` answers `holding` true;
- the reporting database holding the month of seed 1 on `os-sim` minus the held
  share, with an empty project registry;
- the engine database holding the pricing model `2026-03` and one completed,
  not finalized run of `2026-07`;
- the JSON export of that run under `~/tally-tutorial/2026-07`;
- `tally-ca.crt` at the repository root;
- the VictoriaMetrics port-forward on `127.0.0.1:8428`;
- in the shell: `TALLY_REPORTING_DB_URL`, `TALLY_API_TOKEN`,
  `TALLY_ENGINE_DB_URL`, `TALLY_ENGINE_REPORTING_DB_URL`,
  `TALLY_ENGINE_COUNTER_SOURCES`, `TALLY_ENGINE_VM_URL` and `RUN_ID`.

The cluster's hourly `tally-engine` scheduler keeps ticking meanwhile. It moves
the periods along and may bill `2026-08` on its own once that month's grace
window has passed, which leaves a run of `2026-08` beside the one of `2026-07`
and changes nothing above.

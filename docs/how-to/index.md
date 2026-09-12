---
title: How-to guides
description: The shortest correct steps for a task, for a reader who knows what they want.
quadrant: how-to
audience: operator
---

# How-to guides

A how-to guide is a recipe. It gives the shortest correct path to one goal for
a reader who already knows what they want and only needs the steps in the right
order.

A guide does not teach you what the parts are; that is the job of a
[tutorial](/tutorials/). It opens with what it assumes is already running and
what you need at hand, and it closes with how to check that the result is what
you wanted. Between those two there are steps and nothing else.

The sidebar lists the guides by area, so the tasks that belong to one part of
Tally sit together.

## Guides by area

### OpenStack provider

- [Install the collector from the Debian package](/how-to/openstack/install-the-debian-package)
  puts one collector on a control node as a systemd service, with its
  credentials in files only it may read, and upgrades, removes or purges it
  again.
- [Connect the collector to an OpenStack cloud](/how-to/openstack/connect-the-collector)
  points one collector at one cloud, so its services publish the notifications
  the collector reads and that cloud's events arrive at the Reporting API.
- [Issue and revoke credentials](/how-to/openstack/issue-and-revoke-credentials)
  issues the ingest credential a collector reports under and the query token a
  reader holds, and revokes either one.
- [Verify a deployment with the dump](/how-to/openstack/verify-a-deployment-with-the-dump)
  prints what one deployment publishes, before a collector is pointed at it,
  and compares that against the notification mapping.
- [Reconcile a cloud](/how-to/openstack/reconcile-a-cloud) gives the Reporting
  API the clouds file, the account and the entry a sync of one cloud needs, and
  runs that sync.

### Engine

- [Migrate both databases](/how-to/engine/migrate-both-databases) brings the
  reporting and the engine schema to what their binaries expect, and rolls one
  chain back again.
- [Import a pricing model](/how-to/engine/import-a-pricing-model) loads one
  pricing model file into the engine database, where a run resolves the prices
  of the period it rates.
- [Run a period](/how-to/engine/run-a-period) meters and rates one billing
  month into the run that finalization, correction and export work on.
- [Finalize a period](/how-to/engine/finalize-a-period) closes one billing
  month, so its records turn immutable and the period stops taking new ones.
- [Detect late events](/how-to/engine/detect-late-events) reports the events
  that arrived after the run that bills a month read it.
- [Correct a finalized period](/how-to/engine/correct-a-finalized-period) meters
  a finalized month again, stores every non-zero difference as a delta and
  renders one credit note per affected project.
- [Export a run](/how-to/engine/export-a-run) writes one run's result into a
  directory, once as JSON documents and once as a CSV table.
- [Model a customer group with a discount](/how-to/engine/model-a-customer-group)
  registers a meta-project, relates the customer's projects to it and puts the
  group discount on those relations.
- [Export a rollup per customer group](/how-to/engine/export-a-rollup) sums one
  run's statements under the groups its projects belong to and writes one
  document per group beside the statements.

### Observability

- [Publish metrics over OTLP](/how-to/observability/publish-metrics-over-otlp)
  puts one cloud's Ceilometer samples into the metrics store on the push path,
  with the labels and the credential the receivers ask for.
- [Scrape the OpenStack exporters](/how-to/observability/scrape-the-openstack-exporters)
  points the store's two OpenStack scrape jobs at the exporters of one
  deployment, and takes Ceilometer to the store through a Pushgateway.
- [Install the Grafana dashboards](/how-to/observability/install-the-grafana-dashboards)
  publishes Grafana and the four provisioned dashboards on a cluster, and
  checks that the hostname answers and the dashboards render.
- [Fill the dashboards on a dev cluster](/how-to/observability/fill-the-dashboards)
  pushes real events and synthetic exporter series into a dev cluster until
  every panel of all four dashboards carries a value.
- [Deploy alerting and route notifications](/how-to/observability/deploy-alerting)
  brings up vmalert and Alertmanager with a deployment's own external URLs, and
  names the receiver an alert is handed to.

### Simulator

- [Run a simulated month against the dev cluster](/how-to/simulator/run-a-month)
  publishes one generated OpenStack month onto the broker the dev cluster's
  collector consumes from, paces it, and reads the month back through the
  Reporting API.
- [Replay a recorded month](/how-to/simulator/replay-a-recorded-month) writes a
  month to files with `run --out` and puts one of those files back on a broker,
  without the generator or the seed the month came from.
- [Compare an export against the oracle](/how-to/simulator/compare-an-export)
  holds the CSV export of a metered simulated month against `oracle.json`, the
  generator's own statement of that month.
- [Switch on fault switches](/how-to/simulator/switch-on-faults) runs a
  simulated month with fault switches on and reads what each one changes on the
  bus, in the counters and in the comparison.
- [Reconcile the simulated cloud](/how-to/simulator/reconcile-the-simulated-cloud)
  syncs the dev cluster's Reporting API against the OpenStack API a running
  month serves, at the instant the month stands at.
- [Register simulated projects](/how-to/simulator/register-simulated-projects)
  registers the tenants and the Gardener projects of a simulated month with the
  dev registry, and bills the month through the relations it wrote.

### Respond to an alert

- [TallyCloudEventsSilent](/how-to/alerts/TallyCloudEventsSilent) fires when a
  cloud that ingested collector events at some point in the last 24 hours has
  ingested none for more than an hour.
- [TallySyncStale](/how-to/alerts/TallySyncStale) fires when no reconciliation
  run of a cloud reached `completed` in the last 30 minutes.
- [TallyScrapeTargetDown](/how-to/alerts/TallyScrapeTargetDown) fires when a
  configured scrape target has answered nothing for five minutes.
- [TallyScrapeJobMissing](/how-to/alerts/TallyScrapeJobMissing) fires when one
  of the two discovered jobs resolves to no target at all, so the job leaves
  the target page instead of turning red.
- [TallyExporterServiceSilent](/how-to/alerts/TallyExporterServiceSilent) fires
  when the database exporter target is up while one of the five billed services
  emits no series for a cloud.

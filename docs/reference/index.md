---
title: Reference
description: The exact contract of every surface Tally exposes, checked against the code.
quadrant: reference
audience: integrator
---

# Reference

A reference page is a specification. It states the exact contract of one
surface: the fields, the types, the defaults, the errors and the guarantees. It
is dry and exhaustive, and it holds nothing else. Steps belong in
[How-to guides](/how-to/), and the reasoning belongs in
[Explanation](/explanation/).

A reference page here is produced from the code it describes or checked against
it, so the contract on the page and the contract in the binary cannot drift
apart unnoticed.

## Surfaces

### Reporting API

- [Endpoints](/reference/api/reporting-api) lists every route with the
  parameters it takes, the body it reads, the statuses it answers with and the
  credential it requires.
- [Schemas](/reference/api/reporting-api-schemas) states every component schema
  of that contract, property by property.

### Command lines

- [tally-reporting](/reference/command-line/tally-reporting) states what the
  API server process takes on the command line, what it serves and what it does
  on a signal.
- [tally-reporting-admin](/reference/command-line/tally-reporting-admin) lists
  the subcommands and flags that issue credentials, register virtual projects
  and migrate the reporting database.
- [tally-engine](/reference/command-line/tally-engine) lists the subcommands
  and flags of the engine, what each one reads from the environment and what it
  prints.
- [tally-openstack-collector](/reference/command-line/tally-openstack-collector)
  states the two modes of the collector, the queues it consumes, its HTTP
  routes and the bounds it applies to a notification.
- [tally-openstack-simulator](/reference/command-line/tally-openstack-simulator)
  states the subcommands of the simulator, its control endpoint, its fake
  OpenStack API, its fault switches and the files a run writes.
- [tally-vertical-slice](/reference/command-line/tally-vertical-slice) states
  the flags, the environment, the exit status and the output document of the
  vertical slice.

### Configuration

- [Reporting API settings](/reference/configuration/tally-reporting) lists every
  environment variable the API server and the admin CLI read.
- [Engine settings](/reference/configuration/tally-engine) lists every
  environment variable the metering engine reads.
- [OpenStack collector settings](/reference/configuration/tally-openstack-collector)
  lists every environment variable the collector reads.
- [OpenStack simulator settings](/reference/configuration/tally-openstack-simulator)
  lists every environment variable the simulator reads, and the subcommand that
  reads it.
- [Clouds file](/reference/configuration/clouds-file) states the YAML file that
  names the clouds the Reporting API reconciles, with the settings of the
  OpenStack adapter.
- [Counter sources file](/reference/configuration/counter-sources-file) states
  the YAML file that declares the counter metrics the engine measures per usage
  interval, and how each one is measured.

### Schemas and formats

- [Canonical event](/reference/formats/canonical-event) states the wire shape of
  an event, the bounds ingestion applies to it and what an ingest call answers.
- [Label convention](/reference/formats/label-convention) states the vocabulary
  and the labels every event, metric series and exporter carries.
- [Pricing model file](/reference/formats/pricing-model) states the YAML file a
  pricing catalog is imported from, its schema and how a price is written.
- [Pricing adjustments](/reference/formats/pricing-adjustments) states the
  `pricing_adjustments` array a relation carries, the order it is applied in and
  the lines it produces on a statement.
- [Export formats](/reference/formats/exports) states the files an export
  writes, their names, their JSON members and their CSV columns.
- [OpenStack notification mapping](/reference/formats/notification-mapping)
  states which oslo notification types the collector records, the event each one
  becomes, and how its state and size are read.

### Observability

- [Metrics](/reference/observability/metrics) lists every Prometheus series the
  Reporting API and the OpenStack collector expose, with its type, labels and
  meaning, and the scrape jobs that read them.
- [Alert rules](/reference/observability/alert-rules) lists every alerting and
  recording rule vmalert evaluates, with its expression, severity, wait and
  runbook, and the routing Alertmanager ships with.
- [Grafana dashboards](/reference/observability/dashboards) lists the
  provisioned dashboards, their uids, their variables and every panel with the
  expression it reads.

A block between the `<!-- refdoc:begin -->` and `<!-- refdoc:end -->` markers on
a page is rendered from the source it documents. The freshness tests render it
again on every run, and `make generate` writes the current rendering back into
the page.

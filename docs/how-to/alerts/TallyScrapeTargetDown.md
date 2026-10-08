---
title: TallyScrapeTargetDown
description: A configured scrape target has answered nothing for five minutes.
quadrant: how-to
audience: operator
---

# TallyScrapeTargetDown

`up == 0`, `for: 5m`.

## Symptom

A configured scrape target has answered nothing for five minutes. The `job` and
`instance` labels of the firing series name which one.

## Impact on billing

It depends on the job:

- `reporting-api`: every service metric is gone, and with it every rule that
  reads one, so the ingestion and reconciliation alerts go blind at the same
  time. The API may also be refusing ingest, in which case the collectors are
  filling their buffers.
- `otel-collector`: the pushed series are lost at the receiver, Ceilometer's
  OTLP path among them. Nothing buffers them on the way in.
- `openstack-collector`: the outbox gauges go dark while the collector keeps
  consuming, so a growing buffer is not reported until the target answers
  again. TallyCollectorBufferAging reads the gauge it no longer gets.
- `openstack-db-exporter` and `ceilometer`: a metering source runs empty. The
  inventory Tally bills from stops being observed for the cloud the job carries
  in its static labels.

## First checks

1. `/targets` on the VictoriaMetrics hostname, which names the error per
   target. On a dev cluster that is
   `https://vm.tally.127-0-0-1.nip.io:8443/targets`.
2. Pod status and log of the workload behind the target, and the Service and
   port names it is reached through.
3. For `openstack-db-exporter`, the limits on its database account
   ([the read-only database user](/how-to/observability/scrape-the-openstack-exporters#create-the-read-only-database-user)).
   A statement cap that ends the queries fails the scrape, while a connection
   cap that is too low leaves the target up and the scrape short, which is
   TallyExporterServiceSilent rather than this alert.
4. Whether this is a dev cluster. Three of its static targets are down there
   by design. `openstack-db-exporter` and `openstack-collector` read the
   simulator and the collector of the compose stack, which run only between
   `make simulator-up` and `make simulator-down`, and `ceilometer` is always
   down, because its target does not exist in that cluster.

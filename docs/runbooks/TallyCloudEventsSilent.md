# TallyCloudEventsSilent

`sum by (cloud) (increase(tally_events_ingested_total{source="collector"}[1h])) == 0 and sum by (cloud) (increase(tally_events_ingested_total{source="collector"}[24h])) > 0`, `for: 15m`.

## Symptom

A cloud that ingested collector events at some point in the last 24 hours has
ingested none for more than an hour. The second half of the expression is what
keeps a cloud that is idle by design, or one that has been decommissioned, from
firing this alert forever.

## Impact on billing

Every create, delete, and resize the cloud performed inside the gap is unbilled
or overbilled until a reconciliation run turns the difference into synthetic
events. A resource that is created and deleted inside the gap is invisible for
good: no run ever observes it, so nothing books it, and no later repair
recovers it. That is the accepted limitation of the event-driven design, listed
under [Known limitations](https://b42labs.github.io/tally/explanation/dual-ingestion-and-reconciliation#known-limitations) in the concept.

## First checks

1. The collector process beside the broker: `/healthz` and `/readyz` on
   `TALLY_OSC_HTTP_PORT`, the gauges `tally_collector_buffer_depth` and
   `tally_collector_oldest_buffered_seconds`, and the counter
   `tally_collector_delivery_errors_total`. A buffer that grows says the
   collector still consumes and cannot deliver; a readiness that fails names
   its reason in the log
   ([`openstack-collector.md`](../openstack-collector.md)).
2. The broker connection. `--dump` prints one line per delivery and reads
   nothing but the AMQP variables, so it says whether the bus carries
   notifications at all. It consumes through a queue of its own and takes no
   delivery away from a running collector.
3. The Reporting API as the collector reaches it: the base URL in
   `TALLY_OSC_REPORTING_URL`, and an ingest credential whose scope matches
   `TALLY_OSC_CLOUD`. An event outside that scope is refused with the reason
   `scope` and is never resent.
4. Whether reconciliation still runs for the cloud
   (`tally_sync_runs_total{status="completed"}`). It is what heals the gap, so
   a silent collector and a stale sync together cost more than either alone.

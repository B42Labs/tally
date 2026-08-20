# TallyExporterServiceSilent

`(up{job="openstack-db-exporter"} == 1) unless on (cloud) <series>`, joined by `or` over `openstack_nova_total_vms`, `openstack_cinder_volumes`, `openstack_neutron_floating_ips`, `openstack_glance_images`, and `openstack_loadbalancer_total_loadbalancers`, `for: 15m`.

## Symptom

The exporter target is up while one of the five billed services emits no series
for a cloud. The exporter answers 200, the scrape duration stays normal, and the
match on `cloud` keeps a healthy cloud from covering for another. At a 300
second scrape interval, `for: 15m` wants three short scrapes in a row.

## Impact on billing

That service is unmetered for the cloud while every scrape-health signal stays
green. Nothing on `/targets` and no `up` series shows it, so the gap surfaces
when the invoice comes up short.

## First checks

1. Which of the five series is missing for which cloud: one instant query per
   series against the store.
2. The exporter log for database errors of that service. A connection pool that
   loses the race gets `ERROR 1226`, its collectors emit nothing, and the scrape
   still answers 200.
3. The connection cap on the exporter's database account
   ([`openstack-metrics.md`](../openstack-metrics.md#the-read-only-database-user)).
   The exporter opens one pool per DSN, eight of them, and gathers the
   collectors in parallel, so a `MAX_USER_CONNECTIONS` at or below eight
   truncates every scrape by a different set of services.
4. The scrape duration on `/targets`. One near the job's 60 second
   `scrape_timeout` points at the queries, while a normal one with series
   missing points at the cap.

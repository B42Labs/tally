---
title: Reconcile the simulated cloud
description: Sync the dev cluster's Reporting API against the OpenStack API a running simulated month serves, once a minute, at the instant the month stands at.
quadrant: how-to
audience: contributor
---

# Reconcile the simulated cloud

This guide runs reconciliation against the cloud a simulated month serves. A
`run` answers the OpenStack listings a sync reads out of the same oracle a
comparison holds an export to, so the projection is corrected against the month
the generator built, which is described in
[the simulated OpenStack world](/explanation/the-simulated-openstack-world).

## Before you start

- A dev cluster from `make up` with a month publishing on the compose stack,
  which [run a simulated month](/how-to/simulator/run-a-month) starts. The fake
  API is up for exactly as long as the `run` publishes or holds.
- `kubectl` with the `kind-tally` context, plus `curl` and `jq` on the machine
  the loop runs on.
- `tally-dev-internal-token`, the internal token of the dev overlay, which the
  internal sync route is guarded by.
- The [simulator command line](/reference/command-line/tally-openstack-simulator)
  page, which lists the routes of
  [the fake OpenStack API](/reference/command-line/tally-openstack-simulator#the-fake-openstack-api)
  a run serves.
- The [simulator settings](/reference/configuration/tally-openstack-simulator)
  page, which names `TALLY_SIM_HTTP_ADDR` and `TALLY_SIM_HTTP_PORT`, the two the
  fake API and the control endpoint are served on.

## Wire the dev overlay to the fake API

1. Read the wiring the dev overlay ships.
   `deploy/kubernetes/overlays/dev/reconciliation/` holds the clouds config and
   the clouds.yaml the pod reads. The clouds config is generated into a
   ConfigMap and the clouds.yaml into a Secret, because that one carries the
   cloud's password, and the two are projected together at `/etc/tally/clouds`.
   The [clouds file example](/reference/configuration/clouds-file#example) is the
   entry that directory carries.

2. Take the two values the overlay sets. It sets `TALLY_REPORTING_SYNC_ALLOW_AT`
   to true, so the `at` member of a sync body is taken. The cloud is `os-sim`,
   the `SIM_CLOUD` default, reached from the pod in the kind node at
   `http://host.docker.internal:8091/v3`.

3. Edit both files of that directory for a drill under another cloud name. What
   an entry holds, and what the adapter needs of the account behind it, is in
   [reconcile a cloud](/how-to/openstack/reconcile-a-cloud).

## Run the sync loop

1. Start the port-forward and the loop in a second shell right after the stack's
   URLs print. It posts one sync per minute and tells each one where the month
   stands, which `GET /clock` answers under `virtual_now`, held at the
   `period_to` of the same document:

   ```sh
   kubectl --context kind-tally -n tally port-forward svc/reporting-api 8082:80 &
   while true; do
     doc="$(curl -sS --max-time 30 http://127.0.0.1:8091/clock)" \
       || echo "clock read failed at $(date -u +%FT%TZ)" >&2
     at="$(jq -r 'if .virtual_now > .period_to
                  then .period_to else .virtual_now end' <<<"$doc")"
     body="$(jq -nc --arg at "$at" '{at: $at}')"
     curl -sS --max-time 65 -X POST \
       -H "Authorization: Bearer tally-dev-internal-token" \
       -H 'Content-Type: application/json' -d "$body" \
       http://127.0.0.1:8082/internal/sync/os-sim \
       || echo "sync post failed at $(date -u +%FT%TZ) (at=$at)" >&2
     echo
     sleep 60
   done
   ```

   Sixty wall seconds are about 12.4 virtual hours at factor 744.

2. Stop the loop when `published` reaches `total`: Ctrl-C on the loop, then
   `kill %1` on the port-forward it started. What goes into a write-up is the
   totals the answers carried: `created`, `updated` and `deleted`.

## Check the result

1. Read the answer of an iteration that reached the Reporting API. One of four
   comes back. The first is the document below, with the counts the sync found:

   ```json
   {"sync_run_id": "...", "stats": {"created": 0, "updated": 0, "deleted": 0}}
   ```

   At factor 744 a sync corrects the resources whose notifications the collector
   has consumed and not posted yet: its outbox goes out every 5 seconds
   (`TALLY_OSC_FLUSH_INTERVAL_S`, 62 virtual minutes at this factor). Such a
   correction is a `sync.create` dated at the platform's instant, which is the
   instant the collector's own create carries, a `sync.update` dated at the told
   instant, or a `sync.delete` found by absence and dated there as well.
   `0, 0, 0` is what a sync answers when the outbox was empty at its instant.

2. Read a `409` with `a sync for this cloud is already running` as the previous
   iteration's run still holding the cloud. The next iteration waits it out.

3. Read a `500` with `the sync run failed` as a sync that was running when the
   fake API went away. A loop stopped after the run has ended gets the `400`
   below instead.

4. Read a `400` with `the request does not match the API contract` and `body.at`
   in its `errors` as `/clock` not answering: before the run listens and after it
   has ended. The `at` the `jq` derives is the empty string there, and the
   OpenAPI validation of the Reporting API refuses the body before the handler
   sees it.

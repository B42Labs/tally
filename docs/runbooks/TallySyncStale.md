# TallySyncStale

`sum by (cloud) (increase(tally_sync_runs_total{status="completed"}[30m])) == 0`, with no `for`, so it fires on the first evaluation that finds the window empty.

## Symptom

No reconciliation run of this cloud reached `completed` in the last 30 minutes.
The expected cadence is one run every 10 minutes, so the window covers three of
them and a single missed run does not fire the alert.

## Impact on billing

Drift stops being corrected: a resource the projection holds and the cloud no
longer has keeps accruing charges, and one the cloud holds and the projection
never learned of stays unbilled. Every event the collector lost stays lost for
as long as this holds, because a completed run is what books it.

## First checks

1. The Reporting API log for the sync runs of the cloud, and whatever calls
   `POST /internal/sync/{cloud}` on the schedule. A run that never starts and a
   run that starts and fails both leave this counter flat.
2. `tally_sync_errors_total` for the cloud and the `sync_runs` row of the last
   run. A run that recorded any error at all ends `failed`, is answered 500,
   and holds the reasons in that row.
3. The cloud's `adapter_config` entry, the clouds.yaml the pod reads, and
   whether Keystone and the service APIs answer. A failure that names no single
   resource type ends the whole run before a type is observed.
4. The 45 second budget a run is bounded at. A cloud whose enumeration outgrows
   it never completes a run, so every call is answered while the counter stays
   flat.

The adapter, its runs, and what a partial outage does to one are described in
[`openstack-reconciliation.md`](../openstack-reconciliation.md).

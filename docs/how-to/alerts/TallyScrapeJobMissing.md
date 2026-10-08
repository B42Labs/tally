---
title: TallyScrapeJobMissing
description: A discovered scrape job resolves to no target at all, so it leaves the target page instead of turning red.
quadrant: how-to
audience: operator
---

# TallyScrapeJobMissing

`absent(up{job="reporting-api"}) or absent(up{job="otel-collector"}) or absent(up{job="openstack-collector"})`,
`for: 5m`, in
[`scrape-rules.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/vmalert/scrape-rules.yaml),
which an overlay replaces together with its scrape config. The expression above
is the base's.

## Symptom

A discovered scrape job resolves to no target at all. `up` is a per-target
series, so zero targets produce no `up` series rather than `up == 0`: the job
disappears from `/targets` instead of turning red, and TallyScrapeTargetDown
stays silent.

## Impact on billing

The same as the job's targets being down, described in
[TallyScrapeTargetDown](/how-to/alerts/TallyScrapeTargetDown), without a red
target to see it by.

## First checks

1. The Deployment's replica count. A Deployment scaled to zero takes its job
   off the page.
2. The Service name and the port name the relabel rule keeps,
   `reporting-api;http`, `otel-collector;metrics` and `openstack-collector;http`
   in
   [`scrape.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/victoriametrics/scrape.yaml).
   A rename on either side drops every endpoint of the job.
3. The `victoriametrics-scrape` RoleBinding in
   [`victoriametrics.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/victoriametrics/victoriametrics.yaml).
   Without it the endpointslice discovery is refused and the job has nothing to
   keep.
4. The VictoriaMetrics log for discovery errors, an unreachable API server
   among them.
5. When the firing job is one the deployment does not scrape, compare the
   overlay's `scrape-rules.yaml` with its `scrape.yaml`. A clause left over from
   the base names a job the overlay never configured, and it fires for as long
   as the cluster runs. The overlay's file carries one `absent` clause per job
   of its `scrape.yaml` that has `kubernetes_sd_configs`, and nothing else.

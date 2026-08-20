# TallyScrapeJobMissing

`absent(up{job="reporting-api"}) or absent(up{job="otel-collector"})`, `for: 5m`.

## Symptom

One of the two discovered jobs resolves to no target at all. `up` is a
per-target series, so zero targets produce no `up` series rather than `up == 0`:
the job disappears from `/targets` instead of turning red, and
TallyScrapeTargetDown stays silent.

## Impact on billing

The same as the job's targets being down, described in
[TallyScrapeTargetDown](TallyScrapeTargetDown.md), without a red target to see
it by.

## First checks

1. The Deployment's replica count. A Deployment scaled to zero takes its job
   off the page.
2. The Service name and the port name the relabel rule keeps, `reporting-api;http`
   and `otel-collector;metrics` in
   [`scrape.yaml`](../../deploy/kubernetes/base/victoriametrics/scrape.yaml). A
   rename on either side drops every endpoint of the job.
3. The `victoriametrics-scrape` RoleBinding in
   [`victoriametrics.yaml`](../../deploy/kubernetes/base/victoriametrics/victoriametrics.yaml).
   Without it the endpointslice discovery is refused and the job has nothing to
   keep.
4. The VictoriaMetrics log for discovery errors, an unreachable API server
   among them.

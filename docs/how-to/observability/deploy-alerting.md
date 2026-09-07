---
title: Deploy alerting and route notifications
description: "Publish vmalert and Alertmanager with a deployment's own external URLs, name the receiver an alert is delivered to, and reach both on a dev cluster."
quadrant: how-to
audience: operator
---

# Deploy alerting and route notifications

This guide brings up the two components that turn the metrics store into
notifications, and says where a notification goes. At the end vmalert evaluates
the repository's rules against VictoriaMetrics, Alertmanager groups what fires
and hands it to a receiver the deployment names, and both UIs answer on the
hostnames the overlay patches. What the rules ask and why the pair is split
this way is in [alerting design](/explanation/alerting-design).

## Before you start

- A cluster with VictoriaMetrics running, which every rule expression is
  evaluated against.
- An overlay of your own over `deploy/kubernetes/base`, since a deployment
  patches the external URLs and replaces the Alertmanager config rather than
  editing the base.
- Docker on the host, which is all `make check-alerting` needs.
- A shell at the repository root for `make up` and `make -s ca`, and `kubectl`
  against the cluster for the silence.
- The [alert rules](/reference/observability/alert-rules) reference page, which
  states every rule with its expression, its `for` and its labels.

## Publish both components

1. Patch the two external URLs in the overlay, one per component. The base sets
   `VMALERT_EXTERNAL_URL` to the placeholder `https://vmalert.tally.example.com`
   and `ALERTMANAGER_EXTERNAL_URL` to `https://alertmanager.tally.example.com`,
   and an unpatched value sends a reader who follows a Source link to a host
   that answers nothing:

   ```yaml
   patches:
     - patch: |
         apiVersion: apps/v1
         kind: Deployment
         metadata:
           name: vmalert
         spec:
           template:
             spec:
               containers:
                 - name: vmalert
                   env:
                     - name: VMALERT_EXTERNAL_URL
                       value: https://vmalert.tally.127-0-0-1.nip.io:8443

     # The same placeholder on the other side of the pair.
     - patch: |
         apiVersion: apps/v1
         kind: StatefulSet
         metadata:
           name: alertmanager
         spec:
           template:
             spec:
               containers:
                 - name: alertmanager
                   env:
                     - name: ALERTMANAGER_EXTERNAL_URL
                       value: https://alertmanager.tally.127-0-0-1.nip.io:8443
   ```

   The pair above is the one in
   [`deploy/kubernetes/overlays/dev/kustomization.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/overlays/dev/kustomization.yaml).
   Each overlay patches the variable rather than the argument, because
   `-external.url` reads its value from the argument and Kubernetes expands
   `$(VAR)` in an argument from the container's own environment.

2. Patch the route hostname of each component together with its variable. The
   base leaves both hostnames at `vmalert.tally.example.com` and
   `alertmanager.tally.example.com`, and both external URLs at the matching
   placeholder, which is the pairing the dev overlay keeps.

3. Neither route carries a credential. Whatever reaches the Alertmanager
   hostname reads every firing alert with its labels, which by the rules of
   `rules.yaml` render the cloud names and the exporter and target addresses of
   the OpenStack control plane, and the receivers the deployment delivers to;
   whatever reaches the vmalert hostname reads every rule expression and every
   active alert with the same labels. What each route withholds — the write
   paths, `/api/v2/status` and `/metrics` on the one, the root prefix with
   `/flags`, `/metrics` and `/debug/pprof` on the other — is in
   [what the dev route publishes](/explanation/alerting-design#what-the-dev-route-publishes);
   the reads above are not among them. On the dev overlay both hostnames
   resolve to `127.0.0.1` and the Gateway is bound to the developer's own
   machine. A deployment that puts them anywhere else puts an authenticating
   proxy in front of them, or publishes neither route and reaches both from
   inside the cluster.

## Name a receiver

1. Add the deployment's own delivery by replacing the generated ConfigMap from
   the overlay rather than by patching the file in the base:

   ```yaml
   configMapGenerator:
     - name: alertmanager-config
       behavior: replace
       files:
         - config.yaml
   ```

2. Carry the `webhook_configs` or the `email_configs` under the receiver of the
   overlay's own `config.yaml`. That is the mechanism
   [replace the scrape targets](/how-to/observability/scrape-the-openstack-exporters#replace-the-scrape-targets)
   records for the scrape config, used here for the same reason: a file the
   base owns stays the base's, and the deployment's copy is a file of its own.
   Which route a delivery is reached through is in
   [where an alert is delivered](/explanation/alerting-design#where-an-alert-is-delivered).

## Edit the rules

1. Edit `rules.yaml` or `config.yaml` in its component's directory. Neither
   file is applied as a file: each is the source of a `configMapGenerator` in
   its component's `kustomization.yaml`, generating `vmalert-rules` and
   `alertmanager-config`, and kustomize appends a content hash to each name.
   Editing a file changes the generated name, which changes the pod spec that
   mounts it, which rolls the pod. Neither component is left evaluating rules
   or routing by a config that no longer matches the tree.

2. Validate both files before the commit:

   ```sh
   make check-alerting
   ```

   It loads `rules.yaml` into vmalert with `-dryRun` and runs
   `amtool check-config` over `config.yaml`, each in the image the cluster
   runs, so an expression or a routing field the pinned version refuses fails
   here rather than in the cluster.

## Reach both on a dev cluster

1. Bring the cluster up. `make up` publishes both on the dev overlay's
   hostnames:

   ```text
   https://vmalert.tally.127-0-0-1.nip.io:8443/vmalert/
   https://alertmanager.tally.127-0-0-1.nip.io:8443
   ```

2. Reach either with the dev CA that signed the Gateway's certificate:

   ```sh
   make -s ca > tally-ca.crt
   curl --cacert tally-ca.crt \
     https://vmalert.tally.127-0-0-1.nip.io:8443/api/v1/rules
   ```

## Silence an alert

1. Create the silence from inside the cluster, with the `amtool` the image
   ships:

   ```sh
   kubectl --context kind-tally -n tally exec statefulset/alertmanager -- \
     amtool --alertmanager.url=http://127.0.0.1:9093 silence add \
     --author=<who> --comment='<why>' --duration=2h alertname=<name>
   ```

   Which write paths the dev route refuses, and how a refused one is answered,
   is in
   [what the dev route publishes](/explanation/alerting-design#what-the-dev-route-publishes).

## Check the result

1. About five minutes after `make up`, `TallyScrapeTargetDown` fires for the
   jobs `openstack-db-exporter` and `ceilometer`. Both are static targets for
   exporters that run beside an OpenStack control plane rather than in this
   cluster, which is the designed dev state described under
   [replace the scrape targets](/how-to/observability/scrape-the-openstack-exporters#replace-the-scrape-targets),
   and not a fault to chase. What to do when it fires against a deployment is
   in its runbook,
   [`TallyScrapeTargetDown`](/how-to/alerts/TallyScrapeTargetDown).

2. No other rule fires on a cluster nothing has reported to.
   `TallyScrapeJobMissing` stays silent because both discovered jobs resolve to
   targets, and `TallyExporterServiceSilent` needs an exporter target that
   answers a scrape, which is the target that is down. The remaining seven read
   `tally_` series the store does not carry yet, and an expression over nothing
   returns nothing. `TallyRecordedSeriesMissing` is quiet for a different
   reason: its `absent()` clause is true here, and the
   `count(tally_current_resources) > 0` it is paired with is what keeps it from
   reporting a cluster that has not reconciled yet as a stalled write path.

3. When an alert you expect does not appear, read `/api/v1/rules` on the
   vmalert hostname and find the rule. A non-empty `lastError` on it means the
   datasource query failed rather than that the condition was false, and the
   message names what VictoriaMetrics answered.

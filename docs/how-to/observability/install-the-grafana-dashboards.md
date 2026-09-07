---
title: Install the Grafana dashboards
description: "Publish Grafana with its four provisioned dashboards on a cluster, sign in, and check that it answers and renders."
quadrant: how-to
audience: operator
---

# Install the Grafana dashboards

This guide publishes Grafana and the four dashboards the repository provisions
into the folder `Tally`. At the end the hostname answers, the dashboards are
there, and an edit to a provisioned file reaches the pod. What the datasource
may reach and what the route refuses is in
[Grafana and the read-only proxy](/explanation/grafana-and-the-read-only-proxy).

## Before you start

- A cluster with VictoriaMetrics running, which every panel queries through the
  datasource.
- An overlay that patches the Grafana route hostname, or a dev cluster, which
  the dev overlay already patches.
- `kubectl` against that cluster, and a shell at the repository root for
  `make up` and `make -s ca`.
- The [Grafana dashboards](/reference/observability/dashboards) reference page,
  which states the four dashboards, their panels and their variables.

## Publish Grafana

1. Bring the cluster up:

   ```sh
   make up
   ```

   It publishes Grafana on the dev overlay's hostname:

   ```text
   https://grafana.tally.127-0-0-1.nip.io:8443
   ```

2. Patch the HTTPRoute hostname in the overlay and set `GF_SERVER_ROOT_URL` to
   that same URL including the `:8443` host port, which is where kind publishes
   https. A link Grafana renders without the port sends the browser to a closed
   port. The dev overlay carries both, which is why `make up` is enough on a
   dev cluster:

   ```yaml
   env:
     - name: GF_SERVER_ROOT_URL
       value: https://grafana.tally.127-0-0-1.nip.io:8443
   ```

3. Expect a login page from the base alone. It sets no `GF_AUTH_ANONYMOUS_*`
   variable and leaves the hostname at the placeholder
   `grafana.tally.example.com`, so Grafana serves one until an overlay says
   otherwise.

## Sign in

1. Set the two anonymous variables in the overlay, so that a request without a
   session renders every dashboard and saves nothing:

   ```yaml
   env:
     - name: GF_AUTH_ANONYMOUS_ENABLED
       value: "true"
     - name: GF_AUTH_ANONYMOUS_ORG_ROLE
       value: Viewer
   ```

2. Sign in as `admin` to change anything. On a dev cluster the password is
   `tally-dev-grafana-password`, the `admin-password` key of the generated
   `tally-grafana` Secret in
   [`overlays/dev/kustomization.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/overlays/dev/kustomization.yaml).

## Edit a provisioned file

1. Edit the datasource, the provider or a dashboard in the tree. The three are
   `configMapGenerator` entries in
   [`kustomization.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/base/grafana/kustomization.yaml),
   and kustomize appends a content hash to each generated ConfigMap name.

2. Apply the overlay and watch the pod roll. The changed name changes the pod
   spec that mounts it, so Grafana is never left serving a datasource or a
   dashboard that no longer matches the file in the tree:

   ```sh
   kubectl -n tally rollout status deployment/grafana
   ```

   ```text
   deployment "grafana" successfully rolled out
   ```

## Check the result

1. Ask Grafana for its health. The call needs the dev CA that signed the
   Gateway's certificate:

   ```sh
   make -s ca > tally-ca.crt
   curl --cacert tally-ca.crt https://grafana.tally.127-0-0-1.nip.io:8443/api/health
   ```

   It answers 200 with a JSON body whose `database` member is `ok`.

2. Open `https://grafana.tally.127-0-0-1.nip.io:8443` and go to the folder
   `Tally`. The four dashboards it carries, with every panel and the expression
   the panel reads, are on the
   [Grafana dashboards](/reference/observability/dashboards) page.

3. On a cluster nothing has reported to, every panel backed by an exporter
   series or a collector series shows "No data" rather than a query error. The
   `platform` and `cloud` variables read
   `label_values(tally_current_resources, ...)`, which resolves to nothing while
   the projection is empty. What keeps an empty variable from breaking a panel
   is its "All" value `.*`: a selector `cloud=~".*"` matches whatever the store
   holds, including nothing.

4. The scrape-health stat on Tally / Ingestion Health reports `up == 0` for
   `ceilometer`, a static target for an exporter that runs beside an OpenStack
   control plane rather than in this cluster, and for `openstack-db-exporter`
   until a simulator run publishes a month, when that job scrapes the
   simulator's inventory. That is the designed dev state, described under
   [replace the scrape targets](/how-to/observability/scrape-the-openstack-exporters#replace-the-scrape-targets),
   and not a fault to chase.

5. To put values on all four dashboards without a cloud behind them, follow
   [fill the dashboards on a dev cluster](/how-to/observability/fill-the-dashboards).

---
title: Use another Gateway API implementation
description: "Deploy the prod overlay behind Traefik or another Gateway API implementation, without the kustomize component that holds the Envoy Gateway objects."
quadrant: how-to
audience: operator
---

# Use another Gateway API implementation

This guide deploys the prod overlay to a cluster whose Gateway API
implementation is not Envoy Gateway. The base is plain Gateway API. What needs
Envoy Gateway is in the kustomize component
`deploy/kubernetes/components/envoy-gateway`, and this guide takes that
component out of the overlay.

The steps were checked with Traefik `v3.7.13` from Helm chart `41.6.1`, set
with `providers.kubernetesGateway.enabled=true` and `gateway.enabled=false`,
on kind `v0.32.0` with the standard channel of Gateway API `v1.6.2`. The check
covered the Gateway, the four HTTPRoutes and the GRPCRoute of the overlay: the
Gateway is accepted and programmed, every route attaches, plain HTTP is
redirected, and a hostname no route names is answered 404. It did not cover
certificate issuance through the listener, OTLP traffic or a LoadBalancer.

## Before you start

- A Gateway API implementation on the cluster, with a GatewayClass and the v1
  kinds `Gateway`, `HTTPRoute` and `GRPCRoute`.
- cert-manager with its Gateway API support switched on, which is
  `config.gatewayAPI.enabled=true` in its Helm chart. Install it after the
  Gateway API resource types exist, because it looks for them at startup.
- Everything else
  [Deploy the stack to a cluster](/how-to/cluster/deploy-the-collecting-stack#before-you-start)
  asks for, apart from `helm` for `make prod-addons`.
- The sections "Cut the release that publishes the images", "Set the domain",
  "Name the cloud", "Configure reconciliation" and "Write the secrets" of that
  guide, done. `kubectl apply -k` reads the `.env` files, `collector.env`,
  `secrets/clouds.yaml` and `reconciliation/clouds-config.yaml`.

## Take the component out of the overlay

1. In `deploy/kubernetes/overlays/prod/kustomization.yaml`, remove the
   `envoy-gateway` line of the `components` entry and keep the other three:
   the collector, the migration Job and the sync:

   ```yaml
   components:
     - ../../components/openstack-collector
     - ../../components/migrations
     - ../../components/reconciliation
   ```

2. In the same file, remove the patch that deletes
   `HTTPRouteFilter alertmanager-deny-writes`:

   ```yaml
     - patch: |
         apiVersion: gateway.envoyproxy.io/v1alpha1
         kind: HTTPRouteFilter
         metadata:
           name: alertmanager-deny-writes
         $patch: delete
   ```

   The component declares that filter. With the patch left in,
   `kubectl kustomize deploy/kubernetes/overlays/prod` stops:

   ```text
   error: no resource matches strategic merge patch "HTTPRouteFilter.v1alpha1.gateway.envoyproxy.io/alertmanager-deny-writes.[noNs]": no matches for Id HTTPRouteFilter.v1alpha1.gateway.envoyproxy.io/alertmanager-deny-writes.[noNs]; failed to find unique target for patch HTTPRouteFilter.v1alpha1.gateway.envoyproxy.io/alertmanager-deny-writes.[noNs]
   ```

## Name your class and ports

1. List the classes of the cluster:

   ```sh
   kubectl --context <ctx> get gatewayclass
   ```

   On a cluster with Traefik:

   ```text
   NAME      CONTROLLER                      ACCEPTED   AGE
   traefik   traefik.io/gateway-controller   True       12s
   ```

2. Add a replacement of `/spec/gatewayClassName` to the JSON patch on
   `Gateway tally` in `deploy/kubernetes/overlays/prod/kustomization.yaml`,
   with the name of your class:

   ```yaml
     - target:
         group: gateway.networking.k8s.io
         kind: Gateway
         name: tally
       patch: |
         - op: remove
           path: /spec/listeners/2
         - op: replace
           path: /spec/gatewayClassName
           value: traefik
   ```

3. Where the implementation binds a listener to a port of its own, replace the
   ports of the `http` and the `https` listener in the same patch. Traefik's
   chart serves its entry points on 8000 and 8443:

   ```yaml
         - op: replace
           path: /spec/listeners/0/port
           value: 8000
         - op: replace
           path: /spec/listeners/1/port
           value: 8443
   ```

   With a port that matches no entry point, Traefik refuses the listener. The
   Gateway then reports `Accepted` as `False` with the reason `PortUnavailable`
   under `status.listeners`, and this message:

   ```text
   Cannot find entryPoint for Gateway: no matching entryPoint for port 80 and protocol "HTTP"
   ```

## Replace what the component did

1. Limit both OTLP hostnames, `otlp.` and `otlp-grpc.`, to 10 requests a second
   per client address, with the means of your implementation.
   [What is exposed and what is not](/explanation/the-openstack-metrics-pipeline#what-is-exposed-and-what-is-not)
   says what the limit protects.

2. Answer `/api/datasources/proxy` on the Grafana hostname with a 403, with the
   means of your implementation.
   [What the route publishes](/explanation/grafana-and-the-read-only-proxy#what-the-route-publishes)
   says what the refusal is for.

The component's other two objects need no replacement: the cluster declares
the GatewayClass, and the overlay does not publish Alertmanager.

Nothing else on this page fails without the two steps. Checks 4 to 6 under
[Check the result](#check-the-result) are what shows that both are in place.

## Deploy

`make prod-addons` and `make prod-up` are not used here. The first installs
Envoy Gateway, and the second waits on the LoadBalancer Service Envoy Gateway
creates.

1. Check that each of the six `.env` files carries every key of its example,
   and that no value is empty or still a placeholder. `make prod-up` refuses
   such a file, and `kubectl apply -k` applies it: the pod that mounts a key
   the Secret lacks does not start, and with an empty `admin-password` Grafana
   keeps its default admin password. The loop prints the file and the key of
   every key a file lacks, and the `grep` those of every value to fill in:

   ```sh
   for example in deploy/kubernetes/overlays/prod/secrets/*.env.example; do
     for key in $(sed -n 's/^\([^#=][^=]*\)=.*/\1/p' "$example"); do
       grep -q "^$key=" "${example%.example}" || echo "${example%.example}:$key"
     done
   done
   grep -EH '^[^#=]+=([[:space:]]*$|.*<[a-z-]+>)' \
     deploy/kubernetes/overlays/prod/secrets/*.env | cut -d= -f1
   ```

   Both print nothing when every key is there and every value is filled in. A
   line such as this one names a key to copy from the example, or a value to
   fill in, before the next step:

   ```text
   deploy/kubernetes/overlays/prod/secrets/tally-grafana.env:admin-password
   ```

2. Check that `collector.env` names the cloud. `make prod-up` refuses an empty
   `TALLY_OSC_CLOUD` and one with whitespace around it, and `kubectl apply -k`
   applies both: the collector exits with `checking the configuration:
   TALLY_OSC_CLOUD: must be set` on the first, and the ConfigMap keeps the
   whitespace of the second, so the sync asks for a cloud the clouds config
   does not name. The command counts the lines that set a value without
   whitespace around it:

   ```sh
   grep -Ec '^TALLY_OSC_CLOUD=[^[:space:]]+$' deploy/kubernetes/overlays/prod/collector.env
   ```

   ```text
   1
   ```

3. Check the input of reconciliation. `make prod-up` refuses each of the
   three, and `kubectl apply -k` applies them: a placeholder fails every sync
   with a 500, a cloud other than the one of `collector.env` is answered 404,
   and an empty `cloud` ends the Reporting API at startup. The first command
   counts the placeholders left in `secrets/clouds.yaml`, the second the
   `cloud` lines that name the cloud of `collector.env`, and the third the
   `os_cloud` lines that name an entry:

   ```sh
   grep -Ec '<[a-z-]+>' deploy/kubernetes/overlays/prod/secrets/clouds.yaml
   grep -cxF "  - cloud: $(sed -n 's/^TALLY_OSC_CLOUD=//p' deploy/kubernetes/overlays/prod/collector.env)" \
     deploy/kubernetes/overlays/prod/reconciliation/clouds-config.yaml
   grep -Ec '^      os_cloud: [^[:space:]]+$' deploy/kubernetes/overlays/prod/reconciliation/clouds-config.yaml
   ```

   ```text
   0
   1
   1
   ```

4. Delete the migration Job of the previous deploy once it has finished. A
   Job's pod template is immutable, so the apply of a new tag fails on the old
   Job. The block is the one of
   [deploy from a pipeline](/how-to/cluster/deploy-the-collecting-stack#deploy-from-a-pipeline):
   it leaves a Job that is still running alone, because a migration that is
   killed can leave a half-built index, and stops on it:

   ```sh
   (
     state="$(kubectl --context <ctx> -n tally get job tally-migrate --ignore-not-found \
       -o jsonpath='{.metadata.name}:{.status.conditions[?(@.status=="True")].type}')" \
       || { echo "cluster read failed; not deleting" >&2; exit 1; }
     case "$state" in
       ''|*Complete*|*Failed*)
         kubectl --context <ctx> -n tally delete job tally-migrate --ignore-not-found ;;
       *)
         echo "tally-migrate has not finished; wait for it before deploying" >&2
         exit 1 ;;
     esac
   )
   ```

5. Apply the certificate issuer and the overlay:

   ```sh
   kubectl --context <ctx> apply -f deploy/kubernetes/overlays/prod/issuers.yaml
   kubectl --context <ctx> apply -k deploy/kubernetes/overlays/prod
   ```

6. Wait for the migration Job, which applies both chains in the cluster, and
   then for the Reporting API:

   ```sh
   kubectl --context <ctx> -n tally wait --for=condition=complete job/tally-migrate --timeout=30m
   kubectl --context <ctx> -n tally rollout status deployment/reporting-api
   ```

   The Job reads nothing of the Gateway. When it fails,
   `kubectl --context <ctx> -n tally logs -l batch.kubernetes.io/job-name=tally-migrate --all-containers --prefix --tail=-1`
   prints why.

7. Issue the ingest credential into the Secret the collector waits for, as
   [issue an ingest credential](/how-to/cluster/deploy-the-collecting-stack#issue-an-ingest-credential)
   shows. The collector pod stays in `ContainerCreating` until then. The
   credential needs the migrated database of the step before, and nothing of
   the Gateway.

8. Point the domain at the address your implementation publishes the Gateway
   on, and wait for the certificate, as
   [point the domain at the cluster](/how-to/cluster/deploy-the-collecting-stack#point-the-domain-at-the-cluster)
   shows.

## Check the result

1. The overlay renders no object of Envoy Gateway:

   ```sh
   kubectl kustomize deploy/kubernetes/overlays/prod | grep -c envoyproxy
   ```

   ```text
   0
   ```

2. The Gateway is programmed. The `PROGRAMMED` column reads `True`:

   ```sh
   kubectl --context <ctx> -n tally get gateway tally
   ```

3. Run the checks of
   [Deploy the stack to a cluster](/how-to/cluster/deploy-the-collecting-stack#check-the-result).
   Its last check runs `make prod-up`, so leave that one out.

4. Each OTLP hostname answers a burst from one address with 429. Each command
   sends 40 requests at once, without a credential:

   ```sh
   seq 40 | xargs -P 40 -I{} curl -s -o /dev/null -w '%{http_code}\n' -X POST \
     https://otlp.tally.demo.b42labs.com/v1/metrics | sort | uniq -c
   seq 40 | xargs -P 40 -I{} curl -s -o /dev/null -w '%{http_code}\n' -X POST \
     https://otlp-grpc.tally.demo.b42labs.com/ | sort | uniq -c
   ```

   The dev stack, behind Envoy Gateway and the component's limit, prints:

   ```text
     10 401
     30 429
     10 415
     30 429
   ```

   `401` and `415` are the collector's own answers, to a request without a
   credential and to one that is not gRPC. An output without `429` means that
   hostname has no limit and every request reached the collector: with the
   limit taken away, the dev stack prints `40 401` and `40 415`.

5. The limit counts per client address. The `429` of the check above shows
   that a limit is there, and no more: one bucket for the whole hostname
   answers a burst from one address the same way, and so does a per-address
   limit behind a LoadBalancer that hides the client's address. Run this on
   one machine, then on two machines with different public addresses at the
   same time. It sends ten such bursts, one a second:

   ```sh
   for i in $(seq 10); do
     seq 40 | xargs -P 40 -I{} curl -s -o /dev/null -w '%{http_code}\n' -X POST \
       https://otlp.tally.demo.b42labs.com/v1/metrics
     sleep 1
   done | sort | uniq -c
   ```

   The dev stack, asked from two addresses at once, prints for each what one
   address gets alone:

   ```text
    100 401
    300 429
   ```

   The counts are not exact. A bucket refills while a burst is still
   arriving, so a slower path gets more `401`, and two runs from one address
   differ by a few: with each burst spread over 200 ms, the dev stack prints
   between 110 and 116. Judge by the sum of the two machines, because one of
   two that share a bucket can keep most of its count. Under a limit per
   client address the two get between them about twice what one gets alone.
   Machines that share a bucket get between them about what one gets alone,
   and any sender holds that bucket empty for every publisher without a
   credential. With the component's rule for distinct addresses taken away,
   the dev stack prints `26 401` for one address and `82 401` for the other,
   108 between them and not 200. Repeat the check with the `otlp-grpc.` URL
   of check 4, where `415` takes the place of `401`.

6. The Gateway refuses the datasource proxy before Grafana sees the request:

   ```sh
   curl -sS -o /dev/null -w '%{http_code}\n' \
     https://grafana.tally.demo.b42labs.com/api/datasources/proxy/uid/x/
   ```

   ```text
   403
   ```

   `401` is Grafana's own answer to that request, and means the prefix is
   published.

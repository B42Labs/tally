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
  [Deploy the collecting stack to a cluster](/how-to/cluster/deploy-the-collecting-stack#before-you-start)
  asks for, apart from `helm` for `make prod-addons`.
- The sections "Cut the release that publishes the images", "Set the domain"
  and "Write the secrets" of that guide, done. `kubectl apply -k` reads the
  `.env` files, and `make prod-migrate` refuses to run from a checkout that is
  not at the tag `newTag` names.

## Take the component out of the overlay

1. In `deploy/kubernetes/overlays/prod/kustomization.yaml`, remove the
   `components` entry:

   ```yaml
   components:
     - ../../components/envoy-gateway
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

1. Check that no value in the five `.env` files is empty or still a
   placeholder. `make prod-up` refuses such a file, and `kubectl apply -k`
   applies it: with an empty `admin-password` Grafana keeps its default admin
   password. The command prints the file and the key of every such value:

   ```sh
   grep -EH '^[^#=]+=([[:space:]]*$|.*<[a-z-]+>)' \
     deploy/kubernetes/overlays/prod/secrets/*.env | cut -d= -f1
   ```

   It prints nothing when every value is filled in. A line such as this one
   names a value to fill in before the next step:

   ```text
   deploy/kubernetes/overlays/prod/secrets/tally-grafana.env:admin-password
   ```

2. Apply the certificate issuer and the overlay:

   ```sh
   kubectl --context <ctx> apply -f deploy/kubernetes/overlays/prod/issuers.yaml
   kubectl --context <ctx> apply -k deploy/kubernetes/overlays/prod
   ```

3. Wait for the database, apply the reporting migration chain and wait for the
   Reporting API:

   ```sh
   kubectl --context <ctx> -n tally rollout status statefulset/timescaledb
   make prod-migrate PROD_CONTEXT=<ctx>
   kubectl --context <ctx> -n tally rollout status deployment/reporting-api
   ```

   `make prod-migrate` reaches the database through a port-forward and reads
   nothing of the Gateway.

4. Point the domain at the address your implementation publishes the Gateway
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
   [Deploy the collecting stack to a cluster](/how-to/cluster/deploy-the-collecting-stack#check-the-result).
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

---
title: Deploy the collecting stack to a cluster
description: "Put the Reporting API, the OTLP endpoint and Grafana on a dedicated cluster from a laptop, behind Let's Encrypt certificates, with the secrets kept out of the repository."
quadrant: how-to
audience: operator
---

# Deploy the collecting stack to a cluster

This guide deploys the collecting half of Tally to a dedicated Kubernetes
cluster: the Reporting API with TimescaleDB, VictoriaMetrics, the OTel
Collector, Grafana, vmalert and Alertmanager. The Gateway publishes `api.`,
`otlp.`, `otlp-grpc.` and `grafana.` over HTTPS, behind a certificate Let's
Encrypt signs. The metering scheduler does not run there, and the store,
vmalert and Alertmanager are reached through port-forwards. Every step runs
from a checkout on your machine with three make targets, and no CI job touches
the cluster. What the prod overlay changes against the dev overlay is in
[where the dev stack ends](/contributing/dev-stack#where-the-dev-stack-ends).

## Before you start

- A kubectl context for the cluster, with a LoadBalancer implementation and a
  default StorageClass. The commands below write it as `<ctx>`, and every prod
  target refuses to run while `PROD_CONTEXT` is empty. The LoadBalancer has to
  pass the client's address on to the Gateway rather than proxy the
  connection: the OTLP endpoint limits requests per client address, and behind
  a proxying LoadBalancer every publisher shares one limit.
- `helm`, which `make prod-addons` installs the add-ons with.
- Go at the version `go.mod` states, because the migration and the admin CLI
  run with `go run` from the checkout.
- `nc`, which `make prod-up` probes its port-forward to the database with.
- `openssl` from OpenSSL 3 and `htpasswd`, for the secrets and the certificate
  check. The LibreSSL that macOS ships has no `x509 -ext`.
- Docker and `curl`, for the checks.
- A DNS zone for the domain, in which you can create a wildcard record.
- A release tag whose images are in the registry, or a commit whose `ci` run is
  green to cut one from.
- A checkout of the repository at that tag. `go run` migrates with the chain of
  the checkout, which has to match the image the cluster runs, so
  `make prod-up` and `make prod-migrate` refuse to run from any other commit.

## Cut the release that publishes the images

1. Tag the commit and push the tag. The `release` workflow runs on every tag
   matching `v*` and pushes `ghcr.io/b42labs/tally-reporting:<tag>` and
   `ghcr.io/b42labs/tally-engine:<tag>`:

   ```sh
   git tag <tag>
   git push origin <tag>
   ```

   Wait until the run of the tag has finished on the repository's Actions tab.
   [Releases](/contributing/toolchain#releases) describes what the run builds.

2. Check that the image pulls without a login, which is how the cluster's nodes
   pull it:

   ```sh
   docker logout ghcr.io
   docker pull ghcr.io/b42labs/tally-reporting:<tag>
   ```

3. When the pull is denied, the package is still private: the first push of an
   image creates its package that way. An organisation admin opens the B42Labs
   organisation on GitHub, then Packages, the `tally-reporting` package and
   Package settings, and makes the package public under "Change visibility".
   The same goes for `tally-engine`. A public package cannot be made private
   again. Run the pull of step 2 again afterwards.

4. Check out the tag, and set `newTag` in
   `deploy/kubernetes/overlays/prod/kustomization.yaml` to it when it names
   another one:

   ```sh
   git checkout <tag>
   ```

   ```yaml
   images:
     - name: tally-reporting
       newName: ghcr.io/b42labs/tally-reporting
       newTag: v0.2.0
   ```

## Set the domain

1. Edit the six values in `deploy/kubernetes/overlays/prod/hosts.yaml`. It is
   the one file that names the domain, and kustomize copies each value into the
   route, the certificate and the external URL that use it:

   ```yaml
   data:
     api: api.tally.demo.b42labs.com
     otlp: otlp.tally.demo.b42labs.com
     otlp-grpc: otlp-grpc.tally.demo.b42labs.com
     grafana: grafana.tally.demo.b42labs.com
     vmalert: vmalert.tally.demo.b42labs.com
     alertmanager: alertmanager.tally.demo.b42labs.com
   ```

   Each value is a full hostname. Keep each line one unquoted key at two spaces
   with no comment after it, because `make prod-up` reads the lines as text.

2. The rest of this guide uses the demo domain `tally.demo.b42labs.com`. Put
   yours in its place.

## Install the add-ons

1. Install Envoy Gateway and cert-manager:

   ```sh
   make prod-addons PROD_CONTEXT=<ctx>
   ```

   It installs Envoy Gateway `v1.8.3` and then cert-manager `v1.21.1` from
   their Helm charts, with the Gateway API support of cert-manager switched on,
   and waits for their deployments. The last line is the last of those waits:

   ```text
   deployment "cert-manager-cainjector" successfully rolled out
   ```

   A second run upgrades the two releases in place, so the target is safe to
   run again.

2. Check that the resource types the overlay uses exist:

   ```sh
   kubectl --context <ctx> get crd gateways.gateway.networking.k8s.io \
     certificates.cert-manager.io -o name
   ```

   ```text
   customresourcedefinition.apiextensions.k8s.io/gateways.gateway.networking.k8s.io
   customresourcedefinition.apiextensions.k8s.io/certificates.cert-manager.io
   ```

## Write the secrets

1. Copy the five examples and make the copies readable by you alone:

   ```sh
   for f in deploy/kubernetes/overlays/prod/secrets/*.env.example; do cp "$f" "${f%.example}"; done
   chmod 600 deploy/kubernetes/overlays/prod/secrets/*.env
   ```

   Git ignores the `.env` files, so they are the only copy of these values.
   Keep them: TimescaleDB takes both database passwords on the first start of
   its empty volume and keeps them, so a regenerated `tally-db.env` no longer
   matches the database.

2. Fill in each key after its `=`, unquoted:

   | File | Key | Value |
   | --- | --- | --- |
   | `tally-db.env` | `password` | `openssl rand -hex 32` |
   | `tally-db.env` | `engine-password` | `openssl rand -hex 32`, a value of its own |
   | `tally-db.env` | `db-url` | the example URL with the value of `password` in place of `<password>` |
   | `tally-internal-token.env` | `token` | `openssl rand -hex 32` |
   | `tally-grafana.env` | `admin-password` | `openssl rand -hex 32` |
   | `tally-vm-admin.env` | `delete-auth-key` | `openssl rand -hex 32` |
   | `tally-otlp-auth.env` | `htpasswd` | the line of step 3 |

   A hex value needs no encoding in `db-url`. `engine-password` is required
   although the cluster runs no engine, because the initdb script that creates
   the engine's reader role fails on an empty one. Grafana's `admin` user signs
   in with `admin-password`.

3. Hash the OTLP password. `htpasswd` prompts for it, so it stays out of the
   shell's history:

   ```sh
   htpasswd -nBC 12 tally
   ```

   ```text
   New password:
   Re-type new password:
   tally:$2y$12$<bcrypt hash>
   ```

   Paste the last line after `htpasswd=`. Keep the password itself: the file
   holds only its hash, and the publisher of every cloud and the check at the
   end of this guide send the password. Why the line has this form is in
   [set the credential](/how-to/observability/publish-metrics-over-otlp#set-the-credential).

## Deploy the overlay

1. Deploy the overlay:

   ```sh
   make prod-up PROD_CONTEXT=<ctx>
   ```

   It checks that every `.env` file exists with each value filled in and that
   the checkout is at the tag `newTag` names, applies the `letsencrypt`
   ClusterIssuer and the overlay, and waits for every rollout and for the
   LoadBalancer address of the Gateway. Then it applies the reporting
   migration chain through a port-forward to TimescaleDB, waits for the
   Reporting API and prints where the Gateway answers:

   ```text
   ==> the Gateway answers on 203.0.113.10
       the hostnames of hosts.yaml:
         api.tally.demo.b42labs.com  (api)
         otlp.tally.demo.b42labs.com  (otlp)
         otlp-grpc.tally.demo.b42labs.com  (otlp-grpc)
         grafana.tally.demo.b42labs.com  (grafana)
         vmalert.tally.demo.b42labs.com  (vmalert)
         alertmanager.tally.demo.b42labs.com  (alertmanager)

   Point *.tally.demo.b42labs.com at that address, then: kubectl --context <ctx> -n tally wait certificate/tally-wildcard --for=condition=Ready --timeout=10m

   ==> the unpublished services, through a port-forward:
       kubectl --context <ctx> -n tally port-forward svc/victoriametrics 8428:8428
       kubectl --context <ctx> -n tally port-forward svc/vmalert 8880:8880
       kubectl --context <ctx> -n tally port-forward svc/alertmanager 9093:9093
   ```

   The address is a hostname when the LoadBalancer hands out one.

2. When a wait runs out, `make prod-up` stops there and names the command that
   shows why. It is safe to run again.

3. When the migration fails, `make prod-up` stops with the error of
   `tally-reporting-admin migrate`.
   [When an apply fails](/how-to/engine/migrate-both-databases#when-an-apply-fails)
   says how to recover. Reach the database for it through the port-forward of
   [issue an ingest credential](#issue-an-ingest-credential), and stop that
   forward before you run `make prod-up` again: it refuses a port something
   already listens on.

## Point the domain at the cluster

1. Create a wildcard `A` record for `*.tally.demo.b42labs.com` with the address
   `make prod-up` printed. When the LoadBalancer hands out a hostname, create a
   `CNAME` record for it instead.

2. Wait for the certificate:

   ```sh
   kubectl --context <ctx> -n tally wait certificate/tally-wildcard \
     --for=condition=Ready --timeout=10m
   ```

   ```text
   certificate.cert-manager.io/tally-wildcard condition met
   ```

   cert-manager proves each of the four published hostnames to Let's Encrypt
   over HTTP-01 on the Gateway's http listener, so the certificate is issued
   only once the record resolves. Until the certificate is Ready the https
   listener answers nothing. When the wait runs out,
   `kubectl --context <ctx> -n tally describe certificate tally-wildcard` shows
   the conditions and events that say why.

3. Check the certificate the Gateway serves:

   ```sh
   openssl s_client -connect api.tally.demo.b42labs.com:443 \
     -servername api.tally.demo.b42labs.com </dev/null 2>/dev/null \
     | openssl x509 -noout -issuer -ext subjectAltName
   ```

   ```text
   issuer=C=US, O=Let's Encrypt, CN=R12
   X509v3 Subject Alternative Name:
       DNS:api.tally.demo.b42labs.com, DNS:grafana.tally.demo.b42labs.com, DNS:otlp-grpc.tally.demo.b42labs.com, DNS:otlp.tally.demo.b42labs.com
   ```

   The `CN` of the issuer names whichever Let's Encrypt intermediate signed the
   certificate. The leaf lists the four published hostnames and no other.

## Issue an ingest credential

1. Forward TimescaleDB to your machine in a second terminal and leave it
   running. The overlay has no postgres listener, so nothing outside the
   cluster reaches the database otherwise:

   ```sh
   kubectl --context <ctx> -n tally port-forward svc/timescaledb 15432:5432
   ```

   ```text
   Forwarding from 127.0.0.1:15432 -> 5432
   Forwarding from [::1]:15432 -> 5432
   ```

2. Issue the credential from the checkout, for the cloud the collector reports
   under. The connection string reads the password out of `tally-db.env`, so
   the password stays out of the shell's history:

   ```sh
   TALLY_REPORTING_DB_URL="postgres://tally:$(sed -n 's/^password=//p' deploy/kubernetes/overlays/prod/secrets/tally-db.env)@127.0.0.1:15432/tally_reporting?sslmode=disable" \
     go run ./cmd/tally-reporting-admin create-ingest-credential \
     --platform openstack \
     --cloud <cloud> \
     --description 'event collector for <cloud>'
   ```

   ```text
   tly_i_6eb7f2ea8ffbb37f44d41bdc3382d193c3de752f89d5bafe7b85afc93a65c32b
   created ingest_credentials 8f0c2f34-1a4d-4c2e-9a53-6b1c0f5e77a1
   the token above is printed this one time: store it now, it will not be shown again
   ```

3. Store the token where the collector reads it, as
   [issue and revoke credentials](/how-to/openstack/issue-and-revoke-credentials#issue-an-ingest-credential)
   shows. The token goes to stdout alone, so the redirection that page uses
   works with the command above. The same page revokes a credential.

4. Set the collector's `TALLY_OSC_REPORTING_URL` to `https://api.<domain>`,
   which is `https://api.tally.demo.b42labs.com` for the demo domain, as
   [connect the collector](/how-to/openstack/connect-the-collector#configure-the-collector)
   shows. Stop the port-forward with Ctrl-C once the credential is issued.

## Reach the unpublished services

1. Forward each service in a terminal of its own. `make prod-up` prints the
   same three commands:

   ```sh
   kubectl --context <ctx> -n tally port-forward svc/victoriametrics 8428:8428
   kubectl --context <ctx> -n tally port-forward svc/vmalert 8880:8880
   kubectl --context <ctx> -n tally port-forward svc/alertmanager 9093:9093
   ```

2. Open the local URL of the service:

   | Service | Local URL |
   | --- | --- |
   | VictoriaMetrics | `http://127.0.0.1:8428/` |
   | vmalert | `http://127.0.0.1:8880/` |
   | Alertmanager | `http://127.0.0.1:9093/` |

   The three answer every request without a credential, which is why no route
   publishes them.

## Check the result

1. The Reporting API answers a request without a token with a 401 problem
   document:

   ```sh
   curl -sS -o /dev/null -w '%{http_code} %{content_type}\n' \
     https://api.tally.demo.b42labs.com/api/v1/events
   ```

   ```text
   401 application/problem+json
   ```

2. Plain HTTP is redirected to HTTPS, with no port in the URL:

   ```sh
   curl -sS -o /dev/null -w '%{http_code} %{redirect_url}\n' \
     http://api.tally.demo.b42labs.com/
   ```

   ```text
   301 https://api.tally.demo.b42labs.com/
   ```

3. The OTLP endpoint refuses a push without the credential:

   ```sh
   curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
     https://otlp.tally.demo.b42labs.com/v1/metrics
   ```

   ```text
   401
   ```

4. With the credential it answers a status other than `401`. curl prompts for
   the password you gave `htpasswd`:

   ```sh
   curl -sS -o /dev/null -w '%{http_code}\n' -u tally -X POST \
     https://otlp.tally.demo.b42labs.com/v1/metrics
   ```

   The request carries no metrics, so only the absence of `401` counts: it
   shows that the credential was accepted.

5. Grafana serves its login page:

   ```sh
   curl -sS -o /dev/null -w '%{http_code}\n' \
     https://grafana.tally.demo.b42labs.com/login
   ```

   ```text
   200
   ```

6. The store, vmalert, Alertmanager and the database answer nothing from
   outside the cluster:

   ```sh
   for host in vm vmalert alertmanager; do
     curl -s -o /dev/null -w '%{http_code}\n' "https://$host.tally.demo.b42labs.com/"
   done
   nc -z -w 5 203.0.113.10 5432; echo $?
   ```

   ```text
   000
   000
   000
   1
   ```

   `000` is what curl prints when no HTTP answer arrived, and `1` is the exit
   status of `nc` when the port does not accept a connection.

7. A second deploy changes nothing. The filter keeps the lines of
   `kubectl apply` that report a change and the migration's answer:

   ```sh
   make prod-up PROD_CONTEXT=<ctx> | grep -E ' (created|configured)$|^nothing to apply$'
   ```

   ```text
   nothing to apply
   ```

   Every object is reported `unchanged`, so no pod rolls:
   `kubectl --context <ctx> -n tally get pods` lists the same pods before and
   after the run.

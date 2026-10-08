---
title: Deploy the stack to a cluster
description: "Put the Reporting API, the OTLP endpoint, Grafana, the OpenStack collector, the scheduler and the sync on a dedicated cluster that migrates, rates and reconciles by itself, behind Let's Encrypt certificates, with the secrets kept out of the repository."
quadrant: how-to
audience: operator
---

# Deploy the stack to a cluster

This guide deploys Tally to a dedicated Kubernetes cluster: the Reporting API
with TimescaleDB, VictoriaMetrics, the OTel Collector, Grafana, vmalert and
Alertmanager, the OpenStack collector, the metering scheduler and the sync of
the cloud. The collector consumes the notifications of one cloud from a broker
outside the cluster and posts them to the Reporting API. The cluster migrates,
rates and reconciles by itself: Job `tally-migrate` applies both migration
chains on every deploy, the hourly CronJob `tally-engine` rates a month once a
pricing catalog prices it, and CronJob `tally-sync` reconciles the cloud every
10 minutes. The Gateway publishes `api.`, `otlp.`, `otlp-grpc.` and `grafana.`
over HTTPS, behind a certificate Let's Encrypt signs, and the store, vmalert
and Alertmanager are reached through port-forwards. The deploy runs from a
checkout on your machine with two make targets, `make prod-addons` and
`make prod-up`, and no CI job touches the cluster;
[deploy from a pipeline](#deploy-from-a-pipeline) says what a pipeline runs in
their place. What the prod overlay changes against the dev overlay is in
[where the dev stack ends](/contributing/dev-stack#where-the-dev-stack-ends).
The guide installs Envoy Gateway. On a cluster that runs another Gateway API
implementation,
[Use another Gateway API implementation](/how-to/cluster/use-another-gateway-api-implementation)
replaces the add-on and the deploy steps.

## Before you start

- A kubectl context for the cluster, with a LoadBalancer implementation and a
  default StorageClass. The commands below write it as `<ctx>`, and every prod
  target refuses to run while `PROD_CONTEXT` is empty. The LoadBalancer has to
  pass the client's address on to the Gateway rather than proxy the
  connection: the OTLP endpoint limits requests per client address, and behind
  a proxying LoadBalancer every publisher shares one limit.
- `helm`, which `make prod-addons` installs the add-ons with.
- Go at the version `go.mod` states, because the ingest credential and the
  pricing catalog are written with `go run` from the checkout.
- `nc`, for the check of the database port at the end.
- `openssl` from OpenSSL 3 and `htpasswd`, for the secrets and the certificate
  check. The LibreSSL that macOS ships has no `x509 -ext`.
- Docker and `curl`, for the checks.
- A DNS zone for the domain, in which you can create a wildcard record.
- The broker of the cloud, reachable over AMQP from the pods of the cluster,
  with the account of
  [create the broker account](/how-to/openstack/connect-the-collector#create-the-broker-account).
- The OpenStack services of that cloud, configured as
  [configure the OpenStack services](/how-to/openstack/connect-the-collector#configure-the-openstack-services)
  says.
- An account on that cloud that lists the resources of every project, as
  [reconcile a cloud](/how-to/openstack/reconcile-a-cloud#check-the-account)
  checks it. The Reporting API reconciles the cloud with it.
- A pricing model file for the cloud, as
  [import a pricing model](/how-to/engine/import-a-pricing-model) describes it.
- A release tag whose images are in the registry, or a commit whose `ci` run is
  green to cut one from.
- A checkout of the repository at that tag. The checkout supplies the
  manifests the images run under, and the CLIs of the checkout write the
  credential and the catalog into the schema of the image, so `make prod-up`
  refuses to run from any other commit.

## Cut the release that publishes the images

1. Tag the commit and push the tag. The `release` workflow runs on every tag
   matching `v*` and pushes four images to `ghcr.io/b42labs`, each at
   `<tag>`: `tally-reporting`, `tally-engine`, `tally-openstack-collector` and
   `tally-reporting-admin`:

   ```sh
   git tag <tag>
   git push origin <tag>
   ```

   Wait until the run of the tag has finished on the repository's Actions tab.
   [Releases](/contributing/toolchain#releases) describes what the run builds.

2. Check that the four images the overlay deploys pull without a login, which
   is how the cluster's nodes pull them:

   ```sh
   docker logout ghcr.io
   docker pull ghcr.io/b42labs/tally-reporting:<tag>
   docker pull ghcr.io/b42labs/tally-openstack-collector:<tag>
   docker pull ghcr.io/b42labs/tally-engine:<tag>
   docker pull ghcr.io/b42labs/tally-reporting-admin:<tag>
   ```

3. When a pull is denied, the package is still private: the first push of an
   image creates its package that way. An organisation admin opens the B42Labs
   organisation on GitHub, then Packages, the `tally-reporting` package and
   Package settings, and makes the package public under "Change visibility".
   The same goes for `tally-openstack-collector`, `tally-engine` and
   `tally-reporting-admin`. A public package cannot be made private again. Run
   the pulls of step 2 again afterwards.

4. Check out the tag, and set the four `newTag` values in
   `deploy/kubernetes/overlays/prod/kustomization.yaml` to it when they name
   another one:

   ```sh
   git checkout <tag>
   ```

   ```yaml
   images:
     - name: tally-reporting
       newName: ghcr.io/b42labs/tally-reporting
       newTag: <tag>
     - name: tally-openstack-collector
       newName: ghcr.io/b42labs/tally-openstack-collector
       newTag: <tag>
     - name: tally-engine
       newName: ghcr.io/b42labs/tally-engine
       newTag: <tag>
     - name: tally-reporting-admin
       newName: ghcr.io/b42labs/tally-reporting-admin
       newTag: <tag>
   ```

   The overlay deploys one release, so `make prod-up` refuses two different
   tags.

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

## Name the cloud

1. Set `TALLY_OSC_CLOUD` in `deploy/kubernetes/overlays/prod/collector.env` to
   the cloud the collector reports under:

   ```text
   TALLY_OSC_CLOUD=<cloud>
   ```

   Every event the collector emits is attributed to that cloud, the ingest
   credential of a later section is issued for it, and CronJob `tally-sync`
   syncs it. The file ships with an empty value, which `make prod-up` refuses.

2. Add the other settings the deployment needs to the same file, one
   `KEY=VALUE` per line. `TALLY_OSC_EXCHANGES` lists the exchanges of a cloud
   that runs octavia or renamed one, and `TALLY_OSC_QUEUE_TYPE=classic` is the
   setting for a broker older than RabbitMQ 4.0:

   ```text
   TALLY_OSC_EXCHANGES=nova,neutron,openstack,glance,octavia
   ```

   The [collector settings](/reference/configuration/tally-openstack-collector)
   page lists every variable. The broker URL and the ingest token are not
   among the lines of this file: both are Secrets, and the header of the file
   names the six variables the deployment fixes.

## Configure reconciliation

1. Name the cloud the Reporting API reconciles in
   `deploy/kubernetes/overlays/prod/reconciliation/clouds-config.yaml`:

   ```yaml
   clouds:
     - cloud: <cloud>
       platform: openstack
       adapter: openstack
       adapter_config:
         os_cloud: <os-cloud>
         include_octavia: false
   ```

   `cloud` is the value of `TALLY_OSC_CLOUD` in `collector.env`. CronJob
   `tally-sync` takes its cloud from there, and a sync of a cloud this file
   does not name is answered 404. `os_cloud` is the name of the entry in
   `secrets/clouds.yaml` that the Reporting API authenticates with, which
   [write the secrets](#write-the-secrets) fills. Set `include_octavia: true`
   when `TALLY_OSC_EXCHANGES` lists `octavia`, so the sync lists the load
   balancers the events book.

2. Keep the `cloud` and the `os_cloud` line at their indentation, unquoted and
   with no comment after the value, because `make prod-up` reads the two lines
   as text. The file ships with both names empty, which `make prod-up`
   refuses. The [clouds file](/reference/configuration/clouds-file) reference
   lists every key.

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

1. Copy the six examples and make the copies readable by you alone:

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
   | `tally-db.env` | `engine-db-url` | the example URL with the value of `password` in place of `<password>` |
   | `tally-db.env` | `engine-reporting-db-url` | the example URL with the value of `engine-password` in place of `<engine-password>` |
   | `tally-internal-token.env` | `token` | `openssl rand -hex 32` |
   | `tally-grafana.env` | `admin-password` | `openssl rand -hex 32` |
   | `tally-vm-admin.env` | `delete-auth-key` | `openssl rand -hex 32` |
   | `tally-otlp-auth.env` | `htpasswd` | the line of step 3 |
   | `tally-collector-amqp.env` | `amqp-url` | the example URL with the broker account, its password and the broker's host in place of the placeholders |

   A hex value needs no encoding in the three URLs. Grafana's `admin` user
   signs in with `admin-password`.

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

4. Copy the example of the credential the Reporting API reconciles the cloud
   with, make the copy readable by you alone, and fill in every `<...>` value:

   ```sh
   cp deploy/kubernetes/overlays/prod/secrets/clouds.yaml.example deploy/kubernetes/overlays/prod/secrets/clouds.yaml
   chmod 600 deploy/kubernetes/overlays/prod/secrets/clouds.yaml
   ```

   The entry name is the `os_cloud` of
   [configure reconciliation](#configure-reconciliation). The example
   authenticates with an application credential, and
   [restrict the account to read requests](/how-to/openstack/reconcile-a-cloud#restrict-the-account-to-read-requests)
   creates one that every API answers for the requests of a sync alone. Git
   ignores the file, and `make prod-up` refuses it while a placeholder is left.

## Deploy the overlay

1. Deploy the overlay:

   ```sh
   make prod-up PROD_CONTEXT=<ctx>
   ```

   It checks that every `.env` file exists with each key of its example and
   each value filled in, that `collector.env` names a cloud, that
   `secrets/clouds.yaml` carries no placeholder, that `clouds-config.yaml`
   names the cloud of `collector.env` and an `os_cloud`, and that the checkout
   is at the tag every `newTag` value names. Then it applies the `letsencrypt`
   ClusterIssuer, deletes the migration Job `tally-migrate` of the previous
   deploy and applies the overlay, which creates the Job again. It waits for
   every rollout but the collector's and for the LoadBalancer address of the
   Gateway, then for the Job, whose log it prints. The reporting chain is
   applied first, in an init container, and the engine chain after it. Then it
   waits for the Reporting API and prints where the Gateway answers. On the
   first deploy the output ends with these lines:

   ```text
   ==> waiting for the migration Job
   [pod/tally-migrate-twtkd/reporting] applied migration 1
   [pod/tally-migrate-twtkd/reporting] applied migration 2
   [pod/tally-migrate-twtkd/reporting] applied migration 3
   [pod/tally-migrate-twtkd/reporting] applied migration 4
   [pod/tally-migrate-twtkd/reporting] applied migration 5
   [pod/tally-migrate-twtkd/reporting] applied migration 6
   [pod/tally-migrate-twtkd/reporting] applied migration 7
   [pod/tally-migrate-twtkd/reporting] applied migration 8
   [pod/tally-migrate-twtkd/reporting] applied migration 9
   [pod/tally-migrate-twtkd/reporting] applied migration 10
   [pod/tally-migrate-twtkd/reporting] applied migration 11
   [pod/tally-migrate-twtkd/reporting] applied migration 12
   [pod/tally-migrate-twtkd/engine] applied migration 1
   [pod/tally-migrate-twtkd/engine] applied migration 2
   deployment "reporting-api" successfully rolled out

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

   ==> the collector starts once the Secret tally-collector-token exists, and is Ready once it holds its broker session:
       kubectl --context <ctx> -n tally rollout status deployment/openstack-collector

   ==> the scheduler rates a month once a pricing catalog prices it; until then its hourly Job fails:
       kubectl --context <ctx> -n tally get cronjob tally-engine tally-sync
   ```

   The address is a hostname when the LoadBalancer hands out one. The
   collector pod stays in `ContainerCreating` until
   [issue an ingest credential](#issue-an-ingest-credential) has created its
   token Secret, and starts by itself afterwards.

2. When a wait runs out, `make prod-up` stops there and names the command that
   shows why. It is safe to run again.

3. When the migration Job fails, `make prod-up` prints its log and stops with
   `the migration Job tally-migrate failed`. The Job tries four times, each
   time in a pod of its own, so the log carries the error of every attempt
   under the name of its pod and container.
   [When an apply fails](/how-to/engine/migrate-both-databases#when-an-apply-fails)
   says how to recover. Reach the database for it through the port-forward of
   [issue an ingest credential](#issue-an-ingest-credential), then run
   `make prod-up` again: it replaces the failed Job.

4. When the Job has not finished after 30 minutes, `make prod-up` stops and
   leaves it running, because a migration that is killed can leave a
   half-built index. `PROD_MIGRATE_WAIT_S` sets the wait in seconds. A later
   `make prod-up` refuses to replace the Job until it has finished, and names
   the command that waits for it.

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

2. Issue the credential from the checkout and pipe the token into the Secret
   the collector mounts. The connection string reads the password out of
   `tally-db.env` and the cloud out of `collector.env`, so the password stays
   out of the shell's history, and the credential is issued for the cloud the
   collector reports under:

   ```sh
   kubectl --context <ctx> -n tally create secret generic tally-collector-token \
     --from-literal=ingest-token=probe --dry-run=server >/dev/null \
     && token="$(TALLY_REPORTING_DB_URL="postgres://tally:$(sed -n 's/^password=//p' deploy/kubernetes/overlays/prod/secrets/tally-db.env)@127.0.0.1:15432/tally_reporting?sslmode=disable" \
     go run ./cmd/tally-reporting-admin create-ingest-credential \
     --platform openstack \
     --cloud "$(sed -n 's/^TALLY_OSC_CLOUD=//p' deploy/kubernetes/overlays/prod/collector.env)" \
     --description 'in-cluster event collector')" \
     && printf '%s' "$token" | kubectl --context <ctx> -n tally create secret generic tally-collector-token \
       --from-file=ingest-token=/dev/stdin
   unset token
   ```

   ```text
   created ingest_credentials 16d90e80-4c90-4356-a9f1-842faab52592
   the token above is printed this one time: store it now, it will not be shown again
   secret/tally-collector-token created
   ```

   The token reaches neither a file nor the argument list of a process. The
   first `kubectl create` is a server-side dry run: when the Secret exists, the
   context is wrong or the cluster refuses the call, it stops the command
   before a credential is issued, so a second run issues nothing and replaces
   nothing. The `&&` after the issue keeps a failed issue from creating a
   Secret with an empty key. If the second `kubectl create` fails all the same,
   the credential of the id printed above is issued and its token is gone:
   revoke that id, as
   [issue and revoke credentials](/how-to/openstack/issue-and-revoke-credentials#revoke-a-credential)
   shows. The API refuses an event outside the scope of its credential with
   the reason `scope` and never takes it again, which is why the cloud is read
   from the file the collector reads it from.

3. Wait for the collector. The pod has been waiting for the Secret, and the
   kubelet starts it without a further step:

   ```sh
   kubectl --context <ctx> -n tally rollout status deployment/openstack-collector
   ```

   ```text
   Waiting for deployment "openstack-collector" rollout to finish: 0 of 1 updated replicas are available...
   deployment "openstack-collector" successfully rolled out
   ```

   The pod is Ready once the collector holds its session on the broker. When
   the wait does not end,
   `kubectl --context <ctx> -n tally logs deployment/openstack-collector` names
   the reason: a line `the AMQP session ended, reconnecting` carries the error
   of the broker.

4. Leave the port-forward running for
   [import a pricing catalog](#import-a-pricing-catalog).

To replace the token, forward the database as in step 1, delete the Secret
with `kubectl --context <ctx> -n tally delete secret tally-collector-token` and
run the command of step 2 again, which issues a new credential and creates the
Secret from it. Then restart the collector with
`kubectl --context <ctx> -n tally rollout restart deployment/openstack-collector`.
The collector reads the file at its start only, so the running pod keeps the
old token until the restart. Revoke the old credential afterwards, as
[issue and revoke credentials](/how-to/openstack/issue-and-revoke-credentials#revoke-a-credential)
shows.

## Import a pricing catalog

The scheduler rates a month once its grace window of 72 hours has passed and a
pricing catalog prices it. Until a catalog does, its hourly Job fails on that
month with `no pricing model is valid for this period`, which is the signal
that the catalog is missing.

1. Import the catalog through the port-forward of
   [issue an ingest credential](#issue-an-ingest-credential). The connection
   string reads the password out of `tally-db.env`:

   ```sh
   TALLY_ENGINE_DB_URL="postgres://tally:$(sed -n 's/^password=//p' deploy/kubernetes/overlays/prod/secrets/tally-db.env)@127.0.0.1:15432/tally_engine?sslmode=disable" \
     go run ./cmd/tally-engine pricing import <file>
   ```

   ```text
   imported pricing model 2026-03 valid from 2026-03-01T00:00:00Z
   ```

   [Import a pricing model](/how-to/engine/import-a-pricing-model) says what
   the file declares and how to read the imported versions back.

2. Stop the port-forward with Ctrl-C.

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

7. The collector holds its session on the broker. Its log carries a `summary`
   line every minute:

   ```sh
   kubectl --context <ctx> -n tally logs deployment/openstack-collector \
     | grep '"msg":"summary"' | tail -1
   ```

   ```text
   {"time":"2026-10-02T19:13:21.226380379Z","level":"INFO","msg":"summary","service":"tally-openstack-collector","interval_seconds":60,"connected":true,"consumed":28,"skipped":119,"unparseable":0,"delivered":28,"delivery_errors":0,"buffered":0,"oldest_buffered_seconds":0}
   ```

   `"connected":true` is what counts. `delivered` rises once the cloud sends
   notifications, and `buffered` stays at 0 while the Reporting API takes them.

8. The store scrapes the collector. Through the VictoriaMetrics port-forward
   of [reach the unpublished services](#reach-the-unpublished-services), read
   the health of the `openstack-collector` target:

   ```sh
   curl -s http://127.0.0.1:8428/api/v1/targets \
     | jq -r '.data.activeTargets[] | select(.labels.job == "openstack-collector") | .health'
   ```

   ```text
   up
   ```

   The job discovers the collector pod through the Service
   `openstack-collector`. The three discovered jobs of the prod scrape config,
   `reporting-api`, `otel-collector` and `openstack-collector`, are the three
   `absent` clauses of
   `deploy/kubernetes/overlays/prod/victoriametrics/scrape-rules.yaml`, so
   `TallyScrapeJobMissing` stays quiet while all three resolve to targets.

9. The migration Job has completed:

   ```sh
   kubectl --context <ctx> -n tally get job tally-migrate
   ```

   ```text
   NAME            STATUS     COMPLETIONS   DURATION   AGE
   tally-migrate   Complete   1/1           6s         6s
   ```

10. A sync of the cloud completes. Create a Job from the CronJob, wait for it,
    read its log and delete it:

    ```sh
    kubectl --context <ctx> -n tally create job --from=cronjob/tally-sync sync-check
    kubectl --context <ctx> -n tally wait --for=condition=complete job/sync-check --timeout=2m
    kubectl --context <ctx> -n tally logs job/sync-check
    kubectl --context <ctx> -n tally delete job sync-check
    ```

    ```text
    job.batch/sync-check created
    job.batch/sync-check condition met
    {"stats":{"created":0,"deleted":0,"updated":2},"sync_run_id":"2c0b7e07-197b-4d53-af19-dce55400e216"}
    job.batch "sync-check" deleted from tally namespace
    ```

    The log is the answer of the Reporting API, and `sync_run_id` names the row
    of `sync_runs` the run left. A Job that fails logs one line starting with
    `wget:`. `404 Not Found` is a cloud `clouds-config.yaml` does not name,
    `500 Internal Server Error` a run that failed, whose reasons
    [check the result](/how-to/openstack/reconcile-a-cloud#check-the-result)
    reads, `409 Conflict` a scheduled run that held the cloud at the same
    moment, and `download timed out` a Reporting API that does not answer.

11. Both CronJobs are scheduled:

    ```sh
    kubectl --context <ctx> -n tally get cronjob tally-engine tally-sync
    ```

    ```text
    NAME           SCHEDULE       TIMEZONE   SUSPEND   ACTIVE   LAST SCHEDULE   AGE
    tally-engine   0 * * * *      <none>     False     0        <none>          8m57s
    tally-sync     */10 * * * *   <none>     False     0        3s              8m15s
    ```

    `LAST SCHEDULE` reads `<none>` until the first schedule after the deploy:
    the next multiple of 10 minutes for `tally-sync` and the next full hour
    for `tally-engine`.

12. A second deploy changes nothing but the migration Job. The filter keeps
    the lines of `kubectl apply` that report a change and the answers of the
    two chains:

    ```sh
    make prod-up PROD_CONTEXT=<ctx> | grep -E ' (created|configured)$|nothing to apply$'
    ```

    ```text
    job.batch/tally-migrate created
    [pod/tally-migrate-b5qkd/reporting] nothing to apply
    [pod/tally-migrate-b5qkd/engine] nothing to apply
    ```

    The Job is created again, and both chains at their head apply nothing.
    Every other object is reported `unchanged`, so no Deployment or
    StatefulSet rolls.

## Deploy from a pipeline

`make prod-up` checks its input and then runs two applies and a wait, and a
pipeline that deploys a release runs those three steps itself:

1. Delete the migration Job of the previous deploy once it has finished. A
   Job's pod template is immutable, so the apply of a new tag fails on the old
   Job with `field is immutable`. A Job that is still running is left alone
   and stops the pipeline: a migration that is killed can leave a half-built
   index. The read prints nothing for no Job, the name alone for one that has
   not finished, and `Complete` or `Failed` beside it for one that has. A read
   that fails stops the pipeline as well, rather than reading as no Job:

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

2. Apply the certificate issuer and the overlay:

   ```sh
   kubectl --context <ctx> apply -f deploy/kubernetes/overlays/prod/issuers.yaml
   kubectl --context <ctx> apply -k deploy/kubernetes/overlays/prod
   ```

3. Gate the pipeline on the migration Job:

   ```sh
   kubectl --context <ctx> -n tally wait --for=condition=complete job/tally-migrate --timeout=30m
   ```

   A failed Job runs the wait out, and
   `kubectl --context <ctx> -n tally logs -l batch.kubernetes.io/job-name=tally-migrate --all-containers --prefix --tail=-1`
   prints why it failed.

The overlay reads the untracked secret files and `secrets/clouds.yaml` from the
checkout the pipeline applies from; what supplies them there is the pipeline's
tooling. `kubectl apply -k` applies an empty value as it applies any other,
and the shipped clouds config ends the Reporting API at startup with
`clouds[0]: cloud must be set`. The checks `make prod-up` runs first are the
commands
[use another Gateway API implementation](/how-to/cluster/use-another-gateway-api-implementation#deploy)
prints, and a pipeline runs them before it applies. The ingest credential and
the pricing catalog stay one-time steps from a checkout.

---
title: Set up your local Tally
description: Create a kind cluster with the Tally dev overlay, trust its certificate authority, mint an API token, and make a first call against the Reporting API.
quadrant: tutorial
audience: all
---
<!-- Shown output captured on 2026-09-07 from commit 789d782 with kind v0.32.0, kubectl v1.36.1, Docker Desktop 4.86.0, Go 1.27.1 on macOS 15.7.4. -->

# Set up your local Tally

In this lesson you clone the repository, create a kind cluster with the Tally
dev overlay on it, trust the certificate authority that cluster issues its
certificates from, mint an API token, and make your first call against the
Reporting API.

At the end you have a running Tally answering on
`https://api.tally.127-0-0-1.nip.io:8443`, an admin token in a variable of your
shell, and the CA certificate in `tally-ca.crt` at the repository root. That is
the state the rest of this track starts from.

This lesson takes about 60 minutes.

## Before you start

- macOS 15.7.4 with Docker Desktop 4.86.0 running, given 6 CPUs and 8 GB of
  memory. `docker version` prints the Docker Desktop version on its `Server:`
  line, and this prints what the engine was given:

  ```sh
  docker info --format '{{.NCPU}} CPUs, {{.MemTotal}} bytes'
  ```

  ```text
  6 CPUs, 8322072576 bytes
  ```

  The lessons are written for macOS with Docker Desktop, the platform the
  `Makefile` and `deploy/kind/kind.yaml` assume.
- `git` 2.51.0 (`git --version`), `kind` v0.32.0 (`kind version`), `kubectl`
  v1.36.1 (`kubectl version --client`) and Go 1.26 or newer (`go version`) on
  the path. The run had `go1.26.1` installed, and the first `go run` inside the
  repository downloads `go1.27.1`, the toolchain `go.mod` names.
- `jq` 1.8.1 (`jq --version`) and `curl` 8.7.1 (`curl --version`), the one
  macOS ships.
- No kind cluster named `tally` on the machine. `kind get clusters` printed
  `No kind clusters found.` on the run. If it prints `tally`, tear that cluster
  down with the `make down` of lesson 5 first.
- 10 GB of free disk for Docker Desktop. Its usage grew by 9.5 GB over this
  lesson on the run, measured with `docker system df` before and after across
  images, volumes and build cache.

## Get the code

1. Clone the repository and change into it:

   ```sh
   git clone https://github.com/B42Labs/tally.git
   cd tally
   ```

   ```text
   Cloning into 'tally'...
   remote: Enumerating objects: 3555, done.        
   remote: Counting objects: 100% (727/727), done.        
   remote: Compressing objects: 100% (407/407), done.        
   Receiving objects: 100% (3555/3555), 3.29 MiB | 119.00 KiB/s
   Receiving objects: 100% (3555/3555), 3.30 MiB | 111.00 KiB/s, done.
   Resolving deltas: 100% (1866/1866)
   Resolving deltas: 100% (1866/1866), done.
   ```

   The object counts and the transfer speed are your own.

2. Read the commit you are on:

   ```sh
   git rev-parse --short HEAD
   ```

   ```text
   789d782
   ```

   A newer commit is fine. Every output this page shows comes from a run at
   `789d782`. Every command from here on runs from this directory, the
   repository root.

## Create the cluster and bring the stack up

1. Bring the stack up:

   ```sh
   make up
   ```

   The target creates the kind cluster `tally` and says so with
   `==> creating kind cluster tally`, installs cert-manager v1.21.1 and Envoy
   Gateway v1.8.3 and waits for their rollouts, applies the dev certificate
   authority, builds the four images (`tally-reporting`, `tally-engine`,
   `tally-openstack-collector` and `tally-openstack-simulator`), loads them into
   the cluster, applies the dev overlay, and applies the two migration chains,
   the reporting one and the engine one. Several hundred lines go by. These are
   the last ones:

   ```text
   Stack is up:
     https://api.tally.127-0-0-1.nip.io:8443/api/v1        Reporting API
     https://vm.tally.127-0-0-1.nip.io:8443                VictoriaMetrics
     https://grafana.tally.127-0-0-1.nip.io:8443           Grafana
     https://vmalert.tally.127-0-0-1.nip.io:8443/vmalert/  vmalert
     https://alertmanager.tally.127-0-0-1.nip.io:8443      Alertmanager
     https://otlp.tally.127-0-0-1.nip.io:8443              OTLP/HTTP
     db.tally.127-0-0-1.nip.io:5432                        TimescaleDB

   Trust the dev CA with: make -s ca > tally-ca.crt
   ```

   The seven URLs and the last line have to match exactly. Everything above
   them differs in timing and in the ids Kubernetes hands out.

2. Read the error if `make up` stopped on a rollout instead of printing that
   block. The run behind this page needed four calls. The first ended after
   eight minutes under
   `kubectl --context kind-tally -n envoy-gateway-system rollout status
   deployment/envoy-gateway --timeout=300s`:

   ```text
   Waiting for deployment "envoy-gateway" rollout to finish: 0 of 1 updated replicas are available...
   error: timed out waiting for the condition
   make: *** [up] Error 1
   ```

   The second ended on the same error under the `statefulset/timescaledb`
   rollout, the third on `error: deployment "reporting-api" exceeded its
   progress deadline`. Each time the node was still pulling an image: the
   cluster pulls its images on the first `make up`, and a slow network outlasts
   the five-minute rollout timeout.

3. Wait until every pod but `reporting-api` reads `Running` before the next
   call:

   ```sh
   kubectl --context kind-tally -n tally get pods
   ```

   ```text
   NAME                             READY   STATUS    RESTARTS   AGE
   alertmanager-0                   1/1     Running   0          37m
   grafana-6c58589c8b-fg4tm         2/2     Running   0          37m
   otel-collector-599c64db4-vbh2z   1/1     Running   0          37m
   reporting-api-6b8ffd9598-smsw8   0/1     Running   0          37m
   timescaledb-0                    1/1     Running   0          37m
   victoriametrics-0                1/1     Running   0          37m
   vmalert-7699459f4-gf2nc          1/1     Running   0          37m
   ```

   The names carry ids of their own and the ages are your machine's.
   `reporting-api` reads `0/1` here, and waiting does not change it: a
   `make up` that stopped on a rollout never reached its migration step, and
   the API holds itself unready while its database carries no schema. Its log
   says so once per readiness probe:

   ```sh
   kubectl --context kind-tally -n tally logs deployment/reporting-api | grep readiness | tail -1
   ```

   ```text
   {"time":"2026-09-07T18:57:55.783841918Z","level":"WARN","msg":"readiness probe could not use the database","service":"tally-reporting","request_id":"4bc504a2-732d-4902-8c80-d04cd5e5ed2e","error":"reading the schema version: ERROR: relation \"goose_db_version\" does not exist (SQLSTATE 42P01)"}
   ```

   `goose_db_version` is the table a migration chain records itself in, and
   `make up` applies the two chains after the rollouts it waited on. So run
   `make up` again once the other pods read `Running`: against a cluster that
   already exists it reuses it, applies the overlay again, applies both chains
   and prints the block above. The fourth call did that in 16 seconds.

   `==> kind cluster tally already exists` as the first line of a first
   `make up` means a cluster from an earlier run of this track is still on the
   machine. Tear it down with the `make down` of lesson 5 and start over. On a
   repeated call after a timeout that line is the expected one.

4. Read the pods once `make up` has printed its block:

   ```sh
   kubectl --context kind-tally -n tally get pods
   ```

   ```text
   NAME                             READY   STATUS      RESTARTS   AGE
   alertmanager-0                   1/1     Running     0          41m
   grafana-6c58589c8b-fg4tm         2/2     Running     0          41m
   otel-collector-599c64db4-vbh2z   1/1     Running     0          41m
   reporting-api-6b8ffd9598-smsw8   1/1     Running     0          41m
   tally-engine-29813460-mhpnt      0/1     Completed   0          4m18s
   timescaledb-0                    1/1     Running     0          41m
   victoriametrics-0                1/1     Running     0          41m
   vmalert-7699459f4-gf2nc          1/1     Running     0          41m
   ```

   `reporting-api` reads `1/1` now: the chain that call applied gave the
   readiness probe the schema it asks for. Every other long-running pod reads
   `Running`. The `tally-engine-<id>` pod is a Job of the hourly scheduler and
   appears once the clock has passed an hour mark. Its first tick moves the
   month that has ended into its grace window and reads `Completed`; a later
   tick meters that month, finds no pricing model and reads `Error`, which is
   expected until lesson 3 imports one.

## Trust the dev CA

1. Write the cluster's CA certificate to a file:

   ```sh
   make -s ca > tally-ca.crt
   ```

   The target prints nothing. `tally-ca.crt` now holds the certificate of the
   certificate authority cert-manager created for this cluster, 562 bytes on
   the run. It is not installed into the operating system or into a browser:
   cert-manager creates a new CA on every `make up`, so an installed one is
   stale after the next tear-down. `curl` is handed the file per call with
   `--cacert` instead.

2. Call the Reporting API with the file and no credential:

   ```sh
   curl --cacert tally-ca.crt https://api.tally.127-0-0-1.nip.io:8443/api/v1/resources
   ```

   ```json
   {"type":"urn:tally:error:unauthorized","title":"Unauthorized","status":401,"detail":"the request carries no bearer token"}
   ```

   The API answered and refused, which is what a verified connection without a
   credential looks like. `type` `urn:tally:error:unauthorized` and `status`
   401 have to match. The answer is an RFC 9457 problem document, the one shape
   every error of this API has, described under
   [errors](/reference/api/reporting-api#errors).

3. Make the same call without the file:

   ```sh
   curl https://api.tally.127-0-0-1.nip.io:8443/api/v1/resources
   ```

   ```text
   curl: (60) SSL certificate problem: unable to get local issuer certificate
   More details here: https://curl.se/docs/sslcerts.html

   curl failed to verify the legitimacy of the server and therefore could not
   establish a secure connection to it. To learn more about this situation and
   how to fix it, please visit the web page mentioned above.
   ```

   This is what the file is for. The `(60)` line has to match, and a `curl`
   that is not the one macOS ships may word the lines after it differently.

## Mint an API token

1. Point the admin CLI at the dev reporting database:

   ```sh
   export TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
   ```

   The command prints nothing. The URL reaches the dev database through the
   Gateway's TCP listener, the path a `psql` on your machine takes as well, and
   `tally-reporting-admin` reads it from the environment.

2. Mint the token and export it:

   ```sh
   TALLY_API_TOKEN="$(go run ./cmd/tally-reporting-admin create-api-token --role admin --description 'tutorial')"
   export TALLY_API_TOKEN
   ```

   ```text
   created api_tokens f2f68e0c-7228-4c75-a0da-8e344dabd56c
   the token above is printed this one time: store it now, it will not be shown again
   ```

   The two lines are the CLI's notices on stderr. The token itself went to
   stdout and from there into the variable, and it is printed this one time.
   The id is your own. The first `go run` compiles the binary and, on a machine
   with Go 1.26, downloads the go1.27.1 toolchain before that, which is why it
   takes a while.

   The token carries the role `admin`, the role that every operation of this
   API accepts; the other two roles and how a token is revoked are in
   [issue and revoke credentials](/how-to/openstack/issue-and-revoke-credentials).

   A `dial tcp` error naming `db.tally.127-0-0-1.nip.io:5432` in place of the
   two notices means `make up` did not finish. Run it again.

## Make your first call

1. Ask the API for the resources it knows:

   ```sh
   curl --cacert tally-ca.crt -H "Authorization: Bearer $TALLY_API_TOKEN" 'https://api.tally.127-0-0-1.nip.io:8443/api/v1/resources'
   ```

   ```json
   {"items":[],"next_cursor":null}
   ```

   The fleet is empty because nothing has reported into it yet, which lesson 2
   changes. `items` has to be empty and `next_cursor` null. A 401 here means
   the variable is empty in this shell: the token is printed once, so mint
   another one with the two commands above.

## What you learned

- The cluster runs the Reporting API, its database, the metrics store and the
  dashboards behind one Gateway, the arrangement
  [the system at a glance](/explanation/architecture-and-the-provider-pattern#the-system-at-a-glance)
  draws.
- `tally-reporting-admin`, the command that minted the token, is one of
  [the six binaries](/explanation/architecture-and-the-provider-pattern#the-six-binaries),
  and you ran it with `go run` instead of from an image.
- An API token carries a role, and the role decides what it may do:
  [who may write what](/explanation/architecture-and-the-provider-pattern#who-may-write-what).
- `GET /api/v1/resources` reads the projection, which stays empty until
  something reports:
  [the projection](/explanation/events-as-the-source-of-truth#the-projection).

## Where to go next

[Simulate a month of OpenStack](/tutorials/simulate-a-month-of-openstack) fills
the empty fleet you just read.

It starts from the state this lesson leaves behind: the kind cluster `tally`
with the dev overlay running, `tally-ca.crt` at the repository root, and
`TALLY_REPORTING_DB_URL` and `TALLY_API_TOKEN` in the shell. Keep that shell
open. The token is printed once, and lesson 2 says how to mint another one if
the shell was closed.

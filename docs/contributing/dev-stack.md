---
title: The dev stack
description: "What make up builds on your machine: the kind cluster, the dev overlay, the Tilt loop, the simulator stack, and every make target."
quadrant: contributing
audience: contributor
---

# The dev stack

`make up` leaves one kind cluster named `tally` on your machine. It runs the
kustomize base a production overlay would run, and every service in it answers
over HTTPS under `*.tally.127-0-0-1.nip.io:8443`. Lesson 1 of the tutorials,
[Set up your local Tally](/tutorials/set-up-your-local-tally), runs that target
once with the output it prints. This page says what the pieces are and which
target owns each of them.

## What the machine needs

The targets on this page are driven by tools on the host: `git`, `docker` with
its `compose` plugin, `kind`, `kubectl`, Go, `jq` and `curl`. `make check-tools`
probes each of them and prints one line per tool, `ok` with what the tool
answered, `missing` when it is not on the path, or `broken` with the error it
answered instead. It exits non-zero when any of them failed, which is the cheap
way to find a tool that is not there: `make up` reaches the same tool minutes
in and stops with a half-created cluster behind it.

No version is asserted. The pins that decide anything live where the thing they
pin does, in `deploy/kind/kind.yaml`, in the manifests and in `go.mod`, and a
list of versions in the `Makefile` would age beside them and start calling a
working machine wrong. The one comparison the target makes is the Go on the
path against the `go` line of `go.mod`, which it reads out of the file. Beside
the tools it prints what the Docker engine was given and warns, rather than
fails, when that is under `DOCKER_MIN_CPUS` CPUs or `DOCKER_MIN_MEMORY_GB` GB:
the stack comes up on less, more slowly, and that number is the first thing to
look at when a readiness wait runs out.

`docs` and `docs-build` need a Node toolchain besides, which `check-tools`
leaves alone because `npm ci` says plainly enough what is missing.
[The toolchain](/contributing/toolchain#dependencies) names the version.

## The kind cluster

[`deploy/kind/kind.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kind/kind.yaml)
is the cluster's own configuration: one control-plane node on
`kindest/node:v1.35.5`, pinned by digest so that the tag cannot re-resolve to
another image. The node image is held one Kubernetes minor behind kind's
default on purpose, because Envoy Gateway v1.8 supports Kubernetes v1.32 to
v1.35.

All traffic enters through the Gateway, so the node publishes three ports and
nothing uses a per-service NodePort or a port-forward.

| Host port | Node port | What it carries |
| --- | --- | --- |
| 8081 | 30080 | http, which the Gateway redirects to https |
| 8443 | 30443 | https, every service's real URL |
| 5432 | 30432 | TimescaleDB, through the Gateway's TCP listener |

http sits on 8081 rather than on 8080 because 8080 is the Reporting API's own
port, and the cluster holds its host ports for as long as it exists, which
would stop a local `go run ./cmd/tally-reporting` from ever binding.

Every host binding names `127.0.0.1` and lies above 1024. Docker Desktop
publishes a privileged port only through the `com.docker.vmnetd` helper, which
is not installed on every Mac, and rootless Docker and Podman cannot publish
one at all, so binding 80 and 443 makes `make up` fail outright on the machines
this project is developed on. Only the host side moves: the node ports, the
Envoy Service ports and the Gateway listeners keep the standard numbers, which
is what leaves a prod overlay untouched by the rule. That is why every dev URL
carries a port suffix, as in `https://api.tally.127-0-0-1.nip.io:8443`.

Creating the cluster leaves a kubectl context named `kind-tally` behind, and
every kubectl call of the Makefile names it through
`KUBECTL := kubectl --context $(KUBE_CONTEXT)`. Creating a kind cluster
switches the current context, but reusing an existing one does not, so an
unqualified kubectl would deploy Tally into whatever cluster happened to be
selected.

## What make up installs

The target reuses a cluster that already exists, so a second run carries on
rather than starting over. In order, it installs:

1. cert-manager at `CERT_MANAGER_VERSION` (`v1.21.1`) and Envoy Gateway at
   `ENVOY_GATEWAY_VERSION` (`v1.8.3`), applied server-side from their release
   manifests, each followed by a rollout wait.
2. A check for the experimental Gateway API channel. The target asks for the
   `TCPRoute` CRD, `tcproutes.gateway.networking.k8s.io`, which is what routes
   Postgres; a release that does not bundle the channel stops the run here.
3. The dev certificate authority of
   [`overlays/dev/issuers.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/overlays/dev/issuers.yaml),
   applied with plain kubectl rather than through the overlay. The CA
   Certificate has to live in cert-manager's own namespace, and the overlay's
   namespace transformer would move it into `tally`, where cert-manager would
   never find the key it signs with.
4. `make images`, which builds the four images `IMAGES` lists,
   `tally-reporting`, `tally-engine`, `tally-openstack-collector` and
   `tally-openstack-simulator`, each tagged `:dev`. `kind load` then puts the
   two `SERVICES`, `tally-reporting` and `tally-engine`, onto the node.
5. The images the base runs, which `NODE_IMAGES` reads out of the manifests
   rather than pinning a second time in the Makefile. The loop pulls only what
   the host lacks and skips what the node already carries, which is what keeps
   a second `make up` from moving every image again. Before it existed, the
   node pulled TimescaleDB itself inside a timed readiness wait, and that is
   where `make up` used to end.
6. The dev overlay, applied with `kubectl apply -k`.
7. A readiness wait per component and one on the Gateway's `Programmed`
   condition.
8. `make migrate`, which applies the reporting and the engine chains through
   the Gateway. The Reporting API never migrates on its own and its readiness
   probe answers 503 for as long as its database carries no schema, so the
   chain runs before the last rollout wait.

Each wait runs under `WAIT_TIMEOUT` (`300s`), and a rollout gets
`WAIT_ATTEMPTS` (`6`) of them; the product is the budget. An expired wait is
repeated, because a pull outlasting one wait is the normal case on a slow line
rather than a fault. A rollout that is genuinely stuck still ends the run once
the budget is spent, with a message that says the stack is incomplete.

The target ends by printing the seven addresses of the stack.

| Address | What answers there |
| --- | --- |
| `https://api.tally.127-0-0-1.nip.io:8443/api/v1` | Reporting API |
| `https://vm.tally.127-0-0-1.nip.io:8443` | VictoriaMetrics |
| `https://grafana.tally.127-0-0-1.nip.io:8443` | Grafana |
| `https://vmalert.tally.127-0-0-1.nip.io:8443/vmalert/` | vmalert, which publishes that prefix and not the root |
| `https://alertmanager.tally.127-0-0-1.nip.io:8443` | Alertmanager |
| `https://otlp.tally.127-0-0-1.nip.io:8443` | OTLP/HTTP |
| `db.tally.127-0-0-1.nip.io:5432` | TimescaleDB |

The Gateway serves a certificate signed by the dev CA, which is in no root
store on the host. `make ca` prints that certificate, so a call from the host
takes it as its trust anchor:

```sh
make -s ca > tally-ca.crt
curl --cacert tally-ca.crt https://api.tally.127-0-0-1.nip.io:8443/api/v1
```

## The dev overlay

[`overlays/dev/kustomization.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/kubernetes/overlays/dev/kustomization.yaml)
deploys the base into the namespace `tally`. The base lists nine components,
one directory each: `gateway`, `timescaledb`, `victoriametrics`,
`otel-collector`, `reporting-api`, `tally-engine`, `grafana`, `alertmanager`
and `vmalert`. An overlay changes hostnames, the certificate issuer and
environment-specific infrastructure, never the shape of what is deployed.

The overlay patches a nip.io hostname onto each HTTPRoute of the base, so every
service answers under its own name below `*.tally.127-0-0-1.nip.io`, and points
the wildcard Certificate at the dev CA. `envoyproxy.yaml` pins the Envoy
Service to the node ports `kind.yaml` publishes; without it Envoy Gateway
allocates node ports at random and the host port mappings point at nothing.

Six secrets are generated in the clear on purpose, because this database is
reachable only through a Gateway bound to `127.0.0.1` on the developer's own
machine:

- `tally-db`, the two database passwords and the three connection URLs the
  Reporting API and the engine read.
- `tally-internal-token`, the token of the internal routes.
- `tally-otlp-auth`, which the OTLP receivers read as an htpasswd file.
- `tally-grafana`, the Grafana admin password.
- `tally-vm-admin`, what VictoriaMetrics takes as `-deleteAuthKey`.
- `tally-reconciliation-auth`, the `clouds.yaml` the Reporting API
  authenticates the reconciled cloud with. It carries a cloud password, which
  is why it is a Secret rather than part of the ConfigMap below.

Three ConfigMaps are generated beside them. `tally-reconciliation` holds what
the Reporting API reconciles here. `victoriametrics-scrape` is marked
`behavior: replace`, so it overrides the generated ConfigMap of the same name
in the base with this cluster's scrape config. `tally-counter-sources` tells
the engine where to measure the egress counter the simulator pushes. Each
generated name carries a hash of its content, so editing one of these files
re-rolls the pod that mounts it.

Two Makefile variables carry the URLs a `psql` on the host takes. Both reach
TimescaleDB through the Gateway's TCP listener, which is the path the migration
chains take as well.

```text
TALLY_DEV_DB_URL        postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable
TALLY_DEV_ENGINE_DB_URL postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_engine?sslmode=disable
```

## The Tilt loop

`make dev` runs `tilt up`. Tilt is installed by the contributor; the Makefile
installs nothing for it. The
[`Tiltfile`](https://github.com/B42Labs/tally/blob/main/Tiltfile) opens with
`allow_k8s_contexts('kind-tally')`, so the loop touches no cluster but the one
`make up` created, and it needs that cluster: the add-ons and the dev CA are
already in place when Tilt starts.

Two `docker_build` calls build `tally-reporting` and `tally-engine` from the
repository root. Each limits its context with
`only=['go.mod', 'go.sum', 'cmd', 'internal', 'migrations', 'Dockerfile']`,
because only a Go change or a migration can alter a binary, which embeds the
migration chain.

`k8s_yaml(kustomize('deploy/kubernetes/overlays/dev'))` is what the loop
deploys, so Tilt owns exactly what the overlay deploys and nothing beside it.
Each resource carries a label, `tally` for the Reporting API and the engine and
`infrastructure` for the rest, and a link to its real URL, so the Tilt UI opens
the service itself rather than a port-forward.

The collector and the simulator stay outside the loop. They run beside a broker
rather than in the cluster, which is what the simulator stack is for.

## The simulator stack

[`deploy/compose/compose.yaml`](https://github.com/B42Labs/tally/blob/main/deploy/compose/compose.yaml)
runs three containers on the developer's machine: a RabbitMQ broker,
`rabbitmq:4.3.5-management-alpine` pinned by digest the way the kind node image
is; the collector image `tally-openstack-collector:dev`, running unmodified,
since nothing in it knows the notifications it consumes are generated; and the
simulator `tally-openstack-simulator:dev`, which publishes a generated month of
oslo.messaging notifications onto the broker.

Every host port is bound to `127.0.0.1` and lies above 1024, for the reason the
cluster's ports are: 5672 and 15672 for the broker and its management UI, 8090
for the collector, and 8091 for the simulator's control endpoint.

The collector's outbox is a named volume mounted at `/home/nonroot`. The image
runs as uid 65532, and Docker gives a fresh named volume the ownership of the
directory it is mounted over, which for `/home/nonroot` in the distroless image
is that uid. A volume mounted at a path the image does not carry is root-owned,
and the collector cannot open its outbox in it.

`make simulator-up SIM_PERIOD=<month>` starts the stack. It builds
`SIM_IMAGES`, the collector and the simulator alone. The Reporting API is not
among them: `up` deploys it into the cluster and applies the migration chain, so
building it here would leave a tag nothing loads and a schema nobody applied.
After a change to the Reporting API or to the migrations, run `make up` again.

The target then issues an ingest credential for the cloud, and with
`SIM_REGISTER_PROJECTS=true` an admin api token for the project registry as
well. The credential is issued fresh on every run, because the cluster may have
been recreated since the last one and a new database knows none of the tokens
the old one handed out. Both credentials, with the ten `SIM_` values, are
written into `deploy/compose/.env` under `umask 077`: that api token writes the
whole registry, and the default umask of a shell would leave the file readable
by every other user of the machine.

It prints seven addresses.

| Address | What answers there |
| --- | --- |
| `http://127.0.0.1:15672` | broker UI, guest/guest |
| `http://127.0.0.1:8090/metrics` | collector |
| `http://127.0.0.1:8091/clock` | simulator control |
| `http://127.0.0.1:8091/metrics` | simulator inventory, the database exporter stand-in |
| `https://api.tally.127-0-0-1.nip.io:8443/api/v1` | Reporting API |
| `https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics` | OTLP endpoint the series are pushed to |
| `https://vm.tally.127-0-0-1.nip.io:8443/targets` | scrape targets |

Ten variables set what the run generates and how it reaches the cluster.

| Variable | Default | Meaning |
| --- | --- | --- |
| `SIM_CLOUD` | `os-sim` | The cloud the events are ingested under. |
| `SIM_SEED` | `1` | The seed the month is generated from; the same seed gives the same month. |
| `SIM_FACTOR` | `744` | How much wall time is compressed; 744 puts a 31-day month on the bus in an hour. |
| `SIM_PERIOD` | none | The past month to simulate, as `YYYY-MM`. The target refuses to run until one is named, because any month guessed here would be as wrong as another. |
| `SIM_FAULTS` | empty | The fault switches to turn on, comma-separated: `pre-existing`, `missing-create`, `duplicates`, `reordering`, `refused-shapes`, `held-back`. Empty is every switch off. |
| `SIM_REGISTER_PROJECTS` | `false` | Registers the month's tenants and Gardener projects with the dev registry before the first notification goes out. The rows outlive `simulator-down`, which touches the registry not at all. |
| `SIM_GARDEN_CLOUD` | `garden-sim` | The cloud the two Gardener projects are registered under. It differs from `SIM_CLOUD` because a cloud is one installation of one platform. |
| `SIM_OTLP_USER` | `tally` | The user the OTLP endpoint of the dev Gateway asks for. |
| `SIM_OTLP_PASSWORD` | `tally-dev-otlp-password` | Its password, which is the literal the dev overlay generates into `tally-otlp-auth`. |
| `SIM_METRICS_INTERVAL` | `300s` | The grid the pushed traffic and inventory series lie on, which is the interval Ceilometer polls at. |

`make simulator-down` stops the containers and drops the outbox volume, the
broker's queue and `.env`, so the next run starts from nothing rather than
delivering what the last one left behind. The dev reporting database is not
part of that: the events of the last run stay ingested, and no subcommand of
`tally-reporting-admin` deletes them. Running the same period again under
another seed or another cloud adds a second, disjoint set of rows beside the
first, and `make down && make up` is the way to a clean cluster.

[Run a simulated month against the dev cluster](/how-to/simulator/run-a-month)
takes the stack through one month step by step.

## The demo

`make demo` is the whole demonstration in one target. It brings the cluster up,
publishes a month, registers what bills it, meters it, finalizes the run, books
the notifications the simulator held back as a correction, and then serves the
[demo console](/reference/command-line/tally-console) on all of it. What it
leaves behind is the state a walk-through needs: a month whose resources and
events are ingested, a finalized run with its project statements, a correction
with its credit notes and deltas, and a registry carrying a customer, a partner
and the two Gardener projects.

Every step is one the [tutorials](/tutorials/) take by hand, in the order those
lessons take them.

1. `make up` and `make simulator-up`, the latter with the held-back switch on
   and the project registration on, so the month arrives with its tenants and
   its Gardener projects registered.
2. The clock's factor is set to 0, which puts the rest of the month on the bus
   at once. The target then waits for three things: the simulator holding the
   share it keeps back, the broker and the collector's outbox running empty
   behind it, and the last hour of the month standing in VictoriaMetrics, which
   is what says the pushed series are all there.
3. The classic tenants of the month are grouped under `DEMO_CUSTOMER` at a
   discount on every membership, and the month's CI tenant is put under
   `DEMO_PARTNER` at a discount and a commission. That is what gives the
   statements an adjustments table and the run a kickback to settle.
4. The catalog is imported, the month is metered and the run is finalized. The
   engine calls run under a port-forward to VictoriaMetrics: the dev overlay
   measures the egress of an instance with a metricsql query, and the Gateway
   publishes the store over HTTPS alone, which the engine has no CA setting
   for.
5. The held-back notifications are released and delivered, and what arrived
   after the finalized run read the period is booked as a correction, which is
   finalized in its turn.
6. `make console` serves the result until Ctrl-C. The console starts again with
   `make console` alone, which touches none of the above.

The steps between the two `make` calls are four targets of their own,
`demo-drain`, `demo-registry`, `demo-bill` and `demo-correct`, the first of
which runs twice, so a demo that is being repeated can run one of them without
the others.

The target carries on from what stands rather than starting over. The cluster
is reused, a project that is registered is found in place, a relation that is
already active is answered 409 by the registry and left as it is, a catalog
that is imported is not imported again, and a period an earlier demo finalized
keeps the run that closed it: the engine refuses to meter a finalized month,
and a month nothing arrived late for is not corrected either. `make down && make up`
is what starts the month over.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DEMO_PERIOD` | `2026-07` | The month the demo simulates and bills, as `YYYY-MM`. It is the month the tutorials pin, so the console shows the numbers those lessons print, and it stays a past month whatever today is. |
| `DEMO_PRICING` | `pricing/2026-03.yaml` | The catalog the run rates against, imported once and then referred to by the version it carries. |
| `DEMO_CUSTOMER` | `acme` | The meta-project the month's classic tenants are grouped under. |
| `DEMO_PARTNER` | `cloudhouse` | The partner the month's CI tenant is managed by. |
| `DEMO_CUSTOMER_DISCOUNT` | `0.10` | The discount every membership of the customer carries. |
| `DEMO_PARTNER_DISCOUNT` | `0.15` | The discount the partner's relation carries. |
| `DEMO_PARTNER_KICKBACK` | `0.10` | The commission the partner is owed on what it manages. |
| `DEMO_VM_PORT` | `8428` | The port on 127.0.0.1 the engine reaches VictoriaMetrics through while the demo bills the month. |
| `DEMO_WAIT_ATTEMPTS` | `60` | How many reads one wait of the demo may take. |
| `DEMO_WAIT_SECONDS` | `10` | How long it waits between two of them, which puts the budget of one wait at ten minutes. |

## The make targets

The table below is rendered from the `## target: description` comments of the
`Makefile` by `make generate` and pinned by `go test ./docs/`.

<!-- refdoc:begin make-targets -->
| Target | What it does |
| --- | --- |
| `check-tools` | check that the tools the dev stack and the tutorials need answer |
| `up` | create the kind cluster, install the add-ons, and deploy the dev overlay |
| `images` | build one container image per binary |
| `deb` | build the Debian package of the OpenStack collector into dist/ |
| `sbom` | write the SBOM of the packaged collector into dist/ |
| `down` | delete the kind cluster |
| `dev` | rebuild and redeploy on change |
| `simulator-up` | run the simulator, the collector, and a broker against the dev cluster |
| `simulator-down` | stop the simulator stack and drop its volumes |
| `ca` | print the dev CA certificate, for curl --cacert and browser trust |
| `console` | run the demo console against the dev cluster |
| `demo` | prepare the whole demo month and serve the console on it |
| `test` | run the test suite |
| `lint` | run golangci-lint |
| `fmt` | format every Go file with gofumpt, through golangci-lint's formatter |
| `check-alerting` | validate the alert rules and the Alertmanager config |
| `migrate` | apply the reporting and the engine migration chains |
| `generate` | run the code generators and refresh the generated blocks of the reference pages and the handbook |
| `docs` | serve the documentation site locally with live reload |
| `docs-build` | build the documentation site; a dead internal link fails the build |
<!-- refdoc:end make-targets -->

### Variables that change a target

Every variable below is set with `?=`, so one run overrides it on the command
line, as in `make up WAIT_ATTEMPTS=12`.

- `CLUSTER_NAME` (`tally`) names the kind cluster `up` creates, `down` deletes
  and `kind load` puts the images into.
- `NAMESPACE` (`tally`) is the namespace whose rollouts `up` waits on.
- `WAIT_TIMEOUT` (`300s`) is how long one readiness wait may take.
- `WAIT_ATTEMPTS` (`6`) is how many of those waits a rollout gets before `up`
  gives up on it.
- `DOCKER_MIN_CPUS` (`4`) and `DOCKER_MIN_MEMORY_GB` (`8`) are the floors
  `check-tools` warns about the Docker engine under.
- `ENVOY_GATEWAY_VERSION` (`v1.8.3`) is the Envoy Gateway release `up` installs
  the manifests of.
- `CERT_MANAGER_VERSION` (`v1.21.1`) is the cert-manager release beside it.
- `GOLANGCI_LINT_VERSION` (`v2.13.2`) is the linter `lint` and `fmt` run from
  the module cache, and it is the version `.github/workflows/ci.yaml` pins for
  the linter action, so the host and CI judge the code with one linter.
- `OAPI_CODEGEN_VERSION` (`v2.8.0`) is the oapi-codegen release `generate`
  builds the server types with.
- `SQLC_VERSION` (`v1.31.1`) is the sqlc release `generate` builds the query
  code with.
- `NFPM_VERSION` (`v2.47.0`) is the nfpm release `deb` builds the Debian
  package with, from the same module cache as the three above.
- `SYFT_VERSION` (`v1.51.1`) is the syft release `sbom` catalogs the packaged
  binary with, from the same module cache as the four above.
- `DEB_VERSION` (`0.0.0+dev`) is the version `deb` stamps into that package.
  The repository carries no tags, so the default is a development version every
  release sorts above; a build that is going somewhere passes its own.
- `DEB_GOARCH` (`amd64`) is the architecture `deb` cross-compiles and packages
  for.
- The `SIM_` set belongs to the simulator stack and is in the table above,
  and the `DEMO_` set belongs to `demo` and is in the table under
  [the demo](#the-demo).
- `CONSOLE_PORT` (`8095`) is the port `console` binds the demo console to on
  127.0.0.1, and the port of the URL it prints.

## Where the dev stack ends

There is no `prod` overlay. The binding layout in section 2 of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md)
lists `overlays/prod/` as added when Tally is first deployed to a real cluster,
so `overlays/dev/` is the only overlay in the tree.

kind is never used in CI either, as
[Continuous integration](/contributing/toolchain#continuous-integration)
records. The acceptance drills and the tutorials run on a contributor's machine
instead, which is where what this page claims about the stack was checked. The
[Phase 3 drill record](/contributing/drills/phase3) is the longest of them.

[Tear down your local Tally](/tutorials/tear-down-your-local-tally) removes the
cluster, the simulator stack and the files these targets wrote.

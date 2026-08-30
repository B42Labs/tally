# Tally development entry points.
#
# `make up` gives you a complete stack on kind, reachable over real HTTPS URLs
# under *.tally.127-0-0-1.nip.io:8443. `make dev` adds the rebuild-on-change
# loop. The port suffix is there because kind publishes https on 8443 rather
# than 443; deploy/kind/kind.yaml explains why.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -eu -o pipefail -c

# Add-on versions. The Kubernetes version is pinned in deploy/kind/kind.yaml,
# which is the cluster's own configuration.
ENVOY_GATEWAY_VERSION ?= v1.8.3
CERT_MANAGER_VERSION ?= v1.21.1
GOLANGCI_LINT_VERSION ?= v2.12.2

# Code generators. They run from the module cache at these versions; nothing has
# to be installed on the host.
OAPI_CODEGEN_VERSION ?= v2.8.0
SQLC_VERSION ?= v1.31.1

CLUSTER_NAME ?= tally
NAMESPACE ?= tally
DEV_OVERLAY := deploy/kubernetes/overlays/dev
SERVICES := tally-reporting tally-engine

# The two lists are not the same. SERVICES is what `up` deploys into kind, so it
# is also what gets loaded into the cluster: the engine ships as the scheduler
# CronJob, so it belongs there beside the Reporting API. IMAGES is everything
# `images` builds: the collector image is built and publishable, but it runs
# beside the broker of an OpenStack control plane rather than in the dev cluster.
# The simulator is on the producing side of the same kind of broker, and
# `simulator-up` starts one for it on the developer's machine.
IMAGES := $(SERVICES) tally-openstack-collector tally-openstack-simulator

# The images the stack runs, which is what `simulator-up` builds. It is not
# IMAGES: the Reporting API is deployed into the cluster by `up`, which loads it
# there and applies the migration chain, so building it here would leave a tag
# nothing loads and a schema nobody applied. After a change to the Reporting API
# or to the migrations, run `make up` again rather than this.
SIM_IMAGES := tally-openstack-collector tally-openstack-simulator

# The simulator stack, deploy/compose/compose.yaml: a broker, the collector, and
# the simulator, run beside the dev cluster rather than in it. The five SIM_
# values below are what `simulator-up` writes into the .env file compose reads.
# A factor of 744 puts a 31-day month on the bus in an hour.
SIM_CLOUD ?= os-sim
SIM_SEED ?= 1
SIM_FACTOR ?= 744
# No default: the target refuses to run until a month is named, because any
# month guessed here would be as wrong as another.
SIM_PERIOD ?=
# The fault switches to turn on, comma-separated: pre-existing, missing-create,
# duplicates, reordering, refused-shapes, held-back. Empty is every switch off.
SIM_FAULTS ?=
COMPOSE := docker compose -f deploy/compose/compose.yaml

# Every kubectl call names the cluster explicitly. Creating a kind cluster
# switches the current context, but reusing an existing one does not, so an
# unqualified kubectl would deploy Tally into whatever cluster happened to be
# selected.
KUBE_CONTEXT := kind-$(CLUSTER_NAME)
KUBECTL := kubectl --context $(KUBE_CONTEXT)

# Reaches TimescaleDB through the Gateway's TCP listener, which is the same path
# a developer's psql takes.
TALLY_DEV_DB_URL ?= postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable

# The engine's database, on the same listener beside the reporting one.
TALLY_DEV_ENGINE_DB_URL ?= postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_engine?sslmode=disable

# Read from the manifests rather than pinned a second time here, so
# `check-alerting` always validates the configs with the versions the cluster
# runs.
# The character class is what a tag may hold and nothing else. := expands the
# shell once and stores the result, so whatever the manifest carries here is
# what `docker run` below is handed: a class that admitted ';' or '$$' would let
# a manifest line decide what CI runs.
VMALERT_IMAGE := $(shell grep -oE 'victoriametrics/vmalert:[A-Za-z0-9._-]+' deploy/kubernetes/base/vmalert/vmalert.yaml | head -n1)
ALERTMANAGER_IMAGE := $(shell grep -oE 'prom/alertmanager:[A-Za-z0-9._-]+' deploy/kubernetes/base/alertmanager/alertmanager.yaml | head -n1)

.PHONY: up down dev ca test lint fmt check-alerting migrate generate images \
	simulator-up simulator-down

## up: create the kind cluster, install the add-ons, and deploy the dev overlay
up:
	@if ! kind get clusters | grep -qx '$(CLUSTER_NAME)'; then \
		echo '==> creating kind cluster $(CLUSTER_NAME)'; \
		kind create cluster --config deploy/kind/kind.yaml; \
	else \
		echo '==> kind cluster $(CLUSTER_NAME) already exists'; \
	fi
	@echo '==> installing cert-manager $(CERT_MANAGER_VERSION)'
	$(KUBECTL) apply --server-side -f https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml
	$(KUBECTL) -n cert-manager rollout status deployment/cert-manager --timeout=300s
	$(KUBECTL) -n cert-manager rollout status deployment/cert-manager-webhook --timeout=300s
	$(KUBECTL) -n cert-manager rollout status deployment/cert-manager-cainjector --timeout=300s
	@echo '==> installing Envoy Gateway $(ENVOY_GATEWAY_VERSION)'
	$(KUBECTL) apply --server-side -f https://github.com/envoyproxy/gateway/releases/download/$(ENVOY_GATEWAY_VERSION)/install.yaml
	$(KUBECTL) -n envoy-gateway-system rollout status deployment/envoy-gateway --timeout=300s
	@echo '==> checking for the experimental Gateway API channel'
	@$(KUBECTL) get crd tcproutes.gateway.networking.k8s.io >/dev/null 2>&1 || { \
		echo 'ERROR: the TCPRoute CRD is missing, so Postgres cannot be routed.' >&2; \
		echo '       Envoy Gateway $(ENVOY_GATEWAY_VERSION) is expected to bundle the' >&2; \
		echo '       experimental Gateway API channel; a release that does not needs' >&2; \
		echo '       experimental-install.yaml applied before it.' >&2; \
		exit 1; \
	}
	@echo '==> installing the dev certificate authority'
	$(KUBECTL) apply -f $(DEV_OVERLAY)/issuers.yaml
	$(MAKE) images
	@echo '==> loading images into the cluster'
	@for service in $(SERVICES); do \
		kind load docker-image "$$service:dev" --name '$(CLUSTER_NAME)'; \
	done
	@echo '==> applying the dev overlay'
	$(KUBECTL) apply -k $(DEV_OVERLAY)
	$(KUBECTL) -n $(NAMESPACE) rollout status statefulset/timescaledb --timeout=300s
	$(KUBECTL) -n $(NAMESPACE) rollout status statefulset/victoriametrics --timeout=300s
	$(KUBECTL) -n $(NAMESPACE) rollout status deployment/otel-collector --timeout=300s
	$(KUBECTL) -n $(NAMESPACE) rollout status deployment/grafana --timeout=300s
	$(KUBECTL) -n $(NAMESPACE) rollout status statefulset/alertmanager --timeout=300s
	$(KUBECTL) -n $(NAMESPACE) rollout status deployment/vmalert --timeout=300s
	$(KUBECTL) -n $(NAMESPACE) wait gateway/tally --for=condition=Programmed --timeout=300s
	@# The API stays unready until the database carries its schema, and it never
	@# migrates on its own, so the chain has to be applied before the rollout can
	@# finish. It runs through the Gateway, which is what the wait above is for.
	@echo '==> applying the migration chain'
	$(MAKE) migrate
	$(KUBECTL) -n $(NAMESPACE) rollout status deployment/reporting-api --timeout=300s
	@echo
	@echo 'Stack is up:'
	@echo '  https://api.tally.127-0-0-1.nip.io:8443/api/v1        Reporting API'
	@echo '  https://vm.tally.127-0-0-1.nip.io:8443                VictoriaMetrics'
	@echo '  https://grafana.tally.127-0-0-1.nip.io:8443           Grafana'
	@# vmalert is listed with its /vmalert/ prefix because the route publishes
	@# that prefix and the two read endpoints, not the root.
	@echo '  https://vmalert.tally.127-0-0-1.nip.io:8443/vmalert/  vmalert'
	@echo '  https://alertmanager.tally.127-0-0-1.nip.io:8443      Alertmanager'
	@echo '  https://otlp.tally.127-0-0-1.nip.io:8443              OTLP/HTTP'
	@echo '  db.tally.127-0-0-1.nip.io:5432                        TimescaleDB'
	@echo
	@echo 'Trust the dev CA with: make -s ca > tally-ca.crt'

## images: build one container image per binary
images:
	@for service in $(IMAGES); do \
		echo "==> building $$service"; \
		docker build --build-arg "CMD=$$service" -t "$$service:dev" .; \
	done

## down: delete the kind cluster
down:
	@if kind get clusters | grep -qx '$(CLUSTER_NAME)'; then \
		kind delete cluster --name '$(CLUSTER_NAME)'; \
	else \
		echo 'kind cluster $(CLUSTER_NAME) does not exist'; \
	fi

## dev: rebuild and redeploy on change
dev:
	tilt up

# The stack needs a dev cluster from `make up`. Nothing here creates one, and
# the credential step below is where a missing one fails, with the admin CLI's
# connection error.
#
# That credential is issued fresh on every run because the cluster may have been
# recreated since the last one, and a new database knows none of the tokens the
# old one handed out. A stale token is quiet: the Reporting API answers 401, the
# collector keeps the events and retries the batch once per flush, and the only
# sign of it is a line in the collector's log.
## simulator-up: run the simulator, the collector, and a broker against the dev cluster
simulator-up:
	@[ -n '$(SIM_PERIOD)' ] || { echo 'ERROR: set SIM_PERIOD to the past month to simulate, e.g. make simulator-up SIM_PERIOD=2026-07' >&2; exit 1; }
	$(MAKE) images IMAGES='$(SIM_IMAGES)'
	@echo '==> writing the dev CA to tally-ca.crt'
	$(MAKE) -s ca > tally-ca.crt
	@echo '==> issuing an ingest credential for $(SIM_CLOUD)'
	@token="$$(TALLY_REPORTING_DB_URL='$(TALLY_DEV_DB_URL)' go run ./cmd/tally-reporting-admin create-ingest-credential --platform openstack --cloud '$(SIM_CLOUD)' --description 'openstack simulator')"; printf 'TALLY_SIM_CLOUD=%s\nTALLY_SIM_PERIOD=%s\nTALLY_SIM_SEED=%s\nTALLY_SIM_FACTOR=%s\nTALLY_SIM_FAULTS=%s\nTALLY_OSC_TOKEN=%s\n' '$(SIM_CLOUD)' '$(SIM_PERIOD)' '$(SIM_SEED)' '$(SIM_FACTOR)' '$(SIM_FAULTS)' "$$token" > deploy/compose/.env
	$(COMPOSE) up -d
	@echo
	@echo 'Simulator stack is up:'
	@echo '  http://127.0.0.1:15672                          broker UI, guest/guest'
	@echo '  http://127.0.0.1:8090/metrics                   collector'
	@echo '  http://127.0.0.1:8091/clock                     simulator control'
	@echo '  https://api.tally.127-0-0-1.nip.io:8443/api/v1  Reporting API'
	@echo
	@echo "Finish the month at once with:"
	@echo "  curl -X PUT -d '{\"factor\": 0}' http://127.0.0.1:8091/clock"
	@echo "Release the held-back notifications of a run with SIM_FAULTS=held-back with:"
	@echo "  curl -X POST http://127.0.0.1:8091/release"

# Dropping the volumes empties the outbox and the broker's queue, so the next
# `simulator-up` starts from nothing rather than delivering what the last run
# left behind. The dev reporting database is not part of that: the events of the
# last run stay ingested, and no subcommand of tally-reporting-admin deletes
# them. Running the same period again under another seed or another cloud adds a
# second, disjoint set of rows beside the first, and `make down && make up` is
# what drops them.
## simulator-down: stop the simulator stack and drop its volumes
simulator-down:
	$(COMPOSE) down --volumes
	rm -f deploy/compose/.env

## ca: print the dev CA certificate, for curl --cacert and browser trust
ca:
	@$(KUBECTL) -n cert-manager get secret tally-dev-ca -o jsonpath='{.data.tls\.crt}' | base64 -d

## test: run the test suite
test:
	go test ./...

## lint: run golangci-lint
lint:
	golangci-lint run

## fmt: format every Go file with gofumpt, through golangci-lint's formatter
fmt:
	golangci-lint fmt

# Each file is loaded by the binary that will evaluate it, so an expression or a
# routing field the pinned version rejects fails here rather than in the
# cluster. Docker is the only prerequisite; no cluster is involved.
## check-alerting: validate the alert rules and the Alertmanager config
check-alerting:
	@[ -n '$(VMALERT_IMAGE)' ] || { \
		echo 'ERROR: no vmalert image found in deploy/kubernetes/base/vmalert/vmalert.yaml' >&2; \
		exit 1; \
	}
	@[ -n '$(ALERTMANAGER_IMAGE)' ] || { \
		echo 'ERROR: no Alertmanager image found in deploy/kubernetes/base/alertmanager/alertmanager.yaml' >&2; \
		exit 1; \
	}
	docker run --rm -v "$(CURDIR)/deploy/kubernetes/base/vmalert:/etc/vmalert:ro" \
		'$(VMALERT_IMAGE)' -dryRun -rule=/etc/vmalert/rules.yaml
	docker run --rm --entrypoint amtool \
		-v "$(CURDIR)/deploy/kubernetes/base/alertmanager:/etc/alertmanager:ro" \
		'$(ALERTMANAGER_IMAGE)' check-config /etc/alertmanager/config.yaml

# Both chains run through the Gateway's TCP listener. tally_engine is created
# by the initdb script the timescaledb ConfigMap carries, and Postgres runs
# initdb only against an empty data directory, so a dev cluster created before
# that ConfigMap existed needs one `make down && make up` before the second line
# below succeeds.
## migrate: apply the reporting and the engine migration chains
migrate:
	TALLY_REPORTING_DB_URL='$(TALLY_DEV_DB_URL)' go run ./cmd/tally-reporting-admin migrate
	TALLY_ENGINE_DB_URL='$(TALLY_DEV_ENGINE_DB_URL)' go run ./cmd/tally-engine migrate

## generate: run the code generators
generate:
	go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION) \
		-config api/reporting/oapi-codegen.yaml api/reporting/openapi.yaml
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

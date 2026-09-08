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

# Code generators and the linter. They run from the module cache at these
# versions; nothing has to be installed on the host. The linter version is the
# one .github/workflows/ci.yaml pins for golangci-lint-action, so the host and
# CI judge the code with one linter; the two pins are moved together.
OAPI_CODEGEN_VERSION ?= v2.8.0
SQLC_VERSION ?= v1.31.1
GOLANGCI_LINT_VERSION ?= v2.13.2

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
# the simulator, run beside the dev cluster rather than in it. The ten SIM_
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
# Registers the month's tenants and Gardener projects with the dev registry
# before the first notification goes out: true or false. Off by default because
# the rows outlive `simulator-down`, which touches the dev registry not at all.
SIM_REGISTER_PROJECTS ?= false
# The cloud the two Gardener projects are registered under. It differs from
# SIM_CLOUD because a cloud is one installation of one platform.
SIM_GARDEN_CLOUD ?= garden-sim
# The credential the OTLP endpoint of the dev Gateway asks for. The default
# password is the literal the dev overlay generates into the tally-otlp-auth
# secret (deploy/kubernetes/overlays/dev/kustomization.yaml).
SIM_OTLP_USER ?= tally
SIM_OTLP_PASSWORD ?= tally-dev-otlp-password
# The grid the pushed traffic and inventory series lie on, which is the interval
# Ceilometer polls at.
SIM_METRICS_INTERVAL ?= 300s
COMPOSE := docker compose -f deploy/compose/compose.yaml

# Every kubectl call names the cluster explicitly. Creating a kind cluster
# switches the current context, but reusing an existing one does not, so an
# unqualified kubectl would deploy Tally into whatever cluster happened to be
# selected.
KUBE_CONTEXT := kind-$(CLUSTER_NAME)
KUBECTL := kubectl --context $(KUBE_CONTEXT)

# How long one readiness wait may take, and how many of them a rollout gets. The
# product is the budget. A first `make up` on a fresh node pulls every image the
# stack runs, and a pull outlasting one wait is the normal case on a slow line
# rather than a fault: TimescaleDB, the largest at 555 MB, took fifteen minutes
# on the machine the tutorials were captured on, where a single five-minute wait
# ended the run three times over and left a stack whose migration chain had
# never been applied. So an expired wait is repeated, which leaves a pull that is
# still running to finish, while a rollout that is genuinely stuck still ends the
# run once the budget is spent.
WAIT_TIMEOUT ?= 300s
WAIT_ATTEMPTS ?= 6

# await runs one readiness wait under that budget. $(1) is the kubectl arguments
# to wait with, without the timeout, and $(2) is what the messages call the thing
# waited on.
#
# The failure names what stopping here costs, because the expensive one is
# silent: `up` applies the two migration chains after these waits, the Reporting
# API never migrates on its own, and its readiness probe answers 503 for as long
# as its database carries no schema. A reader who does not know that sees a pod
# sitting at 0/1 and no reason for it.
define await
	@attempt=1; 	until $(KUBECTL) $(1) --timeout=$(WAIT_TIMEOUT); do 		if [ "$$attempt" -ge '$(WAIT_ATTEMPTS)' ]; then 			echo '' >&2; 			echo 'ERROR: $(2) did not become ready in $(WAIT_ATTEMPTS) waits of $(WAIT_TIMEOUT).' >&2; 			echo '       make up stops here, so the stack is incomplete. Stopping before' >&2; 			echo '       the migration chain leaves the Reporting API at 0/1 until a later' >&2; 			echo '       make up applies it: it never migrates on its own.' >&2; 			echo '       kubectl --context $(KUBE_CONTEXT) get pods -A, and the events of' >&2; 			echo '       the pod that is not ready, say why it is not.' >&2; 			echo '       make up is safe to run again: it reuses the cluster and carries on.' >&2; 			exit 1; 		fi; 		attempt=$$((attempt + 1)); 		echo '==> $(2) is not ready after $(WAIT_TIMEOUT); the node may still be pulling an image, waiting again ('"$$attempt"'/$(WAIT_ATTEMPTS))'; 	done
endef

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
# Every image the stack runs that is not built here, read out of the manifests
# for the same reason and with the same care as the two below: the class admits
# what a repository and a tag may hold and nothing else, so no manifest line can
# decide what `up` pulls. The `:dev` tags are what `images` builds, and the
# SERVICES loop of `up` puts those on the node.
NODE_IMAGES := $(shell grep -rhoE 'image: [A-Za-z0-9._/-]+:[A-Za-z0-9._-]+' deploy/kubernetes/base | sed 's/^image: //' | grep -v ':dev$$' | sort -u)

VMALERT_IMAGE := $(shell grep -oE 'victoriametrics/vmalert:[A-Za-z0-9._-]+' deploy/kubernetes/base/vmalert/vmalert.yaml | head -n1)
ALERTMANAGER_IMAGE := $(shell grep -oE 'prom/alertmanager:[A-Za-z0-9._-]+' deploy/kubernetes/base/alertmanager/alertmanager.yaml | head -n1)

.PHONY: check-tools up down dev ca test lint fmt check-alerting migrate generate \
	images simulator-up simulator-down docs docs-build

# What `check-tools` holds the Docker engine to. One kind node runs the whole
# stack, and an engine given less than this spends the readiness waits of `up`
# swapping rather than pulling. They are floors, not the size of the machine
# this was written on: what the engine was actually given is printed beside
# them.
DOCKER_MIN_CPUS ?= 4
DOCKER_MIN_MEMORY_GB ?= 8

# The language version go.mod states, which is the minimum a toolchain on the
# host has to satisfy. It is read out of the file rather than written a second
# time here, so a bumped module carries this with it. That Go downloads the
# version the `toolchain` line names on its first run inside the repository,
# which is why nothing here asks for that one.
GO_MIN_VERSION := $(shell sed -n 's/^go \([0-9][0-9.]*\)$$/\1/p' go.mod)

# check-tools runs one probe per tool and prints what the probe answered, rather
# than judging a version against a list. A version written here would be a
# second pin beside the ones that decide anything, deploy/kind/kind.yaml, the
# manifests and go.mod, and it would age on its own, telling a reader whose
# machine works that their machine is wrong. What the target asserts is that the
# tool is on the path and answers, and for Go that it satisfies the minimum the
# module itself states.
#
# The probe function takes what to call the tool, what needs it, and the command
# that proves it works. A command that is not there exits 127, which is what
# separates a tool nobody installed from one that is installed and answering
# with an error: a Docker whose engine is not running fails the second way, and
# a reader told to install Docker there would look for a long time.
#
# What Docker Desktop was given is a warning rather than a failure. The stack
# comes up on less, more slowly, and that number is the first thing to look at
# when a readiness wait of `up` runs out.
#
# Nothing here touches a cluster, so the target runs before `up` ever has, which
# is where a missing tool is cheapest to find. The Node toolchain is not probed:
# `docs` and `docs-build` are the only targets that need it, and the `npm ci`
# they run says plainly enough what is missing.
## check-tools: check that the tools the dev stack and the tutorials need answer
check-tools:
	@failed=0; \
	report() { printf '%-8s %-15s %s\n' "$$1" "$$2" "$$3"; }; \
	first() { printf '%s\n' "$$1" | head -n1; }; \
	probe() { \
		label="$$1"; need="$$2"; shift 2; \
		if output="$$("$$@" 2>&1)"; then \
			report ok "$$label" "$$(first "$$output")"; \
			return 0; \
		else \
			status=$$?; \
		fi; \
		failed=$$((failed + 1)); \
		if [ "$$status" -eq 127 ]; then \
			report missing "$$label" "not on the path, and $$need needs it"; \
		else \
			report broken "$$label" "$$(first "$$output")"; \
		fi; \
	}; \
	echo '==> the tools the dev stack, the simulator stack and the tutorials are driven with'; \
	probe git 'cloning the repository' git --version; \
	probe docker 'building the images and running the kind node' docker version --format 'engine {{.Server.Version}}'; \
	probe 'docker compose' 'the simulator stack of deploy/compose' docker compose version; \
	probe kind 'creating the dev cluster' kind version; \
	probe kubectl 'every call the targets make against the cluster' kubectl version --client; \
	probe jq 'reading the JSON the lessons print' jq --version; \
	probe curl 'the calls against the Reporting API' curl --version; \
	if goversion="$$(go env GOVERSION 2>&1)"; then \
		if [ "$$(printf '%s\n%s\n' '$(GO_MIN_VERSION)' "$${goversion#go}" | sort -V | head -n1)" = '$(GO_MIN_VERSION)' ]; then \
			report ok go "$$goversion, at or above the go $(GO_MIN_VERSION) of go.mod"; \
		else \
			report old go "$$goversion is under the go $(GO_MIN_VERSION) go.mod asks for"; \
			failed=$$((failed + 1)); \
		fi; \
	else \
		report missing go 'not on the path, and every binary here is built with it'; \
		failed=$$((failed + 1)); \
	fi; \
	if resources="$$(docker info --format '{{.NCPU}} {{.MemTotal}}' 2>/dev/null)"; then \
		cpus="$${resources%% *}"; \
		gb=$$(( ($${resources##* } + 536870912) / 1073741824 )); \
		if [ "$$cpus" -ge '$(DOCKER_MIN_CPUS)' ] && [ "$$gb" -ge '$(DOCKER_MIN_MEMORY_GB)' ]; then \
			report ok 'docker size' "$$cpus CPUs and $$gb GB"; \
		else \
			report warn 'docker size' "$$cpus CPUs and $$gb GB, under the $(DOCKER_MIN_CPUS) CPUs and $(DOCKER_MIN_MEMORY_GB) GB make up wants"; \
		fi; \
	fi; \
	echo; \
	if [ "$$failed" -gt 0 ]; then \
		echo "ERROR: $$failed of the checks above did not pass. make up would reach" >&2; \
		echo '       the same tool minutes in and stop there, with a stack half' >&2; \
		echo '       created. docs/contributing/dev-stack.md says what each of them' >&2; \
		echo '       is used for.' >&2; \
		exit 1; \
	fi; \
	echo 'Every tool answered. make up creates the cluster.'

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
	$(call await,-n cert-manager rollout status deployment/cert-manager,cert-manager)
	$(call await,-n cert-manager rollout status deployment/cert-manager-webhook,the cert-manager webhook)
	$(call await,-n cert-manager rollout status deployment/cert-manager-cainjector,the cert-manager cainjector)
	@echo '==> installing Envoy Gateway $(ENVOY_GATEWAY_VERSION)'
	$(KUBECTL) apply --server-side -f https://github.com/envoyproxy/gateway/releases/download/$(ENVOY_GATEWAY_VERSION)/install.yaml
	$(call await,-n envoy-gateway-system rollout status deployment/envoy-gateway,Envoy Gateway)
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
	@# A kind node holds none of the stack's other images on the first `make up`,
	@# so it pulls each of them itself, one at a time, inside the readiness waits
	@# below. That is where `up` used to end: on the machine the tutorials were
	@# captured on the node spent half an hour on TimescaleDB alone, past the
	@# budget of its wait, while the host Docker already held that very image. So
	@# the host's copy goes onto the node here, where nothing is timing it, and
	@# only an image the host lacks is fetched at all. It is what the phase 3
	@# drill did by hand, docs/contributing/drills/phase3.md, before `up` did it.
	@#
	@# An image the node already carries is skipped, which is what keeps a second
	@# `make up` from moving every one of them again.
	@for image in $(NODE_IMAGES); do \
		if docker exec '$(CLUSTER_NAME)-control-plane' crictl inspecti "$$image" >/dev/null 2>&1; then \
			continue; \
		fi; \
		docker image inspect "$$image" >/dev/null 2>&1 || { \
			echo "==> pulling $$image"; \
			docker pull --quiet "$$image" >/dev/null; \
		}; \
		kind load docker-image "$$image" --name '$(CLUSTER_NAME)'; \
	done
	@echo '==> applying the dev overlay'
	$(KUBECTL) apply -k $(DEV_OVERLAY)
	$(call await,-n $(NAMESPACE) rollout status statefulset/timescaledb,TimescaleDB)
	$(call await,-n $(NAMESPACE) rollout status statefulset/victoriametrics,VictoriaMetrics)
	$(call await,-n $(NAMESPACE) rollout status deployment/otel-collector,the OpenTelemetry collector)
	$(call await,-n $(NAMESPACE) rollout status deployment/grafana,Grafana)
	$(call await,-n $(NAMESPACE) rollout status statefulset/alertmanager,Alertmanager)
	$(call await,-n $(NAMESPACE) rollout status deployment/vmalert,vmalert)
	$(call await,-n $(NAMESPACE) wait gateway/tally --for=condition=Programmed,the Gateway)
	@# The API stays unready until the database carries its schema, and it never
	@# migrates on its own, so the chain has to be applied before the rollout can
	@# finish. It runs through the Gateway, which is what the wait above is for.
	@echo '==> applying the migration chain'
	$(MAKE) migrate
	$(call await,-n $(NAMESPACE) rollout status deployment/reporting-api,the Reporting API)
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
# sign of it is a line in the collector's log. With SIM_REGISTER_PROJECTS=true
# a second credential, an admin api token for the project registry, is issued
# the same way; with the switch off it is the empty string. The file is removed
# and written again under umask 077, because that api token writes the whole
# registry and the default umask of a shell would leave it readable by every
# other user of the machine, as would the mode of a file an earlier run left
# behind. The OTLP password is a third credential the same file carries, and the
# same umask covers it. `tally-reporting-admin revoke-api-token <id>` is what
# ends it; the id is the one the issuing line above prints, and
# `simulator-down` revokes nothing.
## simulator-up: run the simulator, the collector, and a broker against the dev cluster
simulator-up:
	@[ -n '$(SIM_PERIOD)' ] || { echo 'ERROR: set SIM_PERIOD to the past month to simulate, e.g. make simulator-up SIM_PERIOD=2026-07' >&2; exit 1; }
	@case '$(SIM_REGISTER_PROJECTS)' in true|false) ;; *) echo 'ERROR: SIM_REGISTER_PROJECTS must be true or false' >&2; exit 1;; esac
	$(MAKE) images IMAGES='$(SIM_IMAGES)'
	@echo '==> writing the dev CA to tally-ca.crt'
	$(MAKE) -s ca > tally-ca.crt
	@echo '==> issuing an ingest credential for $(SIM_CLOUD)'
	@token="$$(TALLY_REPORTING_DB_URL='$(TALLY_DEV_DB_URL)' go run ./cmd/tally-reporting-admin create-ingest-credential --platform openstack --cloud '$(SIM_CLOUD)' --description 'openstack simulator')"; \
	api_token=''; \
	if [ '$(SIM_REGISTER_PROJECTS)' = true ]; then \
		echo '==> issuing an admin api token for the project registry'; \
		api_token="$$(TALLY_REPORTING_DB_URL='$(TALLY_DEV_DB_URL)' go run ./cmd/tally-reporting-admin create-api-token --role admin --description 'openstack simulator')"; \
	fi; \
	rm -f deploy/compose/.env; \
	umask 077; \
	printf 'TALLY_SIM_CLOUD=%s\nTALLY_SIM_PERIOD=%s\nTALLY_SIM_SEED=%s\nTALLY_SIM_FACTOR=%s\nTALLY_SIM_FAULTS=%s\nTALLY_SIM_REGISTER_PROJECTS=%s\nTALLY_SIM_GARDEN_CLOUD=%s\nTALLY_OSC_TOKEN=%s\nTALLY_SIM_API_TOKEN=%s\nTALLY_SIM_OTLP_USER=%s\nTALLY_SIM_OTLP_PASSWORD=%s\nTALLY_SIM_METRICS_INTERVAL=%s\n' \
		'$(SIM_CLOUD)' '$(SIM_PERIOD)' '$(SIM_SEED)' '$(SIM_FACTOR)' '$(SIM_FAULTS)' '$(SIM_REGISTER_PROJECTS)' '$(SIM_GARDEN_CLOUD)' "$$token" "$$api_token" '$(SIM_OTLP_USER)' '$(SIM_OTLP_PASSWORD)' '$(SIM_METRICS_INTERVAL)' > deploy/compose/.env
	$(COMPOSE) up -d
	@echo
	@echo 'Simulator stack is up:'
	@echo '  http://127.0.0.1:15672                               broker UI, guest/guest'
	@echo '  http://127.0.0.1:8090/metrics                        collector'
	@echo '  http://127.0.0.1:8091/clock                          simulator control'
	@echo '  http://127.0.0.1:8091/metrics                        simulator inventory, the database exporter stand-in'
	@echo '  https://api.tally.127-0-0-1.nip.io:8443/api/v1       Reporting API'
	@echo '  https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics  OTLP endpoint the series are pushed to'
	@echo '  https://vm.tally.127-0-0-1.nip.io:8443/targets       scrape targets'
	@echo
	@# The four hints below carry one shape: a line naming what the hint does,
	@# then the command or the path indented under it, with a blank line between
	@# them. The registry command is broken over three lines the way
	@# docs/how-to/simulator/register-simulated-projects.md writes it, because
	@# one line of it runs past the width of a terminal and wraps mid-token.
	@echo 'Finish the month at once:'
	@echo "  curl -X PUT -d '{\"factor\": 0}' http://127.0.0.1:8091/clock"
	@echo
	@echo 'Release the notifications a run with SIM_FAULTS=held-back keeps back:'
	@echo '  curl -X POST http://127.0.0.1:8091/release'
	@echo
	@echo 'Inspect the registry a run with SIM_REGISTER_PROJECTS=true registered into:'
	@echo '  curl --cacert tally-ca.crt \'
	@echo '    -H "Authorization: Bearer $$(grep TALLY_SIM_API_TOKEN deploy/compose/.env | cut -d= -f2)" \'
	@echo "    'https://api.tally.127-0-0-1.nip.io:8443/api/v1/projects?cloud=$(SIM_CLOUD)'"
	@echo
	@echo 'Reconcile the cloud the run serves:'
	@echo '  docs/how-to/simulator/reconcile-the-simulated-cloud.md'

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
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

## fmt: format every Go file with gofumpt, through golangci-lint's formatter
fmt:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) fmt

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

## generate: run the code generators and refresh the generated blocks of the reference pages and the handbook
generate:
	go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION) \
		-config api/reporting/oapi-codegen.yaml api/reporting/openapi.yaml
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate
	# Two of these pages are written by two packages each: the docs package
	# renders the document types of the simulator and of the vertical slice,
	# and the cmd package of each renders its own command line. Their test
	# binaries run at once and rewrite a page under its exclusive lock, so
	# neither of the two updates is written over.
	# The same run refreshes the make-target block of the dev-stack page.
	TALLY_UPDATE_DOCS=1 go test ./docs/ ./cmd/... -run 'TestReferencePage|TestContributingPages' -count=1

# npm ci recreates node_modules from the lockfile and writes this marker, which
# is what tells make the install is current. It runs once, and again after a
# lockfile change.
node_modules/.package-lock.json: package-lock.json
	npm ci

## docs: serve the documentation site locally with live reload
docs: node_modules/.package-lock.json
	npm run docs:dev

## docs-build: build the documentation site; a dead internal link fails the build
docs-build: node_modules/.package-lock.json
	npm run docs:build

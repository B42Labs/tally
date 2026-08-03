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
SERVICES := tally-reporting

# Every kubectl call names the cluster explicitly. Creating a kind cluster
# switches the current context, but reusing an existing one does not, so an
# unqualified kubectl would deploy Tally into whatever cluster happened to be
# selected.
KUBE_CONTEXT := kind-$(CLUSTER_NAME)
KUBECTL := kubectl --context $(KUBE_CONTEXT)

# Reaches TimescaleDB through the Gateway's TCP listener, which is the same path
# a developer's psql takes.
TALLY_DEV_DB_URL ?= postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable

.PHONY: up down dev ca test lint fmt migrate generate images

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
	$(KUBECTL) -n $(NAMESPACE) wait gateway/tally --for=condition=Programmed --timeout=300s
	@# The API stays unready until the database carries its schema, and it never
	@# migrates on its own, so the chain has to be applied before the rollout can
	@# finish. It runs through the Gateway, which is what the wait above is for.
	@echo '==> applying the migration chain'
	$(MAKE) migrate
	$(KUBECTL) -n $(NAMESPACE) rollout status deployment/reporting-api --timeout=300s
	@echo
	@echo 'Stack is up:'
	@echo '  https://api.tally.127-0-0-1.nip.io:8443/api/v1  Reporting API'
	@echo '  https://vm.tally.127-0-0-1.nip.io:8443          VictoriaMetrics'
	@echo '  https://otlp.tally.127-0-0-1.nip.io:8443        OTLP/HTTP'
	@echo '  db.tally.127-0-0-1.nip.io:5432                  TimescaleDB'
	@echo
	@echo 'Trust the dev CA with: make -s ca > tally-ca.crt'

## images: build one container image per service
images:
	@for service in $(SERVICES); do \
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

## migrate: apply the reporting migration chain through the admin CLI
migrate:
	TALLY_REPORTING_DB_URL='$(TALLY_DEV_DB_URL)' go run ./cmd/tally-reporting-admin migrate

## generate: run the code generators
generate:
	go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION) \
		-config api/reporting/oapi-codegen.yaml api/reporting/openapi.yaml
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION) generate

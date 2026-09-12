---
title: The toolchain
description: "The Go toolchain, the code generators, the linter and formatter, the container build, the dependency bots, and what to regenerate after a change."
quadrant: contributing
audience: contributor
---

# The toolchain

Tally is one Go module, `github.com/b42labs/tally`. Six binaries live under
`cmd/`: `tally-reporting`, `tally-reporting-admin`, `tally-engine`,
`tally-openstack-collector`, `tally-openstack-simulator` and
`tally-vertical-slice`. Everything they share lives under `internal/`. The
binding stack decisions are in section 1 of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md).
This page says which tool the repository runs each of them with, and what to
run again after a change.

## Go

Two lines of `go.mod` name a Go version, and they say different things. The
`go 1.26.0` line is the language version the module is written against and the
minimum a toolchain has to satisfy. The `toolchain go1.27.1` line is the
toolchain the module is built and tested with. A host with Go 1.26 downloads
go1.27.1 on its first `go run` or `go build` in the repository; the tutorial
[Set up your local Tally](/tutorials/set-up-your-local-tally) shows that
download. The `Dockerfile` builds with `golang:1.27.1-alpine`, the same
version.

Three places name that version and move together: the `toolchain` line of
`go.mod`, the base image of the `Dockerfile`, and the version `setup-go` reads
in `.github/workflows/ci.yaml`. The workflow sets `go-version-file: go.mod`,
so the third one follows the first on its own.

## Generated code

`make generate` runs two code generators, then the doc tests that refresh the
generated blocks of the site.

| Generator | Version | Input | Output |
| --- | --- | --- | --- |
| oapi-codegen | `OAPI_CODEGEN_VERSION` (`v2.8.0`) | `api/reporting/openapi.yaml`, configured by `api/reporting/oapi-codegen.yaml` | `internal/reporting/httpapi/openapi.gen.go`: the chi server interface, the models and the embedded spec |
| sqlc | `SQLC_VERSION` (`v1.31.1`) | `migrations/reporting`, `internal/reporting/store/queries.sql` | `internal/reporting/store/sqlcgen` |
| sqlc | `SQLC_VERSION` (`v1.31.1`) | `migrations/engine`, `internal/engine/store/queries.sql` | `internal/engine/store/sqlcgen` |
| sqlc | `SQLC_VERSION` (`v1.31.1`) | `migrations/reporting`, `internal/engine/source/queries.sql` | `internal/engine/source/sqlcgen`, the engine's read view over the reporting chain |
| sqlc | `SQLC_VERSION` (`v1.31.1`) | `migrations/engine`, `internal/console/store/queries.sql` | `internal/console/store/sqlcgen`, the demo console's read view over the engine chain |
| the doc tests under `TALLY_UPDATE_DOCS=1` | the module's own toolchain | `docs/reference_test.go`, `docs/contributing_test.go`, the `cmd` packages' `TestReferencePageIsCurrent` | the generated blocks of the reference pages and of the handbook |

Generated code is committed, so a plain `go build` needs no generator. Every
generator runs from the module cache at its pinned version, and nothing is
installed on the host.

## What to regenerate after a change

| You changed | Run | What changes | What fails until you do |
| --- | --- | --- | --- |
| `api/reporting/openapi.yaml` | `make generate` | `openapi.gen.go`, the [endpoints](/reference/api/reporting-api) and [schemas](/reference/api/reporting-api-schemas) pages | a handler set that no longer matches the generated server interface fails `go build`; a stale page fails `TestReferencePagesAreCurrent` |
| a file under `migrations/reporting/`, `internal/reporting/store/queries.sql`, `internal/engine/source/queries.sql` | `make generate` | the reporting and the source `sqlcgen` packages | a query a store calls that no longer exists fails `go build`; `internal/reporting/store/migrate_test.go` runs the chain against a container |
| a file under `migrations/engine/`, `internal/engine/store/queries.sql`, `internal/console/store/queries.sql` | `make generate` | the engine and the console `sqlcgen` packages | `go build`, the same way; `internal/engine/store/migrate_test.go` |
| a `Config` struct, a cobra command tree, a manifest under `deploy/`, a dashboard, `internal/engine/pricing/pricing.schema.json`, `internal/core/event/event.go`, an export writer, a golden file under `internal/engine/export/testdata/golden/` | `make generate` | the reference page whose subtest names the source, in `docs/reference_test.go` or in the `TestReferencePageIsCurrent` of the binary's `cmd` package | `TestReferencePagesAreCurrent`, or that `TestReferencePageIsCurrent`, reading `block "..." differs from its source, run make generate` |
| a `## target:` comment of the `Makefile` | `make generate` | the `make-targets` block of [The dev stack](/contributing/dev-stack) | `TestContributingPagesAreCurrent` |

## Lint and format

`make lint` and `make fmt` run golangci-lint v2 at `GOLANGCI_LINT_VERSION`
(`v2.13.2`) from the module cache, so neither uses a binary on the host. A
golangci-lint built with an older Go than the module's toolchain refuses the
module, which is why the targets do not call one. The first run compiles the
linter with the module's toolchain and takes a while; later runs start from the
build cache.

`.golangci.yml` enables the `standard` linter set plus `forbidigo` with two
rules, `decimal.NewFromFloat` and `.InexactFloat64`. Money and usage quantities
never come from and never leave decimal form (section 6 of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md)),
so both patterns are rejected where they appear. The one formatter is
`gofumpt`, which `make fmt` applies.

CI runs the same version through `golangci/golangci-lint-action`. Its
`version:` in `.github/workflows/ci.yaml` and the Makefile's pin are kept equal
by hand. `go vet ./...` runs beside it there and is worth running locally.

## Container images

`make images` builds one image per binary from the one `Dockerfile`. Each build
passes `--build-arg CMD=<binary>` and tags the result `<binary>:dev`. The file
has two stages: the first compiles a static binary with `CGO_ENABLED=0`,
`-trimpath` and `-ldflags="-s -w"`, the second copies that binary onto
`gcr.io/distroless/static-debian12:nonroot` and runs it as `nonroot`. Dev and
prod run the same image.

`IMAGES` is wider than `SERVICES`. `SERVICES` is what `make up` loads into
kind, `tally-reporting` and `tally-engine`;
[The dev stack](/contributing/dev-stack) is that cluster. `IMAGES` adds
`tally-openstack-collector` and `tally-openstack-simulator`, which run beside a
broker rather than in the cluster.

`.dockerignore` keeps `node_modules/` and the site's build and cache
directories, `docs/.vitepress/dist/` and `docs/.vitepress/cache/`, out of the
build context.

## Debian package

`make deb` builds `tally-openstack-collector` as a `.deb` into `dist/`. The
collector is the one binary that runs on a host rather than in a cluster, next
to the broker of an OpenStack control plane, so it is the one that is packaged;
everything else ships as an image.

The target cross-compiles `linux/$(DEB_GOARCH)` with the `Dockerfile`'s build
flags, so the packaged binary is the image's binary, and then runs
[nfpm](https://nfpm.goreleaser.com/) at `NFPM_VERSION` (`v2.47.0`) from the
module cache. Nothing is installed on the host and Docker is not involved, so
the target runs on macOS as well; reading the result there takes `ar x` and
`tar tzvf data.tar.gz`, because macOS has no `dpkg-deb`.

`nfpm.yaml` at the repository root is the package definition, and `packaging/`
holds what it installs:

| Path | Content |
| --- | --- |
| `/usr/bin/tally-openstack-collector` | the static binary |
| `/lib/systemd/system/tally-openstack-collector.service` | the unit, running as the `tally` system user |
| `/etc/default/tally-openstack-collector` | every variable with its default, a conffile |
| `/etc/tally/amqp-url`, `/etc/tally/ingest-token` | the two secrets, `0640 root:tally`, conffiles shipped empty |
| `/var/lib/tally/collector/` | the outbox directory, `0750 tally:tally` |

`postinstall.sh` creates the `tally` user and group and applies the ownership
dpkg cannot resolve at unpack time; `preremove.sh` stops and disables the unit;
`postremove.sh` drops `/etc/tally` on purge and keeps the outbox, because
between the acknowledgement on the bus and the delivery an event lives in that
file and nowhere else.

`packaging/packaging_test.go` pins all of it to `openstack.EnvNames` and to the
paths the unit uses, and it reads files rather than building, so it needs
neither Docker nor dpkg. The `package` step of `.github/workflows/ci.yaml` is
what builds, installs, verifies and purges the package on a runner. Publishing
it on a tagged release is not set up yet.

## Dependencies

`go.mod` and `go.sum` pin every module the build resolves. Renovate proposes
updates as pull requests: Go modules, npm packages, `.nvmrc` and the GitHub
Actions the workflows use. Its configuration, `renovate.json`, is
`config:recommended` and nothing else.

The site carries a Node toolchain beside that. `package.json` pins `vitepress`
exactly, at `1.6.4`, and its `engines` field asks for Node 24 or newer.
`.nvmrc` says `24`, which is the version `setup-node` reads. The committed
`package-lock.json` is what `npm ci` installs from.

## Continuous integration

`.github/workflows/ci.yaml` holds two jobs, and both run on every pull request
and on every push to `main`.

The `ci` job checks the repository out, runs `setup-go` against `go.mod`, and
then the lint action, `go vet ./...`, `go test ./...` and
`make check-alerting`. Docker on the runner is what the integration tests and
`check-alerting` use; kind is never installed there, so the cluster of
[The dev stack](/contributing/dev-stack) runs on a contributor's machine alone.

The `docs` job runs `setup-node` against `.nvmrc`, `npm ci` and
`npm run docs:build`. VitePress fails the build on a dead internal link, so
that step is the link check of the site.

`deploy-docs.yaml` publishes `main` to GitHub Pages. It runs when a push
touches `docs/`, `package.json`, `package-lock.json`, `.nvmrc` or the workflow
itself.

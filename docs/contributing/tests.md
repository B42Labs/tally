---
title: How the tests are organised
description: "The unit tests, the container-backed integration tests, the golden suites and the tests that pin manifests and pages, and how to run them on your machine."
quadrant: contributing
audience: contributor
---

# How the tests are organised

`make test` is `go test ./...` over every package of the module. One command
runs everything, and the four kinds of test below are told apart by what each
needs from the machine: nothing at all, Docker, Docker plus the golden
fixtures, or the files on disk. The standard they are written to is section 9
of
[`roadmap/00-conventions.md`](https://github.com/B42Labs/tally/blob/main/roadmap/00-conventions.md).

## Unit tests

A unit test covers pure logic and does no I/O. It sits beside the code it
tests, in the same package or in that package's `_test` package, and it needs
nothing installed on the machine.

Most of `internal/core/` is of this kind: `event` with the validation rules of
the canonical event, `timeline` with the interval folding, `money`, `ids`,
`adjustment`, `project`, `cardinality` and `health`. Beside them sit the
OpenStack notification mapping
(`internal/providers/openstack/mapping_test.go`), the renderers of
`internal/refdoc` that produce the generated blocks of the reference pages,
and the generators of `internal/providers/openstack/simulator`, which draw a
month of instances, volumes, load balancers and noise from a seed. These run
in milliseconds.

## Integration tests

Every file named `*_integration_test.go`, and every test that imports one of
the two `storetest` packages, starts a database container of its own.

`internal/reporting/store/storetest` runs TimescaleDB on
`timescale/timescaledb:latest-pg16`, the image the dev cluster runs, so a test
meets the version a developer's cluster does.
`internal/engine/store/storetest` runs plain PostgreSQL 16 on `postgres:16`,
without the TimescaleDB extension the reporting side needs. Both wait until
the server answers a query rather than until it logs a line, apply the real
migration chain to it, open a pool on the result, and tear the container down
when the test ends.

The two AMQP tests, `internal/providers/openstack/amqp_integration_test.go`
and `internal/providers/openstack/simulator/publish_integration_test.go`,
start a RabbitMQ container the same way, `rabbitmq:4-alpine` through
testcontainers. Each of them starts a broker of its own, because the
collector's queue name is fixed and two tests on one broker would consume each
other's notifications.

None of these carries a build tag, and there is no `-short` mode: a plain
`go test ./...` runs them all. Without a reachable Docker daemon every one of
them fails at `starting the database container`, so Docker Desktop has to be
running before `make test`, and the first run pulls the two database images.

Section 9 asks for real databases rather than mocks. What these tests are
about is the SQL and the schema: ingestion, deduplication, projection replay,
reconciliation diffing and metering runs. A faked store answers whatever the
test taught it, which proves the test and not the query, and it never meets
the migration chain at all.

## The golden suites

### What a case holds

`internal/engine/testdata/golden/` holds 16 case directories. Every one of
them carries `events.json`, the history the case is metered from, and
`registry.json`, the projects, the relations and the adjustments it is rated
under. Thirteen carry `expected.json`, the figures the run is held against.
`correction_credit`, `invariant_violation` and `scheduler_drill` keep their
expectations in Go instead, because what each of them asserts is a sequence of
runs rather than one table of numbers.

Three more kinds of fixture appear where a case needs them. `instance_resize`,
`harbor_counters` and `e2e_power_cycle` carry `counters.yaml` with
`metrics.json`, the metricsql counter sources the case meters and the answers
the stubbed querier gives them. `correction_credit`, `reseller` and
`scheduler_drill` carry `late.json`, the events that arrive after the period
was closed. `temporal` carries `expected_april.json`, the second month that
case bills.

### The cases and the gates they belong to

`TestGolden` runs 21 subtests. Twelve of them are seeded and read back by one
loop, which meters one period once. The nine after the loop carry a month
further than that first pass.

Nine subtests are the Phase 3 gate of
[WP 3.11](https://github.com/B42Labs/tally/blob/main/roadmap/03-phase-3-metering-rating.md):
`instance_resize`, `hetzner_upgrade`, `volume_resize_retype`,
`shoot_scale_hibernate`, `harbor_counters`, `e2e_power_cycle`,
`related_costs`, `correction_credit` and `reproducibility`.

Six are the Phase 5 gate of
[WP 5.6](https://github.com/B42Labs/tally/blob/main/roadmap/05-phase-5-commercial-pricing.md):
`reseller`, `scoped_discount`, `inherited_member_discount`,
`order_and_stacking`, `temporal` and `phase3_regression`.

Six were added beside the two gates: `virtual_relations`,
`invariant_violation`, `scheduler_drill`, `adjusted_reproducibility`,
`auditability_drill` and `adjusted_correction`.

Four of the 21 have no directory of their own and run on the fixtures of other
cases: `phase3_regression`, `adjusted_reproducibility`, `auditability_drill`
and `adjusted_correction`.

### How a case runs

One run of the suite starts two containers, and every case gets a pair of
databases of its own inside them, so the cases share the containers without
sharing anything they bill, and the images and the migration chains are paid
for once rather than once per case.

A case's events are inserted into the events table and folded into the
candidate index by `projection.Replay`, the writer the ingest path folds
through, so a case is metered from the index production derives. The metricsql
querier is the only seam the suite substitutes, because VictoriaMetrics is
another process. Everything else a case runs through is the engine's own code,
down to the Reporting API the auditability drill walks, which is built in
process over the case's reporting database through the real router.

### Why the suite allows no tolerance

Every expected value in the suite was derived by hand, from
[Worked examples](/explanation/worked-examples), from the WP 5.6 table of the
Phase 5 document and from `pricing/2026-03.yaml`. None of them is written back
from a run: a number the engine produced says nothing about whether the engine
is right, and a suite that regenerates its expectations records the last change
instead of judging it.

Money is decimal from end to end and rounded at one entry point
([Money and rounding](/explanation/money-and-rounding)), so two correct
implementations of the same rule agree to the cent. A tolerance would admit
nothing but a wrong one.
[`roadmap/README.md`](https://github.com/B42Labs/tally/blob/main/roadmap/README.md)
states the rule for the whole project: a work package is done when its
acceptance criteria and its tests pass, and "roughly matching" numbers are a
failure.

### The golden fixtures outside the engine

- `cmd/tally-vertical-slice/slice_integration_test.go` rates the Phase 1
  golden numbers over one project of one cloud, whose instances carry the
  history of the concept's section 3.4 example.
- `internal/providers/openstack/testdata/golden/` holds the mapping fixtures,
  `notifications` in and `events` out. Every captured notification is run
  through the mapping and held against the event it is expected to become,
  byte for byte, and a fixture with no expected event is one the table is
  meant to skip. The test checks the other direction as well, so an expected
  event that no notification produces fails.
- `internal/core/testkit` is the conformance kit every provider collector
  passes: schema validity, deterministic event ids and the envelope rules,
  written once rather than once per provider.

## Tests that pin manifests and pages

Each of these reads a file the build never compiles, and each catches a
mismatch that would otherwise fail quietly.

- `deploy/compose/compose_test.go` pins the compose stack to what the two
  binaries in it read. A variable no binary reads leaves the container
  starting on the default it was meant to replace, a buffer path outside the
  outbox volume puts the undelivered events in the writable layer that
  `docker compose down` drops, and a missing `extra_hosts` entry sends every
  flush to a name the container's resolver does not know.
- `deploy/kubernetes/base/alertmanager/manifest_test.go` pins who may change
  Alertmanager's state and where a firing alert ends up. A route that forwards
  `POST /api/v2/silences` renders exactly like one that refuses it, and a
  critical alert that repeats no sooner than the rest is noticeable only once
  an incident is four hours old.
- `deploy/kubernetes/base/grafana/manifest_test.go` pins how far an anonymous
  request reaches. A pod that mounts the wrong Secret key still passes its
  readiness probe, a route that publishes the datasource proxy still renders
  every panel, and a session cookie without `Secure` looks the same in the
  browser.
- `deploy/kubernetes/base/tally-engine/manifest_test.go` pins the scheduler
  CronJob's contract with the engine binary and with the secret it reads its
  two databases from. A manifest that lost the tick argument runs the engine's
  help text and the Job succeeds; none of it stalls a rollout or fails a
  probe, and it ends in a Job history nobody reads.
- `deploy/kubernetes/base/timescaledb/manifest_test.go` pins the wiring that
  creates the engine's database on a fresh cluster. A StatefulSet that lost an
  initdb mount starts a Postgres which passes both probes and answers every
  reporting query, and what is missing surfaces one step later in
  `make migrate`.
- `deploy/kubernetes/base/vmalert/manifest_test.go` pins whether an evaluated
  rule reaches anyone. A vmalert that has lost `-notifier.url` still starts,
  still evaluates every rule and still answers its probes while nothing is
  ever posted to Alertmanager.
- `deploy/kubernetes/overlays/dev/manifest_test.go` pins the two files this
  overlay adds to the metrics pipeline. A scrape config that dropped or
  renamed a job of the base leaves `TallyScrapeTargetDown`,
  `TallyScrapeJobMissing` and `TallyExporterServiceSilent` selecting jobs the
  cluster no longer scrapes, and neither the scrape nor the rules fail on
  their own.
- `deploy/kubernetes/base/grafana/dashboards_test.go` pins the JSON contract
  of the provisioned dashboards. Grafana loads these files at startup and
  reports a broken one only in its own log, so a truncated file or a renamed
  datasource reaches a cluster before anyone sees it.
- `deploy/kubernetes/base/vmalert/rules_test.go` pins what turns a metric into
  a page. A rule dropped in an edit leaves the condition it watched unwatched,
  and a runbook annotation naming a page that was renamed hands whoever is
  woken at 03:00 a dead link.
- `migrations/reporting/embed_test.go` runs `TestVersionMatchesTheChain` over
  the reporting chain. A migration added without raising the constant
  readiness compares against would let a pod serve traffic on a schema its
  code is newer than.
- `migrations/engine/embed_test.go` runs the same test over the engine chain,
  where the constant names the schema the code expects.
- `docs/docs_test.go` pins every page of this site to the rules
  [Authoring conventions](/contributing/authoring-conventions) states. A page
  no sidebar links is published and never opened, a heading style drifts
  across six parallel pull requests before anyone compares two of them, and a
  sidebar entry pointing at nothing renders as a dead link.
- `docs/reference_test.go` pins the generated blocks of the reference pages to
  the sources they are rendered from. Nothing about a stale operation table
  looks stale, so each subtest renders one page's blocks and hands them to
  `refdoc.Verify`.
- `docs/contributing_test.go` does the same for the handbook, whose
  make-target table is rendered from the `## target:` comments of the
  `Makefile`.

They read from disk and start nothing.

## Running the tests

`make test` runs the whole suite. While working on one thing, narrow the run
to a package and a subtest:

```sh
go test ./internal/engine/ -run 'TestGolden/reseller' -count=1
go test ./docs/ -run 'TestHeadingsAreSentenceCaseWithOneH1/how-to'
```

`go vet ./...` and `make lint` are the other two checks CI applies to the Go
code; [The toolchain](/contributing/toolchain) says which linter version they
pin. `make check-alerting` loads `rules.yaml` into the vmalert image the
cluster runs and the Alertmanager config into `amtool` from the Alertmanager
image, so an expression or a routing field the pinned version rejects fails
here rather than in the cluster. It needs Docker and no cluster.

Every integration test pays for a container start of its own, so the
integration packages dominate the wall clock of a full run and narrowing to a
package is how to get an answer while a change is still in progress.

## What CI runs

The `ci` job of `.github/workflows/ci.yaml` runs the tests, and
[Continuous integration](/contributing/toolchain#continuous-integration) lists
its steps.

What it leaves out are the acceptance drills and the tutorials. They run on a
contributor's machine against the dev stack, which is why each drill record
names the commit it ran at: [Phase 2](/contributing/drills/phase2) and
[Phase 3](/contributing/drills/phase3).

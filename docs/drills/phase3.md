# Phase 3 acceptance drill: a month through metering, rating, finalization and export

Exit criterion 4 of
[`../../roadmap/03-phase-3-metering-rating.md`](../../roadmap/03-phase-3-metering-rating.md#phase-exit-criteria)
asks for a full month of dev-stack OpenStack data that meters, rates, finalizes
and exports without warnings, or with each warning triaged. This drill is that
month. Meta Issue #33 makes it part of the phase's definition of done.

The same month runs twice on one dev cluster. The first run is clean, and it is
what the exit criterion asks for. The second runs with five fault switches on,
and it is the triage: every warning it records and every difference it produces
has to carry a switch that accounts for it.

The month is the simulated one of seed 1 over `2026-07` on cloud `os-sim`,
described in [`../openstack-simulator.md`](../openstack-simulator.md). The truth
the export is held against is the oracle the simulator writes beside the month
it publishes. The warnings and the differences the second run triages are the
ones the fault switches earn.

The drill needs Docker Desktop, kind, `kubectl`, Go, `jq` and `curl`, and the
macOS setup the Makefile assumes. Every command runs from the repository root.
At factor 744 the two months take about three hours of wall clock, most of it
spent waiting for notifications to go out.

A difference `compare` prints that no switch explains, and a warning a run
records that neither a switch nor a cause stated below accounts for, is a
finding about the engine. It is filed as an issue of its own and never fixed
inside the drill, and the checklist at the end stays unticked until it is
closed.

The drill runs outside CI for the reason the vertical slice's does
([the vertical slice page](https://b42labs.github.io/tally/reference/command-line/tally-vertical-slice#verification)):
it needs Docker, a kind cluster and hours of wall clock, and the CI runner has
the first of the three alone.

## Preparation

`make up` creates the kind cluster and deploys the dev overlay: the Reporting
API, the hourly `tally-engine` CronJob, VictoriaMetrics, the OTel collector, and
both migration chains applied through the Gateway. Against a cluster that
already exists the target reuses it and applies the overlay again, so running it
twice costs time and changes nothing.

```sh
make up
```

A first `make up` on a machine that pulls the images of the overlay for the
first time can outlast a rollout timeout and fail there. It is run again once
the pods are Running.

Every engine command of this drill runs in one shell, and this block is exported
into that shell once:

```sh
export TALLY_ENGINE_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_engine?sslmode=disable'
export TALLY_ENGINE_REPORTING_DB_URL='postgres://tally_engine:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable'
export TALLY_ENGINE_COUNTER_SOURCES=deploy/kubernetes/overlays/dev/counter-sources.yaml
export TALLY_ENGINE_VM_URL=http://127.0.0.1:8428
kubectl --context kind-tally -n tally port-forward svc/victoriametrics 8428:8428 &
```

- `TALLY_ENGINE_DB_URL` is the engine's own database on the Gateway's TCP
  listener, the URL `make migrate` runs the engine chain against.
- `TALLY_ENGINE_REPORTING_DB_URL` reads the reporting database as the login role
  `tally_engine`, which
  [`02-create-engine-reader.sh`](../../deploy/kubernetes/base/timescaledb/02-create-engine-reader.sh)
  creates with the password the dev overlay generates. The overlay carries the
  same URL in the `tally-db` secret under the key `engine-reporting-db-url`.
- `TALLY_ENGINE_COUNTER_SOURCES` points at
  [`counter-sources.yaml`](../../deploy/kubernetes/overlays/dev/counter-sources.yaml)
  of the dev cluster. The variable defaults to the path
  `/etc/tally/counter-sources.yaml`, which no laptop carries.
- The port-forward reaches Service `victoriametrics` on port 8428, which
  `TALLY_ENGINE_VM_URL` names.

The port-forward exists because the engine's VictoriaMetrics client is built
with no CA of its own and Go on macOS reads no `SSL_CERT_FILE`, so the Gateway's
`https` hostname does not verify from the host.

The egress source is declared `required: false`, so a port-forward that died
leaves the run completed with one `counter_source_failed` warning per instance
draft and no `egress_gb` quantity behind it. On the clean month such a warning
means the store was unreachable and nothing else. The fix is to restart the
port-forward and run the month again; the new run supersedes the one that
carries the warnings and names it as `superseded run <id>` on its own output.

The exports and the oracles go under one directory of that shell's own:

```sh
export DRILL_DIR="$(mktemp -d "${TMPDIR:-/tmp}/tally-drill.XXXXXX")"
```

`mktemp -d` creates the directory itself, under a name no second drill and no
other process on the machine picks. A fixed path under a world-writable `/tmp`
is one an unprivileged process can pre-create as a symlink into a directory it
owns, and the export's non-empty-directory guard below does not fire on an empty
directory somebody else owns. The value goes into the write-up: the exported
files are the record, and every path below names them through it.

A `run` before the pricing import fails, and the error ends in `no pricing model
is valid for this period`. It leaves a `billing_periods` row behind all the
same, because the period is recorded before the prices are resolved, and the
hourly tick walks from that row on ("What else the run shows" below).

`pricing/2026-03.yaml` prices `openstack/instance`, `openstack/volume` and
`openstack/floating_ip`, and it prices neither `image` nor `loadbalancer`. The
drill imports it unchanged (author's decision of 2026-09-03), so those two types
are the two warnings the clean month is expected to record:

```sh
go run ./cmd/tally-engine pricing import pricing/2026-03.yaml
```

```text
imported pricing model 2026-03 valid from 2026-03-01T00:00:00Z
```

A second import of the same file answers `pricing model 2026-03 already
imported` and stores nothing.

## The clean month

The stack that publishes the month starts with:

```sh
make simulator-up SIM_PERIOD=2026-07 SIM_REGISTER_PROJECTS=true
```

The other four values stay at their defaults: `SIM_SEED=1`, `SIM_CLOUD=os-sim`,
`SIM_FACTOR=744`, which puts a 31-day month on the bus in an hour, and
`SIM_GARDEN_CLOUD=garden-sim`. The target builds the collector and simulator
images, writes the dev CA to `tally-ca.crt`, issues an ingest credential for
`os-sim` and, because of the switch, an admin api token beside it, writes both
into `deploy/compose/.env` under `umask 077`, and starts the broker, the
collector and the simulator of
[`compose.yaml`](../../deploy/compose/compose.yaml). It prints seven URLs:

- `http://127.0.0.1:15672`, the broker's management UI, guest/guest
- `http://127.0.0.1:8090/metrics`, the collector
- `http://127.0.0.1:8091/clock`, the simulator's control endpoint
- `http://127.0.0.1:8091/metrics`, the simulator's inventory
- `https://api.tally.127-0-0-1.nip.io:8443/api/v1`, the Reporting API
- `https://otlp.tally.127-0-0-1.nip.io:8443/v1/metrics`, the OTLP endpoint
- `https://vm.tally.127-0-0-1.nip.io:8443/targets`, the scrape targets

A call without the period is refused before an image is built, with `ERROR: set
SIM_PERIOD to the past month to simulate, e.g. make simulator-up
SIM_PERIOD=2026-07`. Against a cluster that is not up the target gets as far as
the credential step and fails there with the admin CLI's connection error, which
is what the Makefile comment above the target says.

`SIM_REGISTER_PROJECTS=true` registers the month's six tenants and its two
Gardener projects with the dev registry before the first notification goes out
([the project registry](../openstack-simulator.md#the-project-registry)).

A `run` also serves the month as an OpenStack API
([the fake OpenStack API](../openstack-simulator.md#the-fake-openstack-api)), so
the reconciliation loop starts in a second shell right after the URLs print. It
posts one sync per minute and tells each one where the month stands
([triggering a sync](../openstack-reconciliation.md#triggering-a-sync),
[telling a sync the instant it runs at](../openstack-reconciliation.md#telling-a-sync-the-instant-it-runs-at)):

```sh
kubectl --context kind-tally -n tally port-forward svc/reporting-api 8082:80 &
while true; do
  doc="$(curl -sS --max-time 30 http://127.0.0.1:8091/clock)" \
    || echo "clock read failed at $(date -u +%FT%TZ)" >&2
  at="$(jq -r 'if .virtual_now > .period_to
               then .period_to else .virtual_now end' <<<"$doc")"
  body="$(jq -nc --arg at "$at" '{at: $at}')"
  curl -sS --max-time 65 -X POST \
    -H "Authorization: Bearer tally-dev-internal-token" \
    -H 'Content-Type: application/json' -d "$body" \
    http://127.0.0.1:8082/internal/sync/os-sim \
    || echo "sync post failed at $(date -u +%FT%TZ) (at=$at)" >&2
  echo
  sleep 60
done
```

`tally-dev-internal-token` is the internal token of the dev overlay. That
overlay is also what wires the Reporting API to the fake OpenStack API the
simulator serves: `deploy/kubernetes/overlays/dev/reconciliation/` holds the
clouds config and the clouds.yaml for cloud `os-sim`, and the overlay sets
`TALLY_REPORTING_SYNC_ALLOW_AT` to true so the `at` member is taken. Sixty wall
seconds are about 12.4 virtual hours at factor 744.

Neither call is allowed to block or to fail quietly. A curl that blocks stalls
the loop, and an hour of it is the rest of the month unsynced while the loop
still looks like it is working; a curl that reached nothing at all is the same
loss without the wait. The clock read carries `--max-time 30`. The sync post
carries `--max-time 65`, which is past every answer the Reporting API can still
deliver: the 45 seconds `syncBudget` gives the run itself
(`internal/reporting/httpapi/sync.go`), the two writes that end it, each on a
context detached from that budget and bounded by a `completionBudget` of 10
seconds (`internal/reporting/reconciliation/sync.go`), and the 60-second
`writeTimeout` the server holds the response to (`cmd/tally-reporting/main.go`),
which a run past that hits before the client does. A client timeout under that
would close the connection on a run the server had not given up on, which the
faulted month's syncs and a first sync with no bound to start from are long
enough to be; the handler cancels the run on that close and records it failed,
and a failed run does not move the bound the next one starts from. The next sync
would walk a wider window, take longer, and be cut off again, until nothing
completes for the rest of the month and all the operator has is a `sync post
failed` no port-forward explains. `sync post failed` on every iteration is the
reporting-api port-forward gone, which dies the way the VictoriaMetrics one does
and restarts itself no more than that one; the fix is to start it again. `clock
read failed` inside the month is the simulator gone, which the empty `at` and
the `400` below would otherwise make look like a month that had ended.

The body is built with `jq -nc` rather than by splicing `$at` into a JSON
literal. The value crosses an unauthenticated HTTP boundary before it is posted
to an internal endpoint under a bearer token, and `jq` is what keeps whatever
answers on 8091 from writing the rest of the document.

One iteration that reached the Reporting API gets one of four answers:

```json
{"sync_run_id": "...", "stats": {"created": 0, "updated": 0, "deleted": 0}}
```

- The document above, with the counts the sync found. At factor 744 a sync
  corrects the resources whose notifications the collector has consumed and not
  posted yet: its outbox goes out every 5 seconds
  (`TALLY_OSC_FLUSH_INTERVAL_S`, 62 virtual minutes at this factor). Such a
  correction is a `sync.create` dated at the platform's instant, which is the
  instant the collector's own create carries, a `sync.update` dated at the told
  instant, or a `sync.delete` found by absence and dated there as well.
  `0, 0, 0` is what a sync answers when the outbox was empty at its instant.
- `409` with `a sync for this cloud is already running`, while the previous
  iteration's run still holds the cloud. The next iteration waits it out.
- `500` with `the sync run failed`, for a sync that was running when the fake
  API went away. A loop stopped after the run has ended gets the `400` below
  instead.
- `400` with `the request does not match the API contract` and `body.at` in its
  `errors`, on every iteration while `/clock` does not answer: before the run
  listens and after it has ended. The `at` the `jq` derives is the empty string
  there, and the OpenAPI validation of the Reporting API refuses the body
  before the handler sees it.

The told instant is read before the sync posts, so the fake API answers the
listings minutes of virtual time past it. A resource the cloud deleted inside
that gap is found by absence and booked deleted at the told instant, ahead of
the platform's own delete. The difference such a delete leaves in the
comparison is the finding "The first run" records.

The loop stops when `published` reaches `total`: Ctrl-C on the loop, then
`kill %1` on the port-forward it started. What goes into the write-up is the
totals the answers carried: `created`, `updated` and `deleted`.

The month is watched through the control endpoint
([the control endpoint](../openstack-simulator.md#the-control-endpoint)):

```sh
curl -s http://127.0.0.1:8091/clock
```

```json
{"virtual_now":"2026-07-09T14:22:00Z","factor":744,"published":52,"total":15727,"held":0,"holding":false,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
```

`published` reaches `total`, 15727 for seed 1, after about an hour. The run ends
there and the control endpoint stops answering.

The collector's counters, summed over their `event_type` labels, are read off
its own endpoint:

```sh
curl -s http://127.0.0.1:8090/metrics | awk '
  /^tally_collector_consumed_total/ { c += $2 }
  /^tally_collector_skipped_total/ { s += $2 }
  END { print c, s }'
```

It answers `1812 13915` for seed 1, the two counts
[what the collector shows](../openstack-simulator.md#what-the-collector-shows)
states, and `tally_collector_unparseable_total` is 0.
`tally_collector_buffer_depth` has to read 0 before the first engine command: it
is the outbox the collector drains into the Reporting API, and a run over a
month the collector is still delivering leaves the rest of it to `detect-late`.

`make simulator-up` wrote the dev CA to `tally-ca.crt`, which the `curl --cacert`
calls against `/targets` and `/api/v1/query` need:

```sh
curl --cacert tally-ca.crt 'https://vm.tally.127-0-0-1.nip.io:8443/targets'
```

Four jobs are listed
([acceptance drill](../openstack-metrics.md#acceptance-drill)). The
`openstack-db-exporter` job is up while the month publishes and down again
afterwards, because its target is the simulator's inventory endpoint.

The oracle comes from a file-mode run beside the stack run:

```sh
TALLY_SIM_CLOUD=os-sim go run ./cmd/tally-openstack-simulator run \
  --period 2026-07 --seed 1 --out "$DRILL_DIR/clean/month"
```

With no `TALLY_SIM_AMQP_URL`, no `TALLY_SIM_OTLP_URL` and no
`--register-projects` it dials nothing, pushes nothing and registers nothing. It
writes `notifications.jsonl`, `events.jsonl` and `oracle.json`
([the oracle](../openstack-simulator.md#the-oracle),
[file mode and replay](../openstack-simulator.md#file-mode-and-replay)). It runs
from the same checkout the stack's image was built from, because one seed,
period and cloud render the same month byte for byte only within one build, and
`ReadOracle` refuses a document of another format.

The month is metered and rated with:

```sh
go run ./cmd/tally-engine run --period 2026-07
```

The first line is `run <id> completed for 2026-07 with pricing model 2026-03`.
The second is `metered N candidates into N usage records, N rated records and N
project statements`. The third is `warnings recorded in runs.stats: 0 metering,
0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable
fields, 0 unregistered projects`. Where a tick of the hourly CronJob metered the
month first, a `superseded run <id>` line stands between the second and the
third ("What else the run shows" below).

The two unpriced entries are `openstack/image` and `openstack/loadbalancer`.
`runs.stats.unpriced` holds them as `{platform, resource_type, count}`. They are
the types `pricing/2026-03.yaml` does not price: expected, explained, not fixed.
A `counter_source_failed` warning on this month means the VictoriaMetrics
port-forward was down. Restart it and run the month again.

The run is exported twice, once as documents and once as a table:

```sh
go run ./cmd/tally-engine export --run <id> --format json \
  --out "$DRILL_DIR/clean/json"
```

```text
run <id> exported for 2026-07 as json into $DRILL_DIR/clean/json
wrote run.json and N statements
wrote kickbacks.json with 0 kickbacks
```

```sh
jq .stats "$DRILL_DIR/clean/json/run.json"
```

The `stats` object carries `candidates`, `usage_records`, `rated_records`,
`statements` and `unpriced` with its two entries, and no other list. The two
Gardener projects have no usage of their own and get a statement each,
`statement-garden-sim%2Falpha.json` and `statement-garden-sim%2Fbeta.json`. Both
carry the line items of the tenant their shoots run on under `related_costs`,
with the relation type `infrastructure_tenant`:

```sh
jq '.related_costs[] | {relation_type, project_id, total}' \
  "$DRILL_DIR/clean/json/statement-garden-sim%2Falpha.json"
```

```sh
go run ./cmd/tally-engine export --run <id> --format csv \
  --out "$DRILL_DIR/clean/csv"
```

```text
run <id> exported for 2026-07 as csv into $DRILL_DIR/clean/csv
wrote rated.csv with N rated records
wrote kickbacks.csv with 0 kickbacks
```

An `--out` that is not empty is refused with `--out: <dir> is not empty, and an
export does not remove what an earlier one left there`, so each export of this
drill gets a directory of its own.

The export is held against the oracle
([comparing an export](../openstack-simulator.md#comparing-an-export)):

```sh
go run ./cmd/tally-openstack-simulator compare \
  --oracle "$DRILL_DIR/clean/month/oracle.json" \
  --export "$DRILL_DIR/clean/csv" --pricing pricing/2026-03.yaml
```

```text
image: 9 resources are not priced by pricing model 2026-03 and were not compared
loadbalancer: 5 resources are not priced by pricing model 2026-03 and were not compared
the export matches the oracle over N resources
```

That is exit status 0 and the answer the clean month has to give. A verdict of
`N of M resources differ from the oracle` is exit status 1, with `N resources
differ from the oracle` on stderr. An export rated with another model is refused
rather than compared, with a message ending in `which pricing model <version>
does not price: pass the model the run rated with`.

The counter dimensions are outside the comparison, because `increase()`
extrapolates over the edges of its window; the oracle's `traffic` rows are the
intended figure ([the counter](../openstack-simulator.md#the-counter)). No
instance of seed 1 spans the whole period, because every classic instance is
created inside the first hours of the month, so the read-off picks one classic
instance that lives from its create to the period end:

```sh
ID="$(jq -r '.resources[]
  | select(.resource_type == "instance" and .workload == "classic"
           and (.intervals | last | .to) == "2026-08-01T00:00:00Z")
  | .resource_id' "$DRILL_DIR/clean/month/oracle.json" | head -1)"
jq -r --arg id "$ID" '[.traffic[]
  | select(.resource_id == $id) | .egress_bytes] | add / pow(2; 30)' \
  "$DRILL_DIR/clean/month/oracle.json"
LC_ALL=C awk -F, -v id="$ID" '$9 == id && $14 == "egress_gb" { sum += $15 }
  END { printf "%.4f\n", sum }' "$DRILL_DIR/clean/csv/rated.csv"
```

Column 9 of `rated.csv` is `resource_id`, column 14 `dimension` and column 15
`quantity`. `LC_ALL=C` stands in front of the awk because a shell whose locale
writes a decimal comma makes awk read `37.1490` as 37. The first figure is the
sum of the oracle's `egress_bytes` over 2^30, the divisor `bytesPerGibibyte` of
`oracle.go` and of the counter source. The second is the `egress_gb` quantity
the export carries. Both go into the write-up with the gap between them, which
is the extrapolation `increase()` leaves. They are recorded, not compared.

The month is then closed:

```sh
go run ./cmd/tally-engine finalize --period 2026-07 --run <id>
go run ./cmd/tally-engine periods list
go run ./cmd/tally-engine detect-late --period 2026-07
```

`finalize` answers `run <id> finalized, period 2026-07 closed`. `periods list`
prints `2026-07 finalized finalized_run=<id> finalized_at=<instant>`, and a
`2026-08` line the hourly tick created may stand beside it. Running the month
again is refused, with the error opening on `the billing period is finalized`.
`detect-late` answers `run <id> read 2026-07 at <snapshot>` and `no events
arrived later`.

## Reset the databases

The faulted month has to be the clean month's period, seed and cloud, so that
its resource ids are the clean month's and the write-up can name one resource in
both (author's decision of 2026-08-30 in #88). A finalized period refuses a
second `run`, so both databases are emptied between the two months:

```sh
make simulator-down
(
  set -e
  kubectl --context kind-tally -n tally patch cronjob tally-engine \
    -p '{"spec":{"suspend":true}}'
  while :; do
    active="$(kubectl --context kind-tally -n tally get cronjob tally-engine \
      -o jsonpath='{.status.active[*].name}')" \
      || { echo "cluster read failed; not resetting" >&2; exit 1; }
    [ -n "$active" ] || break
    echo "a tick is still there ($active); waiting at $(date -u +%FT%TZ)"
    sleep 10
  done
  TALLY_REPORTING_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_reporting?sslmode=disable' \
    go run ./cmd/tally-reporting-admin migrate-down-to 0 --yes
  TALLY_ENGINE_DB_URL='postgres://tally:tally-dev-password@db.tally.127-0-0-1.nip.io:5432/tally_engine?sslmode=disable' \
    go run ./cmd/tally-engine migrate-down-to 0 --yes
  make migrate
  go run ./cmd/tally-engine pricing import pricing/2026-03.yaml
  kubectl --context kind-tally -n tally patch cronjob tally-engine \
    -p '{"spec":{"suspend":false}}'
)
```

Both rollbacks name their database on the line that runs them rather than taking
it from the export block of "Preparation", which this shell has held open for an
hour by now. `migrate-down-to 0` drops every table of the chain it is pointed at,
`--yes` is the only thing standing in front of that, and a target read out of
sight from a variable that another task may have set is not a target the
operator confirmed.

`make simulator-down` stops the compose stack alone: the cluster keeps running,
and with it the hourly `tally-engine` CronJob of
[`tally-engine.yaml`](../../deploy/kubernetes/base/tally-engine/tally-engine.yaml),
whose schedule is `0 * * * *`. A tick that fires while the chains sit at version
0 dies on a missing `billing_periods` relation, and with `backoffLimit: 0` that
failed Job stands in the history as the kind of failure this drill's rule sends
the operator after. A tick already inside a `run` is worse: goose needs ACCESS
EXCLUSIVE to drop a table and queues behind that transaction, the engine's period
advisory lock does not serialize against goose's migration lock, and the tick
then works on tables that vanish under it. Suspending the CronJob takes both
away, but only for the ticks that have not started: `suspend` stops the
controller from creating Jobs and does nothing to a Job it already created. The
`while` loop is what waits that one out. It reads `.status.active` on the
CronJob, which names the Jobs the controller still counts as running and is the
same list `concurrencyPolicy: Forbid` holds the next tick behind. The controller
writes that list when it creates the Job, so a tick that fired a second before
the reset is in it before a pod of its own exists; a `get pods -l
app.kubernetes.io/name=tally-engine` would answer nothing in that second,
because the label sits on the pod template and the Job object carries none of
it, and the reset would begin under a tick coming up behind it. The list is
empty again once the controller has seen the Job end, whichever way it ended.

The block runs in a subshell under `set -e` so that the check gates rather than
prints: a `patch` that did not land, against the wrong context or an API server
that is not answering, stops the block before the first `migrate-down-to 0
--yes`, and a rollback that failed stops it before `make migrate` builds on half
a schema. The read inside the loop does not lean on that and carries an `||`
with an `exit 1` behind it, because `set -e` says nothing about the commands of
a `while` condition and the status of a pipeline is its last command's: a
`kubectl` that met an API server hiccup and wrote its error to stderr and
nothing to stdout would have read as "no tick" either way, and the rollback
would have run under one. Here the status of the read is what the block acts on,
and a cluster that did not answer stops the reset rather than starting it. The
`suspend:false` is the last step inside that chain, so a block that stopped
leaves the CronJob suspended and the operator puts the schedule back once the
reset is through.

Without `--yes` both rollbacks are refused with `--yes: rolling back drops the
data of every migration above the target`. Each one prints one `rolled back
migration <n>` line per migration it undid. Between the reporting rollback and
`make migrate` the Reporting API pod is unready, because its readiness check
refuses a schema older than the build; it is ready again once the chain is back.

What goes: the clean month's events and the projection built from them, the
registry rows and their relations, the api tokens, and the sync runs whose
`started_at` bounds the instant the next sync is told. On the engine side, the
billing periods, the runs, the usage and rated records, the statements, the
deltas and the pricing models, which is why the import runs again.

What stays: the clean month's series in VictoriaMetrics, which the faulted month
pushes again with the same values, because a switch changes what the bus carries
and never what the cloud did. The exported files under `$DRILL_DIR/clean` stay
as well, and they are the record. So does the `tally_engine` login role, which
initdb creates and no migration owns.

The faulted month is not registered: `SIM_REGISTER_PROJECTS` stays at `false`.
Its tenants therefore appear in `runs.stats.unregistered_projects` as
`{cloud, project_id, resources}`, six of them for seed 1, the six `openstack`
rows a registration would have created, and each is billed standalone. That is
the third warning class the drill triages.

## The faulted month

Before the stack starts, the deduplication counter is read once. The 30-second
query latency offset of VictoriaMetrics applies, so a query run right after a
push can answer empty and is repeated after the offset:

```sh
curl --cacert tally-ca.crt -G \
  'https://vm.tally.127-0-0-1.nip.io:8443/api/v1/query' \
  --data-urlencode 'query=tally_events_deduplicated_total{cloud="os-sim"}'
```

The same query runs again at the hold, and the difference between the two is
what the write-up records. 86 is the figure of seed 1 with `duplicates` alone;
the five-switch month carries fewer copies, 77 in the first run, because the
other switches take away transitions a copy would have followed.

One faulted month carries the five compatible switches (author's decision of
2026-09-03). `pre-existing` excludes `missing-create` and is left out. Each
switch draws from a stream of its own, so the resources one touches do not move
when another is on beside it:

```sh
make simulator-up SIM_PERIOD=2026-07 \
  SIM_FAULTS=missing-create,duplicates,reordering,refused-shapes,held-back
```

The oracle of that month is written with the same list:

```sh
TALLY_SIM_CLOUD=os-sim go run ./cmd/tally-openstack-simulator run \
  --period 2026-07 --seed 1 \
  --faults missing-create,duplicates,reordering,refused-shapes,held-back \
  --out "$DRILL_DIR/faults/month"
```

It writes `held-back.jsonl` beside the three files of the clean month, 84 lines
for seed 1.

The reconciliation loop runs as it did on the clean month, in a second shell,
started right after the URLs print. Its answers now carry `created`, `updated`
and `deleted` above 0: the `sync.create` corrections of the `missing-create`
resources the projection had not seen yet, dated at the period start because the
fake API serves a touched resource's creation clipped there, and the held-back
transitions each sync finds. An instance delete is booked at nova's
`terminated_at`; the other deletes and every update are booked at the instant
the sync was told it runs at. The totals go into the write-up.

The loop changes what the switches show. A `missing-create` resource the loop
reached before its first in-month notification has a history that starts with a
create, so the engine warns nothing about it and `compare` marks no difference
on it; the switch shows in that sync's `created` count instead. The warning and
the mark appear for the touched resources the bus named first.

`GET /clock` reports `holding` true and `held` 84 once the last regular
notification is out, after about an hour. One more iteration of the loop is
allowed to answer at `period_to`, where the `jq` clamps `at`. Then the loop is
stopped with Ctrl-C and its port-forward with `kill %1`, before the first engine
command, so that the run's snapshot holds every correction the loop booked and
the release is the only late arrival.

The collector is read at the hold: `consumed`, `skipped`,
`tally_collector_unparseable_total`, 20 with `refused-shapes` alone and 18 in
the five-switch month of the first run, and the three `instance.*` series under
`skipped`. `tally_collector_buffer_depth` is 0.
The per-switch figures stand in
[the fault switches](../openstack-simulator.md#the-fault-switches). No document
states the combined totals, so the first run of this drill is what records them.

During the hold the month is metered, closed and exported:

```sh
go run ./cmd/tally-engine run --period 2026-07
go run ./cmd/tally-engine finalize --period 2026-07 --run <f>
go run ./cmd/tally-engine export --run <f> --format csv \
  --out "$DRILL_DIR/faults/csv"
go run ./cmd/tally-engine export --run <f> --format json \
  --out "$DRILL_DIR/faults/json"
```

`run` prints `run <f> completed for 2026-07 with pricing model 2026-03`, the
`metered ...` line, and `warnings recorded in runs.stats: N metering, 0 counter,
0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 6
unregistered projects`. The N metering warnings are
`history_starts_without_create`, and they have two sources: a `missing-create`
resource the loop did not create before its first in-month notification, and a
resource whose create was held back and whose delete was on the bus, a CI runner
that lived minutes and that no sync saw alive. The first run saw 25 of the
second kind and none of the first. `runs.stats.metering_warnings` holds them as
`{cloud, resource_type, resource_id, code}`. The 2 unpriced come from the
pricing model and the 6 unregistered from the registration switch. `finalize`
answers `run <f> finalized, period 2026-07 closed`. The two exports print the
lines the clean month's did, and `.stats` of `run.json` now carries
`metering_warnings` and `unregistered_projects` beside `unpriced`.

```sh
go run ./cmd/tally-openstack-simulator compare \
  --oracle "$DRILL_DIR/faults/month/oracle.json" \
  --export "$DRILL_DIR/faults/csv" --pricing pricing/2026-03.yaml
```

This one is expected to differ, so it exits 1 with `N resources differ from the
oracle` on stderr. Above the verdict stands `the month ran with the fault
switches missing-create, duplicates, reordering, refused-shapes, held-back`, the
switches in the order `FaultNames` lists them. The verdict itself is `N of M
resources differ from the oracle`. A difference with no mark is a finding, and
so is one marked by `duplicates`, `reordering` or `refused-shapes` alone: those
three switches move nothing in the export, so they account for no interval
bound. ` (touched by missing-create)` stands only on a resource the loop did not
create before its first in-month notification.

The held share goes out only after that first comparison:

```sh
curl -X POST http://127.0.0.1:8091/release
```

It answers 200 with the clock document as it stood the moment before the
release, so `held` reads 0 and `holding` false in it. Sent before the hold it
answers 409 with `the month is still publishing; release once /clock reports
holding true`. Then `published` reaches `total`, the endpoint stops answering,
and `tally_collector_buffer_depth` is 0 again.

```sh
go run ./cmd/tally-engine detect-late --period 2026-07
go run ./cmd/tally-engine correct --period 2026-07
go run ./cmd/tally-engine finalize --period 2026-07 --run <c>
go run ./cmd/tally-engine export --run <c> --format csv \
  --out "$DRILL_DIR/faults/corrected"
go run ./cmd/tally-engine export --run <c> --format json \
  --out "$DRILL_DIR/faults/notes"
```

`detect-late` prints `run <f> read 2026-07 at <snapshot>`, one
`os-sim/openstack/<type>/<id>: N late events, last received <instant>` line per
released resource, and `book them with tally-engine correct --period 2026-07`.
`correct` prints `run <c> completed as a correction of run <f> for 2026-07 with
pricing model 2026-03`, `metered N candidates into N usage records and N rated
records`, and either `N deltas in K credit notes` or `no deltas: the finalized
numbers of 2026-07 stand`, followed by its own `warnings recorded in runs.stats`
line. `finalize` answers `correction run <c> finalized for 2026-07`. The csv
export writes `wrote rated.csv with N rated records`, `wrote deltas.csv with M
deltas` and `wrote kickbacks.csv with 0 kickback deltas`; the json export writes
`wrote run.json and K credit notes`, `wrote kickbacks.json with 0 kickback
deltas` and one `credit-note-*.json` per affected project.

```sh
go run ./cmd/tally-openstack-simulator compare \
  --oracle "$DRILL_DIR/faults/month/oracle.json" \
  --export "$DRILL_DIR/faults/corrected" --pricing pricing/2026-03.yaml
```

The ` (touched by held-back)` differences are gone here: the release put those
notifications on the bus and the correction books them. What remains is a
` (touched by missing-create)` difference on a resource the loop did not create
first, because no correction closes a gap the bus never carried, and any
difference of the told-instant race above. The first run saw none of the first
kind and one of the second.

The triage:

| Warning code or compare mark | Switch or cause | Where it is read |
| --- | --- | --- |
| `history_starts_without_create` | `held-back`, a held create with a delivered delete | `run` output, `.stats.metering_warnings` |
| `history_starts_without_create` | `missing-create`, on a resource the loop did not create first | `run` output, `.stats.metering_warnings` |
| ` (touched by missing-create)` | `missing-create` | both comparisons |
| ` (touched by held-back)` | `held-back` | the first comparison |
| `tally_events_deduplicated_total` | `duplicates` | the VictoriaMetrics query above |
| `tally_collector_unparseable_total`, three `instance.*` skipped series | `refused-shapes` | `http://127.0.0.1:8090/metrics` |
| none | `reordering` | no warning, no counter, no difference |
| `unpriced` `openstack/image`, `openstack/loadbalancer` | the pricing model, not a switch | `run` output, `run.json`, the two compare lines |
| `unregistered_projects` | the registration switch, not a fault switch | `run` output, `run.json` |
| a delete dated at the told instant, ahead of the platform's own | the loop's told instant against the fake API's clock, not a switch | the comparison |
| `candidate_without_history` | every candidate has an event inside the month | `run` output |
| `counter_source_failed` | only a metrics store that is unreachable | `run` output |
| `counter_identity_not_queryable` | every instance carries the id the query selects on | `run` output |
| `period_not_ended` | `2026-07` had ended | `run` output |
| `attribution_multiple_paths` | at most one attributing relation per project | `run` output |
| `attribution_cycle` | the two registered relations hold no cycle | `run` output |
| `adjustment_kickback_target_not_partner` | the month registers no partner | `run` output |
| `unreadable` | the simulator writes every size as a number | `run` output |

## The checklist

- [ ] The clean month's `run` completed with `runs.stats` carrying no warning
      beyond the two `unpriced` entries.
- [ ] `finalize` closed the period and `periods list` says so.
- [ ] Both exports of the clean month were written.
- [ ] `compare` matched the clean month over every priced resource.
- [ ] Every sync answer of the clean month carried 0, 0, 0.
- [ ] Every warning and every difference of the faulted month carries a switch
      that explains it.
- [ ] The correction's comparison differs only on resources marked
      ` (touched by missing-create)`.

## The first run

The drill ran on 2026-09-03 at commit 77eb5eb of the branch
`implement/issue-51-phase3-drill`, over the simulated month of seed 1 for
`2026-07` on cloud `os-sim` at factor 744. The machine runs macOS with Docker
Desktop, and its shell locale is `de_DE.UTF-8`. The clean month ran from 18:54Z
to 20:11Z, the databases were reset at 20:12Z, and the faulted month ran from
20:13Z to 21:20Z. Every instant below is UTC. `DRILL_DIR` was `/tmp/drill`, the
fixed path the procedure carried at that commit, and the output quoted below
names it.

### Preparation

The cluster took three `make up` calls on a machine that held none of the
overlay's images. The first ended on

```text
kubectl --context kind-tally -n tally rollout status statefulset/timescaledb --timeout=300s
Waiting for 1 pods to be ready...
error: timed out waiting for the condition
```

while the node was still pulling. The second applied both migration chains and
ended on `error: deployment "reporting-api" exceeded its progress deadline`,
with the pod waiting on its image. The images the host Docker already held were
put on the node with `kind load docker-image`, the four it did not hold
(`victoriametrics/vmalert:v1.148.0`, `prom/alertmanager:v0.34.0`,
`victoriametrics/victoria-metrics:v1.148.0` and
`otel/opentelemetry-collector-contrib:0.157.0`) were pulled on the host and
loaded the same way, and the third `make up` came through with every pod Ready.
None of this touches the engine.

A `run` before the pricing import failed with exit status 1:

```text
Error: selecting the pricing model for the period beginning 2026-07-01T00:00:00Z: no pricing model is valid for this period
```

`periods list` printed `2026-07 open` after it. The import answered `imported
pricing model 2026-03 valid from 2026-03-01T00:00:00Z`, and a second import of
the same file `pricing model 2026-03 already imported`.

### The clean month

`make simulator-up` without the period printed the `ERROR: set SIM_PERIOD ...`
line and exited 2. With `SIM_PERIOD=2026-07 SIM_REGISTER_PROJECTS=true` the
stack was up at 18:54:52Z and printed the seven URLs.

The loop posted 75 requests. 60 of them were answered 200, from 18:55:14Z to
19:54:21Z, and 15 were answered 400 after the run had ended, the first at
19:55:21Z. Over the 60 the totals are 42 created, 6 updated and 36 deleted; 35
carried `0, 0, 0` and the other 25 carried a correction. The first, told
2026-07-01T04:31:54Z, created six:

```text
18:55:14 at=2026-07-01T04:31:54Z {"stats":{"created":6,"deleted":0,"updated":0},"sync_run_id":"33fb9384-55c8-495f-a807-adf9bce56636"}
```

Its six `sync.create` events are four volumes, an instance and a floating IP
created between 04:06:21 and 04:23:11 virtual, each dated at the platform's
instant. The collector's own create for each of them arrived three seconds after
the sync, out of the outbox whose `tally_collector_buffer_depth` read 12 at that
moment. No answer was a 409 and none a 500.

`published` reached 15727 between 19:54:21Z and 19:55:21Z, an hour after the
start, and the control endpoint stopped answering there. The collector then read
1812 consumed, 13915 skipped, `tally_collector_unparseable_total` 0 and
`tally_collector_buffer_depth` 0, the two counts the simulator's doc states. The
`openstack-db-exporter` target read `0/1 up` when the loop stopped, on its first
scrape after the run had ended.

The oracle's file-mode run logged `completed` with `published` 0 and `total`
15727, and wrote `notifications.jsonl` with 15727 lines, `events.jsonl` with
1812 and `oracle.json` with 871 resources.

`run` at 20:09:44Z:

```text
run aa48861f-69e1-4e1c-9feb-d46cd3d11506 completed for 2026-07 with pricing model 2026-03
metered 871 candidates into 996 usage records, 3265 rated records and 6 project statements
superseded run bcdab7ba-c615-45d1-bfb1-8047c3d017ac
warnings recorded in runs.stats: 0 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 0 unregistered projects
```

The superseded run is the one the 20:00Z tick of the CronJob had made. The json
export answered:

```text
run aa48861f-69e1-4e1c-9feb-d46cd3d11506 exported for 2026-07 as json into /tmp/drill/clean/json
wrote run.json and 6 statements
wrote kickbacks.json with 0 kickbacks
```

`.stats` of `run.json` carries `unpriced` with `openstack/image` at 9 and
`openstack/loadbalancer` at 5, `candidates` 871, `statements` 6, `rated_records`
3265, `usage_records` 996 and `snapshot_at`, and no other list. The six
statements are four `statement-os-sim%2F<tenant>.json` and the two Gardener
ones, whose `related_costs` read

```text
{"relation_type":"infrastructure_tenant","project_id":"005be5adeef3d87e280d03d9d57c38b4","total":3147.41}
{"relation_type":"infrastructure_tenant","project_id":"e31f9083a7e5ee15071a3bd53cb2bac7","total":661.45}
```

for alpha and for beta. The csv export wrote `rated.csv with 3265 rated records`
and `kickbacks.csv with 0 kickbacks`, and repeating it into the same directory
was refused with `Error: --out: /tmp/drill/clean/csv is not empty, and an export
does not remove what an earlier one left there`.

`compare` exited 1:

```text
instance 8489e5af-80dd-42e6-bb23-94bb7229f70e: the oracle expects [2026-07-17T17:32:17Z, 2026-07-17T18:12:00Z) and the export books [2026-07-17T17:32:17Z, 2026-07-17T18:11:48Z)
image: 9 resources are not priced by pricing model 2026-03 and were not compared
loadbalancer: 5 resources are not priced by pricing model 2026-03 and were not compared
1 of 857 resources differ from the oracle
Error: 1 resources differ from the oracle
```

The one difference is the told-instant race, and it is the finding of this run
("The deviations" below).

The egress read-off picked instance `079faae9-9d39-426f-a963-769cb12aa629`,
classic, with 11 intervals from 2026-07-01T03:17:02Z to the period end. The
oracle's `traffic` rows sum to 746.0670 GiB and the export's `egress_gb`
quantities to 746.0460, a gap of 0.0210 GiB.

`finalize` answered `run aa48861f-69e1-4e1c-9feb-d46cd3d11506 finalized, period
2026-07 closed`, and `periods list` after it:

```text
2026-07 finalized finalized_run=aa48861f-69e1-4e1c-9feb-d46cd3d11506 finalized_at=2026-09-03T20:11:27Z
2026-08 grace
```

A second `run` was refused with `Error: the billing period is finalized: 2026-07
was closed by run aa48861f-69e1-4e1c-9feb-d46cd3d11506, and a finalized period
is changed with tally-engine correct --period 2026-07`. `detect-late` answered
`run aa48861f-69e1-4e1c-9feb-d46cd3d11506 read 2026-07 at
2026-09-03T20:09:45Z` and `no events arrived later`.

Three Jobs of the hourly CronJob ran, all three complete. The 19:00Z one logged
`2026-07 open -> grace` and `2026-08 open -> grace`, the 20:00Z one `2026-07 run
bcdab7ba-c615-45d1-bfb1-8047c3d017ac completed`, and the 21:00Z one, after the
reset, `2026-08 open -> grace`. No Job failed: the drill ran on 2026-09-03,
inside the 72-hour grace window of 2026-08, so no tick tried to bill that month
and the failed-Job state the manifest's header comment describes was not seen.

### The reset

`make simulator-down` removed the three containers, the network, the outbox
volume and `deploy/compose/.env`. The reporting rollback without `--yes` was
refused with `Error: --yes: rolling back drops the data of every migration above
the target`; with it, it printed `rolled back migration 10` down to `rolled back
migration 1`, and the engine rollback `rolled back migration 2` and `rolled back
migration 1`. The Reporting API pod read `0/1` between the rollback and
`make migrate`, which applied migrations 1 to 10 and 1 to 2; the pod read `1/1`
again after it. The import answered `imported pricing model 2026-03 valid from
2026-03-01T00:00:00Z`, and `periods list` `no billing periods`.

`tally_events_deduplicated_total{cloud="os-sim"}` had no series before the
faulted month. The clean month deduplicated nothing, so the counter was never
exposed, and its absence is its zero. The clean month's series stayed in
VictoriaMetrics, 667 `ceilometer_network_outgoing_bytes_total` series over the
month.

### The faulted month

The stack came up at 20:13:01Z with the five switches. The oracle's run logged
`transitions` 15551, `billable` 1752, `stream` 15632 and `held` 84, then
`completed` with `total` 15716, and wrote `notifications.jsonl` with 15632
lines, `events.jsonl` with 1752, `held-back.jsonl` with 84 and an `oracle.json`
whose `faults` names the five switches.

The loop posted 65 syncs, all answered 200, with the totals 69 created, 5
updated and 49 deleted. The first, told 2026-07-01T03:02:28Z, created 29: the 18
`missing-create` resources (5 instances, 8 volumes and 5 floating IPs), each
with a `sync.create` at the period start 2026-07-01T00:00:00Z, and 11
corrections of the collector's lag. Five answers came at `period_to` during the
hold, the first with 2 deleted and the four others with `0, 0, 0`, and the loop
was stopped at 21:17:44Z after them and before the first engine command. Every
`missing-create` resource was therefore created before the engine saw the month.

`/clock` reported `holding` true at 21:13:23Z, with `published` 15632, `total`
15716 and `held` 84. The collector read 1745 consumed, 13869 skipped,
`tally_collector_unparseable_total` 18 and `tally_collector_buffer_depth` 0 at
the hold, with `tally_collector_skipped_total` at 34 for
`instance.create.end`, 35 for `instance.delete.end` and 1 for
`instance.unshelve.end`. `tally_events_deduplicated_total{cloud="os-sim"}` read
77 at the hold and 77 after the release.

Those are the combined totals of the five switches, which no other document
states. 1752 billable events from `missing-create`, plus the 77 duplicate copies
the collector mapped, minus the 84 held ones, make the 1745 consumed; 13799
skipped from `missing-create` plus the 70 versioned twins of `refused-shapes`
make the 13869 skipped. The month ends at 18 unparseable rather than the 20 of
`refused-shapes` alone and at 77 copies rather than the 86 of `duplicates`
alone, because the other switches take away transitions a twin or a copy would
have followed.

`run` at 21:17:44Z:

```text
run 8d336aeb-666e-47b0-b0e6-d887829738d5 completed for 2026-07 with pricing model 2026-03
metered 869 candidates into 927 usage records, 2989 rated records and 6 project statements
warnings recorded in runs.stats: 25 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 6 unregistered projects
```

All 25 metering warnings are `history_starts_without_create` on instances, and
each of them is a CI runner whose `compute.instance.create.end` was held back
and whose delete was on the bus: a history that starts with a delete. 36 of the
84 held notifications are instance creates. None of the 25 came from
`missing-create`, because the loop's first sync had created all 18 of those
resources, and no sync saw a CI runner alive, because each lived under 40
virtual minutes, far less than the 12.4 virtual hours between two syncs. The six
unregistered projects are the month's six tenants, with 324, 16, 444, 15, 16 and
18 resources.

`finalize` answered `run 8d336aeb-666e-47b0-b0e6-d887829738d5 finalized, period
2026-07 closed`. The csv export wrote `rated.csv with 2989 rated records` and
`kickbacks.csv with 0 kickbacks`, the json export `run.json and 6 statements`
and `kickbacks.json with 0 kickbacks`, and `.stats` carries `metering_warnings`
with its 25 entries and `unregistered_projects` with its 6 beside `unpriced`.

The first comparison exited 1:

```text
the month ran with the fault switches missing-create, duplicates, reordering, refused-shapes, held-back
39 of 857 resources differ from the oracle
Error: 39 resources differ from the oracle
```

The 39 by mark: 33 ` (touched by held-back)`, and one each of
` (touched by duplicates, held-back)`,
` (touched by duplicates, refused-shapes, held-back)`,
` (touched by missing-create, held-back)`,
` (touched by refused-shapes, held-back)`,
` (touched by reordering, held-back)` and ` (touched by duplicates)`. By kind:
27 instances `missing from the export`, CI runners whose create was held back;
11 interval ends hours after the oracle's, on volumes and
instances whose delete was held back and which a sync found deleted by absence
at the told instant; and one interval end 31 seconds ahead of the oracle's, the
told-instant race again:

```text
volume bcf4ab00-a379-4d3d-ad5c-adba0f124ebc: the oracle expects [2026-07-09T16:14:58Z, 2026-07-10T10:39:10Z) and the export books [2026-07-09T16:14:58Z, 2026-07-10T10:38:39Z) (and 1 more) (touched by duplicates)
```

Two of the 27 missing instances, `86321e6d-9f5b-4119-b056-d0c89669a281` and
`cf0e36d7-f77a-4b4f-ae2a-d21c51449bca`, carry no metering warning: both their
create and their delete were held back, so the projection held nothing of them
to warn about. No difference carried ` (touched by missing-create)` alone.

The release at 21:18:21Z answered 200 with the clock document as it stood the
moment before it:

```text
{"virtual_now":"2026-08-03T18:05:49Z","factor":744,"published":15632,"total":15716,"held":0,"holding":false,"period_from":"2026-07-01T00:00:00Z","period_to":"2026-08-01T00:00:00Z"}
```

A second release answered 409 with `the held-back notifications were already
released`. A release before the hold was not sent in this run, so the 409 the
procedure names for that case stands as the code states it. The endpoint had
stopped answering by 21:19:21Z, and the collector read 1829 consumed, 13869
skipped, 18 unparseable and buffer depth 0.

`detect-late` at 21:19:56Z printed `run 8d336aeb-666e-47b0-b0e6-d887829738d5
read 2026-07 at 2026-09-03T21:17:47Z`, then 80 resource lines (67 instances, 12
volumes and 1 floating IP, the resources of `held-back.jsonl`) of the form

```text
os-sim/openstack/volume/dc476aff-35dc-4615-a50d-1512329dec6c: 1 late events, last received 2026-09-03T21:18:21Z
```

and `book them with tally-engine correct --period 2026-07`. `correct`:

```text
run 87cfc6b6-3197-4d2d-9831-5977b0688fc3 completed as a correction of run 8d336aeb-666e-47b0-b0e6-d887829738d5 for 2026-07 with pricing model 2026-03
metered 871 candidates into 954 usage records and 3097 rated records
95 deltas in 5 credit notes
warnings recorded in runs.stats: 0 metering, 0 counter, 0 attribution, 0 adjustment, 2 unpriced resource types, 0 unreadable fields, 5 unregistered projects
```

`finalize` answered `correction run 87cfc6b6-3197-4d2d-9831-5977b0688fc3
finalized for 2026-07`, and `periods list` still names the regular run under
`finalized_run`:

```text
2026-07 finalized finalized_run=8d336aeb-666e-47b0-b0e6-d887829738d5 finalized_at=2026-09-03T21:17:48Z
2026-08 grace
```

The csv export wrote `rated.csv with 3097 rated records`, `deltas.csv with 95
deltas` and `kickbacks.csv with 0 kickback deltas`; the json export wrote
`run.json and 5 credit notes`, `kickbacks.json with 0 kickback deltas` and five
`credit-note-os-sim%2F<tenant>.json` files with the totals -0.22, 0.40, 1.04,
-0.07 and -0.01. 82 of the 95 deltas are positive and 13 negative.

The corrected comparison exited 1 with `1 of 857 resources differ from the
oracle`, on the same `volume bcf4ab00-a379-4d3d-ad5c-adba0f124ebc` line as the
first comparison. Every `held-back` difference is gone, and no
` (touched by missing-create)` difference remained, because the loop had created
those resources first.

### The deviations

- `make up` failed twice before the cluster stood, on a rollout timeout and on a
  progress deadline, both times while the node pulled images. The images were
  loaded onto the node and `make up` ran again. "Preparation" states that a
  first `make up` can end this way.
- The reset ran with the hourly CronJob live, twelve minutes after the 20:00Z
  tick had finished run `bcdab7ba-c615-45d1-bfb1-8047c3d017ac`, and the 21:00Z
  tick found the chains applied again. Neither the failed Job nor the rollback
  behind a tick's transaction that "Reset the databases" describes was hit, and
  it was the timing that kept them away. The procedure suspends the CronJob
  across the reset since; this run did not.
- The clean month's sync answers did not all carry `0, 0, 0`: 25 of the 60
  carried a correction, 42 created, 6 updated and 36 deleted over the run. The
  collector posts its outbox every 5 seconds (`TALLY_OSC_FLUSH_INTERVAL_S`,
  default 5, `internal/providers/openstack/config.go`), which is 62 virtual
  minutes at factor 744, so a sync lists resources the bus has carried and the
  projection does not hold yet. The fold merges a `sync.create` at the same
  instant as the real create, a `sync.update` with the same values and a
  `sync.delete` behind the real delete, so none of the 25 moved a billed figure.
  "The clean month" states what a sync finds at this factor, and checklist item
  5 stays open.
- The 15 iterations after the run had ended were answered 400, not the 500 the
  procedure named for a loop stopped too late. With `/clock` gone the `jq`
  derives an empty `at`, and the OpenAPI validation of the Reporting API refuses
  the body before the handler sees it:

  ```text
  19:55:21 at= {"type":"urn:tally:error:validation","title":"Validation failed","status":400,"detail":"the request does not match the API contract","errors":[{"loc":"body.at","msg":"string doesn't match the format \"date-time\" (string doesn't match pattern \"^[0-9]{4}-(0[1-9]|10|11|12)-(0[1-9]|[12][0-9]|3[01])T([0-1][0-9]|2[0-3]):[0-5][0-9]:([0-5][0-9]|60)(\\.[0-9]+)?(Z|(\\+|-)[0-9]{2}:[0-9]{2})$\")"}]}
  ```

  The 500 is the answer of a sync that was running when the fake API went away.
  The procedure names both.
- The fault-switch line prints the switches in the order `FaultNames` lists
  them, `missing-create, duplicates, reordering, refused-shapes, held-back`, not
  alphabetically. The procedure carries that order.
- All 25 metering warnings came from `held-back` and none from `missing-create`,
  and no difference carried ` (touched by missing-create)` alone, because the
  loop's first sync had created the 18 touched resources at the period start.
  The procedure names both sources of the warning.
- The awk of the egress read-off printed `744,0000` on this machine, because the
  `de_DE.UTF-8` locale made it read `37.1490` as 37 and print a decimal comma.
  With `LC_ALL=C` in front of it the sum is `746.0460`. The procedure carries
  `LC_ALL=C`.
- One difference of each comparison is the told-instant race, and no switch
  accounts for it. It is the finding of this run, and the issue the author files
  is about it.

The clean month's difference is instance
`8489e5af-80dd-42e6-bb23-94bb7229f70e`, a CI runner that lived 40 minutes. The
reporting database holds the collector's `compute.instance.create.end` at
17:32:17, a `sync.delete` at 18:11:48 from the sync told that instant (received
at 19:27:18Z), and the collector's `compute.instance.delete.end` at 18:12:00
(received two seconds later). The loop reads `/clock` and then posts, and one
wall second is 12.4 virtual minutes, so the fake API answered the sync's
listings minutes past the told instant, when the instance was already deleted.
Its `terminated_at` lies after the told instant and therefore outside the
deleted-servers window, so the sync found the instance by absence and booked its
delete at the told instant, 12 seconds ahead of the platform's own. The fold
closes the interval at the first delete, so the export bills 12 seconds less
than the oracle. That is the poll-time dating of a delete found by absence, the
concept's accepted approximation, turned by the factor into a delete ahead of
the platform's instant. In the faulted month the same race hit volume
`bcf4ab00-a379-4d3d-ad5c-adba0f124ebc` 31 seconds early, under a
` (touched by duplicates)` mark that does not account for it, because
`duplicates` moves nothing in the export.

### The checklist after this run

- [x] The clean month's `run` completed with `runs.stats` carrying no warning
      beyond the two `unpriced` entries.
- [x] `finalize` closed the period and `periods list` says so.
- [x] Both exports of the clean month were written.
- [ ] `compare` matched the clean month over every priced resource. 1 of 857
      resources differs: the told-instant race on instance
      `8489e5af-80dd-42e6-bb23-94bb7229f70e`.
- [ ] Every sync answer of the clean month carried 0, 0, 0. 25 of the 60
      answers carried a correction: the collector's batch delivery lag.
- [ ] Every warning and every difference of the faulted month carries a switch
      that explains it. 38 of the 39 differences and all 25 warnings do; volume
      `bcf4ab00-a379-4d3d-ad5c-adba0f124ebc` carries `duplicates`, which does
      not.
- [ ] The correction's comparison differs only on resources marked
      ` (touched by missing-create)`. Its one remaining difference is that same
      race, and no `missing-create` mark remained.

Two mechanisms stand behind the four open items: the collector's flush interval
against the loop's cadence, and the told instant lying minutes before the fake
API's clock at factor 744. Both are properties of the drill's compression and of
the poll-time dating of a delete found by absence, not of the metering. The
told-instant race is the finding this run records for the issue the author
files.

## What else the run shows

The `tally-engine` CronJob of
[`tally-engine.yaml`](../../deploy/kubernetes/base/tally-engine/tally-engine.yaml)
runs `tick` every hour. A tick walks from the earliest stored billing period, or
from the month before now when none is stored. It moves each month it reaches
from `open` to `grace`, bills a month whose grace window of 72 hours has passed
and that carries no complete regular run, and finalizes nothing, because
`TALLY_ENGINE_AUTO_FINALIZE` is `false`. A tick that ran `2026-07` before the
operator did shows up in the operator's `run` output as `superseded run <id>`. A
tick after the operator's run leaves the month alone, because the month carries
a complete run. The failed Jobs of `2026-08` from before the pricing import,
each ending in `no pricing model is valid for this period`, are the state the
manifest's header comment describes. They are read with:

```sh
kubectl --context kind-tally -n tally get jobs
kubectl --context kind-tally -n tally logs job/<name>
```

A tick that holds the period's advisory lock makes an operator's `run` fail with
an error opening on `another run of this period is in progress`. Retry it a
minute later.

`make simulator-down` drops the stack, its outbox and `deploy/compose/.env`, and
it leaves the reporting database as it is. `make down` deletes the cluster and
its volumes, and that is the way to a clean cluster.

The drill exports dev credentials into a shell. The clean month's admin api
token is not one of them by the end: the reset drops `api_tokens`
([`0001_init.sql`](../../migrations/reporting/0001_init.sql)), so
`revoke-api-token` against that id fails with `api_tokens <id>: not found`. What
outlives the drill is the ingest credential the faulted month's `simulator-up`
issued. `tally-reporting-admin revoke-ingest-credential <id>` ends that one, on
the id that `simulator-up` output carries, and `make down` ends it by taking the
cluster with it. `simulator-down` revokes nothing.

Two costs of the run, both accepted:

- The two months' series stay in the dev store for the retention
  VictoriaMetrics runs with, and the exported files stay under `$DRILL_DIR` on
  the machine. `make down` is the shorter way out of the first.
- The drill books usage under a cloud and tenants that do not exist. It is for
  dev clusters alone: run against a store any invoice is derived from, it puts
  invented usage into the billing record.

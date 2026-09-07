---
title: Migrate both databases
description: Apply the reporting and the engine migration chains with their own CLIs, roll one chain back, and read which migrations each database carries.
quadrant: how-to
audience: operator
---

# Migrate both databases

This guide brings both databases to the schema their binaries expect and rolls
one chain back again. `tally-reporting-admin` owns the reporting chain,
`tally-engine` owns the engine chain, and no other subcommand of either binary
runs DDL.

## Before you start

- Both databases reachable from the machine you run the CLIs on, with a
  connection string for a role that may run DDL on each.
- `tally-reporting-admin` and `tally-engine` at the version the deployment
  runs, with their subcommands on the
  [reporting admin CLI](/reference/command-line/tally-reporting-admin) and the
  [engine CLI](/reference/command-line/tally-engine) pages.
- `kubectl` against the cluster that runs the hourly `tally-engine` CronJob,
  for the rollback below.
- The [engine settings](/reference/configuration/tally-engine) page, which
  names `TALLY_ENGINE_DB_URL`, beside the
  [Reporting API settings](/reference/configuration/tally-reporting) page,
  which names `TALLY_REPORTING_DB_URL`.
- `pg_dump` and `psql` at the version of the server, and somewhere to put a
  dump, for the rollback below. Neither CLI copies anything before it drops,
  and a rollback drops what every migration above the target created.
- A `~/.pgpass` at mode `0600` with one line per database:
  `db.internal:5432:tally_engine:tally:<password>` and the same for
  `tally_reporting`. The `psql` and `pg_dump` calls below connect with a URI
  that carries no password and read it from there: a password in the URI is an
  argument, and arguments stand in `ps` output and in the world-readable
  `/proc/<pid>/cmdline` for the life of the process, which for a dump of a
  billing database is minutes. The CLIs of this repository are unaffected —
  they read their URL from the environment.

## Apply both chains

1. Point the admin CLI at the reporting database and apply the reporting
   chain:

   ```sh
   export TALLY_REPORTING_DB_URL='postgres://tally:password@db.internal:5432/tally_reporting?sslmode=require'
   tally-reporting-admin migrate
   ```

   ```text
   applied migration 9
   applied migration 10
   ```

   A chain already at its head answers `nothing to apply`.

   The chain goes before the image that expects it. Migration 9 takes the
   reporting database to the version a Reporting API built from this tree
   requires, and that build's readiness check refuses a database below it, so
   rolling the image first leaves the new pods out of rotation until this
   command has run.

2. Point the engine at its own database and apply the engine chain:

   ```sh
   export TALLY_ENGINE_DB_URL='postgres://tally:password@db.internal:5432/tally_engine?sslmode=require'
   tally-engine migrate
   ```

   ```text
   applied migration 2
   ```

## When an apply fails

1. Migration 9 builds `idx_events_received` one chunk at a time and therefore
   outside a transaction, so a build that is interrupted leaves the chunks it
   finished indexed with the migration unrecorded, and the rerun fails on the
   index that is already there. Drop it and apply the chain again:

   ```sh
   psql 'postgres://tally@db.internal:5432/tally_reporting?sslmode=require' \
     -c 'DROP INDEX IF EXISTS idx_events_received;'
   tally-reporting-admin migrate
   ```

2. Migration 10 aborts by design in two ways. It counts the rows under the
   reserved `meta` and `partner` platforms first and refuses a database that
   holds any, naming them; which real cloud such a row belonged to is the
   operator's to decide, so the chain waits until they are corrected. It then
   adds its constraints under `SET LOCAL lock_timeout = '3s'`, and a failure
   naming the lock is a run that queued behind a reader of `projects` or
   `current_resources` — a metering run holds one for its whole length. That
   one is rerun once the reader is gone, off the hour the CronJob fires on.

## Roll a chain back

A rollback runs the `Down` direction of every migration above the target, and
those directions drop what their `Up` created. `tally-engine migrate-down-to 1`
runs the down of `migrations/engine/0002_adjustment_records.sql`, which is
`DROP TABLE adjustment_records`: the applied pricing adjustments of every run,
one row per adjustment with its rate, the base it was applied to, the signed
amount it produced and the partner a kickback is paid to. The trigger that
keeps a finalized run immutable is a row trigger and does not fire on DDL, so
the drop goes through. The `migrate` that follows re-creates the table empty and
`migrate-status` then reports the chain healthy, so nothing after the rollback
says the rows are gone. The dump taken first is what they come back from.

1. Take a restorable copy of the database first. `-Fc` writes the custom
   format `pg_restore` reads:

   ```sh
   export TALLY_ENGINE_DB_URL='postgres://tally:password@db.internal:5432/tally_engine?sslmode=require'
   (umask 077; pg_dump -Fc \
     'postgres://tally@db.internal:5432/tally_engine?sslmode=require' \
     > engine-pre-rollback.dump)
   ```

   A bare redirection creates the file under the umask the shell carries,
   `0022` on a stock installation, which is mode `0644` on a complete copy of a
   billing database: every adjustment with its rate and the partner a kickback
   is paid to, and on the reporting side every ingested event. The `umask 077`
   makes it `0600` instead, and the subshell keeps that umask off the shell you
   ran the command in. Nothing below removes the file.

2. Suspend the hourly `tally-engine` CronJob, wait out a tick that is already
   running, roll the chain back, apply it again and put the schedule back.
   `suspend` keeps the controller from creating Jobs and does nothing to a Job
   it already created, and `.status.active` names the Jobs the controller still
   counts as running:

   ```sh
   (
     set -e
     export TALLY_ENGINE_DB_URL='postgres://tally:password@db.internal:5432/tally_engine?sslmode=require'
     kubectl -n tally patch cronjob tally-engine -p '{"spec":{"suspend":true}}'
     empty=0
     while [ "$empty" -lt 2 ]; do
       active="$(kubectl -n tally get cronjob tally-engine \
         -o jsonpath='{.status.active[*].name}')" \
         || { echo "cluster read failed; not rolling back" >&2; exit 1; }
       if [ -n "$active" ]; then
         empty=0
         echo "a tick is still there ($active); waiting at $(date -u +%FT%TZ)"
       else
         empty=$((empty + 1))
       fi
       sleep 10
     done
     tally-engine migrate-down-to 1 --yes
     tally-engine migrate
     kubectl -n tally patch cronjob tally-engine -p '{"spec":{"suspend":false}}'
   )
   ```

   ```text
   cronjob.batch/tally-engine patched
   a tick is still there (tally-engine-29354280); waiting at 2026-07-09T14:22:00Z
   rolled back migration 2
   applied migration 2
   cronjob.batch/tally-engine patched
   ```

   The subshell exports the database it works on rather than taking it from the
   shell it runs in, and it runs under `set -e`, so a `patch` that did not land
   stops it before the first `migrate-down-to`. The read inside the loop
   carries an `||` with an `exit 1` of its own, which `set -e` does not give
   the commands of a `while` condition. `suspend:false` is the last step in the
   chain, so a block that stopped leaves the CronJob suspended and the schedule
   goes back by hand.

   The loop wants two empty reads rather than one, because the controller
   writes `.status.active` after it has created the Job rather than together
   with it: a tick that fires in the same second as the `patch` has a pod
   starting while its status update has not landed yet, and a single empty read
   would send the rollback into a run that is writing.

3. The reporting chain takes the same argument and the same flag, and it needs
   a different set of writers stopped. Its writer is not a CronJob: the
   Reporting API serves the collectors' ingest path and
   `POST /internal/sync/{cloud}` continuously, and its readiness check takes
   the pod out of the Service only after the schema has already gone backwards,
   so the writes in flight until then fail against objects that are being
   dropped. An event acknowledged on that path is gone from the collector's
   outbox, which was the only other copy it had. Suspend the engine's CronJob
   as well: the down of `migrations/reporting/0008_engine_reader_role.sql`
   revokes the `SELECT` the engine reads this database through, so any target
   below 8 leaves an unsuspended tick failing with `permission denied for table
   events` until the chain is applied again.

   ```sh
   (
     set -e
     export TALLY_REPORTING_DB_URL='postgres://tally:password@db.internal:5432/tally_reporting?sslmode=require'
     (umask 077; pg_dump -Fc \
       'postgres://tally@db.internal:5432/tally_reporting?sslmode=require' \
       > reporting-pre-rollback.dump)
     replicas="$(kubectl -n tally get deployment/reporting-api \
       -o jsonpath='{.spec.replicas}')"
     restore() {
       kubectl -n tally scale deployment/reporting-api --replicas="$replicas" \
         || echo "scaling back to $replicas failed; do it by hand" >&2
       kubectl -n tally patch cronjob tally-engine -p '{"spec":{"suspend":false}}' \
         || echo "unsuspending the CronJob failed; do it by hand" >&2
     }
     trap restore EXIT
     kubectl -n tally patch cronjob tally-engine -p '{"spec":{"suspend":true}}'
     empty=0
     while [ "$empty" -lt 2 ]; do
       active="$(kubectl -n tally get cronjob tally-engine \
         -o jsonpath='{.status.active[*].name}')" \
         || { echo "cluster read failed; not rolling back" >&2; exit 1; }
       if [ -n "$active" ]; then
         empty=0
         echo "a tick is still there ($active); waiting at $(date -u +%FT%TZ)"
       else
         empty=$((empty + 1))
       fi
       sleep 10
     done
     kubectl -n tally scale deployment/reporting-api --replicas=0
     kubectl -n tally wait --for=delete pod \
       -l app.kubernetes.io/name=reporting-api --timeout=2m
     tally-reporting-admin migrate-down-to 9 --yes
     tally-reporting-admin migrate
   )
   ```

   ```text
   cronjob.batch/tally-engine patched
   deployment.apps/reporting-api scaled
   pod/reporting-api-7d9f4c8b5c-2xk9v condition met
   rolled back migration 10
   applied migration 10
   deployment.apps/reporting-api scaled
   cronjob.batch/tally-engine patched
   ```

   The replica count is read before the scale to zero rather than written into
   the block, so the Deployment comes back at the count it ran at, and the
   `trap` is what puts it and the schedule back. Every path out of the block
   runs it, including the one that matters most: the down of migration 10 drops
   its constraints under the same three-second `lock_timeout` the previous
   section describes, so an abort there is routine rather than exceptional, and
   without the trap it would leave ingest refused, the query API down and
   metering stopped until somebody noticed. Each restoring command carries its
   own message, because a `set -e` that fired once would otherwise stop the
   second one from being tried.

   The block waits for the pod to be gone rather than for `rollout status`,
   which reads a Deployment's replica counts: the ReplicaSet controller stops
   counting a pod the moment it carries a deletion timestamp, so a pod that has
   been sent `SIGTERM` and is finishing an ingest batch is already uncounted
   and `rollout status` returns while it still writes. The Deployment sets no
   `terminationGracePeriodSeconds`, so it has the default 30 seconds to finish,
   and the DDL below would run into the transaction the scale to zero exists to
   let end. `wait --for=delete` returns once the pod object is gone, which is
   after the process has exited.

   Ingest is refused for the length of the block: a collector holds its events
   in its own outbox and delivers them when the API answers again.

4. `migrate-down-to` without `--yes` is refused before the database is opened,
   and nothing is dropped. The refusal goes to stderr and the command exits 1:

   ```sh
   tally-engine migrate-down-to 1
   ```

   ```text
   --yes: rolling back drops the data of every migration above the target
   ```

## Check the result

1. Ask each chain what its database carries. Every migration of the chain gets
   a line, `applied` or `pending`:

   ```sh
   tally-reporting-admin migrate-status | tail -2
   tally-engine migrate-status
   ```

   ```text
   migration 9 applied
   migration 10 applied
   migration 1 applied
   migration 2 applied
   ```

2. A rollback prints one `rolled back migration <n>` line per migration it
   undid, and `nothing to roll back` where the chain already sits at the
   target.

3. A Reporting API left running across a reporting rollback goes unready
   rather than serving the older schema: its readiness check refuses a database
   below the version its build expects. That is the state the scale to zero and
   the wait above avoid, because readiness takes the pod out of the Service
   only after the DDL has run. Once the chain is back and the Deployment is
   scaled up again, the pod is ready:

   ```sh
   kubectl -n tally get pods -l app.kubernetes.io/name=reporting-api
   ```

   ```text
   NAME                             READY   STATUS    RESTARTS   AGE
   reporting-api-7d9f4c8b5c-2xk9v   1/1     Running   0          3h12m
   ```

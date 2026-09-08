---
title: Tear down your local Tally
description: Stop the simulator stack, delete the kind cluster, and remove the files and the shell variables the lessons wrote, after the last lesson you want to run.
quadrant: tutorial
audience: all
---
<!-- Shown output captured on 2026-09-07 from commit 789d782 with kind v0.32.0, kubectl v1.36.1, Docker Desktop 4.86.0, Go 1.27.1 on macOS 15.7.4. -->

# Tear down your local Tally

Do this lesson last. It belongs to neither track: it ends the cluster and the
compose stack every other lesson works on, and nothing here can be undone. The
billing track continues from the state
[Watch the month in Grafana](/tutorials/watch-the-month-in-grafana) leaves
behind, so a reader going on to it, through
[Discount a customer group](/tutorials/discount-a-customer-group) and the four
lessons after it, runs this one after that track's last lesson,
[Book the late events as a credit note](/tutorials/book-the-late-events-as-a-credit-note).
Tearing down between the two tracks leaves the billing track nothing to
continue from, and the way back is the core track again from
[Set up your local Tally](/tutorials/set-up-your-local-tally).

In this lesson you take the machine back to where lesson 1 started. The
simulator stack goes with its broker and its outbox, and the kind cluster goes
with every database in it: the month of lesson 2, the run of lesson 3 and the
project registry. Every token and credential the lessons issued goes with those
databases, because that is where they live. You remove the CA file
`tally-ca.crt` at the repository root and the export directory
`~/tally-tutorial` by hand.

What stays is the four `tally-*:dev` images in Docker, the Go build and module
caches under your home directory, and the clone.

This lesson takes about 5 minutes.

## Before you start

- Every lesson you still want to run, done. This one leaves no environment for
  a later lesson to work on.
- The state lesson 1 leaves, or the state any later lesson leaves: a cluster
  from `make up`, and whatever the later lessons added to it. A step that finds
  nothing to remove says so and changes nothing.
- A shell at the repository root.
- The shell that holds the port-forward lesson 3 started in the background, so
  that it can be stopped there. Any shell ends it with:

  ```sh
  pkill -f 'port-forward svc/victoriametrics'
  ```

  It prints nothing and ends the background job lesson 3 started, and on a
  machine where lesson 3 never ran it ends nothing and exits 1, which is fine.

## Stop the simulator stack

1. Remove the three containers of the stack, their network, the outbox volume
   and `deploy/compose/.env`:

   ```sh
   make simulator-down
   ```

   ```text
   docker compose -f deploy/compose/compose.yaml down --volumes
    Container tally-simulator-simulator-1 Stopping 
    Container tally-simulator-collector-1 Stopping 
    Container tally-simulator-simulator-1 Stopped 
    Container tally-simulator-simulator-1 Removing 
    Container tally-simulator-simulator-1 Removed 
    Container tally-simulator-collector-1 Stopped 
    Container tally-simulator-collector-1 Removing 
    Container tally-simulator-collector-1 Removed 
    Container tally-simulator-rabbitmq-1 Stopping 
    Container tally-simulator-rabbitmq-1 Stopped 
    Container tally-simulator-rabbitmq-1 Removing 
    Container tally-simulator-rabbitmq-1 Removed 
    Network tally-simulator_default Removing 
    Volume tally-simulator_outbox Removing 
    Volume tally-simulator_outbox Removed 
    Network tally-simulator_default Removed 
   rm -f deploy/compose/.env
   ```

   The names have to match: the three containers, the network
   `tally-simulator_default` and the volume `tally-simulator_outbox`. The order
   the `Stopping` and `Stopped` lines arrive in is your own. With the simulator
   container the 84 notifications it was holding are discarded: no release was
   sent, and the held share exists nowhere else. The last line removes
   `deploy/compose/.env`, the file with the ingest credential, and the
   credential itself stays in the reporting database until the cluster goes in
   the next step. On a machine where lesson 2 never ran, compose warns about
   unset `TALLY_SIM_` variables, because no `deploy/compose/.env` exists, and
   removes nothing, which is fine.

## Delete the cluster

1. Delete the kind cluster:

   ```sh
   make down
   ```

   ```text
   Deleting cluster "tally" ...
   Deleted nodes: ["tally-control-plane"]
   ```

   Both lines have to match. With the node go both databases and what they
   hold: the month, the run, the project registry, and every token and
   credential the lessons issued. The series lesson 2 pushed and the state the
   dashboards of lesson 4 read go with it as well.

2. Check that no cluster is left:

   ```sh
   kind get clusters
   ```

   ```text
   No kind clusters found.
   ```

   The line has to match. It is the state lesson 1 starts from. A second
   `make down` prints that same line again, from the check it makes before it
   deletes anything, and then `kind cluster tally does not exist`. It changes
   nothing.

## Remove what the lessons wrote

1. Remove the CA file, the export directory, the seven variables of the core
   track and the eleven of the billing track:

   ```sh
   rm tally-ca.crt
   rm -r ~/tally-tutorial
   unset TALLY_REPORTING_DB_URL TALLY_API_TOKEN TALLY_ENGINE_DB_URL TALLY_ENGINE_REPORTING_DB_URL TALLY_ENGINE_COUNTER_SOURCES TALLY_ENGINE_VM_URL RUN_ID ACME_1_ID ACME_2_ID ACME_3_ID ACME_ID CI_ID PARTNER_ID ALPHA_TENANT_ID BETA_TENANT_ID ALPHA_ID BETA_ID CORRECTION_ID
   ```

   The three commands print nothing. `tally-ca.crt` is stale from here on: the
   next `make up` creates a new certificate authority, and `make -s ca` writes
   the file again for it. `~/tally-tutorial` held the export of lesson 3, the
   six statements with `run.json` and `kickbacks.json` beside them. After the
   billing track it also held that track's seven export directories,
   `2026-07-group`, `2026-07-reseller`, `2026-07-attributed`, `2026-07-final`,
   `2026-07-final-again`, `2026-07-final-csv` and `2026-07-notes`. The seven
   variables are the shell state lessons 1 to 3 built, and `unset` leaves this
   shell without them. The eleven after `RUN_ID` are the billing track's, and
   `unset` skips a variable that is not set without a word, so the line is the
   same on a machine where the billing track never ran.
   `rm: tally-ca.crt: No such file or directory` means the file was already
   gone, which is fine.

## What stays

1. List the images the track built:

   ```sh
   docker image ls 'tally-*'
   ```

   ```text
   IMAGE                           ID             DISK USAGE   CONTENT SIZE   EXTRA
   tally-engine:dev                5411e0bd59f9       15.3MB             0B        
   tally-openstack-collector:dev   d879c5ca78ff       15.9MB             0B        
   tally-openstack-simulator:dev   6527cde4f567       17.6MB             0B        
   tally-reporting:dev             cf2be4a2bd30       19.3MB             0B        
   ```

   The four `:dev` images are what `make up` built, and their names have to
   match. The ids, the sizes and the column layout are Docker's own. Beside the
   images stay Docker's build cache from the image builds, which
   `docker builder prune` removes, the Go build and module caches under your
   home directory, and the clone.

2. Remove the images if you want the disk back:

   ```sh
   docker image rm tally-reporting:dev tally-engine:dev tally-openstack-collector:dev tally-openstack-simulator:dev
   ```

   This is your choice and not a step of the track: the four images cost about
   70 MB, and the next `make up` builds them again.

## What you learned

- No command of Tally deletes an event. The month is gone because the database
  went with the cluster:
  [why the history is append-only](/explanation/events-as-the-source-of-truth#why-the-history-is-append-only).
- The collector's outbox is a volume of the compose stack, and
  `make simulator-down` drops it with the stack:
  [the outbox on disk](/explanation/how-the-collector-consumes-a-bus#the-outbox-on-disk).
- The four images are four of the six binaries, built by `make up` and rebuilt
  by the next one:
  [the six binaries](/explanation/architecture-and-the-provider-pattern#the-six-binaries).
- A second run of this track renders the same month, byte for byte, because the
  seed decides what the simulated world does:
  [determinism](/explanation/the-simulated-openstack-world#determinism).

## Where to go next

[Set up your local Tally](/tutorials/set-up-your-local-tally) runs the track
again from an empty machine, which is what this machine is now: no kind
cluster, no compose stack, no `tally-ca.crt`, no `~/tally-tutorial`, and the
clone.

For a cloud of your own, the [how-to guides](/how-to/) take these commands task
by task, and the [reference](/reference/) is where the flags, the settings and
the formats are written down.

---
title: Replay a recorded month
description: Write a generated month to files and publish one of those files onto a broker again, at the pace the file asks for.
quadrant: how-to
audience: contributor
---

# Replay a recorded month

This guide writes a month to disk with `run --out` and puts one of the written
files back on a broker with `replay`, which needs neither the generator nor the
seed the month came from. What the month holds is in
[the simulated OpenStack world](/explanation/the-simulated-openstack-world).

## Before you start

- A broker to publish onto, and a collector consuming from it. The compose stack
  of [run a simulated month](/how-to/simulator/run-a-month) is one, with its
  broker on `amqp://guest:guest@127.0.0.1:5672/`.
- `tally-openstack-simulator` on your machine, or a shell at the repository root
  for `go run ./cmd/tally-openstack-simulator`.
- A directory for the month's files. Every run empties it of the four files an
  earlier run left there.
- The [simulator command line](/reference/command-line/tally-openstack-simulator)
  page, which describes each of
  [the files](/reference/command-line/tally-openstack-simulator#files) a run
  writes.
- The [simulator settings](/reference/configuration/tally-openstack-simulator)
  page, which states that `run` takes `TALLY_SIM_CLOUD` and `replay` takes
  `TALLY_SIM_AMQP_URL`.

## Write the month to files

1. Render the month into a directory. With no broker configured the run writes
   the files and publishes nothing, and it writes the whole month before it
   publishes anything, so a run interrupted halfway still leaves a complete
   month on disk:

   ```sh
   TALLY_SIM_CLOUD=os-sim tally-openstack-simulator run \
     --period 2026-07 --seed 1 --out /tmp/m
   ```

2. Take the file to replay out of the directory. `notifications.jsonl` holds one
   line per notification, `events.jsonl` one canonical event per billable
   notification, `oracle.json` the generator's statement of the month, and
   `held-back.jsonl` what the `held-back` switch kept off the bus:

   ```sh
   ls /tmp/m
   ```

   ```text
   events.jsonl
   notifications.jsonl
   oracle.json
   ```

## Replay a file

1. Publish a captured file. The virtual clock starts at the first line's
   timestamp, and the message ids are the recorded ones:

   ```sh
   TALLY_SIM_AMQP_URL='amqp://guest:guest@127.0.0.1:5672/' \
     tally-openstack-simulator replay --in /tmp/m/notifications.jsonl --factor 744
   ```

   A line whose timestamp lies before the previous one is published at once,
   which is what lets a file that is not perfectly sorted replay whole.

2. Replay a held-back file at factor 0, which puts it on the bus at once:

   ```sh
   TALLY_SIM_AMQP_URL='amqp://guest:guest@127.0.0.1:5672/' \
     tally-openstack-simulator replay --in /tmp/m/held-back.jsonl --factor 0
   ```

   The factor is what matters there. The replay clock starts at the first line's
   timestamp and the held instants are spread over the whole month, so a replay
   at 744 trickles the file out over an hour.

3. Give a `pre-existing` month the same treatment and read its start from the
   file rather than from the period. Such a month replays from its earliest
   pre-month create, because the clock starts at the first line, and the burst a
   run publishes ahead of the month is paced by the lead the switch drew.

## Check the result

1. Replay the same file a second time. The recorded message ids are the ones the
   first replay carried, so nothing new is stored: the batch is deduplicated at
   ingest and `tally_events_deduplicated_total` moves instead of
   `tally_events_ingested_total`.

2. Replay an empty file, one holding a line that is not JSON, and one holding a
   body without a timestamp. `ReadStream` reads the file before the first message
   goes out, so each of the three ends the replay with exit status 1 and nothing
   published:

   ```sh
   : > /tmp/empty.jsonl
   TALLY_SIM_AMQP_URL='amqp://guest:guest@127.0.0.1:5672/' \
     tally-openstack-simulator replay --in /tmp/empty.jsonl --factor 0
   echo "exit $?"
   ```

   ```text
   exit 1
   ```

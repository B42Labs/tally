---
title: Install the collector from the Debian package
description: Install, configure and run one collector as a systemd service on a Debian or Ubuntu control node, and upgrade, remove or purge it again.
quadrant: how-to
audience: operator
---

# Install the collector from the Debian package

This guide puts one `tally-openstack-collector` on an OpenStack control node as
a service systemd starts, restarts and logs. At the end the collector runs as
the unprivileged `tally` user, reads its credentials from files only that user
may read, and buffers into a directory that survives an upgrade. Which
notifications it then consumes, and what the cloud has to publish for it, is
[connect the collector to an OpenStack cloud](/how-to/openstack/connect-the-collector).

## Before you start

- A Debian 12 or 13, or Ubuntu 24.04 or 26.04, host on `amd64`, with `sudo`.
- The package file. Build it from a checkout with `make deb`, which writes
  `dist/tally-openstack-collector_<version>_amd64.deb`, and copy that file to
  the host. The build needs Go and nothing else, and it runs on macOS as well
  as on Linux.
- The broker's AMQP URL, the cloud name and the base URL of the Reporting API.
- The ingest credential this cloud reports under.
  [Issue and revoke credentials](/how-to/openstack/issue-and-revoke-credentials)
  has those steps.
- The [collector settings](/reference/configuration/tally-openstack-collector)
  page, which lists every variable with its default.

## Install the package

1. Install the file. The leading `./` is what tells `apt` this is a path and
   not a package name:

   ```sh
   sudo apt install ./tally-openstack-collector_<version>_amd64.deb
   ```

   ```text
   Setting up tally-openstack-collector (<version>) ...
   ```

2. Read back what it installed. The `tally` user and group are created by the
   package, and the state directory belongs to them:

   ```sh
   dpkg-query -W -f='${Status}\n' tally-openstack-collector
   getent passwd tally
   stat -c '%n %U:%G %a' /var/lib/tally/collector /etc/tally/ingest-token
   ```

   ```text
   install ok installed
   tally:x:998:998::/var/lib/tally/collector:/usr/sbin/nologin
   /var/lib/tally/collector tally:tally 750
   /etc/tally/ingest-token root:tally 640
   ```

   The service is installed stopped and disabled. It carries no credentials and
   no cloud yet, and the collector refuses that configuration before it
   listens, so nothing is started until you have configured it.

## Put the credentials in place

1. Write the broker URL and the ingest token into the two files the package
   shipped empty. Writing into the existing file keeps its `0640 root:tally`
   mode, which a new file created by a redirect would not have:

   ```sh
   printf '%s' 'amqp://tally:<password>@rabbitmq.example:5672/' | sudo tee /etc/tally/amqp-url > /dev/null
   printf '%s' 'tly_i_<token>' | sudo tee /etc/tally/ingest-token > /dev/null
   ```

2. Check that both kept their mode and are no longer empty. One trailing
   newline is trimmed when the file is read, so an `echo` here would have been
   fine too; an empty file is refused:

   ```sh
   stat -c '%n %U:%G %a' /etc/tally/amqp-url /etc/tally/ingest-token
   test -s /etc/tally/amqp-url && test -s /etc/tally/ingest-token && echo filled
   ```

   ```text
   /etc/tally/amqp-url root:tally 640
   /etc/tally/ingest-token root:tally 640
   filled
   ```

## Configure the collector

1. Open `/etc/default/tally-openstack-collector` and fill the two empty
   settings. Every other variable is in that file, commented out with its
   default beside it:

   ```sh
   sudoedit /etc/default/tally-openstack-collector
   ```

   ```sh
   TALLY_OSC_CLOUD=os-prod-eu1
   TALLY_OSC_REPORTING_URL=https://tally-reporting.internal
   ```

2. Where the cloud runs octavia, or renamed an exchange, uncomment the
   exchanges line and list its own. The collector declares the exchanges
   passively, so one the broker does not carry fails the connection:

   ```sh
   TALLY_OSC_EXCHANGES=nova,neutron,cinder,glance,octavia
   ```

3. Leave `TALLY_OSC_BUFFER_PATH` as it is. It points into
   `/var/lib/tally/collector`, the directory the package owns and the unit
   recreates; anywhere else the service may not write, because the unit runs
   under `ProtectSystem=strict`.

## Start the service

1. Enable the unit and start it:

   ```sh
   sudo systemctl enable --now tally-openstack-collector
   ```

2. Check that it came up and read its first lines. The collector logs JSON to
   the journal:

   ```sh
   systemctl is-active tally-openstack-collector
   journalctl -u tally-openstack-collector -n 1 -o cat
   ```

   ```text
   active
   {"time":"2026-07-09T14:22:00.512Z","level":"INFO","msg":"listening","service":"tally-openstack-collector","port":8080}
   ```

   A start that ends in `failed` names the value to fix. An empty credential
   file reports `TALLY_OSC_AMQP_URL_FILE: file /etc/tally/amqp-url is empty`,
   an unset cloud reports `TALLY_OSC_CLOUD: must be set`, and a token set in
   the environment file beside its `_FILE` companion reports
   `set TALLY_OSC_TOKEN or TALLY_OSC_TOKEN_FILE, not both`.

## Upgrade, remove and purge

1. Upgrade by installing the newer file. Your edits to
   `/etc/default/tally-openstack-collector` and to the two credential files are
   kept, because all three are conffiles; a changed default arrives beside them
   as `.dpkg-dist` for you to compare. The outbox is untouched, so events that
   have not reached the Reporting API are delivered after the restart:

   ```sh
   sudo apt install ./tally-openstack-collector_<newer-version>_amd64.deb
   ```

2. Remove the package to stop and disable the service while keeping its
   configuration and its outbox:

   ```sh
   sudo apt remove tally-openstack-collector
   ```

3. Purge it to drop the configuration and the credentials as well. The outbox
   is deliberately kept: between the acknowledgement on the bus and the
   delivery, an event lives in that file and nowhere else, so no purge destroys
   usage that exists in no other copy. Delete it by hand once you know it is
   empty:

   ```sh
   sudo apt purge tally-openstack-collector
   ```

   ```text
   tally-openstack-collector: /var/lib/tally/collector/outbox.db was kept.
     It may still hold events that never reached the Reporting API.
     Delete it by hand once you know it is empty or no longer needed.
   ```

   The `tally` user and group stay as well: an orphaned system account is
   harmless, and a later Tally package on this host uses the same one.

## Check the result

1. Ask the running service for readiness. It answers 200 while the consumer
   holds the broker connection and the outbox answers:

   ```sh
   curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/readyz
   ```

   ```text
   200
   ```

2. Read the two counters twice, a minute apart, while the cloud is in use. Both
   rise:

   ```sh
   curl -sS http://127.0.0.1:8080/metrics | grep -E '^tally_collector_(consumed|delivered)_total'
   ```

   ```text
   tally_collector_consumed_total{event_type="compute.instance.create.end"} 14
   tally_collector_delivered_total 12
   ```

3. Confirm the service comes back on its own. Kill it and read the state again
   a few seconds later; `Restart=on-failure` brings it back, and the outbox it
   reopens is the one it was writing:

   ```sh
   sudo systemctl kill -s KILL tally-openstack-collector
   sleep 10
   systemctl is-active tally-openstack-collector
   ```

   ```text
   active
   ```

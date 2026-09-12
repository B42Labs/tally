#!/bin/sh
set -e
if [ "$1" = "configure" ]; then
    # The user and the group are named tally rather than after this package: a
    # later package for another Tally service on the same host runs as the same
    # account and reads the same /etc/tally.
    #
    # --quiet keeps dpkg's output clean. Without it adduser reports "Not
    # creating home directory" for the --no-create-home it was just asked for.
    # Warnings and errors still get through.
    if ! getent group tally >/dev/null; then
        addgroup --system --quiet tally
    fi
    if ! getent passwd tally >/dev/null; then
        adduser --system --quiet --no-create-home --ingroup tally \
            --home /var/lib/tally/collector --shell /usr/sbin/nologin tally
    fi

    # dpkg resolves the archive's user and group names at unpack time, before
    # this script runs, so on a fresh install the shipped paths fall back to the
    # numeric ids in the header and land root:root. Apply the intended ownership
    # here. It is idempotent, so it also repairs an install made before the
    # tally account existed.
    for secret in /etc/tally/amqp-url /etc/tally/ingest-token; do
        if [ -e "$secret" ]; then
            chown root:tally "$secret"
            chmod 0640 "$secret"
        fi
    done
    if [ -d /var/lib/tally/collector ]; then
        chown tally:tally /var/lib/tally/collector
        chmod 0750 /var/lib/tally/collector
    fi

    if [ -d /run/systemd/system ]; then
        systemctl daemon-reload
    fi

    # The service is left stopped and disabled. A fresh install carries no
    # broker URL, no token and no cloud, and the collector refuses that
    # configuration before it listens, so starting it here would only flap
    # against Restart=on-failure until someone configured it.
fi

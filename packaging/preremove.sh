#!/bin/sh
set -e
# Only on remove: an upgrade also runs this script, and stopping the service
# there would take the sender away from an outbox it is draining.
if [ "$1" = "remove" ]; then
    systemctl stop tally-openstack-collector 2>/dev/null || true
    systemctl disable tally-openstack-collector 2>/dev/null || true
fi

#!/bin/sh
set -e
if [ "$1" = "remove" ] || [ "$1" = "purge" ]; then
    if [ -d /run/systemd/system ]; then
        systemctl daemon-reload || true
    fi
fi

if [ "$1" = "purge" ]; then
    # dpkg deletes the two credential conffiles itself on purge; this drops the
    # directory they were the only content of. It fails while anything else is
    # in there, which is what keeps a second Tally package's files.
    rmdir /etc/tally 2>/dev/null || true

    # The outbox stays, and this script deletes nothing under /var/lib/tally.
    # Between the acknowledgement on the bus and the delivery to the Reporting
    # API an event lives in that file and nowhere else, so a purge that removed
    # it would destroy usage that exists in no other copy, including the usage
    # of an operator who purges in order to reinstall.
    if [ -e /var/lib/tally/collector/outbox.db ]; then
        echo 'tally-openstack-collector: /var/lib/tally/collector/outbox.db was kept.' >&2
        echo '  It may still hold events that never reached the Reporting API.' >&2
        echo '  Delete it by hand once you know it is empty or no longer needed.' >&2
    fi
fi

# The tally user and group are kept in every case: an orphaned system account
# is harmless, it may still own files elsewhere, and a later Tally package on
# this host uses the same one.

#!/bin/sh
set -e

# The `[ -d /run/systemd/system ]` guard skips systemctl in chroots and
# containers where systemd is not running (matching postinstall.sh and the
# debhelper convention), so `dpkg -r`/`--purge` succeeds there too.
if [ "$1" = "remove" ] || [ "$1" = "purge" ]; then
    if [ -d /run/systemd/system ]; then
        systemctl daemon-reload
    fi
fi

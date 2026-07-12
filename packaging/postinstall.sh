#!/bin/sh
set -e

# `configure` is passed on both fresh installs and upgrades. The
# `[ -d /run/systemd/system ]` guard is the debhelper-conventional way to
# skip systemctl in chroots and containers where systemd is not the init
# system (and lets the PR package smoke test run in a plain container).
if [ "$1" = "configure" ] && [ -d /run/systemd/system ]; then
    systemctl daemon-reload

    # On an upgrade the old binary keeps running until the service is
    # restarted. A restart drains this node's HA gateways first, which is
    # hitless ONLY one node at a time: restarting the whole fleet at once —
    # as unattended-upgrades or a config-management run (Ansible/Salt) does —
    # makes every HA peer drain concurrently, their takeover handshakes
    # deadlock, and the Floating IPs blackhole until systemd SIGKILLs the
    # stuck processes at TimeoutStopSec. The package therefore does NOT
    # auto-restart on upgrade; it prints the pending restart so the running
    # version does not silently lag. Operators doing a controlled
    # one-node-at-a-time roll opt in with OVN_NETWORK_RESTART_ON_UPGRADE=true
    # in /etc/default/ovn-network-agent. try-restart only acts on an already
    # active unit, so fresh installs (never started) are unaffected.
    if systemctl is-active --quiet ovn-network-agent.service; then
        # /etc/default/ovn-network-agent is a systemd EnvironmentFile whose
        # values may contain spaces, so read the flag with sed rather than
        # sourcing the file (sourcing would execute such values as commands).
        restart_on_upgrade=$(
            sed -n 's/^[[:space:]]*OVN_NETWORK_RESTART_ON_UPGRADE[[:space:]]*=[[:space:]]*//p' \
                /etc/default/ovn-network-agent 2>/dev/null \
                | tr -d '"\r' | sed 's/[[:space:]]*$//' | tail -n1
        )
        case "$restart_on_upgrade" in
        1 | [Tt][Rr][Uu][Ee] | [Yy][Ee][Ss])
            echo "ovn-network-agent: OVN_NETWORK_RESTART_ON_UPGRADE is set — restarting the running service to load the upgraded binary (gateway drain keeps the failover hitless one node at a time)."
            systemctl try-restart ovn-network-agent.service || \
                echo "ovn-network-agent: automatic restart failed — a restart is pending; run 'systemctl restart ovn-network-agent' manually." >&2
            ;;
        *)
            echo "ovn-network-agent: the upgraded binary is installed but the running service was NOT restarted, so the running version now lags the installed one. Restart this node when ready with 'systemctl restart ovn-network-agent' (drains HA gateways, hitless one node at a time), or set OVN_NETWORK_RESTART_ON_UPGRADE=true in /etc/default/ovn-network-agent to restart automatically on upgrade." >&2
            ;;
        esac
    fi
fi

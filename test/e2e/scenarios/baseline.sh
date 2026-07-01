#!/usr/bin/env bash
# Baseline reachability scenario for the containerlab E2E harness.
#
# Goal (issue #45): client-1 pings the FIP on the active gateway and
# observes 100% success once the agent has reconciled. The reconcile
# window (RECONCILE_TIMEOUT) defaults to 60s: on cold CI runners the
# full data path — OVN BFD/geneve tunnel bring-up, the cr-lr0-public HA
# election, the agent's FRR FIP routes and BGP propagation to the
# upstream — regularly needs more than the 30s originally measured on a
# warm local Docker host. That tight window made this reachability gate
# flaky across every scenario that runs it as a sanity pre-check
# (hairpin.sh / pf-hairpin.sh / pf-external.sh). 60s matches
# pf-external.sh, the one scenario that never flaked here.
#
# The bootstrap script seeds the OVN NB DB with a logical switch,
# logical router and two FIPs (192.0.2.10, 192.0.2.11) distributed
# across the three gateway chassis with HA priorities 30/20/10. This
# scenario simply verifies that reachability through the FIP is intact
# once the agent has caught up. Per the issue's "Test runner" section
# the probe is a plain `docker exec clab-<lab>-client-1 ping`.
#
# Environment overrides (used by the CI workflow):
#   LAB                container-name prefix (defaults to the topology name "ovn-e2e")
#   FIP                target FIP to ping (defaults to the priority-30 chassis FIP)
#   CLIENT             source container (defaults to clab-${LAB}-client-1)
#   RECONCILE_TIMEOUT  seconds to wait for the agent to reconcile (default 60)
#   PING_COUNT         packets for the final reachability check (default 5)
#   PING_TIMEOUT       per-packet wait, passed to ping -W (default 2)

set -euo pipefail

LAB="${LAB:-ovn-e2e}"
FIP="${FIP:-192.0.2.10}"
CLIENT="${CLIENT:-clab-${LAB}-client-1}"
RECONCILE_TIMEOUT="${RECONCILE_TIMEOUT:-60}"
PING_COUNT="${PING_COUNT:-5}"
PING_TIMEOUT="${PING_TIMEOUT:-2}"

log() { printf '[baseline] %s\n' "$*" >&2; }

# Poll the reachability check for up to RECONCILE_TIMEOUT seconds. The
# agent's reconcile loop installs FRR static routes and OVS hairpin
# flows asynchronously after ovn-controller binds the chassisredirect
# port; a polling probe waits for the data path to come up without
# hard-coding a sleep.
wait_for_reachability() {
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    log "waiting up to ${RECONCILE_TIMEOUT}s for ${CLIENT} → ${FIP} to come up"
    while (( $(date +%s) < deadline )); do
        if docker exec "${CLIENT}" ping -c 1 -W 1 "${FIP}" >/dev/null 2>&1; then
            log "first packet through after $(( $(date +%s) - (deadline - RECONCILE_TIMEOUT) ))s"
            return 0
        fi
        sleep 2
    done
    log "no reply within ${RECONCILE_TIMEOUT}s — falling through to the final probe so its output appears in the log"
    return 0
}

# Final probe — the exit code of this scenario is the exit code of
# ping, which is non-zero on any packet loss (per the acceptance
# criterion).
probe() {
    log "ping -c ${PING_COUNT} -W ${PING_TIMEOUT} ${FIP} from ${CLIENT}"
    docker exec "${CLIENT}" ping -c "${PING_COUNT}" -W "${PING_TIMEOUT}" "${FIP}"
}

main() {
    wait_for_reachability
    probe
}

main "$@"

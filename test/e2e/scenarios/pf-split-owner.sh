#!/usr/bin/env bash
#
# Port-forward VIP whose gateway port lives on another chassis (issue #247).
#
# Goal: pin the failure the nightly chaos run kept finding by accident. A
# port-forward VIP is answered by whichever chassis holds the DNAT rules,
# while OVN client traffic leaves through whichever chassis holds the
# router's chassisredirect port. On a fleet where only some gateways carry
# the VIP configuration — a rollout that has reached some nodes but not
# all — those are different chassis, and the VIP goes dark for every
# client behind an OVN network until an operator moves the port back by
# hand. Failback does not happen on its own: the agent defends the active
# chassis's priority lead, which is exactly the anti-flapping behaviour
# the rest of the design wants.
#
# What makes both legs work is a default route in each gateway's
# vrf-provider, which bootstrap.sh has the upstream originate to every
# gateway. Without it:
#
#   * the request dies in the owner's VRF, which has no route towards the
#     peer announcing the VIP, and
#   * the reply dies in the announcing node's VRF, which (as an lr0
#     standby) no longer leaks 192.0.2.0/24 and has no route back to the
#     client's FIP.
#
# Neither leg is visible from the announce plane: the VIP address, the
# DNAT ruleset, the prefix-list and the BGP announcement all stay
# healthy while the data path is dark.
#
# Topology (all seeded by bootstrap.sh unless noted):
#
#   vm1 netns on gateway-3, 192.168.10.10 behind FIP 192.0.2.10
#   VIP 198.18.0.50:8080 on gateway-1 only, backed by pf-backend on
#     gateway-1's own management address (172.30.0.11:8080)
#   lr-split-vlan  — added here, a router with no workloads pinned to
#     gateway-1, whose only job is to keep gateway-1 announce-capable
#     after it loses lr0 (#206 holds a router-less node's VIPs dormant)
#
# The VIP sits in 198.18.0.0/15 (RFC 2544) rather than in a lab subnet:
# an address inside the provider network would be a connected route on
# lr0, so OVN would deliver it on the public logical switch and ARP for
# it there, and the chassis kernel's DNAT would never see the packet.
# Outside every connected subnet it follows lr0's default route out
# through cr-lr0-public onto br-ex, where the MAC-tweak flow hands it to
# the kernel. This is the same trap pf-hairpin.sh documents.
#
# Phases:
#
#   1. Owner = gateway-1, the VIP carrier. The probe never leaves that
#      chassis, so it passes with or without the fix. It is the sanity
#      gate: a red phase 1 means the VIP wiring is wrong, not the split.
#   2. Owner = gateway-3, which carries no VIP configuration. Now both
#      legs cross the fabric and the run is measuring what #247 broke.
#
# Pre-condition: a lab that is up and baseline-green (`make e2e-up`).
#
# Teardown (EXIT trap, best-effort): stop the backend, drop the VLAN
# layer, restore gateway-1's agent config and restart it, put the
# underlay back, and reset the lr0-public priorities to the bootstrap
# values.
#
# Environment overrides (used by the CI workflow):
#
#   LAB                containerlab lab name (default ovn-e2e)
#   MASTER             the VIP carrier (default gateway-1)
#   SPLIT_OWNER        chassis moved onto cr-lr0-public (default gateway-3)
#   VIP / VIP_PORT     the port-forward VIP (default 198.18.0.50:8080)
#   PROBE_TIMEOUT      per-probe timeout in seconds (default 5)
#   RECONCILE_TIMEOUT  how long a phase polls (default 60)
#   RESTART_TIMEOUT    how long to wait for the agent after a restart (default 90)
#   ARTIFACTS_DIR      where failure dumps are written (default: none)
#   SANITY_GATE        run baseline.sh first (default 1, set 0 to skip)

set -euo pipefail

LAB="${LAB:-ovn-e2e}"
MASTER="${MASTER:-gateway-1}"
MASTER_NODE="clab-${LAB}-${MASTER}"
SPLIT_OWNER="${SPLIT_OWNER:-gateway-3}"
SPLIT_OWNER_NODE="clab-${LAB}-${SPLIT_OWNER}"
# The third gateway. It takes no part in the scenario; it is named only so
# the teardown can put the whole bootstrap priority ladder back.
MIDDLE="${MIDDLE:-gateway-2}"
UPSTREAM="${UPSTREAM:-upstream}"
UPSTREAM_NODE="clab-${LAB}-${UPSTREAM}"
CENTRAL="${CENTRAL:-clab-${LAB}-central}"

LR_PUBLIC_PORT="${LR_PUBLIC_PORT:-lr0-public}"
CR_PORT="cr-${LR_PUBLIC_PORT}"

# The workload the probe runs from: bootstrap seeds vm1 on gateway-3
# behind FIP 192.0.2.10. It is deliberately on the split owner, so its
# egress rides whichever chassis holds cr-lr0-public.
CLIENT_NETNS="${CLIENT_NETNS:-vm1}"
CLIENT_NODE="${CLIENT_NODE:-${SPLIT_OWNER_NODE}}"

VIP="${VIP:-198.18.0.50}"
VIP_PORT="${VIP_PORT:-8080}"
BACKEND_IP="${BACKEND_IP:-172.30.0.11}"
BACKEND_PORT="${BACKEND_PORT:-8080}"
BACKEND_LOG="${BACKEND_LOG:-/tmp/pf-split-owner-backend.log}"

# A router with no workloads, pinned to MASTER. #206 keeps a node's VIPs
# dormant while it hosts no local routers, so without this MASTER would
# stop announcing the VIP the moment lr0 moves away — and the scenario
# would be measuring dormancy rather than the split path.
VLAN_LR="${VLAN_LR:-lr-split-vlan}"
VLAN_LS="${VLAN_LS:-ls-split-vlan}"
VLAN_LN_LSP="${VLAN_LN_LSP:-ls-split-vlan-ln}"
VLAN_LRP="${VLAN_LRP:-lr-split-vlan-public}"
VLAN_LRP_MAC="${VLAN_LRP_MAC:-02:00:00:00:5e:01}"
VLAN_LRP_CIDR="${VLAN_LRP_CIDR:-198.51.100.4/24}"
VLAN_TAG="${VLAN_TAG:-101}"
VLAN_PHYSNET="${VLAN_PHYSNET:-physnet1}"
VLAN_PRIORITY="${VLAN_PRIORITY:-50}"

AGENT_CONFIG_PATH="${AGENT_CONFIG_PATH:-/etc/ovn-network-agent/config.yaml}"
PROBE_TIMEOUT="${PROBE_TIMEOUT:-5}"
RECONCILE_TIMEOUT="${RECONCILE_TIMEOUT:-60}"
RESTART_TIMEOUT="${RESTART_TIMEOUT:-90}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-}"
SANITY_GATE="${SANITY_GATE:-1}"

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_DIR="$(cd "${SCENARIOS_DIR}/.." && pwd)"
GWNODE_CONFIG="${GWNODE_CONFIG:-${E2E_DIR}/gwnode-config.yaml}"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"

# Captured before the first restart; see capture_master_underlay.
UNDERLAY_CIDR=""
UNDERLAY_IFACE=""
UNDERLAY_PEER_CIDR=""

log() { printf '[pf-split-owner] %s\n' "$*" >&2; }

nbctl() { docker exec "${CENTRAL}" ovn-nbctl "$@"; }
sbctl() { docker exec "${CENTRAL}" ovn-sbctl "$@"; }

# Write stdin to `path` under ARTIFACTS_DIR when it is set; quietly
# no-op otherwise. Same shape used by pf-hairpin.sh.
write_artifact() {
    local path="$1"
    if [ -z "${ARTIFACTS_DIR}" ]; then
        cat >/dev/null
        return 0
    fi
    mkdir -p "${ARTIFACTS_DIR}"
    cat >"${ARTIFACTS_DIR}/${path}"
}

sanity_gate() {
    if [ "${SANITY_GATE}" != "1" ]; then
        log "SANITY_GATE=${SANITY_GATE}: skipping baseline pre-check"
        return 0
    fi
    if [ ! -x "${BASELINE}" ]; then
        log "baseline script not executable at ${BASELINE} — skipping pre-check"
        return 0
    fi
    log "running baseline.sh as a sanity gate (set SANITY_GATE=0 to skip)"
    "${BASELINE}"
}

# -----------------------------------------------------------------------------
# OVN topology
# -----------------------------------------------------------------------------

# A gatewayless VLAN network whose only purpose is keeping MASTER's
# HasLocalRouters true after lr0 moves away. It carries no scenario
# traffic, so it gets no workloads, no FIPs and no underlay wiring — the
# localnet port and tag exist only because a provider network needs them
# to be a well-formed OVN network.
ensure_vlan_anchor() {
    log "ensuring anchor router ${VLAN_LR} pinned to ${MASTER} (keeps its VIPs announceable)"
    nbctl --may-exist ls-add "${VLAN_LS}"
    nbctl --may-exist lsp-add "${VLAN_LS}" "${VLAN_LN_LSP}"
    nbctl lsp-set-type      "${VLAN_LN_LSP}" localnet
    nbctl lsp-set-addresses "${VLAN_LN_LSP}" unknown
    nbctl lsp-set-options   "${VLAN_LN_LSP}" network_name="${VLAN_PHYSNET}"
    nbctl set Logical_Switch_Port "${VLAN_LN_LSP}" tag="${VLAN_TAG}"

    nbctl --may-exist lr-add "${VLAN_LR}"
    nbctl --may-exist lrp-add "${VLAN_LR}" "${VLAN_LRP}" "${VLAN_LRP_MAC}" "${VLAN_LRP_CIDR}"
    nbctl --may-exist lsp-add "${VLAN_LS}" "${VLAN_LS}-${VLAN_LR}"
    nbctl lsp-set-type      "${VLAN_LS}-${VLAN_LR}" router
    nbctl lsp-set-addresses "${VLAN_LS}-${VLAN_LR}" router
    nbctl lsp-set-options   "${VLAN_LS}-${VLAN_LR}" router-port="${VLAN_LRP}"

    # A single Gateway_Chassis row, so the agent's active-lead boost finds
    # no peer to outrank and leaves the priority where we put it.
    nbctl lrp-set-gateway-chassis "${VLAN_LRP}" "${MASTER}" "${VLAN_PRIORITY}"
}

# -----------------------------------------------------------------------------
# Agent configuration on MASTER
# -----------------------------------------------------------------------------

# Give MASTER — and only MASTER — the VIP. That asymmetry is the whole
# point: a fleet mid-rollout is what puts the VIP and the gateway port on
# different chassis. SPLIT_OWNER needs no configuration of its own.
#
# port_forward_l3mdev_accept is required because the backend socket sits
# in the default VRF while the VIP traffic ingresses vrf-provider.
write_agent_config() {
    log "writing ${AGENT_CONFIG_PATH} on ${MASTER} (port_forwards VIP=${VIP})"
    {
        cat "${GWNODE_CONFIG}"
        cat <<EOF

# Injected by test/e2e/scenarios/pf-split-owner.sh (issue #247).
port_forward_l3mdev_accept: true
port_forwards:
  - vip: "${VIP}"
    manage_vip: true
    hairpin_masquerade: false
    rules:
      - proto: tcp
        port: ${VIP_PORT}
        dest_addr: "${BACKEND_IP}"
        dest_port: ${BACKEND_PORT}
EOF
    } | docker exec -i "${MASTER_NODE}" sh -c "cat > '${AGENT_CONFIG_PATH}'"
}

restore_agent_config() {
    log "restoring baseline agent config on ${MASTER}"
    docker exec -i "${MASTER_NODE}" sh -c "cat > '${AGENT_CONFIG_PATH}'" \
        < "${GWNODE_CONFIG}" || true
}

# `docker restart` is the only way to make the agent re-read its config:
# the gwnode image runs no service manager and the entrypoint execs the
# agent as tini's only child. Copied from pf-hairpin.sh.
restart_master() {
    log "restarting ${MASTER_NODE} so the agent reloads ${AGENT_CONFIG_PATH}"
    docker restart "${MASTER_NODE}" >/dev/null

    local deadline
    deadline=$(( $(date +%s) + RESTART_TIMEOUT ))
    while (( $(date +%s) < deadline )); do
        if docker exec "${MASTER_NODE}" \
                pgrep -f /usr/local/bin/ovn-network-agent >/dev/null 2>&1; then
            log "agent process is running on ${MASTER}"
            return 0
        fi
        sleep 2
    done
    log "ERROR: agent did not (re)start within ${RESTART_TIMEOUT}s on ${MASTER}"
    return 1
}

# -----------------------------------------------------------------------------
# Underlay repair
#
# `docker restart` destroys the containerlab veth `MASTER:eth1 ↔
# upstream:ethN`, and with it the underlay address and the BGP session.
# pf-hairpin.sh can ignore that because its data path rides geneve; this
# scenario cannot, because both legs of the split path cross the fabric.
# Mirrors hairpin-churn.sh, which solves the same problem.
# -----------------------------------------------------------------------------

# Record the link before anything restarts it, off the live lab rather
# than duplicating bootstrap.sh's UNDERLAY_LINKS table.
capture_master_underlay() {
    UNDERLAY_CIDR="$(docker exec "${MASTER_NODE}" ip -o -4 addr show eth1 2>/dev/null \
        | awk '{ print $4; exit }' || true)"
    if [ -z "${UNDERLAY_CIDR}" ]; then
        log "ERROR: ${MASTER} has no IPv4 address on eth1 — the underlay cannot be restored after the restart"
        return 1
    fi
    # Each gateway link is its own /30 on its own third octet, so the
    # address prefix identifies the peer interface without assuming which
    # host address either side took.
    local addr prefix peer
    addr="${UNDERLAY_CIDR%/*}"
    prefix="${addr%.*}."
    peer="$(docker exec "${UPSTREAM_NODE}" ip -o -4 addr show 2>/dev/null \
        | awk -v prefix="${prefix}" -v mine="${UNDERLAY_CIDR}" \
              'index($4, prefix) == 1 && $4 != mine { print $2, $4; exit }' || true)"
    if [ -z "${peer}" ]; then
        log "ERROR: no ${UPSTREAM} interface is on ${prefix}0/30 beside ${UNDERLAY_CIDR}"
        return 1
    fi
    read -r UNDERLAY_IFACE UNDERLAY_PEER_CIDR <<<"${peer}"
    log "captured ${MASTER}:eth1 ${UNDERLAY_CIDR} <-> ${UPSTREAM}:${UNDERLAY_IFACE} ${UNDERLAY_PEER_CIDR}"
}

# Put the veth and both addresses back. FRR keeps its own written config
# across a restart (bootstrap.sh ends its push with `write memory`), so
# the BGP session re-establishes on its own once the addresses are back.
restore_master_underlay() {
    [ -n "${UNDERLAY_CIDR}" ] || return 0
    if ! docker exec "${MASTER_NODE}" ip link show eth1 >/dev/null 2>&1; then
        log "re-creating the ${MASTER}:eth1 ↔ ${UPSTREAM}:${UNDERLAY_IFACE} veth"
        containerlab tools veth create \
            -a "${MASTER_NODE}:eth1" \
            -b "${UPSTREAM_NODE}:${UNDERLAY_IFACE}" >/dev/null || {
            log "WARNING: could not re-create ${MASTER}'s underlay veth"
            return 0
        }
    fi
    log "restoring ${MASTER}'s underlay (${UNDERLAY_CIDR} on eth1, ${UNDERLAY_PEER_CIDR} on ${UPSTREAM}:${UNDERLAY_IFACE})"
    docker exec "${MASTER_NODE}" sh -c "
        ovs-vsctl --if-exists del-port br-ex eth1
        ip link set eth1 down
        ip link set eth1 master vrf-provider
        ip link set eth1 up
        ip addr replace ${UNDERLAY_CIDR} dev eth1
    " || log "WARNING: wiring ${MASTER}'s eth1 back into vrf-provider failed"
    docker exec "${UPSTREAM_NODE}" sh -c "
        ip link set ${UNDERLAY_IFACE} up
        ip addr replace ${UNDERLAY_PEER_CIDR} dev ${UNDERLAY_IFACE}
    " || log "WARNING: wiring the ${UPSTREAM} side of ${MASTER}'s link back failed"
}

# The precondition both legs rest on. Polled rather than asserted once,
# because after an underlay repair the session has to re-establish before
# the upstream re-originates the default.
wait_for_vrf_default() {
    local gw="clab-${LAB}-$1"
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    log "waiting up to ${RECONCILE_TIMEOUT}s for a vrf-provider default route on $1"
    while (( $(date +%s) < deadline )); do
        if [ -n "$(docker exec "${gw}" ip route show vrf vrf-provider default 2>/dev/null)" ]; then
            log "$1 has a vrf-provider default route"
            return 0
        fi
        sleep 2
    done
    log "ERROR: $1 has no vrf-provider default route after ${RECONCILE_TIMEOUT}s — the upstream is not originating one (bootstrap.sh configure_upstream_frr)"
    return 1
}

# -----------------------------------------------------------------------------
# Backend
# -----------------------------------------------------------------------------

# The responder behind the VIP, in MASTER's *default* netns: that is
# where the DNAT rule points (MASTER's own management address), and it is
# why the config sets port_forward_l3mdev_accept.
start_backend() {
    log "starting pf-backend on ${MASTER}:${BACKEND_PORT}"
    docker exec "${MASTER_NODE}" pkill -f /usr/local/bin/pf-backend 2>/dev/null || true
    docker exec "${MASTER_NODE}" sh -c ": >'${BACKEND_LOG}'"
    docker exec -d "${MASTER_NODE}" \
        /usr/local/bin/pf-backend \
            -addr ":${BACKEND_PORT}" \
            -log "${BACKEND_LOG}"

    local deadline
    deadline=$(( $(date +%s) + 10 ))
    while (( $(date +%s) < deadline )); do
        if docker exec "${MASTER_NODE}" \
                sh -c "ss -ltn 'sport = :${BACKEND_PORT}' | grep -q LISTEN"; then
            log "pf-backend is listening on ${BACKEND_PORT}"
            return 0
        fi
        sleep 1
    done
    log "ERROR: pf-backend did not listen on ${BACKEND_PORT} within 10s"
    return 1
}

# -----------------------------------------------------------------------------
# Chassisredirect ownership
# -----------------------------------------------------------------------------

current_cr_owner() {
    local chassis_uuid
    chassis_uuid="$(sbctl --bare --columns=chassis find Port_Binding \
        logical_port="${CR_PORT}" 2>/dev/null || true)"
    [ -n "${chassis_uuid}" ] || return 0
    sbctl --bare --columns=name list Chassis "${chassis_uuid}" 2>/dev/null || true
}

# The highest priority among lr0-public's own Gateway_Chassis rows.
#
# Scoped to that port on purpose. The election is per logical router port,
# and this scenario deliberately adds a second router (the VLAN anchor,
# priority 50) whose row would otherwise dominate a fleet-wide maximum and
# make the comparison below meaningless. Gateway_Chassis names are
# "<lrp>-<chassis>", which is what makes the rows separable.
lr_public_peak_priority() {
    nbctl --format=csv --no-headings --columns=name,priority list Gateway_Chassis 2>/dev/null \
        | awk -F, -v prefix="${LR_PUBLIC_PORT}-" \
              'index($1, prefix) == 1 { print $2 }' \
        | sort -n | tail -1
}

# Raise `gw`'s priority on lr0-public above every other row on that port so
# ovn-northd re-elects it — the move an operator makes to migrate a gateway
# deliberately. Skipped once it already holds the peak, so the polling
# caller stays idempotent.
#
# It has to out-run the losing chassis's own agent, which boosts itself by
# one to defend the lead until the port actually moves. Stepping by ten
# wins that race, and once the port has moved the peer is no longer active
# and stops boosting at all.
reassert_priority() {
    local gw="$1" mine peak
    mine="$(nbctl --bare --columns=priority find Gateway_Chassis \
        "name=${LR_PUBLIC_PORT}-${gw}" 2>/dev/null | head -1 || true)"
    peak="$(lr_public_peak_priority || true)"
    [ -n "${peak}" ] || return 0
    if [ -n "${mine}" ] && [ "${mine}" -eq "${peak}" ]; then
        return 0 # already leads; ovn-northd just has not moved the port yet
    fi
    nbctl lrp-set-gateway-chassis "${LR_PUBLIC_PORT}" "${gw}" "$(( peak + 10 ))" || true
}

wait_for_cr_owner() {
    local gw="$1" budget="${2:-${RECONCILE_TIMEOUT}}"
    local deadline
    deadline=$(( $(date +%s) + budget ))
    log "waiting up to ${budget}s for ${CR_PORT} to bind to ${gw}"
    while (( $(date +%s) < deadline )); do
        reassert_priority "${gw}"
        if [ "$(current_cr_owner)" = "${gw}" ]; then
            log "${CR_PORT} bound to ${gw}"
            return 0
        fi
        sleep 2
    done
    log "ERROR: ${CR_PORT} did not bind to ${gw} within ${budget}s"
    local table
    table="$(nbctl list Gateway_Chassis 2>&1 || true)"
    printf '%s\n' "${table}" | write_artifact "gateway-chassis-timeout.txt"
    printf '%s\n' "${table}" | sed 's/^/    /' >&2
    return 1
}

# -----------------------------------------------------------------------------
# Probe
# -----------------------------------------------------------------------------

# A bare TCP handshake, through bash's /dev/tcp redirect: the gwnode image
# ships no HTTP client, and the vantage is inside the lab rather than on
# client-1. Same idiom as pf-hairpin.sh and the chaos runner's probeTCP,
# which measures this very VIP from this very netns.
#
# A handshake is enough to prove what #247 broke. It completes only if the
# SYN reached the backend and the SYN-ACK came back, so it exercises both
# legs — and the reply leg is the one a split owner kills.
probe_once() {
    docker exec "${CLIENT_NODE}" \
        ip netns exec "${CLIENT_NETNS}" \
        timeout "${PROBE_TIMEOUT}" bash -c "exec 3<>/dev/tcp/${VIP}/${VIP_PORT}"
}

assert_probe_succeeds() {
    local label="$1" description="$2"
    log "${label}: probing ${VIP}:${VIP_PORT} from ${CLIENT_NETNS} on ${SPLIT_OWNER} — ${description} (up to ${RECONCILE_TIMEOUT}s)"
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    while (( $(date +%s) < deadline )); do
        if probe_once; then
            log "${label}: probe succeeded"
            return 0
        fi
        sleep 2
    done
    log "ERROR: ${label}: probe did not succeed within ${RECONCILE_TIMEOUT}s"
    dump_routing_state "${label}-failure" 1
    return 1
}

# The three things that explain a dead split path, dumped per gateway:
# the VRF's own routes (does it know where to send this?), the policy
# rules that steer traffic into the VRF, and the prefix-list that decides
# what each gateway announces.
#
# With `echo` set the dump also goes to stderr, so a local run without
# ARTIFACTS_DIR still shows why the probe failed.
dump_routing_state() {
    local label="$1" echo_it="${2:-}" gw node state
    state="$(
        printf '# routing state — %s\n' "${label}"
        for gw in "${MASTER}" "${SPLIT_OWNER}"; do
            node="clab-${LAB}-${gw}"
            printf '\n=== %s: ip route show vrf vrf-provider ===\n' "${gw}"
            docker exec "${node}" ip route show vrf vrf-provider 2>&1 || true
            printf '\n=== %s: ip rule ===\n' "${gw}"
            docker exec "${node}" ip rule 2>&1 || true
            printf '\n=== %s: ANNOUNCED-NETWORKS ===\n' "${gw}"
            docker exec "${node}" vtysh -c 'show ip prefix-list ANNOUNCED-NETWORKS' 2>&1 || true
        done
        printf '\n=== %s owner ===\n' "${CR_PORT}"
        current_cr_owner || true
    )"
    printf '%s\n' "${state}" | write_artifact "routing-${label}.txt"
    if [ -n "${echo_it}" ]; then
        printf '%s\n' "${state}" | sed 's/^/    /' >&2
    fi
}

# -----------------------------------------------------------------------------

teardown() {
    log "teardown: dropping the backend, the VLAN anchor and the injected config"
    dump_routing_state "teardown" || true
    docker exec "${MASTER_NODE}" pkill -f /usr/local/bin/pf-backend 2>/dev/null || true
    docker exec "${MASTER_NODE}" rm -f "${BACKEND_LOG}" 2>/dev/null || true

    # lr-del cascades the LRP; ls-del cascades the LSPs including the
    # localnet port. The agent prunes br-ex.<tag> on its next reconcile.
    nbctl --if-exists lr-del "${VLAN_LR}" || true
    nbctl --if-exists ls-del "${VLAN_LS}" || true

    restore_agent_config
    docker restart "${MASTER_NODE}" >/dev/null 2>&1 || true
    restore_master_underlay || true

    # Hand the port back before writing the bootstrap priorities, not
    # after: SPLIT_OWNER is active and its agent defends its lead, so a
    # plain reset to 30 would leave MASTER below it and the election
    # untouched. Once MASTER owns the port again its own agent keeps it
    # there, and the numbers below are what a fresh lab would show.
    wait_for_cr_owner "${MASTER}" "${TEARDOWN_TIMEOUT:-30}" || true
    nbctl lrp-set-gateway-chassis "${LR_PUBLIC_PORT}" "${MASTER}" 30 || true
    nbctl lrp-set-gateway-chassis "${LR_PUBLIC_PORT}" "${MIDDLE}" 20 || true
    nbctl lrp-set-gateway-chassis "${LR_PUBLIC_PORT}" "${SPLIT_OWNER}" 10 || true
}

main() {
    sanity_gate

    capture_master_underlay

    trap teardown EXIT

    ensure_vlan_anchor

    write_agent_config
    restart_master
    restore_master_underlay
    start_backend

    # The restart handed cr-lr0-public to a peer, whose agent then
    # defended the lead. Take it back so phase 1 starts from a known
    # owner rather than from whoever won the race.
    wait_for_cr_owner "${MASTER}"
    wait_for_vrf_default "${MASTER}"
    wait_for_vrf_default "${SPLIT_OWNER}"

    # Phase 1 — owner is the VIP carrier, so the path never leaves it.
    assert_probe_succeeds "phase1" "owner ${MASTER} carries the VIP"

    # Phase 2 — owner is the chassis with no VIP configuration. The
    # request now leaves its VRF towards ${MASTER}, and the reply leaves
    # ${MASTER}'s VRF towards a FIP ${SPLIT_OWNER} announces. Both need
    # the default route this scenario exists to protect.
    log "moving ${CR_PORT} to ${SPLIT_OWNER}, which carries no port_forwards"
    wait_for_cr_owner "${SPLIT_OWNER}"
    assert_probe_succeeds "phase2" "owner ${SPLIT_OWNER} carries no VIP config"

    log "both phases passed: the split-owner port-forward path works"
}

main "$@"

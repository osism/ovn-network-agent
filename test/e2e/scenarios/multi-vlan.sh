#!/usr/bin/env bash
# Multi-VLAN provider-network scenario for the containerlab E2E harness.
#
# Goal (issue #147): a single gateway node announces FIPs from TWO
# VLAN-tagged provider networks on the same physnet. Two routers — each
# with its own VLAN localnet network (tags 101/102 on physnet1) and a
# gatewayless public subnet — are pinned to gateway-1. The agent must:
#
#   * create one kernel 802.1Q subinterface per segment (br-ex.101,
#     br-ex.102) carrying the bridge IP and the per-FIP /32 routes,
#   * install MAC-tweak flows (cookie 0x999) per localnet patch port —
#     two distinct in_ports, which the old single-patch-port data path
#     could not do,
#   * announce both FIPs as /32 via BGP from the same node,
#
# and both FIPs must be reachable end-to-end from client-1 while the
# flat baseline FIP (192.0.2.10) keeps working (issue #147 AC 1 + 2).
#
# Why no underlay change is needed: the fabric routes each FIP /32 via
# BGP into gateway-1's vrf-provider, and the agent hands the packet to
# OVN through the node-local kernel path (br-ex.<tag> → tagged patch
# port). The VLAN frames never leave the node, so the /30 underlay
# links stay untouched.
#
# Pre-condition: the lab is up. When SANITY_GATE=1 (default) this
# scenario runs `baseline.sh` first so a broken green path is reported
# as a baseline regression instead of being attributed to multi-VLAN.
#
# Teardown: the routers, switches, netns workloads and MAC bindings
# added here are removed on EXIT (via trap), returning the lab to
# baseline state. The agent itself prunes the br-ex.<tag> subinterfaces
# and per-segment flows on its next reconcile once the routers are
# gone. The CI workflow additionally invokes the teardown step with
# `if: always()`.
#
# Environment overrides (used by the CI workflow):
#   LAB                 container-name prefix (defaults to the topology name "ovn-e2e")
#   MASTER              chassis both routers are pinned to (defaults to gateway-1)
#   MASTER_UNDERLAY_IP  MASTER's vrf-provider /30 address as seen by upstream (default 100.64.1.2)
#   WORKLOAD_HOST       chassis hosting the two workloads (defaults to gateway-3)
#   CENTRAL             OVN central container (defaults to clab-${LAB}-central)
#   UPSTREAM            upstream router container (defaults to clab-${LAB}-upstream)
#   CLIENT              probe source container (defaults to clab-${LAB}-client-1)
#   BASELINE_FIP        flat-network FIP that must stay green (default 192.0.2.10)
#   RECONCILE_TIMEOUT   seconds to wait for agent/OVN convergence per check (default 60)
#   PING_COUNT          packets for the final reachability checks (default 5)
#   PING_TIMEOUT        per-packet wait, passed to ping -W (default 2)
#   ARTIFACTS_DIR       directory for flow/route snapshots (default empty = skip)
#   SANITY_GATE         run baseline.sh first when 1 (default 1)

set -euo pipefail

LAB="${LAB:-ovn-e2e}"
MASTER="${MASTER:-gateway-1}"
MASTER_NODE="clab-${LAB}-${MASTER}"
MASTER_UNDERLAY_IP="${MASTER_UNDERLAY_IP:-100.64.1.2}"
WORKLOAD_HOST="${WORKLOAD_HOST:-gateway-3}"
WORKLOAD_NODE="clab-${LAB}-${WORKLOAD_HOST}"
CENTRAL="${CENTRAL:-clab-${LAB}-central}"
UPSTREAM="${UPSTREAM:-clab-${LAB}-upstream}"
CLIENT="${CLIENT:-clab-${LAB}-client-1}"
BASELINE_FIP="${BASELINE_FIP:-192.0.2.10}"
RECONCILE_TIMEOUT="${RECONCILE_TIMEOUT:-60}"
PING_COUNT="${PING_COUNT:-5}"
PING_TIMEOUT="${PING_TIMEOUT:-2}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-}"
SANITY_GATE="${SANITY_GATE:-1}"

PHYSNET="${PHYSNET:-physnet1}"
BRIDGE_DEV="${BRIDGE_DEV:-br-ex}"

# Per-network table: "<vlan-tag> <public-prefix> <tenant-prefix> <mac-octet>"
# Everything else (switch/router/port/netns names, FIPs, MACs) is derived
# from these four fields — see net_vars().
NETWORKS=(
    "101 198.51.100 192.168.101 65"
    "102 203.0.113 192.168.102 66"
)

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"

log() { printf '[multi-vlan] %s\n' "$*" >&2; }

nbctl() { docker exec "${CENTRAL}" ovn-nbctl "$@"; }

# Derive all per-network names from a NETWORKS entry into globals. Called
# at the top of every per-network function so the naming lives in exactly
# one place.
#
#   tag=101 pub=198.51.100 tenant=192.168.101 hex=65 →
#     LS_PUB=ls-vlan101       LN_LSP=ln-vlan101
#     LS_TENANT=ls-vlan101-t  LR=lr-vlan101
#     LRP_PUB=lr-vlan101-public (198.51.100.1/24, 02:00:00:65:02:01)
#     LRP_TEN=lr-vlan101-tenant (192.168.101.1/24, 02:00:00:65:01:01)
#     FIP=198.51.100.10 → FIP_INTERNAL=192.168.101.10
#     VM_LSP/VM_NETNS=vm101, VM_MAC=02:00:00:65:0a:0a
#     SEG_DEV=br-ex.101
net_vars() {
    local tag="$1" pub="$2" tenant="$3" hex="$4"
    TAG="${tag}"
    LS_PUB="ls-vlan${tag}"
    LN_LSP="ln-vlan${tag}"
    LS_TENANT="ls-vlan${tag}-t"
    LR="lr-vlan${tag}"
    LRP_PUB="lr-vlan${tag}-public"
    LRP_PUB_MAC="02:00:00:${hex}:02:01"
    LRP_PUB_CIDR="${pub}.1/24"
    LRP_TEN="lr-vlan${tag}-tenant"
    LRP_TEN_MAC="02:00:00:${hex}:01:01"
    LRP_TEN_CIDR="${tenant}.1/24"
    VGW_IP="${pub}.254"
    FIP="${pub}.10"
    FIP_INTERNAL="${tenant}.10"
    VM_LSP="vm${tag}"
    VM_NETNS="vm${tag}"
    VM_MAC="02:00:00:${hex}:0a:0a"
    VM_HOST_VETH="vm${tag}-host"
    VM_NS_VETH="vm${tag}-eth0"
    WORKLOAD_GW="${tenant}.1"
    SEG_DEV="${BRIDGE_DEV}.${tag}"
}

# Write `content` (read from stdin) to `path` under ARTIFACTS_DIR when
# it is set; quietly no-op otherwise. Mirrors hairpin.sh.
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

# Dump the agent's MAC-tweak flows on MASTER's br-ex.
dump_mactweak_flows() {
    docker exec "${MASTER_NODE}" \
        ovs-ofctl --no-stats dump-flows "${BRIDGE_DEV}" 2>/dev/null \
        | grep 'cookie=0x999' || true
}

# Provision one VLAN provider network + router + tenant network +
# workload. All NB calls are idempotent (--may-exist / value-replacing).
#
# Deliberately NO default route and NO nexthop MAC binding are seeded
# here (unlike bootstrap.sh's flat network): the networks are
# gatewayless, and providing 0.0.0.0/0 via <pub>.254 plus the matching
# Static_MAC_Binding with the segment interface's MAC is exactly the
# agent behaviour this scenario exercises.
ensure_network() {
    net_vars "$@"

    log "ensuring VLAN network ${LS_PUB} (tag ${TAG} on ${PHYSNET}) + router ${LR}"
    nbctl --may-exist ls-add "${LS_PUB}"
    nbctl --may-exist lsp-add "${LS_PUB}" "${LN_LSP}"
    nbctl lsp-set-type      "${LN_LSP}" localnet
    nbctl lsp-set-addresses "${LN_LSP}" unknown
    nbctl lsp-set-options   "${LN_LSP}" network_name="${PHYSNET}"
    # Neutron's OVN driver sets the segmentation ID on the localnet
    # port's `tag` column; ovn-northd propagates it to the SB
    # Port_Binding, where the agent reads it.
    nbctl set Logical_Switch_Port "${LN_LSP}" tag="${TAG}"

    nbctl --may-exist ls-add "${LS_TENANT}"
    nbctl --may-exist lr-add "${LR}"

    nbctl --may-exist lrp-add "${LR}" "${LRP_TEN}" "${LRP_TEN_MAC}" "${LRP_TEN_CIDR}"
    nbctl --may-exist lsp-add "${LS_TENANT}" "${LS_TENANT}-${LR}"
    nbctl lsp-set-type      "${LS_TENANT}-${LR}" router
    nbctl lsp-set-addresses "${LS_TENANT}-${LR}" router
    nbctl lsp-set-options   "${LS_TENANT}-${LR}" router-port="${LRP_TEN}"

    nbctl --may-exist lrp-add "${LR}" "${LRP_PUB}" "${LRP_PUB_MAC}" "${LRP_PUB_CIDR}"
    nbctl --may-exist lsp-add "${LS_PUB}" "${LS_PUB}-${LR}"
    nbctl lsp-set-type      "${LS_PUB}-${LR}" router
    nbctl lsp-set-addresses "${LS_PUB}-${LR}" router
    nbctl lsp-set-options   "${LS_PUB}-${LR}" router-port="${LRP_PUB}"

    log "pinning ${LRP_PUB} to ${MASTER}"
    nbctl lrp-set-gateway-chassis "${LRP_PUB}" "${MASTER}" 30

    log "ensuring FIP ${FIP} → ${FIP_INTERNAL} on ${LR}"
    nbctl --may-exist lr-nat-add "${LR}" dnat_and_snat "${FIP}" "${FIP_INTERNAL}"

    log "ensuring workload LSP ${VM_LSP} on ${LS_TENANT}"
    nbctl --may-exist lsp-add "${LS_TENANT}" "${VM_LSP}"
    nbctl lsp-set-addresses "${VM_LSP}" "${VM_MAC} ${FIP_INTERNAL}"
}

# Provision the netns responder for one network on WORKLOAD_HOST —
# the ensure_workload_netns pattern from bootstrap.sh.
ensure_responder() {
    net_vars "$@"
    log "provisioning ${VM_NETNS} responder on ${WORKLOAD_HOST} (${FIP_INTERNAL}/24)"
    docker exec -i \
        --env "VM_NETNS=${VM_NETNS}" \
        --env "VM_LSP=${VM_LSP}" \
        --env "VM_MAC=${VM_MAC}" \
        --env "VM_IP_CIDR=${FIP_INTERNAL}/24" \
        --env "VM_GW=${WORKLOAD_GW}" \
        --env "VM_HOST_VETH=${VM_HOST_VETH}" \
        --env "VM_NS_VETH=${VM_NS_VETH}" \
        "${WORKLOAD_NODE}" sh -eu <<'EOSH'
if ! ip link show "${VM_HOST_VETH}" >/dev/null 2>&1; then
    ip link add "${VM_HOST_VETH}" type veth peer name "${VM_NS_VETH}"
fi
ovs-vsctl --may-exist add-port br-int "${VM_HOST_VETH}" \
    -- set Interface "${VM_HOST_VETH}" external_ids:iface-id="${VM_LSP}"
ip link set "${VM_HOST_VETH}" up

# Enter the namespace rather than asking `ip netns list` whether it exists:
# a container restart leaves a dead anchor under /run/netns that keeps it
# listed while every use of it fails. See ensure_workload_netns in
# bootstrap.sh for the full rationale.
if ! ip netns exec "${VM_NETNS}" true 2>/dev/null; then
    ip netns delete "${VM_NETNS}" 2>/dev/null || true
    ip netns add "${VM_NETNS}"
fi
if ! ip -n "${VM_NETNS}" link show "${VM_NS_VETH}" >/dev/null 2>&1; then
    ip link set "${VM_NS_VETH}" netns "${VM_NETNS}"
fi
ip -n "${VM_NETNS}" link set lo up
ip -n "${VM_NETNS}" link set "${VM_NS_VETH}" address "${VM_MAC}"
ip -n "${VM_NETNS}" link set "${VM_NS_VETH}" up
ip -n "${VM_NETNS}" addr replace "${VM_IP_CIDR}" dev "${VM_NS_VETH}"
ip -n "${VM_NETNS}" route replace default via "${VM_GW}"
EOSH
}

# Remove everything ensure_network/ensure_responder added for one
# network. Best-effort and idempotent.
teardown_network() {
    net_vars "$@"
    log "teardown: removing ${LR}, ${LS_PUB}, ${LS_TENANT}, ${VM_NETNS}"
    docker exec -i \
        --env "VM_NETNS=${VM_NETNS}" \
        --env "VM_HOST_VETH=${VM_HOST_VETH}" \
        "${WORKLOAD_NODE}" sh -u <<'EOSH' || true
ovs-vsctl --if-exists del-port br-int "${VM_HOST_VETH}" || true
if ip link show "${VM_HOST_VETH}" >/dev/null 2>&1; then
    ip link delete "${VM_HOST_VETH}" || true
fi
if ip netns list | awk '{print $1}' | grep -qx "${VM_NETNS}"; then
    ip netns delete "${VM_NETNS}" || true
fi
EOSH
    # lr-del cascades the NAT rows and the agent's managed default route;
    # ls-del cascades the LSPs (incl. the localnet port). The agent's MAC
    # binding for the virtual gateway is a root-table row and must go
    # explicitly. The agent prunes br-ex.<tag>, its routes and flows on
    # MASTER on the next reconcile once the router is gone.
    nbctl --if-exists lr-del "${LR}" || true
    nbctl --if-exists ls-del "${LS_PUB}" || true
    nbctl --if-exists ls-del "${LS_TENANT}" || true
    nbctl static-mac-binding-del "${LRP_PUB}" "${VGW_IP}" >/dev/null 2>&1 || true
}

teardown_all() {
    local entry
    for entry in "${NETWORKS[@]}"; do
        # shellcheck disable=SC2086  # word-splitting the table row is intended
        teardown_network ${entry}
    done
}

# Poll until `docker exec MASTER_NODE <cmd...>` succeeds or the
# reconcile window expires.
wait_on_master() {
    local desc="$1"; shift
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    log "waiting up to ${RECONCILE_TIMEOUT}s for ${desc}"
    while (( $(date +%s) < deadline )); do
        if "$@" >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    log "ERROR: ${desc} did not materialise within ${RECONCILE_TIMEOUT}s"
    return 1
}

check_segment_link() {
    docker exec "${MASTER_NODE}" ip link show dev "$1"
}

check_kernel_route() {
    docker exec "${MASTER_NODE}" ip -4 route show "$1/32" dev "$2" \
        | grep -q "$1"
}

# ovn-controller stamps external_ids:ovn-localnet-port=<lsp> on BOTH
# Port rows of the patch pair it creates for a localnet port — the
# br-int side as well as the provider-bridge side — so an unscoped
# `find Port` returns two rows. Scope the lookup to the provider
# bridge's ports, exactly like the agent's segment discovery does.
segment_patch_port() {
    docker exec --env LSP="$1" --env BRIDGE="${BRIDGE_DEV}" "${MASTER_NODE}" sh -eu -c '
        for port in $(ovs-vsctl list-ports "${BRIDGE}"); do
            val="$(ovs-vsctl --if-exists get Port "${port}" \
                external_ids:ovn-localnet-port 2>/dev/null | tr -d "[:space:]\"")" || val=""
            if [ "${val}" = "${LSP}" ]; then
                printf "%s" "${port}"
                exit 0
            fi
        done
        exit 1
    '
}

check_mactweak_flow_for_segment() {
    local lsp="$1"
    local port ofport
    port="$(segment_patch_port "${lsp}")"
    [ -n "${port}" ] || return 1
    ofport="$(docker exec "${MASTER_NODE}" ovs-vsctl get Interface "${port}" ofport | tr -d '[:space:]')"
    [ -n "${ofport}" ] && [ "${ofport}" != "-1" ] || return 1
    dump_mactweak_flows | grep -Eq "in_port=(${ofport}|\"?${port}\"?)([, ]|$)"
}

check_bgp_announcement() {
    docker exec "${UPSTREAM}" vtysh -c "show bgp ipv4 unicast $1/32" 2>/dev/null \
        | grep -q "${MASTER_UNDERLAY_IP}"
}

# Final reachability probe per FIP — non-zero on any packet loss.
probe() {
    local fip="$1"
    log "ping -c ${PING_COUNT} -W ${PING_TIMEOUT} ${fip} from ${CLIENT}"
    docker exec "${CLIENT}" ping -c "${PING_COUNT}" -W "${PING_TIMEOUT}" "${fip}"
}

wait_for_reachability() {
    local fip="$1"
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    log "waiting up to ${RECONCILE_TIMEOUT}s for ${CLIENT} → ${fip} to come up"
    while (( $(date +%s) < deadline )); do
        if docker exec "${CLIENT}" ping -c 1 -W 1 "${fip}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    log "no reply from ${fip} within ${RECONCILE_TIMEOUT}s — falling through to the final probe so its output appears in the log"
    return 0
}

snapshot_artifacts() {
    local phase="$1"
    {
        printf '# cookie=0x999 flows on %s:%s (%s)\n\n' "${MASTER}" "${BRIDGE_DEV}" "${phase}"
        dump_mactweak_flows
        printf '\n# links on %s (%s)\n\n' "${MASTER}" "${phase}"
        docker exec "${MASTER_NODE}" ip -d link show type vlan 2>/dev/null || true
        printf '\n# ipv4 routes on %s (%s)\n\n' "${MASTER}" "${phase}"
        docker exec "${MASTER_NODE}" ip -4 route show 2>/dev/null || true
    } | write_artifact "multi-vlan-${phase}.txt"
}

main() {
    sanity_gate
    snapshot_artifacts "before"

    trap teardown_all EXIT
    local entry
    for entry in "${NETWORKS[@]}"; do
        # shellcheck disable=SC2086
        ensure_network ${entry}
        # shellcheck disable=SC2086
        ensure_responder ${entry}
    done

    # Per-segment kernel + OVS state on the master chassis.
    for entry in "${NETWORKS[@]}"; do
        # shellcheck disable=SC2086
        net_vars ${entry}
        wait_on_master "segment interface ${SEG_DEV} on ${MASTER}" \
            check_segment_link "${SEG_DEV}"
        wait_on_master "kernel route ${FIP}/32 on ${SEG_DEV}" \
            check_kernel_route "${FIP}" "${SEG_DEV}"
        wait_on_master "MAC-tweak flow on ${LN_LSP}'s patch port" \
            check_mactweak_flow_for_segment "${LN_LSP}"
        wait_on_master "BGP announcement of ${FIP}/32 via ${MASTER_UNDERLAY_IP}" \
            check_bgp_announcement "${FIP}"
    done

    snapshot_artifacts "after"

    # End-to-end reachability for both VLAN FIPs.
    for entry in "${NETWORKS[@]}"; do
        # shellcheck disable=SC2086
        net_vars ${entry}
        wait_for_reachability "${FIP}"
        probe "${FIP}"
    done

    # AC 2 coexistence: the flat baseline FIP must still be green while
    # both VLAN networks are active on the same node.
    log "verifying flat baseline FIP ${BASELINE_FIP} still works alongside the VLAN segments"
    probe "${BASELINE_FIP}"
}

main "$@"

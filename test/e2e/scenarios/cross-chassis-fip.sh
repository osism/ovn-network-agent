#!/usr/bin/env bash
# Cross-chassis FIP-to-FIP scenario for the containerlab E2E harness.
#
# Goal (issue #265): with FIP routes in the main table (the lab's default —
# gwnode-config.yaml sets no route_table_id), traffic between two FIPs whose
# routers hold their chassisredirect ports on DIFFERENT gateways must be
# delivered. Before #265 the destination gateway's veth-leak source rule
# (`from 192.0.2.0/24 lookup 200`) caught the packet arriving on veth-default
# before the main table was consulted and sent it straight back into
# vrf-provider, where the FRR static for the /32 pointed at veth-default
# again: the packet bounced between the veth ends until its TTL expired. The
# agent's fix is one ingress exception rule, `iif veth-default lookup main`,
# one priority below the leak rules.
#
# Why a separate scenario: every other probe originates outside the provider
# prefix (client-1) or stays on one chassis (the hairpin scenarios, reflected
# inside OVS). Neither path ever enters a second gateway's veth pair, so the
# loop was invisible to CI.
#
# Topology used:
#   * lr0 with FIP_A = 192.0.2.10 → 192.168.10.10 (vm1 on gateway-3) — seeded
#     by bootstrap.sh, cr-lr0-public on MASTER (gateway-1 in the baseline lab)
#   * lr1 — added here — on the same provider switch ls-public, with
#     lr1-public 192.0.2.2/24 pinned to PEER (gateway-2) as its only
#     candidate, tenant switch ls1 (192.168.20.0/24), and
#     FIP_C = 192.0.2.20 → 192.168.20.10 (vm3, also on gateway-3)
#   No default route and no Static_MAC_Binding are seeded for lr1: the agent
#   on PEER programs both (EnsureGatewayRouting in ovn_gateway.go), exactly
#   as it does for lr0.
#
# Packet path on a green run (vm3 → FIP_A):
#   vm3 (gateway-3) → geneve → cr-lr1-public on PEER → SNAT to FIP_C →
#   ls-public localnet → br-ex on PEER → kernel: leak rule → table 200 →
#   veth-default → vrf-provider → BGP → upstream → MASTER's vrf-provider →
#   FRR static FIP_A/32 via the veth pair → veth-default on MASTER →
#   `iif veth-default lookup main` → FIP_A/32 dev br-ex → OVN → DNAT → vm1.
#   The reply takes the mirror image through both kernels.
#
# Assertions:
#   1. cr-lr1-public is bound to PEER and cr-lr0-public to a different
#      chassis — otherwise the run would measure the hairpin path.
#   2. PEER has FIP_C/32 in its kernel table and both gateways carry the
#      ingress exception rule at INGRESS_RULE_PRIORITY.
#   3. ping FIP_A from vm3 and FIP_C from vm1: zero loss.
#   4. PEER's veth-default packet counters grew by at least PING_COUNT across
#      the probes, which pins that the traffic crossed the kernel path and
#      did not stay inside OVN.
#
# Pre-condition: the lab is up and baseline-green (`make e2e-up`). When
# SANITY_GATE=1 (default) this scenario runs baseline.sh first.
#
# Teardown (EXIT trap, best effort): the NAT, the workload LSP, the router
# ports and their switch ports, lr1, ls1, the MAC binding the agent
# programmed for lr1, and the vm3 netns/veth on WORKLOAD_HOST, so a
# subsequent `make e2e-baseline` passes on the same lab. On a failure the
# gateways' rule, veth-counter and route state is dumped first.
#
# Environment overrides (used by the CI workflow):
#   LAB                    container-name prefix (default ovn-e2e)
#   MASTER                 chassis owning cr-lr0-public (default gateway-1)
#   PEER                   chassis lr1-public is pinned to (default gateway-2)
#   WORKLOAD_HOST          chassis hosting vm1 and vm3 (default gateway-3)
#   CENTRAL                OVN central container (default clab-${LAB}-central)
#   FIP_A                  bootstrap FIP behind lr0 (default 192.0.2.10)
#   FIP_C                  FIP added here behind lr1 (default 192.0.2.20)
#   FIP_C_INTERNAL         backing IP for FIP_C (default 192.168.20.10)
#   INGRESS_RULE_PRIORITY  expected priority of the ingress rule (default 1999,
#                          the baked veth_leak_rule_priority minus one)
#   PING_COUNT             packets per direction (default 5)
#   PING_TIMEOUT           per-packet wait, passed to ping -W (default 2)
#   RECONCILE_TIMEOUT      seconds to wait for OVN/agent convergence (default 60)
#   ARTIFACTS_DIR          directory for the failure dumps (default empty = skip)
#   SANITY_GATE            run baseline.sh first when 1 (default 1)

set -euo pipefail

LAB="${LAB:-ovn-e2e}"
MASTER="${MASTER:-gateway-1}"
PEER="${PEER:-gateway-2}"
WORKLOAD_HOST="${WORKLOAD_HOST:-gateway-3}"
CENTRAL="${CENTRAL:-clab-${LAB}-central}"
MASTER_NODE="clab-${LAB}-${MASTER}"
PEER_NODE="clab-${LAB}-${PEER}"
WORKLOAD_NODE="clab-${LAB}-${WORKLOAD_HOST}"

LR0_CR_PORT="${LR0_CR_PORT:-cr-lr0-public}"
FIP_A="${FIP_A:-192.0.2.10}"
FIP_A_NETNS="${FIP_A_NETNS:-vm1}"

LS1="${LS1:-ls1}"
LR1="${LR1:-lr1}"
LRP_TENANT="${LRP_TENANT:-lr1-ls1}"
LRP_TENANT_MAC="${LRP_TENANT_MAC:-02:00:00:00:14:01}"
LRP_TENANT_CIDR="${LRP_TENANT_CIDR:-192.168.20.1/24}"
LRP_PUBLIC="${LRP_PUBLIC:-lr1-public}"
LRP_PUBLIC_MAC="${LRP_PUBLIC_MAC:-02:00:00:00:14:02}"
LRP_PUBLIC_CIDR="${LRP_PUBLIC_CIDR:-192.0.2.2/24}"
LS_PUBLIC="${LS_PUBLIC:-ls-public}"
# The virtual gateway the agent programs on lr1-public (last usable IP of
# the provider /24, the same one bootstrap.sh seeds for lr0). Only the
# teardown needs it, to drop the Static_MAC_Binding the agent created.
VGW_IP="${VGW_IP:-192.0.2.254}"
FIP_C="${FIP_C:-192.0.2.20}"
FIP_C_INTERNAL="${FIP_C_INTERNAL:-192.168.20.10}"
FIP_C_LSP="${FIP_C_LSP:-ls1-vm3}"
FIP_C_MAC="${FIP_C_MAC:-02:00:00:00:14:0a}"
FIP_C_NETNS="${FIP_C_NETNS:-vm3}"
FIP_C_HOST_VETH="${FIP_C_HOST_VETH:-vm3-host}"
FIP_C_NS_VETH="${FIP_C_NS_VETH:-vm3-eth0}"
WORKLOAD_GW="${WORKLOAD_GW:-192.168.20.1}"
WORKLOAD_CIDR_LEN="${WORKLOAD_CIDR_LEN:-24}"
INGRESS_RULE_PRIORITY="${INGRESS_RULE_PRIORITY:-1999}"

PING_COUNT="${PING_COUNT:-5}"
PING_TIMEOUT="${PING_TIMEOUT:-2}"
RECONCILE_TIMEOUT="${RECONCILE_TIMEOUT:-60}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-}"
SANITY_GATE="${SANITY_GATE:-1}"

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"

log() { printf '[cross-chassis-fip] %s\n' "$*" >&2; }
nbctl() { docker exec "${CENTRAL}" ovn-nbctl "$@"; }
sbctl() { docker exec "${CENTRAL}" ovn-sbctl "$@"; }

# Write stdin to `path` under ARTIFACTS_DIR when it is set; quietly no-op
# otherwise. Same shape as the other scenarios.
write_artifact() {
    local path="$1"
    if [ -z "${ARTIFACTS_DIR}" ]; then
        cat >/dev/null
        return 0
    fi
    mkdir -p "${ARTIFACTS_DIR}"
    cat >"${ARTIFACTS_DIR}/${path}"
}

# Parse `N packets transmitted, M received` out of a ping summary block and
# print the integer loss (transmitted - received). Returns non-zero when the
# line is missing or the two fields are not integers, which the caller
# reports as a probe failure. Own copy, like drain-hitless.sh keeps one: the
# numeric guard matters because BusyBox prints `M packets received,` where
# iputils prints `M received,`, and a naive field pick would coerce the word
# to 0 and report a fabricated full loss.
parse_ping_loss() {
    local file="$1"
    awk '
        /packets transmitted/ {
            tx = $1
            for (i = 1; i <= NF; i++) {
                if ($i == "received,") {
                    rx = $(i-1)
                    break
                }
            }
            if (tx !~ /^[0-9]+$/ || rx !~ /^[0-9]+$/) exit 1
            print tx - rx
            found = 1
            exit 0
        }
        END { if (!found) exit 1 }
    ' "${file}"
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

# Seed the second router on the shared provider switch. Every add is
# --may-exist and every set replaces its value, so re-running is a no-op.
ensure_router() {
    log "ensuring ${LR1} on ${LS_PUBLIC} (${LRP_PUBLIC_CIDR}) with tenant switch ${LS1} (${LRP_TENANT_CIDR})"
    nbctl --may-exist ls-add "${LS1}"
    nbctl --may-exist lr-add "${LR1}"

    nbctl --may-exist lrp-add "${LR1}" "${LRP_TENANT}" "${LRP_TENANT_MAC}" "${LRP_TENANT_CIDR}"
    nbctl --may-exist lsp-add "${LS1}" "${LS1}-${LR1}"
    nbctl lsp-set-type      "${LS1}-${LR1}" router
    nbctl lsp-set-addresses "${LS1}-${LR1}" router
    nbctl lsp-set-options   "${LS1}-${LR1}" router-port="${LRP_TENANT}"

    nbctl --may-exist lrp-add "${LR1}" "${LRP_PUBLIC}" "${LRP_PUBLIC_MAC}" "${LRP_PUBLIC_CIDR}"
    nbctl --may-exist lsp-add "${LS_PUBLIC}" "${LS_PUBLIC}-${LR1}"
    nbctl lsp-set-type      "${LS_PUBLIC}-${LR1}" router
    nbctl lsp-set-addresses "${LS_PUBLIC}-${LR1}" router
    nbctl lsp-set-options   "${LS_PUBLIC}-${LR1}" router-port="${LRP_PUBLIC}"

    log "pinning ${LRP_PUBLIC} to ${PEER} as its only candidate"
    nbctl lrp-set-gateway-chassis "${LRP_PUBLIC}" "${PEER}" 30

    log "ensuring FIP ${FIP_C} → ${FIP_C_INTERNAL} on ${LR1}"
    nbctl --may-exist lr-nat-add "${LR1}" dnat_and_snat "${FIP_C}" "${FIP_C_INTERNAL}"

    log "ensuring workload LSP ${FIP_C_LSP} on ${LS1}"
    nbctl --may-exist lsp-add "${LS1}" "${FIP_C_LSP}"
    nbctl lsp-set-addresses "${FIP_C_LSP}" "${FIP_C_MAC} ${FIP_C_INTERNAL}"
}

# Provision the FIP_C responder on WORKLOAD_HOST: a veth pair with one end
# attached to br-int (carrying iface-id=ls1-vm3 so ovn-controller binds the
# LSP to this chassis), the other end in a netns with the workload IP and a
# default route to lr1. Mirrors ensure_responder in multi-vlan.sh.
ensure_responder() {
    log "provisioning ${FIP_C_NETNS} responder on ${WORKLOAD_HOST} (${FIP_C_INTERNAL}/${WORKLOAD_CIDR_LEN})"
    docker exec -i \
        --env "FIP_C_NETNS=${FIP_C_NETNS}" \
        --env "FIP_C_LSP=${FIP_C_LSP}" \
        --env "FIP_C_MAC=${FIP_C_MAC}" \
        --env "FIP_C_INTERNAL=${FIP_C_INTERNAL}" \
        --env "WORKLOAD_GW=${WORKLOAD_GW}" \
        --env "WORKLOAD_CIDR_LEN=${WORKLOAD_CIDR_LEN}" \
        --env "FIP_C_HOST_VETH=${FIP_C_HOST_VETH}" \
        --env "FIP_C_NS_VETH=${FIP_C_NS_VETH}" \
        "${WORKLOAD_NODE}" sh -eu <<'EOSH'
if ! ip link show "${FIP_C_HOST_VETH}" >/dev/null 2>&1; then
    ip link add "${FIP_C_HOST_VETH}" type veth peer name "${FIP_C_NS_VETH}"
fi
ovs-vsctl --may-exist add-port br-int "${FIP_C_HOST_VETH}" \
    -- set Interface "${FIP_C_HOST_VETH}" external_ids:iface-id="${FIP_C_LSP}"
ip link set "${FIP_C_HOST_VETH}" up

# Enter the namespace rather than asking `ip netns list` whether it exists:
# a container restart leaves a dead anchor under /run/netns that keeps it
# listed while every use of it fails. See ensure_workload_netns in
# bootstrap.sh for the full rationale.
if ! ip netns exec "${FIP_C_NETNS}" true 2>/dev/null; then
    ip netns delete "${FIP_C_NETNS}" 2>/dev/null || true
    ip netns add "${FIP_C_NETNS}"
fi
if ! ip -n "${FIP_C_NETNS}" link show "${FIP_C_NS_VETH}" >/dev/null 2>&1; then
    ip link set "${FIP_C_NS_VETH}" netns "${FIP_C_NETNS}"
fi
ip -n "${FIP_C_NETNS}" link set lo up
ip -n "${FIP_C_NETNS}" link set "${FIP_C_NS_VETH}" address "${FIP_C_MAC}"
ip -n "${FIP_C_NETNS}" link set "${FIP_C_NS_VETH}" up
ip -n "${FIP_C_NETNS}" addr replace \
    "${FIP_C_INTERNAL}/${WORKLOAD_CIDR_LEN}" dev "${FIP_C_NS_VETH}"
ip -n "${FIP_C_NETNS}" route replace default via "${WORKLOAD_GW}"
EOSH
}

# Poll a check up to RECONCILE_TIMEOUT. The check's output is discarded; the
# description carries the failure message.
wait_for() {
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

chassis_uuid() {
    sbctl --bare --columns=_uuid find Chassis "name=$1" | tr -d '[:space:]'
}

cr_port_chassis() {
    sbctl --bare --columns=chassis find Port_Binding "logical_port=$1" | tr -d '[:space:]'
}

# check_cr_port_on <cr-port> <chassis-name>
check_cr_port_on() {
    local want got
    want="$(chassis_uuid "$2")"
    got="$(cr_port_chassis "$1")"
    [ -n "${want}" ] && [ "${got}" = "${want}" ]
}

# The scenario measures the cross-chassis path only while the two routers'
# chassisredirect ports live on different gateways. A shared chassis would
# turn every probe into the same-chassis hairpin path, which the hairpin
# scenarios already cover — fail loudly rather than pass for the wrong reason.
assert_distinct_chassis() {
    local lr0 lr1 name
    lr0="$(cr_port_chassis "${LR0_CR_PORT}")"
    lr1="$(cr_port_chassis "cr-${LRP_PUBLIC}")"
    if [ -z "${lr0}" ] || [ -z "${lr1}" ]; then
        log "ERROR: ${LR0_CR_PORT} (${lr0:-unbound}) or cr-${LRP_PUBLIC} (${lr1:-unbound}) is unbound"
        return 1
    fi
    if [ "${lr0}" = "${lr1}" ]; then
        name="$(sbctl --bare --columns=name list Chassis "${lr0}" | tr -d '[:space:]')"
        log "ERROR: ${LR0_CR_PORT} and cr-${LRP_PUBLIC} are both bound to ${name:-${lr0}}: this run would measure the same-chassis hairpin path, not the cross-chassis one"
        return 1
    fi
    log "${LR0_CR_PORT} and cr-${LRP_PUBLIC} are on different chassis"
}

# check_kernel_route <node> <ip>
check_kernel_route() {
    docker exec "$1" ip -4 route show "$2/32" | grep -q "$2"
}

# check_ingress_rule <node>: the agent's exception rule, as `ip rule show`
# prints it — "1999:	from all iif veth-default lookup main".
check_ingress_rule() {
    docker exec "$1" ip rule show \
        | grep -E "^${INGRESS_RULE_PRIORITY}:.*iif veth-default.*lookup main"
}

# check_reply <node> <netns> <target>: one echo reply, used to absorb the
# agents' convergence (FRR static, BGP propagation) before the strict probe.
check_reply() {
    docker exec "$1" ip netns exec "$2" ping -c 1 -W 1 "$3"
}

# veth_packets <node>: rx + tx packets of veth-default, from the two counter
# lines `ip -s link show` prints under its RX:/TX: headers.
veth_packets() {
    docker exec "$1" ip -s link show veth-default \
        | awk '/RX:/ { getline; rx = $2 } /TX:/ { getline; tx = $2 } END { print rx + tx }'
}

# probe <node> <netns> <target> <label>: PING_COUNT packets, zero loss.
probe() {
    local node="$1" netns="$2" target="$3" label="$4"
    local out loss
    log "${label}: ping -c ${PING_COUNT} -W ${PING_TIMEOUT} ${target} from ${node} netns ${netns}"
    out="$(mktemp)"
    docker exec "${node}" ip netns exec "${netns}" \
        ping -c "${PING_COUNT}" -W "${PING_TIMEOUT}" "${target}" 2>&1 | tee "${out}" >&2 || true
    if ! loss="$(parse_ping_loss "${out}")"; then
        rm -f "${out}"
        log "ERROR: ${label}: could not parse the ping summary"
        return 1
    fi
    rm -f "${out}"
    if [ "${loss}" -ne 0 ]; then
        log "ERROR: ${label}: ${loss} of ${PING_COUNT} packets lost"
        return 1
    fi
    log "${label}: ${PING_COUNT}/${PING_COUNT} replies"
}

# The state a loop or a missing rule shows up in, from both gateways.
dump_state() {
    local chassis node
    for chassis in "${MASTER}" "${PEER}"; do
        node="clab-${LAB}-${chassis}"
        {
            printf '# %s: ip rule show\n\n' "${chassis}"
            docker exec "${node}" ip rule show 2>&1 || true
            printf '\n# %s: ip -s link show veth-default\n\n' "${chassis}"
            docker exec "${node}" ip -s link show veth-default 2>&1 || true
            printf '\n# %s: ip route show table 200\n\n' "${chassis}"
            docker exec "${node}" ip route show table 200 2>&1 || true
            printf '\n# %s: ip route show\n\n' "${chassis}"
            docker exec "${node}" ip route show 2>&1 || true
        } | write_artifact "cross-chassis-fip-${chassis}.txt"
    done
}

teardown() {
    log "teardown: removing ${FIP_C_NETNS} responder, FIP ${FIP_C}, ${LR1} and ${LS1}"
    docker exec -i \
        --env "FIP_C_NETNS=${FIP_C_NETNS}" \
        --env "FIP_C_HOST_VETH=${FIP_C_HOST_VETH}" \
        "${WORKLOAD_NODE}" sh -u <<'EOSH' || true
ovs-vsctl --if-exists del-port br-int "${FIP_C_HOST_VETH}" || true
if ip link show "${FIP_C_HOST_VETH}" >/dev/null 2>&1; then
    ip link delete "${FIP_C_HOST_VETH}" || true
fi
if ip netns list | awk '{print $1}' | grep -qx "${FIP_C_NETNS}"; then
    ip netns delete "${FIP_C_NETNS}" || true
fi
EOSH
    nbctl --if-exists lr-nat-del "${LR1}" dnat_and_snat "${FIP_C}" || true
    nbctl --if-exists lsp-del "${FIP_C_LSP}" || true
    nbctl --if-exists lsp-del "${LS_PUBLIC}-${LR1}" || true
    nbctl --if-exists lsp-del "${LS1}-${LR1}" || true
    nbctl --if-exists lrp-del "${LRP_PUBLIC}" || true
    nbctl --if-exists lrp-del "${LRP_TENANT}" || true
    nbctl --if-exists lr-del "${LR1}" || true
    nbctl --if-exists ls-del "${LS1}" || true
    # The agent on PEER programmed this binding for lr1's virtual gateway;
    # lr-del does not cascade to Static_MAC_Binding.
    nbctl static-mac-binding-del "${LRP_PUBLIC}" "${VGW_IP}" >/dev/null 2>&1 || true
}

on_exit() {
    local rc=$?
    if [ "${rc}" -ne 0 ]; then
        log "scenario failed (exit ${rc}); dumping gateway state before teardown"
        dump_state
    fi
    teardown
    exit "${rc}"
}

main() {
    sanity_gate

    trap on_exit EXIT
    ensure_router
    ensure_responder

    wait_for "cr-${LRP_PUBLIC} bound to ${PEER}" \
        check_cr_port_on "cr-${LRP_PUBLIC}" "${PEER}"
    assert_distinct_chassis
    wait_for "kernel route ${FIP_C}/32 on ${PEER}" \
        check_kernel_route "${PEER_NODE}" "${FIP_C}"
    wait_for "ingress rule at priority ${INGRESS_RULE_PRIORITY} on ${MASTER}" \
        check_ingress_rule "${MASTER_NODE}"
    wait_for "ingress rule at priority ${INGRESS_RULE_PRIORITY} on ${PEER}" \
        check_ingress_rule "${PEER_NODE}"
    wait_for "a first reply from ${FIP_A} inside ${FIP_C_NETNS}" \
        check_reply "${WORKLOAD_NODE}" "${FIP_C_NETNS}" "${FIP_A}"

    local before after delta
    before="$(veth_packets "${PEER_NODE}")"
    probe "${WORKLOAD_NODE}" "${FIP_C_NETNS}" "${FIP_A}" "${FIP_C_NETNS} → FIP_A"
    probe "${WORKLOAD_NODE}" "${FIP_A_NETNS}" "${FIP_C}" "${FIP_A_NETNS} → FIP_C"
    after="$(veth_packets "${PEER_NODE}")"
    delta=$(( after - before ))
    if [ "${delta}" -lt "${PING_COUNT}" ]; then
        log "ERROR: ${PEER}'s veth-default packet counters grew by ${delta} across ${PING_COUNT}-packet probes (want at least ${PING_COUNT}): the traffic did not cross the kernel path"
        return 1
    fi
    log "${PEER}'s veth-default packet counters grew by ${delta} across the probes: the traffic crossed the kernel path on both gateways"
}

main "$@"

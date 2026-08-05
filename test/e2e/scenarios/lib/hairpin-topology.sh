# shellcheck shell=bash
# FIP_B topology helpers for the same-chassis hairpin scenarios.
#
# hairpin.sh (issue #108) owned this topology outright: the second FIP on
# the master chassis, its backing LSP, the responder netns on the workload
# host, and the two observers that read the agent's `cookie=0x998` flows
# back off `br-ex`. hairpin-churn.sh (issue #243) needs exactly the same
# topology under a different measurement, so it lives here instead of
# being copied.
#
# Sourcing contract:
#
#   * Source it, do not execute it. It defines functions and defaults and
#     runs nothing.
#   * Every variable below is set with `: "${VAR:=default}"`, so an
#     environment override made before the source wins, and a scenario may
#     override one after the source too.
#   * It defines no `log`. Each scenario keeps its own prefix, so a line
#     from the shared helpers still says which scenario emitted it — define
#     `log` before the first call into these functions.
#   * It sets no shell options. `set -euo pipefail` is the sourcing
#     scenario's call.
#
# Variables, all overridable:
#   LAB                 container-name prefix (defaults to the topology name "ovn-e2e")
#   MASTER              chassis owning cr-lr0-public (defaults to gateway-1)
#   WORKLOAD_HOST       chassis hosting the workload netns (defaults to gateway-3)
#   CENTRAL             OVN central container (defaults to clab-${LAB}-central)
#   LR_NAME             logical router (defaults to lr0)
#   LS_NAME             tenant logical switch (defaults to ls0)
#   FIP_B               the FIP these helpers add (default 192.0.2.12)
#   FIP_B_INTERNAL      backing IP for FIP_B (default 192.168.10.12)
#   FIP_B_LSP           NB LSP name for the FIP_B backend (default ls0-vm2)
#   FIP_B_MAC           MAC for the FIP_B backend (default 02:00:00:00:0a:0b)
#   FIP_B_NETNS         netns name on WORKLOAD_HOST for the FIP_B responder (default vm2)
#   FIP_B_HOST_VETH     host-side veth name on WORKLOAD_HOST (default vm2-host)
#   FIP_B_NS_VETH       netns-side veth name (default vm2-eth0)
#   WORKLOAD_GW         tenant gateway IP for the responder (default 192.168.10.1)
#   WORKLOAD_CIDR_LEN   netmask length for FIP_B_INTERNAL on the responder (default 24)
#   RECONCILE_TIMEOUT   seconds to wait for the agent to install a hairpin flow (default 60)
#   ARTIFACTS_DIR       directory write_artifact writes into (default empty = skip)

: "${LAB:=ovn-e2e}"
: "${MASTER:=gateway-1}"
: "${WORKLOAD_HOST:=gateway-3}"
: "${CENTRAL:=clab-${LAB}-central}"
: "${LR_NAME:=lr0}"
: "${LS_NAME:=ls0}"
: "${FIP_B:=192.0.2.12}"
: "${FIP_B_INTERNAL:=192.168.10.12}"
: "${FIP_B_LSP:=ls0-vm2}"
: "${FIP_B_MAC:=02:00:00:00:0a:0b}"
: "${FIP_B_NETNS:=vm2}"
: "${FIP_B_HOST_VETH:=vm2-host}"
: "${FIP_B_NS_VETH:=vm2-eth0}"
: "${WORKLOAD_GW:=192.168.10.1}"
: "${WORKLOAD_CIDR_LEN:=24}"
: "${RECONCILE_TIMEOUT:=60}"
: "${ARTIFACTS_DIR:=}"

# Derived from the two chassis names, and deliberately not overridable on
# their own: a MASTER_NODE that disagreed with MASTER would make every
# error message point at the wrong container.
MASTER_NODE="clab-${LAB}-${MASTER}"
WORKLOAD_NODE="clab-${LAB}-${WORKLOAD_HOST}"

nbctl() { docker exec "${CENTRAL}" ovn-nbctl "$@"; }

# Write `content` (read from stdin) to `path` under ARTIFACTS_DIR when
# it is set; quietly no-op otherwise. Mirrors the helper used by
# stale-chassis.sh so the CI-side ARTIFACTS_DIR plumbing is uniform.
write_artifact() {
    local path="$1"
    if [ -z "${ARTIFACTS_DIR}" ]; then
        cat >/dev/null
        return 0
    fi
    mkdir -p "${ARTIFACTS_DIR}"
    cat >"${ARTIFACTS_DIR}/${path}"
}

# Parse `N packets transmitted, M received` out of a ping summary block
# and print the integer loss (transmitted - received). Returns non-zero
# when the line is missing, or when the two fields are not integers —
# the caller logs either as a probe failure. Lifted from drain-hitless.sh,
# which keeps its own copy.
#
# The numeric guard is not paranoia. iputils prints `200 packets
# transmitted, 199 received,` so the field before `received,` is the
# count, but BusyBox prints `200 packets transmitted, 200 packets
# received,` — there the field before it is the word `packets`, which
# awk coerces to 0 and turns into a full-loss reading. The probe container
# is an environment override, so an Alpine/BusyBox one would report a
# fabricated full loss and blame the agent for an image mismatch. Fail
# loudly instead.
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

# Dump the cookie=0x998 lines from MASTER's br-ex. Empty stdout when
# the agent has not (yet) installed any hairpin flows. Used both for
# the polling wait and for the artifact snapshot.
dump_hairpin_flows() {
    docker exec "${MASTER_NODE}" \
        ovs-ofctl --no-stats dump-flows br-ex 2>/dev/null \
        | grep 'cookie=0x998' || true
}

# Poll for the agent's hairpin flow tagged with the given destination IP
# on MASTER's br-ex. Returns 0 once a matching flow appears and 1 if
# the deadline expires. The agent installs hairpin flows on each
# reconcile cycle once the new NAT is observed in OVN; with a 5s
# reconcile_interval (gwnode-config.yaml) the flow lands well within
# RECONCILE_TIMEOUT in the green case.
wait_for_hairpin_flow() {
    local target="$1"
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    log "waiting up to ${RECONCILE_TIMEOUT}s for hairpin flow dst=${target} on ${MASTER}:br-ex"
    while (( $(date +%s) < deadline )); do
        # OVS dumps the L3 destination match as either `nw_dst=` (legacy
        # form, what the OVS shipped with Ubuntu 24.04 cloud-archive
        # flamingo emits) or `ip_dst=` (OXM-style, newer OVS). Accept
        # both so the match does not depend on which OVS the gwnode
        # image happens to bundle.
        if dump_hairpin_flows | grep -Eq "(nw_dst|ip_dst)=${target}([^0-9]|$)"; then
            return 0
        fi
        sleep 2
    done
    log "no hairpin flow appeared within ${RECONCILE_TIMEOUT}s; current cookie=0x998 dump:"
    dump_hairpin_flows | sed 's/^/    /' >&2
    return 1
}

# Add the LSP for the FIP_B backend on the tenant switch. Idempotent
# via --may-exist; lsp-set-addresses replaces any prior value.
ensure_fip_b_lsp() {
    log "ensuring LSP ${FIP_B_LSP} on ${LS_NAME} (${FIP_B_MAC} ${FIP_B_INTERNAL})"
    nbctl --may-exist lsp-add "${LS_NAME}" "${FIP_B_LSP}"
    nbctl lsp-set-addresses "${FIP_B_LSP}" \
        "${FIP_B_MAC} ${FIP_B_INTERNAL}"
}

# Add the dnat_and_snat NAT for FIP_B → FIP_B_INTERNAL on lr0.
# Idempotent via --may-exist.
ensure_fip_b_nat() {
    log "ensuring FIP ${FIP_B} → ${FIP_B_INTERNAL} on ${LR_NAME}"
    nbctl --may-exist lr-nat-add "${LR_NAME}" \
        dnat_and_snat "${FIP_B}" "${FIP_B_INTERNAL}"
}

# Provision the FIP_B responder on WORKLOAD_HOST: a veth pair with one
# end attached to br-int (carrying iface-id=ls0-vm2 so ovn-controller
# binds the LSP to this chassis), the other end placed in a netns with
# the workload IP and a default route to the LR. Mirrors the
# ensure_workload_netns pattern from bootstrap.sh.
ensure_fip_b_responder() {
    log "provisioning ${FIP_B_NETNS} responder on ${WORKLOAD_HOST} (${FIP_B_INTERNAL}/${WORKLOAD_CIDR_LEN})"
    docker exec -i \
        --env "FIP_B_NETNS=${FIP_B_NETNS}" \
        --env "FIP_B_LSP=${FIP_B_LSP}" \
        --env "FIP_B_MAC=${FIP_B_MAC}" \
        --env "FIP_B_INTERNAL=${FIP_B_INTERNAL}" \
        --env "WORKLOAD_GW=${WORKLOAD_GW}" \
        --env "WORKLOAD_CIDR_LEN=${WORKLOAD_CIDR_LEN}" \
        --env "FIP_B_HOST_VETH=${FIP_B_HOST_VETH}" \
        --env "FIP_B_NS_VETH=${FIP_B_NS_VETH}" \
        "${WORKLOAD_NODE}" sh -eu <<'EOSH'
if ! ip link show "${FIP_B_HOST_VETH}" >/dev/null 2>&1; then
    ip link add "${FIP_B_HOST_VETH}" type veth peer name "${FIP_B_NS_VETH}"
fi
ovs-vsctl --may-exist add-port br-int "${FIP_B_HOST_VETH}" \
    -- set Interface "${FIP_B_HOST_VETH}" external_ids:iface-id="${FIP_B_LSP}"
ip link set "${FIP_B_HOST_VETH}" up

# Enter the namespace rather than asking `ip netns list` whether it exists:
# a container restart leaves a dead anchor under /run/netns that keeps it
# listed while every use of it fails. See ensure_workload_netns in
# bootstrap.sh for the full rationale.
if ! ip netns exec "${FIP_B_NETNS}" true 2>/dev/null; then
    ip netns delete "${FIP_B_NETNS}" 2>/dev/null || true
    ip netns add "${FIP_B_NETNS}"
fi
if ! ip -n "${FIP_B_NETNS}" link show "${FIP_B_NS_VETH}" >/dev/null 2>&1; then
    ip link set "${FIP_B_NS_VETH}" netns "${FIP_B_NETNS}"
fi
ip -n "${FIP_B_NETNS}" link set lo up
ip -n "${FIP_B_NETNS}" link set "${FIP_B_NS_VETH}" address "${FIP_B_MAC}"
ip -n "${FIP_B_NETNS}" link set "${FIP_B_NS_VETH}" up
ip -n "${FIP_B_NETNS}" addr replace \
    "${FIP_B_INTERNAL}/${WORKLOAD_CIDR_LEN}" dev "${FIP_B_NS_VETH}"
ip -n "${FIP_B_NETNS}" route replace default via "${WORKLOAD_GW}"
EOSH
}

# Tear down everything ensure_fip_b_* added. Best-effort and
# idempotent: a teardown failure must not mask the scenario's own
# pass/fail signal, so every step is independently allowed to fail.
teardown_fip_b() {
    log "teardown: removing ${FIP_B_NETNS} responder + LSP + NAT for ${FIP_B}"
    docker exec -i \
        --env "FIP_B_NETNS=${FIP_B_NETNS}" \
        --env "FIP_B_HOST_VETH=${FIP_B_HOST_VETH}" \
        "${WORKLOAD_NODE}" sh -u <<'EOSH' || true
ovs-vsctl --if-exists del-port br-int "${FIP_B_HOST_VETH}" || true
if ip link show "${FIP_B_HOST_VETH}" >/dev/null 2>&1; then
    ip link delete "${FIP_B_HOST_VETH}" || true
fi
if ip netns list | awk '{print $1}' | grep -qx "${FIP_B_NETNS}"; then
    ip netns delete "${FIP_B_NETNS}" || true
fi
EOSH
    nbctl --if-exists lsp-del "${FIP_B_LSP}" || true
    nbctl --if-exists lr-nat-del "${LR_NAME}" dnat_and_snat "${FIP_B}" || true
}

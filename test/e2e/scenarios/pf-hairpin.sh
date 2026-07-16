#!/usr/bin/env bash
# Port-forward hairpin scenario for the containerlab E2E harness.
#
# Goal (issue #110): exercise the agent's `hairpin_masquerade` flag
# end-to-end. With the flag OFF, a client behind a FIP that connects to
# a port-forwarded VIP on the same chassis must NOT establish a TCP
# connection: the backend reply re-enters OVN's pipeline and reaches
# the client with the backend's tenant IP as the source (not the VIP),
# so the client's TCP stack rejects it and the probe times out. With
# the flag ON, the agent emits a `postrouting_snat` masquerade rule on
# the active gateway chassis, the backend replies to the chassis IP,
# the chassis conntrack reverses both NAT layers, and the client sees
# the original VIP as the reply source — the probe succeeds.
#
# Both phases assert the configured behaviour explicitly: the negative
# case must actually fail without the flag, otherwise the flag is not
# load-bearing and a future regression would be invisible.
#
# VIP choice — a deviation from the issue body, which says
# `192.0.2.50:53`. The agent's port_forwards feature performs DNAT in
# the chassis kernel (nftables `prerouting`). For the DNAT to fire,
# the VIP traffic from `vmc` has to leave OVN and transit `gateway-1`'s
# kernel. `192.0.2.50` is inside the provider network `192.0.2.0/24`,
# which is a *connected* route on `lr0`: OVN would deliver VIP traffic
# on the public logical switch and ARP for it there (nothing answers,
# since the VIP only lives on the chassis kernel), so the packet never
# reaches the kernel DNAT. A VIP outside every OVN-connected subnet
# (here `198.51.100.50`, TEST-NET-2) follows `lr0`'s default route out
# through `cr-lr0-public` onto `br-ex`, where the agent's MAC-tweak
# flow hands it to the kernel and `prerouting` DNAT catches it in
# transit. Override `VIP` to retry other addresses.
#
# Topology used:
#   * On gateway-1 (master, owns cr-lr0-public):
#       - loopback1 dummy device in vrf-provider (where the agent
#         publishes the managed VIP /32 — provisioned by
#         gwnode-entrypoint.sh on every container start).
#       - vmc workload netns behind FIP_C=192.0.2.13 → 192.168.10.13,
#         bound to the tenant switch via veth + iface-id=ls0-vmc. The
#         client lives behind a FIP so the masquerade-reversed reply
#         (dst=FIP_C) is caught by the agent's per-FIP `to <FIP>/32`
#         policy rule and steered to br-ex/OVN — a non-FIP client IP
#         would be shadowed by the veth-leak `from <provider>` rule.
#       - A tenant-shim OVS internal port on br-int bound to LSP
#         ls0-shim (192.168.10.99/24). This is the bit that makes the
#         scenario implementable in the lab: once nftables rewrites the
#         destination to vm1's tenant IP, the chassis kernel needs a
#         route into the tenant network — tenant-shim's connected
#         /24 is that route. Without it both phases would time out
#         because the DNAT'd packet would be dropped at the chassis
#         (no route for 192.168.10.0/24 in main), so the masquerade
#         flag would never be load-bearing. See the PR description.
#   * On gateway-3 (workload host): pf-backend listens on vm1:53.
#
# Why two phases that share state instead of two independent runs:
# Each agent restart costs ~20s of container boot + chassis re-binding.
# Sharing the lab/topology setup between phases keeps the scenario
# within issue #110's 7-minute CI budget — only the agent's config
# file changes between phases, and the OVN topology / loopback / shim
# / backend stay in place.
#
# Pre-condition: lab is up. When SANITY_GATE=1 (default) this scenario
# runs `baseline.sh` first so a broken green path is reported as a
# baseline regression instead of being attributed to the hairpin flag.
#
# Teardown (EXIT trap, best-effort):
#   * Restore gateway-1's baseline agent config (drops port_forwards
#     entirely so a follow-up `make e2e-baseline` sees the same state
#     a fresh lab-up does).
#   * Restart gateway-1 so the agent reloads the restored config.
#   * Remove FIP_C, ls0-vmc, ls0-shim, the vmc netns, tenant-shim, the
#     loopback1 dummy and the pf-backend process.
# The CI workflow additionally invokes the same teardown step with
# `if: always()` so a fatal interpreter error here (which skips the
# trap) is still cleaned up.
#
# Environment overrides (used by the CI workflow):
#   LAB                container-name prefix (defaults to the topology name "ovn-e2e")
#   MASTER             chassis owning cr-lr0-public (defaults to gateway-1)
#   WORKLOAD_HOST      chassis hosting the pf-backend backend on vm1 (defaults to gateway-3)
#   CENTRAL            OVN central container (defaults to clab-${LAB}-central)
#   LR_NAME            logical router (defaults to lr0)
#   LS_NAME            tenant logical switch (defaults to ls0)
#   VIP                external VIP that vmc dials (default 198.51.100.50 —
#                      must be outside every OVN-connected subnet; see above)
#   VIP_PORT           external TCP port on the VIP (default 53)
#   BACKEND_IP         vm1's tenant IP (default 192.168.10.10)
#   BACKEND_PORT       TCP port pf-backend listens on inside vm1 (default 53)
#   BACKEND_NETNS      netns name for the backend (default vm1)
#   FIP_C              new FIP added by this scenario (default 192.0.2.13)
#   FIP_C_INTERNAL     backing IP for FIP_C / vmc (default 192.168.10.13)
#   FIP_C_LSP          NB LSP name for the vmc workload (default ls0-vmc)
#   FIP_C_MAC          MAC for vmc (default 02:00:00:00:0a:0c)
#   FIP_C_NETNS        netns name on MASTER for vmc (default vmc)
#   FIP_C_HOST_VETH    host-side veth on MASTER (default vmc-host)
#   FIP_C_NS_VETH      netns-side veth (default vmc-eth0)
#   SHIM_LSP           NB LSP name for the tenant-shim port (default ls0-shim)
#   SHIM_IP            chassis-kernel tenant IP for tenant-shim (default 192.168.10.99)
#   SHIM_MAC           MAC for tenant-shim (default 02:00:00:00:0a:99)
#   SHIM_DEV           OVS internal port name on br-int (default tenant-shim)
#   WORKLOAD_GW        tenant gateway IP for vmc / shim (default 192.168.10.1)
#   WORKLOAD_CIDR_LEN  netmask length for tenant addresses (default 24)
#   AGENT_CONFIG_PATH  in-container agent config path (default /etc/ovn-network-agent/config.yaml)
#   GWNODE_CONFIG      host-side baseline config (defaults to the file next to this script)
#   PROBE_TIMEOUT      seconds for the `timeout` wrapper around the probe (default 5)
#   RECONCILE_TIMEOUT  seconds to wait for agent reconcile + DNAT/masquerade rules (default 60 — see baseline.sh for the cold-runner rationale)
#   RESTART_TIMEOUT    seconds to wait for the gateway container to come back after `docker restart` (default 90)
#   ARTIFACTS_DIR      directory to write nft snapshots into (default empty = skip)
#   SANITY_GATE        run baseline.sh first when 1 (default 1)
#   NFT_TABLE_FAMILY   nftables family for the agent table (default ip — the
#                      issue body says `inet`; the agent actually emits `ip`)
#   NFT_TABLE_NAME     nftables table name (default ovn-network-agent)

set -euo pipefail

LAB="${LAB:-ovn-e2e}"
MASTER="${MASTER:-gateway-1}"
MASTER_NODE="clab-${LAB}-${MASTER}"
WORKLOAD_HOST="${WORKLOAD_HOST:-gateway-3}"
WORKLOAD_NODE="clab-${LAB}-${WORKLOAD_HOST}"
CENTRAL="${CENTRAL:-clab-${LAB}-central}"
LR_NAME="${LR_NAME:-lr0}"
LS_NAME="${LS_NAME:-ls0}"
LR_PUBLIC_PORT="${LR_PUBLIC_PORT:-lr0-public}"
CR_PORT="cr-${LR_PUBLIC_PORT}"

VIP="${VIP:-198.51.100.50}"
VIP_PORT="${VIP_PORT:-53}"
BACKEND_IP="${BACKEND_IP:-192.168.10.10}"
BACKEND_PORT="${BACKEND_PORT:-53}"
BACKEND_NETNS="${BACKEND_NETNS:-vm1}"
BACKEND_LOG="${BACKEND_LOG:-/tmp/pf-hairpin-backend.log}"

FIP_C="${FIP_C:-192.0.2.13}"
FIP_C_INTERNAL="${FIP_C_INTERNAL:-192.168.10.13}"
FIP_C_LSP="${FIP_C_LSP:-ls0-vmc}"
FIP_C_MAC="${FIP_C_MAC:-02:00:00:00:0a:0c}"
FIP_C_NETNS="${FIP_C_NETNS:-vmc}"
FIP_C_HOST_VETH="${FIP_C_HOST_VETH:-vmc-host}"
FIP_C_NS_VETH="${FIP_C_NS_VETH:-vmc-eth0}"

SHIM_LSP="${SHIM_LSP:-ls0-shim}"
SHIM_IP="${SHIM_IP:-192.168.10.99}"
SHIM_MAC="${SHIM_MAC:-02:00:00:00:0a:99}"
SHIM_DEV="${SHIM_DEV:-tenant-shim}"

WORKLOAD_GW="${WORKLOAD_GW:-192.168.10.1}"
WORKLOAD_CIDR_LEN="${WORKLOAD_CIDR_LEN:-24}"

AGENT_CONFIG_PATH="${AGENT_CONFIG_PATH:-/etc/ovn-network-agent/config.yaml}"
PROBE_TIMEOUT="${PROBE_TIMEOUT:-5}"
RECONCILE_TIMEOUT="${RECONCILE_TIMEOUT:-60}"
RESTART_TIMEOUT="${RESTART_TIMEOUT:-90}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-}"
SANITY_GATE="${SANITY_GATE:-1}"
NFT_TABLE_FAMILY="${NFT_TABLE_FAMILY:-ip}"
NFT_TABLE_NAME="${NFT_TABLE_NAME:-ovn-network-agent}"

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_DIR="$(cd "${SCENARIOS_DIR}/.." && pwd)"
GWNODE_CONFIG="${GWNODE_CONFIG:-${E2E_DIR}/gwnode-config.yaml}"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"

log() { printf '[pf-hairpin] %s\n' "$*" >&2; }

nbctl() { docker exec "${CENTRAL}" ovn-nbctl "$@"; }
sbctl() { docker exec "${CENTRAL}" ovn-sbctl "$@"; }

# Write `content` (read from stdin) to `path` under ARTIFACTS_DIR when
# it is set; quietly no-op otherwise. Same shape used by hairpin.sh.
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

# Dump the agent's nftables table from MASTER. Empty stdout when the
# agent has not (yet) installed any rules. Used for the polling waits
# and for the artifact snapshots.
dump_nft_table() {
    docker exec "${MASTER_NODE}" \
        nft list table "${NFT_TABLE_FAMILY}" "${NFT_TABLE_NAME}" 2>/dev/null || true
}

write_nft_snapshot() {
    local label="$1"
    {
        printf '# nft list table %s %s on %s — %s\n\n' \
            "${NFT_TABLE_FAMILY}" "${NFT_TABLE_NAME}" "${MASTER}" "${label}"
        dump_nft_table
    } | write_artifact "nft-${label}.txt"
}

# Add the LSP backing the vmc workload netns on ls0. The MAC + IP must
# match what the netns will claim so OVN's ARP responder answers
# correctly. Idempotent via --may-exist; lsp-set-addresses replaces any
# prior value.
ensure_vmc_lsp() {
    log "ensuring LSP ${FIP_C_LSP} on ${LS_NAME} (${FIP_C_MAC} ${FIP_C_INTERNAL})"
    nbctl --may-exist lsp-add "${LS_NAME}" "${FIP_C_LSP}"
    nbctl lsp-set-addresses "${FIP_C_LSP}" "${FIP_C_MAC} ${FIP_C_INTERNAL}"
}

# Add the dnat_and_snat NAT for FIP_C → FIP_C_INTERNAL on lr0. The
# binding to ${MASTER} happens implicitly because cr-lr0-public lives
# on ${MASTER} (priority-30 master in the baseline lab).
ensure_vmc_fip() {
    log "ensuring FIP ${FIP_C} → ${FIP_C_INTERNAL} on ${LR_NAME}"
    nbctl --may-exist lr-nat-add "${LR_NAME}" dnat_and_snat \
        "${FIP_C}" "${FIP_C_INTERNAL}"
}

# Add the OVN-side LSP for the tenant-shim port. The MAC + IP is what
# OVN's ARP responder serves so that vm1 can reach the shim from
# inside OVN once the agent masquerades the source. Empty
# port_security: ovn-northd applies the configured addresses to the
# ARP responder but does NOT reject ingress packets whose source IP
# differs from ${SHIM_IP}. Phase 1's forward packet carries src=
# ${FIP_C} (post OVN SNAT), which must traverse the shim untouched so
# the asymmetric-routing failure is exposed.
ensure_tenant_shim_lsp() {
    log "ensuring tenant-shim LSP ${SHIM_LSP} (${SHIM_MAC} ${SHIM_IP})"
    nbctl --may-exist lsp-add "${LS_NAME}" "${SHIM_LSP}"
    nbctl lsp-set-addresses "${SHIM_LSP}" "${SHIM_MAC} ${SHIM_IP}"
    nbctl lsp-set-port-security "${SHIM_LSP}" ""
}

# Provision the chassis-side kernel state that `docker restart` wipes:
# the vmc workload netns, both ends of its veth pair (host side bound
# to br-int via iface-id=ls0-vmc), and the kernel side of the OVS
# internal tenant-shim port (IP + MAC). OVS conf.db DOES survive the
# restart, so re-running `ovs-vsctl add-port` is a cheap no-op after
# the first call; the kernel-side bits get re-asserted from scratch
# every time.
#
# The veth and netns are unconditionally torn down before re-creation
# because `docker stop` leaves the netns name listed under
# /var/run/netns but the namespace it referenced is gone — a stale
# entry makes `ip netns exec` fail with EINVAL ("setting the network
# namespace ... failed: Invalid argument"), which previously masked
# itself as a Phase 1 "expected timeout".
provision_chassis_kernel_state() {
    log "(re-)provisioning vmc netns + tenant-shim on ${MASTER}"
    docker exec -i \
        --env "FIP_C_NETNS=${FIP_C_NETNS}" \
        --env "FIP_C_LSP=${FIP_C_LSP}" \
        --env "FIP_C_MAC=${FIP_C_MAC}" \
        --env "FIP_C_INTERNAL=${FIP_C_INTERNAL}" \
        --env "WORKLOAD_GW=${WORKLOAD_GW}" \
        --env "WORKLOAD_CIDR_LEN=${WORKLOAD_CIDR_LEN}" \
        --env "FIP_C_HOST_VETH=${FIP_C_HOST_VETH}" \
        --env "FIP_C_NS_VETH=${FIP_C_NS_VETH}" \
        --env "SHIM_DEV=${SHIM_DEV}" \
        --env "SHIM_LSP=${SHIM_LSP}" \
        --env "SHIM_MAC=${SHIM_MAC}" \
        --env "SHIM_IP=${SHIM_IP}" \
        "${MASTER_NODE}" sh -eu <<'EOSH'
# Always start from a clean slate for the vmc netns + veth pair. The
# OVS conf.db row for the host-side veth is removed first so the
# subsequent --may-exist add-port re-creates it with the iface-id
# pointing at the freshly-bound port.
ovs-vsctl --if-exists del-port br-int "${FIP_C_HOST_VETH}" || true
if ip link show "${FIP_C_HOST_VETH}" >/dev/null 2>&1; then
    ip link delete "${FIP_C_HOST_VETH}" || true
fi
ip netns del "${FIP_C_NETNS}" 2>/dev/null || true

ip link add "${FIP_C_HOST_VETH}" type veth peer name "${FIP_C_NS_VETH}"
ovs-vsctl --may-exist add-port br-int "${FIP_C_HOST_VETH}" \
    -- set Interface "${FIP_C_HOST_VETH}" external_ids:iface-id="${FIP_C_LSP}"
ip link set "${FIP_C_HOST_VETH}" up

ip netns add "${FIP_C_NETNS}"
ip link set "${FIP_C_NS_VETH}" netns "${FIP_C_NETNS}"
ip -n "${FIP_C_NETNS}" link set lo up
ip -n "${FIP_C_NETNS}" link set "${FIP_C_NS_VETH}" address "${FIP_C_MAC}"
ip -n "${FIP_C_NETNS}" link set "${FIP_C_NS_VETH}" up
ip -n "${FIP_C_NETNS}" addr replace \
    "${FIP_C_INTERNAL}/${WORKLOAD_CIDR_LEN}" dev "${FIP_C_NS_VETH}"
ip -n "${FIP_C_NETNS}" route replace default via "${WORKLOAD_GW}"

# tenant-shim: OVS recreates the internal port on each ovs-vswitchd
# startup from conf.db, but the kernel-side MAC and IP do not survive
# `docker restart`. Re-apply them each time. ovs-vsctl is idempotent
# and resolves to a no-op after the first call.
ovs-vsctl --may-exist add-port br-int "${SHIM_DEV}" \
    -- set Interface "${SHIM_DEV}" type=internal \
       external_ids:iface-id="${SHIM_LSP}"
ip link set "${SHIM_DEV}" address "${SHIM_MAC}"
ip link set "${SHIM_DEV}" up
ip addr replace "${SHIM_IP}/${WORKLOAD_CIDR_LEN}" dev "${SHIM_DEV}"
EOSH
}

# Sanity-check that the vmc netns is entry-able. The previous
# implementation conflated "netns missing" with "TCP timed out" — both
# produce non-zero exit from the probe, which silently turned Phase 1
# into a false positive when the netns was stale. Run this before
# every probe so a setup error fails the scenario explicitly instead
# of being absorbed by `assert_phase1_timeout`.
verify_vmc_netns() {
    if ! docker exec "${MASTER_NODE}" \
            ip netns exec "${FIP_C_NETNS}" true 2>/dev/null; then
        log "ERROR: cannot enter ${FIP_C_NETNS} netns on ${MASTER} — chassis kernel state is missing or stale"
        return 1
    fi
}

# Start pf-backend on vm1:${BACKEND_PORT}. Same shape as pf-external.sh
# (which already builds and ships pf-backend); the response body is
# fixed and uninteresting — the only thing the probe asserts is that
# the TCP handshake completes.
start_backend() {
    log "starting pf-backend on ${WORKLOAD_HOST} netns ${BACKEND_NETNS} (:${BACKEND_PORT})"
    docker exec "${WORKLOAD_NODE}" sh -c ": >'${BACKEND_LOG}'"
    docker exec -d "${WORKLOAD_NODE}" \
        ip netns exec "${BACKEND_NETNS}" \
        /usr/local/bin/pf-backend \
            -addr ":${BACKEND_PORT}" \
            -log "${BACKEND_LOG}"
    local deadline
    deadline=$(( $(date +%s) + 10 ))
    while (( $(date +%s) < deadline )); do
        if docker exec "${WORKLOAD_NODE}" \
                ip netns exec "${BACKEND_NETNS}" \
                ss -ltn "sport = :${BACKEND_PORT}" 2>/dev/null \
                | grep -q "LISTEN"; then
            log "pf-backend listening on ${BACKEND_IP}:${BACKEND_PORT}"
            return 0
        fi
        sleep 1
    done
    log "ERROR: pf-backend did not bind on :${BACKEND_PORT} within 10s"
    return 1
}

# Write a fresh /etc/ovn-network-agent/config.yaml into MASTER that
# concatenates the baseline gwnode-config.yaml with a port_forwards
# block whose hairpin_masquerade is set to "${hm}" (true or false).
# The agent reads the file on startup; we restart MASTER below to make
# this take effect.
write_agent_config() {
    local hm="$1"
    log "writing ${AGENT_CONFIG_PATH} on ${MASTER} (port_forwards hairpin_masquerade=${hm})"
    {
        cat "${GWNODE_CONFIG}"
        cat <<EOF

# Injected by test/e2e/scenarios/pf-hairpin.sh (issue #110). Adds a
# single port_forward rule so the agent installs the nftables DNAT
# pipeline on this chassis; the masquerade flag is the load-bearing
# variable between the two phases.
port_forwards:
  - vip: "${VIP}"
    manage_vip: true
    hairpin_masquerade: ${hm}
    rules:
      - proto: tcp
        port: ${VIP_PORT}
        dest_addr: "${BACKEND_IP}"
        dest_port: ${BACKEND_PORT}
EOF
    } | docker exec -i "${MASTER_NODE}" sh -c "cat > '${AGENT_CONFIG_PATH}'"
}

# Replace MASTER's agent config with the host-side baseline so a
# `make e2e-baseline` re-run after this scenario sees the same agent
# state a fresh lab-up does. Best-effort: a failure here is logged but
# does not mask the scenario's own pass/fail signal.
restore_agent_config() {
    log "restoring baseline agent config on ${MASTER}"
    docker exec -i "${MASTER_NODE}" sh -c "cat > '${AGENT_CONFIG_PATH}'" \
        < "${GWNODE_CONFIG}" || true
}

# `docker restart` of the gateway container is the closest equivalent
# to the issue's `systemctl restart ovn-network-agent`: the gwnode
# image runs the agent as tini's only child (the entrypoint exec's
# it) with no service manager, so there is no per-service control. Restart is heavyweight (OVS + ovn-controller
# + FRR re-init) — the scenario's CI budget allows for two such cycles
# (one per phase).
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

# Raise ${MASTER}'s Gateway_Chassis priority above every row in the NB
# so ovn-northd re-elects it — the same move an operator makes to
# migrate a gateway deliberately. Skipped once ${MASTER} already holds
# the group's unique peak, so the polling caller stays idempotent.
# Best-effort: wait_for_master_chassis owns the pass/fail signal.
reassert_master_priority() {
    local mine peak
    mine="$(nbctl --bare --columns=priority find Gateway_Chassis \
        "chassis_name=${MASTER}" 2>/dev/null | head -1 || true)"
    peak="$(nbctl --bare --columns=priority list Gateway_Chassis 2>/dev/null \
        | sort -n | tail -1 || true)"
    [ -n "${peak}" ] || return 0
    if [ -n "${mine}" ] && [ "${mine}" -eq "${peak}" ]; then
        return 0 # already leads; ovn-northd just has not moved the port yet
    fi
    nbctl lrp-set-gateway-chassis "${LR_PUBLIC_PORT}" "${MASTER}" \
        "$(( peak + 10 ))" || true
}

# Wait until cr-lr0-public is bound to ${MASTER} again. The container
# restart drops the priority-30 chassis from the HA election and OVN
# fails over to gateway-2 — whose agent then boosts its own
# Gateway_Chassis priority above ${MASTER}'s configured 30
# (EnsureActivePriorityLead, the anti-flapping guard), so OVN's
# election alone never fails back. Each poll re-asserts ${MASTER}'s
# priority above the group's current max until the SB shows the port
# on ${MASTER}; this converges because once the port moves, the peers
# are no longer active and their boost is skipped. Probing before the
# rebind would test gateway-2's nftables (where we have not configured
# port_forwards), so this gate is essential.
wait_for_master_chassis() {
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    log "waiting up to ${RECONCILE_TIMEOUT}s for ${CR_PORT} to bind back to ${MASTER}"
    while (( $(date +%s) < deadline )); do
        reassert_master_priority
        # Port_Binding.chassis is a UUID reference; resolve it via a
        # second lookup against the Chassis table (same shape as
        # failover.sh's current_master helper).
        local chassis_uuid chassis_name
        chassis_uuid="$(sbctl --bare --columns=chassis find Port_Binding \
            logical_port="${CR_PORT}" 2>/dev/null || true)"
        if [ -n "${chassis_uuid}" ]; then
            chassis_name="$(sbctl --bare --columns=name list Chassis \
                "${chassis_uuid}" 2>/dev/null || true)"
            if [ "${chassis_name}" = "${MASTER}" ]; then
                log "${CR_PORT} bound to ${MASTER}"
                return 0
            fi
        fi
        sleep 2
    done
    log "ERROR: ${CR_PORT} did not bind back to ${MASTER} within ${RECONCILE_TIMEOUT}s"
    return 1
}

# Wait for the agent's prerouting_dnat rule for ${VIP}:${VIP_PORT} to
# appear after the restart. Polling is preferable to a fixed sleep:
# the lab's reconcile_interval is 5s but startup ordering (OVS →
# ovn-controller → FRR → agent → OVN connect → first reconcile) varies
# and a hard sleep would either flake or waste budget.
wait_for_dnat_rule() {
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    log "waiting up to ${RECONCILE_TIMEOUT}s for DNAT rule for ${VIP}:${VIP_PORT} on ${MASTER}"
    while (( $(date +%s) < deadline )); do
        # The agent emits `ip daddr <VIP> tcp dport <PORT> dnat to
        # <BACKEND_IP>:<BACKEND_PORT>`; `nft list table` echoes that
        # back roughly verbatim but can canonicalise spacing or family
        # tokens (e.g. `ct original daddr` vs `ct original ip daddr`).
        # The match below pins the VIP, dport, and DNAT target without
        # locking us to a specific nft version's exact rendering.
        if dump_nft_table \
                | grep -Eq "daddr[[:space:]]+${VIP}[[:space:]].*dport[[:space:]]+${VIP_PORT}[[:space:]].*dnat to[[:space:]]+${BACKEND_IP}:${BACKEND_PORT}"; then
            return 0
        fi
        sleep 2
    done
    log "ERROR: DNAT rule for ${VIP}:${VIP_PORT} did not appear within ${RECONCILE_TIMEOUT}s"
    dump_nft_table | sed 's/^/    /' >&2
    return 1
}

# Match the agent's hairpin masquerade emission, which the agent writes
# as `ip saddr <src-match> ct original daddr <vip> ct status dnat
# masquerade`. nft is allowed to canonicalise the `ct original` family
# keyword (`ct original daddr` vs `ct original ip daddr`) on output, so
# the match accepts either.
hairpin_rule_present() {
    dump_nft_table \
        | grep -Eq "ct original( ip)? daddr[[:space:]]+${VIP}[[:space:]].*masquerade"
}

# Probe ${VIP}:${VIP_PORT} from inside the vmc netns. Uses bash's
# /dev/tcp redirect (built in; works with no extra package), so the
# gwnode image does not need netcat. `timeout 5 ... </dev/tcp/...`
# exits 0 only when the TCP handshake completes; on RST/timeout the
# redirect fails and bash exits non-zero (timeout exits 124 on the
# upper bound).
probe_once() {
    docker exec "${MASTER_NODE}" \
        ip netns exec "${FIP_C_NETNS}" \
        timeout "${PROBE_TIMEOUT}" bash -c "exec 3<>/dev/tcp/${VIP}/${VIP_PORT}"
}

assert_phase1_timeout() {
    log "phase 1: probe ${VIP}:${VIP_PORT} from ${FIP_C_NETNS} (expecting timeout)"
    # Capture stderr too: a stale netns shows up as `setting the
    # network namespace ... failed: Invalid argument` from `ip netns
    # exec`, which is a setup error, not the expected TCP timeout.
    # verify_vmc_netns runs before this in main() and should catch
    # that case, but check the stderr signature here as a belt-and-
    # suspenders against `ip netns` exiting non-zero for a reason
    # other than a real TCP timeout / RST.
    local out rc
    set +e
    out="$(probe_once 2>&1)"
    rc=$?
    set -e
    if [ "${rc}" -eq 0 ]; then
        log "ERROR: phase 1 probe succeeded — hairpin_masquerade:false should have left this asymmetric"
        return 1
    fi
    if printf '%s\n' "${out}" \
            | grep -qE 'setting the network namespace|Cannot open network namespace|No such file or directory'; then
        log "ERROR: phase 1 probe failed because of a netns/setup error, not a TCP timeout"
        printf '%s\n' "${out}" | sed 's/^/    /' >&2
        return 1
    fi
    log "phase 1 probe failed as expected (no masquerade → asymmetric reply path)"
}

assert_phase2_success() {
    log "phase 2: probe ${VIP}:${VIP_PORT} from ${FIP_C_NETNS} (expecting success within ${RECONCILE_TIMEOUT}s)"
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    while (( $(date +%s) < deadline )); do
        if probe_once; then
            log "phase 2 probe succeeded (hairpin_masquerade:true reverses NAT on the chassis)"
            return 0
        fi
        sleep 2
    done
    log "ERROR: phase 2 probe did not succeed within ${RECONCILE_TIMEOUT}s"
    write_nft_snapshot "phase2-failure"
    dump_nft_table | sed 's/^/    /' >&2
    return 1
}

assert_masquerade_rule_present() {
    log "asserting nft table on ${MASTER} contains a masquerade rule for VIP=${VIP}"
    if ! hairpin_rule_present; then
        log "ERROR: no 'ct original daddr ${VIP} ... masquerade' rule observed"
        dump_nft_table | sed 's/^/    /' >&2
        return 1
    fi
    log "masquerade rule for ${VIP} observed in nft table"
}

# Tear down everything the scenario added. Best-effort and idempotent:
# every step is independently allowed to fail so a teardown error does
# not mask the scenario's own pass/fail signal.
teardown() {
    log "teardown: dropping pf-backend, OVN topology, shim, vmc netns"
    write_nft_snapshot "teardown" || true
    docker exec "${WORKLOAD_NODE}" pkill -f /usr/local/bin/pf-backend 2>/dev/null || true
    docker exec "${WORKLOAD_NODE}" rm -f "${BACKEND_LOG}" 2>/dev/null || true

    restore_agent_config

    docker exec -i \
        --env "FIP_C_NETNS=${FIP_C_NETNS}" \
        --env "FIP_C_HOST_VETH=${FIP_C_HOST_VETH}" \
        --env "SHIM_DEV=${SHIM_DEV}" \
        "${MASTER_NODE}" sh -u <<'EOSH' || true
ovs-vsctl --if-exists del-port br-int "${FIP_C_HOST_VETH}" || true
if ip link show "${FIP_C_HOST_VETH}" >/dev/null 2>&1; then
    ip link delete "${FIP_C_HOST_VETH}" || true
fi
if ip netns list | awk '{print $1}' | grep -qx "${FIP_C_NETNS}"; then
    ip netns delete "${FIP_C_NETNS}" || true
fi
ovs-vsctl --if-exists del-port br-int "${SHIM_DEV}" || true
EOSH
    nbctl --if-exists lsp-del "${FIP_C_LSP}" || true
    nbctl --if-exists lsp-del "${SHIM_LSP}" || true
    nbctl --if-exists lr-nat-del "${LR_NAME}" dnat_and_snat "${FIP_C}" || true

    # Bounce ${MASTER} once more so the restored config takes effect
    # for any follow-up scenarios sharing the same lab. Wrapped in `||
    # true` because a failure here must not mask the scenario's signal.
    docker restart "${MASTER_NODE}" >/dev/null 2>&1 || true
}

main() {
    sanity_gate

    trap teardown EXIT

    # OVN-side topology only — these rows live in central's NB DB and
    # survive `docker restart` of the chassis. loopback1 is created
    # unconditionally by gwnode-entrypoint.sh on every container start.
    # The chassis-side kernel state (vmc netns, veth pair, tenant-shim
    # MAC/IP) is wiped on every `docker restart`, so it is provisioned
    # *after* each restart below.
    ensure_vmc_lsp
    ensure_vmc_fip
    ensure_tenant_shim_lsp
    start_backend

    # Phase 1 — hairpin_masquerade: false → probe must fail.
    write_agent_config false
    restart_master
    wait_for_master_chassis
    provision_chassis_kernel_state
    verify_vmc_netns
    wait_for_dnat_rule
    write_nft_snapshot "phase1-off"
    if hairpin_rule_present; then
        log "ERROR: agent emitted a hairpin masquerade rule for ${VIP} while hairpin_masquerade:false — guarding against silent regressions"
        return 1
    fi
    assert_phase1_timeout

    # Phase 2 — hairpin_masquerade: true → probe must succeed.
    write_agent_config true
    restart_master
    wait_for_master_chassis
    provision_chassis_kernel_state
    verify_vmc_netns
    wait_for_dnat_rule
    # The masquerade rule needs the agent's first reconcile to deliver
    # providerNetworks; assert_phase2_success polls anyway, so we just
    # need both the rule and a working data path before declaring
    # success.
    assert_phase2_success
    write_nft_snapshot "phase2-on"
    assert_masquerade_rule_present
}

main "$@"

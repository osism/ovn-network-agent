#!/usr/bin/env bash
# Same-chassis hairpin scenario for the containerlab E2E harness.
#
# Goal (issue #108): with two FIPs hosted on the same active gateway
# chassis (`gateway-1`, the priority-30 master), traffic from a workload
# behind FIP_A to FIP_B must traverse the agent's OpenFlow hairpin rule
# (cookie `0x998`, `actions=output:in_port`) on `br-ex` and complete a
# round trip with 100% reply rate within `RECONCILE_TIMEOUT` (default
# 60s — see baseline.sh for why the cold-runner reconcile window was
# raised from the original 30s).
#
# Why a separate scenario: the baseline only exercises a single FIP on
# the master chassis. The hairpin path is what kicks in when two FIPs
# on the same LR live on the same chassis and one workload reaches the
# other through the public network — without the agent's hairpin flow,
# OVS drops `output:in_port` by default and the second-hop DNAT never
# fires, so a regression in that flow is invisible to baseline.
#
# Topology used:
#   * FIP_A = 192.0.2.10 → 192.168.10.10 (vm1, on gateway-3 — seeded by bootstrap.sh)
#   * FIP_B = 192.0.2.12 → 192.168.10.12 (vm2, also on gateway-3 — added here)
#   Both NATs sit on `lr0`, whose `cr-lr0-public` is bound to gateway-1
#   in the baseline lab; both FIPs therefore live on gateway-1 and
#   require a hairpin flow there.
#
# Why scenario-local (and not a third FIP in bootstrap.sh):
# Per issue #108 option (2), the second backend is added by this script
# and removed on teardown. That keeps the baseline lab minimal — the
# bootstrap stays focused on the smallest topology baseline.sh needs —
# and prevents the hairpin scenario from leaking state into other
# scenarios that share the same lab (failover, stale-chassis).
#
# Why a real responder for FIP_B (and not just the NAT entry):
# OVN does install the FIP regardless of whether a backing LSP exists,
# and the agent installs the hairpin flow regardless of whether the
# FIP has a backend (it keys off the NAT row alone — see agent.go,
# `hairpinMACMap`). So the flow's presence is necessary but not
# sufficient: a green flow with no responder would let a regression
# in OVN's DNAT-on-hairpin-return path go unnoticed. The probe asserts
# end-to-end reachability, which only succeeds when the hairpin flow
# AND the second-hop DNAT both work.
#
# Pre-condition: the lab is up. When SANITY_GATE=1 (default) this
# scenario runs `baseline.sh` first so a broken green path is reported
# as a baseline regression instead of being attributed to hairpin.
#
# Teardown: the LSP, NAT, host-side veth and netns added for FIP_B are
# removed on EXIT (via trap), returning the lab to baseline state. The
# CI workflow additionally invokes the same teardown step with
# `if: always()` so a fatal interpreter error here (which skips the
# trap) is still cleaned up.
#
# The FIP_B topology itself — the LSP, the NAT, the responder, the
# teardown, and the two flow observers — lives in
# lib/hairpin-topology.sh, which hairpin-churn.sh (issue #243) sources
# too. What stays here is this scenario's own measurement: the sanity
# gate, the per-FIP flow assertion, and the five-packet probe.
#
# Environment overrides (used by the CI workflow):
#   LAB                 container-name prefix (defaults to the topology name "ovn-e2e")
#   MASTER              chassis owning cr-lr0-public (defaults to gateway-1)
#   WORKLOAD_HOST       chassis hosting both vm1 and vm2 (defaults to gateway-3)
#   CENTRAL             OVN central container (defaults to clab-${LAB}-central)
#   LR_NAME             logical router (defaults to lr0)
#   LS_NAME             tenant logical switch (defaults to ls0)
#   FIP_A               existing FIP whose workload sources the probe (default 192.0.2.10)
#   FIP_B               new FIP added by this scenario (default 192.0.2.12)
#   FIP_B_INTERNAL      backing IP for FIP_B (default 192.168.10.12)
#   FIP_B_LSP           NB LSP name for the FIP_B backend (default ls0-vm2)
#   FIP_B_MAC           MAC for the FIP_B backend (default 02:00:00:00:0a:0b)
#   FIP_B_NETNS         netns name on WORKLOAD_HOST for the FIP_B responder (default vm2)
#   FIP_B_HOST_VETH     host-side veth name on WORKLOAD_HOST (default vm2-host)
#   FIP_B_NS_VETH       netns-side veth name (default vm2-eth0)
#   WORKLOAD_NETNS      netns the probe runs in — host of FIP_A's workload (default vm1)
#   WORKLOAD_GW         tenant gateway IP for the new responder (default 192.168.10.1)
#   WORKLOAD_CIDR_LEN   netmask length for FIP_B_INTERNAL on the responder (default 24)
#   RECONCILE_TIMEOUT   seconds to wait for the agent to install FIP_B's hairpin flow (default 60)
#   PING_COUNT          packets for the final reachability check (default 5)
#   PING_TIMEOUT        per-packet wait, passed to ping -W (default 2)
#   ARTIFACTS_DIR       directory to write before/after hairpin-flow snapshots (default empty = skip)
#   SANITY_GATE         run baseline.sh first when 1 (default 1)

set -euo pipefail

FIP_A="${FIP_A:-192.0.2.10}"
WORKLOAD_NETNS="${WORKLOAD_NETNS:-vm1}"
PING_COUNT="${PING_COUNT:-5}"
PING_TIMEOUT="${PING_TIMEOUT:-2}"
SANITY_GATE="${SANITY_GATE:-1}"

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"

log() { printf '[hairpin] %s\n' "$*" >&2; }

# The shared FIP_B topology, its two flow observers, and write_artifact.
# It defines no log of its own, so the prefix above is what its lines
# carry — source it after the definition.
# shellcheck source=test/e2e/scenarios/lib/hairpin-topology.sh
. "${SCENARIOS_DIR}/lib/hairpin-topology.sh"

# Run baseline.sh as a sanity gate so a broken green path fails fast
# under the right label. Disabled by SANITY_GATE=0 — useful when
# iterating locally on the hairpin behaviour itself.
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

# Assert that MASTER's br-ex has at least one cookie=0x998 flow with
# `actions=output:in_port` for each expected FIP. The agent emits one
# match per IP (see HairpinFlow in ovs.go); per the issue's
# acceptance criteria the dump must show "at least one matching rule
# per FIP on the chassis".
verify_hairpin_flows() {
    local flows
    flows="$(dump_hairpin_flows)"
    log "current cookie=0x998 flows on ${MASTER}:br-ex:"
    if [ -z "${flows}" ]; then
        log "    <none>"
        log "ERROR: ${MASTER}:br-ex has no cookie=0x998 flows at all"
        return 1
    fi
    printf '%s\n' "${flows}" | sed 's/^/    /' >&2

    local fip missing=""
    for fip in "$@"; do
        # Match on the destination-IP hint and on the action so a
        # priority-tweak, MAC-tweak collision, or accidental drop
        # rewrite of the same cookie can't pass for a hairpin.
        # The destination match is dumped as `nw_dst=` (legacy) or
        # `ip_dst=` (OXM) depending on OVS version; the action is
        # dumped as the OVS-canonical `IN_PORT` shorthand for the
        # `output:in_port` form the agent installs. Accept both
        # spellings on each side so a future OVS bump does not
        # silently turn this assertion into a no-op.
        if ! printf '%s\n' "${flows}" \
                | grep -E "(nw_dst|ip_dst)=${fip}([^0-9]|$)" \
                | grep -Eq 'actions=.*(IN_PORT|output:in_port)'; then
            missing="${missing} ${fip}"
        fi
    done
    if [ -n "${missing}" ]; then
        log "ERROR: missing cookie=0x998 actions=output:in_port flow for FIP(s):${missing}"
        return 1
    fi
}

# Final probe — the exit code of this scenario is the exit code of
# this ping, which is non-zero on any packet loss (per the acceptance
# criterion). Source the probe from inside the FIP_A workload's netns
# so the source IP is FIP_A's logical IP and OVN's egress SNAT picks
# FIP_A as the visible source — the same path a real tenant VM behind
# FIP_A would take.
probe() {
    log "ping -c ${PING_COUNT} -W ${PING_TIMEOUT} ${FIP_B} from ${WORKLOAD_HOST} netns ${WORKLOAD_NETNS}"
    docker exec "${WORKLOAD_NODE}" \
        ip netns exec "${WORKLOAD_NETNS}" \
        ping -c "${PING_COUNT}" -W "${PING_TIMEOUT}" "${FIP_B}"
}

main() {
    sanity_gate

    {
        printf '# cookie=0x998 flows on %s:br-ex BEFORE adding FIP_B (%s)\n\n' \
            "${MASTER}" "${FIP_B}"
        dump_hairpin_flows
    } | write_artifact "hairpin-flows-before.txt"

    trap teardown_fip_b EXIT
    ensure_fip_b_lsp
    ensure_fip_b_nat
    ensure_fip_b_responder

    if ! wait_for_hairpin_flow "${FIP_B}"; then
        log "ERROR: agent did not install hairpin flow for ${FIP_B} within ${RECONCILE_TIMEOUT}s"
        {
            printf '# cookie=0x998 flows on %s:br-ex AFTER %ds wait\n\n' \
                "${MASTER}" "${RECONCILE_TIMEOUT}"
            dump_hairpin_flows
        } | write_artifact "hairpin-flows-after.txt"
        return 1
    fi

    {
        printf '# cookie=0x998 flows on %s:br-ex AFTER adding FIP_B (%s)\n\n' \
            "${MASTER}" "${FIP_B}"
        dump_hairpin_flows
    } | write_artifact "hairpin-flows-after.txt"

    verify_hairpin_flows "${FIP_A}" "${FIP_B}"
    probe
}

main "$@"

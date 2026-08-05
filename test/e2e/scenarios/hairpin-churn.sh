#!/usr/bin/env bash
# Same-chassis hairpin under reconcile churn, for the containerlab E2E
# harness.
#
# Goal (issue #243): the hairpin data path must not lose a single packet
# while the agent reconciles the flow plane over and over. hairpin.sh
# sends five pings once, after the flow has settled, so a hole that opens
# between two reconciles is invisible to it. Before #241 every reconcile
# blanked the `cookie=0x998` plane before rebuilding it, which this
# scenario turns into a failed CI job instead of a tenant ticket.
#
# What it does, in order:
#
#   1. Lays down the same FIP_B topology hairpin.sh uses, from
#      lib/hairpin-topology.sh, and waits for its hairpin flow.
#   2. Metrics phase, still on the baked 5 s cadence: asserts the agent
#      reports desired == installed, wipes the plane out of band, and
#      watches the gauges report the deficit and then the heal — the
#      OVNNetworkAgentHairpinFlowsMissing alert's firing and clearing
#      conditions, measured against a real agent.
#   3. Rewrites the master's `reconcile_interval` to 1s and restarts it,
#      so the agent re-reconciles the plane every second.
#   4. Drives NB churn from the central container: a NAT row added and
#      removed every CHURN_INTERVAL, each edit an event reconcile on top
#      of the 1 s cadence.
#   5. Probes FIP_B continuously from the FIP_A workload's netns for
#      PROBE_DURATION seconds and requires **zero** lost packets.
#
# Why the probe survives the restart: the geneve tunnels ride eth0, the
# management network (`ENCAP_IP` in gwnode-entrypoint.sh), which
# `docker restart` preserves. The containerlab veth `eth1 ↔ upstream` is
# destroyed by any container exit (see bootstrap.sh's
# report_container_restarts), so the scenario captures that link before
# the first restart and puts it back afterwards — otherwise the master
# would come back unable to advertise anything and a following
# `make e2e-baseline` would fail.
#
# Pre-condition: the lab is up. When SANITY_GATE=1 (default) this
# scenario runs `baseline.sh` first so a broken green path is reported
# as a baseline regression instead of being attributed to the hairpin
# plane.
#
# Teardown (EXIT trap, best-effort throughout): stop the churn driver,
# remove the churn NAT row, tear down FIP_B, restore the baseline agent
# config, restart the master, restore its underlay and wait for
# `cr-lr0-public` to bind back — so a following `make e2e-baseline`
# passes.
#
# Environment overrides beyond the ones lib/hairpin-topology.sh
# documents (LAB, MASTER, WORKLOAD_HOST, CENTRAL, LR_NAME, LS_NAME,
# FIP_B*, WORKLOAD_GW, WORKLOAD_CIDR_LEN, RECONCILE_TIMEOUT,
# ARTIFACTS_DIR):
#   WORKLOAD_NETNS      netns the probe runs in (default vm1)
#   PROBE_DURATION      seconds of continuous probing (default 120)
#   PROBE_INTERVAL      ping inter-packet spacing, seconds (default 0.2)
#   PING_TIMEOUT        per-packet wait, passed to ping -W (default 2)
#   CHURN_FIP           NB churn NAT external IP (default 192.0.2.99)
#   CHURN_FIP_INTERNAL  its backing IP; no LSP is needed (default 192.168.10.199)
#   CHURN_INTERVAL      seconds between churn edits (default 2)
#   CHURN_RECONCILE     reconcile_interval written for the churn phase (default 1s)
#   METRICS_INTERVALS   reconcile intervals the metrics phase allows per step (default 2)
#   FLOW_SAMPLE_INTERVAL seconds between flow snapshots during the probe (default 10)
#   METRICS_PORT        agent metrics port inside the master (default 9273)
#   AGENT_CONFIG_PATH   in-container agent config path (default /etc/ovn-network-agent/config.yaml)
#   GWNODE_CONFIG       host-side baseline config (defaults to test/e2e/gwnode-config.yaml)
#   RESTART_TIMEOUT     seconds to wait for the agent after `docker restart` (default 90)
#   UPSTREAM            upstream container short name (default upstream)
#   LR_PUBLIC_PORT      the LR's public port (default lr0-public)
#   SANITY_GATE         run baseline.sh first when 1 (default 1)

set -euo pipefail

WORKLOAD_NETNS="${WORKLOAD_NETNS:-vm1}"
PROBE_DURATION="${PROBE_DURATION:-120}"
PROBE_INTERVAL="${PROBE_INTERVAL:-0.2}"
PING_TIMEOUT="${PING_TIMEOUT:-2}"
CHURN_FIP="${CHURN_FIP:-192.0.2.99}"
CHURN_FIP_INTERNAL="${CHURN_FIP_INTERNAL:-192.168.10.199}"
CHURN_INTERVAL="${CHURN_INTERVAL:-2}"
CHURN_RECONCILE="${CHURN_RECONCILE:-1s}"
METRICS_INTERVALS="${METRICS_INTERVALS:-2}"
FLOW_SAMPLE_INTERVAL="${FLOW_SAMPLE_INTERVAL:-10}"
METRICS_PORT="${METRICS_PORT:-9273}"
AGENT_CONFIG_PATH="${AGENT_CONFIG_PATH:-/etc/ovn-network-agent/config.yaml}"
RESTART_TIMEOUT="${RESTART_TIMEOUT:-90}"
UPSTREAM="${UPSTREAM:-upstream}"
LR_PUBLIC_PORT="${LR_PUBLIC_PORT:-lr0-public}"
CR_PORT="cr-${LR_PUBLIC_PORT}"
SANITY_GATE="${SANITY_GATE:-1}"

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_DIR="$(cd "${SCENARIOS_DIR}/.." && pwd)"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"
GWNODE_CONFIG="${GWNODE_CONFIG:-${E2E_DIR}/gwnode-config.yaml}"

log() { printf '[hairpin-churn] %s\n' "$*" >&2; }

# The shared FIP_B topology, its flow observers, write_artifact and
# parse_ping_loss. It defines no log of its own, so the prefix above is
# what its lines carry — source it after the definition.
# shellcheck source=test/e2e/scenarios/lib/hairpin-topology.sh
. "${SCENARIOS_DIR}/lib/hairpin-topology.sh"

UPSTREAM_NODE="clab-${LAB}-${UPSTREAM}"

sbctl() { docker exec "${CENTRAL}" ovn-sbctl "$@"; }

# Mutable state the teardown consults. Each is empty until the step that
# owns it has run, so the trap can fire at any point.
CHURN_PID=""
CHURN_COUNTER=""
SAMPLER_PID=""
FLOW_SAMPLES=""
PROBE_OUTPUT=""
CONFIG_SWAPPED=0
UNDERLAY_CIDR=""
UNDERLAY_IFACE=""
UNDERLAY_PEER_CIDR=""

# Run baseline.sh as a sanity gate so a broken green path fails fast
# under the right label. Disabled by SANITY_GATE=0 — useful when
# iterating locally on the churn behaviour itself.
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

# ---------------------------------------------------------------------------
# Metrics phase
# ---------------------------------------------------------------------------

# Read the agent's Prometheus endpoint from inside the master. The gwnode
# image ships no HTTP client, so this is bash's /dev/tcp redirect — the
# same snippet the chaos runner scrapes with (metricsScrapeScript in
# test/e2e/chaos/observe.go).
scrape_metrics() {
    docker exec "${MASTER_NODE}" bash -c \
        "exec 3<>/dev/tcp/127.0.0.1/${METRICS_PORT}; printf 'GET /metrics HTTP/1.0\r\n\r\n' >&3; cat <&3"
}

# Print the value of one exposition line, keyed by the exact series name
# including any labels. Returns non-zero when the series is absent.
metric_value() {
    awk -v key="$1" '$1 == key { value = $2; found = 1 } END { if (!found) exit 1; print value }'
}

# The reconcile cadence the lab bakes in, in whole seconds. Read from the
# config file rather than hard-coded so a change there cannot silently
# desync the polling budgets below.
baked_reconcile_seconds() {
    local raw
    raw="$(awk -F: '/^reconcile_interval:/ { gsub(/[" ]/, "", $2); print $2 }' "${GWNODE_CONFIG}")"
    case "${raw}" in
        *[0-9]s) printf '%s' "${raw%s}" ;;
        *)
            log "ERROR: cannot read a whole-second reconcile_interval from ${GWNODE_CONFIG} (got '${raw}')"
            return 1
            ;;
    esac
}

# Poll the master's metrics until `predicate <desired> <installed>`
# succeeds, or the deadline expires. Fails immediately — never into the
# timeout — when the scrape carries no hairpin_flows_ series at all,
# because that means the endpoint is wrong or the build is old, not that
# the plane is converging.
wait_for_plane_metrics() {
    local predicate="$1" budget="$2" what="$3"
    local deadline scrape desired installed
    deadline=$(( $(date +%s) + budget ))
    while :; do
        scrape="$(scrape_metrics 2>/dev/null || true)"
        # Both series or neither: they are registered together, so one of
        # them missing means the endpoint is wrong or the build predates
        # them. Either way, retrying cannot fix it.
        desired="$(printf '%s\n' "${scrape}" | metric_value ovn_network_agent_hairpin_flows_desired || true)"
        installed="$(printf '%s\n' "${scrape}" | metric_value ovn_network_agent_hairpin_flows_installed || true)"
        if [ -z "${desired}" ] || [ -z "${installed}" ]; then
            log "ERROR: metrics missing from scrape — ovn_network_agent_hairpin_flows_desired/_installed absent on ${MASTER}:${METRICS_PORT}"
            return 1
        fi
        if "${predicate}" "${desired}" "${installed}"; then
            log "${what}: desired=${desired} installed=${installed}"
            return 0
        fi
        if ! (( $(date +%s) < deadline )); then
            log "ERROR: ${what} not observed within ${budget}s (last read: desired=${desired} installed=${installed})"
            return 1
        fi
        sleep 1
    done
}

# The gauges are exposed as Prometheus floats ("5" or "5.0" depending on
# the client version), so every comparison goes through awk rather than
# bash arithmetic.
plane_converged() { awk -v d="$1" -v i="$2" 'BEGIN { exit !(d == i && d >= 2) }'; }
plane_deficient() { awk -v d="$1" -v i="$2" 'BEGIN { exit !(i < d) }'; }
plane_equal() { awk -v d="$1" -v i="$2" 'BEGIN { exit !(d == i) }'; }

hairpin_apply_errors() {
    scrape_metrics 2>/dev/null \
        | metric_value 'ovn_network_agent_ovs_flow_apply_errors_total{plane="hairpin"}'
}

# Assert the flow-plane gauges track a plane that is wiped out from under
# the agent: equal after convergence, deficient within METRICS_INTERVALS
# reconcile intervals of the wipe, equal again within METRICS_INTERVALS
# more, and no apply error counted — an out-of-band deletion is not a
# failed mutation.
metrics_phase() {
    local interval budget errors_before errors_after
    interval="$(baked_reconcile_seconds)" || return 1
    budget=$(( interval * METRICS_INTERVALS ))
    log "metrics phase on the baked ${interval}s cadence, ${METRICS_INTERVALS} intervals (${budget}s) per step"

    wait_for_plane_metrics plane_converged "${RECONCILE_TIMEOUT}" "flow plane converged" || return 1

    errors_before="$(hairpin_apply_errors)" || {
        log "ERROR: ovs_flow_apply_errors_total{plane=\"hairpin\"} missing from the scrape"
        return 1
    }

    log "wiping the cookie=0x998 plane on ${MASTER}:br-ex out of band"
    docker exec "${MASTER_NODE}" ovs-ofctl del-flows br-ex cookie=0x998/-1 || {
        log "ERROR: could not wipe the hairpin plane on ${MASTER}"
        return 1
    }

    wait_for_plane_metrics plane_deficient "${budget}" "deficit after the wipe" || return 1
    wait_for_plane_metrics plane_equal "${budget}" "plane healed" || return 1

    errors_after="$(hairpin_apply_errors)" || return 1
    if [ "${errors_before}" != "${errors_after}" ]; then
        log "ERROR: an out-of-band wipe must not count as an apply error, but ovs_flow_apply_errors_total{plane=\"hairpin\"} moved ${errors_before} → ${errors_after}"
        return 1
    fi
    log "apply errors unchanged at ${errors_after}"
}

# ---------------------------------------------------------------------------
# Churn cadence: config swap, restart, underlay repair
# ---------------------------------------------------------------------------

# Render the baseline config with reconcile_interval replaced. The key is
# *replaced*, never appended: the agent parses the file with yaml.v3,
# which rejects a document carrying the same mapping key twice.
render_churn_config() {
    sed "s/^reconcile_interval:.*/reconcile_interval: \"${CHURN_RECONCILE}\"/" "${GWNODE_CONFIG}"
}

# Record the master's underlay link before anything restarts it. Read off
# the live lab rather than duplicating bootstrap.sh's UNDERLAY_LINKS
# table, so an address change there needs no edit here.
capture_master_underlay() {
    UNDERLAY_CIDR="$(docker exec "${MASTER_NODE}" ip -o -4 addr show eth1 2>/dev/null \
        | awk '{ print $4; exit }' || true)"
    if [ -z "${UNDERLAY_CIDR}" ]; then
        log "WARNING: ${MASTER} has no IPv4 address on eth1; the teardown cannot restore its underlay"
        return 0
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
        log "WARNING: no ${UPSTREAM} interface is on ${prefix}0/30 beside ${UNDERLAY_CIDR}; the teardown cannot restore ${MASTER}'s underlay"
        UNDERLAY_CIDR=""
        return 0
    fi
    read -r UNDERLAY_IFACE UNDERLAY_PEER_CIDR <<<"${peer}"
    log "captured ${MASTER}:eth1 ${UNDERLAY_CIDR} <-> ${UPSTREAM}:${UNDERLAY_IFACE} ${UNDERLAY_PEER_CIDR}"
}

# Put the containerlab veth and both underlay addresses back after a
# container restart destroyed them. Mirrors wire_gateway_underlay and
# configure_upstream in bootstrap.sh for this one link. FRR keeps its own
# written config across a restart, so the BGP session comes back on its
# own once the addresses are in place.
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

# `docker restart` of the gateway container is the only way to make the
# agent re-read its config: the gwnode image runs no service manager and
# the entrypoint execs the agent as tini's only child. Copied from
# pf-hairpin.sh, which restarts the master for the same reason.
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

# Raise ${MASTER}'s Gateway_Chassis priority above every row in the NB so
# ovn-northd re-elects it. Skipped once ${MASTER} already holds the
# group's unique peak, so the polling caller stays idempotent.
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

# Wait until cr-lr0-public is bound to ${MASTER} again. The restart drops
# the priority-30 chassis from the HA election and OVN fails over to a
# peer — whose agent then boosts its own Gateway_Chassis priority above
# ${MASTER}'s configured 30 (EnsureActivePriorityLead, the anti-flapping
# guard), so OVN's election alone never fails back. Each poll re-asserts
# ${MASTER}'s priority until the SB shows the port there; this converges
# because once the port moves, the peers are no longer active and their
# boost is skipped. Probing before the rebind would measure a chassis
# that holds no FIP.
wait_for_master_chassis() {
    local deadline
    deadline=$(( $(date +%s) + RECONCILE_TIMEOUT ))
    log "waiting up to ${RECONCILE_TIMEOUT}s for ${CR_PORT} to bind back to ${MASTER}"
    while (( $(date +%s) < deadline )); do
        reassert_master_priority
        # Port_Binding.chassis is a UUID reference; resolve it via a
        # second lookup against the Chassis table.
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

switch_to_churn_cadence() {
    local rendered keys
    rendered="$(render_churn_config)"
    keys="$(printf '%s\n' "${rendered}" | grep -c '^reconcile_interval:' || true)"
    if [ "${keys}" != "1" ]; then
        log "ERROR: the rendered config carries ${keys} reconcile_interval keys, want exactly 1 — yaml.v3 rejects a duplicated mapping key"
        return 1
    fi
    if ! printf '%s\n' "${rendered}" | grep -q "^reconcile_interval: \"${CHURN_RECONCILE}\"$"; then
        log "ERROR: the rendered config does not carry reconcile_interval: \"${CHURN_RECONCILE}\""
        return 1
    fi

    capture_master_underlay
    log "writing reconcile_interval=${CHURN_RECONCILE} to ${AGENT_CONFIG_PATH} on ${MASTER}"
    printf '%s\n' "${rendered}" \
        | docker exec -i "${MASTER_NODE}" sh -c "cat > '${AGENT_CONFIG_PATH}'"
    CONFIG_SWAPPED=1

    restart_master || return 1
    restore_master_underlay
    wait_for_master_chassis || return 1
    wait_for_hairpin_flow "${FIP_B}" || return 1
}

# ---------------------------------------------------------------------------
# Churn driver and probe
# ---------------------------------------------------------------------------

# Add and remove a NAT row on the LR, forever, one edit per
# CHURN_INTERVAL. No backing LSP is needed: the agent keys hairpin flows
# off the NAT row alone, so each edit is a real change to the desired
# flow plane and each is an event reconcile on the master. A single
# ovn-nbctl failure is logged and the loop continues; one line is
# appended per completed add+delete cycle so a driver that died can be
# told apart from a plane that never churned.
start_churn_driver() {
    CHURN_COUNTER="$(mktemp)"
    log "starting the NB churn driver: ${CHURN_FIP} added and removed every ${CHURN_INTERVAL}s"
    (
        while :; do
            nbctl --may-exist lr-nat-add "${LR_NAME}" dnat_and_snat \
                "${CHURN_FIP}" "${CHURN_FIP_INTERNAL}" >/dev/null 2>&1 \
                || log "churn: lr-nat-add ${CHURN_FIP} failed, continuing"
            sleep "${CHURN_INTERVAL}"
            nbctl --if-exists lr-nat-del "${LR_NAME}" dnat_and_snat \
                "${CHURN_FIP}" >/dev/null 2>&1 \
                || log "churn: lr-nat-del ${CHURN_FIP} failed, continuing"
            sleep "${CHURN_INTERVAL}"
            printf 'cycle\n' >>"${CHURN_COUNTER}"
        done
    ) &
    CHURN_PID=$!
}

stop_churn_driver() {
    [ -n "${CHURN_PID}" ] || return 0
    kill "${CHURN_PID}" 2>/dev/null || true
    wait "${CHURN_PID}" 2>/dev/null || true
    CHURN_PID=""
}

remove_churn_nat() {
    nbctl --if-exists lr-nat-del "${LR_NAME}" dnat_and_snat "${CHURN_FIP}" >/dev/null 2>&1 || true
}

# A driver that died in its first seconds would leave a quiet lab and a
# green probe that proved nothing. Half the expected cycles is the bar:
# an ovn-nbctl that is merely slow still churns, one that is gone does
# not.
assert_churn_progressed() {
    local completed expected
    completed="$(wc -l <"${CHURN_COUNTER}" | tr -d ' ')"
    expected=$(( PROBE_DURATION / (2 * CHURN_INTERVAL) ))
    log "churn driver completed ${completed} add/delete cycles over ${PROBE_DURATION}s (expected about ${expected})"
    if [ "${completed}" -lt $(( expected / 2 )) ]; then
        log "ERROR: churn driver starved — ${completed} completed cycles, fewer than half of the expected ${expected}"
        return 1
    fi
}

# Snapshot the hairpin plane during the probe, so a run that did lose
# packets can be read against what the plane looked like at the time.
start_flow_sampler() {
    FLOW_SAMPLES="$(mktemp)"
    (
        while :; do
            {
                printf '# %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
                dump_hairpin_flows
                printf '\n'
            } >>"${FLOW_SAMPLES}"
            sleep "${FLOW_SAMPLE_INTERVAL}"
        done
    ) &
    SAMPLER_PID=$!
}

stop_flow_sampler() {
    [ -n "${SAMPLER_PID}" ] || return 0
    kill "${SAMPLER_PID}" 2>/dev/null || true
    wait "${SAMPLER_PID}" 2>/dev/null || true
    SAMPLER_PID=""
}

# Probe FIP_B from inside the FIP_A workload's netns, so the source IP is
# FIP_A's logical IP and OVN's egress SNAT picks FIP_A as the visible
# source — the same path a real tenant VM behind FIP_A would take. ping's
# own exit status is ignored: any loss makes it non-zero, and the loss
# count from its summary is what decides.
run_probe() {
    local count
    count="$(awk -v d="${PROBE_DURATION}" -v i="${PROBE_INTERVAL}" 'BEGIN { printf "%d", d / i }')"
    PROBE_OUTPUT="$(mktemp)"
    log "probing ${FIP_B} from ${WORKLOAD_HOST} netns ${WORKLOAD_NETNS}: ${count} packets, ${PROBE_INTERVAL}s apart (~${PROBE_DURATION}s)"
    docker exec "${WORKLOAD_NODE}" \
        ip netns exec "${WORKLOAD_NETNS}" \
        ping -i "${PROBE_INTERVAL}" -c "${count}" -W "${PING_TIMEOUT}" "${FIP_B}" \
        >"${PROBE_OUTPUT}" 2>&1 || true
}

# The pass criterion. A missing or truncated summary — the probe was
# killed, or `docker exec` never reached the netns — makes parse_ping_loss
# fail, and that is a scenario failure rather than a zero-loss reading.
assert_zero_loss() {
    local loss
    if ! loss="$(parse_ping_loss "${PROBE_OUTPUT}")"; then
        log "ERROR: could not parse a ping summary out of the probe output; the probe did not complete:"
        sed 's/^/    /' "${PROBE_OUTPUT}" >&2
        return 1
    fi
    log "probe summary:"
    grep -E 'packets transmitted|rtt' "${PROBE_OUTPUT}" | sed 's/^/    /' >&2
    if [ "${loss}" -ne 0 ]; then
        log "ERROR: ${loss} packets lost over ${PROBE_DURATION}s of hairpin traffic under reconcile churn; the plane is not stable"
        return 1
    fi
    log "zero packets lost over ${PROBE_DURATION}s under ${CHURN_RECONCILE} reconciles and ${CHURN_INTERVAL}s NB churn"
}

# ---------------------------------------------------------------------------
# Artifacts and teardown
# ---------------------------------------------------------------------------

write_flow_snapshot() {
    local when="$1" path="$2"
    {
        printf '# cookie=0x998 flows on %s:br-ex %s\n\n' "${MASTER}" "${when}"
        dump_hairpin_flows
    } | write_artifact "${path}"
}

collect_failure_artifacts() {
    [ -n "${ARTIFACTS_DIR}" ] || return 0
    log "collecting failure artifacts into ${ARTIFACTS_DIR}"
    write_flow_snapshot "AFTER the failure" "hairpin-flows-after.txt"
    if [ -n "${FLOW_SAMPLES}" ] && [ -s "${FLOW_SAMPLES}" ]; then
        cat "${FLOW_SAMPLES}" | write_artifact "hairpin-flows-during.txt"
    fi
    if [ -n "${PROBE_OUTPUT}" ] && [ -s "${PROBE_OUTPUT}" ]; then
        cat "${PROBE_OUTPUT}" | write_artifact "hairpin-churn-probe.txt"
    fi
    # The agent runs in the container foreground, so its log is the
    # container's.
    docker logs --tail 500 "${MASTER_NODE}" 2>&1 | write_artifact "master-agent.log"
}

# Best-effort throughout: every step is independently allowed to fail, so
# a teardown error cannot mask the scenario's own pass/fail signal. The
# shell exits with the status it entered the trap with.
teardown() {
    local status=$?
    if [ "${status}" -ne 0 ]; then
        collect_failure_artifacts || true
    fi
    stop_flow_sampler || true
    stop_churn_driver || true
    remove_churn_nat
    teardown_fip_b || true
    if [ "${CONFIG_SWAPPED}" -eq 1 ]; then
        log "restoring the baseline agent config on ${MASTER}"
        docker exec -i "${MASTER_NODE}" sh -c "cat > '${AGENT_CONFIG_PATH}'" \
            <"${GWNODE_CONFIG}" || true
        restart_master || true
        restore_master_underlay || true
        wait_for_master_chassis || true
    fi
    rm -f "${CHURN_COUNTER}" "${FLOW_SAMPLES}" "${PROBE_OUTPUT}" 2>/dev/null || true
}

main() {
    sanity_gate

    write_flow_snapshot "BEFORE adding FIP_B (${FIP_B})" "hairpin-flows-before.txt"

    trap teardown EXIT
    ensure_fip_b_lsp
    ensure_fip_b_nat
    ensure_fip_b_responder
    wait_for_hairpin_flow "${FIP_B}"

    metrics_phase
    switch_to_churn_cadence

    start_churn_driver
    start_flow_sampler
    run_probe
    stop_flow_sampler
    stop_churn_driver

    if [ -n "${FLOW_SAMPLES}" ]; then
        write_artifact "hairpin-flows-during.txt" <"${FLOW_SAMPLES}"
    fi
    write_flow_snapshot "AFTER the probe (${FIP_B})" "hairpin-flows-after.txt"

    assert_churn_progressed
    assert_zero_loss
}

main "$@"

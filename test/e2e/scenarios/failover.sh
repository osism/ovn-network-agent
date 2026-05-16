#!/usr/bin/env bash
# HA failover scenario for the containerlab E2E harness.
#
# Goal (issue #105): with the lab green (baseline passing), simulate
# loss of the priority-30 master chassis (`gateway-1`). OVN must
# re-elect `cr-lr0-public` to the priority-20 chassis (`gateway-2`),
# BGP must withdraw the FIP /32s on the dead session and re-advertise
# them from the new master, and `client-1 → FIP` must resume within
# `FAILOVER_TIMEOUT` (default 30s).
#
# Implementation note — chassis loss simulation:
# The issue's "Action" line says `docker stop clab-${LAB}-gateway-1`.
# Containerlab wires the per-gateway underlay (gateway-N:eth1 ↔
# upstream:ethN) as veth pairs at deploy time; `docker stop` destroys
# the container's netns and with it BOTH ends of those veths. A
# follow-up `docker start` brings the container back but containerlab
# does not re-establish the link, so the master has no underlay, no
# BGP session, and the FIP advertisement never comes back — the test
# can't return the lab to baseline state and "3+ green runs in a row"
# is impossible without `make e2e-down && make e2e-up` between runs.
# Stop only ovn-controller via `ovn-ctl stop_controller` instead: it
# terminates cleanly on SIGTERM, releases its claim on cr-lr0-public,
# and OVN re-elects to the priority-20 chassis. The agent and FRR on
# the "lost" chassis keep running, so they observe the Port_Binding
# change and withdraw the FIP /32s from BGP themselves. Failback is
# `ovn-ctl start_controller` on the same node — no container restart,
# no veth re-create, no bootstrap re-run. The OVN HA mechanism under
# test (BFD-monitored re-election of cr-lr0-public) is identical;
# only the simulation of node death is softer.
#
# A note on why this is not SIGSTOP'ing ovn-controller: a frozen
# process still holds its SB claim. The release happens at clean
# shutdown via the SIGTERM handler, so re-election needs the process
# to actually exit.
#
# Strict variant (issue #131): when LOSS_BUDGET is set, the scenario
# additionally measures the data-plane outage. A 0.1s-spaced `ping`
# flood from CLIENT is captured with `tcpdump` on its eth1 across the
# re-election; the failover outage is the largest gap between
# consecutive ICMP echo replies and must stay within LOSS_BUDGET
# seconds. The pcap is written to ARTIFACTS_DIR for triage. The
# loss-measurement pattern follows issue #113 (`ping -i 0.1` +
# `tcpdump`). With LOSS_BUDGET unset the scenario behaves exactly as
# before.
#
# Pre-condition: the lab is up. When SANITY_GATE=1 (default) this
# scenario runs `baseline.sh` first so a broken green path is reported
# as a baseline regression instead of being attributed to failover.
#
# Teardown: ovn-controller on the master is resumed on EXIT (via
# trap). The script then waits up to FAILBACK_TIMEOUT seconds for
# `cr-lr0-public` to bind back to MASTER and for `client-1 → FIP`
# reachability to return, leaving the lab at baseline state. The CI
# workflow additionally invokes the teardown step with `if: always()`
# so a fatal interpreter error here (which skips the trap) is still
# cleaned up.
#
# Environment overrides (used by the CI workflow):
#   LAB                container-name prefix (defaults to the topology name "ovn-e2e")
#   FIP                target FIP to ping (defaults to the priority-30 chassis FIP)
#   CLIENT             source container (defaults to clab-${LAB}-client-1)
#   MASTER             chassis to fail (defaults to gateway-1, the priority-30 chassis)
#   CENTRAL            OVN central container (defaults to clab-${LAB}-central)
#   LR_PUBLIC_PORT     LR external port name (defaults to lr0-public)
#   FAILOVER_TIMEOUT   seconds to wait for reachability to recover (default 30)
#   FAILBACK_TIMEOUT   seconds to wait for the lab to return to baseline (default 60)
#   PING_COUNT         packets for the final reachability check (default 5)
#   PING_TIMEOUT       per-packet wait, passed to ping -W (default 2)
#   SANITY_GATE        run baseline.sh first when 1 (default 1)
#   LOSS_BUDGET        when set, run the strict variant and fail if the
#                      failover outage exceeds this many seconds
#                      (issue #131; default unset = strict variant off)
#   PROBE_INTERVAL     ping spacing for the strict-variant flood (default 0.1)
#   ARTIFACTS_DIR      when set, the strict-variant pcap is saved here

set -euo pipefail

LAB="${LAB:-ovn-e2e}"
FIP="${FIP:-192.0.2.10}"
CLIENT="${CLIENT:-clab-${LAB}-client-1}"
MASTER="${MASTER:-gateway-1}"
MASTER_NODE="clab-${LAB}-${MASTER}"
CENTRAL="${CENTRAL:-clab-${LAB}-central}"
LR_PUBLIC_PORT="${LR_PUBLIC_PORT:-lr0-public}"
CR_PORT="cr-${LR_PUBLIC_PORT}"
FAILOVER_TIMEOUT="${FAILOVER_TIMEOUT:-30}"
FAILBACK_TIMEOUT="${FAILBACK_TIMEOUT:-60}"
PING_COUNT="${PING_COUNT:-5}"
PING_TIMEOUT="${PING_TIMEOUT:-2}"
SANITY_GATE="${SANITY_GATE:-1}"
LOSS_BUDGET="${LOSS_BUDGET:-}"
PROBE_INTERVAL="${PROBE_INTERVAL:-0.1}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-}"

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"

log() { printf '[failover] %s\n' "$*" >&2; }

# Resolve the chassis name currently anchoring `cr-lr0-public`. The
# Port_Binding.chassis column is a UUID reference; resolve it via a
# second lookup against the Chassis table. Returns empty string when
# no chassis is bound (e.g. during re-election or when the master is
# down and the port hasn't migrated yet).
current_master() {
    local chassis_uuid chassis_name
    chassis_uuid=$(docker exec "${CENTRAL}" ovn-sbctl --bare \
        --columns=chassis find Port_Binding \
        logical_port="${CR_PORT}" 2>/dev/null || true)
    if [ -z "${chassis_uuid}" ]; then
        return 0
    fi
    chassis_name=$(docker exec "${CENTRAL}" ovn-sbctl --bare \
        --columns=name list Chassis "${chassis_uuid}" 2>/dev/null || true)
    printf '%s' "${chassis_name}"
}

# Stop ovn-controller cleanly on the master via the canonical OVN
# service script. ovn-controller's SIGTERM handler releases its claim
# on cr-lr0-public; OVN's ha_chassis_group election then promotes the
# next-priority candidate. The agent and FRR on the chassis stay up
# and observe the SB Port_Binding change so the FIP /32 statics in
# vrf-provider get removed and FRR withdraws the BGP advertisement.
stop_controller_on_master() {
    log "stopping ovn-controller on ${MASTER_NODE} to trigger HA re-election"
    docker exec "${MASTER_NODE}" \
        /usr/share/ovn/scripts/ovn-ctl stop_controller >/dev/null
}

# Start ovn-controller again on the master. BFD comes back up, the
# chassis registers as healthy in SB, OVN re-elects cr-lr0-public to
# the priority-30 chassis. Idempotent: ovn-ctl is a no-op when the
# controller is already running, so running this in the teardown
# trap after a successful run is safe.
start_controller_on_master() {
    log "teardown: starting ovn-controller on ${MASTER_NODE}"
    docker exec "${MASTER_NODE}" \
        /usr/share/ovn/scripts/ovn-ctl start_controller >/dev/null \
        || log "teardown: ovn-ctl start_controller returned non-zero; continuing"
}

# Wait against a single FAILBACK_TIMEOUT budget for two signals:
#   1. SB rebind of cr-lr0-public to MASTER (control plane),
#   2. client-1 → FIP reachability to come back (data plane).
# Best-effort: a teardown failure must not mask the scenario's own
# pass/fail signal, so this never aborts the script.
restore_master() {
    start_controller_on_master

    local deadline
    deadline=$(( $(date +%s) + FAILBACK_TIMEOUT ))

    local who=""
    while (( $(date +%s) < deadline )); do
        who="$(current_master)"
        if [ "${who}" = "${MASTER}" ]; then
            log "teardown: ${CR_PORT} is bound to ${MASTER} again"
            break
        fi
        sleep 2
    done
    if [ "${who}" != "${MASTER}" ]; then
        log "teardown: ${CR_PORT} did not bind back to ${MASTER} within ${FAILBACK_TIMEOUT}s (currently: ${who:-<none>})"
        return 0
    fi

    log "teardown: waiting for ${CLIENT} → ${FIP} to recover after failback"
    while (( $(date +%s) < deadline )); do
        if docker exec "${CLIENT}" ping -c 1 -W 1 "${FIP}" >/dev/null 2>&1; then
            log "teardown: reachability through ${FIP} restored"
            return 0
        fi
        sleep 2
    done
    log "teardown: reachability through ${FIP} did not recover within ${FAILBACK_TIMEOUT}s of failback"
}

# Run baseline.sh as a sanity gate so a broken green path fails fast
# under the right label. Disabled by SANITY_GATE=0 — useful when
# iterating locally on the failover behaviour itself.
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

# Poll the reachability check for up to FAILOVER_TIMEOUT seconds. As
# soon as one packet gets through, OVN has re-elected the gateway,
# BGP has re-advertised the FIP /32, and the new master's agent has
# reconciled the leak path. Counts the wall-clock elapsed to make the
# pass/fail log line useful for triage.
wait_for_recovery() {
    local start_ts deadline
    start_ts=$(date +%s)
    deadline=$(( start_ts + FAILOVER_TIMEOUT ))
    log "waiting up to ${FAILOVER_TIMEOUT}s for ${CLIENT} → ${FIP} to recover"
    while (( $(date +%s) < deadline )); do
        if docker exec "${CLIENT}" ping -c 1 -W 1 "${FIP}" >/dev/null 2>&1; then
            log "first packet through after $(( $(date +%s) - start_ts ))s"
            return 0
        fi
        sleep 2
    done
    log "no reply within ${FAILOVER_TIMEOUT}s — falling through to the final probe so its output appears in the log"
    return 0
}

# Final probe — the exit code of this scenario is the exit code of
# the post-recovery ping, which is non-zero on any packet loss.
probe() {
    log "ping -c ${PING_COUNT} -W ${PING_TIMEOUT} ${FIP} from ${CLIENT}"
    docker exec "${CLIENT}" ping -c "${PING_COUNT}" -W "${PING_TIMEOUT}" "${FIP}"
}

# Strict variant: drive the failover while a fine-grained ping flood
# from CLIENT is captured with tcpdump on its eth1. The failover outage
# is the largest gap between consecutive ICMP echo replies; the variant
# fails when that exceeds LOSS_BUDGET seconds. tcpdump runs with -U so
# the pcap is consistent on disk even if the capture is not stopped
# cleanly. Returns non-zero on a budget breach so main() can fail the
# scenario after still confirming the control-plane migration.
measured_failover() {
    local pcap="/tmp/failover-strict.pcap"
    local window=$(( FAILOVER_TIMEOUT + 10 ))
    # Seconds of flood before the re-election and after recovery. The
    # capture must cover both, so min_span below is derived from them.
    local baseline_secs=2 post_secs=3

    log "strict mode: capturing ICMP on ${CLIENT}:eth1, loss budget ${LOSS_BUDGET}s"
    docker exec -d "${CLIENT}" \
        timeout "$(( window + 5 ))" tcpdump -i eth1 -n -U -w "${pcap}" icmp
    sleep 1  # let tcpdump open the capture before traffic starts

    # Steady ping flood spanning the whole failover window: one packet every
    # PROBE_INTERVAL for `window` seconds. -c bounds it so it self-terminates
    # even if the pkill below is missed. Derive the count from
    # PROBE_INTERVAL rather than hardcoding its default reciprocal, so
    # overriding the knob during triage still bounds the flood to the window
    # instead of letting the ping outlive the capture.
    local count
    count=$(awk -v w="${window}" -v i="${PROBE_INTERVAL}" 'BEGIN { printf "%d", w / i }')
    docker exec -d "${CLIENT}" \
        ping -n -i "${PROBE_INTERVAL}" -c "${count}" "${FIP}"
    sleep "${baseline_secs}"  # baseline traffic before the re-election

    stop_controller_on_master
    wait_for_recovery
    sleep "${post_secs}"  # capture a few post-recovery replies

    docker exec "${CLIENT}" pkill -INT tcpdump   >/dev/null 2>&1 || true
    docker exec "${CLIENT}" pkill -f 'ping -n -i' >/dev/null 2>&1 || true
    sleep 1  # let tcpdump flush and exit

    if [ -n "${ARTIFACTS_DIR}" ]; then
        mkdir -p "${ARTIFACTS_DIR}"
        docker cp "${CLIENT}:${pcap}" "${ARTIFACTS_DIR}/failover-strict.pcap" \
            >/dev/null 2>&1 || log "could not copy pcap into ${ARTIFACTS_DIR}"
    fi

    # The largest gap between consecutive echo replies is the outage; the
    # span from the first to the last reply is how much of the failover the
    # capture actually witnessed.
    local stats outage replies span
    stats=$(docker exec "${CLIENT}" \
        tcpdump -tt -n -r "${pcap}" 'icmp[icmptype] = icmp-echoreply' 2>/dev/null \
        | awk '{ n++; if (prev == "") first = $1; else { g = $1 - prev; if (g > max) max = g } prev = $1 }
               END { printf "%.2f %d %.2f", max + 0, n + 0, (n ? prev - first : 0) }')
    read -r outage replies span <<<"${stats}"
    log "captured ${replies} echo replies spanning ${span}s; largest reply gap ${outage}s (budget ${LOSS_BUDGET}s)"

    # The capture must cover the baseline, the re-election and the
    # post-recovery replies. A reply-count floor cannot express this: the
    # baseline alone yields baseline_secs/PROBE_INTERVAL replies (~20 at the
    # defaults), so a flood that died at the failover still clears any small
    # floor while its largest gap is one probe interval — the budget check
    # then passes without the re-election ever being observed. A count floor
    # scaled to the window would be wrong in the other direction: the flood
    # is killed once wait_for_recovery returns, so it normally runs for far
    # less than the window it is bounded by.
    local min_span=$(( baseline_secs + post_secs ))
    if awk -v s="${span}" -v m="${min_span}" 'BEGIN { exit !(s < m) }'; then
        log "ERROR: echo replies span only ${span}s, expected >= ${min_span}s — the flood or capture died before the failover completed"
        return 1
    fi
    if awk -v o="${outage}" -v b="${LOSS_BUDGET}" 'BEGIN { exit !(o > b) }'; then
        log "ERROR: failover outage ${outage}s exceeds the ${LOSS_BUDGET}s budget"
        return 1
    fi
    log "failover outage ${outage}s within the ${LOSS_BUDGET}s budget"
    return 0
}

main() {
    sanity_gate

    local before
    before="$(current_master)"
    log "current ${CR_PORT} chassis before failover: ${before:-<none>}"
    if [ "${before}" != "${MASTER}" ]; then
        log "WARNING: expected master ${MASTER}, observed ${before:-<none>} — continuing anyway"
    fi

    trap restore_master EXIT

    # The strict variant measures the data-plane outage across the
    # re-election; the default variant just polls for recovery.
    local strict_rc=0
    if [ -n "${LOSS_BUDGET}" ]; then
        measured_failover || strict_rc=1
    else
        stop_controller_on_master
        wait_for_recovery
    fi

    # Guard against a false pass: a probe that succeeds because OVS on
    # MASTER kept executing stale flows after ovn-controller died is
    # NOT a successful failover. Confirm that cr-lr0-public actually
    # migrated to a different chassis before declaring success.
    local after
    after="$(current_master)"
    log "current ${CR_PORT} chassis after failover: ${after:-<none>}"
    if [ "${after}" = "${MASTER}" ]; then
        log "ERROR: ${CR_PORT} did not migrate away from ${MASTER} — re-election did not happen"
        return 1
    fi

    probe || return 1
    return "${strict_rc}"
}

main "$@"

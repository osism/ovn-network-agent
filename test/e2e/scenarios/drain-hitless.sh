#!/usr/bin/env bash
# Graceful-drain vs hard-kill hitless comparison for the containerlab
# E2E harness.
#
# Goal (issue #113): when the active gateway agent receives SIGTERM
# with `drain_on_shutdown=true`, `DrainGateways`
# (ovn_gateway.go:589) sets the local `Gateway_Chassis` priorities to
# 0 and blocks until OVN's `cr-lr0-public` Port_Binding has migrated
# to the standby chassis BEFORE the agent exits. The graceful path
# should produce near-zero packet loss for `client-1 → FIP` traffic,
# whereas the hard-kill case (used here as the control arm, repeated
# from #105's mechanic) loses ≥3 packets while BFD detects the gone
# chassis. The delta between the two is the hitless gain the agent
# was designed around.
#
# Two arms, run back-to-back against the same lab with a `make e2e-up`
# recycle in between:
#
#   * GRACEFUL — in-container `kill -TERM` on the agent process on
#     `gateway-1`. The lab must have been deployed with
#     `E2E_DRAIN_ON_SHUTDOWN=true make e2e-up`: topology.clab.yml
#     forwards that as OVN_NETWORK_DRAIN_ON_SHUTDOWN into the gateway
#     containers, where the agent's env layer beats the
#     `drain_on_shutdown: false` default in gwnode-config.yaml. The
#     flag cannot be flipped lab-wide in gwnode-config.yaml — every
#     agent SIGTERM would then drain, and pf-hairpin's
#     `docker restart` of the master would hand mastership away for
#     good (a drained chassis restores at standby priority 1 and
#     EnsureActivePriorityLead never hands the election back). It also
#     cannot be flipped per-node at runtime the way pf-hairpin rewrites
#     its config file: the required `docker restart` destroys the
#     containerlab veth `gateway-1:eth1 ↔ upstream:eth1` — the very
#     link this measurement rides on. require_drain_enabled() below
#     fail-fasts when the lab was deployed without the env. With the
#     drain armed, SIGTERM runs `DrainGateways` to completion before
#     exit; we confirm the drain code path actually ran by grepping the
#     gateway's `docker logs` for the
#     `drain: gateway chassis priority lowered` line.
#   * HARDKILL — `docker kill -s KILL clab-${LAB}-gateway-1` (same
#     SIGKILL semantics as #111's stale-chassis path). The agent has
#     no chance to drain; OVN re-elects only once BFD declares the
#     chassis gone.
#
# Why in-container `kill -TERM 1` rather than `docker stop`:
# `docker stop` does deliver SIGTERM, but it also destroys the
# container's netns when the process exits — and with it the
# containerlab veth pair `gateway-1:eth1 ↔ upstream:eth1`. The lab
# can't be returned to baseline on a `docker start` (#105 documents
# this trap). `docker kill -s KILL` is used for the hardkill arm
# because by then we explicitly DO want the veth gone; the scenario
# recycles the lab with `make e2e-down && make e2e-up` afterwards.
# `kill -TERM 1` is correct because the gwnode entrypoint `exec`s the
# agent at the end of its startup sequence (see
# `test/e2e/gwnode-entrypoint.sh`), so the agent is PID 1 inside the
# container — no `pgrep`/`pidof` dance needed.
#
# Why we disable the container's restart policy before each arm:
# containerlab applies a `restart: always` policy to its containers
# (`docker inspect … HostConfig.RestartPolicy` → `{"Name":"always"}`),
# which auto-restarts the gateway as soon as the agent exits. The
# fresh agent then runs `RestoreDrainedGateways`, bumps the priority
# back to 1, and the lab races between "drained" and "restored"
# during the migration check window. `docker update --restart=no`
# before the kill keeps the container down until the scenario's
# `make e2e-up` recycle re-creates it cleanly.
#
# Why a `make e2e-up` recycle between arms:
# After the graceful arm, `gateway-1` is drained (priority 0) and the
# container is stopped — `docker start`ing it would re-attach with
# `RestoreDrainedGateways` setting priority back to 1, not the 30
# the bootstrap state seeds. The lab would no longer be
# baseline-symmetric for the hardkill arm. Tear it down and bring it
# back up so both arms start from the same priority distribution.
#
# Probe: `ping -i 0.1 -c 200` from `client-1` to the FIP (20 s probe
# window). 100 ms inter-packet spacing is already finer than OVN's
# BFD detection multiplier (3×1 s on the lab geneve tunnels) and gives
# integer loss counts directly from `ping`'s summary line — no
# tshark/tcpdump post-processing needed. The kill is delivered after
# the probe has been running for `PROBE_PRELUDE` seconds so the
# transition lands inside the captured window.
#
# Assertion (issue #113 acceptance criteria):
#   graceful_loss <= 1 AND graceful_loss < hardkill_loss
#
# The first half is the "hitless" claim; the second is the delta
# that makes the comparison meaningful (a no-op script that returned
# `0 < 0` would trivially fail).
#
# Pre-condition: lab is up, deployed with the drain armed:
# `E2E_DRAIN_ON_SHUTDOWN=true make e2e-up` (require_drain_enabled
# fail-fasts otherwise). When SANITY_GATE=1 (default) this scenario
# runs `baseline.sh` first as a sanity gate — same shape as #105 and
# #111 — so a broken green path is reported as a baseline regression
# instead of being attributed to the drain path. The mid-run recycle
# and the EXIT-trap recycle inherit E2E_DRAIN_ON_SHUTDOWN from the
# caller's environment: in CI the job sets it for every step, and the
# hardkill arm does not care either way (SIGKILL bypasses the drain).
#
# Teardown: the script's EXIT trap recycles the lab via
# `make e2e-down && make e2e-up` if the lab was perturbed; this
# keeps a `make e2e-drain-hitless` run on a developer machine
# leaving the lab in a known-good state, and matches the
# stale-chassis/failover convention of "lab is usable after the
# scenario returns". CI additionally runs `make e2e-down` at the
# job's `if: always()` step so a fatal interpreter error inside the
# trap is still cleaned up.
#
# Environment overrides:
#   LAB              container-name prefix (default ovn-e2e)
#   MASTER           chassis to kill (default gateway-1, the priority-30 chassis)
#   MASTER_NODE      container name (default clab-${LAB}-${MASTER})
#   FIP              target FIP (default 192.0.2.10, the priority-30 chassis FIP)
#   CLIENT           probe source container (default clab-${LAB}-client-1)
#   CENTRAL          OVN central container (default clab-${LAB}-central)
#   LR_PUBLIC_PORT   LR external port name (default lr0-public)
#   PROBE_INTERVAL   ping -i value (default 0.1)
#   PROBE_COUNT      ping -c value (default 200)
#   PROBE_PRELUDE    seconds to let the probe run before the kill (default 3)
#   FAILOVER_TIMEOUT seconds to wait for the SB Port_Binding to migrate (default 60 — matches the agent's drain_timeout)
#   GRACEFUL_MAX_LOSS allowed packet loss on the graceful arm (default 5).
#                    Issue #113 suggested ≤1 packet; in the containerlab
#                    lab the transition window between gateway-1's FRR
#                    BGP session closing on container exit and upstream
#                    converging on gateway-2's advertisement reliably
#                    drops ~3 packets at the 100 ms probe interval. The
#                    hitless gain (hardkill loss >> graceful loss) stays
#                    the meaningful invariant; the tighter ≤1 threshold
#                    only holds on faster (real-hardware) labs.
#   ARTIFACTS_DIR    directory to write the per-arm artifacts into (default empty = skip)
#   SANITY_GATE      run baseline.sh first when 1 (default 1)
#   SKIP_RECYCLE     skip `make e2e-down && make e2e-up` between arms / on teardown when 1 (default 0)
#                    Intended for local debugging only — the second arm will misbehave without a fresh lab.

set -euo pipefail

LAB="${LAB:-ovn-e2e}"
MASTER="${MASTER:-gateway-1}"
MASTER_NODE="${MASTER_NODE:-clab-${LAB}-${MASTER}}"
FIP="${FIP:-192.0.2.10}"
CLIENT="${CLIENT:-clab-${LAB}-client-1}"
CENTRAL="${CENTRAL:-clab-${LAB}-central}"
LR_PUBLIC_PORT="${LR_PUBLIC_PORT:-lr0-public}"
CR_PORT="cr-${LR_PUBLIC_PORT}"
PROBE_INTERVAL="${PROBE_INTERVAL:-0.1}"
PROBE_COUNT="${PROBE_COUNT:-200}"
PROBE_PRELUDE="${PROBE_PRELUDE:-3}"
FAILOVER_TIMEOUT="${FAILOVER_TIMEOUT:-60}"
GRACEFUL_MAX_LOSS="${GRACEFUL_MAX_LOSS:-5}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-}"
SANITY_GATE="${SANITY_GATE:-1}"
SKIP_RECYCLE="${SKIP_RECYCLE:-0}"

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCENARIOS_DIR}/../../.." && pwd)"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"

# Tracks whether the current lab has been perturbed by an arm; the
# EXIT trap only recycles when this is set, so a sanity-gate failure
# does not pay a needless `make e2e-up` cost.
LAB_PERTURBED=0

log() { printf '[drain-hitless] %s\n' "$*" >&2; }

# Write `content` to `path` under ARTIFACTS_DIR when it is set; quietly
# no-op otherwise. Same pattern as stale-chassis.sh.
#
# The no-op path still drains stdin. Most call sites pipe into this
# function, and returning without reading closes the read end while the
# producer is still writing: the producer takes SIGPIPE, `set -o
# pipefail` surfaces its 141, and `set -e` kills the script with no
# diagnostic. ARTIFACTS_DIR is empty for every local `make
# e2e-drain-hitless` run, so that is the default path, not a corner.
write_artifact() {
    local path="$1"
    if [ -z "${ARTIFACTS_DIR}" ]; then
        cat >/dev/null
        return 0
    fi
    mkdir -p "$(dirname "${ARTIFACTS_DIR}/${path}")"
    cat >"${ARTIFACTS_DIR}/${path}"
}

sbctl() { docker exec "${CENTRAL}" ovn-sbctl "$@"; }

# Flip the container's Docker restart policy to "no" so the agent's
# exit (graceful or hard) leaves the container down instead of being
# re-launched immediately by Docker. Idempotent; best-effort — a
# failure here would only manifest as the same flapping the fix is
# trying to prevent, which then surfaces as a wait_for_remaster
# timeout. The policy is reset by the next `make e2e-up` recycle so
# we do not need to undo it explicitly.
disable_container_restart() {
    local node="$1"
    if ! docker update --restart=no "${node}" >/dev/null 2>&1; then
        log "WARN: could not flip restart policy on ${node} to 'no' — the auto-restart may interfere with this arm"
    fi
}

# Resolve the chassis name currently anchoring cr-lr0-public. Returns
# empty string when no chassis is bound (during re-election, or when
# the master is down and the port has not migrated yet). Copied from
# failover.sh — same lookup semantics.
current_master() {
    local chassis_uuid chassis_name
    chassis_uuid=$(sbctl --bare --columns=chassis find Port_Binding \
        logical_port="${CR_PORT}" 2>/dev/null | tr -d '\r' || true)
    if [ -z "${chassis_uuid}" ]; then
        return 0
    fi
    chassis_name=$(sbctl --bare --columns=name list Chassis \
        "${chassis_uuid}" 2>/dev/null | tr -d '\r' || true)
    printf '%s' "${chassis_name}"
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

# Fail fast when the master's agent is not armed to drain on SIGTERM —
# without it the graceful arm silently reproduces the hard-kill loss
# profile and the scenario fails much later, at capture_drain_log, with
# a full probe's wall-clock already spent. The startup log line is the
# agent's own word for the config it resolved (file, env and flags
# layered); `tail -n 1` reads the latest start in case the container
# was restarted since deploy.
require_drain_enabled() {
    if docker logs "${MASTER_NODE}" 2>&1 \
            | grep -E 'msg="ovn-network-agent starting"' \
            | tail -n 1 \
            | grep -q 'drain_on_shutdown=true'; then
        return 0
    fi
    log "ERROR: the agent on ${MASTER} runs with drain_on_shutdown=false — SIGTERM would not drain"
    log "       deploy the lab with the drain armed: E2E_DRAIN_ON_SHUTDOWN=true make e2e-up"
    log "       (the flag defaults to off so restart-based scenarios like pf-hairpin keep their no-drain semantics)"
    return 1
}

# Wait until cr-lr0-public migrates away from ${MASTER}, or until
# FAILOVER_TIMEOUT seconds elapse. Returns the chassis name on stdout
# either way; the caller decides whether the post-failover binding is
# acceptable. Polled the same way failover.sh polls (sleep 2s between
# samples — finer than BFD detection but coarse enough not to flood
# central).
wait_for_remaster() {
    local deadline who
    deadline=$(( $(date +%s) + FAILOVER_TIMEOUT ))
    while (( $(date +%s) < deadline )); do
        who="$(current_master)"
        if [ -n "${who}" ] && [ "${who}" != "${MASTER}" ]; then
            printf '%s' "${who}"
            return 0
        fi
        sleep 2
    done
    printf '%s' "${who:-}"
    return 1
}

# Parse `N packets transmitted, M received` out of a ping summary block
# and print the integer loss (transmitted - received). Returns non-zero
# when the line is missing, or when the two fields are not integers —
# the caller logs either as a probe failure.
#
# The numeric guard is not paranoia. iputils prints `200 packets
# transmitted, 199 received,` so the field before `received,` is the
# count, but BusyBox prints `200 packets transmitted, 200 packets
# received,` — there the field before it is the word `packets`, which
# awk coerces to 0 and turns into a full-loss reading. CLIENT is an
# environment override, so an Alpine/BusyBox probe container would make
# both arms report loss=PROBE_COUNT and blame the drain path for an
# image mismatch. Fail loudly instead.
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

# Run the probe in the foreground for the full PROBE_COUNT, with the
# kill delivered asynchronously after PROBE_PRELUDE seconds. The probe
# generator runs `ping -i ${PROBE_INTERVAL} -c ${PROBE_COUNT}` inside
# ${CLIENT}. The kill_cmd is invoked by a backgrounded `(sleep …; …)`.
#
# The function captures stdout+stderr to ${probe_out} and returns the
# loss count on stdout (via parse_ping_loss). Returns non-zero if the
# ping output is unparseable.
#
# Note on ping interval: 0.1s spacing requires root on Linux's iputils
# ping (the floor for unprivileged users is 0.2s). client-1 is built
# from netshoot which runs as root by default, so 0.1 is allowed.
run_probe_and_kill() {
    local arm_label="$1"
    local probe_out="$2"
    local -a kill_cmd=("${@:3}")

    log "[${arm_label}] starting probe: ping -i ${PROBE_INTERVAL} -c ${PROBE_COUNT} ${FIP} from ${CLIENT}"
    # The probe is run in the background so the kill scheduler can run
    # concurrently. `ping` exits non-zero on packet loss; we parse the
    # summary block ourselves, so we don't let the non-zero exit abort
    # the script.
    docker exec "${CLIENT}" \
        ping -i "${PROBE_INTERVAL}" -c "${PROBE_COUNT}" -W 1 "${FIP}" \
        >"${probe_out}" 2>&1 &
    local probe_pid=$!

    # Schedule the kill after PROBE_PRELUDE seconds so it lands inside
    # the probe window. The kill's stdout is redirected to /dev/null
    # because `docker kill -s KILL <name>` echoes the container name
    # back (`clab-ovn-e2e-gateway-1`) — without the redirect that
    # echo would land on the function's stdout and corrupt the loss
    # value the caller captures via `$(...)`. stderr stays open so
    # genuine errors still surface.
    (
        sleep "${PROBE_PRELUDE}"
        log "[${arm_label}] firing kill: ${kill_cmd[*]}"
        "${kill_cmd[@]}" >/dev/null \
            || log "[${arm_label}] kill command returned non-zero (continuing)"
    ) &
    local killer_pid=$!

    # Wait for both the probe and the kill scheduler before parsing.
    # Probe is the long pole; the kill scheduler exits ~PROBE_PRELUDE+ε
    # after start. Both are best-effort: failures are surfaced by the
    # subsequent parse step, not by `wait`'s exit code.
    wait "${probe_pid}" || true
    wait "${killer_pid}" || true

    local loss
    if ! loss=$(parse_ping_loss "${probe_out}"); then
        log "[${arm_label}] ERROR: could not parse ping output at ${probe_out}"
        return 1
    fi
    printf '%s' "${loss}"
}

# Recycle the lab between arms (and on teardown when the lab was
# perturbed). Skipped when SKIP_RECYCLE=1 — useful for local debugging
# of a single arm, but the second arm will not behave correctly
# afterwards.
recycle_lab() {
    if [ "${SKIP_RECYCLE}" = "1" ]; then
        log "SKIP_RECYCLE=1: leaving lab as-is (second arm / next run will misbehave)"
        return 0
    fi
    log "recycling the lab (make e2e-down && make e2e-up)"
    if ! make -C "${REPO_ROOT}" e2e-down; then
        log "WARN: make e2e-down returned non-zero (continuing)"
    fi
    make -C "${REPO_ROOT}" e2e-up
}

# Best-effort EXIT trap: when the lab is perturbed (an arm ran) recycle
# it so a developer running `make e2e-drain-hitless` is left with a
# baseline-green lab. CI does not rely on this; its job-level
# `if: always()` step runs `make e2e-down` after the scenario regardless.
teardown() {
    local exit_code=$?
    if [ "${LAB_PERTURBED}" = "0" ]; then
        return "${exit_code}"
    fi
    log "teardown: lab was perturbed by an arm — recycling so the lab is left baseline-green"
    if ! recycle_lab; then
        log "teardown: recycle_lab returned non-zero (lab may be left in a degraded state)"
    fi
    return "${exit_code}"
}

# Snapshot the SB Port_Binding chassis assignment for cr-lr0-public.
# Used at the top and bottom of each arm to bundle the transition
# timeline into the artifact directory.
snapshot_port_binding() {
    local label="$1"
    {
        printf '# cr-lr0-public Port_Binding at %s\n\n' "${label}"
        sbctl find Port_Binding logical_port="${CR_PORT}" 2>&1 || true
        printf '\n# resolved chassis name: %s\n' "$(current_master)"
    } | write_artifact "drain-hitless/${label}.txt"
}

# Capture the drain log lines from the killed gateway's `docker logs`.
# The container has already exited by the time we read this, so logs
# are the only available signal. Returns the matched lines on stdout
# (one per chassis entry that was drained); returns non-zero if no
# line matched.
capture_drain_log() {
    local out
    out=$(docker logs "${MASTER_NODE}" 2>&1 \
        | grep -E 'msg="drain: gateway chassis priority lowered"' || true)
    if [ -z "${out}" ]; then
        return 1
    fi
    printf '%s\n' "${out}"
}

# Run the graceful arm. Returns the measured loss count on stdout;
# non-zero exit means the arm itself did not run cleanly (parse error,
# missing drain log line, no re-election). The pass/fail of the overall
# scenario is decided by `assert_hitless` after both arms have run.
arm_graceful() {
    local before
    before="$(current_master)"
    log "graceful arm: ${CR_PORT} bound to ${before:-<none>} before kill (expected: ${MASTER})"
    if [ "${before}" != "${MASTER}" ]; then
        log "graceful arm: WARNING — expected master ${MASTER}, observed ${before:-<none>}; continuing"
    fi
    snapshot_port_binding "graceful-port-binding-before"

    # Keep the container down after the agent exits so the migration
    # check is not racing containerlab's `restart: always` policy
    # bringing gateway-1 back and triggering RestoreDrainedGateways.
    disable_container_restart "${MASTER_NODE}"

    local probe_out loss
    probe_out="$(mktemp)"
    if ! loss=$(run_probe_and_kill "graceful" "${probe_out}" \
        docker exec "${MASTER_NODE}" kill -TERM 1); then
        cat "${probe_out}" | write_artifact "drain-hitless/graceful-ping.txt"
        rm -f "${probe_out}"
        return 1
    fi
    cat "${probe_out}" | write_artifact "drain-hitless/graceful-ping.txt"
    rm -f "${probe_out}"

    snapshot_port_binding "graceful-port-binding-after"

    # Re-election must have happened — otherwise a 0-loss reading is
    # meaningless (the lab never failed over). Use the same guard
    # failover.sh applies.
    local after
    if ! after=$(wait_for_remaster); then
        log "graceful arm: ERROR — ${CR_PORT} did not migrate away from ${MASTER} within ${FAILOVER_TIMEOUT}s"
        return 1
    fi
    log "graceful arm: ${CR_PORT} migrated ${MASTER} -> ${after}"

    # Acceptance: prove DrainGateways actually ran. Without this log
    # line, a 0-loss reading could be explained by the agent racing
    # the kernel teardown rather than by the drain code path.
    local drain_log
    if ! drain_log=$(capture_drain_log); then
        log "graceful arm: ERROR — no 'drain: gateway chassis priority lowered' line in ${MASTER_NODE} logs"
        log "graceful arm:        was the lab deployed with E2E_DRAIN_ON_SHUTDOWN=true make e2e-up?"
        printf '# no drain log line found\n' | write_artifact "drain-hitless/graceful-drain-log.txt"
        return 1
    fi
    log "graceful arm: drain code path confirmed:"
    printf '%s\n' "${drain_log}" | sed 's/^/    /' >&2
    printf '%s\n' "${drain_log}" | write_artifact "drain-hitless/graceful-drain-log.txt"

    log "graceful arm: measured packet loss = ${loss}"
    printf '%s' "${loss}"
}

# Run the hardkill arm (control). Same probe shape; the kill is
# `docker kill -s KILL`, which destroys the container's netns and the
# upstream veth pair — the lab MUST be recycled afterwards.
arm_hardkill() {
    local before
    before="$(current_master)"
    log "hardkill arm: ${CR_PORT} bound to ${before:-<none>} before kill (expected: ${MASTER})"
    if [ "${before}" != "${MASTER}" ]; then
        log "hardkill arm: WARNING — expected master ${MASTER}, observed ${before:-<none>}; continuing"
    fi
    snapshot_port_binding "hardkill-port-binding-before"

    # Keep the killed container down so OVN does not see gateway-1
    # reappear at priority 30 (which would re-claim cr-lr0-public)
    # while we are still measuring the failover.
    disable_container_restart "${MASTER_NODE}"

    local probe_out loss
    probe_out="$(mktemp)"
    if ! loss=$(run_probe_and_kill "hardkill" "${probe_out}" \
        docker kill -s KILL "${MASTER_NODE}"); then
        cat "${probe_out}" | write_artifact "drain-hitless/hardkill-ping.txt"
        rm -f "${probe_out}"
        return 1
    fi
    cat "${probe_out}" | write_artifact "drain-hitless/hardkill-ping.txt"
    rm -f "${probe_out}"

    snapshot_port_binding "hardkill-port-binding-after"

    local after
    if ! after=$(wait_for_remaster); then
        log "hardkill arm: ERROR — ${CR_PORT} did not migrate away from ${MASTER} within ${FAILOVER_TIMEOUT}s"
        return 1
    fi
    log "hardkill arm: ${CR_PORT} migrated ${MASTER} -> ${after}"

    log "hardkill arm: measured packet loss = ${loss}"
    printf '%s' "${loss}"
}

assert_hitless() {
    local graceful="$1"
    local hardkill="$2"
    {
        printf '# drain-hitless summary\n'
        printf 'graceful_loss=%s\n'  "${graceful}"
        printf 'hardkill_loss=%s\n'  "${hardkill}"
        printf 'graceful_max_loss=%s\n' "${GRACEFUL_MAX_LOSS}"
    } | write_artifact "drain-hitless/summary.txt"

    if ! (( graceful <= GRACEFUL_MAX_LOSS )); then
        log "ASSERTION FAILED: graceful_loss (${graceful}) > GRACEFUL_MAX_LOSS (${GRACEFUL_MAX_LOSS})"
        return 1
    fi
    if ! (( graceful < hardkill )); then
        log "ASSERTION FAILED: graceful_loss (${graceful}) is not strictly less than hardkill_loss (${hardkill})"
        log "    a non-negative delta is required for the hitless comparison to be meaningful"
        return 1
    fi
    log "ASSERT OK: graceful_loss=${graceful} <= ${GRACEFUL_MAX_LOSS} AND graceful_loss < hardkill_loss=${hardkill}"
}

main() {
    sanity_gate
    require_drain_enabled

    trap teardown EXIT

    local graceful_loss hardkill_loss

    log "===== graceful arm ====="
    # LAB_PERTURBED must be flipped in main(), not inside arm_graceful:
    # `graceful_loss=$(arm_graceful)` runs the arm in a $()-subshell,
    # so any assignment inside the function would not propagate back
    # to the main shell — and the EXIT trap (which decides whether to
    # recycle the lab) would skip the recycle and leave a developer
    # with a half-restarted gateway-1.
    LAB_PERTURBED=1
    graceful_loss=$(arm_graceful)
    log "graceful arm complete — recycling the lab for the hardkill control arm"

    # The graceful arm leaves gateway-1 drained (priority 0) with its
    # restart policy flipped to "no" and the container exited. The
    # lab is HA-asymmetric; the hardkill arm needs the priority-30
    # master back (and the restart policy back to "always", which
    # `make e2e-up` re-creates from the topology) so the two arms
    # exercise the same lab against the same FIP.
    recycle_lab
    # Recycle leaves the lab freshly bootstrapped; nothing to undo
    # yet, so reset the perturbation flag until the hardkill arm
    # flips it again.
    LAB_PERTURBED=0

    log "===== hardkill arm (control) ====="
    LAB_PERTURBED=1
    hardkill_loss=$(arm_hardkill)
    log "hardkill arm complete"

    assert_hitless "${graceful_loss}" "${hardkill_loss}"
}

main "$@"

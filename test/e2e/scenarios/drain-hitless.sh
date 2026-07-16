#!/usr/bin/env bash
# Graceful-drain vs hard-kill hitless comparison for the containerlab
# E2E harness.
#
# Goal (issue #113): when the active gateway agent receives SIGTERM
# with `drain_on_shutdown=true`, `DrainGateways` (see ovn_gateway.go)
# sets the local `Gateway_Chassis` priorities to
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
# `kill -TERM 1` is correct because PID 1 is tini (see
# `test/e2e/Dockerfile.gwnode`), which forwards the signal to the agent
# it supervises — the gwnode entrypoint `exec`s the agent at the end of
# its startup sequence, so the agent is tini's only child — and exits
# with it. No `pgrep`/`pidof` dance needed.
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
# integer loss counts directly from `ping`'s summary line — the loss
# count never depends on the packet capture. The kill is delivered
# after the probe has been running for `PROBE_PRELUDE` seconds so the
# transition lands inside the captured window. Every lost packet is
# one `PROBE_INTERVAL` in which the FIP did not answer, so
# `loss × PROBE_INTERVAL` is the data-plane outage the arm measured.
#
# Alongside the probe each arm records a once-per-second transition
# timeline pairing the SB `cr-lr0-public` Port_Binding chassis with
# the BGP nexthop `upstream` currently routes the FIP /32 to. It is
# the "BGP/SB Port_Binding transition timeline" the issue's artifact
# bundle asks for, and it is also what dates the migration for the RTO
# check below — so it runs on every invocation, not only when
# ARTIFACTS_DIR is set. Only when ARTIFACTS_DIR is set does an arm
# additionally record, best-effort, a `tcpdump` capture of the probe on
# `client-1`'s eth1, rendered to text so the ICMP sequence gap can be
# lined up against the kill timestamp. That capture can never fail an
# arm: the assertion below reads ping's summary.
#
# Assertion (issue #113's acceptance criteria; the budget re-derived
# per issue #182):
#
#   1. graceful outage <= GRACEFUL_MAX_OUTAGE_MS — the "hitless" claim.
#   2. graceful_loss    <  hardkill_loss — the delta that makes the
#      comparison meaningful (a no-op script returning `0 < 0` would
#      trivially fail).
#   3. GRACEFUL_MAX_LOSS <  hardkill_loss — the budget asserted in (1)
#      has to be one that a shutdown which did NOT drain would trip.
#      See assert_hitless() for why this third one exists.
#
# Where the budget comes from (issue #182)
# ----------------------------------------
# Issue #113 asked for "at most one lost packet" (a 100 ms budget at the
# default spacing) before any measurement existed, and the first
# calibration replaced that with 500 ms off a single 200 ms sample. One
# sample cannot establish a distribution: 500 ms derives a ceiling of 5
# packets, healthy runs routinely measured exactly 5, and the assertion
# passed with zero margin until ordinary runner noise failed unrelated
# pull requests.
#
# The 1500 ms default rests instead on the graceful-arm loss of every
# drain-hitless CI run carrying the drain takeover handshake (issue #129,
# agent commit 87ead87, on main since 2026-07-11) — n=20, runs
# 29141110469 through 29289492625, each run's
# `graceful arm: measured packet loss` line:
#
#      2 packets ( 200 ms)  ###
#      3 packets ( 300 ms)  #####
#      4 packets ( 400 ms)  ##
#      5 packets ( 500 ms)  ######
#      6 packets ( 600 ms)  ###
#     11 packets (1100 ms)  #
#
#     n=20   min 200 ms   median 450 ms   p90 600 ms   max 1100 ms
#
# 1500 ms clears the worst of those twenty by 1.36x. The ceiling is
# bounded from above too, and that is the half that keeps the scenario
# honest: the hard-kill control — same lab, same probe, a chassis that
# dies without draining — measured 23-30 packets (2300-3000 ms) across
# the same runs, in a far tighter band than the graceful arm's. 1500 ms
# sits at 65% of the tightest control run, so a shutdown that fails to
# drain still overshoots the budget by at least 8 packets. Assertion 3
# re-checks that on every run rather than trusting today's numbers to
# hold: raising GRACEFUL_MAX_OUTAGE_MS until it can no longer tell a
# broken drain from a working one now fails the scenario instead of
# quietly hollowing it out.
#
# If this budget starts failing again, re-derive it — collect the
# graceful loss across ~20 runs (every run prints it and bundles it into
# summary.txt) and set it from that distribution, recording the sample
# size here. Do not nudge it up to whatever the last red run produced.
#
# What the budget bounds
# ----------------------
# The lost packets sit in OVN's northd/ovn-controller flow-reprogramming
# window that follows the `Port_Binding` migration: between
# `cr-lr0-public` moving to gateway-2 and the gateways' flows converging
# on the new owner, `upstream` still ECMPs part of the traffic at a
# chassis that no longer owns the port.
#
# The agent bounds its own half of that window. Issue #129 added the
# drain takeover handshake: `DrainGateways` waits for the takeover
# chassis to stamp its NB readiness marker — its word that it has
# announced the FIP routes — and then holds `drain_settle_delay` before
# cleanup withdraws the leaving node's routes, the whole wait bounded by
# `drain_timeout`. That handshake is why the numbers above look the way
# they do. Before it landed, the arm was bimodal: three of the five
# pre-handshake runs (29104810149 on main, 29111714177, 29123397062)
# measured 48-49 packets — a ~4.9 s outage — while the other two
# measured 2-3. Across the twenty runs since, nothing has exceeded 11.
# What remains inside the budget is OVN's reprogramming on the takeover
# chassis, which the agent does not control; GRACEFUL_MAX_OUTAGE_MS is
# the budget this scenario holds that residue to, not a delay the agent
# implements.
#
# That 48-49-packet mode is also the concrete regression this budget has
# to keep catching, and the reason the ceiling is not simply parked below
# the control arm: a drain path that still runs — the log says
# `drain: complete`, capture_drain_log() is satisfied — but no longer
# delivers a hitless failover is exactly what assertion 1 is for. At 15
# packets it catches that recorded regression more than three times over.
#
# What it will NOT catch, stated plainly: a degradation inside the
# budget, say the median moving from 450 ms to 1000 ms. The arm's own
# spread on this runner class (200-1100 ms) is wider than such a shift,
# so no ceiling can separate the two without failing on the tail. The
# scenario resolves gross regressions, not gradual ones; watch the
# `graceful_outage_ms` trend in summary.txt for the latter.
#
# The scenario keeps the duration primary and derives the packet ceiling
# from `PROBE_INTERVAL`, so overriding the spacing cannot silently move
# the budget.
#
# Both arms must additionally end with `cr-lr0-public` migrated off
# the master within FAILOVER_TIMEOUT of the kill — the same RTO #105
# asserts for this lab, dated from the transition timeline (see
# `wait_for_failover`) — and with `client-1 → FIP` answering again.
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
# scenario returns". CI sets RECYCLE_ON_EXIT=0 — its job-level
# `make e2e-down` step (`if: always()`) tears the lab down anyway,
# so paying for a third `containerlab deploy` would only eat into
# the job's time budget and would destroy the perturbed lab
# state collect-artifacts.sh wants to dump on failure.
#
# Environment overrides:
#   LAB              container-name prefix (default ovn-e2e)
#   MASTER           chassis to kill (default gateway-1, the priority-30 chassis)
#   MASTER_NODE      container name (default clab-${LAB}-${MASTER})
#   FIP              target FIP (default 192.0.2.10, the priority-30 chassis FIP)
#   CLIENT           probe source container (default clab-${LAB}-client-1)
#   CENTRAL          OVN central container (default clab-${LAB}-central)
#   UPSTREAM         upstream BGP router container (default clab-${LAB}-upstream)
#   LR_PUBLIC_PORT   LR external port name (default lr0-public)
#   PROBE_INTERVAL   ping -i value (default 0.1)
#   PROBE_COUNT      ping -c value (default 200)
#   PROBE_PRELUDE    seconds to let the probe run before the kill (default 3)
#   FAILOVER_TIMEOUT seconds after the kill for the port to migrate AND
#                    reachability to return (default 30 — the RTO #105 asserts)
#   GRACEFUL_MAX_OUTAGE_MS data-plane outage budget for the graceful arm
#                    (default 1500 — derived from the graceful-arm loss of 20
#                    CI runs, see the assertion block comment above). The
#                    packet ceiling GRACEFUL_MAX_LOSS is derived from it and
#                    PROBE_INTERVAL. Raising it to triage a slow lab is fine
#                    up to a point: a value the hard-kill control arm would
#                    itself pass fails the run by design, because a budget
#                    that cannot tell a broken drain from a working one
#                    asserts nothing.
#   ARTIFACTS_DIR    directory to write the per-arm artifacts into (default empty = skip)
#   SANITY_GATE      run baseline.sh first when 1 (default 1)
#   SKIP_RECYCLE     skip `make e2e-down && make e2e-up` between arms / on teardown when 1 (default 0)
#                    Intended for local debugging only — the second arm will misbehave without a fresh lab.
#   RECYCLE_ON_EXIT  recycle the lab from the EXIT trap when 1 (default 1; CI sets 0)

set -euo pipefail

LAB="${LAB:-ovn-e2e}"
MASTER="${MASTER:-gateway-1}"
MASTER_NODE="${MASTER_NODE:-clab-${LAB}-${MASTER}}"
FIP="${FIP:-192.0.2.10}"
CLIENT="${CLIENT:-clab-${LAB}-client-1}"
CENTRAL="${CENTRAL:-clab-${LAB}-central}"
UPSTREAM="${UPSTREAM:-clab-${LAB}-upstream}"
LR_PUBLIC_PORT="${LR_PUBLIC_PORT:-lr0-public}"
CR_PORT="cr-${LR_PUBLIC_PORT}"
PROBE_INTERVAL="${PROBE_INTERVAL:-0.1}"
PROBE_COUNT="${PROBE_COUNT:-200}"
PROBE_PRELUDE="${PROBE_PRELUDE:-3}"
FAILOVER_TIMEOUT="${FAILOVER_TIMEOUT:-30}"
GRACEFUL_MAX_OUTAGE_MS="${GRACEFUL_MAX_OUTAGE_MS:-1500}"
ARTIFACTS_DIR="${ARTIFACTS_DIR:-}"
SANITY_GATE="${SANITY_GATE:-1}"
SKIP_RECYCLE="${SKIP_RECYCLE:-0}"
RECYCLE_ON_EXIT="${RECYCLE_ON_EXIT:-1}"

# Packet-loss ceiling for the graceful arm, derived so it can never drift
# from the outage budget it is meant to express: at -i 0.1 a `<= 1 packet`
# rule is a 100 ms budget, but at the 0.2 s unprivileged iputils floor the
# same rule would silently buy the agent 200 ms.
#
# ceil() and not floor(): an outage of D ms sampled every i ms costs at most
# ceil(D/i) packets, so ceil is the bound that never fails an arm which
# stayed inside the budget. The flip side is that a coarser PROBE_INTERVAL
# cannot resolve a finer budget — which is why the defaults probe at the
# finest interval iputils allows.
GRACEFUL_MAX_LOSS="$(awk -v ms="${GRACEFUL_MAX_OUTAGE_MS}" -v i="${PROBE_INTERVAL}" \
    'BEGIN { n = ms / (i * 1000); print (n == int(n) ? n : int(n) + 1) }')"

SCENARIOS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCENARIOS_DIR}/../../.." && pwd)"
BASELINE="${BASELINE:-${SCENARIOS_DIR}/baseline.sh}"

# Tracks whether the current lab has been perturbed by an arm; the
# EXIT trap only recycles when this is set, so a sanity-gate failure
# does not pay a needless `make e2e-up` cost.
LAB_PERTURBED=0

# Path of a file holding the PID of the background transition-timeline
# sampler for the arm that is currently running; empty content when no
# sampler is active. Set by main().
#
# A file and not a variable for the same reason main() hoists LAB_PERTURBED:
# the arms run as `loss=$(arm_graceful)` command substitutions, so a PID
# assigned by start_timeline inside that subshell would never propagate back
# to wait_for_failover or to the EXIT trap — both of which have to reach the
# sampler.
TIMELINE_PID_FILE=""

log() { printf '[drain-hitless] %s\n' "$*" >&2; }

# Write `content` to `path` under ARTIFACTS_DIR when it is set; quietly
# no-op otherwise. Same pattern as stale-chassis.sh: `path` is a flat
# file name, because the CI job already points ARTIFACTS_DIR at the
# scenario's own `drain-hitless/` sub-bundle.
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
    mkdir -p "${ARTIFACTS_DIR}"
    cat >"${ARTIFACTS_DIR}/${path}"
}

sbctl() { docker exec "${CENTRAL}" ovn-sbctl "$@"; }

# The BGP nexthop(s) `upstream` currently routes the FIP /32 to,
# comma-joined. The per-gateway underlay /30s make this a proxy for
# "which gateway is advertising the FIP right now": 100.64.N.2 is
# gateway-N (see bootstrap.sh), and a transient two-nexthop reading is
# both gateways advertising mid-transition. Prints the empty string
# while no route is installed — exactly the window the timeline exists
# to show.
#
# Only the nexthop lines (`* 100.64.1.2, via eth1, weight 1`) are of
# interest: the `Known via "bgp"` header line also carries a `via`
# token, so the field before it has to look like an IPv4 address.
upstream_fip_nexthop() {
    docker exec "${UPSTREAM}" vtysh -c "show ip route ${FIP}/32" 2>/dev/null \
        | tr -d '\r' \
        | awk '
            {
                for (i = 2; i <= NF; i++) {
                    if ($i != "via") continue
                    nh = $(i-1)
                    gsub(/,/, "", nh)
                    if (nh ~ /^[0-9]+(\.[0-9]+){3}$/) {
                        hops = (hops == "" ? nh : hops "," nh)
                    }
                }
            }
            END { if (hops != "") print hops }' \
        || true
}

# Flip the container's Docker restart policy to "no" so the agent's
# exit (graceful or hard) leaves the container down instead of being
# re-launched immediately by Docker. Idempotent; best-effort — a
# failure here would only manifest as the same flapping the fix is
# trying to prevent, which then surfaces as a wait_for_failover
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

# Run baseline.sh against the freshly recycled lab. Unlike sanity_gate
# this is NOT optional: `make e2e-up` returns as soon as bootstrap has
# seeded NB, and the agents still need a reconcile cycle before
# `client-1 → FIP` answers. A probe started inside that window would
# charge the reconcile drops to the arm and inflate its loss count.
lab_ready_gate() {
    if [ ! -x "${BASELINE}" ]; then
        log "ERROR: baseline script not executable at ${BASELINE} — cannot gate the recycled lab"
        return 1
    fi
    log "waiting for the recycled lab to reach baseline-green before the next arm"
    "${BASELINE}"
}

# First transition-timeline row at or after ${kill_ts} that shows
# cr-lr0-public bound to a chassis which is neither `<none>` nor ${MASTER}
# — i.e. the sample in which the migration became visible. Prints
# `<epoch> <chassis>`; prints nothing when the timeline never recorded one.
#
# Rows whose first field is not an epoch are skipped: the sampler folds its
# stderr into the same file, so a transient `docker exec` error must not be
# mistaken for a chassis reading. Rows older than the kill are skipped so a
# lab that was already misconfigured (cr-lr0-public not on ${MASTER} before
# the kill) cannot date a "migration" to before the kill and pass with a
# negative delta.
first_migration_row() {
    local timeline="$1" kill_ts="$2"
    awk -v master="${MASTER}" -v kill_ts="${kill_ts}" '
        $1 !~ /^[0-9]+$/ { next }
        $1 < kill_ts { next }
        NF >= 3 && $3 != "<none>" && $3 != master { print $1, $3; exit }
    ' "${timeline}"
}

# Assert both failover signals returned within FAILOVER_TIMEOUT seconds of
# the kill — the same RTO issue #105 asserts for this lab:
#
#   1. cr-lr0-public is bound to a chassis other than ${MASTER}. This is
#      the false-pass guard failover.sh also applies: a low-loss reading is
#      meaningless if the lab never re-elected (OVS on the dead master can
#      keep executing stale flows for a while).
#   2. `client-1 → FIP` answers again, i.e. the data plane is actually
#      restored through the NEW master.
#
# Signal 1 is DATED from the transition timeline rather than sampled here.
# By the time an arm calls this function the probe has long drained: the
# kill fires PROBE_PRELUDE seconds into a probe that runs
# PROBE_COUNT × PROBE_INTERVAL seconds, so at the defaults we regain control
# ~17s after the kill. A sample taken then can only ever observe the end
# state, never the instant it was reached — which is what would make every
# FAILOVER_TIMEOUT below the probe's tail pass unconditionally. The timeline
# samples current_master once per second from before the kill until at least
# kill_ts + FAILOVER_TIMEOUT, so the first row naming another chassis dates
# the migration to within the sampler's 1s resolution.
#
# Signal 2 is only polled, not dated. The probe already measured how long
# the data plane was down (loss × PROBE_INTERVAL) but it cannot see past its
# own end: a hardkill arm that never recovered ends the probe having lost
# every packet after the kill, ~17s of outage, which would slip under a 30s
# budget. What remains unknown is therefore whether the FIP came back at
# all, and that is what this poll answers, bounded by the same deadline.
#
# Prints the new chassis name on stdout; returns non-zero when either signal
# missed the budget.
wait_for_failover() {
    local kill_ts="$1" timeline="$2"
    local deadline row="" migrated_ts who delta
    deadline=$(( kill_ts + FAILOVER_TIMEOUT ))

    while :; do
        row="$(first_migration_row "${timeline}" "${kill_ts}")"
        [ -n "${row}" ] && break
        (( $(date +%s) < deadline )) || break
        sleep 1
    done

    # The sampler buffers its last lines until it exits; stop it and read the
    # flushed file once more so a migration that landed just before the
    # deadline is not reported as one that never happened.
    stop_timeline
    if [ -z "${row}" ]; then
        row="$(first_migration_row "${timeline}" "${kill_ts}")"
    fi
    if [ -z "${row}" ]; then
        log "${CR_PORT} never migrated away from ${MASTER} within ${FAILOVER_TIMEOUT}s of the kill"
        log "last transition sample: $(tail -n 1 "${timeline}")"
        return 1
    fi

    migrated_ts="${row%% *}"
    who="${row##* }"
    delta=$(( migrated_ts - kill_ts ))
    if (( delta > FAILOVER_TIMEOUT )); then
        log "${CR_PORT} migrated to ${who} ${delta}s after the kill, over the ${FAILOVER_TIMEOUT}s budget"
        return 1
    fi

    while ! docker exec "${CLIENT}" ping -c 1 -W 1 "${FIP}" >/dev/null 2>&1; do
        if ! (( $(date +%s) < deadline )); then
            log "${CLIENT} → ${FIP} did not answer again within ${FAILOVER_TIMEOUT}s of the kill (${CR_PORT} is on ${who})"
            return 1
        fi
        sleep 1
    done

    log "${CR_PORT} migrated to ${who} ${delta}s after the kill (budget ${FAILOVER_TIMEOUT}s)"
    printf '%s' "${who}"
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

# Wall-clock seconds the probe occupies, plus slack for ping's startup
# and its summary block. Bounds the packet capture so it does not outlive
# its arm.
probe_window_seconds() {
    awk -v c="${PROBE_COUNT}" -v i="${PROBE_INTERVAL}" 'BEGIN { printf "%d", c * i + 10 }'
}

# Wall-clock seconds the transition timeline samples for. It must outlive
# both the probe and the kill-anchored FAILOVER_TIMEOUT budget: the timeline
# is what dates the migration, and a budget the sampler stopped short of
# could never be observed as exceeded.
timeline_window_seconds() {
    awk -v probe="$(probe_window_seconds)" -v prelude="${PROBE_PRELUDE}" \
        -v rto="${FAILOVER_TIMEOUT}" \
        'BEGIN { t = prelude + rto + 5; printf "%d", (t > probe ? t : probe) }'
}

# Milliseconds of data-plane outage a loss count stands for at the current
# probe spacing. The probe is the only clock here: every lost packet is one
# PROBE_INTERVAL in which the FIP did not answer.
outage_ms() {
    awk -v loss="$1" -v i="${PROBE_INTERVAL}" 'BEGIN { printf "%d", loss * i * 1000 }'
}

# Capture the probe on client-1's eth1 for the length of the arm. The
# capture is triage evidence only — the loss assertion reads ping's own
# summary — so every step here is best-effort and never fails an arm.
start_capture() {
    local arm="$1" window="$2"
    if [ -z "${ARTIFACTS_DIR}" ]; then
        return 0
    fi
    if ! docker exec -d "${CLIENT}" timeout "${window}" \
        tcpdump -i eth1 -n -U -w "/tmp/drain-hitless-${arm}.pcap" \
        "icmp and host ${FIP}" >/dev/null 2>&1; then
        log "[${arm}] WARN: could not start tcpdump on ${CLIENT} (continuing without a capture)"
        return 0
    fi
    # Give tcpdump a moment to attach so the first probe packet is not
    # missed and mis-read as a lost one during triage.
    sleep 1
}

# Render the capture to text. `-tt` prints absolute epoch timestamps so
# the ICMP sequence gap can be lined up against the kill timestamp the
# arm records; `-nn` keeps addresses numeric.
stop_capture() {
    local arm="$1"
    if [ -z "${ARTIFACTS_DIR}" ]; then
        return 0
    fi
    docker exec "${CLIENT}" pkill -f 'tcpdump -i eth1' >/dev/null 2>&1 || true
    if ! docker exec "${CLIENT}" \
        tcpdump -nn -tt -r "/tmp/drain-hitless-${arm}.pcap" 2>/dev/null \
        | write_artifact "${arm}-icmp-seq.txt"; then
        log "[${arm}] WARN: no capture to render (tcpdump unavailable or pcap missing)"
    fi
}

# Sample the control-plane transition once per second into ${path}: which
# chassis anchors cr-lr0-public in SB, and which BGP nexthop `upstream`
# routes the FIP /32 to.
#
# This does two jobs. It is the "BGP/SB Port_Binding transition timeline"
# the issue's artifact bundle asks for — it shows where in the transition
# the lost packets sat — and it is the record wait_for_failover dates the
# migration from. The second job is why it is not gated on ARTIFACTS_DIR
# like the packet capture is: a local `make e2e-drain-hitless` must hold the
# agent to the same RTO the CI job does.
start_timeline() {
    local arm="$1" path="$2" window
    window="$(timeline_window_seconds)"
    log "[${arm}] sampling the transition timeline for ${window}s"
    (
        local deadline who nexthop
        deadline=$(( $(date +%s) + window ))
        printf '# epoch utc sb_chassis(%s) upstream_bgp_nexthop(%s)\n' "${CR_PORT}" "${FIP}"
        while (( $(date +%s) < deadline )); do
            who="$(current_master)"
            nexthop="$(upstream_fip_nexthop)"
            printf '%s %s %s %s\n' "$(date +%s)" "$(date -u +%H:%M:%S)" \
                "${who:-<none>}" "${nexthop:-<none>}"
            sleep 1
        done
    ) >"${path}" 2>&1 &
    printf '%s' "$!" >"${TIMELINE_PID_FILE}"
}

# Stop the sampler, if one is running. Idempotent, and safe to call from a
# shell that is not the sampler's parent: `wait` on a non-child is an error
# the sampler's own exit makes moot, since every timeline line is flushed as
# it is printed.
stop_timeline() {
    local pid
    pid="$(cat "${TIMELINE_PID_FILE}" 2>/dev/null || true)"
    if [ -n "${pid}" ]; then
        kill "${pid}" >/dev/null 2>&1 || true
        wait "${pid}" 2>/dev/null || true
        : >"${TIMELINE_PID_FILE}"
    fi
}

# Run the probe in the foreground for the full PROBE_COUNT, with the
# kill delivered asynchronously after PROBE_PRELUDE seconds. The probe
# generator runs `ping -i ${PROBE_INTERVAL} -c ${PROBE_COUNT}` inside
# ${CLIENT}. The kill_cmd is invoked by a backgrounded `(sleep …; …)`,
# which stamps ${kill_ts_file} with the epoch second it fired so the
# caller can measure the RTO from the kill rather than from probe end.
#
# The transition timeline is started here but deliberately NOT stopped:
# wait_for_failover reads it after the probe has drained and stops the
# sampler itself, so the timeline spans the whole kill-anchored budget.
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
    local kill_ts_file="$3"
    local timeline="$4"
    local -a kill_cmd=("${@:5}")

    start_capture "${arm_label}" "$(probe_window_seconds)"
    start_timeline "${arm_label}" "${timeline}"

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
        date +%s >"${kill_ts_file}"
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

    stop_capture "${arm_label}"

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
    # An arm that failed before wait_for_failover leaves the sampler alive;
    # it would keep `docker exec`ing into a lab this trap is about to
    # recycle. Idempotent when the sampler already stopped.
    stop_timeline
    rm -f "${TIMELINE_PID_FILE}"
    if [ "${LAB_PERTURBED}" = "0" ]; then
        return "${exit_code}"
    fi
    if [ "${RECYCLE_ON_EXIT}" != "1" ]; then
        log "teardown: RECYCLE_ON_EXIT=${RECYCLE_ON_EXIT} — leaving the perturbed lab for the caller to tear down"
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
        printf '# upstream BGP nexthop for %s: %s\n' \
            "${FIP}" "$(upstream_fip_nexthop)"
    } | write_artifact "${label}.txt"
}

# Capture the drain log lines from the killed gateway's `docker logs`.
# The container has already exited by the time we read this, so logs
# are the only available signal.
#
# `drain: gateway chassis priority lowered` is emitted inside the
# op-building loop of `DrainGateways` (ovn_gateway.go), BEFORE the ops
# are handed to `transactOps` — it proves the drain path was entered,
# not that the priority-0 update ever reached the NB DB. When the
# transaction fails, `DrainGateways` returns an error, agent.go logs
# `drain failed` and the agent exits anyway: no priority was lowered
# and the gateway dies exactly like a hard kill. Accepting the
# `lowered` line alone would bundle an artifact that affirms a drain
# that never happened, sending a maintainer after OVN's migration path
# while the real cause sits ungrepped in the logs.
#
# `drain: complete` is the only line emitted after the transaction
# committed AND the SB poll observed the port migrate off this chassis,
# so require it. `drain: timeout exceeded` means the agent gave up and
# exited with the port still bound here — the drain did not deliver the
# migration the graceful arm measures.
#
# Every matched line — failures included — goes to stdout so the
# artifact bundle names the real cause. Returns non-zero unless the log
# proves a committed, migrated drain.
capture_drain_log() {
    local logs lowered completed failed
    logs="$(docker logs "${MASTER_NODE}" 2>&1 || true)"
    lowered="$(printf '%s\n' "${logs}" \
        | grep -E 'msg="drain: gateway chassis priority lowered"' || true)"
    completed="$(printf '%s\n' "${logs}" \
        | grep -E 'msg="drain: complete' || true)"
    failed="$(printf '%s\n' "${logs}" \
        | grep -E 'msg="drain failed"|msg="drain: timeout exceeded' || true)"

    if [ -n "${lowered}" ]; then
        printf '%s\n' "${lowered}"
    fi
    if [ -n "${failed}" ]; then
        printf '%s\n' "${failed}"
    fi
    if [ -n "${completed}" ]; then
        printf '%s\n' "${completed}"
    fi

    if [ -z "${lowered}" ] || [ -z "${completed}" ] || [ -n "${failed}" ]; then
        return 1
    fi
}

# Read the epoch second the kill fired, as stamped by the killer
# subshell just before it fired. A missing stamp means the subshell
# died before reaching `date`, which should never happen; fall back to
# "now" (a laxer RTO budget) and say so loudly rather than aborting an
# arm whose measurement is already in hand.
read_kill_ts() {
    local file="$1" ts
    ts="$(cat "${file}" 2>/dev/null || true)"
    if [ -z "${ts}" ]; then
        log "WARN: kill timestamp was never stamped; measuring the RTO from now"
        ts="$(date +%s)"
    fi
    printf '%s' "${ts}"
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

    local probe_out kill_ts_file timeline loss kill_ts
    probe_out="$(mktemp)"
    kill_ts_file="$(mktemp)"
    timeline="$(mktemp)"
    if ! loss=$(run_probe_and_kill "graceful" "${probe_out}" "${kill_ts_file}" "${timeline}" \
        docker exec "${MASTER_NODE}" kill -TERM 1); then
        stop_timeline
        write_artifact "graceful-ping.txt" <"${probe_out}"
        write_artifact "graceful-transition-timeline.txt" <"${timeline}"
        rm -f "${probe_out}" "${kill_ts_file}" "${timeline}"
        return 1
    fi
    write_artifact "graceful-ping.txt" <"${probe_out}"
    kill_ts="$(read_kill_ts "${kill_ts_file}")"
    rm -f "${probe_out}" "${kill_ts_file}"

    snapshot_port_binding "graceful-port-binding-after"

    # Re-election must have happened inside #105's RTO and the data plane
    # must be back — otherwise a 0-loss reading is meaningless (the lab
    # never failed over). Same guard failover.sh applies. wait_for_failover
    # stops the timeline sampler, so the artifact is complete afterwards.
    local after rc=0
    after="$(wait_for_failover "${kill_ts}" "${timeline}")" || rc=$?
    write_artifact "graceful-transition-timeline.txt" <"${timeline}"
    rm -f "${timeline}"
    if (( rc != 0 )); then
        log "graceful arm: ERROR — the SIGTERM did not produce a failover inside the ${FAILOVER_TIMEOUT}s budget"
        return 1
    fi
    log "graceful arm: ${CR_PORT} migrated ${MASTER} -> ${after}, ${FIP} reachable again"

    # Acceptance: prove DrainGateways actually committed the priority
    # update and saw the port migrate. Without that, a 0-loss reading
    # could be explained by the agent racing the kernel teardown rather
    # than by the drain code path.
    local drain_log drain_rc=0
    drain_log="$(capture_drain_log)" || drain_rc=$?
    if [ -n "${drain_log}" ]; then
        printf '%s\n' "${drain_log}" | write_artifact "graceful-drain-log.txt"
    else
        printf '# no drain log lines found\n' | write_artifact "graceful-drain-log.txt"
    fi
    if (( drain_rc != 0 )); then
        log "graceful arm: ERROR — ${MASTER_NODE} logs do not show a drain that committed and migrated"
        log "graceful arm:        want both 'drain: gateway chassis priority lowered' and 'drain: complete', and neither 'drain failed' nor 'drain: timeout exceeded'"
        log "graceful arm:        was the lab deployed with E2E_DRAIN_ON_SHUTDOWN=true make e2e-up?"
        printf '%s\n' "${drain_log:-<no drain lines at all>}" | sed 's/^/    /' >&2
        return 1
    fi
    log "graceful arm: drain code path confirmed:"
    printf '%s\n' "${drain_log}" | sed 's/^/    /' >&2

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

    local probe_out kill_ts_file timeline loss kill_ts
    probe_out="$(mktemp)"
    kill_ts_file="$(mktemp)"
    timeline="$(mktemp)"
    if ! loss=$(run_probe_and_kill "hardkill" "${probe_out}" "${kill_ts_file}" "${timeline}" \
        docker kill -s KILL "${MASTER_NODE}"); then
        stop_timeline
        write_artifact "hardkill-ping.txt" <"${probe_out}"
        write_artifact "hardkill-transition-timeline.txt" <"${timeline}"
        rm -f "${probe_out}" "${kill_ts_file}" "${timeline}"
        return 1
    fi
    write_artifact "hardkill-ping.txt" <"${probe_out}"
    kill_ts="$(read_kill_ts "${kill_ts_file}")"
    rm -f "${probe_out}" "${kill_ts_file}"

    snapshot_port_binding "hardkill-port-binding-after"

    local after rc=0
    after="$(wait_for_failover "${kill_ts}" "${timeline}")" || rc=$?
    write_artifact "hardkill-transition-timeline.txt" <"${timeline}"
    rm -f "${timeline}"
    if (( rc != 0 )); then
        log "hardkill arm: ERROR — the SIGKILL did not produce a failover inside the ${FAILOVER_TIMEOUT}s budget"
        return 1
    fi
    log "hardkill arm: ${CR_PORT} migrated ${MASTER} -> ${after}, ${FIP} reachable again"

    log "hardkill arm: measured packet loss = ${loss}"
    printf '%s' "${loss}"
}

assert_hitless() {
    local graceful="$1"
    local hardkill="$2"
    local graceful_ms hardkill_ms budget_headroom
    graceful_ms="$(outage_ms "${graceful}")"
    hardkill_ms="$(outage_ms "${hardkill}")"
    # How far the ceiling sits below what a shutdown that did not drain
    # costs on this very lab, in packets. Negative or zero means the
    # budget has stopped discriminating — see the guard below.
    budget_headroom=$(( hardkill - GRACEFUL_MAX_LOSS ))
    {
        printf '# drain-hitless summary\n'
        printf 'probe_interval=%s\n' "${PROBE_INTERVAL}"
        printf 'graceful_loss=%s\n'  "${graceful}"
        printf 'graceful_outage_ms=%s\n' "${graceful_ms}"
        printf 'hardkill_loss=%s\n'  "${hardkill}"
        printf 'hardkill_outage_ms=%s\n' "${hardkill_ms}"
        printf 'graceful_max_outage_ms=%s\n' "${GRACEFUL_MAX_OUTAGE_MS}"
        printf 'graceful_max_loss=%s\n' "${GRACEFUL_MAX_LOSS}"
        printf 'hitless_gain_packets=%s\n' "$(( hardkill - graceful ))"
        printf 'budget_headroom_packets=%s\n' "${budget_headroom}"
    } | write_artifact "summary.txt"

    # Guard the budget itself, before judging the arm against it.
    #
    # The hard-kill arm is this scenario's own negative control: it is what
    # a shutdown that does not drain costs, measured on the same lab, the
    # same probe and the same run. So the budget is only worth asserting
    # while a hard kill would still trip it — the moment
    # GRACEFUL_MAX_LOSS reaches hardkill_loss, a completely broken drain
    # path passes assertion 1 and the "hitless" claim means nothing.
    #
    # That is the failure mode issue #182 warned about: raising
    # GRACEFUL_MAX_OUTAGE_MS until the red goes away deletes the property
    # the scenario exists to hold, and it does so silently, because the
    # run turns green. Checking it here makes the weakening loud. It also
    # cannot be satisfied by tuning: hardkill_loss is measured, not
    # configured.
    #
    # No invented safety factor on top. `GRACEFUL_MAX_LOSS < hardkill_loss`
    # is exactly the condition "a non-draining shutdown fails this
    # scenario", and a factor above it would be a second budget nobody
    # measured — the margin that IS measured lives in the derivation of
    # GRACEFUL_MAX_OUTAGE_MS (see the assertion block at the top).
    if ! (( GRACEFUL_MAX_LOSS < hardkill )); then
        log "ASSERTION FAILED: the graceful budget cannot discriminate a broken drain on this lab"
        log "    the budget is ${GRACEFUL_MAX_OUTAGE_MS} ms = ${GRACEFUL_MAX_LOSS} packets at -i ${PROBE_INTERVAL}, but this run's"
        log "    hard-kill control — a chassis that died WITHOUT draining — lost only ${hardkill} packets (~${hardkill_ms} ms)."
        log "    A budget that a hard kill itself would pass asserts nothing about the drain path."
        log "    Either the budget was raised too far, or the control arm reconverged unusually fast; do not"
        log "    raise GRACEFUL_MAX_OUTAGE_MS to clear this — re-derive it from a multi-run distribution (issue #182)."
        return 1
    fi

    if ! (( graceful <= GRACEFUL_MAX_LOSS )); then
        log "ASSERTION FAILED: the graceful arm lost ${graceful} packets (~${graceful_ms} ms of outage), over the ${GRACEFUL_MAX_OUTAGE_MS} ms budget"
        log "    at -i ${PROBE_INTERVAL} that budget allows at most ${GRACEFUL_MAX_LOSS} lost packet(s)"
        log "    the budget rests on a 20-run distribution (200-1100 ms); a single run above it is a real regression,"
        log "    not noise to tune away — the hard-kill control on this run lost ${hardkill} packets (~${hardkill_ms} ms) for comparison"
        return 1
    fi
    if ! (( graceful < hardkill )); then
        log "ASSERTION FAILED: graceful_loss (${graceful}) is not strictly less than hardkill_loss (${hardkill})"
        log "    a positive delta is required for the hitless comparison to be meaningful"
        return 1
    fi
    log "ASSERT OK: graceful outage ~${graceful_ms} ms <= ${GRACEFUL_MAX_OUTAGE_MS} ms AND graceful_loss=${graceful} < hardkill_loss=${hardkill}"
    log "    budget headroom: the ${GRACEFUL_MAX_LOSS}-packet ceiling sits ${budget_headroom} packets below this run's hard-kill control (${hardkill})"
}

main() {
    sanity_gate
    require_drain_enabled

    TIMELINE_PID_FILE="$(mktemp)"
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
    lab_ready_gate

    log "===== hardkill arm (control) ====="
    LAB_PERTURBED=1
    hardkill_loss=$(arm_hardkill)
    log "hardkill arm complete"

    assert_hitless "${graceful_loss}" "${hardkill_loss}"
}

main "$@"

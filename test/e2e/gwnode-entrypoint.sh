#!/usr/bin/env bash
# Entrypoint for the containerlab gateway-node image.
#
# Starts the supporting daemons in a fixed order — OVS local DB, OVS
# vswitchd (userspace datapath), ovn-controller, FRR — then execs the
# ovn-network-agent in the foreground. tini is PID 1 (see
# Dockerfile.gwnode) and runs this script as its only child, so the
# container's lifecycle stays tied to the agent process — and the
# daemons that daemonize out of this script get reaped when they exit
# instead of lingering as zombies under the non-reaping agent.

set -Eeuo pipefail

log() { printf '[gwnode] %s\n' "$*" >&2; }

# `set -e` kills this script without a word when an unguarded command fails
# (tini exits with its child, taking the container down), and Docker's
# `restart: always` replaces the container so fast that the crash
# is invisible — the restarted entrypoint's output just continues the log.
# That silence is what made the eth1-destroying restart race (see the
# daemon-start block below) cost days to root-cause. The trap cannot stop
# the exit, but it names the dying command and line in `docker logs`.
# `set -E` extends the trap into functions.
trap 'log "FATAL: entrypoint exiting at line ${LINENO}: ${BASH_COMMAND} (exit $?)"' ERR

# Use the chassis name the OVN central bootstrap script seeds. Falls back
# to the container hostname so the image still works when launched outside
# the canned topology.
CHASSIS_NAME="${CHASSIS_NAME:-$(hostname -s)}"
OVN_SB_REMOTE="${OVN_SB_REMOTE:-tcp:central:6642}"
# Encap IP must be unique per chassis: containerlab puts every node on
# the management network as `eth0`, so picking that address gives each
# gateway a routable, distinct geneve endpoint. The earlier 127.0.0.1
# default collided across the three gateways and only the first
# registration "stuck" in SB, which broke gateway_chassis HA priorities
# (cr-lr0-public landed on whichever chassis happened to register
# first, not on the priority-30 master).
ENCAP_IP="${ENCAP_IP:-$(ip -o -4 addr show eth0 | awk '{print $4}' | cut -d/ -f1)}"
BRIDGE_DEV="${BRIDGE_DEV:-br-ex}"
BRIDGE_MAPPING="${BRIDGE_MAPPING:-physnet1:${BRIDGE_DEV}}"
VRF_NAME="${VRF_NAME:-vrf-provider}"
VRF_TABLE_ID="${VRF_TABLE_ID:-100}"

# ---------------------------------------------------------------------------
# Why no daemon start script's exit status may be fatal here
# ---------------------------------------------------------------------------
#
# This script runs under `set -e` AND it is tini's only child (main()
# `exec`s the agent at the end). So an unguarded command that returns non-zero
# does not merely abort a step: tini follows it down, the container exits, and
# Docker's `restart: always` policy — which containerlab applies to every node
# — restarts it.
#
# That restart is what destroys the lab. containerlab wires the veths about a
# second AFTER it creates the containers:
#
#     06:27:13 INFO Creating container name=gateway-1
#     06:27:14 INFO Created link: gateway-1:eth1 ▪┄┄▪ upstream:eth1
#
# A restart that lands before the link is created is harmless — containerlab
# wires eth1 into the new netns. A restart that lands after it takes eth1 down
# with the old netns, and the interface never comes back: bootstrap.sh then
# dies on `Cannot find device "eth1"` against a lab that can no longer be
# repaired. That one-second window is the whole of the intermittency; the crash
# itself is not rare at all.
#
# The crash is also silent. `set -e` prints nothing, and the RESTARTED
# container comes up healthy and re-registers its chassis in SB — so
# bootstrap's SB-registration gate passes and the real cause appears nowhere
# in the job log.
#
# Every daemon start below has this shape, and each one has already been
# observed killing PID 1 in CI:
#
#   * `ovs-ctl start` can return non-zero after ovsdb-server and vswitchd are
#     both up and answering.
#   * `frrinit.sh start` can return non-zero after watchfrr is up — its daemons
#     race the initial vtysh connect (`zebra state -> down : initial connection
#     attempt failed`, recovering a moment later).
#   * `ovn-ctl start_controller` has the same shape.
#
# None of these exit statuses is a readiness signal, and none of them needs to
# be: every one is already followed by a probe that polls for the state we
# actually depend on. So the rule for this whole section is —
#
#     an exit status is a hint, readiness is a probe, and giving up prints why.
#
# start_daemon() implements the first half, await_ready() the second.

# Run a daemon's start command, logging a non-zero exit instead of dying on it.
start_daemon() {
    local label="$1"
    shift
    if ! "$@"; then
        log "WARN: ${label} returned non-zero; deferring to the readiness probe"
    fi
}

# Poll until the daemon is actually usable, returning non-zero on timeout
# instead of dying. Only callers that can do something about a timeout use
# this directly — start_frr, which retries the start; every other daemon goes
# through await_ready.
probe_ready() {
    local attempts="$1"
    shift
    local i
    for (( i = 1; i <= attempts; i++ )); do
        if "$@" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# probe_ready for the daemons whose failure this script cannot recover from. A
# genuine failure must still fail, so a timeout exits — but it names the daemon
# and, when given a `diag` function, dumps its state first, so the next
# occurrence is root-causable from the container log alone.
#
# `diag` may be empty for daemons whose failure the probe itself makes obvious.
await_ready() {
    local label="$1" attempts="$2" diag="$3"
    shift 3
    if probe_ready "${attempts}" "$@"; then
        return 0
    fi
    log "ERROR: ${label} did not become ready within ${attempts}s"
    if [ -n "${diag}" ]; then
        "${diag}"
    fi
    exit 1
}

start_ovs() {
    log "starting Open vSwitch (kernel datapath)"
    mkdir -p /var/run/openvswitch /var/log/openvswitch /etc/openvswitch
    # The userspace datapath (datapath_type=netdev) sounded attractive
    # for a container-only lab, but OVN's chassisredirect election uses
    # BFD over the geneve tunnels between chassis and BFD never
    # converges with userspace OVS in this setup — cr-lrp gets claimed,
    # then immediately released, and the LR external port stays
    # unbound. Kernel OVS is the path that actually carries inter-chassis
    # traffic in containerlab. The host module is mounted into the
    # container via the /lib/modules:ro bind in topology.clab.yml, and
    # we best-effort modprobe it on startup in case the host did not
    # auto-load it.
    modprobe openvswitch 2>/dev/null || log "modprobe openvswitch failed (already loaded or built in?)"
    # ovs-ctl honours the existing conf.db when present and creates a new
    # one otherwise, which keeps re-runs idempotent.
    start_daemon "ovs-ctl start" \
        /usr/share/openvswitch/scripts/ovs-ctl --system-id="${CHASSIS_NAME}" start
    await_ready "Open vSwitch (ovs-vsctl)" 30 "" ovs-vsctl show
}

# ovn-controller resolves the SB remote's hostname with OVS's internal
# unbound-based resolver, which only queries the resolv.conf nameserver
# and never reads /etc/hosts. Docker's embedded DNS intermittently stops
# answering for a freshly recreated containerlab management network
# (observed after drain-hitless's `make e2e-down && make e2e-up`
# recycle: "dns_resolve|WARN|central: failed to resolve" for minutes),
# which strands ovn-controller in a reconnect loop so the chassis never
# registers in SB. glibc resolution keeps working throughout because
# containerlab writes the lab nodes into /etc/hosts, so resolve the
# hostname here once with getent and hand ovn-controller a numeric IP.
resolve_sb_remote() {
    local proto host port ip
    IFS=: read -r proto host port <<<"${OVN_SB_REMOTE}"
    if [[ -z "${host}" || -z "${port}" || "${host}" =~ ^[0-9.]+$ ]]; then
        return 0 # already numeric, or a form we do not understand — keep
    fi
    for _ in $(seq 1 30); do
        # getent exits non-zero while the name is still unregistered — the
        # very state this loop exists to wait out. Without the `|| true`,
        # pipefail turns that into a failed assignment and `set -e` kills
        # PID 1 on the first iteration, silently; when Docker's restart
        # then lands after containerlab has wired eth1, the veth dies with
        # the old netns and the lab is unrecoverable ("Cannot find device
        # eth1" in bootstrap). That single line caused 7 of 9 lab bring-up
        # failures between 2026-07-14 and 2026-07-16.
        ip="$(getent hosts "${host}" | awk '{print $1; exit}' || true)"
        if [ -n "${ip}" ]; then
            log "resolved SB remote ${host} -> ${ip} for ovn-controller"
            OVN_SB_REMOTE="${proto}:${ip}:${port}"
            return 0
        fi
        sleep 1
    done
    log "WARNING: ${host} did not resolve within 30s; leaving ovn-remote as-is"
}

configure_ovs() {
    # The encap IP is logged because it is the one value here that is not
    # fixed by the topology file: it is read off eth0 at every boot. When
    # the 2026-07-18 chaos run deadlocked two chassis on SB's unique
    # (type, ip) Encap index (issue #208), nothing in the container log
    # said which address each gateway had claimed, and the swap had to be
    # reconstructed from the collected OVS dumps.
    log "configuring Open_vSwitch external_ids for ovn-controller (chassis=${CHASSIS_NAME} encap-ip=${ENCAP_IP})"
    ovs-vsctl set Open_vSwitch . \
        external_ids:ovn-remote="${OVN_SB_REMOTE}" \
        external_ids:ovn-encap-type=geneve \
        external_ids:ovn-encap-ip="${ENCAP_IP}" \
        external_ids:system-id="${CHASSIS_NAME}" \
        external_ids:hostname="${CHASSIS_NAME}" \
        external_ids:ovn-bridge-mappings="${BRIDGE_MAPPING}"

    log "ensuring ${BRIDGE_DEV} exists"
    ovs-vsctl --may-exist add-br "${BRIDGE_DEV}"
    ip link set "${BRIDGE_DEV}" up
}

start_ovn_controller() {
    log "starting ovn-controller"
    start_daemon "ovn-ctl start_controller" \
        /usr/share/ovn/scripts/ovn-ctl start_controller
    # br-int is ovn-controller's own handiwork, so its existence — not
    # ovn-ctl's exit status — is the signal that the daemon is really running.
    await_ready "ovn-controller (br-int)" 30 "" ovs-vsctl br-exists br-int
}

setup_vrf() {
    # The agent's veth VRF leak feature attaches a veth peer to a kernel
    # VRF device (matches the production gateway layout). Create the
    # device here so the agent does not crash on first reconcile with
    # "Link not found".
    log "ensuring kernel VRF device ${VRF_NAME} (table ${VRF_TABLE_ID})"
    modprobe vrf 2>/dev/null || log "modprobe vrf failed (already loaded or built in?)"
    if ! ip link show "${VRF_NAME}" >/dev/null 2>&1; then
        ip link add "${VRF_NAME}" type vrf table "${VRF_TABLE_ID}"
    fi
    ip link set "${VRF_NAME}" up
}

setup_loopback() {
    # The agent's port-forward feature manages VIP /32 addresses on a
    # loopback device inside the provider VRF (default port_forward_dev:
    # `loopback1`). reconcilePortForwardVIPs() looks the device up
    # unconditionally as soon as `port_forwards:` is present in the
    # config — even when no VIP has `manage_vip: true` — so the agent
    # exits with "find device loopback1: Link not found" on startup if
    # the device is missing. Production hosts provision it via
    # systemd-networkd; the lab does not have systemd-networkd, so we
    # create it here as a dummy interface enslaved to ${VRF_NAME}.
    # Mirrors test/integration/setup.sh:create_loopback1 (the
    # integration harness has the same requirement). Idempotent: re-runs
    # on container restart simply re-assert the existing device.
    log "ensuring loopback1 dummy device in ${VRF_NAME}"
    if ! ip link show loopback1 >/dev/null 2>&1; then
        ip link add loopback1 type dummy
    fi
    ip link set loopback1 master "${VRF_NAME}" 2>/dev/null || true
    ip link set loopback1 up
}

FRR_READY_ATTEMPTS="${FRR_READY_ATTEMPTS:-30}"
FRR_CONFIG_ATTEMPTS="${FRR_CONFIG_ATTEMPTS:-30}"
# The daemons the entrypoint's config is addressed to. configure_frr reads
# the running config before it decides what to push, and vtysh renders that
# config from the daemons it happened to reach — so all of these must be in
# `show daemons` before the answer means anything. See frr_config_daemons_ready.
FRR_CONFIG_DAEMONS="${FRR_CONFIG_DAEMONS:-zebra bgpd staticd}"
# How many times start_frr may run the whole clean + start + probe cycle
# before it gives up and lets PID 1 die. See the retry loop for why the
# second attempt is worth its own FRR_READY_ATTEMPTS budget.
FRR_START_ATTEMPTS="${FRR_START_ATTEMPTS:-2}"

# The daemons frrinit.sh runs on this image, as an ERE for pgrep/pkill.
FRR_DAEMON_PATTERN='watchfrr|zebra|bgpd|staticd'

# Everything we know about why FRR would not come up, on the way out.
# Best-effort throughout: this runs on the failure path, and a missing
# log file must not replace the real error with a shell error.
dump_frr_diagnostics() {
    log "--- FRR diagnostics ---"
    /usr/lib/frr/frrinit.sh status 2>&1 | sed 's/^/[gwnode]   /' >&2 || true
    pgrep -af "${FRR_DAEMON_PATTERN}" 2>/dev/null \
        | sed 's/^/[gwnode]   /' >&2 || true
    tail -n 40 /var/log/frr/* 2>/dev/null | sed 's/^/[gwnode]   /' >&2 || true
}

# Remove the previous incarnation's FRR runtime state.
#
# `docker restart` keeps the container filesystem but not the processes, so
# whatever the killed daemons left in FRR's two state directories survives
# into the next boot — while that boot's PID numbering starts from 1 again.
#
# /var/tmp/frr is watchfrr's own state; stale entries there make it refuse to
# start. /var/run/frr is the one that cost a chaos run (issue #216): it holds
# the pid files, the vty sockets and the zserv socket, and frrcommon.sh's
# daemon_stop() trusts any pid file it can read — it `kill -0`s the number,
# SIGINTs it, then spins in `while kill -0 "$pid"` for up to 120 s waiting for
# it to die. The daemons the entrypoint starts before FRR (ovsdb-server,
# ovs-vswitchd, ovn-controller) land in exactly the PID range the previous
# boot's FRR daemons recorded, so that number is regularly live again — and
# then the stop never returns, having SIGINT'd an innocent daemon on the way.
#
# watchfrr walks straight into it. Its initial connect to zebra/bgpd/staticd
# fails because they are not up yet (normal, and logged as such), so it forks
# `watchfrr.sh restart all` -> all_stop -> daemon_stop, which hangs on the
# stale pid; watchfrr shoots the child after 20 s, has started no daemon, and
# exits "[EC 268435457] all configured daemons failed to start". Confirmed
# locally against this image by planting a live PID in /var/run/frr/bgpd.pid
# across a `docker restart`, which reproduces that log verbatim.
clean_frr_runtime_state() {
    rm -rf /var/tmp/frr/* 2>/dev/null || true
    rm -rf /var/run/frr/* 2>/dev/null || true
}

# Clear the field before a retry. Deliberately not `frrinit.sh stop`: that
# drives the very daemon_stop() a stale pid file hangs, so the cleanup would
# inherit the failure it exists to undo.
stop_frr_leftovers() {
    pkill -TERM -x "${FRR_DAEMON_PATTERN}" 2>/dev/null || true
    local i
    for (( i = 1; i <= 10; i++ )); do
        pgrep -x "${FRR_DAEMON_PATTERN}" >/dev/null 2>&1 || return 0
        sleep 1
    done
    log "leftover FRR daemons survived SIGTERM; sending SIGKILL"
    pkill -KILL -x "${FRR_DAEMON_PATTERN}" 2>/dev/null || true
    sleep 1
}

# A failed FRR start used to be terminal: await_ready exited, tini followed it
# down, and Docker's `restart: always` recreated the container. That is a ~85 s
# round trip which also discards everything an external actor had built against
# the dead incarnation — the chaos harness's underlay veth and VRF repairs, any
# netns responder re-created in the meantime — and none of it is visible in the
# log, because the new container's entrypoint simply continues it. So retry in
# place first, say so loudly, and keep PID-1 suicide as the last resort.
#
# The per-attempt budget stays FRR_READY_ATTEMPTS rather than being doubled
# into one long wait: a healthy FRR answers vtysh in a second or two, so a
# retry only ever spends time on a boot that was otherwise going to spend a
# whole container lifecycle.
start_frr() {
    local attempt
    for (( attempt = 1; attempt <= FRR_START_ATTEMPTS; attempt++ )); do
        if (( attempt > 1 )); then
            log "RETRY: FRR never came up; stopping leftovers and starting it again in place"
            stop_frr_leftovers
        fi
        log "starting FRR (attempt ${attempt}/${FRR_START_ATTEMPTS})"
        clean_frr_runtime_state
        # /usr/lib/frr/frrinit.sh is the canonical service entrypoint shipped
        # by the FRR Debian package; it launches the daemons listed in
        # /etc/frr/daemons.
        start_daemon "frrinit.sh start" /usr/lib/frr/frrinit.sh start
        if probe_ready "${FRR_READY_ATTEMPTS}" vtysh -c 'show version'; then
            return 0
        fi
        log "ERROR: FRR (vtysh) did not become ready within ${FRR_READY_ATTEMPTS}s" \
            "(attempt ${attempt}/${FRR_START_ATTEMPTS})"
        dump_frr_diagnostics
    done
    log "ERROR: FRR did not come up in ${FRR_START_ATTEMPTS} attempts; exiting"
    exit 1
}

# The daemons this vtysh invocation reached, one per line. `show daemons`
# prints them space-separated on a single line.
frr_connected_daemons() {
    vtysh -c 'show daemons' 2>/dev/null | tr ' ' '\n' | sed '/^$/d'
}

# Whether vtysh reached every daemon in FRR_CONFIG_DAEMONS.
#
# This is the gate in front of the running-config read, and the whole
# reason the read can be trusted. `show running-config` renders the config
# of the daemons vtysh connected to at that moment: zebra answers first,
# bgpd registers its vty socket a moment later — and a read that lands in
# between reports no BGP block no matter what bgpd is at that instant
# loading out of /etc/frr/frr.conf. Deciding "nothing configured, push the
# placeholder" on that answer is exactly the clobber frr_config exists to
# avoid, so the caller's retry loop waits the daemons out instead.
frr_config_daemons_ready() {
    local connected d
    connected="$(frr_connected_daemons)" || return 1
    for d in ${FRR_CONFIG_DAEMONS}; do
        grep -qxF -- "${d}" <<<"${connected}" || return 1
    done
    return 0
}

# The config the agent expects, on stdin for vtysh: the prefix-list it
# writes /32 entries into, and a vrf-provider BGP router with a
# placeholder upstream neighbour. The neighbour does not need to
# establish a session for the lab to come up — per issue #44 the upstream
# peer may stay idle.
#
# Both of those are seeds for a container that has neither — not
# assertions. This function runs on *every* container start, including the
# ones that come back up with the real config already loaded (issue #218):
# `write memory` in bootstrap.sh's configure_gateway_frr persists the real
# BGP session into /etc/frr/frr.conf, the container filesystem survives
# `docker restart`, so FRR reloads it long before the entrypoint gets
# here. The push is additive — it never issues `no router bgp` first — so
# landing it on top of the real config merges the two: the router-id is
# reset to 127.0.0.1 (dropping and re-forming every established session)
# and the dead 192.0.2.1 placeholder neighbour is re-added beside the real
# one. Same story when the chaos harness's rewireUnderlay
# (test/e2e/chaos/lab.go) reaches vtysh first after a gateway restart.
#
# So whatever the running config already carries wins, and only the
# genuinely missing pieces are emitted. Opening the placeholder with `no
# router bgp ... vrf ${VRF_NAME}` instead — self-cleaning, the way the two
# writers of the real config are — was the alternative and is worse: it
# would destroy the real config deterministically on every restart rather
# than merging into it.
#
# `$1` is the current running config, read by configure_frr once the
# daemons that own it are all up.
frr_config() {
    local running="$1"
    printf 'configure terminal\n'
    # An empty vrf node renders as nothing in the running config, so there
    # is no state to probe for here — and re-asserting it is a no-op.
    printf 'vrf %s\nexit-vrf\n' "${VRF_NAME}"
    if grep -q '^ip prefix-list ANNOUNCED-NETWORKS ' <<<"${running}"; then
        # The agent owns the entries at runtime and replaces this seed with
        # the real /32s, so re-seeding would put a permit-everything entry
        # back underneath them until the next reconcile removes it again.
        log "ANNOUNCED-NETWORKS already exists; leaving its entries to the agent"
    else
        printf 'ip prefix-list ANNOUNCED-NETWORKS seq 5 permit 0.0.0.0/0 ge 32 le 32\n'
    fi
    # The ASN is a wildcard on purpose: the question is whether *any* BGP
    # router already owns this VRF, and the two writers that install the
    # real one (configure_gateway_frr in bootstrap.sh, configureGatewayBGP
    # in test/e2e/chaos/lab.go) take their ASN from their own variables.
    if grep -qE "^router bgp [0-9]+ vrf ${VRF_NAME}$" <<<"${running}"; then
        log "BGP is already configured in vrf ${VRF_NAME}; not pushing the placeholder over it"
    else
        cat <<EOF
router bgp 65000 vrf ${VRF_NAME}
 bgp router-id 127.0.0.1
 no bgp default ipv4-unicast
 neighbor 192.0.2.1 remote-as 65001
 address-family ipv4 unicast
  redistribute static
  neighbor 192.0.2.1 activate
  neighbor 192.0.2.1 prefix-list ANNOUNCED-NETWORKS out
 exit-address-family
EOF
    fi
    printf 'end\n'
}

configure_frr() {
    log "ensuring the FRR config the agent expects (vrf ${VRF_NAME} + ANNOUNCED-NETWORKS)"
    # `show version` answering does not mean bgpd and staticd have both
    # finished registering with zebra, so both halves — the running-config
    # read that decides what is missing and the push that fills it in — are
    # retried rather than trusted on the first try. What gets pushed is
    # idempotent, so re-running it after a partial apply is safe. The
    # reason for the last failure is captured so it can be reported: vtysh's
    # output went to the container's stdout before, which the caller never
    # saw once `set -e` had already killed PID 1.
    local attempt out="" running
    for attempt in $(seq 1 "${FRR_CONFIG_ATTEMPTS}"); do
        if ! frr_config_daemons_ready; then
            out="vtysh has not reached all of [${FRR_CONFIG_DAEMONS}] yet; connected: $(frr_connected_daemons | tr '\n' ' ')"
        elif ! running="$(vtysh -c 'show running-config' 2>&1)"; then
            out="${running}"
        elif out="$(frr_config "${running}" | vtysh 2>&1)"; then
            return 0
        fi
        log "FRR config not applied yet (attempt ${attempt}/${FRR_CONFIG_ATTEMPTS}), retrying"
        sleep 1
    done
    log "ERROR: the FRR config was not applied after ${FRR_CONFIG_ATTEMPTS} attempts"
    log "last failure:"
    printf '%s\n' "${out}" | sed 's/^/[gwnode]   /' >&2
    dump_frr_diagnostics
    exit 1
}

# A chaos configuration profile (test/e2e/chaos, issue #177) rewrites the
# whole agent config file and drops this marker next to it. topology.clab.yml
# sets OVN_NETWORK_DRAIN_ON_SHUTDOWN on every gateway, and the agent's
# environment layer beats its config file — so without stepping the override
# aside, a profile could never turn the drain on. No marker, no change: every
# existing scenario (drain-hitless included) keeps the deploy-time switch.
yield_config_to_chaos_profile() {
    local marker=/etc/ovn-network-agent/chaos-profile
    [[ -f "${marker}" ]] || return 0
    log "chaos profile '$(cat "${marker}")' owns the config file: unsetting OVN_NETWORK_DRAIN_ON_SHUTDOWN"
    unset OVN_NETWORK_DRAIN_ON_SHUTDOWN
}

main() {
    start_ovs
    resolve_sb_remote
    configure_ovs
    start_ovn_controller
    setup_vrf
    setup_loopback
    start_frr
    configure_frr
    yield_config_to_chaos_profile

    log "exec ovn-network-agent"
    exec /usr/local/bin/ovn-network-agent \
        --config /etc/ovn-network-agent/config.yaml \
        "$@"
}

main "$@"

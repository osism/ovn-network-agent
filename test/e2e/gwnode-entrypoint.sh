#!/usr/bin/env bash
# Entrypoint for the containerlab gateway-node image.
#
# Starts the supporting daemons in a fixed order — OVS local DB, OVS
# vswitchd (userspace datapath), ovn-controller, FRR — then execs the
# ovn-network-agent in the foreground so the container's lifecycle is
# tied to the agent process.

set -euo pipefail

log() { printf '[gwnode] %s\n' "$*" >&2; }

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
# This script runs under `set -e` AND it is the container's PID 1 (main()
# `exec`s the agent at the end). So an unguarded command that returns non-zero
# does not merely abort a step: it kills PID 1, the container exits, and
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

# Poll until the daemon is actually usable. A genuine failure must still fail,
# so a timeout exits — but it names the daemon and, when given a `diag`
# function, dumps its state first, so the next occurrence is root-causable from
# the container log alone.
#
# `diag` may be empty for daemons whose failure the probe itself makes obvious.
await_ready() {
    local label="$1" attempts="$2" diag="$3"
    shift 3
    local i
    for (( i = 1; i <= attempts; i++ )); do
        if "$@" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
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
        ip="$(getent hosts "${host}" | awk '{print $1; exit}')"
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
    log "configuring Open_vSwitch external_ids for ovn-controller"
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

# Everything we know about why FRR would not come up, on the way out.
# Best-effort throughout: this runs on the failure path, and a missing
# log file must not replace the real error with a shell error.
dump_frr_diagnostics() {
    log "--- FRR diagnostics ---"
    /usr/lib/frr/frrinit.sh status 2>&1 | sed 's/^/[gwnode]   /' >&2 || true
    pgrep -af 'watchfrr|zebra|bgpd|staticd' 2>/dev/null \
        | sed 's/^/[gwnode]   /' >&2 || true
    tail -n 40 /var/log/frr/* 2>/dev/null | sed 's/^/[gwnode]   /' >&2 || true
}

start_frr() {
    log "starting FRR"
    # watchfrr keeps state under /var/tmp/frr; stale entries from a
    # previous crash-restart make it refuse to start. Clean up before
    # launching frrinit.sh.
    rm -rf /var/tmp/frr/* 2>/dev/null || true
    # /usr/lib/frr/frrinit.sh is the canonical service entrypoint shipped
    # by the FRR Debian package; it launches the daemons listed in
    # /etc/frr/daemons.
    start_daemon "frrinit.sh start" /usr/lib/frr/frrinit.sh start
    await_ready "FRR (vtysh)" "${FRR_READY_ATTEMPTS}" dump_frr_diagnostics \
        vtysh -c 'show version'
}

# The config the agent expects, on stdin for vtysh: the prefix-list it
# writes /32 entries into, and a vrf-provider BGP router with a
# placeholder upstream neighbour. The neighbour does not need to
# establish a session for the lab to come up — per issue #44 the upstream
# peer may stay idle.
frr_config() {
    cat <<EOF
configure terminal
ip prefix-list ANNOUNCED-NETWORKS seq 5 permit 0.0.0.0/0 ge 32 le 32
vrf ${VRF_NAME}
exit-vrf
router bgp 65000 vrf ${VRF_NAME}
 bgp router-id 127.0.0.1
 no bgp default ipv4-unicast
 neighbor 192.0.2.1 remote-as 65001
 address-family ipv4 unicast
  redistribute static
  neighbor 192.0.2.1 activate
  neighbor 192.0.2.1 prefix-list ANNOUNCED-NETWORKS out
 exit-address-family
end
EOF
}

configure_frr() {
    log "pushing minimal FRR config (vrf ${VRF_NAME} + ANNOUNCED-NETWORKS)"
    # `show version` answering does not mean bgpd and staticd have both
    # finished registering with zebra, so the push is retried rather than
    # trusted on the first try. The config is idempotent, so re-running it
    # after a partial apply is safe. vtysh's output is captured so the last
    # failure can be reported: it went to the container's stdout before,
    # which the caller never saw once `set -e` had already killed PID 1.
    local attempt out
    for attempt in $(seq 1 "${FRR_CONFIG_ATTEMPTS}"); do
        if out="$(frr_config | vtysh 2>&1)"; then
            return 0
        fi
        log "vtysh config push failed (attempt ${attempt}/${FRR_CONFIG_ATTEMPTS}), retrying"
        sleep 1
    done
    log "ERROR: vtysh never accepted the FRR config after ${FRR_CONFIG_ATTEMPTS} attempts"
    log "last vtysh output:"
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

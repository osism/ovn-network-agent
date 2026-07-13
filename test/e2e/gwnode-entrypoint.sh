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
    /usr/share/openvswitch/scripts/ovs-ctl --system-id="${CHASSIS_NAME}" start
    # Wait for ovs-vsctl to respond.
    for _ in $(seq 1 30); do
        if ovs-vsctl show >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "ovs-vsctl did not respond after start" >&2
    exit 1
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
    /usr/share/ovn/scripts/ovn-ctl start_controller
    for _ in $(seq 1 30); do
        if ovs-vsctl br-exists br-int 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    echo "ovn-controller did not create br-int" >&2
    exit 1
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

start_frr() {
    log "starting FRR"
    # watchfrr keeps state under /var/tmp/frr; stale entries from a
    # previous crash-restart make it refuse to start. Clean up before
    # launching frrinit.sh.
    rm -rf /var/tmp/frr/* 2>/dev/null || true
    # /usr/lib/frr/frrinit.sh is the canonical service entrypoint shipped
    # by the FRR Debian package; it launches the daemons listed in
    # /etc/frr/daemons.
    /usr/lib/frr/frrinit.sh start
    for _ in $(seq 1 30); do
        if vtysh -c 'show version' >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done
    if ! vtysh -c 'show version' >/dev/null 2>&1; then
        echo "FRR did not become ready" >&2
        exit 1
    fi
}

configure_frr() {
    # Push the minimal config the agent expects: the prefix-list it
    # writes /32 entries into and a vrf-provider BGP router with a
    # placeholder upstream neighbour. The neighbour does not need to
    # establish a session for the lab to come up — per issue #44 the
    # upstream peer may stay idle.
    log "pushing minimal FRR config (vrf ${VRF_NAME} + ANNOUNCED-NETWORKS)"
    vtysh <<EOF
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

main() {
    start_ovs
    resolve_sb_remote
    configure_ovs
    start_ovn_controller
    setup_vrf
    setup_loopback
    start_frr
    configure_frr

    log "exec ovn-network-agent"
    exec /usr/local/bin/ovn-network-agent \
        --config /etc/ovn-network-agent/config.yaml \
        "$@"
}

main "$@"

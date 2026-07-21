#!/usr/bin/env bash
# setup.sh — bootstrap an OVN/OVS/FRR/nftables stack for integration tests.
#
# Targets a single Ubuntu host (CI: ubuntu-latest GHA runner, local: a fresh
# Ubuntu 24.04 VM is recommended). Runs idempotently: re-running on an
# already-bootstrapped host should be a no-op.
#
# Side effects:
#   * apt-installs openvswitch-switch, ovn-host, ovn-central, frr, nftables
#   * starts ovsdb-server (NB+SB), ovn-northd, ovn-controller, ovs-vswitchd
#   * binds OVN NB to tcp:127.0.0.1:6641 and SB to tcp:127.0.0.1:6642
#   * creates br-ex and br-int with datapath_type=netdev
#   * configures FRR with a vrf-provider VRF and a dummy BGP peer
#
# Requires: root (sudo).

set -euo pipefail

log() { printf '[setup] %s\n' "$*" >&2; }

require_root() {
    if [[ "$(id -u)" -ne 0 ]]; then
        echo "setup.sh must run as root (use sudo)" >&2
        exit 1
    fi
}

apt_install() {
    log "apt update"
    DEBIAN_FRONTEND=noninteractive apt-get update -y
    # Ubuntu 24.04 base ships OVN 24.03, but the agent's NB NAT model uses
    # the `match`/`priority` columns added in OVN 24.09. Pull OVN from the
    # Ubuntu Cloud Archive flamingo pocket (OpenStack 2025.2 → OVN 25.09).
    log "enabling cloud-archive:flamingo for current OVN"
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        software-properties-common ubuntu-cloud-keyring
    add-apt-repository -y cloud-archive:flamingo
    DEBIAN_FRONTEND=noninteractive apt-get update -y
    log "apt install"
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        openvswitch-switch \
        ovn-host \
        ovn-central \
        frr \
        frr-pythontools \
        nftables \
        iproute2
}

start_ovs() {
    log "starting openvswitch-switch"
    systemctl enable --now openvswitch-switch
    # Wait for ovs-vsctl to respond.
    for _ in $(seq 1 30); do
        if ovs-vsctl show >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "openvswitch-switch did not start" >&2
    exit 1
}

start_ovn_central() {
    log "starting ovn-central (NB/SB ovsdb-server + ovn-northd)"
    systemctl enable --now ovn-central
    # Bind NB/SB to TCP for the agent.
    log "configuring NB tcp:6641 and SB tcp:6642 listeners"
    ovn-nbctl set-connection ptcp:6641:127.0.0.1
    ovn-sbctl set-connection ptcp:6642:127.0.0.1
    # Wait for both endpoints to accept connections.
    for _ in $(seq 1 30); do
        if ovn-nbctl --db=tcp:127.0.0.1:6641 show >/dev/null 2>&1 \
            && ovn-sbctl --db=tcp:127.0.0.1:6642 show >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "OVN NB/SB TCP listeners did not become ready" >&2
    exit 1
}

start_ovn_host() {
    log "configuring ovn-controller"
    local hostname
    hostname=$(hostname -s)
    # Pin both system-id (becomes Chassis.name) AND external_ids:hostname
    # (becomes Chassis.hostname). Without external_ids:hostname,
    # ovn-controller falls back to gethostname(2), which on some runners
    # returns the FQDN, breaking the agent's local-router detection
    # (it compares Chassis.hostname against the short form of its own
    # hostname).
    ovs-vsctl set Open_vSwitch . \
        external_ids:ovn-remote=tcp:127.0.0.1:6642 \
        external_ids:ovn-encap-type=geneve \
        external_ids:ovn-encap-ip=127.0.0.1 \
        external_ids:system-id="${hostname}" \
        external_ids:hostname="${hostname}"
    systemctl enable --now ovn-host
    for _ in $(seq 1 30); do
        if ovs-vsctl br-exists br-int 2>/dev/null; then
            return 0
        fi
        sleep 1
    done
    echo "ovn-controller did not create br-int" >&2
    exit 1
}

create_bridges() {
    log "creating br-ex (datapath_type=netdev)"
    ovs-vsctl --may-exist add-br br-ex -- set bridge br-ex datapath_type=netdev
    ip link set br-ex up
    # br-int is created by ovn-controller; ensure datapath_type is consistent
    # so the smoke stack does not require kernel datapath modules.
    if ovs-vsctl br-exists br-int 2>/dev/null; then
        ovs-vsctl set bridge br-int datapath_type=netdev
    fi
    # Bridge mapping so that ovn-controller knows br-ex is the provider bridge.
    ovs-vsctl set Open_vSwitch . external_ids:ovn-bridge-mappings=physnet1:br-ex
}

configure_frr() {
    log "configuring FRR (vrf-provider + dummy BGP peer)"
    # Enable bgpd.
    if ! grep -q '^bgpd=yes' /etc/frr/daemons; then
        sed -i 's/^bgpd=no/bgpd=yes/' /etc/frr/daemons
    fi
    systemctl enable --now frr
    # Wait for vtysh to respond.
    for _ in $(seq 1 30); do
        if vtysh -c 'show version' >/dev/null 2>&1; then
            break
        fi
        sleep 1
    done

    # The frr package's postinst starts frr.service at install time with the
    # stock daemons file (bgpd=no), and /etc/frr/daemons is only read on a
    # service (re)start — `enable --now` on an already-running service is a
    # no-op, leaving bgpd down and every `router bgp` line below rejected.
    # Restart to pick up the edit; skipped when a re-run finds bgpd already
    # registered with watchfrr.
    if ! vtysh -c 'show daemons' 2>/dev/null | grep -qw bgpd; then
        log "restarting frr to start bgpd"
        systemctl restart frr
    fi
    for _ in $(seq 1 30); do
        if vtysh -c 'show daemons' 2>/dev/null | grep -qw bgpd; then
            break
        fi
        sleep 1
    done
    if ! vtysh -c 'show daemons' 2>/dev/null | grep -qw bgpd; then
        echo "bgpd did not start (check /etc/frr/daemons and frr.service)" >&2
        exit 1
    fi

    # Create the provider VRF as a Linux device so FRR can attach to it.
    # On the GHA azure-flavoured kernel the vrf module lives in
    # linux-modules-extra-<uname>, which isn't installed by default; pull it
    # in (best-effort) before modprobe.
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        "linux-modules-extra-$(uname -r)" || true
    modprobe vrf
    if ! ip link show vrf-provider >/dev/null 2>&1; then
        ip link add vrf-provider type vrf table 100
        ip link set vrf-provider up
    fi

    # Push minimal FRR config: vrf-provider + ANNOUNCED-NETWORKS prefix-list +
    # a dummy BGP peer (192.0.2.1, no real session). Idempotent: re-running
    # the conf-t block overwrites prior state for these objects.
    # The redistribute-connected route-map is inert here (the dummy neighbor
    # never establishes) but kept for parity with the gateway configs the chaos
    # lab and gwnode entrypoint write — the port-forward VIP's announce path
    # (#223).
    vtysh <<'EOF'
configure terminal
ip prefix-list ANNOUNCED-NETWORKS seq 5 permit 0.0.0.0/0 ge 32 le 32
route-map ANNOUNCE-CONNECTED permit 10
 match ip address prefix-list ANNOUNCED-NETWORKS
exit
vrf vrf-provider
exit-vrf
router bgp 65000 vrf vrf-provider
 bgp router-id 127.0.0.1
 no bgp default ipv4-unicast
 neighbor 192.0.2.1 remote-as 65001
 neighbor 192.0.2.1 update-source lo
 address-family ipv4 unicast
  redistribute static
  redistribute connected route-map ANNOUNCE-CONNECTED
  neighbor 192.0.2.1 activate
  neighbor 192.0.2.1 prefix-list ANNOUNCED-NETWORKS out
 exit-address-family
end
write memory
EOF

    # vtysh applies config line by line and exits 0 even when a whole block
    # is rejected (e.g. its daemon is down), so verify the BGP instance
    # actually exists in the running config.
    if ! vtysh -c 'show running-config' | grep -q '^router bgp 65000 vrf vrf-provider'; then
        echo "router bgp config was not applied (is bgpd running?)" >&2
        exit 1
    fi
}

ensure_nftables() {
    log "ensuring nftables service is enabled"
    systemctl enable --now nftables || true
    # The agent creates its own table; we don't need to pre-populate anything.
}

create_loopback1() {
    # The agent's port-forward feature attaches managed VIP /32 addresses to a
    # loopback device inside the provider VRF. Production hosts provision
    # `loopback1` via systemd-networkd or similar; the agent never creates it.
    # The integration harness needs the same surface, so we materialise it as
    # a dummy device enslaved to vrf-provider.
    log "creating loopback1 dummy device in vrf-provider"
    if ! ip link show loopback1 >/dev/null 2>&1; then
        ip link add loopback1 type dummy
    fi
    ip link set loopback1 master vrf-provider
    ip link set loopback1 up
}

main() {
    require_root
    apt_install
    start_ovs
    start_ovn_central
    start_ovn_host
    create_bridges
    configure_frr
    create_loopback1
    ensure_nftables
    log "setup complete"
}

main "$@"

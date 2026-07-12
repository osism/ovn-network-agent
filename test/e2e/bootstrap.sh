#!/usr/bin/env bash
# Bring the containerlab E2E lab to a state where scenarios can probe
# real reachability through OVN. Idempotent — re-running is a no-op.
#
# The lab matches how the ovn-network-agent is deployed in production:
# OVN handles the logical L2/L3, the agent's veth leak hands OVN-egressed
# traffic off to a kernel VRF (vrf-provider), and FRR/BGP advertises FIPs
# to an upstream router. That means the underlay between each gateway
# and `upstream` is a routed /30 in vrf-provider — not the OVN logical
# subnet — so the leak path does not loop back on itself.
#
# What this script provisions, in order:
#   1. NB DB
#        * Logical switch  ls0       (192.168.10.0/24)             — tenant
#        * Logical switch  ls-public                                — external,
#          with a `localnet` port that bridges to `physnet1` (which the
#          gwnode entrypoint maps to `br-ex` via ovn-bridge-mappings).
#        * Logical router  lr0
#            - lr0-ls0      (192.168.10.1/24)  attaches lr0 to ls0
#            - lr0-public   (192.0.2.1/24)     attaches lr0 to ls-public
#        * Gateway_Chassis distribution on lr0-public:
#              gateway-1 priority 30 (active master)
#              gateway-2 priority 20
#              gateway-3 priority 10
#        * Default static route 0.0.0.0/0 → 192.0.2.254 (virtual nexthop).
#        * Static_MAC_Binding 192.0.2.254 → 02:00:00:00:fe:01 on
#          lr0-public so the LR pipeline can resolve the nexthop
#          without on-wire ARP (the agent's catch-all flow rewrites
#          the eth.dst to the kernel side before egress anyway).
#        * Two dnat_and_snat NATs:
#              192.0.2.10 → 192.168.10.10
#              192.0.2.11 → 192.168.10.11
#        * Workload LSP on ls0: 192.168.10.10 / 02:00:00:00:0a:0a
#          (hosted by gateway-3 — the priority-10 chassis). Keeping
#          the responder off the master chassis is what makes
#          single-chassis failover scenarios (#105) testable: stopping
#          the master never takes out the workload at the same time,
#          and the baseline already exercises cross-chassis geneve
#          (master gateway-1 ↔ workload host gateway-3).
#   2. Per-gateway underlay
#        * eth1 moves out of br-ex into vrf-provider; gateway-N gets
#          100.64.N.2/30 on eth1.
#   3. Upstream container
#        * eth1/2/3 = 100.64.{1,2,3}.1/30 (one /30 per gateway link),
#          eth4 = 10.0.0.1/24 (client side), IPv4 forwarding enabled.
#        * FRR with bgpd enabled and one eBGP neighbor per gateway,
#          redistributing connected so the gateways learn the
#          10.0.0.0/24 return path.
#   4. Each gateway FRR
#        * eBGP from vrf-provider against its specific upstream /30
#          endpoint (replaces the placeholder neighbor pushed by the
#          gwnode entrypoint). The agent installs FIP /32 static
#          routes in vrf-provider; FRR redistributes them so the
#          upstream learns where the FIPs live.
#   5. Client-1 container
#        * eth1 = 10.0.0.2/24, default route via 10.0.0.1.
#   6. Kernel-side workload on gateway-3
#        * veth pair `vm1-host`↔`vm1-eth0`, host side bound to br-int
#          with `external_ids:iface-id=ls0-vm1`, peer side moved into a
#          `vm1` netns with 192.168.10.10/24 + default route via
#          192.168.10.1.
#
# Usage:
#   ./test/e2e/bootstrap.sh                    # against the default `ovn-e2e` lab
#   LAB_NAME=other ./test/e2e/bootstrap.sh     # override the containerlab lab name
#
# When a readiness gate times out, the awaited daemon's own output and
# surrounding state are dumped to stderr (the job log) so the failure
# can be root-caused from the collected artifacts. The upstream bgpd
# start honours:
#   BGPD_WAIT_SECS=30      # readiness wait per start attempt, seconds

set -euo pipefail

LAB_NAME="${LAB_NAME:-ovn-e2e}"
OVN_CENTRAL="${OVN_CENTRAL:-clab-${LAB_NAME}-central}"
OVN_NBCTL="${OVN_NBCTL:-docker exec ${OVN_CENTRAL} ovn-nbctl}"

LS_NAME="${LS_NAME:-ls0}"
LS_PUBLIC="${LS_PUBLIC:-ls-public}"
LS_PUBLIC_LN="${LS_PUBLIC_LN:-${LS_PUBLIC}-ln}"
LS_PUBLIC_LR="${LS_PUBLIC_LR:-${LS_PUBLIC}-lr0}"
LR_NAME="${LR_NAME:-lr0}"
LR_TENANT_PORT="${LR_TENANT_PORT:-${LR_NAME}-ls0}"
LR_PUBLIC_PORT="${LR_PUBLIC_PORT:-${LR_NAME}-public}"
LR_TENANT_MAC="${LR_TENANT_MAC:-02:00:00:00:01:01}"
LR_PUBLIC_MAC="${LR_PUBLIC_MAC:-02:00:00:00:02:01}"
LR_TENANT_CIDR="${LR_TENANT_CIDR:-192.168.10.1/24}"
LR_PUBLIC_CIDR="${LR_PUBLIC_CIDR:-192.0.2.1/24}"
PHYSNET="${PHYSNET:-physnet1}"

UPSTREAM_NAME="${UPSTREAM_NAME:-upstream}"
UPSTREAM_NODE="clab-${LAB_NAME}-${UPSTREAM_NAME}"
UPSTREAM_CLIENT_CIDR="${UPSTREAM_CLIENT_CIDR:-10.0.0.1/24}"

# OVN's LR has a default static route via this virtual upstream IP.
# The IP itself is never plumbed onto a wire — it only exists so the
# LR pipeline has a next-hop to resolve. We install a Static_MAC_Binding
# below so the resolution is satisfied without needing on-wire ARP.
UPSTREAM_NEXTHOP_IP="${UPSTREAM_NEXTHOP_IP:-192.0.2.254}"
UPSTREAM_NEXTHOP_MAC="${UPSTREAM_NEXTHOP_MAC:-02:00:00:00:fe:01}"

# Per-link /30 underlay between each gateway and the upstream. Each link
# is its own broadcast domain (point-to-point in topology.clab.yml), so
# we hand out /30s instead of a shared /24. The BGP sessions ride these
# links: each gateway's vrf-provider peers with its specific upstream
# /30 endpoint.
#
# Format: "<gateway-node> <gateway-cidr> <upstream-iface> <upstream-cidr>"
UNDERLAY_LINKS=(
    "gateway-1 100.64.1.2/30 eth1 100.64.1.1/30"
    "gateway-2 100.64.2.2/30 eth2 100.64.2.1/30"
    "gateway-3 100.64.3.2/30 eth3 100.64.3.1/30"
)

BGP_ASN_GATEWAYS="${BGP_ASN_GATEWAYS:-65000}"
BGP_ASN_UPSTREAM="${BGP_ASN_UPSTREAM:-65001}"
BGP_ROUTER_ID_UPSTREAM="${BGP_ROUTER_ID_UPSTREAM:-100.64.0.1}"

# Upstream bgpd start budget. The daemon is started in-place (see
# configure_upstream_frr) and polled for readiness; BGPD_WAIT_SECS is
# the readiness wait per start attempt.
BGPD_WAIT_SECS="${BGPD_WAIT_SECS:-30}"

CLIENT_NAME="${CLIENT_NAME:-client-1}"
CLIENT_NODE="clab-${LAB_NAME}-${CLIENT_NAME}"
CLIENT_CIDR="${CLIENT_CIDR:-10.0.0.2/24}"
CLIENT_GW="${CLIENT_GW:-10.0.0.1}"

WORKLOAD_HOST="${WORKLOAD_HOST:-gateway-3}"
WORKLOAD_NETNS="${WORKLOAD_NETNS:-vm1}"
WORKLOAD_LSP="${WORKLOAD_LSP:-${LS_NAME}-vm1}"
WORKLOAD_MAC="${WORKLOAD_MAC:-02:00:00:00:0a:0a}"
WORKLOAD_IP="${WORKLOAD_IP:-192.168.10.10}"
WORKLOAD_CIDR="${WORKLOAD_CIDR:-${WORKLOAD_IP}/24}"
WORKLOAD_GW="${WORKLOAD_GW:-192.168.10.1}"
WORKLOAD_HOST_VETH="${WORKLOAD_HOST_VETH:-vm1-host}"
WORKLOAD_NS_VETH="${WORKLOAD_NS_VETH:-vm1-eth0}"

GATEWAYS=(
    "gateway-1 30"
    "gateway-2 20"
    "gateway-3 10"
)

FIPS=(
    "192.0.2.10 192.168.10.10"
    "192.0.2.11 192.168.10.11"
)

DEFAULT_ROUTE_CIDR="${DEFAULT_ROUTE_CIDR:-0.0.0.0/0}"

log() { printf '[bootstrap] %s\n' "$*" >&2; }

# Run an ovn-nbctl command via the configured transport.
nbctl() {
    # shellcheck disable=SC2086  # we want word-splitting on OVN_NBCTL
    ${OVN_NBCTL} "$@"
}

# Per the hint in issue #45: poll the NB connection before we start
# writing rows. The gateway containers may not have finished starting
# ovn-controller yet, but ovn-nbctl talks to the central container, so
# all we need to verify is that NB is reachable from the host.
wait_for_nb() {
    log "waiting for OVN NB at ${OVN_CENTRAL}"
    for _ in $(seq 1 60); do
        if nbctl --timeout=2 show >/dev/null 2>&1; then
            log "OVN NB reachable"
            return 0
        fi
        sleep 1
    done
    echo "OVN NB at ${OVN_CENTRAL} did not become reachable within 60s" >&2
    # One final probe with stderr passed through so the actual
    # connection error reaches the job log, plus the central container's
    # stdout (northd/ovsdb daemon output — see central-entrypoint.sh).
    nbctl --timeout=2 show 1>&2 || true
    docker logs --tail 50 "${OVN_CENTRAL}" >&2 || true
    exit 1
}

# Wait until every gateway has registered itself as a Chassis row in
# SB. Without this, the very first `lrp-set-gateway-chassis` calls bind
# the LR external port to whichever subset of chassis happens to have
# registered already, and HA priorities only take effect after all
# candidates exist. We poll SB directly (via `ovn-sbctl`) instead of
# trusting the OVS bridge state on each gateway, because br-int gets
# created by ovn-controller before SB registration finishes.
wait_for_chassis() {
    local expected="$#"
    log "waiting for SB chassis registration: $*"
    for _ in $(seq 1 60); do
        local registered=0
        local missing=""
        for name in "$@"; do
            if docker exec "${OVN_CENTRAL}" ovn-sbctl --timeout=2 \
                    --bare --columns=name find Chassis name="${name}" \
                    2>/dev/null | grep -qx "${name}"; then
                registered=$(( registered + 1 ))
            else
                missing="${missing} ${name}"
            fi
        done
        if (( registered == expected )); then
            log "all ${expected} chassis registered"
            return 0
        fi
        sleep 1
    done
    echo "SB chassis registration timed out; missing:${missing:-<unknown>}" >&2
    docker exec "${OVN_CENTRAL}" ovn-sbctl list Chassis >&2 || true
    # For each still-missing gateway, dump the daemon whose registration
    # we were waiting on (ovn-controller logs to a file, not stdout) plus
    # the container's entrypoint output, which catches a container that
    # died before ovn-controller even started.
    # shellcheck disable=SC2086  # ${missing} is a space-separated name list
    for name in ${missing}; do
        log "--- ${name}: ovn-controller.log ---"
        docker exec "clab-${LAB_NAME}-${name}" \
            tail -n 50 /var/log/ovn/ovn-controller.log >&2 || true
        log "--- ${name}: docker logs ---"
        docker logs --tail 30 "clab-${LAB_NAME}-${name}" >&2 || true
    done
    exit 1
}

ensure_tenant_switch() {
    log "ensuring tenant switch ${LS_NAME}"
    nbctl --may-exist ls-add "${LS_NAME}"
}

ensure_public_switch() {
    log "ensuring external switch ${LS_PUBLIC} with localnet ↦ ${PHYSNET}"
    nbctl --may-exist ls-add "${LS_PUBLIC}"
    # `localnet` is OVN's escape hatch from the logical network to a
    # physical bridge: ovn-controller binds it via the chassis's
    # `external_ids:ovn-bridge-mappings` (set in gwnode-entrypoint.sh
    # to `${PHYSNET}:br-ex`).
    nbctl --may-exist lsp-add "${LS_PUBLIC}" "${LS_PUBLIC_LN}"
    nbctl lsp-set-type      "${LS_PUBLIC_LN}" localnet
    nbctl lsp-set-addresses "${LS_PUBLIC_LN}" unknown
    nbctl lsp-set-options   "${LS_PUBLIC_LN}" network_name="${PHYSNET}"
}

ensure_router() {
    log "ensuring logical router ${LR_NAME}"
    nbctl --may-exist lr-add "${LR_NAME}"
}

ensure_router_ports() {
    log "ensuring ${LR_TENANT_PORT} on ${LR_NAME} (${LR_TENANT_CIDR})"
    nbctl --may-exist lrp-add "${LR_NAME}" "${LR_TENANT_PORT}" \
        "${LR_TENANT_MAC}" "${LR_TENANT_CIDR}"

    log "ensuring ${LS_NAME} ↔ ${LR_NAME} switch port"
    nbctl --may-exist lsp-add "${LS_NAME}" "${LS_NAME}-${LR_NAME}"
    nbctl lsp-set-type        "${LS_NAME}-${LR_NAME}" router
    nbctl lsp-set-addresses   "${LS_NAME}-${LR_NAME}" router
    nbctl lsp-set-options     "${LS_NAME}-${LR_NAME}" \
        router-port="${LR_TENANT_PORT}"

    log "ensuring ${LR_PUBLIC_PORT} on ${LR_NAME} (${LR_PUBLIC_CIDR})"
    nbctl --may-exist lrp-add "${LR_NAME}" "${LR_PUBLIC_PORT}" \
        "${LR_PUBLIC_MAC}" "${LR_PUBLIC_CIDR}"

    log "ensuring ${LS_PUBLIC} ↔ ${LR_NAME} switch port"
    nbctl --may-exist lsp-add "${LS_PUBLIC}" "${LS_PUBLIC_LR}"
    nbctl lsp-set-type      "${LS_PUBLIC_LR}" router
    nbctl lsp-set-addresses "${LS_PUBLIC_LR}" router
    nbctl lsp-set-options   "${LS_PUBLIC_LR}" \
        router-port="${LR_PUBLIC_PORT}"
}

ensure_gateway_chassis() {
    for entry in "${GATEWAYS[@]}"; do
        local chassis priority
        read -r chassis priority <<<"${entry}"
        log "binding ${LR_PUBLIC_PORT} to ${chassis} priority ${priority}"
        # lrp-set-gateway-chassis replaces the prior priority for the same
        # (port, chassis) tuple, which keeps the operation idempotent.
        nbctl lrp-set-gateway-chassis "${LR_PUBLIC_PORT}" \
            "${chassis}" "${priority}"
    done
}

ensure_default_route() {
    log "ensuring default route ${DEFAULT_ROUTE_CIDR} → ${UPSTREAM_NEXTHOP_IP}"
    # Reply traffic from the workload heading back to client-1 enters
    # the LR via lr0-ls0; without this route the LR would drop the
    # packet because 10.0.0.0/24 is not directly connected.
    nbctl --may-exist lr-route-add "${LR_NAME}" \
        "${DEFAULT_ROUTE_CIDR}" "${UPSTREAM_NEXTHOP_IP}"
}

ensure_fips() {
    for entry in "${FIPS[@]}"; do
        local external internal
        read -r external internal <<<"${entry}"
        log "ensuring FIP ${external} → ${internal}"
        nbctl --may-exist lr-nat-add "${LR_NAME}" \
            dnat_and_snat "${external}" "${internal}"
    done
}

ensure_workload_lsp() {
    log "ensuring workload LSP ${WORKLOAD_LSP} on ${LS_NAME}"
    nbctl --may-exist lsp-add "${LS_NAME}" "${WORKLOAD_LSP}"
    nbctl lsp-set-addresses "${WORKLOAD_LSP}" \
        "${WORKLOAD_MAC} ${WORKLOAD_IP}"
}

wire_gateway_underlay() {
    # Move each gateway's eth1 from br-ex into vrf-provider and give it a
    # /30 underlay address. This is the change that makes the agent's
    # veth-leak design line up with the lab:
    #
    #   * The agent installs a policy rule "from 192.0.2.0/24 lookup 200"
    #     and a leak-table route "default via 169.254.0.2 dev veth-default"
    #     that send OVN-egressed traffic into vrf-provider.
    #   * With eth1 in vrf-provider, vrf-provider has a real underlay
    #     interface and learns the return path (10.0.0.0/24 → upstream)
    #     via BGP, so the leaked traffic finally exits the chassis.
    #   * For the reverse direction (upstream → FIP), upstream BGP-routes
    #     the FIP /32 to the gateway's vrf-provider IP; the packet enters
    #     on eth1 (in vrf-provider), the agent's table-100 route
    #     "192.0.2.0/24 via 169.254.0.1 dev veth-provider" sends it back
    #     to the default netns, and the agent's per-FIP scope-link routes
    #     on br-ex deliver it to OVN via OVS NORMAL → patch port.
    for entry in "${UNDERLAY_LINKS[@]}"; do
        local gw gw_cidr _upstream_iface _upstream_cidr
        read -r gw gw_cidr _upstream_iface _upstream_cidr <<<"${entry}"
        local node="clab-${LAB_NAME}-${gw}"
        log "wiring ${gw}:eth1 into vrf-provider (${gw_cidr})"
        docker exec "${node}" sh -ec "
            ovs-vsctl --if-exists del-port br-ex eth1
            ip link set eth1 down
            ip link set eth1 master vrf-provider
            ip link set eth1 up
            ip addr replace ${gw_cidr} dev eth1
        "
    done
}

configure_upstream() {
    log "configuring ${UPSTREAM_NODE}: per-link /30s + ${UPSTREAM_CLIENT_CIDR} on eth4"
    # Build the list of `ip addr replace ...` calls for each gateway-facing
    # interface so the configuration matches the UNDERLAY_LINKS table exactly.
    local addr_cmds=""
    for entry in "${UNDERLAY_LINKS[@]}"; do
        local _gw _gw_cidr upstream_iface upstream_cidr
        read -r _gw _gw_cidr upstream_iface upstream_cidr <<<"${entry}"
        addr_cmds="${addr_cmds}
            ip link set ${upstream_iface} up
            ip addr replace ${upstream_cidr} dev ${upstream_iface}"
    done
    docker exec "${UPSTREAM_NODE}" sh -ec "
        sysctl -wq net.ipv4.ip_forward=1
        ${addr_cmds}
        ip link set eth4 up
        ip addr replace ${UPSTREAM_CLIENT_CIDR} dev eth4
    "
}

# Dump the upstream container's FRR daemon state to stderr (the job
# log) so a bgpd bring-up failure can be root-caused after the fact. The
# upstream image is unpinned (frrouting/frr:latest), so every command is
# individually best-effort and drawn from an independent source: a
# future image change degrades one section, not the whole report.
dump_upstream_frr_state() {
    log "dumping ${UPSTREAM_NODE} FRR state for triage"
    log "--- /etc/frr/daemons ---"
    docker exec "${UPSTREAM_NODE}" cat /etc/frr/daemons >&2 || true
    log "--- vtysh show daemons ---"
    docker exec "${UPSTREAM_NODE}" vtysh -c "show daemons" >&2 || true
    log "--- vtysh show version ---"
    docker exec "${UPSTREAM_NODE}" vtysh -c "show version" >&2 || true
    log "--- process list ---"
    docker exec "${UPSTREAM_NODE}" ps >&2 || true
    log "--- tail /var/log/frr/* ---"
    docker exec "${UPSTREAM_NODE}" sh -c 'tail -n 100 /var/log/frr/* 2>/dev/null' >&2 || true
    log "--- docker logs (watchfrr / PID 1) ---"
    docker logs --tail 100 "${UPSTREAM_NODE}" >&2 || true
}

configure_upstream_frr() {
    log "configuring ${UPSTREAM_NODE} FRR (BGP to each gateway)"
    # bgpd ships disabled (bgpd=no) in the frrouting/frr image. Restart
    # is NOT an option here: the container's PID 1 is the watchfrr that
    # supervises the FRR daemons, and `/usr/lib/frr/frrinit.sh restart`
    # tears down that whole process tree → the container exits with 137
    # before this script gets to the vtysh call. Start bgpd in-place
    # instead, and keep the daemons file in sync so a future restart
    # would honour it.
    #
    # BGPD_WAIT_SECS is passed in via `docker exec --env` (the
    # ensure_workload_netns pattern) so the script body stays
    # single-quoted; the start output is captured and surfaced to the
    # job log instead of discarded, so a failed start names its cause.
    if ! docker exec -i --env "BGPD_WAIT_SECS=${BGPD_WAIT_SECS}" \
            "${UPSTREAM_NODE}" sh -eu <<'EOSH'; then
if grep -q "^bgpd=no" /etc/frr/daemons 2>/dev/null; then
    sed -i "s/^bgpd=no/bgpd=yes/" /etc/frr/daemons
fi
start_out=""
if ! pgrep -x bgpd >/dev/null; then
    if start_out=$(/usr/lib/frr/bgpd -d -A 127.0.0.1 -u frr -g frr 2>&1); then
        :
    else
        rc=$?
        printf 'bgpd start exited %s: %s\n' "${rc}" "${start_out}" >&2
    fi
fi
for _ in $(seq 1 "${BGPD_WAIT_SECS}"); do
    if vtysh -c "show daemons" 2>/dev/null | grep -qw bgpd; then
        exit 0
    fi
    sleep 1
done
echo "bgpd did not come up on $(hostname) within ${BGPD_WAIT_SECS}s" >&2
if [ -n "${start_out}" ]; then
    printf 'last bgpd start output: %s\n' "${start_out}" >&2
fi
exit 1
EOSH
        dump_upstream_frr_state
        echo "bgpd did not come up on ${UPSTREAM_NODE}" >&2
        exit 1
    fi
    # Push BGP config in one vtysh transaction. Build the script with
    # literal newlines — earlier this used $(printf '…\n…') chained with
    # ${var}$(...) concatenation, but command substitution strips
    # trailing newlines and every line collapsed onto its neighbour,
    # producing "router bgp 65001 bgp router-id …" which vtysh silently
    # ignored. The result was an upstream FRR with no BGP instance and
    # gateway sessions permanently stuck in "Active".
    local neighbors_remote_as=""
    local neighbors_activate=""
    for entry in "${UNDERLAY_LINKS[@]}"; do
        local _gw gw_cidr _upstream_iface _upstream_cidr
        read -r _gw gw_cidr _upstream_iface _upstream_cidr <<<"${entry}"
        local gw_ip="${gw_cidr%/*}"
        neighbors_remote_as+=" neighbor ${gw_ip} remote-as ${BGP_ASN_GATEWAYS}"$'\n'
        neighbors_activate+="  neighbor ${gw_ip} activate"$'\n'
    done
    docker exec -i "${UPSTREAM_NODE}" vtysh <<EOF
configure terminal
router bgp ${BGP_ASN_UPSTREAM}
 bgp router-id ${BGP_ROUTER_ID_UPSTREAM}
 no bgp default ipv4-unicast
 no bgp ebgp-requires-policy
${neighbors_remote_as} address-family ipv4 unicast
  redistribute connected
${neighbors_activate} exit-address-family
end
write memory
EOF
}

configure_gateway_frr() {
    # The gwnode entrypoint pushes a placeholder BGP config with the
    # neighbour pinned to 192.0.2.1 (OVN's own LR port IP, which doesn't
    # speak BGP). That session can't establish; replace it now that the
    # underlay is up so each gateway peers with its specific upstream /30.
    for entry in "${UNDERLAY_LINKS[@]}"; do
        local gw _gw_cidr _upstream_iface upstream_cidr
        read -r gw _gw_cidr _upstream_iface upstream_cidr <<<"${entry}"
        local node="clab-${LAB_NAME}-${gw}"
        local upstream_ip="${upstream_cidr%/*}"
        log "configuring ${gw} FRR (BGP neighbor ${upstream_ip} in vrf-provider)"
        docker exec -i "${node}" vtysh <<EOF
configure terminal
no router bgp ${BGP_ASN_GATEWAYS} vrf vrf-provider
router bgp ${BGP_ASN_GATEWAYS} vrf vrf-provider
 bgp router-id ${upstream_ip%.*}.2
 no bgp default ipv4-unicast
 no bgp ebgp-requires-policy
 neighbor ${upstream_ip} remote-as ${BGP_ASN_UPSTREAM}
 address-family ipv4 unicast
  redistribute static
  neighbor ${upstream_ip} activate
  neighbor ${upstream_ip} prefix-list ANNOUNCED-NETWORKS out
 exit-address-family
end
write memory
EOF
    done
}

ensure_upstream_nexthop_mac_binding() {
    # OVN's LR has a default static route via ${UPSTREAM_NEXTHOP_IP}
    # (192.0.2.254). That IP doesn't live on any wire — the underlay is
    # made of /30s between vrf-provider and upstream — so OVN can't ARP
    # resolve it. A Static_MAC_Binding satisfies the LR pipeline's ARP
    # lookup so the egress packet leaves OVN and the agent's catch-all
    # flow on br-ex then redirects it into the kernel + vrf-provider
    # path.
    log "ensuring static MAC binding ${UPSTREAM_NEXTHOP_IP} → ${UPSTREAM_NEXTHOP_MAC} on ${LR_PUBLIC_PORT}"
    nbctl --may-exist static-mac-binding-add "${LR_PUBLIC_PORT}" \
        "${UPSTREAM_NEXTHOP_IP}" "${UPSTREAM_NEXTHOP_MAC}"
}

configure_client() {
    log "configuring ${CLIENT_NODE}: ${CLIENT_CIDR} on eth1, default via ${CLIENT_GW}"
    docker exec "${CLIENT_NODE}" sh -ec "
        ip link set eth1 up
        ip addr replace ${CLIENT_CIDR} dev eth1
        ip route replace default via ${CLIENT_GW}
    "
}

ensure_workload_netns() {
    local node="clab-${LAB_NAME}-${WORKLOAD_HOST}"
    log "ensuring workload netns ${WORKLOAD_NETNS} on ${WORKLOAD_HOST} (${WORKLOAD_CIDR})"
    # `docker exec` without `-i` closes stdin immediately, which makes
    # `sh -eu <<EOSH` see EOF and exit before reading any command. The
    # bug was silent because `-eu` never tripped: no command ever ran.
    docker exec -i \
        --env "WORKLOAD_NETNS=${WORKLOAD_NETNS}" \
        --env "WORKLOAD_LSP=${WORKLOAD_LSP}" \
        --env "WORKLOAD_MAC=${WORKLOAD_MAC}" \
        --env "WORKLOAD_CIDR=${WORKLOAD_CIDR}" \
        --env "WORKLOAD_GW=${WORKLOAD_GW}" \
        --env "WORKLOAD_HOST_VETH=${WORKLOAD_HOST_VETH}" \
        --env "WORKLOAD_NS_VETH=${WORKLOAD_NS_VETH}" \
        "${node}" sh -eu <<'EOSH'
# Host-side veth — owned by OVS, bound to the OVN logical port via
# external_ids:iface-id. Created idempotently; deletion is left to the
# `make e2e-down` teardown that wipes the entire container.
if ! ip link show "${WORKLOAD_HOST_VETH}" >/dev/null 2>&1; then
    ip link add "${WORKLOAD_HOST_VETH}" type veth peer name "${WORKLOAD_NS_VETH}"
fi
ovs-vsctl --may-exist add-port br-int "${WORKLOAD_HOST_VETH}" \
    -- set Interface "${WORKLOAD_HOST_VETH}" external_ids:iface-id="${WORKLOAD_LSP}"
ip link set "${WORKLOAD_HOST_VETH}" up

# Workload netns + peer-side veth.
if ! ip netns list | awk '{print $1}' | grep -qx "${WORKLOAD_NETNS}"; then
    ip netns add "${WORKLOAD_NETNS}"
fi
if ! ip -n "${WORKLOAD_NETNS}" link show "${WORKLOAD_NS_VETH}" >/dev/null 2>&1; then
    ip link set "${WORKLOAD_NS_VETH}" netns "${WORKLOAD_NETNS}"
fi
ip -n "${WORKLOAD_NETNS}" link set lo up
ip -n "${WORKLOAD_NETNS}" link set "${WORKLOAD_NS_VETH}" address "${WORKLOAD_MAC}"
ip -n "${WORKLOAD_NETNS}" link set "${WORKLOAD_NS_VETH}" up
ip -n "${WORKLOAD_NETNS}" addr replace "${WORKLOAD_CIDR}" dev "${WORKLOAD_NS_VETH}"
ip -n "${WORKLOAD_NETNS}" route replace default via "${WORKLOAD_GW}"
EOSH
}

main() {
    log "OVN_NBCTL = ${OVN_NBCTL}"
    wait_for_nb
    # Build the list of chassis names we expect to see in SB from the
    # GATEWAYS table so a topology change only needs editing one place.
    local chassis_names=()
    for entry in "${GATEWAYS[@]}"; do
        local name _priority
        read -r name _priority <<<"${entry}"
        chassis_names+=("${name}")
    done
    wait_for_chassis "${chassis_names[@]}"

    # OVN NB topology + workload binding.
    ensure_tenant_switch
    ensure_public_switch
    ensure_router
    ensure_router_ports
    ensure_gateway_chassis
    ensure_default_route
    ensure_fips
    ensure_workload_lsp
    ensure_upstream_nexthop_mac_binding

    # Underlay: each gateway's eth1 moves out of br-ex into vrf-provider
    # with a /30, upstream takes the matching /30s plus a /24 toward the
    # clients, client-1 picks up its address + default route.
    wire_gateway_underlay
    configure_upstream
    configure_client

    # FRR / BGP — must come after the underlay is up so the BGP TCP
    # sessions have somewhere to land.
    configure_upstream_frr
    configure_gateway_frr

    # Workload (kernel netns + LSP binding) last so ovn-controller sees
    # the SB Port_Binding row and the OVS port at the same time.
    ensure_workload_netns
    log "bootstrap complete"
}

main "$@"

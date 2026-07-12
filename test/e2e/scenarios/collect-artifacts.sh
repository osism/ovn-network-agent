#!/usr/bin/env bash
# Dump the state of the containerlab E2E lab into a directory tree so
# the CI workflow can upload it as an artifact and humans can triage a
# remote failure without re-running the lab. The shape mirrors the
# acceptance criteria in issue #45:
#
#   <out>/inspect/containerlab.txt          — `containerlab inspect -t <topology>`
#   <out>/docker/<node>.log                 — `docker logs` per container
#   <out>/ovs/<gateway>/<dump>.txt          — OVS bridge + flow dumps per gateway
#   <out>/ovn/{nb,sb}-<table>.txt           — `ovn-nbctl list`/`ovn-sbctl list` dumps
#   <out>/frr/<gateway>-running-config.txt  — FRR `show running-config`
#   <out>/frr/<gateway>-bgp-summary.txt     — `show bgp summary`
#   <out>/frr/upstream-*.txt                — upstream FRR daemon + BGP state
#   <out>/ovn-controller/<gateway>.log      — gateway ovn-controller log (chassis-registration daemon)
#   <out>/agent/<gateway>.log               — `docker logs` of the gateway (agent runs in foreground)
#
# Best-effort: every command is allowed to fail individually so a single
# missing tool does not prevent the rest of the bundle from being
# collected.

set -u

LAB="${LAB:-ovn-e2e}"
TOPOLOGY="${TOPOLOGY:-test/e2e/topology.clab.yml}"
# Default node lists. The previous `("${GATEWAYS[@]:-a b c}")` form
# collapsed the default into a single array element, so per-node
# captures ran `docker logs clab-ovn-e2e-gateway-1 gateway-2 gateway-3`
# once and only produced one bogus output file. Env-override is kept by
# only applying the default when the array is unset.
if [ -z "${GATEWAYS+x}" ]; then
    GATEWAYS=(gateway-1 gateway-2 gateway-3)
fi
if [ -z "${CLIENTS+x}" ]; then
    CLIENTS=(client-1 client-2)
fi
CENTRAL="${CENTRAL:-central}"
UPSTREAM="${UPSTREAM:-upstream}"
OUT_DIR="${1:-}"

if [ -z "${OUT_DIR}" ]; then
    echo "usage: $0 <output-directory>" >&2
    exit 2
fi

mkdir -p "${OUT_DIR}"

log() { printf '[artifacts] %s\n' "$*" >&2; }

# Run a command and write stdout+stderr to a file. Never aborts on
# failure — best-effort collection.
capture() {
    local dest="$1"
    shift
    mkdir -p "$(dirname "${dest}")"
    "$@" >"${dest}" 2>&1 || true
}

# `docker exec` in the lab's container namespace. Container names follow
# the `clab-<lab>-<node>` convention emitted by containerlab.
exec_in() {
    local node="$1"
    shift
    docker exec "clab-${LAB}-${node}" "$@"
}

collect_inspect() {
    log "containerlab inspect"
    capture "${OUT_DIR}/inspect/containerlab.txt" \
        containerlab inspect -t "${TOPOLOGY}"
}

collect_docker_logs() {
    log "docker logs for every lab container"
    local nodes=("${CENTRAL}" "${UPSTREAM}" "${GATEWAYS[@]}" "${CLIENTS[@]}")
    for node in "${nodes[@]}"; do
        capture "${OUT_DIR}/docker/${node}.log" \
            docker logs "clab-${LAB}-${node}"
    done
}

collect_ovs() {
    log "OVS dumps from each gateway"
    for gw in "${GATEWAYS[@]}"; do
        capture "${OUT_DIR}/ovs/${gw}/show.txt" \
            exec_in "${gw}" ovs-vsctl show
        capture "${OUT_DIR}/ovs/${gw}/br-int-flows.txt" \
            exec_in "${gw}" ovs-ofctl --no-stats dump-flows br-int
        capture "${OUT_DIR}/ovs/${gw}/br-ex-flows.txt" \
            exec_in "${gw}" ovs-ofctl --no-stats dump-flows br-ex
        capture "${OUT_DIR}/ovs/${gw}/ports.txt" \
            exec_in "${gw}" ovs-vsctl --columns=name,type,external_ids list Interface
    done
}

collect_ovn() {
    log "OVN NB/SB dumps from central"
    capture "${OUT_DIR}/ovn/nb-show.txt"  exec_in "${CENTRAL}" ovn-nbctl show
    capture "${OUT_DIR}/ovn/sb-show.txt"  exec_in "${CENTRAL}" ovn-sbctl show
    # `list` gives full column dumps including FIPs, gateway chassis bindings,
    # and the SB Port_Binding rows that show which chassis a router port is
    # currently anchored to.
    local nb_tables=(Logical_Switch Logical_Switch_Port Logical_Router \
                     Logical_Router_Port NAT Gateway_Chassis Load_Balancer)
    for t in "${nb_tables[@]}"; do
        capture "${OUT_DIR}/ovn/nb-${t}.txt" exec_in "${CENTRAL}" ovn-nbctl list "${t}"
    done
    local sb_tables=(Chassis Port_Binding Datapath_Binding MAC_Binding)
    for t in "${sb_tables[@]}"; do
        capture "${OUT_DIR}/ovn/sb-${t}.txt" exec_in "${CENTRAL}" ovn-sbctl list "${t}"
    done
}

collect_frr() {
    log "FRR running-config and BGP/routing state from each gateway"
    for gw in "${GATEWAYS[@]}"; do
        capture "${OUT_DIR}/frr/${gw}-running-config.txt" \
            exec_in "${gw}" vtysh -c "show running-config"
        capture "${OUT_DIR}/frr/${gw}-bgp-summary.txt" \
            exec_in "${gw}" vtysh -c "show bgp summary"
        capture "${OUT_DIR}/frr/${gw}-bgp-ipv4.txt" \
            exec_in "${gw}" vtysh -c "show bgp ipv4 unicast"
        capture "${OUT_DIR}/frr/${gw}-ip-route-vrf.txt" \
            exec_in "${gw}" vtysh -c "show ip route vrf all"
    done
    capture "${OUT_DIR}/frr/upstream-running-config.txt" \
        exec_in "${UPSTREAM}" vtysh -c "show running-config"
    # The upstream bgpd start is the one daemon bring-up performs itself
    # (see bootstrap.sh:configure_upstream_frr); capture the same state
    # its failure path dumps into the job log so a bring-up flake can be
    # root-caused from the artifact bundle alone.
    capture "${OUT_DIR}/frr/upstream-show-daemons.txt" \
        exec_in "${UPSTREAM}" vtysh -c "show daemons"
    capture "${OUT_DIR}/frr/upstream-bgp-summary.txt" \
        exec_in "${UPSTREAM}" vtysh -c "show bgp summary"
    capture "${OUT_DIR}/frr/upstream-daemons-file.txt" \
        exec_in "${UPSTREAM}" cat /etc/frr/daemons
    capture "${OUT_DIR}/frr/upstream-processes.txt" \
        exec_in "${UPSTREAM}" ps
    capture "${OUT_DIR}/frr/upstream-frr-log.txt" \
        exec_in "${UPSTREAM}" sh -c 'tail -n 200 /var/log/frr/* 2>/dev/null'
}

collect_kernel() {
    log "kernel routing / link state from each gateway"
    for gw in "${GATEWAYS[@]}"; do
        capture "${OUT_DIR}/kernel/${gw}-ip-addr.txt"      exec_in "${gw}" ip addr
        capture "${OUT_DIR}/kernel/${gw}-ip-route.txt"     exec_in "${gw}" ip route show table all
        capture "${OUT_DIR}/kernel/${gw}-ip-rule.txt"      exec_in "${gw}" ip rule show
        capture "${OUT_DIR}/kernel/${gw}-nft-ruleset.txt"  exec_in "${gw}" nft list ruleset
    done
}

# Agent logs are part of the gateway containers' stdout (the entrypoint
# `exec`s the agent in the foreground). `docker logs` already captures
# them, but keep an explicit copy under <out>/agent/ so reviewers do not
# have to know that convention.
collect_agent_logs() {
    log "agent logs (re-exposed from gateway docker logs)"
    for gw in "${GATEWAYS[@]}"; do
        capture "${OUT_DIR}/agent/${gw}.log" \
            docker logs "clab-${LAB}-${gw}"
    done
}

# The SB chassis-registration gate waits for each gateway's
# ovn-controller to register. ovn-controller logs to a file rather than
# the container's stdout (which the entrypoint reserves for the agent),
# so `docker logs` does not carry it — pull the log file explicitly.
collect_ovn_controller_logs() {
    log "ovn-controller logs from each gateway"
    for gw in "${GATEWAYS[@]}"; do
        capture "${OUT_DIR}/ovn-controller/${gw}.log" \
            exec_in "${gw}" cat /var/log/ovn/ovn-controller.log
    done
}

main() {
    log "writing artifacts to ${OUT_DIR}"
    collect_inspect
    collect_docker_logs
    collect_ovs
    collect_ovn
    collect_frr
    collect_kernel
    collect_agent_logs
    collect_ovn_controller_logs
    log "done"
}

main "$@"

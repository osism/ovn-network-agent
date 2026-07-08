---
outline: [2, 3]
---

# Containerlab E2E harness

The containerlab-based end-to-end test environment lives under
[`test/e2e/`](https://github.com/osism/ovn-network-agent/tree/main/test/e2e)
and is the foundation laid out by issue
[#44](https://github.com/osism/ovn-network-agent/issues/44) (parent
issue [#39](https://github.com/osism/ovn-network-agent/issues/39)).

The harness has two layers:

1. **Infrastructure** — the image build files, the containerlab
   topology, and a bootstrap script that seeds the OVN NB DB.
   Delivered by [#44](https://github.com/osism/ovn-network-agent/issues/44).
2. **Scenarios** — bash probes under `test/e2e/scenarios/` that drive
   the running lab. Each scenario has its own `make` target and, for
   most of them, a CI job. See [Running locally](#running-locally) for
   the full set and what each one asserts.

## Layout

```
test/e2e/
  Dockerfile.gwnode         — Ubuntu 24.04 + OVS + ovn-controller + FRR + agent
  Dockerfile.central        — ovn-northd + NB/SB ovsdb-server
  gwnode-entrypoint.sh      — starts OVS / ovn-controller / FRR, then execs the agent
  gwnode-config.yaml        — default agent config baked into the gwnode image
  central-entrypoint.sh     — starts ovn-northd + ovsdb-server, exposes 6641/6642
  topology.clab.yml         — containerlab topology (central + 3 gateways + upstream + 2 clients)
  bootstrap.sh              — idempotent OVN NB seed (1 LS, 1 LR with 2 FIPs, HA across 3 chassis)
  scenarios/
    baseline.sh             — baseline reachability scenario (issue #45)
    failover.sh             — HA failover scenario, master chassis loss (issue #105)
    hairpin.sh              — same-chassis hairpin scenario, two FIPs on master (issue #108)
    multi-vlan.sh           — multi-VLAN provider networks, two segments on master (issue #147)
    pf-external.sh          — port-forward / DNAT scenario, source IP preserved (issue #109)
    pf-hairpin.sh           — port-forward hairpin masquerade scenario (issue #110)
    stale-chassis.sh        — stale chassis cleanup scenario, hard kill (issue #111)
    collect-artifacts.sh    — dump lab state for offline triage
  pf-backend/
    main.go                 — tiny HTTP responder shipped at /usr/local/bin/pf-backend
                              in the gwnode image; logs each connection's source IP
                              for the port-forward scenario's assertion.
```

## Prerequisites

- A Linux host with Docker (privileged mode is required for the
  containerlab containers).
- [containerlab](https://containerlab.dev/install/) ≥ 0.55.
- `make`, `docker buildx`, and a Go toolchain matching `go.mod` — the
  gwnode image builds the agent from source.

`make e2e-install-tools` bootstraps containerlab on Linux via the
upstream installer (`get.containerlab.dev`). It is a no-op when
`containerlab` is already on `PATH`.

::: warning macOS is not a supported containerlab host
Upstream ships no darwin binary for containerlab. The supported macOS
workflow is to run the lab from a Linux VM (OrbStack, Colima, Docker
Desktop's Linux VM, …); see the
[upstream guide](https://containerlab.dev/macos/). `make e2e-install-tools`
exits with an explanatory error on macOS instead of pretending to
install something.
:::

The lab also runs on a GitHub Actions `ubuntu-latest` runner without
extra setup: Docker is preinstalled and supports privileged
containers, which is what containerlab requires.

## Topology

```
            +--------+
            | client |
            |   -1   |
            +---+----+
                |
+----------+    |    +----------+
|          |----+----|          |
|  client  |         | upstream |--- BGP ---+
|   -2     |---------|  (FRR)   |           |
+----------+         +----+-----+           |
                          |                 |
       +------------------+------------------+
       |                  |                  |
+------+------+    +------+------+    +------+------+
|  gateway-1  |    |  gateway-2  |    |  gateway-3  |
|  (gwnode)   |    |  (gwnode)   |    |  (gwnode)   |
+------+------+    +------+------+    +------+------+
       |                  |                  |
       +-------- mgmt net (containerlab) ----+
                          |
                     +----+----+
                     | central |
                     +---------+
```

- The **management** network is the default containerlab bridge;
  every node has an `eth0` on it. The gateway agents reach
  `central:6642` (OVN SB) and `central:6641` (OVN NB) here.
- The **underlay** is a set of point-to-point links:
  `gateway-N:eth1 ↔ upstream:eth{1,2,3}`. BGP between the gateways
  and the upstream router runs over these links. No real BGP session
  is required for the lab to come up — FRR may stay idle.
- The two **clients** sit behind the upstream router
  (`upstream:eth4` and `upstream:eth5`) and are used as reachability
  probes for scenario-level tests.

## Bootstrap state

`bootstrap.sh` is idempotent — re-running it is a no-op. It first
waits for OVN NB to become reachable from the host, then provisions
the lab in three layers, described below.

### NB DB

- tenant logical switch `ls0` (`192.168.10.0/24`),
- external logical switch `ls-public` with a `localnet` port that
  bridges to `physnet1` (which the gwnode entrypoint maps to `br-ex`
  via `ovn-bridge-mappings`),
- logical router `lr0` with two ports:
  `lr0-ls0` (`192.168.10.1/24`) attached to `ls0` and
  `lr0-public` (`192.0.2.1/24`) attached to `ls-public`,
- a Gateway_Chassis distribution on `lr0-public`:
  `gateway-1` priority 30, `gateway-2` priority 20,
  `gateway-3` priority 10,
- a default static route `0.0.0.0/0 → 192.0.2.254` (virtual
  nexthop — see the Static_MAC_Binding entry below),
- a `Static_MAC_Binding` for `192.0.2.254 → 02:00:00:00:fe:01` on
  `lr0-public` so the LR pipeline resolves the default nexthop
  without on-wire ARP. The agent's catch-all flow on `br-ex`
  rewrites `eth.dst` to the kernel side before the packet leaves
  OVN anyway, so the MAC itself is only needed to satisfy the LR
  pipeline.
- two `dnat_and_snat` NATs (FIPs):
  `192.0.2.10 ↔ 192.168.10.10` and
  `192.0.2.11 ↔ 192.168.10.11`,
- a workload LSP `ls0-vm1` on `ls0` with address
  `02:00:00:00:0a:0a 192.168.10.10`.

### Underlay (per gateway)

- `eth1` is **moved out of `br-ex` into `vrf-provider`** and gets a
  routed `/30` underlay address (`gateway-N:eth1 = 100.64.N.2/30`).
  This is the change that lines the lab up with how the agent ships
  in production: the agent's policy rule
  `from 192.0.2.0/24 lookup 200` and leak-table default route point
  into `vrf-provider`, so `vrf-provider` needs a real underlay
  interface — not a port on `br-ex` that would loop back through
  the same policy rule.

### Outside OVN

- `upstream`: per-link `/30`s towards each gateway
  (`eth1 = 100.64.1.1/30`, `eth2 = 100.64.2.1/30`,
  `eth3 = 100.64.3.1/30`), `10.0.0.1/24` on `eth4` (towards
  `client-1`), IPv4 forwarding enabled, FRR with `bgpd` enabled and
  one eBGP neighbor per gateway.
- each gateway's FRR (in `vrf-provider`): eBGP against its specific
  upstream `/30` endpoint, redistributing the FIP `/32` static
  routes that the agent installs in `vrf-provider`. The placeholder
  neighbor pushed by `gwnode-entrypoint.sh` (`192.0.2.1`) is
  replaced by `bootstrap.sh` once the underlay is up.
- `client-1`: `10.0.0.2/24` on `eth1`, default route via `10.0.0.1`.
- `gateway-3` (the priority-10 chassis): a veth pair
  `vm1-host` ↔ `vm1-eth0` — the host side is bound to `br-int` with
  `external_ids:iface-id=ls0-vm1`, the peer side lives inside a `vm1`
  network namespace configured with `192.168.10.10/24` and a default
  route via `192.168.10.1`. This is the responder behind the FIP.
  The workload sits on `gateway-3` (not the master) so failover
  scenarios that stop the master chassis do not also take out the
  responder — see issue
  [#105](https://github.com/osism/ovn-network-agent/issues/105). As a
  side effect, the baseline exercises cross-chassis geneve
  (master `gateway-1` ↔ workload host `gateway-3`).

## Running locally

All commands run from the repository root. `make e2e-up` builds the
images, runs `containerlab deploy`, and seeds the lab via
`bootstrap.sh`; `make e2e-down` destroys it again:

```sh
make e2e-up    # build images + containerlab deploy + bootstrap
make e2e-down  # containerlab destroy
```

Between bring-up and teardown, each scenario is its own `make` target:

| Scenario | `make` target | What it asserts | Issue |
| --- | --- | --- | --- |
| [Baseline](#baseline) | `e2e-baseline` | An external client reaches a FIP once the agent reconciles. | [#45](https://github.com/osism/ovn-network-agent/issues/45) |
| [Failover](#failover) | `e2e-failover` | `cr-lr0-public` re-elects to a surviving chassis after the master is lost. | [#105](https://github.com/osism/ovn-network-agent/issues/105) |
| [Hairpin](#hairpin) | `e2e-hairpin` | The `cookie=0x998` hairpin flow reflects FIP-to-FIP traffic on `br-ex`. | [#108](https://github.com/osism/ovn-network-agent/issues/108) |
| [Multi-VLAN](#multi-vlan) | `e2e-multi-vlan` | Two VLAN provider networks on one node get per-segment kernel interfaces, flows, and BGP-announced FIPs. | [#147](https://github.com/osism/ovn-network-agent/issues/147) |
| [Port-forward (external client)](#port-forward-external-client) | `e2e-pf-external` | Inbound DNAT preserves the external client's source IP. | [#109](https://github.com/osism/ovn-network-agent/issues/109) |
| [Port-forward hairpin masquerade](#port-forward-hairpin-masquerade) | `e2e-pf-hairpin` | The `hairpin_masquerade` flag is load-bearing for a co-located FIP. | [#110](https://github.com/osism/ovn-network-agent/issues/110) |
| [Stale chassis](#stale-chassis) | `e2e-stale-chassis` | Surviving peers garbage-collect the NB rows of a hard-killed chassis. | [#111](https://github.com/osism/ovn-network-agent/issues/111) |

Every scenario except `baseline` runs the baseline first as a sanity
gate; set `SANITY_GATE=0` to skip it. The subsections below describe
what each scenario does and which environment variables it accepts for
triage.

### Baseline

```sh
make e2e-baseline
```

[`baseline.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/baseline.sh)
pings the FIP `192.0.2.10` from `clab-ovn-e2e-client-1` and waits up to
`RECONCILE_TIMEOUT` (default 60 s) for the agent's reconcile loop to
install the routes. The scenario's exit code mirrors `ping`'s — any
packet loss fails the run. The window was raised from the 30 s
originally measured on a warm local Docker host because cold CI runners
regularly need longer for the full data path (OVN BFD/geneve tunnels,
the `cr-lr0-public` HA election, the agent's FRR FIP routes and BGP
propagation) to converge — a too-tight window made this gate flaky in
every scenario that runs it as a sanity pre-check.

**Overrides for triage:** `FIP`, `RECONCILE_TIMEOUT`, `PING_COUNT`,
`PING_TIMEOUT`.

### Failover

```sh
make e2e-failover
```

[`failover.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/failover.sh)
exercises OVN HA re-election after the master chassis is lost. It:

1. Runs the baseline first as a sanity gate.
2. Simulates chassis loss by stopping `ovn-controller` on `gateway-1`
   via `ovn-ctl stop_controller`. The clean SIGTERM makes
   ovn-controller release its claim on `cr-lr0-public`, and OVN
   re-elects to the priority-20 chassis.
3. Polls reachability through the FIP until the new master answers,
   with `FAILOVER_TIMEOUT` (default 30 s) as the deadline.
4. Asserts that `cr-lr0-public` actually migrated away from `MASTER` —
   guarding against a false pass where OVS keeps executing stale
   flows — then runs a 5-packet final probe that must return 100 %
   success.

An EXIT trap then restarts `ovn-controller` via
`ovn-ctl start_controller` and waits up to `FAILBACK_TIMEOUT`
(default 60 s) for `cr-lr0-public` to bind back **and** for
`client-1 → FIP` reachability to return, leaving the lab at baseline
state.

::: details Why stop only ovn-controller, not the whole container
Containerlab wires the per-gateway underlay
(`gateway-N:eth1 ↔ upstream:ethN`) as veth pairs at deploy time and
does not re-establish them on container restart. `docker stop` /
`docker start` would leave the master with no underlay and no BGP
session, so the lab could not be returned to baseline. The OVN HA
mechanism under test — re-election of `cr-lr0-public` after the
priority-30 chassis's claim goes away — is the same either way.
:::

**Overrides for triage:** `MASTER`, `FIP`, `FAILOVER_TIMEOUT`,
`FAILBACK_TIMEOUT`, `SANITY_GATE`.

### Hairpin

```sh
make e2e-hairpin
```

[`hairpin.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/hairpin.sh)
exercises the agent's same-chassis hairpin OpenFlow rule
(`cookie=0x998`, `actions=output:in_port`) on `br-ex`. The baseline lab
seeds a single FIP-with-backend on `lr0` (`192.0.2.10` →
`192.168.10.10` on `vm1`, hosted on `gateway-3`), but the hairpin path
can only fire once a *second* FIP backend co-located on the same active
master exists.

The scenario:

1. Runs the baseline first as a sanity gate.
2. Adds — scenario-locally — a second FIP `192.0.2.12` on `lr0` with a
   backing LSP `ls0-vm2` (`192.168.10.12`, MAC `02:00:00:00:0a:0b`) and
   a `vm2` netns + veth on `gateway-3`, so the new FIP has a real
   responder.
3. Polls `gateway-1` for the new hairpin flow on `br-ex` (default
   `RECONCILE_TIMEOUT=60s`) and asserts that **both** FIPs carry a
   `cookie=0x998` flow with `actions=output:in_port` — matching the
   issue's acceptance criterion of "at least one matching rule per FIP
   on the chassis".
4. Runs the probe: `ping -c 5 -W 2 192.0.2.12` from inside the existing
   `vm1` netns on `gateway-3`. The probe's exit code is the scenario's
   exit code — any packet loss fails the run.

The EXIT trap removes the LSP, NAT, host-side veth and netns added for
the second FIP, returning the lab to baseline so a subsequent
`make e2e-baseline` keeps passing.

::: details Expected packet flow on a green run
The probe runs from inside `vm1` (the existing FIP_A workload) and
targets FIP_B's external IP:

`vm1` (`192.168.10.10` on `gateway-3`) → geneve → `br-int` on
`gateway-1` → `lr0` pipeline (egress SNAT to `192.0.2.10`, route to the
connected `192.0.2.0/24` via `lr0-public`) → `cr-lr0-public` on
`gateway-1` → `ls-public` localnet → `br-ex`.

There the agent's `cookie=0x998` flow on `gateway-1` matches
`ip_dst=192.0.2.12` and reflects the packet back via `output:in_port`
into OVN, where the LR pipeline now ingresses on the external port,
applies DNAT (`192.0.2.12` → `192.168.10.12`), and forwards through
`lr0-ls0` to `vm2` on `gateway-3`.

Without the hairpin flow, OVS drops `output:in_port` by default and the
second-hop DNAT never fires — which is what makes a regression in
`ReconcileOVSHairpinFlows` visible end-to-end.
:::

::: details Why a scenario-local second FIP, not a third FIP in bootstrap.sh
The scenario uses **option (2)** from issue #108 (scenario-local
addition of the second FIP) rather than option (1), a third FIP baked
into `bootstrap.sh`. Keeping the baseline minimal means failover and
stale-chassis still observe the same lab the original issues specified,
and the hairpin scenario stays self-contained — its teardown leaves
nothing behind for other scenarios to trip over.
:::

**Overrides for triage:** `FIP_B`, `FIP_B_INTERNAL`, `MASTER`,
`WORKLOAD_HOST`, `RECONCILE_TIMEOUT`, `SANITY_GATE`.

### Multi-VLAN

```sh
make e2e-multi-vlan
```

[`multi-vlan.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/multi-vlan.sh)
exercises the multi-network data path from issue #147: one gateway node
serving FIPs from **two VLAN provider networks** on the same physnet,
alongside the flat baseline network.

The scenario:

1. Runs the baseline first as a sanity gate.
2. Adds — scenario-locally — two VLAN provider networks on `physnet1`
   (localnet ports with `tag=101` / `tag=102`, public subnets
   `198.51.100.0/24` and `203.0.113.0/24`), each with its own router
   pinned to `gateway-1`, a tenant switch, a netns workload on
   `gateway-3`, and one FIP (`198.51.100.10`, `203.0.113.10`).
3. Waits for the agent on `gateway-1` to create the per-segment kernel
   subinterfaces `br-ex.101` / `br-ex.102`, land each FIP's `/32` route
   on its own subinterface, install a `cookie=0x999` MAC-tweak flow on
   each network's localnet patch port, and announce both FIPs as `/32`
   via BGP (checked on the upstream router).
4. Probes both VLAN FIPs from `client-1` and finally re-probes the flat
   baseline FIP `192.0.2.10`, proving the flat and VLAN data paths
   coexist on the same node.

No underlay change is required: the fabric routes the `/32`s via BGP
into `vrf-provider`, so the VLAN frames never leave the node. The EXIT
trap removes the routers, switches, and workloads; the agent itself
prunes the subinterfaces and per-segment flows on its next reconcile.
The CI workflow runs this scenario as its own `multi-vlan` job gating
on `baseline`.

**Overrides for triage:** `MASTER`, `MASTER_UNDERLAY_IP`,
`WORKLOAD_HOST`, `BASELINE_FIP`, `RECONCILE_TIMEOUT`, `PING_COUNT`,
`PING_TIMEOUT`, `SANITY_GATE`.

### Port-forward (external client)

```sh
make e2e-pf-external
```

[`pf-external.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/pf-external.sh)
exercises OVN's port-forward / DNAT data path with traffic from an
external client and asserts the backend observes the client's original
source IP end-to-end — the "no SNAT on the way in" property OpenStack
tenants rely on for source-IP-based access control. The conceptual
model is documented in
[Port forwarding (DNAT)](../explanation/port-forwarding.md).

The scenario:

1. Runs the baseline first as a sanity gate.
2. Adds — scenario-locally — an OVN `Load_Balancer` row (`pf-external`)
   mapping `192.0.2.50:80` to `192.168.10.10:8080/tcp` and attaches it
   to `lr0`.
3. Plumbs the VIP into the forward path with a static route on
   `upstream` (`192.0.2.50/32 via 100.64.1.2`) and a scope-link route
   on `gateway-1` (`192.0.2.50/32 dev br-ex`). The agent does not yet
   propagate `Load_Balancer` VIPs into vrf-provider / br-ex (a
   follow-up to #109), so the scenario seeds these routes itself.
4. Starts a tiny Go HTTP responder (`/usr/local/bin/pf-backend`, built
   in the same Dockerfile stage as the agent) inside the existing
   `vm1` netns on `gateway-3`; it logs the source IP of each accepted
   connection.
5. `curl`s `http://192.0.2.50/` from `client-1` (polled for up to
   `RECONCILE_TIMEOUT=60s`) and asserts the backend log records a
   `peer=<client-1-eth1-IP>:*` line. A stray SNAT step on the way in
   (LR internal address, gateway chassis address, …) would substitute
   a different IP here and fail the grep.

The EXIT trap removes the `Load_Balancer`, both kernel routes and the
responder, returning the lab to baseline so a subsequent
`make e2e-baseline` keeps passing.

::: details Why a Load_Balancer, not a dnat_and_snat NAT row
A `dnat_and_snat` row performs SNAT on the way in — exactly the
property the scenario is meant to disprove. Pure-DNAT NAT rows
(`type=dnat`) carry no port-mapping, so they cannot express the
`:80 → :8080` translation the issue asks for. `Load_Balancer` is the
canonical OVN port-forward primitive — the same `lr-lb-add` path
`neutron-ovn-agent` and `kube-ovn` use in production — and is the only
ovn-nbctl primitive that combines pure DNAT with a per-port mapping
today.
:::

**Overrides for triage:** `VIP`, `VIP_PORT`, `BACKEND_IP`,
`BACKEND_PORT`, `MASTER`, `MASTER_UNDERLAY`, `RECONCILE_TIMEOUT`,
`SANITY_GATE`.

### Port-forward hairpin masquerade

```sh
make e2e-pf-hairpin
```

[`pf-hairpin.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/pf-hairpin.sh)
exercises the agent's `hairpin_masquerade` flag — the source-selective
SNAT documented in
[Port forwarding (DNAT)](../explanation/port-forwarding.md).

The agent performs port-forward DNAT in the chassis kernel (via
nftables). When a FIP on the same chassis dials the VIP, the backend
reply traverses OVN directly and bypasses the chassis conntrack, so the
client sees the backend's tenant IP as the reply source and drops the
segment. `hairpin_masquerade: true` adds a `postrouting_snat`
masquerade rule that rewrites the source to the chassis IP, so the
backend replies through the chassis and conntrack reverses both NAT
layers.

**Topology the scenario adds.** On top of the baseline lab the scenario
adds — scenario-locally:

- a new FIP `192.0.2.13` on `lr0` (`fip-c`) with a backing LSP
  `ls0-vmc` (`192.168.10.13`, MAC `02:00:00:00:0a:0c`) and a `vmc`
  netns + veth on `gateway-1`, so the client behind the new FIP is
  co-located with the active master;
- an OVS internal port `tenant-shim` on `gateway-1:br-int`, bound to a
  new LSP `ls0-shim` (`192.168.10.99`, MAC `02:00:00:00:0a:99`,
  port_security disabled so the asymmetric source on the forward leg
  is allowed through).

The shim is the route the kernel uses to reach `vm1` on `gateway-3`
once nftables has rewritten the destination; its ARP responder is what
lets `vm1` reach the shim when the masquerade flag is on. Without it
both phases would time out — the forward packet would be dropped at
`gateway-1`, which has no route for the tenant network in `main` — and
the test would not be load-bearing.

**The two phases** share that static topology and differ only in the
agent's config:

1. **Phase 1 (negative).** The scenario writes
   `/etc/ovn-network-agent/config.yaml` on `gateway-1` with the
   baseline config plus a `port_forwards` entry for
   `198.51.100.50:53 → 192.168.10.10:53` (`hairpin_masquerade: false`),
   `docker restart`s the gateway container so the agent reloads the
   config, waits for the chassis to re-bind `cr-lr0-public` and for the
   DNAT rule to appear in `nft list table ip ovn-network-agent`, then
   probes `198.51.100.50:53` from inside the `vmc` netns with
   `timeout 5 bash -c '</dev/tcp/198.51.100.50/53'`. The handshake
   **must** fail; if it succeeds the flag is not load-bearing and the
   assertion turns the job red.
2. **Phase 2 (positive).** The scenario rewrites the config with
   `hairpin_masquerade: true`, restarts the gateway again, waits for
   the chassis re-bind and DNAT rule, then polls the same probe for up
   to `RECONCILE_TIMEOUT` (default 60 s). The probe **must** complete,
   and `nft list table ip ovn-network-agent` on `gateway-1` **must**
   carry a `ct original daddr 198.51.100.50 ... masquerade` rule. Both
   the `phase1-off` and `phase2-on` nft snapshots are written to
   `ARTIFACTS_DIR`, so a failure shows the exact ruleset that was in
   effect.

The EXIT trap restores the baseline agent config, restarts `gateway-1`
once more so the restored config takes effect, and removes the FIP,
both LSPs, the `vmc` netns, `tenant-shim` and the `pf-backend`
process — so a subsequent `make e2e-baseline` keeps passing.
`loopback1` is provisioned unconditionally by `gwnode-entrypoint.sh`
(`setup_loopback`) on every container start, because the agent looks
the device up as soon as `port_forwards:` is present in the config —
even when no VIP has `manage_vip: true` — and creating it from the
scenario would not survive a `docker restart`.

::: details Why the VIP is 198.51.100.50, not the 192.0.2.50 the issue names
The agent's port-forward DNAT fires in the chassis kernel, so VIP
traffic from `vmc` has to leave OVN and transit `gateway-1`'s kernel.
`192.0.2.50` is inside the provider network `192.0.2.0/24`, which is a
connected route on `lr0` — OVN would deliver VIP traffic on the public
logical switch and ARP for it there (nothing answers), and the kernel
DNAT would never see the packet. A VIP outside every OVN-connected
subnet — `198.51.100.50` (TEST-NET-2) — follows `lr0`'s default route
out through `cr-lr0-public` onto `br-ex`, where the agent's MAC-tweak
flow hands it to the kernel and `prerouting` DNAT catches it in
transit.
:::

::: details Why docker restart, not systemctl restart ovn-network-agent
The gwnode image is not running systemd, so `systemctl restart
ovn-network-agent` (which the issue body suggests) is unavailable. The
entrypoint `exec`s the agent so it becomes PID 1; the only way to make
the agent re-read its config is to restart the whole container. This
costs ~20 s of OVS / ovn-controller / FRR re-init per phase but stays
inside the 7-minute CI budget for two reconciles.
:::

::: details Why the nftables table is "ip ovn-network-agent", not "inet ovn-network-agent"
The agent emits its table in the `ip` family — see `nftTableName` and
the `table ip %s` literal in
[nftables.go](https://github.com/osism/ovn-network-agent/blob/main/nftables.go#L12) —
even though the issue body says `inet ovn-network-agent`. The artifact
capture and the masquerade-rule assertion both target
`ip ovn-network-agent`; `NFT_TABLE_FAMILY` and `NFT_TABLE_NAME` are
exposed as overrides for forward compatibility.
:::

**Overrides for triage:** `VIP`, `VIP_PORT`, `BACKEND_IP`,
`BACKEND_PORT`, `FIP_C`, `FIP_C_INTERNAL`, `MASTER`, `WORKLOAD_HOST`,
`RECONCILE_TIMEOUT`, `RESTART_TIMEOUT`, `SANITY_GATE`.

### Stale chassis

```sh
make e2e-stale-chassis
```

[`stale-chassis.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/stale-chassis.sh)
exercises the agent's garbage-collection path for managed NB rows after
a peer chassis disappears **without** a graceful shutdown. It runs the
baseline first as a sanity gate, seeds a sentinel managed static route
on `lr0` tagged with
`external_ids:ovn-network-agent-chassis=<MASTER>`, then:

1. **Hard-kills `gateway-1`** with `docker kill -s KILL`. The agent's
   SIGTERM handler is intentionally skipped, so the dead chassis
   cleans nothing up itself.
2. **Drains the dead chassis in NB** with
   `ovn-nbctl lrp-set-gateway-chassis lr0-public <MASTER> 0` — the same
   mutation the agent's own `DrainGateways` writes on graceful shutdown
   ([ovn_gateway.go:589](https://github.com/osism/ovn-network-agent/blob/main/ovn_gateway.go#L589)).
   The scenario runs it externally because the killed agent never got
   to; in production an HA orchestrator (BFD monitor / Pacemaker /
   neutron-ovn-agent) is responsible for this step.
3. **Removes the SB Chassis row** with `ovn-sbctl chassis-del`,
   simulating the external reaper (neutron-ovn-agent on `chassis-down`,
   ovn-northd's own stale-chassis sweeper on recent OVN versions, or an
   HA orchestrator observing the node down).

Surviving agents on `gateway-2` and `gateway-3` then notice the chassis
row is gone from SB, wait `stale_chassis_grace_period` (configured to
`30s` in the gwnode E2E config so the scenario stays inside its CI
budget), and remove the rows tagged for the dead chassis via
`CleanupStaleChassisManagedEntries`.

The scenario polls NB for up to `STALE_TIMEOUT` (default 150 s) and
additionally greps the surviving agents' `docker logs` for the
`stale chassis route removed` line referencing `chassis=<MASTER>` —
both signals must fire to prove the cleanup was deliberate. On exit the
killed chassis is restarted with `docker start` (so the artifact
collector can still `docker exec` into it) and the residual sentinel
route is removed.

::: warning Destructive — not chainable with failover
Because the scenario externally drains `gateway-1`'s `Gateway_Chassis`
priority to 0 after the kill, OVN does **not** re-elect `gateway-1` on
`docker start`, and `gateway-2` stays master. `make e2e-baseline`
against the same lab keeps passing — reachability via the new master
is intact. But the lab is now HA-asymmetric (single-master,
`gateway-1` permanently drained at priority 0), so `make e2e-failover`
against it has no priority-30 master left to fail and will misbehave.

Chain `make e2e-down && make e2e-up` between destructive scenarios when
running locally. CI handles this automatically: the workflow's
`make e2e-down` step runs with `if: always()` regardless of the
scenario outcome.
:::

::: details Why a hard kill, not docker stop or ovn-ctl stop_controller
`docker stop` delivers SIGTERM, which the agent traps to run its
graceful-shutdown path — that case is what the failover scenario
already covers. The stale-chassis path is specifically for the
non-graceful death case where surviving peers are the only ones that
can clean up. `docker kill -s KILL` skips every signal handler in the
container.
:::

::: details Why the explicit chassis-del is needed
A killed ovn-controller does **not** remove its own SB row — the row
is only released on graceful shutdown. Without the explicit deletion,
surviving agents would keep seeing the dead chassis as alive and the
cleanup loop would never fire. The explicit `chassis-del` simulates the
external reaper that removes a dead chassis row in production
(neutron-ovn-agent on `chassis-down`, ovn-northd's own stale-chassis
sweeper on recent OVN versions, or an HA orchestrator observing the
node down). The path under test is what the agent does *after* the row
disappears, not how the row disappears.
:::

::: details Why a sentinel managed route is needed
The production code path the cleanup loop targets — managed static
routes tagged with a chassis name — is not exercised by the baseline
lab on its own: the agent only creates a default route via the virtual
gateway IP, and the new master re-tags that row instead of leaving an
orphan for the cleanup loop to find (see `ensureDefaultRoute` in
`ovn_gateway.go`). Seeding a unique sentinel prefix gives the cleanup
loop a row that no surviving agent will reclaim, so its deletion is
unambiguous evidence of the stale-chassis path running.
:::

**Overrides for triage:** `MASTER`, `PEERS`, `STALE_TIMEOUT`,
`SENTINEL_PREFIX`, `LR_PUBLIC_PORT`, `SANITY_GATE`.

### Manual setup for triage

The sequence below is equivalent to `make e2e-up`, useful when you need
to step through the bring-up by hand:

```sh
docker build -f test/e2e/Dockerfile.central -t ovn-network-agent/central:e2e .
docker build -f test/e2e/Dockerfile.gwnode  -t ovn-network-agent/gwnode:e2e  .

sudo containerlab deploy   -t test/e2e/topology.clab.yml
./test/e2e/bootstrap.sh

# Inspect the agent on gateway-1:
docker exec clab-ovn-e2e-gateway-1 ovn-network-agent --help
docker exec clab-ovn-e2e-gateway-1 ovs-vsctl show
# Linux truncates /proc/<pid>/comm to 15 chars, so the agent process must
# be matched via the full cmdline (pgrep -f), not pgrep -x.
docker exec clab-ovn-e2e-gateway-1 pgrep -f /usr/local/bin/ovn-network-agent

# Inspect OVN central:
docker exec clab-ovn-e2e-central ovn-nbctl show

sudo containerlab destroy -t test/e2e/topology.clab.yml --cleanup
```

## Image-size budget

The acceptance criteria require the gwnode image to stay under
**600 MB**. The Dockerfile keeps the build slim by:

- using `--no-install-recommends` for every apt install,
- aggressively purging `software-properties-common` after the
  `cloud-archive:flamingo` PPA is registered,
- removing `/var/lib/apt/lists`, `/var/cache/apt/archives`,
  `/usr/share/doc`, `/usr/share/man`, `/usr/share/locale` after
  install,
- copying only the statically-linked agent binary from the Go build
  stage.

Check the resulting size with:

```sh
docker image inspect ovn-network-agent/gwnode:e2e \
    --format '{{.Size}}' | numfmt --to=iec --suffix=B
```

## Continuous integration

The harness runs on every push to `main` and on manual
`workflow_dispatch` via
[`.github/workflows/e2e.yml`](https://github.com/osism/ovn-network-agent/blob/main/.github/workflows/e2e.yml).
The workflow does **not** run on pull requests: spinning the lab up
adds ~10 minutes to CI on a green run, which is too coarse for the
per-PR feedback loop the rest of the workflows target.

Five jobs run, each on its own runner so a regression in one scenario
is reported in isolation:

- **`baseline`** — installs containerlab, runs `make e2e-up`, executes
  `test/e2e/scenarios/baseline.sh`, dumps + uploads artifacts on
  failure (`e2e-artifacts-<run id>-<attempt>`), and always tears the
  lab down.
- **`failover`** (`needs: baseline`) — same shape as baseline, but
  executes `test/e2e/scenarios/failover.sh`. On failure the artifact
  bundle is uploaded as `e2e-artifacts-failover-<run id>-<attempt>`.
- **`hairpin`** (`needs: baseline`, runs in parallel with `failover`)
  — same shape, but executes `test/e2e/scenarios/hairpin.sh`. The
  job points the scenario's `ARTIFACTS_DIR` at the same artifact
  root so the before/after `cookie=0x998` `dump-flows` snapshots are
  bundled with the lab-state dump. On failure the artifact bundle
  is uploaded as `e2e-artifacts-hairpin-<run id>-<attempt>`.
- **`pf-external`** (`needs: baseline`, runs in parallel with
  `failover` and `hairpin`) — same shape, but executes
  `test/e2e/scenarios/pf-external.sh`. The job points the scenario's
  `ARTIFACTS_DIR` at the same artifact root so the backend
  source-IP log is bundled with the lab-state dump on failure (per
  issue #109's acceptance criterion). On failure the artifact
  bundle is uploaded as `e2e-artifacts-pf-external-<run id>-<attempt>`.
- **`stale-chassis`** (`needs: failover`) — same shape, but executes
  `test/e2e/scenarios/stale-chassis.sh`. The job points the
  scenario's `ARTIFACTS_DIR` at the same artifact root so the
  before/after NB snapshots and the peer cleanup-log capture are
  bundled with the lab-state dump. On failure the artifact bundle is
  uploaded as `e2e-artifacts-stale-chassis-<run id>-<attempt>`.

All five jobs are capped at 15 minutes — matching the budgets in
issues
[#45](https://github.com/osism/ovn-network-agent/issues/45),
[#105](https://github.com/osism/ovn-network-agent/issues/105),
[#108](https://github.com/osism/ovn-network-agent/issues/108),
[#109](https://github.com/osism/ovn-network-agent/issues/109), and
[#111](https://github.com/osism/ovn-network-agent/issues/111).

### Triaging a failed run

The uploaded artifact bundle mirrors the directories the collector
writes:

```
<artifact>/
  inspect/containerlab.txt         — output of `containerlab inspect`
  docker/<node>.log                — `docker logs` per lab container
  ovs/<gateway>/show.txt           — OVS bridges and interfaces
  ovs/<gateway>/br-int-flows.txt   — OpenFlow dump for br-int
  ovs/<gateway>/br-ex-flows.txt    — OpenFlow dump for br-ex
  ovn/nb-show.txt                  — `ovn-nbctl show` on central
  ovn/sb-show.txt                  — `ovn-sbctl show` on central
  ovn/nb-<table>.txt               — full NB row dumps (NAT, Gateway_Chassis, …)
  ovn/sb-<table>.txt               — full SB row dumps (Chassis, Port_Binding, …)
  frr/<gateway>-running-config.txt — `vtysh -c "show running-config"`
  frr/<gateway>-bgp-summary.txt    — `vtysh -c "show bgp summary"`
  kernel/<gateway>-ip-route.txt    — `ip route show table all`
  agent/<gateway>.log              — copy of the gateway container's stdout
  hairpin/hairpin-flows-before.txt — `cookie=0x998` flows on master:br-ex before adding FIP_B (hairpin only)
  hairpin/hairpin-flows-after.txt  — `cookie=0x998` flows on master:br-ex after adding FIP_B (hairpin only)
  pf-external/pf-backend.log       — per-connection source-IP log from the workload-side HTTP responder (pf-external only)
  stale-chassis/nb-before-kill.txt — NB rows tagged for the killed chassis pre-kill (stale-chassis only)
  stale-chassis/nb-after-kill.txt  — NB rows still tagged for the killed chassis after the cleanup deadline (stale-chassis only)
  stale-chassis/peer-cleanup.log   — surviving peer's `stale chassis route removed` line (stale-chassis only)
```

You can reproduce the same dump on a local lab with:

```sh
./test/e2e/scenarios/collect-artifacts.sh /tmp/e2e-artifacts
```

## Multi-architecture builds

The Go build stage in `Dockerfile.gwnode` honours `TARGETARCH`, and
the Ubuntu/OVS/OVN/FRR packages used at runtime are published for
both `amd64` and `arm64`. Multi-arch publication requires the
`docker-buildx-plugin` (on Debian/Ubuntu Docker CE hosts:
`sudo apt-get install -y docker-buildx-plugin`). Once it is
installed:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
    -f test/e2e/Dockerfile.gwnode -t ovn-network-agent/gwnode:e2e --push .
```

The `make e2e-up` target only builds for the host platform via plain
`docker build`, which is the right default for local development and
CI on a single runner and does not need the buildx plugin.

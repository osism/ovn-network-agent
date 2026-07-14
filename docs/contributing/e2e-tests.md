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
    drain-hitless.sh        — graceful drain vs hard kill, hitless comparison (issue #113)
    collect-artifacts.sh    — dump lab state for offline triage
  chaos/                    — seeded chaos runner, `make e2e-chaos` (issue #176)
    main.go                 — flags, wiring, exit codes, artifact collection
    engine.go               — the seeded tick loop: draw, guardrails, execute, converge
    actions.go              — the fault registry (four starter actions)
    lab.go                  — the single seam to docker / containerlab
    state.go                — the combined start state and the node-restore path
    probe.go                — continuous reachability probe from client-1
    checks.go               — baseline invariants (agents alive, no dual claim)
    journal.go              — the JSONL journal and the run record
  pf-backend/
    main.go                 — tiny HTTP responder shipped at /usr/local/bin/pf-backend
                              in the gwnode image; logs each connection's source IP
                              for the port-forward scenario's assertion.
```

## Prerequisites

- A Linux host with Docker (privileged mode is required for the
  containerlab containers).
- [containerlab](https://containerlab.dev/install/). CI and
  `make e2e-install-tools` install the pinned version (`0.77.0`); any
  release ≥ 0.55 works locally.
- `make`, `docker buildx`, and a Go toolchain matching `go.mod` — the
  gwnode image builds the agent from source.

On Debian/Ubuntu, `make e2e-install-tools` downloads the pinned
containerlab `.deb`, verifies its sha256 against the checksum committed
in the `Makefile`, and installs it with `apt-get` — a known, verified
binary instead of piping the upstream `get.containerlab.dev` installer
into a shell. It is a no-op when `containerlab` is already on `PATH`. To
bump the version, update `CONTAINERLAB_VERSION` and **both**
`CONTAINERLAB_SHA256_*` values in the `Makefile` from the upstream
release's `checksums.txt`. On non-Debian distributions the target stops
with a pointer to the [upstream install docs](https://containerlab.dev/install/);
install containerlab there manually.

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

Bring-up gates on three readiness waits: OVN NB reachability, SB
chassis registration for every gateway, and the upstream `bgpd`. The
`bgpd` start — the one daemon bootstrap starts itself — is retried up
to `BGPD_START_ATTEMPTS` (default `3`) times, each attempt waiting
`BGPD_WAIT_SECS` (default `30` s) for the daemon to register, so a
one-off startup hiccup heals itself instead of failing the job. When
any gate times out, the awaited daemon's own output and surrounding
state are dumped into the job log (and, for a CI run, the collected
artifacts — see [Triaging a failed run](#triaging-a-failed-run)) so
the failure can be root-caused after the fact. The mid-run lab recycle
in `drain-hitless` re-runs the same gates, so it inherits the same
diagnostics and retries.

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
| [Chaos runner](#chaos-runner) | `e2e-chaos` | Under a seeded, randomized fault sequence the agents stay alive, reachability recovers within budget, and no gateway port is claimed twice. | [#176](https://github.com/osism/ovn-network-agent/issues/176) |
| [Drain-hitless](#drain-hitless) | `e2e-drain-hitless` | A graceful `SIGTERM` drain loses fewer packets than a hard `docker kill` of the same chassis. | [#113](https://github.com/osism/ovn-network-agent/issues/113) |
| [Failover](#failover) | `e2e-failover` | `cr-lr0-public` re-elects to a surviving chassis after the master is lost. | [#105](https://github.com/osism/ovn-network-agent/issues/105) |
| [Hairpin](#hairpin) | `e2e-hairpin` | The `cookie=0x998` hairpin flow reflects FIP-to-FIP traffic on `br-ex`. | [#108](https://github.com/osism/ovn-network-agent/issues/108) |
| [Multi-VLAN](#multi-vlan) | `e2e-multi-vlan` | Two VLAN provider networks on one node get per-segment kernel interfaces, flows, and BGP-announced FIPs. | [#147](https://github.com/osism/ovn-network-agent/issues/147) |
| [Port-forward (external client)](#port-forward-external-client) | `e2e-pf-external` | Inbound DNAT preserves the external client's source IP. | [#109](https://github.com/osism/ovn-network-agent/issues/109) |
| [Port-forward hairpin masquerade](#port-forward-hairpin-masquerade) | `e2e-pf-hairpin` | The `hairpin_masquerade` flag is load-bearing for a co-located FIP. | [#110](https://github.com/osism/ovn-network-agent/issues/110) |
| [Stale chassis](#stale-chassis) | `e2e-stale-chassis` | Surviving peers garbage-collect the NB rows of a hard-killed chassis. | [#111](https://github.com/osism/ovn-network-agent/issues/111) |

Every scenario except `baseline` runs the baseline first as a sanity
gate; set `SANITY_GATE=0` to skip it. The [chaos runner](#chaos-runner)
is the one exception: it gates on its own combined start state instead,
which is a superset of the baseline. The subsections below describe what
each scenario does and which environment variables it accepts for triage.

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

### Drain-hitless

```sh
E2E_DRAIN_ON_SHUTDOWN=true make e2e-up
make e2e-drain-hitless
```

[`drain-hitless.sh`](https://github.com/osism/ovn-network-agent/blob/main/test/e2e/scenarios/drain-hitless.sh)
compares the agent's graceful-drain code path
([`DrainGateways`](https://github.com/osism/ovn-network-agent/blob/main/ovn_gateway.go))
against the hard-kill case (#105's mechanic, reused here as the
control arm) and asserts the graceful path stays hitless. The
graceful arm sends `docker exec … kill -TERM 1` on `gateway-1`
(the gwnode entrypoint `exec`s the agent at startup, so the agent
is PID 1 — no `pgrep` needed and the containerlab veth pair is
not torn down between SIGTERM and the drain completing). The
hardkill arm uses `docker kill -s KILL clab-${LAB}-gateway-1`.
Both arms first run `docker update --restart=no` on the gateway
container so that containerlab's default `restart: always` does
not auto-revive the agent mid-measurement and confuse the
migration check.

The lab must be deployed with the drain armed (the
`E2E_DRAIN_ON_SHUTDOWN=true make e2e-up` above, which is also how the
CI job deploys it): `topology.clab.yml` forwards
`E2E_DRAIN_ON_SHUTDOWN` into the gateway
containers as `OVN_NETWORK_DRAIN_ON_SHUTDOWN`, where the agent's env
layer beats the `drain_on_shutdown: false` default in
`gwnode-config.yaml`. The flag is deliberately not baked into the
shared config: every agent SIGTERM would then drain, and pf-hairpin's
`docker restart` of the master would hand mastership away for good (a
drained chassis restores at standby priority 1, and
`EnsureActivePriorityLead` never hands the election back). It also
cannot be flipped per-node at runtime the way pf-hairpin rewrites its
config file, because the `docker restart` that reload requires
destroys the containerlab veth `gateway-1:eth1 ↔ upstream:eth1` — the
link the measurement rides on. The scenario fail-fasts with a
remediation message when the lab was deployed without the variable.
With the drain armed, the agent lowers its `Gateway_Chassis` priority
to 0 on SIGTERM and blocks until `cr-lr0-public` migrates before
exiting — that ordering is what keeps the `client-1 → FIP` outage
inside the graceful budget during the transition.

The probe is `ping -i 0.1 -c 200` from `client-1` (20 s probe
window, 100 ms inter-packet spacing — finer than OVN's BFD detection
multiplier of 3×1 s); the kill is delivered `PROBE_PRELUDE` seconds
into the probe so the transition lands inside the captured window.
The loss count comes straight from `ping`'s
`N packets transmitted, M received` summary, so the scenario stays
single-file bash without tshark post-processing.

Each arm then has `FAILOVER_TIMEOUT` seconds **from the moment the
kill fired** (default 30 s — the same RTO
[#105](https://github.com/osism/ovn-network-agent/issues/105) asserts
for this lab) to satisfy both failover signals: `cr-lr0-public` must
have migrated away from `gateway-1`, and `client-1 → FIP` must answer
again. The migration check is the false-pass guard — a 0-loss reading
without it would only indicate the kill never fired, since OVS on the
dead master can keep executing stale flows for a while.

The migration is *dated* from the transition timeline rather than
sampled once the probe is over. The kill lands `PROBE_PRELUDE` seconds
into a 20 s probe, so the scenario does not regain control until ~17 s
after the kill; a sample taken then can only show the end state, never
the instant it was reached, and every `FAILOVER_TIMEOUT` shorter than
the probe's tail would pass unconditionally. Reachability is polled
rather than dated: the probe already measured how long the data plane
was down (`loss × PROBE_INTERVAL`) but it cannot see past its own end,
so what the poll adds is whether the FIP came back at all.

Each arm records a once-per-second transition timeline pairing the SB
`cr-lr0-public` chassis with the BGP nexthop `upstream` routes the FIP
`/32` to. Because the RTO check reads it, the timeline is recorded on
every run — a local `make e2e-drain-hitless` holds the agent to the
same budget the CI job does. When `ARTIFACTS_DIR` is set each arm
additionally records, best-effort, a `tcpdump` capture of the probe on
`client-1`'s `eth1` (rendered to text with absolute timestamps, so the
ICMP sequence gap can be lined up against the kill). The capture can
never fail an arm: the loss count never depends on it.

The graceful arm additionally requires two log lines in `gateway-1`'s
`docker logs`: `drain: gateway chassis priority lowered` **and**
`drain: complete`. The first is emitted while `DrainGateways` is still
building the OVSDB operations, so on its own it survives a transaction
that never commits — the gateway then dies exactly like a hard kill
while the log still claims the drain ran. Only `drain: complete` is
emitted after the priority-0 update landed and the SB poll saw the port
migrate away. A `drain failed` or `drain: timeout exceeded` line fails
the arm outright and is bundled into `graceful-drain-log.txt` so the
artifact names the real cause. Without these checks a low-loss reading
could be explained by the agent racing the kernel teardown rather than
by the drain code path running; the criterion guards against a future
change that lets the agent exit before `DrainGateways` completes.

Between the two arms the scenario itself runs
`make e2e-down && make e2e-up` so both arms start from the same
priority-30/20/10 baseline. The recycle is mandatory: after the
graceful arm `gateway-1` has been drained to priority 0 and exited;
`docker start`ing it would re-attach with `RestoreDrainedGateways`
setting priority back to 1, not the 30 the bootstrap seeds, and the
hardkill arm would no longer be comparable. The recycled lab is then
gated on `baseline.sh` before the hardkill arm starts — `make e2e-up`
returns once bootstrap has seeded NB, but the agents still need a
reconcile cycle before the FIP answers, and a probe started inside
that window would charge the reconcile drops to the control arm.

The same EXIT trap recycles the lab once more after the hardkill arm
so a developer run leaves the lab baseline-green. CI sets
`RECYCLE_ON_EXIT=0` instead: its `make e2e-down` step already runs
with `if: always()`, so a third `containerlab deploy` would only eat
into the job's time budget and, on the failure path, destroy the
lab state the artifact collector is about to dump.

The scenario asserts three things:

1. the graceful arm's outage is at most `GRACEFUL_MAX_OUTAGE_MS`
   (default **1500 ms**) — the hitless claim;
2. `graceful_loss` is strictly less than `hardkill_loss` — what makes
   the comparison meaningful, since both arms hit the same lab and the
   same FIP, so the delta between them *is* the hitless gain;
3. the budget in (1) is itself strictly below `hardkill_loss` — a
   budget a hard kill would pass cannot tell a broken drain from a
   working one, so the scenario refuses to assert it.

**Where the budget comes from.** Issue #113 phrased its ceiling as "at
most one lost packet" (100 ms at the default probe spacing) before any
measurement existed, and the first calibration replaced that with 500 ms
off a single 200 ms sample. One sample is not a distribution: 500 ms
derives a ceiling of 5 packets, healthy runs routinely measured exactly
5, and ordinary runner noise then failed unrelated pull requests
([#182](https://github.com/osism/ovn-network-agent/issues/182)).

The 1500 ms default rests on the graceful-arm loss of **20 CI runs** —
every drain-hitless run carrying the drain takeover handshake
([#129](https://github.com/osism/ovn-network-agent/issues/129), on
`main` since 2026-07-11):

| Graceful loss | Outage | Runs |
| --- | --- | --- |
| 2 packets | 200 ms | 3 |
| 3 packets | 300 ms | 5 |
| 4 packets | 400 ms | 2 |
| 5 packets | 500 ms | 6 |
| 6 packets | 600 ms | 3 |
| 11 packets | 1100 ms | 1 |

(n=20, min 200 ms, median 450 ms, p90 600 ms, max 1100 ms.)

1500 ms clears the worst of those by 1.36×. The ceiling is bounded from
above as well, and that half is what keeps the scenario honest: the
hard-kill control — same lab, same probe, a chassis that dies without
draining — measured 23–30 packets (2300–3000 ms) across the same runs,
in a much tighter band. 1500 ms sits at 65 % of the tightest control
run, so a shutdown that fails to drain still overshoots the budget by at
least 8 packets. Assertion 3 re-checks that on every run instead of
trusting today's numbers to hold: raising `GRACEFUL_MAX_OUTAGE_MS` until
it can no longer discriminate now *fails* the scenario rather than
quietly hollowing it out. If the budget starts failing again, re-derive
it from ~20 runs (every run prints its loss and bundles it into
`summary.txt`) rather than nudging it past the last red run.

**What the budget bounds.** The lost packets sit in OVN's
northd/ovn-controller flow-reprogramming window after the
`Port_Binding` migration: between `cr-lr0-public` moving to `gateway-2`
and the gateways' flows converging on the new owner, `upstream` still
ECMPs part of the traffic at a chassis that no longer owns the port.
The agent bounds its own half of that window — the #129 drain takeover
handshake makes `DrainGateways` wait for the takeover chassis to stamp
its NB readiness marker (its word that it has announced the FIP routes)
and then hold `drain_settle_delay` before cleanup withdraws the leaving
node's routes, all bounded by `drain_timeout`. That handshake is why the
numbers above look the way they do: before it landed the arm was
bimodal, three of five runs measuring 48–49 packets (~4.9 s); across the
twenty runs since, nothing has exceeded 11. What remains inside the
budget is OVN's reprogramming on the *takeover* chassis, which the agent
does not control.

That 48–49-packet mode is also the concrete regression the budget has to
keep catching — a drain that still *runs* (the log says `drain:
complete`, so the drain-log gate is satisfied) but no longer *delivers* a
hitless failover. At 15 packets the ceiling catches it more than three
times over. What it will not catch, stated plainly: a degradation inside
the budget, say the median moving from 450 ms to 1000 ms. The arm's own
spread on this runner class (200–1100 ms) is wider than such a shift, so
no ceiling separates the two without failing on the tail. The scenario
resolves gross regressions, not gradual ones — watch the
`graceful_outage_ms` trend in `summary.txt` for those.

The scenario keeps the duration primary and derives the packet ceiling
from `PROBE_INTERVAL` so overriding the spacing cannot silently move the
budget — at the 0.2 s unprivileged `iputils` floor, a packet ceiling
would otherwise silently double.

**Overrides for triage:** `MASTER`, `FIP`, `UPSTREAM`,
`PROBE_INTERVAL`, `PROBE_COUNT`, `PROBE_PRELUDE`, `FAILOVER_TIMEOUT`,
`GRACEFUL_MAX_OUTAGE_MS`, `SKIP_RECYCLE`, `RECYCLE_ON_EXIT`,
`SANITY_GATE`.

### Chaos runner

```sh
make e2e-chaos
make e2e-chaos CHAOS_FLAGS="-duration 3m -seed 7 -out /tmp/chaos-a"
```

[`chaos/`](https://github.com/osism/ovn-network-agent/tree/main/test/e2e/chaos)
is not a scenario but a driver. The eight scenarios each prove one fault
in isolation; production faults overlap and repeat. The runner drives the
same lab through a *randomized* fault sequence for a bounded duration and
leaves a journal you can triage from — and replay.

It is a Go `main` package rather than another bash script for two
reasons: `$RANDOM` is not stable across bash versions, which would break
the replay contract outright, and the run needs concurrent probing plus
structured artifacts. Its unit tests run under `make test` on any
platform (they drive the real engine against a fake `docker`).

**Inputs — and the replay contract.** The seed, the duration, the tick
bounds and the action weights are the *only* inputs, and every decision
the engine makes is drawn from a PCG stream seeded by `-seed` alone:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-seed` | `42` | Seeds every decision: the tick interval, the action, the target, the hold. |
| `-duration` | `10m` | How long to keep injecting faults. Must be at least `1s`. |
| `-tick-min` / `-tick-max` | `10s` / `30s` | Bounds of the interval between decisions. `-tick-min` must be at least `100ms`: a tick interval near zero spins the loop and grows the journal without bound, on a run where no fault fits between two ticks. |
| `-weights` | registry defaults | `name=n,…`; an unknown action name is rejected. `-weights gateway-kill=0` disables a fault. |
| `-lab` | `ovn-e2e` | containerlab lab name. |
| `-out` | `chaos-artifacts` | Where the journal, the run record and the lab-state dump land. |
| `-collect` | `test/e2e/scenarios/collect-artifacts.sh` | The lab-state collector, run into `<out>/lab-state` when the run does not pass. The default is repo-relative, so run the binary from the repo root (`make e2e-chaos` does). |

Two runs with identical inputs against identically-behaving labs replay
the identical action sequence. Diff them:

```sh
make e2e-up && make e2e-chaos CHAOS_FLAGS="-duration 3m -out /tmp/chaos-a"
make e2e-down && make e2e-up
make e2e-chaos CHAOS_FLAGS="-duration 3m -out /tmp/chaos-b"

diff \
  <(jq -c 'select(.event=="decision") | {tick,action,target,interval_ms,hold_ms}' /tmp/chaos-a/journal.jsonl) \
  <(jq -c 'select(.event=="decision") | {tick,action,target,interval_ms,hold_ms}' /tmp/chaos-b/journal.jsonl)
```

Each tick draws exactly four values — interval, action, target, hold —
**before** the guardrails are consulted, so a guardrail skip cannot shift
the stream: the run that skipped a decision draws the same values
afterwards as the run that executed it. Wall clock only bounds *how many*
ticks fit in `-duration`, so a slower lab truncates the tail of the
sequence rather than changing it.

**Combined start state.** The runner layers the existing scenarios'
setups onto the bootstrap baseline, so one run exercises all of them at
once: `hairpin.sh`'s second FIP (`192.0.2.12` with a `vm2` responder),
`multi-vlan.sh`'s two VLAN provider networks (tags 101/102, routers
pinned to `gateway-1`), and `pf-external.sh`'s `Load_Balancer` VIP
(`192.0.2.50:80` in front of the `vm1` backend). The layering is
idempotent, which is what makes it reusable as the post-fault restore
path. If the start state is not green within 120 s the run aborts with
exit code 2 — a fault injected into a lab that was not green to begin
with would report false violations for the rest of the run.

**Probes.** A goroutine per target samples reachability from `client-1`
once a second for the whole run, so loss *during* a fault hold is
recorded even while the engine is blocked:

| Probe | Target |
| --- | --- |
| `fip-vm1` | `ping 192.0.2.10` |
| `fip-vm2` | `ping 192.0.2.12` |
| `fip-vlan101` | `ping 198.51.100.10` |
| `fip-vlan102` | `ping 203.0.113.10` |
| `pf-vip` | `curl http://192.0.2.50:80/` |

The baseline lab's second FIP, `192.0.2.11`, is deliberately **not**
probed: `bootstrap.sh` seeds its NAT row but nothing answers behind
`192.168.10.11`, so it would be red for the whole run.

**Actions.** The starter set reuses the fault mechanics the scenarios
already prove. The registry order and the weights are part of the replay
contract — a new action is appended, never inserted.

| Action | Weight | Hold | Recovery budget | Mechanic |
| --- | --- | --- | --- | --- |
| `controller-restart` | 3 | 10–30 s | 90 s | `ovn-ctl stop_controller` / `start_controller` ([failover](#failover)) |
| `gateway-kill` | 1 | 15–45 s | 180 s | `docker kill -s KILL`, then `docker start` ([stale chassis](#stale-chassis)) |
| `agent-terminate` | 2 | 10–30 s | 180 s | `kill -TERM 1` — the agent is PID 1 ([drain-hitless](#drain-hitless)) |
| `gateway-restart` | 2 | — | 180 s | `docker restart` ([pf-hairpin](#port-forward-hairpin-masquerade)) |

Every action that kills a container first runs `docker update
--restart=no`, exactly as `drain-hitless.sh` does — containerlab deploys
with `restart: always`, so without it docker revives the node before the
fault is observable at all. The policy is restored on the way back.

`agent-terminate` only exercises the agent's *drain* path when the lab
was deployed with `E2E_DRAIN_ON_SHUTDOWN=true` (see
[Drain-hitless](#drain-hitless)); otherwise the SIGTERM is just a
graceful exit. The fault is the same either way — the drain only changes
how much the run loses.

**Guardrails** keep a run meaningful. A decision is skipped (and
journaled with its `skip_reason`) when the target has not returned and
converged since it was last hit, or when it is the last healthy gateway.

**Convergence and recovery budgets.** After each restore the runner polls
the node back to health: the container healthy, its chassis back in SB,
and every probe green. Budget expiry is a `recovery-timeout` violation
and parks the node — it is never targeted again and the run fails. A park
also ends the run early (journaled as `run-aborted`): the parked node's
data path stays dark, and since convergence gates on *every* probe being
green, no later action could converge either — the rest of the duration
would produce nothing but violations derived from the first one.
Budgets are measured from the *restore*, not from the injection:
resources pinned to the node under fault (the VLAN routers and the VIP on
`gateway-1`, every responder on `gateway-3`) are legitimately dark while
it is held down. The probe-loss buckets still record what happened
mid-hold.

**Baseline checks** sweep every 10 s, independently of what the engine is
doing: every node the run considers healthy must be running an agent, and
no `chassisredirect` port may be claimed by two chassis at once
(`chassis` ∪ `additional_chassis`). A sweep the runner could not answer —
central under load, the `sbctl --timeout=5` expiring — is journaled as
`check-error` and counted, not swallowed. The counts are the record's
evidence that the invariants were evaluated at all: a run whose every
dual-claim lookup failed asserted nothing, and fails with a
`checks-never-ran` violation instead of reporting a clean pass.

::: details Why the runner re-wires the containerlab veth
Any container exit destroys the containerlab veth
`gateway-N:eth1 ↔ upstream:ethN`, and `docker start` does not bring it
back — which is why [failover](#failover) stops only `ovn-controller` and
why [stale-chassis](#stale-chassis) and [drain-hitless](#drain-hitless)
tell you to recycle the whole lab. A chaos runner cannot recycle: the
guardrails only re-target a node that has *returned*. So `restoreNode`
re-creates the link with `containerlab tools veth create`, re-applies the
underlay `/30` and the BGP session `bootstrap.sh` seeds, and — on
`gateway-3` — rebuilds the netns responders and the port-forward backend
its destroyed network namespace took with it. This needs the same
privileges `containerlab deploy` already does.
:::

::: details Why the runner re-points the port-forward VIP
`pf-external.sh` pins the VIP's two routes (the forward route on
`upstream`, the scope-link route on `br-ex`) to `gateway-1`, because the
agent does not propagate `Load_Balancer` VIPs into the underlay. A chaos
run migrates the master, so the runner re-points both at whichever
chassis currently owns `cr-lr0-public` — the job an external orchestrator
does in production — and journals it as `vip-repoint`. Without it the VIP
probe would go permanently red on the first re-election and every later
violation would be noise.
:::

::: warning Destructive — recycle the lab afterwards
A chaos run leaves the lab wherever the anti-flap election landed:
`cr-lr0-public` is generally **not** back on `gateway-1`, and the runner
deliberately never re-asserts priorities mid-run. Chain
`make e2e-down && make e2e-up` before running scenarios that assume the
bootstrap master.
:::

**Artifacts.** Both land under `-out`:

```
<out>/
  journal.jsonl   — one JSON object per decision and phase, in order
  summary.json    — the run record (schema chaos-run-record/v1)
  lab-state/      — collect-artifacts.sh dump, on a non-zero exit only
```

`journal.jsonl` carries `run-start` (the echoed inputs), `state-applied`,
one `decision` per tick (with the drawn values and either `executed` or a
`skip_reason`), `inject` / `restore` / `converged`, `node-state`,
`probe-transition`, `vip-repoint`, `violation` and `run-end`.
`summary.json` aggregates the run: inputs, tick and decision counts,
actions by name, how many baseline sweeps ran and how many of them
evaluated the dual-claim invariant, per-probe sent/lost plus 10-second
loss buckets, the per-action recovery durations, and every violation.

**Exit codes:** `0` the run passed, `1` the run recorded a violation,
`2` the runner could not set the run up (bad flags, a start state that
never went green). On any non-zero exit the lab's existing
`collect-artifacts.sh` bundle is dumped into `<out>/lab-state`.

**In CI.** The runner has its own workflow,
[`e2e-chaos.yml`](https://github.com/osism/ovn-network-agent/blob/main/.github/workflows/e2e-chaos.yml),
which runs on `workflow_dispatch` only and uploads the journal, the
summary and the lab-state dump on every outcome. The seed and duration
are dispatch inputs defaulting to `-seed 42 -duration 3m`. It is
deliberately not part of `e2e.yml`'s PR path: the scenarios there each
assert one fault and leave the lab baseline-green, while a chaos run is
a randomized sequence that deliberately leaves the master wherever the
last election put it. Dispatch it from the Actions tab when you touch
the agent's failover, drain or chassis-cleanup paths.

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

The harness runs on pull requests and on manual `workflow_dispatch` via
[`.github/workflows/e2e.yml`](https://github.com/osism/ovn-network-agent/blob/main/.github/workflows/e2e.yml).
Docs-only changes are skipped with a `paths-ignore` filter (`docs/**`,
`**.md`) so a typo fix does not spin the lab up. The workflow does
**not** run on push to `main`: the branch ruleset requires PRs to be up
to date, so the merged commit already passed E2E on its PR and
re-running the ~10 min suite post-merge would only burn runner time.
The [chaos runner](#chaos-runner) is not part of this workflow — it has
its own dispatch-only
[`e2e-chaos.yml`](https://github.com/osism/ovn-network-agent/blob/main/.github/workflows/e2e-chaos.yml),
described in the chaos-runner section above.

One job runs per scenario, each on its own runner so a regression in one
scenario is reported in isolation. Every job installs containerlab and
loads the required kernel modules through the shared
[`e2e-lab-setup`](https://github.com/osism/ovn-network-agent/blob/main/.github/actions/e2e-lab-setup/action.yml)
composite action:

- **`baseline`** — runs the `e2e-lab-setup` action and `make e2e-up`,
  executes `test/e2e/scenarios/baseline.sh`, dumps + uploads artifacts
  on failure (`e2e-artifacts-<run id>-<attempt>`), and always tears the
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
- **`multi-vlan`** (`needs: baseline`, runs in parallel with the other
  baseline-gated jobs) — same shape, but executes
  `test/e2e/scenarios/multi-vlan.sh`. The job points the scenario's
  `ARTIFACTS_DIR` at the same artifact root so the per-segment
  flow/link/route snapshots are bundled with the lab-state dump. On
  failure the artifact bundle is uploaded as
  `e2e-artifacts-multi-vlan-<run id>-<attempt>`.
- **`pf-external`** (`needs: baseline`, runs in parallel with
  `failover` and `hairpin`) — same shape, but executes
  `test/e2e/scenarios/pf-external.sh`. The job points the scenario's
  `ARTIFACTS_DIR` at the same artifact root so the backend
  source-IP log is bundled with the lab-state dump on failure (per
  issue #109's acceptance criterion). On failure the artifact
  bundle is uploaded as `e2e-artifacts-pf-external-<run id>-<attempt>`.
- **`pf-hairpin`** (`needs: baseline`) — same shape, but executes
  `test/e2e/scenarios/pf-hairpin.sh`. The job points the scenario's
  `ARTIFACTS_DIR` at the same artifact root so the phase1-off /
  phase2-on / teardown `nft` snapshots are bundled with the lab-state
  dump (per issue #110's acceptance criterion). On failure the artifact
  bundle is uploaded as `e2e-artifacts-pf-hairpin-<run id>-<attempt>`.
- **`stale-chassis`** (`needs: failover`) — same shape, but executes
  `test/e2e/scenarios/stale-chassis.sh`. The job points the
  scenario's `ARTIFACTS_DIR` at the same artifact root so the
  before/after NB snapshots and the peer cleanup-log capture are
  bundled with the lab-state dump. On failure the artifact bundle is
  uploaded as `e2e-artifacts-stale-chassis-<run id>-<attempt>`.
- **`drain-hitless`** (`needs: stale-chassis`) — same shape, but
  executes `test/e2e/scenarios/drain-hitless.sh`. The job sets
  `E2E_DRAIN_ON_SHUTDOWN=true` at job level, so both its own
  `make e2e-up` and the scenario's mid-run recycle deploy gateways
  whose agent drains on SIGTERM — no other job arms the drain (see
  the [Drain-hitless](#drain-hitless) section for why the flag is not
  in the shared `gwnode-config.yaml`). The job points the
  scenario's `ARTIFACTS_DIR` at the same artifact root so the
  per-arm ping output, the ICMP sequence dump, the BGP/SB transition
  timeline, the drain log capture, the Port_Binding before/after
  snapshots and the loss-summary file are bundled with the lab-state
  dump. The scenario recycles the lab itself between the two arms via
  `make e2e-down && make e2e-up`; the job sets `RECYCLE_ON_EXIT=0` so
  the scenario's EXIT trap does not rebuild the lab a third time (and
  does not destroy the failed lab before the collect step runs). On
  failure the artifact bundle is uploaded as
  `e2e-artifacts-drain-hitless-<run id>-<attempt>`.

Every job except `drain-hitless` is capped at 15 minutes — matching
the budgets in issues
[#45](https://github.com/osism/ovn-network-agent/issues/45),
[#105](https://github.com/osism/ovn-network-agent/issues/105),
[#108](https://github.com/osism/ovn-network-agent/issues/108),
[#109](https://github.com/osism/ovn-network-agent/issues/109), and
[#111](https://github.com/osism/ovn-network-agent/issues/111).
[`drain-hitless`](https://github.com/osism/ovn-network-agent/issues/113)
is capped at 25: it deploys the lab twice and runs `baseline.sh` twice
(sanity gate, then the gate on the recycled lab), so its green path is
roughly twice the baseline job's. The cap is a backstop — a
`timeout-minutes` cancellation is not a failure, so it skips the
`if: failure()` artifact steps and leaves nothing to triage.

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
  frr/upstream-running-config.txt  — upstream `vtysh -c "show running-config"`
  frr/upstream-show-daemons.txt    — upstream `vtysh -c "show daemons"`
  frr/upstream-bgp-summary.txt     — upstream `vtysh -c "show bgp summary"`
  frr/upstream-daemons-file.txt    — upstream `/etc/frr/daemons`
  frr/upstream-processes.txt       — upstream process list (`ps`)
  frr/upstream-frr-log.txt         — upstream `/var/log/frr/*` tail
  kernel/<gateway>-ip-route.txt    — `ip route show table all`
  agent/<gateway>.log              — copy of the gateway container's stdout
  ovn-controller/<gateway>.log     — gateway `ovn-controller` log (chassis-registration daemon)
  hairpin/hairpin-flows-before.txt — `cookie=0x998` flows on master:br-ex before adding FIP_B (hairpin only)
  hairpin/hairpin-flows-after.txt  — `cookie=0x998` flows on master:br-ex after adding FIP_B (hairpin only)
  pf-external/pf-backend.log       — per-connection source-IP log from the workload-side HTTP responder (pf-external only)
  stale-chassis/nb-before-kill.txt — NB rows tagged for the killed chassis pre-kill (stale-chassis only)
  stale-chassis/nb-after-kill.txt  — NB rows still tagged for the killed chassis after the cleanup deadline (stale-chassis only)
  stale-chassis/peer-cleanup.log   — surviving peer's `stale chassis route removed` line (stale-chassis only)
  drain-hitless/graceful-ping.txt                — ping output from the SIGTERM/graceful arm (drain-hitless only)
  drain-hitless/graceful-icmp-seq.txt            — tcpdump of the probe on client-1 eth1, epoch-stamped (drain-hitless only)
  drain-hitless/graceful-transition-timeline.txt — per-second SB chassis + upstream BGP nexthop across the drain (drain-hitless only)
  drain-hitless/graceful-drain-log.txt           — `drain: gateway chassis priority lowered` + the terminal `drain:` line (`complete`, `failed` or `timeout exceeded`) from gateway-1 (drain-hitless only)
  drain-hitless/graceful-port-binding-before.txt — cr-lr0-public Port_Binding snapshot before the graceful kill (drain-hitless only)
  drain-hitless/graceful-port-binding-after.txt  — cr-lr0-public Port_Binding snapshot after the graceful kill (drain-hitless only)
  drain-hitless/hardkill-ping.txt                — ping output from the SIGKILL/control arm (drain-hitless only)
  drain-hitless/hardkill-icmp-seq.txt            — tcpdump of the probe on client-1 eth1, epoch-stamped (drain-hitless only)
  drain-hitless/hardkill-transition-timeline.txt — per-second SB chassis + upstream BGP nexthop across the hard kill (drain-hitless only)
  drain-hitless/hardkill-port-binding-before.txt — cr-lr0-public Port_Binding snapshot before the hard kill (drain-hitless only)
  drain-hitless/hardkill-port-binding-after.txt  — cr-lr0-public Port_Binding snapshot after the hard kill (drain-hitless only)
  drain-hitless/summary.txt                      — both loss counts, the configured threshold, the hitless gain, and the budget's headroom over the control arm (drain-hitless only)
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

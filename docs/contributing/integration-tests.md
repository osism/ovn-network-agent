# Integration tests

Integration tests exercise the agent against a real OVN/OVS/FRR/nftables
stack on a single Linux host. They live behind the `integration` build tag
so the default `go test ./...` run does not pick them up.

## Layout

```
test/integration/
  README.md                                   — stub linking to this page
  setup.sh                                    — apt + service bootstrap (run once per host)
  smoke_test.go                               — connect / initial-reconcile / clean-shutdown smoke test
  scenarios_helpers_test.go                   — shared per-scenario boilerplate (startScenario, readyAgent)
  scenario_fip_test.go                        — FIP add/remove, gatewayless gw, multi-router on one chassis
  scenario_failover_test.go                   — failover, stale-chassis cleanup (incl. multi-stale + one-stale-one-returning), drain & restore-drained
  scenario_reconnect_test.go                  — OVN pause/resume resilience + reconnect through the 30s inactivity probe (#64, #160)
  scenario_drain_edges_test.go                — drain edge cases (timeout, no local routers, cleanup_on_shutdown=false, stuck NB write)
  scenario_drift_test.go                      — periodic-reconcile + verifyRoutes drift recovery (#55)
  scenario_network_cidrs_test.go              — manual network_cidr override vs. auto-discovery, empty-filter sweep
  scenario_gateway_port_test.go               — legacy single-router gateway_port filter (#62)
  scenario_nat_types_test.go                  — `snat` vs `dnat_and_snat` rows + distributed `external_mac` (#62)
  scenario_port_forward_test.go               — DNAT, sticky multi-backend, VIP mgmt, masquerade, hairpin
  scenario_prefix_list_test.go                — ReconcileFRRPrefixList lifecycle on real FRR (#58)
  scenario_bridge_ip_test.go                  — cold-start bridge IP + proxy_arp housekeeping (#63)
  scenario_vethleak_test.go                   — SetupVethLeak / ReconcileVethLeakNetworks / TeardownVethLeak (#56) + veth-default ingress exception rule (#265)
  scenario_ipv6_test.go                       — IPv6-capable OVS flow plumbing on br-ex (#54; kernel/FRR paths are v4-only today)
  scenario_metrics_test.go                    — Prometheus /metrics scrapes (reconcile counter, drain outcome, stale-chassis, desired-state gauges)
  scenario_multi_segment_test.go              — multi-VLAN localnet segments: per-segment interfaces/routes/flows, failover isolation, same-segment hairpin, restart re-adoption (#147)
  scenario_failure_injection_test.go          — mid-cycle vtysh/nft/ovs-ofctl failures + self-heal (#88 item 1)
  scenario_route_tables_test.go               — non-overlapping route_table_id / port_forward_table_id / veth_leak_table_id (#88 item 2)
  scenario_cleanup_no_drain_test.go           — RemoveManagedNBEntries with drain_on_shutdown=false (#88 item 3)
  scenario_router_masquerade_ordering_test.go — router_masquerade configured before SNAT NAT exists (#88 item 4)
  scenario_same_batch_fip_test.go             — single OVSDB transaction adds + removes FIPs (#88 item 5)
  scenario_partial_failure_retry_test.go      — FRR write fails, kernel untouched, only FRR re-added (#88 item 6)
  scenario_ovn_ssl_test.go                    — mutual-TLS NB/SB connect against dedicated pssl ovsdb-servers + untrusted-CA rejection + bad-cert-path startup failure (#164)
  testenv/                                    — Setup, Teardown, RunAgent, MakeLocalRouter, Assert*, ScrapeMetrics, WithFailingTool, StartDrainRebind, StopOVNDatabases, StopNBDatabase, …
```

The failure-injection scenarios all share the `TestScenario_FailureInjection_`
prefix so they can be targeted as a group, e.g. for the flake-resistance
check from issue #88:

```sh
sudo OVN_AGENT_BINARY=$PWD/ovn-network-agent \
    go test -tags=integration -v -count=10 \
            -run TestScenario_FailureInjection_ \
            ./test/integration/...
```

Port-forward scenarios additionally rely on:

- a `loopback1` dummy device enslaved to `vrf-provider` (created by setup.sh)
- the `nft -j list ruleset` JSON output for stable, structured assertions
  (parsers live in `testenv/nftjson.go`)
- save/restore of the global `udp_l3mdev_accept` / `tcp_l3mdev_accept`
  sysctls when a scenario sets `port_forward_l3mdev_accept: true`

Scenario tests pause `ovn-northd` for the duration of each test (via
`testenv.PauseOVNNorthd`) so the test driver can write SB Port_Binding rows
directly without northd garbage-collecting them. `ovn-controller` keeps
running. `setup.sh` is unchanged from the harness foundation.

The multi-segment scenarios use `testenv.LocalRouterOpts.SegmentOpts` to
seed the SB rows of a router's localnet segment (external-switch
`Datapath_Binding`, `localnet` and peer patch `Port_Binding`) and
`testenv.EnsureSegmentPatchPort` to create the tagged provider-bridge patch
port (`external_ids:ovn-localnet-port`) that ovn-controller would create in
a real deployment.

## Local prerequisites

- Linux host (Ubuntu 24.04 recommended; the harness skips on macOS / Windows).
- Root privileges (`CAP_NET_ADMIN` is required for netlink operations in
  `routing_linux.go`).
- Go toolchain matching `go.mod`.
- A host you do not mind mutating: `setup.sh` installs packages, creates
  `br-ex`, configures FRR with a `vrf-provider` VRF, and so on.

## Running locally

```sh
sudo ./test/integration/setup.sh    # one-time, installs packages and starts services
make test-integration                # builds the binary and runs the tagged tests
```

`make test-integration` builds `./ovn-network-agent` first, then exports
`OVN_AGENT_BINARY=$PWD/ovn-network-agent` and invokes
`go test -tags=integration -v -count=1 ./test/integration/...`.

To point the harness at a binary built elsewhere:

```sh
sudo OVN_AGENT_BINARY=/path/to/ovn-network-agent \
    go test -tags=integration -v -count=1 ./test/integration/...
```

The tests skip (rather than fail) when:

- not running on Linux, or
- not running as root, or
- `br-ex`, `nft`, `ovs-vsctl`, `ovs-ofctl`, or `vtysh` is missing.

Run `setup.sh` if you see skip messages for the bridge or binaries.

## Adding new tests

1. Create a `*_test.go` file in `test/integration/` with the build tag:
   ```go
   //go:build integration

   package integration
   ```
2. Call `testenv.Setup(t)` first; it registers a cleanup that scrubs leftover
   nft tables / kernel routes / FRR static routes between tests.
3. Drive the agent via `testenv.RunAgent(t, cfg)` and verify outcomes with
   the `Assert*` helpers (`AssertKernelRoute`, `AssertOVSFlow`,
   `AssertFRRRoute`, `AssertNftRule`).
4. Always call `proc.Stop(timeout)` — if the test forgets, `t.Cleanup`
   SIGKILLs the process, but state cleanup may be incomplete.

## CI

`.github/workflows/integration.yml` runs the same flow on `ubuntu-latest`:
`setup.sh` → `make build-integration` → `go test -tags=integration ...`
under sudo, then a coverage-report step.

## Coverage instrumentation

CI builds the agent with `make build-integration`
(`go build -cover -covermode=atomic -race`) so integration-only Linux code
(`main()`, `Connect()`, `routing_linux.go`, `nftables_linux.go`) is counted
and the concurrency-heavy paths run under the race detector. Each agent
process writes per-process counter files to the directory named by
`GOCOVERDIR`, which `testenv.RunAgent` propagates to the subprocess; only
agents that exit cleanly on SIGTERM emit their counters (a SIGKILL loses
them, which is acceptable best-effort).

To collect and read the merged figure locally:

```sh
make build-integration
mkdir -p covdata
sudo OVN_AGENT_BINARY=$PWD/ovn-network-agent GOCOVERDIR=$PWD/covdata \
    go test -tags=integration -count=1 -timeout 25m ./test/integration/...
go tool covdata percent -i covdata
```

The published figure is informational; the enforced unit-coverage floor
lives in `test.yml` (see the CI docs for why it is tracked separately).

# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

This file was started on März 19, 2026. Changes prior to this date are not included in the CHANGELOG.

## [v0.9.0] - 2026-10-22

### Changed
- Print every chaos violation to the job log below the summary line so a failed run stays triageable after its artifact expires (osism/ovn-network-agent#314)
- Keep a failed or cancelled chaos job's artifacts for 30 days instead of 7 (osism/ovn-network-agent#315)

### Fixed
- Write the upstream bgpd's log to /var/log/frr/bgpd.log and enable BGP neighbor-change logging so chaos artifacts capture upstream BGP session history (osism/ovn-network-agent#316)
- Report the error when a test's Run() call returns prematurely instead of discarding it (osism/ovn-network-agent#317)

### Dependencies
- ossf/scorecard-action v2.4.4 → v2.5.0 (osism/ovn-network-agent#308)
- golangci-lint v2.14.0 → v2.15.0 (osism/ovn-network-agent#317)
- golang.org/x/crypto v0.54.0 → v0.57.0 (osism/ovn-network-agent#317)
- golang.org/x/net v0.57.0 → v0.60.0 (osism/ovn-network-agent#317)
- golang.org/x/text v0.40.0 → v0.42.0 (osism/ovn-network-agent#317)
- github.com/prometheus/client_golang v1.24.1 → v1.25.0 (osism/ovn-network-agent#317)
- actions/setup-node, actions/upload-artifact, actions/download-artifact, orhun/git-cliff-action, github/codeql-action pinned digests refreshed (osism/ovn-network-agent#317)

## [v0.8.0] - 2026-10-04

### Added
- Journal a veth re-creation that had to wait for, delete, or retry past the previous incarnation's stale upstream end during a chaos restore or undo (osism/ovn-network-agent#278)
- Collect upstream `ip -d addr`/`ip -d link` state in E2E test artifact bundles to help diagnose veth re-creation issues (osism/ovn-network-agent#278)
- Report `down_ms` and `down_windows` per probe on the chaos run recovery record and converged journal event (osism/ovn-network-agent#285)
- Gwnode E2E image gains a compute role (`GWNODE_ROLE=compute`) and the lab a `compute-1` chassis, so chaos workloads can run off the gateways (osism/ovn-network-agent#286)
- Add a `route_watch` option (on by default) that reconciles at once when a kernel route the agent owns is deleted or replaced from outside, classifying the drift as a kernel or FRR route change and exposing it via the new `route_drift_total` metric (osism/ovn-network-agent#289)
- Add test coverage for the route watch's kernel/FRR route repair, foreign-route-replace handling, and exclusion of the agent's own route removals from drift counting (osism/ovn-network-agent#289)
- Track route drift (`route_drift_total`) deltas across the E2E chaos runner's route-drop actions and list them in a new report table (osism/ovn-network-agent#289)
- Add a Fault traces report section listing chassisredirect owner and upstream BGP path changes around each traced double-failover, with docs on the returned gateway port and the last-chassis BFD guard (osism/ovn-network-agent#290)
- Add an `ovs_flow_watch` option (enabled by default) that watches the provider bridge for deleted OVS hairpin and MAC-tweak flows owned by the agent and reconciles immediately instead of waiting for the periodic reconcile, falling back to the periodic reconcile while the watch is down, with a new `ovn_network_agent_ovs_flow_drift_total` metric counting those deletions per flow plane, integration and chaos-test coverage, and documentation (osism/ovn-network-agent#296)
- Gate E2E bring-up on the upstream zebra process, retrying its start before the FRR setup so a zebra crash at container start no longer leaves the lab dead on arrival (osism/ovn-network-agent#298)
- Add the `frr_watch` option (on by default) that detects an FRR restart by polling `vtysh -c "show modules"` for daemon process ID changes and reconciles once the daemons have settled, repairing static routes and prefix-list entries that are not saved to FRR's running configuration (osism/ovn-network-agent#299)
- Add an `ovn_network_agent_frr_restarts_total` metric counting the FRR restarts the FRR watch detects (osism/ovn-network-agent#299)
- Log the reconcile trigger (`startup`, `periodic`, or `event`) on the reconciling log line (osism/ovn-network-agent#299)

### Changed
- Chaos E2E recoveries table shows summed probe loss per recovery, and `down_ms`/`down_windows` are derived from the journal for older run records lacking them, with the chaos-analysis docs and baseline updated to match (osism/ovn-network-agent#285)
- Chaos E2E planned-restart summary counts every drained restart instead of exempting the workload host, since workloads no longer run on the gateways (osism/ovn-network-agent#286)
- Move chaos E2E test workloads to a dedicated compute chassis so a lifecycle fault no longer darkens every probe and masks failover measurements (osism/ovn-network-agent#286)
- Add a standby gateway chassis for the VLAN routers and the cross-chassis router in chaos E2E tests, and probe every FIP path in the drain-everywhere profile (osism/ovn-network-agent#287)
- E2E chaos harness now follows the CR owner of `cr-lr0-public` during fault injection and the hold via a one-second poll instead of re-pointing the port-forward VIP's routes only after restore, journaling each re-point with its phase and listing them in a new "VIP re-points" report section; the chaos-analysis skill's baseline table is now marked as predating this change (osism/ovn-network-agent#288)
- Lower the recovery budget of the `kernel-route-drop` and `frr-route-drop` chaos actions from 60s to 10s, since the route watch repairs them independently of the reconcile cadence (osism/ovn-network-agent#289)
- Document the route watch, its `route_watch` option, the `route_drift_total` metric, and the chaos report's route drift table (osism/ovn-network-agent#289)
- Return the local chassisredirect port names from OVN SB state instead of just a count (osism/ovn-network-agent#290)
- Log a gateway port that is bound to the local chassis again while the drain handshake waits for a takeover, and list such ports in the drain-timeout warning (osism/ovn-network-agent#290)
- Chaos E2E tests: trace chassisredirect port owners and the upstream's selected BGP paths around double-failover faults and journal every change (osism/ovn-network-agent#290)
- Rework chaos-analysis to rank open packet loss by fault action and phase, split out loss the lab causes by design, and add `--timeline` and `--baseline` comparison options (osism/ovn-network-agent#294)
- Extract the subscribe/debounce/rate-limit/backoff loop shared by drift watches into a reusable drift watcher engine, so the route watch and future watches no longer duplicate it (osism/ovn-network-agent#296)
- Let test/e2e/bootstrap.sh be sourced without running the lab bring-up, so its functions can be tested in isolation (osism/ovn-network-agent#298)
- Collect the chaos lab-state bundle after every run instead of only on failure, so a passing run's agent logs are available to attribute a recovery that stayed within its budget (osism/ovn-network-agent#299)

### Fixed
- Reset the consecutive re-add streak on a reconcile cycle that changes no routes, instead of leaving the `ovn_network_agent_consecutive_readds` gauge frozen at a stale non-zero value until the next route change (osism/ovn-network-agent#277)
- Tolerate the previous incarnation's stale upstream veth end when re-creating the underlay during an E2E chaos restore, waiting for and deleting it or retrying the create instead of failing the restore with `file exists` (osism/ovn-network-agent#278)
- Correct chaos run downtime measurement to sum every red window per probe target after the fault inject instead of dating the outage from inject to its last recovery, which overstated a short failover followed by a late blink as one long outage (osism/ovn-network-agent#285)
- Wait for two consecutive probe samples confirmed green since the restore before declaring a chaos-test fault converged, fixing recovery budgets that asserted nothing because convergence was checked before a probe could register loss (osism/ovn-network-agent#295)
- Stop counting baseline sweep failures as errors when they occur under a held chaos fault that stalls what the check reads (e.g. `sb-pause` stalling the dual-claim lookup); these are now tracked separately as skipped checks and surfaced in the run summary, report header, and chaos-analysis output (osism/ovn-network-agent#300)

## [v0.7.0] - 2026-09-28

### Added
- Discover the localnet segment (physnet and VLAN tag) of each locally-active router's external network and attach it to router and NAT state, extending the monitor-cache consistency guard to the new inputs (osism/ovn-network-agent#148)
- Support per-localnet-segment OVS MAC-tweak and hairpin flows, enabling multiple VLAN/flat provider segments on one bridge (osism/ovn-network-agent#148)
- Add localnet_segments metrics gauge reporting the number of locally-active router segments (osism/ovn-network-agent#148)
- Add test coverage and documentation for multi-network VLAN provider network segments (osism/ovn-network-agent#148)
- Add graceful drain vs hard-kill E2E scenario with CI job and documentation (osism/ovn-network-agent#153)
- Retry the upstream bgpd start within a bounded budget during E2E bring-up instead of failing on a one-off startup hiccup (osism/ovn-network-agent#170)
- Dump the awaited daemon's state (upstream FRR, OVN NB, SB chassis registration) to the job log when an E2E bring-up readiness gate times out (osism/ovn-network-agent#170)
- Collect upstream FRR daemon state and each gateway's ovn-controller log in E2E test artifacts (osism/ovn-network-agent#170)
- Restart the service automatically on package upgrade via opt-in `OVN_NETWORK_RESTART_ON_UPGRADE`, with systemd calls guarded for chroots and containers (osism/ovn-network-agent#171)
- Warn at startup when `vtysh` is not found on PATH, since FRR is now a soft dependency (osism/ovn-network-agent#171)
- Add a CI smoke test driving the `.deb` package install/upgrade/remove/purge cycle on every PR (osism/ovn-network-agent#171)
- Add a rolling upgrade guide, CONTRIBUTING.md, a bug-report issue form, and a pull request template (osism/ovn-network-agent#171)
- Build the documentation site on pull requests to catch dead links before merge (osism/ovn-network-agent#172)
- Add `--check-config` flag to validate configuration and exit, for pre-restart validation before restarting a live gateway node (osism/ovn-network-agent#168)
- Add an `ovn_connection_state` gauge that mirrors live OVSDB connection status for the SB and NB clients (osism/ovn-network-agent#173)
- Add integration test coverage for OVSDB reconnection after a long outage and for a stuck NB write during drain (osism/ovn-network-agent#173)
- Seeded chaos runner for the E2E lab with a replayable fault sequence, probing, baseline invariant checks and recovery tracking, plus an `e2e-chaos` make target to drive it (osism/ovn-network-agent#181)
- Document the E2E chaos runner, covering inputs, the replay contract, and fault actions (osism/ovn-network-agent#181)
- Add chaos configuration profiles with a per-gateway agent-config overlay for the chaos runner (osism/ovn-network-agent#188)
- Apply a chaos profile to the running lab per gateway, validating each gateway's rendered agent configuration before any live file is swapped and rolling gateways one at a time (osism/ovn-network-agent#188)
- Add a config-flip chaos action that rewrites a gateway's configuration mid-run and restarts it onto the change, and document the chaos configuration profiles and flip whitelist (osism/ovn-network-agent#188)
- Add chaos-testing support for control-plane, upstream and network-impairment fault scenarios, including database/northd pause, double-failover, and management-path loss/delay actions (osism/ovn-network-agent#190)
- Add data-plane drift, routing-flap, and OVN churn chaos actions to the e2e chaos runner, with catalog documentation (osism/ovn-network-agent#190)
- Expose the agent metrics endpoint in the e2e gwnode config and record settle inputs and journal offsets in the chaos run record for the expected-state oracle (osism/ovn-network-agent#191)
- Add config-aware expected-state oracle for E2E chaos tests, computing per-gateway desired state from OVN snapshots and configuration (osism/ovn-network-agent#191)
- Add config-aware oracle and observation layer to the E2E chaos test suite for verifying gateway state across OVN, kernel, FRR, OVS, and nftables data planes (osism/ovn-network-agent#191)
- Add a config-aware settle-window oracle to the chaos E2E runner that verifies kernel routes, FRR statics, prefix-lists, hairpin/MAC-tweak flows, nftables rules, managed VIPs and upstream BGP announcements converge to their expected state between faults (osism/ovn-network-agent#191)
- Add `-report` to render a recorded E2E chaos run as a Markdown report with verdict, fault histogram, recoveries, probe loss and skipped decisions (osism/ovn-network-agent#197)
- Add `ovn_network_agent_failover_announce_seconds` histogram measuring failover announce latency from chassisredirect observation to BGP announce (osism/ovn-network-agent#198)
- Add a strict failover E2E variant (`make e2e-failover-strict`) that fails if the data-plane outage during re-election exceeds a ~2s budget (osism/ovn-network-agent#198)
- Export a `build_info` gauge labelled with the running agent version for fleet version-skew visibility (osism/ovn-network-agent#200)
- Serve a `/readyz` endpoint reporting OVN connectivity and reconcile health, distinguishing a functional agent from a merely alive process via the existing liveness-only `/healthz` (osism/ovn-network-agent#200)
- Ship ready-to-load Prometheus alert rules (`contrib/prometheus-rules.yaml`) matching the metrics reference, and add an operator troubleshooting runbook with cause-diagnosis-remediation for each alert, the drain and stale-cleanup outcomes, and the `/healthz`/`/readyz` probes (osism/ovn-network-agent#200)
- Add TLS support via ovn_ssl_ca/ovn_ssl_cert/ovn_ssl_key config options for ssl: OVN NB/SB remotes, including mutual TLS (osism/ovn-network-agent#202)
- Add chaos-analysis skill to aggregate packet loss and downtime across E2E Chaos runs (osism/ovn-network-agent#240)
- Add `hairpin_flows_desired`, `hairpin_flows_installed`, and `ovs_flow_apply_errors_total` metrics for the OVS hairpin flow plane, and alert when installed flows trail the desired state (osism/ovn-network-agent#246)
- Add E2E scenario asserting zero packet loss on the same-chassis hairpin path under continuous OVN flow-plane reconcile churn (osism/ovn-network-agent#246)
- Add node/netns probe vantages to the E2E chaos harness and measure the same-node hairpin FIP and VIP paths (osism/ovn-network-agent#246)
- Detect when a provider VRF has no default route, exposing `vrf_default_route_present` and `announced_vips` metrics, a Prometheus alert, troubleshooting runbook, and a dedicated chaos E2E verification plane (osism/ovn-network-agent#249)
- Add E2E test scenario covering a port-forward VIP whose gateway port lives on another chassis (osism/ovn-network-agent#249)
- Add script to dispatch ad-hoc E2E Chaos runs with a random seed and profile (osism/ovn-network-agent#251)
- Add integration and E2E test coverage for the veth-default ingress exception rule, including a lifecycle/drift-recovery test, a forwarding-loop regression check, a new cross-chassis FIP-to-FIP E2E scenario, and a matching chaos-test topology layer and probe (osism/ovn-network-agent#266)
- Reload the configuration on SIGHUP, applying reloadable keys (log level, reconcile interval, FRR prefix list, stale chassis grace period, port forwards, cleanup/drain-on-shutdown settings, drain timeout, drain settle delay, OVN client certificate and key) without restarting the agent, and add a `config_reload_total` metric and an `ExecReload` systemd directive (osism/ovn-network-agent#272)

### Changed
- Require Go 1.26.5 to build the agent (osism/ovn-network-agent#137, osism/ovn-network-agent#149)
- Route FIP and SNAT /32 kernel routes over each VLAN segment's own interface instead of the shared bridge device (osism/ovn-network-agent#148)
- Write per-router static MAC bindings using each router's own segment interface MAC, fixing conflicts when multiple agents serve different bridges (osism/ovn-network-agent#148)
- Surface NB cache and server-side Gateway_Chassis contents in the drain empty-match log for triage (osism/ovn-network-agent#153)
- Calibrate the E2E drain-hitless scenario to a measured outage-duration budget, measure failover RTO from the kill instant, gate the recycled lab on a baseline check before the control arm, and record BGP/SB transition-timeline and ICMP capture artifacts (osism/ovn-network-agent#153)
- Skip the drain-hitless scenario's lab recycle on CI exit to avoid extra deploy cost and preserve failure artifacts for collection (osism/ovn-network-agent#153)
- Sync E2E test documentation with the drain-hitless scenario's updated behavior (osism/ovn-network-agent#153)
- Run only necessary CI jobs after merging to main, dropping redundant push-triggered lint, test, integration, build, and E2E jobs, and remove the redundant cross-compile build job (osism/ovn-network-agent#155)
- Wake the gateway drain migration wait from chassisredirect SB events instead of relying on the fixed poll interval (osism/ovn-network-agent#156)
- Replace the fixed drain settle delay with an event-driven handshake that waits for the takeover chassis to signal readiness, and drop the default settle delay from 3s to 500ms since it now only covers the final safety margin after the signal (osism/ovn-network-agent#156)
- Sync the shipped config samples (`ovn-network-agent.default`, `ovn-network-agent.yaml.sample`) with the code and add a docgen check so they cannot drift again (osism/ovn-network-agent#168)
- Install the binary and systemd unit `ExecStart` to `/usr/bin` instead of `/usr/local/bin`, single-sourcing the packaged unit (osism/ovn-network-agent#171)
- Change the systemd unit's FRR dependency from `Requires=` to `Wants=` and the package dependency from `depends` to `recommends`, so FRR restarts or upgrades no longer drain the agent (osism/ovn-network-agent#171)
- Consolidate changelog generation into git-cliff with a cumulative CHANGELOG.md, replacing the unused label-based release notes config (osism/ovn-network-agent#171)
- Cancel superseded CI runs on a force-push via per-workflow concurrency groups (osism/ovn-network-agent#172)
- Run the unit test suite on push to main and on a weekly schedule, in addition to pull requests (osism/ovn-network-agent#172)
- Gate release artifact builds on the unit test suite passing first (osism/ovn-network-agent#172)
- Move govulncheck into its own workflow with push-to-main and weekly runs (osism/ovn-network-agent#172)
- Cross-compile for linux/arm64 on pull requests to catch arch-specific build failures earlier (osism/ovn-network-agent#172)
- Commit a curated .golangci.yml enabling gosec, errorlint, gocritic, and revive (osism/ovn-network-agent#172)
- Pin containerlab to a verified release and hoist E2E lab setup into a shared composite action (osism/ovn-network-agent#172)
- Skip integration and E2E jobs on docs-only pull requests (osism/ovn-network-agent#172)
- Pin the add-to-project reusable workflow to a commit SHA instead of a branch (osism/ovn-network-agent#172)
- Drop the unused pull-requests write permission from the dependency-review workflow (osism/ovn-network-agent#172)
- Commit the main branch ruleset as importable JSON and document the CI pipeline gates, required status checks, and maintenance procedures (osism/ovn-network-agent#172)
- Instrument the integration test binary with coverage and race detection, and update related documentation (osism/ovn-network-agent#173)
- Simplify internal NAT state tracking by removing the unused FIPs snapshot field (osism/ovn-network-agent#174)
- Refactor OVN state handling into a guarded snapshot holder and decompose the state refresh into pure, testable step helpers (osism/ovn-network-agent#174)
- Extract pure desired-state computation (hairpin targets, localnet segments, IPv4/dual-stack route filtering) out of the reconcile loop into testable functions (osism/ovn-network-agent#174)
- Split nftables ruleset generation into per-chain emitter functions, with the rendered ruleset unchanged (osism/ovn-network-agent#174)
- Simplify RouteManager by storing the agent Config directly instead of copying individual fields (osism/ovn-network-agent#174)
- Read FRR static routes and prefix-list entries via vtysh JSON output instead of parsing human-readable tables, preventing silent route loss if FRR changes its text formatting (osism/ovn-network-agent#174)
- Match netlink "route already removed" and "rule already removed" outcomes by errno (ESRCH/ENOENT) instead of error text (osism/ovn-network-agent#174)
- Probe nftables table existence with `nft -j list tables` before deleting it on teardown instead of matching the delete error text (osism/ovn-network-agent#174)
- Refactored configuration loading to drive CLI flags, environment variables, and the config file from a single option registry instead of six separately maintained definitions (osism/ovn-network-agent#174)
- Generate OVSDB models from the checked-in OVN schemas instead of hand-writing them, establishing OVN 24.09 as the minimum supported version, and document the supported OVN range and model regeneration procedure (osism/ovn-network-agent#174)
- Exclude the generated OVSDB models from linting (osism/ovn-network-agent#174)
- Re-derive the drain-hitless graceful-outage budget from a 20-run distribution and guard against a budget that can no longer distinguish a broken drain from a working one (osism/ovn-network-agent#183)
- Move the seeded E2E chaos session into its own dispatch-only workflow (osism/ovn-network-agent#187)
- Run the chaos E2E workflow nightly as a matrix across all six curated profiles, with workflow_dispatch replay and a PR `chaos-smoke` label, pinned to the profile registry to catch coverage drift (osism/ovn-network-agent#192)
- Reorder reconcile so the BGP announce (FRR route install and BGP soft-refresh) runs before the stability steps, reducing failover takeover latency, and add a failover-announce-latency metric with a documented alert (osism/ovn-network-agent#198)
- Consolidate known limitations (IPv4-only routing plane, no config reload, port-forward hashing, single provider bridge) into one section on the docs home page and README, and add a fleet-skew build_info PromQL query to the upgrade guide (osism/ovn-network-agent#200)
- Require the configured OVS wrapper to forward stdin (e.g. `docker exec -i`) since flow programming is now batched over stdin (osism/ovn-network-agent#245)
- Extract the FIP_B hairpin E2E test topology into a shared library for reuse by other hairpin test scenarios (osism/ovn-network-agent#246)
- Run the E2E Chaos nightly and dispatch workflows on self-hosted runners (osism/ovn-network-agent#252)
- Pre-pull E2E base images with retries before docker build so a single flaky registry connection no longer aborts the lab bring-up (osism/ovn-network-agent#269)
- Documented the SIGHUP configuration reload, covering reloadable keys, TLS client certificate rotation, and FRR prefix-list renaming on a running agent (osism/ovn-network-agent#272)
- Chaos e2e config-flip now reloads a gateway in place via SIGHUP when the running agent supports it, falling back to a restart otherwise, and journals the drain state of planned restarts with a new "Planned restarts" report section (osism/ovn-network-agent#273)
- Added the `drain-everywhere` chaos e2e profile, draining every gateway and probing only paths with a standby chassis (osism/ovn-network-agent#273)

### Fixed
- Fix project-board automation failing on fork PRs due to missing secrets access (osism/ovn-network-agent#142, contributed by @ideaship)
- Raise the E2E reachability reconcile window from 30s to 60s to de-flake baseline, hairpin and pf-hairpin scenarios on cold CI runners (osism/ovn-network-agent#145)
- Allow OVN_NETWORK_DRAIN_ON_SHUTDOWN to enable draining, not just disable it (osism/ovn-network-agent#153)
- Correct the drain-hitless scenario's delta assertion failure message to require a strictly positive packet-loss delta (osism/ovn-network-agent#153)
- Fix VitePress build failure by upgrading to VitePress v2 for Vite 8 compatibility (osism/ovn-network-agent#155)
- Drop malformed or non-IPv4 NAT, LRP, and SB gateway addresses before they reach the route, FRR, and BGP announce path so a single invalid OVN row no longer blocks IPv4 FIP announcement (osism/ovn-network-agent#167)
- Skip invalid IPs and failed vtysh or OVS-flow batches individually instead of aborting the whole FRR route or OVS hairpin-flow reconcile (osism/ovn-network-agent#167)
- Scope kernel and FRR route reconciliation to agent-owned routes via a route protocol tag and the agent's veth nexthop instead of CIDR-based ownership, so operator-created routes are no longer removed (osism/ovn-network-agent#167)
- Reject a non-positive reconcile-interval during config validation instead of panicking the running agent (osism/ovn-network-agent#168)
- Fail fast with a clear error naming the offending setting on unparsable duration/integer config values, unknown config file keys, and boolean environment variables previously honored in only one direction, instead of silently falling back to defaults (osism/ovn-network-agent#168)
- Include external_ids in the Port_Binding cache drift guard so dropped updates to the NAT filter are no longer missed (osism/ovn-network-agent#174)
- Surface per-router gateway routing failures instead of silently returning success (osism/ovn-network-agent#174)
- Normalize chassis hostname matching so FQDN chassis hostnames are correctly recognized as local (osism/ovn-network-agent#174)
- Wait for the refresh loop to stop before the shutdown-time state refresh to prevent concurrent state writes (osism/ovn-network-agent#174)
- Resolve chassisredirect ports to their logical router port via options:distributed-port instead of parsing the port name, preventing routers from being dropped from the local set (osism/ovn-network-agent#174)
- Fix anti-flapping priority boost being silently skipped for Gateway_Chassis rows that don't follow the `{lrp}_{chassis}` naming convention, by joining them to their LRP via the gateway_chassis reference column (osism/ovn-network-agent#174)
- Resolve the OVN Southbound remote hostname to a numeric IP before starting ovn-controller in the E2E test harness, avoiding DNS resolution failures (osism/ovn-network-agent#174)
- Prevent a daemon-start race from restarting gateway containers and corrupting the e2e lab (osism/ovn-network-agent#186)
- Fix the chaos test responder reset killing its own shell when resetting the pf-backend process (osism/ovn-network-agent#188)
- Background the chaos frr-restart fault injection so a slow FRR recycle on a loaded CI runner no longer times out and falsely fails a run (osism/ovn-network-agent#193)
- Fixed a race condition in E2E lab bring-up where premature SB remote name resolution could crash the gwnode entrypoint and leave the lab unrecoverable, and pinned lab container images by digest for reproducibility (osism/ovn-network-agent#194)
- Fixed the flaky E2E chaos frr-restart test by widening the FRR recycle wait budget and reaping zombie FRR daemons with tini as PID 1 in the gwnode container (osism/ovn-network-agent#195, osism/ovn-network-agent#196)
- Fix CI integration test bootstrap to actually start bgpd, which was silently left down and caused every BGP soft-refresh to fail (osism/ovn-network-agent#198)
- Record drain_total{outcome="noop"} instead of misreporting a no-op gateway drain as completed (osism/ovn-network-agent#200)
- Re-create the workload network namespace when its anchor is left behind by a container restart, instead of treating a dead anchor as a live namespace (osism/ovn-network-agent#209)
- Keep a booting gateway out of the chaos run's healthy pool by checking the agent process directly instead of relying on container health alone (osism/ovn-network-agent#210)
- Drop the accidentally committed chaos runner binary (osism/ovn-network-agent@e246cdc)
- Leave a port-forward VIP dormant on a gateway without local routers instead of installing a route that can never be advertised (osism/ovn-network-agent#211)
- Retry the upstream next-hop MAC binding write in the E2E bootstrap against a racing gateway agent (osism/ovn-network-agent#212)
- Pin the E2E lab's management addresses so a restart cannot swap encap IPs between gateways (osism/ovn-network-agent#213)
- Repair the FIP next-hop route when zebra never learns the veth connected route, so routes can resolve and be advertised via BGP (osism/ovn-network-agent#215)
- Clean stale FRR runtime state and retry the FRR startup so a gateway node survives a container restart (osism/ovn-network-agent#219)
- Make the chaos test restore process survive a container reincarnating during underlay recovery (osism/ovn-network-agent#221)
- Seed the gwnode placeholder BGP configuration only when no real BGP configuration exists, preventing it from clobbering an established session (osism/ovn-network-agent#222)
- Fix port-forward VIPs never being advertised via BGP by announcing them through their connected route instead of a static route that was permanently shadowed by it, which also held the `ovn_network_agent_inactive_routes` metric non-zero and logged a spurious inactive-route error every cycle (osism/ovn-network-agent#224)
- Fix the heterogeneous E2E chaos profile's API VIP never going green by also announcing it from gateway-1, since the dormant-VIP gate left it unannounced on every gateway when pinned to gateway-2 alone (osism/ovn-network-agent#225)
- Give every announceable port-forward VIP its own exact /32 entry in the FRR prefix-list so VIPs outside every hosted network are no longer silently filtered (osism/ovn-network-agent#227)
- Quote the lb-vip-churn remove key so ovn-nbctl accepts it, and label chaos runs whose only violations are harness-caused as harness-fault instead of fail (osism/ovn-network-agent#230)
- Match the upstream bgpd process by full path so BusyBox pgrep/pkill correctly detect and control it during chaos tests (osism/ovn-network-agent#232)
- Guard the upstream vtysh -b reload to only run when an integrated frr.conf exists, avoiding a false failure on images without integrated config (osism/ovn-network-agent#233)
- Re-plumb the port-forward VIP routes on every master check instead of only on a master change, so a recycled owner regains its routes (osism/ovn-network-agent#234)
- Fix dropped packets during OVS hairpin and MAC-tweak flow reconciliation by replacing the full delete-and-readd sweep with a differential apply that only changes what is needed (osism/ovn-network-agent#245)
- Fix VRF default route detection to match routes by prefix instead of a nil destination (osism/ovn-network-agent#250)
- Restore drained gateway chassis priorities on startup regardless of the `drain_on_shutdown` setting (osism/ovn-network-agent#255)
- Repair bgpd's prefix-list copy when an FRR restart causes vtysh writes to miss it, which could permanently block a network's BGP announcements (osism/ovn-network-agent#257)
- Exempt veth-default ingress from the veth-leak source rule to stop a packet loop between cross-chassis FIP-to-FIP traffic on a shared provider network (osism/ovn-network-agent#266)
- Gateway drain no longer holds the shutdown for the full drain timeout on router ports that have no standby chassis to fail over to (osism/ovn-network-agent#273)
- Chaos E2E planned gateway restarts now use the production shutdown budget instead of `docker restart`, preventing draining agents from being killed prematurely during tests (osism/ovn-network-agent#273)

### Dependencies
- golang.org/x/crypto 0.41.0 → 0.53.0 (osism/ovn-network-agent#150)
- golang.org/x/net 0.43.0 → 0.55.0 (osism/ovn-network-agent#150)
- golang.org/x/sys 0.35.0 → 0.46.0 (osism/ovn-network-agent#150)
- golang.org/x/text 0.28.0 → 0.38.0 (osism/ovn-network-agent#150)
- vite 5.4.21 → 6.4.3 (osism/ovn-network-agent#150)
- vite 7 → 8 (osism/ovn-network-agent#151)
- actions/checkout v6 → v7 (osism/ovn-network-agent#139)
- actions/setup-go 4a36011 → 924ae3a (osism/ovn-network-agent#157)
- github/codeql-action 9e0d7b8 → 99df26d (osism/ovn-network-agent#157)
- golangci/golangci-lint-action 1e7e51e → ba0d7d2 (osism/ovn-network-agent#157)
- golang/govulncheck-action b625fbe → 032d455 (osism/ovn-network-agent#157)
- actions/attest-build-provenance a2bbfa2 → 0f67c3f (osism/ovn-network-agent#157)
- softprops/action-gh-release b430933 → 718ea10 (osism/ovn-network-agent#157)
- actions/setup-node v6 → v7 (osism/ovn-network-agent#199)
- actions/setup-go v6 → v7 (osism/ovn-network-agent#201)
- osism/.github b57ddf3 → 04f1ab2 (osism/ovn-network-agent#189)
- ossf/scorecard-action v2.4.3 → v2.4.4 (osism/ovn-network-agent#231)
- vitepress 2.0.0-alpha.18 → 2.0.0-alpha.19 (osism/ovn-network-agent#244)
- github/codeql-action 99df26d → 5595cca (osism/ovn-network-agent#135)
- softprops/action-gh-release 718ea10 → 3d0d988 (osism/ovn-network-agent#140)
- actions/checkout 9c091bb → 3d3c42e (osism/ovn-network-agent#220)
- golangci-lint 2.12.2 → 2.13.2 (osism/ovn-network-agent#260)
- vitepress 2.0.0-alpha.19 → 2.0.0-alpha.20 (osism/ovn-network-agent#267)
- actions/attest-build-provenance 0f67c3f → 4d10147 (osism/ovn-network-agent#268)
- actions/deploy-pages cd2ce8f → 368f825 (osism/ovn-network-agent#268)
- github/codeql-action 5595cca → b96794f (osism/ovn-network-agent#268)
- orhun/git-cliff-action f50e115 → 3d96a18 (osism/ovn-network-agent#268)
- softprops/action-gh-release 3d0d988 → efb3536 (osism/ovn-network-agent#268)
- github.com/prometheus/client_golang 1.23.2 → 1.24.1 (osism/ovn-network-agent#268)
- golangci/golangci-lint 2.13.2 → 2.14.0 (osism/ovn-network-agent#274)

## [v0.6.0] - 2026-05-16

### Added
- Add a port-forward-only operating mode: the agent can now run as a standalone VIP service without an OVN connection, with config validation for the OVN/port-forward matrix, OVN client and OVN-dependent steps gated accordingly, and documentation for the new mode (osism/ovn-network-agent#122)
- Add detection of FRR static routes that are configured but not advertised via BGP (e.g. due to an unresolvable next-hop), surfaced through the new `ovn_network_agent_inactive_routes` metric (osism/ovn-network-agent#123)
- Add a `drain_settle_delay` option that holds FIP route advertisement for a configurable period after a gateway drain completes, giving the takeover chassis time to come up before cleanup withdraws routes (osism/ovn-network-agent#126)

### Changed
- Trigger a BGP soft-refresh when FRR routes are added, not just removed, so a takeover chassis announces new FIPs immediately during failover instead of waiting on FRR's normal redistribution timing (osism/ovn-network-agent#126)
- Document the post-drain settle delay that keeps FIP routes advertised while the takeover gateway chassis comes up (osism/ovn-network-agent#126)
- Restructure the E2E harness documentation into navigable sections with an overview table and collapsible rationale details (osism/ovn-network-agent#133)

### Fixed
- Fix the agent losing track of FIPs and routers when the OVN monitor cache drops a table INSERT shortly after a fresh subscription, by cross-checking the cache against a direct server select and rebuilding from it on a mismatch (osism/ovn-network-agent#123)
- Fix the leaving gateway deleting the takeover chassis' default routes and static MAC bindings during shutdown by refreshing OVN state after the drain completes and before cleanup runs (osism/ovn-network-agent#123)
- Fix FIPs becoming unreachable after ovn-controller recreates the br-ex patch port by re-validating the cached OpenFlow port on every reconcile instead of caching it for the agent's lifetime (osism/ovn-network-agent#123)
- Fix slow resolution of Gateway_Chassis priority ties by waking the agent on Gateway_Chassis changes instead of waiting for the periodic reconcile (osism/ovn-network-agent#123)
- Fall back to a direct NB select when restoring drained gateways and the monitor cache is missing the local chassis's row, preventing a chassis from staying stuck at priority 0 after startup (osism/ovn-network-agent#127)
- Make the OVN monitor cache consistency check content-aware so a dropped update, not just a dropped insert, triggers a cache rebuild (osism/ovn-network-agent#127)
- Route the remaining failover-path NB/SB reads (gateway drain polling, managed route and MAC binding cleanup, stale-chassis detection) through the cache consistency guard (osism/ovn-network-agent#127)
- Confirm the chassisredirect port binding against the SB server before boosting gateway chassis priority, preventing a priority ratchet when two agents believe they are active for the same router (osism/ovn-network-agent#127)

### Dependencies
- github/codeql-action 68bde55 → 9e0d7b8 (osism/ovn-network-agent#124)

## [v0.5.0] - 2026-05-15

### Added
- Add a containerlab-based E2E test harness: OVN central and gateway-node (OVS + ovn-controller + FRR + agent) images, a canned 3-gateway topology with upstream BGP peer and clients, an idempotent OVN NB bootstrap script, `make e2e-up`/`make e2e-down`/`make e2e-install-tools` targets, and contributor documentation (osism/ovn-network-agent#103)
- Extend the E2E test harness bootstrap to provision a full working data plane (external switch, underlay, BGP, kernel workload) and add a baseline reachability scenario with CI workflow, Make target, and documentation (osism/ovn-network-agent#104)
- Add an HA failover E2E scenario that stops the master chassis to verify OVN re-election, host the test workload on the priority-10 chassis so failover does not also take out the responder, and wire the scenario into CI with documentation (osism/ovn-network-agent#107)
- Add stale-chassis cleanup E2E scenario that hard-kills a gateway and verifies surviving peers garbage-collect its managed NB rows (osism/ovn-network-agent#112)
- Add same-chassis hairpin E2E test scenario verifying the agent's OpenFlow hairpin rule on `br-ex` for co-located FIPs, with make/CI wiring and documentation (osism/ovn-network-agent#108)
- Add port-forward (DNAT) E2E test scenario with a pf-backend HTTP responder verifying source IP preservation, with make/CI wiring and documentation (osism/ovn-network-agent#109)
- Add port-forward hairpin masquerade E2E scenario (`pf-hairpin.sh`), wire it into `make e2e-pf-hairpin` and CI, and document the flag's test coverage (osism/ovn-network-agent#110)

### Changed
- Tighten the E2E test reconcile interval and stale-chassis grace period to fit within CI job budgets (osism/ovn-network-agent#112)
- Run the E2E workflow on pull requests in addition to pushes to main so authors can debug CI-only lab failures (osism/ovn-network-agent#117)
- Load the vrf kernel module before bringing up the E2E lab, installing linux-modules-extra on demand (osism/ovn-network-agent#117)

### Fixed
- Switch the gwnode OVS datapath from userspace to kernel and derive a unique per-chassis encap IP, fixing OVN chassisredirect election and gateway HA in the E2E lab (osism/ovn-network-agent#104)
- Fix drain and active-priority-lead reconciliation to fall back to a direct NB select when the libovsdb cache is missing the local Gateway_Chassis row (osism/ovn-network-agent#116)
- Fix E2E artifact collection script quoting that collapsed per-node log captures into a single invocation (osism/ovn-network-agent#117)
- Match legacy OVS `nw_dst`/`IN_PORT` flow-dump syntax in hairpin scenario assertions (osism/ovn-network-agent#118)
- Provision the `loopback1` device in the gwnode entrypoint to prevent agent startup failure when port forwarding is configured (osism/ovn-network-agent#110)

## [v0.4.0] - 2026-05-13

### Added
- Add test coverage for OVS flow management functions (osism/ovn-network-agent#38)
- Add test coverage for OVSDB write paths in gateway routing, covering route and MAC-binding reconciliation, priority boosting, stale-entry cleanup, and gateway drain/restore (osism/ovn-network-agent#37)
- Add integration test harness (testenv package, smoke test, setup script, Makefile target, and CI workflow) for exercising the agent against a real OVN/OVS/FRR/nftables stack (osism/ovn-network-agent#47)
- Integration test scenarios for layer 2 reconciliation covering FIP add/remove, gatewayless virtual gateway, multi-router on one chassis, failover, stale-chassis cleanup, drain on shutdown, and restore-drained on startup (osism/ovn-network-agent#48)
- Add layer 2 port-forward integration test scenarios covering DNAT, sticky multi-backend hashing, VIP management, masquerade, l3mdev sysctls, output chains, and SIGTERM cleanup (osism/ovn-network-agent#49)
- Add optional Prometheus `/metrics` endpoint with reconcile, route stability, OVN connection, drain, and stale chassis metrics (osism/ovn-network-agent#50)
- Add integration test harness primitives FastDefaults, WithAgent, and dump-on-fail diagnostics (osism/ovn-network-agent#66)
- Add IPv6 layer 2 integration test scenarios for FIP, hairpin, and dual-stack routers (osism/ovn-network-agent#71)
- Add layer 2 drift-recovery integration test scenarios (osism/ovn-network-agent#72)
- Add integration test scenarios for FRR prefix-list reconcile behavior (osism/ovn-network-agent#75)
- Add NetworkCIDRs field to AgentConfig in the integration test harness (osism/ovn-network-agent#76)
- Add integration test coverage for drain edge cases: timeout, no local routers, and cleanup-on-shutdown disabled (osism/ovn-network-agent#77)
- Add Prometheus /metrics integration tests covering reconcile counters, drain outcome, stale-chassis cleanup, and desired-state gauges (osism/ovn-network-agent#78)
- Add integration test coverage for UDP port forwarding, port translation, fwmark policy routing, and multi-VIP separation (osism/ovn-network-agent#79)
- Add SNAT and external_mac NAT test fixtures and gateway_port agent configuration for integration tests (osism/ovn-network-agent#80)
- Add integration test coverage for NAT-type variants (SNAT, external_mac) and gateway_port single-port mode (osism/ovn-network-agent#62)
- Add integration test coverage for bridge-IP and proxy-ARP lifecycle management (osism/ovn-network-agent#63)
- Add integration test coverage for multi-stale-chassis cleanup scenarios (osism/ovn-network-agent#64)
- Add unit test coverage for the OVN connect-retry loop and an integration test scenario for OVN database pause/resume resilience (osism/ovn-network-agent#64)
- Add AssertEventually-with-dump test helper and expose port-forward fwmark/priority/table-id constants in the integration test environment (osism/ovn-network-agent#67)
- Add router_masquerade VIP option for source-selective SNAT that fixes hairpin NAT for instances behind a router without a Floating IP (osism/ovn-network-agent#15)
- Add test coverage for config edge cases: main routing table alias collisions, malformed StringOrSlice YAML input, and VethProviderIP auto-compute wrap-around (osism/ovn-network-agent#87)
- Add test coverage for nftables ruleset reapply after failure, veth forwarding sysctl errors, reconcile under a cancelled context, and snake-cased LRP names in gateway chassis priority boosts (osism/ovn-network-agent#87)
- Add integration test harness support for tool failure injection (vtysh/nft/ovs-ofctl) and configurable routing-table IDs, with scenarios covering mid-cycle failures and self-heal, route-table collisions, shutdown cleanup without drain, router masquerade ordering, same-batch FIP add/remove, and partial FRR failure retry (osism/ovn-network-agent#88)
- Add VitePress tooling for the documentation site (osism/ovn-network-agent#92)
- Add a Diátaxis-organised documentation site (Tutorials, How-to guides, Reference, Explanation, Contributing) migrated from the README files, with automated deployment to GitHub Pages on changes to docs (osism/ovn-network-agent#92)
- Add a static-analysis reference generator (docgen) that derives the configuration, CLI, and metrics documentation pages from config.go and metrics.go, including metric label-value enumerations, generated via `go generate` with a CI guard job that fails builds on stale docs (osism/ovn-network-agent#96)
- SECURITY.md with a private vulnerability disclosure policy and supported-versions matrix (osism/ovn-network-agent#98)
- Vtysh exec hook and unit tests for FRR routing helpers, plus coverage for ListManagedRouteChassis and OVSDB per-operation error handling (osism/ovn-network-agent#100)

### Changed
- Add gofmt formatting check to CI lint workflow (osism/ovn-network-agent@e3a7099)
- Pin nfpm to a fixed version in the release workflow to prevent unreviewed upstream changes (osism/ovn-network-agent@87ce984)
- Add table of contents to README and correct Go version requirement (osism/ovn-network-agent@47ee186)
- Sync ovn-network-agent.default sample with all environment variables consumed by the agent (osism/ovn-network-agent@d3e416f)
- Clarify transactOps comment to describe behavior instead of a specific libovsdb version (osism/ovn-network-agent@e08975d)
- Clean up gofmt formatting drift across the codebase (osism/ovn-network-agent@27b657a)
- Replace timer-based OVN state refresh scheduling with a channel-driven event loop, threading context through reconcile and cleanup instead of storing it on OVNClient (osism/ovn-network-agent@b3a236f)
- Automatically add opened issues and pull requests to the project board (osism/ovn-network-agent#52)
- Collapse single-backend port-forward integration test scenarios into a table-driven matrix (osism/ovn-network-agent#79)
- Document new failure-injection integration test scenarios and the flake-resistance test command in the integration test README (osism/ovn-network-agent#88)
- Replace the monolithic README and integration test docs with short stubs linking to the published documentation site (osism/ovn-network-agent#92)
- Updated architecture and how-it-works documentation to reflect the gateway priority lead, veth-leak reconciliation, and port-forward VIP routing, and refreshed the integration-test directory listing (osism/ovn-network-agent#97)
- Increase unit test coverage for OVN client event handling and state refresh, metrics recording, agent reconciliation, and OVS flow discovery (osism/ovn-network-agent#100)
- Enforce a minimum statement-coverage floor in CI and add Linux-specific unit tests for routing helpers (osism/ovn-network-agent#100)

### Fixed
- Correct drain restore note in the YAML config sample (osism/ovn-network-agent@d29eb4f)
- Cap systemd restart storms and widen the service stop timeout from 60s to 120s so gateway drain and OVN NB cleanup can finish before SIGKILL (osism/ovn-network-agent@359a07c)
- Fix a leak of partially-initialized OVSDB clients when `Connect()` fails, so retries start from a clean slate instead of leaking the previous connection (osism/ovn-network-agent#26)
- Fix unbounded goroutine fan-out during HA failover storms by coalescing concurrent state refreshes into a single in-flight pass plus one queued follow-up (osism/ovn-network-agent@529ffbf)
- Fix a data race on the OVN client's ready flag by making it atomic (osism/ovn-network-agent@88e2afc)
- Fix nftables syntax for multi-backend DNAT maps and multi-VIP conntrack sets (osism/ovn-network-agent#49)
- Fix veth-leak policy rules not being reinstalled when only the rule, not the route, is removed out-of-band (osism/ovn-network-agent#74)
- Fix gofmt alignment in port-forward validation test (osism/ovn-network-agent#90)
- Fix integration test harness to correctly apply ExtraEnv PATH overrides on Linux so failure-injection shims are not bypassed (osism/ovn-network-agent#94)
- Fix nft failure-injection shim to target the ruleset load call and add invocation diagnostics so forced failures are reliably detected in failure-injection scenarios (osism/ovn-network-agent#88)

### Dependencies
- actions/upload-artifact bbbca2d → 043fb46 (osism/ovn-network-agent#18)
- softprops/action-gh-release v2 → v3 (osism/ovn-network-agent#19, osism/ovn-network-agent#20)
- golangci-lint v2.11.4 → v2.12.0 (osism/ovn-network-agent#24)
- orhun/git-cliff-action c93ef52 → f50e115 (osism/ovn-network-agent#23)
- sigstore/cosign-installer v3 → v4.1.1 (osism/ovn-network-agent#22)
- github/codeql-action c10b806 → e46ed2c (osism/ovn-network-agent#21)
- golangci-lint v2.12.0 → v2.12.1 (osism/ovn-network-agent#40)
- golangci/golangci-lint v2.12.1 → v2.12.2 (osism/ovn-network-agent#53)
- github.com/prometheus/client_golang v1.22.0 → v1.23.2 (osism/ovn-network-agent#51)
- sigstore/cosign-installer v4.1.1 → v4.1.2 (osism/ovn-network-agent#68)
- actions/dependency-review-action v4 → v5 (osism/ovn-network-agent#73)
- github/codeql-action e46ed2c → 68bde55 (osism/ovn-network-agent#69)

## [v0.3.0] - 2026-03-30

### Added
- Add `hairpin_masquerade` VIP option for source-selective SNAT that fixes hairpin NAT when a floating IP on the same node connects to a port-forwarded VIP (osism/ovn-network-agent@3946fb0)
- Add per-rule `masquerade` override and OUTPUT-hook nftables chains (`output_ctzone`, `output_fwmark`) so DNAT backends can run on the same host as the VIP (osism/ovn-network-agent@3946fb0)
- Add `port_forward_l3mdev_accept` option to deliver DNAT'd packets to backends in a different VRF on the same host (osism/ovn-network-agent@3946fb0)
- Add configurable `port_forward_ct_zone` (default 64000) to avoid conntrack zone collisions with OVN (osism/ovn-network-agent@3946fb0)
- Add per-IP OVS hairpin flows so floating IPs on OVN routers sharing the same gateway chassis can communicate with each other (osism/ovn-network-agent#16)
- Improve supply chain security with SHA-pinned GitHub Actions, govulncheck, CodeQL analysis, and OpenSSF Scorecard (osism/ovn-network-agent@ec12328)

### Changed
- Document the OVS hairpin flows feature in the README (osism/ovn-network-agent@6a2c7fb)

### Fixed
- Propagate OVS flow deletion failures during hairpin and MAC-tweak flow reconciliation as errors instead of silently logging them (osism/ovn-network-agent#16)
- Rewrite both source and destination MAC on hairpin flows for reliable cross-router delivery (osism/ovn-network-agent@bc883ac)

### Dependencies
- github/codeql-action v3 → v4 (osism/ovn-network-agent#17)

## [v0.2.0] - 2026-03-24

### Added
- Add nftables-based DNAT port forwarding for anycast VIPs, letting services on gateway nodes be reached via BGP-announced VIPs while preserving client IPs through connmark-based return routing (osism/ovn-network-agent@4191d2d)
- Add gateway drain mode: lowers Gateway_Chassis priority to 0 before shutdown so OVN migrates HA gateways to standby chassis before cleanup, closing the traffic disruption window between BGP withdrawal and OVN failover (osism/ovn-network-agent@5da9b2a)
- Add sticky multi-backend support for DNAT port forwarding via `dest_addrs`, using nftables jhash source-IP hashing for consistent client-to-backend mapping (osism/ovn-network-agent@14225ea)

### Changed
- Rename project from ovn-route-agent to ovn-network-agent, changing OVN ExternalIDs keys, the nftables table name, environment variable prefix (OVN_ROUTE_ → OVN_NETWORK_), and systemd/config paths (osism/ovn-network-agent@026506f)

### Fixed
- Add systemd Wants/After dependencies on the OVS vswitchd and OVN controller container services and raise TimeoutStopSec to 60s so the agent stops before them and gateway drain can complete during shutdown (osism/ovn-network-agent@df901ff)

### Dependencies
- golangci-lint v2.11.3 → v2.11.4 (osism/ovn-network-agent#14)

## [v0.1.1] - 2026-03-22

### Added
- Add stale chassis cleanup for OVN NB entries so surviving agents remove static routes and MAC bindings left behind by chassis that disappear from the OVN SB Chassis table, after a configurable grace period (osism/ovn-network-agent@36b117c)

## [v0.1.0] - 2026-03-19

### Added
- Initial implementation of ovn-route-agent, an event-driven Floating IP route synchronization agent for OVN-based OpenStack environments (osism/ovn-network-agent@9c7fb97)
- Add multi-router support, proxy ARP, OVS MAC-tweak flows, and a per-bridge routing table for correct floating IP packet flow (osism/ovn-network-agent@a693638)
- Add gatewayless provider network support with automatic OVN NB default routes and static MAC bindings (osism/ovn-network-agent@b38c934, osism/ovn-network-agent@7074365)
- Remove managed OVN Northbound static routes and MAC bindings for locally-active routers on shutdown (osism/ovn-network-agent@3ce231b)
- Add detailed architecture documentation with control and data plane diagrams (osism/ovn-network-agent@ad438ca)
- Add native veth VRF route leaking managed directly by the agent, replacing the external veth-vrf-leak.sh script, with new --veth-leak-enabled, --veth-provider-ip, --veth-leak-table-id, and --veth-leak-rule-priority options (osism/ovn-network-agent@44908ad, osism/ovn-network-agent@e56868e)
- Auto-discover provider networks from OVN `Logical_Router_Port.Networks` when `network_cidr` is not set, with manual configuration still taking precedence (osism/ovn-network-agent@8a25161)
- Add dynamic FRR prefix-list management via new `frr_prefix_list` option, maintaining `permit <network> ge 32 le 32` entries for discovered provider networks (osism/ovn-network-agent@8a25161)
- Extract SNAT IPs from SB Port_Binding NatAddresses on gateway patch ports so routes are announced before the NB NAT entry exists (osism/ovn-network-agent@f62ec52, osism/ovn-network-agent@1399d2a)
- Add git-cliff based changelog generation for GitHub releases (osism/ovn-network-agent@9a80a1e)
- Attach a CycloneDX SBOM as a release artifact (osism/ovn-network-agent@70ed544)
- Add Debian package (.deb) builds, cosign signatures, and SLSA provenance attestations to releases (osism/ovn-network-agent@97a0aba, osism/ovn-network-agent@40131ed)
- Add Renovate configuration for automated dependency updates (osism/ovn-network-agent#1)

### Changed
- Reconcile veth VRF leak routes and policy rules dynamically per discovered or configured network instead of requiring a static `network_cidr` (osism/ovn-network-agent@8a25161)
- Batch FRR route additions and removals into single vtysh calls and trigger a BGP outbound soft-refresh for faster convergence (osism/ovn-network-agent@f9dc570)
- Bump minimum Go version requirement to 1.25 (osism/ovn-network-agent@97a0aba)
- Improve control plane and data plane ASCII architecture diagrams in documentation (osism/ovn-network-agent@72f4bea)

### Fixed
- Fix silent OVSDB transaction failures by checking per-operation results (osism/ovn-network-agent@07078a1)
- Fix OVSDB named-uuid syntax error by replacing hyphens with underscores (osism/ovn-network-agent@29be447)
- Remove the bridge IP from `br-ex` on shutdown cleanup (osism/ovn-network-agent@5c5df36)
- Restrict BGP outbound soft-refresh to route removals only and verify routes after every change, re-adding any that go missing, to avoid disrupting existing BGP announcements (osism/ovn-network-agent@e3ddde1)

### Dependencies
- libovsdb 0.7.0 → 0.8.1, migrated from ovn-org/libovsdb to ovn-kubernetes/libovsdb (osism/ovn-network-agent@2c90330)
- golangci-lint v2.11.1 → v2.11.3 (osism/ovn-network-agent#2)
- github.com/vishvananda/netlink v1.3.0 → v1.3.1 (osism/ovn-network-agent#3)
- actions/attest-build-provenance v2 → v4 (osism/ovn-network-agent#5)
- actions/checkout v5 → v6 (osism/ovn-network-agent#6)
- actions/upload-artifact v6 → v7 (osism/ovn-network-agent#7)
- actions/download-artifact v6 → v8 (osism/ovn-network-agent#7)
- golangci/golangci-lint-action v7 → v9 (osism/ovn-network-agent#8)
- github.com/cenkalti/backoff v4.3.0 → v5.0.3 (osism/ovn-network-agent#9)


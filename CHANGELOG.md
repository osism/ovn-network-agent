# Changelog

All notable changes to this project are documented here. This file is
generated with [git-cliff](https://git-cliff.org); regenerate it with
`git cliff -o CHANGELOG.md`. Release notes on GitHub use the same
configuration with `--latest --strip header`.

## [Unreleased]

### Build

- require Go 1.26.4
- patch security advisories in Go and npm dependencies
- require Go 1.26.5
- single-source the systemd unit at /usr/bin
- restart the service on package upgrade, guard for chroots
- soften the FRR dependency to Wants= and Recommends

### CI

- run the drain-hitless scenario after the stale-chassis E2E job
- drop the drain-hitless EXIT-trap recycle in CI
- run only necessary jobs after merge to main
- smoke-test the .deb install/upgrade/remove cycle on PRs

### Documentation

- describe multi-network VLAN segment support
- cover the drain-hitless scenario
- sync the drain-hitless description with the scenario
- fix VitePress build by upgrading to v2 (Vite 8)
- describe the drain takeover handshake
- describe route ownership tags and the IPv4-only guard
- document bring-up retries and the new triage artifacts
- add the rolling upgrade guide
- add CONTRIBUTING.md, a bug-report form, and a PR template

### Miscellaneous

- update dependency vite to v8
- update actions/checkout action to v7
- update GitHub Actions digests

### Other

- Fix project-board automation for fork PRs
- Merge pull request #142 from osism/fix/add-to-project-fork-prs
- discover localnet segments and attach them to local routers
- install MAC-tweak and hairpin flows per localnet patch port
- route FIP /32s over each segment's kernel interface
- write per-router MAC bindings from the segment interface
- add localnet_segments gauge
- surface NB cache contents on empty-match log
- let OVN_NETWORK_DRAIN_ON_SHUTDOWN enable draining
- wake the drain migration wait from chassisredirect events
- write a takeover readiness marker after a successful announce
- replace the fixed drain settle with awaiting the takeover marker
- redefine drain_settle_delay as the post-readiness margin
- validate NAT external IPs unconditionally in refreshState
- route and announce only IPv4 from the desired set
- enforce IPv4 in validateIP
- skip invalid IPs and failed chunks in AddFRRRoutes
- continue past per-flow failures in hairpin reconcile
- own kernel routes via protocol tag, FRR routes via nexthop
- retire the empty-filters managed-route fallback
- reject non-positive reconcile-interval
- fail fast on unparsable duration and integer values
- honor both directions for every boolean env var
- warn on unknown config file keys
- add --check-config for pre-restart validation
- sync shipped env file and sample with the code
- fail when shipped samples miss a config option

### Testing

- raise the reconcile window to 60s to de-flake reachability
- cover multi-VLAN segments, failover and restart
- add multi-vlan scenario with two VLAN networks on one node
- add drain-hitless scenario
- calibrate the drain-hitless outage budget
- correct the drain-hitless delta assertion message
- settle ends on the readiness marker, not a timer
- cover dual-stack announce and route ownership
- dump daemon state when a bring-up gate times out
- retry the upstream bgpd start within a bounded budget
- collect upstream FRR and ovn-controller state
## [0.6.0] - 2026-05-16

### Documentation

- document port-forward-only mode
- document the post-drain settle delay
- restructure the E2E harness page for readability

### Other

- derive operating mode and validate the OVN/port-forward matrix
- make the OVN client optional for port-forward-only mode
- relax the OVN-remote requirement for port-forward-only mode
- Update github/codeql-action digest to 9e0d7b8 (#124)
- guard refreshState against monitor-cache INSERT drops
- detect FRR static routes that are configured but not advertised
- refresh OVN state after drain so cleanup keeps the peer's entries
- rediscover the br-ex patch port instead of caching it forever
- wake on Gateway_Chassis changes so priority ties resolve fast
- add drain_settle_delay option
- hold for the settle delay after drain before cleanup
- soft-refresh BGP when takeover routes are added
- fall back to NB select when restore-drain misses the local row
- make the OVN cache consistency check content-aware
- route the remaining failover-path NB/SB reads through the guard
- confirm the chassisredirect binding before an active-lead boost
## [0.5.0] - 2026-05-15

### CI

- add E2E workflow that runs the baseline scenario on main pushes
- run the failover scenario after the baseline E2E job
- run the stale-chassis scenario after the failover E2E job
- also run the E2E workflow on pull requests
- load the vrf kernel module before bringing the E2E lab up

### Documentation

- publish containerlab E2E harness guide
- cover the failover scenario and the workload-host move
- cover the stale-chassis cleanup scenario
- describe the same-chassis hairpin E2E scenario (issue #108)
- describe the port-forward DNAT E2E scenario (issue #109)
- describe the port-forward hairpin masquerade E2E scenario (issue #110)

### Other

- fall back to NB select when cache is missing the local row
- fall back to NB select when cache is missing the local row
- wire the e2e-hairpin scenario into make and CI (issue #108)
- wire the e2e-pf-external scenario into make and CI (issue #109)
- wire the e2e-pf-hairpin scenario into make and CI (issue #110)

### Testing

- add OVN central image for containerlab harness
- add gateway-node image (OVS + ovn-controller + FRR + agent)
- add containerlab topology and OVN NB bootstrap
- add e2e-up/e2e-down make targets and README
- drop staticd toggle in gwnode Dockerfile
- add make target to install containerlab + preflight check
- build images with plain docker build, not buildx
- create kernel VRF + seed FRR config in gwnode entrypoint
- match the agent via cmdline in the gwnode healthcheck
- switch gwnode OVS to kernel datapath
- extend bootstrap to provision a working data plane
- add baseline reachability scenario and artifact collector
- wire e2e-baseline make target and document scenarios + CI
- host the workload on gateway-3 instead of the master
- add HA failover scenario (master chassis loss)
- tighten cleanup cadence for the CI lab
- add stale-chassis cleanup scenario
- fix per-node artifact collection quoting
- add same-chassis hairpin scenario (issue #108)
- match OVS legacy nw_dst / IN_PORT in hairpin assertions
- ship a tiny pf-backend HTTP responder (issue #109)
- add port-forward DNAT scenario (issue #109)
- provision loopback1 in the gwnode entrypoint (issue #110)
- add port-forward hairpin masquerade scenario (issue #110)
## [0.4.0] - 2026-05-13

### Bug Fixes

- cap restart storms and widen stop budget for drain+cleanup
- close partial OVSDB clients on Connect failure (#26)
- coalesce immediateStateRefresh to bound goroutine fan-out
- make OVNClient.ready atomic to fix data race
- correct map-DNAT and multi-VIP set syntax
- update module github.com/prometheus/client_golang to v1.23.2 (#51)

### CI

- pin nfpm to v2.46.3 in release workflow
- add gofmt drift check
- deploy the VitePress docs to GitHub Pages on push to main (#92)
- enforce 78% statement-coverage floor
- align coverage floor with Linux build; add Linux unit tests

### Documentation

- add table of contents and fix Go version requirement
- sync ovn-network-agent.default with applyEnvConfig
- correct drain restore note in yaml sample
- anchor transactOps comment to behavior, not libovsdb version
- mention new #64 resilience scenarios in test README
- index the new #88 failure-injection scenarios
- add VitePress tooling for the documentation site (#92)
- migrate README content into Diátaxis-organised docs site (#92)
- shrink README and integration stub to point at the published site (#92)
- regenerate reference pages via go generate and CI guard
- realign explanations and integration-test layout with current code (#97)
- add SECURITY.md with private disclosure policy (#98)

### Features

- add optional Prometheus /metrics endpoint (#50)
- add router_masquerade for hairpin NAT fix on instances behind a router without FIP (#15)

### Miscellaneous

- update actions/upload-artifact digest to 043fb46 (#18)
- update softprops/action-gh-release digest to 3bb1273 (#19)
- update dependency golangci/golangci-lint to v2.12.0 (#24)
- update orhun/git-cliff-action digest to f50e115 (#23)
- update sigstore/cosign-installer action to v4 (#22)
- update github/codeql-action digest to e46ed2c (#21)
- update softprops/action-gh-release action to v3 (#20)
- update dependency golangci/golangci-lint to v2.12.1 (#40)
- update dependency golangci/golangci-lint to v2.12.2 (#53)
- update sigstore/cosign-installer action to v4.1.2 (#68)
- update actions/dependency-review-action action to v5 (#73)
- update github/codeql-action digest to 68bde55 (#69)

### Other

- gofmt drift cleanup
- automatically add all opened issues and PRs to project board
- introduce static-analysis reference generator
- capture metric label-value enumerations
- gofmt parse.go
- introduce vtysh exec hook for unit testing
- add unit tests for FRR helpers via the vtysh hook
- cover ListManagedRouteChassis and OVSDB op errors
- cover event handlers, signal channels, and refreshState
- cover initMetrics and the remaining setter branches
- cover cleanupStaleChassis, removeAllRoutes, ensureRoutes, cleanup
- exercise EnsureOVSFlows first-call discovery path
- drop nil-check before len() in reconcile test

### Refactor

- remove OVNClient.ctx field, thread ctx via parameters

### Testing

- cover flow management functions via exec capture (#38)
- cover OVSDB write paths in ovn_gateway.go (#37)
- add layer 2 integration test harness foundation (#47)
- add layer 2 reconciliation scenarios (#48)
- add layer 2 port-forward scenarios
- add harness primitives FastDefaults, WithAgent, dump-on-fail (#66)
- add IPv6 layer 2 scenarios (FIP, hairpin, dual-stack) (#71)
- add layer 2 drift-recovery scenarios (#72)
- add layer 2 veth-leak lifecycle scenarios (#74)
- add FRR prefix-list reconcile scenarios (#75)
- add NetworkCIDRs field to AgentConfig harness
- add NetworkFilters / network_cidr scenarios
- add drain edge-case scenarios (#77)
- add MetricsListen field to AgentConfig harness
- add /metrics scrape helpers to testenv
- add Prometheus /metrics scrape scenarios
- add AssertIPRulePriority / AssertRouteInTable helpers
- add UDP / port-translation / fwmark / multi-VIP PF scenarios
- collapse single-backend PF scenarios into a matrix
- add SNAT / external_mac fixtures and gateway_port config
- add #62 NAT-type variant scenarios
- add #62 gateway_port single-port scenarios
- add bridge-address helpers for #63
- add #63 bridge-IP and proxy-ARP lifecycle scenarios
- add #64 multi-stale-chassis scenarios
- unit-cover the OVN connect-retry loop (#64 scenario 2)
- add #64 OVN database pause/resume scenario
- expose port-forward priority constants in testenv (#67 item 4)
- add AssertEventually-with-dump helper (#67 item 1)
- cover route_table_id 0 vs main-table aliases (#87 item 1)
- cover StringOrSlice malformed-input path (#87 item 2)
- cover nextIPInSubnet wrap at 255.255.255.255 caller (#87 item 3)
- cover applyNftRuleset reapply after nft failure (#87 item 4)
- cover ensureVethForwarding sysctl error path (#87 item 5)
- cover reconcile under pre-cancelled context (#87 item 6)
- cover EnsureActivePriorityLead with snake_cased LRP (#87 item 7)
- gofmt fix for item 1 alignment
- add WithFailingTool shim + table-id config knobs (#88)
- mid-cycle vtysh/nft/ovs-ofctl failure + self-heal (#88 item 1)
- route_table_id collision coverage (#88 item 2)
- cleanup-on-shutdown without drain phase (#88 item 3)
- router_masquerade startup before SNAT NAT (#88 item 4)
- same-batch FIP add + remove (#88 item 5)
- partial-failure FRR retry, kernel untouched (#88 item 6)
- gofmt fixes for the new failure-injection files
- ensure ExtraEnv overrides win across libc implementations
- record per-invocation armed state in shim log (#88)
- record counter and match diagnostics in shim log (#88)
- target nft failure injection at the load call (#88)
## [0.3.0] - 2026-03-30

### Bug Fixes

- rewrite both src and dst MAC on hairpin flows for reliable cross-router delivery

### Documentation

- add hairpin OVS flows section to README

### Features

- add hairpin_masquerade, port_forward_l3mdev_accept, and port_forward_ct_zone
- add OVS hairpin flows for same-chassis cross-router FIP communication (#16)
- improve supply chain security with SHA-pinned actions, govulncheck, CodeQL, and Scorecard

### Miscellaneous

- update github/codeql-action action to v4 (#17)
## [0.2.0] - 2026-03-24

### Bug Fixes

- add proper systemd dependencies for graceful start/stop ordering

### Features

- add nftables-based DNAT port forwarding for anycast VIPs
- add gateway drain mode for graceful HA failover on shutdown
- add sticky multi-backend support for DNAT port forwarding

### Miscellaneous

- update dependency golangci/golangci-lint to v2.11.4 (#14)

### Refactor

- rename project from ovn-route-agent to ovn-network-agent
## [0.1.1] - 2026-03-22

### Features

- add stale chassis cleanup for OVN NB entries
## [0.1.0] - 2026-03-19

### Bug Fixes

- silent OVSDB transaction failures by checking operation results
- OVSDB named-uuid syntax error by replacing hyphens with underscores
- remove bridge IP (br-ex) on shutdown cleanup
- harden route reconciliation with BGP refresh only on removals and post-change verification
- update module github.com/vishvananda/netlink to v1.3.1 (#3)
- update module github.com/cenkalti/backoff/v4 to v5 (#9)

### Build

- migrate libovsdb from ovn-org to ovn-kubernetes/libovsdb v0.8.1

### CI

- add git-cliff changelog generation for releases
- attach CycloneDX SBOM as release artifact
- add .deb packaging, cosign signatures, and SLSA provenance to releases

### Documentation

- add detailed architecture documentation with control and data plane diagrams
- document SB NatAddresses SNAT extraction and fast failover debounce bypass
- expand gatewayless provider network and virtual gateway documentation
- add .deb and binary installation instructions from GitHub Releases
- improve control plane and data plane ASCII architecture diagrams

### Features

- initial implementation of ovn-route-agent
- add multi-router support, proxy ARP, OVS MAC-tweak flows, and per-bridge routing table
- add gatewayless provider network support with OVN NB writes
- clean up managed OVN NB entries on shutdown
- add native veth VRF route leaking
- auto-discover provider networks from OVN and manage FRR prefix-list dynamically
- extract SNAT IPs from SB gateway port NatAddresses for immediate route announcement

### Miscellaneous

- update dependency golangci/golangci-lint to v2.11.3 (#2)
- update actions/attest-build-provenance action to v4 (#5)
- update actions/checkout action to v6 (#6)
- update github artifact actions (#7)
- update golangci/golangci-lint-action action to v9 (#8)

### Other

- Add renovate.json (#1)

### Performance

- batch FRR route operations and trigger BGP soft-refresh for faster convergence

### Refactor

- remove contrib/ from filenames and inline origin references

# ovn-network-agent

Event-driven network agent for OVN-based OpenStack environments. A real-time
daemon that watches OVN databases directly via the OVSDB protocol to
synchronize Floating IP routes and optionally forward traffic from anycast
VIPs to internal backends.

## Requirements

- **OVN 24.09 or newer** — 24.09 is the floor the checked-in OVSDB schemas are
  generated from; CI additionally tests against OVN 25.09. See
  [OVSDB models and the supported OVN range](https://osism.github.io/ovn-network-agent/contributing/ovsdb-models).
- **FRR 8.0 or newer** — the agent reads routes and prefix-lists via `vtysh`'s
  JSON output (`show ip route … json`, `show ip prefix-list … json`).
- **Linux** with `nftables` and Open vSwitch on the gateway node.

## Documentation

Full documentation is published at
**<https://osism.github.io/ovn-network-agent/>**, organised with the
[Diátaxis](https://diataxis.fr) framework:

- [Tutorial — first agent on a test host](https://osism.github.io/ovn-network-agent/tutorials/first-agent)
- How-to guides:
  - [Install the agent](https://osism.github.io/ovn-network-agent/guides/installation)
  - [Upgrade the agent](https://osism.github.io/ovn-network-agent/guides/upgrade)
  - [Configure the agent](https://osism.github.io/ovn-network-agent/guides/configuration)
  - [Create a gatewayless provider network](https://osism.github.io/ovn-network-agent/guides/gatewayless-provider-network)
  - [Set up port forwarding (DNAT)](https://osism.github.io/ovn-network-agent/guides/port-forwarding)
  - [Configure gateway drain](https://osism.github.io/ovn-network-agent/guides/gateway-drain)
  - [Configure the FRR prefix list](https://osism.github.io/ovn-network-agent/guides/frr-prefix-list)
  - [Enable metrics and alerts](https://osism.github.io/ovn-network-agent/guides/metrics)
  - [Troubleshooting](https://osism.github.io/ovn-network-agent/guides/troubleshooting)
- Reference:
  - [Configuration](https://osism.github.io/ovn-network-agent/reference/configuration)
  - [Metrics](https://osism.github.io/ovn-network-agent/reference/metrics)
- Explanation:
  - [Architecture](https://osism.github.io/ovn-network-agent/explanation/architecture)
  - [How the reconcile loop works](https://osism.github.io/ovn-network-agent/explanation/how-it-works)
  - [Multi-router support](https://osism.github.io/ovn-network-agent/explanation/multi-router)
  - [Gatewayless provider networks](https://osism.github.io/ovn-network-agent/explanation/gatewayless-networks)
  - [Port forwarding (DNAT)](https://osism.github.io/ovn-network-agent/explanation/port-forwarding)
  - [Gateway drain mode](https://osism.github.io/ovn-network-agent/explanation/gateway-drain)
- Contributing:
  - [Integration tests](https://osism.github.io/ovn-network-agent/contributing/integration-tests)
  - [Containerlab E2E harness](https://osism.github.io/ovn-network-agent/contributing/e2e-tests)
  - [OVSDB models and the supported OVN range](https://osism.github.io/ovn-network-agent/contributing/ovsdb-models)

To build the docs locally:

```bash
npm install
npm run docs:dev      # serve at http://localhost:5173
npm run docs:build    # static build in docs/.vitepress/dist
```

The Markdown sources live in `docs/`. Editing them and pushing to `main`
triggers the [Deploy Documentation](.github/workflows/deploy-docs.yaml)
workflow.

## Quick start

Requires Go 1.26+ (see `go.mod` for the exact minimum). Build the binary,
write a minimal config pointing at your OVN cluster, and run it:

```bash
make build

cat > /etc/ovn-network-agent/config.yaml <<EOF
ovn_sb_remote: "tcp:10.10.0.1:6642,tcp:10.10.0.2:6642,tcp:10.10.0.3:6642"
ovn_nb_remote: "tcp:10.10.0.1:6641,tcp:10.10.0.2:6641,tcp:10.10.0.3:6641"
EOF

sudo ./ovn-network-agent --config /etc/ovn-network-agent/config.yaml --log-level debug --dry-run
```

See the [tutorial](https://osism.github.io/ovn-network-agent/tutorials/first-agent)
for the full walkthrough.

## Known limitations

See [Known limitations](https://osism.github.io/ovn-network-agent/#known-limitations)
in the docs for detail. In brief:

- **IPv4-only FIP routing plane** — kernel routes, FRR announcements, and the virtual gateway are IPv4 only ([#85](https://github.com/osism/ovn-network-agent/issues/85) / [#70](https://github.com/osism/ovn-network-agent/issues/70)).
- **No runtime configuration reload** — config is read once at startup; restart the agent to apply changes ([#91](https://github.com/osism/ovn-network-agent/issues/91)).
- **Port forwarding is IPv4-only** — multi-backend VIPs use `jhash ip saddr mod N`: sticky per client but not a consistent hash (a backend change remaps most clients), with no backend health checks.
- **Single provider bridge per agent** — one `bridge_dev` per instance; VLAN localnet segments all ride that one bridge.
- **Sizing** — the OVSDB monitors replicate whole Southbound/Northbound tables into every agent, so memory and reconcile time grow with cloud size, not node-local state alone.

## Origin

This agent is based on the shell script
[`ovn-network-agent.sh`](./contrib/ovn-network-agent.sh) which served as the
original prototype. The built-in veth VRF leak functionality
(`--veth-leak-enabled`) replaces the standalone script
[`veth-vrf-leak.sh`](./contrib/veth-vrf-leak.sh).

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for the development setup, commit
conventions, generated-docs workflow, and the test tiers.

## Security

To report a security vulnerability, please follow the private disclosure
process in [SECURITY.md](./SECURITY.md). Do not file public issues for
suspected security problems.

## License

Apache License 2.0 — see [LICENSE](./LICENSE).

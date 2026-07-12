# Configure the agent

Settings are loaded with the following priority (highest wins):

**CLI flags > environment variables > config file > defaults**

For the full list of every flag, env var, and config key, see the
[configuration reference](../reference/configuration).

## Config file (YAML)

```bash
ovn-network-agent --config /etc/ovn-network-agent/config.yaml
# or via environment variable
OVN_NETWORK_CONFIG=/etc/ovn-network-agent/config.yaml ovn-network-agent
```

See
[`ovn-network-agent.yaml.sample`](https://github.com/osism/ovn-network-agent/blob/main/ovn-network-agent.yaml.sample)
for a full example.

## Example

Config file `/etc/ovn-network-agent/config.yaml` with the base settings:

```yaml
ovn_sb_remote: "tcp:10.10.0.1:6642,tcp:10.10.0.2:6642,tcp:10.10.0.3:6642"
ovn_nb_remote: "tcp:10.10.0.1:6641,tcp:10.10.0.2:6641,tcp:10.10.0.3:6641"

# Optional: provider networks are auto-discovered from OVN when omitted
# network_cidr:
#   - "192.0.2.0/24"
#   - "198.51.100.0/24"
```

Run with the config file, overriding log level and enabling dry-run via CLI
flags:

```bash
ovn-network-agent --config /etc/ovn-network-agent/config.yaml --log-level debug --dry-run
```

CLI flags take precedence over values in the config file.

## Operating modes

The agent runs in one of two modes, derived automatically from the
configuration — there is no mode flag:

| `ovn_sb_remote` / `ovn_nb_remote` | `port_forwards` | Mode |
|---|---|---|
| both set | any | **full mode** |
| both empty | non-empty | **port-forward-only mode** |
| both empty | empty | error — nothing to do |
| exactly one set | any | error — incomplete OVN configuration |

The active mode is logged in the startup banner (`mode=full` or
`mode=port-forward-only`).

### Full mode

The default. The agent connects to the OVN Southbound and Northbound
databases, synchronises Floating IP routes, manages gateway routing, and
also serves any configured `port_forwards`.

### Port-forward-only mode

When `port_forwards` is configured but **both** OVN remotes are left unset,
the agent runs as a standalone VIP service: it manages only the configured
port-forward VIPs — DNAT rules, VIP addresses, connmark return routing, and
FRR static routes for BGP announcement — and never connects to OVN.

Use this for a node that should only expose configured VIPs (for example a
DNS resolver, monitoring collector, or API proxy) and is not an OVN gateway
chassis. Such a node needs no provider bridge (`br-ex`), no FIPs, and no
gateway routing.

Two masquerade options depend on OVN-derived state and are therefore
rejected at startup in port-forward-only mode:

- `router_masquerade` — router SNAT IPs are discovered from OVN; without an
  OVN connection there is no source for them.
- `hairpin_masquerade` — requires an explicit `network_cidr`, because
  provider CIDRs are normally auto-discovered from OVN.

```yaml
# Port-forward-only config: no OVN remotes, only port_forwards.
port_forwards:
  - vip: "198.51.100.10"
    manage_vip: true
    rules:
      - proto: tcp
        port: 443
        dest_addr: "10.0.0.100"
```

Switching modes at runtime is not supported — it requires a restart.

## Prerequisites

- **OVN** (full mode only): TCP access to OVN Southbound and Northbound
  databases on the control nodes (the agent runs on network/gateway nodes
  where no local DB sockets exist). Not needed in port-forward-only mode.
- **FRR**: `vtysh` must be available and the VRF + BGP configuration must
  already exist.
- **Linux**: provider bridge (e.g. `br-ex`) must exist in full mode;
  port-forward-only mode does not use it.
- **VRF route leaking**: the agent automatically creates and manages a veth
  pair connecting the default VRF to `vrf-provider` (enabled by default via
  `--veth-leak-enabled`). Per-network routes are reconciled dynamically based
  on auto-discovered or configured provider networks.
- **nftables**: `nft` binary must be in `PATH` (required for port forwarding /
  DNAT).
- **Permissions**: root or `CAP_NET_ADMIN` for netlink route manipulation.

## Validate a configuration

Use `--check-config` to parse and validate the full configuration and exit,
without connecting to OVN or touching system state. This is the pre-restart
gate for upgrade automation: run it before restarting a live gateway node so
a bad config is caught before it can disrupt the agent.

```bash
ovn-network-agent --check-config --config /etc/ovn-network-agent/config.yaml
```

The exit code is the contract:

- `0` — the configuration is valid; the command prints `configuration OK`.
- `1` — the configuration is invalid; the command logs a `configuration
  error` naming the offending setting.

`--check-config` applies the same layering (CLI flags, environment variables,
config file) and the same validation the agent runs at startup, so it
exercises the exact configuration the agent would load.

The agent fails fast on bad input rather than starting with a surprising
effective value:

- An unparsable or out-of-range value — an unparsable duration or integer, or
  a non-positive `reconcile_interval` — is a startup error naming the setting,
  on the flag, environment-variable, and config-file paths alike.
- An unknown config-file key (for example a typo like `drain_on_shutdow`) is
  logged as a warning naming the key and then ignored — the key is accepted so
  a newer config stays forward-compatible with an older agent, but the typo is
  no longer silent.

## Where to go next

- [Configuration reference](../reference/configuration) — every setting in one
  table.
- [Install the agent](installation) — packaging-specific install paths.
- [Configure gateway drain](gateway-drain) — recommended for HA deployments.

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
port-forward VIPs — DNAT rules, VIP addresses, and connmark return routing —
and never connects to OVN. Each managed VIP address is announced via BGP
through its connected route on `port_forward_dev` (the gateway's FRR must
`redistribute connected` through the `ANNOUNCED-NETWORKS` route-map — see the
[port-forwarding guide](port-forwarding#vip-address-management)), not through
an FRR static route.

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

## TLS for the OVN connections

The agent dials `ssl:` remotes over TLS when you set the `ovn_ssl_ca`,
`ovn_ssl_cert`, and `ovn_ssl_key` options. Use this to protect the
write-capable Northbound connection on the management network.

```yaml
ovn_sb_remote: "ssl:10.10.0.1:6642,ssl:10.10.0.2:6642,ssl:10.10.0.3:6642"
ovn_nb_remote: "ssl:10.10.0.1:6641,ssl:10.10.0.2:6641,ssl:10.10.0.3:6641"

ovn_ssl_ca: "/etc/ovn-network-agent/ca.pem"
ovn_ssl_cert: "/etc/ovn-network-agent/cert.pem"
ovn_ssl_key: "/etc/ovn-network-agent/key.pem"
```

An ovsdb-server started with `--ca-cert` requires a client certificate: set
both `ovn_ssl_cert` and `ovn_ssl_key` (they must be set together). With only
`ovn_ssl_ca`, the agent verifies the servers against your private CA but
presents no client certificate.

Set `ovn_ssl_ca` even when the servers use a publicly trusted certificate.
Without it the agent falls back to the host's system trust store, so any CA in
that store can impersonate the databases; the agent logs a warning at startup
when an `ssl:` remote is dialed with no CA pinned.

The private key authenticates the agent to the write-capable Northbound
database, so keep it readable only by the account the agent runs as:

```bash
chmod 0600 /etc/ovn-network-agent/key.pem
```

A group- or world-accessible `ovn_ssl_key` is a startup error naming the file
and its mode — check the mode your configuration management deploys, not just
the one you set by hand.

Use the same PKI as the rest of the deployment. The three options correspond
to Neutron's `[ovn] ovn_nb_ca_cert` / `ovn_nb_certificate` /
`ovn_nb_private_key` (and the `_sb_` equivalents) and to ovn-controller's `-C`
/ `-c` / `-p` flags — point the agent at the same CA and, if required, a
keypair issued by it.

The agent's Go TLS client verifies the server certificate against the host it
dials, so each server certificate must carry the remote's IP address or DNS
name as a subject alternative name (SAN). C ovs/ovn clients check only the CA
chain, so a certificate minted with `ovs-pki` without SANs passes
ovn-controller but fails the agent's handshake.

A single remote list must not mix `ssl:` and `tcp:` endpoints — the agent
rejects such a list at startup. libovsdb fails over between the entries
transparently, so one plaintext entry would let a connection silently
downgrade to cleartext. Schemes are matched case-insensitively, and a scheme
other than `ssl:`, `tcp:` or `unix:` is rejected at startup rather than at the
first connection attempt.

The NB and SB lists may use different schemes from each other, so you can
migrate one database at a time. While only one of them is on `ssl:`, the agent
logs a warning at startup so a half-finished migration is not mistaken for a
completed one — the cleartext database still carries the chassis and
`Port_Binding` data the agent turns into routes, FRR announcements, and NAT
rules.

The certificate files are read once at startup; there is no runtime reload
([#91](https://github.com/osism/ovn-network-agent/issues/91)). Restart the
agent after rotating them. An already-expired `ovn_ssl_cert` is loaded with a
warning rather than rejected — an established session survives its own expiry,
so the failure surfaces only at the next reconnect.

### Staying on plaintext tcp:

The Northbound connection is write-capable — the agent writes default routes,
static MAC bindings, and Gateway_Chassis priorities — so a plaintext `tcp:`
remote is a tampering and man-in-the-middle surface on the control plane. If
you cannot move to `ssl:` yet, reduce the exposure:

- Run the OVN databases on a dedicated management network.
- Restrict the listener via the ovsdb-server `Connection` row. For example,
  `ovn-nbctl set-connection ptcp:6641:<mgmt-ip>` binds the NB listener to the
  management address only.

## Prerequisites

- **OVN** (full mode only): TCP or TLS (`ssl:`) access to the OVN Southbound
  and Northbound databases on the control nodes (the agent runs on
  network/gateway nodes where no local DB sockets exist). Not needed in
  port-forward-only mode.
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
- A broken TLS certificate file — an unreadable or unparsable `ovn_ssl_ca`,
  `ovn_ssl_cert`, or `ovn_ssl_key` — is a startup error too, and
  `--check-config` loads and parses the same files, so it is caught before a
  restart rather than on the next start.
- An unknown config-file key (for example a typo like `drain_on_shutdow`) is
  logged as a warning naming the key and then ignored — the key is accepted so
  a newer config stays forward-compatible with an older agent, but the typo is
  no longer silent.

## Where to go next

- [Configuration reference](../reference/configuration) — every setting in one
  table.
- [Install the agent](installation) — packaging-specific install paths.
- [Configure gateway drain](gateway-drain) — recommended for HA deployments.

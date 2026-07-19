# First agent on a test host

This tutorial walks you through running `ovn-network-agent` on a single gateway
node end-to-end: building the binary, writing a minimal config, starting the
service, and verifying that routes appear. By the end you will have a single
agent watching a single OVN cluster and writing kernel routes plus FRR static
routes for the FIPs of its locally-active routers.

If you only want a quick task-oriented reference, jump to:

- [Install the agent](../guides/installation) — packaging-specific install paths.
- [Configure the agent](../guides/configuration) — all configuration mechanisms in one place.
- [Configuration reference](../reference/configuration) — every flag, env var, and config key.

## Prerequisites

Before you start, the host needs the following in place. These match the
[prerequisites listed in the explanation section](../explanation/architecture):

- **OVN** — TCP (or TLS `ssl:`) access to OVN Southbound and Northbound
  databases on the control nodes. The agent runs on network/gateway nodes where no local DB
  sockets exist.
- **FRR** — `vtysh` must be available and the VRF + BGP configuration must
  already exist.
- **Linux** — provider bridge (e.g. `br-ex`) must exist.
- **VRF route leaking** — the agent automatically creates and manages a veth
  pair connecting the default VRF to `vrf-provider` (enabled by default via
  `--veth-leak-enabled`). Per-network routes are reconciled dynamically based
  on auto-discovered or configured provider networks.
- **nftables** — `nft` binary must be in `PATH` (required for port forwarding
  / DNAT).
- **Permissions** — root or `CAP_NET_ADMIN` for netlink route manipulation.

::: tip
For an isolated test host you can use the helper script
`test/integration/setup.sh` which installs the packages, creates `br-ex`, and
configures FRR with a `vrf-provider`. See the
[integration tests guide](../contributing/integration-tests) for details.
:::

## Step 1 — Build the binary

Requires Go 1.26+ (see `go.mod` for the exact minimum).

```bash
# Standard build (linux)
make build

# Or a static binary for deployment
make build-static
```

Both targets produce a single binary `ovn-network-agent` in the working
directory.

## Step 2 — Write a minimal config

Create `/etc/ovn-network-agent/config.yaml` with the two required settings:

```yaml
ovn_sb_remote: "tcp:10.10.0.1:6642,tcp:10.10.0.2:6642,tcp:10.10.0.3:6642"
ovn_nb_remote: "tcp:10.10.0.1:6641,tcp:10.10.0.2:6641,tcp:10.10.0.3:6641"

# Optional: provider networks are auto-discovered from OVN when omitted
# network_cidr:
#   - "192.0.2.0/24"
#   - "198.51.100.0/24"
```

Every other setting has a sensible default; the full sample is at
[`ovn-network-agent.yaml.sample`](https://github.com/osism/ovn-network-agent/blob/main/ovn-network-agent.yaml.sample).

## Step 3 — Run the agent in the foreground

Start the agent with verbose logging and dry-run to safely watch what it would
do without actually touching the data plane:

```bash
sudo ./ovn-network-agent \
    --config /etc/ovn-network-agent/config.yaml \
    --log-level debug \
    --dry-run
```

You should see log lines for the OVN SB/NB connections, an initial reconcile,
and the list of locally-active routers (if any). CLI flags take precedence
over values in the config file.

## Step 4 — Run for real

Remove `--dry-run` and the agent starts writing routes:

```bash
sudo ./ovn-network-agent --config /etc/ovn-network-agent/config.yaml
```

In production, install the systemd unit and enable the service — see
[Install the agent](../guides/installation) for the packaging-specific paths.

## Step 5 — Verify

In another terminal, confirm the agent installed routes and bridge state:

```bash
# Kernel /32 routes for every FIP locally active on this chassis
ip route show dev br-ex

# Bridge link-local IP (default 169.254.169.254/32) and proxy_arp=1
ip addr show br-ex
cat /proc/sys/net/ipv4/conf/br-ex/proxy_arp

# OVS flows: MAC-tweak (cookie 0x999) and hairpin (cookie 0x998)
sudo ovs-ofctl dump-flows br-ex | grep -E '0x99[89]'

# FRR static routes inside vrf-provider
sudo vtysh -c 'show running-config' | grep -A 1 'vrf vrf-provider'
```

If you enabled the metrics endpoint, scrape it to see the reconcile counter
and OVN connection state:

```bash
ovn-network-agent --metrics-listen 127.0.0.1:9273 ...
curl -s http://127.0.0.1:9273/metrics | grep ovn_network_agent_
```

See the [metrics reference](../reference/metrics) for the full list and
suggested alerts.

## Where to go next

- [How the reconcile loop works](../explanation/how-it-works) — understand
  what each step is doing.
- [Architecture](../explanation/architecture) — control-plane and data-plane
  diagrams.
- [Create a gatewayless provider network](../guides/gatewayless-provider-network)
  — the most common day-1 setup task on the OpenStack side.

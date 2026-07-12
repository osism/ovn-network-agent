# Upgrade the agent

Upgrade a gateway fleet one node at a time. Because the agent drains HA
gateways on shutdown, restarting it to load a new binary is hitless by
design — you can roll a whole fleet without a connectivity gap. This guide
covers the rolling procedure, how to verify each node, mixed-version
fleets during the roll, and how to roll back.

For how the drain and takeover handshake work, see
[Gateway drain mode](../explanation/gateway-drain).

## Before you upgrade

- Read the release notes and the
  [CHANGELOG](https://github.com/osism/ovn-network-agent/blob/main/CHANGELOG.md)
  for the target version.
- Note the support policy: during the `0.x` phase there are **no
  backported fixes** — always move to the latest release rather than
  patching an older line. See the
  [security policy](https://github.com/osism/ovn-network-agent/blob/main/SECURITY.md).

## Upgrade one node at a time

Do the following on a single gateway node, verify it (below), and only
then move to the next.

1. Download the new package and install it over the running one:

   ```bash
   sudo apt install ./ovn-network-agent_<version>_<arch>.deb
   # or: sudo dpkg -i ovn-network-agent_<version>_<arch>.deb
   ```

2. Restart the service on this node so the new binary takes over:

   ```bash
   sudo systemctl restart ovn-network-agent
   ```

   The restart is hitless: the node drains its HA gateways first, holding
   its Floating IP routes until the takeover chassis has provably taken
   over (see
   [the takeover handshake](../explanation/gateway-drain#the-takeover-handshake)).
   Expect the command to block while the old process drains — up to
   `drain_timeout` (default 60s; the unit's `TimeoutStopSec` is 120s).

   The package does **not** restart automatically by default, because the
   drain is only hitless one node at a time: an `unattended-upgrades` or
   config-management fleet upgrade would restart every HA peer at once,
   their takeover handshakes would deadlock, and the Floating IPs would
   blackhole. If you drive this rolling procedure yourself (one node,
   verify, next node), you can let the package restart the service for you
   by setting `OVN_NETWORK_RESTART_ON_UPGRADE=true` in
   `/etc/default/ovn-network-agent`; leave it unset on any fleet updated in
   parallel.

3. State re-adoption is automatic on restart — you do not restore anything
   by hand. The agent re-adopts its kernel routes (tagged with route
   protocol `44`) and rebuilds its dedicated `ovn-network-agent` nftables
   table on startup, and it restores any gateways it drained.

`postinst` never fails the install: whenever it does not restart the
service — the default, or an opted-in restart that failed (for example a
bad config change landed alongside the upgrade) — it prints a notice that
a restart is pending. Run `sudo systemctl restart ovn-network-agent` once
you are ready (and once any cause is fixed).

## Verify the node

Confirm the running version matches the installed one before moving on:

```bash
# Installed package version
dpkg-query -W -f='${Version}\n' ovn-network-agent

# Binary version
/usr/bin/ovn-network-agent --version

# Running version — the 'version' field of the startup line
sudo journalctl -u ovn-network-agent | grep 'ovn-network-agent starting' | tail -1

# Service is active
systemctl is-active ovn-network-agent
```

Optionally confirm Floating IP reachability from a client and check the
[metrics endpoint](./metrics) before proceeding to the next node.

## Mixed-version fleets

While the fleet is half-upgraded, a peer still running a pre-handshake
agent never writes the readiness marker, so a drain on an upgraded node
waits the full `drain_timeout` before falling back. This is safe — the
node never releases its routes early, it is only slower. If you need
faster drains during the upgrade window, temporarily lower `drain_timeout`
or set `drain_settle_delay: 0` (see
[Configure gateway drain](./gateway-drain)).

## Downgrade / rollback

Install the older package to roll back:

```bash
sudo apt install --allow-downgrades ./ovn-network-agent_<older>_<arch>.deb
# or: sudo dpkg -i ovn-network-agent_<older>_<arch>.deb
```

- The package does not restart on `configure` unless
  `OVN_NETWORK_RESTART_ON_UPGRADE=true` (see above), so after a downgrade
  run `sudo systemctl restart ovn-network-agent` manually — one node at a
  time — so the running binary matches the installed one.
- No state cleanup is required. The NB `external_ids` marker
  (`ovn-network-agent-advertised`) that a newer agent writes for the
  takeover handshake is never read by an older agent — a downgraded or
  mixed-version fleet simply loses the handshake speed-up, never
  correctness.
- `/etc/default/ovn-network-agent` is a conffile and survives both
  upgrades and downgrades; your environment overrides are preserved.

## FRR coupling

The unit declares `Wants=frr.service` (not `Requires=`), so FRR package
upgrades and restarts do **not** restart or drain the agent. If FRR is
briefly unavailable, route writes that fail are logged and retried on the
next reconcile (default 60s); no manual action is needed.

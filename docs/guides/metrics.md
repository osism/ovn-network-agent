# Enable metrics and alerts

The agent can expose Prometheus-formatted metrics on an optional HTTP
endpoint. The endpoint is **off by default**; bind to `127.0.0.1` for
node-local scraping, or to `0.0.0.0` for a remote scraper.

For the complete metric catalogue (names, types, labels), see the
[metrics reference](../reference/metrics).

## Enable the endpoint

Set `--metrics-listen` (or `OVN_NETWORK_METRICS_LISTEN` / `metrics_listen`)
to a `host:port` such as `127.0.0.1:9273`:

```bash
ovn-network-agent --metrics-listen 127.0.0.1:9273
curl -s http://127.0.0.1:9273/metrics
```

Three paths are served:

- `/metrics` — Prometheus exposition format.
- `/healthz` — always returns `200 ok` while the process is up (liveness
  only).
- `/readyz` — returns `200` only when the agent is functional (OVN
  connected, last reconcile succeeded); `503` otherwise.

All metrics are prefixed with `ovn_network_agent_`.

## Health and readiness probes

The listener serves two orthogonal signals. `/healthz` is pure liveness: it
returns `200 ok` whenever the process is up. `/readyz` is readiness — it
returns `200` only when the agent is actually functional, and `503`
otherwise.

`/readyz` reports ready only when all of these hold:

- OVN NB and SB are connected. This check is skipped in port-forward-only
  mode, where there is no OVN client.
- At least one reconcile cycle has completed.
- The last reconcile's route sync succeeded.

On a `503`, the body carries one `unready: …` line per failing check, so you
can read the reason without opening the logs. The possible lines are
`unready: ovn nb disconnected`, `unready: ovn sb disconnected`,
`unready: awaiting first reconcile`, and `unready: last reconcile failed`.

Probe both endpoints with curl:

```bash
curl -i http://127.0.0.1:9273/healthz
curl -i http://127.0.0.1:9273/readyz
```

A ready agent answers `200 ok`. When `/readyz` returns `503`, read the
`unready:` lines in the body to see which check is failing.

Wire the probes into your platform's health checks, and treat `/healthz` as
the restart signal and `/readyz` as the traffic/serving signal. A
Kubernetes-style HTTP readiness probe, for example, checks `/readyz` and
expects `200`:

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 9273
  periodSeconds: 10
readinessProbe:
  httpGet:
    path: /readyz
    port: 9273
  periodSeconds: 10
```

Both probes exist only when `metrics_listen` is set, which is off by default
(see the [configuration reference](../reference/configuration)).

The systemd unit intentionally stays `Type=simple`. A start-gating `READY=1`
(`Type=notify`) would conflict with the agent's deliberate connect-retry
startup contract: when OVN is unreachable on cold start the agent keeps
retrying rather than failing its unit, which systemd would only restart in a
tight loop anyway. A health-tied watchdog would likewise make systemd
restart-loop a degraded-but-recovering agent. Monitor readiness from the
monitoring plane via `/readyz` instead.

## Suggested alerts

The complete, generated alert list lives in the
[metrics reference](../reference/metrics#suggested-alerts). Ready-to-load
rules implementing them ship in
[`contrib/prometheus-rules.yaml`](https://github.com/osism/ovn-network-agent/blob/main/contrib/prometheus-rules.yaml),
and each alert has a cause→diagnosis→remediation section in the
[troubleshooting guide](./troubleshooting). This page deliberately does not
reproduce the expressions, so it can never drift from the reference.

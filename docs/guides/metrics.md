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

Two paths are served:

- `/metrics` — Prometheus exposition format.
- `/healthz` — returns `200 ok` for liveness probes.

All metrics are prefixed with `ovn_network_agent_`.

## Suggested alerts

- `consecutive_readds >= 3` — persistent route instability (FRR or kernel
  races).
- `ovn_connection_state{database="nb"} == 0` for >2m — NB DB unreachable;
  agent cannot write OVN state.
- `rate(route_readds_total[10m]) > 0` — flapping routes.
- `histogram_quantile(0.95, rate(failover_announce_seconds_bucket[1h])) > 2`
  — failover announces slower than the ~2s failover budget.
- `histogram_quantile(0.95, rate(reconcile_duration_seconds_bucket[5m])) > 5`
  — slow reconciles.

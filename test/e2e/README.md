# Containerlab E2E harness

The E2E test harness is documented at
**<https://osism.github.io/ovn-network-agent/contributing/e2e-tests>**.

The Markdown source lives at
[`docs/contributing/e2e-tests.md`](../../docs/contributing/e2e-tests.md);
edit that file when adding new scenarios or changing local-run
instructions.

Quick reference:

```sh
make e2e-install-tools   # one-time, installs containerlab on Linux
make e2e-up              # build images, deploy topology, seed OVN NB
make e2e-baseline        # run the baseline reachability scenario
make e2e-failover        # run the HA failover scenario (master chassis loss)
make e2e-failover-strict # run the failover scenario with the strict ~2s outage budget
make e2e-chaos           # run a seeded 10-minute chaos session
make e2e-down            # destroy the lab
```

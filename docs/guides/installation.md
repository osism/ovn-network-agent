# Install the agent

Pre-built binaries and Debian packages for `amd64` and `arm64` are available on
the [GitHub Releases](https://github.com/osism/ovn-network-agent/releases) page.

Pick the method that matches how you manage software on the target host.

## Debian package

```bash
# Download the .deb package (replace VERSION and ARCH as needed)
curl -LO https://github.com/osism/ovn-network-agent/releases/download/vVERSION/ovn-network-agent_VERSION_ARCH.deb

# Example: v0.1.0, amd64
curl -LO https://github.com/osism/ovn-network-agent/releases/download/v0.1.0/ovn-network-agent_0.1.0_amd64.deb

# Install
sudo dpkg -i ovn-network-agent_0.1.0_amd64.deb
```

The package installs:

- `/usr/bin/ovn-network-agent` — the binary
- `/lib/systemd/system/ovn-network-agent.service` — systemd service
- `/etc/default/ovn-network-agent` — environment defaults (preserved on upgrade)
- `/etc/ovn-network-agent/config.yaml.sample` — sample configuration

After installation, create your configuration and start the service:

```bash
sudo cp /etc/ovn-network-agent/config.yaml.sample /etc/ovn-network-agent/config.yaml
sudo vi /etc/ovn-network-agent/config.yaml
sudo systemctl enable --now ovn-network-agent
```

## Binary

```bash
# Download the static binary (replace ARCH as needed: amd64 or arm64)
curl -LO https://github.com/osism/ovn-network-agent/releases/download/vVERSION/ovn-network-agent-linux-ARCH

# Example: v0.1.0, amd64
curl -LO https://github.com/osism/ovn-network-agent/releases/download/v0.1.0/ovn-network-agent-linux-amd64

# Install
sudo install -m 0755 ovn-network-agent-linux-amd64 /usr/bin/ovn-network-agent
```

Set up the systemd service and configuration manually:

```bash
sudo cp ovn-network-agent.service /etc/systemd/system/
sudo cp ovn-network-agent.default /etc/default/ovn-network-agent

sudo mkdir -p /etc/ovn-network-agent
sudo cp ovn-network-agent.yaml.sample /etc/ovn-network-agent/config.yaml
sudo vi /etc/ovn-network-agent/config.yaml

sudo systemctl daemon-reload
sudo systemctl enable --now ovn-network-agent
```

## From source

Requires Go 1.26+ (see `go.mod` for the exact minimum).

```bash
make build-static
sudo install -m 0755 ovn-network-agent /usr/bin/ovn-network-agent
```

Other available Makefile targets:

```bash
# Standard build (linux)
make build

# Run tests
make test

# Lint
make fmt
make vet

# Install to /usr/bin
sudo make install
```

Produces a single binary `ovn-network-agent`.

## Check status

```bash
sudo systemctl status ovn-network-agent
sudo journalctl -u ovn-network-agent -f
```

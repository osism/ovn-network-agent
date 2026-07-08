BINARY    := ovn-network-agent
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOFLAGS   := -trimpath

.PHONY: all build build-static clean fmt vet test test-integration install docs-gen docs-gen-check e2e-images e2e-up e2e-down e2e-install-tools e2e-baseline e2e-failover e2e-hairpin e2e-multi-vlan e2e-pf-external e2e-pf-hairpin e2e-stale-chassis

# Containerlab E2E harness. See test/e2e/README.md for the topology and
# acceptance criteria (issue #44).
E2E_TOPOLOGY    := test/e2e/topology.clab.yml
E2E_BOOTSTRAP   := test/e2e/bootstrap.sh
E2E_BASELINE    := test/e2e/scenarios/baseline.sh
E2E_FAILOVER    := test/e2e/scenarios/failover.sh
E2E_HAIRPIN     := test/e2e/scenarios/hairpin.sh
E2E_MULTI_VLAN  := test/e2e/scenarios/multi-vlan.sh
E2E_PF_EXTERNAL := test/e2e/scenarios/pf-external.sh
E2E_PF_HAIRPIN  := test/e2e/scenarios/pf-hairpin.sh
E2E_STALE       := test/e2e/scenarios/stale-chassis.sh
E2E_GWNODE_TAG  := ovn-network-agent/gwnode:e2e
E2E_CENTRAL_TAG := ovn-network-agent/central:e2e

all: build

build:
	GOOS=linux go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) .

# Static binary for deployment on minimal systems
build-static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) .

fmt:
	go fmt ./...

vet:
	go vet ./...

test:
	go test -v ./...

# Integration tests exercise the agent against a real OVN/OVS/FRR/nftables
# stack. They require Linux + root (CAP_NET_ADMIN). See
# docs/contributing/integration-tests.md (published at
# https://osism.github.io/ovn-network-agent/contributing/integration-tests)
# for local-run prerequisites.
test-integration: build
	OVN_AGENT_BINARY=$(CURDIR)/$(BINARY) go test -tags=integration -v -count=1 ./test/integration/...

clean:
	rm -f $(BINARY)

install: build
	install -m 0755 $(BINARY) /usr/local/bin/$(BINARY)

# Regenerate docs/reference/{configuration,cli,metrics}.md from the
# canonical Go declarations in config.go and metrics.go. See
# tools/docgen for the implementation. Run after touching either
# file or the agent's flag/env/YAML surface.
docs-gen:
	go run ./tools/docgen

# Fail if the generated reference pages are out of date. Used in CI
# so PRs that touch config.go or metrics.go without regenerating the
# reference docs are caught before merge.
docs-gen-check: docs-gen
	@git diff --exit-code -- docs/reference/ || ( \
		echo ""; \
		echo "docs/reference/ is out of date — run 'make docs-gen' and commit the result."; \
		exit 1; \
	)

# Build the containerlab E2E images for the host platform. The gwnode
# Dockerfile builds the agent from source via a Go build stage, so no
# pre-build of the binary is required. Plain `docker build` is used so
# the target works on hosts without the docker-buildx-plugin (which is
# only required for multi-arch publication, documented in
# docs/contributing/e2e-tests.md).
e2e-images:
	docker build -f test/e2e/Dockerfile.central -t $(E2E_CENTRAL_TAG) .
	docker build -f test/e2e/Dockerfile.gwnode  -t $(E2E_GWNODE_TAG)  .

# Install the containerlab CLI when it is missing. Linux only: the
# upstream project publishes Linux binaries and a one-line installer
# (https://get.containerlab.dev) for Debian/RHEL hosts and ships no
# darwin binary, so on macOS the recommended path is to run
# containerlab inside a Linux VM — see
# https://containerlab.dev/macos/. This target reports that and
# exits with a non-zero status on macOS instead of pretending to
# install something.
e2e-install-tools:
	@if command -v containerlab >/dev/null 2>&1; then \
		echo "containerlab already installed: $$(command -v containerlab)"; \
	elif [ "$$(uname -s)" = "Linux" ]; then \
		echo "installing containerlab via the upstream installer (needs sudo)"; \
		bash -c "$$(curl -sL https://get.containerlab.dev)"; \
	elif [ "$$(uname -s)" = "Darwin" ]; then \
		echo ""; \
		echo "containerlab does not ship a native macOS binary."; \
		echo "Run the E2E lab from a Linux host or a Linux VM"; \
		echo "(OrbStack, Docker Desktop's Linux VM, Colima, ...)"; \
		echo "See https://containerlab.dev/macos/ for the recommended setup."; \
		exit 1; \
	else \
		echo "unsupported platform $$(uname -s); install containerlab manually from https://containerlab.dev/install/"; \
		exit 1; \
	fi

# Bring the containerlab E2E lab up: build the images, deploy the
# topology, and seed the OVN NB DB with the canned state. Errors out
# with a pointer to `make e2e-install-tools` when containerlab is
# missing.
e2e-up: e2e-images
	@command -v containerlab >/dev/null 2>&1 || ( \
		echo ""; \
		echo "containerlab is not on PATH."; \
		echo "Run 'make e2e-install-tools' once to install it, then retry."; \
		exit 1; \
	)
	containerlab deploy -t $(E2E_TOPOLOGY)
	$(E2E_BOOTSTRAP)

# Tear the containerlab E2E lab down.
e2e-down:
	containerlab destroy -t $(E2E_TOPOLOGY) --cleanup

# Run the baseline reachability scenario (issue #45) against a lab that
# is already up. Mirrors the step the CI workflow runs, so that a
# `make e2e-up && make e2e-baseline && make e2e-down` cycle on a dev
# machine reproduces the CI path exactly.
e2e-baseline:
	$(E2E_BASELINE)

# Run the HA failover scenario (issue #105) against a lab that is
# already up. Stops the priority-30 chassis to trigger OVN HA
# re-election and waits for reachability through cr-lr0-public to
# recover within FAILOVER_TIMEOUT (default 30s). Mirrors the step the
# CI workflow runs; the scenario's own EXIT trap restores the lab to
# baseline state so a subsequent `make e2e-baseline` works without
# tearing the lab down.
e2e-failover:
	$(E2E_FAILOVER)

# Run the same-chassis hairpin scenario (issue #108) against a lab
# that is already up. Adds a second FIP backend (ls0-vm2 / 192.0.2.12)
# co-located on the active master, asserts the agent installs the
# OpenFlow hairpin rule (cookie 0x998) on br-ex for both FIPs, and
# pings the new FIP from the existing workload netns to exercise the
# hairpin data path end-to-end. Mirrors the step the CI workflow runs;
# the scenario's own EXIT trap removes the second FIP so a subsequent
# `make e2e-baseline` works without tearing the lab down.
e2e-hairpin:
	$(E2E_HAIRPIN)

# Run the multi-VLAN provider-network scenario (issue #147) against a
# lab that is already up. Adds two VLAN provider networks (tags 101/102
# on physnet1), each with a gatewayless public subnet, a router pinned
# to gateway-1, and a netns workload behind a FIP. Asserts the agent
# creates one kernel subinterface per segment (br-ex.101/br-ex.102),
# routes each FIP /32 over its own segment interface, installs
# MAC-tweak flows per localnet patch port, and announces both FIPs via
# BGP — then probes both FIPs plus the flat baseline FIP from client-1.
# The scenario's own EXIT trap removes the added networks so a
# subsequent `make e2e-baseline` works without tearing the lab down.
e2e-multi-vlan:
	$(E2E_MULTI_VLAN)

# Run the port-forward / DNAT scenario (issue #109) against a lab
# that is already up. Adds an OVN Load_Balancer for 192.0.2.50:80 →
# ls0-vm1:8080 on lr0, starts a tiny HTTP backend in the existing vm1
# netns, curls the VIP from client-1, and asserts the backend log
# records client-1's underlay IP — i.e. OVN performed pure DNAT with
# no SNAT on the way in. Mirrors the step the CI workflow runs; the
# scenario's own EXIT trap removes the Load_Balancer, the per-chassis
# kernel route, the upstream static route, and the backend process,
# so a subsequent `make e2e-baseline` keeps passing.
e2e-pf-external:
	$(E2E_PF_EXTERNAL)

# Run the port-forward hairpin scenario (issue #110) against a lab that
# is already up. Adds a co-located workload (vmc behind FIP_C on
# gateway-1) and a tenant-shim OVS internal port that gives the
# chassis kernel a routed path into ls0, then drives two phases
# against the agent's `hairpin_masquerade` flag: phase 1 (off) must
# time out because the backend reply bypasses the chassis conntrack,
# phase 2 (on) must succeed because the masquerade rule re-routes the
# reply through the chassis. Each phase restarts the gateway-1
# container so the agent reloads its config; the scenario's own EXIT
# trap restores the baseline agent config and removes every NB/kernel
# row added here so a subsequent `make e2e-baseline` keeps passing.
e2e-pf-hairpin:
	$(E2E_PF_HAIRPIN)

# Run the stale-chassis cleanup scenario (issue #111) against a lab
# that is already up. Hard-kills the priority-30 chassis (SIGKILL, no
# graceful agent shutdown) and asserts that NB rows tagged for the
# dead chassis are removed by surviving peers within
# stale_chassis_grace_period + a margin for jitter and reconcile
# cadence. Mirrors the step the CI workflow runs; the scenario's own
# EXIT trap restarts the killed chassis and waits for baseline-green.
e2e-stale-chassis:
	$(E2E_STALE)

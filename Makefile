BINARY    := ovn-network-agent
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOFLAGS   := -trimpath

.PHONY: all build build-static build-integration clean fmt vet test test-integration install docs-gen docs-gen-check models-gen models-gen-check e2e-images e2e-up e2e-down e2e-install-tools e2e-baseline e2e-failover e2e-failover-strict e2e-hairpin e2e-multi-vlan e2e-pf-external e2e-pf-hairpin e2e-stale-chassis e2e-drain-hitless e2e-chaos e2e-chaos-report

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
E2E_DRAIN       := test/e2e/scenarios/drain-hitless.sh
E2E_CHAOS       := go run ./test/e2e/chaos
E2E_GWNODE_TAG  := ovn-network-agent/gwnode:e2e
E2E_CENTRAL_TAG := ovn-network-agent/central:e2e

# Pinned containerlab release for CI and local installs. The sha256
# values are the linux_{amd64,arm64}.deb lines from the upstream
# release's checksums.txt. Bump the version and BOTH checksums together:
#   https://github.com/srl-labs/containerlab/releases
CONTAINERLAB_VERSION      := 0.77.0
CONTAINERLAB_SHA256_amd64 := 675eea8bd4d05ea3abc4a98cfa859975c9886705d6a510fead4dfd8dbed8b793
CONTAINERLAB_SHA256_arm64 := 9bfd89d1afbff87c316febc721eb960148420ec94f3e063ba54b13ef38e8ba60

all: build

build:
	GOOS=linux go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) .

# Static binary for deployment on minimal systems
build-static:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BINARY) .

# Instrumented agent binary for the CI integration job: coverage counters
# (-cover, so integration-only Linux code shows up in the merged figure) plus
# the race detector (-race, which drives refreshLoop / drainWatchCh / event
# handlers under real OVSDB event storms). -covermode=atomic is required with
# -race, and -race needs cgo, so this builds only on a Linux host — it is not a
# darwin cross-build.
build-integration:
	GOOS=linux go build $(GOFLAGS) -cover -covermode=atomic -race -ldflags '$(LDFLAGS)' -o $(BINARY) .

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
	OVN_AGENT_BINARY=$(CURDIR)/$(BINARY) go test -tags=integration -v -count=1 -timeout 25m ./test/integration/...

clean:
	rm -f $(BINARY)

install: build
	install -m 0755 $(BINARY) /usr/bin/$(BINARY)

# Regenerate docs/reference/{configuration,cli,metrics}.md from the
# canonical Go declarations in config.go and metrics.go. See
# tools/docgen for the implementation. Run after touching either
# file or the agent's flag/env/YAML surface. The generator also fails
# if ovn-network-agent.default or ovn-network-agent.yaml.sample misses
# an option the agent consumes.
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

# Regenerate the OVSDB models from the OVN schemas checked in under
# schemas/. Run this after bumping a schema; see
# docs/contributing/ovsdb-models.md.
models-gen:
	go run github.com/ovn-kubernetes/libovsdb/cmd/modelgen -p nbdb -o internal/nbdb schemas/ovn-nb.ovsschema
	go run github.com/ovn-kubernetes/libovsdb/cmd/modelgen -p sbdb -o internal/sbdb schemas/ovn-sb.ovsschema
	gofmt -w internal/nbdb internal/sbdb

# Fail if the generated models are out of date with respect to the
# checked-in schemas. Used in CI so a schema bump without a regen — or a
# hand-edit of generated code — is caught before merge.
models-gen-check: models-gen
	@git diff --exit-code -- internal/nbdb internal/sbdb || ( \
		echo ""; \
		echo "internal/nbdb or internal/sbdb is out of date — run 'make models-gen' and commit the result."; \
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

# Install the containerlab CLI when it is missing. Linux/Debian only:
# the pinned .deb (CONTAINERLAB_VERSION above) is downloaded and its
# sha256 verified against the committed checksum before apt-get installs
# it — replacing the unpinned `curl | bash` installer so CI and local
# installs get a known, verified binary. This narrows the target to
# dpkg-based distros (what CI and the documented dev setup use); on
# other distros it errors with a pointer to the upstream install docs.
# Upstream ships no darwin binary, so on macOS the recommended path is
# to run containerlab inside a Linux VM (https://containerlab.dev/macos/)
# and this target reports that instead of pretending to install
# something.
e2e-install-tools:
	@set -e; \
	if command -v containerlab >/dev/null 2>&1; then \
		echo "containerlab already installed: $$(command -v containerlab)"; \
	elif [ "$$(uname -s)" = "Linux" ]; then \
		if ! command -v dpkg >/dev/null 2>&1 || ! command -v apt-get >/dev/null 2>&1; then \
			echo "the pinned .deb install needs dpkg/apt-get (Debian/Ubuntu)."; \
			echo "On other distributions install containerlab $(CONTAINERLAB_VERSION)"; \
			echo "manually from https://containerlab.dev/install/."; \
			exit 1; \
		fi; \
		arch=$$(dpkg --print-architecture); \
		case "$$arch" in \
			amd64) sha=$(CONTAINERLAB_SHA256_amd64) ;; \
			arm64) sha=$(CONTAINERLAB_SHA256_arm64) ;; \
			*) echo "no pinned containerlab .deb for dpkg architecture $$arch;"; \
			   echo "install it manually from https://containerlab.dev/install/."; \
			   exit 1 ;; \
		esac; \
		deb="containerlab_$(CONTAINERLAB_VERSION)_linux_$$arch.deb"; \
		url="https://github.com/srl-labs/containerlab/releases/download/v$(CONTAINERLAB_VERSION)/$$deb"; \
		tmp=$$(mktemp -d); \
		trap 'rm -rf "$$tmp"' EXIT; \
		echo "installing containerlab $(CONTAINERLAB_VERSION) ($$arch) from the pinned .deb (needs sudo)"; \
		curl -fsSL -o "$$tmp/$$deb" "$$url"; \
		echo "$$sha  $$tmp/$$deb" | sha256sum -c -; \
		sudo apt-get install -y "$$tmp/$$deb"; \
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

# Run the HA failover scenario with the strict outage-budget assertion
# (issue #131): a 0.1s-spaced ping flood from client-1 is captured with
# tcpdump across the re-election, and the largest reply gap must stay
# within LOSS_BUDGET seconds (default 2). Mirrors the failover-strict
# leg of the CI failover job; runs the same failover.sh, only with the
# strict variant on.
e2e-failover-strict:
	LOSS_BUDGET=2 $(E2E_FAILOVER)

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

# Run the graceful-drain vs hard-kill hitless scenario (issue #113)
# against a lab that is already up and deployed with the drain armed
# (`E2E_DRAIN_ON_SHUTDOWN=true make e2e-up` — the scenario fail-fasts
# otherwise). Runs two arms back-to-back: a
# `kill -TERM` on the priority-30 chassis's agent (proves the drain
# committed by matching both the `drain: gateway chassis priority
# lowered` and the `drain: complete` log lines) and a
# `docker kill -s KILL` control arm. Between the two arms the
# scenario itself runs `make e2e-down && make e2e-up` so both arms
# start from identical priority-30/20/10 baseline state. Asserts the
# graceful outage stays within GRACEFUL_MAX_OUTAGE_MS (default 500 ms,
# calibrated to the measured CI floor — see the scenario header) and
# `graceful_loss < hardkill_loss`.
# The scenario's EXIT trap recycles the lab again on success so a
# developer run leaves the lab baseline-green.
e2e-drain-hitless:
	$(E2E_DRAIN)

# Run a seeded chaos session (issue #176) against a lab that is already
# up. Layers the hairpin, multi-VLAN and port-forward scenario setups on
# the bootstrap baseline, then drives the lab through a randomized fault
# sequence: every tick waits a random interval, picks a weighted action,
# checks its guardrails and executes it, while a continuous probe from
# client-1 measures every FIP and the port-forward VIP. The action catalog
# (issue #178) spans container-level starter faults and a config change,
# control-plane outages, management-path impairment, data-plane drift,
# routing flaps and OVN churn. Defaults: seed 42, 10 minutes, 10–30 s
# between decisions.
#
# Reproducibility is the contract: the seed, the duration, the tick
# bounds and the action weights are the only inputs, and every decision
# derives from the seed — so two runs with the same flags replay the
# identical action sequence. Pass flags through CHAOS_FLAGS:
#
#   make e2e-chaos CHAOS_FLAGS="-duration 3m -seed 7 -out /tmp/chaos-a"
#
# The journal (every decision) and the run summary (actions, probe loss
# over time, recovery durations, violations) land under -out; a run that
# recorded a violation exits 1 and dumps the lab state via
# collect-artifacts.sh, exactly like the scenario jobs do.
CHAOS_FLAGS ?=
e2e-chaos:
	$(E2E_CHAOS) $(CHAOS_FLAGS)

# Render a recorded chaos run as a Markdown report: the verdict, the
# slowest recoveries against their budgets, and every probe-loss window
# attributed to the fault it overlapped. CHAOS_RUN is the run directory
# (or summary.json) a run wrote with -out, or a GitHub Actions run URL —
# the URL form fetches the artifacts with `gh run download`, so `gh`
# must be installed and authenticated. The CI workflow renders the same
# report into each chaos job's summary.
#
#   make e2e-chaos-report CHAOS_RUN=/tmp/chaos-a
#   make e2e-chaos-report CHAOS_RUN=https://github.com/osism/ovn-network-agent/actions/runs/<id>
CHAOS_RUN ?= chaos-artifacts
e2e-chaos-report:
	$(E2E_CHAOS) -report $(CHAOS_RUN)

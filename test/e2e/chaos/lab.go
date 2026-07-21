package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// commander is the seam between the chaos runner and the host CLIs it
// drives (docker, containerlab). Everything the runner does to the lab
// funnels through here, so the unit tests replace exactly this one
// interface and exercise the real engine, actions and state layering
// against recorded argv.
type commander interface {
	run(ctx context.Context, name string, args ...string) (string, error)
}

// cmdTimeout bounds one host CLI invocation. The runner drives a lab it
// is actively breaking, where a `docker exec` can hang for good — and
// none of the contexts reaching here carries a deadline of its own. An
// unbounded invocation would wedge whichever goroutine issued it: a
// prober frozen mid-sample keeps reporting its last value, so a data path
// that is actually dead reads as green and the engine declares the node
// converged.
const cmdTimeout = 30 * time.Second

// execCommander runs the host CLIs. timeout bounds one invocation; the
// zero value uses cmdTimeout.
type execCommander struct {
	timeout time.Duration
}

// run returns the command's stdout. stderr is kept out of it and only
// folded into the error: every caller parses the output (OVSDB JSON,
// docker -f templates, chassis names), and ovn-sbctl logs its ovsdb-idl
// reconnect and transaction warnings to stderr while still exiting 0 —
// exactly what a chassis the runner has just SIGKILLed provokes. Merged
// into stdout, one WARN line turns a healthy lookup into a parse error or
// a corrupted UUID.
func (c execCommander) run(ctx context.Context, name string, args ...string) (string, error) {
	timeout := c.timeout
	if timeout == 0 {
		timeout = cmdTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name,
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// underlayLink mirrors one row of the UNDERLAY_LINKS table in
// bootstrap.sh: the /30 point-to-point link between a gateway's eth1
// (in vrf-provider) and its dedicated interface on `upstream`.
type underlayLink struct {
	gateway       string
	gatewayCIDR   string
	upstreamIface string
	upstreamCIDR  string
}

var underlayLinks = []underlayLink{
	{"gateway-1", "100.64.1.2/30", "eth1", "100.64.1.1/30"},
	{"gateway-2", "100.64.2.2/30", "eth2", "100.64.2.1/30"},
	{"gateway-3", "100.64.3.2/30", "eth3", "100.64.3.1/30"},
}

func linkFor(gateway string) (underlayLink, bool) {
	for _, l := range underlayLinks {
		if l.gateway == gateway {
			return l, true
		}
	}
	return underlayLink{}, false
}

func gatewayNames() []string {
	names := make([]string, 0, len(underlayLinks))
	for _, l := range underlayLinks {
		names = append(names, l.gateway)
	}
	return names
}

const (
	// The lab's fixed cast, as bootstrap.sh seeds it.
	centralNode  = "central"
	upstreamNode = "upstream"
	clientNode   = "client-1"
	workloadHost = "gateway-3"
	crPort       = "cr-lr0-public"
	lrPublicPort = "lr0-public"

	// The agent as the gwnode image ships it: the binary, the config file
	// the entrypoint execs it with, and the two paths the chaos runner
	// owns next to it — the staged config it validates before swapping,
	// and the marker that tells the entrypoint a profile owns the file.
	agentBinary         = "/usr/local/bin/ovn-network-agent"
	agentConfigPath     = "/etc/ovn-network-agent/config.yaml"
	agentConfigNextPath = "/etc/ovn-network-agent/config.next.yaml"
	profileMarkerPath   = "/etc/ovn-network-agent/chaos-profile"

	// The port-forward VIP from pf-external.sh. The agent does not
	// propagate Load_Balancer VIPs into the underlay, so the runner
	// keeps the two routes pointed at the current master itself — see
	// ensureVIPRouting.
	vipAddr = "192.0.2.50"
	vipURL  = "http://192.0.2.50:80/"

	// Readiness budget for the daemons a restarted gateway brings back
	// up (OVS, then FRR) before rewireUnderlay may talk to them.
	daemonReadyTimeout = 60 * time.Second
)

// lab drives one deployed containerlab lab. Poll loops take their clock
// from sleep/now so the unit tests can run them without wall-clock time.
type lab struct {
	name  string
	cmd   commander
	sleep func(time.Duration)
	now   func() time.Time
}

func newLab(name string, cmd commander) *lab {
	return &lab{name: name, cmd: cmd, sleep: time.Sleep, now: time.Now}
}

// node maps a topology node name to its container name.
func (l *lab) node(name string) string { return "clab-" + l.name + "-" + name }

func (l *lab) docker(ctx context.Context, args ...string) (string, error) {
	return l.cmd.run(ctx, "docker", args...)
}

// exec runs argv inside a lab container.
func (l *lab) exec(ctx context.Context, node string, argv ...string) (string, error) {
	return l.docker(ctx, append([]string{"exec", l.node(node)}, argv...)...)
}

// sh runs a shell snippet inside a lab container. Every snippet in this
// package is assembled from the static tables above, never from
// operator input, so the argv-level `sh -euc` form is safe and keeps
// the commander seam to a single method.
func (l *lab) sh(ctx context.Context, node, script string) (string, error) {
	return l.exec(ctx, node, "sh", "-euc", script)
}

func (l *lab) nbctl(ctx context.Context, args ...string) (string, error) {
	return l.exec(ctx, centralNode, append([]string{"ovn-nbctl"}, args...)...)
}

func (l *lab) sbctl(ctx context.Context, args ...string) (string, error) {
	return l.exec(ctx, centralNode, append([]string{"ovn-sbctl"}, args...)...)
}

// stopController / startController reproduce failover.sh's chassis-loss
// simulation: a clean ovn-controller shutdown releases the claim on
// cr-lr0-public without tearing the container (and its containerlab
// veths) down.
func (l *lab) stopController(ctx context.Context, gw string) error {
	if _, err := l.exec(ctx, gw, "/usr/share/ovn/scripts/ovn-ctl", "stop_controller"); err != nil {
		return fmt.Errorf("stop ovn-controller on %s: %w", gw, err)
	}
	return nil
}

func (l *lab) startController(ctx context.Context, gw string) error {
	if _, err := l.exec(ctx, gw, "/usr/share/ovn/scripts/ovn-ctl", "start_controller"); err != nil {
		return fmt.Errorf("start ovn-controller on %s: %w", gw, err)
	}
	return nil
}

// setRestartPolicy flips the container's docker restart policy.
// containerlab deploys with `restart: always`, so a kill would be
// undone by docker before the runner observes the outage —
// drain-hitless.sh disables the policy for the same reason.
func (l *lab) setRestartPolicy(ctx context.Context, gw, policy string) error {
	if _, err := l.docker(ctx, "update", "--restart="+policy, l.node(gw)); err != nil {
		return fmt.Errorf("set restart policy %q on %s: %w", policy, gw, err)
	}
	return nil
}

func (l *lab) killGateway(ctx context.Context, gw string) error {
	if _, err := l.docker(ctx, "kill", "-s", "KILL", l.node(gw)); err != nil {
		return fmt.Errorf("kill %s: %w", gw, err)
	}
	return nil
}

// terminateAgent signals the agent with SIGTERM through PID 1 — tini,
// which forwards the signal to the agent it supervises and follows it
// down (the same handle drain-hitless.sh uses). Whether the agent drains
// before exiting depends on how the lab was deployed
// (E2E_DRAIN_ON_SHUTDOWN).
func (l *lab) terminateAgent(ctx context.Context, gw string) error {
	if _, err := l.exec(ctx, gw, "kill", "-TERM", "1"); err != nil {
		return fmt.Errorf("terminate agent on %s: %w", gw, err)
	}
	return nil
}

func (l *lab) startGateway(ctx context.Context, gw string) error {
	if _, err := l.docker(ctx, "start", l.node(gw)); err != nil {
		return fmt.Errorf("start %s: %w", gw, err)
	}
	return nil
}

func (l *lab) restartGateway(ctx context.Context, gw string) error {
	if _, err := l.docker(ctx, "restart", l.node(gw)); err != nil {
		return fmt.Errorf("restart %s: %w", gw, err)
	}
	return nil
}

// containerRunning reports whether the container is up. Used to wait
// for a SIGTERM'ed agent to actually take the container down. An
// inspect docker could not answer is not an exit — the error is handed
// to the caller rather than folded into the verdict.
func (l *lab) containerRunning(ctx context.Context, gw string) (bool, error) {
	out, err := l.docker(ctx, "inspect", "-f", "{{.State.Running}}", l.node(gw))
	if err != nil {
		return true, err
	}
	return strings.TrimSpace(out) == "true", nil
}

// waitContainerExit polls until the container is down or the budget
// expires. A failed inspect keeps the poll going — reading it as an exit
// would report a container that never stopped as cleanly terminated, and
// the restore would then `docker start` a running node.
func (l *lab) waitContainerExit(ctx context.Context, gw string, budget time.Duration) error {
	deadline := l.now().Add(budget)
	var lastErr error
	for l.now().Before(deadline) {
		running, err := l.containerRunning(ctx, gw)
		if err == nil && !running {
			return nil
		}
		lastErr = err
		l.sleep(time.Second)
	}
	if lastErr != nil {
		return fmt.Errorf("container %s could not be inspected within %s after SIGTERM: %w",
			gw, budget, lastErr)
	}
	return fmt.Errorf("container %s still running %s after SIGTERM", gw, budget)
}

// containerHealth returns the docker healthcheck verdict. The gwnode
// image's HEALTHCHECK covers exactly the three daemons a converged node
// needs (ovs-vswitchd, ovn-controller, the agent), so it is the
// per-node convergence signal.
func (l *lab) containerHealth(ctx context.Context, gw string) string {
	out, err := l.docker(ctx, "inspect", "-f", "{{.State.Health.Status}}", l.node(gw))
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(out)
}

// containerIdentity returns a token that changes when the container is
// replaced by a fresh incarnation. .State.StartedAt is the cheapest stable
// identity: docker restamps it every time it (re)starts the container, so a
// value that differs across a restore means the netns the restore's repairs
// went into is gone — the container reincarnated under them, the way a failed
// first FRR start does after a `docker restart` (#216). An inspect docker
// could not answer is handed to the caller, never folded into "unchanged":
// fabricating a stable identity from a failed read would let a reincarnation
// pass unnoticed.
func (l *lab) containerIdentity(ctx context.Context, gw string) (string, error) {
	out, err := l.docker(ctx, "inspect", "-f", "{{.State.StartedAt}}", l.node(gw))
	if err != nil {
		return "", fmt.Errorf("read the identity of %s: %w", gw, err)
	}
	id := strings.TrimSpace(out)
	if id == "" {
		return "", fmt.Errorf("%s reports no start time — cannot tell one incarnation from the next", gw)
	}
	return id, nil
}

// agentAlive reports whether the agent process is running on gw. pgrep
// exits 1 when nothing matches — the condition under test, and a negative
// answer. Every other failure means the question could not be asked at
// all: a docker daemon under load, a container-state error, the 30s
// cmdTimeout expiring. Folding those into "no agent" would turn a busy
// runner into an agent-down violation and fail the run over a lab that
// was fine, so they are returned as an error.
func (l *lab) agentAlive(ctx context.Context, gw string) (bool, error) {
	out, err := l.exec(ctx, gw, "pgrep", "-f", agentBinary)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// writeFile writes content to a path inside a container. The content is a
// rendered agent config, so it goes through the same argv-level quoting
// configureGatewayBGP already uses for its vtysh block.
func (l *lab) writeFile(ctx context.Context, node, path, content string) error {
	if _, err := l.sh(ctx, node, "printf '%s' "+shellQuote(content)+" > "+path); err != nil {
		return fmt.Errorf("write %s on %s: %w", path, node, err)
	}
	return nil
}

func (l *lab) readFile(ctx context.Context, node, path string) (string, error) {
	out, err := l.exec(ctx, node, "cat", path)
	if err != nil {
		return "", fmt.Errorf("read %s on %s: %w", path, node, err)
	}
	return out, nil
}

func (l *lab) moveFile(ctx context.Context, node, from, to string) error {
	if _, err := l.exec(ctx, node, "mv", from, to); err != nil {
		return fmt.Errorf("move %s to %s on %s: %w", from, to, node, err)
	}
	return nil
}

func (l *lab) removeFile(ctx context.Context, node, path string) error {
	if _, err := l.exec(ctx, node, "rm", "-f", path); err != nil {
		return fmt.Errorf("remove %s on %s: %w", path, node, err)
	}
	return nil
}

// mgmtIP is the gateway's address on the containerlab management network,
// derived exactly the way the gwnode entrypoint derives its geneve encap
// IP. It is where the API VIP's DNAT backend listens.
func (l *lab) mgmtIP(ctx context.Context, gw string) (string, error) {
	out, err := l.sh(ctx, gw, "ip -o -4 addr show eth0 | awk '{print $4}' | cut -d/ -f1")
	if err != nil {
		return "", fmt.Errorf("read the management address of %s: %w", gw, err)
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return "", fmt.Errorf("%s has no address on the management network", gw)
	}
	return ip, nil
}

// checkConfig hands a staged config file to the agent's own validator.
//
// Exit 1 is the answer the gate is asking for — the config is invalid —
// and comes back as (false, why, nil). Every other failure means the
// question could not be asked at all: a docker daemon under load, a
// container that is gone, the 30s cmdTimeout expiring. Folding those into
// "invalid" would be the milder mistake; folding them into "valid" would
// swap a live agent config on the strength of a check that never ran, so
// they are returned as an error — the same split agentAlive makes.
func (l *lab) checkConfig(ctx context.Context, gw, path string) (bool, string, error) {
	out, err := l.exec(ctx, gw, agentBinary, "--check-config", "--config", path)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, err.Error(), nil
		}
		return false, "", fmt.Errorf("validate %s on %s: %w", path, gw, err)
	}
	return true, strings.TrimSpace(out), nil
}

// gatewayBack reports whether a gateway has returned to service after a
// container lifecycle event: the container healthy, its agent process
// running, and its chassis back in SB.
//
// The agent is asked for by name rather than left to the container
// healthcheck, even though the healthcheck covers it too. The run holds
// every node in its healthy pool to the agent-alive invariant, so it has to
// re-admit a node on that same signal — anything weaker puts a gateway that
// is still running its entrypoint back in the pool, and every sweep until
// the agent execs records an `agent-down` against a node that is merely
// booting (issue #205). Reading the invariant off the same probe that
// asserts it is what keeps the two from drifting apart again.
//
// agentAlive's error means the question could not be asked, which reads
// here as "not back yet": the callers are poll loops with a budget of their
// own, so a docker daemon under load costs a retry rather than a verdict.
func (l *lab) gatewayBack(ctx context.Context, gw string) bool {
	if l.containerHealth(ctx, gw) != "healthy" {
		return false
	}
	alive, err := l.agentAlive(ctx, gw)
	if err != nil || !alive {
		return false
	}
	return l.chassisInSB(ctx, gw)
}

// gatewayBackTimeout bounds the wait for a returning gateway to finish its
// entrypoint before the restore repairs it. It is sized to absorb a #216
// double boot (~87s: a failed first FRR start suicides PID 1 and docker boots
// a second incarnation) with margin — a clean boot execs the agent in a second
// or two — while staying well inside restoreTimeout, so even a rewire that has
// to be re-run against a reincarnation cannot push one restore past its
// backstop.
const gatewayBackTimeout = 120 * time.Second

// waitGatewayBack holds until gw has finished coming back after a container
// lifecycle event — container healthy, agent exec'd, chassis re-registered
// (gatewayBack) — or the budget expires. restoreNode waits on it before it
// touches the netns, so the repairs land on the incarnation the entrypoint
// finished with rather than one a failed FRR start is about to reboot out from
// under them (#216). gatewayBack's transient "not back yet" (a docker daemon
// under load) costs a poll, not a verdict; the loop gives up as soon as ctx is
// done, since restoreTimeout expires long before this clock would.
func (l *lab) waitGatewayBack(ctx context.Context, gw string) error {
	deadline := l.now().Add(gatewayBackTimeout)
	for l.now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for %s to come back: %w", gw, err)
		}
		if l.gatewayBack(ctx, gw) {
			return nil
		}
		l.sleep(convergePollInterval)
	}
	return fmt.Errorf("%s did not come back within %s after its container was recycled",
		gw, gatewayBackTimeout)
}

// chassisInSB reports whether gw still has (or has re-registered) its
// SB Chassis row.
func (l *lab) chassisInSB(ctx context.Context, gw string) bool {
	out, err := l.sbctl(ctx, "--timeout=5", "--bare", "--columns=name",
		"find", "Chassis", "name="+gw)
	return err == nil && strings.TrimSpace(out) == gw
}

// currentMaster resolves the chassis anchoring cr-lr0-public. The
// Port_Binding.chassis column is a UUID reference, so it takes a second
// lookup against the Chassis table — the same two-step failover.sh
// does. An empty name means the port is currently unbound (re-election
// in flight).
func (l *lab) currentMaster(ctx context.Context) string {
	uuid, err := l.sbctl(ctx, "--timeout=5", "--bare", "--columns=chassis",
		"find", "Port_Binding", "logical_port="+crPort)
	if err != nil || strings.TrimSpace(uuid) == "" {
		return ""
	}
	name, err := l.sbctl(ctx, "--timeout=5", "--bare", "--columns=name",
		"list", "Chassis", strings.TrimSpace(uuid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// crClaim is one chassisredirect Port_Binding and the set of chassis
// claiming it.
type crClaim struct {
	port    string
	chassis []string
}

// crPortClaims lists every chassisredirect Port_Binding with the chassis
// referenced by its `chassis` and `additional_chassis` columns. More
// than one distinct chassis on a port means two nodes are forwarding for
// the same gateway port at once — the split-brain the run must never
// observe.
func (l *lab) crPortClaims(ctx context.Context) ([]crClaim, error) {
	out, err := l.sbctl(ctx, "--timeout=5", "--format=json",
		"--columns=logical_port,chassis,additional_chassis",
		"find", "Port_Binding", "type=chassisredirect")
	if err != nil {
		return nil, fmt.Errorf("list chassisredirect port bindings: %w", err)
	}
	rows, err := parseOVSDBTable(out)
	if err != nil {
		return nil, fmt.Errorf("parse chassisredirect port bindings: %w", err)
	}
	claims := make([]crClaim, 0, len(rows))
	for _, row := range rows {
		claim := crClaim{port: strings.Join(row["logical_port"], "")}
		seen := map[string]bool{}
		for _, col := range []string{"chassis", "additional_chassis"} {
			for _, uuid := range row[col] {
				if uuid != "" && !seen[uuid] {
					seen[uuid] = true
					claim.chassis = append(claim.chassis, uuid)
				}
			}
		}
		claims = append(claims, claim)
	}
	return claims, nil
}

// chassisName resolves an SB Chassis UUID to its name, so a dual-claim
// violation names the nodes instead of their UUIDs. Falls back to the
// UUID when the row is gone.
func (l *lab) chassisName(ctx context.Context, uuid string) string {
	out, err := l.sbctl(ctx, "--timeout=5", "--bare", "--columns=name", "list", "Chassis", uuid)
	if err != nil || strings.TrimSpace(out) == "" {
		return uuid
	}
	return strings.TrimSpace(out)
}

// parseOVSDBTable flattens `ovn-sbctl --format=json` output into one
// map per row, each column reduced to the plain strings it carries.
// OVSDB renders a cell as a scalar, as ["uuid", "<id>"], or as
// ["set", [<cell>, ...]] — all three collapse to a list of strings
// here, which is all the callers need.
func parseOVSDBTable(out string) ([]map[string][]string, error) {
	var table struct {
		Headings []string            `json:"headings"`
		Data     [][]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &table); err != nil {
		return nil, fmt.Errorf("decode ovsdb json: %w", err)
	}
	rows := make([]map[string][]string, 0, len(table.Data))
	for _, data := range table.Data {
		row := map[string][]string{}
		for i, heading := range table.Headings {
			if i >= len(data) {
				break
			}
			row[heading] = flattenOVSDBCell(data[i])
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func flattenOVSDBCell(raw json.RawMessage) []string {
	var scalar string
	if err := json.Unmarshal(raw, &scalar); err == nil {
		if scalar == "" {
			return nil
		}
		return []string{scalar}
	}
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) != 2 {
		return nil
	}
	var kind string
	if err := json.Unmarshal(tuple[0], &kind); err != nil {
		return nil
	}
	switch kind {
	case "uuid", "named-uuid":
		var value string
		if err := json.Unmarshal(tuple[1], &value); err != nil {
			return nil
		}
		return []string{value}
	case "set":
		var members []json.RawMessage
		if err := json.Unmarshal(tuple[1], &members); err != nil {
			return nil
		}
		var values []string
		for _, member := range members {
			values = append(values, flattenOVSDBCell(member)...)
		}
		return values
	default:
		return nil
	}
}

// rewireUnderlay re-establishes the containerlab veth
// `gateway-N:eth1 ↔ upstream:ethN` that any container exit destroys, and
// re-applies the underlay + BGP config bootstrap.sh seeds on top of it.
// Without this a killed or restarted gateway comes back with no
// underlay, no BGP session and no FIP advertisement — which is why the
// existing scenarios either avoid container lifecycle events or recycle
// the whole lab instead of returning a node to service.
func (l *lab) rewireUnderlay(ctx context.Context, gw string) error {
	link, ok := linkFor(gw)
	if !ok {
		return fmt.Errorf("no underlay link known for %s", gw)
	}
	if _, err := l.exec(ctx, gw, "ip", "link", "show", "eth1"); err != nil {
		if _, err := l.cmd.run(ctx, "containerlab", "tools", "veth", "create",
			"-a", l.node(gw)+":eth1",
			"-b", l.node(upstreamNode)+":"+link.upstreamIface); err != nil {
			return fmt.Errorf("re-create underlay veth for %s: %w", gw, err)
		}
	}

	if err := l.waitReady(ctx, gw, "ovs-vsctl show"); err != nil {
		return fmt.Errorf("wait for OVS on %s: %w", gw, err)
	}
	// Mirrors wire_gateway_underlay in bootstrap.sh.
	if _, err := l.sh(ctx, gw, strings.Join([]string{
		"ovs-vsctl --if-exists del-port br-ex eth1",
		"ip link set eth1 down",
		"ip link set eth1 master vrf-provider",
		"ip link set eth1 up",
		"ip addr replace " + link.gatewayCIDR + " dev eth1",
	}, "; ")); err != nil {
		return fmt.Errorf("wire %s underlay: %w", gw, err)
	}
	// Mirrors configure_upstream in bootstrap.sh for this one link.
	if _, err := l.sh(ctx, upstreamNode, strings.Join([]string{
		"ip link set " + link.upstreamIface + " up",
		"ip addr replace " + link.upstreamCIDR + " dev " + link.upstreamIface,
	}, "; ")); err != nil {
		return fmt.Errorf("wire upstream side of %s: %w", gw, err)
	}

	if err := l.waitReady(ctx, gw, "vtysh -c 'show version'"); err != nil {
		return fmt.Errorf("wait for FRR on %s: %w", gw, err)
	}
	if err := l.configureGatewayBGP(ctx, gw, link); err != nil {
		return err
	}
	return l.verifyUnderlay(ctx, gw, link)
}

// verifyUnderlay asserts the rewire's own outcome before the restore declares
// the node back: eth1 exists and carries the expected underlay address, and
// FRR's running-config names the real upstream neighbor with the real
// router-id rather than the entrypoint's placeholder. A rewire that cannot
// satisfy this has landed nowhere useful — most often because the container
// reincarnated under it (#216), taking the netns the rewire configured with
// it — and must fail the restore truthfully instead of returning a success the
// recovery gate then spends its whole budget contradicting. It does not wait
// for the session to reach Established: that legitimately takes seconds and is
// what the converge gate's end-to-end probes already prove.
func (l *lab) verifyUnderlay(ctx context.Context, gw string, link underlayLink) error {
	addr, err := l.sh(ctx, gw, "ip -o -4 addr show eth1")
	if err != nil {
		return fmt.Errorf("verify eth1 on %s: %w", gw, err)
	}
	if !strings.Contains(addr, link.gatewayCIDR) {
		return fmt.Errorf("eth1 on %s does not carry %s after the rewire: %q",
			gw, link.gatewayCIDR, strings.TrimSpace(addr))
	}
	cfg, err := l.sh(ctx, gw, "vtysh -c 'show running-config'")
	if err != nil {
		return fmt.Errorf("read the BGP config on %s: %w", gw, err)
	}
	routerID := addrOf(link.gatewayCIDR)
	if !strings.Contains(cfg, "bgp router-id "+routerID) {
		return fmt.Errorf("BGP on %s is not configured with router-id %s after the rewire "+
			"(entrypoint placeholder still in place?)", gw, routerID)
	}
	neighbor := addrOf(link.upstreamCIDR)
	if !strings.Contains(cfg, "neighbor "+neighbor) {
		return fmt.Errorf("BGP on %s names no neighbor %s after the rewire", gw, neighbor)
	}
	return nil
}

// configureGatewayBGP replaces the placeholder BGP config the gwnode
// entrypoint seeds into a gateway that has none with the real session
// against this gateway's upstream /30 — configure_gateway_frr in
// bootstrap.sh. The block opens with `no router bgp ...`, so re-applying
// it is self-cleaning.
//
// It no longer races the entrypoint for the last word: configure_frr
// skips its placeholder whenever the VRF already carries a BGP router
// (issue #218), so this config survives whichever of the two runs second.
//
// `redistribute connected route-map ANNOUNCE-CONNECTED` is the port-forward
// VIP's announce path (#223): the VIP is a local /32 on port_forward_dev, so
// its connected route — filtered through a route-map that matches
// ANNOUNCED-NETWORKS — is what BGP exports. The route-map form is deliberate:
// on a standby the agent deletes ANNOUNCED-NETWORKS, and an undefined list
// inside a route-map `match` is a no-match that falls through to the terminal
// deny, exporting nothing — whereas a bare `redistribute connected` would leak
// the underlay /30s. The route-map rides in the same transaction and is
// persisted by the existing `write memory`.
func (l *lab) configureGatewayBGP(ctx context.Context, gw string, link underlayLink) error {
	upstreamIP := addrOf(link.upstreamCIDR)
	routerID := addrOf(link.gatewayCIDR)
	config := strings.Join([]string{
		"configure terminal",
		"route-map ANNOUNCE-CONNECTED permit 10",
		" match ip address prefix-list ANNOUNCED-NETWORKS",
		"exit",
		"no router bgp 65000 vrf vrf-provider",
		"router bgp 65000 vrf vrf-provider",
		" bgp router-id " + routerID,
		" no bgp default ipv4-unicast",
		" no bgp ebgp-requires-policy",
		" neighbor " + upstreamIP + " remote-as 65001",
		" address-family ipv4 unicast",
		"  redistribute static",
		"  redistribute connected route-map ANNOUNCE-CONNECTED",
		"  neighbor " + upstreamIP + " activate",
		"  neighbor " + upstreamIP + " prefix-list ANNOUNCED-NETWORKS out",
		" exit-address-family",
		"end",
		"write memory",
	}, "\n")
	if _, err := l.sh(ctx, gw, "printf '%s\\n' "+shellQuote(config)+" | vtysh"); err != nil {
		return fmt.Errorf("configure BGP on %s: %w", gw, err)
	}
	return nil
}

// waitReady polls a readiness command inside a container until it
// succeeds or daemonReadyTimeout expires. It gives up as soon as ctx is
// done: a restore chains several of these, and the context that bounds
// the whole restore expires long before the last one's own clock would —
// left unwatched, each remaining loop would spin out its 60s against
// commands the dead context fails instantly.
func (l *lab) waitReady(ctx context.Context, node, probe string) error {
	return l.waitReadyFor(ctx, node, probe, daemonReadyTimeout)
}

// waitReadyFor is waitReady on the caller's own budget — for the wait
// that carries more than one daemon coming up, like the backgrounded FRR
// recycle whose completion marker arrives only after a full stop+start.
func (l *lab) waitReadyFor(ctx context.Context, node, probe string, timeout time.Duration) error {
	deadline := l.now().Add(timeout)
	for l.now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for %q on %s: %w", probe, node, err)
		}
		if _, err := l.sh(ctx, node, probe); err == nil {
			return nil
		}
		l.sleep(2 * time.Second)
	}
	return fmt.Errorf("%q on %s did not succeed within %s", probe, node, timeout)
}

// ensureVIPRouting points the port-forward VIP's two hand-plumbed routes
// at `master`. pf-external.sh pins them to gateway-1 because the agent
// does not propagate Load_Balancer VIPs into the underlay; a chaos run
// migrates the master, so the runner re-points them — the job an
// external orchestrator would do in production. Without it the VIP probe
// would go permanently red on the first migration and every later
// violation would be noise.
func (l *lab) ensureVIPRouting(ctx context.Context, master string) error {
	link, ok := linkFor(master)
	if !ok {
		return fmt.Errorf("no underlay link known for master %s", master)
	}
	nexthop := addrOf(link.gatewayCIDR)
	if _, err := l.exec(ctx, upstreamNode, "ip", "route", "replace",
		vipAddr+"/32", "via", nexthop); err != nil {
		return fmt.Errorf("point %s at %s on upstream: %w", vipAddr, master, err)
	}
	if _, err := l.exec(ctx, master, "ip", "route", "replace",
		vipAddr+"/32", "dev", "br-ex", "scope", "link"); err != nil {
		return fmt.Errorf("add %s scope-link route on %s: %w", vipAddr, master, err)
	}
	return nil
}

// ping / httpGet are the two probe primitives, both sourced from
// client-1 — the external vantage point every scenario probes from.
func (l *lab) ping(ctx context.Context, addr string) error {
	_, err := l.exec(ctx, clientNode, "ping", "-c", "1", "-W", "1", addr)
	return err
}

func (l *lab) httpGet(ctx context.Context, url string) error {
	_, err := l.exec(ctx, clientNode, "curl", "--silent", "--max-time", "3",
		"--output", "/dev/null", url)
	return err
}

// addrOf strips the prefix length from one of the table's CIDRs.
func addrOf(cidr string) string { return strings.SplitN(cidr, "/", 2)[0] }

// shellQuote wraps s in single quotes for the container-side `sh -c`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

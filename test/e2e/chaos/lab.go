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

// terminateAgent signals the agent with SIGTERM. The gwnode entrypoint
// execs the agent, so it is PID 1 — the same handle drain-hitless.sh
// uses. Whether the agent drains before exiting depends on how the lab
// was deployed (E2E_DRAIN_ON_SHUTDOWN).
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
// for a SIGTERM'ed agent (PID 1) to actually take the container down. An
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

// agentAlive reports whether the agent process is running on gw. pgrep
// exits 1 when nothing matches — the condition under test, and a negative
// answer. Every other failure means the question could not be asked at
// all: a docker daemon under load, a container-state error, the 30s
// cmdTimeout expiring. Folding those into "no agent" would turn a busy
// runner into an agent-down violation and fail the run over a lab that
// was fine, so they are returned as an error.
func (l *lab) agentAlive(ctx context.Context, gw string) (bool, error) {
	out, err := l.exec(ctx, gw, "pgrep", "-f", "/usr/local/bin/ovn-network-agent")
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return false, nil
		}
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
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
	return l.configureGatewayBGP(ctx, gw, link)
}

// configureGatewayBGP replaces the placeholder BGP config the gwnode
// entrypoint pushes on every container start with the real session
// against this gateway's upstream /30 — configure_gateway_frr in
// bootstrap.sh. The block opens with `no router bgp ...`, so re-applying
// it is self-cleaning.
func (l *lab) configureGatewayBGP(ctx context.Context, gw string, link underlayLink) error {
	upstreamIP := addrOf(link.upstreamCIDR)
	routerID := addrOf(link.gatewayCIDR)
	config := strings.Join([]string{
		"configure terminal",
		"no router bgp 65000 vrf vrf-provider",
		"router bgp 65000 vrf vrf-provider",
		" bgp router-id " + routerID,
		" no bgp default ipv4-unicast",
		" no bgp ebgp-requires-policy",
		" neighbor " + upstreamIP + " remote-as 65001",
		" address-family ipv4 unicast",
		"  redistribute static",
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
	deadline := l.now().Add(daemonReadyTimeout)
	for l.now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for %q on %s: %w", probe, node, err)
		}
		if _, err := l.sh(ctx, node, probe); err == nil {
			return nil
		}
		l.sleep(2 * time.Second)
	}
	return fmt.Errorf("%q on %s did not succeed within %s", probe, node, daemonReadyTimeout)
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

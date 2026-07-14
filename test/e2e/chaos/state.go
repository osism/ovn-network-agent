package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The start state layers the setups of three existing scenarios on top of
// the bootstrap baseline, so one chaos run can exercise all of them at
// once:
//
//   - hairpin.sh      — a second FIP (192.0.2.12) with a vm2 responder,
//     so the master carries two FIPs and the hairpin flow is live.
//   - multi-vlan.sh   — two VLAN provider networks (tags 101/102), each
//     with a router pinned to gateway-1 and a FIP behind a responder.
//   - pf-external.sh  — a Load_Balancer VIP (192.0.2.50:80) in front of
//     the vm1 backend, plus the two routes the agent does not manage.
//
// Which of them a run puts up is the profile's call (profiles.go): a
// profile that configures its gateways without OVN has no use for a
// Load_Balancer VIP, and one that measures the agent's own DNAT path
// brings its own backend instead.
//
// The bootstrap baseline itself (HA router, FIP 192.0.2.10, the vm1
// workload) is assumed — the runner drives a lab that `make e2e-up`
// already brought up.
//
// Everything below mirrors the command blocks in those scripts. The
// layering is idempotent for the same reason theirs is (--may-exist,
// `ip ... replace`, guarded `ip link add`), which is what makes it
// reusable as the post-fault restore path.

// startStateTimeout is how long the layered state gets to come up green
// before the run is abandoned — a start state that never went green
// would report every later fault as a false violation.
const startStateTimeout = 120 * time.Second

// backendBindTimeout is how long an HTTP responder gets to bind its
// listener before the layering gives up on it. A backend that never bound
// would leave its VIP probe red for the whole run.
const backendBindTimeout = 10 * time.Second

// The two responders' log files. They are the same binary, and on the
// workload host both run in the same PID namespace, so the log path is
// what a `pkill -f` pattern keys on: `pkill -f /usr/local/bin/pf-backend`
// would take the other one down with it.
const (
	pfBackendLog  = "/tmp/pf-backend.log"
	apiBackendLog = "/tmp/api-backend.log"
)

// vlanNetwork is one row of multi-vlan.sh's NETWORKS table.
type vlanNetwork struct {
	tag    string
	pub    string // public /24 prefix
	tenant string // tenant /24 prefix
	hex    string // MAC octet
}

var vlanNetworks = []vlanNetwork{
	{"101", "198.51.100", "192.168.101", "65"},
	{"102", "203.0.113", "192.168.102", "66"},
}

// netns is a kernel-side responder on the workload host: a veth pair
// with the host end on br-int carrying the OVN logical port's iface-id,
// and the peer end in a netns with the tenant address.
type netns struct {
	name string
	lsp  string
	mac  string
	cidr string
	gw   string
}

// responders is every netns the profile's start state needs on gateway-3
// — the bootstrap vm1 plus the ones the layers it puts up add.
// reprovisionNode re-creates all of them after gateway-3 has been through
// a container lifecycle event, which destroys its network namespace.
func responders(p *profile) []netns {
	// bootstrap.sh:ensure_workload_netns
	all := []netns{{"vm1", "ls0-vm1", "02:00:00:00:0a:0a", "192.168.10.10/24", "192.168.10.1"}}
	if p.hairpin {
		// hairpin.sh:ensure_fip_b_responder
		all = append(all, netns{"vm2", "ls0-vm2", "02:00:00:00:0a:0b", "192.168.10.12/24", "192.168.10.1"})
	}
	if p.vlans {
		// multi-vlan.sh:ensure_responder
		for _, n := range vlanNetworks {
			all = append(all, netns{
				name: "vm" + n.tag,
				lsp:  "vm" + n.tag,
				mac:  "02:00:00:" + n.hex + ":0a:0a",
				cidr: n.tenant + ".10/24",
				gw:   n.tenant + ".1",
			})
		}
	}
	return all
}

// applyStartState layers the profile's scenario setups onto the baseline
// and waits for every probe target the profile measures to answer.
func applyStartState(ctx context.Context, l *lab, p *profile) error {
	if p.hairpin {
		if err := applyHairpinLayer(ctx, l); err != nil {
			return err
		}
	}
	if p.vlans {
		for _, n := range vlanNetworks {
			if err := applyVLANLayer(ctx, l, n); err != nil {
				return err
			}
		}
	}
	if err := ensureResponders(ctx, l, p); err != nil {
		return err
	}
	if p.ovnLB {
		if err := applyPortForwardLayer(ctx, l); err != nil {
			return err
		}
	}
	for _, gw := range p.apiVIPGateways() {
		if err := startAPIBackend(ctx, l, gw); err != nil {
			return err
		}
	}
	return waitStartStateGreen(ctx, l, p)
}

// applyHairpinLayer mirrors ensure_fip_b_lsp / ensure_fip_b_nat in
// hairpin.sh. The responder itself is created by ensureResponders.
func applyHairpinLayer(ctx context.Context, l *lab) error {
	steps := [][]string{
		{"--may-exist", "lsp-add", "ls0", "ls0-vm2"},
		{"lsp-set-addresses", "ls0-vm2", "02:00:00:00:0a:0b 192.168.10.12"},
		{"--may-exist", "lr-nat-add", "lr0", "dnat_and_snat", "192.0.2.12", "192.168.10.12"},
	}
	for _, args := range steps {
		if _, err := l.nbctl(ctx, args...); err != nil {
			return fmt.Errorf("hairpin layer: %w", err)
		}
	}
	return nil
}

// applyVLANLayer mirrors ensure_network in multi-vlan.sh: a VLAN
// provider network on physnet1, a gatewayless public subnet, a router
// pinned to gateway-1, and a FIP with a backing LSP.
func applyVLANLayer(ctx context.Context, l *lab, n vlanNetwork) error {
	lsPub := "ls-vlan" + n.tag
	lnLSP := "ln-vlan" + n.tag
	lsTenant := "ls-vlan" + n.tag + "-t"
	lr := "lr-vlan" + n.tag
	lrpPub := "lr-vlan" + n.tag + "-public"
	lrpTen := "lr-vlan" + n.tag + "-tenant"
	vm := "vm" + n.tag

	steps := [][]string{
		{"--may-exist", "ls-add", lsPub},
		{"--may-exist", "lsp-add", lsPub, lnLSP},
		{"lsp-set-type", lnLSP, "localnet"},
		{"lsp-set-addresses", lnLSP, "unknown"},
		{"lsp-set-options", lnLSP, "network_name=physnet1"},
		{"set", "Logical_Switch_Port", lnLSP, "tag=" + n.tag},

		{"--may-exist", "ls-add", lsTenant},
		{"--may-exist", "lr-add", lr},

		{"--may-exist", "lrp-add", lr, lrpTen, "02:00:00:" + n.hex + ":01:01", n.tenant + ".1/24"},
		{"--may-exist", "lsp-add", lsTenant, lsTenant + "-" + lr},
		{"lsp-set-type", lsTenant + "-" + lr, "router"},
		{"lsp-set-addresses", lsTenant + "-" + lr, "router"},
		{"lsp-set-options", lsTenant + "-" + lr, "router-port=" + lrpTen},

		{"--may-exist", "lrp-add", lr, lrpPub, "02:00:00:" + n.hex + ":02:01", n.pub + ".1/24"},
		{"--may-exist", "lsp-add", lsPub, lsPub + "-" + lr},
		{"lsp-set-type", lsPub + "-" + lr, "router"},
		{"lsp-set-addresses", lsPub + "-" + lr, "router"},
		{"lsp-set-options", lsPub + "-" + lr, "router-port=" + lrpPub},

		// The VLAN routers stay pinned to gateway-1 the way the scenario
		// pins them. A chaos run migrates cr-lr0-public around, but these
		// routers have a single candidate chassis — so a fault on
		// gateway-1 legitimately darkens both VLAN FIPs until it is back.
		{"lrp-set-gateway-chassis", lrpPub, "gateway-1", "30"},

		{"--may-exist", "lr-nat-add", lr, "dnat_and_snat", n.pub + ".10", n.tenant + ".10"},
		{"--may-exist", "lsp-add", lsTenant, vm},
		{"lsp-set-addresses", vm, "02:00:00:" + n.hex + ":0a:0a " + n.tenant + ".10"},
	}
	for _, args := range steps {
		if _, err := l.nbctl(ctx, args...); err != nil {
			return fmt.Errorf("vlan %s layer: %w", n.tag, err)
		}
	}
	return nil
}

// applyPortForwardLayer mirrors pf-external.sh: the backend in the vm1
// netns, the Load_Balancer, and the two routes the agent does not manage
// (which ensureVIPRouting keeps pointed at the current master from here
// on).
func applyPortForwardLayer(ctx context.Context, l *lab) error {
	if err := startPFBackend(ctx, l); err != nil {
		return err
	}
	if _, err := l.nbctl(ctx, "--may-exist", "lb-add", "pf-external",
		vipAddr+":80", "192.168.10.10:8080", "tcp"); err != nil {
		return fmt.Errorf("port-forward layer: %w", err)
	}
	if _, err := l.nbctl(ctx, "--may-exist", "lr-lb-add", "lr0", "pf-external"); err != nil {
		return fmt.Errorf("port-forward layer: %w", err)
	}
	master := l.currentMaster(ctx)
	if master == "" {
		return fmt.Errorf("port-forward layer: %s is unbound, cannot point the VIP anywhere", crPort)
	}
	if err := l.ensureVIPRouting(ctx, master); err != nil {
		return fmt.Errorf("port-forward layer: %w", err)
	}
	return nil
}

// startPFBackend (re)starts the HTTP responder behind the Load_Balancer
// VIP in the vm1 netns and waits for it to bind — start_backend in
// pf-external.sh. Any previous instance is killed first, so this doubles
// as the restore path after gateway-3 has been recycled.
func startPFBackend(ctx context.Context, l *lab) error {
	if _, err := l.sh(ctx, workloadHost,
		"pkill -f "+pfBackendLog+" || true; : >"+pfBackendLog); err != nil {
		return fmt.Errorf("reset pf-backend on %s: %w", workloadHost, err)
	}
	if _, err := l.docker(ctx, "exec", "-d", l.node(workloadHost),
		"ip", "netns", "exec", "vm1", "/usr/local/bin/pf-backend",
		"-addr", ":8080", "-log", pfBackendLog); err != nil {
		return fmt.Errorf("start pf-backend on %s: %w", workloadHost, err)
	}
	if err := waitListening(ctx, l, workloadHost,
		"ip netns exec vm1 ss -ltn 'sport = :8080' | grep -q LISTEN"); err != nil {
		return fmt.Errorf("pf-backend on %s: %w", workloadHost, err)
	}
	return nil
}

// startAPIBackend (re)starts the responder behind the API VIP. Unlike the
// Load_Balancer VIP's backend it runs in the gateway's *default* network
// namespace, because that is where the VIP's port_forwards rule DNATs to:
// the gateway's own management address. It is the reason those gateways
// need port_forward_l3mdev_accept — the socket is in the default VRF
// while the VIP traffic ingresses vrf-provider.
func startAPIBackend(ctx context.Context, l *lab, gw string) error {
	addr := fmt.Sprintf(":%d", apiVIPPort)
	if _, err := l.sh(ctx, gw,
		"pkill -f "+apiBackendLog+" || true; : >"+apiBackendLog); err != nil {
		return fmt.Errorf("reset the API backend on %s: %w", gw, err)
	}
	if _, err := l.docker(ctx, "exec", "-d", l.node(gw),
		"/usr/local/bin/pf-backend", "-addr", addr, "-log", apiBackendLog); err != nil {
		return fmt.Errorf("start the API backend on %s: %w", gw, err)
	}
	if err := waitListening(ctx, l, gw,
		fmt.Sprintf("ss -ltn 'sport = %s' | grep -q LISTEN", addr)); err != nil {
		return fmt.Errorf("the API backend on %s: %w", gw, err)
	}
	return nil
}

// waitListening polls a responder's listener until it is up. A backend
// that never bound must fail the layering rather than leave its VIP probe
// red — and every recovery gate behind it timing out — for the whole run.
func waitListening(ctx context.Context, l *lab, node, probe string) error {
	deadline := l.now().Add(backendBindTimeout)
	for l.now().Before(deadline) {
		if _, err := l.sh(ctx, node, probe); err == nil {
			return nil
		}
		l.sleep(time.Second)
	}
	return fmt.Errorf("did not bind within %s", backendBindTimeout)
}

// ensureResponders (re)creates every kernel-side responder on the
// workload host. Idempotent, and the shape is lifted verbatim from
// ensure_workload_netns in bootstrap.sh.
func ensureResponders(ctx context.Context, l *lab, p *profile) error {
	for _, n := range responders(p) {
		hostVeth := n.name + "-host"
		nsVeth := n.name + "-eth0"
		script := strings.Join([]string{
			fmt.Sprintf("ip link show %s >/dev/null 2>&1 || ip link add %s type veth peer name %s",
				hostVeth, hostVeth, nsVeth),
			fmt.Sprintf("ovs-vsctl --may-exist add-port br-int %s -- set Interface %s external_ids:iface-id=%s",
				hostVeth, hostVeth, n.lsp),
			fmt.Sprintf("ip link set %s up", hostVeth),
			fmt.Sprintf("ip netns list | awk '{print $1}' | grep -qx %s || ip netns add %s", n.name, n.name),
			fmt.Sprintf("ip -n %s link show %s >/dev/null 2>&1 || ip link set %s netns %s",
				n.name, nsVeth, nsVeth, n.name),
			fmt.Sprintf("ip -n %s link set lo up", n.name),
			fmt.Sprintf("ip -n %s link set %s address %s", n.name, nsVeth, n.mac),
			fmt.Sprintf("ip -n %s link set %s up", n.name, nsVeth),
			fmt.Sprintf("ip -n %s addr replace %s dev %s", n.name, n.cidr, nsVeth),
			fmt.Sprintf("ip -n %s route replace default via %s", n.name, n.gw),
		}, "; ")
		if _, err := l.sh(ctx, workloadHost, script); err != nil {
			return fmt.Errorf("provision responder %s on %s: %w", n.name, workloadHost, err)
		}
	}
	return nil
}

// restoreNode returns a gateway to service after a container lifecycle
// event. Two things are gone that `docker start` does not bring back:
//
//   - the containerlab veth `gateway-N:eth1 ↔ upstream:ethN`, and with
//     it the underlay and the BGP session (rewireUnderlay), and
//   - on the workload host, every netns and veth behind the FIPs, plus
//     the port-forward backend (reprovisionNode).
//
// This is the path the existing scenarios avoid — they either leave the
// container up or recycle the whole lab. A chaos run has to put the node
// back, because the guardrails only re-target a node that has returned
// and converged.
func restoreNode(ctx context.Context, l *lab, p *profile, gw string) error {
	if err := l.rewireUnderlay(ctx, gw); err != nil {
		return err
	}
	return reprovisionNode(ctx, l, p, gw)
}

// reprovisionNode re-creates the node-local state a container restart
// wiped: the workload host's netns responders and the Load_Balancer VIP's
// backend, and — on any gateway whose profile configures the API VIP —
// the responder that VIP forwards to. The Load_Balancer VIP's scope-link
// route is not re-applied here: ensureVIPRouting does it during
// convergence, wherever the master happens to be by then.
func reprovisionNode(ctx context.Context, l *lab, p *profile, gw string) error {
	if gw == workloadHost {
		if err := l.waitReady(ctx, gw, "ovs-vsctl br-exists br-int"); err != nil {
			return fmt.Errorf("wait for br-int on %s: %w", gw, err)
		}
		if err := ensureResponders(ctx, l, p); err != nil {
			return err
		}
		if p.ovnLB {
			if err := startPFBackend(ctx, l); err != nil {
				return err
			}
		}
	}
	if !p.gwConfig(gw).apiVIP {
		return nil
	}
	return startAPIBackend(ctx, l, gw)
}

// waitStartStateGreen holds the run until every probe target answers, so
// a fault is never injected into a lab that was not green to begin with.
func waitStartStateGreen(ctx context.Context, l *lab, p *profile) error {
	deadline := l.now().Add(startStateTimeout)
	for l.now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		red := redStartTargets(ctx, l, p)
		if len(red) == 0 {
			return nil
		}
		l.sleep(2 * time.Second)
	}
	return fmt.Errorf("start state not green within %s: %s",
		startStateTimeout, strings.Join(redStartTargets(ctx, l, p), ", "))
}

func redStartTargets(ctx context.Context, l *lab, p *profile) []string {
	var red []string
	for _, t := range p.probes {
		var err error
		switch t.kind {
		case probeHTTP:
			err = l.httpGet(ctx, t.addr)
		case probePing:
			err = l.ping(ctx, t.addr)
		}
		if err != nil {
			red = append(red, t.name)
		}
	}
	return red
}

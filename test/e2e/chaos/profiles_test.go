package main

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// baseConfig is the config the gwnode image bakes into every gateway —
// the document every overlay is layered over. The tests read the real
// file so a change to it that a profile depends on (the OVN remotes, the
// reconcile cadence) shows up here rather than in a lab.
func baseConfig(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../gwnode-config.yaml")
	if err != nil {
		t.Fatalf("read the baked gwnode config: %v", err)
	}
	return raw
}

// render is renderConfig plus the round-trip back through yaml.v3 — which
// is how the agent itself reads the file. A document with a duplicate
// mapping key parses here exactly as it would fail there.
func render(t *testing.T, c gwConfig, mgmtIP string) map[string]any {
	t.Helper()
	raw, err := renderConfig(baseConfig(t), c, mgmtIP)
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the rendered config is not valid YAML: %v\n%s", err, raw)
	}
	return doc
}

func TestProfileRegistryNamesTheCuratedSet(t *testing.T) {
	want := []string{
		"everything-on", "flat-minimal", "flat-dnat",
		"vlan-no-dnat", "pf-only", "heterogeneous",
	}

	var got []string
	for _, p := range profiles() {
		got = append(got, p.name)
		if p.description == "" {
			t.Fatalf("profile %s ships without a description", p.name)
		}
		if len(p.probes) == 0 {
			t.Fatalf("profile %s measures nothing", p.name)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registry = %v, want %v", got, want)
	}

	def, err := profileByName(defaultProfileName)
	if err != nil {
		t.Fatalf("the default profile does not resolve: %v", err)
	}
	if !def.hairpin || !def.vlans || !def.ovnLB || len(def.gateways) != 0 {
		t.Fatalf("the default profile changed today's run: %+v", def)
	}
}

// An unknown -profile must fail before the run touches a lab, and say
// what it could have picked instead.
func TestProfileByNameRejectsAnUnknownProfile(t *testing.T) {
	_, err := profileByName("everything-off")
	if err == nil {
		t.Fatal("an unknown profile resolved")
	}
	if !strings.Contains(err.Error(), "everything-off") || !strings.Contains(err.Error(), defaultProfileName) {
		t.Fatalf("error %q neither names the unknown profile nor lists the known ones", err)
	}
}

// Byte identity for the empty overlay is what lets the applier skip a
// gateway: a fresh lab's everything-on run must not restart a single one.
func TestEmptyOverlayRendersTheBaseBytesVerbatim(t *testing.T) {
	base := baseConfig(t)

	got, err := renderConfig(base, gwConfig{}, "172.20.20.4")
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}
	if !bytes.Equal(got, base) {
		t.Fatalf("an empty overlay rewrote the baked config:\n%s", got)
	}
}

// An overlay owns the keys it names and leaves every other key of the
// baked config alone — the lab's bridge, VRF and chassis settings are not
// a profile's business.
func TestOverlayLayersOverTheBaseAndKeepsTheRest(t *testing.T) {
	doc := render(t, gwConfig{
		drainOnShutdown:   true,
		cleanupOnShutdown: true,
		reconcileInterval: "15s",
	}, "172.20.20.4")

	for key, want := range map[string]any{
		"drain_on_shutdown":   true,
		"cleanup_on_shutdown": true,
		"reconcile_interval":  "15s",
		// Untouched by the overlay, and load-bearing for the lab.
		"ovn_sb_remote": "tcp:central:6642",
		"bridge_dev":    "br-ex",
		"vrf_name":      "vrf-provider",
	} {
		if doc[key] != want {
			t.Fatalf("%s = %v, want %v", key, doc[key], want)
		}
	}
}

// Port-forward-only mode is the absence of both remotes (validateMode) —
// an empty string would do, but removing the keys is what a gateway
// deployed without OVN would actually look like.
func TestPortForwardOnlyOverlayUnsetsBothOVNRemotes(t *testing.T) {
	doc := render(t, profileGateway(t, "pf-only", "gateway-1"), "172.20.20.5")

	for _, key := range []string{"ovn_sb_remote", "ovn_nb_remote"} {
		if _, present := doc[key]; present {
			t.Fatalf("%s survived the port-forward-only overlay: %v", key, doc[key])
		}
	}
	if got := stringsOf(doc["network_cidr"]); len(got) != 1 || got[0] != "192.0.2.0/24" {
		t.Fatalf("network_cidr = %v, want the provider net — without OVN nothing else announces the VIP", got)
	}
	if len(vipsIn(t, doc)) != 1 {
		t.Fatalf("port-forward-only without a port_forwards block leaves the agent nothing to do: %v", doc)
	}
}

// The API VIP's backend is the responder on the gateway's *own*
// management address, so the rendered rule must carry the address the
// applier discovered for that gateway — not another gateway's.
func TestAPIVIPBlockPointsAtTheGatewaysOwnBackend(t *testing.T) {
	doc := render(t, gwConfig{apiVIP: true}, "172.20.20.7")

	if doc["port_forward_l3mdev_accept"] != true {
		t.Fatal("the same-host backend in the default VRF needs port_forward_l3mdev_accept")
	}
	vip := vipsIn(t, doc)[apiVIPAddr]
	if vip == nil {
		t.Fatalf("the API VIP is not in the rendered config: %v", doc["port_forwards"])
	}
	if vip["masquerade"] != false {
		t.Fatal("a same-host backend must not be masqueraded — the OUTPUT chains handle the replies")
	}
	rules, ok := vip["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("rules = %v, want exactly the one TCP rule", vip["rules"])
	}
	rule, _ := rules[0].(map[string]any)
	if rule["dest_addr"] != "172.20.20.7" || rule["port"] != apiVIPPort {
		t.Fatalf("rule = %v, want tcp/%d to the gateway's own management address", rule, apiVIPPort)
	}
}

// The hairpin VIP renders as a second port_forwards entry after the API
// VIP's. Order matters to the flips, which select an entry by address out
// of the list they find.
func TestHairpinVIPRendersBesideTheAPIVIP(t *testing.T) {
	doc := render(t, gwConfig{apiVIP: true, hairpinVIP: true}, "172.20.20.7")

	entries, ok := doc["port_forwards"].([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("port_forwards = %v, want both VIPs", doc["port_forwards"])
	}
	if got := vipAddrOf(entries[0]); got != apiVIPAddr {
		t.Fatalf("first entry is %s, want the API VIP %s", got, apiVIPAddr)
	}
	if got := vipAddrOf(entries[1]); got != hairpinVIPAddr {
		t.Fatalf("second entry is %s, want the hairpin VIP %s", got, hairpinVIPAddr)
	}

	vip := vipsIn(t, doc)[hairpinVIPAddr]
	if vip["manage_vip"] != true {
		t.Fatal("the agent has to own the hairpin VIP address for the probe to reach it")
	}
	// A same-host backend must not be masqueraded, and the key is spelled
	// out so the masquerade flip toggling it twice lands back on exactly
	// this document.
	if vip["masquerade"] != false || vip["hairpin_masquerade"] != false {
		t.Fatalf("hairpin VIP = %v, want both masquerade keys present and false", vip)
	}
	rules, ok := vip["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("rules = %v, want exactly the one TCP rule", vip["rules"])
	}
	rule, _ := rules[0].(map[string]any)
	if rule["port"] != hairpinVIPPort || rule["dest_addr"] != "172.20.20.7" || rule["dest_port"] != apiVIPPort {
		t.Fatalf("rule = %v, want tcp/%d to the gateway's own backend on %d",
			rule, hairpinVIPPort, apiVIPPort)
	}
}

// A hairpinVIP-only overlay changes the document, so it must not be
// mistaken for the zero value — renderConfig hands the base bytes back
// verbatim for an empty overlay, and the applier reads that byte identity
// as "nothing to do, no restart".
func TestHairpinVIPOnlyOverlayIsNotEmpty(t *testing.T) {
	if (gwConfig{hairpinVIP: true}).empty() {
		t.Fatal("a hairpinVIP overlay reports empty, so renderConfig would drop it")
	}
	got, err := renderConfig(baseConfig(t), gwConfig{hairpinVIP: true}, "172.20.20.7")
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}
	if bytes.Equal(got, baseConfig(t)) {
		t.Fatal("the hairpinVIP overlay rendered the base bytes verbatim")
	}
}

// The hairpin VIP's backend is the responder startAPIBackend puts up on
// API-VIP gateways, and nothing else starts one. A gateway carrying the
// VIP without it would be a DNAT rule with nothing behind it, and its
// probe would be red for the whole run.
func TestHairpinVIPNeverAppearsWithoutItsBackend(t *testing.T) {
	for _, p := range profiles() {
		for _, gw := range gatewayNames() {
			c := p.gwConfig(gw)
			if c.hairpinVIP && !c.apiVIP {
				t.Fatalf("%s gives %s the hairpin VIP without the API VIP's backend", p.name, gw)
			}
		}
	}
}

// Which profiles measure the same-node paths. hairpin-fip needs the
// hairpin layer's second FIP; hairpin-vip additionally needs a gateway
// configured with the VIP, which is why it is not in every profile that
// carries the layer.
func TestSameNodeProbesAreWiredToTheRightProfiles(t *testing.T) {
	wantFIP := map[string]bool{
		"everything-on": true, "flat-dnat": true,
		"vlan-no-dnat": true, "heterogeneous": true,
	}
	wantVIP := map[string]bool{"flat-dnat": true, "heterogeneous": true}
	// The cross-chassis probe rides every profile that puts the hairpin
	// layer up: both need OVN and a second router beside lr0 (#265).
	wantCross := map[string]bool{
		"everything-on": true, "flat-dnat": true,
		"vlan-no-dnat": true, "heterogeneous": true,
	}

	for _, p := range profiles() {
		if got := hasProbe(p.probes, probeHairpinFIP.name); got != wantFIP[p.name] {
			t.Errorf("%s probes hairpin-fip = %v, want %v", p.name, got, wantFIP[p.name])
		}
		if got := hasProbe(p.probes, probeHairpinVIP.name); got != wantVIP[p.name] {
			t.Errorf("%s probes hairpin-vip = %v, want %v", p.name, got, wantVIP[p.name])
		}
		if got := hasProbe(p.probes, probeCrossFIP.name); got != wantCross[p.name] {
			t.Errorf("%s probes cross-fip = %v, want %v", p.name, got, wantCross[p.name])
		}
		// Both ride vm1's namespace on the workload host, and both need
		// the hairpin layer's FIP_B or the VIP behind them.
		for _, target := range p.probes {
			switch target.name {
			case probeHairpinFIP.name:
				if !p.hairpin {
					t.Errorf("%s probes the same-node hairpin FIP without the hairpin layer", p.name)
				}
			case probeHairpinVIP.name:
				if !anyGatewayHasHairpinVIP(p) {
					t.Errorf("%s probes the hairpin VIP but no gateway is configured with it", p.name)
				}
			case probeCrossFIP.name:
				if !p.crossChassis {
					t.Errorf("%s probes the cross-chassis FIP without the cross-chassis layer", p.name)
				}
			}
			if target.name == probeHairpinFIP.name || target.name == probeHairpinVIP.name {
				if target.node != workloadHost || target.netns != "vm1" {
					t.Errorf("%s: %s probes from %q/%q, want the workload host's vm1",
						p.name, target.name, target.node, target.netns)
				}
			}
			// The cross-chassis probe must leave OVN on gateway-2, which
			// only a workload behind lr1 does: vm3 on the workload host.
			if target.name == probeCrossFIP.name {
				if target.node != workloadHost || target.netns != "vm3" {
					t.Errorf("%s: %s probes from %q/%q, want the workload host's vm3",
						p.name, target.name, target.node, target.netns)
				}
			}
		}
	}
}

// The heterogeneous profile is the point of the per-gateway overlay: the
// three gateways run three different configurations at once.
func TestHeterogeneousProfileRendersOneConfigPerGateway(t *testing.T) {
	p, err := profileByName("heterogeneous")
	if err != nil {
		t.Fatalf("profileByName: %v", err)
	}

	first := render(t, p.gwConfig("gateway-1"), "172.20.20.4")
	if vipsIn(t, first)[apiVIPAddr] == nil {
		t.Fatalf("gateway-1 = %v, want the API VIP", first)
	}
	if first["drain_on_shutdown"] == true {
		t.Fatalf("gateway-1 = %v, want the VIP without the drain — that is gateway-2's variant", first)
	}

	second := render(t, p.gwConfig("gateway-2"), "172.20.20.5")
	if second["drain_on_shutdown"] != true || vipsIn(t, second)[apiVIPAddr] == nil {
		t.Fatalf("gateway-2 = %v, want the drain and the API VIP", second)
	}

	third := render(t, p.gwConfig("gateway-3"), "172.20.20.6")
	if got := stringsOf(third["network_cidr"]); len(got) != len(explicitCIDRs) {
		t.Fatalf("gateway-3 network_cidr = %v, want the manual filter %v", got, explicitCIDRs)
	}
	if third["reconcile_interval"] != "15s" || third["cleanup_on_shutdown"] != true {
		t.Fatalf("gateway-3 = %v, want the slow cadence and the cleanup", third)
	}
	if _, present := third["port_forwards"]; present {
		t.Fatalf("gateway-3 was handed a DNAT config it never asked for: %v", third["port_forwards"])
	}

	if got := strings.Join(p.apiVIPGateways(), ","); got != "gateway-1,gateway-2" {
		t.Fatalf("apiVIPGateways = %v, want exactly the gateways whose configs carry the VIP", got)
	}
}

// A profile must only probe what its own layers put up — a target behind
// a layer that is off has no responder and would be red for the whole
// run, timing out every recovery gate.
func TestProfileProbesOnlyWhatItsLayersPutUp(t *testing.T) {
	for _, p := range profiles() {
		for _, target := range p.probes {
			switch target.name {
			case probeVM2.name:
				if !p.hairpin {
					t.Fatalf("%s probes the hairpin FIP without the hairpin layer", p.name)
				}
			case probeVLAN101.name, probeVLAN102.name:
				if !p.vlans {
					t.Fatalf("%s probes a VLAN FIP without the VLAN layers", p.name)
				}
			case probeLBVIP.name:
				if !p.ovnLB {
					t.Fatalf("%s probes the Load_Balancer VIP without the port-forward layer", p.name)
				}
			case probeAPIVIP.name:
				if len(p.apiVIPGateways()) == 0 {
					t.Fatalf("%s probes the API VIP but no gateway is configured with it", p.name)
				}
			case probeCrossFIP.name:
				if !p.crossChassis {
					t.Fatalf("%s probes the cross-chassis FIP without the cross-chassis layer", p.name)
				}
			}
		}
		// The reverse: a configured API VIP nobody measures would leave the
		// profile's whole point untested.
		if len(p.apiVIPGateways()) > 0 && !hasProbe(p.probes, probeAPIVIP.name) {
			t.Fatalf("%s configures the API VIP but never probes it", p.name)
		}
	}
}

// A profile added to the registry must also reach the workflow's nightly
// matrix and its dispatch options — otherwise a night silently stops
// covering it, or a replay dispatch cannot select it. Both lists are
// duplicated from the registry by hand (a choice input's options cannot be
// computed, and the schedule) matrix array lives inside a shell script), so
// this parses each list and asserts the profile actually appears there —
// not merely somewhere in the file, where a bare comment mention would
// satisfy the check.
func TestChaosWorkflowSweepsEveryProfile(t *testing.T) {
	workflow, err := os.ReadFile("../../../.github/workflows/e2e-chaos.yml")
	if err != nil {
		t.Fatalf("read the chaos workflow: %v", err)
	}

	nightly := workflowNightlyProfiles(t, workflow)
	options := workflowDispatchOptions(t, workflow)

	for _, p := range profiles() {
		if !nightly[p.name] {
			t.Fatalf("profile %s is in the registry but not in the workflow's nightly matrix array — a night silently stops covering it", p.name)
		}
		if !options[p.name] {
			t.Fatalf("profile %s is in the registry but not in the workflow's dispatch choice options — it cannot be replayed", p.name)
		}
	}
}

// workflowNightlyProfiles returns the profiles the schedule) case fans the
// nightly matrix out over. The array lives inside the resolve step's shell
// script (a YAML block scalar), so it is read out of the raw bytes rather
// than the parsed YAML.
func workflowNightlyProfiles(t *testing.T, workflow []byte) map[string]bool {
	t.Helper()
	m := regexp.MustCompile(`(?s)schedule\).*?profiles='(\[.*?\])'`).FindSubmatch(workflow)
	if m == nil {
		t.Fatal("could not locate the nightly profiles array in the schedule) case")
	}
	var names []string
	if err := json.Unmarshal(m[1], &names); err != nil {
		t.Fatalf("nightly profiles array is not valid JSON: %v", err)
	}
	return nameSet(names)
}

// workflowDispatchOptions returns the profiles the workflow_dispatch
// `profile` choice offers for replay.
func workflowDispatchOptions(t *testing.T, workflow []byte) map[string]bool {
	t.Helper()
	var doc struct {
		On struct {
			WorkflowDispatch struct {
				Inputs struct {
					Profile struct {
						Options []string `yaml:"options"`
					} `yaml:"profile"`
				} `yaml:"inputs"`
			} `yaml:"workflow_dispatch"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(workflow, &doc); err != nil {
		t.Fatalf("parse the chaos workflow as YAML: %v", err)
	}
	opts := doc.On.WorkflowDispatch.Inputs.Profile.Options
	if len(opts) == 0 {
		t.Fatal("could not locate the workflow_dispatch profile choice options")
	}
	return nameSet(opts)
}

// nameSet indexes a list of profile names for membership tests.
func nameSet(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return set
}

// profileGateway resolves one gateway's overlay out of a named profile.
func profileGateway(t *testing.T, name, gw string) gwConfig {
	t.Helper()
	p, err := profileByName(name)
	if err != nil {
		t.Fatalf("profileByName(%q): %v", name, err)
	}
	return p.gwConfig(gw)
}

// vipsIn indexes a rendered document's port_forwards block by VIP.
func vipsIn(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	entries, ok := doc["port_forwards"].([]any)
	if !ok {
		return out
	}
	for _, entry := range entries {
		vip, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("port_forwards entry is not a mapping: %v", entry)
		}
		addr, _ := vip["vip"].(string)
		out[addr] = vip
	}
	return out
}

// anyGatewayHasHairpinVIP reports whether the profile configures the
// hairpin VIP anywhere — without it there is no DNAT rule for the
// hairpin-vip probe to reach.
func anyGatewayHasHairpinVIP(p *profile) bool {
	for _, gw := range gatewayNames() {
		if p.gwConfig(gw).hairpinVIP {
			return true
		}
	}
	return false
}

func hasProbe(targets []probeTarget, name string) bool {
	for _, t := range targets {
		if t.name == name {
			return true
		}
	}
	return false
}

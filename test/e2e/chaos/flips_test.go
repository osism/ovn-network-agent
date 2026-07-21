package main

import (
	"testing"
)

// The whitelist order is part of the replay contract — the engine draws a
// flip by index — so a new one is appended, never inserted.
func TestFlipWhitelistOrderIsStable(t *testing.T) {
	want := []string{
		"drain-toggle", "masquerade-toggle", "cidr-toggle",
		"cadence-toggle", "pf-rule-toggle",
	}

	for i, f := range flips() {
		if i >= len(want) || f.name != want[i] {
			t.Fatalf("flip %d = %q, want %q", i, f.name, want)
		}
		if f.applicable == nil || f.apply == nil {
			t.Fatalf("flip %s is not fully declared", f.name)
		}
		if flipName(i) != want[i] {
			t.Fatalf("flipName(%d) = %q, want %q", i, flipName(i), want[i])
		}
	}
	if len(flips()) != len(want) {
		t.Fatalf("whitelist has %d flips, want %d", len(flips()), len(want))
	}
}

// Every flip is a toggle: applied twice it puts the gateway back on the
// configuration the profile gave it. Without that, a long run would drift
// away from its profile one flip at a time and the run would no longer be
// testing the configuration it says it is.
func TestEveryFlipRoundTripsBackToTheProfilesConfig(t *testing.T) {
	for i, f := range flips() {
		t.Run(f.name, func(t *testing.T) {
			// flat-dnat carries the API VIP, so every flip is applicable
			// on it — including the masquerade one.
			c := flipContext(t, "flat-dnat", "gateway-1")
			if !f.applicable(c) {
				t.Fatalf("%s is not applicable on a gateway with the API VIP", f.name)
			}
			before := marshal(t, c.doc)

			from, to := f.apply(c)
			if from == to {
				t.Fatalf("%s reported a move from %q to %q — it changed nothing", f.name, from, to)
			}
			if marshal(t, c.doc) == before {
				t.Fatalf("%s reported %s -> %s but rewrote nothing", f.name, from, to)
			}

			back, forth := flips()[i].apply(c)
			if back != to || forth != from {
				t.Fatalf("%s toggled back from %q to %q, want %q -> %q", f.name, back, forth, to, from)
			}
			if got := marshal(t, c.doc); got != before {
				t.Fatalf("%s did not round-trip:\n%s\nwant\n%s", f.name, got, before)
			}
		})
	}
}

// The CIDR flip toggles between OVN-discovered networks and a manual
// filter — but the way back is always to what the *profile* configured.
// In port-forward-only mode the filter is the only reason the VIP is
// announced at all: toggling it away to nothing would black the run's one
// probe out for good.
func TestCIDRFlipNeverEmptiesAFilterTheProfileNeeds(t *testing.T) {
	c := flipContext(t, "pf-only", "gateway-1")

	from, to := flipNamed(t, "cidr-toggle").apply(c)
	if from != "192.0.2.0/24" || to != cidrLabel(explicitCIDRs) {
		t.Fatalf("cidr-toggle moved %q -> %q, want the profile's filter to the manual one", from, to)
	}

	from, to = flipNamed(t, "cidr-toggle").apply(c)
	if to != "192.0.2.0/24" {
		t.Fatalf("cidr-toggle moved %q -> %q, want it back to the filter the profile configured", from, to)
	}
	if got := stringsOf(c.doc["network_cidr"]); len(got) == 0 {
		t.Fatal("the toggle back emptied the filter: nothing would announce the VIP any more")
	}
}

// A flip that means nothing on the gateway's current configuration is a
// guardrail skip, not a rewrite: the masquerade flip has no VIP to flip it
// on, and the CIDR flip has nowhere to toggle back to when the profile
// already configured the manual filter.
func TestInapplicableFlips(t *testing.T) {
	tests := []struct {
		flip    string
		profile string
		gw      string
		reason  string
	}{
		{
			flip: "masquerade-toggle", profile: defaultProfileName, gw: "gateway-1",
			reason: "the gateway has no port-forward VIP at all",
		},
		{
			flip: "masquerade-toggle", profile: "heterogeneous", gw: "gateway-3",
			reason: "the profile keeps the API VIP off this gateway",
		},
		{
			flip: "cidr-toggle", profile: "heterogeneous", gw: "gateway-3",
			reason: "the profile already put the manual filter on this gateway",
		},
	}
	for _, tc := range tests {
		t.Run(tc.flip+"/"+tc.profile+"/"+tc.gw, func(t *testing.T) {
			t.Parallel()
			if flipNamed(t, tc.flip).applicable(flipContext(t, tc.profile, tc.gw)) {
				t.Fatalf("%s was applicable even though %s", tc.flip, tc.reason)
			}
		})
	}
}

// The port-forward flip carries a VIP of its own. Touching the API VIP's
// rules instead would take the probed data path down on a gateway that is
// meant to keep serving it.
func TestPortForwardFlipLeavesTheProbedVIPAlone(t *testing.T) {
	c := flipContext(t, "flat-dnat", "gateway-2")

	if _, to := flipNamed(t, "pf-rule-toggle").apply(c); to != flipVIPAddr {
		t.Fatalf("pf-rule-toggle added %q, want the unprobed flip VIP %s", to, flipVIPAddr)
	}
	if apiVIPIn(c.doc) == nil {
		t.Fatalf("the probed API VIP was removed by the flip: %v", c.doc["port_forwards"])
	}
	if len(vipsOf(c.doc)) != 2 {
		t.Fatalf("port_forwards = %v, want the API VIP alongside the flip's own", c.doc["port_forwards"])
	}
}

// flipContext renders one gateway's profile configuration and hands back
// what a flip sees: the live document, the profile's own, and the
// gateway's management address.
func flipContext(t *testing.T, profileName, gw string) flipCtx {
	t.Helper()
	p := testProfile(t, profileName)
	raw, err := renderConfig(baseConfig(t), p.gwConfig(gw), "172.20.20.5")
	if err != nil {
		t.Fatalf("renderConfig: %v", err)
	}
	doc, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	baseline, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	return flipCtx{doc: doc, baseline: baseline, mgmtIP: "172.20.20.5"}
}

func flipNamed(t *testing.T, name string) flip {
	t.Helper()
	for _, f := range flips() {
		if f.name == name {
			return f
		}
	}
	t.Fatalf("no flip named %q in the whitelist", name)
	return flip{}
}

func marshal(t *testing.T, doc map[string]any) string {
	t.Helper()
	raw, err := marshalConfig(doc)
	if err != nil {
		t.Fatalf("marshalConfig: %v", err)
	}
	return string(raw)
}

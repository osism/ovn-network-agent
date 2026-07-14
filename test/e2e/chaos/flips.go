package main

import (
	"slices"
	"strconv"
	"strings"
)

// A configuration change is a fault like any other: rollouts, tuning and
// emergency flag flips happen on live gateways, under load, one node at a
// time. The config-flip action draws one of the toggles below, rewrites
// the target's configuration with it, and restarts the node onto it —
// through the same validate-then-swap path the profile apply uses.
//
// The whitelist is append-only, exactly like the action registry: the
// engine draws a flip *by index* from the seeded stream, so inserting one
// would replay every recorded seed as a different run.

// The two reconcile cadences the cadence flip toggles between. The fast
// one is what the lab bakes in (tighter than the production default so
// the stale-chassis cleanup lands inside the CI budget); the slow one is
// three times that, which is enough to change how much a fault costs
// without stalling a recovery gate.
const (
	fastCadence = "5s"
	slowCadence = "15s"
)

// flipCtx is what a flip sees: the gateway's live configuration document,
// which it mutates in place; the document the profile put there, which is
// what a toggle returns to; and the gateway's management address, the
// backend a VIP the flip adds forwards to.
type flipCtx struct {
	doc      map[string]any
	baseline map[string]any
	mgmtIP   string
}

// flip is one whitelisted configuration change the config-flip action may
// make to a live gateway.
type flip struct {
	name string

	// applicable reports whether the flip means anything on this
	// gateway's current configuration. An inapplicable flip is a
	// guardrail skip, not a failure — the engine journals it and moves
	// on, and the decision stream stays aligned either way.
	applicable func(c flipCtx) bool

	// apply toggles the option in place, reporting the values it moved
	// between.
	apply func(c flipCtx) (from, to string)
}

func flips() []flip {
	return []flip{
		{
			// What the agent does with a SIGTERM: exit, or hand its
			// chassis over first. The flip's own restart is the SIGTERM,
			// so the very next one exercises whichever path it just set.
			name:       "drain-toggle",
			applicable: alwaysApplicable,
			apply: func(c flipCtx) (string, string) {
				return toggleBool(c.doc, "drain_on_shutdown")
			},
		},
		{
			// The agent's hairpin SNAT. Traffic from the provider
			// networks is masqueraded, everything else is not — so an
			// external probe from client-1 (outside every provider net) is
			// reachable either way, and what the flip changes is the
			// nftables ruleset the agent has to program and re-program.
			name:       "masquerade-toggle",
			applicable: func(c flipCtx) bool { return apiVIPIn(c.doc) != nil },
			apply: func(c flipCtx) (string, string) {
				return toggleBool(apiVIPIn(c.doc), "hairpin_masquerade")
			},
		},
		{
			// OVN-discovered networks versus a manual filter — the
			// codepath that has to *exclude* routes rather than add them.
			// The manual list covers every probed prefix, so it exercises
			// the filter without blacking a probe out, and the toggle back
			// is always to what the profile configured: in
			// port-forward-only mode an empty filter would stop the VIP
			// being announced at all.
			name: "cidr-toggle",
			applicable: func(c flipCtx) bool {
				return !slices.Equal(stringsOf(c.baseline["network_cidr"]), explicitCIDRs)
			},
			apply: func(c flipCtx) (string, string) {
				from := stringsOf(c.doc["network_cidr"])
				if slices.Equal(from, explicitCIDRs) {
					to := stringsOf(c.baseline["network_cidr"])
					if len(to) == 0 {
						delete(c.doc, "network_cidr")
					} else {
						c.doc["network_cidr"] = anySlice(to)
					}
					return cidrLabel(from), cidrLabel(to)
				}
				c.doc["network_cidr"] = anySlice(explicitCIDRs)
				return cidrLabel(from), cidrLabel(explicitCIDRs)
			},
		},
		{
			// How often the agent re-reconciles — the tuning knob an
			// operator reaches for first, and the one that decides how
			// long a missed event stays missed.
			name:       "cadence-toggle",
			applicable: alwaysApplicable,
			apply: func(c flipCtx) (string, string) {
				from, _ := c.doc["reconcile_interval"].(string)
				to := slowCadence
				if from == slowCadence {
					to = fastCadence
				}
				c.doc["reconcile_interval"] = to
				return from, to
			},
		},
		{
			// A port-forward rule added and removed under load. It carries
			// a VIP of its own rather than touching the API VIP's rules:
			// a rule that exists on one gateway and not on another must
			// never draw probe traffic to a gateway with no backend.
			name:       "pf-rule-toggle",
			applicable: alwaysApplicable,
			apply: func(c flipCtx) (string, string) {
				vips := vipsOf(c.doc)
				for i, vip := range vips {
					if vipAddrOf(vip) != flipVIPAddr {
						continue
					}
					remaining := append(vips[:i:i], vips[i+1:]...)
					if len(remaining) == 0 {
						delete(c.doc, "port_forwards")
					} else {
						c.doc["port_forwards"] = remaining
					}
					return flipVIPAddr, "absent"
				}
				c.doc["port_forwards"] = append(vips, flipVIPBlock(c.mgmtIP))
				return "absent", flipVIPAddr
			},
		},
	}
}

// flipName names a drawn flip for the journal, so a decision that was
// skipped still says which flip it would have been.
func flipName(idx int) string { return flips()[idx].name }

func alwaysApplicable(flipCtx) bool { return true }

// toggleBool inverts a boolean key, treating an absent key as false —
// which is what the agent's own config layering does with it.
func toggleBool(doc map[string]any, key string) (string, string) {
	from, _ := doc[key].(bool)
	doc[key] = !from
	return strconv.FormatBool(from), strconv.FormatBool(!from)
}

// vipsOf is the document's port_forwards list, as a YAML round-trip
// leaves it.
func vipsOf(doc map[string]any) []any {
	vips, _ := doc["port_forwards"].([]any)
	return vips
}

func vipAddrOf(entry any) string {
	vip, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	addr, _ := vip["vip"].(string)
	return addr
}

// apiVIPIn is the document's API VIP entry, or nil when it carries none —
// which is what makes the masquerade flip inapplicable on a gateway whose
// profile configured no DNAT at all.
func apiVIPIn(doc map[string]any) map[string]any {
	for _, entry := range vipsOf(doc) {
		if vipAddrOf(entry) == apiVIPAddr {
			vip, _ := entry.(map[string]any)
			return vip
		}
	}
	return nil
}

// cidrLabel renders a network filter for the journal. An empty filter is
// not "nothing": it is the agent discovering the provider networks from
// OVN itself.
func cidrLabel(cidrs []string) string {
	if len(cidrs) == 0 {
		return "auto"
	}
	return strings.Join(cidrs, ",")
}

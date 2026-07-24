package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The OVN churn class mutates the OVN topology under load, the way an
// operator or an orchestrator does in production: floating IPs and
// port-forward entries added and removed, gateway-chassis priorities
// flipped externally, a chassis deleted and left to return within the
// stale-cleanup grace period. Each churn is deliberately left in place —
// the next draw toggles it back — and the change is journaled as an
// ovn-churn event carrying what it touched and the values it moved between.

const (
	// churnFIP is the floating IP the fip-churn action adds and removes. It
	// is deliberately unprobed, like the bootstrap's backendless 192.0.2.11:
	// a FIP that appears and disappears must never draw probe traffic.
	churnFIP         = "192.0.2.60"
	churnFIPInternal = "192.168.10.60"

	// churnLBVIP is a second vips entry the lb-vip-churn action toggles on
	// the port-forward Load_Balancer, pointing at the same backend on a port
	// of its own. Also unprobed.
	churnLBVIP     = "192.0.2.50:81"
	churnLBBackend = "192.168.10.10:8080"
)

// churner owns the OVN churn actions' journal. The lab it drives is the
// runner's; the journal is set once the run has earned an artifact
// directory, the same late binding the applier uses.
type churner struct {
	lab  *lab
	jrnl *journal
}

func newChurner(l *lab) *churner {
	return &churner{lab: l}
}

// emit records one executed churn: what it touched and the values it moved
// between, so an add/remove cycle is legible from the journal.
func (c *churner) emit(target, object, from, to string) {
	if c.jrnl == nil {
		return
	}
	c.jrnl.emit(event{Event: evOVNChurn, Target: target, Object: object, From: from, To: to})
}

// churnActions is the OVN churn fault class. Every action holds nothing —
// the change is instantaneous — and its restore is a no-op, because the
// churn is meant to persist until the next draw toggles it back (and, for
// chassis-delete, because the live ovn-controller re-registers its own row
// well within the grace period).
func churnActions(p *profile, ch *churner) []*action {
	return []*action{
		{
			name:           "fip-churn",
			weight:         2,
			scope:          scopeCentral,
			object:         "fip " + churnFIP,
			recoveryBudget: 60 * time.Second,
			inject:         ch.fipChurn,
			restore:        churnPersists,
		},
		{
			name:           "lb-vip-churn",
			weight:         2,
			scope:          scopeCentral,
			object:         "lb-vip " + churnLBVIP,
			recoveryBudget: 60 * time.Second,
			// The port-forward Load_Balancer only exists in a profile that put
			// it up.
			applicable: func(context.Context, string, int) bool { return p.ovnLB },
			inject:     ch.lbVIPChurn,
			restore:    churnPersists,
		},
		{
			name:           "priority-flip",
			weight:         2,
			scope:          scopeGateway,
			object:         lrPublicPort,
			recoveryBudget: 120 * time.Second,
			inject:         ch.priorityFlip,
			restore:        churnPersists,
		},
		{
			name:           "chassis-delete",
			weight:         1,
			scope:          scopeGateway,
			object:         "chassis",
			recoveryBudget: 90 * time.Second,
			inject:         ch.chassisDelete,
			restore:        churnPersists,
		},
	}
}

// fipChurn toggles a spare floating IP on lr0: it looks up whether the NAT
// row exists and adds or removes it accordingly.
func (c *churner) fipChurn(ctx context.Context, _ *lab, _ string, _ int) error {
	uuid, err := c.lab.nbctl(ctx, "--bare", "--columns=_uuid", "find", "NAT", "external_ip="+churnFIP)
	if err != nil {
		return fmt.Errorf("look up the churn FIP NAT row: %w", err)
	}
	from, to := churnFIP, "absent"
	if strings.TrimSpace(uuid) == "" {
		from, to = "absent", churnFIP
		if _, err := c.lab.nbctl(ctx, "lr-nat-add", "lr0", "dnat_and_snat", churnFIP, churnFIPInternal); err != nil {
			return fmt.Errorf("add the churn FIP: %w", err)
		}
	} else if _, err := c.lab.nbctl(ctx, "lr-nat-del", "lr0", "dnat_and_snat", churnFIP); err != nil {
		return fmt.Errorf("remove the churn FIP: %w", err)
	}
	c.emit(centralNode, "fip "+churnFIP, from, to)
	return nil
}

// lbVIPChurn toggles a second vips entry on the port-forward Load_Balancer.
func (c *churner) lbVIPChurn(ctx context.Context, _ *lab, _ string, _ int) error {
	vips, err := c.lab.nbctl(ctx, "--bare", "--columns=vips", "find", "Load_Balancer", "name=pf-external")
	if err != nil {
		return fmt.Errorf("look up the port-forward Load_Balancer vips: %w", err)
	}
	from, to := "present", "absent"
	if strings.Contains(vips, churnLBVIP) {
		// The key is an OVSDB string atom holding a colon, so it only parses
		// in its double-quoted form — the same quoting the add branch below
		// puts around it. nbctl runs argv-style with no shell in between, so
		// the quotes have to be in the argument itself.
		if _, err := c.lab.nbctl(ctx, "remove", "load_balancer", "pf-external", "vips",
			`"`+churnLBVIP+`"`); err != nil {
			return fmt.Errorf("remove the churn VIP entry: %w", err)
		}
	} else {
		from, to = "absent", "present"
		if _, err := c.lab.nbctl(ctx, "set", "load_balancer", "pf-external",
			`vips:"`+churnLBVIP+`"="`+churnLBBackend+`"`); err != nil {
			return fmt.Errorf("add the churn VIP entry: %w", err)
		}
	}
	c.emit(centralNode, "lb-vip "+churnLBVIP, from, to)
	return nil
}

// priorityFlip bumps the target's Gateway_Chassis priority above the
// group's current maximum, exactly as bootstrap.sh:converge_master_chassis
// promotes the master externally. It forces a migration the agent's
// EnsureActivePriorityLead then defends; followMaster re-points the
// port-forward VIP and the converge gate waits on green probes.
func (c *churner) priorityFlip(ctx context.Context, _ *lab, gw string, _ int) error {
	out, err := c.lab.nbctl(ctx, "--bare", "--columns=priority", "list", "Gateway_Chassis")
	if err != nil {
		return fmt.Errorf("list Gateway_Chassis priorities: %w", err)
	}
	peak := 0
	for _, field := range strings.Fields(out) {
		if p, err := strconv.Atoi(field); err == nil && p > peak {
			peak = p
		}
	}
	bumped := peak + 10
	if _, err := c.lab.nbctl(ctx, "lrp-set-gateway-chassis", lrPublicPort, gw, strconv.Itoa(bumped)); err != nil {
		return fmt.Errorf("bump the %s Gateway_Chassis priority for %s: %w", lrPublicPort, gw, err)
	}
	c.emit(gw, lrPublicPort, strconv.Itoa(peak), strconv.Itoa(bumped))
	return nil
}

// chassisDelete removes the target's SB Chassis row while its ovn-controller
// keeps running, so the controller re-registers the row well inside the 30 s
// stale_chassis_grace_period — the surviving agents' stale cleanup must not
// fire. The existing gateway convergence (chassis back in SB, green probes)
// is exactly the "returns within the grace period" gate.
func (c *churner) chassisDelete(ctx context.Context, _ *lab, gw string, _ int) error {
	if _, err := c.lab.sbctl(ctx, "--if-exists", "chassis-del", gw); err != nil {
		return fmt.Errorf("delete the SB Chassis row for %s: %w", gw, err)
	}
	c.emit(gw, "chassis "+gw, "present", "absent")
	return nil
}

// churnPersists is the restore for the churn class: the change is left in
// place on purpose. The next draw toggles the FIP and VIP back, the agent
// re-defends the priority, and the live ovn-controller re-registers its own
// Chassis row — so the runner puts nothing back.
func churnPersists(context.Context, *lab, string) error { return nil }

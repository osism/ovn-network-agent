package main

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"time"
)

// =============================================================================
// SIGHUP configuration reload
// =============================================================================
//
// A reload re-reads the configuration with the loader main handed NewAgent,
// diffs it against the running configuration option by option, applies the
// reloadable keys and keeps every other key at its running value with a
// warning. It runs on Run's goroutine between reconcile cycles, so a cycle
// sees either the whole old configuration or the whole new one.

// reloadableKeys lists the config keys a reload applies to the running agent.
// Every other key of configOptions() is restart-only, including any key added
// later: a new option has to be made reloadable on purpose.
var reloadableKeys = map[string]bool{
	"log_level":                  true,
	"reconcile_interval":         true,
	"frr_prefix_list":            true,
	"stale_chassis_grace_period": true,
	"port_forwards":              true,
	"cleanup_on_shutdown":        true,
	"drain_on_shutdown":          true,
	"drain_timeout":              true,
	"drain_settle_delay":         true,
	"ovn_ssl_cert":               true,
	"ovn_ssl_key":                true,
}

// Reasons a changed key keeps its running value.
const (
	reasonStartupOnly       = "only read at startup"
	reasonPortForwardToggle = "enabling or disabling port forwarding needs a restart"
	reasonClientCertToggle  = "adding or removing the OVN client certificate needs a restart"
	reasonCAContent         = "the CA file changed; the running agent keeps the CA it loaded at startup"
)

// errReloadUnavailable is returned by a reload on an agent built without a
// configuration loader.
var errReloadUnavailable = errors.New("configuration reload is not configured")

// restartChange is a changed key a reload kept at its running value.
type restartChange struct {
	Key    string
	Reason string
}

// reloadPlan is what planReload decided for one reload.
type reloadPlan struct {
	// merged is the running configuration with the applied keys taken from
	// next. It keeps the running OVNTLS and OVNClientCert pointers, which
	// libovsdb and every Config copy already share.
	merged Config
	// next is the configuration the loader returned.
	next Config
	// applied lists the keys whose new value takes effect, in registry order.
	applied []string
	// restartRequired lists the changed keys kept at their running value.
	restartRequired []restartChange
}

// planReload diffs running against next and decides, per changed key, whether
// the reload applies it or keeps the running value. It is pure: no I/O and no
// logging. The merged configuration is re-validated, because a mix of applied
// and kept values can be invalid even though next alone was not (a pf-only
// agent that keeps its empty OVN remotes cannot take a router_masquerade VIP).
func planReload(running, next Config) (reloadPlan, error) {
	p := reloadPlan{merged: running, next: next}
	bothClientCert := running.OVNClientCert != nil && next.OVNClientCert != nil

	for _, o := range configOptions() {
		if reflect.DeepEqual(o.value(&running), o.value(&next)) {
			// The paths are unchanged, but a file behind them may not be.
			switch {
			case o.Key == "ovn_ssl_cert" && bothClientCert && clientCertChanged(running.OVNClientCert, next.OVNClientCert):
				p.applied = append(p.applied, o.Key)
			case o.Key == "ovn_ssl_ca" && caContentChanged(running.OVNTLS, next.OVNTLS):
				p.restartRequired = append(p.restartRequired, restartChange{o.Key, reasonCAContent})
			}
			continue
		}
		switch {
		case !reloadableKeys[o.Key]:
			p.restartRequired = append(p.restartRequired, restartChange{o.Key, reasonStartupOnly})
		case o.Key == "port_forwards" && (len(running.PortForwards) == 0) != (len(next.PortForwards) == 0):
			p.restartRequired = append(p.restartRequired, restartChange{o.Key, reasonPortForwardToggle})
		case (o.Key == "ovn_ssl_cert" || o.Key == "ovn_ssl_key") && !bothClientCert:
			p.restartRequired = append(p.restartRequired, restartChange{o.Key, reasonClientCertToggle})
		default:
			o.copyValue(&p.merged, &next)
			p.applied = append(p.applied, o.Key)
		}
	}

	if err := validateSettings(&p.merged); err != nil {
		return reloadPlan{}, fmt.Errorf("validate reloaded configuration: %w", err)
	}
	if err := validateMode(&p.merged); err != nil {
		return reloadPlan{}, fmt.Errorf("validate reloaded configuration: %w", err)
	}
	return p, nil
}

// clientCertChanged reports whether two holders serve different leaf
// certificates.
func clientCertChanged(a, b *clientCertHolder) bool {
	ca, cb := a.load(), b.load()
	if ca == nil || cb == nil || len(ca.Certificate) == 0 || len(cb.Certificate) == 0 {
		return ca != cb
	}
	return !bytes.Equal(ca.Certificate[0], cb.Certificate[0])
}

// caContentChanged reports whether two TLS configs pin different CA pools.
func caContentChanged(a, b *tls.Config) bool {
	if a == nil || b == nil || a.RootCAs == nil || b.RootCAs == nil {
		return false
	}
	return !a.RootCAs.Equal(b.RootCAs)
}

// withdrawnManagedVIPs returns, in running order, every VIP the agent manages
// under running that next drops or no longer manages. Their addresses must be
// removed explicitly: the per-cycle reconcile only walks the current list.
func withdrawnManagedVIPs(running, next []PortForwardVIP) []string {
	stillManaged := make(map[string]bool, len(next))
	for _, pf := range next {
		if pf.ManageVIP {
			stillManaged[pf.VIP] = true
		}
	}
	var out []string
	for _, pf := range running {
		if pf.ManageVIP && !stillManaged[pf.VIP] {
			out = append(out, pf.VIP)
		}
	}
	return out
}

// RequestReload asks Run's loop to reload the configuration. It never blocks;
// requests that arrive while one is pending coalesce into it.
func (a *Agent) RequestReload() {
	select {
	case a.reloadCh <- struct{}{}:
	default:
		// Already pending
	}
}

// reload loads the configuration, plans the reload and applies it. On any
// error the running configuration is left untouched.
func (a *Agent) reload() (reloadPlan, error) {
	if a.reloadConfig == nil {
		return reloadPlan{}, errReloadUnavailable
	}
	next, err := a.reloadConfig()
	if err != nil {
		return reloadPlan{}, fmt.Errorf("load configuration: %w", err)
	}
	p, err := planReload(a.cfg, next)
	if err != nil {
		return reloadPlan{}, err
	}
	a.applyReload(p)
	return p, nil
}

// applyReload puts a plan into effect. The host-side cleanups run first, while
// the routing layer still holds the old values they need; failures there are
// logged and do not stop the reload, matching shutdown cleanup.
func (a *Agent) applyReload(p reloadPlan) {
	applied := func(key string) bool { return slices.Contains(p.applied, key) }
	running := a.cfg

	// Empty the previous prefix-list before switching names; the reconcile
	// the reload triggers fills the new one.
	if applied("frr_prefix_list") && running.FRRPrefixList != "" {
		slog.Info("FRR prefix-list renamed, emptying the previous list",
			"old", running.FRRPrefixList, "new", p.merged.FRRPrefixList)
		if err := a.routing.ReconcileFRRPrefixList(nil, nil); err != nil {
			slog.Error("failed to empty the previous FRR prefix-list", "name", running.FRRPrefixList, "error", err)
		}
	}

	if applied("port_forwards") {
		if vips := withdrawnManagedVIPs(running.PortForwards, p.merged.PortForwards); len(vips) > 0 {
			if err := a.routing.withdrawVIPs(vips); err != nil {
				slog.Error("failed to withdraw VIP addresses removed by the reload", "vips", vips, "error", err)
			}
		}
	}

	a.cfg = p.merged
	a.routing.cfg = p.merged

	if applied("drain_settle_delay") && a.ovn != nil {
		a.ovn.setDrainSettleDelay(p.merged.DrainSettleDelay)
	}

	// With the cleanup disabled cleanupStaleChassis returns before touching
	// the tracker, so reset it as a fresh start with the feature off would.
	if applied("stale_chassis_grace_period") && p.merged.StaleChassisGracePeriod <= 0 {
		a.missingChassis = make(map[string]time.Time)
		setMissingChassis(0)
	}

	if applied("log_level") {
		logLevel.Set(parseLogLevel(p.merged.LogLevel))
	}

	// planReload only applies the client-cert keys when both configurations
	// carry a holder. merged keeps the running holder, which every Config
	// copy and the tls.Config libovsdb redials with share.
	if applied("ovn_ssl_cert") || applied("ovn_ssl_key") {
		cert := p.next.OVNClientCert.load()
		p.merged.OVNClientCert.store(cert)
		attrs := []any{"path", p.merged.OVNSSLCert}
		if cert.Leaf != nil {
			attrs = append(attrs, "not_after", cert.Leaf.NotAfter)
		}
		slog.Info("OVN client certificate reloaded", attrs...)
	}
}

// handleReload runs one reload for Run's loop and reports its outcome in the
// log and the config_reload_total metric. It returns whether the reconcile
// interval changed, so Run can reset its ticker.
func (a *Agent) handleReload() bool {
	p, err := a.reload()
	if err != nil {
		slog.Error("configuration reload failed, keeping the running configuration", "error", err)
		recordConfigReload("error")
		return false
	}

	restartKeys := make([]string, 0, len(p.restartRequired))
	for _, c := range p.restartRequired {
		slog.Warn("configuration change needs a restart, keeping the running value", "key", c.Key, "reason", c.Reason)
		restartKeys = append(restartKeys, c.Key)
	}
	applied := p.applied
	if applied == nil {
		applied = []string{}
	}
	slog.Info("configuration reloaded", "applied", applied, "restart_required", restartKeys)
	recordConfigReload("success")

	if len(p.applied) > 0 {
		a.triggerReconcile()
	}
	return slices.Contains(p.applied, "reconcile_interval")
}

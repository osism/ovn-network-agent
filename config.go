package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var errVersionRequested = errors.New("version requested")

// StringOrSlice is a YAML type that accepts both a scalar string and a list of strings.
type StringOrSlice []string

func (s *StringOrSlice) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*s = []string{value.Value}
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*s = list
	return nil
}

// containedInAny returns true if ip is contained in any of the given networks.
// effectiveNetworkFilters returns manual config if non-empty, otherwise discovered networks.
// This is the single source of truth for the "manual takes precedence" rule, used by
// both NAT filtering (ovn.go) and veth-leak/prefix-list reconciliation (agent.go).
func effectiveNetworkFilters(manual, discovered []*net.IPNet) []*net.IPNet {
	if len(manual) > 0 {
		return manual
	}
	return discovered
}

func containedInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// splitAndTrim splits s by sep, trims whitespace, and returns non-empty parts.
func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// PortForwardRule describes a single DNAT port forwarding rule.
// A rule can have a single backend (DestAddr) or multiple backends (DestAddrs).
// When multiple backends are configured, nftables jhash-based source-IP hashing
// is used to consistently map clients to the same backend (sticky load balancing).
type PortForwardRule struct {
	Proto      string   `yaml:"proto"`      // "tcp" or "udp"
	Port       int      `yaml:"port"`       // incoming port on VIP (1-65535)
	DestAddr   string   `yaml:"dest_addr"`  // single backend IP address (mutually exclusive with dest_addrs)
	DestAddrs  []string `yaml:"dest_addrs"` // multiple backend IP addresses for sticky hashing
	DestPort   int      `yaml:"dest_port"`  // backend port (0 = same as Port)
	Masquerade *bool    `yaml:"masquerade"` // per-rule masquerade override (nil = inherit from VIP)
}

// effectiveMasquerade returns the masquerade setting for this rule.
// If the rule has an explicit setting, it wins. Otherwise the VIP-level default is used.
func (r *PortForwardRule) effectiveMasquerade(vipDefault bool) bool {
	if r.Masquerade != nil {
		return *r.Masquerade
	}
	return vipDefault
}

// destAddrs returns the effective list of backend addresses for the rule.
// It merges the singular DestAddr and plural DestAddrs fields.
func (r *PortForwardRule) destAddrs() []string {
	if len(r.DestAddrs) > 0 {
		return r.DestAddrs
	}
	if r.DestAddr != "" {
		return []string{r.DestAddr}
	}
	return nil
}

// PortForwardVIP describes a VIP with its forwarding rules.
type PortForwardVIP struct {
	VIP               string            `yaml:"vip"`                // anycast VIP address
	ManageVIP         bool              `yaml:"manage_vip"`         // agent adds/removes VIP on port_forward_dev
	Masquerade        bool              `yaml:"masquerade"`         // SNAT forwarded traffic with outgoing interface IP
	HairpinMasquerade bool              `yaml:"hairpin_masquerade"` // SNAT only traffic from provider networks (fixes hairpin NAT for FIPs)
	RouterMasquerade  bool              `yaml:"router_masquerade"`  // SNAT only traffic from known router SNAT IPs (fixes hairpin NAT for instances behind a router without FIP)
	Rules             []PortForwardRule `yaml:"rules"`
}

// Config holds all runtime configuration for the agent.
type Config struct {
	OVNSBRemote string
	OVNNBRemote string

	// OVNSSLCA, OVNSSLCert and OVNSSLKey are PEM file paths used to dial
	// ssl: OVN remotes. They are read once at startup; rotating the files
	// requires a restart.
	OVNSSLCA   string
	OVNSSLCert string
	OVNSSLKey  string

	// OVNTLS is derived by validateConfig from the three ovn-ssl-* path
	// options; nil when none of them are set.
	OVNTLS *tls.Config

	BridgeDev         string
	VRFName           string
	VethNexthop       string
	NetworkCIDRs      []string
	NetworkFilters    []*net.IPNet
	GatewayPort       string
	RouteTableID      int
	BridgeIP          string // IP to add to br-ex for ARP resolution (default: 169.254.169.254)
	OVSWrapper        string // e.g. "docker exec openvswitch_vswitchd" — prepended to ovs-vsctl/ovs-ofctl calls
	ReconcileInterval time.Duration
	LogLevel          string
	DryRun            bool

	// CheckConfig is CLI-only (--check-config): parse and validate the
	// configuration, then exit without running the agent. It has no
	// environment-variable or YAML binding.
	CheckConfig bool

	CleanupOnShutdown bool
	DrainOnShutdown   bool
	DrainTimeout      time.Duration

	// DrainSettleDelay is the safety margin held after the takeover chassis
	// signals readiness (via the NB readiness marker) — or, when no marker can
	// be expected, after the drain confirms the chassisredirect ports have
	// migrated away — before cleanup withdraws the FIP routes. The marker wait
	// itself is event-driven and bounded by drain_timeout, so this is only the
	// final margin, not the whole hold. 0 disables the readiness wait and the
	// margin entirely (cleanup runs as soon as the ports migrate).
	DrainSettleDelay time.Duration

	FRRPrefixList string // FRR prefix-list name to manage dynamically (e.g. ANNOUNCED-NETWORKS)

	// Stale chassis cleanup: grace period before removing OVN NB entries
	// from chassis that have disappeared from the SB Chassis table.
	// 0 = disabled.
	StaleChassisGracePeriod time.Duration

	// MetricsListen is the address the Prometheus /metrics endpoint binds
	// to (e.g. "127.0.0.1:9273"). Empty = disabled.
	MetricsListen string

	// Veth VRF leak settings
	VethLeakEnabled      bool
	VethProviderIP       string // IP of the veth-provider side (default: computed from VethNexthop + 1)
	VethLeakTableID      int    // Routing table for leak default route (default: 200)
	VethLeakRulePriority int    // Policy rule priority (default: 2000)

	// PortForwardOnly is derived by validateMode: true when no OVN remotes
	// are configured but port_forwards are. In this mode the agent runs as
	// a standalone VIP service and skips all OVN-dependent work.
	PortForwardOnly bool

	// Port forwarding (DNAT) settings
	PortForwardEnabled      bool             // derived: len(PortForwards) > 0
	PortForwardDev          string           // loopback device for VIP addresses (default: "loopback1")
	PortForwardTableID      int              // routing table for DNAT return traffic (default: 201)
	PortForwardL3mdevAccept bool             // set udp/tcp_l3mdev_accept=1 for cross-VRF same-host backends (default: false)
	PortForwardCTZone       int              // conntrack zone for DNAT flows; must not collide with OVN zones (default: 64000)
	PortForwards            []PortForwardVIP // VIP forwarding rules from config
}

// =============================================================================
// Configuration option registry
// =============================================================================
//
// Every configuration knob is declared exactly once, as one row of
// configOptions(). A row carries the option's CLI flag name, its default, its
// usage text and the Config field it binds to; the environment variable and the
// YAML key are *derived* from the flag name, so the three names an option is
// known by cannot drift apart.
//
// Adding an option is one row. Flag registration, the default, all three
// precedence layers (file < env < flag) and the generated reference docs follow
// from it — replacing the six hand-synchronised sites this previously took.
// tools/docgen parses this same table to produce docs/reference/.

// configOption is one configuration knob.
type configOption struct {
	// Flag is the CLI flag name, e.g. "reconcile-interval". Empty for
	// YAML-only options.
	Flag string
	// Key is the config-file key, e.g. "reconcile_interval". Derived from
	// Flag unless the option is YAML-only.
	Key string
	// Usage is the flag help text and the option's description in the docs.
	Usage string

	// register declares the flag on fs, bound to this option's own storage.
	// Nil for YAML-only options.
	register func(fs *flag.FlagSet)
	// applyDefault seeds cfg with the option's default.
	applyDefault func(cfg *Config)
	// applyFlag copies the parsed flag value into cfg. Called only for flags
	// the user set explicitly (fs.Visit), so an unset flag never overrides a
	// value from the file or the environment. Nil for YAML-only options.
	applyFlag func(cfg *Config) error
	// applyEnv parses an environment value into cfg. Nil for YAML-only options.
	applyEnv func(cfg *Config, v string) error
	// applyYAML decodes this option's config-file node into cfg.
	applyYAML func(cfg *Config, n *yaml.Node) error
}

// envVarName derives an option's environment variable from its flag name:
// "reconcile-interval" -> "OVN_NETWORK_RECONCILE_INTERVAL".
func envVarName(flagName string) string {
	return "OVN_NETWORK_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// yamlKeyName derives an option's config-file key from its flag name:
// "reconcile-interval" -> "reconcile_interval".
func yamlKeyName(flagName string) string {
	return strings.ReplaceAll(flagName, "-", "_")
}

// EnvVar returns the environment variable that sets this option, or "" when it
// has none (YAML-only options).
func (o configOption) EnvVar() string {
	if o.Flag == "" {
		return ""
	}
	return envVarName(o.Flag)
}

// newOpt builds the name/usage skeleton shared by every typed constructor.
func newOpt(flagName, usage string) configOption {
	return configOption{Flag: flagName, Key: yamlKeyName(flagName), Usage: usage}
}

// stringOpt declares a string option. The file layer treats an empty string as
// "not set", matching the historical `if fc.X != ""` behaviour.
func stringOpt(flagName, def, usage string, field func(*Config) *string) configOption {
	o := newOpt(flagName, usage)
	var v string
	o.register = func(fs *flag.FlagSet) { fs.StringVar(&v, flagName, def, usage) }
	o.applyDefault = func(cfg *Config) { *field(cfg) = def }
	o.applyFlag = func(cfg *Config) error { *field(cfg) = v; return nil }
	o.applyEnv = func(cfg *Config, s string) error { *field(cfg) = s; return nil }
	o.applyYAML = func(cfg *Config, n *yaml.Node) error {
		var s string
		if err := n.Decode(&s); err != nil {
			return fmt.Errorf("invalid %s: %w", o.Key, err)
		}
		if s != "" {
			*field(cfg) = s
		}
		return nil
	}
	return o
}

// boolOpt declares a boolean option. The flag is a real bool flag, so the bare
// `-dry-run` form keeps working; in the file layer the key's mere presence sets
// the value, which is what the old *bool config fields expressed.
func boolOpt(flagName string, def bool, usage string, field func(*Config) *bool) configOption {
	o := newOpt(flagName, usage)
	var v bool
	o.register = func(fs *flag.FlagSet) { fs.BoolVar(&v, flagName, def, usage) }
	o.applyDefault = func(cfg *Config) { *field(cfg) = def }
	o.applyFlag = func(cfg *Config) error { *field(cfg) = v; return nil }
	o.applyEnv = func(cfg *Config, s string) error {
		b, err := strconv.ParseBool(s)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %w", o.EnvVar(), s, err)
		}
		*field(cfg) = b
		return nil
	}
	o.applyYAML = func(cfg *Config, n *yaml.Node) error {
		var b bool
		if err := n.Decode(&b); err != nil {
			return fmt.Errorf("invalid %s: %w", o.Key, err)
		}
		*field(cfg) = b
		return nil
	}
	return o
}

// intOpt declares an integer option.
func intOpt(flagName string, def int, usage string, field func(*Config) *int) configOption {
	o := newOpt(flagName, usage)
	var v int
	o.register = func(fs *flag.FlagSet) { fs.IntVar(&v, flagName, def, usage) }
	o.applyDefault = func(cfg *Config) { *field(cfg) = def }
	o.applyFlag = func(cfg *Config) error { *field(cfg) = v; return nil }
	o.applyEnv = func(cfg *Config, s string) error {
		n, err := strconv.Atoi(s)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %w", o.EnvVar(), s, err)
		}
		*field(cfg) = n
		return nil
	}
	o.applyYAML = func(cfg *Config, n *yaml.Node) error {
		var i int
		if err := n.Decode(&i); err != nil {
			return fmt.Errorf("invalid %s: %w", o.Key, err)
		}
		*field(cfg) = i
		return nil
	}
	return o
}

// durationOpt declares a time.Duration option. The flag is registered as a
// string so the parse failure can name the flag ("-reconcile-interval") rather
// than surfacing flag package's generic duration error.
func durationOpt(flagName string, def time.Duration, usage string, field func(*Config) *time.Duration) configOption {
	o := newOpt(flagName, usage)
	var v string
	o.register = func(fs *flag.FlagSet) { fs.StringVar(&v, flagName, "", usage) }
	o.applyDefault = func(cfg *Config) { *field(cfg) = def }
	o.applyFlag = func(cfg *Config) error {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid -%s %q: %w", flagName, v, err)
		}
		*field(cfg) = d
		return nil
	}
	o.applyEnv = func(cfg *Config, s string) error {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %w", o.EnvVar(), s, err)
		}
		*field(cfg) = d
		return nil
	}
	o.applyYAML = func(cfg *Config, n *yaml.Node) error {
		var s string
		if err := n.Decode(&s); err != nil {
			return fmt.Errorf("invalid %s: %w", o.Key, err)
		}
		if s == "" {
			return nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("invalid %s %q: %w", o.Key, s, err)
		}
		*field(cfg) = d
		return nil
	}
	return o
}

// stringSliceOpt declares a list option. The flag and environment forms are
// comma-separated; the YAML form accepts either a scalar or a list
// (StringOrSlice).
func stringSliceOpt(flagName, usage string, field func(*Config) *[]string) configOption {
	o := newOpt(flagName, usage)
	var v string
	o.register = func(fs *flag.FlagSet) { fs.StringVar(&v, flagName, "", usage) }
	o.applyFlag = func(cfg *Config) error { *field(cfg) = splitAndTrim(v, ","); return nil }
	o.applyEnv = func(cfg *Config, s string) error { *field(cfg) = splitAndTrim(s, ","); return nil }
	o.applyYAML = func(cfg *Config, n *yaml.Node) error {
		var list StringOrSlice
		if err := n.Decode(&list); err != nil {
			return fmt.Errorf("invalid %s: %w", o.Key, err)
		}
		if len(list) > 0 {
			*field(cfg) = []string(list)
		}
		return nil
	}
	return o
}

// portForwardsOpt declares the port_forwards list, which has no flag or
// environment form: it is a nested structure that only the config file can
// express.
func portForwardsOpt(key, usage string, field func(*Config) *[]PortForwardVIP) configOption {
	return configOption{
		Key:   key,
		Usage: usage,
		applyYAML: func(cfg *Config, n *yaml.Node) error {
			var pfs []PortForwardVIP
			if err := n.Decode(&pfs); err != nil {
				return fmt.Errorf("invalid %s: %w", key, err)
			}
			if len(pfs) > 0 {
				*field(cfg) = pfs
			}
			return nil
		},
	}
}

// configOptions is the single declaration of every configuration option. Order
// is the order flags appear in --help and in the generated reference tables.
func configOptions() []configOption {
	return []configOption{
		stringOpt("ovn-sb-remote", "", "OVN Southbound DB remote, comma-separated for cluster",
			func(c *Config) *string { return &c.OVNSBRemote }),
		stringOpt("ovn-nb-remote", "", "OVN Northbound DB remote, comma-separated for cluster",
			func(c *Config) *string { return &c.OVNNBRemote }),
		stringOpt("ovn-ssl-ca", "", "Path to the CA certificate (PEM) that signed the OVN NB/SB server certificates, for ssl: remotes",
			func(c *Config) *string { return &c.OVNSSLCA }),
		stringOpt("ovn-ssl-cert", "", "Path to the client certificate (PEM) presented to OVN NB/SB for mutual TLS (requires ovn-ssl-key)",
			func(c *Config) *string { return &c.OVNSSLCert }),
		stringOpt("ovn-ssl-key", "", "Path to the private key (PEM) for ovn-ssl-cert (requires ovn-ssl-cert)",
			func(c *Config) *string { return &c.OVNSSLKey }),
		stringOpt("bridge-dev", "br-ex", "Provider bridge device for kernel routes",
			func(c *Config) *string { return &c.BridgeDev }),
		stringOpt("vrf-name", "vrf-provider", "VRF name for FRR routes",
			func(c *Config) *string { return &c.VRFName }),
		stringOpt("veth-nexthop", "169.254.0.1", "Nexthop IP for FRR static routes in the VRF",
			func(c *Config) *string { return &c.VethNexthop }),
		stringSliceOpt("network-cidr", "Filter FIPs by CIDRs (comma-separated, e.g. 10.0.0.0/24,172.16.0.0/12), empty = all",
			func(c *Config) *[]string { return &c.NetworkCIDRs }),
		stringOpt("gateway-port", "", "Chassisredirect port filter; empty = track all routers automatically",
			func(c *Config) *string { return &c.GatewayPort }),
		intOpt("route-table-id", 0, "Routing table ID for FIP routes (1-252); 0 = main table",
			func(c *Config) *int { return &c.RouteTableID }),
		stringOpt("bridge-ip", "169.254.169.254", "IP to add to bridge device for ARP resolution (default: 169.254.169.254)",
			func(c *Config) *string { return &c.BridgeIP }),
		stringOpt("ovs-wrapper", "", "Command prefix for ovs-vsctl/ovs-ofctl (e.g. 'docker exec openvswitch_vswitchd')",
			func(c *Config) *string { return &c.OVSWrapper }),
		durationOpt("reconcile-interval", 60*time.Second, "Full reconciliation interval (e.g. 60s, 5m)",
			func(c *Config) *time.Duration { return &c.ReconcileInterval }),
		stringOpt("log-level", "info", "Log level (debug, info, warn, error)",
			func(c *Config) *string { return &c.LogLevel }),
		boolOpt("dry-run", false, "Dry-run mode: connect and reconcile but only log what would be done",
			func(c *Config) *bool { return &c.DryRun }),
		boolOpt("cleanup-on-shutdown", true, "Remove all managed routes on shutdown (SIGINT/SIGTERM)",
			func(c *Config) *bool { return &c.CleanupOnShutdown }),
		boolOpt("drain-on-shutdown", true, "Drain HA gateways before shutdown by lowering Gateway_Chassis priority",
			func(c *Config) *bool { return &c.DrainOnShutdown }),
		durationOpt("drain-timeout", 60*time.Second, "Max time to wait for gateway drain (e.g. 60s)",
			func(c *Config) *time.Duration { return &c.DrainTimeout }),
		durationOpt("drain-settle-delay", 500*time.Millisecond, "Safety margin held after the takeover chassis signals readiness, before cleanup (e.g. 500ms; 0 disables the hold)",
			func(c *Config) *time.Duration { return &c.DrainSettleDelay }),
		stringOpt("frr-prefix-list", "ANNOUNCED-NETWORKS", "FRR prefix-list name to manage dynamically (default: ANNOUNCED-NETWORKS)",
			func(c *Config) *string { return &c.FRRPrefixList }),
		durationOpt("stale-chassis-grace-period", 5*time.Minute, "Grace period before cleaning up entries from missing chassis (e.g. 5m, 0 to disable)",
			func(c *Config) *time.Duration { return &c.StaleChassisGracePeriod }),
		boolOpt("veth-leak-enabled", true, "Enable veth VRF route leaking",
			func(c *Config) *bool { return &c.VethLeakEnabled }),
		stringOpt("veth-provider-ip", "", "IP of the veth-provider side (default: veth-nexthop + 1)",
			func(c *Config) *string { return &c.VethProviderIP }),
		intOpt("veth-leak-table-id", 200, "Routing table ID for veth leak default route (1-252)",
			func(c *Config) *int { return &c.VethLeakTableID }),
		intOpt("veth-leak-rule-priority", 2000, "Policy rule priority for veth leak rules",
			func(c *Config) *int { return &c.VethLeakRulePriority }),
		stringOpt("metrics-listen", "", "Address for the Prometheus /metrics endpoint (e.g. 127.0.0.1:9273); empty = disabled",
			func(c *Config) *string { return &c.MetricsListen }),
		stringOpt("port-forward-dev", "loopback1", "Loopback device for VIP addresses in VRF (default: loopback1)",
			func(c *Config) *string { return &c.PortForwardDev }),
		intOpt("port-forward-table-id", 201, "Routing table ID for DNAT return traffic (1-252)",
			func(c *Config) *int { return &c.PortForwardTableID }),
		boolOpt("port-forward-l3mdev-accept", false, "Set udp/tcp_l3mdev_accept=1 for cross-VRF same-host DNAT backends",
			func(c *Config) *bool { return &c.PortForwardL3mdevAccept }),
		intOpt("port-forward-ct-zone", 64000, "Conntrack zone for DNAT flows (must not collide with OVN zones, 1-65535)",
			func(c *Config) *int { return &c.PortForwardCTZone }),
		portForwardsOpt("port_forwards", "See sample config for usage.",
			func(c *Config) *[]PortForwardVIP { return &c.PortForwards }),
	}
}

// loadConfig builds the configuration with the following priority
// (highest wins): CLI flags > environment variables > config file > defaults.
func loadConfig(args []string) (Config, error) {
	fs := flag.NewFlagSet("ovn-network-agent", flag.ContinueOnError)

	// CLI-only action flags. These select what the process does rather than
	// configure how it behaves, so they have no environment or YAML binding
	// and stay outside the option registry.
	configPath := fs.String("config", os.Getenv("OVN_NETWORK_CONFIG"), "Path to YAML config file")
	showVersion := fs.Bool("version", false, "Print version and exit")
	fCheckConfig := fs.Bool("check-config", false, "Validate configuration and exit (exit code 0 = valid, 1 = invalid)")

	opts := configOptions()
	for _, o := range opts {
		if o.register != nil {
			o.register(fs)
		}
	}

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	if *showVersion {
		return Config{}, errVersionRequested
	}

	// Layer 0: defaults
	var cfg Config
	for _, o := range opts {
		if o.applyDefault != nil {
			o.applyDefault(&cfg)
		}
	}

	// Layer 1: config file
	if *configPath != "" {
		doc, err := readConfigFile(*configPath)
		if err != nil {
			return Config{}, fmt.Errorf("load config %s: %w", *configPath, err)
		}
		if err := applyFileConfig(&cfg, doc); err != nil {
			return Config{}, fmt.Errorf("load config %s: %w", *configPath, err)
		}
	}

	// Layer 2: environment variables
	if err := applyEnvConfig(&cfg); err != nil {
		return Config{}, err
	}

	// Layer 3: CLI flags (only explicitly set ones)
	byFlag := make(map[string]configOption, len(opts))
	for _, o := range opts {
		if o.Flag != "" {
			byFlag[o.Flag] = o
		}
	}
	var flagErr error
	fs.Visit(func(f *flag.Flag) {
		if flagErr != nil {
			return
		}
		o, ok := byFlag[f.Name]
		if !ok || o.applyFlag == nil {
			return
		}
		if err := o.applyFlag(&cfg); err != nil {
			flagErr = err
		}
	})
	if flagErr != nil {
		return Config{}, flagErr
	}

	// --check-config is a CLI-only action flag (parse + validate + exit);
	// it has no env/YAML binding, so it is read straight from the flag.
	cfg.CheckConfig = *fCheckConfig

	// Validate configuration
	if err := validateConfig(&cfg); err != nil {
		return Config{}, err
	}

	// Derive the operating mode (full vs port-forward-only) and reject
	// invalid OVN/port-forward combinations.
	if err := validateMode(&cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// validateMode derives the agent's operating mode from the presence of OVN
// remotes and port-forward configuration, setting Config.PortForwardOnly and
// rejecting invalid combinations:
//
//	ovn_sb_remote + ovn_nb_remote   port_forwards   mode
//	------------------------------- --------------- -----------------------
//	both set                        any             full mode
//	both empty                      non-empty       port-forward-only mode
//	both empty                      empty           error — nothing to do
//	exactly one set                 any             error — incomplete OVN
//
// In port-forward-only mode it additionally rejects masquerade options that
// depend on OVN-derived state: router_masquerade has no router-SNAT-IP source
// without OVN, and hairpin_masquerade needs an explicit network_cidr because
// provider CIDRs are otherwise auto-discovered from OVN.
func validateMode(cfg *Config) error {
	sbSet := cfg.OVNSBRemote != ""
	nbSet := cfg.OVNNBRemote != ""

	switch {
	case sbSet && nbSet:
		cfg.PortForwardOnly = false
	case !sbSet && !nbSet:
		if len(cfg.PortForwards) == 0 {
			return fmt.Errorf("nothing to do: set ovn-sb-remote and ovn-nb-remote for full mode, or configure port_forwards for port-forward-only mode")
		}
		cfg.PortForwardOnly = true
	default:
		return fmt.Errorf("incomplete OVN configuration: ovn-sb-remote and ovn-nb-remote must both be set, or both be empty to run in port-forward-only mode")
	}

	if !cfg.PortForwardOnly {
		return nil
	}

	for i, pf := range cfg.PortForwards {
		if pf.RouterMasquerade {
			return fmt.Errorf("port_forwards[%d] (vip=%s): router_masquerade requires an OVN connection — router SNAT IPs cannot be discovered in port-forward-only mode", i, pf.VIP)
		}
		if pf.HairpinMasquerade && len(cfg.NetworkCIDRs) == 0 {
			return fmt.Errorf("port_forwards[%d] (vip=%s): hairpin_masquerade in port-forward-only mode requires network_cidr to be set — provider CIDRs cannot be auto-discovered without OVN", i, pf.VIP)
		}
	}

	return nil
}

// ovnTLSConfig builds the *tls.Config used to dial ssl: OVN remotes from the
// ovn-ssl-ca/-cert/-key path options. It returns (nil, nil) when none are set,
// so a plain tcp: deployment carries no TLS material. The CA, when given,
// becomes RootCAs so server certificates verify against the operator's private
// CA instead of the system trust store; the cert/key pair (which must be set
// together) becomes the client certificate for mutual TLS; the key file is
// rejected unless its mode keeps it off-limits to group and other. Reading the
// files here means a bad path fails the config load — and --check-config —
// rather than the first dial.
func ovnTLSConfig(caPath, certPath, keyPath string) (*tls.Config, error) {
	if caPath == "" && certPath == "" && keyPath == "" {
		return nil, nil
	}
	if (certPath == "") != (keyPath == "") {
		return nil, fmt.Errorf("ovn-ssl-cert and ovn-ssl-key must be set together")
	}

	tc := &tls.Config{MinVersion: tls.VersionTLS12}

	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read ovn-ssl-ca %q: %w", caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ovn-ssl-ca %q: no PEM certificates found", caPath)
		}
		tc.RootCAs = pool
	}

	if certPath != "" {
		// The private key is what lets anything speak for this agent
		// against the write-capable NB database, so refuse to load one
		// that other local accounts can read. A config-management run
		// that forgets the mode would otherwise hand every account on
		// the gateway node a working client identity, silently.
		fi, err := os.Stat(keyPath)
		if err != nil {
			return nil, fmt.Errorf("stat ovn-ssl-key %q: %w", keyPath, err)
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			return nil, fmt.Errorf("ovn-ssl-key %q has mode %04o: the private key must not be group- or world-accessible (chmod 0600)", keyPath, perm)
		}
		cert, err := tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("load ovn-ssl-cert %q / ovn-ssl-key %q: %w", certPath, keyPath, err)
		}
		// LoadX509KeyPair checks that the key matches the certificate but
		// never looks at the validity period, so an expired leaf loads
		// cleanly and passes --check-config while every handshake it is
		// used for is rejected. Warn instead of failing: an agent that
		// refuses to start mid-renewal is worse than one that reconnects
		// as soon as the new PEMs land.
		if leaf := cert.Leaf; leaf != nil && time.Now().After(leaf.NotAfter) {
			slog.Warn("ovn-ssl-cert has expired: OVN TLS handshakes will be rejected until it is renewed and the agent restarted",
				"path", certPath, "not_after", leaf.NotAfter)
		}
		tc.Certificates = []tls.Certificate{cert}
	}

	return tc, nil
}

// remoteUsesSSL reports whether an OVN remote list dials over TLS. Every
// endpoint in the list must agree: an all-ssl: list uses TLS, a list with no
// ssl: endpoints (including the empty list) does not. A list that mixes ssl:
// with plaintext endpoints is rejected — libovsdb fails over between the
// entries transparently, so a single tcp: entry would let a connection
// silently downgrade away from TLS.
//
// Schemes are matched case-insensitively because libovsdb hands each endpoint
// to net/url, which lowercases the scheme before dispatching: "SSL:" dials TLS
// there. Classifying it as plaintext here would let "SSL:host,tcp:host" past
// the mixing check and reinstate exactly the downgrade this guards against.
// An endpoint whose scheme libovsdb cannot dial at all is rejected here rather
// than at the first connect, so --check-config catches the typo.
func remoteUsesSSL(flagName, remote string) (bool, error) {
	var ssl, plain int
	for _, endpoint := range ovsdbEndpoints(remote) {
		scheme, _, ok := strings.Cut(endpoint, ":")
		if !ok {
			return false, fmt.Errorf("invalid %s %q: endpoint %q has no scheme", flagName, remote, endpoint)
		}
		switch strings.ToLower(scheme) {
		case "ssl":
			ssl++
		case "tcp", "unix":
			plain++
		default:
			return false, fmt.Errorf("invalid %s %q: unsupported endpoint scheme %q", flagName, remote, scheme)
		}
	}
	if ssl > 0 && plain > 0 {
		return false, fmt.Errorf("invalid %s %q: mixes ssl: and non-ssl endpoints — use one scheme for the whole list so a failover cannot silently downgrade to plaintext", flagName, remote)
	}
	return ssl > 0, nil
}

func validateConfig(cfg *Config) error {
	if net.ParseIP(cfg.VethNexthop) == nil {
		return fmt.Errorf("invalid veth-nexthop IP: %q", cfg.VethNexthop)
	}
	if cfg.VRFName != "" && !isValidIdentifier(cfg.VRFName) {
		return fmt.Errorf("invalid vrf-name: %q (only alphanumeric, hyphen, underscore, dot allowed)", cfg.VRFName)
	}
	if cfg.RouteTableID < 0 || cfg.RouteTableID > 252 {
		return fmt.Errorf("invalid route-table-id: %d (must be 0-252)", cfg.RouteTableID)
	}
	if len(cfg.NetworkCIDRs) > 0 {
		cfg.NetworkFilters = make([]*net.IPNet, 0, len(cfg.NetworkCIDRs))
		for _, cidrStr := range cfg.NetworkCIDRs {
			_, cidr, err := net.ParseCIDR(cidrStr)
			if err != nil {
				return fmt.Errorf("invalid network-cidr %q: %w", cidrStr, err)
			}
			cfg.NetworkFilters = append(cfg.NetworkFilters, cidr)
		}
	}

	// FRR prefix-list validation
	if cfg.FRRPrefixList != "" && !isValidIdentifier(cfg.FRRPrefixList) {
		return fmt.Errorf("invalid frr-prefix-list: %q (only alphanumeric, hyphen, underscore, dot allowed)", cfg.FRRPrefixList)
	}

	// TLS for ssl: OVN remotes. Reject a remote list that mixes ssl: with
	// plaintext endpoints (a failover must not silently downgrade), then
	// derive the *tls.Config from the ovn-ssl-* path options.
	sbSSL, err := remoteUsesSSL("ovn-sb-remote", cfg.OVNSBRemote)
	if err != nil {
		return err
	}
	nbSSL, err := remoteUsesSSL("ovn-nb-remote", cfg.OVNNBRemote)
	if err != nil {
		return err
	}
	cfg.OVNTLS, err = ovnTLSConfig(cfg.OVNSSLCA, cfg.OVNSSLCert, cfg.OVNSSLKey)
	if err != nil {
		return err
	}
	// Warn — rather than reject — on the suspicious-but-working
	// combinations, matching the unknown-config-key precedent in
	// applyFileConfig. Every ssl: remote that reaches a dial without a
	// RootCAs pin trusts the host's system root store, so any publicly
	// trusted CA can impersonate the databases; TLS material with no ssl:
	// remote is merely inert.
	if (sbSSL || nbSSL) && cfg.OVNTLS == nil {
		slog.Warn("ssl: OVN remotes configured without ovn-ssl-ca/ovn-ssl-cert/ovn-ssl-key: server certificates must chain to the system trust store and no client certificate will be presented")
	}
	if (sbSSL || nbSSL) && cfg.OVNTLS != nil && cfg.OVNTLS.RootCAs == nil {
		slog.Warn("ovn-ssl-cert/ovn-ssl-key are set without ovn-ssl-ca: OVN server certificates are verified against the system trust store, so any publicly trusted CA can impersonate the databases — set ovn-ssl-ca to pin the deployment's own CA")
	}
	if cfg.OVNTLS != nil && !sbSSL && !nbSSL {
		slog.Warn("ovn-ssl-ca/ovn-ssl-cert/ovn-ssl-key are set but no OVN remote uses ssl:, so the TLS options have no effect")
	}
	// Per-database schemes are allowed so operators can migrate NB and SB
	// separately, but the half-migrated state must not look like the finished
	// one: the cleartext side still carries chassis and Port_Binding data that
	// becomes kernel routes, FRR announcements and NAT rules here.
	if sbSSL != nbSSL {
		slog.Warn("OVN remotes use mixed schemes: one database is dialed over TLS and the other in cleartext",
			"ovn_sb_remote_tls", sbSSL, "ovn_nb_remote_tls", nbSSL)
	}

	// ReconcileInterval feeds time.NewTicker, which panics on a
	// non-positive duration — reject it here so a bad value fails the
	// config load instead of crashing the agent after it has already
	// mutated system state.
	if cfg.ReconcileInterval <= 0 {
		return fmt.Errorf("invalid reconcile-interval: %v (must be positive)", cfg.ReconcileInterval)
	}

	if cfg.DrainTimeout < 0 {
		return fmt.Errorf("invalid drain-timeout: must be non-negative")
	}
	if cfg.DrainOnShutdown && cfg.DrainTimeout == 0 {
		return fmt.Errorf("drain-on-shutdown requires a positive drain-timeout")
	}
	if cfg.DrainSettleDelay < 0 {
		return fmt.Errorf("invalid drain-settle-delay: must be non-negative")
	}

	if cfg.StaleChassisGracePeriod < 0 {
		return fmt.Errorf("invalid stale-chassis-grace-period: must be non-negative")
	}

	if cfg.MetricsListen != "" {
		if _, _, err := net.SplitHostPort(cfg.MetricsListen); err != nil {
			return fmt.Errorf("invalid metrics-listen %q: %w", cfg.MetricsListen, err)
		}
	}

	// Veth leak validation
	if cfg.VethLeakEnabled {
		if cfg.VethLeakTableID < 1 || cfg.VethLeakTableID > 252 {
			return fmt.Errorf("invalid veth-leak-table-id: %d (must be 1-252)", cfg.VethLeakTableID)
		}
		// RouteTableID 0 means "main table" and cannot conflict with an explicit leak table.
		if cfg.VethLeakTableID == cfg.RouteTableID && cfg.RouteTableID != 0 {
			return fmt.Errorf("veth-leak-table-id (%d) must not equal route-table-id (%d)", cfg.VethLeakTableID, cfg.RouteTableID)
		}
		// Auto-compute VethProviderIP from VethNexthop + 1 if not set.
		if cfg.VethProviderIP == "" {
			nexthopIP := net.ParseIP(cfg.VethNexthop)
			cfg.VethProviderIP = nextIPInSubnet(nexthopIP).String()
		}
		if net.ParseIP(cfg.VethProviderIP) == nil {
			return fmt.Errorf("invalid veth-provider-ip: %q", cfg.VethProviderIP)
		}
	}

	// Port forward validation
	cfg.PortForwardEnabled = len(cfg.PortForwards) > 0
	if cfg.PortForwardEnabled {
		if cfg.PortForwardTableID < 1 || cfg.PortForwardTableID > 252 {
			return fmt.Errorf("invalid port-forward-table-id: %d (must be 1-252)", cfg.PortForwardTableID)
		}
		if cfg.PortForwardTableID == cfg.RouteTableID && cfg.RouteTableID != 0 {
			return fmt.Errorf("port-forward-table-id (%d) must not equal route-table-id (%d)", cfg.PortForwardTableID, cfg.RouteTableID)
		}
		if cfg.PortForwardCTZone < 1 || cfg.PortForwardCTZone > 65535 {
			return fmt.Errorf("invalid port-forward-ct-zone: %d (must be 1-65535)", cfg.PortForwardCTZone)
		}
		if cfg.PortForwardTableID == cfg.VethLeakTableID {
			return fmt.Errorf("port-forward-table-id (%d) must not equal veth-leak-table-id (%d)", cfg.PortForwardTableID, cfg.VethLeakTableID)
		}
		if !isValidIdentifier(cfg.PortForwardDev) {
			return fmt.Errorf("invalid port-forward-dev: %q", cfg.PortForwardDev)
		}
		if !cfg.VethLeakEnabled {
			return fmt.Errorf("port forwarding requires veth_leak_enabled=true (needs veth pair for return traffic)")
		}
		seenVIPs := make(map[string]bool, len(cfg.PortForwards))
		for i, pf := range cfg.PortForwards {
			vipIP := net.ParseIP(pf.VIP)
			if vipIP == nil {
				return fmt.Errorf("port_forwards[%d]: invalid VIP: %q", i, pf.VIP)
			}
			if vipIP.To4() == nil {
				return fmt.Errorf("port_forwards[%d]: IPv6 VIP not supported: %q (only IPv4)", i, pf.VIP)
			}
			if seenVIPs[pf.VIP] {
				return fmt.Errorf("port_forwards[%d]: duplicate VIP: %q", i, pf.VIP)
			}
			seenVIPs[pf.VIP] = true
			if len(pf.Rules) == 0 {
				return fmt.Errorf("port_forwards[%d] (vip=%s): no rules defined", i, pf.VIP)
			}
			seenRules := make(map[string]bool, len(pf.Rules))
			for j := range pf.Rules {
				r := &pf.Rules[j]
				if r.Proto != "tcp" && r.Proto != "udp" {
					return fmt.Errorf("port_forwards[%d].rules[%d]: invalid proto %q (must be tcp or udp)", i, j, r.Proto)
				}
				if r.Port < 1 || r.Port > 65535 {
					return fmt.Errorf("port_forwards[%d].rules[%d]: invalid port %d (must be 1-65535)", i, j, r.Port)
				}
				ruleKey := fmt.Sprintf("%s/%d", r.Proto, r.Port)
				if seenRules[ruleKey] {
					return fmt.Errorf("port_forwards[%d].rules[%d]: duplicate %s port %d on VIP %s", i, j, r.Proto, r.Port, pf.VIP)
				}
				seenRules[ruleKey] = true

				// Validate dest_addr / dest_addrs: exactly one must be set.
				if r.DestAddr != "" && len(r.DestAddrs) > 0 {
					return fmt.Errorf("port_forwards[%d].rules[%d]: dest_addr and dest_addrs are mutually exclusive", i, j)
				}
				addrs := r.destAddrs()
				if len(addrs) == 0 {
					return fmt.Errorf("port_forwards[%d].rules[%d]: dest_addr or dest_addrs required", i, j)
				}
				const maxBackends = 256
				if len(addrs) > maxBackends {
					return fmt.Errorf("port_forwards[%d].rules[%d]: dest_addrs has %d entries (max %d)", i, j, len(addrs), maxBackends)
				}
				seen := make(map[string]bool, len(addrs))
				for k, addr := range addrs {
					destIP := net.ParseIP(addr)
					if destIP == nil {
						return fmt.Errorf("port_forwards[%d].rules[%d]: invalid dest_addr[%d]: %q", i, j, k, addr)
					}
					if destIP.To4() == nil {
						return fmt.Errorf("port_forwards[%d].rules[%d]: IPv6 dest_addr not supported: %q (only IPv4)", i, j, addr)
					}
					if seen[addr] {
						return fmt.Errorf("port_forwards[%d].rules[%d]: duplicate dest_addr: %q", i, j, addr)
					}
					seen[addr] = true
				}
				if r.DestPort < 0 || r.DestPort > 65535 {
					return fmt.Errorf("port_forwards[%d].rules[%d]: invalid dest_port %d (must be 0-65535)", i, j, r.DestPort)
				}
			}
		}
	}

	return nil
}

// nextIPInSubnet returns the next IP address after the given IP.
// NOTE: wraps around to 0.0.0.0 for 255.255.255.255 — callers must validate.
func nextIPInSubnet(ip net.IP) net.IP {
	ip = ip.To4()
	if ip == nil {
		return nil
	}
	next := make(net.IP, len(ip))
	copy(next, ip)
	for i := len(next) - 1; i >= 0; i-- {
		next[i]++
		if next[i] != 0 {
			break
		}
	}
	return next
}

// isValidIdentifier checks that a string contains only safe characters
// for use in shell commands (alphanumeric, hyphen, underscore, dot).
func isValidIdentifier(s string) bool {
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return len(s) > 0
}

// configDoc is a config file decoded into raw YAML nodes, keyed by top-level
// key. Decoding into nodes rather than a mirror struct is what removes the
// second place an option had to be declared: applyFileConfig walks the option
// registry and asks each option to decode its own node.
type configDoc map[string]yaml.Node

func readConfigFile(path string) (configDoc, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc configDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// applyFileConfig applies every config-file key that maps to a registry option.
//
// Keys the registry does not know are warned about but never rejected: a typo
// like "drain_on_shutdow" would otherwise be dropped and the default applied
// silently — exactly where a wrong effective value (drain/cleanup) turns a
// routine reboot into an outage — while still letting a newer config file load
// on an older agent.
func applyFileConfig(cfg *Config, doc configDoc) error {
	known := make(map[string]bool, len(doc))
	for _, o := range configOptions() {
		if o.applyYAML == nil {
			continue
		}
		known[o.Key] = true
		node, ok := doc[o.Key]
		if !ok {
			continue
		}
		if err := o.applyYAML(cfg, &node); err != nil {
			return err
		}
	}

	var unknown []string
	for key := range doc {
		if !known[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		slog.Warn("config file contains unknown keys (ignored — check for typos)",
			"keys", strings.Join(unknown, ", "))
	}
	return nil
}

// applyEnvConfig applies every option whose environment variable is set to a
// non-empty value. The variable name is derived from the flag name, so it can
// never drift from it.
func applyEnvConfig(cfg *Config) error {
	for _, o := range configOptions() {
		if o.applyEnv == nil {
			continue
		}
		v := os.Getenv(o.EnvVar())
		if v == "" {
			continue
		}
		if err := o.applyEnv(cfg, v); err != nil {
			return err
		}
	}
	return nil
}

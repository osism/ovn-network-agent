package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	backoff "github.com/cenkalti/backoff/v4"
	"github.com/ovn-kubernetes/libovsdb/cache"
	"github.com/ovn-kubernetes/libovsdb/client"
	"github.com/ovn-kubernetes/libovsdb/model"
	"github.com/ovn-kubernetes/libovsdb/ovsdb"
)

const eventDebounceInterval = 500 * time.Millisecond

// reconcileDebounceInterval coalesces multiple state refreshes (SB + NB)
// into a single reconciliation trigger.
const reconcileDebounceInterval = 100 * time.Millisecond

// =============================================================================
// OVN Southbound DB models
// =============================================================================

type SBPortBinding struct {
	UUID                       string            `ovsdb:"_uuid"`
	Datapath                   string            `ovsdb:"datapath"`
	TunnelKey                  int               `ovsdb:"tunnel_key"`
	LogicalPort                string            `ovsdb:"logical_port"`
	Type                       string            `ovsdb:"type"`
	Chassis                    *string           `ovsdb:"chassis"`
	AdditionalChassis          []string          `ovsdb:"additional_chassis"`
	Encap                      *string           `ovsdb:"encap"`
	AdditionalEncap            []string          `ovsdb:"additional_encap"`
	Options                    map[string]string `ovsdb:"options"`
	ParentPort                 *string           `ovsdb:"parent_port"`
	Tag                        *int              `ovsdb:"tag"`
	Mac                        []string          `ovsdb:"mac"`
	NatAddresses               []string          `ovsdb:"nat_addresses"`
	Up                         *bool             `ovsdb:"up"`
	ExternalIDs                map[string]string `ovsdb:"external_ids"`
	GatewayChassis             []string          `ovsdb:"gateway_chassis"`
	HaChassisGroup             *string           `ovsdb:"ha_chassis_group"`
	VirtualParent              *string           `ovsdb:"virtual_parent"`
	RequestedChassis           *string           `ovsdb:"requested_chassis"`
	RequestedAdditionalChassis []string          `ovsdb:"requested_additional_chassis"`
	MirrorRules                []string          `ovsdb:"mirror_rules"`
}

type SBChassis struct {
	UUID        string            `ovsdb:"_uuid"`
	Name        string            `ovsdb:"name"`
	Hostname    string            `ovsdb:"hostname"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

// =============================================================================
// OVN Northbound DB models
// =============================================================================

type NBNAT struct {
	UUID              string            `ovsdb:"_uuid"`
	Type              string            `ovsdb:"type"`
	ExternalIP        string            `ovsdb:"external_ip"`
	ExternalMAC       *string           `ovsdb:"external_mac"`
	ExternalPortRange string            `ovsdb:"external_port_range"`
	LogicalIP         string            `ovsdb:"logical_ip"`
	LogicalPort       *string           `ovsdb:"logical_port"`
	GatewayPort       *string           `ovsdb:"gateway_port"`
	Match             string            `ovsdb:"match"`
	Priority          int               `ovsdb:"priority"`
	Options           map[string]string `ovsdb:"options"`
	AllowedExtIPs     *string           `ovsdb:"allowed_ext_ips"`
	ExemptedExtIPs    *string           `ovsdb:"exempted_ext_ips"`
	ExternalIDs       map[string]string `ovsdb:"external_ids"`
}

type NBLogicalRouter struct {
	UUID         string            `ovsdb:"_uuid"`
	Name         string            `ovsdb:"name"`
	Ports        []string          `ovsdb:"ports"`
	Nat          []string          `ovsdb:"nat"`
	StaticRoutes []string          `ovsdb:"static_routes"`
	ExternalIDs  map[string]string `ovsdb:"external_ids"`
}

type NBLogicalRouterPort struct {
	UUID     string   `ovsdb:"_uuid"`
	Name     string   `ovsdb:"name"`
	MAC      string   `ovsdb:"mac"`
	Networks []string `ovsdb:"networks"`
}

type NBLogicalRouterStaticRoute struct {
	UUID        string            `ovsdb:"_uuid"`
	IPPrefix    string            `ovsdb:"ip_prefix"`
	Nexthop     string            `ovsdb:"nexthop"`
	OutputPort  *string           `ovsdb:"output_port"`
	Policy      *string           `ovsdb:"policy"`
	Options     map[string]string `ovsdb:"options"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
}

type NBStaticMACBinding struct {
	UUID        string `ovsdb:"_uuid"`
	LogicalPort string `ovsdb:"logical_port"`
	IP          string `ovsdb:"ip"`
	MAC         string `ovsdb:"mac"`
}

type NBGatewayChassis struct {
	UUID        string            `ovsdb:"_uuid"`
	ChassisName string            `ovsdb:"chassis_name"`
	Name        string            `ovsdb:"name"`
	Priority    int               `ovsdb:"priority"`
	ExternalIDs map[string]string `ovsdb:"external_ids"`
	Options     map[string]string `ovsdb:"options"`
}

// =============================================================================
// OVN Client wrapper
// =============================================================================

// ovsdbClient is the interface for OVSDB clients.
// The SB client is read-only; the NB client also performs writes
// (static routes, MAC bindings).
type ovsdbClient interface {
	Connect(context.Context) error
	Close()
	Connected() bool
	Cache() *cache.TableCache
	NewMonitor(...client.MonitorOption) *client.Monitor
	Monitor(context.Context, *client.Monitor) (client.MonitorCookie, error)
	List(ctx context.Context, result interface{}) error
	Create(models ...model.Model) ([]ovsdb.Operation, error)
	Where(models ...model.Model) client.ConditionalAPI
	Transact(ctx context.Context, ops ...ovsdb.Operation) ([]ovsdb.OperationResult, error)
}

// LocalnetSegment describes the localnet segment (provider network) that a
// locally-active router's external network is attached to. A nil VLANTag
// means a flat (untagged) network.
type LocalnetSegment struct {
	LocalnetPort string // SB localnet Port_Binding logical_port name
	NetworkName  string // options:network_name of the localnet port (the physnet)
	VLANTag      *int   // SB Port_Binding tag; nil = flat
}

// LocalRouterInfo describes a logical router whose gateway is active on this chassis.
type LocalRouterInfo struct {
	RouterName  string           // NB Logical_Router name
	RouterUUID  string           // NB Logical_Router UUID
	LRPName     string           // NB Logical_Router_Port name (e.g. "lrp-abc123")
	LRPUUID     string           // NB Logical_Router_Port UUID
	LRPMAC      string           // NB Logical_Router_Port MAC (e.g. "fa:16:3e:xx:xx:xx")
	LRPNetworks []string         // NB Logical_Router_Port networks (e.g. ["198.51.100.11/24"])
	CRPort      string           // SB chassisredirect logical_port (e.g. "cr-lrp-abc123")
	Segment     *LocalnetSegment // localnet segment of the external network; nil = unresolved
}

// segmentName returns the localnet port name of a router's segment, or ""
// (the fallback segment) when the segment is unresolved.
func segmentName(seg *LocalnetSegment) string {
	if seg == nil {
		return ""
	}
	return seg.LocalnetPort
}

type OVNState struct {
	mu sync.RWMutex

	// Derived state
	LocalChassisName string

	// Multi-router gateway state: routers whose chassisredirect port
	// is active on this chassis.
	LocalRouters    []LocalRouterInfo
	HasLocalRouters bool

	// NAT entries from NB, filtered to only locally-active routers.
	FIPs    []string // dnat_and_snat external IPs
	SNATIPs []string // snat external IPs

	// NATIPToRouterMAC maps each FIP/SNAT external IP to the MAC of the
	// router port that owns it. Used by hairpin flows to set dl_dst so
	// that OVN's L2 lookup delivers the reflected packet to the correct
	// router port.
	NATIPToRouterMAC map[string]string

	// NATIPToSegment maps each FIP/SNAT external IP to the localnet port
	// name of the segment its external network is on. IPs whose segment
	// is unresolved are absent from the map (the "" fallback segment).
	NATIPToSegment map[string]string

	// Networks auto-discovered from Logical_Router_Port.Networks of locally-active routers.
	DiscoveredNetworks []*net.IPNet

	// AllChassisNames is the set of chassis hostnames currently present in
	// the SB Chassis table. Used for stale chassis cleanup.
	AllChassisNames map[string]bool
}

type OVNClient struct {
	sbClient ovsdbClient
	nbClient ovsdbClient
	state    *OVNState
	cfg      Config

	onChange func() // callback when state changes

	// ready is set to true after Connect() completes initial setup.
	// Event handlers check this to avoid signalling refreshes before
	// both databases are connected and monitored. Atomic because cache
	// event handlers can read it from other goroutines while Connect()
	// is still completing setup.
	ready atomic.Bool

	// debounceCh receives signals from cache event handlers for tables that
	// should trigger a debounced state refresh. immediateCh receives signals
	// for chassisredirect changes that bypass debouncing for fast HA
	// failover. Both are buffered with capacity 1 so a storm of events
	// coalesces: at most one signal is queued, so the refresh loop runs at
	// most one in-flight pass plus one follow-up.
	debounceCh  chan struct{}
	immediateCh chan struct{}

	// drainWatchCh receives signals from the SB event handler on
	// chassisredirect Port_Binding changes. Unlike debounceCh/immediateCh it
	// does not feed the refresh loop; DrainGateways consumes it so the drain
	// wait re-checks countLocalCRPorts the moment a port migrates, instead of
	// waiting for the fixed safety re-poll. Buffered with capacity 1 so a
	// storm of events coalesces into at most one queued wake-up.
	drainWatchCh chan struct{}

	// loopDone is closed when refreshLoop exits. Set by Connect() before
	// starting the loop; nil before Connect() succeeds.
	loopDone chan struct{}
}

func NewOVNClient(cfg Config, onChange func()) *OVNClient {
	return &OVNClient{
		cfg:          cfg,
		state:        &OVNState{},
		onChange:     onChange,
		debounceCh:   make(chan struct{}, 1),
		immediateCh:  make(chan struct{}, 1),
		drainWatchCh: make(chan struct{}, 1),
	}
}

func (o *OVNClient) Connect(ctx context.Context) error {
	hostname, err := getHostname()
	if err != nil {
		return fmt.Errorf("get hostname: %w", err)
	}
	o.state.mu.Lock()
	o.state.LocalChassisName = hostname
	o.state.mu.Unlock()

	slog.Info("connecting to OVN databases", "hostname", hostname)

	// On any error path, close partially-initialised clients so a retry
	// (agent.go retry loop) does not leak the previous connection by
	// overwriting o.sbClient / o.nbClient with a fresh one.
	success := false
	defer func() {
		if !success {
			o.closeClients()
		}
	}()

	reconnectBackoff := backoff.NewExponentialBackOff()

	// Connect to Southbound DB
	sbDBModel, err := sbDatabaseModel()
	if err != nil {
		return fmt.Errorf("create SB database model: %w", err)
	}

	sbOpts := []client.Option{
		client.WithReconnect(5*time.Second, reconnectBackoff),
		client.WithInactivityCheck(30*time.Second, 10*time.Second, reconnectBackoff),
	}
	for _, ep := range ovsdbEndpoints(o.cfg.OVNSBRemote) {
		sbOpts = append(sbOpts, client.WithEndpoint(ep))
	}
	o.sbClient, err = client.NewOVSDBClient(sbDBModel, sbOpts...)
	if err != nil {
		return fmt.Errorf("create SB client: %w", err)
	}

	if err := o.sbClient.Connect(ctx); err != nil {
		return fmt.Errorf("connect SB: %w", err)
	}
	slog.Info("connected to OVN SB", "remote", o.cfg.OVNSBRemote)
	setOVNConnectionState("sb", true)

	// Monitor SB tables
	o.sbClient.Cache().AddEventHandler(&sbEventHandler{ovn: o})

	sbMon := o.sbClient.NewMonitor(
		client.WithTable(&SBPortBinding{}),
		client.WithTable(&SBChassis{}),
	)
	if _, err := o.sbClient.Monitor(ctx, sbMon); err != nil {
		return fmt.Errorf("monitor SB: %w", err)
	}

	// Connect to Northbound DB
	nbDBModel, err := nbDatabaseModel()
	if err != nil {
		return fmt.Errorf("create NB database model: %w", err)
	}

	nbReconnectBackoff := backoff.NewExponentialBackOff()

	nbOpts := []client.Option{
		client.WithReconnect(5*time.Second, nbReconnectBackoff),
		client.WithInactivityCheck(30*time.Second, 10*time.Second, nbReconnectBackoff),
	}
	for _, ep := range ovsdbEndpoints(o.cfg.OVNNBRemote) {
		nbOpts = append(nbOpts, client.WithEndpoint(ep))
	}
	o.nbClient, err = client.NewOVSDBClient(nbDBModel, nbOpts...)
	if err != nil {
		return fmt.Errorf("create NB client: %w", err)
	}

	if err := o.nbClient.Connect(ctx); err != nil {
		return fmt.Errorf("connect NB: %w", err)
	}
	slog.Info("connected to OVN NB", "remote", o.cfg.OVNNBRemote)
	setOVNConnectionState("nb", true)

	// Monitor NB tables
	o.nbClient.Cache().AddEventHandler(&nbEventHandler{ovn: o})

	nbMon := o.nbClient.NewMonitor(
		client.WithTable(&NBNAT{}),
		client.WithTable(&NBLogicalRouter{}),
		client.WithTable(&NBLogicalRouterPort{}),
		client.WithTable(&NBLogicalRouterStaticRoute{}),
		client.WithTable(&NBStaticMACBinding{}),
		client.WithTable(&NBGatewayChassis{}),
	)
	if _, err := o.nbClient.Monitor(ctx, nbMon); err != nil {
		return fmt.Errorf("monitor NB: %w", err)
	}

	// Mark as ready so event handlers can now signal refreshes.
	o.ready.Store(true)

	// Initial state refresh.
	o.refreshState(ctx)

	// Drain any signals queued by initial monitor events — refreshState
	// already captured the current state, so they would only cause
	// redundant work.
	o.drainSignals()

	// Start the refresh loop. It captures ctx via closure and exits when
	// ctx is cancelled. Close() waits for loopDone before tearing down
	// the OVSDB clients.
	o.loopDone = make(chan struct{})
	go func() {
		defer close(o.loopDone)
		o.refreshLoop(ctx)
	}()

	// Mirror each client's live connection status into the
	// ovn_connection_state gauge. Capture the client values now (not
	// o.sbClient/o.nbClient) so a concurrent closeClients() nil-ing the
	// fields cannot race the poller. The watcher shares refreshLoop's ctx,
	// which Run cancels before Close() runs, so it never outlives the client.
	go watchConnectionState(ctx, o.sbClient, o.nbClient, connStatePollInterval)

	success = true
	return nil
}

// connStatePollInterval is how often watchConnectionState samples each client's
// connection status. One second is fine-grained enough to catch a transient
// outage while adding negligible load.
const connStatePollInterval = time.Second

// watchConnectionState mirrors the live connection status of the SB and NB
// clients into the ovn_connection_state gauge until ctx is cancelled.
//
// The gauge is otherwise only cleared by closeClients(), so a transient OVSDB
// outage would never show in the metric: libovsdb's reconnect path
// re-establishes the monitors and repopulates the cache internally and only
// signals DisconnectNotify on a terminal (non-reconnect) close, so there is no
// event to hook for a drop-and-recover cycle. Polling Connected() is the only
// reliable mirror of the outage, which is what lets an operator alert on a lost
// OVN connection and what the reconnect integration scenario asserts on.
func watchConnectionState(ctx context.Context, sb, nb ovsdbClient, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			setOVNConnectionState("sb", sb.Connected())
			setOVNConnectionState("nb", nb.Connected())
		}
	}
}

func (o *OVNClient) Close() {
	// Stop new event handlers from signalling so closeClients can run
	// without further channel sends from cache callbacks.
	o.ready.Store(false)
	if o.loopDone != nil {
		<-o.loopDone
	}
	o.closeClients()
}

// closeClients closes both OVSDB clients (if set) and clears the references
// so a subsequent Connect() retry starts from a clean slate.
func (o *OVNClient) closeClients() {
	if o.sbClient != nil {
		o.sbClient.Close()
		o.sbClient = nil
		setOVNConnectionState("sb", false)
	}
	if o.nbClient != nil {
		o.nbClient.Close()
		o.nbClient = nil
		setOVNConnectionState("nb", false)
	}
}

// drainSignals empties both signal channels.
func (o *OVNClient) drainSignals() {
	select {
	case <-o.debounceCh:
	default:
	}
	select {
	case <-o.immediateCh:
	default:
	}
}

// GetState returns a snapshot of the current OVN state.
func (o *OVNClient) GetState() OVNState {
	o.state.mu.RLock()
	defer o.state.mu.RUnlock()
	localRouters := make([]LocalRouterInfo, len(o.state.LocalRouters))
	copy(localRouters, o.state.LocalRouters)
	discoveredNets := make([]*net.IPNet, len(o.state.DiscoveredNetworks))
	copy(discoveredNets, o.state.DiscoveredNetworks)
	allChassis := make(map[string]bool, len(o.state.AllChassisNames))
	for k, v := range o.state.AllChassisNames {
		allChassis[k] = v
	}
	natIPToMAC := make(map[string]string, len(o.state.NATIPToRouterMAC))
	for k, v := range o.state.NATIPToRouterMAC {
		natIPToMAC[k] = v
	}
	natIPToSegment := make(map[string]string, len(o.state.NATIPToSegment))
	for k, v := range o.state.NATIPToSegment {
		natIPToSegment[k] = v
	}
	return OVNState{
		LocalChassisName:   o.state.LocalChassisName,
		LocalRouters:       localRouters,
		HasLocalRouters:    o.state.HasLocalRouters,
		FIPs:               append([]string{}, o.state.FIPs...),
		SNATIPs:            append([]string{}, o.state.SNATIPs...),
		NATIPToRouterMAC:   natIPToMAC,
		NATIPToSegment:     natIPToSegment,
		DiscoveredNetworks: discoveredNets,
		AllChassisNames:    allChassis,
	}
}

// refreshState performs a unified state refresh from both OVN databases.
// It determines which routers are locally active and collects their NAT entries.
//
// Mapping chain:
//
//	SB chassisredirect (logical_port="cr-lrp-X", chassis=local?)
//	  → NB Logical_Router_Port (name="lrp-X")
//	  → NB Logical_Router (ports ⊇ {LRP UUID}, nat = {NAT UUIDs})
//	  → NB NAT (external_ip)
func (o *OVNClient) refreshState(ctx context.Context) {
	// Step 1: Find all chassisredirect port bindings and resolve chassis hostnames.
	// Every table is read through cachedList, which falls back to a direct
	// server select when the monitor cache is missing rows (see ovn_cache.go).
	portBindings, err := cachedList(ctx, o.sbClient, "Port_Binding",
		pbCheckColumns, keyOfSBPortBinding, decodeSBPortBinding)
	if err != nil {
		slog.Error("failed to list port bindings", "error", err)
		return
	}

	chassis, err := cachedList(ctx, o.sbClient, "Chassis",
		chCheckColumns, keyOfSBChassis, decodeSBChassis)
	if err != nil {
		slog.Error("failed to list chassis", "error", err)
		return
	}

	chassisHostname := make(map[string]string, len(chassis))
	allChassisNames := make(map[string]bool, len(chassis))
	for _, ch := range chassis {
		chassisHostname[ch.UUID] = ch.Hostname
		chassisHostname[ch.Name] = ch.Hostname
		allChassisNames[ch.Hostname] = true
	}

	localChassisName := o.state.LocalChassisName

	// Collect LRP names from chassisredirect ports active on this chassis.
	// A chassisredirect logical_port has the form "cr-lrp-<LRP_NAME>".
	// localLRPNames maps: LRP name → CR port logical_port name.
	localLRPNames := make(map[string]string)
	for _, pb := range portBindings {
		if pb.Type != "chassisredirect" || pb.Chassis == nil || *pb.Chassis == "" {
			continue
		}
		// If gateway_port is configured, restrict to that single port.
		if o.cfg.GatewayPort != "" && pb.LogicalPort != o.cfg.GatewayPort {
			continue
		}
		hostname := chassisHostname[*pb.Chassis]
		if hostname == localChassisName {
			lrpName := strings.TrimPrefix(pb.LogicalPort, "cr-")
			localLRPNames[lrpName] = pb.LogicalPort
		}
	}

	// Step 1b: Resolve each local LRP's localnet segment. A gateway LRP's
	// peer logical switch (the external network) carries a localnet port
	// whose SB Port_Binding holds the physnet name (options:network_name)
	// and the optional VLAN tag. Two joins over the Port_Binding table:
	// localnet rows indexed by datapath, then the switch-side patch rows
	// (options:peer == LRP name) sharing that datapath. Deliberately no
	// neutron:device_owner filter: the join must also work for non-Neutron
	// topologies (e.g. the E2E lab) that set no external_ids.
	localnetByDatapath := make(map[string]SBPortBinding)
	for _, pb := range portBindings {
		if pb.Type == "localnet" {
			localnetByDatapath[pb.Datapath] = pb
		}
	}
	segmentByLRP := make(map[string]*LocalnetSegment)
	for _, pb := range portBindings {
		if pb.Type != "patch" {
			continue
		}
		peer := pb.Options["peer"]
		if peer == "" {
			continue
		}
		if _, isLocal := localLRPNames[peer]; !isLocal {
			continue
		}
		ln, ok := localnetByDatapath[pb.Datapath]
		if !ok {
			continue
		}
		seg := &LocalnetSegment{
			LocalnetPort: ln.LogicalPort,
			NetworkName:  ln.Options["network_name"],
		}
		if ln.Tag != nil {
			tag := *ln.Tag
			seg.VLANTag = &tag
		}
		segmentByLRP[peer] = seg
	}

	// Step 2: Build NB Logical_Router_Port name → UUID map.
	lrps, err := cachedList(ctx, o.nbClient, "Logical_Router_Port",
		lrpCheckColumns, keyOfNBLogicalRouterPort, decodeNBLogicalRouterPort)
	if err != nil {
		slog.Error("failed to list logical router ports", "error", err)
		return
	}
	lrpNameToUUID := make(map[string]string, len(lrps))
	lrpByUUID := make(map[string]NBLogicalRouterPort, len(lrps))
	lrpNameToMAC := make(map[string]string, len(lrps))
	for _, lrp := range lrps {
		lrpNameToUUID[lrp.Name] = lrp.UUID
		lrpByUUID[lrp.UUID] = lrp
		lrpNameToMAC[lrp.Name] = lrp.MAC
	}

	// Resolve local LRP names → UUIDs.
	localLRPUUIDs := make(map[string]bool)
	for lrpName := range localLRPNames {
		if uuid, ok := lrpNameToUUID[lrpName]; ok {
			localLRPUUIDs[uuid] = true
		}
	}

	// Step 3: Find routers that own a locally-active LRP. Collect their NAT UUIDs.
	routers, err := cachedList(ctx, o.nbClient, "Logical_Router",
		lrCheckColumns, keyOfNBLogicalRouter, decodeNBLogicalRouter)
	if err != nil {
		slog.Error("failed to list logical routers", "error", err)
		return
	}

	// natUUIDToRouterMAC maps each NAT UUID to the MAC of the router port
	// that owns it, so hairpin flows can set the correct dl_dst.
	// natUUIDToSegment records the owning router's localnet segment
	// alongside, so per-IP state lands on the right patch port/interface.
	natUUIDToRouterMAC := make(map[string]string)
	natUUIDToSegment := make(map[string]string)
	var localRouters []LocalRouterInfo
	for _, router := range routers {
		var matchedLRP *NBLogicalRouterPort
		for _, portUUID := range router.Ports {
			if localLRPUUIDs[portUUID] {
				lrp := lrpByUUID[portUUID]
				matchedLRP = &lrp
				break
			}
		}
		if matchedLRP == nil {
			continue
		}
		segment := segmentByLRP[matchedLRP.Name]
		localRouters = append(localRouters, LocalRouterInfo{
			RouterName:  router.Name,
			RouterUUID:  router.UUID,
			LRPName:     matchedLRP.Name,
			LRPUUID:     matchedLRP.UUID,
			LRPMAC:      matchedLRP.MAC,
			LRPNetworks: matchedLRP.Networks,
			CRPort:      localLRPNames[matchedLRP.Name],
			Segment:     segment,
		})
		for _, natUUID := range router.Nat {
			if matchedLRP.MAC != "" {
				natUUIDToRouterMAC[natUUID] = matchedLRP.MAC
			}
			if segment != nil {
				natUUIDToSegment[natUUID] = segment.LocalnetPort
			}
		}
	}

	// Step 4: Extract network CIDRs from locally-active LRPs.
	var discoveredNets []*net.IPNet
	for _, lr := range localRouters {
		for _, ns := range lr.LRPNetworks {
			_, cidr, err := net.ParseCIDR(ns)
			if err != nil {
				slog.Warn("failed to parse LRP network", "network", ns, "router", lr.RouterName, "error", err)
				continue
			}
			discoveredNets = append(discoveredNets, cidr)
		}
	}
	discoveredNets = uniqueNetworks(discoveredNets)

	// Determine effective network filters: manual config takes precedence over auto-discovery.
	effectiveFilters := effectiveNetworkFilters(o.cfg.NetworkFilters, discoveredNets)

	// Step 5: Filter NAT entries to only those belonging to locally-active routers.
	nats, err := cachedList(ctx, o.nbClient, "NAT",
		natCheckColumns, keyOfNBNAT, decodeNBNAT)
	if err != nil {
		slog.Error("failed to list NAT entries", "error", err)
		return
	}

	var fips, snatIPs []string
	natIPToRouterMAC := make(map[string]string)
	natIPToSegment := make(map[string]string)
	for _, nat := range nats {
		routerMAC, ok := natUUIDToRouterMAC[nat.UUID]
		if !ok {
			continue
		}
		ip := nat.ExternalIP
		// Validate every external IP unconditionally: a malformed row must
		// die here at ingest, regardless of whether a filter is in effect, so
		// it can never reach an nft/FRR/kernel command payload downstream.
		parsedIP := net.ParseIP(ip)
		if parsedIP == nil {
			slog.Warn("skipping NAT entry with invalid external IP",
				"external_ip", ip, "nat", nat.UUID, "type", nat.Type)
			continue
		}
		if len(effectiveFilters) > 0 && !containedInAny(parsedIP, effectiveFilters) {
			continue
		}
		natIPToRouterMAC[ip] = routerMAC
		if seg, ok := natUUIDToSegment[nat.UUID]; ok {
			natIPToSegment[ip] = seg
		}
		switch nat.Type {
		case "dnat_and_snat":
			fips = append(fips, ip)
		case "snat":
			// SNATIPs feeds the IPv4-only nft masquerade set (buildNftRuleset's
			// "table ip"), so keep IPv6 out until full v6 support (#85/#70)
			// lands. The dual-stack natIPToRouterMAC above still carries the
			// address for the family-aware OVS hairpin plane (#54).
			if parsedIP.To4() != nil {
				snatIPs = append(snatIPs, ip)
			} else {
				slog.Debug("excluding non-IPv4 SNAT external IP from the nft masquerade set, IPv6 support tracked in #85/#70",
					"external_ip", ip)
			}
		}
	}

	// Step 5b: Extract SNAT IPs from SB gateway port NatAddresses.
	// When a router has an external gateway but no connected internal subnets,
	// Neutron has not yet created the NB NAT entry. However, the SB
	// Port_Binding for the gateway patch port already contains the NAT IP
	// in its NatAddresses field. Extract these so routes are announced
	// as soon as the gateway is set.
	for _, pb := range portBindings {
		if pb.Type != "patch" {
			continue
		}
		peer := pb.Options["peer"]
		if peer == "" {
			continue
		}
		if _, isLocal := localLRPNames[peer]; !isLocal {
			continue
		}
		if pb.ExternalIDs["neutron:device_owner"] != "network:router_gateway" {
			continue
		}
		routerMAC := lrpNameToMAC[peer]
		for _, natAddr := range pb.NatAddresses {
			for _, ip := range parseNatAddressIPs(natAddr) {
				// parseNatAddressIPs only yields net.ParseIP-valid addresses;
				// re-parse here so the filter and family gate apply to every
				// candidate regardless of whether a filter is in effect.
				parsedIP := net.ParseIP(ip)
				if len(effectiveFilters) > 0 && !containedInAny(parsedIP, effectiveFilters) {
					continue
				}
				// Keep IPv6 out of the IPv4-only nft masquerade set (#85/#70)
				// while still tracking it for the OVS hairpin plane (#54).
				if parsedIP.To4() != nil {
					snatIPs = append(snatIPs, ip)
				} else {
					slog.Debug("excluding non-IPv4 SB SNAT address from the nft masquerade set, IPv6 support tracked in #85/#70",
						"external_ip", ip)
				}
				if routerMAC != "" {
					natIPToRouterMAC[ip] = routerMAC
				}
				if seg := segmentByLRP[peer]; seg != nil {
					natIPToSegment[ip] = seg.LocalnetPort
				}
			}
		}
	}

	// Step 6: Update state atomically.
	o.state.mu.Lock()
	o.state.LocalRouters = localRouters
	o.state.HasLocalRouters = len(localRouters) > 0
	o.state.FIPs = fips
	o.state.SNATIPs = snatIPs
	o.state.NATIPToRouterMAC = natIPToRouterMAC
	o.state.NATIPToSegment = natIPToSegment
	o.state.DiscoveredNetworks = discoveredNets
	o.state.AllChassisNames = allChassisNames
	o.state.mu.Unlock()

	slog.Info("state updated",
		"local_routers", len(localRouters),
		"fips", len(fips),
		"snat_ips", len(snatIPs),
		"discovered_networks", len(discoveredNets),
	)
	for _, lr := range localRouters {
		slog.Debug("locally active router",
			"router", lr.RouterName,
			"lrp", lr.LRPName,
			"cr_port", lr.CRPort,
			"segment", segmentName(lr.Segment),
		)
	}
}

// refreshLoop is the OVN client's event loop. It receives signals from cache
// event handlers and runs state refreshes with appropriate debouncing.
// Started by Connect() and runs until ctx is cancelled.
//
//   - debounceCh signals coalesce into one refresh per eventDebounceInterval,
//     followed by an onChange callback debounced by reconcileDebounceInterval.
//   - immediateCh signals bypass debouncing and trigger an immediate refresh +
//     onChange. A pending debounce timer is cancelled because the immediate
//     refresh supersedes it.
//
// Signal channels have capacity 1, and the loop processes signals
// sequentially, so a storm of events produces at most one in-flight pass
// plus one queued follow-up.
func (o *OVNClient) refreshLoop(ctx context.Context) {
	var (
		debounceTimer  *time.Timer
		debounceFire   <-chan time.Time
		reconcileTimer *time.Timer
		reconcileFire  <-chan time.Time
	)

	for {
		select {
		case <-ctx.Done():
			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			if reconcileTimer != nil {
				reconcileTimer.Stop()
			}
			return

		case <-o.debounceCh:
			if debounceTimer == nil {
				debounceTimer = time.NewTimer(eventDebounceInterval)
				debounceFire = debounceTimer.C
			}

		case <-debounceFire:
			debounceTimer = nil
			debounceFire = nil
			o.refreshState(ctx)
			if reconcileTimer == nil && o.onChange != nil {
				reconcileTimer = time.NewTimer(reconcileDebounceInterval)
				reconcileFire = reconcileTimer.C
			}

		case <-reconcileFire:
			reconcileTimer = nil
			reconcileFire = nil
			if o.onChange != nil {
				o.onChange()
			}

		case <-o.immediateCh:
			// Immediate supersedes any pending debounce: refresh now and
			// invoke onChange directly to bypass reconcile debouncing.
			if debounceTimer != nil {
				debounceTimer.Stop()
				debounceTimer = nil
				debounceFire = nil
			}
			o.refreshState(ctx)
			if o.onChange != nil {
				o.onChange()
			}
		}
	}
}

// debounceStateRefresh signals the refresh loop to schedule a debounced state
// refresh. Concurrent calls coalesce: at most one signal is queued.
func (o *OVNClient) debounceStateRefresh() {
	if !o.ready.Load() {
		return // not fully connected yet
	}
	select {
	case o.debounceCh <- struct{}{}:
	default:
		// already queued
	}
}

// immediateStateRefresh signals the refresh loop to run a state refresh
// immediately, bypassing debouncing. Used for chassisredirect changes to
// minimise HA failover reaction time. Concurrent calls coalesce: at most
// one signal is queued, so a burst of events results in at most one
// in-flight pass plus one queued follow-up.
func (o *OVNClient) immediateStateRefresh() {
	if !o.ready.Load() {
		return // not fully connected yet
	}
	select {
	case o.immediateCh <- struct{}{}:
	default:
		// already queued
	}
}

// signalDrainWatch wakes a DrainGateways poll waiting for chassisredirect
// ports to migrate away. It is fired from the SB event handler on every
// chassisredirect Port_Binding change so the drain re-checks countLocalCRPorts
// the moment OVN rebinds a port, instead of waiting for the fixed safety
// re-poll. Concurrent calls coalesce: at most one wake-up is queued. Outside a
// drain nothing reads the channel, so the queued signal is simply harmless.
func (o *OVNClient) signalDrainWatch() {
	if !o.ready.Load() {
		return // not fully connected yet
	}
	select {
	case o.drainWatchCh <- struct{}{}:
	default:
		// already queued
	}
}

// =============================================================================
// SB event handler (implements cache.EventHandler)
// =============================================================================

type sbEventHandler struct {
	ovn *OVNClient
}

func (h *sbEventHandler) OnAdd(table string, m model.Model) {
	if h.isChassisRedirect(table, m) {
		slog.Debug("chassisredirect port added, immediate refresh")
		h.ovn.signalDrainWatch()
		h.ovn.immediateStateRefresh()
		return
	}
	h.handleChange(table)
}

func (h *sbEventHandler) OnUpdate(table string, old, updated model.Model) {
	if h.isChassisRedirect(table, updated) {
		slog.Debug("chassisredirect port updated, immediate refresh")
		h.ovn.signalDrainWatch()
		h.ovn.immediateStateRefresh()
		return
	}
	h.handleChange(table)
}

func (h *sbEventHandler) OnDelete(table string, m model.Model) {
	if h.isChassisRedirect(table, m) {
		slog.Debug("chassisredirect port deleted, immediate refresh")
		h.ovn.signalDrainWatch()
		h.ovn.immediateStateRefresh()
		return
	}
	h.handleChange(table)
}

func (h *sbEventHandler) isChassisRedirect(table string, m model.Model) bool {
	if table != "Port_Binding" {
		return false
	}
	pb, ok := m.(*SBPortBinding)
	return ok && pb.Type == "chassisredirect"
}

func (h *sbEventHandler) handleChange(table string) {
	slog.Debug("SB change detected", "table", table)
	if table == "Port_Binding" || table == "Chassis" {
		h.ovn.debounceStateRefresh()
	}
}

// =============================================================================
// NB event handler (implements cache.EventHandler)
// =============================================================================

type nbEventHandler struct {
	ovn *OVNClient
}

func (h *nbEventHandler) OnAdd(table string, m model.Model) {
	h.handleChange(table)
}

func (h *nbEventHandler) OnUpdate(table string, old, updated model.Model) {
	h.handleChange(table)
}

func (h *nbEventHandler) OnDelete(table string, m model.Model) {
	h.handleChange(table)
}

func (h *nbEventHandler) handleChange(table string) {
	slog.Debug("NB change detected", "table", table)
	switch table {
	case "NAT", "Logical_Router", "Logical_Router_Port":
		h.ovn.debounceStateRefresh()
	case "Gateway_Chassis":
		// refreshState does not read Gateway_Chassis, but a priority change
		// must still wake the agent: the reconcile that follows the refresh
		// runs EnsureActivePriorityLead, which is how the active chassis
		// reacts to a peer draining (priority→0), restoring (→1) or
		// boosting. Without this trigger a peer's drain/restore is invisible
		// until the 60s periodic reconcile — long enough for a priority tie
		// to leave ovn-northd flapping the chassisredirect port and
		// stranding the FIPs. Rapid stop/start cycles are bursts of
		// Gateway_Chassis writes, which is why the hang is worst under quick
		// succession.
		h.ovn.debounceStateRefresh()
	}
}

// =============================================================================
// Helpers
// =============================================================================

func sbDatabaseModel() (model.ClientDBModel, error) {
	return model.NewClientDBModel("OVN_Southbound", map[string]model.Model{
		"Port_Binding": &SBPortBinding{},
		"Chassis":      &SBChassis{},
	})
}

func nbDatabaseModel() (model.ClientDBModel, error) {
	return model.NewClientDBModel("OVN_Northbound", map[string]model.Model{
		"NAT":                         &NBNAT{},
		"Logical_Router":              &NBLogicalRouter{},
		"Logical_Router_Port":         &NBLogicalRouterPort{},
		"Logical_Router_Static_Route": &NBLogicalRouterStaticRoute{},
		"Static_MAC_Binding":          &NBStaticMACBinding{},
		"Gateway_Chassis":             &NBGatewayChassis{},
	})
}

func ovsdbEndpoints(remote string) []string {
	parts := strings.Split(remote, ",")
	var endpoints []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			endpoints = append(endpoints, p)
		}
	}
	return endpoints
}

// uniqueNetworks deduplicates and sorts a list of *net.IPNet by string representation.
func uniqueNetworks(nets []*net.IPNet) []*net.IPNet {
	seen := make(map[string]bool, len(nets))
	var result []*net.IPNet
	for _, n := range nets {
		key := n.String()
		if !seen[key] {
			seen[key] = true
			result = append(result, n)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].String() < result[j].String()
	})
	return result
}

// parseNatAddressIPs extracts IP addresses from an OVN NatAddresses entry.
// Format: "MAC IP1 [IP2 ...] [is_chassis_resident("port")]"
func parseNatAddressIPs(natAddr string) []string {
	fields := strings.Fields(natAddr)
	if len(fields) < 2 {
		return nil
	}
	var ips []string
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "is_chassis_resident") {
			break
		}
		if net.ParseIP(f) != nil {
			ips = append(ips, f)
		}
	}
	return ips
}

func getHostname() (string, error) {
	h, err := os.Hostname()
	if err != nil {
		return "", err
	}
	if idx := strings.IndexByte(h, '.'); idx != -1 {
		h = h[:idx]
	}
	return h, nil
}

package azure

import (
	"cmp"
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const syncRetryDelay = 5 * time.Second

// A machine's firewall admits connections from peers only, so the
// simulation turns network security groups into the peers, protocols and
// ports they allow. It evaluates each group's rules in priority order over
// regions of (peer address, port) per protocol: a rule decides the part of
// its region that no rule before it decided. A virtual machine gets what
// both its subnet's group and its network interface's group allow.

// span is an inclusive range of IPv4 addresses or ports.
type span struct{ lo, hi uint32 }

// box is a set of peers and ports.
type box struct{ addr, port span }

// region is the boxes of each protocol: tcp, udp and icmp.
type region map[string][]box

var (
	protocols = []string{"tcp", "udp", "icmp"}
	allAddrs  = span{0, 1<<32 - 1}
	allPorts  = span{0, 65535}
	// azureLoadBalancer is the address the AzureLoadBalancer tag stands for.
	azureLoadBalancer = netip.MustParseAddr("168.63.129.16")
)

func spanOf(p netip.Prefix) span {
	p = p.Masked()
	b := p.Addr().As4()
	lo := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return span{lo, lo | (1<<(32-p.Bits()) - 1)}
}

func (a span) intersect(b span) (span, bool) {
	lo, hi := max(a.lo, b.lo), min(a.hi, b.hi)
	return span{lo, hi}, lo <= hi
}

// minus returns the parts of a outside b.
func (a span) minus(b span) []span {
	c, ok := a.intersect(b)
	if !ok {
		return []span{a}
	}
	var out []span
	if a.lo < c.lo {
		out = append(out, span{a.lo, c.lo - 1})
	}
	if c.hi < a.hi {
		out = append(out, span{c.hi + 1, a.hi})
	}
	return out
}

func (a box) minus(b box) []box {
	addr, ok1 := a.addr.intersect(b.addr)
	_, ok2 := a.port.intersect(b.port)
	if !ok1 || !ok2 {
		return []box{a}
	}
	var out []box
	for _, s := range a.addr.minus(b.addr) {
		out = append(out, box{s, a.port})
	}
	for _, s := range a.port.minus(b.port) {
		out = append(out, box{addr, s})
	}
	return out
}

func (r region) minus(o region) region {
	out := region{}
	for proto, boxes := range r {
		rest := slices.Clone(boxes)
		for _, cut := range o[proto] {
			var next []box
			for _, b := range rest {
				next = append(next, b.minus(cut)...)
			}
			rest = next
		}
		if len(rest) > 0 {
			out[proto] = rest
		}
	}
	return out
}

func (r region) add(o region) region {
	out := region{}
	for _, proto := range protocols {
		if boxes := append(slices.Clone(r[proto]), o[proto]...); len(boxes) > 0 {
			out[proto] = boxes
		}
	}
	return out
}

func (r region) intersect(o region) region {
	out := region{}
	for proto, boxes := range r {
		for _, a := range boxes {
			for _, b := range o[proto] {
				addr, ok1 := a.addr.intersect(b.addr)
				port, ok2 := a.port.intersect(b.port)
				if ok1 && ok2 {
					out[proto] = append(out[proto], box{addr, port})
				}
			}
		}
	}
	return out
}

// everything is a region of every protocol, peer and port within addrs.
func everything(addrs []span) region {
	out := region{}
	for _, proto := range protocols {
		for _, a := range addrs {
			out[proto] = append(out[proto], box{a, portsOf(proto, allPorts)})
		}
	}
	return out
}

// portsOf is the port span of a rule's protocol: ICMP has no ports.
func portsOf(proto string, ports span) span {
	if proto == "icmp" {
		return span{0, 0}
	}
	return ports
}

// netScope is what the address tags stand for on the simulated network.
type netScope struct {
	// vnet is the private network, which VirtualNetwork stands for.
	vnet []span
}

func (n netScope) spans(value string) []span {
	switch strings.ToLower(value) {
	case "*", "any":
		return []span{allAddrs}
	case "virtualnetwork":
		return n.vnet
	case "internet":
		rest := []span{allAddrs}
		for _, v := range n.vnet {
			var next []span
			for _, r := range rest {
				next = append(next, r.minus(v)...)
			}
			rest = next
		}
		return rest
	case "azureloadbalancer":
		return []span{spanOf(netip.PrefixFrom(azureLoadBalancer, 32))}
	}
	if p, err := netip.ParsePrefix(value); err == nil {
		return []span{spanOf(p)}
	}
	if a, err := netip.ParseAddr(value); err == nil {
		return []span{spanOf(netip.PrefixFrom(a, 32))}
	}
	return nil
}

func (n netScope) covers(values []string, addr netip.Addr) bool {
	self := spanOf(netip.PrefixFrom(addr, 32))
	for _, v := range values {
		for _, s := range n.spans(v) {
			if _, ok := s.intersect(self); ok {
				return true
			}
		}
	}
	return false
}

func ruleRegion(p ruleProperties, peers []string, n netScope) region {
	var spans []span
	for _, v := range peers {
		spans = append(spans, n.spans(v)...)
	}
	protos := protocols
	if p.Protocol != "*" {
		protos = []string{strings.ToLower(p.Protocol)}
	}
	out := region{}
	for _, proto := range protos {
		for _, v := range p.destinationPorts() {
			lo, hi, _ := parsePortRange(v)
			for _, a := range spans {
				out[proto] = append(out[proto], box{a, portsOf(proto, span{uint32(lo), uint32(hi)})})
			}
		}
	}
	return out
}

// evaluate returns what a network security group allows a machine at self
// in one direction: inbound from peers, or outbound to them.
func evaluate(rules []ruleJSON, direction string, self netip.Addr, n netScope) region {
	all := append(slices.Clone(rules), defaultRules...)
	slices.SortStableFunc(all, func(a, b ruleJSON) int { return a.Properties.Priority - b.Properties.Priority })
	allowed, decided := region{}, region{}
	for _, rule := range all {
		p := rule.Properties
		if p.Direction != direction {
			continue
		}
		mine, peers := p.destinations(), p.sources()
		if direction == "Outbound" {
			mine, peers = p.sources(), p.destinations()
		}
		if !n.covers(mine, self) {
			continue
		}
		r := ruleRegion(p, peers, n)
		if p.Access == "Allow" {
			allowed = allowed.add(r.minus(decided))
		}
		decided = decided.add(r)
	}
	return allowed
}

// policyFor is the firewall of the virtual machine's machine. Without a
// public IP, or with a public IP but no network security group, which
// Standard public IPs require, only the virtual network reaches it.
func (s *Simulation) policyFor(sub *subscription, v *vm) *machines.Firewall {
	refs := v.Properties.NetworkProfile.NetworkInterfaces
	if len(refs) == 0 {
		return &machines.Firewall{Inbound: []machines.Rule{}, Outbound: []machines.Rule{}}
	}
	n := s.nicByID(sub, refs[0].ID)
	if n == nil || len(n.IPConfigurations) == 0 {
		return &machines.Firewall{Inbound: []machines.Rule{}, Outbound: []machines.Rule{}}
	}
	cfg := n.IPConfigurations[0].Properties
	self, _ := netip.ParseAddr(cfg.PrivateIPAddress)
	scope := netScope{vnet: []span{spanOf(s.networks.Private.Prefix)}}
	in, out := everything([]span{allAddrs}), everything([]span{allAddrs})
	var groups []*nsg
	_, sn := s.subnetByID(sub, idOf(cfg.Subnet))
	if sn != nil {
		if g := s.nsgByID(sub, idOf(sn.Properties.NetworkSecurityGroup)); g != nil {
			groups = append(groups, g)
		}
	}
	if g := s.nsgByID(sub, idOf(n.Properties.NetworkSecurityGroup)); g != nil {
		groups = append(groups, g)
	}
	for _, g := range groups {
		in = in.intersect(evaluate(g.Rules, "Inbound", self, scope))
		out = out.intersect(evaluate(g.Rules, "Outbound", self, scope))
	}
	hasPublicIP := s.publicIPByID(sub, idOf(cfg.PublicIPAddress)) != nil
	if !hasPublicIP || len(groups) == 0 {
		in = in.intersect(everything(scope.vnet))
	}
	if !hasPublicIP && sn != nil && !sn.Properties.outboundAccess() {
		out = out.intersect(everything(scope.vnet))
	}
	return &machines.Firewall{Inbound: machineRules(in), Outbound: machineRules(out)}
}

// machineRules turns a region into machine firewall rules, one for each
// protocol and port range, listing the peers as CIDR blocks.
func machineRules(r region) []machines.Rule {
	rules := []machines.Rule{}
	for _, proto := range protocols {
		byPorts := map[span][]span{}
		for _, b := range r[proto] {
			port := b.port
			if proto != "icmp" {
				if port.hi == 0 {
					continue
				}
				port.lo = max(port.lo, 1)
			}
			byPorts[port] = append(byPorts[port], b.addr)
		}
		ports := make([]span, 0, len(byPorts))
		for p := range byPorts {
			ports = append(ports, p)
		}
		slices.SortFunc(ports, compareSpans)
		for _, p := range ports {
			rule := machines.Rule{Protocol: proto, Peers: cidrs(byPorts[p])}
			if proto != "icmp" && (p.lo > 1 || p.hi < 65535) {
				rule.Ports = strconv.Itoa(int(p.lo))
				if p.hi != p.lo {
					rule.Ports += "-" + strconv.Itoa(int(p.hi))
				}
			}
			rules = append(rules, rule)
		}
	}
	return rules
}

func compareSpans(a, b span) int {
	if c := cmp.Compare(a.lo, b.lo); c != 0 {
		return c
	}
	return cmp.Compare(a.hi, b.hi)
}

// merge joins overlapping and adjacent spans, in order.
func merge(spans []span) []span {
	sorted := slices.Clone(spans)
	slices.SortFunc(sorted, compareSpans)
	var out []span
	for _, s := range sorted {
		if n := len(out); n > 0 && uint64(s.lo) <= uint64(out[n-1].hi)+1 {
			out[n-1].hi = max(out[n-1].hi, s.hi)
			continue
		}
		out = append(out, s)
	}
	return out
}

// cidrs returns the fewest CIDR blocks that cover the spans, in order.
func cidrs(spans []span) []string {
	var out []string
	for _, s := range merge(spans) {
		lo := uint64(s.lo)
		for lo <= uint64(s.hi) {
			bits := 32
			for bits > 0 {
				size := uint64(1) << (33 - bits)
				if lo%size != 0 || lo+size-1 > uint64(s.hi) {
					break
				}
				bits--
			}
			addr := netip.AddrFrom4([4]byte{byte(lo >> 24), byte(lo >> 16), byte(lo >> 8), byte(lo)})
			out = append(out, netip.PrefixFrom(addr, bits).String())
			lo += uint64(1) << (32 - bits)
		}
	}
	return out
}

func policyKey(policy *machines.Firewall) string {
	data, _ := json.Marshal(policy)
	return string(data)
}

// firewallSync tracks what each machine last received, its firewall and its
// public addresses, so the simulation pushes only changes.
type firewallSync struct {
	kicks     chan struct{}
	mu        sync.Mutex
	policies  map[string]string
	addresses map[string]string
}

func newFirewallSync() *firewallSync {
	return &firewallSync{kicks: make(chan struct{}, 1), policies: map[string]string{}, addresses: map[string]string{}}
}

func (f *firewallSync) kick() {
	select {
	case f.kicks <- struct{}{}:
	default:
	}
}

func (f *firewallSync) applied(machine string, policy *machines.Firewall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies[machine] = policyKey(policy)
}

func (f *firewallSync) addressesApplied(machine string, addrs []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addresses[machine] = strings.Join(addrs, ",")
}

func (f *firewallSync) forget(machine string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.policies, machine)
	delete(f.addresses, machine)
}

func (f *firewallSync) needs(machine string, policy *machines.Firewall, addrs []string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.policies[machine]
	a, okA := f.addresses[machine]
	return !ok || p != policyKey(policy), (!okA && len(addrs) > 0) || (okA && a != strings.Join(addrs, ","))
}

func (s *Simulation) syncFirewalls(ctx context.Context) {
	retry := time.NewTimer(syncRetryDelay)
	retry.Stop()
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.firewalls.kicks:
		case <-retry.C:
		}
		if !s.pushFirewalls(ctx) {
			retry.Reset(syncRetryDelay)
		}
	}
}

// pushFirewalls gives every running machine its current firewall and public
// addresses. It reports false when a push failed and should be retried.
func (s *Simulation) pushFirewalls(ctx context.Context) bool {
	type push struct {
		vm, machine string
		policy      *machines.Firewall
		addrs       []string
		firewall    bool
		addresses   bool
	}
	var pushes []push
	s.store.Read(func(sub *subscription) {
		for _, v := range sub.VMs {
			if v.Machine == "" || v.ProvisioningState != statusSucceeded || v.PowerState != "running" {
				continue
			}
			policy, addrs := s.policyFor(sub, v), nonNil(s.publicAddressesOf(sub, v))
			if fw, ad := s.firewalls.needs(v.Machine, policy, addrs); fw || ad {
				pushes = append(pushes, push{v.Name, v.Machine, policy, addrs, fw, ad})
			}
		}
	})
	ok := true
	for _, p := range pushes {
		if p.firewall {
			if err := s.machines.SetFirewall(ctx, p.machine, p.policy); err != nil {
				s.log.Warn("push a firewall to a virtual machine's machine", "vm", p.vm, "error", err)
				ok = false
			} else {
				s.firewalls.applied(p.machine, p.policy)
			}
		}
		if p.addresses {
			if err := s.machines.SetAddresses(ctx, p.machine, p.addrs); err != nil {
				s.log.Warn("push public addresses to a virtual machine's machine", "vm", p.vm, "error", err)
				ok = false
			} else {
				s.firewalls.addressesApplied(p.machine, p.addrs)
			}
		}
	}
	return ok
}

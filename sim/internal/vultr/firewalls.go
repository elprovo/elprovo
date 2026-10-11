package vultr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

const (
	firewallGroupNotFound = "Invalid firewall group ID."
	firewallRuleNotFound  = "Firewall rule ID not found."
	maxFirewallRules      = 50
	firewallRetryDelay    = 5 * time.Second
)

type firewallGroupJSON struct {
	ID            string `json:"id"`
	Description   string `json:"description"`
	DateCreated   string `json:"date_created"`
	DateModified  string `json:"date_modified"`
	InstanceCount int    `json:"instance_count"`
	RuleCount     int    `json:"rule_count"`
	MaxRuleCount  int    `json:"max_rule_count"`
}

type firewallRuleJSON struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	IPType     string `json:"ip_type"`
	Action     string `json:"action"`
	Protocol   string `json:"protocol"`
	Port       string `json:"port"`
	Subnet     string `json:"subnet"`
	SubnetSize int    `json:"subnet_size"`
	Source     string `json:"source"`
	Notes      string `json:"notes"`
}

func renderFirewallGroup(a *account, g *firewallGroup) firewallGroupJSON {
	out := firewallGroupJSON{
		ID: g.ID, Description: g.Description, DateCreated: timestamp(g.CreatedAt), DateModified: timestamp(g.ModifiedAt),
		RuleCount: len(g.Rules), MaxRuleCount: maxFirewallRules,
	}
	for _, i := range a.Instances {
		if i.FirewallGroupID == g.ID {
			out.InstanceCount++
		}
	}
	return out
}

func renderFirewallRule(r firewallRule) firewallRuleJSON {
	return firewallRuleJSON{
		ID: r.ID, Type: r.IPType, IPType: r.IPType, Action: "accept", Protocol: r.Protocol, Port: r.Port,
		Subnet: r.Subnet, SubnetSize: r.SubnetSize, Source: r.Source, Notes: r.Notes,
	}
}

func (s *Simulation) createFirewallGroup(r *http.Request) (int, any, error) {
	var req struct {
		Description string `json:"description"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out firewallGroupJSON
	err := s.store.Update(func(a *account) error {
		now := s.now()
		g := &firewallGroup{ID: newUUID(), Description: req.Description, CreatedAt: now, ModifiedAt: now}
		a.FirewallGroups = append(a.FirewallGroups, g)
		out = renderFirewallGroup(a, g)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"firewall_group": out}, nil
}

func (s *Simulation) listFirewallGroups(r *http.Request) (int, any, error) {
	var out []firewallGroupJSON
	s.store.Read(func(a *account) {
		for _, g := range a.FirewallGroups {
			out = append(out, renderFirewallGroup(a, g))
		}
	})
	return listResponse(r, "firewall_groups", out)
}

func (s *Simulation) getFirewallGroup(r *http.Request) (int, any, error) {
	var out *firewallGroupJSON
	s.store.Read(func(a *account) {
		if g := a.firewallGroup(r.PathValue("id")); g != nil {
			j := renderFirewallGroup(a, g)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound(firewallGroupNotFound)
	}
	return http.StatusOK, map[string]any{"firewall_group": out}, nil
}

func (s *Simulation) updateFirewallGroup(r *http.Request) (int, any, error) {
	var req struct {
		Description *string `json:"description"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Description == nil {
		return 0, nil, errBadRequest("description is required.")
	}
	err := s.store.Update(func(a *account) error {
		g := a.firewallGroup(r.PathValue("id"))
		if g == nil {
			return errNotFound(firewallGroupNotFound)
		}
		g.Description, g.ModifiedAt = *req.Description, s.now()
		return nil
	})
	return http.StatusNoContent, nil, err
}

// deleteFirewallGroup deletes the group and takes it off its instances,
// which then accept all traffic.
func (s *Simulation) deleteFirewallGroup(r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	err := s.store.Update(func(a *account) error {
		if a.firewallGroup(id) == nil {
			return errNotFound(firewallGroupNotFound)
		}
		a.FirewallGroups = slices.DeleteFunc(a.FirewallGroups, func(g *firewallGroup) bool { return g.ID == id })
		for _, i := range a.Instances {
			if i.FirewallGroupID == id {
				i.FirewallGroupID = ""
			}
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusNoContent, nil, nil
}

func (s *Simulation) listFirewallRules(r *http.Request) (int, any, error) {
	var out []firewallRuleJSON
	found := false
	s.store.Read(func(a *account) {
		g := a.firewallGroup(r.PathValue("id"))
		if g == nil {
			return
		}
		found = true
		for _, rule := range g.Rules {
			out = append(out, renderFirewallRule(rule))
		}
	})
	if !found {
		return 0, nil, errNotFound(firewallGroupNotFound)
	}
	return listResponse(r, "firewall_rules", out)
}

type firewallRuleRequest struct {
	IPType     string `json:"ip_type"`
	Protocol   string `json:"protocol"`
	Subnet     string `json:"subnet"`
	SubnetSize *int   `json:"subnet_size"`
	Port       string `json:"port"`
	Source     string `json:"source"`
	Notes      string `json:"notes"`
}

// rule validates the request and returns the rule it describes. The
// simulation enforces tcp, udp and icmp rules on IPv4; it stores IPv6 rules,
// which no simulated machine needs, and refuses what it can't enforce.
func (req firewallRuleRequest) rule() (firewallRule, error) {
	protocol := strings.ToLower(req.Protocol)
	switch {
	case req.IPType != "v4" && req.IPType != "v6":
		return firewallRule{}, errBadRequest("Invalid ip_type: use v4 or v6.")
	case protocol == "gre" || protocol == "esp" || protocol == "ah":
		return firewallRule{}, errBadRequest("Protocol %s is not supported by the simulation: it enforces tcp, udp and icmp.", protocol)
	case protocol != "tcp" && protocol != "udp" && protocol != "icmp":
		return firewallRule{}, errBadRequest("Invalid protocol: use icmp, tcp, udp, gre, esp or ah.")
	case req.Source != "":
		return firewallRule{}, errBadRequest("Source %q is not supported by the simulation: give subnet and subnet_size.", req.Source)
	case req.SubnetSize == nil:
		return firewallRule{}, errBadRequest("subnet_size is required.")
	}
	addr, err := netip.ParseAddr(req.Subnet)
	if err != nil || addr.Is4() != (req.IPType == "v4") || *req.SubnetSize < 0 || *req.SubnetSize > addr.BitLen() {
		return firewallRule{}, errBadRequest("Invalid subnet or subnet_size for ip_type %s.", req.IPType)
	}
	if protocol == "icmp" && req.Port != "" {
		return firewallRule{}, errBadRequest("Ports apply to tcp and udp rules only.")
	}
	if protocol != "icmp" && !validPorts(req.Port) {
		return firewallRule{}, errBadRequest("Invalid port %q: give a port or a range such as 8000:9000.", req.Port)
	}
	return firewallRule{
		IPType: req.IPType, Protocol: protocol, Port: req.Port, Subnet: addr.String(), SubnetSize: *req.SubnetSize, Notes: req.Notes,
	}, nil
}

// validPorts accepts a port or a colon-separated range of ports.
func validPorts(ports string) bool {
	low, high, isRange := strings.Cut(ports, ":")
	if !isRange {
		high = low
	}
	lo, err1 := strconv.Atoi(low)
	hi, err2 := strconv.Atoi(high)
	return err1 == nil && err2 == nil && lo >= 1 && hi <= 65535 && lo <= hi
}

func (s *Simulation) createFirewallRule(r *http.Request) (int, any, error) {
	var req firewallRuleRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	rule, err := req.rule()
	if err != nil {
		return 0, nil, err
	}
	var out firewallRuleJSON
	err = s.store.Update(func(a *account) error {
		g := a.firewallGroup(r.PathValue("id"))
		if g == nil {
			return errNotFound(firewallGroupNotFound)
		}
		if len(g.Rules) >= maxFirewallRules {
			return errBadRequest("A firewall group may have at most %d rules.", maxFirewallRules)
		}
		g.LastRuleID++
		rule.ID = g.LastRuleID
		g.Rules = append(g.Rules, rule)
		g.ModifiedAt = s.now()
		out = renderFirewallRule(rule)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusCreated, map[string]any{"firewall_rule": out}, nil
}

func (s *Simulation) getFirewallRule(r *http.Request) (int, any, error) {
	var out *firewallRuleJSON
	var groupFound bool
	s.store.Read(func(a *account) {
		g := a.firewallGroup(r.PathValue("id"))
		if g == nil {
			return
		}
		groupFound = true
		if i := ruleIndex(g, r.PathValue("num")); i >= 0 {
			j := renderFirewallRule(g.Rules[i])
			out = &j
		}
	})
	switch {
	case !groupFound:
		return 0, nil, errNotFound(firewallGroupNotFound)
	case out == nil:
		return 0, nil, errNotFound(firewallRuleNotFound)
	}
	return http.StatusOK, map[string]any{"firewall_rule": out}, nil
}

func (s *Simulation) deleteFirewallRule(r *http.Request) (int, any, error) {
	err := s.store.Update(func(a *account) error {
		g := a.firewallGroup(r.PathValue("id"))
		if g == nil {
			return errNotFound(firewallGroupNotFound)
		}
		i := ruleIndex(g, r.PathValue("num"))
		if i < 0 {
			return errNotFound(firewallRuleNotFound)
		}
		g.Rules = slices.Delete(g.Rules, i, i+1)
		g.ModifiedAt = s.now()
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusNoContent, nil, nil
}

func ruleIndex(g *firewallGroup, num string) int {
	id, err := strconv.Atoi(num)
	if err != nil {
		return -1
	}
	return slices.IndexFunc(g.Rules, func(r firewallRule) bool { return r.ID == id })
}

// policyFor returns what the instance's machine lets through. Without a
// firewall group, all traffic passes. A group drops every new inbound
// connection that no IPv4 rule allows and never filters outbound traffic, as
// Vultr's firewall does on the public interface. It does not filter a VPC,
// so on an instance in one, all traffic from the private Docker network,
// which plays the instance's VPCs, passes. An instance outside a VPC has no
// such network, so traffic from it is dropped too.
func policyFor(a *account, i *instance, private netip.Prefix) *machines.Firewall {
	g := a.firewallGroup(i.FirewallGroupID)
	if g == nil {
		return nil
	}
	policy := &machines.Firewall{Inbound: []machines.Rule{}, Outbound: []machines.Rule{}}
	for _, protocol := range []string{"tcp", "udp", "icmp"} {
		policy.Outbound = append(policy.Outbound, machines.Rule{Protocol: protocol, Peers: []string{"0.0.0.0/0"}})
		if len(i.VPCs) > 0 && private.IsValid() {
			policy.Inbound = append(policy.Inbound, machines.Rule{Protocol: protocol, Peers: []string{private.Masked().String()}})
		}
	}
	for _, r := range g.Rules {
		if r.IPType != "v4" {
			continue
		}
		rule := machines.Rule{Protocol: r.Protocol, Peers: []string{netip.PrefixFrom(netip.MustParseAddr(r.Subnet), r.SubnetSize).Masked().String()}}
		if r.Port != "1:65535" {
			rule.Ports = strings.Replace(r.Port, ":", "-", 1)
		}
		policy.Inbound = append(policy.Inbound, rule)
	}
	return policy
}

func policyKey(policy *machines.Firewall) string {
	data, _ := json.Marshal(policy)
	return string(data)
}

// firewallSync remembers the policy each instance's machine last received,
// so the sync loop pushes only changes.
type firewallSync struct {
	kicks  chan struct{}
	mu     sync.Mutex
	pushed map[string]string
}

func newFirewallSync() *firewallSync {
	return &firewallSync{kicks: make(chan struct{}, 1), pushed: map[string]string{}}
}

func (f *firewallSync) kick() {
	select {
	case f.kicks <- struct{}{}:
	default:
	}
}

func (f *firewallSync) applied(id string, policy *machines.Firewall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed[id] = policyKey(policy)
}

func (f *firewallSync) forget(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pushed, id)
}

func (f *firewallSync) has(id, key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.pushed[id]
	return ok && v == key
}

func (s *Simulation) syncFirewalls(ctx context.Context) {
	retry := time.NewTimer(firewallRetryDelay)
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
			retry.Reset(firewallRetryDelay)
		}
	}
}

func (s *Simulation) pushFirewalls(ctx context.Context) bool {
	type push struct {
		instanceID string
		machine    string
		policy     *machines.Firewall
	}
	var pushes []push
	s.store.Read(func(a *account) {
		for _, i := range a.Instances {
			if i.Machine == "" || i.Destroying || i.PowerStatus != "running" {
				continue
			}
			if p := policyFor(a, i, s.networks.Private.Prefix); !s.firewalls.has(i.ID, policyKey(p)) {
				pushes = append(pushes, push{i.ID, i.Machine, p})
			}
		}
	})
	ok := true
	for _, p := range pushes {
		if err := s.machines.SetFirewall(ctx, p.machine, p.policy); err != nil {
			s.log.Warn("push firewall to instance machine", "instance", p.instanceID, "error", err)
			ok = false
			continue
		}
		s.firewalls.applied(p.instanceID, p.policy)
	}
	return ok
}

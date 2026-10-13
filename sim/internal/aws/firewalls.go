package aws

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const firewallRetryDelay = 5 * time.Second

// policyFor returns the firewall of an instance's machine: the union of the
// rules of its security groups. A rule that names a security group admits
// the private addresses of the instances in that group, as in a VPC.
func policyFor(w *world, i *instance) *machines.Firewall {
	policy := &machines.Firewall{Inbound: []machines.Rule{}, Outbound: []machines.Rule{}}
	for _, id := range i.SecurityGroupIDs {
		g := w.securityGroup(i.Region, id)
		if g == nil {
			continue
		}
		for _, p := range g.Ingress {
			policy.Inbound = append(policy.Inbound, machineRules(w, p)...)
		}
		for _, p := range g.Egress {
			policy.Outbound = append(policy.Outbound, machineRules(w, p)...)
		}
	}
	return policy
}

func machineRules(w *world, p permission) []machines.Rule {
	peers := append([]string{}, p.CIDRs...)
	for _, id := range p.Groups {
		for _, member := range w.Instances {
			if member.PrivateIP != "" && member.State != "terminated" && slices.Contains(member.SecurityGroupIDs, id) && !slices.Contains(peers, member.PrivateIP) {
				peers = append(peers, member.PrivateIP)
			}
		}
	}
	if len(peers) == 0 {
		return nil
	}
	protocols := []string{p.Protocol}
	if p.Protocol == "-1" {
		protocols = []string{"tcp", "udp", "icmp"}
	}
	var out []machines.Rule
	for _, protocol := range protocols {
		rule := machines.Rule{Protocol: protocol, Peers: peers}
		if protocol != "icmp" && p.Protocol != "-1" && (p.FromPort != 0 || p.ToPort != 65535) {
			rule.Ports = strconv.Itoa(p.FromPort)
			if p.ToPort != p.FromPort {
				rule.Ports += "-" + strconv.Itoa(p.ToPort)
			}
		}
		out = append(out, rule)
	}
	return out
}

func policyKey(policy *machines.Firewall) string {
	data, _ := json.Marshal(policy)
	return string(data)
}

// firewallSync remembers the policy each machine last received, and wakes
// the push loop when security groups or instances change.
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

// pushFirewalls gives each running machine its current policy and reports
// whether every push succeeded.
func (s *Simulation) pushFirewalls(ctx context.Context) bool {
	type push struct {
		instanceID string
		machine    string
		policy     *machines.Firewall
	}
	var pushes []push
	s.store.Read(func(w *world) {
		for _, i := range w.Instances {
			if i.State != "running" || i.Machine == "" {
				continue
			}
			if p := policyFor(w, i); !s.firewalls.has(i.ID, policyKey(p)) {
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

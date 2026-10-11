package digitalocean

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

const (
	maxFirewallRules    = 50
	maxFirewallDroplets = 10
	maxFirewallTags     = 5
	firewallRetryDelay  = 5 * time.Second
)

var firewallNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.\-]+$`)

type targets struct {
	Addresses        []string `json:"addresses,omitempty"`
	DropletIDs       []int64  `json:"droplet_ids,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	LoadBalancerUIDs []string `json:"load_balancer_uids,omitempty"`
	KubernetesIDs    []string `json:"kubernetes_ids,omitempty"`
}

type inboundRule struct {
	Protocol string  `json:"protocol"`
	Ports    string  `json:"ports,omitempty"`
	Action   string  `json:"action"`
	Sources  targets `json:"sources"`
}

type outboundRule struct {
	Protocol     string  `json:"protocol"`
	Ports        string  `json:"ports,omitempty"`
	Action       string  `json:"action"`
	Destinations targets `json:"destinations"`
}

type firewallRequest struct {
	Name          string         `json:"name"`
	InboundRules  []inboundRule  `json:"inbound_rules"`
	OutboundRules []outboundRule `json:"outbound_rules"`
	DropletIDs    []int64        `json:"droplet_ids"`
	Tags          []string       `json:"tags"`
}

type pendingChange struct {
	DropletID int64  `json:"droplet_id"`
	Removing  bool   `json:"removing"`
	Status    string `json:"status"`
}

type firewallJSON struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Status         string          `json:"status"`
	InboundRules   []inboundRule   `json:"inbound_rules"`
	OutboundRules  []outboundRule  `json:"outbound_rules"`
	CreatedAt      string          `json:"created_at"`
	DropletIDs     []int64         `json:"droplet_ids"`
	Tags           []string        `json:"tags"`
	PendingChanges []pendingChange `json:"pending_changes"`
}

func (s *Simulation) renderFirewall(a *account, f *firewall) firewallJSON {
	out := firewallJSON{
		ID: f.ID, Name: f.Name, Status: "succeeded", InboundRules: nonNil(f.InboundRules), OutboundRules: nonNil(f.OutboundRules),
		CreatedAt: timestamp(f.CreatedAt), DropletIDs: nonNil(f.DropletIDs), Tags: nonNil(f.Tags), PendingChanges: []pendingChange{},
	}
	for _, d := range a.Droplets {
		if !f.appliesTo(d) || d.Status != "active" || d.Machine == "" {
			continue
		}
		if !s.firewalls.has(d.ID, policyKey(policyFor(a, d))) {
			out.Status = "waiting"
			out.PendingChanges = append(out.PendingChanges, pendingChange{DropletID: d.ID, Status: "waiting"})
		}
	}
	return out
}

func (f *firewall) appliesTo(d *droplet) bool {
	return slices.Contains(f.DropletIDs, d.ID) || slices.ContainsFunc(f.Tags, func(t string) bool { return slices.Contains(d.Tags, t) })
}

func checkFirewall(a *account, req *firewallRequest) error {
	switch {
	case !firewallNamePattern.MatchString(req.Name):
		return errUnprocessable("name must start with a letter or digit and contain only letters, digits, '.' and '-'")
	case len(req.InboundRules)+len(req.OutboundRules) == 0:
		return errUnprocessable("must contain at least one inbound or outbound access rule")
	case len(req.InboundRules)+len(req.OutboundRules) > maxFirewallRules:
		return errUnprocessable("a firewall may have at most %d rules", maxFirewallRules)
	}
	for i := range req.InboundRules {
		r := &req.InboundRules[i]
		if err := checkRule(a, r.Protocol, r.Ports, &r.Action, r.Sources); err != nil {
			return err
		}
	}
	for i := range req.OutboundRules {
		r := &req.OutboundRules[i]
		if err := checkRule(a, r.Protocol, r.Ports, &r.Action, r.Destinations); err != nil {
			return err
		}
	}
	return checkMembers(a, req.DropletIDs, req.Tags)
}

func checkMembers(a *account, dropletIDs []int64, tags []string) error {
	if len(dropletIDs) > maxFirewallDroplets {
		return errUnprocessable("a firewall may have at most %d droplets; use tags", maxFirewallDroplets)
	}
	if len(tags) > maxFirewallTags {
		return errUnprocessable("a firewall may have at most %d tags", maxFirewallTags)
	}
	return checkTargets(a, targets{DropletIDs: dropletIDs, Tags: tags})
}

func checkRule(a *account, protocol, ports string, action *string, t targets) error {
	switch *action {
	case "":
		*action = "allow"
	case "allow":
	case "deny":
		return errUnprocessable("deny rules are not supported by the simulation")
	default:
		return errUnprocessable("action must be allow or deny")
	}
	switch protocol {
	case "icmp":
		if ports != "" && ports != "0" {
			return errUnprocessable("ICMP rules take no ports")
		}
	case "tcp", "udp":
		if !validPorts(ports) {
			return errUnprocessable("ports %q must be a port, a range such as 8000-9000, 0 or all", ports)
		}
	default:
		return errUnprocessable("protocol must be tcp, udp or icmp")
	}
	return checkTargets(a, t)
}

func validPorts(ports string) bool {
	if ports == "all" || ports == "0" {
		return true
	}
	low, high, isRange := strings.Cut(ports, "-")
	if !isRange {
		high = low
	}
	lo, err1 := strconv.Atoi(low)
	hi, err2 := strconv.Atoi(high)
	return err1 == nil && err2 == nil && lo >= 1 && hi <= 65535 && lo <= hi
}

func checkTargets(a *account, t targets) error {
	if len(t.LoadBalancerUIDs) > 0 || len(t.KubernetesIDs) > 0 {
		return errUnprocessable("load balancers and Kubernetes clusters are not simulated")
	}
	for _, addr := range t.Addresses {
		if _, err := netip.ParsePrefix(addr); err != nil {
			if _, err := netip.ParseAddr(addr); err != nil {
				return errUnprocessable("address %q is not an IP address or CIDR block", addr)
			}
		}
	}
	for _, id := range t.DropletIDs {
		if a.droplet(id) == nil {
			return errUnprocessable("droplet %d does not exist", id)
		}
	}
	for _, name := range t.Tags {
		if a.tag(name) == nil {
			return errUnprocessable("tag %q does not exist", name)
		}
	}
	return nil
}

func (s *Simulation) createFirewall(r *http.Request) (int, any, error) {
	var req firewallRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out firewallJSON
	err := s.store.Update(func(a *account) error {
		if err := checkFirewall(a, &req); err != nil {
			return err
		}
		f := &firewall{
			ID: newUUID(), Name: req.Name, InboundRules: req.InboundRules, OutboundRules: req.OutboundRules,
			DropletIDs: req.DropletIDs, Tags: req.Tags, CreatedAt: s.now().UTC(),
		}
		a.Firewalls = append(a.Firewalls, f)
		out = s.renderFirewall(a, f)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusAccepted, map[string]any{"firewall": out}, nil
}

func (s *Simulation) listFirewalls(r *http.Request) (int, any, error) {
	var out []firewallJSON
	s.store.Read(func(a *account) {
		for _, f := range a.Firewalls {
			out = append(out, s.renderFirewall(a, f))
		}
	})
	return listResponse(r, "firewalls", out)
}

func (s *Simulation) getFirewall(r *http.Request) (int, any, error) {
	var out *firewallJSON
	s.store.Read(func(a *account) {
		if f := a.firewall(r.PathValue("id")); f != nil {
			j := s.renderFirewall(a, f)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"firewall": out}, nil
}

func (s *Simulation) updateFirewall(r *http.Request) (int, any, error) {
	var req firewallRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out firewallJSON
	err := s.store.Update(func(a *account) error {
		f := a.firewall(r.PathValue("id"))
		if f == nil {
			return errNotFound()
		}
		if err := checkFirewall(a, &req); err != nil {
			return err
		}
		f.Name, f.InboundRules, f.OutboundRules, f.DropletIDs, f.Tags = req.Name, req.InboundRules, req.OutboundRules, req.DropletIDs, req.Tags
		out = s.renderFirewall(a, f)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusOK, map[string]any{"firewall": out}, nil
}

func (s *Simulation) deleteFirewall(r *http.Request) (int, any, error) {
	return s.changeFirewall(r, func(a *account, f *firewall) error {
		a.Firewalls = slices.DeleteFunc(a.Firewalls, func(other *firewall) bool { return other.ID == f.ID })
		return nil
	}, nil)
}

func (s *Simulation) addFirewallDroplets(r *http.Request) (int, any, error) {
	var req struct {
		DropletIDs []int64 `json:"droplet_ids"`
	}
	return s.changeFirewall(r, func(a *account, f *firewall) error {
		ids := slices.Clone(f.DropletIDs)
		for _, id := range req.DropletIDs {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		if err := checkMembers(a, ids, f.Tags); err != nil {
			return err
		}
		f.DropletIDs = ids
		return nil
	}, &req)
}

func (s *Simulation) removeFirewallDroplets(r *http.Request) (int, any, error) {
	var req struct {
		DropletIDs []int64 `json:"droplet_ids"`
	}
	return s.changeFirewall(r, func(a *account, f *firewall) error {
		f.DropletIDs = slices.DeleteFunc(f.DropletIDs, func(id int64) bool { return slices.Contains(req.DropletIDs, id) })
		return nil
	}, &req)
}

func (s *Simulation) addFirewallTags(r *http.Request) (int, any, error) {
	var req struct {
		Tags []string `json:"tags"`
	}
	return s.changeFirewall(r, func(a *account, f *firewall) error {
		tags := slices.Clone(f.Tags)
		for _, t := range req.Tags {
			tags = appendUnique(tags, t)
		}
		if err := checkMembers(a, f.DropletIDs, tags); err != nil {
			return err
		}
		f.Tags = tags
		return nil
	}, &req)
}

func (s *Simulation) removeFirewallTags(r *http.Request) (int, any, error) {
	var req struct {
		Tags []string `json:"tags"`
	}
	return s.changeFirewall(r, func(a *account, f *firewall) error {
		f.Tags = slices.DeleteFunc(f.Tags, func(t string) bool { return slices.Contains(req.Tags, t) })
		return nil
	}, &req)
}

func (s *Simulation) addFirewallRules(r *http.Request) (int, any, error) {
	var req firewallRequest
	return s.changeFirewall(r, func(a *account, f *firewall) error {
		next := firewallRequest{
			Name: f.Name, InboundRules: append(slices.Clone(f.InboundRules), req.InboundRules...),
			OutboundRules: append(slices.Clone(f.OutboundRules), req.OutboundRules...), DropletIDs: f.DropletIDs, Tags: f.Tags,
		}
		if len(req.InboundRules)+len(req.OutboundRules) == 0 {
			return errUnprocessable("give at least one inbound or outbound rule")
		}
		if err := checkFirewall(a, &next); err != nil {
			return err
		}
		f.InboundRules, f.OutboundRules = next.InboundRules, next.OutboundRules
		return nil
	}, &req)
}

func (s *Simulation) removeFirewallRules(r *http.Request) (int, any, error) {
	var req firewallRequest
	return s.changeFirewall(r, func(a *account, f *firewall) error {
		for i := range req.InboundRules {
			if req.InboundRules[i].Action == "" {
				req.InboundRules[i].Action = "allow"
			}
		}
		for i := range req.OutboundRules {
			if req.OutboundRules[i].Action == "" {
				req.OutboundRules[i].Action = "allow"
			}
		}
		f.InboundRules = slices.DeleteFunc(f.InboundRules, func(r inboundRule) bool {
			return slices.ContainsFunc(req.InboundRules, func(x inboundRule) bool { return reflect.DeepEqual(x, r) })
		})
		f.OutboundRules = slices.DeleteFunc(f.OutboundRules, func(r outboundRule) bool {
			return slices.ContainsFunc(req.OutboundRules, func(x outboundRule) bool { return reflect.DeepEqual(x, r) })
		})
		return nil
	}, &req)
}

func (s *Simulation) changeFirewall(r *http.Request, change func(*account, *firewall) error, req any) (int, any, error) {
	if req != nil {
		if err := decodeBody(r, req); err != nil {
			return 0, nil, err
		}
	}
	err := s.store.Update(func(a *account) error {
		f := a.firewall(r.PathValue("id"))
		if f == nil {
			return errNotFound()
		}
		return change(a, f)
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusNoContent, nil, nil
}

func policyFor(a *account, d *droplet) *machines.Firewall {
	var policy *machines.Firewall
	for _, f := range a.Firewalls {
		if !f.appliesTo(d) {
			continue
		}
		if policy == nil {
			policy = &machines.Firewall{Inbound: []machines.Rule{}, Outbound: []machines.Rule{}}
		}
		for _, r := range f.InboundRules {
			policy.Inbound = append(policy.Inbound, machineRule(a, r.Protocol, r.Ports, r.Sources))
		}
		for _, r := range f.OutboundRules {
			policy.Outbound = append(policy.Outbound, machineRule(a, r.Protocol, r.Ports, r.Destinations))
		}
	}
	return policy
}

func machineRule(a *account, protocol, ports string, t targets) machines.Rule {
	rule := machines.Rule{Protocol: protocol, Peers: []string{}}
	if protocol != "icmp" && ports != "all" && ports != "0" && ports != "1-65535" {
		rule.Ports = ports
	}
	add := func(ip string) {
		if ip != "" && !slices.Contains(rule.Peers, ip) {
			rule.Peers = append(rule.Peers, ip)
		}
	}
	for _, addr := range t.Addresses {
		if p, err := netip.ParsePrefix(addr); err == nil && p.Addr().Is4() {
			add(p.Masked().String())
		} else if ip, err := netip.ParseAddr(addr); err == nil && ip.Is4() {
			add(ip.String())
		}
	}
	for _, id := range t.DropletIDs {
		if d := a.droplet(id); d != nil {
			add(d.PrivateIP)
			add(d.PublicIP)
		}
	}
	for _, name := range t.Tags {
		for _, d := range a.dropletsTagged(name) {
			add(d.PrivateIP)
			add(d.PublicIP)
		}
	}
	return rule
}

func policyKey(policy *machines.Firewall) string {
	data, _ := json.Marshal(policy)
	return string(data)
}

type firewallSync struct {
	kicks  chan struct{}
	mu     sync.Mutex
	pushed map[int64]string
}

func newFirewallSync() *firewallSync {
	return &firewallSync{kicks: make(chan struct{}, 1), pushed: map[int64]string{}}
}

func (f *firewallSync) kick() {
	select {
	case f.kicks <- struct{}{}:
	default:
	}
}

func (f *firewallSync) applied(id int64, policy *machines.Firewall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed[id] = policyKey(policy)
}

func (f *firewallSync) forget(id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pushed, id)
}

func (f *firewallSync) has(id int64, key string) bool {
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
		dropletID int64
		machine   string
		policy    *machines.Firewall
	}
	var pushes []push
	s.store.Read(func(a *account) {
		for _, d := range a.Droplets {
			if d.Status != "active" || d.Locked || d.Machine == "" {
				continue
			}
			if p := policyFor(a, d); !s.firewalls.has(d.ID, policyKey(p)) {
				pushes = append(pushes, push{d.ID, d.Machine, p})
			}
		}
	})
	ok := true
	for _, p := range pushes {
		if err := s.machines.SetFirewall(ctx, p.machine, p.policy); err != nil {
			s.log.Warn("push firewall to droplet machine", "droplet", p.dropletID, "error", err)
			ok = false
			continue
		}
		s.firewalls.applied(p.dropletID, p.policy)
	}
	return ok
}

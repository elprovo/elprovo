package hetzner

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

	"github.com/elprovo/elprovo/sim/internal/machines"
)

const (
	maxFirewallRules      = 50
	maxRuleAddresses      = 100
	maxFirewallsPerServer = 5
	firewallRetryDelay    = 5 * time.Second
)

type firewallRule struct {
	Description    *string  `json:"description"`
	Direction      string   `json:"direction"`
	SourceIPs      []string `json:"source_ips"`
	DestinationIPs []string `json:"destination_ips"`
	Protocol       string   `json:"protocol"`
	Port           *string  `json:"port"`
}

type idJSON struct {
	ID int64 `json:"id"`
}

type selectorJSON struct {
	Selector string `json:"selector"`
}

// firewallResource is what a firewall applies to: one server, or the
// servers that match a label selector.
type firewallResource struct {
	Type          string        `json:"type"`
	Server        *idJSON       `json:"server,omitempty"`
	LabelSelector *selectorJSON `json:"label_selector,omitempty"`
}

type appliedResourceJSON struct {
	Type   string `json:"type"`
	Server idJSON `json:"server"`
}

type appliedToJSON struct {
	Type               string                `json:"type"`
	Server             *idJSON               `json:"server,omitempty"`
	LabelSelector      *selectorJSON         `json:"label_selector,omitempty"`
	AppliedToResources []appliedResourceJSON `json:"applied_to_resources,omitempty"`
}

type firewallJSON struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	Created   string            `json:"created"`
	Rules     []firewallRule    `json:"rules"`
	AppliedTo []appliedToJSON   `json:"applied_to"`
}

func (f *firewall) render(p *project) firewallJSON {
	out := firewallJSON{
		ID: f.ID, Name: f.Name, Labels: labelsOrEmpty(f.Labels), Created: timestamp(f.Created),
		Rules: nonNil(f.Rules), AppliedTo: []appliedToJSON{},
	}
	for _, res := range f.AppliedTo {
		entry := appliedToJSON{Type: res.Type, Server: res.Server, LabelSelector: res.LabelSelector}
		if res.LabelSelector != nil {
			entry.AppliedToResources = []appliedResourceJSON{}
			for _, srv := range p.Servers {
				if !f.appliesDirectly(srv.ID) && matchesSelector(res.LabelSelector.Selector, srv.Labels) {
					entry.AppliedToResources = append(entry.AppliedToResources, appliedResourceJSON{Type: "server", Server: idJSON{srv.ID}})
				}
			}
		}
		out.AppliedTo = append(out.AppliedTo, entry)
	}
	return out
}

func matchesSelector(text string, labels map[string]string) bool {
	sel, err := parseSelector(text)
	return err == nil && sel.matches(labels)
}

func (f *firewall) appliesDirectly(serverID int64) bool {
	return slices.ContainsFunc(f.AppliedTo, func(r firewallResource) bool { return r.Server != nil && r.Server.ID == serverID })
}

func (f *firewall) applyToServer(serverID int64) {
	if !f.appliesDirectly(serverID) {
		f.AppliedTo = append(f.AppliedTo, firewallResource{Type: "server", Server: &idJSON{serverID}})
	}
}

func (f *firewall) appliesTo(srv *server) bool {
	return f.appliesDirectly(srv.ID) || slices.ContainsFunc(f.AppliedTo, func(r firewallResource) bool {
		return r.LabelSelector != nil && matchesSelector(r.LabelSelector.Selector, srv.Labels)
	})
}

// firewallsOf returns the firewalls that apply to the server, directly or
// through a label selector.
func (p *project) firewallsOf(srv *server) []*firewall {
	var out []*firewall
	for _, f := range p.Firewalls {
		if f.appliesTo(srv) {
			out = append(out, f)
		}
	}
	return out
}

// serversOf returns the servers a firewall resource covers.
func (p *project) serversOf(res firewallResource) []*server {
	var out []*server
	for _, srv := range p.Servers {
		if (res.Server != nil && res.Server.ID == srv.ID) || (res.LabelSelector != nil && matchesSelector(res.LabelSelector.Selector, srv.Labels)) {
			out = append(out, srv)
		}
	}
	return out
}

// checkRules validates rules as the API reference describes them and
// returns them as Hetzner echoes them: IPv6 blocks in canonical form, and
// a null port and description where none was given.
func checkRules(rules []firewallRule) ([]firewallRule, error) {
	if len(rules) > maxFirewallRules {
		return nil, errInvalidInput("rules", "a firewall may have at most %d rules", maxFirewallRules)
	}
	out := make([]firewallRule, 0, len(rules))
	for _, r := range rules {
		rule, err := checkRule(r)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, nil
}

func checkRule(r firewallRule) (firewallRule, error) {
	switch r.Protocol {
	case "tcp", "udp":
		if r.Port == nil || !validPort(*r.Port) {
			return r, errInvalidInput("rules", "port must be a port, a range such as 1024-5000, or any, for protocol %s", r.Protocol)
		}
	case "icmp":
		if r.Port != nil {
			return r, errInvalidInput("rules", "port is only applicable for protocols tcp and udp")
		}
	case "esp", "gre":
		return r, errUnsupported("%s rules are not supported by the simulation: machines filter tcp, udp and icmp", r.Protocol)
	default:
		return r, errInvalidInput("rules", "protocol must be tcp, udp, icmp, esp or gre")
	}
	var err error
	switch r.Direction {
	case "in":
		if len(r.DestinationIPs) > 0 {
			return r, errInvalidInput("rules", "destination_ips are only allowed for direction out")
		}
		r.SourceIPs, err = canonicalBlocks(r.SourceIPs)
		r.DestinationIPs = []string{}
	case "out":
		if len(r.SourceIPs) > 0 {
			return r, errInvalidInput("rules", "source_ips are only allowed for direction in")
		}
		r.DestinationIPs, err = canonicalBlocks(r.DestinationIPs)
		r.SourceIPs = []string{}
	default:
		return r, errInvalidInput("rules", "direction must be in or out")
	}
	return r, err
}

func validPort(port string) bool {
	if port == "any" {
		return true
	}
	low, high, isRange := strings.Cut(port, "-")
	if !isRange {
		high = low
	}
	lo, err1 := strconv.Atoi(low)
	hi, err2 := strconv.Atoi(high)
	return err1 == nil && err2 == nil && lo >= 1 && hi <= 65535 && lo <= hi
}

func canonicalBlocks(blocks []string) ([]string, error) {
	if len(blocks) == 0 {
		return nil, errInvalidInput("rules", "a rule needs at least one IP block")
	}
	if len(blocks) > maxRuleAddresses {
		return nil, errInvalidInput("rules", "a rule may name at most %d IP blocks", maxRuleAddresses)
	}
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		prefix, err := netip.ParsePrefix(b)
		if err != nil || prefix.Masked() != prefix {
			return nil, errInvalidInput("rules", "%q is not a CIDR block with empty host bits", b)
		}
		if prefix.Addr().Is6() {
			b = prefix.String()
		}
		out = append(out, b)
	}
	return out, nil
}

func checkResources(p *project, resources []firewallResource) error {
	for _, res := range resources {
		switch {
		case res.Type == "server" && res.Server != nil:
			if p.server(res.Server.ID) == nil {
				return errCode(http.StatusNotFound, "firewall_resource_not_found", "server %d not found", res.Server.ID)
			}
		case res.Type == "label_selector" && res.LabelSelector != nil:
			if _, err := parseSelector(res.LabelSelector.Selector); err != nil {
				return err
			}
		default:
			return errInvalidInput("apply_to", "a resource must be a server or a label_selector with its object")
		}
	}
	return nil
}

func sameResource(a, b firewallResource) bool {
	if a.Server != nil && b.Server != nil {
		return a.Server.ID == b.Server.ID
	}
	return a.LabelSelector != nil && b.LabelSelector != nil && a.LabelSelector.Selector == b.LabelSelector.Selector
}

// applyAction records the action that applies or removes a firewall on a
// resource. It names the firewall, a label selector when there is one, and
// every server the resource covers; it succeeds once each of those servers
// has the firewall's rules.
func (s *Simulation) applyAction(p *project, f *firewall, command string, res firewallResource) *action {
	refs := []resourceRef{{ID: f.ID, Type: "firewall"}}
	for _, srv := range p.serversOf(res) {
		refs = append(refs, resourceRef{ID: srv.ID, Type: "server"})
	}
	return s.startAction(p, command, refs...)
}

type firewallRequest struct {
	Name    string             `json:"name"`
	Labels  map[string]string  `json:"labels"`
	Rules   []firewallRule     `json:"rules"`
	ApplyTo []firewallResource `json:"apply_to"`
}

func (s *Simulation) createFirewall(r *http.Request) (int, any, error) {
	var req firewallRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Name == "" {
		return 0, nil, errInvalidInput("name", "is required")
	}
	if err := checkLabels(req.Labels); err != nil {
		return 0, nil, err
	}
	rules, err := checkRules(req.Rules)
	if err != nil {
		return 0, nil, err
	}
	var body map[string]any
	err = s.store.Update(func(p *project) error {
		if slices.ContainsFunc(p.Firewalls, func(f *firewall) bool { return f.Name == req.Name }) {
			return errUniqueness("name", "firewall name is already used")
		}
		if err := checkResources(p, req.ApplyTo); err != nil {
			return err
		}
		f := &firewall{ID: p.nextID(), Name: req.Name, Labels: req.Labels, Rules: rules, Created: s.now().UTC()}
		actions := []actionJSON{}
		for _, res := range req.ApplyTo {
			if slices.ContainsFunc(f.AppliedTo, func(o firewallResource) bool { return sameResource(o, res) }) {
				return errCode(http.StatusUnprocessableEntity, "server_already_added", "a resource is applied more than once")
			}
			f.AppliedTo = append(f.AppliedTo, res)
		}
		p.Firewalls = append(p.Firewalls, f)
		for _, res := range f.AppliedTo {
			actions = append(actions, s.applyAction(p, f, "apply_firewall", res).render())
		}
		body = map[string]any{"firewall": f.render(p), "actions": actions}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusCreated, body, nil
}

func (s *Simulation) listFirewalls(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	keep, err := selectorFilter(q)
	if err != nil {
		return 0, nil, err
	}
	name := q.Get("name")
	var out []firewallJSON
	s.store.Read(func(p *project) {
		for _, f := range p.Firewalls {
			if (name == "" || f.Name == name) && keep(f.Labels) {
				out = append(out, f.render(p))
			}
		}
	})
	return listResponse(r, "firewalls", out, sortFields[firewallJSON]{
		"id":      byID(func(f firewallJSON) int64 { return f.ID }),
		"name":    byString(func(f firewallJSON) string { return f.Name }),
		"created": byString(func(f firewallJSON) string { return f.Created }),
	})
}

func (s *Simulation) getFirewall(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "firewall")
	if err != nil {
		return 0, nil, err
	}
	var out *firewallJSON
	s.store.Read(func(p *project) {
		if f := p.firewall(id); f != nil {
			j := f.render(p)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound("firewall")
	}
	return http.StatusOK, map[string]any{"firewall": out}, nil
}

func (s *Simulation) updateFirewall(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "firewall")
	if err != nil {
		return 0, nil, err
	}
	var req struct {
		Name   string             `json:"name"`
		Labels *map[string]string `json:"labels"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Labels != nil {
		if err := checkLabels(*req.Labels); err != nil {
			return 0, nil, err
		}
	}
	var out firewallJSON
	err = s.store.Update(func(p *project) error {
		f := p.firewall(id)
		if f == nil {
			return errNotFound("firewall")
		}
		if req.Name != "" && req.Name != f.Name {
			if slices.ContainsFunc(p.Firewalls, func(o *firewall) bool { return o.Name == req.Name }) {
				return errUniqueness("name", "firewall name is already used")
			}
			f.Name = req.Name
		}
		if req.Labels != nil {
			f.Labels = *req.Labels
		}
		out = f.render(p)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"firewall": out}, nil
}

// deleteFirewall deletes a firewall that applies to nothing.
func (s *Simulation) deleteFirewall(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "firewall")
	if err != nil {
		return 0, nil, err
	}
	err = s.store.Update(func(p *project) error {
		f := p.firewall(id)
		if f == nil {
			return errNotFound("firewall")
		}
		if len(f.AppliedTo) > 0 {
			return errCode(http.StatusUnprocessableEntity, "resource_in_use", "firewall %d is still applied to a resource", id)
		}
		p.Firewalls = slices.DeleteFunc(p.Firewalls, func(o *firewall) bool { return o.ID == id })
		return nil
	})
	return http.StatusNoContent, nil, err
}

type firewallActionRequest struct {
	Rules      *[]firewallRule    `json:"rules"`
	ApplyTo    []firewallResource `json:"apply_to"`
	RemoveFrom []firewallResource `json:"remove_from"`
}

func (s *Simulation) postFirewallAction(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "firewall")
	if err != nil {
		return 0, nil, err
	}
	var req firewallActionRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	name := r.PathValue("action")
	var rules []firewallRule
	if name == "set_rules" {
		if req.Rules == nil {
			return 0, nil, errInvalidInput("rules", "is required")
		}
		if rules, err = checkRules(*req.Rules); err != nil {
			return 0, nil, err
		}
	}
	actions := []actionJSON{}
	err = s.store.Update(func(p *project) error {
		f := p.firewall(id)
		if f == nil {
			return errNotFound("firewall")
		}
		var started []*action
		var err error
		switch name {
		case "set_rules":
			started = s.setRules(p, f, rules)
		case "apply_to_resources":
			started, err = s.applyFirewall(p, f, req.ApplyTo)
		case "remove_from_resources":
			started, err = s.removeFirewall(p, f, req.RemoveFrom)
		default:
			return errUnsupported("firewall action %q is not supported by the simulation", name)
		}
		for _, a := range started {
			actions = append(actions, a.render())
		}
		return err
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusCreated, map[string]any{"actions": actions}, nil
}

func (s *Simulation) setRules(p *project, f *firewall, rules []firewallRule) []*action {
	f.Rules = rules
	refs := []resourceRef{{ID: f.ID, Type: "firewall"}}
	for _, srv := range p.Servers {
		if f.appliesTo(srv) {
			refs = append(refs, resourceRef{ID: srv.ID, Type: "server"})
		}
	}
	return []*action{s.startAction(p, "set_firewall_rules", refs...)}
}

func (s *Simulation) applyFirewall(p *project, f *firewall, resources []firewallResource) ([]*action, error) {
	if err := checkResources(p, resources); err != nil {
		return nil, err
	}
	for _, res := range resources {
		if slices.ContainsFunc(f.AppliedTo, func(o firewallResource) bool { return sameResource(o, res) }) {
			return nil, errCode(http.StatusUnprocessableEntity, "firewall_already_applied", "firewall %d is already applied to the resource", f.ID)
		}
		if res.Server != nil && len(p.firewallsOf(p.server(res.Server.ID))) >= maxFirewallsPerServer {
			return nil, errResourceLimit("firewalls_per_server", "a server can have at most 5 firewalls")
		}
	}
	var started []*action
	for _, res := range resources {
		f.AppliedTo = append(f.AppliedTo, res)
		started = append(started, s.applyAction(p, f, "apply_firewall", res))
	}
	return started, nil
}

func (s *Simulation) removeFirewall(p *project, f *firewall, resources []firewallResource) ([]*action, error) {
	var started []*action
	for _, res := range resources {
		if res.Server != nil && p.server(res.Server.ID) == nil {
			return nil, errCode(http.StatusNotFound, "firewall_resource_not_found", "server %d not found", res.Server.ID)
		}
		if res.Server != nil && !f.appliesDirectly(res.Server.ID) && f.appliesTo(p.server(res.Server.ID)) {
			return nil, errCode(http.StatusUnprocessableEntity, "firewall_managed_by_label_selector", "firewall %d applies to server %d through a label selector", f.ID, res.Server.ID)
		}
	}
	for _, res := range resources {
		a := s.applyAction(p, f, "remove_firewall", res)
		f.AppliedTo = slices.DeleteFunc(f.AppliedTo, func(o firewallResource) bool { return sameResource(o, res) })
		started = append(started, a)
	}
	return started, nil
}

// policyFor returns the firewall a server's machine enforces: the union of
// the rules of every firewall that applies to it, or nil when none does.
// As on Hetzner, outbound traffic is open unless a firewall has outbound
// rules, and on a server attached to a network, all traffic to and from the
// private network passes, because Hetzner's firewalls do not filter private
// networks. Every private address is on the machines' private Docker
// network, which plays the server's networks.
func (s *Simulation) policyFor(p *project, srv *server) *machines.Firewall {
	fws := p.firewallsOf(srv)
	if len(fws) == 0 {
		return nil
	}
	policy := &machines.Firewall{Inbound: []machines.Rule{}, Outbound: []machines.Rule{}}
	outbound := false
	for _, f := range fws {
		for _, r := range f.Rules {
			if r.Direction == "in" {
				policy.Inbound = append(policy.Inbound, machineRule(r.Protocol, r.Port, r.SourceIPs))
			} else {
				outbound = true
				policy.Outbound = append(policy.Outbound, machineRule(r.Protocol, r.Port, r.DestinationIPs))
			}
		}
	}
	if !outbound {
		policy.Outbound = allowAll([]string{"0.0.0.0/0"})
	}
	if private := s.networks.Private.Prefix; len(srv.Networks) > 0 && private.IsValid() {
		policy.Inbound = append(policy.Inbound, allowAll([]string{private.Masked().String()})...)
		if outbound {
			policy.Outbound = append(policy.Outbound, allowAll([]string{private.Masked().String()})...)
		}
	}
	return policy
}

func machineRule(protocol string, port *string, blocks []string) machines.Rule {
	rule := machines.Rule{Protocol: protocol, Peers: []string{}}
	if port != nil && *port != "any" {
		rule.Ports = *port
	}
	for _, b := range blocks {
		if prefix, err := netip.ParsePrefix(b); err == nil && prefix.Addr().Is4() {
			rule.Peers = append(rule.Peers, prefix.String())
		}
	}
	return rule
}

func allowAll(peers []string) []machines.Rule {
	return []machines.Rule{
		{Protocol: "tcp", Peers: peers}, {Protocol: "udp", Peers: peers}, {Protocol: "icmp", Peers: peers},
	}
}

func policyKey(policy *machines.Firewall) string {
	data, _ := json.Marshal(policy)
	return string(data)
}

// firewallSettled reports whether the server's machine enforces the rules
// it should. A server that is not running has nothing to enforce.
func (s *Simulation) firewallSettled(p *project, srv *server) bool {
	if srv.Status != "running" || srv.Machine == "" {
		return true
	}
	return s.firewalls.has(srv.ID, policyKey(s.policyFor(p, srv)))
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

// syncFirewalls pushes each running server's firewall to its machine when
// it changes, and finishes firewall actions once their servers have their
// rules. Failed pushes are retried.
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
		ok := s.pushFirewalls(ctx)
		s.settleFirewallActions()
		if !ok {
			retry.Reset(firewallRetryDelay)
		}
	}
}

func (s *Simulation) pushFirewalls(ctx context.Context) bool {
	type push struct {
		serverID int64
		machine  string
		policy   *machines.Firewall
	}
	var pushes []push
	s.store.Read(func(p *project) {
		for _, srv := range p.Servers {
			if srv.Status != "running" || srv.Machine == "" || p.running(resourceRef{ID: srv.ID, Type: "server"}) {
				continue
			}
			if policy := s.policyFor(p, srv); !s.firewalls.has(srv.ID, policyKey(policy)) {
				pushes = append(pushes, push{srv.ID, srv.Machine, policy})
			}
		}
	})
	ok := true
	for _, push := range pushes {
		if err := s.machines.SetFirewall(ctx, push.machine, push.policy); err != nil {
			s.log.Warn("push firewall to server machine", "server", push.serverID, "error", err)
			ok = false
			continue
		}
		s.firewalls.applied(push.serverID, push.policy)
	}
	return ok
}

// settleFirewallActions finishes each running firewall action whose servers
// all enforce their current rules.
func (s *Simulation) settleFirewallActions() {
	pending := false
	s.store.Read(func(p *project) { pending = slices.ContainsFunc(p.Actions, runningFirewallAction) })
	if !pending {
		return
	}
	err := s.store.Update(func(p *project) error {
		for _, a := range p.Actions {
			if !runningFirewallAction(a) {
				continue
			}
			settled := true
			for _, ref := range a.Resources[1:] {
				if srv := p.server(ref.ID); ref.Type == "server" && srv != nil && !s.firewallSettled(p, srv) {
					settled = false
				}
			}
			if settled {
				s.finish(a, nil)
			}
		}
		return nil
	})
	if err != nil {
		s.log.Error("record firewall actions", "error", err)
	}
}

func runningFirewallAction(a *action) bool {
	return a.Status == "running" && len(a.Resources) > 0 && a.Resources[0].Type == "firewall"
}

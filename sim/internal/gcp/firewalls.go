package gcp

import (
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

// firewall is a VPC firewall rule. The simulation supports allow rules;
// ingress rules are enforced inside each machine, and egress is always
// allowed, as the implied egress rule of a VPC network allows it.
type firewall struct {
	ID                uint64      `json:"id"`
	Name              string      `json:"name"`
	Description       string      `json:"description"`
	Network           string      `json:"network"`
	Priority          int         `json:"priority"`
	Direction         string      `json:"direction"`
	Allowed           []allowRule `json:"allowed"`
	SourceRanges      []string    `json:"source_ranges"`
	SourceTags        []string    `json:"source_tags"`
	TargetTags        []string    `json:"target_tags"`
	DestinationRanges []string    `json:"destination_ranges"`
	Disabled          bool        `json:"disabled"`
	CreatedAt         time.Time   `json:"created_at"`
}

type allowRule struct {
	Protocol string   `json:"IPProtocol"`
	Ports    []string `json:"ports,omitempty"`
}

var protocolNumbers = map[string]string{"6": "tcp", "17": "udp", "1": "icmp"}

type firewallJSON struct {
	Kind              string      `json:"kind"`
	ID                string      `json:"id"`
	CreationTimestamp string      `json:"creationTimestamp"`
	Name              string      `json:"name"`
	Description       string      `json:"description,omitempty"`
	Network           string      `json:"network"`
	Priority          int         `json:"priority"`
	Direction         string      `json:"direction"`
	Allowed           []allowRule `json:"allowed"`
	SourceRanges      []string    `json:"sourceRanges,omitempty"`
	SourceTags        []string    `json:"sourceTags,omitempty"`
	TargetTags        []string    `json:"targetTags,omitempty"`
	DestinationRanges []string    `json:"destinationRanges,omitempty"`
	LogConfig         logConfig   `json:"logConfig"`
	Disabled          bool        `json:"disabled"`
	SelfLink          string      `json:"selfLink"`
}

type logConfig struct {
	Enable bool `json:"enable"`
}

func (s *Simulation) renderFirewall(f *firewall) firewallJSON {
	return firewallJSON{
		Kind: "compute#firewall", ID: idString(f.ID), CreationTimestamp: timestamp(f.CreatedAt), Name: f.Name,
		Description: f.Description, Network: s.link("global", "networks", f.Network), Priority: f.Priority,
		Direction: f.Direction, Allowed: f.Allowed, SourceRanges: f.SourceRanges, SourceTags: f.SourceTags,
		TargetTags: f.TargetTags, DestinationRanges: f.DestinationRanges, Disabled: f.Disabled,
		SelfLink: s.link("global", "firewalls", f.Name),
	}
}

type firewallRequest struct {
	Name                  string      `json:"name"`
	Description           string      `json:"description"`
	Network               string      `json:"network"`
	Priority              *int        `json:"priority"`
	Direction             string      `json:"direction"`
	Allowed               []allowRule `json:"allowed"`
	Denied                []allowRule `json:"denied"`
	SourceRanges          []string    `json:"sourceRanges"`
	SourceTags            []string    `json:"sourceTags"`
	TargetTags            []string    `json:"targetTags"`
	SourceServiceAccounts []string    `json:"sourceServiceAccounts"`
	TargetServiceAccounts []string    `json:"targetServiceAccounts"`
	DestinationRanges     []string    `json:"destinationRanges"`
	Disabled              bool        `json:"disabled"`
	LogConfig             *logConfig  `json:"logConfig"`
}

func (s *Simulation) checkFirewall(req firewallRequest) (*firewall, error) {
	if err := checkName("firewall", req.Name); err != nil {
		return nil, err
	}
	f := &firewall{
		Name: req.Name, Description: req.Description, Priority: 1000, Direction: req.Direction, Allowed: req.Allowed,
		SourceRanges: req.SourceRanges, SourceTags: req.SourceTags, TargetTags: req.TargetTags,
		DestinationRanges: req.DestinationRanges, Disabled: req.Disabled,
	}
	if f.Direction == "" {
		f.Direction = "INGRESS"
	}
	if req.Priority != nil {
		f.Priority = *req.Priority
	}
	network := req.Network
	if network == "" {
		network = "global/networks/default"
	}
	netRef := parseRef(network)
	switch {
	case !s.belongs(netRef, "networks", ""):
		return nil, errInvalid("Invalid value for field 'resource.network': '%s'.", req.Network)
	case len(req.Denied) > 0:
		return nil, errUnsupported("A deny rule (denied)")
	case len(req.SourceServiceAccounts)+len(req.TargetServiceAccounts) > 0:
		return nil, errUnsupported("Matching service accounts (sourceServiceAccounts, targetServiceAccounts)")
	case req.LogConfig != nil && req.LogConfig.Enable:
		return nil, errUnsupported("Firewall rules logging (logConfig.enable)")
	case len(f.Allowed) == 0:
		return nil, errInvalid("Invalid value for field 'resource.allowed': ''. Exactly one of allowed or denied must be set.")
	case f.Priority < 0 || f.Priority > 65535:
		return nil, errInvalid("Invalid value for field 'resource.priority': '%d'. Must be between 0 and 65535.", f.Priority)
	case f.Direction != "INGRESS" && f.Direction != "EGRESS":
		return nil, errInvalid("Invalid value for field 'resource.direction': '%s'.", f.Direction)
	case f.Direction == "EGRESS" && len(f.SourceTags) > 0:
		return nil, errInvalid("Invalid value for field 'resource.sourceTags': source tags may not be set for an egress rule.")
	case f.Direction == "INGRESS" && len(f.DestinationRanges) > 0:
		return nil, errUnsupported("destinationRanges on an ingress rule")
	}
	f.Network = netRef.name
	if f.Direction == "INGRESS" && len(f.SourceRanges) == 0 && len(f.SourceTags) == 0 {
		f.SourceRanges = []string{"0.0.0.0/0"}
	}
	for _, rng := range append(slices.Clone(f.SourceRanges), f.DestinationRanges...) {
		if _, err := netip.ParsePrefix(rng); err != nil {
			if _, err := netip.ParseAddr(rng); err != nil {
				return nil, errInvalid("Invalid value for field 'resource.sourceRanges': '%s'. Must be a CIDR address range.", rng)
			}
		}
	}
	for i, a := range f.Allowed {
		if p, ok := protocolNumbers[a.Protocol]; ok {
			f.Allowed[i].Protocol = p
		}
		if err := checkAllowRule(f.Allowed[i]); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func checkAllowRule(a allowRule) error {
	switch a.Protocol {
	case "tcp", "udp":
	case "icmp", "all":
		if len(a.Ports) > 0 {
			return errInvalid("Invalid value for field 'resource.allowed[].ports': ports may be set only for tcp, udp, sctp.")
		}
	default:
		return errUnsupported("Protocol %q", a.Protocol)
	}
	for _, p := range a.Ports {
		if !validPorts(p) {
			return errInvalid("Invalid value for field 'resource.allowed[].ports': '%s'. Must be a port or a range such as 8000-9000.", p)
		}
	}
	return nil
}

func validPorts(ports string) bool {
	low, high, isRange := strings.Cut(ports, "-")
	if !isRange {
		high = low
	}
	lo, err1 := strconv.Atoi(low)
	hi, err2 := strconv.Atoi(high)
	return err1 == nil && err2 == nil && lo >= 0 && hi <= 65535 && lo <= hi
}

func (s *Simulation) insertFirewall(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	var req firewallRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	f, err := s.checkFirewall(req)
	if err != nil {
		return 0, nil, err
	}
	var out operationJSON
	err = s.store.Update(func(w *world) error {
		if w.firewall(f.Name) != nil {
			return errAlreadyExists("projects/" + s.cfg.project + "/global/firewalls/" + f.Name)
		}
		if w.network(f.Network) == nil {
			return errNotFound("projects/" + s.cfg.project + "/global/networks/" + f.Network)
		}
		f.ID, f.CreatedAt = w.nextID(), s.now()
		w.Firewalls = append(w.Firewalls, f)
		op := s.newOperation(w, "global", "", "insert", s.link("global", "firewalls", f.Name), f.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusOK, out, nil
}

func (s *Simulation) listFirewalls(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	if err := noFilter(r); err != nil {
		return 0, nil, err
	}
	var out []firewallJSON
	s.store.Read(func(w *world) {
		for _, f := range w.Firewalls {
			out = append(out, s.renderFirewall(f))
		}
	})
	return page(r, "compute#firewallList", s.link("global", "firewalls"), out)
}

func (s *Simulation) getFirewall(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	var out *firewallJSON
	s.store.Read(func(w *world) {
		if f := w.firewall(r.PathValue("name")); f != nil {
			rendered := s.renderFirewall(f)
			out = &rendered
		}
	})
	if out == nil {
		return 0, nil, errNotFound("projects/" + s.cfg.project + "/global/firewalls/" + r.PathValue("name"))
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) deleteFirewall(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	name := r.PathValue("name")
	var out operationJSON
	err := s.store.Update(func(w *world) error {
		f := w.firewall(name)
		if f == nil {
			return errNotFound("projects/" + s.cfg.project + "/global/firewalls/" + name)
		}
		w.Firewalls = slices.DeleteFunc(w.Firewalls, func(other *firewall) bool { return other == f })
		op := s.newOperation(w, "global", "", "delete", s.link("global", "firewalls", name), f.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusOK, out, nil
}

// allowAllOutbound is the implied egress rule of a VPC network.
var allowAllOutbound = []machines.Rule{
	{Protocol: "tcp", Peers: []string{"0.0.0.0/0"}},
	{Protocol: "udp", Peers: []string{"0.0.0.0/0"}},
	{Protocol: "icmp", Peers: []string{"0.0.0.0/0"}},
}

// policyFor returns the traffic the instance's machine accepts: what the
// enabled ingress rules of its network that target it allow, and all
// egress. Source tags resolve to the internal addresses of the network's
// instances with those tags.
func policyFor(w *world, inst *instance) *machines.Firewall {
	policy := &machines.Firewall{Inbound: []machines.Rule{}, Outbound: allowAllOutbound}
	for _, f := range w.Firewalls {
		if f.Network != inst.Network || f.Disabled || f.Direction != "INGRESS" {
			continue
		}
		if len(f.TargetTags) > 0 && !slices.ContainsFunc(f.TargetTags, func(t string) bool { return slices.Contains(inst.Tags, t) }) {
			continue
		}
		peers := sourcePeers(w, f)
		if len(peers) == 0 {
			continue
		}
		for _, a := range f.Allowed {
			policy.Inbound = append(policy.Inbound, machineRules(a, peers)...)
		}
	}
	return policy
}

func sourcePeers(w *world, f *firewall) []string {
	var peers []string
	add := func(p string) {
		if p != "" && !slices.Contains(peers, p) {
			peers = append(peers, p)
		}
	}
	for _, rng := range f.SourceRanges {
		if p, err := netip.ParsePrefix(rng); err == nil && p.Addr().Is4() {
			add(p.Masked().String())
		} else if a, err := netip.ParseAddr(rng); err == nil && a.Is4() {
			add(a.String())
		}
	}
	for _, other := range w.Instances {
		if other.Network == f.Network && slices.ContainsFunc(f.SourceTags, func(t string) bool { return slices.Contains(other.Tags, t) }) {
			add(other.PrivateIP)
		}
	}
	return peers
}

func machineRules(a allowRule, peers []string) []machines.Rule {
	protocols := []string{a.Protocol}
	if a.Protocol == "all" {
		protocols = []string{"tcp", "udp", "icmp"}
	}
	ports := a.Ports
	if len(ports) == 0 || a.Protocol == "icmp" || a.Protocol == "all" {
		ports = []string{""}
	}
	var rules []machines.Rule
	for _, protocol := range protocols {
		for _, p := range ports {
			if p == "0-65535" || p == "1-65535" {
				p = ""
			}
			rules = append(rules, machines.Rule{Protocol: protocol, Ports: p, Peers: slices.Clone(peers)})
		}
	}
	return rules
}

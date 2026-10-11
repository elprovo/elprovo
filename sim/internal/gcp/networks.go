package gcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"time"
)

var namePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

const (
	defaultMTU         = 1460
	defaultRoutingMode = "REGIONAL"
)

type network struct {
	ID                    uint64    `json:"id"`
	Name                  string    `json:"name"`
	Description           string    `json:"description"`
	AutoCreateSubnetworks bool      `json:"auto_create_subnetworks"`
	RoutingMode           string    `json:"routing_mode"`
	MTU                   int       `json:"mtu"`
	EnforcementOrder      string    `json:"enforcement_order"`
	CreatedAt             time.Time `json:"created_at"`
}

type subnetwork struct {
	ID                    uint64    `json:"id"`
	Name                  string    `json:"name"`
	Description           string    `json:"description"`
	Region                string    `json:"region"`
	Network               string    `json:"network"`
	IPCIDRRange           string    `json:"ip_cidr_range"`
	PrivateIPGoogleAccess bool      `json:"private_ip_google_access"`
	CreatedAt             time.Time `json:"created_at"`
}

type networkJSON struct {
	Kind                                  string            `json:"kind"`
	ID                                    string            `json:"id"`
	CreationTimestamp                     string            `json:"creationTimestamp"`
	Name                                  string            `json:"name"`
	Description                           string            `json:"description,omitempty"`
	SelfLink                              string            `json:"selfLink"`
	SelfLinkWithID                        string            `json:"selfLinkWithId"`
	AutoCreateSubnetworks                 bool              `json:"autoCreateSubnetworks"`
	Subnetworks                           []string          `json:"subnetworks,omitempty"`
	RoutingConfig                         map[string]string `json:"routingConfig"`
	MTU                                   int               `json:"mtu"`
	NetworkFirewallPolicyEnforcementOrder string            `json:"networkFirewallPolicyEnforcementOrder"`
}

func (s *Simulation) renderNetwork(w *world, n *network) networkJSON {
	out := networkJSON{
		Kind: "compute#network", ID: idString(n.ID), CreationTimestamp: timestamp(n.CreatedAt), Name: n.Name,
		Description: n.Description, SelfLink: s.link("global", "networks", n.Name), SelfLinkWithID: s.link("global", "networks", idString(n.ID)),
		AutoCreateSubnetworks: n.AutoCreateSubnetworks, MTU: n.MTU, NetworkFirewallPolicyEnforcementOrder: n.EnforcementOrder,
		RoutingConfig: map[string]string{"routingMode": n.RoutingMode, "bgpBestPathSelectionMode": "LEGACY"},
	}
	for _, sub := range w.Subnetworks {
		if sub.Network == n.Name {
			out.Subnetworks = append(out.Subnetworks, s.link("regions", sub.Region, "subnetworks", sub.Name))
		}
	}
	return out
}

type networkRequest struct {
	Name                                  string            `json:"name"`
	Description                           string            `json:"description"`
	AutoCreateSubnetworks                 *bool             `json:"autoCreateSubnetworks"`
	IPv4Range                             string            `json:"IPv4Range"`
	RoutingConfig                         map[string]string `json:"routingConfig"`
	MTU                                   int               `json:"mtu"`
	NetworkFirewallPolicyEnforcementOrder string            `json:"networkFirewallPolicyEnforcementOrder"`
	EnableUlaInternalIPv6                 bool              `json:"enableUlaInternalIpv6"`
	NetworkProfile                        string            `json:"networkProfile"`
}

func checkName(kind, name string) error {
	if !namePattern.MatchString(name) {
		return errInvalid("Invalid value for field 'resource.name': '%s'. Must be a match of regex '(?:[a-z](?:[-a-z0-9]{0,61}[a-z0-9])?)' (%s)", name, kind)
	}
	return nil
}

func (s *Simulation) insertNetwork(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	var req networkRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if err := checkName("network", req.Name); err != nil {
		return 0, nil, err
	}
	n := &network{
		Name: req.Name, Description: req.Description, RoutingMode: defaultRoutingMode, MTU: defaultMTU,
		EnforcementOrder: "AFTER_CLASSIC_FIREWALL", AutoCreateSubnetworks: req.AutoCreateSubnetworks != nil && *req.AutoCreateSubnetworks,
	}
	switch {
	case req.IPv4Range != "":
		return 0, nil, errUnsupported("A legacy network (IPv4Range)")
	case req.EnableUlaInternalIPv6:
		return 0, nil, errUnsupported("IPv6 (enableUlaInternalIpv6)")
	case req.NetworkProfile != "":
		return 0, nil, errUnsupported("networkProfile")
	case req.MTU != 0 && (req.MTU < 1300 || req.MTU > 8896):
		return 0, nil, errInvalid("Invalid value for field 'resource.mtu': '%d'. Must be between 1300 and 8896.", req.MTU)
	}
	if req.MTU != 0 {
		n.MTU = req.MTU
	}
	if mode := req.RoutingConfig["routingMode"]; mode != "" {
		if mode != "REGIONAL" && mode != "GLOBAL" {
			return 0, nil, errInvalid("Invalid value for field 'resource.routingConfig.routingMode': '%s'.", mode)
		}
		n.RoutingMode = mode
	}
	if order := req.NetworkFirewallPolicyEnforcementOrder; order != "" {
		if order != "AFTER_CLASSIC_FIREWALL" && order != "BEFORE_CLASSIC_FIREWALL" {
			return 0, nil, errInvalid("Invalid value for field 'resource.networkFirewallPolicyEnforcementOrder': '%s'.", order)
		}
		n.EnforcementOrder = order
	}
	var out operationJSON
	err := s.store.Update(func(w *world) error {
		if w.network(n.Name) != nil {
			return errAlreadyExists("projects/" + s.cfg.project + "/global/networks/" + n.Name)
		}
		n.ID, n.CreatedAt = w.nextID(), s.now()
		w.Networks = append(w.Networks, n)
		if n.AutoCreateSubnetworks {
			s.addAutoSubnetworks(w, n)
		}
		op := s.newOperation(w, "global", "", "insert", s.link("global", "networks", n.Name), n.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

// addDefaultNetwork adds the auto mode network named default with the
// firewall rules that Google Cloud gives a new project.
func (s *Simulation) addDefaultNetwork(w *world) {
	n := &network{
		ID: w.nextID(), Name: "default", Description: "Default network for the project", AutoCreateSubnetworks: true,
		RoutingMode: defaultRoutingMode, MTU: defaultMTU, EnforcementOrder: "AFTER_CLASSIC_FIREWALL", CreatedAt: s.now(),
	}
	w.Networks = append(w.Networks, n)
	s.addAutoSubnetworks(w, n)
	all := []allowRule{{Protocol: "tcp", Ports: []string{"0-65535"}}, {Protocol: "udp", Ports: []string{"0-65535"}}, {Protocol: "icmp"}}
	for _, f := range []*firewall{
		{Name: "default-allow-internal", Description: "Allow internal traffic on the default network", SourceRanges: []string{"10.128.0.0/9"}, Allowed: all},
		{Name: "default-allow-ssh", Description: "Allow SSH from anywhere", SourceRanges: []string{"0.0.0.0/0"}, Allowed: []allowRule{{Protocol: "tcp", Ports: []string{"22"}}}},
		{Name: "default-allow-rdp", Description: "Allow RDP from anywhere", SourceRanges: []string{"0.0.0.0/0"}, Allowed: []allowRule{{Protocol: "tcp", Ports: []string{"3389"}}}},
		{Name: "default-allow-icmp", Description: "Allow ICMP from anywhere", SourceRanges: []string{"0.0.0.0/0"}, Allowed: []allowRule{{Protocol: "icmp"}}},
	} {
		f.ID, f.Network, f.Priority, f.Direction, f.CreatedAt = w.nextID(), n.Name, 65534, "INGRESS", s.now()
		w.Firewalls = append(w.Firewalls, f)
	}
}

// addAutoSubnetworks gives an auto mode network one subnetwork per region,
// named after the network, with /20 ranges from 10.128.0.0/9.
func (s *Simulation) addAutoSubnetworks(w *world, n *network) {
	for i, region := range s.catalog.regions {
		w.Subnetworks = append(w.Subnetworks, &subnetwork{
			ID: w.nextID(), Name: n.Name, Region: region, Network: n.Name, CreatedAt: s.now(),
			IPCIDRRange: fmt.Sprintf("10.%d.%d.0/20", 128+i/16, i%16*16),
		})
	}
}

func (s *Simulation) listNetworks(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	if err := noFilter(r); err != nil {
		return 0, nil, err
	}
	var out []networkJSON
	s.store.Read(func(w *world) {
		for _, n := range w.Networks {
			out = append(out, s.renderNetwork(w, n))
		}
	})
	return page(r, "compute#networkList", s.link("global", "networks"), out)
}

func (s *Simulation) getNetwork(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	var out *networkJSON
	s.store.Read(func(w *world) {
		if n := w.network(r.PathValue("name")); n != nil {
			rendered := s.renderNetwork(w, n)
			out = &rendered
		}
	})
	if out == nil {
		return 0, nil, errNotFound("projects/" + s.cfg.project + "/global/networks/" + r.PathValue("name"))
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) deleteNetwork(r *http.Request) (int, any, error) {
	if err := s.checkProject(r); err != nil {
		return 0, nil, err
	}
	name := r.PathValue("name")
	var out operationJSON
	err := s.store.Update(func(w *world) error {
		n := w.network(name)
		if n == nil {
			return errNotFound("projects/" + s.cfg.project + "/global/networks/" + name)
		}
		link := s.link("global", "networks", name)
		if f := find(w.Firewalls, func(f *firewall) bool { return f.Network == name }); f != nil {
			return errInUse("The network resource '%s' is already being used by '%s'", link, s.link("global", "firewalls", f.Name))
		}
		if i := find(w.Instances, func(i *instance) bool { return i.Network == name }); i != nil {
			return errInUse("The network resource '%s' is already being used by '%s'", link, s.link("zones", i.Zone, "instances", i.Name))
		}
		if sub := find(w.Subnetworks, func(sub *subnetwork) bool { return sub.Network == name }); sub != nil && !n.AutoCreateSubnetworks {
			return errInUse("The network resource '%s' is already being used by '%s'", link, s.link("regions", sub.Region, "subnetworks", sub.Name))
		}
		w.Subnetworks = slices.DeleteFunc(w.Subnetworks, func(sub *subnetwork) bool { return sub.Network == name })
		w.Networks = slices.DeleteFunc(w.Networks, func(other *network) bool { return other == n })
		op := s.newOperation(w, "global", "", "delete", link, n.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

type subnetworkJSON struct {
	Kind                    string `json:"kind"`
	ID                      string `json:"id"`
	CreationTimestamp       string `json:"creationTimestamp"`
	Name                    string `json:"name"`
	Description             string `json:"description,omitempty"`
	Network                 string `json:"network"`
	IPCIDRRange             string `json:"ipCidrRange"`
	GatewayAddress          string `json:"gatewayAddress"`
	Region                  string `json:"region"`
	SelfLink                string `json:"selfLink"`
	PrivateIPGoogleAccess   bool   `json:"privateIpGoogleAccess"`
	Fingerprint             string `json:"fingerprint"`
	Purpose                 string `json:"purpose"`
	StackType               string `json:"stackType"`
	PrivateIPv6GoogleAccess string `json:"privateIpv6GoogleAccess"`
}

func (s *Simulation) renderSubnetwork(sub *subnetwork) subnetworkJSON {
	prefix := netip.MustParsePrefix(sub.IPCIDRRange)
	return subnetworkJSON{
		Kind: "compute#subnetwork", ID: idString(sub.ID), CreationTimestamp: timestamp(sub.CreatedAt), Name: sub.Name,
		Description: sub.Description, Network: s.link("global", "networks", sub.Network), IPCIDRRange: sub.IPCIDRRange,
		GatewayAddress: prefix.Addr().Next().String(), Region: s.link("regions", sub.Region),
		SelfLink: s.link("regions", sub.Region, "subnetworks", sub.Name), PrivateIPGoogleAccess: sub.PrivateIPGoogleAccess,
		Fingerprint: fingerprint(fmt.Sprint(sub.ID, sub.PrivateIPGoogleAccess, sub.Description)), Purpose: "PRIVATE",
		StackType: "IPV4_ONLY", PrivateIPv6GoogleAccess: "DISABLE_GOOGLE_ACCESS",
	}
}

type subnetworkRequest struct {
	Name                  string            `json:"name"`
	Description           string            `json:"description"`
	Network               string            `json:"network"`
	IPCIDRRange           string            `json:"ipCidrRange"`
	PrivateIPGoogleAccess bool              `json:"privateIpGoogleAccess"`
	SecondaryIPRanges     []json.RawMessage `json:"secondaryIpRanges"`
	StackType             string            `json:"stackType"`
	Purpose               string            `json:"purpose"`
	Role                  string            `json:"role"`
	LogConfig             map[string]any    `json:"logConfig"`
	IPv6AccessType        string            `json:"ipv6AccessType"`
}

func (s *Simulation) insertSubnetwork(r *http.Request) (int, any, error) {
	region, err := s.region(r)
	if err != nil {
		return 0, nil, err
	}
	var req subnetworkRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if err := checkName("subnetwork", req.Name); err != nil {
		return 0, nil, err
	}
	prefix, err := netip.ParsePrefix(req.IPCIDRRange)
	switch {
	case err != nil || !prefix.Addr().Is4() || prefix.Masked() != prefix:
		return 0, nil, errInvalid("Invalid IPCidrRange: %s conflicts with the format of an IPv4 network.", req.IPCIDRRange)
	case prefix.Bits() < 8 || prefix.Bits() > 29:
		return 0, nil, errInvalid("Invalid IPCidrRange: %s. The prefix length must be between 8 and 29.", req.IPCIDRRange)
	case len(req.SecondaryIPRanges) > 0:
		return 0, nil, errUnsupported("secondaryIpRanges")
	case req.StackType != "" && req.StackType != "IPV4_ONLY":
		return 0, nil, errUnsupported("stackType %s", req.StackType)
	case req.Purpose != "" && req.Purpose != "PRIVATE":
		return 0, nil, errUnsupported("purpose %s", req.Purpose)
	case req.Role != "" || req.IPv6AccessType != "":
		return 0, nil, errUnsupported("role and ipv6AccessType")
	case req.LogConfig != nil && req.LogConfig["enable"] == true:
		return 0, nil, errUnsupported("Flow logs (logConfig.enable)")
	}
	netRef := parseRef(req.Network)
	if !s.belongs(netRef, "networks", "") {
		return 0, nil, errInvalid("Invalid value for field 'resource.network': '%s'.", req.Network)
	}
	var out operationJSON
	err = s.store.Update(func(w *world) error {
		n := w.network(netRef.name)
		if n == nil {
			return errNotFound("projects/" + s.cfg.project + "/global/networks/" + netRef.name)
		}
		if n.AutoCreateSubnetworks {
			return errUnsupported("Adding a subnetwork to an auto mode network")
		}
		if w.subnetwork(region, req.Name) != nil {
			return errAlreadyExists("projects/" + s.cfg.project + "/regions/" + region + "/subnetworks/" + req.Name)
		}
		for _, other := range w.Subnetworks {
			if other.Network == n.Name && netip.MustParsePrefix(other.IPCIDRRange).Overlaps(prefix) {
				return errInvalid("Invalid IPCidrRange: %s conflicts with existing subnetwork '%s' in region '%s'.", req.IPCIDRRange, other.Name, other.Region)
			}
		}
		sub := &subnetwork{
			ID: w.nextID(), Name: req.Name, Description: req.Description, Region: region, Network: n.Name,
			IPCIDRRange: prefix.String(), PrivateIPGoogleAccess: req.PrivateIPGoogleAccess, CreatedAt: s.now(),
		}
		w.Subnetworks = append(w.Subnetworks, sub)
		op := s.newOperation(w, "region", region, "insert", s.link("regions", region, "subnetworks", sub.Name), sub.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) listSubnetworks(r *http.Request) (int, any, error) {
	region, err := s.region(r)
	if err != nil {
		return 0, nil, err
	}
	if err := noFilter(r); err != nil {
		return 0, nil, err
	}
	var out []subnetworkJSON
	s.store.Read(func(w *world) {
		for _, sub := range w.Subnetworks {
			if sub.Region == region {
				out = append(out, s.renderSubnetwork(sub))
			}
		}
	})
	return page(r, "compute#subnetworkList", s.link("regions", region, "subnetworks"), out)
}

func (s *Simulation) getSubnetwork(r *http.Request) (int, any, error) {
	region, err := s.region(r)
	if err != nil {
		return 0, nil, err
	}
	var out *subnetworkJSON
	s.store.Read(func(w *world) {
		if sub := w.subnetwork(region, r.PathValue("name")); sub != nil {
			rendered := s.renderSubnetwork(sub)
			out = &rendered
		}
	})
	if out == nil {
		return 0, nil, errNotFound("projects/" + s.cfg.project + "/regions/" + region + "/subnetworks/" + r.PathValue("name"))
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) deleteSubnetwork(r *http.Request) (int, any, error) {
	region, err := s.region(r)
	if err != nil {
		return 0, nil, err
	}
	name := r.PathValue("name")
	var out operationJSON
	err = s.store.Update(func(w *world) error {
		sub := w.subnetwork(region, name)
		if sub == nil {
			return errNotFound("projects/" + s.cfg.project + "/regions/" + region + "/subnetworks/" + name)
		}
		link := s.link("regions", region, "subnetworks", name)
		if n := w.network(sub.Network); n != nil && n.AutoCreateSubnetworks {
			return errInvalid("The subnetwork '%s' of an auto mode network can't be deleted.", link)
		}
		if i := find(w.Instances, func(i *instance) bool { return i.Subnetwork == name && regionOf(i.Zone) == region }); i != nil {
			return errInUse("The subnetwork resource '%s' is already being used by '%s'", link, s.link("zones", i.Zone, "instances", i.Name))
		}
		w.Subnetworks = slices.DeleteFunc(w.Subnetworks, func(other *subnetwork) bool { return other == sub })
		op := s.newOperation(w, "region", region, "delete", link, sub.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

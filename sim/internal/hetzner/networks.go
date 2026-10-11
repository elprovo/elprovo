package hetzner

import (
	"net/http"
	"net/netip"
	"slices"
)

var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"),
}

var networkZones = []string{"eu-central", "us-east", "us-west", "ap-southeast"}

type subnetJSON struct {
	Type        string `json:"type"`
	IPRange     string `json:"ip_range"`
	NetworkZone string `json:"network_zone"`
	Gateway     string `json:"gateway"`
	VSwitchID   *int64 `json:"vswitch_id"`
}

type networkJSON struct {
	ID                    int64             `json:"id"`
	Name                  string            `json:"name"`
	IPRange               string            `json:"ip_range"`
	Subnets               []subnetJSON      `json:"subnets"`
	Routes                []struct{}        `json:"routes"`
	Servers               []int64           `json:"servers"`
	LoadBalancers         []int64           `json:"load_balancers"`
	Protection            protectionJSON    `json:"protection"`
	Labels                map[string]string `json:"labels"`
	Created               string            `json:"created"`
	ExposeRoutesToVSwitch bool              `json:"expose_routes_to_vswitch"`
}

func (n *network) render(p *project) networkJSON {
	out := networkJSON{
		ID: n.ID, Name: n.Name, IPRange: n.IPRange, Subnets: []subnetJSON{}, Routes: []struct{}{}, Servers: []int64{},
		LoadBalancers: []int64{}, Labels: labelsOrEmpty(n.Labels), Created: timestamp(n.Created),
	}
	for _, sn := range n.Subnets {
		out.Subnets = append(out.Subnets, subnetJSON{Type: sn.Type, IPRange: sn.IPRange, NetworkZone: sn.NetworkZone, Gateway: sn.Gateway})
	}
	for _, srv := range p.Servers {
		if slices.Contains(srv.Networks, n.ID) {
			out.Servers = append(out.Servers, srv.ID)
		}
	}
	return out
}

func (n *network) hasZone(zone string) bool {
	return slices.ContainsFunc(n.Subnets, func(sn subnet) bool { return sn.NetworkZone == zone })
}

func (n *network) hasSubnet(ipRange string) bool {
	return slices.ContainsFunc(n.Subnets, func(sn subnet) bool { return sn.IPRange == ipRange })
}

func checkNetworkRange(text string) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(text)
	if err != nil || prefix.Masked() != prefix || !prefix.Addr().Is4() || prefix.Bits() > 24 ||
		!slices.ContainsFunc(privateRanges, func(r netip.Prefix) bool { return r.Bits() <= prefix.Bits() && r.Contains(prefix.Addr()) }) {
		return netip.Prefix{}, errInvalidInput("ip_range", "%q must be an RFC 1918 block of size /24 or larger in CIDR notation", text)
	}
	return prefix, nil
}

type subnetRequest struct {
	Type        string `json:"type"`
	IPRange     string `json:"ip_range"`
	NetworkZone string `json:"network_zone"`
	VSwitchID   *int64 `json:"vswitch_id"`
}

// addSubnet checks a subnet request against the network and adds it. A
// subnet without ip_range takes the first free /24 of the network.
func (n *network) addSubnet(req subnetRequest) error {
	switch {
	case req.Type == "server" || req.Type == "vswitch" || req.VSwitchID != nil:
		return errUnsupported("subnets of type %s are not supported by the simulation; use cloud", req.Type)
	case req.Type != "cloud":
		return errInvalidInput("type", "must be cloud, server or vswitch")
	case !slices.Contains(networkZones, req.NetworkZone):
		return errInvalidInput("network_zone", "%q is not a network zone", req.NetworkZone)
	}
	network := netip.MustParsePrefix(n.IPRange)
	taken := func(p netip.Prefix) bool {
		return slices.ContainsFunc(n.Subnets, func(sn subnet) bool { return netip.MustParsePrefix(sn.IPRange).Overlaps(p) })
	}
	var prefix netip.Prefix
	if req.IPRange == "" {
		for addr := network.Addr(); network.Contains(addr); addr = nextBlock(addr) {
			if candidate := netip.PrefixFrom(addr, 24); !taken(candidate) {
				prefix = candidate
				break
			}
		}
		if !prefix.IsValid() {
			return errCode(http.StatusUnprocessableEntity, "no_subnet_available", "network %d has no free /24 left", n.ID)
		}
	} else {
		var err error
		prefix, err = netip.ParsePrefix(req.IPRange)
		if err != nil || prefix.Masked() != prefix || prefix.Bits() < network.Bits() || !network.Contains(prefix.Addr()) {
			return errInvalidInput("ip_range", "%q must be a block inside the network's ip_range %s", req.IPRange, n.IPRange)
		}
		if taken(prefix) {
			return errInvalidInput("ip_range", "%s overlaps another subnet", req.IPRange)
		}
	}
	n.Subnets = append(n.Subnets, subnet{Type: "cloud", IPRange: prefix.String(), NetworkZone: req.NetworkZone, Gateway: network.Addr().Next().String()})
	return nil
}

func nextBlock(addr netip.Addr) netip.Addr {
	b := addr.As4()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v += 256
	if v < 256 {
		return netip.Addr{}
	}
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

func (s *Simulation) createNetwork(r *http.Request) (int, any, error) {
	var req struct {
		Name                  string            `json:"name"`
		IPRange               string            `json:"ip_range"`
		Labels                map[string]string `json:"labels"`
		Subnets               []subnetRequest   `json:"subnets"`
		Routes                []struct{}        `json:"routes"`
		ExposeRoutesToVSwitch bool              `json:"expose_routes_to_vswitch"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	switch {
	case req.Name == "":
		return 0, nil, errInvalidInput("name", "is required")
	case len(req.Routes) > 0:
		return 0, nil, errUnsupported("routes are not supported by the simulation")
	case req.ExposeRoutesToVSwitch:
		return 0, nil, errUnsupported("vSwitches are not supported by the simulation")
	}
	prefix, err := checkNetworkRange(req.IPRange)
	if err != nil {
		return 0, nil, err
	}
	if err := checkLabels(req.Labels); err != nil {
		return 0, nil, err
	}
	var out networkJSON
	err = s.store.Update(func(p *project) error {
		if slices.ContainsFunc(p.Networks, func(n *network) bool { return n.Name == req.Name }) {
			return errUniqueness("name", "network name is already used")
		}
		n := &network{ID: p.nextID(), Name: req.Name, IPRange: prefix.String(), Labels: req.Labels, Created: s.now().UTC()}
		for _, sn := range req.Subnets {
			if err := n.addSubnet(sn); err != nil {
				return err
			}
		}
		p.Networks = append(p.Networks, n)
		out = n.render(p)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"network": out}, nil
}

func (s *Simulation) listNetworks(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	keep, err := selectorFilter(q)
	if err != nil {
		return 0, nil, err
	}
	name := q.Get("name")
	var out []networkJSON
	s.store.Read(func(p *project) {
		for _, n := range p.Networks {
			if (name == "" || n.Name == name) && keep(n.Labels) {
				out = append(out, n.render(p))
			}
		}
	})
	return listResponse(r, "networks", out, sortFields[networkJSON]{
		"id":      byID(func(n networkJSON) int64 { return n.ID }),
		"name":    byString(func(n networkJSON) string { return n.Name }),
		"created": byString(func(n networkJSON) string { return n.Created }),
	})
}

func (s *Simulation) getNetwork(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "network")
	if err != nil {
		return 0, nil, err
	}
	var out *networkJSON
	s.store.Read(func(p *project) {
		if n := p.network(id); n != nil {
			j := n.render(p)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound("network")
	}
	return http.StatusOK, map[string]any{"network": out}, nil
}

func (s *Simulation) updateNetwork(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "network")
	if err != nil {
		return 0, nil, err
	}
	var req struct {
		Name                  string             `json:"name"`
		Labels                *map[string]string `json:"labels"`
		ExposeRoutesToVSwitch *bool              `json:"expose_routes_to_vswitch"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.ExposeRoutesToVSwitch != nil && *req.ExposeRoutesToVSwitch {
		return 0, nil, errUnsupported("vSwitches are not supported by the simulation")
	}
	if req.Labels != nil {
		if err := checkLabels(*req.Labels); err != nil {
			return 0, nil, err
		}
	}
	var out networkJSON
	err = s.store.Update(func(p *project) error {
		n := p.network(id)
		if n == nil {
			return errNotFound("network")
		}
		if req.Name != "" && req.Name != n.Name {
			if slices.ContainsFunc(p.Networks, func(o *network) bool { return o.Name == req.Name }) {
				return errUniqueness("name", "network name is already used")
			}
			n.Name = req.Name
		}
		if req.Labels != nil {
			n.Labels = *req.Labels
		}
		out = n.render(p)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"network": out}, nil
}

// deleteNetwork deletes the network and detaches its servers, as Hetzner
// does.
func (s *Simulation) deleteNetwork(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "network")
	if err != nil {
		return 0, nil, err
	}
	err = s.store.Update(func(p *project) error {
		if p.network(id) == nil {
			return errNotFound("network")
		}
		for _, srv := range p.Servers {
			srv.Networks = slices.DeleteFunc(srv.Networks, func(n int64) bool { return n == id })
		}
		p.Networks = slices.DeleteFunc(p.Networks, func(n *network) bool { return n.ID == id })
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.firewalls.kick()
	return http.StatusNoContent, nil, nil
}

func (s *Simulation) postNetworkAction(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "network")
	if err != nil {
		return 0, nil, err
	}
	name := r.PathValue("action")
	if name != "add_subnet" && name != "delete_subnet" {
		return 0, nil, errUnsupported("network action %q is not supported by the simulation", name)
	}
	var req subnetRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out actionJSON
	err = s.store.Update(func(p *project) error {
		n := p.network(id)
		if n == nil {
			return errNotFound("network")
		}
		if name == "add_subnet" {
			if err := n.addSubnet(req); err != nil {
				return err
			}
		} else {
			if !n.hasSubnet(req.IPRange) {
				return errNotFound("subnet")
			}
			if slices.ContainsFunc(p.Servers, func(srv *server) bool { return slices.Contains(srv.Networks, n.ID) }) {
				return errService("the simulation deletes a subnet only from a network without attached servers")
			}
			n.Subnets = slices.DeleteFunc(n.Subnets, func(sn subnet) bool { return sn.IPRange == req.IPRange })
		}
		out = s.succeeded(p, name, resourceRef{ID: id, Type: "network"}).render()
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"action": out}, nil
}

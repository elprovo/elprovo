package hetzner

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
)

type dnsPtrJSON struct {
	IP     string `json:"ip"`
	DNSPtr string `json:"dns_ptr"`
}

type primaryIPJSON struct {
	ID           int64             `json:"id"`
	Name         string            `json:"name"`
	Labels       map[string]string `json:"labels"`
	Created      string            `json:"created"`
	Blocked      bool              `json:"blocked"`
	Location     locationJSON      `json:"location"`
	IP           string            `json:"ip"`
	DNSPtr       []dnsPtrJSON      `json:"dns_ptr"`
	Protection   protectionJSON    `json:"protection"`
	Type         string            `json:"type"`
	AutoDelete   bool              `json:"auto_delete"`
	AssigneeType string            `json:"assignee_type"`
	AssigneeID   *int64            `json:"assignee_id"`
}

func (ip *primaryIP) render() primaryIPJSON {
	loc, _ := location(ip.Location)
	out := primaryIPJSON{
		ID: ip.ID, Name: ip.Name, Labels: labelsOrEmpty(ip.Labels), Created: timestamp(ip.Created), Location: loc, IP: ip.IP,
		DNSPtr: []dnsPtrJSON{}, Type: "ipv4", AutoDelete: ip.AutoDelete, AssigneeType: "unassigned",
	}
	if ip.AssigneeID != 0 {
		assignee := ip.AssigneeID
		out.AssigneeType, out.AssigneeID = "server", &assignee
	}
	return out
}

// allocateAddress returns a free address on the public network for a new
// primary IP. Docker gives containers addresses from the bottom of the
// subnet and other machines take theirs from the top, so primary IPs come
// from the middle upward.
func (s *Simulation) allocateAddress(p *project) (string, error) {
	prefix := s.networks.Public.Prefix.Masked()
	if !prefix.IsValid() || prefix.Bits() > 29 {
		return "", errService("the public network %s is too small for primary IPs", prefix)
	}
	taken := map[netip.Addr]bool{s.networks.Public.Gateway: true}
	for _, ip := range p.PrimaryIPs {
		if addr, err := netip.ParseAddr(ip.IP); err == nil {
			taken[addr] = true
		}
	}
	start := prefix.Addr().As4()
	base := uint32(start[0])<<24 | uint32(start[1])<<16 | uint32(start[2])<<8 | uint32(start[3])
	hostBits := 32 - prefix.Bits()
	for i := uint32(1) << (hostBits - 1); i < uint32(1)<<hostBits-1; i++ {
		v := base + i
		addr := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		if !taken[addr] {
			return addr.String(), nil
		}
	}
	return "", errCode(http.StatusPreconditionFailed, "resource_unavailable", "no free IPv4 address is left")
}

func (s *Simulation) listPrimaryIPs(r *http.Request) (int, any, error) {
	q := r.URL.Query()
	keep, err := selectorFilter(q)
	if err != nil {
		return 0, nil, err
	}
	name, address := q.Get("name"), q.Get("ip")
	var out []primaryIPJSON
	s.store.Read(func(p *project) {
		for _, ip := range p.PrimaryIPs {
			if (name == "" || ip.Name == name) && (address == "" || ip.IP == address) && keep(ip.Labels) {
				out = append(out, ip.render())
			}
		}
	})
	return listResponse(r, "primary_ips", out, sortFields[primaryIPJSON]{
		"id":      byID(func(ip primaryIPJSON) int64 { return ip.ID }),
		"created": byString(func(ip primaryIPJSON) string { return ip.Created }),
	})
}

type createPrimaryIPRequest struct {
	Name         string            `json:"name"`
	Labels       map[string]string `json:"labels"`
	Type         string            `json:"type"`
	Location     json.RawMessage   `json:"location"`
	Datacenter   json.RawMessage   `json:"datacenter"`
	AssigneeType string            `json:"assignee_type"`
	AssigneeID   *int64            `json:"assignee_id"`
	AutoDelete   bool              `json:"auto_delete"`
}

// createPrimaryIP creates an unassigned IPv4 primary IP in a location. A
// server can take it when it is created.
func (s *Simulation) createPrimaryIP(r *http.Request) (int, any, error) {
	var req createPrimaryIPRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	switch {
	case req.Name == "":
		return 0, nil, errInvalidInput("name", "is required")
	case req.Type == "ipv6":
		return 0, nil, errUnsupported("IPv6 primary IPs are not supported by the simulation")
	case req.Type != "ipv4":
		return 0, nil, errInvalidInput("type", "must be ipv4 or ipv6")
	case req.AssigneeID != nil:
		return 0, nil, errUnsupported("the simulation fixes a server's primary IPv4 when it creates the server; create the primary IP with a location and name it in public_net.ipv4")
	case len(req.Datacenter) > 0:
		return 0, nil, errInvalidInput("datacenter", "datacenters were removed from the API; use location")
	case len(req.Location) == 0:
		return 0, nil, errInvalidInput("location", "is required when the primary IP is not assigned")
	}
	locID, locName := idOrName(req.Location)
	loc, ok := location(firstNonEmpty(locName, strconv.FormatInt(locID, 10)))
	if !ok {
		return 0, nil, errInvalidInput("location", "location %s not found", string(req.Location))
	}
	if err := checkLabels(req.Labels); err != nil {
		return 0, nil, err
	}
	var out primaryIPJSON
	err := s.store.Update(func(p *project) error {
		if slices.ContainsFunc(p.PrimaryIPs, func(ip *primaryIP) bool { return ip.Name == req.Name }) {
			return errUniqueness("name", "primary IP name is already used")
		}
		address, err := s.allocateAddress(p)
		if err != nil {
			return err
		}
		ip := &primaryIP{ID: p.nextID(), Name: req.Name, IP: address, Location: loc.Name, AutoDelete: req.AutoDelete, Labels: req.Labels, Created: s.now().UTC()}
		p.PrimaryIPs = append(p.PrimaryIPs, ip)
		out = ip.render()
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"primary_ip": out, "action": nil}, nil
}

func (s *Simulation) getPrimaryIP(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "primary IP")
	if err != nil {
		return 0, nil, err
	}
	var out *primaryIPJSON
	s.store.Read(func(p *project) {
		if ip := p.primaryIP(id); ip != nil {
			j := ip.render()
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound("primary IP")
	}
	return http.StatusOK, map[string]any{"primary_ip": out}, nil
}

func (s *Simulation) updatePrimaryIP(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "primary IP")
	if err != nil {
		return 0, nil, err
	}
	var req struct {
		Name       string             `json:"name"`
		Labels     *map[string]string `json:"labels"`
		AutoDelete *bool              `json:"auto_delete"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Labels != nil {
		if err := checkLabels(*req.Labels); err != nil {
			return 0, nil, err
		}
	}
	var out primaryIPJSON
	err = s.store.Update(func(p *project) error {
		ip := p.primaryIP(id)
		if ip == nil {
			return errNotFound("primary IP")
		}
		if req.Name != "" && req.Name != ip.Name {
			if slices.ContainsFunc(p.PrimaryIPs, func(o *primaryIP) bool { return o.Name == req.Name }) {
				return errUniqueness("name", "primary IP name is already used")
			}
			ip.Name = req.Name
		}
		if req.Labels != nil {
			ip.Labels = *req.Labels
		}
		if req.AutoDelete != nil {
			ip.AutoDelete = *req.AutoDelete
		}
		out = ip.render()
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"primary_ip": out}, nil
}

// deletePrimaryIP deletes an unassigned primary IP. Since 1 May 2026,
// Hetzner refuses to delete one that is still assigned.
func (s *Simulation) deletePrimaryIP(r *http.Request) (int, any, error) {
	id, err := pathID(r, "id", "primary IP")
	if err != nil {
		return 0, nil, err
	}
	err = s.store.Update(func(p *project) error {
		ip := p.primaryIP(id)
		if ip == nil {
			return errNotFound("primary IP")
		}
		if ip.AssigneeID != 0 {
			return errCode(http.StatusUnprocessableEntity, "must_be_unassigned", "primary IP %d is still assigned to server %d", id, ip.AssigneeID)
		}
		p.PrimaryIPs = slices.DeleteFunc(p.PrimaryIPs, func(o *primaryIP) bool { return o.ID == id })
		return nil
	})
	return http.StatusNoContent, nil, err
}

// postPrimaryIPAction refuses assign and unassign: a machine keeps the
// public address it was created with.
func (s *Simulation) postPrimaryIPAction(r *http.Request) (int, any, error) {
	if _, err := s.existingResource("primary_ip", r.PathValue("id")); err != nil {
		return 0, nil, err
	}
	return 0, nil, errUnsupported("primary IP action %q is not supported by the simulation: a server keeps the primary IP it was created with until it is deleted", r.PathValue("action"))
}

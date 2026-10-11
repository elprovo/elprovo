package digitalocean

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
)

// reservedIP is a public address that the account keeps and points at one
// droplet at a time. Its machine answers at the address as an extra one on
// its public interface.
type reservedIP struct {
	IP        string `json:"ip"`
	Region    string `json:"region"`
	DropletID int64  `json:"droplet_id,omitempty"`
	Locked    bool   `json:"locked"`
}

type reservedIPJSON struct {
	IP        string       `json:"ip"`
	Region    regionJSON   `json:"region"`
	Droplet   *dropletJSON `json:"droplet"`
	Locked    bool         `json:"locked"`
	ProjectID string       `json:"project_id"`
}

func (s *Simulation) renderReservedIP(a *account, r *reservedIP) reservedIPJSON {
	out := reservedIPJSON{IP: r.IP, Region: s.catalog.region(r.Region), Locked: r.Locked, ProjectID: a.UUID}
	if d := a.droplet(r.DropletID); d != nil {
		rendered := s.render(d)
		out.Droplet = &rendered
	}
	return out
}

func (a *account) reservedIP(ip string) *reservedIP {
	for _, r := range a.ReservedIPs {
		if r.IP == ip {
			return r
		}
	}
	return nil
}

// reservedIPsOf returns the reserved IPs that point at the droplet.
func (a *account) reservedIPsOf(dropletID int64) []string {
	var ips []string
	for _, r := range a.ReservedIPs {
		if r.DropletID == dropletID {
			ips = append(ips, r.IP)
		}
	}
	return ips
}

// ipResourceID is the action resource id of a reserved IP: its address as a
// number, as DigitalOcean reports it.
func ipResourceID(ip string) int64 {
	b := netip.MustParseAddr(ip).As4()
	return int64(b[0])<<24 | int64(b[1])<<16 | int64(b[2])<<8 | int64(b[3])
}

// allocateReservedIP returns a free address on the public network. Docker
// gives containers addresses from the bottom of the subnet and machines get
// theirs from the top, so reserved IPs come from the middle upward.
func (s *Simulation) allocateReservedIP(ctx context.Context) (string, error) {
	networks, err := s.machines.Networks(ctx)
	if err != nil {
		return "", err
	}
	prefix := networks.Public.Prefix.Masked()
	start := prefix.Addr().As4()
	n := uint32(start[0])<<24 | uint32(start[1])<<16 | uint32(start[2])<<8 | uint32(start[3])
	hostBits := 32 - prefix.Bits()
	if hostBits < 3 {
		return "", errUnprocessable("the public network is too small for reserved IPs")
	}
	taken := map[string]bool{}
	s.store.Read(func(a *account) {
		for _, r := range a.ReservedIPs {
			taken[r.IP] = true
		}
		for _, d := range a.Droplets {
			taken[d.PublicIP] = true
		}
	})
	for i := uint32(1) << (hostBits - 1); i < uint32(1)<<hostBits-1; i++ {
		v := n + i
		ip := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}).String()
		if !taken[ip] {
			return ip, nil
		}
	}
	return "", errUnprocessable("no free reserved IP is left")
}

func (s *Simulation) listReservedIPs(r *http.Request) (int, any, error) {
	var out []reservedIPJSON
	s.store.Read(func(a *account) {
		for _, ip := range a.ReservedIPs {
			out = append(out, s.renderReservedIP(a, ip))
		}
	})
	return listResponse(r, "reserved_ips", out)
}

type createReservedIPRequest struct {
	Region    string `json:"region"`
	DropletID int64  `json:"droplet_id"`
	ProjectID string `json:"project_id"`
}

// createReservedIP reserves an address in a region, or for a droplet, which
// it then points at the droplet with an assign action.
func (s *Simulation) createReservedIP(r *http.Request) (int, any, error) {
	var req createReservedIPRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if (req.Region == "") == (req.DropletID == 0) {
		return 0, nil, errUnprocessable("Specify either region or droplet_id.")
	}
	ip, err := s.allocateReservedIP(r.Context())
	if err != nil {
		return 0, nil, err
	}
	var out reservedIPJSON
	var links map[string]any
	var run func(context.Context)
	err = s.store.Update(func(a *account) error {
		region := req.Region
		if req.DropletID != 0 {
			d := a.droplet(req.DropletID)
			if d == nil {
				return errUnprocessable("Droplet not found.")
			}
			region = d.Region
		} else if !slices.ContainsFunc(s.catalog.regions, func(slug string) bool { return slug == region }) {
			return errUnprocessable("Region %q is not available.", region)
		}
		reserved := &reservedIP{IP: ip, Region: region}
		a.ReservedIPs = append(a.ReservedIPs, reserved)
		links = map[string]any{}
		if req.DropletID != 0 {
			act, start, err := s.startReservedIPAction(a, reserved, reservedIPActionRequest{Type: "assign", DropletID: req.DropletID})
			if err != nil {
				return err
			}
			run = start
			links["droplets"] = []map[string]any{{"id": req.DropletID, "rel": "droplet", "href": fmt.Sprintf("%s/v2/droplets/%d", baseURL(r), req.DropletID)}}
			links["actions"] = []map[string]any{{"id": act.ID, "rel": act.Type, "href": fmt.Sprintf("%s/v2/actions/%d", baseURL(r), act.ID)}}
		}
		out = s.renderReservedIP(a, reserved)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	if run != nil {
		s.background(run)
	}
	return http.StatusAccepted, map[string]any{"reserved_ip": out, "links": links}, nil
}

func (s *Simulation) getReservedIP(r *http.Request) (int, any, error) {
	var out *reservedIPJSON
	s.store.Read(func(a *account) {
		if ip := a.reservedIP(r.PathValue("ip")); ip != nil {
			rendered := s.renderReservedIP(a, ip)
			out = &rendered
		}
	})
	if out == nil {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"reserved_ip": out}, nil
}

// deleteReservedIP releases the address. An address that still points at a
// droplet leaves that droplet first.
func (s *Simulation) deleteReservedIP(r *http.Request) (int, any, error) {
	var machine string
	var remaining []string
	err := s.store.Update(func(a *account) error {
		ip := a.reservedIP(r.PathValue("ip"))
		if ip == nil {
			return errNotFound()
		}
		if ip.Locked {
			return errUnprocessable("The reserved IP has a pending event.")
		}
		if d := a.droplet(ip.DropletID); d != nil {
			machine = d.Machine
			remaining = slices.DeleteFunc(a.reservedIPsOf(d.ID), func(v string) bool { return v == ip.IP })
		}
		a.ReservedIPs = slices.DeleteFunc(a.ReservedIPs, func(v *reservedIP) bool { return v == ip })
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	if machine != "" {
		if err := s.machines.SetAddresses(r.Context(), machine, remaining); err != nil {
			s.log.Error("remove a deleted reserved IP from its machine", "error", err)
		}
	}
	return http.StatusNoContent, nil, nil
}

type reservedIPActionRequest struct {
	Type      string `json:"type"`
	DropletID int64  `json:"droplet_id"`
}

func (s *Simulation) postReservedIPAction(r *http.Request) (int, any, error) {
	var req reservedIPActionRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out actionJSON
	var run func(context.Context)
	err := s.store.Update(func(a *account) error {
		ip := a.reservedIP(r.PathValue("ip"))
		if ip == nil {
			return errNotFound()
		}
		act, start, err := s.startReservedIPAction(a, ip, req)
		if err != nil {
			return err
		}
		out, run = s.renderAction(act), start
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	s.background(run)
	return http.StatusCreated, map[string]any{"action": out}, nil
}

// startReservedIPAction records an assign or unassign action and returns the
// work that moves the address between machines.
func (s *Simulation) startReservedIPAction(a *account, ip *reservedIP, req reservedIPActionRequest) (*action, func(context.Context), error) {
	if ip.Locked {
		return nil, nil, errUnprocessable("The reserved IP has a pending event.")
	}
	var from, to *droplet
	from = a.droplet(ip.DropletID)
	typ := "unassign_ip"
	switch req.Type {
	case "assign":
		to = a.droplet(req.DropletID)
		if to == nil {
			return nil, nil, errUnprocessable("Droplet not found.")
		}
		if to.Region != ip.Region {
			return nil, nil, errUnprocessable("The reserved IP and the Droplet must be in the same region.")
		}
		if to.Machine == "" {
			return nil, nil, errUnprocessable("Droplet already has a pending event.")
		}
		typ = "assign_ip"
	case "unassign":
		if from == nil {
			return nil, nil, errUnprocessable("The reserved IP is not assigned to a Droplet.")
		}
	default:
		return nil, nil, errUnprocessable("Action type %q is not supported for reserved IPs.", req.Type)
	}
	act := &action{
		ID: a.nextID(), Status: "in-progress", Type: typ, StartedAt: s.now().UTC(),
		ResourceID: ipResourceID(ip.IP), ResourceType: "reserved_ip", Region: ip.Region,
	}
	a.Actions = append(a.Actions, act)
	ip.Locked = true
	pushes := map[string][]string{}
	if from != nil {
		pushes[from.Machine] = slices.DeleteFunc(a.reservedIPsOf(from.ID), func(v string) bool { return v == ip.IP })
	}
	ip.DropletID = 0
	if to != nil {
		ip.DropletID = to.ID
		pushes[to.Machine] = a.reservedIPsOf(to.ID)
	}
	actionID, address := act.ID, ip.IP
	return act, func(ctx context.Context) { s.moveReservedIP(ctx, actionID, address, pushes) }, nil
}

// moveReservedIP gives each machine its new list of reserved IPs and
// finishes the action.
func (s *Simulation) moveReservedIP(ctx context.Context, actionID int64, address string, pushes map[string][]string) {
	status := "completed"
	for machine, addrs := range pushes {
		if machine == "" {
			continue
		}
		if err := s.machines.SetAddresses(ctx, machine, addrs); err != nil {
			s.log.Error("move reserved IP", "ip", address, "machine", machine, "error", err)
			status = "errored"
		}
	}
	err := s.store.Update(func(a *account) error {
		if act := a.action(actionID); act != nil {
			now := s.now().UTC()
			act.Status, act.CompletedAt = status, &now
		}
		if ip := a.reservedIP(address); ip != nil {
			ip.Locked = false
		}
		return nil
	})
	if err != nil {
		s.log.Error("record reserved IP action", "action", actionID, "error", err)
	}
}

func (s *Simulation) listReservedIPActions(r *http.Request) (int, any, error) {
	ip := r.PathValue("ip")
	exists := false
	s.store.Read(func(a *account) { exists = a.reservedIP(ip) != nil })
	if !exists {
		return 0, nil, errNotFound()
	}
	id := ipResourceID(ip)
	return listResponse(r, "actions", s.actionsOf(func(act *action) bool { return act.ResourceType == "reserved_ip" && act.ResourceID == id }))
}

func (s *Simulation) getReservedIPAction(r *http.Request) (int, any, error) {
	if _, err := netip.ParseAddr(r.PathValue("ip")); err != nil {
		return 0, nil, errNotFound()
	}
	id := ipResourceID(r.PathValue("ip"))
	actionID, _ := strconv.ParseInt(r.PathValue("action"), 10, 64)
	found := s.actionsOf(func(act *action) bool {
		return act.ID == actionID && act.ResourceType == "reserved_ip" && act.ResourceID == id
	})
	if len(found) == 0 {
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"action": found[0]}, nil
}

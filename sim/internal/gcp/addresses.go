package gcp

import (
	"context"
	"net/http"
	"net/netip"
	"slices"
	"time"
)

// address is a static external IPv4 address that the project keeps. An
// instance that names it as its access config's natIP answers at it: its
// machine holds it as an extra address on its public interface.
type address struct {
	ID          uint64            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Region      string            `json:"region"`
	Address     string            `json:"address"`
	NetworkTier string            `json:"network_tier"`
	Labels      map[string]string `json:"labels"`
	CreatedAt   time.Time         `json:"created_at"`
}

type addressJSON struct {
	Kind              string            `json:"kind"`
	ID                string            `json:"id"`
	CreationTimestamp string            `json:"creationTimestamp"`
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	Address           string            `json:"address"`
	Status            string            `json:"status"`
	Region            string            `json:"region"`
	SelfLink          string            `json:"selfLink"`
	Users             []string          `json:"users,omitempty"`
	NetworkTier       string            `json:"networkTier"`
	Labels            map[string]string `json:"labels,omitempty"`
	LabelFingerprint  string            `json:"labelFingerprint"`
	AddressType       string            `json:"addressType"`
}

func (s *Simulation) renderAddress(w *world, a *address) addressJSON {
	out := addressJSON{
		Kind: "compute#address", ID: idString(a.ID), CreationTimestamp: timestamp(a.CreatedAt), Name: a.Name,
		Description: a.Description, Address: a.Address, Status: "RESERVED", Region: s.link("regions", a.Region),
		SelfLink: s.link("regions", a.Region, "addresses", a.Name), NetworkTier: a.NetworkTier, Labels: a.Labels,
		LabelFingerprint: fingerprint(jsonKey(a.Labels)), AddressType: "EXTERNAL",
	}
	if inst := w.addressUser(a); inst != nil {
		out.Status, out.Users = "IN_USE", []string{s.link("zones", inst.Zone, "instances", inst.Name)}
	}
	return out
}

func (w *world) addressUser(a *address) *instance {
	return find(w.Instances, func(i *instance) bool { return i.StaticIP == a.Address && regionOf(i.Zone) == a.Region })
}

type addressRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Address     string            `json:"address"`
	AddressType string            `json:"addressType"`
	NetworkTier string            `json:"networkTier"`
	IPVersion   string            `json:"ipVersion"`
	Purpose     string            `json:"purpose"`
	Subnetwork  string            `json:"subnetwork"`
	Network     string            `json:"network"`
	Labels      map[string]string `json:"labels"`
}

func (s *Simulation) insertAddress(r *http.Request) (int, any, error) {
	region, err := s.region(r)
	if err != nil {
		return 0, nil, err
	}
	var req addressRequest
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if err := checkName("address", req.Name); err != nil {
		return 0, nil, err
	}
	switch {
	case req.AddressType != "" && req.AddressType != "EXTERNAL":
		return 0, nil, errUnsupported("addressType %s", req.AddressType)
	case req.IPVersion != "" && req.IPVersion != "IPV4":
		return 0, nil, errUnsupported("ipVersion %s", req.IPVersion)
	case req.Purpose != "" || req.Subnetwork != "" || req.Network != "":
		return 0, nil, errUnsupported("purpose, network and subnetwork on an external address")
	case req.Address != "":
		return 0, nil, errUnsupported("Reserving a given address (address)")
	case req.NetworkTier != "" && req.NetworkTier != "PREMIUM" && req.NetworkTier != "STANDARD":
		return 0, nil, errInvalid("Invalid value for field 'resource.networkTier': '%s'.", req.NetworkTier)
	}
	ip, err := s.allocateAddress(r.Context())
	if err != nil {
		return 0, nil, err
	}
	a := &address{Name: req.Name, Description: req.Description, Region: region, Address: ip, NetworkTier: req.NetworkTier, Labels: req.Labels}
	if a.NetworkTier == "" {
		a.NetworkTier = "PREMIUM"
	}
	var out operationJSON
	err = s.store.Update(func(w *world) error {
		if w.address(region, a.Name) != nil {
			return errAlreadyExists("projects/" + s.cfg.project + "/regions/" + region + "/addresses/" + a.Name)
		}
		if addressTaken(w, ip) {
			return &apiError{http.StatusServiceUnavailable, "resourceNotReady", "The address pool is busy; try again."}
		}
		a.ID, a.CreatedAt = w.nextID(), s.now()
		w.Addresses = append(w.Addresses, a)
		op := s.newOperation(w, "region", region, "insert", s.link("regions", region, "addresses", a.Name), a.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

func addressTaken(w *world, ip string) bool {
	return slices.ContainsFunc(w.Addresses, func(a *address) bool { return a.Address == ip }) ||
		slices.ContainsFunc(w.Instances, func(i *instance) bool { return i.PublicIP == ip })
}

// allocateAddress returns a free address on the public network. Docker gives
// containers addresses from the bottom of the subnet and machines get theirs
// from the top, so static addresses come from the middle upward.
func (s *Simulation) allocateAddress(ctx context.Context) (string, error) {
	networks, err := s.machines.Networks(ctx)
	if err != nil {
		return "", err
	}
	prefix := networks.Public.Prefix.Masked()
	hostBits := 32 - prefix.Bits()
	if hostBits < 3 {
		return "", errInvalid("The public network is too small for static addresses.")
	}
	start := prefix.Addr().As4()
	base := uint32(start[0])<<24 | uint32(start[1])<<16 | uint32(start[2])<<8 | uint32(start[3])
	var free string
	s.store.Read(func(w *world) {
		for i := uint32(1) << (hostBits - 1); i < uint32(1)<<hostBits-1; i++ {
			v := base + i
			ip := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}).String()
			if !addressTaken(w, ip) {
				free = ip
				return
			}
		}
	})
	if free == "" {
		return "", &apiError{http.StatusForbidden, "quotaExceeded", "Quota 'STATIC_ADDRESSES' exceeded."}
	}
	return free, nil
}

func (s *Simulation) listAddresses(r *http.Request) (int, any, error) {
	region, err := s.region(r)
	if err != nil {
		return 0, nil, err
	}
	if err := noFilter(r); err != nil {
		return 0, nil, err
	}
	var out []addressJSON
	s.store.Read(func(w *world) {
		for _, a := range w.Addresses {
			if a.Region == region {
				out = append(out, s.renderAddress(w, a))
			}
		}
	})
	return page(r, "compute#addressList", s.link("regions", region, "addresses"), out)
}

func (s *Simulation) getAddress(r *http.Request) (int, any, error) {
	region, err := s.region(r)
	if err != nil {
		return 0, nil, err
	}
	var out *addressJSON
	s.store.Read(func(w *world) {
		if a := w.address(region, r.PathValue("name")); a != nil {
			rendered := s.renderAddress(w, a)
			out = &rendered
		}
	})
	if out == nil {
		return 0, nil, errNotFound("projects/" + s.cfg.project + "/regions/" + region + "/addresses/" + r.PathValue("name"))
	}
	return http.StatusOK, out, nil
}

func (s *Simulation) deleteAddress(r *http.Request) (int, any, error) {
	region, err := s.region(r)
	if err != nil {
		return 0, nil, err
	}
	name := r.PathValue("name")
	var out operationJSON
	err = s.store.Update(func(w *world) error {
		a := w.address(region, name)
		if a == nil {
			return errNotFound("projects/" + s.cfg.project + "/regions/" + region + "/addresses/" + name)
		}
		link := s.link("regions", region, "addresses", name)
		if inst := w.addressUser(a); inst != nil {
			return errInUse("The address resource '%s' is already being used by '%s'", link, s.link("zones", inst.Zone, "instances", inst.Name))
		}
		w.Addresses = slices.DeleteFunc(w.Addresses, func(other *address) bool { return other == a })
		op := s.newOperation(w, "region", region, "delete", link, a.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

// setAddressLabels replaces the address's labels when the request's
// fingerprint matches the current labels.
func (s *Simulation) setAddressLabels(r *http.Request) (int, any, error) {
	region, err := s.region(r)
	if err != nil {
		return 0, nil, err
	}
	var req struct {
		Labels           map[string]string `json:"labels"`
		LabelFingerprint string            `json:"labelFingerprint"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if err := checkLabels(req.Labels); err != nil {
		return 0, nil, err
	}
	name := r.PathValue("name")
	var out operationJSON
	err = s.store.Update(func(w *world) error {
		a := w.address(region, name)
		if a == nil {
			return errNotFound("projects/" + s.cfg.project + "/regions/" + region + "/addresses/" + name)
		}
		if req.LabelFingerprint != fingerprint(jsonKey(a.Labels)) {
			return &apiError{http.StatusPreconditionFailed, "conditionNotMet", "Labels fingerprint either invalid or resource labels have changed"}
		}
		a.Labels = req.Labels
		op := s.newOperation(w, "region", region, "setLabels", s.link("regions", region, "addresses", name), a.ID)
		s.done(op, nil)
		out = s.renderOperation(w, op)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, out, nil
}

package vultr

import (
	"context"
	"net/http"
	"net/netip"
	"slices"
)

const reservedIPNotFound = "Invalid reserved-ip ID."

type reservedIPJSON struct {
	ID         string `json:"id"`
	Region     string `json:"region"`
	IPType     string `json:"ip_type"`
	Subnet     string `json:"subnet"`
	SubnetSize int    `json:"subnet_size"`
	Label      string `json:"label"`
	InstanceID string `json:"instance_id"`
}

func renderReservedIP(r *reservedIP) reservedIPJSON {
	return reservedIPJSON{ID: r.ID, Region: r.Region, IPType: "v4", Subnet: r.Address, SubnetSize: 32, Label: r.Label, InstanceID: r.InstanceID}
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
	hostBits := 32 - prefix.Bits()
	if hostBits < 3 {
		return "", errBadRequest("The public network is too small for reserved IPs.")
	}
	start := prefix.Addr().As4()
	base := uint32(start[0])<<24 | uint32(start[1])<<16 | uint32(start[2])<<8 | uint32(start[3])
	taken := map[string]bool{}
	s.store.Read(func(a *account) {
		for _, r := range a.ReservedIPs {
			taken[r.Address] = true
		}
		for _, i := range a.Instances {
			taken[i.PublicIP] = true
		}
	})
	for n := uint32(1) << (hostBits - 1); n < uint32(1)<<hostBits-1; n++ {
		v := base + n
		ip := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}).String()
		if !taken[ip] {
			return ip, nil
		}
	}
	return "", errBadRequest("No free reserved IP is left.")
}

// createReservedIP reserves an IPv4 address in a region. The request body of
// the API reference has no instance_id; the Terraform provider sends one
// anyway and then attaches the address with the attach call, so create
// ignores it.
func (s *Simulation) createReservedIP(r *http.Request) (int, any, error) {
	var req struct {
		Region string `json:"region"`
		IPType string `json:"ip_type"`
		Label  string `json:"label"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	switch {
	case req.IPType == "v6":
		return 0, nil, errBadRequest("IPv6 reserved IPs are not supported by the simulation.")
	case req.IPType != "v4":
		return 0, nil, errBadRequest("Invalid ip_type: use v4 or v6.")
	}
	if _, ok := region(req.Region); !ok {
		return 0, nil, errBadRequest("Invalid region.")
	}
	ip, err := s.allocateReservedIP(r.Context())
	if err != nil {
		return 0, nil, err
	}
	var out reservedIPJSON
	err = s.store.Update(func(a *account) error {
		reserved := &reservedIP{ID: newUUID(), Region: req.Region, Address: ip, Label: req.Label}
		a.ReservedIPs = append(a.ReservedIPs, reserved)
		out = renderReservedIP(reserved)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"reserved_ip": out}, nil
}

func (s *Simulation) listReservedIPs(r *http.Request) (int, any, error) {
	var out []reservedIPJSON
	s.store.Read(func(a *account) {
		for _, reserved := range a.ReservedIPs {
			out = append(out, renderReservedIP(reserved))
		}
	})
	return listResponse(r, "reserved_ips", out)
}

func (s *Simulation) getReservedIP(r *http.Request) (int, any, error) {
	var out *reservedIPJSON
	s.store.Read(func(a *account) {
		if reserved := a.reservedIP(r.PathValue("id")); reserved != nil {
			j := renderReservedIP(reserved)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound(reservedIPNotFound)
	}
	return http.StatusOK, map[string]any{"reserved_ip": out}, nil
}

func (s *Simulation) updateReservedIP(r *http.Request) (int, any, error) {
	var req struct {
		Label *string `json:"label"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Label == nil {
		return 0, nil, errBadRequest("label is required.")
	}
	var out reservedIPJSON
	err := s.store.Update(func(a *account) error {
		reserved := a.reservedIP(r.PathValue("id"))
		if reserved == nil {
			return errNotFound(reservedIPNotFound)
		}
		reserved.Label = *req.Label
		out = renderReservedIP(reserved)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"reserved_ip": out}, nil
}

// moveReservedIP points the reserved IP the path names at instanceID, or at
// no instance when it is empty, and releases it when release is set: check
// validates the move, the machines that lose or gain the address get their
// new lists, and then the account records it. A failed push changes nothing.
// A machine that is being deleted keeps its list.
func (s *Simulation) moveReservedIP(r *http.Request, instanceID string, release bool, check func(*account, *reservedIP) error) error {
	s.addressMu.Lock()
	defer s.addressMu.Unlock()
	id := r.PathValue("id")
	pushes := map[string][]string{}
	var err error
	s.store.Read(func(a *account) {
		reserved := a.reservedIP(id)
		if reserved == nil {
			err = errNotFound(reservedIPNotFound)
			return
		}
		if err = check(a, reserved); err != nil {
			return
		}
		if from := a.instance(reserved.InstanceID); from != nil && from.ID != instanceID && from.Machine != "" && !from.Destroying {
			pushes[from.Machine] = slices.DeleteFunc(a.extraAddresses(from.ID), func(v string) bool { return v == reserved.Address })
		}
		if to := a.instance(instanceID); to != nil && to.ID != reserved.InstanceID {
			pushes[to.Machine] = append(a.extraAddresses(to.ID), reserved.Address)
		}
	})
	if err != nil {
		return err
	}
	for machine, addrs := range pushes {
		if err := s.machines.SetAddresses(r.Context(), machine, addrs); err != nil {
			return err
		}
	}
	return s.store.Update(func(a *account) error {
		if reserved := a.reservedIP(id); reserved != nil {
			reserved.InstanceID = instanceID
		}
		if release {
			a.ReservedIPs = slices.DeleteFunc(a.ReservedIPs, func(v *reservedIP) bool { return v.ID == id })
			for _, i := range a.Instances {
				if i.MainReservedIP == id {
					i.MainReservedIP = ""
				}
			}
		}
		return nil
	})
}

// attachReservedIP routes the address to an instance in its region.
// Attaching it to the instance it is attached to already succeeds; an
// address attached elsewhere must be detached first.
func (s *Simulation) attachReservedIP(r *http.Request) (int, any, error) {
	var req struct {
		InstanceID string `json:"instance_id"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	err := s.moveReservedIP(r, req.InstanceID, false, func(a *account, reserved *reservedIP) error {
		inst := a.instance(req.InstanceID)
		switch {
		case inst == nil || inst.Destroying:
			return errBadRequest("Invalid instance_id.")
		case inst.Region != reserved.Region:
			return errBadRequest("The reserved IP and the instance must be in the same region.")
		case inst.Machine == "":
			return errBadRequest("The instance is still being created.")
		case reserved.InstanceID != "" && reserved.InstanceID != inst.ID:
			return errBadRequest("The reserved IP is already attached to another server.")
		}
		return nil
	})
	return http.StatusNoContent, nil, err
}

// detachReservedIP takes the address off its instance.
func (s *Simulation) detachReservedIP(r *http.Request) (int, any, error) {
	err := s.moveReservedIP(r, "", false, func(a *account, reserved *reservedIP) error {
		if a.instance(reserved.InstanceID) == nil {
			return errBadRequest("The reserved IP is not attached to an instance.")
		}
		return keepLiveMainAddress(a, reserved)
	})
	return http.StatusNoContent, nil, err
}

// deleteReservedIP releases the address, taking it off its instance first.
func (s *Simulation) deleteReservedIP(r *http.Request) (int, any, error) {
	err := s.moveReservedIP(r, "", true, keepLiveMainAddress)
	return http.StatusNoContent, nil, err
}

// keepLiveMainAddress refuses to take a reserved IP off the instance it is
// the main address of, until that instance is being destroyed.
func keepLiveMainAddress(a *account, reserved *reservedIP) error {
	if inst := a.instance(reserved.InstanceID); inst != nil && inst.MainReservedIP == reserved.ID && !inst.Destroying {
		return errBadRequest("The reserved IP is the main IP of instance %s; delete the instance to release it.", inst.ID)
	}
	return nil
}

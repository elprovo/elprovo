package vultr

import (
	"net/http"
	"net/netip"
	"slices"
)

const vpcNotFound = "Invalid VPC ID."

var privateRanges = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"),
}

type vpcJSON struct {
	ID           string `json:"id"`
	Region       string `json:"region"`
	DateCreated  string `json:"date_created"`
	Description  string `json:"description"`
	V4Subnet     string `json:"v4_subnet"`
	V4SubnetMask int    `json:"v4_subnet_mask"`
}

func renderVPC(v *vpc) vpcJSON {
	return vpcJSON{ID: v.ID, Region: v.Region, DateCreated: timestamp(v.CreatedAt), Description: v.Description, V4Subnet: v.Subnet, V4SubnetMask: v.SubnetMask}
}

// freeVPCSubnet returns a /20 in 10.0.0.0/8 that no VPC of the account uses,
// the size Vultr gives a VPC when the request names no subnet.
func freeVPCSubnet(a *account) (netip.Prefix, bool) {
	for second := 1; second < 256; second++ {
		for third := 0; third < 256; third += 16 {
			candidate := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(second), byte(third), 0}), 20)
			taken := slices.ContainsFunc(a.VPCs, func(v *vpc) bool {
				other, err := netip.ParseAddr(v.Subnet)
				return err == nil && netip.PrefixFrom(other, v.SubnetMask).Overlaps(candidate)
			})
			if !taken {
				return candidate, true
			}
		}
	}
	return netip.Prefix{}, false
}

func (s *Simulation) createVPC(r *http.Request) (int, any, error) {
	var req struct {
		Region       string `json:"region"`
		Description  string `json:"description"`
		V4Subnet     string `json:"v4_subnet"`
		V4SubnetMask int    `json:"v4_subnet_mask"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if _, ok := region(req.Region); !ok {
		return 0, nil, errBadRequest("Invalid region.")
	}
	var subnet netip.Prefix
	if (req.V4Subnet == "") != (req.V4SubnetMask == 0) {
		return 0, nil, errBadRequest("v4_subnet and v4_subnet_mask must be given together.")
	}
	if req.V4Subnet != "" {
		addr, err := netip.ParseAddr(req.V4Subnet)
		if err != nil || !addr.Is4() || req.V4SubnetMask < 8 || req.V4SubnetMask > 29 {
			return 0, nil, errBadRequest("Invalid v4_subnet or v4_subnet_mask.")
		}
		subnet = netip.PrefixFrom(addr, req.V4SubnetMask)
		if subnet.Masked() != subnet || !slices.ContainsFunc(privateRanges, func(p netip.Prefix) bool { return p.Contains(addr) && p.Bits() <= subnet.Bits() }) {
			return 0, nil, errBadRequest("v4_subnet must be a private (RFC 1918) network address.")
		}
	}
	var out vpcJSON
	err := s.store.Update(func(a *account) error {
		if !subnet.IsValid() {
			free, ok := freeVPCSubnet(a)
			if !ok {
				return errBadRequest("No free VPC subnet is left.")
			}
			subnet = free
		}
		v := &vpc{ID: newUUID(), Region: req.Region, Description: req.Description, Subnet: subnet.Addr().String(), SubnetMask: subnet.Bits(), CreatedAt: s.now()}
		a.VPCs = append(a.VPCs, v)
		out = renderVPC(v)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"vpc": out}, nil
}

func (s *Simulation) listVPCs(r *http.Request) (int, any, error) {
	var out []vpcJSON
	s.store.Read(func(a *account) {
		for _, v := range a.VPCs {
			out = append(out, renderVPC(v))
		}
	})
	return listResponse(r, "vpcs", out)
}

func (s *Simulation) getVPC(r *http.Request) (int, any, error) {
	var out *vpcJSON
	s.store.Read(func(a *account) {
		if v := a.vpc(r.PathValue("id")); v != nil {
			j := renderVPC(v)
			out = &j
		}
	})
	if out == nil {
		return 0, nil, errNotFound(vpcNotFound)
	}
	return http.StatusOK, map[string]any{"vpc": out}, nil
}

func (s *Simulation) updateVPC(r *http.Request) (int, any, error) {
	var req struct {
		Description *string `json:"description"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if req.Description == nil {
		return 0, nil, errBadRequest("description is required.")
	}
	err := s.store.Update(func(a *account) error {
		v := a.vpc(r.PathValue("id"))
		if v == nil {
			return errNotFound(vpcNotFound)
		}
		v.Description = *req.Description
		return nil
	})
	return http.StatusNoContent, nil, err
}

// deleteVPC refuses while an instance is attached, including one that is
// being destroyed, with the message the Terraform provider retries on.
func (s *Simulation) deleteVPC(r *http.Request) (int, any, error) {
	id := r.PathValue("id")
	err := s.store.Update(func(a *account) error {
		if a.vpc(id) == nil {
			return errNotFound(vpcNotFound)
		}
		if slices.ContainsFunc(a.Instances, func(i *instance) bool { return slices.Contains(i.VPCs, id) }) {
			return errBadRequest("Unable to delete VPC: there are instances attached to this VPC network.")
		}
		a.VPCs = slices.DeleteFunc(a.VPCs, func(v *vpc) bool { return v.ID == id })
		return nil
	})
	return http.StatusNoContent, nil, err
}

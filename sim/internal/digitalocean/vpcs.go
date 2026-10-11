package digitalocean

import (
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
)

var (
	vpcNamePattern = regexp.MustCompile(`^[a-zA-Z0-9\-.]+$`)
	privateRanges  = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16")}
)

type vpcJSON struct {
	ID          string `json:"id"`
	URN         string `json:"urn"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Region      string `json:"region"`
	IPRange     string `json:"ip_range"`
	Default     bool   `json:"default"`
	CreatedAt   string `json:"created_at"`
}

func renderVPC(v *vpc) vpcJSON {
	return vpcJSON{
		ID: v.ID, URN: "do:vpc:" + v.ID, Name: v.Name, Description: v.Description, Region: v.Region,
		IPRange: v.IPRange, Default: v.Default, CreatedAt: timestamp(v.CreatedAt),
	}
}

func freeVPCRange(a *account) string {
	for second := 100; second < 256; second++ {
		for third := 0; third < 256; third += 16 {
			candidate := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(second), byte(third), 0}), 20)
			if !overlapsVPC(a, candidate, "") {
				return candidate.String()
			}
		}
	}
	return ""
}

func overlapsVPC(a *account, p netip.Prefix, except string) bool {
	return slices.ContainsFunc(a.VPCs, func(v *vpc) bool {
		other, err := netip.ParsePrefix(v.IPRange)
		return v.ID != except && err == nil && other.Overlaps(p)
	})
}

func checkIPRange(a *account, value string) error {
	p, err := netip.ParsePrefix(value)
	if err != nil || !p.Addr().Is4() || p.Masked() != p {
		return errUnprocessable("ip_range %q is not a valid IPv4 CIDR block", value)
	}
	if p.Bits() < 16 || p.Bits() > 28 {
		return errUnprocessable("ip_range may not be smaller than /28 nor larger than /16")
	}
	if !slices.ContainsFunc(privateRanges, func(r netip.Prefix) bool { return r.Contains(p.Addr()) && r.Bits() <= p.Bits() }) {
		return errUnprocessable("ip_range must be within an RFC 1918 private network")
	}
	if overlapsVPC(a, p, "") {
		return errUnprocessable("ip_range %s overlaps another VPC in your account", value)
	}
	return nil
}

func (s *Simulation) createVPC(r *http.Request) (int, any, error) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Region      string `json:"region"`
		IPRange     string `json:"ip_range"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	var out vpcJSON
	err := s.store.Update(func(a *account) error {
		if err := checkVPCName(a, req.Name, ""); err != nil {
			return err
		}
		if len(req.Description) > 255 {
			return errUnprocessable("description must be at most 255 characters")
		}
		if !s.catalog.hasRegion(req.Region) {
			return errUnprocessable("region %q is not available", req.Region)
		}
		if req.IPRange == "" {
			req.IPRange = freeVPCRange(a)
		} else if err := checkIPRange(a, req.IPRange); err != nil {
			return err
		}
		v := &vpc{ID: newUUID(), Name: req.Name, Description: req.Description, Region: req.Region, IPRange: req.IPRange, CreatedAt: s.now().UTC()}
		a.VPCs = append(a.VPCs, v)
		out = renderVPC(v)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusCreated, map[string]any{"vpc": out}, nil
}

func checkVPCName(a *account, name, except string) error {
	if !vpcNamePattern.MatchString(name) {
		return errUnprocessable("name may only contain alphanumeric characters, dashes, and periods")
	}
	if slices.ContainsFunc(a.VPCs, func(v *vpc) bool { return v.Name == name && v.ID != except }) {
		return errUnprocessable("a VPC named %q already exists", name)
	}
	return nil
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
		return 0, nil, errNotFound()
	}
	return http.StatusOK, map[string]any{"vpc": out}, nil
}

func (s *Simulation) updateVPC(r *http.Request) (int, any, error) {
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Default     *bool   `json:"default"`
	}
	if err := decodeBody(r, &req); err != nil {
		return 0, nil, err
	}
	if r.Method == http.MethodPut && req.Name == nil {
		return 0, nil, errUnprocessable("name is required")
	}
	var out vpcJSON
	err := s.store.Update(func(a *account) error {
		v := a.vpc(r.PathValue("id"))
		if v == nil {
			return errNotFound()
		}
		if req.Name != nil {
			if err := checkVPCName(a, *req.Name, v.ID); err != nil {
				return err
			}
		}
		if req.Description != nil && len(*req.Description) > 255 {
			return errUnprocessable("description must be at most 255 characters")
		}
		if req.Default != nil && !*req.Default && v.Default {
			return errUnprocessable("a default VPC can not be unset; make another VPC the default")
		}
		if req.Name != nil {
			v.Name = *req.Name
		}
		if req.Description != nil {
			v.Description = *req.Description
		}
		if req.Default != nil && *req.Default && !v.Default {
			if old := a.defaultVPC(v.Region); old != nil {
				old.Default = false
			}
			v.Default = true
		}
		out = renderVPC(v)
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	return http.StatusOK, map[string]any{"vpc": out}, nil
}

func (s *Simulation) deleteVPC(r *http.Request) (int, any, error) {
	err := s.store.Update(func(a *account) error {
		v := a.vpc(r.PathValue("id"))
		switch {
		case v == nil:
			return errNotFound()
		case v.Default:
			return errForbidden("Can not delete default VPCs")
		case slices.ContainsFunc(a.Droplets, func(d *droplet) bool { return d.VPCUUID == v.ID }):
			return errForbidden("Can not delete VPC with members")
		}
		a.VPCs = slices.DeleteFunc(a.VPCs, func(other *vpc) bool { return other.ID == v.ID })
		return nil
	})
	return http.StatusNoContent, nil, err
}

func (s *Simulation) listVPCMembers(r *http.Request) (int, any, error) {
	type member struct {
		URN       string `json:"urn"`
		Name      string `json:"name"`
		CreatedAt string `json:"created_at"`
	}
	var out []member
	found := false
	resourceType := r.URL.Query().Get("resource_type")
	s.store.Read(func(a *account) {
		v := a.vpc(r.PathValue("id"))
		if v == nil {
			return
		}
		found = true
		if resourceType != "" && resourceType != "droplet" {
			return
		}
		for _, d := range a.Droplets {
			if d.VPCUUID == v.ID {
				out = append(out, member{"do:droplet:" + strconv.FormatInt(d.ID, 10), d.Name, timestamp(d.CreatedAt)})
			}
		}
	})
	if !found {
		return 0, nil, errNotFound()
	}
	return listResponse(r, "members", out)
}

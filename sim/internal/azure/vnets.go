package azure

import (
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

var networkNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9_.-]{0,78}[a-zA-Z0-9_])?$`)

type vnet struct {
	meta
	AddressPrefixes []string `json:"address_prefixes"`
	DNSServers      []string `json:"dns_servers,omitempty"`
	// PrivateEndpointPolicies is Disabled unless the client sets it.
	PrivateEndpointPolicies string    `json:"private_endpoint_policies"`
	Subnets                 []*subnet `json:"subnets"`
}

type subnet struct {
	Name       string           `json:"name"`
	Etag       string           `json:"etag"`
	Properties subnetProperties `json:"properties"`
}

type subnetProperties struct {
	AddressPrefix                     string         `json:"addressPrefix,omitempty"`
	AddressPrefixes                   []string       `json:"addressPrefixes,omitempty"`
	NetworkSecurityGroup              *subResource   `json:"networkSecurityGroup,omitempty"`
	DefaultOutboundAccess             *bool          `json:"defaultOutboundAccess,omitempty"`
	PrivateEndpointNetworkPolicies    string         `json:"privateEndpointNetworkPolicies,omitempty"`
	PrivateLinkServiceNetworkPolicies string         `json:"privateLinkServiceNetworkPolicies,omitempty"`
	RouteTable                        *subResource   `json:"routeTable,omitempty"`
	NatGateway                        *subResource   `json:"natGateway,omitempty"`
	ServiceEndpoints                  []any          `json:"serviceEndpoints,omitempty"`
	Delegations                       []any          `json:"delegations,omitempty"`
	IPConfigurations                  []*subResource `json:"ipConfigurations,omitempty"`
	ProvisioningState                 string         `json:"provisioningState,omitempty"`
}

// prefixes are the subnet's address ranges, from addressPrefix or
// addressPrefixes, whichever the client sent.
func (p subnetProperties) prefixes() []string {
	if len(p.AddressPrefixes) > 0 {
		return p.AddressPrefixes
	}
	if p.AddressPrefix != "" {
		return []string{p.AddressPrefix}
	}
	return nil
}

// outboundAccess reports whether machines in the subnet without a public IP
// reach the internet. Azure's default outbound access is on unless the
// subnet turns it off.
func (p subnetProperties) outboundAccess() bool {
	return p.DefaultOutboundAccess == nil || *p.DefaultOutboundAccess
}

type subnetJSON struct {
	ID         string           `json:"id,omitempty"`
	Name       string           `json:"name"`
	Etag       string           `json:"etag,omitempty"`
	Type       string           `json:"type,omitempty"`
	Properties subnetProperties `json:"properties"`
}

type vnetProperties struct {
	AddressSpace struct {
		AddressPrefixes []string `json:"addressPrefixes"`
	} `json:"addressSpace"`
	DhcpOptions *struct {
		DNSServers []string `json:"dnsServers"`
	} `json:"dhcpOptions,omitempty"`
	Subnets                     []subnetJSON `json:"subnets"`
	DdosProtectionPlan          *subResource `json:"ddosProtectionPlan,omitempty"`
	EnableDdosProtection        bool         `json:"enableDdosProtection"`
	VirtualNetworkPeerings      []any        `json:"virtualNetworkPeerings"`
	PrivateEndpointVNetPolicies string       `json:"privateEndpointVNetPolicies,omitempty"`
	ResourceGUID                string       `json:"resourceGuid,omitempty"`
	ProvisioningState           string       `json:"provisioningState,omitempty"`
}

func (s *Simulation) renderSubnet(sub *subscription, v *vnet, sn *subnet) subnetJSON {
	props := sn.Properties
	props.ProvisioningState = statusSucceeded
	id := s.resourceID(v.Group, typeVNet, v.Name, "subnets", sn.Name)
	for _, n := range sub.NICs {
		for _, c := range n.IPConfigurations {
			if sameID(idOf(c.Properties.Subnet), id) {
				props.IPConfigurations = append(props.IPConfigurations, &subResource{ID: s.resourceID(n.Group, typeNIC, n.Name, "ipConfigurations", c.Name)})
			}
		}
	}
	if props.PrivateEndpointNetworkPolicies == "" {
		props.PrivateEndpointNetworkPolicies = "Disabled"
	}
	if props.PrivateLinkServiceNetworkPolicies == "" {
		props.PrivateLinkServiceNetworkPolicies = "Enabled"
	}
	return subnetJSON{ID: id, Name: sn.Name, Etag: sn.Etag, Type: typeSubnet, Properties: props}
}

func (s *Simulation) renderVNet(sub *subscription, v *vnet) resourceJSON {
	var props vnetProperties
	props.AddressSpace.AddressPrefixes = nonNil(v.AddressPrefixes)
	props.DhcpOptions = &struct {
		DNSServers []string `json:"dnsServers"`
	}{nonNil(v.DNSServers)}
	props.Subnets = []subnetJSON{}
	for _, sn := range v.Subnets {
		props.Subnets = append(props.Subnets, s.renderSubnet(sub, v, sn))
	}
	props.VirtualNetworkPeerings, props.PrivateEndpointVNetPolicies = []any{}, v.PrivateEndpointPolicies
	props.ResourceGUID, props.ProvisioningState = v.GUID, statusSucceeded
	return resourceJSON{ID: s.idFor(typeVNet, &v.meta), Name: v.Name, Type: typeVNet, Location: v.Location, Tags: cloneTags(v.Tags), Etag: v.Etag, Properties: props}
}

func parsePrefixes(values []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, v := range values {
		p, err := netip.ParsePrefix(v)
		if err != nil {
			return nil, errBadRequest("InvalidCIDRNotation", "The address prefix %s in resource is invalid. It must be in CIDR notation.", v)
		}
		if !p.Addr().Is4() {
			return nil, errUnsupported("The IPv6 address prefix %s", v)
		}
		if p.Masked() != p {
			return nil, errBadRequest("InvalidCIDRNotation", "The address prefix %s has an invalid CIDR notation. For the given prefix length, the address prefix should be %s.", v, p.Masked())
		}
		out = append(out, p)
	}
	return out, nil
}

func within(inner netip.Prefix, outer []netip.Prefix) bool {
	return slices.ContainsFunc(outer, func(o netip.Prefix) bool { return o.Bits() <= inner.Bits() && o.Contains(inner.Addr()) })
}

// checkSubnet validates a subnet of the network whose other subnets are
// others.
func (s *Simulation) checkSubnet(sub *subscription, v *vnet, space []netip.Prefix, sn subnetJSON, others []*subnet) error {
	props := sn.Properties
	switch {
	case !networkNamePattern.MatchString(sn.Name):
		return errInvalidParameter("Subnet name '%s' is invalid.", sn.Name)
	case len(props.prefixes()) == 0:
		return errBadRequest("InvalidRequestFormat", "Subnet %s must have an addressPrefix or addressPrefixes.", sn.Name)
	case props.RouteTable != nil:
		return errUnsupported("A route table on subnet %s", sn.Name)
	case props.NatGateway != nil:
		return errUnsupported("A NAT gateway on subnet %s", sn.Name)
	case len(props.ServiceEndpoints) > 0:
		return errUnsupported("A service endpoint on subnet %s", sn.Name)
	case len(props.Delegations) > 0:
		return errUnsupported("A delegation on subnet %s", sn.Name)
	}
	prefixes, err := parsePrefixes(props.prefixes())
	if err != nil {
		return err
	}
	for _, p := range prefixes {
		if !within(p, space) {
			return errBadRequest("NetcfgSubnetRangeOutsideVnet", "Subnet '%s' is not valid because its IP address range is outside the IP address range of virtual network '%s'.", sn.Name, v.Name)
		}
		for _, o := range others {
			if strings.EqualFold(o.Name, sn.Name) {
				continue
			}
			theirs, _ := parsePrefixes(o.Properties.prefixes())
			if slices.ContainsFunc(theirs, p.Overlaps) {
				return errBadRequest("NetcfgSubnetRangesOverlap", "Subnet '%s' is not valid in virtual network '%s' because its IP address range overlaps with that of an existing subnet in virtual network.", sn.Name, v.Name)
			}
		}
	}
	if id := idOf(props.NetworkSecurityGroup); id != "" && s.nsgByID(sub, id) == nil {
		return s.missingReference(id, s.resourceID(v.Group, typeVNet, v.Name, "subnets", sn.Name))
	}
	return nil
}

// subnetInUse returns the id of an IP configuration in the subnet, or "".
func (s *Simulation) subnetInUse(sub *subscription, subnetID string) string {
	for _, n := range sub.NICs {
		for _, c := range n.IPConfigurations {
			if sameID(idOf(c.Properties.Subnet), subnetID) {
				return s.resourceID(n.Group, typeNIC, n.Name, "ipConfigurations", c.Name)
			}
		}
	}
	return ""
}

func errSubnetInUse(name, user string) *armError {
	return errBadRequest("InUseSubnetCannotBeDeleted", "Subnet %s is in use by %s and cannot be deleted. In order to delete the subnet, delete all the resources within the subnet.", name, user)
}

func (s *Simulation) putVNet(r *http.Request, p params) (reply, error) {
	var req resourceRequest[vnetProperties]
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	location, err := s.checkLocation(req.Location)
	if err != nil {
		return reply{}, err
	}
	if !networkNamePattern.MatchString(p["name"]) {
		return reply{}, errInvalidParameter("Resource name %s is invalid.", p["name"])
	}
	if req.Properties.DdosProtectionPlan != nil || req.Properties.EnableDdosProtection {
		return reply{}, errUnsupported("DDoS protection")
	}
	space, err := parsePrefixes(req.Properties.AddressSpace.AddressPrefixes)
	if err != nil {
		return reply{}, err
	}
	if len(space) == 0 {
		return reply{}, errInvalidParameter("The virtual network %s must have at least one address prefix in addressSpace.", p["name"])
	}
	var out resourceJSON
	var op *operation
	status := http.StatusOK
	err = s.store.Update(func(sub *subscription) error {
		g, err := requireGroup(sub, p["group"])
		if err != nil {
			return err
		}
		v := lookup(sub.VNets, g.Name, p["name"])
		isNew := v == nil
		if isNew {
			v = &vnet{meta: meta{Group: g.Name, Name: p["name"], GUID: newUUID()}}
		} else if v.Location != location {
			return errBadRequest("InvalidResourceLocation", "The resource '%s' already exists in location '%s' in resource group '%s'. A resource with the same name cannot be created in location '%s'.", v.Name, v.Location, g.Name, location)
		}
		subnets, err := s.plannedSubnets(sub, v, space, req.Properties.Subnets)
		if err != nil {
			return err
		}
		v.Location, v.Tags, v.Etag = location, cloneTags(req.Tags), etag()
		v.AddressPrefixes, v.Subnets = req.Properties.AddressSpace.AddressPrefixes, subnets
		v.DNSServers, v.PrivateEndpointPolicies = nil, req.Properties.PrivateEndpointVNetPolicies
		if v.PrivateEndpointPolicies == "" {
			v.PrivateEndpointPolicies = "Disabled"
		}
		if req.Properties.DhcpOptions != nil {
			v.DNSServers = req.Properties.DhcpOptions.DNSServers
		}
		if isNew {
			sub.VNets = append(sub.VNets, v)
			status = http.StatusCreated
		}
		op = s.doneOperation(sub, "Microsoft.Network", location)
		out = s.renderVNet(sub, v)
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	s.firewalls.kick()
	return reply{status: status, body: out, headers: s.asyncHeaders(r, op)}, nil
}

// plannedSubnets validates the subnets a network PUT sends, which replace
// the network's subnets, and returns them.
func (s *Simulation) plannedSubnets(sub *subscription, v *vnet, space []netip.Prefix, sent []subnetJSON) ([]*subnet, error) {
	var planned []*subnet
	for _, sn := range sent {
		planned = append(planned, &subnet{Name: sn.Name, Properties: sn.Properties})
	}
	for i, sn := range sent {
		if err := s.checkSubnet(sub, v, space, sn, planned[:i]); err != nil {
			return nil, err
		}
		planned[i].Properties.IPConfigurations, planned[i].Properties.ProvisioningState = nil, ""
		planned[i].Etag = etag()
	}
	for _, old := range v.Subnets {
		if slices.ContainsFunc(planned, func(n *subnet) bool { return strings.EqualFold(n.Name, old.Name) }) {
			continue
		}
		if user := s.subnetInUse(sub, s.resourceID(v.Group, typeVNet, v.Name, "subnets", old.Name)); user != "" {
			return nil, errSubnetInUse(old.Name, user)
		}
	}
	return planned, nil
}

func (s *Simulation) getVNet(_ *http.Request, p params) (reply, error) {
	var out *resourceJSON
	s.store.Read(func(sub *subscription) {
		if v := lookup(sub.VNets, p["group"], p["name"]); v != nil {
			j := s.renderVNet(sub, v)
			out = &j
		}
	})
	if out == nil {
		return reply{}, errNotFound(typeVNet, p["name"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) listVNets(r *http.Request, p params) (reply, error) {
	var out []resourceJSON
	s.store.Read(func(sub *subscription) {
		for _, v := range sub.VNets {
			if p["group"] == "" || strings.EqualFold(v.Group, p["group"]) {
				out = append(out, s.renderVNet(sub, v))
			}
		}
	})
	return listReply(r, out)
}

func (s *Simulation) deleteVNet(r *http.Request, p params) (reply, error) {
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		v := lookup(sub.VNets, p["group"], p["name"])
		if v == nil {
			return nil
		}
		for _, sn := range v.Subnets {
			if user := s.subnetInUse(sub, s.resourceID(v.Group, typeVNet, v.Name, "subnets", sn.Name)); user != "" {
				return errSubnetInUse(sn.Name, user)
			}
		}
		sub.VNets = without(sub.VNets, v.Group, v.Name)
		op = s.doneOperation(sub, "Microsoft.Network", v.Location)
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	s.firewalls.kick()
	return s.accepted(r, op), nil
}

func (s *Simulation) putSubnet(r *http.Request, p params) (reply, error) {
	var req subnetJSON
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	req.Name = p["subnet"]
	var out subnetJSON
	var op *operation
	status := http.StatusOK
	err := s.store.Update(func(sub *subscription) error {
		if _, err := requireGroup(sub, p["group"]); err != nil {
			return err
		}
		v := lookup(sub.VNets, p["group"], p["name"])
		if v == nil {
			return errNotFound(typeVNet, p["name"], p["group"])
		}
		space, _ := parsePrefixes(v.AddressPrefixes)
		if err := s.checkSubnet(sub, v, space, req, v.Subnets); err != nil {
			return err
		}
		props := req.Properties
		props.IPConfigurations, props.ProvisioningState = nil, ""
		i := slices.IndexFunc(v.Subnets, func(sn *subnet) bool { return strings.EqualFold(sn.Name, req.Name) })
		if i < 0 {
			v.Subnets = append(v.Subnets, &subnet{Name: req.Name})
			i = len(v.Subnets) - 1
			status = http.StatusCreated
		}
		v.Subnets[i].Properties, v.Subnets[i].Etag = props, etag()
		op = s.doneOperation(sub, "Microsoft.Network", v.Location)
		out = s.renderSubnet(sub, v, v.Subnets[i])
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	s.firewalls.kick()
	return reply{status: status, body: out, headers: s.asyncHeaders(r, op)}, nil
}

func (s *Simulation) getSubnet(_ *http.Request, p params) (reply, error) {
	var out *subnetJSON
	s.store.Read(func(sub *subscription) {
		v := lookup(sub.VNets, p["group"], p["name"])
		if v == nil {
			return
		}
		for _, sn := range v.Subnets {
			if strings.EqualFold(sn.Name, p["subnet"]) {
				j := s.renderSubnet(sub, v, sn)
				out = &j
			}
		}
	})
	if out == nil {
		return reply{}, errNotFound(typeSubnet, p["name"]+"/"+p["subnet"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) listSubnets(r *http.Request, p params) (reply, error) {
	var out []subnetJSON
	var found bool
	s.store.Read(func(sub *subscription) {
		v := lookup(sub.VNets, p["group"], p["name"])
		if found = v != nil; !found {
			return
		}
		for _, sn := range v.Subnets {
			out = append(out, s.renderSubnet(sub, v, sn))
		}
	})
	if !found {
		return reply{}, errNotFound(typeVNet, p["name"], p["group"])
	}
	return listReply(r, out)
}

func (s *Simulation) deleteSubnet(r *http.Request, p params) (reply, error) {
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		v := lookup(sub.VNets, p["group"], p["name"])
		if v == nil {
			return nil
		}
		i := slices.IndexFunc(v.Subnets, func(sn *subnet) bool { return strings.EqualFold(sn.Name, p["subnet"]) })
		if i < 0 {
			return nil
		}
		if user := s.subnetInUse(sub, s.resourceID(v.Group, typeVNet, v.Name, "subnets", v.Subnets[i].Name)); user != "" {
			return errSubnetInUse(v.Subnets[i].Name, user)
		}
		v.Subnets = slices.Delete(v.Subnets, i, i+1)
		op = s.doneOperation(sub, "Microsoft.Network", v.Location)
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	s.firewalls.kick()
	return s.accepted(r, op), nil
}

// subnetByID returns the network and subnet that id names.
func (s *Simulation) subnetByID(sub *subscription, id string) (*vnet, *subnet) {
	r, ok := s.parseRef(id)
	if !ok || r.typ != typeSubnet {
		return nil, nil
	}
	v := lookup(sub.VNets, r.group, r.name)
	if v == nil {
		return nil, nil
	}
	for _, sn := range v.Subnets {
		if strings.EqualFold(sn.Name, r.child) {
			return v, sn
		}
	}
	return v, nil
}

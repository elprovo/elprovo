package azure

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

type nic struct {
	meta
	MAC              string         `json:"mac"`
	IPConfigurations []ipConfigJSON `json:"ip_configurations"`
	Properties       nicProperties  `json:"properties"`
}

type nicProperties struct {
	IPConfigurations            []ipConfigJSON `json:"ipConfigurations"`
	NetworkSecurityGroup        *subResource   `json:"networkSecurityGroup,omitempty"`
	DNSSettings                 *nicDNS        `json:"dnsSettings,omitempty"`
	EnableAcceleratedNetworking bool           `json:"enableAcceleratedNetworking"`
	EnableIPForwarding          bool           `json:"enableIPForwarding"`
	DisableTCPStateTracking     bool           `json:"disableTcpStateTracking"`
	AuxiliaryMode               string         `json:"auxiliaryMode,omitempty"`
	AuxiliarySku                string         `json:"auxiliarySku,omitempty"`
	MACAddress                  string         `json:"macAddress,omitempty"`
	Primary                     *bool          `json:"primary,omitempty"`
	VirtualMachine              *subResource   `json:"virtualMachine,omitempty"`
	NicType                     string         `json:"nicType,omitempty"`
	HostedWorkloads             []string       `json:"hostedWorkloads,omitempty"`
	ResourceGUID                string         `json:"resourceGuid,omitempty"`
	ProvisioningState           string         `json:"provisioningState,omitempty"`
}

type nicDNS struct {
	DNSServers               []string `json:"dnsServers"`
	AppliedDNSServers        []string `json:"appliedDnsServers"`
	InternalDNSNameLabel     string   `json:"internalDnsNameLabel,omitempty"`
	InternalDomainNameSuffix string   `json:"internalDomainNameSuffix,omitempty"`
}

type ipConfigJSON struct {
	ID         string             `json:"id,omitempty"`
	Name       string             `json:"name"`
	Etag       string             `json:"etag,omitempty"`
	Type       string             `json:"type,omitempty"`
	Properties ipConfigProperties `json:"properties"`
}

type ipConfigProperties struct {
	PrivateIPAddress                      string       `json:"privateIPAddress,omitempty"`
	PrivateIPAllocationMethod             string       `json:"privateIPAllocationMethod,omitempty"`
	PrivateIPAddressVersion               string       `json:"privateIPAddressVersion,omitempty"`
	Subnet                                *subResource `json:"subnet,omitempty"`
	PublicIPAddress                       *subResource `json:"publicIPAddress,omitempty"`
	Primary                               *bool        `json:"primary,omitempty"`
	LoadBalancerBackendAddressPools       []any        `json:"loadBalancerBackendAddressPools,omitempty"`
	LoadBalancerInboundNatRules           []any        `json:"loadBalancerInboundNatRules,omitempty"`
	ApplicationSecurityGroups             []any        `json:"applicationSecurityGroups,omitempty"`
	ApplicationGatewayBackendAddressPools []any        `json:"applicationGatewayBackendAddressPools,omitempty"`
	GatewayLoadBalancer                   *subResource `json:"gatewayLoadBalancer,omitempty"`
	ProvisioningState                     string       `json:"provisioningState,omitempty"`
}

func (s *Simulation) renderNIC(sub *subscription, n *nic) resourceJSON {
	props := n.Properties
	id := s.idFor(typeNIC, &n.meta)
	for _, c := range n.IPConfigurations {
		c.ID, c.Type, c.Etag = s.resourceID(n.Group, typeNIC, n.Name, "ipConfigurations", c.Name), typeIPConfig, n.Etag
		c.Properties.ProvisioningState = statusSucceeded
		props.IPConfigurations = append(props.IPConfigurations, c)
	}
	dns := nicDNS{DNSServers: []string{}, AppliedDNSServers: []string{}, InternalDomainNameSuffix: n.GUID[:8] + ".internal.cloudapp.net"}
	if n.Properties.DNSSettings != nil {
		dns.InternalDNSNameLabel = n.Properties.DNSSettings.InternalDNSNameLabel
	}
	props.DNSSettings = &dns
	props.MACAddress, props.NicType, props.ResourceGUID, props.ProvisioningState = n.MAC, "Standard", n.GUID, statusSucceeded
	if props.AuxiliaryMode == "" {
		props.AuxiliaryMode = "None"
	}
	if props.AuxiliarySku == "" {
		props.AuxiliarySku = "None"
	}
	if v := s.nicUser(sub, id); v != nil {
		props.VirtualMachine = &subResource{ID: s.idFor(typeVM, &v.meta)}
		primary := true
		props.Primary = &primary
	}
	return resourceJSON{ID: id, Name: n.Name, Type: typeNIC, Location: n.Location, Tags: cloneTags(n.Tags), Etag: n.Etag, Properties: props}
}

// nicUser returns the virtual machine the interface belongs to, or nil.
func (s *Simulation) nicUser(sub *subscription, id string) *vm {
	for _, v := range sub.VMs {
		for _, ref := range v.Properties.NetworkProfile.NetworkInterfaces {
			if sameID(ref.ID, id) {
				return v
			}
		}
	}
	return nil
}

// checkIPConfigurations validates the interface's IP configurations and
// returns them with Azure's defaults filled in.
func (s *Simulation) checkIPConfigurations(sub *subscription, nicID, location string, configs []ipConfigJSON) ([]ipConfigJSON, error) {
	if len(configs) == 0 {
		return nil, errInvalidParameter("Network interface %s must have one or more IP configurations.", nicID)
	}
	if len(configs) > 1 {
		return nil, errUnsupported("A network interface with more than one IP configuration")
	}
	c := configs[0]
	p := &c.Properties
	switch {
	case !networkNamePattern.MatchString(c.Name):
		return nil, errInvalidParameter("IP configuration name '%s' is invalid.", c.Name)
	case len(p.LoadBalancerBackendAddressPools)+len(p.LoadBalancerInboundNatRules)+len(p.ApplicationGatewayBackendAddressPools) > 0 || p.GatewayLoadBalancer != nil:
		return nil, errUnsupported("A load balancer or application gateway on IP configuration %s", c.Name)
	case len(p.ApplicationSecurityGroups) > 0:
		return nil, errUnsupported("An application security group on IP configuration %s", c.Name)
	case p.PrivateIPAddressVersion != "" && !strings.EqualFold(p.PrivateIPAddressVersion, "IPv4"):
		return nil, errUnsupported("Private IP address version %s", p.PrivateIPAddressVersion)
	case p.PrivateIPAllocationMethod != "" && !strings.EqualFold(p.PrivateIPAllocationMethod, "Dynamic"):
		return nil, errUnsupported("The %s private IP allocation method; use Dynamic", p.PrivateIPAllocationMethod)
	}
	ipConfigID := nicID + "/ipConfigurations/" + c.Name
	v, sn := s.subnetByID(sub, idOf(p.Subnet))
	if sn == nil {
		return nil, s.missingReference(idOf(p.Subnet), ipConfigID)
	}
	if v.Location != location {
		return nil, errBadRequest("InvalidResourceReference", "Subnet %s is in location %s, but network interface %s is in location %s.", idOf(p.Subnet), v.Location, nicID, location)
	}
	if id := idOf(p.PublicIPAddress); id != "" {
		ip := s.publicIPByID(sub, id)
		if ip == nil {
			return nil, s.missingReference(id, ipConfigID)
		}
		if user := s.publicIPUser(sub, id); user != "" && !sameID(user, ipConfigID) {
			return nil, errBadRequest("PublicIPAddressInUse", "Resource %s is referenced by resource %s. Public IP address %s cannot be associated with more than one IP configuration.", id, user, id)
		}
		p.PublicIPAddress = &subResource{ID: s.idFor(typePublicIP, &ip.meta)}
	}
	primary := true
	p.Subnet = &subResource{ID: s.resourceID(v.Group, typeVNet, v.Name, "subnets", sn.Name)}
	p.PrivateIPAllocationMethod, p.PrivateIPAddressVersion, p.Primary, p.ProvisioningState = "Dynamic", "IPv4", &primary, ""
	return []ipConfigJSON{{Name: c.Name, Properties: *p}}, nil
}

func (s *Simulation) putNIC(r *http.Request, p params) (reply, error) {
	var req resourceRequest[nicProperties]
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	location, err := s.checkLocation(req.Location)
	if err != nil {
		return reply{}, err
	}
	props := req.Properties
	if !networkNamePattern.MatchString(p["name"]) {
		return reply{}, errInvalidParameter("Resource name %s is invalid.", p["name"])
	}
	if props.DNSSettings != nil && len(props.DNSSettings.DNSServers) > 0 {
		return reply{}, errUnsupported("Custom DNS servers on a network interface")
	}
	var out resourceJSON
	var op *operation
	status := http.StatusOK
	err = s.store.Update(func(sub *subscription) error {
		g, err := requireGroup(sub, p["group"])
		if err != nil {
			return err
		}
		id := s.resourceID(g.Name, typeNIC, p["name"])
		configs, err := s.checkIPConfigurations(sub, id, location, props.IPConfigurations)
		if err != nil {
			return err
		}
		if nsgID := idOf(props.NetworkSecurityGroup); nsgID != "" {
			n := s.nsgByID(sub, nsgID)
			if n == nil {
				return s.missingReference(nsgID, id)
			}
			props.NetworkSecurityGroup = &subResource{ID: s.idFor(typeNSG, &n.meta)}
		}
		n := lookup(sub.NICs, g.Name, p["name"])
		if n == nil {
			addr, err := s.allocatePrivateAddress(sub)
			if err != nil {
				return err
			}
			configs[0].Properties.PrivateIPAddress = addr
			sub.NextMAC++
			n = &nic{meta: meta{Group: g.Name, Name: p["name"], GUID: newUUID()}, MAC: macAddress(sub.NextMAC)}
			sub.NICs = append(sub.NICs, n)
			status = http.StatusCreated
		} else {
			if n.Location != location {
				return errBadRequest("InvalidResourceLocation", "The resource '%s' already exists in location '%s'.", n.Name, n.Location)
			}
			old := n.IPConfigurations[0].Properties
			if s.nicUser(sub, id) != nil && !sameID(idOf(old.Subnet), idOf(configs[0].Properties.Subnet)) {
				return errBadRequest("SubnetChangeNotAllowed", "The subnet of IP configuration %s cannot change while network interface %s is attached to a virtual machine.", configs[0].Name, n.Name)
			}
			configs[0].Properties.PrivateIPAddress = old.PrivateIPAddress
		}
		n.Location, n.Tags, n.Etag, n.IPConfigurations = location, cloneTags(req.Tags), etag(), configs
		n.Properties = nicProperties{
			NetworkSecurityGroup: props.NetworkSecurityGroup, EnableAcceleratedNetworking: props.EnableAcceleratedNetworking,
			EnableIPForwarding: props.EnableIPForwarding, DisableTCPStateTracking: props.DisableTCPStateTracking,
			AuxiliaryMode: props.AuxiliaryMode, AuxiliarySku: props.AuxiliarySku,
		}
		if props.DNSSettings != nil {
			n.Properties.DNSSettings = &nicDNS{InternalDNSNameLabel: props.DNSSettings.InternalDNSNameLabel}
		}
		op = s.doneOperation(sub, "Microsoft.Network", location)
		out = s.renderNIC(sub, n)
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	s.firewalls.kick()
	return reply{status: status, body: out, headers: s.asyncHeaders(r, op)}, nil
}

// allocatePrivateAddress returns a free address on the private network, from
// the top down, as the machines package picks addresses. The machine of the
// interface's virtual machine boots with it.
func (s *Simulation) allocatePrivateAddress(sub *subscription) (string, error) {
	taken := map[netip.Addr]bool{}
	for _, n := range sub.NICs {
		for _, c := range n.IPConfigurations {
			if a, err := netip.ParseAddr(c.Properties.PrivateIPAddress); err == nil {
				taken[a] = true
			}
		}
	}
	prefix := s.networks.Private.Prefix.Masked()
	if !prefix.IsValid() || 32-prefix.Bits() < 2 {
		return "", errConflict("SubnetIsFull", "The private network of the simulation has no free address.")
	}
	b := prefix.Addr().As4()
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	broadcast := n | (1<<(32-prefix.Bits()) - 1)
	for v := broadcast - 1; v > n; v-- {
		a := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		if !taken[a] && a != s.networks.Private.Gateway {
			return a.String(), nil
		}
	}
	return "", errConflict("SubnetIsFull", "The private network of the simulation has no free address.")
}

// macAddress spells the nth MAC address in Microsoft's 00-0D-3A range, as
// Azure reports them.
func macAddress(n uint32) string {
	return fmt.Sprintf("00-0D-3A-%02X-%02X-%02X", byte(n>>16), byte(n>>8), byte(n))
}

func (s *Simulation) getNIC(_ *http.Request, p params) (reply, error) {
	var out *resourceJSON
	s.store.Read(func(sub *subscription) {
		if n := lookup(sub.NICs, p["group"], p["name"]); n != nil {
			j := s.renderNIC(sub, n)
			out = &j
		}
	})
	if out == nil {
		return reply{}, errNotFound(typeNIC, p["name"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) listNICs(r *http.Request, p params) (reply, error) {
	var out []resourceJSON
	s.store.Read(func(sub *subscription) {
		for _, n := range inGroup(sub.NICs, p["group"]) {
			out = append(out, s.renderNIC(sub, n))
		}
	})
	return listReply(r, out)
}

func (s *Simulation) deleteNIC(r *http.Request, p params) (reply, error) {
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		n := lookup(sub.NICs, p["group"], p["name"])
		if n == nil {
			return nil
		}
		id := s.idFor(typeNIC, &n.meta)
		if v := s.nicUser(sub, id); v != nil {
			return errBadRequest("NicInUse", "Network Interface %s is used by existing resource %s. In order to delete the network interface, it must be dissociated from the resource. To learn more, see aka.ms/deletenic.", id, s.idFor(typeVM, &v.meta))
		}
		sub.NICs = without(sub.NICs, n.Group, n.Name)
		op = s.doneOperation(sub, "Microsoft.Network", n.Location)
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	s.firewalls.kick()
	return s.accepted(r, op), nil
}

func (s *Simulation) nicByID(sub *subscription, id string) *nic {
	r, ok := s.parseRef(id)
	if !ok || r.typ != typeNIC || r.child != "" {
		return nil
	}
	return lookup(sub.NICs, r.group, r.name)
}

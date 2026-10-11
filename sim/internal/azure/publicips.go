package azure

import (
	"net/http"
	"net/netip"
	"strings"

	"github.com/stagely-dev/stagely/sim/internal/machines"
)

type publicIP struct {
	meta
	SKU        string             `json:"sku"`
	Tier       string             `json:"tier"`
	Zones      []string           `json:"zones,omitempty"`
	Address    string             `json:"address"`
	Properties publicIPProperties `json:"properties"`
}

type publicIPProperties struct {
	PublicIPAllocationMethod string       `json:"publicIPAllocationMethod"`
	PublicIPAddressVersion   string       `json:"publicIPAddressVersion"`
	IdleTimeoutInMinutes     int          `json:"idleTimeoutInMinutes"`
	DNSSettings              *publicIPDNS `json:"dnsSettings,omitempty"`
	IPTags                   []any        `json:"ipTags"`
	PublicIPPrefix           *subResource `json:"publicIPPrefix,omitempty"`
	IPAddress                string       `json:"ipAddress,omitempty"`
	IPConfiguration          *subResource `json:"ipConfiguration,omitempty"`
	ResourceGUID             string       `json:"resourceGuid,omitempty"`
	ProvisioningState        string       `json:"provisioningState,omitempty"`
}

type publicIPDNS struct {
	DomainNameLabel string `json:"domainNameLabel,omitempty"`
	Fqdn            string `json:"fqdn,omitempty"`
	ReverseFqdn     string `json:"reverseFqdn,omitempty"`
}

func (s *Simulation) renderPublicIP(sub *subscription, ip *publicIP) resourceJSON {
	props := ip.Properties
	props.IPAddress, props.ResourceGUID, props.ProvisioningState = ip.Address, ip.GUID, statusSucceeded
	props.IPTags = nonNil(props.IPTags)
	if dns := props.DNSSettings; dns != nil && dns.DomainNameLabel != "" {
		c := *dns
		c.Fqdn = dns.DomainNameLabel + "." + ip.Location + ".cloudapp.azure.com"
		props.DNSSettings = &c
	}
	props.IPConfiguration = refOf(s.publicIPUser(sub, s.idFor(typePublicIP, &ip.meta)))
	return resourceJSON{
		ID: s.idFor(typePublicIP, &ip.meta), Name: ip.Name, Type: typePublicIP, Location: ip.Location, Tags: cloneTags(ip.Tags), Etag: ip.Etag,
		SKU: sku{Name: ip.SKU, Tier: ip.Tier}, Zones: ip.Zones, Properties: props,
	}
}

// publicIPUser returns the id of the IP configuration the address belongs
// to, or "".
func (s *Simulation) publicIPUser(sub *subscription, id string) string {
	for _, n := range sub.NICs {
		for _, c := range n.IPConfigurations {
			if sameID(idOf(c.Properties.PublicIPAddress), id) {
				return s.resourceID(n.Group, typeNIC, n.Name, "ipConfigurations", c.Name)
			}
		}
	}
	return ""
}

func (s *Simulation) putPublicIP(r *http.Request, p params) (reply, error) {
	var req resourceRequest[publicIPProperties]
	if err := decodeBody(r, &req); err != nil {
		return reply{}, err
	}
	location, err := s.checkLocation(req.Location)
	if err != nil {
		return reply{}, err
	}
	props := req.Properties
	sk := sku{Name: "Standard", Tier: "Regional"}
	if req.SKU != nil {
		sk = *req.SKU
		if sk.Tier == "" {
			sk.Tier = "Regional"
		}
	}
	switch {
	case !networkNamePattern.MatchString(p["name"]):
		return reply{}, errInvalidParameter("Resource name %s is invalid.", p["name"])
	case strings.EqualFold(sk.Name, "Basic"):
		return reply{}, errBadRequest("BasicSkuPublicIpRetired", "Basic SKU public IP addresses were retired on September 30, 2025. Use the Standard SKU.")
	case !strings.EqualFold(sk.Name, "Standard") || !strings.EqualFold(sk.Tier, "Regional"):
		return reply{}, errUnsupported("The public IP SKU %s %s", sk.Name, sk.Tier)
	case props.PublicIPAddressVersion != "" && !strings.EqualFold(props.PublicIPAddressVersion, "IPv4"):
		return reply{}, errUnsupported("Public IP address version %s", props.PublicIPAddressVersion)
	case !strings.EqualFold(props.PublicIPAllocationMethod, "Static"):
		return reply{}, errBadRequest("StandardSkuPublicIPAddressesMustBeStatic", "Standard sku publicIp %s must have AllocationMethod set to Static.", p["name"])
	case len(props.IPTags) > 0:
		return reply{}, errUnsupported("An IP tag on a public IP address")
	case props.PublicIPPrefix != nil:
		return reply{}, errUnsupported("A public IP prefix")
	}
	sk.Name = "Standard"
	props.PublicIPAllocationMethod, props.PublicIPAddressVersion = "Static", "IPv4"
	if props.IdleTimeoutInMinutes == 0 {
		props.IdleTimeoutInMinutes = 4
	}
	if props.IdleTimeoutInMinutes < 4 || props.IdleTimeoutInMinutes > 30 {
		return reply{}, errInvalidParameter("The idle timeout of a public IP address must be 4 to 30 minutes.")
	}
	props.IPAddress, props.IPConfiguration, props.ResourceGUID, props.ProvisioningState = "", nil, "", ""
	var out resourceJSON
	var op *operation
	status := http.StatusOK
	err = s.store.Update(func(sub *subscription) error {
		g, err := requireGroup(sub, p["group"])
		if err != nil {
			return err
		}
		ip := lookup(sub.PublicIPs, g.Name, p["name"])
		if ip == nil {
			addr, err := s.allocatePublicAddress(sub)
			if err != nil {
				return err
			}
			ip = &publicIP{meta: meta{Group: g.Name, Name: p["name"], GUID: newUUID()}, Address: addr}
			sub.PublicIPs = append(sub.PublicIPs, ip)
			status = http.StatusCreated
		} else if ip.Location != location {
			return errBadRequest("InvalidResourceLocation", "The resource '%s' already exists in location '%s'.", ip.Name, ip.Location)
		}
		ip.Location, ip.Tags, ip.Etag, ip.SKU, ip.Tier, ip.Zones = location, cloneTags(req.Tags), etag(), sk.Name, sk.Tier, req.Zones
		ip.Properties = props
		op = s.doneOperation(sub, "Microsoft.Network", location)
		out = s.renderPublicIP(sub, ip)
		return nil
	})
	if err != nil {
		return reply{}, err
	}
	return reply{status: status, body: out, headers: s.asyncHeaders(r, op)}, nil
}

// allocatePublicAddress returns a free address on the public network.
// Docker gives containers addresses from the bottom of the subnet and
// machines get theirs from the top, so public IPs come from the middle
// upward.
func (s *Simulation) allocatePublicAddress(sub *subscription) (string, error) {
	taken := map[netip.Addr]bool{}
	for _, ip := range sub.PublicIPs {
		if a, err := netip.ParseAddr(ip.Address); err == nil {
			taken[a] = true
		}
	}
	if addr, ok := middleFree(s.networks.Public, taken); ok {
		return addr.String(), nil
	}
	return "", errConflict("PublicIPCountLimitReached", "The public network of the simulation has no free address left.")
}

func middleFree(subnet machines.Subnet, taken map[netip.Addr]bool) (netip.Addr, bool) {
	prefix := subnet.Prefix.Masked()
	if !prefix.IsValid() || 32-prefix.Bits() < 3 {
		return netip.Addr{}, false
	}
	b := prefix.Addr().As4()
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	hostBits := 32 - prefix.Bits()
	for i := uint32(1) << (hostBits - 1); i < uint32(1)<<hostBits-1; i++ {
		v := n + i
		a := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		if !taken[a] && a != subnet.Gateway {
			return a, true
		}
	}
	return netip.Addr{}, false
}

func (s *Simulation) getPublicIP(_ *http.Request, p params) (reply, error) {
	var out *resourceJSON
	s.store.Read(func(sub *subscription) {
		if ip := lookup(sub.PublicIPs, p["group"], p["name"]); ip != nil {
			j := s.renderPublicIP(sub, ip)
			out = &j
		}
	})
	if out == nil {
		return reply{}, errNotFound(typePublicIP, p["name"], p["group"])
	}
	return okReply(out), nil
}

func (s *Simulation) listPublicIPs(r *http.Request, p params) (reply, error) {
	var out []resourceJSON
	s.store.Read(func(sub *subscription) {
		for _, ip := range inGroup(sub.PublicIPs, p["group"]) {
			out = append(out, s.renderPublicIP(sub, ip))
		}
	})
	return listReply(r, out)
}

func (s *Simulation) deletePublicIP(r *http.Request, p params) (reply, error) {
	var op *operation
	err := s.store.Update(func(sub *subscription) error {
		ip := lookup(sub.PublicIPs, p["group"], p["name"])
		if ip == nil {
			return nil
		}
		id := s.idFor(typePublicIP, &ip.meta)
		if user := s.publicIPUser(sub, id); user != "" {
			return errBadRequest("PublicIPAddressCannotBeDeleted", "Public IP address %s can not be deleted since it is still allocated to resource %s. In order to delete the public IP, disassociate/detach the Public IP address from the resource.", id, user)
		}
		sub.PublicIPs = without(sub.PublicIPs, ip.Group, ip.Name)
		op = s.doneOperation(sub, "Microsoft.Network", ip.Location)
		return nil
	})
	if err != nil || op == nil {
		return reply{status: http.StatusNoContent}, err
	}
	return s.accepted(r, op), nil
}

func (s *Simulation) publicIPByID(sub *subscription, id string) *publicIP {
	r, ok := s.parseRef(id)
	if !ok || r.typ != typePublicIP || r.child != "" {
		return nil
	}
	return lookup(sub.PublicIPs, r.group, r.name)
}

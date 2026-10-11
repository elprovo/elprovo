package aws

import (
	"context"
	"net/netip"
	"slices"
)

// address is an Elastic IP: a public address the account keeps and points
// at one instance at a time. The instance's machine answers at it as an
// extra address on its public interface.
type address struct {
	AllocationID  string `json:"allocation_id"`
	Region        string `json:"region"`
	PublicIP      string `json:"public_ip"`
	InstanceID    string `json:"instance_id,omitempty"`
	AssociationID string `json:"association_id,omitempty"`
}

func (w *world) address(region, allocationID string) *address {
	return find(w.Addresses, func(a *address) bool { return a.AllocationID == allocationID && a.Region == region })
}

func (s *Simulation) ec2AddressActions() map[string]ec2Action {
	return map[string]ec2Action{
		"AllocateAddress":            {s.allocateAddress, []string{"Domain", "Address", "PublicIpv4Pool", "NetworkBorderGroup", "TagSpecification"}},
		"DescribeAddresses":          {s.describeAddresses, []string{"AllocationId", "PublicIp", "Filter"}},
		"DescribeAddressesAttribute": {s.describeAddressesAttribute, []string{"AllocationId", "Attribute", "MaxResults", "NextToken"}},
		"AssociateAddress":           {s.associateAddress, []string{"AllocationId", "InstanceId", "AllowReassociation"}},
		"DisassociateAddress":        {s.disassociateAddress, []string{"AssociationId"}},
		"ReleaseAddress":             {s.releaseAddress, []string{"AllocationId", "NetworkBorderGroup"}},
	}
}

// freePublicIP returns a free address on the public network. Docker gives
// containers addresses from the bottom of the subnet and machines get theirs
// from the top, so Elastic IPs come from the middle upward.
func (s *Simulation) freePublicIP(ctx context.Context) (string, error) {
	networks, err := s.machines.Networks(ctx)
	if err != nil {
		return "", err
	}
	prefix := networks.Public.Prefix.Masked()
	hostBits := 32 - prefix.Bits()
	if hostBits < 3 {
		return "", clientError("AddressLimitExceeded", "the public network is too small for Elastic IPs")
	}
	taken := map[string]bool{}
	s.store.Read(func(w *world) {
		for _, a := range w.Addresses {
			taken[a.PublicIP] = true
		}
		for _, i := range w.Instances {
			taken[i.MachinePublicIP] = true
		}
	})
	start := prefix.Addr().As4()
	n := uint32(start[0])<<24 | uint32(start[1])<<16 | uint32(start[2])<<8 | uint32(start[3])
	for i := uint32(1) << (hostBits - 1); i < uint32(1)<<hostBits-1; i++ {
		v := n + i
		ip := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}).String()
		if !taken[ip] {
			return ip, nil
		}
	}
	return "", clientError("AddressLimitExceeded", "The maximum number of addresses has been reached.")
}

type allocateAddressResponse struct {
	ec2Meta
	PublicIP           string `xml:"publicIp"`
	AllocationID       string `xml:"allocationId"`
	Domain             string `xml:"domain"`
	PublicIPv4Pool     string `xml:"publicIpv4Pool"`
	NetworkBorderGroup string `xml:"networkBorderGroup"`
}

func (s *Simulation) allocateAddress(c *ec2Call) (ec2Response, error) {
	if c.params.get("Address") != "" {
		return nil, errUnsupported("Bringing your own address")
	}
	if pool := c.params.get("PublicIpv4Pool"); pool != "" && pool != "amazon" {
		return nil, errUnsupported("Address pool %s", pool)
	}
	if g := c.params.get("NetworkBorderGroup"); g != "" && g != c.region {
		return nil, errInvalidParameter("Invalid network border group %s", g)
	}
	specs, err := tagSpecifications(c.params, "elastic-ip")
	if err != nil {
		return nil, err
	}
	ip, err := s.freePublicIP(c.ctx)
	if err != nil {
		return nil, err
	}
	out := &allocateAddressResponse{PublicIP: ip, Domain: "vpc", PublicIPv4Pool: "amazon", NetworkBorderGroup: c.region}
	err = s.store.Update(func(w *world) error {
		a := &address{AllocationID: newID("eipalloc"), Region: c.region, PublicIP: ip}
		w.Addresses = append(w.Addresses, a)
		for _, t := range specs["elastic-ip"] {
			w.setTag(a.AllocationID, t)
		}
		out.AllocationID = a.AllocationID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type addressXML struct {
	PublicIP                string `xml:"publicIp"`
	AllocationID            string `xml:"allocationId"`
	Domain                  string `xml:"domain"`
	InstanceID              string `xml:"instanceId,omitempty"`
	AssociationID           string `xml:"associationId,omitempty"`
	NetworkInterfaceID      string `xml:"networkInterfaceId,omitempty"`
	NetworkInterfaceOwnerID string `xml:"networkInterfaceOwnerId,omitempty"`
	PrivateIPAddress        string `xml:"privateIpAddress,omitempty"`
	PublicIPv4Pool          string `xml:"publicIpv4Pool"`
	NetworkBorderGroup      string `xml:"networkBorderGroup"`
	TagSet                  []tag  `xml:"tagSet>item"`
}

func renderAddress(w *world, a *address) addressXML {
	out := addressXML{
		PublicIP: a.PublicIP, AllocationID: a.AllocationID, Domain: "vpc", InstanceID: a.InstanceID, AssociationID: a.AssociationID,
		PublicIPv4Pool: "amazon", NetworkBorderGroup: a.Region, TagSet: w.tagsOf(a.AllocationID),
	}
	if i := w.instance(a.InstanceID); i != nil {
		out.NetworkInterfaceID, out.NetworkInterfaceOwnerID, out.PrivateIPAddress = i.ENIID, w.AccountID, i.PrivateIP
	}
	return out
}

type describeAddressesResponse struct {
	ec2Meta
	Addresses []addressXML `xml:"addressesSet>item"`
}

func (s *Simulation) describeAddresses(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	out := &describeAddressesResponse{}
	var err error
	s.store.Read(func(w *world) {
		addresses := regional(w.Addresses, c.region)
		addresses, err = selectIDs(addresses, func(a *address) string { return a.AllocationID }, c.params.list("AllocationId"), notFoundByID)
		if err == nil {
			addresses, err = selectIDs(addresses, func(a *address) string { return a.PublicIP }, c.params.list("PublicIp"), func(ip string) *apiError {
				return errNotFound("InvalidAddress.NotFound", "Address '%s' not found.", ip)
			})
		}
		for _, a := range addresses {
			if err != nil {
				return
			}
			x := renderAddress(w, a)
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "allocation-id":
					return values(x.AllocationID), true
				case "association-id":
					return values(x.AssociationID), true
				case "instance-id":
					return values(x.InstanceID), true
				case "public-ip":
					return values(x.PublicIP), true
				case "domain":
					return values(x.Domain), true
				case "network-interface-id":
					return values(x.NetworkInterfaceID), true
				case "private-ip-address":
					return values(x.PrivateIPAddress), true
				case "network-border-group":
					return values(x.NetworkBorderGroup), true
				}
				return nil, false
			}, w.tagsOf(a.AllocationID))
			if match {
				out.Addresses = append(out.Addresses, x)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type addressAttributeXML struct {
	PublicIP     string `xml:"publicIp"`
	AllocationID string `xml:"allocationId"`
}

type describeAddressesAttributeResponse struct {
	ec2Meta
	Addresses []addressAttributeXML `xml:"addressSet>item"`
}

// describeAddressesAttribute answers the domain-name attribute. Simulated
// addresses have no reverse DNS record.
func (s *Simulation) describeAddressesAttribute(c *ec2Call) (ec2Response, error) {
	if attr := c.params.get("Attribute"); attr != "domain-name" {
		return nil, errInvalidParameter("Value (%s) for parameter attribute is invalid.", attr)
	}
	out := &describeAddressesAttributeResponse{}
	var err error
	s.store.Read(func(w *world) {
		var addresses []*address
		addresses, err = selectIDs(regional(w.Addresses, c.region), func(a *address) string { return a.AllocationID }, c.params.list("AllocationId"), notFoundByID)
		for _, a := range addresses {
			out.Addresses = append(out.Addresses, addressAttributeXML{a.PublicIP, a.AllocationID})
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

type associateAddressResponse struct {
	ec2Meta
	Return        bool   `xml:"return"`
	AssociationID string `xml:"associationId"`
}

// addressMove is the address list one machine gets after an association
// changes.
type addressMove struct {
	machine string
	addrs   []string
}

func (w *world) addressesOf(instanceID string) []string {
	var out []string
	for _, a := range w.Addresses {
		if a.InstanceID == instanceID {
			out = append(out, a.PublicIP)
		}
	}
	return out
}

func (s *Simulation) associateAddress(c *ec2Call) (ec2Response, error) {
	allocationID, instanceID := c.params.get("AllocationId"), c.params.get("InstanceId")
	switch {
	case allocationID == "":
		return nil, errMissingParameter("AllocationId")
	case instanceID == "":
		return nil, errUnsupported("Associating an address with anything but an instance")
	}
	reassociate, _ := c.params.boolean("AllowReassociation")
	out := &associateAddressResponse{Return: true}
	var moves []addressMove
	err := s.store.Update(func(w *world) error {
		a := w.address(c.region, allocationID)
		if a == nil {
			return notFoundByID(allocationID)
		}
		i := w.instance(instanceID)
		if i == nil || i.Region != c.region {
			return notFoundByID(instanceID)
		}
		if i.State != "running" || i.Machine == "" {
			return clientError("IncorrectInstanceState", "The instance '%s' is not in a valid state for this operation.", instanceID)
		}
		if a.InstanceID == instanceID {
			out.AssociationID = a.AssociationID
			return nil
		}
		if a.InstanceID != "" && !reassociate {
			return clientError("Resource.AlreadyAssociated", "resource %s is already associated with associate-id %s", allocationID, a.AssociationID)
		}
		if previous := w.instance(a.InstanceID); previous != nil && previous.Machine != "" {
			moves = append(moves, addressMove{previous.Machine, slices.DeleteFunc(w.addressesOf(previous.ID), func(ip string) bool { return ip == a.PublicIP })})
		}
		for _, other := range w.Addresses {
			if other.InstanceID == instanceID {
				other.InstanceID, other.AssociationID = "", ""
			}
		}
		a.InstanceID, a.AssociationID = instanceID, newID("eipassoc")
		moves = append(moves, addressMove{i.Machine, []string{a.PublicIP}})
		out.AssociationID = a.AssociationID
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.moveAddresses(c.ctx, moves); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Simulation) moveAddresses(ctx context.Context, moves []addressMove) error {
	for _, m := range moves {
		if err := s.machines.SetAddresses(ctx, m.machine, m.addrs); err != nil {
			return err
		}
	}
	return nil
}

func (s *Simulation) disassociateAddress(c *ec2Call) (ec2Response, error) {
	id := c.params.get("AssociationId")
	if id == "" {
		return nil, errMissingParameter("AssociationId")
	}
	var moves []addressMove
	err := s.store.Update(func(w *world) error {
		a := find(w.Addresses, func(a *address) bool { return a.Region == c.region && a.AssociationID == id })
		if a == nil {
			return clientError("InvalidAssociationID.NotFound", "The association ID '%s' does not exist", id)
		}
		if i := w.instance(a.InstanceID); i != nil && i.Machine != "" && i.State == "running" {
			moves = append(moves, addressMove{i.Machine, slices.DeleteFunc(w.addressesOf(i.ID), func(ip string) bool { return ip == a.PublicIP })})
		}
		a.InstanceID, a.AssociationID = "", ""
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := s.moveAddresses(c.ctx, moves); err != nil {
		return nil, err
	}
	return ok()
}

func (s *Simulation) releaseAddress(c *ec2Call) (ec2Response, error) {
	id := c.params.get("AllocationId")
	if id == "" {
		return nil, errMissingParameter("AllocationId")
	}
	err := s.store.Update(func(w *world) error {
		a := w.address(c.region, id)
		if a == nil {
			return notFoundByID(id)
		}
		if a.InstanceID != "" {
			return clientError("InvalidIPAddress.InUse", "Address %s is in use.", a.PublicIP)
		}
		w.Addresses = slices.DeleteFunc(w.Addresses, func(x *address) bool { return x == a })
		w.forget(id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

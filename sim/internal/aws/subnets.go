package aws

import (
	"net/netip"
	"slices"
	"strconv"
)

type subnet struct {
	ID           string `json:"id"`
	Region       string `json:"region"`
	VPCID        string `json:"vpc_id"`
	CIDR         string `json:"cidr"`
	AZ           string `json:"az"`
	DefaultForAZ bool   `json:"default_for_az"`
	MapPublicIP  bool   `json:"map_public_ip"`
}

func (s *Simulation) ec2SubnetActions() map[string]ec2Action {
	return map[string]ec2Action{
		"CreateSubnet":          {s.createSubnet, []string{"VpcId", "CidrBlock", "AvailabilityZone", "AvailabilityZoneId", "TagSpecification"}},
		"DescribeSubnets":       {s.describeSubnets, []string{"SubnetId", "Filter", "MaxResults", "NextToken"}},
		"DeleteSubnet":          {s.deleteSubnet, []string{"SubnetId"}},
		"ModifySubnetAttribute": {s.modifySubnetAttribute, []string{"SubnetId", "MapPublicIpOnLaunch", "AssignIpv6AddressOnCreation", "EnableDns64", "EnableResourceNameDnsARecordOnLaunch", "EnableResourceNameDnsAAAARecordOnLaunch", "PrivateDnsHostnameTypeOnLaunch"}},
	}
}

type privateDNSNameOptionsXML struct {
	HostnameType                    string `xml:"hostnameType"`
	EnableResourceNameDNSARecord    bool   `xml:"enableResourceNameDnsARecord"`
	EnableResourceNameDNSAAAARecord bool   `xml:"enableResourceNameDnsAAAARecord"`
}

type subnetXML struct {
	SubnetID                    string                   `xml:"subnetId"`
	SubnetARN                   string                   `xml:"subnetArn"`
	State                       string                   `xml:"state"`
	OwnerID                     string                   `xml:"ownerId"`
	VPCID                       string                   `xml:"vpcId"`
	CIDRBlock                   string                   `xml:"cidrBlock"`
	AvailableIPAddressCount     int                      `xml:"availableIpAddressCount"`
	AvailabilityZone            string                   `xml:"availabilityZone"`
	AvailabilityZoneID          string                   `xml:"availabilityZoneId"`
	DefaultForAZ                bool                     `xml:"defaultForAz"`
	MapPublicIPOnLaunch         bool                     `xml:"mapPublicIpOnLaunch"`
	MapCustomerOwnedIPOnLaunch  bool                     `xml:"mapCustomerOwnedIpOnLaunch"`
	AssignIPv6AddressOnCreation bool                     `xml:"assignIpv6AddressOnCreation"`
	EnableDNS64                 bool                     `xml:"enableDns64"`
	IPv6Native                  bool                     `xml:"ipv6Native"`
	PrivateDNSNameOptions       privateDNSNameOptionsXML `xml:"privateDnsNameOptionsOnLaunch"`
	TagSet                      []tag                    `xml:"tagSet>item"`
}

func renderSubnet(w *world, sn *subnet) subnetXML {
	prefix := netip.MustParsePrefix(sn.CIDR)
	used := 0
	for _, i := range w.Instances {
		if i.SubnetID == sn.ID && i.State != "terminated" {
			used++
		}
	}
	return subnetXML{
		SubnetID: sn.ID, SubnetARN: "arn:aws:ec2:" + sn.Region + ":" + w.AccountID + ":subnet/" + sn.ID, State: "available",
		OwnerID: w.AccountID, VPCID: sn.VPCID, CIDRBlock: sn.CIDR, AvailableIPAddressCount: 1<<(32-prefix.Bits()) - 5 - used,
		AvailabilityZone: sn.AZ, AvailabilityZoneID: zoneID(sn.AZ), DefaultForAZ: sn.DefaultForAZ, MapPublicIPOnLaunch: sn.MapPublicIP,
		PrivateDNSNameOptions: privateDNSNameOptionsXML{HostnameType: "ip-name"}, TagSet: w.tagsOf(sn.ID),
	}
}

type createSubnetResponse struct {
	ec2Meta
	Subnet subnetXML `xml:"subnet"`
}

func (s *Simulation) createSubnet(c *ec2Call) (ec2Response, error) {
	vpcID, cidr := c.params.get("VpcId"), c.params.get("CidrBlock")
	if vpcID == "" {
		return nil, errMissingParameter("VpcId")
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil || !prefix.Addr().Is4() || prefix.Masked() != prefix {
		return nil, errInvalidParameter("Value (%s) for parameter cidrBlock is invalid. This is not a valid CIDR block.", cidr)
	}
	if prefix.Bits() < 16 || prefix.Bits() > 28 {
		return nil, clientError("InvalidSubnet.Range", "The CIDR '%s' is invalid.", cidr)
	}
	zone, err := s.chooseZone(c)
	if err != nil {
		return nil, err
	}
	specs, err := tagSpecifications(c.params, "subnet")
	if err != nil {
		return nil, err
	}
	out := &createSubnetResponse{}
	err = s.store.Update(func(w *world) error {
		v := w.vpc(c.region, vpcID)
		if v == nil {
			return notFoundByID(vpcID)
		}
		if !netip.MustParsePrefix(v.CIDR).Contains(prefix.Addr()) || netip.MustParsePrefix(v.CIDR).Bits() > prefix.Bits() {
			return clientError("InvalidSubnet.Range", "The CIDR '%s' is invalid.", cidr)
		}
		for _, other := range w.Subnets {
			if other.VPCID == vpcID && netip.MustParsePrefix(other.CIDR).Overlaps(prefix) {
				return clientError("InvalidSubnet.Conflict", "The CIDR '%s' conflicts with another subnet", cidr)
			}
		}
		sn := &subnet{ID: newID("subnet"), Region: c.region, VPCID: vpcID, CIDR: cidr, AZ: zone}
		w.Subnets = append(w.Subnets, sn)
		w.associateDefaultACL(sn)
		for _, t := range specs["subnet"] {
			w.setTag(sn.ID, t)
		}
		out.Subnet = renderSubnet(w, sn)
		out.Subnet.State = "pending"
		return nil
	})
	return out, err
}

// chooseZone returns the zone a request names, by name or id, or the
// region's first zone.
func (s *Simulation) chooseZone(c *ec2Call) (string, error) {
	zones := s.catalog.zones(c.region)
	if name := c.params.get("AvailabilityZone"); name != "" {
		if !slices.Contains(zones, name) {
			return "", errInvalidParameter("Value (%s) for parameter availabilityZone is invalid. Subnets can currently only be created in the following availability zones: %v.", name, zones)
		}
		return name, nil
	}
	if id := c.params.get("AvailabilityZoneId"); id != "" {
		i := slices.IndexFunc(zones, func(z string) bool { return zoneID(z) == id })
		if i < 0 {
			return "", errInvalidParameter("Value (%s) for parameter availabilityZoneId is invalid.", id)
		}
		return zones[i], nil
	}
	return zones[0], nil
}

type describeSubnetsResponse struct {
	ec2Meta
	Subnets []subnetXML `xml:"subnetSet>item"`
}

func (s *Simulation) describeSubnets(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	out := &describeSubnetsResponse{}
	var err error
	s.store.Read(func(w *world) {
		var subnets []*subnet
		subnets, err = selectIDs(regional(w.Subnets, c.region), func(sn *subnet) string { return sn.ID }, c.params.list("SubnetId"), notFoundByID)
		for _, sn := range subnets {
			if err != nil {
				return
			}
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "subnet-id":
					return values(sn.ID), true
				case "vpc-id":
					return values(sn.VPCID), true
				case "availability-zone", "availabilityZone":
					return values(sn.AZ), true
				case "availability-zone-id":
					return values(zoneID(sn.AZ)), true
				case "cidr-block", "cidr", "cidrBlock":
					return values(sn.CIDR), true
				case "default-for-az", "defaultForAz":
					return boolString(sn.DefaultForAZ), true
				case "map-public-ip-on-launch":
					return boolString(sn.MapPublicIP), true
				case "state":
					return values("available"), true
				case "owner-id":
					return values(w.AccountID), true
				}
				return nil, false
			}, w.tagsOf(sn.ID))
			if match {
				out.Subnets = append(out.Subnets, renderSubnet(w, sn))
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Simulation) deleteSubnet(c *ec2Call) (ec2Response, error) {
	id := c.params.get("SubnetId")
	err := s.store.Update(func(w *world) error {
		sn := w.subnet(c.region, id)
		if sn == nil {
			return notFoundByID(id)
		}
		if slices.ContainsFunc(w.Instances, func(i *instance) bool { return i.SubnetID == id && i.State != "terminated" }) {
			return clientError("DependencyViolation", "The subnet '%s' has dependencies and cannot be deleted.", id)
		}
		for _, t := range w.RouteTables {
			t.Associations = slices.DeleteFunc(t.Associations, func(a routeAssociation) bool { return a.SubnetID == id })
		}
		for _, a := range w.NetworkACLs {
			a.Associations = slices.DeleteFunc(a.Associations, func(as aclAssociation) bool { return as.SubnetID == id })
		}
		w.Subnets = slices.DeleteFunc(w.Subnets, func(x *subnet) bool { return x == sn })
		w.forget(id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

func (s *Simulation) modifySubnetAttribute(c *ec2Call) (ec2Response, error) {
	id := c.params.get("SubnetId")
	for _, name := range []string{"AssignIpv6AddressOnCreation", "EnableDns64", "EnableResourceNameDnsARecordOnLaunch", "EnableResourceNameDnsAAAARecordOnLaunch"} {
		if v, _ := c.params.boolean(name + ".Value"); v {
			return nil, errUnsupported("Subnet attribute %s", name)
		}
	}
	if v := c.params.get("PrivateDnsHostnameTypeOnLaunch"); v != "" && v != "ip-name" {
		return nil, errUnsupported("PrivateDnsHostnameTypeOnLaunch %s", strconv.Quote(v))
	}
	err := s.store.Update(func(w *world) error {
		sn := w.subnet(c.region, id)
		if sn == nil {
			return notFoundByID(id)
		}
		if v, ok := c.params.boolean("MapPublicIpOnLaunch.Value"); ok {
			sn.MapPublicIP = v
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

package aws

import (
	"net/netip"
	"slices"
)

type vpc struct {
	ID                 string `json:"id"`
	Region             string `json:"region"`
	CIDR               string `json:"cidr"`
	CIDRAssociationID  string `json:"cidr_association_id"`
	Default            bool   `json:"default"`
	DHCPOptionsID      string `json:"dhcp_options_id"`
	Tenancy            string `json:"tenancy"`
	EnableDNSSupport   bool   `json:"enable_dns_support"`
	EnableDNSHostnames bool   `json:"enable_dns_hostnames"`
	EnableNAUMetrics   bool   `json:"enable_nau_metrics"`
}

type networkACL struct {
	ID           string           `json:"id"`
	Region       string           `json:"region"`
	VPCID        string           `json:"vpc_id"`
	Default      bool             `json:"default"`
	Associations []aclAssociation `json:"associations"`
}

type aclAssociation struct {
	ID       string `json:"id"`
	SubnetID string `json:"subnet_id"`
}

func (w *world) networkACL(region, id string) *networkACL {
	return find(w.NetworkACLs, func(a *networkACL) bool { return a.ID == id && a.Region == region })
}

func (w *world) defaultACL(vpcID string) *networkACL {
	return find(w.NetworkACLs, func(a *networkACL) bool { return a.VPCID == vpcID && a.Default })
}

func (s *Simulation) ec2VPCActions() map[string]ec2Action {
	return map[string]ec2Action{
		"CreateVpc":            {s.createVPC, []string{"CidrBlock", "InstanceTenancy", "AmazonProvidedIpv6CidrBlock", "TagSpecification"}},
		"DescribeVpcs":         {s.describeVPCs, []string{"VpcId", "Filter", "MaxResults", "NextToken"}},
		"DeleteVpc":            {s.deleteVPC, []string{"VpcId"}},
		"DescribeVpcAttribute": {s.describeVPCAttribute, []string{"VpcId", "Attribute"}},
		"ModifyVpcAttribute":   {s.modifyVPCAttribute, []string{"VpcId", "EnableDnsSupport", "EnableDnsHostnames", "EnableNetworkAddressUsageMetrics"}},
		"DescribeNetworkAcls":  {s.describeNetworkACLs, []string{"NetworkAclId", "Filter", "MaxResults", "NextToken"}},
	}
}

// createDefaultVPC creates a region's default VPC as AWS does for a new
// account: 172.31.0.0/16 with a default subnet in each zone that gives
// instances public addresses, an internet gateway and a route to it.
func (s *Simulation) createDefaultVPC(w *world, region string) {
	v := s.newVPC(w, region, "172.31.0.0/16", "default")
	v.Default, v.EnableDNSHostnames = true, true
	igw := &internetGateway{ID: newID("igw"), Region: region, VPCID: v.ID}
	w.InternetGateways = append(w.InternetGateways, igw)
	main := w.mainRouteTable(v.ID)
	main.Routes = append(main.Routes, route{Destination: "0.0.0.0/0", GatewayID: igw.ID, Origin: "CreateRoute"})
	for i, zone := range s.catalog.zones(region) {
		prefix := netip.PrefixFrom(netip.AddrFrom4([4]byte{172, 31, byte(16 * i), 0}), 20)
		sn := &subnet{ID: newID("subnet"), Region: region, VPCID: v.ID, CIDR: prefix.String(), AZ: zone, DefaultForAZ: true, MapPublicIP: true}
		w.Subnets = append(w.Subnets, sn)
		w.associateDefaultACL(sn)
	}
}

// newVPC creates a VPC with what AWS gives every VPC: a main route table
// with the local route, a default security group and a default network ACL.
func (s *Simulation) newVPC(w *world, region, cidr, tenancy string) *vpc {
	v := &vpc{
		ID: newID("vpc"), Region: region, CIDR: cidr, CIDRAssociationID: newID("vpc-cidr-assoc"),
		DHCPOptionsID: s.dhcpOptionsID(w, region), Tenancy: tenancy, EnableDNSSupport: true,
	}
	w.VPCs = append(w.VPCs, v)
	w.RouteTables = append(w.RouteTables, &routeTable{
		ID: newID("rtb"), Region: region, VPCID: v.ID,
		Routes:       []route{{Destination: cidr, GatewayID: "local", Origin: "CreateRouteTable"}},
		Associations: []routeAssociation{{ID: newID("rtbassoc"), Main: true}},
	})
	sg := &securityGroup{ID: newID("sg"), Region: region, VPCID: v.ID, Name: "default", Description: "default VPC security group"}
	sg.Ingress = []permission{{Protocol: "-1", Groups: []string{sg.ID}}}
	sg.Egress = []permission{allowAllEgress()}
	w.SecurityGroups = append(w.SecurityGroups, sg)
	w.NetworkACLs = append(w.NetworkACLs, &networkACL{ID: newID("acl"), Region: region, VPCID: v.ID, Default: true})
	return v
}

// dhcpOptionsID returns the region's default DHCP options set, which every
// VPC uses.
func (s *Simulation) dhcpOptionsID(w *world, region string) string {
	for _, v := range w.VPCs {
		if v.Region == region {
			return v.DHCPOptionsID
		}
	}
	return newID("dopt")
}

func (w *world) associateDefaultACL(sn *subnet) {
	if acl := w.defaultACL(sn.VPCID); acl != nil {
		acl.Associations = append(acl.Associations, aclAssociation{ID: newID("aclassoc"), SubnetID: sn.ID})
	}
}

type cidrAssociationXML struct {
	AssociationID  string     `xml:"associationId"`
	CIDRBlock      string     `xml:"cidrBlock"`
	CIDRBlockState stateValue `xml:"cidrBlockState"`
}

type stateValue struct {
	State string `xml:"state"`
}

type vpcXML struct {
	VPCID           string               `xml:"vpcId"`
	OwnerID         string               `xml:"ownerId"`
	State           string               `xml:"state"`
	CIDRBlock       string               `xml:"cidrBlock"`
	CIDRAssociation []cidrAssociationXML `xml:"cidrBlockAssociationSet>item"`
	DHCPOptionsID   string               `xml:"dhcpOptionsId"`
	InstanceTenancy string               `xml:"instanceTenancy"`
	IsDefault       bool                 `xml:"isDefault"`
	TagSet          []tag                `xml:"tagSet>item"`
}

func renderVPC(w *world, v *vpc, state string) vpcXML {
	return vpcXML{
		VPCID: v.ID, OwnerID: w.AccountID, State: state, CIDRBlock: v.CIDR,
		CIDRAssociation: []cidrAssociationXML{{v.CIDRAssociationID, v.CIDR, stateValue{"associated"}}},
		DHCPOptionsID:   v.DHCPOptionsID, InstanceTenancy: v.Tenancy, IsDefault: v.Default, TagSet: w.tagsOf(v.ID),
	}
}

type createVPCResponse struct {
	ec2Meta
	VPC vpcXML `xml:"vpc"`
}

func (s *Simulation) createVPC(c *ec2Call) (ec2Response, error) {
	prefix, err := netip.ParsePrefix(c.params.get("CidrBlock"))
	switch {
	case c.params.get("CidrBlock") == "":
		return nil, errMissingParameter("CidrBlock")
	case err != nil || !prefix.Addr().Is4() || prefix.Masked() != prefix:
		return nil, errInvalidParameter("Value (%s) for parameter cidrBlock is invalid. This is not a valid CIDR block.", c.params.get("CidrBlock"))
	case prefix.Bits() < 16 || prefix.Bits() > 28:
		return nil, clientError("InvalidVpc.Range", "The CIDR '%s' is invalid.", prefix)
	}
	if v, _ := c.params.boolean("AmazonProvidedIpv6CidrBlock"); v {
		return nil, errUnsupported("IPv6")
	}
	tenancy := c.params.get("InstanceTenancy")
	if tenancy == "" {
		tenancy = "default"
	}
	if tenancy != "default" {
		return nil, errUnsupported("Instance tenancy %q", tenancy)
	}
	specs, err := tagSpecifications(c.params, "vpc")
	if err != nil {
		return nil, err
	}
	out := &createVPCResponse{}
	err = s.store.Update(func(w *world) error {
		v := s.newVPC(w, c.region, prefix.String(), tenancy)
		for _, t := range specs["vpc"] {
			w.setTag(v.ID, t)
		}
		out.VPC = renderVPC(w, v, "pending")
		return nil
	})
	return out, err
}

type describeVPCsResponse struct {
	ec2Meta
	VPCs []vpcXML `xml:"vpcSet>item"`
}

func (s *Simulation) describeVPCs(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	out := &describeVPCsResponse{}
	var err error
	s.store.Read(func(w *world) {
		var vpcs []*vpc
		vpcs, err = selectIDs(regional(w.VPCs, c.region), func(v *vpc) string { return v.ID }, c.params.list("VpcId"), notFoundByID)
		for _, v := range vpcs {
			if err != nil {
				return
			}
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "vpc-id":
					return values(v.ID), true
				case "cidr", "cidr-block-association.cidr-block", "cidrBlock":
					return values(v.CIDR), true
				case "is-default", "isDefault":
					return boolString(v.Default), true
				case "state":
					return values("available"), true
				case "owner-id":
					return values(w.AccountID), true
				case "dhcp-options-id":
					return values(v.DHCPOptionsID), true
				}
				return nil, false
			}, w.tagsOf(v.ID))
			if match {
				out.VPCs = append(out.VPCs, renderVPC(w, v, "available"))
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Simulation) deleteVPC(c *ec2Call) (ec2Response, error) {
	id := c.params.get("VpcId")
	err := s.store.Update(func(w *world) error {
		v := w.vpc(c.region, id)
		if v == nil {
			return notFoundByID(id)
		}
		dependent := slices.ContainsFunc(w.Subnets, func(sn *subnet) bool { return sn.VPCID == id }) ||
			slices.ContainsFunc(w.InternetGateways, func(g *internetGateway) bool { return g.VPCID == id }) ||
			slices.ContainsFunc(w.SecurityGroups, func(g *securityGroup) bool { return g.VPCID == id && g.Name != "default" }) ||
			slices.ContainsFunc(w.RouteTables, func(t *routeTable) bool { return t.VPCID == id && !t.main() })
		if dependent {
			return clientError("DependencyViolation", "The vpc '%s' has dependencies and cannot be deleted.", id)
		}
		for _, t := range w.RouteTables {
			if t.VPCID == id {
				w.forget(t.ID)
			}
		}
		for _, g := range w.SecurityGroups {
			if g.VPCID == id {
				w.forget(g.ID)
			}
		}
		for _, a := range w.NetworkACLs {
			if a.VPCID == id {
				w.forget(a.ID)
			}
		}
		w.RouteTables = slices.DeleteFunc(w.RouteTables, func(t *routeTable) bool { return t.VPCID == id })
		w.SecurityGroups = slices.DeleteFunc(w.SecurityGroups, func(g *securityGroup) bool { return g.VPCID == id })
		w.NetworkACLs = slices.DeleteFunc(w.NetworkACLs, func(a *networkACL) bool { return a.VPCID == id })
		w.VPCs = slices.DeleteFunc(w.VPCs, func(x *vpc) bool { return x == v })
		w.forget(id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

type attributeBool struct {
	Value bool `xml:"value"`
}

type describeVPCAttributeResponse struct {
	ec2Meta
	VPCID              string         `xml:"vpcId"`
	EnableDNSSupport   *attributeBool `xml:"enableDnsSupport,omitempty"`
	EnableDNSHostnames *attributeBool `xml:"enableDnsHostnames,omitempty"`
	EnableNAUMetrics   *attributeBool `xml:"enableNetworkAddressUsageMetrics,omitempty"`
}

func (s *Simulation) describeVPCAttribute(c *ec2Call) (ec2Response, error) {
	id, attribute := c.params.get("VpcId"), c.params.get("Attribute")
	out := &describeVPCAttributeResponse{VPCID: id}
	var found bool
	s.store.Read(func(w *world) {
		v := w.vpc(c.region, id)
		if found = v != nil; !found {
			return
		}
		switch attribute {
		case "enableDnsSupport":
			out.EnableDNSSupport = &attributeBool{v.EnableDNSSupport}
		case "enableDnsHostnames":
			out.EnableDNSHostnames = &attributeBool{v.EnableDNSHostnames}
		case "enableNetworkAddressUsageMetrics":
			out.EnableNAUMetrics = &attributeBool{v.EnableNAUMetrics}
		}
	})
	if !found {
		return nil, notFoundByID(id)
	}
	if out.EnableDNSSupport == nil && out.EnableDNSHostnames == nil && out.EnableNAUMetrics == nil {
		return nil, errInvalidParameter("Value (%s) for parameter attribute is invalid. Unknown attribute.", attribute)
	}
	return out, nil
}

func (s *Simulation) modifyVPCAttribute(c *ec2Call) (ec2Response, error) {
	id := c.params.get("VpcId")
	err := s.store.Update(func(w *world) error {
		v := w.vpc(c.region, id)
		if v == nil {
			return notFoundByID(id)
		}
		if b, ok := c.params.boolean("EnableDnsSupport.Value"); ok {
			v.EnableDNSSupport = b
		}
		if b, ok := c.params.boolean("EnableDnsHostnames.Value"); ok {
			v.EnableDNSHostnames = b
		}
		if b, ok := c.params.boolean("EnableNetworkAddressUsageMetrics.Value"); ok {
			v.EnableNAUMetrics = b
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ok()
}

type aclEntryXML struct {
	RuleNumber int    `xml:"ruleNumber"`
	Protocol   string `xml:"protocol"`
	RuleAction string `xml:"ruleAction"`
	Egress     bool   `xml:"egress"`
	CIDRBlock  string `xml:"cidrBlock"`
}

type aclAssociationXML struct {
	ID           string `xml:"networkAclAssociationId"`
	NetworkACLID string `xml:"networkAclId"`
	SubnetID     string `xml:"subnetId"`
}

type networkACLXML struct {
	NetworkACLID string              `xml:"networkAclId"`
	VPCID        string              `xml:"vpcId"`
	Default      bool                `xml:"default"`
	OwnerID      string              `xml:"ownerId"`
	Entries      []aclEntryXML       `xml:"entrySet>item"`
	Associations []aclAssociationXML `xml:"associationSet>item"`
	TagSet       []tag               `xml:"tagSet>item"`
}

type describeNetworkACLsResponse struct {
	ec2Meta
	ACLs []networkACLXML `xml:"networkAclSet>item"`
}

// describeNetworkACLs lists network ACLs. Every ACL allows all traffic, as a
// default ACL does; the simulation has no API to change them.
func (s *Simulation) describeNetworkACLs(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	out := &describeNetworkACLsResponse{}
	var err error
	s.store.Read(func(w *world) {
		var acls []*networkACL
		acls, err = selectIDs(regional(w.NetworkACLs, c.region), func(a *networkACL) string { return a.ID }, c.params.list("NetworkAclId"), notFoundByID)
		for _, a := range acls {
			if err != nil {
				return
			}
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "network-acl-id":
					return values(a.ID), true
				case "vpc-id":
					return values(a.VPCID), true
				case "default":
					return boolString(a.Default), true
				case "association.subnet-id":
					var ids []string
					for _, as := range a.Associations {
						ids = append(ids, as.SubnetID)
					}
					return ids, true
				case "owner-id":
					return values(w.AccountID), true
				}
				return nil, false
			}, w.tagsOf(a.ID))
			if !match {
				continue
			}
			x := networkACLXML{NetworkACLID: a.ID, VPCID: a.VPCID, Default: a.Default, OwnerID: w.AccountID, TagSet: w.tagsOf(a.ID)}
			for _, egress := range []bool{true, false} {
				x.Entries = append(x.Entries,
					aclEntryXML{RuleNumber: 100, Protocol: "-1", RuleAction: "allow", Egress: egress, CIDRBlock: "0.0.0.0/0"},
					aclEntryXML{RuleNumber: 32767, Protocol: "-1", RuleAction: "deny", Egress: egress, CIDRBlock: "0.0.0.0/0"})
			}
			for _, as := range a.Associations {
				x.Associations = append(x.Associations, aclAssociationXML{as.ID, a.ID, as.SubnetID})
			}
			out.ACLs = append(out.ACLs, x)
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// regional is a resource that lives in one region.
type regionalResource interface {
	regionName() string
}

func (v *vpc) regionName() string             { return v.Region }
func (a *networkACL) regionName() string      { return a.Region }
func (s *subnet) regionName() string          { return s.Region }
func (g *internetGateway) regionName() string { return g.Region }
func (t *routeTable) regionName() string      { return t.Region }
func (g *securityGroup) regionName() string   { return g.Region }
func (a *address) regionName() string         { return a.Region }
func (i *image) regionName() string           { return i.Region }
func (s *snapshot) regionName() string        { return s.Region }
func (v *volume) regionName() string          { return v.Region }
func (k *keyPair) regionName() string         { return k.Region }
func (i *instance) regionName() string        { return i.Region }

// regional returns the items in a region.
func regional[T regionalResource](items []T, region string) []T {
	var out []T
	for _, item := range items {
		if item.regionName() == region {
			out = append(out, item)
		}
	}
	return out
}

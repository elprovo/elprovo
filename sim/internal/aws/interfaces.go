package aws

func (s *Simulation) ec2InterfaceActions() map[string]ec2Action {
	return map[string]ec2Action{
		"DescribeNetworkInterfaces": {s.describeNetworkInterfaces, []string{"NetworkInterfaceId", "Filter", "MaxResults", "NextToken"}},
	}
}

type eniAttachmentXML struct {
	AttachmentID        string `xml:"attachmentId"`
	InstanceID          string `xml:"instanceId"`
	InstanceOwnerID     string `xml:"instanceOwnerId"`
	DeviceIndex         int    `xml:"deviceIndex"`
	NetworkCardIndex    int    `xml:"networkCardIndex"`
	Status              string `xml:"status"`
	AttachTime          string `xml:"attachTime"`
	DeleteOnTermination bool   `xml:"deleteOnTermination"`
}

type eniAssociationXML struct {
	PublicIP      string `xml:"publicIp"`
	PublicDNSName string `xml:"publicDnsName"`
	IPOwnerID     string `xml:"ipOwnerId"`
	AllocationID  string `xml:"allocationId,omitempty"`
	AssociationID string `xml:"associationId,omitempty"`
}

type eniPrivateAddressXML struct {
	PrivateIPAddress string             `xml:"privateIpAddress"`
	PrivateDNSName   string             `xml:"privateDnsName"`
	Primary          bool               `xml:"primary"`
	Association      *eniAssociationXML `xml:"association,omitempty"`
}

type networkInterfaceXML struct {
	NetworkInterfaceID string                 `xml:"networkInterfaceId"`
	SubnetID           string                 `xml:"subnetId"`
	VPCID              string                 `xml:"vpcId"`
	AvailabilityZone   string                 `xml:"availabilityZone"`
	Description        string                 `xml:"description"`
	OwnerID            string                 `xml:"ownerId"`
	RequesterManaged   bool                   `xml:"requesterManaged"`
	Status             string                 `xml:"status"`
	MACAddress         string                 `xml:"macAddress"`
	PrivateIPAddress   string                 `xml:"privateIpAddress"`
	PrivateDNSName     string                 `xml:"privateDnsName"`
	SourceDestCheck    bool                   `xml:"sourceDestCheck"`
	InterfaceType      string                 `xml:"interfaceType"`
	GroupSet           []groupIdentifierXML   `xml:"groupSet>item"`
	Attachment         eniAttachmentXML       `xml:"attachment"`
	Association        *eniAssociationXML     `xml:"association,omitempty"`
	PrivateIPAddresses []eniPrivateAddressXML `xml:"privateIpAddressesSet>item"`
	TagSet             []tag                  `xml:"tagSet>item"`
}

func renderNetworkInterface(w *world, i *instance) networkInterfaceXML {
	public := w.publicIP(i)
	privateDNS, publicDNS := instanceDNS(i.Region, i.PrivateIP, public)
	out := networkInterfaceXML{
		NetworkInterfaceID: i.ENIID, SubnetID: i.SubnetID, VPCID: i.VPCID, AvailabilityZone: i.AZ, OwnerID: w.AccountID,
		Status: "in-use", MACAddress: i.MAC, PrivateIPAddress: i.PrivateIP, PrivateDNSName: privateDNS,
		SourceDestCheck: i.SourceDestCheck, InterfaceType: "interface", GroupSet: renderGroups(w, i), TagSet: w.tagsOf(i.ENIID),
		Attachment: eniAttachmentXML{
			AttachmentID: i.ENIAttachmentID, InstanceID: i.ID, InstanceOwnerID: w.AccountID, Status: "attached",
			AttachTime: timestamp(i.LaunchTime), DeleteOnTermination: true,
		},
	}
	if public != "" {
		out.Association = &eniAssociationXML{PublicIP: public, PublicDNSName: publicDNS, IPOwnerID: "amazon"}
		if a := find(w.Addresses, func(a *address) bool { return a.InstanceID == i.ID }); a != nil {
			out.Association.IPOwnerID, out.Association.AllocationID, out.Association.AssociationID = w.AccountID, a.AllocationID, a.AssociationID
		}
	}
	if i.PrivateIP != "" {
		out.PrivateIPAddresses = []eniPrivateAddressXML{{i.PrivateIP, privateDNS, true, out.Association}}
	}
	return out
}

type describeNetworkInterfacesResponse struct {
	ec2Meta
	NetworkInterfaces []networkInterfaceXML `xml:"networkInterfaceSet>item"`
}

// describeNetworkInterfaces lists the instances' network interfaces. Each
// instance has exactly one, its primary interface, which lives and dies with
// the instance.
func (s *Simulation) describeNetworkInterfaces(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	out := &describeNetworkInterfacesResponse{}
	var err error
	s.store.Read(func(w *world) {
		var live []*instance
		for _, i := range regional(w.Instances, c.region) {
			if i.State != "terminated" {
				live = append(live, i)
			}
		}
		live, err = selectIDs(live, func(i *instance) string { return i.ENIID }, c.params.list("NetworkInterfaceId"), func(id string) *apiError {
			return errNotFound("InvalidNetworkInterfaceID.NotFound", "The networkInterface ID '%s' does not exist", id)
		})
		for _, i := range live {
			if err != nil {
				return
			}
			x := renderNetworkInterface(w, i)
			var match bool
			match, err = matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "network-interface-id":
					return values(x.NetworkInterfaceID), true
				case "subnet-id":
					return values(x.SubnetID), true
				case "vpc-id":
					return values(x.VPCID), true
				case "group-id":
					return i.SecurityGroupIDs, true
				case "attachment.instance-id":
					return values(i.ID), true
				case "availability-zone":
					return values(i.AZ), true
				case "description":
					return values(x.Description), true
				case "status":
					return values(x.Status), true
				case "private-ip-address":
					return values(x.PrivateIPAddress), true
				case "requester-managed":
					return boolString(false), true
				case "interface-type":
					return values(x.InterfaceType), true
				case "owner-id":
					return values(w.AccountID), true
				}
				return nil, false
			}, w.tagsOf(i.ENIID))
			if match {
				out.NetworkInterfaces = append(out.NetworkInterfaces, x)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

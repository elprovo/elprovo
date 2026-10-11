package aws

type instanceStateXML struct {
	Code int    `xml:"code"`
	Name string `xml:"name"`
}

type placementXML struct {
	AvailabilityZone   string `xml:"availabilityZone"`
	AvailabilityZoneID string `xml:"availabilityZoneId"`
	GroupName          string `xml:"groupName"`
	Tenancy            string `xml:"tenancy"`
}

type groupIdentifierXML struct {
	GroupID   string `xml:"groupId"`
	GroupName string `xml:"groupName"`
}

type stateReasonXML struct {
	Code    string `xml:"code"`
	Message string `xml:"message"`
}

type ebsInstanceDeviceXML struct {
	VolumeID            string `xml:"volumeId"`
	Status              string `xml:"status"`
	AttachTime          string `xml:"attachTime"`
	DeleteOnTermination bool   `xml:"deleteOnTermination"`
}

type instanceBlockDeviceXML struct {
	DeviceName string               `xml:"deviceName"`
	EBS        ebsInstanceDeviceXML `xml:"ebs"`
}

type interfaceAssociationXML struct {
	PublicIP      string `xml:"publicIp"`
	PublicDNSName string `xml:"publicDnsName"`
	IPOwnerID     string `xml:"ipOwnerId"`
}

type interfaceAttachmentXML struct {
	AttachmentID        string `xml:"attachmentId"`
	DeviceIndex         int    `xml:"deviceIndex"`
	NetworkCardIndex    int    `xml:"networkCardIndex"`
	Status              string `xml:"status"`
	AttachTime          string `xml:"attachTime"`
	DeleteOnTermination bool   `xml:"deleteOnTermination"`
}

type privateAddressXML struct {
	PrivateIPAddress string                   `xml:"privateIpAddress"`
	PrivateDNSName   string                   `xml:"privateDnsName"`
	Primary          bool                     `xml:"primary"`
	Association      *interfaceAssociationXML `xml:"association,omitempty"`
}

type instanceNetworkInterfaceXML struct {
	NetworkInterfaceID string                   `xml:"networkInterfaceId"`
	SubnetID           string                   `xml:"subnetId"`
	VPCID              string                   `xml:"vpcId"`
	Description        string                   `xml:"description"`
	OwnerID            string                   `xml:"ownerId"`
	Status             string                   `xml:"status"`
	MACAddress         string                   `xml:"macAddress"`
	PrivateIPAddress   string                   `xml:"privateIpAddress"`
	PrivateDNSName     string                   `xml:"privateDnsName"`
	SourceDestCheck    bool                     `xml:"sourceDestCheck"`
	InterfaceType      string                   `xml:"interfaceType"`
	GroupSet           []groupIdentifierXML     `xml:"groupSet>item"`
	Attachment         interfaceAttachmentXML   `xml:"attachment"`
	Association        *interfaceAssociationXML `xml:"association,omitempty"`
	PrivateIPAddresses []privateAddressXML      `xml:"privateIpAddressesSet>item"`
}

type monitoringXML struct {
	State string `xml:"state"`
}

type cpuOptionsXML struct {
	CoreCount      int `xml:"coreCount"`
	ThreadsPerCore int `xml:"threadsPerCore"`
}

type capacityReservationXML struct {
	Preference string `xml:"capacityReservationPreference"`
}

type configuredXML struct {
	Configured bool `xml:"configured"`
}

type enabledXML struct {
	Enabled bool `xml:"enabled"`
}

type metadataOptionsXML struct {
	State                   string `xml:"state"`
	HTTPTokens              string `xml:"httpTokens"`
	HTTPPutResponseHopLimit int    `xml:"httpPutResponseHopLimit"`
	HTTPEndpoint            string `xml:"httpEndpoint"`
	HTTPProtocolIPv6        string `xml:"httpProtocolIpv6"`
	InstanceMetadataTags    string `xml:"instanceMetadataTags"`
}

type maintenanceOptionsXML struct {
	AutoRecovery string `xml:"autoRecovery"`
}

type instanceXML struct {
	InstanceID               string                        `xml:"instanceId"`
	ImageID                  string                        `xml:"imageId"`
	InstanceState            instanceStateXML              `xml:"instanceState"`
	PrivateDNSName           string                        `xml:"privateDnsName"`
	DNSName                  string                        `xml:"dnsName"`
	Reason                   string                        `xml:"reason"`
	KeyName                  string                        `xml:"keyName,omitempty"`
	AMILaunchIndex           int                           `xml:"amiLaunchIndex"`
	InstanceType             string                        `xml:"instanceType"`
	LaunchTime               string                        `xml:"launchTime"`
	Placement                placementXML                  `xml:"placement"`
	Monitoring               monitoringXML                 `xml:"monitoring"`
	SubnetID                 string                        `xml:"subnetId,omitempty"`
	VPCID                    string                        `xml:"vpcId,omitempty"`
	PrivateIPAddress         string                        `xml:"privateIpAddress,omitempty"`
	IPAddress                string                        `xml:"ipAddress,omitempty"`
	SourceDestCheck          bool                          `xml:"sourceDestCheck"`
	GroupSet                 []groupIdentifierXML          `xml:"groupSet>item"`
	StateReason              *stateReasonXML               `xml:"stateReason,omitempty"`
	Architecture             string                        `xml:"architecture"`
	RootDeviceType           string                        `xml:"rootDeviceType"`
	RootDeviceName           string                        `xml:"rootDeviceName"`
	BlockDeviceMapping       []instanceBlockDeviceXML      `xml:"blockDeviceMapping>item"`
	VirtualizationType       string                        `xml:"virtualizationType"`
	ClientToken              string                        `xml:"clientToken"`
	TagSet                   []tag                         `xml:"tagSet>item"`
	Hypervisor               string                        `xml:"hypervisor"`
	NetworkInterfaces        []instanceNetworkInterfaceXML `xml:"networkInterfaceSet>item"`
	EBSOptimized             bool                          `xml:"ebsOptimized"`
	ENASupport               bool                          `xml:"enaSupport"`
	CPUOptions               cpuOptionsXML                 `xml:"cpuOptions"`
	CapacityReservation      capacityReservationXML        `xml:"capacityReservationSpecification"`
	HibernationOptions       configuredXML                 `xml:"hibernationOptions"`
	MetadataOptions          metadataOptionsXML            `xml:"metadataOptions"`
	EnclaveOptions           enabledXML                    `xml:"enclaveOptions"`
	BootMode                 string                        `xml:"bootMode"`
	PlatformDetails          string                        `xml:"platformDetails"`
	UsageOperation           string                        `xml:"usageOperation"`
	UsageOperationUpdateTime string                        `xml:"usageOperationUpdateTime"`
	PrivateDNSNameOptions    privateDNSNameOptionsXML      `xml:"privateDnsNameOptions"`
	MaintenanceOptions       maintenanceOptionsXML         `xml:"maintenanceOptions"`
	CurrentInstanceBootMode  string                        `xml:"currentInstanceBootMode"`
}

func renderGroups(w *world, i *instance) []groupIdentifierXML {
	var out []groupIdentifierXML
	for _, id := range i.SecurityGroupIDs {
		name := ""
		if g := w.securityGroup(i.Region, id); g != nil {
			name = g.Name
		}
		out = append(out, groupIdentifierXML{id, name})
	}
	return out
}

func (s *Simulation) renderInstance(w *world, i *instance) instanceXML {
	t, _ := s.catalog.instanceType(i.InstanceType)
	public := w.publicIP(i)
	private := i.PrivateIP
	if i.State == "terminated" {
		private = ""
	}
	privateDNS, publicDNS := instanceDNS(i.Region, private, public)
	launched := timestamp(i.LaunchTime)
	out := instanceXML{
		InstanceID: i.ID, ImageID: i.ImageID, InstanceState: stateOf(i.State), PrivateDNSName: privateDNS, DNSName: publicDNS,
		Reason: i.TransitionReason, KeyName: i.KeyName, AMILaunchIndex: i.LaunchIndex, InstanceType: i.InstanceType, LaunchTime: launched,
		Placement:  placementXML{AvailabilityZone: i.AZ, AvailabilityZoneID: zoneID(i.AZ), Tenancy: "default"},
		Monitoring: monitoringXML{"disabled"}, SubnetID: i.SubnetID, VPCID: i.VPCID, PrivateIPAddress: private, IPAddress: public, SourceDestCheck: i.SourceDestCheck,
		GroupSet: renderGroups(w, i), Architecture: "x86_64", RootDeviceType: "ebs", RootDeviceName: rootDeviceName,
		VirtualizationType: "hvm", ClientToken: i.ClientToken, TagSet: w.tagsOf(i.ID), Hypervisor: "xen",
		EBSOptimized: i.EBSOptimized, ENASupport: true, CPUOptions: cpuOptionsXML{max(1, t.vcpus/2), min(2, t.vcpus)},
		CapacityReservation: capacityReservationXML{"open"},
		MetadataOptions: metadataOptionsXML{
			State: "applied", HTTPTokens: i.HTTPTokens, HTTPPutResponseHopLimit: i.HTTPHopLimit, HTTPEndpoint: i.HTTPEndpoint,
			HTTPProtocolIPv6: "disabled", InstanceMetadataTags: i.MetadataTags,
		},
		BootMode: "uefi-preferred", PlatformDetails: "Linux/UNIX", UsageOperation: "RunInstances", UsageOperationUpdateTime: launched,
		PrivateDNSNameOptions: privateDNSNameOptionsXML{HostnameType: "ip-name"}, MaintenanceOptions: maintenanceOptionsXML{"default"},
		CurrentInstanceBootMode: "legacy-bios",
	}
	if i.Monitoring {
		out.Monitoring.State = "enabled"
	}
	if i.StateReason != "" {
		out.StateReason = &stateReasonXML{i.StateReason, stateReasonMessage(i.StateReason)}
	}
	if i.State == "terminated" {
		out.SubnetID, out.VPCID, out.GroupSet = "", "", nil
		return out
	}
	if v := w.volume(i.VolumeID); v != nil {
		out.BlockDeviceMapping = []instanceBlockDeviceXML{{
			DeviceName: v.Device, EBS: ebsInstanceDeviceXML{v.ID, "attached", launched, v.DeleteOnTermination},
		}}
	}
	eni := instanceNetworkInterfaceXML{
		NetworkInterfaceID: i.ENIID, SubnetID: i.SubnetID, VPCID: i.VPCID, OwnerID: w.AccountID, Status: "in-use",
		MACAddress: i.MAC, PrivateIPAddress: private, PrivateDNSName: privateDNS, SourceDestCheck: i.SourceDestCheck,
		InterfaceType: "interface", GroupSet: renderGroups(w, i),
		Attachment: interfaceAttachmentXML{AttachmentID: i.ENIAttachmentID, Status: "attached", AttachTime: launched, DeleteOnTermination: true},
	}
	if public != "" {
		owner := "amazon"
		if find(w.Addresses, func(a *address) bool { return a.InstanceID == i.ID }) != nil {
			owner = w.AccountID
		}
		eni.Association = &interfaceAssociationXML{public, publicDNS, owner}
	}
	if private != "" {
		eni.PrivateIPAddresses = []privateAddressXML{{private, privateDNS, true, eni.Association}}
	}
	out.NetworkInterfaces = []instanceNetworkInterfaceXML{eni}
	return out
}

func stateReasonMessage(code string) string {
	switch code {
	case "Client.UserInitiatedShutdown":
		return "Client.UserInitiatedShutdown: User initiated shutdown"
	case "Server.InternalError":
		return "Server.InternalError: Internal error on launch"
	}
	return code
}

func (s *Simulation) renderReservation(w *world, reservation string) reservationXML {
	out := reservationXML{ReservationID: reservation, OwnerID: w.AccountID}
	for _, i := range w.Instances {
		if i.ReservationID == reservation {
			out.Instances = append(out.Instances, s.renderInstance(w, i))
		}
	}
	return out
}

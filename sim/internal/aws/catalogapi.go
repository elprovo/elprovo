package aws

import (
	"slices"
	"strconv"
)

func (s *Simulation) ec2CatalogActions() map[string]ec2Action {
	return map[string]ec2Action{
		"DescribeRegions":               {s.describeRegions, []string{"RegionName", "AllRegions", "Filter"}},
		"DescribeAvailabilityZones":     {s.describeAvailabilityZones, []string{"ZoneName", "ZoneId", "AllAvailabilityZones", "Filter"}},
		"DescribeInstanceTypes":         {s.describeInstanceTypes, []string{"InstanceType", "Filter", "MaxResults", "NextToken"}},
		"DescribeInstanceTypeOfferings": {s.describeInstanceTypeOfferings, []string{"LocationType", "Filter", "MaxResults", "NextToken"}},
		"DescribeAccountAttributes":     {s.describeAccountAttributes, []string{"AttributeName"}},
	}
}

type regionXML struct {
	RegionName     string `xml:"regionName"`
	RegionEndpoint string `xml:"regionEndpoint"`
	OptInStatus    string `xml:"optInStatus"`
}

type describeRegionsResponse struct {
	ec2Meta
	Regions []regionXML `xml:"regionInfo>item"`
}

func (s *Simulation) describeRegions(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	names := c.params.list("RegionName")
	out := &describeRegionsResponse{}
	for _, name := range names {
		if !s.catalog.hasRegion(name) {
			return nil, clientError("InvalidParameterValue", "Invalid region: %s", name)
		}
	}
	for _, region := range s.catalog.regions {
		if len(names) > 0 && !slices.Contains(names, region) {
			continue
		}
		r := regionXML{region, "ec2." + region + ".amazonaws.com", "opt-in-not-required"}
		match, err := matchFilters(filters, func(name string) ([]string, bool) {
			switch name {
			case "region-name":
				return values(r.RegionName), true
			case "endpoint":
				return values(r.RegionEndpoint), true
			case "opt-in-status":
				return values(r.OptInStatus), true
			}
			return nil, false
		}, nil)
		if err != nil {
			return nil, err
		}
		if match {
			out.Regions = append(out.Regions, r)
		}
	}
	return out, nil
}

type zoneXML struct {
	ZoneName           string `xml:"zoneName"`
	ZoneState          string `xml:"zoneState"`
	RegionName         string `xml:"regionName"`
	ZoneID             string `xml:"zoneId"`
	GroupName          string `xml:"groupName"`
	NetworkBorderGroup string `xml:"networkBorderGroup"`
	OptInStatus        string `xml:"optInStatus"`
	ZoneType           string `xml:"zoneType"`
}

type describeZonesResponse struct {
	ec2Meta
	Zones []zoneXML `xml:"availabilityZoneInfo>item"`
}

func (s *Simulation) describeAvailabilityZones(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	names, ids := c.params.list("ZoneName"), c.params.list("ZoneId")
	zones := s.catalog.zones(c.region)
	for _, name := range names {
		if !slices.Contains(zones, name) {
			return nil, errInvalidParameter("The zone '%s' does not exist.", name)
		}
	}
	out := &describeZonesResponse{}
	for _, zone := range zones {
		if (len(names) > 0 && !slices.Contains(names, zone)) || (len(ids) > 0 && !slices.Contains(ids, zoneID(zone))) {
			continue
		}
		z := zoneXML{zone, "available", c.region, zoneID(zone), c.region, c.region, "opt-in-not-required", "availability-zone"}
		match, err := matchFilters(filters, func(name string) ([]string, bool) {
			switch name {
			case "zone-name":
				return values(z.ZoneName), true
			case "zone-id":
				return values(z.ZoneID), true
			case "state":
				return values(z.ZoneState), true
			case "region-name":
				return values(z.RegionName), true
			case "zone-type":
				return values(z.ZoneType), true
			case "opt-in-status":
				return values(z.OptInStatus), true
			case "group-name":
				return values(z.GroupName), true
			case "network-border-group":
				return values(z.NetworkBorderGroup), true
			}
			return nil, false
		}, nil)
		if err != nil {
			return nil, err
		}
		if match {
			out.Zones = append(out.Zones, z)
		}
	}
	return out, nil
}

type processorInfoXML struct {
	SupportedArchitectures   []string `xml:"supportedArchitectures>item"`
	SustainedClockSpeedInGhz float64  `xml:"sustainedClockSpeedInGhz"`
	Manufacturer             string   `xml:"manufacturer"`
}

type vcpuInfoXML struct {
	DefaultVCPUs          int `xml:"defaultVCpus"`
	DefaultCores          int `xml:"defaultCores"`
	DefaultThreadsPerCore int `xml:"defaultThreadsPerCore"`
}

type memoryInfoXML struct {
	SizeInMiB int `xml:"sizeInMiB"`
}

type ebsInfoXML struct {
	EBSOptimizedSupport string `xml:"ebsOptimizedSupport"`
	EncryptionSupport   string `xml:"encryptionSupport"`
	NVMeSupport         string `xml:"nvmeSupport"`
}

type networkInfoXML struct {
	NetworkPerformance        string `xml:"networkPerformance"`
	MaximumNetworkInterfaces  int    `xml:"maximumNetworkInterfaces"`
	IPv4AddressesPerInterface int    `xml:"ipv4AddressesPerInterface"`
	IPv6AddressesPerInterface int    `xml:"ipv6AddressesPerInterface"`
	IPv6Supported             bool   `xml:"ipv6Supported"`
	ENASupport                string `xml:"enaSupport"`
	EFASupported              bool   `xml:"efaSupported"`
}

type instanceTypeXML struct {
	InstanceType                  string           `xml:"instanceType"`
	CurrentGeneration             bool             `xml:"currentGeneration"`
	FreeTierEligible              bool             `xml:"freeTierEligible"`
	SupportedUsageClasses         []string         `xml:"supportedUsageClasses>item"`
	SupportedRootDeviceTypes      []string         `xml:"supportedRootDeviceTypes>item"`
	SupportedVirtualizationTypes  []string         `xml:"supportedVirtualizationTypes>item"`
	BareMetal                     bool             `xml:"bareMetal"`
	Hypervisor                    string           `xml:"hypervisor"`
	ProcessorInfo                 processorInfoXML `xml:"processorInfo"`
	VCPUInfo                      vcpuInfoXML      `xml:"vCpuInfo"`
	MemoryInfo                    memoryInfoXML    `xml:"memoryInfo"`
	InstanceStorageSupported      bool             `xml:"instanceStorageSupported"`
	EBSInfo                       ebsInfoXML       `xml:"ebsInfo"`
	NetworkInfo                   networkInfoXML   `xml:"networkInfo"`
	HibernationSupported          bool             `xml:"hibernationSupported"`
	BurstablePerformanceSupported bool             `xml:"burstablePerformanceSupported"`
	DedicatedHostsSupported       bool             `xml:"dedicatedHostsSupported"`
	AutoRecoverySupported         bool             `xml:"autoRecoverySupported"`
	SupportedBootModes            []string         `xml:"supportedBootModes>item"`
}

func renderInstanceType(t instanceType) instanceTypeXML {
	return instanceTypeXML{
		InstanceType: t.name, CurrentGeneration: true, FreeTierEligible: t.name == "t3.micro",
		SupportedUsageClasses: []string{"on-demand", "spot"}, SupportedRootDeviceTypes: []string{"ebs"},
		SupportedVirtualizationTypes: []string{"hvm"}, Hypervisor: "nitro",
		ProcessorInfo:        processorInfoXML{[]string{"x86_64"}, 3.1, "Intel"},
		VCPUInfo:             vcpuInfoXML{t.vcpus, max(1, t.vcpus/2), min(2, t.vcpus)},
		MemoryInfo:           memoryInfoXML{t.memoryMiB},
		EBSInfo:              ebsInfoXML{"default", "supported", "required"},
		NetworkInfo:          networkInfoXML{t.network, t.interfaces, 6, 6, true, "required", false},
		HibernationSupported: true, BurstablePerformanceSupported: t.burstable, AutoRecoverySupported: true,
		SupportedBootModes: []string{"legacy-bios", "uefi"},
	}
}

type describeInstanceTypesResponse struct {
	ec2Meta
	InstanceTypes []instanceTypeXML `xml:"instanceTypeSet>item"`
}

func (s *Simulation) describeInstanceTypes(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	names := c.params.list("InstanceType")
	for _, name := range names {
		if _, ok := s.catalog.instanceType(name); !ok {
			return nil, clientError("InvalidInstanceType", "The following supplied instance types do not exist: [%s]", name)
		}
	}
	out := &describeInstanceTypesResponse{}
	for _, t := range instanceTypes {
		if len(names) > 0 && !slices.Contains(names, t.name) {
			continue
		}
		match, err := matchFilters(filters, func(name string) ([]string, bool) {
			switch name {
			case "instance-type":
				return values(t.name), true
			case "current-generation":
				return boolString(true), true
			case "burstable-performance-supported":
				return boolString(t.burstable), true
			case "processor-info.supported-architecture":
				return values("x86_64"), true
			case "vcpu-info.default-vcpus":
				return values(strconv.Itoa(t.vcpus)), true
			case "memory-info.size-in-mib":
				return values(strconv.Itoa(t.memoryMiB)), true
			}
			return nil, false
		}, nil)
		if err != nil {
			return nil, err
		}
		if match {
			out.InstanceTypes = append(out.InstanceTypes, renderInstanceType(t))
		}
	}
	return out, nil
}

type instanceTypeOfferingXML struct {
	InstanceType string `xml:"instanceType"`
	LocationType string `xml:"locationType"`
	Location     string `xml:"location"`
}

type describeInstanceTypeOfferingsResponse struct {
	ec2Meta
	Offerings []instanceTypeOfferingXML `xml:"instanceTypeOfferingSet>item"`
}

// describeInstanceTypeOfferings reports that every instance type is offered
// in every zone of the region.
func (s *Simulation) describeInstanceTypeOfferings(c *ec2Call) (ec2Response, error) {
	filters := c.params.filters()
	locationType := c.params.get("LocationType")
	var locations []string
	switch locationType {
	case "", "region":
		locationType, locations = "region", []string{c.region}
	case "availability-zone":
		locations = s.catalog.zones(c.region)
	case "availability-zone-id":
		for _, z := range s.catalog.zones(c.region) {
			locations = append(locations, zoneID(z))
		}
	default:
		return nil, errInvalidParameter("Invalid location type %s", locationType)
	}
	out := &describeInstanceTypeOfferingsResponse{}
	for _, t := range instanceTypes {
		for _, location := range locations {
			o := instanceTypeOfferingXML{t.name, locationType, location}
			match, err := matchFilters(filters, func(name string) ([]string, bool) {
				switch name {
				case "instance-type":
					return values(o.InstanceType), true
				case "location":
					return values(o.Location), true
				}
				return nil, false
			}, nil)
			if err != nil {
				return nil, err
			}
			if match {
				out.Offerings = append(out.Offerings, o)
			}
		}
	}
	return out, nil
}

type accountAttributeXML struct {
	AttributeName string   `xml:"attributeName"`
	Values        []string `xml:"attributeValueSet>item>attributeValue"`
}

type describeAccountAttributesResponse struct {
	ec2Meta
	Attributes []accountAttributeXML `xml:"accountAttributeSet>item"`
}

func (s *Simulation) describeAccountAttributes(c *ec2Call) (ec2Response, error) {
	var defaultVPC string
	s.store.Read(func(w *world) {
		if v := w.defaultVPC(c.region); v != nil {
			defaultVPC = v.ID
		}
	})
	all := []accountAttributeXML{
		{"supported-platforms", []string{"VPC"}},
		{"default-vpc", []string{defaultVPC}},
		{"max-instances", []string{strconv.Itoa(instanceLimit)}},
		{"vpc-max-security-groups-per-interface", []string{"5"}},
		{"max-elastic-ips", []string{"5"}},
		{"vpc-max-elastic-ips", []string{"5"}},
	}
	names := c.params.list("AttributeName")
	out := &describeAccountAttributesResponse{}
	for _, a := range all {
		if len(names) == 0 || slices.Contains(names, a.AttributeName) {
			out.Attributes = append(out.Attributes, a)
		}
	}
	return out, nil
}

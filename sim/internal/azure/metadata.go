package azure

import (
	"encoding/json"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// instanceMetadata returns the documents that the Azure Instance Metadata
// Service serves to the virtual machine, by path: the instance document
// /metadata/instance, its compute and network parts, and their leaves as
// text. The machine serves them at http://169.254.169.254.
func (s *Simulation) instanceMetadata(sub *subscription, v *vm) map[string]string {
	compute := s.computeMetadata(v)
	network := s.networkMetadata(sub, v)
	docs := map[string]string{}
	whole, _ := json.Marshal(map[string]any{"compute": compute, "network": network})
	docs["/metadata/instance"] = string(whole)
	addTree(docs, "/metadata/instance/compute", compute)
	addTree(docs, "/metadata/instance/network", network)
	return docs
}

func (s *Simulation) computeMetadata(v *vm) map[string]any {
	p := v.Properties
	var keys []map[string]string
	for _, k := range p.OSProfile.LinuxConfiguration.SSH.PublicKeys {
		keys = append(keys, map[string]string{"keyData": k.KeyData, "path": k.Path})
	}
	var tags []map[string]string
	var pairs []string
	for _, k := range slices.Sorted(maps.Keys(v.Tags)) {
		tags = append(tags, map[string]string{"name": k, "value": v.Tags[k]})
		pairs = append(pairs, k+":"+v.Tags[k])
	}
	publisher, offer, sku, version := "", "", "", ""
	if ref := p.StorageProfile.ImageReference; ref != nil && ref.ID == "" {
		publisher, offer, sku, version = ref.Publisher, ref.Offer, ref.Sku, v.ImageVersion
	}
	zone := ""
	if len(v.Zones) > 0 {
		zone = v.Zones[0]
	}
	return map[string]any{
		"azEnvironment": "AzurePublicCloud", "customData": "", "evictionPolicy": "", "isHostCompatibilityLayerVm": "false", "licenseType": "",
		"location": v.Location, "name": v.Name, "offer": offer, "osType": "Linux", "placementGroupId": "", "platformFaultDomain": "0",
		"platformUpdateDomain": "0", "priority": "", "provider": "Microsoft.Compute", "publisher": publisher, "sku": sku, "version": version,
		"osProfile": map[string]string{
			"adminUsername": p.OSProfile.AdminUsername, "computerName": p.OSProfile.ComputerName, "disablePasswordAuthentication": "true",
		},
		"plan":       map[string]string{"name": "", "product": "", "publisher": ""},
		"publicKeys": nonNil(keys), "resourceGroupName": v.Group, "resourceId": s.idFor(typeVM, &v.meta),
		"securityProfile": map[string]string{"secureBootEnabled": "false", "virtualTpmEnabled": "false"},
		"subscriptionId":  s.cfg.subscription, "tags": strings.Join(pairs, ";"), "tagsList": nonNil(tags), "userData": p.UserData,
		"vmId": v.VMID, "vmScaleSetName": "", "vmSize": p.HardwareProfile.VMSize, "zone": zone,
	}
}

func (s *Simulation) networkMetadata(sub *subscription, v *vm) map[string]any {
	interfaces := []any{}
	for _, ref := range v.Properties.NetworkProfile.NetworkInterfaces {
		n := s.nicByID(sub, ref.ID)
		if n == nil {
			continue
		}
		var addrs, subnets []map[string]string
		for _, c := range n.IPConfigurations {
			public := ""
			if ip := s.publicIPByID(sub, idOf(c.Properties.PublicIPAddress)); ip != nil {
				public = ip.Address
			}
			addrs = append(addrs, map[string]string{"privateIpAddress": c.Properties.PrivateIPAddress, "publicIpAddress": public})
			if _, sn := s.subnetByID(sub, idOf(c.Properties.Subnet)); sn != nil {
				for _, prefix := range sn.Properties.prefixes() {
					if p, err := netip.ParsePrefix(prefix); err == nil {
						subnets = append(subnets, map[string]string{"address": p.Addr().String(), "prefix": strconv.Itoa(p.Bits())})
					}
				}
			}
		}
		interfaces = append(interfaces, map[string]any{
			"ipv4":       map[string]any{"ipAddress": nonNil(addrs), "subnet": nonNil(subnets)},
			"ipv6":       map[string]any{"ipAddress": []any{}},
			"macAddress": strings.ReplaceAll(n.MAC, "-", ""),
		})
	}
	return map[string]any{"interface": interfaces}
}

// addTree adds the JSON document of value at path and, below it, every
// member: objects and arrays as JSON and their leaves as text, as the
// service answers with format=text.
func addTree(docs map[string]string, path string, value any) {
	switch v := value.(type) {
	case map[string]any:
		data, _ := json.Marshal(v)
		docs[path] = string(data)
		for k, child := range v {
			addTree(docs, path+"/"+k, child)
		}
	case map[string]string:
		data, _ := json.Marshal(v)
		docs[path] = string(data)
		for k, child := range v {
			docs[path+"/"+k] = child
		}
	case []any:
		data, _ := json.Marshal(v)
		docs[path] = string(data)
		for i, child := range v {
			addTree(docs, path+"/"+strconv.Itoa(i), child)
		}
	case []map[string]string:
		data, _ := json.Marshal(v)
		docs[path] = string(data)
		for i, child := range v {
			addTree(docs, path+"/"+strconv.Itoa(i), child)
		}
	case string:
		docs[path] = v
	default:
		data, _ := json.Marshal(v)
		docs[path] = string(data)
	}
}

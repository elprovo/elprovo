package vultr

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/elprovo/elprovo/sim/internal/machines"
)

type metadataInput struct {
	instance    *instance
	region      regionJSON
	publicKeys  []string
	vpcID       string
	privateIPv4 string
	publicIPv4  string
	networks    machines.Networks
}

type metadataIPv4 struct {
	Additional []struct{} `json:"additional"`
	Address    string     `json:"address"`
	Gateway    string     `json:"gateway"`
	Netmask    string     `json:"netmask"`
}

type metadataIPv6 struct {
	Additional []struct{} `json:"additional"`
	Network    string     `json:"network"`
	Prefix     string     `json:"prefix"`
}

type metadataInterface struct {
	IPv4        metadataIPv4 `json:"ipv4"`
	IPv6        metadataIPv6 `json:"ipv6"`
	NetworkType string       `json:"network-type"`
	NetworkV2ID string       `json:"network-v2-id,omitempty"`
}

// instanceMetadata returns the documents that Vultr's metadata service
// serves to the instance, by path: the /v1/ tree and /v1.json, with the
// public interface and, for an instance in a VPC, the private one. The JSON
// document also carries user-data and vendor-data, which cloud-init's Vultr
// datasource reads from it.
func instanceMetadata(in metadataInput) map[string]string {
	interfaces := []metadataInterface{{
		IPv4: metadataIPv4{
			Additional: []struct{}{}, Address: in.publicIPv4, Gateway: in.networks.Public.Gateway.String(), Netmask: netmask(in.networks.Public),
		},
		IPv6: metadataIPv6{Additional: []struct{}{}}, NetworkType: "public",
	}}
	if in.vpcID != "" {
		interfaces = append(interfaces, metadataInterface{
			IPv4:        metadataIPv4{Additional: []struct{}{}, Address: in.privateIPv4, Netmask: netmask(in.networks.Private)},
			IPv6:        metadataIPv6{Additional: []struct{}{}},
			NetworkType: "private", NetworkV2ID: in.vpcID,
		})
	}
	bgp := map[string]string{"my-address": "", "my-asn": "", "peer-address": "", "peer-asn": ""}
	regionCode := strings.ToUpper(in.region.ID)
	inst := in.instance
	full, _ := json.Marshal(map[string]any{
		"bgp": map[string]any{"ipv4": bgp, "ipv6": bgp}, "hostname": inst.Hostname, "instanceid": shortID(inst.ID),
		"instance-v2-id": inst.ID, "interfaces": interfaces, "public-keys": nonNil(in.publicKeys),
		"region":    map[string]string{"regioncode": regionCode, "countrycode": in.region.Country},
		"user-data": inst.UserData, "vendor-data": "",
	})
	docs := map[string]string{
		"/v1.json":               string(full),
		"/v1/hostname":           inst.Hostname,
		"/v1/instanceid":         shortID(inst.ID),
		"/v1/instance-v2-id":     inst.ID,
		"/v1/public-keys":        strings.Join(in.publicKeys, "\n"),
		"/v1/region/regioncode":  regionCode,
		"/v1/region/countrycode": in.region.Country,
	}
	for _, family := range []string{"ipv4", "ipv6"} {
		for name, value := range bgp {
			docs["/v1/bgp/"+family+"/"+name] = value
		}
	}
	for n, iface := range interfaces {
		base := "/v1/interfaces/" + strconv.Itoa(n) + "/"
		docs[base+"network-type"] = iface.NetworkType
		docs[base+"ipv4/address"] = iface.IPv4.Address
		docs[base+"ipv4/gateway"] = iface.IPv4.Gateway
		docs[base+"ipv4/netmask"] = iface.IPv4.Netmask
		docs[base+"ipv6/network"] = ""
		docs[base+"ipv6/prefix"] = ""
		if iface.NetworkV2ID != "" {
			docs[base+"network-v2-id"] = iface.NetworkV2ID
		}
	}
	return docs
}

// shortID stands for an instance's old-style "SUBID", which Vultr serves as
// instanceid next to the v2 id.
func shortID(id string) string {
	return strings.ReplaceAll(id, "-", "")[:12]
}

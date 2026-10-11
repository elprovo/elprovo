package vultr

import (
	"encoding/json"
	"testing"
)

func TestInstanceMetadataFollowsVultrsLayout(t *testing.T) {
	inst := &instance{ID: "36e9cf60-5d93-4e31-8ebf-613b3d2874fb", Hostname: "web-1", UserData: "#cloud-config\n"}
	fra, _ := region("fra")
	networks, _ := newFakeMachines().Networks(t.Context())

	docs := instanceMetadata(metadataInput{
		instance: inst, region: fra, publicKeys: []string{"ssh-ed25519 AAAA one", "ssh-ed25519 BBBB two"},
		publicIPv4: "203.0.113.7", networks: networks,
	})

	want := map[string]string{
		"/v1/hostname":                  "web-1",
		"/v1/instanceid":                "36e9cf605d93",
		"/v1/instance-v2-id":            inst.ID,
		"/v1/public-keys":               "ssh-ed25519 AAAA one\nssh-ed25519 BBBB two",
		"/v1/region/regioncode":         "FRA",
		"/v1/region/countrycode":        "DE",
		"/v1/interfaces/0/network-type": "public",
		"/v1/interfaces/0/ipv4/address": "203.0.113.7",
		"/v1/interfaces/0/ipv4/gateway": "172.31.0.1",
		"/v1/interfaces/0/ipv4/netmask": "255.255.0.0",
		"/v1/bgp/ipv4/peer-asn":         "",
	}
	for path, value := range want {
		if got, ok := docs[path]; !ok || got != value {
			t.Errorf("%s = %q (present %v), want %q", path, got, ok, value)
		}
	}
	var full struct {
		Hostname     string   `json:"hostname"`
		InstanceV2ID string   `json:"instance-v2-id"`
		PublicKeys   []string `json:"public-keys"`
		Region       struct {
			RegionCode string `json:"regioncode"`
		} `json:"region"`
		Interfaces []struct {
			IPv4 struct {
				Address string `json:"address"`
			} `json:"ipv4"`
			NetworkType string `json:"network-type"`
		} `json:"interfaces"`
		UserData   string  `json:"user-data"`
		VendorData *string `json:"vendor-data"`
	}
	if err := json.Unmarshal([]byte(docs["/v1.json"]), &full); err != nil {
		t.Fatalf("v1.json: %v", err)
	}
	if full.InstanceV2ID != inst.ID || full.Region.RegionCode != "FRA" || len(full.Interfaces) != 1 || full.Interfaces[0].IPv4.Address != "203.0.113.7" {
		t.Fatalf("v1.json = %+v", full)
	}
	if full.UserData != "#cloud-config\n" || full.VendorData == nil || len(full.PublicKeys) != 2 {
		t.Fatalf("v1.json lacks what cloud-init's Vultr datasource reads: %+v", full)
	}
	if _, ok := docs["/v1/interfaces/1/network-type"]; ok {
		t.Fatal("an instance outside a VPC has a second interface")
	}
}

func TestInstanceInAVPCHasAPrivateInterface(t *testing.T) {
	fra, _ := region("fra")
	networks, _ := newFakeMachines().Networks(t.Context())

	docs := instanceMetadata(metadataInput{
		instance: &instance{ID: "36e9cf60-5d93-4e31-8ebf-613b3d2874fb"}, region: fra, vpcID: "vpc-1",
		privateIPv4: "172.30.255.254", publicIPv4: "203.0.113.7", networks: networks,
	})

	want := map[string]string{
		"/v1/interfaces/1/network-type":  "private",
		"/v1/interfaces/1/ipv4/address":  "172.30.255.254",
		"/v1/interfaces/1/ipv4/netmask":  "255.255.0.0",
		"/v1/interfaces/1/network-v2-id": "vpc-1",
	}
	for path, value := range want {
		if got, ok := docs[path]; !ok || got != value {
			t.Errorf("%s = %q (present %v), want %q", path, got, ok, value)
		}
	}
}
